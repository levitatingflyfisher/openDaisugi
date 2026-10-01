package foreman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// I-1: a bad view.json together with a cut node that needs the model must
// not leave an empty view and the whole log as the tail.
func TestBadViewWithACutModelNodeStillRebuildsTheView(t *testing.T) {
	for _, reader := range []bool{false, true} {
		parent := t.TempDir()
		dir, _ := makeLong(t, parent)
		breakNode0(t, dir)
		os.WriteFile(filepath.Join(dir, "view.json"), []byte("[["), fileMode)
		var c *Chat
		var err error
		if reader {
			c, err = OpenReadOnly(dir)
		} else {
			c, err = Open(ctx, Config{Dir: dir, Now: newClock().now})
		}
		if err != nil {
			t.Fatal(err)
		}
		tail, _ := c.Tail()
		if len(c.ViewLines()) != 5 || len(tail) != 0 {
			t.Fatalf("reader=%v: view %d lines, tail %d messages; want 5 and 0", reader, len(c.ViewLines()), len(tail))
		}
		if !reader {
			if _, err := os.Stat(filepath.Join(dir, "view.json")); err != nil {
				t.Fatalf("no view.json was written: %v", err)
			}
			c.Close()
		}
	}
}

// A node that is only pending (never built) stays in the tail word for
// word: the replay does not step past it.
func TestBadViewKeepsAnUnbuiltTailWordForWord(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "foreman")
	c := openT(t, dir, &fakeSummarizer{fail: true}, newClock(), nil)
	for i := 0; i < 4; i++ {
		mustAppend(t, c, KindUser, "short words")
	}
	if _, err := c.Append(ctx, KindUser, strings.Repeat("y", 600)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	os.WriteFile(filepath.Join(dir, "view.json"), []byte("[["), fileMode)
	c2, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	tail, _ := c2.Tail()
	if len(c2.ViewLines()) != 4 || len(tail) != 1 {
		t.Fatalf("view %d lines, tail %d; want 4 and 1", len(c2.ViewLines()), len(tail))
	}
}

// M-1: only a failed save of the rebuilt view.json moves the history. A
// failed tree append has nothing to do with the view.
func TestBadViewAndATreeWriteErrorDoesNotMoveTheHistory(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	tree := filepath.Join(dir, "tree", "2026-10-08.jsonl")
	data, _ := os.ReadFile(tree)
	lines := strings.SplitAfter(string(data), "\n")
	os.WriteFile(tree, []byte(strings.Join(lines[:len(lines)-2], "")), fileMode)
	os.WriteFile(filepath.Join(dir, "view.json"), []byte("[["), fileMode)
	real := openAppend
	openAppend = func(path string) (appendFile, error) { return nil, errInjected }
	defer func() { openAppend = real }()
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err == nil {
		c.Close()
		t.Fatal("Open hid a tree write error")
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 1 {
		t.Fatalf("the history moved: %v", ents)
	}
}

// M-2: a reader that rejects view.json loads the whole log.
func TestReaderWithABadViewCountReadsTheWholeLog(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	os.WriteFile(filepath.Join(dir, "view.json"), []byte(`{"count":0,"batch":false,"lines":[[0,3]]}`+"\n"), fileMode)
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Count() != 5 || len(c.ViewLines()) != 5 {
		t.Fatalf("Count %d, view %d lines; want 5 and 5", c.Count(), len(c.ViewLines()))
	}
}

// M-3: a name that cannot be looked up does not hang freeName.
func TestFreeNameEndsOnALookupError(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- freeName(filepath.Join(t.TempDir(), strings.Repeat("x", 300))) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("freeName looped on a lookup error")
	}
}

// M-4: null, a wrong key case and invalid UTF-8 are bad tree lines, as in
// the Rust port. M-9a: the note counts them in the plural.
func TestStrictTreeLinesAndThePluralNote(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	tree := filepath.Join(dir, "tree", "2026-10-08.jsonl")
	before, _ := os.ReadFile(tree)
	appendTo(t, tree, "null\n"+`{"L":0,"I":3,"text":"x","size":1}`+"\n"+"{\"l\":0,\"i\":3,\"text\":\"\xff\",\"size\":1}\n")
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.Contains(c.Notice(), "3 bad lines in tree/2026-10-08.jsonl were copied to ") {
		t.Fatalf("Notice = %q", c.Notice())
	}
	if after, _ := os.ReadFile(tree); string(after) != string(before) {
		t.Fatalf("the tree changed: %q", after)
	}
}

// M-9a: two bad tree files in one open make one note with both.
func TestTwoBadTreeFilesMakeOneNote(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	appendTo(t, filepath.Join(dir, "tree", "2026-10-08.jsonl"), "{\n")
	os.WriteFile(filepath.Join(dir, "tree", "2026-10-07.jsonl"), []byte("x\n"), fileMode)
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n := c.Notice()
	a, b := strings.Index(n, "1 bad line in tree/2026-10-07.jsonl was copied"), strings.Index(n, "1 bad line in tree/2026-10-08.jsonl was copied")
	if a < 0 || b < a || strings.Count(n, "Foreman tree repaired") != 1 {
		t.Fatalf("Notice = %q", n)
	}
}

// M-6: a torn last tree line gets a .broken copy.
func TestATornTreeTailIsCopiedBeforeItIsCut(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	tree := filepath.Join(dir, "tree", "2026-10-08.jsonl")
	appendTo(t, tree, `{"l":0,"i":`)
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	saved := tree + ".broken-" + stamp
	if got, err := os.ReadFile(saved); err != nil || string(got) != `{"l":0,"i":` {
		t.Fatalf("copy = %q, %v", got, err)
	}
	if !strings.Contains(c.Notice(), "1 torn last line in tree/2026-10-08.jsonl was copied to "+saved+" and removed.") {
		t.Fatalf("Notice = %q", c.Notice())
	}
}

// M-8: an offset inside the last rune is past the end.
func TestZoomOffsetInsideTheLastRuneIsPastTheEnd(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "12345678é")
	if _, err := c.ZoomPage(0, 1, 9, 4); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("ZoomPage = %v", err)
	}
}
