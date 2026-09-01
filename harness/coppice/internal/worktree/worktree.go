// Package worktree keeps one git worktree per task beside the repo, under
// <repo>-worktrees/<name>. All git runs through os/exec with git -C, so the
// package never changes the process directory.
package worktree

import (
	"errors"
	"fmt"
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

// git runs one git command in dir and returns trimmed stdout. On failure the
// error carries git's stderr.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
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
	_, err := git(repo, "worktree", "remove", Path(repo, name))
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
