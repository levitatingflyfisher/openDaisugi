package rank

import "testing"

func TestFamilyOf(t *testing.T) {
	for in, want := range map[string]string{
		"claude-haiku-4-5": "claude", "gpt-5-mini": "gpt", "o4-mini": "gpt", "gemma3:4b": "gemini",
		"gemini-2.5-flash": "gemini", "Qwen3-8B": "qwen", "": "", "9b": "", "judge-9b": "judge",
		"haiku": "claude", "anthropic/claude-haiku-4-5": "claude", "openai/gpt-5-mini": "gpt", "local/": "local", "/x": "", "sonnet": "claude", "opus": "claude", "fable": "claude",
	} {
		if got := FamilyOf(in); got != want {
			t.Errorf("FamilyOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKendallTauB(t *testing.T) {
	one := func(xs ...string) [][]string {
		var o [][]string
		for _, x := range xs {
			o = append(o, []string{x})
		}
		return o
	}
	if v, ok := KendallTauB(one("a", "b", "c"), one("a", "b", "c")); !ok || v != 1.0 {
		t.Errorf("same order: %v %v", v, ok)
	}
	if v, ok := KendallTauB(one("a", "b", "c"), one("c", "b", "a")); !ok || v != -1.0 {
		t.Errorf("reverse: %v %v", v, ok)
	}
	if v, ok := KendallTauB([][]string{{"a", "b"}, {"c"}}, one("a", "b", "c")); !ok || v != 0.816496581 {
		t.Errorf("ties: %v %v", v, ok)
	}
	if _, ok := KendallTauB(one("a"), one("a")); ok {
		t.Error("one item is not measurable")
	}
}

func TestCyclicMagnitude(t *testing.T) {
	v := func(w, l string) Vote { return Vote{I: w, J: l, Win: 1.0, W: 1.0} }
	if _, ok := CyclicMagnitude([]Vote{v("a", "b"), v("b", "c")}); ok {
		t.Error("a path closes no loop")
	}
	if m, ok := CyclicMagnitude([]Vote{v("a", "b"), v("b", "c"), v("c", "a")}); !ok || m != 1.0 {
		t.Errorf("cycle: %v %v", m, ok)
	}
	if m, ok := CyclicMagnitude([]Vote{v("a", "b"), v("b", "c"), v("a", "c")}); !ok || m != 0.0 {
		t.Errorf("transitive: %v %v", m, ok)
	}
}

// The values are the oracle's (tests/test_rank.py, test_beta_sample_values_are_pinned_for_the_ports).
func TestBetaSampleAndWeightsMatchTheOracle(t *testing.T) {
	if got := BetaSample(3, 2, &SplitMix64{7}); got != 0.582930293 {
		t.Errorf("beta(3,2) = %v", got)
	}
	if got := BetaSample(1, 1, &SplitMix64{7}); got != 0.389829748 {
		t.Errorf("beta(1,1) = %v", got)
	}
	w := JudgeWeights(map[string]*Trust{
		"good": {Agreed: 40, Disagreed: 2}, "poor": {Agreed: 6, Disagreed: 6}, "cut": {Disagreed: 5, Dropped: true},
	}, "ab12")
	if len(w) != 2 || w["good"] != 1.921783876 || w["poor"] != 0.589813668 {
		t.Errorf("weights = %v", w)
	}
}
