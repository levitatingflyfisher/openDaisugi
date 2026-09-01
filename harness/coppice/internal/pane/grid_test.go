package pane

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
	"github.com/opendaisugi/coppice/internal/vt"
)

func skipWithoutLib(t *testing.T) {
	t.Helper()
	toolchain.RequireOrSkip(t)
}

// golden compares got against testdata/vt/<name>.golden.txt, and rewrites the
// file when COPPICE_UPDATE_GOLDEN is set.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "vt", name+".golden.txt")
	if os.Getenv("COPPICE_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v. Create it with COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/", err)
	}
	if got != string(want) {
		t.Fatalf("grid for %s differs.\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func feedStream(t *testing.T, g *Grid, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "vt", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write(b); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenGrids(t *testing.T) {
	skipWithoutLib(t)
	cases := []struct{ stream string }{
		{"basic.bin"}, {"wrap.bin"}, {"scroll.bin"}, {"erase.bin"},
		{"osc.bin"}, {"promptbox.bin"},
	}
	for _, c := range cases {
		t.Run(c.stream, func(t *testing.T) {
			g, err := NewGrid(20, 6)
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			feedStream(t, g, c.stream)
			got, err := g.Read(ReadVisible)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, strings.TrimSuffix(c.stream, ".bin"), got)
		})
	}
}

func TestOSCStreamSetsTitleAndProgress(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	feedStream(t, g, "osc.bin")
	if got := g.Title(); got != "claude working" {
		t.Fatalf("Title() = %q, want %q", got, "claude working")
	}
	if got := g.Progress(); !strings.HasPrefix(got, "4;1") {
		t.Fatalf("Progress() = %q, want it to start with 4;1", got)
	}
}

// Detection used to be capped at the
// bottom 12 non-empty rows. It is now the whole unwrapped screen, so a
// caller writing more than 12 lines into a grid taller than that must still
// see the earliest ones, not just the tail.
func TestReadDetectionIsTheWholeUnwrappedScreenNotAWindow(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for i := 1; i <= 25; i++ {
		if _, err := g.Write([]byte(strings.Repeat("x", 3) + string(rune('a'+i%26)) + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := g.Read(ReadDetection)
	if err != nil {
		t.Fatal(err)
	}
	want, err := g.term.PlainScreenUnwrapped()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Read(ReadDetection) = %q, want PlainScreenUnwrapped()'s own %q", got, want)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) < 25 {
		t.Fatalf("detection returned %d non-trailing-blank lines, want all 25 written, not a bottom window", len(lines))
	}
}

func TestReadRecentReachesIntoScrollback(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	feedStream(t, g, "scroll.bin")
	visible, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(visible, "l1") {
		t.Fatalf("visible = %q, want l1 to have scrolled off a 4 row terminal", visible)
	}
	recent, err := g.Read(ReadRecent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recent, "l1") {
		t.Fatalf("recent = %q, want it to reach back to l1", recent)
	}
}

func TestResizeChangesTheReportedSize(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Resize(40, 12); err != nil {
		t.Fatal(err)
	}
	c, r := g.Size()
	if c != 40 || r != 12 {
		t.Fatalf("Size() = %d x %d, want 40 x 12", c, r)
	}
}

func TestFirstFrameIsFullAndTheSecondCarriesOnlyChangedRows(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.Write([]byte("aa\r\nbb\r\n")); err != nil {
		t.Fatal(err)
	}

	fs := NewFrameState()
	first, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the first frame reported no change")
	}
	if first.Seq != 1 {
		t.Fatalf("first frame seq = %d, want 1", first.Seq)
	}
	if len(first.RowsChanged) != 4 {
		t.Fatalf("first frame carried %d rows, want all 4", len(first.RowsChanged))
	}

	// Nothing moved.
	if _, changed, err := fs.Next("w1:p1", g); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Fatal("an unchanged grid produced a frame")
	}

	// One row changes.
	if _, err := g.Write([]byte("cc")); err != nil {
		t.Fatal(err)
	}
	second, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a changed grid produced no frame")
	}
	if second.Seq != 2 {
		t.Fatalf("second frame seq = %d, want 2", second.Seq)
	}
	if len(second.RowsChanged) != 1 {
		t.Fatalf("second frame carried %d rows, want exactly the 1 that changed", len(second.RowsChanged))
	}
	if _, ok := second.RowsChanged[2]; !ok {
		t.Fatalf("second frame changed rows %v, want row 2", keysOf(second.RowsChanged))
	}
}

func TestResizeForcesAFullFrame(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	fs := NewFrameState()
	if _, _, err := fs.Next("w1:p1", g); err != nil {
		t.Fatal(err)
	}
	if err := g.Resize(20, 8); err != nil {
		t.Fatal(err)
	}
	f, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(f.RowsChanged) != 8 {
		t.Fatalf("after a resize: changed=%v rows=%d, want a full 8 row frame",
			changed, len(f.RowsChanged))
	}
}

// TestAFrameIsInternallyConsistentUnderConcurrentResize guards against a
// frame whose Cols/Rows come from one instant and whose RowsChanged keys (and
// row widths) come from another. One goroutine resizes the grid back and
// forth; another asks for frames. Every frame handed back, no matter when it
// lands relative to a resize, must describe itself: every changed row index
// must be in range for that same frame's Rows, and every changed row must
// have exactly that frame's Cols cells. This test never writes any text, so
// a size change is the only thing that can ever produce a frame, and every
// frame Next produces here is therefore a full frame - so len(RowsChanged)
// must equal Rows on every one of them, not just some.
func TestAFrameIsInternallyConsistentUnderConcurrentResize(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	fs := NewFrameState()
	var (
		mu     sync.Mutex
		frames []proto.Frame
	)
	sizes := [2][2]int{{20, 4}, {30, 8}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			sz := sizes[i%2]
			if err := g.Resize(sz[0], sz[1]); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			f, changed, err := fs.Next("w1:p1", g)
			if err != nil {
				t.Error(err)
				return
			}
			if !changed {
				continue
			}
			mu.Lock()
			frames = append(frames, f)
			mu.Unlock()
		}
	}()
	wg.Wait()

	if len(frames) == 0 {
		t.Fatal("no frames were collected; the test can't check anything")
	}
	for _, f := range frames {
		for y, row := range f.RowsChanged {
			if y < 0 || y >= f.Rows {
				t.Fatalf("frame seq %d: RowsChanged key %d is out of range for Rows=%d", f.Seq, y, f.Rows)
			}
			if len(row) != f.Cols {
				t.Fatalf("frame seq %d: row %d has %d cells, want Cols=%d", f.Seq, y, len(row), f.Cols)
			}
		}
		if len(f.RowsChanged) != f.Rows {
			t.Fatalf("frame seq %d: RowsChanged has %d rows, want a full frame of Rows=%d "+
				"(no text was ever written, so every emitted frame here must be full)",
				f.Seq, len(f.RowsChanged), f.Rows)
		}
	}
}

// TestACursorOnlyMoveEmitsAnEmptyRowsChangedMap covers the frame that carries
// no row changes at all because only the cursor moved. RowsChanged must still
// be a non-nil, empty map so the wire encoding is "rows_changed":{}, never
// "rows_changed":null - a client that always expects an object would choke on
// null.
func TestACursorOnlyMoveEmitsAnEmptyRowsChangedMap(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.Write([]byte("aa\r\nbb\r\n")); err != nil {
		t.Fatal(err)
	}

	fs := NewFrameState()
	first, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the first frame reported no change")
	}

	// Move the cursor home. No cell content changes.
	if _, err := g.Write([]byte("\x1b[H")); err != nil {
		t.Fatal(err)
	}
	second, changed, err := fs.Next("w1:p1", g)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a cursor-only move produced no frame")
	}
	if second.Seq != first.Seq+1 {
		t.Fatalf("second frame seq = %d, want %d", second.Seq, first.Seq+1)
	}
	if second.RowsChanged == nil {
		t.Fatal("RowsChanged is nil, want a non-nil empty map")
	}
	if len(second.RowsChanged) != 0 {
		t.Fatalf("RowsChanged has %d rows, want 0 for a cursor-only move", len(second.RowsChanged))
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"rows_changed":{}`) {
		t.Fatalf("json = %s, want it to contain \"rows_changed\":{}, not null", b)
	}
}

// TestHexOfFormatsColoursAsLowerHexWithAHashPrefix pins hexOf's exact wire
// format (a lower-case "#rrggbb", "" for no colour) so a change to how it's
// computed - e.g. away from fmt.Sprintf on the 60 Hz frame path - can't
// silently change what a client receives.
func TestHexOfFormatsColoursAsLowerHexWithAHashPrefix(t *testing.T) {
	if got := hexOf(nil); got != "" {
		t.Fatalf("hexOf(nil) = %q, want \"\"", got)
	}
	cases := []struct {
		c    vt.RGB
		want string
	}{
		{vt.RGB{R: 0, G: 0, B: 0}, "#000000"},
		{vt.RGB{R: 255, G: 255, B: 255}, "#ffffff"},
		{vt.RGB{R: 0x1a, G: 0x2b, B: 0x3c}, "#1a2b3c"},
		{vt.RGB{R: 5, G: 10, B: 250}, "#050afa"},
	}
	for _, c := range cases {
		if got := hexOf(&c.c); got != c.want {
			t.Fatalf("hexOf(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

func keysOf(m map[int][]proto.Cell) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// A pane
// whose process exited keeps its grid, so its final screen stays readable;
// only an operator close frees it. Grid needs a closed flag so that close is
// safe to call more than once, and so every method that would otherwise call
// into the freed C-backed vt.Term after Close reports it instead of making
// that call.
//
// Title, Progress and Size keep their existing signatures rather than
// gaining an error return: Size never touches the C-backed term at all (it
// only reads Grid's own cached ints, so it stays safe regardless of closed),
// and Title/Progress keep their current single-value
// signatures, since other code already calls them that way
// (`OSCTitle: lp.Grid.Title()`) - a two-value return
// would not compile against that already-written code. Title/Progress
// still gain the closed check; they just report "" instead of an error,
// which is the pre-existing shape of "there is nothing to report" for both
// (a Term with no OSC 0/9977 sequence yet also reports "").
func TestEveryGridMethodAfterCloseIsSafe(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := g.Resize(30, 6); err != nil {
		t.Fatal(err)
	}

	g.Close()
	g.Close() // idempotent: must not panic or double-free the C handles

	if _, err := g.Write([]byte("x")); !errors.Is(err, ErrGridClosed) {
		t.Fatalf("Write after Close = %v, want ErrGridClosed", err)
	}
	if err := g.Resize(10, 3); !errors.Is(err, ErrGridClosed) {
		t.Fatalf("Resize after Close = %v, want ErrGridClosed", err)
	}
	if _, err := g.Read(ReadVisible); !errors.Is(err, ErrGridClosed) {
		t.Fatalf("Read after Close = %v, want ErrGridClosed", err)
	}
	if _, _, _, _, err := g.Snapshot(); !errors.Is(err, ErrGridClosed) {
		t.Fatalf("Snapshot after Close = %v, want ErrGridClosed", err)
	}
	if got := g.Title(); got != "" {
		t.Fatalf("Title after Close = %q, want empty (no C call on a freed terminal)", got)
	}
	if got := g.Progress(); got != "" {
		t.Fatalf("Progress after Close = %q, want empty (no C call on a freed terminal)", got)
	}
	// Size never touches the C-backed term, so it stays whatever Resize last
	// told it - reading it after Close is inherently safe and reports the
	// last known size rather than an error.
	if c, r := g.Size(); c != 30 || r != 6 {
		t.Fatalf("Size after Close = %d x %d, want the last known 30 x 6", c, r)
	}
}
