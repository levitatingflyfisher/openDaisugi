//go:build linux

package gatefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func realRoot(t *testing.T) string {
	t.Helper()
	r := filepath.Join(t.TempDir(), "gate")
	if err := os.Mkdir(r, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(r, 0o700)
	return r
}

// GR r1 M4: a trailing slash or a "." part makes the kernel resolve a
// symlink at the last component, so the root is cleaned before the no-follow
// open, as the gate cleans it (str(Path(root))).
func TestARootWithATrailingSlashDoesNotFollowASymlink(t *testing.T) {
	real := realRoot(t)
	link := filepath.Join(t.TempDir(), "gate")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, spelled := range []string{link, link + "/", link + "/.", link + "//", filepath.Dir(link) + "/./gate"} {
		if fds, why := OpenDirs(spelled); fds != nil || why == "" {
			closeAll(fds)
			t.Fatalf("%q: opened a symlinked root (%q)", spelled, why)
		}
		fds, err := MakeDirs(spelled, "answers")
		if err == nil {
			closeAll(fds)
			t.Fatalf("%q: made a directory through a symlinked root", spelled)
		}
		var r *Refused
		if !errors.As(err, &r) {
			t.Fatalf("%q: %v", spelled, err)
		}
	}
	if _, err := os.Stat(filepath.Join(real, "answers")); err == nil {
		t.Fatal("something was made in the link's target")
	}
}

func TestACleanRootIsShownAsTheGateShowsIt(t *testing.T) {
	real := realRoot(t)
	os.Chmod(real, 0o775)
	_, why := OpenDirs(real + "/")
	if want := Refusal(real, "is writable by group or others", true); why != want {
		t.Fatalf("%q, want %q", why, want)
	}
}

// GR r1 M2: coppice makes no directory more private than the gate reads it.
func TestMakeDirsRefusesAGroupWritableRootAndKeepsItsMode(t *testing.T) {
	r := realRoot(t)
	os.Chmod(r, 0o775)
	fds, err := MakeDirs(r, "answers")
	var ref *Refused
	if err == nil || !errors.As(err, &ref) {
		closeAll(fds)
		t.Fatalf("want a refusal, got %v", err)
	}
	if st, _ := os.Stat(r); st.Mode().Perm() != 0o775 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(r, "answers")); err == nil {
		t.Fatal("a directory was made in a refused root")
	}
}
