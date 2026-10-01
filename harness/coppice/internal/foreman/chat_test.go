package foreman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

// testClock is a clock that a test moves by hand.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 8, 23, 58, 0, 0, time.UTC)}
}

func openT(t *testing.T, dir string, s Summarizer, clk *testClock, tweak func(*Config)) *Chat {
	t.Helper()
	cfg := Config{Dir: dir, Summarizer: s, Now: clk.now}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func mustAppend(t *testing.T, c *Chat, kind, text string) []int64 {
	t.Helper()
	ids, err := c.Append(ctx, kind, text)
	if err != nil {
		t.Fatalf("Append(%s): %v", kind, err)
	}
	return ids
}

func TestAppendLogsWordForWord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fm")
	clk := newClock()
	c := openT(t, dir, &fakeSummarizer{}, clk, nil)
	mustAppend(t, c, KindUser, "hello <world> & \"you\"\nline two")
	clk.t = clk.t.Add(3 * time.Minute) // the next day
	mustAppend(t, c, KindForeman, "héllo")
	mustAppend(t, c, KindWork, "[Mason] report: done")
	mustAppend(t, c, KindPast, "an old chat line")
	mustAppend(t, c, KindNote, "a note")

	if c.Count() != 5 {
		t.Fatalf("Count = %d", c.Count())
	}
	m, err := c.Message(0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "hello <world> & \"you\"\nline two" || m.Kind != KindUser || m.Size != len(m.Text) ||
		m.Date != "2026-10-08T23:58:00Z" {
		t.Fatalf("message 0 = %+v", m)
	}
	day1, err := os.ReadFile(filepath.Join(dir, "main", "2026-10-08.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"i":0,"kind":"user","text":"hello <world> & \"you\"\nline two","size":30,"date":"2026-10-08T23:58:00Z"}` + "\n"
	if string(day1) != want {
		t.Fatalf("day file =\n%s\nwant\n%s", day1, want)
	}
	if d, _ := c.Date(1); d != "2026-10-09T00:01:00Z" {
		t.Fatalf("Date(1) = %q", d)
	}
	if _, err := os.Stat(filepath.Join(dir, "main", "2026-10-09.jsonl")); err != nil {
		t.Fatalf("no second day file: %v", err)
	}
}

func TestModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "fm")
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "x")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(fileMode)
		if d.IsDir() {
			want = dirMode
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenTightensAnOpenDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != dirMode {
		t.Fatalf("dir mode %o", info.Mode().Perm())
	}
}

func TestNoThoughtsAndNoUnknownKinds(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	for _, k := range []string{"thought", "thinking", "", "tool"} {
		if _, err := c.Append(ctx, k, "secret plans"); err == nil {
			t.Errorf("kind %q was logged", k)
		}
	}
	if c.Count() != 0 {
		t.Fatalf("Count = %d after refused appends", c.Count())
	}
	files, _ := dayFiles(filepath.Join(dir, "main"))
	if len(files) != 0 {
		t.Fatalf("refused appends wrote %v", files)
	}
}

func TestLongTextSplitsIntoMessagesInARow(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), func(cfg *Config) { cfg.SplitBytes = 10 })
	ids := mustAppend(t, c, KindUser, "0123456789abcdefghijéé")
	if len(ids) != 3 || ids[0] != 0 || ids[2] != 2 {
		t.Fatalf("ids = %v", ids)
	}
	var got []string
	for _, i := range ids {
		m, _ := c.Message(i)
		if m.Size > 10 {
			t.Fatalf("message %d has %d bytes", i, m.Size)
		}
		got = append(got, m.Text)
	}
	if strings.Join(got, "") != "0123456789abcdefghijéé" || got[2] != "éé" {
		t.Fatalf("parts = %q", got)
	}
}

func TestSplitAndClipKeepRunes(t *testing.T) {
	if got := clip("aé", 2); got != "a" {
		t.Fatalf("clip = %q", got)
	}
	if got := split("ééé", 3); len(got) != 3 || got[0] != "é" {
		t.Fatalf("split = %q", got)
	}
	if got := split("", 3); len(got) != 1 || got[0] != "" {
		t.Fatalf("split empty = %q", got)
	}
	if got := split("€", 2); len(got) != 1 || got[0] != "€" {
		t.Fatalf("split of a rune wider than n = %q", got)
	}
}

func TestInvalidUTF8IsRepaired(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "a\xffb")
	m, _ := c.Message(0)
	if m.Text != "a�b" || m.Size != len(m.Text) {
		t.Fatalf("message = %+v", m)
	}
}

func TestTreeJoinsShortLinesAndSummarizesLongOnes(t *testing.T) {
	s := &fakeSummarizer{}
	c := openT(t, t.TempDir(), s, newClock(), nil)
	mustAppend(t, c, KindUser, "one")
	mustAppend(t, c, KindForeman, "two")
	n, ok := c.Node(NodeID{1, 0})
	if !ok || n.Text != "one\ntwo" || s.compress+s.merge != 0 {
		t.Fatalf("node 0+2 = %+v %v; calls %d/%d", n, ok, s.compress, s.merge)
	}
	long := strings.Repeat("x", 600)
	mustAppend(t, c, KindUser, long)
	if s.compress != 1 {
		t.Fatalf("compress calls = %d", s.compress)
	}
	if n, _ := c.Node(NodeID{0, 2}); n.Text != "c "+strings.Repeat("x", 60) {
		t.Fatalf("node 2+1 = %q", n.Text)
	}
	mustAppend(t, c, KindUser, strings.Repeat("y", 500))
	// 2+2 joins "c xxx" (62) and 500 y's: 563 bytes, too long, so a merge.
	if s.merge != 1 {
		t.Fatalf("merge calls = %d", s.merge)
	}
	if _, ok := c.Node(NodeID{2, 0}); !ok {
		t.Fatal("node 0+4 not built: parents are built as soon as both halves are")
	}
}

func TestSummaryIsClippedToTheNodeBound(t *testing.T) {
	c := openT(t, t.TempDir(), longSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, strings.Repeat("z", 1000))
	if n, _ := c.Node(NodeID{0, 0}); n.Size != DefaultNodeBytes || len(n.Text) != DefaultNodeBytes {
		t.Fatalf("node size = %d", n.Size)
	}
}

func smallView(cfg *Config) {
	cfg.ViewMax = 2048
	cfg.ViewMin = 1024
}

func TestViewStaysInBoundsAndSawtooths(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), smallView)
	sawBatch := false
	for i := 0; i < 600; i++ {
		text := fmt.Sprintf("message %d %s", i, strings.Repeat("w", i%90))
		if i%13 == 0 {
			text = strings.Repeat("long ", 120)
		}
		mustAppend(t, c, KindUser, text)
		size := c.ViewSize()
		if size > 2048 {
			t.Fatalf("after message %d the view is %d bytes", i, size)
		}
		if i > 50 && size <= 1024 {
			sawBatch = true
		}
		// The view covers every message once, in order.
		next := int64(0)
		for _, id := range c.ViewLines() {
			if id.First() != next {
				t.Fatalf("after message %d the view has a gap at %d", i, next)
			}
			next = id.Last() + 1
		}
		if next != c.Count() {
			t.Fatalf("after message %d the view covers %d of %d", i, next, c.Count())
		}
	}
	if !sawBatch {
		t.Fatal("the view never merged down to ViewMin")
	}
}

// The view only ever grows by one line or merges: no step rebuilds it.
func TestViewIsIncremental(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), smallView)
	var prev []NodeID
	for i := 0; i < 300; i++ {
		mustAppend(t, c, KindUser, fmt.Sprintf("m%d %s", i, strings.Repeat("q", 40)))
		cur := c.ViewLines()
		// Every old line is still there or inside a line of cur.
		for _, p := range prev {
			found := false
			for _, q := range cur {
				if q.First() <= p.First() && p.Last() <= q.Last() && q.L >= p.L {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("message %d: line %s left the view without a merge", i, p.Name())
			}
		}
		prev = cur
	}
}

func readAll(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	var paths []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != "lock" {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "== %s\n%s", rel, data)
	}
	return b.String()
}

func script(i int) (string, string) {
	kinds := []string{KindUser, KindForeman, KindWork, KindForeman}
	text := fmt.Sprintf("line %d: %s", i, strings.Repeat("r", (i*37)%300))
	if i%11 == 0 {
		text = strings.Repeat("long text ", 70)
	}
	return kinds[i%len(kinds)], text
}

// A restart loads the same view byte for byte, and a chat that was closed
// and opened on the way ends with the same files as one that never was.
func TestRestartIsByteForByte(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	clkA, clkB := newClock(), newClock()
	ca := openT(t, a, &fakeSummarizer{}, clkA, smallView)
	cb := openT(t, b, &fakeSummarizer{}, clkB, smallView)
	for i := 0; i < 400; i++ {
		k, text := script(i)
		clkA.t = clkA.t.Add(time.Minute)
		clkB.t = clkA.t
		mustAppend(t, ca, k, text)
		mustAppend(t, cb, k, text)
		if i%37 == 36 {
			before, _ := os.ReadFile(filepath.Join(b, "view.json"))
			render := cb.Render()
			cb.Close()
			cb = openT(t, b, &fakeSummarizer{}, clkB, smallView)
			after, _ := os.ReadFile(filepath.Join(b, "view.json"))
			if !bytes.Equal(before, after) || cb.Render() != render {
				t.Fatalf("message %d: the view changed over a restart", i)
			}
		}
	}
	if readAll(t, a) != readAll(t, b) {
		t.Fatal("a chat that restarted ends with other files than one that did not")
	}
}

func TestSecondWriterIsRefused(t *testing.T) {
	dir := t.TempDir()
	openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	_, err := Open(ctx, Config{Dir: dir, Summarizer: &fakeSummarizer{}})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}
	if _, err := OpenReadOnly(dir); err != nil {
		t.Fatalf("a reader is refused: %v", err)
	}
}

// A crash can cut the last log line, or come after the log line and
// before view.json. Open drops the cut line and brings the view up to the
// log, to the same bytes a chat with no crash has.
func TestCrashRecovery(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	clk := newClock()
	ca := openT(t, a, &fakeSummarizer{}, clk, smallView)
	cb := openT(t, b, &fakeSummarizer{}, clk, smallView)
	var savedView []byte
	for i := 0; i < 120; i++ {
		k, text := script(i)
		mustAppend(t, ca, k, text)
		mustAppend(t, cb, k, text)
		if i == 99 {
			savedView, _ = os.ReadFile(filepath.Join(b, "view.json"))
		}
	}
	cb.Close()
	// view.json from 20 messages ago, and half a line at the end of the log.
	if err := os.WriteFile(filepath.Join(b, "view.json"), savedView, fileMode); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(b, "main", clk.t.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(day, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"i":120,"kind":"user","te`)
	f.Close()
	cb = openT(t, b, &fakeSummarizer{}, clk, smallView)
	if cb.Count() != 120 {
		t.Fatalf("Count after recovery = %d", cb.Count())
	}
	if readAll(t, a) != readAll(t, b) {
		t.Fatal("recovery gives other files than a chat with no crash")
	}
}

func TestSummarizerFailureKeepsTheMessage(t *testing.T) {
	dir := t.TempDir()
	s := &fakeSummarizer{fail: true}
	clk := newClock()
	c := openT(t, dir, s, clk, nil)
	ids, err := c.Append(ctx, KindUser, strings.Repeat("v", 700))
	if !errors.Is(err, errFake) || len(ids) != 1 {
		t.Fatalf("Append = %v, %v", ids, err)
	}
	if c.Count() != 1 || c.Pending() != 1 {
		t.Fatalf("Count %d Pending %d", c.Count(), c.Pending())
	}
	if !strings.Contains(c.Render(), "0+1|"+Unbuilt) {
		t.Fatalf("view = %q", c.Render())
	}
	s.fail = false
	mustAppend(t, c, KindUser, "next")
	if c.Pending() != 0 || strings.Contains(c.Render(), Unbuilt) {
		t.Fatalf("pending %d, view %q", c.Pending(), c.Render())
	}
	// A restart with the summarizer down still opens, and says so.
	c.Close()
	s.fail = true
	c2, err := Open(ctx, Config{Dir: dir, Summarizer: s, Now: clk.now})
	if err != nil {
		t.Fatalf("reopen with nothing pending: %v", err)
	}
	c2.Close()
}

func TestZoomAndDate(t *testing.T) {
	c := openT(t, t.TempDir(), &fakeSummarizer{}, newClock(), nil)
	for i := 0; i < 5; i++ {
		mustAppend(t, c, KindUser, fmt.Sprintf("msg %d\nsecond line", i))
	}
	got, err := c.Zoom(3, 1)
	if err != nil || got != "msg 3\nsecond line" {
		t.Fatalf("Zoom(3,1) = %q, %v", got, err)
	}
	got, err = c.Zoom(0, 4)
	want := "0+2|msg 0 second line msg 1 second line\n2+2|msg 2 second line msg 3 second line\n"
	if err != nil || got != want {
		t.Fatalf("Zoom(0,4) = %q, %v", got, err)
	}
	for _, bad := range [][2]int64{{0, 3}, {1, 2}, {4, 2}, {0, 8}, {5, 1}, {-1, 1}} {
		if _, err := c.Zoom(bad[0], bad[1]); err == nil {
			t.Errorf("Zoom(%d,%d) took a bad node", bad[0], bad[1])
		}
	}
	if d, err := c.Date(4); err != nil || d != "2026-10-08T23:58:00Z" {
		t.Fatalf("Date(4) = %q, %v", d, err)
	}
	if _, err := c.Date(5); err == nil {
		t.Fatal("Date(5) of a 5-message chat worked")
	}
}

func TestDayReadsOneDay(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), &fakeSummarizer{}, clk, nil)
	mustAppend(t, c, KindUser, "late")
	clk.t = clk.t.Add(5 * time.Minute)
	mustAppend(t, c, KindUser, "early")
	mustAppend(t, c, KindForeman, "reply")
	got, err := c.Day("2026-10-09")
	if err != nil || len(got) != 2 || got[0].I != 1 || got[1].Text != "reply" {
		t.Fatalf("Day = %+v, %v", got, err)
	}
	if got, _ := c.Day("2026-10-01"); len(got) != 0 {
		t.Fatalf("an empty day = %+v", got)
	}
	if _, err := c.Day("yesterday"); err == nil {
		t.Fatal("Day took a bad day")
	}
}

func TestReadOnlyDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	mustAppend(t, c, KindUser, "one")
	day, _ := dayFiles(filepath.Join(dir, "main"))
	path := filepath.Join(dir, "main", day[0])
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"i":1,"ki`) // a writer mid-line
	f.Close()
	before := readAll(t, dir)
	r, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Count() != 1 || r.Render() != c.Render() {
		t.Fatalf("reader sees %d messages, view %q", r.Count(), r.Render())
	}
	if _, err := r.Append(ctx, KindUser, "x"); err == nil {
		t.Fatal("a reader appended")
	}
	if readAll(t, dir) != before {
		t.Fatal("a reader changed the files")
	}
}

// A batch that runs out of built pairs stays open and goes on at the next
// message, once the parents it needs are built.
func TestBatchWaitsForUnbuiltParents(t *testing.T) {
	cfg := Config{ViewMax: 1000, ViewMin: 500}
	cfg.fill()
	c := &Chat{cfg: cfg, nodes: map[NodeID]Node{}}
	text := strings.Repeat("p", 300)
	for i := int64(0); i < 4; i++ {
		c.nodes[NodeID{0, i}] = Node{L: 0, I: i, Text: text, Size: len(text)}
		c.viewStep(i)
	}
	if !c.view.Batch || len(c.view.Lines) != 4 {
		t.Fatalf("with no parents built: batch %v, %d lines", c.view.Batch, len(c.view.Lines))
	}
	for _, n := range []Node{{L: 1, I: 0, Text: "a"}, {L: 1, I: 1, Text: "c"}, {L: 0, I: 4, Text: "b"}} {
		c.nodes[n.ID()] = n
	}
	c.viewStep(4)
	if c.view.Batch || c.ViewSize() > 500 {
		t.Fatalf("after the parents were built: batch %v, size %d, lines %v", c.view.Batch, c.ViewSize(), c.view.Lines)
	}
}
