package verify

import (
	"encoding/json"
	"testing"
)

func withLLM(t *testing.T, f func(rule, payload string) LLMVerdict) {
	old := LLM
	LLM = f
	t.Cleanup(func() { LLM = old })
}

const llmPlan = `{"id": "p", "source": "s", "task": "t é", "steps": [` +
	`{"id": "s1", "depends_on": [], "metadata": {}, "postcondition": null, "preferred_model": null,` +
	` "type": "shell", "command": "echo hi"}]}`

// The payload is json.dumps({"task": ..., "steps": [...]}): each step's
// keys in the order they were read, non-ASCII escaped.
func TestLLMCheckPayload(t *testing.T) {
	var got string
	withLLM(t, func(rule, payload string) LLMVerdict {
		got = rule + "|" + payload
		return LLMVerdict{Satisfied: true}
	})
	plan, err := ParsePlan(json.RawMessage(llmPlan))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := EvaluatePredicate(LLMCheck{Rule: "kind"}, plan, Envelope{})
	if err != nil || !ok {
		t.Fatalf("got %v %v", ok, err)
	}
	want := `kind|{"task": "t \u00e9", "steps": [{"id": "s1", "depends_on": [], "metadata": {}, ` +
		`"postcondition": null, "preferred_model": null, "type": "shell", "command": "echo hi"}]}`
	if got != want {
		t.Fatalf("payload\n got %s\nwant %s", got, want)
	}
}

func TestLLMCheckFailures(t *testing.T) {
	plan, _ := ParsePlan(json.RawMessage(llmPlan))
	withLLM(t, func(string, string) LLMVerdict {
		return LLMVerdict{Errored: true, Reason: "error: llm_check call failed: boom"}
	})
	_, err := EvaluatePredicate(LLMCheck{Rule: "r"}, plan, Envelope{})
	if err == nil || err.Error() != "error: llm_check call failed: boom" || !WordedEvalError(err) {
		t.Fatalf("got %v", err)
	}
	withLLM(t, func(string, string) LLMVerdict { return LLMVerdict{Unported: "an llm_check under SSL_CERT_FILE"} })
	inv := `{"type": "j", "description": "d", "expr": {"op": "llm_check", "rule": "r"}}`
	var env Envelope
	if err := json.Unmarshal([]byte(`{"id": "e", "generated_by": "g", "task": "t", "permissions": {}, "invariants": [`+inv+`]}`), &env); err != nil {
		t.Fatal(err)
	}
	vs := CheckPredicateInvariants(plan, env, false, nil)
	if len(vs) != 1 || vs[0].Known ||
		vs[0].Message != "invariant 'j' evaluation error: an llm_check under SSL_CERT_FILE is not in this binary yet" {
		t.Fatalf("got %+v", vs)
	}
	withLLM(t, func(string, string) LLMVerdict { return LLMVerdict{Satisfied: false} })
	vs = CheckPredicateInvariants(plan, env, false, nil)
	if len(vs) != 1 || !vs[0].Known || vs[0].Message != "invariant 'j' violated" ||
		vs[0].Detail.Value("description") != "d" {
		t.Fatalf("got %+v", vs)
	}
	env.Stakes = "physical"
	_, err = EvaluatePredicate(LLMCheck{Rule: "r"}, plan, env)
	if err == nil || err.Error() != "llm_check blocked for physical stakes — use sound primitives only" {
		t.Fatalf("got %v", err)
	}
}

// An unresolved alias is worded with Python's quotes.
func TestUnresolvedAliasWords(t *testing.T) {
	plan, _ := ParsePlan(json.RawMessage(llmPlan))
	_, err := EvaluatePredicate(Not{Child: AliasRef{Name: "no_secrets"}}, plan, Envelope{})
	if err == nil || err.Error() != "unresolved alias reference 'no_secrets'; resolve aliases before evaluation" {
		t.Fatalf("got %v", err)
	}
}
