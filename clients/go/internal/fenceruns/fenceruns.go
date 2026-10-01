// Package fenceruns is fence_runs.py: what the resident gate trusts about a
// caller, and what it never takes from a request (FN-R-21).
//
// The resident gate runs outside the fence and makes every write. So it
// takes no path from a request: its root and captures root are fixed when
// it starts. A resident call's session id must be plain. A checkpoint is
// taken only inside the workspace of the fence run above the caller:
// `daisugi fence`, still outside the fence, writes
// <root>/fence-runs/<pid>.json, which no fenced process can write.
//
// A checkpoint never lets git choose the tree (FN-R-27): the tree is the
// bound workspace, git is told GIT_DIR, GIT_COMMON_DIR and GIT_WORK_TREE,
// and a .git that leads out of the workspace denies the call. A resident
// transcript is read only when it is a regular file under
// ~/.claude/projects of the password entry's home (FN-R-29).
package fenceruns

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"daisugi-verify/internal/statefile"
)

// RunsDir is the directory under the gate root that holds the records.
const RunsDir = "fence-runs"

const maxDepth = 64

// The refusals, word for word as fence_runs.py has them.
const (
	SessionRefusal = "the resident gate takes a session id only when it matches [A-Za-z0-9._-]{1,128}, " +
		"so it denies this call"
	PathRefusal = "the resident gate takes no path from a request: its root and captures root are " +
		"fixed when it starts. Drop --root and --captures-root from the request"
	NoWorkspaceRefusal = "the resident gate takes a checkpoint only inside the workspace a fence run bound, " +
		"and this caller runs under no fence run, so it denies this call"
	OutsideRefusal = "the checkpoint cwd is not inside the workspace this caller's fence run bound, " +
		"so the resident gate denies this call"
	// FencedCheckpointRefusal is FENCED_CHECKPOINT_REFUSAL (FN-R-36): the
	// unfenced server never runs git for a fenced session.
	FencedCheckpointRefusal = "checkpoints are off inside the fence"
	GitDirRefusal           = "the workspace's git dir is not a plain directory inside the workspace this caller's " +
		"fence run bound (a gitfile, a link, a common dir, or objects or refs that lead out), " +
		"so the resident gate denies this call"
)

// TranscriptCap is the largest transcript a resident checkpoint reads.
const TranscriptCap = 128 * 1024 * 1024

var pathFlags = []string{"--root", "--captures-root"}

// SessionOK is session_ok: [A-Za-z0-9._-]{1,128}, not . or ..
func SessionOK(sid string) bool {
	if sid == "" || len(sid) > 128 || sid == "." || sid == ".." {
		return false
	}
	for i := 0; i < len(sid); i++ {
		c := sid[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// NamesAPath is names_a_path: a word whose name part is --root or
// --captures-root, or a prefix argparse would read as one.
func NamesAPath(argv []string) bool {
	for _, w := range argv {
		name, _, _ := strings.Cut(w, "=")
		if len(name) < 3 || !strings.HasPrefix(name, "--") {
			continue
		}
		for _, f := range pathFlags {
			if strings.HasPrefix(f, name) {
				return true
			}
		}
	}
	return false
}

// StripPathFlags is strip_path_flags: argv without --root and
// --captures-root and their values.
func StripPathFlags(argv []string) []string {
	out := make([]string, 0, len(argv))
	skip := false
	for _, w := range argv {
		if skip {
			skip = false
			continue
		}
		if w == "--root" || w == "--captures-root" {
			skip = true
			continue
		}
		if strings.HasPrefix(w, "--root=") || strings.HasPrefix(w, "--captures-root=") {
			continue
		}
		out = append(out, w)
	}
	return out
}

func statFields(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return nil
	}
	return strings.Fields(s[i+2:])
}

// ProcStart is the start time /proc gives pid (field 22), or "".
func ProcStart(pid int) string {
	if f := statFields(pid); len(f) > 19 {
		return f[19]
	}
	return ""
}

func parent(pid int) int {
	if f := statFields(pid); len(f) > 1 {
		if n, err := strconv.Atoi(f[1]); err == nil {
			return n
		}
	}
	return 0
}

type record struct {
	Pid       *int    `json:"pid"`
	Start     *string `json:"start"`
	Workspace *string `json:"workspace"`
}

// BoundWorkspace is bound_workspace: the workspace of the fence run the
// caller pid runs under, or "".
func BoundWorkspace(root string, peer *int) string {
	if peer == nil {
		return ""
	}
	pid := *peer
	for i := 0; i < maxDepth && pid > 1; i++ {
		raw, ok, err := statefile.ReadFile(root, []string{RunsDir}, strconv.Itoa(pid)+".json")
		if ok && err == nil {
			var rec record
			if json.Unmarshal(raw, &rec) == nil && rec.Pid != nil && *rec.Pid == pid &&
				rec.Start != nil && *rec.Start == ProcStart(pid) &&
				rec.Workspace != nil && filepath.IsAbs(*rec.Workspace) {
				return *rec.Workspace
			}
		}
		pid = parent(pid)
	}
	return ""
}

// Register writes the fence run record for pid: the fence calls it outside
// the fence, before the ruleset. Records of pids that are gone are swept.
func Register(root string, pid int, workspace string) error {
	ws, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		ws = filepath.Clean(workspace)
	}
	sweep(root)
	b, _ := json.Marshal(map[string]any{"pid": pid, "start": ProcStart(pid), "workspace": ws})
	_, err = statefile.WriteFile(root, []string{RunsDir}, strconv.Itoa(pid)+".json", string(b), true)
	return err
}

func sweep(root string) {
	ents, err := os.ReadDir(filepath.Join(root, RunsDir))
	if err != nil {
		return
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(name); err == nil && ProcStart(pid) == "" {
			_, _ = statefile.UnlinkFile(root, []string{RunsDir}, e.Name())
		}
	}
}

// Inside is inside: cwd, every link followed, is ws or below it.
func Inside(cwd, ws string) bool {
	real := Realpath(cwd)
	w := strings.TrimRight(ws, "/")
	if w == "" {
		return true
	}
	return real == w || strings.HasPrefix(real, w+"/")
}

// Realpath is os.path.realpath for an absolute path: every link followed,
// and a part that does not exist kept as it is written.
func Realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	dir = strings.TrimRight(dir, "/")
	if dir == "" || dir == p {
		return filepath.Clean(p)
	}
	rd := Realpath(dir)
	switch base {
	case "", ".":
		return rd
	case "..":
		return filepath.Dir(rd)
	}
	return filepath.Join(rd, base)
}

// readLinkFile is _read_link_file: the first line of a small git link file
// (a gitfile, gitdir, commondir), read with O_NOFOLLOW, or false.
func readLinkFile(path string) (string, bool) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", false
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return "", false
	}
	buf := make([]byte, 4097)
	n, err := syscall.Read(fd, buf)
	if err != nil || n < 0 || n > 4096 {
		return "", false
	}
	data := buf[:n]
	if strings.IndexByte(string(data), 0) >= 0 || !utf8.Valid(data) {
		return "", false
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t\n\r\v\f")
	return line, line != ""
}

// joinFrom is os.path.join(dir, p): p itself when it is absolute.
func joinFrom(dir, p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return strings.TrimRight(dir, "/") + "/" + p
}

// worktreeDirs is _worktree_dirs: the git dir and common dir of a git
// worktree whose link checks out by realpath (W-1), or false.
func worktreeDirs(ws, gitfile string) (string, string, bool) {
	line, ok := readLinkFile(gitfile)
	if !ok || !strings.HasPrefix(line, "gitdir: ") || len(line) == len("gitdir: ") {
		return "", "", false
	}
	admin := Realpath(joinFrom(ws, line[len("gitdir: "):]))
	common := filepath.Dir(filepath.Dir(admin))
	if filepath.Base(filepath.Dir(admin)) != "worktrees" || !isDir(admin) {
		return "", "", false
	}
	back, ok := readLinkFile(admin + "/gitdir")
	if !ok || Realpath(joinFrom(admin, back)) != Realpath(gitfile) {
		return "", "", false
	}
	cd, ok := readLinkFile(admin + "/commondir")
	if !ok || Realpath(joinFrom(admin, cd)) != common {
		return "", "", false
	}
	return admin, common, true
}

func isDir(p string) bool {
	var st syscall.Stat_t
	return syscall.Stat(p, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR
}

// GitLayout is git_layout: the refusal, or the git dir and common dir of a
// checkpoint of ws. All "" when ws has no .git: it gets no checkpoint. A
// plain .git dir is both dirs; a gitfile is taken only for a git worktree
// whose link checks out (W-1).
func GitLayout(ws string) (why, gitDir, commonDir string) {
	if Realpath(ws) != ws {
		return GitDirRefusal, "", ""
	}
	g := strings.TrimRight(ws, "/") + "/.git"
	var st syscall.Stat_t
	if err := syscall.Lstat(g, &st); err != nil {
		if err == syscall.ENOENT {
			return "", "", ""
		}
		return GitDirRefusal, "", ""
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		gitDir, commonDir = g, g
	case syscall.S_IFREG:
		a, c, ok := worktreeDirs(ws, g)
		if !ok {
			return GitDirRefusal, "", ""
		}
		gitDir, commonDir = a, c
	default:
		return GitDirRefusal, "", ""
	}
	c := commonDir
	if err := syscall.Lstat(c, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || lexists(c+"/commondir") {
		return GitDirRefusal, "", ""
	}
	for _, sub := range []string{"objects", "refs", "refs/daisugi"} {
		if err := syscall.Lstat(c+"/"+sub, &st); err != nil {
			if err == syscall.ENOENT {
				continue
			}
			return GitDirRefusal, "", ""
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			return GitDirRefusal, "", ""
		}
	}
	ents, err := os.ReadDir(c + "/objects")
	if err != nil && !os.IsNotExist(err) {
		return GitDirRefusal, "", ""
	}
	for _, e := range ents {
		if e.Type()&os.ModeSymlink != 0 {
			return GitDirRefusal, "", ""
		}
	}
	bad := false
	_ = filepath.WalkDir(c+"/refs/daisugi", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			bad = true
			return filepath.SkipAll
		}
		return nil
	})
	if bad {
		return GitDirRefusal, "", ""
	}
	return "", gitDir, commonDir
}

// GitDirRefusalFor is git_dir_refusal: why a checkpoint of ws would write
// where git's own layout does not lead, or "". "" also when ws has no .git.
func GitDirRefusalFor(ws string) string {
	why, _, _ := GitLayout(ws)
	return why
}

func lexists(p string) bool {
	var st syscall.Stat_t
	return syscall.Lstat(p, &st) == nil
}

// TranscriptRoot is transcript_root: ~/.claude/projects of the password
// entry's home, never $HOME. A var, so a test can point it at a fake home.
var TranscriptRoot = func() string {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || u.HomeDir == "" {
		return ""
	}
	return strings.TrimRight(u.HomeDir, "/") + "/.claude/projects"
}

// ResidentTranscript is resident_transcript: the bytes of the transcript a
// resident payload names, or false. Only a regular file under
// TranscriptRoot (realpath) is read, opened O_NOFOLLOW|O_NONBLOCK.
func ResidentTranscript(path string) ([]byte, bool) {
	if path == "" || strings.ContainsRune(path, 0) || !strings.HasPrefix(path, "/") {
		return nil, false
	}
	root := TranscriptRoot()
	if root == "" {
		return nil, false
	}
	base := Realpath(root)
	real := Realpath(path)
	if real == base || !Inside(real, base) {
		return nil, false
	}
	fd, err := syscall.Open(real, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), real)
	defer f.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size > TranscriptCap {
		return nil, false
	}
	buf := make([]byte, 0, st.Size)
	chunk := make([]byte, 1<<20)
	for len(buf) <= TranscriptCap {
		n, err := syscall.Read(fd, chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, false
		}
		if n == 0 {
			break
		}
	}
	if len(buf) > TranscriptCap {
		return nil, false
	}
	return buf, true
}
