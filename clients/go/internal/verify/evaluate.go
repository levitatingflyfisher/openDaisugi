package verify

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// pyUEscape matches Python `re`'s \uXXXX Unicode escape (4 hex digits) —
// valid in Python's regex dialect, NOT valid in Go's RE2
// (regexp.Compile errors "invalid escape sequence: `\u`" on it verbatim).
// The corpus carries at least one real predicate authored with — (an
// em dash) this way. translatePyRegex rewrites it to RE2's braced hex
// escape (\x{XXXX}) before compiling — a regex-DIALECT bridge, not a
// shell-grammar adjudication, so it lives here rather than in
// ADJUDICATIONS.md; still worth a comment since it's silent otherwise.
var pyUEscape = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

func translatePyRegex(pattern string) string {
	return pyUEscape.ReplaceAllString(pattern, `\x{$1}`)
}

// resolvePath ports predicate_z3._resolve_path for the dict-only case (our
// scopes are always JSON-decoded maps, never Python objects with
// attributes — the getattr fallback branch never applies here).
func resolvePath(scope map[string]interface{}, path string) (interface{}, bool) {
	var cur interface{} = scope
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, present := m[part]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func jsonLen(v interface{}) (int, bool) {
	switch t := v.(type) {
	case string:
		return len([]rune(t)), true
	case []interface{}:
		return len(t), true
	case map[string]interface{}:
		return len(t), true
	}
	return 0, false
}

func asFloat(v interface{}) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// evalScalar ports predicate_z3._eval_scalar: the per-step ground-truth
// evaluator. LLMCheck and AliasRef are not evaluable here — matches the
// oracle raising ValueError, surfaced as an error the caller turns into a
// "predicate ... evaluation error" violation (verify._check_predicate_item).
func evalScalar(expr Expression, scope map[string]interface{}) (bool, error) {
	switch e := expr.(type) {
	case Equals:
		v, _ := resolvePath(scope, e.Path)
		return reflect.DeepEqual(v, e.Value), nil
	case NotEquals:
		v, present := resolvePath(scope, e.Path)
		return present && !reflect.DeepEqual(v, e.Value), nil
	case InSet:
		v, present := resolvePath(scope, e.Path)
		if !present {
			return false, nil
		}
		for _, want := range e.Values {
			if reflect.DeepEqual(v, want) {
				return true, nil
			}
		}
		return false, nil
	case NotInSet:
		v, present := resolvePath(scope, e.Path)
		if !present {
			return false, nil
		}
		for _, want := range e.Values {
			if reflect.DeepEqual(v, want) {
				return false, nil
			}
		}
		return true, nil
	case Matches:
		v, present := resolvePath(scope, e.Path)
		s, ok := v.(string)
		if !present || !ok {
			return false, nil
		}
		re, err := regexp.Compile(translatePyRegex(e.Regex))
		if err != nil {
			return false, fmt.Errorf("regex %q: %w", e.Regex, err)
		}
		return re.MatchString(s), nil
	case NotMatches:
		v, present := resolvePath(scope, e.Path)
		s, ok := v.(string)
		if !present || !ok {
			return true, nil
		}
		re, err := regexp.Compile(translatePyRegex(e.Regex))
		if err != nil {
			return false, fmt.Errorf("regex %q: %w", e.Regex, err)
		}
		return !re.MatchString(s), nil
	case NumericRange:
		v, present := resolvePath(scope, e.Path)
		f, ok := asFloat(v)
		if !present || !ok {
			return false, nil
		}
		return e.Min <= f && f <= e.Max, nil
	case LengthRange:
		v, present := resolvePath(scope, e.Path)
		if !present {
			return false, nil
		}
		n, ok := jsonLen(v)
		if !ok {
			return false, nil
		}
		if n < e.Min {
			return false, nil
		}
		if e.Max != nil && n > *e.Max {
			return false, nil
		}
		return true, nil
	case Exists:
		_, present := resolvePath(scope, e.Path)
		return present, nil
	case IsEmpty:
		v, present := resolvePath(scope, e.Path)
		if !present || v == nil {
			return true, nil
		}
		if n, ok := jsonLen(v); ok {
			return n == 0, nil
		}
		return false, nil
	case And:
		for _, c := range e.Children {
			ok, err := evalScalar(c, scope)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case Or:
		for _, c := range e.Children {
			ok, err := evalScalar(c, scope)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case Not:
		ok, err := evalScalar(e.Child, scope)
		return !ok, err
	case Implies:
		a, err := evalScalar(e.A, scope)
		if err != nil {
			return false, err
		}
		if !a {
			return true, nil
		}
		return evalScalar(e.B, scope)
	case LLMCheck:
		return false, fmt.Errorf("LLMCheck must be evaluated via evaluate_llm_check, not _eval_scalar")
	case AliasRef:
		return false, fmt.Errorf("unresolved alias reference %q; resolve aliases before evaluation", e.Name)
	default:
		return false, fmt.Errorf("unknown predicate op: %v", expr.Op())
	}
}

// EvaluatePredicate ports predicate_z3.evaluate_predicate: the plan-level
// ground-truth evaluator used by verify._check_predicate_item (the Python
// "fast path" — NOT the Z3 symbolic compiler, which this client only uses
// for vacuity classification and subsumption).
func EvaluatePredicate(expr Expression, plan ActionPlan, env Envelope) (bool, error) {
	stepDicts := make([]map[string]interface{}, len(plan.Steps))
	for i, s := range plan.Steps {
		stepDicts[i] = s.Raw
	}
	return evalPredicateGo(expr, plan, env, stepDicts)
}

func evalPredicateGo(e Expression, plan ActionPlan, env Envelope, stepDicts []map[string]interface{}) (bool, error) {
	switch expr := e.(type) {
	case ForallSteps:
		for _, s := range stepDicts {
			ok, err := evalScalar(expr.Pred, s)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case ExistsStep:
		for _, s := range stepDicts {
			ok, err := evalScalar(expr.Pred, s)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case ForallOutputs:
		var outputs []map[string]interface{}
		for _, s := range stepDicts {
			meta, _ := s["metadata"].(map[string]interface{})
			if meta == nil {
				continue
			}
			if out, present := meta["output"]; present && out != nil {
				outputs = append(outputs, map[string]interface{}{"output": out})
			}
		}
		for _, o := range outputs {
			ok, err := evalScalar(expr.Pred, o)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case DependsOn:
		for _, s := range stepDicts {
			if id, _ := s["id"].(string); id == expr.StepIDA {
				deps, _ := s["depends_on"].([]interface{})
				for _, d := range deps {
					if ds, ok := d.(string); ok && ds == expr.StepIDB {
						return true, nil
					}
				}
				return false, nil
			}
		}
		return false, nil
	case Before:
		var ids []string
		for _, s := range stepDicts {
			id, _ := s["id"].(string)
			ids = append(ids, id)
		}
		ia, ib := indexOf(ids, expr.StepIDA), indexOf(ids, expr.StepIDB)
		if ia < 0 || ib < 0 {
			return false, nil
		}
		return ia < ib, nil
	case LLMCheck:
		if env.Stakes == "physical" {
			return false, fmt.Errorf("llm_check blocked for physical stakes — use sound primitives only")
		}
		return false, fmt.Errorf("llm_check is not supported by this client (no corpus case exercises it)")
	case And:
		for _, c := range expr.Children {
			ok, err := evalPredicateGo(c, plan, env, stepDicts)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case Or:
		for _, c := range expr.Children {
			ok, err := evalPredicateGo(c, plan, env, stepDicts)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case Not:
		ok, err := evalPredicateGo(expr.Child, plan, env, stepDicts)
		return !ok, err
	case Implies:
		a, err := evalPredicateGo(expr.A, plan, env, stepDicts)
		if err != nil {
			return false, err
		}
		if !a {
			return true, nil
		}
		return evalPredicateGo(expr.B, plan, env, stepDicts)
	default:
		synthetic := map[string]interface{}{"steps": toAnySlice(stepDicts)}
		return evalScalar(e, synthetic)
	}
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func toAnySlice(ms []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}
