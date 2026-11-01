package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/pando"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/steward"
	"github.com/opendaisugi/coppice/internal/worktree"
)

func stewardReq(tool string, args map[string]any) steward.ToolRequest {
	return steward.ToolRequest{Tool: tool, Args: args}
}

// leadPane makes a claude pane the operator starts with a branch in its
// env, as spawnLead does, and returns its id.
func leadPane(t *testing.T, s *Server, prog, branch string) string {
	t.Helper()
	if _, err := pando.EnsureBranch(s.branchStoreDir(), branch, nil); err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","harness":"claude","cmd_argv":[%q,"--model","opus"],"env":{%q:%q}}`,
		t.TempDir(), prog, pando.BranchVar, branch)
	return result(t, roundTrip(t, s, req)[0])["pane"].(string)
}

// createFromPane sends pane.create as the pane id and returns the answer.
func createFromPane(t *testing.T, s *Server, as, prog, extra string) map[string]any {
	t.Helper()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"` + as + `"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("hello: %v", r)
	}
	send(fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","harness":"claude","cmd_argv":[%q]%s}`, t.TempDir(), prog, extra))
	return next()
}

// A long-running agent's claude starts with a session id that is on the
// store's session map before it runs, and keeps no skip marker.
func TestALeadPaneStartsWithAMappedSession(t *testing.T) {
	s := newEndedServer(t)
	prog, out := claudeStub(t)
	id := leadPane(t, s, prog, "trellis")
	f := strings.Fields(argLines(t, out, 1)[0])
	if len(f) < 2 || f[0] != "--session-id" {
		t.Fatalf("args = %v", f)
	}
	if got := pando.LoadRoutes(s.branchStoreDir()).For(f[1], ""); got != "trellis" {
		t.Fatalf("session %s maps to %q", f[1], got)
	}
	rec, _ := s.tree.Pane(id)
	if _, ok := rec.Env[pando.SkipVar]; ok || rec.Env[pando.BranchVar] != "trellis" {
		t.Fatalf("env = %v", rec.Env)
	}
	if len(listed(s)) != 0 {
		t.Fatalf("a lead's session is on the skip list: %v", listed(s))
	}
}

// A pane a lead asks for is a worker of the lead's branch, whatever branch
// the request names in its env; a long_running one is a lead one level down.
func TestAPaneALeadAsksForStaysInsideItsBranch(t *testing.T) {
	s := newEndedServer(t)
	prog, _ := claudeStub(t)
	lead := leadPane(t, s, prog, "trellis")
	r := createFromPane(t, s, lead, prog, fmt.Sprintf(`,"env":{%q:"other",%q:"0"}`, pando.BranchVar, pando.SkipVar))
	if r["ok"] != true {
		t.Fatalf("create: %v", r)
	}
	w, _ := s.tree.Pane(r["result"].(map[string]any)["pane"].(string))
	if w.Env[pando.BranchVar] != "trellis" || w.Env[pando.SkipVar] != "1" {
		t.Fatalf("worker env = %v", w.Env)
	}
	r = createFromPane(t, s, lead, prog, `,"long_running":true,"pando_branch":"ui"`)
	if r["ok"] != true {
		t.Fatalf("nested lead: %v", r)
	}
	n, _ := s.tree.Pane(r["result"].(map[string]any)["pane"].(string))
	if n.Env[pando.BranchVar] != "trellis/ui" || n.Env[pando.SkipVar] != "" {
		t.Fatalf("nested env = %v", n.Env)
	}
	if _, err := pando.ExistingBranch(s.branchStoreDir(), "trellis/ui"); err != nil {
		t.Fatal(err)
	}
	// A bad label, and an escape, are refused.
	for _, bad := range []string{"..", "../other", "/abs", "a/../b"} {
		r = createFromPane(t, s, lead, prog, fmt.Sprintf(`,"long_running":true,"pando_branch":%q`, bad))
		if r["ok"] == true {
			t.Errorf("pando_branch %q was taken", bad)
		}
	}
	// A worker of the branch may not start a lead, and nor may a root agent.
	r = createFromPane(t, s, w.ID, prog, `,"long_running":true,"pando_branch":"x"`)
	if r["ok"] == true {
		t.Fatalf("a worker started a lead: %v", r)
	}
	r = createFromPane(t, s, "w1:p99", prog, `,"long_running":true,"pando_branch":"x"`)
	if r["ok"] == true {
		t.Fatalf("an agent with no branch started a lead: %v", r)
	}
	// A plain agent pane with no branch stays a closed worker, with no branch.
	r = createFromPane(t, s, "w1:p99", prog, fmt.Sprintf(`,"env":{%q:"trellis"}`, pando.BranchVar))
	if r["ok"] != true {
		t.Fatalf("create: %v", r)
	}
	p, _ := s.tree.Pane(r["result"].(map[string]any)["pane"].(string))
	if _, ok := p.Env[pando.BranchVar]; ok || p.Env[pando.SkipVar] != "1" {
		t.Fatalf("root worker env = %v", p.Env)
	}
}

// A resume starts the pane from its record: the branch stays.
func TestAResumedLeadKeepsItsBranch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newEndedServer(t)
	prog, out := claudeStub(t)
	id := leadPane(t, s, prog, "trellis")
	waitDone(t, s, id)
	got := roundTrip(t, s, `{"id":"2","cmd":"pane.resume","pane":"`+id+`"}`)
	if !got[0].OK {
		t.Fatalf("pane.resume: %+v", got[0].Error)
	}
	newID := result(t, got[0])["pane"].(string)
	rec, _ := s.tree.Pane(newID)
	if rec.Env[pando.BranchVar] != "trellis" {
		t.Fatalf("resumed env = %v", rec.Env)
	}
	lines := argLines(t, out, 2)
	b := strings.Fields(lines[1])[1]
	if pando.LoadRoutes(s.branchStoreDir()).For(b, "") != "trellis" {
		t.Fatalf("the restarted session %s is not mapped", b)
	}
}

// A session coppice learns later for a live lead (a fork, a resume) is
// mapped too.
func TestALearnedSessionOfALeadIsMapped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newEndedServer(t)
	dir := t.TempDir()
	prog := filepath.Join(dir, "claude-sleep.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	id := leadPane(t, s, prog, "trellis")
	t.Cleanup(func() { roundTrip(t, s, `{"id":"9","cmd":"pane.close","pane":"`+id+`"}`) })
	rec, _ := s.tree.Pane(id)
	// H2: an id the pane reports whose transcript is not in its own Claude
	// project dir (the owner's last session, read from ~/.claude.json) is
	// never mapped.
	owner := "22222222-2222-4333-8444-555555555555"
	s.noteHarnessSession(id, "claude", owner)
	if pando.LoadRoutes(s.branchStoreDir()).For(owner, "") != "" {
		t.Fatal("a session that is not the pane's own was mapped")
	}
	// Its own transcript (a fork or a resume in its dir) is.
	sid := "11111111-2222-4333-8444-555555555555"
	pd := claudeProjectDir(rec.Cwd)
	os.MkdirAll(pd, 0o700)
	os.WriteFile(filepath.Join(pd, sid+".jsonl"), []byte("{}\n"), 0o600)
	s.noteHarnessSession(id, "claude", sid)
	if pando.LoadRoutes(s.branchStoreDir()).For(sid, "") != "trellis" {
		t.Fatal("a learned session is not mapped")
	}
	// A pane with a dir route writes no id line at all.
	if err := pando.AddRoute(s.branchStoreDir(), "trellis", pando.DirRoutePrefix+rec.Cwd); err != nil {
		t.Fatal(err)
	}
	sid2 := "33333333-2222-4333-8444-555555555555"
	os.WriteFile(filepath.Join(pd, sid2+".jsonl"), []byte("{}\n"), 0o600)
	s.noteHarnessSession(id, "claude", sid2)
	if pando.LoadRoutes(s.branchStoreDir()).For(sid2, "") != "" {
		t.Fatal("a dir-routed pane wrote an id line")
	}
}

// A fenced pane of a branch may read that branch, read only.
func TestAFencedLeadReadsItsBranchOnly(t *testing.T) {
	fakeFenceBin(t)
	s := newTestServer(t)
	armWarden(t, s)
	d, err := pando.EnsureBranch(s.branchStoreDir(), "trellis", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/work", Env: map[string]string{pando.BranchVar: "trellis"}}, s.fenceTmp("w1:p1"))
	if err != nil || !hasPair(w, "--allow", d) {
		t.Fatalf("wrap = %v, err %v", w, err)
	}
	for _, a := range w {
		if strings.HasSuffix(a, ":rw") && strings.Contains(a, "pando") {
			t.Fatalf("the branch is writable: %v", w)
		}
	}
	w, _ = s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/work"}, s.fenceTmp("w1:p2"))
	if strings.Contains(strings.Join(w, " "), "pando") {
		t.Fatalf("a pane with no branch reads the memory: %v", w)
	}
	// A marker that does not resolve gives no grant at all.
	w, _ = s.fenceWrap(layout.Pane{Harness: "claude", Cwd: "/work", Env: map[string]string{pando.BranchVar: "../work"}}, s.fenceTmp("w1:p3"))
	if strings.Contains(strings.Join(w, " "), "pando") {
		t.Fatalf("a bad marker got a grant: %v", w)
	}
}

// spawn with long_running starts a lead pane in the project.
func TestSpawnLongRunningStartsALeadPane(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prog, out := claudeStub(t)
	if err := config.Save(config.Config{Harness: map[string]config.Harness{"claude": {Command: prog}}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	proj := t.TempDir()
	msg, err := s.spawnLead(proj, "trellis", "build the reader")
	if err != nil || !strings.Contains(msg, "branch trellis") {
		t.Fatalf("%q %v", msg, err)
	}
	f := strings.Fields(argLines(t, out, 1)[0])
	if len(f) != 5 || f[0] != "--session-id" || f[2] != "build" {
		t.Fatalf("args = %v", f)
	}
	var lead layout.Pane
	for _, p := range s.tree.Panes() {
		if p.Env[pando.BranchVar] == "trellis" {
			lead = p
		}
	}
	if lead.Cwd != proj || lead.Label != "trellis-lead" || lead.Env[pando.SkipVar] != "" {
		t.Fatalf("lead = %+v", lead)
	}
	if pando.LoadRoutes(s.branchStoreDir()).For(f[1], "") != "trellis" {
		t.Fatal("the lead's session is not mapped")
	}
}

// A worker of a branch keeps its transcript under that branch; the steward
// reaches it with branch and child, and the steward's recall finds branch
// lines with their labels.
func TestABranchWorkersTranscriptLivesUnderItsBranch(t *testing.T) {
	e := newFakeEnv(t)
	s, logDir := chatServer(t, e, nil)
	fc := s.ensureChat()
	if fc.problem != "" {
		t.Fatal(fc.problem)
	}
	fc.mu.Lock()
	fc.chat.Append(ctx0, pando.KindUser, "the parent line about the household")
	fc.mu.Unlock()
	file := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(file, []byte(fakeSession), 0o600); err != nil {
		t.Fatal(err)
	}
	line := s.recordChild(fc, &workerRec{Label: "trellis-1", Branch: "trellis"}, file)
	if !strings.Contains(line, "in branch trellis") || !strings.Contains(line, `branch="trellis" child="trellis-1"`) {
		t.Fatalf("line = %q", line)
	}
	if _, err := os.Stat(filepath.Join(logDir, "branches", "trellis", "children", "trellis-1", "main")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "children", "trellis-1")); err == nil {
		t.Fatal("the transcript is also beside the root")
	}
	got, err := s.zoomChild(fc, "trellis-1", map[string]any{"id": float64(0), "n": float64(1), "branch": "trellis"})
	if err != nil || !strings.Contains(got, "sqlite index") {
		t.Fatalf("zoom: %q %v", got, err)
	}
	for _, bad := range []map[string]any{{"branch": ".."}, {"branch": "/etc"}, {"branch": "trellis", "child": "../../x"}} {
		bad["id"], bad["n"] = float64(0), float64(1)
		if _, err := s.zoomChild(fc, "", bad); err == nil {
			t.Errorf("%v was taken", bad)
		}
	}
	// A branch lead's own lines, found from the steward's recall.
	d, _ := pando.EnsureBranch(logDir, "trellis", nil)
	c, err := pando.Open(ctx0, pando.Config{Dir: d})
	if err != nil {
		t.Fatal(err)
	}
	c.Append(ctx0, pando.KindUser, "the epub parser of the trellis lead")
	c.Close()
	ts := &turnSocket{s: s, fc: fc}
	text, err := ts.call(stewardReq("recall", map[string]any{"query": "epub parser"}))
	if err != nil || !strings.Contains(text, "trellis:0+1") {
		t.Fatalf("recall: %q %v", text, err)
	}
	text, err = ts.call(stewardReq("date", map[string]any{"id": float64(0), "branch": "trellis"}))
	if err != nil || !strings.HasSuffix(strings.TrimSpace(text), "Z") {
		t.Fatalf("date: %q %v", text, err)
	}
	_ = time.Second
}

// F4: a long-running pane must be a claude pane.
func TestALongRunningPaneMustBeClaude(t *testing.T) {
	s := newEndedServer(t)
	prog, _ := claudeStub(t)
	lead := leadPane(t, s, prog, "trellis")
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"` + lead + `"}`)
	next()
	send(fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","harness":"codex","cmd_argv":[%q],"long_running":true,"pando_branch":"x"}`, t.TempDir(), prog))
	r := next()
	if r["ok"] == true || !strings.Contains(fmt.Sprint(r["error"]), "takes harness claude") {
		t.Fatalf("a codex lead was taken: %v", r)
	}
}

// F1/F3: in a git repo a lead runs in a worktree of its own, and a dir
// route sends that worktree's sessions to its branch. A closed branch is
// opened again only by a new lead.
func TestALeadRunsInItsOwnWorktreeWithADirRoute(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prog, out := claudeStub(t)
	if err := config.Save(config.Config{Harness: map[string]config.Harness{"claude": {Command: prog}}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	repo := filepath.Join(t.TempDir(), "trellis")
	for _, c := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if b, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", c, err, b)
		}
	}
	if _, err := pando.EnsureBranch(s.branchStoreDir(), "trellis", nil); err != nil {
		t.Fatal(err)
	}
	pando.CloseBranch(s.branchStoreDir(), "trellis", nil)
	msg, err := s.spawnLead(repo, "trellis", "build the reader")
	wt := filepath.Join(repo+"-worktrees", leadWorktreeName("trellis"))
	if err != nil || !strings.Contains(msg, "in its own clone "+wt) {
		t.Fatalf("%q %v", msg, err)
	}
	argLines(t, out, 1)
	if pando.LoadRoutes(s.branchStoreDir()).ForCwd(filepath.Join(wt, "src")) != "trellis" {
		t.Fatal("no dir route for the lead's worktree")
	}
	d, _ := pando.ExistingBranch(s.branchStoreDir(), "trellis")
	if bi, _ := pando.ReadBranch(d); bi.State != pando.BranchOpen {
		t.Fatalf("a new lead did not open the branch again: %+v", bi)
	}
	for _, p := range s.tree.Panes() {
		if p.Env[pando.BranchVar] == "trellis" && p.Cwd != wt {
			t.Fatalf("the lead runs in %s", p.Cwd)
		}
	}
}

func leadRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "trellis")
	for _, c := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if b, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", c, err, b)
		}
	}
	return repo
}

// G3/H1: names are injective; a lead gets a shared clone of the repo, the
// same clone again on the next spawn, its branch starts from a branch left
// by an earlier lead; a stray dir refuses the spawn, never the owner's dir.
func TestLeadClonesAreDistinctReusedAndNeverFallBack(t *testing.T) {
	names := map[string]string{}
	for _, b := range []string{"trellis/ui", "trellis.ui", "trellis-ui", "Trellis-ui", "trellis_ui"} {
		n := leadWorktreeName(b)
		if o, ok := names[n]; ok {
			t.Fatalf("%s and %s share %s", o, b, n)
		}
		names[n] = b
	}
	s := newEndedServer(t)
	repo := leadRepo(t)
	wt, _, objects, where, err := s.leadWorktree(repo, "trellis")
	if err != nil || len(objects) != 1 || objects[0] != filepath.Join(repo, ".git", "objects") {
		t.Fatalf("%q %q %v", wt, objects, err)
	}
	if !strings.Contains(where, "in "+repo+", with: git fetch "+wt+" lead-trellis:lead-trellis") {
		t.Fatalf("fetch line: %s", where)
	}
	if b, _ := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").Output(); strings.TrimSpace(string(b)) != leadWorktreeName("trellis") {
		t.Fatalf("the clone is on %q", b)
	}
	if alt, err := os.ReadFile(filepath.Join(wt, ".git", "objects", "info", "alternates")); err != nil || !strings.Contains(string(alt), filepath.Join(repo, ".git", "objects")) {
		t.Fatalf("not a shared clone: %q %v", alt, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "worktrees")); err == nil {
		t.Fatal("the owner's repo got a linked worktree")
	}
	// J4: the fetch line makes the branch in the owner's repo, and the next
	// clone of this lead starts from it.
	exec.Command("git", "-C", wt, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "lead work").Run()
	if b, err := exec.Command("git", "-C", repo, "fetch", "-q", wt, "lead-trellis:lead-trellis").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v %s", err, b)
	}
	head, _ := exec.Command("git", "-C", repo, "rev-parse", "lead-trellis").Output()
	wt2, _, _, _, err := s.leadWorktree(repo, "trellis")
	if err != nil || wt2 != wt {
		t.Fatalf("again: %q %v", wt2, err)
	}
	// The owner fetched an earlier lead's branch; a new clone starts there.
	os.RemoveAll(wt)
	if _, _, _, _, err := s.leadWorktree(repo, "trellis"); err != nil {
		t.Fatal(err)
	}
	if got, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output(); string(got) != string(head) {
		t.Fatalf("the new clone starts at %s, want %s", got, head)
	}
	os.RemoveAll(wt)
	os.MkdirAll(wt, 0o700)
	if _, _, _, _, err := s.leadWorktree(repo, "trellis"); err == nil || !strings.Contains(err.Error(), "does not start") {
		t.Fatalf("a stray dir was taken: %v", err)
	}
	rec := layout.Pane{Env: leadEnv(nil, "trellis", repo, objects)}
	if got, err := leadObjectsDirs(rec); err != nil || len(got) != 1 || got[0] != objects[0] {
		t.Fatalf("objects dirs: %q %v", got, err)
	}
	// K1: a store that is relative or goes through a link refuses the pane.
	if _, err := leadObjectsDirs(layout.Pane{Env: map[string]string{LeadObjectsVar: "rel/path"}}); err == nil {
		t.Fatal("a relative dir was taken")
	}
	link := filepath.Join(t.TempDir(), "objects")
	os.Symlink(objects[0], link)
	if _, err := leadObjectsDirs(layout.Pane{Env: map[string]string{LeadObjectsVar: link}}); err == nil {
		t.Fatal("a store through a link was taken")
	}
}

// J1: a project that is a linked worktree: the objects dir is its main
// repo's, named by git, and a fenced clone of it can be read.
func TestALeadOfALinkedWorktreeProjectReadsTheMainObjects(t *testing.T) {
	s := newEndedServer(t)
	repo := leadRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if b, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "side", linked).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	_, _, objects, _, err := s.leadWorktree(linked, "side")
	if err != nil || len(objects) != 1 || objects[0] != filepath.Join(repo, ".git", "objects") {
		t.Fatalf("%q %v", objects, err)
	}
}

// spawnParent starts a steward lead in a fresh repo and returns the repo,
// its pane record and the claude stub.
func spawnParent(t *testing.T) (*Server, string, layout.Pane, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prog, _ := claudeStub(t)
	if err := config.Save(config.Config{Harness: map[string]config.Harness{"claude": {Command: prog}}}); err != nil {
		t.Fatal(err)
	}
	s := newEndedServer(t)
	repo := leadRepo(t)
	if _, err := s.spawnLead(repo, "trellis", "lead"); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.tree.Panes() {
		if p.Env[pando.BranchVar] == "trellis" {
			return s, repo, p, prog
		}
	}
	t.Fatal("no parent lead")
	return nil, "", layout.Pane{}, ""
}

func nestedFrom(t *testing.T, s *Server, parent layout.Pane, prog, cwd string) layout.Pane {
	t.Helper()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"` + parent.ID + `"}`)
	next()
	send(fmt.Sprintf(`{"id":"1","cmd":"pane.create","cwd":%q,"kind":"pty","harness":"claude","cmd_argv":[%q],"long_running":true,"pando_branch":"ui","env":{%q:"/etc",%q:"/etc"}}`, cwd, prog, LeadObjectsVar, LeadRepoVar))
	r := next()
	if r["ok"] != true {
		t.Fatalf("nested: %v", r)
	}
	rec, _ := s.tree.Pane(r["result"].(map[string]any)["pane"].(string))
	return rec
}

// K1/L1: a nested lead clones the parent's recorded repo, whatever cwd the
// caller names, starts from the parent's branch once the owner has fetched
// it, and is granted the repo's objects only.
func TestANestedLeadClonesTheRecordedRepo(t *testing.T) {
	s, repo, parent, prog := spawnParent(t)
	if parent.Env[LeadRepoVar] != repo {
		t.Fatalf("parent record: %v", parent.Env)
	}
	exec.Command("git", "-C", parent.Cwd, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "parent work").Run()
	want, _ := exec.Command("git", "-C", parent.Cwd, "rev-parse", "HEAD").Output()
	// L1: coppice reads nothing of the parent's clone; the owner fetches
	// the parent's branch into the repo, and the nested lead starts there.
	if b, err := exec.Command("git", "-C", repo, "fetch", "-q", parent.Cwd, "lead-trellis:lead-trellis").CombinedOutput(); err != nil {
		t.Fatalf("owner fetch: %v %s", err, b)
	}
	rec := nestedFrom(t, s, parent, prog, t.TempDir())
	wantDir := filepath.Join(repo+"-worktrees", leadWorktreeName("trellis/ui"))
	if rec.Cwd != wantDir || rec.Env[LeadRepoVar] != repo {
		t.Fatalf("nested runs in %s with repo %q", rec.Cwd, rec.Env[LeadRepoVar])
	}
	if origin, _ := exec.Command("git", "-C", rec.Cwd, "config", "--get", "remote.origin.url").Output(); strings.TrimSpace(string(origin)) != repo {
		t.Fatalf("nested clone of %s", origin)
	}
	if got, _ := exec.Command("git", "-C", rec.Cwd, "rev-parse", "HEAD").Output(); string(got) != string(want) {
		t.Fatalf("nested starts at %s, want the parent's %s", got, want)
	}
	got, err := leadObjectsDirs(rec)
	if err != nil || len(got) != 1 || got[0] != filepath.Join(repo, ".git", "objects") {
		t.Fatalf("grant = %q %v", got, err)
	}
	if pando.LoadRoutes(s.branchStoreDir()).ForCwd(rec.Cwd) != "trellis/ui" {
		t.Fatal("no dir route for the nested lead")
	}
}

// K1: a parent that points its clone's objects at a secret dir gets a
// nested lead with no grant under it, and working git from the repo.
func TestAPlantedObjectsLinkGrantsNothing(t *testing.T) {
	s, repo, parent, prog := spawnParent(t)
	secret := filepath.Join(t.TempDir(), "ssh")
	os.MkdirAll(secret, 0o700)
	os.WriteFile(filepath.Join(secret, "id_ed25519"), []byte("KEY"), 0o600)
	objs := filepath.Join(parent.Cwd, ".git", "objects")
	os.Rename(objs, objs+".real")
	if err := os.Symlink(secret, objs); err != nil {
		t.Fatal(err)
	}
	rec := nestedFrom(t, s, parent, prog, parent.Cwd)
	got, err := leadObjectsDirs(rec)
	if err != nil || len(got) != 1 || got[0] != filepath.Join(repo, ".git", "objects") {
		t.Fatalf("grant = %q %v", got, err)
	}
	if out, err := exec.Command("git", "-C", rec.Cwd, "log", "-1", "--format=%s").Output(); err != nil || strings.TrimSpace(string(out)) != "x" {
		t.Fatalf("nested git: %q %v", out, err)
	}
	// The owner's repo itself behind a link is refused at the source.
	if _, err := worktreeObjectsOf(objs); err == nil {
		t.Fatal("a linked objects dir was named")
	}
}

// A parent with no recorded repo (a lead outside git) gives a nested lead
// in the parent's own dir, never the caller's cwd, with no grant (J6).
func TestANestedLeadOfANonGitLeadStaysInItsParentsDir(t *testing.T) {
	s := newEndedServer(t)
	prog, _ := claudeStub(t)
	lead := leadPane(t, s, prog, "trellis")
	prec, _ := s.tree.Pane(lead)
	rec := nestedFrom(t, s, prec, prog, leadRepo(t))
	if rec.Cwd != prec.Cwd || rec.Env[LeadRepoVar] != "" || rec.Env[LeadObjectsVar] != "" {
		t.Fatalf("nested = %s %v", rec.Cwd, rec.Env)
	}
}

func worktreeObjectsOf(objectsDir string) (string, error) {
	return worktree.ObjectsDir(filepath.Dir(filepath.Dir(objectsDir)))
}
