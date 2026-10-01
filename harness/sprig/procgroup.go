package sprig

import (
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
)

// toolGroups holds the process group of each bash and gate process sprig
// started that may still have members. KillToolGroups kills them all when
// sprig ends, so nothing a tool left behind outlives sprig (MX-R-8).
var toolGroups struct {
	sync.Mutex
	ids []int
}

// runInGroup starts cmd in a process group of its own, with SIGKILL as
// its parent-death signal, records the group and waits. The OS thread
// stays locked from the start to the wait, since the parent-death signal
// follows the thread that started the child. A group with no member left
// after the wait is forgotten.
func runInGroup(cmd *exec.Cmd) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if err := cmd.Start(); err != nil {
		return err
	}
	pgid := cmd.Process.Pid
	toolGroups.Lock()
	toolGroups.ids = append(toolGroups.ids, pgid)
	toolGroups.Unlock()
	err := cmd.Wait()
	if syscall.Kill(-pgid, 0) != nil {
		toolGroups.Lock()
		for i, g := range toolGroups.ids {
			if g == pgid {
				toolGroups.ids = append(toolGroups.ids[:i], toolGroups.ids[i+1:]...)
				break
			}
		}
		toolGroups.Unlock()
	}
	return err
}

// forgetToolGroup forgets a group that has no member left, and says
// whether it did.
func forgetToolGroup(pgid int) bool {
	if syscall.Kill(-pgid, 0) == nil {
		return false
	}
	toolGroups.Lock()
	defer toolGroups.Unlock()
	for i, g := range toolGroups.ids {
		if g == pgid {
			toolGroups.ids = append(toolGroups.ids[:i], toolGroups.ids[i+1:]...)
			break
		}
	}
	return true
}

// KillToolGroups kills every recorded tool group and forgets them.
func KillToolGroups() {
	toolGroups.Lock()
	defer toolGroups.Unlock()
	for _, g := range toolGroups.ids {
		_ = syscall.Kill(-g, syscall.SIGKILL)
	}
	toolGroups.ids = nil
}

// onSignalKillToolGroups makes SIGTERM, SIGINT and SIGHUP kill the tool
// groups before sprig exits with 128 and the signal's number. The
// returned function undoes it.
func onSignalKillToolGroups() func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	stop := make(chan struct{})
	go func() {
		select {
		case s := <-sigs:
			KillToolGroups()
			os.Exit(128 + int(s.(syscall.Signal)))
		case <-stop:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(stop)
	}
}
