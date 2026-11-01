package pando

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveLine(typ, uuid, ts, text string) string {
	role := typ
	return fmt.Sprintf(`{"type":%q,"uuid":%q,"sessionId":"sess1","cwd":"/work/proj","timestamp":%q,"message":{"role":%q,"content":[{"type":"text","text":%q}]}}`,
		typ, uuid, ts, role, text)
}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIngestClaudeIsLiveIdempotentAndKeepsTheSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pando", "work")
	tr := filepath.Join(t.TempDir(), "sess1.jsonl")
	writeTranscript(t, tr,
		liveLine("user", "u1", "2026-11-05T10:00:00Z", "where is the config"),
		liveLine("assistant", "a1", "2026-11-05T10:00:05Z", "in /etc/x"))
	c, err := Open(context.Background(), Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n, err := c.IngestClaude(context.Background(), tr, nil)
	if err != nil || n != 2 {
		t.Fatalf("first ingest: %d %v", n, err)
	}
	// The same transcript again, and a longer one: only the new line enters.
	writeTranscript(t, tr,
		liveLine("user", "u1", "2026-11-05T10:00:00Z", "where is the config"),
		liveLine("assistant", "a1", "2026-11-05T10:00:05Z", "in /etc/x"),
		liveLine("user", "u2", "2026-11-05T10:01:00Z", "thanks"))
	n, err = c.IngestClaude(context.Background(), tr, nil)
	if err != nil || n != 1 {
		t.Fatalf("second ingest: %d %v", n, err)
	}
	if c.Count() != 3 {
		t.Fatalf("count %d", c.Count())
	}
	m, _ := c.Message(0)
	if m.Kind != KindUser || m.Src != ClaudeSrc || m.Chat != "sess1" || m.Who != "owner" || !strings.Contains(m.Text, "where is the config") {
		t.Fatalf("message 0: %+v", m)
	}
	m, _ = c.Message(1)
	if m.Kind != KindSteward {
		t.Fatalf("message 1 kind %q", m.Kind)
	}
	// A reopened store still knows them.
	c.Close()
	c2, err := Open(context.Background(), Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if n, _ := c2.IngestClaude(context.Background(), tr, nil); n != 0 || c2.Count() != 3 {
		t.Fatalf("reopen logged again: %d %d", n, c2.Count())
	}
}

func TestRunHookNeverFailsAndPrintsTheViewAtSessionStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pando", "work")
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	writeTranscript(t, tr, liveLine("user", "u1", "2026-11-05T10:00:00Z", "remember the blue door"))
	ctx := context.Background()
	if out := RunHook(ctx, dir, HookEvent{Event: "SessionStart"}, nil, nil); out != "" {
		t.Fatalf("no store, no context: %q", out)
	}
	if out := RunHook(ctx, dir, HookEvent{Event: "Stop", TranscriptPath: tr}, nil, nil); out != "" {
		t.Fatalf("stop prints nothing: %q", out)
	}
	out := RunHook(ctx, dir, HookEvent{Event: "SessionStart"}, nil, nil)
	if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "blue door") {
		t.Fatalf("session start: %q", out)
	}
	// A bad transcript path is logged, not raised.
	if out := RunHook(ctx, dir, HookEvent{Event: "Stop", TranscriptPath: "/nonexistent/x.jsonl"}, nil, nil); out != "" {
		t.Fatal(out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, HookLogName)); err != nil || !strings.Contains(string(b), "Stop") {
		t.Fatalf("hook.log: %q %v", b, err)
	}
}

func TestReadHookEventOfGarbageIsEmpty(t *testing.T) {
	ev := ReadHookEvent(bufio.NewReader(strings.NewReader("not json")))
	if ev.Event != "" {
		t.Fatal(ev)
	}
}

func TestAppendLiveRefusesNoAct(t *testing.T) {
	c, err := Open(context.Background(), Config{Dir: filepath.Join(t.TempDir(), "p"), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.AppendLive(context.Background(), Live{Kind: KindUser, Text: "x"}); err == nil {
		t.Fatal("want an error")
	}
}

func TestMCPViewToolShowsTheViewAndTheTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pando", "work")
	c, err := Open(context.Background(), Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Append(context.Background(), KindUser, "the blue door code is 4417"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	out, ok := mcpHandle(&Scope{Store: dir, Dir: dir}, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"view","arguments":{}}}`))
	if !ok || !strings.Contains(string(out), "4417") {
		t.Fatalf("view tool: %s", out)
	}
}

func TestHookVerbOfAMissingStoreExitsZero(t *testing.T) {
	data := t.TempDir()
	code, out, _ := cli(t, `{"hook_event_name":"SessionStart"}`, "hook", "--data-dir", data)
	if code != 0 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestContextVerbPrintsNothingForAnEmptyStore(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work")
	if code, out, _ := cli(t, "", "context", "--data-dir", data); code != 0 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
	seedStore(t, data, "home", "milk is low")
	if _, out, _ := cli(t, "", "context", "--store", "home", "--data-dir", data); !strings.Contains(out, "milk is low") {
		t.Fatalf("%q", out)
	}
}

// Round 7 fix: only the owner's chats enter the memory (ruling H2).

func skipCLI(t *testing.T, stdin string, env map[string]string, home string, argv ...string) (int, string) {
	t.Helper()
	var out, errw strings.Builder
	code := Run(argv, CLIEnv{Stdin: strings.NewReader(stdin), Out: &out, Err: &errw, Now: newClock().now, Home: home,
		Getenv: func(k string) string { return env[k] }})
	return code, out.String()
}

func TestAHookInASkippedProcessLogsNothingAndPrintsNoView(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "the owner said the blue door code is 4417")
	tr := filepath.Join(t.TempDir(), "worker.jsonl")
	writeTranscript(t, tr, liveLine("user", "w1", "2026-11-05T10:00:00Z", "worker only line"))
	stop := `{"hook_event_name":"Stop","transcript_path":` + fmt.Sprintf("%q", tr) + `}`
	start := `{"hook_event_name":"SessionStart"}`
	skip := map[string]string{"DAISUGI_PANDO_SKIP": "1"}
	// A summarizer call or a spawned worker: no view, no log.
	if code, out := skipCLI(t, start, skip, "", "hook", "--data-dir", data); code != 0 || out != "" {
		t.Fatalf("skipped session start: %d %q", code, out)
	}
	if code, out := skipCLI(t, stop, skip, "", "hook", "--data-dir", data); code != 0 || out != "" {
		t.Fatalf("skipped stop: %d %q", code, out)
	}
	if _, out := skipCLI(t, "", skip, "", "context", "--data-dir", data); out != "" {
		t.Fatalf("skipped context: %q", out)
	}
	if _, out, _ := cli(t, "", "log", "--data-dir", data); strings.Contains(out, "worker only line") {
		t.Fatalf("the worker was logged: %q", out)
	}
	// The owner's chat, in the same store, is logged and gets the view. The
	// worker's own session is on the skip list for good, so the owner's chat
	// is another transcript.
	otr := filepath.Join(t.TempDir(), "owner.jsonl")
	writeTranscript(t, otr, liveLine("user", "o1", "2026-11-05T10:00:00Z", "owner line"))
	ostop := `{"hook_event_name":"Stop","transcript_path":` + fmt.Sprintf("%q", otr) + `}`
	if code, out := skipCLI(t, ostop, map[string]string{}, "", "hook", "--data-dir", data); code != 0 || out != "" {
		t.Fatalf("owner stop: %d %q", code, out)
	}
	if _, out, _ := cli(t, "", "log", "--data-dir", data); !strings.Contains(out, "owner line") || strings.Contains(out, "worker only line") {
		t.Fatalf("the owner chat was not logged alone: %q", out)
	}
	if _, out := skipCLI(t, start, map[string]string{"DAISUGI_PANDO_SKIP": "0"}, "", "hook", "--data-dir", data); !strings.Contains(out, "4417") {
		t.Fatalf("the owner gets the view: %q", out)
	}
}

func TestATranscriptUnderTheCoppiceDataDirIsNotLogged(t *testing.T) {
	home := t.TempDir()
	worktree := filepath.Join(home, ".local", "state", "coppice", "chat", "work", "wt")
	line := func(cwd string) string {
		return fmt.Sprintf(`{"type":"user","uuid":"u-%s","sessionId":"s","cwd":%q,"timestamp":"2026-11-05T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello from %s"}]}}`,
			filepath.Base(cwd), cwd, filepath.Base(cwd))
	}
	for _, tc := range []struct {
		cwd    string
		logged bool
	}{{worktree, false}, {"/work/proj", true}} {
		dir := filepath.Join(t.TempDir(), "pando", "work")
		c, err := Open(context.Background(), Config{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		tr := filepath.Join(t.TempDir(), "s.jsonl")
		writeTranscript(t, tr, line(tc.cwd))
		n, err := c.IngestClaude(context.Background(), tr, SkipDirs(dir, home, func(string) string { return "" }))
		c.Close()
		if err != nil || (n == 1) != tc.logged {
			t.Fatalf("cwd %s: logged %d err %v", tc.cwd, n, err)
		}
	}
}

// A Claude Code pane steward runs in the steward dir. Its chat is the
// owner's, so follow logs it (workers there carry the marker and the list).
func TestAClaudeCodePaneStewardIsLogged(t *testing.T) {
	home := t.TempDir()
	stewardDir := filepath.Join(home, ".local", "state", "coppice-steward")
	dir := filepath.Join(t.TempDir(), "pando", "work")
	c, err := Open(context.Background(), Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	writeTranscript(t, tr, fmt.Sprintf(`{"type":"user","uuid":"u-st","sessionId":"s","cwd":%q,"timestamp":"2026-11-05T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello steward"}]}}`, stewardDir))
	n, err := c.IngestClaude(context.Background(), tr, SkipDirs(dir, home, func(string) string { return "" }))
	if err != nil || n != 1 {
		t.Fatalf("logged %d err %v, want the steward's line", n, err)
	}
}

func TestSkipDirsNameTheCoppiceDirs(t *testing.T) {
	env := map[string]string{"COPPICE_DATA_DIR": "/d/coppice", "XDG_STATE_HOME": "/s", "XDG_DATA_HOME": "/x"}
	got := strings.Join(SkipDirs("/data/pando/work", "/home/u", func(k string) string { return env[k] }), " ")
	for _, w := range []string{"/data/pando/work", "/d/coppice", "/s/coppice",
		"/data/coppice", "/x/opendaisugi/coppice", "/home/u/.opendaisugi/coppice"} {
		if !strings.Contains(got, w) {
			t.Fatalf("%q lacks %s", got, w)
		}
	}
	// With no XDG_DATA_HOME the default one is named.
	got = strings.Join(SkipDirs("/data/pando/work", "/home/u", func(string) string { return "" }), " ")
	if !strings.Contains(got, "/home/u/.local/share/opendaisugi/coppice") {
		t.Fatalf("%q lacks the default XDG data dir", got)
	}
}

// The pando MCP entry sits in the user's Claude Code config, so a worker or a
// model call of daisugi's own would start it too. It must give them nothing.
func TestTheMCPServerOfASkippedProcessListsNoToolAndAnswersNoCall(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "the blue door code is 4417")
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall","arguments":{"query":"door"}}}` + "\n"
	_, out := skipCLI(t, in, map[string]string{"DAISUGI_PANDO_SKIP": "1"}, "", "serve", "--data-dir", data)
	if strings.Contains(out, "4417") || strings.Contains(out, `"name":"recall"`) || !strings.Contains(out, `"tools":[]`) || !strings.Contains(out, ClosedText) {
		t.Fatalf("a skipped process got the memory:\n%s", out)
	}
	_, out = skipCLI(t, in, map[string]string{}, "", "serve", "--data-dir", data)
	if !strings.Contains(out, `"name":"recall"`) || !strings.Contains(out, "4417") {
		t.Fatalf("the owner's server lost its tools:\n%s", out)
	}
}
