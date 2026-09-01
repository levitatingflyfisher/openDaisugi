package cli

import (
	"errors"
	"io"
	"strings"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
)

var installOpts = []opt{
	{names: []string{"--dry-run"}, help: "Show what would change without writing anything."},
	{names: []string{"--yes", "-y"}, help: "Skip confirmation prompt."},
	{names: []string{"--print-skill"}, help: "Not in this binary yet."},
	{names: []string{"--uninstall"}, help: "With --gate, remove the gate hooks; with --harness, the extensions."},
	{names: []string{"--runtime"}, value: true, multiple: true, metavar: "TEXT",
		help: "Target named runtime(s) only, e.g. --runtime claude."},
	{names: []string{"--harness"}, value: true, multiple: true, metavar: "TEXT",
		help: "Install a loop harness's gate extension. Supported: pi, opencode."},
	{names: []string{"--gate"}, neg: "--no-gate", help: "Install the fail-closed verify hook (shadow by default)."},
	{names: []string{"--enforce"}, neg: "--shadow", help: "Gate mode: --enforce denies out-of-envelope calls; --shadow only observes."},
	{names: []string{"--ask"}, neg: "--no-ask", help: "Not in this binary yet."},
	{names: []string{"--gateway"}, neg: "--no-gateway", help: "Not in this binary yet."},
	{names: []string{"--allow-shell-decomposition"}, neg: "--no-allow-shell-decomposition", help: "Not in this binary yet."},
	{names: []string{"--base-url"}, value: true, metavar: "TEXT", help: "Not in this binary yet."},
	{names: []string{"--report"}, value: true, metavar: "TEXT", help: "Not in this binary yet."},
	{names: []string{"--router"}, value: true, metavar: "TEXT", help: "Not in this binary yet."},
	{names: []string{"--efficient-model"}, value: true, metavar: "TEXT", help: "Not in this binary yet."},
	{names: []string{"--capable-model"}, value: true, metavar: "TEXT", help: "Not in this binary yet."},
	{names: []string{"--api-key-env"}, value: true, metavar: "TEXT", help: "Not in this binary yet."},
}

// EnforceNeedsPolicy is what install --gate --enforce says when no
// envelope is registered: Python's cli.ENFORCE_NEEDS_POLICY.
const EnforceNeedsPolicy = "Enforce needs a policy first. Run: daisugi gate init --workspace DIR " +
	"for a starter envelope, then this command again. " +
	"Or install in shadow mode: daisugi install --gate"

// The install flags this binary does not carry. A run that names one is
// refused before anything is read or written.
// --ask is one of them for now; gate check --ask itself is native.
var notYetFlags = []string{"--print-skill", "--gateway", "--allow-shell-decomposition", "--base-url",
	"--report", "--router", "--efficient-model", "--capable-model", "--api-key-env", "--ask"}

func (e *Env) install(args []string) error {
	p, err := parseArgs(args, installOpts, 0)
	if err != nil {
		return e.usage("install", err)
	}
	if p.help {
		return e.cmdHelp("install", "", "Wire the gate into agent harnesses: --gate writes the gate hook "+
			"(Claude Code, Codex); --harness writes the pi or OpenCode extension. The skill, MCP, "+
			"capture and instruction layers are not in this binary yet.", installOpts)
	}
	for _, f := range notYetFlags {
		// --no-gateway changes nothing; --no-allow-shell-decomposition
		// writes config.yaml in Python, so it is refused too.
		if p.has(f) || p.flag(f) || (f == "--allow-shell-decomposition" && p.flagSet(f)) {
			return e.notYet("daisugi install " + f)
		}
	}
	harness := p.vals["--harness"]
	if len(harness) > 0 {
		return e.installHarness(harness, p)
	}
	if !p.flag("--gate") {
		if p.flag("--uninstall") {
			return e.notYet("daisugi install --uninstall without --gate")
		}
		return e.notYet("daisugi install without --gate or --harness")
	}
	lookPath := install.LookPath(e.env)
	var runtimes []install.Runtime
	if names := p.vals["--runtime"]; len(names) > 0 {
		runtimes, err = install.Select(names)
		if err != nil {
			e.errf("Error: %v\n", err)
			return exit(2)
		}
	} else {
		runtimes = install.Detect(e.home, lookPath)
	}
	if len(runtimes) == 0 {
		e.out("No supported agent runtimes detected (Claude Code, Codex, Hermes, OpenClaw).\n")
		e.out("Install one and re-run, or see docs/hook-integration.md for manual setup.\n")
		return nil
	}
	if p.flag("--uninstall") {
		return e.uninstallGate(runtimes)
	}
	enforce, ask := p.flag("--enforce"), false
	root := gateroot.Join(e.home, ".opendaisugi/gate")
	// With no envelope registered, an enforce hook denies every call, so
	// the agent can do nothing at all. Refuse before anything is written.
	if enforce {
		envs, err := gateroot.Envelopes(root)
		if err != nil {
			return e.refuse("install", err)
		}
		if len(envs) == 0 {
			e.errf("%s\n", EnforceNeedsPolicy)
			return exit(1)
		}
	}
	self, err := install.Self()
	if err != nil {
		return e.fail("install", err)
	}
	// A mise install runs from a versioned path; the hook names mise's
	// shim, or an Omarchy stub, which survive an upgrade, when one runs
	// this binary.
	self, versioned := install.HookPath(self, e.env, e.miseWhich)
	if versioned {
		e.errf("%s\n", install.VersionedNote(self))
	}
	// Decide every change before printing the plan: a settings file this
	// binary cannot edit refuses the run with nothing written.
	plan := func() ([]*install.Change, error) {
		var changes []*install.Change
		for _, rt := range runtimes {
			ch, err := install.PlanApply(rt, e.home, self, root, enforce, ask)
			if err != nil {
				return nil, err
			}
			changes = append(changes, ch)
		}
		return changes, nil
	}
	planned, err := plan()
	if err != nil {
		return e.refuse("install", err)
	}
	e.out("\nDetected runtimes:\n")
	for _, ch := range planned {
		if ch.Failed {
			e.out("  ! %s: %s\n", ch.Runtime.Name, ch.Why)
		} else {
			e.out("  ✓ %s\n", ch.Runtime.Name)
		}
	}
	e.out("\nopenDaisugi will make these changes:\n\n")
	var gaps []string
	for _, rt := range runtimes {
		e.out("[%s]\n", rt.Name)
		for _, s := range install.Plan(rt, e.home, enforce, ask) {
			target, marker := "", "+"
			if s.Target != "" {
				target = "  → " + s.Target
			}
			if !s.Supported {
				marker = "!"
				gaps = append(gaps, "  ["+rt.Name+"] gate: "+s.Description)
			}
			e.out("  %s [gate] %s%s\n", marker, s.Description, target)
		}
		e.out("\n")
	}
	if len(gaps) > 0 {
		e.out("Honest gaps (selected but not wired for this harness):\n%s\n\n", strings.Join(gaps, "\n"))
	}
	e.out("This binary installs the gate layer only. The skill, MCP, capture and instruction\n" +
		"layers come from the Python CLI's `daisugi install`.\n\n")
	e.out("The gate checks each call against a registered envelope — this install writes the hook, " +
		"not the policy. Run `daisugi gate init` to register one (add --allow-shell-decomposition " +
		"to admit `a && b` and pipes).\n\n")
	if p.flag("--dry-run") {
		e.out("Dry run — no files written.\n")
		return nil
	}
	if !p.flag("--yes") {
		ok, err := e.confirm("Proceed?")
		if err != nil {
			return err
		}
		if !ok {
			e.out("Aborted.\n")
			return nil
		}
	}
	// Plan again: the files may have changed while the prompt waited.
	changes, err := plan()
	if err != nil {
		return e.refuse("install", err)
	}
	var written, failed []string
	for _, ch := range changes {
		for _, ed := range ch.Edits {
			if ed.Warning != "" {
				e.errf("warning: %s\n", ed.Warning)
			}
		}
		followed, err := install.ApplyChange(ch)
		e.noteFollowed(followed)
		if err != nil {
			return e.fail("install", err)
		}
		if ch.Failed {
			failed = append(failed, ch.Runtime.Name+": "+ch.Why)
		} else {
			written = append(written, ch.Written()...)
		}
	}
	if len(written) > 0 {
		e.out("\nDone. Files modified:\n")
		for _, f := range written {
			e.out("  %s\n", f)
		}
		e.out("\nRestart your agent session to pick up the changes.\n")
	} else if len(failed) == 0 {
		e.out("\nAll runtimes were already configured — nothing changed.\n")
	}
	// A runtime that failed wrote nothing: name it and exit 1, never
	// report it as configured.
	for _, f := range failed {
		e.errf("Failed: %s. Nothing was written for it.\n", f)
	}
	if len(failed) > 0 {
		return exit(1)
	}
	return nil
}

// noteFollowed says which writes went through a symlink to another file.
func (e *Env) noteFollowed(followed []string) {
	for _, f := range followed {
		link, target, _ := strings.Cut(f, " -> ")
		e.noteFile(link, target)
	}
}

// confirm is typer.confirm(text, default=False) on a piped stdin: y or
// yes is true, n, no or an empty line false, anything else asks again
// after "Error: invalid input", and end of input aborts with exit 1.
func (e *Env) confirm(text string) (bool, error) {
	for {
		e.out("%s [y/N]: ", text)
		line, err := e.in.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			e.errf("Aborted.\n")
			return false, exit(1)
		}
		switch strings.ToLower(strings.TrimSpace(strings.TrimRight(line, "\r\n"))) {
		case "y", "yes":
			return true, nil
		case "n", "no", "":
			return false, nil
		}
		e.out("Error: invalid input\n")
	}
}

func (e *Env) uninstallGate(runtimes []install.Runtime) error {
	var changes []*install.Change
	for _, rt := range runtimes {
		ch, err := install.PlanReverse(rt, e.home)
		if err != nil {
			return e.refuse("install --uninstall", err)
		}
		changes = append(changes, ch)
	}
	var written, failed []string
	for _, ch := range changes {
		if ch.Failed {
			failed = append(failed, ch.Runtime.Name+": "+ch.Why)
			continue
		}
		followed, err := install.ApplyChange(ch)
		e.noteFollowed(followed)
		if err != nil {
			return e.fail("install --uninstall", err)
		}
		written = append(written, ch.Written()...)
	}
	names := make([]string, len(runtimes))
	for i, rt := range runtimes {
		names[i] = rt.Name
	}
	e.out("Uninstalled from: %s\n", strings.Join(names, ", "))
	if len(failed) > 0 {
		// A runtime left untouched, and why (a gate hook in a form not
		// read): the run exits 1.
		e.out("Failures (left untouched): %s\n", strings.Join(failed, "; "))
	}
	if len(written) > 0 {
		e.out("\nReverted:\n")
		for _, f := range written {
			e.out("  %s\n", f)
		}
	}
	if len(failed) > 0 {
		return exit(1)
	}
	return nil
}

func (e *Env) installHarness(names []string, p *parsed) error {
	for _, h := range names {
		if h != "pi" && h != "opencode" {
			e.errf("error: unknown harness %s. Supported: pi, opencode.\n", pyjson.Repr(h))
			return exit(2)
		}
	}
	restart := map[string]string{
		"pi":       "Restart pi, or run /reload in an interactive session, for it to take effect.",
		"opencode": "Restart OpenCode for it to take effect.",
	}
	label := map[string]string{"pi": "pi", "opencode": "OpenCode"}
	if p.flag("--uninstall") {
		for _, h := range names {
			target := install.Target(h, e.home, e.env)
			if h == "pi" {
				target = gateroot.Parent(target)
			}
			if p.flag("--dry-run") {
				e.out("[%s] would remove %s\n", h, target)
				continue
			}
			removed, err := install.UninstallHarness(h, e.home, e.env)
			if err != nil {
				return e.fail("install", err)
			}
			if len(removed) > 0 {
				e.out("[%s] removed %d file(s).\n", h, len(removed))
			} else {
				e.out("[%s] nothing was installed.\n", h)
			}
		}
		return nil
	}
	if p.flag("--dry-run") {
		for _, h := range names {
			e.out("[%s] would write %s\n", h, install.Target(h, e.home, e.env))
		}
		return nil
	}
	for _, h := range names {
		if _, err := install.InstallHarness(h, e.home, e.env); err != nil {
			var he *install.HarnessError
			if errors.As(err, &he) {
				e.errf("[%s] error: %s\n", h, he.Msg)
				return exit(1)
			}
			return e.fail("install", err)
		}
		e.out("[%s] gate extension installed. %s\n", h, restart[h])
	}
	for _, h := range names {
		e.out("%s asks the resident gate (`daisugi gate serve`), which this binary does not serve yet: "+
			"run it from the Python CLI. The gate only watches until you set gate_mode: enforce.\n", label[h])
	}
	return nil
}
