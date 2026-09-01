// Package pyjson reads and writes JSON the way Python's json module does,
// so a Go program can emit byte-for-byte the same lines a Python program
// writes and read input the way json.loads reads it.
//
// Differences from encoding/json that matter here:
//   - objects keep their key order; a repeated key keeps its first
//     position and takes its last value, as dict(pairs) does
//   - integers keep their exact digits; floats print as Python's repr
//   - NaN, Infinity and -Infinity are read and written as Python does
//   - Dumps uses Python's default separators (", " and ": ") and can
//     escape every non-ASCII character (ensure_ascii)
//   - '<', '>' and '&' are never escaped
package pyjson

import (
	"daisugi-verify/internal/pystr"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Int is a JSON integer, kept as its normalized decimal text so that
// integers of any size round-trip exactly.
type Int struct{ Text string }

// Float is a JSON number with a fraction or exponent, or NaN/Infinity.
type Float float64

// Object is a JSON object with its key order. Values sit beside their
// keys; a map index is built only for an object with many keys, since
// most objects hold a handful and a map costs more than a scan.
type Object struct {
	keys []string
	vals []any
	idx  map[string]int
}

// objectIndexAt is the key count past which an Object keeps a map index.
const objectIndexAt = 32

// NewObject returns an empty object.
func NewObject() *Object { return &Object{} }

// NewObjectCap returns an empty object with room for n keys.
func NewObjectCap(n int) *Object {
	return &Object{keys: make([]string, 0, n), vals: make([]any, 0, n)}
}

func (o *Object) find(k string) int {
	if o.idx != nil {
		if i, ok := o.idx[k]; ok {
			return i
		}
		return -1
	}
	for i, x := range o.keys {
		if x == k {
			return i
		}
	}
	return -1
}

// Set adds a key at the end, or replaces the value of an existing key in
// place.
func (o *Object) Set(k string, v any) *Object {
	if i := o.find(k); i >= 0 {
		o.vals[i] = v
		return o
	}
	o.keys = append(o.keys, k)
	o.vals = append(o.vals, v)
	if o.idx != nil {
		o.idx[k] = len(o.keys) - 1
	} else if len(o.keys) > objectIndexAt {
		o.reindex()
	}
	return o
}

func (o *Object) reindex() {
	o.idx = make(map[string]int, len(o.keys))
	for i, x := range o.keys {
		o.idx[x] = i
	}
}

// Get returns the value of k and whether it is present.
func (o *Object) Get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	if i := o.find(k); i >= 0 {
		return o.vals[i], true
	}
	return nil, false
}

// at is the value of the i-th key.
func (o *Object) at(i int) any { return o.vals[i] }

// Value returns the value of k, or nil when it is absent (dict.get).
func (o *Object) Value(k string) any {
	v, _ := o.Get(k)
	return v
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Truthy reports Python truthiness of a decoded value.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case Int:
		return strings.TrimLeft(strings.TrimPrefix(x.Text, "-"), "0") != ""
	case Float:
		return float64(x) != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *Object:
		return x.Len() > 0
	case int:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// ErrUnsupported marks input Python would read but this package declines
// to model. LoadsStrict uses it for what pydantic's reader does not share
// with json.loads.
var ErrUnsupported = errors.New("pyjson: input outside the modeled subset")

// MaxDepth is how deeply nested a payload json.loads reads in the gate
// before its C scanner passes the recursion limit (measured: 9,994
// containers, the outermost one included).
const MaxDepth = 9994

// MaxIntDigits is sys.get_int_max_str_digits(): json.loads raises on an
// integer literal with more digits.
const MaxIntDigits = 4300

// Loads parses text the way json.loads(text) does. text must be valid
// UTF-8. The error is ErrUnsupported when the input is outside the modeled
// subset, and a plain error when Python would raise too.
func Loads(text string) (any, error) {
	return load(text, false)
}

// LoadsStrict is Loads that also reports a repeated key or a NaN or
// Infinity literal as ErrUnsupported, for input that Python reads with
// another JSON reader (pydantic's).
func LoadsStrict(text string) (any, error) {
	return load(text, true)
}

func load(text string, strict bool) (any, error) {
	if strings.HasPrefix(text, "\ufeff") {
		return nil, errors.New("unexpected UTF-8 BOM")
	}
	p := &parser{s: text, strict: strict}
	p.ws()
	v, err := p.value(1)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, errors.New("extra data")
	}
	return v, nil
}

type parser struct {
	s      string
	i      int
	strict bool
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

var errSyntax = errors.New("expecting value")

func (p *parser) value(depth int) (any, error) {
	if p.i >= len(p.s) {
		return nil, errSyntax
	}
	c := p.s[p.i]
	switch {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		p.i++
		return p.str()
	case strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return nil, nil
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false, nil
	case p.strict && (c == 'N' || c == 'I' || strings.HasPrefix(p.s[p.i:], "-I")):
		return nil, ErrUnsupported
	case strings.HasPrefix(p.s[p.i:], "NaN"):
		p.i += 3
		return Float(math.NaN()), nil
	case strings.HasPrefix(p.s[p.i:], "Infinity"):
		p.i += 8
		return Float(math.Inf(1)), nil
	case strings.HasPrefix(p.s[p.i:], "-Infinity"):
		p.i += 9
		return Float(math.Inf(-1)), nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, errSyntax
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// number follows Python's NUMBER_RE: (-?(?:0|[1-9]\d*))(\.\d+)?([eE][-+]?\d+)?
func (p *parser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i >= len(p.s) || !isDigit(p.s[p.i]) {
		return nil, errSyntax
	}
	if p.s[p.i] == '0' {
		p.i++
	} else {
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
	}
	intEnd := p.i
	isFloat := false
	if p.i+1 < len(p.s) && p.s[p.i] == '.' && isDigit(p.s[p.i+1]) {
		p.i += 2
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
		isFloat = true
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		j := p.i + 1
		if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
			j++
		}
		if j < len(p.s) && isDigit(p.s[j]) {
			for j < len(p.s) && isDigit(p.s[j]) {
				j++
			}
			p.i = j
			isFloat = true
		}
	}
	if !isFloat {
		digits := strings.TrimPrefix(p.s[start:intEnd], "-")
		if len(digits) > MaxIntDigits {
			return nil, fmt.Errorf("Exceeds the limit (%d digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit", MaxIntDigits, len(digits))
		}
		return normInt(p.s[start:intEnd]), nil
	}
	f, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		// Out of range: Python gives inf, strconv gives an error with inf.
		var ne *strconv.NumError
		if errors.As(err, &ne) && ne.Err == strconv.ErrRange {
			return Float(f), nil
		}
		return nil, errSyntax
	}
	return Float(f), nil
}

func normInt(t string) Int {
	neg := strings.HasPrefix(t, "-")
	d := strings.TrimLeft(strings.TrimPrefix(t, "-"), "0")
	if d == "" {
		return Int{Text: "0"}
	}
	if neg {
		return Int{Text: "-" + d}
	}
	return Int{Text: d}
}

func hexVal(s string) (rune, bool) {
	if len(s) != 4 {
		return 0, false
	}
	var r rune
	for i := 0; i < 4; i++ {
		c := s[i]
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

func (p *parser) str() (string, error) {
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", errors.New("unterminated string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c == '\\':
			if p.i+1 >= len(p.s) {
				return "", errors.New("unterminated string")
			}
			e := p.s[p.i+1]
			p.i += 2
			switch e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if p.i+4 > len(p.s) {
					return "", errors.New("invalid \\uXXXX escape")
				}
				r, ok := hexVal(p.s[p.i : p.i+4])
				if !ok {
					return "", errors.New("invalid \\uXXXX escape")
				}
				p.i += 4
				if utf16.IsSurrogate(r) {
					if r < 0xdc00 && p.i+6 <= len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
						r2, ok2 := hexVal(p.s[p.i+2 : p.i+6])
						if ok2 && r2 >= 0xdc00 && r2 <= 0xdfff {
							p.i += 6
							b.WriteRune(utf16.DecodeRune(r, r2))
							continue
						}
					}
					// A lone surrogate is a valid Python str; it is kept in
					// its generalized UTF-8 form (see internal/pystr).
					b.Write(pystr.AppendRune(nil, r))
					continue
				}
				b.WriteRune(r)
			default:
				return "", errors.New("invalid escape")
			}
		case c < 0x20:
			return "", errors.New("invalid control character")
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}

// errDeep is the RecursionError json's C scanner raises past the limit.
var errDeep = errors.New("maximum recursion depth exceeded while decoding a JSON array from a unicode string")

func (p *parser) array(depth int) (any, error) {
	if depth > MaxDepth {
		return nil, errDeep
	}
	p.i++ // [
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errSyntax
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, nil
		default:
			return nil, errSyntax
		}
	}
}

func (p *parser) object(depth int) (any, error) {
	if depth > MaxDepth {
		return nil, errDeep
	}
	p.i++ // {
	o := NewObject()
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return o, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, errSyntax
		}
		p.i++
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, errSyntax
		}
		p.i++
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		if o.find(k) >= 0 && p.strict {
			return nil, ErrUnsupported
		}
		o.Set(k, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errSyntax
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return o, nil
		default:
			return nil, errSyntax
		}
	}
}
