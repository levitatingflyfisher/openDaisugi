// Package aliases is the oracle's alias registry (opendaisugi/aliases.py)
// and its shipped system aliases (system_aliases.py): named,
// parameterized predicate expressions in three tiers, registered with a
// static path check and a Z3 vacuity check, and resolved by one
// structural substitution.
//
// No command of the oracle builds a registry, so no command here does
// either: at every command an alias is unresolved (verify words it). The
// registry is the library part integrations/hermes.py calls, measured by
// cmd/alias-probe against the oracle (clients/alias_compare.py).
package aliases

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// Alias is aliases.Alias. Expr is the body as plain data.
type Alias struct {
	Name        string
	Params      []string
	Expr        any
	Tier        string
	Description string
}

// Dump is Alias.model_dump(mode="json").
func (a Alias) Dump() *pyjson.Object {
	params := make([]any, len(a.Params))
	for i, p := range a.Params {
		params[i] = p
	}
	return pyjson.NewObject().Set("name", a.Name).Set("params", params).Set("expr", a.Expr).
		Set("tier", a.Tier).Set("description", a.Description)
}

// Error is an exception the oracle raises: its class name and str().
type Error struct{ Class, Msg string }

func (e *Error) Error() string { return e.Msg }

func raise(class, format string, a ...any) *Error {
	return &Error{Class: class, Msg: fmt.Sprintf(format, a...)}
}

var tierOrder = map[string]int{"envelope": 0, "household": 1, "system": 2}

// pathOps are the ops that name a plan path.
var pathOps = map[string]bool{
	"equals": true, "not_equals": true, "in_set": true, "not_in_set": true, "matches": true,
	"not_matches": true, "numeric_range": true, "exists": true, "is_empty": true, "depends_on": true,
	"before": true, "alias": true, "llm_check": true,
}

// Registry is aliases.AliasRegistry.
type Registry struct {
	entries map[string][]Alias
}

// New is AliasRegistry().
func New() *Registry { return &Registry{entries: map[string][]Alias{}} }

// Contains is `name in registry`.
func (r *Registry) Contains(name string) bool {
	_, ok := r.entries[name]
	return ok
}

// Register adds an alias, or returns the oracle's exception and adds
// nothing. A system alias's name is taken for good, in either order.
func (r *Registry) Register(a Alias) error {
	taken := r.entries[a.Name]
	if len(taken) > 0 {
		system := a.Tier == "system"
		for _, t := range taken {
			system = system || t.Tier == "system"
		}
		if system {
			return raise("ValueError", "alias '%s' is a system alias name; a %s alias cannot share it, "+
				"since it would redefine what the system word means", a.Name, a.Tier)
		}
	}
	refs, err := referencesAPath(a.Expr)
	if err != nil {
		return err
	}
	if !refs {
		return raise("ValueError", "alias '%s' has no plan-path reference (looks vacuous); static check "+
			"requires at least one Equals/NotEquals/Matches/... on a path", a.Name)
	}
	if err := vacuity(a); err != nil {
		return err
	}
	r.entries[a.Name] = append(r.entries[a.Name], a)
	return nil
}

// referencesAPath is aliases._references_a_path. Iterating a value that
// is no list raises as Python does: a str or a dict iterates to strings,
// anything else is not iterable.
func referencesAPath(expr any) (bool, *Error) {
	d, ok := expr.(*pyjson.Object)
	if !ok {
		return false, nil
	}
	op, _ := d.Value("op").(string)
	if pathOps[op] {
		return true, nil
	}
	switch op {
	case "and", "or":
		children, present := d.Get("children")
		if !present {
			return false, nil
		}
		switch c := children.(type) {
		case []any:
			for _, x := range c {
				if ok, err := referencesAPath(x); err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		case string, *pyjson.Object:
			return false, nil
		}
		return false, raise("TypeError", "'%s' object is not iterable", pmodel.TypeName(children))
	case "not":
		return referencesAPath(d.Value("child"))
	case "implies":
		if ok, err := referencesAPath(d.Value("a")); err != nil || ok {
			return ok, err
		}
		return referencesAPath(d.Value("b"))
	case "forall_steps", "exists_step", "forall_outputs", "forall_writes":
		return referencesAPath(d.Value("pred"))
	}
	return false, nil
}

// namesAnAlias is aliases._names_an_alias: an alias reference at any
// depth.
func namesAnAlias(expr any) bool {
	switch x := expr.(type) {
	case *pyjson.Object:
		if x.Value("op") == "alias" {
			return true
		}
		for _, k := range x.Keys() {
			if namesAnAlias(x.Value(k)) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if namesAnAlias(v) {
				return true
			}
		}
	}
	return false
}

// placeholders finds `$name` for the given names, longest name first, so
// `$p` never takes the front of `$p_name`. Each match is the index of the
// `$` and the name.
type placeholders []string

func newPlaceholders(names []string) placeholders {
	out := append([]string{}, names...)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// sub replaces each placeholder in s with repl(name), scanning once:
// replaced text is never scanned again.
func (p placeholders) sub(s string, repl func(name string) string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' {
			matched := false
			for _, n := range p {
				if strings.HasPrefix(s[i+1:], n) {
					b.WriteString(repl(n))
					i += 1 + len(n)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func (p placeholders) search(s string) bool {
	found := false
	p.sub(s, func(n string) string { found = true; return "" })
	return found
}

func hasPlaceholder(expr any, params []string) bool {
	if len(params) == 0 {
		return false
	}
	p := newPlaceholders(params)
	var walk func(x any) bool
	walk = func(x any) bool {
		switch v := x.(type) {
		case string:
			return p.search(v)
		case *pyjson.Object:
			for _, k := range v.Keys() {
				if walk(v.Value(k)) {
					return true
				}
			}
		case []any:
			for _, i := range v {
				if walk(i) {
					return true
				}
			}
		}
		return false
	}
	return walk(expr)
}

// parse is parse_expression on plain data: the validated dump, or the
// ValidationError.
func parse(v any) (any, *Error) {
	out, verr := pmodel.Validate(pmodel.ExpressionTitle, pmodel.Expression, v, pmodel.Python)
	if verr != nil {
		return nil, &Error{Class: "ValidationError", Msg: verr.String()}
	}
	return out, nil
}

// vacuity is AliasRegistry._vacuity: tautologies and contradictions are
// refused, a body that names another alias, or that parses only once a
// typed placeholder is bound, is deferred, and any other error refuses.
func vacuity(a Alias) error {
	if namesAnAlias(a.Expr) {
		return nil
	}
	if _, isDict := a.Expr.(*pyjson.Object); !isDict {
		// The path check refused every body that is no dict.
		return nil
	}
	dumped, perr := parse(a.Expr)
	if perr != nil {
		if len(a.Params) > 0 && hasPlaceholder(a.Expr, a.Params) {
			return nil
		}
		return raise("ValueError", "alias '%s' body is not a valid predicate: %s", a.Name, perr.Msg)
	}
	expr, err := verify.ParseExpression(json.RawMessage(pyjson.Dumps(dumped, true)))
	if err != nil {
		return raise("ValueError", "alias '%s' could not be checked for vacuity, so it is not registered: %v", a.Name, err)
	}
	verdict, err := verify.Vacuity(expr, 500)
	if err != nil {
		return raise("ValueError", "alias '%s' could not be checked for vacuity, so it is not registered: %v", a.Name, err)
	}
	if verdict == verify.Tautology || verdict == verify.Contradiction {
		return raise("VacuousAliasError", "alias '%s' is %s (constrains nothing / never satisfiable); "+
			"the predicate must be non-trivial to be registered", a.Name, verdict)
	}
	return nil
}

// Lookup is AliasRegistry.lookup: the highest-precedence tier, the first
// registered among equals.
func (r *Registry) Lookup(name string) (Alias, error) {
	entries, ok := r.entries[name]
	if !ok {
		return Alias{}, &Error{Class: "UnknownAliasError", Msg: pmodel.Repr(name)}
	}
	best := entries[0]
	for _, a := range entries[1:] {
		if tierOrder[a.Tier] < tierOrder[best.Tier] {
			best = a
		}
	}
	return best, nil
}

// Resolve is AliasRegistry.resolve over a validated expression dump.
func (r *Registry) Resolve(expr any) (any, error) {
	return r.resolve(expr, map[string]bool{})
}

func (r *Registry) resolve(expr any, seen map[string]bool) (any, error) {
	o, ok := expr.(*pyjson.Object)
	if !ok {
		return expr, nil
	}
	op, _ := o.Value("op").(string)
	copyWith := func(key string, v any) *pyjson.Object {
		out := pyjson.NewObject()
		for _, k := range o.Keys() {
			out.Set(k, o.Value(k))
		}
		return out.Set(key, v)
	}
	switch op {
	case "alias":
		name, _ := o.Value("name").(string)
		if seen[name] {
			return nil, raise("AliasCycleError", "alias cycle detected: %s -> ... -> %s", name, name)
		}
		a, err := r.Lookup(name)
		if err != nil {
			return nil, err
		}
		args, _ := o.Value("args").(*pyjson.Object)
		if args == nil {
			args = pyjson.NewObject()
		}
		var missing []any
		for _, p := range a.Params {
			if _, present := args.Get(p); !present {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			return nil, raise("ValueError", "alias '%s' missing required args: %s", name, pmodel.Repr(missing))
		}
		sub := substitute(a.Expr, args)
		if _, isDict := sub.(*pyjson.Object); isDict {
			parsed, perr := parse(sub)
			if perr != nil {
				return nil, perr
			}
			sub = parsed
		}
		next := map[string]bool{name: true}
		for k := range seen {
			next[k] = true
		}
		return r.resolve(sub, next)
	case "and", "or":
		children, _ := o.Value("children").([]any)
		out := make([]any, len(children))
		for i, c := range children {
			v, err := r.resolve(c, seen)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return copyWith("children", out), nil
	case "not":
		v, err := r.resolve(o.Value("child"), seen)
		if err != nil {
			return nil, err
		}
		return copyWith("child", v), nil
	case "implies":
		a, err := r.resolve(o.Value("a"), seen)
		if err != nil {
			return nil, err
		}
		b, err := r.resolve(o.Value("b"), seen)
		if err != nil {
			return nil, err
		}
		return copyWith("a", a).Set("b", b), nil
	case "forall_steps", "exists_step", "forall_outputs", "forall_writes":
		v, err := r.resolve(o.Value("pred"), seen)
		if err != nil {
			return nil, err
		}
		return copyWith("pred", v), nil
	}
	return expr, nil
}

// pyStr is str() of a decoded JSON value.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pmodel.Repr(v)
}

// reSpecial is the set re.escape escapes.
const reSpecial = "()[]{}?*+-|^$\\.&~# \t\n\r\v\f"

// reEscape is re.escape.
func reEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && strings.ContainsRune(reSpecial, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// substitute is aliases._substitute_params: one pass over the structure.
// A string that is exactly `$name` becomes the argument itself (outside a
// regex field); a `$name` inside a string becomes its text, and inside a
// regex field its escaped text.
func substitute(expr any, args *pyjson.Object) any {
	p := newPlaceholders(args.Keys())
	var walk func(x any, inRegex bool) any
	walk = func(x any, inRegex bool) any {
		switch v := x.(type) {
		case string:
			if len(p) == 0 {
				return v
			}
			if strings.HasPrefix(v, "$") && !inRegex {
				if val, present := args.Get(v[1:]); present {
					return val
				}
			}
			if inRegex {
				return p.sub(v, func(n string) string { return reEscape(pyStr(args.Value(n))) })
			}
			return p.sub(v, func(n string) string { return pyStr(args.Value(n)) })
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				out[i] = walk(item, inRegex)
			}
			return out
		case *pyjson.Object:
			out := pyjson.NewObject()
			for _, k := range v.Keys() {
				out.Set(k, walk(v.Value(k), k == "regex"))
			}
			return out
		}
		return x
	}
	return walk(expr, false)
}

// ParseAndResolve is registry.resolve(parse_expression(expr)): the
// resolved expression's dump, or the oracle's exception.
func ParseAndResolve(r *Registry, expr any) (any, error) {
	parsed, perr := parse(expr)
	if perr != nil {
		return nil, perr
	}
	return r.Resolve(parsed)
}
