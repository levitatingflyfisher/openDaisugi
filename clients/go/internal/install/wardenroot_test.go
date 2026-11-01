package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// install._absolute_warden_root: the hook bakes the root, and a relative one
// would resolve against the agent's workspace. Python raises ValueError.
func TestARelativeWardenRootIsRefusedWithPythonsMessage(t *testing.T) {
	want := "the warden root '../dd/warden' is not an absolute path; set HOME, OPENDAISUGI_HOME or XDG_DATA_HOME to an absolute path"
	err := CheckWardenRoot("../dd/warden")
	var g *WardenRootError
	if !errors.As(err, &g) || g.Msg != want {
		t.Fatalf("CheckWardenRoot = %v, want a WardenRootError %q", err, want)
	}
	if err := CheckWardenRoot("/abs/warden"); err != nil {
		t.Fatalf("absolute root refused: %v", err)
	}
}

func TestPlanInstallRefusesARelativeWardenRootForClaudeAndCodex(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	for _, rt := range Runtimes[:2] {
		_, err := PlanInstall(rt, home, "/bin/daisugi", "rel/warden", Opts{})
		var g *WardenRootError
		if !errors.As(err, &g) {
			t.Errorf("%s: err = %v, want a WardenRootError", rt.Key, err)
		}
	}
}
