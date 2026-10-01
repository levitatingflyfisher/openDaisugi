package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
)

// The model and router swaps (F12a). A model swap on claude is /model NAME
// through the ready queue; a router swap stops the agent and resumes its
// conversation with a new base URL in the env.

func TestSetModelOnClaudeWaitsForTheIdlePrompt(t *testing.T) {
	s := readyServer(t)
	wait, open := gate(t)
	id := startFake(t, s, "claude", `stty -echo; echo loading; `+wait+
		`printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "m", "cmd": "agent.set_model", "pane": id, "model": "opus"})
	if !got.OK {
		t.Fatalf("agent.set_model: %+v", got.Error)
	}
	res, _ := got.Result.(map[string]any)
	if res["when"] != "queued" || res["queued"] != float64(1) || res["note"] != "queued until fake is ready" {
		t.Fatalf("reply = %v, want queued 1 with the note", res)
	}
	if strings.Contains(screenOf(t, s, id), "GOT") {
		t.Fatal("the model line reached the pane before its input box showed")
	}
	open()
	waitScreen(t, s, id, "GOT /model opus")
}

// With no verdict from the screen reader (no manifest tick yet, so nothing
// can say the prompt is idle), /model waits in the ready queue like it does
// for a busy screen. It is never typed at once.
func TestSetModelWithNoScreenVerdictWritesNothingUntilTheQueueSeesIdle(t *testing.T) {
	s, set := newDetectServer(t)
	id := startFake(t, s, "claude", `stty -echo; printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "m", "cmd": "agent.set_model", "pane": id, "model": "opus"})
	if !got.OK {
		t.Fatalf("agent.set_model: %+v", got.Error)
	}
	res, _ := got.Result.(map[string]any)
	if res["when"] != "queued" || res["queued"] != float64(1) {
		t.Fatalf("reply = %v, want queued 1", res)
	}
	time.Sleep(700 * time.Millisecond)
	if strings.Contains(screenOf(t, s, id), "GOT") {
		t.Fatal("the model line was typed with no verdict from the screen")
	}
	s.StartManifestTick(set)
	t.Cleanup(s.StopManifestTick)
	waitScreen(t, s, id, "GOT /model opus")
}

func TestSetModelRefusesNamesThatAreNotPlain(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `stty -echo; printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	for _, m := range []string{"", "two words", "a\nb", "-rf", "a;b", strings.Repeat("a", 65), "ñ"} {
		got := call(t, s, map[string]any{"id": "m", "cmd": "agent.set_model", "pane": id, "model": m})
		if got.OK || got.Error.Code != "bad_request" || got.Error.Message != ModelWordsRefusal {
			t.Fatalf("model %q: %+v", m, got)
		}
	}
	if strings.Contains(screenOf(t, s, id), "GOT") {
		t.Fatal("a refused name reached the pane")
	}
}

func TestSetModelKeepsTheNoteForOtherHarnesses(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	if !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Harness = "pi" }); err != nil {
		t.Fatal(err)
	}
	got = roundTrip(t, s, `{"id":"2","cmd":"agent.set_model","pane":"w1:p1","model":"opus"}`)
	if got[0].OK || got[0].Error.Code != "bad_request" ||
		got[0].Error.Message != "Swapping the model of auth fix is not built for pi yet." {
		t.Fatalf("set_model on pi: %+v", got[0])
	}
	// A model on a prompt is refused the same way, and the prompt is not sent.
	got = roundTrip(t, s, `{"id":"3","cmd":"agent.prompt","pane":"w1:p1","text":"hi","model":"opus"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "not built for") {
		t.Fatalf("agent.prompt with a model on a pty pane: %+v", got[0])
	}
}

func TestTheSwapVerbsAreRefusedFromAPaneAPluginAndAnUnplacedPeer(t *testing.T) {
	s := newAgentServer(t)
	if got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1)); !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	for _, f := range []*peerFacts{
		{checked: true, pane: true, paneID: "w1:p1"},
		{checked: true, plugin: "p"},
		{checked: true, unknown: true},
	} {
		for line, want := range map[string]string{
			`{"id":"1","cmd":"agent.set_model","pane":"w1:p1","model":"opus"}`:      ModelRefusal,
			`{"id":"2","cmd":"agent.set_router","pane":"w1:p1","router":"direct"}`:  RouterRefusal,
			`{"id":"3","cmd":"agent.prompt","pane":"w1:p1","text":"x","model":"o"}`: ModelRefusal,
			`{"id":"4","cmd":"pane.resume","pane":"w1:p1","router":"direct"}`:       RouterRefusal,
		} {
			send, next, stop := streamFacts(t, s, f)
			send(line)
			r := next()
			stop()
			e, _ := r["error"].(map[string]any)
			if r["ok"] != false || e["code"] != "unauthorized" || e["message"] != want {
				t.Fatalf("%+v %s: %v", f, line, r)
			}
		}
	}
}

// swapRouterServer is a server whose config names a gateway and a claude
// harness that resumes with an echo, and one live claude pane that has a
// conversation to resume.
func swapRouterServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gw, _ := capsGateway(t, 200, capsYes) // a gateway that answers
	if err := config.Save(config.Config{Gateway: &gw, Harness: map[string]config.Harness{
		"claude": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"echo RESUMED-{session}; sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	s.RegisterAgentCommands()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","harness":"claude","kind":"pty","args":["sleep 30"]}`)
	id, _ := result(t, got[0])["pane"].(string)
	if err := s.updatePane(id, func(p *layout.Pane) { p.HarnessSessionID = "sess-1" }); err != nil {
		t.Fatal(err)
	}
	return s, id, gw
}

func TestSetRouterRestartsAndResumesWithTheGatewayInTheEnv(t *testing.T) {
	s, id, gw := swapRouterServer(t)
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"`+id+`","router":"gateway"}`)
	m := result(t, got[0])
	newID, _ := m["pane"].(string)
	if m["resumed"] != true || m["router"] != "gateway" || newID == "" || newID == id {
		t.Fatalf("reply = %v, want resumed on a new pane", m)
	}
	rec, ok := s.tree.Pane(newID)
	if !ok || rec.Env["ANTHROPIC_BASE_URL"] != gw {
		t.Fatalf("new record env = %v, want ANTHROPIC_BASE_URL %s", rec.Env, gw)
	}
	if _, ok := s.tree.Pane(id); ok {
		t.Fatal("the old record stayed after the resume")
	}
	if r := s.stackOf(rec, s.viewFacts(rec))["router"]; r != "gateway" {
		t.Fatalf("stack.router = %v, want gateway", r)
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.wait_output","pane":"`+newID+`","contains":"RESUMED-sess-1","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("the new pane never resumed the session: %+v", got[0].Error)
	}
}

func TestSetRouterDirectNamesTheVendorAddressNeverAMissingKey(t *testing.T) {
	s, id, gw := swapRouterServer(t)
	if err := s.updatePane(id, func(p *layout.Pane) { p.Env = map[string]string{"ANTHROPIC_BASE_URL": gw} }); err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"`+id+`","router":"direct"}`)
	newID, _ := result(t, got[0])["pane"].(string)
	rec, _ := s.tree.Pane(newID)
	if rec.Env["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Fatalf("env = %v, want the vendor address", rec.Env)
	}
	if r := s.stackOf(rec, s.viewFacts(rec))["router"]; r != "direct" {
		t.Fatalf("stack.router = %v, want direct", r)
	}
}

func TestSetRouterRefusesWhatWouldLoseTheConversationOrHasNoGateway(t *testing.T) {
	s, id, _ := swapRouterServer(t)
	if err := s.updatePane(id, func(p *layout.Pane) { p.HarnessSessionID = "" }); err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"`+id+`","router":"gateway"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "no conversation to resume") {
		t.Fatalf("no session id: %+v", got[0])
	}
	if _, ok := s.Live(id); !ok {
		t.Fatal("a refused swap stopped the agent")
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"agent.set_router","pane":"`+id+`","router":"switchyard"}`)
	if got[0].OK || got[0].Error.Message != RouterWordsRefusal {
		t.Fatalf("switchyard: %+v", got[0])
	}
	// No gateway set: a gateway swap is refused, direct still works.
	empty := ""
	if err := config.Save(config.Config{Gateway: &empty, Harness: map[string]config.Harness{
		"claude": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"echo RESUMED-{session}; sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s.outsideMu.Lock()
	s.outside = outsideCache{}
	s.outsideMu.Unlock()
	if err := s.updatePane(id, func(p *layout.Pane) { p.HarnessSessionID = "sess-1" }); err != nil {
		t.Fatal(err)
	}
	got = roundTrip(t, s, `{"id":"4","cmd":"agent.set_router","pane":"`+id+`","router":"gateway"}`)
	if got[0].OK || !strings.Contains(got[0].Error.Message, "no gateway is set") {
		t.Fatalf("no gateway: %+v", got[0])
	}
}

func TestSetRouterKeepsTheNoteForOtherHarnesses(t *testing.T) {
	s := newAgentServer(t)
	s.RegisterEndedCommands()
	if got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1)); !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Harness = "codex" }); err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"w1:p1","router":"direct"}`)
	if got[0].OK || got[0].Error.Message != "Swapping the router of auth fix is not built for codex yet." {
		t.Fatalf("codex: %+v", got[0])
	}
}

func TestPaneResumeTakesARouterForAnEndedRecord(t *testing.T) {
	s := newEndedServer(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gw := "http://127.0.0.1:18787"
	if err := config.Save(config.Config{Gateway: &gw}); err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`)
	id, _ := result(t, got[0])["pane"].(string)
	waitDone(t, s, id)
	got = roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`","router":"switchyard"}`)
	if got[0].OK || got[0].Error.Message != RouterWordsRefusal {
		t.Fatalf("a bad router word: %+v", got[0])
	}
	got = roundTrip(t, s, `{"id":"3","cmd":"pane.resume","pane":"`+id+`","router":"gateway"}`)
	// The shell pane has no harness, so no env key is built for it.
	if got[0].OK || !strings.Contains(got[0].Error.Message, "not built for") {
		t.Fatalf("a pane with no harness: %+v", got[0])
	}
}

// A router swap stops the old agent before the new one starts. When the
// new start fails, the swap tries one plain resume with the old env, and
// says in both cases that the router did not change.

func TestSetRouterThatFailsToStartResumesTheOldRouterAndSaysWhy(t *testing.T) {
	s, id, gw := swapRouterServer(t)
	s.startVeto = func(rec layout.Pane) error {
		if rec.Env["ANTHROPIC_BASE_URL"] == gw {
			return fmt.Errorf("the new router would not start")
		}
		return nil
	}
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"`+id+`","router":"gateway"}`)
	if got[0].OK {
		t.Fatalf("a failed swap answered ok: %+v", got[0])
	}
	msg := got[0].Error.Message
	if !strings.Contains(msg, "router of ") || !strings.Contains(msg, "did not change") ||
		!strings.Contains(msg, "the new router would not start") {
		t.Fatalf("message = %q, want the router unchanged and the reason", msg)
	}
	var live string
	for _, p := range s.tree.Panes() {
		if _, ok := s.Live(p.ID); ok {
			live = p.ID
		}
	}
	rec, ok := s.tree.Pane(live)
	if !ok || rec.Env["ANTHROPIC_BASE_URL"] == gw {
		t.Fatalf("no live pane on the old env after the retry: %v", rec.Env)
	}
}

func TestSetRouterThatCannotStartAnythingSaysTheAgentIsInTheEndedList(t *testing.T) {
	s, id, _ := swapRouterServer(t)
	label := s.labelOf(id)
	s.startVeto = func(layout.Pane) error { return fmt.Errorf("no start") }
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"`+id+`","router":"gateway"}`)
	want := label + " was stopped and its router did not change. It is in the ended list. " +
		"To start it again, run: coppice pane resume " + id + "."
	if got[0].OK || got[0].Error.Message != want {
		t.Fatalf("reply = %+v, want error %q", got[0], want)
	}
	if rec, ok := s.tree.Pane(id); !ok || !rec.Closed {
		t.Fatalf("the old record is not in the ended list: %+v", rec)
	}
}
