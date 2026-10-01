package server

import (
	"bytes"
	"context"
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
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
)

// A worker is one `daisugi tree run` the foreman's spawn tool started. The
// server is the tree's starter for it (it proves the edge, reserves the
// budget and runs sprig), so the foreman has one acting tool and the gate
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
}

// projectEnvelope is the operator's file for one project: the envelope its
// workers run in and the budget of its root. It lives at
// <config dir>/envelopes/<project>.json.
type projectEnvelope struct {
	Envelope json.RawMessage `json:"envelope"`
	// Tokens and Turns are the root's budget.
	Tokens int `json:"tokens"`
	Turns  int `json:"turns"`
}

// workerTools are the tools every worker gets.
var workerTools = []string{"read", "write", "edit", "bash"}

func envelopeDir() string { return filepath.Join(filepath.Dir(config.Path()), "envelopes") }

func (s *Server) workDir(fc *foremanChat) string { return filepath.Join(fc.dir, "work") }

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
func (s *Server) loadWorkers(fc *foremanChat) {
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
	for _, w := range s.work {
		if w.State == workRunning {
			w.State, changed = workStopped, true
			stopped = append(stopped, "["+w.Label+"] was stopped when coppice stopped")
		}
	}
	if changed {
		s.saveWorkersLocked(fc)
	}
}

// saveWorkersLocked writes the table. The caller holds workMu.
func (s *Server) saveWorkersLocked(fc *foremanChat) {
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
func (s *Server) logWork(fc *foremanChat, text string) {
	if !fc.hasChat() {
		s.reportToPane(text)
		return
	}
	if err := s.chatAppend(fc, foreman.KindWork, text); err != nil {
		s.noteOnce("The foreman's log could not take a worker's report: " + err.Error())
	}
}

// toolSpawn is the foreman's spawn tool. It checks the project and its
// envelope, records the worker and returns at once; a goroutine runs it.
func (s *Server) toolSpawn(project, brief, profile, model string) (string, error) {
	fc := s.ensureChat()
	// hasChat reads fc.chat under fc.mu: a close sets it to nil under that
	// lock while a turn's tool call runs (FR-R-10).
	if !fc.hasChat() {
		return "", errNoChat
	}
	return s.toolSpawnIn(fc, project, brief, profile, model)
}

// toolSpawnIn is spawn for the table and work dir of fc. The pane foreman's
// fc holds no chat: its workers report to its pane.
func (s *Server) toolSpawnIn(fc *foremanChat, project, brief, profile, model string) (string, error) {
	if strings.TrimSpace(brief) == "" {
		return "", errors.New("the brief is empty")
	}
	dir, err := s.resolveProject(project)
	if err != nil {
		return "", err
	}
	name := filepath.Base(dir)
	envPath := filepath.Join(envelopeDir(), name+".json")
	pe, err := readProjectEnvelope(envPath, name)
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
		Started: s.clock().UTC().Format(foreman.DateLayout), State: workRunning,
	}
	s.work = append(s.work, w)
	s.saveWorkersLocked(fc)
	s.workWG.Add(1)
	ctx := s.workCtx
	s.workMu.Unlock()
	go s.runWorker(ctx, fc, w, pe, brief)
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
	return &pe, nil
}

// workerSessionPath is the session file sprig writes for a session id:
// <id>.jsonl in the gate root's sessions dir.
func (s *Server) workerSessionPath(fc *foremanChat, session string) string {
	return filepath.Join(s.workDir(fc), "gate", "sessions", session+".jsonl")
}

// finishWorker sets a worker's end state and logs its message, unless the
// server is closing: a worker the close stopped is reported at the next start.
func (s *Server) finishWorker(ctx context.Context, fc *foremanChat, w *workerRec, state, session, text string) {
	if ctx.Err() != nil {
		return
	}
	s.workMu.Lock()
	w.State, w.Session = state, session
	if session != "" {
		w.SessionFile = s.workerSessionPath(fc, session)
	}
	s.saveWorkersLocked(fc)
	s.workMu.Unlock()
	s.logWork(fc, text)
}

// cmdOpts is one command the server runs for the foreman.
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
	cmd := exec.CommandContext(ctx, o.Name, o.Args...)
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
		return syscall.Kill(-pid, syscall.SIGTERM)
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
// gate in the worker's tree knows the data dir and the log dir beside it.
func (s *Server) workerEnv() []string {
	return append(os.Environ(), "COPPICE_DATA_DIR="+s.cfg.DataDir)
}

// workerGrace is how long a stopped worker has to end its node before its
// group is killed.
const workerGrace = 5 * time.Second

// flagValue reports whether v can be the value of a flag the model chose:
// the characters of a profile name or a model id, and no leading dash.
var flagValue = regexp.MustCompile(`^[A-Za-z0-9._:/@+][A-Za-z0-9._:/@+-]*$`)

// ensureRoot makes the project's root in the tree once, as project-<name>.
func (s *Server) ensureRoot(ctx context.Context, fc *foremanChat, name string, pe *projectEnvelope) error {
	work := s.workDir(fc)
	marker := filepath.Join(work, "roots", name)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return err
	}
	envFile := filepath.Join(work, "roots", name+".envelope.json")
	if err := os.WriteFile(envFile, pe.Envelope, 0o600); err != nil {
		return err
	}
	args := []string{"tree", "root", envFile, "--session", "project-" + name, "--data-dir", work, "--root", filepath.Join(work, "gate")}
	if pe.Tokens > 0 {
		args = append(args, "--tokens", strconv.Itoa(pe.Tokens))
	}
	if pe.Turns > 0 {
		args = append(args, "--turns", strconv.Itoa(pe.Turns))
	}
	_, stderr, code, err := runCmd(ctx, cmdOpts{Dir: work, Name: daisugiBin(), Args: args, Env: s.workerEnv(), Grace: workerGrace})
	if err != nil {
		return fmt.Errorf("cannot run daisugi: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("daisugi tree root failed: %s", firstLine(stderr, nil))
	}
	return os.WriteFile(marker, []byte(s.clock().UTC().Format(foreman.DateLayout)+"\n"), 0o600)
}

// runWorker is the goroutine of one worker: the root, the profile bundle,
// then `daisugi tree run`.
func (s *Server) runWorker(ctx context.Context, fc *foremanChat, w *workerRec, pe *projectEnvelope, brief string) {
	defer s.workWG.Done()
	fail := func(why string) { s.finishWorker(ctx, fc, w, workFailed, "", "["+w.Label+"] failed: "+why) }
	if err := s.ensureRoot(ctx, fc, w.Project, pe); err != nil {
		fail(err.Error())
		return
	}
	work := s.workDir(fc)
	args := []string{"tree", "run", "--parent", "project-" + w.Project, "--profile=" + w.Profile}
	if w.Model != "" {
		args = append(args, "--model="+w.Model)
	}
	var bundle []byte
	if w.Profile != config.DefaultCompactorProfile {
		var err error
		if bundle, err = s.bundleBytes(ctx); err != nil {
			fail(err.Error())
			return
		}
		defer clear(bundle)
		args = append(args, "--keys-fd", "3")
	}
	args = append(args, "--data-dir", work, "--root", filepath.Join(work, "gate"), "-")
	req := map[string]any{"task": brief, "envelope": json.RawMessage(pe.Envelope), "tools": workerTools}
	stdin, _ := json.Marshal(req)
	stdout, stderr, code, err := runCmd(ctx, cmdOpts{Dir: w.Dir, Name: daisugiBin(), Args: args, Stdin: stdin,
		FD3: bundle, HasFD3: bundle != nil, Env: s.workerEnv(), Grace: workerGrace})
	if ctx.Err() != nil {
		return
	}
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
	}
	parsed := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out) == nil
	reasons := strings.Join(out.Reasons, "; ")
	label := "[" + w.Label + "]"
	switch {
	case !parsed && code == 0:
		fail("tree run gave an answer that is not JSON")
	case !parsed:
		fail(firstLine(stderr, fmt.Errorf("tree run exited with %d", code)))
	case code == 0 && out.Status == "ran":
		s.finishWorker(ctx, fc, w, workRan, out.Session, label+" report\n"+out.Answer)
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
		if why == "" {
			why = firstLine(stderr, fmt.Errorf("tree run exited with %d", code))
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
	stop()
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
	if b.Len() == 0 {
		return "workers: none\n"
	}
	return "workers running:\n" + b.String()
}

// workerLogBytes is how much of a worker's own log zoom(label) returns:
// the newest bytes.
const workerLogBytes = 32 << 10

// toolZoomWorker is zoom(label): the newest 32 KiB of that worker's own log,
// its session file. The file is the one the worker table names, and only
// when it lies below the work dir's sessions.
func (s *Server) toolZoomWorker(fc *foremanChat, label string) (string, error) {
	s.workMu.Lock()
	var rec workerRec
	found := false
	var labels []string
	for _, w := range s.work {
		labels = append(labels, w.Label)
		if w.Label == label {
			rec, found = *w, true
		}
	}
	s.workMu.Unlock()
	if !found {
		if len(labels) == 0 {
			return "", fmt.Errorf("no worker is named %q. There are no workers", label)
		}
		return "", fmt.Errorf("no worker is named %q. The workers are %s", label, strings.Join(labels, ", "))
	}
	if rec.SessionFile == "" {
		if rec.State == workRunning {
			return "", fmt.Errorf("%s is still running. Its log can be read when it ends; status shows its state", label)
		}
		return "", fmt.Errorf("%s left no session log (it %s)", label, rec.State)
	}
	sessions := filepath.Join(s.workDir(fc), "gate", "sessions")
	file := filepath.Clean(rec.SessionFile)
	if rel, err := filepath.Rel(sessions, file); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the log of %s is outside the workers' sessions and is not read", label)
	}
	// The directory is resolved; the file itself is opened without following
	// a link and without blocking (a fifo), and checked on the open
	// descriptor, so nothing can be swapped in between.
	realDir, err := filepath.EvalSymlinks(filepath.Dir(file))
	realRoot, rerr := filepath.EvalSymlinks(sessions)
	if err != nil || rerr != nil {
		return "", fmt.Errorf("the log of %s cannot be read: it is not there", label)
	}
	if rel, err := filepath.Rel(realRoot, realDir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the log of %s is outside the workers' sessions and is not read", label)
	}
	f, err := os.OpenFile(filepath.Join(realDir, filepath.Base(file)), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("the log of %s cannot be read: %v", label, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("the log of %s is not a file", label)
	}
	return tailOf(f, fi.Size(), workerLogBytes)
}

// tailOf reads the newest max bytes of f, starting at a line and at a
// character, and says how many bytes it left out.
func tailOf(f *os.File, size, max int64) (string, error) {
	off := int64(0)
	if size > max {
		off = size - max
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return "", err
	}
	cut := off
	if off > 0 {
		// Start at the first whole line; a log with no line break keeps the
		// bytes, from the first whole character.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 && i+1 < len(buf) {
			buf = buf[i+1:]
			cut += int64(i + 1)
		} else {
			for len(buf) > 0 && !utf8.RuneStart(buf[0]) {
				buf = buf[1:]
				cut++
			}
		}
	}
	text := string(buf)
	if cut > 0 {
		text = fmt.Sprintf("(the first %d bytes of this log are not shown)\n%s", cut, text)
	}
	return text, nil
}

// toolStatus is the foreman's status tool: the worker table.
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
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s | %s\n", w.Label, w.Project, w.Profile, model, w.Started, w.State, file)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
