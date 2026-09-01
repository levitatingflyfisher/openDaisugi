package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// railGroup is one folding group of the rail: a task, or with no tasks a
// project, with its agents in the order they were made.
type railGroup struct {
	Key   string
	Title string
	Rows  []Row
}

// createdBefore orders two pane ids by when they were made. Ids count up,
// as w1:p2 before w1:p10, so digit runs compare as numbers.
func createdBefore(a, b string) bool {
	for a != "" && b != "" {
		da, db := digitRun(a), digitRun(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[da:], b[db:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// digitRun is how many digits lead s.
func digitRun(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// projectName is the base name of a row's directory, or "no directory".
func projectName(r Row) string {
	if r.Worktree == "" {
		return "no directory"
	}
	return filepath.Base(r.Worktree)
}

// groups is the rail tree: one group per task that has a live agent, then
// one per project for the agents with no task. Each kind keeps the order
// its first agent was made in, and inside a group agents keep the order
// they were made in, so a state change never moves a row or a header.
// The bar count, the amber header and shift-tab show who needs you.
// Subagent rows are left out here: they follow their parent.
func (m *Model) groups() []railGroup {
	rows := m.visible()
	var out, projects []railGroup
	at := map[string]int{}
	atProject := map[string]int{}
	var top []Row
	for _, r := range rows {
		if r.Parent == "" {
			top = append(top, r)
		}
	}
	sort.SliceStable(top, func(a, b int) bool { return createdBefore(top[a].ID, top[b].ID) })
	for _, r := range top {
		if r.Task != "" {
			key := "task:" + r.Task
			i, ok := at[key]
			if !ok {
				i = len(out)
				at[key] = i
				out = append(out, railGroup{Key: key, Title: textwidth.Printable(m.taskLabel(r.Task), 0)})
			}
			out[i].Rows = append(out[i].Rows, r)
			continue
		}
		name := projectName(r)
		key := "project:" + name
		i, ok := atProject[key]
		if !ok {
			i = len(projects)
			atProject[key] = i
			projects = append(projects, railGroup{Key: key, Title: textwidth.Printable(name, 0)})
		}
		projects[i].Rows = append(projects[i].Rows, r)
	}
	return append(out, projects...)
}

// needs is how many agents of the group need you.
func (g railGroup) needs() int {
	n := 0
	for _, r := range g.Rows {
		if r.needsYou() {
			n++
		}
	}
	return n
}

// counts is the header's state counts, as "1 needs · 2 working".
func (g railGroup) counts() string {
	var needs, working, idle, done int
	for _, r := range g.Rows {
		switch {
		case r.needsYou():
			needs++
		case r.State == "working" || r.Held != nil:
			working++
		case r.State == "done":
			done++
		default:
			idle++
		}
	}
	var parts []string
	for _, c := range []struct {
		n    int
		word string
	}{{needs, "needs"}, {working, "working"}, {idle, "idle"}, {done, "done"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.word))
		}
	}
	return strings.Join(parts, " · ")
}

// treeRows is every live row in rail order, folded groups too, each
// pane's subagents after it. It is the order windows fill in.
func (m *Model) treeRows() []Row {
	rows := m.visible()
	var out []Row
	for _, g := range m.groups() {
		for _, r := range g.Rows {
			out = append(out, r)
			out = append(out, childrenOf(rows, r.ID)...)
		}
	}
	return out
}

// groupOf is the key of the group that holds pane id, or "".
func (m *Model) groupOf(id string) string {
	for _, g := range m.groups() {
		for _, r := range g.Rows {
			if r.ID == id {
				return g.Key
			}
		}
	}
	return ""
}

// ToggleGroup folds or unfolds one group. The cursor stays on its header.
func (m *Model) ToggleGroup(key string) {
	if m.Folded == nil {
		m.Folded = map[string]bool{}
	}
	m.Folded[key] = !m.Folded[key]
	m.Cursor = m.indexOf("group:"+key, m.Cursor)
	m.atHeader = true
}

// groupLine is how a group header draws: the fold arrow, the title and
// the state counts. A header with an agent that needs you draws amber.
func groupLine(m *Model, g railGroup, cursor, focused bool, cols int) string {
	mark := "  "
	if cursor {
		mark = promptMark
	}
	arrow := "▾"
	if m.Folded[g.Key] {
		arrow = "▸"
	}
	text := mark + arrow + " " + g.Title
	if c := g.counts(); c != "" {
		text += "  " + c
	}
	color := ""
	if g.needs() > 0 {
		color = sgrAmber
	}
	if cursor && focused {
		color += sgrReverse
	}
	return color + textwidth.Pad(text, cols) + sgrReset
}
