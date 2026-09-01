package cli

import (
	"fmt"
	"sort"
	"strings"
)

// opt is one command-line option, spelled the way the Python CLI (typer
// over click) spells it.
type opt struct {
	names    []string // "--root", or "--yes" and "-y"
	neg      string   // "--no-gate" for a --gate/--no-gate pair
	value    bool     // takes a value
	multiple bool     // may repeat; every value kept
	help     string
	metavar  string
	hidden   bool // parsed, never listed in --help
}

// parsed holds what parseArgs read.
type parsed struct {
	vals  map[string][]string // by the option's first name
	flags map[string]*bool    // set flags; nil when absent
	args  []string
	help  bool
}

func (p *parsed) str(name, def string) string {
	if v := p.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}

func (p *parsed) has(name string) bool { return len(p.vals[name]) > 0 }

func (p *parsed) flag(name string) bool {
	b := p.flags[name]
	return b != nil && *b
}

func (p *parsed) flagSet(name string) bool { return p.flags[name] != nil }

// usageError is click's UsageError: exit 2 with the usage line first.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// parseArgs reads args as click does: options anywhere, "--opt=value" or
// "--opt value" (the next word is the value even when it starts with a
// dash), "--" ending the options, and --help anywhere.
func parseArgs(args []string, opts []opt, maxArgs int) (*parsed, error) {
	p := &parsed{vals: map[string][]string{}, flags: map[string]*bool{}}
	find := func(name string) (*opt, bool) {
		for i := range opts {
			o := &opts[i]
			for _, n := range o.names {
				if n == name {
					return o, true
				}
			}
			if o.neg != "" && o.neg == name {
				return o, false
			}
		}
		return nil, false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.args = append(p.args, args[i+1:]...)
			break
		}
		if a == "--help" {
			p.help = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			p.args = append(p.args, a)
			continue
		}
		name, value, hasEq := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			name, value, hasEq = a, "", false
		}
		o, positive := find(name)
		if o == nil {
			msg := fmt.Sprintf("No such option: %s", name)
			if strings.HasPrefix(name, "--") {
				if close := closeMatches(name, longNames(opts)); len(close) > 0 {
					sort.Strings(close)
					msg += " (Possible options: " + strings.Join(close, ", ") + ")"
				}
			}
			return nil, &usageError{msg}
		}
		key := o.names[0]
		if !o.value {
			if hasEq {
				return nil, &usageError{fmt.Sprintf("Option '%s' does not take a value.", name)}
			}
			b := positive || o.neg == ""
			if o.neg != "" && name == o.neg {
				b = false
			}
			p.flags[key] = &b
			continue
		}
		if !hasEq {
			if i+1 >= len(args) {
				return nil, &usageError{fmt.Sprintf("Option '%s' requires an argument.", name)}
			}
			i++
			value = args[i]
		}
		if o.multiple {
			p.vals[key] = append(p.vals[key], value)
		} else {
			p.vals[key] = []string{value}
		}
	}
	if !p.help && len(p.args) > maxArgs {
		extra := p.args[maxArgs:]
		return nil, &usageError{fmt.Sprintf("Got unexpected extra argument(s) (%s)", strings.Join(extra, " "))}
	}
	return p, nil
}

// longNames is click's _long_opt: every long option name, --help too.
func longNames(opts []opt) []string {
	out := []string{"--help"}
	for _, o := range opts {
		for _, n := range o.names {
			if strings.HasPrefix(n, "--") {
				out = append(out, n)
			}
		}
		if strings.HasPrefix(o.neg, "--") {
			out = append(out, o.neg)
		}
	}
	return out
}

// closeMatches is difflib.get_close_matches(word, possibilities): at
// most three, each with a ratio of 0.6 or more, best first.
func closeMatches(word string, possibilities []string) []string {
	type scored struct {
		s float64
		x string
	}
	var hits []scored
	a := []rune(word)
	for _, x := range possibilities {
		b := []rune(x)
		if r := seqRatio(b, a); r >= 0.6 {
			hits = append(hits, scored{r, x})
		}
	}
	// heapq.nlargest(3, (score, x)): by score, then by x, both descending.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].s != hits[j].s {
			return hits[i].s > hits[j].s
		}
		return hits[i].x > hits[j].x
	})
	var out []string
	for i := 0; i < len(hits) && i < 3; i++ {
		out = append(out, hits[i].x)
	}
	return out
}

// seqRatio is difflib.SequenceMatcher(None, a, b).ratio() for short
// sequences (no junk, no autojunk): 2*M/T over the matching blocks.
func seqRatio(a, b []rune) float64 {
	t := len(a) + len(b)
	if t == 0 {
		return 1
	}
	return 2 * float64(matched(a, 0, len(a), b, 0, len(b))) / float64(t)
}

// matched sums the matching blocks of a[alo:ahi] and b[blo:bhi]: the
// longest match (first in a, then first in b), then each side of it.
func matched(a []rune, alo, ahi int, b []rune, blo, bhi int) int {
	bi, bj, bk := alo, blo, 0
	for i := alo; i < ahi; i++ {
		for j := blo; j < bhi; j++ {
			k := 0
			for i+k < ahi && j+k < bhi && a[i+k] == b[j+k] {
				k++
			}
			if k > bk {
				bi, bj, bk = i, j, k
			}
		}
	}
	if bk == 0 {
		return 0
	}
	return bk + matched(a, alo, bi, b, blo, bj) + matched(a, bi+bk, ahi, b, bj+bk, bhi)
}
