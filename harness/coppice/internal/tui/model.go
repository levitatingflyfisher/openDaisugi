// Package tui is the floor: the roster of every pane, grouped by whether
// the operator is needed, with a peek on one pane, a prompt line, and a
// hand-off to attach for one pane full screen. This file is the screen
// model. It holds what the roster shows and nothing about how it is drawn.
package tui

import (
	"fmt"
	"sort"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/textwidth"
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
	// Task is the id of the task the pane works for, or "".
	Task string
	// Parent is set on a subagent row: the id of the pane whose harness
	// runs it. Child is the subagent's own id. A subagent row is read
	// only. It gets no tile, no peek, no attach and no close.
	Parent string
	Child  string
	// Held is set while a foreman hears this row's ask first. The row
	// sits under WORKING until the hold ends.
	Held *Held
	// Looking names the people attached to the pane now, from pane.list.
	Looking []string
	// Stack is the one line a window header shows under the name: the
	// loop, model, router, daisugi mode, last gate verdict and tokens,
	// from the stack, gate and tokens fields of pane.list. It is "" when
	// the server sends none of them.
	Stack string
	// ExitCode is set on an ended row when the server knows the exit
	// code.
	ExitCode *int
	// Kind is pty or headless, from pane.list's own kind field.
	Kind string
	// Trust is true while the pane shows Claude's folder trust screen.
	Trust bool
}

// KindHeadless is the Row.Kind of an agent that takes whole messages
// instead of raw keystrokes: sprig, codex, opencode, and claude in
// stream-json mode.
const KindHeadless = "headless"

// Held is who holds a row's ask: the task whose foreman hears it, and
// how long it has waited there.
type Held struct {
	Foreman   string
	TaskLabel string
	Waited    float64
}

// heldOf reads the held field of a pane.list row or a state event, or
// nil when there is none. now ages the hold from its start.
func heldOf(m map[string]any, now float64) *Held {
	h, ok := m["held"].(map[string]any)
	if !ok {
		return nil
	}
	out := &Held{Foreman: str(h, "by"), TaskLabel: str(h, "task_label")}
	if out.TaskLabel == "" {
		out.TaskLabel = str(h, "task")
	}
	if since, ok := num(h, "since"); ok && now > since {
		out.Waited = now - since
	}
	return out
}

// needsYou is true for a row the operator must answer now: a blocked
// pane whose ask no foreman holds.
func (r Row) needsYou() bool {
	return r.State == "blocked" && r.Held == nil && r.Parent == ""
}

// lookingOf reads the looking list of a pane.list row, or nil when nobody
// looks. A value that is not a string is skipped.
func lookingOf(m map[string]any) []string {
	raw, _ := m["looking"].([]any)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ChildMessage is what every action on a subagent row says.
const ChildMessage = "a subagent lives inside its parent. Enter on the parent."

// childID is the roster id of subagent child of pane parent.
func childID(parent, child string) string { return parent + "/" + child }

// Section is one titled group of rows on the roster.
type Section struct {
	Title string
	Rows  []Row
}

// PeekView is the open detail view on one pane: the last lines of its screen,
// the ask it holds, and the gate's verdict when the data carries one. Ask
// is the same line the row shows, the tool and a colon leading the summary
// when the tool is known.
type PeekView struct {
	Pane    string
	Label   string
	Text    string
	Ask     string
	Tool    string
	Verdict string
	Rule    int
	HasGate bool
	// AskID is the id of the ask the pane holds, or "" when it holds
	// none. Tier is that ask's tier, undoable or permanent. An ask with no
	// tier is permanent.
	AskID string
	Tier  string
	// Trust is true when the pane shows Claude's folder trust screen and
	// holds no ask. y trusts the folder and n is not now.
	Trust bool
}

// Name is what the operator types to allow a permanent ask: the pane's
// label, or its id when it has no label. The server uses the same rule.
func (p *PeekView) Name() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Pane
}

// Answerable is true when the peek shows an ask the operator can answer.
func (p *PeekView) Answerable() bool { return p != nil && p.AskID != "" }

// Permanent is true when the peek's ask needs the pane name to allow.
func (p *PeekView) Permanent() bool { return p.Tier != proto.TierUndoable }

// Model is the whole state of the floor screen. Tiles is the slot model
// the tiles beside the roster draw from, and Screens holds one screen per
// pane a shown tile holds. The floor attaches each such pane with input
// rights at its tile's size. Both are nil on a floor with no tiles.
type Model struct {
	Rows    []Row
	Cursor  int
	Peek    *PeekView
	Prompt  string
	Message string
	Default string
	NeedYou int
	Tiles   *tiles.Tiles
	Screens map[string]*attach.Screen
	// Talking is true while the prompt line has the keyboard: Space is
	// text and Enter runs the line. ctrl-t sets it, and so does the first
	// printable byte. Esc clears it with the prompt.
	Talking bool
	// Confirm holds the id of the pane ctrl-w asked to close. The prompt
	// line shows the question until y closes it or any other key cancels.
	Confirm string
	// Tasks is the last task.list, refreshed with the roster.
	Tasks []TaskRow
	// Path is the stack of tasks the roster is pushed into, from the
	// top task down. The roster shows the panes of the last task and its
	// descendants. An empty path shows every pane.
	Path []string
	// Tree is the open tree view, which stands in for the roster while
	// it is open.
	Tree *TreeView
	// RowAt holds, for every screen line of the last render, the
	// cursor-order index of the roster row drawn there, or -1. TileAt holds,
	// for every screen column, the shown slot index drawn there, or -1.
	RowAt  []int
	TileAt []int
	// TileY holds, for every screen line of the last render, whether the
	// tiles are drawn on that line. A click hits a tile only on such a
	// line.
	TileY []bool
	// TileSizes holds, for every pane the last render showed in a tile,
	// the tile's inner size: its width, and its height less the header
	// line. The floor attaches and resizes the pane to it.
	TileSizes map[string]Size
	// Typing is the id of the pane whose tile owns the keyboard, or ""
	// while the roster owns it.
	Typing string
	// HeadlessLine and HeadlessCursor hold, for every headless pane a key
	// has ever reached, its own input line and the byte offset of the
	// cursor in it. A window shows this instead of forwarding keys raw:
	// Enter sends it as agent.prompt and clears it. It survives a click
	// away and back, keyed by pane id.
	HeadlessLine   map[string]string
	HeadlessCursor map[string]int
	// Leave is the key that gives the keyboard back to the roster. The
	// typing header and footer name it.
	Leave byte
	// Talk is the key that records a voice clip. Zero means
	// config.DefaultTalk. Voice is the line about the clip: recording,
	// being heard, where its text went, or why there is none.
	Talk  byte
	Voice string
	// Notes holds the last NotesShown notes, oldest first. They draw dim
	// above the prompt.
	Notes []string
	// Ended is the last pane.list with ended true: the agents that ended
	// on their own, newest first. They never fill a window. The rail
	// lists them in the Recent fold, which RecentOpen opens.
	Ended      []Row
	RecentOpen bool
	// EndedLine is the one line shown when an agent ends on its own.
	// EndedSticky marks a non-zero exit: it draws amber and stays until
	// the owner clicks it or opens Recent.
	EndedLine   string
	EndedSticky bool
	// EndedAt is when the ended line was set.
	EndedAt time.Time
	// Facts is the floor header row's facts from floor.facts, or nil.
	Facts *Facts
	// Paused is the notice shown while keys are dropped after the agent
	// that had them ended or left the screen, or "".
	Paused string
	// Folded holds the rail groups folded to their header line, by group
	// key.
	Folded map[string]bool
	// CursorHidden is set by the last render when the typing agent's own
	// cursor falls outside its window, so the frame hides the cursor.
	CursorHidden bool
	// WinRows is the inner height of a window in the last render, or 0
	// with no windows.
	WinRows int
	// Picker is the open project picker, which stands in for the rail
	// while it is open.
	Picker *Picker
	// Renaming holds the id of the pane the prompt line renames. The
	// prompt holds the new label until Enter sends it or Esc drops it.
	Renaming string
	// ClearAll is true while the prompt line asks whether to forget
	// every ended agent the fold lists.
	ClearAll bool
	// CursorX and CursorY are where the last render put the hardware
	// cursor: the typing agent's own cursor inside its window, the end of
	// the prompt line while it has the keyboard, or the selected row.
	CursorX, CursorY int
	// RailW is the width of the rail in the last render. TileX holds the
	// first column of every shown window and TileW its width. HeaderY is
	// the screen line of the window headers, or -1. EndedY is the screen
	// line of the ended line, or -1.
	RailW   int
	TileX   []int
	TileW   []int
	HeaderY int
	EndedY  int
	// atHeader is true when the cursor was put on a group header on
	// purpose. See norm.
	atHeader bool
	// shown is how many windows the last render showed. bodyCur is the
	// body line of the cursor in the last render, or -1.
	shown   int
	bodyCur int
}

// Picker is the open project picker: the projects from project.list and
// the one under its cursor.
type Picker struct {
	Projects []Project
	Cursor   int
}

// Project is one row of project.list.
type Project struct {
	Path   string
	Name   string
	Pinned bool
}

// NotesShown is how many notes the floor draws.
const NotesShown = 3

// AddNote keeps one note, and only the last NotesShown.
func (m *Model) AddNote(text string) {
	m.Notes = append(m.Notes, text)
	if len(m.Notes) > NotesShown {
		m.Notes = append([]string(nil), m.Notes[len(m.Notes)-NotesShown:]...)
	}
}

// Size is a pane size in cells.
type Size struct {
	Cols, Rows int
}

// Section titles, in the order the roster shows them.
const (
	TitleNeedsYou = "NEEDS YOU"
	TitleWorking  = "WORKING"
	TitleDone     = "DONE"
)

// Group sorts the pane rows into the three sections. Subagent rows are
// left out: they follow their parent, see ordered. NEEDS YOU holds blocked
// rows whose ask no foreman holds, DONE holds done rows, and WORKING holds
// every other row. Inside a section rows keep the order they were made
// in, so a state change never moves a row within its section. Every
// section is returned even when it is empty.
func Group(rows []Row) []Section {
	secs := []Section{{Title: TitleNeedsYou}, {Title: TitleWorking}, {Title: TitleDone}}
	for _, r := range rows {
		if r.Parent != "" {
			continue
		}
		switch {
		case r.needsYou():
			secs[0].Rows = append(secs[0].Rows, r)
		case r.State == "done":
			secs[2].Rows = append(secs[2].Rows, r)
		default:
			secs[1].Rows = append(secs[1].Rows, r)
		}
	}
	for i := range secs {
		rs := secs[i].Rows
		sort.SliceStable(rs, func(a, b int) bool { return createdBefore(rs[a].ID, rs[b].ID) })
	}
	return secs
}

// under returns the task and every descendant of it as a set.
func (m *Model) under(id string) map[string]bool {
	children := map[string][]string{}
	for _, t := range m.Tasks {
		if t.Parent != "" {
			children[t.Parent] = append(children[t.Parent], t.ID)
		}
	}
	set := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if set[id] {
			return
		}
		set[id] = true
		for _, c := range children[id] {
			walk(c)
		}
	}
	walk(id)
	return set
}

// scope is the task the roster is pushed into, or "" on the top floor.
func (m *Model) scope() string {
	if len(m.Path) == 0 {
		return ""
	}
	return m.Path[len(m.Path)-1]
}

// visible is the rows the roster shows: every row, or while pushed, the
// rows of the scope task and its descendants.
func (m *Model) visible() []Row {
	if m.scope() == "" {
		return m.Rows
	}
	keep := m.under(m.scope())
	var out []Row
	for _, r := range m.Rows {
		if keep[r.Task] {
			out = append(out, r)
		}
	}
	return out
}

// taskLabel is the label of task id, or the id when it has none or is
// not known.
func (m *Model) taskLabel(id string) string {
	for _, t := range m.Tasks {
		if t.ID == id && t.Label != "" {
			return t.Label
		}
	}
	return id
}

// task returns task id from the last task.list.
func (m *Model) task(id string) (TaskRow, bool) {
	for _, t := range m.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return TaskRow{}, false
}

// hasTask reports whether task id is in the last task.list.
func (m *Model) hasTask(id string) bool {
	_, ok := m.task(id)
	return ok
}

// chain is the ids from the top task down to task id, or nil when id is
// not in the last task.list. A parent that is not known ends the walk, and
// so does a parent seen before.
func (m *Model) chain(id string) []string {
	if !m.hasTask(id) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		t, ok := m.task(cur)
		if !ok {
			break
		}
		seen[cur] = true
		out = append([]string{cur}, out...)
		cur = t.Parent
	}
	return out
}

// Push scopes the roster to task id and its descendants. The path becomes
// the chain from the top task down to id, so the bar always reads from the
// floor down. A task not in the last task.list pushes nothing. The cursor
// goes to the first row, and a peek on a pane the scope hides closes.
func (m *Model) Push(id string) {
	c := m.chain(id)
	if c == nil {
		return
	}
	m.Path = c
	m.Cursor = 0
	m.rescope()
}

// Pop goes up one level. On the top floor it does nothing. The cursor
// stays on the same pane.
func (m *Model) Pop() {
	if len(m.Path) == 0 {
		return
	}
	keep := m.selectedKey()
	m.Path = m.Path[:len(m.Path)-1]
	m.rescope()
	m.Cursor = m.indexOf(keep, 0)
}

// rescope brings the count and the peek in line with a new scope.
func (m *Model) rescope() {
	m.recount()
	if m.Peek == nil {
		return
	}
	for _, r := range m.visible() {
		if r.ID == m.Peek.Pane {
			return
		}
	}
	m.ClosePeek()
}

// NewTree opens the tree on the scope: every task on the top floor, or
// while pushed, the scope task and its descendants.
func (m *Model) NewTree(cols int) *TreeView {
	tasks := m.Tasks
	if sc := m.scope(); sc != "" {
		keep := m.under(sc)
		tasks = nil
		for _, t := range m.Tasks {
			if keep[t.ID] {
				if t.ID == sc {
					t.Parent = ""
				}
				tasks = append(tasks, t)
			}
		}
	}
	return NewTreeView(tasks, m.Rows, cols)
}

// ordered returns every row in section order, the order the cursor walks.
// Each pane's subagent rows follow it.
func ordered(rows []Row) []Row {
	var out []Row
	for _, s := range Group(rows) {
		for _, r := range s.Rows {
			out = append(out, r)
			out = append(out, childrenOf(rows, r.ID)...)
		}
	}
	return out
}

// childrenOf returns the subagent rows of pane id, in list order.
func childrenOf(rows []Row, id string) []Row {
	var out []Row
	for _, r := range rows {
		if r.Parent == id {
			out = append(out, r)
		}
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

// Words a blocked row with no ask shows, by what found the block. Only the
// gate's own block blames the gate.
const (
	BlockedByGate      = "Blocked. The gate gave no detail."
	BlockedOwnQuestion = "Waiting on the agent's own question. Open it to answer."
	// TrustLine is what a row on Claude's folder trust screen shows.
	TrustLine = "Asks to trust this folder. Peek with Space, then y trusts it or n is not now."
)

// trustOf reports whether a pane.list row or a state event shows Claude's
// folder trust screen.
func trustOf(m map[string]any) bool {
	return str(m, "state") == "blocked" && askOf(m) == nil && proto.IsTrustDetail(str(m, "detail"))
}

// eventLine is the line a pane.list row or a state event shows: the ask
// when there is one, else, for a blocked pane, what found the block,
// else the detail.
func eventLine(m map[string]any) string {
	ask := askOf(m)
	if ask == nil && str(m, "state") == "blocked" {
		if str(m, "source") == "gate" {
			return BlockedByGate
		}
		if trustOf(m) {
			return TrustLine
		}
		return BlockedOwnQuestion
	}
	return lineFor(str(m, "detail"), ask)
}

// askOf reads the ask sub-map of a list row or a state event.
func askOf(m map[string]any) map[string]any {
	ask, _ := m["ask"].(map[string]any)
	return ask
}

// RowsFrom turns pane.list rows into roster rows. Closed panes are
// dropped. now is used to age each row from its event time.
func RowsFrom(list []map[string]any, now float64) []Row {
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
			Line:     eventLine(p),
			Task:     str(p, "task"),
			Kind:     str(p, "kind"),
			Trust:    trustOf(p),
		}
		if ts, ok := num(p, "ts"); ok {
			r.Age = now - ts
		}
		r.QuietFor, _ = num(p, "quiet_for")
		r.Looking = lookingOf(p)
		r.Stack = stackText(p)
		if r.State == "blocked" {
			r.Held = heldOf(p, now)
		}
		rows = append(rows, r)
		kids, _ := p["children"].([]any)
		for _, k := range kids {
			km, ok := k.(map[string]any)
			if !ok || str(km, "id") == "" {
				continue
			}
			c := Row{
				ID: childID(r.ID, str(km, "id")), Parent: r.ID, Child: str(km, "id"),
				Label: str(km, "label"), State: str(km, "state"), Task: r.Task,
			}
			if ts, ok := num(km, "ts"); ok {
				c.Age = now - ts
			}
			rows = append(rows, c)
		}
	}
	return rows
}

// Apply replaces the rows with a fresh pane.list. Closed panes are dropped.
// The cursor keeps pointing at the same pane when that pane is still on the
// roster, and is clamped to the new row count otherwise. An open peek on a
// pane that left the list closes with it. A path task that is gone from
// Tasks leaves the path with every level under it, with a message.
func (m *Model) Apply(list []map[string]any, now float64) {
	keep := m.selectedKey()
	onRow := false
	if it, ok := m.SelectedItem(); ok && it.kind == itemRow {
		onRow = true
	}
	m.Rows = RowsFrom(list, now)
	for i, id := range m.Path {
		if !m.hasTask(id) {
			m.Message = "task " + id + " is gone"
			m.Path = m.Path[:i]
			break
		}
	}
	m.recount()
	fallback := m.Cursor
	n := 0
	for _, it := range m.items() {
		if it.kind == itemRow || it.kind == itemGroup {
			n++
		}
	}
	if onRow && n > 0 {
		// A row that left gives the cursor to the live row near it,
		// never to the Recent fold below the rows.
		fallback = clamp(fallback, n)
	}
	m.Cursor = m.indexOf(keep, fallback)
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

// rosterOrder is every pane id in cursor order. Subagent rows are left
// out: they never fill a tile.
func (m *Model) rosterOrder() []string {
	all := m.treeRows()
	ids := make([]string, 0, len(all))
	for _, r := range all {
		if r.Parent == "" {
			ids = append(ids, r.ID)
		}
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

// isHeadless reports whether pane id takes whole messages instead of raw
// keystrokes. A pane not on the roster reads as not headless.
func (m *Model) isHeadless(id string) bool {
	r, ok := m.row(id)
	return ok && r.Kind == KindHeadless
}

// headlessBuf returns the input line and the byte offset of the cursor
// in it for a headless pane's own window, or "", 0 for one that has
// never taken a key.
func (m *Model) headlessBuf(id string) (string, int) {
	return m.HeadlessLine[id], m.HeadlessCursor[id]
}

// setHeadlessBuf stores the input line and cursor for a headless pane's
// own window.
func (m *Model) setHeadlessBuf(id, line string, cursor int) {
	if m.HeadlessLine == nil {
		m.HeadlessLine = map[string]string{}
	}
	if m.HeadlessCursor == nil {
		m.HeadlessCursor = map[string]int{}
	}
	m.HeadlessLine[id] = line
	m.HeadlessCursor[id] = cursor
}

// indexOf finds the cursor position of the item with key id: a pane id,
// or the key of a Recent item. When it is gone it returns fallback
// clamped to the item count.
func (m *Model) indexOf(id string, fallback int) int {
	all := m.items()
	if id != "" {
		for i, it := range all {
			if it.key() == id {
				return i
			}
		}
	}
	i := clamp(fallback, len(all))
	// A fallback that lands on an open group's header moves to its first
	// agent, so the floor never opens with the cursor on a header.
	if i+1 < len(all) && all[i].kind == itemGroup && all[i+1].kind == itemRow {
		i++
	}
	return i
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

// recount refreshes NeedYou from the rows the roster shows.
func (m *Model) recount() {
	n := 0
	for _, r := range m.visible() {
		if r.needsYou() {
			n++
		}
	}
	m.NeedYou = n
}

// Event folds one state event into the row it names. The row's state,
// source, line and hold follow the event and its age resets to zero. An
// event with no held field ends the hold. An event
// for a pane the roster does not know is ignored. The next poll picks that
// pane up with the rest of the list.
func (m *Model) Event(ev map[string]any) {
	id := str(ev, "pane")
	if id == "" {
		return
	}
	keep := m.selectedKey()
	found := false
	for i := range m.Rows {
		if m.Rows[i].ID != id {
			continue
		}
		m.Rows[i].State = str(ev, "state")
		m.Rows[i].Source = str(ev, "source")
		m.Rows[i].Line = eventLine(ev)
		m.Rows[i].Trust = trustOf(ev)
		m.Rows[i].Age = 0
		m.Rows[i].Held = nil
		if m.Rows[i].State == "blocked" {
			m.Rows[i].Held = heldOf(ev, 0)
		}
		found = true
	}
	if !found {
		return
	}
	m.recount()
	m.Cursor = m.indexOf(keep, m.Cursor)
}

// Child folds one child event into the subagent rows of its parent. A
// child the roster does not know yet joins after its parent's other
// subagents. An event for a parent the roster does not know is ignored.
func (m *Model) Child(ev map[string]any) {
	parent, kid := str(ev, "pane"), str(ev, "child")
	if parent == "" || kid == "" {
		return
	}
	p, ok := m.row(parent)
	if !ok {
		return
	}
	id := childID(parent, kid)
	for i := range m.Rows {
		if m.Rows[i].ID == id {
			m.Rows[i].State = str(ev, "state")
			if l := str(ev, "label"); l != "" {
				m.Rows[i].Label = l
			}
			m.Rows[i].Age = 0
			return
		}
	}
	keep := m.selectedKey()
	m.Rows = append(m.Rows, Row{
		ID: id, Parent: parent, Child: kid, Label: str(ev, "label"), State: str(ev, "state"), Task: p.Task,
	})
	m.Cursor = m.indexOf(keep, m.Cursor)
}

// Selected returns the live row under the cursor. It is false when the
// cursor is on a Recent item.
func (m *Model) Selected() (Row, bool) {
	it, ok := m.SelectedItem()
	if !ok || it.kind != itemRow {
		return Row{}, false
	}
	return it.row, true
}

// Move shifts the cursor by delta and keeps it inside the rows.
func (m *Model) Move(delta int) {
	all := m.items()
	m.norm(all)
	m.Cursor = clamp(m.Cursor+delta, len(all))
	m.landed()
}

// NextNeed moves the cursor to the next row that needs the operator. From
// a NEEDS YOU row it goes to the next one in that section and wraps. From
// anywhere else it goes to the first NEEDS YOU row. With nobody blocked it
// walks WORKING the same way. With nothing in either it does not move.
// Subagent rows are never a stop.
func (m *Model) NextNeed() {
	secs := Group(m.visible())
	want := secs[0].Rows
	if len(want) == 0 {
		want = secs[1].Rows
	}
	stop := map[string]bool{}
	for _, r := range want {
		stop[r.ID] = true
	}
	var at []string
	for _, r := range m.treeRows() {
		if stop[r.ID] {
			at = append(at, r.ID)
		}
	}
	if len(at) == 0 {
		return
	}
	next := at[0]
	if cur, ok := m.Selected(); ok {
		for k, id := range at {
			if id == cur.ID {
				next = at[(k+1)%len(at)]
			}
		}
	}
	// A stop inside a folded group unfolds it, so the cursor can land.
	if g := m.groupOf(next); m.Folded[g] {
		m.Folded[g] = false
	}
	m.Cursor = m.indexOf(next, m.Cursor)
	m.atHeader = false
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
	p := &PeekView{Pane: r.ID, Label: r.Label, Text: read, Trust: r.Trust && ask == nil}
	if ask != nil {
		p.Ask = lineFor("", ask)
		p.Tool = str(ask, "tool")
		p.AskID = str(ask, "id")
		p.Tier = proto.NormalTier(str(ask, "tier"))
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

// labelOf is the label of pane id, or the id when it has none.
func (m *Model) labelOf(id string) string {
	if r, ok := m.row(id); ok && r.Label != "" {
		return r.Label
	}
	return id
}

// PromptLine is the text after the prompt mark: the question ctrl-w
// asked, the clear-all question, the rename field, or the typed prompt.
func (m *Model) PromptLine() string {
	switch {
	case m.Confirm != "":
		return StopQuestion(m.labelOf(m.Confirm))
	case m.ClearAll:
		return ClearAllQuestion(len(m.recent()))
	case m.Renaming != "":
		return "rename " + textwidth.Printable(m.labelOf(m.Renaming), 0) + ": " + m.Prompt
	}
	return m.Prompt
}

// ClearAllQuestion is the one line Clear all asks before it forgets n
// ended agents.
func ClearAllQuestion(n int) string {
	if n == 1 {
		return "Forget 1 ended agent? Enter forgets it, Esc keeps it."
	}
	return fmt.Sprintf("Forget %d ended agents? Enter forgets them, Esc keeps them.", n)
}

// StopQuestion is the one line ctrl-w asks before it stops a live agent.
func StopQuestion(label string) string {
	return "Stop " + textwidth.Printable(label, 0) + "? Enter stops it, Esc keeps it."
}
