package verify

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
)

// VerifyStep ports verify.verify_step, the supervisor's per-step gate on a
// singleton plan: delegation safety, permissions, skill delegations, the
// robotics checks and the (trivial) DAG, each short-circuiting. The
// envelope's own Z3 checks and the plan-level predicates are the whole-plan
// verify's.
func VerifyStep(plan ActionPlan, env Envelope, z3TimeoutMs int) VerifyResultGo {
	violations := checkDelegationSafety(plan, env)
	if len(violations) > 0 {
		return result(violations)
	}
	strict := ResolveStrict(nil, env)
	violations = CheckPermissions(plan, env, strict)
	if len(violations) > 0 {
		return result(violations)
	}
	var warnings, timeouts []string
	unmodeled := false
	done := func(vs []Violation) VerifyResultGo {
		r := result(vs)
		r.Timeouts, r.Warnings, r.WarningsUnmodeled = timeouts, warnings, unmodeled
		return r
	}
	violations = CheckSkillDelegations(plan, env, strict, z3TimeoutMs, &warnings, &timeouts, &unmodeled)
	if len(violations) > 0 {
		return done(violations)
	}
	violations = CheckPlanInvariantsRobotics(plan, env)
	if len(violations) > 0 {
		return done(violations)
	}
	return done(CheckDAG(plan))
}

// recognizedStage2 is stage2._OPAQUE_POSTCONDITION_HANDLERS' keys.
var recognizedStage2 = map[string]bool{"exit_code": true, "file_exists": true, "file_size_range": true}

// Stage2Refusal names a postcondition VerifyCompletedStep does not decide
// the oracle's way (an expr it does not evaluate, or one that asks a
// model), or "" when it decides every enforced one. A caller refuses such
// an envelope before anything runs.
func Stage2Refusal(env Envelope) string {
	// verify() asks a model for an llm_check invariant; this binary does
	// not.
	for _, inv := range env.Invariants {
		if strings.Contains(string(inv.Expr), `"llm_check"`) {
			return fmt.Sprintf("invariant '%s' asks a model (llm_check)", inv.Type)
		}
	}
	for _, pc := range env.Postconditions {
		if !pc.Enforce {
			continue
		}
		raw := strings.TrimSpace(string(pc.Expr))
		if raw == "" || raw == "null" {
			continue
		}
		if !strings.HasPrefix(raw, "{") {
			return fmt.Sprintf("postcondition '%s' has an expr that is not a dict", pc.Type)
		}
		if strings.Contains(raw, `"llm_check"`) || strings.Contains(raw, `"alias"`) {
			return fmt.Sprintf("postcondition '%s' asks a model or names an alias", pc.Type)
		}
		if _, err := ParseExpression(pc.Expr); err != nil {
			return fmt.Sprintf("postcondition '%s' has an expr this binary does not read", pc.Type)
		}
	}
	return ""
}

func pyIntEq(rc int, n *json.Number) bool {
	v, ok := new(big.Int).SetString(string(*n), 10)
	return ok && v.Cmp(big.NewInt(int64(rc))) == 0
}

func bigOf(n *json.Number, def *big.Int) (*big.Int, bool) {
	if n == nil {
		return def, true
	}
	v, ok := new(big.Int).SetString(string(*n), 10)
	return v, ok
}

// VerifyCompletedStep ports stage2.verify_completed_step: the envelope's
// enforced postconditions over one completed step, whose metadata holds
// its output and rc. Only the violation messages are returned; the
// supervisor reads the first.
func VerifyCompletedStep(step Step, env Envelope, strict *bool) []string {
	effective := ResolveStrict(strict, env)
	plan := ActionPlan{Source: "stage2", Task: env.Task, Steps: []Step{step}}
	var out []string
	violated := func(pc Postcondition) {
		out = append(out, fmt.Sprintf("postcondition '%s' violated on completed step %s", pc.Type, step.ID))
	}
	for _, pc := range env.Postconditions {
		if !pc.Enforce {
			continue
		}
		raw := strings.TrimSpace(string(pc.Expr))
		if raw == "" || raw == "null" {
			if recognizedStage2[pc.Type] {
				if !stage2Handler(pc, step) {
					violated(pc)
				}
				continue
			}
			if effective {
				out = append(out, fmt.Sprintf("postcondition '%s' declares a safety property with no "+
					"verifiable expr; cannot be discharged under strict mode", pc.Type))
			}
			continue
		}
		expr, err := ParseExpression(pc.Expr)
		if err != nil {
			out = append(out, fmt.Sprintf("postcondition '%s' evaluation error: %v", pc.Type, err))
			continue
		}
		ok, err := EvaluatePredicate(expr, plan, env)
		if err != nil {
			out = append(out, fmt.Sprintf("postcondition '%s' evaluation error: %v", pc.Type, err))
			continue
		}
		if !ok {
			violated(pc)
		}
	}
	return out
}

// stage2Handler is one of the opaque postcondition handlers: true when it
// is discharged.
func stage2Handler(pc Postcondition, step Step) bool {
	switch pc.Type {
	case "exit_code":
		md, _ := step.Raw["metadata"].(map[string]interface{})
		rc, has := md["rc"]
		if !has || rc == nil || pc.Expected == nil {
			return false
		}
		n, ok := rc.(float64)
		if !ok {
			return false
		}
		return pyIntEq(int(n), pc.Expected)
	case "file_exists":
		if pc.Path == nil || *pc.Path == "" {
			return false
		}
		_, err := os.Stat(*pc.Path)
		return err == nil
	case "file_size_range":
		if pc.Path == nil || *pc.Path == "" || (pc.Min == nil && pc.Max == nil) {
			return false
		}
		st, err := os.Stat(*pc.Path)
		if err != nil {
			return false
		}
		size := big.NewInt(st.Size())
		lo, ok1 := bigOf(pc.Min, big.NewInt(0))
		if !ok1 {
			return false
		}
		if lo.Cmp(size) > 0 {
			return false
		}
		if pc.Max == nil {
			return true
		}
		hi, ok2 := bigOf(pc.Max, nil)
		return ok2 && size.Cmp(hi) <= 0
	}
	return false
}
