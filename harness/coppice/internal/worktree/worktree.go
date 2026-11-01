// Package worktree keeps one git worktree per task beside the repo, under
// <repo>-worktrees/<name>. All git runs through os/exec with git -C, so the
// package never changes the process directory.
package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrBadName is returned for a task name outside [a-z0-9-].
var ErrBadName = errors.New("task names use lowercase letters, digits, and dashes")

// ErrDetached is returned when the repo has no current branch.
var ErrDetached = errors.New("the repo is not on a branch. Check out one first.")

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// ValidName reports whether name can be a worktree and branch name.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrBadName
	}
	return nil
}

// Path is where the worktree for name lives, beside the repo. The repo path
// is cleaned, never resolved through symlinks, so the result sits next to
// the path the caller gave.
func Path(repo, name string) string {
	return filepath.Join(filepath.Clean(repo)+"-worktrees", name)
}

// safeGit are the flags every git call of coppice's starts with.
var safeGit = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// git runs one git command in dir and returns trimmed stdout. On failure the
// error carries git's stderr.
func git(dir string, args ...string) (string, error) {
	// coppice runs git as the owner, outside any fence, in a dir an agent
	// may have written: no hook and no fsmonitor command of that repo runs
	// (review fix 3, H3).
	cmd := exec.Command("git", append(append(safeGit[:len(safeGit):len(safeGit)], "-C", dir), args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// currentBranch returns the branch dir is on. A detached HEAD is ErrDetached.
func currentBranch(dir string) (string, error) {
	branch, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if branch == "HEAD" {
		return "", ErrDetached
	}
	return branch, nil
}

// setUpstream points the branch checked out at path at base. It is a
// variable so a test can make it fail.
var setUpstream = func(path, base string) error {
	_, err := git(path, "branch", "--set-upstream-to="+base)
	return err
}

// Add creates a worktree on a new branch called name, checked out at
// Path(repo, name), with its upstream set to the branch the repo is on.
// When the upstream cannot be set, the worktree and the branch are removed
// again, so the next Add with that name starts clean.
func Add(repo, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	base, err := currentBranch(repo)
	if err != nil {
		return "", err
	}
	path := Path(repo, name)
	if _, err := git(repo, "worktree", "add", "-b", name, path); err != nil {
		return "", err
	}
	if err := setUpstream(path, base); err != nil {
		_, _ = git(repo, "worktree", "remove", "--force", path)
		_, _ = git(repo, "branch", "-D", name)
		return "", err
	}
	return path, nil
}

// Remove deletes the worktree for name. With keep true it does nothing. Git
// refuses a worktree with uncommitted changes, and that refusal is returned
// as is. The branch is never deleted.
func Remove(repo, name string, keep bool) error {
	if keep {
		return nil
	}
	return RemoveAt(repo, Path(repo, name))
}

// RemoveAt deletes the worktree checked out at path, one of repo's
// worktrees. A caller that recorded the path Add returned passes it here:
// a worktree added from inside another worktree sits beside that worktree,
// not beside the main repo, so Path of the main repo does not name it.
// Git's refusal of a dirty worktree is returned as is. The branch stays.
func RemoveAt(repo, path string) error {
	_, err := git(repo, "worktree", "remove", path)
	return err
}

// Status reports how far the worktree's branch is ahead of and behind its
// upstream, and the branch name. A worktree with no upstream is an error.
func Status(path string) (ahead, behind int, branch string, err error) {
	branch, err = currentBranch(path)
	if err != nil {
		return 0, 0, "", err
	}
	out, err := git(path, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if err != nil {
		return 0, 0, "", err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, "", fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, "", fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	return ahead, behind, branch, nil
}

// Dirty reports whether the worktree has uncommitted or untracked changes.
func Dirty(path string) (bool, error) {
	out, err := git(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Toplevel returns the root of the repo that holds cwd. A cwd outside any
// repo is an error.
func Toplevel(cwd string) (string, error) {
	return git(cwd, "rev-parse", "--show-toplevel")
}

// Repo returns the main repo of the worktree at path: the directory that
// holds the shared .git.
func Repo(path string) (string, error) {
	common, err := git(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Dir(common), nil
}

// registered reports whether path is a worktree git knows for repo.
func registered(repo, path string) bool {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	want := filepath.Clean(path)
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && filepath.Clean(p) == want {
			return true
		}
	}
	return false
}

// branchExists reports whether repo has a local branch called name.
func branchExists(repo, name string) bool {
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// EnsureClone is a lead's own clone of repo at Path(repo, name), on a
// branch called name (review fix 3, H1: a clone, never a linked worktree,
// so the lead's git never writes the owner's repo). It is a `git clone
// --shared`: objects come from repo's object store, which a fenced lead
// reads only. A clone that is there is used again; a dir at the path that
// is not a clone of repo is an error, never taken over. The owner brings
// the work over with `git fetch <clone> name:name`.
func EnsureClone(repo, name string) (string, error) {
	return EnsureCloneFrom(repo, name, "")
}

// EnsureCloneFrom is EnsureClone for a lead's nested lead (review fix 5,
// K1, and the L1 ruling): the clone is always of the owner's repo, and
// nothing of the parent's clone is ever read, fetched or granted. A new
// clone starts from origin/<fromBranch> (the parent's branch) when the
// owner has fetched it into the repo already, else from the repo's branch
// as EnsureClone does.
func EnsureCloneFrom(repo, name, fromBranch string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	path := Path(repo, name)
	if _, err := os.Lstat(path); err == nil {
		origin, oerr := git(path, "config", "--get", "remote.origin.url")
		if oerr != nil || filepath.Clean(origin) != filepath.Clean(repo) {
			return "", fmt.Errorf("%s exists and is not a clone of %s; move it aside", path, repo)
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := git(filepath.Dir(path), "clone", "--quiet", "--shared", "--no-checkout", repo, path); err != nil {
		return "", err
	}
	base, err := currentBranch(repo)
	if err != nil {
		_ = os.RemoveAll(path)
		return "", err
	}
	// The lead's branch starts at the repo's current branch, or at its own
	// branch when the repo has one left from an earlier lead.
	start := "origin/" + base
	if branchExists(repo, name) {
		start = "origin/" + name
	} else if fromBranch != "" && ValidName(fromBranch) == nil && branchExists(repo, fromBranch) {
		start = "origin/" + fromBranch
	}
	if _, err := git(path, "checkout", "--quiet", "-b", name, start); err != nil {
		_ = os.RemoveAll(path)
		return "", err
	}
	return path, nil
}

// ObjectsDir is the object store of the repo at dir, as git itself names
// it (a linked worktree resolves to its common dir), absolute. It is asked
// on the owner's side; nothing a lead writes is read (review fix 4, J1).
func ObjectsDir(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		return "", fmt.Errorf("git named %q as the objects dir of %s", out, dir)
	}
	// Defence in depth (K1): the store must be <common dir>/objects, both
	// real dirs with no link on the way. git follows a link at objects and
	// answers its target; that answer is refused, never granted.
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	want := filepath.Join(filepath.Clean(common), "objects")
	if filepath.Clean(out) != want {
		return "", fmt.Errorf("git named %s as the objects dir of %s, not %s; it is not granted", out, dir, want)
	}
	if r, err := filepath.EvalSymlinks(out); err != nil || r != want {
		return "", fmt.Errorf("the objects dir %s of %s goes through a symbolic link; it is not granted", out, dir)
	}
	if st, err := os.Stat(out); err != nil || !st.IsDir() {
		return "", fmt.Errorf("the objects dir %s of %s is not there", out, dir)
	}
	return filepath.Clean(out), nil
}
