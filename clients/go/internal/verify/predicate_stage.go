package verify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"daisugi-verify/internal/pyjson"
)

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
	label    string // "invariant" | "postcondition"
	typeName string
	target   *string
	rawExpr  json.RawMessage
	enforce  bool
}

// checkPredicateItem ports verify._check_predicate_item. `aliases` is
// always unavailable in this client (the conformance corpus never records
// a verify() call carrying an AliasRegistry — see docs/spec/conformance.md
// "Cases embedding ... AliasRegistry ... skipped"), so an AliasRef always
// takes the "no registry" branch.
func checkPredicateItem(item predicateItem, plan ActionPlan, env Envelope, strict bool, pin *string, base string, warnings *[]string) []Violation {
	if !item.enforce {
		return nil
	}
	opaque := len(item.rawExpr) == 0 || string(item.rawExpr) == "null"
	// An opaque invariant whose type names a word of the system dialect
	// (verify._dialect_word). Audit: the opaque handling below decides and
	// the word's would-deny is a warning. Enforce: the word decides.
	if word, isWord := WordFor(item.typeName); isWord && opaque && item.label == "invariant" {
		if pin != nil {
			return enforceWord(item, word, plan, env, *pin, base)
		}
		auditWord(item, word, plan, env, base, warnings)
	}
	if opaque {
		// Opaque item: no predicate to evaluate.
		if item.label == "invariant" {
			if reason := roboticsBackingMissing(item.typeName, env.Permissions); reason != "" {
				return []Violation{V("predicate", fmt.Sprintf(
					"invariant '%s' is declared but its backing permission (%s) is absent; the check no-ops and the invariant is unenforced — add the bound or remove the invariant",
					item.typeName, reason))}
			}
		}
		dischargedElsewhere := (item.label == "invariant" && recognizedOpaqueTypes[item.typeName]) ||
			(item.label == "postcondition" && recognizedStage2PostconditionTypes[item.typeName])
		if strict && !dischargedElsewhere {
			return []Violation{V("predicate", fmt.Sprintf(
				"%s '%s' declares a safety property with no verifiable expr; cannot be discharged under strict mode", item.label, item.typeName)).
				With(pyjson.NewObject().Set(item.label, item.typeName).Set("reason", "opaque_unrecognized").
					Set("suggested_remediation", "add an `expr` to make it verifiable, "+
						"or set enforce=False to keep it as documentation"), nil)}
		}
		return nil
	}

	var probe any
	dec := json.NewDecoder(bytes.NewReader(item.rawExpr))
	dec.UseNumber()
	if dec.Decode(&probe) == nil {
		if _, isObj := probe.(map[string]any); !isObj {
			// _normalize_expr passes a non-dict through; evaluation then
			// raises on its Python type.
			return []Violation{V("predicate", fmt.Sprintf("%s '%s' evaluation error: unknown predicate op: '%s'",
				item.label, item.typeName, pyTypeName(probe)))}
		}
	}
	expr, err := ParseExpression(item.rawExpr)
	if err != nil {
		return []Violation{V("predicate", fmt.Sprintf("%s '%s' evaluation error: %v", item.label, item.typeName, err))}
	}

	if alias, ok := expr.(AliasRef); ok {
		// Unresolved alias: this client never carries a registry.
		return []Violation{V("predicate", fmt.Sprintf(
			"%s '%s' references unresolved alias '%s'; pass an AliasRegistry via aliases= to verify()", item.label, item.typeName, alias.Name))}
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
		return []Violation{V("predicate", fmt.Sprintf(
			"%s '%s' can never be satisfied (unsatisfiable); the envelope can never pass — fix the predicate", item.label, item.typeName))}
	}
	if vacuity == Tautology {
		if strict {
			return []Violation{V("predicate", fmt.Sprintf(
				"%s '%s' is a tautology (constrains nothing); tighten the predicate or remove it", item.label, item.typeName))}
		}
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("%s '%s' is a tautology (constrains nothing); "+
				"tighten the predicate or remove it", item.label, item.typeName))
		}
	}

	ok, evalErr := EvaluatePredicate(expr, plan, env)
	if evalErr != nil {
		return []Violation{V("predicate", fmt.Sprintf("%s '%s' evaluation error: %v", item.label, item.typeName, evalErr))}
	}
	if !ok {
		return []Violation{V("predicate", fmt.Sprintf("%s '%s' violated", item.label, item.typeName))}
	}
	return nil
}

// CheckPredicateInvariants ports verify._check_predicate_invariants, with
// no dialect pin (the words in audit).
func CheckPredicateInvariants(plan ActionPlan, env Envelope, strict bool, warnings *[]string) []Violation {
	return CheckPredicateInvariantsPin(plan, env, strict, nil, "", warnings)
}

// CheckPredicateInvariantsPin is verify._check_predicate_invariants with
// the dialect pin of verify(dialect_pin=...), nil for audit, and the base
// a word is placed from (verify(dialect_base=...), "" for none).
func CheckPredicateInvariantsPin(plan ActionPlan, env Envelope, strict bool, pin *string, base string, warnings *[]string) []Violation {
	var violations []Violation
	for _, inv := range env.Invariants {
		violations = append(violations, checkPredicateItem(predicateItem{
			label: "invariant", typeName: inv.Type, target: inv.Target, rawExpr: inv.Expr, enforce: inv.Enforce,
		}, plan, env, strict, pin, base, warnings)...)
	}
	for _, pc := range env.Postconditions {
		violations = append(violations, checkPredicateItem(predicateItem{
			label: "postcondition", typeName: pc.Type, rawExpr: pc.Expr, enforce: pc.Enforce,
		}, plan, env, strict, pin, base, warnings)...)
	}
	return violations
}

// wordVerdict is verify._word_verdict: unfold word(target), placed from
// base, and evaluate the kernel term; why says what fails.
func wordVerdict(word, target string, plan ActionPlan, env Envelope, base string) (bool, string) {
	expr, err := UnfoldWord(word, target, base)
	if err != nil {
		if ug, ok := err.(UnsupportedGlob); ok {
			return false, fmt.Sprintf("the target is not a supported glob (%s)", ug.Msg)
		}
		return false, fmt.Sprintf("evaluation error: %v", err)
	}
	holds, err := EvaluatePredicate(expr, plan, env)
	if err != nil {
		return false, fmt.Sprintf("evaluation error: %v", err)
	}
	if holds {
		return true, ""
	}
	placed, _ := ResolveTarget(target, base)
	regex, _ := GlobRegex(placed)
	return false, wordWitness(regex, plan, base)
}

// auditWord is verify._audit_word.
func auditWord(item predicateItem, word string, plan ActionPlan, env Envelope, base string, warnings *[]string) {
	glob := WordTarget(word, item.target)
	holds, why := wordVerdict(word, glob, plan, env, base)
	if holds || warnings == nil {
		return
	}
	*warnings = append(*warnings, fmt.Sprintf("%sinvariant '%s' is %s('%s'); %s; enforcing would deny",
		DialectAuditPrefix, item.typeName, word, glob, why))
}

// enforceWord is verify._enforce_word.
func enforceWord(item predicateItem, word string, plan ActionPlan, env Envelope, pin, base string) []Violation {
	glob := WordTarget(word, item.target)
	if pin != DialectHash {
		return []Violation{V("predicate", fmt.Sprintf(
			"%s '%s' is the word %s, but the enforced dialect '%s' is not this build's dialect '%s'",
			item.label, item.typeName, word, pin, DialectHash)).
			With(pyjson.NewObject().Set(item.label, item.typeName).Set("word", word).
				Set("reason", "dialect_pin_mismatch").
				Set("suggested_remediation", fmt.Sprintf(
					"read the definitions of dialect '%s', then set dialect_enforce: %s in config.yaml",
					DialectHash, DialectHash)), nil)}
	}
	holds, why := wordVerdict(word, glob, plan, env, base)
	if holds {
		return nil
	}
	return []Violation{V("predicate", fmt.Sprintf("%s '%s' violated: %s('%s'); %s",
		item.label, item.typeName, word, glob, why)).
		With(pyjson.NewObject().Set(item.label, item.typeName).Set("word", word).
			Set("target", glob).Set("dialect", DialectHash).Set("reason", "word_violated"), nil)}
}

// pyTypeName is type(v).__name__ for a value json gives.
func pyTypeName(v any) string {
	switch x := v.(type) {
	case string:
		return "str"
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	case nil:
		return "NoneType"
	}
	return "object"
}
