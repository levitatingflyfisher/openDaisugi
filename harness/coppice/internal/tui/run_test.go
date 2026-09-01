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
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/tiles"
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
	// The foreman's wait is short, so a floor test sees the server's
	// not-ready note in seconds.
	// The home holds a gate hook, so a floor here does not show the
	// install hint and every test sees the layout it measures.
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "DAISUGI_GATE_HOOK=opendaisugi.gate /x/daisugi gate check --mode shadow"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(hook), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir, HookHome: home,
		ForemanDir: filepath.Join(dir, "state", "foreman"), ForemanWait: 1500 * time.Millisecond})
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
	out, err := runFloor(t, s, scripted("\x03"))
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
	for _, b := range Table() {
		if !strings.Contains(first, b.Keys+" "+b.Help) {
			t.Fatalf("footer lacks %q: %q", b.Keys+" "+b.Help, first)
		}
	}
}

func TestQIsTextOnlyAfterCtrlT(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("q", "\x14q", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing running. Enter opens claude here.") {
		t.Fatalf("empty floor frame missing: %q", out)
	}
	frames := splitFrames(out)
	if len(frames) < 3 || !strings.Contains(stripSGR(frames[1]), railHint) {
		t.Fatalf("q on the rail did not show the hint: %q", frames)
	}
	if !strings.Contains(stripSGR(lastFrame(out)), "› q") {
		t.Fatalf("q after ctrl-t did not reach the prompt: %q", lastFrame(out))
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
	err := Run(Options{Socket: s.Socket(), In: scripted(" ", "\x1b", "\x03"), Out: &out,
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

// rowName is the id or label on one rail row line: the first word after
// the mark, past the window number when the row has one.
func rowName(l string) string {
	fs := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(l, "  "), "› "))
	if len(fs) > 1 && len(fs[0]) == 1 && fs[0] >= "1" && fs[0] <= "9" {
		return fs[1]
	}
	if len(fs) == 0 {
		return ""
	}
	return fs[0]
}

// markedRow returns the id or label on the one cursor row of a frame.
func markedRow(t *testing.T, frame string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(stripSGR(frame), "\r\n") {
		l = strings.TrimPrefix(l, "\x1b[K")
		if strings.HasPrefix(l, "  › ") {
			found = append(found, rowName(l))
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
	out, err := runFloor(t, s, scripted("\x1b[B", "\x1b[B", "\x03"))
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
	out, err := runFloor(t, s, scripted("\x14close "+id+"\r", "\x03"))
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

func TestTalkWithNoDefaultHarnessShowsHowToSetOne(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("\x14hello there\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := stripSGR(frames[len(frames)-1])
	if !strings.Contains(last, "no default harness") {
		t.Fatalf("the no default harness message is missing: %q", last)
	}
	if strings.Contains(last, "› hello there") {
		t.Fatalf("prompt not cleared: %q", last)
	}
}

func TestThePromptEchoesAndBackspaceDeletesARune(t *testing.T) {
	s := newTestServer(t)
	out, err := runFloor(t, s, scripted("\x14ab", "\x7f", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	if !strings.Contains(stripSGR(frames[len(frames)-1]), "› a ") {
		t.Fatalf("prompt = %q", frames[len(frames)-1])
	}
}

// bytesKeys turns a byte string into keyByte keys for handle.
func byteKeys(s string) []key {
	ks := make([]key, 0, len(s))
	for _, b := range []byte(s) {
		ks = append(ks, key{kind: keyByte, b: b})
	}
	return ks
}

// handleAll applies every key to f and fails the test on an error.
func handleAll(t *testing.T, f *floor, ks ...key) {
	t.Helper()
	for _, k := range ks {
		if _, err := f.handle(k); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCtrlTTalksAndSpaceAppendsWhileTalking(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	f := testFloor(m, 80)
	handleAll(t, f, key{kind: keyByte, b: 0x14})
	if !m.Talking || m.Prompt != "" {
		t.Fatalf("ctrl-t: talking %v prompt %q", m.Talking, m.Prompt)
	}
	handleAll(t, f, byteKeys(" hi there")...)
	if m.Prompt != " hi there" {
		t.Fatalf("prompt = %q, want the typed bytes with both spaces", m.Prompt)
	}
	if m.Peek != nil {
		t.Fatal("space while talking opened the peek")
	}
}

// A key the rail does not use is not text. The prompt line never takes
// the keys on its own, and the message says how to type a line.
func TestAPrintableByteOnTheRailIsNotText(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	f := testFloor(m, 80)
	handleAll(t, f, byteKeys("aqz")...)
	if m.Talking || m.Prompt != "" {
		t.Fatalf("talking %v prompt %q", m.Talking, m.Prompt)
	}
	if m.Message != railHint {
		t.Fatalf("message %q, want the hint", m.Message)
	}
}

func TestEnterOnAnEmptyPromptWhileTalkingOnlyStopsTalking(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	f := testFloor(m, 80)
	handleAll(t, f, key{kind: keyByte, b: 0x14}, key{kind: keyByte, b: '\r'})
	if m.Talking || m.Prompt != "" {
		t.Fatalf("talking %v prompt %q after Enter on an empty prompt", m.Talking, m.Prompt)
	}
}

func TestEscClearsAFocusedPrompt(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	f := testFloor(m, 80)
	handleAll(t, f, byteKeys("\x14ab")...)
	handleAll(t, f, key{kind: keyEsc})
	if m.Talking || m.Prompt != "" {
		t.Fatalf("talking %v prompt %q after esc", m.Talking, m.Prompt)
	}
}

func TestEscOnTheBareRosterChangesNothing(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "blocked"}}, Cursor: 1, Message: "hello"}
	f := testFloor(m, 80)
	before := strings.Join(Render(m, 80, 12), "\n")
	handleAll(t, f, key{kind: keyEsc})
	after := strings.Join(Render(m, 80, 12), "\n")
	if before != after {
		t.Fatalf("esc changed the frame:\n%s\n%s", before, after)
	}
	if m.Message != "hello" || m.Cursor != 1 {
		t.Fatalf("esc changed the model: message %q cursor %d", m.Message, m.Cursor)
	}
}

// ctrl-w asks before it stops an agent, whether or not it has a window.
// The windows do not change while it asks.
func TestCtrlWOnATiledRowAsksToStopTheAgent(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", State: "working"}, {ID: "b", State: "working"}},
		Tiles: tiles.New(tiles.Focus, 2), Screens: map[string]*attach.Screen{}}
	m.Tiles.Fill("a")
	m.Tiles.Fill("b")
	m.Cursor = m.indexOf("a", 0)
	f := testFloor(m, 200)
	handleAll(t, f, key{kind: keyByte, b: 0x17})
	if m.Confirm != "a" {
		t.Fatalf("confirm = %q, want a", m.Confirm)
	}
	if got := strings.Join(m.Tiles.Slots, " "); got != "a b" {
		t.Fatalf("the question changed the windows: %q", got)
	}
}

func TestCtrlWThenEscKeepsTheAgent(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	if _, err := runFloor(t, s, scripted("\x17", "\x1b", "\x03")); err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	for _, r := range res["panes"].([]any) {
		if r.(map[string]any)["id"] == id {
			return
		}
	}
	t.Fatalf("pane %s was stopped after Esc", id)
}

func TestCtrlWThenEnterStopsTheAgentAndItLeaves(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	out, err := runFloor(t, s, scripted("\x17", "\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripSGR(out), "› "+StopQuestion(id)) {
		t.Fatalf("the question is not on the prompt line: %q", out)
	}
	last := stripSGR(lastFrame(out))
	if !strings.Contains(last, "Nothing running.") || strings.Contains(last, "Recent") {
		t.Fatalf("after Enter the floor is not empty: %q", last)
	}
	if strings.Contains(last, "ended") {
		t.Fatalf("a stopped agent drew the ended line: %q", last)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	for _, r := range res["panes"].([]any) {
		if r.(map[string]any)["id"] == id {
			t.Fatalf("pane %s is still live on the server", id)
		}
	}
}

func TestCtrlWConfirmCancelsOnAnyOtherKey(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "a", Label: "auth fix", State: "working"}}}
	f := testFloor(m, 80)
	handleAll(t, f, key{kind: keyByte, b: 0x17})
	if m.Confirm != "a" {
		t.Fatalf("confirm = %q, want a", m.Confirm)
	}
	lines := Render(m, 80, 8)
	if got := strings.TrimRight(stripSGR(lines[len(lines)-1]), " "); got != "› Stop auth fix? Enter stops it, Esc keeps it." {
		t.Fatalf("prompt line = %q", got)
	}
	handleAll(t, f, key{kind: keyByte, b: 'n'})
	if m.Confirm != "" || m.Prompt != "" || m.Talking {
		t.Fatalf("n did not only cancel: confirm %q prompt %q talking %v", m.Confirm, m.Prompt, m.Talking)
	}
	handleAll(t, f, key{kind: keyByte, b: 0x17}, key{kind: keyEsc})
	if m.Confirm != "" {
		t.Fatalf("esc did not cancel: %q", m.Confirm)
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
	runWideFloor(t, s, dir, scripted("\x14lock\r", "\x03"))
	b, err := os.ReadFile(filepath.Join(dir, "floor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"layout":"focus","locked":true}` {
		t.Fatalf("floor.json = %s", b)
	}
	out := runWideFloor(t, s, dir, scripted("\x14swap 1 2\r", "\x03"))
	if !strings.Contains(out, "Layout is locked. Type unlock first.") {
		t.Fatalf("the lock did not survive a restart: %q", out)
	}
}

func TestTheLayoutRoundTripsThroughFloorJSONAcrossTwoRuns(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	runWideFloor(t, s, dir, scripted("\x14layout one\r", "\x03"))
	out := runWideFloor(t, s, dir, scripted("\x14swap 1 2\r", "\x03"))
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
	out := runWideFloor(t, s, dir, scripted("\x14swap 1 2\r", "\x03"))
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
	out, err := runFloor(t, s, scripted("\x1b[B", "\x1b[B", "\x14read nope\r", "\x03"))
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
	out, err := runFloor(t, s, scripted("\x14close nope\r", "\x03"))
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
	out, err := runFloor(t, s, scripted("\r", "\x00", "\x03"))
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
	err := Run(Options{Socket: filepath.Join(t.TempDir(), "none.sock"), In: scripted("\x03"), Out: &out,
		Size: func() (int, int) { return 80, 24 }})
	if !errors.Is(err, attach.ErrServerGone) {
		t.Fatalf("err = %v, want ErrServerGone", err)
	}
}

// The floor hands its leave key to attach. With ctrl-a chosen, the status
// line under the pane names ctrl-a, and ctrl-a comes back to the roster.
func TestTheFloorPassesTheLeaveKeyToAttach(t *testing.T) {
	s := newTestServer(t)
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	out := newSignalWriter()
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{Socket: s.Socket(), In: pr, Out: out,
			Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "claude", Leave: 0x01})
	}()
	_, _ = io.WriteString(pw, "\r")
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(out.String(), "ctrl-a leave") }) {
		t.Fatalf("the status line does not name ctrl-a: %q", out.String())
	}
	if strings.Contains(out.String(), "ctrl-space leave") {
		t.Fatalf("the status line names the default: %q", out.String())
	}
	_, _ = io.WriteString(pw, "\x01")
	const cleared = "\x1b[2J\x1b[H\x1b[Kcoppice ·"
	if !waitFor(t, 5*time.Second, func() bool { return strings.Count(out.String(), cleared) >= 2 }) {
		t.Fatalf("ctrl-a did not come back to the roster: %q", out.String())
	}
	_, _ = io.WriteString(pw, "\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctrl-c")
	}
}

// Enter on an empty floor asks the server for the default harness by
// name, with no command of its own, so the config file decides what
// runs. The pane then opens, the leave key leaves it, and ctrl-c quits.
func TestEnterOnAnEmptyFloorOpensTheDefaultHarnessFromTheConfig(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(Options{Socket: s.Socket(), In: scripted("\r", "\x00", "\x03"), Out: &out,
		Size: func() (int, int) { return 80, 24 }, Cwd: t.TempDir(), Default: "fakeh"})
	if err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	rows, _ := res["panes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("panes after Enter = %d, want 1: %v", len(rows), rows)
	}
	row, _ := rows[0].(map[string]any)
	cmd, _ := row["cmd"].([]any)
	if len(cmd) == 0 || cmd[0] != "sh" {
		t.Fatalf("cmd = %v, want it to start with sh", cmd)
	}
	if row["harness"] != "fakeh" {
		t.Fatalf("harness = %v, want fakeh", row["harness"])
	}
}

// With no config file, Enter on an empty floor puts the server's refusal
// on the message line and the floor goes on.
func TestEnterOnAnEmptyFloorWithNoConfigShowsTheRefusal(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// PATH holds nothing, so no real harness can ever start here.
	t.Setenv("PATH", t.TempDir())
	out, err := runFloor(t, s, scripted("\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := stripSGR(frames[len(frames)-1])
	if !strings.Contains(last, `no harness named "claude"`) {
		t.Fatalf("the refusal is not on the message line: %q", last)
	}
}

// The prompt word open <name> [args...] goes through the same harness
// path as Enter and the shell, so the config table decides the command
// and the typed args follow it.
func TestOpenAtThePromptOpensTheHarnessFromTheConfig(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	out, err := runFloor(t, s, scripted("\x14open fakeh -x\r", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	res := rawCall(t, s.Socket(), "pane.list", nil)
	rows, _ := res["panes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("panes after open = %d, want 1: %v %q", len(rows), rows, out)
	}
	row, _ := rows[0].(map[string]any)
	cmd, _ := row["cmd"].([]any)
	if len(cmd) != 4 || cmd[0] != "sh" || cmd[3] != "-x" {
		t.Fatalf("cmd = %v, want sh -c 'sleep 30' -x", cmd)
	}
	if row["harness"] != "fakeh" {
		t.Fatalf("harness = %v, want fakeh", row["harness"])
	}
}

func TestShiftTabMovesTheCursorToTheNextNeed(t *testing.T) {
	m := &Model{Rows: []Row{{ID: "w", State: "working"}, {ID: "b", State: "blocked"}}, Cursor: 1}
	f := testFloor(m, 80)
	ks, _ := readKey(feed("\x1b[Z"), 0x1b)
	for _, k := range ks {
		if _, err := f.handle(k); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := m.Selected(); r.ID != "b" {
		t.Fatalf("shift-tab selected %q, want b", r.ID)
	}
}

// createTask makes a task with the given fields and returns its id.
func createTask(t *testing.T, socket string, params map[string]any) string {
	t.Helper()
	res := rawCall(t, socket, "task.create", params)
	id, _ := res["task"].(string)
	if id == "" {
		t.Fatalf("task.create returned no task: %v", res)
	}
	return id
}

// tree at the prompt opens the text tree in place of the roster. Enter on
// a task line pushes the roster into that task and names it in the bar.
// Esc pops back to every pane.
func TestTreeOpensAndEnterPushesTheRosterIntoOneTask(t *testing.T) {
	s := newTestServer(t)
	team := createTask(t, s.Socket(), map[string]any{"label": "review-team"})
	docs := createTask(t, s.Socket(), map[string]any{"label": "docs", "parent": team, "cwd": t.TempDir()})
	other := createTask(t, s.Socket(), map[string]any{"label": "other", "cwd": t.TempDir()})
	inDocs := rawCall(t, s.Socket(), "pane.create", map[string]any{
		"task": docs, "kind": "pty", "cmd_argv": []string{"sh", "-c", "sleep 30"}, "label": "writer"})
	inOther := rawCall(t, s.Socket(), "pane.create", map[string]any{
		"task": other, "kind": "pty", "cmd_argv": []string{"sh", "-c", "sleep 30"}, "label": "loner"})
	docsPane, _ := inDocs["pane"].(string)
	otherPane, _ := inOther["pane"].(string)
	// tree, Enter on the first task line, which is the team, Esc, then quit.
	out, err := runFloor(t, s, scripted("\x14tree\r", "\r", "\x1b", "\x03"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	var treeFrame, filtered, cleared string
	for _, fr := range frames {
		plain := stripSGR(fr)
		switch {
		case strings.Contains(plain, "├─") || strings.Contains(plain, "└─"):
			treeFrame = plain
		case strings.Contains(plain, "coppice › review-team"):
			filtered = plain
		}
	}
	cleared = stripSGR(frames[len(frames)-1])
	if treeFrame == "" {
		t.Fatalf("no frame drew the tree:\n%s", out)
	}
	for _, want := range []string{"review-team", "docs", "other", "writer", "loner", "› "} {
		if !strings.Contains(treeFrame, want) {
			t.Fatalf("tree frame is missing %q:\n%s", want, treeFrame)
		}
	}
	if filtered == "" {
		t.Fatalf("no frame named the task in the bar:\n%s", out)
	}
	if !strings.Contains(filtered, "writer") || strings.Contains(filtered, "loner") {
		t.Fatalf("filtered roster does not show only the team's panes:\n%s", filtered)
	}
	if !strings.Contains(cleared, "writer") || !strings.Contains(cleared, "loner") || strings.Contains(cleared, "› review-team") {
		t.Fatalf("Esc did not pop:\n%s", cleared)
	}
	_ = docsPane
	_ = otherPane
}

// A server that refuses task.list still gives the floor its roster. The
// refusal is the message line, and the tasks are empty.
func TestARefusedTaskListStillRendersTheRoster(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	srv, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Handle("task.list", func(_ *server.Client, r *proto.Request) proto.Response {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "no command \"task.list\"")
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	s := &testServer{sock: sock, srv: srv}
	createShellPane(t, s.Socket(), "sh", "-c", "echo hello; sleep 30")
	out, err := runFloor(t, s, scripted("\x03"))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	last := stripSGR(frames[len(frames)-1])
	if !strings.Contains(last, "sh") || strings.Contains(last, "Nothing running") {
		t.Fatalf("the roster is missing:\n%s", last)
	}
	if !strings.Contains(last, "task.list") {
		t.Fatalf("the refusal is not the message line:\n%s", last)
	}
}
