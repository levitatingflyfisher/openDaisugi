package tui

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// TaskRow is one task as task.list reports it. HasAhead is true when the
// row carried an ahead count.
type TaskRow struct {
	ID       string
	Label    string
	Parent   string
	Cwd      string
	Worktree string
	Model    string
	// Foreman is the id of the pane that hears the task's asks first.
	Foreman  string
	State    string
	Panes    []string
	Ahead    int
	HasAhead bool
}

// TreeView is the open tree: its lines, the task id each line stands for
// or "" for a pane line, and the cursor, which rests on a task line.
type TreeView struct {
	Lines  []string
	TaskAt []string
	Cursor int
}

// DecodeTasks turns task.list rows into TaskRows.
func DecodeTasks(list []map[string]any) []TaskRow {
	out := make([]TaskRow, 0, len(list))
	for _, t := range list {
		r := TaskRow{
			ID:       str(t, "id"),
			Label:    str(t, "label"),
			Parent:   str(t, "parent"),
			Cwd:      str(t, "cwd"),
			Worktree: str(t, "worktree"),
			Model:    str(t, "model"),
			Foreman:  str(t, "foreman"),
			State:    str(t, "state"),
		}
		if raw, ok := t["panes"].([]any); ok {
			for _, p := range raw {
				if id, ok := p.(string); ok {
					r.Panes = append(r.Panes, id)
				}
			}
		}
		if n, ok := num(t, "ahead"); ok {
			r.Ahead, r.HasAhead = int(n), true
		}
		out = append(out, r)
	}
	return out
}

// stateMark is the dot and word a tree line ends with.
func stateMark(state string) string {
	switch state {
	case "blocked":
		return "● needs you"
	case "working":
		return "● working"
	case "idle":
		return "○ idle"
	case "done":
		return "· done"
	case "":
		return ""
	}
	return "· " + state
}

// taskText is a task line without its prefix: the label, the worktree's
// base name, the ahead count, and the state mark.
func taskText(t TaskRow) string {
	parts := []string{t.Label}
	if t.Label == "" {
		parts[0] = t.ID
	}
	if t.Worktree != "" {
		parts = append(parts, filepath.Base(t.Worktree))
	}
	if t.HasAhead {
		parts = append(parts, "+"+strconv.Itoa(t.Ahead))
	}
	if mark := stateMark(t.State); mark != "" {
		parts = append(parts, mark)
	}
	// A label and a directory name come from agents, so they are drawn
	// with no control characters.
	return textwidth.Printable(strings.Join(parts, "  "), 0)
}

// paneText is a pane line without its prefix: the label and the state.
func paneText(r Row) string {
	label := textwidth.Printable(r.Label, 0)
	if label == "" {
		label = r.ID
	}
	if mark := stateMark(r.State); mark != "" {
		return label + "  " + mark
	}
	return label
}

// treeLines walks the tasks depth first and draws one line per task and
// one per pane under its task. Roots come in the order given. Under a task
// its panes come first, then its child tasks. taskAt holds the task id of
// each task line and "" for a pane line.
func treeLines(tasks []TaskRow, panes []Row, cols int) (lines, taskAt []string) {
	byID := map[string]TaskRow{}
	children := map[string][]string{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var roots []string
	for _, t := range tasks {
		if _, ok := byID[t.Parent]; ok && t.Parent != t.ID {
			children[t.Parent] = append(children[t.Parent], t.ID)
		} else {
			roots = append(roots, t.ID)
		}
	}
	paneRows := map[string]Row{}
	for _, r := range panes {
		paneRows[r.ID] = r
	}
	seen := map[string]bool{}
	var walk func(id, indent string, last bool)
	walk = func(id, indent string, last bool) {
		if seen[id] {
			return
		}
		seen[id] = true
		t := byID[id]
		branch, deeper := "├─ ", "│  "
		if last {
			branch, deeper = "└─ ", "   "
		}
		lines = append(lines, textwidth.Truncate(indent+branch+taskText(t), cols))
		taskAt = append(taskAt, id)
		var panesHere []Row
		for _, pid := range t.Panes {
			if r, ok := paneRows[pid]; ok {
				panesHere = append(panesHere, r)
			}
		}
		kids := children[id]
		for i, r := range panesHere {
			b := "├─ "
			if i == len(panesHere)-1 && len(kids) == 0 {
				b = "└─ "
			}
			lines = append(lines, textwidth.Truncate(indent+deeper+b+paneText(r), cols))
			taskAt = append(taskAt, "")
		}
		for i, kid := range kids {
			walk(kid, indent+deeper, i == len(kids)-1)
		}
	}
	for i, id := range roots {
		walk(id, "", i == len(roots)-1)
	}
	return lines, taskAt
}

// RenderTree draws the tasks as a text tree, every line cut to cols.
func RenderTree(tasks []TaskRow, panes []Row, cols int) []string {
	lines, _ := treeLines(tasks, panes, cols)
	return lines
}

// NewTreeView opens the tree with the cursor on the first task line.
func NewTreeView(tasks []TaskRow, panes []Row, cols int) *TreeView {
	lines, taskAt := treeLines(tasks, panes, cols)
	return &TreeView{Lines: lines, TaskAt: taskAt}
}

// Move shifts the cursor delta task lines, skipping pane lines, and stops
// at either end.
func (v *TreeView) Move(delta int) {
	step := 1
	if delta < 0 {
		step, delta = -1, -delta
	}
	for ; delta > 0; delta-- {
		next := v.Cursor
		for {
			next += step
			if next < 0 || next >= len(v.TaskAt) {
				return
			}
			if v.TaskAt[next] != "" {
				break
			}
		}
		v.Cursor = next
	}
}

// Selected is the task id under the cursor, or "" on an empty tree.
func (v *TreeView) Selected() string {
	if v.Cursor < 0 || v.Cursor >= len(v.TaskAt) {
		return ""
	}
	return v.TaskAt[v.Cursor]
}
