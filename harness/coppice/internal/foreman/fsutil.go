package foreman

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// The chat is the owner's whole working life: only the owner may read it.
const (
	dirMode  = 0o700
	fileMode = 0o600
)

// readFile and chmod are os.ReadFile and os.Chmod. Tests swap in ones that
// fail.
var (
	readFile = os.ReadFile
	chmod    = os.Chmod
)

// ensureDir makes dir and every missing parent with mode 0700, and
// tightens dir itself to 0700 if it was already there. A parent that was
// already there is left as it is.
func ensureDir(dir string) error {
	var missing []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || filepath.Dir(d) == d {
			break
		}
		missing = append(missing, d)
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	for _, d := range missing {
		if err := os.Chmod(d, dirMode); err != nil {
			return err
		}
	}
	return os.Chmod(dir, dirMode)
}

// tightenFiles sets every regular file in the chat dir, main/ and tree/ to
// 0600 when it is open to the group or others. A file that is narrower is
// left as it is.
func tightenFiles(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, "main"), filepath.Join(dir, "tree")} {
		ents, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o077 != 0 {
				if err := os.Chmod(filepath.Join(d, e.Name()), fileMode); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// syncDir makes a new or renamed entry in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeAtomic replaces path with data: a temp file in the same directory,
// fsync, rename, then fsync the directory. A crash leaves the old file or
// the new one, never a mix.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

// appendFile is the part of *os.File an append uses. Tests swap in one
// that fails.
type appendFile interface {
	Write([]byte) (int, error)
	Sync() error
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Close() error
}

// openAppend opens a log file for appending, creating it 0600.
var openAppend = func(path string) (appendFile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
}

// errCutBack means a failed append could not be cut back off its file. The
// file may now end in a bad line, so the chat must take no more appends.
var errCutBack = errors.New("a failed append could not be removed from the log")

// appendLine appends one line to path, creates it 0600 if needed, and
// fsyncs it before it returns. It returns the offset the line starts at.
//
// When the write or the fsync fails, the file is cut back to its length
// before the append, so the next append starts on a clean end. The cut is
// made durable by the next append's fsync. If the cut fails too, the error
// wraps errCutBack.
func appendLine(path string, line []byte) (int64, error) {
	_, statErr := os.Stat(path)
	f, err := openAppend(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	off := st.Size()
	undo := func(err error) error {
		if terr := f.Truncate(off); terr != nil {
			return fmt.Errorf("%w (%w: %v)", err, errCutBack, terr)
		}
		_ = f.Sync()
		return err
	}
	if _, err := f.Write(line); err != nil {
		return 0, undo(err)
	}
	if err := f.Sync(); err != nil {
		return 0, undo(err)
	}
	if os.IsNotExist(statErr) {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return 0, err
		}
	}
	return off, nil
}

// encodeLine is one JSON value and a newline. HTML characters are not
// escaped, so a port that writes plain JSON writes the same bytes. Go still
// escapes U+2028 and U+2029; the golden fixtures leave them out.
func encodeLine(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// clip is the longest prefix of s that has at most n bytes and ends on a
// rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// split cuts s into parts of at most n bytes, each cut on a rune boundary.
// An empty s is one empty part.
func split(s string, n int) []string {
	if len(s) <= n {
		return []string{s}
	}
	var out []string
	for len(s) > 0 {
		p := clip(s, n)
		if p == "" { // n is smaller than one rune; take the rune whole
			_, size := utf8.DecodeRuneInString(s)
			p = s[:size]
		}
		out = append(out, p)
		s = s[len(p):]
	}
	return out
}
