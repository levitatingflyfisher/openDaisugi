package attach

import (
	"regexp"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func fullFrame() proto.Frame {
	return proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 5, Rows: 2, Cursor: [2]int{1, 1},
		RowsChanged: map[int][]proto.Cell{
			0: {{Text: "h"}, {Text: "i"}, {}, {}, {}},
			1: {{Text: "y"}, {Text: "o"}, {}, {}, {}},
		},
	}
}

func TestApplyFullFrameThenText(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	want := "hi\nyo"
	if got := s.Text(); got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
}

func TestApplyDiffFrameLeavesUntouchedRowsAlone(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 2, Cols: 5, Rows: 2, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{
			1: {{Text: "z"}, {Text: "z"}, {}, {}, {}},
		},
	})
	if got := s.Text(); got != "hi\nzz" {
		t.Fatalf("Text() = %q, want %q", got, "hi\nzz")
	}
}

func TestApplyResizesWhenTheFrameSizeChanges(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 3, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "a"}, {Text: "b"}, {Text: "c"}}},
	})
	if got := s.Text(); got != "abc" {
		t.Fatalf("Text() = %q, want %q", got, "abc")
	}
}

func TestRenderToPlacesTheCursorLast(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The cursor at (1,1) is row 2, column 2 in one-based ANSI.
	pos := "\x1b[2;2H"
	if !strings.HasSuffix(out, pos) {
		t.Fatalf("render does not end by placing the cursor at %q: %q", pos, out)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "yo") {
		t.Fatalf("render lost the content: %q", out)
	}
}

// The frame render path must not leave a screen the pane never had: a resize
// clears the screen once, before the new content lands, so no cell outside
// the new grid lingers on the terminal.
func TestRenderToClearsTheScreenOnResize(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(fullFrame())
	var before strings.Builder
	if err := s.RenderTo(&before); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before.String(), "\x1b[2J") {
		t.Fatalf("an ordinary redraw cleared the screen: %q", before.String())
	}

	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 3, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "a"}, {Text: "b"}, {Text: "c"}}},
	})
	var after strings.Builder
	if err := s.RenderTo(&after); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(after.String(), "\x1b[2J") {
		t.Fatalf("render after a resize does not clear the screen first: %q", after.String())
	}

	var again strings.Builder
	if err := s.RenderTo(&again); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again.String(), "\x1b[2J") {
		t.Fatalf("render cleared the screen twice for one resize: %q", again.String())
	}
}

func TestStatusLineShowsIdLabelHarnessStateAndSource(t *testing.T) {
	ev := &proto.PaneStateEvent{State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{Tool: "Bash", Summary: "rm -rf build/"}}
	got := StatusLine("w1:p3", "auth fix", "claude-code", ev, 120, "ctrl-]")
	for _, want := range []string{"w1:p3", "auth fix", "claude-code", "blocked", "gate", "Bash"} {
		if !strings.Contains(got, want) {
			t.Fatalf("StatusLine = %q, want it to contain %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "   ctrl-] leave") {
		t.Fatalf("StatusLine = %q, want it to end with the leave key", got)
	}
}

// The status line names whatever key the config chose, not a fixed one.
func TestStatusLineNamesTheConfiguredLeaveKey(t *testing.T) {
	got := StatusLine("w1:p3", "", "", nil, 80, "ctrl-a")
	if !strings.HasSuffix(got, "   ctrl-a leave") || strings.Contains(got, "ctrl-]") {
		t.Fatalf("StatusLine = %q, want it to end with ctrl-a leave", got)
	}
}

// A pane nobody has reported on says unknown, not idle.
func TestStatusLineWithNoStateSaysUnknown(t *testing.T) {
	got := StatusLine("w1:p3", "", "", nil, 80, "ctrl-]")
	if !strings.Contains(got, "unknown") {
		t.Fatalf("StatusLine = %q, want unknown", got)
	}
	if strings.Contains(got, "idle") {
		t.Fatalf("StatusLine = %q, want no idle for a pane with no source", got)
	}
}

func TestStatusLineFitsTheTerminalWidth(t *testing.T) {
	ev := &proto.PaneStateEvent{State: proto.StateWorking, Source: proto.SrcHeadless}
	got := StatusLine("w1:p3", strings.Repeat("long label ", 20), "claude-code", ev, 40, "ctrl-]")
	if n := len([]rune(stripANSI(got))); n > 40 {
		t.Fatalf("status line is %d columns, want at most 40", n)
	}
}

// A frame is data from the wire, not a value this package computed. A
// negative size must not panic the renderer.
func TestApplyClampsANegativeFrameSizeToZero(t *testing.T) {
	s := NewScreen(5, 2)
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: -1, Rows: -1, Cursor: [2]int{0, 0},
	})
	if got := s.Text(); got != "" {
		t.Fatalf("Text() = %q, want empty for a clamped zero-size screen", got)
	}
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
}

func TestStatusLineClampsANegativeWidthToZero(t *testing.T) {
	got := StatusLine("w1:p1", "", "", nil, -1, "ctrl-]")
	if got != "" {
		t.Fatalf("StatusLine with a negative width = %q, want empty", got)
	}
}

func TestRenderToAppliesBoldAndAnRGBForeground(t *testing.T) {
	s := NewScreen(3, 1)
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{
			0: {{Text: "a", Attrs: 1, FG: "#ff8800"}, {Text: "b"}, {Text: "c"}},
		},
	})
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "\x1b[0;1m") {
		t.Fatalf("a bold cell did not carry SGR 1: %q", out)
	}
	if !strings.Contains(out, "\x1b[38;2;255;136;0m") {
		t.Fatalf("the #ff8800 foreground did not carry its RGB sequence: %q", out)
	}
}

// internal/vt's own Viewport was probed directly against libghostty: feeding
// "中x" into a real terminal places "中" at column 0, an EMPTY cell at
// column 1, and "x" at column 2 - libghostty's grid already reserves a
// spacer slot for the second half of a double-width glyph. RenderTo must not
// print anything for that spacer: the terminal itself advances two columns
// rendering "中", so an explicit space for column 1's empty cell would add a
// column nothing put there, pushing "x" one terminal column to the right of
// where the grid says it belongs.
func TestRenderToDoesNotAddAColumnForASpacerAfterAWideGlyph(t *testing.T) {
	s := NewScreen(5, 1)
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 5, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{
			0: {{Text: "中"}, {Text: ""}, {Text: "x"}, {Text: ""}, {Text: ""}},
		},
	})
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
	plain := stripANSI(b.String())
	if !strings.Contains(plain, "中x") {
		t.Fatalf("RenderTo output %q does not place x immediately after the wide glyph", plain)
	}
	if strings.Contains(plain, "中 x") {
		t.Fatalf("RenderTo output %q still prints a literal space for the spacer cell after a wide glyph", plain)
	}
}

// textwidth.IsWide's CJK range, 0x2E80-0xA4CF, used to sweep
// in two blocks whose East_Asian_Width is Neutral, not Wide: the Yijing
// hexagram symbols, U+4DC0-U+4DFF, and U+303F. A cell holding one of those,
// followed by a genuinely blank cell, must still print a literal space for
// that blank cell, the same as any other narrow glyph.
func TestRenderToStillPrintsASpaceAfterAYijingHexagramSymbol(t *testing.T) {
	s := NewScreen(3, 1)
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{
			0: {{Text: "䷀"}, {Text: ""}, {Text: "x"}},
		},
	})
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
	plain := stripANSI(b.String())
	if !strings.Contains(plain, "䷀ x") {
		t.Fatalf("RenderTo output %q, want a literal space after U+4DC0: it is narrow, outside textwidth.IsWide's true double-width blocks", plain)
	}
}

// An ordinary empty cell, one with no wide glyph before it, must still
// render as a space: only a spacer that directly follows a wide glyph is
// skipped, never a genuinely blank cell.
func TestRenderToStillPrintsASpaceForAnOrdinaryEmptyCell(t *testing.T) {
	s := NewScreen(3, 1)
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{
			0: {{Text: "a"}, {Text: ""}, {Text: "b"}},
		},
	})
	var b strings.Builder
	if err := s.RenderTo(&b); err != nil {
		t.Fatal(err)
	}
	plain := stripANSI(b.String())
	if !strings.Contains(plain, "a b") {
		t.Fatalf("RenderTo output %q, want a literal space between two narrow cells", plain)
	}
}

func TestLinesGivesOneColoredRowPerGridRow(t *testing.T) {
	s := NewScreen(5, 2)
	f := fullFrame()
	f.RowsChanged[0][0].Attrs = 1
	s.Apply(f)
	lines := s.Lines()
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2", len(lines))
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "\x1b[0m") {
			t.Fatalf("row does not end with the reset: %q", l)
		}
		if got := len([]rune(stripANSI(l))); got != 5 {
			t.Fatalf("row is %d cells, want 5: %q", got, l)
		}
	}
	if !strings.HasPrefix(lines[0], "\x1b[0;1m") {
		t.Fatalf("bold cell not colored: %q", lines[0])
	}
	if stripANSI(lines[1]) != "yo   " {
		t.Fatalf("row 1 = %q", stripANSI(lines[1]))
	}
}

func TestLinesPrintsNothingForTheSpacerAfterAWideGlyph(t *testing.T) {
	s := NewScreen(4, 1)
	s.Apply(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 4, Rows: 1,
		RowsChanged: map[int][]proto.Cell{0: {{Text: "中"}, {}, {Text: "x"}, {}}},
	})
	if got := stripANSI(s.Lines()[0]); got != "中x " {
		t.Fatalf("row = %q, want %q", got, "中x ")
	}
}

func TestLinesOnAnEmptyScreenIsEmpty(t *testing.T) {
	if n := len(NewScreen(0, 0).Lines()); n != 0 {
		t.Fatalf("%d lines, want 0", n)
	}
}
