package cli

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServer runs `daisugi gate serve --root <dir>/gate` in this process
// (no GateExe: calls are decided in process) and waits for its socket.
func startServer(t *testing.T) string {
	t.Helper()
	// A short directory: a socket path is limited to 107 bytes, and
	// t.TempDir() names the test in it.
	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "gate", "gate.sock")
	var errb bytes.Buffer
	go Main(&Env{Args: []string{"gate", "serve", "--root", filepath.Join(dir, "gate")},
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &errb,
		Environ: []string{"HOME=" + dir, "COPPICE_PANE=w1:p1", "COPPICE_SOCK=/x"}})
	for i := 0; i < 500; i++ {
		if st, err := os.Lstat(sock); err == nil && st.Mode()&os.ModeSocket != 0 && st.Mode().Perm() == 0o600 {
			return sock
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no socket; stderr %q", errb.String())
	return ""
}

func ask(t *testing.T, sock, line string, halfClose bool) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if halfClose {
		_ = c.(*net.UnixConn).CloseWrite()
	}
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestGateServeAnswersOneLinePerConnection(t *testing.T) {
	sock := startServer(t)
	root := filepath.Dir(sock)
	if st, _ := os.Stat(root); st.Mode().Perm() != 0o700 {
		t.Errorf("root mode %v", st.Mode().Perm())
	}
	allow := `{"v": 1, "argv": ["--format", "pi", "--root", "` + root + `"], "stdin_b64": "e30="}` + "\n"
	if got := ask(t, sock, allow, false); got != `{"v": 1, "stdout": "{\"continue\": true}", "stderr": "", "exit_code": 0}`+"\n" {
		t.Errorf("allow: %q", got)
	}
	bad := `{"v": 1, "stdout": "", "stderr": "openDaisugi gate: DENIED ` + "\\u2014" + ` bad request", "exit_code": 2}` + "\n"
	for _, line := range []string{"hello\n", "[1]\n", `{"argv": 5}` + "\n"} {
		if got := ask(t, sock, line, false); got != bad {
			t.Errorf("%q: %q", line, got)
		}
	}
	// A line cut short by a half-close is read as it stands.
	if got := ask(t, sock, `{"argv": [`, true); got != bad {
		t.Errorf("half close: %q", got)
	}
	// --help is SystemExit(0) in the oracle's handler: no reply at all.
	if got := ask(t, sock, `{"argv": ["--help"]}`+"\n", false); got != "" {
		t.Errorf("help: %q", got)
	}
	// The server goes on after each.
	if got := ask(t, sock, allow, false); !strings.Contains(got, `"exit_code": 0`) {
		t.Errorf("after: %q", got)
	}
}

func TestGateServeHookReportNeverReadsItsOwnPane(t *testing.T) {
	sock := startServer(t)
	root := filepath.Dir(sock)
	// {"v":1,"ts":1,"session_id":"s","harness":"pi","state":"idle","source":"gate"}
	line := `{"argv": ["hook", "report", "--root", "` + root + `"], "stdin_b64": ` +
		`"eyJ2IjoxLCJ0cyI6MSwic2Vzc2lvbl9pZCI6InMiLCJoYXJuZXNzIjoicGkiLCJzdGF0ZSI6ImlkbGUiLCJzb3VyY2UiOiJnYXRlIn0="}` + "\n"
	if got := ask(t, sock, line, false); got != `{"v": 1, "stdout": "", "stderr": "", "exit_code": 0}`+"\n" {
		t.Fatalf("report: %q", got)
	}
	tree, err := os.ReadFile(filepath.Join(filepath.Dir(root), "sessions", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The gate source is downgraded, and the server's own COPPICE_PANE
	// is nowhere in what it wrote.
	if !strings.Contains(string(tree), `"source": "headless"`) || strings.Contains(string(tree), "w1:p1") {
		t.Errorf("tree %s", tree)
	}
}

func TestGateServeReplacesAStaleSocketFile(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "gate")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gate.sock"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := listenGate(root, filepath.Join(root, "gate.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	st, err := os.Lstat(filepath.Join(root, "gate.sock"))
	if err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("after close: %v %v", st, err)
	}
	if rs, _ := os.Stat(root); rs.Mode().Perm() != 0o700 {
		t.Errorf("root mode %v", rs.Mode().Perm())
	}
}

func TestProbeGateTellsNothingStaleAndLiveApart(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "g.sock")
	if got := probeGate(p); got != gateNone {
		t.Errorf("nothing: %s", got)
	}
	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	dead.Close()
	if got := probeGate(p); got != gateStale {
		t.Errorf("dead socket: %s", got)
	}
	os.Remove(p)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := probeGate(p); got != gateStale {
		t.Errorf("regular file: %s", got)
	}
	os.Remove(p)
	live, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if got := probeGate(p); got != gateLive {
		t.Errorf("listening: %s", got)
	}
}

func TestGateServeRefusesARootALiveGateServes(t *testing.T) {
	sock := startServer(t)
	st, _ := os.Lstat(sock)
	var errb bytes.Buffer
	code := Main(&Env{Args: []string{"gate", "serve", "--root", filepath.Dir(sock)},
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &errb,
		Environ: []string{"HOME=" + filepath.Dir(filepath.Dir(sock))}})
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	want := "gate: a resident gate already answers on " + sock + "; stop it first\n"
	if errb.String() != want {
		t.Errorf("stderr %q", errb.String())
	}
	if st2, err := os.Lstat(sock); err != nil || !os.SameFile(st, st2) {
		t.Errorf("the first server's socket changed: %v", err)
	}
}
