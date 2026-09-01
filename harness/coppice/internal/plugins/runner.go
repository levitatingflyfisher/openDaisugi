package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultTries is how many times in a row a policy may fail before the
// runner stops starting it.
const DefaultTries = 5

// Runner starts each enabled policy as a process. A policy that exits 0 is
// finished. One that fails is started again after a wait, up to Tries
// times in a row.
type Runner struct {
	// Socket is the coppice socket a policy dials.
	Socket string
	// WorkDir holds a fresh copy of each shipped policy's files, written
	// on each start, and the shared library beside them in _lib.
	WorkDir string
	// Lib is the shared library the shipped policies import.
	Lib fs.FS
	// Launch starts one process through start and returns its pid. The
	// server passes LaunchPlugin here, so it knows the pid before the
	// policy can dial. Nil runs start alone.
	Launch func(id string, start func() (int, error)) (int, error)
	// Exited is told each pid that exited.
	Exited func(pid int)
	// Config holds the operator's settings for each plugin id. They lie
	// over the manifest's own config.
	Config map[string]map[string]any
	Log    *slog.Logger
	// Output gets what each policy writes. Nil drops it.
	Output io.Writer
	// Tries caps the failed starts in a row. Zero means DefaultTries.
	Tries int
	// Backoff is the wait before start try+1. Nil means one second, then
	// twice as long each time, up to thirty seconds.
	Backoff func(try int) time.Duration
	// StableAfter is how long a run must last to count as a success.
	// Zero means one minute.
	StableAfter time.Duration

	mu      sync.Mutex
	stop    chan struct{}
	stopped bool
	procs   map[int]*os.Process
	wg      sync.WaitGroup
}

// DataHome is $XDG_DATA_HOME, or ~/.local/share when that is empty.
func DataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share")
}

// WorkDir is where the runner writes the shipped policies:
// $XDG_DATA_HOME/coppice/plugins.
func WorkDir() string { return filepath.Join(DataHome(), "coppice", "plugins") }

// pythonInterpreter picks the python name a .py policy runs under: python3
// when it resolves on PATH, else bare python. Some systems (minimal images,
// Debian/Ubuntu without python-is-python3) have no python3 on PATH at all,
// so hardcoding it silently fails to start every Python policy there.
func pythonInterpreter() string {
	return pythonInterpreterWith(exec.LookPath)
}

func pythonInterpreterWith(lookPath func(string) (string, error)) string {
	if _, err := lookPath("python3"); err == nil {
		return "python3"
	}
	if _, err := lookPath("python"); err == nil {
		return "python"
	}
	// Neither resolves. Name python3 anyway: the policy then fails to
	// start with a clear "executable file not found" error instead of
	// silently picking a name known not to exist.
	return "python3"
}

// commandFor is the argv that runs policy p from dir. A Python script runs
// under whichever of python3/python resolves on PATH, so it needs no
// execute bit and no uv. Anything else runs as it is.
func commandFor(p Plugin, dir string) []string {
	path := filepath.Join(dir, filepath.FromSlash(p.Run))
	if strings.HasSuffix(p.Run, ".py") {
		return []string{pythonInterpreter(), path}
	}
	return []string{path}
}

// Start starts every policy in ps. Views are skipped.
func (r *Runner) Start(ps []Plugin) {
	r.mu.Lock()
	if r.stop == nil {
		r.stop = make(chan struct{})
		r.procs = map[int]*os.Process{}
	}
	r.mu.Unlock()
	// The shared library is written once, before any policy starts, so no
	// policy's start can remove it while another reads it.
	for _, p := range ps {
		if p.Kind == KindPolicy && p.Shipped {
			if err := r.writeLib(); err != nil {
				r.log().Error("plugins: cannot write the shared library", "err", err)
			}
			break
		}
	}
	for _, p := range ps {
		if p.Kind != KindPolicy {
			continue
		}
		r.wg.Add(1)
		go r.keep(p)
	}
}

// writeLib writes the shared library to WorkDir/_lib, mode 0700.
func (r *Runner) writeLib() error {
	if r.WorkDir == "" {
		return errors.New("the runner has no work directory")
	}
	if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(r.WorkDir, 0o700); err != nil {
		return err
	}
	if r.Lib == nil {
		return nil
	}
	return writeTree(r.Lib, filepath.Join(r.WorkDir, "_lib"))
}

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

// keep runs p until Stop, or until it fails Tries times in a row.
func (r *Runner) keep(p Plugin) {
	defer r.wg.Done()
	tries := r.Tries
	if tries <= 0 {
		tries = DefaultTries
	}
	stable := r.StableAfter
	if stable <= 0 {
		stable = time.Minute
	}
	failed := 0
	for {
		if r.isStopped() {
			return
		}
		began := time.Now()
		err := r.runOnce(p)
		if r.isStopped() {
			return
		}
		if err == nil {
			r.log().Info("plugins: plugin finished", "plugin", p.ID)
			return
		}
		r.log().Warn("plugins: plugin exited", "plugin", p.ID, "err", errText(err))
		if time.Since(began) >= stable {
			failed = 0
		}
		failed++
		if failed >= tries {
			r.log().Error(fmt.Sprintf("plugins: gave up on plugin %s after %d tries. Fix it, then restart the server.", p.ID, failed),
				"plugin", p.ID)
			return
		}
		select {
		case <-r.stop:
			return
		case <-time.After(r.backoff(failed)):
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "exit 0"
	}
	return err.Error()
}

func (r *Runner) backoff(try int) time.Duration {
	if r.Backoff != nil {
		return r.Backoff(try)
	}
	// Past six tries the doubling is past thirty seconds, and a large
	// shift would wrap.
	if try < 1 || try > 6 {
		return 30 * time.Second
	}
	d := time.Second << (try - 1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func (r *Runner) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// runOnce prepares p's directory, starts it, and waits for it to exit.
func (r *Runner) runOnce(p Plugin) error {
	dir, err := r.prepare(p)
	if err != nil {
		return err
	}
	argv := commandFor(p, dir)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = r.env(p)
	out := r.Output
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout, cmd.Stderr = out, out
	// A process that outlives the policy may hold its output pipe. Wait
	// stops waiting for the pipe after this long.
	cmd.WaitDelay = 2 * time.Second
	setProcAttrs(cmd)
	start := func() (int, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.stopped {
			return 0, errors.New("the runner stopped")
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		r.procs[cmd.Process.Pid] = cmd.Process
		return cmd.Process.Pid, nil
	}
	launch := r.Launch
	if launch == nil {
		launch = func(_ string, f func() (int, error)) (int, error) { return f() }
	}
	pid, err := launch(p.ID, start)
	if err != nil {
		// Launch may have started the process and failed after. It must
		// not run on unseen.
		if cmd.Process != nil {
			killGroup(cmd.Process.Pid, true)
			_ = cmd.Wait()
			r.forget(cmd.Process.Pid)
		}
		return err
	}
	// The session goes first, so a process the policy left behind cannot
	// hold the output pipe and keep Wait waiting.
	err = r.waitAndClear(cmd, pid)
	r.forget(pid)
	if r.Exited != nil {
		r.Exited(pid)
	}
	return err
}

// waitAndClear waits for the policy to exit, then kills every process
// left in its group and its session. WaitDelay bounds the wait for a
// pipe that such a process still holds.
func (r *Runner) waitAndClear(cmd *exec.Cmd, pid int) error {
	err := cmd.Wait()
	// The policy is reaped, so its pid alone names nothing. Its session is
	// found by scan.
	killSession(pid)
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

func (r *Runner) forget(pid int) {
	r.mu.Lock()
	delete(r.procs, pid)
	r.mu.Unlock()
}

// env is the runner's own environment without the names a pane carries,
// with the socket, the plugin id and its config added.
func (r *Runner) env(p Plugin) []string {
	drop := map[string]bool{
		"COPPICE_PANE": true, "COPPICE_SOCK": true, "COPPICE_SOCKET": true,
		"COPPICE_PLUGIN": true, "COPPICE_PLUGIN_CONFIG": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			out = append(out, kv)
		}
	}
	cfg := map[string]any{}
	for k, v := range p.Config {
		cfg[k] = v
	}
	for k, v := range r.Config[p.ID] {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		raw = []byte("{}")
	}
	return append(out,
		"COPPICE_SOCKET="+r.Socket, "COPPICE_SOCK="+r.Socket,
		"COPPICE_PLUGIN="+p.ID, "COPPICE_PLUGIN_CONFIG="+string(raw))
}

// prepare returns the directory p runs from. A plugin on disk runs where
// it is. A shipped one is written fresh under WorkDir, mode 0700, beside
// the shared library Start wrote.
func (r *Runner) prepare(p Plugin) (string, error) {
	if !p.Shipped {
		return p.Dir, nil
	}
	if r.WorkDir == "" {
		return "", errors.New("the runner has no work directory")
	}
	dir := filepath.Join(r.WorkDir, p.ID)
	if err := writeTree(p.FS, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// writeTree replaces dir with the files of fsys. Directories and files are
// mode 0700.
func writeTree(fsys fs.FS, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o700)
	})
}

// Stop ends every policy and waits for the runner to finish. Each policy
// gets a terminate signal to its whole process group, then a kill after
// two seconds.
func (r *Runner) Stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	if r.stop == nil {
		r.stop = make(chan struct{})
		r.procs = map[int]*os.Process{}
	}
	close(r.stop)
	pids := make([]int, 0, len(r.procs))
	for pid := range r.procs {
		pids = append(pids, pid)
	}
	r.mu.Unlock()
	for _, pid := range pids {
		killGroup(pid, false)
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(2 * time.Second):
	}
	// Only a policy not yet reaped is signalled by its group. The rest of
	// each session is found by scan.
	r.mu.Lock()
	for pid := range r.procs {
		killGroup(pid, true)
	}
	r.mu.Unlock()
	for _, pid := range pids {
		killSession(pid)
	}
	<-done
}
