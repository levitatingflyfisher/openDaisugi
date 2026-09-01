package tui

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// treeFixture is a team with one child that runs two panes, and a second
// root task with a worktree that needs the operator.
func treeFixture() ([]TaskRow, []Row) {
	tasks := []TaskRow{
		{ID: "t1", Label: "review-team", State: "working"},
		{ID: "t2", Label: "docs", Parent: "t1", State: "working", Panes: []string{"w1:p1", "w1:p2"}},
		{ID: "t3", Label: "gate-refactor", Worktree: "/repo-worktrees/gate-refactor",
			State: "blocked", Ahead: 3, HasAhead: true, Panes: []string{"w1:p3"}},
	}
	panes := []Row{
		{ID: "w1:p1", Label: "writer", Task: "t2", State: "working"},
		{ID: "w1:p2", Label: "checker", Task: "t2", State: "idle"},
		{ID: "w1:p3", Label: "claude", Task: "t3", State: "blocked"},
		{ID: "w1:p4", Label: "loose", State: "working"},
	}
	return tasks, panes
}

func TestRenderTreeDrawsTasksThenPanesWithBoxLines(t *testing.T) {
	tasks, panes := treeFixture()
	lines := RenderTree(tasks, panes, 80)
	text := strings.Join(lines, "\n")
	for _, want := range []string{"├─", "└─", "review-team", "docs", "gate-refactor",
		"writer", "checker", "● needs you", "● working", "○ idle", "+3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("tree is missing %q:\n%s", want, text)
		}
	}
	// The worktree shows by its base name, never the whole path.
	if strings.Contains(text, "/repo-worktrees") {
		t.Fatalf("tree shows the whole worktree path:\n%s", text)
	}
	if strings.Count(text, "gate-refactor") != 2 {
		t.Fatalf("want the label and the worktree base name once each:\n%s", text)
	}
	// A pane outside every task is not in the tree.
	if strings.Contains(text, "loose") {
		t.Fatalf("a pane with no task is in the tree:\n%s", text)
	}
	// Panes sit under their task, one level deeper.
	docs, writer := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "docs") {
			docs = i
		}
		if strings.Contains(l, "writer") {
			writer = i
		}
	}
	if docs < 0 || writer != docs+1 {
		t.Fatalf("writer is not right under docs:\n%s", text)
	}
	if strings.Index(lines[writer], "writer") <= strings.Index(lines[docs], "docs") {
		t.Fatalf("the pane is not indented past its task:\n%s", text)
	}
}

func TestRenderTreeNeverExceedsCols(t *testing.T) {
	tasks, panes := treeFixture()
	for _, cols := range []int{80, 24, 8} {
		for _, l := range RenderTree(tasks, panes, cols) {
			if w := textwidth.Width(l); w > cols {
				t.Fatalf("line %q is %d cells at cols %d", l, w, cols)
			}
		}
	}
	if got := RenderTree(nil, nil, 80); len(got) != 0 {
		t.Fatalf("an empty tree drew %q", got)
	}
}

func TestTreeViewCursorWalksTaskLinesOnly(t *testing.T) {
	tasks, panes := treeFixture()
	v := NewTreeView(tasks, panes, 80)
	if len(v.Lines) != len(v.TaskAt) {
		t.Fatalf("%d lines, %d task ids", len(v.Lines), len(v.TaskAt))
	}
	if v.TaskAt[v.Cursor] != "t1" {
		t.Fatalf("cursor opens on %q, want t1", v.TaskAt[v.Cursor])
	}
	v.Move(1)
	if v.TaskAt[v.Cursor] != "t2" {
		t.Fatalf("after one down: %q, want t2", v.TaskAt[v.Cursor])
	}
	v.Move(1)
	if v.TaskAt[v.Cursor] != "t3" {
		t.Fatalf("after two down: %q, want t3, the panes were not skipped", v.TaskAt[v.Cursor])
	}
	v.Move(1)
	if v.TaskAt[v.Cursor] != "t3" {
		t.Fatalf("down past the end moved: %q", v.TaskAt[v.Cursor])
	}
	v.Move(-5)
	if v.TaskAt[v.Cursor] != "t1" {
		t.Fatalf("up past the start moved: %q", v.TaskAt[v.Cursor])
	}
	if got := v.Selected(); got != "t1" {
		t.Fatalf("Selected = %q", got)
	}
	if empty := NewTreeView(nil, nil, 80); empty.Selected() != "" {
		t.Fatal("an empty view selected a task")
	}
}

func TestDecodeTasksReadsTheWire(t *testing.T) {
	rows := DecodeTasks([]map[string]any{
		{"id": "t1", "label": "a", "parent": "", "worktree": "/w/a", "model": "opus",
			"state": "blocked", "panes": []any{"w1:p1"}, "ahead": float64(2)},
		{"id": "t2", "label": "b", "parent": "t1", "state": ""},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	a := rows[0]
	if a.ID != "t1" || a.Label != "a" || a.Worktree != "/w/a" || a.Model != "opus" ||
		a.State != "blocked" || len(a.Panes) != 1 || a.Panes[0] != "w1:p1" || !a.HasAhead || a.Ahead != 2 {
		t.Fatalf("a = %+v", a)
	}
	if b := rows[1]; b.Parent != "t1" || b.HasAhead || len(b.Panes) != 0 {
		t.Fatalf("b = %+v", b)
	}
}

func TestPushKeepsThePanesOfTheTaskAndItsDescendants(t *testing.T) {
	tasks, panes := treeFixture()
	m := &Model{Tasks: tasks}
	m.Rows = panes
	m.Push("t1")
	ids := m.rosterOrder()
	if len(ids) != 2 || !(contains(ids, "w1:p1") && contains(ids, "w1:p2")) {
		t.Fatalf("pushed roster = %v, want the two docs panes", ids)
	}
	m.Pop()
	if got := m.rosterOrder(); len(got) != 4 {
		t.Fatalf("popped roster = %v", got)
	}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
