package pando

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

// A worker runs in the project, where the owner works too. Its transcript sits
// in the same Claude Code project dir, so only its session id can tell it from
// the owner's chat.

func sessLine(sid, uuid, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"cwd":"/work/proj","timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":%q}]}}`,
		uuid, sid, time.Now().UTC().Format(DateLayout), text)
}

func countText(t *testing.T, dir, needle string) int {
	t.Helper()
	c, err := OpenReadOnly(dir)
	if err != nil {
		return 0
	}
	n := 0
	for i := int64(0); i < c.Count(); i++ {
		if m, err := c.Message(i); err == nil && strings.Contains(m.Text, needle) {
			n++
		}
	}
	return n
}

func TestAHookInASkippedProcessPutsItsSessionOnTheSkipList(t *testing.T) {
	data := t.TempDir()
	tr := filepath.Join(t.TempDir(), "w-1.jsonl")
	ev := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"w-1","transcript_path":%q}`, tr)
	code, out := skipCLI(t, ev, map[string]string{"DAISUGI_PANDO_SKIP": "1"}, "", "hook", "--data-dir", data)
	if code != 0 || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	set := LoadSkip(SkipListIn(data))
	if !set["w-1"] || !set[tr] {
		t.Fatalf("the list lacks the session: %v", set)
	}
	// The owner's hook adds nothing.
	ev = `{"hook_event_name":"SessionStart","session_id":"o-1","transcript_path":"/x/o-1.jsonl"}`
	skipCLI(t, ev, nil, "", "hook", "--data-dir", data)
	if LoadSkip(SkipListIn(data))["o-1"] {
		t.Fatal("an owner session was put on the skip list")
	}
}

func TestFollowAndImportDropAListedSessionAndKeepTheOwnersInTheSameProjectDir(t *testing.T) {
	dir, proj := followFixture(t)
	writeTranscript(t, filepath.Join(proj, "worker-1.jsonl"), sessLine("worker-1", "w1", "WORKERLINE one"))
	writeTranscript(t, filepath.Join(proj, "owner-1.jsonl"), sessLine("owner-1", "o1", "OWNERLINE one"))
	if err := AppendSkip(SkipListPath(dir), "worker-1"); err != nil {
		t.Fatal(err)
	}
	o := &FollowOpts{Dir: dir, Log: &bytes.Buffer{}, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	added, _, _, err := FollowOnce(context.Background(), o)
	if err != nil || added != 1 {
		t.Fatalf("added %d %v", added, err)
	}
	if countText(t, dir, "WORKERLINE") != 0 || countText(t, dir, "OWNERLINE") != 1 {
		t.Fatal("follow logged the wrong session")
	}
	// The importer reads the same list.
	idir := filepath.Join(t.TempDir(), "pando", "work")
	if err := AppendSkip(SkipListPath(idir), "worker-1"); err != nil {
		t.Fatal(err)
	}
	r, err := readSession(filepath.Join(proj, "worker-1.jsonl"), SrcClaude,
		ImportOpts{From: SrcClaude, SkipSessions: LoadSkip(SkipListPath(idir))})
	if err != nil || len(r.msgs) != 0 {
		t.Fatalf("the importer read a listed session: %d %v", len(r.msgs), err)
	}
	r, err = readSession(filepath.Join(proj, "owner-1.jsonl"), SrcClaude,
		ImportOpts{From: SrcClaude, SkipSessions: LoadSkip(SkipListPath(idir))})
	if err != nil || len(r.msgs) != 1 {
		t.Fatalf("the importer dropped the owner: %d %v", len(r.msgs), err)
	}
}

// A listed transcript path drops a session whose id the file does not show.
func TestASkipListedPathDropsTheSession(t *testing.T) {
	dir, proj := followFixture(t)
	p := filepath.Join(proj, "renamed.jsonl")
	writeTranscript(t, p, sessLine("inner-id", "w1", "WORKERLINE"))
	if err := AppendSkip(SkipListPath(dir), p); err != nil {
		t.Fatal(err)
	}
	o := &FollowOpts{Dir: dir, Log: &bytes.Buffer{}, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	if added, _, _, err := FollowOnce(context.Background(), o); err != nil || added != 0 {
		t.Fatalf("added %d %v", added, err)
	}
}

// The follow service runs while a worker writes. The worker's hook has put its
// session on the list before its first transcript line, so nothing of it is
// logged, however the polls fall. The owner's chat in the same dir is.
func TestFollowRunningWhileAWorkerWritesLogsOnlyTheOwner(t *testing.T) {
	dir, proj := followFixture(t)
	data := filepath.Dir(filepath.Dir(dir))
	var buf bytes.Buffer
	o := &FollowOpts{Dir: dir, Log: &buf, Interval: 5 * time.Millisecond, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = Follow(ctx, o) }()

	// The worker: the hook first (as the skipped process runs it), then the
	// transcript grows line by line.
	wpath := filepath.Join(proj, "worker-9.jsonl")
	hook := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"worker-9","transcript_path":%q}`, wpath)
	if code, _ := skipCLI(t, hook, map[string]string{"DAISUGI_PANDO_SKIP": "1"}, "", "hook", "--data-dir", data); code != 0 {
		t.Fatal("hook failed")
	}
	var lines []string
	for i := 0; i < 12; i++ {
		lines = append(lines, sessLine("worker-9", fmt.Sprintf("w%d", i), fmt.Sprintf("WORKERLINE %d", i)))
		writeTranscript(t, wpath, lines...)
		if i == 4 {
			writeTranscript(t, filepath.Join(proj, "owner-9.jsonl"), sessLine("owner-9", "o1", "OWNERLINE"))
		}
		time.Sleep(8 * time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countText(t, dir, "OWNERLINE") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // a few more polls over the finished worker file
	stop()
	wg.Wait()
	if n := countText(t, dir, "WORKERLINE"); n != 0 {
		t.Fatalf("the follow service logged %d lines of a worker", n)
	}
	if n := countText(t, dir, "OWNERLINE"); n != 1 {
		t.Fatalf("the owner's chat was logged %d times", n)
	}
}

// The spawner picks the session id and lists it before the child starts, so
// not even the first line can be taken.
func TestASpawnerListsTheIdBeforeTheChildExists(t *testing.T) {
	dir, proj := followFixture(t)
	id, err := NewSkippedSession(filepath.Dir(filepath.Dir(dir)))
	if err != nil || len(id) != 36 {
		t.Fatalf("id %q %v", id, err)
	}
	if !LoadSkip(SkipListPath(dir)).Has(id, "") {
		t.Fatal("the id was not listed when it was handed out")
	}
	writeTranscript(t, filepath.Join(proj, id+".jsonl"), sessLine(id, "w1", "WORKERLINE"))
	o := &FollowOpts{Dir: dir, Log: &bytes.Buffer{}, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	if added, _, _, err := FollowOnce(context.Background(), o); err != nil || added != 0 {
		t.Fatalf("added %d %v", added, err)
	}
	_ = os.Remove(SkipListPath(dir))
}

// A skipped server does not need the store: a worker's Claude Code must not
// see an MCP failure because no store exists yet.
func TestASkippedServeAnswersClosedEvenWhenThereIsNoStore(t *testing.T) {
	data := t.TempDir()
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	code, out := skipCLI(t, in, map[string]string{"DAISUGI_PANDO_SKIP": "1"}, "", "serve", "--data-dir", data)
	if code != 0 || !strings.Contains(out, `"tools":[]`) {
		t.Fatalf("code %d out %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(data, "pando", "work")); err == nil {
		t.Fatal("a skipped server made the store")
	}
}

// The hook reads its event to the end even when it logs nothing, so the
// parent never meets a closed pipe.
func TestASkippedHookReadsItsWholeInput(t *testing.T) {
	data := t.TempDir()
	r := strings.NewReader(`{"hook_event_name":"Stop","session_id":"s","transcript_path":"/x/s.jsonl"}` + "\n")
	var out, errw strings.Builder
	Run([]string{"hook", "--data-dir", data}, CLIEnv{Stdin: r, Out: &out, Err: &errw, Now: newClock().now,
		Getenv: func(k string) string { return map[string]string{"DAISUGI_PANDO_SKIP": "1"}[k] }})
	if r.Len() != 0 {
		t.Fatalf("%d bytes of the event were left unread", r.Len())
	}
}

// A long session fires a hook on every turn. The list holds its session once.
func TestAHooksSessionIsListedOnce(t *testing.T) {
	data := t.TempDir()
	ev := `{"hook_event_name":"Stop","session_id":"w-9","transcript_path":"/x/w-9.jsonl"}`
	for i := 0; i < 4; i++ {
		skipCLI(t, ev, map[string]string{"DAISUGI_PANDO_SKIP": "1"}, "", "hook", "--data-dir", data)
	}
	b, err := os.ReadFile(SkipListIn(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "w-9"); got != 2 {
		t.Fatalf("the list holds the session %d times (id and path), want 2 lines in all:\n%s", got, b)
	}
}

// A spawner with a data dir of its own names its list in the environment;
// the hook lists the session there as well.
func TestAHookListsInTheListTheSpawnerNames(t *testing.T) {
	data, other := t.TempDir(), filepath.Join(t.TempDir(), "pando", "skip.sessions")
	ev := `{"hook_event_name":"SessionStart","session_id":"w-7","transcript_path":"/x/w-7.jsonl"}`
	skipCLI(t, ev, map[string]string{"DAISUGI_PANDO_SKIP": "1", "DAISUGI_PANDO_SKIPLIST": other}, "", "hook", "--data-dir", data)
	if !LoadSkip(other)["w-7"] || !LoadSkip(SkipListIn(data))["w-7"] {
		t.Fatal("the session is not on both lists")
	}
}
