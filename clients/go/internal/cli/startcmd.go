package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/wardenroot"
)

// startStep is start.StartStep.
type startStep struct{ key, state, text string }

// The view step as the oracle words it without the Textual extra: this
// binary opens the plain live view (K4-5). The warden-server step starts
// this binary's `warden serve`, as Python's start starts its own (G2-4).
const startViewText = "open the live view (daisugi dashboard); install the tui extra for the full instrument"

// sessionKey is start._session_key: the resolved directory's name, made
// filename-safe, and 8 hex digits of the SHA-256 of its path.
func sessionKey(cwd string) (string, error) {
	resolved, err := wardenroot.Resolve(cwd)
	if err != nil {
		return "", err
	}
	base := resolved
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	for _, r := range base {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "._")
	if name == "" {
		name = "start"
	}
	sum := sha256.Sum256([]byte(resolved))
	return name + "-" + hex.EncodeToString(sum[:])[:8], nil
}

// start is `daisugi start`: the warden hook scoped to this directory, a
// starter envelope keyed to it, then the plain live view (K4-5).
func (e *Env) start(args []string) error {
	const cmd = "start"
	opts := []opt{
		{names: []string{"--enforce"}, help: "Install the warden in enforce mode (default: audit)."},
		{names: []string{"--ask"}, help: "Let the warden hand a would-deny to a present operator. Needs --enforce."},
		{names: []string{"--no-ui"}, help: "Do the setup, do not open the view."},
		{names: []string{"--dry-run"}, help: "Show the steps and their state; change nothing."},
		dataDirOpt,
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Get a checked session over this directory and watch it in the live view.", opts)
	}
	enforce, ask, noUI, dry := p.flag("--enforce"), p.flag("--ask"), p.flag("--no-ui"), p.flag("--dry-run")
	if ask && !enforce {
		e.errf("the --ask flag only matters in --enforce mode.\n" +
			"in audit mode nothing is ever denied, so there is nothing to ask about.\n" +
			"pass --enforce --ask together, or drop --ask.\n")
		return exit(2)
	}
	cwd, err := wardenroot.Getwd()
	if err != nil {
		return e.fail(cmd, err)
	}
	cwd = wardenroot.PathStr(cwd)
	dataDir := e.dataDir(p)
	steps, err := e.startSteps(cwd, dataDir, enforce, ask, !dry, noUI)
	if err != nil {
		return err
	}
	width := 0
	for _, s := range steps {
		width = max(width, pystr.Len(s.key))
	}
	failed := false
	for _, s := range steps {
		e.out("  %s%s  %s%s  %s\n", s.key, strings.Repeat(" ", width-pystr.Len(s.key)),
			s.state, strings.Repeat(" ", max(0, 7-pystr.Len(s.state))), s.text)
		failed = failed || s.state == "failed"
	}
	if failed {
		return exit(1)
	}
	if !dry && !e.quiet && e.unconfined(wardenroot.Join(dataDir, "warden")) {
		e.errf("%s\n", unconfinedLine)
	}
	if dry || noUI {
		return nil
	}
	if !e.quiet {
		e.errf("opening the live view…\n")
	}
	// Python's start does not catch Ctrl-C: click ends the view with
	// "Aborted!" and exit 1.
	return e.runLive(cmd, dataDir, 2.0, func() error {
		e.errf("\nAborted!\n")
		return exit(1)
	})
}

func (e *Env) startSteps(cwd, dataDir string, enforce, ask, act, noUI bool) ([]startStep, error) {
	const cmd = "start"
	view := startStep{"view", "would", startViewText}
	if noUI {
		view.state = "skipped"
	}
	root := wardenroot.Join(dataDir, "warden")
	mode := "audit"
	if enforce {
		mode = "enforce"
	}
	settings := wardenroot.Join(cwd, ".claude/settings.json")
	key, err := sessionKey(cwd)
	if err != nil {
		return nil, e.fail(cmd, err)
	}
	var out []startStep
	claude, err := install.LookPath(e.env)("claude")
	if err != nil {
		return append(out,
			startStep{"harness", "failed", "Tried to find the agent harness. No `claude` on PATH. " +
				"Install Claude Code, then run `daisugi start` again."},
			startStep{"hook", "skipped", "no harness to hook into"},
			startStep{"envelope", "skipped", "no harness session to register an envelope for"},
			startStep{"warden-server", "skipped", "no harness session needs it yet"},
			view), nil
	}
	out = append(out, startStep{"harness", "done", "Claude Code found at " + claude})
	// The hook bakes the warden root. A relative one is refused before any
	// write, in a dry run too: a plan must not promise a hook it cannot write.
	if err := install.CheckWardenRoot(root); err != nil {
		return nil, e.refuse(cmd, err)
	}
	unreadable := func() error {
		return e.refuse(cmd, errors.New("a Claude Code settings.json holds hooks this binary does not read yet"))
	}
	global := wardenroot.Join(e.home, ".claude/settings.json")
	globalNote := ""
	if global != settings {
		gm, err := config.InstalledHookMode(global)
		if err != nil {
			return nil, unreadable()
		}
		if gm != "" {
			globalNote = fmt.Sprintf(" (note: a machine-global warden hook is ALSO installed at %s, mode: %s — "+
				"`daisugi install` put it there; it fires on every directory's sessions, including this one)",
				global, gm)
		}
	}
	installed, err := config.InstalledHookMode(settings)
	if err != nil {
		return nil, unreadable()
	}
	// Everything the run writes is checked before the first write.
	var edit *install.Edit
	var self string
	if installed == "" && act {
		if self, err = install.Self(); err != nil {
			return nil, e.fail(cmd, err)
		}
		self, _ = install.HookPath(self, e.env, e.miseWhich)
		s := key
		entry := install.HookEntry(self, install.HookOptions{Mode: mode, Root: root, Session: &s, Ask: ask})
		if edit, err = install.PlanClaudeWarden(settings, entry); err != nil {
			return nil, e.refuse(cmd, err)
		}
	}
	// `start --enforce` over an audit project hook rewrites it in place, as
	// `install --enforce` does, and says so in one line. Never steps down.
	var stepUp *install.Edit
	if installed == "audit" && mode == "enforce" {
		if self, err = install.Self(); err != nil {
			return nil, e.fail(cmd, err)
		}
		self, _ = install.HookPath(self, e.env, e.miseWhich)
		s := key
		entry := install.HookEntry(self, install.HookOptions{Mode: mode, Root: root, Session: &s, Ask: ask})
		up, err := install.PlanClaudeWarden(settings, entry)
		if err != nil {
			return nil, e.refuse(cmd, err)
		}
		if up != nil && up.Warning == "" && !up.NoteOnly && up.Note != "" {
			stepUp = up
		}
	}
	envDir := wardenroot.EnvelopesDir(root)
	envPath := wardenroot.Join(envDir, key+".json")
	_, envErr := os.Stat(envPath)
	var env *pyjson.Object
	if envErr != nil && act {
		ws, err := wardenroot.Resolve(cwd)
		if err != nil {
			return nil, e.fail(cmd, err)
		}
		o, err := wardenroot.StarterEnvelope(ws, false)
		if err != nil {
			return nil, e.fail(cmd, err)
		}
		env = o
	}
	switch {
	case stepUp != nil && act:
		followed, err := install.Apply(stepUp)
		e.noteFile(settings, followed)
		if err != nil {
			return nil, e.failPy(cmd, err)
		}
		out = append(out, startStep{"hook", "done", stepUp.Note + globalNote})
	case stepUp != nil:
		out = append(out, startStep{"hook", "would", "would change the warden hook: " + stepUp.Note + globalNote})
	case installed != "":
		hint := ""
		if installed != mode {
			hint = fmt.Sprintf("; it's already %s — to switch to %s, remove the warden hook line from %s and run "+
				"`daisugi start` again", installed, mode, settings)
		}
		out = append(out, startStep{"hook", "skipped", fmt.Sprintf("a warden hook is already installed in %s "+
			"(mode: %s, this directory only)%s%s", settings, installed, hint, globalNote)})
	case act:
		if err := os.MkdirAll(wardenroot.Parent(settings), 0o777); err != nil {
			return nil, e.failPy(cmd, err)
		}
		if edit != nil && edit.Warning != "" {
			e.errf("warning: %s\n", edit.Warning)
		}
		followed, err := install.Apply(edit)
		if edit != nil {
			e.noteFile(settings, followed)
		}
		if err != nil {
			return nil, e.failPy(cmd, err)
		}
		out = append(out, startStep{"hook", "done", fmt.Sprintf("installed the warden hook in %s mode into %s "+
			"(this directory only)%s — a project-settings hook needs Claude Code to trust this folder before it "+
			"fires; confirm it did with `daisugi warden report`", mode, settings, globalNote)})
	default:
		out = append(out, startStep{"hook", "would", fmt.Sprintf("install the warden hook in %s mode into %s "+
			"(this directory only)%s", mode, settings, globalNote)})
	}
	switch {
	case envErr == nil:
		out = append(out, startStep{"envelope", "skipped",
			"an envelope for this directory is already registered at " + envPath})
	case act:
		if _, err := wardenroot.Register(env, key, root); err != nil {
			return nil, e.failPy(cmd, err)
		}
		out = append(out, startStep{"envelope", "done",
			fmt.Sprintf("registered a starter envelope for %s at %s", cwd, envPath)})
	default:
		out = append(out, startStep{"envelope", "would", "register a starter envelope for " + cwd})
	}
	sock := wardenroot.Join(root, "warden.sock")
	// A socket file is not a running warden: a server stopped by SIGKILL
	// leaves it behind. Only one that answers is.
	state := probeWarden(sock)
	switch {
	case state == wardenLive:
		out = append(out, startStep{"warden-server", "skipped", "the resident warden is already running"})
	case act:
		if state == wardenStale {
			_ = syscall.Unlink(sock)
		}
		// start._detach: this binary's `warden serve`, in a session of its
		// own, with no terminal; then the socket must appear.
		// The first server wins, so start fixes the captures root the
		// fence fixes (FN-R-32, FN-R-35).
		startWardenSpawn(e, root, wardenCapturesArgs(root)...)
		t0 := time.Now()
		for !exists(sock) && time.Since(t0) < startServerWait {
			time.Sleep(50 * time.Millisecond)
		}
		if exists(sock) {
			out = append(out, startStep{"warden-server", "done", "started the resident warden on " + sock})
		} else {
			out = append(out, startStep{"warden-server", "failed", fmt.Sprintf("started `daisugi warden serve` but %s "+
				"never appeared after %.0fs. Run `daisugi warden serve --root %s` in a terminal to see why it exited.",
				sock, startServerWait.Seconds(), root)})
		}
	default:
		out = append(out, startStep{"warden-server", "would", "start the resident warden (daisugi warden serve)"})
	}
	return append(out, view), nil
}

// wardenCapturesArgs is the captures root a resident warden on root gets,
// whoever starts it: <root>/captures, sprig's own default.
func wardenCapturesArgs(root string) []string {
	return []string{"--captures-root", wardenroot.Join(root, "captures")}
}

// startWardenSpawn is how start starts the server; a test sees its argv.
var startWardenSpawn = (*Env).spawnWardenServer

// startServerWait is StartOptions.server_wait_s.
const startServerWait = 2 * time.Second

// exists is Path.exists(): the path, followed through a symlink, is there.
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// spawnWardenServer starts `<self> warden serve --root root [extra]` detached, with
// this process's environment and no stdin, stdout or stderr. A spawn that
// fails is seen as a socket that never appears.
func (e *Env) spawnWardenServer(root string, extra ...string) int {
	self, err := install.Self()
	if err != nil {
		return 0
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0
	}
	defer null.Close()
	cmd := exec.Command(self, append([]string{"warden", "serve", "--root", root}, extra...)...)
	cmd.Env = e.Environ
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() != nil {
		return 0
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid
}
