package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// xSummarizer makes every line "x", so a test can build a tree with no model.
type xSummarizer struct{}

func (xSummarizer) Compress(context.Context, foreman.Message, int) (string, error) { return "x", nil }
func (xSummarizer) Merge(context.Context, foreman.NodeID, string, string, int) (string, error) {
	return "x", nil
}

func snapFixture(t *testing.T) (*foremanChat, func(n int)) {
	t.Helper()
	c, err := foreman.Open(context.Background(), foreman.Config{Dir: t.TempDir(), ViewMax: 1 << 20, ViewMin: 1 << 19})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	next := 0
	add := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := c.Append(context.Background(), foreman.KindWork, fmt.Sprintf("[w] line %d %s", next, strings.Repeat("y", 90))); err != nil {
				t.Fatal(err)
			}
			next++
		}
		if err := c.Drain(context.Background(), xSummarizer{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	return &foremanChat{chat: c}, add
}

func split(fc *foremanChat) (snap, added string) {
	a, b := fc.splitView(fc.chat.ViewText(escapeTurnTags, -1))
	return string(a), string(b)
}

func TestTheSnapshotIsFrozenAgainWhenTheAddedLinesPass16KiB(t *testing.T) {
	fc, add := snapFixture(t)
	add(20)
	s1, a1 := split(fc)
	if s1 == "" || a1 != "" {
		t.Fatalf("the first split: snapshot %d bytes, added %d", len(s1), len(a1))
	}
	// A few lines more: the snapshot stays, the lines go in the added part.
	add(10)
	s2, a2 := split(fc)
	if s2 != s1 || a2 == "" || len(a2) > snapshotGrowBytes {
		t.Fatalf("after a few lines: snapshot changed %v, added %d bytes", s2 != s1, len(a2))
	}
	// Past 16 KiB of added lines: one fresh snapshot takes them all.
	add(200)
	s3, a3 := split(fc)
	if s3 == s1 || len(a3) != 0 || !strings.HasPrefix(s3, s1) {
		t.Fatalf("after many lines: snapshot %d bytes, added %d", len(s3), len(a3))
	}
	// And it stays until the next 16 KiB.
	add(5)
	s4, a4 := split(fc)
	if s4 != s3 || a4 == "" {
		t.Fatalf("after five lines: snapshot changed %v, added %d", s4 != s3, len(a4))
	}
}
