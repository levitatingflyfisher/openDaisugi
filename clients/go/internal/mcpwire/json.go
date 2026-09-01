package mcpwire

import (
	"math"
	"strconv"
	"strings"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Compact is pydantic's model_dump_json of a value: no spaces, text as
// it is (only quotes, backslashes and control characters escaped),
// pydantic's float text, and NaN and the infinities as null.
func Compact(v any) string {
	var b strings.Builder
	write(&b, v, "", 0, false)
	return b.String()
}

// Indent is pydantic_core.to_json(v, indent=2): the text FastMCP puts in
// a tool result's text block. NaN and the infinities are written as
// NaN, Infinity and -Infinity.
func Indent(v any) string {
	var b strings.Builder
	write(&b, v, "  ", 0, true)
	return b.String()
}

func floatText(f float64, nanText bool) string {
	switch {
	case math.IsNaN(f):
		if nanText {
			return "NaN"
		}
		return "null"
	case math.IsInf(f, 1):
		if nanText {
			return "Infinity"
		}
		return "null"
	case math.IsInf(f, -1):
		if nanText {
			return "-Infinity"
		}
		return "null"
	}
	return gateroot.FloatJSON(f)
}

func write(b *strings.Builder, v any, unit string, level int, nanText bool) {
	newline := func(l int) {
		if unit == "" {
			return
		}
		b.WriteByte('\n')
		for i := 0; i < l; i++ {
			b.WriteString(unit)
		}
	}
	sep := ":"
	if unit != "" {
		sep = ": "
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case pyjson.Int:
		b.WriteString(x.Text)
	case pyjson.Raw:
		b.WriteString(string(x))
	case int:
		b.WriteString(itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case pyjson.Float:
		b.WriteString(floatText(float64(x), nanText))
	case float64:
		b.WriteString(floatText(x, nanText))
	case string:
		writeString(b, x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		write(b, items, unit, level, nanText)
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(level + 1)
			write(b, e, unit, level+1, nanText)
		}
		newline(level)
		b.WriteByte(']')
	case *pyjson.Object:
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
			writeString(b, k)
			b.WriteString(sep)
			write(b, x.Value(k), unit, level+1, nanText)
		}
		newline(level)
		b.WriteByte('}')
	default:
		panic("mcpwire: cannot write a value of this type")
	}
}

const hexDigits = "0123456789abcdef"

// writeString is serde_json's string: ", \ and the control characters
// below 0x20 escaped, everything else as UTF-8.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := pystr.DecodeRune(s[i:])
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
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[r>>4])
				b.WriteByte(hexDigits[r&0xf])
			} else {
				b.WriteString(s[i : i+n])
			}
		}
		i += n
	}
	b.WriteByte('"')
}
