package pane

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// SpawnOpts is everything a pty pane needs to start. Sock and PaneID become
// COPPICE_SOCK and COPPICE_PANE in the child's environment: that pair is how
// the gate hook in the child finds its way back to report state.
type SpawnOpts struct {
	Cwd    string
	Argv   []string
	Env    map[string]string
	Cols   int
	Rows   int
	Sock   string
	PaneID string
}

// BuildEnv merges the server's own environment with the pane's extras and the
// two coppice variables. A key set twice would be ambiguous to the child, so
// later values replace earlier ones rather than appending.
//
// Spec-02 makes the injection unconditional: every spawned process gets
// COPPICE_SOCK and COPPICE_PANE. When an argument is empty the inherited value
// is REMOVED, never left in place. A server started from inside a coppice pane
// inherits that pane's COPPICE_PANE, and passing it on would make the gate hook
// in the child report state for somebody else's pane.
func BuildEnv(base []string, extra map[string]string, sock, paneID string) []string {
	merged := map[string]string{}
	order := []string{}
	set := func(k, v string) {
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range extra {
		set(k, v)
	}
	set("TERM", "xterm-256color")
	set("COPPICE_SOCK", sock)
	set("COPPICE_PANE", paneID)
	out := make([]string, 0, len(order))
	for _, k := range order {
		// An empty value means "this must not reach the child", not "leave
		// whatever we inherited".
		if (k == "COPPICE_SOCK" || k == "COPPICE_PANE") && merged[k] == "" {
			continue
		}
		out = append(out, k+"="+merged[k])
	}
	return out
}

// PTY is a process on a pseudo-terminal, with its output feeding a Grid.
//
// pty.StartWithSize makes the direct child a session leader with the pty as
// its controlling terminal (Setsid + Setctty), so its pgid equals its pid.
// That is what lets Close signal the whole process group with -pid rather
// than just the one process this struct was handed.
//
// Two goroutines run for the life of a PTY, and each owns exactly one piece
// of state so neither can race the other:
//   - the reader drains the master into the grid until it errors (EIO once
//     nothing at all holds the slave open, or the read end closing under it
//     from Close) or EOF, then exits. It never touches cmd or done.
//   - the waiter blocks on cmd.Wait(), which returns as soon as the direct
//     child exits even if a grandchild still holds the pty slave open. It
//     records the exit code and is the only goroutine that closes done.
//
// A harness that backgrounds work and exits (`x & exit N`) would hang
// forever if "the child exited" were inferred from the master read
// returning EIO instead: a grandchild holding the slave keeps that read
// blocked long after the direct child, and its exit code, are gone.
type PTY struct {
	cmd  *exec.Cmd
	f    *os.File
	grid *Grid
	done chan struct{}

	readerDone chan struct{}

	mu   sync.Mutex
	code int
	got  bool

	closeOnce sync.Once
	closeErr  error
}

func StartPTY(o SpawnOpts, g *Grid) (*PTY, error) {
	if len(o.Argv) == 0 {
		return nil, fmt.Errorf("pane needs a command to run")
	}
	// Spec-02 makes the two coppice variables unconditional. An empty one here
	// means the caller forgot, and a pane whose gate hook cannot find its way
	// home is worse than a pane that refuses to start.
	if o.Sock == "" || o.PaneID == "" {
		return nil, fmt.Errorf("a pane needs both a socket path and a pane id in SpawnOpts")
	}
	if o.Cols <= 0 {
		o.Cols = 120
	}
	if o.Rows <= 0 {
		o.Rows = 40
	}
	cmd := exec.Command(o.Argv[0], o.Argv[1:]...)
	cmd.Dir = o.Cwd
	cmd.Env = BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(o.Cols), Rows: uint16(o.Rows)})
	if err != nil {
		return nil, fmt.Errorf("cannot start %s: %w", o.Argv[0], err)
	}
	p := &PTY{
		cmd:        cmd,
		f:          f,
		grid:       g,
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
	}

	// The reader: drains the master into the grid until it errors, however
	// that happens, and touches nothing but the grid.
	go func() {
		defer close(p.readerDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				_, _ = g.Write(buf[:n])
			}
			if err != nil {
				// EIO once nothing holds the slave open, EOF, or the master
				// closing out from under us via Close: all just mean "stop
				// reading", not news worth reporting.
				break
			}
		}
	}()

	// The waiter: the ONLY goroutine that calls cmd.Wait(), records the exit
	// code, and closes done. cmd.Wait() returns as soon as the direct child
	// exits, whether or not a grandchild still holds the pty slave open, so
	// Done() cannot be blocked by the reader goroutine above.
	go func() {
		code := 0
		if werr := cmd.Wait(); werr != nil {
			var ee *exec.ExitError
			if ok := asExitError(werr, &ee); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		p.mu.Lock()
		p.code, p.got = code, true
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func (p *PTY) Write(b []byte) (int, error) { return p.f.Write(b) }

// Resize sets the grid first, then the child's winsize. A child that redraws on
// SIGWINCH must find the grid already the new size, or the first redraw lands
// in a grid of the old shape.
func (p *PTY) Resize(cols, rows int) error {
	if err := p.grid.Resize(cols, rows); err != nil {
		return err
	}
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *PTY) Done() <-chan struct{} { return p.done }

// Drained closes once the reader goroutine has stopped feeding the grid -
// EIO, EOF, or Close's own master-close. Done means the child exited; a
// grandchild can still hold the pty slave open well after that, so a caller
// that wants the child's last bytes to have landed in the grid first must
// wait on this too, not just on Done.
func (p *PTY) Drained() <-chan struct{} { return p.readerDone }

func (p *PTY) ExitCode() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.got
}

// Close tears down the pane's whole process tree, not just the one process it
// started. A harness that backgrounds work leaves grandchildren holding the
// pty slave (or nothing at all, if they detach further); killing only
// cmd.Process would leave that work running behind a pane that looks closed.
//
// It signals the process group first with SIGHUP, a request a well-behaved
// process can act on before it dies, gives it a short grace period, and only
// then escalates to SIGKILL for whatever is still alive - a process that
// ignores or traps SIGHUP included. Idempotent: a second call is a no-op.
func (p *PTY) Close() error {
	p.closeOnce.Do(func() {
		if p.cmd.Process != nil {
			pgid := p.cmd.Process.Pid // pgid == pid: StartWithSize sets Setsid.
			_ = syscall.Kill(-pgid, syscall.SIGHUP)
			time.Sleep(200 * time.Millisecond)
			if err := syscall.Kill(-pgid, 0); err == nil {
				// Signal 0 sent successfully: the group still exists.
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		p.closeErr = p.f.Close()
		<-p.readerDone
	})
	return p.closeErr
}
