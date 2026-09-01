//go:build linux

package tui

import (
	"os/exec"
	"syscall"
)

// setRecorderProcAttrs has the kernel end the recorder when the floor
// dies without closing, so a killed floor never leaves the microphone
// recording.
func setRecorderProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
