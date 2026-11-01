package warden

import (
	"math/big"
	"strconv"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/spawn"
)

// delegateEnvelope is what the router reads of the envelope.
func delegateEnvelope(env *envelope) *spawn.Envelope {
	return &spawn.Envelope{Stakes: env.Stakes, Network: env.Network, NetworkHosts: env.NetworkHosts}
}

// actingRule is spawn.acting_rule over this warden root. A rule file
// pyjson does not model hands the call back.
func (r *runner) actingRule() *spawn.Rule {
	rule, err := spawn.ActingRule(r.root)
	if err != nil {
		unported("a graft rule file the port's JSON reader does not model")
	}
	return rule
}

// routeSpawn is spawn.route_spawn from this warden root's data dir.
func (r *runner) routeSpawn(env *envelope, allowRemote bool) spawn.Route {
	rt, err := spawn.RouteSpawn(pathParent(r.root), delegateEnvelope(env), allowRemote, r.envGet)
	if err != nil {
		unported("a local_tier1.json the port's JSON reader does not model")
	}
	return rt
}

// syntheticRecord is the one-field record _decide_delegate_call builds.
func (r *runner) syntheticRecord(p *pyjson.Object, toolName, stepType, key, value string) *record {
	rec := &record{ToolName: toolName, StepType: stepType, obj: pyjson.NewObject()}
	rec.obj.Set("captured_at", nil)
	rec.obj.Set("session_id", r.safeSession(p.Value("session_id")))
	rec.obj.Set("tool_name", toolName)
	rec.obj.Set("step_type", stepType)
	rec.obj.Set(key, value)
	if key == "path" {
		rec.Path, rec.PathRaw = value, value
	} else {
		rec.URL, rec.URLRaw = value, value
	}
	j := joinOf(p)
	for _, k := range j.Keys() {
		rec.obj.Set(k, j.Value(k))
	}
	return rec
}

// decideSpawnCall is warden._decide_delegate_call: an allowed spawn
// MCP call must also pass as a read of its absolute path, and as a
// network send when the router's worker is remote.
func (r *runner) decideSpawnCall(p *pyjson.Object, rec *record, d *decision, env *envelope,
	root *workspace, callCwd *string) *decision {
	defer r.enter()()
	toolName := rec.ToolName
	path, isStr := rec.Arguments.Value("path").(string)
	if !isStr || !isabs(path) {
		out := r.deny("spawn: the path must be an absolute path, so the warden checks the file " +
			"the spawn reads")
		out.ToolName, out.StepType, out.Detail = toolName, "mcp", path
		return out
	}
	if env.Stakes == "physical" {
		out := r.deny("spawn: the spawn is refused under physical stakes")
		out.ToolName, out.StepType, out.Detail = toolName, "mcp", path
		return out
	}
	norm := normpath(path)
	// A code write reads its target only when the target exists.
	if m, _ := rec.Arguments.Value("mode").(string); m == "code_write" && !spawn.Lexists(norm) {
		return r.spawnSend(p, d, env, root, callCwd, toolName, norm)
	}
	got := r.evaluateRecord(r.syntheticRecord(p, "Read", "file_read", "path", norm), env, root, callCwd)
	if got.WouldDeny {
		got.ToolName = toolName
		got.Reason = "spawn reads " + norm + ": " + got.Reason
		return got
	}
	return r.spawnSend(p, d, env, root, callCwd, toolName, norm)
}

// spawnSend is warden._delegate_send: the network send of a spawn
// call to a remote worker.
func (r *runner) spawnSend(p *pyjson.Object, d *decision, env *envelope, root *workspace, callCwd *string,
	toolName, norm string) *decision {
	defer r.enter()()
	rule := r.actingRule()
	allowRemote := rule != nil && rule.AllowRemote
	rt := r.routeSpawn(env, allowRemote)
	if rt.OK && rt.Tier == "remote" {
		got := r.evaluateRecord(r.syntheticRecord(p, "WebFetch", "network", "url", rt.URL), env, root, callCwd)
		if got.WouldDeny {
			got.ToolName = toolName
			got.Reason = "spawn sends " + norm + " to " + rt.Host + ": " + got.Reason
			got.Tier = tierPermanent
			return got
		}
	}
	return d
}

// graftRedirectReason is warden._graft_redirect_reason.
func graftRedirectReason(lines int, minLines string) string {
	return "this file has " + strconv.Itoa(lines) + " lines, over the " + minLines + "-line threshold for a whole " +
		"read. Call the spawn tool (" + spawn.Tool + ") with the file's " +
		"absolute path and your question: a worker model reads the file and returns " +
		"exact quotes. For exact text before an edit, read the part you need with Read " +
		"and a limit of at most " + minLines + " lines"
}

// maybeGraft is warden._maybe_graft: a deny_redirect graft on an allowed
// whole read of a large file.
func (r *runner) maybeGraft(p *pyjson.Object, d *decision, env *envelope, sessionID any) *decision {
	defer r.enter()()
	if r.fmt != "claude" || !d.Allow || d.WouldDeny {
		return d
	}
	if tn, _ := toolNameOf(p).(string); tn != "Read" || d.StepType != "file_read" {
		return d
	}
	rule := r.actingRule()
	if rule == nil {
		return d
	}
	minLines := rule.MinLinesBig()
	if inp, ok := toolInputOf(p).(*pyjson.Object); ok {
		if lim, ok := inp.Value("limit").(pyjson.Int); ok {
			n, _ := new(big.Int).SetString(lim.Text, 10)
			if n != nil && n.Sign() > 0 && n.Cmp(minLines) <= 0 {
				return d
			}
		}
	}
	rec, ok := r.payloadToRecord(p, r.fmt)
	if !ok {
		return d
	}
	path, isStr := rec.PathRaw.(string)
	if !isStr || !isabs(path) {
		return d
	}
	m, _ := spawn.MeasureFile(path)
	if m == nil || big.NewInt(int64(m.Lines)).Cmp(minLines) <= 0 {
		return d
	}
	graft := rule.AsObject().
		Set("lines", pyjson.Int{Text: strconv.Itoa(m.Lines)}).
		Set("file_lines_over", rule.MinLines)
	arm := ""
	if rule.State == "audit" || rule.State == "trial" {
		arm = spawn.ArmOf(rule, r.safeSession(sessionID))
		graft.Set("arm", arm)
	}
	rt := r.routeSpawn(env, rule.AllowRemote)
	out := *d
	if !rt.OK {
		graft.Set("applied", false).Set("why", rt.Reason)
		out.Graft = graft
		return &out
	}
	graft.Set("worker", rt.AsObject())
	// The envelope must allow the spawn call the reason names, or the
	// model would only meet a second deny.
	call := &record{ToolName: spawn.Tool, StepType: "mcp", MCPServer: spawn.Server, MCPTool: spawn.Name,
		Arguments: pyjson.NewObject().Set("path", path), obj: pyjson.NewObject()}
	call.obj.Set("captured_at", nil)
	call.obj.Set("session_id", r.safeSession(p.Value("session_id")))
	call.obj.Set("tool_name", spawn.Tool)
	call.obj.Set("step_type", "mcp")
	call.obj.Set("mcp_server", spawn.Server)
	call.obj.Set("mcp_tool", spawn.Name)
	call.obj.Set("arguments", call.Arguments)
	j := joinOf(p)
	for _, k := range j.Keys() {
		call.obj.Set(k, j.Value(k))
	}
	var callCwd *string
	if c, ok := p.Value("cwd").(string); ok {
		callCwd = &c
	}
	if got := r.evaluateRecord(call, env, nil, callCwd); got.WouldDeny {
		graft.Set("applied", false).Set("why", "the envelope would not allow the spawn tool: "+got.Reason)
		out.Graft = graft
		return &out
	}
	if rule.State == "audit" {
		graft.Set("applied", false).Set("why", "the rule is in audit: the read is not denied")
		out.Graft = graft
		return &out
	}
	if rule.State == "trial" && arm == "control" {
		graft.Set("applied", false).Set("why", "the session is in the trial's control arm: the read is not denied")
		out.Graft = graft
		return &out
	}
	if r.mode == "audit" {
		// An audit-mode warden never denies for cost (RP-4).
		graft.Set("applied", false).Set("why", "the warden is in audit mode: the read is not denied")
		out.Graft = graft
		return &out
	}
	graft.Set("applied", true).Set("why", nil)
	out.Allow, out.WouldDeny = false, false
	out.Reason = graftRedirectReason(m.Lines, rule.MinLines.Text)
	out.Graft = graft
	return &out
}
