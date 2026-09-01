// Package pane owns what a pane is: a grid fed by bytes, plus either a PTY or a
// headless adapter behind it. A client cannot tell the two kinds apart except
// by a badge, because both arrive as frames.
package pane

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/vt"
)

// ErrGridClosed is what every Grid method that would otherwise touch the
// C-backed vt.Term returns once Close has run. A pane whose process exited
// keeps its grid so its final screen stays readable, and only an operator
// close frees it - but once freed, calling back into it is undefined
// behavior, not a slow path worth attempting.
var ErrGridClosed = errors.New("this grid is closed")

type ReadSource string

const (
	ReadVisible   ReadSource = "visible"
	ReadRecent    ReadSource = "recent"
	ReadDetection ReadSource = "detection"
)

// RecentRows is how far back `read recent` reaches, in lines.
const RecentRows = 200

// scrollbackLines is the per-pane scrollback budget. It bounds memory per pane
// and must stay comfortably above RecentRows.
const scrollbackLines = 2000

// Grid is one terminal. It is safe for concurrent use.
type Grid struct {
	mu     sync.Mutex
	term   *vt.Term
	cols   int
	rows   int
	closed bool

	// lastWrite is the unix nanosecond clock of the last Write that fed
	// bytes into the terminal, or zero when none has. It is atomic, not
	// under mu, so a reader can ask without waiting behind a feed.
	lastWrite atomic.Int64

	// firstWrite is the unix nanosecond clock of the first Write that fed
	// bytes, or zero when none has.
	firstWrite atomic.Int64

	// paste is true while the program in the pane has bracketed paste
	// on: the last mode sequence it wrote was CSI ?2004h, not CSI ?2004l.
	paste atomic.Bool
}

// pasteOn and pasteOff are the sequences that turn bracketed paste on and
// off.
const (
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"
)

func NewGrid(cols, rows int) (*Grid, error) {
	if cols <= 0 || rows <= 0 {
		return nil, fmt.Errorf("grid size must be positive, got %dx%d", cols, rows)
	}
	term, err := vt.New(uint16(cols), uint16(rows), scrollbackLines)
	if err != nil {
		return nil, err
	}
	return &Grid{term: term, cols: cols, rows: rows}, nil
}

func (g *Grid) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, ErrGridClosed
	}
	if err := g.term.Feed(p); err != nil {
		return 0, err
	}
	if len(p) > 0 {
		now := time.Now().UnixNano()
		g.lastWrite.Store(now)
		g.firstWrite.CompareAndSwap(0, now)
	}
	if on, off := strings.LastIndex(string(p), pasteOn), strings.LastIndex(string(p), pasteOff); on != off {
		g.paste.Store(on > off)
	}
	return len(p), nil
}

// FirstWrite reports when the grid first took bytes, and false when it
// never has.
func (g *Grid) FirstWrite() (time.Time, bool) {
	ns := g.firstWrite.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// PasteMode reports whether the program in the pane has bracketed paste
// on, so text sent between the paste marks reads as one block and its
// line breaks do not submit it. A sequence split across two writes is
// missed.
func (g *Grid) PasteMode() bool {
	return g.paste.Load()
}

// LastWrite reports when the grid last took bytes, and false when it never
// has. It reads one atomic, so it is safe on a closed grid too.
func (g *Grid) LastWrite() (time.Time, bool) {
	ns := g.lastWrite.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

func (g *Grid) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("grid size must be positive, got %dx%d", cols, rows)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrGridClosed
	}
	if err := g.term.Resize(uint16(cols), uint16(rows)); err != nil {
		return err
	}
	g.cols, g.rows = cols, rows
	return nil
}

// Size never touches the C-backed term - it only reads Grid's own cached
// ints - so it stays safe, and keeps reporting the last known size, even
// after Close.
func (g *Grid) Size() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cols, g.rows
}

// Title and Progress report "" once closed rather than gaining an error
// return: both keep their existing single-value signature, since other
// code already calls them that way
// (`OSCTitle: lp.Grid.Title()`) - a two-value return would not compile
// against that already-written code. "" is also the pre-existing shape of
// "nothing to report" for both (a Term with no OSC sequence yet reports the
// same), so a closed grid reporting it is not a new case for a caller to
// handle.
func (g *Grid) Title() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ""
	}
	return g.term.Title()
}

func (g *Grid) Progress() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ""
	}
	return g.term.Progress()
}

// Read renders the pane as plain text. detection is the whole screen the
// manifest evaluator sees, unwrapped, so `pane read --source detection` is a
// debugging view of the exact input a rule's own region slices from (this
// used to hand the evaluator only the bottom 12
// non-empty lines, which silently narrowed every rule asking for more - see
// internal/detect/README.md). The lock is held for the whole call, not just
// the closed check: releasing it early would let a concurrent Close free the
// C handles between the check and the call into g.term below.
func (g *Grid) Read(src ReadSource) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return "", ErrGridClosed
	}
	switch src {
	case ReadVisible:
		return g.term.PlainScreen()
	case ReadRecent:
		all, err := g.term.PlainAll()
		if err != nil {
			return "", err
		}
		return lastLines(all, RecentRows), nil
	case ReadDetection:
		return g.term.PlainScreenUnwrapped()
	default:
		return "", fmt.Errorf("read source %q is not visible, recent or detection", src)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Snapshot is one atomic view of the grid: the viewport cells, the cursor,
// and the size they were taken at. Viewport and Cursor are two separate calls
// into vt.Term, each independently locked at that layer, so without a lock
// here spanning both, a Resize landing between them would hand back a
// viewport from one size and a cursor from another. Holding g.mu for the
// whole read closes that: Resize also holds g.mu for its entire body, so the
// two can never interleave. cols and nrows come from the shape of the
// viewport slice itself, not from a separate g.cols/g.rows read - vt.Term's
// Viewport always returns exactly nrows rows of cols cells each, so deriving
// the size this way can never disagree with the rows actually returned.
func (g *Grid) Snapshot() (rows [][]proto.Cell, cursor [2]int, cols, nrows int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, [2]int{}, 0, 0, ErrGridClosed
	}
	vpRows, err := g.term.Viewport()
	if err != nil {
		return nil, [2]int{}, 0, 0, err
	}
	out := make([][]proto.Cell, len(vpRows))
	for y, row := range vpRows {
		cells := make([]proto.Cell, len(row))
		for x, c := range row {
			cells[x] = proto.Cell{
				Text: c.Text, FG: hexOf(c.FG), BG: hexOf(c.BG), Attrs: c.Attrs,
			}
		}
		out[y] = cells
	}
	cx, cy, _ := g.term.Cursor()
	nrows = len(out)
	if nrows > 0 {
		cols = len(out[0])
	}
	return out, [2]int{int(cx), int(cy)}, cols, nrows, nil
}

const hexDigits = "0123456789abcdef"

// hexOf renders one colour as "#rrggbb", or "" for no colour. Snapshot calls
// this per non-empty cell on the 60 Hz frame path, so it writes into a fixed
// 7-byte array instead of going through fmt.Sprintf's formatting machinery.
func hexOf(c *vt.RGB) string {
	if c == nil {
		return ""
	}
	var buf [7]byte
	buf[0] = '#'
	buf[1] = hexDigits[c.R>>4]
	buf[2] = hexDigits[c.R&0xf]
	buf[3] = hexDigits[c.G>>4]
	buf[4] = hexDigits[c.G&0xf]
	buf[5] = hexDigits[c.B>>4]
	buf[6] = hexDigits[c.B&0xf]
	return string(buf[:])
}

// Close is idempotent: a second call is a no-op rather than a second attempt
// to free the same C handles.
func (g *Grid) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	g.term.Close()
}

// FrameState is one client's view of one pane. The sequence number and the row
// hashes are per client, because two clients attach at different moments and
// each needs its own baseline.
type FrameState struct {
	seq    uint64
	hashes []uint64
	cols   int
	rows   int
	cursor [2]int
	// force makes the next frame a full one even when nothing changed. A
	// client that paused, or that missed dropped frames, cannot apply a
	// delta to the grid it holds.
	force bool
}

func NewFrameState() *FrameState { return &FrameState{} }

// ForceFull makes the next call to Next build a full frame, whether or not
// the grid changed.
func (fs *FrameState) ForceFull() { fs.force = true }

// Next builds the frame to send this client. The second return value is false
// when nothing changed, so the caller sends nothing at all. The frame's Full
// field reports whether it carries every row.
func (fs *FrameState) Next(paneID string, g *Grid) (proto.Frame, bool, error) {
	cells, cursor, cols, rows, err := g.Snapshot()
	if err != nil {
		return proto.Frame{}, false, err
	}
	full := fs.force || fs.seq == 0 || fs.cols != cols || fs.rows != rows || len(fs.hashes) != len(cells)

	changed := map[int][]proto.Cell{}
	next := make([]uint64, len(cells))
	for y, row := range cells {
		next[y] = hashRow(row)
		if full || next[y] != fs.hashes[y] {
			changed[y] = row
		}
	}
	if len(changed) == 0 && cursor == fs.cursor {
		return proto.Frame{}, false, nil
	}
	fs.seq++
	fs.hashes = next
	fs.cols, fs.rows = cols, rows
	fs.cursor = cursor
	fs.force = false
	return proto.Frame{
		Event: "frame", Pane: paneID, Seq: fs.seq, Cols: cols, Rows: rows,
		Cursor: cursor, RowsChanged: changed, Full: full,
	}, true, nil
}

func hashRow(row []proto.Cell) uint64 {
	h := fnv.New64a()
	for _, c := range row {
		_, _ = h.Write([]byte(c.Text))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(c.FG))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(c.BG))
		_, _ = h.Write([]byte{byte(c.Attrs), byte(c.Attrs >> 8), 0})
	}
	return h.Sum64()
}
