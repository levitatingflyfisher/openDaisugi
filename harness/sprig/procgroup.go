package sprig

import (
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// toolGroups holds the process group of each bash and warden process sprig
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
	_, err := runInGroupFor(cmd, 0)
	return err
}

// runInGroupFor is runInGroup with a cap: past it the whole group is
// killed, and killed is true. Zero is no cap. Output a process outside the
// group still holds is not waited for past a short delay.
func runInGroupFor(cmd *exec.Cmd, limit time.Duration) (killed bool, err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if limit > 0 && cmd.WaitDelay == 0 {
		cmd.WaitDelay = 2 * time.Second
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	pgid := cmd.Process.Pid
	toolGroups.Lock()
	toolGroups.ids = append(toolGroups.ids, pgid)
	toolGroups.Unlock()
	var fired atomic.Bool
	var t *time.Timer
	if limit > 0 {
		t = time.AfterFunc(limit, func() {
			fired.Store(true)
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		})
	}
	err = cmd.Wait()
	// Stopped at once: a timer that fires after the wait would report a
	// finished call as killed and signal a reaped group (review r9, 7).
	if t != nil && t.Stop() {
		fired.Store(false)
	}
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
	return fired.Load(), err
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
