package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/internal/fenceruns"
)

// The bound snapshot (FN-R-27, FN-R-34), called directly: checkpoints are
// off inside the fence (FN-R-36), so the resident server no longer takes
// one for a fenced session, and the snapshot stays for unfenced use.
// tests/test_gate_resident_checkpoint.py is the oracle's copy.

const cpSecret = "the-secret-the-fence-denies\n"

type boundEnv struct {
	t   *testing.T
	dir string
}

func newBoundEnv(t *testing.T) *boundEnv {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &boundEnv{t: t, dir: d}
}

func (c *boundEnv) runner() *runner {
	return &runner{root: filepath.Join(c.dir, "gate"), verifyTimeoutS: 5, t0: time.Now()}
}

// snap takes the bound snapshot of ws from cwd, and fails the test when
// it takes none.
func (c *boundEnv) snap(ws, cwd string) {
	c.t.Helper()
	if _, ok := c.runner().snapshot(cwd, "s1", "e1", ws); !ok {
		c.t.Fatalf("no snapshot of %s", ws)
	}
}

// refused says whether ws's git layout is refused and no snapshot is taken.
func (c *boundEnv) refused(ws string) bool {
	_, ok := c.runner().snapshot(ws, "s1", "e1", ws)
	return fenceruns.GitDirRefusalFor(ws) != "" && !ok
}

func cpGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func cpRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cpGit(t, dir, "init", "-q")
	cpGit(t, dir, "config", "user.email", "t@t")
	cpGit(t, dir, "config", "user.name", "t")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600)
	cpGit(t, dir, "add", "a.txt")
	cpGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func cpRefs(t *testing.T, repo string) []string {
	return strings.Fields(cpGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/daisugi"))
}

func cpHashed(t *testing.T, repo string) bool {
	out := cpGit(t, repo, "cat-file", "--batch-all-objects", "--batch")
	return strings.Contains(out, cpSecret)
}

func cpNames(t *testing.T, repo, ref string) []string {
	return strings.Fields(cpGit(t, repo, "ls-tree", "-r", "--name-only", ref))
}

func cpHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestBoundSnapshotCoreWorktreeCannotMoveTheTree(t *testing.T) {
	c := newBoundEnv(t)
	other := filepath.Join(c.dir, "other")
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(other, "id_key"), []byte(cpSecret), 0o600)
	ws := cpRepo(t, filepath.Join(c.dir, "ws"))
	cpGit(t, ws, "config", "core.worktree", other)
	c.snap(ws, ws)
	refs := cpRefs(t, ws)
	if len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	names := cpNames(t, ws, refs[0])
	if !cpHas(names, "a.txt") || cpHas(names, "id_key") {
		t.Errorf("names %v", names)
	}
	if cpHashed(t, ws) {
		t.Error("a file outside the workspace was hashed")
	}
}

func TestBoundSnapshotAGitDirThatLeadsOutDeniesTheCall(t *testing.T) {
	for _, shape := range []string{"gitfile", "link", "commondir", "objects-link", "fanout-link"} {
		t.Run(shape, func(t *testing.T) {
			c := newBoundEnv(t)
			other := cpRepo(t, filepath.Join(c.dir, "other"))
			ws := filepath.Join(c.dir, "ws")
			g := filepath.Join(ws, ".git")
			if shape == "gitfile" {
				os.MkdirAll(ws, 0o700)
				os.WriteFile(g, []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600)
			} else {
				cpRepo(t, ws)
			}
			switch shape {
			case "link":
				os.Rename(g, filepath.Join(c.dir, "moved.git"))
				os.Symlink(filepath.Join(c.dir, "moved.git"), g)
			case "commondir":
				os.WriteFile(filepath.Join(g, "commondir"), []byte(filepath.Join(other, ".git")+"\n"), 0o600)
			case "objects-link":
				os.Rename(filepath.Join(g, "objects"), filepath.Join(c.dir, "moved-objects"))
				os.Symlink(filepath.Join(c.dir, "moved-objects"), filepath.Join(g, "objects"))
			case "fanout-link":
				os.MkdirAll(filepath.Join(c.dir, "far"), 0o700)
				os.Symlink(filepath.Join(c.dir, "far"), filepath.Join(g, "objects", "ab"))
			}
			if !c.refused(ws) {
				t.Errorf("%s: not refused", shape)
			}
			if refs := cpRefs(t, other); len(refs) != 0 {
				t.Errorf("other repo got %v", refs)
			}
		})
	}
}

func TestBoundSnapshotNoWalkUpToAParentRepo(t *testing.T) {
	c := newBoundEnv(t)
	parent := cpRepo(t, filepath.Join(c.dir, "parent"))
	ws := filepath.Join(parent, "ws")
	os.MkdirAll(ws, 0o700)
	os.WriteFile(filepath.Join(ws, "b.txt"), []byte("two\n"), 0o600)
	if _, ok := c.runner().snapshot(ws, "s1", "e1", ws); ok {
		t.Error("a workspace with no .git got a snapshot")
	}
	if refs := cpRefs(t, parent); len(refs) != 0 {
		t.Errorf("the parent repo was checkpointed: %v", refs)
	}
}

func TestBoundSnapshotASubdirCwdSnapshotsTheBoundWorkspace(t *testing.T) {
	c := newBoundEnv(t)
	ws := cpRepo(t, filepath.Join(c.dir, "ws"))
	os.MkdirAll(filepath.Join(ws, "sub"), 0o700)
	os.WriteFile(filepath.Join(ws, "sub", "c.txt"), []byte("three\n"), 0o600)
	c.snap(ws, filepath.Join(ws, "sub"))
	refs := cpRefs(t, ws)
	if len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	names := cpNames(t, ws, refs[0])
	if !cpHas(names, "a.txt") || !cpHas(names, "sub/c.txt") {
		t.Errorf("names %v", names)
	}
}

func TestBoundSnapshotALinkThatLeavesTheWorkspaceIsNotCopied(t *testing.T) {
	c := newBoundEnv(t)
	other := filepath.Join(c.dir, "other")
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(other, "id_key"), []byte(cpSecret), 0o600)
	ws := cpRepo(t, filepath.Join(c.dir, "ws"))
	os.Symlink(filepath.Join(other, "id_key"), filepath.Join(ws, "key"))
	os.Symlink(other, filepath.Join(ws, "dir"))
	c.snap(ws, ws)
	refs := cpRefs(t, ws)
	if len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	names := cpNames(t, ws, refs[0])
	if cpHas(names, "key") || cpHas(names, "dir") || !cpHas(names, "a.txt") {
		t.Errorf("names %v", names)
	}
	if cpHashed(t, ws) {
		t.Error("a file outside the workspace was hashed")
	}
}

func cpWorktree(t *testing.T, dir string) (string, string) {
	main := cpRepo(t, filepath.Join(dir, "main"))
	wt := filepath.Join(dir, "wt")
	cpGit(t, main, "worktree", "add", "-q", "--detach", wt)
	return main, wt
}

func TestBoundSnapshotAGitWorktreeTakesACheckpointAndRestoresIt(t *testing.T) {
	c := newBoundEnv(t)
	main, wt := cpWorktree(t, c.dir)
	os.WriteFile(filepath.Join(wt, "w.txt"), []byte("in the worktree\n"), 0o600)
	c.snap(wt, wt)
	refs := cpRefs(t, wt)
	if len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	names := cpNames(t, wt, refs[0])
	if !cpHas(names, "a.txt") || !cpHas(names, "w.txt") {
		t.Fatalf("names %v", names)
	}
	// The Go port has no restore of its own; git restores the ref in the worktree.
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("changed\n"), 0o600)
	os.Remove(filepath.Join(wt, "w.txt"))
	cpGit(t, wt, "restore", "--source="+refs[0], "--worktree", "--", ".")
	if b, _ := os.ReadFile(filepath.Join(wt, "a.txt")); string(b) != "one\n" {
		t.Errorf("a.txt %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "w.txt")); string(b) != "in the worktree\n" {
		t.Errorf("w.txt %q", b)
	}
	if _, err := os.Stat(filepath.Join(main, "w.txt")); err == nil {
		t.Error("the main tree was written")
	}
}

func TestBoundSnapshotAForgedWorktreeGitfileDeniesTheCall(t *testing.T) {
	for _, shape := range []string{"other-worktree", "no-back-link", "back-link-elsewhere", "commondir-elsewhere", "not-under-worktrees"} {
		t.Run(shape, func(t *testing.T) {
			c := newBoundEnv(t)
			main, wt := cpWorktree(t, c.dir)
			other := cpRepo(t, filepath.Join(c.dir, "other"))
			ws := filepath.Join(c.dir, "ws")
			os.MkdirAll(ws, 0o700)
			head := cpGit(t, main, "rev-parse", "HEAD")
			admin := filepath.Join(main, ".git", "worktrees", "forged")
			var target string
			switch shape {
			case "other-worktree":
				target = filepath.Join(main, ".git", "worktrees", "wt")
			case "not-under-worktrees":
				target = filepath.Join(c.dir, "loose")
				os.MkdirAll(target, 0o700)
				os.WriteFile(filepath.Join(target, "gitdir"), []byte(filepath.Join(ws, ".git")+"\n"), 0o600)
				os.WriteFile(filepath.Join(target, "commondir"), []byte(filepath.Join(main, ".git")+"\n"), 0o600)
				os.WriteFile(filepath.Join(target, "HEAD"), []byte(head), 0o600)
			default:
				os.MkdirAll(admin, 0o700)
				os.WriteFile(filepath.Join(admin, "HEAD"), []byte(head), 0o600)
				if shape != "no-back-link" {
					back := filepath.Join(ws, ".git")
					if shape == "back-link-elsewhere" {
						back = filepath.Join(wt, ".git")
					}
					os.WriteFile(filepath.Join(admin, "gitdir"), []byte(back+"\n"), 0o600)
				}
				common := "../.."
				if shape == "commondir-elsewhere" {
					common = filepath.Join(other, ".git")
				}
				os.WriteFile(filepath.Join(admin, "commondir"), []byte(common+"\n"), 0o600)
				target = admin
			}
			os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: "+target+"\n"), 0o600)
			if !c.refused(ws) {
				t.Errorf("%s: not refused", shape)
			}
			if len(cpRefs(t, main)) != 0 || len(cpRefs(t, other)) != 0 {
				t.Error("a repo got a checkpoint")
			}
		})
	}
}

func TestBoundSnapshotAWorktreeRunsNoFilterOrFsmonitorFromTheCommonConfig(t *testing.T) {
	c := newBoundEnv(t)
	main, wt := cpWorktree(t, c.dir)
	mark := filepath.Join(c.dir, "marks")
	os.MkdirAll(mark, 0o700)
	mon := filepath.Join(c.dir, "fsmonitor.sh")
	os.WriteFile(mon, []byte("#!/bin/sh\ntouch "+mark+"/fsmonitor\n"), 0o755)
	cpGit(t, main, "config", "filter.x.clean", "sh -c 'touch "+mark+"/clean; cat'")
	cpGit(t, main, "config", "filter.x.required", "true")
	cpGit(t, main, "config", "core.fsmonitor", mon)
	os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("*.txt filter=x\n"), 0o600)
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("three\n"), 0o600)
	c.snap(wt, wt)
	if refs := cpRefs(t, wt); len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	if ents, _ := os.ReadDir(mark); len(ents) != 0 {
		t.Errorf("a filter or fsmonitor from the common config ran: %v", ents)
	}
}
