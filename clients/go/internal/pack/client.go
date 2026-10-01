package pack

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/voice"
)

// The caller side of daisugi-pack-1 (opendaisugi.pack.client).

const (
	loadTimeout  = 300 * time.Second
	stopGrace    = 2 * time.Second
	stderrLines  = 20
	maxReplyLine = 256 << 20
)

// Outcome is client.Outcome: Code 0 with Result, or 1 with Error.
type Outcome struct {
	Code   int
	Result *pyjson.Object
	Error  string
}

// WorkerArgv is client.worker_argv.
func WorkerArgv(packDir, name string) []string {
	return []string{
		filepath.Join(packDir, "venv", "bin", "python"),
		"-I",
		filepath.Join(packDir, "worker", WorkerFile),
		"--pack",
		name,
	}
}

// Request is worker.request: the frame of one job.
func Request(job string, args []string) []byte {
	list := make([]any, len(args))
	for i, a := range args {
		list[i] = a
	}
	body := pyjson.Dumps(pyjson.NewObject().Set("job", job).Set("args", list), true)
	return voice.Frame([]byte(body))
}

// ReplyKind is worker.reply_kind.
func ReplyKind(o *pyjson.Object) string {
	if o == nil {
		return ""
	}
	isStr := func(k string) bool { v, ok := o.Get(k); _, s := v.(string); return ok && s }
	if isStr("ready") {
		return "ready"
	}
	if isStr("progress") {
		return "progress"
	}
	if v, ok := o.Get("result"); ok {
		if _, isObj := v.(*pyjson.Object); isObj {
			return "result"
		}
	}
	if isStr("error") {
		return "error"
	}
	return ""
}

func strOf(o *pyjson.Object, k string) string { s, _ := o.Value(k).(string); return s }

// RunJob is client.run_job. env nil is this process's environment.
func RunJob(packDir, name, job string, args []string, onProgress func(string), env []string) Outcome {
	argv := WorkerArgv(packDir, name)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	stderr, err3 := cmd.StderrPipe()
	if err1 != nil || err2 != nil || err3 != nil || cmd.Start() != nil {
		return Outcome{1, nil, fmt.Sprintf("pack %s: cannot start the worker (%s)", name, argv[0])}
	}
	var mu sync.Mutex
	var tail []string
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), maxReplyLine)
		for sc.Scan() {
			text := strings.ToValidUTF8(sc.Text(), "�")
			if strings.TrimSpace(text) != "" {
				mu.Lock()
				tail = append(tail, text)
				if len(tail) > stderrLines {
					tail = tail[1:]
				}
				mu.Unlock()
			}
		}
	}()
	lines := make(chan []byte, 64)
	go func() {
		r := bufio.NewReaderSize(stdout, 64*1024)
		for {
			b, err := r.ReadBytes('\n')
			if len(b) > 0 {
				lines <- b
			}
			if err != nil {
				break
			}
		}
		close(lines)
	}()
	wait := func() int {
		err := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return -int(ws.Signal())
			}
			return ee.ExitCode()
		}
		return 0
	}
	dead := func() Outcome {
		// Read stderr to its end before Wait closes the pipe.
		select {
		case <-errDone:
		case <-time.After(5 * time.Second):
		}
		rc := wait()
		mu.Lock()
		defer mu.Unlock()
		return Outcome{1, nil, fmt.Sprintf("pack %s: the worker %s", name, voice.ExitReason(rc, tail))}
	}
	kill := func() {
		_ = cmd.Process.Kill()
		wait()
	}
	stop := func() {
		stdin.Close()
		done := make(chan struct{})
		go func() { wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(stopGrace):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	notAReply := func(raw []byte) Outcome {
		kill()
		text := strings.TrimSpace(strings.ToValidUTF8(string(raw), "�"))
		if r := []rune(text); len(r) > 80 {
			text = string(r[:80])
		}
		return Outcome{1, nil, fmt.Sprintf("pack %s: the worker wrote a line that is not a reply: %s", name, text)}
	}
	var first []byte
	select {
	case b, ok := <-lines:
		if !ok {
			return dead()
		}
		first = b
	case <-time.After(loadTimeout):
		kill()
		return Outcome{1, nil, fmt.Sprintf("pack %s: the worker was not ready in %g s", name, loadTimeout.Seconds())}
	}
	obj := voice.ParseResidentLine(first)
	if ReplyKind(obj) != "ready" {
		return notAReply(first)
	}
	if got := strOf(obj, "ready"); got != Protocol {
		kill()
		return Outcome{1, nil, fmt.Sprintf("pack %s: the worker speaks %s, not %s. Install it again: daisugi pack install %s --force", name, got, Protocol, name)}
	}
	_, _ = stdin.Write(Request(job, args))
	for raw := range lines {
		obj := voice.ParseResidentLine(raw)
		switch ReplyKind(obj) {
		case "progress":
			onProgress(strOf(obj, "progress"))
		case "result":
			stop()
			res, _ := obj.Value("result").(*pyjson.Object)
			return Outcome{0, res, ""}
		case "error":
			stop()
			return Outcome{1, nil, fmt.Sprintf("pack %s: %s: %s", name, job, strOf(obj, "error"))}
		default:
			return notAReply(raw)
		}
	}
	return dead()
}
