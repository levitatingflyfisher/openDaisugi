// Package attach is the single-pane renderer behind `coppice attach`. Splits
// and multi-pane layouts are the cockpit's job and the PWA's job; this is one
// pane, a status line, and one key that leaves.
package attach

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/textwidth"
)

// Screen holds the client's copy of the pane grid, rebuilt from frames.
type Screen struct {
	cols, rows int
	grid       [][]proto.Cell
	cursor     [2]int
	// resized is true once a frame has changed the grid size and RenderTo
	// has not yet cleared the terminal for it. RenderTo clears once, then
	// clears this, so an ordinary redraw at a stable size never wipes the
	// screen.
	resized bool
}

func NewScreen(cols, rows int) *Screen {
	s := &Screen{}
	s.setSize(cols, rows)
	return s
}

// setSize clamps a negative width or height to zero before allocating. A
// frame is data from the wire, not a value this package computed, so a
// negative row or column count must never reach make: it would panic the
// renderer instead of just showing an empty screen.
func (s *Screen) setSize(cols, rows int) {
	if cols < 0 {
		cols = 0
	}
	if rows < 0 {
		rows = 0
	}
	s.cols, s.rows = cols, rows
	s.grid = make([][]proto.Cell, rows)
	for y := range s.grid {
		s.grid[y] = make([]proto.Cell, cols)
	}
}

// Apply folds one frame in. A frame whose size differs from ours replaces the
// grid: the server resized, and keeping stale rows would show a screen that
// never existed. RenderTo clears the terminal once for this change, so a cell
// outside the new grid never lingers there either.
func (s *Screen) Apply(f proto.Frame) {
	if f.Cols != s.cols || f.Rows != s.rows {
		s.setSize(f.Cols, f.Rows)
		s.resized = true
	}
	for y, row := range f.RowsChanged {
		if y < 0 || y >= s.rows {
			continue
		}
		for x := 0; x < s.cols; x++ {
			if x < len(row) {
				s.grid[y][x] = row[x]
			} else {
				s.grid[y][x] = proto.Cell{}
			}
		}
	}
	s.cursor = f.Cursor
}

// Cursor is the pane's own cursor from the last frame: column, then row,
// both from zero.
func (s *Screen) Cursor() (x, y int) { return s.cursor[0], s.cursor[1] }

// Text is the plain content, used by tests and by anything that wants the
// screen without colour.
func (s *Screen) Text() string {
	lines := make([]string, s.rows)
	for y, row := range s.grid {
		var b strings.Builder
		for _, c := range row {
			if c.Text == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Text)
			}
		}
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	return strings.Join(lines, "\n")
}

// RenderTo writes the whole screen with colour, then places the cursor. The
// cursor goes last so the terminal does not flicker it across the redraw.
//
// A double-width glyph occupies two grid columns: the glyph itself, then an
// empty spacer cell libghostty places right after it. This is confirmed
// against a real terminal: feeding "中x" puts "中" at column 0, an empty
// cell at column 1, and "x" at column 2. The real terminal already advances
// two columns printing the glyph, so that spacer must print nothing -
// printing an explicit space for it would add a column nothing put there,
// and push every later cell on the row one column right of where the grid
// says it belongs. wide, below, decides which glyphs count.
func (s *Screen) RenderTo(w io.Writer) error {
	var b strings.Builder
	if s.resized {
		b.WriteString("\x1b[2J")
		s.resized = false
	}
	b.WriteString("\x1b[H")
	for y, row := range s.grid {
		b.WriteString("\x1b[K")
		last := proto.Cell{}
		skipSpacer := false
		for _, c := range row {
			if skipSpacer && c.Text == "" {
				skipSpacer = false
				continue
			}
			skipSpacer = false
			if c.FG != last.FG || c.BG != last.BG || c.Attrs != last.Attrs {
				b.WriteString(sgr(c))
				last = c
			}
			if c.Text == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Text)
				if textwidth.IsWide(c.Text) {
					skipSpacer = true
				}
			}
		}
		b.WriteString("\x1b[0m")
		if y < len(s.grid)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(fmt.Sprintf("\x1b[%d;%dH", s.cursor[1]+1, s.cursor[0]+1))
	_, err := io.WriteString(w, b.String())
	return err
}

// Lines returns one string per grid row, colored the way RenderTo colors.
// A spacer cell after a wide glyph prints nothing. Every row is exactly
// cols cells wide and ends with the SGR reset.
func (s *Screen) Lines() []string {
	out := make([]string, len(s.grid))
	for y, row := range s.grid {
		var b strings.Builder
		last := proto.Cell{}
		skipSpacer := false
		for _, c := range row {
			if skipSpacer && c.Text == "" {
				skipSpacer = false
				continue
			}
			skipSpacer = false
			if c.FG != last.FG || c.BG != last.BG || c.Attrs != last.Attrs {
				b.WriteString(sgr(c))
				last = c
			}
			if c.Text == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Text)
				if textwidth.IsWide(c.Text) {
					skipSpacer = true
				}
			}
		}
		b.WriteString("\x1b[0m")
		out[y] = b.String()
	}
	return out
}

// sgr turns one cell's attribute bits into SGR codes. The bit order matches
// internal/vt's Attribute constants exactly, so a reordering there must move
// this list too:
//
//	bit 1  AttrBold      -> SGR 1
//	bit 2  AttrFaint      -> SGR 2
//	bit 4  AttrItalic     -> SGR 3
//	bit 8  AttrUnderline  -> SGR 4
//	bit 16 AttrBlink      -> SGR 5
//	bit 32 AttrInverse    -> SGR 7
//	bit 64 AttrStrike     -> SGR 9
func sgr(c proto.Cell) string {
	parts := []string{"0"}
	if c.Attrs&1 != 0 { // bold
		parts = append(parts, "1")
	}
	if c.Attrs&2 != 0 { // faint
		parts = append(parts, "2")
	}
	if c.Attrs&4 != 0 { // italic
		parts = append(parts, "3")
	}
	if c.Attrs&8 != 0 { // underline
		parts = append(parts, "4")
	}
	if c.Attrs&16 != 0 { // blink
		parts = append(parts, "5")
	}
	if c.Attrs&32 != 0 { // inverse
		parts = append(parts, "7")
	}
	if c.Attrs&64 != 0 { // strike
		parts = append(parts, "9")
	}
	out := "\x1b[" + strings.Join(parts, ";") + "m"
	if rgb, ok := parseHex(c.FG); ok {
		out += fmt.Sprintf("\x1b[38;2;%d;%d;%dm", rgb[0], rgb[1], rgb[2])
	}
	if rgb, ok := parseHex(c.BG); ok {
		out += fmt.Sprintf("\x1b[48;2;%d;%d;%dm", rgb[0], rgb[1], rgb[2])
	}
	return out
}

func parseHex(s string) ([3]int, bool) {
	var out [3]int
	if len(s) != 7 || s[0] != '#' {
		return out, false
	}
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &out[0], &out[1], &out[2]); err != nil {
		return out, false
	}
	return out, true
}

// field makes a label or a harness name one status line field. Each run of
// white space becomes one space and other control runes are dropped, so a
// field never holds the two spaces that part one field from the next.
func field(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r) || !unicode.IsPrint(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// StatusLine is the one line under the pane. It names the pane, its label, its
// harness, its merged state and the source of that state, because a state with
// no visible source is a guess the operator cannot audit. It ends with the
// name of the key that leaves the pane.
func StatusLine(paneID, label, harness string, ev *proto.PaneStateEvent, cols int, leave string) string {
	if cols < 0 {
		cols = 0
	}
	state, source := proto.StateUnknown, "none"
	extra := ""
	if ev != nil {
		state = ev.State
		source = ev.Source
		if ev.Ask != nil {
			extra = " " + ev.Ask.Tool
			if ev.Ask.Summary != "" {
				extra += ": " + ev.Ask.Summary
			}
		}
	}
	label, harness = field(label), field(harness)
	parts := []string{paneID}
	if label != "" {
		parts = append(parts, label)
	}
	if harness != "" {
		parts = append(parts, harness)
	}
	parts = append(parts, state+" via "+source)
	line := strings.Join(parts, "  ") + extra
	line += "   " + leave + " leave"
	r := []rune(line)
	if len(r) > cols {
		if cols <= 1 {
			return string(r[:cols])
		}
		line = string(r[:cols-1]) + "…"
	}
	return line
}
