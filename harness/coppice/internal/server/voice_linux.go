//go:build linux

package server

import (
	"os/exec"
	"syscall"
)

// setVoiceProcAttrs has the kernel end the voice process when the server
// dies without a stop, so no orphan keeps its loopback port.
func setVoiceProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
