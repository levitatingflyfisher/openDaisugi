package supervise

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// runSprig is AgenticExecutor._run_sprig_step: the step run as the sprig
// binary in its workspace, with the gate in --gate-cmd pinned to root and
// session, and the wall in --tools. The texts of a failed run are the
// oracle's own.
func (a *Agentic) runSprig(step *pyjson.Object, allowed []string, workspace, root, session string,
	timeoutS, maxOutputBytes int, started time.Time) (ExecResult, error) {
	fail := func(msg string) (ExecResult, error) {
		return ExecResult{RC: 1, Stdout: TruncateOutput(msg, maxOutputBytes), DurationMs: ms(started)}, nil
	}
	// The gate is pinned by its own command (--root, --session), never by
	// the payload sprig sends.
	gate := []string{a.Self, "gate", "check", "--mode", "enforce", "--root", root, "--session", session,
		"--captures-root", gateroot.Join(root, "captures")}
	var wall []string
	for _, t := range allowed {
		name := sprigTools[t]
		dup := false
		for _, w := range wall {
			dup = dup || w == name
		}
		if !dup {
			wall = append(wall, name)
		}
	}
	argv := []string{"--json", "--gate", "--gate-cmd", strings.Join(gate, " "), "--tools", strings.Join(wall, ","),
		"--model", a.Model, "--session-dir", gateroot.Join(root, "sessions"), "--session", session}
	if mt, ok := step.Value("max_turns").(pyjson.Int); ok {
		argv = append(argv, "--max-turns", mt.Text)
	}
	argv = append(argv, "-")

	stdin, perr := pystr.FSEncode(str(step, "prompt"))
	if perr != nil {
		stdin = []byte(str(step, "prompt"))
	}
	bin, err := a.Sprig, error(nil)
	if a.LookPath != nil {
		bin, err = a.LookPath(a.Sprig)
	}
	var rc int
	var out, errb []byte
	var timedOut bool
	if err == nil {
		rc, out, errb, timedOut, err = runGroup(bin, argv, workspace, a.Environ, stdin, timeoutS)
	}
	if err != nil {
		return fail("agentic sub-agent failed: cannot start sprig (" + a.Sprig + ")")
	}
	if timedOut {
		return fail(fmt.Sprintf("agentic sub-agent failed: sprig ran past %ds and was killed", timeoutS))
	}
	if rc != 0 {
		return fail(sprigExitText(rc, errb))
	}
	raw := pystr.DecodeReplace(out)
	parsed, derr := pyjson.LoadsPy(raw, 900)
	if derr != nil {
		return fail("agentic sub-agent returned unparseable output: " + pystr.Repr(pystr.Slice(raw, 0, 300)))
	}
	obj, _ := parsed.(*pyjson.Object)
	var answer string
	isStr := false
	if obj != nil {
		answer, isStr = obj.Value("answer").(string)
	}
	if !isStr {
		return fail("agentic sub-agent reply has no answer: " + pystr.Repr(pystr.Slice(raw, 0, 300)))
	}
	return ExecResult{RC: 0, Stdout: TruncateOutput(answer, maxOutputBytes), DurationMs: ms(started)}, nil
}

// sprigExitText is _sprig_exit_text: the exit code (or the signal) and the
// last line sprig wrote to stderr.
func sprigExitText(rc int, stderr []byte) string {
	if rc < 0 {
		return fmt.Sprintf("agentic sub-agent failed: sprig was killed by signal %d", -rc)
	}
	text := fmt.Sprintf("agentic sub-agent failed: sprig exited with code %d", rc)
	var last string
	for _, ln := range strings.Split(pystr.DecodeReplace(stderr), "\n") {
		if s := pystr.Strip(ln); s != "" {
			last = s
		}
	}
	if last != "" {
		text += ": " + pystr.Slice(last, 0, 500)
	}
	return text
}

// sprigDrainS is SPRIG_DRAIN_S: how long the output is read after the
// timeout kill.
const sprigDrainS = 2

// runGroup runs name in its own process group and kills the whole group
// at the timeout, so a tool or gate process it started dies with it. It
// returns the exit code (minus the signal for a killed process), stdout,
// stderr and whether the timeout fired; err when the program cannot start.
func runGroup(name string, args []string, dir string, environ []string, stdin []byte,
	timeoutS int) (int, []byte, []byte, bool, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Wait returns once sprig has exited and its pipes are closed. A
	// process sprig left that holds a pipe keeps it waiting until the
	// timeout kills the group: a timeout, as in the oracle (communicate
	// with a timeout).
	if err := cmd.Start(); err != nil {
		return 0, nil, nil, false, err
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	select {
	case <-done:
	case <-time.After(time.Duration(timeoutS) * time.Second):
		timedOut = true
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// A process that left the group can still hold a pipe: wait for
		// the rest of the output a short, fixed time, then stop waiting.
		// Wait's goroutine is left to end when that process does; its
		// buffers are not read.
		select {
		case <-done:
		case <-time.After(sprigDrainS * time.Second):
			return -1, nil, nil, true, nil
		}
	}
	rc := 0
	if st := cmd.ProcessState; st != nil {
		rc = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rc = -int(ws.Signal())
		}
	}
	return rc, out.Bytes(), errb.Bytes(), timedOut, nil
}
