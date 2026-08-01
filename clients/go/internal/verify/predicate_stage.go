package verify

import "encoding/json"

// recognizedOpaqueTypes / recognizedStage2PostconditionTypes mirror
// _invariant_types.py — the single source of truth the oracle asserts its
// z3_checks handler set against.
var recognizedOpaqueTypes = map[string]bool{
	"end_effector_in_workspace": true,
	"joint_limits_respected":    true,
	"velocity_bounded":          true,
	"no_obstacle_penetration":   true,
}

var recognizedStage2PostconditionTypes = map[string]bool{
	"exit_code":       true,
	"file_exists":     true,
	"file_size_range": true,
}

// roboticsBackingMissing ports verify._robotics_backing_missing.
func roboticsBackingMissing(typeName string, perms Permission) string {
	if typeName == "end_effector_in_workspace" && perms.WorkspaceBounds == nil {
		return "workspace_bounds"
	}
	if typeName == "velocity_bounded" && perms.VelocityLimit == nil {
		return "velocity_limit"
	}
	return ""
}

type predicateItem struct {
	label   string // "invariant" | "postcondition"
	typeName string
	rawExpr json.RawMessage
	enforce bool
}

// checkPredicateItem ports verify._check_predicate_item. `aliases` is
// always unavailable in this client (the conformance corpus never records
// a verify() call carrying an AliasRegistry — see docs/spec/conformance.md
// "Cases embedding ... AliasRegistry ... skipped"), so an AliasRef always
// takes the "no registry" branch.
func checkPredicateItem(item predicateItem, plan ActionPlan, env Envelope, strict bool, warnings *[]string) []Violation {
	if !item.enforce {
		return nil
	}
	if len(item.rawExpr) == 0 || string(item.rawExpr) == "null" {
		// Opaque item: no predicate to evaluate.
		if item.label == "invariant" {
			if reason := roboticsBackingMissing(item.typeName, env.Permissions); reason != "" {
				return []Violation{V("predicate")}
			}
		}
		dischargedElsewhere := (item.label == "invariant" && recognizedOpaqueTypes[item.typeName]) ||
			(item.label == "postcondition" && recognizedStage2PostconditionTypes[item.typeName])
		if strict && !dischargedElsewhere {
			return []Violation{V("predicate")}
		}
		return nil
	}

	expr, err := ParseExpression(item.rawExpr)
	if err != nil {
		return []Violation{V("predicate")}
	}

	if alias, ok := expr.(AliasRef); ok {
		_ = alias
		return []Violation{V("predicate")} // unresolved alias — no registry
	}

	// Vacuity check. The oracle wraps this in try/except Exception ->
	// "non_trivial" (Z3 unavailable, unsupported expr, a nested unresolved
	// alias inside And/Implies, or ADJUDICATIONS.md F-3's NotEquals sort
	// mismatch all land here). check_vacuity's OWN default timeout (500ms)
	// is used regardless of the case's z3_timeout_ms — verify.py calls it
	// with no timeout_ms argument.
	vacuity := NonTrivial
	if z3c, zerr := sharedZ3(); zerr == nil {
		if v, verr := CheckVacuity(z3c, expr, 500); verr == nil {
			vacuity = v
		}
	}
	if vacuity == Contradiction {
		return []Violation{V("predicate")}
	}
	if vacuity == Tautology {
		if strict {
			return []Violation{V("predicate")}
		}
		if warnings != nil {
			*warnings = append(*warnings, item.typeName+" is a tautology")
		}
	}

	ok, evalErr := EvaluatePredicate(expr, plan, env)
	if evalErr != nil {
		return []Violation{V("predicate")}
	}
	if !ok {
		return []Violation{V("predicate")}
	}
	return nil
}

// CheckPredicateInvariants ports verify._check_predicate_invariants.
func CheckPredicateInvariants(plan ActionPlan, env Envelope, strict bool, warnings *[]string) []Violation {
	var violations []Violation
	for _, inv := range env.Invariants {
		violations = append(violations, checkPredicateItem(predicateItem{
			label: "invariant", typeName: inv.Type, rawExpr: inv.Expr, enforce: inv.Enforce,
		}, plan, env, strict, warnings)...)
	}
	for _, pc := range env.Postconditions {
		violations = append(violations, checkPredicateItem(predicateItem{
			label: "postcondition", typeName: pc.Type, rawExpr: pc.Expr, enforce: pc.Enforce,
		}, plan, env, strict, warnings)...)
	}
	return violations
}
