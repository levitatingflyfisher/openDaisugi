//go:build linux

package server

import (
	"strings"
	"testing"
	"time"
)

// A pane's pid places a peer in the pane only while it is that pane's
// process: not once the process ended, and not when the kernel gave the
// pid to a new process, told by its start time.
func TestAPanePidPlacesOnlyThePanesOwnProcess(t *testing.T) {
	s := newAgentServer(t)
	got := roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	id := result(t, got[0])["pane"].(string)
	lp, ok := s.Live(id)
	if !ok {
		t.Fatal("no live pane")
	}
	pid := lp.PTY.Pid()
	if s.panePIDs()[pid] != id {
		t.Fatalf("pid %d is not placed in %s", pid, id)
	}
	if lp.ptyStart == 0 {
		t.Fatal("no start time was kept for the pane's process")
	}
	// A pid with another start time is another process.
	lp.ptyStart++
	if _, placed := s.panePIDs()[pid]; placed {
		t.Fatal("a process with another start time was placed in the pane")
	}
	lp.ptyStart--

	got = roundTrip(t, s, `{"id":"2","cmd":"pane.create","cwd":"`+t.TempDir()+
		`","cmd_argv":["sh","-c","exit 0"],"kind":"pty"}`)
	ended := result(t, got[0])["pane"].(string)
	elp, ok := s.Live(ended)
	if !ok {
		return // already gone, so nothing places it
	}
	epid := elp.PTY.Pid()
	deadline := time.Now().Add(5 * time.Second)
	for !elp.ptyGone() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, placed := s.panePIDs()[epid]; placed {
		t.Fatal("a pane whose process ended still places its pid")
	}
}
