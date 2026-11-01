package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/envdefault"
)

// The default envelope goes through the real tree: tree check, tree root,
// the edge proof of tree run's spawn, and the warden sprig asks. The deny
// invariants survive into the child envelope the spawn registers: a fake
// sprig sends its warden the payload real sprig sends (Bash, input cmd).
// fenced runs sprig under the fence, as coppice does with the warden on.
func runDefaultEnvelope(t *testing.T, envFor func(ws string) []byte, fenced bool) {
	t.Helper()
	bin := fenceBin(t)
	w := newWorld(t)
	env := envFor(w.ws)
	work := filepath.Join(w.base, "work")
	root := filepath.Join(work, "warden")
	envFile := filepath.Join(w.base, "env.json")
	if err := os.WriteFile(envFile, env, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, extra []string, args ...string) (string, string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = w.ws
		cmd.Env = append(w.env(), extra...)
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
		killStarted(t, se.String())
		return so.String(), se.String(), code
	}
	if so, se, code := run("", nil, "tree", "check", envFile, envFile); code != 0 {
		t.Fatalf("tree check: %d %s %s", code, so, se)
	}
	if so, se, code := run("", nil, "tree", "root", envFile, "--session", "project-x", "--data-dir", work, "--root", root); code != 0 {
		t.Fatalf("tree root: %d %s %s", code, so, se)
	}
	sprig := filepath.Join(w.ws, ".bin", "sprig")
	script := `#!/bin/sh
warden=
while [ $# -gt 0 ]; do
  case "$1" in --warden-cmd) warden=$2; shift;; esac; shift
done
ask() {
  printf '{"tool_name":"Bash","tool_input":{"cmd":"%s"},"cwd":"` + w.ws + `"}' "$1" > ` + w.ws + `/p.json
  if sh -c "$warden < ` + w.ws + `/p.json" >/dev/null 2>` + w.ws + `/why; then printf allow; else printf 'deny(%s)' "$(grep -o 'deny_[a-z_]*' ` + w.ws + `/why | head -n 1)"; fi
}
printf '{"answer": "push=%s status=%s cap=%s", "turns": 1}' "$(ask 'git push origin main')" "$(ask 'git status')" "$SPRIG_STEP_TIMEOUT_S"
`
	if err := os.MkdirAll(filepath.Dir(sprig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sprig, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	extra := []string{"DAISUGI_SPRIG=" + sprig}
	args := []string{"tree", "run", "--parent", "project-x", "--profile=default", "--data-dir", work, "--root", root}
	if fenced {
		extra = append(extra, `DAISUGI_SPRIG_FENCE=["`+bin+`","fence","--workspace","`+w.ws+`","--warden-root","`+root+`"]`)
		args = append(args, "--sprig-fence")
	}
	req := `{"task":"t","envelope":` + string(env) + `,"tools":["read","bash"]}`
	start := time.Now()
	so, se, code := run(req, extra, append(args, "-")...)
	took := time.Since(start)
	want := `"answer": "push=deny(deny_git_push) status=allow cap=30"`
	if code != 0 || !strings.Contains(so, want) {
		t.Fatalf("tree run (%s): %d\n%s\n%s", took, code, so, se)
	}
	if took > 20*time.Second {
		t.Fatalf("tree run took %s", took)
	}
}

func TestTheDefaultEnvelopeRunsThroughTheTreeAndDeniesPush(t *testing.T) {
	runDefaultEnvelope(t, envdefault.Envelope, false)
}

func TestTheDefaultEnvelopeDeniesPushUnderTheFence(t *testing.T) {
	runDefaultEnvelope(t, envdefault.Envelope, true)
}

// A proposal the steward made gets the denies appended, and runs too.
func TestAProposalWithTheDeniesRunsThroughTheTree(t *testing.T) {
	runDefaultEnvelope(t, func(ws string) []byte {
		in := []byte(`{"generated_by":"steward","task":"build","permissions":{"file_read":["` + ws + `/**"],` +
			`"file_write":["` + ws + `/**"],"shell":true,"shell_allowlist":["git","flutter"]}}`)
		out, err := envdefault.WithDeny(in, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}, false)
}
