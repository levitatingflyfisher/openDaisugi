package sprig

import (
	"testing"
	"time"
)

// recordingSessionObserver captures every call it receives, in order, so a
// test can assert both WHAT was reported and that the SAME tool_use_id
// threads through OnToolCall/OnVerdict/OnToolResult for one call.
type recordingSessionObserver struct {
	prompts    []string
	assistants []assistantCall
	toolCalls  []toolCallEvent
	verdicts   []verdictEvent
	results    []resultEvent
}

type assistantCall struct {
	model, text string
	usage       Usage
	toolUses    []ToolUse
}
type toolCallEvent struct {
	id, name string
	input    map[string]any
}
type verdictEvent struct {
	id      string
	allow   bool
	reason  string
	latency time.Duration
}
type resultEvent struct {
	id      string
	ok      bool
	summary string
}

func (r *recordingSessionObserver) OnPrompt(text string) { r.prompts = append(r.prompts, text) }
func (r *recordingSessionObserver) OnAssistant(model, text string, usage Usage, toolUses []ToolUse) {
	r.assistants = append(r.assistants, assistantCall{model, text, usage, toolUses})
}
func (r *recordingSessionObserver) OnToolCall(id, name string, input map[string]any) {
	r.toolCalls = append(r.toolCalls, toolCallEvent{id, name, input})
}
func (r *recordingSessionObserver) OnVerdict(id string, allow bool, reason string, latency time.Duration) {
	r.verdicts = append(r.verdicts, verdictEvent{id, allow, reason, latency})
}
func (r *recordingSessionObserver) OnToolResult(id string, ok bool, summary string) {
	r.results = append(r.results, resultEvent{id, ok, summary})
}

// --- Executor: SessionObserver fires on every path, id threads through ----

func TestExecutorReportsToolCallVerdictAndResultUnderTheSameID(t *testing.T) {
	spy := &spyTool{out: "did work"}
	ex := NewExecutor(map[string]Tool{"spy": spy}, AllowAll{})
	obs := &recordingSessionObserver{}
	ex.SessionObserver = obs

	ex.Execute(ToolCall{ID: "abc123", Name: "spy", Input: map[string]any{"x": 1}})

	if len(obs.toolCalls) != 1 || obs.toolCalls[0].id != "abc123" || obs.toolCalls[0].name != "spy" {
		t.Fatalf("OnToolCall: %+v", obs.toolCalls)
	}
	if len(obs.verdicts) != 1 || obs.verdicts[0].id != "abc123" || !obs.verdicts[0].allow {
		t.Fatalf("OnVerdict: %+v", obs.verdicts)
	}
	if len(obs.results) != 1 || obs.results[0].id != "abc123" || !obs.results[0].ok || obs.results[0].summary != "did work" {
		t.Fatalf("OnToolResult: %+v", obs.results)
	}
}

func TestExecutorReportsADenyVerdictWhenTheGateRefuses(t *testing.T) {
	spy := &spyTool{out: "did work"}
	ex := NewExecutor(map[string]Tool{"spy": spy}, DenyAll{Reason: "outside envelope"})
	obs := &recordingSessionObserver{}
	ex.SessionObserver = obs

	ex.Execute(ToolCall{ID: "d1", Name: "spy"})

	if spy.ran {
		t.Fatal("a denied call must not run the tool")
	}
	if len(obs.verdicts) != 1 || obs.verdicts[0].allow || obs.verdicts[0].reason != "outside envelope" {
		t.Fatalf("OnVerdict: %+v", obs.verdicts)
	}
	if len(obs.results) != 1 || obs.results[0].ok {
		t.Fatalf("OnToolResult must report the refusal, got %+v", obs.results)
	}
}

func TestExecutorReportsAnUnknownToolAsADeniedVerdict(t *testing.T) {
	ex := NewExecutor(map[string]Tool{}, AllowAll{})
	obs := &recordingSessionObserver{}
	ex.SessionObserver = obs

	ex.Execute(ToolCall{ID: "u1", Name: "ghost"})

	if len(obs.verdicts) != 1 || obs.verdicts[0].allow {
		t.Fatalf("an unknown tool must report a deny verdict: %+v", obs.verdicts)
	}
}

func TestExecutorToleratesANilSessionObserver(t *testing.T) {
	// The zero-value Executor (no SessionObserver assigned) must not panic —
	// every real caller (fleet, mcp, hook) constructs one this way today.
	ex := NewExecutor(map[string]Tool{"spy": &spyTool{out: "ok"}}, AllowAll{})
	res := ex.Execute(ToolCall{Name: "spy"})
	if !res.Allowed {
		t.Fatalf("got %+v", res)
	}
}

// --- Agent: mints a tool_use_id per call, reports OnPrompt/OnAssistant -----

func TestAgentMintsATransientIDAndThreadsItThroughTheSessionTree(t *testing.T) {
	// The text-hardened backend (model_claude.go) never sets ToolCall.ID — the
	// loop must mint one so OnToolCall/OnVerdict/OnToolResult can be joined.
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "echo hi"}}}},
		{Role: "assistant", Text: "done"},
	}}
	obs := &recordingSessionObserver{}
	ex := NewExecutor(DefaultTools(), AllowAll{})
	ex.SessionObserver = obs
	agent := &Agent{Model: model, Exec: ex, MaxTurns: 5, SessionObserver: obs}

	if _, err := agent.Run("say hi"); err != nil {
		t.Fatal(err)
	}

	if len(obs.prompts) != 1 || obs.prompts[0] != "say hi" {
		t.Fatalf("OnPrompt: %+v", obs.prompts)
	}
	if len(obs.assistants) != 2 {
		t.Fatalf("want an OnAssistant per model reply, got %+v", obs.assistants)
	}
	firstUses := obs.assistants[0].toolUses
	if len(firstUses) != 1 || firstUses[0].ID == "" || firstUses[0].Name != "bash" {
		t.Fatalf("the assistant entry must echo a minted, non-empty id: %+v", firstUses)
	}
	mintedID := firstUses[0].ID
	if len(obs.toolCalls) != 1 || obs.toolCalls[0].id != mintedID {
		t.Fatalf("OnToolCall must use the SAME id the assistant entry echoed: call=%q assistant=%q",
			obs.toolCalls[0].id, mintedID)
	}
	if len(obs.verdicts) != 1 || obs.verdicts[0].id != mintedID {
		t.Fatalf("OnVerdict must use the same id: %+v", obs.verdicts)
	}
	if len(obs.results) != 1 || obs.results[0].id != mintedID {
		t.Fatalf("OnToolResult must use the same id: %+v", obs.results)
	}
}

func TestAgentPreservesAnIDTheModelAlreadySet(t *testing.T) {
	// The API backend (path E) gets a real tool_use id from Anthropic — the
	// loop must not mint a second one over it.
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{ID: "toolu_real", Name: "bash", Input: map[string]any{"cmd": "x"}}}},
		{Role: "assistant", Text: "done"},
	}}
	obs := &recordingSessionObserver{}
	ex := NewExecutor(DefaultTools(), AllowAll{})
	ex.SessionObserver = obs
	agent := &Agent{Model: model, Exec: ex, MaxTurns: 5, SessionObserver: obs}

	if _, err := agent.Run("x"); err != nil {
		t.Fatal(err)
	}
	if len(obs.toolCalls) != 1 || obs.toolCalls[0].id != "toolu_real" {
		t.Fatalf("the model's own id must survive, got %+v", obs.toolCalls)
	}
}

func TestAgentToleratesANilSessionObserver(t *testing.T) {
	model := &scriptModel{turns: []Message{{Role: "assistant", Text: "ok"}}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5}
	if _, err := agent.Run("x"); err != nil {
		t.Fatal(err)
	}
}
