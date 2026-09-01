package gate

import (
	"os/exec"
	"syscall"
)

// A child with a deadline can start processes of its own: git runs an
// alias or a helper, a verifier client runs a shell. A kill of the child's
// pid alone leaves those running, and they can hold the child's pipes open
// or hang for ever (a git call blocked on a fifo in the repo's config did,
// for a day). So each such child starts in a process group of its own, and
// the whole group is killed.
//
// inGroup makes cmd start in a new process group and die with the thread
// that started it (the kernel sends SIGKILL). That covers the case where
// this process is itself killed while it waits: nothing is left to kill
// the group then. For a cmd made with a context, the context's cancel
// kills the whole group, so no process in it still holds the child's pipes
// after the deadline. A cmd made without one kills its group itself.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return killGroup(cmd) }
	}
}

// killGroup sends SIGKILL to cmd's process group and to cmd itself. It
// refuses a pid of 1 or less, since kill(-1) and kill(0) reach far more
// than one group. Call it before the child is reaped, so its pid (the
// group's id) cannot yet name another process.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 1 {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Process.Kill()
}
