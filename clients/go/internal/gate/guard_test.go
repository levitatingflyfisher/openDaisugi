package gate

import (
	"os"
	"path/filepath"
	"strings"
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
