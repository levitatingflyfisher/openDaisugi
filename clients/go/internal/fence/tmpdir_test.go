package fence

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// N-5: each run gets a fresh leaf, never a name an earlier run used.
func TestOwnTmpdirIsFreshEachRun(t *testing.T) {
	env := map[string]string{"XDG_DATA_HOME": t.TempDir()}
	a, b := OwnTmpdir(env, "/h", 42), OwnTmpdir(env, "/h", 42)
	if a == b {
		t.Fatalf("two runs of pid 42 share %s", a)
	}
	if !strings.HasPrefix(filepath.Base(a), "42-") {
		t.Fatalf("the leaf %s does not start with the pid", a)
	}
}

func TestMakeTmpdirRefusesALeafThatIsThere(t *testing.T) {
	d := filepath.Join(t.TempDir(), "fence-tmp", "7-old")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "left"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakeTmpdir(d, false); err == nil {
		t.Fatal("an existing leaf was reused")
	}
}

func TestMakeTmpdirSweepsTheLeavesOfDeadRuns(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "fence-tmp")
	dead := filepath.Join(parent, "999999999-ab")
	live := filepath.Join(parent, strconv.Itoa(os.Getpid())+"-cd")
	for _, d := range []string{dead, live} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := MakeTmpdir(filepath.Join(parent, strconv.Itoa(os.Getpid())+"-ef"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("the dead run's leaf stayed")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a live run's leaf went")
	}
}
