package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// needGit skips the test when git is not on PATH. Every test here runs git.
func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo makes a repo on branch main with one commit, inside t.TempDir().
func initRepo(t *testing.T) string {
	t.Helper()
	needGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "init", "-q", "-b", "main")
	run(t, repo, "config", "user.email", "test@example.invalid")
	run(t, repo, "config", "user.name", "test")
	commitFile(t, repo, "README")
	return repo
}

// commitFile writes a file and commits it in dir.
func commitFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", name)
	run(t, dir, "commit", "-q", "-m", "add "+name)
}

func TestAddCreatesAWorktreeBesideTheRepo(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "gate-refactor")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != repo+"-worktrees" {
		t.Fatalf("not beside the repo: %s", p)
	}
	if filepath.Base(p) != "gate-refactor" {
		t.Fatalf("not named after the task: %s", p)
	}
	if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
		t.Fatal("no worktree")
	}
	if got := run(t, p, "rev-parse", "--abbrev-ref", "HEAD"); got != "gate-refactor" {
		t.Fatalf("worktree branch = %q, want gate-refactor", got)
	}
	if got := run(t, p, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "main" {
		t.Fatalf("upstream = %q, want main", got)
	}
}

func TestBadNamesAreRefusedWithATeachingError(t *testing.T) {
	repo := initRepo(t)
	for _, name := range []string{"Gate Refactor", "gate_refactor", "", "a/b", "..", "gate.refactor"} {
		_, err := Add(repo, name)
		if err == nil || !strings.Contains(err.Error(), "lowercase") {
			t.Fatalf("Add(%q) = %v, want the teaching error", name, err)
		}
	}
	if err := ValidName("ok-name-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repo + "-worktrees"); !os.IsNotExist(err) {
		t.Fatal("a refused name still made the worktrees directory")
	}
}

func TestStatusCountsAhead(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "x")
	if err != nil {
		t.Fatal(err)
	}
	ahead, behind, branch, err := Status(p)
	if err != nil || ahead != 0 || behind != 0 || branch != "x" {
		t.Fatalf("fresh: %d %d %s %v", ahead, behind, branch, err)
	}
	commitFile(t, p, "a.txt")
	ahead, behind, branch, err = Status(p)
	if err != nil || ahead != 1 || behind != 0 || branch != "x" {
		t.Fatalf("after one commit: %d %d %s %v", ahead, behind, branch, err)
	}
	commitFile(t, repo, "b.txt")
	ahead, behind, _, err = Status(p)
	if err != nil || ahead != 1 || behind != 1 {
		t.Fatalf("after main moved: %d %d %v", ahead, behind, err)
	}
}

func TestStatusOnAPlainDirectoryErrors(t *testing.T) {
	needGit(t)
	if _, _, _, err := Status(t.TempDir()); err == nil {
		t.Fatal("Status on a plain directory returned no error")
	}
}

func TestStatusWithoutAnUpstreamErrors(t *testing.T) {
	repo := initRepo(t)
	if _, _, _, err := Status(repo); err == nil {
		t.Fatal("Status on a branch with no upstream returned no error")
	}
}

func TestAddOnADetachedRepoIsRefused(t *testing.T) {
	repo := initRepo(t)
	run(t, repo, "checkout", "-q", "--detach")
	_, err := Add(repo, "x")
	if err == nil || err.Error() != "the repo is not on a branch. Check out one first." {
		t.Fatalf("Add on a detached repo = %v", err)
	}
}

func TestAddOutsideARepoErrors(t *testing.T) {
	needGit(t)
	if _, err := Add(t.TempDir(), "x"); err == nil {
		t.Fatal("Add outside a repo returned no error")
	}
}

func TestRemoveWithoutKeepDeletesACleanWorktree(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, "x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
	// The branch stays. Only the checkout goes.
	run(t, repo, "rev-parse", "--verify", "refs/heads/x")
}

func TestRemoveWithKeepLeavesTheWorktree(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, "x", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
		t.Fatal("keep removed the worktree")
	}
}

func TestRemoveRefusesADirtyWorktree(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "dirty.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, "x", false); err == nil {
		t.Fatal("Remove deleted a dirty worktree")
	}
	if _, err := os.Stat(filepath.Join(p, "dirty.txt")); err != nil {
		t.Fatal("the dirty file is gone")
	}
}

func TestDirtyReportsUncommittedChanges(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "x")
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := Dirty(p)
	if err != nil || dirty {
		t.Fatalf("fresh worktree: dirty=%v err=%v", dirty, err)
	}
	if err := os.WriteFile(filepath.Join(p, "new.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err = Dirty(p)
	if err != nil || !dirty {
		t.Fatalf("with an untracked file: dirty=%v err=%v", dirty, err)
	}
	if _, err := Dirty(t.TempDir()); err == nil {
		t.Fatal("Dirty on a plain directory returned no error")
	}
}

func TestToplevelFindsTheRepoFromASubdirectory(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Toplevel(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != repo {
		t.Fatalf("Toplevel = %q, want %q", got, repo)
	}
	if _, err := Toplevel(t.TempDir()); err == nil {
		t.Fatal("Toplevel outside a repo returned no error")
	}
}

func TestRepoFindsTheMainRepoFromAWorktree(t *testing.T) {
	repo := initRepo(t)
	p, err := Add(repo, "x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Repo(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != repo {
		t.Fatalf("Repo = %q, want %q", got, repo)
	}
}

// When the upstream cannot be set, the worktree and its branch go, so the
// next Add with the same name starts clean.
func TestAFailedUpstreamRemovesTheWorktreeAndTheBranch(t *testing.T) {
	repo := initRepo(t)
	saved := setUpstream
	setUpstream = func(path, base string) error { return errors.New("no upstream today") }
	_, err := Add(repo, "x")
	setUpstream = saved
	if err == nil {
		t.Fatal("Add returned no error when the upstream failed")
	}
	if _, err := os.Stat(Path(repo, "x")); !os.IsNotExist(err) {
		t.Fatalf("the worktree stayed on disk: %v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", "refs/heads/x").Output(); err == nil {
		t.Fatalf("the branch stayed: %s", out)
	}
	if _, err := Add(repo, "x"); err != nil {
		t.Fatalf("a second Add after the cleanup failed: %v", err)
	}
}
