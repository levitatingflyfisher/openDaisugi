package rank

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/internal/branchgit"
	"daisugi-verify/internal/pyjson"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// twoWorktrees: a repo with a.txt, and two detached worktrees at HEAD.
func twoWorktrees(t *testing.T) (repo, wa, wb, base, common string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "a.txt"), "one\n")
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-qm", "init")
	wa, wb = filepath.Join(root, "wa"), filepath.Join(root, "wb")
	gitT(t, repo, "worktree", "add", "-q", "--detach", wa, "HEAD")
	gitT(t, repo, "worktree", "add", "-q", "--detach", wb, "HEAD")
	base = strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	return repo, wa, wb, base, filepath.Join(repo, ".git")
}

func patchArg(from, onto, fb, ob, repo string) *pyjson.Object {
	return pyjson.NewObject().Set("kind", "worktree_patch").Set("from", from).Set("from_base", fb).
		Set("from_repo", repo).Set("onto", onto).Set("onto_base", ob).Set("onto_repo", repo)
}

func TestAConfigAndAHookTheChildWroteDoNotRunUnderAPivot(t *testing.T) {
	repo, wa, wb, base, common := twoWorktrees(t)
	// A base that is not the other's makes the alternative's patch reach the
	// three-way merge and fail there, so the restore path runs checkout. The
	// commit comes before the config is armed.
	write(t, filepath.Join(repo, "a.txt"), "two\n")
	gitT(t, repo, "commit", "-qam", "second")
	base2 := strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	marker := filepath.Join(filepath.Dir(repo), "ran")
	script := "#!/bin/sh\necho x >> " + marker + "\n"
	hook := filepath.Join(filepath.Dir(repo), "hook.sh")
	write(t, hook, script)
	_ = os.Chmod(hook, 0o755)
	gitT(t, repo, "config", "core.fsmonitor", hook)
	post := filepath.Join(repo, ".git", "hooks", "post-checkout")
	write(t, post, script)
	_ = os.Chmod(post, 0o755)
	gitT(t, wa, "status")
	gitT(t, wa, "checkout", "HEAD", "--", "a.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded git did not run the child's config or hook")
	}
	_ = os.Remove(marker)
	write(t, filepath.Join(wa, "a.txt"), "chosen\n")
	write(t, filepath.Join(wb, "a.txt"), "alternative\n")
	out := pivotPatch(patchArg(wb, wa, base2, base, common), []any{})
	_ = out
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the child's config or hook ran under a pivot")
	}
}

func TestADotGitAtASiblingsGitDirIsRefusedByAPivot(t *testing.T) {
	_, wa, wb, base, common := twoWorktrees(t)
	sgd := strings.TrimSpace(gitT(t, wb, "rev-parse", "--path-format=absolute", "--git-dir"))
	write(t, filepath.Join(wa, "evil.txt"), "evil\n")
	write(t, filepath.Join(wa, ".git"), "gitdir: "+sgd+"\n")
	before, _ := os.ReadFile(filepath.Join(sgd, "index"))
	out := pivotPatch(patchArg(wb, wa, base, base, common), []any{})
	if out == nil || out.Value("applied") != false || !strings.Contains(out.Value("why").(string), "no longer belongs") {
		t.Fatalf("pivot: %v", out)
	}
	after, _ := os.ReadFile(filepath.Join(sgd, "index"))
	if string(before) != string(after) {
		t.Fatal("the sibling's index was written")
	}
}

func TestAFailedMergeLeavesNoNewFileOfTheAlternativeBehind(t *testing.T) {
	repo, wa, wb, base, common := twoWorktrees(t)
	// wb was cut at a later commit: its patch meets a tree it was not made
	// for, so it reaches the three-way merge and conflicts there.
	write(t, filepath.Join(repo, "a.txt"), "two\n")
	gitT(t, repo, "commit", "-qam", "second")
	base2 := strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	gitT(t, wb, "checkout", "-q", "--detach", base2)
	write(t, filepath.Join(wa, "a.txt"), "chosen\n")
	write(t, filepath.Join(wb, "a.txt"), "alternative\n")
	write(t, filepath.Join(wb, "only_b.txt"), "only in b\n")
	out := pivotPatch(patchArg(wb, wa, base2, base, common), []any{})
	if out == nil || out.Value("applied") != false || !strings.Contains(out.Value("why").(string), "git apply failed") {
		t.Fatalf("pivot: %v", out)
	}
	if _, err := os.Stat(filepath.Join(wa, "only_b.txt")); err == nil {
		t.Fatal("the alternative's new file was left behind")
	}
	if b, _ := os.ReadFile(filepath.Join(wa, "a.txt")); string(b) != "chosen\n" {
		t.Fatalf("a.txt is %q", b)
	}
	if u := gitT(t, wa, "diff", "--name-only", "--diff-filter=U"); u != "" {
		t.Fatalf("unmerged: %q", u)
	}
}

func TestANestedRepositoryTheChildMadeRunsNoProgramUnderAPivot(t *testing.T) {
	repo, wa, wb, base, common := twoWorktrees(t)
	root := filepath.Dir(repo)
	marker := filepath.Join(root, "ran-nested")
	script := filepath.Join(root, "clean.sh")
	write(t, script, "#!/bin/sh\necho x >> "+marker+"\ncat\n")
	_ = os.Chmod(script, 0o755)
	sub := filepath.Join(wa, "sub")
	_ = os.MkdirAll(sub, 0o755)
	gitT(t, sub, "init", "-q")
	write(t, filepath.Join(sub, "s.txt"), "aaaa\n")
	write(t, filepath.Join(sub, ".gitattributes"), "* filter=x\n")
	gitT(t, sub, "add", "-A")
	gitT(t, sub, "commit", "-qm", "n")
	gitT(t, sub, "config", "filter.x.clean", script)
	gitT(t, wa, "add", "sub")
	touch := func(text string, sec int64) {
		f := filepath.Join(sub, "s.txt")
		write(t, f, text)
		tm := time.Unix(sec, 0)
		_ = os.Chtimes(f, tm, tm)
	}
	touch("bbbb\n", 1)
	gitT(t, wa, "diff", "--name-only", base)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded diff did not run the nested filter")
	}
	_ = os.Remove(marker)
	touch("cccc\n", 7)
	write(t, filepath.Join(wa, "a.txt"), "chosen\n")
	write(t, filepath.Join(wb, "a.txt"), "alternative\n")
	if out := pivotPatch(patchArg(wb, wa, base, base, common), []any{}); out != nil {
		t.Fatalf("pivot: %v", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the nested repository's filter ran under a pivot")
	}
}

func TestAPivotReadsAChildThatCommittedIntoItsOwnStore(t *testing.T) {
	_, wa, wb, base, common := twoWorktrees(t)
	for _, w := range []string{wa, wb} {
		name := filepath.Base(w)
		mine := filepath.Join(common, "worktrees", name, "objects")
		_ = os.MkdirAll(mine, 0o700)
		write(t, filepath.Join(w, "a.txt"), name+" edit\n")
		write(t, filepath.Join(w, "new_"+name+".txt"), "n\n")
		for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c"}} {
			cmd := exec.Command("git", append([]string{"-C", w}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_OBJECT_DIRECTORY="+mine, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(common, "objects"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	// A store that was not folded away is not read.
	if out := pivotPatch(patchArg(wb, wa, base, base, common), []any{}); out == nil {
		t.Fatal("a pivot read a worktree whose store was not folded")
	}
	for _, w := range []string{wa, wb} {
		if !branchgit.Fold(common, w, base) {
			t.Fatalf("fold %s", w)
		}
	}
	if out := pivotPatch(patchArg(wb, wa, base, base, common), []any{}); out != nil {
		t.Fatalf("pivot: %v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(wa, "a.txt")); string(b) != "wb edit\n" {
		t.Fatalf("a.txt: %q", b)
	}
	if _, err := os.Stat(filepath.Join(wa, "new_wb.txt")); err != nil {
		t.Fatal("the alternative's new file is missing")
	}
	// Every blob the pivot staged is in the shared store, and the repository
	// stays whole.
	ls, err := exec.Command("git", "-C", wa, "ls-files", "-s").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(ls)), "\n") {
		oid := strings.Fields(line)[1]
		if err := exec.Command("git", "--git-dir", common, "cat-file", "-e", oid).Run(); err != nil {
			t.Fatalf("staged blob not in the shared store: %s", line)
		}
	}
	for _, args := range [][]string{{"fsck"}, {"gc", "-q"}, {"log", "--all", "--oneline"}, {"rev-list", "--all"}} {
		if out, err := exec.Command("git", append([]string{"--git-dir", common}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
