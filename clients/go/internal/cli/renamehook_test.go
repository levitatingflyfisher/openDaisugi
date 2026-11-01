package cli

// The one-time move end to end: an enforce hook that master wrote, with its
// --root on the old dir, and a populated old dir. The old words below are
// read on purpose (names-ok).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func oldEnforceHome(t *testing.T, rootFmt string) (home, data, settings string) {
	t.Helper()
	home = t.TempDir()
	data = filepath.Join(home, ".opendaisugi")
	old := filepath.Join(data, "gate") // names-ok: old dir
	if err := os.MkdirAll(filepath.Join(old, "envelopes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "envelopes", "default.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := strings.ReplaceAll(rootFmt, "{old}", old)
	// The install checks that the program of the hook it keeps can run.
	prog := filepath.Join(home, "bin", "daisugi")
	if err := os.MkdirAll(filepath.Dir(prog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := "DAISUGI_GATE_HOOK=opendaisugi.gate " + prog + " gate check --mode enforce --root " + root + // names-ok: old hook
		" --format claude --verify-timeout 10.0 || exit 2"
	settings = filepath.Join(home, ".claude", "settings.json")
	body := `{"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "` + cmd + `"}]}]}}`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, data, settings
}

func TestInstallPointsAnOldHookAtTheMovedDir(t *testing.T) {
	for _, rootFmt := range []string{"{old}", "'{old}'"} {
		home, data, settings := oldEnforceHome(t, rootFmt)
		if code, out, errs := run(t, home, "", "install", "--yes", "--runtime", "claude"); code != 0 {
			t.Fatalf("%s: install exit %d, %s %s", rootFmt, code, out, errs)
		}
		b, _ := os.ReadFile(settings)
		s := strings.ReplaceAll(string(b), "'", "")
		if !strings.Contains(s, "--root "+data+"/warden ") {
			t.Fatalf("%s: root not moved: %s", rootFmt, s)
		}
		if !strings.Contains(s, "--mode enforce") || strings.Contains(s, "DAISUGI_GATE") { // names-ok
			t.Fatalf("%s: %s", rootFmt, s)
		}
		if _, err := os.Stat(filepath.Join(data, "warden", "envelopes", "default.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(data, "gate")); err == nil { // names-ok
			t.Fatal("the old dir is still there")
		}
		code, out, errs := run(t, home, "", "install", "--yes", "--runtime", "claude")
		if code != 0 || strings.Contains(out+errs, "both") {
			t.Fatalf("%s: second install exit %d, %s %s", rootFmt, code, out, errs)
		}
	}
}

func TestTheOldGateVerbSaysWhyItDenies(t *testing.T) { // names-ok
	code, out, errs := run(t, t.TempDir(), "", "gate", "check", "--mode", "enforce")       // names-ok
	want := "daisugi gate is now daisugi warden; run daisugi install to update the hook\n" // names-ok
	if code != 2 || out != "" || errs != want {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}
