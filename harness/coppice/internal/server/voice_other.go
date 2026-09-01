//go:build !linux

package server

import "os/exec"

// setVoiceProcAttrs does nothing here. Only Linux can tie a child's life
// to its parent, so a server killed hard can leave the voice process
// running.
func setVoiceProcAttrs(cmd *exec.Cmd) {}
