package gate

import (
	"math"
	"math/big"

	"daisugi-verify/internal/pyjson"
)

// pyMethod is a bound method getattr found on a value: it equals nothing,
// is no str, no number and has no len.
type pyMethod struct{ name string }

// pyMissing is predicate_z3._MISSING.
type pyMissingT struct{}

var pyMissing = pyMissingT{}

// numeric returns a value's number as an exact rational: bool, int and
// finite float. ok is false for any other value; nan and inf are floats
// with no rational form.
func numeric(v any) (r *big.Rat, isNum, finite bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewRat(1, 1), true, true
		}
		return big.NewRat(0, 1), true, true
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok {
			return nil, true, false
		}
		return new(big.Rat).SetInt(n), true, true
	case int:
		return big.NewRat(int64(x), 1), true, true
	case pyjson.Float:
		return floatRat(float64(x))
	case float64:
		return floatRat(x)
	}
	return nil, false, false
}

func floatRat(f float64) (*big.Rat, bool, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, true, false
	}
	r := new(big.Rat)
	r.SetFloat64(f)
	return r, true, true
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case pyjson.Float:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// pyEq is Python's == on values json.loads makes (and a step's dump).
func pyEq(a, b any) bool {
	if ra, na, fa := numeric(a); na {
		rb, nb, fb := numeric(b)
		if !nb {
			return false
		}
		if fa && fb {
			return ra.Cmp(rb) == 0
		}
		// An inf equals the same inf; nan equals nothing.
		x, xok := asFloat(a)
		y, yok := asFloat(b)
		return xok && yok && x == y && !math.IsNaN(x)
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
			if !pyEq(x[i], y[i]) {
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
			yv, present := y.Get(k)
			if !present || !pyEq(x.Value(k), yv) {
				return false
			}
		}
		return true
	}
	return false
}

// pyIn is `v in values`.
func pyIn(v any, values []any) bool {
	for _, x := range values {
		if pyEq(v, x) {
			return true
		}
	}
	return false
}

var (
	strMethods = set("capitalize", "casefold", "center", "count", "encode", "endswith", "expandtabs", "find",
		"format", "format_map", "index", "isalnum", "isalpha", "isascii", "isdecimal", "isdigit", "isidentifier",
		"islower", "isnumeric", "isprintable", "isspace", "istitle", "isupper", "join", "ljust", "lower",
		"lstrip", "maketrans", "partition", "removeprefix", "removesuffix", "replace", "rfind", "rindex",
		"rjust", "rpartition", "rsplit", "rstrip", "split", "splitlines", "startswith", "strip", "swapcase",
		"title", "translate", "upper", "zfill")
	listMethods  = set("append", "clear", "copy", "count", "extend", "index", "insert", "pop", "remove", "reverse", "sort")
	intMethods   = set("as_integer_ratio", "bit_count", "bit_length", "conjugate", "from_bytes", "is_integer", "to_bytes")
	floatMethods = set("as_integer_ratio", "conjugate", "fromhex", "hex", "is_integer")
)

// getattr is getattr(v, name, _MISSING) for a value that is no dict.
func getattr(v any, name string) any {
	if len(name) >= 2 && name[:2] == "__" {
		unported("a predicate path reads a Python dunder attribute")
	}
	switch x := v.(type) {
	case string:
		if strMethods[name] {
			return pyMethod{name}
		}
	case []any:
		if listMethods[name] {
			return pyMethod{name}
		}
	case bool, pyjson.Int, int:
		if intMethods[name] {
			return pyMethod{name}
		}
		r, _, _ := numeric(x)
		switch name {
		case "real", "numerator":
			return pyjson.Int{Text: r.Num().String()}
		case "imag":
			return pyjson.Int{Text: "0"}
		case "denominator":
			return pyjson.Int{Text: "1"}
		}
	case pyjson.Float, float64:
		if floatMethods[name] {
			return pyMethod{name}
		}
		f, _ := asFloat(x)
		switch name {
		case "real":
			return pyjson.Float(f)
		case "imag":
			return pyjson.Float(0)
		}
	case pyMethod:
		unported("a predicate path reads an attribute of a method")
	}
	return pyMissing
}
