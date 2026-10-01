package foreman

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fake transcripts only, in a scratch home. Nothing here reads a real
// ~/.claude or ~/.codex.

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func claudeLine(typ, uuid, ts, content string, extra string) string {
	return fmt.Sprintf(`{"type":%q,"uuid":%q,"sessionId":"s1","timestamp":%q,"cwd":"/work/proj"%s,"message":{"role":%q,"content":%s}}`,
		typ, uuid, ts, extra, typ, content)
}

// fakeHome makes one Claude session (s1: 2 user + 2 assistant messages that
// count, and lines that do not) and one Codex session (c1: 1 user + 1
// assistant that count).
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeLines(t, filepath.Join(home, ".claude", "projects", "-work-proj", "s1.jsonl"),
		claudeLine("user", "u1", "2026-03-01T10:00:00.000Z", `"fix the bug"`, ""),
		claudeLine("assistant", "a1", "2026-03-01T10:00:05.000Z",
			`[{"type":"thinking","thinking":"secret"},{"type":"text","text":"looking"},{"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]`, ""),
		claudeLine("user", "u2", "2026-03-01T10:00:06.000Z", `[{"type":"tool_result","content":"file list"}]`, ""),
		claudeLine("user", "u3", "2026-03-01T10:00:07.000Z", `"<command-name>/clear</command-name>"`, ""),
		claudeLine("user", "u4", "2026-03-01T10:00:08.000Z", `"meta"`, `,"isMeta":true`),
		claudeLine("assistant", "a2", "2026-03-01T10:00:09.000Z", `[{"type":"text","text":"side"}]`, `,"isSidechain":true`),
		`{"type":"summary","summary":"x"}`,
		`not json`,
		claudeLine("user", "u5", "2026-03-02T09:00:00.000Z", `[{"type":"text","text":"thanks"}]`, ""),
		claudeLine("assistant", "a3", "2026-03-02T09:00:01.000Z", `[{"type":"text","text":"done"}]`, ""),
	)
	writeLines(t, filepath.Join(home, ".codex", "sessions", "2026", "03", "05", "rollout-2026-03-05T08-00-00-c1.jsonl"),
		`{"timestamp":"2026-03-05T08:00:00.000Z","type":"session_meta","payload":{"id":"c1","cwd":"/work/other"}}`,
		`{"timestamp":"2026-03-05T08:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"timestamp":"2026-03-05T08:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"write a test"}]}}`,
		`{"timestamp":"2026-03-05T08:00:03.000Z","type":"response_item","payload":{"type":"reasoning","summary":[]}}`,
		`{"timestamp":"2026-03-05T08:00:04.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"go test\"]}","call_id":"k1"}}`,
		`{"timestamp":"2026-03-05T08:00:05.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"k1","output":"ok"}}`,
		`{"timestamp":"2026-03-05T08:00:06.000Z","type":"event_msg","payload":{"type":"user_message","message":"write a test"}}`,
		`{"timestamp":"2026-03-05T08:00:07.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"written"}]}}`,
	)
	return home
}

func TestScanCountsTheMessagesAThatMatter(t *testing.T) {
	home := fakeHome(t)
	ss := Scan(home, ImportOpts{}, ImportState{}, nil)
	all, by := Totals(ss)
	if all.Sessions != 2 || all.Messages != 6 {
		t.Fatalf("all = %+v, want 2 sessions and 6 messages", all)
	}
	if by[SrcClaude].Messages != 4 || by[SrcCodex].Messages != 2 {
		t.Fatalf("by source = %+v", by)
	}
	if ss[0].Key != "claude:s1" || ss[1].Key != "codex:c1" {
		t.Fatalf("order = %s, %s; want the oldest first", ss[0].Key, ss[1].Key)
	}
	want := "past chats: 2 (about 6 messages)\n  claude: 1 chat, 4 messages\n  codex: 1 chat, 2 messages\n"
	if got := Describe(ss); got != want {
		t.Fatalf("Describe =\n%q\nwant\n%q", got, want)
	}
}

func TestScanFiltersBySourceSinceAndTools(t *testing.T) {
	home := fakeHome(t)
	if all, _ := Totals(Scan(home, ImportOpts{From: "codex"}, ImportState{}, nil)); all.Sessions != 1 || all.Messages != 2 {
		t.Fatalf("codex only = %+v", all)
	}
	since := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	if all, _ := Totals(Scan(home, ImportOpts{Since: since}, ImportState{}, nil)); all.Messages != 4 {
		t.Fatalf("since 03-02 = %+v, want the 2 claude and 2 codex messages", all)
	}
	// Tool calls are off by default, and on they add the calls and results.
	if all, _ := Totals(Scan(home, ImportOpts{Tools: true}, ImportState{}, nil)); all.Messages != 6+1+1+2 {
		t.Fatalf("with tools = %+v, want 10 (a claude tool call and result, a codex call and output)", all)
	}
}

func TestThinkingIsNeverTaken(t *testing.T) {
	home := fakeHome(t)
	r, err := readSession(filepath.Join(home, ".claude", "projects", "-work-proj", "s1.jsonl"), SrcClaude, ImportOpts{Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.msgs {
		if strings.Contains(m.Text, "secret") {
			t.Fatalf("thinking leaked: %q", m.Text)
		}
	}
	if r.msgs[0].Text != "claude user in proj: fix the bug" {
		t.Fatalf("first = %q", r.msgs[0].Text)
	}
	if !strings.Contains(r.msgs[2].Text, "claude tool in proj: Bash ls -la") {
		t.Fatalf("tool = %q", r.msgs[2].Text)
	}
}

func TestAnEstimateMakesNoCallAndNoWrite(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir()
	Scan(home, ImportOpts{}, ImportState{}, nil)
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("scan wrote %v", ents)
	}
	var out, errw bytes.Buffer
	t.Setenv("HOME", home)
	if code := Main([]string{"import", "--dry-run", "--data-dir", dir}, "", &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "past chats: 2 (about 6 messages)") || !strings.HasSuffix(out.String(), "nothing was changed\n") {
		t.Fatalf("dry run said %q", out.String())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("--dry-run wrote %v", ents)
	}
}

// importRig is a chat, a summarizer that records its calls, and an
// importer over a fake home.
type importRig struct {
	t     *testing.T
	dir   string
	home  string
	c     *Chat
	sum   *recSummarizer
	im    *Importer
	mu    sync.Mutex
	clock *testClock
}

type recSummarizer struct {
	mu    sync.Mutex
	calls []string
}

func (r *recSummarizer) Compress(_ context.Context, m Message, limit int) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "c:"+m.Kind+":"+clip(m.Text, 40))
	r.mu.Unlock()
	return "c " + clip(m.Text, 60), nil
}

func (r *recSummarizer) Merge(_ context.Context, id NodeID, left, right string, limit int) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "m:"+id.Name())
	r.mu.Unlock()
	return "m " + clip(left, 40) + " | " + clip(right, 40), nil
}

func newImportRig(t *testing.T, home string) *importRig {
	t.Helper()
	r := &importRig{t: t, dir: filepath.Join(t.TempDir(), "foreman"), home: home, sum: &recSummarizer{}, clock: newClock()}
	r.open()
	return r
}

func (r *importRig) open() {
	if r.c != nil {
		r.c.Close()
	}
	r.c = openT(r.t, r.dir, r.sum, r.clock, nil)
	r.im = &Importer{Dir: r.dir}
}

func (r *importRig) accept(o ImportOpts) {
	r.t.Helper()
	st, _ := ReadImportState(r.dir)
	if err := Accept(r.dir, o, Scan(r.home, o, st, r.c.HasAct)); err != nil {
		r.t.Fatal(err)
	}
}

// pump is the compactor loop in small: drain, then feed one, until idle.
func (r *importRig) pump(idle func() bool) int {
	r.t.Helper()
	n := 0
	for {
		if err := r.c.Drain(ctx, r.sum, &r.mu); err != nil {
			r.t.Fatal(err)
		}
		fed, err := r.im.Feed(ctx, r.c, &r.mu, idle)
		if err != nil {
			r.t.Fatal(err)
		}
		if !fed {
			return n
		}
		n++
	}
}

func (r *importRig) kinds() []string {
	var out []string
	for i := int64(0); i < r.c.Count(); i++ {
		m, _ := r.c.Message(i)
		out = append(out, m.Kind)
	}
	return out
}

func TestImportLogsPastMessagesOneAtATimeWithTheirOwnDates(t *testing.T) {
	r := newImportRig(t, fakeHome(t))
	r.accept(ImportOpts{})
	if got := r.pump(nil); got != 6 {
		t.Fatalf("fed %d, want 6", got)
	}
	for i, k := range r.kinds() {
		if k != KindPast {
			t.Fatalf("message %d is %q, want past", i, k)
		}
	}
	if d, _ := r.c.Date(0); d != "2026-03-01T10:00:00Z" {
		t.Fatalf("date 0 = %q, want the date the message was said", d)
	}
	st, _ := ReadImportState(r.dir)
	if st.Word() != "done" || st.Done != 6 || st.Total != 6 {
		t.Fatalf("state = %s %d/%d", st.Word(), st.Done, st.Total)
	}
	if r.c.Pending() != 0 {
		t.Fatalf("%d nodes pending", r.c.Pending())
	}
}

func TestAReImportBringsOnlyTheNewMessages(t *testing.T) {
	home := fakeHome(t)
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	r.pump(nil)
	// Nothing is new now.
	st, _ := ReadImportState(r.dir)
	if ss := Scan(home, ImportOpts{}, st, r.c.HasAct); len(ss) != 0 {
		t.Fatalf("scan after import found %+v", ss)
	}
	// The session grows: one message at the very time of the last one
	// imported, and one later.
	path := filepath.Join(home, ".claude", "projects", "-work-proj", "s1.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, claudeLine("user", "u6", "2026-03-02T09:00:01.000Z", `"same second"`, ""))
	fmt.Fprintln(f, claudeLine("user", "u7", "2026-03-02T09:05:00.000Z", `"later"`, ""))
	f.Close()
	ss := Scan(home, ImportOpts{}, st, r.c.HasAct)
	if all, _ := Totals(ss); all.Messages != 2 {
		t.Fatalf("scan = %+v, want the 2 new messages", all)
	}
	r.accept(ImportOpts{})
	if got := r.pump(nil); got != 2 {
		t.Fatalf("fed %d, want 2", got)
	}
	if r.c.Count() != 8 {
		t.Fatalf("%d messages, want 8", r.c.Count())
	}
}

func TestALaterImportWithToolsBringsTheCallsOlderThanTheLastMessage(t *testing.T) {
	home := fakeHome(t)
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	r.pump(nil)
	st, _ := ReadImportState(r.dir)
	o := ImportOpts{Tools: true}
	if all, _ := Totals(Scan(home, o, st, r.c.HasAct)); all.Messages != 4 {
		t.Fatalf("tool calls and results = %+v, want 4", all)
	}
	r.accept(o)
	if got := r.pump(nil); got != 4 {
		t.Fatalf("fed %d, want 4", got)
	}
}

func TestImportWaitsWhileALiveTurnRuns(t *testing.T) {
	r := newImportRig(t, fakeHome(t))
	r.accept(ImportOpts{})
	busy := true
	if got := r.pump(func() bool { return !busy }); got != 0 || r.c.Count() != 0 {
		t.Fatalf("fed %d while a turn ran", got)
	}
	busy = false
	if got := r.pump(func() bool { return !busy }); got != 6 {
		t.Fatalf("fed %d after the turn, want 6", got)
	}
}

func TestPauseStopsNewImportCallsAndResumeGoesOn(t *testing.T) {
	r := newImportRig(t, fakeHome(t))
	r.accept(ImportOpts{})
	for k := 0; k < 2; k++ {
		r.c.Drain(ctx, r.sum, &r.mu)
		if fed, _ := r.im.Feed(ctx, r.c, &r.mu, nil); !fed {
			t.Fatal("nothing fed")
		}
	}
	if err := SetPaused(r.dir, true); err != nil {
		t.Fatal(err)
	}
	calls := len(r.sum.calls)
	if got := r.pump(nil); got != 0 {
		t.Fatalf("fed %d while paused", got)
	}
	if r.c.Count() != 2 {
		t.Fatalf("%d messages, want 2", r.c.Count())
	}
	if len(r.sum.calls) != calls {
		t.Fatalf("the summarizer was called %d times while paused", len(r.sum.calls)-calls)
	}
	if st, _ := ReadImportState(r.dir); st.Word() != "paused" {
		t.Fatalf("word = %s", st.Word())
	}
	SetPaused(r.dir, false)
	if got := r.pump(nil); got != 4 {
		t.Fatalf("fed %d after resume, want 4", got)
	}
}

func TestARestartResumesWhereItStopped(t *testing.T) {
	r := newImportRig(t, fakeHome(t))
	r.accept(ImportOpts{})
	for k := 0; k < 3; k++ {
		r.c.Drain(ctx, r.sum, &r.mu)
		r.im.Feed(ctx, r.c, &r.mu, nil)
	}
	r.open() // the chat and the importer are made again
	if got := r.pump(nil); got != 3 {
		t.Fatalf("fed %d after the restart, want the 3 left", got)
	}
	if r.c.Count() != 6 {
		t.Fatalf("%d messages, want 6 and none twice", r.c.Count())
	}
}

func TestACrashBetweenTheLogAndTheStateLogsNothingTwice(t *testing.T) {
	r := newImportRig(t, fakeHome(t))
	r.accept(ImportOpts{})
	// The message reached the log; the state was not saved.
	st, _ := ReadImportState(r.dir)
	rd, _ := readSession(st.Queue[0].Path, st.Queue[0].Src, st.Opts())
	m := rd.msgs[0]
	if _, err := r.c.AppendImported(ctx, m.Text, m.TS, m.Act); err != nil {
		t.Fatal(err)
	}
	r.open()
	r.pump(nil)
	if r.c.Count() != 6 {
		t.Fatalf("%d messages, want 6", r.c.Count())
	}
	seen := map[string]bool{}
	for i := int64(0); i < r.c.Count(); i++ {
		mm, _ := r.c.Message(i)
		if seen[mm.Text] {
			t.Fatalf("%q was logged twice", mm.Text)
		}
		seen[mm.Text] = true
	}
	if st, _ := ReadImportState(r.dir); st.Word() != "done" || st.Total != st.Done {
		t.Fatalf("state = %s %d/%d", st.Word(), st.Done, st.Total)
	}
}

func TestDecliningIsRememberedAndTheCommandStillWorks(t *testing.T) {
	home := fakeHome(t)
	dir := filepath.Join(t.TempDir(), "foreman")
	if err := Decline(dir); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadImportState(dir)
	if st.Word() != "declined" || !st.Offered {
		t.Fatalf("state = %+v", st)
	}
	if c, err := Open(ctx, Config{Dir: dir, Now: newClock().now}); err == nil {
		st2, _ := ReadImportState(dir)
		if st2.Word() != "declined" {
			t.Fatalf("opening the chat changed the choice: %s", st2.Word())
		}
		c.Close()
	}
	t.Setenv("HOME", home)
	var out, errw bytes.Buffer
	if code := Main([]string{"import", "--data-dir", dir}, "", &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	st, _ = ReadImportState(dir)
	if st.Word() != "running" || st.Total != 6 || len(st.Queue) != 2 {
		t.Fatalf("after the command: %s total %d queue %d", st.Word(), st.Total, len(st.Queue))
	}
}

func TestImportCommandOutputAndErrors(t *testing.T) {
	home := fakeHome(t)
	dir := filepath.Join(t.TempDir(), "foreman")
	t.Setenv("HOME", home)
	run := func(args ...string) (string, string, int) {
		var out, errw bytes.Buffer
		code := Main(append([]string{"import", "--data-dir", dir}, args...), "", &out, &errw)
		return out.String(), errw.String(), code
	}
	if o, _, c := run("--status"); c != 0 || o != "import: off\n" {
		t.Fatalf("status = %q %d", o, c)
	}
	if _, e, c := run("--from", "gemini"); c != 1 || !strings.Contains(e, `--from needs claude, codex or all, not "gemini"`) {
		t.Fatalf("bad from: %q %d", e, c)
	}
	if _, e, c := run("--since", "march"); c != 1 || !strings.Contains(e, `--since needs a date like 2026-03-01`) {
		t.Fatalf("bad since: %q %d", e, c)
	}
	if _, e, c := run("--pause", "--resume"); c != 1 || !strings.Contains(e, "import takes one of") {
		t.Fatalf("two verbs: %q %d", e, c)
	}
	o, _, c := run("--from", "codex")
	if c != 0 || !strings.HasPrefix(o, "past chats: 1 (about 2 messages)\n  codex: 1 chat, 2 messages\n") || !strings.HasSuffix(o, "never ahead of a live message\n") {
		t.Fatalf("import = %q %d", o, c)
	}
	if o, _, _ := run("--pause"); o != "import paused (0 of 2 messages)\n" {
		t.Fatalf("pause = %q", o)
	}
	if o, _, _ := run("--status"); o != "import: paused\nprogress: 0 of 2 messages\n" {
		t.Fatalf("status = %q", o)
	}
	if o, _, _ := run("--resume"); o != "import resumed (0 of 2 messages)\n" {
		t.Fatalf("resume = %q", o)
	}
	if o, _, _ := run("--status", "--json"); o != `{"state":"running","offered":true,"paused":false,"done":0,"total":2,"queued":1}`+"\n" {
		t.Fatalf("status json = %q", o)
	}
	// Run again: the codex session is queued already, so only claude is new.
	if o, _, _ := run(); !strings.HasPrefix(o, "past chats: 2 (about 6 messages)") {
		t.Fatalf("second import = %q", o)
	}
	if st, _ := ReadImportState(dir); st.Total != 6 || len(st.Queue) != 2 {
		t.Fatalf("queue after the second import: total %d queue %d", st.Total, len(st.Queue))
	}
}

func TestAMissingTranscriptIsDroppedAndTheTotalFollows(t *testing.T) {
	home := fakeHome(t)
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	if err := os.Remove(filepath.Join(home, ".codex", "sessions", "2026", "03", "05", "rollout-2026-03-05T08-00-00-c1.jsonl")); err != nil {
		t.Fatal(err)
	}
	r.pump(nil)
	st, _ := ReadImportState(r.dir)
	if st.Word() != "done" || st.Done != 4 || st.Total != 4 {
		t.Fatalf("state = %s %d/%d", st.Word(), st.Done, st.Total)
	}
}

func TestABrokenStateFileIsSetAside(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ImportFile), []byte("{nope"), 0o600)
	st, err := ReadImportState(dir)
	if err != nil || st.Word() != "off" {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ImportFile+".broken")); err != nil {
		t.Fatalf("the bad file was not kept: %v", err)
	}
}
