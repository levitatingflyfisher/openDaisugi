package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// railGroup is one folding group of the rail: a task, or with no tasks a
// project, with its agents in the order they were made. A subtask is a
// group under its parent task: Parent is the parent group's key, Depth
// counts the levels above it, and Total holds every agent under the group,
// its subtasks too. A group with no Total counts only its own Rows.
type railGroup struct {
	Key    string
	Title  string
	Rows   []Row
	Parent string
	Depth  int
	Total  []Row
}

// all is every agent the group's header counts: its own and those of the
// subtasks under it.
func (g railGroup) all() []Row {
	if g.Total != nil {
		return g.Total
	}
	return g.Rows
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
// one per project for the agents with no task. A subtask nests under its
// parent task, right after it, and a parent with no agent of its own still
// draws a header when a subtask under it has one. Each kind keeps the
// order its first agent was made in, and inside a group agents keep the
// order they were made in, so a state change never moves a row or a
// header. The bar count, the amber header and shift-tab show who needs
// you. Subagent rows are left out here: they follow their parent.
func (m *Model) groups() []railGroup {
	rows := m.visible()
	var projects []railGroup
	atProject := map[string]int{}
	var top []Row
	for _, r := range rows {
		if r.Parent == "" {
			top = append(top, r)
		}
	}
	sort.SliceStable(top, func(a, b int) bool { return createdBefore(top[a].ID, top[b].ID) })
	own := map[string][]Row{}
	var taskOrder []string
	for _, r := range top {
		if r.Task != "" {
			if _, ok := own[r.Task]; !ok {
				taskOrder = append(taskOrder, r.Task)
			}
			own[r.Task] = append(own[r.Task], r)
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
	return append(m.taskGroups(own, taskOrder), projects...)
}

// taskGroups lays the task groups out as a forest. A task is a root when
// it has no known parent that draws, and a task draws when it has an agent
// or a subtask that draws. Roots and siblings keep the order their first
// agent, subtasks included, was made in.
func (m *Model) taskGroups(own map[string][]Row, order []string) []railGroup {
	draws := map[string]bool{}
	scope := m.scope()
	var inScope map[string]bool
	if scope != "" {
		inScope = m.under(scope)
	}
	for _, id := range order {
		draws[id] = true
		seen := map[string]bool{id: true}
		for cur := id; ; {
			t, ok := m.task(cur)
			if !ok || t.Parent == "" || seen[t.Parent] || (inScope != nil && !inScope[t.Parent]) {
				break
			}
			if _, known := m.task(t.Parent); !known {
				break
			}
			draws[t.Parent] = true
			seen[t.Parent] = true
			cur = t.Parent
		}
	}
	parentOf := func(id string) string {
		if t, ok := m.task(id); ok && t.Parent != id && draws[t.Parent] {
			return t.Parent
		}
		return ""
	}
	kids := map[string][]string{}
	var ids []string
	for id := range draws {
		ids = append(ids, id)
	}
	// first is the id of the oldest agent in the task or under it.
	var first func(id string, walked map[string]bool) string
	first = func(id string, walked map[string]bool) string {
		if walked[id] {
			return ""
		}
		walked[id] = true
		best := ""
		if rs := own[id]; len(rs) > 0 {
			best = rs[0].ID
		}
		for _, k := range ids {
			if parentOf(k) != id {
				continue
			}
			if f := first(k, walked); f != "" && (best == "" || createdBefore(f, best)) {
				best = f
			}
		}
		return best
	}
	firstOf := map[string]string{}
	for _, id := range ids {
		firstOf[id] = first(id, map[string]bool{})
	}
	sort.SliceStable(ids, func(a, b int) bool {
		fa, fb := firstOf[ids[a]], firstOf[ids[b]]
		if fa != fb {
			return createdBefore(fa, fb)
		}
		return ids[a] < ids[b]
	})
	var roots []string
	for _, id := range ids {
		if p := parentOf(id); p != "" {
			kids[p] = append(kids[p], id)
		} else {
			roots = append(roots, id)
		}
	}
	var out []railGroup
	done := map[string]bool{}
	var walk func(id, parent string, depth int) []Row
	walk = func(id, parent string, depth int) []Row {
		if done[id] {
			return nil
		}
		done[id] = true
		at := len(out)
		g := railGroup{Key: "task:" + id, Title: textwidth.Printable(m.taskLabel(id), 0), Rows: own[id], Depth: depth}
		if parent != "" {
			g.Parent = "task:" + parent
		}
		out = append(out, g)
		total := append([]Row(nil), own[id]...)
		for _, k := range kids[id] {
			total = append(total, walk(k, id, depth+1)...)
		}
		out[at].Total = total
		return total
	}
	for _, id := range roots {
		walk(id, "", 0)
	}
	// A parent cycle has no root. Its tasks draw as roots.
	for _, id := range ids {
		walk(id, "", 0)
	}
	return out
}

// hidden reports whether group g sits under a folded parent group.
func (m *Model) hidden(g railGroup, all []railGroup) bool {
	for p := g.Parent; p != ""; {
		if m.Folded[p] {
			return true
		}
		next := ""
		for _, a := range all {
			if a.Key == p {
				next = a.Parent
				break
			}
		}
		p = next
	}
	return false
}

// needs is how many agents of the group need you.
func (g railGroup) needs() int {
	n := 0
	for _, r := range g.all() {
		if r.needsYou() {
			n++
		}
	}
	return n
}

// counts is the header's state counts, as "1 needs · 2 working".
func (g railGroup) counts() string {
	var needs, working, idle, done int
	for _, r := range g.all() {
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
	text := mark + strings.Repeat("  ", min(g.Depth, maxIndent)) + arrow + " " + g.Title
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
