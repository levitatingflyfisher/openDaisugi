package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
)

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stubPi writes a fake `pi --mode rpc`: it streams eventsFile to stdout and
// records everything written to its stdin into stdinLog, for assertions.
// `exec 3<&0` plus reading from fd 3 in the backgrounded loop is required: a
// non-interactive shell redirects an async command's stdin from /dev/null,
// so a plain `while read ... &` never sees the real stdin pipe.
func stubPi(t *testing.T, eventsFile, stdinLog string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pi")
	body := "#!/bin/sh\n" +
		"exec 3<&0\n" +
		"while IFS= read -r line <&3; do printf '%s\\n' \"$line\" >> " + shQuote(stdinLog) + "; done &\n" +
		"cat " + shQuote(eventsFile) + "\n" +
		"wait\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// start writes the transcript and the stdin log, starts the adapter against
// the stub, and stops it at cleanup so no stub outlives its test.
func start(t *testing.T, transcript string, opts pane.StartOpts) (pane.Proc, string) {
	t.Helper()
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	if err := os.WriteFile(stdinLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.Cwd == "" {
		opts.Cwd = dir
	}
	a := adapter{Bin: stubPi(t, events, stdinLog)}
	p, err := a.Start(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, stdinLog
}

func jsonLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("stdin line not JSON: %v (%q)", err, line)
		}
		out = append(out, got)
	}
	return out
}

func lastJSONLine(t *testing.T, path string) map[string]any {
	t.Helper()
	lines := jsonLines(t, path)
	if len(lines) == 0 {
		t.Fatalf("no JSON lines in %s", path)
	}
	return lines[len(lines)-1]
}

// waitState reads events until one carries the wanted state, returning it.
func waitState(t *testing.T, p pane.Proc, want string) pane.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatalf("event channel closed before a %s state arrived", want)
			}
			if ev.Kind == pane.EvState && ev.State == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s state", want)
		}
	}
}

func TestAdapterEmitsBlockedOnExtensionUIRequestAndAnswers(t *testing.T) {
	transcript := `{"type":"agent_start"}` + "\n" +
		`{"type":"extension_ui_request","id":"req-1","method":"confirm","title":"Dangerous!","message":"Allow rm -rf?"}` + "\n"
	p, stdinLog := start(t, transcript, pane.StartOpts{})

	waitState(t, p, "working")
	ev := waitState(t, p, "blocked")
	if ev.Ask == nil {
		t.Fatal("a blocked state must carry an ask")
	}
	if ev.Ask.ID != "req-1" || ev.Ask.Tool != "confirm" || ev.Ask.Summary != "Dangerous!" {
		t.Fatalf("ask = %+v, want id req-1, tool confirm, summary Dangerous!", ev.Ask)
	}
	if ev.Ask.Deadline < float64(time.Now().Unix()) {
		t.Fatalf("ask deadline %v is already in the past", ev.Ask.Deadline)
	}

	if err := p.Prompt("y"); err != nil {
		t.Fatalf("Prompt (answer): %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	resp := lastJSONLine(t, stdinLog)
	if resp["type"] != "extension_ui_response" || resp["id"] != "req-1" || resp["confirmed"] != true {
		t.Fatalf("unexpected extension_ui_response: %+v", resp)
	}
}

func TestAnswerToASelectDialogCarriesValueNotConfirmed(t *testing.T) {
	transcript := `{"type":"extension_ui_request","id":"req-2","method":"select","title":"Pick one","options":["Allow","Block"]}` + "\n"
	p, stdinLog := start(t, transcript, pane.StartOpts{})
	waitState(t, p, "blocked")
	if err := p.Prompt("Block"); err != nil {
		t.Fatalf("Prompt (answer): %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	resp := lastJSONLine(t, stdinLog)
	if resp["type"] != "extension_ui_response" || resp["id"] != "req-2" || resp["value"] != "Block" {
		t.Fatalf("unexpected extension_ui_response: %+v", resp)
	}
	if _, has := resp["confirmed"]; has {
		t.Fatalf("a select answer must not carry confirmed: %+v", resp)
	}
}

func TestFireAndForgetUIRequestsDoNotBlock(t *testing.T) {
	transcript := `{"type":"extension_ui_request","id":"n-1","method":"notify","message":"hi"}` + "\n" +
		`{"type":"agent_settled"}` + "\n"
	p, _ := start(t, transcript, pane.StartOpts{})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("channel closed before agent_settled arrived")
			}
			if ev.Kind == pane.EvState && ev.State == "blocked" {
				t.Fatal("a notify request must not read as blocked")
			}
			if ev.Kind == pane.EvState && ev.State == "idle" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for idle")
		}
	}
}

func TestPromptSendsExactJSON(t *testing.T) {
	p, stdinLog := start(t, "", pane.StartOpts{})
	if err := p.Prompt("run ls"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "prompt" || got["message"] != "run ls" {
		t.Fatalf("unexpected prompt JSON: %+v", got)
	}
	if _, has := got["streamingBehavior"]; has {
		t.Fatalf("an idle prompt must not carry streamingBehavior: %+v", got)
	}
}

func TestSteerSendsTheSteerCommand(t *testing.T) {
	p, stdinLog := start(t, "", pane.StartOpts{})
	if err := p.Steer("stop that"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "steer" || got["message"] != "stop that" {
		t.Fatalf("unexpected steer JSON: %+v", got)
	}
}

func TestSessionIDRoundTripsThroughResume(t *testing.T) {
	transcript := `{"type":"response","command":"get_state","success":true,"data":{"sessionId":"abc123","sessionFile":"/home/x/.pi/agent/sessions/foo/1_abc.jsonl"}}` + "\n"
	p, stdinLog := start(t, transcript, pane.StartOpts{})
	// get_state's response updates metadata directly and emits no
	// pane.Event, so poll rather than wait on the events channel.
	deadline := time.Now().Add(2 * time.Second)
	var resumeValue string
	for time.Now().Before(deadline) {
		if v, ok := p.SessionID(); ok {
			resumeValue = v
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resumeValue == "" {
		t.Fatal("timed out waiting for get_state response to be processed")
	}
	if resumeValue != "/home/x/.pi/agent/sessions/foo/1_abc.jsonl" {
		t.Fatalf("SessionID() = %q, want the sessionFile path, not the bare sessionId", resumeValue)
	}
	first := jsonLines(t, stdinLog)
	if len(first) == 0 || first[0]["type"] != "get_state" {
		t.Fatalf("a fresh start must ask get_state first, got %+v", first)
	}

	p2, stdinLog2 := start(t, "", pane.StartOpts{Resume: resumeValue})
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog2)
	if got["type"] != "switch_session" || got["sessionPath"] != resumeValue {
		t.Fatalf("unexpected switch_session JSON: %+v", got)
	}
	if v, ok := p2.SessionID(); !ok || v != resumeValue {
		t.Fatalf("a resumed pane must report its resume path at once, got %q %v", v, ok)
	}
}

func TestPromptDuringStreamingCarriesSteerBehavior(t *testing.T) {
	p, stdinLog := start(t, `{"type":"agent_start"}`+"\n", pane.StartOpts{})
	waitState(t, p, "working")
	if err := p.Prompt("keep going"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "prompt" || got["message"] != "keep going" || got["streamingBehavior"] != "steer" {
		t.Fatalf("unexpected prompt JSON while streaming: %+v", got)
	}
}

func TestTextDeltasCoalesceIntoOneEventPerMessage(t *testing.T) {
	transcript := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello "}}` + "\n" +
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"world"}}` + "\n" +
		`{"type":"message_end","message":{}}` + "\n" +
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"ls"}}` + "\n" +
		`{"type":"agent_settled"}` + "\n"
	p, _ := start(t, transcript, pane.StartOpts{})
	var texts []string
	var tools []pane.Event
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("channel closed before agent_settled")
			}
			switch ev.Kind {
			case pane.EvText:
				texts = append(texts, ev.Text)
			case pane.EvTool:
				tools = append(tools, ev)
			case pane.EvState:
				if ev.State == "idle" {
					break loop
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for idle")
		}
	}
	if len(texts) != 1 || texts[0] != "Hello world" {
		t.Fatalf("texts = %q, want one coalesced message", texts)
	}
	if len(tools) != 1 || tools[0].Tool != "bash" || !strings.Contains(tools[0].Detail, "ls") {
		t.Fatalf("tools = %+v, want one bash tool event naming ls", tools)
	}
}

func TestExtensionErrorReadsAsUnknownNotIdle(t *testing.T) {
	transcript := `{"type":"extension_error","extensionPath":"x.ts","event":"tool_call","error":"boom"}` + "\n"
	p, _ := start(t, transcript, pane.StartOpts{})
	deadline := time.After(2 * time.Second)
	select {
	case ev, ok := <-p.Events():
		if !ok {
			t.Fatal("channel closed")
		}
		st, _ := pane.StateOf(ev)
		if st != "unknown" || !strings.Contains(ev.Detail, "boom") {
			t.Fatalf("event = %+v (state %s), want unknown naming boom", ev, st)
		}
	case <-deadline:
		t.Fatal("timed out")
	}
}

func TestNewIsRegisteredUnderTheNamePi(t *testing.T) {
	if New().Name() != "pi" {
		t.Fatalf("Name() = %q, want %q", New().Name(), "pi")
	}
}

func TestStopKillsTheProcessAndClosesEvents(t *testing.T) {
	p, _ := start(t, "", pane.StartOpts{})
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-p.Events():
			if !ok {
				if err := p.Prompt("late"); err == nil {
					t.Fatal("Prompt after Stop must fail")
				}
				return
			}
		case <-deadline:
			t.Fatal("Events() did not close after Stop")
		}
	}
}

func TestNaturalExitEmitsEndThenCloses(t *testing.T) {
	dir := t.TempDir()
	stdinLog := filepath.Join(dir, "stdin.log")
	bin := filepath.Join(dir, "pi")
	// A stub that emits one event and exits on its own, no stdin loop.
	body := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"agent_start\"}'\nexit 3\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = stdinLog
	a := adapter{Bin: bin}
	p, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	deadline := time.After(2 * time.Second)
	sawEnd := false
	for !sawEnd {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("channel closed before EvEnd")
			}
			if ev.Kind == pane.EvEnd {
				sawEnd = true
				if ev.State != "done" || !strings.Contains(ev.Detail, "exit=3") {
					t.Fatalf("end event = %+v, want done naming exit=3", ev)
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for EvEnd")
		}
	}
	select {
	case _, ok := <-p.Events():
		if ok {
			t.Fatal("an event after EvEnd")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events() did not close after EvEnd")
	}
}

func TestLivePiSpawnsAndReachesTheGateLog(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi not on PATH; this test needs a real pi install")
	}
	t.Skip("needs a running resident gate plus a gate-log reader to assert against, " +
		"neither of which exists in this Go-only package. A named gap, not a silent one.")
}

func TestAskDeadlineComesFromTheDialogTimeout(t *testing.T) {
	transcript := `{"type":"extension_ui_request","id":"req-t","method":"confirm","title":"Sure?","timeout":30000}` + "\n"
	p, _ := start(t, transcript, pane.StartOpts{})
	ev := waitState(t, p, "blocked")
	now := float64(time.Now().Unix())
	if ev.Ask == nil || ev.Ask.Deadline < now+25 || ev.Ask.Deadline > now+35 {
		t.Fatalf("ask = %+v, want a deadline about 30 s from now (%v)", ev.Ask, now)
	}
}

func TestPromptAfterADialogTimeoutIsAPromptNotAStaleAnswer(t *testing.T) {
	transcript := `{"type":"extension_ui_request","id":"req-x","method":"confirm","title":"Sure?","timeout":100}` + "\n"
	p, stdinLog := start(t, transcript, pane.StartOpts{})
	waitState(t, p, "blocked")
	time.Sleep(250 * time.Millisecond)
	if err := p.Prompt("do x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "prompt" || got["message"] != "do x" {
		t.Fatalf("after pi's own timeout the prompt must reach pi as a prompt, got %+v", got)
	}
}

func TestAgentSettledClearsAPendingDialog(t *testing.T) {
	transcript := `{"type":"extension_ui_request","id":"req-s","method":"input","title":"Name?"}` + "\n" +
		`{"type":"agent_settled"}` + "\n"
	p, stdinLog := start(t, transcript, pane.StartOpts{})
	waitState(t, p, "blocked")
	waitState(t, p, "idle")
	if err := p.Prompt("next"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "prompt" || got["message"] != "next" {
		t.Fatalf("a settled run has no open dialog; got %+v", got)
	}
}
