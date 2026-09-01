// Package gateway is the token-saving gateway: the HTTP proxy between a
// harness and its model, the rules router in front of it, the meter and
// the turn journal behind it. It is opendaisugi.gateway, gateway_asgi,
// gateway_pipeline, gateway_journal, gateway_answers, gateway_openai,
// gateway_report and routing, with the same bytes on the wire and in the
// files.
//
// The request bodies it routes are any JSON a client sends, and the
// oracle reads them with Python's duck typing: a body whose shape makes a
// Python operation raise is forwarded untouched. The helpers in this file
// model those operations on decoded JSON values (pyjson's *Object, []any,
// string, Int, Float, bool and nil) and report a raise as errRaise.
package gateway

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// errRaise is a Python exception the oracle catches and does not print.
var errRaise = errors.New("python raised")

// maxJSONDepth is the nesting past which json.loads raises
// RecursionError in the oracle: CPython 3.12's C recursion limit, less the
// few levels the call itself takes. maxPyDepth is the same for recursion
// in Python code (_block_chars), under the default limit of 1000 less
// the server's own frames. Neither is matched to the last level; no case
// sits within a few levels of either (GW-5).
const (
	maxJSONDepth = 9990
	maxPyDepth   = 960
)

// get is v.get(key, def): v must be a dict.
func get(v any, key string, def any) (any, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errRaise
	}
	if x, has := o.Get(key); has {
		return x, nil
	}
	return def, nil
}

// getOr is get for a value already known to be a dict.
func getOr(o *pyjson.Object, key string, def any) any {
	if x, has := o.Get(key); has {
		return x
	}
	return def
}

// iterate is `for x in v`: a list's items, a dict's keys, a str's
// characters. Anything else is not iterable.
func iterate(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case *pyjson.Object:
		keys := x.Keys()
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	case string:
		rs := pystr.Runes(x)
		out := make([]any, len(rs))
		for i, r := range rs {
			out[i] = pystr.FromRunes([]rune{r})
		}
		return out, nil
	}
	return nil, errRaise
}

// reversedOf is `reversed(v)`: a list, a dict (its keys) and a str
// reverse; anything else raises TypeError.
func reversedOf(v any) ([]any, error) {
	items, err := iterate(v)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(items))
	for i, x := range items {
		out[len(items)-1-i] = x
	}
	return out, nil
}

// pyLen is len(v) for a str, list or dict; anything else raises.
func pyLen(v any) (int, error) {
	switch x := v.(type) {
	case string:
		return pystr.Len(x), nil
	case []any:
		return len(x), nil
	case *pyjson.Object:
		return x.Len(), nil
	}
	return 0, errRaise
}

// isStr reports whether v is a Python str.
func isStr(v any) bool {
	_, ok := v.(string)
	return ok
}

// eqStr is `v == s` for a str s: only an equal str is equal.
func eqStr(v any, s string) bool {
	x, ok := v.(string)
	return ok && x == s
}

// hashable reports whether v can be a dict key: a list or a dict cannot.
func hashable(v any) bool {
	switch v.(type) {
	case []any, *pyjson.Object:
		return false
	}
	return true
}

// pyEqual is Python's == on two decoded JSON values.
func pyEqual(a, b any) bool {
	na, aNum := numOf(a)
	nb, bNum := numOf(b)
	if aNum && bNum {
		return na.eq(nb)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case *pyjson.Object:
		y, ok := b.(*pyjson.Object)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.Keys() {
			yv, has := y.Get(k)
			if !has || !pyEqual(x.Value(k), yv) {
				return false
			}
		}
		return true
	}
	return false
}

// num is a Python int (exact) or float.
type num struct {
	isFloat bool
	i       *big.Int
	f       float64
}

func numOf(v any) (num, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return num{i: big.NewInt(1)}, true
		}
		return num{i: big.NewInt(0)}, true
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok {
			return num{}, false
		}
		return num{i: n}, true
	case pyjson.Float:
		return num{isFloat: true, f: float64(x)}, true
	case float64:
		return num{isFloat: true, f: x}, true
	}
	return num{}, false
}

func (a num) eq(b num) bool {
	if !a.isFloat && !b.isFloat {
		return a.i.Cmp(b.i) == 0
	}
	if a.isFloat && b.isFloat {
		return a.f == b.f
	}
	i, f := a.i, b.f
	if a.isFloat {
		i, f = b.i, a.f
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	bf := new(big.Float).SetInt(i)
	return bf.Cmp(big.NewFloat(f)) == 0
}

// intToFloat is float(n): correctly rounded, OverflowError past the
// largest double.
func intToFloat(n *big.Int) (float64, error) {
	if n.IsInt64() {
		v := n.Int64()
		if v > -(1<<53) && v < 1<<53 {
			return float64(v), nil
		}
	}
	f, _ := new(big.Float).SetInt(n).Float64()
	if math.IsInf(f, 0) {
		return 0, errRaise
	}
	return f, nil
}

// pyInt is int(v) on a decoded JSON value: an int is itself, a bool 0 or
// 1, a float truncated (NaN and infinities raise), a str read as int(s)
// reads it; anything else raises TypeError.
func pyInt(v any) (*big.Int, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok {
			return nil, errRaise
		}
		return n, nil
	case pyjson.Float:
		return floatToInt(float64(x))
	case float64:
		return floatToInt(x)
	case string:
		return intFromStr(x)
	}
	return nil, errRaise
}

func floatToInt(f float64) (*big.Int, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errRaise
	}
	n, _ := new(big.Float).SetFloat64(math.Trunc(f)).Int(nil)
	return n, nil
}

// intFromStr is int(s) in base 10: surrounding whitespace, one sign,
// decimal digits of any script, single underscores between digits.
func intFromStr(s string) (*big.Int, error) {
	s = pystr.Strip(s)
	rs := pystr.Runes(s)
	neg := false
	if len(rs) > 0 && (rs[0] == '+' || rs[0] == '-') {
		neg = rs[0] == '-'
		rs = rs[1:]
	}
	if len(rs) == 0 {
		return nil, errRaise
	}
	var digits strings.Builder
	prevUnderscore := true
	for _, r := range rs {
		if r == '_' {
			if prevUnderscore {
				return nil, errRaise
			}
			prevUnderscore = true
			continue
		}
		d := decimalValue(r)
		if d < 0 {
			return nil, errRaise
		}
		digits.WriteByte(byte('0' + d))
		prevUnderscore = false
	}
	if prevUnderscore {
		return nil, errRaise
	}
	if digits.Len() > 4300 {
		return nil, errRaise
	}
	n, _ := new(big.Int).SetString(digits.String(), 10)
	if neg {
		n.Neg(n)
	}
	return n, nil
}

// decimalValue is the decimal digit value of r (Unicode category Nd), or
// -1.
func decimalValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	if r < 0x80 || !unicode.Is(unicode.Nd, r) {
		return -1
	}
	// Every Nd block is ten consecutive code points starting at a zero.
	z := r
	for unicode.Is(unicode.Nd, z-1) && r-z < 9 {
		z--
	}
	return int(r - z)
}

// trueDiv is a / b for Python ints: the correctly rounded quotient.
func trueDiv(a, b *big.Int) float64 {
	f, _ := new(big.Rat).SetFrac(a, b).Float64()
	return f
}

// latin1 decodes header bytes as httpx does.
func latin1(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}

// LoadsBytes is json.loads(b) for bytes: the encoding detected from the
// first bytes (UTF-32, UTF-16, UTF-8 with or without a BOM), decoded with
// surrogatepass, then read as json.loads reads a str. A body that does
// not decode, or is not JSON, is an error; tooDeep is a RecursionError.
func LoadsBytes(b []byte) (v any, err error) {
	text, ok := decodeJSONBytes(b)
	if !ok {
		return nil, errRaise
	}
	v, derr := pyjson.LoadsPy(text, maxJSONDepth)
	if derr != nil {
		if derr.TooDeep {
			return nil, errTooDeep
		}
		return nil, errRaise
	}
	return v, nil
}

// errTooDeep is RecursionError from json.loads: the oracle does not catch
// it where it catches ValueError.
var errTooDeep = errors.New("RecursionError")

func decodeJSONBytes(b []byte) (string, bool) {
	has := func(p ...byte) bool { return len(b) >= len(p) && string(b[:len(p)]) == string(p) }
	switch {
	case has(0, 0, 0xFE, 0xFF), has(0xFF, 0xFE, 0, 0):
		return decodeUTF32(b, has(0xFF, 0xFE, 0, 0), true)
	case has(0xFE, 0xFF), has(0xFF, 0xFE):
		return decodeUTF16(b, has(0xFF, 0xFE), true)
	case has(0xEF, 0xBB, 0xBF):
		return decodeSurrogatePass(b[3:])
	}
	if len(b) >= 4 {
		if b[0] == 0 {
			if b[1] != 0 {
				return decodeUTF16(b, false, false)
			}
			return decodeUTF32(b, false, false)
		}
		if b[1] == 0 {
			if b[2] != 0 || b[3] != 0 {
				return decodeUTF16(b, true, false)
			}
			return decodeUTF32(b, true, false)
		}
	} else if len(b) == 2 {
		if b[0] == 0 {
			return decodeUTF16(b, false, false)
		}
		if b[1] == 0 {
			return decodeUTF16(b, true, false)
		}
	}
	return decodeSurrogatePass(b)
}

// decodeSurrogatePass is b.decode("utf-8", "surrogatepass"): UTF-8, with
// an encoded surrogate read as that code point.
func decodeSurrogatePass(b []byte) (string, bool) {
	for i := 0; i < len(b); {
		if len(b)-i >= 3 && b[i] == 0xED && b[i+1] >= 0xA0 && b[i+1] <= 0xBF && b[i+2] >= 0x80 && b[i+2] <= 0xBF {
			i += 3
			continue
		}
		r, n := utf8Decode(b[i:])
		if r < 0 {
			return "", false
		}
		i += n
	}
	return string(b), true
}

func utf8Decode(b []byte) (rune, int) {
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError && n <= 1 {
		return -1, 1
	}
	return r, n
}

// decodeUTF16 is the utf-16 codec with surrogatepass: a lone surrogate
// is kept; an odd byte count fails.
func decodeUTF16(b []byte, le, bom bool) (string, bool) {
	if bom {
		b = b[2:]
	}
	if len(b)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		if le {
			units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
	}
	var out []byte
	for i := 0; i < len(units); i++ {
		u := rune(units[i])
		if utf16.IsSurrogate(u) && u < 0xDC00 && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] <= 0xDFFF {
			out = pystr.AppendRune(out, utf16.DecodeRune(u, rune(units[i+1])))
			i++
			continue
		}
		out = pystr.AppendRune(out, u)
	}
	return string(out), true
}

func decodeUTF32(b []byte, le, bom bool) (string, bool) {
	if bom {
		b = b[4:]
	}
	if len(b)%4 != 0 {
		return "", false
	}
	var out []byte
	for i := 0; i < len(b); i += 4 {
		var u uint32
		if le {
			u = uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24
		} else {
			u = uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
		}
		if u > 0x10FFFF {
			return "", false
		}
		out = pystr.AppendRune(out, rune(u))
	}
	return string(out), true
}
