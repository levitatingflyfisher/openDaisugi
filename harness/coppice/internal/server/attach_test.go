package server

import (
	"encoding/json"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// stream drives ServeConn and hands back a decoder plus a send function, so a
// test can interleave requests and read the events they cause.
func stream(t *testing.T, s *Server) (send func(string), next func() map[string]any, stop func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	d := proto.NewDecoder(outR)
	send = func(line string) {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	// d.Next() blocks until a line arrives. A pane that has gone quiet after
	// its last frame (the ordinary end state of every test pane here, which
	// sleeps once its output is done) will never produce another one, so a
	// caller that asks for one more line than the server will ever send
	// would otherwise hang this goroutine, and the test, forever. The
	// deadline below is what turns a bug like that into a failed test
	// instead of a wedged `go test` run.
	next = func() map[string]any {
		t.Helper()
		type result struct {
			line []byte
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			line, err := d.Next()
			ch <- result{line, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("reading the next line: %v", r.err)
			}
			var m map[string]any
			if err := json.Unmarshal(r.line, &m); err != nil {
				t.Fatal(err)
			}
			return m
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the next line; the server stopped sending anything")
			return nil
		}
	}
	stop = func() { _ = inW.Close() }
	return
}

// waitForGoroutineBaseline polls until runtime.NumGoroutine() is back at or
// below before, failing the test if it never gets there. Every pump test
// uses this to prove the frame pump it started actually exits, instead of
// trusting that a passing assertion elsewhere means nothing was left
// running.
func waitForGoroutineBaseline(t *testing.T, before int, deadline time.Duration) {
	t.Helper()
	dl := time.Now().Add(deadline)
	for {
		if n := runtime.NumGoroutine(); n <= before {
			return
		}
		if time.Now().After(dl) {
			t.Fatalf("goroutine count did not return to baseline: before=%d now=%d",
				before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rowText reconstructs one row's plain text from its wire cells. Each cell
// on the wire is [text, fg, bg, attrs], one grid cell per array entry, so a
// two-character run like "hi" never appears as a contiguous substring of the
// frame's raw JSON - it is "h" and "i" in two separate cell arrays, with a
// closing bracket, a comma and an opening bracket sitting between them. This
// walks the cells and concatenates their text instead of grepping the wire
// bytes.
func rowText(rows map[string]any, row int) string {
	cells, _ := rows[strconv.Itoa(row)].([]any)
	var b strings.Builder
	for _, c := range cells {
		cell, _ := c.([]any)
		if len(cell) > 0 {
			if s, ok := cell[0].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

func TestAttachSendsAFullFirstFrameThenRowDiffs(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","echo hi; sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	// Captured here, not before stream()/pane.create: those start the
	// server's own long-lived goroutines for this connection and pane (the
	// PTY reader, watchExit) that stop() below cannot and must not tear
	// down - only closing the pane does that. Capturing "before" any
	// earlier than this made the baseline check below only pass once the
	// child's own sleep 3 happened to finish, coupling this test's pass/fail
	// to real time it has no business depending on; the two sibling pump
	// tests (TestFramePumpEndsOnDetach, TestFramePumpEndsWhenThePaneCloses)
	// already capture it here for the same reason.
	before := runtime.NumGoroutine()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	deadline := time.Now().Add(6 * time.Second)
	sawFull := false
	sawEcho := false
	for time.Now().Before(deadline) {
		m := next()
		if m["event"] != "frame" {
			continue
		}
		rows, _ := m["rows_changed"].(map[string]any)
		if !sawFull {
			if len(rows) != 6 {
				t.Fatalf("first frame carried %d rows, want all 6", len(rows))
			}
			sawFull = true
			// Deliberately falls through to the "hi" check below rather than
			// continuing: echo is fast enough that it can land before this
			// pump's very first tick, in which case the first frame IS the
			// one carrying it, not a later diff. Treating this frame as
			// "only the baseline" would mean waiting for a second frame
			// that a now-quiet pane will never send.
		}
		for row := 0; row < 6; row++ {
			if strings.Contains(rowText(rows, row), "hi") {
				sawEcho = true
				break
			}
		}
		if sawEcho {
			break
		}
	}
	if !sawEcho {
		t.Fatal("no frame carried the child's output within 6 s")
	}

	// The pane keeps sleeping with nothing left to say, so no frame will ever
	// change again. Disconnecting must still wind the pump down: it cannot
	// be waiting on a changed frame to notice the client is gone.
	stop()
	waitForGoroutineBaseline(t, before, 3*time.Second)
}

func TestAttachOnAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.attach","pane":"w9:p9"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrNoSuchPane {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

func TestDetachWithoutAttachIsNotAttached(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","sleep 2"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.detach","pane":"w1:p1"}`)
	if got[1].OK || got[1].Error.Code != proto.ErrNotAttached {
		t.Fatalf("got %+v, want not_attached", got[1])
	}
}

// TestDetachRemovesTheStateSubscriptionAttachAdded checks that
// pane.attach subscribes a client to a pane's state events as a side
// effect (so a renderer showing a status line does not need a second
// call), and pane.detach must undo exactly that - not merely stop the
// frame pump - when nothing else asked for it. A client that only ever
// watched a pane through attach must stop hearing about it once it has
// explicitly said it is done.
func TestDetachRemovesTheStateSubscriptionAttachAdded(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}
	send(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`)
	for {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("pane.detach failed: %v", m)
			}
			break
		}
	}

	// This event must never reach the client: pane.detach undid the
	// subscription pane.attach added for w1:p1, and nothing else asked for
	// it.
	detached := "w1:p1"
	s.ApplyState(detached, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &detached,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})

	// Proving an absence by blocking on the next read would hang forever if
	// the fix is right - there is nothing to read. Instead, subscribe to a
	// second, unrelated pane and fire an event for that one: the server
	// delivers events to one client in the order ApplyState was called for
	// them, so if w1:p1's event above had been (wrongly) delivered, it
	// would be sitting ahead of this marker in the queue. Seeing the marker
	// as the very first thing read here proves w1:p1's event was never
	// queued at all.
	send(`{"id":"4","cmd":"events.subscribe","panes":["marker"],"kinds":["state"]}`)
	for {
		m := next()
		if m["id"] == "4" {
			if m["ok"] != true {
				t.Fatalf("events.subscribe failed: %v", m)
			}
			break
		}
	}
	marker := "marker"
	s.ApplyState(marker, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &marker,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	if m := next(); m["event"] != "state" || m["pane"] != "marker" {
		t.Fatalf("first event after detach = %v, want the marker event "+
			"(w1:p1's own event should never have been sent)", m)
	}
}

// TestDetachLeavesAnExplicitSubscriptionAlone covers the other half:
// pane.detach must not undo a subscription the client asked for on its
// own, even one that happens to cover a pane the client also attached to.
func TestDetachLeavesAnExplicitSubscriptionAlone(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}
	send(`{"id":"3","cmd":"events.subscribe","panes":"*","kinds":["state"]}`)
	for {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("events.subscribe failed: %v", m)
			}
			break
		}
	}
	send(`{"id":"4","cmd":"pane.detach","pane":"w1:p1"}`)
	for {
		m := next()
		if m["id"] == "4" {
			if m["ok"] != true {
				t.Fatalf("pane.detach failed: %v", m)
			}
			break
		}
	}

	p := "w1:p1"
	s.ApplyState(p, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &p,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	if m := next(); m["event"] != "state" || m["pane"] != "w1:p1" {
		t.Fatalf(`got %v, want the w1:p1 state event - the explicit "*" subscribe must survive detach`, m)
	}
}

func TestSubscribeDeliversStateEventsForTheChosenPanesOnly(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","panes":["w1:p1"],"kinds":["state"]}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("events.subscribe failed: %v", m)
	}

	// An event for another pane must not arrive.
	other := "w1:p2"
	s.ApplyState(other, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &other,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	wanted := "w1:p1"
	s.ApplyState(wanted, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &wanted,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	m := next()
	if m["event"] != "state" || m["pane"] != "w1:p1" {
		t.Fatalf("first delivered event = %v, want the w1:p1 state event", m)
	}
}

func TestSubscribeToEveryPaneWithAStar(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"events.subscribe","panes":"*","kinds":["state"]}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("events.subscribe failed: %v", m)
	}
	p := "w3:p7"
	s.ApplyState(p, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &p,
		State: proto.StateWorking, Source: proto.SrcProcess,
	})
	if m := next(); m["pane"] != "w3:p7" {
		t.Fatalf("star subscription missed %v", m)
	}
}

func TestSubscribeRejectsAnUnknownKind(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	got := roundTrip(t, s, `{"id":"1","cmd":"events.subscribe","kinds":["confetti"]}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
}

// TestSubscribeRejectsDegeneratePanesValues covers the panes values that
// must not silently subscribe to nothing: an empty list, an empty-string
// pane id, and a JSON null (which unmarshals into handleSubscribe's "one
// string" branch the same way an explicit "" does - both leave the target
// string at its zero value). A caller who sent one of these almost
// certainly meant something, not a subscription that will never deliver.
func TestSubscribeRejectsDegeneratePanesValues(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	cases := []struct {
		name  string
		panes string // raw JSON for the "panes" value
	}{
		{"empty list", `[]`},
		{"empty string", `""`},
		{"null", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := roundTrip(t, s,
				`{"id":"1","cmd":"events.subscribe","panes":`+c.panes+`,"kinds":["state"]}`)
			if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
				t.Fatalf("panes=%s: got %+v, want bad_request", c.panes, got[0])
			}
		})
	}
}

// One wedged client must not stall ApplyState for every pane. A PWA on a
// stalled tailnet is the realistic case, and the gate hook is blocking on a
// pane.report_state response while it happens.
func TestAWedgedClientIsDroppedRatherThanStallingTheServer(t *testing.T) {
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	// A reader that never reads: its pipe fills and its Send blocks.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	defer inW.Close()
	_, _ = io.WriteString(inW, `{"id":"1","cmd":"events.subscribe","panes":"*","kinds":["state"]}`+"\n")
	// Read only the subscribe response, then stop reading for ever.
	if _, err := proto.NewDecoder(outR).Next(); err != nil {
		t.Fatal(err)
	}

	p := "w1:p1"
	done := make(chan struct{})
	go func() {
		for i := 0; i < EventQueue*4; i++ {
			s.ApplyState(p, proto.PaneStateEvent{
				V: 1, TS: nowSeconds(), SessionID: "s", Harness: "shell", Pane: &p,
				State: proto.StateWorking, Source: proto.SrcProcess,
				Detail: strconv.Itoa(i),
			})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyState stalled behind a client that stopped reading")
	}
}

func TestFramesAreCoalescedAtSixtyHertz(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	frames := 0
	// readResponse drains events up to and including the response for id,
	// counting any frame events it passes along the way. A bare next() here
	// would risk swallowing exactly the frame this test is trying to count:
	// under load, the frame pump's first tick can beat the pane.attach
	// response onto the wire (the response write is a few microseconds of
	// work, but nothing guarantees the scheduler runs it before the pump's
	// next 16 ms tick fires), and a diff-only pane like this one - it goes
	// quiet right after its burst - may never send a second frame to make up
	// for a swallowed first one.
	readResponse := func(id string) map[string]any {
		t.Helper()
		for {
			m := next()
			if m["event"] == "frame" {
				frames++
				continue
			}
			if m["id"] == id {
				return m
			}
		}
	}
	// A child that writes a distinct line every 20 ms for about 1.5 s, then
	// exits. A fast, CPU-bound burst (3000 lines as fast
	// as sh could loop) makes this test flaky in both directions:
	// on a quiet box the burst could finish, and the pane exit, within a
	// couple of milliseconds - faster than this test could even finish
	// sending pane.attach - and on a loaded one under -race, the reader
	// goroutine parsing 3000 lines held the grid's mutex almost
	// continuously, starving framePump's own fs.Next() call long enough
	// that "done" could reach the wire before any frame did. Both failure
	// modes trace to the same root cause: a CPU-bound child leaves no room
	// for the pump to run. Pacing the writes with a real sleep between each
	// one removes that contention (the child is mostly asleep, not burning
	// CPU), which is what makes both a real ceiling and a real floor
	// possible to assert here without depending on how fast this machine
	// happens to be.
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","i=0; while [ $i -lt 75 ]; do echo line-$i; sleep 0.02; i=$((i+1)); done"],` +
		`"kind":"pty","cols":20,"rows":6}`)
	if m := readResponse("1"); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := readResponse("2"); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	// pane.attach already subscribed this client to state events for this
	// pane (see handleAttach), so the "done" event watchExit sends once the
	// child exits arrives on this same stream, interleaved with the frames.
	// Collecting until then, rather than for a fixed window, keeps this
	// test's pass/fail independent of exactly how long the paced burst
	// takes on this machine. The 20 s ceiling is a backstop against a
	// genuine hang, not a budget this test expects to use - the burst alone
	// is paced to take about 1.5 s.
	start := time.Now()
	deadline := start.Add(20 * time.Second)
	sawDone := false
	for time.Now().Before(deadline) {
		m := next()
		switch {
		case m["event"] == "frame":
			frames++
		case m["event"] == "state" && m["state"] == proto.StateDone:
			sawDone = true
		}
		if sawDone {
			break
		}
	}
	if !sawDone {
		t.Fatal("never saw the pane's done state event within 20 s")
	}
	elapsed := time.Since(start)
	// Both bounds are load-bearing here, not just the ceiling: the floor
	// proves this run actually observed real frame traffic, so the ceiling
	// check above it is asserting something, not passing vacuously because
	// framePump never got scheduled at all. 75 lines
	// 20 ms apart guarantees at least that many distinct grid changes over
	// about 1.5 s of real, mostly-idle wall-clock time - not a CPU-bound
	// sprint a starved scheduler could skip past entirely - so 5 is a safe
	// floor even under -race.
	const minFrames = 5
	if frames < minFrames {
		t.Fatalf("got %d frames over %s, want at least %d - the pump did not produce real frame traffic",
			frames, elapsed, minFrames)
	}
	rate := float64(frames) / elapsed.Seconds()
	// FrameInterval (16 ms) puts a hard ceiling of 1000/16 = 62.5 Hz on how
	// often framePump can ever emit a frame, and the paced burst only
	// changes the grid 75 times across about 1.5 s (~50 Hz) even in
	// principle, so 75 Hz is already a generous ceiling over both of those
	// - what it actually guards against is a regression back to firing a
	// frame per byte or per write instead of per tick, which would blow
	// well past it.
	const maxRate = 75.0
	if rate > maxRate {
		t.Fatalf("frame rate = %.1f/s over %s (%d frames), want the 60 Hz ceiling to hold (max %.0f/s)",
			rate, elapsed, frames, maxRate)
	}
}

// TestFramePumpEndsOnDetach proves the pump stops as soon as pane.detach
// runs, not merely once the connection eventually closes: a client that
// stays connected but detaches (switching panes in a multi-pane cockpit,
// say) must not accumulate one live pump per pane it ever looked at.
func TestFramePumpEndsOnDetach(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	before := runtime.NumGoroutine()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	// Wait for the pump to actually tick before detaching, so this proves a
	// clean exit rather than the pump simply never having started. A frame
	// event is a functional signal that the pump is alive: NumGoroutine is
	// not, because the request-dispatch goroutine for this very pane.attach
	// (and this test's own reader goroutine inside next()) exit at almost
	// the same moment the pump starts, so the count can net to zero, or even
	// dip below "before", at the instant a poll samples it.
	for {
		if m := next(); m["event"] == "frame" {
			break
		}
	}

	send(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`)
	// Drain any frame already queued ahead of the detach response.
	for {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("pane.detach failed: %v", m)
			}
			break
		}
	}

	waitForGoroutineBaseline(t, before, 2*time.Second)
}

// TestFramePumpEndsWhenThePaneCloses proves the pump does not spin forever
// against a grid an operator has just closed and freed.
func TestFramePumpEndsWhenThePaneCloses(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	before := runtime.NumGoroutine()
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	// See TestFramePumpEndsOnDetach for why this waits on a frame event
	// rather than polling runtime.NumGoroutine() for an increase.
	for {
		if m := next(); m["event"] == "frame" {
			break
		}
	}

	send(`{"id":"3","cmd":"pane.close","pane":"w1:p1"}`)
	for {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("pane.close failed: %v", m)
			}
			break
		}
	}

	waitForGoroutineBaseline(t, before, 2*time.Second)
}

// TestDisconnectingClearsOperatorStatus is the direct test of the master
// spec 3.1 rule that only a client with an OPEN pane.attach may speak with
// source operator: a client that vanished without calling pane.detach must
// not keep counting as one.
func TestDisconnectingClearsOperatorStatus(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	var c *Client
	s.mu.Lock()
	for cl := range s.clients {
		c = cl
	}
	s.mu.Unlock()
	if c == nil {
		t.Fatal("no client registered for this connection")
	}
	if !c.Operator("w1:p1") {
		t.Fatal("pane.attach did not record operator status")
	}

	stop() // disconnect without ever calling pane.detach

	deadline := time.Now().Add(2 * time.Second)
	for c.Operator("w1:p1") {
		if time.Now().After(deadline) {
			t.Fatal("operator status was never cleared after the client disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPaneGoneClearsOperatorStatus proves the frame pump's pane-gone
// branches clear this client's c.Attached entry the same way c.Dead()
// does - otherwise Operator() would keep reporting true for a pane an
// operator pane.close already closed and freed, well after the connection
// itself stays open and the client never called pane.detach.
func TestPaneGoneClearsOperatorStatus(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	var c *Client
	s.mu.Lock()
	for cl := range s.clients {
		c = cl
	}
	s.mu.Unlock()
	if c == nil {
		t.Fatal("no client registered for this connection")
	}
	if !c.Operator("w1:p1") {
		t.Fatal("pane.attach did not record operator status")
	}

	send(`{"id":"3","cmd":"pane.close","pane":"w1:p1"}`)
	for {
		m := next()
		if m["id"] == "3" {
			if m["ok"] != true {
				t.Fatalf("pane.close failed: %v", m)
			}
			break
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for c.Operator("w1:p1") {
		if time.Now().After(deadline) {
			t.Fatal("operator status was never cleared after the pane closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGridClosedClearsOperatorStatusEvenWithTheLiveEntryStillPresent
// exercises the frame pump's other pane-gone branch directly, rather than
// relying on pane.close's own ordering to reach it: an ErrGridClosed read -
// the grid closed while the live entry itself is still there, the narrow
// window handleClose's Grid.Close()-then-removeLive ordering leaves open -
// must clear c.Attached the same way a fully removed live entry does.
func TestGridClosedClearsOperatorStatusEvenWithTheLiveEntryStillPresent(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	var c *Client
	s.mu.Lock()
	for cl := range s.clients {
		c = cl
	}
	s.mu.Unlock()
	if c == nil {
		t.Fatal("no client registered for this connection")
	}
	if !c.Operator("w1:p1") {
		t.Fatal("pane.attach did not record operator status")
	}

	lp, ok := s.Live("w1:p1")
	if !ok {
		t.Fatal("no live pane")
	}
	lp.Grid.Close() // the live entry stays; only the grid goes away

	deadline := time.Now().Add(2 * time.Second)
	for c.Operator("w1:p1") {
		if time.Now().After(deadline) {
			t.Fatal("operator status was never cleared after the grid closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDetachAfterThePumpAlreadyForgotThePaneLeavesTheSubscriptionAlone
// guards against pane.detach narrowing the subscription
// unconditionally, even on a call that did not do any detaching itself -
// framePump already had, moments earlier, because an operator closed the
// pane from a second connection first. That raced watchExit's own cleanup:
// the detach call correctly came back not_attached, but the subscription
// went away anyway, purely depending on whether this detach happened to run
// before or after the pump's own forget().
func TestDetachAfterThePumpAlreadyForgotThePaneLeavesTheSubscriptionAlone(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.attach failed: %v", m)
	}

	var c *Client
	s.mu.Lock()
	for cl := range s.clients {
		c = cl
	}
	s.mu.Unlock()
	if c == nil {
		t.Fatal("no client registered for this connection")
	}
	if !c.Operator("w1:p1") {
		t.Fatal("pane.attach did not record operator status")
	}

	// Close the pane from a second, unrelated connection - the same as an
	// operator running coppice pane close from a different terminal.
	closed := roundTrip(t, s, `{"id":"1","cmd":"pane.close","pane":"w1:p1"}`)
	if !closed[0].OK {
		t.Fatalf("pane.close failed: %v", closed[0])
	}

	// Wait for framePump to notice on its own and forget the pane - the
	// same signal TestPaneGoneClearsOperatorStatus uses to prove the pump,
	// not this test, is what did the forgetting.
	deadline := time.Now().Add(2 * time.Second)
	for c.Operator("w1:p1") {
		if time.Now().After(deadline) {
			t.Fatal("the frame pump never forgot the closed pane")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// pane.detach now runs against a pane this client's c.Attached no longer
	// mentions - it must report not_attached, same as any other detach on a
	// pane never attached at all. The kill behind pane.close above also
	// makes the real child process exit, which fires watchExit's own
	// natural "done" event on its own schedule - a real event this
	// subscription is right to still be open for, and one that can legally
	// land on the wire before, after, or interleaved with this detach's
	// response, so this reads by id rather than assuming the very next line
	// is the response.
	send(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`)
	var m map[string]any
	for {
		m = next()
		if m["id"] == "3" {
			break
		}
	}
	if m["ok"] != false {
		t.Fatalf("pane.detach after the pump already forgot the pane = %v, want an error", m)
	}
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil || errObj["code"] != string(proto.ErrNotAttached) {
		t.Fatalf("pane.detach error = %v, want not_attached", m["error"])
	}

	// The subscription pane.attach set up must still be intact: this detach
	// did not do the detaching, so it must not have narrowed anything.
	// Proving that by firing another ApplyState and watching for it on the
	// wire, the way the sibling tests in this file do, does not work here:
	// master spec 3.1 makes done terminal (state.Merge's rule 2, "absorbing
	// everything after it"), and the pane.close above already made the real
	// child exit, so by now the store's current state for w1:p1 IS done -
	// any later non-done ApplyState call for it, however this test names
	// the source, gets absorbed unchanged and Store.Apply reports no change,
	// so nothing would ever reach the wire to prove anything either way.
	// This checks the bookkeeping directly instead - c.SubPanes/c.autoPanes
	// are exactly what handleDetach reads and writes, so this is the same
	// fact a real subsequent event's delivery would depend on.
	c.mu.Lock()
	stillSubscribed := c.SubPanes["w1:p1"]
	c.mu.Unlock()
	if !stillSubscribed {
		t.Fatal("pane.detach narrowed a subscription it did not itself create " +
			"(this detach call reported not_attached, meaning the pump had already " +
			"forgotten the pane on its own)")
	}
}

// TestNewRegistersTheAttachVerbs is TestNewRegistersThePaneVerbs's sibling
// for this file: New wires RegisterAttachCommands into itself, the same way
// it wires RegisterPaneCommands, so a caller cannot forget the call and ship
// a daemon with pane verbs but no pane.attach. This uses newTestServer
// directly, not calling RegisterAttachCommands itself - every other test in
// this file does, which means none of them would notice if that wiring in
// New were ever reverted.
func TestNewRegistersTheAttachVerbs(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"events.subscribe","kinds":["state"]}`)
	if !got[0].OK {
		t.Fatalf("events.subscribe is not registered by New: %+v", got[0].Error)
	}
}

// TestAttachingTwiceToTheSamePaneIsIdempotent covers the "already attached"
// branch: a second pane.attach for a pane this client already watches must
// not start a second competing pump.
func TestAttachingTwiceToTheSamePaneIsIdempotent(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()

	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() +
		`","cmd_argv":["sh","-c","sleep 3"],"kind":"pty","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	send(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("first pane.attach failed: %v", m)
	}

	before := runtime.NumGoroutine()
	send(`{"id":"3","cmd":"pane.attach","pane":"w1:p1","cols":20,"rows":6}`)
	if m := next(); m["ok"] != true {
		t.Fatalf("second pane.attach failed: %v", m)
	}
	// No new pump should have started for the same pane.
	time.Sleep(50 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("attaching twice started another goroutine: before=%d now=%d", before, n)
	}
}
