//go:build linux

package plugins

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setProcAttrs starts the policy in a session of its own, so its pid names
// its session and its process group. The server places a double fork by
// that session, and the runner ends the whole session. The kernel kills the
// policy when the server dies without a stop.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}
}

// killGroup signals the process group that pid leads: terminate, or kill
// when hard is true.
func killGroup(pid int, hard bool) {
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pid, sig)
}

// killSession kills every process in the session that sid names, found by
// scanning /proc, which catches one that left the policy's group. It never
// signals sid on its own: once the policy is reaped, that pid may belong
// to another process.
func killSession(sid int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if sessionOf(pid) == sid {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// sessionOf is the session id of pid, or 0 when it cannot be read.
func sessionOf(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 4 {
		return 0
	}
	n, _ := strconv.Atoi(f[3])
	return n
}
