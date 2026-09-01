package verify

import (
	"strings"
	"testing"
)

// The step counts tests/test_glob_step_budget.py pins in the oracle.
func TestGlobStepsMatchTheOracle(t *testing.T) {
	deep := "/" + strings.Repeat("a/", 30) + "b"
	for _, c := range []struct {
		norm, glob string
		want       int
	}{
		{"/work/a", "/work/*", 4},
		{"/work/a/b", "/work/**/b", 6},
		{"/work/a", "/work/**", 0},
		{deep, "/**/x/**/c", 34},
		{deep, "/**/a/**/a/**/b", 36},
	} {
		if got := GlobMatchSteps(c.norm, c.glob); got != c.want {
			t.Errorf("%s against %s: %d steps, want %d", c.glob, c.norm, got, c.want)
		}
	}
	over := "/" + strings.Repeat("**/", 6) + "c"
	func() {
		defer func() {
			if g, ok := recover().(GlobTooComplex); !ok || g.Glob != over || g.Limit != 100_000 {
				t.Errorf("want GlobTooComplex, got %v", g)
			}
		}()
		PathMatchesAny(deep, []string{"/work/**", over})
	}()
	if !PathMatchesAny(deep, []string{"/**", over}) {
		t.Error("a glob that matches first should end the search")
	}
}
