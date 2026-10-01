package foreman

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// pickySummarizer fails the nodes of the messages whose text starts with a
// name in bad, and counts the calls to each message.
type pickySummarizer struct {
	mu    sync.Mutex
	bad   map[string]bool
	calls map[int64]int
}

func newPicky(bad ...string) *pickySummarizer {
	p := &pickySummarizer{bad: map[string]bool{}, calls: map[int64]int{}}
	for _, b := range bad {
		p.bad[b] = true
	}
	return p
}

func (p *pickySummarizer) Compress(_ context.Context, m Message, limit int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[m.I]++
	if p.bad[strings.SplitN(m.Text, ":", 2)[0]] {
		return "", errFake
	}
	return "c " + clip(m.Text, 20), nil
}

func (p *pickySummarizer) Merge(_ context.Context, id NodeID, l, r string, limit int) (string, error) {
	return "m " + clip(l, 20) + " | " + clip(r, 20), nil
}

func (p *pickySummarizer) count(i int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[i]
}

func (p *pickySummarizer) heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bad = map[string]bool{}
}

func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func logLongs(t *testing.T, c *Chat, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := c.Append(ctx, KindUser, longText(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAFailedNodeStopsTheNodesBehindItUntilItIsBuilt(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	logLongs(t, c, 6)
	s := newPicky("2")
	if err := c.Drain(ctx, s, nil); !errors.Is(err, errFake) {
		t.Fatalf("Drain = %v", err)
	}
	// Messages 3, 4 and 5 are not asked while message 2 fails.
	for _, i := range []int64{3, 4, 5} {
		if s.count(i) != 0 {
			t.Fatalf("message %d was asked %d times", i, s.count(i))
		}
	}
	if c.view.Count != 2 {
		t.Fatalf("the view covers %d messages, want the two before the failed one", c.view.Count)
	}
	// The gap is the failed message and what waits behind it; all of it is
	// in the tail.
	tail, _ := c.Tail()
	if len(tail) != 4 || tail[0].I != 2 {
		t.Fatalf("tail = %d messages from %d", len(tail), tail[0].I)
	}
	s.heal()
	clk.advance(31 * time.Second)
	if err := c.Drain(ctx, s, nil); err != nil || c.Pending() != 0 || c.view.Count != 6 {
		t.Fatalf("after the wait: %v, pending %d, view %d", err, c.Pending(), c.view.Count)
	}
	for _, i := range []int64{3, 4, 5} {
		if s.count(i) != 1 {
			t.Fatalf("message %d was asked %d times, want once", i, s.count(i))
		}
	}
}

func TestAFailedNodeIsAskedAgainOnlyAfterItsWait(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	logLongs(t, c, 1)
	s := newPicky("0")
	drain := func() error { return c.Drain(ctx, s, nil) }
	steps := []struct {
		advance time.Duration
		calls   int
	}{
		{0, 1},                 // the first failure
		{29 * time.Second, 1},  // still waiting 30 s
		{2 * time.Second, 2},   // 31 s: asked again; the next wait is 2 min
		{119 * time.Second, 2}, // still waiting
		{2 * time.Second, 3},   // 2 min 1 s: asked again; the next wait is 5 min
		{299 * time.Second, 3}, // still waiting
		{2 * time.Second, 4},   // 5 min 1 s: asked again; every later wait is 5 min
		{299 * time.Second, 4}, //
		{2 * time.Second, 5},   //
		{time.Second, 5},       //
	}
	for k, st := range steps {
		clk.advance(st.advance)
		for rep := 0; rep < 3; rep++ { // a loop that drains at once calls nothing new
			if err := drain(); !errors.Is(err, errFake) {
				t.Fatalf("step %d: Drain = %v", k, err)
			}
		}
		if got := s.count(0); got != st.calls {
			t.Fatalf("step %d: %d calls, want %d", k, got, st.calls)
		}
	}
}

func TestRetryInSaysWhenTheNextTryIs(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	if _, ok := c.RetryIn(); ok {
		t.Fatal("RetryIn with nothing failed")
	}
	logLongs(t, c, 1)
	_ = c.Drain(ctx, newPicky("0"), nil)
	if d, ok := c.RetryIn(); !ok || d != 30*time.Second {
		t.Fatalf("RetryIn = %v %v", d, ok)
	}
	clk.advance(10 * time.Second)
	if d, _ := c.RetryIn(); d != 20*time.Second {
		t.Fatalf("RetryIn = %v", d)
	}
	clk.advance(time.Minute)
	if d, ok := c.RetryIn(); !ok || d != 0 {
		t.Fatalf("RetryIn after the wait = %v %v", d, ok)
	}
}

func TestAnAppendAsksAFailedNodeAgainAtOnce(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	logLongs(t, c, 1)
	s := newPicky("0")
	_ = c.Drain(ctx, s, nil)
	if s.count(0) != 1 {
		t.Fatalf("calls = %d", s.count(0))
	}
	s.heal()
	if _, err := c.Append(ctx, KindUser, "short"); err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx, s, nil); err != nil || s.count(0) != 2 || c.Pending() != 0 {
		t.Fatalf("Drain = %v, calls %d, pending %d", err, s.count(0), c.Pending())
	}
}

func TestAnAppendEndsTheWaitButKeepsTheCountOfFailures(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	logLongs(t, c, 1)
	s := newPicky("0")
	_ = c.Drain(ctx, s, nil) // first failure: waits 30 s
	if _, err := c.Append(ctx, KindUser, "short"); err != nil {
		t.Fatal(err)
	}
	_ = c.Drain(ctx, s, nil) // asked at once; fails a second time
	if s.count(0) != 2 {
		t.Fatalf("calls = %d", s.count(0))
	}
	if d, ok := c.RetryIn(); !ok || d != 2*time.Minute {
		t.Fatalf("RetryIn = %v %v, want the second step, 2 min", d, ok)
	}
}

func TestAStalledCompactorLosesNoMessageFromTheTail(t *testing.T) {
	clk := newClock()
	c := openT(t, t.TempDir(), nil, clk, nil)
	logLongs(t, c, 30)
	_ = c.Drain(ctx, newPicky("0"), nil)
	tail, err := c.Tail()
	if err != nil || len(tail) != 30 {
		t.Fatalf("tail = %d messages, %v", len(tail), err)
	}
}
