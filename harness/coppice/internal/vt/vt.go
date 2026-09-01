// Package vt is the only package in coppice that touches libghostty. The
// binding's author states its Go API is not stable yet, so a bump breaks the
// compile here and nowhere else. Everything above this package sees Term and
// Cell.
package vt

import (
	"fmt"
	"strings"
	"sync"

	libghostty "go.mitchellh.com/libghostty"
)

// RGB is one resolved colour. A nil *RGB means "the terminal default", which
// the client renders with its own palette.
type RGB struct{ R, G, B uint8 }

// Attribute bits carried on a Cell. The wire format sends this as one integer.
const (
	AttrBold uint16 = 1 << iota
	AttrFaint
	AttrItalic
	AttrUnderline
	AttrBlink
	AttrInverse
	AttrStrike
)

// Cell is one grid cell. Text is the full grapheme cluster, so a wide glyph or
// an emoji with a modifier arrives whole.
type Cell struct {
	Text  string
	FG    *RGB
	BG    *RGB
	Attrs uint16
}

// Term wraps one libghostty terminal. Every method holds mu: libghostty
// terminals are not safe for concurrent use, and a pane feeds bytes from its
// PTY reader while a client asks for a frame.
//
// The render state, the row iterator and the cell cursor are C handles. They
// are allocated once here and reused, never per Viewport call: framePump asks
// for a frame every 16 ms per attached client per pane, and allocating three
// handles on that path without closing them leaks until the process dies.
type Term struct {
	mu       sync.Mutex
	term     *libghostty.Terminal
	rs       *libghostty.RenderState
	ri       *libghostty.RenderStateRowIterator
	rc       *libghostty.RenderStateRowCells
	progress string
	closed   bool
}

// New creates a terminal of the given size with the given scrollback budget in
// lines. Pass 0 for scrollbackLines to keep only the viewport.
func New(cols, rows uint16, scrollbackLines uint) (*Term, error) {
	t := &Term{}
	opts := []libghostty.TerminalOption{
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollbackLines(scrollbackLines),
		// The callback takes the terminal and the report. Storing the
		// formatted payload is all we need: the manifest engine matches
		// Herdr's osc_progress region against exactly this shape. The binding
		// documents Progress == -1 as "omitted": the source program sent no
		// percent field, so the payload must not grow a fake one.
		libghostty.WithProgressReport(func(_ *libghostty.Terminal, r libghostty.TerminalProgressReport) {
			if r.Progress < 0 {
				t.progress = fmt.Sprintf("4;%d", int(r.State))
			} else {
				t.progress = fmt.Sprintf("4;%d;%d", int(r.State), int(r.Progress))
			}
		}),
	}
	term, err := libghostty.NewTerminal(opts...)
	if err != nil {
		return nil, fmt.Errorf("vt: create terminal: %w", err)
	}
	rs, err := libghostty.NewRenderState()
	if err != nil {
		term.Close()
		return nil, fmt.Errorf("vt: render state: %w", err)
	}
	ri, err := libghostty.NewRenderStateRowIterator()
	if err != nil {
		rs.Close()
		term.Close()
		return nil, fmt.Errorf("vt: row iterator: %w", err)
	}
	rc, err := libghostty.NewRenderStateRowCells()
	if err != nil {
		ri.Close()
		rs.Close()
		term.Close()
		return nil, fmt.Errorf("vt: row cells: %w", err)
	}
	t.term, t.rs, t.ri, t.rc = term, rs, ri, rc
	return t, nil
}

// Feed writes VT bytes. VTWrite returns nothing, so the only failure this can
// report is a closed terminal.
func (t *Term) Feed(p []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("vt: terminal is closed")
	}
	t.term.VTWrite(p)
	return nil
}

func (t *Term) Resize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.term.Resize(cols, rows, 0, 0)
}

// Size returns the terminal's own idea of its size. Both accessors can fail, so
// a failure reports 0x0 rather than a plausible lie.
func (t *Term) Size() (uint16, uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size()
}

func (t *Term) size() (uint16, uint16) {
	cols, err := t.term.Cols()
	if err != nil {
		return 0, 0
	}
	rows, err := t.term.Rows()
	if err != nil {
		return 0, 0
	}
	return cols, rows
}

func (t *Term) Cursor() (uint16, uint16, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	x, err := t.term.CursorX()
	if err != nil {
		return 0, 0, false
	}
	y, err := t.term.CursorY()
	if err != nil {
		return 0, 0, false
	}
	vis, err := t.term.CursorVisible()
	if err != nil {
		return x, y, false
	}
	return x, y, vis
}

func (t *Term) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	title, err := t.term.Title()
	if err != nil {
		return ""
	}
	return title
}

// Progress is the last OSC 9;4 payload seen, or "" when none has arrived.
func (t *Term) Progress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress
}

// PlainScreen is the visible screen only, as plain text. A Formatter given a
// nil selection formats the terminal's whole active-screen buffer, scrollback
// included (empirically confirmed: it matches SelectAll's output exactly), so
// getting the screen alone needs a Selection bounded to PointTagActive, the
// area the cursor can move in.
func (t *Term) PlainScreen() (string, error) { return t.lockedActivePlain(false) }

// PlainScreenUnwrapped joins rows the terminal soft-wrapped. The manifest
// evaluator reads this, because Herdr's rules are written against unwrapped
// lines.
func (t *Term) PlainScreenUnwrapped() (string, error) { return t.lockedActivePlain(true) }

func (t *Term) lockedActivePlain(unwrap bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sel, err := t.activeSelection()
	if err != nil {
		return "", err
	}
	return t.plain(unwrap, sel)
}

// activeSelection builds a selection spanning exactly the visible screen
// (PointTagActive, the cursor-movable area), never scrollback. Must be called
// with mu held.
func (t *Term) activeSelection() (*libghostty.Selection, error) {
	cols, rows := t.size()
	if cols == 0 || rows == 0 {
		return nil, fmt.Errorf("vt: terminal has zero size")
	}
	start, err := t.term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: 0, Y: 0})
	if err != nil {
		return nil, fmt.Errorf("vt: active selection start: %w", err)
	}
	end, err := t.term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: cols - 1, Y: uint32(rows - 1)})
	if err != nil {
		return nil, fmt.Errorf("vt: active selection end: %w", err)
	}
	return &libghostty.Selection{Start: *start, End: *end}, nil
}

// plain must be called with mu held.
func (t *Term) plain(unwrap bool, sel *libghostty.Selection) (string, error) {
	opts := []libghostty.FormatterOption{
		libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain),
		libghostty.WithFormatterTrim(true),
		libghostty.WithFormatterUnwrap(unwrap),
	}
	if sel != nil {
		opts = append(opts, libghostty.WithFormatterSelection(sel))
	}
	f, err := libghostty.NewFormatter(t.term, opts...)
	if err != nil {
		return "", fmt.Errorf("vt: formatter: %w", err)
	}
	defer f.Close()
	s, err := f.FormatString()
	if err != nil {
		return "", fmt.Errorf("vt: format: %w", err)
	}
	return s, nil
}

// PlainAll is the scrollback plus the viewport, as plain text. SelectAll
// returns a selection and does not install it, so nothing here touches the
// terminal's active selection: a client may be holding one.
func (t *Term) PlainAll() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sel, err := t.term.SelectAll()
	if err != nil {
		return "", fmt.Errorf("vt: select all: %w", err)
	}
	out, err := t.plain(false, sel)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// Viewport returns the visible grid, one slice per row, always exactly
// rows x cols cells. A cell with no text is the empty string, not a space, so
// a diff can tell "blank" from "a space was typed".
func (t *Term) Viewport() ([][]Cell, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.rs.Update(t.term); err != nil {
		return nil, fmt.Errorf("vt: render state update: %w", err)
	}
	colsU, rowsU := t.size()
	cols, nrows := int(colsU), int(rowsU)
	out := make([][]Cell, 0, nrows)

	if err := t.rs.RowIterator(t.ri); err != nil {
		return nil, fmt.Errorf("vt: row iterator: %w", err)
	}
	buf := make([]byte, 0, 8)
	for t.ri.Next() {
		row := make([]Cell, cols)
		if err := t.ri.Cells(t.rc); err != nil {
			return nil, fmt.Errorf("vt: row cells: %w", err)
		}
		for x := 0; x < cols && t.rc.Next(); x++ {
			buf = buf[:0]
			b, err := t.rc.AppendGraphemes(buf)
			if err != nil {
				return nil, fmt.Errorf("vt: graphemes: %w", err)
			}
			c := Cell{Text: string(b)}
			if st, err := t.rc.Style(); err == nil && st != nil {
				c.Attrs = attrBits(*st)
			}
			if fg, err := t.rc.FgColor(); err == nil && fg != nil {
				r, g, bl := fg.Components()
				c.FG = &RGB{R: r, G: g, B: bl}
			}
			if bg, err := t.rc.BgColor(); err == nil && bg != nil {
				r, g, bl := bg.Components()
				c.BG = &RGB{R: r, G: g, B: bl}
			}
			row[x] = c
		}
		out = append(out, row)
		if len(out) == nrows {
			break
		}
	}
	for len(out) < nrows {
		out = append(out, make([]Cell, cols))
	}
	return out, nil
}

func attrBits(s libghostty.Style) uint16 {
	var a uint16
	if s.Bold() {
		a |= AttrBold
	}
	if s.Faint() {
		a |= AttrFaint
	}
	if s.Italic() {
		a |= AttrItalic
	}
	if s.Underline() != libghostty.UnderlineNone {
		a |= AttrUnderline
	}
	if s.Blink() {
		a |= AttrBlink
	}
	if s.Inverse() {
		a |= AttrInverse
	}
	if s.Strikethrough() {
		a |= AttrStrike
	}
	return a
}

// Close releases the four C handles in the reverse order they were created.
// Terminal.Close returns nothing.
func (t *Term) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	t.rc.Close()
	t.ri.Close()
	t.rs.Close()
	t.term.Close()
}
