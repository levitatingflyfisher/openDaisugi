package pando

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreDirRules(t *testing.T) {
	d := t.TempDir()
	got, err := StoreDir(d, "work")
	if err != nil || got != filepath.Join(d, "pando", "work") {
		t.Fatalf("work: %q %v", got, err)
	}
	if _, err := StoreDir(d, "all"); err != ErrStoreAll {
		t.Fatalf("all must be refused, got %v", err)
	}
	for _, bad := range []string{"", "Work", "1x", "a/b", "a b", "x-very-long-name-that-goes-past-thirty-two-chars"} {
		if _, err := StoreDir(d, bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
	if DefaultStore != "work" {
		t.Fatal("default store is work")
	}
}

func TestListStoresOnlyRealOnes(t *testing.T) {
	d := t.TempDir()
	mk := func(name string, withMain bool) {
		p := filepath.Join(d, "pando", name)
		if withMain {
			p = filepath.Join(p, "main")
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mk("work", true)
	mk("home", true)
	mk("junk", false)
	got, err := ListStores(d)
	if err != nil || len(got) != 2 || got[0] != "home" || got[1] != "work" {
		t.Fatalf("got %v %v", got, err)
	}
}

func seedStore(t *testing.T, data, store string, lines ...string) string {
	t.Helper()
	dir, err := StoreDir(data, store)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, l := range lines {
		if _, err := c.Append(ctx, KindUser, l); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func cli(t *testing.T, stdin string, argv ...string) (int, string, string) {
	t.Helper()
	var out, errw strings.Builder
	code := Run(argv, CLIEnv{Stdin: strings.NewReader(stdin), Out: &out, Err: &errw, Now: newClock().now})
	return code, out.String(), errw.String()
}

// Review focus 2: a reader does not take the lock and sees what a writer
// has fsynced.
func TestReadVerbsWorkWhileAWriterHoldsTheStore(t *testing.T) {
	data := t.TempDir()
	dir := seedStore(t, data, "work", "we chose sqlite for the index", "and potion stays optional")
	w, err := Open(ctx, Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Append(ctx, KindUser, "a third line, written live")
	code, out, errs := cli(t, "", "zoom", "0", "1", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "we chose sqlite") {
		t.Fatalf("zoom: %d %q %q", code, out, errs)
	}
	code, out, _ = cli(t, "", "date", "1", "--data-dir", data)
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), "Z") {
		t.Fatalf("date: %d %q", code, out)
	}
	code, out, _ = cli(t, "", "log", "--data-dir", data)
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(out, "a third line, written live") {
		t.Fatalf("log: %d %q", code, out)
	}
	code, _, errs = cli(t, "", "append", "x", "--data-dir", data)
	if code != 1 || !strings.Contains(errs, "another process has this pando store open") {
		t.Fatalf("a second writer: %d %q", code, errs)
	}
}

// Review focus 5.
func TestRefusesAllAndMissingHome(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "x")
	code, _, errs := cli(t, "", "status", "--store", "all", "--data-dir", data)
	if code != 2 || !strings.Contains(errs, "one store at a time: pando stores are separate on purpose.") {
		t.Fatalf("all: %d %q", code, errs)
	}
	code, _, errs = cli(t, "", "status", "--store", "home", "--data-dir", data)
	if code != 1 || !strings.Contains(errs, "no store home at "+filepath.Join(data, "pando", "home")) {
		t.Fatalf("home: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(data, "pando", "home")); err == nil {
		t.Fatal("a read verb made the store")
	}
	code, out, _ := cli(t, "", "append", "hello house", "--store", "home", "--data-dir", data)
	if code != 0 || out != "0\n" {
		t.Fatalf("append: %d %q", code, out)
	}
	st, err := os.Stat(filepath.Join(data, "pando", "home"))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("home store: %v %v", st, err)
	}
	// The two stores never meet.
	code, out, _ = cli(t, "", "recall", "sqlite", "--store", "home", "--data-dir", data)
	if code != 0 || out != "" {
		t.Fatalf("home must not see work: %d %q", code, out)
	}
	_, out, _ = cli(t, "", "stores", "--data-dir", data)
	if out != "home\nwork\n" {
		t.Fatalf("stores: %q", out)
	}
}

func TestRecallRanksByWords(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "the index is sqlite", "lunch was late", "sqlite index rebuilds from files")
	code, out, errs := cli(t, "", "recall", "sqlite index", "-k", "2", "--data-dir", data)
	if code != 0 {
		t.Fatal(errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || strings.Contains(out, "lunch") {
		t.Fatalf("recall must rank the two sqlite lines first: %q", out)
	}
	if !strings.HasPrefix(lines[0], "2+1\t") && !strings.HasPrefix(lines[0], "0+1\t") {
		t.Fatalf("a hit starts with a node id: %q", lines[0])
	}
}

func TestTendBuildsPendingWithAFake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	long := strings.Repeat("word ", 300)
	for i := 0; i < 4; i++ {
		if _, err := c.Append(ctx, KindUser, long); err != nil {
			t.Fatal(err)
		}
	}
	before := c.Pending()
	if before < 4 {
		t.Fatalf("pending %d", before)
	}
	fake := &fakeSummarizer{}
	built, err := Tend(ctx, c, fake, 2)
	if err != nil || fake.compress != 2 || c.Pending() >= before || built < 2 {
		t.Fatalf("built %d err %v compress %d pending %d", built, err, fake.compress, c.Pending())
	}
	if _, err := Tend(ctx, c, fake, 0); err != nil || c.Pending() != 0 {
		t.Fatalf("tend all: pending %d err %v", c.Pending(), err)
	}
	// A failure stops the loop and the node waits.
	if _, err := c.Append(ctx, KindUser, long); err != nil {
		t.Fatal(err)
	}
	if _, err := Tend(ctx, c, &fakeSummarizer{fail: true}, 0); err != errFake {
		t.Fatalf("want the fake's error, got %v", err)
	}
	if d, ok := c.RetryIn(); !ok || d <= 0 {
		t.Fatalf("the node must wait: %v %v", d, ok)
	}
}

func TestTendDryRunListsAndCallsNoModel(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", strings.Repeat("word ", 300))
	called := false
	var out, errw strings.Builder
	code := Run([]string{"tend", "--dry-run", "--data-dir", data}, CLIEnv{Out: &out, Err: &errw,
		NewSummarizer: func(string) (Summarizer, error) { called = true; return &fakeSummarizer{}, nil }})
	if code != 0 || called || !strings.Contains(out.String(), "pending: 1\nnot in the view yet: 1\n0+1\n") || !strings.Contains(out.String(), "nothing was changed") {
		t.Fatalf("dry run: %d %q %q", code, out.String(), errw.String())
	}
	out.Reset()
	code = Run([]string{"tend", "--data-dir", data}, CLIEnv{Out: &out, Err: &errw,
		NewSummarizer: func(string) (Summarizer, error) { return &fakeSummarizer{}, nil }})
	if code != 0 || !strings.Contains(out.String(), "built: 1, fell back to clipped: 0\npending: 0\n") {
		t.Fatalf("tend: %d %q %q", code, out.String(), errw.String())
	}
}

func rpc(t *testing.T, c *Chat, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := ServeMCP(ctx, c, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		res = append(res, m)
	}
	return res
}

func TestMCPListsFourReadToolsAndZooms(t *testing.T) {
	dir := seedStore(t, t.TempDir(), "work", "the warden denies by default", "sqlite holds the index")
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := rpc(t, c,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"spawn","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"recall","arguments":{"query":"sqlite"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"zoom","arguments":{"id":9,"n":1}}}`,
	)
	if got := res[0]["result"].(map[string]any)["serverInfo"].(map[string]any)["name"]; got != "pando" {
		t.Fatalf("server name %v", got)
	}
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("want zoom, date, recall, view; got %d", len(tools))
	}
	if b, _ := json.Marshal(res[2]); !strings.Contains(string(b), "warden denies") {
		t.Fatalf("zoom: %v", res[2])
	}
	if res[3]["error"] == nil {
		t.Fatal("spawn must be unknown here")
	}
	if b, _ := json.Marshal(res[4]); !strings.Contains(string(b), "1+1") {
		t.Fatalf("recall: %v", res[4])
	}
	if res[5]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("a bad id is a tool error: %v", res[5])
	}
}

func TestMigrateCopiesChecksRenamesNeverDeletes(t *testing.T) {
	old := filepath.Join(t.TempDir(), "foreman")
	c, err := Open(ctx, Config{Dir: old, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	c.Append(ctx, KindUser, "keep me")
	c.Close()
	os.WriteFile(filepath.Join(old, "turns.jsonl"), []byte("{}\n"), 0o600)

	data := t.TempDir()
	rep, err := Migrate(old, data, "work", time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 1 || !strings.HasSuffix(rep.MovedTo, "foreman.moved-20261030T120000Z") {
		t.Fatalf("%+v", rep)
	}
	if len(rep.Kept) != 1 || rep.Kept[0] != "turns.jsonl" {
		t.Fatalf("kept %v", rep.Kept)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old dir must be renamed, not left")
	}
	if _, err := os.Stat(filepath.Join(rep.MovedTo, "main")); err != nil {
		t.Fatal("the old log must stay inside the moved dir")
	}
	dir, _ := StoreDir(data, "work")
	r, err := OpenReadOnly(dir)
	if err != nil || r.Count() != 1 {
		t.Fatalf("copy: %v", err)
	}
	// A second run says it is done and changes nothing.
	again, err := Migrate(old, data, "work", time.Now())
	if err != nil || !again.Already || again.MovedTo != rep.MovedTo {
		t.Fatalf("second run: %+v %v", again, err)
	}
}

// Review focus 3.
func TestMigrateRefusesWhileOpenAndCopiesNothing(t *testing.T) {
	old := filepath.Join(t.TempDir(), "foreman")
	c, _ := Open(ctx, Config{Dir: old})
	c.Append(ctx, KindUser, "x")
	defer c.Close()
	data := t.TempDir()
	_, err := Migrate(old, data, "work", time.Now())
	if err != ErrOpenElsewhere {
		t.Fatalf("want ErrOpenElsewhere, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "pando")); err == nil {
		t.Fatal("nothing may be copied")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("the old dir must stay")
	}
}

func TestMigrateRefusesAnExistingStore(t *testing.T) {
	old := filepath.Join(t.TempDir(), "foreman")
	c, _ := Open(ctx, Config{Dir: old})
	c.Append(ctx, KindUser, "x")
	c.Close()
	data := t.TempDir()
	seedStore(t, data, "work", "already here")
	if _, err := Migrate(old, data, "work", time.Now()); err != ErrTargetExists {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("the old dir must stay")
	}
}

const webFixture = "../../fixtures/pando/transcripts/web/conversations.json"

func TestScanWebKeysByConversationAndMessage(t *testing.T) {
	ss, err := ScanWeb(webFixture, ImportOpts{From: "web"}, func(string) bool { return false })
	if err != nil || len(ss) != 3 {
		t.Fatalf("sessions %d err %v", len(ss), err)
	}
	by := map[string]ImportSession{}
	for _, s := range ss {
		by[s.Key] = s
	}
	a, b := by["web:c0000000-0000-4000-8000-000000000001"], by["web:c0000000-0000-4000-8000-000000000002"]
	// The fixture repeats message uuid m1 in both chats. Each chat keeps it.
	if a.New+b.New != 5 {
		t.Fatalf("want 5 new messages across both chats, got %d and %d", a.New, b.New)
	}
	seen := func(act string) bool { return act == a.Key+":m1" }
	ss2, _ := ScanWeb(webFixture, ImportOpts{From: "web"}, seen)
	for _, s := range ss2 {
		if s.Key == a.Key && s.New != a.New-1 {
			t.Fatal("a message already logged by its act must not count as new")
		}
		if s.Key == b.Key && s.New != b.New {
			t.Fatal("the same message uuid in another chat is a different message")
		}
	}
	var prop bytes.Buffer
	if err := WriteProposal(&prop, ss); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prop.String(), "\twork\t3 messages to import\tGarden bed plan") || !strings.Contains(prop.String(), "\tskip\tthe chat is empty\tBlank") {
		t.Fatalf("proposal:\n%s", prop.String())
	}
}

func TestParseMapAndApplyToProjectOnly(t *testing.T) {
	m, err := ParseMap("/home/old=/home/new")
	if err != nil || m.Old != "/home/old" || m.New != "/home/new" {
		t.Fatal(m, err)
	}
	if _, err := ParseMap("nope"); err == nil {
		t.Fatal("a map needs =")
	}
	o := ImportOpts{Map: []PathMap{m}}
	if got := o.project("/home/old/repo"); got != "/home/new/repo" {
		t.Fatalf("project %q", got)
	}
	if got := o.project("/home/old"); got != "/home/new" {
		t.Fatalf("project %q", got)
	}
	if got := o.project("/home/olden/x"); got != "/home/olden/x" {
		t.Fatalf("a prefix match is by path element: %q", got)
	}
}

func TestProposalAcceptRefusesAllAndImportsToTheNamedStore(t *testing.T) {
	data := t.TempDir()
	prop := filepath.Join(t.TempDir(), "p.tsv")
	var out, errw strings.Builder
	env := CLIEnv{Out: &out, Err: &errw, Now: newClock().now}
	if code := Run([]string{"import", "--from", "web", "--web", webFixture, "--propose", prop, "--data-dir", data}, env); code != 0 {
		t.Fatalf("propose: %d %s", code, errw.String())
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Fatalf("propose wrote %v", ents)
	}
	b, _ := os.ReadFile(prop)
	edited := strings.Replace(string(b), "\twork\t2 messages to import\tBirthday list", "\thome\t2 messages to import\tBirthday list", 1)
	os.WriteFile(prop, []byte(edited), 0o600)
	out.Reset()
	if code := Run([]string{"import", "--from", "web", "--web", webFixture, "--accept", prop, "--data-dir", data}, env); code != 0 {
		t.Fatalf("accept: %d %s", code, errw.String())
	}
	w, _ := StoreDir(data, "work")
	h, _ := StoreDir(data, "home")
	wc, err1 := OpenReadOnly(w)
	hc, err2 := OpenReadOnly(h)
	if err1 != nil || err2 != nil || wc.Count() != 3 || hc.Count() != 2 {
		t.Fatalf("work %v home %v (%v %v)", wc, hc, err1, err2)
	}
	m, _ := hc.Message(0)
	if m.Kind != KindPast || !strings.Contains(m.Text, "boats") || strings.Contains(m.Text, "carrots") {
		t.Fatalf("home message: %+v", m)
	}
	// Run again: every act is in the log, so nothing is logged twice.
	if code := Run([]string{"import", "--from", "web", "--web", webFixture, "--accept", prop, "--data-dir", data}, env); code != 0 {
		t.Fatalf("accept again: %d %s", code, errw.String())
	}
	wc, _ = OpenReadOnly(w)
	if wc.Count() != 3 {
		t.Fatalf("logged twice: %d", wc.Count())
	}
	bad := filepath.Join(t.TempDir(), "bad.tsv")
	os.WriteFile(bad, []byte("web:c0000000-0000-4000-8000-000000000001\tall\tx\ty\n"), 0o600)
	errw.Reset()
	if code := Run([]string{"import", "--from", "web", "--web", webFixture, "--accept", bad, "--data-dir", data}, env); code != 2 || !strings.Contains(errw.String(), "one store at a time") {
		t.Fatalf("all: %d %q", code, errw.String())
	}
}

func TestChildStoreIsSeparateAndTended(t *testing.T) {
	data := t.TempDir()
	dir := seedStore(t, data, "work", "parent line")
	if _, err := ChildDir(dir, "../x"); err == nil {
		t.Fatal("a label cannot climb out")
	}
	ch, err := OpenChild(ctx, dir, "w1", Config{Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("step ", 200)
	for i := 0; i < 3; i++ {
		if _, err := ch.Append(ctx, KindTool, long); err != nil {
			t.Fatal(err)
		}
	}
	roots := ch.ChildRoots("w1")
	ch.Close()
	if len(roots) != 2 || roots[0] != "w1:0+2" {
		t.Fatalf("roots %v", roots)
	}
	p, _ := OpenReadOnly(dir)
	if p.Count() != 1 {
		t.Fatalf("the parent log must not hold the child: %d", p.Count())
	}
	n, err := TendChildren(ctx, dir, &fakeSummarizer{}, 0, newClock().now)
	if err != nil || n < 3 {
		t.Fatalf("tend children: %d %v", n, err)
	}
	cd, _ := ChildDir(dir, "w1")
	c, _ := OpenReadOnly(cd)
	if c.Pending() != 0 {
		t.Fatalf("pending %d", c.Pending())
	}
	// Every message of the child is in its view: none is out of sight.
	if len(c.ViewEntries()) == 0 {
		t.Fatal("empty view")
	}
	code, out, _ := cli(t, "", "zoom", "0", "1", "--child", "w1", "--data-dir", data)
	if code != 0 || !strings.HasPrefix(out, "step step") {
		t.Fatalf("child zoom: %d %q", code, out)
	}
	code, out, _ = cli(t, "", "zoom", "0", "1", "--data-dir", data)
	if code != 0 || out != "parent line\n" {
		t.Fatalf("parent zoom must not read the child: %d %q", code, out)
	}
}

func TestImportTendBuildsTheNodesAtOnceAndNeedsAModelFirst(t *testing.T) {
	data := t.TempDir()
	prop := filepath.Join(t.TempDir(), "p.tsv")
	var out, errw strings.Builder
	env := CLIEnv{Out: &out, Err: &errw, Now: newClock().now}
	if code := Run([]string{"import", "--from", "web", "--web", webFixture, "--propose", prop, "--data-dir", data}, env); code != 0 {
		t.Fatalf("propose: %d %s", code, errw.String())
	}
	// No model: the import is refused before it writes anything.
	accept := []string{"import", "--from", "web", "--web", webFixture, "--accept", prop, "--tend", "--data-dir", data}
	if code := Run(accept, env); code != 1 || !strings.Contains(errw.String(), "no model is set up for tend") {
		t.Fatalf("no model: %d %q", code, errw.String())
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Fatalf("a refused import wrote %v", ents)
	}
	var profile string
	env.NewSummarizer = func(p string) (Summarizer, error) { profile = p; return &fakeSummarizer{}, nil }
	out.Reset()
	if code := Run(append(accept, "--profile", "desk"), env); code != 0 {
		t.Fatalf("tend: %d %s", code, errw.String())
	}
	if profile != "desk" || !strings.Contains(out.String(), "logged: 5 messages\nbuilt: ") || !strings.HasSuffix(out.String(), "pending: 0\n") {
		t.Fatalf("profile %q output %q", profile, out.String())
	}
	w, _ := StoreDir(data, "work")
	c, err := OpenReadOnly(w)
	if err != nil || c.Pending() != 0 {
		t.Fatalf("pending %v %v", c, err)
	}
	// Without --tend the nodes stay pending, as before.
	data2 := t.TempDir()
	plain := []string{"import", "--from", "web", "--web", webFixture, "--accept", prop, "--data-dir", data2}
	out.Reset()
	if code := Run(plain, env); code != 0 || !strings.Contains(out.String(), "Run: daisugi pando tend") {
		t.Fatalf("plain: %d %q", code, out.String())
	}
}
