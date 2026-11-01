package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/wardenroot"
)

var installOpts = []opt{
	{names: []string{"--dry-run"}, help: "Show what would change without writing anything."},
	{names: []string{"--yes", "-y"}, help: "Skip confirmation prompt."},
	{names: []string{"--print-skill"}, help: "Print the opendaisugi-checklist skill content to stdout."},
	{names: []string{"--list"}, help: "List the components install can write, and stop."},
	{names: []string{"--uninstall"}, help: "Reverse all managed changes."},
	{names: []string{"--runtime"}, value: true, multiple: true, metavar: "TEXT",
		help: "Target named runtime(s) only, e.g. --runtime claude."},
	{names: []string{"--harness"}, value: true, multiple: true, metavar: "TEXT",
		help: "Install a loop harness's warden extension. Supported: pi, opencode."},
	{names: []string{"--only"}, value: true, multiple: true, metavar: "TEXT",
		help: "Install only these components, comma-separated or repeated: skill, mcp, capture, instructions, warden, pando, router. Default: all but router. See --list."},
	{names: []string{"--warden"}, hidden: true},
	{names: []string{"--enforce"}, neg: "--audit", help: "Warden mode: --enforce denies out-of-envelope calls; --audit records them and allows."},
	{names: []string{"--shadow"}, hidden: true},
	{names: []string{"--ask"}, neg: "--no-ask", help: "Hand an enforce-mode would-deny to a present operator for a bounded time."},
	{names: []string{"--router"}, neg: "--no-router", help: "Also point the harness at the local token-saving router (opt-in)."},
	{names: []string{"--allow-shell-decomposition"}, neg: "--no-allow-shell-decomposition",
		help: "Let the envelope admit compound shell (ADR-0010)."},
	{names: []string{"--base-url"}, value: true, metavar: "TEXT", help: "Router base_url to wire in when --router is set."},
	{names: []string{"--report"}, value: true, metavar: "TEXT", help: "Report warden state to a floor host: herdr or coppice."},
	{names: []string{"--routing-mode"}, value: true, metavar: "TEXT", help: "Who picks the model for each router turn: rules, switchyard, knn, or off."},
	{names: []string{"--efficient-model"}, value: true, metavar: "TEXT", help: "The model id of Switchyard's efficient tier."},
	{names: []string{"--capable-model"}, value: true, metavar: "TEXT", help: "The model id of Switchyard's capable tier."},
	{names: []string{"--api-key-env"}, value: true, metavar: "TEXT", help: "An environment variable holding an Anthropic API key for Switchyard's cloud tiers."},
}

// EnforceNeedsPolicy is what install --enforce says when no
// envelope is registered: Python's cli.ENFORCE_NEEDS_POLICY.
const EnforceNeedsPolicy = "Enforce needs a policy first. Run: daisugi warden init --workspace DIR " +
	"for a starter envelope, then this command again. " +
	"Or run: daisugi install (audit mode records every call and blocks none)"

func (e *Env) install(args []string) error {
	if handed, err := e.portHop(args); handed || err != nil {
		return err
	}
	p, err := parseArgs(args, installOpts, 0)
	if err != nil {
		return e.usage("install", err)
	}
	if p.help {
		return e.cmdHelp("install", "", "Wire openDaisugi into every detected agent harness: the skill, the MCP "+
			"server, the capture hook, the pathway instructions and the warden hook in audit mode; --enforce makes the "+
			"warden deny, --router points the harness at the token-saving router, --harness writes the pi or "+
			"OpenCode extension.", installOpts)
	}
	if p.flag("--shadow") {
		// cli._shadow_moved: the old name of --audit, one line.
		return e.usage("install", &usageError{"Invalid value for '--shadow': shadow mode is now audit mode: use --audit"})
	}
	if p.flag("--warden") {
		// cli._warden_flag_moved: the warden hook is part of every install, one line.
		return e.usage("install", &usageError{"Invalid value for '--warden': the warden hook is part of every install: drop --warden (add --enforce to make it deny)"})
	}
	if p.flag("--print-skill") {
		e.out("%s\n", install.SkillText())
		return nil
	}
	if p.flag("--list") {
		e.out("%s\n", install.ListComponents())
		return nil
	}
	var only map[string]bool
	if names := p.vals["--only"]; len(names) > 0 {
		var err error
		if only, err = install.ComponentLayers(names); err != nil {
			e.errf("%v\n", err)
			return exit(2)
		}
		if !p.flag("--uninstall") && len(p.vals["--harness"]) == 0 {
			if p.flag("--enforce") && !only[install.LayerWarden] {
				e.errf("--enforce needs the warden component: add warden to --only\n")
				return exit(2)
			}
			if p.flag("--router") && !only[install.LayerBaseURL] {
				e.errf("--router is a component: add router to --only, or drop --router\n")
				return exit(2)
			}
		}
	}
	if !p.flag("--uninstall") {
		// The names of the warden and the router changed: move the owner's
		// own state once, before anything new is written.
		if _, err := install.MoveOldState(e.dataHome(), e.home, e.env, time.Now(), e.Stdout, p.flag("--dry-run")); err != nil {
			var refused *install.MoveRefused
			if errors.As(err, &refused) {
				return exit(2)
			}
			return e.fail("install", err)
		}
	}
	harness := p.vals["--harness"]
	if len(harness) > 0 {
		if only != nil {
			e.out("Note: --only does not apply to --harness.\n")
		}
		return e.installHarness(harness, p)
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
		return e.uninstallAll(runtimes, only)
	}
	gw := p.flag("--router") || only[install.LayerBaseURL]
	// base_url or DEFAULT_ROUTER_BASE_URL: an empty one is the default.
	baseURL := p.str("--base-url", "")
	if baseURL == "" {
		baseURL = install.DefaultBaseURL
	}
	enforce, ask := p.flag("--enforce"), p.flag("--ask")
	root := wardenroot.Join(e.dataHome(), "warden")
	// With no envelope registered, an enforce hook denies every call, so
	// the agent can do nothing at all. Refuse before anything is written.
	if enforce {
		envs, err := wardenroot.Envelopes(root)
		if err != nil {
			return e.refuse("install", err)
		}
		if len(envs) == 0 {
			e.errf("%s\n", EnforceNeedsPolicy)
			return exit(1)
		}
	}
	if ask && !enforce {
		e.out("Note: --ask only applies with --enforce; no operator ask will be wired.\n")
	}
	effectiveAsk := ask && enforce
	report := p.str("--report", "")
	if p.has("--report") && report != "herdr" && report != "coppice" {
		e.errf("Error: --report must be herdr or coppice. Run: daisugi install --report herdr\n")
		return exit(1)
	}
	// The routing mode is checked before anything is written, so a bad
	// value leaves the harness and config.yaml as they were.
	router, err := e.planRoutingUpdate(p, gw)
	if err != nil {
		return err
	}
	self, err := install.SelfEntry()
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
	o := install.Opts{Router: gw, Enforce: enforce, Ask: effectiveAsk, URL: baseURL, Report: report, Only: only}
	// Decide every change before printing the plan: a file this binary
	// cannot edit refuses the run with nothing written.
	plan := func() ([]*install.Change, error) {
		var changes []*install.Change
		for _, rt := range runtimes {
			ch, err := install.PlanInstall(rt, e.home, self, root, o)
			if err != nil {
				return nil, err
			}
			changes = append(changes, ch)
		}
		install.DedupeUnitEdits(changes)
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
	wardenWired := false
	unitShown := false
	for _, rt := range runtimes {
		e.out("[%s]\n", rt.Name)
		for _, s := range install.PlanAllSteps(rt, e.home, o) {
			if s.Layer == install.LayerPando && strings.HasSuffix(s.Target, install.PandoUnitName) {
				if unitShown {
					continue // Claude Code and Hermes share the one unit
				}
				unitShown = true
			}
			target, marker := "", "+"
			if s.Target != "" {
				target = "  → " + s.Target
			}
			if s.Layer == "warden" && s.Supported {
				wardenWired = true
			}
			if !s.Supported {
				marker = "!"
				gaps = append(gaps, "  ["+rt.Name+"] "+s.Layer+": "+s.Description)
			}
			e.out("  %s [%s] %s%s\n", marker, s.Layer, s.Description, target)
		}
		e.out("\n")
	}
	if len(gaps) > 0 {
		e.out("Honest gaps (selected but not wired for this harness):\n%s\n\n", strings.Join(gaps, "\n"))
	}
	e.out("Skill is discovered on demand — zero added tokens for simple sessions.\n")
	if o.Has(install.LayerCapture) {
		e.out("Tool calls are captured to %s/ for distillation.\n\n", e.tilde(wardenroot.Join(e.dataHome(), "captures")))
	} else {
		e.out("\n")
	}
	if wardenWired {
		e.out("The warden checks each call against a registered envelope — this install writes the hook, " +
			"not the policy. Run `daisugi warden init` to register one (add --allow-shell-decomposition " +
			"to admit `a && b` and pipes).\n\n")
	}
	if router != nil {
		e.out("Router: set routing_mode to %s.\n", router["routing_mode"])
	}
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
	var written, failed, notes []string
	install.UnitHook = e.unitHook()
	defer func() { install.UnitHook = nil }()
	for _, ch := range changes {
		for _, ed := range ch.Edits {
			if ed.Warning != "" {
				e.errf("warning: %s\n", ed.Warning)
			}
			if ed.PreWarning != "" {
				e.errf("warning: %s\n", ed.PreWarning)
			}
		}
		followed, err := install.ApplyChange(ch)
		e.noteFollowed(followed)
		if err != nil {
			return e.fail("install", err)
		}
		// A runtime whose apply raised lists nothing, even the files it
		// wrote before it raised (Partial), as Python's does.
		if ch.Failed {
			failed = append(failed, ch.Runtime.Name+": "+ch.Why)
		} else {
			written = append(written, ch.Written()...)
		}
		for _, ed := range ch.Edits {
			if ed != nil && ed.Note != "" {
				notes = append(notes, ed.Note)
			}
		}
	}
	if len(written) > 0 {
		e.out("\nDone. Files modified:\n")
		for _, f := range written {
			e.out("  %s\n", f)
		}
		for _, n := range notes {
			e.out("%s\n", n)
		}
		e.out("\n%s\n", install.RestartLine)
	} else if len(failed) == 0 {
		// A hook install left as it is was not configured by this run.
		left := false
		for _, n := range notes {
			left = left || install.LeftAsIs(n)
		}
		if left {
			e.out("\n")
		} else {
			e.out("\nAll runtimes were already configured — nothing changed.\n")
		}
		for _, n := range notes {
			e.out("%s\n", n)
		}
	}
	// A runtime that failed wrote nothing: name it and exit 1, never
	// report it as configured.
	for _, f := range failed {
		e.errf("Failed: %s. Nothing was written for it.\n", f)
	}
	cfgPath := wardenroot.Join(e.dataHome(), "config.yaml")
	if p.flagSet("--allow-shell-decomposition") {
		allow := p.flag("--allow-shell-decomposition")
		if err := e.saveInstallConfig(cfgPath, map[string]any{"shell_allow_decomposition": allow}); err != nil {
			return err
		}
		state := "off"
		if allow {
			state = "on"
		}
		e.out("Compound-shell decomposition default: %s (%s) — config is your data, so it survives "+
			"--uninstall; edit or delete the file to reset it.\n", state, cfgPath)
	}
	if router != nil {
		if err := e.applyRoutingUpdate(router); err != nil {
			return err
		}
	}
	if report != "" {
		if err := e.saveInstallConfig(cfgPath, map[string]any{"floor_report": report}); err != nil {
			return err
		}
		if report == "coppice" {
			e.out("Floor report set to coppice. Saved in %s. The coppice server is built. Nothing reports to it yet.\n", cfgPath)
		}
	}
	// Ask once whether to distil repeated tasks in the background.
	// Interactive only: a --yes install leaves consent unasked.
	if !p.flag("--yes") {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return e.refuse("install", fmt.Errorf("%s is not one this binary rewrites: %w", cfgPath, err))
		}
		if cfg.AutoTend == nil {
			yes, err := e.confirmDefault("\nLet openDaisugi distil your repeated tasks in the background, so reuse "+
				"compounds automatically? (only affects cost — the guard enforces safety either way)", true)
			if err != nil {
				return err
			}
			if err := e.saveInstallConfig(cfgPath, map[string]any{"auto_tend": yes}); err != nil {
				return err
			}
			if yes {
				e.out("Background distillation is ON — no cron needed: your capture hook kicks off a " +
					"rate-limited tend on its own. (You can also run `daisugi hook auto-tend` any time.)\n")
			} else {
				e.out("Left OFF — run `daisugi tend` yourself whenever you want it.\n")
			}
		}
	}
	var okRuntimes []install.Runtime
	for _, ch := range changes {
		if !ch.Failed {
			okRuntimes = append(okRuntimes, ch.Runtime)
		}
	}
	broken := install.UnrunnableCommands(okRuntimes, e.home)
	for _, line := range broken {
		e.errf("%s\n", line)
	}
	if line := install.TemporaryExeNote(self); line != "" {
		e.errf("%s\n", line)
	}
	if len(failed) > 0 || len(broken) > 0 {
		return exit(1)
	}
	return nil
}

// saveInstallConfig is save_config(load_config(path).model_copy(update)).
func (e *Env) saveInstallConfig(path string, update map[string]any) error {
	if err := config.Save(path, e.home, update); err != nil {
		if errors.Is(err, config.ErrInvalid) {
			e.errf("daisugi install: pydantic_core._pydantic_core.ValidationError: %s does not validate\n", path)
			return exit(1)
		}
		return e.refuse("install", fmt.Errorf("%s is not one this binary rewrites: %w", path, err))
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

// confirmDefault is typer.confirm(text, default=def) on a piped stdin: an
// empty line is the default.
func (e *Env) confirmDefault(text string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		e.out("%s %s: ", text, hint)
		line, err := e.in.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			e.errf("Aborted.\n")
			return false, exit(1)
		}
		switch strings.ToLower(strings.TrimSpace(strings.TrimRight(line, "\r\n"))) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		case "":
			return def, nil
		}
		e.out("Error: invalid input\n")
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

// uninstallAll is install.uninstall: every managed change reversed for
// each runtime.
func (e *Env) uninstallAll(runtimes []install.Runtime, only map[string]bool) error {
	var changes []*install.Change
	gone := map[string]bool{}
	for _, rt := range runtimes {
		ch, err := install.PlanReverseAll(rt, e.home, gone, only)
		if err != nil {
			return e.refuse("install --uninstall", err)
		}
		changes = append(changes, ch)
	}
	install.DedupeUnitEdits(changes)
	var written, failed []string
	install.UnitHook = e.unitHook()
	defer func() { install.UnitHook = nil }()
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
	what := ""
	if only != nil {
		what = strings.Join(install.ComponentNames(only), ", ") + " "
	}
	e.out("Uninstalled %sfrom: %s\n", what, strings.Join(names, ", "))
	if len(failed) > 0 {
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
				target = wardenroot.Parent(target)
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
		e.out("[%s] warden extension installed. %s\n", h, restart[h])
	}
	for _, h := range names {
		e.out("%s asks the warden in-process. The warden only watches until you set "+
			"warden_mode: enforce. Start it with `daisugi start`.\n", label[h])
	}
	return nil
}

// portHop hands the whole install to the port DAISUGI_PORT names, when
// that is not this one (ruling PK-R-15): it runs that binary in place of
// this process, with the same arguments. true when it did.
func (e *Env) portHop(args []string) (bool, error) {
	self := e.selfPath
	if self == "" {
		s, err := install.Self()
		if err != nil {
			return false, err
		}
		self = s
	}
	target, err := install.PortHop("go", self, e.env)
	if err != nil {
		e.errf("daisugi: %v\n", err)
		return false, exit(2)
	}
	if target == "" {
		return false, nil
	}
	argv := []string{target}
	if e.quiet {
		argv = append(argv, "-q")
	}
	if e.plain {
		argv = append(argv, "--plain")
	}
	argv = append(append(argv, "install"), args...)
	env := append(append([]string{}, e.Environ...), install.PortHopEnv+"=1")
	run := e.execFn
	if run == nil {
		run = syscall.Exec
	}
	if err := run(target, argv, env); err != nil {
		e.errf("daisugi: cannot run %s: %v\n", target, err)
		return false, exit(1)
	}
	return true, nil
}
