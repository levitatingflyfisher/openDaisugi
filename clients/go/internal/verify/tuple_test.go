package verify

import (
	"encoding/json"
	"testing"
)

// The oracle's step dicts are model_dump() output, which keeps a
// cartesian_move's target_position and target_orientation and a vla's
// target_pose as tuples. A tuple equals no list, so a value compared over
// one of them is answered as Python answers it (PW-R-11).
func TestTupleFieldsCompareAsPythonTuples(t *testing.T) {
	cart := `[{"id": "c1", "type": "cartesian_move", "target_position": [1, 2, 3]}]`
	vla := `[{"id": "v1", "type": "vla", "task": "x", "target_pose": [1, 2, 3]}]`
	cases := []struct {
		name, steps, expr string
		want              bool
	}{
		{"equals", cart, `{"op": "forall_steps", "pred": {"op": "equals", "path": "target_position", "value": [1, 2, 3]}}`, false},
		{"not_equals", cart, `{"op": "forall_steps", "pred": {"op": "not_equals", "path": "target_position", "value": [1, 2, 3]}}`, true},
		{"in_set", cart, `{"op": "forall_steps", "pred": {"op": "in_set", "path": "target_position", "values": [[1, 2, 3]]}}`, false},
		{"not_in_set", cart, `{"op": "forall_steps", "pred": {"op": "not_in_set", "path": "target_position", "values": [[1, 2, 3]]}}`, true},
		{"length_range", cart, `{"op": "forall_steps", "pred": {"op": "length_range", "path": "target_position", "min": 3, "max": 3}}`, true},
		{"is_empty", cart, `{"op": "forall_steps", "pred": {"op": "is_empty", "path": "target_position"}}`, false},
		{"is_empty null orientation", cart, `{"op": "forall_steps", "pred": {"op": "is_empty", "path": "target_orientation"}}`, true},
		{"exists", cart, `{"op": "forall_steps", "pred": {"op": "exists", "path": "target_position"}}`, true},
		{"whole steps", cart, `{"op": "equals", "path": "steps", "value": [{"id": "c1", "type": "cartesian_move", "target_position": [1, 2, 3]}]}`, false},
		{"vla equals", vla, `{"op": "forall_steps", "pred": {"op": "equals", "path": "target_pose", "value": [1, 2, 3]}}`, false},
		{"vla not_equals", vla, `{"op": "forall_steps", "pred": {"op": "not_equals", "path": "target_pose", "value": [1, 2, 3]}}`, true},
	}
	for _, c := range cases {
		plan, err := ParsePlan(json.RawMessage(`{"id": "p", "source": "s", "task": "t", "steps": ` + c.steps + `}`))
		if err != nil {
			t.Fatal(err)
		}
		expr, err := ParseExpression(json.RawMessage(c.expr))
		if err != nil {
			t.Fatal(err)
		}
		got, err := EvaluatePredicate(expr, plan, Envelope{})
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	// The step itself still holds a list, for the robotics stage.
	plan, _ := ParsePlan(json.RawMessage(`{"id": "p", "source": "s", "task": "t", "steps": ` + cart + `}`))
	expr, _ := ParseExpression(json.RawMessage(`{"op": "exists", "path": "x"}`))
	_, _ = EvaluatePredicate(expr, plan, Envelope{})
	if _, ok := plan.Steps[0].Raw["target_position"].([]interface{}); !ok {
		t.Fatalf("the step's own field changed: %T", plan.Steps[0].Raw["target_position"])
	}
}
