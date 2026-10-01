package sprig

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoopStopsAtTheTokenCap(t *testing.T) {
	call := Message{Role: "assistant", Usage: Usage{Fresh: 3, Out: 4, CacheRead: 2, CacheWrite: 1},
		Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "/nope"}}}}
	model := &scriptModel{turns: []Message{call, call, call}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 9,
		MaxTokens: 15, CapTokens: true}
	_, err := agent.Run("go")
	if err == nil || err.Error() != "used 20 tokens of the 15 allowed" {
		t.Fatalf("err %v", err)
	}
	if model.i != 2 {
		t.Fatalf("model asked %d times, want 2", model.i)
	}
}

func TestLoopTokenCapZeroAsksNothing(t *testing.T) {
	model := &scriptModel{turns: []Message{{Role: "assistant", Text: "x"}}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 9, CapTokens: true}
	if _, err := agent.Run("go"); err == nil || err.Error() != "used 0 tokens of the 0 allowed" || model.i != 0 {
		t.Fatalf("err %v asked %d", err, model.i)
	}
}

func TestLoopTokenCapCountsExtraTokens(t *testing.T) {
	model := &scriptModel{turns: []Message{{Role: "assistant", Text: "x"}}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 9,
		MaxTokens: 5, CapTokens: true, ExtraTokens: func() int { return 5 }}
	if _, err := agent.Run("go"); err == nil || err.Error() != "used 5 tokens of the 5 allowed" {
		t.Fatalf("err %v", err)
	}
}

func TestCLIMaxTokensAndModelTurns(t *testing.T) {
	var out, errb bytes.Buffer
	code := newCLI("done").Run([]string{"--json", "--max-tokens", "100", "x"}, strings.NewReader(""), &out, &errb)
	if code != 0 || !strings.Contains(out.String(), `"model_turns":1,`) || !strings.Contains(out.String(), `"turns":2`) {
		t.Fatalf("exit %d out %s err %s", code, out.String(), errb.String())
	}
	out.Reset()
	code = newCLI("done").Run([]string{"--max-tokens", "0", "x"}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.HasSuffix(errb.String(), "sprig: used 0 tokens of the 0 allowed\n") {
		t.Fatalf("exit %d err %q", code, errb.String())
	}
}
