package server

import (
	"os"
	"path/filepath"
	"testing"
)

// StartLockHeld probes without taking ownership: free before anyone starts,
// held while a real server holds the lock, free again once it releases.
func TestStartLockHeldReportsFreeThenHeldThenFreeAgain(t *testing.T) {
	dir := t.TempDir()

	held, err := StartLockHeld(dir)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("StartLockHeld reported held on a fresh data dir, want free")
	}

	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}

	held, err = StartLockHeld(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("StartLockHeld reported free while a real server holds the lock")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	held, err = StartLockHeld(dir)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("StartLockHeld reported held after Close released the lock")
	}
}

// A probe that acquires the real lock, the way the CLI's old lockIsFree
// helper used to, writes this process's own pid into server.lock on every
// call. A caller polling this in a loop while waiting for a stop must not
// repeatedly overwrite the file with its own pid.
func TestStartLockHeldNeverWritesTheLockFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(dir, "server.sock"), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before, err := os.ReadFile(lockFile(dir))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := StartLockHeld(dir); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(lockFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("StartLockHeld rewrote server.lock:\nbefore: %q\nafter:  %q", before, after)
	}
}
