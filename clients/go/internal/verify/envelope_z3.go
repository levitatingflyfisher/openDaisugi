package verify

import (
	"fmt"
	"strings"
)

func smtBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// CheckEnvelopeSelfConsistency ports z3_checks.check_envelope_self_consistency.
// Emits SMT-LIB2 text and checks it via the shared z3 -in subprocess (Full
// profile: docs/spec/conformance.md — "emitting SMT-LIB2 text and invoking
// a solver binary", not a bound API), even though the three constraints
// pinned here (shell_allowlist implies shell; a file_exists postcondition
// implies file_write; 0 < max_execution_time_s <= 3600) are each trivially
// decidable by inspection — see clients/go/README.md for the honest
// bench accounting of what actually needs the solver.
//
// timeout is the VerificationTimeout text when Z3 answers unknown: the
// oracle's verify() keeps that as a warning, and a caller that must not
// admit an unchecked envelope refuses on it. A Z3 error is a violation:
// the check did not pass.
func CheckEnvelopeSelfConsistency(env Envelope, timeoutMs int) (violations []Violation, timeout string) {
	z3c, err := sharedZ3()
	if err != nil {
		// Full profile unavailable (z3 missing) -- fail closed rather than
		// silently pass an unverified envelope.
		return []Violation{V("z3", "Z3 is not available: "+err.Error())}, ""
	}
	var b strings.Builder
	b.WriteString("(declare-const shell Bool)\n")
	b.WriteString("(declare-const can_write Bool)\n")
	fmt.Fprintf(&b, "(assert (= shell %s))\n", smtBool(env.Permissions.Shell))
	fmt.Fprintf(&b, "(assert (= can_write %s))\n", smtBool(len(env.Permissions.FileWrite) > 0))
	if len(env.Permissions.ShellAllowlist) > 0 {
		b.WriteString("(assert (= shell true))\n")
	}
	for _, pc := range env.Postconditions {
		if pc.Type == "file_exists" {
			b.WriteString("(assert (= can_write true))\n")
		}
	}
	b.WriteString("(declare-const max_time Int)\n")
	fmt.Fprintf(&b, "(assert (= max_time %d))\n", env.Permissions.MaxExecutionTimeS)
	b.WriteString("(assert (> max_time 0))\n")
	b.WriteString("(assert (<= max_time 3600))\n")

	result, err := z3c.CheckSat(b.String(), timeoutMs)
	if err != nil {
		return []Violation{V("z3", "Z3 could not check the envelope: "+err.Error())}, ""
	}
	if result == "unknown" {
		return nil, fmt.Sprintf("Z3 self-consistency check exceeded %dms", timeoutMs)
	}
	if result == "unsat" {
		return []Violation{V("z3", "Envelope is internally inconsistent")}, ""
	}
	return nil, ""
}

// CheckPlanAgainstEnvelope ports z3_checks.check_plan_against_envelope.
func CheckPlanAgainstEnvelope(plan ActionPlan, env Envelope, timeoutMs int) (violations []Violation, timeout string) {
	z3c, err := sharedZ3()
	if err != nil {
		return []Violation{V("z3", "Z3 is not available: "+err.Error())}, ""
	}
	var b strings.Builder
	b.WriteString("(declare-const shell_available Bool)\n")
	b.WriteString("(declare-const write_available Bool)\n")
	fmt.Fprintf(&b, "(assert (= shell_available %s))\n", smtBool(env.Permissions.Shell))
	fmt.Fprintf(&b, "(assert (= write_available %s))\n", smtBool(len(env.Permissions.FileWrite) > 0))
	for _, s := range plan.Steps {
		if s.Type == "shell" {
			b.WriteString("(assert (= shell_available true))\n")
			break
		}
	}
	for _, s := range plan.Steps {
		if s.Type == "file_write" {
			b.WriteString("(assert (= write_available true))\n")
			break
		}
	}

	result, err := z3c.CheckSat(b.String(), timeoutMs)
	if err != nil {
		return []Violation{V("z3", "Z3 could not check the plan: "+err.Error())}, ""
	}
	if result == "unknown" {
		return nil, fmt.Sprintf("Z3 plan-vs-envelope check exceeded %dms", timeoutMs)
	}
	if result == "unsat" {
		return []Violation{V("z3", "Plan requirements contradict envelope permissions")}, ""
	}
	return nil, ""
}
