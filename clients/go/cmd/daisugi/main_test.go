package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
