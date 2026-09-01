package state

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/opendaisugi/coppice/internal/proto"
)

// ev attaches an Ask whenever state is blocked, regardless of source: master
// spec 3.1 only requires an ask on a gate blocked event, but allows one on
// any blocked event, and several tests below need a blocked event from a
// non-gate source that still carries a real (expirable) ask.
func ev(state, source string, ts float64) proto.PaneStateEvent {
	e := proto.PaneStateEvent{
		V: 1, TS: ts, SessionID: "s1", Harness: "claude-code", State: state, Source: source,
	}
	if state == proto.StateBlocked {
		e.Ask = &proto.Ask{ID: "toolu_1", Tool: "Bash", Summary: "rm -rf x", Deadline: ts + 90}
	}
	return e
}

func TestFirstEventWins(t *testing.T) {
	got := Merge(nil, 0, ev(proto.StateWorking, proto.SrcManifest, 100), 100)
	if got.State != proto.StateWorking || got.Source != proto.SrcManifest {
		t.Fatalf("Merge(nil, …) = %s/%s, want working/manifest", got.State, got.Source)
	}
}

func TestManifestCannotOverrideGateWithinTheHoldWindow(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 101), 101)
	if got.Source != proto.SrcGate || got.State != proto.StateWorking {
		t.Fatalf("manifest overrode the gate at +1 s: got %s/%s", got.State, got.Source)
	}
}

func TestManifestMayOverrideGateAfterTheHoldWindow(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 103), 103)
	if got.Source != proto.SrcManifest || got.State != proto.StateIdle {
		t.Fatalf("manifest was still blocked at +3 s: got %s/%s", got.State, got.Source)
	}
}

// The hold window is measured against the server's receive time, not against a
// timestamp the caller chose. A hook with a clock a year in the future must not
// be able to hold a pane against every lower source for ever.
func TestTheHoldWindowIgnoresASkewedEventTimestamp(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 9_999_999_999) // far future ts
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 103), 103)
	if got.Source != proto.SrcManifest {
		t.Fatalf("a future event ts extended the hold: got %s/%s", got.State, got.Source)
	}
}

// Amended master spec 3.1 (2026-09-09): a `gate` blocked hold releases only
// to a `gate` event, the ask's deadline, or a terminal `done` - not to an
// operator fact. The ask's answer arrives through the gate; an `operator`
// event is a fact about something else and must wait like everyone else.
func TestGateBlockedHoldsUntilAGateEventClearsIt(t *testing.T) {
	cur := ev(proto.StateBlocked, proto.SrcGate, 100)
	// A working event from the process five seconds later must not clear a gate
	// hold. Only the gate, the deadline, or a done clears it.
	got := Merge(&cur, 100, ev(proto.StateWorking, proto.SrcProcess, 105), 105)
	if got.State != proto.StateBlocked {
		t.Fatalf("a process working event cleared a gate hold: got %s", got.State)
	}
	// Not even the operator, who outranks everything else, clears it: the
	// ask's answer is a gate event, not an operator fact.
	got = Merge(&cur, 100, ev(proto.StateWorking, proto.SrcOperator, 105), 105)
	if got.State != proto.StateBlocked {
		t.Fatalf("an operator working event cleared a gate hold: got %s", got.State)
	}
	got = Merge(&cur, 100, ev(proto.StateWorking, proto.SrcGate, 105), 105)
	if got.State != proto.StateWorking {
		t.Fatalf("the gate could not clear its own hold: got %s", got.State)
	}
}

// Amended master spec 3.1 (2026-09-09): once the ask's deadline has passed,
// Merge releases the hold and lets the next real event through as itself. It
// does NOT synthesize a "working" event here - that is EffectiveState's job,
// for a reader with nothing fresh to merge against (see
// TestAnExpiredHeadlessBlockReadsAsWorking below). The gate always sends its
// own working report the moment its wait resolves; this only stops the hold
// from swallowing whatever arrives after the deadline.
func TestGateBlockedHoldReleasesAtTheDeadlineAndTheNextEventApplies(t *testing.T) {
	cur := ev(proto.StateBlocked, proto.SrcGate, 100) // deadline 190
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 191), 191)
	if got.State != proto.StateIdle || got.Source != proto.SrcManifest {
		t.Fatalf("an expired ask still held back the next event: got %s/%s", got.State, got.Source)
	}
}

func TestProcessDoneOutranksAHeldGateBlock(t *testing.T) {
	// The process exited. There is nobody left to answer the ask.
	cur := ev(proto.StateBlocked, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcProcess, 101), 101)
	if got.State != proto.StateDone {
		t.Fatalf("process exit did not end a gate hold: got %s", got.State)
	}
}

// A fail-open this test guards: a gate WORKING event, then a process exit
// half a second later. The gate-hold branch does not apply because the current
// state is working, so the plain rank comparison used to discard the exit and
// the pane read "working" for ever. watchExit and pumpAdapter fire exactly once
// and never retry, so the loss is permanent.
func TestProcessExitEndsAGateWorkingHoldImmediately(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcProcess, 100.5), 100.5)
	if got.State != proto.StateDone || got.Source != proto.SrcProcess {
		t.Fatalf("Merge = %s/%s, want done/process: a dead process must never read working",
			got.State, got.Source)
	}
}

func TestHeadlessEndEndsAGateWorkingHoldImmediately(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcHeadless, 100.1), 100.1)
	if got.State != proto.StateDone {
		t.Fatalf("Merge = %s, want done from the headless end-of-session event", got.State)
	}
}

// Table form, so every source pair is covered rather than the two the prose
// happened to name.
func TestDoneFromProcessOrHeadlessBeatsEveryHeldSource(t *testing.T) {
	cases := []struct {
		curState, curSource string
		inSource            string
	}{
		{proto.StateWorking, proto.SrcGate, proto.SrcProcess},
		{proto.StateWorking, proto.SrcGate, proto.SrcHeadless},
		{proto.StateWorking, proto.SrcOperator, proto.SrcProcess},
		{proto.StateWorking, proto.SrcOperator, proto.SrcHeadless},
		{proto.StateBlocked, proto.SrcGate, proto.SrcProcess},
		{proto.StateBlocked, proto.SrcGate, proto.SrcHeadless},
		{proto.StateIdle, proto.SrcGate, proto.SrcProcess},
	}
	for _, c := range cases {
		cur := ev(c.curState, c.curSource, 100)
		got := Merge(&cur, 100, ev(proto.StateDone, c.inSource, 100.2), 100.2)
		if got.State != proto.StateDone {
			t.Fatalf("cur={%s,%s} in={done,%s} = %s, want done",
				c.curState, c.curSource, c.inSource, got.State)
		}
	}
}

// done from a source that may not produce it must NOT win.
// The downgrade the fuzz test has to allow for: a done nobody was entitled to
// send becomes unknown from that same source. Never idle, and never a silent
// drop, because the source did observe something.
func TestADoneFromAnIneligibleSourceBecomesUnknownFromThatSource(t *testing.T) {
	for _, src := range []string{proto.SrcOperator, proto.SrcGate} {
		got := Merge(nil, 0, ev(proto.StateDone, src, 100), 100)
		if got.State != proto.StateUnknown {
			t.Fatalf("done from %s = %s, want unknown", src, got.State)
		}
		if got.Source != src {
			t.Fatalf("the downgrade changed the source to %s, want %s", got.Source, src)
		}
		if got.Ask != nil {
			t.Fatalf("the downgrade kept an ask: %+v", got.Ask)
		}
	}
}

func TestDoneFromOperatorOrManifestDoesNotWin(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	if got := Merge(&cur, 100, ev(proto.StateDone, proto.SrcOperator, 100.2), 100.2); got.State == proto.StateDone {
		t.Fatal("an operator claimed done; only process and headless may produce it")
	}
	in := ev(proto.StateWorking, proto.SrcManifest, 100.2)
	in.State = proto.StateDone
	if got := Merge(&cur, 100, in, 100.2); got.State == proto.StateDone {
		t.Fatal("a manifest produced done, which master spec 3.1 forbids")
	}
}

func TestManifestNeverYieldsDone(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcManifest, 100)
	in := ev(proto.StateWorking, proto.SrcManifest, 200)
	in.State = proto.StateDone // a caller that ignored Validate
	got := Merge(&cur, 100, in, 200)
	if got.State == proto.StateDone {
		t.Fatal("a manifest event produced done, which master spec 3.1 forbids")
	}
}

func TestDoneIsTerminal(t *testing.T) {
	cur := ev(proto.StateDone, proto.SrcProcess, 100)
	got := Merge(&cur, 100, ev(proto.StateWorking, proto.SrcOperator, 200), 200)
	if got.State != proto.StateDone {
		t.Fatalf("a done pane came back to life: got %s", got.State)
	}
}

// Mirrored from the Python suite:
// the 2 s precedence window must hold back ONLY an incoming `manifest`
// event. A real fact from `headless` arriving a heartbeat after a gate
// 'working' must apply at once - the old precedence comparison (any
// lower-precedence incoming source held back) would have swallowed this
// idle and kept showing a stale 'working' pane.
func TestAHeadlessStopIsNotSwallowedByAFreshGateWorking(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcHeadless, 101.2), 101.2)
	if got.State != proto.StateIdle || got.Source != proto.SrcHeadless {
		t.Fatalf("a headless idle was swallowed by the gate's still-fresh working: got %s/%s",
			got.State, got.Source)
	}
}

// Same pair as above, but the incoming event is from `manifest` - the one
// source the 2 s window still holds back, since it carries no real signal of
// its own.
func TestAManifestIsStillHeldBackWithinTwoSeconds(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 101.2), 101.2)
	if got.State != proto.StateWorking || got.Source != proto.SrcGate {
		t.Fatalf("a manifest event overrode a still-fresh gate working: got %s/%s",
			got.State, got.Source)
	}
}

// The gate-blocked hold is untouched by the manifest-only window fix: it
// still holds against a lower-precedence fact until the ask's deadline
// passes (or a gate event clears it), exactly as before.
func TestGateBlockedStillHoldsAgainstAHeadlessIdle(t *testing.T) {
	cur := ev(proto.StateBlocked, proto.SrcGate, 100) // deadline 190
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcHeadless, 101.2), 101.2)
	if got.State != proto.StateBlocked {
		t.Fatalf("a headless idle cleared an unexpired gate hold: got %s", got.State)
	}
}

// EffectiveState's expiry is not restricted to source == gate: a
// headless-sourced 'blocked' with a passed ask deadline reads as
// 'working' too, for a reader with nothing fresh to merge against.
func TestAnExpiredHeadlessBlockReadsAsWorking(t *testing.T) {
	blocked := ev(proto.StateBlocked, proto.SrcHeadless, 100) // deadline 190
	got := EffectiveState(&blocked, 191)
	if got.State != proto.StateWorking || got.Ask != nil {
		t.Fatalf("an expired headless block did not read as working: %+v", got)
	}
	if got.Detail != "ask deadline passed" {
		t.Fatalf("Detail = %q, want %q", got.Detail, "ask deadline passed")
	}
	if blocked.State != proto.StateBlocked {
		t.Fatal("EffectiveState mutated the event it was given")
	}
}

// The operator is a fact like any other non-manifest source: it applies at
// once even inside the 2 s window, against a gate `working` (not `blocked` -
// the gate hold is a separate rule tested above).
func TestAnOperatorFactAppliesAtOnce(t *testing.T) {
	cur := ev(proto.StateWorking, proto.SrcGate, 100)
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcOperator, 100.3), 100.3)
	if got.State != proto.StateIdle || got.Source != proto.SrcOperator {
		t.Fatalf("an operator fact was held back: got %s/%s", got.State, got.Source)
	}
}

func TestStoreReportsWhetherTheStateChanged(t *testing.T) {
	s := NewStore()
	if _, changed := s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100), 100); !changed {
		t.Fatal("the first event did not report a change")
	}
	if _, changed := s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100.2), 100.2); changed {
		t.Fatal("a repeat of the same state reported a change")
	}
	cur, ok := s.Current("w1:p1")
	if !ok || cur.State != proto.StateWorking {
		t.Fatalf("Current = %+v %v, want a working event", cur, ok)
	}
	recv, ok := s.Received("w1:p1")
	if !ok || recv != 100 {
		t.Fatalf("Received = %v %v, want the server clock of the event that stuck", recv, ok)
	}
}

// A repeat that does not change the state must not refresh the hold either:
// otherwise a chatty gate keeps the screen scanner away for ever.
func TestARepeatDoesNotRefreshTheHold(t *testing.T) {
	s := NewStore()
	s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100), 100)
	s.Apply("w1:p1", ev(proto.StateWorking, proto.SrcGate, 100.2), 100.2)
	if recv, _ := s.Received("w1:p1"); recv != 100 {
		t.Fatalf("Received = %v, want 100: a repeat of the same state is not news", recv)
	}
}

// The fail-closed invariant of the whole floor: the state a pane shows must be
// the state its most recent winning event carried. This is stronger than "some
// event in this trial said idle": it compares the merged state against the
// event that produced it.
func TestFuzzTheMergedStateAlwaysCameFromAnEventThatCarriedIt(t *testing.T) {
	states := []string{proto.StateIdle, proto.StateWorking, proto.StateBlocked,
		proto.StateDone, proto.StateUnknown}
	sources := []string{proto.SrcOperator, proto.SrcGate, proto.SrcHeadless,
		proto.SrcProcess, proto.SrcManifest}
	rng := rand.New(rand.NewSource(20260908))
	// Merge returns exactly one of: the incoming event, the current one, or a
	// value derived from the current one. The current one is itself, by
	// induction, one of those three, so the two inventions below are the whole
	// space of values no event carried.
	//
	// Under the amended master spec 3.1 rules both inventions are dead code
	// here, for two different reasons:
	//   - "expired" is unreachable from Merge at all, in any caller, not just
	//     this harness: Merge no longer synthesizes an expired-ask "working"
	//     event on its own (it falls through to ordinary precedence instead);
	//     that synthesis moved to EffectiveState, which this loop never calls.
	//   - "downgraded" is unreachable specifically THROUGH this harness:
	//     Merge's own downgrade-ineligible-done path still exists (defense in
	//     depth for a caller that skips Validate), but `in.Validate() != nil
	//     { continue }` below filters an ineligible done out of every trial
	//     before it ever reaches Apply, so this loop never feeds Merge the
	//     input that would exercise it.
	// Both clauses are kept exactly as specified (rather than deleted as dead
	// code) because they cost nothing and they document the two values Merge
	// is allowed to invent, so if a future change revives either path, this
	// test keeps checking it automatically.
	for trial := 0; trial < 2000; trial++ {
		s := NewStore()
		// history holds every event this pane has ever accepted at the door,
		// as it was SENT. Merge may rewrite the state on the way through.
		var history []proto.PaneStateEvent
		ts := 100.0
		for step := 0; step < 12; step++ {
			st := states[rng.Intn(len(states))]
			src := sources[rng.Intn(len(sources))]
			ts += rng.Float64() * 4
			in := ev(st, src, ts)
			if in.Validate() != nil {
				continue
			}
			history = append(history, in)
			merged, _ := s.Apply("w1:p1", in, ts)

			// The merged state is legitimate only if some accepted event
			// carried exactly that state and source, or it is one of the TWO
			// values Merge is allowed to invent.
			ok := false
			for _, h := range history {
				if h.State == merged.State && h.Source == merged.Source {
					ok = true
					break
				}
			}
			// Invention 1: an expired gate ask becomes working with no ask.
			expired := merged.State == proto.StateWorking &&
				merged.Source == proto.SrcGate && merged.Ask == nil &&
				merged.Detail == "ask deadline passed"
			// Invention 2: a done from a source that may not produce one is
			// downgraded to unknown, keeping the source. Only process and
			// headless may say done (master spec 3.1), so a gate or operator
			// done arrives as unknown from that same source, and no event ever
			// carried that pair.
			downgraded := false
			if merged.State == proto.StateUnknown && merged.Ask == nil {
				for _, h := range history {
					if h.Source == merged.Source && h.State == proto.StateDone &&
						h.Source != proto.SrcProcess && h.Source != proto.SrcHeadless {
						downgraded = true
						break
					}
				}
			}
			derived := expired || downgraded
			if !ok && !derived {
				t.Fatalf("trial %d step %d: merged %s/%s came from no accepted event: %+v",
					trial, step, merged.State, merged.Source, history)
			}
			if merged.State == proto.StateIdle {
				said := false
				for _, h := range history {
					if h.State == proto.StateIdle {
						said = true
					}
				}
				if !said {
					t.Fatalf("trial %d step %d produced idle with nobody reporting idle",
						trial, step)
				}
			}
		}
	}
}

// A blocked event with no ask is not a gate hold at all (the gate-hold branch
// only fires for cur.Source == gate, and even then only ever with an ask -
// proto.Validate requires one). A blocked/headless event with Ask == nil is
// just an ordinary current state: any non-manifest incoming source applies
// to it at once, exactly as it would against any other non-blocked state.
func TestABlockedEventWithNoAskIsAFactLikeAnyOtherAgainstAHeadlessIdle(t *testing.T) {
	cur := proto.PaneStateEvent{
		V: 1, TS: 100, SessionID: "s1", Harness: "claude-code",
		State: proto.StateBlocked, Source: proto.SrcHeadless, // no Ask
	}
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcHeadless, 100.5), 100.5)
	if got.State != proto.StateIdle || got.Source != proto.SrcHeadless {
		t.Fatalf("a headless blocked-with-no-ask held back a headless fact: got %s/%s",
			got.State, got.Source)
	}
}

// The same nil-ask blocked/headless current state still holds against a
// manifest event inside the 2 s window, via the ordinary precedence rule
// (manifest is the one source the window still holds back), not via the
// gate-hold rule.
func TestABlockedEventWithNoAskStillHoldsWithinTheWindowAgainstManifest(t *testing.T) {
	cur := proto.PaneStateEvent{
		V: 1, TS: 100, SessionID: "s1", Harness: "claude-code",
		State: proto.StateBlocked, Source: proto.SrcHeadless, // no Ask
	}
	got := Merge(&cur, 100, ev(proto.StateIdle, proto.SrcManifest, 101), 101)
	if got.State != proto.StateBlocked || got.Source != proto.SrcHeadless {
		t.Fatalf("a manifest event overrode a still-fresh nil-ask blocked state within the window: got %s/%s",
			got.State, got.Source)
	}
}

// EffectiveState's expiry rule only fires when there is an ask to expire. A
// blocked event with Ask == nil never expires, however far now runs - and
// must not panic on the nil Ask.
func TestEffectiveStateLeavesANilAskBlockedEventUnchangedEvenFarInTheFuture(t *testing.T) {
	cur := proto.PaneStateEvent{
		V: 1, TS: 100, SessionID: "s1", Harness: "claude-code",
		State: proto.StateBlocked, Source: proto.SrcHeadless, // no Ask
	}
	got := EffectiveState(&cur, 999_999_999)
	if got.State != proto.StateBlocked || got.Ask != nil {
		t.Fatalf("EffectiveState changed a nil-ask blocked event: %+v", got)
	}
}

func TestEffectiveStateOfNilIsNil(t *testing.T) {
	if got := EffectiveState(nil, 100); got != nil {
		t.Fatalf("EffectiveState(nil, …) = %+v, want nil", got)
	}
}

// Exists to give -race something to detect: one goroutine writes through
// Apply while another reads through Current and Received, both against the
// same pane, for 1000 iterations each.
func TestStoreIsSafeForConcurrentApplyAndCurrent(t *testing.T) {
	s := NewStore()
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			st := proto.StateWorking
			if i%2 == 0 {
				st = proto.StateIdle
			}
			ts := 100.0 + float64(i)
			s.Apply("w1:p1", ev(st, proto.SrcHeadless, ts), ts)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.Current("w1:p1")
			s.Received("w1:p1")
		}
	}()
	wg.Wait()
}

// Apply's returned merged value must not alias the copy the Store keeps: a
// caller mutating the Ask it was handed back must never be able to reach
// into the store.
func TestApplyReturnsAnAskThatDoesNotAliasTheStore(t *testing.T) {
	s := NewStore()
	merged, _ := s.Apply("w1:p1", ev(proto.StateBlocked, proto.SrcGate, 100), 100)
	if merged.Ask == nil {
		t.Fatal("expected the merged blocked/gate event to carry an ask")
	}
	merged.Ask.Deadline = 999_999

	cur, ok := s.Current("w1:p1")
	if !ok {
		t.Fatal("Current: pane not found")
	}
	if cur.Ask == nil {
		t.Fatal("Current: stored event lost its ask")
	}
	if cur.Ask.Deadline == 999_999 {
		t.Fatalf("mutating Apply's returned Ask changed the stored event: %+v", cur.Ask)
	}
}

// Current must hand back an event whose pointer fields are the caller's own.
// pane.list reads Current and marshals ev.Ask straight into a client
// response; two separate Current() calls for the same pane must never end up
// sharing memory that a caller of one could reach through the other, or a
// concurrent Apply for that pane.
func TestCurrentDoesNotAliasTheStoreOrAnEarlierRead(t *testing.T) {
	s := NewStore()
	s.Apply("w1:p1", ev(proto.StateBlocked, proto.SrcGate, 100), 100)

	first, ok := s.Current("w1:p1")
	if !ok || first.Ask == nil {
		t.Fatal("Current: expected a blocked/gate event with an ask")
	}
	first.Ask.Deadline = 999_999

	second, ok := s.Current("w1:p1")
	if !ok {
		t.Fatal("Current: pane not found on the second read")
	}
	if second.Ask == nil {
		t.Fatal("Current: stored event lost its ask")
	}
	if second.Ask.Deadline == 999_999 {
		t.Fatalf("mutating one Current() result changed a later Current() result: %+v", second.Ask)
	}
}
