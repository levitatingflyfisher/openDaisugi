package pando

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bigStore(t *testing.T, n int, days bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pando", "work")
	clk := newClock()
	c, err := Open(ctx, Config{Dir: dir, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if _, err := c.Append(ctx, KindUser, fmt.Sprintf("message number %d about the blue door", i)); err != nil {
			t.Fatal(err)
		}
		if days && i%7 == 6 {
			clk.t = clk.t.Add(26 * time.Hour)
		} else {
			clk.t = clk.t.Add(time.Minute)
		}
	}
	return dir
}

// The cheap read gives the same bytes as the full read, whatever the shape of
// the store: few messages, many, in one day file or in several.
func TestTheViewContextReadsLessAndGivesTheSameBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int
		days bool
	}{{1, false}, {5, false}, {21, false}, {60, false}, {60, true}, {200, true}} {
		dir := bigStore(t, tc.n, tc.days)
		full, err := OpenReadOnly(dir)
		if err != nil {
			t.Fatal(err)
		}
		want := full.ViewContext()
		got, ok := viewContextFast(dir)
		if !ok {
			t.Fatalf("n=%d days=%v: the fast read gave up on a plain store", tc.n, tc.days)
		}
		if got != want || want == "" {
			t.Fatalf("n=%d days=%v differ:\n--- fast\n%s\n--- full\n%s", tc.n, tc.days, got, want)
		}
		if again, _ := ViewContextOf(dir); again != got {
			t.Fatal("two reads of the same store differ")
		}
	}
}

func TestTheViewContextFallsBackWhenTheStoreIsNotPlain(t *testing.T) {
	dir := bigStore(t, 30, true)
	full, _ := OpenReadOnly(dir)
	want := full.ViewContext()
	// A torn last line is skipped by a reader; both reads agree.
	days, _ := dayFiles(filepath.Join(dir, "main"))
	last := filepath.Join(dir, "main", days[len(days)-1])
	f, _ := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"i":30,"kind":"user","te`)
	f.Close()
	if got, err := ViewContextOf(dir); err != nil || got != want {
		t.Fatalf("torn tail: %v\n%s\nwant\n%s", err, got, want)
	}
	// A line lost from an older file: ids no longer run on from the count.
	first := filepath.Join(dir, "main", days[0])
	b, _ := os.ReadFile(first)
	lines := strings.SplitAfter(string(b), "\n")
	os.WriteFile(first, []byte(strings.Join(lines[1:], "")), 0o600)
	if _, ok := viewContextFast(dir); ok {
		t.Fatal("the fast read trusted a log with a missing message")
	}
	// No store: the error of the full read.
	if _, err := ViewContextOf(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("a missing store gave no error")
	}
}
