// Package statefile is how the gate opens the files it keeps under its
// root (GR-R-1, GR-R-2). It is the twin of src/opendaisugi/_state_files.py
// and of the Rust module gate::statefile; the three do the same thing and
// write the same words.
//
// The gate root is opened as a directory without following a symlink at
// its last component (the path down to the root may follow links), then
// each name below it is opened by the fd of the directory above. A
// directory must be this user's and writable by no group and no other
// user; a file must be a regular file this user owns, with one link,
// writable by no group and no other user. A refused file reads as absent:
// DISARMED as not disarmed, an envelope as none, a marker as none. A
// refusal that is not a plain "missing" leaves one stderr note, once per
// file and reason in a process.
package statefile

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"

	"daisugi-verify/internal/pystr"
)

// Geteuid is os.Geteuid, a variable so a test can name another owner.
var Geteuid = os.Geteuid

const (
	dirFlags  = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fileFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	// oPath is O_PATH, which the syscall package does not name (the same
	// value on every Linux port).
	oPath = 0x200000
)

var (
	notedMu sync.Mutex
	noted   = map[string]bool{}
	// collected holds a gate child's notes until it has its answer. A hook
	// call is decided in a child whose stderr nobody reads, and the
	// answer's stderr is what the host sees, so the notes go in front of
	// it (see ChildFrame). Nil outside a child: a note goes to stderr at
	// once.
	collected  []byte
	collecting bool
)

// Note is _state_files.state_note: `daisugi gate: not reading <refusal
// text>` on stderr, once per text.
func Note(text string) {
	line := "daisugi gate: not reading " + text
	notedMu.Lock()
	defer notedMu.Unlock()
	if noted[line] {
		return
	}
	noted[line] = true
	if collecting {
		collected = append(collected, line+"\n"...)
		return
	}
	fmt.Fprintln(os.Stderr, line)
}

// Quote is _state_files.quote: the path in single quotes, a quote inside
// it written as quote, backslash, quote, quote. Always quoted, so the three
// ports write the same bytes.
func Quote(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }

// FixFor is _state_files.fix_for: what the owner does about a refusal.
func FixFor(why, p string, isDir bool) string {
	q := Quote(p)
	switch {
	case why == "is a symbolic link":
		return "remove the link " + q
	case why == "is not a directory" || why == "is not a regular file":
		return "move " + q + " aside"
	case why == "is owned by another user":
		return `chown "$(id -un)" ` + q
	case why == "has more than one hard link":
		return "cp " + q + " " + q + ".new && mv " + q + ".new " + q
	case why == "is writable by group or others":
		if isDir {
			return "chmod 700 " + q
		}
		return "chmod 600 " + q
	case strings.HasPrefix(why, "is larger than "):
		return "shorten or remove " + q
	}
	return "check that this user can read " + q
}

// RefusalText is _state_files.refusal_text: `<path>: <why> (fix: ...)`.
func RefusalText(p, why string, isDir bool) string {
	return p + ": " + why + " (fix: " + FixFor(why, p, isDir) + ")"
}

// refuse leaves the note and returns the refusal text.
func refuse(notePath, why string, isDir bool) string {
	text := RefusalText(notePath, why, isDir)
	Note(text)
	return text
}

// CollectNotes keeps the notes instead of printing them (a gate child).
func CollectNotes() {
	notedMu.Lock()
	collecting, collected = true, nil
	notedMu.Unlock()
}

// TakeNotes returns the notes kept since CollectNotes and ends keeping them.
func TakeNotes() []byte {
	notedMu.Lock()
	defer notedMu.Unlock()
	out := collected
	collecting, collected = false, nil
	return out
}

// GiveNotes prints the notes a child gave, each line once in this process
// (a resident server's own stderr, as the Python server's notes are).
func GiveNotes(notes []byte) {
	notedMu.Lock()
	defer notedMu.Unlock()
	for _, line := range strings.Split(strings.TrimSuffix(string(notes), "\n"), "\n") {
		if line == "" || noted[line] {
			continue
		}
		noted[line] = true
		fmt.Fprintln(os.Stderr, line)
	}
}

// ResetNotes forgets which notes were given, for a test.
func ResetNotes() {
	notedMu.Lock()
	noted = map[string]bool{}
	notedMu.Unlock()
}

// whyNot is _state_files._why_not_opened: why an open of name failed, with
// refused false when it is plainly missing. wantDir names a directory.
// openErr is the error of that open: ENOENT with no-follow flags means the
// name did not exist when it was opened, and that stands, because a writer
// may rename a file into place a moment later and a second look at the name
// would call a plain miss a refusal.
func whyNot(parent int, name string, wantDir bool, openErr error) (why string, refused bool) {
	if openErr == syscall.ENOENT {
		return "", false
	}
	var fd int
	var err error
	const f = oPath | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if parent < 0 {
		fd, err = syscall.Open(name, f, 0)
	} else {
		fd, err = syscall.Openat(parent, name, f, 0)
	}
	if err == syscall.ENOENT || err == syscall.ENOTDIR {
		return "", false
	}
	if err != nil {
		return "cannot be opened", true
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return "cannot be opened", true
	}
	switch {
	case st.Mode&syscall.S_IFMT == syscall.S_IFLNK:
		return "is a symbolic link", true
	case wantDir && st.Mode&syscall.S_IFMT != syscall.S_IFDIR:
		return "is not a directory", true
	}
	return "cannot be opened", true
}

// openDir is _state_files._open_dir: an fd for the directory name under
// parent (a path when parent is -1) with no refusal; or -1 and "" when it
// is missing; or -1 and the refusal text.
func openDir(parent int, name, notePath string) (int, string) {
	var fd int
	var err error
	if parent < 0 {
		fd, err = syscall.Open(name, dirFlags, 0)
	} else {
		fd, err = syscall.Openat(parent, name, dirFlags, 0)
	}
	if err != nil {
		if why, refused := whyNot(parent, name, true, err); refused {
			return -1, refuse(notePath, why, true)
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
	return -1, refuse(notePath, why, true)
}

// rootBase is the root as a note spells it and as a path is opened; false
// when it cannot be a path.
func rootBase(root string) (base, enc string, ok bool) {
	base = norm(root)
	if strings.ContainsRune(base, 0) {
		return "", "", false
	}
	b, eerr := pystr.FSEncode(base)
	if eerr != nil {
		return "", "", false
	}
	return base, string(b), true
}

// ProbeDirs is _state_files.probe_state_dirs: the fds of the gate root and
// each directory below it, in order, with no refusal; nil and "" when one
// is missing; nil and the refusal when one is refused (the ones opened are
// closed). The caller closes the fds with Close.
func ProbeDirs(root string, dirs ...string) ([]int, string) {
	base, enc, ok := rootBase(root)
	if !ok {
		return nil, ""
	}
	rfd, why := openDir(-1, enc, base)
	if rfd < 0 {
		return nil, why
	}
	fds := []int{rfd}
	rel := base
	for _, d := range dirs {
		rel += "/" + d
		dfd, why := openDir(fds[len(fds)-1], d, rel)
		if dfd < 0 {
			Close(fds)
			return nil, why
		}
		fds = append(fds, dfd)
	}
	return fds, ""
}

// OpenDirs is ProbeDirs without the refusal.
func OpenDirs(root string, dirs ...string) ([]int, bool) {
	fds, _ := ProbeDirs(root, dirs...)
	return fds, fds != nil
}

// Close closes every fd.
func Close(fds []int) {
	for _, f := range fds {
		syscall.Close(f)
	}
}

// ProbeFileIn is _state_files.probe_file_in: an fd for name in the
// directory dirFD (called noteDir in a note) with no refusal; -1 and "" when
// it is missing; -1 and the refusal text when it is refused, which also
// leaves a note. The caller closes the fd. maxBytes < 0 sets no size limit.
func ProbeFileIn(dirFD int, noteDir, name string, maxBytes int64) (int, string) {
	notePath := noteDir + "/" + name
	fd, err := syscall.Openat(dirFD, name, fileFlags, 0)
	if err != nil {
		if why, refused := whyNot(dirFD, name, false, err); refused {
			return -1, refuse(notePath, why, false)
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
	return -1, refuse(notePath, why, false)
}

// OpenFileIn is ProbeFileIn without the refusal.
func OpenFileIn(dirFD int, noteDir, name string, maxBytes int64) (int, bool) {
	fd, _ := ProbeFileIn(dirFD, noteDir, name, maxBytes)
	return fd, fd >= 0
}

// Probe is _state_files.probe_state_file: an fd for <root>/<dirs>/<name>
// with no refusal; -1 and "" when it is missing; -1 and the refusal when
// the root, a directory or the file is refused.
func Probe(root string, dirs []string, name string, maxBytes int64) (int, string) {
	fds, why := ProbeDirs(root, dirs...)
	if fds == nil {
		return -1, why
	}
	defer Close(fds)
	return ProbeFileIn(fds[len(fds)-1], NoteDir(root, dirs...), name, maxBytes)
}

// Open is Probe without the refusal. The caller closes the fd with
// syscall.Close.
func Open(root string, dirs []string, name string, maxBytes int64) (int, bool) {
	fd, _ := Probe(root, dirs, name, maxBytes)
	return fd, fd >= 0
}

// NoteDir is the directory <root>/<dirs> as a note spells it.
func NoteDir(root string, dirs ...string) string {
	p := norm(root)
	if len(dirs) > 0 {
		p += "/" + strings.Join(dirs, "/")
	}
	return p
}

// ReadAll reads fd to its end.
func ReadAll(fd int) ([]byte, error) {
	var raw []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return raw, nil
		}
		raw = append(raw, buf[:n]...)
	}
}

// ProbeRead reads <root>/<dirs>/<name> as Probe opens it. present is false
// when the file is missing or refused (refusal is "" for missing); err is a
// failed read of an open file.
func ProbeRead(root string, dirs []string, name string) (raw []byte, present bool, refusal string, err error) {
	fd, refusal := Probe(root, dirs, name, -1)
	if fd < 0 {
		return nil, false, refusal, nil
	}
	defer syscall.Close(fd)
	raw, err = ReadAll(fd)
	return raw, err == nil, "", err
}

// ReadFile is ProbeRead without the refusal.
func ReadFile(root string, dirs []string, name string) (raw []byte, ok bool, err error) {
	raw, ok, _, err = ProbeRead(root, dirs, name)
	return raw, ok, err
}

// norm is str(Path(root)): empty and "." parts dropped, ".." kept.
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
