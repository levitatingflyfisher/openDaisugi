//go:build !linux

package tui

import "os/exec"

// setRecorderProcAttrs does nothing here. Only Linux can tie a child's
// life to its parent, so a floor killed hard can leave the recorder
// running.
func setRecorderProcAttrs(cmd *exec.Cmd) {}
