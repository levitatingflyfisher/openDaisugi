package cli

import (
	"strings"
	"testing"
)

// The CPU-only recommendation depends on the box's memory, so no fixture
// holds it; these are the oracle's answers for fixed profiles.
func TestRecommendCPUOnlyAsTheOracle(t *testing.T) {
	tail := ". This is provisional — qualify it on YOUR box (run the candidate against the real envelope " +
		"schema and check the pass rate) before trusting it as Tier-1; the model family is your pick, not a " +
		"verified default."
	cpu := " (CPU inference — expect slower generation; favor the smaller end and a low context size)"
	f := func(x float64) *float64 { return &x }
	for _, c := range []struct {
		ram    *float64
		budget float64
		size   string
		head   string
	}{
		{f(16.7), 10.0, "~8B", "budget ~10GB from 16.7GB RAM" + cpu + ". Recommending a ~8B-class instruct model at Q4_K_M"},
		{f(4.0), 2.4, "≤1B", "budget ~2GB from 4GB RAM" + cpu + ". Recommending a ≤1B-class instruct model at Q4_K_M"},
		{nil, 0, "≤1B", "budget ~0GB from undetected memory (treating conservatively). Recommending a ≤1B-class instruct model at Q4_K_M"},
	} {
		h := hardware{system: "Linux", arch: "x86_64", cpus: 4, ram: c.ram}
		r := recommend(h)
		if h.budget() != c.budget || r.size != c.size || r.rationale != c.head+tail {
			t.Errorf("%v: %v %s %q", c.ram, h.budget(), r.size, r.rationale)
		}
		if !strings.HasPrefix(r.rationale, "budget") {
			t.Error(r.rationale)
		}
	}
}
