package supervise

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	return len(f) > 0 && f[0] != "Z"
}

func gone(t *testing.T, pids ...int) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		any := false
		for _, p := range pids {
			any = any || alive(p)
		}
		if !any {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	t.Fatalf("still running: %v", pids)
}

// killGroups sends SIGTERM, then SIGKILL to what is left; never group 1
// or its own.
func TestKillGroupsSignalsThenKills(t *testing.T) {
	stubborn := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 0.05; done")
	stubborn.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	plain := exec.Command("sleep", "60")
	plain.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, c := range []*exec.Cmd{stubborn, plain} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	killGroups([]int{stubborn.Process.Pid, plain.Process.Pid, 1, syscall.Getpgrp()})
	for c, want := range map[*exec.Cmd]syscall.Signal{stubborn: syscall.SIGKILL, plain: syscall.SIGTERM} {
		_ = c.Wait()
		ws := c.ProcessState.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != want {
			t.Fatalf("%v: %v", c.Args, ws)
		}
	}
}

func pidsIn(t *testing.T, path string) []int {
	b, _ := os.ReadFile(path)
	var out []int
	for _, f := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

// Every exit kills the child's group: a process it left in the group dies.
func TestAChildThatExitsLeavesNothingInItsGroup(t *testing.T) {
	pids := t.TempDir() + "/pids"
	script := "sleep 300 </dev/null >/dev/null 2>&1 & echo $! > " + pids + "; echo '{}'"
	var started int
	rc, out, _, timedOut, big, err := runGroupFds("sh", []string{"-c", script}, t.TempDir(), os.Environ(), nil, 10, nil,
		func(pid int) { started = pid }, nil)
	if err != nil || rc != 0 || timedOut || big || strings.TrimSpace(string(out)) != "{}" || started == 0 {
		t.Fatalf("rc %d out %q timeout %v big %v err %v started %d", rc, out, timedOut, big, err, started)
	}
	gone(t, pidsIn(t, pids)...)
}

// A timeout kills the recorded groups below first, then the child's.
func TestATimeoutKillsTheBranchGroups(t *testing.T) {
	other := exec.Command("sleep", "300")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = other.Wait() }()
	_, _, _, timedOut, _, err := runGroupFds("sh", []string{"-c", "sleep 300"}, t.TempDir(), os.Environ(), nil, 1, nil,
		nil, func() []int { return []int{other.Process.Pid} })
	if err != nil || !timedOut {
		t.Fatalf("timeout %v err %v", timedOut, err)
	}
	gone(t, other.Process.Pid)
}

// Output past 16 MiB stops the child.
func TestOutputPast16MiBIsTooBig(t *testing.T) {
	_, _, _, _, big, err := runGroupFds("sh", []string{"-c", "head -c 17000000 /dev/zero; sleep 300"}, t.TempDir(),
		os.Environ(), nil, 30, nil, nil, nil)
	if err != nil || !big {
		t.Fatalf("big %v err %v", big, err)
	}
}
