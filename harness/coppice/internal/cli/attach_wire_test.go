package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/opendaisugi/coppice/internal/proto"
)

// fakeAttachServer is a bare unix listener, no server.Server, that records
// every pane.attach and pane.resize request it sees and acknowledges just
// enough to keep attach.Run's own handshake happy. It exists to answer one
// question: for one terminal height, do the
// pane.attach request and the later pane.resize request actually agree on
// rows, on the wire, both derived from the SAME h?
type fakeAttachServer struct {
	ln net.Listener

	mu         sync.Mutex
	conns      []net.Conn
	attachConn net.Conn
	resizeConn net.Conn
	attachCh   chan map[string]any
	resizeCh   chan map[string]any
	// resizeHold, when non-nil, is read before a pane.resize reply is sent.
	// A test that never closes it holds the reply open indefinitely, the
	// same as a wedged handler on the real server. nil, the default, means
	// reply at once, as every other test using this fake expects.
	resizeHold <-chan struct{}
}

func newFakeAttachServer(t *testing.T, sock string) *fakeAttachServer {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAttachServer{
		ln:       ln,
		attachCh: make(chan map[string]any, 1),
		resizeCh: make(chan map[string]any, 1),
	}
	go f.acceptLoop()
	return f
}

func (f *fakeAttachServer) acceptLoop() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		go f.handle(conn)
	}
}

func (f *fakeAttachServer) handle(conn net.Conn) {
	dec := proto.NewDecoder(conn)
	enc := proto.NewEncoder(conn)
	for {
		line, err := dec.Next()
		if err != nil {
			return
		}
		var req map[string]any
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		id := req["id"]
		switch req["cmd"] {
		case "pane.attach":
			f.mu.Lock()
			f.attachConn = conn
			f.mu.Unlock()
			select {
			case f.attachCh <- req:
			default:
			}
			_ = enc.Send(map[string]any{
				"id": id, "ok": true,
				"result": map[string]any{"pane": req["pane"], "attached": true},
			})
		case "pane.resize":
			f.mu.Lock()
			f.resizeConn = conn
			hold := f.resizeHold
			f.mu.Unlock()
			select {
			case f.resizeCh <- req:
			default:
			}
			if hold != nil {
				<-hold
			}
			_ = enc.Send(map[string]any{
				"id": id, "ok": true,
				"result": map[string]any{"cols": req["cols"], "rows": req["rows"]},
			})
		default:
			// pane.list, the label fetch, or anything else: an empty ok
			// reply is enough that no caller hangs waiting on it.
			_ = enc.Send(map[string]any{"id": id, "ok": true, "result": map[string]any{}})
		}
	}
}

// closeAll stops accepting and closes every connection seen so far, which is
// what makes attach.Run's own read loop see EOF and return ErrServerGone -
// the clean way to end a test that never sends a detach keystroke.
func (f *fakeAttachServer) closeAll() {
	_ = f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
}

// closeAttachConn closes only the connection that carried pane.attach,
// leaving any other connection - a resize call in flight, in particular -
// untouched. It exists so a test can make attach.Run return on its own
// while a separate resize call stays genuinely open on the wire.
func (f *fakeAttachServer) closeAttachConn(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	conn := f.attachConn
	f.mu.Unlock()
	if conn == nil {
		t.Fatal("closeAttachConn called before pane.attach ever arrived")
	}
	_ = conn.Close()
}

// For one terminal height h, the pane.attach request and the later
// pane.resize request - sent after a real SIGWINCH - must both carry rows
// h-1, matching attach.Run's own paneRows := o.Rows - 1. This drives the
// actual wire values instead of a two-line helper,
// and it proves the resize reaches the server over a dial that cannot
// autostart, with COPPICE_NO_AUTOSTART deliberately left unset, so nothing
// hides a wrongly-wired Dial behind a disabled autostart.
func TestAttachAndResizeAgreeOnRowsFromOneTerminalHeight(t *testing.T) {
	// Deliberately unset, not "1": if watchResize ever regresses to Dial
	// instead of dialExisting, autostart is free to fire, and the
	// no-artifacts check below would actually catch it.
	t.Setenv("COPPICE_NO_AUTOSTART", "")

	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	srv := newFakeAttachServer(t, sock)
	t.Cleanup(srv.closeAll)

	const h = 24
	ptmx, ttyf, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	// Safe to close as soon as c.Run below returns: runAttach now joins
	// watchResize (close(resizeDone), then <-resizeStopped) before it does,
	// so no goroutine can still be calling termSize(ttyf) - reading
	// ttyf.Fd() - after that point. Before that join existed, a SIGWINCH
	// already mid-handling when Run returned could still be touching ttyf a
	// moment after this test function would otherwise return, and closing
	// it here raced that read under -race, reproducibly.
	defer ptmx.Close()
	defer ttyf.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: h, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	defer inW.Close()

	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir, In: inR, Out: ttyf, Err: io.Discard})
	done := make(chan int, 1)
	go func() { done <- c.Run([]string{"attach", "w1:p1"}) }()

	var attachReq map[string]any
	select {
	case attachReq = <-srv.attachCh:
	case <-time.After(3 * time.Second):
		t.Fatal("pane.attach never arrived")
	}
	if got := intField(attachReq["rows"]); got != h-1 {
		t.Fatalf("pane.attach rows = %v, want %d (h=%d)", attachReq["rows"], h-1, h)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}

	var resizeReq map[string]any
	select {
	case resizeReq = <-srv.resizeCh:
	case <-time.After(3 * time.Second):
		t.Fatal("pane.resize never arrived after SIGWINCH")
	}
	if got := intField(resizeReq["rows"]); got != h-1 {
		t.Fatalf("pane.resize rows = %v, want %d (h=%d), the SAME h pane.attach used",
			resizeReq["rows"], h-1, h)
	}

	// The resize that just arrived cannot have autostarted anything - no
	// server.log, no layout.json, no lock file appears in DataDir, even
	// though autostart is nominally enabled: COPPICE_NO_AUTOSTART is unset
	// above. This alone does not discriminate dialExisting from Dial: the
	// fake listener is still reachable, so either would have connected on
	// the first try and never reached an autostart fallback at all.
	assertNoAutostartArtifacts(t, dir)

	// The discriminating half: stop the listener from
	// accepting new connections - net.UnixListener.Close unlinks the socket
	// file, and the already-established attach connection is a separate fd,
	// untouched - then send a second SIGWINCH. If watchResize ever
	// regresses to Dial, the now-unreachable socket sends it down the
	// autostart fallback, which creates server.log in DataDir before the
	// spawn itself even runs. The spawn would fail regardless, since
	// os.Executable() here is this test binary, not a real coppice.
	// dialExisting has no such fallback to regress into.
	_ = srv.ln.Close()
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		assertNoAutostartArtifacts(t, dir)
		time.Sleep(20 * time.Millisecond)
	}

	srv.closeAll()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runAttach did not return after the fake server closed")
	}
}

// The next.Create branch, reached only when a live attach session asks for
// a new pane, dials with dialExisting, never c.dial: attach never
// autostarts, with or without a pane id, and asking for a new pane from
// inside one is not a second way in for the same autostart the picker
// branch and server stop already refuse. COPPICE_NO_AUTOSTART is
// deliberately left unset, the same discriminating shape the other
// autostart tests in this file use: only the listener, not the already
// established attach connection, is closed, so ctrl+a c reaches attach.Run
// and returns Next{Create: true} while the create branch's own dial meets
// an unreachable server.
func TestAttachCreateBranchDoesNotAutostart(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "")

	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	srv := newFakeAttachServer(t, sock)
	t.Cleanup(srv.closeAll)

	ptmx, ttyf, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer ttyf.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	defer inW.Close()

	var errb bytes.Buffer
	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir, In: inR, Out: ttyf, Err: &errb})
	done := make(chan int, 1)
	go func() { done <- c.Run([]string{"attach", "w1:p1"}) }()

	select {
	case <-srv.attachCh:
	case <-time.After(3 * time.Second):
		t.Fatal("pane.attach never arrived")
	}

	// Refuses any new connection from here on, while the established
	// attach connection stays open: attach.Run keeps streaming, and only
	// the create branch's own dial, below, meets an unreachable server.
	_ = srv.ln.Close()

	if _, err := inW.Write([]byte{1, 'c'}); err != nil { // ctrl+a, c: ActCreate
		t.Fatal(err)
	}

	select {
	case code := <-done:
		if code != 3 {
			t.Fatalf("runAttach exited %d, want 3: the server is unreachable", code)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("runAttach never returned after ctrl+a c with the server gone")
	}

	// Pins the path this test claims to exercise: the create branch's own
	// bare "cannot reach the server at" message, not attach.Run's own
	// ErrServerGone, which prefixes "the server went away:". Both paths
	// exit 3 and leave no autostart artifacts, so without this the test
	// could pass unchanged even if the create branch stopped being reached
	// at all.
	if got := errb.String(); !strings.Contains(got, "cannot reach the server at") {
		t.Fatalf("output %q does not name the unreachable server", got)
	} else if strings.Contains(got, "the server went away") {
		t.Fatalf("output %q is attach.Run's ErrServerGone message, not the create branch's own dial", got)
	}

	assertNoAutostartArtifacts(t, dir)
}

// pumpBuf copies from r into a Builder a test can poll from another
// goroutine while the copy is still running. ptmx, the pty master, is what
// actually receives the bytes runAttach writes to ttyf, the slave; a test
// watching for a specific escape sequence has to read from here, not ttyf.
type pumpBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (p *pumpBuf) run(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.b.Write(buf[:n])
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *pumpBuf) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.b.String()
}

// runAttach used to join
// resizeStopped, bounded only by however long the in-flight resize call
// took, before writing the alternate-screen exit. A wedged handler on the
// far end could hold a person's shell inside the alternate screen for that
// whole wait. This proves the order directly: the escape sequence must
// reach the terminal well before the resize call - held open here for the
// entire test - ever resolves.
func TestRunAttachExitsTheAlternateScreenBeforeWaitingOnAnInFlightResize(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")

	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	srv := newFakeAttachServer(t, sock)
	resizeHold := make(chan struct{})
	defer close(resizeHold) // lets the held reply finally go out, so handle can return
	srv.resizeHold = resizeHold
	t.Cleanup(srv.closeAll)

	ptmx, ttyf, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer ttyf.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	pump := &pumpBuf{}
	go pump.run(ptmx)

	inR, inW := io.Pipe()
	defer inW.Close()

	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir, In: inR, Out: ttyf, Err: io.Discard})
	done := make(chan int, 1)
	go func() { done <- c.Run([]string{"attach", "w1:p1"}) }()

	select {
	case <-srv.attachCh:
	case <-time.After(3 * time.Second):
		t.Fatal("pane.attach never arrived")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.resizeCh:
	case <-time.After(3 * time.Second):
		t.Fatal("pane.resize never arrived after SIGWINCH")
	}
	// The resize reply is now held open, on purpose, for the rest of this
	// test: resizeHold is not closed until the deferred close above runs.

	srv.closeAttachConn(t) // makes attach.Run return ErrServerGone at once

	waitForContainsCLI(t, pump.String, "\x1b[?1049l", 500*time.Millisecond)

	select {
	case <-done:
		t.Fatal("runAttach returned before the held resize call's own deadline; " +
			"the join no longer waits for an in-flight call before returning")
	default:
		// Still running, as it must be: the join on the resize goroutine is
		// still bounded by resizeDialDeadline, only the terminal handoff
		// moved ahead of it.
	}

	select {
	case code := <-done:
		if code != 3 {
			t.Fatalf("runAttach exited %d, want 3 (ErrServerGone)", code)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("runAttach never returned; the resize join should still finish on its own deadline")
	}
}

// TestRunAttachClearsTheScreenRightAfterEnteringTheAlternateScreen pins a
// terminal that ignores the alternate-screen request: the full clear and
// cursor home written right after `\x1b[?1049h` put the pane on a clean
// screen either way, since a terminal that honoured the request already
// shows a blank alternate screen and the clear is a no-op on it.
func TestRunAttachClearsTheScreenRightAfterEnteringTheAlternateScreen(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")

	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	srv := newFakeAttachServer(t, sock)
	t.Cleanup(srv.closeAll)

	ptmx, ttyf, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer ttyf.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	pump := &pumpBuf{}
	go pump.run(ptmx)

	inR, inW := io.Pipe()
	defer inW.Close()

	c := onATerminal(&CLI{Version: "test", Socket: sock, DataDir: dir, In: inR, Out: ttyf, Err: io.Discard})
	done := make(chan int, 1)
	go func() { done <- c.Run([]string{"attach", "w1:p1"}) }()

	select {
	case <-srv.attachCh:
	case <-time.After(3 * time.Second):
		t.Fatal("pane.attach never arrived")
	}

	srv.closeAttachConn(t) // makes attach.Run return ErrServerGone at once

	waitForContainsCLI(t, pump.String, "\x1b[?1049l", 500*time.Millisecond)

	select {
	case code := <-done:
		if code != 3 {
			t.Fatalf("runAttach exited %d, want 3 (ErrServerGone)", code)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("runAttach never returned")
	}

	recorded := pump.String()
	const enter = "\x1b[?1049h"
	const clear = "\x1b[2J\x1b[H"
	if !strings.HasPrefix(recorded, enter) {
		t.Fatalf("recorded bytes do not start with the alternate-screen entry: %q", recorded)
	}
	if !strings.HasPrefix(recorded[len(enter):], clear) {
		t.Fatalf("no full clear and cursor home right after the alternate-screen entry, want %q immediately after %q: %q",
			clear, enter, recorded)
	}
}

func waitForContainsCLI(t *testing.T, get func() string, want string, timeout time.Duration) {
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

func assertNoAutostartArtifacts(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"server.log", "layout.json", "server.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("watchResize autostarted a server: %s exists in DataDir", name)
		}
	}
}

func intField(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return -1
	}
}

// runAttach used to close resizeDone and call signal.Stop without
// ever joining watchResize's own goroutine. A SIGWINCH already buffered on
// sig at that moment could still be mid-handling - dialing, or waiting on
// cl.Do's reply - well after the loop had already moved on to another pane,
// or already returned, which could send a stale pane.resize for a pane
// nobody is watching any more. This proves the join itself waits for a
// genuinely in-flight resize call.
//
// Before dialExisting carried its own deadline, a server
// that never replies at all held this join open for doReadDeadline's own
// 150s, and runAttach withheld the alternate-screen exit for that whole
// time too. The fake server here never sends a reply, at all: the join
// must still finish, bounded near resizeDialDeadline, not by the server
// ever answering.
func TestWatchResizeJoinWaitsForAnInFlightResizeToFinish(t *testing.T) {
	// watchResize dials. dialExisting never autostarts on its own, so this
	// is belt and suspenders, matching the rule that every test able to
	// dial sets it.
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	sock := filepath.Join(t.TempDir(), "server.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	reached := make(chan struct{})
	// Closed only at the very end, once the join has already been proven
	// bounded: this is what lets the accept goroutine return and close its
	// connection instead of outliving the test.
	stopServer := make(chan struct{})
	defer close(stopServer)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		dec := proto.NewDecoder(conn)
		if _, err := dec.Next(); err != nil {
			return
		}
		close(reached)
		// Never sends a reply. The join below must not depend on one ever
		// arriving.
		<-stopServer
	}()

	ptmx, ttyf, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer ttyf.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	c := &CLI{Version: "test", Socket: sock, DataDir: t.TempDir(), Out: ttyf, Err: io.Discard}
	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		c.watchResize(sig, done, "w1:p1")
	}()

	sig <- syscall.SIGWINCH
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("watchResize never reached the fake server")
	}

	// The join pattern runAttach now uses, exactly: close done, then wait
	// for the goroutine to have actually returned.
	joinFinished := make(chan struct{})
	start := time.Now()
	go func() {
		close(done)
		<-stopped
		close(joinFinished)
	}()

	select {
	case <-joinFinished:
		t.Fatal("the join finished immediately, before the connection's own deadline could have fired")
	case <-time.After(500 * time.Millisecond):
		// Still blocked, as it must be: no reply has come, and
		// resizeDialDeadline's own 2 seconds has not elapsed yet.
	}

	select {
	case <-joinFinished:
	case <-time.After(4 * time.Second):
		t.Fatal("the join never finished; a resize call with no reply must time out on its own deadline, not block forever")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("join took %v to finish, want it bounded near resizeDialDeadline (2s), not doReadDeadline's 150s", elapsed)
	}
}
