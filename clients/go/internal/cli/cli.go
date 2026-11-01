// Package cli is the `daisugi` command: the warden commands and the warden
// half of `install`, with the flags, files and output of the Python CLI
// (opendaisugi.cli). A command or flag it does not carry says so in one
// line and exits 2, changing nothing.
//
// The CLI never runs Python. `daisugi warden check`, the hook entry an
// installed hook runs, answers every call through internal/warden, decided
// in a child process of this binary; a call the warden cannot decide is
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

	"daisugi-verify/internal/datahome"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/warden"
	"daisugi-verify/internal/wardenroot"
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
	// WardenExe, when set, is this binary: warden check then decides each call
	// in a child process of it, so no abnormal end reads as an allow
	// (warden.RunGuardedAs). Empty runs the warden in this process.
	WardenExe string

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
	// quiet is the root -q: progress notes are not printed.
	quiet bool
	// plain is the root --plain: no box drawing.
	plain bool
	// noColor is --no-color: the same as NO_COLOR=1.
	noColor bool
	// checkClient is the model client llm_check calls go through.
	checkClient *llm.Client
	// selfPath and execFn stand in for install.Self and syscall.Exec in
	// tests of the DAISUGI_PORT hand-over.
	selfPath string
	execFn   func(argv0 string, argv, env []string) error
	// unitHookFn stands in for systemctl in tests of install and uninstall.
	unitHookFn func(action, unit string) error
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

const startHere = `daisugi: a warden that proves each agent action stays inside an envelope, and a garden
of reusable pathways distilled from the work.

Start here
  daisugi start                          warden this directory (audit mode) and watch it
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
	"bench", "conformance", "coppice",
	"warden audit",
}

const rootHelp = `Usage: daisugi [OPTIONS] COMMAND [ARGS]...

  Runtime assurance for agent actions. This is the daisugi Go binary:
  the warden commands, the warden half of install, and the pathway store.
  warden check answers every call natively.

Options:
  --version        Show the opendaisugi version and exit.
  --plain          No color, no box drawing; greppable output.
  -q, --quiet      Results only; no progress notes.
  -v, --verbose    Show tracebacks and detail.
  --no-color       Disable color (same as NO_COLOR=1).
  --help           Show this message and exit.

Start here:
  start            Warden this directory (audit by default) and open the live view.
  status           Show day-one readiness: are token savings live and are actions verified?
  config           Show every setting as daisugi will use it, and where each one came from.
  dashboard        The live floor: the module map with gauges read from the stores.
  help             Show the short start-here text, or every command with --all.

View:
  modules          Show the module wiring: what is active, available, or an open slot.
  metrics          Prometheus metrics: the exposition text, or a /metrics endpoint.

Warden:
  warden check       Answer one tool-call hook: the verdict for the payload on stdin.
  warden init        Register a reviewable starter envelope for this session.
  warden register    Register the envelope the warden checks this session's calls against.
  warden arm         Remove the disarm marker; the warden resumes evaluating calls.
  warden disarm      Kill switch: the warden allows everything until re-armed.
  warden status      Show armed/disarmed state, the verdict mode, and the envelopes.
  warden report      Summarize the audit log: what an enforcing warden would have denied.
  warden settings    Print the Claude Code hooks-settings JSON that wires in the warden.
  warden proposals   List recorded envelope-edit proposals.
  warden replay      Replay a captured session through the warden offline (nothing executes).
  warden serve       Run the resident warden in the foreground (Ctrl-C to stop).
  install          Wire the warden into agent harnesses (--enforce, --harness).

Pathways:
  pathways list    List all compiled pathways.
  pathways show    Show a compiled pathway in detail.
  pathways stats   Summarize stored pathways (count, total hits).
  pathways delete  Delete a compiled pathway.
  pathways export  Export a pathway: json, skill, mermaid, md or smtlib.
  pathways import  Import a pathway bundle, re-verify it, and admit it.
  viz              Render a distilled pathway's plan as a standalone page.

Garden:
  gardener prune   Evict stale / failure-dominated pathways.
  gardener merge   Collapse near-duplicate pathways.
  gardener run     Run the full gardener pipeline (prune + merge).
  gardener watch   Cron-friendly one-shot gardener.
  gardener status  Report store size, activation stats, failure ratios.
  tend             Distil successful journal traces into pathways.

Router:
  router          Run the token-saving router: a local proxy a harness points at.
  router report   Calibrate the router on its journal: realized routing, reuse ceiling.
  router status    Show the routing mode, the Switchyard binary and child, recent turns.
  router stop      Stop every switchyard-server a router left running.
  route            Recommend the cheapest viable model or tier for a task.

Envelopes:
  generate-envelope  Generate a safety envelope for a task from a model.

Run:
  orchestrate      Run a prompt end to end: decompose, size, supervised execute, synthesize.
  run              Execute a plan against an envelope under runtime supervision.
  verify           Verify an action plan against a safety envelope.

Setup:
  onboard          Turn existing agent transcripts into verified traces and pathways.
  tiers setup      Detect hardware, recommend a local model, and optionally qualify and wire it.
  tiers stats      Show per-tier call counts, estimated tokens, and pathway hit rate.
  models list      List the curated models, the default for this box, and the one in use.
  models search    Search the Hugging Face API for models, filtered by size and license.
  models use       Record the model the garden uses. Any id, path or GGUF file is accepted.
  pack list        List the ML packs and which are installed.
  pack install     Install a pack: a pinned Python, a venv, the locked wheels.
  pack run         Run one job (selftest, train, vla-chunk) in a pack's worker.
  lora train       Train a LoRA adapter in the train pack.
  mcp serve        Serve the openDaisugi tools over MCP stdio.
  backup           Write your journal, pathways, envelopes and config to one .tgz.
  restore          Put a backup back; --force moves the old data aside first.

Registry and release:
  registry init    Clone a registry repo to a local directory.
  registry pull    git pull and materialize new pathway bundles into the local cache.
  registry publish Sign + commit + push a local pathway as a bundle to the registry.
  registry status  Show the local clone's diagnostic info.
  registry pull-and-tend  Pull new bundles from the registry, then run tend.
  batch prove      Statically prove a declared batch before any iteration.
  release keygen   Generate an ed25519 release-signing keypair.
  release sign     Build a SHA-256 manifest over ARTIFACTS and sign it.
  release verify   Verify a release: trusted signature AND intact artifacts.

Journal:
  journal search   Search journal traces by task (the lexical and potion matchers).
  journal replay   Re-run verify() on a stored trace and report drift.
  journal stats    Print aggregate stats from the journal index.
  journal parse    Parse an agent transcript into episodes.
  journal ingest   Ingest parsed episodes into the journal.

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

// dataHome is opendaisugi.datahome.data_home() for this command's
// environment and home: OPENDAISUGI_HOME, else XDG_DATA_HOME/opendaisugi
// when ~/.opendaisugi does not exist, else ~/.opendaisugi.
func (e *Env) dataHome() string {
	return datahome.Dir(func(k string) string { return e.env[k] }, e.home, datahome.Exists)
}

// homeDir is Path.home(): $HOME as pathlib prints it.
func (e *Env) homeDir() (string, error) {
	h := e.env["HOME"]
	if h == "" || !strings.HasPrefix(h, "/") {
		return "", errors.New("HOME is unset or not an absolute path")
	}
	return wardenroot.PathStr(h), nil
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
		case "-q", "--quiet":
			e.quiet = true
			continue
		case "--plain":
			e.plain = true
			continue
		case "--no-color":
			e.noColor = true
			continue
		case "-v", "--verbose":
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
	if len(args) >= 2 && args[0] == "warden" && args[1] == "check" {
		// The hook entry: the warden package reads the environment itself,
		// and a refusal here would be exit 2, a deny, even in audit.
		return e.wardenCheck(args[2:])
	}
	if args[0] == "fence" {
		// First, before any config or store: the fence costs one exec.
		return e.fenceCmd(args[1:])
	}
	home, err := e.homeDir()
	if err != nil {
		return e.refuse(args[0], err)
	}
	e.home = home
	verify.LLM = e.llmCheck
	switch args[0] {
	case "warden":
		return e.warden(args[1:])
	case "help":
		return e.helpCmd(args[1:])
	case "viz":
		return e.vizCmd(args[1:])
	case "install":
		return e.install(args[1:])
	case "recipe":
		return e.recipeCmd(args[1:])
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
	case "router":
		return e.router(args[1:])
	case "graft":
		return e.graft(args[1:])
	case "rank":
		return e.rankCmd(args[1:])
	case "pando":
		return e.pandoCmd(args[1:])
	case "tree":
		return e.treeCmd(args[1:])
	case "agent-profiles":
		return e.agentProfilesCmd(args[1:])
	case "route":
		return e.route(args[1:])
	case "config":
		return e.configCmd(args[1:])
	case "status":
		return e.statusCmd(args[1:])
	case "journal":
		return e.journal(args[1:])
	case "start":
		return e.start(args[1:])
	case "generate-envelope":
		return e.generateEnvelope(args[1:])
	case "run":
		return e.runCmd(args[1:])
	case "weave":
		return e.weaveCmd(args[1:])
	case "orchestrate":
		return e.orchestrateCmd(args[1:])
	case "mcp":
		return e.mcpCmd(args[1:])
	case "onboard":
		return e.onboard(args[1:])
	case "tiers":
		return e.tiers(args[1:])
	case "models":
		return e.modelsCmd(args[1:])
	case "pack":
		return e.packCmd(args[1:])
	case "lora":
		return e.loraCmd(args[1:])
	case "setup":
		return e.setupMoved(args[1:])
	case "modules":
		return e.modulesCmd(args[1:])
	case "dashboard":
		return e.dashboardCmd(args[1:])
	case "metrics":
		return e.metricsCmd(args[1:])
	case "registry":
		return e.registryCmd(args[1:])
	case "batch":
		return e.batchCmd(args[1:])
	case "release":
		return e.release(args[1:])
	case "voice":
		return e.voiceCmd(args[1:])
	case "verify":
		return e.verifyCmd(args[1:])
	case "backup":
		return e.backupCmd(args[1:])
	case "restore":
		return e.restoreCmd(args[1:])
	case "gate": // names-ok: a hook written before the rename still calls this
		// Fail closed, and say why: exit 2 blocks the call.
		e.errf("daisugi gate is now daisugi warden; run daisugi install to update the hook\n") // names-ok
		return exit(2)
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

var wardenHelp = `Usage: daisugi warden [OPTIONS] COMMAND [ARGS]...

  Call-time tool warden (ADR-0007): verify each live tool call against a
  registered envelope. Audit by default; --mode enforce denies.

Commands:
  check      Answer one hook payload on stdin with the host's verdict.
  init       Generate and register a reviewable starter envelope.
  register   Register the envelope the warden checks calls against.
  disarm     Kill switch: the warden allows everything until re-armed.
  arm        Remove the disarm marker; the warden resumes evaluating calls.
  status     Show armed state, the verdict mode, and the envelopes.
  report     Summarize the audit log.
  settings   Print the Claude Code hooks-settings JSON for the warden.
  proposals  List recorded envelope-edit proposals.
  serve      Run the resident warden in the foreground (Ctrl-C to stop).
  schemas    The MCP tool schemas on file, which warden register checks
             mcp_rules against.

Not yet in this binary: replay, audit.
`

func (e *Env) warden(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", wardenHelp)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "check":
		return e.wardenCheck(rest)
	case "init":
		return e.wardenInit(rest)
	case "register":
		return e.wardenRegister(rest)
	case "schemas":
		return e.wardenSchemas(rest)
	case "disarm":
		return e.wardenDisarm(rest)
	case "arm":
		return e.wardenArm(rest)
	case "status":
		return e.wardenStatus(rest)
	case "report":
		return e.wardenReport(rest)
	case "settings":
		return e.wardenSettings(rest)
	case "proposals":
		return e.wardenProposals(rest)
	case "serve":
		return e.wardenServe(rest)
	case "replay":
		return e.wardenReplay(rest)
	case "audit":
		return e.notYet("daisugi warden " + sub)
	}
	e.errf("Usage: daisugi warden [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi warden --help' for help.\n\nError: No such command '%s'.\n", sub)
	return exit(2)
}

// wardenCheck is the hook entry, the contract of `python -m opendaisugi.warden`:
// the same flags, stdin, stdout, stderr and exit code, answered by the warden
// package in this process.
func (e *Env) wardenCheck(args []string) error {
	for _, a := range args {
		if a == "--help" {
			e.out("%s", checkHelp)
			return nil
		}
	}
	stdin, err := io.ReadAll(io.LimitReader(e.in, warden.MaxPayloadBytes+1))
	if err != nil {
		stdin = nil
	}
	var res warden.Result
	frame := warden.ChildPipe(e.env[warden.ChildEnv])
	switch {
	case frame != nil:
		// The child of a guarded run: one frame for the parent, and the
		// format's deny on its own stdout and exit code.
		frame.Write(warden.ChildFrame(args, stdin, e.Environ))
		frame.Close()
		res = warden.ChildDeny(args)
		io.WriteString(e.Stdout, res.Stdout)
		io.WriteString(e.Stderr, res.Stderr)
		if res.Exit != 0 {
			return exit(res.Exit)
		}
		return nil
	case warden.Fenced(e.Environ):
		// Inside daisugi fence the warden's state is read only: the resident
		// warden, outside the fence, decides and writes (FN-R-12).
		res = warden.FencedCheck(args, stdin, e.Environ)
	case e.WardenExe != "":
		res = warden.RunGuardedAs(e.WardenExe, []string{"warden", "check"}, args, stdin,
			warden.ChildEnviron(e.Environ), warden.ChildDeadline(args))
	default:
		res = warden.Run(args, stdin, e.Environ)
	}
	if trace := e.env["DAISUGI_WARDEN_TRACE"]; trace != "" {
		writeTrace(trace, res)
	}
	io.WriteString(e.Stdout, res.Stdout)
	io.WriteString(e.Stderr, res.Stderr)
	if res.Exit != 0 {
		return exit(res.Exit)
	}
	return nil
}

const checkHelp = `Usage: daisugi warden check --mode audit|enforce [OPTIONS] < payload.json

  Read one hook payload from stdin and emit the host's verdict contract.
  On the Claude Code path a deny is exit code 2 with the reason on stderr.
  This is the command an installed hook runs.

Options:
  --mode audit|enforce      audit observes and logs; enforce denies.
  --root PATH               Warden state directory (default: the data directory's warden).
  --format NAME             Host contract: claude | pi | opencode | hermes | openclaw.
  --verify-timeout SECONDS  Inner verifier budget; running out of it denies.
  --captures-root PATH      Also mirror each call into this captures directory.
  --session ID              Check against this registered session's envelope.
`

// writeTrace appends one JSON line saying whether the warden answered the
// call (native), denied it undecided (unported) or denied it after the
// child ended abnormally (crash), and why, as cmd/daisugi-warden does. It
// never changes stdout, stderr or the exit code.
func writeTrace(path string, res warden.Result) {
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
