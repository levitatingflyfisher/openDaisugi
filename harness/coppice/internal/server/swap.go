package server

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// The model and router swaps of the stack bar (F12a). Each says when it
// takes effect: a model swap on claude lands at the next idle prompt, on
// headless sprig with the next turn; a router swap restarts the agent and
// resumes its conversation with a new base URL in its env, since a running
// process cannot change its own env.

// ModelRefusal and RouterRefusal are what a pane, a plugin and an unplaced
// peer get for the swap verbs.
const (
	ModelRefusal  = "only the operator sets an agent's model."
	RouterRefusal = "only the operator sets an agent's router."
)

// ModelWordsRefusal is what a model name that is not plain gets. The name
// is typed into claude's input box, so it carries no space and no control
// character.
const ModelWordsRefusal = "model must be a plain name: letters, digits and . _ : / - [ ], up to 64 characters."

// RouterWordsRefusal is what a router word other than direct or gateway
// gets. Switchyard is a mode of the gateway itself, so a pane picks the
// gateway and the gateway's own setting names what routes behind it.
const RouterWordsRefusal = "router must be direct or gateway."

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,63}$`)

func validRouterWord(w string) bool { return w == "direct" || w == "gateway" }

// swapRefusal is the words for a swap the pane's harness has no way to do.
func swapRefusal(what, label, harness string) string {
	return fmt.Sprintf("Swapping the %s of %s is not built for %s yet.", what, label, harness)
}

// routerKeys names, for each harness a router swap is built for, the env
// key that holds its base URL and the vendor address it takes for direct.
var routerKeys = map[string]struct{ key, vendor string }{
	"claude": {"ANTHROPIC_BASE_URL", "https://api.anthropic.com"},
}

// routerEnv is the env entry a router swap sets for a harness. Direct is
// the vendor's own address, never a missing key: a pane's process also
// inherits the server's env, so a missing key could still route through
// the gateway.
func (s *Server) routerEnv(harness, router string) (key, val string, err error) {
	k, ok := routerKeys[harness]
	if !ok {
		return "", "", fmt.Errorf("Swapping the router is not built for %s yet.", harnessWord(harness))
	}
	if router == "direct" {
		return k.key, k.vendor, nil
	}
	gateway, _ := s.gatewayFacts()
	if gateway == "" {
		return "", "", fmt.Errorf("no gateway is set. Add gateway = \"http://127.0.0.1:8787\" to coppice.toml.")
	}
	return k.key, gateway, nil
}

// handleSetModel answers agent.set_model {pane, model}. On claude it
// queues /model NAME through the ready queue, so it lands only at an idle
// prompt, and waits there when the screen gives no verdict. On a headless sprig pane the turns after it run on NAME. Any
// other harness is refused with words.
func (s *Server) handleSetModel(c *Client, r *proto.Request) proto.Response {
	id := paneOf(r)
	if c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	model, _ := r.Str("model")
	if !modelName.MatchString(model) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, ModelWordsRefusal)
	}
	lp, bad := s.livePane(r)
	if bad != nil {
		return *bad
	}
	rec, _ := s.tree.Pane(id)
	label := s.labelOf(id)
	switch {
	case rec.Harness == "claude" && lp.PTY != nil:
		text := "/model " + model
		// With no verdict from the screen reader, nothing can say the
		// prompt is idle, so the line waits in the queue as it does for
		// a busy screen. It is never typed at once (F12-R-9).
		out, err := s.submitPrompt(id, text, senderPane(c), c)
		if err != nil {
			return promptErr(r, err)
		}
		if out.sent {
			return proto.OKResp(r.ID, map[string]any{"pane": id, "model": model, "when": "now"})
		}
		return proto.OKResp(r.ID, map[string]any{
			"pane": id, "model": model, "when": "queued", "queued": out.queued,
			"note": fmt.Sprintf(QueuedNote, label),
		})
	case rec.Harness == "sprig" && lp.Adapter != nil:
		ms, ok := lp.Adapter.(pane.ModelSetter)
		if !ok {
			break
		}
		ms.SetModel(model)
		// The record keeps the choice, so a resume or a restart of this
		// pane runs its turns on the same model (F12-R-11).
		_ = s.updatePane(id, func(p *layout.Pane) { p.Model = model })
		return proto.OKResp(r.ID, map[string]any{"pane": id, "model": model, "when": "next turn"})
	}
	return proto.ErrResp(r.ID, proto.ErrBadRequest, swapRefusal("model", label, harnessWord(rec.Harness)))
}

// harnessWord is a harness name for a sentence.
func harnessWord(h string) string {
	if h == "" {
		return "a pane with no harness"
	}
	return h
}

// stopWait is the longest a router swap waits for the old process to go.
const stopWait = 10 * time.Second

// handleSetRouter answers agent.set_router {pane, router}: it stops the
// agent, keeps its record as ended, and resumes the conversation in a new
// pane whose env names the new base URL. It refuses an agent that has no
// conversation to resume, because a restart would lose it. The agent comes
// back with a new pane id and enforcing: a watching marker goes with the
// old pane.
func (s *Server) handleSetRouter(c *Client, r *proto.Request) proto.Response {
	id := paneOf(r)
	if c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	router, _ := r.Str("router")
	if !validRouterWord(router) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, RouterWordsRefusal)
	}
	if s.isClosed() {
		return proto.ErrResp(r.ID, proto.ErrServerClosed, "this server has closed. It cannot start a new pane.")
	}
	rec, ok := s.tree.Pane(id)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	label := s.labelOf(id)
	if rec.Closed {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed, fmt.Sprintf("%s has ended.", label))
	}
	if _, built := routerKeys[rec.Harness]; !built {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, swapRefusal("router", label, harnessWord(rec.Harness)))
	}
	if _, _, err := s.routerEnv(rec.Harness, router); err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
	}
	// The agent would come back with a base URL that fails every call, so
	// a gateway that does not answer is refused before anything stops. A
	// gateway off this computer is not dialed, so it is not judged here.
	if router == "gateway" {
		gateway, _ := s.gatewayFacts()
		if answers, known := s.gatewayAnswers(gateway); known && !answers {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("the gateway at %s does not answer. Start it, then swap.", gateway))
		}
	}
	if !s.canResume(rec) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf(
			"%s has no conversation to resume yet, so a restart would lose it. Wait for its first reply, then swap.", label))
	}
	if !s.claimResume(id) {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf("pane %s is already resuming.", id))
	}
	defer s.releaseResume(id)

	lp, live := s.Live(id)
	if !live {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed, fmt.Sprintf("%s has ended.", label))
	}
	if lp.PTY != nil {
		_ = lp.PTY.Close()
	}
	if lp.Adapter != nil {
		_ = lp.Adapter.Stop()
	}
	if lp.gone != nil {
		select {
		case <-lp.gone:
		case <-time.After(stopWait):
			return proto.ErrResp(r.ID, proto.ErrInternal, fmt.Sprintf("%s did not stop in time. Try again.", label))
		}
	}
	// Keep the record, as a process that ends on its own does, so the
	// resume below reads it. The exit code is the one watchExit stored.
	_ = s.closePane(id, nil)
	resp := s.resumeEnded(r.ID, resumeArgs{id: id, router: router})
	if resp.OK {
		return resp
	}
	// The old process is gone, so the swap must not leave the agent dead
	// without a word: try one plain resume with the old env.
	why := ""
	if resp.Error != nil {
		why = resp.Error.Message
	}
	if back := s.resumeEnded(r.ID, resumeArgs{id: id}); back.OK {
		newID := ""
		if m, ok := back.Result.(map[string]any); ok {
			newID, _ = m["pane"].(string)
		}
		return proto.ErrResp(r.ID, proto.ErrSpawnFailed, routerKeptMessage(label, why, newID))
	}
	return proto.ErrResp(r.ID, proto.ErrSpawnFailed, routerLostMessage(label, id))
}

// routerLostMessage is the reply for a router swap that stopped the agent
// and then could not start it again, on either router.
func routerLostMessage(label, id string) string {
	return fmt.Sprintf("%s was stopped and its router did not change. It is in the ended list. "+
		"To start it again, run: coppice pane resume %s.", label, id)
}

// routerKeptMessage is the reply for a router swap whose new start failed
// and whose old router came back. why is the failure of the new start.
func routerKeptMessage(label, why, newID string) string {
	return fmt.Sprintf("The router of %s did not change: %s The agent runs again on its old router, as pane %s.",
		label, endSentence(why), newID)
}

// endSentence puts a full stop on a reason that has none.
func endSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

// canResume is true when pane.resume would resume rec's conversation
// rather than start fresh: the record has a harness session id, and a pty
// pane's harness has resume_args (see resumableCount).
func (s *Server) canResume(rec layout.Pane) bool {
	if rec.HarnessSessionID == "" {
		return false
	}
	switch rec.Kind {
	case layout.KindHeadless:
		return true
	case layout.KindPTY:
		_, ok := resumeArgvFor(rec.Harness, rec.HarnessSessionID, rec.ExtraArgs)
		return ok
	}
	return false
}

// panePathStamp matches a pane stamp at the start of a URL path.
var panePathStamp = regexp.MustCompile(`^/pane/[0-9a-f]{16}/w[0-9]{1,9}:p[0-9]{1,9}`)

// stampURL puts the pane prefix /pane/<floor>/<pane> at the start of a base
// URL's path, ahead of any path it has (a /v1), and takes off a stamp it
// already carries. The gateway strips the prefix and stamps its turn
// record with the pane and the floor, so the floor can say which pane a
// saving belongs to. A URL it cannot read comes back as it was.
func stampURL(raw, floor, id string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	rest := panePathStamp.ReplaceAllString(u.Path, "")
	u.Path = "/pane/" + floor + "/" + id + strings.TrimRight(rest, "/")
	u.RawPath = ""
	return u.String()
}

// stampedEnv is the env a pane's process starts with: the record's own,
// with each base URL that the record's own env sets to the gateway
// carrying the pane prefix. A base URL the process only inherits from the
// server is left alone, and the record keeps the plain URL, so a resume or
// a restart stamps the new pane id.
func (s *Server) stampedEnv(rec layout.Pane) map[string]string {
	if len(rec.Env) == 0 {
		return rec.Env
	}
	gateway, _ := s.gatewayFacts()
	if gateway == "" {
		return rec.Env
	}
	out := maps.Clone(rec.Env)
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"} {
		v := strings.TrimSpace(rec.Env[k])
		if v == "" || !sameEndpoint(v, gateway) || !s.gatewayStamps(gateway) {
			continue
		}
		out[k] = stampURL(v, s.floorKey(), rec.ID)
	}
	return out
}

// capsAnswer is what the gateway at one address said about the pane prefix.
type capsAnswer struct {
	yes bool
	at  time.Time
}

// capsTimeout bounds the ask for the gateway's capabilities.
const capsTimeout = 500 * time.Millisecond

// gatewayStamps reports whether the gateway at this address says it takes
// the pane prefix: GET /_caps answers {"pane_prefix": true}. An older
// gateway has no such route, and a stamped path would fail upstream, so
// anything but a clear yes is a no and the base URL stays plain (F12-R-10).
// It asks only a loopback gateway, once for each address: a yes is kept for
// the life of the server, a no for outsideTTL, so a gateway that starts or
// updates later is asked again.
func (s *Server) gatewayStamps(gateway string) bool {
	u, err := url.Parse(gateway)
	if err != nil || u.Host == "" || !isLoopbackName(strings.ToLower(u.Hostname())) {
		return false
	}
	s.outsideMu.Lock()
	a, ok := s.caps[gateway]
	s.outsideMu.Unlock()
	if ok && (a.yes || time.Since(a.at) < outsideTTL) {
		return a.yes
	}
	yes := askPanePrefix(gateway)
	s.outsideMu.Lock()
	if s.caps == nil {
		s.caps = map[string]capsAnswer{}
	}
	s.caps[gateway] = capsAnswer{yes: yes, at: time.Now()}
	s.outsideMu.Unlock()
	return yes
}

// askPanePrefix sends GET /_caps to a loopback gateway and reads
// pane_prefix. It follows no redirect and reads at most 1 KiB.
func askPanePrefix(gateway string) bool {
	client := http.Client{
		Timeout:       capsTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(strings.TrimRight(gateway, "/") + "/_caps")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var caps struct {
		PanePrefix bool `json:"pane_prefix"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&caps); err != nil {
		return false
	}
	return caps.PanePrefix
}
