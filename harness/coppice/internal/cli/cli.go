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
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/plugins"
	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/tmuxmirror"
	"github.com/opendaisugi/coppice/internal/tui"
	"github.com/opendaisugi/coppice/internal/web"
	"github.com/opendaisugi/coppice/skills"
	"golang.org/x/term"
)

const usage = `coppice: panes, agents and their state, on one machine.

  coppice                                  open the floor
  coppice claude [ARGS...]                 open that harness here and attach
  coppice open HARNESS [ARGS...]           the long form
  coppice server start|stop|status|token
  coppice workspace create [--cwd DIR] [--label TEXT]
  coppice workspace list
  coppice tab create [--label TEXT] [--workspace ID]
  coppice tab list [--workspace ID]
  coppice pane create [--cwd DIR] [--label TEXT] [--kind pty|headless]
                      [--harness NAME] [--cols N] [--rows N] [--task ID] -- COMMAND...
  coppice pane list [--ended]|send-text|send-keys|run|read|close|resize|wait-output|explain
  coppice pane fork PANE [--label TEXT]
  coppice pane forget PANE|--ended        remove an ended pane's record; a live pane refuses
  coppice pane resume PANE                start a new pane from an ended one; removes it
  coppice pane rename PANE LABEL...       change a pane's label in place, live or ended
  coppice pane trust PANE [--not-now]     answer Claude's folder trust screen; --not-now
                                           presses Esc, which ends Claude
  coppice new [PROJECT] [--no-attach]     the default harness, in a project by name or
                                           path, or here; attaches, unless --no-attach
  coppice project add|list|rm [DIR]       pin, list or unpin a working directory
  coppice agent list|get|prompt|wait|read
  coppice agent allow|deny PANE ASK [--reason TEXT]   answer a gate ask; a pane cannot,
                                           but a foreman may deny an ask it holds
  coppice agent allow PANE ASK --confirm NAME  allow a permanent ask by its pane name
  coppice agent allow PANE ASK --scope task    allow an undoable ask for the task
  coppice floor note TEXT                  print one line on the floor
  coppice floor talk TEXT                  say TEXT to the floor's foreman; starts one if none runs
  coppice skill foreman                    print the page that makes a pane the foreman
  coppice task create --label NAME [--parent ID] [--cwd DIR] [--worktree] [--model NAME]
  coppice task list [--tree]
  coppice task close ID [--keep-worktree]
  coppice task move ID --parent ID
  coppice task set-foreman ID --pane PANE  name the pane that hears the task's asks first
  coppice attach [PANE]
  coppice tmux-mirror [--session NAME] [--tmux-socket PATH]
                                           one tmux window per pane, each attached
  coppice web cert init|show|tailscale
  coppice web serve|token
  coppice --remote ssh://HOST <pane, agent, workspace or tab command, or server status/stop>
  coppice --stdio                          speak the protocol on stdin and stdout

Add --json to any server, workspace, tab, pane, agent or task command to get
one JSON object instead of a table or a line. attach has no --json flag.
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

	// ensureServer brings up the coppice server when none answers. It
	// defaults to a Dial, which autostarts one. A test sets a stub.
	ensureServer func() error

	// openURL shows a URL in the browser. It defaults to the desktop's
	// opener. A test sets a stub.
	openURL func(string) error
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
	case "tmux-mirror":
		return c.runTmuxMirror(remote, argv[1:])
	case "web":
		return c.runWeb(remote, argv[1:])
	case "task":
		return c.runTask(remote, argv)
	case "workspace", "tab", "pane", "agent", "session":
		return c.runVerb(remote, argv)
	case "floor":
		return c.runVerb(remote, argv)
	case "skill":
		return c.runSkill(argv[1:])
	case "open":
		return c.runOpen(remote, argv[1:])
	case "new":
		return c.runNew(remote, argv[1:])
	case "project":
		return c.runProject(remote, argv[1:])
	default:
		// A word that is not a verb is a harness name to open, or an
		// unknown command. open decides, and prints the usage after its
		// refusal when the word is unknown.
		return c.open(remote, argv, true)
	}
}

// runSkill is coppice skill NAME: it prints one page the binary carries.
// It dials no socket.
func (c *CLI) runSkill(argv []string) int {
	names := make([]string, 0, len(skills.Pages))
	for n := range skills.Pages {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(argv) != 1 {
		fmt.Fprintf(c.Err, "skill needs one page name. Pages: %s\n", strings.Join(names, ", "))
		return 1
	}
	page, ok := skills.Pages[argv[0]]
	if !ok {
		fmt.Fprintf(c.Err, "no skill page %q. Pages: %s\n", argv[0], strings.Join(names, ", "))
		return 1
	}
	fmt.Fprint(c.Out, page)
	return 0
}

// runOpen is coppice open HARNESS [ARGS...]: open that harness in the
// current directory and attach.
func (c *CLI) runOpen(remote string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, "open needs a harness name. Run coppice to see the list.")
		return 1
	}
	return c.open(remote, argv, false)
}

// open resolves argv[0] as a harness name, opens a pane for it in the
// current directory, and attaches. bare is true when the name came as a
// bare word, so an unknown word gets the usage after the refusal.
func (c *CLI) open(remote string, argv []string, bare bool) int {
	return c.openAt(remote, argv, bare, "", false)
}

// openAt is open's full implementation, plus two knobs coppice new needs
// and open itself never sets: cwdOverride, which stands in for the
// process's own working directory when it is not empty, and noAttach,
// which forces the pane-id reply open already gives on a pipe.
//
// --remote is refused first, before the config is read or PATH is
// searched: attach dials a local socket, and that is the one reason to
// show. With a config file, the name must have a [harness.<name>] table.
// Without one, the name must be a harness Discover finds on PATH, and
// the config is written then, with every found harness and this one as
// the default. On a pipe, or with noAttach, the pane opens and its id is
// printed instead of attaching.
func (c *CLI) openAt(remote string, argv []string, bare bool, cwdOverride string, noAttach bool) int {
	if remote != "" {
		fmt.Fprintln(c.Err, "coppice open needs a local socket; it is not wired for --remote yet. "+
			"SSH into the host and run coppice open there.")
		return 1
	}
	name, rest := argv[0], argv[1:]
	cfg, found, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read %s: %v\n", config.Path(), err)
		return 1
	}
	var save *config.Config
	if found {
		if _, ok := cfg.Harness[name]; !ok {
			names := make([]string, 0, len(cfg.Harness))
			for n := range cfg.Harness {
				names = append(names, n)
			}
			sort.Strings(names)
			fmt.Fprintf(c.Err, "unknown command %q. Harnesses in %s: %s.\n",
				name, config.Path(), strings.Join(names, ", "))
			if bare {
				fmt.Fprintln(c.Err, usage)
			}
			return 1
		}
	} else {
		disc := config.Discover(exec.LookPath)
		if len(disc) == 0 {
			// The bare word may be a typo of a verb, so the usage
			// follows the note.
			fmt.Fprintln(c.Err, config.AppOnlyNote(nil))
			if bare {
				fmt.Fprintln(c.Err, usage)
			}
			return 1
		}
		hit := false
		names := make([]string, 0, len(disc))
		for _, f := range disc {
			names = append(names, f.Name)
			hit = hit || f.Name == name
		}
		if !hit {
			if bare {
				fmt.Fprintf(c.Err, "unknown command %q\n%s\n", name, usage)
			} else {
				fmt.Fprintf(c.Err, "%s is not on PATH. Harnesses found: %s.\n",
					name, strings.Join(names, ", "))
			}
			return 1
		}
		fresh := config.FromFound(disc, name)
		save = &fresh
	}
	if save != nil {
		if err := config.Save(*save); err != nil {
			fmt.Fprintf(c.Err, "cannot write %s: %v\n", config.Path(), err)
			return 1
		}
	}
	cwd := cwdOverride
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(c.Err, "cannot read the working directory: %v\n", err)
			return 1
		}
	}
	cl, code := c.dial(remote)
	if code != 0 {
		return code
	}
	cols, rows := c.floorSize()
	// The pane gets one row less than the terminal, the row attach keeps
	// for its status line.
	if rows > 1 {
		rows--
	}
	params := map[string]any{"harness": name, "cwd": cwd, "kind": "pty", "cols": cols, "rows": rows}
	if len(rest) > 0 {
		params["args"] = rest
	}
	res, err := cl.Do("pane.create", params)
	if err != nil {
		return c.reportDoErr(cl, err)
	}
	_ = cl.Close()
	id, _ := res["pane"].(string)
	if id == "" {
		fmt.Fprintln(c.Err, "the server created no pane. Run: coppice pane list")
		return 1
	}
	if noAttach {
		fmt.Fprintf(c.Out, "opened %s\n", id)
		return 0
	}
	if !c.stdinIsTerminal() {
		fmt.Fprintf(c.Out, "opened %s. attach needs a terminal.\n", id)
		return 0
	}
	return c.runAttach("", []string{id})
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

// runTask is the task group. task list --tree draws the text tree here,
// from task.list and pane.list. task create sends --cwd as an absolute
// path, so "." names the shell's directory and never the server's. Every
// other task verb takes the generic path.
func (c *CLI) runTask(remote string, argv []string) int {
	if len(argv) >= 2 && argv[1] == "create" && remote == "" {
		argv = append([]string{}, argv...)
		for i := 2; i+1 < len(argv); i++ {
			if argv[i] != "--cwd" {
				continue
			}
			abs, err := filepath.Abs(argv[i+1])
			if err != nil {
				fmt.Fprintf(c.Err, "cannot resolve --cwd %q: %v\n", argv[i+1], err)
				return 1
			}
			argv[i+1] = abs
		}
	}
	if len(argv) >= 2 && argv[1] == "list" {
		rest := argv[2:]
		tree := false
		kept := rest[:0]
		for _, a := range rest {
			if a == "--tree" {
				tree = true
				continue
			}
			kept = append(kept, a)
		}
		if tree {
			if len(kept) > 0 {
				fmt.Fprintf(c.Err, "task list --tree takes no other flag, got %q\n", kept[0])
				return 1
			}
			return c.runTaskTree(remote)
		}
	}
	return c.runVerb(remote, argv)
}

// runTaskTree prints the tree the floor's tree word draws, at the
// terminal's width when Out is a terminal, else at 80 columns.
func (c *CLI) runTaskTree(remote string) int {
	cl, code := c.dial(remote)
	if code != 0 {
		return code
	}
	defer cl.Close()
	tasksRes, err := cl.Do("task.list", nil)
	if err != nil {
		return c.reportDoErr(cl, err)
	}
	panesRes, err := cl.Do("pane.list", nil)
	if err != nil {
		return c.reportDoErr(cl, err)
	}
	tasks := tui.DecodeTasks(listOf(tasksRes, "tasks"))
	panes := tui.RowsFrom(listOf(panesRes, "panes"), float64(time.Now().UnixNano())/1e9)
	cols := 80
	if f, ok := c.Out.(*os.File); ok {
		if w, _, err := termSize(f); err == nil && w > 0 {
			cols = w
		}
	}
	lines := tui.RenderTree(tasks, panes, cols)
	if len(lines) == 0 {
		fmt.Fprintln(c.Out, "No tasks. Run: coppice task create --label NAME")
		return 0
	}
	for _, l := range lines {
		fmt.Fprintln(c.Out, l)
	}
	return 0
}

// listOf reads the list under key as rows.
func listOf(res map[string]any, key string) []map[string]any {
	raw, _ := res[key].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

// printHuman is the table, or the line, for a person. --json is the data for
// a program. They are deliberately different outputs for two audiences: a
// table that also has to parse as data serves neither well.
func (c *CLI) printHuman(group, verb string, res map[string]any) {
	if key := pluralKey(group, verb); key != "" {
		if rows, ok := res[key].([]any); ok {
			for _, r := range rows {
				m, _ := r.(map[string]any)
				if group == "task" {
					fmt.Fprintf(c.Out, "%-6v %-10v %-6v %-20v %v\n",
						orDash(m["id"]), orDash(m["state"]), orDash(m["parent"]), orDash(m["label"]), orDash(m["worktree"]))
					continue
				}
				if group == "project" {
					mark := " "
					if pinned, _ := m["pinned"].(bool); pinned {
						mark = "*"
					}
					fmt.Fprintf(c.Out, "%s %-16v %v\n", mark, orDash(m["name"]), orDash(m["path"]))
					continue
				}
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

	s, err := server.New(server.Config{SocketPath: c.Socket, DataDir: c.DataDir, Voice: voiceConfig()})
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
	// Restore already ran the at-start half of the seven-day sweep on
	// whatever it just found ended. This is the hourly half, for as long
	// as this process keeps running.
	s.StartEndedSweep()
	// Voice starts on its own goroutine, so a model that loads slowly never
	// holds up the socket or the started line.
	s.StartVoice()

	// The phone server is opt-in and reads its own settings from web.json.
	// AutoStart starts nothing when that file is absent or disabled, and a
	// config it cannot read is a warning, not a reason to refuse the whole
	// coppice server.
	enabled, settings := c.loadPluginsAndConfig()
	runner := c.startPolicies(s, enabled, settings)
	// Deferred after s.Close, so it runs first: no policy outlives the
	// server.
	defer runner.Stop()
	stopWeb, err := web.AutoStart(context.Background(), web.ConfigPath(c.DataDir), web.Options{
		Dial:   web.UnixDialer{Path: c.Socket},
		Tokens: web.TokenStore{Path: web.TokenPath(c.DataDir)},
		Gate:   web.AskChannel{Root: web.GateRoot(c.DataDir)},
		Views:  plugins.ViewsOf(enabled),
		// The library the views import ships with the binary.
		ViewLib: plugins.SharedLib(),
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

// leaveKey reads the leave key from the config file. A missing file gives
// the default. A file that does not parse, or a [keys] leave the parser
// refuses, is printed to Err and stops the run with exit 1: attach must
// never open a pane with a key nobody can press.
func (c *CLI) leaveKey() (byte, bool) {
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read %s: %v\n", config.Path(), err)
		return 0, false
	}
	leave, err := cfg.LeaveKey()
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 0, false
	}
	return leave, true
}

// paneKind looks up one pane's kind and label from pane.list, on a
// short-lived connection of its own. ok is false when the server cannot
// be reached, refuses the call, or the pane is not in the list: any of
// those leaves the caller no worse informed than before this lookup
// existed, so runAttach falls through to attach.Run exactly as it did
// before this check was added.
func (c *CLI) paneKind(id string) (kind, label string, ok bool) {
	cl, err := dialExisting(c.Socket, 0)
	if err != nil {
		return "", "", false
	}
	defer cl.Close()
	res, err := cl.Do("pane.list", nil)
	if err != nil {
		return "", "", false
	}
	rows, _ := res["panes"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if pid, _ := m["id"].(string); pid == id {
			k, _ := m["kind"].(string)
			l, _ := m["label"].(string)
			return k, l, true
		}
	}
	return "", "", false
}

// runAttach drives one attach session. --remote is refused rather than
// half-wired: attach.Run dials a local unix socket directly, and proxying
// that over ssh is future work, not this one.
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
	leave, ok := c.leaveKey()
	if !ok {
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

	// A headless agent takes whole messages, never raw keystrokes: attach
	// would either forward keys it refuses (codex, opencode, sprig) or
	// feed them straight to its stdin past the ask guard (claude in
	// stream-json mode). It never reaches attach.Run.
	if kind, label, ok := c.paneKind(paneID); ok && kind == "headless" {
		name := label
		if name == "" {
			name = paneID
		}
		fmt.Fprintf(c.Err, "%s takes whole messages, not raw keystrokes. Open the floor and type in its window, "+
			"or run: coppice agent prompt %s \"...\"\n", name, paneID)
		return 1
	}

	cols, rows := resolveAttachSize(0, 0, false)
	if f, ok := c.Out.(*os.File); ok {
		if w, h, err := termSize(f); err == nil {
			cols, rows = resolveAttachSize(w, h, true)
		}
	}

	// One long-lived key reader, joined by attach.Run before it returns,
	// so no reader on stdin outlives the session.
	keys := attach.ReadKeys(c.In)

	isTTY := false
	if f, ok := c.Out.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}

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

	err := attach.Run(attach.Options{
		Socket: c.Socket, Pane: paneID, In: c.In, Out: c.Out, Cols: cols, Rows: rows, Keys: keys,
		Leave: leave,
	})

	// The alternate-screen exit comes first, right after asking
	// watchResize to stop, not after joining it: resizeDialDeadline
	// bounds that join to a few seconds, but a person sitting at a
	// frozen pane should get their shell back the instant attach
	// returns, not after any wait at all.
	close(resizeDone)
	if isTTY {
		_, _ = io.WriteString(c.Out, "\x1b[?1049l")
	}
	// Joined, not just asked to stop: without waiting for resizeStopped,
	// a SIGWINCH already buffered on sig before signal.Stop below could
	// still be mid-handling, calling termSize and dialing, after this
	// function has returned, sending pane.resize for a pane nobody
	// watches any more.
	<-resizeStopped
	signal.Stop(sig)

	if err != nil {
		fmt.Fprintln(c.Err, err)
		if errors.Is(err, attach.ErrServerGone) {
			return 3
		}
		return 1
	}
	return 0
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
// is running, and a dial that fails is exit 3. The config file is read
// next, on the cooked terminal: a file that does not parse stops the run
// with exit 1, and a missing file is the first run, which finds the
// harnesses on PATH and writes the file. The alternate screen wraps the
// whole run once, so attach's own nested entry and exit inside it change
// nothing.
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

	cfg, found, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read %s: %v\n", config.Path(), err)
		return 1
	}
	if !found {
		cfg, err = tui.FirstRun(config.Discover(exec.LookPath), c.In, c.Out)
		if err != nil {
			return 1
		}
	}
	leave, err := cfg.LeaveKey()
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	talk, err := cfg.TalkKey(leave)
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}

	isTTY := false
	if f, ok := c.Out.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	if isTTY {
		_, _ = io.WriteString(c.Out, "\x1b[?1049h\x1b[2J\x1b[H")
	}
	// A web config that does not read leaves the address empty, which the
	// floor reports as a web server that is off.
	webCfg, _ := web.LoadConfig(web.ConfigPath(c.DataDir))
	views := []string{}
	if ps, _ := plugins.LoadEnabled(cfg.EnabledPlugins()); len(ps) > 0 {
		views = viewIDs(ps)
	}
	err = tui.Run(tui.Options{
		Socket: c.Socket, In: c.In, Out: c.Out, Size: c.floorSize, Cwd: cwd, Default: cfg.Default,
		DataDir: c.DataDir, Leave: leave, Talk: talk,
		Views: views, WebURL: web.LocalURL(webCfg),
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

// runTmuxMirror is coppice tmux-mirror. It drives tmux in control mode and
// keeps one window per open pane in one session. Each window runs coppice
// attach on its pane. It reads pane.list once a second and never sends a
// verb that changes a pane.
func (c *CLI) runTmuxMirror(remote string, argv []string) int {
	if remote != "" {
		fmt.Fprintln(c.Err, "coppice tmux-mirror needs a local socket; it is not wired for --remote yet. "+
			"SSH into the host and run it there.")
		return 1
	}
	session, tmuxSocket := "", ""
	for len(argv) > 0 {
		switch argv[0] {
		case "--session":
			if len(argv) < 2 || argv[1] == "" {
				fmt.Fprintln(c.Err, "--session needs a name")
				return 1
			}
			session, argv = argv[1], argv[2:]
		case "--tmux-socket":
			if len(argv) < 2 || argv[1] == "" {
				fmt.Fprintln(c.Err, "--tmux-socket needs a path")
				return 1
			}
			tmuxSocket, argv = argv[1], argv[2:]
		default:
			fmt.Fprintf(c.Err, "unknown flag %q. coppice tmux-mirror takes --session and --tmux-socket.\n", argv[0])
			return 1
		}
	}
	tmuxPane := os.Getenv("TMUX_PANE")
	if session == "" && tmuxPane == "" {
		fmt.Fprintln(c.Err, "run coppice tmux-mirror inside tmux, or pass --session NAME")
		return 1
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintln(c.Err, "coppice tmux-mirror needs tmux on PATH")
		return 1
	}
	base := []string{}
	if tmuxSocket != "" {
		base = append(base, "-S", tmuxSocket)
	}
	if session == "" {
		out, err := exec.Command(tmux, append(base, "display-message", "-p", "-t", tmuxPane, "#{session_id}")...).Output()
		session = strings.TrimSpace(string(out))
		if err != nil || session == "" {
			fmt.Fprintln(c.Err, "cannot find the tmux session of this pane. Pass --session NAME.")
			return 1
		}
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot find the coppice program: %v\n", err)
		return 1
	}
	sock, err := filepath.Abs(c.Socket)
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read the socket path %s: %v\n", c.Socket, err)
		return 1
	}
	cl, err := dialExisting(sock, 0)
	if err != nil {
		fmt.Fprintf(c.Err, "cannot reach the server at %s. Run: coppice server start\n", sock)
		return 3
	}
	defer cl.Close()

	cmd := exec.Command(tmux, append(base, "-C", "attach-session", "-t", session)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	cmd.Stderr = c.Err
	// The control client runs in its own process group. A hang-up or a
	// SIGTERM to the mirror's group then reaches the mirror and not tmux,
	// so the mirror can still put the status line back through it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(c.Err, "cannot start tmux: %v\n", err)
		return 1
	}
	defer func() { _ = cmd.Wait() }()
	tc := tmuxmirror.NewClient(stdin, stdout)
	defer tc.Close()
	// A control client that takes no output keeps tmux from sending every
	// byte each pane prints.
	if _, err := tc.Do("refresh-client -f no-output"); err != nil {
		fmt.Fprintf(c.Err, "tmux refused the control client: %v\n", err)
		return 1
	}
	m := &tmuxmirror.Mirror{Tmux: tc, Session: session, Exe: exe, Socket: sock,
		Log: func(line string) { fmt.Fprintf(c.Err, "coppice tmux-mirror: %s\n", line) },
		OneShot: func(args []string) error {
			return exec.Command(tmux, append(append([]string{}, base...), args...)...).Run()
		}}
	if err := m.Start(); err != nil {
		fmt.Fprintf(c.Err, "tmux refused to show the status line of %s: %v\n", session, err)
		return 1
	}
	// Each way out puts the session's status line back, so no count stays
	// on screen that no pane backs. When the control client is gone, Stop
	// runs tmux once instead.
	defer func() { _ = m.Stop() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	fmt.Fprintf(c.Err, "coppice tmux-mirror: mirroring panes into tmux session %s. ctrl-c stops it.\n", session)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		res, err := cl.Do("pane.list", nil)
		if err != nil {
			return c.reportDoErr(cl, err)
		}
		if err := m.Sync(tmuxmirror.PanesFromList(res)); err != nil {
			select {
			case <-tc.Done():
				fmt.Fprintln(c.Err, "tmux ended the mirror")
				return 0
			default:
			}
			fmt.Fprintf(c.Err, "coppice tmux-mirror: %v\n", err)
			return 1
		}
		// Wait for the next tick. A window close syncs at once, and any
		// other notification only waits on.
	wait:
		for {
			select {
			case <-tick.C:
				break wait
			case ev := <-tc.Events():
				m.Handle(ev)
				if ev.Kind == tmuxmirror.EventWindowClose {
					break wait
				}
			case <-tc.Done():
				fmt.Fprintln(c.Err, "tmux ended the mirror")
				return 0
			case <-sig:
				return 0
			}
		}
	}
}
