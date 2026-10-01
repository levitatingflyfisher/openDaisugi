package foreman

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fix round 1 of the import review (IM-R-1 to IM-R-5, IM-R-11).

const fixMarkerText = "Limit: 100 bytes. Ruler.\n" + CompactorMarker + "\n\nThe message:\n<message>x</message>"

// FM-R-78: a session of coppice's own is never imported: not by cwd under
// the data or foreman dir, and not by the compactor's marker sentence.
func TestImportSkipsCoppiceOwnSessions(t *testing.T) {
	home := fakeHome(t)
	dir := filepath.Join(t.TempDir(), "state", "foreman")
	// A compactor call: the cwd is the foreman's compactor dir.
	writeLines(t, filepath.Join(home, ".claude", "projects", "-state-foreman-compactor", "k1.jsonl"),
		fmt.Sprintf(`{"type":"user","uuid":"k1u","sessionId":"k1","timestamp":"2026-03-03T10:00:00.000Z","cwd":%q,"message":{"role":"user","content":"anything"}}`,
			filepath.Join(dir, "compactor")))
	// A compactor call by sprig: a neutral cwd, so only the question tells.
	writeLines(t, filepath.Join(home, ".claude", "projects", "-tmp", "k2.jsonl"),
		fmt.Sprintf(`{"type":"user","uuid":"k2u","sessionId":"k2","timestamp":"2026-03-03T10:00:00.000Z","cwd":"/tmp","message":{"role":"user","content":%q}}`, fixMarkerText),
		`{"type":"assistant","uuid":"k2a","sessionId":"k2","timestamp":"2026-03-03T10:00:01.000Z","cwd":"/tmp","message":{"role":"assistant","content":[{"type":"text","text":"a summary"}]}}`)
	o := ImportOpts{Skip: []string{dir}}
	ss := Scan(home, o, ImportState{}, nil)
	all, _ := Totals(ss)
	if all.Sessions != 2 || all.Messages != 6 {
		t.Fatalf("scan = %+v (%v), want only the 2 real sessions", all, ss)
	}
	// Without the rule they are counted: the test can fail.
	if all, _ := Totals(Scan(home, ImportOpts{}, ImportState{}, nil)); all.Sessions != 3 {
		t.Fatalf("without Skip: %+v", all)
	}
}

// A real user message that only quotes the marker later is still imported:
// the first user message decides.
func TestAMarkerLaterInASessionDoesNotHideIt(t *testing.T) {
	home := t.TempDir()
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		claudeLine("user", "u1", "2026-03-01T10:00:00.000Z", `"hello"`, ""),
		claudeLine("user", "u2", "2026-03-01T10:00:01.000Z", `"`+CompactorMarker+`"`, ""))
	if all, _ := Totals(Scan(home, ImportOpts{}, ImportState{}, nil)); all.Messages != 2 {
		t.Fatalf("scan = %+v", all)
	}
}

// FM-R-72: a live message is compacted before any import job, even one
// queued earlier.
func TestALiveJobRunsBeforeAnyImportJob(t *testing.T) {
	home := t.TempDir()
	long := strings.Repeat("x", 700)
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, claudeLine("user", fmt.Sprintf("u%d", i), fmt.Sprintf("2026-03-01T10:00:0%d.000Z", i), `"`+long+`"`, ""))
	}
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), lines...)
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	for k := 0; k < 2; k++ {
		if err := r.c.Drain(ctx, r.sum, &r.mu); err != nil {
			t.Fatal(err)
		}
		if fed, err := r.im.Feed(ctx, r.c, &r.mu, nil); err != nil || !fed {
			t.Fatalf("feed %d: %v %v", k, fed, err)
		}
	}
	if _, err := r.c.Append(ctx, KindUser, "LIVE "+long); err != nil {
		t.Fatal(err)
	}
	before := len(r.sum.calls)
	r.pump(nil)
	calls := r.sum.calls[before:]
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "c:user:LIVE") {
		t.Fatalf("calls %v: want the live message first, with no import call before it", calls)
	}
}

// While a turn runs no model call starts for an import node, though one is
// pending; a live node still runs.
func TestNoImportCallWhileATurnRuns(t *testing.T) {
	home := t.TempDir()
	long := strings.Repeat("x", 700)
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		claudeLine("user", "u0", "2026-03-01T10:00:00.000Z", `"`+long+`"`, ""),
		claudeLine("user", "u1", "2026-03-01T10:00:01.000Z", `"`+long+`"`, ""))
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	if fed, err := r.im.Feed(ctx, r.c, &r.mu, nil); err != nil || !fed {
		t.Fatal(fed, err)
	}
	busy := true
	if err := r.c.DrainGated(ctx, r.sum, &r.mu, func() bool { return busy }); err != nil {
		t.Fatal(err)
	}
	if len(r.sum.calls) != 0 || r.c.Pending() == 0 {
		t.Fatalf("calls %v, pending %d: an import call ran during a turn", r.sum.calls, r.c.Pending())
	}
	if _, err := r.c.Append(ctx, KindUser, "LIVE "+long); err != nil {
		t.Fatal(err)
	}
	if err := r.c.DrainGated(ctx, r.sum, &r.mu, func() bool { return busy }); err != nil {
		t.Fatal(err)
	}
	if len(r.sum.calls) != 1 || !strings.HasPrefix(r.sum.calls[0], "c:user:LIVE") {
		t.Fatalf("calls %v: the live node should run, the import node not", r.sum.calls)
	}
	busy = false
	if err := r.c.DrainGated(ctx, r.sum, &r.mu, func() bool { return busy }); err != nil || r.c.Pending() != 0 {
		t.Fatalf("after the turn: %v, pending %d", err, r.c.Pending())
	}
}

// probeLock is a chat lock that knows whether it is held.
type probeLock struct {
	mu   sync.Mutex
	held bool
}

func (l *probeLock) Lock()   { l.mu.Lock(); l.held = true }
func (l *probeLock) Unlock() { l.held = false; l.mu.Unlock() }

// FM-R-81: no transcript is parsed under the chat lock, also when several
// queued sessions are imported already and each is popped in turn.
func TestNoTranscriptIsParsedUnderTheChatLock(t *testing.T) {
	home := fakeHome(t)
	writeLines(t, filepath.Join(home, ".claude", "projects", "p2", "s2.jsonl"),
		strings.Replace(claudeLine("user", "w1", "2026-03-04T10:00:00.000Z", `"third"`, ""), `"s1"`, `"s2"`, 1))
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	r.pump(nil)
	// The state is lost; everything is queued again, all of it in the log.
	if err := os.Remove(filepath.Join(r.dir, ImportFile)); err != nil {
		t.Fatal(err)
	}
	if err := Accept(r.dir, ImportOpts{}, Scan(home, ImportOpts{}, ImportState{}, nil)); err != nil {
		t.Fatal(err)
	}
	if st, _ := ReadImportState(r.dir); len(st.Queue) != 3 {
		t.Fatalf("queue = %d, want 3", len(st.Queue))
	}
	lk := &probeLock{}
	underLock := 0
	readHook = func(string) {
		if lk.held {
			underLock++
		}
	}
	t.Cleanup(func() { readHook = nil })
	r.im = &Importer{Dir: r.dir}
	if fed, err := r.im.Feed(ctx, r.c, lk, nil); err != nil || fed {
		t.Fatalf("Feed = %v, %v; every message is in the log already", fed, err)
	}
	if underLock != 0 {
		t.Fatalf("%d transcripts were parsed under the chat lock", underLock)
	}
	if st, _ := ReadImportState(r.dir); st.Word() != "done" || r.c.Count() != 7 {
		t.Fatalf("state %s, %d messages", st.Word(), r.c.Count())
	}
}

// failAppendN makes the n-th append to the message log fail mid-line.
func failAppendN(t *testing.T, n int) {
	t.Helper()
	real := openAppend
	calls := 0
	openAppend = func(path string) (appendFile, error) {
		f, err := real(path)
		if err != nil || !strings.Contains(filepath.ToSlash(path), "/main/") {
			return f, err
		}
		calls++
		if calls == n {
			return &failFile{File: f.(*os.File), mode: "write"}, nil
		}
		return f, nil
	}
	t.Cleanup(func() { openAppend = real })
}

func splitRig(t *testing.T, text string) (*importRig, string) {
	t.Helper()
	home := t.TempDir()
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		claudeLine("user", "u0", "2026-03-01T10:00:00.000Z", `"`+text+`"`, ""),
		claudeLine("user", "u1", "2026-03-01T10:00:01.000Z", `"after"`, ""))
	r := &importRig{t: t, dir: filepath.Join(t.TempDir(), "foreman"), home: home, sum: &recSummarizer{}, clock: newClock()}
	r.c = openT(t, r.dir, r.sum, r.clock, func(c *Config) { c.SplitBytes = 100 })
	r.im = &Importer{Dir: r.dir}
	r.accept(ImportOpts{})
	return r, home
}

func allText(c *Chat) []string {
	var out []string
	for i := int64(0); i < c.Count(); i++ {
		m, _ := c.Message(i)
		out = append(out, m.Text)
	}
	return out
}

// FM-R-82: a retry after an append that failed part way logs the missing
// parts only.
func TestARetryAfterAFailedAppendLogsNoPartTwice(t *testing.T) {
	text := strings.Repeat("a", 250)
	r, _ := splitRig(t, text)
	failAppendN(t, 2) // the second part fails
	if _, err := r.im.Feed(ctx, r.c, &r.mu, nil); !errors.Is(err, errInjected) {
		t.Fatalf("Feed = %v, want the injected error", err)
	}
	if got := r.c.Count(); got != 1 {
		t.Fatalf("%d parts logged, want 1", got)
	}
	r.pump(nil)
	texts := allText(r.c)
	joined := strings.Join(texts[:len(texts)-1], "")
	if want := "claude user in proj: " + text; joined != want {
		t.Fatalf("the parts join to %d bytes, want %d (%q)", len(joined), len(want), texts)
	}
	if texts[len(texts)-1] != "claude user in proj: after" {
		t.Fatalf("last = %q", texts[len(texts)-1])
	}
}

// The same after a restart: part 1 is in the log, the state never heard of
// it, and parts 2 and 3 must still come.
func TestARestartAfterAHalfLoggedMessageLogsTheRest(t *testing.T) {
	text := strings.Repeat("b", 250)
	r, _ := splitRig(t, text)
	failAppendN(t, 2)
	if _, err := r.im.Feed(ctx, r.c, &r.mu, nil); !errors.Is(err, errInjected) {
		t.Fatalf("Feed = %v", err)
	}
	r.c.Close()
	r.c = openT(t, r.dir, r.sum, r.clock, func(c *Config) { c.SplitBytes = 100 })
	r.im = &Importer{Dir: r.dir}
	r.pump(nil)
	texts := allText(r.c)
	if want := "claude user in proj: " + text; strings.Join(texts[:len(texts)-1], "") != want {
		t.Fatalf("parts = %q", texts)
	}
	seen := map[string]int{}
	for _, tx := range texts {
		seen[tx]++
	}
	if seen["claude user in proj: after"] != 1 {
		t.Fatalf("texts = %q", texts)
	}
}

// A failure after every part is logged (the settle after it) does not log
// the message again.
func TestARetryAfterAFullyLoggedMessageLogsNothing(t *testing.T) {
	text := strings.Repeat("c", 250)
	r, _ := splitRig(t, text)
	m := ImportMsg{Text: "claude user in proj: " + text, TS: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), Act: "imp:claude:u0"}
	if _, err := r.c.AppendImported(ctx, m.Text, m.TS, m.Act); err != nil {
		t.Fatal(err)
	}
	n := r.c.Count()
	if _, err := r.c.AppendImported(ctx, m.Text, m.TS, m.Act); err != nil || r.c.Count() != n {
		t.Fatalf("second append: %v, %d messages, want %d", err, r.c.Count(), n)
	}
}

// FM-R-79: a second accept changes only the sessions it adds.
func TestASecondAcceptKeepsTheOptionsOfQueuedSessions(t *testing.T) {
	home := fakeHome(t)
	r := newImportRig(t, home)
	since := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	o1 := ImportOpts{From: "claude", Since: since}
	st, _ := ReadImportState(r.dir)
	if err := Accept(r.dir, o1, Scan(home, o1, st, nil)); err != nil {
		t.Fatal(err)
	}
	// Later: the owner adds Codex, with no date and with tools.
	o2 := ImportOpts{From: "all", Tools: true}
	st, _ = ReadImportState(r.dir)
	if err := Accept(r.dir, o2, Scan(home, o2, st, nil)); err != nil {
		t.Fatal(err)
	}
	r.pump(nil)
	for _, tx := range allText(r.c) {
		if strings.Contains(tx, "fix the bug") || strings.Contains(tx, "tool") && strings.Contains(tx, "claude") {
			t.Fatalf("the Claude session was imported with the new options: %q", tx)
		}
	}
	texts := strings.Join(allText(r.c), "\n")
	for _, want := range []string{"claude user in proj: thanks", "codex user in other: write a test", "codex tool in other: shell"} {
		if !strings.Contains(texts, want) {
			t.Fatalf("missing %q in\n%s", want, texts)
		}
	}
}

// FM-R-80: two files with one session id are two sessions.
func TestTwoFilesWithOneSessionIdAreBothImported(t *testing.T) {
	home := t.TempDir()
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s1.jsonl"),
		claudeLine("user", "u1", "2026-03-01T10:00:00.000Z", `"first file"`, ""))
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s1-copy.jsonl"),
		claudeLine("user", "u2", "2026-03-02T10:00:00.000Z", `"second file"`, ""))
	writeLines(t, filepath.Join(home, ".claude", "projects", "q", "s1.jsonl"),
		claudeLine("user", "u3", "2026-03-03T10:00:00.000Z", `"third file"`, ""))
	ss := Scan(home, ImportOpts{}, ImportState{}, nil)
	if all, _ := Totals(ss); all.Sessions != 3 || all.Messages != 3 {
		t.Fatalf("scan = %v", ss)
	}
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	r.pump(nil)
	if r.c.Count() != 3 {
		t.Fatalf("%d messages, want 3: %q", r.c.Count(), allText(r.c))
	}
	st, _ := ReadImportState(r.dir)
	if st.Word() != "done" || st.Done != 3 {
		t.Fatalf("state %s %d", st.Word(), st.Done)
	}
}

// IM-R-9: --json alone is no mode, and must not start an import.
func TestJSONAloneNeverStartsAnImport(t *testing.T) {
	home := fakeHome(t)
	dir := filepath.Join(t.TempDir(), "foreman")
	t.Setenv("HOME", home)
	var out, errw strings.Builder
	if code := Main([]string{"import", "--json", "--data-dir", dir}, "", &out, &errw); code != 1 || !strings.Contains(errw.String(), "--json goes with --status") {
		t.Fatalf("exit %d, stderr %q", code, errw.String())
	}
	if st, _ := ReadImportState(dir); st.Decision != "" || len(st.Queue) != 0 {
		t.Fatalf("--json queued something: %+v", st)
	}
}
