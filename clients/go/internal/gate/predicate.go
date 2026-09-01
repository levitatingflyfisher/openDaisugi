package gate

import (
	"fmt"
	"math/big"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pyre"
	"daisugi-verify/internal/pystr"
)

// This file is the verifier's predicate stage (verify._check_predicate_item)
// for the gate's one-step plan: parse_expression, the vacuity check on Z3
// (vacuity.py, predicate_z3._compile_scalar, regex_to_z3), and the ground
// evaluation (predicate_z3.evaluate_predicate).

var recognizedOpaque = set("end_effector_in_workspace", "joint_limits_respected", "velocity_bounded", "no_obstacle_penetration")
var stage2Postconditions = set("exit_code", "file_exists", "file_size_range")

// expr is a parsed Expression: the validated model, its op first.
type expr = *pyjson.Object

func opOf(e any) string {
	if o, ok := e.(*pyjson.Object); ok {
		if s, ok := o.Value("op").(string); ok {
			return s
		}
	}
	return ""
}

// parseExpression is predicate.parse_expression on a dict.
func parseExpression(raw any) expr {
	v, verr := pmodel.Validate(pmodel.ExpressionTitle, pmodel.Expression, raw, pmodel.Python)
	if verr != nil {
		panic(pystr.NewException("ValidationError", verr.String()))
	}
	return v.(*pyjson.Object)
}

// normalizeExpr is verify._normalize_expr: None, a parsed dict, or the
// raw value itself.
func normalizeExpr(raw any) any {
	if raw == nil {
		return nil
	}
	if _, ok := raw.(*pyjson.Object); ok {
		return parseExpression(raw)
	}
	return raw
}

// stepDump is the model_dump of the gate's one step, s0.
func stepDump(rec *record) *pyjson.Object {
	d := pyjson.NewObject().
		Set("id", "s0").
		Set("depends_on", []any{}).
		Set("metadata", pyjson.NewObject()).
		Set("postcondition", nil).
		Set("preferred_model", nil).
		Set("type", rec.StepType)
	switch rec.StepType {
	case "shell":
		d.Set("command", rec.Command)
	case "file_read":
		d.Set("path", rec.Path)
	case "file_write":
		d.Set("path", rec.Path).Set("content", "")
	case "network":
		d.Set("url", rec.URL).Set("method", "GET").Set("headers", pyjson.NewObject())
	case "mcp":
		args := rec.Arguments
		if args == nil || args.Len() == 0 {
			args = pyjson.NewObject()
		}
		d.Set("server", rec.MCPServer).Set("tool", rec.MCPTool).Set("arguments", args)
	}
	return d
}

// checkPredicates is verify._check_predicate_invariants.
func (r *runner) checkPredicates(rec *record, env *envelope) []violation {
	strict := env.Stakes == "high" || env.Stakes == "physical"
	var vs []violation
	for _, it := range env.Invariants {
		vs = append(vs, r.checkPredicateItem("invariant", it, rec, env, strict)...)
	}
	for _, it := range env.Postconditions {
		vs = append(vs, r.checkPredicateItem("postcondition", it, rec, env, strict)...)
	}
	return vs
}

func (r *runner) checkPredicateItem(label string, it predicateItem, rec *record, env *envelope, strict bool) []violation {
	if !it.Enforce {
		return nil
	}
	e := normalizeExpr(it.Expr)
	if e == nil {
		backing := ""
		if label == "invariant" {
			if it.Type == "end_effector_in_workspace" && env.WorkspaceBounds == nil {
				backing = "workspace_bounds"
			}
			if it.Type == "velocity_bounded" && env.VelocityLimit == nil {
				backing = "velocity_limit"
			}
		}
		if backing != "" {
			return []violation{{Stage: "predicate",
				Message: fmt.Sprintf("invariant '%s' is declared but its backing permission (%s) is absent; the check no-ops and the invariant is unenforced — add the bound or remove the invariant", it.Type, backing),
				Detail:  kv(label, it.Type, "reason", "robotics_invariant_unbacked")}}
		}
		discharged := (label == "invariant" && recognizedOpaque[it.Type]) ||
			(label == "postcondition" && stage2Postconditions[it.Type])
		if strict && !discharged {
			return []violation{{Stage: "predicate",
				Message: fmt.Sprintf("%s '%s' declares a safety property with no verifiable expr; cannot be discharged under strict mode", label, it.Type),
				Detail: kv(label, it.Type, "reason", "opaque_unrecognized",
					"suggested_remediation", "add an `expr` to make it verifiable, or set enforce=False to keep it as documentation")}}
		}
		return nil
	}
	if opOf(e) == "alias" {
		name := e.(*pyjson.Object).Value("name").(string)
		return []violation{{Stage: "predicate",
			Message: fmt.Sprintf("%s '%s' references unresolved alias '%s'; pass an AliasRegistry via aliases= to verify()", label, it.Type, name),
			Detail: kv(label, it.Type, "reason", "unresolved_alias", "alias", name,
				"suggested_remediation", "register the alias in an AliasRegistry and pass aliases= to verify()")}}
	}
	switch r.vacuity(e) {
	case "contradiction":
		return []violation{{Stage: "predicate",
			Message: fmt.Sprintf("%s '%s' can never be satisfied (unsatisfiable); the envelope can never pass — fix the predicate", label, it.Type),
			Detail: kv(label, it.Type, "reason", "contradiction",
				"suggested_remediation", fmt.Sprintf("this %s is unsatisfiable (always false); the envelope can never pass — fix the predicate", label))}}
	case "tautology":
		if strict {
			return []violation{{Stage: "predicate",
				Message: fmt.Sprintf("%s '%s' is a tautology (constrains nothing); tighten the predicate or remove it", label, it.Type),
				Detail: kv(label, it.Type, "reason", "tautology",
					"suggested_remediation", fmt.Sprintf("this %s constrains nothing; tighten the predicate or remove it", label))}}
		}
	}
	var ok bool
	if exc := catch(func() { ok = r.evaluatePredicate(e, stepDump(rec), env) }); exc != nil {
		return []violation{{Stage: "predicate",
			Message: fmt.Sprintf("%s '%s' evaluation error: %s", label, it.Type, exc.Msg),
			Detail:  kv(label, it.Type)}}
	}
	if !ok {
		return []violation{{Stage: "predicate", Message: fmt.Sprintf("%s '%s' violated", label, it.Type),
			Detail: kv(label, it.Type, "description", it.Description)}}
	}
	return nil
}

func valueError(msg string) { panic(pystr.NewException("ValueError", msg)) }

// typeNameOfExpr is getattr(expr, 'op', type(expr).__name__) for an
// expression or a raw value.
func opOrType(e any) string {
	if op := opOf(e); op != "" {
		return op
	}
	return pyTypeName(e)
}

// evaluatePredicate is predicate_z3.evaluate_predicate on a one-step plan.
func (r *runner) evaluatePredicate(e any, step *pyjson.Object, env *envelope) bool {
	steps := []any{step}
	var goE func(e any) bool
	goE = func(e any) bool {
		o, _ := e.(*pyjson.Object)
		switch opOf(e) {
		case "forall_steps":
			for _, s := range steps {
				if !evalScalar(o.Value("pred"), s) {
					return false
				}
			}
			return true
		case "exists_step":
			for _, s := range steps {
				if evalScalar(o.Value("pred"), s) {
					return true
				}
			}
			return false
		case "forall_outputs":
			// A gate step has no metadata output.
			return true
		case "depends_on":
			for _, s := range steps {
				so := s.(*pyjson.Object)
				if pyEq(so.Value("id"), o.Value("step_id_a")) {
					deps, _ := so.Value("depends_on").([]any)
					return pyIn(o.Value("step_id_b"), deps)
				}
			}
			return false
		case "before":
			ids := []any{"s0"}
			if !pyIn(o.Value("step_id_a"), ids) || !pyIn(o.Value("step_id_b"), ids) {
				return false
			}
			return false // both are s0: index(a) < index(b) is false
		case "llm_check":
			if env.Stakes == "physical" {
				valueError("llm_check blocked for physical stakes — use sound primitives only")
			}
			rule, _ := o.Value("rule").(string)
			res := r.runLLMCheck(rule, pyjson.NewObject().Set("task", env.Task).Set("steps", steps))
			// Fail closed: a failed call raises, and the item is a
			// violation.
			if res.errored {
				valueError(res.reason)
			}
			return res.satisfied
		case "and":
			for _, c := range o.Value("children").([]any) {
				if !goE(c) {
					return false
				}
			}
			return true
		case "or":
			for _, c := range o.Value("children").([]any) {
				if goE(c) {
					return true
				}
			}
			return false
		case "not":
			return !goE(o.Value("child"))
		case "implies":
			return !goE(o.Value("a")) || goE(o.Value("b"))
		}
		return evalScalar(e, pyjson.NewObject().Set("steps", steps))
	}
	return goE(e)
}

// resolvePath is predicate_z3._resolve_path.
func resolvePath(obj any, path string) any {
	cur := obj
	for _, part := range strings.Split(path, ".") {
		if o, ok := cur.(*pyjson.Object); ok {
			v, present := o.Get(part)
			if !present {
				return pyMissing
			}
			cur = v
			continue
		}
		cur = getattr(cur, part)
		if cur == pyMissing {
			return pyMissing
		}
	}
	return cur
}

func hasLen(v any) bool {
	switch v.(type) {
	case string, []any, *pyjson.Object:
		return true
	}
	return false
}

// evalScalar is predicate_z3._eval_scalar.
func evalScalar(e any, scope any) bool {
	o, _ := e.(*pyjson.Object)
	path := func() any { return resolvePath(scope, o.Value("path").(string)) }
	switch opOf(e) {
	case "equals":
		return pyEq(path(), o.Value("value"))
	case "not_equals":
		v := path()
		return v != pyMissing && !pyEq(v, o.Value("value"))
	case "in_set":
		v := path()
		return v != pyMissing && pyIn(v, o.Value("values").([]any))
	case "not_in_set":
		v := path()
		return v != pyMissing && !pyIn(v, o.Value("values").([]any))
	case "matches", "not_matches":
		v := path()
		s, isStr := v.(string)
		if v == pyMissing || !isStr {
			return opOf(e) == "not_matches"
		}
		found := reSearch(o.Value("regex").(string), s)
		if opOf(e) == "matches" {
			return found
		}
		return !found
	case "numeric_range":
		v := path()
		r, isNum, finite := numeric(v)
		if v == pyMissing || !isNum {
			return false
		}
		// float(val): a float as it is, an int rounded, raising past
		// float's range.
		f, isFloat := asFloat(v)
		if !isFloat {
			if !finite || isHuge(r) {
				panic(pystr.NewException("OverflowError", "int too large to convert to float"))
			}
			f, _ = r.Float64()
		}
		return o.Value("min").(float64) <= f && f <= o.Value("max").(float64)
	case "length_range":
		v := path()
		if v == pyMissing || !hasLen(v) {
			return false
		}
		n := big.NewInt(int64(pyLenOf(v)))
		minN, _ := new(big.Int).SetString(o.Value("min").(pyjson.Int).Text, 10)
		if n.Cmp(minN) < 0 {
			return false
		}
		if mx, ok := o.Value("max").(pyjson.Int); ok {
			maxN, _ := new(big.Int).SetString(mx.Text, 10)
			if n.Cmp(maxN) > 0 {
				return false
			}
		}
		return true
	case "exists":
		return path() != pyMissing
	case "is_empty":
		v := path()
		if v == pyMissing || v == nil {
			return true
		}
		if hasLen(v) {
			return pyLenOf(v) == 0
		}
		return false
	case "and":
		for _, c := range o.Value("children").([]any) {
			if !evalScalar(c, scope) {
				return false
			}
		}
		return true
	case "or":
		for _, c := range o.Value("children").([]any) {
			if evalScalar(c, scope) {
				return true
			}
		}
		return false
	case "not":
		return !evalScalar(o.Value("child"), scope)
	case "implies":
		return !evalScalar(o.Value("a"), scope) || evalScalar(o.Value("b"), scope)
	case "llm_check":
		valueError("LLMCheck must be evaluated via evaluate_llm_check, not _eval_scalar")
	case "alias":
		valueError(fmt.Sprintf("unresolved alias reference '%s'; resolve aliases before evaluation", o.Value("name").(string)))
	}
	valueError("unknown predicate op: " + pystr.Repr(opOrType(e)))
	return false
}

// isHuge reports an int float() raises OverflowError on: one that rounds
// to 2**1024, so at least 2**1024 - 2**970 in size.
func isHuge(r *big.Rat) bool {
	a := new(big.Rat).Abs(r)
	return a.Cmp(floatLimit) >= 0
}

var floatLimit = new(big.Rat).SetInt(new(big.Int).Sub(
	new(big.Int).Lsh(big.NewInt(1), 1024), new(big.Int).Lsh(big.NewInt(1), 970)))

// reSearch is re.search(pattern, s) is not None: a bad pattern raises
// re.error (or OverflowError) as Python words it.
func reSearch(pattern, s string) bool {
	re, err := pyre.Compile(pattern, 0)
	if err != nil {
		if err.Type == "unported" {
			unported(err.Msg)
		}
		typ := err.Type
		if typ == "error" {
			typ = "re.error"
		}
		panic(pystr.NewException(typ, err.String()))
	}
	// re's FutureWarnings (a nested set, say) are ignored by the gate
	// (gate.py filters them), so they change nothing here.
	return re.Search(s)
}
