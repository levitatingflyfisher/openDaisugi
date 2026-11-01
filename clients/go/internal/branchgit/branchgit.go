// Package branchgit reads a branch worktree without trusting what the
// child wrote in it (branch_git.py). The child can rewrite the worktree's
// .git pointer, so the parent takes the repository and the base commit from
// what it recorded itself and checks, before each git call, that the
// worktree still belongs to that repository.
package branchgit

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/pystr"
)

// IsObjectID is tree.is_object_id: 40 or 64 lower-case hex digits.
func IsObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Guard is what every git call the parent makes in a branch worktree, or in
// the tree an adopt writes, passes first (branch_git.GUARD). The shared
// config and the hooks of a repository are a place a branch child may
// reach, so a file system monitor and a hooks directory the config names
// are switched off. /dev/null holds no hook, so no hook is found.
//
// It also ignores submodules: a nested repository the child made would be
// asked for its status, and that status reads the nested config and runs the
// filters the child wrote there.
var Guard = []string{
	"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
	"-c", "diff.ignoreSubmodules=all", "-c", "submodule.recurse=false",
}

// Store is the object store a fenced branch child writes: inside its own
// admin directory. The parent never reads it and never writes it.
func Store(repo, worktree string) string {
	return filepath.Join(repo, "worktrees", filepath.Base(worktree), "objects")
}

// drop removes path without following a symbolic link at any level.
func drop(path string) { _ = os.RemoveAll(path) }

// Drop is drop, for the parent's own clean up of a branch admin directory.
func Drop(path string) { drop(path) }

// Reclaim gives the parent back its rights on a branch admin directory and
// all under it (branch_git.reclaim): owner rwx on each directory, owner rw on
// each regular file. The child has the same uid and may have taken them
// away. Each entry is read with lstat; a link is never followed and nothing
// outside admin is changed. Call it only once the child and all its
// processes are gone.
func Reclaim(admin string) {
	todo := []string{admin}
	for len(todo) > 0 {
		path := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		mode := fi.Mode().Perm()
		var want os.FileMode
		switch {
		case fi.IsDir():
			want = mode | 0o700
		case fi.Mode().IsRegular():
			want = mode | 0o600
		default:
			continue
		}
		if want != mode {
			_ = os.Chmod(path, want|(fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)))
		}
		if fi.IsDir() {
			if es, err := os.ReadDir(path); err == nil {
				for _, e := range es {
					todo = append(todo, filepath.Join(path, e.Name()))
				}
			}
		}
	}
}

// Fold folds a branch child's own object store away, once the child and all
// its processes are gone and before the parent reads its worktree
// (branch_git.fold). The worktree's HEAD goes back to base, its index to
// base's tree, and its reflogs, its own refs and its object store are
// deleted. The child's commits then survive only as the files in the
// worktree, and the diff against base reads those files. Without this, the
// operator's repository holds a HEAD, an index and reflogs that name objects
// only the child's store holds, so gc, fsck and log --all fail, and a parent
// write skips an object the child store already has.
//
// git runs on the recorded git directory, never through the worktree's .git
// pointer, which the child can rewrite. A link the child made in its admin
// directory is removed, never followed.
//
// It first gives the parent back its rights on the admin directory, so a
// child that took them cannot keep its logs, refs or store. True only when
// the logs, the refs, any reftable directory and the store are gone, HEAD
// names base and the index holds base's tree.
func Fold(repo, worktree, base string) bool {
	if !IsObjectID(base) || repo == "" {
		return false
	}
	name := filepath.Base(worktree)
	admin := filepath.Join(repo, "worktrees", name)
	if fi, err := os.Lstat(admin); err != nil || !fi.IsDir() {
		return false
	}
	Reclaim(admin)
	// A fenced run refuses a reftable repository before any worktree is
	// made, so a reftable directory here is one the child planted. A lock the
	// child put on its worktree would keep prune from removing it.
	for _, part := range []string{"logs", "refs", "reftable", "locked", "HEAD.lock", "index.lock"} {
		drop(filepath.Join(admin, part))
	}
	for _, part := range []string{"HEAD", "index"} {
		p := filepath.Join(admin, part)
		if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
			drop(p)
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_OBJECT_DIRECTORY=") || strings.HasPrefix(kv, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") ||
			strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_INDEX_FILE="+filepath.Join(admin, "index"))
	at := []string{"-c", "core.logAllRefUpdates=false", "--git-dir=" + repo}
	if _, _, ok := GitEnv(0, env, nil, append(at, "update-ref", "--no-deref", "worktrees/"+name+"/HEAD", base)...); !ok {
		return false
	}
	if _, _, ok := GitEnv(0, env, nil, append(at, "read-tree", base)...); !ok {
		return false
	}
	drop(filepath.Join(admin, "logs"))
	drop(Store(repo, worktree))
	for _, part := range []string{"logs", "refs", "reftable", "objects"} {
		if _, err := os.Lstat(filepath.Join(admin, part)); err == nil || !os.IsNotExist(err) {
			return false
		}
	}
	head := filepath.Join(admin, "HEAD")
	if fi, err := os.Lstat(head); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(head)
	if err != nil || strings.TrimSpace(pystr.DecodeReplace(b)) != base {
		return false
	}
	if fi, err := os.Lstat(filepath.Join(admin, "index")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	_, _, ok := GitEnv(0, env, nil, append(at, "diff-index", "--cached", "--quiet", base, "--")...)
	return ok
}

// RefFormat is branch_git.ref_format: the ref format of the repository whose
// git directory is repo, from its own config (extensions.refStorage; none
// is files). Read before any child runs, from the operator's config, never
// from a path a child can write.
func RefFormat(repo string) string {
	cmd := exec.Command("git", Guarded("config", "--file", filepath.Join(repo, "config"), "--get", "extensions.refStorage")...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return "files"
		}
		return "unknown"
	}
	s := strings.ToLower(strings.TrimSpace(pystr.DecodeReplace(out.Bytes())))
	if s == "" {
		return "files"
	}
	return s
}

// Guarded is argv with the guard in front.
func Guarded(argv ...string) []string {
	return append(append([]string{}, Guard...), argv...)
}

// Git runs git and returns stdout, stderr and whether it exited 0.
func Git(stdin []byte, argv ...string) ([]byte, []byte, bool) {
	return GitTimeout(0, stdin, argv...)
}

// GitTimeout is Git with a time limit; 0 is none.
func GitTimeout(limit time.Duration, stdin []byte, argv ...string) ([]byte, []byte, bool) {
	return GitEnv(limit, nil, stdin, argv...)
}

// GitEnv is GitTimeout with an environment (nil is the inherited one).
func GitEnv(limit time.Duration, env []string, stdin []byte, argv ...string) ([]byte, []byte, bool) {
	ctx := context.Background()
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", Guarded(argv...)...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err == nil
}

// CommonDir is the absolute common git directory path resolves to now.
func CommonDir(path string) (string, bool) {
	out, _, ok := Git(nil, "-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if !ok {
		return "", false
	}
	s := strings.TrimSpace(pystr.DecodeReplace(out))
	return s, s != ""
}

func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Owns is true when path is the linked worktree git made for it in repo: its
// .git is a regular file and its git directory is exactly
// <repo>/worktrees/<name>. The common directory alone also admits the
// repository's main git directory and a sibling's.
func Owns(path, repo string) bool {
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if fi, err := os.Lstat(filepath.Join(path, ".git")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	// With extensions.worktreeConfig on, git reads this file, which the child
	// can write, and the -c guard cannot switch that off.
	if _, err := os.Lstat(filepath.Join(repo, "worktrees", filepath.Base(path), "config.worktree")); err == nil {
		return false
	}
	// A store of the child's own that was not folded away: the worktree's
	// HEAD and index may name objects only that store holds.
	if _, err := os.Lstat(Store(repo, path)); err == nil {
		return false
	}
	out, _, ok := Git(nil, "-C", path, "rev-parse", "--path-format=absolute", "--git-dir")
	if !ok {
		return false
	}
	found := strings.TrimSpace(pystr.DecodeReplace(out))
	return found != "" && real(found) == real(filepath.Join(repo, "worktrees", filepath.Base(path))) && Belongs(path, repo)
}

// Belongs is true when the work tree at path resolves to repo. It is for a
// tree the owner named (an adopt target); a branch worktree needs Owns.
func Belongs(path, repo string) bool {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	found, ok := CommonDir(path)
	return ok && real(found) == real(repo)
}

// Lost is the refusal text for a worktree that left its repository.
func Lost(session, repo string) string {
	return "the worktree of " + session + " no longer belongs to " + repo
}

// Usable is true for a base that is an object id and a non-empty repo.
func Usable(base, repo string) bool { return IsObjectID(base) && repo != "" }

// Stage is git add -A (--intent-to-add when intent) of a branch worktree,
// leaving out the nested repositories the index already holds as gitlinks. An
// add refreshes each index entry, and refreshing a gitlink asks the nested
// repository for its status, which reads its config and runs the filters the
// child wrote there.
func Stage(worktree string, intent bool) {
	listed, _, ok := Git(nil, "-C", worktree, "ls-files", "-s", "-z")
	var links []string
	if ok {
		for _, rec := range strings.Split(pystr.DecodeReplace(listed), "\x00") {
			if strings.HasPrefix(rec, "160000 ") {
				if _, name, found := strings.Cut(rec, "\t"); found {
					links = append(links, ":(exclude,literal)"+name)
				}
			}
		}
	}
	argv := []string{"-C", worktree, "add", "-A"}
	if intent {
		argv = append(argv, "--intent-to-add")
	}
	argv = append(append(argv, "--", "."), links...)
	Git(nil, argv...)
}

// Intent marks new files so a diff shows them. Only after Owns.
func Intent(worktree string) { Stage(worktree, true) }

// Diff is the worktree's changes against base. It returns ok false when the
// worktree does not belong to repo, the base is not an object id, or git
// fails.
func Diff(worktree, base, repo string, extra []string, markNew bool, limit time.Duration) ([]byte, bool) {
	if !IsObjectID(base) || !Owns(worktree, repo) {
		return nil, false
	}
	if markNew {
		Intent(worktree)
	}
	argv := []string{"-C", worktree, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all"}
	argv = append(argv, extra...)
	argv = append(argv, base, "--")
	out, _, ok := GitTimeout(limit, nil, argv...)
	if !ok {
		return nil, false
	}
	return out, true
}
