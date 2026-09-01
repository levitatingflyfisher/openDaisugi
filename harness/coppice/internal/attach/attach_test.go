package attach

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// listenUnix starts an in-process unix socket listener for one test. Every
// connection it accepts arrives on the returned channel. The listener and
// its accept loop stop when the test ends, and any connection the test never
// consumed is closed too.
func listenUnix(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 8)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-stopped
		for {
			select {
			case c := <-conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return sock, conns
}

func acceptConn(t *testing.T, conns <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection arrived in time")
		return nil
	}
}

// acceptCall accepts the next connection and reads its first request. Run
// makes a background pane.list call, request id "lbl", to learn this pane's
// label; acceptCall answers that call with an empty pane list and keeps
// looking, so a test waiting for the attach connection or a neighbour lookup
// never mistakes the label fetch for it.
func acceptCall(t *testing.T, conns <-chan net.Conn) (net.Conn, *proto.Decoder, *proto.Encoder, proto.Request) {
	t.Helper()
	for {
		c := acceptConn(t, conns)
		dec := proto.NewDecoder(c)
		enc := proto.NewEncoder(c)
		req := readRequestFrom(t, dec)
		if req.ID == "lbl" {
			_ = enc.Send(proto.OKResp(req.ID, map[string]any{"panes": []map[string]any{}}))
			_ = c.Close()
			continue
		}
		return c, dec, enc, req
	}
}

// readRequestFrom reads one line and decodes it as a request. It never hangs
// a test past 5 seconds: a wedged read is a failure, not a stall.
func readRequestFrom(t *testing.T, dec *proto.Decoder) proto.Request {
	t.Helper()
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := dec.Next()
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("reading a request: %v", r.err)
		}
		var req proto.Request
		if err := json.Unmarshal(r.line, &req); err != nil {
			t.Fatalf("request line is not valid JSON: %v", err)
		}
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a request")
		return proto.Request{}
	}
}

func sendState(t *testing.T, enc *proto.Encoder, ev proto.PaneStateEvent) {
	t.Helper()
	if err := enc.Send(stateEvent{Event: "state", PaneStateEvent: ev}); err != nil {
		t.Fatal(err)
	}
}

// syncBuf is a Writer safe for one goroutine to fill while the test polls it
// from another. Run writes to Out from its own goroutine; the test reads it
// while Run is still running, so a plain bytes.Buffer would race.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitForContains(t *testing.T, get func() string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(get(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for output to contain %q, got %q", want, get())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type runOutcome struct {
	next Next
	err  error
}

func waitRun(t *testing.T, ch <-chan runOutcome) runOutcome {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return in time")
		return runOutcome{}
	}
}

func TestReadKeysDeliversBytesAndClosesOnEOF(t *testing.T) {
	r, w := io.Pipe()
	ch := ReadKeys(r)
	go func() {
		_, _ = w.Write([]byte{'a', 'b'})
		_ = w.Close()
	}()
	var got []byte
	for b := range ch {
		got = append(got, b)
	}
	if string(got) != "ab" {
		t.Fatalf("ReadKeys delivered %q, want %q", got, "ab")
	}
}

func TestRunAttachesRendersAFrameAndDetaches(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 5, Rows: 2, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if req.Cmd != "pane.attach" {
		t.Fatalf("first request was %q, want pane.attach", req.Cmd)
	}
	if id, _ := req.Str("pane"); id != "w1:p1" {
		t.Fatalf("pane.attach named pane %q, want w1:p1", id)
	}
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}
	if err := enc.Send(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 5, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "h"}, {Text: "i"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForContains(t, out.String, "hi", 2*time.Second)

	keys <- Leader
	keys <- 'd'

	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
	}
	if id, _ := detachReq.Str("pane"); id != "w1:p1" {
		t.Fatalf("pane.detach named pane %q, want w1:p1", id)
	}

	r := waitRun(t, resultCh)
	if r.err != nil {
		t.Fatalf("Run returned an error: %v", r.err)
	}
	if r.next != (Next{}) {
		t.Fatalf("Run returned %+v, want an empty Next", r.next)
	}
	close(keys)
}

func TestRunReadsFromInWhenKeysIsNil(t *testing.T) {
	sock, conns := listenUnix(t)
	inR, inW := io.Pipe()
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{Socket: sock, Pane: "w1:p1", In: inR, Out: out, Cols: 10, Rows: 3})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	go func() {
		_, _ = inW.Write([]byte{Leader, 'd'})
	}()

	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
	}
	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
	}
	_ = inW.Close()
}

// Run only adopts a state event whose pane matches the one it is attached
// to. A state event broadcasts for every pane the server knows, not only the
// attached one, so an unfiltered Run would show a stranger's state on this
// pane's status line.
func TestRunOnlyUpdatesStatusFromMatchingPaneStateEvents(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 40, Rows: 6, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	other := "w1:p2"
	sendState(t, enc, proto.PaneStateEvent{
		V: 1, TS: 1, SessionID: "s2", Harness: "codex", Pane: &other,
		State: proto.StateIdle, Source: proto.SrcManifest,
	})

	this := "w1:p1"
	sendState(t, enc, proto.PaneStateEvent{
		V: 1, TS: 2, SessionID: "s1", Harness: "claude-code", Pane: &this,
		State: proto.StateWorking, Source: proto.SrcHeadless,
	})

	// The second event proves the read loop has already passed the first
	// one: it is for this pane, so it must show up.
	waitForContains(t, out.String, "working", 2*time.Second)
	got := out.String()
	if strings.Contains(got, "idle") {
		t.Fatalf("status line adopted another pane's state: %q", got)
	}
	if strings.Contains(got, "codex") {
		t.Fatalf("status line adopted another pane's harness: %q", got)
	}
	if strings.Contains(got, "manifest") {
		t.Fatalf("status line adopted another pane's source: %q", got)
	}
	if !strings.Contains(got, "claude-code") {
		t.Fatalf("status line lost this pane's own harness: %q", got)
	}

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, dec)
	waitRun(t, resultCh)
	close(keys)
}

// A resize must never leave the status line describing a screen that no
// longer exists: after the pane shrinks, the status line must move to the
// new last row and truncate to the new width, not the size Run dialed with.
func TestRunStatusLineFollowsAResizedScreen(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 5, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	if err := enc.Send(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 3, Rows: 1, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "a"}, {Text: "b"}, {Text: "c"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForContains(t, out.String, "abc", 2*time.Second)

	got := out.String()
	if !strings.Contains(got, "\x1b[2;1H\x1b[7m") {
		t.Fatalf("status line did not move to the resized pane's own row: %q", got)
	}
	if strings.Contains(got, "\x1b[3;1H\x1b[7m") {
		t.Fatalf("status line stayed at the stale pre-resize row: %q", got)
	}

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, dec)
	waitRun(t, resultCh)
	close(keys)
}

func TestRunActCloseSendsPaneClose(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- Leader
	keys <- 'x'

	closeReq := readRequestFrom(t, dec)
	if closeReq.Cmd != "pane.close" {
		t.Fatalf("got cmd %q, want pane.close", closeReq.Cmd)
	}
	if id, _ := closeReq.Str("pane"); id != "w1:p1" {
		t.Fatalf("pane.close named pane %q, want w1:p1", id)
	}

	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
	}
	close(keys)
}

func TestRunActNextAsksPaneListAndReturnsNeighbour(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	attachConn, adec, aenc, req := acceptCall(t, conns)
	defer attachConn.Close()
	if err := aenc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- Leader
	keys <- 'n'

	listConn, _, lenc, lreq := acceptCall(t, conns)
	defer listConn.Close()
	if lreq.Cmd != "pane.list" {
		t.Fatalf("got cmd %q, want pane.list", lreq.Cmd)
	}
	if err := lenc.Send(proto.OKResp(lreq.ID, map[string]any{"panes": []map[string]any{
		{"id": "w1:p1", "closed": false},
		{"id": "w1:p2", "closed": false},
	}})); err != nil {
		t.Fatal(err)
	}

	detachReq := readRequestFrom(t, adec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
	}

	r := waitRun(t, resultCh)
	if r.err != nil {
		t.Fatalf("Run returned an error: %v", r.err)
	}
	if r.next.Pane != "w1:p2" || r.next.Create {
		t.Fatalf("Run returned %+v, want Next{Pane: %q}", r.next, "w1:p2")
	}
	close(keys)
}

// ctrl+a n and ctrl+a p must both skip a closed pane in the middle of the
// list: with three panes and the middle one closed, either key from the
// first pane reaches the third, one by wrapping forward and the other by
// wrapping back to the only other open pane.
func TestRunActNextAndPrevSkipClosedPanes(t *testing.T) {
	panes := []map[string]any{
		{"id": "w1:p1", "closed": false},
		{"id": "w1:p2", "closed": true},
		{"id": "w1:p3", "closed": false},
	}
	run := func(t *testing.T, key byte, want string) {
		sock, conns := listenUnix(t)
		keys := make(chan byte, 4)
		out := &syncBuf{}
		resultCh := make(chan runOutcome, 1)
		go func() {
			n, err := Run(Options{
				Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
				Cols: 10, Rows: 3, Keys: keys,
			})
			resultCh <- runOutcome{n, err}
		}()

		attachConn, adec, aenc, req := acceptCall(t, conns)
		defer attachConn.Close()
		if err := aenc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
			t.Fatal(err)
		}

		keys <- Leader
		keys <- key

		listConn, _, lenc, lreq := acceptCall(t, conns)
		defer listConn.Close()
		if err := lenc.Send(proto.OKResp(lreq.ID, map[string]any{"panes": panes})); err != nil {
			t.Fatal(err)
		}

		_ = readRequestFrom(t, adec) // pane.detach
		r := waitRun(t, resultCh)
		if r.err != nil {
			t.Fatalf("Run returned an error: %v", r.err)
		}
		if r.next.Pane != want {
			t.Fatalf("Run returned %+v, want Next{Pane: %q}", r.next, want)
		}
		close(keys)
	}
	t.Run("next", func(t *testing.T) { run(t, 'n', "w1:p3") })
	t.Run("prev", func(t *testing.T) { run(t, 'p', "w1:p3") })
}

// A leader key with no other open pane to go to prints the reason and stays
// attached: a control that silently does nothing is worse than a message
// saying why.
func TestRunWithNoOtherOpenPanePrintsTheMessageAndContinues(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 80, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	attachConn, adec, aenc, req := acceptCall(t, conns)
	defer attachConn.Close()
	if err := aenc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- Leader
	keys <- 'n'

	listConn, _, lenc, lreq := acceptCall(t, conns)
	defer listConn.Close()
	if err := lenc.Send(proto.OKResp(lreq.ID, map[string]any{"panes": []map[string]any{
		{"id": "w1:p1", "closed": false},
	}})); err != nil {
		t.Fatal(err)
	}

	waitForContains(t, out.String, "There is no other open pane.", 2*time.Second)

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, adec)
	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v after the no-neighbour message, want an empty Next and no error",
			r.next, r.err)
	}
	close(keys)
}

func TestRunReturnsAnErrorWhenAttachFails(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 1)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:pX", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, _, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.ErrResp(req.ID, proto.ErrNoSuchPane,
		`no pane "w1:pX". Run: coppice pane list`)); err != nil {
		t.Fatal(err)
	}

	r := waitRun(t, resultCh)
	if r.err == nil {
		t.Fatal("Run returned no error for a failed attach")
	}
	if !strings.Contains(r.err.Error(), string(proto.ErrNoSuchPane)) {
		t.Fatalf("Run's error %q does not name the code", r.err.Error())
	}
	close(keys)
}

// The input goroutine must not outlive Run. A CLI shares one Keys channel
// across many Run calls, and a leftover goroutine from a finished Run would
// race the next call's own goroutine for every keystroke.
func TestRunsInputGoroutineStopsWhenRunReturns(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, _, enc, req := acceptCall(t, conns)
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close() // the server drops the connection; Run did not ask for this

	waitRun(t, resultCh)

	select {
	case keys <- 'x':
		t.Fatal("a goroutine from the finished Run still consumes the shared Keys channel")
	case <-time.After(200 * time.Millisecond):
	}
	close(keys)
}

// A terminal delivers a non-ASCII character as more than one byte, one read
// at a time. Sending each byte alone as its own pane.send_text mangles it:
// encoding/json replaces a lone invalid byte with the replacement rune. Run
// must hold the bytes until they form a whole rune.
func TestRunAssemblesUTF8BeforeSendingText(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- 0xC3
	keys <- 0xA9

	sendReq := readRequestFrom(t, dec)
	if sendReq.Cmd != "pane.send_text" {
		t.Fatalf("got cmd %q, want pane.send_text", sendReq.Cmd)
	}
	if text, _ := sendReq.Str("text"); text != "é" {
		t.Fatalf("pane.send_text carried %q, want %q", text, "é")
	}

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, dec)
	waitRun(t, resultCh)
	close(keys)
}

// A lone byte that can never start a valid UTF-8 sequence is not a
// keystroke. Run drops it rather than sending it, and the next plain byte
// sends normally.
func TestRunDropsALoneInvalidUTF8ByteAndSendsTheNextPlainByte(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- 0x80
	keys <- 'x'

	sendReq := readRequestFrom(t, dec)
	if sendReq.Cmd != "pane.send_text" {
		t.Fatalf("got cmd %q, want pane.send_text", sendReq.Cmd)
	}
	if text, _ := sendReq.Str("text"); text != "x" {
		t.Fatalf("pane.send_text carried %q, want %q", text, "x")
	}

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, dec)
	waitRun(t, resultCh)
	close(keys)
}

// A blocked ask can go stale on the clock alone, with no new event ever
// arriving to say so. Run must repaint the status line on its own once the
// ask's deadline passes.
func TestRunExpiresABlockedAskWithoutANewFrame(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 40, Rows: 6, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	this := "w1:p1"
	sendState(t, enc, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: "s1", Harness: "claude-code", Pane: &this,
		State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm -rf build/", Deadline: nowSeconds() + 0.3},
	})

	waitForContains(t, out.String, "blocked", 2*time.Second)
	waitForContains(t, out.String, "working", 3*time.Second)

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, dec)
	waitRun(t, resultCh)
	close(keys)
}

// Run owns the split between the terminal and the pane: it reserves the
// last row for the status line, so it must ask the server to attach with
// one fewer row than the terminal actually has.
func TestRunAttachesWithOneFewerRowThanTheTerminal(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 2)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 80, Rows: 24, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if rows, _ := req.Int("rows"); rows != 23 {
		t.Fatalf("pane.attach asked for %d rows, want 23 (the terminal height minus the status line)", rows)
	}
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, dec)
	waitRun(t, resultCh)
	close(keys)
}

// Run fetches this pane's label once at attach time and shows it on the
// status line, the same pane.list reply neighbour already parses.
func TestRunFetchesAndShowsThePaneLabel(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 2)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 40, Rows: 5, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	// Answer the label fetch by hand rather than through acceptCall: this
	// test needs to wait until the client has actually read the reply
	// before it sends a frame, so the label is certainly applied by then.
	// Run makes exactly two connections at startup, the attach and the
	// label fetch, in either order, so this reads exactly two.
	var attachConn net.Conn
	var adec *proto.Decoder
	var aenc *proto.Encoder
	var attachReq proto.Request
	for i := 0; i < 2; i++ {
		c := acceptConn(t, conns)
		dec := proto.NewDecoder(c)
		enc := proto.NewEncoder(c)
		req := readRequestFrom(t, dec)
		if req.ID == "lbl" {
			if err := enc.Send(proto.OKResp(req.ID, map[string]any{"panes": []map[string]any{
				{"id": "w1:p1", "label": "auth fix"},
			}})); err != nil {
				t.Fatal(err)
			}
			var buf [1]byte
			_, _ = c.Read(buf[:]) // blocks until the client reads the reply and closes
			_ = c.Close()
			continue
		}
		attachConn, adec, aenc, attachReq = c, dec, enc, req
	}
	defer attachConn.Close()
	if err := aenc.Send(proto.OKResp(attachReq.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}
	// Closing the label connection only proves fetchLabel has returned, not
	// that the goroutine holding label has already stored it: that last
	// step still races this test. Resending the frame until "auth fix"
	// shows up, rather than checking once, is what makes the wait correct
	// regardless of that scheduling gap.
	frame := proto.Frame{
		Event: "frame", Pane: "w1:p1", Cols: 40, Rows: 4, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "h"}, {Text: "i"}}},
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		frame.Seq++
		if err := aenc.Send(frame); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "auth fix") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the label to appear: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, adec)
	waitRun(t, resultCh)
	close(keys)
}

// Only a failed pane.attach ends the session. A later command's error reply,
// a keystroke sent to a pane that closed underneath it, say, is shown and
// Run keeps running.
func TestRunShowsANonFatalErrorReplyAndContinues(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 80, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- 'x'

	sendReq := readRequestFrom(t, dec)
	if sendReq.Cmd != "pane.send_text" {
		t.Fatalf("got cmd %q, want pane.send_text", sendReq.Cmd)
	}
	if err := enc.Send(proto.ErrResp(sendReq.ID, proto.ErrPaneClosed, "this pane is closed")); err != nil {
		t.Fatal(err)
	}

	waitForContains(t, out.String, "pane_closed: this pane is closed", 2*time.Second)

	keys <- Leader
	keys <- 'd'
	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach; a non-fatal error reply must not have ended the session",
			detachReq.Cmd)
	}
	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
	}
	close(keys)
}

// A dial failure is the same class ErrServerGone already
// names - the server never acknowledges the attach. Folding it in lets a
// caller tell "no server is here" apart from a user error with one check,
// errors.Is(err, ErrServerGone), instead of a string match on the message.
func TestRunReturnsErrServerGoneWhenNothingIsListening(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "server.sock")
	_, err := Run(Options{
		Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: &syncBuf{},
		Cols: 10, Rows: 3, Keys: make(chan byte),
	})
	if !errors.Is(err, ErrServerGone) {
		t.Fatalf("Run returned %v, want ErrServerGone", err)
	}
}

func TestRunReturnsErrServerGoneWhenTheConnectionDropsBeforeTheAck(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 1)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, _, _, _ := acceptCall(t, conns)
	_ = conn.Close() // never reply to pane.attach

	r := waitRun(t, resultCh)
	if !errors.Is(r.err, ErrServerGone) {
		t.Fatalf("Run returned %v, want ErrServerGone", r.err)
	}
	close(keys)
}

func TestRunReturnsErrServerGoneWhenTheConnectionDropsMidSession(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 1)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, _, enc, req := acceptCall(t, conns)
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}
	if err := enc.Send(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 10, Rows: 2, Cursor: [2]int{0, 0},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "h"}, {Text: "i"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForContains(t, out.String, "hi", 2*time.Second)
	_ = conn.Close() // the server drops mid-session; Run never asked to detach

	r := waitRun(t, resultCh)
	if !errors.Is(r.err, ErrServerGone) {
		t.Fatalf("Run returned %v, want ErrServerGone", r.err)
	}
	close(keys)
}

// With Keys set, In must never be read, even when it holds bytes that look
// like a command.
func TestRunNeverReadsInWhenKeysIsSet(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader("\x01x"), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- Leader
	keys <- 'd'

	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach; In's own leader+x would send pane.close first if Run had read it",
			detachReq.Cmd)
	}
	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
	}
	close(keys)
}

// When there is no more keyboard to read from, staying attached would leave
// a live session rendering frames nobody can ever answer. Run detaches on
// its own and says why.
func TestRunDetachesAndReportsWhenKeysCloses(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 1)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	close(keys)

	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
	}

	waitForContains(t, out.String, "stdin closed. Detached.", 2*time.Second)

	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
	}
}

// numGoroutinesSettled samples the goroutine count after letting the runtime
// finish scheduling work already in flight, so a genuinely finished
// goroutine has time to actually exit before this reads the count.
func numGoroutinesSettled() int {
	runtime.Gosched()
	runtime.GC()
	return runtime.NumGoroutine()
}

// Without the decoder goroutine's send also selecting on done, a fatal
// attach reply followed immediately by a second line drives it into a
// permanent block: the fatal reply fills the one-slot lines buffer, the
// second line's send has to wait for the main loop to drain that slot, and
// once the main loop returns on the fatal reply, the goroutine's own next
// read error (from the connection Run's own teardown closes) has no slot
// left and no reader left either. This polls the process goroutine count
// rather than a channel, because the decoder goroutine and lines are both
// unexported and local to Run; a permanent leak shows up as a count that
// never comes back down within the wait.
func TestDecoderGoroutineExitsAfterAFatalReplyWithASecondLineQueued(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 1)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)

	before := numGoroutinesSettled()

	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:pX", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, _, _, req := acceptCall(t, conns)
	defer conn.Close()
	// Both lines go out in one conn.Write, not two separate enc.Send calls:
	// Run's own connection closes as soon as its main loop returns on the
	// fatal reply, and a second Write arriving after that close sees a
	// broken pipe on this end. Queuing both lines in the kernel's socket
	// buffer before Run can even read the first one is what lets this test
	// reach the second line at all.
	errLine, err := json.Marshal(proto.ErrResp(req.ID, proto.ErrNoSuchPane,
		`no pane "w1:pX". Run: coppice pane list`))
	if err != nil {
		t.Fatal(err)
	}
	frameLine, err := json.Marshal(proto.Frame{
		Event: "frame", Pane: "w1:pX", Seq: 1, Cols: 1, Rows: 1, Cursor: [2]int{0, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	both := append(append(errLine, '\n'), append(frameLine, '\n')...)
	if _, err := conn.Write(both); err != nil {
		t.Fatal(err)
	}

	r := waitRun(t, resultCh)
	if r.err == nil {
		t.Fatal("Run returned no error for a failed attach")
	}
	close(keys)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if now := numGoroutinesSettled(); now <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count is still %d, %d seconds after Run returned, started at %d; a decoder goroutine looks blocked forever",
				numGoroutinesSettled(), 2, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A malformed ok:false reply that carries no error object must not be read
// as an acknowledgement: only a genuine ok:true for id "attach" may clear
// the ack deadline. attachAckTimeout is a var, not a const, so this test can
// shrink it rather than wait out the real five seconds.
func TestRunEndsInErrServerGoneWhenTheAttachAckIsMalformed(t *testing.T) {
	old := attachAckTimeout
	attachAckTimeout = 100 * time.Millisecond
	defer func() { attachAckTimeout = old }()

	sock, conns := listenUnix(t)
	keys := make(chan byte, 1)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 10, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, _, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(map[string]any{"id": req.ID, "ok": false}); err != nil {
		t.Fatal(err)
	}

	r := waitRun(t, resultCh)
	if !errors.Is(r.err, ErrServerGone) {
		t.Fatalf("Run returned %v, want ErrServerGone", r.err)
	}
	close(keys)
}

// Before the join-defer is registered right where the input goroutine
// starts, an early return between creating done/stopped and starting that
// goroutine, the very first pane.attach send failing, say, would wait on
// stopped forever: nothing would ever exist to close it. A fake server that
// abortively resets every connection it accepts, via SO_LINGER 0, makes the
// client's very first write fail deterministically, which is what exercises
// that exact return path.
func TestRunReturnsPromptlyWhenTheInitialAttachSendFails(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// The accepted connection is aborted with SO_LINGER 0, every time. This
	// alone was already enough to end the connection promptly, one way or
	// another: either the abortive close wins its race against the
	// client's first write, and enc.Send itself fails, or the write wins
	// and the close lands a moment later, and the client's very next read
	// fails instead. Either path is an error. What actually made this test
	// flaky was a second, independent race: see the comment on Keys below.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			uc := c.(*net.UnixConn)
			if raw, err := uc.SyscallConn(); err == nil {
				_ = raw.Control(func(fd uintptr) {
					_ = syscall.SetsockoptLinger(int(fd), syscall.SOL_SOCKET, syscall.SO_LINGER,
						&syscall.Linger{Onoff: 1, Linger: 0})
				})
			}
			_ = uc.Close()
		}
	}()

	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		// Keys, an unbuffered channel that is never closed, not In: the
		// second half of the old flake was that In: strings.NewReader("")
		// returns EOF almost at once, which fires Run's own stdin-closed
		// detach path (attach.go's endStdinClosed, weClosed) as an
		// independent race against the send failure this test means to
		// observe. weClosed true made a later read error look like an
		// ordinary detach, returning a nil error. A Keys channel that never
		// fires and never closes cannot start that race, and its reading
		// goroutine always selects on Run's own done channel too, so it
		// still exits the moment Run returns.
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Keys: make(chan byte),
			Out: out, Cols: 10, Rows: 3,
		})
		resultCh <- runOutcome{n, err}
	}()

	r := waitRun(t, resultCh)
	if r.err == nil {
		t.Fatal("Run returned no error when the initial attach send failed")
	}
}

// neighbour's own dial has no timeout of its own to lean on, so a peer that
// accepts and never answers must not be able to hold up ctrl+a n forever:
// the deadline on that secondary connection is what ends the wait. Run must
// still print the no-neighbour message and detach cleanly afterward.
func TestRunActNextEndsAfterATimeoutWhenNeighbourNeverAnswers(t *testing.T) {
	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 80, Rows: 3, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	attachConn, adec, aenc, req := acceptCall(t, conns)
	defer attachConn.Close()
	if err := aenc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}

	keys <- Leader
	keys <- 'n'

	// Accept the pane.list connection and never answer it at all.
	listConn, _, _, _ := acceptCall(t, conns)
	defer listConn.Close()

	waitForContains(t, out.String, "There is no other open pane.", 5*time.Second)

	keys <- Leader
	keys <- 'd'
	_ = readRequestFrom(t, adec)
	r := waitRun(t, resultCh)
	if r.err != nil || r.next != (Next{}) {
		t.Fatalf("Run returned %+v %v after a silent neighbour lookup, want an empty Next and no error",
			r.next, r.err)
	}
	close(keys)
}

// The initial pane.attach send now carries the same teardownDeadline every
// later send on this connection does (sendMain). This is that deadline's
// RED: a fake server that accepts and never reads, paired with a pane id
// padded well past any configured unix socket buffer, is what actually
// makes the send block instead of just buffering - the same technique
// internal/server's own TestATeardownDoesNotWaitForAHandlerStuckWritingToADeadPeer
// uses to prove the equivalent bound on the server's own side.
func TestRunRespectsTheWriteDeadlineOnTheInitialAttachSend(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	before := numGoroutinesSettled()

	accepted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Never read, and held open only until the test itself says so.
		// Closing early would let the write fail as a plain reset instead
		// of actually blocking on a full buffer. Waiting on done, not a
		// fixed sleep, so this goroutine exits the moment the test is
		// finished with it instead of outliving the test's own assertions.
		<-done
		_ = c.Close()
	}()

	huge := strings.Repeat("x", 8<<20) // comfortably over any configured unix socket buffer

	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: huge, In: strings.NewReader(""), Keys: make(chan byte),
			Out: out, Cols: 10, Rows: 3,
		})
		resultCh <- runOutcome{n, err}
	}()

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		close(done)
		t.Fatal("the fake server never accepted the connection")
	}

	start := time.Now()
	r := waitRun(t, resultCh)
	elapsed := time.Since(start)
	close(done)
	if r.err == nil {
		t.Fatal("Run returned no error for a write that should have blocked past its own deadline")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Run took %v to return once the write started blocking, want it bounded near teardownDeadline", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if now := numGoroutinesSettled(); now <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count is still %d, 2s after the fake server was told to close, started at %d; it should exit at once, not outlive a fixed sleep",
				numGoroutinesSettled(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The label fetch used to keep its own connection open up to its full
// labelFetchTimeout (500ms) even after Run itself had already returned: the
// select in Run that gave up waiting for it never actually stopped it. This
// proves the fix: once Run returns, on a clean detach that has nothing to
// do with the label fetch, that connection closes at once instead of up to
// half a second later.
func TestLabelFetchDoesNotOutliveRun(t *testing.T) {
	sock, conns := listenUnix(t)

	keys := make(chan byte)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Keys: keys,
			Out: out, Cols: 10, Rows: 3,
		})
		resultCh <- runOutcome{n, err}
	}()

	var attachConn, labelConn net.Conn
	for attachConn == nil || labelConn == nil {
		c := acceptConn(t, conns)
		dec := proto.NewDecoder(c)
		req := readRequestFrom(t, dec)
		switch req.ID {
		case "attach":
			attachConn = c
			enc := proto.NewEncoder(c)
			if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
				t.Fatal(err)
			}
		case "lbl":
			// Deliberately never answered: this connection must close the
			// moment Run returns, not up to labelFetchTimeout later.
			labelConn = c
		}
	}
	defer attachConn.Close()

	// ctrl+a d: detach cleanly, so Run returns on its own, for a reason
	// that has nothing to do with the label fetch.
	keys <- Leader
	keys <- 'd'

	r := waitRun(t, resultCh)
	returned := time.Now()
	if r.err != nil {
		t.Fatalf("Run returned an error on a clean detach: %v", r.err)
	}

	// A generous deadline: this measures how long the read actually took,
	// not whether it happened at all.
	_ = labelConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err := labelConn.Read(buf)
	closedAt := time.Now()
	if err == nil {
		t.Fatal("the label fetch connection is still open (a real byte arrived) after Run returned")
	}
	if elapsed := closedAt.Sub(returned); elapsed > 300*time.Millisecond {
		t.Fatalf("the label fetch connection took %v to close after Run returned, "+
			"want well under labelFetchTimeout (500ms)", elapsed)
	}
}

// TestHelpNeverPaintsOnTheStatusRow pins the fix for a person's real ctrl+a ?
// run: the help block must never touch the status row and must never scroll
// the terminal. The precondition that produced the defect is a pane whose
// shell has already filled the screen, so the cursor sits on the pane's last
// row; the frame below parks the cursor there on purpose.
func TestHelpNeverPaintsOnTheStatusRow(t *testing.T) {
	helpRow := regexp.MustCompile(`\x1b\[(\d+);1H`)

	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 50, Rows: 8, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}
	if err := enc.Send(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 50, Rows: 7, Cursor: [2]int{0, 6},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "h"}, {Text: "i"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForContains(t, out.String, "hi", 2*time.Second)

	out.mu.Lock()
	out.b.Reset()
	out.mu.Unlock()

	keys <- Leader
	keys <- '?'

	waitForContains(t, out.String, "ctrl+a ctrl+a", 2*time.Second)
	help := out.String()

	if strings.ContainsAny(help, "\r\n") {
		t.Fatalf("help write contains a bare newline, want cursor positioning only: %q", help)
	}
	rows := helpRow.FindAllStringSubmatch(help, -1)
	if len(rows) != 7 {
		t.Fatalf("help write positioned %d rows, want 7 (one per HelpText line): %q", len(rows), help)
	}
	for _, m := range rows {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("help row %q is not a number", m[1])
		}
		if n < 1 || n > 7 {
			t.Fatalf("help wrote row %d, want 1-7: the status row is 8 and must stay untouched: %q", n, help)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(HelpText, "\r\n"), "\r\n") {
		if !strings.Contains(help, line) {
			t.Fatalf("help write is missing line %q: %q", line, help)
		}
	}

	out.mu.Lock()
	out.b.Reset()
	out.mu.Unlock()
	if err := enc.Send(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 2, Cols: 50, Rows: 7, Cursor: [2]int{0, 6},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "h"}, {Text: "i"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForContains(t, out.String, "hi", 2*time.Second)
	if !strings.HasPrefix(out.String(), "\x1b[2J") {
		t.Fatalf("the frame after help did not start with a full clear, want the help block "+
			"cleared rather than painted over: %q", out.String())
	}

	keys <- Leader
	keys <- 'd'
	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
	}
	r := waitRun(t, resultCh)
	if r.err != nil {
		t.Fatalf("Run returned an error: %v", r.err)
	}
	close(keys)
}

// TestHelpClampsToTheLastLinesOnAShortTerminal pins writeHelp's row clamp.
// TestHelpNeverPaintsOnTheStatusRow alone cannot reach this branch: with
// Rows: 8, screen.rows is exactly 7, the same as len(HelpText's lines), so
// writeHelp's len(lines) > s.rows check is never true there. Rows: 4 leaves
// screen.rows at 3, one row short of the whole block, so the terminal must
// show only the tail of it, never a row above 1 and never a row at or below
// the status row.
func TestHelpClampsToTheLastLinesOnAShortTerminal(t *testing.T) {
	helpRow := regexp.MustCompile(`\x1b\[(\d+);1H`)

	sock, conns := listenUnix(t)
	keys := make(chan byte, 4)
	out := &syncBuf{}
	resultCh := make(chan runOutcome, 1)
	go func() {
		n, err := Run(Options{
			Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
			Cols: 50, Rows: 4, Keys: keys,
		})
		resultCh <- runOutcome{n, err}
	}()

	conn, dec, enc, req := acceptCall(t, conns)
	defer conn.Close()
	if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
		t.Fatal(err)
	}
	// screen.rows is 3 here (Rows-1). Park the cursor on the pane's last
	// row, the same precondition TestHelpNeverPaintsOnTheStatusRow uses.
	if err := enc.Send(proto.Frame{
		Event: "frame", Pane: "w1:p1", Seq: 1, Cols: 50, Rows: 3, Cursor: [2]int{0, 2},
		RowsChanged: map[int][]proto.Cell{0: {{Text: "h"}, {Text: "i"}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForContains(t, out.String, "hi", 2*time.Second)

	out.mu.Lock()
	out.b.Reset()
	out.mu.Unlock()

	keys <- Leader
	keys <- '?'

	waitForContains(t, out.String, "ctrl+a ctrl+a", 2*time.Second)
	help := out.String()

	if strings.ContainsAny(help, "\r\n") {
		t.Fatalf("help write contains a bare newline, want cursor positioning only: %q", help)
	}
	rows := helpRow.FindAllStringSubmatch(help, -1)
	if len(rows) != 3 {
		t.Fatalf("help write positioned %d rows, want 3 (screen.rows, not HelpText's 7 lines): %q",
			len(rows), help)
	}
	for _, m := range rows {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("help row %q is not a number", m[1])
		}
		if n < 1 || n > 3 {
			t.Fatalf("help wrote row %d, want 1-3: the status row is 4 and must stay untouched: %q",
				n, help)
		}
	}
	// The clamp keeps the last three lines, not the first three.
	for _, line := range []string{
		"ctrl+a x  close this pane",
		"ctrl+a ?  this list",
		"ctrl+a ctrl+a  send one ctrl+a to the pane",
	} {
		if !strings.Contains(help, line) {
			t.Fatalf("help write is missing tail line %q: %q", line, help)
		}
	}
	if strings.Contains(help, "next pane") {
		t.Fatalf("help write kept a line the short-terminal clamp should have dropped: %q", help)
	}

	keys <- Leader
	keys <- 'd'
	detachReq := readRequestFrom(t, dec)
	if detachReq.Cmd != "pane.detach" {
		t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
	}
	r2 := waitRun(t, resultCh)
	if r2.err != nil {
		t.Fatalf("Run returned an error: %v", r2.err)
	}
	close(keys)
}

// TestSiblingMessagesNeverWriteABareNewline pins the three writes named
// alongside the help block: a leader key with no other open pane to go to,
// a non-fatal error reply, and the message endStdinClosed sends on its way
// out. Each one is positioned by absolute cursor placement, the same as
// writeHelp, and carries no bare newline of its own to scroll the terminal.
func TestSiblingMessagesNeverWriteABareNewline(t *testing.T) {
	t.Run("no other open pane", func(t *testing.T) {
		sock, conns := listenUnix(t)
		keys := make(chan byte, 4)
		out := &syncBuf{}
		resultCh := make(chan runOutcome, 1)
		go func() {
			n, err := Run(Options{
				Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
				Cols: 80, Rows: 3, Keys: keys,
			})
			resultCh <- runOutcome{n, err}
		}()

		attachConn, adec, aenc, req := acceptCall(t, conns)
		defer attachConn.Close()
		if err := aenc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
			t.Fatal(err)
		}

		keys <- Leader
		keys <- 'n'

		listConn, _, lenc, lreq := acceptCall(t, conns)
		defer listConn.Close()
		if err := lenc.Send(proto.OKResp(lreq.ID, map[string]any{"panes": []map[string]any{
			{"id": "w1:p1", "closed": false},
		}})); err != nil {
			t.Fatal(err)
		}

		waitForContains(t, out.String, "There is no other open pane.", 2*time.Second)
		msg := out.String()
		if strings.ContainsAny(msg, "\r\n") {
			t.Fatalf("no-neighbour write contains a bare newline, want cursor positioning only: %q", msg)
		}

		keys <- Leader
		keys <- 'd'
		_ = readRequestFrom(t, adec)
		r := waitRun(t, resultCh)
		if r.err != nil || r.next != (Next{}) {
			t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
		}
		close(keys)
	})

	t.Run("non-fatal error reply", func(t *testing.T) {
		sock, conns := listenUnix(t)
		keys := make(chan byte, 4)
		out := &syncBuf{}
		resultCh := make(chan runOutcome, 1)
		go func() {
			n, err := Run(Options{
				Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
				Cols: 80, Rows: 3, Keys: keys,
			})
			resultCh <- runOutcome{n, err}
		}()

		conn, dec, enc, req := acceptCall(t, conns)
		defer conn.Close()
		if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
			t.Fatal(err)
		}

		keys <- 'x'

		sendReq := readRequestFrom(t, dec)
		if sendReq.Cmd != "pane.send_text" {
			t.Fatalf("got cmd %q, want pane.send_text", sendReq.Cmd)
		}
		if err := enc.Send(proto.ErrResp(sendReq.ID, proto.ErrPaneClosed, "this pane is closed")); err != nil {
			t.Fatal(err)
		}

		waitForContains(t, out.String, "pane_closed: this pane is closed", 2*time.Second)
		msg := out.String()
		if strings.ContainsAny(msg, "\r\n") {
			t.Fatalf("error reply write contains a bare newline, want cursor positioning only: %q", msg)
		}

		keys <- Leader
		keys <- 'd'
		_ = readRequestFrom(t, dec)
		r := waitRun(t, resultCh)
		if r.err != nil || r.next != (Next{}) {
			t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
		}
		close(keys)
	})

	t.Run("stdin closed", func(t *testing.T) {
		sock, conns := listenUnix(t)
		keys := make(chan byte, 1)
		out := &syncBuf{}
		resultCh := make(chan runOutcome, 1)
		go func() {
			n, err := Run(Options{
				Socket: sock, Pane: "w1:p1", In: strings.NewReader(""), Out: out,
				Cols: 80, Rows: 3, Keys: keys,
			})
			resultCh <- runOutcome{n, err}
		}()

		conn, dec, enc, req := acceptCall(t, conns)
		defer conn.Close()
		if err := enc.Send(proto.OKResp(req.ID, map[string]any{"pane": "w1:p1", "attached": true})); err != nil {
			t.Fatal(err)
		}

		close(keys)

		detachReq := readRequestFrom(t, dec)
		if detachReq.Cmd != "pane.detach" {
			t.Fatalf("got cmd %q, want pane.detach", detachReq.Cmd)
		}

		waitForContains(t, out.String, "stdin closed. Detached.", 2*time.Second)
		msg := out.String()
		if strings.ContainsAny(msg, "\r\n") {
			t.Fatalf("stdin-closed write contains a bare newline, want cursor positioning only: %q", msg)
		}

		r := waitRun(t, resultCh)
		if r.err != nil || r.next != (Next{}) {
			t.Fatalf("Run returned %+v %v, want an empty Next and no error", r.next, r.err)
		}
	})
}
