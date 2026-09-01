package verify

import (
	"fmt"
	"net/url"
	"strings"

	"daisugi-verify/internal/pystr"
)

const maxInterpreterDepth = 4

// sanctionedWriteSinks / sanctionedReadSources mirror verify.py's
// _SANCTIONED_WRITE_SINKS / _SANCTIONED_READ_SOURCES.
var sanctionedWriteSinks = map[string]bool{"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true}
var sanctionedReadSources = map[string]bool{"/dev/null": true, "/dev/stdin": true}

// knownStepTypes mirrors verify._KNOWN_STEP_TYPES.
var knownStepTypes = map[string]bool{
	"shell": true, "network": true, "file_read": true, "file_write": true,
	"mcp": true, "task": true, "skill": true, "agentic": true,
	"joint_move": true, "cartesian_move": true, "gripper": true,
	"sim_reset": true, "vla": true,
}

// agenticToolCapabilities mirrors verify._AGENTIC_TOOL_CAPABILITIES.
var agenticToolCapabilities = map[string]string{
	"Bash": "shell", "Read": "file_read", "Glob": "file_read", "Grep": "file_read",
	"Write": "file_write", "Edit": "file_write", "MultiEdit": "file_write",
	"WebFetch": "network", "WebSearch": "network",
}

// VerifyOptions mirrors the verify()/kwargs surface the conformance corpus
// exercises.
type VerifyOptions struct {
	Strict      *bool
	Z3TimeoutMs int
}

// VerifyResultGo is the internal result (violations only — warnings/timing
// are informative and not part of the wire verdict).
type VerifyResultGo struct {
	OK         bool
	Violations []Violation
	// Timeouts are the Z3 checks that answered unknown, worded as the
	// oracle's VerificationTimeout; verify() keeps them as warnings.
	Timeouts []string
}

// Verify ports verify._verify: delegation-safety -> permissions -> skill
// subsumption -> z3 self-consistency -> z3 plan-vs-envelope -> predicate ->
// robotics z3 -> dag, short-circuiting after every stage except predicate+
// robotics-z3 (both accumulate before the next short-circuit — verify.py
// Stage 2b/2c).
func Verify(plan ActionPlan, env Envelope, opts VerifyOptions) VerifyResultGo {
	strict := ResolveStrict(opts.Strict, env)

	violations := checkDelegationSafety(plan, env)
	if len(violations) > 0 {
		return result(violations)
	}

	violations = CheckPermissions(plan, env, strict)
	if len(violations) > 0 {
		return result(violations)
	}

	var warnings []string // discarded — informative only
	var timeouts []string
	violations = CheckSkillDelegations(plan, env, strict, opts.Z3TimeoutMs, &warnings, &timeouts)
	if len(violations) > 0 {
		return withTimeouts(result(violations), timeouts)
	}

	vs, to := CheckEnvelopeSelfConsistency(env, opts.Z3TimeoutMs)
	violations = append(violations, vs...)
	if to != "" {
		timeouts = append(timeouts, to)
	}
	if len(violations) > 0 {
		return withTimeouts(result(violations), timeouts)
	}
	vs, to = CheckPlanAgainstEnvelope(plan, env, opts.Z3TimeoutMs)
	violations = append(violations, vs...)
	if to != "" {
		timeouts = append(timeouts, to)
	}
	if len(violations) > 0 {
		return withTimeouts(result(violations), timeouts)
	}

	violations = append(violations, CheckPredicateInvariants(plan, env, strict, &warnings)...)
	violations = append(violations, CheckPlanInvariantsRobotics(plan, env)...)
	if len(violations) > 0 {
		return withTimeouts(result(violations), timeouts)
	}

	violations = append(violations, CheckDAG(plan)...)
	return withTimeouts(result(violations), timeouts)
}

func withTimeouts(r VerifyResultGo, timeouts []string) VerifyResultGo {
	r.Timeouts = timeouts
	return r
}

func result(violations []Violation) VerifyResultGo {
	return VerifyResultGo{OK: len(violations) == 0, Violations: violations}
}

// --- delegation safety (verify._check_delegation_safety) -----------------------

func checkDelegationSafety(plan ActionPlan, env Envelope) []Violation {
	if env.Stakes != "physical" {
		return nil
	}
	var violations []Violation
	for _, step := range plan.Steps {
		if step.Type == "agentic" {
			violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
				"Step '%s' is an agentic delegation but envelope stakes='physical'; physical-stakes plans cannot be LLM-delegated", step.ID)))
			continue
		}
		if pm, ok := step.PreferredModel(); ok && pm != "" {
			violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
				"Step '%s' requests delegation to '%s' but envelope stakes='physical'; physical-stakes plans cannot be LLM-delegated", step.ID, pm)))
		}
	}
	return violations
}

// --- permissions stage (verify.check_permissions) -------------------------------

func CheckPermissions(plan ActionPlan, env Envelope, strict bool) []Violation {
	var violations []Violation
	perms := env.Permissions

	for _, step := range plan.Steps {
		switch step.Type {
		case "shell":
			if !perms.Shell {
				violations = append(violations, VStep("permissions", step.ID,
					fmt.Sprintf("Step '%s' requires shell but envelope forbids it", step.ID)))
				continue
			}
			cmd, _ := step.ShellCommand()
			violations = append(violations, checkShellCommand(cmd, step.ID, perms, env.ShellInterpreterPolicy, 0)...)
		case "network":
			if !perms.Network {
				violations = append(violations, VStep("permissions", step.ID,
					fmt.Sprintf("Step '%s' requires network but envelope forbids it", step.ID)))
				continue
			}
			rawURL, _ := step.NetworkURL()
			u, _ := url.Parse(rawURL)
			scheme := ""
			if u != nil {
				scheme = strings.ToLower(u.Scheme)
			}
			if scheme != "http" && scheme != "https" {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' network URL scheme '%s' not allowed (only http/https); got %s", step.ID, scheme, pystr.Repr(rawURL))))
				continue
			}
			if len(perms.NetworkHosts) > 0 {
				host := ""
				if u != nil {
					host = u.Hostname()
				}
				if !hostInList(host, perms.NetworkHosts) {
					violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
						"Step '%s' network host '%s' not in network_hosts allowlist %s", step.ID, strings.ToLower(host), pystr.ReprList(perms.NetworkHosts))))
				}
			}
		case "file_read":
			path, _ := step.FilePath()
			if !PathMatchesAny(path, perms.FileRead) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' file_read path '%s' not permitted by file_read %s", step.ID, path, pystr.ReprList(perms.FileRead))))
			}
		case "file_write":
			path, _ := step.FilePath()
			if !PathMatchesAny(path, perms.FileWrite) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' file_write path '%s' not permitted by file_write %s", step.ID, path, pystr.ReprList(perms.FileWrite))))
			}
		case "mcp":
			server, _ := step.MCPServer()
			tool, _ := step.MCPTool()
			key := server + "/" + tool
			if !headAllowed(key, perms.McpAllowlist) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' MCP tool '%s' not in mcp_allowlist %s", step.ID, key, pystr.ReprList(perms.McpAllowlist))))
			}
		case "agentic":
			violations = append(violations, checkAgenticStep(step, perms)...)
		default:
			if strict && !knownStepTypes[step.Type] && !contains(perms.CustomStepAllowlist, step.Type) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' has unverifiable step type '%s' (no permission surface or handler); rejected under strict mode", step.ID, step.Type)))
			}
		}
	}
	return violations
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func hostInList(host string, hosts []string) bool {
	host = strings.ToLower(host)
	for _, h := range hosts {
		if strings.ToLower(h) == host {
			return true
		}
	}
	return false
}

func checkAgenticStep(step Step, perms Permission) []Violation {
	var violations []Violation
	tools := step.AgenticTools()
	if len(tools) == 0 {
		violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
			"Step '%s' is agentic but requests no tools — a tool-less delegated subtask is a TaskStep (pure reasoning); use that instead", step.ID)))
		return violations
	}
	workspace, _ := step.AgenticWorkspace()
	if !PathMatchesAny(workspace, perms.FileRead) {
		violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
			"Step '%s' agentic workspace '%s' is not inside the envelope's file_read globs %s — a sub-agent must be able to read its own working directory",
			step.ID, workspace, pystr.ReprList(perms.FileRead))))
	}
	for _, tool := range tools {
		cap, ok := agenticToolCapabilities[tool]
		if !ok {
			violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
				"Step '%s' requests host tool '%s' which has no capability mapping — denied by default", step.ID, tool)))
			continue
		}
		if !capabilityGranted(perms, cap) {
			violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
				"Step '%s' requests host tool '%s' but the envelope grants no %s capability", step.ID, tool, cap)))
		}
	}
	return violations
}

func capabilityGranted(perms Permission, cap string) bool {
	switch cap {
	case "shell":
		return perms.Shell
	case "file_read":
		return len(perms.FileRead) > 0
	case "file_write":
		return len(perms.FileWrite) > 0
	case "network":
		return perms.Network
	}
	return false
}

// --- shell command checking (verify._check_shell_command et al) -----------------

func checkShellCommand(command, stepID string, perms Permission, policy string, depth int) []Violation {
	if depth > maxInterpreterDepth {
		return []Violation{VStep("permissions", stepID, fmt.Sprintf(
			"Step '%s' interpreter recursion exceeded max depth %d", stepID, maxInterpreterDepth))}
	}
	stripped := strings.TrimSpace(command)
	if stripped == "" {
		return nil
	}
	if hasShellMetachar(command) {
		if perms.ShellAllowDecomposition && depth <= maxInterpreterDepth {
			decomp := DecomposeCommand(command)
			if decomp.OK {
				// A decomposed piece is guaranteed metachar-free by
				// construction; call verifySimpleCommand directly (NOT
				// checkShellCommand) — the raw metachar gate is
				// deliberately not re-run on decomposed parts (verify.py
				// comment), and re-running it here would both diverge
				// from the oracle on quoted metachars (e.g. `grep -E
				// "a|b"`) AND recurse forever at a constant depth.
				var violations []Violation
				violations = append(violations, checkRedirectScopes(decomp, stepID, perms)...)
				for _, simple := range decomp.Commands {
					violations = append(violations, verifySimpleCommand(simple, stepID, perms, policy, depth)...)
				}
				return violations
			}
			// decomposition refused -> falls through to the metachar
			// violation below, same as the oracle.
		}
		return []Violation{VStep("permissions", stepID, fmt.Sprintf(
			"Step '%s' shell command contains dangerous metacharacters (;, |, &, `, <, >, $(, newline)", stepID)+atDepth(depth))}
	}
	return verifySimpleCommand(stripped, stepID, perms, policy, depth)
}

func checkRedirectScopes(decomp Decomposition, stepID string, perms Permission) []Violation {
	var violations []Violation
	for _, path := range decomp.Writes {
		if sanctionedWriteSinks[path] {
			continue
		}
		if !PathMatchesAny(path, perms.FileWrite) {
			violations = append(violations, VStep("permissions", stepID, fmt.Sprintf(
				"Step '%s' shell redirect writes '%s' outside file_write scope %s", stepID, path, pystr.ReprList(perms.FileWrite))))
		}
	}
	for _, path := range decomp.Reads {
		if sanctionedReadSources[path] {
			continue
		}
		if !PathMatchesAny(path, perms.FileRead) {
			violations = append(violations, VStep("permissions", stepID, fmt.Sprintf(
				"Step '%s' shell redirect reads '%s' outside file_read scope %s", stepID, path, pystr.ReprList(perms.FileRead))))
		}
	}
	return violations
}

func verifySimpleCommand(command, stepID string, perms Permission, policy string, depth int) []Violation {
	var violations []Violation
	stripped := strings.TrimSpace(command)
	head, hasHead := extractShellHead(stripped)
	if !hasHead {
		return violations
	}
	if !headAllowed(head, perms.ShellAllowlist) {
		return []Violation{VStep("permissions", stepID, fmt.Sprintf(
			"Step '%s' shell command '%s' not in allowlist %s", stepID, head, pystr.ReprList(perms.ShellAllowlist))+atDepth(depth))}
	}
	payload, ok := ParseInterpreter(command)
	if !ok {
		return violations
	}
	if payload.Opaque {
		if policy == "strict" {
			violations = append(violations, VStep("permissions", stepID, fmt.Sprintf(
				"Step '%s' invokes opaque interpreter '%s' whose payload cannot be recursively verified (strict shell_interpreter_policy rejects)", stepID, payload.Head)))
		}
		return violations
	}
	for _, inner := range payload.InnerCommands {
		violations = append(violations, checkShellCommand(inner, stepID, perms, policy, depth+1)...)
	}
	return violations
}

// atDepth is the suffix a message carries inside an interpreter payload.
func atDepth(depth int) string {
	if depth > 0 {
		return fmt.Sprintf(" (inside interpreter at depth %d)", depth)
	}
	return ""
}
