package verify

import (
	"fmt"
	"net/url"
	"strings"

	"daisugi-verify/internal/pyjson"
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
	// sprig's spawn tool: the warden sees it as mcp__sprig__spawn, which
	// the envelope admits only when mcp_allowlist names sprig/spawn.
	"Spawn": "mcp_allowlist",
}

// SpawnMCPKey is verify.SPAWN_MCP_KEY: the mcp_allowlist key of the Spawn
// tool, as the warden keys mcp__sprig__spawn.
const SpawnMCPKey = "sprig/spawn"

// AgenticToolGranted is verify.agentic_tool_granted: whether the envelope
// backs a host tool an agentic step asks for. Spawn needs its own name in
// mcp_allowlist: any other entry is not it.
func AgenticToolGranted(tool string, perms Permission) bool {
	capName, ok := agenticToolCapabilities[tool]
	if !ok {
		return false
	}
	if tool == "Spawn" {
		return headAllowed(SpawnMCPKey, perms.McpAllowlist)
	}
	return capabilityGranted(perms, capName)
}

// AgenticToolCapability is the permission a host tool needs, and whether
// it has one.
func AgenticToolCapability(tool string) (string, bool) {
	c, ok := agenticToolCapabilities[tool]
	return c, ok
}

// VerifyOptions mirrors the verify()/kwargs surface the conformance corpus
// exercises.
type VerifyOptions struct {
	Strict      *bool
	Z3TimeoutMs int
	// DialectPin is verify(dialect_pin=...): nil keeps the words in audit.
	DialectPin *string
	// DialectBase is verify(dialect_base=...): the directory a word is
	// placed from; nil, or a path that is not absolute, places nothing.
	DialectBase *string
	// Fenced is verify(fenced=...): the call runs under a fence run, so the
	// shell-access findings are warnings that start with FencePrefix, not
	// violations (ADR-0028).
	Fenced bool
}

// FencePrefix is verify.FENCE_PREFIX: the start of a warning that records a
// shell file access a fenced call made, which the kernel fence stops.
const FencePrefix = "fence-enforced: "

// VerifyResultGo is the internal result (violations only — warnings/timing
// are informative and not part of the wire verdict).
type VerifyResultGo struct {
	OK         bool
	Violations []Violation
	// Timeouts are the Z3 checks that answered unknown, worded as the
	// oracle's VerificationTimeout; verify() keeps them as warnings.
	Timeouts []string
	// Warnings is VerificationResult.warnings, in the oracle's order and
	// words. WarningsUnmodeled is set when the oracle may add a warning
	// this port does not compute (a skill delegation with a contract that
	// holds may name unverified invariants): a caller that stores the
	// warnings must then refuse.
	Warnings          []string
	WarningsUnmodeled bool
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

	var fenceLog *[]string
	if opts.Fenced {
		fenceLog = &[]string{}
	}
	violations = CheckPermissionsFenced(plan, env, strict, DialectBase(opts.DialectBase), fenceLog)
	var warnings []string
	if fenceLog != nil {
		for _, m := range *fenceLog {
			warnings = append(warnings, FencePrefix+m)
		}
	}
	if len(violations) > 0 {
		r := result(violations)
		r.Warnings = warnings
		return r
	}

	var timeouts []string
	unmodeled := false
	done := func(vs []Violation) VerifyResultGo {
		r := result(vs)
		r.Timeouts, r.Warnings, r.WarningsUnmodeled = timeouts, warnings, unmodeled
		return r
	}
	violations = CheckSkillDelegations(plan, env, strict, opts.Z3TimeoutMs, &warnings, &timeouts, &unmodeled)
	if len(violations) > 0 {
		return done(violations)
	}

	vs, to := CheckEnvelopeSelfConsistency(env, opts.Z3TimeoutMs)
	violations = append(violations, vs...)
	if to != "" {
		timeouts = append(timeouts, to)
		if len(vs) == 0 {
			warnings = append(warnings, to)
		}
	}
	if len(violations) > 0 {
		return done(violations)
	}
	vs, to = CheckPlanAgainstEnvelope(plan, env, opts.Z3TimeoutMs)
	violations = append(violations, vs...)
	if to != "" {
		timeouts = append(timeouts, to)
		if len(vs) == 0 {
			warnings = append(warnings, to)
		}
	}
	if len(violations) > 0 {
		return done(violations)
	}

	violations = append(violations, CheckPredicateInvariantsPin(plan, env, strict, opts.DialectPin, DialectBase(opts.DialectBase), &warnings)...)
	violations = append(violations, CheckPlanInvariantsRobotics(plan, env)...)
	if len(violations) > 0 {
		return done(violations)
	}

	violations = append(violations, CheckDAG(plan)...)
	return done(violations)
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
	return CheckPermissionsAt(plan, env, strict, "")
}

// CheckPermissionsAt is verify.check_permissions with its base: the
// directory a shell step's relative accesses are placed from ("" for none).
func CheckPermissionsAt(plan ActionPlan, env Envelope, strict bool, base string) []Violation {
	return CheckPermissionsFenced(plan, env, strict, base, nil)
}

// NamesNetDevice is verify._names_net_device on a shell-access message: the
// finding is for a path under /dev/tcp or /dev/udp. Bash opens those with
// socket and connect, which the kernel fence never sees, so a fenced call
// still decides them (ADR-0028). The path is the quoted text before
// " outside ".
func NamesNetDevice(message string) bool {
	head, _, _ := strings.Cut(message, " outside ")
	for _, dev := range []string{"/dev/tcp", "/dev/udp"} {
		if strings.Contains(head, "'"+dev+"/") || strings.Contains(head, "'"+dev+"'") {
			return true
		}
	}
	return false
}

// CheckPermissionsFenced is check_permissions with its fence_log: when it
// is not nil, the shell-access findings go into it as messages and are not
// violations (ADR-0028).
func CheckPermissionsFenced(plan ActionPlan, env Envelope, strict bool, base string, fenceLog *[]string) []Violation {
	var violations []Violation
	perms := env.Permissions

	for _, step := range plan.Steps {
		switch step.Type {
		case "shell":
			if !perms.Shell {
				violations = append(violations, VStep("permissions", step.ID,
					fmt.Sprintf("Step '%s' requires shell but envelope forbids it", step.ID)).With(kv("step", step.ID), nil))
				continue
			}
			cmd, _ := step.ShellCommand()
			shellViolations := checkShellCommand(cmd, step.ID, perms, env.ShellInterpreterPolicy, 0)
			violations = append(violations, shellViolations...)
			// A head the allowlist refused already denies the step; naming
			// its files too would only repeat it.
			if !hasHeadViolation(shellViolations) {
				accesses := checkShellAccesses(cmd, step.ID, perms, base)
				if fenceLog == nil {
					violations = append(violations, accesses...)
				} else {
					for _, a := range accesses {
						// The fence does not limit the network (ADR-0028).
						if NamesNetDevice(a.Message) {
							violations = append(violations, a)
						} else {
							*fenceLog = append(*fenceLog, a.Message)
						}
					}
				}
			}
		case "network":
			if !perms.Network {
				violations = append(violations, VStep("permissions", step.ID,
					fmt.Sprintf("Step '%s' requires network but envelope forbids it", step.ID)).With(kv("step", step.ID), nil))
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
					"Step '%s' network URL scheme '%s' not allowed (only http/https); got %s", step.ID, scheme, pystr.Repr(rawURL))).
					With(kv("step", step.ID, "scheme", scheme, "url", rawURL), nil))
				continue
			}
			if len(perms.NetworkHosts) > 0 {
				host := ""
				if u != nil {
					host = u.Hostname()
				}
				if !hostInList(host, perms.NetworkHosts) {
					violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
						"Step '%s' network host '%s' not in network_hosts allowlist %s", step.ID, strings.ToLower(host), pystr.ReprList(perms.NetworkHosts))).
						With(kv("step", step.ID, "host", strings.ToLower(host), "url", rawURL), nil))
				}
			}
		case "file_read":
			path, _ := step.FilePath()
			if !PathMatchesAny(path, perms.FileRead) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' file_read path '%s' not permitted by file_read %s", step.ID, path, pystr.ReprList(perms.FileRead))).
					With(kv("step", step.ID, "path", path), nil))
			}
		case "file_write":
			path, _ := step.FilePath()
			if !PathMatchesAny(path, perms.FileWrite) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' file_write path '%s' not permitted by file_write %s", step.ID, path, pystr.ReprList(perms.FileWrite))).
					With(kv("step", step.ID, "path", path), nil))
			}
		case "mcp":
			server, _ := step.MCPServer()
			tool, _ := step.MCPTool()
			violations = append(violations, checkMCPStep(step, server, tool, perms, base)...)
		case "agentic":
			violations = append(violations, checkAgenticStep(step, perms)...)
		default:
			if strict && !knownStepTypes[step.Type] && !contains(perms.CustomStepAllowlist, step.Type) {
				violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
					"Step '%s' has unverifiable step type '%s' (no permission surface or handler); rejected under strict mode", step.ID, step.Type)).
					With(kv("step", step.ID, "type", step.Type), nil))
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
			"Step '%s' is agentic but requests no tools — a tool-less delegated subtask is a TaskStep (pure reasoning); use that instead", step.ID)).
			With(kv("step", step.ID), nil))
		return violations
	}
	workspace, _ := step.AgenticWorkspace()
	if !PathMatchesAny(workspace, perms.FileRead) {
		violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
			"Step '%s' agentic workspace '%s' is not inside the envelope's file_read globs %s — a sub-agent must be able to read its own working directory",
			step.ID, workspace, pystr.ReprList(perms.FileRead))).With(kv("step", step.ID, "workspace", workspace), nil))
	}
	for _, tool := range tools {
		cap, ok := agenticToolCapabilities[tool]
		if !ok {
			violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
				"Step '%s' requests host tool '%s' which has no capability mapping — denied by default", step.ID, tool)).
				With(kv("step", step.ID, "tool", tool), nil))
			continue
		}
		if !AgenticToolGranted(tool, perms) {
			violations = append(violations, VStep("permissions", step.ID, fmt.Sprintf(
				"Step '%s' requests host tool '%s' but the envelope grants no %s capability", step.ID, tool, cap)).
				With(kv("step", step.ID, "tool", tool, "capability", cap), nil))
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
			"Step '%s' interpreter recursion exceeded max depth %d", stepID, maxInterpreterDepth)).
			With(kv("step", stepID, "command", command, "depth", depth), nil)}
	}
	stripped := strings.TrimSpace(command)
	if stripped == "" {
		return nil
	}
	if hasShellMetachar(command) {
		refused := ""
		if perms.ShellAllowDecomposition && depth <= maxInterpreterDepth {
			decomp := DecomposeCommand(command)
			if decomp.OK {
				// A decomposed piece is guaranteed metachar-free by
				// construction; call verifySimpleCommand directly (NOT
				// checkShellCommand) — the raw metachar warden is
				// deliberately not re-run on decomposed parts (verify.py
				// comment), and re-running it here would both diverge
				// from the oracle on quoted metachars (e.g. `grep -E
				// "a|b"`) AND recurse forever at a constant depth.
				var violations []Violation
				for _, simple := range decomp.Commands {
					violations = append(violations, verifySimpleCommand(simple, stepID, perms, policy, depth)...)
				}
				return violations
			}
			// decomposition refused -> falls through to the metachar
			// violation below, same as the oracle.
			refused = decomp.Reason
		}
		detail := kv("step", stepID, "command", command, "depth", depth)
		if refused != "" {
			detail.Set("decomposition_refused", refused)
		}
		return []Violation{VStep("permissions", stepID, fmt.Sprintf(
			"Step '%s' shell command contains dangerous metacharacters (;, |, &, `, <, >, $(, newline)", stepID)+atDepth(depth)).
			With(detail, decompositionRemediation(stepID, command))}
	}
	return verifySimpleCommand(stripped, stepID, perms, policy, depth)
}

func hasHeadViolation(vs []Violation) bool {
	for _, v := range vs {
		if v.Detail != nil {
			if _, ok := v.Detail.Get("command_head"); ok {
				return true
			}
		}
	}
	return false
}

// CoversEverything is verify.covers_everything: a scope glob that is exactly
// ** or /**.
func CoversEverything(globs []string) bool {
	for _, g := range globs {
		if g == "**" || g == "/**" {
			return true
		}
	}
	return false
}

// matchGlobSafe is matchGlob where a glob too complex to match matches nothing.
func matchGlobSafe(norm, glob string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, is := r.(GlobTooComplex); is {
				ok = false
				return
			}
			panic(r)
		}
	}()
	return matchGlob(norm, glob)
}

// CoversTree is verify.covers_tree: dir and everything under it is in scope
// only under a glob that ends in /** (SG-2).
func CoversTree(dir string, globs []string) bool {
	if CoversEverything(globs) {
		return true
	}
	norm := posixNormpath(dir)
	for _, g := range globs {
		if !strings.HasSuffix(g, "/**") {
			continue
		}
		if matchGlobSafe(norm, g) || matchGlobSafe(norm, g[:len(g)-3]) {
			return true
		}
	}
	return false
}

// checkShellAccesses is verify._check_shell_accesses: every file a shell
// line reads or writes faces the envelope's file scopes (SG-1 to SG-6).
func checkShellAccesses(command, stepID string, perms Permission, base string) []Violation {
	acc := ShellAccesses(command, base)
	var violations []Violation
	for _, side := range []struct {
		verb, scopeName, noun string
		accesses              []Access
		scope                 []string
	}{
		{"writes", "file_write", "write", acc.Writes, perms.FileWrite},
		{"reads", "file_read", "read", acc.Reads, perms.FileRead},
	} {
		for _, a := range side.accesses {
			var msg string
			var detail *pyjson.Object
			var remediation string
			if a.Tree {
				if CoversTree(a.Path, side.scope) {
					continue
				}
				msg = fmt.Sprintf("Step '%s' shell %s under '%s' (recursive) outside %s scope %s",
					stepID, side.verb, a.Path, side.scopeName, pystr.ReprList(side.scope))
				detail = kv("step", stepID, "shell_"+side.noun+"_tree", a.Path)
				remediation = "Add '" + a.Path + "/**' to permissions." + side.scopeName
			} else {
				if PathMatchesAny(a.Path, side.scope) {
					continue
				}
				if a.Via == "redirect" {
					msg = fmt.Sprintf("Step '%s' shell redirect %s '%s' outside %s scope %s",
						stepID, side.verb, a.Path, side.scopeName, pystr.ReprList(side.scope))
					detail = kv("step", stepID, "redirect_"+side.noun, a.Path)
				} else {
					msg = fmt.Sprintf("Step '%s' shell %s '%s' outside %s scope %s",
						stepID, side.verb, a.Path, side.scopeName, pystr.ReprList(side.scope))
					detail = kv("step", stepID, "shell_"+side.noun, a.Path)
				}
				remediation = "Add '" + a.Path + "' (or a covering glob) to permissions." + side.scopeName
			}
			violations = append(violations, VStep("permissions", stepID, msg).With(detail, strp(remediation)))
		}
	}
	for _, side := range []struct {
		noun, scopeName, unknown string
		scope                    []string
	}{
		{"write", "file_write", acc.WritesUnknown, perms.FileWrite},
		{"read", "file_read", acc.ReadsUnknown, perms.FileRead},
	} {
		if side.unknown == "" || CoversEverything(side.scope) {
			continue
		}
		violations = append(violations, VStep("permissions", stepID, fmt.Sprintf(
			"Step '%s' shell %s targets cannot be placed (%s) and %s scope %s is not everything",
			stepID, side.noun, side.unknown, side.scopeName, pystr.ReprList(side.scope))).
			With(kv("step", stepID, "shell_"+side.noun+"s_unknown", side.unknown),
				strp("Name each file the command "+side.noun+"s, without xargs, globs or variables")))
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
			"Step '%s' shell command '%s' not in allowlist %s", stepID, head, pystr.ReprList(perms.ShellAllowlist))+atDepth(depth)).
			With(kv("step", stepID, "command_head", head, "depth", depth), strp("Add '"+head+"' to permissions.shell_allowlist"))}
	}
	payload, ok := ParseInterpreter(command)
	if !ok {
		return violations
	}
	if payload.Opaque {
		if policy == "strict" {
			violations = append(violations, VStep("permissions", stepID, fmt.Sprintf(
				"Step '%s' invokes opaque interpreter '%s' whose payload cannot be recursively verified (strict shell_interpreter_policy rejects)", stepID, payload.Head)).
				With(kv("step", stepID, "interpreter", payload.Head), nil))
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

// decompositionRemediation is _build_decomposition_remediation: for a
// command that splits on top-level &&, || and ; into pieces with no $( or
// backtick, the sequential ShellSteps it could be written as; else nil.
func decompositionRemediation(stepID, command string) *string {
	parts := SplitCompoundShell(command)
	if len(parts) <= 1 {
		return nil
	}
	for _, p := range parts {
		if strings.Contains(p, "$(") || strings.Contains(p, "`") {
			return nil
		}
	}
	lines := make([]string, len(parts))
	for i, p := range parts {
		dep := ""
		if i > 0 {
			dep = fmt.Sprintf(", depends_on=['%s_d%d']", stepID, i-1)
		}
		lines[i] = fmt.Sprintf("  ShellStep(id='%s_d%d', command=%s%s)", stepID, i, pystr.Repr(p), dep)
	}
	s := "Decompose into sequential ShellSteps:\n" + strings.Join(lines, "\n")
	return &s
}

// SplitCompoundShell is parsers.claude_code._split_compound_shell: the
// command split on &&, || and ; outside quotes, each part stripped, empty
// parts dropped. It does not look inside $( or backticks.
func SplitCompoundShell(command string) []string {
	if command == "" {
		return nil
	}
	rs := []rune(command)
	var parts []string
	var cur []rune
	inSingle, inDouble := false, false
	flush := func() {
		parts = append(parts, pystr.Strip(string(cur)))
		cur = cur[:0]
	}
	for i := 0; i < len(rs); {
		c := rs[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur = append(cur, c)
			i++
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur = append(cur, c)
			i++
			continue
		}
		if !inSingle && !inDouble {
			switch {
			case c == ';':
				flush()
				i++
				continue
			case c == '&' && i+1 < len(rs) && rs[i+1] == '&':
				flush()
				i += 2
				continue
			case c == '|' && i+1 < len(rs) && rs[i+1] == '|':
				flush()
				i += 2
				continue
			}
		}
		cur = append(cur, c)
		i++
	}
	if tail := pystr.Strip(string(cur)); tail != "" {
		parts = append(parts, tail)
	}
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
