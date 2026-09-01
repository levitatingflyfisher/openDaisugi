package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// The blank import registers the real claude adapter with the package
	// registry. Without it, adapters.Get("claude") in Restore's resume path
	// would find nothing, and TestRestoreBringsBackTheLayoutAndMarksPanesHonestly
	// would never actually exercise startPane's headless branch - it would take
	// the "no adapter named" path instead, and COPPICE_CLAUDE_BIN below would
	// go unused. No other file in this package registers claude, codex or
	// sprig: the fake-headless-* adapters elsewhere in this package are local,
	// in-memory stand-ins registered by hand, not this one.
	_ "github.com/opendaisugi/coppice/internal/adapters/claude"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func TestRestoreBringsBackTheLayoutAndMarksPanesHonestly(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	cwd := t.TempDir()

	// Restore() reaches startPane and the claude adapter for a headless pane
	// with a recorded session id. Without this the test would spawn the
	// operator's real `claude -p … --resume sess-42` against their
	// subscription. Point the adapter at the replay script instead.
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "adapters", "fake-claude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_CLAUDE_BIN", fake)
	t.Setenv("COPPICE_CLAUDE_FIXTURE",
		filepath.Join(filepath.Dir(fake), "claude-stream.jsonl"))
	// argvLog proves the resume actually reached the binary, rather than
	// merely that Restore's resume branch was taken - see the argv poll
	// below, after Restore returns.
	argvLog := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("COPPICE_CLAUDE_ECHO_ARGV", argvLog)

	// First server: create two panes, one of which records a harness session id.
	s1, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s1.Close() })
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s1.RegisterPaneCommands()
	roundTrip(t, s1,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],`+
			`"kind":"pty","label":"the pty one"}`,
		`{"id":"2","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],`+
			`"kind":"pty","label":"the other one"}`)
	if err := s1.Tree().UpdatePane("w1:p2", func(p *layout.Pane) {
		p.HarnessSessionID = "sess-42"
		p.Harness = "claude"
		p.Kind = layout.KindHeadless
	}); err != nil {
		t.Fatal(err)
	}
	s1.saveLayout()
	_ = s1.Close()

	// Second server on the same data dir.
	s2, err := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	// s1 released the lock in Close, so s2 can take it.
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s2.RegisterPaneCommands()
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	rep := s2.LastRestore()
	if rep.Resumed != 1 {
		t.Fatalf("report says %d resumed panes, want 1: w1:p2 should have resumed", rep.Resumed)
	}
	if rep.MarkedUnknown != 0 {
		t.Fatalf("report marked %d panes unknown, want 0: w1:p2 should have resumed, not gone unknown",
			rep.MarkedUnknown)
	}
	// The old wording, "w1:p2 resumed harness session
	// sess-42.", claimed a completed resume on the strength of a successful
	// spawn alone - startPane returning nil says the adapter was asked to
	// resume, not that it actually did. The note now says what actually
	// happened at this point: the ask was made.
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "w1:p2") && strings.Contains(n, "sess-42") {
			found = true
			if strings.Contains(n, "resumed harness session") {
				t.Fatalf("note %q claims a completed resume from a spawn alone", n)
			}
			if !strings.Contains(n, "asked") {
				t.Fatalf("note %q does not say the resume was only asked for", n)
			}
		}
	}
	if !found {
		t.Fatalf("no restore note named w1:p2 and sess-42: %v", rep.Notes)
	}

	// The fake writes its argv before its read loop starts, but Start is
	// asynchronous, so poll rather than read once straight after Restore.
	deadline := time.Now().Add(3 * time.Second)
	var argv string
	for {
		b, readErr := os.ReadFile(argvLog)
		if readErr == nil && len(strings.TrimSpace(string(b))) > 0 {
			argv = string(b)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the resumed adapter never reported its argv")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(argv, "--resume sess-42") {
		t.Fatalf("resumed argv = %q, want it to contain --resume sess-42", argv)
	}

	p1, ok := s2.Tree().Pane("w1:p1")
	if !ok {
		t.Fatal("w1:p1 did not survive the restart")
	}
	if p1.Label != "the pty one" || p1.Cwd != cwd {
		t.Fatalf("restored pane = %+v, want the label and cwd back", p1)
	}
	if !p1.Closed {
		t.Fatal("a pty pane whose process is gone came back open, want it marked closed")
	}
	ev, ok := s2.States().Current("w1:p1")
	if !ok {
		t.Fatal("the restored pane has no state at all")
	}
	if ev.State == proto.StateIdle {
		t.Fatalf("a restored pane reports idle, want done or unknown: %+v", ev)
	}
	if ev.State != proto.StateDone && ev.State != proto.StateUnknown {
		t.Fatalf("restored state = %s, want done or unknown", ev.State)
	}

	// A new pane after a restore must not reuse an id a client already holds.
	next, err := s2.Tree().CreatePane("w1", "w1:t1", layout.Pane{Cwd: cwd, Kind: layout.KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "w1:p3" {
		t.Fatalf("post-restore pane id = %q, want w1:p3", next.ID)
	}
}

func TestRestoreReportsWhatItCouldAndCouldNotDo(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	cwd := t.TempDir()
	s1, _ := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	t.Cleanup(func() { _ = s1.Close() })
	if err := s1.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s1.RegisterPaneCommands()
	// A pane already ended before the restart - here, one whose own
	// process exited - must be counted on its own (AlreadyClosed), not
	// folded into MarkedDone/MarkedUnknown alongside the pane that was
	// actually open when the daemon went away. An operator pane.close is
	// not this case any more: it removes the record at once, so
	// there is nothing left for a later restart to find and count.
	roundTrip(t, s1,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`,
		`{"id":"3","cmd":"pane.wait_output","pane":"w1:p2","state":"done","timeout_ms":5000}`)
	s1.saveLayout()
	_ = s1.Close()

	s2, _ := New(Config{SocketPath: filepath.Join(dir, "b.sock"), DataDir: dir})
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s2.RegisterPaneCommands()
	if err := s2.Restore(); err != nil {
		t.Fatal(err)
	}
	rep := s2.LastRestore()
	if rep.Panes != 2 {
		t.Fatalf("report says %d panes, want 2", rep.Panes)
	}
	if rep.AlreadyClosed != 1 {
		t.Fatalf("report says %d panes were already closed, want 1: w1:p2 closed before the restart",
			rep.AlreadyClosed)
	}
	if rep.MarkedDone != 1 {
		t.Fatalf("report says %d panes marked done, want 1: only w1:p1 was open", rep.MarkedDone)
	}
	if rep.Resumed != 0 {
		t.Fatalf("report claims %d resumed panes, want 0: a pty process cannot be resumed",
			rep.Resumed)
	}
	if len(rep.Notes) == 0 {
		t.Fatal("the report carries no notes, so an operator cannot see what was lost")
	}

	got := roundTrip(t, s2, `{"id":"1","cmd":"server.status"}`)
	b, _ := json.Marshal(got[0].Result)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["restore"]; !ok {
		t.Fatalf("server.status does not carry the restore report: %s", b)
	}
}

func TestRestoreRefusesWithoutTheStartLock(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Restore(); err == nil {
		t.Fatal("Restore ran without the start lock, want a refusal")
	}
}

// A handler reads s.tree with no lock of its own once Serve has started -
// Handle already refuses new registrations for the same class of hazard, and
// Restore uses the same serving flag and the same locking to refuse here.
func TestRestoreRefusesAfterServeHasStarted(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	// Serve sets s.serving at the very start of its own goroutine, so the
	// instant after go func(){ s.Serve() }() is not guaranteed to see it yet.
	// Poll until it does, the way TestHandleAfterServeStartsIsRefused
	// (server_test.go) already polls Handle for the same reason.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := s.Restore(); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Restore kept succeeding after Serve started, want it to eventually refuse")
		}
		time.Sleep(time.Millisecond)
	}
}

// Restore replaces the whole tree, so a live pane whose record only exists
// in the old tree would be orphaned if Restore ever ran while one was
// running. This never happens through the ordinary path - Restore always
// runs before Serve starts anything - so this drives the guard directly,
// with a LivePane this test injects by hand, rather than standing up a
// whole running pane just to reach the same check.
func TestRestoreRefusesWhenALivePaneExists(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s.putLive("w1:p1", &LivePane{})
	if err := s.Restore(); err == nil {
		t.Fatal("Restore ran with a live pane already registered, want a refusal")
	}
}

func TestRestoreWithNoLayoutFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(); err != nil {
		t.Fatalf("Restore on a fresh data dir = %v, want nil", err)
	}
	if len(s.Tree().Panes()) != 0 {
		t.Fatal("a fresh data dir produced panes")
	}
	// A caller that does `for (const n of status.restore.notes)` over the
	// server.status payload must see an empty array, never null.
	b, err := json.Marshal(s.LastRestore())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"notes":[]`) {
		t.Fatalf("restore report json = %s, want notes to serialize as [] rather than null", b)
	}
}

func TestACorruptLayoutFileIsRefusedAndKept(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "layout.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := New(Config{SocketPath: filepath.Join(dir, "a.sock"), DataDir: dir})
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	err := s.Restore()
	if err == nil {
		t.Fatal("Restore accepted a corrupt layout file, want an error")
	}
	// The bad file must still be there. Overwriting it would destroy the only
	// record of what the operator had.
	if _, statErr := os.Stat(filepath.Join(dir, "layout.json")); statErr != nil {
		t.Fatal("Restore deleted the corrupt layout file")
	}
}
