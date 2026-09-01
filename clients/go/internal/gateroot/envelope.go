package gateroot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// ErrUnsure marks an envelope pydantic may coerce or refuse in a way this
// package does not model (a numeric string for an int, a float, a
// robotics bound). The caller refuses it and writes nothing.
var ErrUnsure = errors.New("the envelope holds a value this binary does not read yet")

// InvalidError is an envelope pydantic is certain to refuse.
type InvalidError struct{ Why string }

func (e *InvalidError) Error() string { return "invalid envelope: " + e.Why }

func invalidf(format string, a ...any) error {
	return &InvalidError{Why: fmt.Sprintf(format, a...)}
}

// nonFinite is models.non_finite_error's refusal: an envelope with NaN,
// Infinity or -Infinity in any number is invalid, never a null limit.
func nonFinite(k string) error {
	return invalidf("%s: Input should be a finite number", k)
}

// StarterAllowlist is gate._STARTER_SHELL_ALLOWLIST, sorted.
var StarterAllowlist = []string{
	"cargo", "cat", "cd", "echo", "find", "git", "grep", "head", "ls", "npm", "printf",
	"pwd", "pytest", "python", "python3", "rg", "sort", "tail", "uniq", "wc", "which",
}

// DefaultFallbackModel is FallbackStrategy.model's default.
const DefaultFallbackModel = "anthropic/claude-sonnet-4-20250514"

func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "env_" + hex.EncodeToString(b), nil
}

func strs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// StarterEnvelope is gate.starter_envelope(ws, stakes="medium",
// allow_shell_decomposition=decompose) as a model dump; ws is resolved.
func StarterEnvelope(ws string, decompose bool) (*pyjson.Object, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	in := pyjson.NewObject().
		Set("id", id).
		Set("generated_by", "opendaisugi.gate.starter_envelope").
		Set("task", "session in "+ws).
		Set("permissions", pyjson.NewObject().
			Set("file_read", strs([]string{ws + "/**"})).
			Set("file_write", strs([]string{ws + "/**"})).
			Set("shell", true).
			Set("shell_allowlist", strs(StarterAllowlist)).
			Set("shell_allow_decomposition", decompose).
			Set("network", false).
			Set("max_execution_time_s", pyjson.Int{Text: "60"}).
			Set("max_output_size_mb", pyjson.Int{Text: "20"})).
		Set("stakes", "medium")
	return Validate(in)
}

// Register is gate.register_envelope: the dump written to
// envelopes/<safe session or default>.json, the directory 0700 and the
// file 0600 from the moment it exists. followed is the file written when
// the envelope's path was a symlink.
func Register(env *pyjson.Object, session string, root string) (path, followed string, err error) {
	d := EnvelopesDir(root)
	if err := mkdirPrivate(d); err != nil {
		return "", "", err
	}
	name := "default"
	if session != "" {
		name = SafeSessionID(session)
	}
	p := Join(d, name+".json")
	// pydantic's model_dump_json keeps non-ASCII text as is.
	followed, err = WriteFileMode(p, pyjson.DumpsIndent(env, 2, false), 0o600)
	return p, followed, err
}

// Validate is Envelope(**in) followed by model_dump(): every field in
// model order with its default filled in. Only exact types are read;
// pydantic's coercions are refused with ErrUnsure.
func Validate(in *pyjson.Object) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	id, err := optStr(in, "id", false)
	if err != nil {
		return nil, err
	}
	if id == nil {
		s, err := newID()
		if err != nil {
			return nil, err
		}
		id = s
	}
	out.Set("id", id)
	for _, k := range []string{"generated_by", "task"} {
		v, present := in.Get(k)
		if !present {
			return nil, invalidf("%s: field required", k)
		}
		s, err := str(v, k)
		if err != nil {
			return nil, err
		}
		out.Set(k, s)
	}
	pv, present := in.Get("permissions")
	if !present {
		return nil, invalidf("permissions: field required")
	}
	perms, err := permission(pv)
	if err != nil {
		return nil, err
	}
	out.Set("permissions", perms)
	for _, k := range []string{"invariants", "postconditions"} {
		l, err := modelList(in, k)
		if err != nil {
			return nil, err
		}
		out.Set(k, l)
	}
	fb, err := fallback(in)
	if err != nil {
		return nil, err
	}
	out.Set("fallback", fb)
	parent, err := optStr(in, "parent_envelope", true)
	if err != nil {
		return nil, err
	}
	out.Set("parent_envelope", parent)
	tight, err := boolean(in, "tightening_only", true)
	if err != nil {
		return nil, err
	}
	out.Set("tightening_only", tight)
	summary, err := optStr(in, "summary", true)
	if err != nil {
		return nil, err
	}
	if s, isStr := summary.(string); isStr && utf8.RuneCountInString(s) > 80 {
		return nil, invalidf("summary: at most 80 characters")
	}
	out.Set("summary", summary)
	cacheKey, err := optStr(in, "cache_key", true)
	if err != nil {
		return nil, err
	}
	out.Set("cache_key", cacheKey)
	stakes, err := literal(in, "stakes", "low", "low", "medium", "high", "physical")
	if err != nil {
		return nil, err
	}
	out.Set("stakes", stakes)
	policy, err := literal(in, "shell_interpreter_policy", "surface", "surface", "strict", "allow")
	if err != nil {
		return nil, err
	}
	out.Set("shell_interpreter_policy", policy)
	return out, nil
}

// kindOf names a decoded JSON value's type for messages.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case pyjson.Int:
		return "int"
	case pyjson.Float:
		return "float"
	case string:
		return "string"
	case []any:
		return "list"
	case *pyjson.Object:
		return "object"
	}
	return "value"
}

// str accepts a string. pydantic refuses every other JSON type for str.
func str(v any, k string) (string, error) {
	s, isStr := v.(string)
	if !isStr {
		return "", invalidf("%s: a string is required, not %s", k, kindOf(v))
	}
	return s, nil
}

func optStr(o *pyjson.Object, k string, nullable bool) (any, error) {
	v, present := o.Get(k)
	if !present {
		return nil, nil
	}
	if v == nil {
		if nullable {
			return nil, nil
		}
		return nil, invalidf("%s: a string is required, not null", k)
	}
	return str(v, k)
}

// boolean is pydantic's lax bool: a bool, 0 or 1, 0.0 or 1.0, or one of
// the words true/false, yes/no, on/off, t/f, y/n, 1/0 in any case.
func boolean(o *pyjson.Object, k string, def bool) (bool, error) {
	v, present := o.Get(k)
	if !present {
		return def, nil
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case pyjson.Int:
		if x.Text == "0" || x.Text == "1" {
			return x.Text == "1", nil
		}
	case pyjson.Float:
		if x == 0 || x == 1 {
			return x == 1, nil
		}
	case string:
		switch strings.ToLower(x) {
		case "true", "yes", "on", "t", "y", "1":
			return true, nil
		case "false", "no", "off", "f", "n", "0":
			return false, nil
		}
	}
	return false, invalidf("%s: a bool is required, not %s", k, kindOf(v))
}

var (
	reIntStr    = lazyre.New(`^[+-]?[0-9](?:_?[0-9])*$`)
	reIntFloat  = lazyre.New(`^([+-]?[0-9]+)\.0+$`)
	reFloatStr  = lazyre.New(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	reNumberish = lazyre.New(`^[0-9+\-_.eE \t\n\r\f\v]*$`)
)

// intText is an integer's decimal text as Python prints int(x).
func intText(digits string) (pyjson.Int, bool) {
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return pyjson.Int{}, false
	}
	return pyjson.Int{Text: n.String()}, true
}

// laxInt is pydantic's lax int: an int, a bool, a float with no fraction,
// or a string of digits (spaces around and underscores between allowed,
// or a zero fraction).
func laxInt(v any, k string) (pyjson.Int, error) {
	switch x := v.(type) {
	case pyjson.Int:
		return x, nil
	case bool:
		if x {
			return pyjson.Int{Text: "1"}, nil
		}
		return pyjson.Int{Text: "0"}, nil
	case pyjson.Float:
		f := float64(x)
		if math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) {
			return pyjson.Int{}, invalidf("%s: an integer is required, not %s", k, pyjson.FloatRepr(f))
		}
		if math.Abs(f) > 1<<53 {
			return pyjson.Int{}, ErrUnsure
		}
		return pyjson.Int{Text: strconv.FormatInt(int64(f), 10)}, nil
	case string:
		t := strings.TrimSpace(x)
		if reIntStr().MatchString(t) {
			if n, ok := intText(strings.ReplaceAll(t, "_", "")); ok {
				return n, nil
			}
		}
		if m := reIntFloat().FindStringSubmatch(t); m != nil {
			if n, ok := intText(m[1]); ok {
				return n, nil
			}
		}
		if reNumberish().MatchString(x) {
			return pyjson.Int{}, ErrUnsure
		}
	}
	return pyjson.Int{}, invalidf("%s: an integer is required, not %s", k, kindOf(v))
}

func integer(o *pyjson.Object, k string, def string) (pyjson.Int, error) {
	v, present := o.Get(k)
	if !present {
		return pyjson.Int{Text: def}, nil
	}
	return laxInt(v, k)
}

// laxFloat is pydantic's lax float: an int or float, a bool, or a string
// in the simplest decimal form (others are unsure).
func laxFloat(v any, k string) (any, error) {
	f, err := laxFloat64(v, k)
	if err != nil {
		return nil, err
	}
	return pyjson.Raw(FloatJSON(f)), nil
}

func laxFloat64(v any, k string) (float64, error) {
	switch x := v.(type) {
	case pyjson.Int:
		f, err := strconv.ParseFloat(x.Text, 64)
		if math.IsInf(f, 0) {
			// An int too large for a float reads as an infinity.
			return 0, nonFinite(k)
		}
		if err != nil {
			return 0, ErrUnsure
		}
		return f, nil
	case pyjson.Float:
		f := float64(x)
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, nonFinite(k)
		}
		return f, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		if f, ok := pmodel.FloatFromString(x); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			return 0, nonFinite(k)
		}
		t := strings.TrimSpace(x)
		if reFloatStr().MatchString(t) {
			f, err := strconv.ParseFloat(t, 64)
			if err == nil && !math.IsInf(f, 0) {
				return f, nil
			}
		}
		if reNumberish().MatchString(x) || strings.ContainsAny(strings.ToLower(x), "nif") {
			return 0, ErrUnsure
		}
	}
	return 0, invalidf("%s: a number is required, not %s", k, kindOf(v))
}

// FloatJSON is pydantic's JSON text for a float.
func FloatJSON(f float64) string {
	s := pyjson.FloatRepr(f)
	if mant, ok := strings.CutSuffix(s, "e-05"); ok {
		// Python goes to scientific notation below 1e-4, pydantic below
		// 1e-5: 2.5e-05 is written 0.000025.
		sign := ""
		if strings.HasPrefix(mant, "-") {
			sign, mant = "-", mant[1:]
		}
		return sign + "0.0000" + strings.Replace(mant, ".", "", 1)
	}
	if i := strings.IndexAny(s, "e"); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		sign := ""
		if exp[0] == '+' || exp[0] == '-' {
			sign, exp = exp[:1], exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		s = mant + "e" + sign + exp
	}
	return s
}

// floatTuple reads a list of exactly n numbers.
func floatTuple(v any, n int, k string) ([]any, error) {
	l, isList := v.([]any)
	if !isList {
		return nil, invalidf("%s: a list of %d numbers is required, not %s", k, n, kindOf(v))
	}
	if len(l) != n {
		return nil, invalidf("%s: a list of %d numbers is required, not %d", k, n, len(l))
	}
	out := make([]any, n)
	for i, e := range l {
		f, err := laxFloat(e, k)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// bounds reads an axis-aligned box: two lists of three numbers.
func bounds(v any, k string) ([]any, error) {
	l, isList := v.([]any)
	if !isList || len(l) != 2 {
		return nil, invalidf("%s: two lists of three numbers are required", k)
	}
	out := make([]any, 2)
	for i, e := range l {
		t, err := floatTuple(e, 3, k)
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

func optFloat(p *pyjson.Object, k string) (any, error) {
	v, present := p.Get(k)
	if !present || v == nil {
		return nil, nil
	}
	return laxFloat(v, k)
}

func strList(o *pyjson.Object, k string) ([]any, error) {
	v, present := o.Get(k)
	if !present {
		return []any{}, nil
	}
	l, isList := v.([]any)
	if !isList {
		return nil, invalidf("%s: a list is required, not %s", k, kindOf(v))
	}
	out := make([]any, 0, len(l))
	for _, e := range l {
		s, err := str(e, k+"[]")
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func literal(o *pyjson.Object, k, def string, allowed ...string) (string, error) {
	v, present := o.Get(k)
	if !present {
		return def, nil
	}
	s, err := str(v, k)
	if err != nil {
		return "", err
	}
	for _, a := range allowed {
		if s == a {
			return s, nil
		}
	}
	return "", invalidf("%s: %q is not one of %v", k, s, allowed)
}

func object(v any, k string) (*pyjson.Object, error) {
	o, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, invalidf("%s: an object is required, not %s", k, kindOf(v))
	}
	return o, nil
}

func permission(v any) (*pyjson.Object, error) {
	p, err := object(v, "permissions")
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	add := func(k string, val any, err error) error {
		if err != nil {
			return err
		}
		out.Set(k, val)
		return nil
	}
	lists := func(k string) error {
		l, err := strList(p, k)
		return add(k, l, err)
	}
	bools := func(k string) error {
		b, err := boolean(p, k, false)
		return add(k, b, err)
	}
	ints := func(k, def string) error {
		i, err := integer(p, k, def)
		return add(k, i, err)
	}
	steps := []func() error{
		func() error { return lists("file_read") },
		func() error { return lists("file_write") },
		func() error { return bools("network") },
		func() error { return lists("network_hosts") },
		func() error { return bools("shell") },
		func() error { return lists("shell_allowlist") },
		func() error { return bools("shell_allow_decomposition") },
		func() error { return lists("mcp_allowlist") },
		func() error { return lists("custom_step_allowlist") },
		func() error { return ints("max_execution_time_s", "30") },
		func() error { return ints("max_output_size_mb", "10") },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return nil, err
		}
	}
	// The robotics bounds, all floats.
	var wb any
	if x, present := p.Get("workspace_bounds"); present && x != nil {
		if wb, err = bounds(x, "workspace_bounds"); err != nil {
			return nil, err
		}
	}
	out.Set("workspace_bounds", wb)
	obstacles := []any{}
	if x, present := p.Get("obstacles"); present {
		l, isList := x.([]any)
		if !isList {
			return nil, invalidf("obstacles: a list is required, not %s", kindOf(x))
		}
		for _, e := range l {
			b, err := bounds(e, "obstacles[]")
			if err != nil {
				return nil, err
			}
			obstacles = append(obstacles, b)
		}
	}
	out.Set("obstacles", obstacles)
	vl, err := optFloat(p, "velocity_limit")
	if err != nil {
		return nil, err
	}
	out.Set("velocity_limit", vl)
	joints := pyjson.NewObject()
	if x, present := p.Get("joint_limits"); present {
		o, isObj := x.(*pyjson.Object)
		if !isObj {
			return nil, invalidf("joint_limits: an object is required, not %s", kindOf(x))
		}
		for _, name := range o.Keys() {
			t, err := floatTuple(o.Value(name), 2, "joint_limits."+name)
			if err != nil {
				return nil, err
			}
			joints.Set(name, t)
		}
	}
	out.Set("joint_limits", joints)
	tl, err := optFloat(p, "torque_limit")
	if err != nil {
		return nil, err
	}
	out.Set("torque_limit", tl)
	return out, nil
}

func fallback(in *pyjson.Object) (*pyjson.Object, error) {
	out := pyjson.NewObject().
		Set("strategy", "tier2_recompute").
		Set("model", DefaultFallbackModel).
		Set("include_refinement", true)
	v, present := in.Get("fallback")
	if !present {
		return out, nil
	}
	f, err := object(v, "fallback")
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"strategy", "model"} {
		if x, present := f.Get(k); present {
			s, err := str(x, "fallback."+k)
			if err != nil {
				return nil, err
			}
			out.Set(k, s)
		}
	}
	b, err := boolean(f, "include_refinement", true)
	if err != nil {
		return nil, err
	}
	out.Set("include_refinement", b)
	return out, nil
}

// modelList reads invariants or postconditions. Their fields are typed;
// expr is any JSON value, dumped as pydantic dumps it, which this package
// models for everything but floats.
func modelList(in *pyjson.Object, k string) ([]any, error) {
	v, present := in.Get(k)
	if !present {
		return []any{}, nil
	}
	l, isList := v.([]any)
	if !isList {
		return nil, ErrUnsure
	}
	out := make([]any, 0, len(l))
	for _, e := range l {
		o, err := object(e, k+"[]")
		if err != nil {
			return nil, err
		}
		var m *pyjson.Object
		if k == "invariants" {
			m, err = invariant(o)
		} else {
			m, err = postcondition(o)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func requiredStr(o *pyjson.Object, k, where string) (string, error) {
	v, present := o.Get(k)
	if !present {
		return "", invalidf("%s.%s: field required", where, k)
	}
	return str(v, where+"."+k)
}

func invariant(o *pyjson.Object) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	t, err := requiredStr(o, "type", "invariants[]")
	if err != nil {
		return nil, err
	}
	out.Set("type", t)
	for _, k := range []string{"target", "scope"} {
		s, err := optStr(o, k, true)
		if err != nil {
			return nil, err
		}
		out.Set(k, s)
	}
	d, err := requiredStr(o, "description", "invariants[]")
	if err != nil {
		return nil, err
	}
	out.Set("description", d)
	e, err := expr(o)
	if err != nil {
		return nil, err
	}
	out.Set("expr", e)
	b, err := boolean(o, "enforce", true)
	if err != nil {
		return nil, err
	}
	out.Set("enforce", b)
	return out, nil
}

func optInt(o *pyjson.Object, k string) (any, error) {
	v, present := o.Get(k)
	if !present || v == nil {
		return nil, nil
	}
	return integer(o, k, "")
}

func postcondition(o *pyjson.Object) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	t, err := requiredStr(o, "type", "postconditions[]")
	if err != nil {
		return nil, err
	}
	out.Set("type", t)
	p, err := optStr(o, "path", true)
	if err != nil {
		return nil, err
	}
	out.Set("path", p)
	for _, k := range []string{"expected", "min", "max"} {
		i, err := optInt(o, k)
		if err != nil {
			return nil, err
		}
		out.Set(k, i)
	}
	d, err := optStr(o, "description", true)
	if err != nil {
		return nil, err
	}
	out.Set("description", d)
	e, err := expr(o)
	if err != nil {
		return nil, err
	}
	out.Set("expr", e)
	b, err := boolean(o, "enforce", true)
	if err != nil {
		return nil, err
	}
	out.Set("enforce", b)
	return out, nil
}

// expr is an invariant's or postcondition's expr: any JSON value, dumped
// as pydantic dumps it, with its floats in pydantic's form.
func expr(o *pyjson.Object) (any, error) {
	v, present := o.Get("expr")
	if !present {
		return nil, nil
	}
	return pydanticFloats(v)
}

func pydanticFloats(v any) (any, error) {
	switch x := v.(type) {
	case pyjson.Float:
		f := float64(x)
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, nonFinite("expr")
		}
		return pyjson.Raw(FloatJSON(f)), nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			c, err := pydanticFloats(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			c, err := pydanticFloats(x.Value(k))
			if err != nil {
				return nil, err
			}
			out.Set(k, c)
		}
		return out, nil
	}
	return v, nil
}
