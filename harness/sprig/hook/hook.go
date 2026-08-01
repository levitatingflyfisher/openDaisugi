// Package hook lets any sprig.Gate govern a DRIVER's own native tools by acting
// as a Claude Code PreToolUse hook. This is Design D: unlike the MCP bridge
// (which replaces the driver's tools), the driver keeps its built-in
// read/write/edit/bash and every call is intercepted here, BEFORE it runs.
//
// The hook receives {tool_name, tool_input} on stdin and returns a
// permissionDecision. Because it sees the FULL tool_input — the actual Bash
// command string included — it closes the blind spot a name-only matcher has.
// The same sprig.Gate powers the loop, the MCP server, and this hook.
package hook

import (
	"encoding/json"

	sprig "github.com/opendaisugi/sprig"
)

type preToolUse struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

// Decide reads a PreToolUse payload, rules on it with the gate, and returns the
// hook's decision JSON plus whether the call is allowed. It is fail-closed: an
// unreadable payload denies. The caller MUST exit 2 on a deny — a JSON
// permissionDecision is a decision INSIDE the permission flow, which
// bypassPermissions skips; only exit 2 is an unconditional block.
func Decide(input []byte, gate sprig.Gate) (out []byte, allow bool) {
	var p preToolUse
	if err := json.Unmarshal(input, &p); err != nil || p.ToolName == "" {
		return decision("deny", "sprig-hook: unreadable PreToolUse payload (fail-closed)"), false
	}
	v := gate.Check(sprig.ToolCall{Name: p.ToolName, Input: p.ToolInput})
	if v.Allow {
		return decision("allow", ""), true
	}
	reason := v.Reason
	if reason == "" {
		reason = "refused by the gate"
	}
	return decision("deny", reason), false
}

func decision(kind, reason string) []byte {
	out := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": kind}
	if reason != "" {
		out["permissionDecisionReason"] = reason
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": out})
	return b
}
