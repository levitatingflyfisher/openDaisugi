package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// A request that carries timeout_ms - agent.wait,
// agent.prompt --wait, pane.wait_output - must not be capped by the fixed
// doReadDeadline floor when the caller's own --timeout runs longer than it.
// The margin is enough headroom for the server's own timeout reply to win
// the race in the ordinary case, without waiting out a huge --timeout on a
// server that really has gone silent.
func TestReadDeadlineForAddsAMarginOnTopOfTimeoutMs(t *testing.T) {
	orig := doReadDeadline
	doReadDeadline = 5 * time.Second
	defer func() { doReadDeadline = orig }()

	cases := []struct {
		name   string
		params map[string]any
		want   time.Duration
	}{
		{"no timeout_ms falls back to the floor", nil, 5 * time.Second},
		{"timeout_ms as int gets a 10s margin",
			map[string]any{"timeout_ms": 600000}, 600*time.Second + 10*time.Second},
		{"a short timeout_ms still gets the margin, even under the floor",
			map[string]any{"timeout_ms": 1000}, 1*time.Second + 10*time.Second},
		{"a zero timeout_ms falls back to the floor, matching the server's own default",
			map[string]any{"timeout_ms": 0}, 5 * time.Second},
		{"a negative timeout_ms falls back to the floor, matching the server's own default",
			map[string]any{"timeout_ms": -5}, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := readDeadlineFor(c.params); got != c.want {
				t.Fatalf("readDeadlineFor(%v) = %v, want %v", c.params, got, c.want)
			}
		})
	}
}

func TestClientDoRefusesReservedParamKeys(t *testing.T) {
	// Autostart is off: this test must fail the dial it is testing, not
	// spawn a background server against a temp socket nobody will clean up.
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	sock, dir := liveServer(t)
	c, err := Dial(sock, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Do("pane.list", map[string]any{"cmd": "sneaky"}); err == nil {
		t.Fatal(`Do accepted params["cmd"], want a refusal`)
	}
	if _, err := c.Do("pane.list", map[string]any{"id": "sneaky"}); err == nil {
		t.Fatal(`Do accepted params["id"], want a refusal`)
	}
}

func TestBackgroundServerArgvCarriesSocketAndDataDir(t *testing.T) {
	argv := backgroundArgv("/usr/bin/coppice", "/tmp/s.sock", "/tmp/data")
	want := []string{"/usr/bin/coppice", "--socket", "/tmp/s.sock", "--data-dir", "/tmp/data",
		"server", "start", "--foreground"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("backgroundArgv = %v, want %v", argv, want)
	}
}

func TestLogPathIsUnderDataDir(t *testing.T) {
	got := logPath("/tmp/data")
	if got != "/tmp/data/server.log" {
		t.Fatalf("logPath = %q, want /tmp/data/server.log", got)
	}
}

func TestDaemonEnvDropsPaneAndSocketButKeepsXDGAndBinOverrides(t *testing.T) {
	in := []string{
		"COPPICE_PANE=w1:p1",
		"COPPICE_SOCK=/x/old.sock",
		"XDG_RUNTIME_DIR=/run/1000",
		"XDG_CONFIG_HOME=/home/x/.config",
		"COPPICE_CLAUDE_BIN=/opt/claude",
		"HOME=/home/x",
	}
	out := daemonEnv(in)
	for _, want := range []string{
		"XDG_RUNTIME_DIR=/run/1000", "XDG_CONFIG_HOME=/home/x/.config",
		"COPPICE_CLAUDE_BIN=/opt/claude", "HOME=/home/x",
	} {
		found := false
		for _, kv := range out {
			if kv == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("daemonEnv(%v) = %v, missing %q", in, out, want)
		}
	}
	for _, kv := range out {
		if strings.HasPrefix(kv, "COPPICE_PANE=") || strings.HasPrefix(kv, "COPPICE_SOCK=") {
			t.Fatalf("daemonEnv(%v) = %v, must not carry %q", in, out, kv)
		}
	}
}

// A window resize must not be able to bring up a whole detached daemon: if
// the server died mid-attach, the next SIGWINCH must skip the resize, not
// autostart a replacement server nobody asked for.
func TestDialExistingNeverAutostarts(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "absent.sock")
	if _, err := dialExisting(sock, 0); err == nil {
		t.Fatal("dialExisting connected to a socket that was never created")
	}
	if _, err := os.Stat(filepath.Join(dir, "layout.json")); err == nil {
		t.Fatal("dialExisting autostarted a server")
	}
}

// A connection that dies after a successful dial is a transport failure, not
// a user error: the caller cannot tell it apart from no_such_pane otherwise.
func TestATransportFailureMidCallExitsThreeNotOne(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // read the request
		_ = conn.Close()      // then vanish with no reply at all
	}()

	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	code, _, errb := runCLI(t, sock, dir, "pane", "list")
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, errb)
	}
	if !strings.Contains(errb, "the server closed the connection") {
		t.Fatalf("error %q does not explain the transport failure", errb)
	}
}

// Do must actually use the derived deadline, not just
// compute it. doReadDeadline is shrunk well below the reply's own delay; a
// request carrying timeout_ms survives that delay only if Do extended its
// read deadline past the shrunk floor.
func TestDoWaitsPastTheShrunkFloorWhenTheRequestCarriesATimeout(t *testing.T) {
	orig := doReadDeadline
	doReadDeadline = 50 * time.Millisecond
	defer func() { doReadDeadline = orig }()

	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		dec := proto.NewDecoder(conn)
		req, err := dec.Next()
		if err != nil {
			return
		}
		var r struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req, &r)
		// Well past doReadDeadline's shrunk 50ms, well under the derived
		// deadline (a 300ms timeout_ms plus the 10s margin).
		time.Sleep(300 * time.Millisecond)
		enc := proto.NewEncoder(conn)
		_ = enc.Send(map[string]any{"id": r.ID, "ok": true, "result": map[string]any{}})
	}()

	c, err := dialExisting(sock, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Do("pane.wait_output", map[string]any{"pane": "w1:p1", "timeout_ms": 300}); err != nil {
		t.Fatalf("Do returned %v, want the derived deadline to have covered the reply's own delay", err)
	}
}

// Client.Do must not hang forever waiting for a server that accepts a
// connection and then goes silent. doReadDeadline is a var so the test can
// shrink it instead of waiting out the real, generous default.
func TestDoTimesOutRatherThanHangingForever(t *testing.T) {
	orig := doReadDeadline
	doReadDeadline = 200 * time.Millisecond
	defer func() { doReadDeadline = orig }()

	dir := t.TempDir()
	sock := filepath.Join(dir, "server.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		<-stop // hold the connection open, but never answer
	}()

	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	code, _, errb := runCLI(t, sock, dir, "pane", "list")
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, errb)
	}
	if !strings.Contains(errb, "the server closed the connection") {
		t.Fatalf("error %q does not name the timeout", errb)
	}
}

// A --remote target that fails must show the ssh child's own stderr, not a
// bare EOF: os/exec sends a child's stderr to /dev/null unless the caller
// captures it, and ssh only ever reports "unknown host" or similar to
// stderr, never to the exit code check that runs before the process even
// starts.
func TestRemoteTransportFailureNamesTheSSHStderr(t *testing.T) {
	dir := t.TempDir()
	fakeSSH := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\necho 'Could not resolve hostname nonexistent-host' >&2\nexit 1\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COPPICE_NO_AUTOSTART", "1")

	code, _, errb := runCLI(t, "/nonexistent.sock", t.TempDir(),
		"--remote", "ssh://nonexistent-host", "pane", "list")
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, errb)
	}
	if !strings.Contains(errb, "Could not resolve hostname") {
		t.Fatalf("error %q does not carry the ssh stderr", errb)
	}
}
