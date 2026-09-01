package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gate"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
)

// hookEvents are the --event values `hook record` takes.
var hookEvents = map[string]bool{
	"pre_tool_use": true, "stop": true, "notification": true, "subagent_start": true, "subagent_stop": true,
}

// spawnAutoTend is hook._spawn_detached_auto_tend: `daisugi hook
// auto-tend --data-dir <dir>` started in a session of its own, its
// streams on /dev/null, never waited on. Tests replace it.
var spawnAutoTend = func(env map[string]string, dataDir string) {
	bin, err := install.LookPath(env)("daisugi")
	if err != nil {
		return
	}
	cmd := exec.Command(bin, "hook", "auto-tend", "--data-dir", dataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// hookRecord is `daisugi hook record`: read a hook payload from stdin,
// record it, and print the host's continue contract. It never blocks the
// host: past the options, nothing it reads or cannot write changes the
// contract or the exit code.
func (e *Env) hookRecord(args []string) error {
	const cmd = "hook record"
	opts := []opt{
		{names: []string{"--captures-root"}, value: true, metavar: "PATH"},
		{names: []string{"--format"}, value: true, metavar: "TEXT",
			help: "Host runtime stdout contract: claude | codex | hermes | openclaw."},
		{names: []string{"--event"}, value: true, metavar: "TEXT",
			help: "Which host hook this is wired to: pre_tool_use (default, records the tool call) | stop " +
				"(session went idle) | notification (a permission prompt or an idle-prompt notification) | " +
				"subagent_start | subagent_stop, which report a subagent row under the pane in coppice."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Read a hook payload from stdin, record it, return the host's continue contract.", opts)
	}
	root := p.str("--captures-root", gateroot.Join(e.home, ".opendaisugi/captures"))
	format := p.str("--format", "claude")
	event := p.str("--event", "pre_tool_use")
	if !hookEvents[event] {
		e.errf("Error: --event must be one of pre_tool_use, stop, notification, subagent_start, subagent_stop.\n")
		return exit(1)
	}
	raw, err := io.ReadAll(e.in)
	if err != nil {
		raw = nil
	}
	e.out("%s\n", gate.HookRecord(raw, root, format, event, e.env))
	if event == "pre_tool_use" {
		e.maybeTriggerTend(gateroot.Parent(root), nowSeconds())
	}
	return nil
}

// maybeTriggerTend is hook.maybe_trigger_background_tend: at most once
// per 1800 s, and only with the user's consent, start a detached
// auto-tend. Any error ends it quietly with no spawn.
func (e *Env) maybeTriggerTend(dataDir string, now float64) {
	const minInterval = 1800
	defer func() { _ = recover() }()
	stamp := filepath.Join(dataDir, ".hook-record-tend-trigger")
	last, err := readStamp(stamp)
	if err != nil || now-last < minInterval {
		return
	}
	// A config this binary cannot read counts as no consent.
	cfg, err := config.Load(filepath.Join(dataDir, "config.yaml"))
	if err != nil || cfg.AutoTend == nil || !*cfg.AutoTend {
		return
	}
	if os.MkdirAll(dataDir, 0o777) != nil {
		return
	}
	if os.WriteFile(stamp, []byte(pyjson.FloatRepr(now)), 0o666) != nil {
		return
	}
	spawnAutoTend(e.env, dataDir)
}
