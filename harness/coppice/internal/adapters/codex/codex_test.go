package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func td(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "adapters", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// startFixtureFile wires COPPICE_CODEX_BIN at the fake script against a
// committed testdata fixture and starts a real adapter.Start. It also wires
// the fake's argv echo file, so a test can inspect what actually reached the
// child process.
func startFixtureFile(t *testing.T, fixture string) (pane.Proc, string) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	fake := td(t, "fake-codex.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", td(t, fixture))
	t.Setenv("COPPICE_CODEX_ECHO_ARGV", argvLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, argvLog
}

// start is startFixtureFile against the shared session_id/assistant_message
// fixture every test that does not care which vocabulary it exercises uses.
func start(t *testing.T) (pane.Proc, string) {
	t.Helper()
	return startFixtureFile(t, "codex-stream.jsonl")
}

// startWithLines is startFixtureFile against a fixture built from raw JSON
// lines given directly, rather than a shared testdata file - for the small,
// single-shape fixtures a parse-behaviour test needs on its own.
func startWithLines(t *testing.T, lines ...string) (pane.Proc, string) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	f := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := td(t, "fake-codex.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", f)
	t.Setenv("COPPICE_CODEX_ECHO_ARGV", argvLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, argvLog
}

func drain(t *testing.T, p pane.Proc, want int, d time.Duration) []pane.Event {
	t.Helper()
	var out []pane.Event
	deadline := time.After(d)
	for len(out) < want {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("collected %d of %d events in %s: %+v", len(out), want, d, out)
		}
	}
	return out
}

func TestPromptRunsCodexExecJSONWithThePromptInArgv(t *testing.T) {
	p, argvLog := start(t)
	if err := p.Prompt("check the tests"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	for _, want := range []string{"exec", "--json", "check the tests"} {
		if !strings.Contains(line, want) {
			t.Fatalf("argv %q is missing %q", line, want)
		}
	}
	// A -- end-of-flags separator sits right before the
	// prompt text, so a prompt starting with "-" is never misread as a flag.
	if !strings.Contains(line, "--json -- check the tests") {
		t.Fatalf("argv %q does not separate the prompt with --", line)
	}
}

func TestASecondPromptResumesTheRecordedSession(t *testing.T) {
	p, argvLog := start(t)
	if err := p.Prompt("first"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if id, ok := p.SessionID(); !ok || id != "cx-8f21" {
		t.Fatalf("SessionID() = %q %v, want cx-8f21", id, ok)
	}
	if err := p.Prompt("second"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	b, _ := os.ReadFile(argvLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("codex ran %d times, want 2 (one process per turn)", len(lines))
	}
	// resume argv is `codex exec resume <id> --json <prompt>`
	// per published docs, not the invented `--resume <id>` flag shape.
	if !strings.HasPrefix(lines[1], "exec resume cx-8f21 --json") {
		t.Fatalf("the second run %q does not resume per `codex exec resume <id> --json <prompt>`", lines[1])
	}
	if !strings.Contains(lines[1], "-- second") {
		t.Fatalf("the second run %q does not separate the prompt with --", lines[1])
	}
}

func TestTurnCompletedBecomesIdleAndTheChannelStaysOpen(t *testing.T) {
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 4, 5*time.Second)
	last := evs[len(evs)-1]
	if last.Kind != pane.EvState || last.State != "idle" {
		t.Fatalf("last event = %+v, want an idle state event", last)
	}
	// One process per turn means the process exiting is not the session ending.
	// A second prompt must still work.
	if err := p.Prompt("again"); err != nil {
		t.Fatalf("the channel closed after one turn: %v", err)
	}
}

func TestStopClosesTheChannel(t *testing.T) {
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-p.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the channel stayed open 5 s after Stop")
		}
	}
}

// pane.close reaches Stop while a turn is still streaming. Closing the events
// channel from Stop would let the turn goroutine send on a closed channel,
// which panics, and a panic in an adapter goroutine takes the whole daemon
// down and orphans every other pane's process. Run this with -race.
//
// The events channel defaults to a 256-slot buffer, which this test's
// 4-event fixture never fills, so the send-side "case <-p.stop" branch in
// relay would otherwise never actually run under this test - only the easy
// "stop noticed between sends" path would. Shrinking eventsBufferSize to 1
// for the duration forces at least some of these 50 iterations to block on
// a full buffer and unblock via that branch instead, same trick claude's own
// test uses.
func TestStopWhileTheStreamIsFlowingDoesNotPanic(t *testing.T) {
	old := eventsBufferSize
	eventsBufferSize = 1
	t.Cleanup(func() { eventsBufferSize = old })
	for i := 0; i < 50; i++ {
		p, _ := start(t)
		if err := p.Prompt("go"); err != nil {
			t.Fatal(err)
		}
		// Stop immediately, with no wait: the turn goroutine is mid-stream.
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("a second Stop failed: %v", err)
		}
		deadline := time.After(5 * time.Second)
		for done := false; !done; {
			select {
			case _, ok := <-p.Events():
				if !ok {
					done = true
				}
			case <-deadline:
				t.Fatal("the event channel never closed after Stop")
			}
		}
	}
}

func TestAnUnparsableLineYieldsAnErrorEvent(t *testing.T) {
	toolchain.RequireOrSkip(t)
	broken := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(broken, []byte("}{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := td(t, "fake-codex.sh")
	_ = os.Chmod(fake, 0o755)
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", broken)

	g, _ := pane.NewGrid(80, 24)
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvError {
		t.Fatalf("first event = %+v, want an error event", evs[0])
	}
}

// --- Both published wire vocabularies ---------------------------------------

// One fixture uses session_id/session.created/assistant_message.
// Published codex exec --json docs also show thread_id/thread.started/
// agent_message. Both must map to the same shape of events - see the
// package doc for why this adapter refuses to bet on just one.
func TestSessionAndThreadVocabulariesMapToTheSameEvents(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		wantID  string
	}{
		{"session_id vocabulary (session.created/assistant_message)", "codex-stream.jsonl", "cx-8f21"},
		{"thread_id vocabulary (thread.started/agent_message)", "codex-stream-thread.jsonl", "th-9c14"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := startFixtureFile(t, c.fixture)
			if err := p.Prompt("go"); err != nil {
				t.Fatal(err)
			}
			evs := drain(t, p, 4, 5*time.Second)
			if evs[0].Kind != pane.EvState || evs[0].State != "working" {
				t.Fatalf("first event = %+v, want a working session-start row", evs[0])
			}
			if evs[1].Kind != pane.EvText || evs[1].Text == "" {
				t.Fatalf("second event = %+v, want assistant/agent message text", evs[1])
			}
			if evs[2].Kind != pane.EvTool {
				t.Fatalf("third event = %+v, want a shell tool event", evs[2])
			}
			if evs[3].Kind != pane.EvState || evs[3].State != "idle" {
				t.Fatalf("last event = %+v, want idle", evs[3])
			}
			if id, ok := p.SessionID(); !ok || id != c.wantID {
				t.Fatalf("SessionID() = %q %v, want %q", id, ok, c.wantID)
			}
		})
	}
}

// item.updated is codex's per-delta shape; item.started fires before there
// is any content. Neither earns a grid row - adapter.go rule (d) wants one
// event per completed message, not one per token.
func TestItemStartedAndItemUpdatedProduceNoRow(t *testing.T) {
	p, _ := startWithLines(t,
		`{"type":"item.started","item":{"type":"agent_message"}}`,
		`{"type":"item.updated","item":{"type":"agent_message","text":"partial"}}`,
		`{"type":"turn.completed"}`,
	)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvState || evs[0].State != "idle" {
		t.Fatalf("first delivered event = %+v, want turn.completed's idle row with nothing ahead of it", evs[0])
	}
}

func TestItemCompletedErrorYieldsAnErrorEvent(t *testing.T) {
	p, _ := startWithLines(t, `{"type":"item.completed","item":{"type":"error","text":"boom"}}`)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvError {
		t.Fatalf("item-level error = %+v, want an error event", evs[0])
	}
}

// An item.completed whose type this adapter does not otherwise recognise,
// but which still carries text, must surface that text rather than losing
// it to a bare "unmodelled" row.
func TestItemCompletedWithUnknownTypeButTextYieldsEvText(t *testing.T) {
	p, _ := startWithLines(t,
		`{"type":"item.completed","item":{"type":"reasoning_summary","text":"thinking about it"}}`,
		`{"type":"turn.completed"}`,
	)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 2, 5*time.Second)
	if evs[0].Kind != pane.EvText || evs[0].Text != "thinking about it" {
		t.Fatalf("text-bearing unmodelled item = %+v, want EvText carrying its text", evs[0])
	}
}

// An item.completed whose type is unrecognised AND textless still leaves a
// visible working row naming the type - never idle, never silence.
func TestItemCompletedWithUnknownTypeAndNoTextYieldsWorkingRow(t *testing.T) {
	p, _ := startWithLines(t,
		`{"type":"item.completed","item":{"type":"mystery_item"}}`,
		`{"type":"turn.completed"}`,
	)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 2, 5*time.Second)
	if evs[0].Kind != pane.EvState || evs[0].State != "working" {
		t.Fatalf("unmodelled textless item = %+v, want a working row", evs[0])
	}
	if !strings.Contains(evs[0].Detail, "mystery_item") {
		t.Fatalf("Detail = %q, want it to name mystery_item", evs[0].Detail)
	}
}

// A top-level type shape neither vocabulary names must still fail loudly,
// visibly, as working - a wire mismatch shows up in the transcript instead
// of hiding, exactly the proviso the package doc states plainly.
func TestUnknownTopLevelTypeYieldsAWorkingRow(t *testing.T) {
	p, _ := startWithLines(t,
		`{"type":"some.new.event","stuff":true}`,
		`{"type":"turn.completed"}`,
	)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 2, 5*time.Second)
	if evs[0].Kind != pane.EvState || evs[0].State != "working" {
		t.Fatalf("unknown top-level type = %+v, want a working row", evs[0])
	}
	if !strings.Contains(evs[0].Detail, "some.new.event") {
		t.Fatalf("Detail = %q, want it to name the unknown type", evs[0].Detail)
	}
}

// --- A failed turn must not wedge a waiter ----------------------------------

func TestTurnFailedBecomesIdleWithReason(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"nested error.message", `{"type":"turn.failed","error":{"message":"model overloaded"}}`, "model overloaded"},
		{"bare top-level message", `{"type":"turn.failed","message":"boom"}`, "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := startWithLines(t, c.line)
			if err := p.Prompt("go"); err != nil {
				t.Fatal(err)
			}
			evs := drain(t, p, 1, 5*time.Second)
			if evs[0].Kind != pane.EvState || evs[0].State != "idle" {
				t.Fatalf("turn.failed = %+v, want idle, not a wedge", evs[0])
			}
			if !strings.Contains(evs[0].Detail, "turn failed") || !strings.Contains(evs[0].Detail, c.want) {
				t.Fatalf("Detail = %q, want it to say turn failed and name %q", evs[0].Detail, c.want)
			}
		})
	}
}

// A turn whose process exits without ever sending turn.completed or
// turn.failed - a crash, or a turn that simply stopped talking - must not
// leave the pane looking like it is still working forever.
func TestTurnEndingWithoutCompletionNamesTheExitCode(t *testing.T) {
	p, _ := startWithLines(t,
		`{"type":"session.created","session_id":"cx-1"}`,
		`{"type":"item.completed","item":{"type":"assistant_message","text":"working on it"}}`,
	)
	t.Setenv("COPPICE_CODEX_EXIT", "17")
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 3, 5*time.Second)
	last := evs[len(evs)-1]
	if last.Kind != pane.EvState || last.State != "idle" {
		t.Fatalf("last event = %+v, want the wedge-prevention idle row", last)
	}
	if !strings.Contains(last.Detail, "turn ended without completion exit=17") {
		t.Fatalf("Detail = %q, want it to name exit=17", last.Detail)
	}
}

// A Stop-requested kill must not ALSO produce the incomplete-turn
// diagnostic: the pane is already ending, and a redundant row on top of that
// would be noise, not signal.
func TestARequestedStopSuppressesTheIncompleteTurnDiagnostic(t *testing.T) {
	p, _ := startWithLines(t, `{"type":"session.created","session_id":"cx-1"}`)
	t.Setenv("COPPICE_CODEX_SLEEP", "0.3")
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				return
			}
			if ev.Kind == pane.EvState && strings.Contains(ev.Detail, "turn ended without completion") {
				t.Fatalf("a Stop-requested exit still produced the incomplete-turn diagnostic: %+v", ev)
			}
		case <-deadline:
			t.Fatal("the channel never closed after Stop")
		}
	}
}

// --- One turn at a time ------------------------------------------------------

func TestPromptWhileATurnIsRunningIsRefused(t *testing.T) {
	p, argvLog := start(t)
	t.Setenv("COPPICE_CODEX_SLEEP", "0.3")
	if err := p.Prompt("first"); err != nil {
		t.Fatal(err)
	}
	if err := p.Prompt("second"); err == nil {
		t.Fatal("a second Prompt while a turn is running should be refused")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error = %q, want it to name an already-running turn", err.Error())
	}
	drain(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("codex ran %d times, want exactly 1 - the refused Prompt must not have spawned anything", len(lines))
	}
	if err := p.Prompt("third"); err != nil {
		t.Fatalf("a prompt after the first turn completed should succeed: %v", err)
	}
}

// A pile of concurrent Prompt attempts racing a concurrent Stop must never
// panic (run this with -race) and must never leave a turn claimed once the
// pane has fully stopped - beginTurn's own mutex-ordered handshake with Stop
// is what this proves. running/turnActive are unexported fields this
// in-package test reads directly rather than shelling out to find a process.
func TestBeginTurnRacesStopWithoutLeavingATurnClaimed(t *testing.T) {
	for i := 0; i < 20; i++ {
		pp, _ := start(t)
		pr := pp.(*proc)

		var wg sync.WaitGroup
		for j := 0; j < 5; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = pp.Prompt("go")
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pp.Stop()
		}()
		wg.Wait()
		if err := pp.Stop(); err != nil {
			t.Fatalf("a follow-up Stop failed: %v", err)
		}

		deadline := time.After(5 * time.Second)
		for done := false; !done; {
			select {
			case _, ok := <-pp.Events():
				if !ok {
					done = true
				}
			case <-deadline:
				t.Fatal("the event channel never closed after Stop")
			}
		}

		pr.mu.Lock()
		running, active := pr.running, pr.turnActive
		pr.mu.Unlock()
		if running != nil || active {
			t.Fatalf("Stop left a turn claimed: running=%v active=%v", running, active)
		}
	}
}

// Stop must not leak the relay or runTurn goroutines. runtime.NumGoroutine is
// noisy (GC, finalizers, the test runner itself), so this polls toward a
// baseline with slack rather than asserting an exact count.
func TestStopLeavesNoGoroutineRunning(t *testing.T) {
	before := runtime.NumGoroutine()
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not settle after Stop: now %d, before %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSteerIsHonestlyUnsupported(t *testing.T) {
	p, _ := start(t)
	err := p.Steer("stop that")
	if !errors.Is(err, pane.ErrUnsupported) {
		t.Fatalf("Steer() = %v, want errors.Is(err, pane.ErrUnsupported)", err)
	}
}

func TestWriteStdinBeforeStopIsUnsupported(t *testing.T) {
	p, _ := start(t)
	err := p.WriteStdin([]byte("x"))
	if !errors.Is(err, pane.ErrUnsupported) {
		t.Fatalf("WriteStdin() = %v, want errors.Is(err, pane.ErrUnsupported)", err)
	}
}

func TestPromptAfterStopIsRefused(t *testing.T) {
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	err := p.Prompt("again")
	if err == nil {
		t.Fatal("Prompt after Stop reported success")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Prompt after Stop = %q, want it to say the pane stopped", err.Error())
	}
}

func TestWriteStdinAfterStopIsRefused(t *testing.T) {
	p, _ := start(t)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	err := p.WriteStdin([]byte("x"))
	if err == nil {
		t.Fatal("WriteStdin after Stop reported success")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("WriteStdin after Stop = %q, want it to say the pane stopped", err.Error())
	}
}

// stoppedErr guards on both p.stop and p.relayDone independently - this
// constructs each case directly rather than through the full pane lifecycle,
// since codex's relay in practice only ever reaches relayDone as a
// consequence of p.stop closing first (see relay's own doc comment), so no
// black-box test through the public API could tell the two guards apart.
func TestStoppedErrGuardsOnBothChannels(t *testing.T) {
	fresh := &proc{stop: make(chan struct{}), relayDone: make(chan struct{})}
	if err := fresh.stoppedErr(); err != nil {
		t.Fatalf("a fresh proc should not report stopped: %v", err)
	}

	stoppedByStop := &proc{stop: make(chan struct{}), relayDone: make(chan struct{})}
	close(stoppedByStop.stop)
	if err := stoppedByStop.stoppedErr(); err == nil {
		t.Fatal("stoppedErr() should report stopped once p.stop is closed")
	}

	stoppedByRelay := &proc{stop: make(chan struct{}), relayDone: make(chan struct{})}
	close(stoppedByRelay.relayDone)
	if err := stoppedByRelay.stoppedErr(); err == nil {
		t.Fatal("stoppedErr() should report stopped once p.relayDone is closed, even if p.stop is not")
	}
}

// resumeArgv used to drop StartOpts.Argv entirely - only the
// fresh-run branch spliced it in. Start seeds p.sessionID from o.Resume, so
// a pane restored with a recorded session id takes the RESUME path on its
// very first prompt and silently never received its cmd_argv (--model, a
// sandbox flag, ...). Both paths must carry it, same as claude does on both
// of its paths.
func TestResumedPaneKeepsItsExtraArgvOnTheFirstPrompt(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fake := td(t, "fake-codex.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", td(t, "codex-stream.jsonl"))
	t.Setenv("COPPICE_CODEX_ECHO_ARGV", argvLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd: t.TempDir(), Argv: []string{"--model", "x"}, Resume: "s1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	for _, want := range []string{"resume s1", "--model x"} {
		if !strings.Contains(line, want) {
			t.Fatalf("a resumed pane's first-prompt argv %q is missing %q", line, want)
		}
	}
}

// A fresh (non-resumed) pane's own StartOpts.Argv is the control: it always
// worked, and must keep working.
func TestAFreshPaneAlsoCarriesItsExtraArgv(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fake := td(t, "fake-codex.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CODEX_BIN", fake)
	t.Setenv("COPPICE_CODEX_FIXTURE", td(t, "codex-stream.jsonl"))
	t.Setenv("COPPICE_CODEX_ECHO_ARGV", argvLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd: t.TempDir(), Argv: []string{"--model", "x"},
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.TrimSpace(string(b)), "--model x") {
		t.Fatalf("a fresh pane's first-prompt argv %q is missing --model x", strings.TrimSpace(string(b)))
	}
}

// A grandchild that inherits this turn's stdout pipe (a backgrounded
// job the harness fires and forgets) must not wedge Stop forever. The fake
// script's COPPICE_CODEX_ORPHAN knob backgrounds a 40s sleep right before it
// exits, exactly the shape that would hang a naive read-to-EOF-then-Wait
// loop for the sleep's whole lifetime.
func TestStopIsNotWedgedByAGrandchildHoldingTheStdoutPipe(t *testing.T) {
	p, _ := start(t)
	t.Setenv("COPPICE_CODEX_ORPHAN", "1")
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	// Drain the whole turn first. turn.completed frees the slot (endTurn)
	// well before the script's own process - now backgrounding
	// the orphan sleep - actually exits, so by the time Stop runs below,
	// p.running is already nil: Stop's OWN immediate group-kill has nothing
	// left to find, and this test is exercising runTurn's WaitDelay-bounded
	// cleanup instead, not Stop's fast path for a turn still mid-stream.
	drain(t, p, 4, 5*time.Second)

	began := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("Stop took %v to return with a grandchild holding the stdout pipe, want well under 3s", elapsed)
	}

	// The chosen design (see Stop's and runTurn's own comments) kills the
	// whole process group, not just the turn's own pid, so the backgrounded
	// sleep does not linger for its own remaining lifetime after Stop
	// returns.
	out, _ := exec.Command("pgrep", "-f", "sleep 40.4321").Output()
	if pids := strings.TrimSpace(string(out)); pids != "" {
		t.Fatalf("a grandchild (sleep 40.4321) is still alive after Stop returned: pids %s", pids)
	}
}

// Calling cmd.Wait concurrently with the scan
// loop, to unstick a grandchild-held pipe, closes the pipe (for a pipe
// this code manages itself) the moment it sees the DIRECT child
// exit - grandchild or not - so on an entirely ordinary fast exit, with no
// grandchild involved at all, Wait's close can race this turn's own
// still-in-progress reads and truncate the tail. This pins that every line
// from a fast-exiting turn arrives, looped so a race that only shows up
// some fraction of the time (about 1 run in 20-40 in isolation)
// gets many chances to reproduce.
func TestAFastExitLosesNoTailText(t *testing.T) {
	for i := 0; i < 20; i++ {
		p, _ := start(t)
		if err := p.Prompt("go"); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		evs := drain(t, p, 4, 5*time.Second)
		if len(evs) != 4 {
			t.Fatalf("iteration %d: got %d events, want all 4 fixture lines: %+v", i, len(evs), evs)
		}
		last := evs[3]
		if last.Kind != pane.EvState || last.State != "idle" {
			t.Fatalf("iteration %d: last event = %+v, want the turn.completed idle row - a lost tail line reads as an error instead", i, last)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

// Fd hygiene: once a turn completes normally (no Stop), its own end of the
// stdout pipe (Prompt's os.Pipe, not cmd.StdoutPipe) must actually close,
// not just be abandoned. A pane runs many turns over its life; a leaked fd
// per turn accumulates for as long as the pane stays open.
func TestReadEndClosesAfterATurnCompletesNormally(t *testing.T) {
	p, _ := start(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)

	cp := p.(*proc)
	cp.mu.Lock()
	r := cp.readEnd
	cp.mu.Unlock()
	if r == nil {
		t.Fatal("no readEnd recorded for the completed turn")
	}
	buf := make([]byte, 1)
	if _, err := r.Read(buf); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("readEnd.Read after a normal turn completion = %v, want os.ErrClosed", err)
	}
}

// A top-level {"type":"error",...} line is not item.completed's own error
// shape (TestItemCompletedErrorYieldsAnErrorEvent, above): it is a stream-
// level fault, and it can carry a reason the same two ways turn.failed's own
// wireLine.reason() already reads - nested under error.message, or as a bare
// top-level message. Detail must carry that reason, not just the literal
// word "error" with nothing anyone could act on.
func TestTopLevelErrorLineCarriesItsReason(t *testing.T) {
	p, _ := startWithLines(t, `{"type":"error","error":{"message":"rate limited"}}`)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvError {
		t.Fatalf("top-level error = %+v, want an error event", evs[0])
	}
	if !strings.Contains(evs[0].Detail, "rate limited") {
		t.Fatalf("top-level error Detail = %q, want it to carry the reason", evs[0].Detail)
	}
}

// Each turn's root pid is kept, so the server can place a process a turn
// started as belonging to this pane.
func TestPidsNamesEachTurn(t *testing.T) {
	p, _ := start(t)
	if len(p.(pane.Pider).Pids()) != 0 {
		t.Fatal("pids before any turn")
	}
	if err := p.Prompt("check the tests"); err != nil {
		t.Fatal(err)
	}
	drain(t, p, 4, 5*time.Second)
	if pids := p.(pane.Pider).Pids(); len(pids) != 1 || pids[0] <= 0 {
		t.Fatalf("pids after one turn = %v", pids)
	}
}
