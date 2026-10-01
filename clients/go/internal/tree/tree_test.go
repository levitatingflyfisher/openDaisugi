package tree

import (
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

func envOf(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, text)
	if verr != nil {
		t.Fatal(verr)
	}
	return v.(*pyjson.Object)
}

func TestEdgeOKReportsEveryPartInOrder(t *testing.T) {
	parent := envOf(t, `{"id": "p", "generated_by": "t", "task": "t", "stakes": "high", "deadline": 100,
		"permissions": {"shell": true, "shell_allowlist": ["git"]}}`)
	child := envOf(t, `{"id": "c", "generated_by": "t", "task": "t", "stakes": "low", "deadline": 200,
		"permissions": {"shell": true, "shell_allowlist": ["git status", "rm"], "max_execution_time_s": 60}}`)
	res := EdgeOK(parent, child, DefaultTimeoutMs)
	want := []string{
		`stakes: the child's "low" is lower than the parent's "high"`,
		"max_execution_time_s: the child's 60 is more than the parent's 30",
		"deadline: the child's 200.0 is after the parent's 100.0",
		`shell_allowlist: the child adds ["rm"]`,
	}
	if res.Holds || strings.Join(res.Reasons, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", res.Reasons)
	}
}

func TestEdgeOKInheritsTheDeadline(t *testing.T) {
	parent := envOf(t, `{"id": "p", "generated_by": "t", "task": "t", "deadline": 100, "permissions": {}}`)
	child := envOf(t, `{"id": "c", "generated_by": "t", "task": "t", "permissions": {}}`)
	res := EdgeOK(parent, child, DefaultTimeoutMs)
	if !res.Holds || !res.Inherited || Q(res.Child.Value("deadline")) != "100.0" {
		t.Fatalf("got %+v", res)
	}
	if child.Value("deadline") != nil {
		t.Fatal("the child itself changed")
	}
}
