package warden

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/datahome"
	"daisugi-verify/internal/fenceruns"
)

// FencedEnv is the variable `daisugi fence` sets in the harness's
// environment. A warden hook that sees it runs inside the fence, where the
// warden's state is read only (FN-R-12).
const FencedEnv = "DAISUGI_FENCED"

// Fenced says whether environ marks a process `daisugi fence` started.
func Fenced(environ []string) bool {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, FencedEnv+"="); ok && v != "" {
			return true
		}
	}
	return false
}

// FencedRoot is the warden root a hook argv names (--root), else the data
// dir's warden, as warden_client._root_from_argv finds it.
func FencedRoot(argv, environ []string) string {
	for i, a := range argv {
		if a == "--root" && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--root="); ok {
			return v
		}
	}
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	home := env["HOME"]
	return datahome.Dir(func(k string) string { return env[k] }, home, datahome.Exists) + "/warden"
}

// residentSocketOK is warden_client._socket_is_trustworthy: a socket, not a
// link, owned by this uid, mode 0600.
func residentSocketOK(sock string) bool {
	var st syscall.Stat_t
	if err := syscall.Lstat(sock, &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFSOCK && int(st.Uid) == os.Getuid() && st.Mode&0o7777 == 0o600
}

// FencedCheck is `warden check` inside the fence: a thin client of the
// resident warden on <root>/warden.sock. The server runs outside the fence and
// makes every write (asks, audit, sessions, checkpoints), so the fence
// keeps the warden's state read only. Any failure to reach it, or a reply it
// cannot read, is the format's deny: inside the fence there is no other
// path to a verdict. An ask goes through the server too, with a deadline
// that covers the ask.
//
// A hook process denies on an interrupt or a terminate, as the guarded parent
// does: a host that times the hook out sends that signal while the resident
// warden is stuck, and a host reads no verdict as go on. The verdict is the
// mode's (ModeFor): audit allows with exit 0, enforce denies with exit 2.
func FencedCheck(argv []string, stdin []byte, environ []string) Result {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	return fencedCheckWith(argv, stdin, environ, sigCh)
}

// fencedCheckWith is FencedCheck with the signal channel given.
func fencedCheckWith(argv []string, stdin []byte, environ []string, sigCh <-chan os.Signal) Result {
	abort := make(chan struct{})
	done := make(chan Result, 1)
	go func() { done <- fencedExchange(argv, stdin, environ, abort) }()
	select {
	case res := <-done:
		return res
	case sig := <-sigCh:
		close(abort) // ends the wait on the socket
		f := DenyFormat(argv)
		if ModeFor(argv, environ) == "audit" {
			return AuditEscapeResult(f, "signal: "+sig.String())
		}
		res := DenyResult(f, "the fenced warden hook was ended by a signal ("+sig.String()+"), so it denies")
		res.Why = "signal: " + sig.String()
		return res
	}
}

// fencedExchange asks the resident warden and reads its reply. Closing abort
// ends a wait on the socket.
func fencedExchange(argv []string, stdin []byte, environ []string, abort <-chan struct{}) Result {
	sock := FencedRoot(argv, environ) + "/warden.sock"
	deny := func(why string) Result {
		return DenyResult(DenyFormat(argv), "the fenced warden hook cannot reach the resident warden ("+why+
			"), so it denies. Start it with: daisugi warden serve")
	}
	if !residentSocketOK(sock) {
		return deny("no private socket at " + sock)
	}
	// --root only found the socket: the server has its own root and takes
	// no path from a request (FN-R-21).
	req := map[string]any{"v": 1, "argv": fenceruns.StripPathFlags(argv),
		"stdin_b64": base64.StdEncoding.EncodeToString(stdin)}
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	if env["COPPICE_SOCK"] != "" && env["COPPICE_PANE"] != "" {
		req["coppice_sock"], req["coppice_pane"] = env["COPPICE_SOCK"], env["COPPICE_PANE"]
	}
	if h := env["HERDR_PANE_ID"]; h != "" {
		req["herdr_pane"] = h
	} else if h := env["HERDR_PANE"]; h != "" {
		req["herdr_pane"] = h
	}
	if d := env["COPPICE_DATA_DIR"]; strings.HasPrefix(d, "/") {
		req["coppice_data_dir"] = d
	}
	line, err := json.Marshal(req)
	if err != nil {
		return deny(err.Error())
	}
	deadline := time.Now().Add(ChildDeadline(argv) + 10*time.Second)
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return deny(err.Error())
	}
	defer c.Close()
	fin := make(chan struct{})
	defer close(fin)
	go func() {
		select {
		case <-abort:
			_ = c.Close()
		case <-fin:
		}
	}()
	_ = c.SetDeadline(deadline)
	if _, err := c.Write(append(line, '\n')); err != nil {
		return deny(err.Error())
	}
	var buf []byte
	chunk := make([]byte, 65536)
	for len(buf) < MaxRequestBytes {
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if n > 0 && buf[len(buf)-1] == '\n' {
			break
		}
		if err != nil {
			return deny("the server ended the connection with no reply")
		}
	}
	var reply struct {
		V        *int    `json:"v"`
		Stdout   *string `json:"stdout"`
		Stderr   *string `json:"stderr"`
		ExitCode *int    `json:"exit_code"`
	}
	if json.Unmarshal(buf, &reply) != nil || reply.V == nil || *reply.V != 1 ||
		reply.Stdout == nil || reply.Stderr == nil || reply.ExitCode == nil {
		return deny("a reply it cannot read")
	}
	res := Result{Exit: *reply.ExitCode}
	// warden_client.main prints each non-empty stream with a newline.
	if *reply.Stdout != "" {
		res.Stdout = *reply.Stdout + "\n"
	}
	if *reply.Stderr != "" {
		res.Stderr = *reply.Stderr + "\n"
	}
	return res
}
