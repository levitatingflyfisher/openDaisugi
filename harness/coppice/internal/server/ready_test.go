package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// trustScreen is the real Claude Code 2.1.282 trust screen from the
// fixture, with its header line cut off.
func trustScreen(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "screens", "claude", "blocked-10.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if i := strings.Index(text, "\n"); strings.HasPrefix(text, "#rule:") && i >= 0 {
		text = text[i+1:]
	}
	return text
}

// promptBox is a screen claude's live_prompt_box rule reads as idle.
const promptBox = `────────────────────\n❯ \n────────────────────\n`

// readyServer is a server with the pane and agent verbs and the manifest
// tick, which turns the prompt gate on.
func readyServer(t *testing.T) *Server {
	t.Helper()
	s, set := newDetectServer(t)
	s.StartManifestTick(set)
	t.Cleanup(s.StopManifestTick)
	return s
}

// startFake starts a pty pane labelled "fake" that runs script under bash,
// with harness as its harness. It answers the pane id.
func startFake(t *testing.T, s *Server, harness, script string) string {
	t.Helper()
	req := map[string]any{
		"id": "c", "cmd": "pane.create", "cwd": t.TempDir(), "kind": "pty",
		"cmd_argv": []string{"bash", "-c", script}, "label": "fake", "cols": 120, "rows": 30,
	}
	if harness != "" {
		req["harness"] = harness
	}
	got := call(t, s, req)
	if !got.OK {
		t.Fatalf("pane.create: %+v", got.Error)
	}
	res, _ := got.Result.(map[string]any)
	id, _ := res["pane"].(string)
	return id
}

// call sends one request and answers its reply.
func call(t *testing.T, s *Server, req map[string]any) proto.Response {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return roundTrip(t, s, string(b))[0]
}

// screenOf reads pane id's visible screen.
func screenOf(t *testing.T, s *Server, id string) string {
	t.Helper()
	got := call(t, s, map[string]any{"id": "r", "cmd": "pane.read", "pane": id})
	res, _ := got.Result.(map[string]any)
	text, _ := res["text"].(string)
	return text
}

// waitManifest waits up to 8 s for pane id's merged state to be st from the
// manifest, and answers that event.
func waitManifest(t *testing.T, s *Server, id, st string) proto.PaneStateEvent {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for {
		ev, ok := s.States().Current(id)
		if ok && ev.State == st && ev.Source == proto.SrcManifest {
			return ev
		}
		if time.Now().After(end) {
			t.Fatalf("state = %+v, want %s from the manifest", ev, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// showTrust is a script that draws the trust screen and then runs rest.
func showTrust(t *testing.T, rest string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "trust.txt")
	if err := os.WriteFile(f, []byte(trustScreen(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	return "cat " + f + "; " + rest
}

// Claude's folder trust screen reads blocked, and the row says why in
// words.
func TestTheTrustScreenNeedsYouWithItsWords(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, "sleep 10"))
	ev := waitManifest(t, s, id, proto.StateBlocked)
	if !strings.HasPrefix(ev.Detail, "asks to trust this folder") || !IsTrustDetail(ev.Detail) {
		t.Fatalf("detail = %q, want the trust words and rule", ev.Detail)
	}
}

// A prompt sent while the harness starts waits in the queue, and goes once
// the input box shows.
func TestAPromptWaitsWhileThePaneStartsAndGoesWhenReady(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `stty -echo; echo loading; `+
		`if read -t 1.5 early; then echo "EARLY $early"; fi; `+
		`printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "hello"})
	if !got.OK {
		t.Fatalf("send_text: %+v", got.Error)
	}
	res, _ := got.Result.(map[string]any)
	if res["queued"] != float64(1) || res["note"] != "queued until fake is ready" {
		t.Fatalf("reply = %v, want queued 1 with the note", res)
	}
	text := waitScreen(t, s, id, "GOT hello")
	if strings.Contains(text, "EARLY") {
		t.Fatalf("the prompt reached the pane before its input box showed:\n%s", text)
	}
}

// agent.prompt to a pty pane goes through the same queue, and says so.
func TestAgentPromptIsQueuedToo(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `stty -echo; echo loading; sleep 1; `+
		`printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "p", "cmd": "agent.prompt", "pane": id, "text": "hi there"})
	if !got.OK {
		t.Fatalf("agent.prompt: %+v", got.Error)
	}
	res, _ := got.Result.(map[string]any)
	if res["note"] != "queued until fake is ready" {
		t.Fatalf("reply = %v, want the queued note", res)
	}
	waitScreen(t, s, id, "GOT hi there")
}

// A prompt to a pane on its trust screen is refused, and nothing reaches
// the pane: an Enter there would answer the question.
func TestAPromptToAPaneOnAQuestionIsRefused(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty -echo; read x; echo "ANSWERED $x"; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	want := fmt.Sprintf(TrustQuestionRefusal, "fake", id)
	for _, cmd := range []string{"pane.send_text", "agent.prompt"} {
		got := call(t, s, map[string]any{"id": "p", "cmd": cmd, "pane": id, "text": "hello"})
		if got.OK || got.Error.Code != proto.ErrBadRequest || got.Error.Message != want {
			t.Fatalf("%s reply = %+v, want the refusal %q", cmd, got, want)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if text := screenOf(t, s, id); strings.Contains(text, "ANSWERED") {
		t.Fatalf("a refused prompt reached the pane:\n%s", text)
	}
}

// Raw text and keys are the owner's own keys. They pass a question on the
// screen, never queued and never refused.
func TestRawKeysPassThroughAQuestion(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty -echo; read x; echo "ANSWERED $x"; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "ab", "enter": false})
	if !got.OK {
		t.Fatalf("raw send_text: %+v", got.Error)
	}
	if res, _ := got.Result.(map[string]any); res["queued"] != nil {
		t.Fatalf("raw text was queued: %v", res)
	}
	got = call(t, s, map[string]any{"id": "k", "cmd": "pane.send_keys", "pane": id, "keys": []string{"c", "enter"}})
	if !got.OK {
		t.Fatalf("send_keys: %+v", got.Error)
	}
	waitScreen(t, s, id, "ANSWERED abc")
}

// The queue of one pane is capped in count and in bytes.
func TestThePromptQueueIsCapped(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `echo loading; sleep 20`)
	for i := 0; i < maxPromptQueue; i++ {
		got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "x"})
		if !got.OK {
			t.Fatalf("prompt %d: %+v", i+1, got.Error)
		}
	}
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "x"})
	if got.OK || !strings.Contains(got.Error.Message, "has 16 prompts, 16 bytes, waiting already") {
		t.Fatalf("prompt past the count cap = %+v, want a refusal", got)
	}

	id2 := startFake(t, s, "claude", `echo loading; sleep 20`)
	half := strings.Repeat("y", maxPromptBytes/2+1)
	if got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id2, "text": half}); !got.OK {
		t.Fatalf("first half: %+v", got.Error)
	}
	got = call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id2, "text": half})
	if got.OK || !strings.Contains(got.Error.Message, "A queue holds 16 prompts and 65536 bytes") {
		t.Fatalf("prompt past the byte cap = %+v, want a refusal", got)
	}
}

// One prompt past the byte cap is refused on its own, even to a pane that
// is ready, and nothing reaches the pane.
func TestAnOversizedPromptIsRefusedEvenWhenReady(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `printf '`+promptBox+`'; stty -echo; read x; echo "GOT"; sleep 10`)
	waitManifest(t, s, id, proto.StateIdle)
	big := strings.Repeat("y", maxPromptBytes+1)
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": big})
	want := fmt.Sprintf("the prompt is %d bytes. A prompt may be %d bytes at most.", maxPromptBytes+1, maxPromptBytes)
	if got.OK || got.Error.Message != want {
		t.Fatalf("reply = %+v, want %q", got, want)
	}
}

// A pane that reads ready gets the prompt at once, with no queue, once it
// has stayed ready for readySettle.
func TestAnIdlePaneGetsThePromptAtOnceAfterTheSettle(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `printf '`+promptBox+`'; stty -echo; read x; echo "GOT $x"; sleep 10`)
	waitManifest(t, s, id, proto.StateIdle)
	start := time.Now()
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "now"})
	took := time.Since(start)
	res, _ := got.Result.(map[string]any)
	if !got.OK || res["queued"] != nil || res["sent"] != float64(4) {
		t.Fatalf("reply = %+v, want sent at once", got)
	}
	if took < readySettle {
		t.Fatalf("the prompt went after %v, want the %v settle first", took, readySettle)
	}
	waitScreen(t, s, id, "GOT now")
}

// Several queued prompts go in the order they came.
func TestQueuedPromptsGoInOrder(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `stty -echo; echo loading; sleep 1; `+
		`for i in 1 2 3; do clear; printf '`+promptBox+`'; read x; echo "GOT$i $x"; sleep 0.5; done; sleep 10`)
	for _, w := range []string{"first", "second", "third"} {
		got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": w})
		if res, _ := got.Result.(map[string]any); !got.OK || res["queued"] == nil {
			t.Fatalf("prompt %s = %+v, want it queued", w, got)
		}
	}
	waitScreen(t, s, id, "GOT1 first")
	waitScreen(t, s, id, "GOT2 second")
	waitScreen(t, s, id, "GOT3 third")
}

// A question that shows while prompts wait holds them: nothing goes into
// it, and they go once the input box is back.
func TestAQuestionWhilePromptsWaitHoldsThem(t *testing.T) {
	s := readyServer(t)
	f := filepath.Join(t.TempDir(), "trust.txt")
	if err := os.WriteFile(f, []byte(trustScreen(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	id := startFake(t, s, "claude", `stty -echo; echo loading; sleep 0.5; clear; cat `+f+`; `+
		`if read -t 2 early; then echo "EARLY $early"; fi; clear; printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "held"})
	if res, _ := got.Result.(map[string]any); !got.OK || res["queued"] == nil {
		t.Fatalf("reply = %+v, want queued", got)
	}
	text := waitScreen(t, s, id, "GOT held")
	if strings.Contains(text, "EARLY") {
		t.Fatalf("a queued prompt went into the question:\n%s", text)
	}
}

// The drop timer runs only while the pane is not working, and starts
// again when the state changes.
func TestTheDropTimerSkipsWorkingTime(t *testing.T) {
	old := promptQueueWait
	promptQueueWait = 1500 * time.Millisecond
	t.Cleanup(func() { promptQueueWait = old })
	s := readyServer(t)
	working := `✻ Thinking… (3s · esc to interrupt)`
	id := startFake(t, s, "claude", `stty -echo; printf '`+working+`\n'; sleep 3; `+
		`clear; printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	// The process source speaks while the screen changes, so the merged
	// state is not the manifest's. The queue reads the screen itself.
	time.Sleep(500 * time.Millisecond)
	if v, ok := s.screenVerdict(id); !ok || v.State != "working" {
		t.Fatalf("the screen reads %+v, want working", v)
	}
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "late"})
	if res, _ := got.Result.(map[string]any); !got.OK || res["queued"] == nil {
		t.Fatalf("reply = %+v, want queued", got)
	}
	waitScreen(t, s, id, "GOT late")
}

// A pane that stays in one state other than working past the wait drops
// its queue, and the note says so by label.
func TestTheDropTimerDropsAStuckQueue(t *testing.T) {
	old := promptQueueWait
	promptQueueWait = 800 * time.Millisecond
	t.Cleanup(func() { promptQueueWait = old })
	s := readyServer(t)
	id := startFake(t, s, "claude", `echo loading; sleep 20`)
	got := call(t, s, map[string]any{"id": "p", "cmd": "agent.prompt", "pane": id, "text": "x", "wait": true, "timeout_ms": 5000})
	if got.OK || got.Error == nil || !strings.Contains(got.Error.Message, "not ready in time") {
		t.Fatalf("a waiting sender got %+v, want the drop as its error", got)
	}
	waitNote(t, s, "fake was not ready in 0 min, so 1 prompt was not sent.")
}

// A pane that ends drops its queue with a note that names it by label,
// also after pane.close removed its record.
func TestAPaneThatEndsDropsItsQueueWithANote(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `echo loading; sleep 20`)
	for i := 0; i < 2; i++ {
		if got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "x"}); !got.OK {
			t.Fatalf("send: %+v", got.Error)
		}
	}
	if got := call(t, s, map[string]any{"id": "c", "cmd": "pane.close", "pane": id}); !got.OK {
		t.Fatalf("close: %+v", got.Error)
	}
	waitNote(t, s, "fake ended, so 2 prompts were not sent.")
}

// A pane that sent a prompt hears of the drop: a note to it, and one line
// in its own prompt.
func TestADropIsToldToTheSendingPane(t *testing.T) {
	s := readyServer(t)
	sender := startFake(t, s, "claude", `printf '`+promptBox+`'; stty -echo; read x; echo "TOLD $x"; sleep 20`)
	waitManifest(t, s, sender, proto.StateIdle)
	id := startFake(t, s, "claude", `echo loading; sleep 20`)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: sender})
	defer stop()
	send(`{"id":"1","cmd":"agent.prompt","pane":"` + id + `","text":"hello"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("agent.prompt from a pane = %v", r)
	}
	if got := call(t, s, map[string]any{"id": "c", "cmd": "pane.close", "pane": id}); !got.OK {
		t.Fatalf("close: %+v", got.Error)
	}
	waitScreen(t, s, sender, "TOLD coppice: fake ended, so 1 prompt was not sent.")
	for _, n := range s.notesSnapshot() {
		if n.To == sender && strings.Contains(n.Text, "fake ended") {
			return
		}
	}
	t.Fatal("no note was addressed to the sending pane")
}

// The second Enter goes at most once, even when the box keeps the text.
func TestTheSecondEnterGoesAtMostOnce(t *testing.T) {
	s := readyServer(t)
	stuck := `────────────────────\n❯ stuck prompt text\n────────────────────\n`
	// The pane reads the text and two Enters, then shows whatever else
	// comes in the next 3 s after MORE.
	id := startFake(t, s, "claude", `printf '`+stuck+`'; stty raw -echo; head -c 19 | od -An -c; `+
		`timeout 3 cat | od -An -c | sed 's/^/MORE/'; echo DONE; sleep 10`)
	waitManifest(t, s, id, proto.StateIdle)
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "stuck prompt text"})
	if !got.OK {
		t.Fatalf("send: %+v", got.Error)
	}
	text := waitScreen(t, s, id, "DONE")
	if strings.Count(text, `\r`) != 2 || strings.Contains(text, "MORE") {
		t.Fatalf("the pane did not get the Enter and exactly one more:\n%s", text)
	}
}

// When the server can no longer read the pane's screen against its
// manifest, the queue holds. It never sends blind.
func TestAQueueHoldsWhenItCannotReadTheScreen(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `stty -echo; echo loading; sleep 0.5; `+
		`printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "blind"})
	if res, _ := got.Result.(map[string]any); !got.OK || res["queued"] == nil {
		t.Fatalf("reply = %+v, want queued", got)
	}
	s.StopManifestTick()
	time.Sleep(2 * time.Second)
	if text := screenOf(t, s, id); strings.Contains(text, "GOT") {
		t.Fatalf("the queue sent a prompt it could not check:\n%s", text)
	}
}

// A drop for a timeout keeps the trust answer's grace, so a second
// pane.trust right after is still refused.
func TestADropKeepsTheTrustGrace(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty raw -echo; sleep 20`))
	waitManifest(t, s, id, proto.StateBlocked)
	if got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id}); !got.OK {
		t.Fatalf("first pane.trust: %+v", got.Error)
	}
	s.promptMu.Lock()
	s.prompts[id] = &promptQueue{label: "fake", items: []queuedPrompt{{text: "x", done: make(chan error, 1)}}}
	s.promptMu.Unlock()
	s.dropPrompts(id, errPromptTimeout)
	got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id})
	if got.OK || !strings.Contains(got.Error.Message, "has an answer to its trust screen already") {
		t.Fatalf("pane.trust after a timeout drop = %+v, want a refusal", got)
	}
}

// A pane that sent agent.prompt with wait, and gave up waiting before the
// prompt went, still hears of a later drop.
func TestADropReachesASenderWhoseWaitTimedOut(t *testing.T) {
	s := readyServer(t)
	sender := startFake(t, s, "claude", `printf '`+promptBox+`'; stty -echo; read x; echo "TOLD $x"; sleep 20`)
	waitManifest(t, s, sender, proto.StateIdle)
	id := startFake(t, s, "claude", `echo loading; sleep 20`)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: sender})
	defer stop()
	send(`{"id":"1","cmd":"agent.prompt","pane":"` + id + `","text":"hello","wait":true,"timeout_ms":100}`)
	if r := next(); r["ok"] != false {
		t.Fatalf("agent.prompt with a short wait = %v, want its timeout", r)
	}
	if got := call(t, s, map[string]any{"id": "c", "cmd": "pane.close", "pane": id}); !got.OK {
		t.Fatalf("close: %+v", got.Error)
	}
	waitScreen(t, s, sender, "TOLD coppice: fake ended, so 1 prompt was not sent.")
}

// A sender still waiting when the drop comes gets it as its error, and
// no second word of it.
func TestAWaitingSenderHearsTheDropOnce(t *testing.T) {
	old := promptQueueWait
	promptQueueWait = 800 * time.Millisecond
	t.Cleanup(func() { promptQueueWait = old })
	s := readyServer(t)
	sender := startFake(t, s, "claude", `printf '`+promptBox+`'; stty -echo; read x; echo "TOLD $x"; sleep 20`)
	waitManifest(t, s, sender, proto.StateIdle)
	id := startFake(t, s, "claude", `echo loading; sleep 20`)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: sender})
	defer stop()
	send(`{"id":"1","cmd":"agent.prompt","pane":"` + id + `","text":"hello","wait":true,"timeout_ms":8000}`)
	r := next()
	e, _ := r["error"].(map[string]any)
	if r["ok"] != false || !strings.Contains(fmt.Sprint(e["message"]), "not ready in time") {
		t.Fatalf("a waiting sender got %v, want the drop as its error", r)
	}
	time.Sleep(1500 * time.Millisecond)
	if text := screenOf(t, s, sender); strings.Contains(text, "TOLD") {
		t.Fatalf("a sender that got the error was told again:\n%s", text)
	}
}

// notesSnapshot is a copy of the server's notes.
func (s *Server) notesSnapshot() []note {
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	return append([]note(nil), s.notes...)
}

// waitNote waits up to 8 s for a floor note whose text is want.
func waitNote(t *testing.T, s *Server, want string) {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		for _, n := range s.notesSnapshot() {
			if n.Text == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no note %q; notes: %+v", want, s.notesSnapshot())
}

// A pane whose harness has no manifest, or a manifest with no idle rule,
// gets a prompt at once, as before.
func TestNoManifestMeansTodaysBehaviour(t *testing.T) {
	s := readyServer(t)
	// pi has a manifest with no idle rule, so it can never read ready.
	for _, harness := range []string{"", "no-such-harness", "pi"} {
		id := startFake(t, s, harness, `stty -echo; echo loading; read x; echo "GOT $x"; sleep 10`)
		got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "now"})
		if !got.OK {
			t.Fatalf("send_text: %+v", got.Error)
		}
		res, _ := got.Result.(map[string]any)
		if res["queued"] != nil || res["sent"] != float64(4) {
			t.Fatalf("harness %q reply = %v, want sent at once", harness, res)
		}
		waitScreen(t, s, id, "GOT now")
	}
}

// pane.run to a harness pane follows the prompt rules: refused on a
// question with nothing typed, and queued while the pane starts.
func TestPaneRunFollowsThePromptRules(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty -echo; read x; echo "ANSWERED $x"; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	want := fmt.Sprintf(TrustQuestionRefusal, "fake", id)
	for _, line := range []string{"make test", ""} {
		got := call(t, s, map[string]any{"id": "r", "cmd": "pane.run", "pane": id, "line": line})
		if got.OK || got.Error.Code != proto.ErrBadRequest || got.Error.Message != want {
			t.Fatalf("pane.run %q on a question = %+v, want the refusal", line, got)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if text := screenOf(t, s, id); strings.Contains(text, "ANSWERED") {
		t.Fatalf("a refused pane.run reached the pane:\n%s", text)
	}

	id2 := startFake(t, s, "claude", `stty -echo; echo loading; `+
		`if read -t 1.5 early; then echo "EARLY $early"; fi; `+
		`printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "r", "cmd": "pane.run", "pane": id2, "line": "make test"})
	res, _ := got.Result.(map[string]any)
	if !got.OK || res["queued"] != float64(1) || res["note"] != "queued until fake is ready" {
		t.Fatalf("pane.run while starting = %+v, want queued", got)
	}
	text := waitScreen(t, s, id2, "GOT make test")
	if strings.Contains(text, "EARLY") {
		t.Fatalf("pane.run reached the pane before its input box showed:\n%s", text)
	}
}

// A shell pane, with no harness, runs a line at once, as before.
func TestPaneRunInAShellIsUnchanged(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "", `stty -echo; echo loading; read x; echo "GOT $x"; sleep 10`)
	got := call(t, s, map[string]any{"id": "r", "cmd": "pane.run", "pane": id, "line": "ls"})
	res, _ := got.Result.(map[string]any)
	if !got.OK || res["queued"] != nil || res["sent"] != float64(3) {
		t.Fatalf("pane.run in a shell = %+v, want sent at once", got)
	}
	waitScreen(t, s, id, "GOT ls")
}

// recProc records every write to a pane, with its time.
type recProc struct {
	pane.Proc
	mu     sync.Mutex
	writes []string
	at     []time.Time
}

func (p *recProc) WriteStdin(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = append(p.writes, string(b))
	p.at = append(p.at, time.Now())
	return nil
}

// A prompt's Enter is its own write, pasteGap after the text, so Claude
// does not read the two as one paste.
func TestAPromptSendsItsEnterApart(t *testing.T) {
	s := newTestServer(t)
	rec := &recProc{}
	s.liveMu.Lock()
	s.live["w9:p9"] = &LivePane{Adapter: rec}
	s.liveMu.Unlock()
	t.Cleanup(func() {
		s.liveMu.Lock()
		delete(s.live, "w9:p9")
		s.liveMu.Unlock()
	})
	s.promptMu.Lock()
	s.promptSending["w9:p9"] = true
	s.promptMu.Unlock()
	if err := s.deliverPrompt("w9:p9", "say pong"); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.writes) != 2 || rec.writes[0] != "say pong" || rec.writes[1] != "\r" {
		t.Fatalf("writes = %q, want the text, then the Enter alone", rec.writes)
	}
	if gap := rec.at[1].Sub(rec.at[0]); gap < pasteGap {
		t.Fatalf("the Enter came %v after the text, want at least %v", gap, pasteGap)
	}
}

// The second Enter goes only when the input box still holds the text.
func TestBoxHolds(t *testing.T) {
	cases := []struct {
		box, text string
		want      bool
	}{
		{"❯ Reply with the single word pong.", "Reply with the single word pong.", true},
		{"❯ Reply with the single\n  word pong.", "Reply with the single word pong.", true},
		{"❯ [Pasted text #1 +3 lines]", "a\nb\nc\nd", true},
		{"❯ ", "Reply with the single word pong.", false},
		{"❯ something else", "hello", false},
	}
	for _, c := range cases {
		if got := boxHolds(c.box, c.text); got != c.want {
			t.Errorf("boxHolds(%q, %q) = %v, want %v", c.box, c.text, got, c.want)
		}
	}
}

// pane.trust moves the cursor from "No, exit" to the yes option and
// presses Enter, each key its own write.
func TestTrustChoosesYes(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty raw -echo; head -c 4 | od -An -tx1; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id})
	if !got.OK {
		t.Fatalf("pane.trust: %+v", got.Error)
	}
	waitScreen(t, s, id, "1b 5b 42 0d")
}

// A prompt sent right after Trust this folder, while Claude still draws
// the trust screen, waits in the queue and is not refused.
func TestAPromptRightAfterTrustIsQueued(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty raw -echo; head -c 4 >/dev/null; sleep 1; `+
		`stty sane -echo; clear; printf '`+promptBox+`'; read x; echo "GOT $x"; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	if got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id}); !got.OK {
		t.Fatalf("pane.trust: %+v", got.Error)
	}
	got := call(t, s, map[string]any{"id": "p", "cmd": "pane.send_text", "pane": id, "text": "hello"})
	if !got.OK {
		t.Fatalf("a prompt right after trust = %+v, want it queued", got.Error)
	}
	waitScreen(t, s, id, "GOT hello")
}

// pane.trust with trust false presses Esc.
func TestNotNowPressesEsc(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty raw -echo; head -c 1 | od -An -tx1; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id, "trust": false})
	if !got.OK {
		t.Fatalf("pane.trust: %+v", got.Error)
	}
	waitScreen(t, s, id, "1b")
}

// pane.trust refuses a pane that shows no trust screen, and sends it
// nothing.
func TestTrustRefusesAnotherScreen(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", `printf '`+promptBox+`'; sleep 10`)
	waitManifest(t, s, id, proto.StateIdle)
	got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id})
	if got.OK || got.Error.Message != "fake does not ask to trust a folder now." {
		t.Fatalf("reply = %+v, want the refusal", got)
	}
}

// A pane connection, such as the foreman, gets the same refusal for a
// prompt to a pane on a question, and may not answer the trust screen.
func TestAPaneGetsTheQuestionRefusalAndCannotTrust(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty -echo; read x; echo "ANSWERED $x"; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p99"})
	defer stop()
	send(`{"id":"1","cmd":"agent.prompt","pane":"` + id + `","text":"hello"}`)
	r := next()
	e, _ := r["error"].(map[string]any)
	if r["ok"] != false || e["message"] != fmt.Sprintf(TrustQuestionRefusal, "fake", id) {
		t.Fatalf("agent.prompt from a pane = %v, want the question refusal", r)
	}
	send(`{"id":"2","cmd":"pane.trust","pane":"` + id + `"}`)
	r = next()
	e, _ = r["error"].(map[string]any)
	if r["ok"] != false || e["code"] != "unauthorized" || e["message"] != TrustRefusal {
		t.Fatalf("pane.trust from a pane = %v, want %q", r, TrustRefusal)
	}
}

// trustMoves counts from the cursor to the yes option, in both orders.
func TestTrustMoves(t *testing.T) {
	cases := []struct {
		screen string
		want   int
	}{
		{" ❯ No, exit\n   Yes, I trust this folder\n", 1},
		{"   No, exit\n ❯ Yes, I trust this folder\n", 0},
		{" ❯ 1. Yes, I trust this folder\n   2. No, exit\n", 0},
		{"   1. Yes, I trust this folder\n ❯ 2. No, exit\n", -1},
	}
	for _, c := range cases {
		if got, ok := trustMoves(c.screen); !ok || got != c.want {
			t.Errorf("trustMoves(%q) = %d %v, want %d", c.screen, got, ok, c.want)
		}
	}
	// No cursor mark: never guess which line is chosen.
	if _, ok := trustMoves("   No, exit\n   Yes, I trust this folder\n"); ok {
		t.Error("trustMoves guessed a cursor on a screen with no mark")
	}
}

// A second pane.trust inside trustGrace is refused and sends nothing.
func TestASecondTrustAnswerIsRefused(t *testing.T) {
	s := readyServer(t)
	id := startFake(t, s, "claude", showTrust(t, `stty raw -echo; head -c 4 | od -An -tx1; `+
		`timeout 2 cat | od -An -tx1 | sed 's/^/MORE/'; echo DONE; sleep 10`))
	waitManifest(t, s, id, proto.StateBlocked)
	if got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id}); !got.OK {
		t.Fatalf("first pane.trust: %+v", got.Error)
	}
	got := call(t, s, map[string]any{"id": "t", "cmd": "pane.trust", "pane": id, "trust": false})
	if got.OK || !strings.Contains(got.Error.Message, "has an answer to its trust screen already") {
		t.Fatalf("second pane.trust = %+v, want a refusal", got)
	}
	text := waitScreen(t, s, id, "DONE")
	if !strings.Contains(text, "1b 5b 42 0d") || strings.Contains(text, "MORE") {
		t.Fatalf("the second answer reached the pane:\n%s", text)
	}
}
