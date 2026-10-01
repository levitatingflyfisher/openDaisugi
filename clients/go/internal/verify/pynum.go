package verify

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// maxExactInt is 2^53: every int to it is a float64 whose shortest
// digits are the int itself.
var maxExactInt = new(big.Int).Lsh(big.NewInt(1), 53)

// pyNum is a number as Python holds it: an int exactly, or a float.
type pyNum struct {
	isInt bool
	i     *big.Int
	f     float64
}

// pyNumberOf reads v as the number Python compares: a bool is the int 0
// or 1, a float64 is a float (an int read from JSON that a float64 holds
// exactly is the same value either way), and a json.Number is an int kept
// exactly (int text) or a float past the float64 range (Python reads it
// as an infinity).
func pyNumberOf(v interface{}) (pyNum, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return pyNum{isInt: true, i: big.NewInt(1)}, true
		}
		return pyNum{isInt: true, i: big.NewInt(0)}, true
	case float64:
		return pyNum{f: x}, true
	case json.Number:
		t := string(x)
		if !strings.ContainsAny(t, ".eE") {
			if n, ok := new(big.Int).SetString(t, 10); ok {
				return pyNum{isInt: true, i: n}, true
			}
			return pyNum{}, false
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil && !math.IsInf(f, 0) {
			return pyNum{}, false
		}
		return pyNum{f: f}, true
	}
	return pyNum{}, false
}

// pyNumCmp is Python's ordering of two numbers, exact between an int and
// a float: -1, 0 or 1, and false when either is NaN (every comparison
// with NaN is false).
func pyNumCmp(a, b pyNum) (int, bool) {
	switch {
	case a.isInt && b.isInt:
		return a.i.Cmp(b.i), true
	case !a.isInt && !b.isInt:
		switch {
		case math.IsNaN(a.f) || math.IsNaN(b.f):
			return 0, false
		case a.f < b.f:
			return -1, true
		case a.f > b.f:
			return 1, true
		}
		return 0, true
	case a.isInt:
		c, ok := pyNumCmp(b, a)
		return -c, ok
	}
	// a is a float, b an int.
	switch {
	case math.IsNaN(a.f):
		return 0, false
	case math.IsInf(a.f, 1):
		return 1, true
	case math.IsInf(a.f, -1):
		return -1, true
	}
	return new(big.Rat).SetFloat64(a.f).Cmp(new(big.Rat).SetInt(b.i)), true
}

// pyNumEq is Python's == between two numbers.
func pyNumEq(a, b pyNum) bool {
	c, ok := pyNumCmp(a, b)
	return ok && c == 0
}

// exactNumbers makes each number in v the value Python holds, in place: a
// float64 for every float and every int to 2^53, else a json.Number (an
// int past 2^53, kept as its exact text, or a number past the float64
// range). An int past 2^53 stays text even when a float64 holds it
// exactly: a float64 prints as its shortest digits (2^60 as
// 1152921504606847000), not as the int Z3 is given.
func exactNumbers(v interface{}) interface{} {
	switch x := v.(type) {
	case json.Number:
		t := string(x)
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return x
		}
		if !strings.ContainsAny(t, ".eE") {
			n, ok := new(big.Int).SetString(t, 10)
			if !ok {
				return x
			}
			if n.CmpAbs(maxExactInt) > 0 {
				return x
			}
		}
		return f
	case map[string]interface{}:
		for k, e := range x {
			x[k] = exactNumbers(e)
		}
	case []interface{}:
		for i, e := range x {
			x[i] = exactNumbers(e)
		}
	}
	return v
}

// pyFloat is float(v) for a number read from JSON, as pydantic coerces an
// int into a float field (rounded to the nearest float64).
func pyFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// floatValues is v with every number a float64, as a pydantic float field
// (or a tuple or dict of them) holds it.
func floatValues(v interface{}) interface{} {
	switch x := v.(type) {
	case json.Number:
		if f, ok := pyFloat(x); ok {
			return f
		}
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, e := range x {
			out[k] = floatValues(e)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = floatValues(e)
		}
		return out
	case pyTuple:
		out := make(pyTuple, len(x))
		for i, e := range x {
			out[i] = floatValues(e)
		}
		return out
	}
	return v
}
