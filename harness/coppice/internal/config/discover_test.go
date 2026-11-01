package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeLookPath finds only the names in have, at /bin/<name>.
func fakeLookPath(have ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, n := range have {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

func TestDiscoverKeepsProbeOrder(t *testing.T) {
	found := Discover(fakeLookPath("pi", "claude", "sprig"))
	var names []string
	for _, f := range found {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"claude", "pi", "sprig"}) {
		t.Fatalf("names = %v, want probe order", names)
	}
	if found[0].Path != "/bin/claude" {
		t.Fatalf("path = %q", found[0].Path)
	}
}

func TestNothingFoundTeaches(t *testing.T) {
	found := Discover(fakeLookPath())
	if len(found) != 0 {
		t.Fatalf("found %v, want none", found)
	}
	want := "No harness on PATH. Codex Desktop, Cursor, and Antigravity are apps, and coppice cannot own their panes. Install claude, codex, pi, sprig, or opencode."
	if got := AppOnlyNote(found); got != want {
		t.Fatalf("note = %q", got)
	}
	if got := AppOnlyNote([]Found{{Name: "pi"}}); got != "" {
		t.Fatalf("note with a harness found = %q, want empty", got)
	}
}

func TestDiscoverMapsEachHarnessToItsState(t *testing.T) {
	found := Discover(fakeLookPath("claude", "codex", "pi", "sprig", "opencode"))
	want := map[string]string{
		"claude": "hooks", "codex": "hooks", "pi": "rpc", "sprig": "hooks", "opencode": "sse",
	}
	if len(found) != len(want) {
		t.Fatalf("found %d, want %d", len(found), len(want))
	}
	for _, f := range found {
		if want[f.Name] != f.State {
			t.Fatalf("%s state = %q, want %q", f.Name, f.State, want[f.Name])
		}
	}
}

func TestFromFoundBuildsOneTablePerHarness(t *testing.T) {
	found := Discover(fakeLookPath("claude", "pi"))
	c := FromFound(found, "pi")
	if c.Default != "pi" {
		t.Fatalf("default = %q", c.Default)
	}
	if len(c.Harness) != 2 {
		t.Fatalf("tables = %v", c.Harness)
	}
	if h := c.Harness["claude"]; h.Command != "claude" || h.State != "hooks" || len(h.Args) != 0 {
		t.Fatalf("claude table = %+v", h)
	}
	if h := c.Harness["pi"]; h.Command != "pi" || h.State != "rpc" {
		t.Fatalf("pi table = %+v", h)
	}
}

// A first-run claude table resumes an ended pty pane's own session. No
// other harness gets resume args it has not been checked to take.
func TestFromFoundGivesClaudeItsResumeArgs(t *testing.T) {
	c := FromFound(Discover(fakeLookPath("claude", "pi")), "claude")
	if got := c.Harness["claude"].ResumeArgs; len(got) != 2 || got[0] != "--resume" || got[1] != "{session}" {
		t.Fatalf("claude resume_args = %v", got)
	}
	if got := c.Harness["pi"].ResumeArgs; len(got) != 0 {
		t.Fatalf("pi resume_args = %v, want none", got)
	}
}

func TestAMiseWrapperResolvesToTheRealBinary(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "claude")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	wrap := filepath.Join(dir, "claude")
	body := "#!/usr/bin/env bash\nmise use -g node@lts claude\nexec mise x claude -- claude \"$@\"\n"
	if err := os.WriteFile(wrap, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var asked []string
	old := MiseWhich
	defer func() { MiseWhich = old }()
	MiseWhich = func(n string) (string, error) { asked = append(asked, n); return real, nil }

	found := Discover(func(n string) (string, error) {
		if n == "claude" {
			return wrap, nil
		}
		return "", errors.New("no")
	})
	if len(found) != 1 || found[0].Command != real || !strings.Contains(found[0].Note, "mise wrapper") {
		t.Fatalf("found = %+v", found)
	}
	if !reflect.DeepEqual(asked, []string{"claude"}) {
		t.Errorf("mise asked %v", asked)
	}
	if got := FromFound(found, "claude").Harness["claude"].Command; got != real {
		t.Errorf("command = %q", got)
	}
}

func TestOnlyANarrowMiseWrapperIsResolved(t *testing.T) {
	dir := t.TempDir()
	old := MiseWhich
	defer func() { MiseWhich = old }()
	MiseWhich = func(n string) (string, error) { t.Errorf("mise asked about %s", n); return "", errors.New("no") }
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"codex":    "\x7fELF binary",                                                                             // not a script
		"pi":       "#!/bin/sh\nexec mise x other -- other \"$@\"\n",                                             // names another tool
		"sprig":    "#!/bin/sh\necho hi\n",                                                                       // no mise exec
		"opencode": "#!/bin/sh\n" + strings.Repeat("# pad\n", 500) + "exec mise x opencode -- opencode \"$@\"\n", // too large
	}
	for n, body := range cases {
		if _, ok := resolveMiseWrapper(write(n, body)); ok {
			t.Errorf("%s resolved", n)
		}
	}
	// A wrapper whose mise answer is relative or missing stays as it was.
	MiseWhich = func(string) (string, error) { return "claude", nil }
	if _, ok := resolveMiseWrapper(write("claude", "#!/bin/bash\nexec mise x claude -- claude \"$@\"\n")); ok {
		t.Error("a relative answer was taken")
	}
}

func TestMiseWhichRunsInTheHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if cmd, err := miseWhichCmd("claude"); err != nil || cmd.Dir != home {
		t.Errorf("mise which runs in %q, want %q", cmd.Dir, home)
	}
}

func TestWithNoHomeDirMiseIsNotAskedAndTheNoteSaysSo(t *testing.T) {
	t.Setenv("HOME", "")
	if cmd, err := miseWhichCmd("claude"); err == nil || cmd != nil {
		t.Fatalf("mise would run with no home: %v %v", cmd, err)
	}
	dir := t.TempDir()
	w := filepath.Join(dir, "claude")
	if err := os.WriteFile(w, []byte("#!/bin/bash\nexec mise x claude -- claude \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found := Discover(func(n string) (string, error) {
		if n == "claude" {
			return w, nil
		}
		return "", errors.New("no")
	})
	if len(found) != 1 || found[0].Command != "" {
		t.Fatalf("found %+v", found)
	}
	notes := FoundNotes(found)
	if len(notes) != 1 || !strings.Contains(notes[0], "home dir is unknown") || !strings.Contains(notes[0], "mise was not asked") {
		t.Errorf("notes %v", notes)
	}
}
