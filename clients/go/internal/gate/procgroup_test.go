package gate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
)

// Every child the gate starts with a deadline runs in a process group of
// its own, and the whole group is killed at the deadline. These tests give
// the child a helper that holds the child's pipes and outlives it, which a
// kill of the child's pid alone never reaches. The helper is a sleep with a
// unique duration, so /proc can find it.

// marker is a sleep duration no other process on the box uses.
func marker() string {
	return fmt.Sprintf("97.%09d", time.Now().UnixNano()%1_000_000_000)
}

// livePids is every live (not zombie) process whose argv contains s.
func livePids(s string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmd, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !strings.Contains(string(cmd), s) {
			continue
		}
		st, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if i := strings.LastIndexByte(string(st), ')'); i >= 0 && strings.HasPrefix(string(st[i:]), ") Z") {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// requireGone waits up to two seconds for every process naming s to end
// (a parent-death signal is sent asynchronously), and kills survivors so
// a failing run leaves nothing behind.
func requireGone(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if len(livePids(s)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	left := livePids(s)
	for _, p := range left {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	t.Fatalf("processes from the call outlived it: %v", left)
}

func killLeftovers(t *testing.T, s string) {
	t.Cleanup(func() {
		for _, p := range livePids(s) {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return repo
}

func testRunner() *runner {
	return &runner{env: map[string]string{"HOME": "/home/user", "PATH": "/usr/bin:/bin"}, t0: time.Now()}
}

// A git call past its deadline ends at once, and so does everything git
// started. The repo's alias runs a sleep that holds git's stdout.
func TestAGitCallPastItsDeadlineLeavesNothingBehind(t *testing.T) {
	repo := gitRepo(t)
	m := marker()
	killLeftovers(t, m)
	if out, err := exec.Command("git", "-C", repo, "config", "alias.slow", "!sleep "+m).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t0 := time.Now()
	if _, ok := testRunner().git(repo, time.Now().Add(300*time.Millisecond), nil, "slow"); ok {
		t.Fatal("a git call past its deadline succeeded")
	}
	if took := time.Since(t0); took > 3*time.Second {
		t.Errorf("took %s", took)
	}
	requireGone(t, m)
}

// TestHelperGitBlocksOnAFifo is not a test on its own: the next test runs
// the test binary with it selected, as a gate child whose git call hangs.
func TestHelperGitBlocksOnAFifo(t *testing.T) {
	repo := os.Getenv("DAISUGI_TEST_FIFO_REPO")
	if repo == "" {
		t.Skip("a helper for TestAGateKilledMidGitTakesItsGitWithIt")
	}
	testRunner().git(repo, time.Now().Add(time.Minute), nil, "rev-parse", "--is-inside-work-tree")
}

// A repo whose config includes a fifo with no writer makes git block for
// ever. When the gate child is killed while that git call runs (the
// guarded parent does this at its own deadline), git must die with it.
func TestAGateKilledMidGitTakesItsGitWithIt(t *testing.T) {
	repo := gitRepo(t)
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("no mkfifo")
	}
	fifo := filepath.Join(t.TempDir(), "include.cfg")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "config", "include.path", fifo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	killLeftovers(t, repo)
	helper := exec.Command(os.Args[0], "-test.run=^TestHelperGitBlocksOnAFifo$")
	helper.Env = append(os.Environ(), "DAISUGI_TEST_FIFO_REPO="+repo)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	started := false
	for i := 0; i < 250 && !started; i++ {
		for _, p := range livePids(repo) {
			if p != helper.Process.Pid {
				started = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = helper.Process.Kill()
	_ = helper.Wait()
	if !started {
		t.Fatal("the helper's git call never started")
	}
	requireGone(t, repo)
}

// The guarded child is killed at the deadline together with everything it
// started.
func TestGuardKillsTheChildsGroupAtTheDeadline(t *testing.T) {
	m := marker()
	killLeftovers(t, m)
	exe := filepath.Join(t.TempDir(), "child.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nsleep "+m+" &\nexec sleep 31\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := RunGuarded(exe, []string{"--format", "claude"}, nil, os.Environ(), 300*time.Millisecond)
	if !res.Crashed || res.Exit != 2 {
		t.Fatalf("got %+v", res)
	}
	requireGone(t, m)
}

// A child that has answered is killed together with everything it
// started.
func TestGuardKillsTheChildsGroupAfterItsFrame(t *testing.T) {
	m := marker()
	killLeftovers(t, m)
	exe := fakeChild(t, allowFrame, "sleep "+m+" &\nexec sleep 31")
	res := RunGuarded(exe, []string{"--format", "claude"}, nil, os.Environ(), 20*time.Second)
	if res.Crashed || res.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	requireGone(t, m)
}

// A verifier client past its timeout ends at once, and so does everything
// it started.
func TestAClientPastItsTimeoutLeavesNothingBehind(t *testing.T) {
	m := marker()
	killLeftovers(t, m)
	exe := filepath.Join(t.TempDir(), "client.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nsleep "+m+" &\nexec sleep 31\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := pyjson.NewObject().Set("id", "c1")
	t0 := time.Now()
	_, why := runClient([]string{exe}, c, 0.3, []string{"PATH=/usr/bin:/bin"})
	if !strings.HasPrefix(why, "timed out") {
		t.Fatalf("got %q", why)
	}
	if took := time.Since(t0); took > 3*time.Second {
		t.Errorf("took %s", took)
	}
	requireGone(t, m)
}

// A herdr report past its deadline is killed together with everything it
// started.
func TestAHerdrReportPastItsDeadlineLeavesNothingBehind(t *testing.T) {
	m := marker()
	killLeftovers(t, m)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	script := "#!/bin/sh\n: > " + started + "\nsleep " + m + " &\nexec sleep 31\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := testRunner()
	r.env["PATH"] = dir + ":/usr/bin:/bin"
	ev := pyjson.NewObject().Set("state", "working").Set("harness", "claude")
	r.sendHerdrReport("p1", ev, time.Now().Add(300*time.Millisecond))
	if _, err := os.Stat(started); err != nil {
		t.Fatal("the fake herdr never ran")
	}
	requireGone(t, m)
}
