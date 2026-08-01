// Package fleet runs and supervises many sprig agents at once. The measured pain
// point of many-agent work is status visibility (harness-meta §3); fleet solves
// it by STATE FUSION — each agent reports its own transitions through sprig's
// Observer, so the board is always current without scraping tmux or parsing logs.
// This is grove's engine; the TUI is a thin renderer over Snapshot().
package fleet

import (
	"sync"

	sprig "github.com/opendaisugi/sprig"
)

// Job is one agent's task and live status on the board.
type Job struct {
	ID     string
	Task   string
	State  string // idle | running | tool:<name> | blocked | done | failed
	Answer string
	Err    error
}

// NeedsYou reports whether a job is waiting on the human — the one question a
// fleet view exists to answer at a glance (Raskin: keep the locus of attention
// on the decision, not the plumbing).
func (j Job) NeedsYou() bool { return j.State == "blocked" }

// Fleet is a thread-safe board of jobs.
type Fleet struct {
	mu        sync.Mutex
	jobs      map[string]*Job
	order     []string
	pending   map[string]Pending   // jobID → a call awaiting a human ruling
	decisions map[string]chan bool // jobID → the channel escalate() blocks on
}

func New() *Fleet {
	return &Fleet{
		jobs:      map[string]*Job{},
		pending:   map[string]Pending{},
		decisions: map[string]chan bool{},
	}
}

// Add registers a job (idle until Run launches it).
func (f *Fleet) Add(id, task string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		f.order = append(f.order, id)
	}
	f.jobs[id] = &Job{ID: id, Task: task, State: "idle"}
}

func (f *Fleet) setState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[id]; j != nil {
		j.State = state
	}
}

// jobObserver forwards one agent's transitions onto the board.
type jobObserver struct {
	fleet *Fleet
	id    string
}

func (o *jobObserver) OnState(s string) { o.fleet.setState(o.id, s) }

// Run launches every job concurrently — one goroutine each — on an agent built
// by newAgent (which wires the model + gate). It attaches a per-job observer so
// state fuses live, and blocks until all jobs finish.
func (f *Fleet) Run(newAgent func(job *Job) *sprig.Agent) {
	f.mu.Lock()
	ids := append([]string(nil), f.order...)
	f.mu.Unlock()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			f.mu.Lock()
			job := f.jobs[id]
			f.mu.Unlock()

			agent := newAgent(job)
			agent.Observer = &jobObserver{fleet: f, id: id}
			ans, err := agent.Run(job.Task)

			f.mu.Lock()
			job.Answer, job.Err = ans, err
			if err != nil {
				job.State = "failed"
			} else {
				job.State = "done"
			}
			f.mu.Unlock()
		}(id)
	}
	wg.Wait()
}

// Snapshot returns a thread-safe copy of the board, in insertion order.
func (f *Fleet) Snapshot() []Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Job, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, *f.jobs[id])
	}
	return out
}
