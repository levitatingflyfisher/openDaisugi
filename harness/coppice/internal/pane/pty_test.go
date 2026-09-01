package pane

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// processSurvives polls `pgrep -f marker` for up to one second and reports
// whether a process still matches. Tests build marker from a nanosecond
// timestamp so it cannot collide with something unrelated already running on
// the machine, and so three -count=3 iterations never share a marker.
func processSurvives(marker string) bool {
	deadline := time.Now().Add(time.Second)
	for {
		if err := exec.Command("pgrep", "-f", marker).Run(); err != nil {
			return false // pgrep found no match: the process is gone.
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForProcess polls `pgrep -f marker` for up to two seconds and fails the
// test if no matching process ever appears. A caller that calls Close()
// before the marked process exists proves nothing: Close cannot kill what
// hasn't forked yet, and the test would pass whether or not the group-kill
// actually works.
func waitForProcess(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := exec.Command("pgrep", "-f", marker).Run(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no process matching %q ever started", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func uniqueSleepArg(seconds int) string {
	return fmt.Sprintf("%d.%d", seconds, time.Now().UnixNano())
}

func TestBuildEnvInjectsTheSocketAndPaneID(t *testing.T) {
	got := BuildEnv([]string{"PATH=/bin", "COPPICE_PANE=stale"},
		map[string]string{"FOO": "bar"}, "/run/coppice/server.sock", "w1:p3")
	want := map[string]string{
		"PATH": "/bin", "FOO": "bar",
		"COPPICE_SOCK": "/run/coppice/server.sock", "COPPICE_PANE": "w1:p3",
		"TERM": "xterm-256color",
	}
	seen := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := seen[k]; dup {
			t.Fatalf("env has %s twice: %v", k, got)
		}
		seen[k] = v
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("env[%s] = %q, want %q", k, seen[k], v)
		}
	}
}

// Spec-02 makes the injection unconditional, so an inherited value must be
// removed rather than passed on. A server started from inside a coppice pane
// would otherwise hand its own pane id to every child it spawns, and the gate
// hook in that child would report state for the wrong pane.
func TestBuildEnvScrubsAnInheritedPaneID(t *testing.T) {
	got := BuildEnv([]string{"PATH=/bin", "COPPICE_PANE=w9:p9", "COPPICE_SOCK=/old.sock"},
		nil, "", "")
	for _, kv := range got {
		if strings.HasPrefix(kv, "COPPICE_PANE=") || strings.HasPrefix(kv, "COPPICE_SOCK=") {
			t.Fatalf("env still carries %q, want the inherited value removed", kv)
		}
	}
}

func TestStartPTYRefusesWithoutASocketOrPaneID(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"true"}, Cols: 20, Rows: 4, PaneID: "w1:p1",
	}, g); err == nil {
		t.Fatal("StartPTY accepted an empty socket path, want an error")
	}
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"true"}, Cols: 20, Rows: 4, Sock: "/x.sock",
	}, g); err == nil {
		t.Fatal("StartPTY accepted an empty pane id, want an error")
	}
}

func TestPTYRunsACommandAndTheGridSeesItsOutput(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "echo hi"},
		Cols: 40, Rows: 10, Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not exit within 5 s")
	}
	code, ok := p.ExitCode()
	if !ok || code != 0 {
		t.Fatalf("ExitCode() = %d %v, want 0 true", code, ok)
	}
	// Done() fires the instant the direct child exits, which can be before
	// the separate reader goroutine has drained "hi" into the grid. Wait for
	// Drained() too, bounded, so this doesn't race the grid read below - the
	// same ordering coppice-server's watchExit relies on.
	select {
	case <-p.Drained():
	case <-time.After(time.Second):
	}
	screen, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen, "hi") {
		t.Fatalf("grid = %q, want it to contain hi", screen)
	}
}

func TestPTYReportsANonZeroExitCode(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "exit 7"}, Cols: 20, Rows: 4,
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not exit within 5 s")
	}
	if code, ok := p.ExitCode(); !ok || code != 7 {
		t.Fatalf("ExitCode() = %d %v, want 7 true", code, ok)
	}
}

func TestPTYPassesTheEnvironmentToTheChild(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(60, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "echo pane=$COPPICE_PANE"},
		Cols: 60, Rows: 6, Sock: "/tmp/x.sock", PaneID: "w1:p9",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	<-p.Done()
	time.Sleep(50 * time.Millisecond) // let the reader drain the last bytes
	screen, err := g.Read(ReadVisible)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen, "pane=w1:p9") {
		t.Fatalf("grid = %q, want pane=w1:p9", screen)
	}
}

func TestStartPTYFailsLoudlyOnAMissingBinary(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"coppice-no-such-binary"}, Cols: 20, Rows: 4,
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g); err == nil {
		t.Fatal("StartPTY succeeded for a binary that does not exist, want an error")
	}
}

func TestResizeSetsTheChildWinsize(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Argv: []string{"sh", "-c", "sleep 5"}, Cols: 40, Rows: 10,
		Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	cols, rows := g.Size()
	if cols != 100 || rows != 30 {
		t.Fatalf("grid size after resize = %dx%d, want 100x30", cols, rows)
	}
	// The grid can agree with itself even if Setsize was never called, since
	// g.Size() only reports what Resize told the grid. Reading the pty's own
	// winsize back out is what actually proves Setsize ran.
	ws, err := pty.GetsizeFull(p.f)
	if err != nil {
		t.Fatal(err)
	}
	if int(ws.Cols) != 100 || int(ws.Rows) != 30 {
		t.Fatalf("pty winsize after resize = %dx%d, want 100x30", ws.Cols, ws.Rows)
	}
}

// pty.StartWithSize makes the child a session
// and process-group leader with the pty as its controlling terminal, so the
// master's Read only returns EIO once nothing at all holds the slave open. A
// harness that backgrounds work and exits (`something & exit N`) leaves a
// grandchild holding the slave, and a pump that ties "the child exited" to
// "the master returned EIO" would never notice: Done() would hang forever
// even though the pane's own process is long gone.
//
// The grandchild traps and ignores SIGHUP so it survives the ordinary hangup
// the kernel delivers to the whole foreground process group when the session
// leader (our immediate child) exits. Without that, the session teardown
// alone would kill the grandchild and mask the bug this test exists to catch.
func TestExitIsReportedEvenWhenAGrandchildHoldsThePty(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	marker := uniqueSleepArg(30)
	p, err := StartPTY(SpawnOpts{
		Cwd:  t.TempDir(),
		Argv: []string{"sh", "-c", fmt.Sprintf(`(trap '' HUP; exec sleep %s) & exit 3`, marker)},
		Cols: 40, Rows: 10, Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not fire within 2s: a grandchild holding the pty open must not block exit reporting")
	}
	if code, ok := p.ExitCode(); !ok || code != 3 {
		t.Fatalf("ExitCode() = %d %v, want 3 true", code, ok)
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if processSurvives(marker) {
		t.Fatalf("a grandchild matching %q is still alive a second after Close()", marker)
	}
}

// Close signals the whole process group, not
// just the direct child, so a pane can never leave live work behind it.
//
// The backgrounded bystander ignores SIGHUP for the same reason as in
// TestExitIsReportedEvenWhenAGrandchildHoldsThePty: killing only cmd.Process
// still tears down the session and delivers an ordinary SIGHUP to the rest of
// the (single, non-job-controlled) foreground process group, which would
// kill an ordinary bystander anyway and hide a Close() that reaches nothing
// but the one PID it was given. Only an explicit, group-wide signal reaches a
// bystander that has made itself immune to that cascade.
func TestCloseKillsTheWholeProcessGroup(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	bystander := uniqueSleepArg(41)
	leader := uniqueSleepArg(42)
	p, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(),
		Argv: []string{"sh", "-c", fmt.Sprintf(
			`(trap '' HUP; exec sleep %s) & exec sleep %s`, bystander, leader)},
		Cols: 40, Rows: 10, Sock: "/dev/null", PaneID: "w1:p1",
	}, g)
	if err != nil {
		t.Fatal(err)
	}

	// Close() before the bystander has actually forked would kill nothing but
	// a session that hasn't grown a group-mate yet, and the assertion below
	// would pass vacuously either way. Wait for it to exist first.
	waitForProcess(t, bystander)

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() did not fire within 1s of Close()")
	}
	if processSurvives(bystander) {
		t.Fatalf("the SIGHUP-immune bystander matching %q is still alive after Close(); "+
			"Close must signal the whole process group, not just cmd.Process", bystander)
	}
}

func TestBuildEnvExtraOverridesABaseEntry(t *testing.T) {
	got := BuildEnv([]string{"FOO=old"}, map[string]string{"FOO": "new"}, "/x.sock", "w1:p1")
	found := false
	for _, kv := range got {
		if strings.HasPrefix(kv, "FOO=") {
			found = true
			if kv != "FOO=new" {
				t.Fatalf("env has %q, want FOO=new", kv)
			}
		}
	}
	if !found {
		t.Fatal("env has no FOO entry at all")
	}
}

func TestStartPTYRejectsAnEmptyArgv(t *testing.T) {
	skipWithoutLib(t)
	g, err := NewGrid(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := StartPTY(SpawnOpts{
		Cwd: t.TempDir(), Cols: 20, Rows: 4, Sock: "/x.sock", PaneID: "w1:p1",
	}, g); err == nil {
		t.Fatal("StartPTY accepted an empty Argv, want an error")
	}
}
