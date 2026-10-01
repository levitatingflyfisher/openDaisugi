package foreman

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errInjected = errors.New("injected disk error")

// failFile fails one append: mid-line on Write, or at Sync.
type failFile struct {
	*os.File
	mode string
}

func (f *failFile) Write(p []byte) (int, error) {
	if f.mode == "write" {
		n, _ := f.File.Write(p[:len(p)/2])
		return n, errInjected
	}
	return f.File.Write(p)
}

func (f *failFile) Sync() error {
	if f.mode == "sync" {
		return errInjected
	}
	return f.File.Sync()
}

// failNextLogAppend makes the next append to the message log fail in mode.
func failNextLogAppend(t *testing.T, mode string) {
	t.Helper()
	real := openAppend
	t.Cleanup(func() { openAppend = real })
	openAppend = func(path string) (appendFile, error) {
		f, err := real(path)
		if err != nil || !strings.Contains(filepath.ToSlash(path), "/main/") {
			return f, err
		}
		openAppend = real
		return &failFile{File: f.(*os.File), mode: mode}, nil
	}
}

func TestFailedAppendLeavesACleanLog(t *testing.T) {
	for _, mode := range []string{"write", "sync"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			clk := newClock()
			c := openT(t, dir, &fakeSummarizer{}, clk, nil)
			mustAppend(t, c, KindUser, "one")
			before := readAll(t, filepath.Join(dir, "main"))

			failNextLogAppend(t, mode)
			if _, err := c.Append(ctx, KindUser, "two"); !errors.Is(err, errInjected) {
				t.Fatalf("Append = %v, want the injected error", err)
			}
			if c.Count() != 1 {
				t.Fatalf("Count = %d after a failed append", c.Count())
			}
			if got := readAll(t, filepath.Join(dir, "main")); got != before {
				t.Fatalf("the failed append left bytes in the log:\n%s", got)
			}
			ids := mustAppend(t, c, KindUser, "three")
			if len(ids) != 1 || ids[0] != 1 {
				t.Fatalf("next append got ids %v", ids)
			}
			c.Close()
			c2 := openT(t, dir, &fakeSummarizer{}, clk, nil)
			if m, err := c2.Message(1); err != nil || m.Text != "three" || c2.Count() != 2 {
				t.Fatalf("after reopen: count %d, message 1 %+v, %v", c2.Count(), m, err)
			}
		})
	}
}

// When a split append fails part way, the parts already logged are in the
// view, so the view still covers the whole log.
func TestFailedSplitAppendKeepsTheViewWhole(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	c := openT(t, dir, &fakeSummarizer{}, clk, func(cfg *Config) { cfg.SplitBytes = 4 })
	real := openAppend
	calls := 0
	openAppend = func(path string) (appendFile, error) {
		f, err := real(path)
		if err != nil || !strings.Contains(filepath.ToSlash(path), "/main/") {
			return f, err
		}
		calls++
		if calls == 2 {
			return &failFile{File: f.(*os.File), mode: "write"}, nil
		}
		return f, nil
	}
	defer func() { openAppend = real }()
	ids, err := c.Append(ctx, KindUser, "aaaabbbbcccc")
	if !errors.Is(err, errInjected) || len(ids) != 1 {
		t.Fatalf("Append = %v, %v", ids, err)
	}
	openAppend = real
	coverage := func(c *Chat) int64 {
		next := int64(0)
		for _, id := range c.ViewLines() {
			next = id.Last() + 1
		}
		return next
	}
	if coverage(c) != c.Count() {
		t.Fatalf("the view covers %d of %d messages", coverage(c), c.Count())
	}
	mustAppend(t, c, KindUser, "dddd")
	if coverage(c) != c.Count() || c.Count() != 2 {
		t.Fatalf("after the next append: view covers %d of %d", coverage(c), c.Count())
	}
	c.Close()
	openT(t, dir, &fakeSummarizer{}, clk, func(cfg *Config) { cfg.SplitBytes = 4 })
}
