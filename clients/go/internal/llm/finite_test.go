package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
)

// GD-16: NaN, Infinity or -Infinity anywhere in a reply's numbers is
// schema-invalid. The texts are the oracle's own (tests/test_claude_code_llm.py
// and tests/test_llm.py).
const nanEnvelopeError = "claude -p output failed Envelope validation: 2 validation errors for Envelope\n" +
	"permissions.velocity_limit\n" +
	"  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n" +
	"    For further information visit https://errors.pydantic.dev/2.13/v/finite_number\n" +
	"permissions.torque_limit\n" +
	"  Input should be a finite number [type=finite_number, input_value=-inf, input_type=float]\n" +
	"    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"

func TestParseStructuredRefusesNonFinite(t *testing.T) {
	env := Schema{Name: "Envelope", Model: pmodel.Envelope}
	for _, text := range []string{
		`{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN, "torque_limit": -Infinity}}`,
		`{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN, "torque_limit": -1e999}}`,
	} {
		if _, err := parseStructured(text, env); err == nil || err.Error() != nanEnvelopeError {
			t.Errorf("%s:\n%v", text, err)
		}
	}
	plan := `{"source": "s", "task": "t", "steps": [{"id": "a", "type": "shell", ` +
		`"command": "ls", "metadata": {"n": [1, Infinity]}}]}`
	_, err := parseStructured(plan, Schema{Name: "ActionPlan", Model: pmodel.ActionPlanReply})
	if err == nil || !strings.Contains(err.Error(), "steps.0.metadata.n.1\n  Input should be a finite number") ||
		!strings.Contains(err.Error(), "input_value=inf, input_type=float") {
		t.Fatal(err)
	}
	if _, err := parseStructured(`{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": 2.5}}`, env); err != nil {
		t.Fatal(err)
	}
}

// GD-R-6: coerce_step runs over every item before the StepBase check, so
// an invalid registered step after an item that is no step is the error
// pydantic reports (the oracle's own text).
func TestParseStructuredNamesTheInvalidStepFirst(t *testing.T) {
	plan := `{"source": "s", "task": "t", "steps": [{"type": "nope", "id": "x"}, {"type": "shell", "id": "a"}]}`
	_, err := parseStructured(plan, Schema{Name: "ActionPlan", Model: pmodel.ActionPlanReply})
	if err == nil || !strings.Contains(err.Error(), "steps.command\n  Field required [type=missing, "+
		"input_value={'type': 'shell', 'id': 'a'}, input_type=dict]") {
		t.Fatal(err)
	}
	plan2 := `{"source": "s", "task": "t", "steps": ["{'type': 'shell', 'id': 'a', 'command': 'ls'}"]}`
	out, err := parseStructured(plan2, Schema{Name: "ActionPlan", Model: pmodel.ActionPlanReply})
	if err != nil {
		t.Fatal(err)
	}
	if got := pmodel.Repr(out.Value("steps")); !strings.Contains(got, "'command': 'ls'") {
		t.Fatal(got)
	}
}

const nanReply = `{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN}}`

const nanError = "1 validation error for Envelope\n" +
	"permissions.velocity_limit\n" +
	"  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n" +
	"    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"

// fakeAnthropic answers each call with the next reply and keeps the bodies.
func fakeAnthropic(t *testing.T, replies ...string) (*Client, *[]string) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		out, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": replies[0]}},
			"stop_reason": "end_turn"})
		replies = replies[1:]
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{"OPENDAISUGI_LLM_BACKEND": "litellm", "ANTHROPIC_API_KEY": "sk-test",
		"ANTHROPIC_API_BASE": srv.URL}
	c := New(Env{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Home: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard})
	return c, &bodies
}

func TestLitellmReplyWithNaNIsReaskedThenRefused(t *testing.T) {
	c, bodies := fakeAnthropic(t, nanReply, nanReply)
	_, err := c.Structured(Call{Model: "anthropic/claude-sonnet-4-20250514", User: "go",
		Response: Schema{Name: "Envelope", Model: pmodel.Envelope}, MaxRetries: 1})
	if err == nil || err.Error() != "ValidationError: 1 validation error for Envelope (2 attempts)" {
		t.Fatal(err)
	}
	var sent struct {
		Messages []struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"messages"`
	}
	if len(*bodies) != 2 || json.Unmarshal([]byte((*bodies)[1]), &sent) != nil {
		t.Fatal(*bodies)
	}
	last := sent.Messages[len(sent.Messages)-1].Content[0].Text
	if !strings.Contains(last, nanError) {
		t.Fatal(last)
	}
}

func TestLitellmReplyAfterAReaskIsUsed(t *testing.T) {
	c, _ := fakeAnthropic(t, strings.Replace(nanReply, "NaN", "-Infinity", 1),
		`{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": 1e3}}`)
	out, err := c.Structured(Call{Model: "anthropic/claude-sonnet-4-20250514", User: "go",
		Response: Schema{Name: "Envelope", Model: pmodel.Envelope}, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v := out.Value("permissions"); v == nil {
		t.Fatal(out)
	}
}
