package verify

import (
	"fmt"
	"strings"
)

// --- glob -> SMT-LIB2 string-theory encoding (subsumption.py._glob_to_z3) ------
//
// NOT the same algorithm as globs.go's fnmatch/pathlib port (_head_allowed /
// _path_matches_any) — subsumption.py deliberately uses a separate, much
// narrower prefix/suffix encoding so it can be proved in Z3's decidable
// string theory. Anything outside {"**", "prefix/**", no-star exact,
// "*suffix", "prefix*suffix"} is "unsupported": the oracle would encode it
// as permissive-True, but `_patterns_subsume` refuses to trust an
// unsupported OUTER glob (fail-closed — see glob_unsupported below).

func globUnsupported(glob string) bool {
	if glob == "**" || strings.HasSuffix(glob, "/**") || !strings.Contains(glob, "*") {
		return false
	}
	if strings.HasPrefix(glob, "*") && !strings.Contains(glob[1:], "*") {
		return false
	}
	return strings.Count(glob, "*") != 1
}

// globToZ3 returns an SMT-LIB2 boolean term over the string variable `v`.
func globToZ3(v, glob string) string {
	if glob == "**" {
		return "true"
	}
	if strings.HasSuffix(glob, "/**") {
		prefix := glob[:len(glob)-3]
		return fmt.Sprintf("(or (str.prefixof %s %s) (= %s %s))",
			smtQuoteString(prefix+"/"), v, v, smtQuoteString(prefix))
	}
	if !strings.Contains(glob, "*") {
		return fmt.Sprintf("(= %s %s)", v, smtQuoteString(glob))
	}
	if strings.HasPrefix(glob, "*") && !strings.Contains(glob[1:], "*") {
		return fmt.Sprintf("(str.suffixof %s %s)", smtQuoteString(glob[1:]), v)
	}
	if strings.Count(glob, "*") == 1 {
		parts := strings.SplitN(glob, "*", 2)
		return fmt.Sprintf("(and (str.prefixof %s %s) (str.suffixof %s %s))",
			smtQuoteString(parts[0]), v, smtQuoteString(parts[1]), v)
	}
	return "true" // unsupported shape — permissive, matches the oracle's fallback
}

// patternsSubsume ports subsumption._patterns_subsume: is every value
// `inner` admits also admitted by `outer`? Fail-closed: an unsupported
// OUTER glob shape, or a solver timeout, means "cannot prove" -> not
// subsumed.
func patternsSubsume(z3c *Z3Client, inner, outer []string, timeoutMs int) (violation string, err error) {
	if len(inner) == 0 {
		return "", nil
	}
	for _, g := range outer {
		if globUnsupported(g) {
			return fmt.Sprintf("outer declares an unsupported glob shape (%q)", g), nil
		}
	}
	innerOK := smtOr(mapGlob("v", inner))
	outerOK := "false"
	if len(outer) > 0 {
		outerOK = smtOr(mapGlob("v", outer))
	}
	smt2 := "(declare-const v String)\n" +
		fmt.Sprintf("(assert %s)\n(assert (not %s))\n", innerOK, outerOK)
	result, err := z3c.CheckSat(smt2, timeoutMs)
	if err != nil {
		return "", err
	}
	switch result {
	case "unsat":
		return "", nil
	case "sat":
		return "inner admits a value outer forbids", nil
	default:
		return "could not prove subsumption (solver timeout)", nil
	}
}

func mapGlob(v string, globs []string) []string {
	out := make([]string, len(globs))
	for i, g := range globs {
		out[i] = globToZ3(v, g)
	}
	return out
}

// --- network / robot-capability / interpreter structural checks ---------------

func networkScopeViolation(outer, inner Permission) string {
	if !inner.Network {
		return ""
	}
	if !outer.Network {
		return "network: inner uses network but outer forbids it"
	}
	if len(outer.NetworkHosts) == 0 {
		return ""
	}
	if len(inner.NetworkHosts) == 0 {
		return "network: inner admits any host but outer restricts hosts"
	}
	outerSet := map[string]bool{}
	for _, h := range outer.NetworkHosts {
		outerSet[strings.ToLower(h)] = true
	}
	for _, h := range inner.NetworkHosts {
		if !outerSet[strings.ToLower(h)] {
			return "network: inner hosts not in outer allowlist"
		}
	}
	return ""
}

func permissionScopeViolation(z3c *Z3Client, outer, inner Permission, timeoutMs int) (string, error) {
	if inner.ShellAllowDecomposition && !outer.ShellAllowDecomposition {
		return "shell_allow_decomposition: inner admits compound shell but outer does not", nil
	}
	for _, axis := range []struct {
		label       string
		innerP, outerP []string
	}{
		{"file_read", inner.FileRead, outer.FileRead},
		{"file_write", inner.FileWrite, outer.FileWrite},
		{"mcp_allowlist", inner.McpAllowlist, outer.McpAllowlist},
	} {
		reason, err := patternsSubsume(z3c, axis.innerP, axis.outerP, timeoutMs)
		if err != nil {
			return "", err
		}
		if reason != "" {
			return axis.label + ": " + reason, nil
		}
	}
	return networkScopeViolation(outer, inner), nil
}

func robotCapabilityViolation(outer, inner Permission) string {
	if outer.WorkspaceBounds != nil {
		if inner.WorkspaceBounds == nil {
			return "inner declares no workspace_bounds but outer constrains the workspace"
		}
		oMin, oMax := outer.WorkspaceBounds[0], outer.WorkspaceBounds[1]
		iMin, iMax := inner.WorkspaceBounds[0], inner.WorkspaceBounds[1]
		for k := 0; k < 3; k++ {
			if iMin[k] < oMin[k] || iMax[k] > oMax[k] {
				return "inner workspace_bounds exceed outer"
			}
		}
	}
	if outer.VelocityLimit != nil {
		if inner.VelocityLimit == nil || *inner.VelocityLimit > *outer.VelocityLimit {
			return "inner velocity_limit undeclared or exceeds outer"
		}
	}
	if outer.TorqueLimit != nil {
		if inner.TorqueLimit == nil || *inner.TorqueLimit > *outer.TorqueLimit {
			return "inner torque_limit undeclared or exceeds outer"
		}
	}
	for joint, oRange := range outer.JointLimits {
		iRange, ok := inner.JointLimits[joint]
		if !ok {
			return fmt.Sprintf("inner does not bound joint %q that outer limits", joint)
		}
		if iRange[0] < oRange[0] || iRange[1] > oRange[1] {
			return fmt.Sprintf("inner joint %q range exceeds outer", joint)
		}
	}
	missing := 0
	for _, ob := range outer.Obstacles {
		found := false
		for _, ib := range inner.Obstacles {
			if ob == ib {
				found = true
				break
			}
		}
		if !found {
			missing++
		}
	}
	if missing > 0 {
		return "inner omits obstacle region(s) the outer forbids"
	}
	return ""
}

func detectInterpreters(perms Permission) []string {
	if !perms.Shell {
		return nil
	}
	var out []string
	for _, name := range perms.ShellAllowlist {
		if ShellInterpreters[name] {
			out = append(out, name)
		}
	}
	return out
}

// --- shell admission + invariant compilation for the Z3 subsumption query -----

var shellMetacharsForSubsumption = []string{";", "|", "&", "`", "<", ">", "\n", "\r"}

func encodeShellAdmission(perms Permission, cmdVar string) string {
	if !perms.Shell {
		return "false"
	}
	headOK := "false"
	if len(perms.ShellAllowlist) > 0 {
		var terms []string
		for _, head := range perms.ShellAllowlist {
			terms = append(terms,
				fmt.Sprintf("(= %s %s)", cmdVar, smtQuoteString(head)),
				fmt.Sprintf("(str.prefixof %s %s)", smtQuoteString(head+" "), cmdVar))
		}
		headOK = smtOr(terms)
	}
	var noMeta []string
	for _, ch := range shellMetacharsForSubsumption {
		noMeta = append(noMeta, fmt.Sprintf("(not (str.contains %s %s))", cmdVar, smtQuoteString(ch)))
	}
	noMeta = append(noMeta, fmt.Sprintf("(not (str.contains %s %s))", cmdVar, smtQuoteString("$(")))
	return fmt.Sprintf("(and %s %s)", headOK, smtAnd(noMeta))
}

// compileInvariantsForSubsumption ports subsumption._compile_invariants:
// conjunction of expr-bearing invariants (compiled over a fully symbolic
// scope sharing `cmdVar` for a "command" path reference), plus the set of
// opaque (expr-less) invariant types, plus (under strict) the set of
// opaque types NOT in recognizedOpaqueTypes (which short-circuits
// subsumption to holds=false when they belong to the inner envelope).
func compileInvariantsForSubsumption(c *smtCompiler, cmdVar string, invariants []Invariant, strict bool) (term string, strictBlocking []string, err error) {
	var terms []string
	for _, inv := range invariants {
		if !inv.Enforce {
			continue
		}
		if len(inv.Expr) == 0 || string(inv.Expr) == "null" {
			if strict && !recognizedOpaqueTypes[inv.Type] {
				strictBlocking = append(strictBlocking, inv.Type)
			}
			continue
		}
		expr, perr := ParseExpression(inv.Expr)
		if perr != nil {
			return "", strictBlocking, perr
		}
		expr = stripQuantifier(expr) // ForallSteps/ExistsStep collapse to their child predicate
		t, cerr := c.compileScalar(expr)
		if cerr != nil {
			return "", strictBlocking, cerr
		}
		terms = append(terms, t)
	}
	if len(terms) == 0 {
		return "true", strictBlocking, nil
	}
	return smtAnd(terms), strictBlocking, nil
}

// EnvelopeSubsumes ports subsumption.envelope_subsumes. Only the boolean
// `holds` result and the `err` (for genuine solver failures) are used by
// this client — the oracle's rich Counterexample/reasons are informative
// and not part of the wire verdict.
func EnvelopeSubsumes(z3c *Z3Client, outer, inner Envelope, timeoutMs int, strict bool) (bool, error) {
	if v := robotCapabilityViolation(outer.Permissions, inner.Permissions); v != "" {
		return false, nil
	}
	scopeViolation, err := permissionScopeViolation(z3c, outer.Permissions, inner.Permissions, timeoutMs)
	if err != nil {
		return false, err
	}
	if scopeViolation != "" {
		return false, nil
	}

	if outer.ShellInterpreterPolicy == "strict" && len(detectInterpreters(inner.Permissions)) > 0 {
		return false, nil
	}

	const cmdVar = "ctx_command"
	innerC := newSMTCompiler("ctx")
	outerC := newSMTCompiler("ctx")
	// Seed both compilers' string-var cache so an invariant referencing
	// "command" shares the same declared symbol as the shell-admission
	// encoding (matches _Scope.vars seeding in the oracle).
	innerC.stringVars["command"] = cmdVar
	outerC.stringVars["command"] = cmdVar

	innerInv, innerStrictBlocking, err := compileInvariantsForSubsumption(innerC, cmdVar, inner.Invariants, strict)
	if err != nil {
		return false, err
	}
	// outerStrictBlocking (opaque outer invariants under strict) is
	// surfaced by the oracle only in `unverified_invariants` — informative,
	// not part of the wire verdict — so it's intentionally discarded here.
	outerInv, _, err := compileInvariantsForSubsumption(outerC, cmdVar, outer.Invariants, strict)
	if err != nil {
		return false, err
	}
	if strict && len(innerStrictBlocking) > 0 {
		return false, nil
	}

	innerShell := encodeShellAdmission(inner.Permissions, cmdVar)
	outerShell := encodeShellAdmission(outer.Permissions, cmdVar)

	// Soft-node polarity (subsumption.py v0.13.0 fix): inner soft nodes bind
	// True (optimistic), outer-only soft nodes fail closed outright rather
	// than being pinned False (that direction is unsound under negation).
	innerSoftSet := map[string]bool{}
	for _, n := range innerC.soft {
		innerSoftSet[n] = true
	}
	for _, n := range outerC.soft {
		if !innerSoftSet[n] {
			return false, nil
		}
	}

	var decls []string
	decls = append(decls, fmt.Sprintf("(declare-const %s String)", cmdVar))
	seen := map[string]bool{cmdVar: true}
	for _, d := range append(append([]string{}, innerC.decls...), outerC.decls...) {
		if seen[d] {
			continue
		}
		seen[d] = true
		decls = append(decls, d)
	}
	var asserts []string
	for _, n := range innerC.soft {
		asserts = append(asserts, fmt.Sprintf("(assert (= %s true))", n))
	}
	innerAdmits := fmt.Sprintf("(and %s %s)", innerShell, innerInv)
	outerAdmits := fmt.Sprintf("(and %s %s)", outerShell, outerInv)
	asserts = append(asserts, fmt.Sprintf("(assert %s)", innerAdmits))
	asserts = append(asserts, fmt.Sprintf("(assert (not %s))", outerAdmits))

	smt2 := strings.Join(decls, "\n") + "\n" + strings.Join(asserts, "\n")
	result, err := z3c.CheckSat(smt2, timeoutMs)
	if err != nil {
		return false, err
	}
	switch result {
	case "unsat":
		return true, nil
	case "sat":
		return false, nil
	default:
		return false, fmt.Errorf("z3 subsumption check exceeded %dms", timeoutMs)
	}
}

// --- the delegation-safety stage (verify.check_skill_delegations) -------------

// CheckSkillDelegations ports verify.check_skill_delegations. A SkillStep
// with no contract_envelope is opaque (strict rejects, non-strict warns +
// allows). One with a contract proves envelope_subsumes(caller, contract).
func CheckSkillDelegations(plan ActionPlan, env Envelope, strict bool, timeoutMs int, warnings *[]string) []Violation {
	var violations []Violation
	haveSkill := false
	for _, s := range plan.Steps {
		if s.Type == "skill" {
			haveSkill = true
			break
		}
	}
	if !haveSkill {
		return nil
	}
	z3c, zerr := sharedZ3()
	for _, step := range plan.Steps {
		if step.Type != "skill" {
			continue
		}
		contractEnv, hasContract := step.ContractEnvelope()
		if !hasContract {
			if strict {
				violations = append(violations, VStep("delegation", step.ID))
			} else if warnings != nil {
				skillID, _ := step.SkillID()
				*warnings = append(*warnings, "opaque skill "+skillID+" (allowed under lenient mode)")
			}
			continue
		}
		if zerr != nil {
			// Full profile unavailable -- fail closed rather than trust an
			// unproved delegation.
			violations = append(violations, VStep("delegation", step.ID))
			continue
		}
		holds, err := EnvelopeSubsumes(z3c, env, *contractEnv, timeoutMs, strict)
		if err != nil || !holds {
			violations = append(violations, VStep("delegation", step.ID))
		}
	}
	return violations
}
