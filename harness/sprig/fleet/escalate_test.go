package fleet

import (
	"testing"
	"time"

	sprig "github.com/opendaisugi/sprig"
)

// twoTurn: turn 1 requests a tool, turn 2 finishes.
type twoTurn struct {
	i     int
	call  sprig.ToolCall
	final string
}

func (m *twoTurn) Next([]sprig.Message) (sprig.Message, error) {
	m.i++
	if m.i == 1 {
		return sprig.Message{Role: "assistant", Calls: []sprig.ToolCall{m.call}}, nil
	}
	return sprig.Message{Role: "assistant", Text: m.final}, nil
}

type spyTool struct{ ran bool }

func (s *spyTool) Name() string                       { return "spy" }
func (s *spyTool) Run(map[string]any) (string, error) { s.ran = true; return "did it", nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func stateOf(f *Fleet, id string) string {
	for _, j := range f.Snapshot() {
		if j.ID == id {
			return j.State
		}
	}
	return ""
}

func TestEscalatingGateOnlyBothersYouWhenTheEnvelopeFlagsIt(t *testing.T) {
	// Base ALLOWS → no escalation, the human is never involved.
	f := New()
	f.Add("ok", "safe task")
	spy := &spyTool{}
	f.Run(func(j *Job) *sprig.Agent {
		return &sprig.Agent{
			Model:    &twoTurn{call: sprig.ToolCall{Name: "spy"}, final: "done"},
			Exec:     sprig.NewExecutor(map[string]sprig.Tool{"spy": spy}, EscalatingGate{Base: sprig.AllowAll{}, Fleet: f, JobID: j.ID}),
			MaxTurns: 5,
		}
	})
	if !spy.ran || stateOf(f, "ok") != "done" {
		t.Fatal("an envelope-allowed call must run with no human ruling")
	}
}

func TestEscalatingApproveOverridesADenial(t *testing.T) {
	f := New()
	f.Add("j", "risky task")
	spy := &spyTool{}
	go f.Run(func(job *Job) *sprig.Agent {
		return &sprig.Agent{
			Model:    &twoTurn{call: sprig.ToolCall{Name: "spy"}, final: "done"},
			Exec:     sprig.NewExecutor(map[string]sprig.Tool{"spy": spy}, EscalatingGate{Base: sprig.DenyAll{Reason: "outside envelope"}, Fleet: f, JobID: "j"}),
			MaxTurns: 5,
		}
	})
	// the envelope flagged it → the job blocks, waiting on you
	waitFor(t, "job blocked", func() bool { return stateOf(f, "j") == "blocked" })
	p, ok := f.Pending("j")
	if !ok || p.Call.Name != "spy" {
		t.Fatalf("the pending call should be visible to the operator, got %+v", p)
	}
	if spy.ran {
		t.Fatal("the tool must NOT run before you rule")
	}
	f.Approve("j") // you override
	waitFor(t, "job done", func() bool { return stateOf(f, "j") == "done" })
	if !spy.ran {
		t.Fatal("approve must let the flagged call run")
	}
}

func TestEscalatingDenyUpholdsTheRefusal(t *testing.T) {
	f := New()
	f.Add("j", "risky")
	spy := &spyTool{}
	go f.Run(func(job *Job) *sprig.Agent {
		return &sprig.Agent{
			Model:    &twoTurn{call: sprig.ToolCall{Name: "spy"}, final: "ok, stopped"},
			Exec:     sprig.NewExecutor(map[string]sprig.Tool{"spy": spy}, EscalatingGate{Base: sprig.DenyAll{Reason: "no"}, Fleet: f, JobID: "j"}),
			MaxTurns: 5,
		}
	})
	waitFor(t, "blocked", func() bool { return stateOf(f, "j") == "blocked" })
	f.Deny("j")
	waitFor(t, "done", func() bool { return stateOf(f, "j") == "done" })
	if spy.ran {
		t.Fatal("deny must keep the tool from running")
	}
}
