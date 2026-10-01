package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// GR-R-7 to GR-R-10: a refusal is a third answer beside opened and missing;
// the note ends with the fix; a writer works from a directory fd, so a link
// planted at a name is replaced (the gate's own files) or refused (the
// operator's), never written through. The Python twin is
// tests/test_gate_state_writers.py.

func TestTheNoteEndsWithTheFix(t *testing.T) {
	notes := captureRaw(t)
	root := gateRoot(t)
	f := filepath.Join(root, "DISARMED")
	write(t, f, 0o666)
	fd, refusal := Probe(root, nil, "DISARMED", -1)
	if fd >= 0 {
		t.Fatal("opened")
	}
	want := f + ": is writable by group or others (fix: chmod 600 '" + f + "')"
	if refusal != want {
		t.Fatalf("refusal %q, want %q", refusal, want)
	}
	if got := notes(); got != "daisugi gate: not reading "+want+"\n" {
		t.Fatalf("note %q", got)
	}
}

func TestTheFixForADirectoryAndALink(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	if err := os.Chmod(root, 0o775); err != nil {
		t.Fatal(err)
	}
	_, refusal := Probe(root, nil, "DISARMED", -1)
	if want := root + ": is writable by group or others (fix: chmod 700 '" + root + "')"; refusal != want {
		t.Fatalf("%q", refusal)
	}
	os.Chmod(root, 0o700)
	os.Remove(filepath.Join(root, "envelopes"))
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "envelopes")); err != nil {
		t.Fatal(err)
	}
	_, refusal = Probe(root, []string{"envelopes"}, "default.json", -1)
	e := root + "/envelopes"
	if want := e + ": is a symbolic link (fix: remove the link '" + e + "')"; refusal != want {
		t.Fatalf("%q", refusal)
	}
}

func TestAQuoteInAPathIsQuotedLikeThePythonOne(t *testing.T) {
	capture(t)
	r := filepath.Join(t.TempDir(), "it's")
	if err := os.Mkdir(r, 0o775); err != nil {
		t.Fatal(err)
	}
	os.Chmod(r, 0o775)
	_, refusal := Probe(r, nil, "DISARMED", -1)
	if !strings.HasSuffix(refusal, "(fix: chmod 700 '"+filepath.Dir(r)+"/it'\\''s')") {
		t.Fatalf("%q", refusal)
	}
}

func TestMissingIsNotRefused(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	if fd, refusal := Probe(root, nil, "DISARMED", -1); fd >= 0 || refusal != "" {
		t.Fatal("missing file")
	}
	if fd, refusal := Probe(root, []string{"grafts"}, "a.json", -1); fd >= 0 || refusal != "" {
		t.Fatal("missing dir")
	}
}

func TestTheGatesOwnWriterReplacesALinkAndWritesNothingThroughIt(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	d := filepath.Join(root, "checkpoint-state")
	os.Mkdir(d, 0o700)
	os.Symlink(victim, filepath.Join(d, "s1.json"))
	for n := 0; n < 8; n++ {
		os.Symlink(victim, filepath.Join(d, ".s1.json."+string(rune('a'+n))+".tmp"))
	}
	if _, err := WriteFile(root, []string{"checkpoint-state"}, "s1.json", `{"prompt": "p"}`, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("written through the link: %q", b)
	}
	st, err := os.Lstat(filepath.Join(d, "s1.json"))
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("not replaced by a 0600 file: %v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "s1.json")); string(b) != `{"prompt": "p"}` {
		t.Fatalf("%q", b)
	}
}

func TestAnOperatorWriterRefusesALinkAtTheName(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	os.Symlink(victim, filepath.Join(root, "DISARMED"))
	_, err := WriteFile(root, nil, "DISARMED", "x\n", false)
	var wr *WriteRefused
	if !errors.As(err, &wr) {
		t.Fatalf("err %v", err)
	}
	p := root + "/DISARMED"
	if want := p + ": is a symbolic link (fix: remove the link '" + p + "')"; err.Error() != want {
		t.Fatalf("%q", err.Error())
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("written through")
	}
}

func TestAWriterRefusesASymlinkedDirectory(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	os.Remove(filepath.Join(root, "envelopes"))
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, filepath.Join(root, "envelopes"))
	_, err := WriteFile(root, []string{"envelopes"}, "default.json", "{}", false)
	var wr *WriteRefused
	if !errors.As(err, &wr) {
		t.Fatalf("err %v", err)
	}
	if _, serr := os.Stat(filepath.Join(elsewhere, "default.json")); serr == nil {
		t.Fatal("written through the link")
	}
}

func TestAWriterMakesThingsPrivateWhateverTheUmask(t *testing.T) {
	capture(t)
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	r := filepath.Join(t.TempDir(), "home", "gate")
	if _, err := WriteFile(r, []string{"envelopes"}, "default.json", "{}", false); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(r, "envelopes"), 0o775)
	if _, err := WriteFile(r, []string{"envelopes"}, "default.json", "{}", false); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{r: 0o700, r + "/envelopes": 0o700, r + "/envelopes/default.json": 0o600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
}

func TestVerifyWrittenRaisesForARefusedFile(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	f := filepath.Join(root, "DISARMED")
	write(t, f, 0o666)
	if err := VerifyWritten(root, nil, "DISARMED"); err == nil {
		t.Fatal("a refused file verified")
	}
	os.Chmod(f, 0o600)
	if err := VerifyWritten(root, nil, "DISARMED"); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedPathsListsEachRefusedFile(t *testing.T) {
	capture(t)
	root := gateRoot(t)
	write(t, filepath.Join(root, "DISARMED"), 0o666)
	write(t, filepath.Join(root, "envelopes", "default.json"), 0o600)
	write(t, filepath.Join(root, "envelopes", "s1.json"), 0o664)
	got := RefusedPaths(root)
	want := []string{
		root + "/DISARMED: is writable by group or others (fix: chmod 600 '" + root + "/DISARMED')",
		root + "/envelopes/s1.json: is writable by group or others (fix: chmod 600 '" + root + "/envelopes/s1.json')",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q", got)
	}
	os.Chmod(root, 0o775)
	if got := RefusedPaths(root); len(got) != 1 || !strings.HasPrefix(got[0], root+": is writable") {
		t.Fatalf("%q", got)
	}
}

// GR r1 M2: the gate's own writers never repair a root the reader refuses.
// An operator command repairs the mode and says so.

func TestTheGatesOwnWriterRefusesAGroupWritableRootAndKeepsItsMode(t *testing.T) {
	root := gateRoot(t)
	if err := os.Chmod(root, 0o775); err != nil {
		t.Fatal(err)
	}
	_, err := WriteFile(root, []string{"checkpoint-state"}, "s1.json", "{}", true)
	var wr *WriteRefused
	if !errors.As(err, &wr) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if want := root + ": is writable by group or others (fix: chmod 700 '" + root + "')"; wr.Text != want {
		t.Fatalf("%q", wr.Text)
	}
	if st, _ := os.Stat(root); st.Mode().Perm() != 0o775 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "checkpoint-state")); err == nil {
		t.Fatal("a directory was made in a refused root")
	}
}

func TestTheGatesOwnWriterRefusesAGroupWritableSubdirectory(t *testing.T) {
	root := gateRoot(t)
	d := filepath.Join(root, "asks")
	os.Mkdir(d, 0o700)
	os.Chmod(d, 0o770)
	_, err := WriteFile(root, []string{"asks"}, "a.json", "{}", true)
	var wr *WriteRefused
	if !errors.As(err, &wr) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o770 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestAnOperatorWriterRepairsTheModeAndSaysSo(t *testing.T) {
	notes := captureRaw(t)
	root := gateRoot(t)
	os.Chmod(root, 0o775)
	if _, err := WriteFile(root, nil, "DISARMED", "x\n", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(root); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if got, want := notes(), "daisugi gate: repaired "+root+": it was writable by group or others; now 700\n"; got != want {
		t.Fatalf("%q", got)
	}
}
