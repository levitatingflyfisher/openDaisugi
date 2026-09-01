package tui

import (
	"bytes"
	"strconv"
)

// Click is one mouse report in SGR 1006 form. Button is 0 for left, 1 for
// middle, 2 for right, and 64 or 65 for the wheel. X and Y are 1-based
// terminal columns and rows. Release is true for a button release.
type Click struct {
	Button  int
	X, Y    int
	Release bool
}

// sgrMousePrefix opens every SGR 1006 mouse report.
const sgrMousePrefix = "\x1b[<"

// ParseSGR parses one SGR 1006 mouse report, ESC [ < B ; X ; Y then M for
// a press or m for a release. Anything else is refused.
func ParseSGR(b []byte) (Click, bool) {
	if !bytes.HasPrefix(b, []byte(sgrMousePrefix)) || len(b) < len(sgrMousePrefix)+1 {
		return Click{}, false
	}
	final := b[len(b)-1]
	if final != 'M' && final != 'm' {
		return Click{}, false
	}
	parts := bytes.Split(b[len(sgrMousePrefix):len(b)-1], []byte(";"))
	if len(parts) != 3 {
		return Click{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(string(p))
		if err != nil || n < 0 {
			return Click{}, false
		}
		nums[i] = n
	}
	return Click{Button: nums[0], X: nums[1], Y: nums[2], Release: final == 'm'}, true
}

// HitRow returns the cursor-order index of the roster row drawn on screen
// row y, 1-based. It is false for a row that holds no roster row.
func HitRow(m *Model, y int) (int, bool) {
	if y < 1 || y > len(m.RowAt) || m.RowAt[y-1] < 0 {
		return 0, false
	}
	return m.RowAt[y-1], true
}

// HitTile returns the shown slot index drawn under screen column x,
// 1-based. It is false for a column outside every tile.
func HitTile(m *Model, x int) (int, bool) {
	if x < 1 || x > len(m.TileAt) || m.TileAt[x-1] < 0 {
		return 0, false
	}
	return m.TileAt[x-1], true
}
