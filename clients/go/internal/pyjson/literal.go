package pyjson

import (
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pystr"
)

// LitStatus is what ast.literal_eval does with a text.
type LitStatus int

const (
	// LitValue: literal_eval returns Literal.Value.
	LitValue LitStatus = iota
	// LitNone: literal_eval raises one of the errors decode_dict_text
	// catches (SyntaxError, ValueError, TypeError), so it returns None.
	LitNone
	// LitRefused: Python's answer is one this port does not model. With a
	// Value, the value holds an Opaque somewhere, or a dict lost a key
	// that is not a str; with no Value, Python's parser prints a
	// SyntaxWarning on stderr while it reads the text.
	LitRefused
)

// Opaque stands in a literal for a value this port does not model: a
// set, bytes, a complex number, Ellipsis, or a str with a \N{...} escape.
type Opaque struct{ Why string }

// Literal is the result of LiteralEval.
type Literal struct {
	Status LitStatus
	Value  any
	// Why names what is not modelled (LitRefused).
	Why string
	// Tuple is set when the value holds a tuple, read here as a list.
	Tuple bool
}

// LiteralEval is ast.literal_eval(text) on Python 3.12, for text that
// decode_dict_text hands it: dicts, lists, tuples (read as lists, with
// Tuple set), str in every quote and prefix form with Python's escapes
// and implicit concatenation, ints in every base with underscores
// (any size), floats, True, False and None. Every text literal_eval
// raises on reads as LitNone. A set, bytes, a complex number, Ellipsis,
// a \N{...} escape and a dict key that is not a str read as LitRefused,
// as does any text whose tokens make Python's parser print a
// SyntaxWarning (an invalid escape, an octal escape above 0o377, a
// number run into a keyword), since Python then writes to stderr.
func LiteralEval(text string) Literal {
	if strings.IndexByte(text, 0) >= 0 {
		// "source code string cannot contain null bytes".
		return Literal{Status: LitNone}
	}
	if !utf8.ValidString(text) {
		// A lone surrogate does not encode to UTF-8 (UnicodeEncodeError).
		return Literal{Status: LitNone}
	}
	// literal_eval strips spaces and tabs; the tokenizer translates
	// \r\n and \r to \n.
	src := strings.TrimLeft(text, " \t")
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	lx := &litLexer{s: src}
	lx.run()
	if lx.warn != "" {
		return Literal{Status: LitRefused, Why: lx.warn}
	}
	if lx.err {
		return Literal{Status: LitNone}
	}
	p := &litParser{toks: lx.toks}
	n, ok := p.top()
	if !ok {
		return Literal{Status: LitNone}
	}
	ev := &litEval{}
	v, ok := ev.eval(n)
	if !ok {
		return Literal{Status: LitNone}
	}
	if ev.why != "" {
		return Literal{Status: LitRefused, Value: v, Why: ev.why, Tuple: ev.tuple}
	}
	return Literal{Status: LitValue, Value: v, Tuple: ev.tuple}
}

// --- tokens ---

type litTokKind int

const (
	tkNum litTokKind = iota
	tkStr
	tkName
	tkOp
	tkNewline
	tkEnd
)

type litTok struct {
	kind litTokKind
	text string // the token's source text (a string's body for tkStr)
	// For tkStr: the prefix letters, lower-cased, and whether the quote
	// is tripled.
	prefix string
}

type litLexer struct {
	s     string
	i     int
	toks  []litTok
	stack []byte // open brackets
	err   bool   // the tokenizer raised: no token after this
	warn  string // a SyntaxWarning Python prints
}

const litMaxLevel = 200

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigitB(c byte) bool      { return c >= '0' && c <= '9' }
func isHexB(c byte) bool        { return isDigitB(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// isIdentStart and isIdentChar are the tokenizer's tests; any byte of
// a non-ASCII character counts (it reads such a character as part of a
// name, or stops on it, and both end in a SyntaxError here).
func isIdentStart(c byte) bool { return isASCIILetter(c) || c == '_' || c >= 0x80 }
func isIdentChar(c byte) bool  { return isIdentStart(c) || isDigitB(c) }

func (lx *litLexer) at(k int) byte {
	if lx.i+k < len(lx.s) {
		return lx.s[lx.i+k]
	}
	return 0
}

func (lx *litLexer) push(k litTokKind, text string) {
	lx.toks = append(lx.toks, litTok{kind: k, text: text})
}

// run tokenizes the whole text, as Python does while it parses it and,
// after a syntax error, while it checks the rest of the source. It
// stops at the first error the tokenizer raises. A string's escapes are
// checked here too: Python warns on an invalid one when it reads the
// string.
func (lx *litLexer) run() {
	for !lx.err && lx.warn == "" {
		c := lx.at(0)
		switch {
		case lx.i >= len(lx.s):
			lx.push(tkEnd, "")
			return
		case c == ' ' || c == '\t' || c == '\f':
			lx.i++
		case c == '#':
			for lx.i < len(lx.s) && lx.s[lx.i] != '\n' {
				lx.i++
			}
		case c == '\n':
			lx.i++
			if len(lx.stack) == 0 {
				lx.push(tkNewline, "\n")
			}
		case c == '\\':
			if lx.at(1) == '\n' {
				lx.i += 2
				continue
			}
			lx.err = true
		case isDigitB(c) || c == '.' && isDigitB(lx.at(1)):
			lx.number()
		case c == '.':
			if lx.at(1) == '.' && lx.at(2) == '.' {
				lx.push(tkOp, "...")
				lx.i += 3
			} else {
				lx.push(tkOp, ".")
				lx.i++
			}
		case c == '\'' || c == '"':
			lx.str("")
		case isIdentStart(c):
			lx.name()
		case c == '(' || c == '[' || c == '{':
			if len(lx.stack) >= litMaxLevel {
				lx.err = true // "too many nested parentheses"
				return
			}
			lx.stack = append(lx.stack, c)
			lx.push(tkOp, string(c))
			lx.i++
		case c == ')' || c == ']' || c == '}':
			open := map[byte]byte{')': '(', ']': '[', '}': '{'}[c]
			if len(lx.stack) == 0 || lx.stack[len(lx.stack)-1] != open {
				lx.err = true // unmatched or not matching
				return
			}
			lx.stack = lx.stack[:len(lx.stack)-1]
			lx.push(tkOp, string(c))
			lx.i++
		case c == '*' && lx.at(1) == '*':
			lx.push(tkOp, "**")
			lx.i += 2
		default:
			// Another operator: the parser fails on it. A byte the
			// tokenizer rejects ($, ?, `) also ends here in a SyntaxError;
			// reading on can only find a warning Python would not print,
			// which refuses rather than answers wrongly.
			lx.push(tkOp, string(c))
			lx.i++
		}
	}
}

func (lx *litLexer) name() {
	start := lx.i
	// String prefixes, as the tokenizer reads them.
	var b, r, u, f bool
	for {
		c := lx.at(0)
		lc := c | 0x20
		switch {
		case lc == 'b' && !(b || u || f):
			b = true
		case lc == 'u' && !(b || u || r || f):
			u = true
		case lc == 'r' && !(r || u):
			r = true
		case lc == 'f' && !(f || b || u):
			f = true
		default:
			goto ident
		}
		lx.i++
		if q := lx.at(0); q == '\'' || q == '"' {
			if f {
				// An f-string: its own tokens, and it is never a literal.
				// Python may still warn inside it; refuse.
				lx.warn = "an f-string"
				return
			}
			lx.str(strings.ToLower(lx.s[start:lx.i]))
			return
		}
	}
ident:
	for lx.i < len(lx.s) && isIdentChar(lx.s[lx.i]) {
		lx.i++
	}
	lx.push(tkName, lx.s[start:lx.i])
}

// str reads one string token; prefix is its prefix, lower-cased.
func (lx *litLexer) str(prefix string) {
	q := lx.at(0)
	triple := lx.at(1) == q && lx.at(2) == q
	n := 1
	if triple {
		n = 3
	}
	lx.i += n
	start := lx.i
	for {
		if lx.i >= len(lx.s) {
			lx.err = true // unterminated
			return
		}
		c := lx.s[lx.i]
		if c == '\\' {
			lx.i += 2
			if lx.i > len(lx.s) {
				lx.err = true
				return
			}
			continue
		}
		if c == '\n' && !triple {
			lx.err = true // unterminated string literal
			return
		}
		if c == q && (!triple || lx.at(1) == q && lx.at(2) == q) {
			body := lx.s[start:lx.i]
			lx.i += n
			lx.toks = append(lx.toks, litTok{kind: tkStr, text: body, prefix: prefix + map[bool]string{true: "3"}[triple]})
			if w := escapeWarning(body, prefix); w != "" {
				lx.warn = w
			}
			return
		}
		lx.i++
	}
}

// escapeWarning is the SyntaxWarning Python prints for a string body,
// or "".
func escapeWarning(body, prefix string) string {
	if strings.Contains(prefix, "r") {
		return ""
	}
	isBytes := strings.Contains(prefix, "b")
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) {
			return ""
		}
		c := body[i]
		switch {
		case c == '\n' || c == '\\' || c == '\'' || c == '"' || c == 'a' || c == 'b' || c == 'f' ||
			c == 'n' || c == 'r' || c == 't' || c == 'v' || c == 'x':
		case c >= '0' && c <= '7':
			j := i
			for j < len(body) && j < i+3 && body[j] >= '0' && body[j] <= '7' {
				j++
			}
			if v, _ := strconv.ParseUint(body[i:j], 8, 32); v > 0o377 {
				return "an invalid octal escape"
			}
			i = j - 1
		case (c == 'N' || c == 'u' || c == 'U') && !isBytes:
		case c >= 0x80 && !isBytes:
			// A backslash before a non-ASCII character is kept, with no
			// warning.
		default:
			return "an invalid escape sequence"
		}
	}
	return ""
}

// number reads a numeric token as the tokenizer does (tok_get), with
// verify_end_of_number after it.
func (lx *litLexer) number() {
	start := lx.i
	c := lx.at(0)
	kind := "decimal"
	fail := func() { lx.err = true }
	decimalTail := func() bool {
		for {
			for isDigitB(lx.at(0)) {
				lx.i++
			}
			if lx.at(0) != '_' {
				return true
			}
			lx.i++
			if !isDigitB(lx.at(0)) {
				fail()
				return false
			}
		}
	}
	var exponent, fraction, imaginary func() bool
	imaginary = func() bool {
		lx.i++ // the j
		kind = "imaginary"
		return true
	}
	exponent = func() bool {
		e := lx.i
		lx.i++
		if c := lx.at(0); c == '+' || c == '-' {
			lx.i++
			if !isDigitB(lx.at(0)) {
				fail()
				return false
			}
		} else if !isDigitB(c) {
			// "1e" then no digit: the number ends before the e, and the e
			// is checked as what follows it.
			lx.i = e
			return true
		}
		if !decimalTail() {
			return false
		}
		if c := lx.at(0); c == 'j' || c == 'J' {
			return imaginary()
		}
		return true
	}
	fraction = func() bool {
		// At the first byte after the '.'.
		if isDigitB(lx.at(0)) {
			if !decimalTail() {
				return false
			}
		}
		switch lx.at(0) {
		case 'e', 'E':
			return exponent()
		case 'j', 'J':
			return imaginary()
		}
		return true
	}
	ok := true
	switch {
	case c == '.':
		lx.i++
		ok = fraction()
	case c == '0':
		lx.i++
		switch lx.at(0) {
		case 'x', 'X', 'o', 'O', 'b', 'B':
			base := lx.at(0) | 0x20
			kind = map[byte]string{'x': "hexadecimal", 'o': "octal", 'b': "binary"}[base]
			lx.i++
			digit := func(c byte) bool {
				switch base {
				case 'x':
					return isHexB(c)
				case 'o':
					return c >= '0' && c <= '7'
				}
				return c == '0' || c == '1'
			}
			for {
				if lx.at(0) == '_' {
					lx.i++
				}
				if !digit(lx.at(0)) {
					fail()
					return
				}
				for digit(lx.at(0)) {
					lx.i++
				}
				if lx.at(0) != '_' {
					break
				}
			}
			if base != 'x' && isDigitB(lx.at(0)) {
				fail() // "invalid digit ... in octal/binary literal"
				return
			}
		default:
			nonzero := false
			for {
				if lx.at(0) == '_' {
					lx.i++
					if !isDigitB(lx.at(0)) {
						fail()
						return
					}
				}
				if lx.at(0) != '0' {
					break
				}
				lx.i++
			}
			if isDigitB(lx.at(0)) {
				nonzero = true
				if !decimalTail() {
					return
				}
			}
			switch lx.at(0) {
			case '.':
				lx.i++
				ok = fraction()
			case 'e', 'E':
				ok = exponent()
			case 'j', 'J':
				ok = imaginary()
			default:
				if nonzero {
					fail() // leading zeros
					return
				}
			}
		}
	default:
		if !decimalTail() {
			return
		}
		switch lx.at(0) {
		case '.':
			lx.i++
			ok = fraction()
		case 'e', 'E':
			ok = exponent()
		case 'j', 'J':
			ok = imaginary()
		}
	}
	if !ok {
		return
	}
	lx.push(tkNum, lx.s[start:lx.i])
	lx.endOfNumber(kind)
}

// endOfNumber is verify_end_of_number: a keyword that can follow a
// number in valid code makes a SyntaxWarning, another ASCII name
// character a SyntaxError.
func (lx *litLexer) endOfNumber(kind string) {
	rest := lx.s[lx.i:]
	c := lx.at(0)
	warn := false
	switch c {
	case 'a':
		warn = strings.HasPrefix(rest, "and")
	case 'e':
		warn = strings.HasPrefix(rest, "else")
	case 'f':
		warn = strings.HasPrefix(rest, "for")
	case 'i':
		n := lx.at(1)
		warn = n == 'f' || n == 'n' || n == 's'
	case 'o':
		warn = strings.HasPrefix(rest, "or")
	case 'n':
		warn = strings.HasPrefix(rest, "not")
	}
	if warn {
		lx.warn = "an invalid " + kind + " literal"
		return
	}
	if c < 0x80 && isIdentChar(c) && lx.i < len(lx.s) {
		lx.err = true
	}
}

// --- syntax tree ---

type litKind int

const (
	nInt litKind = iota
	nFloat
	nComplex
	nStr
	nBytes
	nTrue
	nFalse
	nNone
	nEllipsis
	nList
	nTuple
	nSet
	nDict
	nUnary   // a sign on a number
	nCplxSum // a signed int or float plus or minus a complex constant
	nHugeInt // a hex, octal or binary int past 4300 decimal digits
)

type litNode struct {
	kind  litKind
	text  string // nInt: decimal digits; nStr: the value
	f     float64
	neg   bool // nUnary
	elems []*litNode
	vals  []*litNode // nDict: values (elems are keys)
	// nStr: a \N{...} escape this port does not resolve.
	named bool
}

type litParser struct {
	toks []litTok
	i    int
}

func (p *litParser) peek() litTok { return p.toks[p.i] }

func (p *litParser) isOp(s string) bool {
	t := p.toks[p.i]
	return t.kind == tkOp && t.text == s
}

// top is an eval-mode expression: one expression, then only newlines.
func (p *litParser) top() (*litNode, bool) {
	n, ok := p.expr()
	if !ok {
		return nil, false
	}
	for p.peek().kind == tkNewline {
		p.i++
	}
	if p.peek().kind != tkEnd {
		return nil, false
	}
	return n, true
}

// expr is a signed operand, or a sum of two that literal_eval reads as
// a complex number.
func (p *litParser) expr() (*litNode, bool) {
	left, ok := p.signed()
	if !ok {
		return nil, false
	}
	if !p.isOp("+") && !p.isOp("-") {
		return left, true
	}
	p.i++
	right, ok := p.signed()
	if !ok || p.isOp("+") || p.isOp("-") {
		return nil, false
	}
	// left must be a signed int or float, right an unsigned complex.
	base := left
	if base.kind == nUnary {
		base = base.elems[0]
	}
	if (base.kind != nInt && base.kind != nFloat && base.kind != nHugeInt) || right.kind != nComplex {
		return nil, false
	}
	return &litNode{kind: nCplxSum}, true
}

func (p *litParser) signed() (*litNode, bool) {
	if p.isOp("+") || p.isOp("-") {
		neg := p.peek().text == "-"
		p.i++
		x, ok := p.signedOperand()
		if !ok {
			return nil, false
		}
		if x.kind != nInt && x.kind != nFloat && x.kind != nComplex && x.kind != nHugeInt {
			return nil, false
		}
		return &litNode{kind: nUnary, neg: neg, elems: []*litNode{x}}, true
	}
	return p.atom()
}

// signedOperand is what follows a sign: an atom (a sign on a sign is
// not a literal).
func (p *litParser) signedOperand() (*litNode, bool) {
	if p.isOp("+") || p.isOp("-") {
		return nil, false
	}
	return p.atom()
}

func (p *litParser) atom() (*litNode, bool) {
	t := p.peek()
	switch t.kind {
	case tkNum:
		p.i++
		return numberNode(t.text)
	case tkStr:
		return p.strings()
	case tkName:
		p.i++
		switch t.text {
		case "True":
			return &litNode{kind: nTrue}, true
		case "False":
			return &litNode{kind: nFalse}, true
		case "None":
			return &litNode{kind: nNone}, true
		case "set":
			if p.isOp("(") && p.toks[p.i+1].kind == tkOp && p.toks[p.i+1].text == ")" {
				p.i += 2
				return &litNode{kind: nSet}, true
			}
		}
		return nil, false
	case tkOp:
		switch t.text {
		case "...":
			p.i++
			return &litNode{kind: nEllipsis}, true
		case "(":
			p.i++
			if p.isOp(")") {
				p.i++
				return &litNode{kind: nTuple}, true
			}
			first, ok := p.expr()
			if !ok {
				return nil, false
			}
			if p.isOp(")") {
				p.i++
				return first, true
			}
			xs, ok := p.seqAfter(first, ")")
			if !ok {
				return nil, false
			}
			return &litNode{kind: nTuple, elems: xs}, true
		case "[":
			p.i++
			if p.isOp("]") {
				p.i++
				return &litNode{kind: nList}, true
			}
			first, ok := p.expr()
			if !ok {
				return nil, false
			}
			if p.isOp("]") {
				p.i++
				return &litNode{kind: nList, elems: []*litNode{first}}, true
			}
			xs, ok := p.seqAfter(first, "]")
			if !ok {
				return nil, false
			}
			return &litNode{kind: nList, elems: xs}, true
		case "{":
			p.i++
			if p.isOp("}") {
				p.i++
				return &litNode{kind: nDict}, true
			}
			first, ok := p.expr()
			if !ok {
				return nil, false
			}
			if p.isOp(":") {
				return p.dictAfter(first)
			}
			if p.isOp("}") {
				p.i++
				return &litNode{kind: nSet, elems: []*litNode{first}}, true
			}
			xs, ok := p.seqAfter(first, "}")
			if !ok {
				return nil, false
			}
			return &litNode{kind: nSet, elems: xs}, true
		}
	}
	return nil, false
}

// seqAfter reads ", x, y [,] END" after the first element.
func (p *litParser) seqAfter(first *litNode, end string) ([]*litNode, bool) {
	xs := []*litNode{first}
	for {
		if !p.isOp(",") {
			return nil, false
		}
		p.i++
		if p.isOp(end) {
			p.i++
			return xs, true
		}
		x, ok := p.expr()
		if !ok {
			return nil, false
		}
		xs = append(xs, x)
		if p.isOp(end) {
			p.i++
			return xs, true
		}
	}
}

// dictAfter reads ": v, k: v [,] }" after the first key.
func (p *litParser) dictAfter(first *litNode) (*litNode, bool) {
	d := &litNode{kind: nDict}
	k := first
	for {
		if !p.isOp(":") {
			return nil, false
		}
		p.i++
		v, ok := p.expr()
		if !ok {
			return nil, false
		}
		d.elems = append(d.elems, k)
		d.vals = append(d.vals, v)
		if p.isOp("}") {
			p.i++
			return d, true
		}
		if !p.isOp(",") {
			return nil, false
		}
		p.i++
		if p.isOp("}") {
			p.i++
			return d, true
		}
		k, ok = p.expr()
		if !ok {
			return nil, false
		}
	}
}

// strings reads adjacent string tokens as one constant.
func (p *litParser) strings() (*litNode, bool) {
	var b []byte
	isBytes := strings.Contains(p.peek().prefix, "b")
	named := false
	for p.peek().kind == tkStr {
		t := p.peek()
		p.i++
		if strings.Contains(t.prefix, "b") != isBytes {
			return nil, false // "cannot mix bytes and nonbytes literals"
		}
		raw := strings.Contains(t.prefix, "r")
		if isBytes {
			for i := 0; i < len(t.text); i++ {
				if t.text[i] >= 0x80 {
					return nil, false // bytes can only contain ASCII
				}
			}
			if !raw && !bytesEscapesOK(t.text) {
				return nil, false
			}
			continue
		}
		if raw {
			b = append(b, t.text...)
			continue
		}
		s, nm, ok := decodeStrEscapes(t.text)
		if !ok {
			return nil, false
		}
		named = named || nm
		b = append(b, s...)
	}
	if isBytes {
		return &litNode{kind: nBytes}, true
	}
	return &litNode{kind: nStr, text: string(b), named: named}, true
}

// bytesEscapesOK is false where _PyBytes_DecodeEscape raises: a \x
// without two hex digits.
func bytesEscapesOK(body string) bool {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i < len(body) && body[i] == 'x' {
			if !isHexB(at(body, i+1)) || !isHexB(at(body, i+2)) {
				return false
			}
			i += 2
		}
	}
	return true
}

func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// decodeStrEscapes is the unicode-escape decoding of a str body. named
// is set for a \N{...} escape, whose character this port does not look
// up (the value then holds the escape's text). ok is false where Python
// raises.
func decodeStrEscapes(body string) (out []byte, named, ok bool) {
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			_, size := utf8.DecodeRuneInString(body[i:])
			out = append(out, body[i:i+size]...)
			i += size
			continue
		}
		i++
		if i >= len(body) {
			// A body cannot end in a lone backslash (the quote would be
			// escaped), but read it as Python keeps it.
			out = append(out, '\\')
			break
		}
		e := body[i]
		i++
		switch e {
		case '\n':
		case '\\', '\'', '"':
			out = append(out, e)
		case 'a':
			out = append(out, 7)
		case 'b':
			out = append(out, 8)
		case 'f':
			out = append(out, 12)
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, 11)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i - 1
			k := j
			for k < len(body) && k < j+3 && body[k] >= '0' && body[k] <= '7' {
				k++
			}
			r, _ := strconv.ParseUint(body[j:k], 8, 32)
			i = k
			out = pystr.AppendRune(out, rune(r))
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if i+n > len(body) {
				return nil, false, false
			}
			for _, h := range []byte(body[i : i+n]) {
				if !isHexB(h) {
					return nil, false, false
				}
			}
			r, _ := strconv.ParseUint(body[i:i+n], 16, 64)
			if r > 0x10FFFF {
				return nil, false, false
			}
			i += n
			out = pystr.AppendRune(out, rune(r))
		case 'N':
			// \N{name}: a malformed escape raises; a well-formed one
			// names a character this port does not look up.
			end := strings.IndexByte(body[i:], '}')
			if i >= len(body) || body[i] != '{' || end < 2 {
				return nil, false, false
			}
			named = true
			out = append(out, '\\', 'N')
		default:
			// Kept with its backslash (the warning was checked when the
			// token was read); a non-ASCII character keeps it with none.
			out = append(out, '\\')
			i--
		}
	}
	return out, named, true
}

func numberNode(text string) (*litNode, bool) {
	clean := strings.ReplaceAll(text, "_", "")
	last := clean[len(clean)-1] | 0x20
	if last == 'j' {
		return &litNode{kind: nComplex}, true
	}
	if len(clean) > 1 && clean[0] == '0' {
		switch clean[1] | 0x20 {
		case 'x', 'o', 'b':
			base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[clean[1]|0x20]
			n, ok := new(big.Int).SetString(clean[2:], base)
			if !ok {
				return nil, false
			}
			if t := n.String(); len(t) > MaxIntDigits {
				// Read, but str() of it raises: a value this port does
				// not hold.
				return &litNode{kind: nHugeInt}, true
			}
			return &litNode{kind: nInt, text: n.String()}, true
		}
	}
	if !strings.ContainsAny(clean, ".eE") {
		// Python 3.12 reads at most 4300 decimal digits into an int; past
		// that the literal is a SyntaxError (a run of zeros is 0).
		if len(clean) > MaxIntDigits && strings.Trim(clean, "0") != "" {
			return nil, false
		}
		n, ok := new(big.Int).SetString(clean, 10)
		if !ok {
			return nil, false
		}
		return &litNode{kind: nInt, text: n.String()}, true
	}
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		if ne, isNum := err.(*strconv.NumError); !isNum || ne.Err != strconv.ErrRange {
			return nil, false
		}
	}
	return &litNode{kind: nFloat, f: f}, true
}

// --- evaluation ---

type litEval struct {
	why   string
	tuple bool
}

func (ev *litEval) opaque(why string) Opaque {
	if ev.why == "" {
		ev.why = why
	}
	return Opaque{Why: why}
}

// hashable is false for a list, a dict or a set, and for a tuple that
// holds one: as a dict key or a set item it raises TypeError.
func hashable(n *litNode) bool {
	switch n.kind {
	case nList, nDict, nSet:
		return false
	case nTuple:
		for _, x := range n.elems {
			if !hashable(x) {
				return false
			}
		}
	}
	return true
}

func (ev *litEval) eval(n *litNode) (any, bool) {
	switch n.kind {
	case nInt:
		return Int{Text: n.text}, true
	case nFloat:
		return Float(n.f), true
	case nUnary:
		x := n.elems[0]
		switch x.kind {
		case nInt:
			if !n.neg || x.text == "0" {
				return Int{Text: x.text}, true
			}
			return Int{Text: "-" + x.text}, true
		case nFloat:
			if n.neg {
				return Float(-x.f), true
			}
			return Float(x.f), true
		}
		if x.kind == nHugeInt {
			return ev.opaque("an int past 4300 decimal digits"), true
		}
		return ev.opaque("a complex number"), true
	case nHugeInt:
		return ev.opaque("an int past 4300 decimal digits"), true
	case nComplex, nCplxSum:
		return ev.opaque("a complex number"), true
	case nStr:
		if n.named {
			return ev.opaque(`a \N{...} escape`), true
		}
		return n.text, true
	case nBytes:
		return ev.opaque("bytes"), true
	case nTrue:
		return true, true
	case nFalse:
		return false, true
	case nNone:
		return nil, true
	case nEllipsis:
		return ev.opaque("Ellipsis"), true
	case nList, nTuple:
		if n.kind == nTuple {
			ev.tuple = true
		}
		xs := make([]any, 0, len(n.elems))
		for _, e := range n.elems {
			v, ok := ev.eval(e)
			if !ok {
				return nil, false
			}
			xs = append(xs, v)
		}
		return xs, true
	case nSet:
		for _, e := range n.elems {
			if !hashable(e) {
				return nil, false
			}
			if _, ok := ev.eval(e); !ok {
				return nil, false
			}
		}
		return ev.opaque("a set"), true
	case nDict:
		o := NewObject()
		for i, k := range n.elems {
			if !hashable(k) {
				return nil, false
			}
			kv, ok := ev.eval(k)
			if !ok {
				return nil, false
			}
			v, ok := ev.eval(n.vals[i])
			if !ok {
				return nil, false
			}
			ks, isStr := kv.(string)
			if !isStr {
				ev.opaque("a dict key that is not a str")
				continue
			}
			o.Set(ks, v)
		}
		return o, true
	}
	return nil, false
}
