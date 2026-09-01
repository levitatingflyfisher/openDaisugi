package config

import (
	"errors"
	"reflect"
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
