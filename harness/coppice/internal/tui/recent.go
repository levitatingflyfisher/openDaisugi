package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// itemKind names what one cursor stop on the rail is.
type itemKind int

const (
	// itemRow is a live pane or one of its subagents.
	itemRow itemKind = iota
	// itemFold is the Recent line at the foot of the rail.
	itemFold
	// itemResumeAll and itemClearAll act on every ended agent the fold
	// lists. They show only while the fold is open.
	itemResumeAll
	itemClearAll
	// itemEnded is one ended agent in the open fold.
	itemEnded
	// itemGroup is the header of a task or project group.
	itemGroup
)

// item is one cursor stop on the rail. group is set on a group header.
type item struct {
	kind  itemKind
	row   Row
	group railGroup
}

// key names an item so the cursor can find it again after a refresh. A
// live row's key is its pane id, so indexOf works with a plain id.
func (it item) key() string {
	switch it.kind {
	case itemRow:
		return it.row.ID
	case itemFold:
		return "recent"
	case itemResumeAll:
		return "recent:resume"
	case itemClearAll:
		return "recent:clear"
	case itemGroup:
		return "group:" + it.group.Key
	}
	return "ended:" + it.row.ID
}

// recent is the ended agents the rail lists: every one, or while pushed,
// those of the scope task and its descendants. The server sends them
// newest first, and they keep that order.
func (m *Model) recent() []Row {
	if m.scope() == "" {
		return m.Ended
	}
	keep := m.under(m.scope())
	var out []Row
	for _, r := range m.Ended {
		if keep[r.Task] {
			out = append(out, r)
		}
	}
	return out
}

// items is every cursor stop on the rail, in order: each group's header
// and, unless it is folded, its agents with their subagents, then the
// Recent fold when any agent ended, then, while the fold is open, its two
// actions and the ended agents.
func (m *Model) items() []item {
	var out []item
	rows := m.visible()
	for _, g := range m.groups() {
		out = append(out, item{kind: itemGroup, group: g})
		if m.Folded[g.Key] {
			continue
		}
		for _, r := range g.Rows {
			out = append(out, item{kind: itemRow, row: r})
			for _, c := range childrenOf(rows, r.ID) {
				out = append(out, item{kind: itemRow, row: c})
			}
		}
	}
	ended := m.recent()
	if len(ended) == 0 {
		return out
	}
	out = append(out, item{kind: itemFold})
	if !m.RecentOpen {
		return out
	}
	out = append(out, item{kind: itemResumeAll}, item{kind: itemClearAll})
	for _, r := range ended {
		out = append(out, item{kind: itemEnded, row: r})
	}
	return out
}

// norm keeps the cursor off a group header it was not moved onto: a
// cursor at a header by default, as on a new model, means the first
// agent under it. The arrows, a click, or a fold put it on a header on
// purpose, and then it stays.
func (m *Model) norm(all []item) {
	if m.atHeader || m.Cursor < 0 || m.Cursor+1 >= len(all) {
		return
	}
	if all[m.Cursor].kind == itemGroup && all[m.Cursor+1].kind == itemRow {
		m.Cursor++
	}
}

// landed records whether the cursor now sits on a group header on
// purpose.
func (m *Model) landed() {
	all := m.items()
	m.atHeader = m.Cursor >= 0 && m.Cursor < len(all) && all[m.Cursor].kind == itemGroup
}

// SelectedItem returns the cursor stop under the cursor.
func (m *Model) SelectedItem() (item, bool) {
	all := m.items()
	m.norm(all)
	if m.Cursor < 0 || m.Cursor >= len(all) {
		return item{}, false
	}
	return all[m.Cursor], true
}

// selectedKey is the key of the item under the cursor, or "".
func (m *Model) selectedKey() string {
	if it, ok := m.SelectedItem(); ok {
		return it.key()
	}
	return ""
}

// endedRowsFrom turns a pane.list with ended true into rows. A row the
// server does not mark closed is dropped, so an older server that ignores
// the ended field lists nothing here.
func endedRowsFrom(list []map[string]any, now float64) []Row {
	var out []Row
	for _, p := range list {
		if closed, _ := p["closed"].(bool); !closed {
			continue
		}
		r := Row{
			ID: str(p, "id"), Label: str(p, "label"), Harness: str(p, "harness"),
			Worktree: str(p, "cwd"), State: "ended", Task: str(p, "task"),
		}
		if code, ok := num(p, "exit_code"); ok {
			c := int(code)
			r.ExitCode = &c
		}
		if at, ok := num(p, "ended_at"); ok && now > at {
			r.Age = now - at
		}
		out = append(out, r)
	}
	return out
}

// ApplyEnded replaces the ended agents with a fresh pane.list ended true.
// The cursor stays on the same item when it is still there.
func (m *Model) ApplyEnded(list []map[string]any, now float64) {
	keep := m.selectedKey()
	m.Ended = endedRowsFrom(list, now)
	m.Cursor = m.indexOf(keep, m.Cursor)
}

// endedText is the one line the floor shows when an agent ends on its
// own: its label, and its exit code when the server knows it.
func endedText(r Row) string {
	label := textwidth.Printable(r.Label, 0)
	if label == "" {
		label = r.ID
	}
	if r.ExitCode == nil {
		return label + " ended"
	}
	return label + " ended (" + exitText(*r.ExitCode) + ")"
}

// EndedClear is how long the ended line of an agent that ended well
// stays. A non-zero exit stays until the owner looks.
const EndedClear = 10 * time.Second

// exitText is how an exit code reads: "exit 3", or "killed" for the -1
// the server reports for a process a signal ended.
func exitText(code int) string {
	if code < 0 {
		return "killed"
	}
	return fmt.Sprintf("exit %d", code)
}

// noteEnded shows the ended line for r. A non-zero exit is sticky: it
// draws amber and stays until the owner clicks it or opens Recent. A
// sticky line is never replaced by one that is not.
func (m *Model) noteEnded(r Row) {
	sticky := r.ExitCode != nil && *r.ExitCode != 0
	if m.EndedSticky && !sticky {
		return
	}
	m.EndedLine, m.EndedSticky, m.EndedAt = endedText(r), sticky, time.Now()
}

// expireEnded clears an ended line that is not sticky once it has shown
// for EndedClear. It reports whether it cleared one.
func (m *Model) expireEnded(now time.Time) bool {
	if m.EndedLine == "" || m.EndedSticky || now.Sub(m.EndedAt) < EndedClear {
		return false
	}
	m.clearEnded()
	return true
}

// nameOf is the label of pane id, live or ended, or the id when it has
// none.
func (m *Model) nameOf(id string) string {
	if m.has(id) {
		return m.labelOf(id)
	}
	for _, r := range m.Ended {
		if r.ID == id {
			return m.labelOfEnded(r)
		}
	}
	return id
}

// cursorToEnded puts the rail cursor on the Recent entry of pane id when
// the fold is open, else on the fold, so the next key cannot act on a
// live neighbour. With no Recent entry it leaves the cursor alone.
func (m *Model) cursorToEnded(id string) {
	for _, r := range m.recent() {
		if r.ID != id {
			continue
		}
		key := "recent"
		if m.RecentOpen {
			key = "ended:" + id
		}
		m.Cursor = m.indexOf(key, m.Cursor)
		return
	}
}

// startOrder is the order the windows fill in when the floor opens: the
// agents that need you, then the working ones, then the rest, each group
// in rail order. Subagents never fill a window.
func (m *Model) startOrder() []string {
	rows := m.treeRows()
	var out []string
	for _, pick := range []func(Row) bool{
		func(r Row) bool { return r.needsYou() },
		func(r Row) bool { return !r.needsYou() && r.State == "working" },
		func(r Row) bool { return !r.needsYou() && r.State != "working" },
	} {
		for _, r := range rows {
			if r.Parent == "" && pick(r) {
				out = append(out, r.ID)
			}
		}
	}
	return out
}

// keyPressed runs on every key. A line that is not sticky goes once its
// time is up, the same rule the web page keeps, so a key never clears it
// early.
func (m *Model) keyPressed() {
	m.expireEnded(time.Now())
}

// clearEnded removes the ended line.
func (m *Model) clearEnded() {
	m.EndedLine, m.EndedSticky = "", false
}

// ToggleRecent opens or closes the Recent fold. Opening it clears the
// ended line, since the owner now sees the list it points at.
func (m *Model) ToggleRecent() {
	keep := m.selectedKey()
	m.RecentOpen = !m.RecentOpen
	if m.RecentOpen {
		m.clearEnded()
	}
	m.Cursor = m.indexOf(keep, m.Cursor)
}

// recentLine is how one Recent item draws on the rail.
func recentLine(m *Model, it item, cursor, focused bool, cols int) string {
	mark := "  "
	if cursor {
		mark = promptMark
	}
	var text string
	color := sgrFaint
	switch it.kind {
	case itemFold:
		arrow := "▸"
		if m.RecentOpen {
			arrow = "▾"
		}
		text = fmt.Sprintf("%s Recent (%d)", arrow, len(m.recent()))
		color = ""
	case itemResumeAll:
		text = "  Resume all"
		color = ""
	case itemClearAll:
		text = "  Clear all"
		color = ""
	default:
		// The rail is narrow, so an ended row keeps what tells two apart:
		// the label, how it ended, and when.
		label := textwidth.Printable(it.row.Label, 0)
		if label == "" {
			label = it.row.ID
		}
		exit := "ended"
		if it.row.ExitCode != nil {
			exit = exitText(*it.row.ExitCode)
		}
		text = "  " + strings.Join([]string{label, exit, ageText(it.row.Age) + " ago"}, "  ")
		if it.row.ExitCode != nil && *it.row.ExitCode != 0 {
			color = sgrAmber
		}
	}
	line := "  " + mark + text
	if it.kind == itemEnded {
		line = textwidth.Pad(textwidth.Truncate(line, cols-2), cols-2) + " " + closeMark
	}
	if cursor && focused {
		color += sgrReverse
	}
	return color + textwidth.Pad(textwidth.Truncate(line, cols), cols) + sgrReset
}
