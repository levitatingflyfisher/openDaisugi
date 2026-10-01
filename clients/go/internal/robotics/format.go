//go:build mujoco

package robotics

import (
	"fmt"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// PyError is an exception the oracle's executor raises: Type is the class,
// and Error() is str(exception), which the supervisor records as
// "executor error: " + str(e).
type PyError struct {
	Type, Msg string
}

func (e *PyError) Error() string { return e.Msg }

// keyError is KeyError(msg): str() of a KeyError is the repr of its text.
func keyError(msg string) *PyError { return &PyError{Type: "KeyError", Msg: pystr.Repr(msg)} }

// Num is a number a caller passed, kept as Python holds it: an int stays
// an int, so repr(torque_limit) prints "5" for 5 and "5.0" for 5.0.
type Num struct {
	// V is a pyjson.Float or a pyjson.Int.
	V any
}

// F is the number as a float.
func (n Num) F() float64 {
	switch x := n.V.(type) {
	case pyjson.Float:
		return float64(x)
	case float64:
		return x
	case pyjson.Int:
		var f float64
		fmt.Sscan(x.Text, &f)
		return f
	}
	return 0
}

// Repr is repr() of the number.
func (n Num) Repr() string {
	if i, ok := n.V.(pyjson.Int); ok {
		return i.Text
	}
	return pmodel.FloatRepr(n.F())
}

// JSON is the number for a JSON dump.
func (n Num) JSON() any {
	if i, ok := n.V.(pyjson.Int); ok {
		return i
	}
	return pyjson.Float(n.F())
}

// tupleRepr is repr() of a tuple of floats.
func tupleRepr(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = pmodel.FloatRepr(x)
	}
	if len(parts) == 1 {
		return "(" + parts[0] + ",)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// pairsRepr is repr() of a list of (str, str) tuples.
func pairsRepr(ps [][2]string) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = "(" + pystr.Repr(p[0]) + ", " + pystr.Repr(p[1]) + ")"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// floatOf is a validated float field.
func floatOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case pyjson.Float:
		return float64(x)
	case pyjson.Int:
		return Num{V: x}.F()
	}
	return 0
}

// floatsOf is a validated tuple of floats.
func floatsOf(v any) []float64 {
	xs, _ := v.([]any)
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = floatOf(x)
	}
	return out
}

func strOf(o *pyjson.Object, k string) string {
	s, _ := o.Value(k).(string)
	return s
}

// className is type(step).__name__ for a registered step type.
func className(step *pyjson.Object) string {
	if m, ok := pmodel.StepTypes[strOf(step, "type")]; ok {
		return m.Name
	}
	return "StepBase"
}
