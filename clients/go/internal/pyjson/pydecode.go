package pyjson

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"daisugi-verify/internal/pystr"
)

// DecodeError is json.JSONDecodeError: its message is
// "<msg>: line L column C (char P)", P counted in code points.
type DecodeError struct {
	Msg      string
	Pos      int // code point index
	Line     int
	Col      int
	NotJSON  bool   // a ValueError other than JSONDecodeError (an int past the digit limit)
	TooDeep  bool   // RecursionError from nesting
	RawError string // the message when NotJSON
}

func (e *DecodeError) Error() string {
	if e.NotJSON {
		return e.RawError
	}
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.Msg, e.Line, e.Col, e.Pos)
}

// pyDecoder is CPython 3.12's json.loads with the C scanner (_json.c):
// the same values as Loads, and on a bad document the JSONDecodeError
// the C scanner raises, at the same position.
type pyDecoder struct {
	s     []rune
	depth int
	max   int
}

type stopIteration struct{ idx int }

func (d *pyDecoder) fail(msg string, pos int) {
	panic(d.errAt(msg, pos))
}

func (d *pyDecoder) errAt(msg string, pos int) *DecodeError {
	line := 1
	last := -1
	for i := 0; i < pos && i < len(d.s); i++ {
		if d.s[i] == '\n' {
			line++
			last = i
		}
	}
	return &DecodeError{Msg: msg, Pos: pos, Line: line, Col: pos - last}
}

// LoadsPy is json.loads(text) with Python's own error text. maxDepth is
// the nesting past which the caller's stack would raise RecursionError
// (reported as TooDeep).
func LoadsPy(text string, maxDepth int) (v any, err *DecodeError) {
	d := &pyDecoder{s: pystr.Runes(text), max: maxDepth}
	defer func() {
		if r := recover(); r != nil {
			switch x := r.(type) {
			case *DecodeError:
				v, err = nil, x
			case stopIteration:
				v, err = nil, d.errAt("Expecting value", x.idx)
			default:
				panic(r)
			}
		}
	}()
	if len(d.s) > 0 && d.s[0] == 0xFEFF {
		d.fail("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)
	}
	idx := d.ws(0)
	v, end := d.scan(idx)
	end = d.ws(end)
	if end != len(d.s) {
		d.fail("Extra data", end)
	}
	return v, nil
}

func (d *pyDecoder) ws(i int) int {
	for i < len(d.s) {
		switch d.s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func (d *pyDecoder) has(i int, lit string) bool {
	rs := []rune(lit)
	if i+len(rs) > len(d.s) {
		return false
	}
	for k, r := range rs {
		if d.s[i+k] != r {
			return false
		}
	}
	return true
}

// scan is scan_once_unicode: the value at idx and the index after it.
func (d *pyDecoder) scan(idx int) (any, int) {
	if idx < 0 || idx >= len(d.s) {
		panic(stopIteration{idx})
	}
	switch c := d.s[idx]; {
	case c == '"':
		return d.str(idx + 1)
	case c == '{':
		d.enter("object")
		defer func() { d.depth-- }()
		return d.object(idx + 1)
	case c == '[':
		d.enter("array")
		defer func() { d.depth-- }()
		return d.array(idx + 1)
	case c == 'n' && d.has(idx, "null"):
		return nil, idx + 4
	case c == 't' && d.has(idx, "true"):
		return true, idx + 4
	case c == 'f' && d.has(idx, "false"):
		return false, idx + 5
	case c == 'N' && d.has(idx, "NaN"):
		return Float(math.NaN()), idx + 3
	case c == 'I' && d.has(idx, "Infinity"):
		return Float(math.Inf(1)), idx + 8
	case c == '-' && d.has(idx, "-Infinity"):
		return Float(math.Inf(-1)), idx + 9
	}
	return d.number(idx)
}

func (d *pyDecoder) enter(kind string) {
	d.depth++
	if d.max > 0 && d.depth > d.max {
		panic(&DecodeError{TooDeep: true, NotJSON: true,
			RawError: "maximum recursion depth exceeded while decoding a JSON " + kind + " from a unicode string"})
	}
}

func isD(r rune) bool { return r >= '0' && r <= '9' }

// number is _match_number_unicode.
func (d *pyDecoder) number(start int) (any, int) {
	idx := start
	end := len(d.s) - 1
	if d.s[idx] == '-' {
		idx++
		if idx > end {
			panic(stopIteration{start})
		}
	}
	switch {
	case d.s[idx] >= '1' && d.s[idx] <= '9':
		idx++
		for idx <= end && isD(d.s[idx]) {
			idx++
		}
	case d.s[idx] == '0':
		idx++
	default:
		panic(stopIteration{start})
	}
	isFloat := false
	if idx < end && d.s[idx] == '.' && isD(d.s[idx+1]) {
		isFloat = true
		idx += 2
		for idx <= end && isD(d.s[idx]) {
			idx++
		}
	}
	if idx < end && (d.s[idx] == 'e' || d.s[idx] == 'E') {
		e := idx
		idx++
		if idx < end && (d.s[idx] == '-' || d.s[idx] == '+') {
			idx++
		}
		for idx <= end && isD(d.s[idx]) {
			idx++
		}
		if isD(d.s[idx-1]) {
			isFloat = true
		} else {
			idx = e
		}
	}
	text := string(d.s[start:idx])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !strings.Contains(err.Error(), "range") {
			panic(stopIteration{start})
		}
		return Float(f), idx
	}
	digits := strings.TrimPrefix(text, "-")
	if len(digits) > MaxIntDigits {
		panic(&DecodeError{NotJSON: true, RawError: fmt.Sprintf(
			"Exceeds the limit (%d digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit",
			MaxIntDigits, len(digits))})
	}
	return normInt(text), idx
}

// str is scanstring_unicode (strict), from just after the opening quote.
func (d *pyDecoder) str(end int) (string, int) {
	begin := end - 1
	var b []rune
	for {
		if end >= len(d.s) {
			d.fail("Unterminated string starting at", begin)
		}
		c := d.s[end]
		if c == '"' {
			return string(runesToWTF8(b)), end + 1
		}
		if c < 0x20 {
			d.fail("Invalid control character at", end)
		}
		if c != '\\' {
			b = append(b, c)
			end++
			continue
		}
		end++
		if end >= len(d.s) {
			d.fail("Unterminated string starting at", begin)
		}
		c = d.s[end]
		if c != 'u' {
			rep, ok := map[rune]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[c]
			if !ok {
				d.fail("Invalid \\escape", end-1)
			}
			b = append(b, rep)
			end++
			continue
		}
		end++
		// The C scanner wants a character after the four digits.
		if end+4 >= len(d.s) {
			d.fail("Invalid \\uXXXX escape", end-1)
		}
		u := d.hex4(end)
		end += 4
		if u >= 0xD800 && u <= 0xDBFF && end+6 < len(d.s) && d.s[end] == '\\' && d.s[end+1] == 'u' {
			u2 := d.hex4(end + 2)
			if u2 >= 0xDC00 && u2 <= 0xDFFF {
				u = 0x10000 + ((u - 0xD800) << 10) + (u2 - 0xDC00)
				end += 6
			}
		}
		b = append(b, u)
	}
}

func (d *pyDecoder) hex4(at int) rune {
	var v rune
	for k := 0; k < 4; k++ {
		c := d.s[at+k]
		switch {
		case c >= '0' && c <= '9':
			v = v*16 + c - '0'
		case c >= 'a' && c <= 'f':
			v = v*16 + c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = v*16 + c - 'A' + 10
		default:
			d.fail("Invalid \\uXXXX escape", at-1)
		}
	}
	return v
}

func runesToWTF8(rs []rune) []byte {
	var b []byte
	for _, r := range rs {
		b = pystr.AppendRune(b, r)
	}
	return b
}

// object is _parse_object_unicode, from just after the '{'.
func (d *pyDecoder) object(idx int) (any, int) {
	o := NewObject()
	idx = d.ws(idx)
	if idx < len(d.s) && d.s[idx] == '}' {
		return o, idx + 1
	}
	for {
		if idx >= len(d.s) || d.s[idx] != '"' {
			d.fail("Expecting property name enclosed in double quotes", idx)
		}
		key, next := d.str(idx + 1)
		idx = d.ws(next)
		if idx >= len(d.s) || d.s[idx] != ':' {
			d.fail("Expecting ':' delimiter", idx)
		}
		idx = d.ws(idx + 1)
		val, next2 := d.scan(idx)
		o.Set(key, val)
		idx = d.ws(next2)
		if idx < len(d.s) && d.s[idx] == '}' {
			return o, idx + 1
		}
		if idx >= len(d.s) || d.s[idx] != ',' {
			d.fail("Expecting ',' delimiter", idx)
		}
		idx = d.ws(idx + 1)
	}
}

// array is _parse_array_unicode, from just after the '['.
func (d *pyDecoder) array(idx int) (any, int) {
	out := []any{}
	idx = d.ws(idx)
	if idx < len(d.s) && d.s[idx] == ']' {
		return out, idx + 1
	}
	for {
		val, next := d.scan(idx)
		out = append(out, val)
		idx = d.ws(next)
		if idx < len(d.s) && d.s[idx] == ']' {
			return out, idx + 1
		}
		if idx >= len(d.s) || d.s[idx] != ',' {
			d.fail("Expecting ',' delimiter", idx)
		}
		idx = d.ws(idx + 1)
	}
}
