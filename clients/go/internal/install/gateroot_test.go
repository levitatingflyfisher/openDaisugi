package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// install._absolute_gate_root: the hook bakes the root, and a relative one
// would resolve against the agent's workspace. Python raises ValueError.
func TestARelativeGateRootIsRefusedWithPythonsMessage(t *testing.T) {
	want := "the gate root '../dd/gate' is not an absolute path; set HOME, OPENDAISUGI_HOME or XDG_DATA_HOME to an absolute path"
	err := CheckGateRoot("../dd/gate")
	var g *GateRootError
	if !errors.As(err, &g) || g.Msg != want {
		t.Fatalf("CheckGateRoot = %v, want a GateRootError %q", err, want)
	}
	if err := CheckGateRoot("/abs/gate"); err != nil {
		t.Fatalf("absolute root refused: %v", err)
	}
}

func TestPlanInstallRefusesARelativeGateRootForClaudeAndCodex(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	for _, rt := range Runtimes[:2] {
		_, err := PlanInstall(rt, home, "/bin/daisugi", "rel/gate", Opts{Gate: true})
		var g *GateRootError
		if !errors.As(err, &g) {
			t.Errorf("%s: err = %v, want a GateRootError", rt.Key, err)
		}
		if _, err := PlanInstall(rt, home, "/bin/daisugi", "rel/gate", Opts{}); err != nil {
			t.Errorf("%s without --gate: %v", rt.Key, err)
		}
	}
}
