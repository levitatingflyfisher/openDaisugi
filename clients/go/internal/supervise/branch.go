package supervise

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/branchgit"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/rank"
	"daisugi-verify/internal/tree"
)

// DefaultParallel is tree_branch.DEFAULT_PARALLEL.
const DefaultParallel = 4

func gitCmd(argv ...string) ([]byte, []byte, bool) {
	cmd := exec.Command("git", branchgit.Guarded(argv...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err == nil
}

// BranchesDir is tree_branch.branches_dir.
func BranchesDir(dataDir string) string { return filepath.Join(dataDir, "tree", "branches") }

// gitTop is tree_branch._git_top.
func gitTop(cwd string) (string, error) {
	out, _, ok := gitCmd("-C", cwd, "rev-parse", "--show-toplevel")
	if !ok {
		return "", fmt.Errorf("branching needs a git workspace; %s is not one", cwd)
	}
	return strings.TrimSpace(pystr.DecodeReplace(out)), nil
}

// repoOf is tree_branch.repo_of: the commit HEAD names and the repository's
// git directory, as the parent sees them now. The parent records both on
// the branch rows.
func repoOf(top string) (string, string, error) {
	head, _, ok := gitCmd("-C", top, "rev-parse", "HEAD")
	base := ""
	if ok {
		base = strings.TrimSpace(pystr.DecodeReplace(head))
	}
	common, cok := branchgit.CommonDir(top)
	if !tree.IsObjectID(base) || !cok {
		return "", "", fmt.Errorf("branching needs a git workspace with a commit; %s has none", top)
	}
	return base, common, nil
}

// MakeWorktree is tree_branch.make_worktree: a detached worktree of top's
// repository at base (HEAD when base is empty).
func MakeWorktree(top, dataDir, session, base string) (string, error) {
	if base != "" && !tree.IsObjectID(base) {
		return "", fmt.Errorf("the worktree for %s could not be made: base is not a commit id", session)
	}
	dir := BranchesDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, session)
	at := base
	if at == "" {
		at = "HEAD"
	}
	_, errb, ok := gitCmd("-C", top, "worktree", "add", "--detach", dest, at)
	if !ok {
		lines := strings.Split(strings.TrimSpace(pystr.DecodeReplace(errb)), "\n")
		last := ""
		if len(lines) > 0 && lines[0] != "" {
			last = lines[len(lines)-1]
		}
		return "", fmt.Errorf("the worktree for %s could not be made: %s", session, last)
	}
	_ = os.Chmod(dest, 0o700)
	return dest, nil
}

// branchRef is tree_branch.branch_ref: the base commit and repository the
// parent recorded for a branch.
func branchRef(dataDir, session string) (string, string, bool) {
	n := tree.ReadTree(dataDir).Nodes[session]
	if n == nil || n.Base == nil || n.Repo == nil || !branchgit.Usable(*n.Base, *n.Repo) {
		return "", "", false
	}
	return *n.Base, *n.Repo, true
}

// RemoveWorktree is tree_branch.remove_worktree. It never fails. repo is the
// git directory the parent recorded; the worktree's own .git pointer is
// never asked, because the child can rewrite it.
func RemoveWorktree(path, repo string) {
	if repo != "" {
		gitCmd("--git-dir="+repo, "worktree", "remove", "--force", path)
		gitCmd("--git-dir="+repo, "worktree", "prune")
	}
	_ = os.RemoveAll(path)
}

// DiscardWorktree is tree_branch.discard_worktree: it removes a branch
// worktree whose fold failed, and its admin directory in repo whatever the
// child did to it. The parent takes its rights back and drops a lock first,
// then asks git, then removes what is left of <repo>/worktrees/<name>
// itself. It never fails.
func DiscardWorktree(path, repo string) {
	admin := filepath.Join(repo, "worktrees", filepath.Base(path))
	branchgit.Reclaim(admin)
	branchgit.Drop(filepath.Join(admin, "locked"))
	RemoveWorktree(path, repo)
	branchgit.Reclaim(admin)
	branchgit.Drop(admin)
	gitCmd("--git-dir="+repo, "worktree", "prune")
}

// foldBranch is branchgit.Fold; a test swaps it to make a fold fail.
var foldBranch = branchgit.Fold

// WeakIPC is the fence flag a branch run refuses.
const WeakIPC = "--allow-weak-ipc"

// WeakIPCText is tree_branch.WEAK_IPC_TEXT.
const WeakIPCText = "A branch run cannot take --allow-weak-ipc: without the socket fence a child can ask " +
	"the session bus to start a process the run cannot stop. Remove the flag, or run the " +
	"branches without a fence. Nothing was started."

// NoFence is the fence flag that confines nothing; a branch run refuses it (R5-2).
const NoFence = "--no-fence"

// NoFenceText is tree_branch.NO_FENCE_TEXT.
const NoFenceText = "A branch fence cannot take --no-fence: the run would treat the branches as fenced while " +
	"nothing confines them. Remove the flag, or run the branches without a fence. " +
	"Nothing was started."

// ReftableText is tree_branch.reftable_text.
func ReftableText(repo, format string) string {
	return repo + " keeps its refs in the " + format + " format, and a fenced branch run can fold a " +
		"child's refs only in the files format. Run the branches without a fence, or convert " +
		"the repository with git refs migrate --ref-format=files. Nothing was started."
}

func rmdirIfEmpty(p string) { _ = os.Remove(p) }

func branchDiff(worktree, base, repo string) []byte {
	out, _ := branchgit.Diff(worktree, base, repo, nil, true, 0)
	return out
}

// BranchHash is tree_branch.branch_hash (W3K-10).
func BranchHash(worktree, base, repo, answer string) string {
	h := sha256.New()
	h.Write(branchDiff(worktree, base, repo))
	h.Write([]byte{0})
	h.Write([]byte(answer))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func diffLines(worktree, base, repo string) int {
	n := 0
	for _, line := range strings.Split(pystr.DecodeReplace(branchDiff(worktree, base, repo)), "\n") {
		// str.splitlines also splits on other separators; the diff text has none.
		if (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) && !strings.HasPrefix(line, "+++") && !strings.HasPrefix(line, "---") {
			n++
		}
	}
	return n
}

var scrubKeep = map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TMPDIR": true, "TERM": true}

// Scrubbed is tree_branch.scrubbed: the environment a check runs in.
func Scrubbed(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if scrubKeep[name] && !agentprof.IsKeyName(name) {
			out = append(out, kv)
		}
	}
	return out
}

// Check is one hard check of a branch.
type Check struct {
	Name     string
	Argv     []string
	Required bool
}

func runChecks(checks []Check, worktree string, env []string, timeoutS int, ran bool) []*pyjson.Object {
	var out []*pyjson.Object
	for _, c := range checks {
		res := "not_run"
		if ran {
			res = "fail"
			if len(c.Argv) > 0 {
				cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
				cmd.Dir = worktree
				cmd.Env = env
				if err := cmd.Start(); err == nil {
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					select {
					case err := <-done:
						if err == nil {
							res = "pass"
						}
					case <-time.After(time.Duration(timeoutS) * time.Second):
						_ = cmd.Process.Kill()
						<-done
					}
				}
			}
		}
		out = append(out, pyjson.NewObject().Set("name", c.Name).Set("required", c.Required).Set("result", res))
	}
	return out
}

// BranchOpts are run_branches's keywords.
type BranchOpts struct {
	Parent                            string
	Envelope                          *pyjson.Object
	Profiles                          agentprof.Profiles
	ProfileName                       *string
	Model                             string
	Tokens, Turns                     *big.Int
	ParentTokensUsed, ParentTurnsUsed *big.Int
	Task                              string
	Tools                             []string
	ToolsRepr                         string
	MaxTurns                          *big.Int
	Cwd                               string
	TimeoutS                          int
	// StepCapS is the cap of each bash call and of each check; 0 is none.
	StepCapS  int
	TimeoutMs int
	Attempts  int
	Why       string
	Parallel  int // 0 for the default
	Checks    []Check
	Judge     *string
	// JudgeFn runs the judge loop over a fit (rank.JudgeOnce in the CLI).
	// nil when no judge was asked for.
	JudgeFn func(doc *pyjson.Object, dataDir string, res *pyjson.Object) (*pyjson.Object, error)
	// Start is the child starter; tests swap it.
	Start func(dataDir, wardenRoot string, o NodeOpts) (*NodeRun, error)
}

// BranchRun is tree_branch.BranchRun.
type BranchRun struct {
	Status        string // ran, failed, refused or asked
	LeaderSession *string
	Answer        *string
	ChoiceID      *string
	Ranking       *pyjson.Object
	Branches      []*pyjson.Object
	Reasons       []string
	RankingID     *string
	AskID         *string
	Usage         bool // the fence is wrong for a branch: exit 2
}

// Doc is BranchRun.doc.
func (b *BranchRun) Doc() *pyjson.Object {
	opt := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	reasons := []any{}
	for _, r := range b.Reasons {
		reasons = append(reasons, r)
	}
	branches := []any{}
	for _, x := range b.Branches {
		branches = append(branches, x)
	}
	var ranking any
	if b.Ranking != nil {
		ranking = b.Ranking
	}
	return pyjson.NewObject().Set("status", b.Status).Set("session", opt(b.LeaderSession)).Set("answer", opt(b.Answer)).
		Set("reasons", reasons).Set("branches", branches).Set("leader", opt(b.LeaderSession)).
		Set("choice_id", opt(b.ChoiceID)).Set("ranking", ranking).Set("ranking_id", opt(b.RankingID)).
		Set("ask_id", opt(b.AskID))
}

// KillGraceS is how long the sweep waits after SIGTERM before SIGKILL, and
// SweepS how long it may take after that to stop what the children left.
const (
	KillGraceS = 2 * time.Second
	SweepS     = 5 * time.Second
)

// SweepText is the refusal when a process a child left will not stop.
const SweepText = "a process a branch child left could not be stopped; nothing of the branches was read"

func refuse(reasons ...string) *BranchRun { return &BranchRun{Status: "refused", Reasons: reasons} }

// RunBranches is tree_branch.run_branches. starter runs a child (a
// Starter's RunNode by default).
func (s *Starter) RunBranches(dataDir, wardenRoot string, o BranchOpts) (*BranchRun, error) {
	start := o.Start
	if start == nil {
		start = s.RunNode
	}
	parallel := o.Parallel
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	if parallel > o.Attempts {
		parallel = o.Attempts
	}
	if parallel < 1 {
		parallel = 1
	}
	top, err := gitTop(o.Cwd)
	var base, repo string
	if err == nil {
		base, repo, err = repoOf(top)
	}
	if err != nil {
		return refuse(err.Error()), nil
	}
	// The sessions, minted in order from the parent's proposals so far.
	t := tree.ReadTree(dataDir)
	k := t.Proposals[o.Parent]
	var sessions []string
	for len(sessions) < o.Attempts {
		sid := tree.MintSession(o.Parent, k)
		k++
		if t.Nodes[sid] != nil || tree.EnvelopeExists(wardenRoot, sid) {
			continue
		}
		sessions = append(sessions, sid)
	}
	if s.Fence != nil {
		words := s.Fence[1:]
		for _, w := range words {
			if w == WeakIPC || strings.HasPrefix(w, WeakIPC+"=") {
				r := refuse(WeakIPCText)
				r.Usage = true
				return r, nil
			}
		}
		for _, w := range words {
			if w == NoFence || strings.HasPrefix(w, NoFence+"=") {
				r := refuse(NoFenceText)
				r.Usage = true
				return r, nil
			}
		}
		if f := branchgit.RefFormat(repo); f != "files" {
			r := refuse(ReftableText(repo, f))
			r.Usage = true
			return r, nil
		}
		envFile := filepath.Join(wardenRoot, "envelopes", "default.json")
		for i, w := range words {
			if w == "--envelope" && i+1 < len(words) {
				envFile = words[i+1]
			} else if strings.HasPrefix(w, "--envelope=") {
				envFile = strings.TrimPrefix(w, "--envelope=")
			}
		}
		home := ""
		for _, kv := range s.Environ {
			if strings.HasPrefix(kv, "HOME=") {
				home = strings.TrimPrefix(kv, "HOME=")
			}
		}
		// The fence reads a relative grant against its workspace, which for a
		// branch child is the child's worktree.
		for _, sid := range sessions {
			wt := filepath.Join(BranchesDir(dataDir), sid)
			if grant := FenceCovers(words, envFile, repo, home, wt); grant != "" {
				r := refuse("A branch child needs the repository's git directory read only, but the " +
					"fence also grants " + grant + " read and write, which covers " + repo + ". Nothing was started.")
				r.Usage = true
				return r, nil
			}
		}
	}
	ss := make([]any, len(sessions))
	for i, sid := range sessions {
		ss[i] = sid
	}
	sum := sha256.Sum256([]byte(pyjson.CanonicalASCII([]any{o.Parent, o.Task, ss})))
	rankingID := "br_" + hex.EncodeToString(sum[:])[:12]
	var made []string
	undoWorktrees := func() {
		for _, w := range made {
			RemoveWorktree(w, repo)
		}
		rmdirIfEmpty(BranchesDir(dataDir))
	}
	for _, sid := range sessions {
		w, merr := MakeWorktree(top, dataDir, sid, base)
		if merr != nil {
			undoWorktrees()
			return refuse(merr.Error()), nil
		}
		made = append(made, w)
	}
	var prof *string
	if o.ProfileName != nil && *o.ProfileName != agentprof.Default {
		prof = o.ProfileName
	}
	model := o.Model
	sp, err := tree.SpawnBranches(dataDir, wardenRoot, o.Envelope, o.Parent, sessions, o.Tokens, o.Turns, o.TimeoutMs,
		tree.SpawnOpts{Profile: prof, Model: optStr(model), ParentTokensUsed: o.ParentTokensUsed,
			ParentTurnsUsed: o.ParentTurnsUsed, BranchOf: &rankingID, Why: &o.Why, Base: &base, Repo: &repo})
	if err != nil {
		undoWorktrees()
		return nil, err
	}
	if sp.Status != "started" {
		undoWorktrees()
		out := refuse(sp.Reasons...)
		if sp.Status == "asked" {
			out.Status = "asked"
			id := sp.AskID
			out.AskID = &id
		}
		return out, nil
	}

	type result struct {
		run    *NodeRun
		wallMs int
	}
	results := make([]result, len(sessions))
	sw := newSweep()
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			sid := sessions[i]
			var nr *NodeRun
			func() {
				defer func() {
					if r := recover(); r != nil {
						nr = &NodeRun{Status: "failed", Session: &sid, Reasons: []string{fmt.Sprint(r)}}
					}
				}()
				var e error
				nr, e = start(dataDir, wardenRoot, NodeOpts{Parent: o.Parent, Session: &sid, Envelope: o.Envelope,
					Profiles: o.Profiles, ProfileName: o.ProfileName, Model: o.Model, Tokens: o.Tokens, Turns: o.Turns,
					ParentTokensUsed: o.ParentTokensUsed, ParentTurnsUsed: o.ParentTurnsUsed, Task: o.Task,
					Tools: o.Tools, ToolsRepr: o.ToolsRepr, MaxTurns: o.MaxTurns, Cwd: made[i], TimeoutS: o.TimeoutS, StepCapS: o.StepCapS,
					DefaultModel: "haiku", TimeoutMs: o.TimeoutMs, Reserved: true})
				if e != nil {
					nr = &NodeRun{Status: "failed", Session: &sid, Reasons: []string{e.Error()}}
				}
			}()
			results[i] = result{nr, int(time.Since(t0).Milliseconds())}
		}(i)
	}
	wg.Wait()
	swept := sw.clear()

	// End every branch, in branch order, with its own use (W3K-13).
	for i, sid := range sessions {
		tree.EndSubtree(dataDir, wardenRoot, sid, false)
		_, _ = tree.End(dataDir, wardenRoot, sid, results[i].run.TokensUsed, results[i].run.TurnsUsed)
	}
	if !swept {
		return refuse(SweepText), nil
	}
	// A fenced child wrote its objects to a store of its own. Fold it away
	// before any read. In a fenced run every worktree is folded, whatever the
	// child left: it may have deleted its store, or taken the parent's rights
	// on its admin directory. A branch that does not fold is out, and its
	// worktree is removed, so the repository names nothing of the child's.
	folded := make([]bool, len(made))
	for i, wt := range made {
		ok := false
		if _, err := os.Lstat(branchgit.Store(repo, wt)); s.Fence == nil && err != nil {
			ok = true
		} else {
			ok = foldBranch(repo, wt, base)
		}
		if !ok {
			DiscardWorktree(wt, repo)
		}
		folded[i] = ok
	}

	env := Scrubbed(s.Environ)
	author := model
	if author == "" {
		if p, _ := o.Profiles.Lookup(o.ProfileName); p != nil && p.Model != nil {
			author = *p.Model
		}
	}
	if author == "" {
		author = "haiku"
	}
	var atts, branches []any
	answers := map[string]string{}
	for i, sid := range sessions {
		nr := results[i].run
		ran := (nr.Status == "started" || nr.Status == "ran") && folded[i]
		answer := ""
		if nr.Answer != nil {
			answer = *nr.Answer
		}
		answers[sid] = answer
		wt := made[i]
		result := "fail"
		if ran {
			result = "pass"
		}
		tests := []*pyjson.Object{pyjson.NewObject().Set("name", "the child ran").Set("required", true).Set("result", result)}
		tests = append(tests, runChecks(o.Checks, wt, env, o.StepCapS, ran)...)
		cost := pyjson.NewObject().Set("wall_ms", pyjson.Int{Text: fmt.Sprint(results[i].wallMs)}).
			Set("diff_lines", pyjson.Int{Text: fmt.Sprint(diffLines(wt, base, repo))})
		if nr.TokensUsed != nil {
			cost.Set("tokens", pyjson.Int{Text: nr.TokensUsed.String()})
		}
		first := ""
		if tr := pystr.Strip(answer); tr != "" {
			first = pystr.Slice(strings.SplitN(strings.ReplaceAll(strings.ReplaceAll(tr, "\r\n", "\n"), "\r", "\n"), "\n", 2)[0], 0, 80)
		}
		testList := make([]any, len(tests))
		for j, tt := range tests {
			testList[j] = tt
		}
		att := pyjson.NewObject().Set("id", sid).Set("content_hash", BranchHash(wt, base, repo, answer)).Set("author", author).
			Set("edge_proof", "ok").Set("started_by_parent", true).Set("tests", testList).Set("cost", cost).
			Set("where", pyjson.NewObject().Set("kind", "worktree").Set("path", wt).Set("base", base).Set("repo", repo))
		if first != "" {
			att.Set("previews", pyjson.NewObject().Set("summary", first))
		}
		atts = append(atts, att)
		var tu, tn any
		if nr.TokensUsed != nil {
			tu = pyjson.Int{Text: nr.TokensUsed.String()}
		}
		if nr.TurnsUsed != nil {
			tn = pyjson.Int{Text: nr.TurnsUsed.String()}
		}
		status := "failed"
		if ran {
			status = "ran"
		}
		branches = append(branches, pyjson.NewObject().Set("session", sid).Set("status", status).Set("worktree", wt).
			Set("tokens_used", tu).Set("turns_used", tn).Set("tests", testList))
	}
	doc := pyjson.NewObject().Set("ranking_id", rankingID).Set("task", o.Task).Set("project", top).Set("attempts", atts)
	if o.Judge != nil {
		doc.Set("policy", pyjson.NewObject().Set("judges", []any{*o.Judge}))
	}
	r, perr := rank.Parse(doc)
	if perr != nil {
		return nil, perr
	}
	res := rank.FitDoc(r, dataDir)
	if o.Judge != nil && o.JudgeFn != nil {
		res, err = o.JudgeFn(doc, dataDir, res)
		if err != nil {
			return nil, err
		}
	}
	rawBranches := make([]*pyjson.Object, len(branches))
	for i, b := range branches {
		rawBranches[i] = b.(*pyjson.Object)
	}
	if res.Value("status") == "none_survived" {
		var reasons []string
		for _, e := range res.Value("eliminated").([]any) {
			eo := e.(*pyjson.Object)
			var rs []string
			for _, x := range eo.Value("reasons").([]any) {
				rs = append(rs, x.(string))
			}
			reasons = append(reasons, fmt.Sprintf("%s: %s", eo.Value("id"), strings.Join(rs, "; ")))
		}
		return &BranchRun{Status: "failed", Branches: rawBranches, Ranking: res, RankingID: &rankingID, Reasons: reasons}, nil
	}
	var choiceID *string
	if len(res.Value("order").([]any)) > 1 {
		leader := res.Value("leader").(string)
		row := rank.OpenedRow(r, res, rank.Now(), pyjson.NewObject().Set("run_id", rankingID).Set("downstream", []any{}),
			"daisugi tree adopt "+leader)
		id := row.Value("choice_id").(string)
		choiceID = &id
		known := false
		for _, c := range rank.ReadCards(dataDir) {
			known = known || c.ID() == id
		}
		if !known {
			if err := rank.AppendRows(dataDir, []*pyjson.Object{row}); err != nil {
				return nil, err
			}
		}
	}
	leader := res.Value("leader").(string)
	ans := answers[leader]
	return &BranchRun{Status: "ran", LeaderSession: &leader, Answer: &ans, ChoiceID: choiceID, Ranking: res,
		Branches: rawBranches, RankingID: &rankingID}, nil
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PruneBranches is tree_branch.prune_branches: remove the worktrees of
// branches whose card is closed, and those of ended branches that have no
// card once they are older than the card decay (14 days). A running branch
// and a branch of an open card stay, and so does a lone survivor that no
// card holds: `tree adopt` is the only way its edits reach the parent.
func PruneBranches(dataDir string) []string { return PruneBranchesAt(dataDir, rank.Now()) }

// PruneBranchesAt is PruneBranches at a given time.
func PruneBranchesAt(dataDir string, now float64) []string {
	base := BranchesDir(dataDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	t := tree.ReadTree(dataDir)
	cards := map[string]*rank.Card{}
	for _, c := range rank.ReadCards(dataDir) {
		if rid, ok := c.Opened.Value("ranking_id").(string); ok {
			cards[rid] = c
		}
	}
	removed := []string{}
	for _, d := range entries {
		node := t.Nodes[d.Name()]
		if node == nil || node.State != "ended" {
			continue
		}
		rid := ""
		if node.BranchOf != nil {
			rid = *node.BranchOf
		}
		card := cards[rid]
		if card == nil {
			if !(node.EndedAt != 0 && now-node.EndedAt > rank.DecaySeconds) {
				continue
			}
		} else if card.Close == nil {
			continue
		}
		repo := ""
		if node.Repo != nil {
			repo = *node.Repo
		}
		// A child may have taken the parent's rights on its admin dir, and a
		// sweep refusal skipped the fold: take them back first (R5-1).
		if repo != "" {
			DiscardWorktree(filepath.Join(base, d.Name()), repo)
		} else {
			RemoveWorktree(filepath.Join(base, d.Name()), repo)
		}
		removed = append(removed, d.Name())
	}
	rmdirIfEmpty(base)
	return removed
}

// AdoptResult is tree_branch.Adopt.
type AdoptResult struct {
	Status  string // adopted, nothing or refused
	Session string
	Files   []string
	Why     string
}

// Doc is Adopt.doc.
func (a *AdoptResult) Doc() *pyjson.Object {
	files := make([]any, len(a.Files))
	for i, f := range a.Files {
		files[i] = f
	}
	return pyjson.NewObject().Set("status", a.Status).Set("session", a.Session).Set("files", files).Set("why", a.Why)
}

func gitIn(env []string, input []byte, argv ...string) ([]byte, []byte, bool) {
	cmd := exec.Command("git", branchgit.Guarded(argv...)...)
	cmd.Env = env
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err == nil
}

// Adopt is tree_branch.adopt: apply a branch's edits to the parent's
// working tree with git apply --3way, or change nothing.
func Adopt(dataDir, session, cwd string) *AdoptResult {
	wt := filepath.Join(BranchesDir(dataDir), session)
	if !tree.ValidSession(session) {
		return &AdoptResult{Status: "refused", Session: session, Why: "No branch worktree for " + session + "."}
	}
	if st, err := os.Stat(wt); err != nil || !st.IsDir() {
		return &AdoptResult{Status: "refused", Session: session, Why: "No branch worktree for " + session + "."}
	}
	base, repo, ok := branchRef(dataDir, session)
	if !ok {
		return &AdoptResult{Status: "refused", Session: session,
			Why: "The ledger has no recorded base for " + session + "; not adopted."}
	}
	if !branchgit.Owns(wt, repo) {
		return &AdoptResult{Status: "refused", Session: session, Why: branchgit.Lost(session, repo) + "; nothing was read."}
	}
	if cwd == "" {
		cwd = filepath.Dir(repo)
	} else if !branchgit.Belongs(cwd, repo) {
		return &AdoptResult{Status: "refused", Session: session, Why: cwd + " is not a work tree of " + repo + "."}
	}
	topOut, _, ok := gitCmd("-C", cwd, "rev-parse", "--show-toplevel")
	if !ok {
		return &AdoptResult{Status: "refused", Session: session, Why: cwd + " is not a git work tree."}
	}
	top := strings.TrimSpace(pystr.DecodeReplace(topOut))
	branchgit.Intent(wt)
	patch, _ := branchgit.Diff(wt, base, repo, []string{"--no-renames"}, false, 0)
	names, _ := branchgit.Diff(wt, base, repo, []string{"--name-only", "--no-renames", "-z"}, false, 0)
	var files []string
	for _, f := range strings.Split(pystr.DecodeReplace(names), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	sortStrings(files)
	if len(patch) == 0 || len(files) == 0 {
		return &AdoptResult{Status: "nothing", Session: session, Why: session + " changed no file."}
	}
	st, _, _ := gitCmd(append([]string{"-C", top, "status", "--porcelain", "-z", "--"}, files...)...)
	dirty := map[string]bool{}
	for _, e := range strings.Split(pystr.DecodeReplace(st), "\x00") {
		if len(e) > 3 {
			dirty[e[3:]] = true
		}
	}
	if len(dirty) > 0 {
		var ds []string
		for d := range dirty {
			ds = append(ds, d)
		}
		sortStrings(ds)
		return &AdoptResult{Status: "refused", Session: session, Files: files,
			Why: fmt.Sprintf("Refused: %s has uncommitted changes in %s; commit or stash first.", ds[0], top)}
	}
	tr, _, _ := gitCmd(append([]string{"-C", top, "ls-tree", "-r", "--name-only", "-z", "HEAD", "--"}, files...)...)
	tracked := map[string]bool{}
	for _, f := range strings.Split(pystr.DecodeReplace(tr), "\x00") {
		tracked[f] = true
	}
	_, errb, ok := gitIn(nil, patch, "-C", top, "apply", "--3way", "--whitespace=nowarn", "-")
	if !ok {
		gitCmd(append([]string{"-C", top, "reset", "-q", "--"}, files...)...)
		for _, f := range files {
			if tracked[f] {
				gitCmd("-C", top, "checkout", "HEAD", "--", f)
			} else {
				_ = os.Remove(filepath.Join(top, f))
			}
		}
		lines := strings.Split(strings.TrimSpace(pystr.DecodeReplace(errb)), "\n")
		why := fmt.Sprintf("Refused: the edits of %s do not apply cleanly to %s", session, top)
		if len(lines) > 0 && lines[0] != "" {
			why += " (" + lines[len(lines)-1] + ")."
		} else {
			why += "."
		}
		return &AdoptResult{Status: "refused", Session: session, Files: files, Why: why}
	}
	return &AdoptResult{Status: "adopted", Session: session, Files: files,
		Why: fmt.Sprintf("Applied %d files to %s.", len(files), top)}
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
