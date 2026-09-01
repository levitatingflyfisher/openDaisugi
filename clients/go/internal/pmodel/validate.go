package pmodel

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Mode is pydantic's input mode: JSON (model_validate_json) or Python
// (validate_python on values json.loads made). Their messages differ.
type Mode int

const (
	JSON Mode = iota
	Python
)

// Err is one line of a ValidationError.
type Err struct {
	Loc   []any // string keys and int indexes
	Type  string
	Msg   string
	Input any
	// NoInput leaves input_value out, as json_invalid's text form has it.
	InputText string
}

// ValidationError is pydantic_core.ValidationError.
type ValidationError struct {
	Title string
	Errs  []Err
}

func (e *ValidationError) Error() string { return e.String() }

// String is str(exc).
func (e *ValidationError) String() string {
	var b strings.Builder
	n := len(e.Errs)
	s := "s"
	if n == 1 {
		s = ""
	}
	fmt.Fprintf(&b, "%d validation error%s for %s", n, s, e.Title)
	for _, er := range e.Errs {
		b.WriteString("\n")
		if len(er.Loc) > 0 {
			parts := make([]string, len(er.Loc))
			for i, l := range er.Loc {
				parts[i] = fmt.Sprint(l)
			}
			b.WriteString(strings.Join(parts, ".") + "\n")
		}
		iv := er.InputText
		if iv == "" {
			iv = TruncatedRepr(er.Input)
		}
		fmt.Fprintf(&b, "  %s [type=%s, input_value=%s, input_type=%s]\n", er.Msg, er.Type, iv, TypeName(er.Input))
		fmt.Fprintf(&b, "    For further information visit https://errors.pydantic.dev/2.13/v/%s", er.Type)
	}
	return b.String()
}

// Schema validates one value.
type Schema interface {
	validate(v any, loc []any, mode Mode) (any, []Err)
}

func with(loc []any, k any) []any {
	out := make([]any, len(loc)+1)
	copy(out, loc)
	out[len(loc)] = k
	return out
}

// one is a single error at loc. It copies loc, since the loops below pass
// a buffer they reuse for each child.
func one(loc []any, typ, msg string, v any) []Err {
	return []Err{{Loc: append([]any(nil), loc...), Type: typ, Msg: msg, Input: v}}
}

// child is a buffer for the location of each child of loc: the loops set
// its last item per child instead of allocating a new location each time.
func child(loc []any) []any {
	b := make([]any, len(loc)+1)
	copy(b, loc)
	return b
}

// Str is str, with an optional max_length.
type Str struct{ MaxLen int }

func (s Str) validate(v any, loc []any, mode Mode) (any, []Err) {
	x, ok := v.(string)
	if !ok {
		return nil, one(loc, "string_type", "Input should be a valid string", v)
	}
	if s.MaxLen > 0 && pystr.Len(x) > s.MaxLen {
		return nil, one(loc, "string_too_long", fmt.Sprintf("String should have at most %d characters", s.MaxLen), v)
	}
	return v, nil
}

// Any is typing.Any.
type Any struct{}

func (Any) validate(v any, loc []any, mode Mode) (any, []Err) { return v, nil }

// Bool is bool, lax.
type Bool struct{}

var (
	trueWords  = map[string]bool{"1": true, "on": true, "t": true, "true": true, "y": true, "yes": true}
	falseWords = map[string]bool{"0": true, "off": true, "f": true, "false": true, "n": true, "no": true}
)

func (Bool) validate(v any, loc []any, mode Mode) (any, []Err) {
	parsing := func() []Err {
		return one(loc, "bool_parsing", "Input should be a valid boolean, unable to interpret input", v)
	}
	typeErr := func() []Err { return one(loc, "bool_type", "Input should be a valid boolean", v) }
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		l := strings.ToLower(x)
		if trueWords[l] {
			return true, nil
		}
		if falseWords[l] {
			return false, nil
		}
		return nil, parsing()
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok || !n.IsInt64() {
			return nil, typeErr()
		}
		switch n.Int64() {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return nil, parsing()
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) >= 9.223372036854775807e18 {
			return nil, typeErr()
		}
		switch f {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return nil, parsing()
	}
	return nil, typeErr()
}

// Int is int, lax. The value is a pyjson.Int.
type Int struct{}

func (Int) validate(v any, loc []any, mode Mode) (any, []Err) {
	switch x := v.(type) {
	case bool:
		if x {
			return pyjson.Int{Text: "1"}, nil
		}
		return pyjson.Int{Text: "0"}, nil
	case pyjson.Int:
		return v, nil
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, one(loc, "finite_number", "Input should be a finite number", v)
		}
		if f != math.Trunc(f) {
			return nil, one(loc, "int_from_float", "Input should be a valid integer, got a number with a fractional part", v)
		}
		if math.Abs(f) >= 9.223372036854775807e18 {
			return nil, one(loc, "int_parsing_size", "Unable to parse input string as an integer, exceeded maximum size", v)
		}
		return pyjson.Int{Text: strconv.FormatInt(int64(f), 10)}, nil
	case string:
		if t, ok := intFromString(x); ok {
			return pyjson.Int{Text: t}, nil
		}
		return nil, one(loc, "int_parsing", "Input should be a valid integer, unable to parse string as an integer", v)
	}
	return nil, one(loc, "int_type", "Input should be a valid integer", v)
}

// intFromString reads a str as pydantic reads it into an int: surrounding
// whitespace, a sign, underscores between digits, and a ".0" tail.
func intFromString(s string) (string, bool) {
	s = pystr.Strip(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		if strings.Trim(s[i+1:], "0") != "" {
			return "", false
		}
		s = s[:i]
	}
	neg := false
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return "", false
	}
	s = strings.ReplaceAll(s, "_", "")
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0", true
	}
	if neg {
		return "-" + s, true
	}
	return s, true
}

// Float is float, lax. The value is a float64.
type Float struct{}

func (Float) validate(v any, loc []any, mode Mode) (any, []Err) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1.0, nil
		}
		return 0.0, nil
	case pyjson.Int:
		f, _ := new(big.Float).SetPrec(200).SetString(x.Text)
		out, _ := f.Float64()
		if mode == Python && math.IsInf(out, 0) {
			// float(int) overflows in Python mode; in JSON mode jiter's
			// big int reads as an infinity.
			return nil, one(loc, "float_type", "Input should be a valid number", v)
		}
		return out, nil
	case pyjson.Float:
		return float64(x), nil
	case string:
		if f, ok := floatFromString(x); ok {
			return f, nil
		}
		return nil, one(loc, "float_parsing", "Input should be a valid number, unable to parse string as a number", v)
	}
	return nil, one(loc, "float_type", "Input should be a valid number", v)
}

// FloatFromString reads a str as Python's float() does.
func FloatFromString(s string) (float64, bool) { return floatFromString(s) }

// floatFromString reads a str as float() does.
func floatFromString(s string) (float64, bool) {
	s = pystr.Strip(s)
	low := strings.ToLower(s)
	switch strings.TrimLeft(low, "+-") {
	case "nan":
		return math.NaN(), true
	case "inf", "infinity":
		if strings.HasPrefix(low, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	if strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
		return 0, false
	}
	t := strings.ReplaceAll(s, "_", "")
	if t == "" || strings.ContainsAny(t, "xXpP") {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// Nullable is X | None.
type Nullable struct{ Inner Schema }

func (n Nullable) validate(v any, loc []any, mode Mode) (any, []Err) {
	if v == nil {
		return nil, nil
	}
	return n.Inner.validate(v, loc, mode)
}

// List is list[X].
type List struct{ Elem Schema }

func (l List) validate(v any, loc []any, mode Mode) (any, []Err) {
	xs, ok := v.([]any)
	if !ok {
		msg := "Input should be a valid list"
		if mode == JSON {
			msg = "Input should be a valid array"
		}
		return nil, one(loc, "list_type", msg, v)
	}
	out := make([]any, 0, len(xs))
	var errs []Err
	at := child(loc)
	for i, x := range xs {
		at[len(loc)] = i
		y, e := l.Elem.validate(x, at, mode)
		errs = append(errs, e...)
		out = append(out, y)
	}
	return out, errs
}

// Tuple is tuple[A, B, ...] of fixed length.
type Tuple struct{ Items []Schema }

func (t Tuple) validate(v any, loc []any, mode Mode) (any, []Err) {
	xs, ok := v.([]any)
	if !ok {
		msg := "Input should be a valid tuple"
		if mode == JSON {
			msg = "Input should be a valid array"
		}
		return nil, one(loc, "tuple_type", msg, v)
	}
	if len(xs) > len(t.Items) {
		return nil, one(loc, "too_long", fmt.Sprintf("Tuple should have at most %d items after validation, not %d", len(t.Items), len(xs)), v)
	}
	out := make([]any, 0, len(t.Items))
	var errs []Err
	for i, s := range t.Items {
		if i >= len(xs) {
			errs = append(errs, Err{Loc: with(loc, i), Type: "missing", Msg: "Field required", Input: v})
			continue
		}
		y, e := s.validate(xs[i], with(loc, i), mode)
		errs = append(errs, e...)
		out = append(out, y)
	}
	return out, errs
}

// Dict is dict[str, X].
type Dict struct{ Val Schema }

func (d Dict) validate(v any, loc []any, mode Mode) (any, []Err) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		msg := "Input should be a valid dictionary"
		if mode == JSON {
			msg = "Input should be an object"
		}
		return nil, one(loc, "dict_type", msg, v)
	}
	out := pyjson.NewObjectCap(o.Len())
	var errs []Err
	at := child(loc)
	for _, k := range o.Keys() {
		at[len(loc)] = k
		y, e := d.Val.validate(o.Value(k), at, mode)
		errs = append(errs, e...)
		out.Set(k, y)
	}
	return out, errs
}

// Literal is Literal["a", "b", ...].
type Literal struct{ Choices []string }

func (l Literal) validate(v any, loc []any, mode Mode) (any, []Err) {
	if s, ok := v.(string); ok {
		for _, c := range l.Choices {
			if s == c {
				return s, nil
			}
		}
	}
	quoted := make([]string, len(l.Choices))
	for i, c := range l.Choices {
		quoted[i] = pystr.Repr(c)
	}
	msg := "Input should be " + quoted[0]
	if len(quoted) > 1 {
		msg = "Input should be " + strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
	return nil, one(loc, "literal_error", msg, v)
}

// Field is one model field.
type Field struct {
	Name     string
	Schema   Schema
	Required bool
	// Default gives the value of an absent field.
	Default func() any
}

// Model is a pydantic BaseModel with extra fields ignored.
type Model struct {
	Name   string
	Fields []Field
	// Finite is the after-validator Envelope and ActionPlan carry: once
	// the fields validate, each number in the result that is not finite
	// is a finite_number error at its place (NonFinite).
	Finite bool

	once  sync.Once
	names []any
}

// boxedNames is each field's name as an interface value, made once.
func (m *Model) boxedNames() []any {
	m.once.Do(func() {
		m.names = make([]any, len(m.Fields))
		for i, f := range m.Fields {
			m.names[i] = f.Name
		}
	})
	return m.names
}

func (m *Model) validate(v any, loc []any, mode Mode) (any, []Err) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		msg := "Input should be a valid dictionary or instance of " + m.Name
		if mode == JSON {
			msg = "Input should be an object"
		}
		return nil, one(loc, "model_type", msg, v)
	}
	return m.fields(o, loc, mode)
}

func (m *Model) fields(o *pyjson.Object, loc []any, mode Mode) (any, []Err) {
	out := pyjson.NewObjectCap(len(m.Fields))
	var errs []Err
	names := m.boxedNames()
	at := child(loc)
	for fi, f := range m.Fields {
		x, present := o.Get(f.Name)
		if !present {
			if f.Required {
				errs = append(errs, Err{Loc: with(loc, f.Name), Type: "missing", Msg: "Field required", Input: o})
				continue
			}
			out.Set(f.Name, f.Default())
			continue
		}
		at[len(loc)] = names[fi]
		y, e := f.Schema.validate(x, at, mode)
		errs = append(errs, e...)
		out.Set(f.Name, y)
	}
	if m.Finite && len(errs) == 0 {
		errs = NonFinite(out, loc)
	}
	return out, errs
}

// NonFinite is models.non_finite_error's walk: one finite_number error
// for each NaN, Infinity or -Infinity in v (a validated value, as its
// model_dump holds it), at loc plus its place, in document order.
func NonFinite(v any, loc []any) []Err {
	var errs []Err
	var walk func(v any, loc []any)
	walk = func(v any, loc []any) {
		switch x := v.(type) {
		case pyjson.Float, float64:
			f, _ := x.(float64)
			if p, ok := x.(pyjson.Float); ok {
				f = float64(p)
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				errs = append(errs, Err{Loc: append([]any(nil), loc...), Type: "finite_number",
					Msg: "Input should be a finite number", Input: x})
			}
		case *pyjson.Object:
			for _, k := range x.Keys() {
				walk(x.Value(k), with(loc, k))
			}
		case []any:
			for i, e := range x {
				walk(e, with(loc, i))
			}
		}
	}
	walk(v, loc)
	return errs
}

// Validate runs a schema on a value and returns pydantic's error for it.
func Validate(title string, s Schema, v any, mode Mode) (any, *ValidationError) {
	out, errs := s.validate(v, nil, mode)
	if len(errs) > 0 {
		return nil, &ValidationError{Title: title, Errs: errs}
	}
	return out, nil
}

// ValidateJSON is model_validate_json: jiter's parse, then the schema.
func ValidateJSON(title string, s Schema, text string) (any, *ValidationError) {
	v, jerr := ParseJSON(text)
	if jerr != nil {
		return nil, &ValidationError{Title: title, Errs: []Err{{
			Type:  "json_invalid",
			Msg:   "Invalid JSON: " + jerr.Msg + " at " + Position(text, jerr.At),
			Input: text,
		}}}
	}
	return Validate(title, s, v, JSON)
}
