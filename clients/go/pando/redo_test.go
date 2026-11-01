package pando

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustDayFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := dayFiles(filepath.Join(dir, "tree"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func readTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "tree", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gapSummarizer is a ModelSummarizer over a fake model that can be broken
// (it returns no text, as a thinking model out of tokens does) and fixed.
type gapSummarizer struct {
	mu      sync.Mutex
	broken  bool // no text for a node of one message
	mergeOK bool // a merge still gives text while broken
	calls   int
	n       int
}

func (g *gapSummarizer) model() *ModelSummarizer {
	return &ModelSummarizer{Call: func(_ context.Context, _, prompt string) (string, CallUsage, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls++
		g.n++
		merge := strings.Contains(prompt, "<left>")
		if g.broken && !(merge && g.mergeOK) {
			return "", CallUsage{Stop: "length"}, nil
		}
		return "line " + string(rune('a'+g.n%26)), CallUsage{}, nil
	}}
}

func twoLong(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	seedStore(t, data, "work", strings.Repeat("alpha ", 200), strings.Repeat("beta ", 200))
	return data
}

func tendWith(g *gapSummarizer, data string, args ...string) (int, string, string) {
	var out, errw strings.Builder
	env := CLIEnv{Out: &out, Err: &errw, Now: newClock().now,
		NewSummarizer: func(string) (Summarizer, error) { return g.model(), nil }}
	code := Run(append(args, "--data-dir", data), env)
	return code, out.String(), errw.String()
}

func nodeText(t *testing.T, data string, name string) string {
	t.Helper()
	dir, _ := StoreDir(data, "work")
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ParseName(name)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := c.Node(id)
	if !ok {
		t.Fatalf("no node %s", name)
	}
	return n.Text
}

func TestTendCountsAndExplainsClippedNodes(t *testing.T) {
	data := twoLong(t)
	g := &gapSummarizer{broken: true}
	code, out, errw := tendWith(g, data, "tend", "--yes")
	if code != 0 || !strings.Contains(out, "built: 3, fell back to clipped: 3\npending: 0\n") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	if !strings.Contains(out, ClippedWhy) || !strings.Contains(out, "--redo-clipped") {
		t.Fatalf("no reason: %q", out)
	}
	if !strings.HasPrefix(nodeText(t, data, "0+1"), ClipMarker) {
		t.Fatal("the node is not a clipped copy")
	}
	// A run with no fallback says nothing more.
	data2 := twoLong(t)
	code, out, _ = tendWith(&gapSummarizer{}, data2, "tend", "--yes")
	if code != 0 || !strings.Contains(out, "fell back to clipped: 0\n") || strings.Contains(out, ClippedWhy) {
		t.Fatalf("%d %q", code, out)
	}
}

func TestTendStopsWhenTheFirstFiveCallsAllClip(t *testing.T) {
	data := t.TempDir()
	lines := make([]string, 8)
	for i := range lines {
		lines[i] = strings.Repeat("word ", 300)
	}
	seedStore(t, data, "work", lines...)
	g := &gapSummarizer{broken: true}
	code, out, errw := tendWith(g, data, "tend", "--yes")
	if code != 1 || g.calls != ClipStopAfter {
		t.Fatalf("exit %d after %d calls: %q %q", code, g.calls, out, errw)
	}
	if !strings.Contains(errw, "the model returned no text for the first 5 nodes") ||
		!strings.Contains(errw, "max_tokens") || !strings.Contains(errw, "--redo-clipped") {
		t.Fatalf("message: %q", errw)
	}
	// A run that gets one line in the first five goes on.
	data2 := t.TempDir()
	seedStore(t, data2, "work", lines...)
	g2 := &gapSummarizer{broken: true, mergeOK: true}
	if code, _, errw := tendWith(g2, data2, "tend", "--yes"); code != 0 {
		t.Fatalf("exit %d: %q", code, errw)
	}
}

func TestRedoClippedBuildsTheNodesAgainAndTheOnesAbove(t *testing.T) {
	data := twoLong(t)
	// The leaves clip, the merge above them gives text: the parent is not a
	// clipped node, but it is made from clipped lines.
	g := &gapSummarizer{broken: true, mergeOK: true}
	if code, _, errw := tendWith(g, data, "tend", "--yes"); code != 0 {
		t.Fatal(errw)
	}
	before := nodeText(t, data, "0+2")
	if strings.HasPrefix(before, ClipMarker) {
		t.Fatalf("the parent should be a merge: %q", before)
	}
	code, out, _ := tendWith(g, data, "tend", "--redo-clipped", "--dry-run")
	if code != 0 || !strings.HasPrefix(out, "nodes to build again: 3\nmodel calls: at most 3\n0+1\n1+1\n0+2\n") || !strings.Contains(out, "nothing was changed") {
		t.Fatalf("dry run: %d %q", code, out)
	}
	if nodeText(t, data, "0+1") != nodeText(t, data, "0+1") || !strings.HasPrefix(nodeText(t, data, "0+1"), ClipMarker) {
		t.Fatal("a dry run changed a node")
	}
	// Fix the model. The two leaves and the parent above them are built
	// again. The parent joins two short lines, so it takes no model call.
	g.broken = false
	g.calls = 0
	code, out, errw := tendWith(g, data, "tend", "--redo-clipped", "--yes")
	if code != 0 || !strings.Contains(out, "built again: 3, fell back to clipped: 0\n") || g.calls != 2 {
		t.Fatalf("redo: %d %q %q (%d calls)", code, out, errw, g.calls)
	}
	for _, n := range []string{"0+1", "1+1", "0+2"} {
		if strings.HasPrefix(nodeText(t, data, n), ClipMarker) {
			t.Fatalf("%s is still clipped", n)
		}
	}
	if nodeText(t, data, "0+2") == before {
		t.Fatal("the parent was not made again")
	}
	// Nothing is left.
	g.calls = 0
	code, out, _ = tendWith(g, data, "tend", "--redo-clipped", "--yes")
	if code != 0 || !strings.HasPrefix(out, "nodes to build again: 0\n") || g.calls != 0 {
		t.Fatalf("again: %d %q (%d calls)", code, out, g.calls)
	}
	// The tree still reads, and the view shows no clipped line.
	code, out, _ = cli(t, "", "view", "--data-dir", data)
	if code != 0 || strings.Contains(out, ClipMarker) {
		t.Fatalf("view: %d %q", code, out)
	}
}

func TestRedoClippedKeepsOneBudgetAndGoesOldestFirst(t *testing.T) {
	data := twoLong(t)
	g := &gapSummarizer{broken: true}
	if code, _, errw := tendWith(g, data, "tend", "--yes"); code != 0 {
		t.Fatal(errw)
	}
	g.broken, g.calls = false, 0
	code, out, _ := tendWith(g, data, "tend", "--redo-clipped", "--max", "1")
	if code != 0 || g.calls != 1 || !strings.Contains(out, "built again: 1,") {
		t.Fatalf("%d %q (%d calls)", code, out, g.calls)
	}
	if strings.HasPrefix(nodeText(t, data, "0+1"), ClipMarker) || !strings.HasPrefix(nodeText(t, data, "1+1"), ClipMarker) {
		t.Fatal("the oldest node goes first")
	}
	// The rest, in a second run: the second leaf (one call) and the parent
	// above (joined without a call).
	g.calls = 0
	code, out, _ = tendWith(g, data, "tend", "--redo-clipped")
	if code != 0 || g.calls != 1 || !strings.Contains(out, "built again: 2, fell back to clipped: 0\n") {
		t.Fatalf("%d %q (%d calls)", code, out, g.calls)
	}
}

func TestRedoClippedStopsWhenTheModelIsStillBroken(t *testing.T) {
	data := t.TempDir()
	lines := make([]string, 8)
	for i := range lines {
		lines[i] = strings.Repeat("word ", 300)
	}
	seedStore(t, data, "work", lines...)
	// Clip every node without the stop rule: the chat's own drain.
	dir, _ := StoreDir(data, "work")
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx, (&gapSummarizer{broken: true}).model(), &sync.Mutex{}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	g := &gapSummarizer{broken: true}
	code, _, errw := tendWith(g, data, "tend", "--redo-clipped", "--yes")
	if code != 1 || g.calls != ClipStopAfter || !strings.Contains(errw, "no text for the first 5 nodes") {
		t.Fatalf("%d after %d calls: %q", code, g.calls, errw)
	}
	// A run that cannot change a node writes nothing.
	if !strings.HasPrefix(nodeText(t, data, "0+1"), ClipMarker) {
		t.Fatal("a node changed")
	}
}

func TestRedoClippedRefusesAPaidRunOverTheLimit(t *testing.T) {
	data := t.TempDir()
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = strings.Repeat("word ", 300)
	}
	seedStore(t, data, "work", lines...)
	dir, _ := StoreDir(data, "work")
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx, (&gapSummarizer{broken: true}).model(), &sync.Mutex{}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	g := &gapSummarizer{}
	code, out, errw := tendWith(g, data, "tend", "--redo-clipped")
	if code != 1 || g.calls != 0 || !strings.Contains(errw, "Claude Code (paid)") || !strings.HasPrefix(out, "nodes to build again: ") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	// --max is its own guard, and so is --profile.
	if code, _, errw := tendWith(g, data, "tend", "--redo-clipped", "--max", "3"); code != 0 || g.calls != 3 {
		t.Fatalf("%d %d %q", code, g.calls, errw)
	}
	if code, _, errw := tendWith(g, data, "tend", "--redo-clipped", "--profile", "desk", "--max", "40"); code != 0 {
		t.Fatalf("%d %q", code, errw)
	}
}

func TestRedoClippedGoesWithTendOnly(t *testing.T) {
	code, _, errw := cli(t, "", "view", "--redo-clipped", "--data-dir", t.TempDir())
	if code != 2 || !strings.Contains(errw, "--redo-clipped goes with tend") {
		t.Fatalf("%d %q", code, errw)
	}
}

func TestAReloadTakesTheLastLineOfANode(t *testing.T) {
	data := twoLong(t)
	g := &gapSummarizer{broken: true}
	tendWith(g, data, "tend", "--yes")
	g.broken = false
	tendWith(g, data, "tend", "--redo-clipped", "--yes")
	dir, _ := StoreDir(data, "work")
	// The tree file holds both lines of the node; a load keeps the later.
	raw := 0
	for _, f := range mustDayFiles(t, dir) {
		raw += strings.Count(readTestFile(t, dir, f), `"l":0,"i":0,`)
	}
	if raw != 2 {
		t.Fatalf("want the old and the new line of node 0+1, got %d", raw)
	}
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Notice() != "" {
		t.Fatalf("a rewritten node is not a bad line: %q", c.Notice())
	}
	if n, _ := c.Node(NodeID{L: 0, I: 0}); strings.HasPrefix(n.Text, ClipMarker) {
		t.Fatal("the old line won")
	}
}
