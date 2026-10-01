package statefile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
)

// The writers (GR-R-9) work from a directory fd: the root and each
// directory below it are opened without following a link, and the file is
// made by openat (O_CREAT | O_EXCL | O_NOFOLLOW, 0600) under a temp name
// nobody can predict, then renamed over its name inside the same directory.
// A link a concurrent write planted at a name is never written through. The
// gate's own files (checkpoint state, the ask channel) replace whatever
// stands at their name; the operator's files (DISARMED, an envelope, a graft
// rule) refuse a link or a non-file there, so the owner is told. Python
// twin: _state_files.write_state_file.

// WriteRefused is a writer's refusal of a path the reader would refuse. Its
// message is one line: `<path>: <reason> (fix: ...)`.
type WriteRefused struct{ Text string }

func (e *WriteRefused) Error() string { return e.Text }

// makeDirs is _state_files._make_dirs: the fds of the root and each
// directory below it, made 0700 when missing. A link at or below the root,
// or a directory another user owns, is a *WriteRefused. So is a directory
// that group or others can write, as the reader refuses it (GR-R-8), unless
// repair is set, which is the operator's own command: it sets every
// directory to 0700 and says so on stderr when the mode was loose. The
// gate's own writers never repair and never chmod.
func makeDirs(root string, dirs []string, repair bool) ([]int, error) {
	base, enc, ok := rootBase(root)
	if !ok {
		return nil, &os.PathError{Op: "open", Path: root, Err: syscall.EINVAL}
	}
	if err := os.MkdirAll(parentOf(base), 0o777); err != nil {
		return nil, err
	}
	var fds []int
	closeAll := func() { Close(fds) }
	name, parent, path := enc, -1, base
	for i := -1; i < len(dirs); i++ {
		if i >= 0 {
			name, parent, path = dirs[i], fds[len(fds)-1], path+"/"+dirs[i]
		}
		var err error
		if parent < 0 {
			err = syscall.Mkdir(name, 0o700)
		} else {
			err = syscall.Mkdirat(parent, name, 0o700)
		}
		if err != nil && err != syscall.EEXIST {
			closeAll()
			return nil, &os.PathError{Op: "mkdir", Path: path, Err: err}
		}
		var fd int
		if parent < 0 {
			fd, err = syscall.Open(name, dirFlags, 0)
		} else {
			fd, err = syscall.Openat(parent, name, dirFlags, 0)
		}
		if err != nil {
			why, refused := whyNot(parent, name, true, err)
			if !refused {
				why = "cannot be opened"
			}
			closeAll()
			return nil, &WriteRefused{RefusalText(path, why, true)}
		}
		fds = append(fds, fd)
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			closeAll()
			return nil, err
		}
		if int(st.Uid) != Geteuid() {
			closeAll()
			return nil, &WriteRefused{RefusalText(path, "is owned by another user", true)}
		}
		loose := st.Mode&0o022 != 0
		if loose && !repair {
			closeAll()
			return nil, &WriteRefused{RefusalText(path, "is writable by group or others", true)}
		}
		if repair {
			if err := syscall.Fchmod(fd, 0o700); err != nil {
				closeAll()
				return nil, err
			}
			if loose {
				fmt.Fprintf(os.Stderr, "daisugi gate: repaired %s: it was writable by group or others; now 700\n", path)
			}
		}
	}
	return fds, nil
}

// parentOf is Path(base).parent as a string.
func parentOf(base string) string {
	i := strings.LastIndex(base, "/")
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return base[:i]
}

// MakeDirs is _state_files.make_state_dirs. The caller closes the fds.
func MakeDirs(root string, dirs ...string) ([]int, error) { return makeDirs(root, dirs, true) }

// WriteFile is _state_files.write_state_file: text written to
// <root>/<dirs>/<name> (0600) through the directory fd. With replaceLink a
// link or other entry at the name is replaced; without it a link or a
// non-regular file there is a *WriteRefused. It returns the path.
func WriteFile(root string, dirs []string, name, text string, replaceLink bool) (string, error) {
	fds, err := makeDirs(root, dirs, !replaceLink)
	if err != nil {
		return "", err
	}
	defer Close(fds)
	dfd := fds[len(fds)-1]
	path := NoteDir(root, dirs...) + "/" + name
	if !replaceLink {
		if why := nonRegular(dfd, name); why != "" {
			return "", &WriteRefused{RefusalText(path, why, false)}
		}
	}
	const flags = syscall.O_WRONLY | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	var tmp string
	fd := -1
	for i := 0; i < 16; i++ {
		var rnd [6]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return "", err
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
			return "", &WriteRefused{RefusalText(path, "cannot be opened", false)}
		}
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	werr := syscall.Fchmod(fd, 0o600)
	data := []byte(text)
	for werr == nil && len(data) > 0 {
		n, e := syscall.Write(fd, data)
		if e == syscall.EINTR {
			continue
		}
		if e != nil {
			werr = e
			break
		}
		data = data[n:]
	}
	if cerr := syscall.Close(fd); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = syscall.Renameat(dfd, tmp, dfd, name)
	}
	if werr != nil {
		syscall.Unlinkat(dfd, tmp)
		return "", &os.PathError{Op: "write", Path: path, Err: werr}
	}
	return path, nil
}

// nonRegular says why the entry name in dfd is not a regular file ("is a
// symbolic link", "is not a regular file"), or "" when it is one or is not
// there. It does not follow a link.
func nonRegular(dfd int, name string) string {
	fd, err := syscall.Openat(dfd, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return ""
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		return ""
	case syscall.S_IFLNK:
		return "is a symbolic link"
	}
	return "is not a regular file"
}

// openForUnlink is _state_files._open_for_unlink: the directory fds for a
// removal. A missing directory is quiet (nil, ""); a link or a directory
// another user owns is a refusal text.
func openForUnlink(root string, dirs []string) ([]int, string) {
	base, enc, ok := rootBase(root)
	if !ok {
		return nil, ""
	}
	var fds []int
	name, parent, path := enc, -1, base
	for i := -1; i < len(dirs); i++ {
		if i >= 0 {
			name, parent, path = dirs[i], fds[len(fds)-1], path+"/"+dirs[i]
		}
		var fd int
		var err error
		if parent < 0 {
			fd, err = syscall.Open(name, dirFlags, 0)
		} else {
			fd, err = syscall.Openat(parent, name, dirFlags, 0)
		}
		why := ""
		if err != nil {
			w, refused := whyNot(parent, name, true, err)
			if !refused {
				Close(fds)
				return nil, ""
			}
			why = RefusalText(path, w, true)
		} else {
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) == nil && int(st.Uid) == Geteuid() {
				fds = append(fds, fd)
				continue
			}
			syscall.Close(fd)
			why = RefusalText(path, "is owned by another user", true)
		}
		Close(fds)
		return nil, why
	}
	return fds, ""
}

// UnlinkFile is _state_files.unlink_state_file: <root>/<dirs>/<name> removed
// through the directory fd; removed is true when a file was. A missing file,
// directory or root is no error; a refused directory is a *WriteRefused. A
// link at the name is removed as the link it is.
func UnlinkFile(root string, dirs []string, name string) (removed bool, err error) {
	fds, why := openForUnlink(root, dirs)
	if fds == nil {
		if why != "" {
			return false, &WriteRefused{why}
		}
		return false, nil
	}
	defer Close(fds)
	switch err := syscall.Unlinkat(fds[len(fds)-1], name); err {
	case nil:
		return true, nil
	case syscall.ENOENT:
		return false, nil
	default:
		return false, &os.PathError{Op: "unlink", Path: name, Err: err}
	}
}

// VerifyWritten is _state_files.verify_written: a file the operator just
// wrote, read back with the gate's reader. A refusal is a *WriteRefused, so
// a writer never claims success for a file the gate would not read.
func VerifyWritten(root string, dirs []string, name string) error {
	fd, why := Probe(root, dirs, name, -1)
	if fd < 0 {
		if why == "" {
			why = RefusalText(NoteDir(root, dirs...)+"/"+name, "cannot be opened", false)
		}
		return &WriteRefused{why}
	}
	syscall.Close(fd)
	return nil
}

// ListJSON is the sorted names ending in .json in the directory dirFD
// (dotfiles and directories included, as Path.glob("*.json")).
func ListJSON(dirFD int) []string {
	dup, err := syscall.Dup(dirFD)
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	var out []string
	for _, n := range names {
		if strings.HasSuffix(n, ".json") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// RefusedPaths is _state_files.refused_paths: every path under the gate
// root the gate would refuse to read, each as `<path>: <reason> (fix: ...)`:
// the root; then, if it opens, DISARMED, and each of envelopes/, grafts/ and
// checkpoint-state/ with every *.json file in it. A refused directory hides
// what is below it. What is missing is not listed.
func RefusedPaths(root string) []string {
	out := []string{}
	fds, why := ProbeDirs(root)
	if fds == nil {
		if why != "" {
			out = append(out, why)
		}
		return out
	}
	Close(fds)
	if fd, why := Probe(root, nil, "DISARMED", -1); fd >= 0 {
		syscall.Close(fd)
	} else if why != "" {
		out = append(out, why)
	}
	for _, sub := range []string{"envelopes", "grafts", "checkpoint-state"} {
		dfds, why := ProbeDirs(root, sub)
		if dfds == nil {
			if why != "" {
				out = append(out, why)
			}
			continue
		}
		dir := dfds[len(dfds)-1]
		for _, name := range ListJSON(dir) {
			fd, why := ProbeFileIn(dir, NoteDir(root, sub), name, -1)
			if fd >= 0 {
				syscall.Close(fd)
			} else if why != "" {
				out = append(out, why)
			}
		}
		Close(dfds)
	}
	return out
}

// ChmodPrivate is the tail of gate._mkdir_private: the directory at path set
// to 0700 on its own fd, opened without following a link, and only when this
// user owns it. A link at path is left alone, so a link planted there is
// never chmod-ed through (GR-R-9).
func ChmodPrivate(path string) {
	fd, err := syscall.Open(path, dirFlags, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) == nil && int(st.Uid) == Geteuid() {
		_ = syscall.Fchmod(fd, 0o700)
	}
}
