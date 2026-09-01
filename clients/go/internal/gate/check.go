package gate

import (
	"fmt"
	"os"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/shell"
	"daisugi-verify/internal/verify"
)

// violation is models.Violation.
type violation struct {
	Stage       string
	Message     string
	Detail      *pyjson.Object
	Remediation any // string or nil
}

func (v violation) obj() *pyjson.Object {
	return pyjson.NewObject().
		Set("stage", v.Stage).
		Set("message", v.Message).
		Set("detail", v.Detail).
		Set("suggested_remediation", v.Remediation)
}

func kv(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

const maxInterpreterDepth = 4

var (
	sanctionedWriteSinks  = map[string]bool{"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true}
	sanctionedReadSources = map[string]bool{"/dev/null": true, "/dev/stdin": true}
)

func depthSuffix(depth int) string {
	if depth == 0 {
		return ""
	}
	return fmt.Sprintf(" (inside interpreter at depth %d)", depth)
}

// verifyRecord is verify.verify(plan, envelope, strict=None) for the
// one-step plan the gate builds from a record, for envelopes with no
// invariants, postconditions or robotics bounds. The two Z3 stages it
// runs are ground constraints, decided here by inspection.
func (r *runner) verifyRecord(rec *record, env *envelope) []violation {
	const step = "s0"
	var vs []violation
	switch rec.StepType {
	case "shell":
		if !env.Shell {
			return []violation{{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' requires shell but envelope forbids it", step),
				Detail:  kv("step", step)}}
		}
		vs = r.checkShellCommand(rec.Command, step, env, 0)
	case "network":
		vs = checkNetwork(rec.URL, step, env)
	case "file_read":
		if !r.pathMatches(rec.Path, env.FileRead) {
			vs = append(vs, violation{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' file_read path '%s' not permitted by file_read %s",
					step, rec.Path, pyjson.ReprList(env.FileRead)),
				Detail: kv("step", step, "path", rec.Path)})
		}
	case "file_write":
		if !r.pathMatches(rec.Path, env.FileWrite) {
			vs = append(vs, violation{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' file_write path '%s' not permitted by file_write %s",
					step, rec.Path, pyjson.ReprList(env.FileWrite)),
				Detail: kv("step", step, "path", rec.Path)})
		}
	case "mcp":
		key := rec.MCPServer + "/" + rec.MCPTool
		if !verify.HeadAllowed(key, env.McpAllowlist) {
			vs = append(vs, violation{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' MCP tool '%s' not in mcp_allowlist %s",
					step, key, pyjson.ReprList(env.McpAllowlist)),
				Detail: kv("step", step, "mcp_tool", key)})
		}
	}
	if len(vs) > 0 {
		return vs
	}
	// The two Z3 stages are ground: every term is a pinned constant, so
	// Z3 answers sat or unsat at once and never unknown. They are decided
	// here directly; z3stages_test.go asks Z3 the oracle's own formulas
	// over every combination and checks the two agree.
	if !selfConsistent(env) {
		return []violation{{Stage: "z3", Message: "Envelope is internally inconsistent",
			Detail: kv("unsat_core", "[]")}}
	}
	if !planReachable(rec.StepType, env) {
		return []violation{{Stage: "z3", Message: "Plan requirements contradict envelope permissions",
			Detail: kv("unsat_core", "[]")}}
	}
	// Stage 2b: predicate invariants and postconditions.
	if vs := r.checkPredicates(rec, env); len(vs) > 0 {
		return vs
	}
	// Stage 2c, z3_checks.check_plan_invariants: each robotics handler
	// reads only CartesianMoveStep, JointMoveStep and VLAStep targets, and
	// a gate plan holds none, so it finds nothing. Stage 3, the DAG: one
	// step with no dependencies passes.
	return nil
}

// selfConsistent is z3_checks.check_envelope_self_consistency decided
// directly: shell == perms.shell, can_write == bool(file_write), a
// non-empty allowlist asserts shell, a file_exists postcondition asserts
// can_write, and 0 < max_execution_time_s <= 3600.
func selfConsistent(env *envelope) bool {
	if len(env.ShellAllowlist) > 0 && !env.Shell {
		return false
	}
	for _, pc := range env.Postconditions {
		if pc.Type == "file_exists" && len(env.FileWrite) == 0 {
			return false
		}
	}
	return env.maxTimeIn()
}

// planReachable is z3_checks.check_plan_against_envelope decided
// directly for the gate's one step.
func planReachable(stepType string, env *envelope) bool {
	if stepType == "shell" && !env.Shell {
		return false
	}
	return !(stepType == "file_write" && len(env.FileWrite) == 0)
}

func checkNetwork(rawURL, step string, env *envelope) []violation {
	if !env.Network {
		return []violation{{Stage: "permissions",
			Message: fmt.Sprintf("Step '%s' requires network but envelope forbids it", step),
			Detail:  kv("step", step)}}
	}
	scheme, netloc := urlSplit(rawURL)
	scheme = pystr.Lower(scheme)
	if scheme != "http" && scheme != "https" {
		return []violation{{Stage: "permissions",
			Message: fmt.Sprintf("Step '%s' network URL scheme '%s' not allowed (only http/https); got %s",
				step, scheme, pyjson.Repr(rawURL)),
			Detail: kv("step", step, "scheme", scheme, "url", rawURL)}}
	}
	if len(env.NetworkHosts) > 0 {
		_, netloc = urlSplit(rawURL)
		host := urlHostname(netloc)
		found := false
		for _, h := range env.NetworkHosts {
			if pystr.Lower(h) == host {
				found = true
			}
		}
		if !found {
			return []violation{{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' network host '%s' not in network_hosts allowlist %s",
					step, host, pyjson.ReprList(env.NetworkHosts)),
				Detail: kv("step", step, "host", host, "url", rawURL)}}
		}
	}
	return nil
}

func isAlphaByte(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// shellHead is verify._extract_shell_head.
func shellHead(stripped string) (string, bool) {
	if stripped == "" || strings.HasPrefix(stripped, "#") {
		return "", false
	}
	for _, tok := range pySplit(stripped) {
		if isEnvAssign(tok) {
			continue
		}
		return tok, true
	}
	return "", false
}

// isEnvAssign is verify._ENV_ASSIGN_RE.match.
func isEnvAssign(tok string) bool {
	if tok == "" {
		return false
	}
	c := tok[0]
	if !(isAlphaByte(c) || c == '_') {
		return false
	}
	for i := 1; i < len(tok); i++ {
		c := tok[i]
		if c == '=' {
			return true
		}
		if !(isAlphaByte(c) || c == '_' || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return false
}

// checkShellCommand is verify._check_shell_command.
func (r *runner) checkShellCommand(command, step string, env *envelope, depth int) []violation {
	if depth > maxInterpreterDepth {
		return []violation{{Stage: "permissions",
			Message: fmt.Sprintf("Step '%s' interpreter recursion exceeded max depth %d", step, maxInterpreterDepth),
			Detail:  kv("step", step, "command", command, "depth", depth)}}
	}
	stripped := pyStrip(command)
	if stripped == "" {
		return nil
	}
	if !verify.HasShellMetachar(command) {
		return r.verifySimpleCommand(stripped, step, env, depth)
	}
	refused := ""
	if env.ShellAllowDecomposition {
		// _check_shell_command runs at frame 9 in the verifier's worker
		// thread, two frames deeper per interpreter level.
		d, exc := r.decomposeAt(command, 10+2*depth)
		if exc != nil {
			panic(exc)
		}
		if d.OK {
			vs := r.checkRedirectScopes(d, step, env)
			for _, simple := range d.Commands {
				vs = append(vs, r.verifySimpleCommand(simple, step, env, depth)...)
			}
			return vs
		}
		refused = d.Reason
	}
	detail := kv("step", step, "command", command, "depth", depth)
	if refused != "" {
		detail.Set("decomposition_refused", refused)
	}
	return []violation{{Stage: "permissions",
		Message: fmt.Sprintf("Step '%s' shell command contains dangerous metacharacters (;, |, &, `, <, >, $(, newline)", step) +
			depthSuffix(depth),
		Detail:      detail,
		Remediation: decompositionRemediation(step, command)}}
}

func (r *runner) checkRedirectScopes(d shell.Decomposition, step string, env *envelope) []violation {
	var vs []violation
	for _, p := range d.Writes {
		if sanctionedWriteSinks[p] {
			continue
		}
		if !r.pathMatches(p, env.FileWrite) {
			vs = append(vs, violation{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' shell redirect writes '%s' outside file_write scope %s",
					step, p, pyjson.ReprList(env.FileWrite)),
				Detail:      kv("step", step, "redirect_write", p),
				Remediation: fmt.Sprintf("Add '%s' (or a covering glob) to permissions.file_write", p)})
		}
	}
	for _, p := range d.Reads {
		if sanctionedReadSources[p] {
			continue
		}
		if !r.pathMatches(p, env.FileRead) {
			vs = append(vs, violation{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' shell redirect reads '%s' outside file_read scope %s",
					step, p, pyjson.ReprList(env.FileRead)),
				Detail:      kv("step", step, "redirect_read", p),
				Remediation: fmt.Sprintf("Add '%s' (or a covering glob) to permissions.file_read", p)})
		}
	}
	return vs
}

// verifySimpleCommand is verify._verify_simple_command.
func (r *runner) verifySimpleCommand(command, step string, env *envelope, depth int) []violation {
	stripped := pyStrip(command)
	head, ok := shellHead(stripped)
	if !ok {
		return nil
	}
	if !verify.HeadAllowed(head, env.ShellAllowlist) {
		return []violation{{Stage: "permissions",
			Message: fmt.Sprintf("Step '%s' shell command '%s' not in allowlist %s", step, head,
				pyjson.ReprList(env.ShellAllowlist)) + depthSuffix(depth),
			Detail:      kv("step", step, "command_head", head, "depth", depth),
			Remediation: fmt.Sprintf("Add '%s' to permissions.shell_allowlist", head)}}
	}
	payload, ok := verify.ParseInterpreter(command)
	if !ok {
		return nil
	}
	if payload.Opaque {
		if env.Policy == "strict" {
			return []violation{{Stage: "permissions",
				Message: fmt.Sprintf("Step '%s' invokes opaque interpreter '%s' whose payload cannot be recursively verified (strict shell_interpreter_policy rejects)",
					step, payload.Head),
				Detail: kv("step", step, "interpreter", payload.Head)}}
		}
		return nil
	}
	var vs []violation
	for _, inner := range payload.InnerCommands {
		vs = append(vs, r.checkShellCommand(inner, step, env, depth+1)...)
	}
	return vs
}

// decompositionRemediation is verify._build_decomposition_remediation.
func decompositionRemediation(step, command string) any {
	parts := splitCompoundShell(command)
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
		depends := ""
		if i > 0 {
			depends = fmt.Sprintf(", depends_on=['%s_d%d']", step, i-1)
		}
		lines[i] = fmt.Sprintf("  ShellStep(id='%s_d%d', command=%s%s)", step, i, pyjson.Repr(p), depends)
	}
	return "Decompose into sequential ShellSteps:\n" + strings.Join(lines, "\n")
}

// splitCompoundShell is parsers.claude_code._split_compound_shell.
func splitCompoundShell(command string) []string {
	if command == "" {
		return nil
	}
	var parts []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		parts = append(parts, pyStrip(cur.String()))
		cur.Reset()
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(c)
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteByte(c)
			continue
		}
		if !inSingle && !inDouble {
			if c == ';' {
				flush()
				continue
			}
			if (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c {
				flush()
				i++
				continue
			}
		}
		cur.WriteByte(c)
	}
	if tail := pyStrip(cur.String()); tail != "" {
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

// decomposeAt is shell_decompose.decompose_command, its frame at Python
// depth depth. A Python exception the oracle would raise comes back as err.
func (r *runner) decomposeAt(command string, depth int) (shell.Decomposition, *pystr.Exception) {
	r.depthLog("decompose", depth, pystr.Slice(command, 0, 40))
	d, exc := r.shellCache.Decompose(command, depth)
	if exc == nil {
		r.recordDecompose(command, d)
	}
	return d, exc
}

// decompose is decompose_command called from the main thread's current
// frame.
func (r *runner) decompose(command string) (shell.Decomposition, *pystr.Exception) {
	return r.decomposeAt(command, r.depth+1)
}

// depthLog writes one line per decompose_command and realpath call to
// DAISUGI_GATE_DEPTH_LOG, the frame depth the oracle would call it at,
// for comparison with the oracle's own log of the same calls.
func (r *runner) depthLog(kind string, depth int, arg string) {
	path := r.env["DAISUGI_GATE_DEPTH_LOG"]
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	key := "cmd"
	if kind == "realpath" {
		key = "path"
	}
	f.WriteString(pyjson.Dumps(kv("kind", kind, "depth", depth, key, arg), true) + "\n")
}
