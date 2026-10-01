package aliases

import (
	"errors"
	"testing"

	"daisugi-verify/internal/pyjson"
)

func obj(t *testing.T, text string) any {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Substitution is one pass: text an argument puts in is never scanned
// again, the longest name wins, and a regex field gets the escaped text.
func TestSubstituteOnePass(t *testing.T) {
	args := obj(t, `{"p": "$p_name", "p_name": "(x|y)+"}`).(*pyjson.Object)
	expr := obj(t, `{"value": "$p and $p_name", "whole": "$p", "regex": "^$p_name$"}`)
	got := pyjson.Dumps(substitute(expr, args), true)
	want := `{"value": "$p_name and (x|y)+", "whole": "$p_name", "regex": "^\\(x\\|y\\)\\+$"}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestReEscape(t *testing.T) {
	if got := reEscape("a.b-c é\t"); got != "a\\.b\\-c\\ é\\\t" {
		t.Fatalf("got %q", got)
	}
}

func TestRegistryLaws(t *testing.T) {
	r := New()
	if err := LoadSystem(r); err != nil {
		t.Fatal(err)
	}
	eq := obj(t, `{"op": "equals", "path": "type", "value": "shell"}`)
	var e *Error
	if err := r.Register(Alias{Name: "no_secrets", Expr: eq, Tier: "household"}); !errors.As(err, &e) || e.Class != "ValueError" {
		t.Fatalf("a household alias took a system name: %v", err)
	}
	if err := r.Register(Alias{Name: "a", Expr: obj(t, `{"op": "alias", "name": "a", "args": {}}`), Tier: "household"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAndResolve(r, obj(t, `{"op": "alias", "name": "a"}`)); !errors.As(err, &e) ||
		e.Class != "AliasCycleError" || e.Msg != "alias cycle detected: a -> ... -> a" {
		t.Fatalf("got %v", err)
	}
	if _, err := r.Lookup("nope"); !errors.As(err, &e) || e.Class != "UnknownAliasError" || e.Msg != "'nope'" {
		t.Fatalf("got %v", err)
	}
	if _, err := ParseAndResolve(r, obj(t, `{"op": "alias", "name": "never_impersonates"}`)); !errors.As(err, &e) ||
		e.Msg != "alias 'never_impersonates' missing required args: ['principal']" {
		t.Fatalf("got %v", err)
	}
}
