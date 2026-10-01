package layout

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Saves that wait behind one write share the next one (CP-R-53). Each save
// still returns only when the file holds a tree at least as new as the
// change before it, but a burst of changes costs one write, not one per
// change: on a slow disk, a write each made pane.attach wait behind every
// other save, past the attach's 5 s ack.
func TestSavesThatWaitTogetherShareOneWrite(t *testing.T) {
	tr := New()
	ws := tr.CreateWorkspace("w", "/w")
	tab, err := tr.CreateTab(ws.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var ids []string
	for i := 0; i < n; i++ {
		p, err := tr.CreatePane(ws.ID, tab.ID, Pane{Cwd: "/w", Kind: KindPTY, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	path := filepath.Join(t.TempDir(), "layout.json")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	before := tr.writes

	// Every change lands first, while a write holds the turn. Each save
	// then marshals the same newest tree.
	tr.saveMu.Lock()
	for _, id := range ids {
		if err := tr.UpdatePane(id, func(p *Pane) { p.Rows = 43 }); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- tr.Save(path)
		}()
	}
	tr.saveMu.Unlock()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := tr.writes - before; got != 1 {
		t.Fatalf("%d saves of one tree made %d writes, want 1", n, got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), `"rows": 43`) != n {
		t.Fatalf("the file does not hold every change:\n%s", b)
	}
	// A new change writes again.
	if err := tr.UpdatePane(ids[0], func(p *Pane) { p.Rows = 10 }); err != nil {
		t.Fatal(err)
	}
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	if got := tr.writes - before; got != 2 {
		t.Fatalf("a save after a new change made %d writes in all, want 2", got)
	}
}

// The same tree saved to another path is written there: a save skips only
// when the file it names already holds its tree.
func TestTheSameTreeSavedToAnotherPathIsWritten(t *testing.T) {
	tr := New()
	tr.CreateWorkspace("w", "/w")
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := tr.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := tr.Save(b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("the second path was not written: %v", err)
	}
}
