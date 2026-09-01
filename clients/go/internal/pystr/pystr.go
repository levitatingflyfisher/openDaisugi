// Package pystr models the parts of Python's str the gate's oracle relies
// on: code-point indexing, repr, UTF-8 encoding and decoding, and the
// exceptions they raise.
//
// A Python str can hold a lone surrogate (JSON "\ud800" gives one), which
// UTF-8 cannot carry. Go strings here hold such a code point in its
// generalized UTF-8 form (three bytes, ED A0..BF xx), as WTF-8 does. Every
// helper below reads that form as the surrogate it stands for.
package pystr

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Exception is a Python exception: its class name and str(exc).
type Exception struct {
	Type string
	Msg  string
}

func (e *Exception) Error() string { return e.Type + ": " + e.Msg }

// String is str(exc).
func (e *Exception) String() string { return e.Msg }

// NewException builds an exception.
func NewException(typ, msg string) *Exception { return &Exception{Type: typ, Msg: msg} }

// RecursionError is the error Python raises past its recursion limit.
func RecursionError() *Exception {
	return &Exception{Type: "RecursionError", Msg: "maximum recursion depth exceeded"}
}

// IsSurrogate reports whether r is a UTF-16 surrogate code point.
func IsSurrogate(r rune) bool { return r >= 0xD800 && r <= 0xDFFF }

// DecodeRune reads one code point of s, a lone surrogate included.
func DecodeRune(s string) (rune, int) {
	if len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2] >= 0x80 && s[2] <= 0xBF {
		return rune(0xD000) | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), 3
	}
	return utf8.DecodeRuneInString(s)
}

// AppendRune writes r, a surrogate included, in generalized UTF-8.
func AppendRune(b []byte, r rune) []byte {
	if IsSurrogate(r) {
		return append(b, 0xED, byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(b, r)
}

// Runes is list(s).
func Runes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, n := DecodeRune(s[i:])
		out = append(out, r)
		i += n
	}
	return out
}

// FromRunes joins code points back into a string.
func FromRunes(rs []rune) string {
	b := make([]byte, 0, len(rs))
	for _, r := range rs {
		b = AppendRune(b, r)
	}
	return string(b)
}

// Len is len(s).
func Len(s string) int {
	n := 0
	for i := 0; i < len(s); {
		_, k := DecodeRune(s[i:])
		i += k
		n++
	}
	return n
}

// HasSurrogate reports whether s holds a lone surrogate.
func HasSurrogate(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
			return true
		}
	}
	return false
}

// Slice is s[i:j] with Python's index rules (negative from the end,
// clamped). Use a very large j for an open end.
func Slice(s string, i, j int) string {
	rs := Runes(s)
	n := len(rs)
	norm := func(k int) int {
		if k < 0 {
			k += n
			if k < 0 {
				k = 0
			}
		}
		if k > n {
			k = n
		}
		return k
	}
	a, b := norm(i), norm(j)
	if a >= b {
		return ""
	}
	return FromRunes(rs[a:b])
}

// EncodeUTF8 is s.encode("utf-8"): it raises UnicodeEncodeError on a lone
// surrogate.
func EncodeUTF8(s string) ([]byte, *Exception) {
	if !HasSurrogate(s) {
		return []byte(s), nil
	}
	rs := Runes(s)
	for i, r := range rs {
		if !IsSurrogate(r) {
			continue
		}
		j := i
		for j+1 < len(rs) && IsSurrogate(rs[j+1]) {
			j++
		}
		if i == j {
			return nil, NewException("UnicodeEncodeError", fmt.Sprintf(
				"'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", r, i))
		}
		return nil, NewException("UnicodeEncodeError", fmt.Sprintf(
			"'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", i, j))
	}
	return []byte(s), nil
}

// FSEncode is os.fsencode(s) on Linux: UTF-8 with surrogateescape, so a
// surrogate in U+DC80..U+DCFF becomes the byte it escapes, and any other
// surrogate raises UnicodeEncodeError, as every os call on the path does.
func FSEncode(s string) ([]byte, *Exception) {
	if !HasSurrogate(s) {
		return []byte(s), nil
	}
	rs := Runes(s)
	out := make([]byte, 0, len(s))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if !IsSurrogate(r) {
			out = utf8.AppendRune(out, r)
			continue
		}
		j := i
		for j+1 < len(rs) && IsSurrogate(rs[j+1]) {
			j++
		}
		for k := i; k <= j; k++ {
			if rs[k] < 0xDC80 || rs[k] > 0xDCFF {
				if i == j {
					return nil, NewException("UnicodeEncodeError", fmt.Sprintf(
						"'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", r, i))
				}
				return nil, NewException("UnicodeEncodeError", fmt.Sprintf(
					"'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", i, j))
			}
		}
		for k := i; k <= j; k++ {
			out = append(out, byte(rs[k]-0xDC00))
		}
		i = j
	}
	return out, nil
}

// FSDecode is os.fsdecode(b) on Linux: UTF-8 with surrogateescape, so each
// byte that does not decode becomes the surrogate U+DC00 plus the byte.
func FSDecode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out []byte
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			out = AppendRune(out, 0xDC00+rune(b[i]))
			i++
			continue
		}
		out = append(out, b[i:i+n]...)
		i += n
	}
	return string(out)
}

// utf8Subpart reads one code point's bytes at b[i:]: its length, and
// whether it is complete. A start byte no sequence begins with has
// length 1 and bad true.
func utf8Subpart(b []byte, i int) (n int, complete, badStart bool) {
	c := b[i]
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch {
	case c < 0x80:
		return 1, true, false
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1, false, true
	}
	j := i + 1
	for k := 0; k < need; k++ {
		if j >= len(b) {
			return j - i, false, false
		}
		l, h := byte(0x80), byte(0xBF)
		if k == 0 {
			l, h = lo, hi
		}
		if b[j] < l || b[j] > h {
			return j - i, false, false
		}
		j++
	}
	return j - i, true, false
}

// DecodeStrict is b.decode("utf-8"), raising UnicodeDecodeError as Python
// words it.
func DecodeStrict(b []byte) (string, *Exception) {
	if utf8.Valid(b) {
		return string(b), nil
	}
	for i := 0; i < len(b); {
		n, complete, badStart := utf8Subpart(b, i)
		if complete {
			i += n
			continue
		}
		reason := "invalid continuation byte"
		switch {
		case badStart:
			reason = "invalid start byte"
		case i+n >= len(b):
			reason = "unexpected end of data"
		}
		if n == 1 {
			return "", NewException("UnicodeDecodeError", fmt.Sprintf(
				"'utf-8' codec can't decode byte 0x%02x in position %d: %s", b[i], i, reason))
		}
		return "", NewException("UnicodeDecodeError", fmt.Sprintf(
			"'utf-8' codec can't decode bytes in position %d-%d: %s", i, i+n-1, reason))
	}
	return string(b), nil
}

// DecodeReplace is b.decode("utf-8", "replace"): each maximal ill-formed
// subsequence becomes one U+FFFD, as Python's decoder does.
func DecodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			out.WriteByte(c)
			i++
			continue
		}
		need, lo, hi := 0, byte(0x80), byte(0xBF)
		switch {
		case c >= 0xC2 && c <= 0xDF:
			need = 1
		case c == 0xE0:
			need, lo = 2, 0xA0
		case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
			need = 2
		case c == 0xED:
			need, hi = 2, 0x9F
		case c == 0xF0:
			need, lo = 3, 0x90
		case c >= 0xF1 && c <= 0xF3:
			need = 3
		case c == 0xF4:
			need, hi = 3, 0x8F
		default:
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		j := i + 1
		ok := true
		for k := 0; k < need; k++ {
			if j >= len(b) {
				ok = false
				break
			}
			l, h := byte(0x80), byte(0xBF)
			if k == 0 {
				l, h = lo, hi
			}
			if b[j] < l || b[j] > h {
				ok = false
				break
			}
			j++
		}
		if ok {
			out.Write(b[i:j])
		} else {
			out.WriteRune(utf8.RuneError)
		}
		i = j
	}
	return out.String()
}

// IsPrintable is str.isprintable for one code point: false for the
// categories Cc, Cf, Cs, Co, Cn, Zl, Zp and Zs other than the space.
// Go 1.25 and Python 3.12 both carry Unicode 15.0.0.
func IsPrintable(r rune) bool {
	if r == ' ' {
		return true
	}
	if IsSurrogate(r) {
		return false
	}
	return unicode.IsPrint(r)
}

// Repr is repr(s).
func Repr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, n := DecodeRune(s[i:])
		i += n
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case IsPrintable(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// ReprList is repr(list[str]).
func ReprList(ss []string) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = Repr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// BackslashReplace is s.encode("utf-8", "backslashreplace"), as
// sys.stderr writes text: a lone surrogate becomes \udXXX.
func BackslashReplace(s string) string {
	if !HasSurrogate(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := DecodeRune(s[i:])
		if IsSurrogate(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// Splitlines is s.splitlines(): lines split at \n, \r, \r\n, \v, \f,
// \x1c, \x1d, \x1e, \x85, U+2028 and U+2029, with no empty last line.
func Splitlines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, n := DecodeRune(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			i += n
			if r == '\r' && i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
			continue
		}
		i += n
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
