package server

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

// TestPaneHelperDial is not a test on its own. A pane runs this test binary
// with COPPICE_HELPER_DIAL set, and it sends that line on COPPICE_SOCK and
// prints the reply, so the reply shows on the pane's screen.
func TestPaneHelperDial(t *testing.T) {
	line := os.Getenv("COPPICE_HELPER_DIAL")
	if line == "" {
		t.Skip("run only inside a pane")
	}
	conn, err := net.Dial("unix", os.Getenv("COPPICE_SOCK"))
	if err != nil {
		fmt.Println("DIAL FAILED", err)
		return
	}
	defer conn.Close()
	fmt.Fprintln(conn, line)
	reply, _ := bufio.NewReader(conn).ReadString('\n')
	fmt.Print("REPLY " + reply)
	time.Sleep(10 * time.Second)
}

// A process inside a pane that connects on its own, with no hello, is still
// a pane: the kernel names its parent chain.
func TestAProcessInsideAPaneCannotAllowOverTheSocket(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	if len(sock) > 100 {
		t.Skip("the temp dir is too long for a unix socket path")
	}
	s, err := New(Config{SocketPath: sock, DataDir: dir})
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
	plantAsk(t, s.cfg.GateRoot, "ask-3")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	create := fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","cols":200,"rows":10,`+
		`"cmd_argv":[%q,"-test.run=^TestPaneHelperDial$"],`+
		`"env":{"COPPICE_HELPER_DIAL":"{\"id\":\"a\",\"cmd\":\"agent.allow\",\"pane\":\"w1:p1\",\"ask\":\"ask-3\"}"}}`,
		dir, self)
	got := roundTrip(t, s, create)
	if !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := result(t, roundTrip(t, s, `{"id":"2","cmd":"pane.read","pane":"w1:p1"}`)[0])
		text, _ := m["text"].(string)
		if i := strings.Index(text, "REPLY"); i >= 0 && strings.Contains(text[i:], "}") {
			if !strings.Contains(text, "unauthorized") || !strings.Contains(text, "cannot allow") {
				t.Fatalf("the pane's allow was not refused: %q", text)
			}
			if _, err := os.Stat(filepath.Join(s.cfg.GateRoot, "answers", "ask-3.json")); err == nil {
				t.Fatal("a refused allow wrote an answer")
			}
			return
		}
		if strings.Contains(text, "DIAL FAILED") {
			t.Fatalf("the helper could not dial: %q", text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the helper never printed a reply")
}

// The gate hook in a pane learns the server's data directory from
// COPPICE_DATA_DIR, as an absolute path, whatever the pane's own env says.
func TestAPaneIsToldTheServersDataDir(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	if len(sock) > 100 {
		t.Skip("the temp dir is too long for a unix socket path")
	}
	s, err := New(Config{SocketPath: sock, DataDir: dir})
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
	create := fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","cols":200,"rows":10,`+
		`"cmd_argv":["sh","-c","echo \"DD=[$COPPICE_DATA_DIR]\"; sleep 5"],`+
		`"env":{"COPPICE_DATA_DIR":"/from-the-pane"}}`, dir)
	if got := roundTrip(t, s, create); !got[0].OK {
		t.Fatalf("pane.create: %+v", got[0].Error)
	}
	want := "DD=[" + dir + "]"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := result(t, roundTrip(t, s, `{"id":"2","cmd":"pane.read","pane":"w1:p1"}`)[0])
		text, _ := m["text"].(string)
		if strings.Contains(text, "DD=[") {
			if !strings.Contains(text, want) {
				t.Fatalf("the pane saw %q, want %q", text, want)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the pane never printed its data dir")
}

// A relative --data-dir is made absolute once, so every pane names the
// same directory whatever its own cwd.
func TestARelativeDataDirIsMadeAbsolute(t *testing.T) {
	s, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "s.sock"), DataDir: "rel/data"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.cfg.DataDir) {
		t.Fatalf("DataDir = %q, want an absolute path", s.cfg.DataDir)
	}
}
