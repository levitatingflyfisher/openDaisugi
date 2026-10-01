package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first gate server wins, so `daisugi start` gives the server it
// starts the same captures root the fence gives (<gate root>/captures,
// FN-R-35; tests/test_start.py is the oracle's copy).
func TestStartGivesTheResidentGateTheCapturesRootTheFenceGives(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755)
	cwd := filepath.Join(dir, "ws")
	os.MkdirAll(cwd, 0o700)
	data := filepath.Join(dir, "data")
	var got []string
	old := startGateSpawn
	startGateSpawn = func(e *Env, root string, extra ...string) int {
		got = append([]string{"--root", root}, extra...)
		return 0
	}
	t.Cleanup(func() { startGateSpawn = old })
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb}
	e.env = map[string]string{"PATH": bin, "HOME": filepath.Join(dir, "home")}
	e.home = filepath.Join(dir, "home")
	if _, err := e.startSteps(cwd, data, false, false, true, true); err != nil {
		t.Fatalf("start: %v %s", err, errb.String())
	}
	root := filepath.Join(data, "gate")
	want := "--root " + root + " --captures-root " + filepath.Join(root, "captures")
	if strings.Join(got, " ") != want {
		t.Errorf("spawned %q, want %q", got, want)
	}
}
