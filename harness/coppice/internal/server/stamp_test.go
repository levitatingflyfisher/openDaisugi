package server

import (
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

func stampEvent(id, state, source string) proto.PaneStateEvent {
	return proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude-code", Pane: &id,
		State: state, Source: source, Detail: "session stop",
	}
}

// The Stop hook reports its idle as source headless (hook.py), so that is
// the event the stamp must follow (CP-R-57). A process or manifest fact
// never sets it, and a non-idle hook fact clears it.
func TestTheStampFollowsTheHookIdleAsTheHookSendsIt(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcProcess))
	if lp.gateIdleSince.Load() != 0 {
		t.Fatal("a process idle set the stamp")
	}
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	first := lp.gateIdleSince.Load()
	if first == 0 {
		t.Fatal("the Stop hook idle (source headless) did not set the stamp")
	}
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcGate))
	if lp.gateIdleSince.Load() != first {
		t.Fatal("a second idle of the same run moved the stamp")
	}
	s.ApplyState(id, stampEvent(id, proto.StateWorking, proto.SrcGate))
	if lp.gateIdleSince.Load() != 0 {
		t.Fatal("a gate working did not clear the stamp")
	}
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	s.ApplyState(id, stampEvent(id, proto.StateWorking, proto.SrcHeadless))
	if lp.gateIdleSince.Load() != 0 {
		t.Fatal("a headless working did not clear the stamp")
	}
}

// An event the merge held back changes nothing (CP-R-57): the pane stays
// blocked behind a gate hold, so a headless idle there is not a stamp.
func TestAHeldBackHookIdleSetsNoStamp(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	ask := stampEvent(id, proto.StateBlocked, proto.SrcGate)
	ask.Ask = &proto.Ask{ID: "a", Tool: "Bash", Summary: "x", Deadline: nowSeconds() + 600}
	s.ApplyState(id, ask)
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	if lp.gateIdleSince.Load() != 0 {
		t.Fatal("an idle the gate hold discarded set the stamp")
	}
}

// The 3 s boundary runs on the server clock, so a test can move it.
func TestAStampStandsOnlyAfterThreeSeconds(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	working := stampEvent(id, proto.StateWorking, proto.SrcProcess)
	s.clockSkew.Store(int64(2900 * time.Millisecond))
	if s.gateIdleStands(lp, working) {
		t.Fatal("a stamp 2.9 s old stands")
	}
	s.clockSkew.Store(int64(3100 * time.Millisecond))
	if !s.gateIdleStands(lp, working) {
		t.Fatal("a stamp 3.1 s old does not stand")
	}
	if s.gateIdleStands(lp, stampEvent(id, proto.StateWorking, proto.SrcGate)) {
		t.Fatal("a gate working stands as idle")
	}
}

// Input that submits a line starts a turn the hooks may not announce, so a
// stale stamp must not let the queue take the pane as idle (CP-R-57).
func TestASubmittedLineClearsAStaleStamp(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	s.clockSkew.Store(int64(10 * time.Second))
	working := stampEvent(id, proto.StateWorking, proto.SrcProcess)
	if !s.gateIdleStands(lp, working) {
		t.Fatal("a 10 s stamp does not stand before any input")
	}
	s.markInput(id, false)
	if lp.gateIdleSince.Load() == 0 {
		t.Fatal("a half line cleared the stamp")
	}
	s.markInput(id, true)
	if s.gateIdleStands(lp, working) {
		t.Fatal("a stale stamp stands after a submitted line")
	}
}

// The foreman's own page is such input: sendPage must clear the stamp.
func TestTheForemansPageClearsAStaleStamp(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "cat >/dev/null")
	lp, _ := s.Live(id)
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	s.clockSkew.Store(int64(10 * time.Second))
	if err := s.sendPage(id); err != nil {
		t.Fatal(err)
	}
	if s.gateIdleStands(lp, stampEvent(id, proto.StateWorking, proto.SrcProcess)) {
		t.Fatal("a stale stamp stands after the foreman's page")
	}
}

// The hold line the server types into the foreman submits a turn too, so it
// clears a stale stamp (CP-R-57).
func TestTheHoldLineClearsAStaleStamp(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "cat >/dev/null")
	lp, _ := s.Live(id)
	s.ApplyState(id, stampEvent(id, proto.StateIdle, proto.SrcHeadless))
	s.clockSkew.Store(int64(10 * time.Second))
	working := stampEvent(id, proto.StateWorking, proto.SrcProcess)
	if !s.gateIdleStands(lp, working) {
		t.Fatal("a 10 s stamp does not stand before the hold line")
	}
	h := &hold{pane: "p", ask: "a", foreman: id, task: "t", id: "h1", sending: true,
		until: nowSeconds() + 60, timer: time.NewTimer(time.Hour)}
	s.holdsMu.Lock()
	s.holds["p"] = h
	s.holdsMu.Unlock()
	s.tellOne(id, h)
	s.holdsMu.Lock()
	told := h.told
	s.holdsMu.Unlock()
	if !told {
		t.Fatal("the hold line was not written")
	}
	if s.gateIdleStands(lp, working) {
		t.Fatal("a stale stamp stands after the hold line")
	}
}

// A hook idle older than a fresh pane's first draw says nothing about
// whether the harness is ready, so it does not settle the pane (CP-R-57).
func TestAStampOlderThanTheFirstDrawDoesNotSettleAFreshPane(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "while :; do printf .; sleep 0.2; done")
	lp, _ := s.Live(id)
	var first time.Time
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if f, drawn := lp.Grid.FirstWrite(); drawn {
			first = f
			break
		}
		if time.Now().After(end) {
			t.Fatal("the pane never drew")
		}
	}
	s.processTick(id, lp)
	lp.gateIdleSince.Store(first.UnixNano() - int64(10*time.Second))
	idle := func(ev proto.PaneStateEvent) bool { return ev.State == proto.StateIdle }
	settles := func(fresh bool) bool {
		done := make(chan struct{})
		go func() {
			_ = s.settle(id, idle, fresh, "")
			close(done)
		}()
		select {
		case <-done:
			return true
		case <-time.After(1500 * time.Millisecond):
			return false
		}
	}
	if !settles(false) {
		t.Fatal("a 10 s stamp did not settle a pane that is not fresh")
	}
	if settles(true) {
		t.Fatal("a stamp older than the first draw settled a fresh pane")
	}
}

// A hook idle older than a fresh pane's first draw says nothing, and the
// hold of CP-R-56 keeps a short draw from ever reaching the process source,
// so no process idle replaces it. The pane that drew and then kept quiet for
// processQuiet is ready all the same (the tui/chat compare case: a fake
// foreman reports a gate idle, then prints a line and waits).
func TestAStaleHookIdleDoesNotKeepAQuietFreshPaneFromSettling(t *testing.T) {
	s := newPaneServer(t)
	id := createShellPane(t, s, "sh", "-c", "exec sleep 30")
	lp, _ := s.Live(id)
	old := stampEvent(id, proto.StateIdle, proto.SrcGate)
	old.TS = 1
	s.ApplyState(id, old)
	if _, err := lp.Grid.Write([]byte("REPORT ok")); err != nil {
		t.Fatal(err)
	}
	s.processTick(id, lp)
	idle := func(ev proto.PaneStateEvent) bool { return ev.State == proto.StateIdle }
	settles := func() bool {
		done := make(chan struct{})
		go func() {
			_ = s.settle(id, idle, true, "")
			close(done)
		}()
		select {
		case <-done:
			return true
		case <-time.After(1500 * time.Millisecond):
			return false
		}
	}
	if settles() {
		t.Fatal("a pane that drew a moment ago settled on a stale idle")
	}
	s.clockSkew.Store(int64(6 * time.Second))
	if !settles() {
		t.Fatal("a pane quiet for 6 s did not settle on a stale hook idle")
	}
}
