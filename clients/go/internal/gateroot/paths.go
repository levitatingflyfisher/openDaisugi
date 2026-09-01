// Package gateroot is the gate's state directory as opendaisugi.gate lays
// it out: envelopes/ (default.json and one file per session), the DISARMED
// marker, shadow/ (the verdict log, read by package journal) and
// proposals/ (read only). Every file it writes is the file Python writes.
package gateroot

import (
	"os"
	"strings"
	"syscall"
)

// PathStr is str(pathlib.PurePosixPath(p)): repeated slashes collapsed
// (two leading slashes kept), "." parts dropped, no trailing slash.
func PathStr(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		lead = "//"
	case strings.HasPrefix(p, "/"):
		lead = "/"
	}
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." {
			continue
		}
		parts = append(parts, s)
	}
	out := lead + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

// Join is pathlib's a / b for a relative b.
func Join(a, b string) string {
	if a == "." || a == "" {
		return b
	}
	if a == "/" || a == "//" {
		return a + b
	}
	return a + "/" + b
}

// Parent is PurePosixPath(p).parent.
func Parent(p string) string {
	i := strings.LastIndex(p, "/")
	switch {
	case i < 0:
		return "."
	case p == "/" || p == "//":
		return p
	case i == 0:
		return "/"
	case i == 1 && strings.HasPrefix(p, "//"):
		return "//"
	}
	return p[:i]
}

// Getwd is os.getcwd(): the kernel's answer, never $PWD.
func Getwd() (string, error) {
	return syscall.Getwd()
}

// Abs is pathlib's absolute(): cwd joined for a relative path.
func Abs(p string) (string, error) {
	p = PathStr(p)
	if strings.HasPrefix(p, "/") {
		return p, nil
	}
	cwd, err := Getwd()
	if err != nil {
		return "", err
	}
	if p == "." {
		return cwd, nil
	}
	return Join(cwd, p), nil
}

// Resolve is Path.resolve() (strict=False): posixpath.realpath of the
// absolute path. Parts that do not exist are kept as written.
func Resolve(p string) (string, error) {
	abs, err := Abs(p)
	if err != nil {
		return "", err
	}
	return realpath(abs), nil
}

// realpath ports posixpath.realpath(filename, strict=False) for an
// absolute path.
func realpath(filename string) string {
	path, _ := joinrealpath("", filename, map[string]*string{})
	return normpath(path)
}

func joinrealpath(path, rest string, seen map[string]*string) (string, bool) {
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		path = "/"
	}
	for rest != "" {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if path != "" {
				head, tail := split(path)
				path = head
				if tail == ".." {
					path = pjoin(path, "..", "..")
				}
			} else {
				path = ".."
			}
			continue
		}
		newpath := pjoin(path, name)
		st, err := os.Lstat(newpath)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if v, ok := seen[newpath]; ok {
			if v != nil {
				path = *v
				continue
			}
			// A symlink loop: strict=False keeps the rest as written.
			return pjoin(newpath, rest), false
		}
		seen[newpath] = nil
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		var ok bool
		path, ok = joinrealpath(path, target, seen)
		if !ok {
			return pjoin(path, rest), false
		}
		p := path
		seen[newpath] = &p
	}
	return path, true
}

func pjoin(a string, rest ...string) string {
	path := a
	for _, b := range rest {
		switch {
		case strings.HasPrefix(b, "/"):
			path = b
		case path == "" || strings.HasSuffix(path, "/"):
			path += b
		default:
			path += "/" + b
		}
	}
	return path
}

func split(p string) (string, string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail := p[:i], p[i:]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

// normpath is posixpath.normpath.
func normpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, c)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// SafeSessionID is hook._safe_session_id on a string: every character
// outside [A-Za-z0-9._-] becomes "_", dots are stripped from both ends,
// and the result is cut to 128 characters, or "no-session" when empty.
func SafeSessionID(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), ".")
	if len(s) > 128 {
		s = s[:128]
	}
	if s == "" {
		return "no-session"
	}
	return s
}

// exists is Path.exists(): stat follows symlinks; any error is false.
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// mkdirPrivate is gate._mkdir_private: parents with the default mode,
// the directory itself 0700 whether it was made now or not.
func mkdirPrivate(d string) error {
	if err := mkdirParents(d, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(d, 0o700)
	return nil
}

// mkdirParents is Path.mkdir(parents=True, exist_ok=True, mode=mode):
// missing parents get 0777 less the umask, the leaf gets mode.
func mkdirParents(d string, mode os.FileMode) error {
	if st, err := os.Stat(d); err == nil {
		if st.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: d, Err: syscall.EEXIST}
	}
	parent := Parent(d)
	if parent != d && parent != "." {
		if err := mkdirParents(parent, 0o777); err != nil {
			return err
		}
	}
	err := os.Mkdir(d, mode)
	if err != nil && os.IsExist(err) {
		if st, serr := os.Stat(d); serr == nil && st.IsDir() {
			return nil
		}
	}
	return err
}
