package server

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func newAgentServer(t *testing.T) *Server {
	t.Helper()
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	return s
}

const paneCreate = `{"id":"1","cmd":"pane.create","cwd":"%s","cmd_argv":["sh","-c","sleep 5"],` +
	`"kind":"pty","label":"auth fix","cols":40,"rows":10}`

func gateBlockedLine(pane string) string {
	return `{"id":"9","cmd":"pane.report_state","pane":"` + pane + `","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"` + pane +
		`","state":"blocked","source":"gate","ask":{"id":"toolu_1","tool":"Bash",` +
		`"summary":"rm -rf build/","deadline":9999999999.0},"detail":"verdict=deny"}}`
}

func TestReportStateFromTheGateSetsTheMergedState(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		gateBlockedLine("w1:p1"),
		`{"id":"3","cmd":"pane.list"}`)
	if !got[1].OK {
		t.Fatalf("pane.report_state failed: %+v", got[1].Error)
	}
	m := result(t, got[2])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	if p["state"] != "blocked" || p["source"] != "gate" {
		t.Fatalf("after a gate report: state=%v source=%v, want blocked / gate", p["state"], p["source"])
	}
	ask, _ := p["ask"].(map[string]any)
	if ask == nil || ask["tool"] != "Bash" {
		t.Fatalf("the ask did not survive: %v", p["ask"])
	}
}

// Only a client attached to the pane may speak as the operator. A detached
// client claiming operator is downgraded to gate, never trusted at face value.
func TestOperatorSourceIsDowngradedForADetachedClient(t *testing.T) {
	s := newAgentServer(t)
	line := `{"id":"2","cmd":"pane.report_state","pane":"w1:p1","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",` +
		`"state":"working","source":"operator","detail":"I said so"}}`
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		line,
		`{"id":"3","cmd":"pane.list"}`)
	if !got[1].OK {
		t.Fatalf("pane.report_state failed: %+v", got[1].Error)
	}
	m := result(t, got[2])
	panes, _ := m["panes"].([]any)
	p, _ := panes[0].(map[string]any)
	if p["source"] != "gate" {
		t.Fatalf("a detached client kept source=%v, want it downgraded to gate", p["source"])
	}
	b, _ := json.Marshal(got[1].Result)
	if !strings.Contains(string(b), "gate") {
		t.Fatalf("the response %s does not tell the caller its source was changed", b)
	}
}

func TestOperatorSourceIsKeptForAnAttachedClient(t *testing.T) {
	s := newAgentServer(t)
	send, next, stop := stream(t, s)
	defer stop()
	send(strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	next()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":40,"rows":10}`)
	next()
	send(`{"id":"3","cmd":"pane.report_state","pane":"w1:p1","event":` +
		`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",` +
		`"state":"working","source":"operator","detail":"I said so"}}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("pane.report_state failed: %v", m)
			}
			res, _ := m["result"].(map[string]any)
			if res["source"] != "operator" {
				t.Fatalf("an attached client was downgraded to %v, want operator", res["source"])
			}
			return
		}
	}
	t.Fatal("no response to the report within 5 s")
}

func TestReportStateWithAMalformedEventIsBadRequest(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"state":"vibing","source":"gate"}}`)
	if got[1].OK || got[1].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[1])
	}
}

func TestAgentWaitResolvesImmediatelyOnAGateBlocked(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		gateBlockedLine("w1:p1"),
		`{"id":"3","cmd":"agent.wait","pane":"w1:p1","until":"blocked","timeout_ms":2000}`)
	if !got[2].OK {
		t.Fatalf("agent.wait did not resolve on the gate report: %+v", got[2].Error)
	}
	m := result(t, got[2])
	if m["state"] != "blocked" {
		t.Fatalf("agent.wait returned state=%v, want blocked", m["state"])
	}
}

func TestAgentWaitTimesOutWithTheTimeoutCode(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"agent.wait","pane":"w1:p1","until":"working","timeout_ms":300}`)
	if got[1].OK || got[1].Error.Code != proto.ErrTimeout {
		t.Fatalf("got %+v, want timeout", got[1])
	}
	// A sleeping shell writes nothing, so the process source says idle.
	if !strings.Contains(got[1].Error.Message, "idle") {
		t.Fatalf("the timeout message %q does not say what the pane's state actually is",
			got[1].Error.Message)
	}
}

func TestAgentListShowsEveryPaneWithItsMergedState(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		gateBlockedLine("w1:p1"),
		`{"id":"3","cmd":"agent.list"}`)
	m := result(t, got[2])
	agents, _ := m["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agent.list returned %d agents, want 1", len(agents))
	}
	a, _ := agents[0].(map[string]any)
	if a["state"] != "blocked" || a["label"] != "auth fix" {
		t.Fatalf("agent entry = %v, want the blocked auth fix pane", a)
	}
}

func TestAgentPromptWithWaitReturnsTheResolvedState(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	// A shell pane is not an agent, but agent.prompt is defined as "send text
	// and optionally wait", so the shell exercises the whole path.
	//
	// This differs from a literal `sh -c "exit 0"` shape: that command can
	// exit before agent.prompt's own livePane() check ever runs - watchExit
	// then wins the race, the tree already shows the pane closed, and
	// agent.prompt fails with pane_closed before it ever reaches the wait.
	// Confirmed by running the package suite under load (go test -race
	// -count=1, repeated): flaked roughly one run in three, always with that
	// exact pane_closed message, never a different failure. The sleep below
	// keeps the process alive well past the moment agent.prompt writes to
	// it, then lets it exit into the done event the wait is for; 0.3s is a
	// deliberately generous margin over the box's observed timing, not the
	// minimum that happened to pass here.
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 0.3; exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"agent.prompt","pane":"w1:p1","text":"hello","wait":true,"until":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("agent.prompt --wait failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	if m["state"] != "done" {
		t.Fatalf("agent.prompt returned state=%v, want done", m["state"])
	}
}

// This guards a case where agent.prompt --wait and agent.wait always included
// "ask": ev.Ask in their success reply, so a pane with no open ask printed
// the literal Go string "<nil>" through printHuman. Both now omit ask when
// there is none, the same treatment agentRow already gives it.
func TestAgentPromptAndAgentWaitOmitAskWhenThereIsNone(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 0.3; exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"agent.prompt","pane":"w1:p1","text":"hello","wait":true,"until":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("agent.prompt --wait failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	if _, ok := m["ask"]; ok {
		t.Fatalf("agent.prompt --wait reply carries ask=%v with no open ask, want it omitted", m["ask"])
	}

	got2 := roundTrip(t, s, `{"id":"3","cmd":"agent.wait","pane":"w1:p1","until":"done","timeout_ms":5000}`)
	if !got2[0].OK {
		t.Fatalf("agent.wait failed: %+v", got2[0].Error)
	}
	m2 := result(t, got2[0])
	if _, ok := m2["ask"]; ok {
		t.Fatalf("agent.wait reply carries ask=%v with no open ask, want it omitted", m2["ask"])
	}
}

// An adapter is our own code, but it is still the wrong place to decide what a
// state string may be. The enum is closed, and a client switches on it.
func TestAnAdapterCannotInventAState(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	id := "w1:p1"
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude-code", Pane: &id,
		State: "busy", Source: proto.SrcHeadless,
	})
	ev, ok := s.States().Current(id)
	if !ok {
		t.Fatal("the invalid event produced no state at all")
	}
	if ev.State != proto.StateUnknown {
		t.Fatalf("state = %q, want unknown: an invented state must never reach a client", ev.State)
	}
	if ev.State == proto.StateIdle {
		t.Fatal("an invalid event produced idle")
	}
	if !strings.Contains(ev.Detail, "rejected") {
		t.Fatalf("detail = %q, want it to say the event was rejected and why", ev.Detail)
	}
}

func TestAgentWaitResolvesOnAStateThatArrivesRightAfterTheCall(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	// The child exits after a short delay, so the done event lands while
	// agent.wait is already blocked. Registering the waiter after reading the
	// current state would lose it.
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 0.2"],"kind":"pty"}`,
		`{"id":"2","cmd":"agent.wait","pane":"w1:p1","until":"done","timeout_ms":5000}`)
	if !got[1].OK {
		t.Fatalf("agent.wait missed a state that arrived while it was waiting: %+v", got[1].Error)
	}
}

func TestAgentGetOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"agent.get","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

// This guards a case where agentRow read harness_session_id off the state event
// only, never off the tree record Restore itself read it from. A resumed
// pane's record carries a recorded session id from the moment it comes
// back, but its own state event - markRestored's, or its adapter's first
// one - does not always carry one, so agentRow used to show nothing at all
// until the adapter happened to speak.
func TestAgentGetFallsBackToTheRecordedHarnessSessionID(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	roundTrip(t, s, strings.Replace(paneCreate, "%s", cwd, 1))
	id := "w1:p1"
	if err := s.Tree().UpdatePane(id, func(p *layout.Pane) {
		p.HarnessSessionID = "sess-recorded"
	}); err != nil {
		t.Fatal(err)
	}
	// A state event that carries no HarnessSessionID of its own, the same
	// shape markRestored posts.
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude", Pane: &id,
		State: proto.StateUnknown, Source: proto.SrcProcess, Detail: "resume failed",
	})

	got := roundTrip(t, s, `{"id":"2","cmd":"agent.get","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("agent.get failed: %+v", got[0].Error)
	}
	m := result(t, got[0])
	if m["harness_session_id"] != "sess-recorded" {
		t.Fatalf("harness_session_id = %v, want the tree record's sess-recorded", m["harness_session_id"])
	}
}

// agent.get/list
// rows carry ts and session_id copied from the stored merged event - never
// a receive-time stamp, which is a different clock EffectiveState never
// touches. A pane with no stored event yet omits both keys.
func TestAgentGetCarriesTsAndSessionIDFromTheStoredEvent(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	roundTrip(t, s, strings.Replace(paneCreate, "%s", cwd, 1))
	id := "w1:p1"

	got := roundTrip(t, s, `{"id":"2","cmd":"agent.get","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("agent.get failed: %+v", got[0].Error)
	}
	m := result(t, got[0])
	// A live pty pane's first stored event is the process source's own
	// idle fact, so ts is that event's stamp and session_id is the pane id.
	if m["source"] != "process" {
		t.Fatalf("agent.get source = %v, want the process fact a live pane starts with", m["source"])
	}
	if _, ok := m["ts"].(float64); !ok {
		t.Fatalf("agent.get carries no numeric ts for the process fact: %v", m)
	}
	if m["session_id"] != id {
		t.Fatalf("session_id = %v, want the pane id the process fact carries", m["session_id"])
	}

	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: 1234.5, SessionID: "the-session-id", Harness: "claude", Pane: &id,
		State: proto.StateWorking, Source: proto.SrcHeadless,
	})
	got2 := roundTrip(t, s, `{"id":"3","cmd":"agent.get","pane":"`+id+`"}`)
	if !got2[0].OK {
		t.Fatalf("agent.get failed: %+v", got2[0].Error)
	}
	m2 := result(t, got2[0])
	if m2["ts"] != 1234.5 {
		t.Fatalf("ts = %v, want the stored event's own 1234.5", m2["ts"])
	}
	if m2["session_id"] != "the-session-id" {
		t.Fatalf("session_id = %v, want the stored event's own the-session-id", m2["session_id"])
	}
}

// TestNewRegistersTheAgentVerbs is TestNewRegistersThePaneVerbs's sibling for
// this file: New wires RegisterAgentCommands into itself, the same way it
// wires RegisterPaneCommands and RegisterAttachCommands, so a caller cannot
// forget the call and ship a daemon with pane verbs but no agent verbs. This
// uses newTestServer directly, not newAgentServer, precisely so it does NOT
// call RegisterAgentCommands itself - every other test in this file does,
// which means none of them would notice if that wiring in New were ever
// reverted.
func TestNewRegistersTheAgentVerbs(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"agent.list"}`)
	if !got[0].OK {
		t.Fatalf("agent.list is not registered by New: %+v", got[0].Error)
	}
}

// state.EffectiveState must be applied at agent.get's
// current-state read, exactly as pane.list already does (see
// TestPaneListShowsAnExpiredAskAsWorking in panes_test.go) - a blocked hold
// whose ask deadline has passed must read as working, not blocked forever.
func TestAgentGetShowsAnExpiredAskAsWorking(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	paneID := "w1:p1"
	past := nowSeconds() - 10
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: "claude-code", Pane: &paneID,
		State: proto.StateBlocked, Source: proto.SrcHeadless,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf x", Deadline: past},
	})

	got2 := roundTrip(t, s, `{"id":"2","cmd":"agent.get","pane":"w1:p1"}`)
	m := result(t, got2[0])
	if m["state"] != "working" {
		t.Fatalf("agent.get state = %v, want working (the ask's deadline has passed)", m["state"])
	}
	if _, hasAsk := m["ask"]; hasAsk {
		t.Fatalf("agent.get still carries an ask %v, want it cleared once expired", m["ask"])
	}
}

// agent.wait's half of the same guarantee: WaitState's up-front current-state
// check must also go through EffectiveState, or a wait for "working" on a
// pane whose blocked ask already expired would sleep to timeout instead of
// resolving at once - the same bug TestWaitOutputForWorkingReturnsAtOnceWhenAnAskHasExpired
// guards for pane.wait_output.
func TestAgentWaitForWorkingReturnsAtOnceWhenAnAskHasExpired(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	paneID := "w1:p1"
	past := nowSeconds() - 10
	s.ApplyState(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: paneID, Harness: "claude-code", Pane: &paneID,
		State: proto.StateBlocked, Source: proto.SrcHeadless,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf x", Deadline: past},
	})

	start := time.Now()
	got2 := roundTrip(t, s,
		`{"id":"2","cmd":"agent.wait","pane":"w1:p1","until":"working","timeout_ms":5000}`)
	elapsed := time.Since(start)
	if !got2[0].OK {
		t.Fatalf("agent.wait for working failed: %+v", got2[0].Error)
	}
	if elapsed > time.Second {
		t.Fatalf("agent.wait for working took %v, want it to return at once", elapsed)
	}
}

// agent.wait must select on the client's c.Dead() and return
// promptly when the client goes away, mirroring
// TestWaitOutputStopsPollingWhenTheClientDisconnects in panes_test.go. Without
// this, a connection that drops mid-wait pins the handler goroutine - and the
// bounded teardown wait behind it - for up to timeout_ms.
func TestAgentWaitReturnsPromptlyWhenTheClientDisconnects(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { s.ServeConn(inR, outW); _ = outW.Close(); close(done) }()

	d := proto.NewDecoder(outR)
	go func() {
		for {
			if _, err := d.Next(); err != nil {
				return
			}
		}
	}()

	if _, err := io.WriteString(inW,
		`{"id":"2","cmd":"agent.wait","pane":"w1:p1","until":"done","timeout_ms":10000}`+"\n"); err != nil {
		t.Fatal(err)
	}

	// Give the handler a moment to actually register its waiter before
	// disconnecting.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = inW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeConn never returned; agent.wait kept blocking past the disconnect")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("agent.wait took %v to notice the disconnect, want well under its 10s timeout_ms", elapsed)
	}
}

// refreshLiveInfo must hold liveMu across the tree read, not
// just around the map write, or the last of two racing refreshLiveInfo calls
// can publish a snapshot it captured before the other one's tree write - here,
// a Cols update that read the tree while the pane was still open, losing a
// concurrent close.
//
// This is forced deterministically, not left to chance scheduling: the test
// holds liveMu itself, lets updatePane's goroutine park on it (inside
// refreshLiveInfo), commits the close directly against the tree - bypassing
// closePane/refreshLiveInfo entirely, so the parked goroutine's own read is
// the only one that can observe it - then releases liveMu and checks what
// landed.
func TestRefreshLiveInfoNeverPublishesASnapshotFromBeforeAConcurrentClose(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id := "w1:p1"

	s.liveMu.Lock()
	done := make(chan struct{})
	go func() {
		_ = s.updatePane(id, func(p *layout.Pane) { p.Cols = 100 })
		close(done)
	}()
	// Let updatePane's own tree write finish and its refreshLiveInfo park on
	// liveMu, which this goroutine still holds.
	time.Sleep(100 * time.Millisecond)

	exit := 7
	if err := s.tree.ClosePane(id, &exit, 1000); err != nil {
		t.Fatal(err)
	}
	s.liveMu.Unlock()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("updatePane's refreshLiveInfo never finished")
	}

	info, ok := s.paneInfo(id)
	if !ok {
		t.Fatal("paneInfo: pane not found")
	}
	if !info.Closed {
		t.Fatal("LivePane.Info.Closed = false, but the tree already says the pane is closed: " +
			"refreshLiveInfo published a snapshot read before the close, taken outside liveMu")
	}
}

// notifyWaiters and WaitState's own drop() both rebuild s.waiters with the
// same keep := s.waiters[:0] in-place compaction, serialized by s.mu. Two
// waiters on the same pane at once - one satisfied by a state that arrives
// mid-wait, one left to time out - exercises both paths together: one waiter
// removed by notifyWaiters mid-loop (it never reaches keep), the other
// removed later by its own drop() once its timer fires. Neither compaction
// may drop or corrupt the other's registration.
func TestTwoConcurrentWaitersOnTheSamePaneEachGetTheirOwnOutcome(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	id := "w1:p1"

	type outcome struct {
		ev   proto.PaneStateEvent
		done bool
	}
	blockedCh := make(chan outcome, 1)
	doneCh := make(chan outcome, 1)

	go func() {
		ev, done := s.WaitState(id, proto.StateBlocked, 3*time.Second, nil)
		blockedCh <- outcome{ev, done}
	}()
	go func() {
		ev, done := s.WaitState(id, proto.StateDone, 200*time.Millisecond, nil)
		doneCh <- outcome{ev, done}
	}()

	// Give both waiters time to register before the state arrives.
	time.Sleep(50 * time.Millisecond)
	deadline := nowSeconds() + 9999
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude-code", Pane: &id,
		State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf x", Deadline: deadline},
	})

	select {
	case o := <-blockedCh:
		if !o.done || o.ev.State != proto.StateBlocked {
			t.Fatalf("the blocked waiter got %+v, done=%v, want blocked/true", o.ev, o.done)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the blocked waiter never resolved")
	}
	select {
	case o := <-doneCh:
		if o.done {
			t.Fatalf("the done waiter resolved as %+v, want it to time out - the pane never went done", o.ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the done waiter never returned")
	}
}

// This test guards against agents.go reading lp.Info directly, with no
// liveMu, while refreshLiveInfo writes it under liveMu from another
// goroutine - a
// pane.resize on one connection, say, while agent.prompt reads it on another.
// t.Fatal is not called from these goroutines (the testing package requires
// FailNow-family calls to come from the test's own goroutine); each one
// drives its own short-lived connection and reports outcome over a channel.
func TestConcurrentResizeAndAgentPromptDoNotRaceOnLiveInfo(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	run := func(lines []string) error {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
		defer func() { _ = inW.Close() }()
		d := proto.NewDecoder(outR)
		for _, l := range lines {
			if _, err := io.WriteString(inW, l+"\n"); err != nil {
				return err
			}
			line, err := d.Next()
			if err != nil {
				return err
			}
			var r proto.Response
			if err := json.Unmarshal(line, &r); err != nil {
				return err
			}
			if !r.OK {
				return fmt.Errorf("request failed: %+v", r.Error)
			}
		}
		return nil
	}

	errs := make(chan error, 2)
	go func() {
		var lines []string
		for i := 0; i < 50; i++ {
			lines = append(lines, fmt.Sprintf(
				`{"id":"r%d","cmd":"pane.resize","pane":"w1:p1","cols":%d,"rows":10}`, i, 20+(i%10)))
		}
		errs <- run(lines)
	}()
	go func() {
		var lines []string
		for i := 0; i < 50; i++ {
			lines = append(lines, fmt.Sprintf(
				`{"id":"p%d","cmd":"agent.prompt","pane":"w1:p1","text":"x"}`, i))
		}
		errs <- run(lines)
	}()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// agent.read must mirror
// handleRead's grid-lifetime handling, not livePane's stricter rec.Closed
// refusal - a pane whose process exited keeps its grid, and an agent's own
// final screen is exactly the thing worth reading once it's gone. Mirrors
// TestReadWorksOnAPaneWhoseProcessExited in panes_test.go.
func TestAgentReadShowsTheFinalScreenOfAnExitedAgent(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","printf final; exit 0"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","state":"done","timeout_ms":5000}`,
		`{"id":"3","cmd":"agent.read","pane":"w1:p1","region":"visible"}`)
	if !got[1].OK {
		t.Fatalf("waiting for done failed: %+v", got[1].Error)
	}
	if !got[2].OK {
		t.Fatalf("agent.read on a process-exited pane failed: %+v, want it to still work", got[2].Error)
	}
	m := result(t, got[2])
	text, _ := m["text"].(string)
	if !strings.Contains(text, "final") {
		t.Fatalf("agent.read text = %q, want it to contain final", text)
	}
	if m["region"] != "visible" {
		t.Fatalf("agent.read region = %v, want visible (the grid region just read)", m["region"])
	}
	// Within the agent verbs, source means the pane's STATE
	// source everywhere - here, the done event watchExit applied when the
	// process exited - never the grid region, which now has its own key
	// (region, checked above).
	if m["source"] != "process" {
		t.Fatalf("agent.read source = %v, want process (the pane's state source, not the grid region)",
			m["source"])
	}
}

// agent.read's request still accepts source (matching
// pane.read's own request shape, for a caller migrating between the two
// verbs), alongside the preferred region; region wins when both are given.
// Only the RESPONSE key changed - pane.read itself is untouched.
func TestAgentReadRequestAcceptsSourceForCompatibilityButRegionWins(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"agent.read","pane":"w1:p1","source":"detection"}`,
		`{"id":"3","cmd":"agent.read","pane":"w1:p1","region":"visible","source":"detection"}`)
	if !got[1].OK {
		t.Fatalf("agent.read with source failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	if m["region"] != "detection" {
		t.Fatalf("agent.read region = %v, want detection (from the compat source field)", m["region"])
	}
	if !got[2].OK {
		t.Fatalf("agent.read with both fields failed: %+v", got[2].Error)
	}
	m2 := result(t, got[2])
	if m2["region"] != "visible" {
		t.Fatalf("agent.read region = %v, want visible (region takes precedence over source)", m2["region"])
	}
}

// Mirrors TestReadAfterCloseIsPaneClosedAtOnce in panes_test.go: only an
// operator pane.close frees the grid and removes the live entry, and that is
// when agent.read must finally refuse.
// An operator pane.close removes the record at once, so a later
// agent.read against it is no_such_pane, not pane_closed - the same
// change TestReadAfterCloseIsNoSuchPaneAtOnce covers for pane.read.
func TestAgentReadAfterCloseIsNoSuchPane(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.close","pane":"w1:p1"}`,
		`{"id":"3","cmd":"agent.read","pane":"w1:p1"}`)
	if !got[1].OK {
		t.Fatalf("pane.close failed: %+v", got[1].Error)
	}
	if got[2].OK || got[2].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("agent.read after close = %+v, want no_such_pane", got[2])
	}
}

// ApplyState's return value, not the request's own event,
// is what a handler must reply with. An attached client (a real operator, no
// downgrade in play) reports "working" while the pane sits under a gate
// blocked hold with an ask far from expiring - master spec 3.1's rule 4 says
// that hold outranks every other source, including operator, so Merge keeps
// cur unchanged. The reply must show blocked/gate, the fact that actually
// landed, not the operator's working claim echoed back as if it had.
func TestReportStateReplyReflectsWhatWasActuallyStoredNotWhatWasSent(t *testing.T) {
	s := newAgentServer(t)
	send, next, stop := stream(t, s)
	defer stop()
	send(strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	next()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":40,"rows":10}`)
	next()

	// pane.attach also subscribes this client to its own pane's state
	// events, so every report_state from here on broadcasts a "state" event
	// line back to it, interleaved unpredictably with the direct response to
	// the request that caused it - the response goes straight out on
	// c.Enc.Send from its own request goroutine, the broadcast queues
	// through c.Emit/pump on a different one, and nothing orders the two
	// against each other. nextResponse skips anything that is not the
	// response with the matching id, mirroring
	// TestOperatorSourceIsKeptForAnAttachedClient's own pattern for exactly
	// this reason.
	nextResponse := func(id string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if m := next(); m["id"] == id {
				return m
			}
		}
		t.Fatalf("no response to id %q within 5 s", id)
		return nil
	}

	send(gateBlockedLine("w1:p1"))
	if m := nextResponse("9"); m["ok"] != true {
		t.Fatalf("the gate's own report failed: %v", m)
	}
	send(`{"id":"4","cmd":"pane.report_state","pane":"w1:p1","event":` +
		`{"v":1,"ts":1757300001.0,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",` +
		`"state":"working","source":"operator","detail":"I said so"}}`)
	m := nextResponse("4")
	if m["ok"] != true {
		t.Fatalf("the operator's report failed outright: %v", m)
	}
	res, _ := m["result"].(map[string]any)
	if res["state"] != "blocked" || res["source"] != "gate" {
		t.Fatalf("reply = %v, want it to show the gate hold that actually won, not the operator's "+
			"echoed claim (working/operator)", res)
	}
}

// The direct contract: ApplyState's return value IS what
// got stored, for every caller, not only handleReportState.
func TestApplyStateReturnsTheMergedStoredEvent(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	id := "w1:p1"
	got := s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude-code", Pane: &id,
		State: "busy", Source: proto.SrcHeadless,
	})
	stored, ok := s.States().Current(id)
	if !ok {
		t.Fatal("ApplyState stored nothing")
	}
	if got.State != stored.State || got.Source != stored.Source || got.Detail != stored.Detail {
		t.Fatalf("ApplyState returned %+v, want it to equal what was actually stored: %+v", got, stored)
	}
}

// A blocked claim with no ask is valid as operator (a
// human needs no ask to say "I'm blocked"), but the same claim becomes
// invalid the moment a detached client's claim is downgraded to gate (a
// gate's blocked claim always carries the ask it is holding). The report
// must be refused outright, by name, and never even reach ApplyState - not
// silently applied and stored as a rejected "unknown".
func TestReportStateOperatorBlockedWithoutAskIsBadRequestAfterDowngrade(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"pane.report_state","pane":"w1:p1","event":`+
			`{"v":1,"ts":1757300000.0,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",`+
			`"state":"blocked","source":"operator","detail":"stuck"}}`)
	if got[1].OK || got[1].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request: a detached client's blocked claim with no ask "+
			"becomes invalid once downgraded to gate", got[1])
	}
	if !strings.Contains(got[1].Error.Message, "ask") {
		t.Fatalf("error message %q does not name the rule (a gate blocked report needs an ask)",
			got[1].Error.Message)
	}
	// The only stored event is still the process fact the pane started
	// with. A refused report reaches neither the store nor a substitute.
	cur, ok := s.States().Current("w1:p1")
	if !ok || cur.Source != proto.SrcProcess || cur.State != proto.StateIdle {
		t.Fatalf("a refused report must never be applied, not even as a rejected/unknown substitute: %+v", cur)
	}
}

// ApplyState's rejection substitute keeps the incoming
// event's own Source rather than promoting it to process. The pane starts
// with no state at all (Merge's cur == nil rule returns the incoming event
// whole, regardless of its source's precedence rank), which isolates this
// from source-rank behavior entirely: the only variable under test is
// whether Source survives the substitution.
func TestApplyStateRejectionSubstituteKeepsTheIncomingSourceNotProcess(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	id := "w1:p1"
	got := s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude-code", Pane: &id,
		State: "busy", Source: proto.SrcHeadless,
	})
	if got.State != proto.StateUnknown {
		t.Fatalf("state = %q, want unknown", got.State)
	}
	if got.Source != proto.SrcHeadless {
		t.Fatalf("source = %q, want headless (the incoming event's own source), not promoted to process",
			got.Source)
	}
}

// agent.prompt without wait answers {pane, sent} only - no
// state/source/ask, since nothing was waited for.
func TestAgentPromptWithoutWaitReturnsPaneAndSent(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		`{"id":"2","cmd":"agent.prompt","pane":"w1:p1","text":"hello"}`)
	if !got[1].OK {
		t.Fatalf("agent.prompt failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	if m["pane"] != "w1:p1" {
		t.Fatalf("pane = %v, want w1:p1", m["pane"])
	}
	if sent, ok := m["sent"].(float64); !ok || int(sent) != len("hello") {
		t.Fatalf("sent = %v, want %d", m["sent"], len("hello"))
	}
	if _, hasState := m["state"]; hasState {
		t.Fatalf("agent.prompt without wait returned state %v, want only {pane, sent}", m["state"])
	}
}

// pane.report_state must answer within its 200 ms wire budget
// even while an unrelated subscriber
// on this pane is wedged - EventQueue's own doc comment in server.go names
// this exact scenario ("a PWA on a stalled tailnet must not be able to hold
// up ApplyState for every pane"). A second connection attaches (subscribing
// to this pane's state events) and then reads nothing else: its pump
// goroutine blocks forever on the next event it is handed, and every
// Broadcast after that queues into its bounded event channel (EventQueue =
// 256) until that fills too. io.PipeWriter has no SetWriteDeadline, so
// ServeConn teardown's forced-deadline trick cannot reach a pump stuck like
// this - only closing the read end can, which is what cleanup does below.
func TestReportStateAnswersFastEvenWithAWedgedSubscriber(t *testing.T) {
	s := newAgentServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	before := runtime.NumGoroutine()

	wIn, wInW := io.Pipe()
	wOut, wOutW := io.Pipe()
	wDone := make(chan struct{})
	go func() { s.ServeConn(wIn, wOutW); close(wDone) }()
	wDec := proto.NewDecoder(wOut)
	if _, err := io.WriteString(wInW,
		`{"id":"1","cmd":"pane.attach","pane":"w1:p1","cols":40,"rows":10}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := wDec.Next(); err != nil {
		t.Fatalf("pane.attach on the wedged connection: %v", err)
	}
	// wDec.Next() is never called again: nothing drains wOut from here on,
	// so the wedged connection's pump goroutine blocks on its very next
	// event and, after enough of them, so does its 256-deep queue.

	send, next, stop := stream(t, s)
	defer stop()
	for i := 0; i < 300; i++ {
		start := time.Now()
		send(fmt.Sprintf(`{"id":"r%d","cmd":"pane.report_state","pane":"w1:p1","event":`+
			`{"v":1,"ts":%f,"session_id":"d41c","harness":"claude-code","pane":"w1:p1",`+
			`"state":"working","source":"process","detail":"tick %d"}}`, i, nowSeconds(), i))
		m := next()
		elapsed := time.Since(start)
		if elapsed > 200*time.Millisecond {
			t.Fatalf("report %d took %v with a wedged subscriber, want under 200ms", i, elapsed)
		}
		if m["id"] != fmt.Sprintf("r%d", i) || m["ok"] != true {
			t.Fatalf("report %d = %v, want ok", i, m)
		}
	}

	// Free the wedged connection: closing its read pipe unsticks pump()'s
	// blocked Send with io.ErrClosedPipe; closing its write pipe ends
	// ServeConn's read loop. Both are needed, or one half leaks forever -
	// io.Pipe gives no other way to force a stuck Write to return.
	_ = wOut.Close()
	_ = wInW.Close()
	select {
	case <-wDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the wedged connection's ServeConn never returned")
	}
	waitForGoroutineBaseline(t, before, 3*time.Second)
}

// notifyWaiters must classify a waiter's effective state using the SAME
// "now" its caller already computed, not a fresh nowSeconds() of its own:
// two independent clock reads a heartbeat apart could straddle an ask's
// deadline and disagree about whether it has expired. Passing a
// deliberately stale "now", one that predates the ask's deadline while a
// live clock read would already be past it, proves notifyWaiters used
// exactly the value it was given: a fresh nowSeconds() call inside it would
// see the same deadline as already passed and report "working" instead.
func TestNotifyWaitersUsesThePassedNowNotItsOwn(t *testing.T) {
	s := newAgentServer(t)
	paneID := "w1:p1"

	stale := nowSeconds() - 100
	deadline := nowSeconds() - 50 // after stale, but already past by a live clock read

	w := &waiter{pane: paneID, ch: make(chan proto.PaneStateEvent, 1)}
	s.mu.Lock()
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	ev := proto.PaneStateEvent{
		V: 1, TS: stale, SessionID: paneID, Harness: "claude-code", Pane: &paneID,
		State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf x", Deadline: deadline},
	}
	s.notifyWaiters(paneID, ev, stale)

	select {
	case got := <-w.ch:
		if got.State != proto.StateBlocked {
			t.Fatalf("notifyWaiters(now=stale) reported state %q, want blocked: "+
				"it must judge the ask deadline against the now it was passed, not a fresh clock read",
				got.State)
		}
	default:
		t.Fatal("notifyWaiters did not notify the registered waiter")
	}
}
