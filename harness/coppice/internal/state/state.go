// Package state holds master spec section 3.1 in one place: the precedence
// between sources, the gate's hold on blocked, and the rule that idle is
// never a default. Every path into a pane's state goes through Merge; every
// path that reads a cached state with nothing fresh to merge against goes
// through EffectiveState. Amended 2026-09-09: the two-second precedence
// window now holds back only an incoming manifest event (every other source
// is a fact and applies at once), and a gate blocked hold releases only to a
// gate event, a terminal done, or the ask's deadline - not to an operator
// fact. Go's Merge is required to agree with the Python reference
// (opendaisugi.floor.events.merge / effective_state) on every input; where
// this file still diverges from that reference on purpose, the divergence is
// called out in the doc comment for the function that carries it.
package state

import (
	"sync"

	"github.com/opendaisugi/coppice/internal/proto"
)

// HoldWindow is the "within 2 s" of master spec 3.1. Amended 2026-09-09: it
// applies ONLY to an incoming manifest event - manifest is the fallback
// source with no real signal of its own, so it alone may lose a race to a
// still-fresh higher-precedence current state. Every other source (operator,
// gate, headless, process) is a fact about what actually happened and always
// applies at once, window or not. Seconds.
const HoldWindow = 2.0

// endsTheSession reports whether this event is the one kind that outranks every
// hold. Master spec 3.1: done comes only from a process exit or a headless
// end-of-session event, and it is terminal. Nothing may swallow it, because a
// swallowed done leaves a dead agent reading "working" for ever: watchExit and
// pumpAdapter each fire exactly once and never retry.
func endsTheSession(in proto.PaneStateEvent) bool {
	return in.State == proto.StateDone &&
		(in.Source == proto.SrcProcess || in.Source == proto.SrcHeadless)
}

// Merge folds one incoming event into a pane's current state, in the order
// master spec 3.1 (amended 2026-09-09) requires - the numbering below
// matches the order the checks appear in the code, including the
// Go-specific rule 2 that has no Python counterpart:
//
//  1. cur == nil: the incoming event is the whole state; return it.
//  2. cur.State is already done: return cur unconditionally, absorbing
//     everything after it. This rule is a deliberate divergence from the
//     Python reference - see "Divergences from Python" below.
//  3. A done from process or headless is terminal - it wins at once, before
//     the gate hold and before the precedence window, because nothing ever
//     re-sends it. A done from any other source was already rejected by
//     proto.Validate; if it reaches here anyway (a caller that skipped
//     Validate), it is downgraded to unknown from that same source rather
//     than silently dropped or accepted as done, because the source did
//     observe something. This downgrade is a second divergence from
//     Python - see below.
//  4. A gate blocked (which always carries an ask) holds until a gate event
//     clears it, or its ask's deadline has passed. While it holds, every
//     other incoming source - INCLUDING operator - gets cur back: the ask's
//     answer arrives through the gate, not through an arbitrary operator
//     fact. Once the deadline has passed the hold releases and this falls
//     through to rule 5 below; it does NOT synthesize a "working" event
//     here - the gate always sends its own working report the moment its
//     wait resolves, and a reader with nothing fresh to merge against calls
//     EffectiveState instead.
//  5. Ordinary precedence: the 2 s window holds back cur only when the
//     incoming event's source is manifest AND cur's source outranks
//     manifest AND the hold window (measured on the server's receive clock)
//     has not yet elapsed. Every other source is a fact and applies at
//     once. This is a third divergence from Python - see below.
//
// Divergences from Python (opendaisugi.floor.events.merge), each named at
// its owning rule above:
//
//   - Rule 2, the done-absorbing guard: Python's merge has no matching
//     check - its only terminality logic looks at the INCOMING side
//     (`incoming.state == "done" and incoming.source in (...)`), so once
//     current.state is already "done", any later incoming event not itself
//     held by the manifest window overrides it there, at any delay: e.g.
//     cur=done/process, in=working/operator, any now, returns
//     working/operator in Python but done/process here. Master spec 3.1
//     says done "is terminal" outright, so Go's TestDoneIsTerminal requires
//     the explicit guard and Go is the fail-closed side of this
//     disagreement; the Python side may need a matching fix.
//   - Rule 3, the ineligible-done downgrade: has no Python counterpart, but
//     for a structural reason rather than a behavioral one - Python's
//     PaneStateEvent is a frozen dataclass whose __post_init__ raises
//     ValueError for a done from any source but process or headless, so
//     Python's merge() can never be CALLED with such an event in the first
//     place. Go's proto.PaneStateEvent carries no such constructor guard
//     (ParseStateEvent enforces the rule for parsed input, via Validate,
//     but a caller can still build the struct literal directly), so this
//     rule exists purely as defense in depth for that path.
//   - Rule 5, the hold-window clock: Python's merge() gates the window on
//     `now - current.ts` - the CURRENT event's own self-reported timestamp,
//     which a hook with a skewed clock controls. Go gates on
//     `now - curReceived`, the server's own receive clock, which no hook
//     can influence. On TestTheHoldWindowIgnoresASkewedEventTimestamp's
//     input (a cur.TS nine billion seconds in the future, but received by
//     the server at curReceived=100), Python would still be holding at
//     now=103, and Go is not. Go is the correct side here per master spec
//     3.1 as amended ("a hook with a skewed clock must not be able to
//     extend its own hold"); Python is the one that needs to be aligned.
//
// curReceived is the server's own clock when the current event arrived, and
// now is the server's clock for this one. Neither is the event's own ts: a
// hook with a skewed clock must not be able to extend its own hold.
func Merge(cur *proto.PaneStateEvent, curReceived float64,
	in proto.PaneStateEvent, now float64) proto.PaneStateEvent {

	// Only process and headless may say done at all (master spec 3.1).
	// proto.Validate already rejects a done from any other source before it
	// reaches here - this is defense in depth for a caller that skipped
	// Validate, including a manifest, which the spec singles out by name as
	// the source with the least standing to claim a session ended.
	if in.State == proto.StateDone && !endsTheSession(in) {
		in.State = proto.StateUnknown
		in.Ask = nil
	}

	if cur == nil {
		return in
	}

	// Rule 2: done is terminal. The process is gone; nothing observed later
	// is about a running agent. Go-specific - see "Divergences from Python"
	// in the doc comment above.
	if cur.State == proto.StateDone {
		return *cur
	}

	// The session ending outranks every hold, whatever the current state is.
	// This is checked before the gate branch and before the precedence
	// window, because a gate WORKING hold would otherwise discard a process
	// exit that arrives inside the two-second window.
	if endsTheSession(in) {
		return in
	}

	// A gate hold: blocked from the gate stands until the gate itself clears
	// it, or the ask's deadline passes. Amended 2026-09-09: the operator does
	// NOT clear it either - only a gate event does, because the ask's answer
	// is a gate event, not an arbitrary operator fact.
	if cur.State == proto.StateBlocked && cur.Source == proto.SrcGate {
		if in.Source == proto.SrcGate {
			return in
		}
		expired := cur.Ask != nil && now >= cur.Ask.Deadline
		if !expired {
			return *cur
		}
		// The hold has lapsed. Fall through to ordinary precedence below -
		// do not synthesize a "working" event here (that is EffectiveState's
		// job for a reader with no fresh event to merge against).
	}

	// Ordinary precedence. Amended 2026-09-09: the two-second window holds
	// back cur only when the incoming event is from manifest. Every other
	// source - headless, process, operator, gate - is a fact about what
	// actually happened and applies at once, even inside the window and even
	// against a higher-rank current state: holding one of those back would
	// swallow a true state change and show a stale pane. The window is
	// measured on the server's receive clock, never on the event's own ts,
	// so a hook with a skewed clock cannot extend its own hold.
	if in.Source == proto.SrcManifest &&
		proto.SourceRank(in.Source) < proto.SourceRank(cur.Source) &&
		now-curReceived < HoldWindow {
		return *cur
	}
	return in
}

// EffectiveState is the read-time counterpart to Merge's expiry rule. Merge
// only releases a gate-blocked hold when a FRESH incoming event actually
// arrives; a reader holding nothing but a cached event - coppice-server
// reading the last known state, a cockpit roster view - has no fresh event to
// merge against. This returns cur unchanged unless it is blocked with an ask
// whose deadline has passed, in which case it returns a COPY with
// State = working, Ask = nil and Detail = "ask deadline passed", so a stale
// blocked hold never displays forever.
//
// This is not restricted to source == gate: any blocked event carrying an
// ask expires the same way once its deadline passes - a headless- or
// process-sourced block with a stale ask is just as stale as a gate one. A
// blocked event with no ask never expires here, matching Merge's own hold.
func EffectiveState(cur *proto.PaneStateEvent, now float64) *proto.PaneStateEvent {
	if cur == nil {
		return cur
	}
	if cur.State != proto.StateBlocked {
		return cur
	}
	if cur.Ask == nil || now < cur.Ask.Deadline {
		return cur
	}
	out := *cur
	out.State = proto.StateWorking
	out.Ask = nil
	out.Detail = "ask deadline passed"
	return &out
}

// Store is the per-pane current state. It is safe for concurrent use: the PTY
// pump, the manifest tick and the socket dispatcher all write to it.
type Store struct {
	mu       sync.RWMutex
	cur      map[string]proto.PaneStateEvent
	received map[string]float64
}

func NewStore() *Store {
	return &Store{cur: map[string]proto.PaneStateEvent{}, received: map[string]float64{}}
}

// Apply merges one event and reports whether the pane's visible state or
// source changed, so callers only broadcast real news. The receive clock
// advances only when the state actually changed: a chatty source repeating
// itself must not keep the screen scanner away for ever.
//
// merged is often literally *cur or in (Merge returns one of them unchanged
// - see Merge's doc comment), so its pointer fields (Ask, and if ever set,
// HarnessSessionID and Pane) can alias a struct the caller's own in.Ask
// still points at, or the struct already sitting in s.cur[pane]. The map
// entry and the returned value are each given their own clone of every
// pointer field, so a caller mutating the value Apply handed
// back - e.g. merged.Ask.Deadline = ... - can never reach into the store,
// and a later Apply on the same pane can never reach into a value an
// earlier caller is still holding.
func (s *Store) Apply(pane string, in proto.PaneStateEvent, now float64) (proto.PaneStateEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev *proto.PaneStateEvent
	prevReceived := 0.0
	if p, ok := s.cur[pane]; ok {
		prev = &p
		prevReceived = s.received[pane]
	}
	merged := Merge(prev, prevReceived, in, now)
	changed := prev == nil || prev.State != merged.State || prev.Source != merged.Source ||
		askChanged(prev.Ask, merged.Ask)
	s.cur[pane] = clonePointerFields(merged)
	if changed {
		s.received[pane] = now
	}
	return clonePointerFields(merged), changed
}

// clonePointerFields returns ev with its pointer fields (HarnessSessionID,
// Pane, Ask) replaced by copies pointing at their own memory, rather than at
// whatever the caller or the Store already holds. See Apply's doc comment.
func clonePointerFields(ev proto.PaneStateEvent) proto.PaneStateEvent {
	if ev.HarnessSessionID != nil {
		v := *ev.HarnessSessionID
		ev.HarnessSessionID = &v
	}
	if ev.Pane != nil {
		v := *ev.Pane
		ev.Pane = &v
	}
	if ev.Ask != nil {
		v := *ev.Ask
		ev.Ask = &v
	}
	return ev
}

func askChanged(a, b *proto.Ask) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil || b == nil:
		return true
	default:
		return a.ID != b.ID
	}
}

// Current returns a clone: e's pointer fields (HarnessSessionID, Pane, Ask)
// point at their own memory, not at whatever is stored in s.cur[pane]. A
// caller that hands e.Ask straight into a client response - pane.list
// does exactly this - or that mutates it, must never be able to
// reach the store, or race a later Apply for the same pane.
func (s *Store) Current(pane string) (proto.PaneStateEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.cur[pane]
	if !ok {
		return proto.PaneStateEvent{}, false
	}
	return clonePointerFields(e), true
}

// Received is the server clock when this pane's current state arrived. The
// manifest tick uses it, never the event's own ts.
func (s *Store) Received(pane string) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.received[pane]
	return r, ok
}

func (s *Store) Forget(pane string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cur, pane)
	delete(s.received, pane)
}

func (s *Store) Panes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.cur))
	for k := range s.cur {
		out = append(out, k)
	}
	return out
}
