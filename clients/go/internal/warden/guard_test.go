package warden

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeChild writes a script that plays the child: it writes its pid to
// a file "pid" beside itself, writes body (with its nonce put in for
// NONCE) to the frame pipe, closes it, then runs after.
func fakeChild(t *testing.T, body, after string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "child.sh")
	script := "#!/bin/sh\necho $$ > \"$(dirname \"$0\")/pid\"\nprintf '%s' \"$(printf '%s' '" + body + "' | sed \"s/NONCE/$" + ChildEnv + "/\")\" >&3\nexec 3>&-\n" + after + "\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe
}

const allowFrame = `{"stdout":"e30K","stderr":"","exit":0,"native":true,"why":"","done":true,"nonce":"NONCE"}`

// A complete frame is the answer as soon as the pipe closes, even when
// the child then hangs, and the child does not outlive the call.
func TestCompleteFrameAnswersAtOnce(t *testing.T) {
	exe := fakeChild(t, allowFrame, "exec sleep 30")
	pidFile := filepath.Join(filepath.Dir(exe), "pid")
	t0 := time.Now()
	res := RunGuarded(exe, []string{"--format", "claude"}, nil, os.Environ(), 20*time.Second)
	if took := time.Since(t0); took > 5*time.Second {
		t.Fatalf("waited %s for a child that had answered", took)
	}
	if res.Crashed || res.Exit != 0 || res.Stdout != "{}\n" || !res.Native {
		t.Fatalf("got %+v", res)
	}
	// The kill is sent before the answer comes back; give it a moment to land.
	for i := 0; i < 100; i++ {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid := strings.TrimSpace(string(b))
			st, err := os.ReadFile("/proc/" + pid + "/stat")
			if err != nil || strings.Contains(string(st), ") Z ") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the child is still running after its frame was taken")
}

// A frame with the wrong nonce, or cut short, is no answer: the parent
// waits for the child and denies.
func TestIncompleteFrameIsADeny(t *testing.T) {
	for _, body := range []string{
		`{"stdout":"","stderr":"","exit":0,"native":true,"why":"","done":true,"nonce":"other"}`,
		`{"stdout":"","stderr":"","exit":0,"native":true,`,
		`{"stdout":"","stderr":"","exit":0,"native":true,"why":"","done":false,"nonce":"NONCE"}`,
	} {
		res := RunGuarded(fakeChild(t, body, "exit 0"), []string{"--format", "claude"}, nil, os.Environ(), 20*time.Second)
		if !res.Crashed || res.Exit != 2 {
			t.Errorf("%s: got %+v", body, res)
		}
	}
}

// A child that answers nothing before the deadline is killed and denied.
func TestNoFrameByTheDeadlineIsADeny(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "child.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	res := RunGuarded(exe, []string{"--format", "claude"}, nil, os.Environ(), 300*time.Millisecond)
	if took := time.Since(t0); took > 5*time.Second {
		t.Fatalf("took %s", took)
	}
	if !res.Crashed || res.Exit != 2 || !strings.Contains(res.Why, "no answer within") {
		t.Fatalf("got %+v", res)
	}
}

// An interrupt or a terminate while the child works is a deny with exit 2,
// not the parent's death by signal (a host reads that as no verdict). The
// child is killed.
func TestASignalDuringACheckIsADeny(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		res := signalDuringACheck(t, sig, "enforce")
		if !res.Crashed || res.Exit != 2 || !strings.Contains(res.Why, "signal") {
			t.Fatalf("%v: got %+v", sig, res)
		}
	}
}

// In audit mode the same signal gives the format's allow body and exit 0: an
// audit warden never blocks. The mode is the one the child would read.
func TestASignalDuringACheckInAuditModeAllows(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		res := signalDuringACheck(t, sig, "audit")
		if res.Crashed || res.Exit != 0 || res.Stdout != "{\"continue\": true}\n" || !strings.Contains(res.Why, "signal") {
			t.Fatalf("%v: got %+v", sig, res)
		}
	}
}

// signalDuringACheck holds a child that never answers and signals this process
// once the child is running (its pid file exists), so no clock decides it.
func signalDuringACheck(t *testing.T, sig syscall.Signal, mode string) Result {
	t.Helper()
	return signalRun(t, sig, []string{"--mode", mode}, nil, nil)
}

// signalRun is signalDuringACheck with the warden flags after --format claude
// and --root <dir>/root given by the caller. The config file of that root is
// <dir>/config.yaml. extraEnv is added to the environment; setup(dir) runs
// first.
func signalRun(t *testing.T, sig syscall.Signal, flags, extraEnv []string, setup func(dir string)) Result {
	t.Helper()
	dir := t.TempDir()
	if setup != nil {
		setup(dir)
	}
	exe := filepath.Join(dir, "child.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho $$ > \"$(dirname \"$0\")/pid\"\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // the parent is in its select by now
		_ = syscall.Kill(os.Getpid(), sig)
	}()
	t0 := time.Now()
	argv := append(append([]string{"--format", "claude"}, flags...), "--root", filepath.Join(dir, "root"))
	res := RunGuarded(exe, argv, nil, append(os.Environ(), extraEnv...), 20*time.Second)
	if took := time.Since(t0); took > 5*time.Second {
		t.Fatalf("%v: took %s", sig, took)
	}
	return res
}

// No --mode and warden_mode: enforce in the root's config: enforce, so a
// signal denies with exit 2.
func TestASignalWithNoModeFlagAndConfigEnforceDenies(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		res := signalRun(t, sig, nil, nil, func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("warden_mode: enforce\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if !res.Crashed || res.Exit != 2 || !strings.Contains(res.Why, "signal") {
			t.Fatalf("%v: got %+v", sig, res)
		}
	}
}

// No --mode and no config: the default is audit, so a signal allows.
func TestASignalWithNoModeFlagAndNoConfigAllows(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		res := signalRun(t, sig, nil, nil, nil)
		if res.Crashed || res.Exit != 0 || res.Stdout != "{\"continue\": true}\n" || !strings.Contains(res.Why, "signal") {
			t.Fatalf("%v: got %+v", sig, res)
		}
	}
}

// Enforce from config and the calling pane's "watching" marker: the hook is
// audit, so a signal allows where enforce would deny.
func TestASignalInAWatchedPaneAllows(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		res := signalRun(t, sig, nil, []string{"COPPICE_PANE=w1:p1", "COPPICE_SOCK=" + sockA}, func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("warden_mode: enforce\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeMarker(t, filepath.Join(dir, "root"), sockA, "w1:p1", "watching\n")
		})
		if res.Crashed || res.Exit != 0 || res.Stdout != "{\"continue\": true}\n" || !strings.Contains(res.Why, "signal") {
			t.Fatalf("%v: got %+v", sig, res)
		}
	}
}

// ModeFor reads the mode with the child's functions: --mode wins; an argv it
// cannot read reads as enforce unless --mode says audit.
func TestModeFor(t *testing.T) {
	dir := t.TempDir()
	env := []string{"HOME=" + dir, "OPENDAISUGI_DATA_HOME=" + dir}
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"--mode", "audit", "--root", dir + "/r"}, "audit"},
		{[]string{"--mode", "enforce", "--root", dir + "/r"}, "enforce"},
		{[]string{"--mode=audit", "--root", dir + "/r"}, "audit"},
		{[]string{"--mode", "bogus"}, "enforce"},
		{[]string{"--bogus-flag", "--mode", "audit"}, "audit"},
		{[]string{"--bogus-flag"}, "enforce"},
		{[]string{"--root", dir + "/r"}, "audit"}, // no flag, no config
	} {
		if got := ModeFor(c.argv, env); got != c.want {
			t.Errorf("%v: got %q, want %q", c.argv, got, c.want)
		}
	}
}

// The three cases of the mode a call is checked in, apart from --mode: the
// config's warden_mode, the default with no config, and the watching marker.
func TestModeForFollowsConfigThenMarker(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	env := []string{"HOME=" + dir}
	argv := []string{"--root", root}
	if got := ModeFor(argv, env); got != "audit" {
		t.Fatalf("no flag, no config: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("warden_mode: enforce\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ModeFor(argv, env); got != "enforce" {
		t.Fatalf("no flag, config enforce: %q", got)
	}
	if got := ModeFor(append([]string{"--mode", "audit"}, argv...), env); got != "audit" {
		t.Fatalf("flag audit over config enforce: %q", got)
	}
	writeMarker(t, root, sockA, "w1:p1", "watching\n")
	watched := append([]string{"COPPICE_PANE=w1:p1", "COPPICE_SOCK=" + sockA}, env...)
	if got := ModeFor(argv, watched); got != "audit" {
		t.Fatalf("watched pane: %q", got)
	}
	other := append([]string{"COPPICE_PANE=w1:p2", "COPPICE_SOCK=" + sockA}, env...)
	if got := ModeFor(argv, other); got != "enforce" {
		t.Fatalf("another pane: %q", got)
	}
}
