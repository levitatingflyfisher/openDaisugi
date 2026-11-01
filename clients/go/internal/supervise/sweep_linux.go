//go:build linux

package supervise

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

const (
	prSetChildSubreaper = 36
	prGetChildSubreaper = 37
)

// child is a child process of this one: its pid and its start time, so a
// pid used again is not taken for one seen before.
type child struct {
	pid   int
	start string
}

// children is this process's children now (tree_branch._children).
func children() map[child]bool {
	me := []byte(strconv.Itoa(os.Getpid()))
	out := map[child]bool{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(data, ')')
		if i < 0 || i+2 > len(data) {
			continue
		}
		rest := bytes.Fields(data[i+2:])
		if len(rest) > 19 && bytes.Equal(rest[1], me) {
			out[child{pid, string(rest[19])}] = true
		}
	}
	return out
}

// sweep is tree_branch._Sweep: it stops every process a branch child leaves
// behind, before the parent reads a worktree. The child's own process group
// is killed when it exits, but a process that made a new session is not in
// that group. So the tree run makes itself the child subreaper while the
// children run: every process they leave, at any depth, becomes a child of
// this process when its parent ends.
type sweep struct {
	on     bool
	was    int32
	before map[child]bool
}

func newSweep() *sweep {
	s := &sweep{}
	var was int32
	_, _, _ = syscall.RawSyscall6(syscall.SYS_PRCTL, prGetChildSubreaper, uintptr(unsafe.Pointer(&was)), 0, 0, 0, 0)
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0, 0, 0, 0); e != 0 {
		return s
	}
	s.on, s.was, s.before = true, was, children()
	return s
}

// clear stops and reaps what the children left: SIGTERM first, so a server
// (a resident warden the child's fence started) can shut down cleanly, then
// SIGKILL after KillGraceS. False when something is still there after that
// and SweepS more.
func (s *sweep) clear() bool {
	if !s.on {
		return true
	}
	termUntil := time.Now().Add(KillGraceS)
	end := termUntil.Add(SweepS)
	termed := map[int]bool{}
	ok := false
	for {
		var left []int
		for c := range children() {
			if !s.before[c] {
				left = append(left, c.pid)
			}
		}
		if len(left) == 0 {
			ok = true
			break
		}
		now := time.Now()
		if now.After(end) {
			break
		}
		for _, pid := range left {
			if now.Before(termUntil) {
				if !termed[pid] {
					_ = syscall.Kill(pid, syscall.SIGTERM)
					termed[pid] = true
				}
			} else {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		for _, pid := range left {
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, _ = syscall.RawSyscall6(syscall.SYS_PRCTL, prSetChildSubreaper, uintptr(s.was), 0, 0, 0, 0)
	return ok
}
