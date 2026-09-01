package gate

import (
	"strings"

	"daisugi-verify/internal/pystr"
)

// isPySpace is Python's str.isspace for one character.
func isPySpace(r rune) bool { return pystr.IsSpace(r) }

// pyStrip is Python's str.strip() with no argument.
func pyStrip(s string) string { return pystr.Strip(s) }

// pySplit is Python's str.split() with no argument.
func pySplit(s string) []string { return pystr.Split(s) }

// pyLen is Python's len(str): the number of code points.
func pyLen(s string) int { return pystr.Len(s) }

// pyHead is Python's s[:n] on code points.
func pyHead(s string, n int) string { return pystr.Slice(s, 0, n) }

// isASCII reports whether every byte of s is ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// safeSessionID is hook._safe_session_id on a string: every code point
// outside [A-Za-z0-9._-] becomes "_", dots are stripped from both ends,
// and the result is cut to 128 code points, or "no-session" when empty.
func safeSessionID(raw string) string {
	var b strings.Builder
	for _, r := range pystr.Runes(raw) {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	// Every code point is ASCII now, so bytes are code points.
	s := strings.Trim(b.String(), ".")
	if len(s) > 128 {
		s = s[:128]
	}
	if s == "" {
		return "no-session"
	}
	return s
}
