package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"daisugi-verify/internal/gate"
	"daisugi-verify/internal/gateroot"
)

// maxResidentChildren bounds the calls decided at once. Each is a child
// process of this binary; a call past the bound waits for a free one.
const maxResidentChildren = 32

// gateServe is `daisugi gate serve`: the resident gate (gate_server.py).
// One JSON request line per connection on <root>/gate.sock, one reply
// line, then the connection is closed. Each gate call is decided in a
// child process of this binary, so no crash of one call ends the server
// or reads as an allow.
func (e *Env) gateServe(args []string) error {
	opts := []opt{rootOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("gate serve", err)
	}
	if p.help {
		return e.cmdHelp("gate serve", "", "Run the resident gate in the foreground (Ctrl-C to stop).", opts)
	}
	root := e.root(p)
	sock := gateroot.Join(root, "gate.sock")
	// A live gate keeps its socket; a second server never takes it over.
	if probeGate(sock) == gateLive {
		e.errf("gate: a resident gate already answers on %s; stop it first\n", sock)
		return exit(1)
	}
	e.errf("gate: serving on %s (Ctrl-C to stop)\n", sock)
	ln, err := listenGate(root, sock)
	if err != nil {
		e.errf("daisugi gate serve: %s\n", err)
		return exit(1)
	}
	var inode uint64
	if st, err := os.Lstat(sock); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			inode = sys.Ino
		}
	}
	// This process serves every session, so it is no one pane: the calls
	// it decides and the reports it sends never read these.
	childEnv := gate.PaneFreeEnviron(e.Environ)
	stopped := make(chan struct{})
	// Ctrl-C prints a newline and ends the server, as the typer command's
	// KeyboardInterrupt does; SIGTERM ends it with no newline. Either way
	// the socket goes with it: one left behind reads as a running gate to
	// the next `start`. A SIGINT this process was started with ignored
	// stays ignored, as in Python. A SIGTERM ignored at start does not:
	// the Go runtime does not keep it ignored, so no server does.
	watch := []os.Signal{syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGINT) {
		watch = append(watch, syscall.SIGINT)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, watch...)
	go func() {
		if <-sig == syscall.SIGINT {
			e.errf("\n")
		}
		close(stopped)
		ln.Close()
	}()
	defer unlinkIfOurs(sock, inode)
	slots := make(chan struct{}, maxResidentChildren)
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			select {
			case <-stopped:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go e.serveConn(c, childEnv, slots)
	}
}

// What probeGate finds at the socket path.
const (
	gateNone  = "none"
	gateLive  = "live"
	gateStale = "stale"
)

// probeGate is gate_server.probe: nothing at the path, a server that
// accepts a connection (or whose queue is full), or something nobody
// listens on (the socket of a dead server, a regular file). It sends an
// empty request and reads the reply, so a Python server never writes to
// a client that already left.
func probeGate(sock string) string {
	if !exists(sock) {
		return gateNone
	}
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		var ne net.Error
		if errors.Is(err, syscall.EAGAIN) || (errors.As(err, &ne) && ne.Timeout()) {
			return gateLive
		}
		return gateStale
	}
	defer c.Close()
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = io.Copy(io.Discard, c)
	return gateLive
}

// unlinkIfOurs removes the socket this server bound, and never one
// another server bound at the same path since.
func unlinkIfOurs(sock string, inode uint64) {
	st, err := os.Lstat(sock)
	if err != nil || inode == 0 {
		return
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Ino == inode {
		_ = syscall.Unlink(sock)
	}
}

// listenGate is serve()'s start: the root made (0700), a stale socket
// removed, a new one bound (0600).
func listenGate(root, sock string) (*net.UnixListener, error) {
	if err := mkdirPy(root); err != nil {
		return nil, err
	}
	// mkdir's mode is a no-op on a directory that already exists.
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	// sock.exists() follows a symlink; whatever is there is unlinked.
	if _, err := os.Stat(sock); err == nil {
		if err := syscall.Unlink(sock); err != nil {
			return nil, &os.PathError{Op: "unlink", Path: sock, Err: err}
		}
	}
	// The root is 0700 from here on, so nobody else can reach the socket
	// before the chmod below. The umask is not changed: it is shared by
	// every thread of the process, and a directory another goroutine makes
	// meanwhile would lose its search bit.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// The server removes the socket itself, and only while it is still
	// the one it bound (unlinkIfOurs).
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// mkdirPy is Path.mkdir(parents=True, exist_ok=True, mode=0o700): the
// parents with the default mode, the directory itself 0700, and an error
// when the path is there but is no directory.
func mkdirPy(d string) error {
	err := os.Mkdir(d, 0o700)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(d)
		if perr := os.MkdirAll(parent, 0o777); perr != nil {
			return perr
		}
		err = os.Mkdir(d, 0o700)
		if err == nil {
			return nil
		}
	}
	if st, serr := os.Stat(d); serr == nil && st.IsDir() {
		return nil
	}
	return err
}

// serveConn answers one connection: _Handler.handle.
func (e *Env) serveConn(c *net.UnixConn, childEnv []string, slots chan struct{}) {
	defer c.Close()
	// A failure here ends this connection with no reply, which every
	// client reads as a deny; it never ends the server.
	defer func() { _ = recover() }()
	peer := gate.PeerPid(c)
	line := readRequestLine(c)
	q, ok := gate.ParseRequest(line)
	if !ok {
		_, _ = c.Write(gate.BadRequest)
		return
	}
	q.Caller.PeerPid = peer
	var reply []byte
	if q.IsHookReport() {
		out, errText, code := gate.HookReport(q.Argv[2:], q.Stdin, q.Caller, childEnv)
		reply = gate.ReplyLine(out, errText, code)
	} else {
		slots <- struct{}{}
		var res gate.Result
		if e.GateExe != "" {
			res = gate.RunResidentGuarded(e.GateExe, []string{"gate", "check"}, q, childEnv)
		} else {
			res = gate.RunResident(q, childEnv)
		}
		<-slots
		if res.Help {
			// argparse's --help is SystemExit(0) in the oracle's handler:
			// the connection ends with no reply.
			return
		}
		reply = gate.ReplyLine(res.OutStdout, res.OutStderr, res.Exit)
	}
	_, _ = c.Write(reply)
}

// readRequestLine is rfile.readline(_MAX_REQUEST): up to and with the
// first newline, or MaxRequestBytes, or what came before the client
// stopped sending.
func readRequestLine(c *net.UnixConn) []byte {
	var buf []byte
	chunk := make([]byte, 65536)
	for len(buf) < gate.MaxRequestBytes {
		want := min(len(chunk), gate.MaxRequestBytes-len(buf))
		n, err := c.Read(chunk[:want])
		if n > 0 {
			if i := bytes.IndexByte(chunk[:n], '\n'); i >= 0 {
				return append(buf, chunk[:i+1]...)
			}
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			return buf
		}
	}
	return buf
}
