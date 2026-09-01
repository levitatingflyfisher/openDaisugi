package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gate check decides in a child process of the daisugi binary: an allow
// comes back as the gate's own answer, and a child that ends abnormally
// is a deny in the host's contract, never an allow.
func TestGateCheckDeniesAnAbnormalChildEnd(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "daisugi")
	if out, err := exec.Command("go", "build", "-tags", "netgo", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := filepath.Join(t.TempDir(), "data", "gate")
	if err := os.MkdirAll(filepath.Join(root, "envelopes"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := `{"generated_by":"t","task":"t","permissions":{"file_read":["/**"]}}`
	if err := os.WriteFile(filepath.Join(root, "envelopes", "default.json"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}`
	run := func(format, crash string) (string, string, int) {
		cmd := exec.Command(bin, "gate", "check", "--mode", "enforce", "--root", root, "--format", format)
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
		return so.String(), se.String(), code
	}
	if so, se, code := run("claude", ""); code != 0 || so != "{\"continue\": true}\n" {
		t.Fatalf("a plain allow: %d %q %q", code, so, se)
	}
	for _, crash := range []string{"exit0", "fatal", "panic"} {
		if _, se, code := run("claude", crash); code != 2 || !strings.Contains(se, "DENIED") {
			t.Errorf("claude, %s: %d %q", crash, code, se)
		}
		so, _, _ := run("hermes", crash)
		var body map[string]any
		if json.Unmarshal([]byte(so), &body) != nil || body["decision"] != "block" {
			t.Errorf("hermes, %s: %q", crash, so)
		}
	}
}

// SIGTERM ends `daisugi gateway --router switchyard` by that signal, as
// the oracle does, but only after the same cleanup its exit runs: the
// Switchyard child is stopped and its state file is removed.
func TestGatewayStoppedBySigtermStopsTheChildAndRemovesItsStateFile(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 for the fake switchyard-server")
	}
	bin := filepath.Join(t.TempDir(), "daisugi")
	if out, err := exec.Command("go", "build", "-tags", "netgo", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fake, err := filepath.Abs("../../../../tests/fixtures/switchyard/switchyard-server")
	if err != nil {
		t.Fatal(err)
	}
	bindir := t.TempDir()
	wrapper := "#!/bin/sh\nexec python3 " + strconv.Quote(fake) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bindir, "switchyard-server"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	data := filepath.Join(home, ".opendaisugi")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "gateway_router: switchyard\nswitchyard_efficient_model: qwen3-coder:30b\n"
	if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	gw, sy := freePort(t), freePort(t)
	cmd := exec.Command(bin, "gateway", "--port", strconv.Itoa(gw), "--switchyard-port", strconv.Itoa(sy),
		"--openai-cheap-model", "")
	cmd.Env = []string{"HOME=" + home, "PATH=" + bindir + ":/usr/bin:/bin"}
	cmd.Dir = home
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready := false
	for i := 0; i < 600 && !ready; i++ {
		if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(gw)); err == nil {
			c.Close()
			ready = true
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	states, _ := filepath.Glob(filepath.Join(data, "gateway", "switchyard-*.json"))
	if !ready || len(states) != 1 {
		t.Fatalf("not ready (%v) or no one state file (%v); stderr=%q", ready, states, se.String())
	}
	var state struct{ PID int }
	if b, err := os.ReadFile(states[0]); err != nil || json.Unmarshal(b, &state) != nil {
		t.Fatalf("state file: %v", err)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	err = cmd.Wait()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("want death by SIGTERM, got %v", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("want death by SIGTERM, got %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(data, "gateway", "switchyard-*.json")); len(left) != 0 {
		t.Fatalf("the state file stayed: %v", left)
	}
	want := "switchyard: sent SIGTERM to switchyard-server, pid " + strconv.Itoa(state.PID)
	if !strings.Contains(so.String(), want) {
		t.Fatalf("stdout lacks %q: %q", want, so.String())
	}
	for i := 0; ; i++ {
		st, err := os.ReadFile("/proc/" + strconv.Itoa(state.PID) + "/stat")
		if err != nil || strings.Contains(string(st[bytes.LastIndexByte(st, ')'):]), ") Z") {
			break
		}
		if i == 100 {
			_ = syscall.Kill(state.PID, syscall.SIGKILL)
			t.Fatal("the switchyard child outlived the gateway")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
