package sprig

import "testing"

// A negative usage count is a protocol error: the turn fails.
func TestLoopRefusesANegativeUsageCount(t *testing.T) {
	for _, u := range []Usage{{Out: -1000000}, {CacheRead: -1}, {Fresh: -5, Out: 9}, {CacheWrite: -1}} {
		model := &scriptModel{turns: []Message{{Role: "assistant", Text: "x", Usage: u}}}
		agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 9}
		if _, err := agent.Run("go"); err == nil || err.Error() != "the model reported a negative token count" {
			t.Fatalf("%+v: err %v", u, err)
		}
	}
}

// The counts add with no wrap: a huge count stops the loop at the cap.
func TestLoopTokenSumSaturates(t *testing.T) {
	huge := int(^uint(0) >> 2) // 2^62 - 1
	call := Message{Role: "assistant", Usage: Usage{Fresh: huge, Out: huge, CacheRead: huge, CacheWrite: huge},
		Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "/nope"}}}}
	model := &scriptModel{turns: []Message{call, call, call}}
	agent := &Agent{Model: model, Exec: NewExecutor(DefaultTools(), AllowAll{}), MaxTurns: 9,
		MaxTokens: 100, CapTokens: true}
	_, err := agent.Run("go")
	want := "used 9223372036854775807 tokens of the 100 allowed"
	if err == nil || err.Error() != want || model.i != 1 {
		t.Fatalf("err %v asked %d", err, model.i)
	}
	agent.History = append(agent.History, call)
	if got := usageSum(agent.History)["input_tokens"]; got != 2*huge {
		t.Fatalf("sum %d", got)
	}
	agent.History = append(agent.History, call, call)
	if got := usageSum(agent.History)["input_tokens"]; got != int(^uint(0)>>1) {
		t.Fatalf("sum %d", got)
	}
}

// The OpenAI wire keeps a negative count negative, so the loop sees it.
func TestOpenAIUsageKeepsANegativeCount(t *testing.T) {
	for _, body := range []string{
		`{"prompt_tokens":-3,"completion_tokens":1}`,
		`{"prompt_tokens":3,"prompt_tokens_details":{"cached_tokens":-1}}`,
		`{"prompt_tokens":3,"completion_tokens":-1}`,
	} {
		m, err := parseOpenAIResponse([]byte(`{"choices":[{"message":{"content":"x"}}],"usage":` + body + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if u := m.Usage; u.Fresh >= 0 && u.CacheRead >= 0 && u.Out >= 0 {
			t.Fatalf("%s: %+v", body, u)
		}
	}
}
