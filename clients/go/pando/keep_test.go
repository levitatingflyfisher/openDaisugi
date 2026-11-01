package pando

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A whole history moved aside is never removed, whatever the keep setting:
// it may be the only copy of the owner's memory.
func TestAMovedAsideHistoryIsNeverPruned(t *testing.T) {
	defer SetBrokenKeep(BrokenKeep())
	SetBrokenKeep(2)
	parent := t.TempDir()
	dir, day := makeHistory(t, parent)
	var older []string
	for i := 1; i <= 7; i++ {
		p := filepath.Join(parent, fmt.Sprintf("steward.broken-2026010%dT000000Z", i))
		if err := os.MkdirAll(filepath.Join(p, "main"), 0o700); err != nil {
			t.Fatal(err)
		}
		older = append(older, p)
	}
	appendTo(t, day, "not json\n")
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	c.Close()
	for _, p := range older {
		if _, err := os.Stat(filepath.Join(p, "main")); err != nil {
			t.Fatalf("a moved-aside history was removed: %s", p)
		}
	}
	all, _ := filepath.Glob(filepath.Join(parent, "steward.broken-*"))
	if len(all) != 8 {
		t.Fatalf("want 7 old and 1 new moved-aside histories, got %d", len(all))
	}
}

// The keep setting is pando's own: coppice sets it from broken_keep. It
// covers the leaf copies (a tree file's bad lines, a bad view.json).
func TestTheKeepSettingIsPandosOwnAndCoversLeafCopies(t *testing.T) {
	defer SetBrokenKeep(BrokenKeep())
	for _, keep := range []int{0, 2} {
		SetBrokenKeep(keep)
		dir := filepath.Join(t.TempDir(), "s")
		c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
		mustAppend(t, c, KindUser, "x")
		c.Close()
		view := filepath.Join(dir, "view.json")
		for i := 1; i <= 4; i++ {
			os.WriteFile(fmt.Sprintf("%s.broken-2026010%dT000000Z", view, i), []byte("old"), 0o600)
		}
		os.WriteFile(view, []byte("not json"), 0o600)
		c = openT(t, dir, &fakeSummarizer{}, newClock(), nil)
		c.Close()
		got, _ := filepath.Glob(view + ".broken-*")
		want := 5
		if keep == 2 {
			want = 2
		}
		if len(got) != want {
			t.Fatalf("keep %d: %d view copies, want %d", keep, len(got), want)
		}
	}
}
