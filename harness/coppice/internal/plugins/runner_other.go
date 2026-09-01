//go:build !unix

package plugins

import (
	"os"
	"os/exec"
)

func setProcAttrs(cmd *exec.Cmd) {}

func killGroup(pid int, hard bool) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func killSession(sid int) {}
