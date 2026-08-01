// Package sprig is a minimal, automatable coding-agent harness whose one
// non-negotiable edge is a runtime gate: every proposed tool call is verified
// against a policy BEFORE it runs, fail-closed. Rent the loop, own the boundary.
//
// This file is the boundary itself — the smallest thing that proves the thesis:
// the executor physically cannot run a tool call the gate did not allow.
package sprig

import (
	"sort"
	"time"
)

// ToolCall is a model's request to run one tool with some input. ID is a
// tool_use id: the API backend (path E) gets a real one from Anthropic;
// every other backend has the agent loop mint one (session_tree.go's
// newID()) before the call reaches Execute, so the session tree can join
// OnToolCall/OnVerdict/OnToolResult to the same call.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// Verdict is a gate's ruling on a ToolCall. Allow=false means refuse; Reason
// explains why (surfaced to the model and the human).
type Verdict struct {
	Allow  bool
	Reason string
}

// Gate rules on a proposed call before it runs. Implementations must be
// fail-closed: when in doubt, refuse. The real gate delegates to openDaisugi's
// envelope verifier; AllowAll/DenyAll below are for wiring and tests.
type Gate interface {
	Check(ToolCall) Verdict
}

// AllowAll permits everything — the "gate off" / plain-pi mode. Switchable.
type AllowAll struct{}

func (AllowAll) Check(ToolCall) Verdict { return Verdict{Allow: true} }

// DenyAll refuses everything with a fixed reason.
type DenyAll struct{ Reason string }

func (d DenyAll) Check(ToolCall) Verdict { return Verdict{Allow: false, Reason: d.Reason} }

// Tool is one capability the agent can invoke (read/write/edit/bash).
type Tool interface {
	Name() string
	Run(input map[string]any) (string, error)
}

// Result is the outcome of attempting a ToolCall through the gate.
type Result struct {
	ID      string // echoes the ToolCall's tool_use id
	Allowed bool
	Output  string
	Reason  string // why it was refused (when !Allowed)
	Err     error  // a tool that ran but errored
}

// Executor is the guarded loop step: it runs a tool ONLY if the tool is known
// and the gate allows the call. Every other path refuses without side effects.
type Executor struct {
	tools map[string]Tool
	gate  Gate

	// SessionObserver is a DISTINCT field from Agent.Observer (loop.go's
	// fleet-supervision OnState interface) — a different interface entirely,
	// SessionObserver, not Observer. Optional: nil means no session tree is
	// being written, checked before every call.
	SessionObserver SessionObserver
}

func NewExecutor(tools map[string]Tool, gate Gate) *Executor {
	return &Executor{tools: tools, gate: gate}
}

// Execute is fail-closed by construction: an unknown tool is refused before the
// gate is even consulted, and a gate denial returns without touching the tool.
// Every path — unknown tool, gate denial, or a run — reports to SessionObserver
// (when set): OnToolCall first, then OnVerdict for the gate's ruling (an unknown
// tool never reaches the gate, so its "verdict" is Execute's own refusal), then
// OnToolResult for what the model ultimately sees.
func (e *Executor) Execute(call ToolCall) Result {
	t0 := time.Now()
	if e.SessionObserver != nil {
		e.SessionObserver.OnToolCall(call.ID, call.Name, call.Input)
	}
	tool, ok := e.tools[call.Name]
	if !ok {
		res := Result{ID: call.ID, Allowed: false, Reason: "unknown tool: " + call.Name}
		e.reportVerdictAndResult(call.ID, res, t0)
		return res
	}
	if v := e.gate.Check(call); !v.Allow {
		res := Result{ID: call.ID, Allowed: false, Reason: v.Reason}
		e.reportVerdictAndResult(call.ID, res, t0)
		return res
	}
	if e.SessionObserver != nil {
		e.SessionObserver.OnVerdict(call.ID, true, "", time.Since(t0))
	}
	out, err := tool.Run(call.Input)
	res := Result{ID: call.ID, Allowed: true, Output: out, Err: err}
	if e.SessionObserver != nil {
		e.SessionObserver.OnToolResult(call.ID, err == nil, resultText(res))
	}
	return res
}

// reportVerdictAndResult reports a refusal (unknown tool or gate deny) using
// resultText (loop.go) — the SAME rendering fed back to the model — so a
// session tree's tool_result reads exactly like what the model actually saw.
func (e *Executor) reportVerdictAndResult(id string, res Result, t0 time.Time) {
	if e.SessionObserver == nil {
		return
	}
	e.SessionObserver.OnVerdict(id, false, res.Reason, time.Since(t0))
	e.SessionObserver.OnToolResult(id, false, resultText(res))
}

// ToolNames returns the executor's tool names, sorted for a stable listing (used
// by the MCP server's tools/list so what it advertises can't drift from what
// Execute will actually accept).
func (e *Executor) ToolNames() []string {
	names := make([]string, 0, len(e.tools))
	for n := range e.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
