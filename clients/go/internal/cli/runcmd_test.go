package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/verify"
)

// A Z3 check that does not finish in the whole-plan verify rejects the
// run (fail closed): nothing runs, and the run is journaled with the z3
// violation.
func TestRunZ3UnknownFailsClosed(t *testing.T) {
	old := verifyPlan
	defer func() { verifyPlan = old }()
	verifyPlan = func(verify.ActionPlan, verify.Envelope, verify.VerifyOptions) verify.VerifyResultGo {
		return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 returned unknown"}}
	}
	home := t.TempDir()
	env := filepath.Join(home, "e.yaml")
	plan := filepath.Join(home, "p.yaml")
	marker := filepath.Join(home, "ran")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(env, []byte("generated_by: t\ntask: t\npermissions:\n  shell: true\n  shell_allowlist:\n  - touch\n"), 0o644))
	must(os.WriteFile(plan, []byte("source: t\ntask: t\nsteps:\n- id: s1\n  type: shell\n  command: touch "+marker+"\n"), 0o644))
	code, out, errs := run(t, home, "", "run", plan, "-e", env, "--yes")
	if code != 2 || !strings.Contains(out, "(rejected)") || !strings.Contains(errs, "z3: Z3 returned unknown") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the step ran")
	}
	traces, _ := filepath.Glob(filepath.Join(home, ".opendaisugi", "journal", "traces", "*.yaml"))
	if len(traces) != 1 {
		t.Fatalf("traces %v", traces)
	}
	body, _ := os.ReadFile(traces[0])
	if !strings.Contains(string(body), "stage: z3") || !strings.Contains(string(body), "status: rejected") {
		t.Fatal(string(body))
	}
}
