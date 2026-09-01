package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// flowClient is one connection to ServeConn over pipes, read by a goroutine
// that hands each line to an unbuffered channel. A test that stops taking
// lines from that channel parks the reader on one line, and the pipe behind
// it blocks the server's next write. That is how a test plays a client that
// has stopped reading, with no kernel socket buffer to absorb anything.
type flowClient struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
	done  chan struct{}
	pane  string
}

func newFlowClient(t *testing.T, s *Server) *flowClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	fc := &flowClient{t: t, in: inW, lines: make(chan map[string]any), done: make(chan struct{})}
	t.Cleanup(func() {
		close(fc.done)
		_ = inW.Close()
	})
	go func() {
		defer close(fc.lines)
		r := bufio.NewReader(outR)
		for {
			b, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			select {
			case fc.lines <- m:
			case <-fc.done:
				return
			}
		}
	}()
	return fc
}

func (fc *flowClient) send(line string) {
	fc.t.Helper()
	if _, err := io.WriteString(fc.in, line+"\n"); err != nil {
		fc.t.Fatal(err)
	}
}

// next returns the next line, or false when none arrives within d.
func (fc *flowClient) next(d time.Duration) (map[string]any, bool) {
	fc.t.Helper()
	select {
	case m, ok := <-fc.lines:
		if !ok {
			fc.t.Fatal("the connection closed")
		}
		return m, true
	case <-time.After(d):
		return nil, false
	}
}

// replyFor reads until the reply with this id and returns it with every
// event that arrived before it. The order of a reply and a frame the pump
// wrote at the same moment is not fixed, so a test that needs the first
// frame after a verb looks in the events too.
func (fc *flowClient) replyFor(id string) (map[string]any, []map[string]any) {
	fc.t.Helper()
	var events []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := fc.next(time.Until(deadline))
		if !ok {
			break
		}
		if m["id"] == id {
			return m, events
		}
		if _, isEvent := m["event"]; isEvent {
			events = append(events, m)
		}
	}
	fc.t.Fatalf("no reply for id %q within 10 s", id)
	return nil, nil
}

func (fc *flowClient) expectOK(id string) map[string]any {
	fc.t.Helper()
	m, _ := fc.replyFor(id)
	if m["ok"] != true {
		fc.t.Fatalf("request %q failed: %v", id, m)
	}
	return m
}

func (fc *flowClient) expectEvent(kind string) map[string]any {
	fc.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := fc.next(time.Until(deadline))
		if !ok {
			break
		}
		if m["event"] == kind {
			return m
		}
	}
	fc.t.Fatalf("no %q event within 10 s", kind)
	return nil
}

// expectNoEvent reads for d and fails when an event of this kind arrives.
func (fc *flowClient) expectNoEvent(kind string, d time.Duration) {
	fc.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m, ok := fc.next(time.Until(deadline))
		if !ok {
			return
		}
		if m["event"] == kind {
			fc.t.Fatalf("a %q event arrived: %v", kind, m)
		}
	}
}

// firstFrame returns the first frame among events, or reads one.
func (fc *flowClient) firstFrame(events []map[string]any) map[string]any {
	fc.t.Helper()
	for _, ev := range events {
		if ev["event"] == "frame" {
			return ev
		}
	}
	return fc.expectEvent("frame")
}

// newAttachedClient starts a server with one quiet pty pane and one client
// attached to it that has already read the first full frame.
func newAttachedClient(t *testing.T) (*Server, *flowClient) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	fc := newFlowClient(t, s)
	fc.pane = id
	fc.send(`{"id":"a","cmd":"pane.attach","pane":"` + id + `","cols":20,"rows":6}`)
	_, events := fc.replyFor("a")
	fc.firstFrame(events)
	return s, fc
}

// writeLine puts one line on the pane's grid. The test writes the grid
// itself so the frame traffic is under its own control.
func writeLine(t *testing.T, s *Server, paneID string, i int) {
	t.Helper()
	lp, ok := s.Live(paneID)
	if !ok {
		t.Fatalf("pane %s is not live", paneID)
	}
	_, _ = lp.Grid.Write([]byte(fmt.Sprintf("line %d\r\n", i)))
}

// produceFrames changes the grid n times, at least one pump tick apart, so
// each change becomes its own frame.
func produceFrames(t *testing.T, s *Server, paneID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		writeLine(t, s, paneID, i)
		time.Sleep(2 * FrameInterval)
	}
}

// startChatter changes the grid every 20 ms until the test ends.
func startChatter(t *testing.T, s *Server, paneID string) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
				lp, ok := s.Live(paneID)
				if !ok {
					return
				}
				_, _ = lp.Grid.Write([]byte(fmt.Sprintf("chatter %d\r\n", i)))
			}
		}
	}()
}

func TestPausedPaneSendsNoFramesThenAFullFrameOnResume(t *testing.T) {
	s, fc := newAttachedClient(t)
	startChatter(t, s, fc.pane)
	fc.expectEvent("frame")

	fc.send(`{"id":"p","cmd":"events.pause","pane":"` + fc.pane + `"}`)
	reply := fc.expectOK("p")
	res, _ := reply["result"].(map[string]any)
	if res["paused"] != true || res["pane"] != fc.pane {
		t.Fatalf("pause reply = %v", reply)
	}
	fc.expectNoEvent("frame", 500*time.Millisecond)

	fc.send(`{"id":"r","cmd":"events.resume","pane":"` + fc.pane + `"}`)
	reply, events := fc.replyFor("r")
	if reply["ok"] != true {
		t.Fatalf("resume failed: %v", reply)
	}
	res, _ = reply["result"].(map[string]any)
	if res["paused"] != false {
		t.Fatalf("resume reply = %v", reply)
	}
	fr := fc.firstFrame(events)
	if fr["full"] != true {
		t.Fatalf("resume did not send a full frame: %v", fr)
	}
	rows, _ := fr["rows_changed"].(map[string]any)
	if len(rows) != 6 {
		t.Fatalf("the full frame carried %d rows, want all 6", len(rows))
	}
	// The chatter goes on, so the next frame is a delta again.
	if next := fc.expectEvent("frame"); next["full"] == true {
		t.Fatalf("the frame after the full one was full too: %v", next)
	}
}

// TestASlowClientGetsAFrameGapNotAStall lowers FrameBuffer so a short burst
// overflows it. The client takes no line while the burst runs, so the pipe
// blocks the drain on one frame and the pump fills the buffer behind it.
func TestASlowClientGetsAFrameGapNotAStall(t *testing.T) {
	prev := FrameBuffer
	FrameBuffer = 8
	t.Cleanup(func() { FrameBuffer = prev })
	s, fc := newAttachedClient(t)

	produceFrames(t, s, fc.pane, 5*(FrameBuffer+2))

	gap := fc.expectEvent("frame_gap")
	dropped, _ := gap["dropped"].(float64)
	if dropped < 1 {
		t.Fatalf("no drop reported: %v", gap)
	}
	if gap["pane"] != fc.pane {
		t.Fatalf("frame_gap names pane %v, want %s", gap["pane"], fc.pane)
	}
	fr := fc.expectEvent("frame")
	if fr["full"] != true {
		t.Fatalf("the frame after frame_gap is not full: %v", fr)
	}
}

func TestFlowVerbsOnAPaneThisConnectionIsNotAttachedToAreNotAttached(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	got := roundTrip(t, s,
		`{"id":"1","cmd":"events.pause","pane":"`+id+`"}`,
		`{"id":"2","cmd":"events.resume","pane":"`+id+`"}`,
		`{"id":"3","cmd":"events.pause"}`)
	for _, r := range got[:2] {
		if r.OK || r.Error.Code != proto.ErrNotAttached {
			t.Fatalf("got %+v, want not_attached", r)
		}
		if !strings.Contains(r.Error.Message, "coppice attach "+id) {
			t.Fatalf("message %q does not teach the attach command", r.Error.Message)
		}
	}
	if got[2].OK || got[2].Error.Code != proto.ErrBadRequest {
		t.Fatalf("got %+v, want bad_request for a missing pane", got[2])
	}
}

func TestDetachEndsTheFlowSoAPauseAfterItIsNotAttached(t *testing.T) {
	_, fc := newAttachedClient(t)
	fc.send(`{"id":"d","cmd":"pane.detach","pane":"` + fc.pane + `"}`)
	fc.expectOK("d")
	fc.send(`{"id":"p","cmd":"events.pause","pane":"` + fc.pane + `"}`)
	reply, _ := fc.replyFor("p")
	errObj, _ := reply["error"].(map[string]any)
	if reply["ok"] != false || errObj["code"] != string(proto.ErrNotAttached) {
		t.Fatalf("got %v, want not_attached", reply)
	}
}

func TestNewRegistersTheFlowVerbs(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"events.pause", "events.resume"} {
		if _, ok := s.handlers[name]; !ok {
			t.Fatalf("New did not register %s", name)
		}
	}
}

// A frame that carries a drop count and is discarded by the drain must not
// lose the count. The next frame the drain sends carries it. The drain is
// driven by hand here. The discard is caused by awaitFull, which holds until
// a full frame arrives, so the order is exact. A pause discards through the
// same branch.
func TestADropCountSurvivesADiscard(t *testing.T) {
	outR, outW := io.Pipe()
	t.Cleanup(func() { _ = outW.Close() })
	c := newClient(outW)
	t.Cleanup(c.Drop)
	flow := newPaneFlow()
	go c.drainFrames(flow)
	lines := make(chan map[string]any, 8)
	go func() {
		r := bufio.NewReader(outR)
		for {
			b, err := r.ReadBytes('\n')
			if err != nil {
				close(lines)
				return
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			lines <- m
		}
	}()
	next := func() map[string]any {
		t.Helper()
		select {
		case m := <-lines:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no line within 5 s")
			return nil
		}
	}
	delta := proto.Frame{Event: "frame", Pane: "w1:p1", Seq: 8, RowsChanged: map[int][]proto.Cell{}}
	full := proto.Frame{Event: "frame", Pane: "w1:p1", Seq: 9, Full: true,
		RowsChanged: map[int][]proto.Cell{}}

	flow.mu.Lock()
	flow.awaitFull = true
	flow.mu.Unlock()
	flow.frames <- queuedFrame{frame: delta, dropped: 3}
	flow.frames <- queuedFrame{frame: full, dropped: 2}
	close(flow.frames)

	gap := next()
	if gap["event"] != "frame_gap" || gap["dropped"] != float64(5) {
		t.Fatalf("got %v, want a frame_gap with dropped 5", gap)
	}
	if fr := next(); fr["event"] != "frame" || fr["full"] != true {
		t.Fatalf("got %v, want the full frame", fr)
	}
}
