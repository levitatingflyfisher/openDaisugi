package server

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newSettleServer(t *testing.T, settle time.Duration) (*Server, *atomic.Int64) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir,
		ForemanDir: filepath.Join(dir, "state", "coppice", "foreman"), HookHome: dir,
		LayoutSaveSettle: settle})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	s.beforeDataWrite = func() { writes.Add(1) }
	t.Cleanup(func() { _ = s.Close() })
	return s, &writes
}

func TestLayoutBurstGivesOneWriteAfterSettle(t *testing.T) {
	s, writes := newSettleServer(t, 80*time.Millisecond)
	for i := 0; i < 5; i++ {
		s.markLayoutDirty()
		time.Sleep(10 * time.Millisecond)
	}
	if n := writes.Load(); n != 0 {
		t.Fatalf("wrote %d times before the settle time passed", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for writes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := writes.Load(); n != 1 {
		t.Fatalf("a burst of 5 changes gave %d writes, want 1", n)
	}
}

func TestLayoutSecondBurstAfterASaveStillDebounces(t *testing.T) {
	s, writes := newSettleServer(t, 60*time.Millisecond)
	s.markLayoutDirty()
	time.Sleep(300 * time.Millisecond)
	if n := writes.Load(); n != 1 {
		t.Fatalf("first burst gave %d writes, want 1", n)
	}
	for i := 0; i < 5; i++ {
		s.markLayoutDirty()
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("second burst wrote at once: %d writes, want 1 until settle", n)
	}
	time.Sleep(300 * time.Millisecond)
	if n := writes.Load(); n != 2 {
		t.Fatalf("second burst gave %d writes in all, want 2", n)
	}
}

func TestLayoutPendingSaveFlushesOnClose(t *testing.T) {
	s, writes := newSettleServer(t, time.Hour)
	s.markLayoutDirty()
	if n := writes.Load(); n != 0 {
		t.Fatalf("wrote %d times before Close", n)
	}
	_ = s.Close()
	if n := writes.Load(); n != 1 {
		t.Fatalf("Close gave %d writes, want 1", n)
	}
}

func TestLayoutPendingSaveIsWrittenBeforeTeardown(t *testing.T) {
	s, writes := newSettleServer(t, time.Hour)
	var atTeardown atomic.Int64
	atTeardown.Store(-1)
	s.afterTeardownSnapshot = func() { atTeardown.CompareAndSwap(-1, writes.Load()) }
	s.markLayoutDirty()
	_ = s.Close()
	if n := atTeardown.Load(); n != 1 {
		t.Fatalf("%d writes had happened when teardown began, want 1", n)
	}
}

func TestLayoutChangeAfterFlushWritesAtOnce(t *testing.T) {
	s, writes := newSettleServer(t, time.Hour)
	s.flushLayout()
	s.markLayoutDirty()
	if n := writes.Load(); n != 1 {
		t.Fatalf("a change after the flush gave %d writes at once, want 1", n)
	}
}
