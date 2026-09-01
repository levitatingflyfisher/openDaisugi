package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// askingProc is a headless harness that holds asks of its own, as OpenCode
// does. It records who typed each prompt and each answer it gets.
type askingProc struct {
	*fakeProc
	mu       sync.Mutex
	prompts  []string // "text|operator" or "text|pane"
	answers  []string // "id|allow" or "id|deny"
	openAsks map[string]bool
}

func (p *askingProc) PromptFrom(text string, operator bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	who := "pane"
	if operator {
		who = "operator"
	}
	p.prompts = append(p.prompts, text+"|"+who)
	return nil
}

func (p *askingProc) OwnsAsk(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openAsks[id]
}

func (p *askingProc) Answer(id string, allow bool, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := "deny"
	if allow {
		d = "allow"
	}
	p.answers = append(p.answers, id+"|"+d)
	delete(p.openAsks, id)
	return nil
}

func (p *askingProc) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...), append([]string(nil), p.answers...)
}

func startAskingPane(t *testing.T, name string) (*Server, *askingProc, string) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	s := newAgentServer(t)
	proc := &askingProc{fakeProc: newFakeProc(), openAsks: map[string]bool{"per_1": true}}
	adapters.Register(&askingAdapter{name: name, proc: proc})
	id := createHeadlessPane(t, s, name, "oc pane")
	t.Cleanup(func() { proc.closeIn() })
	proc.send(pane.Event{Kind: pane.EvState, State: pane.StateBlockedStr, Ask: &proto.Ask{
		ID: "per_1", Tool: "bash", Summary: "rm -rf build", Deadline: float64(time.Now().Add(time.Hour).Unix()),
	}})
	waitFor(t, 2*time.Second, func() bool { _, held := s.heldAsk(id, "per_1"); return held })
	return s, proc, id
}

type askingAdapter struct {
	name string
	proc *askingProc
}

func (a *askingAdapter) Name() string { return a.name }

func (a *askingAdapter) Start(_ context.Context, _ pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	return a.proc, nil
}

func TestAnOperatorAllowReachesTheHarnessOwnAskWithThePaneName(t *testing.T) {
	s, proc, id := startAskingPane(t, "fake-asking-allow")
	got := roundTrip(t, s,
		`{"id":"1","cmd":"agent.allow","pane":"`+id+`","ask":"per_1"}`,
		`{"id":"2","cmd":"agent.allow","pane":"`+id+`","ask":"per_1","confirm":"oc pane"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrUnauthorized || !strings.Contains(got[0].Error.Message, "oc pane") {
		t.Fatalf("allow without the name = %+v, want the name asked for", got[0])
	}
	if !got[1].OK {
		t.Fatalf("allow = %+v", got[1].Error)
	}
	if _, answers := proc.seen(); strings.Join(answers, ",") != "per_1|allow" {
		t.Fatalf("answers = %v", answers)
	}
}

func TestAnOperatorDenyReachesTheHarnessOwnAsk(t *testing.T) {
	s, proc, id := startAskingPane(t, "fake-asking-deny")
	got := roundTrip(t, s, `{"id":"1","cmd":"agent.deny","pane":"`+id+`","ask":"per_1"}`)
	if !got[0].OK {
		t.Fatalf("deny = %+v", got[0].Error)
	}
	if _, answers := proc.seen(); strings.Join(answers, ",") != "per_1|deny" {
		t.Fatalf("answers = %v", answers)
	}
}

func TestAPaneCannotAllowTheHarnessOwnAsk(t *testing.T) {
	s, proc, id := startAskingPane(t, "fake-asking-pane")
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w9:p9"}`)
	next()
	send(`{"id":"1","cmd":"agent.allow","pane":"` + id + `","ask":"per_1","confirm":"oc pane"}`)
	refusedWith(t, next())
	if _, answers := proc.seen(); len(answers) != 0 {
		t.Fatalf("a pane's allow reached the harness: %v", answers)
	}
}

// A prompt says who typed it, so the harness can refuse a pane's text as
// an answer to its own prompt.
func TestAPromptSaysWhetherTheOperatorTypedIt(t *testing.T) {
	s, proc, id := startAskingPane(t, "fake-asking-prompt")
	roundTrip(t, s, `{"id":"1","cmd":"agent.prompt","pane":"`+id+`","text":"y"}`)
	// Past the ask deadline a pane's typing is not refused by the blocked
	// guard, so the harness must learn the text came from a pane.
	s.forgetAsk(id, "per_1")
	s.ApplyState(id, proto.PaneStateEvent{V: 1, TS: nowSeconds(), SessionID: "s", Harness: "fake",
		Pane: &id, State: proto.StateWorking, Source: proto.SrcHeadless})
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w9:p9"}`)
	next()
	send(`{"id":"2","cmd":"agent.prompt","pane":"` + id + `","text":"y"}`)
	next()
	prompts, answers := proc.seen()
	if strings.Join(prompts, ",") != "y|operator,y|pane" {
		t.Fatalf("prompts = %v", prompts)
	}
	if len(answers) != 0 {
		t.Fatalf("a typed prompt answered an ask: %v", answers)
	}
}

// An ask a harness holds itself is marked, so the phone can say where to
// answer it instead of offering an Allow the gate cannot take.
func TestAHarnessHeldAskIsMarkedInTheAgentList(t *testing.T) {
	s, _, id := startAskingPane(t, "fake-asking-mark")
	got := roundTrip(t, s, `{"id":"1","cmd":"agent.list"}`)
	agents, _ := result(t, got[0])["agents"].([]any)
	for _, raw := range agents {
		row, _ := raw.(map[string]any)
		if row["pane"] != id {
			continue
		}
		ask, _ := row["ask"].(map[string]any)
		if ask["holder"] != proto.HolderHarness {
			t.Fatalf("ask = %v, want holder %q", ask, proto.HolderHarness)
		}
		return
	}
	t.Fatalf("no row for %s in %v", id, agents)
}

// Only the server marks an ask as harness-held. A report cannot claim it.
func TestAReportCannotClaimAHarnessHeldAsk(t *testing.T) {
	s := newAgentServer(t)
	line := strings.Replace(gateBlockedLine("w1:p1"), `"deadline":9999999999.0}`, `"deadline":9999999999.0,"holder":"harness"}`, 1)
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1), line,
		`{"id":"3","cmd":"agent.list"}`)
	agents, _ := result(t, got[2])["agents"].([]any)
	row, _ := agents[0].(map[string]any)
	ask, _ := row["ask"].(map[string]any)
	if _, has := ask["holder"]; has {
		t.Fatalf("a report claimed a harness-held ask: %v", ask)
	}
}

// The gate plugin reports the same ask through the gate after the
// adapter's own event. The mark must survive it, or the phone offers an
// Allow the gate cannot take.
func TestThePluginsReportOfAHarnessHeldAskKeepsTheMark(t *testing.T) {
	s, _, id := startAskingPane(t, "fake-asking-plugin")
	report := func(ask string) {
		roundTrip(t, s, `{"id":"9","cmd":"pane.report_state","pane":"`+id+`","event":`+
			`{"v":1,"ts":`+fmt.Sprint(nowSeconds())+`,"session_id":"ses_1","harness":"opencode","pane":"`+id+
			`","state":"blocked","source":"headless","ask":{"id":"`+ask+`","tool":"bash",`+
			`"summary":"rm -rf build","deadline":9999999999.0}}}`)
	}
	holder := func() any {
		got := roundTrip(t, s, `{"id":"1","cmd":"agent.list"}`)
		agents, _ := result(t, got[0])["agents"].([]any)
		for _, raw := range agents {
			row, _ := raw.(map[string]any)
			if row["pane"] == id {
				ask, _ := row["ask"].(map[string]any)
				return ask["holder"]
			}
		}
		t.Fatalf("no row for %s", id)
		return nil
	}
	report("per_1")
	if h := holder(); h != proto.HolderHarness {
		t.Fatalf("after the plugin's report holder = %v, want %q", h, proto.HolderHarness)
	}
	// An ask the harness does not own gets no mark.
	report("per_other")
	if h := holder(); h != nil {
		t.Fatalf("an ask the harness does not own got holder %v", h)
	}
}

// The web page answers an ask a harness holds over its own connection:
// the hello coppice web sends for a browser, then agent.allow with the
// typed pane name. A pane-placed shell is refused, as
// TestAPaneCannotAllowTheHarnessOwnAsk shows.
func TestThePagesOwnConnectionAnswersAHarnessHeldAsk(t *testing.T) {
	s, proc, id := startAskingPane(t, "fake-asking-web")
	hello := `{"id":"h","cmd":"hello","name_from":"token","name":"phone"}`
	got := roundTrip(t, s, hello,
		`{"id":"1","cmd":"agent.allow","pane":"`+id+`","ask":"per_1","confirm":"oc pane","reason":"allowed from the phone"}`)
	if !got[0].OK || !got[1].OK {
		t.Fatalf("hello %+v, allow %+v", got[0], got[1])
	}
	if _, answers := proc.seen(); strings.Join(answers, ",") != "per_1|allow" {
		t.Fatalf("answers = %v", answers)
	}
}
