package verify

import "unicode/utf8"

// The subset of regex an MCP argument rule may use. This is
// opendaisugi/rule_regex.py (`supported`) ported step for step; the fixture
// clients/fixtures/rule_regex_subset.json holds the verdicts all three ports
// share. See the Python module for the list of what is inside.
//
// Limits: repeat counts multiply to at most MaxRuleRepeat, groups nest at
// most MaxRuleDepth deep, and the expanded size is at most MaxRuleSize.

const (
	MaxRuleRepeat = 1000
	MaxRuleDepth  = 50
	MaxRuleSize   = 2000
)

type subsetParser struct {
	s   []rune
	pos int
}

type outside struct{}

func (p *subsetParser) peek(k int) rune {
	i := p.pos + k
	if i < len(p.s) {
		return p.s[i]
	}
	return -1
}

func isRulePunct(c rune) bool {
	switch {
	case c >= '!' && c <= '/', c >= ':' && c <= '@', c >= '[' && c <= '`', c >= '{' && c <= '~':
		return true
	}
	return false
}

func isRuleDigit(c rune) bool { return c >= '0' && c <= '9' }

func isNotLiteral(c rune) bool {
	switch c {
	case '\\', '.', '^', '$', '[', ']', '(', ')', '|', '*', '+', '?', '{', '}':
		return true
	}
	return false
}

func isQuantStart(c rune) bool { return c == '*' || c == '+' || c == '?' || c == '{' }

func isAllClassEscape(c rune) bool {
	switch c {
	case 'd', 'w', 's', 'D', 'W', 'S':
		return true
	}
	return false
}

func isClassEscape(c rune) bool { return c == 'd' || c == 'w' || c == 's' }

// RuleRegexSupported is rule_regex.supported: the pattern is inside the
// subset.
func RuleRegexSupported(pattern string) (ok bool) {
	if !utf8.ValidString(pattern) { // a lone surrogate (WTF-8) is not valid
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			if _, is := r.(outside); is {
				ok = false
				return
			}
			panic(r)
		}
	}()
	p := &subsetParser{s: []rune(pattern)}
	p.run()
	return true
}

func (p *subsetParser) run() {
	if len(p.s) >= 4 && string(p.s[:4]) == "(?i)" {
		p.pos = 4
	}
	size, prod := p.alt(0)
	if p.pos != len(p.s) {
		panic(outside{}) // a stray ")"
	}
	if size > MaxRuleSize || prod > MaxRuleRepeat {
		panic(outside{})
	}
}

func (p *subsetParser) alt(depth int) (int, int) {
	if depth > MaxRuleDepth {
		panic(outside{})
	}
	size, prod := 0, 1
	for {
		s, pr := p.seq(depth)
		size += s
		if pr > prod {
			prod = pr
		}
		if p.peek(0) == '|' {
			p.pos++
			size++
			continue
		}
		return size, prod
	}
}

func (p *subsetParser) seq(depth int) (int, int) {
	size, prod := 0, 1
	for p.pos < len(p.s) && p.peek(0) != '|' && p.peek(0) != ')' {
		aSize, aProd, quantifiable := p.atom(depth)
		if quantifiable && isQuantStart(p.peek(0)) {
			fSize, fProd := p.quant()
			aSize *= fSize
			aProd *= fProd
			if isQuantStart(p.peek(0)) {
				panic(outside{}) // a second quantifier
			}
		}
		size += aSize
		if aProd > prod {
			prod = aProd
		}
	}
	return size, prod
}

func (p *subsetParser) atom(depth int) (int, int, bool) {
	c := p.peek(0)
	switch {
	case c == '\\':
		e := p.peek(1)
		if isAllClassEscape(e) || (e >= 0 && isRulePunct(e)) {
			p.pos += 2
			return 1, 1, true
		}
		panic(outside{})
	case c == '.':
		p.pos++
		return 1, 1, true
	case c == '^' || c == '$':
		p.pos++
		return 1, 1, false
	case c == '[':
		p.class()
		return 1, 1, true
	case c == '(':
		if p.peek(1) == '?' && p.peek(2) == ':' {
			p.pos += 3
		} else if p.peek(1) == '?' {
			panic(outside{})
		} else {
			p.pos++
		}
		size, prod := p.alt(depth + 1)
		if p.peek(0) != ')' {
			panic(outside{})
		}
		p.pos++
		return size, prod, true
	case isNotLiteral(c):
		panic(outside{}) // a quantifier without an atom, a bare ] } or {
	}
	p.pos++
	return 1, 1, true
}

func (p *subsetParser) quant() (int, int) {
	c := p.peek(0)
	size, prod := 1, 1
	p.pos++
	if c == '{' {
		m := p.number()
		hi, hasHi := m, true
		if p.peek(0) == ',' {
			p.pos++
			if isRuleDigit(p.peek(0)) {
				hi = p.number()
			} else {
				hasHi = false
			}
		}
		if p.peek(0) != '}' {
			panic(outside{})
		}
		p.pos++
		if hasHi && m > hi {
			panic(outside{})
		}
		if hasHi {
			prod, size = hi, hi
		} else {
			prod, size = m, m+1
		}
		if prod <= 0 {
			prod = 1
		}
		if size <= 0 {
			size = 1
		}
	}
	if p.peek(0) == '?' {
		p.pos++
	}
	return size, prod
}

func (p *subsetParser) number() int {
	start := p.pos
	for isRuleDigit(p.peek(0)) {
		p.pos++
	}
	digits := p.s[start:p.pos]
	if len(digits) == 0 || len(digits) > 4 || (len(digits) > 1 && digits[0] == '0') {
		panic(outside{})
	}
	v := 0
	for _, d := range digits {
		v = v*10 + int(d-'0')
	}
	if v > MaxRuleRepeat {
		panic(outside{})
	}
	return v
}

// single is one class item: whether it is a class escape, and its code point.
func (p *subsetParser) single() (bool, rune) {
	c := p.peek(0)
	if c < 0 {
		panic(outside{})
	}
	if c == '\\' {
		e := p.peek(1)
		if isClassEscape(e) {
			p.pos += 2
			return true, 0
		}
		if e >= 0 && isRulePunct(e) {
			p.pos += 2
			return false, e
		}
		panic(outside{})
	}
	if c == '[' {
		panic(outside{})
	}
	p.pos++
	return false, c
}

func (p *subsetParser) class() {
	p.pos++ // [
	if p.peek(0) == '^' {
		p.pos++
	}
	items := 0
	for {
		c := p.peek(0)
		if c < 0 {
			panic(outside{})
		}
		if c == ']' {
			if items == 0 {
				panic(outside{})
			}
			p.pos++
			return
		}
		if (c == '&' && p.peek(1) == '&') || (c == '~' && p.peek(1) == '~') {
			panic(outside{})
		}
		if c == '-' {
			if p.peek(1) == '-' || p.s[p.pos-1] == '-' {
				panic(outside{})
			}
			if items == 0 || p.peek(1) == ']' {
				p.pos++
				items++
				continue
			}
			panic(outside{})
		}
		isCls, lo := p.single()
		items++
		if p.peek(0) == '-' && p.peek(1) != ']' {
			if isCls || p.peek(1) == '-' || p.s[p.pos-1] == '-' {
				panic(outside{})
			}
			p.pos++
			hiCls, hi := p.single()
			if hiCls || hi < lo {
				panic(outside{})
			}
		}
	}
}
