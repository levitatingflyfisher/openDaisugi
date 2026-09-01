package claude

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func testdata(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "adapters", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// startFake wires COPPICE_CLAUDE_BIN at the fake script and starts a real
// adapter.Start against it. It also wires the fake's stdin/argv echo files, so
// any test can inspect what actually reached the child process.
func startFake(t *testing.T) (pane.Proc, *pane.Grid, string) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	fake := testdata(t, "fake-claude.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(t.TempDir(), "stdin.jsonl")
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", testdata(t, "claude-stream.jsonl"))
	t.Setenv("COPPICE_CLAUDE_ECHO_STDIN", stdinLog)
	t.Setenv("COPPICE_CLAUDE_ECHO_ARGV", argvLog)

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
	return p, g, stdinLog
}

// startWithFixture starts the fake claude against a fixture built from raw
// stream-json lines given directly, rather than the shared claude-stream.jsonl
// file the other tests depend on for their own event counts. It is for the
// small, single-shape fixtures a parse-behaviour test needs on its own.
func startWithFixture(t *testing.T, lines ...string) pane.Proc {
	t.Helper()
	toolchain.RequireOrSkip(t)
	f := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := testdata(t, "fake-claude.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", f)

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
	return p
}

func collect(t *testing.T, p pane.Proc, want int, d time.Duration) []pane.Event {
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
			t.Fatalf("collected %d events in %s, want %d: %+v", len(out), d, want, out)
		}
	}
	return out
}

func TestPromptReachesStdinAsOneJSONUserMessage(t *testing.T) {
	p, _, stdinLog := startFake(t)
	if err := p.Prompt("list the files"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	b, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("prompt wrote %d lines, want exactly 1: %q", strings.Count(line, "\n")+1, line)
	}
	for _, want := range []string{`"type":"user"`, `"role":"user"`, "list the files"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stdin line %q is missing %q", line, want)
		}
	}
}

// The argv here is confirmed necessary: without
// --verbose, `claude -p --output-format stream-json` refuses to run at all.
// This pins that flag so a later cleanup pass cannot drop it unnoticed.
func TestArgvCarriesVerboseAndBothStreamFormats(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	argvLog := os.Getenv("COPPICE_CLAUDE_ECHO_ARGV")
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, want := range []string{"-p", "--output-format stream-json", "--input-format stream-json", "--verbose"} {
		if !strings.Contains(line, want) {
			t.Fatalf("argv %q is missing %q", line, want)
		}
	}
}

func TestStreamJSONBecomesTextToolAndResultEvents(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 4, 5*time.Second)
	kinds := map[pane.EventKind]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	if kinds[pane.EvText] == 0 {
		t.Fatalf("no text event: %+v", evs)
	}
	if kinds[pane.EvTool] == 0 {
		t.Fatalf("no tool event: %+v", evs)
	}
	sawIdle := false
	for _, e := range evs {
		if e.Kind == pane.EvState && e.State == "idle" {
			sawIdle = true
		}
	}
	if !sawIdle {
		t.Fatalf("the result line did not produce an idle state event: %+v", evs)
	}
}

func TestSessionIDComesFromTheInitLine(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	id, ok := p.SessionID()
	if !ok || id != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("SessionID() = %q %v, want the id from the init line", id, ok)
	}
}

func TestProcessExitClosesTheEventChannel(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	var last pane.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				// A Stop the caller asked for is not a surprise exit: the
				// exit code is only named when Stop did NOT request it.
				if last.Kind == pane.EvEnd && strings.Contains(last.Detail, "exit=") {
					t.Fatalf("a Stop-requested exit named a code: %+v", last)
				}
				return
			}
			last = ev
		case <-deadline:
			t.Fatal("the event channel stayed open 5 s after Stop")
		}
	}
}

// After the child exits entirely on its own (nobody called Stop), p.stop
// stays open - only p.relayDone closes. A write that only guarded p.stop
// would reach a stdin pipe with no reader on the other end and surface a raw
// broken-pipe error instead of the same honest "this pane has stopped" Prompt
// and WriteStdin give after an explicit Stop.
func TestWritesAfterTheChildExitsOnItsOwnAreAnHonestError(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fake := testdata(t, "fake-claude.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", testdata(t, "claude-stream.jsonl"))
	t.Setenv("COPPICE_CLAUDE_EXIT", "0")

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	// Drain to close: the child exits on its own after replaying once, and
	// nobody here calls Stop.
	deadline := time.After(5 * time.Second)
	for closed := false; !closed; {
		select {
		case _, ok := <-p.Events():
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("Events() never closed after the child's own exit")
		}
	}

	if err := p.Prompt("are you still there"); err == nil {
		t.Fatal("Prompt after the child exited on its own reported success")
	} else if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Prompt after natural exit = %q, want it to say the pane stopped", err.Error())
	}
	if err := p.WriteStdin([]byte("{}\n")); err == nil {
		t.Fatal("WriteStdin after the child exited on its own reported success")
	} else if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("WriteStdin after natural exit = %q, want it to say the pane stopped", err.Error())
	}
}

// When claude exits on its own, and nobody asked it to, the reason belongs in
// the terminal event: a plain "claude exited" cannot tell a crash from a
// clean stop. This mirrors the pty side's own exit=%d (watchExit in
// internal/server/panes.go).
func TestEvEndNamesTheExitCodeWhenTheChildWasNotStopped(t *testing.T) {
	toolchain.RequireOrSkip(t)
	fake := testdata(t, "fake-claude.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", testdata(t, "claude-stream.jsonl"))
	t.Setenv("COPPICE_CLAUDE_EXIT", "3")

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	var end *pane.Event
	deadline := time.After(5 * time.Second)
	for end == nil {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("Events() closed before an EvEnd event arrived")
			}
			if ev.Kind == pane.EvEnd {
				e := ev
				end = &e
			}
		case <-deadline:
			t.Fatal("no EvEnd event within the deadline")
		}
	}
	if !strings.Contains(end.Detail, "exit=3") {
		t.Fatalf("EvEnd.Detail = %q, want it to name exit=3", end.Detail)
	}
}

// A block type this adapter does not model (thinking, say) must still leave a
// trace: a message whose only content is one must not vanish into zero
// events just because it carried nothing text/tool_use recognises.
func TestAssistantMessageWithOnlyAThinkingBlockLeavesATrace(t *testing.T) {
	p := startWithFixture(t,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"pondering"}]}}`)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvState || evs[0].State != "working" {
		t.Fatalf("thinking-only message = %+v, want a working state event", evs[0])
	}
	if !strings.Contains(evs[0].Detail, "thinking") {
		t.Fatalf("thinking-only message Detail = %q, want it to name the block type", evs[0].Detail)
	}
}

// A tool_use block's own id must travel with the EvTool event it produces, so
// a reader can match this row to the tool-result row that later names the
// same id via tool_use_id.
func TestToolUseEventNamesItsOwnID(t *testing.T) {
	p := startWithFixture(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls"}}]}}`)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvTool {
		t.Fatalf("tool_use message = %+v, want an EvTool event", evs[0])
	}
	if !strings.Contains(evs[0].Detail, "toolu_01") {
		t.Fatalf("EvTool.Detail = %q, want it to name toolu_01", evs[0].Detail)
	}
}

// Claude's wire format sends message.content as either an array of typed
// blocks or, for a plain-text-only message, a bare string. The bare string is
// not a shape this adapter fails to read - it is exactly one text block, and
// must never fail closed to EvError.
func TestAssistantContentAsAPlainStringIsOneTextEvent(t *testing.T) {
	p := startWithFixture(t,
		`{"type":"assistant","message":{"content":"just text, no array"}}`)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvText {
		t.Fatalf("string-content message = %+v, want an EvText event", evs[0])
	}
	if evs[0].Text != "just text, no array" {
		t.Fatalf("EvText.Text = %q, want the bare string content", evs[0].Text)
	}
}

// Stop must be safe to call twice, and safe to call while the relay is
// mid-stream with nobody draining Events(): the relay is the only sender and
// the only closer of that channel, so a racing Stop must never make it send
// on (or close over) a channel that already closed. Run this with -race.
//
// The events channel defaults to a 256-slot buffer, which this test's 4-event
// fixture never fills, so the send-side "case <-p.stop" branch in relay would
// otherwise never actually run under this test - only the easy "stop noticed
// between sends" path would. Shrinking eventsBufferSize to 1 for the duration
// forces at least some of these 50 iterations to block on a full buffer and
// unblock via that branch instead.
func TestStopWhileTheStreamIsFlowingDoesNotPanic(t *testing.T) {
	old := eventsBufferSize
	eventsBufferSize = 1
	t.Cleanup(func() { eventsBufferSize = old })
	for i := 0; i < 50; i++ {
		p, _, _ := startFake(t)
		if err := p.Prompt("go"); err != nil {
			t.Fatal(err)
		}
		// Stop immediately, with no drain: the relay may be mid-send.
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

// Fail-closed: a line the parser cannot read marks the pane unknown. It must
// never be dropped silently, and it must never look like idle.
func TestAnUnparsableLineYieldsAnErrorEventNotSilence(t *testing.T) {
	toolchain.RequireOrSkip(t)
	broken := filepath.Join(t.TempDir(), "broken.jsonl")
	if err := os.WriteFile(broken, []byte("{not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := testdata(t, "fake-claude.sh")
	_ = os.Chmod(fake, 0o755)
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE", broken)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := New().Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, p, 1, 5*time.Second)
	if evs[0].Kind != pane.EvError {
		t.Fatalf("first event = %+v, want an error event", evs[0])
	}
	if st, _ := pane.StateOf(evs[0]); st == "idle" {
		t.Fatal("an unparsable line produced idle")
	}
}

func TestSteerIsHonestlyUnsupported(t *testing.T) {
	p, _, _ := startFake(t)
	err := p.Steer("stop that")
	if !errors.Is(err, pane.ErrUnsupported) {
		t.Fatalf("Steer() = %v, want errors.Is(err, pane.ErrUnsupported)", err)
	}
}

// A write after Stop must say the pane stopped, not surface a raw closed-pipe
// error: Prompt and WriteStdin share the same stdin, so they share the same
// stopped-pane guard.
func TestWriteStdinAfterStopIsAnHonestError(t *testing.T) {
	p, _, _ := startFake(t)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	err := p.WriteStdin([]byte("{}\n"))
	if err == nil {
		t.Fatal("WriteStdin after Stop reported success")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("WriteStdin after Stop = %q, want it to say the pane stopped", err.Error())
	}
}

// Stop must not leak the relay goroutine. runtime.NumGoroutine is noisy (GC,
// finalizers, the test runner itself), so this polls toward a baseline with
// slack rather than asserting an exact count.
func TestStopLeavesNoGoroutineRunning(t *testing.T) {
	before := runtime.NumGoroutine()
	p, _, _ := startFake(t)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	collect(t, p, 4, 5*time.Second)
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

// relay's own defer order decides whether p.relayDone closes before or
// after p.events. stoppedErr checks p.relayDone, so a goroutine reacting to
// Events() closing must already see stoppedErr() as non-nil at that exact
// instant - otherwise a Prompt racing in right then could still reach a
// live stdin pipe instead of the honest "this pane has stopped" every other
// path after a natural exit gives. Many trials, in-process and cheap
// (a real but trivial child, no fixture script): the race this guards is a
// handful of nanoseconds between two adjacent close() calls, and only
// repetition makes it observable at all.
func TestStoppedErrIsAlreadyTrueTheInstantEventsCloses(t *testing.T) {
	toolchain.RequireOrSkip(t)
	const trials = 300
	for i := 0; i < trials; i++ {
		cmd := exec.Command("true")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_, cancel := context.WithCancel(context.Background())
		p := &proc{
			cmd:       cmd,
			cancel:    cancel,
			events:    make(chan pane.Event, 8),
			stop:      make(chan struct{}),
			relayDone: make(chan struct{}),
		}
		pr, pw := io.Pipe()
		_ = pw.Close() // EOF at once: relay's scan loop ends without blocking

		var raced atomic.Bool
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			for range p.events {
			}
			if p.stoppedErr() == nil {
				raced.Store(true)
			}
		}()

		p.relay(pr)
		<-watcherDone
		if raced.Load() {
			t.Fatalf("trial %d: stoppedErr() was nil the instant Events() closed", i)
		}
	}
}
