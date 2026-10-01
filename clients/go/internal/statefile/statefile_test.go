package statefile

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// GR-R-1, GR-R-2: the gate reads its state through no link, and only from
// directories and regular files this user owns that group and other cannot
// write. Each test fails when the check is taken out.

func captureRaw(t *testing.T) func() string {
	t.Helper()
	ResetNotes()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })
	return func() string {
		w.Close()
		os.Stderr = old
		b, _ := io.ReadAll(r)
		return string(b)
	}
}

var fixTail = regexp.MustCompile(` \(fix: .*\)\n`)

// capture is captureRaw less each note's `(fix: ...)` tail: these tests are
// about the reason. The tail is held in writers_test.go.
func capture(t *testing.T) func() string {
	raw := captureRaw(t)
	return func() string { return fixTail.ReplaceAllString(raw(), "\n") }
}

func gateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "gate")
	if err := os.MkdirAll(filepath.Join(root, "envelopes"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, filepath.Join(root, "envelopes")} {
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func write(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func opens(root string, dirs []string, name string) bool {
	fd, ok := Open(root, dirs, name, -1)
	if ok {
		syscall.Close(fd)
	}
	return ok
}

func TestAPlainFileIsOpenedAndSilent(t *testing.T) {
	notes := capture(t)
	root := gateRoot(t)
	write(t, filepath.Join(root, "DISARMED"), 0o600)
	write(t, filepath.Join(root, "envelopes", "default.json"), 0o600)
	if !opens(root, nil, "DISARMED") || !opens(root, []string{"envelopes"}, "default.json") {
		t.Fatal("a plain file was refused")
	}
	if opens(root, nil, "missing") || opens(t.TempDir()+"/nothing", nil, "DISARMED") {
		t.Fatal("a missing file was opened")
	}
	if got := notes(); got != "" {
		t.Fatalf("a missing or plain file left a note: %q", got)
	}
}

func TestARefusalLeavesOneNoteNamingTheFileAndTheReason(t *testing.T) {
	root := gateRoot(t)
	other := t.TempDir()
	write(t, filepath.Join(other, "target"), 0o600)
	cases := []struct {
		name  string
		set   func(p string)
		dirs  []string
		file  string
		where string
		why   string
	}{
		{"symlink", func(p string) { os.Symlink(filepath.Join(other, "target"), p) }, nil, "DISARMED", "DISARMED", "is a symbolic link"},
		{"hard link", func(p string) {
			write(t, p, 0o600)
			os.Link(p, filepath.Join(other, "second"))
		}, nil, "DISARMED", "DISARMED", "has more than one hard link"},
		{"group writable", func(p string) { write(t, p, 0o620) }, nil, "DISARMED", "DISARMED", "is writable by group or others"},
		{"other writable", func(p string) { write(t, p, 0o602) }, []string{"envelopes"}, "default.json", "envelopes/default.json", "is writable by group or others"},
		{"directory", func(p string) { os.Mkdir(p, 0o700) }, nil, "DISARMED", "DISARMED", "is not a regular file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notes := capture(t)
			p := filepath.Join(append([]string{root}, append(c.dirs, c.file)...)...)
			os.Remove(p)
			os.RemoveAll(p)
			c.set(p)
			if opens(root, c.dirs, c.file) {
				t.Fatal("a refused file was opened")
			}
			opens(root, c.dirs, c.file)
			want := "daisugi gate: not reading " + root + "/" + c.where + ": " + c.why + "\n"
			if got := notes(); got != want {
				t.Fatalf("notes = %q, want %q", got, want)
			}
			os.RemoveAll(p)
		})
	}
}

func TestADirectoryIsOpenedThroughNoLink(t *testing.T) {
	root := gateRoot(t)
	real := filepath.Join(filepath.Dir(root), "real-envelopes")
	write(t, filepath.Join(root, "envelopes", "default.json"), 0o600)
	if err := os.Rename(filepath.Join(root, "envelopes"), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "envelopes")); err != nil {
		t.Fatal(err)
	}
	notes := capture(t)
	if opens(root, []string{"envelopes"}, "default.json") {
		t.Fatal("a file below a symlinked directory was opened")
	}
	if got, want := notes(), "daisugi gate: not reading "+root+"/envelopes: is a symbolic link\n"; got != want {
		t.Fatalf("notes = %q, want %q", got, want)
	}
}

func TestASymlinkedRootIsRefusedAndASymlinkedParentIsFollowed(t *testing.T) {
	root := gateRoot(t)
	write(t, filepath.Join(root, "DISARMED"), 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	notes := capture(t)
	if opens(link, nil, "DISARMED") {
		t.Fatal("a file below a symlinked root was opened")
	}
	if got, want := notes(), "daisugi gate: not reading "+link+": is a symbolic link\n"; got != want {
		t.Fatalf("notes = %q, want %q", got, want)
	}
	// A link ABOVE the root is followed (GR-R-2).
	parent := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(filepath.Dir(root), parent); err != nil {
		t.Fatal(err)
	}
	notes = capture(t)
	if !opens(filepath.Join(parent, "gate"), nil, "DISARMED") {
		t.Fatal("a root behind a symlinked parent was refused")
	}
	if got := notes(); got != "" {
		t.Fatalf("note = %q", got)
	}
}

func TestARootOthersCanWriteIsRefused(t *testing.T) {
	root := gateRoot(t)
	write(t, filepath.Join(root, "DISARMED"), 0o600)
	for _, m := range []os.FileMode{0o770, 0o707, 0o777} {
		os.Chmod(root, m)
		notes := capture(t)
		if opens(root, nil, "DISARMED") {
			t.Fatalf("a root of mode %o was read", m)
		}
		if got, want := notes(), "daisugi gate: not reading "+root+": is writable by group or others\n"; got != want {
			t.Fatalf("notes = %q, want %q", got, want)
		}
	}
	os.Chmod(root, 0o700)
}

func TestAFileAnotherUserOwnsIsRefused(t *testing.T) {
	root := gateRoot(t)
	write(t, filepath.Join(root, "DISARMED"), 0o600)
	if !opens(root, nil, "DISARMED") {
		t.Fatal("a plain file was refused")
	}
	was := Geteuid
	Geteuid = func() int { return was() + 1 }
	t.Cleanup(func() { Geteuid = was })
	notes := capture(t)
	if opens(root, nil, "DISARMED") {
		t.Fatal("a file another user owns was opened")
	}
	// The root is the first thing refused, so it is the one named.
	if got, want := notes(), "daisugi gate: not reading "+root+": is owned by another user\n"; got != want {
		t.Fatalf("notes = %q, want %q", got, want)
	}
}

func TestANoteIsGivenOncePerFileAndReason(t *testing.T) {
	root := gateRoot(t)
	write(t, filepath.Join(root, "DISARMED"), 0o666)
	notes := capture(t)
	for i := 0; i < 3; i++ {
		opens(root, nil, "DISARMED")
	}
	if n := strings.Count(notes(), "\n"); n != 1 {
		t.Fatalf("%d notes, want 1", n)
	}
}

func TestAFifoIsRefusedWithoutBlocking(t *testing.T) {
	root := gateRoot(t)
	if err := syscall.Mkfifo(filepath.Join(root, "DISARMED"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := capture(t)
	if opens(root, nil, "DISARMED") {
		t.Fatal("a FIFO was opened")
	}
	if got, want := notes(), "daisugi gate: not reading "+root+"/DISARMED: is not a regular file\n"; got != want {
		t.Fatalf("notes = %q, want %q", got, want)
	}
}

// The flake behind the ask-channel deny test: the open saw no file, a writer
// renamed one into place, and a second look at the name called the plain
// miss a refusal. The error of the open decides: ENOENT is a miss.
func TestEnoentFromTheOpenStandsEvenIfTheNameExistsLater(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, refused := whyNot(-1, filepath.Join(dir, "x.json"), false, syscall.ENOENT); refused {
		t.Fatal("ENOENT from the open must read as missing")
	}
	if _, refused := whyNot(-1, filepath.Join(dir, "x.json"), false, syscall.EACCES); !refused {
		t.Fatal("any other error with the name present is a refusal")
	}
}
