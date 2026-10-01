package tui

import (
	"fmt"
	"strings"
	"testing"
)

// The restart line says how many agents can come back, and the footer
// offers R until a key is pressed.
func TestRestartLineAndFooter(t *testing.T) {
	if got := RestartLine(1); got != "1 agent stopped when coppice restarted." {
		t.Fatalf("RestartLine(1) = %q", got)
	}
	if got := RestartLine(4); got != "4 agents stopped when coppice restarted." {
		t.Fatalf("RestartLine(4) = %q", got)
	}
	m := &Model{Restarted: 4}
	if !strings.HasPrefix(footerText(m), "R resume all  ") {
		t.Fatalf("footer = %q, want R resume all first", footerText(m))
	}
	m.Restarted = 0
	if strings.Contains(footerText(m), "R resume all") {
		t.Fatalf("footer = %q, still offers R", footerText(m))
	}
	// A question or the picker has the keys: R is not on offer there.
	m = &Model{Restarted: 2, Picker: &Picker{}}
	if strings.Contains(footerText(m), "R resume all") {
		t.Fatalf("picker footer = %q, offers R", footerText(m))
	}
}

// The line after New names the directory and why it was chosen.
func TestStartedLineSaysWhereAndWhy(t *testing.T) {
	for _, c := range []struct {
		res   map[string]any
		where string
		want  string
	}{
		{map[string]any{"label": "claude-glean", "cwd": "/src/glean", "why": "recent"}, "", "Started claude-glean in /src/glean (your last project)."},
		{map[string]any{"label": "claude-glean-2", "cwd": "/src/glean", "why": "near"}, "", "Started claude-glean-2 in /src/glean."},
		{map[string]any{"label": "claude-x", "cwd": "/src/x", "why": "pinned"}, "", "Started claude-x in /src/x (a pinned project)."},
		{map[string]any{"label": "claude-y", "cwd": "/src/y", "why": "start"}, "", "Started claude-y in /src/y (the start directory)."},
		{map[string]any{}, "glean", "Started a new agent in glean."},
	} {
		if got := StartedLine(c.res, c.where); got != c.want {
			t.Fatalf("StartedLine(%v) = %q, want %q", c.res, got, c.want)
		}
	}
}

// Each picker row counts its agents by state, asks first.
func TestProjectCounts(t *testing.T) {
	m := &Model{Rows: []Row{
		{ID: "a", State: "working", Worktree: "/p/a"},
		{ID: "b", State: "blocked", Worktree: "/p/a/"},
		{ID: "c", State: "working", Worktree: "/p/a"},
		{ID: "d", State: "idle", Worktree: "/p/b"},
		{ID: "e", State: "working", Worktree: "/p/a", Parent: "a"},
	}}
	if got := m.ProjectCounts("/p/a"); got != "1 needs · 2 working" {
		t.Fatalf("counts for a = %q", got)
	}
	if got := m.ProjectCounts("/p/b"); got != "1 idle" {
		t.Fatalf("counts for b = %q", got)
	}
	if got := m.ProjectCounts("/p/none"); got != "" {
		t.Fatalf("counts for none = %q", got)
	}
	// Paths are cleaned before they compare.
	m.Rows = append(m.Rows, Row{ID: "f", State: "idle", Worktree: "/p//x/../a/."})
	if got := m.ProjectCounts("/p/a/"); got != "1 needs · 2 working · 1 idle" {
		t.Fatalf("counts for a, cleaned = %q", got)
	}
	if got := StartedLine(map[string]any{"label": "k", "cwd": "/p//a/./"}, ""); got != "Started k in /p/a." {
		t.Fatalf("StartedLine = %q", got)
	}
}

// A missing project draws dim with (missing); the tenth project is 0.
func TestPickerBodyMarksMissingAndNumbersTheTenthZero(t *testing.T) {
	p := &Picker{}
	for i := 0; i < 10; i++ {
		p.Projects = append(p.Projects, Project{Path: fmt.Sprintf("/p/%d", i), Name: fmt.Sprintf("p%d", i)})
	}
	p.Projects[1].Missing = true
	m := &Model{Picker: p, Rows: []Row{{ID: "a", State: "working", Worktree: "/p/0"}}}
	lines, _ := pickerBody(m, 100, 20)
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "p0  1 working") {
		t.Fatalf("no counts on the first row:\n%s", text)
	}
	if !strings.Contains(text, sgrFaint) || !strings.Contains(text, "p1 (missing)") {
		t.Fatalf("the missing row is not dim and marked:\n%s", text)
	}
	if !strings.Contains(text, "0 p9") {
		t.Fatalf("the tenth row is not numbered 0:\n%s", text)
	}
}

// A long directory shortens to its own name, so the end of the line shows.
func TestDirBase(t *testing.T) {
	for in, want := range map[string]string{"/src/glean": "glean", "/src/glean/": "glean", "/": "/", "glean": "glean", "": ""} {
		if got := dirBase(in); got != want {
			t.Fatalf("dirBase(%q) = %q, want %q", in, got, want)
		}
	}
}
