package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// world is one fake install: a data dir, a config dir and a socket path
// that no server listens on.
func world(t *testing.T) (data, cfg, sock string) {
	t.Helper()
	data, cfg = t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", t.TempDir())
	return data, filepath.Join(cfg, "coppice"), filepath.Join(t.TempDir(), "s.sock")
}

func TestBackupAndRestoreCarryHistoryAndConfigNotSecrets(t *testing.T) {
	data, cfg, sock := world(t)
	put(t, data, "layout.json", "{}")
	put(t, data, "recent-dirs.json", "[]")
	put(t, data, "foreman/main/2026-10-24.jsonl", "{}\n")
	put(t, data, "foreman/tree/2026-10-24.jsonl", "{}\n")
	put(t, data, "foreman/view.json", "{}")
	put(t, data, "foreman/lock", "")
	put(t, data, "foreman/tools.sock", "")
	put(t, data, "web/token", "SECRET")
	put(t, data, "server.log", "log")
	put(t, cfg, "coppice.toml", "fence = \"auto\"\n")
	put(t, cfg, "pools.toml", "")
	put(t, cfg, "api-key", "SECRET")

	file := filepath.Join(t.TempDir(), "b.tgz")
	code, out, errb := runCLI(t, sock, data, "backup", file)
	if code != 0 || !strings.Contains(out, "7 files") {
		t.Fatalf("backup: %d %q %q", code, out, errb)
	}

	data2, cfg2, sock2 := world(t)
	code, out, errb = runCLI(t, sock2, data2, "restore", file)
	if code != 0 || !strings.Contains(out, "7 files") {
		t.Fatalf("restore: %d %q %q", code, out, errb)
	}
	for _, p := range []string{filepath.Join(data2, "layout.json"), filepath.Join(data2, "foreman/tree/2026-10-24.jsonl"),
		filepath.Join(cfg2, "coppice.toml"), filepath.Join(cfg2, "pools.toml")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	for _, p := range []string{filepath.Join(data2, "web"), filepath.Join(data2, "server.log"),
		filepath.Join(data2, "foreman/lock"), filepath.Join(data2, "foreman/tools.sock"), filepath.Join(cfg2, "api-key")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("restored %s", p)
		}
	}
}

func TestRestoreWithARunningServerSaysSoAndChangesNothing(t *testing.T) {
	data, cfg, _ := world(t)
	put(t, data, "layout.json", "{}")
	file := filepath.Join(t.TempDir(), "b.tgz")
	if code, _, errb := runCLI(t, "/nonexistent.sock", data, "backup", file); code != 0 {
		t.Fatal(errb)
	}
	sock := filepath.Join(t.TempDir(), "live.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	data2 := t.TempDir()
	code, _, errb := runCLI(t, sock, data2, "restore", file)
	if code != 1 || strings.Count(errb, "\n") != 1 || !strings.Contains(errb, "coppice server stop") {
		t.Fatalf("got %d %q", code, errb)
	}
	if ents, _ := os.ReadDir(data2); len(ents) != 0 {
		t.Fatal("restore wrote with a server running")
	}
	_ = cfg
}

func TestRestoreRefusesFullTargetUnlessForce(t *testing.T) {
	data, _, sock := world(t)
	put(t, data, "layout.json", "new")
	file := filepath.Join(t.TempDir(), "b.tgz")
	if code, _, errb := runCLI(t, sock, data, "backup", file); code != 0 {
		t.Fatal(errb)
	}
	put(t, data, "layout.json", "old")
	code, _, errb := runCLI(t, sock, data, "restore", file)
	if code != 1 || !strings.Contains(errb, "--force") {
		t.Fatalf("got %d %q", code, errb)
	}
	code, out, errb := runCLI(t, sock, data, "restore", file, "--force")
	if code != 0 || !strings.Contains(out, ".broken-") {
		t.Fatalf("force: %d %q %q", code, out, errb)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "layout.json")); string(b) != "new" {
		t.Fatalf("layout = %q", b)
	}
}

func TestBackupDefaultNameAndUsage(t *testing.T) {
	data, _, sock := world(t)
	put(t, data, "layout.json", "{}")
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if code, _, errb := runCLI(t, sock, data, "backup"); code != 0 {
		t.Fatal(errb)
	}
	ents, _ := os.ReadDir(work)
	if len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "coppice-backup-") || !strings.HasSuffix(ents[0].Name(), ".tgz") {
		t.Fatalf("default name: %v", ents)
	}
	if code, _, _ := runCLI(t, sock, data, "restore"); code != 1 {
		t.Errorf("restore with no file: %d", code)
	}
}

func TestBackupAndRestoreRefuseRemote(t *testing.T) {
	data, _, sock := world(t)
	put(t, data, "layout.json", "{}")
	for _, verb := range []string{"backup", "restore"} {
		code, _, errb := runCLI(t, sock, data, "--remote", "ssh://example.invalid", verb, filepath.Join(t.TempDir(), "b.tgz"))
		if code != 1 || !strings.Contains(errb, "not wired for --remote") {
			t.Errorf("%s: %d %q", verb, code, errb)
		}
	}
	if ents, _ := os.ReadDir(data); len(ents) != 1 {
		t.Errorf("data dir changed: %v", ents)
	}
}

func TestHelpAndUnknownFlags(t *testing.T) {
	data, _, sock := world(t)
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	for _, args := range [][]string{{"backup", "--help"}, {"backup", "-h"}, {"restore", "--help"}, {"restore", "-h"}} {
		code, out, errb := runCLI(t, sock, data, args...)
		if code != 0 || !strings.Contains(out, "Usage: coppice "+args[0]) || errb != "" {
			t.Errorf("%v: %d %q %q", args, code, out, errb)
		}
	}
	for _, args := range [][]string{{"backup", "--out"}, {"restore", "--frce", "x.tgz"}, {"restore", "-x"}} {
		if code, _, errb := runCLI(t, sock, data, args...); code != 1 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Errorf("wrote files: %v", ents)
	}
}

func TestRestoreSaysWhereItWrote(t *testing.T) {
	data, cfg, sock := world(t)
	put(t, data, "layout.json", "{}")
	put(t, cfg, "coppice.toml", "")
	file := filepath.Join(t.TempDir(), "b.tgz")
	if code, _, errb := runCLI(t, sock, data, "backup", file); code != 0 {
		t.Fatal(errb)
	}
	data2, cfg2, sock2 := world(t)
	code, out, errb := runCLI(t, sock2, data2, "restore", file)
	if code != 0 || !strings.Contains(out, "into "+cfg2+" and "+data2) && !strings.Contains(out, "into "+data2+" and "+cfg2) {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}
