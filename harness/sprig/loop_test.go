package sprig

import "testing"

// scriptModel returns pre-scripted assistant turns, ignoring history — enough to
// drive the loop deterministically in tests without a real model.
type scriptModel struct {
	turns []Message
	i     int
}

func (m *scriptModel) Next([]Message) (Message, error) {
	msg := m.turns[m.i]
	m.i++
	return msg, nil
}

func TestLoopRunsGatedToolThenReturnsFinalText(t *testing.T) {
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "echo hi"}}}},
		{Role: "assistant", Text: "done: it said hi"},
	}}
	ex := NewExecutor(DefaultTools(), AllowAll{})
	agent := &Agent{Model: model, Exec: ex, MaxTurns: 5}

	out, err := agent.Run("say hi")
	if err != nil {
		t.Fatal(err)
	}
	if out != "done: it said hi" {
		t.Fatalf("want final assistant text, got %q", out)
	}
	// the tool result must have reached the model as a tool message
	if !containsToolResult(agent.History, "hi") {
		t.Fatal("the bash result never made it back into the history")
	}
}

func TestLoopIsFailClosedWhenGateDenies(t *testing.T) {
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "rm -rf /"}}}},
		{Role: "assistant", Text: "ok, I won't"},
	}}
	ex := NewExecutor(DefaultTools(), DenyAll{Reason: "outside envelope"})
	agent := &Agent{Model: model, Exec: ex, MaxTurns: 5}

	out, err := agent.Run("delete everything")
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok, I won't" {
		t.Fatalf("got %q", out)
	}
	// the model must SEE the refusal (so it can adapt), and the reason is surfaced
	if !containsToolResult(agent.History, "outside envelope") {
		t.Fatal("the refusal reason was not fed back to the model")
	}
}

func TestLoopStopsAtMaxTurns(t *testing.T) {
	// a model that always calls a tool, never finishing
	looping := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "/nope"}}}},
		{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "/nope"}}}},
		{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "/nope"}}}},
	}}
	agent := &Agent{Model: looping, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 2}
	if _, err := agent.Run("loop forever"); err == nil {
		t.Fatal("expected a max-turns error")
	}
}

func containsToolResult(h []Message, needle string) bool {
	for _, m := range h {
		if m.Role == "tool" && contains(m.Text, needle) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

type recordObs struct{ states []string }

func (r *recordObs) OnState(s string) { r.states = append(r.states, s) }

func TestAgentReportsStateToObserver(t *testing.T) {
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "echo hi"}}}},
		{Role: "assistant", Text: "done"},
	}}
	obs := &recordObs{}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5, Observer: obs}
	agent.Run("x")
	joined := ""
	for _, s := range obs.states {
		joined += s + ","
	}
	for _, want := range []string{"running", "tool:bash", "done"} {
		if !contains(joined, want) {
			t.Fatalf("states %q missing %q", joined, want)
		}
	}
}

func TestAgentReportsBlockedOnRefusal(t *testing.T) {
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": "x"}}}},
		{Role: "assistant", Text: "ok"},
	}}
	obs := &recordObs{}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), DenyAll{Reason: "no"}), MaxTurns: 5, Observer: obs}
	agent.Run("x")
	joined := ""
	for _, s := range obs.states {
		joined += s + ","
	}
	if !contains(joined, "blocked") {
		t.Fatalf("must report blocked on refusal, got %q", joined)
	}
}

// --- clarifying retry: a reply that is neither a clean call nor a real answer
// gets ONE nudge, instead of being silently accepted as a (wrong) final answer ---

func TestLoopNudgesOnceOnEmptyReplyThenFinishes(t *testing.T) {
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Text: ""},                // empty = unclear
		{Role: "assistant", Text: "the real answer"}, // recovers after the nudge
	}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5}
	out, err := agent.Run("do it")
	if err != nil {
		t.Fatal(err)
	}
	if out != "the real answer" {
		t.Fatalf("an empty reply must be nudged, not accepted; got %q", out)
	}
	if !containsUserMsg(agent.History, clarifyNudge) {
		t.Fatal("the clarifying nudge was never sent")
	}
}

func TestLoopNudgesOnAttemptedButBrokenCall(t *testing.T) {
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Text: "```sprig-tool\n{oops not json"}, // botched call
		{Role: "assistant", Text: "final"},
	}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5}
	out, err := agent.Run("do it")
	if err != nil {
		t.Fatal(err)
	}
	if out != "final" {
		t.Fatalf("a botched tool fence must be nudged, not accepted; got %q", out)
	}
	if !containsUserMsg(agent.History, clarifyNudge) {
		t.Fatal("the clarifying nudge was never sent")
	}
}

func TestLoopNudgesAtMostOnceThenDegrades(t *testing.T) {
	// The nudge must be capped so a stubborn model never hangs the loop: after one
	// nudge, a still-unclear reply is accepted as final (today's behavior).
	model := &scriptModel{turns: []Message{
		{Role: "assistant", Text: ""},
		{Role: "assistant", Text: ""},
	}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5}
	out, err := agent.Run("do it")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("after one nudge an empty reply is accepted; got %q", out)
	}
	n := 0
	for _, m := range agent.History {
		if m.Role == "user" && m.Text == clarifyNudge {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one nudge, got %d", n)
	}
}

// --- --resume: a seeded history is a prefix, not a replacement ------------

func TestRunSeedsHistoryFromSeedHistoryBeforeTheNewTask(t *testing.T) {
	model := &scriptModel{turns: []Message{{Role: "assistant", Text: "ok, continuing"}}}
	agent := &Agent{
		Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5,
		SeedHistory: []Message{
			{Role: "user", Text: "earlier task"},
			{Role: "assistant", Text: "earlier answer"},
		},
	}
	out, err := agent.Run("continue please")
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok, continuing" {
		t.Fatalf("got %q", out)
	}
	if len(agent.History) != 4 {
		t.Fatalf("want seed(2) + new task(1) + reply(1) — got %d: %+v", len(agent.History), agent.History)
	}
	if agent.History[0].Text != "earlier task" || agent.History[1].Text != "earlier answer" {
		t.Fatalf("seed history must come first, got %+v", agent.History[:2])
	}
	if agent.History[2].Text != "continue please" {
		t.Fatalf("the new task must follow the seed, got %+v", agent.History[2])
	}
}

func TestRunWithNoSeedHistoryStartsFreshAsBefore(t *testing.T) {
	model := &scriptModel{turns: []Message{{Role: "assistant", Text: "ok"}}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 5}
	if _, err := agent.Run("x"); err != nil {
		t.Fatal(err)
	}
	if len(agent.History) != 2 || agent.History[0].Text != "x" {
		t.Fatalf("no SeedHistory must behave exactly as before, got %+v", agent.History)
	}
}

func containsUserMsg(h []Message, needle string) bool {
	for _, m := range h {
		if m.Role == "user" && contains(m.Text, needle) {
			return true
		}
	}
	return false
}
