package gate

import (
	"fmt"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file gives a decoded JSON value the Python behavior the oracle's
// code meets it with: type(v).__name__, repr(v), str(v), len(v), and the
// exceptions a str method raises on a value that is no str.

func pyTypeName(v any) string { return pmodel.TypeName(v) }

func pyValueRepr(v any) string { return pmodel.Repr(v) }

// pyStrOf is str(v).
func pyStrOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyValueRepr(v)
}

// pyStrOr is str(v or ""): the str of a truthy value, else "".
func pyStrOr(v any) string {
	if !pyjson.Truthy(v) {
		return ""
	}
	return pyStrOf(v)
}

// noAttr raises AttributeError for a str method called on a value that
// is no str.
func noAttr(v any, attr string) {
	panic(pystr.NewException("AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pyTypeName(v), attr)))
}

// pyLenOf is len(v).
func pyLenOf(v any) int {
	switch x := v.(type) {
	case string:
		return pystr.Len(x)
	case []any:
		return len(x)
	case *pyjson.Object:
		return x.Len()
	}
	panic(pystr.NewException("TypeError", fmt.Sprintf("object of type '%s' has no len()", pyTypeName(v))))
}

// stringTypeError is the ValidationError pydantic raises when a model's
// str field gets a value that is no str.
func stringTypeError(model, field string, v any) *pystr.Exception {
	e := &pmodel.ValidationError{Title: model, Errs: []pmodel.Err{{Loc: []any{field}, Type: "string_type",
		Msg: "Input should be a valid string", Input: v}}}
	return pystr.NewException("ValidationError", e.String())
}
