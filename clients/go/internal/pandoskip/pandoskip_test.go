package pandoskip

import "testing"

func TestOn(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, " ": false, "1": true, "yes": true} {
		if On(v) != want {
			t.Errorf("On(%q) = %v", v, !want)
		}
	}
}

func TestEnvironSetsTheVariableOnceAndKeepsTheRest(t *testing.T) {
	got := Environ([]string{"A=1", Var + "=0", "B=2"})
	want := []string{"A=1", "B=2", Var + "=1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	t.Setenv("X_PANDO_T", "y")
	found := false
	for _, kv := range Environ(nil) {
		found = found || kv == "X_PANDO_T=y"
	}
	if !found {
		t.Fatal("a nil environ must start from the process environment")
	}
}
