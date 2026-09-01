// Package pyre is Python 3.12's re module for str patterns: the parser
// (re._parser, with its error messages and its tree, which regex_to_z3
// walks) and a backtracking matcher with _sre's semantics, Unicode
// classes, IGNORECASE and all.
//
// The gate's oracle runs Python regexes over the text an agent sends, and
// the envelope's predicates carry regexes an operator writes. Go's regexp
// is RE2: no lookaround, no backreferences, ASCII \b, a different \s. So
// the port matches with this package, on Python's own pattern strings.
package pyre

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"daisugi-verify/internal/pystr"
)

// Op is an sre opcode as the parser emits it.
type Op int

const (
	FAILURE Op = iota
	SUCCESS
	ANY
	ANY_ALL
	ASSERT
	ASSERT_NOT
	AT
	BRANCH
	CATEGORY
	CHARSET
	BIGCHARSET
	GROUPREF
	GROUPREF_EXISTS
	IN
	INFO
	JUMP
	LITERAL
	MARK
	MAX_UNTIL
	MIN_UNTIL
	NOT_LITERAL
	NEGATE
	RANGE
	REPEAT
	REPEAT_ONE
	SUBPATTERN
	MIN_REPEAT_ONE
	ATOMIC_GROUP
	POSSESSIVE_REPEAT
	POSSESSIVE_REPEAT_ONE
	// parser-only
	MIN_REPEAT Op = 100 + iota
	MAX_REPEAT
)

// At codes.
const (
	AT_BEGINNING = iota
	AT_BEGINNING_LINE
	AT_BEGINNING_STRING
	AT_BOUNDARY
	AT_NON_BOUNDARY
	AT_END
	AT_END_LINE
	AT_END_STRING
)

// Category codes.
const (
	CATEGORY_DIGIT = iota
	CATEGORY_NOT_DIGIT
	CATEGORY_SPACE
	CATEGORY_NOT_SPACE
	CATEGORY_WORD
	CATEGORY_NOT_WORD
)

// Flags.
const (
	FlagTemplate   = 1
	FlagIgnoreCase = 2
	FlagLocale     = 4
	FlagMultiline  = 8
	FlagDotAll     = 16
	FlagUnicode    = 32
	FlagVerbose    = 64
	FlagDebug      = 128
	FlagASCII      = 256

	typeFlags   = FlagASCII | FlagLocale | FlagUnicode
	globalFlags = FlagDebug | FlagTemplate
)

// MaxRepeat is _sre.MAXREPEAT; MaxGroups is _sre.MAXGROUPS.
const (
	MaxRepeat = 4294967295
	MaxGroups = 1073741823
	maxWidth  = 1 << 62 // stands for MAXWIDTH (1 << 64), past every real width
)

// Item is one (op, av) pair of the parser's output.
type Item struct {
	Op Op
	// Lit is the code point of LITERAL and NOT_LITERAL, the group of
	// GROUPREF and GROUPREF_EXISTS, the direction of ASSERT and ASSERT_NOT,
	// the code of AT and CATEGORY.
	Lit int
	// Lo and Hi are RANGE's bounds and a repeat's min and max.
	Lo, Hi int
	// Set is IN's items.
	Set []Item
	// Sub is the body of a repeat, SUBPATTERN, ATOMIC_GROUP, an assertion,
	// and GROUPREF_EXISTS's yes branch.
	Sub *SubPattern
	// No is GROUPREF_EXISTS's no branch, or nil.
	No *SubPattern
	// Branches are BRANCH's alternatives.
	Branches []*SubPattern
	// Group is SUBPATTERN's group number, or -1 for None.
	Group              int
	AddFlags, DelFlags int
}

// SubPattern is re._parser.SubPattern.
type SubPattern struct {
	state *State
	Data  []Item
	width *[2]int
}

// State is re._parser.State.
type State struct {
	Flags            int
	groupdict        map[string]int
	groupwidths      []*[2]int
	lookbehindgroups int // -1 for None
	grouprefpos      map[int]int
	grouprefOrder    []int
	// Warnings are the FutureWarning and DeprecationWarning texts the
	// parse would print.
	Warnings []string
}

// Groups is state.groups.
func (s *State) Groups() int { return len(s.groupwidths) }

// Error is re.error, or another exception the parse raises.
type Error struct {
	Type    string // "error", "OverflowError", "ValueError", "RecursionError"
	Msg     string // the unformatted message
	Pattern []rune
	Pos     int // -1 when none
}

func (e *Error) Error() string { return e.String() }

// String is str(exc).
func (e *Error) String() string {
	if e.Pos < 0 || e.Pattern == nil {
		return e.Msg
	}
	msg := fmt.Sprintf("%s at position %d", e.Msg, e.Pos)
	lineno := 1
	last := -1
	hasNL := false
	for i, r := range e.Pattern {
		if r == '\n' {
			hasNL = true
			if i < e.Pos {
				lineno++
				last = i
			}
		}
	}
	colno := e.Pos - last
	if hasNL {
		msg = fmt.Sprintf("%s (line %d, column %d)", msg, lineno, colno)
	}
	return msg
}

func (s *State) opengroup(name *string, src *tokenizer, nameLen int) int {
	gid := s.Groups()
	s.groupwidths = append(s.groupwidths, nil)
	if s.Groups() > MaxGroups {
		panic(src.error("too many groups", nameLen+1))
	}
	if name != nil {
		if ogid, ok := s.groupdict[*name]; ok {
			panic(src.error(fmt.Sprintf("redefinition of group name %s as group %d; was group %d",
				pystr.Repr(*name), gid, ogid), nameLen+1))
		}
		s.groupdict[*name] = gid
	}
	return gid
}

func (s *State) closegroup(gid int, p *SubPattern) {
	w := p.GetWidth()
	s.groupwidths[gid] = &w
}

func (s *State) checkgroup(gid int) bool {
	return gid < s.Groups() && s.groupwidths[gid] != nil
}

func (s *State) checklookbehindgroup(gid int, src *tokenizer) {
	if s.lookbehindgroups >= 0 {
		if !s.checkgroup(gid) {
			panic(src.error("cannot refer to an open group", 0))
		}
		if gid >= s.lookbehindgroups {
			panic(src.error("cannot refer to group defined in the same lookbehind subpattern", 0))
		}
	}
}

func newSub(s *State) *SubPattern { return &SubPattern{state: s} }

// GetWidth is SubPattern.getwidth().
func (p *SubPattern) GetWidth() [2]int {
	if p.width != nil {
		return *p.width
	}
	lo, hi := 0, 0
	add := func(a, b int) int {
		if a+b > maxWidth {
			return maxWidth
		}
		return a + b
	}
	mul := func(a, b int) int {
		if a == 0 || b == 0 {
			return 0
		}
		if a > maxWidth/b {
			return maxWidth
		}
		return a * b
	}
loop:
	for _, it := range p.Data {
		switch it.Op {
		case BRANCH:
			i, j := maxWidth, 0
			for _, b := range it.Branches {
				w := b.GetWidth()
				if w[0] < i {
					i = w[0]
				}
				if w[1] > j {
					j = w[1]
				}
			}
			lo, hi = add(lo, i), add(hi, j)
		case ATOMIC_GROUP, SUBPATTERN:
			w := it.Sub.GetWidth()
			lo, hi = add(lo, w[0]), add(hi, w[1])
		case MIN_REPEAT, MAX_REPEAT, POSSESSIVE_REPEAT:
			w := it.Sub.GetWidth()
			lo = add(lo, mul(w[0], it.Lo))
			if it.Hi == MaxRepeat && w[1] != 0 {
				hi = maxWidth
			} else {
				hi = add(hi, mul(w[1], it.Hi))
			}
		case ANY, RANGE, IN, LITERAL, NOT_LITERAL, CATEGORY:
			lo, hi = add(lo, 1), add(hi, 1)
		case GROUPREF:
			w := p.state.groupwidths[it.Lit]
			lo, hi = add(lo, w[0]), add(hi, w[1])
		case GROUPREF_EXISTS:
			w := it.Sub.GetWidth()
			i, j := w[0], w[1]
			if it.No != nil {
				w2 := it.No.GetWidth()
				if w2[0] < i {
					i = w2[0]
				}
				if w2[1] > j {
					j = w2[1]
				}
			} else {
				i = 0
			}
			lo, hi = add(lo, i), add(hi, j)
		case SUCCESS:
			break loop
		}
	}
	w := [2]int{lo, hi}
	p.width = &w
	return w
}

// tokenizer is re._parser.Tokenizer for a str pattern.
type tokenizer struct {
	s     []rune
	index int
	next  string // "" is None
	has   bool
}

func newTokenizer(s []rune) *tokenizer {
	t := &tokenizer{s: s}
	t.advance()
	return t
}

func (t *tokenizer) advance() {
	index := t.index
	if index >= len(t.s) {
		t.next, t.has = "", false
		return
	}
	c := string(pystr.FromRunes(t.s[index : index+1]))
	if t.s[index] == '\\' {
		index++
		if index >= len(t.s) {
			panic(&Error{Type: "error", Msg: "bad escape (end of pattern)", Pattern: t.s, Pos: len(t.s) - 1})
		}
		c += pystr.FromRunes(t.s[index : index+1])
	}
	t.index = index + 1
	t.next, t.has = c, true
}

func (t *tokenizer) match(c string) bool {
	if t.has && c == t.next {
		t.advance()
		return true
	}
	return false
}

func (t *tokenizer) get() (string, bool) {
	this, has := t.next, t.has
	t.advance()
	return this, has
}

func (t *tokenizer) getwhile(n int, charset func(string) bool) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if !t.has || !charset(t.next) {
			break
		}
		b.WriteString(t.next)
		t.advance()
	}
	return b.String()
}

func (t *tokenizer) getuntil(terminator, name string) string {
	var result []rune
	for {
		c, has := t.next, t.has
		t.advance()
		if !has {
			if len(result) == 0 {
				panic(t.error("missing "+name, 0))
			}
			panic(t.error(fmt.Sprintf("missing %s, unterminated name", terminator), len(result)))
		}
		if c == terminator {
			if len(result) == 0 {
				panic(t.error("missing "+name, 1))
			}
			break
		}
		result = append(result, pystr.Runes(c)...)
	}
	return pystr.FromRunes(result)
}

func (t *tokenizer) tell() int {
	if !t.has {
		return t.index
	}
	return t.index - pystr.Len(t.next)
}

func (t *tokenizer) seek(i int) {
	t.index = i
	t.advance()
}

func (t *tokenizer) error(msg string, offset int) *Error {
	return &Error{Type: "error", Msg: msg, Pattern: t.s, Pos: t.tell() - offset}
}

func (t *tokenizer) checkgroupname(name string, offset int) {
	if !isIdentifier(name) {
		panic(t.error("bad character in group name "+pystr.Repr(name), pystr.Len(name)+offset))
	}
}

// isIdentifier is str.isidentifier: XID_Start or _, then XID_Continue.
// Go's tables stand in: letters, Nl and _ start; those, marks, Nd and Pc
// continue.
func isIdentifier(s string) bool {
	rs := pystr.Runes(s)
	if len(rs) == 0 {
		return false
	}
	for i, r := range rs {
		start := r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_ID_Start, r)
		if i == 0 {
			if !start {
				return false
			}
			continue
		}
		if !(start || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Nd, r) ||
			unicode.Is(unicode.Pc, r) || unicode.Is(unicode.Other_ID_Continue, r)) {
			return false
		}
	}
	return true
}

func isDigitStr(s string) bool { return len(s) == 1 && s[0] >= '0' && s[0] <= '9' }
func isOctStr(s string) bool   { return len(s) == 1 && s[0] >= '0' && s[0] <= '7' }
func isHexStr(s string) bool {
	return len(s) == 1 && strings.ContainsRune("0123456789abcdefABCDEF", rune(s[0]))
}
func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

var escapes = map[string]int{
	`\a`: '\a', `\b`: '\b', `\f`: '\f', `\n`: '\n', `\r`: '\r', `\t`: '\t', `\v`: '\v', `\\`: '\\',
}

func categoryItem(esc string) (Item, bool) {
	switch esc {
	case `\A`:
		return Item{Op: AT, Lit: AT_BEGINNING_STRING}, true
	case `\b`:
		return Item{Op: AT, Lit: AT_BOUNDARY}, true
	case `\B`:
		return Item{Op: AT, Lit: AT_NON_BOUNDARY}, true
	case `\d`:
		return Item{Op: IN, Set: []Item{{Op: CATEGORY, Lit: CATEGORY_DIGIT}}}, true
	case `\D`:
		return Item{Op: IN, Set: []Item{{Op: CATEGORY, Lit: CATEGORY_NOT_DIGIT}}}, true
	case `\s`:
		return Item{Op: IN, Set: []Item{{Op: CATEGORY, Lit: CATEGORY_SPACE}}}, true
	case `\S`:
		return Item{Op: IN, Set: []Item{{Op: CATEGORY, Lit: CATEGORY_NOT_SPACE}}}, true
	case `\w`:
		return Item{Op: IN, Set: []Item{{Op: CATEGORY, Lit: CATEGORY_WORD}}}, true
	case `\W`:
		return Item{Op: IN, Set: []Item{{Op: CATEGORY, Lit: CATEGORY_NOT_WORD}}}, true
	case `\Z`:
		return Item{Op: AT, Lit: AT_END_STRING}, true
	}
	return Item{}, false
}

func hexEscape(src *tokenizer, escape string, n int) Item {
	escape += src.getwhile(n, isHexStr)
	if len(escape) != n+2 {
		panic(src.error("incomplete escape "+escape, pystr.Len(escape)))
	}
	v, _ := strconv.ParseInt(escape[2:], 16, 64)
	if n == 8 && v > 0x10FFFF {
		// chr(c) raises ValueError, which falls through to "bad escape".
		panic(src.error("bad escape "+escape, pystr.Len(escape)))
	}
	return Item{Op: LITERAL, Lit: int(v)}
}

func namedEscape(src *tokenizer) Item {
	if !src.match("{") {
		panic(src.error("missing {", 0))
	}
	charname := src.getuntil("}", "character name")
	r, ok, exc := lookupName(charname)
	if exc != nil {
		// The name cannot be encoded (a lone surrogate): lookup raises
		// UnicodeEncodeError, a ValueError, which re reads as a bad escape.
		panic(src.error(`bad escape \N`, 2))
	}
	if !ok {
		panic(src.error("undefined character name "+pystr.Repr(charname), pystr.Len(charname)+len(`\N{}`)))
	}
	return Item{Op: LITERAL, Lit: int(r)}
}

// classEscape is _class_escape.
func classEscape(src *tokenizer, escape string) Item {
	if v, ok := escapes[escape]; ok {
		return Item{Op: LITERAL, Lit: v}
	}
	if it, ok := categoryItem(escape); ok && it.Op == IN {
		return it
	}
	c := ""
	if len(escape) >= 2 {
		c = pystr.Slice(escape, 1, 2)
	}
	switch {
	case c == "x":
		return hexEscape(src, escape, 2)
	case c == "u":
		return hexEscape(src, escape, 4)
	case c == "U":
		return hexEscape(src, escape, 8)
	case c == "N":
		return namedEscape(src)
	case isOctStr(c):
		escape += src.getwhile(2, isOctStr)
		v, _ := strconv.ParseInt(escape[1:], 8, 64)
		if v > 0o377 {
			panic(src.error(fmt.Sprintf("octal escape value %s outside of range 0-0o377", escape), pystr.Len(escape)))
		}
		return Item{Op: LITERAL, Lit: int(v)}
	case isDigitStr(c):
		panic(src.error("bad escape "+escape, pystr.Len(escape)))
	}
	if pystr.Len(escape) == 2 {
		if len(c) == 1 && isASCIILetter(c[0]) {
			panic(src.error("bad escape "+escape, pystr.Len(escape)))
		}
		return Item{Op: LITERAL, Lit: int(pystr.Runes(escape)[1])}
	}
	panic(src.error("bad escape "+escape, pystr.Len(escape)))
}

// escapeItem is _escape.
func escapeItem(src *tokenizer, escape string, state *State) Item {
	if it, ok := categoryItem(escape); ok {
		return it
	}
	if v, ok := escapes[escape]; ok {
		return Item{Op: LITERAL, Lit: v}
	}
	c := pystr.Slice(escape, 1, 2)
	switch {
	case c == "x":
		return hexEscape(src, escape, 2)
	case c == "u":
		return hexEscape(src, escape, 4)
	case c == "U":
		return hexEscape(src, escape, 8)
	case c == "N":
		return namedEscape(src)
	case c == "0":
		escape += src.getwhile(2, isOctStr)
		v, _ := strconv.ParseInt(escape[1:], 8, 64)
		return Item{Op: LITERAL, Lit: int(v)}
	case isDigitStr(c):
		if src.has && isDigitStr(src.next) {
			n, _ := src.get()
			escape += n
			if isOctStr(escape[1:2]) && isOctStr(escape[2:3]) && src.has && isOctStr(src.next) {
				n, _ := src.get()
				escape += n
				v, _ := strconv.ParseInt(escape[1:], 8, 64)
				if v > 0o377 {
					panic(src.error(fmt.Sprintf("octal escape value %s outside of range 0-0o377", escape), pystr.Len(escape)))
				}
				return Item{Op: LITERAL, Lit: int(v)}
			}
		}
		group, _ := strconv.Atoi(escape[1:])
		if group < state.Groups() {
			if !state.checkgroup(group) {
				panic(src.error("cannot refer to an open group", pystr.Len(escape)))
			}
			state.checklookbehindgroup(group, src)
			return Item{Op: GROUPREF, Lit: group}
		}
		panic(src.error(fmt.Sprintf("invalid group reference %d", group), pystr.Len(escape)-1))
	}
	if pystr.Len(escape) == 2 {
		if len(c) == 1 && isASCIILetter(c[0]) {
			panic(src.error("bad escape "+escape, pystr.Len(escape)))
		}
		return Item{Op: LITERAL, Lit: int(pystr.Runes(escape)[1])}
	}
	panic(src.error("bad escape "+escape, pystr.Len(escape)))
}

// itemEqual is Python's == on two (op, av) tuples. A SubPattern compares
// by identity, so an item that holds one equals only itself.
func itemEqual(a, b Item) bool {
	if a.Op != b.Op {
		return false
	}
	switch a.Op {
	case LITERAL, NOT_LITERAL, GROUPREF, AT, CATEGORY:
		return a.Lit == b.Lit
	case ANY, NEGATE:
		return true
	case RANGE:
		return a.Lo == b.Lo && a.Hi == b.Hi
	case IN:
		if len(a.Set) != len(b.Set) {
			return false
		}
		for i := range a.Set {
			if !itemEqual(a.Set[i], b.Set[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func uniq(items []Item) []Item {
	var out []Item
outer:
	for _, it := range items {
		for _, o := range out {
			if itemEqual(o, it) {
				continue outer
			}
		}
		out = append(out, it)
	}
	return out
}

// parser carries the frame depth for Python's recursion limit.
type parser struct {
	src   *tokenizer
	state *State
	depth int
}

func (ps *parser) enter() func() {
	ps.depth++
	if ps.depth > 1000 {
		panic(&Error{Type: "RecursionError", Msg: "maximum recursion depth exceeded", Pos: -1})
	}
	return func() { ps.depth-- }
}

// parseSub is _parse_sub.
func (ps *parser) parseSub(verbose bool, nested int) *SubPattern {
	defer ps.enter()()
	var items []*SubPattern
	for {
		items = append(items, ps.parse(verbose, nested+1, nested == 0 && len(items) == 0))
		if !ps.src.match("|") {
			break
		}
		if nested == 0 {
			verbose = ps.state.Flags&FlagVerbose != 0
		}
	}
	if len(items) == 1 {
		return items[0]
	}
	sub := newSub(ps.state)
	// check if all items share a common prefix
	for {
		var prefix *Item
		same := true
		for _, item := range items {
			if len(item.Data) == 0 {
				same = false
				break
			}
			if prefix == nil {
				p := item.Data[0]
				prefix = &p
			} else if !itemEqual(item.Data[0], *prefix) {
				same = false
				break
			}
		}
		if !same {
			break
		}
		for _, item := range items {
			item.Data = item.Data[1:]
		}
		sub.Data = append(sub.Data, *prefix)
	}
	// check if the branch can be replaced by a character set
	var set []Item
	ok := true
	for _, item := range items {
		if len(item.Data) != 1 {
			ok = false
			break
		}
		it := item.Data[0]
		if it.Op == LITERAL {
			set = append(set, it)
		} else if it.Op == IN && it.Set[0].Op != NEGATE {
			set = append(set, it.Set...)
		} else {
			ok = false
			break
		}
	}
	if ok {
		sub.Data = append(sub.Data, Item{Op: IN, Set: uniq(set)})
		return sub
	}
	sub.Data = append(sub.Data, Item{Op: BRANCH, Branches: items})
	return sub
}

func (ps *parser) warn(msg string) { ps.state.Warnings = append(ps.state.Warnings, msg) }

// parse is _parse.
func (ps *parser) parse(verbose bool, nested int, first bool) *SubPattern {
	defer ps.enter()()
	src, state := ps.src, ps.state
	sub := newSub(state)
	for {
		if !src.has {
			break
		}
		this := src.next
		if this == "|" || this == ")" {
			break
		}
		src.advance()
		if verbose {
			if this == " " || this == "\t" || this == "\n" || this == "\r" || this == "\v" || this == "\f" {
				continue
			}
			if this == "#" {
				for {
					t, has := src.get()
					if !has || t == "\n" {
						break
					}
				}
				continue
			}
		}
		switch {
		case this[0] == '\\':
			sub.Data = append(sub.Data, escapeItem(src, this, state))
		case !strings.Contains(`.\[{()*+?^$|`, this):
			sub.Data = append(sub.Data, Item{Op: LITERAL, Lit: int(pystr.Runes(this)[0])})
		case this == "[":
			here := src.tell() - 1
			var set []Item
			if src.has && src.next == "[" {
				ps.warn(fmt.Sprintf("Possible nested set at position %d", src.tell()))
			}
			negate := src.match("^")
			for {
				this, has := src.get()
				if !has {
					panic(src.error("unterminated character set", src.tell()-here))
				}
				if this == "]" && len(set) > 0 {
					break
				}
				var code1 Item
				if this[0] == '\\' {
					code1 = classEscape(src, this)
				} else {
					if len(set) > 0 && strings.Contains("-&~|", this) && src.has && src.next == this {
						kind := map[string]string{"-": "difference", "&": "intersection", "~": "symmetric difference", "|": "union"}[this]
						ps.warn(fmt.Sprintf("Possible set %s at position %d", kind, src.tell()-1))
					}
					code1 = Item{Op: LITERAL, Lit: int(pystr.Runes(this)[0])}
				}
				if src.match("-") {
					that, has := src.get()
					if !has {
						panic(src.error("unterminated character set", src.tell()-here))
					}
					if that == "]" {
						if code1.Op == IN {
							code1 = code1.Set[0]
						}
						set = append(set, code1, Item{Op: LITERAL, Lit: '-'})
						break
					}
					var code2 Item
					if that[0] == '\\' {
						code2 = classEscape(src, that)
					} else {
						if that == "-" {
							ps.warn(fmt.Sprintf("Possible set difference at position %d", src.tell()-2))
						}
						code2 = Item{Op: LITERAL, Lit: int(pystr.Runes(that)[0])}
					}
					if code1.Op != LITERAL || code2.Op != LITERAL {
						msg := fmt.Sprintf("bad character range %s-%s", this, that)
						panic(src.error(msg, pystr.Len(this)+1+pystr.Len(that)))
					}
					lo, hi := code1.Lit, code2.Lit
					if hi < lo {
						msg := fmt.Sprintf("bad character range %s-%s", this, that)
						panic(src.error(msg, pystr.Len(this)+1+pystr.Len(that)))
					}
					set = append(set, Item{Op: RANGE, Lo: lo, Hi: hi})
				} else {
					if code1.Op == IN {
						code1 = code1.Set[0]
					}
					set = append(set, code1)
				}
			}
			set = uniq(set)
			if len(set) == 1 && set[0].Op == LITERAL {
				if negate {
					sub.Data = append(sub.Data, Item{Op: NOT_LITERAL, Lit: set[0].Lit})
				} else {
					sub.Data = append(sub.Data, set[0])
				}
			} else {
				if negate {
					set = append([]Item{{Op: NEGATE}}, set...)
				}
				sub.Data = append(sub.Data, Item{Op: IN, Set: set})
			}
		case strings.Contains("*+?{", this):
			here := src.tell()
			min, max := 0, MaxRepeat
			switch this {
			case "?":
				min, max = 0, 1
			case "*":
				min, max = 0, MaxRepeat
			case "+":
				min, max = 1, MaxRepeat
			case "{":
				if src.has && src.next == "}" {
					sub.Data = append(sub.Data, Item{Op: LITERAL, Lit: '{'})
					continue
				}
				lo, hi := "", ""
				for src.has && isDigitStr(src.next) {
					t, _ := src.get()
					lo += t
				}
				if src.match(",") {
					for src.has && isDigitStr(src.next) {
						t, _ := src.get()
						hi += t
					}
				} else {
					hi = lo
				}
				if !src.match("}") {
					sub.Data = append(sub.Data, Item{Op: LITERAL, Lit: '{'})
					src.seek(here)
					continue
				}
				if lo != "" {
					min = parseCount(lo)
				}
				if hi != "" {
					max = parseCount(hi)
					if max < min {
						panic(src.error("min repeat greater than max repeat", src.tell()-here))
					}
				}
			}
			var item *SubPattern
			if len(sub.Data) > 0 {
				// subpattern[-1:] is a new list.
				item = &SubPattern{state: state, Data: append([]Item{}, sub.Data[len(sub.Data)-1:]...)}
			}
			if item == nil || item.Data[0].Op == AT {
				panic(src.error("nothing to repeat", src.tell()-here+pystr.Len(this)))
			}
			switch item.Data[0].Op {
			case MIN_REPEAT, MAX_REPEAT, POSSESSIVE_REPEAT:
				panic(src.error("multiple repeat", src.tell()-here+pystr.Len(this)))
			}
			if item.Data[0].Op == SUBPATTERN {
				it := item.Data[0]
				if it.Group < 0 && it.AddFlags == 0 && it.DelFlags == 0 {
					item = it.Sub
				}
			}
			op := MAX_REPEAT
			if src.match("?") {
				op = MIN_REPEAT
			} else if src.match("+") {
				op = POSSESSIVE_REPEAT
			}
			sub.Data[len(sub.Data)-1] = Item{Op: op, Lo: min, Hi: max, Sub: item}
		case this == ".":
			sub.Data = append(sub.Data, Item{Op: ANY})
		case this == "(":
			start := src.tell() - 1
			capture, atomic := true, false
			var name *string
			addFlags, delFlags := 0, 0
			if src.match("?") {
				char, has := src.get()
				if !has {
					panic(src.error("unexpected end of pattern", 0))
				}
				switch {
				case char == "P":
					if src.match("<") {
						n := src.getuntil(">", "group name")
						src.checkgroupname(n, 1)
						name = &n
					} else if src.match("=") {
						n := src.getuntil(")", "group name")
						src.checkgroupname(n, 1)
						gid, ok := state.groupdict[n]
						if !ok {
							panic(src.error("unknown group name "+pystr.Repr(n), pystr.Len(n)+1))
						}
						if !state.checkgroup(gid) {
							panic(src.error("cannot refer to an open group", pystr.Len(n)+1))
						}
						state.checklookbehindgroup(gid, src)
						sub.Data = append(sub.Data, Item{Op: GROUPREF, Lit: gid})
						continue
					} else {
						c, has := src.get()
						if !has {
							panic(src.error("unexpected end of pattern", 0))
						}
						panic(src.error("unknown extension ?P"+c, pystr.Len(c)+2))
					}
				case char == ":":
					capture = false
				case char == "#":
					for {
						if !src.has {
							panic(src.error("missing ), unterminated comment", src.tell()-start))
						}
						if t, _ := src.get(); t == ")" {
							break
						}
					}
					continue
				case char == "=" || char == "!" || char == "<":
					dir := 1
					lookbehindgroups := -2 // "not set here"
					if char == "<" {
						c, has := src.get()
						if !has {
							panic(src.error("unexpected end of pattern", 0))
						}
						if c != "=" && c != "!" {
							panic(src.error("unknown extension ?<"+c, pystr.Len(c)+2))
						}
						char = c
						dir = -1
						lookbehindgroups = state.lookbehindgroups
						if lookbehindgroups < 0 {
							state.lookbehindgroups = state.Groups()
						}
					}
					p := ps.parseSub(verbose, nested+1)
					if dir < 0 && lookbehindgroups < 0 {
						state.lookbehindgroups = -1
					}
					if !src.match(")") {
						panic(src.error("missing ), unterminated subpattern", src.tell()-start))
					}
					op := ASSERT
					if char != "=" {
						op = ASSERT_NOT
					}
					sub.Data = append(sub.Data, Item{Op: op, Lit: dir, Sub: p})
					continue
				case char == "(":
					condname := src.getuntil(")", "group name")
					var condgroup int
					if !(isASCIIDecimal(condname)) {
						src.checkgroupname(condname, 1)
						g, ok := state.groupdict[condname]
						if !ok {
							panic(src.error("unknown group name "+pystr.Repr(condname), pystr.Len(condname)+1))
						}
						condgroup = g
					} else {
						condgroup = parseCount(condname)
						if condgroup == 0 {
							panic(src.error("bad group number", pystr.Len(condname)+1))
						}
						if condgroup >= MaxGroups {
							panic(src.error(fmt.Sprintf("invalid group reference %d", condgroup), pystr.Len(condname)+1))
						}
						if _, ok := state.grouprefpos[condgroup]; !ok {
							state.grouprefpos[condgroup] = src.tell() - pystr.Len(condname) - 1
							state.grouprefOrder = append(state.grouprefOrder, condgroup)
						}
					}
					state.checklookbehindgroup(condgroup, src)
					yes := ps.parse(verbose, nested+1, false)
					var no *SubPattern
					if src.match("|") {
						no = ps.parse(verbose, nested+1, false)
						if src.has && src.next == "|" {
							panic(src.error("conditional backref with more than two branches", 0))
						}
					}
					if !src.match(")") {
						panic(src.error("missing ), unterminated subpattern", src.tell()-start))
					}
					sub.Data = append(sub.Data, Item{Op: GROUPREF_EXISTS, Lit: condgroup, Sub: yes, No: no})
					continue
				case char == ">":
					capture, atomic = false, true
				case isFlagChar(char) || char == "-":
					fl, global := ps.parseFlags(char)
					if global {
						if !first || len(sub.Data) > 0 {
							panic(src.error("global flags not at the start of the expression", src.tell()-start))
						}
						verbose = state.Flags&FlagVerbose != 0
						continue
					}
					addFlags, delFlags = fl[0], fl[1]
					capture = false
				default:
					panic(src.error("unknown extension ?"+char, pystr.Len(char)+1))
				}
			}
			group := -1
			if capture {
				nameLen := 0
				if name != nil {
					nameLen = pystr.Len(*name)
				}
				group = state.opengroup(name, src, nameLen)
			}
			subVerbose := (verbose || addFlags&FlagVerbose != 0) && delFlags&FlagVerbose == 0
			p := ps.parseSub(subVerbose, nested+1)
			if !src.match(")") {
				panic(src.error("missing ), unterminated subpattern", src.tell()-start))
			}
			if group >= 0 {
				state.closegroup(group, p)
			}
			if atomic {
				sub.Data = append(sub.Data, Item{Op: ATOMIC_GROUP, Sub: p})
			} else {
				sub.Data = append(sub.Data, Item{Op: SUBPATTERN, Group: group, AddFlags: addFlags, DelFlags: delFlags, Sub: p})
			}
		case this == "^":
			sub.Data = append(sub.Data, Item{Op: AT, Lit: AT_BEGINNING})
		case this == "$":
			sub.Data = append(sub.Data, Item{Op: AT, Lit: AT_END})
		}
	}
	// unpack non-capturing groups
	for i := len(sub.Data) - 1; i >= 0; i-- {
		it := sub.Data[i]
		if it.Op == SUBPATTERN && it.Group < 0 && it.AddFlags == 0 && it.DelFlags == 0 {
			rest := append([]Item{}, sub.Data[i+1:]...)
			sub.Data = append(append(sub.Data[:i], it.Sub.Data...), rest...)
		}
	}
	return sub
}

func isASCIIDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseCount is int() of a digit string, raising OverflowError at MAXREPEAT
// as the parser does.
func parseCount(s string) int {
	s = strings.TrimLeft(s, "0")
	if len(s) > 10 {
		panic(&Error{Type: "OverflowError", Msg: "the repetition number is too large", Pos: -1})
	}
	v, _ := strconv.ParseInt("0"+s, 10, 64)
	if v >= MaxRepeat {
		panic(&Error{Type: "OverflowError", Msg: "the repetition number is too large", Pos: -1})
	}
	return int(v)
}

var flagChars = map[string]int{
	"i": FlagIgnoreCase, "L": FlagLocale, "m": FlagMultiline, "s": FlagDotAll,
	"x": FlagVerbose, "a": FlagASCII, "t": FlagTemplate, "u": FlagUnicode,
}

func isFlagChar(c string) bool { _, ok := flagChars[c]; return ok }

func isAlphaStr(c string) bool {
	rs := pystr.Runes(c)
	return len(rs) == 1 && unicode.IsLetter(rs[0])
}

// parseFlags is _parse_flags; global is true for (?flags) with no colon.
func (ps *parser) parseFlags(char string) ([2]int, bool) {
	src, state := ps.src, ps.state
	add, del := 0, 0
	if char != "-" {
		for {
			flag := flagChars[char]
			if char == "L" {
				panic(src.error("bad inline flags: cannot use 'L' flag with a str pattern", 0))
			}
			add |= flag
			if flag&typeFlags != 0 && add&typeFlags != flag {
				panic(src.error("bad inline flags: flags 'a', 'u' and 'L' are incompatible", 0))
			}
			c, has := src.get()
			if !has {
				panic(src.error("missing -, : or )", 0))
			}
			char = c
			if char == ")" || char == "-" || char == ":" {
				break
			}
			if !isFlagChar(char) {
				msg := "missing -, : or )"
				if isAlphaStr(char) {
					msg = "unknown flag"
				}
				panic(src.error(msg, pystr.Len(char)))
			}
		}
	}
	if char == ")" {
		state.Flags |= add
		return [2]int{}, true
	}
	if add&globalFlags != 0 {
		panic(src.error("bad inline flags: cannot turn on global flag", 1))
	}
	if char == "-" {
		c, has := src.get()
		if !has {
			panic(src.error("missing flag", 0))
		}
		char = c
		if !isFlagChar(char) {
			msg := "missing flag"
			if isAlphaStr(char) {
				msg = "unknown flag"
			}
			panic(src.error(msg, pystr.Len(char)))
		}
		for {
			flag := flagChars[char]
			if flag&typeFlags != 0 {
				panic(src.error("bad inline flags: cannot turn off flags 'a', 'u' and 'L'", 0))
			}
			del |= flag
			c, has := src.get()
			if !has {
				panic(src.error("missing :", 0))
			}
			char = c
			if char == ":" {
				break
			}
			if !isFlagChar(char) {
				msg := "missing :"
				if isAlphaStr(char) {
					msg = "unknown flag"
				}
				panic(src.error(msg, pystr.Len(char)))
			}
		}
	}
	if del&globalFlags != 0 {
		panic(src.error("bad inline flags: cannot turn off global flag", 1))
	}
	if add&del != 0 {
		panic(src.error("bad inline flags: flag turned on and off", 1))
	}
	return [2]int{add, del}, false
}

// Parse is re._parser.parse(pattern, flags). depth is the Python frame
// depth of parse's caller, for the recursion limit.
func Parse(pattern string, flags int, depth int) (p *SubPattern, err *Error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Error); ok {
				p, err = nil, e
				return
			}
			panic(r)
		}
	}()
	rs := pystr.Runes(pattern)
	state := &State{Flags: flags, groupdict: map[string]int{}, groupwidths: []*[2]int{nil},
		lookbehindgroups: -1, grouprefpos: map[int]int{}}
	// parse (depth+1) calls Tokenizer (depth+2) and _parse_sub (depth+2).
	ps := &parser{state: state, depth: depth + 1}
	func() {
		defer ps.enter()()
		ps.src = newTokenizer(rs)
	}()
	sub := ps.parseSub(flags&FlagVerbose != 0, 0)
	state.Flags = fixFlags(state.Flags)
	if ps.src.has {
		panic(ps.src.error("unbalanced parenthesis", 0))
	}
	for _, g := range state.grouprefOrder {
		if g >= state.Groups() {
			panic(&Error{Type: "error", Msg: fmt.Sprintf("invalid group reference %d", g), Pattern: rs, Pos: state.grouprefpos[g]})
		}
	}
	return sub, nil
}

func fixFlags(flags int) int {
	if flags&FlagLocale != 0 {
		panic(&Error{Type: "ValueError", Msg: "cannot use LOCALE flag with a str pattern", Pos: -1})
	}
	if flags&FlagASCII == 0 {
		flags |= FlagUnicode
	} else if flags&FlagUnicode != 0 {
		panic(&Error{Type: "ValueError", Msg: "ASCII and UNICODE flags are incompatible", Pos: -1})
	}
	return flags
}

// State returns the pattern's parse state.
func (p *SubPattern) State() *State { return p.state }
