// Package pmodel validates JSON and Python values the way pydantic 2.13
// (pydantic-core 2.46) does for the oracle's models: the same lax
// coercions, the same errors in the same order, and the same
// ValidationError text, which the gate puts into a deny reason.
//
// model_validate_json reads its text with jiter, not json.loads; jiter.go
// is that reader, with its error messages and positions.
package pmodel

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// JSONError is a jiter parse error: its message and byte index.
type JSONError struct {
	Msg string
	At  int
}

// jiter reads a value nested in more than this many containers as an error.
const jiterMaxDepth = 200

// jiterMaxNumber is the longest number literal jiter reads.
const jiterMaxNumber = 4300

type jparser struct {
	s string
	i int
}

func (p *jparser) fail(msg string, at int) { panic(&JSONError{Msg: msg, At: at}) }

// ParseJSON is jiter's parse of text into Python values (pyjson types).
func ParseJSON(text string) (v any, err *JSONError) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*JSONError); ok {
				v, err = nil, e
				return
			}
			panic(r)
		}
	}()
	p := &jparser{s: text}
	p.ws()
	v = p.value(0)
	p.ws()
	if p.i < len(p.s) {
		p.fail("trailing characters", p.i)
	}
	return v, nil
}

func (p *jparser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) eof(what string) { p.fail("EOF while parsing "+what, len(p.s)-1) }

func (p *jparser) value(depth int) any {
	if p.i >= len(p.s) {
		p.eof("a value")
	}
	if depth > jiterMaxDepth {
		p.fail("recursion limit exceeded", p.i)
	}
	c := p.s[p.i]
	switch {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case c == 't':
		p.ident("true")
		return true
	case c == 'f':
		p.ident("false")
		return false
	case c == 'n':
		p.ident("null")
		return nil
	case c == 'N':
		p.ident("NaN")
		return pyjson.Float(math.NaN())
	case c == 'I':
		p.ident("Infinity")
		return pyjson.Float(math.Inf(1))
	case c == '-' && strings.HasPrefix(p.s[p.i:], "-I"):
		p.i++
		p.ident("Infinity")
		return pyjson.Float(math.Inf(-1))
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	p.fail("expected value", p.i)
	return nil
}

func (p *jparser) ident(word string) {
	for k := 0; k < len(word); k++ {
		if p.i >= len(p.s) {
			p.eof("a value")
		}
		if p.s[p.i] != word[k] {
			p.fail("expected ident", p.i)
		}
		p.i++
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (p *jparser) number() any {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i >= len(p.s) {
		p.eof("a value")
	}
	if !isDigit(p.s[p.i]) {
		p.fail("invalid number", p.i)
	}
	if p.s[p.i] == '0' {
		p.i++
		if p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.fail("invalid number", p.i)
		}
	} else {
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
	}
	isFloat := false
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if p.i >= len(p.s) {
			p.eof("a value")
		}
		if !isDigit(p.s[p.i]) {
			p.fail("invalid number", p.i)
		}
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
		isFloat = true
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if p.i >= len(p.s) {
			p.eof("a value")
		}
		if !isDigit(p.s[p.i]) {
			p.fail("invalid number", p.i)
		}
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
		isFloat = true
	}
	lit := p.s[start:p.i]
	if !isFloat {
		if len(lit) > jiterMaxNumber {
			p.fail("number out of range", start+jiterMaxNumber+1)
		}
		d := strings.TrimLeft(strings.TrimPrefix(lit, "-"), "0")
		if d == "" {
			return pyjson.Int{Text: "0"}
		}
		if strings.HasPrefix(lit, "-") {
			return pyjson.Int{Text: "-" + d}
		}
		return pyjson.Int{Text: d}
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			p.fail("invalid number", p.i)
		}
	}
	return pyjson.Float(f)
}

func hex4(s string) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[:4], 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

func (p *jparser) str() string {
	p.i++ // "
	// The common string has no escape and no control character: it is
	// its own text.
	for j := p.i; j < len(p.s); j++ {
		c := p.s[j]
		if c == '"' {
			out := p.s[p.i:j]
			p.i = j + 1
			return out
		}
		if c == '\\' || c < 0x20 {
			break
		}
	}
	var b []byte
	for {
		if p.i >= len(p.s) {
			p.eof("a string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return string(b)
		case c < 0x20:
			p.fail("control character (\\u0000-\\u001F) found while parsing a string", p.i)
		case c == '\\':
			p.i++
			if p.i >= len(p.s) {
				p.eof("a string")
			}
			e := p.s[p.i]
			switch e {
			case '"', '\\', '/':
				b = append(b, e)
			case 'b':
				b = append(b, '\b')
			case 'f':
				b = append(b, '\f')
			case 'n':
				b = append(b, '\n')
			case 'r':
				b = append(b, '\r')
			case 't':
				b = append(b, '\t')
			case 'u':
				r := p.hexEscape()
				if r >= 0xD800 && r <= 0xDBFF {
					// jiter wants \uXXXX with a low surrogate next: it
					// names the first character that is not that.
					if p.i+1 >= len(p.s) {
						p.eof("a string")
					}
					if p.s[p.i+1] != '\\' {
						p.fail("unexpected end of hex escape", p.i+1)
					}
					if p.i+2 >= len(p.s) {
						p.eof("a string")
					}
					if p.s[p.i+2] != 'u' {
						p.fail("unexpected end of hex escape", p.i+2)
					}
					p.i += 2
					r2 := p.hexEscape()
					if r2 < 0xDC00 || r2 > 0xDFFF {
						p.fail("lone leading surrogate in hex escape", p.i)
					}
					b = pystr.AppendRune(b, utf16.DecodeRune(r, r2))
					break
				}
				if r >= 0xDC00 && r <= 0xDFFF {
					p.fail("lone leading surrogate in hex escape", p.i)
				}
				b = pystr.AppendRune(b, r)
			default:
				p.fail("invalid escape", p.i)
			}
			p.i++
		default:
			b = append(b, c)
			p.i++
		}
	}
}

// hexEscape reads the four hex digits after \u, leaving p.i on the last.
func (p *jparser) hexEscape() rune {
	var r rune
	if p.i+4 >= len(p.s) {
		// jiter wants four bytes after the u before it reads any.
		p.eof("a string")
	}
	for k := 0; k < 4; k++ {
		p.i++
		if p.i >= len(p.s) {
			p.eof("a string")
		}
		c := p.s[p.i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			p.fail("invalid escape", p.i)
		}
		r = r<<4 | rune(d)
	}
	return r
}

func (p *jparser) array(depth int) any {
	p.i++ // [
	out := []any{}
	p.ws()
	if p.i >= len(p.s) {
		p.eof("a list")
	}
	if p.s[p.i] == ']' {
		p.i++
		return out
	}
	for {
		p.ws()
		out = append(out, p.value(depth+1))
		p.ws()
		if p.i >= len(p.s) {
			p.eof("a list")
		}
		switch p.s[p.i] {
		case ']':
			p.i++
			return out
		case ',':
			p.i++
			p.ws()
			if p.i >= len(p.s) {
				p.eof("a value")
			}
			if p.s[p.i] == ']' {
				p.fail("trailing comma", p.i)
			}
		default:
			p.fail("expected `,` or `]`", p.i)
		}
	}
}

func (p *jparser) object(depth int) any {
	p.i++ // {
	o := pyjson.NewObjectCap(8)
	p.ws()
	if p.i >= len(p.s) {
		p.eof("an object")
	}
	if p.s[p.i] == '}' {
		p.i++
		return o
	}
	for {
		p.ws()
		if p.i >= len(p.s) {
			p.eof("an object")
		}
		if p.s[p.i] != '"' {
			p.fail("key must be a string", p.i)
		}
		k := p.str()
		p.ws()
		if p.i >= len(p.s) {
			p.eof("an object")
		}
		if p.s[p.i] != ':' {
			p.fail("expected `:`", p.i)
		}
		p.i++
		p.ws()
		o.Set(k, p.value(depth+1))
		p.ws()
		if p.i >= len(p.s) {
			p.eof("an object")
		}
		switch p.s[p.i] {
		case '}':
			p.i++
			return o
		case ',':
			p.i++
			p.ws()
			if p.i >= len(p.s) {
				p.eof("a value")
			}
			if p.s[p.i] == '}' {
				p.fail("trailing comma", p.i)
			}
		default:
			p.fail("expected `,` or `}`", p.i)
		}
	}
}

// Position is jiter's "line L column C" for a byte index: lines count
// newlines up to and including the index, and the column is the bytes
// after the last of them, through the index.
func Position(text string, at int) string {
	line, col := 1, 0
	for i := 0; i <= at && i < len(text); i++ {
		if text[i] == '\n' {
			line++
			col = 0
		} else {
			col++
		}
	}
	if at >= len(text) {
		col += at - len(text) + 1
	}
	return fmt.Sprintf("line %d column %d", line, col)
}
