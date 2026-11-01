package steward

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"daisugi-verify/pando"
)

var ctx = context.Background()

// testClock is a clock that a test moves by hand.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 8, 23, 58, 0, 0, time.UTC)}
}

// fakeSummarizer is the deterministic summarizer of the pando tests.
type fakeSummarizer struct{ mu sync.Mutex }

func (f *fakeSummarizer) Compress(_ context.Context, m pando.Message, limit int) (string, error) {
	return "c " + pando.Clip(m.Text, 60), nil
}

func (f *fakeSummarizer) Merge(_ context.Context, id pando.NodeID, left, right string, limit int) (string, error) {
	return "m " + pando.Clip(left, 40) + " | " + pando.Clip(right, 40), nil
}

func openT(t *testing.T, dir string, s pando.Summarizer, clk *testClock, tweak func(*pando.Config)) *pando.Chat {
	t.Helper()
	cfg := pando.Config{Dir: dir, Now: clk.now}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := pando.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sums.Store(c, s)
	t.Cleanup(func() { c.Close() })
	return c
}

var sums sync.Map

func mustAppend(t *testing.T, c *pando.Chat, kind, text string) []int64 {
	t.Helper()
	ids, err := c.Append(ctx, kind, text)
	if err != nil {
		t.Fatalf("Append(%s): %v", kind, err)
	}
	s, ok := sums.Load(c)
	if !ok {
		s = &fakeSummarizer{}
	}
	if err := c.Drain(ctx, s.(pando.Summarizer), nil); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	return ids
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
		b.WriteString("== " + rel + "\n" + string(data))
	}
	return b.String()
}
