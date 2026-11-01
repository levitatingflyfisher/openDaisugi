package server

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/worktree"

	"daisugi-verify/pando"
)

// Round 8b: Pando branches. A long-running agent the steward starts runs in
// a pane of its own with DAISUGI_PANDO_BRANCH in its env (and no skip
// marker). Its chat goes into its branch of the steward's store, never into
// the store above, and it sees its branch and below. The variable rides on
// the layout record, so a resume, a restore and a fork keep it.

// paneBranch is the branch a pane carries and whether it is a short worker
// of that branch (it carries the skip marker too).
func paneBranch(rec layout.Pane) (branch string, worker bool) {
	get := func(k string) string { return rec.Env[k] }
	return strings.TrimSpace(get(pando.BranchVar)), pando.SkipOn(get)
}

// callerBranch is the branch of the pane that sent a request, "" for none.
func (s *Server) callerBranch(ro role) (branch string, worker bool) {
	if !ro.pane || ro.paneID == "" {
		return "", false
	}
	rec, ok := s.tree.Pane(ro.paneID)
	if !ok {
		return "", false
	}
	return paneBranch(rec)
}

// branchStoreDir is the store the branches of this server live in: the
// steward's store.
func (s *Server) branchStoreDir() string { return s.stewardStoreDir() }

// leadEnv is the env of a long-running agent's pane: the env asked for
// with the branch set and no skip marker, whatever the caller passed.
func leadEnv(env map[string]string, branch, repo string, objects []string) map[string]string {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	delete(out, pando.SkipVar)
	delete(out, pando.SkipListVar)
	delete(out, LeadObjectsVar)
	delete(out, LeadRepoVar)
	out[pando.BranchVar] = branch
	if repo != "" {
		out[LeadRepoVar] = repo
	}
	if len(objects) > 0 {
		out[LeadObjectsVar] = strings.Join(objects, string(os.PathListSeparator))
	}
	return out
}

// LeadRepoVar names, on a lead pane's record, the owner's repo its clone
// was made from (review fix 5, K1). A nested lead clones this repo, never
// the parent's clone, so no lead-written tree is ever asked for a path to
// grant. coppice writes it; a caller's own value is always dropped.
const LeadRepoVar = "COPPICE_LEAD_REPO"

// LeadObjectsVar names, on a lead pane's record, the object stores its
// clone borrows: its repo's (named by git on the owner's side, so a linked
// worktree project resolves to its common dir) and, for a lead's nested
// lead, its parent clone's and its parent's list (review fix 4, J1). The
// fence grants each read only. coppice writes it; a caller's own value is
// always dropped, and the clone's own alternates file, which the lead can
// write, is never read.
const LeadObjectsVar = "COPPICE_LEAD_OBJECTS"

// leadObjectsDirs are the object stores a fenced lead pane reads. A store
// that is not an absolute real dir, or goes through a link, refuses the
// pane: it is never dropped in silence (K1).
func leadObjectsDirs(rec layout.Pane) ([]string, error) {
	var out []string
	for _, d := range filepath.SplitList(rec.Env[LeadObjectsVar]) {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("the lead's object store %q is not an absolute path; the pane does not start", d)
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("the lead's object store %s is not there; the pane does not start", d)
		}
		if r, err := filepath.EvalSymlinks(d); err != nil || r != filepath.Clean(d) {
			return nil, fmt.Errorf("the lead's object store %s goes through a symbolic link; the pane does not start", d)
		}
		out = append(out, filepath.Clean(d))
	}
	return out, nil
}

// branchClaudeArgv is a long-running agent's claude argv with a minted
// --session-id after the program, and that id. The id is on the store's
// session map before the process runs, so the follow service logs even the
// first line of the transcript into the branch. As with listedClaudeArgv,
// the saved argv never holds the flag, and a resume or a fork is left
// alone (its id is mapped once coppice learns it: routePaneSession).
func (s *Server) branchClaudeArgv(rec layout.Pane) (argv []string, sid string) {
	branch, worker := paneBranch(rec)
	if branch == "" || worker || rec.Kind != layout.KindPTY || rec.Harness != "claude" || len(rec.Argv) == 0 {
		return rec.Argv, ""
	}
	for _, a := range rec.Argv[1:] {
		if slices.Contains([]string{"--session-id", "--resume", "-r", "--continue", "-c", "--fork-session"}, a) {
			return rec.Argv, ""
		}
	}
	// A pane whose dir has a route to its branch needs no id line: the dir
	// route sends every session there to the branch (H2).
	if s.dirRouted(rec, branch) {
		return rec.Argv, ""
	}
	id, err := pando.NewBranchSession(s.branchStoreDir(), branch)
	if err != nil {
		return rec.Argv, ""
	}
	return append([]string{rec.Argv[0], "--session-id", id}, rec.Argv[1:]...), id
}

// dirRouted says whether the pane's cwd has a dir route to its branch.
func (s *Server) dirRouted(rec layout.Pane, branch string) bool {
	return rec.Cwd != "" && pando.LoadRoutes(s.branchStoreDir()).ForCwd(rec.Cwd) == branch
}

// routePaneSession puts a session coppice learned for a branch pane (a
// resumed or forked one) on the store's session map. The id comes from the
// pane, so it is taken only when it is the pane's own (review fix 3, H2):
// a pane with a dir route writes no id line at all, and any other pane only
// an id whose transcript sits in the pane's own Claude project dir. An id
// the lead read elsewhere (the owner's last session in ~/.claude.json, say)
// is never mapped.
func (s *Server) routePaneSession(rec layout.Pane, sid string) {
	branch, worker := paneBranch(rec)
	if branch == "" || worker || sid == "" || s.dirRouted(rec, branch) {
		return
	}
	if !ownClaudeTranscript(rec.Cwd, sid) {
		return
	}
	_ = pando.AddRoute(s.branchStoreDir(), branch, sid)
}

// claudeProjectDir is Claude Code's dir for cwd under ~/.claude/projects:
// every character but an ASCII letter or digit made "-"; "" when Claude
// would shorten it (over 200 bytes) or it is not ASCII.
func claudeProjectDir(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil || cwd == "" || len(cwd) > 200 {
		return ""
	}
	b := []byte(cwd)
	for i, c := range b {
		if c >= 0x80 {
			return ""
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return filepath.Join(home, ".claude", "projects", string(b))
}

// ownClaudeTranscript says whether sid's transcript is in cwd's own Claude
// project dir.
func ownClaudeTranscript(cwd, sid string) bool {
	d := claudeProjectDir(cwd)
	if d == "" || strings.ContainsAny(sid, "/\\") {
		return false
	}
	st, err := os.Lstat(filepath.Join(d, sid+".jsonl"))
	return err == nil && st.Mode().IsRegular()
}

// paneBranchDir is the dir a fenced pane of a branch may read: its branch
// and everything below it, read only. "" when the pane has no branch or
// the branch is not there.
func (s *Server) paneBranchDir(rec layout.Pane) string {
	branch, _ := paneBranch(rec)
	if branch == "" {
		return ""
	}
	d, err := pando.ExistingBranch(s.branchStoreDir(), branch)
	if err != nil {
		return ""
	}
	return d
}

// LongRunHarnessRefusal is what pane.create with long_running gets for a
// harness other than claude.
const LongRunHarnessRefusal = "a long-running agent starts on claude: pane.create with long_running takes harness claude, " +
	"because only a Claude session can be put on the branch's session map before it starts."

// LongRunRefusal is what a pane gets that asks for a long-running agent
// below a branch it does not lead.
const LongRunRefusal = "only a long-running agent starts a long-running agent below it: the new branch goes under the caller's own branch."

// agentPaneEnv is the env of a pane an agent asks for. A long-running agent
// that asks for another (long_running with pando_branch SUB) gets a lead of
// a branch below its own. Any other pane an agent asks for is a worker: it
// carries the skip marker, and the caller's branch, so a worker of a lead
// searches that lead's branch and nothing above it.
func (s *Server) agentPaneEnv(ro role, r *proto.Request, env map[string]string, cwd string) (map[string]string, string, error) {
	parent, parentWorker := s.callerBranch(ro)
	long, _ := r.Bool("long_running")
	sub, _ := r.Str("pando_branch")
	if !long {
		if sub != "" {
			return nil, "", errors.New("pando_branch goes with long_running: a worker always works in its caller's branch")
		}
		out := s.pandoAgentEnv(env)
		delete(out, LeadObjectsVar)
		delete(out, LeadRepoVar)
		if parent != "" {
			out[pando.BranchVar] = parent
		} else {
			delete(out, pando.BranchVar)
		}
		return out, cwd, nil
	}
	if parent == "" || parentWorker {
		return nil, "", errors.New(LongRunRefusal)
	}
	// Only a Claude pane's chat can be routed: coppice mints and maps its
	// session id before it starts (F4).
	if h, _ := r.Str("harness"); h != "claude" {
		return nil, "", errors.New(LongRunHarnessRefusal)
	}
	if sub == "" {
		return nil, "", errors.New("long_running needs pando_branch, the label of the new branch below yours")
	}
	path := pando.JoinBranch(parent, sub)
	if _, err := pando.ParseBranch(path); err != nil {
		return nil, "", err
	}
	// A nested lead gets its own clone and dir route, as the steward's
	// leads do (G6). Its repo is the parent's recorded repo, the owner's,
	// never the caller's cwd. Nothing of the parent's clone is read at all
	// (K1, L1), which is why J6 is closed for nested leads: the nested clone
	// starts from origin/lead-<parent> when the owner has fetched the
	// parent's branch into the repo already, else from the repo's branch.
	// A parent with no recorded repo (a lead outside git) gives a nested
	// lead in the parent's own dir, with no clone and no grant.
	prec, ok := s.tree.Pane(ro.paneID)
	if !ok {
		return nil, "", errors.New(LongRunRefusal)
	}
	repo := prec.Env[LeadRepoVar]
	wt := prec.Cwd
	var objects []string
	if repo != "" {
		var err error
		if wt, objects, _, err = s.leadClone(repo, path, leadWorktreeName(parent)); err != nil {
			return nil, "", err
		}
	}
	if _, err := pando.ReopenBranch(s.branchStoreDir(), path, s.clock); err != nil {
		return nil, "", err
	}
	return leadEnv(env, path, repo, objects), wt, nil
}

// spawnLead starts a long-running agent for the steward: a claude pane in
// the project with the brief as its first prompt and the branch in its env.
func (s *Server) spawnLead(dir, branch, brief string) (string, error) {
	if s.isClosed() {
		return "", errors.New("the server is closing")
	}
	argv, ok := harnessArgv("claude", []string{brief})
	if !ok {
		return "", errors.New("a long-running agent starts on claude, and coppice.toml names no claude harness")
	}
	// A lead runs in a worktree of its own, so its Claude project dir is
	// not the owner's (F1), and a dir route sends every session there, a
	// `claude -p` or a model call of the lead's included, to its branch
	// (F3). A project that is not a git repo runs the lead in the project
	// itself, with no dir route, and the answer says so.
	cwd, repo, objects, where, err := s.leadWorktree(dir, branch)
	if err != nil {
		return "", err
	}
	// Only a lead that will start opens a closed branch again (H6).
	if _, err := pando.ReopenBranch(s.branchStoreDir(), branch, s.clock); err != nil {
		return "", err
	}
	labelBase := branch
	if i := strings.LastIndexByte(branch, '/'); i >= 0 {
		labelBase = branch[i+1:]
	}
	label := cleanLabel(labelBase + "-lead")
	s.labelMu.Lock()
	for k := 2; s.liveLabelTaken(label); k++ {
		label = cleanLabel(fmt.Sprintf("%s-lead-%d", labelBase, k))
	}
	wsID, tabID, err := s.placeFor(&proto.Request{}, cwd)
	if err != nil {
		s.labelMu.Unlock()
		return "", err
	}
	rec, err := s.tree.CreatePane(wsID, tabID, layout.Pane{
		Label: label, Cwd: cwd, Argv: argv, Env: leadEnv(nil, branch, repo, objects), Kind: layout.KindPTY,
		Harness: "claude", Cols: 120, Rows: 40, ExtraArgs: []string{brief}, StartedAt: s.clock().Unix(),
	})
	s.labelMu.Unlock()
	if err != nil {
		return "", err
	}
	if err := s.startPane(rec); err != nil {
		_ = s.tree.RemovePane(rec.ID)
		s.removeLive(rec.ID)
		s.states.Forget(rec.ID)
		s.markLayoutDirty()
		return "", err
	}
	s.markLayoutDirty()
	return fmt.Sprintf("started a long-running agent in pane %s (%s), %s. Its chat is the branch %s: recall finds it, and zoom with branch=%q reads it.",
		rec.ID, label, where, branch, branch), nil
}

// leadWorktreeName is the worktree (and git branch) name of a lead: lead-
// and the branch path, a-z and 0-9 kept, every other character written as
// a dash and its two hex digits. So trellis/ui, trellis.ui and trellis-ui
// get three names (G3).
func leadWorktreeName(branch string) string {
	var b strings.Builder
	b.WriteString("lead-")
	for i := 0; i < len(branch); i++ {
		c := branch[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "-%02x", c)
		}
	}
	return b.String()
}

// leadWorktree is the dir a lead starts in and a phrase that says where.
// In a git repo it is the worktree lead-<branch> beside the repo, made
// once and used again, with a dir route to the branch; a worktree that
// cannot be made refuses the spawn, never the owner's dir in its place
// (G3). A project that is not a git repo runs the lead in the project, and
// the phrase says so.
func (s *Server) leadWorktree(dir, branch string) (cwd, repo string, objects []string, where string, err error) {
	top, terr := worktree.Toplevel(dir)
	if terr != nil {
		return dir, "", nil, "in " + dir + " (not a git repo: no clone of its own, so the owner's chats in this project share its Claude project dir)", nil
	}
	path, objects, where, err := s.leadClone(top, branch, "")
	return path, top, objects, where, err
}

// leadClone makes (or uses again) the lead's clone of the owner's repo and
// its dir route, and names the repo's object store owner-side. fromBranch
// is a nested lead's parent branch, used only when the owner has fetched
// it into the repo; "" for a lead of the steward.
func (s *Server) leadClone(repo, branch, fromBranch string) (cwd string, objects []string, where string, err error) {
	if !filepath.IsAbs(repo) {
		return "", nil, "", fmt.Errorf("the lead's repo %q is not an absolute path", repo)
	}
	// The repo's object store, named by git in the owner's repo: the clone
	// borrows it, and the fenced lead must read it (J1). A link refuses.
	obj, err := worktree.ObjectsDir(repo)
	if err != nil {
		return "", nil, "", fmt.Errorf("the object store of %s cannot be named, so the lead does not start: %w", repo, err)
	}
	name := leadWorktreeName(branch)
	path, err := worktree.EnsureCloneFrom(repo, name, fromBranch)
	if err != nil {
		return "", nil, "", fmt.Errorf("the lead's clone for branch %s cannot be made, so it does not start: %w", branch, err)
	}
	key := pando.DirRoutePrefix + path
	if have := pando.LoadRoutes(s.branchStoreDir())[key]; have != "" && have != branch {
		return "", nil, "", fmt.Errorf("the clone %s belongs to the branch %s, not %s", path, have, branch)
	}
	if err := pando.AddRoute(s.branchStoreDir(), branch, key); err != nil {
		return "", nil, "", fmt.Errorf("the dir route of the lead's clone cannot be written, so it does not start: %w", err)
	}
	// J4: name:name makes the branch in the owner's repo; a later clone of
	// this lead starts from it. K3: the owner runs it, in the repo.
	return path, []string{obj}, "in its own clone " + path + " (the owner brings its work over, in " + repo + ", with: git fetch " + path + " " + name + ":" + name +
		"; a lead it starts begins from that branch only after the owner has run that line, so ask the owner to run it first)", nil
}
