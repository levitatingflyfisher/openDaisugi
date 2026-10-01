package foreman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeCutTwo makes a history whose message 0 is short (a node without the
// model) and message 1 is long (a node that needs the model), then breaks
// both of their leaf lines in the tree.
func makeCutTwo(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "foreman")
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "old words")
	mustAppend(t, c, KindUser, strings.Repeat("x", 600))
	for i := 0; i < 3; i++ {
		mustAppend(t, c, KindUser, "old words")
	}
	c.Close()
	path := filepath.Join(dir, "tree", "2026-10-08.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	cut := 0
	for k, l := range lines {
		if strings.Contains(l, `"l":0,"i":0,`) || strings.Contains(l, `"l":0,"i":1,`) {
			lines[k] = "garbage {\n"
			cut++
		}
	}
	if cut != 2 {
		t.Fatalf("cut %d leaf lines, want 2", cut)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), fileMode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// N-2: an open that fails after a repair must leave the bad view.json in
// place, so the next open sees a bad view again, skips the cut nodes and
// says so. Before: the rename left no view.json, no skip, an empty view.
func TestFailedOpenKeepsTheBadViewForTheNextOpen(t *testing.T) {
	parent := t.TempDir()
	dir := makeCutTwo(t, parent)
	view := filepath.Join(dir, "view.json")
	os.WriteFile(view, []byte("[["), fileMode)
	real := openAppend
	openAppend = func(path string) (appendFile, error) { return nil, errInjected }
	// The disk is full: the tree append and the save of view.json both fail.
	full := func(path string, data []byte) error {
		if filepath.Base(path) == "view.json" {
			return errInjected
		}
		return writeAtomic(path, data)
	}
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now, writeFile: full})
	openAppend = real
	if err == nil {
		c.Close()
		t.Fatal("Open hid a tree write error")
	}
	if b, _ := os.ReadFile(view); string(b) != "[[" {
		t.Fatalf("view.json after the failed open = %q, want the bad file", b)
	}
	c, err = Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tail, _ := c.Tail()
	if len(c.ViewLines()) != 5 || len(tail) != 0 {
		t.Fatalf("view %d lines, tail %d messages; want 5 and 0", len(c.ViewLines()), len(tail))
	}
	if !strings.Contains(c.Notice(), "view.json could not be used") {
		t.Fatalf("Notice = %q", c.Notice())
	}
	if m, _ := filepath.Glob(view + ".broken-*"); len(m) == 0 {
		t.Fatal("no copy of the bad view.json")
	}
}

// N-2: with no view.json at all and a cut model node, the replay still
// steps past the cut node (writer and reader).
func TestMissingViewWithACutModelNodeStillRebuildsTheView(t *testing.T) {
	for _, reader := range []bool{false, true} {
		parent := t.TempDir()
		dir, _ := makeLong(t, parent)
		breakNode0(t, dir)
		os.Remove(filepath.Join(dir, "view.json"))
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
			t.Fatalf("reader=%v: view %d lines, tail %d; want 5 and 0", reader, len(c.ViewLines()), len(tail))
		}
		if !reader {
			c.Close()
		}
	}
}

// N-3: null, an extra key that differs only in case, and a lone surrogate
// escape are bad tree lines, as in the Rust port.
func TestHandEditedTreeLinesAreBadAsInRust(t *testing.T) {
	for _, line := range []string{
		`{"l":null,"i":3,"text":"x","size":1}`,
		`{"l":0,"i":3,"text":"x","size":1,"TEXT":"y"}`,
		"{\"l\":0,\"i\":3,\"text\":\"x\",\"size\":1,\"\u017fize\":2}",
		`{"l":0,"i":3,"text":"\ud800","size":1}`,
		`{"l":0,"i":3,"text":"a\udc00b","size":1}`,
	} {
		if _, ok := parseNode([]byte(line)); ok {
			t.Errorf("parseNode took %s", line)
		}
	}
	for _, line := range []string{
		`{"l":0,"i":3,"text":"😀","size":1}`,
		`{"l":0,"i":3,"text":"a\\ud800","size":1}`,
	} {
		if _, ok := parseNode([]byte(line)); !ok {
			t.Errorf("parseNode refused %s", line)
		}
	}
}
