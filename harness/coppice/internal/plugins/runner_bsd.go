//go:build unix && !linux

package plugins

import (
	"os/exec"
	"syscall"
)

func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killGroup(pid int, hard bool) {
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pid, sig)
}

// killSession does nothing here. This system has no /proc to find the
// session with, and the group kill in Stop covers a policy not yet reaped.
func killSession(sid int) {}
