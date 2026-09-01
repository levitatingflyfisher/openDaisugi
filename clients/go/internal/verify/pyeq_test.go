package verify

import (
	"encoding/json"
	"testing"
)

// Python's == holds True == 1 == 1.0 and False == 0, inside lists and
// dicts too, so not_equals and not_in_set find these values equal.
func TestBoolAndNumberCompareAsPythonDoes(t *testing.T) {
	steps := `[{"id": "s1", "type": "shell", "command": "ls", "metadata": {"n": 1, "b": true, "l": [1, true], "z": 0}}]`
	cases := []struct {
		name, expr string
		want       bool
	}{
		{"not_equals 1 True", `{"op": "forall_steps", "pred": {"op": "not_equals", "path": "metadata.n", "value": true}}`, false},
		{"not_in_set True [1]", `{"op": "forall_steps", "pred": {"op": "not_in_set", "path": "metadata.b", "values": [1]}}`, false},
		{"not_equals list", `{"op": "forall_steps", "pred": {"op": "not_equals", "path": "metadata.l", "value": [1.0, 1]}}`, false},
		{"not_equals 0 False", `{"op": "forall_steps", "pred": {"op": "not_equals", "path": "metadata.z", "value": false}}`, false},
		{"equals True 1.0", `{"op": "forall_steps", "pred": {"op": "equals", "path": "metadata.b", "value": 1.0}}`, true},
		{"in_set dict", `{"op": "forall_steps", "pred": {"op": "in_set", "path": "metadata", "values": [{"z": false, "l": [true, 1], "b": 1, "n": true}]}}`, true},
		{"equals text", `{"op": "forall_steps", "pred": {"op": "equals", "path": "command", "value": "ls"}}`, true},
		{"equals null", `{"op": "forall_steps", "pred": {"op": "equals", "path": "metadata.z", "value": null}}`, false},
		{"equals text number", `{"op": "forall_steps", "pred": {"op": "equals", "path": "metadata.n", "value": "1"}}`, false},
	}
	plan, err := ParsePlan(json.RawMessage(`{"id": "p", "source": "s", "task": "t", "steps": ` + steps + `}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		expr, err := ParseExpression(json.RawMessage(c.expr))
		if err != nil {
			t.Fatal(err)
		}
		got, err := EvaluatePredicate(expr, plan, Envelope{})
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
