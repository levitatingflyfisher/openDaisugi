package foreman

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const stamp = "20261008T235800Z"

// makeLong is makeHistory with a long first message, so its node needs the
// model to be built again.
func makeLong(t *testing.T, parent string) (string, string) {
	t.Helper()
	dir := filepath.Join(parent, "foreman")
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, strings.Repeat("x", 600))
	for i := 0; i < 4; i++ {
		mustAppend(t, c, KindUser, "old words")
	}
	c.Close()
	return dir, filepath.Join(dir, "main", "2026-10-08.jsonl")
}

// breakNode0 puts a bad line where node 0+1 was in the tree file and
// returns the tree file path and the bad line.
func breakNode0(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "tree", "2026-10-08.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if !strings.Contains(lines[0], `"l":0,"i":0`) {
		t.Fatalf("first tree line is %q", lines[0])
	}
	bad := "garbage {\n"
	lines[0] = bad
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), fileMode); err != nil {
		t.Fatal(err)
	}
	return path, bad
}

// U3: a bad tree line is repaired. The line is copied to a .broken file, the
// tree file loses it, the node it held is built again, and the history
// stays where it is.
func TestBadTreeLineIsRepairedNotMovedAside(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeLong(t, parent)
	wantView, _ := os.ReadFile(filepath.Join(dir, "view.json"))
	path, bad := breakNode0(t, dir)

	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ents, _ := os.ReadDir(parent); len(ents) != 1 {
		t.Fatalf("the history moved: %v", ents)
	}
	if c.Count() != 5 {
		t.Fatalf("Count = %d", c.Count())
	}
	saved := path + ".broken-" + stamp
	got, err := os.ReadFile(saved)
	if err != nil || string(got) != bad {
		t.Fatalf("copy = %q, %v; want %q", got, err, bad)
	}
	if st, _ := os.Stat(saved); st.Mode().Perm() != fileMode {
		t.Fatalf("copy mode %v", st.Mode())
	}
	want := "Foreman tree repaired: 1 bad line in tree/2026-10-08.jsonl was copied to " + saved +
		" and removed. The nodes those lines held will be built again from the log."
	if c.Notice() != want {
		t.Fatalf("Notice = %q\nwant %q", c.Notice(), want)
	}
	tree, _ := os.ReadFile(path)
	if strings.Contains(string(tree), "garbage") {
		t.Fatalf("the tree still holds a bad line: %q", tree)
	}
	if c.Pending() != 1 {
		t.Fatalf("Pending = %d, want the one node the bad line held", c.Pending())
	}
	if err := drainT(t, c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Node(NodeID{0, 0}); !ok || c.Pending() != 0 {
		t.Fatal("node 0+1 was not built again")
	}
	if v, _ := c.ViewJSON(); string(v) != string(wantView) {
		t.Fatalf("view changed: %s vs %s", v, wantView)
	}
	// A second open is clean.
	c.Close()
	c2 := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	if c2.Notice() != "" || c2.Pending() != 0 {
		t.Fatalf("second open: %q, %d", c2.Notice(), c2.Pending())
	}
}

func TestOutsideNodeLineIsRepairedToo(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	appendTo(t, filepath.Join(dir, "tree", "2026-10-08.jsonl"), `{"l":3,"i":0,"text":"x","size":1}`+"\n")
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	if !strings.HasPrefix(c.Notice(), "Foreman tree repaired: 1 bad line") || c.Count() != 5 {
		t.Fatalf("Notice = %q", c.Notice())
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 1 {
		t.Fatalf("moved: %v", ents)
	}
}

// A reader skips a bad tree line as if it were absent, says so, and writes
// nothing.
func TestReaderSkipsABadTreeLine(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeLong(t, parent)
	breakNode0(t, dir)
	before := readAll(t, dir)
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Notice(), "1 bad line in tree/2026-10-08.jsonl was skipped") ||
		!strings.Contains(c.Notice(), "Nothing was changed.") {
		t.Fatalf("Notice = %q", c.Notice())
	}
	if !strings.Contains(c.Render(), "0+1|"+Unbuilt) {
		t.Fatalf("Render = %q", c.Render())
	}
	if readAll(t, dir) != before {
		t.Fatal("the reader wrote")
	}
	code, out, errOut := runCLI(t, "", "--data-dir", dir, "view")
	if code != 0 || !strings.Contains(out, "0+1|"+Unbuilt) || !strings.Contains(errOut, "skipped") {
		t.Fatalf("CLI = %d %q %q", code, out, errOut)
	}
	if readAll(t, dir) != before {
		t.Fatal("the CLI wrote")
	}
}

func TestBadViewIsMovedAloneAndRebuilt(t *testing.T) {
	for name, body := range map[string]string{
		"not json": "[[", "outside the log": `{"count":99,"batch":false,"lines":[[0,0]]}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir, _ := makeHistory(t, parent)
			good, _ := os.ReadFile(filepath.Join(dir, "view.json"))
			os.WriteFile(filepath.Join(dir, "view.json"), []byte(body), fileMode)
			c, err := Open(ctx, Config{Dir: dir, Now: newClock().now})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if ents, _ := os.ReadDir(parent); len(ents) != 1 || c.Count() != 5 {
				t.Fatalf("moved: %v, Count %d", ents, c.Count())
			}
			saved := filepath.Join(dir, "view.json.broken-"+stamp)
			if got, err := os.ReadFile(saved); err != nil || string(got) != body {
				t.Fatalf("copy = %q, %v", got, err)
			}
			if now, _ := os.ReadFile(filepath.Join(dir, "view.json")); string(now) != string(good) {
				t.Fatalf("view.json = %q, want %q", now, good)
			}
			want := "Foreman view.json could not be used. It was copied to " + saved + " and the view was rebuilt from the tree."
			if c.Notice() != want {
				t.Fatalf("Notice = %q\nwant %q", c.Notice(), want)
			}
		})
	}
}

func TestReaderRebuildsABadViewInMemory(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	good, _ := os.ReadFile(filepath.Join(dir, "view.json"))
	os.WriteFile(filepath.Join(dir, "view.json"), []byte("[["), fileMode)
	before := readAll(t, dir)
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.ViewJSON(); string(v) != string(good) || !strings.Contains(c.Notice(), "view.json could not be used") {
		t.Fatalf("view %q notice %q", v, c.Notice())
	}
	if readAll(t, dir) != before {
		t.Fatal("the reader wrote")
	}
}

// If the rebuild itself fails, the whole history moves aside, as before.
func TestFailedViewRebuildFallsBackToMoveAside(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	os.WriteFile(filepath.Join(dir, "view.json"), []byte("[["), fileMode)
	failView := func(p string, d []byte) error {
		if filepath.Base(p) == "view.json" {
			return errInjected
		}
		return writeAtomic(p, d)
	}
	c, err := Open(ctx, Config{Dir: dir, Now: newClock().now, writeFile: failView})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.Contains(c.Notice(), "view.json could not be rebuilt") || c.Count() != 0 {
		t.Fatalf("Notice = %q, Count %d", c.Notice(), c.Count())
	}
	if _, err := os.Stat(filepath.Join(parent, brokenName)); err != nil {
		t.Fatal(err)
	}
}

// The requeued node goes before anything appended later, in id order, as
// after a crash. A live message is logged and its tail is read at once.
func TestRequeuedNodeKeepsOrder(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeLong(t, parent)
	breakNode0(t, dir)
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	ids, err := c.Append(ctx, KindUser, "live words")
	if err != nil || len(ids) != 1 {
		t.Fatal(err)
	}
	want := []NodeID{{0, 0}, {0, 5}}
	if len(c.pending) < 2 || !reflect.DeepEqual(c.pending[:2], want) {
		t.Fatalf("pending = %v, want %v first", c.pending, want)
	}
	tail, _ := c.Tail()
	if len(tail) != 1 || tail[0].Text != "live words" {
		t.Fatalf("Tail = %v", tail)
	}
}

// A dangling spawn start survives a tree repair: the call log is not
// touched, and the start is still the one dangling call.
func TestRepairKeepsTheCallLog(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeLong(t, parent)
	_ = AppendCall(dir, CallRecord{Call: "c4.1", Phase: CallStart, Turn: 4, Tool: "spawn", Args: `{"x":1}`, At: "t1"})
	breakNode0(t, dir)
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	_ = c
	recs, err := ReadCalls(dir)
	dang := DanglingCalls(recs)
	if err != nil || len(dang) != 1 || dang[0].Call != "c4.1" || dang[0].Turn != 4 || len(recs) != 1 {
		t.Fatalf("calls = %v, %v", recs, err)
	}
}

// 3.9: the batch merge gives the lines MostDue and mergeAt give, one merge
// at a time.
func TestBatchPlannerMatchesMostDue(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 300; trial++ {
		total := int64(5 + rng.Intn(300))
		// The view of a chat of total messages: fine lines, then merged to
		// a random depth, as a view looks mid-life.
		view := []NodeID{}
		for i := int64(0); i < total; i++ {
			view = append(view, NodeID{0, i})
		}
		for m := rng.Intn(int(total)); m > 0; m-- {
			if k := MostDue(view, total, allBuilt); k >= 0 {
				view = mergeAt(view, k)
			}
		}
		built := map[NodeID]bool{}
		for _, id := range view {
			built[id], built[id.Parent()] = rng.Intn(4) > 0, rng.Intn(4) > 0
		}
		isBuilt := func(id NodeID) bool { return built[id] }

		ref := append([]NodeID(nil), view...)
		var refSeq []NodeID
		for {
			k := MostDue(ref, total, isBuilt)
			if k < 0 {
				break
			}
			refSeq = append(refSeq, ref[k].Parent())
			ref = mergeAt(ref, k)
		}
		p := newPlanner(view, total, isBuilt)
		var seq []NodeID
		for {
			k, ok := p.pop()
			if !ok {
				break
			}
			seq = append(seq, p.merge(k))
		}
		if !reflect.DeepEqual(seq, refSeq) || !reflect.DeepEqual(p.lines(), ref) {
			t.Fatalf("trial %d (total %d): planner differs from MostDue\n got %v\nwant %v", trial, total, seq, refSeq)
		}
	}
}

func TestZoomPagesAndHintsRoots(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), nil)
	for i := 0; i < 5; i++ {
		mustAppend(t, c, KindUser, "0123456789abcdefghij")
	}
	got, err := c.ZoomPage(2, 1, 0, 8)
	if err != nil || got != "01234567\n[bytes 0-8 of 20. More: offset 8]" {
		t.Fatalf("page 1 = %q, %v", got, err)
	}
	got, err = c.ZoomPage(2, 1, 8, 8)
	if err != nil || got != "89abcdef\n[bytes 8-16 of 20. More: offset 16]" {
		t.Fatalf("page 2 = %q, %v", got, err)
	}
	got, err = c.ZoomPage(2, 1, 16, 8)
	if err != nil || got != "ghij\n[bytes 16-20 of 20. End.]" {
		t.Fatalf("page 3 = %q, %v", got, err)
	}
	if got, err = c.ZoomPage(2, 1, 0, 0); err != nil || got != "0123456789abcdefghij" {
		t.Fatalf("whole = %q, %v", got, err)
	}
	if _, err = c.ZoomPage(2, 1, 21, 8); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("offset past the end: %v", err)
	}
	if _, err = c.ZoomPage(2, 1, -1, 8); err == nil {
		t.Fatal("negative offset")
	}
	// A cut never splits a rune.
	mustAppend(t, c, KindUser, "ééééé")
	got, err = c.ZoomPage(5, 1, 0, 3)
	if err != nil || got != "é\n[bytes 0-2 of 10. More: offset 2]" {
		t.Fatalf("rune page = %q, %v", got, err)
	}
	// A bad id answers with the roots.
	for _, bad := range [][2]int64{{9, 1}, {0, 3}, {1, 2}, {0, 64}} {
		_, err := c.Zoom(bad[0], bad[1])
		if err == nil || !strings.Contains(err.Error(), "The root nodes are 0+4, 4+2.") {
			t.Errorf("Zoom(%d,%d) = %v", bad[0], bad[1], err)
		}
	}
}

func TestZoomCLIPagesAndHints(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	code, out, _ := runCLI(t, "", "--data-dir", dir, "zoom", "1+1", "--size", "4", "--offset", "4")
	if code != 0 || out != "word\n[bytes 4-8 of 9. More: offset 8]\n" {
		t.Fatalf("CLI = %d %q", code, out)
	}
	code, _, errOut := runCLI(t, "", "--data-dir", dir, "zoom", "77+1")
	if code != 1 || !strings.Contains(errOut, "The root nodes are 0+4, 4+1.") {
		t.Fatalf("CLI = %d %q", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "--data-dir", dir, "zoom", "1+2")
	if code != 1 || !strings.Contains(errOut, "The root nodes are 0+4, 4+1.") {
		t.Fatalf("CLI = %d %q", code, errOut)
	}
}
