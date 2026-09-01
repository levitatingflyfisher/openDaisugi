package pmodel

import (
	"fmt"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// TypeName is type(v).__name__ for a value json.loads makes.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case pyjson.Int, int, int64:
		return "int"
	case pyjson.Float, float64:
		return "float"
	case string:
		return "str"
	case []any, []string:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Repr is repr(v) for a value json.loads makes.
func Repr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case pyjson.Int:
		return x.Text
	case int:
		return fmt.Sprint(x)
	case int64:
		return fmt.Sprint(x)
	case pyjson.Float:
		return FloatRepr(float64(x))
	case float64:
		return FloatRepr(x)
	case string:
		return pystr.Repr(x)
	case []string:
		return pystr.ReprList(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Repr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyjson.Object:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			parts = append(parts, pystr.Repr(k)+": "+Repr(x.Value(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// FloatRepr is repr(float), with nan and inf as Python prints them.
func FloatRepr(f float64) string {
	s := pyjson.FloatRepr(f)
	switch s {
	case "NaN":
		return "nan"
	case "Infinity":
		return "inf"
	case "-Infinity":
		return "-inf"
	}
	return s
}

// TruncatedRepr is how pydantic shows an input value: its repr, cut to
// the first 25 and last 24 code points when longer than 50.
func TruncatedRepr(v any) string {
	r := Repr(v)
	if pystr.Len(r) > 50 {
		return pystr.Slice(r, 0, 25) + "..." + pystr.Slice(r, -24, 1<<30)
	}
	return r
}
