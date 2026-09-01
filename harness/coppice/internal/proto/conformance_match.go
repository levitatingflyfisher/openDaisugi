package proto

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file is the matching language of the protocol corpus in
// testdata/protocol. The Python replay copies these rules line for line, so
// every rule lives here and nowhere else. README.md in that directory states
// the same rules in prose.

// SkipMarker starts an expected line that names a case the server does not
// implement yet. A replay logs it as a gap. It never counts as a pass.
const SkipMarker = "#skip"

// Wildcard, as a whole string value in an expected line, matches any present
// value.
const Wildcard = "*"

// IDName, as a whole string value in an expected line, matches the id the
// request carried.
const IDName = "$id"

// Matcher holds the values that expected lines bound with "$name". One
// Matcher serves one corpus file, because a file is replayed on one
// connection and its bindings carry across cases.
type Matcher struct {
	Bindings map[string]any
}

func NewMatcher() *Matcher { return &Matcher{Bindings: map[string]any{}} }

// Match compares actual against expected and returns nil when they agree.
// The rules, in order:
//
//   - An expected string "*" matches any present value.
//   - An expected string "$id" matches only reqID, the id the request carried.
//   - Any other expected string "$name" matches any present value and binds
//     it under name, replacing an older binding of the same name.
//   - An expected object matches when every key it lists is present in the
//     actual object and matches. Extra keys in the actual object are allowed.
//   - An expected array matches an actual array of the same length whose
//     elements match one to one.
//   - Every other value matches only an equal JSON value.
//
// The error names the path of the first mismatch.
func (m *Matcher) Match(expected, actual, reqID any) error {
	return m.match("", expected, actual, reqID)
}

func (m *Matcher) match(path string, expected, actual, reqID any) error {
	switch e := expected.(type) {
	case string:
		if e == Wildcard {
			return nil
		}
		if e == IDName {
			if !sameJSON(actual, reqID) {
				return fmt.Errorf("%s: want the request id %s, got %s", at(path), jsonOf(reqID), jsonOf(actual))
			}
			return nil
		}
		if name, ok := bindingName(e); ok {
			m.Bindings[name] = actual
			return nil
		}
		if a, ok := actual.(string); !ok || a != e {
			return fmt.Errorf("%s: want %s, got %s", at(path), jsonOf(e), jsonOf(actual))
		}
		return nil
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object, got %s", at(path), jsonOf(actual))
		}
		for k, ev := range e {
			av, present := a[k]
			if !present {
				return fmt.Errorf("%s: key %q is missing", at(path), k)
			}
			if err := m.match(join(path, k), ev, av, reqID); err != nil {
				return err
			}
		}
		return nil
	case []any:
		a, ok := actual.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array, got %s", at(path), jsonOf(actual))
		}
		if len(a) != len(e) {
			return fmt.Errorf("%s: want %d elements, got %d", at(path), len(e), len(a))
		}
		for i := range e {
			if err := m.match(join(path, fmt.Sprint(i)), e[i], a[i], reqID); err != nil {
				return err
			}
		}
		return nil
	default:
		if !sameJSON(expected, actual) {
			return fmt.Errorf("%s: want %s, got %s", at(path), jsonOf(expected), jsonOf(actual))
		}
		return nil
	}
}

// Substitute returns v with every string "$name" replaced by the value bound
// under name. A name with no binding is an error, so a corpus file cannot
// send a placeholder to the server by mistake. "$id" is never bound, and
// "*" is left as it is.
func (m *Matcher) Substitute(v any) (any, error) {
	switch x := v.(type) {
	case string:
		if x == IDName {
			return nil, fmt.Errorf("%q is never bound. A request line may not use it.", x)
		}
		name, ok := bindingName(x)
		if !ok {
			return x, nil
		}
		bound, ok := m.Bindings[name]
		if !ok {
			return nil, fmt.Errorf("%q is not bound. An expected line must bind it first.", x)
		}
		return bound, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, ev := range x {
			sv, err := m.Substitute(ev)
			if err != nil {
				return nil, err
			}
			out[k] = sv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, ev := range x {
			sv, err := m.Substitute(ev)
			if err != nil {
				return nil, err
			}
			out[i] = sv
		}
		return out, nil
	default:
		return v, nil
	}
}

// bindingName reports whether s is a "$name" placeholder other than "$id",
// and returns the name.
func bindingName(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' || s == IDName {
		return "", false
	}
	return s[1:], true
}

// RequestID returns the id a decoded request line carries, or nil when the
// line is not an object or has no id key. Match compares "$id" against it.
func RequestID(req any) any {
	obj, ok := req.(map[string]any)
	if !ok {
		return nil
	}
	return obj["id"]
}

// HasID reports whether a decoded line is an object with an id key. A reply
// always has one, even when its value is null. An event or a notification
// has none, and a replay skips it while it waits for a reply.
func HasID(line any) bool {
	obj, ok := line.(map[string]any)
	if !ok {
		return false
	}
	_, has := obj["id"]
	return has
}

func sameJSON(a, b any) bool { return jsonOf(a) == jsonOf(b) }

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func at(path string) string {
	if path == "" {
		return "top level"
	}
	return path
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return strings.Join([]string{path, key}, ".")
}
