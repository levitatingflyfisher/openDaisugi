package voice

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// The resident engine protocol, daisugi-voice-1, and how the server runs
// the child. The one definition is src/opendaisugi/voice/resident.py; this
// file follows it.
const (
	ResidentProtocol = "daisugi-voice-1"
	MaxFrameBytes    = 64 * 1024 * 1024
	LoadTimeoutS     = 300.0
	ClipTimeoutS     = 120.0
	StopGraceS       = 2.0
	stderrLines      = 20
)

const (
	loadingMsg = "The speech engine is loading its model. Try again in a moment."
	againMsg   = "It is starting again. Try again in a moment."
)

// Frame is one request: the clip's length, big-endian, then the clip.
func Frame(wav []byte) []byte {
	out := make([]byte, 4, 4+len(wav))
	binary.BigEndian.PutUint32(out, uint32(len(wav)))
	return append(out, wav...)
}

// ParseResidentLine is resident.parse_line: the line as a JSON object, or
// nil when it is not UTF-8 or not one object.
func ParseResidentLine(raw []byte) *pyjson.Object {
	text, err := pystr.DecodeStrict(raw)
	if err != nil {
		return nil
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// IsReadyLine is resident.is_ready.
func IsReadyLine(o *pyjson.Object) bool {
	if o == nil {
		return false
	}
	v, _ := o.Get("ready")
	s, ok := v.(string)
	return ok && s == ResidentProtocol
}

// ReplyKind is resident.reply_kind: "text", "error", or "" for a line the
// server does not read as a reply.
func ReplyKind(o *pyjson.Object) string {
	if o == nil {
		return ""
	}
	if v, _ := o.Get("text"); isStr(v) {
		return "text"
	}
	if v, _ := o.Get("error"); isStr(v) {
		return "error"
	}
	return ""
}

func isStr(v any) bool { _, ok := v.(string); return ok }

func strField(o *pyjson.Object, k string) string {
	v, _ := o.Get(k)
	s, _ := v.(string)
	return s
}

// ExitReason is resident.exit_reason. rc is negative for a signal.
func ExitReason(rc int, tail []string) string {
	what := fmt.Sprintf("exited %d", rc)
	if rc < 0 {
		what = fmt.Sprintf("killed by signal %d", -rc)
	}
	if len(tail) > 0 {
		if last := pystr.Slice(pystr.Strip(tail[len(tail)-1]), 0, 200); last != "" {
			return what + ": " + last
		}
	}
	return what
}

// ResidentConfig names the child: its argv, its environment (nil: this
// process's own), the name messages use, and the two timeouts in seconds.
type ResidentConfig struct {
	Name, Binary string
	Argv, Env    []string
	LoadTimeoutS float64
	ClipTimeoutS float64
}

type residentChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte // closed once the child has ended
	done   chan struct{}
	tailMu sync.Mutex
	tail   []string
	rc     int
	ready  bool
}

func (c *residentChild) kill() {
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
}

// reason is how the child ended. It waits until it has.
func (c *residentChild) reason() string {
	<-c.done
	c.tailMu.Lock()
	defer c.tailMu.Unlock()
	return ExitReason(c.rc, c.tail)
}

// Resident is resident.ResidentEngine.
type Resident struct {
	cfg    ResidentConfig
	mu     sync.Mutex
	cond   *sync.Cond
	clipMu sync.Mutex
	state  string // new, loading, ready, failed, stopped
	child  *residentChild
	why    string
}

// NewResident makes an engine that is not started.
func NewResident(cfg ResidentConfig) *Resident {
	if cfg.LoadTimeoutS == 0 {
		cfg.LoadTimeoutS = LoadTimeoutS
	}
	if cfg.ClipTimeoutS == 0 {
		cfg.ClipTimeoutS = ClipTimeoutS
	}
	r := &Resident{cfg: cfg, state: "new"}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func unavailable(msg string) *EngineError { return &EngineError{"unavailable", msg} }

// strerror is Python's OSError.strerror for an exec failure.
func strerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		s := errno.Error()
		return strings.ToUpper(s[:1]) + s[1:]
	}
	return err.Error()
}

func (r *Resident) spawn() (*residentChild, error) {
	cmd := exec.Command(r.cfg.Argv[0], r.cfg.Argv[1:]...)
	cmd.Env = r.cfg.Env
	if r.cfg.Env != nil {
		cmd.Env = append(os.Environ(), r.cfg.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &residentChild{cmd: cmd, stdin: stdin, lines: make(chan []byte, 64), done: make(chan struct{})}
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		rd := bufio.NewReader(stderr)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				s := pystr.DecodeReplace(bytes.TrimSuffix(line, []byte("\n")))
				if pystr.Strip(s) != "" {
					c.tailMu.Lock()
					c.tail = append(c.tail, s)
					if len(c.tail) > stderrLines {
						c.tail = c.tail[1:]
					}
					c.tailMu.Unlock()
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		rd := bufio.NewReader(stdout)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				c.lines <- line
			}
			if err != nil {
				break
			}
		}
		<-errDone
		cmd.Wait()
		rc := 0
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rc = -int(ws.Signal())
		} else {
			rc = cmd.ProcessState.ExitCode()
		}
		c.rc = rc
		close(c.done)
		close(c.lines)
		r.exited(c)
	}()
	return c, nil
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// load waits for the ready line: "" when the child is ready, else why not.
func (r *Resident) load(c *residentChild) string {
	select {
	case raw, ok := <-c.lines:
		if !ok {
			return c.reason()
		}
		o := ParseResidentLine(raw)
		if IsReadyLine(o) {
			return ""
		}
		c.kill()
		if o != nil {
			if v, _ := o.Get("error"); isStr(v) {
				return pystr.Slice(pystr.Strip(v.(string)), 0, 200)
			}
		}
		return "its first line was not the " + ResidentProtocol + " ready line"
	case <-time.After(secs(r.cfg.LoadTimeoutS)):
		c.kill()
		return fmt.Sprintf("it did not load its model in %s seconds", pyG(r.cfg.LoadTimeoutS))
	}
}

// Start starts the child and waits until it is ready.
func (r *Resident) Start() *EngineError {
	c, err := r.spawn()
	if err != nil {
		return unavailable(fmt.Sprintf("%s did not start: %s", r.cfg.Binary, strerror(err)))
	}
	r.mu.Lock()
	r.child, r.state = c, "loading"
	r.mu.Unlock()
	why := r.load(c)
	r.mu.Lock()
	if why == "" {
		c.ready = true
		r.state = "ready"
	} else {
		r.state, r.child = "stopped", nil
	}
	r.cond.Broadcast()
	r.mu.Unlock()
	if why != "" {
		return unavailable(fmt.Sprintf("%s did not start: %s", r.cfg.Binary, why))
	}
	return nil
}

// restart starts a new child in place of old, loading in the background,
// unless old is no longer the current child.
func (r *Resident) restart(old *residentChild) {
	r.mu.Lock()
	if r.state == "stopped" || r.child != old {
		r.mu.Unlock()
		return
	}
	if old != nil {
		old.kill()
	}
	c, err := r.spawn()
	if err != nil {
		r.child, r.state = nil, "failed"
		r.why = fmt.Sprintf("%s did not start: %s", r.cfg.Binary, strerror(err))
		r.cond.Broadcast()
		r.mu.Unlock()
		return
	}
	r.child, r.state = c, "loading"
	r.mu.Unlock()
	go func() {
		why := r.load(c)
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.child != c || r.state == "stopped" {
			return
		}
		if why == "" {
			c.ready = true
			r.state = "ready"
		} else {
			c.kill()
			r.child, r.state, r.why = nil, "failed", why
		}
		r.cond.Broadcast()
	}()
}

func (r *Resident) exited(c *residentChild) {
	r.mu.Lock()
	again := r.child == c && r.state == "ready" && c.ready
	r.mu.Unlock()
	if again {
		r.restart(c)
	}
}

// WaitReady waits while a child loads, and returns the state then.
func (r *Resident) WaitReady(timeoutS float64) string {
	end := time.Now().Add(secs(timeoutS))
	timer := time.AfterFunc(secs(timeoutS), func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	defer timer.Stop()
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.state == "loading" && time.Now().Before(end) {
		r.cond.Wait()
	}
	return r.state
}

func (r *Resident) lost(c *residentChild, what string) *EngineError {
	r.restart(c)
	return unavailable("The speech engine " + what + ". " + againMsg)
}

// TranscribeText is the child's text for one clip. The error is an
// *EngineError (Kind "loading" or "unavailable") or, for a clip the child
// refused, a plain error.
func (r *Resident) TranscribeText(wav []byte) (string, error) {
	r.clipMu.Lock()
	defer r.clipMu.Unlock()
	r.mu.Lock()
	state, c, why := r.state, r.child, r.why
	r.mu.Unlock()
	switch {
	case state == "loading":
		return "", &EngineError{"loading", loadingMsg}
	case state == "failed":
		r.restart(nil)
		return "", unavailable("The speech engine did not start again: " + why + ". The next clip tries again.")
	case state != "ready" || c == nil:
		return "", unavailable("The speech engine has stopped.")
	}
	// The clip timeout covers the write too: a child that stops reading
	// its stdin would block a clip bigger than the pipe.
	timer := time.NewTimer(secs(r.cfg.ClipTimeoutS))
	defer timer.Stop()
	wrote := make(chan struct{})
	go func() {
		c.stdin.Write(Frame(wav))
		close(wrote)
	}()
	timeout := func() error {
		return r.lost(c, fmt.Sprintf("did not answer in %s seconds", pyG(r.cfg.ClipTimeoutS)))
	}
	select {
	case <-wrote:
	case <-timer.C:
		return "", timeout()
	}
	select {
	case raw, ok := <-c.lines:
		if !ok {
			return "", r.lost(c, "stopped during a clip ("+c.reason()+")")
		}
		o := ParseResidentLine(raw)
		switch ReplyKind(o) {
		case "text":
			return strField(o, "text"), nil
		case "error":
			return "", fmt.Errorf("%s: %s", r.cfg.Binary, pystr.Slice(pystr.Strip(strField(o, "error")), 0, 200))
		}
		return "", r.lost(c, "gave a reply that is not one JSON object with text")
	case <-timer.C:
		return "", timeout()
	}
}

// Stop closes the child's stdin, waits a little, then kills it.
func (r *Resident) Stop() {
	r.mu.Lock()
	c := r.child
	r.state, r.child = "stopped", nil
	r.cond.Broadcast()
	r.mu.Unlock()
	if c == nil {
		return
	}
	c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(secs(StopGraceS)):
		c.kill()
		<-c.done
	}
}

// Pid is the current child's process id, or 0.
func (r *Resident) Pid() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.child == nil || r.child.cmd.Process == nil {
		return 0
	}
	return r.child.cmd.Process.Pid
}
