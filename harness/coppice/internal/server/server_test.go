package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir,
		ForemanDir: filepath.Join(dir, "state", "coppice", "foreman"), HookHome: dir})
	if err != nil {
		t.Fatal(err)
	}
	// Restore and Listen both refuse without the start lock, so every test
	// server takes it. Each test has its own data directory, so they never
	// contend.
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// roundTrip drives ServeConn over an in-memory pipe, sending one line and
// reading its response before sending the next, so a sequence like
// pane.create then pane.close is exactly as deterministic as it is for a real
// caller who waits on each reply. A test that wants genuine interleaving (a
// slow request not blocking a fast one behind it) drives ServeConn directly
// instead - see TestASlowWaitDoesNotBlockOtherCommandsOnTheSameConnection.
func roundTrip(t *testing.T, s *Server, lines ...string) []proto.Response {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	go func() {
		s.ServeConn(inR, outW)
		_ = outW.Close()
	}()
	d := proto.NewDecoder(outR)
	var got []proto.Response
	for _, l := range lines {
		if _, err := io.WriteString(inW, l+"\n"); err != nil {
			t.Fatalf("writing request %d: %v", len(got)+1, err)
		}
		line, err := d.Next()
		if err != nil {
			t.Fatalf("reading response %d: %v", len(got), err)
		}
		var r proto.Response
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

func TestSocketPathPrefersXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := SocketPath(); got != "/run/user/1000/coppice/server.sock" {
		t.Fatalf("SocketPath() = %q, want the XDG path", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := SocketPath(); !strings.HasSuffix(got, "/.opendaisugi/coppice/server.sock") {
		t.Fatalf("SocketPath() = %q, want the ~/.opendaisugi fallback", got)
	}
}

func TestListenCreatesAPrivateDirectoryAndSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "nested", "server.sock")
	s, err := New(Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %o, want 700", di.Mode().Perm())
	}
	si, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", si.Mode().Perm())
	}
}

// Removing a leftover socket is only safe while we hold the lock. Without it,
// the check-then-act probe this replaced could unlink a socket another server
// had just bound.
func TestListenRefusesWithoutTheStartLock(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Listen(); err == nil {
		t.Fatal("Listen bound without the start lock, want a refusal")
	}
}

func TestASecondAcquireOfTheStartLockNamesTheHoldersPID(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = b.AcquireStartLock()
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second AcquireStartLock = %v, want ErrAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("error %q does not name the holder's pid", err)
	}
	if !strings.Contains(err.Error(), "coppice server status") {
		t.Fatalf("error %q does not teach the next command", err)
	}
	// Releasing must hand the lock over, or a restart after a clean stop would
	// refuse for ever.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.AcquireStartLock(); err != nil {
		t.Fatalf("the lock was not released by Close: %v", err)
	}
}

func TestUnknownCommandIsBadRequest(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.teleport"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("unknown command gave %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "server.status") {
		t.Fatalf("error message %q does not teach a command that exists", got[0].Error.Message)
	}
}

func TestMalformedLineIsBadRequestAndTheConnectionSurvives(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `not json`, `{"id":"2","cmd":"server.status"}`)
	if got[0].OK || got[0].Error.Code != proto.ErrBadRequest {
		t.Fatalf("malformed line gave %+v, want bad_request", got[0])
	}
	if !got[1].OK {
		t.Fatalf("the connection died after one bad line: %+v", got[1])
	}
}

// DetectionWarnings marshaled as null when no manifest
// ever produced one, so a caller doing `for (const w of status.detection_
// warnings)` would need to special-case the type. RestoreReport.Notes
// already gets the []string{} treatment for the same reason.
func TestDetectionWarningsSerializeAsAnEmptyArrayNotNull(t *testing.T) {
	s := newTestServer(t)
	b, err := json.Marshal(s.DetectionWarnings())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("DetectionWarnings() marshaled as %s, want []", b)
	}
}

func TestServerStatusReportsWhatSurvivesARestart(t *testing.T) {
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"server.status"}`)
	if !got[0].OK {
		t.Fatalf("server.status failed: %+v", got[0])
	}
	b, _ := json.Marshal(got[0].Result)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"pid", "socket", "uptime_s", "panes", "restart_note"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("server.status result is missing %q: %s", k, b)
		}
	}
	note, _ := m["restart_note"].(string)
	if !strings.Contains(note, "layout") || !strings.Contains(note, "not") {
		t.Fatalf("restart_note = %q, want it to say plainly what does not come back", note)
	}
}

// Fail-closed: a connection whose peer uid we cannot read, or whose uid is not
// ours, is dropped. Never served.
func TestOurOwnUIDIsServed(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Our own uid, so this must be served. The refusal is proved by
	// TestRefusesWhenPeerUIDIsUnreadable and TestConnectionFromAnotherUIDIsRefused.
	_, _ = io.WriteString(c, `{"id":"1","cmd":"server.status"}`+"\n")
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	d := proto.NewDecoder(c)
	if _, err := d.Next(); err != nil {
		t.Fatalf("our own uid was refused: %v", err)
	}
}

func TestRefusesWhenPeerUIDIsUnreadable(t *testing.T) {
	s := newTestServer(t)
	s.peerUID = func(*net.UnixConn) (uint32, error) { return 0, errUnreadableUID }
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	d := proto.NewDecoder(c)
	line, err := d.Next()
	if err != nil {
		t.Fatalf("want one unauthorized line then a close, got %v", err)
	}
	var r proto.Response
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Error.Code != proto.ErrUnauthorized {
		t.Fatalf("got %+v, want unauthorized", r)
	}
	if _, err := d.Next(); err == nil {
		t.Fatal("the connection stayed open after unauthorized, want it closed")
	}
}

// The uid mismatch branch is exactly as forceable as the unreadable one: the
// same s.peerUID hook, returning a uid that reads fine but is not ours.
func TestConnectionFromAnotherUIDIsRefused(t *testing.T) {
	s := newTestServer(t)
	s.peerUID = func(*net.UnixConn) (uint32, error) { return uint32(os.Getuid()) + 1, nil }
	var handlerCalls int32
	_ = s.Handle("test.mustnotrun", func(_ *Client, r *proto.Request) proto.Response {
		atomic.AddInt32(&handlerCalls, 1)
		return proto.OKResp(r.ID, nil)
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Send a request before reading anything: a foreign uid must be refused
	// before a single byte of it is read, not merely have its answer withheld.
	_, _ = io.WriteString(c, `{"id":"1","cmd":"test.mustnotrun"}`+"\n")
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	d := proto.NewDecoder(c)
	line, err := d.Next()
	if err != nil {
		t.Fatalf("want one unauthorized line then a close, got %v", err)
	}
	var r proto.Response
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Error.Code != proto.ErrUnauthorized {
		t.Fatalf("got %+v, want unauthorized", r)
	}
	if _, err := d.Next(); err == nil {
		t.Fatal("the connection stayed open after unauthorized, want it closed")
	}
	if got := atomic.LoadInt32(&handlerCalls); got != 0 {
		t.Fatalf("handler ran %d times, want 0: a foreign uid must never reach a handler", got)
	}
}

// The cockpit and the PWA hold ONE long-lived connection and are exactly the
// clients that call agent.wait --until blocked, which blocks for up to two
// minutes inside its handler. Dispatching serially would freeze allow, deny,
// send_text and detach for every other pane for that whole time, while frames
// kept arriving so the UI looked alive and accepted nothing.
func TestASlowWaitDoesNotBlockOtherCommandsOnTheSameConnection(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	_ = s.Handle("test.slow", func(_ *Client, r *proto.Request) proto.Response {
		<-release
		return proto.OKResp(r.ID, map[string]any{"slow": true})
	})

	before := runtime.NumGoroutine()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { s.ServeConn(inR, outW); _ = outW.Close(); close(done) }()

	if _, err := io.WriteString(inW, `{"id":"slow","cmd":"test.slow"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(inW, `{"id":"fast","cmd":"server.status"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	// The fast response must arrive while the slow one is still blocked. One
	// decoder is shared for both reads below: outR is unbuffered, so a second
	// decoder created later could not see bytes the first one already buffered.
	dec := proto.NewDecoder(outR)
	type res struct {
		r   proto.Response
		err error
	}
	got := make(chan res, 1)
	go func() {
		line, err := dec.Next()
		if err != nil {
			got <- res{err: err}
			return
		}
		var r proto.Response
		err = json.Unmarshal(line, &r)
		got <- res{r: r, err: err}
	}()
	select {
	case g := <-got:
		if g.err != nil {
			close(release)
			t.Fatal(g.err)
		}
		if g.r.ID != "fast" {
			close(release)
			t.Fatalf("first response was %q, want fast: the slow handler blocked the connection",
				g.r.ID)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("server.status never answered while a slow handler was running")
	}
	close(release)

	// Drain the slow response too. Before this, the test left it unread: the
	// slow handler's Send then blocked forever on the unbuffered outR, and
	// nothing here ever noticed the leaked handler goroutine, or the wait
	// that this connection's own teardown would otherwise do for it.
	if _, err := dec.Next(); err != nil {
		t.Fatalf("reading the slow response: %v", err)
	}
	_ = inW.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeConn never returned after the connection's input closed")
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count did not return to baseline: before=%d now=%d",
				before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentClientsAreServedIndependently(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			c, err := net.Dial("unix", s.cfg.SocketPath)
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			_, _ = io.WriteString(c, `{"id":"1","cmd":"server.status"}`+"\n")
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = proto.NewDecoder(c).Next()
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
}

// A handler can be stuck writing to a peer that has simply stopped reading,
// with no close of its own to signal that: a stalled tailnet client, a
// frozen tab. The old teardown order waited on that write before dropping
// anything, so a peer like this pinned ServeConn - and the goroutine and
// file descriptor behind it - forever. Only a forced write deadline can free
// it, and teardown must apply one before it waits, not after.
func TestATeardownDoesNotWaitForAHandlerStuckWritingToADeadPeer(t *testing.T) {
	s := newTestServer(t)
	pad := strings.Repeat("x", 8<<20) // comfortably over any configured unix socket buffer
	started := make(chan struct{})
	_ = s.Handle("test.stuck", func(_ *Client, r *proto.Request) proto.Response {
		close(started)
		return proto.OKResp(r.ID, map[string]any{"pad": pad})
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	// Captured once Serve's own, permanent accept-loop goroutine exists but
	// before dialing, so that one is part of the baseline (it does not exit
	// until Close) while the per-connection goroutine, this connection's
	// Client pump, and the handler's dispatch goroutine all count as new.
	before := runtime.NumGoroutine()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := io.WriteString(c, `{"id":"1","cmd":"test.stuck"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// Wait for the handler to actually be running (and thus for the
	// dispatch goroutine wg.Add already counted to exist) before touching
	// the connection: without this, the poll below could observe the
	// goroutine count before the server has even accepted the connection,
	// finding nothing "stuck" for reasons that have nothing to do with the
	// fix under test.
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never started")
	}

	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatal("expected a unix connection")
	}
	// Never read the response: once the kernel's send buffer for this
	// connection fills, the handler's Send blocks. Half-closing our write
	// side (not a full close: the peer's read direction, and thus its
	// unread receive buffer, stays exactly as stuck) makes the server's read
	// loop see EOF and start teardown while the handler is still stuck.
	if err := uc.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// The per-connection goroutine (Serve's accept loop), this connection's
	// Client pump, and the stuck handler's dispatch goroutine must all wind
	// down once teardown's forced write deadline fires - well inside the
	// bound, not after it.
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines never wound down: before=%d now=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The read loop's only exit before this test existed was d.Next() erroring.
// Close drops every client it knows about and releases the start lock, but a
// still-open connection whose peer has not noticed the server going away can
// keep sending requests; the old loop had nothing to stop it dispatching
// them into a server that no longer owns the tree, the state store, or even
// the data directory - a new server may already hold the lock by then.
func TestARequestAfterCloseIsNotDispatched(t *testing.T) {
	s := newTestServer(t)
	var calls int32
	_ = s.Handle("test.counted", func(_ *Client, r *proto.Request) proto.Response {
		atomic.AddInt32(&calls, 1)
		return proto.OKResp(r.ID, nil)
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { s.ServeConn(inR, outW); _ = outW.Close(); close(done) }()
	t.Cleanup(func() { _ = inW.Close() })

	// Drain responses in the background: if the bug lets the request
	// through, the handler's response must not itself block on an unread
	// pipe and mask the assertion below.
	go func() {
		d := proto.NewDecoder(outR)
		for {
			if _, err := d.Next(); err != nil {
				return
			}
		}
	}()

	// Wait for the connection to actually register before closing, so this
	// exercises the ordinary path (Close drops a client it already knows
	// about) rather than the race where Close's client snapshot missed it
	// entirely (which the s.closed check on the read loop covers instead).
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.clients)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection never registered")
		}
		time.Sleep(time.Millisecond)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The connection is still open: nobody closed inR/inW. A real peer that
	// has not yet noticed the server going away could still send this.
	if _, err := io.WriteString(inW, `{"id":"1","cmd":"test.counted"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeConn kept serving a connection after Close, want it to return")
	}

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("handler ran %d times after Close, want 0", got)
	}
}

// Close must not free the start lock while a handler it let keep running is
// still using the data it protects - but a handler with no deadline of its
// own must not be able to wedge a restart forever either, so the wait Close
// does is bounded, not unconditional.
func TestCloseReleasesTheLockOnlyAfterHandlersStop(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	started := make(chan struct{})
	_ = s.Handle("test.block", func(_ *Client, r *proto.Request) proto.Response {
		close(started)
		<-release
		return proto.OKResp(r.ID, nil)
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	c, err := net.Dial("unix", s.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, `{"id":"1","cmd":"test.block"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never started")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()

	other, err := New(Config{SocketPath: s.cfg.SocketPath, DataDir: s.cfg.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// Give Close's goroutine time to reach - and, if it releases too early,
	// pass straight through - the point where it would free the lock.
	time.Sleep(200 * time.Millisecond)
	if err := other.AcquireStartLock(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("a second start took the lock while a handler was still running: %v", err)
	}

	close(release)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close never returned")
	}

	if err := other.AcquireStartLock(); err != nil {
		t.Fatalf("the lock was not released once the handler finished: %v", err)
	}
}

// Binding twice without closing the first would leak it: Close only ever
// knows about whatever net.Listener is currently stored.
func TestListenRefusesASecondCallInsteadOfLeakingTheFirst(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); !errors.Is(err, ErrAlreadyListening) {
		t.Fatalf("second Listen = %v, want ErrAlreadyListening", err)
	}
}

// The read loop looks up s.handlers with no lock of its own, on the
// assumption that every handler is registered before there is anything to
// race it. A Handle call after Serve has started must be refused, not
// allowed to race that lookup.
func TestHandleAfterServeStartsIsRefused(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := s.Handle("test.late", func(_ *Client, r *proto.Request) proto.Response {
			return proto.OKResp(r.ID, nil)
		})
		if err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Handle kept succeeding after Serve started, want it to eventually refuse")
		}
		time.Sleep(time.Millisecond)
	}
}

// A restarted daemon cannot resume a pty, so a stopping server must kill
// what it spawned rather than orphan it. The child writes its own pid to a
// file before it sleeps, so this checks the real process is gone, not just
// that this server stopped tracking it.
func TestCloseKillsALivePTYsProcess(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	cwd := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// Belt and braces: Close is idempotent, and the assertion below already
	// calls it once deliberately. This only matters if an earlier t.Fatal
	// fires first and leaves the sleeping child behind.
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s.RegisterPaneCommands()

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"echo $$ > `+pidFile+`; sleep 30"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	// Wait for the child to actually report its own pid, so this test fails
	// on a real leak later, never on a slow start now.
	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for {
		b, readErr := os.ReadFile(pidFile)
		if readErr == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pty's process never reported its own pid")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("pid %d still exists after Close: %v", pid, err)
	}
}

// A second, overlapping call to Close must not
// return before the first one has actually finished tearing every pane down
// and released the start lock - it must never skip ahead and free the lock
// on its own. The child traps SIGHUP, which PTY.Close sends first, so it
// survives that grace period and is still alive while the first Close's
// teardown is still in flight; without the trap this test could pass by
// accident, the child having already died of its own accord before either
// Close call ran.
func TestASecondOverlappingCloseWaitsForTheFirstToFinish(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	cwd := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// Belt and braces, same reasoning as TestCloseKillsALivePTYsProcess: the
	// assertions below already call Close twice deliberately, and Close is
	// idempotent, so this only matters if an earlier t.Fatal fires first.
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	s.RegisterPaneCommands()

	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"trap '' HUP; echo $$ > `+pidFile+`; sleep 30"],"kind":"pty"}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}

	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for {
		b, readErr := os.ReadFile(pidFile)
		if readErr == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pty's process never reported its own pid")
		}
		time.Sleep(10 * time.Millisecond)
	}

	go func() { _ = s.Close() }()
	// Give the first Close time to set s.closed and start tearing its pane
	// down, so the call below actually takes the idempotent path instead of
	// winning the race to run the real teardown itself.
	time.Sleep(20 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if s.HoldsStartLock() {
		t.Fatal("Close returned but the start lock is still held")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("pid %d still exists after both Close calls returned: %v", pid, err)
	}

	other, err := New(Config{SocketPath: filepath.Join(dir, "other.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if err := other.AcquireStartLock(); err != nil {
		t.Fatalf("a second server could not take the lock this Close released: %v", err)
	}
}

// This is the other half of the same guarantee. A handler already
// dispatched and in flight when Close takes its first teardownLivePanes
// snapshot can still start a pane and register it live afterward -
// handlePaneCreate's isClosed() guard cannot refuse a request dispatched
// before Close began refusing new work. This drives that shape directly: a
// handler stalls on a channel (the same shape
// TestCloseReleasesTheLockOnlyAfterHandlersStop uses), and only once
// released does it start a real pty and call putLive - after Close's first
// snapshot has already run and found s.live empty. Close's second
// teardownLivePanes pass, after waitForHandlers, is the only thing that can
// still catch this pane; without it, this pane would outlive Close.
func TestClosesSecondTeardownPassCatchesAPaneStartedWhileTheFirstPassRan(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	cwd := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	// pidWaitFailed carries a give-up from inside the handler goroutine back
	// to the test goroutine: t.Fatal must run on the goroutine executing the
	// test function, never on a handler dispatched by s.ServeConn's own
	// "go func" (server.go), so the handler below cannot call it directly.
	pidWaitFailed := make(chan string, 1)
	_ = s.Handle("test.stall_then_pane", func(_ *Client, r *proto.Request) proto.Response {
		close(started)
		<-release
		// Past this point is exactly what handlePaneCreate's own tail does
		// for a pty pane (startPane, then putLive) - reached here without
		// its isClosed() guard, because this request was dispatched before
		// Close ever set s.closed, the same way a real pane.create could be.
		g, err := pane.NewGrid(80, 24)
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
		}
		p, err := pane.StartPTY(pane.SpawnOpts{
			Cwd:    cwd,
			Argv:   []string{"sh", "-c", "trap '' HUP; echo $$ > " + pidFile + "; sleep 30"},
			Cols:   80,
			Rows:   24,
			Sock:   s.cfg.SocketPath,
			PaneID: "w1:stall",
		}, g)
		if err != nil {
			g.Close()
			return proto.ErrResp(r.ID, proto.ErrSpawnFailed, err.Error())
		}
		// putLive publishes this pane to Close's backstop pass, which can
		// send SIGHUP the instant putLive returns. Waiting here for the
		// child to have already written its own pid is what makes that safe
		// to race against: "trap '' HUP" and "echo $$ > pidFile" are two
		// sequential statements in the same script, so once the pid file
		// exists the trap is already installed too. Skipping this wait
		// would leave a real window where SIGHUP arrives before the trap
		// does, killing the child on the signal this test means to prove
		// PTY.Close escalates PAST, rather than the one it starts with.
		// Below handlerTeardownWait (2s), not just below the old 3s: this
		// poll runs while the handler is still "in flight", and Close's own
		// waitForHandlers gives up waiting for that after handlerTeardownWait.
		// A poll that could still be running past that point risks the
		// backstop teardown pass taking its snapshot before putLive below
		// ever runs, which is the exact race this whole test exists to
		// catch.
		deadline := time.Now().Add(1500 * time.Millisecond)
		for {
			if b, readErr := os.ReadFile(pidFile); readErr == nil && len(strings.TrimSpace(string(b))) > 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = p.Close()
				g.Close()
				select {
				case pidWaitFailed <- "the child never reported its own pid within " +
					"the poll's own deadline, which must stay under handlerTeardownWait":
				default:
				}
				return proto.ErrResp(r.ID, proto.ErrInternal, "the child never reported its own pid")
			}
			time.Sleep(5 * time.Millisecond)
		}
		s.putLive("w1:stall", &LivePane{PTY: p, Grid: g})
		return proto.OKResp(r.ID, nil)
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	t.Cleanup(func() { _ = inW.Close() })
	go func() {
		d := proto.NewDecoder(outR)
		for {
			if _, err := d.Next(); err != nil {
				return
			}
		}
	}()
	if _, err := io.WriteString(inW, `{"id":"1","cmd":"test.stall_then_pane"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never started")
	}

	// afterTeardownSnapshot (server.go) fires once per call to
	// teardownLivePanes - twice across one Close. sync.Once means only the
	// FIRST firing (Close's first pass, before waitForHandlers) closes
	// snapshotTaken; the second firing (the backstop pass, after the
	// handler below has released and returned) is ignored here.
	snapshotTaken := make(chan struct{})
	var once sync.Once
	s.afterTeardownSnapshot = func() { once.Do(func() { close(snapshotTaken) }) }

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()

	select {
	case <-snapshotTaken:
	case <-time.After(3 * time.Second):
		t.Fatal("Close never took its first teardown snapshot")
	}
	// Only now does the handler proceed to start its pane and call putLive -
	// s.live was empty for Close's first snapshot, so only the second pass
	// can still catch what this releases.
	close(release)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned")
	}

	// A silent give-up here used to be possible: the handler's own poll
	// could exceed handlerTeardownWait and return an error response nothing
	// in this test ever inspected, so the pid-file race this test exists to
	// prove could go untested without the test itself failing.
	select {
	case reason := <-pidWaitFailed:
		t.Fatal(reason)
	default:
	}

	if s.HoldsStartLock() {
		t.Fatal("Close returned but the start lock is still held")
	}

	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for {
		b, readErr := os.ReadFile(pidFile)
		if readErr == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pane's process never reported its own pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("pid %d still exists after Close returned: %v", pid, err)
	}
}

// RestartNote's exact wording is quoted verbatim in README.md
// (internal/boundary's own docs_test.go asserts the README contains this
// same string, since boundary cannot import package server without pulling
// cgo into a package that exists specifically to stay free of it). Pinning
// the value here, not just a substring check, is what keeps a future edit
// to this sentence from silently drifting out of sync with that copy.
func TestRestartNoteExactWording(t *testing.T) {
	want := "A restart brings back the layout, the labels and the working directories. " +
		"It does not bring back running processes. Panes come back closed or unknown, never idle. " +
		"A headless pane resumes only when its adapter recorded a harness session id."
	if RestartNote != want {
		t.Fatalf("RestartNote = %q, want %q\nUpdate README.md's quoted copy in the same commit.", RestartNote, want)
	}
}
