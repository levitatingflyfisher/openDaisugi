package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// ErrTransport marks a failure of the connection to the server itself - a
// network problem, not something the server said. The CLI maps this class
// to exit 3, unreachable, rather than exit 1, a user error: without it, a
// connection that died mid-call looks the same as an ordinary no_such_pane.
var ErrTransport = errors.New("the connection to the server failed")

// classifyTransportErr wraps a raw I/O error as ErrTransport when it looks
// like the connection itself died rather than the request failing on its
// own terms: io.EOF, io.ErrUnexpectedEOF, or any net.Error - a read deadline
// exceeded among them.
func classifyTransportErr(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &ne) {
		return fmt.Errorf("%w: %v", ErrTransport, err)
	}
	return err
}

// doReadDeadline is the floor Do waits for one line of a reply when the
// request carries no timeout_ms of its own - a server that accepts a
// connection and then goes silent must not hang the CLI forever. A request
// that does carry timeout_ms uses readDeadlineFor's derived value instead:
// this floor alone used to cap every request, including
// agent.wait and pane.wait_output calls whose own --timeout ran past it. A
// var, not a const, so a test can shrink it rather than wait out the real,
// generous default. 150s comfortably outlasts agent.wait's and
// pane.wait_output's own 120s documented default timeout.
var doReadDeadline = 150 * time.Second

// readDeadlineMargin is added on top of a request's own timeout_ms.
// agent.wait, agent.prompt --wait and pane.wait_output all
// take --timeout with no cap, and the fixed doReadDeadline floor used to
// abandon the read - and blame the server for it - well before a caller's
// own long --timeout had a chance to run. The margin is headroom for the
// server's own timeout reply to reach the CLI first in the ordinary case,
// not a second timeout meant to fire on its own.
const readDeadlineMargin = 10 * time.Second

// readDeadlineFor picks how long Do waits for one line of a reply. A request
// that carries a positive timeout_ms gets that value plus readDeadlineMargin;
// anything else falls back to doReadDeadline, the generous floor for a
// request with no timeout of its own. Zero and negative timeout_ms fall back
// to the same floor, matching the server's own "!ok || ms <= 0 means use the
// default" rule (agent.go, panes.go): the CLI's own --timeout flag parser
// accepts 0 and negative numbers, and without this guard the client would
// derive a bare 10s deadline while the server ran its own, much longer,
// default wait - the client giving up and blaming the server for a request
// the server was still honestly working on.
func readDeadlineFor(params map[string]any) time.Duration {
	switch v := params["timeout_ms"].(type) {
	case int:
		if v <= 0 {
			return doReadDeadline
		}
		return time.Duration(v)*time.Millisecond + readDeadlineMargin
	case float64:
		if v <= 0 {
			return doReadDeadline
		}
		return time.Duration(v)*time.Millisecond + readDeadlineMargin
	default:
		return doReadDeadline
	}
}

// Client speaks the socket protocol. The local path and the ssh path use the
// same code: --stdio is the same JSONL on a different pipe.
type Client struct {
	enc   *proto.Encoder
	dec   *proto.Decoder
	close func() error
	n     int
	// deadliner refreshes Do's own read deadline before every read, when the
	// underlying connection supports one. nil for a transport that does not,
	// in which case Do reads with no deadline at all rather than panic on a
	// type assertion.
	deadliner interface{ SetReadDeadline(time.Time) error }
	// fixedDeadline is true when dialExisting already set one deadline that
	// covers this connection's whole life, write and every read. Do must
	// not push that deadline back out on each read the way it does for an
	// ordinary Client: the point of a fixed deadline is to fail fast, not
	// to wait out whatever is on the other end.
	fixedDeadline bool
	// stderrFn returns the tail of a remote child's captured stderr, or nil
	// for a local Dial. Call it only through stderrTail, and only after
	// Close: the capture is a goroutine os/exec owns internally, and Wait -
	// which Close calls - is what guarantees that goroutine is done writing.
	// Reading the buffer any earlier races that goroutine.
	stderrFn func() string
}

// stderrTail returns the tail of a remote child's captured stderr, or "" for
// a local Dial or before Close has run.
func (c *Client) stderrTail() string {
	if c.stderrFn == nil {
		return ""
	}
	return c.stderrFn()
}

// logPath is where a detached server writes its own stdout and stderr. It is
// small and is never rotated: a daemon that logs one line per pane event does
// not need one.
func logPath(dataDir string) string { return filepath.Join(dataDir, "server.log") }

// dialOrStart connects to socket, starting a server once if nothing is
// listening. Spec-02's CLI section promises the first invocation brings the
// workshop up, so this - not the operator - does it.
func dialOrStart(socket, dataDir string) (net.Conn, error) {
	conn, err := net.Dial("unix", socket)
	if err == nil {
		return conn, nil
	}
	if serr := StartBackgroundServer(socket, dataDir); serr != nil {
		return nil, fmt.Errorf("cannot reach the server at %s and cannot start one: %v. "+
			"Run: coppice server start --foreground", socket, serr)
	}
	conn, err = waitForSocket(socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("started a server but %s never accepted a connection. "+
			"Check %s. Run: coppice server start --foreground", socket, logPath(dataDir))
	}
	return conn, nil
}

// Dial connects to socket, autostarting a server against dataDir when nothing
// is listening.
func Dial(socket, dataDir string) (*Client, error) {
	conn, err := dialOrStart(socket, dataDir)
	if err != nil {
		return nil, err
	}
	c := &Client{
		enc: proto.NewEncoder(conn), dec: proto.NewDecoder(conn),
		close: conn.Close, deadliner: conn,
	}
	if err := c.hello(false); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// helloID is the id of the hello line. Do numbers its own requests from 1,
// so the hello's reply never matches one of them and Do skips it.
const helloID = "0"

// hello is the first line on every connection the CLI opens from inside a
// pane, where COPPICE_PANE is set: it tells the server the connection is
// that pane. The server then refuses the allow verbs on it and prints each
// verb it runs as a note. Outside a pane it sends nothing.
func (c *Client) hello(remote bool) error {
	h := helloParams(remote)
	if h == nil {
		return nil
	}
	return c.enc.Send(h)
}

// helloParams is the hello line a client inside a pane sends, or nil
// outside a pane. A hello to another host names no pane, since this
// host's pane ids mean nothing there.
func helloParams(remote bool) map[string]any {
	pane := os.Getenv("COPPICE_PANE")
	if pane == "" {
		return nil
	}
	h := map[string]any{"id": helloID, "cmd": "hello", "role": "pane"}
	if !remote {
		h["pane"] = pane
	}
	return h
}

// dialExisting connects to socket only if something is already listening. It
// never autostarts: a window resize must not be able to spawn a whole
// detached daemon just because the server that used to be there is gone.
//
// deadline, when nonzero, bounds the whole connection for its entire life:
// the write and every read, set once here rather than renewed inside Do.
// watchResize passes resizeDialDeadline so a wedged handler on the other
// end cannot hold its caller open for doReadDeadline's own 150s. Pass 0 for
// Do's ordinary, renewing read deadline.
func dialExisting(socket string, deadline time.Duration) (*Client, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	if deadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(deadline))
	}
	c := &Client{
		enc: proto.NewEncoder(conn), dec: proto.NewDecoder(conn),
		close: conn.Close, deadliner: conn, fixedDeadline: deadline > 0,
	}
	if err := c.hello(false); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func waitForSocket(socket string, d time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socket); err == nil {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for %s", socket)
}

// backgroundArgv is the argv StartBackgroundServer re-execs itself with. It is
// its own function so a test can check it without forking a real process.
func backgroundArgv(self, socket, dataDir string) []string {
	return []string{self, "--socket", socket, "--data-dir", dataDir, "server", "start", "--foreground"}
}

// daemonEnv is the environment a detached server child runs under: the
// caller's own environment, minus COPPICE_PANE and COPPICE_SOCK. Autostart
// can run from inside an existing pane - a bare `coppice pane list` typed at
// a claude prompt, say - and that pane's own COPPICE_PANE and COPPICE_SOCK
// must never become the daemon's for the rest of its life. XDG_RUNTIME_DIR,
// XDG_CONFIG_HOME and every COPPICE_*_BIN pass through on purpose: they
// decide which socket, which manifests, and which harness binaries the
// daemon uses.
func daemonEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, "COPPICE_PANE=") || strings.HasPrefix(kv, "COPPICE_SOCK=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// StartBackgroundServer re-execs this binary as a detached foreground server
// and returns as soon as it is spawned. COPPICE_NO_AUTOSTART, set to anything
// non-empty, turns it off - the tests use that so a failing dial stays a
// failing dial.
//
// The child's stdout and stderr go to <dataDir>/server.log, not to this
// process's own streams and not to /dev/null: a daemon that dies at startup
// must leave a trace an operator can read, not just a silence on the other
// end of a dial.
func StartBackgroundServer(socket, dataDir string) error {
	if os.Getenv("COPPICE_NO_AUTOSTART") != "" {
		return fmt.Errorf("autostart is off. COPPICE_NO_AUTOSTART is set")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath(dataDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()

	argv := backgroundArgv(self, socket, dataDir)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = daemonEnv(os.Environ())
	// Unix only, by design: master spec section 8 puts other platforms out
	// of scope, and lock_unix.go/lock_other.go already draw that line for
	// the rest of this package.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// The child outlives us; do not wait for it, and do not leave a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Proxy pumps one client's bytes to the running server and back. This is the
// whole of --stdio. It builds no server, so nothing here can touch
// layout.json: a second server on the real data directory would write its own
// empty tree over the one the running server owns.
func Proxy(socket, dataDir string, in io.Reader, out io.Writer) error {
	conn, err := dialOrStart(socket, dataDir)
	if err != nil {
		return err
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("internal: the proxy connection is not a unix socket")
	}

	outErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, uc)
		outErr <- err
	}()

	// On stdin EOF, half-close the write side and keep reading until the
	// server closes its own end. Returning as soon as the read from in ends -
	// the naive two-goroutines-one-channel shape - would race the reply: with
	// a one-line request the stdin copy finishes at once, long before the
	// server has written anything back.
	_, inErr := io.Copy(uc, in)
	_ = uc.CloseWrite()
	oErr := <-outErr

	if inErr != nil {
		return inErr
	}
	return oErr
}

// remoteArgv turns ssh://host into the command that runs a thin server there.
func remoteArgv(target string) ([]string, error) {
	rest, ok := strings.CutPrefix(target, "ssh://")
	if !ok || rest == "" {
		return nil, fmt.Errorf("remote target %q must look like ssh://host", target)
	}
	return []string{"ssh", rest, "coppice", "--stdio"}, nil
}

// DialRemote runs coppice --stdio on target over ssh and speaks the same
// protocol across its stdin and stdout.
func DialRemote(target string) (*Client, error) {
	argv, err := remoteArgv(target)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// cmd.Start only fails when the ssh binary itself is missing. Every real
	// ssh failure - an unknown host, a refused key, coppice not found on the
	// far side's PATH - happens after a successful Start, once ssh itself
	// exits. Without capturing stderr, os/exec sends it to /dev/null, and the
	// only thing a caller ever sees is a bare EOF once the pipe closes.
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot run %s: %w", strings.Join(argv, " "), err)
	}
	var deadliner interface{ SetReadDeadline(time.Time) error }
	if d, ok := out.(interface{ SetReadDeadline(time.Time) error }); ok {
		deadliner = d
	}
	rc := &Client{
		enc: proto.NewEncoder(in), dec: proto.NewDecoder(out),
		deadliner: deadliner,
		stderrFn: func() string {
			return strings.TrimSpace(errBuf.String())
		},
		close: func() error {
			_ = in.Close()
			_, _ = io.Copy(io.Discard, out)
			return cmd.Wait()
		},
	}
	// A pane that reaches another host still says it is a pane there, so
	// that server refuses it the allow verbs too.
	if err := rc.hello(true); err != nil {
		_ = rc.Close()
		return nil, err
	}
	return rc, nil
}

// reservedParamKeys are set by Do itself, after params are merged. A caller
// that also set one - pane.run's own command text almost shadowed the "cmd"
// verb this exact way once - gets a named refusal instead of a request that
// silently answers the wrong command.
func reservedParamKey(k string) bool { return k == "id" || k == "cmd" }

// Do sends one request and reads until the matching response. Events that
// arrive first are discarded: a one-shot CLI call is not a subscriber.
func (c *Client) Do(cmd string, params map[string]any) (map[string]any, error) {
	for k := range params {
		if reservedParamKey(k) {
			return nil, fmt.Errorf("cmd %s: params must not set %q, it is reserved for the request itself", cmd, k)
		}
	}
	c.n++
	id := fmt.Sprintf("%d", c.n)
	// id and cmd go in first, not after merging params: the refusal above
	// already guarantees params holds neither key by the time execution
	// reaches here, so there is no longer an overwrite for a "set after
	// merge" order to guard against - merging in either order produces the
	// same map.
	req := map[string]any{"id": id, "cmd": cmd}
	for k, v := range params {
		req[k] = v
	}
	if err := c.enc.Send(req); err != nil {
		return nil, classifyTransportErr(err)
	}
	deadline := readDeadlineFor(params)
	for {
		if c.deadliner != nil && !c.fixedDeadline {
			_ = c.deadliner.SetReadDeadline(time.Now().Add(deadline))
		}
		line, err := c.dec.Next()
		if err != nil {
			return nil, classifyTransportErr(err)
		}
		var r struct {
			ID     string         `json:"id"`
			OK     bool           `json:"ok"`
			Result map[string]any `json:"result"`
			Error  *proto.Error   `json:"error"`
			Event  string         `json:"event"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.Event != "" || r.ID != id {
			continue
		}
		if !r.OK {
			if r.Error == nil {
				return nil, fmt.Errorf("internal: the server sent a failure with no error")
			}
			return nil, fmt.Errorf("%s: %s", r.Error.Code, r.Error.Message)
		}
		return r.Result, nil
	}
}

func (c *Client) Close() error {
	if c.close != nil {
		return c.close()
	}
	return nil
}
