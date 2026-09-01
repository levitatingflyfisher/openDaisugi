// Package cli is the `daisugi` command: the gate commands and the gate
// half of `install`, with the flags, files and output of the Python CLI
// (opendaisugi.cli). A command or flag it does not carry says so in one
// line and exits 2, changing nothing.
//
// The CLI never runs Python. `daisugi gate check`, the hook entry an
// installed hook runs, answers every call through internal/gate, decided
// in a child process of this binary; a call the gate cannot decide is
// denied with a reason.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"daisugi-verify/internal/gate"
	"daisugi-verify/internal/gateroot"
)

// Version is the build's version, set at link time:
//
//	go build -ldflags "-X daisugi-verify/internal/cli.Version=0.44.0"
//
// scripts/release.sh sets the release version, and scripts/install.sh the
// git describe of the checkout. A build that sets none reports the commit
// Go stamps into it (see version).
var Version = ""

// version is Version, or the VCS revision go build records (short, with
// -dirty for a tree with changes), or "unknown".
func version() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	return "unknown"
}

// Env is one run's world: arguments, streams and environment.
type Env struct {
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Environ []string
	// GateExe, when set, is this binary: gate check then decides each call
	// in a child process of it, so no abnormal end reads as an allow
	// (gate.RunGuardedAs). Empty runs the gate in this process.
	GateExe string

	env map[string]string
	// miseWhich runs `mise which daisugi` (with --tool when tool is set);
	// nil runs the mise on PATH.
	miseWhich func(tool string) (string, error)
	in        *bufio.Reader
	home      string
	// tendFailed is set while auto-tend runs tend: a failure prints as
	// "tend failed: ..." and auto-tend goes on.
	tendFailed bool
	// raised is set when tend's Daisugi() raised: auto-tend does not
	// catch that one.
	raised bool
	prog   string
}

func (e *Env) out(format string, a ...any)  { fmt.Fprintf(e.Stdout, format, a...) }
func (e *Env) errf(format string, a ...any) { fmt.Fprintf(e.Stderr, format, a...) }

// exitError ends a command with a code after its message was printed.
type exitError struct{ code int }

func (x *exitError) Error() string { return fmt.Sprintf("exit %d", x.code) }

func exit(code int) error { return &exitError{code} }

// notYet is the one line a command or flag this binary does not carry
// prints before exiting 2.
func (e *Env) notYet(what string) error {
	e.errf("%s is not in this binary yet.\n", what)
	return exit(2)
}

// refuse ends a command whose input holds something this binary cannot
// yet read the way Python does. It exits 2 before anything is written.
func (e *Env) refuse(cmd string, err error) error {
	e.errf("daisugi %s: %v. Nothing was changed.\n", cmd, err)
	return exit(2)
}

// fail is an error Python meets too (it raises); exit 1 with the reason.
func (e *Env) fail(cmd string, err error) error {
	if e.tendFailed {
		e.errf("%s\n", tendFailure(err.Error()))
		return exit(1)
	}
	e.errf("daisugi %s: %v\n", cmd, err)
	return exit(1)
}

const startHere = `daisugi: a gate that proves each agent action stays inside an envelope, and a garden
of reusable pathways distilled from the work.

Start here
  daisugi start                          gate this directory (shadow mode) and watch it
  daisugi start --enforce                the same, but out-of-envelope calls are denied
  daisugi orchestrate "list three risks" run a prompt end to end under a verified plan
  daisugi status                         is this machine ready; what is installed
  daisugi dashboard                      the live view of sessions, verdicts, and cost

More
  daisugi config                         every setting, with its source
  daisugi help --all                     every command, grouped
`

// notInBinary is every other command of the Python CLI, so --help shows
// what this binary does not carry.
var notInBinary = []string{
	"start", "status", "dashboard", "orchestrate", "config", "help", "journal",
	"batch", "bench", "conformance", "coppice", "gateway",
	"gateway-report", "generate-envelope", "lora", "mcp", "metrics", "models",
	"modules", "onboard", "registry", "release", "route", "router", "run", "setup",
	"tiers", "verify", "viz", "voice",
	"gate replay", "gate audit", "gate serve",
}

const rootHelp = `Usage: daisugi [OPTIONS] COMMAND [ARGS]...

  Runtime assurance for agent actions. This is the daisugi Go binary:
  the gate commands, the gate half of install, and the pathway store.
  gate check answers every call natively.

Options:
  --version        Show the opendaisugi version and exit.
  --plain          No color, no box drawing; greppable output.
  -q, --quiet      Results only; no progress notes.
  -v, --verbose    Show tracebacks and detail.
  --no-color       Disable color (same as NO_COLOR=1).
  --help           Show this message and exit.

Gate:
  gate check       Answer one tool-call hook: the verdict for the payload on stdin.
  gate init        Register a reviewable starter envelope for this session.
  gate register    Register the envelope the gate checks this session's calls against.
  gate arm         Remove the disarm marker; the gate resumes evaluating calls.
  gate disarm      Kill switch: the gate allows everything until re-armed.
  gate status      Show armed/disarmed state, the verdict mode, and the envelopes.
  gate report      Summarize the shadow log: what an enforcing gate would have denied.
  gate settings    Print the Claude Code hooks-settings JSON that wires in the gate.
  gate proposals   List recorded envelope-edit proposals.
  install          Wire the gate into agent harnesses (--gate, --harness).

Pathways:
  pathways list    List all compiled pathways.
  pathways show    Show a compiled pathway in detail.
  pathways stats   Summarize stored pathways (count, total hits).
  pathways delete  Delete a compiled pathway.
  pathways export  Export a pathway: json, skill, mermaid, md or smtlib.
  pathways import  Import a pathway bundle, re-verify it, and admit it.

Garden:
  gardener prune   Evict stale / failure-dominated pathways.
  gardener merge   Collapse near-duplicate pathways.
  gardener run     Run the full gardener pipeline (prune + merge).
  gardener watch   Cron-friendly one-shot gardener.
  gardener status  Report store size, activation stats, failure ratios.
  tend             Distil successful journal traces into pathways.

Not yet in this binary (use the Python CLI, opendaisugi.cli):
`

// Main runs one command and returns its exit code.
func Main(e *Env) int {
	e.env = map[string]string{}
	for _, kv := range e.Environ {
		k, v, _ := strings.Cut(kv, "=")
		e.env[k] = v
	}
	e.in = bufio.NewReader(e.Stdin)
	e.prog = "daisugi"
	err := e.run(e.Args)
	if err == nil {
		return 0
	}
	var x *exitError
	if errors.As(err, &x) {
		return x.code
	}
	var u *usageError
	if errors.As(err, &u) {
		e.errf("Error: %s\n", u.msg)
		return 2
	}
	e.errf("daisugi: %v\n", err)
	return 1
}

func (e *Env) usage(cmd string, err error) error {
	var u *usageError
	if errors.As(err, &u) {
		e.errf("Usage: daisugi %s [OPTIONS]\nTry 'daisugi %s --help' for help.\n\nError: %s\n", cmd, cmd, u.msg)
		return exit(2)
	}
	return err
}

// homeDir is Path.home(): $HOME as pathlib prints it.
func (e *Env) homeDir() (string, error) {
	h := e.env["HOME"]
	if h == "" || !strings.HasPrefix(h, "/") {
		return "", errors.New("HOME is unset or not an absolute path")
	}
	return gateroot.PathStr(h), nil
}

func (e *Env) run(args []string) error {
	// Root options come before the command.
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "--version":
			e.out("%s\n", version())
			return nil
		case "--help":
			return e.rootHelp()
		case "--plain", "-q", "--quiet", "-v", "--verbose", "--no-color":
			continue
		}
		break
	}
	args = args[i:]
	if len(args) == 0 {
		e.out("%s", startHere)
		return nil
	}
	if strings.HasPrefix(args[0], "-") {
		return e.usage("", &usageError{"No such option: " + args[0]})
	}
	if len(args) >= 2 && args[0] == "gate" && args[1] == "check" {
		// The hook entry: the gate package reads the environment itself,
		// and a refusal here would be exit 2, a deny, even in shadow.
		return e.gateCheck(args[2:])
	}
	home, err := e.homeDir()
	if err != nil {
		return e.refuse(args[0], err)
	}
	e.home = home
	switch args[0] {
	case "gate":
		return e.gate(args[1:])
	case "install":
		return e.install(args[1:])
	case "pathways":
		return e.pathways(args[1:])
	case "gardener":
		return e.gardener(args[1:])
	case "tend":
		return e.tend(args[1:])
	case "hook":
		return e.hook(args[1:])
	case "distill-repeats":
		return e.distillRepeats(args[1:])
	}
	for _, name := range notInBinary {
		if name == args[0] {
			return e.notYet("daisugi " + args[0])
		}
	}
	e.errf("Usage: daisugi [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

func (e *Env) rootHelp() error {
	e.out("%s", rootHelp)
	names := append([]string{}, notInBinary...)
	sort.Strings(names)
	line := " "
	for _, n := range names {
		if len(line)+len(n)+2 > 78 {
			e.out("%s\n", line)
			line = " "
		}
		line += " " + n + ","
	}
	e.out("%s\n", strings.TrimSuffix(line, ","))
	return nil
}

var gateHelp = `Usage: daisugi gate [OPTIONS] COMMAND [ARGS]...

  Call-time tool gate (ADR-0007): verify each live tool call against a
  registered envelope. Shadow by default; --mode enforce denies.

Commands:
  check      Answer one hook payload on stdin with the host's verdict.
  init       Generate and register a reviewable starter envelope.
  register   Register the envelope the gate checks calls against.
  disarm     Kill switch: the gate allows everything until re-armed.
  arm        Remove the disarm marker; the gate resumes evaluating calls.
  status     Show armed state, the verdict mode, and the envelopes.
  report     Summarize the shadow log.
  settings   Print the Claude Code hooks-settings JSON for the gate.
  proposals  List recorded envelope-edit proposals.

Not yet in this binary: replay, audit, serve.
`

func (e *Env) gate(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", gateHelp)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "check":
		return e.gateCheck(rest)
	case "init":
		return e.gateInit(rest)
	case "register":
		return e.gateRegister(rest)
	case "disarm":
		return e.gateDisarm(rest)
	case "arm":
		return e.gateArm(rest)
	case "status":
		return e.gateStatus(rest)
	case "report":
		return e.gateReport(rest)
	case "settings":
		return e.gateSettings(rest)
	case "proposals":
		return e.gateProposals(rest)
	case "replay", "audit", "serve":
		return e.notYet("daisugi gate " + sub)
	}
	e.errf("Usage: daisugi gate [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi gate --help' for help.\n\nError: No such command '%s'.\n", sub)
	return exit(2)
}

// gateCheck is the hook entry, the contract of `python -m opendaisugi.gate`:
// the same flags, stdin, stdout, stderr and exit code, answered by the gate
// package in this process.
func (e *Env) gateCheck(args []string) error {
	for _, a := range args {
		if a == "--help" {
			e.out("%s", checkHelp)
			return nil
		}
	}
	stdin, err := io.ReadAll(io.LimitReader(e.in, gate.MaxPayloadBytes+1))
	if err != nil {
		stdin = nil
	}
	var res gate.Result
	frame := gate.ChildPipe(e.env[gate.ChildEnv])
	switch {
	case frame != nil:
		// The child of a guarded run: one frame for the parent, and the
		// format's deny on its own stdout and exit code.
		frame.Write(gate.ChildFrame(args, stdin, e.Environ))
		frame.Close()
		res = gate.ChildDeny(args)
		io.WriteString(e.Stdout, res.Stdout)
		io.WriteString(e.Stderr, res.Stderr)
		if res.Exit != 0 {
			return exit(res.Exit)
		}
		return nil
	case e.GateExe != "":
		res = gate.RunGuardedAs(e.GateExe, []string{"gate", "check"}, args, stdin,
			gate.ChildEnviron(e.Environ), gate.ChildDeadline(args))
	default:
		res = gate.Run(args, stdin, e.Environ)
	}
	if trace := e.env["DAISUGI_GATE_TRACE"]; trace != "" {
		writeTrace(trace, res)
	}
	io.WriteString(e.Stdout, res.Stdout)
	io.WriteString(e.Stderr, res.Stderr)
	if res.Exit != 0 {
		return exit(res.Exit)
	}
	return nil
}

const checkHelp = `Usage: daisugi gate check --mode shadow|enforce [OPTIONS] < payload.json

  Read one hook payload from stdin and emit the host's verdict contract.
  On the Claude Code path a deny is exit code 2 with the reason on stderr.
  This is the command an installed hook runs.

Options:
  --mode shadow|enforce     shadow observes and logs; enforce denies.
  --root PATH               Gate state directory (default ~/.opendaisugi/gate).
  --format NAME             Host contract: claude | pi | opencode | hermes | openclaw.
  --verify-timeout SECONDS  Inner verifier budget; running out of it denies.
  --captures-root PATH      Also mirror each call into this captures directory.
  --session ID              Check against this registered session's envelope.
`

// writeTrace appends one JSON line saying whether the gate answered the
// call (native), denied it undecided (unported) or denied it after the
// child ended abnormally (crash), and why, as cmd/daisugi-gate does. It
// never changes stdout, stderr or the exit code.
func writeTrace(path string, res gate.Result) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	how := "native"
	switch {
	case res.Crashed:
		how = "crash"
	case !res.Native:
		how = "unported"
	}
	line, _ := json.Marshal(map[string]any{"path": how, "why": res.Why, "exit": res.Exit})
	f.Write(append(line, '\n'))
}
