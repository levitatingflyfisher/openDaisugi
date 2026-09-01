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

	_ "github.com/opendaisugi/coppice/internal/adapters/pi"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// TestPaneHelperReport is not a test on its own. A fake headless harness
// starts this test binary with COPPICE_HELPER_REPORT set to the line to
// send. It sends it on COPPICE_SOCK with no hello and prints the reply.
func TestPaneHelperReport(t *testing.T) {
	line := os.Getenv("COPPICE_HELPER_REPORT")
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
}

// A child of a headless harness cannot report state for another pane,
// whatever source it claims, so it cannot clear that pane's question and
// then type the answer.
func TestAHeadlessHarnessChildCannotReportForAnotherPane(t *testing.T) {
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

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "reply.txt")
	fake := filepath.Join(dir, "fake-pi.sh")
	script := "#!/bin/sh\n\"$HELPER\" -test.run='^TestPaneHelperReport$' > \"$OUT\" 2>&1 &\nexec cat\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_PI_BIN", fake)

	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", dir, 1),
		gateBlockedLine("w1:p1"))
	if !got[0].OK || !got[1].OK {
		t.Fatalf("setup: %+v", got)
	}
	clear := `{"id":"r","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"ts":1,"session_id":"s",` +
		`"harness":"claude","pane":"w1:p1","state":"idle","source":"gate"}}`
	create := fmt.Sprintf(`{"id":"2","cmd":"pane.create","cwd":%q,"kind":"headless","harness":"pi",`+
		`"env":{"HELPER":%q,"OUT":%q,"COPPICE_HELPER_REPORT":%q}}`, dir, self, out, clear)
	if got := roundTrip(t, s, create); !got[0].OK {
		t.Fatalf("headless pane.create: %+v", got[0].Error)
	}
	deadline := time.Now().Add(10 * time.Second)
	var text string
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(out)
		text = string(b)
		if strings.Contains(text, "REPLY") && strings.Contains(text, "}") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(text, "unauthorized") || !strings.Contains(text, "cannot allow") {
		t.Fatalf("the child's report answered %q", text)
	}
	m := result(t, roundTrip(t, s, `{"id":"3","cmd":"agent.get","pane":"w1:p1"}`)[0])
	if m["state"] != "blocked" {
		t.Fatalf("the other pane reads %v, want blocked", m["state"])
	}
}

// A pane process the server cannot name reports nothing about any pane.
func TestAnUnnamedPaneReportsForNoPane(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true})
	defer stop()
	send(`{"id":"1","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"ts":1,"session_id":"s",` +
		`"harness":"claude","pane":"w1:p1","state":"idle","source":"gate"}}`)
	refusedWith(t, next())
	send(`{"id":"2","cmd":"pane.report_child","pane":"w1:p1","child":"a1","state":"working"}`)
	refusedWith(t, next())
}
