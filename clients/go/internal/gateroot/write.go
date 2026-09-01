package gateroot

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// maxLinks bounds a symlink chain, as the kernel's ELOOP does.
const maxLinks = 40

// resolveLinks follows p while it is a symlink, a dangling one included,
// and returns the path of the file a write through p lands on.
func resolveLinks(p string) (string, error) {
	for i := 0; i < maxLinks; i++ {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = target
	}
	return "", &os.PathError{Op: "write", Path: p, Err: syscall.ELOOP}
}

var umaskOnce sync.Once
var umask os.FileMode

// processUmask reads the umask from /proc/self/status, which never
// changes it. Only where /proc is missing is it read by setting it, for
// the instant that takes.
func processUmask() os.FileMode {
	umaskOnce.Do(func() {
		if m, ok := procUmask("/proc/self/status"); ok {
			umask = m
			return
		}
		old := syscall.Umask(0o077)
		syscall.Umask(old)
		umask = os.FileMode(old)
	})
	return umask
}

// procUmask reads the "Umask:" line of a /proc status file.
func procUmask(path string) (os.FileMode, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "Umask:"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32)
			if err != nil {
				return 0, false
			}
			return os.FileMode(n) & 0o777, true
		}
	}
	return 0, false
}

// WriteFile writes text to p as Path.write_text does, but atomically: into
// a temporary file in the same directory, renamed over the old one, so a
// reader never sees a half-written file. An existing file keeps its mode;
// a new one gets 0666 less the umask. When p is a symlink, the file it
// points to is written, as Python's write_text does; followed is that
// file's path then, for the caller to say so.
func WriteFile(p, text string) (followed string, err error) {
	target, err := resolveLinks(p)
	if err != nil {
		return "", err
	}
	if target != p {
		followed = target
	}
	return followed, replaceFile(target, text, 0)
}

// WriteFileMode is WriteFile for a private file: the file has mode from
// the moment it exists, as register_envelope's chmod 0600 leaves it.
func WriteFileMode(p, text string, mode os.FileMode) (followed string, err error) {
	target, err := resolveLinks(p)
	if err != nil {
		return "", err
	}
	if target != p {
		followed = target
	}
	return followed, replaceFile(target, text, mode)
}

// ReplaceFile writes text to p atomically. A symlink at p is replaced by
// the new file, never written through.
func ReplaceFile(p, text string) error { return replaceFile(p, text, 0) }

func replaceFile(p, text string, forced os.FileMode) error {
	mode := 0o666 &^ processUmask()
	if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
		mode = st.Mode().Perm()
	} else if err == nil && st.IsDir() {
		return &os.PathError{Op: "write", Path: p, Err: syscall.EISDIR}
	}
	if forced != 0 {
		mode = forced
	}
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	ok = true
	return nil
}
