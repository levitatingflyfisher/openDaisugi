package sprig

import (
	"fmt"
	"strings"
)

// clarifyNudge is sent once when a reply is neither a clean tool call nor a
// plausible final answer, so a botched call is corrected instead of being
// silently accepted as the (wrong) answer.
const clarifyNudge = "Your last reply was not a valid tool call and not a final answer. " +
	"Reply with ONLY a ```sprig-tool block to call a tool, or your final answer as plain text."

// Message is one turn in the conversation. An assistant turn either finishes
// (Text, no Calls) or requests tools (Calls). A tool turn carries a result.
// Model and Usage are populated when the backend actually knows them: the API
// backend (path E, model_api.go's parseAPIResponse) fills both from the
// Messages API's own model and usage fields; the text-hardened `claude -p`
// backend (model_claude.go) sets Model from its own config but leaves Usage
// at its zero value — that IS honest there, since `claude -p`'s plain-text
// output carries no token counts to report, not a stand-in for real data.
type Message struct {
	Role  string // "user" | "assistant" | "tool"
	Text  string
	Calls []ToolCall
	Model string
	Usage Usage
}

// Model is the only thing sprig doesn't own — swap in claude-code, an API, or a
// local model. Given the history, it returns the next assistant turn.
type Model interface {
	Next(history []Message) (Message, error)
}

// Observer receives the agent's state transitions as they happen, so a fleet can
// fuse status across many agents without scraping. States: "running", "tool:<name>",
// "blocked" (a gate refusal), "done", "failed". Optional — nil means no reporting.
type Observer interface {
	OnState(state string)
}

// Agent is the whole harness: a model, a gated executor, a turn budget. The loop
// is deliberately tiny — "an LLM, a loop, and enough tokens."
type Agent struct {
	Model    Model
	Exec     *Executor
	MaxTurns int
	Observer Observer // optional: reports state transitions for fleet supervision
	History  []Message

	// SessionObserver is a DISTINCT field and type from Observer above — it
	// writes the session tree (session_tree.go: prompt/assistant/tool_call/
	// verdict/tool_result), Observer reports coarse fleet state ("running",
	// "blocked", "done"). Optional: nil means no session tree.
	SessionObserver SessionObserver

	// SeedHistory prefixes History for a resumed session (--session-dir
	// --resume): rebuilt from the session tree's head path by
	// HistoryFromEntries. Nil (the default) reproduces the old behavior —
	// Run starts from just the new task.
	SeedHistory []Message

	clarified bool // whether the current unclear reply has already been nudged
}

func (a *Agent) report(state string) {
	if a.Observer != nil {
		a.Observer.OnState(state)
	}
}

// Run drives the loop: ask the model, run any requested tools THROUGH THE GATE,
// feed results back, until the model finishes with plain text or the turn budget
// runs out. A denied call comes back as a tool result the model can see and adapt
// to — fail-closed, but not silent.
func (a *Agent) Run(task string) (string, error) {
	a.History = append(append([]Message{}, a.SeedHistory...), Message{Role: "user", Text: task})
	a.report("running")
	if a.SessionObserver != nil {
		a.SessionObserver.OnPrompt(task)
	}
	for turn := 0; turn < a.MaxTurns; turn++ {
		msg, err := a.Model.Next(a.History)
		if err != nil {
			a.report("failed")
			return "", err
		}
		// Mint a tool_use_id for any call that doesn't already have one (the
		// API backend gives a real one, model_api.go's parseAPIResponse;
		// every other backend leaves ID empty). Minted BEFORE History gets
		// the message and BEFORE Execute runs, so the same id threads
		// through the assistant entry's ToolUses, OnToolCall, OnVerdict, and
		// OnToolResult — one call, one id, everywhere in the session tree.
		for i := range msg.Calls {
			if msg.Calls[i].ID == "" {
				msg.Calls[i].ID = newID()
			}
		}
		a.History = append(a.History, msg)
		if a.SessionObserver != nil {
			toolUses := make([]ToolUse, 0, len(msg.Calls))
			for _, c := range msg.Calls {
				toolUses = append(toolUses, ToolUse{ID: c.ID, Name: c.Name})
			}
			a.SessionObserver.OnAssistant(msg.Model, msg.Text, msg.Usage, toolUses)
		}
		if len(msg.Calls) == 0 {
			// Neither a clean call nor a real answer? Nudge once, then accept — so a
			// botched call is corrected, but a stubborn model never hangs the loop.
			if !a.clarified && needsClarification(msg.Text) {
				a.clarified = true
				a.History = append(a.History, Message{Role: "user", Text: clarifyNudge})
				continue
			}
			a.report("done")
			return msg.Text, nil // finished
		}
		a.clarified = false // a real call arrived; the next unclear reply earns a fresh nudge
		for _, call := range msg.Calls {
			a.report("tool:" + call.Name)
			res := a.Exec.Execute(call)
			if !res.Allowed {
				a.report("blocked")
			}
			a.History = append(a.History, Message{Role: "tool", Text: resultText(res)})
		}
	}
	a.report("failed")
	return "", fmt.Errorf("sprig: gave up after %d turns without finishing", a.MaxTurns)
}

// attemptFences mark an intent to call a tool. If one survived into what would
// otherwise be a final answer, the model tried and botched the call — so nudge.
// ```json is excluded: a genuine answer may legitimately quote a JSON block.
var attemptFences = []string{"```sprig-tool", "```tool"}

// needsClarification reports whether a no-call reply is too unclear to accept as
// a final answer: empty, or a botched tool call that parsing could not recover.
func needsClarification(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	for _, f := range attemptFences {
		if strings.Contains(t, f) {
			return true
		}
	}
	return false
}

// resultText renders a tool Result for the model: the output on success, an
// explicit REFUSED line (with the reason) when the gate said no, or the error.
func resultText(r Result) string {
	switch {
	case !r.Allowed:
		return "REFUSED by the gate: " + r.Reason
	case r.Err != nil:
		return "error: " + r.Err.Error() + "\n" + r.Output
	default:
		return r.Output
	}
}
