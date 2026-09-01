package pyjson

import (
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pystr"
)

// LiteralEval is ast.literal_eval for the literals a model writes when it
// answers with a Python dict instead of JSON: dicts, lists and tuples
// (read as lists), str in single or double quotes with the common
// escapes, decimal ints and floats with an optional sign, True, False
// and None. ok is false for any other text, including literal forms this
// subset does not read (triple quotes, prefixes, bytes, sets, complex
// numbers, implicit concatenation, hex ints).
func LiteralEval(text string) (v any, ok bool) {
	p := &litParser{s: text}
	defer func() {
		if r := recover(); r != nil {
			if _, isFail := r.(litFail); isFail {
				v, ok = nil, false
				return
			}
			panic(r)
		}
	}()
	p.ws()
	v = p.value(0)
	p.ws()
	if p.i != len(p.s) {
		return nil, false
	}
	return v, true
}

type litFail struct{}

type litParser struct {
	s string
	i int
}

func (p *litParser) fail() { panic(litFail{}) }

// ws skips what the tokenizer skips inside brackets: spaces, tabs, form
// feeds, newlines and comments. A value at the top level is parsed in
// eval mode, where the text is stripped of leading spaces and tabs.
func (p *litParser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r', '\f':
			p.i++
		case '#':
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		case '\\':
			if strings.HasPrefix(p.s[p.i:], "\\\n") {
				p.i += 2
				continue
			}
			return
		default:
			return
		}
	}
}

func (p *litParser) peek() byte {
	if p.i >= len(p.s) {
		p.fail()
	}
	return p.s[p.i]
}

func (p *litParser) value(depth int) any {
	if depth > 200 {
		p.fail()
	}
	p.ws()
	c := p.peek()
	switch {
	case c == '{':
		p.i++
		o := NewObject()
		for {
			p.ws()
			if p.peek() == '}' {
				p.i++
				return o
			}
			k := p.value(depth + 1)
			ks, isStr := k.(string)
			if !isStr {
				// Only str keys reach a model; a dict with other keys is
				// read by literal_eval but is not a step or a reply.
				p.fail()
			}
			p.ws()
			if p.peek() != ':' {
				p.fail()
			}
			p.i++
			o.Set(ks, p.value(depth+1))
			p.ws()
			if p.peek() == ',' {
				p.i++
				continue
			}
			if p.peek() != '}' {
				p.fail()
			}
		}
	case c == '[' || c == '(':
		end := byte(']')
		if c == '(' {
			end = ')'
		}
		p.i++
		xs := []any{}
		n := 0
		trailing := false
		for {
			p.ws()
			if p.peek() == end {
				p.i++
				// (x) without a comma is x itself, not a tuple.
				if c == '(' && n == 1 && !trailing {
					return xs[0]
				}
				if c == '(' && n == 0 {
					return xs
				}
				return xs
			}
			xs = append(xs, p.value(depth+1))
			n++
			trailing = false
			p.ws()
			if p.peek() == ',' {
				p.i++
				trailing = true
				continue
			}
			if p.peek() != end {
				p.fail()
			}
		}
	case c == '\'' || c == '"':
		return p.str()
	case c == '-' || c == '+':
		p.i++
		p.ws()
		x := p.number()
		if c == '+' {
			return x
		}
		switch n := x.(type) {
		case Int:
			if strings.HasPrefix(n.Text, "-") {
				return Int{Text: n.Text[1:]}
			}
			if n.Text == "0" {
				return n
			}
			return Int{Text: "-" + n.Text}
		case Float:
			return -n
		}
	case c >= '0' && c <= '9' || c == '.':
		return p.number()
	}
	for _, w := range []struct {
		word string
		v    any
	}{{"True", true}, {"False", false}, {"None", nil}} {
		if strings.HasPrefix(p.s[p.i:], w.word) {
			after := p.i + len(w.word)
			if after < len(p.s) && isIdent(p.s[after]) {
				p.fail()
			}
			p.i = after
			return w.v
		}
	}
	p.fail()
	return nil
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// number reads a decimal int or float literal (underscores between digits).
func (p *litParser) number() any {
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '_' || p.s[p.i] == '.' ||
		p.s[p.i] == 'e' || p.s[p.i] == 'E' ||
		(p.s[p.i] == '+' || p.s[p.i] == '-') && p.i > start && (p.s[p.i-1] == 'e' || p.s[p.i-1] == 'E')) {
		p.i++
	}
	if p.i < len(p.s) && isIdent(p.s[p.i]) {
		p.fail()
	}
	text := p.s[start:p.i]
	if text == "" || strings.Contains(text, "__") || strings.HasPrefix(text, "_") || strings.HasSuffix(text, "_") ||
		strings.Contains(text, "_.") || strings.Contains(text, "._") {
		p.fail()
	}
	clean := strings.ReplaceAll(text, "_", "")
	if !strings.ContainsAny(clean, ".eE") {
		// A decimal int: no leading zero unless the value is zero.
		if len(clean) > 1 && clean[0] == '0' && strings.Trim(clean, "0") != "" {
			p.fail()
		}
		n, ok := new(big.Int).SetString(clean, 10)
		if !ok {
			p.fail()
		}
		return Int{Text: n.String()}
	}
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		if ne, isNum := err.(*strconv.NumError); !isNum || ne.Err != strconv.ErrRange {
			p.fail()
		}
	}
	return Float(f)
}

// str reads a one-line quoted string with Python's escapes.
func (p *litParser) str() string {
	q := p.s[p.i]
	if strings.HasPrefix(p.s[p.i:], strings.Repeat(string(q), 3)) {
		p.fail()
	}
	p.i++
	var b []byte
	for {
		if p.i >= len(p.s) {
			p.fail()
		}
		c := p.s[p.i]
		switch {
		case c == q:
			p.i++
			return string(b)
		case c == '\n':
			p.fail()
		case c == '\\':
			p.i++
			if p.i >= len(p.s) {
				p.fail()
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case '\n':
			case '\\', '\'', '"':
				b = append(b, e)
			case 'n':
				b = append(b, '\n')
			case 't':
				b = append(b, '\t')
			case 'r':
				b = append(b, '\r')
			case 'a':
				b = append(b, 7)
			case 'b':
				b = append(b, 8)
			case 'f':
				b = append(b, 12)
			case 'v':
				b = append(b, 11)
			case 'x', 'u', 'U':
				n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
				if p.i+n > len(p.s) {
					p.fail()
				}
				r, err := strconv.ParseUint(p.s[p.i:p.i+n], 16, 32)
				if err != nil || r > 0x10FFFF {
					p.fail()
				}
				p.i += n
				b = pystr.AppendRune(b, rune(r))
			case '0', '1', '2', '3', '4', '5', '6', '7':
				j := p.i - 1
				k := j
				for k < len(p.s) && k < j+3 && p.s[k] >= '0' && p.s[k] <= '7' {
					k++
				}
				r, _ := strconv.ParseUint(p.s[j:k], 8, 32)
				p.i = k
				b = pystr.AppendRune(b, rune(r))
			default:
				// An unknown escape keeps the backslash (with a warning).
				b = append(b, '\\')
				p.i--
			}
		default:
			_, size := utf8.DecodeRuneInString(p.s[p.i:])
			b = append(b, p.s[p.i:p.i+size]...)
			p.i += size
		}
	}
}
