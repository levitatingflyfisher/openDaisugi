// Package textwidth measures text in terminal cells. A wide glyph, such as
// a CJK ideograph, takes two cells. A control rune takes none. attach and
// the floor both lay out lines with these functions so a wide label never
// pushes a line past the screen edge.
package textwidth

import (
	"strings"
	"unicode/utf8"
)

// Ellipsis is the one cell Truncate ends a cut string with.
const Ellipsis = "…"

// IsWide reports whether text is a single rune a terminal renders two
// cells wide. It covers the CJK, Hangul, and fullwidth blocks. It returns
// false for anything that is not exactly one rune. Inside the CJK range
// the hexagram symbols and the ideographic half fill space are narrow.
func IsWide(text string) bool {
	r, size := utf8.DecodeRuneInString(text)
	if size != len(text) || size == 0 {
		return false
	}
	return wideRune(r)
}

func wideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // Hangul Jamo
		return true
	case r >= 0x2E80 && r <= 0xA4CF: // CJK radicals, Kangxi, hiragana, katakana, CJK ideographs
		if r == 0x303F || (r >= 0x4DC0 && r <= 0x4DFF) {
			return false
		}
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // Hangul syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
		return true
	case r >= 0xFF00 && r <= 0xFF60: // fullwidth forms
		return true
	case r >= 0xFFE0 && r <= 0xFFE6: // fullwidth signs
		return true
	case r >= 0x20000 && r <= 0x3FFFD: // CJK unified ideographs, extensions
		return true
	}
	return false
}

// runeWidth is the cells one rune takes: 0 for a control rune, 2 for a
// wide glyph, 1 for everything else.
func runeWidth(r rune) int {
	if r < 0x20 || r == 0x7f {
		return 0
	}
	if wideRune(r) {
		return 2
	}
	return 1
}

// Width is the number of cells s takes on one terminal line.
func Width(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// Truncate cuts s to at most max cells. A cut string ends with Ellipsis.
// When max is one or less the result is at most one cell with no ellipsis,
// and when max is zero or less the result is empty.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if Width(s) <= max {
		return s
	}
	if max == 1 {
		for _, r := range s {
			if runeWidth(r) <= 1 {
				return string(r)
			}
			return ""
		}
		return ""
	}
	room := max - Width(Ellipsis)
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > room {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteString(Ellipsis)
	return b.String()
}

// Pad returns s at exactly w cells: right padded with spaces when shorter,
// cut with Truncate when longer.
func Pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = Truncate(s, w)
	if n := w - Width(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// Printable drops every C0 control rune, DEL, every C1 control rune, and
// every byte that is not UTF-8 from s, so the text cannot move a
// terminal's cursor or start an escape sequence. A lone 0x9b byte is CSI
// on a terminal that reads 8-bit controls, so bad bytes go too. With max
// above zero it also cuts the result to max runes.
func Printable(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError {
			continue
		}
		if max > 0 && n == max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
