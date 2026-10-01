//go:build linux

// Package gatefs opens the files under the daisugi gate root as the gate
// does (GR-R-1, GR-R-7 to GR-R-10), so coppice reads and writes only what
// the gate would read, and says so when the gate would refuse.
//
// It is a COPY of the checks in the daisugi module's internal/statefile
// (clients/go), because coppice is a separate module. The two must agree:
// the root is opened as a directory without following a symlink at its last
// component (the path down to the root may follow links), each directory
// below it by the fd of the one above, and each file by its fd. A directory
// must be this user's and writable by no group and no other user. A file
// must be a regular file this user owns, with one link, writable by no
// group and no other user. The reasons and the fix words are the gate's.
package gatefs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"syscall"
)

const (
	dirFlags  = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fileFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	oPath     = 0x200000
)

// Geteuid is syscall.Geteuid, a variable so a test can name another owner.
var Geteuid = syscall.Geteuid

// Quote is the path in single quotes, a quote inside it written as quote,
// backslash, quote, quote.
func Quote(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }

// Refusal is `<path>: <why> (fix: ...)`, the gate's words.
func Refusal(p, why string, isDir bool) string {
	q := Quote(p)
	var fix string
	switch {
	case why == "is a symbolic link":
		fix = "remove the link " + q
	case why == "is not a directory" || why == "is not a regular file":
		fix = "move " + q + " aside"
	case why == "is owned by another user":
		fix = `chown "$(id -un)" ` + q
	case why == "has more than one hard link":
		fix = "cp " + q + " " + q + ".new && mv " + q + ".new " + q
	case why == "is writable by group or others" && isDir:
		fix = "chmod 700 " + q
	case why == "is writable by group or others":
		fix = "chmod 600 " + q
	case strings.HasPrefix(why, "is larger than "):
		fix = "shorten or remove " + q
	default:
		fix = "check that this user can read " + q
	}
	return p + ": " + why + " (fix: " + fix + ")"
}

// Refused is an error that carries a refusal: a path the gate would refuse.
type Refused struct{ Text string }

func (e *Refused) Error() string { return e.Text }

// whyNot says why an open failed: "" when the path is plainly missing.
func whyNot(parent int, name string, wantDir bool) string {
	const f = oPath | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	var fd int
	var err error
	if parent < 0 {
		fd, err = syscall.Open(name, f, 0)
	} else {
		fd, err = syscall.Openat(parent, name, f, 0)
	}
	if err == syscall.ENOENT || err == syscall.ENOTDIR {
		return ""
	}
	if err != nil {
		return "cannot be opened"
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return "cannot be opened"
	}
	switch {
	case st.Mode&syscall.S_IFMT == syscall.S_IFLNK:
		return "is a symbolic link"
	case wantDir && st.Mode&syscall.S_IFMT != syscall.S_IFDIR:
		return "is not a directory"
	}
	return "cannot be opened"
}

func openDir(parent int, name, shown string) (fd int, refusal string) {
	var err error
	if parent < 0 {
		fd, err = syscall.Open(name, dirFlags, 0)
	} else {
		fd, err = syscall.Openat(parent, name, dirFlags, 0)
	}
	if err != nil {
		if why := whyNot(parent, name, true); why != "" {
			return -1, Refusal(shown, why, true)
		}
		return -1, ""
	}
	var st syscall.Stat_t
	why := ""
	switch {
	case syscall.Fstat(fd, &st) != nil:
		why = "cannot be opened"
	case int(st.Uid) != Geteuid():
		why = "is owned by another user"
	case st.Mode&0o022 != 0:
		why = "is writable by group or others"
	default:
		return fd, ""
	}
	syscall.Close(fd)
	return -1, Refusal(shown, why, true)
}

func closeAll(fds []int) {
	for _, f := range fds {
		syscall.Close(f)
	}
}

// OpenDirs opens root and each directory below it. It returns the fds
// (nil when one is missing or refused) and the refusal text ("" when
// nothing is refused). The caller closes the fds.
func OpenDirs(root string, dirs ...string) ([]int, string) {
	root = norm(root)
	fd, why := openDir(-1, root, root)
	if fd < 0 {
		return nil, why
	}
	fds := []int{fd}
	shown := root
	for _, d := range dirs {
		shown += "/" + d
		next, why := openDir(fds[len(fds)-1], d, shown)
		if next < 0 {
			closeAll(fds)
			return nil, why
		}
		fds = append(fds, next)
	}
	return fds, ""
}

// openFileIn opens name in the directory dirFD with the file checks.
func openFileIn(dirFD int, shownDir, name string, maxBytes int64) (int, string) {
	shown := shownDir + "/" + name
	fd, err := syscall.Openat(dirFD, name, fileFlags, 0)
	if err != nil {
		if why := whyNot(dirFD, name, false); why != "" {
			return -1, Refusal(shown, why, false)
		}
		return -1, ""
	}
	var st syscall.Stat_t
	why := ""
	switch {
	case syscall.Fstat(fd, &st) != nil:
		why = "cannot be opened"
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		why = "is not a regular file"
	case int(st.Uid) != Geteuid():
		why = "is owned by another user"
	case st.Nlink != 1:
		why = "has more than one hard link"
	case st.Mode&0o022 != 0:
		why = "is writable by group or others"
	case maxBytes >= 0 && st.Size > maxBytes:
		why = fmt.Sprintf("is larger than %d bytes", maxBytes)
	default:
		return fd, ""
	}
	syscall.Close(fd)
	return -1, Refusal(shown, why, false)
}

// Read reads <root>/<dirs>/<name> as the gate reads it. ok is false when
// the file is missing or refused; refusal is "" for missing and the
// refusal text otherwise. At most maxBytes are read when maxBytes >= 0
// (a larger file is refused).
func Read(root string, dirs []string, name string, maxBytes int64) (data []byte, ok bool, refusal string) {
	root = norm(root)
	fds, why := OpenDirs(root, dirs...)
	if fds == nil {
		return nil, false, why
	}
	defer closeAll(fds)
	fd, why := openFileIn(fds[len(fds)-1], root+joinDirs(dirs), name, maxBytes)
	if fd < 0 {
		return nil, false, why
	}
	defer syscall.Close(fd)
	var out []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, false, Refusal(root+joinDirs(dirs)+"/"+name, "cannot be opened", false)
		}
		if n == 0 {
			return out, true, ""
		}
		out = append(out, buf[:n]...)
	}
}

func joinDirs(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	return "/" + strings.Join(dirs, "/")
}

// norm is str(Path(root)) as the gate cleans a root: empty and "." parts
// dropped, ".." kept. It makes a trailing slash or a "." part harmless: the
// kernel resolves a symlink at the last component of a path that ends in
// one, and the no-follow open would then follow it.
func norm(root string) string {
	var parts []string
	for _, p := range strings.Split(root, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	out := strings.Join(parts, "/")
	if strings.HasPrefix(root, "/") {
		return "/" + out
	}
	if out == "" {
		return "."
	}
	return out
}

// MakeDirs opens root and each directory below it, making a missing one
// 0700. A link at or below the root, a directory another user owns, or one
// that group or others can write, is a *Refused: nothing is chmod-ed.
func MakeDirs(root string, dirs ...string) ([]int, error) {
	root = norm(root)
	var fds []int
	name, parent, shown := root, -1, root
	for i := -1; i < len(dirs); i++ {
		if i >= 0 {
			name, parent, shown = dirs[i], fds[len(fds)-1], shown+"/"+dirs[i]
		}
		var err error
		if parent < 0 {
			err = syscall.Mkdir(name, 0o700)
		} else {
			err = syscall.Mkdirat(parent, name, 0o700)
		}
		if err != nil && err != syscall.EEXIST {
			closeAll(fds)
			return nil, &os.PathError{Op: "mkdir", Path: shown, Err: err}
		}
		var fd int
		if parent < 0 {
			fd, err = syscall.Open(name, dirFlags, 0)
		} else {
			fd, err = syscall.Openat(parent, name, dirFlags, 0)
		}
		if err != nil {
			why := whyNot(parent, name, true)
			if why == "" {
				why = "cannot be opened"
			}
			closeAll(fds)
			return nil, &Refused{Refusal(shown, why, true)}
		}
		fds = append(fds, fd)
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			closeAll(fds)
			return nil, err
		}
		if int(st.Uid) != Geteuid() {
			closeAll(fds)
			return nil, &Refused{Refusal(shown, "is owned by another user", true)}
		}
		if st.Mode&0o022 != 0 {
			// GR-R-8: a directory the gate refuses stays refused. Coppice
			// does not repair it; the owner runs the fix the note names.
			closeAll(fds)
			return nil, &Refused{Refusal(shown, "is writable by group or others", true)}
		}
	}
	return fds, nil
}

// Write writes data to <root>/<dirs>/<name> (0600) through the directory
// fd: a temp file made O_CREAT|O_EXCL|O_NOFOLLOW under a name nobody can
// predict, then renamed inside the directory, so a link planted at the name
// is replaced and never written through. A directory the gate refuses is a
// *Refused, and nothing is written.
func Write(root string, dirs []string, name string, data []byte) error {
	root = norm(root)
	fds, err := MakeDirs(root, dirs...)
	if err != nil {
		return err
	}
	defer closeAll(fds)
	dfd := fds[len(fds)-1]
	const flags = syscall.O_WRONLY | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	var tmp string
	fd := -1
	for i := 0; i < 16; i++ {
		var rnd [6]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		tmp = "." + name + "." + hex.EncodeToString(rnd[:]) + ".tmp"
		fd, err = syscall.Openat(dfd, tmp, flags, 0o600)
		if err == syscall.EEXIST {
			continue
		}
		break
	}
	if fd < 0 {
		if err == nil || err == syscall.EEXIST {
			err = syscall.EEXIST
		}
		return &os.PathError{Op: "open", Path: root + joinDirs(dirs) + "/" + name, Err: err}
	}
	werr := syscall.Fchmod(fd, 0o600)
	rest := data
	for werr == nil && len(rest) > 0 {
		n, e := syscall.Write(fd, rest)
		if e == syscall.EINTR {
			continue
		}
		if e != nil {
			werr = e
			break
		}
		rest = rest[n:]
	}
	if cerr := syscall.Close(fd); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = syscall.Renameat(dfd, tmp, dfd, name)
	}
	if werr != nil {
		syscall.Unlinkat(dfd, tmp)
		return &os.PathError{Op: "write", Path: root + joinDirs(dirs) + "/" + name, Err: werr}
	}
	return nil
}
