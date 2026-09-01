//go:build unix

package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockFile is <data dir>/server.lock. The lock is an flock on an open file
// description, so it is released by the kernel when the process dies, however
// it dies. A pid file alone cannot do that: a stale pid file after a crash
// would block every future start.
func lockFile(dataDir string) string { return filepath.Join(dataDir, "server.lock") }

type startLock struct{ f *os.File }

// acquireStartLock takes the exclusive lock without blocking. On contention it
// returns the pid recorded by the holder, so the caller can name it.
func acquireStartLock(dataDir string) (*startLock, int, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, 0, err
	}
	path := lockFile(dataDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readLockPID(path)
		_ = f.Close()
		return nil, pid, ErrAlreadyRunning
	}
	// We hold it. Record our pid for the next caller's error message.
	if err := f.Truncate(0); err != nil {
		_ = releaseLock(f)
		return nil, 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = releaseLock(f)
		return nil, 0, err
	}
	return &startLock{f: f}, 0, nil
}

// StartLockHeld reports whether dataDir's start lock is currently held by
// another process, without taking ownership of it: it flocks, then unlocks
// at once, and never truncates or writes the lock file. A caller that
// instead probed by acquiring the real lock through AcquireStartLock would
// write its own pid into server.lock on every successful probe - harmless
// once, but pointless churn on a file nobody but the next real start reads,
// and it briefly names the wrong process as the holder.
func StartLockHeld(dataDir string) (bool, error) {
	path := lockFile(dataDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

func releaseLock(f *os.File) error {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

func (l *startLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := releaseLock(l.f)
	l.f = nil
	return err
}

// readLockPID is best effort. An unreadable or empty file means "somebody has
// it and did not say who", which is still a refusal.
func readLockPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
