package supervise

import (
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"daisugi-verify/internal/branchgit"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tree"
)

func envObj(t *testing.T, id string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(`{"id": "` + id + `", "generated_by": "t", "task": "t", "permissions": {"shell": true, "shell_allowlist": ["git"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

// committingStart does what a fenced child does: edit its worktree, then add
// and commit with the branch environment, so the objects land in the child's
// own store. extra runs last.
func committingStart(t *testing.T, extra func(wt, session, common string, env []string)) func(string, string, NodeOpts) (*NodeRun, error) {
	return func(dataDir, wardenRoot string, o NodeOpts) (*NodeRun, error) {
		sid := *o.Session
		n := tree.ReadTree(dataDir).Nodes[sid]
		common := *n.Repo
		env := BranchEnv(os.Environ(), sid, common)
		_ = os.WriteFile(filepath.Join(o.Cwd, "a.txt"), []byte(sid+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(o.Cwd, "new.txt"), []byte("fresh\n"), 0o644)
		run := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", o.Cwd, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("git %v: %v\n%s", args, err, out)
			}
		}
		run("add", "-A")
		run("commit", "-qm", "c")
		_ = os.WriteFile(filepath.Join(o.Cwd, "a.txt"), []byte(sid+" again\n"), 0o644)
		run("add", "a.txt")
		if extra != nil {
			extra(o.Cwd, sid, common, env)
		}
		ans := "answer " + sid
		return &NodeRun{Status: "started", Session: &sid, Answer: &ans, TokensUsed: big.NewInt(10), TurnsUsed: big.NewInt(1)}, nil
	}
}

func branchRun(t *testing.T, extra func(wt, session, common string, env []string)) (repo, data string, run *BranchRun) {
	t.Helper()
	return branchRunFenced(t, extra, nil)
}

// branchRunFenced is branchRun with a fence prefix on the starter.
func branchRunFenced(t *testing.T, extra func(wt, session, common string, env []string), fence []string) (repo, data string, run *BranchRun) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	data = filepath.Join(root, "d")
	wr := filepath.Join(data, "warden")
	_ = os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")
	if _, err := tree.MakeRoot(data, wr, envObj(t, "env_root"), "top", big.NewInt(1000), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	s := &Starter{Environ: os.Environ(), Fence: fence}
	run, err := s.RunBranches(data, wr, BranchOpts{Parent: "top", Envelope: envObj(t, "env_kid"), Cwd: repo, Attempts: 2,
		Parallel: 2, Tokens: big.NewInt(100), Turns: big.NewInt(2), Task: "t", Why: "operator", TimeoutS: 5,
		Model: "m", Start: committingStart(t, extra)})
	if err != nil {
		t.Fatal(err)
	}
	return repo, data, run
}

func whole(t *testing.T, repo string) {
	t.Helper()
	for _, args := range [][]string{{"fsck"}, {"gc", "-q"}, {"log", "--all", "--oneline"}, {"rev-list", "--all"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestAdoptingFromAChildThatCommittedLeavesTheOperatorRepositoryWhole(t *testing.T) {
	repo, data, run := branchRun(t, nil)
	if run.Status != "ran" || run.LeaderSession == nil {
		t.Fatalf("run: %+v", run)
	}
	sess := *run.LeaderSession
	_, common, _ := branchRef(data, sess)
	whole(t, repo) // while the branch worktrees exist
	for _, b := range run.Branches {
		sid := b.Value("session").(string)
		if _, err := os.Lstat(filepath.Join(common, "worktrees", sid, "objects")); err == nil {
			t.Fatalf("the store of %s is still there", sid)
		}
	}
	if res := Adopt(data, sess, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != sess+" again\n" {
		t.Fatalf("a.txt: %q", b)
	}
	whole(t, repo)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "adopted")
	for _, b := range run.Branches {
		RemoveWorktree(b.Value("worktree").(string), common)
	}
	whole(t, repo)
}

func TestAStoreThatWasNotFoldedIsNotRead(t *testing.T) {
	_, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	base, common, _ := branchRef(data, session)
	_ = os.Mkdir(filepath.Join(common, "worktrees", session, "objects"), 0o700)
	if res := Adopt(data, session, ""); res.Status != "refused" {
		t.Fatalf("adopt: %+v", res)
	}
	if _, ok := branchgit.Diff(wt, base, common, nil, true, 0); ok {
		t.Fatal("diff read an unfolded worktree")
	}
	if !branchgit.Fold(common, wt, base) {
		t.Fatal("fold")
	}
	if res := Adopt(data, session, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
}

func TestTheFoldFollowsNoLinkTheChildMade(t *testing.T) {
	repo, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	base, common, _ := branchRef(data, session)
	gd := filepath.Join(common, "worktrees", session)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "operator moves on")
	tip := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	config, _ := os.ReadFile(filepath.Join(common, "config"))
	index, _ := os.ReadFile(filepath.Join(common, "index"))
	keep := filepath.Join(filepath.Dir(repo), "keep")
	_ = os.Mkdir(keep, 0o700)
	_ = os.WriteFile(filepath.Join(keep, "HEAD"), []byte("keep\n"), 0o600)
	_ = os.Mkdir(filepath.Join(gd, "objects"), 0o700)
	_ = os.WriteFile(filepath.Join(gd, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	_ = os.Remove(filepath.Join(gd, "index"))
	_ = os.Symlink(filepath.Join(common, "index"), filepath.Join(gd, "index"))
	_ = os.RemoveAll(filepath.Join(gd, "logs"))
	_ = os.Symlink(keep, filepath.Join(gd, "logs"))
	_ = os.WriteFile(filepath.Join(gd, "HEAD.lock"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(gd, "index.lock"), 0o700)
	if !branchgit.Fold(common, wt, base) {
		t.Fatal("fold")
	}
	if got := strings.TrimSpace(git(t, repo, "rev-parse", "main")); got != tip {
		t.Fatal("the fold moved the operator's branch")
	}
	if c, _ := os.ReadFile(filepath.Join(common, "config")); string(c) != string(config) {
		t.Fatal("the fold wrote the shared config")
	}
	if i, _ := os.ReadFile(filepath.Join(common, "index")); string(i) != string(index) {
		t.Fatal("the fold wrote the operator's index")
	}
	if k, _ := os.ReadFile(filepath.Join(keep, "HEAD")); string(k) != "keep\n" {
		t.Fatal("the fold followed the logs link")
	}
	if h, _ := os.ReadFile(filepath.Join(gd, "HEAD")); strings.TrimSpace(string(h)) != base {
		t.Fatalf("HEAD: %q", h)
	}
	if res := Adopt(data, session, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
	whole(t, repo)
}

func TestAnAlternatesFileTheChildWroteIsNeverOpened(t *testing.T) {
	done := make(chan *AdoptResult, 1)
	go func() {
		_, data, run := branchRun(t, func(wt, session, common string, env []string) {
			info := filepath.Join(common, "worktrees", session, "objects", "info")
			_ = os.MkdirAll(info, 0o700)
			_ = syscall.Mkfifo(filepath.Join(info, "alternates"), 0o600)
			_ = os.WriteFile(filepath.Join(wt, "a.txt"), []byte(session+" late\n"), 0o644)
		})
		if run.LeaderSession == nil {
			done <- &AdoptResult{Status: "no leader"}
			return
		}
		done <- Adopt(data, *run.LeaderSession, "")
	}()
	select {
	case res := <-done:
		if res.Status != "adopted" {
			t.Fatalf("adopt: %+v", res)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a parent git call opened the child's alternates")
	}
}

func TestAProcessTheChildLeftIsStoppedBeforeTheParentReads(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "pids")
	defer func() {
		b, _ := os.ReadFile(pids)
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	repo, data, run := branchRun(t, func(wt, session, common string, env []string) {
		gd := filepath.Join(common, "worktrees", session)
		loop := "echo $$ >> " + pids + "; sleep 0.3; i=0; while [ $i -lt 300 ]; do : > " + gd +
			"/config.worktree; mkdir -p " + gd + "/objects; git -C " + wt +
			" add -A >/dev/null 2>&1; sleep 0.01; i=$((i+1)); done"
		if err := exec.Command("sh", "-c", "setsid sh -c '"+loop+"' </dev/null >/dev/null 2>&1 &").Run(); err != nil {
			t.Error(err)
		}
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if b, _ := os.ReadFile(pids); strings.Contains(string(b), "\n") {
				break
			}
		}
	})
	b, _ := os.ReadFile(pids)
	left := strings.Fields(string(b))
	if len(left) != 2 {
		t.Fatalf("pids: %q", b)
	}
	for _, f := range left {
		pid, _ := strconv.Atoi(f)
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("pid %d the child left is still alive", pid)
		}
	}
	if run.Status != "ran" || run.LeaderSession == nil {
		t.Fatalf("run: %+v", run)
	}
	time.Sleep(600 * time.Millisecond)
	sess := *run.LeaderSession
	_, common, _ := branchRef(data, sess)
	if _, err := os.Lstat(filepath.Join(common, "worktrees", sess, "config.worktree")); err == nil {
		t.Fatal("a config.worktree appeared after the run")
	}
	if res := Adopt(data, sess, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
	whole(t, repo)
}

func TestAGrantIsReadAsTheFenceReadsIt(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	common := filepath.Join(repo, ".git")
	_ = os.MkdirAll(common, 0o755)
	link := filepath.Join(root, "plink")
	_ = os.Symlink(repo, link)
	wt := filepath.Join(root, "d", "tree", "branches", "n-aaaaaaaaaaaa")
	ef := filepath.Join(root, "env.json")
	_ = os.WriteFile(ef, []byte(`{"file_write": ["`+link+`/.git/**"]}`), 0o600)
	for name, g := range map[string][]string{
		"tilde":        {"--allow", "~/repo:rw"},
		"workdir-home": {"--workdir=~"},
		"link":         {"--allow", link + ":rw"},
		"relative":     {"--allow", "../../../../repo/.git:rw"},
	} {
		if got := FenceCovers(append([]string{"--harness", "sprig"}, g...), "", common, root, wt); got == "" {
			t.Errorf("%s: no refusal", name)
		}
	}
	if got := FenceCovers([]string{"--harness", "sprig"}, ef, common, root, wt); got == "" {
		t.Error("link-envelope: no refusal")
	}
	if got := FenceCovers([]string{"--harness", "sprig", "--allow", "~/other:rw"}, "", common, root, wt); got != "" {
		t.Errorf("a grant elsewhere refused: %q", got)
	}
}

func TestAPlantedReftableDirectoryIsFoldedAway(t *testing.T) {
	_, data, session := repoWithBranch(t)
	wt := filepath.Join(BranchesDir(data), session)
	base, common, _ := branchRef(data, session)
	gd := filepath.Join(common, "worktrees", session)
	_ = os.Mkdir(filepath.Join(gd, "objects"), 0o700)
	_ = os.Mkdir(filepath.Join(gd, "reftable"), 0o700)
	if !branchgit.Fold(common, wt, base) {
		t.Fatal("a planted reftable directory stopped the fold")
	}
	for _, part := range []string{"objects", "reftable"} {
		if _, err := os.Lstat(filepath.Join(gd, part)); err == nil {
			t.Fatalf("%s is still there", part)
		}
	}
	if res := Adopt(data, session, ""); res.Status != "adopted" {
		t.Fatalf("adopt: %+v", res)
	}
}

// writable gives the test back the rights a child took, so the temp dir can go.
func writable(root string) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func adminOf(t *testing.T, wt string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// left is what a hostile child leaves in its admin directory after it committed.
func left(t *testing.T, kind string) func(wt, session, common string, env []string) {
	return func(wt, session, common string, env []string) {
		admin := adminOf(t, wt)
		switch kind {
		case "store":
			_ = os.RemoveAll(filepath.Join(admin, "objects"))
		case "admin":
			_ = os.Chmod(admin, 0o555)
		case "logs-refs":
			cmd := exec.Command("git", "-C", wt, "update-ref", "refs/worktree/keep", "HEAD")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("update-ref: %v %s", err, out)
			}
			for _, part := range []string{"logs", "refs/worktree", "refs"} {
				_ = os.Chmod(filepath.Join(admin, part), 0o555)
			}
		case "reftable":
			_ = os.Mkdir(filepath.Join(admin, "reftable"), 0o700)
			_ = os.Chmod(filepath.Join(admin, "reftable"), 0o000)
		}
	}
}

func TestAFencedBranchIsFoldedWhateverTheChildLeft(t *testing.T) {
	for _, kind := range []string{"store", "admin", "logs-refs", "reftable"} {
		t.Run(kind, func(t *testing.T) {
			repo, data, run := branchRunFenced(t, left(t, kind), []string{"fence"})
			defer writable(filepath.Dir(repo))
			whole(t, repo) // right after the run, while the branch worktrees exist
			if run.Status != "ran" || run.LeaderSession == nil {
				t.Fatalf("run: %+v", run)
			}
			_, common, _ := branchRef(data, *run.LeaderSession)
			for _, b := range run.Branches {
				sid := b.Value("session").(string)
				if b.Value("status").(string) != "ran" {
					t.Fatalf("%s: %v", sid, b.Value("status"))
				}
				for _, part := range []string{"objects", "logs", "refs", "reftable"} {
					if _, err := os.Lstat(filepath.Join(common, "worktrees", sid, part)); err == nil {
						t.Fatalf("%s: %s is still there", sid, part)
					}
				}
			}
			if res := Adopt(data, *run.LeaderSession, ""); res.Status != "adopted" {
				t.Fatalf("adopt: %+v", res)
			}
			whole(t, repo)
		})
	}
}

func TestABranchWhoseFoldFailsIsRemovedAndNotRanked(t *testing.T) {
	var calls []string
	was := foldBranch
	foldBranch = func(repo, wt, base string) bool {
		calls = append(calls, filepath.Base(wt))
		if len(calls) == 1 {
			return false
		}
		return was(repo, wt, base)
	}
	defer func() { foldBranch = was }()
	lock := func(wt, session, common string, env []string) {
		admin := adminOf(t, wt)
		_ = os.WriteFile(filepath.Join(admin, "locked"), []byte("mine\n"), 0o600)
		_ = os.Chmod(admin, 0o555)
	}
	repo, data, run := branchRunFenced(t, lock, []string{"fence"})
	defer writable(filepath.Dir(repo))
	whole(t, repo)
	var bad *pyjson.Object
	for _, b := range run.Branches {
		if b.Value("session").(string) == calls[0] {
			bad = b
		}
	}
	if bad == nil || bad.Value("status").(string) != "failed" {
		t.Fatalf("the branch whose fold failed: %+v", bad)
	}
	sid := calls[0]
	common := filepath.Join(repo, ".git")
	if _, err := os.Lstat(filepath.Join(common, "worktrees", sid)); err == nil {
		t.Fatal("its admin directory is still there")
	}
	if _, err := os.Lstat(bad.Value("worktree").(string)); err == nil {
		t.Fatal("its worktree is still there")
	}
	if out, _ := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output(); strings.Contains(string(out), sid) {
		t.Fatalf("git still lists it: %s", out)
	}
	if run.Ranking != nil {
		if order, ok := run.Ranking.Value("order").([]any); ok {
			for _, m := range order {
				if m == sid {
					t.Fatal("it was ranked")
				}
			}
		}
	}
	_ = data
}

func TestAReftableRepositoryRefusesAFencedBranchRunUpFront(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	data := filepath.Join(root, "d")
	wr := filepath.Join(data, "warden")
	_ = os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main", "--ref-format=reftable")
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")
	if _, err := tree.MakeRoot(data, wr, envObj(t, "env_root"), "top", big.NewInt(1000), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	started := 0
	s := &Starter{Environ: os.Environ(), Fence: []string{"fence"}}
	run, err := s.RunBranches(data, wr, BranchOpts{Parent: "top", Envelope: envObj(t, "env_kid"), Cwd: repo, Attempts: 2,
		Parallel: 2, Tokens: big.NewInt(100), Turns: big.NewInt(2), Task: "t", Why: "operator", TimeoutS: 5, Model: "m",
		Start: func(string, string, NodeOpts) (*NodeRun, error) { started++; return nil, nil }})
	if err != nil || run.Status != "refused" || !run.Usage || len(run.Reasons) != 1 ||
		!strings.Contains(run.Reasons[0], "reftable") || strings.Contains(run.Reasons[0], "\n") {
		t.Fatalf("%+v %v", run, err)
	}
	if started != 0 {
		t.Fatal("a child started")
	}
	if _, err := os.Lstat(BranchesDir(data)); err == nil {
		t.Fatal("a worktree was made")
	}
	if n := tree.ReadTree(data).Proposals["top"]; n != 0 {
		t.Fatalf("budget spent: %d proposals", n)
	}
}

func TestABranchRunRefusesAllowWeakIPC(t *testing.T) {
	for _, w := range []string{"--allow-weak-ipc", "--allow-weak-ipc=1"} {
		repo, data, _ := repoWithBranch(t)
		wardenRoot := filepath.Join(data, "warden")
		before := tree.ReadTree(data).Proposals["top"]
		s := &Starter{Fence: []string{"fence", "--harness", "sprig", w}, Environ: os.Environ()}
		run, err := s.RunBranches(data, wardenRoot, BranchOpts{Parent: "top", Cwd: repo, Attempts: 2})
		if err != nil || run.Status != "refused" || !run.Usage || len(run.Reasons) != 1 ||
			!strings.Contains(run.Reasons[0], "--allow-weak-ipc") {
			t.Fatalf("%s: %+v %v", w, run, err)
		}
		if n := tree.ReadTree(data).Proposals["top"]; n != before {
			t.Fatalf("%s: budget spent", w)
		}
	}
}

// R5-2: a branch fence that holds --no-fence confines nothing.
func TestABranchRunRefusesNoFence(t *testing.T) {
	for _, w := range []string{"--no-fence", "--no-fence=1"} {
		repo, data, _ := repoWithBranch(t)
		wardenRoot := filepath.Join(data, "warden")
		before := tree.ReadTree(data).Proposals["top"]
		s := &Starter{Fence: []string{"fence", "--harness", "sprig", w}, Environ: os.Environ()}
		run, err := s.RunBranches(data, wardenRoot, BranchOpts{Parent: "top", Cwd: repo, Attempts: 2})
		if err != nil || run.Status != "refused" || !run.Usage || len(run.Reasons) != 1 ||
			run.Reasons[0] != NoFenceText {
			t.Fatalf("%s: %+v %v", w, run, err)
		}
		if n := tree.ReadTree(data).Proposals["top"]; n != before {
			t.Fatalf("%s: budget spent", w)
		}
	}
}
