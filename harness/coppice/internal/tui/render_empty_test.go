package tui

import (
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/textwidth"
	"github.com/opendaisugi/coppice/internal/tiles"
)

// F9: the rail's first line fits the rail at its default width for any
// harness name up to eight letters, so no "…" cuts it at 160 columns.
func TestTheEmptyRailLineFitsTheRail(t *testing.T) {
	for _, name := range []string{"claude", "codex", "opencode", "pi"} {
		m := &Model{Default: name}
		lines, _ := roster(m, railCols, 6)
		want := "Nothing running. Enter opens " + name + "."
		if got := strings.TrimRight(lines[0], " "); got != want {
			t.Fatalf("%s: rail line %q, want %q", name, got, want)
		}
		if w := textwidth.Width(want); w > railCols {
			t.Fatalf("%s: %d columns, the rail has %d", name, w, railCols)
		}
	}
}

// F10: an empty window on an empty floor says how to start the default
// harness, not "select a row".
func TestAnEmptyWindowOnAnEmptyFloorSaysPressN(t *testing.T) {
	m := &Model{Default: "claude"}
	m.Tiles = tiles.New(tiles.Focus, 1)
	right := strings.Join(rightOfDivider(Render(m, 140, 20)), "\n")
	if !strings.Contains(right, "Empty. Press n to start claude.") {
		t.Fatalf("empty window does not say what to press: %q", right)
	}
	if strings.Contains(right, "Select a row") {
		t.Fatalf("empty window still says to select a row: %q", right)
	}
}
