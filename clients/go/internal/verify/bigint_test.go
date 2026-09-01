package verify

import (
	"strings"
	"testing"
)

// An int of any size is a valid int to pydantic, so the parsers read it
// and the verifier judges it, as the oracle does.
func TestParseEnvelopeReadsIntsOfAnySize(t *testing.T) {
	requireZ3(t)
	for _, n := range []string{"100000000000000000000", "-100000000000000000000", "1" + strings.Repeat("0", 400)} {
		env, err := ParseEnvelope([]byte(`{"id": "e", "generated_by": "t", "task": "t", "permissions": {` +
			`"max_execution_time_s": ` + n + `, "max_output_size_mb": ` + n + `}, "postconditions": [` +
			`{"type": "exit_code", "expected": ` + n + `, "min": ` + n + `, "max": ` + n + `}]}`))
		if err != nil {
			t.Fatalf("%s: %v", n[:5], err)
		}
		v, timeout := CheckEnvelopeSelfConsistency(env, 500)
		if timeout != "" || len(v) != 1 || v[0].Message != "Envelope is internally inconsistent" {
			t.Fatalf("%s: want the inconsistent violation, got %+v %q", n[:5], v, timeout)
		}
	}
	env, err := ParseEnvelope([]byte(`{"id": "e", "generated_by": "t", "task": "t", "permissions": {"max_execution_time_s": 3600}}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := CheckEnvelopeSelfConsistency(env, 500); len(v) != 0 {
		t.Fatalf("3600 is in range, got %+v", v)
	}
	if v, _ := CheckEnvelopeSelfConsistency(DefaultEnvelope(), 500); len(v) != 0 {
		t.Fatalf("the default 30 is in range, got %+v", v)
	}
}

func TestDefaultEnvelopesDoNotShareTheirMaxTime(t *testing.T) {
	a, err := ParseEnvelope([]byte(`{"permissions": {"max_execution_time_s": 7}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := DefaultPermission().MaxExecutionTimeS.String(); got != "30" || a.Permissions.MaxExecutionTimeS.String() != "7" {
		t.Fatalf("default %s, parsed %s", got, a.Permissions.MaxExecutionTimeS)
	}
}

func TestParsePlanReadsNumbersOfAnySize(t *testing.T) {
	big := "1" + strings.Repeat("0", 400)
	p, err := ParsePlan([]byte(`{"id": "p", "source": "s", "task": "t", "steps": [{"id": "a", "type": "shell", ` +
		`"command": "ls", "metadata": {"n": ` + big + `, "m": [` + big + `, 1.5]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	md := p.Steps[0].Raw["metadata"].(map[string]interface{})
	if md["m"].([]interface{})[1] != 1.5 {
		t.Fatalf("a small number stays a float64, got %#v", md["m"])
	}
}
