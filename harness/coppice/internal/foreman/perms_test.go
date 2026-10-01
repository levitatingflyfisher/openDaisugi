package foreman

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenTightensOldFilesAndMakesDirs0700(t *testing.T) {
	top := t.TempDir()
	dir := filepath.Join(top, "a", "b", "fm")
	if err := os.MkdirAll(filepath.Join(dir, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(dir, "main", "2026-10-08.jsonl")
	line := `{"i":0,"kind":"user","text":"x","size":1,"date":"2026-10-08T23:58:00Z"}` + "\n"
	if err := os.WriteFile(day, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	for _, p := range []string{day, filepath.Join(dir, "lock")} {
		if info, _ := os.Stat(p); info.Mode().Perm() != fileMode {
			t.Errorf("%s has mode %o", p, info.Mode().Perm())
		}
	}

	fresh := filepath.Join(top, "x", "y", "fm")
	openT(t, fresh, &fakeSummarizer{}, newClock(), nil)
	for _, p := range []string{filepath.Join(top, "x"), filepath.Join(top, "x", "y"), fresh} {
		if info, _ := os.Stat(p); info.Mode().Perm() != dirMode {
			t.Errorf("created dir %s has mode %o", p, info.Mode().Perm())
		}
	}
}
