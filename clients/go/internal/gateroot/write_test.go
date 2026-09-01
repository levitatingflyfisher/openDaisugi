package gateroot

import (
	"os"
	"path/filepath"
	"testing"
)

// The umask is read from /proc, never by setting it.
func TestProcUmask(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(p, []byte("Name:\tx\nUmask:\t0027\nState:\tR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, ok := procUmask(p); !ok || m != 0o027 {
		t.Fatalf("%o %v", m, ok)
	}
	if _, ok := procUmask(filepath.Join(t.TempDir(), "absent")); ok {
		t.Fatal("an absent file read as a umask")
	}
}

// A write replaces the file whole: the old mode is kept, no temporary
// file is left, and through a symlink the target is written and named.
func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if followed, err := WriteFile(p, "new"); err != nil || followed != "" {
		t.Fatal(followed, err)
	}
	st, _ := os.Stat(p)
	raw, _ := os.ReadFile(p)
	if string(raw) != "new" || st.Mode().Perm() != 0o640 {
		t.Fatalf("%q %o", raw, st.Mode().Perm())
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink("settings.json", link); err != nil {
		t.Fatal(err)
	}
	followed, err := WriteFile(link, "via link")
	if err != nil || followed != p {
		t.Fatal(followed, err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	if err := ReplaceFile(link, "own"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ReplaceFile wrote through the symlink")
	}
	if raw, _ := os.ReadFile(p); string(raw) != "via link" {
		t.Fatalf("target changed: %q", raw)
	}
	if _, err := WriteFileMode(filepath.Join(dir, "env.json"), "{}", 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "env.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("%o", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("a temporary file was left: %v", entries)
	}
}
