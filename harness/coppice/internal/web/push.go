package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// PushDebounce is how long one pane stays quiet after it buzzes the phone. A
// pane that flaps blocked, working, blocked is one event to a human.
const PushDebounce = 5 * time.Second

// Ask mirrors the ask block of a pane state event.
type Ask struct {
	ID       string  `json:"id"`
	Tool     string  `json:"tool"`
	Summary  string  `json:"summary"`
	Deadline float64 `json:"deadline"`
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
}

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
		last: map[string]string{}, sent: map[string]time.Time{}}, nil
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
// the edge, not the level.
func (p *Publisher) OnState(ev StateEvent, label string) {
	p.onState(context.Background(), ev, label)
}

// onState is OnState with a caller-supplied context. Watch uses it so a
// publish started from the event loop is cancelled along with the loop
// itself, instead of running past its parent's lifetime.
func (p *Publisher) onState(ctx context.Context, ev StateEvent, label string) {
	p.mu.Lock()
	was := p.last[ev.Pane]
	p.last[ev.Pane] = ev.State
	if ev.State != "blocked" || was == "blocked" {
		p.mu.Unlock()
		return
	}
	now := p.now()
	if last, ok := p.sent[ev.Pane]; ok && now.Sub(last) < PushDebounce {
		p.mu.Unlock()
		return
	}
	p.sent[ev.Pane] = now
	p.mu.Unlock()

	if label == "" {
		label = ev.Pane
	}
	body := "blocked, and the gate gave no detail"
	if ev.Ask != nil && ev.Ask.Summary != "" {
		body = ev.Ask.Summary
	} else if ev.Detail != "" {
		body = ev.Detail
	}
	click := ""
	if p.cfg.ClickBase != "" {
		click = p.cfg.ClickBase + "/#/pane/" + ev.Pane
	}
	if err := p.publish(ctx, label+" needs you", body, click); err != nil {
		// A notification that did not arrive is not a reason to change what
		// the floor believes. Log it and carry on. The base URL is logged on
		// its own; the error is not, because a failed request's error
		// carries the full URL, and the full URL carries the topic, which
		// is the whole credential for an unauthenticated ntfy server.
		p.log.Warn("web: ntfy publish failed", "pane", ev.Pane, "base", p.cfg.BaseURL, "err", err)
	}
}

// Test publishes one message so the operator can prove the topic works from
// the settings screen.
func (p *Publisher) Test(ctx context.Context) error {
	return p.publish(ctx, "coppice test", "Push works. This came from your own server.", p.cfg.ClickBase)
}

func (p *Publisher) publish(ctx context.Context, title, body, click string) error {
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
