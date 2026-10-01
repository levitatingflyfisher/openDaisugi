package fenceruns

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSessionOK(t *testing.T) {
	for _, s := range []string{"a", "A.b_c-9", string(make([]byte, 0)) + "x"} {
		if !SessionOK(s) {
			t.Errorf("%q should pass", s)
		}
	}
	long := ""
	for i := 0; i < 129; i++ {
		long += "a"
	}
	for _, s := range []string{"", ".", "..", "../x", "a/b", "a b", "é", long} {
		if SessionOK(s) {
			t.Errorf("%q should fail", s)
		}
	}
	if !SessionOK(long[:128]) {
		t.Error("128 chars should pass")
	}
}

func TestNamesAPath(t *testing.T) {
	for _, w := range []string{"--root", "--root=/x", "--ro", "--r=/x", "--captures-root", "--cap", "--c"} {
		if !NamesAPath([]string{"--mode", "enforce", w}) {
			t.Errorf("%q names a path", w)
		}
	}
	for _, w := range []string{"--rootx", "--format", "-r", "--", "--session=--root"} {
		if NamesAPath([]string{w}) {
			t.Errorf("%q names no path", w)
		}
	}
}

func TestStripPathFlags(t *testing.T) {
	got := StripPathFlags([]string{"--mode", "enforce", "--root", "/r", "--captures-root=/c", "--format", "pi"})
	want := []string{"--mode", "enforce", "--format", "pi"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
}

func TestBoundWorkspaceFollowsTheParents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gate")
	ws := t.TempDir()
	pid := os.Getpid()
	if BoundWorkspace(root, &pid) != "" {
		t.Fatal("bound with no record")
	}
	// The parent of this test binary registers: the walk finds it.
	pp := os.Getppid()
	if err := Register(root, pp, ws); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(ws)
	if got := BoundWorkspace(root, &pid); got != real {
		t.Fatalf("got %q want %q", got, real)
	}
	// A record whose start time does not match binds nothing.
	f := filepath.Join(root, RunsDir, itoa(pp)+".json")
	if err := os.WriteFile(f, []byte(`{"pid":`+itoa(pp)+`,"start":"1","workspace":"/"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := BoundWorkspace(root, &pid); got != "" {
		t.Fatalf("a stale record bound %q", got)
	}
}

func TestInsideFollowsLinks(t *testing.T) {
	d := t.TempDir()
	ws := filepath.Join(d, "ws")
	out := filepath.Join(d, "out")
	os.MkdirAll(filepath.Join(ws, "sub"), 0o700)
	os.MkdirAll(out, 0o700)
	os.Symlink(out, filepath.Join(ws, "link"))
	rws, _ := filepath.EvalSymlinks(ws)
	cases := map[string]bool{
		ws: true, filepath.Join(ws, "sub"): true, filepath.Join(ws, "missing", "x"): true,
		out: false, filepath.Join(ws, "link"): false, filepath.Join(ws, "link", "missing"): false,
		filepath.Join(ws, "sub", "..", "..", "out"): false, ws + "x": false,
	}
	for cwd, want := range cases {
		if got := Inside(cwd, rws); got != want {
			t.Errorf("Inside(%q) = %v", cwd, got)
		}
	}
}

func itoa(n int) string {
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// FN-R-29: a resident transcript is read only as a regular file under
// TranscriptRoot; a path outside, a link that leads out and a FIFO are not.
func TestResidentTranscriptOnlyUnderTheProjectsDir(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	projects := filepath.Join(dir, "home", ".claude", "projects")
	os.MkdirAll(filepath.Join(projects, "p"), 0o700)
	old := TranscriptRoot
	TranscriptRoot = func() string { return projects }
	t.Cleanup(func() { TranscriptRoot = old })
	out := filepath.Join(dir, "elsewhere.jsonl")
	os.WriteFile(out, []byte("{}\n"), 0o600)
	if _, ok := ResidentTranscript(out); ok {
		t.Error("a path outside was read")
	}
	link := filepath.Join(projects, "p", "l.jsonl")
	os.Symlink(out, link)
	if _, ok := ResidentTranscript(link); ok {
		t.Error("a link that leads out was read")
	}
	in := filepath.Join(projects, "p", "t.jsonl")
	os.WriteFile(in, []byte("{}\n"), 0o600)
	if _, ok := ResidentTranscript(in); !ok {
		t.Error("the transcript inside was not read")
	}
	fifo := filepath.Join(projects, "p", "f.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, ok := ResidentTranscript(fifo); ok || time.Since(t0) > 3*time.Second {
		t.Error("a FIFO was read or waited on")
	}
}
