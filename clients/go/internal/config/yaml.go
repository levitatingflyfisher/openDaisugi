// Package config reads ~/.opendaisugi/config.yaml the way
// opendaisugi.config.load_config does, for the settings the gate commands
// read, and finds the gate hook mode installed in Claude Code's settings.
//
// Python validates the whole file with pydantic: one bad field anywhere
// makes load_config raise. The YAML is read by package pyyaml, as PyYAML's
// SafeLoader reads it; a value its result model does not hold is
// ErrUnsupported, for the caller to refuse with a reason.
package config

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// ErrUnsupported marks YAML outside the modeled subset. The caller must
// not guess: it refuses the command and says why.
var ErrUnsupported = errors.New("config.yaml uses YAML this binary does not read yet")

// ErrInvalid marks a file load_config is certain to refuse.
var ErrInvalid = errors.New("config.yaml does not validate")

// Kind is the type PyYAML gives a node.
type Kind int

const (
	Null Kind = iota
	Bool
	Int
	Float
	Str
	Seq
	Map
	// Other is a date or a datetime: no field takes one.
	Other
)

// Value is one YAML node. Keys of a mapping keep their first position; a
// repeated key takes its last value, as the SafeLoader's dict does. Only
// string keys are kept by name; NonStrKeys counts the others.
type Value struct {
	Kind       Kind
	B          bool
	Text       string // an Int's digits, a Float's text, a Str's value
	Items      []Value
	Keys       []string
	Map        map[string]Value
	NonStrKeys int
}

// Doc is a parsed top-level mapping (kept for callers that read one).
type Doc struct {
	Keys []string
	Vals map[string]Value
}

// YAMLError is an exception yaml.safe_load raises: load_config does not
// catch it, so to a caller it is a file that does not load (ErrInvalid).
type YAMLError struct{ Exc *pystr.Exception }

func (e *YAMLError) Error() string { return pyyaml.Qualified(e.Exc) + ": " + pyyaml.FirstLine(e.Exc) }

func (e *YAMLError) Unwrap() error { return ErrInvalid }

// ParseYAML is yaml.safe_load(path.read_text()) on one document.
func ParseYAML(text string) (Value, error) {
	if !utf8.ValidString(text) {
		return Value{}, ErrUnsupported
	}
	// read_text reads with universal newlines.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	v, exc, why := pyyaml.Load(text)
	if why != nil {
		return Value{}, ErrUnsupported
	}
	if exc != nil {
		return Value{}, &YAMLError{exc}
	}
	return fromPy(v)
}

func fromPy(v any) (Value, error) {
	switch x := v.(type) {
	case nil:
		return Value{Kind: Null}, nil
	case bool:
		return Value{Kind: Bool, B: x}, nil
	case pyjson.Int:
		return Value{Kind: Int, Text: x.Text}, nil
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) {
			return Value{Kind: Float, Text: "NaN"}, nil
		}
		return Value{Kind: Float, Text: strconv.FormatFloat(f, 'g', -1, 64)}, nil
	case string:
		return Value{Kind: Str, Text: x}, nil
	case pyyaml.Timestamp:
		return Value{Kind: Other, Text: x.Text}, nil
	case []any:
		out := Value{Kind: Seq}
		for _, e := range x {
			c, err := fromPy(e)
			if err != nil {
				return Value{}, err
			}
			out.Items = append(out.Items, c)
		}
		return out, nil
	case *pyjson.Object:
		out := Value{Kind: Map, Map: map[string]Value{}}
		for _, k := range x.Keys() {
			c, err := fromPy(x.Value(k))
			if err != nil {
				return Value{}, err
			}
			if strings.HasPrefix(k, "\x00") {
				out.NonStrKeys++
				continue
			}
			out.Keys = append(out.Keys, k)
			out.Map[k] = c
		}
		return out, nil
	}
	return Value{}, ErrUnsupported
}

// Parse reads a file whose top level must be a mapping (or empty).
func Parse(text string) (*Doc, error) {
	v, err := ParseYAML(text)
	if err != nil {
		return nil, err
	}
	switch v.Kind {
	case Map:
		return &Doc{Keys: v.Keys, Vals: v.Map}, nil
	case Null:
		return &Doc{Vals: map[string]Value{}}, nil
	}
	return nil, &TopLevelError{v}
}

// TopLevelError is a document whose top level is not a mapping.
type TopLevelError struct{ V Value }

func (e *TopLevelError) Error() string { return "config.yaml does not hold a mapping" }
