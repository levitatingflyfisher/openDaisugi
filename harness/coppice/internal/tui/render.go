package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/textwidth"
)

// SGR codes the roster uses. Every colored line ends with sgrReset.
const (
	sgrAmber   = "\x1b[33m"
	sgrBlue    = "\x1b[34m"
	sgrGreen   = "\x1b[32m"
	sgrFaint   = "\x1b[2m"
	sgrReverse = "\x1b[7m"
	sgrAccent  = "\x1b[1;36m"
	sgrReset   = "\x1b[0m"
)

// promptMark leads the prompt line and marks the cursor row.
const promptMark = "› "

// divider stands between the rail and the windows, and between two
// windows. accentDivider stands on each side of the window that has the
// keys, so it reads as a thick border.
const (
	divider       = "│"
	accentDivider = "┃"
)

// closeMark ends every live row and window header. A click on it asks to
// stop that agent. On an ended row it forgets the record.
const closeMark = "×"

// railCols is the width of the rail beside the windows. minWindow is the
// narrowest a window may be and still read: 80 columns.
const (
	railCols  = 38
	minWindow = 80
)

// emptySlotText is the header of a window with no agent in it.
const emptySlotText = "Empty. Select a row and press this window's number."

// headlessPlaceholder is the foot line of a headless agent's window
// while its input line is empty. headlessMark leads the line whenever it
// holds text, typed or left over from before.
const (
	headlessPlaceholder = "This agent takes whole messages. Type and press Enter."
	headlessMark        = "> "
)

// typingWords end the header of the window that has the keys.
func typingWords(leave byte) string {
	return "typing here · " + config.KeyName(leave) + " leaves"
}

// footerText is the key line for what has the keys now: the rail keys
// for a live row, the keys of a Recent item, or while a window, the
// prompt line, a question, the picker or the tree has the keys, the ways
// on and back from there.
func footerText(m *Model) string {
	switch {
	case m.Typing != "":
		return config.KeyName(m.Leave) + " back to the rail, then ctrl-c quits  click another window to type there  " +
			m.talkName() + " speak"
	case m.Confirm != "" || m.ClearAll:
		return "Enter yes  Esc no"
	case m.Renaming != "":
		return "Enter renames  Esc keeps the name"
	case m.Talking:
		return "Enter runs the line  Esc lets it go"
	case m.Picker != nil:
		return "1-9 or Enter starts " + m.defaultHarness() + " there  Up Down move  Esc closes"
	case m.Tree != nil:
		return "Enter goes into the task  Up Down move  Esc closes"
	}
	if it, ok := m.SelectedItem(); ok {
		switch it.kind {
		case itemFold:
			if m.RecentOpen {
				return "Enter closes Recent  n new  N new in project  ctrl-t talk"
			}
			return "Enter opens Recent  n new  N new in project  ctrl-t talk"
		case itemGroup:
			if m.Folded[it.group.Key] {
				return "Enter or Space opens the group  n new  N new in project  shift-tab next need  ctrl-t talk"
			}
			return "Enter or Space folds the group  n new  N new in project  shift-tab next need  ctrl-t talk"
		case itemResumeAll:
			return "Enter resumes every ended agent  Esc up"
		case itemClearAll:
			return "Enter forgets every ended agent  Esc up"
		case itemEnded:
			return "Enter resume  ctrl-w forget  Esc up"
		}
	}
	table := Table()
	if m.Peek.Answerable() {
		// The peek's answer keys win over n while an ask shows.
		for i := range table {
			if table[i].Action == New {
				table[i].Help = "deny"
			}
		}
	}
	return joinKeys(table)
}

// footerLines wraps the footer text at its two-space gaps into lines of
// at most cols cells, so every key stays on screen at any width. It never
// gives more than three lines, and cuts the last one when it must. The
// rail's own key line names the talk key too, when that costs no line;
// the typing footer always names it.
func footerLines(m *Model, cols int) []string {
	text := footerText(m)
	lines := wrapFooter(text, cols)
	if m.Typing == "" && strings.HasSuffix(text, "Esc up") {
		if more := wrapFooter(text+"  "+m.talkName()+" speak", cols); len(more) == len(lines) {
			lines = more
		}
	}
	return lines
}

// wrapWords wraps text at its spaces into at most max lines of at most
// cols cells. The last line keeps the rest and is cut when it must be.
func wrapWords(text string, cols, max int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		switch {
		case cur == "":
			cur = w
		case len(lines) < max-1 && textwidth.Width(cur+" "+w) > cols:
			lines = append(lines, cur)
			cur = w
		default:
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// wrapFooter wraps text at its two-space gaps into lines of at most cols
// cells, never more than three, and cuts the last one when it must.
func wrapFooter(text string, cols int) []string {
	parts := strings.Split(text, "  ")
	var lines []string
	cur := ""
	for _, p := range parts {
		switch {
		case cur == "":
			cur = p
		case textwidth.Width(cur+"  "+p) <= cols:
			cur += "  " + p
		default:
			lines = append(lines, cur)
			cur = p
		}
	}
	lines = append(lines, cur)
	if len(lines) > 3 {
		lines = append(lines[:2], strings.Join(lines[2:], "  "))
	}
	for i := range lines {
		lines[i] = textwidth.Pad(lines[i], cols)
	}
	return lines
}

// windowCount is how many windows a screen of cols columns shows: as many
// as fit beside the rail at minWindow columns each, with one divider
// column before each window.
func windowCount(cols int) int {
	n := (cols - railCols) / (minWindow + 1)
	if n < 0 {
		return 0
	}
	return n
}

// splitWidths gives the rail and each of n windows its width on a screen
// of cols columns. The rail takes railCols. The windows share the rest
// with one divider column before each, and the last window takes any
// remainder.
func splitWidths(cols, n int) (rail int, widths []int) {
	rail = railCols
	if rail > cols {
		rail = cols
	}
	if n < 1 {
		n = 1
	}
	avail := cols - rail - n
	if avail < 0 {
		avail = 0
	}
	widths = make([]int, n)
	each := avail / n
	for i := range widths {
		widths[i] = each
	}
	widths[n-1] += avail - each*n
	return rail, widths
}

// cutSGR cuts a colored line to exactly w cells. SGR codes take no cells
// and stay where they are. The visible text is cut with textwidth.Truncate,
// so a cut line ends with the ellipsis. The result ends with the reset and
// is padded with spaces after it.
func cutSGR(s string, w int) string {
	if w <= 0 {
		return ""
	}
	type piece struct {
		codes string
		r     rune
	}
	var pieces []piece
	var plain strings.Builder
	pending := ""
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				pending += s[i : j+1]
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		pieces = append(pieces, piece{pending, r})
		pending = ""
		plain.WriteRune(r)
		i += size
	}
	kept := textwidth.Truncate(plain.String(), w)
	n, tail := len(pieces), ""
	if kept != plain.String() {
		tail = textwidth.Ellipsis
		n = utf8.RuneCountInString(strings.TrimSuffix(kept, tail))
	}
	var b strings.Builder
	for _, p := range pieces[:n] {
		b.WriteString(p.codes)
		b.WriteRune(p.r)
	}
	b.WriteString(tail)
	b.WriteString(sgrReset)
	if pad := w - textwidth.Width(kept); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}

// stateColor is the SGR code for one pane state.
func stateColor(state string) string {
	switch state {
	case "blocked":
		return sgrAmber
	case "working":
		return sgrBlue
	case "done":
		return sgrGreen
	}
	return sgrFaint
}

// stripSGR removes every SGR sequence, ESC [ ... m, from s.
func stripSGR(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ageText is a row's age in the coarsest unit that is not zero.
func ageText(age float64) string {
	switch {
	case age < 1:
		return "now"
	case age < 60:
		return fmt.Sprintf("%ds", int(age))
	case age < 3600:
		return fmt.Sprintf("%dm", int(age/60))
	}
	return fmt.Sprintf("%dh", int(age/3600))
}

// bar is line 0: the program name, the path of tasks the roster is pushed
// into, and how many of the panes it shows need the operator.
func bar(m *Model) string {
	head := "coppice"
	for _, id := range m.Path {
		head += " › " + textwidth.Printable(m.taskLabel(id), 0)
	}
	switch m.NeedYou {
	case 0:
		return head + " · quiet"
	case 1:
		return head + " · 1 needs you"
	}
	return fmt.Sprintf("%s · %d need you", head, m.NeedYou)
}

// lookingText is "· alice looking" for the people attached to a pane, or
// "" when nobody looks.
func lookingText(names []string) string {
	if len(names) == 0 {
		return ""
	}
	shown := make([]string, 0, len(names))
	for _, n := range names {
		shown = append(shown, textwidth.Printable(n, 0))
	}
	return "· " + strings.Join(shown, ", ") + " looking"
}

// noRows is n entries of -1: screen lines that hold no roster row.
func noRows(n int) []int {
	at := make([]int, n)
	for i := range at {
		at[i] = -1
	}
	return at
}

// fit cuts lines to at most height entries.
func fit(lines []string, height int) []string {
	if height < 0 {
		height = 0
	}
	if len(lines) > height {
		return lines[:height]
	}
	return lines
}

// fitAt cuts a row map to at most height entries.
func fitAt(at []int, height int) []int {
	if height < 0 {
		height = 0
	}
	if len(at) > height {
		return at[:height]
	}
	return at
}

// railFocused is true while the rail has the keys: no window types, and
// no prompt, question, picker or tree has them.
func (m *Model) railFocused() bool {
	return m.Typing == "" && !m.Talking && m.Renaming == "" && m.Confirm == "" &&
		!m.ClearAll && m.Picker == nil && m.Tree == nil
}

// windowTag is the window number a row shows, as "1 ", or two spaces
// when the row's agent is in no shown window. It is "" when the screen
// shows no windows.
func (m *Model) windowTag(id string) string {
	if m.shown == 0 || m.Tiles == nil {
		return ""
	}
	if slot, ok := m.Tiles.SlotOf(id); ok && slot < m.shown {
		return fmt.Sprintf("%d ", slot+1)
	}
	return "  "
}

// rowLine is one pane's line. The cursor row carries the mark, every
// other row two spaces in its place. tag is the window number. A dim row
// draws faint in place of its state's color. The cursor row draws in
// reverse while the rail has the keys. A live row ends with the close
// mark.
func rowLine(r Row, cursor, dim, focused bool, tag string, cols int) string {
	mark := "  "
	if cursor {
		mark = promptMark
	}
	label := textwidth.Printable(r.Label, 0)
	if label == "" {
		label = r.ID
	}
	indent := ""
	if r.Parent != "" {
		// A subagent sits two spaces in under its parent.
		indent = "  "
		if label == r.ID {
			label = r.Child
		}
	}
	state, line := r.State, textwidth.Printable(r.Line, 0)
	color := stateColor(r.State)
	if r.Held != nil {
		// A held ask waits on a foreman, not on the operator.
		state = "held"
		line = "waiting on " + textwidth.Printable(r.Held.TaskLabel, 0) + "'s foreman · " + ageText(r.Held.Waited)
		color = stateColor("working")
	}
	fields := []string{label, textwidth.Printable(r.Harness, 0), state, ageText(r.Age), line, lookingText(r.Looking)}
	kept := fields[:0]
	for _, f := range fields {
		if f != "" {
			kept = append(kept, f)
		}
	}
	text := "  " + mark + tag + indent + strings.Join(kept, "  ")
	plain := textwidth.Pad(text, cols)
	if r.Parent == "" && cols > 4 {
		plain = textwidth.Pad(text, cols-2) + " " + closeMark
	}
	if dim {
		color = sgrFaint
	}
	if cursor && focused {
		color += sgrReverse
	}
	return color + plain + sgrReset
}

// roster is the rail when no peek is open: a header line for every task
// or project group, then one line per agent unless the group is folded,
// then the Recent fold. The line
// under the cursor is marked. While a window types, every row draws dim.
// When the lines do not fit in height, the view slides so the cursor line
// stays on screen. at holds, per line, the cursor-order index of the item
// drawn there, or -1. It records the cursor's line in m.bodyCur.
func roster(m *Model, cols, height int) (lines []string, at []int) {
	rows := m.visible()
	items := m.items()
	m.norm(items)
	focused := m.railFocused()
	cursorAt := -1
	if len(rows) == 0 {
		text := "Nothing running. Enter opens " + m.defaultHarness() + " here."
		switch {
		case m.scope() != "":
			text = "No panes in " + textwidth.Printable(m.taskLabel(m.scope()), 0) + ". Esc goes back."
		case len(items) > 0:
			text = "Nothing running. n starts " + m.defaultHarness() + "."
		}
		lines = append(lines, textwidth.Pad(text, cols))
		at = append(at, -1)
	}
	for i, it := range items {
		if it.kind == itemFold {
			lines = append(lines, textwidth.Pad("", cols))
			at = append(at, -1)
		}
		if i == m.Cursor {
			cursorAt = len(lines)
		}
		switch it.kind {
		case itemGroup:
			lines = append(lines, groupLine(m, it.group, i == m.Cursor, focused, cols))
		case itemRow:
			r := it.row
			tag := m.windowTag(r.ID)
			if r.Parent != "" && tag != "" {
				tag = "  "
			}
			lines = append(lines, rowLine(r, i == m.Cursor, m.Typing != "", focused, tag, cols))
		default:
			lines = append(lines, recentLine(m, it, i == m.Cursor, focused, cols))
		}
		at = append(at, i)
	}
	if height > 0 && cursorAt >= height {
		lines = lines[cursorAt-height+1:]
		at = at[cursorAt-height+1:]
		cursorAt = height - 1
	}
	m.bodyCur = cursorAt
	return fit(lines, height), fitAt(at, height)
}

// defaultHarness is the harness Enter opens on an empty floor.
func (m *Model) defaultHarness() string {
	if m.Default == "" {
		return "claude"
	}
	return m.Default
}

// treeBody draws the open tree in height lines. The cursor line carries
// the mark, every other line two spaces in its place. When the lines do
// not fit, the view slides so the cursor line stays on screen. cur is the
// cursor's line, or -1.
func treeBody(v *TreeView, cols, height int) (lines []string, cur int) {
	if len(v.Lines) == 0 {
		return fit([]string{textwidth.Pad("No tasks. Run: coppice task create --label NAME", cols)}, height), -1
	}
	lines = make([]string, 0, len(v.Lines))
	for i, l := range v.Lines {
		mark := "  "
		if i == v.Cursor {
			mark = promptMark
		}
		lines = append(lines, textwidth.Pad(textwidth.Truncate("  "+mark+l, cols), cols))
	}
	cur = v.Cursor
	if height > 0 && v.Cursor >= height {
		lines = lines[v.Cursor-height+1:]
		cur = height - 1
	}
	return fit(lines, height), cur
}

// pickerBody draws the open project picker in height lines: a title, then
// one numbered line per project, the pinned ones first. cur is the
// cursor's line, or -1.
func pickerBody(m *Model, cols, height int) (lines []string, cur int) {
	p := m.Picker
	lines = append(lines, textwidth.Pad("Start "+m.defaultHarness()+" in a project", cols))
	if len(p.Projects) == 0 {
		lines = append(lines, textwidth.Pad("  No projects yet. Pin one with: coppice project add DIR", cols))
		return fit(lines, height), -1
	}
	cur = -1
	for i, pr := range p.Projects {
		mark := "  "
		if i == p.Cursor {
			mark = promptMark
			cur = len(lines)
		}
		num := "  "
		if i < 9 {
			num = fmt.Sprintf("%d ", i+1)
		}
		pin := ""
		if pr.Pinned {
			pin = "  pinned"
		}
		text := "  " + mark + num + textwidth.Printable(pr.Name, 0) + "  " + textwidth.Printable(pr.Path, 0) + pin
		lines = append(lines, textwidth.Pad(text, cols))
	}
	if height > 0 && cur >= height {
		lines = lines[cur-height+1:]
		cur = height - 1
	}
	return fit(lines, height), cur
}

// body is everything between the bar and the lines under it: the roster,
// and the peek in the lower half when one is open, or the picker or the
// tree in place of them. at maps every line to the rail item drawn there,
// or -1. It records the cursor's line in m.bodyCur.
func body(m *Model, cols, height int) (lines []string, at []int) {
	m.bodyCur = -1
	if height <= 0 {
		return nil, nil
	}
	if m.Picker != nil {
		lines, m.bodyCur = pickerBody(m, cols, height)
		return lines, noRows(len(lines))
	}
	if m.Tree != nil {
		lines, m.bodyCur = treeBody(m.Tree, cols, height)
		return lines, noRows(len(lines))
	}
	if m.Peek == nil {
		return roster(m, cols, height)
	}
	peekH := height / 2
	lines, at = roster(m, cols, height-peekH)
	for len(lines) < height-peekH {
		lines = append(lines, textwidth.Pad("", cols))
	}
	peek := peekLines(m.Peek, cols, peekH)
	lines = append(lines, peek...)
	return lines, append(at, noRows(len(lines)-len(at))...)
}

// stackLines is how many lines the window of pane id gives its stack
// line: one when the server sent a stack, gate or tokens field, else none.
func (m *Model) stackLines(id string) int {
	if r, ok := m.row(id); ok && r.Stack != "" {
		return 1
	}
	return 0
}

// dropCells removes the first n cells of s, cutting only at rune
// boundaries: a rune whose own cells straddle n stays whole on the far
// side of the cut, so the result never starts mid-rune. dropped is the
// width actually removed, which can be more than n by up to one wide
// rune's own cells when n itself falls inside one; the caller must use
// dropped, not n, for anything measured from the cut.
func dropCells(s string, n int) (rest string, dropped int) {
	w := 0
	for i, r := range s {
		if w >= n {
			return s[i:], w
		}
		w += textwidth.Width(string(r))
	}
	return "", w
}

// headlessVisible is the w-cell window of headlessMark+line to show for
// a non-empty line, and the column within it where the cursor (a byte
// offset into line) sits. It scrolls only as far as the cursor has moved
// past the window, never further, so text before the cursor stays put
// until it, too, would no longer fit: the cursor is what stays visible,
// not the whole line, the same choice the pty branch below makes by
// hiding a cursor its own window cannot show rather than lying about
// where it is. The column comes from dropCells' own report of what it
// cut, not from what was asked for: a wide rune straddling the cut can
// take more cells than asked, and using the ask instead would put the
// cursor a cell past where the cut actually landed, inside that rune's
// own second cell on a CJK line.
func headlessVisible(line string, cur, w int) (text string, cursorCol int) {
	if w <= 0 {
		return "", 0
	}
	if cur < 0 || cur > len(line) {
		cur = len(line)
	}
	full := headlessMark + line
	cursorW := textwidth.Width(headlessMark + line[:cur])
	want := cursorW - (w - 1)
	if want < 0 {
		want = 0
	}
	shown, dropped := dropCells(full, want)
	return textwidth.Pad(textwidth.Truncate(shown, w), w), cursorW - dropped
}

// headlessFootLine is the last line of a headless agent's window: its own
// input line, scrolled to keep the cursor visible when it is wider than
// the window, or headlessPlaceholder while it holds nothing. The window
// that has the keys draws it in the normal color; any other window
// showing a headless agent draws it dim, since keys typed there would go
// to the rail instead.
func headlessFootLine(m *Model, id string, w int) string {
	line := m.HeadlessLine[id]
	var shown string
	if line == "" {
		shown = textwidth.Pad(textwidth.Truncate(headlessPlaceholder, w), w)
	} else {
		shown, _ = headlessVisible(line, m.HeadlessCursor[id], w)
	}
	if id == m.Typing {
		return shown
	}
	return sgrFaint + shown + sgrReset
}

// tileLines draws one window of w by h cells for slot i holding pane id.
// The header carries the window number, the cursor mark when the slot is
// focused, then the agent's label, harness and state in the state's
// color, and the close mark. The window that has the keys draws its header
// in the accent color and reverse video and ends it with the typing
// words, which keep their place when the label is cut. The stack line
// follows when the agent has one. An empty slot says what to do instead.
// The agent's screen rows follow, each cut to w cells, and the window is
// cut or padded to h lines. A headless agent's window gives up its last
// line to its own input line instead of showing more screen rows there.
func tileLines(m *Model, id string, i, w, h int) []string {
	if h <= 0 {
		return nil
	}
	mark := "  "
	if m.Tiles != nil && i == m.Tiles.Focus {
		mark = promptMark
	}
	color, text := sgrFaint, emptySlotText
	if id != "" {
		text = id
		if r, ok := m.row(id); ok {
			color = stateColor(r.State)
			label := textwidth.Printable(r.Label, 0)
			if label == "" {
				label = r.ID
			}
			var parts []string
			for _, f := range []string{label, textwidth.Printable(r.Harness, 0), r.State} {
				if f != "" {
					parts = append(parts, f)
				}
			}
			text = strings.Join(parts, "  ")
		}
	}
	head := fmt.Sprintf("%d%s%s", i+1, mark, text)
	end := ""
	if id != "" && w > 4 {
		end = " " + closeMark
	}
	room := w - textwidth.Width(end)
	if id != "" && id == m.Typing {
		color = sgrAccent + sgrReverse
		words := "  " + typingWords(m.Leave)
		head = textwidth.Truncate(head, room-textwidth.Width(words)) + words
	}
	lines := []string{color + textwidth.Pad(head, room) + end + sgrReset}
	if r, ok := m.row(id); ok && id != "" && r.Stack != "" && len(lines) < h {
		lines = append(lines, sgrFaint+textwidth.Pad(textwidth.Truncate("  "+r.Stack, w), w)+sgrReset)
	}
	headless := id != "" && m.isHeadless(id) && h >= 2
	budget := h
	if headless {
		budget = h - 1
	}
	if sc := m.Screens[id]; id != "" && sc != nil {
		for _, l := range sc.Lines() {
			if len(lines) >= budget {
				break
			}
			lines = append(lines, cutSGR(l, w))
		}
	}
	for len(lines) < budget {
		lines = append(lines, textwidth.Pad("", w))
	}
	if headless {
		lines = append(lines, headlessFootLine(m, id, w))
	}
	return lines
}

// split lays the body out with the rail on the left and the windows on
// the right, a divider before each window. It draws one window per shown
// slot, up to n, and one empty window when the tiles have no slot. The
// dividers on both sides of the window that has the keys are thick and
// in the accent color. Every line is exactly cols cells. at maps every
// line to the rail item drawn there, or -1. tileAt maps every column to
// the shown slot drawn there, or -1. It records the inner size of every
// shown window in m.TileSizes, and where each window starts and how wide
// it is in m.TileX and m.TileW.
func split(m *Model, cols, height, n int) (lines []string, at, tileAt []int) {
	shown := m.Tiles.Shown(n)
	m.shown = len(shown)
	if n = len(shown); n < 1 {
		n = 1
	}
	railW, widths := splitWidths(cols, n)
	m.RailW = railW
	tileAt = noRows(cols)
	x := railW
	for i, w := range widths {
		x++
		m.TileX = append(m.TileX, x)
		m.TileW = append(m.TileW, w)
		for k := 0; k < w && x+k < cols; k++ {
			if i < len(shown) {
				tileAt[x+k] = i
			}
		}
		x += w
	}
	if height <= 0 {
		return nil, nil, tileAt
	}
	left, at := body(m, railW, height)
	for len(left) < height {
		left = append(left, textwidth.Pad("", railW))
	}
	at = append(at, noRows(height-len(at))...)
	ids := make([]string, n)
	tiles := make([][]string, n)
	for i := range tiles {
		if i < len(shown) {
			ids[i] = shown[i]
		}
		id := ids[i]
		tiles[i] = tileLines(m, id, i, widths[i], height)
		inner := height - 1 - m.stackLines(id)
		if id != "" && m.isHeadless(id) && height >= 2 {
			inner--
		}
		if _, seen := m.TileSizes[id]; id != "" && !seen && widths[i] > 0 && inner > 0 {
			m.TileSizes[id] = Size{Cols: widths[i], Rows: inner}
		}
	}
	lines = make([]string, height)
	for y := range lines {
		var b strings.Builder
		b.WriteString(left[y])
		for i := range tiles {
			typing := ids[i] != "" && ids[i] == m.Typing
			prevTyping := i > 0 && ids[i-1] != "" && ids[i-1] == m.Typing
			if typing || prevTyping {
				b.WriteString(sgrAccent + accentDivider + sgrReset)
			} else {
				b.WriteString(divider)
			}
			b.WriteString(tiles[i][y])
		}
		lines[y] = b.String()
	}
	return lines, at, tileAt
}

// Render lays the floor out as exactly rows lines of exactly cols cells,
// SGR aside. Line 0 is the bar. Under the body come the notes, the ended
// line, the message line, the footer, one to three lines, and last the
// prompt. The body fills what is left. On a screen wide enough for
// windows, and a model that has them, the body splits into the rail and
// the windows. A screen too short for all of that keeps its last lines,
// so the prompt is always the last line drawn. Render also fills m.RowAt,
// m.TileAt and m.TileY for hit testing on the frame it returns, m.TileSizes
// with the inner size of every shown window, and m.CursorX and m.CursorY
// with where the hardware cursor goes.
func Render(m *Model, cols, rows int) []string {
	m.RowAt, m.TileAt, m.TileY, m.TileSizes = nil, nil, nil, map[string]Size{}
	m.TileX, m.TileW, m.HeaderY, m.EndedY, m.shown, m.RailW = nil, nil, -1, -1, 0, cols
	m.CursorX, m.CursorY, m.CursorHidden, m.WinRows = 0, 0, false, 0
	if rows <= 0 || cols <= 0 {
		return nil
	}
	head := []string{textwidth.Pad(bar(m), cols)}
	if m.Facts != nil && cols >= factsMinCols {
		head = append(head, sgrFaint+textwidth.Pad(textwidth.Truncate(factsLine(m), cols), cols)+sgrReset)
	}
	// With no gate hook and no agent guarded, the floor says how to turn
	// the gate on, on any screen wide enough for the line.
	if gateHintShown(m.Facts) && cols >= hintMinCols {
		head = append(head, textwidth.Pad(gateHint, cols))
	}
	var tail []string
	for _, n := range m.Notes {
		n = textwidth.Printable(n, 0)
		tail = append(tail, sgrFaint+textwidth.Pad(textwidth.Truncate("  "+n, cols), cols)+sgrReset)
	}
	if m.Paused != "" {
		tail = append(tail, sgrAmber+textwidth.Pad(textwidth.Printable(m.Paused, 0), cols)+sgrReset)
	}
	// The voice line wraps, since its end is the fix.
	for _, l := range wrapWords(textwidth.Printable(m.Voice, 0), cols, 3) {
		tail = append(tail, sgrAmber+textwidth.Pad(textwidth.Truncate(l, cols), cols)+sgrReset)
	}
	endedAt := -1
	if m.EndedLine != "" {
		color := ""
		if m.EndedSticky {
			color = sgrAmber
		}
		endedAt = len(tail)
		tail = append(tail, color+textwidth.Pad(textwidth.Printable(m.EndedLine, 0), cols)+sgrReset)
	}
	if m.Message != "" {
		// The message can hold a label or a server's words, so it is drawn
		// with no control characters.
		tail = append(tail, textwidth.Pad(textwidth.Printable(m.Message, 0), cols))
	}
	tail = append(tail, footerLines(m, cols)...)
	prompt := promptMark + m.PromptLine()
	if m.Confirm != "" || m.ClearAll {
		tail = append(tail, sgrAmber+textwidth.Pad(prompt, cols)+sgrReset)
	} else {
		tail = append(tail, textwidth.Pad(prompt, cols))
	}
	bodyH := rows - len(head) - len(tail)
	var lines []string
	var mid []string
	var at, tileAt []int
	tiled := false
	if n := windowCount(cols); n > 0 && m.Tiles != nil {
		mid, at, tileAt = split(m, cols, bodyH, n)
		tiled = true
	} else {
		mid, at = body(m, cols, bodyH)
		tileAt = noRows(cols)
	}
	lines = append(lines, head...)
	lines = append(lines, mid...)
	at = append(noRows(len(head)), at...)
	onTiles := make([]bool, len(lines))
	for i := len(head); i < len(onTiles); i++ {
		onTiles[i] = tiled
	}
	for len(lines) < rows-len(tail) {
		lines = append(lines, textwidth.Pad("", cols))
	}
	tailTop := len(lines)
	lines = append(lines, tail...)
	at = append(at, noRows(len(lines)-len(at))...)
	onTiles = append(onTiles, make([]bool, len(lines)-len(onTiles))...)
	drop := 0
	if len(lines) > rows {
		drop = len(lines) - rows
		lines = lines[drop:]
		at = at[drop:]
		onTiles = onTiles[drop:]
	}
	m.RowAt, m.TileAt, m.TileY = at, tileAt, onTiles
	bodyTop := len(head) - drop
	if tiled && bodyH > 0 {
		m.HeaderY = bodyTop
		m.WinRows = bodyH - 1
	}
	if endedAt >= 0 {
		m.EndedY = tailTop + endedAt - drop
	}
	placeCursor(m, cols, rows, bodyTop, bodyH, prompt)
	return lines
}

// placeCursor sets m.CursorX and m.CursorY: at the end of the prompt line
// while it has the keys, at the typing agent's own cursor inside its
// window, or at the mark of the cursor line in the body. The result is
// always on screen.
func placeCursor(m *Model, cols, rows, bodyTop, bodyH int, prompt string) {
	x, y := 0, bodyTop
	switch {
	case m.Talking || m.Renaming != "" || m.Confirm != "" || m.ClearAll:
		x, y = textwidth.Width(prompt), rows-1
	case m.Typing != "" && m.isHeadless(m.Typing) && len(m.TileX) > 0:
		slot := -1
		for i, id := range m.Tiles.Shown(len(m.TileX)) {
			if id == m.Typing {
				slot = i
			}
		}
		if slot < 0 || bodyH < 2 {
			break
		}
		line, cur := m.headlessBuf(m.Typing)
		var cx int
		if line == "" {
			// The placeholder shows in place of an empty line, and its
			// own text carries no cursor, so this matches headlessMark's
			// own width the way headlessFootLine's empty branch does.
			cx = textwidth.Width(headlessMark)
		} else {
			_, cx = headlessVisible(line, cur, m.TileW[slot])
		}
		cx = min(max(cx, 0), m.TileW[slot]-1)
		x, y = m.TileX[slot]+cx, bodyTop+bodyH-1
	case m.Typing != "" && len(m.TileX) > 0:
		slot := -1
		for i, id := range m.Tiles.Shown(len(m.TileX)) {
			if id == m.Typing {
				slot = i
			}
		}
		if slot < 0 {
			break
		}
		cx, cy := 0, 0
		if sc := m.Screens[m.Typing]; sc != nil {
			cx, cy = sc.Cursor()
		}
		top := 1 + m.stackLines(m.Typing)
		if cx < 0 || cy < 0 || cx >= m.TileW[slot] || cy >= bodyH-top {
			// The agent's cursor is outside what the window shows. A
			// cursor clamped to the edge would lie, so it hides. The frame
			// does not say when the agent itself hides its cursor, so that
			// case still shows one.
			m.CursorHidden = true
		}
		cx = min(max(cx, 0), m.TileW[slot]-1)
		cy = min(max(cy, 0), bodyH-top-1)
		x, y = m.TileX[slot]+cx, bodyTop+top+cy
	case m.bodyCur >= 0:
		x, y = 2, bodyTop+m.bodyCur
	}
	m.CursorX = min(max(x, 0), cols-1)
	m.CursorY = min(max(y, 0), rows-1)
}

// peekLines lays the peek out in height lines: a header, the ask when the
// pane holds one, the gate's verdict when the data carries one, then the
// last lines of the pane's text that fit.
func peekLines(p *PeekView, cols, height int) []string {
	if height <= 0 {
		return nil
	}
	head := []string{textwidth.Pad(strings.TrimRight("peek "+p.Pane+" "+textwidth.Printable(p.Label, 0), " "), cols)}
	if p.Ask != "" {
		head = append(head, textwidth.Pad("asks: "+p.Ask, cols))
	}
	if p.HasGate {
		head = append(head, textwidth.Pad(verdictText(p.Verdict, p.Rule), cols))
	}
	for _, line := range trustLines(p) {
		head = append(head, textwidth.Pad(line, cols))
	}
	for _, line := range tierLines(p) {
		head = append(head, textwidth.Pad(line, cols))
	}
	head = fit(head, height)
	room := height - len(head)
	text := strings.Split(strings.TrimRight(p.Text, "\n"), "\n")
	if len(text) > room {
		text = text[len(text)-room:]
	}
	lines := head
	for _, t := range text {
		lines = append(lines, textwidth.Pad(t, cols))
	}
	for len(lines) < height {
		lines = append(lines, textwidth.Pad("", cols))
	}
	return lines
}

// tierLines are the lines that say how to answer the peek's ask. Deny is
// always one key. An undoable ask allows with one key. A permanent ask
// allows only when the operator types the pane name and presses Enter.
// The rail keys are not text, so the line says to press ctrl-t first.
func tierLines(p *PeekView) []string {
	if !p.Answerable() {
		return nil
	}
	if !p.Permanent() {
		return []string{"Deny is the default.", "y allow once  t allow for this task  n deny"}
	}
	name := textwidth.Printable(p.Name(), 0)
	return []string{"Deny is the default.", "n deny   to allow, press ctrl-t, type " + name + " and enter"}
}

// trustLines are the lines a peek on Claude's folder trust screen shows.
func trustLines(p *PeekView) []string {
	if !p.Trust {
		return nil
	}
	return []string{"Claude asks to trust this folder.", TrustKeys}
}

// TrustKeys is the peek's line of keys for the trust screen. Not now
// presses Esc, which ends Claude.
const TrustKeys = "y trust this folder  n not now, which ends Claude"

// verdictText is the one line that says what the gate decided.
func verdictText(verdict string, rule int) string {
	if verdict == "allow" {
		return "The gate says yes"
	}
	return fmt.Sprintf("The gate says no, rule %d", rule)
}
