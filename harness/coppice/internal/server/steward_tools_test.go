package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/steward"
)

// toolCall dials the turn socket and sends one request.
func toolCall(t *testing.T, path string, req steward.ToolRequest) steward.ToolResponse {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	b, _ := json.Marshal(req)
	conn.Write(append(b, '\n'))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		t.Fatalf("no response: %v", err)
	}
	var resp steward.ToolResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func toolsFixture(t *testing.T) (*Server, *stewardChat, *turnSocket) {
	t.Helper()
	e := newFakeEnv(t)
	s, _ := chatServer(t, e, nil)
	s.StartSteward()
	fc := s.ensureChat()
	if fc.chat == nil {
		t.Fatal(fc.problem)
	}
	fc.mu.Lock()
	fc.chat.Append(ctx0, pando.KindUser, "hello there")
	fc.chat.Append(ctx0, pando.KindSteward, "hi")
	fc.mu.Unlock()
	ts, err := s.openTurnSocket(fc, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.Close)
	return s, fc, ts
}

func TestTheTurnSocketServesTheFourToolsToItsOwnProcess(t *testing.T) {
	_, fc, ts := toolsFixture(t)
	ts.Started(os.Getpid()) // this test process is the turn's process
	if fi, err := os.Stat(ts.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	if r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "zoom", Args: map[string]any{"id": 0, "n": 1}}); !r.OK || r.Result != "hello there" {
		t.Fatalf("zoom = %+v", r)
	}
	if r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "date", Args: map[string]any{"id": 1}}); !r.OK || !strings.HasSuffix(r.Result, "Z") {
		t.Fatalf("date = %+v", r)
	}
	if r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "status"}); !r.OK {
		t.Fatalf("status = %+v", r)
	}
	if r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "spawn", Args: map[string]any{"project": "p"}}); r.OK || !strings.Contains(r.Error, "brief is missing") {
		t.Fatalf("spawn = %+v", r)
	}
	if r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "zoom", Args: map[string]any{"id": 0.5, "n": 1}}); r.OK {
		t.Fatalf("a half message was served: %+v", r)
	}
	// Each call and its result is one tool message, word for word.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	m, err := fc.chat.Message(2)
	if err != nil || m.Kind != pando.KindTool || m.Text != "zoom({\"id\":0,\"n\":1})\nhello there" {
		t.Fatalf("message 2 = %+v, %v", m, err)
	}
	m, _ = fc.chat.Message(3)
	if m.Kind != pando.KindTool || !strings.HasPrefix(m.Text, "date({\"id\":1})\n20") {
		t.Fatalf("message 3 = %+v", m)
	}
	m, _ = fc.chat.Message(5)
	if m.Kind != pando.KindTool || !strings.HasPrefix(m.Text, "spawn({\"project\":\"p\"})\nerror: brief is missing") {
		t.Fatalf("message 5 = %+v", m)
	}
}

func TestOtherVerbsAreRefused(t *testing.T) {
	_, _, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	for _, tool := range []string{"bash", "read", "agent.allow", "floor.chat", "pane.create", ""} {
		r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: tool})
		if r.OK || !strings.Contains(r.Error, "no tool named") {
			t.Fatalf("%q = %+v", tool, r)
		}
	}
}

func TestAPeerOutsideTheTurnsTreeIsRefused(t *testing.T) {
	_, _, ts := toolsFixture(t)
	// The turn's process is a child of this test; this test is not below it.
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	ts.Started(sleeper.Process.Pid)
	r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "zoom", Args: map[string]any{"id": 0, "n": 1}})
	if r.OK || !strings.Contains(r.Error, "refused") {
		t.Fatalf("zoom = %+v", r)
	}
}

func TestAPeerBeforeTheTurnHasStartedIsRefused(t *testing.T) {
	_, _, ts := toolsFixture(t)
	r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "status"})
	if r.OK || !strings.Contains(r.Error, "refused") {
		t.Fatalf("status = %+v", r)
	}
}

// A refused peer that never ends its line is read up to the line cap and no
// further: the server closes, and the peer's writes fail before it has sent
// three times the cap.
func TestARefusedPeerThatNeverEndsItsLineIsReadOnlyToTheCap(t *testing.T) {
	_, _, ts := toolsFixture(t)
	conn, err := net.Dial("unix", ts.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	sent := 0
	_ = conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
	for sent < 3*steward.MaxToolLine {
		n, err := conn.Write(chunk)
		sent += n
		if err != nil {
			break
		}
	}
	if sent >= 3*steward.MaxToolLine {
		t.Fatalf("the server read %d bytes of a line with no end", sent)
	}
}

func TestAChildOfTheTurnsProcessIsServed(t *testing.T) {
	_, _, ts := toolsFixture(t)
	// The turn's process is this test's child; its child (a shell here)
	// dials the socket with this test's own helper binary: the test binary.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperDialsTheTurnSocket")
	cmd.Env = append(os.Environ(), "COPPICE_HELPER_SOCK="+ts.Path())
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ts.Started(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "RESULT=hello there") {
		t.Fatalf("helper output: %s", out.String())
	}
}

// TestHelperDialsTheTurnSocket runs inside the child process of the test
// above: it waits for the parent to name it, then asks for a zoom.
func TestHelperDialsTheTurnSocket(t *testing.T) {
	sock := os.Getenv("COPPICE_HELPER_SOCK")
	if sock == "" {
		t.Skip("a helper of another test")
	}
	// The server learns this process's pid after Start returns; retry.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(steward.ToolRequest{Tool: "zoom", Args: map[string]any{"id": 0, "n": 1}})
		conn.Write(append(b, '\n'))
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		conn.Close()
		var r steward.ToolResponse
		_ = json.Unmarshal(line, &r)
		if r.OK {
			os.Stdout.WriteString("RESULT=" + r.Result + "\n")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never served: %+v", r)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNothingIsServedAfterTheTurn(t *testing.T) {
	_, _, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	path := ts.Path()
	ts.Close()
	if _, err := net.DialTimeout("unix", path, time.Second); err == nil {
		t.Fatal("the socket still takes connections")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket file is still there: %v", err)
	}
}

func TestAStaleSocketIsRemoved(t *testing.T) {
	e := newFakeEnv(t)
	s, _ := chatServer(t, e, nil)
	s.StartSteward()
	fc := s.ensureChat()
	dir := s.turnDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "t-7.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close() // the file stays, as after a crash
	if _, err := os.Lstat(stale); err != nil {
		t.Fatal("no stale socket to test with")
	}
	ts, err := s.openTurnSocket(fc, 7)
	if err != nil {
		t.Fatalf("openTurnSocket: %v", err)
	}
	ts.Close()
	// Anything else at the name is not removed.
	if err := os.WriteFile(stale, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openTurnSocket(fc, 7); err == nil || read(stale) != "mine" {
		t.Fatalf("a file was taken for a stale socket: %v", err)
	}
}

// A turn socket path of 100 bytes or more is refused before anything is
// made, with words that say why, as the standing socket's is (FR-R-9).
func TestATurnSocketPathOf100BytesOrMoreIsRefused(t *testing.T) {
	e := newFakeEnv(t)
	long := filepath.Join(e.dir, strings.Repeat("x", 110))
	s, _ := chatServer(t, e, func(f *StewardConfig) { f.LogDir = long })
	s.StartSteward()
	fc := s.ensureChat()
	_, err := s.openTurnSocket(fc, 1)
	if err == nil || !strings.Contains(err.Error(), "is too long for a socket. Use a shorter data dir") {
		t.Fatalf("openTurnSocket = %v", err)
	}
	if _, serr := os.Lstat(filepath.Join(long, "turns")); serr == nil {
		t.Fatal("the turn dir was made for a socket that cannot open")
	}
}
