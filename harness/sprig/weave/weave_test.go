package weave

import (
	"testing"

	sprig "github.com/opendaisugi/sprig"
)

// echoModel finishes in one turn, echoing a fixed answer.
type echoModel struct{ answer string }

func (m echoModel) Next([]sprig.Message) (sprig.Message, error) {
	return sprig.Message{Role: "assistant", Text: m.answer}, nil
}

const shipPR = `{
  "name": "ship-pr",
  "steps": [
    {"id": "test", "task": "run tests"},
    {"id": "changelog", "task": "write changelog", "needs": ["test"]},
    {"id": "pr", "task": "open PR", "needs": ["changelog"]}
  ]
}`

func TestParseValidWorkflow(t *testing.T) {
	wf, err := Parse([]byte(shipPR))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Name != "ship-pr" || len(wf.Steps) != 3 {
		t.Fatalf("bad parse: %+v", wf)
	}
}

func TestParseRejectsCycles(t *testing.T) {
	bad := `{"name":"x","steps":[{"id":"a","needs":["b"]},{"id":"b","needs":["a"]}]}`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("a cycle must be rejected — the grammar is a safety fence")
	}
}

func TestParseRejectsDanglingNeed(t *testing.T) {
	bad := `{"name":"x","steps":[{"id":"a","needs":["ghost"]}]}`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("a need on a non-existent step must be rejected")
	}
}

func TestRunExecutesInDependencyOrder(t *testing.T) {
	wf, _ := Parse([]byte(shipPR))
	results, err := wf.Run(func(s Step) *sprig.Agent {
		return &sprig.Agent{
			Model:    echoModel{answer: "did:" + s.ID},
			Exec:     sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}),
			MaxTurns: 3,
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	order := []string{}
	for _, r := range results {
		order = append(order, r.StepID)
		if r.Output != "did:"+r.StepID {
			t.Fatalf("step %s output %q", r.StepID, r.Output)
		}
	}
	// test must precede changelog must precede pr
	if !(indexOf(order, "test") < indexOf(order, "changelog") && indexOf(order, "changelog") < indexOf(order, "pr")) {
		t.Fatalf("steps ran out of dependency order: %v", order)
	}
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
