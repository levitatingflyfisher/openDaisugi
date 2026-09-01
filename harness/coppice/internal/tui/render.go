package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// SGR codes the roster uses. Every colored line ends with sgrReset.
const (
	sgrAmber = "\x1b[33m"
	sgrBlue  = "\x1b[34m"
	sgrGreen = "\x1b[32m"
	sgrFaint = "\x1b[2m"
	sgrReset = "\x1b[0m"
)

// promptMark leads the prompt line and marks the cursor row.
const promptMark = "› "

// divider stands between the roster and the tiles, and between two tiles.
const divider = "│"

// emptySlotText is the header of a tile with no pane in it.
const emptySlotText = "Empty. Click a row or press Space."

// tileCols is how many tiles a screen of cols columns shows: none under
// 120, one under 200, two at 200 and up.
func tileCols(cols int) int {
	switch {
	case cols < 120:
		return 0
	case cols < 200:
		return 1
	}
	return 2
}

// splitWidths gives the roster and each of n tiles its width on a screen
// of cols columns. The roster takes 45 percent less one divider column.
// The tiles share the rest with one divider column between them, and the
// last tile takes any remainder.
func splitWidths(cols, n int) (roster int, widths []int) {
	roster = cols*45/100 - 1
	if roster < 0 {
		roster = 0
	}
	avail := cols - roster - 1 - (n - 1)
	if avail < 0 {
		avail = 0
	}
	widths = make([]int, n)
	each := avail / n
	for i := range widths {
		widths[i] = each
	}
	widths[n-1] += avail - each*n
	return roster, widths
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

// bar is line 0: the program name and how many panes need the operator.
func bar(m *Model) string {
	if m.NeedYou == 0 {
		return "coppice · quiet"
	}
	return fmt.Sprintf("coppice · %d need you", m.NeedYou)
}

// rowLine is one pane's line. The cursor row carries the mark, every
// other row two spaces in its place.
func rowLine(r Row, cursor bool, cols int) string {
	mark := "  "
	if cursor {
		mark = promptMark
	}
	label := r.Label
	if label == "" {
		label = r.ID
	}
	fields := []string{label, r.Harness, r.State, ageText(r.Age), r.Line}
	kept := fields[:0]
	for _, f := range fields {
		if f != "" {
			kept = append(kept, f)
		}
	}
	plain := textwidth.Pad("  "+mark+strings.Join(kept, "  "), cols)
	return stateColor(r.State) + plain + sgrReset
}

// roster is the body when no peek is open: a title line for every section
// that holds rows, then one line per row. The line under the cursor is
// marked. When the lines do not fit in height, the window slides so the
// cursor line stays on screen. at holds, per line, the cursor-order index
// of the row drawn there, or -1.
func roster(m *Model, cols, height int) (lines []string, at []int) {
	if len(m.Rows) == 0 {
		def := m.Default
		if def == "" {
			def = "claude"
		}
		lines = fit([]string{textwidth.Pad("Nothing running. Enter opens "+def+" here.", cols)}, height)
		return lines, noRows(len(lines))
	}
	cursorAt := 0
	i := 0
	for _, s := range Group(m.Rows) {
		if len(s.Rows) == 0 {
			continue
		}
		lines = append(lines, textwidth.Pad(s.Title, cols))
		at = append(at, -1)
		for _, r := range s.Rows {
			if i == m.Cursor {
				cursorAt = len(lines)
			}
			lines = append(lines, rowLine(r, i == m.Cursor, cols))
			at = append(at, i)
			i++
		}
	}
	if height > 0 && cursorAt >= height {
		lines = lines[cursorAt-height+1:]
		at = at[cursorAt-height+1:]
	}
	return fit(lines, height), fitAt(at, height)
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

// body is everything between the bar and the message line: the roster,
// and the peek in the lower half when one is open. at maps every line to
// the roster row drawn there, or -1.
func body(m *Model, cols, height int) (lines []string, at []int) {
	if height <= 0 {
		return nil, nil
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

// tileLines draws one tile of w by h cells for slot i holding pane id. The
// header carries the slot number, the cursor mark when the slot is focused,
// then the pane's label, harness and state in the state's color. An empty
// slot says what to do instead. The pane's screen rows follow, each cut to
// w cells, and the tile is cut or padded to h lines.
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
			label := r.Label
			if label == "" {
				label = r.ID
			}
			var parts []string
			for _, f := range []string{label, r.Harness, r.State} {
				if f != "" {
					parts = append(parts, f)
				}
			}
			text = strings.Join(parts, "  ")
		}
	}
	lines := []string{color + textwidth.Pad(fmt.Sprintf("%d%s%s", i+1, mark, text), w) + sgrReset}
	if sc := m.Screens[id]; id != "" && sc != nil {
		for _, l := range sc.Lines() {
			if len(lines) >= h {
				break
			}
			lines = append(lines, cutSGR(l, w))
		}
	}
	for len(lines) < h {
		lines = append(lines, textwidth.Pad("", w))
	}
	return lines
}

// split lays the body out with the roster on the left and the tiles on
// the right, a divider between each part. It draws one column per shown
// slot, up to n, and one empty column when the tiles have no slot. Every
// line is exactly cols cells. at maps every line to the roster row drawn
// there, or -1. tileAt maps every column to the shown slot drawn there, or
// -1.
func split(m *Model, cols, height, n int) (lines []string, at, tileAt []int) {
	shown := m.Tiles.Shown(n)
	if n = len(shown); n < 1 {
		n = 1
	}
	rosterW, widths := splitWidths(cols, n)
	tileAt = noRows(cols)
	x := rosterW + 1
	for i, w := range widths {
		if i > 0 {
			x++
		}
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
	left, at := body(m, rosterW, height)
	for len(left) < height {
		left = append(left, textwidth.Pad("", rosterW))
	}
	at = append(at, noRows(height-len(at))...)
	tiles := make([][]string, n)
	for i := range tiles {
		id := ""
		if i < len(shown) {
			id = shown[i]
		}
		tiles[i] = tileLines(m, id, i, widths[i], height)
	}
	lines = make([]string, height)
	for y := range lines {
		var b strings.Builder
		b.WriteString(left[y])
		for i := range tiles {
			b.WriteString(divider)
			b.WriteString(tiles[i][y])
		}
		lines[y] = b.String()
	}
	return lines, at, tileAt
}

// Render lays the floor out as exactly rows lines of exactly cols cells,
// SGR aside. Line 0 is the bar. The last two lines are the footer and the
// prompt. The message line sits above the footer when a message is set.
// The body fills what is left. On a screen wide enough for tiles, and a
// model that has them, the body splits into the roster and the tiles. A
// screen too short for all of that keeps its last lines, so the prompt is
// always the last line drawn. Render also fills m.RowAt and m.TileAt for
// hit testing on the frame it returns.
func Render(m *Model, cols, rows int) []string {
	m.RowAt, m.TileAt = nil, nil
	if rows <= 0 || cols <= 0 {
		return nil
	}
	head := []string{textwidth.Pad(bar(m), cols)}
	var tail []string
	if m.Message != "" {
		tail = append(tail, textwidth.Pad(m.Message, cols))
	}
	tail = append(tail, textwidth.Pad(Hints(), cols), textwidth.Pad(promptMark+m.Prompt, cols))
	bodyH := rows - len(head) - len(tail)
	var lines []string
	var mid []string
	var at, tileAt []int
	if n := tileCols(cols); n > 0 && m.Tiles != nil {
		mid, at, tileAt = split(m, cols, bodyH, n)
	} else {
		mid, at = body(m, cols, bodyH)
		tileAt = noRows(cols)
	}
	lines = append(lines, head...)
	lines = append(lines, mid...)
	at = append(noRows(len(head)), at...)
	for len(lines) < rows-len(tail) {
		lines = append(lines, textwidth.Pad("", cols))
	}
	lines = append(lines, tail...)
	at = append(at, noRows(len(lines)-len(at))...)
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
		at = at[len(at)-rows:]
	}
	m.RowAt, m.TileAt = at, tileAt
	return lines
}

// peekLines lays the peek out in height lines: a header, the ask when the
// pane holds one, the gate's verdict when the data carries one, then the
// last lines of the pane's text that fit.
func peekLines(p *Peek, cols, height int) []string {
	if height <= 0 {
		return nil
	}
	head := []string{textwidth.Pad(strings.TrimRight("peek "+p.Pane+" "+p.Label, " "), cols)}
	if p.Ask != "" {
		head = append(head, textwidth.Pad("asks: "+p.Ask, cols))
	}
	if p.HasGate {
		head = append(head, textwidth.Pad(verdictText(p.Verdict, p.Rule), cols))
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

// verdictText is the one line that says what the gate decided.
func verdictText(verdict string, rule int) string {
	if verdict == "allow" {
		return "The gate says yes"
	}
	return fmt.Sprintf("The gate says no, rule %d", rule)
}
