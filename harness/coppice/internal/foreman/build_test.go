package foreman

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowSummarizer is the fake with a random delay, and a count of calls in
// flight at once.
type slowSummarizer struct {
	mu          sync.Mutex
	rng         *rand.Rand
	inflight    int32
	maxInflight int32
}

func (s *slowSummarizer) enter() {
	n := atomic.AddInt32(&s.inflight, 1)
	for {
		m := atomic.LoadInt32(&s.maxInflight)
		if n <= m || atomic.CompareAndSwapInt32(&s.maxInflight, m, n) {
			break
		}
	}
	s.mu.Lock()
	d := time.Duration(s.rng.Intn(4000)) * time.Microsecond
	s.mu.Unlock()
	time.Sleep(d)
}

func (s *slowSummarizer) Compress(ctx context.Context, m Message, limit int) (string, error) {
	s.enter()
	defer atomic.AddInt32(&s.inflight, -1)
	return "c " + clip(m.Text, 60), nil
}

func (s *slowSummarizer) Merge(ctx context.Context, id NodeID, l, r string, limit int) (string, error) {
	s.enter()
	defer atomic.AddInt32(&s.inflight, -1)
	return "m " + clip(l, 40) + " | " + clip(r, 40), nil
}

func longText(i int) string { return fmt.Sprintf("%d:", i) + strings.Repeat("w", 600) }

// fillLong logs n long messages and one short one in between, without
// building anything.
func fillLong(t *testing.T, c *Chat, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		text := longText(i)
		if i%3 == 1 {
			text = fmt.Sprintf("short %d", i)
		}
		if _, err := c.Append(ctx, KindUser, text); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodesAreBuiltOneAtATimeInQueueOrder(t *testing.T) {
	c := openT(t, t.TempDir(), nil, newClock(), smallView)
	s := &slowSummarizer{rng: rand.New(rand.NewSource(7))}
	fillLong(t, c, 90)
	if err := c.Drain(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending %d", c.Pending())
	}
	if s.maxInflight != 1 {
		t.Fatalf("%d calls in flight at once, want 1", s.maxInflight)
	}
}

func TestTheViewTakesAMessageOnlyOnceItsNodesAreWritten(t *testing.T) {
	c := openT(t, t.TempDir(), nil, newClock(), nil)
	mustAppendNoDrain := func(text string) {
		if _, err := c.Append(ctx, KindUser, text); err != nil {
			t.Fatal(err)
		}
	}
	mustAppendNoDrain(longText(0)) // needs the model
	mustAppendNoDrain("short")     // needs none, but waits behind node 0
	if c.view.Count != 0 || len(c.ViewLines()) != 0 {
		t.Fatalf("view count %d, lines %v, before any node is written", c.view.Count, c.ViewLines())
	}
	tail, err := c.Tail()
	if err != nil || len(tail) != 2 || tail[1].Text != "short" {
		t.Fatalf("Tail = %v, %v", tail, err)
	}
	if strings.Contains(c.Render(), "0+1|") {
		t.Fatalf("view = %q", c.Render())
	}
	job, ok, err := c.Next()
	if err != nil || !ok || job.ID != (NodeID{0, 0}) {
		t.Fatalf("Next = %+v, %v, %v", job, ok, err)
	}
	if err := c.Commit(job, "c line"); err != nil {
		t.Fatal(err)
	}
	if c.view.Count != 2 || c.Pending() != 0 {
		t.Fatalf("view count %d, pending %d after the commit", c.view.Count, c.Pending())
	}
	if tail, _ := c.Tail(); len(tail) != 0 {
		t.Fatalf("Tail after the commit = %v", tail)
	}
}

func TestAFailedJobIsTheFrontJobAgain(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	for i := 0; i < 3; i++ {
		if _, err := c.Append(ctx, KindUser, longText(i)); err != nil {
			t.Fatal(err)
		}
	}
	job, ok, _ := c.Next()
	if !ok || job.ID.I != 0 {
		t.Fatalf("Next = %+v, %v", job, ok)
	}
	// Next does not take the job: asked again, it gives the same one.
	if again, _, _ := c.Next(); again.ID != job.ID {
		t.Fatalf("second Next = %+v", again)
	}
	c.FailErr(job, errFake)
	if _, ok, err := c.Next(); ok || err == nil {
		t.Fatalf("Next inside the wait = %v, %v", ok, err)
	}
	clk.advance(31 * time.Second)
	if again, ok, _ := c.Next(); !ok || again.ID != job.ID {
		t.Fatalf("Next after the wait = %+v, %v", again, ok)
	}
	if _, ok := c.Node(NodeID{0, 1}); ok || c.Pending() != 4 {
		t.Fatal("a node behind the failed one was built")
	}
}

func TestDrainKeepsTheFailedNodesPendingAndBuildsThemAfterTheirWait(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	fillLong(t, c, 9)
	s := &fakeSummarizer{fail: true}
	if err := c.Drain(ctx, s, nil); err == nil {
		t.Fatal("Drain with a summarizer that is down did not fail")
	}
	if c.Pending() == 0 {
		t.Fatal("nothing is pending after a failed drain")
	}
	// At once, a second Drain asks nothing and still reports the failure.
	if err := c.Drain(ctx, &fakeSummarizer{}, nil); err == nil || c.Pending() == 0 {
		t.Fatalf("a Drain inside the wait = %v, pending %d", err, c.Pending())
	}
	clk.advance(31 * time.Second)
	if err := c.Drain(ctx, &fakeSummarizer{}, nil); err != nil || c.Pending() != 0 {
		t.Fatalf("second Drain = %v, pending %d", err, c.Pending())
	}
	if filepath.Base(c.cfg.Dir) == "" {
		t.Fatal("unreachable")
	}
}

func TestAFailedWriteKeepsTheModelsLineAndRetriesOnlyTheWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only dir")
	}
	c := openT(t, t.TempDir(), nil, newClock(), nil)
	if _, err := c.Append(ctx, KindUser, longText(0)); err != nil {
		t.Fatal(err)
	}
	job, ok, err := c.Next()
	if err != nil || !ok {
		t.Fatalf("Next = %v, %v", ok, err)
	}
	dir := c.treeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := c.Commit(job, "the model line"); err == nil {
		t.Fatal("Commit into a read-only dir did not fail")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The next step writes the kept line; no job is handed out again.
	if _, ok, err := c.Next(); ok || err != nil {
		t.Fatalf("Next after the failed write = %v, %v", ok, err)
	}
	if n, found := c.Node(job.ID); !found || n.Text != "the model line" || c.Pending() != 0 {
		t.Fatalf("node = %+v %v, pending %d", n, found, c.Pending())
	}
}
