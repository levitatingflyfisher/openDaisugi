package fleet

import sprig "github.com/opendaisugi/sprig"

// Pending is a tool call the envelope flagged, now awaiting a human ruling.
type Pending struct {
	Call   sprig.ToolCall
	Reason string // why the envelope refused it
}

// EscalatingGate is "the gate as a game event." It wraps a base gate (the
// openDaisugi envelope): calls the envelope ALLOWS run automatically, so the
// human is never bothered by safe work; only a call the envelope REFUSES pauses
// the agent and surfaces on the fleet board for you to approve (override) or
// deny (uphold). That keeps the operator's locus of attention on the few real
// decisions, not on every read and echo (Raskin).
type EscalatingGate struct {
	Base  sprig.Gate
	Fleet *Fleet
	JobID string
}

func (g EscalatingGate) Check(call sprig.ToolCall) sprig.Verdict {
	v := g.Base.Check(call)
	if v.Allow {
		return v // the envelope cleared it — no human needed
	}
	if g.Fleet.escalate(g.JobID, Pending{Call: call, Reason: v.Reason}) {
		return sprig.Verdict{Allow: true, Reason: "operator override"}
	}
	return sprig.Verdict{Allow: false, Reason: "operator upheld: " + v.Reason}
}

// escalate records the flagged call, marks the job blocked, and BLOCKS the
// agent's goroutine until Approve/Deny rules on it. Returns true on approve.
func (f *Fleet) escalate(id string, p Pending) bool {
	f.mu.Lock()
	f.pending[id] = p
	if j := f.jobs[id]; j != nil {
		j.State = "blocked"
	}
	ch := make(chan bool, 1)
	f.decisions[id] = ch
	f.mu.Unlock()

	approved := <-ch // wait for the operator

	f.mu.Lock()
	delete(f.pending, id)
	delete(f.decisions, id)
	f.mu.Unlock()
	return approved
}

// Pending returns the call a job is waiting on you to rule, if any.
func (f *Fleet) Pending(id string) (Pending, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pending[id]
	return p, ok
}

// Approve overrides the envelope and lets the flagged call run.
func (f *Fleet) Approve(id string) { f.decide(id, true) }

// Deny upholds the envelope's refusal.
func (f *Fleet) Deny(id string) { f.decide(id, false) }

func (f *Fleet) decide(id string, ok bool) {
	f.mu.Lock()
	ch := f.decisions[id]
	f.mu.Unlock()
	if ch != nil {
		ch <- ok
	}
}
