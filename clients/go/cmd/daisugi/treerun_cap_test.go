package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// treeRunWithFakeSprig makes a root with an envelope whose step cap is 1 s,
// then runs `tree run` with a fake sprig that takes three steps of 0.6 s
// each and reports the step cap it was given. extra are more tree run
// flags. It returns stdout, stderr and the exit code.
func treeRunWithFakeSprig(t *testing.T, extra ...string) (string, string, int) {
	t.Helper()
	bin := fenceBin(t)
	w := newWorld(t)
	work := filepath.Join(w.base, "work")
	root := filepath.Join(work, "warden")
	env := `{"generated_by":"t","task":"t","permissions":{"file_read":["` + w.ws + `/**"],"file_write":["` + w.ws + `/**"],` +
		`"shell":true,"shell_allowlist":["*"],"max_execution_time_s":1}}`
	envFile := filepath.Join(w.base, "env.json")
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, extraEnv []string, args ...string) (string, string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = w.ws
		cmd.Env = append(w.env(), extraEnv...)
		cmd.Stdin = strings.NewReader(stdin)
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		return so.String(), se.String(), code
	}
	if so, se, code := run("", nil, "tree", "root", envFile, "--session", "project-x", "--data-dir", work, "--root", root); code != 0 {
		t.Fatalf("tree root: %d %s %s", code, so, se)
	}
	sprig := filepath.Join(w.base, "sprig")
	script := "#!/bin/sh\nfor i in $(seq ${FAKE_STEPS:-3}); do sleep 0.6; done\n" +
		`printf '{"answer": "cap=%s", "turns": 3}' "$SPRIG_STEP_TIMEOUT_S"` + "\n"
	if err := os.WriteFile(sprig, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	req := `{"task":"t","envelope":` + env + `,"tools":["read","bash"]}`
	args := append([]string{"tree", "run", "--parent", "project-x", "--profile=default", "--data-dir", work, "--root", root}, extra...)
	// The run cap's verdict is the timer's (supervise kill_test.go), so a
	// short fake is enough. This is the run cap only: the kill of one slow
	// step is proven in harness/sprig/tools_test.go.
	steps := "3"
	return run(req, []string{"DAISUGI_SPRIG=" + sprig, "FAKE_STEPS=" + steps}, append(args, "-")...)
}

// The envelope's max_execution_time_s caps each step, not the run: three
// steps of 0.6 s pass under a 1 s cap, and sprig is told the cap
// (ledger 2026-11-06: Trellis worker 9 died at the whole-run cap of 30 s).
func TestTheEnvelopeCapsEachStepNotTheRun(t *testing.T) {
	start := time.Now()
	so, se, code := treeRunWithFakeSprig(t)
	if code != 0 || !strings.Contains(so, `"answer": "cap=1"`) {
		t.Fatalf("tree run: %d\n%s\n%s", code, so, se)
	}
	if time.Since(start) < 1800*time.Millisecond {
		t.Fatal("the run took under the three steps; the test proves nothing")
	}
}

// --max-run-s is the run cap, apart from the step cap.
func TestMaxRunSCapsTheWholeRun(t *testing.T) {
	so, se, code := treeRunWithFakeSprig(t, "--max-run-s", "1")
	if code == 0 || !strings.Contains(so, "sprig ran past 1s and was killed") {
		t.Fatalf("tree run: %d\n%s\n%s", code, so, se)
	}
}
