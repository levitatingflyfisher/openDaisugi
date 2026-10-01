package cli

import (
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedDB makes a real SQLite file with one row, which a backup copies with
// VACUUM INTO.
func seedDB(t *testing.T, dir, rel, val string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (v TEXT); INSERT INTO t VALUES (?)", val); err != nil {
		t.Fatal(err)
	}
}

func readDB(t *testing.T, p string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+p+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("SELECT v FROM t").Scan(&v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return v
}

func runIn(t *testing.T, home, cwd string, args ...string) (int, string, string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	return run(t, home, "", args...)
}

func TestBackupThenRestoreIntoAFreshHome(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	data := filepath.Join(home, ".opendaisugi")
	seed(t, data, "config.yaml", "matcher_model: lexical\n")
	seedDB(t, data, "pathways.db", "p1")
	seedDB(t, data, "journal/index.db", "idx")
	seed(t, data, "journal/index.db-wal", "torn")
	seed(t, data, "pathways.db-shm", "torn")
	seed(t, data, "gate/envelopes/s.json", "{}")
	seed(t, data, "gate/audit/2026.jsonl", "{}\n")
	seed(t, data, "gate/disarmed", "")
	seed(t, data, "sessions/a.json", "private")
	seed(t, data, "gate/api-token", "SECRET")

	code, out, errs := runIn(t, home, work, "backup")
	if code != 0 || !strings.Contains(out, "5 files") {
		t.Fatalf("backup: %d %q %q", code, out, errs)
	}
	ents, _ := os.ReadDir(work)
	if len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "daisugi-backup-") || !strings.HasSuffix(ents[0].Name(), ".tgz") {
		t.Fatalf("default name: %v", ents)
	}
	file := filepath.Join(work, ents[0].Name())

	fresh := t.TempDir()
	code, out, errs = run(t, fresh, "", "restore", file)
	if code != 0 || !strings.Contains(out, "5 files") {
		t.Fatalf("restore: %d %q %q", code, out, errs)
	}
	nd := filepath.Join(fresh, ".opendaisugi")
	if v := readDB(t, nd+"/journal/index.db"); v != "idx" {
		t.Fatalf("journal index: %q", v)
	}
	if v := readDB(t, nd+"/pathways.db"); v != "p1" {
		t.Fatalf("pathways: %q", v)
	}
	for _, p := range []string{"sessions", "gate/api-token", "gate/disarmed", "journal/index.db-wal", "pathways.db-shm"} {
		if _, err := os.Stat(filepath.Join(nd, p)); err == nil {
			t.Errorf("restored %s", p)
		}
	}
}

func TestRestoreRefusesAFullTargetWithoutForce(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	seed(t, filepath.Join(home, ".opendaisugi"), "config.yaml", "a: 1\n")
	file := filepath.Join(work, "b.tgz")
	if code, _, errs := run(t, home, "", "backup", file); code != 0 {
		t.Fatal(errs)
	}
	code, _, errs := run(t, home, "", "restore", file)
	if code != 1 || !strings.Contains(errs, "--force") {
		t.Fatalf("want refusal: %d %q", code, errs)
	}
	code, out, errs := run(t, home, "", "restore", file, "--force")
	if code != 0 || !strings.Contains(out, ".broken-") {
		t.Fatalf("force: %d %q %q", code, out, errs)
	}
}

func TestBackupUsageErrors(t *testing.T) {
	home := t.TempDir()
	if code, _, _ := run(t, home, "", "restore"); code != 2 {
		t.Errorf("restore with no file: exit %d", code)
	}
	if code, _, errs := run(t, home, "", "backup", filepath.Join(t.TempDir(), "x.tgz")); code != 1 || !strings.Contains(errs, "nothing to back up") {
		t.Errorf("empty home: %d %q", code, errs)
	}
}

func TestRestoreRefusesWhileAGateAnswers(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	data := filepath.Join(home, ".opendaisugi")
	seed(t, data, "config.yaml", "a: 1\n")
	file := filepath.Join(work, "b.tgz")
	if code, _, errs := run(t, home, "", "backup", file); code != 0 {
		t.Fatal(errs)
	}
	fresh := t.TempDir()
	sock := filepath.Join(fresh, ".opendaisugi", "gate", "gate.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	code, _, errs := run(t, fresh, "", "restore", file)
	if code != 1 || !strings.Contains(errs, "Stop agent sessions or run `daisugi gate disarm` first") {
		t.Fatalf("want refusal: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(fresh, ".opendaisugi", "config.yaml")); err == nil {
		t.Fatal("restored under a live gate")
	}
}

func TestBackupSaysHowManyFilesItLeftOut(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	data := filepath.Join(home, ".opendaisugi")
	seed(t, data, "config.yaml", "a: 1\n")
	seed(t, data, "journal/token-thrift.md", "n")
	code, out, errs := run(t, home, "", "backup", filepath.Join(work, "b.tgz"))
	if code != 0 || !strings.Contains(out, "Left out 1 files whose names look like secrets or locks.") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

func TestBackupRefusesASymlinkedJournal(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	data := filepath.Join(home, ".opendaisugi")
	seed(t, data, "config.yaml", "a: 1\n")
	if err := os.Symlink(work, filepath.Join(data, "journal")); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, home, "", "backup", filepath.Join(work, "b.tgz"))
	if code != 1 || !strings.Contains(errs, filepath.Join(data, "journal")+" is a symlink") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestBackupHelpDoesNotOverclaim(t *testing.T) {
	_, out, _ := run(t, t.TempDir(), "", "backup", "--help")
	if !strings.Contains(out, "No key, token or credential files go in. Your chat and logs do, so keep the file as private as the data.") {
		t.Fatalf("%q", out)
	}
}

func TestAFailedRestorePutsTheOldFilesBackAndSaysSo(t *testing.T) {
	home, work, target := t.TempDir(), t.TempDir(), t.TempDir()
	seed(t, filepath.Join(home, ".opendaisugi"), "config.yaml", "new\n")
	seed(t, filepath.Join(home, ".opendaisugi"), "gate/envelopes/s.json", "{}")
	file := filepath.Join(work, "b.tgz")
	if code, _, errs := run(t, home, "", "backup", file); code != 0 {
		t.Fatal(errs)
	}
	nd := filepath.Join(target, ".opendaisugi")
	seed(t, nd, "config.yaml", "old\n")
	// "gate" is a file, so gate/envelopes cannot be put in place.
	seed(t, nd, "gate", "a file")
	code, out, errs := run(t, target, "", "restore", file, "--force")
	if code != 1 || !strings.Contains(errs, "The old data is back in place") || !strings.Contains(errs, ".restore-") || strings.Contains(out, "Moved aside") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(nd, "config.yaml")); string(b) != "old\n" {
		t.Fatalf("config.yaml = %q", b)
	}
}

func TestRestoreMovesAStaleWalAsideAndTheDatabaseOpens(t *testing.T) {
	home, work, target := t.TempDir(), t.TempDir(), t.TempDir()
	seedDB(t, filepath.Join(home, ".opendaisugi"), "pathways.db", "from the archive")
	file := filepath.Join(work, "b.tgz")
	if code, _, errs := run(t, home, "", "backup", file); code != 0 {
		t.Fatal(errs)
	}
	nd := filepath.Join(target, ".opendaisugi")
	seedDB(t, nd, "pathways.db", "old")
	seed(t, nd, "pathways.db-wal", "stale wal from a crash")
	seed(t, nd, "pathways.db-shm", "stale shm")
	code, out, errs := run(t, target, "", "restore", file, "--force")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if v := readDB(t, nd+"/pathways.db"); v != "from the archive" {
		t.Fatalf("rows = %q", v)
	}
	for _, n := range []string{"pathways.db-wal", "pathways.db-shm"} {
		if _, err := os.Stat(filepath.Join(nd, n)); err == nil {
			t.Errorf("%s stayed beside the restored database", n)
		}
		if !strings.Contains(out, n+".broken-") {
			t.Errorf("%s not in the aside list: %q", n, out)
		}
	}
}
