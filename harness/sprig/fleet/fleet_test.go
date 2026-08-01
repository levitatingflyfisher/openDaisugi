package fleet

import (
	"sync"
	"testing"

	sprig "github.com/opendaisugi/sprig"
)

// finishModel answers in one turn with no tool calls.
type finishModel struct{ answer string }

func (m finishModel) Next([]sprig.Message) (sprig.Message, error) {
	return sprig.Message{Role: "assistant", Text: m.answer}, nil
}

func TestFleetRunsJobsConcurrentlyAndFusesState(t *testing.T) {
	f := New()
	f.Add("a", "task a")
	f.Add("b", "task b")
	f.Add("c", "task c")

	f.Run(func(job *Job) *sprig.Agent {
		return &sprig.Agent{
			Model:    finishModel{answer: "answer for " + job.ID},
			Exec:     sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}),
			MaxTurns: 3,
		}
	})

	snap := f.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("want 3 jobs, got %d", len(snap))
	}
	for _, j := range snap {
		if j.State != "done" {
			t.Fatalf("job %s state = %q, want done", j.ID, j.State)
		}
		if j.Answer != "answer for "+j.ID {
			t.Fatalf("job %s answer = %q", j.ID, j.Answer)
		}
	}
}

func TestSnapshotIsSafeUnderConcurrentReads(t *testing.T) {
	f := New()
	for _, id := range []string{"x", "y", "z"} {
		f.Add(id, "t")
	}
	// hammer Snapshot while a run mutates state — must not race or panic
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.Run(func(j *Job) *sprig.Agent {
			return &sprig.Agent{Model: finishModel{"ok"}, Exec: sprig.NewExecutor(sprig.DefaultTools(), sprig.AllowAll{}), MaxTurns: 2}
		})
	}()
	for i := 0; i < 50; i++ {
		_ = f.Snapshot()
	}
	wg.Wait()
}
