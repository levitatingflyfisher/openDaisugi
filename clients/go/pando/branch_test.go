package pando

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// branchFixture lays out a store with root messages, two branches and a
// branch below one of them:
//
//	work            "the household budget sheet lives in the attic"
//	  trellis       "trellis reader uses the epub parser"
//	    ui          "the ui pane uses a lattice icon"
//	  other         "the other project keeps a secret ledger"
func branchFixture(t *testing.T) (data, store string) {
	t.Helper()
	data = t.TempDir()
	store = seedStore(t, data, "work", "the household budget sheet lives in the attic")
	put := func(path, text string) {
		d, err := EnsureBranch(store, path, newClock().now)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Open(ctx, Config{Dir: d, Now: newClock().now})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Append(ctx, KindUser, text); err != nil {
			t.Fatal(err)
		}
	}
	put("trellis", "trellis reader uses the epub parser")
	put("trellis/ui", "the ui pane uses a lattice icon")
	put("other", "the other project keeps a secret ledger")
	return data, store
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func cliEnv(t *testing.T, stdin string, vars map[string]string, argv ...string) (int, string, string) {
	t.Helper()
	var out, errw strings.Builder
	code := Run(argv, CLIEnv{Stdin: strings.NewReader(stdin), Out: &out, Err: &errw, Now: newClock().now, Getenv: envOf(vars)})
	return code, out.String(), errw.String()
}

func mcpCall(name string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

func serveOnce(t *testing.T, data string, vars map[string]string, lines ...string) string {
	t.Helper()
	_, out, errs := cliEnv(t, strings.Join(lines, "\n")+"\n", vars, "serve", "--data-dir", data)
	if errs != "" {
		t.Logf("stderr: %s", errs)
	}
	return out
}

func TestParseBranchRefusesEscapes(t *testing.T) {
	for _, bad := range []string{"", "/abs", "/", "..", "a/..", "a/../b", ".", "a/./b", "a//b", "a/", "/a", "a b", "a\\b",
		"a/b/c/d/e/f/g/h/i"} {
		if _, err := ParseBranch(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	for _, good := range []string{"trellis", "trellis/ui", "a.b-c_d/E9"} {
		if _, err := ParseBranch(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

func TestBranchDirRefusesALinkAtEveryLevel(t *testing.T) {
	data, store := branchFixture(t)
	_ = data
	// A link as a branch: branches/evil -> the store itself.
	if err := os.Symlink(store, filepath.Join(store, BranchesDir, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := BranchDir(store, "evil"); err == nil {
		t.Fatal("a branch that is a link was taken")
	}
	// A link one level down.
	if err := os.Symlink(filepath.Join(store, BranchesDir, "other"), filepath.Join(store, BranchesDir, "trellis", BranchesDir, "sib")); err != nil {
		t.Fatal(err)
	}
	if _, err := BranchDir(store, "trellis/sib"); err == nil {
		t.Fatal("a nested branch that is a link was taken")
	}
	// The branches dir itself as a link.
	ui := filepath.Join(store, BranchesDir, "trellis", BranchesDir, "ui")
	if err := os.Rename(filepath.Join(store, BranchesDir, "trellis", BranchesDir), filepath.Join(store, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "moved"), filepath.Join(store, BranchesDir, "trellis", BranchesDir)); err != nil {
		t.Fatal(err)
	}
	_ = ui
	if _, err := BranchDir(store, "trellis/ui"); err == nil {
		t.Fatal("a branches dir that is a link was taken")
	}
}

func TestScopeFailsClosed(t *testing.T) {
	_, store := branchFixture(t)
	if sc := ScopeOf(store, envOf(nil)); sc.Closed || sc.Dir != store {
		t.Fatalf("no marker: %+v", sc)
	}
	if sc := ScopeOf(store, envOf(map[string]string{SkipVar: "1"})); !sc.Closed {
		t.Fatalf("a worker with no branch was open: %+v", sc)
	}
	for _, bad := range []string{"..", "../work", "/etc", "nope", "trellis/../other", "trellis/nope"} {
		sc := ScopeOf(store, envOf(map[string]string{BranchVar: bad}))
		if !sc.Closed || sc.Dir == store {
			t.Errorf("marker %q: %+v", bad, sc)
		}
	}
	sc := ScopeOf(store, envOf(map[string]string{BranchVar: "trellis", SkipVar: "1"}))
	if sc.Closed || !sc.Worker || sc.Dir != filepath.Join(store, BranchesDir, "trellis") {
		t.Fatalf("a worker of trellis: %+v", sc)
	}
}

func TestBranchServeSeesItsBranchAndBelowOnly(t *testing.T) {
	data, _ := branchFixture(t)
	lead := map[string]string{BranchVar: "trellis"}
	out := serveOnce(t, data, lead,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		mcpCall("recall", map[string]any{"query": "household budget sheet"}),
		mcpCall("recall", map[string]any{"query": "secret ledger"}),
		mcpCall("recall", map[string]any{"query": "epub parser lattice icon", "k": 8}),
		mcpCall("view", map[string]any{}),
	)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines: %s", len(lines), out)
	}
	if !strings.Contains(lines[0], `"view"`) || !strings.Contains(lines[0], `"branch"`) {
		t.Fatalf("tools: %s", lines[0])
	}
	if strings.Contains(lines[1], "budget") || strings.Contains(lines[2], "ledger") {
		t.Fatalf("a branch saw the store above or a sibling: %s / %s", lines[1], lines[2])
	}
	if !strings.Contains(lines[3], `0+1\t`) || !strings.Contains(lines[3], `ui:0+1\t`) {
		t.Fatalf("recall of the branch and below: %s", lines[3])
	}
	if !strings.Contains(lines[4], "epub parser") || strings.Contains(lines[4], "budget") ||
		!strings.Contains(lines[4], "- ui: ") || strings.Contains(lines[4], "- other") {
		t.Fatalf("view: %s", lines[4])
	}
}

func TestBranchToolsRefuseEveryEscape(t *testing.T) {
	data, store := branchFixture(t)
	if err := os.Symlink(filepath.Join(store, BranchesDir, "other"), filepath.Join(store, BranchesDir, "trellis", BranchesDir, "sib")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, BranchesDir, "trellis", ChildrenDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, BranchesDir, "other"), filepath.Join(store, BranchesDir, "trellis", ChildrenDir, "w-9")); err != nil {
		t.Fatal(err)
	}
	lead := map[string]string{BranchVar: "trellis"}
	calls := []map[string]any{
		{"id": 0, "n": 1, "branch": ".."},
		{"id": 0, "n": 1, "branch": "../other"},
		{"id": 0, "n": 1, "branch": "/" + filepath.Join(store, BranchesDir, "other")},
		{"id": 0, "n": 1, "branch": "other"},
		{"id": 0, "n": 1, "branch": "sib"},
		{"id": 0, "n": 1, "branch": "ui/../../other"},
		{"id": 0, "n": 1, "child": "../../other"},
		{"id": 0, "n": 1, "child": "w-9"},
		{"id": 0, "n": 1, "child": "/" + store},
	}
	var lines []string
	for _, a := range calls {
		lines = append(lines, mcpCall("zoom", a))
	}
	lines = append(lines, mcpCall("date", map[string]any{"id": 0, "branch": ".."}))
	out := serveOnce(t, data, lead, lines...)
	got := strings.Split(strings.TrimSpace(out), "\n")
	if len(got) != len(calls)+1 {
		t.Fatalf("got %d answers: %s", len(got), out)
	}
	for i, l := range got {
		if !strings.Contains(l, `"isError":true`) || strings.Contains(l, "ledger") || strings.Contains(l, "budget") {
			t.Errorf("call %d was not refused: %s", i, l)
		}
	}
	// And the good ones read.
	out = serveOnce(t, data, lead, mcpCall("zoom", map[string]any{"id": 0, "n": 1}),
		mcpCall("zoom", map[string]any{"id": 0, "n": 1, "branch": "ui"}))
	if !strings.Contains(out, "epub parser") || !strings.Contains(out, "lattice icon") {
		t.Fatalf("the branch and below: %s", out)
	}
}

func TestBadMarkerServesClosedNeverTheStore(t *testing.T) {
	data, store := branchFixture(t)
	if err := os.Symlink(store, filepath.Join(store, BranchesDir, "loop")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"..", "/tmp", "nope", "loop", "trellis/../other"} {
		out := serveOnce(t, data, map[string]string{BranchVar: bad},
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			mcpCall("recall", map[string]any{"query": "household budget sheet"}))
		if !strings.Contains(out, `"tools":[]`) || strings.Contains(out, "budget") {
			t.Errorf("marker %q: %s", bad, out)
		}
		code, text, _ := cliEnv(t, "", map[string]string{BranchVar: bad}, "context", "--data-dir", data)
		if code != 0 || text != "" {
			t.Errorf("context with marker %q: %d %q", bad, code, text)
		}
		code, text, _ = cliEnv(t, "", map[string]string{BranchVar: bad}, "recall", "budget", "--data-dir", data)
		if code == 0 || strings.Contains(text, "budget") {
			t.Errorf("recall with marker %q: %d %q", bad, code, text)
		}
	}
}

func TestBranchWorkerGetsSearchAndZoomButNoView(t *testing.T) {
	data, _ := branchFixture(t)
	w := map[string]string{BranchVar: "trellis", SkipVar: "1"}
	out := serveOnce(t, data, w,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		mcpCall("recall", map[string]any{"query": "epub parser"}),
		mcpCall("view", map[string]any{}),
		mcpCall("recall", map[string]any{"query": "household budget sheet"}))
	got := strings.Split(strings.TrimSpace(out), "\n")
	if len(got) != 4 || strings.Contains(got[0], `"view"`) || !strings.Contains(got[1], "epub") ||
		!strings.Contains(got[2], `"error"`) || strings.Contains(got[3], "budget") {
		t.Fatalf("worker: %s", out)
	}
	code, text, _ := cliEnv(t, "", w, "context", "--data-dir", data)
	if code != 0 || text != "" {
		t.Fatalf("a worker got a view: %d %q", code, text)
	}
	// Its hooks log nothing and push nothing.
	code, text, _ = cliEnv(t, `{"hook_event_name":"SessionStart","session_id":"w1"}`, w, "hook", "--data-dir", data)
	if code != 0 || text != "" {
		t.Fatalf("a worker's SessionStart: %d %q", code, text)
	}
}

func TestRootRecallSeesEveryBranchWithItsLabel(t *testing.T) {
	data, _ := branchFixture(t)
	code, out, errs := cliEnv(t, "", nil, "recall", "secret ledger epub lattice budget", "-k", "8", "--data-dir", data)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	for _, want := range []string{"0+1\t", "trellis:0+1\t", "trellis/ui:0+1\t", "other:0+1\t"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %s", want, out)
		}
	}
	// Then zoom follows the label down.
	code, out, _ = cliEnv(t, "", nil, "zoom", "0", "1", "--branch", "trellis/ui", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "lattice icon") {
		t.Fatalf("zoom into a branch: %d %s", code, out)
	}
	// The steward's tools over the root scope.
	sc := Scope{Store: filepath.Join(data, "pando", "work"), Dir: filepath.Join(data, "pando", "work")}
	text, err := ToolZoomScope(sc, map[string]any{"id": float64(0), "n": float64(1), "branch": "other"})
	if err != nil || !strings.Contains(text, "ledger") {
		t.Fatalf("root zoom into other: %q %v", text, err)
	}
}

func TestRootViewNamesLiveBranchesAndTheFastReadAgrees(t *testing.T) {
	data, store := branchFixture(t)
	if err := CloseBranch(store, "other", newClock().now); err != nil {
		t.Fatal(err)
	}
	text, err := ViewContextOf(store)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, BranchLinesHead) || !strings.Contains(text, "- trellis: trellis reader uses the epub parser") ||
		strings.Contains(text, "- other:") || strings.Contains(text, "- ui:") {
		t.Fatalf("root view: %s", text)
	}
	c, err := OpenReadOnly(store)
	if err != nil {
		t.Fatal(err)
	}
	if c.ViewContext() != text {
		t.Fatalf("the fast read and the full read differ:\n%s\n---\n%s", c.ViewContext(), text)
	}
	// A closed branch stays searchable from above.
	_, out, _ := cliEnv(t, "", nil, "recall", "secret ledger", "--data-dir", data)
	if !strings.Contains(out, "other:0+1") {
		t.Fatalf("closed branch not searched: %s", out)
	}
	_, out, _ = cliEnv(t, "", nil, "branches", "--data-dir", data)
	if !strings.Contains(out, "other\tclosed\t1\t") || !strings.Contains(out, "trellis/ui\topen\t1\t") {
		t.Fatalf("branches: %s", out)
	}
}

func TestNestedBranchSeesBelowNotAbove(t *testing.T) {
	data, _ := branchFixture(t)
	ui := map[string]string{BranchVar: "trellis/ui"}
	_, out, _ := cliEnv(t, "", ui, "recall", "epub parser lattice icon budget ledger", "--data-dir", data)
	if !strings.Contains(out, "lattice") || strings.Contains(out, "epub") || strings.Contains(out, "budget") || strings.Contains(out, "ledger") {
		t.Fatalf("nested recall: %s", out)
	}
	_, out, _ = cliEnv(t, "", map[string]string{BranchVar: "trellis"}, "context", "--data-dir", data)
	if !strings.Contains(out, "the branch trellis") || !strings.Contains(out, "- ui: the ui pane") {
		t.Fatalf("lead view: %s", out)
	}
}

func TestBranchProcessCannotWriteByHand(t *testing.T) {
	data, _ := branchFixture(t)
	for _, verb := range [][]string{{"append", "x"}, {"tend"}, {"stores"}, {"close", "trellis"}, {"import"}, {"migrate"}} {
		code, _, errs := cliEnv(t, "", map[string]string{BranchVar: "trellis"}, append(verb, "--data-dir", data)...)
		if code != 1 || !strings.Contains(errs, "a branch of the memory") {
			t.Errorf("%v: %d %s", verb, code, errs)
		}
	}
}

func claudeTranscript(t *testing.T, dir, sid string, lines ...[2]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	var b strings.Builder
	for k, l := range lines {
		b.WriteString(strings.Replace(liveLine(l[0], sid+"-"+string(rune('a'+k)), "2026-11-05T10:00:00Z", l[1]), `"sess1"`, `"`+sid+`"`, 1) + "\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func messagesOf(t *testing.T, dir string) string {
	t.Helper()
	if !hasMain(dir) {
		return ""
	}
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := int64(0); i < c.Count(); i++ {
		m, _ := c.Message(i)
		b.WriteString(m.Text + "\n")
	}
	return b.String()
}

func TestBranchHookLogsIntoTheBranchNeverTheStore(t *testing.T) {
	data, store := branchFixture(t)
	proj := filepath.Join(t.TempDir(), "projects", "-p")
	tr := claudeTranscript(t, proj, "lead1", [2]string{"user", "lead brief: build the reader"}, [2]string{"assistant", "reader plan ready"})
	lead := map[string]string{BranchVar: "trellis"}
	ev := func(name string) string {
		b, _ := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "lead1", "transcript_path": tr})
		return string(b)
	}
	code, out, _ := cliEnv(t, ev("SessionStart"), lead, "hook", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "epub parser") || strings.Contains(out, "budget") {
		t.Fatalf("SessionStart view: %d %s", code, out)
	}
	if LoadRoutes(store).For("lead1", "") != "trellis" {
		t.Fatal("the session is not on the map")
	}
	cliEnv(t, ev("Stop"), lead, "hook", "--data-dir", data)
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); !strings.Contains(got, "reader plan ready") {
		t.Fatalf("branch log: %s", got)
	}
	if got := messagesOf(t, store); strings.Contains(got, "reader") {
		t.Fatalf("the store above got the branch chat: %s", got)
	}
	// The owner's own hook (no marker) on a mapped session routes it too.
	tr2 := claudeTranscript(t, proj, "lead1", [2]string{"user", "lead brief: build the reader"}, [2]string{"assistant", "reader plan ready"},
		[2]string{"user", "resumed from a plain terminal"})
	b, _ := json.Marshal(map[string]string{"hook_event_name": "Stop", "session_id": "lead1", "transcript_path": tr2})
	cliEnv(t, string(b), nil, "hook", "--data-dir", data)
	if got := messagesOf(t, store); strings.Contains(got, "resumed") {
		t.Fatalf("a mapped session reached the store: %s", got)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); !strings.Contains(got, "resumed") {
		t.Fatalf("branch log: %s", got)
	}
}

func TestFollowRoutesAMappedSessionFromItsFirstLine(t *testing.T) {
	data, store := branchFixture(t)
	_ = data
	proj := filepath.Join(t.TempDir(), "projects", "-p")
	// coppice mints the id and maps it before the lead starts.
	sid, err := NewBranchSession(store, "trellis/ui")
	if err != nil {
		t.Fatal(err)
	}
	claudeTranscript(t, proj, sid, [2]string{"user", "first words of the ui lead"})
	claudeTranscript(t, proj, "owner1", [2]string{"user", "the owner speaks at the root"})
	o := &FollowOpts{Dir: store, Log: &bytes.Buffer{}, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}},
		Now: func() time.Time { return time.Now() }}
	if _, _, _, err := FollowOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	root := messagesOf(t, store)
	if strings.Contains(root, "ui lead") || !strings.Contains(root, "owner speaks") {
		t.Fatalf("root: %s", root)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis", BranchesDir, "ui")); !strings.Contains(got, "first words of the ui lead") {
		t.Fatalf("ui branch: %s", got)
	}
}

func TestFollowTendsBranchNodesInTheBackground(t *testing.T) {
	_, store := branchFixture(t)
	fs := &fakeSummarizer{}
	o := &FollowOpts{Dir: store, Log: &bytes.Buffer{}, Summarizer: fs}
	for i := 0; i < 20; i++ {
		if _, _, more, err := FollowOnce(context.Background(), o); err != nil {
			t.Fatal(err)
		} else if !more {
			break
		}
	}
	for _, p := range []string{"trellis", "trellis/ui", "other"} {
		d, _ := ExistingBranch(store, p)
		c, _ := OpenReadOnly(d)
		if c.Pending() != 0 {
			t.Errorf("%s: %d pending", p, c.Pending())
		}
	}
}

func TestTendCoversBranchesAndTheirWorkers(t *testing.T) {
	data, store := branchFixture(t)
	for _, p := range []string{"", "trellis", "trellis/ui", "other"} {
		d := store
		if p != "" {
			d, _ = ExistingBranch(store, p)
		}
		c, err := Open(ctx, Config{Dir: d, Now: newClock().now})
		if err != nil {
			t.Fatal(err)
		}
		c.Append(ctx, KindUser, bigText(300))
		c.Close()
	}
	d, _ := ExistingBranch(store, "trellis")
	w, err := OpenChild(ctx, d, "trellis-1", Config{Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	w.Append(ctx, KindUser, bigText(300))
	w.Close()
	_, out, _ := cliEnv(t, "", nil, "tend", "--dry-run", "--data-dir", data)
	for _, want := range []string{"branches/trellis/1+1", "branches/trellis/children/trellis-1/0+1", "branches/trellis/branches/ui/1+1", "branches/other/1+1"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %s", want, out)
		}
	}
	n, err := TendChildren(ctx, store, &fakeSummarizer{}, 0, newClock().now)
	if err != nil || n < 4 {
		t.Fatalf("built %d %v", n, err)
	}
}

func TestImportSkipsAMappedSession(t *testing.T) {
	data, store := branchFixture(t)
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-p")
	claudeTranscript(t, proj, "lead9", [2]string{"user", "a lead's words"})
	if err := AddRoute(store, "trellis", "lead9"); err != nil {
		t.Fatal(err)
	}
	var out, errw strings.Builder
	Run([]string{"import", "--from", "claude", "--data-dir", data}, CLIEnv{Stdin: strings.NewReader(""), Out: &out, Err: &errw,
		Now: newClock().now, Home: home})
	if got := messagesOf(t, store); strings.Contains(got, "lead's words") {
		t.Fatalf("import took a branch chat into the store: %s %s", got, errw.String())
	}
}

func TestContextWithASessionMapsAHermesChat(t *testing.T) {
	data, store := branchFixture(t)
	code, out, _ := cliEnv(t, "", map[string]string{BranchVar: "trellis"}, "context", "--session", "h-77", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "epub parser") {
		t.Fatalf("%d %s", code, out)
	}
	if LoadRoutes(store)["hermes:h-77"] != "trellis" {
		t.Fatal("the Hermes session is not on the map")
	}
	// The first line for an id wins: a later claim does not move it.
	if err := AddRoute(store, "other", "hermes:h-77"); err != nil {
		t.Fatal(err)
	}
	if LoadRoutes(store)["hermes:h-77"] != "trellis" {
		t.Fatal("a second line moved the session")
	}
}

func TestRouterAppendsToTheMappedBranch(t *testing.T) {
	_, store := branchFixture(t)
	if err := AddRoute(store, "other", "hermes:s5"); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, Config{Dir: store, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRouter(c)
	target, err := rt.Chat(ctx, rt.Branch("hermes:s5", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.AppendLive(ctx, Live{Kind: KindUser, Text: "hermes row of a lead", Act: "hermes:p:s5:1", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rt.Close()
	c.Close()
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "other")); !strings.Contains(got, "hermes row of a lead") {
		t.Fatalf("other: %s", got)
	}
	if got := messagesOf(t, store); strings.Contains(got, "hermes row") {
		t.Fatalf("root: %s", got)
	}
}
