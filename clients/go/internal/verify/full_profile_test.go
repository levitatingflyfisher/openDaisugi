package verify

import (
	"encoding/json"
	"testing"
)

// requireZ3 skips the test when z3 isn't on PATH — these tests exercise
// the Full profile's real "z3 -in" subprocess (docs/spec/conformance.md:
// emit SMT-LIB2 text, invoke a solver binary), not a mock.
func requireZ3(t *testing.T) *Z3Client {
	t.Helper()
	z3c, err := sharedZ3()
	if err != nil {
		t.Skipf("z3 not available: %v", err)
	}
	return z3c
}

func TestCheckEnvelopeSelfConsistency(t *testing.T) {
	requireZ3(t)
	env := DefaultEnvelope()
	env.Permissions.Shell = false
	env.Permissions.ShellAllowlist = []string{"git"} // inconsistent: allowlist without shell
	v := CheckEnvelopeSelfConsistency(env, 500)
	if len(v) != 1 || v[0].Stage != "z3" || v[0].HasStep {
		t.Fatalf("want one z3 violation with step=null, got %+v", v)
	}

	env2 := DefaultEnvelope()
	env2.Permissions.Shell = true
	env2.Permissions.ShellAllowlist = []string{"git"}
	if v := CheckEnvelopeSelfConsistency(env2, 500); len(v) != 0 {
		t.Fatalf("want zero violations for a consistent envelope, got %+v", v)
	}

	env3 := DefaultEnvelope()
	env3.Permissions.MaxExecutionTimeS = 9999 // outside (0, 3600]
	if v := CheckEnvelopeSelfConsistency(env3, 500); len(v) != 1 {
		t.Fatalf("want one violation for max_execution_time_s out of range, got %+v", v)
	}
}

func TestCheckPlanAgainstEnvelope(t *testing.T) {
	requireZ3(t)
	env := DefaultEnvelope() // shell=false
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "shell", map[string]interface{}{"command": "ls"})}}
	v := CheckPlanAgainstEnvelope(plan, env, 500)
	if len(v) != 1 {
		t.Fatalf("want one violation (plan needs shell, envelope forbids it), got %+v", v)
	}
}

func TestCheckVacuity(t *testing.T) {
	z3c := requireZ3(t)
	// Equals(type, shell) AND NotEquals(type, shell) -- unsatisfiable.
	contradiction := And{Children: []Expression{
		Equals{Path: "type", Value: "shell"},
		NotEquals{Path: "type", Value: "shell"},
	}}
	v, err := CheckVacuity(z3c, contradiction, 500)
	if err != nil || v != Contradiction {
		t.Fatalf("want contradiction, got %v err=%v", v, err)
	}

	// Equals(type, shell) OR NotEquals(type, shell) -- always true.
	tautology := Or{Children: []Expression{
		Equals{Path: "type", Value: "shell"},
		NotEquals{Path: "type", Value: "shell"},
	}}
	v, err = CheckVacuity(z3c, tautology, 500)
	if err != nil || v != Tautology {
		t.Fatalf("want tautology, got %v err=%v", v, err)
	}

	// A real constraint -- neither.
	nonTrivial := Equals{Path: "type", Value: "shell"}
	v, err = CheckVacuity(z3c, nonTrivial, 500)
	if err != nil || v != NonTrivial {
		t.Fatalf("want non_trivial, got %v err=%v", v, err)
	}
}

func TestCheckPredicateInvariants_ForallStepsViolated(t *testing.T) {
	requireZ3(t)
	env := DefaultEnvelope()
	exprJSON := `{"op":"forall_steps","pred":{"op":"equals","path":"type","value":"shell"}}`
	var raw json.RawMessage = json.RawMessage(exprJSON)
	env.Invariants = []Invariant{{Type: "must_be_shell", Description: "x", Expr: raw, Enforce: true}}
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "task", map[string]interface{}{"prompt": "x"})}}
	var warnings []string
	v := CheckPredicateInvariants(plan, env, false, &warnings)
	if len(v) != 1 || v[0].Stage != "predicate" {
		t.Fatalf("want one predicate violation, got %+v", v)
	}
}

func TestCheckPredicateInvariants_UnresolvedAlias(t *testing.T) {
	requireZ3(t)
	env := DefaultEnvelope()
	env.Invariants = []Invariant{{
		Type: "only_ls", Description: "via alias", Enforce: true,
		Expr: json.RawMessage(`{"op":"alias","name":"only_ls","args":{}}`),
	}}
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "task", map[string]interface{}{"prompt": "x"})}}
	var warnings []string
	v := CheckPredicateInvariants(plan, env, false, &warnings)
	if len(v) != 1 || v[0].Stage != "predicate" {
		t.Fatalf("want one predicate violation (unresolved alias, no registry), got %+v", v)
	}
}

func TestCheckPredicateInvariants_OpaqueUnderStrict(t *testing.T) {
	requireZ3(t)
	env := DefaultEnvelope()
	env.Invariants = []Invariant{{Type: "custom_unknown", Description: "opaque", Enforce: true}}
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "task", map[string]interface{}{"prompt": "x"})}}
	var warnings []string
	if v := CheckPredicateInvariants(plan, env, true, &warnings); len(v) != 1 {
		t.Fatalf("strict mode: want one violation for an opaque unrecognized invariant, got %+v", v)
	}
	if v := CheckPredicateInvariants(plan, env, false, &warnings); len(v) != 0 {
		t.Fatalf("non-strict mode: want zero violations for an opaque invariant, got %+v", v)
	}
}

func TestEnvelopeSubsumes_ShellAllowlistSuperset(t *testing.T) {
	z3c := requireZ3(t)
	outer := DefaultEnvelope()
	outer.Permissions.Shell = true
	outer.Permissions.ShellAllowlist = []string{"ls", "cat", "rm"}
	inner := DefaultEnvelope()
	inner.Permissions.Shell = true
	inner.Permissions.ShellAllowlist = []string{"ls", "cat"}
	holds, err := EnvelopeSubsumes(z3c, outer, inner, 2000, false)
	if err != nil || !holds {
		t.Fatalf("want holds=true (outer is a superset), got holds=%v err=%v", holds, err)
	}
}

func TestEnvelopeSubsumes_ShellAllowlistNotSubsumed(t *testing.T) {
	z3c := requireZ3(t)
	outer := DefaultEnvelope()
	outer.Permissions.Shell = true
	outer.Permissions.ShellAllowlist = []string{"ls"}
	inner := DefaultEnvelope()
	inner.Permissions.Shell = true
	inner.Permissions.ShellAllowlist = []string{"ls", "rm"}
	holds, err := EnvelopeSubsumes(z3c, outer, inner, 2000, false)
	if err != nil || holds {
		t.Fatalf("want holds=false (inner admits rm, outer doesn't), got holds=%v err=%v", holds, err)
	}
}

func TestCheckSkillDelegations_OpaqueSkillStrictRejects(t *testing.T) {
	env := DefaultEnvelope()
	env.Permissions.Shell = true
	env.Permissions.ShellAllowlist = []string{"ls"}
	plan := ActionPlan{Steps: []Step{
		stepRaw("k1", "skill", map[string]interface{}{"skill_id": "mystery", "skill_input": map[string]interface{}{}}),
	}}
	var warnings []string
	v := CheckSkillDelegations(plan, env, true, 2000, &warnings)
	if len(v) != 1 || v[0].Stage != "delegation" || v[0].Step != "k1" {
		t.Fatalf("strict mode: want one delegation violation for an opaque skill, got %+v", v)
	}
	v = CheckSkillDelegations(plan, env, false, 2000, &warnings)
	if len(v) != 0 {
		t.Fatalf("non-strict mode: want zero violations for an opaque skill, got %+v", v)
	}
}
