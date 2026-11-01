package pando

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func oldChat(t *testing.T, n int) string {
	t.Helper()
	old := filepath.Join(t.TempDir(), "foreman")
	c, err := Open(ctx, Config{Dir: old, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// Each message on its own day, so main/ holds many files.
		if _, err := c.AppendAt(ctx, KindUser, "m", time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	return old
}

// The child half of TestAMigrateKilledMidwayLeavesNoStore: it migrates and
// kills itself after the second file it copies.
func TestMigrateChildProcess(t *testing.T) {
	from, data := os.Getenv("PANDO_MIGRATE_FROM"), os.Getenv("PANDO_MIGRATE_DATA")
	if from == "" {
		t.Skip("run by TestAMigrateKilledMidwayLeavesNoStore")
	}
	n := 0
	migrateHook = func(string) {
		if n++; n == 2 {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
	Migrate(from, data, "work", time.Now())
	t.Fatal("the migrate was not killed")
}

func TestAMigrateKilledMidwayLeavesNoStore(t *testing.T) {
	old := oldChat(t, 4)
	data := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMigrateChildProcess$")
	cmd.Env = append(os.Environ(), "PANDO_MIGRATE_FROM="+old, "PANDO_MIGRATE_DATA="+data)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the child was not killed: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(data, "pando", "work")); !os.IsNotExist(err) {
		t.Fatal("a cut migrate must leave no pando/work")
	}
	tmps, _ := filepath.Glob(filepath.Join(data, "pando", ".work"+MigratingInfix+"*"))
	if len(tmps) != 1 {
		t.Fatalf("the partial copy must stay in its temp dir: %v", tmps)
	}
	if st, err := ListStores(data); err != nil || len(st) != 0 {
		t.Fatalf("the temp dir is not a store: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(old, "main")); err != nil {
		t.Fatal("the old dir must stay where it is")
	}
	// A rerun finishes the move.
	rep, err := Migrate(old, data, "work", time.Now())
	if err != nil || rep.Messages != 4 {
		t.Fatalf("rerun: %+v %v", rep, err)
	}
	dir, _ := StoreDir(data, "work")
	if r, err := OpenReadOnly(dir); err != nil || r.Count() != 4 {
		t.Fatalf("store after rerun: %v", err)
	}
}

func TestAMigrateWhoseCopyFailsItsCheckLeavesNoStore(t *testing.T) {
	old := oldChat(t, 3)
	data := t.TempDir()
	migrateHook = func(stage string) {
		// The copy loses a day file after it is written.
		if strings.HasPrefix(stage, "main/") {
			ms, _ := filepath.Glob(filepath.Join(data, "pando", ".work"+MigratingInfix+"*", "main", "*"))
			if len(ms) > 0 {
				os.Remove(ms[0])
			}
		}
	}
	defer func() { migrateHook = nil }()
	_, err := Migrate(old, data, "work", time.Now())
	if err == nil || !strings.Contains(err.Error(), MigratingInfix) {
		t.Fatalf("the error must name the temp dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(data, "pando", "work")); !os.IsNotExist(err) {
		t.Fatal("a failed migrate must leave no pando/work")
	}
	if _, err := os.Stat(filepath.Join(old, "main")); err != nil {
		t.Fatal("the old dir must stay")
	}
}

// A writer that holds the new store open while the migrate copies makes the
// last rename fail; the store it wrote is not touched.
func TestAMigrateNeverWritesOverAStoreMadeDuringTheCopy(t *testing.T) {
	old := oldChat(t, 2)
	data := t.TempDir()
	dir, _ := StoreDir(data, "work")
	var other *Chat
	migrateHook = func(string) {
		if other == nil {
			c, err := Open(ctx, Config{Dir: dir})
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			c.Append(ctx, KindUser, "written meanwhile")
			other = c
		}
	}
	defer func() { migrateHook = nil }()
	_, err := Migrate(old, data, "work", time.Now())
	if other != nil {
		defer other.Close()
	}
	if err == nil {
		t.Fatal("the migrate must fail")
	}
	r, rerr := OpenReadOnly(dir)
	if rerr != nil || r.Count() != 1 {
		t.Fatalf("the store written meanwhile must stay as it is: %v", rerr)
	}
	if _, err := os.Stat(filepath.Join(old, "main")); err != nil {
		t.Fatal("the old dir must stay")
	}
}
