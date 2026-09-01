package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// PushDebounce is how long one pane stays quiet after it buzzes the phone. A
// pane that flaps blocked, working, blocked is one event to a human.
const PushDebounce = 5 * time.Second

// Ask mirrors the ask block of a pane state event. Tier is undoable or
// permanent; anything else reads as permanent. Gate, when present, is the
// gate's own verdict and the rule that decided it.
type Ask struct {
	ID       string   `json:"id"`
	Tool     string   `json:"tool"`
	Summary  string   `json:"summary"`
	Deadline float64  `json:"deadline"`
	Tier     string   `json:"tier"`
	Gate     *AskGate `json:"gate,omitempty"`
	// Holder is "harness" when a headless harness holds the ask itself.
	// The gate cannot answer it, so no card offers an answer to it.
	Holder string `json:"holder,omitempty"`
}

// Held reports whether a harness holds the ask itself.
func (a *Ask) Held() bool { return a != nil && a.Holder == "harness" }

// AtTheFloor is what the answer route says about an ask a harness holds:
// the gate has no file for it, so the floor answers it. The page's Deny
// and Allow for such an ask go to coppice-server, not to this route.
func AtTheFloor(pane string) string {
	return fmt.Sprintf("The harness holds this ask itself. Open %s on the floor and answer it there.", pane)
}

// Words a push for a blocked pane with no ask says, by what found the
// block. They match the floor's own. Only the gate's block blames the gate.
const (
	BlockedByGate      = "Blocked. The gate gave no detail."
	BlockedOwnQuestion = "Waiting on the agent's own question. Open it to answer."
)

// AskGate is the gate's verdict on an ask and the rule that decided it.
type AskGate struct {
	Verdict string `json:"verdict"`
	Rule    *int   `json:"rule"`
}

// denyGrant is what one deny token may do: deny ask on pane before
// deadline, and nothing else.
type denyGrant struct {
	ask      string
	pane     string
	deadline float64
}

// StateEvent is the part of a pane state event push cares about.
type StateEvent struct {
	Pane    string  `json:"pane"`
	Harness string  `json:"harness"`
	State   string  `json:"state"`
	Source  string  `json:"source"`
	Detail  string  `json:"detail"`
	TS      float64 `json:"ts"`
	Ask     *Ask    `json:"ask"`
	// Held is set while a foreman hears the ask first.
	Held *Held `json:"held,omitempty"`
}

// Held names the foreman that hears an ask first, and its task.
type Held struct {
	By   string `json:"by"`
	Task string `json:"task"`
}

// PushConfig points at a self-hosted ntfy topic. There is no default
// server: push goes through a server the operator runs, or not at all.
type PushConfig struct {
	BaseURL   string // https://ntfy.example
	Topic     string
	TokenEnv  string // the name of the environment variable holding the token
	ClickBase string // the PWA's external URL, for the notification's click target
}

// Publisher turns merged state into notifications. It holds the last state
// it saw per pane, so it can tell a transition from a repeat.
type Publisher struct {
	cfg  PushConfig
	hc   *http.Client
	now  func() time.Time
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]string
	sent map[string]time.Time
	// denies maps each deny token on a lock-screen card to its grant.
	denies map[string]denyGrant
	// retired maps each deny token whose ask is gone to the time, in unix
	// seconds, it is kept until. A tap on an old card then reads as a gone
	// ask, not as a bad token.
	retired map[string]float64
}

// retiredKeep is how long a retired deny token is still known after its
// ask's deadline. A card can sit on a lock screen that long.
const retiredKeep = 24 * 3600

// retiredMax bounds how many retired tokens are kept.
const retiredMax = 1024

// NewPublisher builds a Publisher for cfg. It fails if BaseURL or Topic is
// empty, so a caller who forgot the flags hears about it at startup instead
// of at the first silent notification.
func NewPublisher(cfg PushConfig, hc *http.Client, now func() time.Time, log *slog.Logger) (*Publisher, error) {
	if cfg.BaseURL == "" || cfg.Topic == "" {
		return nil, errors.New("push needs --ntfy URL and --ntfy-topic NAME")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.ClickBase = strings.TrimRight(cfg.ClickBase, "/")
	return &Publisher{cfg: cfg, hc: hc, now: now, log: log,
		last: map[string]string{}, sent: map[string]time.Time{}, denies: map[string]denyGrant{}, retired: map[string]float64{}}, nil
}

// asciiHeader makes a string safe for ntfy: printable ASCII only, one line,
// bounded. ntfy reads Title as an HTTP header, where a newline would split
// the request, and shows the body as text. Both are scrubbed so what the
// phone shows is what the gate said.
func asciiHeader(s string, max int) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			r = ' '
		}
		if r == ' ' {
			if lastSpace {
				continue
			}
			lastSpace = true
		} else {
			lastSpace = false
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) <= max {
		return out
	}
	if max <= 3 {
		if max < 0 {
			max = 0
		}
		return out[:max]
	}
	return strings.TrimSpace(out[:max-3]) + "..."
}

// OnState records one merged state and publishes when a pane has just become
// blocked. Everything else is quiet on purpose: the interesting moment is
// the edge, not the level. A blocked event that a foreman holds counts as
// not blocked, so the edge comes when the hold ends.
func (p *Publisher) OnState(ev StateEvent, label string) {
	p.onState(context.Background(), ev, label)
}

// onState is OnState with a caller-supplied context. Watch uses it so a
// publish started from the event loop is cancelled along with the loop
// itself, instead of running past its parent's lifetime.
func (p *Publisher) onState(ctx context.Context, ev StateEvent, label string) {
	p.mu.Lock()
	p.retireStale(ev)
	was := p.last[ev.Pane]
	now := p.now()
	state := ev.State
	if state == "blocked" && ev.Held != nil {
		state = "held"
	}
	p.last[ev.Pane] = state
	if state != "blocked" || was == "blocked" {
		p.mu.Unlock()
		return
	}
	if last, ok := p.sent[ev.Pane]; ok && now.Sub(last) < PushDebounce {
		p.mu.Unlock()
		return
	}
	p.sent[ev.Pane] = now
	// The deny token is minted under the same lock that saw the edge, so
	// the next event for this pane always sees it and can retire it.
	tok := p.mintDeny(ev)
	p.mu.Unlock()

	if label == "" {
		label = ev.Pane
	}
	body := BlockedOwnQuestion
	switch {
	case ev.Ask.Held():
		body = "Wants " + ev.Ask.Summary + ". Open the floor to answer it."
	case ev.Ask != nil && ev.Ask.Summary != "":
		body = "Wants " + ev.Ask.Summary + ". " + gateSays(ev.Ask.Gate)
	case ev.Detail != "" && ev.Source != "manifest":
		// A manifest's detail is the name of the screen rule that matched,
		// which tells the owner nothing.
		body = ev.Detail
	case ev.Source == "gate":
		body = BlockedByGate
	}
	click := clickURL(p.cfg.ClickBase, ev)
	actions := p.actions(ev, click, tok)
	if err := p.publish(ctx, label+" needs you.", body, click, actions); err != nil {
		// A notification that did not arrive is not a reason to change what
		// the floor believes. Log it and carry on. The base URL is logged on
		// its own; the error is not, because a failed request's error
		// carries the full URL, and the full URL carries the topic, which
		// is the whole credential for an unauthenticated ntfy server.
		p.log.Warn("web: ntfy publish failed", "pane", ev.Pane, "base", p.cfg.BaseURL, "err", err)
	}
}

// clickURL is where the card lands: the pane screen at the ask, as
// <base>/#/pane/<pane>?ask=<ask id>. It carries no credential. The pane id
// is path escaped, so a comma, a semicolon, a question mark, a hash or a
// percent sign in it neither splits the Actions header nor breaks the
// page's route. An ask id
// that SafeID would change stays out, and the card opens the pane alone.
// With no external URL there is no click target.
func clickURL(base string, ev StateEvent) string {
	if base == "" {
		return ""
	}
	click := base + "/#/pane/" + url.PathEscape(ev.Pane)
	if ev.Ask != nil && ev.Ask.ID != "" && SafeID(ev.Ask.ID) == ev.Ask.ID {
		click += "?ask=" + ev.Ask.ID
	}
	return click
}

// gateSays is the sentence about the gate's verdict. The rule shows when
// the gate names one.
func gateSays(g *AskGate) string {
	if g != nil && g.Verdict == "allow" {
		return "Gate says yes."
	}
	if g != nil && g.Rule != nil {
		return fmt.Sprintf("Gate says no, rule %d.", *g.Rule)
	}
	return "Gate says no."
}

// actions is the card's buttons in ntfy's Actions header form: Deny, then
// Look, then Allow only on an undoable ask. Deny posts to the answer route
// with a deny token minted for this ask. The token can deny this ask and
// nothing else, because ntfy stores the action's headers on its server.
// Look and Allow open the pane screen at the ask, where the operator's
// own token answers. With no external URL, or an ask id that cannot ride in the
// header, the card has no buttons.
func (p *Publisher) actions(ev StateEvent, click, tok string) string {
	// The gate cannot answer an ask a harness holds, so its card only
	// looks.
	if ev.Ask.Held() {
		if click == "" {
			return ""
		}
		return "view, Look, " + click
	}
	if ev.Ask == nil || click == "" || tok == "" {
		return ""
	}
	body := `{"tool_use_id":"` + ev.Ask.ID + `","decision":"deny","reason":"denied from the lock screen"}`
	parts := []string{
		"http, Deny, " + p.cfg.ClickBase + "/api/ask/answer, method=POST, " +
			"headers.Authorization=Bearer " + tok + ", headers.Content-Type=application/json, " +
			"body='" + body + "', clear=true",
		"view, Look, " + click,
	}
	if ev.Ask.Tier == "undoable" {
		parts = append(parts, "view, Allow, "+click)
	}
	return strings.Join(parts, "; ")
}

// denyNoDeadline is how long a deny token lives when its ask names no
// deadline.
const denyNoDeadline = 600

// mintDeny makes and records the deny token for ev's ask, or returns ""
// when the card can carry no buttons. The caller holds p.mu.
func (p *Publisher) mintDeny(ev StateEvent) string {
	if ev.Ask == nil || ev.Ask.Held() || p.cfg.ClickBase == "" || ev.Ask.ID == "" || SafeID(ev.Ask.ID) != ev.Ask.ID {
		return ""
	}
	tok, err := newDenyToken()
	if err != nil {
		return ""
	}
	deadline := ev.Ask.Deadline
	if deadline <= 0 {
		deadline = p.nowSeconds() + denyNoDeadline
	}
	p.denies[tok] = denyGrant{ask: ev.Ask.ID, pane: ev.Pane, deadline: deadline}
	return tok
}

// newDenyToken is 32 random bytes in hex.
func newDenyToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// retireStale drops the deny tokens of ev's pane whose ask the pane no
// longer shows. The caller holds p.mu.
func (p *Publisher) retireStale(ev StateEvent) {
	for tok, g := range p.denies {
		if g.pane != ev.Pane {
			continue
		}
		if ev.State != "blocked" || ev.Ask == nil || ev.Ask.ID != g.ask {
			p.retire(tok, g)
		}
	}
}

// retire moves one token from the live grants to the retired ones. The
// caller holds p.mu.
func (p *Publisher) retire(tok string, g denyGrant) {
	delete(p.denies, tok)
	now := p.nowSeconds()
	until := g.deadline
	if until < now {
		until = now
	}
	p.retired[tok] = until + retiredKeep
	for k, v := range p.retired {
		if v <= now {
			delete(p.retired, k)
		}
	}
	for len(p.retired) > retiredMax {
		oldest, at := "", 0.0
		for k, v := range p.retired {
			if oldest == "" || v < at {
				oldest, at = k, v
			}
		}
		delete(p.retired, oldest)
	}
}

func (p *Publisher) nowSeconds() float64 { return float64(p.now().UnixNano()) / 1e9 }

// DenyRetired reports whether token was a deny token whose ask is gone,
// answered, left or past its deadline, within the last day. Every token
// is compared in constant time.
func (p *Publisher) DenyRetired(token string) bool {
	if p == nil || token == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.nowSeconds()
	found := false
	for tok, until := range p.retired {
		if until <= now {
			delete(p.retired, tok)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			found = true
		}
	}
	return found
}

// DenyGrant reports the ask a deny token may deny. It is false for a
// token nobody minted, for one whose ask is gone, and after the ask's
// deadline. Every token is compared in constant time.
func (p *Publisher) DenyGrant(token string) (string, bool) {
	if p == nil || token == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.nowSeconds()
	found := ""
	ok := false
	for tok, g := range p.denies {
		if now >= g.deadline {
			p.retire(tok, g)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			found, ok = g.ask, true
		}
	}
	return found, ok
}

// RetireDeny drops every deny token for ask, once it is answered.
func (p *Publisher) RetireDeny(ask string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for tok, g := range p.denies {
		if g.ask == ask {
			p.retire(tok, g)
		}
	}
}

// Test publishes one message so the operator can prove the topic works from
// the settings screen.
func (p *Publisher) Test(ctx context.Context) error {
	return p.publish(ctx, "coppice test", "Push works. This came from your own server.", p.cfg.ClickBase, "")
}

func (p *Publisher) publish(ctx context.Context, title, body, click, actions string) error {
	url := p.cfg.BaseURL + "/" + p.cfg.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(asciiHeader(body, 500))))
	if err != nil {
		return err
	}
	req.Header.Set("Title", asciiHeader(title, 100))
	if click != "" {
		// Title and the body are both scrubbed by asciiHeader before they
		// become a header or a body. Click went out raw. 2000 characters is
		// far past any realistic external URL plus /#/pane/<pane id>, so a
		// real click target never meets the three-dot truncation this bound
		// exists to catch malformed input with instead.
		req.Header.Set("Click", asciiHeader(click, 2000))
	}
	if actions != "" {
		req.Header.Set("Actions", asciiHeader(actions, 4000))
	}
	if p.cfg.TokenEnv != "" {
		if tok := os.Getenv(p.cfg.TokenEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		// The client wraps every transport failure in an error that carries
		// the request URL, and the request URL carries the topic. Only the
		// underlying failure is kept.
		if unwrapped := errors.Unwrap(err); unwrapped != nil {
			err = unwrapped
		}
		return fmt.Errorf("ntfy request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy answered %d", resp.StatusCode)
	}
	return nil
}

// Watch subscribes to state events on the socket and feeds onState. Labels
// come from pane.list, which is where they live. A pane the list does not
// know is looked up once and then titled by its own id, rather than looked
// up again on every event about it.
func (p *Publisher) Watch(ctx context.Context, d Dialer) error {
	labels := map[string]string{}
	refresh := func() {
		msg, err := Call(ctx, d, map[string]any{"cmd": "pane.list"})
		if err != nil {
			return
		}
		result, _ := msg["result"].(map[string]any)
		panes, _ := result["panes"].([]any)
		for _, raw := range panes {
			item, _ := raw.(map[string]any)
			id, _ := item["id"].(string)
			label, _ := item["label"].(string)
			if id != "" {
				labels[id] = label
			}
		}
	}
	refresh()

	sess, err := Open(ctx, d)
	if err != nil {
		return err
	}
	defer sess.Close()

	const subID = "push-sub"
	sub, err := json.Marshal(map[string]any{
		"id": subID, "cmd": "events.subscribe",
		"panes": "*", "kinds": []string{"state"},
	})
	if err != nil {
		return err
	}
	if err := sess.Send(sub); err != nil {
		return err
	}

	handle := func(line []byte) {
		var envelope struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.Event != "state" {
			return
		}
		var ev StateEvent
		if json.Unmarshal(line, &ev) != nil || ev.Pane == "" {
			return
		}
		if _, known := labels[ev.Pane]; !known {
			refresh()
			if _, known := labels[ev.Pane]; !known {
				labels[ev.Pane] = ""
			}
		}
		p.onState(ctx, ev, labels[ev.Pane])
	}

	// The subscribe reply is one more line on the same stream as every
	// event, so it is read here instead of assumed. A refusal is returned to
	// the caller instead of leaving Watch waiting on events that a refused
	// subscription will never deliver; a state line that arrives before the
	// reply is still handled rather than dropped. The wait for the reply is
	// bounded the same way Call bounds its own wait for a reply.
	subscribed := false
	ackDeadline := time.NewTimer(5 * time.Second)
	defer ackDeadline.Stop()
	for {
		var timeout <-chan time.Time
		if !subscribed {
			timeout = ackDeadline.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return errors.New("coppice-server did not answer events.subscribe in 5 s")
		case line, ok := <-sess.Lines():
			if !ok {
				return errors.New("coppice-server closed the event stream")
			}
			if !subscribed {
				var ack struct {
					ID string `json:"id"`
					OK bool   `json:"ok"`
				}
				if json.Unmarshal(line, &ack) == nil && ack.ID == subID {
					if !ack.OK {
						var msg map[string]any
						json.Unmarshal(line, &msg)
						return refusal(msg)
					}
					subscribed = true
					ackDeadline.Stop()
					continue
				}
			}
			handle(line)
		}
	}
}
