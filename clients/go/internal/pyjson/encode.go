package pyjson

import (
	"daisugi-verify/internal/pystr"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Dumps writes v as json.dumps(v) would, with the default separators.
// asciiOnly is ensure_ascii. Supported values: nil, bool, Int, Float,
// float64, int, string, []any, []string, *Object, and []*Object.
func Dumps(v any, asciiOnly bool) string {
	var b strings.Builder
	encode(&b, v, asciiOnly)
	return b.String()
}

func encode(b *strings.Builder, v any, ascii bool) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case Int:
		b.WriteString(x.Text)
	case Raw:
		b.WriteString(string(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case Float:
		b.WriteString(FloatRepr(float64(x)))
	case float64:
		b.WriteString(FloatRepr(x))
	case string:
		writeString(b, x, ascii)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, s, ascii)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			encode(b, e, ascii)
		}
		b.WriteByte(']')
	case []*Object:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			encode(b, e, ascii)
		}
		b.WriteByte(']')
	case *Object:
		b.WriteByte('{')
		for i, k := range x.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, k, ascii)
			b.WriteString(": ")
			encode(b, x.at(i), ascii)
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("pyjson: cannot encode %T", v))
	}
}

// Raw is a JSON token written as it is, for a number another serializer
// formats its own way (pydantic writes 1e-7 where Python writes 1e-07).
type Raw string

// DumpsIndent writes v as json.dumps(v, indent=indent) would: one item
// per line, "," between items and ": " after keys, and "[]" and "{}" for
// empty containers. asciiOnly is ensure_ascii. pydantic's
// model_dump_json(indent=...) has the same shape with asciiOnly false.
func DumpsIndent(v any, indent int, asciiOnly bool) string {
	var b strings.Builder
	encodeIndent(&b, v, asciiOnly, strings.Repeat(" ", indent), 0)
	return b.String()
}

func encodeIndent(b *strings.Builder, v any, ascii bool, unit string, level int) {
	newline := func(l int) {
		b.WriteByte('\n')
		for i := 0; i < l; i++ {
			b.WriteString(unit)
		}
	}
	var items []any
	switch x := v.(type) {
	case []any:
		items = x
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	case []*Object:
		for _, o := range x {
			items = append(items, o)
		}
	case *Object:
		if x.Len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range x.Keys() {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(level + 1)
			writeString(b, k, ascii)
			b.WriteString(": ")
			encodeIndent(b, x.at(i), ascii, unit, level+1)
		}
		newline(level)
		b.WriteByte('}')
		return
	default:
		encode(b, v, ascii)
		return
	}
	if len(items) == 0 {
		b.WriteString("[]")
		return
	}
	b.WriteByte('[')
	for i, e := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		newline(level + 1)
		encodeIndent(b, e, ascii, unit, level+1)
	}
	newline(level)
	b.WriteByte(']')
}

// Delete removes k, keeping the order of the other keys (del d[k]).
func (o *Object) Delete(k string) {
	i := o.find(k)
	if i < 0 {
		return
	}
	o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
	o.vals = append(o.vals[:i:i], o.vals[i+1:]...)
	if o.idx != nil {
		o.reindex()
	}
}

const hexDigits = "0123456789abcdef"

func writeU(b *strings.Builder, r rune) {
	b.WriteString(`\u`)
	b.WriteByte(hexDigits[(r>>12)&0xf])
	b.WriteByte(hexDigits[(r>>8)&0xf])
	b.WriteByte(hexDigits[(r>>4)&0xf])
	b.WriteByte(hexDigits[r&0xf])
}

func writeString(b *strings.Builder, s string, ascii bool) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := pystr.DecodeRune(s[i:])
		i += n
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				writeU(b, r)
			case r < 0x7f:
				b.WriteRune(r)
			case !ascii:
				b.Write(pystr.AppendRune(nil, r))
			case r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				writeU(b, r1)
				writeU(b, r2)
			default:
				writeU(b, r)
			}
		}
	}
	b.WriteByte('"')
}

// FloatRepr is Python's repr(float): the shortest digits that round-trip,
// in fixed notation when the decimal exponent is in [-4, 16), else in
// scientific notation with at least two exponent digits. Inside json.dumps
// NaN and the infinities print as NaN, Infinity and -Infinity.
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // e.g. -1.2345e+06
	neg := strings.HasPrefix(e, "-")
	e = strings.TrimPrefix(e, "-")
	mant, expPart, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mant, ".", "", 1)
	var out string
	if exp >= -4 && exp < 16 {
		if exp >= 0 {
			if len(digits) <= exp+1 {
				out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			} else {
				out = digits[:exp+1] + "." + digits[exp+1:]
			}
		} else {
			out = "0." + strings.Repeat("0", -exp-1) + digits
		}
	} else {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		es := strconv.Itoa(exp)
		if len(es) < 2 {
			es = "0" + es
		}
		out = m + "e" + sign + es
	}
	if neg {
		return "-" + out
	}
	return out
}

// Round is Python's round(x, n) for the small n the gate uses: the float
// nearest the correctly rounded decimal, half to even.
func Round(x float64, n int) float64 {
	s := strconv.FormatFloat(x, 'f', n, 64) // correctly rounded, half to even on exact ties
	r, _ := strconv.ParseFloat(s, 64)
	return r
}
