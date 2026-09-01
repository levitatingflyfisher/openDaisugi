package pmodel

import (
	"strings"
	"testing"
)

// An Envelope or ActionPlan with NaN, Infinity or -Infinity in any number
// is invalid (models.non_finite_error, run by the models' own
// after-validator). The texts are the oracle's.
func TestNonFiniteEnvelopeAndPlanMatchPydantic(t *testing.T) {
	cases := []struct {
		model *Model
		text  string
		want  string
	}{
		{Envelope, "{\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {\"velocity_limit\": NaN}}",
			"1 validation error for Envelope\npermissions.velocity_limit\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{Envelope, "{\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {\"velocity_limit\": NaN, \"joint_limits\": {\"j\": [-Infinity, 1]}, \"torque_limit\": 1e999}}",
			"3 validation errors for Envelope\npermissions.velocity_limit\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number\npermissions.joint_limits.j.0\n  Input should be a finite number [type=finite_number, input_value=-inf, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number\npermissions.torque_limit\n  Input should be a finite number [type=finite_number, input_value=inf, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{Envelope, "{\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {\"torque_limit\": \" inf \"}}",
			"1 validation error for Envelope\npermissions.torque_limit\n  Input should be a finite number [type=finite_number, input_value=inf, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{Envelope, "{\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {\"workspace_bounds\": [[0,0,0],[1,1,10000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000]]}}",
			"1 validation error for Envelope\npermissions.workspace_bounds.1.2\n  Input should be a finite number [type=finite_number, input_value=inf, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{Envelope, "{\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {}, \"invariants\": [{\"type\": \"t\", \"description\": \"d\", \"expr\": {\"op\": \"equals\", \"path\": \"x\", \"value\": [1, NaN]}}]}",
			"1 validation error for Envelope\ninvariants.0.expr.value.1\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{Envelope, "{\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {\"velocity_limit\": NaN}, \"stakes\": \"nope\"}",
			"1 validation error for Envelope\nstakes\n  Input should be 'low', 'medium', 'high' or 'physical' [type=literal_error, input_value='nope', input_type=str]\n    For further information visit https://errors.pydantic.dev/2.13/v/literal_error"},
		{ActionPlan, "{\"source\": \"s\", \"task\": \"t\", \"steps\": [{\"type\": \"shell\", \"id\": \"a\", \"command\": \"ls\", \"metadata\": {\"n\": [1, NaN]}}]}",
			"1 validation error for ActionPlan\nsteps.0.metadata.n.1\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{ActionPlan, "{\"source\": \"s\", \"task\": \"t\", \"steps\": [{\"type\": \"joint_move\", \"id\": \"a\", \"joint_targets\": {\"x\": Infinity}}]}",
			"1 validation error for ActionPlan\nsteps.0.joint_targets.x\n  Input should be a finite number [type=finite_number, input_value=inf, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{ActionPlan, "{\"source\": \"s\", \"task\": \"t\", \"steps\": [{\"type\": \"skill\", \"id\": \"k\", \"skill_id\": \"x\", \"contract_envelope\": {\"task\": \"t\", \"generated_by\": \"g\", \"permissions\": {\"velocity_limit\": NaN}}}]}",
			"1 validation error for ActionPlan\nsteps.contract_envelope.permissions.velocity_limit\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"},
		{ActionPlan, "{\"source\": \"s\", \"task\": \"t\", \"steps\": [{\"type\": \"shell\", \"id\": \"a\"}]}",
			"1 validation error for ActionPlan\nsteps.command\n  Field required [type=missing, input_value={'type': 'shell', 'id': 'a'}, input_type=dict]\n    For further information visit https://errors.pydantic.dev/2.13/v/missing"},
	}
	for _, c := range cases {
		_, err := ValidateJSON(c.model.Name, c.model, c.text)
		if err == nil || err.String() != c.want {
			t.Errorf("%s:\n got %v\nwant %s", c.text, err, c.want)
		}
	}
}

// Nested in another model, the errors take that model's title and place.
func TestNonFiniteNestedEnvelopeTakesTheOuterPlace(t *testing.T) {
	text := `{"id": "p", "task_description": "t", "task_embedding": [0.1], "source_trace_ids": [], "distilled_at": 1.0,
		"envelope": {"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN}},
		"plan_template": {"source": "s", "task": "t", "steps": []}}`
	_, err := ValidateJSON("CompiledPathway", CompiledPathway, text)
	want := "1 validation error for CompiledPathway\nenvelope.permissions.velocity_limit\n" +
		"  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n" +
		"    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"
	if err == nil || err.String() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
}

// An int too large for a float: in JSON mode it reads as an infinity, so
// the after-validator refuses it; in Python mode float(int) overflows and
// pydantic says it is not a valid number.
func TestHugeIntInAFloatField(t *testing.T) {
	text := `{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": 1` + strings.Repeat("0", 400) + `}}`
	_, err := ValidateJSON("Envelope", Envelope, text)
	if err == nil || err.Errs[0].Type != "finite_number" {
		t.Errorf("JSON mode: got %v", err)
	}
	v, _ := ParseJSON(text)
	_, err = Validate("Envelope", Envelope, v, Python)
	want := "1 validation error for Envelope\npermissions.velocity_limit\n" +
		"  Input should be a valid number [type=float_type, input_value=1000000000000000000000000...000000000000000000000000, input_type=int]\n" +
		"    For further information visit https://errors.pydantic.dev/2.13/v/float_type"
	if err == nil || err.String() != want {
		t.Errorf("Python mode: got %v\nwant %s", err, want)
	}
}
