package sprig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
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

// writeFixture writes complete session-tree JSON row lines (the same shape
// harness/sprig/session_tree.go's SessionWriter appends) to a temp file and
// returns its path, for COPPICE_SPRIG_FIXTURE - fake-sprig.sh appends these
// to the session file for whichever turn is currently running, standing in
// for whatever OnAssistant/OnToolCall/OnVerdict/OnToolResult calls a real
// turn would make.
func writeFixture(t *testing.T, rows ...string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(f, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// harness holds everything a test needs to drive and inspect a
// fake-sprig.sh-backed pane: the running proc, the directory sprig writes
// session files into, and the argv log every turn appends to (fake-sprig.sh
// appends, never overwrites, so a multi-turn test can see every turn's own
// argv in order).
type harness struct {
	t       *testing.T
	p       pane.Proc
	dir     string
	sessDir string
	argvLog string
}

// start wires COPPICE_SPRIG_BIN at fake-sprig.sh and starts a real
// adapter.Start against it, with no --session/--resume given in the base
// argv: the first turn mints a fresh id. extraArgv, if given, rides on
// StartOpts.Argv alongside --session-dir, same as a caller's own
// --gate/--max-turns (or, for the tests that need it, an explicit
// --session/--resume of the caller's own choosing) would.
func start(t *testing.T, extraArgv ...string) *harness {
	t.Helper()
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(dir, "argv.txt")
	t.Setenv("COPPICE_SPRIG_BIN", td(t, "fake-sprig.sh"))
	t.Setenv("COPPICE_SPRIG_ECHO_ARGV", argvLog)

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	argv := append([]string{"--session-dir", sessDir}, extraArgv...)
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd: dir, Argv: argv, Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return &harness{t: t, p: p, dir: dir, sessDir: sessDir, argvLog: argvLog}
}

// setFixture points the NEXT turn's fake-sprig.sh invocation at rows. Safe
// to call again between turns: t.Setenv just reassigns the value.
func (h *harness) setFixture(rows ...string) {
	h.t.Helper()
	h.t.Setenv("COPPICE_SPRIG_FIXTURE", writeFixture(h.t, rows...))
}

func waitFor(t *testing.T, p pane.Proc, match func(pane.Event) bool, d time.Duration) pane.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatal("the event channel closed before the event arrived")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no matching event within %s", d)
		}
	}
}

func waitForIdle(t *testing.T, p pane.Proc) {
	t.Helper()
	waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvState && e.State == "idle" }, 5*time.Second)
}

// The flag name is sprig's, not ours. Reading it out of sprig's own source is
// what stops this adapter drifting onto an invented flag whose tests all pass
// while a real sprig pane refuses to start.
func TestTheSessionFlagMatchesSprigsOwnSource(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sprig", "cli.go"))
	if err != nil {
		t.Skipf("harness/sprig/cli.go is not readable from here: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `fs.String("session",`) {
		t.Fatal(`harness/sprig/cli.go does not declare fs.String("session", …). ` +
			"The adapter's flag name has drifted from sprig's.")
	}
	if strings.Contains(src, `fs.String("session-id",`) {
		t.Fatal("sprig now has a --session-id flag. Re-read cli.go and update the adapter.")
	}
	if !strings.Contains(src, `fs.String("session-dir",`) ||
		!strings.Contains(src, `fs.String("resume",`) {
		t.Fatal("harness/sprig/cli.go no longer declares --session-dir and --resume")
	}
}

func TestStartWithoutASessionDirIsAnError(t *testing.T) {
	toolchain.RequireOrSkip(t)
	g, _ := pane.NewGrid(80, 24)
	defer g.Close()
	if _, err := New().Start(context.Background(),
		pane.StartOpts{Cwd: t.TempDir(), Sock: "/dev/null", PaneID: "w1:p1"}, g); err == nil {
		t.Fatal("Start succeeded with no --session-dir, want an error naming the flag")
	}
}

// --- argv helpers: flagValue / insertAfterSessionDir / stripSessionFlags --

func TestFlagValueStopsAtDoubleDash(t *testing.T) {
	argv := []string{"--session-dir", "/d", "--", "--session", "x"}
	if v, ok := flagValue(argv, "session-dir"); !ok || v != "/d" {
		t.Fatalf(`flagValue(argv, "session-dir") = (%q,%v), want ("/d",true)`, v, ok)
	}
	if v, ok := flagValue(argv, "session"); ok {
		t.Fatalf(`flagValue(argv, "session") = (%q,%v), want not-found: "--session" only `+
			"appears after a literal --, which sprig's own flag.Parse would never read as a flag", v, ok)
	}
}

func TestInsertAfterSessionDirPlacesInjectedFlagsBeforeDoubleDash(t *testing.T) {
	argv := []string{"--session-dir", "/d", "--", "--session", "x"}
	got := insertAfterSessionDir(argv, "--session", "newid")
	want := []string{"--session-dir", "/d", "--session", "newid", "--", "--session", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("insertAfterSessionDir() = %v, want %v", got, want)
	}
}

// A --session-dir-shaped token that only appears AFTER a literal -- is
// opaque positional content, not the real flag - the case
// TestInsertAfterSessionDirPlacesInjectedFlagsBeforeDoubleDash's own input
// cannot distinguish, because its real --session-dir already sits before
// -- regardless of whether the "--" boundary is honoured at all. This one
// puts a --session-dir LOOKALIKE only in the tail, so a version that never
// stops at -- would find it there and splice inside the tail instead of
// after --gate, corrupting content that was supposed to be left alone.
func TestInsertAfterSessionDirIgnoresALookalikeAfterDoubleDash(t *testing.T) {
	argv := []string{"--gate", "--", "--session-dir", "d"}
	got := insertAfterSessionDir(argv, "--session", "newid")
	want := []string{"--gate", "--session", "newid", "--", "--session-dir", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("insertAfterSessionDir() = %v, want %v", got, want)
	}
}

func TestStripSessionFlagsLeavesContentAfterDoubleDashAlone(t *testing.T) {
	argv := []string{"--session-dir", "/d", "--gate", "--", "--session", "x"}
	got := stripSessionFlags(argv)
	want := []string{"--gate", "--", "--session", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stripSessionFlags() = %v, want %v", got, want)
	}
}

// --- parseRow: the tree-row → pane.Event mapping, tested directly and in
// isolation from any process. These confirm it still holds under the
// per-turn rewrite. -----------------------------------------------------

func TestParseRowSessionHeaderIsSilentButSetsSessionID(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"session","v":1,"id":"s1","harness":"sprig","cwd":"/repo","ts":0}`))
	if evs != nil {
		t.Fatalf("session header produced events %+v, want none", evs)
	}
	if id, ok := p.SessionID(); !ok || id != "s1" {
		t.Fatalf("SessionID() = (%q,%v), want (s1,true)", id, ok)
	}
}

func TestParseRowAssistantWithTextBecomesText(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"assistant","text":"hello","usage":{},"toolUses":[]}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvText || evs[0].Text != "hello" {
		t.Fatalf(`got %+v, want one EvText "hello"`, evs)
	}
	if st, _ := pane.StateOf(evs[0]); st != "working" {
		t.Fatalf("StateOf = %q, want working", st)
	}
}

func TestParseRowAssistantWithNoTextStaysVisibleAndWorking(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"assistant","text":"","usage":{},"toolUses":[]}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvState || evs[0].State != "working" {
		t.Fatalf("got %+v, want one EvState working", evs)
	}
}

func TestParseRowToolCallBecomesAToolEvent(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(
		`{"type":"tool_call","toolUseId":"tu1","name":"Bash","input":{"command":"ls"},"detail":"ls"}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvTool || evs[0].Tool != "Bash" {
		t.Fatalf("got %+v, want one EvTool Bash", evs)
	}
}

// A deny verdict is what a coppice client shows as blocked, with the clause
// attached. That is the whole point of using sprig's own gate rather than
// reading its screen.
func TestParseRowDenyVerdictBecomesBlockedWithClause(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(
		`{"type":"verdict","toolUseId":"tu1","decision":"deny","reason":"shell.deny[2]","clause":"shell.deny[2]"}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvState || evs[0].State != "blocked" {
		t.Fatalf("got %+v, want one EvState blocked", evs)
	}
	if evs[0].Detail == "" {
		t.Fatal("the blocked event carries no clause, so a client cannot say why")
	}
}

func TestParseRowAllowVerdictIsWorkingNotBlocked(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"verdict","toolUseId":"tu1","decision":"allow","reason":"","clause":""}`))
	if len(evs) != 1 || evs[0].State != "working" {
		t.Fatalf("got %+v, want working: an allowed call means the agent continues", evs)
	}
}

func TestParseRowToolResultFailureIsMarked(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"tool_result","toolUseId":"tu1","ok":false,"summary":"boom"}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvText ||
		!strings.Contains(evs[0].Text, "boom") || !strings.Contains(evs[0].Text, "failed") {
		t.Fatalf("got %+v, want EvText naming the failure", evs)
	}
}

func TestParseRowToolResultSuccessIsPlain(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"tool_result","toolUseId":"tu1","ok":true,"summary":"a.txt"}`))
	if len(evs) != 1 || evs[0].Text != "a.txt" {
		t.Fatalf(`got %+v, want a plain EvText "a.txt"`, evs)
	}
}

func TestParseRowMalformedJSONYieldsAnErrorNeverIdle(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":`))
	if len(evs) != 1 || evs[0].Kind != pane.EvError {
		t.Fatalf("got %+v, want one EvError", evs)
	}
	if st, _ := pane.StateOf(evs[0]); st == "idle" {
		t.Fatal("a malformed tree row produced idle")
	}
}

func TestParseRowUnmodelledTypeStaysVisibleAndWorking(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"future_thing"}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvState || evs[0].State != "working" || evs[0].Detail != "future_thing" {
		t.Fatalf("got %+v, want one EvState working Detail=future_thing", evs)
	}
}

// harness/sprig/loop.go's own Agent.Run calls OnPrompt first thing, before
// any assistant turn - session_tree.go's OnPrompt writes this row.
func TestParseRowPromptBecomesWorking(t *testing.T) {
	p := &proc{}
	evs := p.parseRow([]byte(`{"type":"prompt","text":"do the thing"}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvState || evs[0].State != "working" {
		t.Fatalf("got %+v, want one EvState working", evs)
	}
}

// --- lastUsefulLine / isSessionPathBanner: picking the EvError detail -----

// The banner is walked PAST here even though it is the LAST line captured -
// unusual chronologically (cli.go prints it before agent.Run, so in
// practice real content usually follows it and already sits last), but
// exactly the shape that actually exercises the skip: with the banner
// first and real content last (the ordinary case), the plain "take the
// last line" rule already returns the right answer with no skip logic
// doing any work at all.
func TestLastUsefulLineSkipsTheSessionPathBanner(t *testing.T) {
	captured := "sprig: no model backend configured\nsprig: session abc123 at /tmp/abc123.jsonl\n"
	got := lastUsefulLine(captured)
	want := "sprig: no model backend configured"
	if got != want {
		t.Fatalf("lastUsefulLine() = %q, want %q", got, want)
	}
}

// The degenerate case the skip logic actually guards: nothing but the
// banner was ever captured (agent.Run failed silently, say), so the right
// answer is "" - the caller's own fallback, not the banner's text
// misreported as if it explained the failure.
func TestLastUsefulLineIsEmptyWhenOnlyTheBannerWasCaptured(t *testing.T) {
	captured := "sprig: session abc123 at /tmp/abc123.jsonl\n"
	if got := lastUsefulLine(captured); got != "" {
		t.Fatalf("lastUsefulLine() = %q, want empty: only the banner was ever captured", got)
	}
}

// A session-tree ERROR shares its leading "sprig: session " text with the
// informational banner (cli.go:121 vs cli.go:109/116) but is real content,
// never the thing to skip.
func TestLastUsefulLineKeepsASessionTreeErrorDespiteTheSharedPrefix(t *testing.T) {
	captured := "sprig: session abc123 at /tmp/abc123.jsonl\n" +
		"sprig: session tree: session file exists: /tmp/abc123.jsonl\n"
	got := lastUsefulLine(captured)
	want := "sprig: session tree: session file exists: /tmp/abc123.jsonl"
	if got != want {
		t.Fatalf("lastUsefulLine() = %q, want %q", got, want)
	}
}

func TestLastUsefulLineCapsALongLine(t *testing.T) {
	long := strings.Repeat("x", detailLineCap+50)
	got := lastUsefulLine(long)
	if len(got) != detailLineCap {
		t.Fatalf("lastUsefulLine() length = %d, want the %d-rune cap", len(got), detailLineCap)
	}
}

// --- the one-process-per-turn lifecycle, against fake-sprig.sh -------------

func TestFirstTurnCreatesTheFileAndYieldsHeaderIdAndRows(t *testing.T) {
	h := start(t)
	h.setFixture(
		`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"hello","usage":{},"toolUses":[]}`,
		`{"type":"tool_call","id":"c1","ts":2,"toolUseId":"tu1","name":"Bash","input":{"command":"ls"},"detail":"ls"}`,
	)
	if err := h.p.Prompt("do the thing"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvText && e.Text == "hello" }, 5*time.Second)
	waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvTool && e.Tool == "Bash" }, 5*time.Second)
	waitForIdle(t, h.p)

	id, ok := h.p.SessionID()
	if !ok || id == "" {
		t.Fatalf("SessionID() = (%q,%v), want a minted id", id, ok)
	}
	b, err := os.ReadFile(filepath.Join(h.sessDir, id+".jsonl"))
	if err != nil {
		t.Fatalf("the session file was never created: %v", err)
	}
	if !strings.Contains(string(b), `"type":"session"`) {
		t.Fatalf("the session file has no header row: %s", b)
	}
}

func TestSecondTurnAppendsAndOnlyNewRowsArrive(t *testing.T) {
	h := start(t)
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"first answer","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("first task"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvText && e.Text == "first answer" }, 5*time.Second)
	waitForIdle(t, h.p)

	h.setFixture(`{"type":"assistant","id":"a2","ts":2,"model":"m","text":"second answer","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("second task"); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvText }, 5*time.Second)
	if ev.Text != "second answer" {
		t.Fatalf("text = %q, want only the second turn's own new row, never a replay of the first", ev.Text)
	}
}

// A pane RESUMING a session id that already has history
// from BEFORE this pane ever started (a previous pane, or a previous
// coppice server run) must never replay that history - only the tail
// offset seeded at Start, before the first turn ever runs, prevents it.
// TestSecondTurnAppendsAndOnlyNewRowsArrive covers the same principle
// ACROSS two turns of one pane, where the offset carries forward
// naturally; this covers the offset's INITIAL seeding, which is the part
// that goes wrong if Start ever starts a resumed pane's tail at 0.
func TestResumingAPreExistingSessionNeverReplaysItsHistory(t *testing.T) {
	h := start(t)
	preExisting := filepath.Join(h.sessDir, "prior.jsonl")
	if err := os.WriteFile(preExisting,
		[]byte(`{"type":"session","v":1,"id":"prior","harness":"sprig","cwd":"/","ts":0}`+"\n"+
			`{"type":"assistant","id":"old1","ts":1,"model":"m","text":"OLD ROW must never replay",`+
			`"usage":{},"toolUses":[]}`+"\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd: h.dir, Argv: []string{"--session-dir", h.sessDir},
		Resume: "prior", Sock: "/dev/null", PaneID: "w1:p2",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	h.setFixture(`{"type":"assistant","id":"new1","ts":2,"model":"m","text":"NEW ROW","usage":{},"toolUses":[]}`)
	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}

	ev := waitFor(t, p, func(e pane.Event) bool { return e.Kind == pane.EvText }, 5*time.Second)
	if ev.Text != "NEW ROW" {
		t.Fatalf("text = %q, want only the new row - pre-existing history must never replay", ev.Text)
	}
}

func TestTaskReachesArgvAfterDoubleDash(t *testing.T) {
	h := start(t)
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"ok","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("do the thing"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)
	b, err := os.ReadFile(h.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "-- do the thing") {
		t.Fatalf("argv %q does not carry the task after --", b)
	}
}

// A task beginning with "-" must still reach the fake as the task itself,
// never misread as a flag. turnArgv's unconditional "--" is a structural
// guarantee (verified separately against the real parser: a direct probe
// against harness/sprig/cli.go confirmed `-- -foo` is read as the
// task, while `-foo` without `--` is "flag provided but not defined"); this
// proves it end to end, through the fake, at both places the task travels:
// the argv log AND the tree's own "prompt" row (fake-sprig.sh writes one,
// same as real sprig's OnPrompt).
func TestATaskBeginningWithADashReachesTheFakeAsTheTask(t *testing.T) {
	h := start(t)
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"ok","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("-foo"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)

	b, err := os.ReadFile(h.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "-- -foo") {
		t.Fatalf("argv %q does not carry -foo as the task after --", b)
	}

	id, ok := h.p.SessionID()
	if !ok {
		t.Fatal("SessionID never became available")
	}
	tree, err := os.ReadFile(filepath.Join(h.sessDir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tree), `"text":"-foo"`) {
		t.Fatalf("the session tree's own prompt row does not carry -foo: %s", tree)
	}
}

// SHOULD-FIX 1: the offset seeding must be unconditional, not gated on
// isResume - a client-supplied --session naming a file that ALREADY
// exists (not this adapter's own resume path at all) is just as much a
// pre-existing file, and its history must not replay into the pane while
// the turn itself fails.
func TestASessionOfAPreExistingFileReplaysNothing(t *testing.T) {
	h := start(t)
	// The file must exist BEFORE Start ever runs - Start is what seeds the
	// tail offset from it, so creating it only after Start (as the harness's
	// own start() would force here) proves nothing: the offset would already
	// have been seeded at 0 against a file that, at THAT moment, did not
	// exist yet.
	if err := os.WriteFile(filepath.Join(h.sessDir, "collide2.jsonl"),
		[]byte(`{"type":"session","v":1,"id":"collide2","harness":"sprig","cwd":"/","ts":0}`+"\n"+
			`{"type":"assistant","id":"old1","ts":1,"model":"m","text":"OLD ROW must never replay",`+
			`"usage":{},"toolUses":[]}`+"\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	g, err := pane.NewGrid(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	p, err := New().Start(context.Background(), pane.StartOpts{
		Cwd: h.dir, Argv: []string{"--session-dir", h.sessDir, "--session", "collide2"},
		Sock: "/dev/null", PaneID: "w1:p2",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if err := p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, p, func(e pane.Event) bool {
		return e.Kind == pane.EvError || (e.Kind == pane.EvText && strings.Contains(e.Text, "OLD ROW"))
	}, 5*time.Second)
	if ev.Kind != pane.EvError {
		t.Fatalf("first event = %+v, want the refusal EvError, never the replayed old row", ev)
	}
}

// SHOULD-FIX 2: a turn whose spawn itself fails must never wedge the pane
// into --resume of a file that was never created. The next Prompt, with a
// working binary again, must still be treated as fresh.
func TestAFailedFirstSpawnLeavesTheNextTurnFreshNotResume(t *testing.T) {
	h := start(t)
	t.Setenv("COPPICE_SPRIG_BIN", filepath.Join(h.dir, "does-not-exist"))
	if err := h.p.Prompt("first"); err == nil {
		t.Fatal("Prompt with an unstartable binary reported success")
	}
	t.Setenv("COPPICE_SPRIG_BIN", td(t, "fake-sprig.sh"))
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"ok","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("second"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)
	b, err := os.ReadFile(h.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "--session ") || strings.Contains(string(b), "--resume ") {
		t.Fatalf("argv %q should use --session (fresh): the failed first spawn never "+
			"created a file, so there is nothing to --resume", b)
	}
}

// A DIFFERENT failure shape from the one above: here cmd.Start itself
// SUCCEEDS (the process launches fine, so sessionEstablished's latch does
// get set true), but sprig's own flag.FlagSet rejects a bad flag in the
// caller's extra argv and exits 2 before ever reaching
// openOrNewSessionWriter - so no session file is written even though a
// turn genuinely ran. turnArgv's own os.Stat is the ONLY thing that
// recovers this on the next turn: with the stat removed (trusting the
// latch alone, matching should-fix 2's own original bug one step further
// back), this pane would try --resume forever against a file that will
// never exist. Pins both halves independently: mutation (a) alone (the
// latch's position) does not fail this test - by design, sessionEstablished
// really is meant to be set after cmd.Start succeeds regardless of what
// the child process goes on to do - only removing the stat does.
func TestASpawnThatExitsBeforeWritingTheFileRetriesFreshNotResume(t *testing.T) {
	h := start(t)
	t.Setenv("COPPICE_SPRIG_FAIL_BEFORE_SESSION", "1")
	if err := h.p.Prompt("first"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvError }, 5*time.Second)

	t.Setenv("COPPICE_SPRIG_FAIL_BEFORE_SESSION", "")
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"ok","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("second"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)

	b, err := os.ReadFile(h.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("argv log has %d lines, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[1], "--session ") || strings.Contains(lines[1], "--resume ") {
		t.Fatalf("second turn argv %q should retry --session (fresh): the first spawn "+
			"exited before ever writing a file, so there is nothing to --resume", lines[1])
	}
}

// The first turn is fresh (--session), every turn after
// resumes (--resume) - even though both turns belong to the same pane and
// the same sessionID.
func TestFirstTurnUsesSessionSecondTurnUsesResume(t *testing.T) {
	h := start(t)
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"one","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("first"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)
	h.setFixture(`{"type":"assistant","id":"a2","ts":2,"model":"m","text":"two","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("second"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)

	b, err := os.ReadFile(h.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("argv log has %d lines, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "--session ") || strings.Contains(lines[0], "--resume ") {
		t.Fatalf("first turn argv %q should use --session, not --resume", lines[0])
	}
	if !strings.Contains(lines[1], "--resume ") || strings.Contains(lines[1], "--session ") {
		t.Fatalf("second turn argv %q should use --resume, not --session", lines[1])
	}
}

func TestPromptWhileATurnIsRunningIsRefused(t *testing.T) {
	h := start(t)
	t.Setenv("COPPICE_SPRIG_SLEEP", "2")
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"slow","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("first"); err != nil {
		t.Fatal(err)
	}
	err := h.p.Prompt("second")
	if err == nil {
		t.Fatal("a second Prompt while a turn is running reported success")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error %q does not say a turn is already running", err.Error())
	}
}

// sprig's own real refusal (NewSessionWriter: "session file exists: PATH")
// must surface as an EvError naming that fact, not vanish into a bare
// nonzero exit code.
func TestARefusedSessionOfAnExistingSessionSurfacesAsAnError(t *testing.T) {
	h := start(t, "--session", "collide")
	if err := os.WriteFile(filepath.Join(h.sessDir, "collide.jsonl"),
		[]byte(`{"type":"session","v":1,"id":"collide","harness":"sprig","cwd":"/","ts":0}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvError }, 5*time.Second)
	if !strings.Contains(ev.Detail, "session file exists") {
		t.Fatalf("error detail %q does not surface sprig's own refusal message", ev.Detail)
	}
	if !strings.Contains(ev.Detail, "exit=1") {
		t.Fatalf("error detail %q drops the exit code even though a message was captured", ev.Detail)
	}
}

// sprig's own real refusal (OpenSessionWriter's os.Stat failure) must
// likewise surface as an EvError.
// A resume-started pane (StartOpts.Resume, or an explicit --resume in argv)
// whose session file is missing must NEVER mint a fresh session under the
// id it was asked to resume - not on the first turn, not on any later one.
// It refuses instead, permanently: sprig is never even spawned (never mind
// asked to try --session), and the file never comes to exist through this
// pane's own doing. This supersedes an earlier version of this test that
// asserted the OPPOSITE: that a --resume of
// a missing file spawned sprig, let IT refuse ("no such file", exit 1), and
// surfaced that as an EvError. That behaviour silently minted a fresh
// session under the resumed id on the very next Prompt (SessionID() would
// report the same id either way, hiding a lost history from a client with
// no way to tell).
func TestAResumeStartedPaneNeverMintsUnderTheResumedID(t *testing.T) {
	h := start(t, "--resume", "ghost")

	err1 := h.p.Prompt("go")
	if err1 == nil {
		t.Fatal("Prompt on a resume-started pane with no session file reported success")
	}
	if !strings.Contains(err1.Error(), "ghost") || !strings.Contains(err1.Error(), "not found") {
		t.Fatalf("Prompt error %q does not name the missing session", err1.Error())
	}
	ev := waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvError }, 5*time.Second)
	if !strings.Contains(ev.Detail, "ghost") || !strings.Contains(ev.Detail, "not found") {
		t.Fatalf("EvError detail %q does not name the missing session", ev.Detail)
	}

	err2 := h.p.Prompt("again")
	if err2 == nil {
		t.Fatal("a second Prompt on the same stuck pane reported success")
	}
	if !strings.Contains(err2.Error(), "ghost") {
		t.Fatalf("the second refusal %q does not name the session either", err2.Error())
	}

	b, err := os.ReadFile(h.argvLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "--session ghost") {
		t.Fatalf("argv log %q shows --session ghost was passed - a resume-started pane "+
			"must never mint a session under the id it was asked to resume", b)
	}
	if _, err := os.Stat(filepath.Join(h.sessDir, "ghost.jsonl")); !os.IsNotExist(err) {
		t.Fatal("ghost.jsonl exists after two refused turns, want it to never have been created")
	}
}

func TestSteerIsHonestlyUnsupported(t *testing.T) {
	h := start(t)
	if err := h.p.Steer("nudge"); !errors.Is(err, pane.ErrUnsupported) {
		t.Fatalf("Steer() = %v, want errors.Is(err, pane.ErrUnsupported)", err)
	}
}

func TestWriteStdinBeforeStopIsUnsupported(t *testing.T) {
	h := start(t)
	if err := h.p.WriteStdin([]byte("x")); !errors.Is(err, pane.ErrUnsupported) {
		t.Fatalf("WriteStdin() = %v, want errors.Is(err, pane.ErrUnsupported)", err)
	}
}

func TestPromptAfterStopIsAnHonestError(t *testing.T) {
	h := start(t)
	if err := h.p.Stop(); err != nil {
		t.Fatal(err)
	}
	err := h.p.Prompt("again")
	if err == nil {
		t.Fatal("Prompt after Stop reported success")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Prompt after Stop = %q, want it to say the pane stopped", err.Error())
	}
}

func TestWriteStdinAfterStopIsAnHonestError(t *testing.T) {
	h := start(t)
	if err := h.p.Stop(); err != nil {
		t.Fatal(err)
	}
	err := h.p.WriteStdin([]byte("x"))
	if err == nil {
		t.Fatal("WriteStdin after Stop reported success")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("WriteStdin after Stop = %q, want it to say the pane stopped", err.Error())
	}
}

// pane.close reaches Stop while a turn is still streaming. Closing the
// events channel from Stop would let the turn goroutine send on a closed
// channel, which panics, and a panic in an adapter goroutine takes the
// whole daemon down and orphans every other pane's process. Run this with
// -race.
//
// The events channel defaults to a 256-slot buffer, which even this test's
// own 30-row fixture never fills, so the send-side "case <-p.stop" branch
// in relay - and the bounded-drain path that must not drop a turn's own
// terminal event - would otherwise never actually run under this test.
// Shrinking eventsBufferSize to 1 for the duration forces at least some of
// these 50 iterations to block on a full buffer and unblock via those
// branches instead, same trick claude's and codex's own tests use.
func TestStopWhileTheStreamIsFlowingDoesNotPanic(t *testing.T) {
	old := eventsBufferSize
	eventsBufferSize = 1
	t.Cleanup(func() { eventsBufferSize = old })
	for i := 0; i < 50; i++ {
		h := start(t)
		// Thirty rows, plus the turn's own final "turn complete" terminal
		// event: a 1-slot events buffer can hold at most one of these at a
		// time, so receiving exactly the first one below and then calling
		// Stop immediately leaves a real backlog sitting in p.in - not an
		// empty channel Stop races ahead of. A large count matters, not
		// just a nonzero one: relay's own outer loop can opportunistically
		// drain several more items normally before it happens to notice
		// p.stop at all (each is its own race), so a small backlog can be
		// fully exhausted that way before Stop ever gets a chance to
		// interrupt anything - leaving relay's shutdown-drain branch
		// nothing left to deliver even though it was entered.
		rows := make([]string, 30)
		for j := range rows {
			rows[j] = `{"type":"assistant","id":"a` + strconv.Itoa(j) + `","ts":1,"model":"m","text":"x","usage":{},"toolUses":[]}`
		}
		h.setFixture(rows...)
		if err := h.p.Prompt("go"); err != nil {
			t.Fatal(err)
		}
		// Receive exactly the first event - a real synchronisation point,
		// not a sleep: this only returns once relay has actually placed
		// something in the (size-1) events channel, which only happens
		// once fake-sprig.sh has actually run and the tail has actually
		// read its rows. The other 29 rows plus the terminal event are
		// still queued in p.in behind it.
		select {
		case _, ok := <-h.p.Events():
			if !ok {
				t.Fatal("the event channel closed before the first event arrived")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no first event within 5s")
		}
		// Stop with no further drain: the turn goroutine and relay are
		// mid-stream, with several events still queued.
		if err := h.p.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := h.p.Stop(); err != nil {
			t.Fatalf("a second Stop failed: %v", err)
		}
		deadline := time.After(5 * time.Second)
		for done := false; !done; {
			select {
			case _, ok := <-h.p.Events():
				if !ok {
					done = true
				}
			case <-deadline:
				t.Fatal("the event channel never closed after Stop")
			}
		}
	}
}

// Stop must not leak the relay, runTurn or tailTurn goroutines. runtime.
// NumGoroutine is noisy (GC, finalizers, the test runner itself), so this
// polls toward the pre-test baseline rather than checking once - but the
// baseline itself is the bar: settling at or below it, with no slack, is
// what actually proves nothing was left running.
func TestStopLeavesNoGoroutineRunning(t *testing.T) {
	before := runtime.NumGoroutine()
	h := start(t)
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"x","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("go"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, h.p)
	if err := h.p.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if runtime.NumGoroutine() <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not settle after Stop: now %d, before %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A --resume onto a large pre-existing session tree must not read the whole
// file in one poll: readNewRows is bounded by maxTailReadPerPoll, and a
// file bigger than that takes a few more polls (TailInterval apart) to
// catch up, rather than one unbounded read and parse.
func TestReadNewRowsIsBoundedPerPoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tree.jsonl")
	row := `{"type":"assistant","id":"a1","ts":1,"model":"m","text":"x","usage":{},"toolUses":[]}` + "\n"
	var b strings.Builder
	for b.Len() < maxTailReadPerPoll*2 {
		b.WriteString(row)
	}
	full := b.String()
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &proc{stop: make(chan struct{})}
	close(p.stop) // send() becomes a no-op: this test only cares about the offset

	newOffset := p.readNewRows(path, 0)
	if newOffset >= int64(len(full)) {
		t.Fatalf("readNewRows(0) consumed the whole %d-byte file in one poll (offset %d), "+
			"want it bounded to roughly maxTailReadPerPoll (%d)", len(full), newOffset, maxTailReadPerPoll)
	}
	if newOffset < maxTailReadPerPoll/2 {
		t.Fatalf("readNewRows(0) offset = %d, want it close to maxTailReadPerPoll (%d), not far short of it",
			newOffset, maxTailReadPerPoll)
	}
}

func TestPidsNamesEachTurn(t *testing.T) {
	h := start(t)
	h.setFixture(`{"type":"assistant","id":"a1","ts":1,"model":"m","text":"hello","usage":{},"toolUses":[]}`)
	if err := h.p.Prompt("do the thing"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, h.p, func(e pane.Event) bool { return e.Kind == pane.EvText && e.Text == "hello" }, 5*time.Second)
	waitForIdle(t, h.p)
	if pids := h.p.(pane.Pider).Pids(); len(pids) != 1 || pids[0] <= 0 {
		t.Fatalf("pids after one turn = %v", pids)
	}
}
