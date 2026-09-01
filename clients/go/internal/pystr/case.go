package pystr

import "strings"

type runeRange struct{ lo, hi rune }

func inRanges(r rune, rs []runeRange) bool {
	lo, hi := 0, len(rs)
	for lo < hi {
		m := (lo + hi) / 2
		switch {
		case r < rs[m].lo:
			hi = m
		case r > rs[m].hi:
			lo = m + 1
		default:
			return true
		}
	}
	return false
}

// Lower is str.lower(): the full lowercase mapping, with the final sigma
// rule.
func Lower(s string) string {
	rs := Runes(s)
	var b strings.Builder
	for i, r := range rs {
		if r == 0x03a3 {
			b.WriteRune(finalSigma(rs, i))
			continue
		}
		if v, ok := lowerTable[r]; ok {
			b.WriteString(v)
			continue
		}
		b.Write(AppendRune(nil, r))
	}
	return b.String()
}

func finalSigma(rs []rune, i int) rune {
	j := i - 1
	for j >= 0 && inRanges(rs[j], caseIgnorable) {
		j--
	}
	final := j >= 0 && inRanges(rs[j], casedNotIgnorable)
	if final && i+1 < len(rs) {
		j = i + 1
		for j < len(rs) && inRanges(rs[j], caseIgnorable) {
			j++
		}
		final = j == len(rs) || !inRanges(rs[j], casedNotIgnorable)
	}
	if final {
		return 0x03c2
	}
	return 0x03c3
}

// Casefold is str.casefold().
func Casefold(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := DecodeRune(s[i:])
		i += n
		if v, ok := casefoldTable[r]; ok {
			b.WriteString(v)
			continue
		}
		b.Write(AppendRune(nil, r))
	}
	return b.String()
}

// IsSpace is str.isspace for one code point.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, ' ',
		0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Strip is str.strip() with no argument.
func Strip(s string) string {
	start := 0
	for start < len(s) {
		r, n := DecodeRune(s[start:])
		if !IsSpace(r) {
			break
		}
		start += n
	}
	end := len(s)
	for end > start {
		// walk back one code point
		k := end - 1
		for k > start && s[k]&0xC0 == 0x80 {
			k--
		}
		r, _ := DecodeRune(s[k:end])
		if !IsSpace(r) {
			break
		}
		end = k
	}
	return s[start:end]
}

// Split is str.split() with no argument.
func Split(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		for i < len(s) {
			r, n := DecodeRune(s[i:])
			if !IsSpace(r) {
				break
			}
			i += n
		}
		if i >= len(s) {
			break
		}
		j := i
		for j < len(s) {
			r, n := DecodeRune(s[j:])
			if IsSpace(r) {
				break
			}
			j += n
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}
