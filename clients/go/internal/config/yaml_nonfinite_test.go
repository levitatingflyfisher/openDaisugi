package config

import (
	"math"
	"strconv"
	"testing"
)

// PyYAML reads .nan and .inf as floats. They resolve to a Float whose text
// strconv.ParseFloat reads, so gate register can refuse an envelope that
// holds one (models.non_finite_error) instead of saying it cannot read it.
func TestPlainNonFiniteFloats(t *testing.T) {
	for in, want := range map[string]float64{
		".nan": math.NaN(), ".NaN": math.NaN(), ".NAN": math.NaN(),
		".inf": math.Inf(1), "+.inf": math.Inf(1), ".Inf": math.Inf(1), "-.INF": math.Inf(-1),
	} {
		v, err := plain(in)
		if err != nil || v.Kind != Float {
			t.Errorf("%s: got %+v %v, want a Float", in, v, err)
			continue
		}
		f, perr := strconv.ParseFloat(v.Text, 64)
		if perr != nil || (math.IsNaN(want) != math.IsNaN(f)) || (!math.IsNaN(want) && f != want) {
			t.Errorf("%s: text %q reads as %v, %v", in, v.Text, f, perr)
		}
	}
	// PyYAML does not resolve these to floats: they stay strings.
	for _, in := range []string{"nan", "inf", "NaN", "+.nan", ".nAn"} {
		if v, err := plain(in); err != nil || v.Kind != Str {
			t.Errorf("%s: got %+v %v, want a Str", in, v, err)
		}
	}
}
