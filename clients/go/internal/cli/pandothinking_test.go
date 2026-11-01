package cli

import "testing"

func TestThinkingOptsPerWire(t *testing.T) {
	for _, c := range []struct {
		level, wire string
		effort      string
		budget, max int
	}{
		{"off", "openai", "none", 0, 1024},
		{"low", "openai", "low", 0, 1024},
		{"high", "openai", "high", 0, 1024},
		{"off", "anthropic", "", 0, 1024},
		{"low", "anthropic", "", 1024, 2048},
		{"medium", "anthropic", "", 4096, 5120},
		{"high", "anthropic", "", 16000, 17024},
	} {
		o := thinkingOpts(c.level, c.wire, 1024)
		if o.ReasoningEffort != c.effort || o.ThinkingBudget != c.budget || o.MaxTokens != c.max {
			t.Errorf("%s/%s: %+v", c.level, c.wire, o)
		}
	}
}
