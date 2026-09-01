// Package cli is the coppice command line. Every verb is one socket call, so
// the CLI and the cockpit and the PWA all drive the same server through the
// same protocol.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/tui"
	"github.com/opendaisugi/coppice/internal/web"
	"golang.org/x/term"
)

const usage = `coppice: panes, agents and their state, on one machine.

  coppice                                  open the floor
  coppice server start|stop|status|token
  coppice workspace create [--cwd DIR] [--label TEXT]
  coppice workspace list
  coppice tab create [--label TEXT] [--workspace ID]
  coppice tab list [--workspace ID]
  coppice pane create [--cwd DIR] [--label TEXT] [--kind pty|headless]
                      [--harness NAME] [--cols N] [--rows N] -- COMMAND...
  coppice pane list|send-text|send-keys|run|read|close|resize|wait-output|explain
  coppice agent list|get|prompt|wait|read
  coppice attach [PANE]
  coppice web cert init|show|tailscale
  coppice web serve|token
  coppice --remote ssh://HOST <pane, agent, workspace or tab command, or server status/stop>
  coppice --stdio                          speak the protocol on stdin and stdout

Add --json to any server, workspace, tab, pane or agent command to get one
JSON object instead of a table or a line. attach has no --json flag.
Run coppice pane list to see your panes.
The first command brings up a server on its own. Set COPPICE_NO_AUTOSTART to
turn that off.`

// CLI is one run of the coppice command line.
type CLI struct {
	Version string
	Socket  string
	DataDir string
	In      io.Reader
	Out     io.Writer
	Err     io.Writer

	// serveWeb runs the phone server. It defaults to web.Serve when left
	// nil, which is what every real run does; a test that wants
	// webServeCommand to return without binding a real listener sets this
	// to a stub instead.
	serveWeb func(context.Context, web.Config, web.Options) error

	// isTerminal answers whether In can show a screen. It defaults to a
	// real check on In as a file, which is what every real run does; a
	// test that drives attach over a pipe sets this to say yes instead.
	isTerminal func() bool
}

// stdinIsTerminal reports whether attach can draw on In. Anything that is
// not a terminal file, a pipe or an in-memory reader, cannot show a screen.
func (c *CLI) stdinIsTerminal() bool {
	if c.isTerminal != nil {
		return c.isTerminal()
	}
	f, ok := c.In.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Run returns the process exit code: 0 success, 1 user error, 3 unreachable.
// A gate deny is 2, which only the harness paths can produce; the CLI never
// does.
func (c *CLI) Run(argv []string) int {
	remote := ""
	for len(argv) > 0 && strings.HasPrefix(argv[0], "--") {
		switch argv[0] {
		case "--version":
			fmt.Fprintln(c.Out, "coppice "+c.Version)
			return 0
		case "--help", "-h":
			fmt.Fprintln(c.Out, usage)
			return 0
		case "--stdio":
			return c.runStdio()
		case "--remote":
			if len(argv) < 2 {
				fmt.Fprintln(c.Err, "--remote needs a target like ssh://host")
				return 1
			}
			remote = argv[1]
			argv = argv[2:]
			continue
		case "--socket":
			if len(argv) < 2 {
				fmt.Fprintln(c.Err, "--socket needs a path")
				return 1
			}
			c.Socket = argv[1]
			argv = argv[2:]
			continue
		case "--data-dir":
			if len(argv) < 2 {
				fmt.Fprintln(c.Err, "--data-dir needs a path")
				return 1
			}
			c.DataDir = argv[1]
			argv = argv[2:]
			continue
		default:
			fmt.Fprintf(c.Err, "unknown option %q\n%s\n", argv[0], usage)
			return 1
		}
	}
	if len(argv) == 0 {
		return c.runFloor(remote)
	}

	switch argv[0] {
	case "server":
		return c.runServer(remote, argv[1:])
	case "attach":
		return c.runAttach(remote, argv[1:])
	case "web":
		return c.runWeb(remote, argv[1:])
	case "workspace", "tab", "pane", "agent", "session":
		return c.runVerb(remote, argv)
	default:
		fmt.Fprintf(c.Err, "unknown command %q\n%s\n", argv[0], usage)
		return 1
	}
}

// reportDoErr prints a Client.Do error and picks the exit code. A transport
// failure - the connection died or never answered - is unreachable, exit 3,
// with the tail of a remote child's own stderr appended when there is one.
// A proto.Error the server itself sent back is a user error, exit 1. It
// closes cl before reading that stderr tail: the capture is a goroutine
// os/exec owns internally, and Close's own Wait is what guarantees that
// goroutine has finished writing before the buffer is read.
func (c *CLI) reportDoErr(cl *Client, err error) int {
	if errors.Is(err, ErrTransport) {
		_ = cl.Close()
		fmt.Fprintln(c.Err, "the server closed the connection. Run: coppice server status")
		if tail := cl.stderrTail(); tail != "" {
			fmt.Fprintln(c.Err, tail)
		}
		return 3
	}
	fmt.Fprintln(c.Err, err)
	return 1
}

func (c *CLI) dial(remote string) (*Client, int) {
	var (
		cl  *Client
		err error
	)
	if remote != "" {
		cl, err = DialRemote(remote)
	} else {
		cl, err = Dial(c.Socket, c.DataDir)
	}
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return nil, 3
	}
	return cl, 0
}

func (c *CLI) runVerb(remote string, argv []string) int {
	if len(argv) < 2 {
		fmt.Fprintf(c.Err, "%s needs a verb. %s\n", argv[0], usage)
		return 1
	}
	group, verb := argv[0], argv[1]

	params, asJSON, errMsg := parseVerbArgs(group, verb, argv[2:])
	if errMsg != "" {
		fmt.Fprintln(c.Err, errMsg)
		return 1
	}

	cl, code := c.dial(remote)
	if code != 0 {
		return code
	}
	defer cl.Close()

	res, err := cl.Do(socketCommand(group, verb), params)
	if err != nil {
		return c.reportDoErr(cl, err)
	}
	if asJSON {
		b, _ := json.Marshal(res)
		fmt.Fprintln(c.Out, string(b))
		return 0
	}
	c.printHuman(group, verb, res)
	return 0
}

// printHuman is the table, or the line, for a person. --json is the data for
// a program. They are deliberately different outputs for two audiences: a
// table that also has to parse as data serves neither well.
func (c *CLI) printHuman(group, verb string, res map[string]any) {
	if key := pluralKey(group, verb); key != "" {
		if rows, ok := res[key].([]any); ok {
			for _, r := range rows {
				m, _ := r.(map[string]any)
				id := firstOf(m, "id", "pane")
				line := fmt.Sprintf("%-10v %-10v %-10v %-10v %v",
					id, orDash(m["kind"]), orDash(m["state"]), orDash(m["source"]), orDash(m["label"]))
				// The state column alone cannot say a
				// pane is closed - a pane that never started carries no
				// state at all - so a closed row gets its own trailing
				// token instead of printing identically to a live one.
				if closed, _ := m["closed"].(bool); closed {
					line += " closed"
				}
				fmt.Fprintln(c.Out, line)
			}
			return
		}
	}
	// No list key: print every scalar the reply carries, sorted, rather than
	// a hand-kept list of the fields worth showing. pane.close's own
	// {"pane","closed":true} used to print only the pane id, with no sign the
	// close had happened - this is what fixed that, for every verb at once.
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Fprintln(c.Out, "ok")
		return
	}
	for _, k := range keys {
		fmt.Fprintf(c.Out, "%s %v\n", k, res[k])
	}
}

func (c *CLI) runServer(remote string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, "coppice server needs start, stop, status or token")
		return 1
	}
	sub, rest := argv[0], argv[1:]
	switch sub {
	case "start", "stop", "status", "token":
	default:
		fmt.Fprintf(c.Err, "unknown server command %q. Try start, stop, status or token\n", sub)
		return 1
	}
	foreground, asJSON, errMsg := parseServerFlags(sub, rest)
	if errMsg != "" {
		fmt.Fprintln(c.Err, errMsg)
		return 1
	}
	switch sub {
	case "start":
		if remote != "" {
			fmt.Fprintln(c.Err, "coppice server start does not take --remote. "+
				"Start the server on the remote host itself.")
			return 1
		}
		return c.serverStart(foreground, asJSON)
	case "stop":
		return c.serverStop(remote, asJSON)
	case "status":
		return c.serverStatus(remote, asJSON)
	default: // "token"
		return c.serverToken(asJSON)
	}
}

// serverStart brings up a server. Without --foreground it detaches: it
// checks whether one is already listening, and if not, spawns a detached
// child and returns as soon as the child's socket answers. --foreground is
// what that child runs, and what an operator uses to watch a server directly.
// startedJSON and alreadyRunningJSON are the two shapes server start --json
// can print, each its own function so a test can pin the exact bytes
// without needing a real spawn to reach them.
func startedJSON(socket, log string) string {
	b, _ := json.Marshal(map[string]any{"socket": socket, "log": log})
	return string(b)
}

func alreadyRunningJSON(socket string) string {
	b, _ := json.Marshal(map[string]any{"already_running": true, "socket": socket})
	return string(b)
}

// startedLine is what a server prints once it is confirmed listening,
// whether that server just started in --foreground or a detached one just
// answered its first connection: the same function backs both branches, so
// the JSON shape and the human text for "a server is now listening" can
// never drift apart between them.
func startedLine(asJSON bool, socket, log string) string {
	if asJSON {
		return startedJSON(socket, log)
	}
	return fmt.Sprintf("coppice is listening on %s\nLogs: %s", socket, log)
}

func (c *CLI) serverStart(foreground, asJSON bool) int {
	if !foreground {
		// Idempotent: a server already listening is success, not an error -
		// the whole point of this command is "make sure one is running".
		if conn, err := net.Dial("unix", c.Socket); err == nil {
			_ = conn.Close()
			if asJSON {
				fmt.Fprintln(c.Out, alreadyRunningJSON(c.Socket))
				return 0
			}
			fmt.Fprintf(c.Out, "already running. Socket: %s\n", c.Socket)
			return 0
		}
		if err := StartBackgroundServer(c.Socket, c.DataDir); err != nil {
			fmt.Fprintln(c.Err, err)
			return 1
		}
		if _, err := waitForSocket(c.Socket, 5*time.Second); err != nil {
			fmt.Fprintf(c.Err, "started a server but %s never accepted a connection. Check %s.\n",
				c.Socket, logPath(c.DataDir))
			return 3
		}
		fmt.Fprintln(c.Out, startedLine(asJSON, c.Socket, logPath(c.DataDir)))
		return 0
	}

	s, err := server.New(server.Config{SocketPath: c.Socket, DataDir: c.DataDir})
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	// The lock comes first, before anything reads or writes the data
	// directory. A second start that got as far as Restore would spawn its
	// own resumed panes and write its tree back over the running server's
	// layout, and only then fail to bind.
	if err := s.AcquireStartLock(); err != nil {
		// ErrAlreadyRunning already names the holder's pid and points to
		// coppice server status. That is the whole message an operator needs.
		fmt.Fprintln(c.Err, err)
		return 1
	}
	// Releases the lock on every exit path. A Server is single use: Listen
	// refuses a second call, and Close never clears that. A restart is
	// always a new process, never this same *Server started again - which is
	// also why the function returns here, on any error below, rather than
	// falling through to Serve on a Server that never bound a listener.
	defer s.Close()

	if err := registerAll(s); err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	if err := s.Listen(); err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	// Restore runs after Listen, not before: a resumed pane's gate hook
	// dials COPPICE_SOCK, and Listen's own backlog is what makes that
	// connection queue instead of refuse while Restore is still running.
	if err := s.Restore(); err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}

	// The phone server is opt-in and reads its own settings from web.json.
	// AutoStart starts nothing when that file is absent or disabled, and a
	// config it cannot read is a warning, not a reason to refuse the whole
	// coppice server.
	stopWeb, err := web.AutoStart(context.Background(), web.ConfigPath(c.DataDir), web.Options{
		Dial:   web.UnixDialer{Path: c.Socket},
		Tokens: web.TokenStore{Path: web.TokenPath(c.DataDir)},
		Gate:   web.AskChannel{Root: web.GateRoot(c.DataDir)},
	})
	if err != nil {
		fmt.Fprintf(c.Err, "web: the phone server did not start: %v\n", err)
	} else {
		defer stopWeb()
	}

	// The goroutine loops, not a single <-sig: the first signal starts
	// Close on its own goroutine, so it never blocks here and can still
	// observe a second one. A second signal during a slow teardown means the
	// operator wants out now, not another wait for a graceful shutdown that
	// is not finishing; force-exit rather than swallow it silently.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		first := true
		for range sig {
			if first {
				first = false
				go func() { _ = s.Close() }()
				continue
			}
			fmt.Fprintln(c.Err, "stopping now")
			os.Exit(1)
		}
	}()

	fmt.Fprintln(c.Out, startedLine(asJSON, c.Socket, logPath(c.DataDir)))
	if err := s.Serve(); err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	return 0
}

func (c *CLI) serverStop(remote string, asJSON bool) int {
	var (
		cl  *Client
		err error
	)
	if remote != "" {
		cl, err = DialRemote(remote)
	} else {
		// dialExisting, never c.dial: server stop's
		// whole purpose is "make sure none is running", the same reasoning
		// that makes server start idempotent - so a dial that finds nothing
		// listening must not start one just to stop it.
		cl, err = dialExisting(c.Socket, 0)
	}
	if err != nil {
		if remote == "" {
			if asJSON {
				fmt.Fprintln(c.Out, `{"stopped":false,"note":"no server is running"}`)
				return 0
			}
			fmt.Fprintln(c.Out, "no server is running")
			return 0
		}
		fmt.Fprintln(c.Err, err)
		return 3
	}
	if _, err := cl.Do("server.stop", nil); err != nil {
		return c.reportDoErr(cl, err)
	}
	_ = cl.Close()

	if remote != "" {
		if asJSON {
			fmt.Fprintln(c.Out, `{"stopped":true,"note":"the remote server is stopping"}`)
			return 0
		}
		fmt.Fprintln(c.Out, "the remote server is stopping")
		return 0
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		// StartLockHeld, not a socket poll: Close releases the start lock
		// only after it has torn down every live pane, well after it stops
		// listening, so a server start racing in right after the socket
		// closes can still lose to a lock the old process has not released
		// yet. This probes the real signal without taking the lock itself.
		if held, err := server.StartLockHeld(c.DataDir); err == nil && !held {
			if asJSON {
				fmt.Fprintln(c.Out, `{"stopped":true}`)
				return 0
			}
			fmt.Fprintln(c.Out, "stopped")
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(c.Err, "the server did not stop within 8 s. Check %s.\n", logPath(c.DataDir))
	return 1
}

func (c *CLI) serverStatus(remote string, asJSON bool) int {
	cl, code := c.dial(remote)
	if code != 0 {
		return code
	}
	defer cl.Close()
	res, err := cl.Do("server.status", nil)
	if err != nil {
		return c.reportDoErr(cl, err)
	}
	if asJSON {
		b, _ := json.Marshal(res)
		fmt.Fprintln(c.Out, string(b))
		return 0
	}
	fmt.Fprint(c.Out, statusText(res))
	return 0
}

// tokenNote is the one sentence server token --json prints as its "note"
// field: there is no token, the socket is the credential. The human line
// below carries the same content but writes its own three-line layout
// instead of calling this, since a person reading a terminal wants line
// breaks, not one long run-on sentence.
func (c *CLI) tokenNote() string {
	return fmt.Sprintf(
		"coppice has no token. The socket is the credential. "+
			"It is %s, mode 0600, and the server refuses any peer whose uid is not yours. "+
			"To reach it from another machine, run: coppice --remote ssh://HOST pane list.",
		c.Socket)
}

func (c *CLI) serverToken(asJSON bool) int {
	if asJSON {
		b, _ := json.Marshal(map[string]any{"token": nil, "note": c.tokenNote()})
		fmt.Fprintln(c.Out, string(b))
		return 0
	}
	// There is no token. The socket is the credential: mode 0600 in a 0700
	// directory, and the server checks the peer uid. Say so rather than
	// printing something that looks like a secret.
	fmt.Fprintf(c.Out,
		"coppice has no token. The socket is the credential.\n"+
			"It is %s, mode 0600, and the server refuses any peer whose uid is not yours.\n"+
			"To reach it from another machine, run: coppice --remote ssh://HOST pane list\n",
		c.Socket)
	return 0
}

func termSize(f *os.File) (int, int, error) { return term.GetSize(int(f.Fd())) }

// resolveAttachSize turns termSize's own result into what attach.Options
// needs. attach.Run reserves the terminal's own last row for its status
// line, so Options.Rows must be the terminal's FULL height, unchanged:
// passing height-1 here made Run reserve a status row from an
// already-shrunk height, so the terminal's real last row was never
// written, and a SIGWINCH afterward moved the status line onto the pane's
// own last row instead of below it. ok is false when termSize could not
// run at all - c.Out is not a file, or the call itself failed - and falls
// back to a sane default.
func resolveAttachSize(w, h int, ok bool) (cols, rows int) {
	if !ok {
		return 120, 40
	}
	return w, h
}

func shell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// runAttach drives one attach session, and, when the user asks for another
// pane or a new one, the next. --remote is refused rather than half-wired:
// attach.Run dials a local unix socket directly, and proxying that over ssh
// is future work, not this one.
func (c *CLI) runAttach(remote string, argv []string) int {
	if remote != "" {
		fmt.Fprintln(c.Err, "coppice attach needs a local socket; it is not wired for --remote yet. "+
			"SSH into the host and run coppice attach there.")
		return 1
	}
	paneID := ""
	if len(argv) > 0 {
		// Every other verb refuses an unknown flag by
		// name rather than swallowing it as a positional. attach's only
		// argument is a pane id, so anything starting with "--" is a
		// mistake, never a pane id to look up.
		if strings.HasPrefix(argv[0], "--") {
			fmt.Fprintf(c.Err, "unknown flag %q. coppice attach's only argument is a pane id.\n", argv[0])
			return 1
		}
		paneID = argv[0]
	}
	// Checked before any dial: a pipe cannot show a screen, and a server
	// error or a silent hang would hide that plain fact.
	if !c.stdinIsTerminal() {
		fmt.Fprintln(c.Err, "attach needs a terminal. Run it from a shell, not a pipe.")
		return 1
	}
	if paneID == "" {
		// dialExisting, never c.dial: attach never autostarts, with or
		// without a pane id - the picker dialing here
		// first, before attach.Run's own dial, must not be a second way in
		// for the same autostart the named-pane form already refuses.
		cl, err := dialExisting(c.Socket, 0)
		if err != nil {
			fmt.Fprintf(c.Err, "cannot reach the server at %s. Run: coppice server start\n", c.Socket)
			return 3
		}
		res, err := cl.Do("pane.list", nil)
		_ = cl.Close()
		if err != nil {
			return c.reportDoErr(cl, err)
		}
		rows, _ := res["panes"].([]any)
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if closed, _ := m["closed"].(bool); !closed {
				paneID, _ = m["id"].(string)
				break
			}
		}
		if paneID == "" {
			fmt.Fprintln(c.Err, "there are no open panes. Run: coppice pane create --cwd . -- claude")
			return 1
		}
	}

	cols, rows := resolveAttachSize(0, 0, false)
	if f, ok := c.Out.(*os.File); ok {
		if w, h, err := termSize(f); err == nil {
			cols, rows = resolveAttachSize(w, h, true)
		}
	}

	// One long-lived key reader for the whole session: a caller that loops
	// on Next across panes must pass the same channel to every attach.Run
	// call, or a fresh reader on In each time would put two readers on one
	// stdin.
	keys := attach.ReadKeys(c.In)

	isTTY := false
	if f, ok := c.Out.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}

	for {
		if isTTY {
			// The full clear and cursor home right after the request put
			// the pane on a clean screen even when the terminal ignores
			// the request: on a terminal that honours it, the alternate
			// screen is already blank, so the clear has nothing to do.
			_, _ = io.WriteString(c.Out, "\x1b[?1049h\x1b[2J\x1b[H")
		}

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGWINCH)
		resizeDone := make(chan struct{})
		resizeStopped := make(chan struct{})
		go func() {
			defer close(resizeStopped)
			c.watchResize(sig, resizeDone, paneID)
		}()

		next, err := attach.Run(attach.Options{
			Socket: c.Socket, Pane: paneID, In: c.In, Out: c.Out, Cols: cols, Rows: rows, Keys: keys,
		})

		// The alternate-screen exit comes first, right after asking
		// watchResize to stop, not after joining it: resizeDialDeadline
		// bounds that join to a few seconds, but a person sitting at a
		// frozen pane should get their shell back the instant this loop
		// knows it is leaving, not after any wait at all.
		close(resizeDone)
		if isTTY {
			_, _ = io.WriteString(c.Out, "\x1b[?1049l")
		}
		// Joined, not just asked to stop: without waiting for resizeStopped,
		// a SIGWINCH already buffered on sig before signal.Stop below could
		// still be mid-handling - calling termSize and dialing - after this
		// loop has already moved on to the next pane, or returned, sending
		// pane.resize for a pane that is no longer the one being watched.
		<-resizeStopped
		signal.Stop(sig)

		if err != nil {
			fmt.Fprintln(c.Err, err)
			if errors.Is(err, attach.ErrServerGone) {
				return 3
			}
			return 1
		}
		switch {
		case next.Pane != "":
			paneID = next.Pane
		case next.Create:
			// dialExisting, never c.dial: attach never autostarts, with or
			// without a pane id, and a live session asking for a new pane
			// is not a second way in for the same autostart the picker
			// branch above and server stop already refuse.
			cl, err := dialExisting(c.Socket, 0)
			if err != nil {
				fmt.Fprintf(c.Err, "cannot reach the server at %s. Run: coppice server start\n", c.Socket)
				return 3
			}
			cwd, _ := os.Getwd()
			// rows here is the terminal's FULL height; the pane itself gets
			// one row less, matching what attach.Run actually streams into
			// once it reserves its own status row.
			res, err := cl.Do("pane.create", map[string]any{
				"cwd": cwd, "kind": "pty", "cmd_argv": []string{shell()}, "cols": cols, "rows": rows - 1,
			})
			_ = cl.Close()
			if err != nil {
				fmt.Fprintln(c.Err, err)
				return 1
			}
			id, _ := res["pane"].(string)
			if id == "" {
				fmt.Fprintln(c.Err, "the server created no pane. Run: coppice pane list")
				return 1
			}
			paneID = id
		default:
			return 0
		}
	}
}

// floorSize is the terminal size the floor renders into, read from Out
// before every frame. It falls back to the same default attach uses when
// Out is not a terminal.
func (c *CLI) floorSize() (int, int) {
	if f, ok := c.Out.(*os.File); ok {
		if w, h, err := termSize(f); err == nil {
			return resolveAttachSize(w, h, true)
		}
	}
	return resolveAttachSize(0, 0, false)
}

// runFloor is coppice with no arguments: the roster, the peek, and attach
// from one screen. --remote is refused the way attach refuses it: the
// floor dials a local socket. A pipe cannot show it, so a non-terminal In
// gets the usage text and exit 1. One dial brings a server up when none
// is running, and a dial that fails is exit 3. The alternate screen wraps
// the whole run once, so attach's own nested entry and exit inside it
// change nothing.
func (c *CLI) runFloor(remote string) int {
	if remote != "" {
		fmt.Fprintln(c.Err, "the floor needs a local socket; it is not wired for --remote yet. "+
			"SSH into the host and run coppice there.")
		return 1
	}
	if !c.stdinIsTerminal() {
		fmt.Fprintln(c.Err, usage)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read the working directory: %v\n", err)
		return 1
	}
	cl, err := Dial(c.Socket, c.DataDir)
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 3
	}
	_ = cl.Close()

	isTTY := false
	if f, ok := c.Out.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	if isTTY {
		_, _ = io.WriteString(c.Out, "\x1b[?1049h\x1b[2J\x1b[H")
	}
	err = tui.Run(tui.Options{
		Socket: c.Socket, In: c.In, Out: c.Out, Size: c.floorSize, Cwd: cwd, Default: "claude",
		DataDir: c.DataDir,
	})
	if isTTY {
		_, _ = io.WriteString(c.Out, "\x1b[?1049l")
	}
	if err != nil {
		fmt.Fprintln(c.Err, err)
		if errors.Is(err, attach.ErrServerGone) {
			return 3
		}
		return 1
	}
	return 0
}

// resizeDialDeadline bounds watchResize's own short-lived connection, sized
// the same as attach's own teardownDeadline. A resize is best-effort work;
// it is not worth making a person wait out a wedged handler for.
const resizeDialDeadline = 2 * time.Second

// watchResize sends pane.resize on every SIGWINCH, over its own short-lived
// connection: the attach connection is already busy streaming frames. It
// sends the new size minus one row for the status line, and lets the next
// frame redraw the rest.
func (c *CLI) watchResize(sig <-chan os.Signal, done <-chan struct{}, paneID string) {
	for {
		select {
		case <-sig:
			f, ok := c.Out.(*os.File)
			if !ok {
				continue
			}
			w, h, err := termSize(f)
			if err != nil || h <= 1 {
				continue
			}
			// dialExisting, never Dial: a resize must not autostart a whole
			// detached daemon just because the server that used to be there
			// is gone. resizeDialDeadline bounds the call too: a wedged
			// handler on the far end must not hold runAttach's own join on
			// this goroutine open for doReadDeadline's full 150s.
			cl, err := dialExisting(c.Socket, resizeDialDeadline)
			if err != nil {
				continue
			}
			_, _ = cl.Do("pane.resize", map[string]any{"pane": paneID, "cols": w, "rows": h - 1})
			_ = cl.Close()
		case <-done:
			return
		}
	}
}

// runStdio pumps bytes between stdin/stdout and the running server's socket.
// It builds no server: --remote ssh://host pane list must see the host's
// real pane tree, and a second server on the real data directory would write
// an empty layout over the running server's file.
func (c *CLI) runStdio() int {
	if err := Proxy(c.Socket, c.DataDir, c.In, c.Out); err != nil {
		fmt.Fprintln(c.Err, err)
		return 3
	}
	return 0
}
