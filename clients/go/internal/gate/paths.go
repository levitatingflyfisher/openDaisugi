package gate

import (
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/shell"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strings"
	"syscall"
)

// This file ports the posixpath and pathlib behavior the Python gate
// relies on, in Python 3.12's exact form, so that every path check lands
// on the same string.

func isabs(p string) bool { return strings.HasPrefix(p, "/") }

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
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(out) == 0) || (len(out) > 0 && out[len(out)-1] == "..") {
			out = append(out, c)
		} else if len(out) > 0 {
			out = out[:len(out)-1]
		}
	}
	s := strings.Repeat("/", initial) + strings.Join(out, "/")
	if s == "" {
		return "."
	}
	return s
}

// join is posixpath.join.
func join(a string, rest ...string) string {
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

// split is posixpath.split.
func split(p string) (string, string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail := p[:i], p[i:]
	if head != "" && head != strings.Repeat("/", len(head)) {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

// basename is posixpath.basename.
func basename(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// expanduser is posixpath.expanduser: ~ is HOME (or the password
// entry of this user when HOME is unset), ~name is that user's entry, and
// a name the password database does not know leaves the path as it is.
func (r *runner) expanduser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	i := strings.Index(p[1:], "/")
	if i < 0 {
		i = len(p)
	} else {
		i++
	}
	var home string
	if i == 1 {
		h, ok := r.env["HOME"]
		if !ok {
			u, err := user.Current()
			if err != nil {
				return p
			}
			h = u.HomeDir
		}
		home = h
	} else {
		name := p[1:i]
		if strings.ContainsRune(name, 0) {
			panic(pystr.NewException("ValueError", "embedded null character"))
		}
		if _, err := pystr.EncodeUTF8(name); err != nil {
			panic(err)
		}
		u, err := user.Lookup(name)
		if err != nil {
			return p
		}
		home = u.HomeDir
	}
	out := strings.TrimRight(home, "/") + p[i:]
	if out == "" {
		return "/"
	}
	return out
}

// pathExpanduser is PurePosixPath(p).expanduser() of Python 3.12 on a
// pathStr-normalized p: a first part starting with ~ is expanded, and a
// name it cannot expand raises RuntimeError.
func (r *runner) pathExpanduser(p string) string {
	parts := pathParts(p)
	if len(parts) == 0 || isabs(p) || !strings.HasPrefix(parts[0], "~") {
		return p
	}
	home := r.expanduser(parts[0])
	if strings.HasPrefix(home, "~") {
		panic(pystr.NewException("RuntimeError", "Could not determine home directory."))
	}
	return pathStr(strings.Join(append([]string{home}, parts[1:]...), "/"))
}

// abspath is posixpath.abspath.
func (r *runner) abspath(p string) string {
	if !isabs(p) {
		p = join(r.getwd(), p)
	}
	return normpath(p)
}

func (r *runner) getwd() string {
	if r.cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			unported("the process working directory cannot be read")
		}
		r.cwd = wd
	}
	return r.cwd
}

// realAnswer is one realpath result: the path, or the exception Python
// raises, and how many frames past realpath's own the walk went.
type realAnswer struct {
	out   string
	exc   *pystr.Exception
	extra int
}

// realpath is posixpath.realpath(strict=False) of Python 3.12, called from
// the frame at r.depth.
//
// Python resolves each symlink hop by recursing, so its deepest frame is
// realpath's own plus three plus the number of nested hops: isabs and
// join, and the _get_sep they call, sit under the innermost
// _joinrealpath. A chain of links long enough to pass the recursion limit
// raises RecursionError, here as there.
func (r *runner) realpath(filename string) string {
	// One call's answers are kept for the rest of the call: a long line
	// asks for the same few paths from each cwd many times, and each
	// answer costs an lstat per component. The file system is taken as
	// still for the length of one call, which the oracle also assumes
	// between its own repeated lookups. A worker abandoned at a deadline
	// may still be running, so the cache is locked.
	r.depthLog("realpath", r.depth+1, pystr.Slice(filename, 0, 60))
	r.realMu.Lock()
	a, ok := r.realCache[filename]
	r.realMu.Unlock()
	if !ok {
		a = realpathOf(filename, r.getwd)
		r.realMu.Lock()
		if r.realCache == nil {
			r.realCache = map[string]realAnswer{}
		}
		r.realCache[filename] = a
		r.realMu.Unlock()
	}
	if r.depth+1+a.extra > shell.RecursionLimit {
		panic(pystr.RecursionError())
	}
	if a.exc != nil {
		panic(a.exc)
	}
	return a.out
}

// realpathOf walks filename once and records the answer.
func realpathOf(filename string, getwd func() string) (a realAnswer) {
	if strings.ContainsRune(filename, 0) {
		// os.lstat raises before anything recurses.
		return realAnswer{exc: pystr.NewException("ValueError", "lstat: embedded null character in path"), extra: 3}
	}
	w := &realWalk{}
	a.exc = catch(func() {
		p, _ := w.join("", filename, map[string]*string{}, 1)
		if !isabs(p) {
			p = join(getwd(), p)
		}
		a.out = normpath(p)
	})
	a.extra = w.max + 2
	if a.extra < 3 {
		a.extra = 3
	}
	return a
}

// realWalk tracks the deepest _joinrealpath frame, counted from
// realpath's own frame.
type realWalk struct{ max int }

func (w *realWalk) join(path, rest string, seen map[string]*string, depth int) (string, bool) {
	if depth > w.max {
		w.max = depth
	}
	if depth > shell.RecursionLimit {
		// No caller is shallow enough for Python to get here: the walk
		// ends in RecursionError whatever comes next.
		panic(pystr.RecursionError())
	}
	if isabs(rest) {
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
				var tail string
				path, tail = split(path)
				if tail == ".." {
					path = join(path, "..", "..")
				}
			} else {
				path = ".."
			}
			continue
		}
		newpath := join(path, name)
		isLink := false
		enc, eerr := pystr.FSEncode(newpath)
		if eerr != nil {
			// os.lstat raises UnicodeEncodeError, which strict=False
			// does not catch.
			panic(eerr)
		}
		if st, err := os.Lstat(string(enc)); err == nil {
			isLink = st.Mode()&fs.ModeSymlink != 0
		}
		// strict=False ignores every OSError lstat raises.
		if !isLink {
			path = newpath
			continue
		}
		if v, ok := seen[newpath]; ok {
			if v != nil {
				path = *v
				continue
			}
			return join(newpath, rest), false
		}
		seen[newpath] = nil
		raw, err := os.Readlink(string(enc))
		if err != nil {
			// Python lets this OSError escape.
			panic(osError(err, newpath))
		}
		target := pystr.FSDecode([]byte(raw))
		var ok bool
		path, ok = w.join(path, target, seen, depth+1)
		if !ok {
			return join(path, rest), false
		}
		p := path
		seen[newpath] = &p
	}
	return path, true
}

func isOSError(err error) bool {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		var errno syscall.Errno
		return errors.As(pe.Err, &errno)
	}
	return false
}

// commonpath is posixpath.commonpath for absolute or relative paths of
// one kind. ok is false where Python raises ValueError.
func commonpath(paths ...string) (string, bool) {
	if len(paths) == 0 {
		return "", false
	}
	abs := isabs(paths[0])
	var split [][]string
	for _, p := range paths {
		if isabs(p) != abs {
			return "", false
		}
		var comps []string
		for _, c := range strings.Split(p, "/") {
			if c != "" && c != "." {
				comps = append(comps, c)
			}
		}
		split = append(split, comps)
	}
	s1, s2 := split[0], split[0]
	for _, s := range split[1:] {
		if lessList(s, s1) {
			s1 = s
		}
		if lessList(s2, s) {
			s2 = s
		}
	}
	common := s1
	for i, c := range s1 {
		if c != s2[i] {
			common = s1[:i]
			break
		}
	}
	prefix := ""
	if abs {
		prefix = "/"
	}
	return prefix + strings.Join(common, "/"), true
}

func lessList(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// relpath is posixpath.relpath.
func (r *runner) relpath(path, start string) string {
	sl := nonEmpty(strings.Split(r.abspath(start), "/"))
	pl := nonEmpty(strings.Split(r.abspath(path), "/"))
	i := 0
	for i < len(sl) && i < len(pl) && sl[i] == pl[i] {
		i++
	}
	var rel []string
	for j := i; j < len(sl); j++ {
		rel = append(rel, "..")
	}
	rel = append(rel, pl[i:]...)
	if len(rel) == 0 {
		return "."
	}
	return join(rel[0], rel[1:]...)
}

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// pathStr is str(PurePosixPath(p)): slashes collapsed, "." parts dropped,
// a trailing slash removed, and a leading "//" kept.
func pathStr(p string) string {
	root := ""
	if strings.HasPrefix(p, "/") {
		root = "/"
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			root = "//"
		}
	}
	parts := nonEmpty(strings.Split(p, "/"))
	var keep []string
	for _, c := range parts {
		if c != "." {
			keep = append(keep, c)
		}
	}
	s := root + strings.Join(keep, "/")
	if s == "" {
		return "."
	}
	return s
}

// pathParts is PurePosixPath(p).parts.
func pathParts(p string) []string {
	s := pathStr(p)
	if s == "." {
		return nil
	}
	var out []string
	if strings.HasPrefix(s, "//") {
		out = append(out, "//")
		s = s[2:]
	} else if strings.HasPrefix(s, "/") {
		out = append(out, "/")
		s = s[1:]
	}
	if s != "" {
		out = append(out, strings.Split(s, "/")...)
	}
	return out
}

// pathParent is str(PurePosixPath(p).parent).
func pathParent(p string) string {
	parts := pathParts(p)
	if len(parts) == 0 {
		return "."
	}
	if parts[0] == "/" || parts[0] == "//" {
		if len(parts) <= 2 {
			return parts[0]
		}
		return parts[0] + strings.Join(parts[1:len(parts)-1], "/")
	}
	if len(parts) == 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

// pathJoin is str(PurePosixPath(a) / b).
func pathJoin(a, b string) string {
	if isabs(b) {
		return pathStr(b)
	}
	return pathStr(pathStr(a) + "/" + b)
}

// pathUnderOrEqual reports whether PurePosixPath(p) == d or d is one of
// its parents. Both must be pathStr-normalized.
func pathUnderOrEqual(p, d string) bool {
	if p == d {
		return true
	}
	for {
		parent := pathParent(p)
		if parent == p {
			return false
		}
		if parent == d {
			return true
		}
		p = parent
	}
}

// exists is Path.exists() of Python 3.12: false for a path that cannot
// be encoded and for the errors that mean "no such file"; any other
// OSError is raised.
func exists(p string) bool {
	if strings.ContainsRune(p, 0) {
		return false
	}
	enc, eerr := pystr.FSEncode(p)
	if eerr != nil {
		return false
	}
	_, err := os.Stat(string(enc))
	if err == nil {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOENT, syscall.ENOTDIR, syscall.EBADF, syscall.ELOOP:
			return false
		}
	}
	panic(osError(err, p))
}

// resolve is Path.resolve(strict=False) of Python 3.12: realpath, then a
// stat that turns a symlink loop into RuntimeError.
func (r *runner) resolve(p string) string {
	defer r.enter()()
	// Path(p).resolve() asks realpath about str(Path(p)).
	s := r.realpath(pathStr(p))
	enc, _ := pystr.FSEncode(s)
	if _, err := os.Stat(string(enc)); err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ELOOP {
			panic(pystr.NewException("RuntimeError", "Symlink loop from "+pystr.Repr(s)))
		}
	}
	return pathStr(s)
}

// osError is the OSError Python raises for err on filename: its subclass
// by errno and its "[Errno N] message: 'filename'" text.
func osError(err error, filename string) *pystr.Exception {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return pystr.NewException("OSError", err.Error())
	}
	typ := "OSError"
	switch errno {
	case syscall.ENOENT:
		typ = "FileNotFoundError"
	case syscall.EACCES, syscall.EPERM:
		typ = "PermissionError"
	case syscall.ENOTDIR:
		typ = "NotADirectoryError"
	case syscall.EISDIR:
		typ = "IsADirectoryError"
	case syscall.EEXIST:
		typ = "FileExistsError"
	case syscall.EINTR:
		typ = "InterruptedError"
	}
	return pystr.NewException(typ, fmt.Sprintf("[Errno %d] %s: %s", int(errno), strerror(errno), pystr.Repr(filename)))
}

// strerror is C's strerror text, which Python puts in an OSError; Go's
// table carries glibc's messages with a lower-case first letter.
func strerror(e syscall.Errno) string {
	s := e.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// lexists is os.path.lexists.
func lexists(p string) bool {
	if strings.ContainsRune(p, 0) {
		return false
	}
	enc, eerr := pystr.FSEncode(p)
	if eerr != nil {
		return false
	}
	_, err := os.Lstat(string(enc))
	return err == nil
}
