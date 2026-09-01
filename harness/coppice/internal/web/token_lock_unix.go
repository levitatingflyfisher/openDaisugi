//go:build unix

package web

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockNames takes an exclusive lock on the names file's lock file, so a
// change to the named tokens reads and writes the file whole. It waits
// for a lock another process holds. The returned func releases it.
func (s TokenStore) lockNames() (func(), error) {
	path := s.NamesPath() + ".lock"
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
