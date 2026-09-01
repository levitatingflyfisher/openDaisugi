// Package tui is the floor: the roster of every pane, grouped by whether
// the operator is needed, with a peek on one pane, a prompt line, and a
// hand-off to attach for one pane full screen. This file is the screen
// model. It holds what the roster shows and nothing about how it is drawn.
package tui

import (
	"sort"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// Row is one pane on the roster.
type Row struct {
	ID       string
	Label    string
	Harness  string
	Worktree string
	State    string
	Source   string
	Line     string
	Age      float64
	QuietFor float64
}

// Section is one titled group of rows on the roster.
type Section struct {
	Title string
	Rows  []Row
}

// Peek is the open detail view on one pane: the last lines of its screen,
// the ask it holds, and the gate's verdict when the data carries one. Ask
// is the same line the row shows, the tool and a colon leading the summary
// when the tool is known.
type Peek struct {
	Pane    string
	Label   string
	Text    string
	Ask     string
	Tool    string
	Verdict string
	Rule    int
	HasGate bool
}

// Model is the whole state of the floor screen. Tiles is the slot model
// the tiles beside the roster draw from, and Screens holds one screen per
// pane the floor watches view-only. Both are nil on a floor with no tiles.
type Model struct {
	Rows    []Row
	Cursor  int
	Peek    *Peek
	Prompt  string
	Message string
	Default string
	NeedYou int
	Tiles   *tiles.Tiles
	Screens map[string]*attach.Screen
	// RowAt holds, for every screen line of the last render, the
	// cursor-order index of the roster row drawn there, or -1. TileAt holds,
	// for every screen column, the shown slot index drawn there, or -1.
	RowAt  []int
	TileAt []int
}

// Section titles, in the order the roster shows them.
const (
	TitleNeedsYou = "NEEDS YOU"
	TitleWorking  = "WORKING"
	TitleDone     = "DONE"
)

// Group sorts rows into the three sections. NEEDS YOU holds blocked rows,
// DONE holds done rows, and WORKING holds every other state. Inside a
// section the youngest row comes first, and equal ages sort by id so the
// order never shifts between two renders of the same data. Every section
// is returned even when it is empty.
func Group(rows []Row) []Section {
	secs := []Section{{Title: TitleNeedsYou}, {Title: TitleWorking}, {Title: TitleDone}}
	for _, r := range rows {
		switch r.State {
		case "blocked":
			secs[0].Rows = append(secs[0].Rows, r)
		case "done":
			secs[2].Rows = append(secs[2].Rows, r)
		default:
			secs[1].Rows = append(secs[1].Rows, r)
		}
	}
	for i := range secs {
		rs := secs[i].Rows
		sort.SliceStable(rs, func(a, b int) bool {
			if rs[a].Age != rs[b].Age {
				return rs[a].Age < rs[b].Age
			}
			return rs[a].ID < rs[b].ID
		})
	}
	return secs
}

// ordered returns every row in section order, the order the cursor walks.
func ordered(rows []Row) []Row {
	var out []Row
	for _, s := range Group(rows) {
		out = append(out, s.Rows...)
	}
	return out
}

// str reads a string field. A missing field or a JSON null is "".
func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// num reads a JSON number. A missing field is 0 with ok false.
func num(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

// lineFor is the one line a row shows. An ask wins over the detail, and
// the ask's tool leads it as "git: push --force" when the tool is known.
func lineFor(detail string, ask map[string]any) string {
	if ask == nil {
		return detail
	}
	summary := str(ask, "summary")
	if tool := str(ask, "tool"); tool != "" {
		return tool + ": " + summary
	}
	return summary
}

// askOf reads the ask sub-map of a list row or a state event.
func askOf(m map[string]any) map[string]any {
	ask, _ := m["ask"].(map[string]any)
	return ask
}

// Apply replaces the rows with a fresh pane.list. Closed panes are dropped.
// The cursor keeps pointing at the same pane when that pane is still on the
// roster, and is clamped to the new row count otherwise. An open peek on a
// pane that left the list closes with it.
func (m *Model) Apply(list []map[string]any, now float64) {
	keep := ""
	if r, ok := m.Selected(); ok {
		keep = r.ID
	}
	rows := make([]Row, 0, len(list))
	for _, p := range list {
		if closed, _ := p["closed"].(bool); closed {
			continue
		}
		r := Row{
			ID:       str(p, "id"),
			Label:    str(p, "label"),
			Harness:  str(p, "harness"),
			Worktree: str(p, "cwd"),
			State:    str(p, "state"),
			Source:   str(p, "source"),
			Line:     lineFor(str(p, "detail"), askOf(p)),
		}
		if ts, ok := num(p, "ts"); ok {
			r.Age = now - ts
		}
		r.QuietFor, _ = num(p, "quiet_for")
		rows = append(rows, r)
	}
	m.Rows = rows
	m.recount()
	m.Cursor = m.indexOf(keep, m.Cursor)
	if m.Peek != nil && !m.has(m.Peek.Pane) {
		m.ClosePeek()
	}
	m.vacateGone()
}

// vacateGone empties the slot of every pane that left the roster. A slot
// that holds a pane still on the roster stays as it is.
func (m *Model) vacateGone() {
	if m.Tiles == nil {
		return
	}
	for i := len(m.Tiles.Slots) - 1; i >= 0; i-- {
		if id := m.Tiles.Slots[i]; id != "" && !m.has(id) {
			m.Tiles.CloseSlot(i)
		}
	}
}

// rosterOrder is every pane id in cursor order.
func (m *Model) rosterOrder() []string {
	all := ordered(m.Rows)
	ids := make([]string, 0, len(all))
	for _, r := range all {
		ids = append(ids, r.ID)
	}
	return ids
}

// row returns the roster row for pane id.
func (m *Model) row(id string) (Row, bool) {
	for _, r := range m.Rows {
		if r.ID == id {
			return r, true
		}
	}
	return Row{}, false
}

// has reports whether pane id is on the roster.
func (m *Model) has(id string) bool {
	for _, r := range m.Rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// indexOf finds the cursor position of pane id in section order. When the
// pane is gone it returns fallback clamped to the row count.
func (m *Model) indexOf(id string, fallback int) int {
	all := ordered(m.Rows)
	if id != "" {
		for i, r := range all {
			if r.ID == id {
				return i
			}
		}
	}
	return clamp(fallback, len(all))
}

// clamp keeps a cursor inside 0 to n-1, and at 0 when there are no rows.
func clamp(i, n int) int {
	if n == 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// recount refreshes NeedYou from the rows.
func (m *Model) recount() {
	n := 0
	for _, r := range m.Rows {
		if r.State == "blocked" {
			n++
		}
	}
	m.NeedYou = n
}

// Event folds one state event into the row it names. The row's state,
// source, and line follow the event and its age resets to zero. An event
// for a pane the roster does not know is ignored. The next poll picks that
// pane up with the rest of the list.
func (m *Model) Event(ev map[string]any) {
	id := str(ev, "pane")
	if id == "" {
		return
	}
	keep := ""
	if r, ok := m.Selected(); ok {
		keep = r.ID
	}
	found := false
	for i := range m.Rows {
		if m.Rows[i].ID != id {
			continue
		}
		m.Rows[i].State = str(ev, "state")
		m.Rows[i].Source = str(ev, "source")
		m.Rows[i].Line = lineFor(str(ev, "detail"), askOf(ev))
		m.Rows[i].Age = 0
		found = true
	}
	if !found {
		return
	}
	m.recount()
	m.Cursor = m.indexOf(keep, m.Cursor)
}

// Selected returns the row under the cursor in section order.
func (m *Model) Selected() (Row, bool) {
	all := ordered(m.Rows)
	if len(all) == 0 || m.Cursor < 0 || m.Cursor >= len(all) {
		return Row{}, false
	}
	return all[m.Cursor], true
}

// Move shifts the cursor by delta and keeps it inside the rows.
func (m *Model) Move(delta int) {
	m.Cursor = clamp(m.Cursor+delta, len(m.Rows))
}

// OpenPeek opens the peek on the row under the cursor. read is the pane's
// visible text. ask, which may be nil, carries the ask the pane holds, and
// an optional gate sub-map with the verdict and the rule that decided it.
// The verdict shows only when that sub-map is present. An empty floor has
// nothing to peek at, so nothing opens.
func (m *Model) OpenPeek(read string, ask map[string]any) {
	r, ok := m.Selected()
	if !ok {
		return
	}
	p := &Peek{Pane: r.ID, Label: r.Label, Text: read}
	if ask != nil {
		p.Ask = lineFor("", ask)
		p.Tool = str(ask, "tool")
		if gate, ok := ask["gate"].(map[string]any); ok {
			p.HasGate = true
			p.Verdict = str(gate, "verdict")
			rule, _ := num(gate, "rule")
			p.Rule = int(rule)
		}
	}
	m.Peek = p
}

// ClosePeek removes the peek. Closing a peek that is not open does nothing.
func (m *Model) ClosePeek() {
	m.Peek = nil
}
