package gate

import (
	"math/big"
	"strconv"

	"daisugi-verify/internal/delegate"
	"daisugi-verify/internal/pyjson"
)

// delegateEnvelope is what the router reads of the envelope.
func delegateEnvelope(env *envelope) *delegate.Envelope {
	return &delegate.Envelope{Stakes: env.Stakes, Network: env.Network, NetworkHosts: env.NetworkHosts}
}

// actingRule is delegate.acting_rule over this gate root. A rule file
// pyjson does not model hands the call back.
func (r *runner) actingRule() *delegate.Rule {
	rule, err := delegate.ActingRule(r.root)
	if err != nil {
		unported("a graft rule file the port's JSON reader does not model")
	}
	return rule
}

// routeDelegate is delegate.route_delegate from this gate root's data dir.
func (r *runner) routeDelegate(env *envelope, allowRemote bool) delegate.Route {
	rt, err := delegate.RouteDelegate(pathParent(r.root), delegateEnvelope(env), allowRemote, r.envGet)
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

// decideDelegateCall is gate._decide_delegate_call: an allowed delegate
// MCP call must also pass as a read of its absolute path, and as a
// network send when the router's worker is remote.
func (r *runner) decideDelegateCall(p *pyjson.Object, rec *record, d *decision, env *envelope,
	root *workspace, callCwd *string) *decision {
	defer r.enter()()
	toolName := rec.ToolName
	path, isStr := rec.Arguments.Value("path").(string)
	if !isStr || !isabs(path) {
		out := r.deny("delegate: the path must be an absolute path, so the gate checks the file " +
			"the delegate reads")
		out.ToolName, out.StepType, out.Detail = toolName, "mcp", path
		return out
	}
	if env.Stakes == "physical" {
		out := r.deny("delegate: the delegate is refused under physical stakes")
		out.ToolName, out.StepType, out.Detail = toolName, "mcp", path
		return out
	}
	norm := normpath(path)
	got := r.evaluateRecord(r.syntheticRecord(p, "Read", "file_read", "path", norm), env, root, callCwd)
	if got.WouldDeny {
		got.ToolName = toolName
		got.Reason = "delegate reads " + norm + ": " + got.Reason
		return got
	}
	rule := r.actingRule()
	allowRemote := rule != nil && rule.AllowRemote
	rt := r.routeDelegate(env, allowRemote)
	if rt.OK && rt.Tier == "remote" {
		got = r.evaluateRecord(r.syntheticRecord(p, "WebFetch", "network", "url", rt.URL), env, root, callCwd)
		if got.WouldDeny {
			got.ToolName = toolName
			got.Reason = "delegate sends " + norm + " to " + rt.Host + ": " + got.Reason
			got.Tier = tierPermanent
			return got
		}
	}
	return d
}

// graftRedirectReason is gate._graft_redirect_reason.
func graftRedirectReason(lines int, minLines string) string {
	return "this file has " + strconv.Itoa(lines) + " lines, over the " + minLines + "-line threshold for a whole " +
		"read. Call the delegate tool (" + delegate.Tool + ") with the file's " +
		"absolute path and your question: a worker model reads the file and returns " +
		"exact quotes. For exact text before an edit, read the part you need with Read " +
		"and a limit of at most " + minLines + " lines"
}

// maybeGraft is gate._maybe_graft: a deny_redirect graft on an allowed
// whole read of a large file.
func (r *runner) maybeGraft(p *pyjson.Object, d *decision, env *envelope) *decision {
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
	m, _ := delegate.MeasureFile(path)
	if m == nil || big.NewInt(int64(m.Lines)).Cmp(minLines) <= 0 {
		return d
	}
	graft := rule.AsObject().
		Set("lines", pyjson.Int{Text: strconv.Itoa(m.Lines)}).
		Set("file_lines_over", rule.MinLines)
	rt := r.routeDelegate(env, rule.AllowRemote)
	out := *d
	if !rt.OK {
		graft.Set("applied", false).Set("why", rt.Reason)
		out.Graft = graft
		return &out
	}
	graft.Set("worker", rt.AsObject())
	// The envelope must allow the delegate call the reason names, or the
	// model would only meet a second deny.
	call := &record{ToolName: delegate.Tool, StepType: "mcp", MCPServer: delegate.Server, MCPTool: delegate.Name,
		Arguments: pyjson.NewObject().Set("path", path), obj: pyjson.NewObject()}
	call.obj.Set("captured_at", nil)
	call.obj.Set("session_id", r.safeSession(p.Value("session_id")))
	call.obj.Set("tool_name", delegate.Tool)
	call.obj.Set("step_type", "mcp")
	call.obj.Set("mcp_server", delegate.Server)
	call.obj.Set("mcp_tool", delegate.Name)
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
		graft.Set("applied", false).Set("why", "the envelope would not allow the delegate tool: "+got.Reason)
		out.Graft = graft
		return &out
	}
	if rule.State != "active" {
		graft.Set("applied", false).Set("why", "the rule is in audit: the read is not denied")
		out.Graft = graft
		return &out
	}
	if r.mode == "audit" {
		// An audit-mode gate never denies for cost (RP-4).
		graft.Set("applied", false).Set("why", "the gate is in audit mode: the read is not denied")
		out.Graft = graft
		return &out
	}
	graft.Set("applied", true).Set("why", nil)
	out.Allow, out.WouldDeny = false, false
	out.Reason = graftRedirectReason(m.Lines, rule.MinLines.Text)
	out.Graft = graft
	return &out
}
