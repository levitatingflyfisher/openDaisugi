package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// testServer is one in-process coppice server on its own socket.
type testServer struct {
	sock string
	srv  *server.Server
}

func (s *testServer) Socket() string { return s.sock }

// newTestServer starts a server on a socket under a temp dir. Every pane
// verb and events.subscribe are registered by server.New.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return &testServer{sock: sock, srv: s}
}

// rawCall sends one request on a fresh connection and returns the reply.
func rawCall(t *testing.T, socket, cmd string, params map[string]any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := map[string]any{"id": "t", "cmd": cmd}
	for k, v := range params {
		req[k] = v
	}
	if err := proto.NewEncoder(conn).Send(req); err != nil {
		t.Fatal(err)
	}
	line, err := proto.NewDecoder(conn).Next()
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("%s failed: %v", cmd, r.Error)
	}
	return r.Result
}

// createShellPane creates a pty pane running argv and waits until the pane
// has drawn something, so a peek on it has text to show.
func createShellPane(t *testing.T, socket string, argv ...string) string {
	t.Helper()
	res := rawCall(t, socket, "pane.create", map[string]any{
		"cwd": t.TempDir(), "kind": "pty", "cmd_argv": argv, "cols": 80, "rows": 24,
	})
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane: %v", res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := rawCall(t, socket, "pane.read", map[string]any{"pane": id, "source": "visible"})
		if text, _ := res["text"].(string); strings.TrimSpace(text) != "" {
			return id
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane %s never drew anything", id)
	return ""
}

// scripted yields the joined keys, then EOF.
func scripted(keys ...string) io.Reader {
	return strings.NewReader(strings.Join(keys, ""))
}

// splitFrames splits Out on the cursor home every frame starts with. The
// first frame clears the screen before its cursor home, so a leading
// element that holds only that clear is dropped with an empty one.
func splitFrames(s string) []string {
	parts := strings.Split(s, "\x1b[H")
	if len(parts) > 0 && (parts[0] == "" || parts[0] == "\x1b[2J") {
		parts = parts[1:]
	}
	return parts
}

func runFloor(t *testing.T, s *testServer, in io.Reader) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(Options{Socket: s.Socket(), In: in, Out: &out,
		Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "claude"})
	return out.String(), err
}

func TestTheFirstFrameListsThePaneAndTheBar(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	out, err := runFloor(t, s, scripted("q"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	if len(frames) < 1 {
		t.Fatal("no frame written")
	}
	first := stripSGR(frames[0])
	if !strings.Contains(first, "coppice ·") {
		t.Fatalf("bar missing: %q", first)
	}
	if !strings.Contains(first, id) {
		t.Fatalf("pane %s missing: %q", id, first)
	}
	if !strings.Contains(first, "working") && !strings.Contains(first, "idle") {
		t.Fatalf("state missing: %q", first)
	}
	if !strings.Contains(first, Hints()) {
		t.Fatalf("footer missing: %q", first)
	}
}

func TestQReturnsNilAfterAFrame(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("q"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing running. Enter opens claude here.") {
		t.Fatalf("empty floor frame missing: %q", out)
	}
}

func TestCtrlCReturnsNil(t *testing.T) {
	s := newTestServer(t)
	if _, err := runFloor(t, s, scripted("\x03")); err != nil {
		t.Fatal(err)
	}
}

func TestEOFOnInReturnsNil(t *testing.T) {
	s := newTestServer(t)
	if _, err := runFloor(t, s, scripted()); err != nil {
		t.Fatal(err)
	}
}

func TestSpaceOpensPeekAndEscClosesIt(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	var out bytes.Buffer
	err := Run(Options{Socket: s.Socket(), In: scripted(" ", "\x1b", "q"), Out: &out,
		Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out.String())
	if len(frames) < 3 {
		t.Fatalf("%d frames, want at least 3", len(frames))
	}
	peeked := false
	for _, f := range frames[1:] {
		if strings.Contains(f, "hello") {
			peeked = true
		}
	}
	if !peeked {
		t.Fatal("peek did not show the pane's last lines")
	}
	if strings.Contains(frames[len(frames)-1], "hello") {
		t.Fatal("esc did not close the peek")
	}
}

// markedRow returns the id or label on the one cursor row of a frame.
func markedRow(t *testing.T, frame string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(stripSGR(frame), "\r\n") {
		l = strings.TrimPrefix(l, "\x1b[K")
		if strings.HasPrefix(l, "  › ") {
			found = append(found, strings.Fields(l)[1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d marked rows in frame %q", len(found), frame)
	}
	return found[0]
}

func TestDownArrowMovesTheCursorToTheSecondPane(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo one; sleep 30")
	createShellPane(t, s.Socket(), "sh", "-c", "echo two; sleep 30")
	out, err := runFloor(t, s, scripted("\x1b[B", "q"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	if len(frames) < 2 {
		t.Fatalf("%d frames, want at least 2", len(frames))
	}
	first, last := markedRow(t, frames[0]), markedRow(t, frames[len(frames)-1])
	if first == last {
		t.Fatalf("the marker did not move: %q on both frames", first)
	}
}

func TestClosePromptClosesThePane(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	out, err := runFloor(t, s, scripted("close "+id+"\r", "q"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := stripSGR(frames[len(frames)-1])
	if strings.Contains(last, id) {
		t.Fatalf("closed pane still listed: %q", last)
	}
	if !strings.Contains(last, "Nothing running.") {
		t.Fatalf("empty floor not shown: %q", last)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	for _, r := range res["panes"].([]any) {
		m := r.(map[string]any)
		if m["id"] == id && m["closed"] != true {
			t.Fatalf("pane %s is still open on the server", id)
		}
	}
}

func TestTalkWithNoForemanShowsTheMessage(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("hello there\r", "q"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := stripSGR(frames[len(frames)-1])
	if !strings.Contains(last, "No foreman. Enter opens claude here") {
		t.Fatalf("no foreman message missing: %q", last)
	}
	if strings.Contains(last, "› hello there") {
		t.Fatalf("prompt not cleared: %q", last)
	}
}

func TestThePromptEchoesAndBackspaceDeletesARune(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("ab", "\x7f", "q"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	if !strings.Contains(stripSGR(frames[len(frames)-1]), "› aq") {
		t.Fatalf("prompt = %q", frames[len(frames)-1])
	}
}

// runWideFloor runs Run at 200 columns with dataDir for floor.json.
func runWideFloor(t *testing.T, s *testServer, dataDir string, in io.Reader) string {
	t.Helper()
	var out bytes.Buffer
	err := Run(Options{Socket: s.Socket(), In: in, Out: &out, DataDir: dataDir,
		Size: func() (int, int) { return 200, 40 }, Cwd: t.TempDir(), Default: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestTheLockRoundTripsThroughFloorJSONAcrossTwoRuns(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	runWideFloor(t, s, dir, scripted("lock\r", "q"))
	b, err := os.ReadFile(filepath.Join(dir, "floor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"layout":"focus","locked":true}` {
		t.Fatalf("floor.json = %s", b)
	}
	out := runWideFloor(t, s, dir, scripted("swap 1 2\r", "q"))
	if !strings.Contains(out, "Layout is locked. Type unlock first.") {
		t.Fatalf("the lock did not survive a restart: %q", out)
	}
}

func TestTheLayoutRoundTripsThroughFloorJSONAcrossTwoRuns(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	runWideFloor(t, s, dir, scripted("layout one\r", "q"))
	out := runWideFloor(t, s, dir, scripted("swap 1 2\r", "q"))
	if !strings.Contains(out, "swap needs slot numbers from 1 to 1") {
		t.Fatalf("layout one did not survive a restart: %q", out)
	}
	last := stripSGR(lastFrame(out))
	if n := strings.Count(strings.Split(last, "\r\n")[3], "│"); n != 1 {
		t.Fatalf("layout one draws %d dividers on a body line, want 1: %q", n, last)
	}
}

func TestAnUnreadableFloorJSONGivesTheDefaultLayout(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWideFloor(t, s, dir, scripted("swap 1 2\r", "q"))
	if strings.Contains(out, "swap needs") || strings.Contains(out, "locked") {
		t.Fatalf("the default is not focus unlocked with two slots: %q", out)
	}
	if strings.Contains(out, "not wired") {
		t.Fatalf("the old message is back: %q", out)
	}
}

func TestReadOfAnUnknownPaneLeavesTheCursorAlone(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo one; sleep 30")
	createShellPane(t, s.Socket(), "sh", "-c", "echo two; sleep 30")
	out, err := runFloor(t, s, scripted("\x1b[B", "read nope\r", "q"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := frames[len(frames)-1]
	if !strings.Contains(stripSGR(last), "no pane nope") {
		t.Fatalf("message missing: %q", last)
	}
	if markedRow(t, frames[0]) == markedRow(t, last) {
		t.Fatalf("the cursor went back to the first row: %q", markedRow(t, last))
	}
}

func TestAServerErrorBecomesTheMessageLine(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("close nope\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := stripSGR(frames[len(frames)-1])
	if !strings.Contains(last, "nope") {
		t.Fatalf("server error missing from the message line: %q", last)
	}
}

func TestEnterOnARowAttachesAndDetachReturnsToTheRoster(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	out, err := runFloor(t, s, scripted("\r", "\x01d", "q"))
	if err != nil {
		t.Fatal(err)
	}
	const cleared = "\x1b[2J\x1b[H\x1b[Kcoppice ·"
	if strings.Count(out, cleared) < 2 {
		t.Fatalf("the roster after attach does not start with a full clear: %q", out)
	}
}

// signalWriter is an Out a test can share with Run on another goroutine.
// Every Write is under a mutex, and first closes on the first Write.
type signalWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	first chan struct{}
	once  sync.Once
}

func newSignalWriter() *signalWriter {
	return &signalWriter{first: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.once.Do(func() { close(w.first) })
	return w.buf.Write(p)
}

func TestAServerThatStopsMakesRunReturnServerGone(t *testing.T) {
	s := newTestServer(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: s.Socket(), In: pr, Out: out,
			Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "claude"})
	}()
	select {
	case <-out.first:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame within 5 s")
	}
	_ = s.srv.Close()
	select {
	case err := <-done:
		if !errors.Is(err, attach.ErrServerGone) {
			t.Fatalf("err = %v, want ErrServerGone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the server stopped")
	}
}

func TestANoServerSocketIsServerGone(t *testing.T) {
	var out bytes.Buffer
	err := Run(Options{Socket: filepath.Join(t.TempDir(), "none.sock"), In: scripted("q"), Out: &out,
		Size: func() (int, int) { return 80, 24 }})
	if !errors.Is(err, attach.ErrServerGone) {
		t.Fatalf("err = %v, want ErrServerGone", err)
	}
}
