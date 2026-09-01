package pyre

import (
	"unicode"

	"daisugi-verify/internal/pystr"
)

// Regexp is a compiled pattern: re.compile(pattern).
type Regexp struct {
	Pattern string
	root    *SubPattern
	flags   int
	groups  int
	// ignore holds the IGNORECASE form of each IN item, by identity.
	ignore map[*Item]*caseSet
	// prefilter is the charset a match's first character must pass, as
	// _sre's search checks it with the pattern's outer flags, or nil.
	prefilter []Item
}

// startOK is the search's charset filter at st.
func (re *Regexp) startOK(s []rune, st int) bool {
	if re.prefilter == nil {
		return true
	}
	return st < len(s) && plainIn(re.prefilter, s[st], re.flags)
}

// Compile is re.compile(pattern, flags): the parse, then the compiler's
// own checks. Warnings the parse would print are returned; a caller that
// cannot print them the way Python does should refuse the pattern.
func Compile(pattern string, flags int) (*Regexp, *Error) {
	p, err := Parse(pattern, flags, 4)
	if err != nil {
		return nil, err
	}
	re := &Regexp{Pattern: pattern, root: p, flags: p.state.Flags, groups: p.state.Groups(), ignore: map[*Item]*caseSet{}}
	if e := re.check(p, re.flags); e != nil {
		return nil, e
	}
	re.prefilter = charsetPrefix(p, re.flags)
	return re, nil
}

// charsetPrefix is the search filter _compile_info builds when the pattern
// has no literal prefix: the set its first character must be in. It walks
// into groups with their flags, but _compile_info compiles the set with
// the pattern's outer flags, so a \w in (?a:...) filters with the Unicode
// \w. _sre searches only where the filter passes, and so does this.
func charsetPrefix(p *SubPattern, flags int) []Item {
	if p.GetWidth()[0] == 0 {
		return nil
	}
	if len(literalPrefix(p, flags)) > 0 {
		return nil
	}
	pat := p
	var it Item
	for {
		if len(pat.Data) == 0 {
			return nil
		}
		it = pat.Data[0]
		if it.Op != SUBPATTERN {
			break
		}
		flags = combineFlags(flags, it.AddFlags, it.DelFlags)
		pat = it.Sub
	}
	cased := func(r int) bool {
		if flags&FlagIgnoreCase == 0 {
			return false
		}
		if flags&FlagUnicode != 0 {
			return uniIsCased(rune(r))
		}
		return asciiIsCased(rune(r))
	}
	switch it.Op {
	case LITERAL:
		if cased(it.Lit) {
			return nil
		}
		return []Item{it}
	case BRANCH:
		var set []Item
		for _, b := range it.Branches {
			if len(b.Data) == 0 {
				return nil
			}
			f := b.Data[0]
			if f.Op == LITERAL && !cased(f.Lit) {
				set = append(set, f)
			} else {
				return nil
			}
		}
		return set
	case IN:
		if flags&FlagIgnoreCase != 0 {
			for _, s := range it.Set {
				switch s.Op {
				case LITERAL:
					if cased(s.Lit) {
						return nil
					}
				case RANGE:
					if s.Hi > 0xffff {
						return nil
					}
					for c := s.Lo; c <= s.Hi; c++ {
						if cased(c) {
							return nil
						}
					}
				}
			}
		}
		return it.Set
	}
	return nil
}

// literalPrefix is _get_literal_prefix's prefix.
func literalPrefix(p *SubPattern, flags int) []int {
	pre, _ := literalPrefixAll(p, flags)
	return pre
}

func literalPrefixAll(p *SubPattern, flags int) ([]int, bool) {
	var prefix []int
	for _, it := range p.Data {
		switch it.Op {
		case LITERAL:
			if flags&FlagIgnoreCase != 0 {
				c := rune(it.Lit)
				if (flags&FlagUnicode != 0 && uniIsCased(c)) || (flags&FlagUnicode == 0 && asciiIsCased(c)) {
					return prefix, false
				}
			}
			prefix = append(prefix, it.Lit)
		case SUBPATTERN:
			f := combineFlags(flags, it.AddFlags, it.DelFlags)
			pre, all := literalPrefixAll(it.Sub, f)
			prefix = append(prefix, pre...)
			if !all {
				return prefix, false
			}
		default:
			return prefix, false
		}
	}
	return prefix, true
}

// Warnings are the warnings the parse would have printed.
func (re *Regexp) Warnings() []string { return re.root.state.Warnings }

// check runs _compiler's errors: a look-behind must be fixed width.
func (re *Regexp) check(p *SubPattern, flags int) *Error {
	for i := range p.Data {
		it := &p.Data[i]
		switch it.Op {
		case ASSERT, ASSERT_NOT:
			if it.Lit < 0 {
				w := it.Sub.GetWidth()
				if w[0] > MaxRepeat {
					return &Error{Type: "error", Msg: "looks too much behind", Pos: -1}
				}
				if w[0] != w[1] {
					return &Error{Type: "error", Msg: "look-behind requires fixed-width pattern", Pos: -1}
				}
			}
			if e := re.check(it.Sub, flags); e != nil {
				return e
			}
		case SUBPATTERN:
			if e := re.check(it.Sub, combineFlags(flags, it.AddFlags, it.DelFlags)); e != nil {
				return e
			}
		case MIN_REPEAT, MAX_REPEAT, POSSESSIVE_REPEAT, ATOMIC_GROUP:
			if e := re.check(it.Sub, flags); e != nil {
				return e
			}
		case BRANCH:
			for _, b := range it.Branches {
				if e := re.check(b, flags); e != nil {
					return e
				}
			}
		case GROUPREF_EXISTS:
			if e := re.check(it.Sub, flags); e != nil {
				return e
			}
			if it.No != nil {
				if e := re.check(it.No, flags); e != nil {
					return e
				}
			}
		case IN:
			if flags&FlagIgnoreCase != 0 {
				re.ignore[it] = newCaseSet(it.Set, flags)
			}
		}
	}
	return nil
}

func combineFlags(flags, add, del int) int {
	if add&typeFlags != 0 {
		flags &^= typeFlags
	}
	return (flags | add) &^ del
}

// matcher is one match attempt over the subject's code points.
type matcher struct {
	re    *Regexp
	s     []rune
	marks []int
	// steps counts work so a caller can bound it.
	steps int
}

type cont func(pos int) bool

// Search is re.search(pattern, s) is not None.
func (re *Regexp) Search(s string) bool {
	_, _, ok := re.SearchAt(pystr.Runes(s), 0, false)
	return ok
}

// SearchAt finds the leftmost match starting at or after pos. With
// mustAdvance, an empty match at pos itself is refused (sre's rule for the
// match after an empty one in finditer and sub).
func (re *Regexp) SearchAt(s []rune, pos int, mustAdvance bool) (start, end int, ok bool) {
	m := &matcher{re: re, s: s, marks: make([]int, 2*re.groups)}
	for st := pos; st <= len(s); st++ {
		if !re.startOK(s, st) {
			continue
		}
		for i := range m.marks {
			m.marks[i] = -1
		}
		e := -1
		if m.seq(re.root.Data, 0, st, re.flags, func(p int) bool {
			if mustAdvance && st == pos && p == pos {
				return false
			}
			e = p
			return true
		}) {
			return st, e, true
		}
	}
	return 0, 0, false
}

// MatchAt is re.match at pos: the match must start there.
func (re *Regexp) MatchAt(s []rune, pos int) (end int, marks []int, ok bool) {
	m := &matcher{re: re, s: s, marks: make([]int, 2*re.groups)}
	for i := range m.marks {
		m.marks[i] = -1
	}
	e := -1
	if m.seq(re.root.Data, 0, pos, re.flags, func(p int) bool { e = p; return true }) {
		return e, m.marks, true
	}
	return 0, nil, false
}

// SearchGroups is re.search returning the span and the group marks.
func (re *Regexp) SearchGroups(s []rune, pos int, mustAdvance bool) (start, end int, marks []int, ok bool) {
	m := &matcher{re: re, s: s, marks: make([]int, 2*re.groups)}
	for st := pos; st <= len(s); st++ {
		if !re.startOK(s, st) {
			continue
		}
		for i := range m.marks {
			m.marks[i] = -1
		}
		e := -1
		if m.seq(re.root.Data, 0, st, re.flags, func(p int) bool {
			if mustAdvance && st == pos && p == pos {
				return false
			}
			e = p
			return true
		}) {
			return st, e, append([]int{}, m.marks...), true
		}
	}
	return 0, 0, nil, false
}

func (m *matcher) seq(items []Item, i, pos, flags int, k cont) bool {
	if i == len(items) {
		return k(pos)
	}
	it := &items[i]
	next := func(p int) bool { return m.seq(items, i+1, p, flags, k) }
	s := m.s
	n := len(s)
	switch it.Op {
	case LITERAL, NOT_LITERAL:
		if pos >= n {
			return false
		}
		eq := m.literalEq(s[pos], it.Lit, flags)
		if eq == (it.Op == LITERAL) {
			return next(pos + 1)
		}
		return false
	case ANY:
		if pos >= n || (flags&FlagDotAll == 0 && s[pos] == '\n') {
			return false
		}
		return next(pos + 1)
	case IN:
		if pos >= n || !m.inSet(it, s[pos], flags) {
			return false
		}
		return next(pos + 1)
	case AT:
		if !m.at(it.Lit, pos, flags) {
			return false
		}
		return next(pos)
	case BRANCH:
		for _, b := range it.Branches {
			saved := append([]int{}, m.marks...)
			if m.seq(b.Data, 0, pos, flags, next) {
				return true
			}
			copy(m.marks, saved)
		}
		return false
	case SUBPATTERN:
		f := combineFlags(flags, it.AddFlags, it.DelFlags)
		g := it.Group
		return m.seq(it.Sub.Data, 0, pos, f, func(p int) bool {
			if g < 0 {
				return next(p)
			}
			s0, e0 := m.marks[2*g], m.marks[2*g+1]
			m.marks[2*g], m.marks[2*g+1] = pos, p
			if next(p) {
				return true
			}
			m.marks[2*g], m.marks[2*g+1] = s0, e0
			return false
		})
	case MAX_REPEAT, MIN_REPEAT:
		return m.repeat(it, pos, flags, next)
	case POSSESSIVE_REPEAT:
		count := 0
		p := pos
		for count < it.Lo {
			e, ok := m.first(it.Sub.Data, p, flags)
			if !ok {
				return false
			}
			p = e
			count++
		}
		for count < it.Hi || it.Hi == MaxRepeat {
			saved := append([]int{}, m.marks...)
			e, ok := m.first(it.Sub.Data, p, flags)
			if !ok {
				copy(m.marks, saved)
				break
			}
			if e == p {
				// an empty iteration: the loop stops, as sre's does
				count++
				break
			}
			p = e
			count++
		}
		return next(p)
	case ATOMIC_GROUP:
		saved := append([]int{}, m.marks...)
		e, ok := m.first(it.Sub.Data, pos, flags)
		if !ok {
			copy(m.marks, saved)
			return false
		}
		if next(e) {
			return true
		}
		copy(m.marks, saved)
		return false
	case ASSERT, ASSERT_NOT:
		start := pos
		if it.Lit < 0 {
			w := it.Sub.GetWidth()[0]
			start = pos - w
		}
		saved := append([]int{}, m.marks...)
		matched := false
		if start >= 0 {
			_, matched = m.firstEnding(it.Sub.Data, start, flags, pos, it.Lit < 0)
		}
		if it.Op == ASSERT {
			if !matched {
				copy(m.marks, saved)
				return false
			}
			if next(pos) {
				return true
			}
			copy(m.marks, saved)
			return false
		}
		copy(m.marks, saved)
		if matched {
			return false
		}
		return next(pos)
	case GROUPREF:
		g := it.Lit
		a, b := m.marks[2*g], m.marks[2*g+1]
		if a < 0 || b < 0 || b < a {
			return false
		}
		l := b - a
		if pos+l > n {
			return false
		}
		for j := 0; j < l; j++ {
			x, y := s[a+j], s[pos+j]
			if flags&FlagIgnoreCase != 0 {
				if flags&FlagASCII != 0 {
					x, y = asciiLower(x), asciiLower(y)
				} else {
					x, y = uniLower(x), uniLower(y)
				}
			}
			if x != y {
				return false
			}
		}
		return next(pos + l)
	case GROUPREF_EXISTS:
		g := it.Lit
		if g < m.re.groups && m.marks[2*g] >= 0 && m.marks[2*g+1] >= 0 {
			return m.seq(it.Sub.Data, 0, pos, flags, next)
		}
		if it.No != nil {
			return m.seq(it.No.Data, 0, pos, flags, next)
		}
		return next(pos)
	}
	return false
}

// first is the end of the first match of items at pos, alone.
func (m *matcher) first(items []Item, pos, flags int) (int, bool) {
	e := -1
	ok := m.seq(items, 0, pos, flags, func(p int) bool { e = p; return true })
	return e, ok
}

// firstEnding matches items at pos for an assertion. A look-behind body
// is fixed width, so its match ends at end.
func (m *matcher) firstEnding(items []Item, pos, flags, end int, behind bool) (int, bool) {
	e := -1
	ok := m.seq(items, 0, pos, flags, func(p int) bool {
		if behind && p != end {
			return false
		}
		e = p
		return true
	})
	return e, ok
}

// repeat is REPEAT with MAX_UNTIL or MIN_UNTIL, as _sre runs them.
func (m *matcher) repeat(it *Item, pos, flags int, k cont) bool {
	count := -1
	lastPtr := -1
	min, max := it.Lo, it.Hi
	body := it.Sub.Data
	var until cont
	if it.Op == MAX_REPEAT {
		until = func(p int) bool {
			m.steps++
			c := count + 1
			if c < min {
				saved := count
				count = c
				if m.seq(body, 0, p, flags, until) {
					return true
				}
				count = saved
				return false
			}
			if (c < max || max == MaxRepeat) && p != lastPtr {
				sc, sl := count, lastPtr
				count, lastPtr = c, p
				marks := append([]int{}, m.marks...)
				if m.seq(body, 0, p, flags, until) {
					return true
				}
				copy(m.marks, marks)
				count, lastPtr = sc, sl
			}
			sc, sl := count, lastPtr
			if k(p) {
				return true
			}
			count, lastPtr = sc, sl
			return false
		}
	} else {
		until = func(p int) bool {
			m.steps++
			c := count + 1
			if c < min {
				saved := count
				count = c
				if m.seq(body, 0, p, flags, until) {
					return true
				}
				count = saved
				return false
			}
			marks := append([]int{}, m.marks...)
			sc, sl := count, lastPtr
			if k(p) {
				return true
			}
			count, lastPtr = sc, sl
			copy(m.marks, marks)
			if (c >= max && max != MaxRepeat) || p == lastPtr {
				return false
			}
			count, lastPtr = c, p
			if m.seq(body, 0, p, flags, until) {
				return true
			}
			count, lastPtr = sc, sl
			return false
		}
	}
	return until(pos)
}

func asciiLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	return r
}

// uniLower is _PyUnicode_ToLowercase: the simple lowercase mapping.
func uniLower(r rune) rune {
	if pystr.IsSurrogate(r) {
		return r
	}
	return unicode.ToLower(r)
}

func uniUpper(r rune) rune {
	if pystr.IsSurrogate(r) {
		return r
	}
	return unicode.ToUpper(r)
}

func uniIsCased(r rune) bool { return r != uniLower(r) || r != uniUpper(r) }

func asciiIsCased(r rune) bool {
	return r < 128 && ((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
}

// literalEq is the compiled LITERAL test for flags.
func (m *matcher) literalEq(ch rune, lit int, flags int) bool {
	c := rune(lit)
	if flags&FlagIgnoreCase == 0 {
		return ch == c
	}
	if flags&FlagUnicode == 0 {
		// ASCII: LITERAL_IGNORE when cased.
		if !asciiIsCased(c) {
			return ch == c
		}
		return asciiLower(ch) == asciiLower(c)
	}
	if !uniIsCased(c) {
		return ch == c
	}
	lo := uniLower(c)
	x := uniLower(ch)
	if fixes, ok := extraCases[lo]; ok {
		if x == lo {
			return true
		}
		for _, f := range fixes {
			if x == f {
				return true
			}
		}
		return false
	}
	return x == lo
}

func isUniWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func isASCIIWord(r rune) bool {
	return r < 128 && (r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
}

// isUniSpace is str.isspace for one code point.
func isUniSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, ' ',
		0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
}

func (m *matcher) at(code, pos, flags int) bool {
	s := m.s
	n := len(s)
	switch code {
	case AT_BEGINNING:
		if flags&FlagMultiline != 0 {
			return pos == 0 || s[pos-1] == '\n'
		}
		return pos == 0
	case AT_BEGINNING_STRING:
		return pos == 0
	case AT_END:
		if flags&FlagMultiline != 0 {
			return pos == n || s[pos] == '\n'
		}
		return pos == n || (pos+1 == n && s[pos] == '\n')
	case AT_END_STRING:
		return pos == n
	case AT_BOUNDARY, AT_NON_BOUNDARY:
		if n == 0 {
			return false
		}
		word := isUniWord
		if flags&FlagUnicode == 0 {
			word = isASCIIWord
		}
		that := pos > 0 && word(s[pos-1])
		this := pos < n && word(s[pos])
		if code == AT_BOUNDARY {
			return this != that
		}
		return this == that
	}
	return false
}

func category(code int, r rune, flags int) bool {
	uni := flags&FlagUnicode != 0
	switch code {
	case CATEGORY_DIGIT, CATEGORY_NOT_DIGIT:
		var d bool
		if uni {
			d = unicode.IsDigit(r)
		} else {
			d = r >= '0' && r <= '9'
		}
		return d == (code == CATEGORY_DIGIT)
	case CATEGORY_SPACE, CATEGORY_NOT_SPACE:
		var sp bool
		if uni {
			sp = isUniSpace(r)
		} else {
			sp = isASCIISpace(r)
		}
		return sp == (code == CATEGORY_SPACE)
	case CATEGORY_WORD, CATEGORY_NOT_WORD:
		var w bool
		if uni {
			w = isUniWord(r)
		} else {
			w = isASCIIWord(r)
		}
		return w == (code == CATEGORY_WORD)
	}
	return false
}

// inSet is the IN test.
func (m *matcher) inSet(it *Item, r rune, flags int) bool {
	if flags&FlagIgnoreCase != 0 {
		if cs := m.re.ignore[it]; cs != nil {
			return cs.has(r)
		}
	}
	return plainIn(it.Set, r, flags)
}

func plainIn(set []Item, r rune, flags int) bool {
	negate := false
	for _, s := range set {
		switch s.Op {
		case NEGATE:
			negate = true
		case LITERAL:
			if r == rune(s.Lit) {
				return !negate
			}
		case RANGE:
			if int(r) >= s.Lo && int(r) <= s.Hi {
				return !negate
			}
		case CATEGORY:
			if category(s.Lit, r, flags) {
				return !negate
			}
		}
	}
	return negate
}
