package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"daisugi-verify/internal/pystr"
)

// A host reads anything but a deny contract as an allow: Claude Code
// treats every exit code other than 2 as allow, and hermes and openclaw
// read an empty stdout as allow. A Go runtime fatal error (a stack past
// its limit, running out of memory, a concurrent map write) cannot be
// recovered inside the process that hit it. So the binary decides each
// call in a child process of itself, and the parent turns any abnormal
// end of the child into the format's deny contract.

// ChildEnv marks the child process. It is removed from the environment
// the child decides with, so nothing the gate starts inherits it.
const ChildEnv = "DAISUGI_GATE_CHILD"

// DefaultDeadline bounds a child whose argv gives no budget to go by.
const DefaultDeadline = 13 * time.Second

// ChildDeadline is how long the parent waits for the child: the verify
// budget, plus the ask budget with --ask, plus 3 s. The installer gives
// the host hook at least the verify budget plus 5 s (the ask budget too
// with --ask), and a host that times a hook out lets the call through,
// so the child must end, and deny, first. The rules that run before the
// verifier have no budget of their own; this bounds them too.
func ChildDeadline(argv []string) (d time.Duration) {
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = pystr.FSDecode([]byte(a))
	}
	defer func() {
		if recover() != nil {
			d = DefaultDeadline
		}
	}()
	o := parseArgv(args, 80)
	budget := 10.0
	if o.verifyTimeout != nil {
		budget = *o.verifyTimeout
	}
	if o.ask {
		ask := 90.0
		if o.askTimeout != nil {
			ask = *o.askTimeout
		}
		budget += max(ask, 0)
	}
	if math.IsNaN(budget) || budget > 3600 {
		budget = 3600
	}
	budget = max(budget, 1) + 3
	return time.Duration(budget * float64(time.Second))
}

// frame is what the child writes to its stdout, and nothing else.
type frame struct {
	// The streams go as bytes: a stream may hold text that is not UTF-8.
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Exit   int    `json:"exit"`
	Native bool   `json:"native"`
	Why    string `json:"why"`
	Done   bool   `json:"done"`
	Nonce  string `json:"nonce"`
}

// frameFD is where the child writes its frame: a pipe only the parent
// holds, passed as the child's fd 3.
const frameFD = 3

// ChildPipe is the frame pipe of a child the parent started, or nil. A
// process is a child only when ChildEnv names a nonce AND fd 3 is a pipe:
// a host that merely sets ChildEnv gets the parent, never a process that
// prints no verdict and exits 0.
func ChildPipe(nonce string) *os.File {
	if nonce == "" {
		return nil
	}
	var st syscall.Stat_t
	if syscall.Fstat(frameFD, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return nil
	}
	// Nothing the gate starts may hold the frame pipe open: the parent
	// reads the end of the frame as the child closing it.
	syscall.CloseOnExec(frameFD)
	return os.NewFile(frameFD, "frame")
}

// ChildDeny is what a child writes on its own stdout and exit code: the
// format's deny. The verdict goes to the parent on the frame pipe only,
// so a child started by mistake reads as a deny to any host.
func ChildDeny(argv []string) Result {
	return DenyResult(DenyFormat(argv), "this process decided the call for its parent gate, which gives the verdict")
}

// ChildFrame decides one call in the child and returns the bytes to write
// to its stdout. A panic here still produces a deny frame; a fatal error
// produces none, and the parent denies.
func ChildFrame(argv []string, stdin []byte, environ []string) (out []byte) {
	env := make([]string, 0, len(environ))
	nonce := ""
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, ChildEnv+"="); ok {
			nonce = v
			continue
		}
		env = append(env, kv)
	}
	defer func() {
		if p := recover(); p != nil {
			res := DenyResult(DenyFormat(argv), fmt.Sprintf("the Go gate failed on this call (%v), so it denies it", p))
			out = encodeFrame(res, fmt.Sprintf("panic: %v", p), nonce)
		}
	}()
	testCrash(env)
	res := Run(argv, stdin, env)
	return encodeFrame(res, res.Why, nonce)
}

func encodeFrame(res Result, why, nonce string) []byte {
	b, _ := json.Marshal(frame{Stdout: []byte(res.Stdout), Stderr: []byte(res.Stderr), Exit: res.Exit,
		Native: res.Native, Why: why, Done: true, Nonce: nonce})
	return b
}

// ChildEnviron is environ as the child gets it: the width of the terminal
// on this process's stdout, when there is one, rides along, since the
// child's own stdout is a pipe and argparse wraps its usage to that width.
func ChildEnviron(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, ttyColumnsEnv+"=") {
			out = append(out, kv)
		}
	}
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, 1, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e == 0 {
		out = append(out, fmt.Sprintf("%s=%d", ttyColumnsEnv, ws.Col))
	}
	return out
}

// RunGuarded decides one call in a child process of exe (this binary) and
// returns its result. Anything but a complete frame from a child that
// exited 0 is a deny: a signal, a fatal error, a crash before the frame, a
// frame cut short, or a child past the deadline, which is killed.
func RunGuarded(exe string, argv []string, stdin []byte, environ []string, deadline time.Duration) Result {
	return RunGuardedAs(exe, nil, argv, stdin, environ, deadline)
}

// RunGuardedAs is RunGuarded for a binary whose child is started with
// sub before the gate's argv (the daisugi CLI's child runs
// `daisugi gate check ...`). The child must answer with ChildFrame when
// ChildEnv is set.
func RunGuardedAs(exe string, sub, argv []string, stdin []byte, environ []string, deadline time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append(append([]string{}, sub...), argv...)...)
	nonce := randomHex(16)
	cmd.Env = append(append([]string{}, environ...), ChildEnv+"="+nonce)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	fail := func(why string) Result {
		res := DenyResult(DenyFormat(argv), "the Go gate ended abnormally ("+why+"), so it denies this call")
		res.Why = why
		res.Crashed = true
		return res
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail("no frame pipe: " + err.Error())
	}
	cmd.ExtraFiles = []*os.File{pw}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fail(err.Error())
	}
	pw.Close()
	frameCh := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(pr)
		pr.Close()
		frameCh <- b
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	// The verdict is the frame alone. The child's own exit code is the
	// format's deny (ChildDeny), so it is read only to say why a child
	// that wrote no frame ended.
	var f frame
	complete := func(raw []byte) bool {
		f = frame{}
		return json.Unmarshal(raw, &f) == nil && f.Done && f.Nonce == nonce
	}
	var raw []byte
	select {
	case raw = <-frameCh:
		// The child closes the pipe once its frame is written, and has
		// done all its work by then: only its own deny and its exit are
		// left. A complete frame is the answer at once; the child is
		// killed, so it cannot outlive this process.
		if ctx.Err() == nil && complete(raw) {
			_ = cmd.Process.Kill()
			return Result{Stdout: string(f.Stdout), Stderr: string(f.Stderr), Exit: f.Exit, Native: f.Native, Why: f.Why}
		}
		err = <-waitCh
	case err = <-waitCh:
		select {
		case raw = <-frameCh:
		case <-time.After(time.Second):
			// A grandchild holding the pipe open: no complete frame.
		}
	}
	if ctx.Err() != nil {
		return fail(fmt.Sprintf("no answer within %s", deadline))
	}
	if !complete(raw) {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			line := firstLine(strings.TrimSpace(stderr.String()))
			if len(line) > 200 {
				line = line[:200]
			}
			return fail(fmt.Sprintf("%s: %s", ee.ProcessState.String(), line))
		}
		if err != nil {
			return fail(err.Error())
		}
		return fail("the child wrote no complete answer")
	}
	return Result{Stdout: string(f.Stdout), Stderr: string(f.Stderr), Exit: f.Exit, Native: f.Native, Why: f.Why}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// testCrash makes the child end abnormally on request, so the tests can
// see the parent deny. It can only turn a call into a deny.
func testCrash(env []string) {
	mode := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "DAISUGI_GATE_TEST_CRASH="); ok {
			mode = v
		}
	}
	switch mode {
	case "exit0":
		os.Exit(0)
	case "exit1":
		os.Exit(1)
	case "fatal":
		// A concurrent map write is a fatal error no recover can catch.
		m := map[int]int{}
		done := make(chan struct{})
		for g := 0; g < 4; g++ {
			go func() {
				for i := 0; ; i++ {
					m[i%64] = i
				}
			}()
		}
		<-done
	case "stack":
		var deep func(int) int
		deep = func(n int) int { var pad [1024]byte; return deep(n+1) + int(pad[n%1024]) }
		deep(0)
	case "hang":
		select {}
	case "panic":
		panic("test panic")
	}
}
