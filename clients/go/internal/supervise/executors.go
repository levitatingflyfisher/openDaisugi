package supervise

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Executor is executor.StepExecutor. An error is an exception from the
// executor, which the supervisor records as a failed step.
type Executor interface {
	Run(step *pyjson.Object, timeoutS, maxOutputBytes int) (ExecResult, error)
}

// Configurable is an executor with configure_from_envelope.
type Configurable interface {
	Configure(env *pyjson.Object)
}

// maxReversalBytes is executor.MAX_REVERSAL_BYTES.
const maxReversalBytes = 1_000_000

func ms(start time.Time) float64 { return float64(time.Since(start).Nanoseconds()) / 1e6 }

// TruncateOutput is executor.truncate_output.
func TruncateOutput(text string, maxBytes int) string {
	b, exc := pystr.EncodeUTF8(text)
	if exc != nil || len(b) <= maxBytes {
		return text
	}
	return pystr.DecodeReplace(b[:maxBytes]) + "\n... [truncated]"
}

// pyExcName is the OSError subclass Python raises for an errno.
func pyExcName(errno syscall.Errno) string {
	switch errno {
	case syscall.ENOENT:
		return "FileNotFoundError"
	case syscall.EISDIR:
		return "IsADirectoryError"
	case syscall.ENOTDIR:
		return "NotADirectoryError"
	case syscall.EACCES, syscall.EPERM:
		return "PermissionError"
	case syscall.EEXIST:
		return "FileExistsError"
	case syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	case syscall.ECONNRESET:
		return "ConnectionResetError"
	}
	return "OSError"
}

func strerror(e syscall.Errno) string {
	s := e.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// pyOSError is (type(e).__name__, str(e)) for an OSError on path; ok is
// false for an error that carries no errno.
func pyOSError(err error, path string) (name, text string, ok bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", "", false
	}
	return pyExcName(errno), fmt.Sprintf("[Errno %d] %s: %s", int(errno), strerror(errno), pystr.Repr(path)), true
}

// DryRun is executor.DryRunExecutor.
type DryRun struct{}

func (DryRun) Run(step *pyjson.Object, _, _ int) (ExecResult, error) {
	var msg string
	switch str(step, "type") {
	case "shell":
		msg = "[dry-run] would shell: " + pystr.Repr(str(step, "command"))
	case "file_read":
		msg = "[dry-run] would file_read: " + str(step, "path")
	case "file_write":
		b, _ := pystr.EncodeUTF8(str(step, "content"))
		msg = fmt.Sprintf("[dry-run] would file_write: %s (%d bytes)", str(step, "path"), len(b))
	case "network":
		msg = "[dry-run] would network: GET " + str(step, "url")
	case "task":
		msg = "[dry-run] would task (delegate to LLM): " + pystr.Repr(str(step, "prompt"))
	case "skill":
		msg = "[dry-run] would skill: " + str(step, "skill_id") + " input=" + pmodel.Repr(step.Value("skill_input"))
	case "mcp":
		msg = "[dry-run] would mcp: " + str(step, "server") + "/" + str(step, "tool") + " args=" + pmodel.Repr(step.Value("arguments"))
	default:
		msg = "[dry-run] unknown step kind: " + pystr.Repr(str(step, "type"))
	}
	return ExecResult{RC: 0, Stdout: msg}, nil
}

// globBase is executor._glob_base.
func globBase(glob string) string {
	cut := -1
	for _, c := range "*?[" {
		if i := strings.IndexRune(glob, c); i != -1 && (cut == -1 || i < cut) {
			cut = i
		}
	}
	if cut == -1 {
		return glob
	}
	prefix := glob[:cut]
	base := prefix
	if i := strings.LastIndex(prefix, "/"); i != -1 {
		base = prefix[:i]
	}
	if base == "" {
		return "/"
	}
	return base
}

// realpath is os.path.realpath (non-strict): symlinks resolved as far as
// the path exists, the rest joined as written.
func realpath(p string) string {
	abs := p
	if !filepath.IsAbs(abs) {
		wd, err := os.Getwd()
		if err == nil {
			abs = filepath.Join(wd, p)
		}
	}
	abs = filepath.Clean(abs)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	// Resolve the longest prefix that exists, then append the rest.
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		name := filepath.Base(dir)
		if rest == "" {
			rest = name
		} else {
			rest = name + "/" + rest
		}
		if parent == dir {
			return abs
		}
		dir = parent
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
	}
}

// resolvedPathEscape is executor._resolved_path_escape.
func resolvedPathEscape(target string, globs []string) string {
	real := realpath(target)
	for _, g := range globs {
		base := realpath(globBase(g))
		prefix := strings.TrimRight(base, "/") + "/"
		if real == base || strings.HasPrefix(real, prefix) {
			return ""
		}
	}
	return fmt.Sprintf("resolved path %s is outside permitted globs %s (symlink escape)", pystr.Repr(real), pystr.ReprList(globs))
}

func globsOf(env *pyjson.Object, key string) []string {
	perms, _ := env.Value("permissions").(*pyjson.Object)
	var out []string
	if perms != nil {
		for _, g := range asList(perms.Value(key)) {
			s, _ := g.(string)
			out = append(out, s)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// FileRead is executor.FileReadExecutor.
type FileRead struct{ globs []string }

func (f *FileRead) Configure(env *pyjson.Object) { f.globs = globsOf(env, "file_read") }

func (f *FileRead) Run(step *pyjson.Object, _, maxOut int) (ExecResult, error) {
	start := time.Now()
	path := str(step, "path")
	if f.globs != nil {
		if esc := resolvedPathEscape(path, f.globs); esc != "" {
			return ExecResult{RC: 2, Stdout: "file_read refused: " + esc, DurationMs: ms(start)}, nil
		}
	}
	data, err := readCapped(path, maxOut)
	if err != nil {
		name, text, ok := pyOSError(err, path)
		if !ok {
			return ExecResult{}, err
		}
		switch name {
		case "FileNotFoundError", "IsADirectoryError", "PermissionError":
			return ExecResult{RC: 1, Stdout: name + ": " + text, DurationMs: ms(start)}, nil
		}
		return ExecResult{}, errors.New(text)
	}
	out := pystr.DecodeReplace(data.buf)
	if data.truncated {
		out += "\n... [truncated]"
	}
	return ExecResult{RC: 0, Stdout: out, DurationMs: ms(start)}, nil
}

type capped struct {
	buf       []byte
	truncated bool
}

// readCapped is FileReadExecutor's chunked read: at most maxOut bytes, and
// whether there was more. A directory fails with EISDIR, as open() does.
func readCapped(path string, maxOut int) (capped, error) {
	fd, err := os.Open(path)
	if err != nil {
		return capped{}, err
	}
	defer fd.Close()
	if st, err := fd.Stat(); err == nil && st.IsDir() {
		return capped{}, syscall.EISDIR
	}
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 64*1024)
	for {
		want := min(64*1024, maxOut-len(buf)+1)
		n, err := fd.Read(chunk[:want])
		buf = append(buf, chunk[:n]...)
		if len(buf) > maxOut {
			return capped{buf[:maxOut], true}, nil
		}
		if err == io.EOF || (n == 0 && err == nil) {
			return capped{buf, false}, nil
		}
		if err != nil {
			return capped{}, err
		}
	}
}

// FileWrite is executor.FileWriteExecutor.
type FileWrite struct{ globs []string }

func (f *FileWrite) Configure(env *pyjson.Object) { f.globs = globsOf(env, "file_write") }

func (f *FileWrite) Run(step *pyjson.Object, _, _ int) (ExecResult, error) {
	start := time.Now()
	path := str(step, "path")
	refused := func(msg string) (ExecResult, error) {
		return ExecResult{RC: 2, Stdout: msg, DurationMs: ms(start), Reversibility: strp("none")}, nil
	}
	if f.globs != nil {
		if esc := resolvedPathEscape(path, f.globs); esc != "" {
			return refused("file_write refused: " + esc)
		}
	}
	parent := pyDirname(path)
	if parent == "" {
		parent = "."
	}
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return refused("symlink at target rejected: " + path)
	}
	osErr := func(err error, target string, tmp string) (ExecResult, error) {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
		name, text, ok := pyOSError(err, target)
		if !ok {
			name, text = "OSError", err.Error()
		}
		return ExecResult{RC: 1, Stdout: name + ": " + text, DurationMs: ms(start), Reversibility: strp("irreversible")}, nil
	}
	reversal, reversible := capturePreImage(path, parent)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		var pe *os.PathError
		target := parent
		if errors.As(err, &pe) {
			target = pe.Path
		}
		return osErr(err, target, "")
	}
	var rb [8]byte
	_, _ = rand.Read(rb[:])
	tmp := filepath.Join(parent, ".daisugi-tmp-"+hex.EncodeToString(rb[:])+"-"+filepath.Base(path))
	fd, err := syscall.Open(tmp, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return osErr(err, tmp, "")
	}
	data, _ := pystr.EncodeUTF8(str(step, "content"))
	written, werr := syscall.Write(fd, data)
	if werr == nil {
		werr = syscall.Fsync(fd)
	}
	_ = syscall.Close(fd)
	if werr != nil {
		return osErr(werr, tmp, tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		var le *os.LinkError
		if errors.As(err, &le) {
			return renameErr(le, tmp, path, start)
		}
		return osErr(err, path, tmp)
	}
	parentReal := realpath(parent)
	finalReal := realpath(path)
	if !(finalReal == parentReal || strings.HasPrefix(finalReal, parentReal+"/")) {
		_ = os.Remove(finalReal)
		return ExecResult{RC: 2, Stdout: fmt.Sprintf("symlink escape detected: %s -> %s", path, finalReal),
			DurationMs: ms(start), Reversibility: strp("irreversible")}, nil
	}
	res := ExecResult{RC: 0, Stdout: fmt.Sprintf("wrote %d bytes to %s", written, path), DurationMs: ms(start)}
	if reversible {
		res.Reversibility, res.Reversal = strp("reversible"), reversal
	} else {
		res.Reversibility = strp("irreversible")
	}
	return res, nil
}

// renameErr is the OSError os.rename raises: its message names both paths.
func renameErr(le *os.LinkError, tmp, path string, start time.Time) (ExecResult, error) {
	_ = os.Remove(tmp)
	var errno syscall.Errno
	name, text := "OSError", le.Error()
	if errors.As(le.Err, &errno) {
		name = pyExcName(errno)
		text = fmt.Sprintf("[Errno %d] %s: %s -> %s", int(errno), strerror(errno), pystr.Repr(tmp), pystr.Repr(path))
	}
	return ExecResult{RC: 1, Stdout: name + ": " + text, DurationMs: ms(start), Reversibility: strp("irreversible")}, nil
}

// pyDirname is os.path.dirname.
func pyDirname(p string) string {
	i := strings.LastIndex(p, "/") + 1
	head := p[:i]
	if head != "" && head != strings.Repeat("/", len(head)) {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// dirsToCreate is executor._dirs_to_create.
func dirsToCreate(parent string) []any {
	dirs := []any{}
	p := absPath(parent)
	for p != "" {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			break
		}
		dirs = append(dirs, p)
		next := filepath.Dir(p)
		if next == p {
			break
		}
		p = next
	}
	return dirs
}

// absPath is os.path.abspath.
func absPath(p string) string {
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = filepath.Join(wd, p)
		}
	}
	return filepath.Clean(p)
}

// capturePreImage is FileWriteExecutor._capture_pre_image.
func capturePreImage(path, parent string) (*pyjson.Object, bool) {
	created := dirsToCreate(parent)
	if _, err := os.Stat(path); err != nil {
		return reversalHandle(path, false, nil, created), true
	}
	fd, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(fd, maxReversalBytes+1))
	fd.Close()
	if err != nil || len(raw) > maxReversalBytes {
		return nil, false
	}
	prior, exc := pystr.DecodeStrict(raw)
	if exc != nil {
		return nil, false
	}
	return reversalHandle(path, true, &prior, []any{}), true
}

func reversalHandle(path string, existed bool, prior *string, created []any) *pyjson.Object {
	return pyjson.NewObject().Set("kind", "file_write").Set("path", path).Set("prior_existed", existed).
		Set("prior_content", sp(prior)).Set("created_dirs", created).Set("note", "")
}

// Network is executor.NetworkExecutor: one GET, no redirect followed.
type Network struct{}

func (Network) Run(step *pyjson.Object, timeoutS, maxOut int) (ExecResult, error) {
	start := time.Now()
	raw := str(step, "url")
	u, err := url.Parse(raw)
	scheme := ""
	if err == nil {
		scheme = strings.ToLower(u.Scheme)
	}
	if scheme != "http" && scheme != "https" {
		return ExecResult{RC: 2, Stdout: fmt.Sprintf("refused non-http(s) URL scheme %s: %s", pystr.Repr(scheme), raw),
			DurationMs: ms(start)}, nil
	}
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return ExecResult{}, err
	}
	if h, ok := step.Value("headers").(*pyjson.Object); ok {
		for _, k := range h.Keys() {
			v, _ := h.Value(k).(string)
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{
		Timeout:       time.Duration(timeoutS) * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{Proxy: nil, DisableCompression: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return ExecResult{RC: 2, Stdout: "TimeoutError: timed out", DurationMs: ms(start), TimedOut: true}, nil
		}
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return ExecResult{RC: 2, Stdout: fmt.Sprintf("URLError: [Errno %d] %s", int(errno), strerror(errno)),
				DurationMs: ms(start)}, nil
		}
		return ExecResult{RC: 2, Stdout: "URLError: " + err.Error(), DurationMs: ms(start)}, nil
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(maxOut)+1))
	reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
	if resp.StatusCode >= 300 {
		out := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, reason)
		if len(body) > 0 {
			out += "\n" + pystr.DecodeReplace(body)
		}
		return ExecResult{RC: 1, Stdout: out, DurationMs: ms(start)}, nil
	}
	if rerr != nil {
		var ne net.Error
		if errors.As(rerr, &ne) && ne.Timeout() {
			return ExecResult{RC: 2, Stdout: "TimeoutError: timed out", DurationMs: ms(start), TimedOut: true}, nil
		}
		return ExecResult{}, rerr
	}
	out := ""
	if len(body) > maxOut {
		out = pystr.DecodeReplace(body[:maxOut]) + "\n... [truncated]"
	} else {
		out = pystr.DecodeReplace(body)
	}
	return ExecResult{RC: 0, Stdout: out, DurationMs: ms(start)}, nil
}

// Shell is executor.SubprocessExecutor: /bin/sh -c in a new session,
// stderr merged into stdout, the group killed on a timeout or when the
// output passes its cap.
type Shell struct {
	// Environ is the child's environment; nil is this process's.
	Environ []string
}

func (sh Shell) Run(step *pyjson.Object, timeoutS, maxOut int) (ExecResult, error) {
	start := time.Now()
	cmd := exec.Command("/bin/sh", "-c", str(step, "command"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = sh.Environ
	pr, pw, err := os.Pipe()
	if err != nil {
		return ExecResult{}, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return ExecResult{}, err
	}
	pw.Close()
	pgid := cmd.Process.Pid
	type readOut struct {
		buf       []byte
		truncated bool
	}
	got := make(chan readOut, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 64*1024)
		for len(buf) < maxOut {
			n, err := pr.Read(chunk[:min(64*1024, maxOut-len(buf))])
			buf = append(buf, chunk[:n]...)
			if err != nil {
				got <- readOut{buf, false}
				return
			}
		}
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		got <- readOut{buf, true}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	select {
	case <-done:
	case <-time.After(time.Duration(timeoutS) * time.Second):
		timedOut = true
		killGroup(pgid, done)
	}
	var out readOut
	select {
	case out = <-got:
	case <-time.After(2 * time.Second):
	}
	pr.Close()
	rc := -1
	if st := cmd.ProcessState; st != nil {
		rc = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rc = -int(ws.Signal())
		}
	}
	text := pystr.DecodeReplace(out.buf)
	if out.truncated {
		text += "\n... [truncated]"
	}
	return ExecResult{RC: rc, Stdout: text, DurationMs: ms(start), TimedOut: timedOut}, nil
}

// killGroup is SubprocessExecutor._kill_group: SIGTERM to the group, two
// seconds, then SIGKILL.
func killGroup(pgid int, done chan error) {
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		<-done
		return
	}
	select {
	case <-done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-done
}

// DefaultExecutors is executor.default_executors.
func DefaultExecutors() map[string]Executor {
	return map[string]Executor{
		"shell":      Shell{},
		"file_read":  &FileRead{},
		"file_write": &FileWrite{},
		"network":    Network{},
	}
}

// CapturePreImage is FileWriteExecutor._capture_pre_image, for the batch
// proof's reversibility probe: the reversal handle and whether a write to
// path could be undone.
func CapturePreImage(path, parent string) (*pyjson.Object, bool) {
	return capturePreImage(path, parent)
}

// PyOSError is (type(e).__name__, str(e)) for an OSError on path.
func PyOSError(err error, path string) (name, text string, ok bool) { return pyOSError(err, path) }

// PyDirname is os.path.dirname.
func PyDirname(p string) string { return pyDirname(p) }
