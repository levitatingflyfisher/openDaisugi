package fleet

import (
	"strings"
	"testing"

	sprig "github.com/opendaisugi/sprig"
)

func TestCockpitNavigationClamps(t *testing.T) {
	f := New()
	f.Add("a", "1")
	f.Add("b", "2")
	f.Add("c", "3")
	c := &Cockpit{F: f}
	c.Key('k') // at top, stays
	if c.Sel != 0 {
		t.Fatalf("k at top: sel=%d", c.Sel)
	}
	c.Key('j')
	c.Key('j')
	if c.Sel != 2 {
		t.Fatalf("two j: sel=%d", c.Sel)
	}
	c.Key('j') // at bottom, clamps
	if c.Sel != 2 {
		t.Fatalf("j at bottom: sel=%d", c.Sel)
	}
}

func TestCockpitQuit(t *testing.T) {
	if !(&Cockpit{F: New()}).Key('q') {
		t.Fatal("q must quit")
	}
}

func TestCockpitApproveUnblocksSelectedAgent(t *testing.T) {
	f := New()
	f.Add("j", "risky")
	spy := &spyTool{}
	go f.Run(func(job *Job) *sprig.Agent {
		return &sprig.Agent{
			Model:    &twoTurn{call: sprig.ToolCall{Name: "spy"}, final: "done"},
			Exec:     sprig.NewExecutor(map[string]sprig.Tool{"spy": spy}, EscalatingGate{Base: sprig.DenyAll{Reason: "no"}, Fleet: f, JobID: "j"}),
			MaxTurns: 5,
		}
	})
	waitFor(t, "blocked", func() bool { return stateOf(f, "j") == "blocked" })
	c := &Cockpit{F: f, Sel: 0}
	c.Key('a') // approve the selected, blocked agent via the keyboard
	waitFor(t, "done", func() bool { return stateOf(f, "j") == "done" })
	if !spy.ran {
		t.Fatal("keyboard approve must let the flagged call run")
	}
}

func TestRenderCockpitMarksSelection(t *testing.T) {
	jobs := []Job{{ID: "a", State: "running"}, {ID: "b", State: "blocked"}}
	out := RenderCockpit(jobs, 1, false)
	if !strings.Contains(out, ">") {
		t.Fatalf("selection cursor missing:\n%s", out)
	}
	// keys still printed (doctrine)
	if !strings.Contains(out, "approve") {
		t.Fatal("keybindings must stay on screen")
	}
}
