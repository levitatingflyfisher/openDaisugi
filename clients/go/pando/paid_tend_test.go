package pando

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedMany logs n long messages, which leaves more than n nodes to build.
func seedMany(t *testing.T, data string, n int) {
	t.Helper()
	long := strings.Repeat("word ", 300)
	lines := make([]string, n)
	for i := range lines {
		lines[i] = long
	}
	seedStore(t, data, "work", lines...)
}

// tendEnv counts the summarizers asked for and keeps one fake, so a test can
// count the model calls.
func tendEnv(calls *int, profile *string, out, errw *strings.Builder) (CLIEnv, *fakeSummarizer) {
	fake := &fakeSummarizer{}
	return CLIEnv{Out: out, Err: errw, Now: newClock().now,
		NewSummarizer: func(p string) (Summarizer, error) {
			*calls++
			*profile = p
			return fake, nil
		}}, fake
}

func TestTendPrintsTheNodeCountFirst(t *testing.T) {
	data := t.TempDir()
	seedMany(t, data, 3)
	var out, errw strings.Builder
	calls, profile := 0, ""
	env, fake := tendEnv(&calls, &profile, &out, &errw)
	_ = fake
	if code := Run([]string{"tend", "--dry-run", "--data-dir", data}, env); code != 0 || !strings.HasPrefix(out.String(), "nodes to build: ") {
		t.Fatalf("dry run: %d %q", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"tend", "--data-dir", data}, env); code != 0 || !strings.HasPrefix(out.String(), "nodes to build: ") {
		t.Fatalf("tend: %d %q %q", code, out.String(), errw.String())
	}
}

func TestTendRefusesTheDefaultModelOverTheLimit(t *testing.T) {
	data := t.TempDir()
	seedMany(t, data, 30)
	var out, errw strings.Builder
	calls, profile := 0, ""
	env, fake := tendEnv(&calls, &profile, &out, &errw)
	_ = fake
	code := Run([]string{"tend", "--data-dir", data}, env)
	if code != 1 || calls != 0 {
		t.Fatalf("no profile, many nodes: code %d, model built %d times", code, calls)
	}
	if !strings.HasPrefix(out.String(), "nodes to build: ") ||
		!strings.Contains(errw.String(), "Claude Code (paid)") ||
		!strings.Contains(errw.String(), "--profile NAME") || !strings.Contains(errw.String(), "--yes") {
		t.Fatalf("the refusal must name the count, the cost and both ways: %q / %q", out.String(), errw.String())
	}
	w, _ := StoreDir(data, "work")
	if c, err := OpenReadOnly(w); err != nil || c.Pending() == 0 {
		t.Fatalf("a refused tend built nodes: %v %v", c, err)
	}
	// A named profile goes on, and so does --yes.
	out.Reset()
	if code := Run([]string{"tend", "--profile", "desk", "--max", "3", "--data-dir", data}, env); code != 0 || profile != "desk" {
		t.Fatalf("profile: %d %q", code, profile)
	}
	out.Reset()
	if code := Run([]string{"tend", "--yes", "--data-dir", data}, env); code != 0 || !strings.Contains(out.String(), "pending: 0\n") {
		t.Fatalf("yes: %d %q %q", code, out.String(), errw.String())
	}
}

func TestTendUnderTheLimitOrWithMaxRunsAsBefore(t *testing.T) {
	data := t.TempDir()
	seedMany(t, data, 30)
	var out, errw strings.Builder
	calls, profile := 0, ""
	env, fake := tendEnv(&calls, &profile, &out, &errw)
	_ = fake
	// --max keeps the paid calls under the limit, so it is its own guard.
	if code := Run([]string{"tend", "--max", "5", "--data-dir", data}, env); code != 0 || calls != 1 {
		t.Fatalf("max: %d %d %q", code, calls, errw.String())
	}
	if n := fake.compress + fake.merge; n != 5 {
		t.Fatalf("--max 5 is five model calls, got %d: %q", n, out.String())
	}
}

func TestMaxIsOneBudgetAcrossTheStoreAndItsChildren(t *testing.T) {
	data := t.TempDir()
	dir := seedStore(t, data, "work", strings.Repeat("word ", 300), strings.Repeat("word ", 300))
	for _, label := range []string{"a1", "b2"} {
		cd, err := ChildDir(dir, label)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Open(ctx, Config{Dir: cd, Now: newClock().now})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := c.Append(ctx, KindUser, strings.Repeat("word ", 300)); err != nil {
				t.Fatal(err)
			}
		}
		c.Close()
	}
	var out, errw strings.Builder
	calls, profile := 0, ""
	env, fake := tendEnv(&calls, &profile, &out, &errw)
	_ = fake
	if code := Run([]string{"tend", "--max", "4", "--data-dir", data}, env); code != 0 {
		t.Fatalf("tend: %d %q", code, errw.String())
	}
	if n := fake.compress + fake.merge; n != 4 {
		t.Fatalf("--max 4 is four model calls in all, got %d: %q", n, out.String())
	}
}

func TestImportTendRefusesTheDefaultModelOverTheLimitAndKeepsTheMessages(t *testing.T) {
	home := t.TempDir()
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, claudeLine("user", "u"+strings.Repeat("x", i+1), "2026-03-01T10:00:00.000Z",
			`"`+strings.Repeat("word ", 300)+`"`, ""))
	}
	writeLines(t, filepath.Join(home, ".claude", "projects", "-p", "s1.jsonl"), lines...)
	data := t.TempDir()
	var out, errw strings.Builder
	calls, profile := 0, ""
	env, fake := tendEnv(&calls, &profile, &out, &errw)
	_ = fake
	env.Home = home
	code := Run([]string{"import", "--from", "claude", "--tend", "--data-dir", data}, env)
	if code != 1 || !strings.Contains(errw.String(), "Claude Code (paid)") || !strings.Contains(out.String(), "logged: 40 messages") {
		t.Fatalf("import --tend: %d %q %q", code, out.String(), errw.String())
	}
}

func TestImportDirReadsAFolderInPlaceOfTheClaudeHome(t *testing.T) {
	home := fakeHome(t) // holds a Claude and a Codex chat under ~
	other := t.TempDir()
	writeLines(t, filepath.Join(other, "-old-proj", "z1.jsonl"),
		claudeLine("user", "x1", "2026-02-01T10:00:00.000Z", `"from the folder"`, ""))
	writeLines(t, filepath.Join(other, "flat.jsonl"),
		claudeLine("user", "y1", "2026-02-02T10:00:00.000Z", `"a flat file"`, ""))
	data := t.TempDir()
	var out, errw strings.Builder
	env := CLIEnv{Out: &out, Err: &errw, Now: newClock().now, Home: home}
	code := Run([]string{"import", "--dir", other, "--dry-run", "--data-dir", data}, env)
	if code != 0 || !strings.HasPrefix(out.String(), "past chats: 2 (about 2 messages)\n  claude: 2 chats, 2 messages\n") {
		t.Fatalf("dry run: %d %q %q", code, out.String(), errw.String())
	}
	out.Reset()
	if code := Run([]string{"import", "--dir", other, "--data-dir", data}, env); code != 0 {
		t.Fatalf("import: %d %q", code, errw.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err != nil {
		t.Fatalf("the Claude home must be untouched: %v", err)
	}
	out.Reset()
	Run([]string{"log", "--data-dir", data}, env)
	if !strings.Contains(out.String(), "from the folder") || !strings.Contains(out.String(), "a flat file") || strings.Contains(out.String(), "fix the bug") {
		t.Fatalf("log: %q", out.String())
	}
}

func TestImportDirRefusals(t *testing.T) {
	data := t.TempDir()
	var out, errw strings.Builder
	env := CLIEnv{Out: &out, Err: &errw, Now: newClock().now, Home: t.TempDir()}
	if code := Run([]string{"import", "--dir", filepath.Join(data, "none"), "--data-dir", data}, env); code != 1 || !strings.Contains(errw.String(), "no folder ") {
		t.Fatalf("missing: %d %q", code, errw.String())
	}
	errw.Reset()
	if code := Run([]string{"import", "--dir", data, "--from", "codex", "--data-dir", data}, env); code != 2 || !strings.Contains(errw.String(), "--dir holds Claude Code chats") {
		t.Fatalf("codex: %d %q", code, errw.String())
	}
}
