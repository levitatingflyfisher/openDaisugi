package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first warden server wins, so `daisugi start` gives the server it
// starts the same captures root the fence gives (<warden root>/captures,
// FN-R-35; tests/test_start.py is the oracle's copy).
func TestStartGivesTheResidentWardenTheCapturesRootTheFenceGives(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755)
	cwd := filepath.Join(dir, "ws")
	os.MkdirAll(cwd, 0o700)
	data := filepath.Join(dir, "data")
	var got []string
	old := startWardenSpawn
	startWardenSpawn = func(e *Env, root string, extra ...string) int {
		got = append([]string{"--root", root}, extra...)
		return 0
	}
	t.Cleanup(func() { startWardenSpawn = old })
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb}
	e.env = map[string]string{"PATH": bin, "HOME": filepath.Join(dir, "home")}
	e.home = filepath.Join(dir, "home")
	if _, err := e.startSteps(cwd, data, false, false, true, true); err != nil {
		t.Fatalf("start: %v %s", err, errb.String())
	}
	root := filepath.Join(data, "warden")
	want := "--root " + root + " --captures-root " + filepath.Join(root, "captures")
	if strings.Join(got, " ") != want {
		t.Errorf("spawned %q, want %q", got, want)
	}
}

// install._absolute_warden_root: `start --data-dir ../dd` is refused with one
// line, exit 2, before any write: .claude and the data dir do not exist.
func TestStartRefusesARelativeWardenRoot(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755)
	cwd := filepath.Join(dir, "ws")
	os.MkdirAll(cwd, 0o700)
	old := startWardenSpawn
	spawned := false
	startWardenSpawn = func(e *Env, root string, extra ...string) int { spawned = true; return 0 }
	t.Cleanup(func() { startWardenSpawn = old })
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb}
	e.env = map[string]string{"PATH": bin, "HOME": filepath.Join(dir, "home")}
	e.home = filepath.Join(dir, "home")
	_, err := e.startSteps(cwd, "../dd", false, false, true, true)
	var x *exitError
	if !errors.As(err, &x) || x.code != 2 {
		t.Fatalf("err = %v, want exit 2", err)
	}
	want := "daisugi start: the warden root '../dd/warden' is not an absolute path; " +
		"set HOME, OPENDAISUGI_HOME or XDG_DATA_HOME to an absolute path. Nothing was changed.\n"
	if errb.String() != want {
		t.Errorf("stderr %q, want %q", errb.String(), want)
	}
	if spawned {
		t.Error("a warden server was started")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude")); err == nil {
		t.Error(".claude was made")
	}
	if _, err := os.Stat(filepath.Join(dir, "dd")); err == nil {
		t.Error("the data dir was made")
	}
}
