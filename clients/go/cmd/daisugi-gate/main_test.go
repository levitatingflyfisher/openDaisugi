package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "daisugi-gate")
	out, err := exec.Command("go", "build", "-tags", "netgo", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func gateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data", "gate")
	if err := os.MkdirAll(filepath.Join(root, "envelopes"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := `{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["ls"],
	 "shell_allow_decomposition":true,"file_read":["/**"],"file_write":["/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

type outcome struct {
	stdout, stderr string
	exit           int
}

func runGate(t *testing.T, bin, root, format, crash, payload string) outcome {
	t.Helper()
	cmd := exec.Command(bin, "--mode", "enforce", "--root", root, "--format", format)
	cmd.Env = []string{"HOME=/home/user", "PATH=/usr/bin:/bin"}
	if crash != "" {
		cmd.Env = append(cmd.Env, "DAISUGI_GATE_TEST_CRASH="+crash)
	}
	cmd.Stdin = strings.NewReader(payload)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return outcome{so.String(), se.String(), code}
}

const allowed = `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"},"cwd":"/work"}`

// Every way the child can end abnormally is a deny in the host's own
// contract: exit 2 for claude, a block body for hermes and openclaw.
// A host that sets DAISUGI_GATE_CHILD itself gets the parent, which
// answers the call, never a child that prints no verdict and exits 0.
func TestAChildVariableFromTheHostIsIgnored(t *testing.T) {
	bin := build(t)
	root := gateRoot(t)
	payload := `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"rm -rf /"},"cwd":"/work"}`
	for _, v := range []string{"1", "x"} {
		cmd := exec.Command(bin, "--mode", "enforce", "--root", root, "--format", "claude")
		cmd.Env = []string{"HOME=/home/user", "PATH=/usr/bin:/bin", "DAISUGI_GATE_CHILD=" + v}
		cmd.Stdin = strings.NewReader(payload)
		var se bytes.Buffer
		cmd.Stderr = &se
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 2 || !strings.Contains(se.String(), "DENIED") {
			t.Errorf("%s: %v %q", v, err, se.String())
		}
	}
}

// A process started as a child by mistake (the variable set and fd 3 a
// pipe) writes the format's deny on its own stdout and exit code: its
// verdict, an allow here, goes only to the frame pipe.
func TestAChildReadsAsADenyToAnyHost(t *testing.T) {
	bin := build(t)
	root := gateRoot(t)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	cmd := exec.Command(bin, "--mode", "enforce", "--root", root, "--format", "claude")
	cmd.Env = []string{"HOME=/home/user", "PATH=/usr/bin:/bin", "DAISUGI_GATE_CHILD=n"}
	cmd.Stdin = strings.NewReader(allowed)
	cmd.ExtraFiles = []*os.File{pw}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	pw.Close()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || so.String() != "" || !strings.Contains(se.String(), "DENIED") {
		t.Fatalf("got %v %q %q", err, so.String(), se.String())
	}
}

// A crash writes the deny of the format argparse reads, an abbreviated or
// repeated --format included.
func TestACrashDeniesInTheFormatArgparseReads(t *testing.T) {
	bin := build(t)
	root := gateRoot(t)
	for _, args := range [][]string{{"--form", "hermes"}, {"--format", "claude", "--format", "hermes"}} {
		cmd := exec.Command(bin, append([]string{"--mode", "enforce", "--root", root}, args...)...)
		cmd.Env = []string{"HOME=/home/user", "PATH=/usr/bin:/bin", "DAISUGI_GATE_TEST_CRASH=fatal"}
		cmd.Stdin = strings.NewReader(allowed)
		var so bytes.Buffer
		cmd.Stdout = &so
		_ = cmd.Run()
		var body map[string]any
		if json.Unmarshal(so.Bytes(), &body) != nil || body["decision"] != "block" {
			t.Errorf("%v: %q", args, so.String())
		}
	}
}

// An argv argparse itself refuses (an unrecognized flag) is a DIFFERENT
// escape than a crash after a good parse: --format was never bound to
// anything at all, so this exercises runNative's own argvExit branch, not
// DenyFormat's. Before this fix that branch was exit 2 with EMPTY stdout
// for every format, including hermes/openclaw. Those hosts never read the
// exit code, so the empty body read as allow to them. claude keeps the
// plain exit-2 deny: that format DOES read the exit code, so the same
// empty body is already the correct deny for it.
func TestABadFlagDeniesInTheFormatArgparseReads(t *testing.T) {
	bin := build(t)
	root := gateRoot(t)
	for _, tc := range []struct {
		fmt        string
		wantStdout bool
	}{
		{"hermes", true},
		{"openclaw", true},
		{"claude", false},
	} {
		cmd := exec.Command(bin, "--mode", "enforce", "--root", root, "--format", tc.fmt, "--no-such-flag")
		cmd.Env = []string{"HOME=/home/user", "PATH=/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader(allowed)
		var so bytes.Buffer
		cmd.Stdout = &so
		err := cmd.Run()
		if !tc.wantStdout {
			ee, ok := err.(*exec.ExitError)
			if !ok || ee.ExitCode() != 2 || so.String() != "" {
				t.Errorf("%s: want exit 2, empty stdout; got %v %q", tc.fmt, err, so.String())
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: want exit 0 (the deny is on stdout); got %v", tc.fmt, err)
		}
		var body map[string]any
		if json.Unmarshal(so.Bytes(), &body) != nil {
			t.Fatalf("%s: not JSON: %q", tc.fmt, so.String())
		}
		if body["decision"] != "block" && body["action"] != "block" && body["block"] != true {
			t.Errorf("%s: stdout does not deny: %q", tc.fmt, so.String())
		}
	}
}

// A repo whose local config points include.path at a FIFO with no writer
// blocks every git call that opens config, rev-parse included: verified
// separately with a bare `git rev-parse` against such a repo, which hangs.
// isRepo used a flat timeout outside the checkpoint's own shared deadline
// and ran twice (once to decide a checkpoint is due, once inside
// snapshot), so 2x that timeout could exceed the child's own deadline;
// the parent then denies a call this gate already decided to allow. The
// fix reads isRepo's deadline from the same shared budget every other
// checkpoint git call uses. This is the end-to-end version: the actual
// compiled binary, under the guarded-child model that can turn a timeout
// into a denied allow.
func TestAChildWithAConfigThatBlocksGitStillFinishesAndAllows(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("no mkfifo")
	}
	bin := build(t)
	root := gateRoot(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "include.cfg")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "config", "include.path", fifo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type": "user", "uuid": "u1", "message": {"content": "hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"ls"},"cwd":` +
		strconvQuote(repo) + `,"transcript_path":` + strconvQuote(tr) + `}`
	cmd := exec.Command(bin, "--mode", "enforce", "--root", root, "--checkpoints", "--verify-timeout", "1")
	cmd.Env = []string{"HOME=/home/user", "PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(payload)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the allowed call was denied (%v); stderr=%q elapsed=%s", err, se.String(), elapsed)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("took %s; isRepo's timeout is not sharing the checkpoint's own deadline", elapsed)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAbnormalChildEndsDeny(t *testing.T) {
	bin := build(t)
	root := gateRoot(t)
	if o := runGate(t, bin, root, "claude", "", allowed); o.exit != 0 {
		t.Fatalf("a plain allow: %+v", o)
	}
	for _, crash := range []string{"exit0", "exit1", "fatal", "stack", "panic"} {
		o := runGate(t, bin, root, "claude", crash, allowed)
		if o.exit != 2 || !strings.Contains(o.stderr, "DENIED") {
			t.Errorf("claude, %s: %+v", crash, o)
		}
		for _, format := range []string{"hermes", "openclaw"} {
			o := runGate(t, bin, root, format, crash, allowed)
			var body map[string]any
			if json.Unmarshal([]byte(o.stdout), &body) != nil || (body["decision"] != "block" && body["block"] != true) {
				t.Errorf("%s, %s: %+v", format, crash, o)
			}
		}
	}
}

// A 10,000-link command makes the oracle's decomposer raise
// RecursionError. The first rule to see it that fails closed is the
// pane rule, so the oracle denies with its refusal. The port does the
// same, and quickly.
func TestTenThousandLinkCommandDenies(t *testing.T) {
	bin := build(t)
	root := gateRoot(t)
	cmd := "ls" + strings.Repeat(" && ls", 10000)
	p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": "/work",
		"tool_input": map[string]string{"command": cmd}})
	start := time.Now()
	o := runGate(t, bin, root, "claude", "", string(p))
	if o.exit != 2 || !strings.Contains(o.stderr, "a pane can propose. It cannot allow.") {
		t.Fatalf("got %+v", o)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

// A 30,000-link symlink chain makes the oracle's realpath raise
// RecursionError. The call is denied, never allowed.
func TestThirtyThousandSymlinkChainDenies(t *testing.T) {
	if testing.Short() {
		t.Skip("makes 30,000 symlinks")
	}
	bin := build(t)
	root := gateRoot(t)
	dir := t.TempDir()
	const n = 30000
	for i := 0; i < n; i++ {
		if err := os.Symlink(filepath.Join(dir, "l"+itoa(i+1)), filepath.Join(dir, "l"+itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "l"+itoa(n)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Read", "cwd": "/work",
		"tool_input": map[string]string{"file_path": filepath.Join(dir, "l0")}})
	o := runGate(t, bin, root, "claude", "", string(p))
	if o.exit != 2 {
		t.Fatalf("got %+v", o)
	}
}

func itoa(i int) string {
	b := []byte{}
	for {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
		if i == 0 {
			return string(b)
		}
	}
}
