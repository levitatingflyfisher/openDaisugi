package verify

import (
	"regexp"
	"strings"
	"sync"
)

// --- fnmatch.translate() port ------------------------------------------------
//
// Only the subset actually exercised by opendaisugi's allowlist/path-glob
// patterns: literals, '*' (consecutive runs collapse to one, per cpython's
// _translate), '?', and '[...]'/'[!...]' character classes (ranges, no
// escaping of '\' inside brackets — no corpus or fixture pattern needs it).
// dotAll controls whether '.' matches '\n':
//   - _head_allowed calls fnmatch.fnmatchcase() directly → DOTALL (fnmatch's
//     own translate() wraps its output in "(?s:...)").
//   - _path_matches_any's pathlib.PurePosixPath.match() strips that DOTALL
//     wrapper before splicing each segment into the multi-line, non-DOTALL
//     compiled pattern (so '*' can never cross a path separator).
// Segments never contain '/' in either caller, so this only matters for a
// literal embedded newline — vanishingly unlikely in real input, kept for
// fidelity anyway since it costs nothing.

var (
	fnmatchCacheMu sync.Mutex
	fnmatchCache   = map[string]*regexp.Regexp{}
)

// noMatch is a sentinel: some translated patterns (an empty "[]" class) can
// never match anything.
var noMatchPattern = regexp.MustCompile(`\x00NEVER\x00`)

func compileFnmatch(pat string, dotAll bool) *regexp.Regexp {
	key := pat
	if dotAll {
		key = "S:" + pat
	} else {
		key = "N:" + pat
	}
	fnmatchCacheMu.Lock()
	if re, ok := fnmatchCache[key]; ok {
		fnmatchCacheMu.Unlock()
		return re
	}
	fnmatchCacheMu.Unlock()

	re := buildFnmatchRegex(pat, dotAll)
	fnmatchCacheMu.Lock()
	fnmatchCache[key] = re
	fnmatchCacheMu.Unlock()
	return re
}

func buildFnmatchRegex(pat string, dotAll bool) *regexp.Regexp {
	var b strings.Builder
	if dotAll {
		b.WriteString("(?s)")
	}
	b.WriteString(`\A`)
	runes := []rune(pat)
	n := len(runes)
	i := 0
	neverMatches := false
	for i < n {
		c := runes[i]
		i++
		switch c {
		case '*':
			b.WriteString(".*")
			for i < n && runes[i] == '*' {
				i++
			}
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < n && runes[j] == '!' {
				j++
			}
			if j < n && runes[j] == ']' {
				j++
			}
			for j < n && runes[j] != ']' {
				j++
			}
			if j >= n {
				b.WriteString(`\[`)
			} else {
				stuff := string(runes[i:j])
				i = j + 1
				switch {
				case stuff == "":
					neverMatches = true
				case stuff == "!":
					b.WriteString(".")
				default:
					if strings.HasPrefix(stuff, "!") {
						stuff = "^" + stuff[1:]
					} else if strings.HasPrefix(stuff, "^") || strings.HasPrefix(stuff, "[") {
						stuff = `\` + stuff
					}
					stuff = strings.ReplaceAll(stuff, `\`, `\\`)
					b.WriteByte('[')
					b.WriteString(stuff)
					b.WriteByte(']')
				}
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	if neverMatches {
		return noMatchPattern
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		// A pattern our translator produced but Go's RE2 rejects (should not
		// happen for the corpus's realistic patterns) — fail closed: match
		// nothing rather than risk a broad, unintended accept.
		return noMatchPattern
	}
	return re
}

func fnmatchCase(name, pat string, dotAll bool) bool {
	return compileFnmatch(pat, dotAll).MatchString(name)
}

// --- verify._GLOB_CHARS_RE ---------------------------------------------------

func hasGlobChars(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// --- verify._head_allowed -----------------------------------------------------
//
// Literal entries require exact equality; glob entries (containing */?/[)
// match segment-by-segment (both split on "/") with EQUAL segment counts —
// this is what left-anchors the match. See PORTING-NOTES.md.
func headAllowed(head string, allowlist []string) bool {
	for _, pat := range allowlist {
		if head == pat {
			return true
		}
		if !hasGlobChars(pat) {
			continue
		}
		headSegs := strings.Split(head, "/")
		patSegs := strings.Split(pat, "/")
		if len(headSegs) != len(patSegs) {
			continue
		}
		allMatch := true
		for i := range headSegs {
			if !fnmatchCase(headSegs[i], patSegs[i], true) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return true
		}
	}
	return false
}
