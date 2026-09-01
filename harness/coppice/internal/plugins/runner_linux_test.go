//go:build linux

package plugins

import (
	"os/exec"
	"testing"
	"time"
)

// killSession finds its targets by session. A pid that names no session,
// such as a reaped policy's pid now held by another process, is never
// signalled on its own.
func TestKillSessionNeverSignalsABarePid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	killSession(cmd.Process.Pid)
	time.Sleep(100 * time.Millisecond)
	if !alive(cmd.Process.Pid) || zombie(cmd.Process.Pid) {
		t.Fatal("killSession killed a process that leads no session")
	}
}
