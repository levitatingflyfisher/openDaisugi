package deeds

import (
	"os"
	"path/filepath"
	"testing"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/signing"
)

func handle(t *testing.T, path string, existed bool, prior any, dirs ...string) *Handle {
	ds := []any{}
	for _, d := range dirs {
		ds = append(ds, d)
	}
	h, err := ParseHandle(pyjson.NewObject().Set("kind", "file_write").Set("path", path).
		Set("prior_existed", existed).Set("prior_content", prior).Set("created_dirs", ds))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A write over a file is undone to its prior text, with no temporary
// file left.
func TestAWriteOverAFileIsUndoneToItsPriorText(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "a.txt")
	if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Apply(handle(t, p, true, "old ☃")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	ents, _ := os.ReadDir(d)
	if string(got) != "old ☃" || len(ents) != 1 {
		t.Fatalf("got %q, %d entries", got, len(ents))
	}
}

// A new file and the directories its write made are removed; undoing it
// again finds nothing and is fine.
func TestANewFileAndItsDirectoriesAreRemoved(t *testing.T) {
	d := t.TempDir()
	sub, deep := filepath.Join(d, "s"), filepath.Join(d, "s", "t")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(deep, "x.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Apply(handle(t, p, false, nil, deep, sub)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sub); err == nil {
		t.Fatal("the directories the write made are still there")
	}
	if err := Apply(handle(t, p, false, nil)); err != nil {
		t.Fatal(err)
	}
}

// A directory at the target is an error, as os.unlink raises.
func TestADirectoryAtTheTargetIsAnError(t *testing.T) {
	if err := Apply(handle(t, t.TempDir(), false, nil)); err == nil {
		t.Fatal("unlink of a directory succeeded")
	}
}

// A handle of another kind does not validate; a kind that validates but
// is not file_write is the oracle's ValueError.
func TestAHandleOfAnotherKind(t *testing.T) {
	if _, err := ParseHandle(pyjson.NewObject().Set("kind", "shell").Set("path", "p").Set("prior_existed", false)); err == nil {
		t.Fatal("a shell handle validated")
	}
	err := Apply(&Handle{Kind: "shell", Path: "p"})
	if pe, ok := err.(*signing.PyError); !ok || pe.Type != "ValueError" {
		t.Fatalf("got %v", err)
	}
}

// A report dumps as dataclasses.asdict does.
func TestAReportDumps(t *testing.T) {
	r := &Report{Undone: []string{"/a"}, Skipped: []*pyjson.Object{pyjson.NewObject().Set("reason", "irreversible")}}
	if got := pyjson.Dumps(r.Dump(), false); got != `{"undone": ["/a"], "skipped": [{"reason": "irreversible"}]}` {
		t.Fatal(got)
	}
}
