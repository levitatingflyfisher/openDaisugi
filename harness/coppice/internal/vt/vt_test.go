package vt

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

func skipWithoutLib(t *testing.T) {
	t.Helper()
	toolchain.RequireOrSkip(t)
}

func TestFeedAndPlainScreen(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("Hello, \x1b[1;32mworld\x1b[0m!\r\n")); err != nil {
		t.Fatal(err)
	}
	got, err := term.PlainScreen()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Hello, world!") {
		t.Fatalf("PlainScreen() = %q, want it to contain %q", got, "Hello, world!")
	}
}

func TestViewportCarriesTextAndStyle(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	// Bold red "AB" then a plain "C".
	if err := term.Feed([]byte("\x1b[1;38;2;255;0;0mAB\x1b[0mC")); err != nil {
		t.Fatal(err)
	}
	rows, err := term.Viewport()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("Viewport() has %d rows, want 3", len(rows))
	}
	if len(rows[0]) != 20 {
		t.Fatalf("row 0 has %d cells, want 20", len(rows[0]))
	}
	if rows[0][0].Text != "A" || rows[0][1].Text != "B" || rows[0][2].Text != "C" {
		t.Fatalf("row 0 first cells = %q %q %q, want A B C",
			rows[0][0].Text, rows[0][1].Text, rows[0][2].Text)
	}
	if rows[0][0].Attrs&AttrBold == 0 {
		t.Fatal("cell A has no AttrBold, want bold")
	}
	if rows[0][2].Attrs&AttrBold != 0 {
		t.Fatal("cell C has AttrBold, want the SGR reset to have cleared it")
	}
	if rows[0][0].FG == nil || *rows[0][0].FG != (RGB{R: 255}) {
		t.Fatalf("cell A FG = %v, want red", rows[0][0].FG)
	}
}

// A never-written cell and a cell where the user actually typed a space must
// stay distinguishable, so a client diffing two frames can tell "nothing
// happened here" from "a space was typed here".
func TestViewportPreservesTypedSpaces(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(10, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("a b")); err != nil {
		t.Fatal(err)
	}
	rows, err := term.Viewport()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][1].Text != " " {
		t.Fatalf("typed-space cell = %q, want %q", rows[0][1].Text, " ")
	}
	if rows[0][5].Text != "" {
		t.Fatalf("never-written cell = %q, want empty", rows[0][5].Text)
	}
}

// Cursor() on a live, unclosed terminal must report where the cursor
// actually is, not just survive a closed one (TestSizeAndCursorReportZeroRatherThanGuessing
// covers the closed case).
func TestCursorReflectsFedText(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("Hello")); err != nil {
		t.Fatal(err)
	}
	x, y, visible := term.Cursor()
	if !visible {
		t.Fatal("Cursor() visible = false, want true after feeding plain text")
	}
	if x != 5 || y != 0 {
		t.Fatalf("Cursor() = (%d, %d), want (5, 0) after writing 5 columns on row 0", x, y)
	}
}

// A cell holding the terminal's default colour must report FG == nil, so a
// client renders it with its own palette rather than a colour libghostty
// invented; a cell that actually received an SGR colour must report it.
func TestCellFGIsNilForDefaultColour(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(10, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("a\x1b[38;2;10;20;30mb")); err != nil {
		t.Fatal(err)
	}
	rows, err := term.Viewport()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].FG != nil {
		t.Fatalf("default-coloured cell FG = %v, want nil", *rows[0][0].FG)
	}
	if rows[0][1].FG == nil || *rows[0][1].FG != (RGB{R: 10, G: 20, B: 30}) {
		t.Fatalf("coloured cell FG = %v, want {10 20 30}", rows[0][1].FG)
	}
}

func TestResizeChangesReportedSize(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Resize(40, 10); err != nil {
		t.Fatal(err)
	}
	cols, rows := term.Size()
	if cols != 40 || rows != 10 {
		t.Fatalf("Size() = %d x %d, want 40 x 10", cols, rows)
	}
}

func TestTitleAndProgressComeFromOSC(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("\x1b]0;claude working\x07\x1b]9;4;1;40\x07")); err != nil {
		t.Fatal(err)
	}
	if got := term.Title(); got != "claude working" {
		t.Fatalf("Title() = %q, want %q", got, "claude working")
	}
	// Herdr's osc_progress region is the OSC 9 payload without the "9;" prefix,
	// which is what its claude.toml rule `^4;0` matches.
	if got := term.Progress(); !strings.HasPrefix(got, "4;1") {
		t.Fatalf("Progress() = %q, want it to start with 4;1", got)
	}
}

// The binding documents Progress == -1 as "omitted". OSC 9;4;<state> with no
// percent field must not grow a fake "-1" third field: Herdr's rules match
// against the literal payload, and a manifest rule for bare "4;0" would never
// fire against "4;0;-1".
func TestProgressOmitsPercentWhenNotReported(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	cases := []struct {
		osc  string
		want string
	}{
		{"\x1b]9;4;0\x07", "4;0"},
		{"\x1b]9;4;3\x07", "4;3"},
		{"\x1b]9;4;1;40\x07", "4;1;40"},
	}
	for _, c := range cases {
		if err := term.Feed([]byte(c.osc)); err != nil {
			t.Fatal(err)
		}
		if got := term.Progress(); got != c.want {
			t.Errorf("after Feed(%q): Progress() = %q, want %q", c.osc, got, c.want)
		}
	}
}

func TestPlainAllIncludesScrollback(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 3, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		if err := term.Feed([]byte(line + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	screen, err := term.PlainScreen()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screen, "one") {
		t.Fatalf("PlainScreen() = %q, want %q to have scrolled off", screen, "one")
	}
	all, err := term.PlainAll()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "one") {
		t.Fatalf("PlainAll() = %q, want it to contain the scrolled-off %q", all, "one")
	}
}

// PlainScreenUnwrapped joins a logical line the terminal soft-wrapped across
// several physical rows, while PlainScreen keeps it split the way the screen
// actually shows it.
func TestPlainScreenUnwrappedJoinsSoftWrappedLines(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(10, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	long := "abcdefghijklmnopqrstuvwxy" // 25 chars: wraps across 3 rows of width 10.
	if err := term.Feed([]byte(long)); err != nil {
		t.Fatal(err)
	}
	wrapped, err := term.PlainScreen()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wrapped, long) {
		t.Fatalf("PlainScreen() = %q, want the long line split across wrapped rows, not joined", wrapped)
	}
	unwrapped, err := term.PlainScreenUnwrapped()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unwrapped, long) {
		t.Fatalf("PlainScreenUnwrapped() = %q, want it to contain the joined line %q", unwrapped, long)
	}
}

// PlainScreenUnwrapped is bounded to the active screen (see PINS.md), so when
// part of a soft-wrapped logical line has scrolled into scrollback, the
// active screen no longer contains the whole line. This test pins down the
// answer: PlainScreenUnwrapped joins whatever physical rows remain on screen
// and says nothing about the missing prefix; it does not reach into history
// to reconstruct the original line, and PlainAll is what a caller needs for
// that. Recorded in PINS.md because it surprised the person who wrote it.
func TestPlainScreenUnwrappedDoesNotReachIntoScrollback(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(10, 3, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	long := "abcdefghijklmnopqrstuvwxy" // 25 chars: fills all 3 rows of width 10 exactly.
	if err := term.Feed([]byte(long)); err != nil {
		t.Fatal(err)
	}
	// One more row of content scrolls the wrapped line's first physical row
	// ("abcdefghij") into scrollback.
	if err := term.Feed([]byte("\r\nZZZZZ")); err != nil {
		t.Fatal(err)
	}
	screenUnwrapped, err := term.PlainScreenUnwrapped()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screenUnwrapped, "abcdefghij") {
		t.Fatalf("PlainScreenUnwrapped() = %q, want the scrolled-off prefix gone", screenUnwrapped)
	}
	if !strings.Contains(screenUnwrapped, "klmnopqrstuvwxy") {
		t.Fatalf("PlainScreenUnwrapped() = %q, want it to still join the remaining visible tail", screenUnwrapped)
	}
	// PlainAll does not unwrap (it formats with unwrap=false), so it does not
	// rejoin the line either; it only proves the scrolled-off prefix still
	// exists somewhere, in scrollback, for a caller who wants it.
	all, err := term.PlainAll()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "abcdefghij") {
		t.Fatalf("PlainAll() = %q, want it to still contain the scrolled-off prefix", all)
	}
}

// framePump calls Viewport up to sixty times a second per attached client. If
// each call allocated a render state, an iterator and a cell cursor without
// closing them, the daemon would leak C memory until it died. This test does
// not measure memory; it pins the design: the handles are fields on Term.
func TestViewportReusesItsHandles(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	if term.rs == nil || term.ri == nil || term.rc == nil {
		t.Fatal("New did not allocate the render-state handles on the Term")
	}
	rs, ri, rc := term.rs, term.ri, term.rc
	var last [][]Cell
	for i := 0; i < 200; i++ {
		last, err = term.Viewport()
		if err != nil {
			t.Fatal(err)
		}
	}
	if term.rs != rs || term.ri != ri || term.rc != rc {
		t.Fatal("Viewport replaced a handle, so it is allocating per call")
	}
	var row strings.Builder
	for _, c := range last[0] {
		row.WriteString(c.Text)
	}
	if !strings.Contains(row.String(), "still here") {
		t.Fatalf("row 0 after 200 Viewport() calls = %q, want it to still contain %q", row.String(), "still here")
	}
}

// Every method that reads terminal state must survive an accessor that
// errors. None of them may return a plausible lie.
func TestSizeAndCursorReportZeroRatherThanGuessing(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(20, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	term.Close()
	// After Close the handles are gone; the accessors must not panic.
	_, _ = term.Size()
	_, _, _ = term.Cursor()
	_ = term.Title()
}

// A double-width glyph occupies two grid columns, not one: libghostty's own
// Viewport places the glyph itself at one column and an EMPTY spacer cell
// right after it, and only then does the next real character land - never
// crowding into the spacer's own column. internal/attach's renderer depends
// on exactly this shape (RenderTo skips a spacer cell right after a wide
// glyph rather than printing an extra space for it); this is the test that
// pins the shape it depends on, against the real library.
func TestViewportPlacesAnEmptySpacerAfterADoubleWidthGlyph(t *testing.T) {
	skipWithoutLib(t)
	term, err := New(10, 2, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Feed([]byte("中x")); err != nil {
		t.Fatal(err)
	}
	rows, err := term.Viewport()
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	if row[0].Text != "中" {
		t.Fatalf("row[0].Text = %q, want the wide glyph itself", row[0].Text)
	}
	if row[1].Text != "" {
		t.Fatalf("row[1].Text = %q, want an empty spacer cell right after the wide glyph", row[1].Text)
	}
	if row[2].Text != "x" {
		t.Fatalf("row[2].Text = %q, want x at column 2, not crowded into the spacer's column 1", row[2].Text)
	}
}
