package pando

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// layerModel is a fake model that writes a long line for a message and a
// short one for a merge, and keeps every question it was asked.
type layerModel struct {
	mu      sync.Mutex
	prompts []string
	n       int
	merges  int
	leaves  int
	failAt  int // the call that fails, counted from the next one; 0 is none
}

func (l *layerModel) model() *ModelSummarizer {
	return &ModelSummarizer{Call: func(_ context.Context, _, prompt string) (string, CallUsage, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.failAt > 0 {
			l.failAt--
			if l.failAt == 0 {
				return "", CallUsage{}, fmt.Errorf("the model is gone")
			}
		}
		l.prompts = append(l.prompts, prompt)
		l.n++
		if strings.Contains(prompt, "<left>") {
			l.merges++
			return fmt.Sprintf("merge %d", l.n), CallUsage{}, nil
		}
		l.leaves++
		return fmt.Sprintf("leaf %d %s", l.n, strings.Repeat("x", 300)), CallUsage{}, nil
	}}
}

func (l *layerModel) summ(string) (Summarizer, error) { return l.model(), nil }

func runWith(l *layerModel, data string, args ...string) (int, string, string) {
	var out, errw strings.Builder
	env := CLIEnv{Out: &out, Err: &errw, Now: newClock().now, NewSummarizer: l.summ}
	code := Run(append(args, "--data-dir", data), env)
	return code, out.String(), errw.String()
}

func fourLong(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	m := strings.Repeat("word ", 130)
	seedStore(t, data, "work", m, m, m, m)
	return data
}

func TestUnclipDropsEveryLeadingMarker(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                           "plain",
		"[clipped] a":                     "a",
		"[clipped] [clipped] [clipped] a": "a",
		"a [clipped] b":                   "a [clipped] b",
		"":                                "",
	} {
		if got := unclip(in); got != want {
			t.Errorf("unclip(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAJoinedParentCarriesOneMarkerWhenAPartIsClipped(t *testing.T) {
	for _, tc := range []struct{ l, r, want string }{
		{"a", "b", "a\nb"},
		{"[clipped] a", "b", "[clipped] a\nb"},
		{"a", "[clipped] b", "[clipped] a\nb"},
		{"[clipped] [clipped] a", "[clipped] b", "[clipped] a\nb"},
	} {
		if got := joinLines(tc.l, tc.r); got != tc.want {
			t.Errorf("joinLines(%q, %q) = %q, want %q", tc.l, tc.r, got, tc.want)
		}
	}
}

func TestAModelMergeNeverSeesAClipMarkerAndAFallbackKeepsOne(t *testing.T) {
	var prompt string
	m := &ModelSummarizer{Call: func(_ context.Context, _, p string) (string, CallUsage, error) {
		prompt = p
		return "", CallUsage{Stop: "length"}, nil
	}}
	left := ClipMarker + ClipMarker + strings.Repeat("alpha ", 80)
	right := ClipMarker + strings.Repeat("beta ", 80)
	got, err := m.Merge(ctx, NodeID{L: 1, I: 0}, left, right, 512)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "[clipped]") {
		t.Fatalf("the model saw a marker: %q", prompt)
	}
	if !strings.HasPrefix(got, ClipMarker+"alpha ") || strings.Count(got, ClipMarker) != 1 {
		t.Fatalf("the fallback must hold one marker: %q", got)
	}
}

func TestATendedParentOfClippedLeavesHoldsOneMarker(t *testing.T) {
	data := fourLong(t)
	// The chat's own drain has no stop rule: every node clips.
	dir, _ := StoreDir(data, "work")
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx, (&gapSummarizer{broken: true}).model(), &sync.Mutex{}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, n := range []string{"0+2", "2+2", "0+4"} {
		if txt := nodeText(t, data, n); !strings.HasPrefix(txt, ClipMarker) || strings.Count(txt, ClipMarker) != 1 {
			t.Fatalf("%s: %q", n, txt)
		}
	}
}

func TestRedoLevelBuildsEveryNodeFromThatLevelUp(t *testing.T) {
	data := fourLong(t)
	l := &layerModel{}
	if code, _, errw := runWith(l, data, "tend", "--yes"); code != 0 || l.leaves != 4 || l.merges != 2 {
		t.Fatalf("%d %q (%d leaves, %d merges)", code, errw, l.leaves, l.merges)
	}
	oldLeaf, oldMerge, oldRoot := nodeText(t, data, "0+1"), nodeText(t, data, "0+2"), nodeText(t, data, "0+4")
	code, out, _ := runWith(l, data, "tend", "--redo-level", "1", "--dry-run")
	if code != 0 || !strings.HasPrefix(out, "nodes to build again: 3\nmodel calls: at most 3\n0+2\n2+2\n0+4\n") || !strings.Contains(out, "nothing was changed") {
		t.Fatalf("dry run: %d %q", code, out)
	}
	if l.leaves != 4 || l.merges != 2 {
		t.Fatal("a dry run called the model")
	}
	code, out, errw := runWith(l, data, "tend", "--redo-level", "1", "--yes")
	if code != 0 || !strings.Contains(out, "built again: 3, fell back to clipped: 0\n") || l.leaves != 4 || l.merges != 4 {
		t.Fatalf("redo: %d %q %q (%d leaves, %d merges)", code, out, errw, l.leaves, l.merges)
	}
	if nodeText(t, data, "0+1") != oldLeaf {
		t.Fatal("a leaf below the level changed")
	}
	if nodeText(t, data, "0+2") == oldMerge || nodeText(t, data, "0+4") == oldRoot {
		t.Fatal("a node at the level was not built again")
	}
	// Level 0 builds every node again: the four leaves and the merges.
	l.leaves, l.merges = 0, 0
	code, out, errw = runWith(l, data, "tend", "--redo-level", "0", "--yes")
	if code != 0 || !strings.Contains(out, "built again: 7, ") || l.leaves != 4 || l.merges != 2 {
		t.Fatalf("level 0: %d %q %q (%d leaves, %d merges)", code, out, errw, l.leaves, l.merges)
	}
}

func TestRedoLevelTakesMaxAndTheUnion(t *testing.T) {
	data := fourLong(t)
	l := &layerModel{}
	runWith(l, data, "tend", "--yes")
	l.leaves, l.merges = 0, 0
	code, out, errw := runWith(l, data, "tend", "--redo-level", "0", "--max", "2", "--yes")
	if code != 0 || l.leaves+l.merges != 2 || !strings.Contains(out, "built again: 2, ") {
		t.Fatalf("max: %d %q %q (%d calls)", code, out, errw, l.leaves+l.merges)
	}
	// One leaf holds a clipped copy. --redo-level 1 leaves it alone;
	// both flags pick it too, once.
	data2 := fourLong(t)
	g := &gapSummarizer{broken: true, mergeOK: true}
	tendWith(g, data2, "tend", "--yes")
	_, out, _ = runWith(l, data2, "tend", "--redo-level", "1", "--dry-run")
	if !strings.HasPrefix(out, "nodes to build again: 3\n") {
		t.Fatalf("level only: %q", out)
	}
	_, out, _ = runWith(l, data2, "tend", "--redo-level", "1", "--redo-clipped", "--dry-run")
	if !strings.HasPrefix(out, "nodes to build again: 7\n") || strings.Count(out, "0+4\n") != 1 {
		t.Fatalf("union: %q", out)
	}
}

func TestRedoLevelGoesWithTendAndTakesANumber(t *testing.T) {
	code, _, errw := cli(t, "", "view", "--redo-level", "1", "--data-dir", t.TempDir())
	if code != 2 || !strings.Contains(errw, "--redo-level goes with tend") {
		t.Fatalf("%d %q", code, errw)
	}
	code, _, errw = cli(t, "", "tend", "--redo-level", "x", "--data-dir", t.TempDir())
	if code != 2 || !strings.Contains(errw, "--redo-level needs a number") {
		t.Fatalf("%d %q", code, errw)
	}
}

// The refusal of a paid redo counts the model calls the plan can make, not
// the nodes: 26 nodes sit at level 1 and above, and only 11 of them can call.
func TestRedoRefusalCountsCallsNotNodes(t *testing.T) {
	data := t.TempDir()
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "w"
	}
	seedStore(t, data, "work", lines...)
	g := &gapSummarizer{}
	if code, _, errw := tendWith(g, data, "tend", "--yes"); code != 0 || g.calls != 0 {
		t.Fatalf("%d %q (%d calls)", code, errw, g.calls)
	}
	code, out, errw := tendWith(g, data, "tend", "--redo-level", "1")
	if code != 0 || !strings.HasPrefix(out, "nodes to build again: 26\nmodel calls: at most 11\n") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	// Level 0 of long messages is one call each: refused over 25.
	long := t.TempDir()
	for i := range lines {
		lines[i] = strings.Repeat("word ", 300)
	}
	seedStore(t, long, "work", lines...)
	code, _, errw = tendWith(g, long, "tend", "--redo-level", "0")
	if code != 0 {
		// Nothing is built yet, so the plan is empty and nothing is refused.
		t.Fatalf("an empty plan is not refused: %d %q", code, errw)
	}
	tendWith(&gapSummarizer{}, long, "tend", "--yes")
	code, _, errw = tendWith(g, long, "tend", "--redo-level", "0")
	if code != 1 || !strings.Contains(errw, "Claude Code (paid)") {
		t.Fatalf("%d %q", code, errw)
	}
}

// A redo that stopped on a failed call goes on where it stopped: a node the
// earlier run built is not called again.
func TestRedoLevelResumesAfterAFailedCall(t *testing.T) {
	data := fourLong(t)
	l := &layerModel{}
	runWith(l, data, "tend", "--yes")
	l.leaves, l.merges = 0, 0
	l.failAt = 4
	code, _, errw := runWith(l, data, "tend", "--redo-level", "0", "--yes")
	if code != 1 || l.leaves != 3 || l.merges != 0 || !strings.Contains(errw, "the model is gone") {
		t.Fatalf("first run: %d %q (%d leaves, %d merges)", code, errw, l.leaves, l.merges)
	}
	dir, _ := StoreDir(data, "work")
	if _, err := os.Stat(filepath.Join(dir, "tree", RedoFile)); err != nil {
		t.Fatalf("an unfinished redo leaves its marker: %v", err)
	}
	code, out, _ := runWith(l, data, "tend", "--redo-level", "0", "--dry-run")
	if code != 0 || !strings.HasPrefix(out, "nodes to build again: 4\n") {
		t.Fatalf("the plan skips the built leaves: %q", out)
	}
	code, out, errw = runWith(l, data, "tend", "--redo-level", "0", "--yes")
	if code != 0 || l.leaves != 4 || l.merges != 2 || !strings.Contains(out, "built again: 4, ") {
		t.Fatalf("second run: %d %q %q (%d leaves, %d merges)", code, out, errw, l.leaves, l.merges)
	}
	if _, err := os.Stat(filepath.Join(dir, "tree", RedoFile)); !os.IsNotExist(err) {
		t.Fatalf("a finished redo removes its marker: %v", err)
	}
	// A new redo is a whole one.
	l.leaves, l.merges = 0, 0
	if code, out, _ := runWith(l, data, "tend", "--redo-level", "0", "--yes"); code != 0 || !strings.Contains(out, "built again: 7, ") || l.leaves != 4 || l.merges != 2 {
		t.Fatalf("third run: %d %q (%d leaves, %d merges)", code, out, l.leaves, l.merges)
	}
}

// Another set or another profile does not resume: it is its own redo.
func TestRedoWithAnotherProfileOrSetStartsAgain(t *testing.T) {
	data := fourLong(t)
	l := &layerModel{}
	runWith(l, data, "tend", "--yes")
	l.failAt = 3
	runWith(l, data, "tend", "--redo-level", "0", "--yes")
	l.leaves, l.merges = 0, 0
	if code, out, _ := runWith(l, data, "tend", "--redo-level", "0", "--profile", "other", "--yes"); code != 0 || !strings.Contains(out, "built again: 7, ") {
		t.Fatalf("another profile: %d %q", code, out)
	}
}

// A redo for another set does not delete the marker of an unfinished one.
func TestARedoForAnotherSetKeepsTheMarker(t *testing.T) {
	data := fourLong(t)
	l := &layerModel{}
	runWith(l, data, "tend", "--yes")
	l.failAt = 1
	runWith(l, data, "tend", "--redo-level", "0", "--yes")
	dir, _ := StoreDir(data, "work")
	marker := filepath.Join(dir, "tree", RedoFile)
	if code, _, errw := runWith(l, data, "tend", "--redo-clipped", "--yes"); code != 0 {
		t.Fatalf("%d %q", code, errw)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the clipped run took the level run's marker: %v", err)
	}
	_, out, _ := runWith(l, data, "tend", "--redo-level", "0", "--dry-run")
	if !strings.HasPrefix(out, "nodes to build again: 7\n") {
		t.Fatalf("the level run still resumes: %q", out)
	}
}

// A child store that was locked and skipped keeps its marker.
func TestALockedChildKeepsItsMarker(t *testing.T) {
	data := fourLong(t)
	dir, _ := StoreDir(data, "work")
	cd, _ := ChildDir(dir, "a1")
	c, err := Open(ctx, Config{Dir: cd, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Append(ctx, KindUser, strings.Repeat("word ", 130)); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l := &layerModel{}
	runWith(l, data, "tend", "--yes")
	marker := filepath.Join(cd, "tree", RedoFile)
	if err := os.WriteFile(marker, []byte(`{"after":0,"model":"default","set":"level:0"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errw := runWith(l, data, "tend", "--redo-level", "0", "--yes"); code != 0 {
		t.Fatalf("%d %q", code, errw)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a skipped child lost its marker: %v", err)
	}
}
