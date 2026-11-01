package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"daisugi-verify/pando"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/steward"
)

// A worker is one `daisugi tree run` the steward's spawn tool started. The
// server is the tree's starter for it (it proves the edge, reserves the
// budget and runs sprig), so the steward has one acting tool and the warden
// stays outside it. A worker's final report comes back as one `work`
// message. A work message starts no turn: the next turn of the owner sees it.

// Worker states.
const (
	workRunning = "running"
	workRan     = "ran"
	workRefused = "refused"
	workAsked   = "asked"
	workFailed  = "failed"
	workStopped = "stopped"
	// workWaiting is a worker that ended its run with a question (help):
	// it holds no process and no model call, and goes on when reply
	// answers it. workGaveUp is a worker that called give_up, or whose
	// question went unanswered past its time budget.
	workWaiting = "waiting"
	workGaveUp  = "gave_up"
	// workQueued and workPaused belong to pool workers (FM-R-93): the job
	// waits for an entry, or its group was stopped to make room.
	workQueued = "queued"
	workPaused = "paused"
)

// workerRec is one worker in the table, as workers.json keeps it.
type workerRec struct {
	Label       string `json:"label"`
	Project     string `json:"project"`
	Dir         string `json:"dir"`
	Profile     string `json:"profile"`
	Model       string `json:"model"`
	Started     string `json:"started"`
	State       string `json:"state"`
	Session     string `json:"session,omitempty"`
	SessionFile string `json:"session_file,omitempty"`
	// Fenced is true when sprig ran under the fence: its session log is
	// then in the work dir's sprig-sessions, outside the warden root.
	Fenced bool `json:"fenced,omitempty"`
	// Pool and Entry are set for a worker whose profile names a pool:
	// the pool, and the entry it runs on ("" while it waits).
	Pool       string `json:"pool,omitempty"`
	Entry      string `json:"entry,omitempty"`
	Background bool   `json:"background,omitempty"`
	// Attempts and Why are set for a spawn that branches.
	Attempts int    `json:"attempts,omitempty"`
	Why      string `json:"why,omitempty"`
	// Ask is the question a waiting worker put (steward_help.go).
	Ask *helpAsk `json:"ask,omitempty"`
	// Branch is the Pando branch the worker works for (round 8b): it gets
	// read-only search and zoom on that branch, and its transcript is kept
	// under it.
	Branch string `json:"branch,omitempty"`
}

// workerFields is the worker table of the server.
type workerFields struct {
	workMu sync.Mutex
	work   []*workerRec
	// workNext numbers the workers of each project.
	workNext map[string]int
	workLoad bool
	workCtx  context.Context
	workStop context.CancelFunc
	workWG   sync.WaitGroup
	// pool is the state of the pools (steward_pool.go); workMu guards it.
	pool poolRuntime
	// helpTimers end a waiting worker whose question went unanswered;
	// helpWaitFor, when set, replaces the wait (tests). workMu guards both.
	helpTimers  map[string]*time.Timer
	helpWaitFor time.Duration
}

// projectEnvelope is the operator's file for one project: the envelope its
// workers run in and the budget of its root. It lives at
// <config dir>/envelopes/<project>.json.
type projectEnvelope struct {
	Envelope json.RawMessage `json:"envelope"`
	// Tokens and Turns are the root's budget.
	Tokens int `json:"tokens"`
	Turns  int `json:"turns"`
	// Checks are the hard checks a branching spawn runs in each branch's
	// worktree: a name and a command. They come from this file, which is
	// the operator's, and never from the model's tool input (W3K-11).
	Checks map[string]string `json:"checks,omitempty"`
	// HelpWaitS is how long a worker of this project waits for an answer to
	// a question, in seconds. Zero is DefaultHelpWaitS. It is the worker's
	// time budget for a question: past it the worker ends as given up.
	HelpWaitS int `json:"help_wait_s,omitempty"`
	// Dir is the project dir the file was written for, and GrantedBy who
	// granted it: "default" (the steward, inside the default permission)
	// or "owner" (an approved card). A file written by hand has neither.
	Dir       string `json:"dir,omitempty"`
	GrantedBy string `json:"granted_by,omitempty"`
	// ModTime is the file's mtime when it was read. It goes into the root's
	// name, so a file that returns to earlier bytes is a new root, not the
	// old one with its budget spent (review r9, finding 8).
	ModTime int64 `json:"-"`
}

// branchWhys are the reasons a spawn may give for branching (W3K-8).
var branchWhys = []string{"failed_verify", "failed_tests", "low_confidence", "operator"}

// branchSpec is the branching a spawn asks for: Attempts 2 to 8 and why.
// The zero value is one worker.
type branchSpec struct {
	Attempts int
	Why      string
}

// check reads attempts and why the way the tool takes them.
func (b branchSpec) check() error {
	if b.Attempts == 0 && b.Why == "" {
		return nil
	}
	if b.Attempts < 2 || b.Attempts > 8 {
		return errors.New("attempts is 2 to 8. Leave it out to start one worker")
	}
	for _, w := range branchWhys {
		if b.Why == w {
			return nil
		}
	}
	return fmt.Errorf("why is one of %s", strings.Join(branchWhys, ", "))
}

// workerTools are the tools every worker gets.
var workerTools = []string{"read", "write", "edit", "bash"}

// workerToolsFor is the tools of one worker: the four, and help for a
// worker that can be asked. A pool job has nobody waiting for it, and a
// branch has no one to ask.
func workerToolsFor(w *workerRec) []string {
	if w.Pool == "" && w.Attempts == 0 {
		return append(append([]string{}, workerTools...), helpToolName)
	}
	return append([]string{}, workerTools...)
}

func envelopeDir() string { return filepath.Join(filepath.Dir(config.Path()), "envelopes") }

func (s *Server) workDir(fc *stewardChat) string { return filepath.Join(fc.dir, "work") }

// resolveProject finds the operator's project by name or by path among the
// pinned and the recent directories.
func (s *Server) resolveProject(name string) (string, error) {
	pinned := pinnedProjects()
	dirs := append([]string{}, pinned...)
	dirs = append(dirs, recentNotPinned(s.cfg.DataDir, pinned)...)
	var match []string
	seen := map[string]bool{}
	var names []string
	for _, d := range dirs {
		if seen[d] || !dirExists(d) {
			continue
		}
		seen[d] = true
		names = append(names, filepath.Base(d))
		if d == name || filepath.Base(d) == name {
			match = append(match, d)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return "", fmt.Errorf("%q is not one of the operator's projects. They are: %s", name, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("%d projects are named %q (%s). Name one by its full path", len(match), name, strings.Join(match, ", "))
}

// loadWorkers reads the worker table; a worker that was running when the
// server stopped is marked stopped and reported once.
func (s *Server) loadWorkers(fc *stewardChat) {
	var stopped []string
	s.workMu.Lock()
	defer func() {
		s.workMu.Unlock()
		for _, t := range stopped {
			s.logWork(fc, t)
		}
	}()
	if s.workLoad {
		return
	}
	s.workLoad = true
	s.workNext = map[string]int{}
	s.workCtx, s.workStop = context.WithCancel(context.Background())
	b, err := os.ReadFile(filepath.Join(s.workDir(fc), "workers.json"))
	if err != nil {
		return
	}
	var f struct {
		Workers []*workerRec   `json:"workers"`
		Next    map[string]int `json:"next"`
	}
	if json.Unmarshal(b, &f) != nil {
		return
	}
	s.work, s.workNext = f.Workers, f.Next
	if s.workNext == nil {
		s.workNext = map[string]int{}
	}
	changed := false
	held, queueNote := s.restoreHeldLocked(fc)
	if queueNote != "" {
		stopped = append(stopped, queueNote)
	}
	for _, w := range s.work {
		if w.State == workWaiting {
			// A question outlives a restart: its timer is armed again from
			// the deadline in the table, or it ends at once when that has
			// passed.
			if text := s.rearmHelpLocked(fc, w); text != "" {
				changed = true
				stopped = append(stopped, text)
			}
			continue
		}
		if w.State == workRunning || w.State == workPaused {
			w.State, changed = workStopped, true
			stopped = append(stopped, "["+w.Label+"] was stopped when coppice stopped")
		}
		if w.State == workQueued {
			if h, ok := held[w.Label]; ok && w.Background {
				s.pool.held = append(s.pool.held, &heldJob{HeldJob: h, rec: w, fc: fc})
				continue
			}
			w.State, changed = workStopped, true
			why := "a foreground job does not wait across a restart"
			if w.Background && queueNote != "" {
				why = "the queue file did not read"
			}
			stopped = append(stopped, "["+w.Label+"] was dropped when coppice stopped: "+why)
		}
	}
	if changed {
		s.saveWorkersLocked(fc)
	}
}

// saveWorkersLocked writes the table. The caller holds workMu.
func (s *Server) saveWorkersLocked(fc *stewardChat) {
	dir := s.workDir(fc)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"workers": s.work, "next": s.workNext})
	tmp := filepath.Join(dir, "workers.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "workers.json"))
	}
}

// logWork logs a worker's line as one work message. It starts no turn.
func (s *Server) logWork(fc *stewardChat, text string) {
	if !fc.hasChat() {
		s.reportToPane(text)
		return
	}
	if err := s.chatAppend(fc, pando.KindWork, text); err != nil {
		s.noteOnce("The steward's log could not take a worker's report: " + err.Error())
	}
}

// toolSpawn is the steward's spawn tool. It checks the project and its
// envelope, records the worker and returns at once; a goroutine runs it.
func (s *Server) toolSpawn(project, brief, profile, model string) (string, error) {
	return s.toolSpawnBackground(project, brief, profile, model, false)
}

// toolSpawnBackground is toolSpawn with the background flag (FM-R-97).
func (s *Server) toolSpawnBackground(project, brief, profile, model string, background bool) (string, error) {
	return s.toolSpawnBackground2(project, brief, profile, model, background, branchSpec{})
}

// toolSpawnBackground2 is toolSpawnBackground with the branching the spawn
// asks for.
func (s *Server) toolSpawnBackground2(project, brief, profile, model string, background bool, br branchSpec) (string, error) {
	fc := s.ensureChat()
	// hasChat reads fc.chat under fc.mu: a close sets it to nil under that
	// lock while a turn's tool call runs (FR-R-10).
	if !fc.hasChat() {
		return "", errNoChat
	}
	return s.toolSpawnBranching(fc, project, brief, profile, model, background, br)
}

// toolSpawnIn is spawn for the table and work dir of fc. The pane steward's
// fc holds no chat: its workers report to its pane.
func (s *Server) toolSpawnIn(fc *stewardChat, project, brief, profile, model string) (string, error) {
	return s.toolSpawnOpts(fc, project, brief, profile, model, false)
}

// toolSpawnOpts is spawn with every option. A profile that names a pool
// goes through the pool's queue (steward_pool.go); any other starts at once.
func (s *Server) toolSpawnOpts(fc *stewardChat, project, brief, profile, model string, background bool) (string, error) {
	return s.toolSpawnBranching(fc, project, brief, profile, model, background, branchSpec{})
}

// toolSpawnBranching is toolSpawnOpts with the branching the spawn asks
// for. A spawn that branches runs on a plain profile, not a pool.
func (s *Server) toolSpawnBranching(fc *stewardChat, project, brief, profile, model string, background bool, br branchSpec) (string, error) {
	return s.toolSpawnFull(fc, project, brief, profile, model, background, br, pandoSpawn{})
}

// pandoSpawn is where a spawn sits in the Pando tree (round 8b): Branch is
// the branch path below the steward's store, and Long starts a long-running
// agent in a pane of its own whose chat is that branch. A short worker with
// a branch gets read-only search and zoom on it, and its transcript is kept
// under it. The zero value is today's worker with closed memory.
type pandoSpawn struct {
	Branch string
	Long   bool
}

// toolSpawnFull is spawn with every option, the Pando branch included.
func (s *Server) toolSpawnFull(fc *stewardChat, project, brief, profile, model string, background bool, br branchSpec, ps pandoSpawn) (string, error) {
	if strings.TrimSpace(brief) == "" {
		return "", errors.New("the brief is empty")
	}
	if err := br.check(); err != nil {
		return "", err
	}
	dir, err := s.resolveProject(project)
	if err != nil {
		return "", err
	}
	name := filepath.Base(dir)
	if ps.Long && ps.Branch == "" {
		ps.Branch = name
	}
	if ps.Branch != "" {
		if _, err := pando.ParseBranch(ps.Branch); err != nil {
			return "", err
		}
	}
	if ps.Long {
		if br.Attempts > 0 || background {
			return "", errors.New("a long-running agent runs in a pane of its own: it takes no attempts and no background")
		}
		return s.spawnLead(dir, ps.Branch, brief)
	}
	// A short worker's branch exists before it starts, so its pando server
	// can search it from the first call (F5). A closed branch stays closed.
	if ps.Branch != "" {
		if _, err := pando.EnsureBranch(s.stewardStoreDir(), ps.Branch, s.clock); err != nil {
			return "", err
		}
	}
	pe, err := s.projectEnvelopeFor(name, dir)
	if err != nil {
		return "", err
	}
	if profile == "" {
		profile = config.DefaultCompactorProfile
	}
	if !flagValue.MatchString(profile) {
		return "", fmt.Errorf("the profile %q is not a profile name", profile)
	}
	if model != "" && !flagValue.MatchString(model) {
		return "", fmt.Errorf("the model %q is not a model id", model)
	}
	pool, _ := s.poolFor(fc, profile)
	if pool != nil && br.Attempts > 0 {
		return "", fmt.Errorf("a spawn with attempts runs on a plain profile. The profile %s names a pool", profile)
	}
	if pool == nil && background {
		return "", fmt.Errorf("a background job needs a pool: the profile %s names none in %s", profile, s.poolsPath())
	}
	if pool != nil {
		if !fc.hasChat() {
			return "", errors.New("pools run for the chat steward only; the pane steward starts a worker on a plain profile")
		}
		if model != "" && model != pool.Model {
			return "", fmt.Errorf("the pool %s serves the model %s only; a spawn on it cannot ask for %s", pool.Name, pool.Model, model)
		}
		return s.spawnPooled(fc, dir, brief, pool, background)
	}
	s.workMu.Lock()
	if !s.workLoad {
		s.workMu.Unlock()
		s.loadWorkers(fc)
		s.workMu.Lock()
	}
	if s.isClosed() {
		s.workMu.Unlock()
		return "", errors.New("the server is closing")
	}
	s.workNext[name]++
	w := &workerRec{
		Label: name + "-" + strconv.Itoa(s.workNext[name]), Project: name, Dir: dir, Profile: profile, Model: model,
		Started: s.clock().UTC().Format(pando.DateLayout), State: workRunning,
		Attempts: br.Attempts, Why: br.Why, Branch: ps.Branch,
	}
	s.work = append(s.work, w)
	s.saveWorkersLocked(fc)
	s.workWG.Add(1)
	ctx := s.workCtx
	s.workMu.Unlock()
	go s.runWorker(ctx, fc, w, pe, brief, "")
	return "started " + w.Label, nil
}

func readProjectEnvelope(path, name string) (*projectEnvelope, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no envelope for the project %s. Write one at %s: {\"envelope\": {...}, \"tokens\": N, \"turns\": N}", name, path)
	}
	var pe projectEnvelope
	if err := json.Unmarshal(b, &pe); err != nil || len(pe.Envelope) == 0 || string(pe.Envelope) == "null" {
		return nil, fmt.Errorf("the envelope file %s does not read. It must be {\"envelope\": {...}, \"tokens\": N, \"turns\": N}", path)
	}
	if fi, err := os.Stat(path); err == nil {
		pe.ModTime = fi.ModTime().UnixNano()
	}
	return &pe, nil
}

// workerSessionPath is the session file sprig writes for a session id:
// <id>.jsonl in the warden root's sessions dir, or in the work dir's
// sprig-sessions when sprig ran under the fence (FN-R-25).
func (s *Server) workerSessionPath(fc *stewardChat, session string, fenced bool) string {
	return filepath.Join(s.workerSessionsDir(fc, fenced), session+".jsonl")
}

// workerSessionsDir is where sprig keeps a worker's sessions.
func (s *Server) workerSessionsDir(fc *stewardChat, fenced bool) string {
	if fenced {
		return filepath.Join(s.workDir(fc), "sprig-sessions")
	}
	return filepath.Join(s.workDir(fc), "warden", "sessions")
}

// finishWorker sets a worker's end state and logs its message, unless the
// server is closing: a worker the close stopped is reported at the next start.
func (s *Server) finishWorker(ctx context.Context, fc *stewardChat, w *workerRec, state, session, text string) {
	if ctx.Err() != nil {
		return
	}
	s.workMu.Lock()
	w.State, w.Session = state, session
	if session != "" {
		w.SessionFile = s.workerSessionPath(fc, session, w.Fenced)
	}
	var lines []poolLine
	if w.Pool != "" {
		lines = s.poolEndedLocked(fc, w, state)
	}
	s.saveWorkersLocked(fc)
	file := w.SessionFile
	s.workMu.Unlock()
	if line := s.recordChild(fc, w, file); line != "" {
		text += "\n" + line
	}
	s.logWork(fc, text)
	s.flushLines(lines)
}

// cmdOpts is one command the server runs for the steward.
type cmdOpts struct {
	Dir  string
	Name string
	Args []string
	// Stdin is written to the command's standard input.
	Stdin []byte
	// FD3, when HasFD3, is written to a pipe the command gets as descriptor 3.
	// It is written after the command has started, so a long value cannot
	// block the start.
	FD3    []byte
	HasFD3 bool
	// Started, when set, is called once with the pid of the started
	// process, after the key is on its way and before runCmd waits.
	Started func(pid int)
	// Env is the environment; nil is the server's own.
	Env []string
	// Grace is how long the command has to stop after the group gets SIGTERM
	// when the context ends. Zero kills the group at once.
	Grace time.Duration
	// Wrap, when set, is a prefix the command runs behind: daisugi fence
	// while the warden is on (FN-R-17).
	Wrap []string
}

// waitError is a failure after the command started that is not an exit
// code, such as a pipe that would not close.
type waitError struct{ err error }

func (e *waitError) Error() string { return e.err.Error() }
func (e *waitError) Unwrap() error { return e.err }

// runCmd runs a command and returns its stdout, the head of its stderr and
// its exit code. When the context ends the whole process group is stopped:
// SIGTERM first when Grace is set, so a delegation tree can end its node,
// then SIGKILL.
func runCmd(ctx context.Context, o cmdOpts) (stdout, stderr string, code int, err error) {
	name, args := o.Name, o.Args
	if len(o.Wrap) > 0 {
		name, args = o.Wrap[0], append(append(append([]string{}, o.Wrap[1:]...), o.Name), o.Args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	if o.Stdin != nil {
		cmd.Stdin = bytes.NewReader(o.Stdin)
	}
	var keyR, keyW *os.File
	if o.HasFD3 {
		var perr error
		if keyR, keyW, perr = os.Pipe(); perr != nil {
			return "", "", 0, perr
		}
		cmd.ExtraFiles = []*os.File{keyR}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var timer *time.Timer
	cmd.Cancel = func() error {
		if o.Grace <= 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		pid := cmd.Process.Pid
		timer = time.AfterFunc(o.Grace, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		err := syscall.Kill(-pid, syscall.SIGTERM)
		// A paused group holds the signal until it goes on (FM-R-108):
		// SIGCONT lets the tree end its node.
		_ = syscall.Kill(-pid, syscall.SIGCONT)
		return err
	}
	cmd.WaitDelay = o.Grace + 2*time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &limitedBuffer{b: &errb, max: 4096}
	if serr := cmd.Start(); serr != nil {
		if keyR != nil {
			keyR.Close()
			keyW.Close()
		}
		return "", "", 0, serr
	}
	var keyDone chan struct{}
	if keyR != nil {
		keyR.Close()
		keyDone = make(chan struct{})
		go func() {
			defer close(keyDone)
			_, _ = keyW.Write(o.FD3)
			keyW.Close()
		}()
	}
	if o.Started != nil {
		o.Started(cmd.Process.Pid)
	}
	runErr := cmd.Wait()
	if keyDone != nil {
		// The caller may clear FD3 once runCmd returns; the writer is done.
		_ = keyW.Close()
		<-keyDone
	}
	if timer != nil {
		timer.Stop()
	}
	code = 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		code, runErr = ee.ExitCode(), nil
	}
	if runErr != nil {
		runErr = &waitError{runErr}
	}
	return out.String(), errb.String(), code, runErr
}

// workerEnv is the environment of a worker's tree commands: the server's
// own, with COPPICE_DATA_DIR set as coppice sets it for every pane, so the
// warden in the worker's tree knows the data dir and the log dir beside it.
func (s *Server) workerEnv() []string {
	// DAISUGI_PANDO_SKIP: a worker is not the owner's chat. It gets no
	// memory view and its messages stay in its own child store.
	// The skip list names its sessions before they run: the worker's tree
	// runs `claude -p` in the project, where the owner's own chats live too.
	env := pando.SkipEnvironWithList(append(os.Environ(), "COPPICE_DATA_DIR="+s.cfg.DataDir), steward.StoreRoot(s.cfg.DataDir))
	// A worker never carries a branch it did not get from its spawn.
	return pando.BranchEnviron(env, "")
}

// workerEnvFor is workerEnv for one worker: a worker of a branch carries the
// branch, so a pando server it starts gives it read-only search and zoom on
// that branch and below.
func (s *Server) workerEnvFor(w *workerRec) []string {
	return pando.BranchEnviron(s.workerEnv(), w.Branch)
}

// workerGrace is how long a stopped worker has to end its node before its
// group is killed.
const workerGrace = 5 * time.Second

// flagValue reports whether v can be the value of a flag the model chose:
// the characters of a profile name or a model id, and no leading dash.
var flagValue = regexp.MustCompile(`^[A-Za-z0-9._:/@+][A-Za-z0-9._:/@+-]*$`)

// rootSession is the session of a project's root for one envelope file:
// project-<name>-<hash>, where hash is the first 8 hex digits of the
// SHA-256 of the envelope bytes, the budget and the file's mtime. A changed file (an approve,
// a default grant, an edit by hand) names a new root, so the next worker
// runs under the envelope the file holds now, never a root made from an
// older one (the Trellis refusal of 2026-11-06).
func rootSession(name string, pe *projectEnvelope) string {
	h := sha256.New()
	h.Write(bytes.TrimSpace(pe.Envelope))
	fmt.Fprintf(h, "\x00%d\x00%d\x00%d", pe.Tokens, pe.Turns, pe.ModTime)
	return "project-" + name + "-" + hex.EncodeToString(h.Sum(nil))[:8]
}

// ensureRoot makes the project's root in the tree once for each envelope
// file, and returns its session (rootSession).
func (s *Server) ensureRoot(ctx context.Context, fc *stewardChat, name string, pe *projectEnvelope) (string, error) {
	work := s.workDir(fc)
	session := rootSession(name, pe)
	marker := filepath.Join(work, "roots", session)
	if _, err := os.Stat(marker); err == nil {
		return session, nil
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return "", err
	}
	envFile := filepath.Join(work, "roots", session+".envelope.json")
	if err := os.WriteFile(envFile, pe.Envelope, 0o600); err != nil {
		return "", err
	}
	args := []string{"tree", "root", envFile, "--session", session, "--data-dir", work, "--root", filepath.Join(work, "warden")}
	if pe.Tokens > 0 {
		args = append(args, "--tokens", strconv.Itoa(pe.Tokens))
	}
	if pe.Turns > 0 {
		args = append(args, "--turns", strconv.Itoa(pe.Turns))
	}
	_, stderr, code, err := runCmd(ctx, cmdOpts{Dir: work, Name: daisugiBin(), Args: args, Env: s.workerEnv(), Grace: workerGrace})
	if err != nil {
		return "", fmt.Errorf("cannot run daisugi: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("daisugi tree root failed: %s", firstLine(stderr, nil))
	}
	return session, os.WriteFile(marker, []byte(s.clock().UTC().Format(pando.DateLayout)+"\n"), 0o600)
}

// rootLeft is what the root may still hand out, tokens and turns, from
// daisugi tree status. ok is false when it cannot be read.
func (s *Server) rootLeft(ctx context.Context, work, session string) (tokens, turns int64, ok bool) {
	stdout, _, code, err := runCmd(ctx, cmdOpts{Dir: work, Name: daisugiBin(), Env: s.workerEnv(), Grace: workerGrace,
		Args: []string{"tree", "status", "--json", "--data-dir", work}})
	if err != nil || code != 0 {
		return 0, 0, false
	}
	var doc struct {
		Nodes []struct {
			Session string `json:"session"`
			Tokens  struct {
				Left *int64 `json:"left"`
			} `json:"tokens"`
			Turns struct {
				Left *int64 `json:"left"`
			} `json:"turns"`
		} `json:"nodes"`
	}
	if json.Unmarshal([]byte(stdout), &doc) != nil {
		return 0, 0, false
	}
	for _, n := range doc.Nodes {
		if n.Session != session {
			continue
		}
		if n.Tokens.Left != nil {
			tokens = *n.Tokens.Left
		}
		if n.Turns.Left != nil {
			turns = *n.Turns.Left
		}
		return tokens, turns, true
	}
	return 0, 0, false
}

// envelopeFolders are the fence grants a worker gets from its project's
// approved envelope: each file_write folder read and write, each other
// file_read folder read only. The envelope is the operator's own file, so
// its folders are the operator's word; the fence still refuses secrets and
// the warden's state. A pattern with a glob character is not a folder and
// is skipped.
func envelopeFolders(raw json.RawMessage) []string {
	var env struct {
		Permissions struct {
			Read  []string `json:"file_read"`
			Write []string `json:"file_write"`
		} `json:"permissions"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p, suffix string) {
		if p == "" || !filepath.IsAbs(p) || strings.ContainsAny(p, "*?[") || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p+suffix)
	}
	for _, p := range env.Permissions.Write {
		add(p, ":rw")
	}
	for _, p := range env.Permissions.Read {
		add(p, "")
	}
	return out
}

// runWorker is the goroutine of one worker: the root, the profile bundle,
// then `daisugi tree run`.
func (s *Server) runWorker(ctx context.Context, fc *stewardChat, w *workerRec, pe *projectEnvelope, brief, baseURL string) {
	s.runWorkerStep(ctx, fc, w, pe, brief, baseURL, "")
}

// runWorkerStep is runWorker for one segment of a worker. resume is "" for
// the first segment; for a segment after an answer it is the session of the
// first segment, and brief is then the answer.
func (s *Server) runWorkerStep(ctx context.Context, fc *stewardChat, w *workerRec, pe *projectEnvelope, brief, baseURL, resume string) {
	defer s.workWG.Done()
	var myPid int
	defer func() { s.poolForget(w.Label, myPid) }()
	stderrText := ""
	fail := func(why string) {
		if s.poolOut(ctx, fc, w, brief, why+"\n"+stderrText) {
			return
		}
		s.finishWorker(ctx, fc, w, workFailed, "", "["+w.Label+"] failed: "+why)
	}
	root, err := s.ensureRoot(ctx, fc, w.Project, pe)
	if err != nil {
		fail(err.Error())
		return
	}
	work := s.workDir(fc)
	args := []string{"tree", "run", "--parent", root, "--profile=" + w.Profile}
	if w.Model != "" {
		args = append(args, "--model="+w.Model)
	}
	// A root with a budget refuses a child that asks for no amount. The
	// spawn tool has no budget field, so a worker asks for what the root
	// has left. An envelope with no budget (the default) asks for none.
	if pe.Tokens > 0 || pe.Turns > 0 {
		tokens, turns := int64(pe.Tokens), int64(pe.Turns)
		if lt, lu, ok := s.rootLeft(ctx, work, root); ok {
			if pe.Tokens > 0 {
				tokens = lt
			}
			if pe.Turns > 0 {
				turns = lu
			}
		}
		if (pe.Tokens > 0 && tokens <= 0) || (pe.Turns > 0 && turns <= 0) {
			fail(fmt.Sprintf("the project's budget is spent: %d tokens and %d turns left. Raise it in %s",
				max(tokens, 0), max(turns, 0), filepath.Join(envelopeDir(), w.Project+".json")))
			return
		}
		if pe.Tokens > 0 {
			args = append(args, "--tokens", strconv.FormatInt(tokens, 10))
		}
		if pe.Turns > 0 {
			args = append(args, "--turns", strconv.FormatInt(turns, 10))
		}
	}
	// The project's hard checks ride as flags from the operator's file,
	// sorted by name. Only a branching run reads them (W3K-11).
	if w.Attempts > 0 {
		names := make([]string, 0, len(pe.Checks))
		for n := range pe.Checks {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			args = append(args, "--check="+n+"="+pe.Checks[n])
		}
	}
	// A worker is short work: its whole run has a cap, so a model call that
	// hangs cannot hold it for good (review r9, finding 6). A lead has none.
	runCap := config.DefaultWorkerMaxRunS
	if cfg, _, cerr := config.Load(); cerr == nil {
		runCap = cfg.WorkerMaxRunSOrDefault()
	}
	args = append(args, "--max-run-s", strconv.Itoa(runCap))
	var bundle []byte
	if w.Profile != config.DefaultCompactorProfile {
		var err error
		if bundle, err = s.bundleBytes(ctx); err != nil {
			fail(err.Error())
			return
		}
		defer clear(bundle)
		if baseURL != "" {
			patched, perr := patchBundleURL(bundle, w.Profile, baseURL)
			if perr != nil {
				fail(perr.Error())
				return
			}
			defer clear(patched)
			bundle = patched
		}
		args = append(args, "--keys-fd", "3")
	}
	args = append(args, "--data-dir", work, "--root", filepath.Join(work, "warden"), "-")
	tools := workerToolsFor(w)
	req := map[string]any{"task": brief, "envelope": json.RawMessage(pe.Envelope), "tools": tools}
	if resume != "" {
		req["resume"] = resume
	}
	if w.Attempts > 0 {
		req["attempts"], req["why"] = w.Attempts, w.Why
	}
	stdin, _ := json.Marshal(req)
	// The tree run stays outside the fence: it writes the worker's warden
	// root (its envelopes). Only sprig and its tools run fenced, with the
	// worktree and their session dir read and write; the warden root is no
	// rw grant of theirs, and the fence treats it as the warden's state
	// (FN-R-17, FN-R-25).
	env := s.workerEnvFor(w)
	fence, err := s.sprigFence(w.Dir, filepath.Join(work, "warden"), envelopeFolders(pe.Envelope)...)
	if err != nil {
		fail(err.Error())
		return
	}
	if fence != nil {
		b, _ := json.Marshal(fence)
		env = append(env, "DAISUGI_SPRIG_FENCE="+string(b))
		// --sprig-fence: the fence is required. A daisugi that does not
		// know the flag refuses it, so its worker never starts bare
		// (FN-R-28).
		args = append(args[:len(args)-1], "--sprig-fence", "-")
		s.workMu.Lock()
		w.Fenced = true
		s.workMu.Unlock()
	}
	stdout, stderr, code, err := runCmd(ctx, cmdOpts{Dir: w.Dir, Name: daisugiBin(), Args: args, Stdin: stdin,
		FD3: bundle, HasFD3: bundle != nil, Env: env, Grace: workerGrace,
		Started: func(pid int) { myPid = pid; s.poolStarted(w.Label, pid) }})
	if ctx.Err() != nil {
		return
	}
	stderrText = stderr
	if err != nil {
		fail("cannot run daisugi: " + err.Error())
		return
	}
	var out struct {
		Status  string   `json:"status"`
		Session string   `json:"session"`
		Answer  string   `json:"answer"`
		Reasons []string `json:"reasons"`
		AskID   string   `json:"ask_id"`
		// A worker that ended by help carries its kind and its question.
		HelpKind string `json:"help_kind"`
		HelpText string `json:"help_text"`
		// A branching run also reports its branches and the card it opened.
		Branches []json.RawMessage `json:"branches"`
		ChoiceID string            `json:"choice_id"`
	}
	parsed := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out) == nil
	reasons := strings.Join(out.Reasons, "; ")
	label := "[" + w.Label + "]"
	switch {
	case !parsed && code == 0:
		fail("tree run gave an answer that is not JSON")
	case !parsed:
		fail(firstLine(stderr, fmt.Errorf("tree run exited with %d", code)))
	case code == 0 && out.Status == "help":
		if resume != "" {
			out.Session = resume
		}
		s.workerHelp(ctx, fc, w, pe, out.Session, out.HelpKind, out.HelpText)
	case code == 0 && out.Status == "ran":
		report := label + " report\n" + out.Answer
		if w.Attempts > 0 {
			report += "\n" + s.branchLine(ctx, work, len(out.Branches), out.ChoiceID)
		}
		if resume != "" {
			out.Session = resume
		}
		s.finishWorker(ctx, fc, w, workRan, out.Session, report)
	case code == 3 || out.Status == "asked":
		text := label + " sent to the operator as ask " + out.AskID
		if reasons != "" {
			text += ". " + reasons
		}
		s.finishWorker(ctx, fc, w, workAsked, out.Session, text)
	case out.Status == "refused":
		s.finishWorker(ctx, fc, w, workRefused, out.Session, label+" refused: "+reasons)
	default:
		why := reasons
		if strings.Contains(why, fmt.Sprintf("sprig ran past %ds and was killed", runCap)) {
			why = fmt.Sprintf("it ran past the worker run cap of %d s ([steward] worker_max_run_s). Long work is a lead: spawn it with long_running: true", runCap)
		}
		if why == "" {
			why = firstLine(stderr, fmt.Errorf("tree run exited with %d", code))
		}
		if s.poolOut(ctx, fc, w, brief, why+"\n"+stderr) {
			return
		}
		s.finishWorker(ctx, fc, w, workFailed, out.Session, label+" failed: "+why)
	}
}

// bundleBytes runs daisugi agent-profiles bundle and returns the bytes it
// wrote. The caller hands them on and clears them. The command is in a
// group of its own that dies with the context, the read is capped at 1 MiB,
// and it ends when the command does, so a grandchild that keeps descriptor 3
// open cannot hold the call.
func (s *Server) bundleBytes(ctx context.Context) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := os.MkdirAll(s.compactorDir(), 0o700); err != nil {
		w.Close()
		return nil, err
	}
	cmd := exec.CommandContext(ctx, daisugiBin(), "agent-profiles", "bundle", "--fd", "3")
	cmd.Dir = s.compactorDir()
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{b: &stderr, max: 4096}
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, fmt.Errorf("cannot start daisugi to read the agent profiles: %w", err)
	}
	w.Close()
	var buf bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = buf.ReadFrom(io.LimitReader(r, 1<<20))
	}()
	werr := cmd.Wait()
	// The command is over. Whatever it wrote is in the pipe; a grandchild
	// that still holds the write end gets no more time.
	select {
	case <-read:
	case <-time.After(200 * time.Millisecond):
		r.Close()
		<-read
	}
	if werr != nil {
		return nil, fmt.Errorf("daisugi agent-profiles bundle failed: %s", firstLine(stderr.String(), werr))
	}
	return buf.Bytes(), nil
}

// closeWorkers stops every worker: its whole process group is killed. A
// worker the close stopped is reported at the next start.
func (s *Server) closeWorkers() {
	s.workMu.Lock()
	stop := s.workStop
	s.workMu.Unlock()
	if stop == nil {
		return
	}
	// A paused job goes on first, so that the stop reaches a process that
	// can end its node (FM-R-108).
	s.workMu.Lock()
	for _, w := range s.work {
		if w.State == workPaused {
			s.thawLocked(w.Label)
		}
	}
	s.workMu.Unlock()
	stop()
	s.closePools()
	done := make(chan struct{})
	go func() { s.workWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(workerGrace + 5*time.Second):
	}
}

// workersState is the workers part of a turn's state block.
func (s *Server) workersState() string {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	var b strings.Builder
	for _, w := range s.work {
		if w.State == workRunning {
			fmt.Fprintf(&b, "- %s on %s since %s\n", w.Label, w.Project, w.Started)
		}
	}
	var wait strings.Builder
	for _, w := range s.work {
		if w.State == workQueued || w.State == workPaused {
			fmt.Fprintf(&wait, "- %s on %s is %s (pool %s)\n", w.Label, w.Project, w.State, w.Pool)
		}
	}
	var asks strings.Builder
	for _, w := range s.work {
		if w.State == workWaiting && w.Ask != nil {
			fmt.Fprintf(&asks, "- %s on %s asks (%s): %s\n", w.Label, w.Project, w.Ask.Kind, oneLine(w.Ask.Text, 300))
		}
	}
	if b.Len() == 0 && wait.Len() == 0 && asks.Len() == 0 {
		return "workers: none\n"
	}
	out := ""
	if b.Len() > 0 {
		out = "workers running:\n" + b.String()
	}
	if wait.Len() > 0 {
		out += "workers waiting:\n" + wait.String()
	}
	if asks.Len() > 0 {
		out += "workers waiting for your answer (use reply):\n" + asks.String()
	}
	return out
}

// toolStatus is the steward's status tool: the worker table.
func (s *Server) toolStatusWithCards(work string) string {
	text := s.toolStatus()
	// The open review cards ride below the workers. The steward never
	// answers one: it is a pane, and the answer route refuses a pane.
	if n, ok := s.openCards(work); ok {
		text += fmt.Sprintf("\nopen cards: %d", n)
	}
	return text
}

func (s *Server) toolStatus() string {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if len(s.work) == 0 {
		return "no workers"
	}
	rows := append([]*workerRec{}, s.work...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Started < rows[j].Started })
	var b strings.Builder
	for _, w := range rows {
		model := w.Model
		if model == "" {
			model = "-"
		}
		file := w.SessionFile
		if file == "" {
			file = "-"
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s | %s", w.Label, w.Project, w.Profile, model, w.Started, w.State, file)
		if w.State == workWaiting && w.Ask != nil {
			fmt.Fprintf(&b, " | asks (%s): %s", w.Ask.Kind, oneLine(w.Ask.Text, 300))
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// branchLine is the one line a branching spawn adds to its report:
// "N branches ran; card <id> opened: <headline>" (W3K-16).
func (s *Server) branchLine(ctx context.Context, work string, n int, choice string) string {
	if choice == "" || !idShape.MatchString(choice) {
		return fmt.Sprintf("%d branches ran; no card opened.", n)
	}
	headline := ""
	stdout, _, code, err := runCmd(ctx, cmdOpts{Dir: work, Name: daisugiBin(), Env: s.workerEnv(), Grace: workerGrace,
		Args: []string{"rank", "show", "--choice", choice, "--json", "--data-dir", work}})
	if err == nil && code == 0 {
		var doc struct {
			Card struct {
				Headline string `json:"headline"`
			} `json:"card"`
		}
		if json.Unmarshal([]byte(stdout), &doc) == nil {
			headline = doc.Card.Headline
		}
	}
	if headline == "" {
		return fmt.Sprintf("%d branches ran; card %s opened.", n, choice)
	}
	return fmt.Sprintf("%d branches ran; card %s opened: %s", n, choice, headline)
}

// idShape is what a card id looks like on the command line.
var idShape = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,127}$`)

// openCards is the count of open review cards, from daisugi rank queue. It
// is false when the count cannot be read, so status says nothing rather
// than a wrong number. The data dir is the steward's work dir, where its
// branching spawns write their cards.
func (s *Server) openCards(work string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdout, _, code, err := runCmd(ctx, cmdOpts{Name: daisugiBin(), Env: s.workerEnv(), Grace: workerGrace,
		Args: []string{"rank", "queue", "--json", "--data-dir", work}})
	if err != nil || code != 0 {
		return 0, false
	}
	var doc struct {
		Cards []json.RawMessage `json:"cards"`
	}
	if json.Unmarshal([]byte(stdout), &doc) != nil || doc.Cards == nil {
		return 0, false
	}
	return len(doc.Cards), true
}
