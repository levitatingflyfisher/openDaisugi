package supervise

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/internal/branchgit"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// recordBranch writes the parent's ledger rows for one branch: the root and
// the branch's spawn row, which holds the base and the repository.
func recordBranch(t *testing.T, data, session, base, repo string) {
	t.Helper()
	dir := filepath.Join(data, "tree")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := `{"event": "root", "session": "top", "envelope_id": "env_root", "tokens": null, "turns": null, "deadline": null, "ts": 1.0}` + "\n" +
		`{"event": "spawn", "parent": "top", "session": "` + session + `", "envelope_id": "env_kid", "tokens": null, "turns": null, "deadline": null, "proved": true, "branch_of": "br_x", "why": "operator", "base": "` + base + `", "repo": "` + repo + `", "ts": 2.0}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(rows); err != nil {
		t.Fatal(err)
	}
}

func repoWithBranch(t *testing.T) (repo, data, session string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	data = filepath.Join(root, "data")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")
	session = "n-aaaaaaaaaaaa"
	base, common, err := repoOf(repo)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := MakeWorktree(repo, data, session, base)
	if err != nil {
		t.Fatal(err)
	}
	recordBranch(t, data, session, base, common)
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, data, session
}

func TestAdoptAppliesTheBranchEdits(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	res := Adopt(data, session, "")
	if res.Status != "adopted" || len(res.Files) != 1 || res.Files[0] != "a.txt" {
		t.Fatalf("adopt: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "two\n" {
		t.Fatalf("a.txt is %q", b)
	}
}

func TestAdoptRefusesADirtyFileAndWritesNothing(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Adopt(data, session, "")
	if res.Status != "refused" || !strings.Contains(res.Why, "a.txt has uncommitted changes") {
		t.Fatalf("adopt: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "mine\n" {
		t.Fatalf("a.txt is %q", b)
	}
}

func TestAdoptRefusesAConflictAndRestoresTheTree(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "moved on")
	res := Adopt(data, session, "")
	if res.Status != "refused" || !strings.Contains(res.Why, "do not apply cleanly") {
		t.Fatalf("adopt: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "other\n" {
		t.Fatalf("a.txt is %q", b)
	}
	if st := git(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("the tree is not clean: %q", st)
	}
}

func TestAdoptRefusesAnUnknownSession(t *testing.T) {
	_, data, _ := repoWithBranch(t)
	for _, s := range []string{"nope", "../x", ""} {
		if res := Adopt(data, s, ""); res.Status != "refused" {
			t.Errorf("adopt %q: %+v", s, res)
		}
	}
}

func TestBranchHashFollowsTheEditsAndTheAnswer(t *testing.T) {
	_, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	base, repo, ok := branchRef(data, session)
	if !ok {
		t.Fatal("no recorded base")
	}
	h1 := BranchHash(wt, base, repo, "answer")
	if h2 := BranchHash(wt, base, repo, "answer"); h1 != h2 {
		t.Fatal("the hash moved")
	}
	if BranchHash(wt, base, repo, "other") == h1 {
		t.Fatal("the answer is not in the hash")
	}
	if !strings.HasPrefix(h1, "sha256:") {
		t.Fatalf("hash %q", h1)
	}
}

func victimRepo(t *testing.T, root string) (victim, victimWt string) {
	t.Helper()
	victim = filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, victim, "init", "-q", "-b", "main")
	for n, b := range map[string]string{"secret.txt": "keep me\n", "a.txt": "victim\n"} {
		if err := os.WriteFile(filepath.Join(victim, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, victim, "add", "-A")
	git(t, victim, "commit", "-qm", "init")
	victimWt = filepath.Join(root, "victim-wt")
	git(t, victim, "worktree", "add", "-q", "--detach", victimWt, "HEAD")
	return victim, victimWt
}

func TestABaseNoteThatNamesAnOutputFileWritesNothing(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	gd := strings.TrimSpace(git(t, wt, "rev-parse", "--path-format=absolute", "--git-dir"))
	target := filepath.Join(filepath.Dir(repo), "pwned")
	if err := os.WriteFile(filepath.Join(gd, "daisugi-base"), []byte("--output="+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, rp, _ := branchRef(data, session)
	BranchHash(wt, base, rp, "x")
	diffLines(wt, base, rp)
	if res := Adopt(data, session, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the base note wrote a file")
	}
	// A base that is not an object id is never given to git.
	if _, ok := branchgit.Diff(wt, "--output="+target, rp, nil, true, 0); ok {
		t.Fatal("a dash base ran")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a dash base wrote a file")
	}
}

func TestARedirectedDotGitIsRefusedAndNoRepositoryIsWritten(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	root := filepath.Dir(repo)
	victim, victimWt := victimRepo(t, root)
	wt := filepath.Join(BranchesDir(data), session)
	vgd := strings.TrimSpace(git(t, victimWt, "rev-parse", "--path-format=absolute", "--git-dir"))
	idx := func(p string) []byte { b, _ := os.ReadFile(p); return b }
	beforeWt, beforeMain := idx(filepath.Join(vgd, "index")), idx(filepath.Join(victim, ".git", "index"))
	for _, target := range []string{vgd, filepath.Join(victim, ".git")} {
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := Adopt(data, session, "")
		if res.Status != "refused" || !strings.Contains(res.Why, "no longer belongs to") {
			t.Fatalf("adopt with .git -> %s: %+v", target, res)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "secret.txt")); string(b) != "keep me\n" {
		t.Fatalf("secret.txt is %q", b)
	}
	if st := git(t, victim, "status", "--porcelain"); st != "" {
		t.Fatalf("victim is dirty: %q", st)
	}
	if string(idx(filepath.Join(vgd, "index"))) != string(beforeWt) || string(idx(filepath.Join(victim, ".git", "index"))) != string(beforeMain) {
		t.Fatal("a victim index was written")
	}
	base, rp, _ := branchRef(data, session)
	if n := diffLines(wt, base, rp); n != 0 {
		t.Fatalf("diff lines of a stranger: %d", n)
	}
}

func TestAdoptIntoAnotherRepositoryIsRefused(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	victim, _ := victimRepo(t, filepath.Dir(repo))
	res := Adopt(data, session, victim)
	if res.Status != "refused" {
		t.Fatalf("adopt: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "a.txt")); string(b) != "victim\n" {
		t.Fatalf("a.txt is %q", b)
	}
}

func TestAdoptRefusesANulSessionWithoutPanicking(t *testing.T) {
	_, data, _ := repoWithBranch(t)
	for _, s := range []string{"a\x00b", "default"} {
		if res := Adopt(data, s, ""); res.Status != "refused" {
			t.Errorf("adopt %q: %+v", s, res)
		}
	}
}

func TestALoneSurvivorIsPrunedOnlyWhenAgedOut(t *testing.T) {
	_, data, session := repoWithBranch(t)
	// End the branch at ts 3 (the fixture's rows are old, 1970).
	f, _ := os.OpenFile(filepath.Join(data, "tree", "ledger.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"event": "end", "session": "` + session + `", "tokens_used": null, "turns_used": null, "ts": 3.0}` + "\n")
	f.Close()
	if got := PruneBranchesAt(data, 3.0+86400); len(got) != 0 {
		t.Fatalf("a young cardless branch was pruned: %v", got)
	}
	if _, err := os.Stat(filepath.Join(BranchesDir(data), session)); err != nil {
		t.Fatal("the worktree is gone")
	}
	if got := PruneBranchesAt(data, 3.0+15*86400); len(got) != 1 || got[0] != session {
		t.Fatalf("an aged branch was kept: %v", got)
	}
}

// R5-1: a child may take the parent's rights on its admin dir. The clean
// reclaims them before it removes the worktree, so the branch still goes.
func TestPruneTakesBackAnAdminDirTheChildLocked(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	common, err := exec.Command("git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	admin := filepath.Join(strings.TrimSpace(string(common)), "worktrees", session)
	if st, err := os.Stat(admin); err != nil || !st.IsDir() {
		t.Fatalf("no admin dir %s: %v", admin, err)
	}
	if err := os.Chmod(admin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(admin, 0o755) })
	f, _ := os.OpenFile(filepath.Join(data, "tree", "ledger.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"event": "end", "session": "` + session + `", "tokens_used": null, "turns_used": null, "ts": 3.0}` + "\n")
	f.Close()
	if got := PruneBranchesAt(data, 3.0+15*86400); len(got) != 1 || got[0] != session {
		t.Fatalf("pruned %v", got)
	}
	if _, err := os.Stat(admin); err == nil {
		t.Fatal("the admin dir is still there")
	}
}

func readIndex(p string) []byte { b, _ := os.ReadFile(p); return b }

func TestDotGitAtTheMainGitDirIsRefusedAndTheMainIndexStays(t *testing.T) {
	for _, form := range []string{"file", "symlink"} {
		repo, data, session := repoWithBranch(t)
		wt := filepath.Join(BranchesDir(data), session)
		main := filepath.Join(repo, ".git")
		if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(wt, ".git"))
		if form == "file" {
			_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+main+"\n"), 0o644)
		} else if err := os.Symlink(main, filepath.Join(wt, ".git")); err != nil {
			t.Fatal(err)
		}
		before := readIndex(filepath.Join(main, "index"))
		base, rp, _ := branchRef(data, session)
		diffLines(wt, base, rp)
		BranchHash(wt, base, rp, "x")
		res := Adopt(data, session, "")
		if res.Status != "refused" || !strings.Contains(res.Why, "no longer belongs to") {
			t.Fatalf("%s: adopt: %+v", form, res)
		}
		if string(readIndex(filepath.Join(main, "index"))) != string(before) {
			t.Fatalf("%s: the main index was written", form)
		}
		if st := git(t, repo, "status", "--porcelain"); st != "" {
			t.Fatalf("%s: the project is dirty: %q", form, st)
		}
	}
}

func TestDotGitAtASiblingsGitDirIsRefusedAndTheSiblingIndexStays(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	base, common, _ := branchRef(data, session)
	sibSession := "n-bbbbbbbbbbbb"
	sib, err := MakeWorktree(repo, data, sibSession, base)
	if err != nil {
		t.Fatal(err)
	}
	recordBranch(t, data, sibSession, base, common)
	sgd := strings.TrimSpace(git(t, sib, "rev-parse", "--path-format=absolute", "--git-dir"))
	wt := filepath.Join(BranchesDir(data), session)
	_ = os.WriteFile(filepath.Join(wt, "evil.txt"), []byte("evil\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+sgd+"\n"), 0o644)
	before, status := readIndex(filepath.Join(sgd, "index")), git(t, sib, "status", "--porcelain")
	diffLines(wt, base, common)
	res := Adopt(data, session, "")
	if res.Status != "refused" || !strings.Contains(res.Why, "no longer belongs to") {
		t.Fatalf("adopt: %+v", res)
	}
	if string(readIndex(filepath.Join(sgd, "index"))) != string(before) || git(t, sib, "status", "--porcelain") != status {
		t.Fatal("the sibling's index was written")
	}
}

// armRepo writes the repository config and the hook a child could have
// written. It returns the marker they create when they run.
func armRepo(t *testing.T, repo string) string {
	t.Helper()
	marker := filepath.Join(filepath.Dir(repo), "ran")
	script := "#!/bin/sh\necho x >> " + marker + "\n"
	hook := filepath.Join(filepath.Dir(repo), "hook.sh")
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.fsmonitor", hook)
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestAConfigAndAHookTheChildWroteDoNotRunUnderTheParentsReads(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	marker := armRepo(t, repo)
	git(t, wt, "status")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded git did not run the child's fsmonitor")
	}
	_ = os.Remove(marker)
	base, rp, _ := branchRef(data, session)
	diffLines(wt, base, rp)
	BranchHash(wt, base, rp, "x")
	branchgit.Diff(wt, base, rp, nil, true, 0)
	if res := Adopt(data, session, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the child's config or hook ran")
	}
}

func TestAHookTheChildWroteDoesNotRunOnARefusedAdopt(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "moved on")
	marker := armRepo(t, repo)
	git(t, repo, "checkout", "HEAD", "--", "a.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded checkout did not run the hook")
	}
	_ = os.Remove(marker)
	if res := Adopt(data, session, ""); res.Status != "refused" || !strings.Contains(res.Why, "do not apply cleanly") {
		t.Fatalf("adopt: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the hook ran on the refusal path")
	}
}

func TestABranchFenceHasItsWorktreeAsWorkspaceAndTheRepoReadOnly(t *testing.T) {
	words := []string{"fence", "--harness", "sprig", "--workspace", "/proj", "--allow", "/s:rw"}
	got := BranchFence(words, "/data/tree/branches/n-x", "n-x", "/proj/.git")
	want := []string{"fence", "--harness", "sprig", "--workspace", "/data/tree/branches/n-x", "--allow", "/s:rw",
		"--allow", "/proj/.git:ro", "--allow", "/proj/.git/worktrees/n-x:rw"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
	if g := BranchFence([]string{"fence"}, "/w", "n-x", ""); strings.Join(g, " ") != "fence --workspace /w" {
		t.Fatalf("no workspace, no repo: %v", g)
	}
}

// nestedRepo plants a repository inside wt whose own config runs a program as
// a clean filter, and stages it as a gitlink the way the child could. It
// returns the marker the program writes.
func nestedRepo(t *testing.T, wt string) string {
	t.Helper()
	root := filepath.Dir(filepath.Dir(wt))
	marker := filepath.Join(root, "ran-nested")
	script := filepath.Join(root, "clean.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, sub, "init", "-q")
	_ = os.WriteFile(filepath.Join(sub, "s.txt"), []byte("aaaa\n"), 0o644)
	_ = os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("* filter=x\n"), 0o644)
	git(t, sub, "add", "-A")
	git(t, sub, "commit", "-qm", "n")
	git(t, sub, "config", "filter.x.clean", script)
	git(t, wt, "add", "sub")
	return marker
}

func touchSub(t *testing.T, wt, text string, sec int64) {
	t.Helper()
	f := filepath.Join(wt, "sub", "s.txt")
	if err := os.WriteFile(f, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := time.Unix(sec, 0)
	if err := os.Chtimes(f, tm, tm); err != nil {
		t.Fatal(err)
	}
}

func TestANestedRepositoryTheChildMadeRunsNoProgramInTheParent(t *testing.T) {
	_, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	marker := nestedRepo(t, wt)
	base, rp, _ := branchRef(data, session)
	touchSub(t, wt, "cccc\n", 2)
	git(t, wt, "diff", "--name-only", base)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded diff did not run the nested filter")
	}
	_ = os.Remove(marker)
	touchSub(t, wt, "dddd\n", 5)
	diffLines(wt, base, rp)
	BranchHash(wt, base, rp, "x")
	branchgit.Diff(wt, base, rp, nil, true, 0)
	if res := Adopt(data, session, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the nested repository's filter ran in the parent")
	}
}

func TestAConfigWorktreeTheChildWroteIsRefusedAndRunsNothing(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	git(t, repo, "config", "extensions.worktreeConfig", "true")
	gd := strings.TrimSpace(git(t, wt, "rev-parse", "--path-format=absolute", "--git-dir"))
	marker := filepath.Join(filepath.Dir(repo), "ran-wtconfig")
	script := filepath.Join(filepath.Dir(repo), "clean.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+marker+"\ncat\n"), 0o755)
	_ = os.WriteFile(filepath.Join(gd, "config.worktree"), []byte("[filter \"x\"]\n\tclean = "+script+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("* filter=x\n"), 0o644)
	tm := time.Unix(3, 0)
	_ = os.Chtimes(filepath.Join(wt, "a.txt"), tm, tm)
	git(t, wt, "diff", "HEAD")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: git did not run the child's config.worktree")
	}
	_ = os.Remove(marker)
	base, rp, _ := branchRef(data, session)
	if res := Adopt(data, session, ""); res.Status != "refused" || !strings.Contains(res.Why, "no longer belongs to") {
		t.Fatalf("adopt: %+v", res)
	}
	if _, ok := branchgit.Diff(wt, base, rp, nil, true, 0); ok {
		t.Fatal("diff read a worktree with a config.worktree")
	}
	_ = os.Remove(filepath.Join(gd, "config.worktree"))
	_ = os.Symlink(filepath.Join(filepath.Dir(repo), "nowhere"), filepath.Join(gd, "config.worktree"))
	if branchgit.Owns(wt, rp) {
		t.Fatal("a symlinked config.worktree counts too")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the child's config.worktree ran")
	}
}

func TestANonFencedStarterLeavesTheEnvironment(t *testing.T) {
	if got := BranchEnv([]string{"A=b"}, "n-aaaaaaaaaaaa", ""); len(got) != 1 {
		t.Fatalf("no repo, no change: %v", got)
	}
}

func TestABranchRefusesToStartWhenAnotherGrantCoversTheRepo(t *testing.T) {
	repo, data, _ := repoWithBranch(t)
	wardenRoot := filepath.Join(data, "warden")
	_ = os.MkdirAll(filepath.Join(wardenRoot, "envelopes"), 0o700)
	cases := map[string][]string{
		"rw":      {"--allow", repo + ":rw"},
		"above":   {"--allow", filepath.Dir(repo) + ":rw"},
		"workdir": {"--workdir", repo + "/.git/hooks"},
		"eq":      {"--allow=" + repo + "/.git:rw"},
	}
	for name, g := range cases {
		s := &Starter{Fence: append([]string{"fence", "--harness", "sprig"}, g...), Environ: os.Environ()}
		run, err := s.RunBranches(data, wardenRoot, BranchOpts{Cwd: repo, Attempts: 2})
		if err != nil || run.Status != "refused" || !run.Usage || len(run.Reasons) != 1 || !strings.Contains(run.Reasons[0], "covers") {
			t.Fatalf("%s: %+v %v", name, run, err)
		}
		if _, err := os.Stat(BranchesDir(data) + "/" + "x"); err == nil {
			t.Fatal("a worktree was made")
		}
	}
	for _, g := range [][]string{{"--allow", repo + ":ro", "--allow", repo + "/x"}, {"--allow", repo + "/src:rw"}} {
		common := filepath.Join(repo, ".git")
		if got := FenceCovers(append([]string{"--harness", "sprig"}, g...), "", common, "", repo); got != "" {
			t.Fatalf("no refusal for %v: %q", g, got)
		}
	}
	env, _ := json.Marshal(map[string]any{"permissions": map[string]any{"file_write": []string{repo + "/**"}}})
	ef := filepath.Join(wardenRoot, "envelopes", "default.json")
	_ = os.WriteFile(ef, env, 0o600)
	s := &Starter{Fence: []string{"fence", "--harness", "sprig"}, Environ: os.Environ()}
	run, _ := s.RunBranches(data, wardenRoot, BranchOpts{Cwd: repo, Attempts: 2})
	if run.Status != "refused" || !run.Usage {
		t.Fatalf("envelope: %+v", run)
	}
}
