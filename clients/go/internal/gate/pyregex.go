package gate

// patterns_gen.go is written by gen_patterns.py from the oracle source.

import (
	"strings"
	"sync"

	"daisugi-verify/internal/pyre"
	"daisugi-verify/internal/pystr"
)

// pyPattern is one oracle regex: its source and its flags.
type pyPattern struct {
	pattern string
	flags   int
}

// stdlibPatterns are the regexes of the Python standard library code the
// gate path runs.
var stdlibPatterns = map[string]pyPattern{
	// urllib.parse._check_bracketed_host
	"url._ipvfuture": {`\Av[a-fA-F0-9]+\..+\Z`, 0},
}

var (
	pyCompiledMu sync.Mutex
	pyCompiled   = map[string]*pyre.Regexp{}
)

// pyRe is the oracle's regex named key (see patterns_gen.go), compiled
// with Python's own re semantics.
func pyRe(key string) *pyre.Regexp {
	pyCompiledMu.Lock()
	defer pyCompiledMu.Unlock()
	if re, ok := pyCompiled[key]; ok {
		return re
	}
	p, ok := pyPatterns[key]
	if !ok {
		p, ok = stdlibPatterns[key]
	}
	if !ok {
		panic("gate: no oracle pattern " + key)
	}
	re, err := pyre.Compile(p.pattern, p.flags)
	if err != nil {
		panic("gate: oracle pattern " + key + ": " + err.Error())
	}
	pyCompiled[key] = re
	return re
}

// pySearch is re.search(pattern, s) is not None.
func pySearch(key, s string) bool { return pyRe(key).Search(s) }

// pyMatch is re.match(pattern, s): the group spans of a match at 0, or nil.
func pyMatch(key, s string) []string {
	rs := pystr.Runes(s)
	_, marks, ok := pyRe(key).MatchAt(rs, 0)
	if !ok {
		return nil
	}
	return groupTexts(rs, marks)
}

func groupTexts(rs []rune, marks []int) []string {
	out := make([]string, len(marks)/2)
	for g := range out {
		if marks[2*g] >= 0 && marks[2*g+1] >= 0 {
			out[g] = pystr.FromRunes(rs[marks[2*g]:marks[2*g+1]])
		}
	}
	return out
}

// pySub is re.sub(pattern, repl, s) with a function repl, which gets the
// match's groups (group 0 first, "" for a group that did not take part).
func pySub(key, s string, repl func(groups []string) string) string {
	re := pyRe(key)
	rs := pystr.Runes(s)
	var b strings.Builder
	last, pos := 0, 0
	mustAdvance := false
	for pos <= len(rs) {
		start, end, marks, ok := re.SearchGroups(rs, pos, mustAdvance)
		if !ok {
			break
		}
		b.WriteString(pystr.FromRunes(rs[last:start]))
		g := groupTexts(rs, marks)
		g[0] = pystr.FromRunes(rs[start:end])
		b.WriteString(repl(g))
		last = end
		mustAdvance = end == start
		pos = end
	}
	b.WriteString(pystr.FromRunes(rs[last:]))
	return b.String()
}
