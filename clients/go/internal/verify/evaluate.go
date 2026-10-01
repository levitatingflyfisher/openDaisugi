package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"daisugi-verify/internal/pyjson"
)

// translatePyRegex bridges the escapes where Python's `re` and Go's RE2
// differ, in one pass over the pattern so an escaped backslash is never
// read as the start of another escape:
//
//   - \uXXXX (4 hex digits) is valid in Python, NOT in RE2 (regexp.Compile
//     errors "invalid escape sequence: `\u`"). The corpus carries at least
//     one real predicate authored with an em dash this way. It becomes
//     RE2's braced hex escape \x{XXXX}.
//   - \Z is the end of the string in Python; RE2 spells it \z (and has no
//     \Z). The dialect's glob regexes end in it.
//
// A regex-DIALECT bridge, not a shell-grammar adjudication, so it lives
// here rather than in ADJUDICATIONS.md.
func translatePyRegex(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '\\' || i+1 >= len(pattern) {
			b.WriteByte(c)
			continue
		}
		next := pattern[i+1]
		switch {
		case next == 'Z':
			b.WriteString(`\z`)
		case next == 'u' && i+6 <= len(pattern) && isHex4(pattern[i+2:i+6]):
			b.WriteString(`\x{` + pattern[i+2:i+6] + `}`)
			i += 4
		default:
			b.WriteByte(c)
			b.WriteByte(next)
		}
		i++
	}
	return b.String()
}

func isHex4(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) == 4
}

// compilePyRegex compiles a Python pattern on RE2 through translatePyRegex.
func compilePyRegex(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(translatePyRegex(pattern))
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
	case pyTuple:
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
	case ForallWrites:
		// Only a step record has write paths (predicate_z3._scope_writes).
		if _, isStep := scope["type"]; !isStep {
			return false, fmt.Errorf("forall_writes must stand inside forall_steps or exists_step")
		}
		writes, ok := StepWritePaths(scope, e.Base)
		if !ok {
			return false, nil
		}
		for _, p := range writes {
			holds, err := evalScalar(e.Pred, map[string]interface{}{"path": p})
			if err != nil || !holds {
				return false, err
			}
		}
		return true, nil
	case Equals:
		v, _ := resolvePath(scope, e.Path)
		return pyEqual(v, e.Value), nil
	case NotEquals:
		v, present := resolvePath(scope, e.Path)
		return present && !pyEqual(v, e.Value), nil
	case InSet:
		v, present := resolvePath(scope, e.Path)
		if !present {
			return false, nil
		}
		for _, want := range e.Values {
			if pyEqual(v, want) {
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
			if pyEqual(v, want) {
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
		re, err := compilePyRegex(e.Regex)
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
		re, err := compilePyRegex(e.Regex)
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
		return false, fmt.Errorf("unresolved alias reference '%s'; resolve aliases before evaluation", e.Name)
	default:
		return false, fmt.Errorf("unknown predicate op: %v", expr.Op())
	}
}

// LLMVerdict is llm_check.LLMCheckResult, and Unported names a call the
// binary does not make the oracle's way ("" when it made it).
type LLMVerdict struct {
	Satisfied bool
	Reason    string
	Errored   bool
	Unported  string
}

// LLM is llm_check.run_llm_check(rule, payload), payload being
// json.dumps(payload, default=str). A command that verifies sets it once,
// before its first verify, as the oracle's evaluator calls the one module
// function. Nil leaves every llm_check an evaluation error.
var LLM func(rule, payload string) LLMVerdict

// WordedEvalError reports an evaluation error the binary words as the
// oracle does at every caller: an llm_check that failed or was blocked,
// and an alias left unresolved. A caller that words no other evaluation
// error refuses the rest.
func WordedEvalError(err error) bool {
	m := err.Error()
	if strings.HasSuffix(m, " is not in this binary yet") {
		return false
	}
	return strings.HasPrefix(m, "error: llm_check call failed: ") ||
		strings.HasPrefix(m, "llm_check blocked for physical stakes") ||
		strings.HasPrefix(m, "unresolved alias reference '")
}

// llmPayload is json.dumps({"task": plan.task, "steps": step_dicts}),
// each step as it was read. False when a step was not read from JSON.
func llmPayload(plan ActionPlan) (string, bool) {
	steps := make([]any, len(plan.Steps))
	for i, s := range plan.Steps {
		if len(s.JSON) == 0 {
			return "", false
		}
		v, err := pyjson.LoadsPy(string(s.JSON), 1<<20)
		if err != nil {
			return "", false
		}
		steps[i] = v
	}
	return pyjson.Dumps(pyjson.NewObject().Set("task", plan.Task).Set("steps", steps), true), true
}

// EvaluatePredicate ports predicate_z3.evaluate_predicate: the plan-level
// ground-truth evaluator used by verify._check_predicate_item (the Python
// "fast path" — NOT the Z3 symbolic compiler, which this client only uses
// for vacuity classification and subsumption).
func EvaluatePredicate(expr Expression, plan ActionPlan, env Envelope) (bool, error) {
	stepDicts := make([]map[string]interface{}, len(plan.Steps))
	for i, s := range plan.Steps {
		stepDicts[i] = dumpedStep(s)
	}
	return evalPredicateGo(expr, plan, env, stepDicts)
}

// pyEqual is Python's == over decoded JSON values: True == 1 == 1.0 and
// False == 0, lists and dicts compare item by item, and a pyTuple equals
// only a pyTuple. A number past the float64 range stays a json.Number and
// equals only the same number.
func pyEqual(a, b interface{}) bool {
	if x, ok := pyNumber(a); ok {
		y, ok := pyNumber(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x == y
	case []interface{}:
		y, ok := b.([]interface{})
		return ok && pyEqualItems(x, y)
	case pyTuple:
		y, ok := b.(pyTuple)
		return ok && pyEqualItems(x, y)
	case map[string]interface{}:
		y, ok := b.(map[string]interface{})
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, present := y[k]
			if !present || !pyEqual(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

func pyEqualItems(x, y []interface{}) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !pyEqual(x[i], y[i]) {
			return false
		}
	}
	return true
}

// pyNumber is a bool or a float64 as the number Python compares it as.
func pyNumber(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// pyTuple is a list value that model_dump() keeps as a Python tuple. A
// tuple equals no list, so reflect.DeepEqual against a decoded JSON value
// (always a []interface{}) is false, as `==` is in Python. Its length is
// the list's.
type pyTuple []interface{}

// tupleFields are the step fields each step type declares as a tuple.
var tupleFields = map[string][]string{
	"cartesian_move": {"target_position", "target_orientation"},
	"vla":            {"target_pose"},
}

// dumpedStep is the step dict the oracle's evaluator reads: the step's
// fields, with each tuple-typed field held as a pyTuple. The step's own
// map is not changed, since the robotics stage reads those fields as
// lists.
func dumpedStep(s Step) map[string]interface{} {
	fields := tupleFields[s.Type]
	if len(fields) == 0 {
		return s.Raw
	}
	out := make(map[string]interface{}, len(s.Raw))
	for k, v := range s.Raw {
		out[k] = v
	}
	for _, f := range fields {
		if l, ok := out[f].([]interface{}); ok {
			out[f] = pyTuple(l)
		}
	}
	return out
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
		if LLM == nil {
			return false, fmt.Errorf("llm_check is not supported by this client (no corpus case exercises it)")
		}
		payload, ok := llmPayload(plan)
		if !ok {
			return false, fmt.Errorf("an llm_check over a step this binary did not read is not in this binary yet")
		}
		res := LLM(expr.Rule, payload)
		if res.Unported != "" {
			return false, fmt.Errorf("%s is not in this binary yet", res.Unported)
		}
		// Fail closed: a failed probabilistic check raises, and the caller
		// records a violation.
		if res.Errored {
			return false, errors.New(res.Reason)
		}
		return res.Satisfied, nil
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
