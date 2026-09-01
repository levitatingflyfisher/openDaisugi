package pyre

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"sync"

	"daisugi-verify/internal/pystr"
)

// This file is unicodedata.lookup as re's \N{...} escape uses it: a name
// counts when lookup gives exactly one code point (a named sequence gives
// several, and ord() of it raises TypeError, which re words as an
// undefined name).

var (
	namesOnce sync.Once
	names     map[string]rune
)

func loadNames() {
	names = map[string]rune{}
	raw, err := base64.StdEncoding.DecodeString(namesGz)
	if err != nil {
		panic(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		panic(err)
	}
	text, err := io.ReadAll(zr)
	if err != nil {
		panic(err)
	}
	for _, line := range strings.Split(string(text), "\n") {
		name, hex, _ := strings.Cut(line, ";")
		v, err := strconv.ParseUint(hex, 16, 32)
		if err != nil {
			panic(err)
		}
		names[name] = rune(v)
	}
}

// nameMaxLen is unicodedata's NAME_MAXLEN: a longer name is a KeyError.
const nameMaxLen = 256

var (
	hangulL = []string{"G", "GG", "N", "D", "DD", "R", "M", "B", "BB", "S", "SS", "", "J", "JJ", "C", "K", "T", "P", "H"}
	hangulV = []string{"A", "AE", "YA", "YAE", "EO", "E", "YEO", "YE", "O", "WA", "WAE", "OE", "YO", "U", "WEO", "WE", "WI",
		"YU", "EU", "YI", "I"}
	hangulT = []string{"", "G", "GG", "GS", "N", "NJ", "NH", "D", "L", "LG", "LM", "LB", "LS", "LT", "LP", "LH", "M", "B",
		"BS", "S", "SS", "NG", "J", "C", "K", "T", "P", "H"}
)

// findSyllable is unicodedata's find_syllable: the longest jamo name at
// the start of s (case-sensitive), as its index and length, or -1.
func findSyllable(s string, table []string) (pos, n int) {
	pos, n = -1, -1
	for i, j := range table {
		if len(j) <= n {
			continue
		}
		if strings.HasPrefix(s, j) {
			pos, n = i, len(j)
		}
	}
	if n == -1 {
		n = 0
	}
	return pos, n
}

func hasPrefixExact(s, prefix string) bool {
	return strings.HasPrefix(s, prefix)
}

// lookupName is ord(unicodedata.lookup(name)): ok is false where re says
// "undefined character name". A lone surrogate in the name raises
// UnicodeEncodeError, as the str-to-UTF-8 argument conversion does.
func lookupName(name string) (r rune, ok bool, exc *Error) {
	b, eerr := pystr.EncodeUTF8(name)
	if eerr != nil {
		return 0, false, &Error{Type: eerr.Type, Msg: eerr.Msg, Pos: -1}
	}
	s := string(b)
	if len(s) > nameMaxLen {
		return 0, false, nil
	}
	if hasPrefixExact(s, "HANGUL SYLLABLE ") {
		rest := s[16:]
		l, n := findSyllable(rest, hangulL)
		rest = rest[n:]
		v, n := findSyllable(rest, hangulV)
		rest = rest[n:]
		t, n := findSyllable(rest, hangulT)
		rest = rest[n:]
		if l < 0 || v < 0 || t < 0 || rest != "" {
			return 0, false, nil
		}
		return 0xAC00 + rune((l*21+v)*28+t), true, nil
	}
	if hasPrefixExact(s, "CJK UNIFIED IDEOGRAPH-") {
		hex := s[22:]
		if len(hex) != 4 && len(hex) != 5 {
			return 0, false, nil
		}
		var v rune
		for i := 0; i < len(hex); i++ {
			c := hex[i]
			switch {
			case c >= '0' && c <= '9':
				v = v*16 + rune(c-'0')
			case c >= 'A' && c <= 'F':
				v = v*16 + rune(c-'A'+10)
			default:
				return 0, false, nil
			}
		}
		for _, rg := range cjkRanges {
			if v >= rg[0] && v <= rg[1] {
				return v, true, nil
			}
		}
		return 0, false, nil
	}
	namesOnce.Do(loadNames)
	r, ok = names[upperASCII(s)]
	return r, ok, nil
}

// upperASCII is Py_TOUPPER over each byte, as unicodedata compares names.
func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
