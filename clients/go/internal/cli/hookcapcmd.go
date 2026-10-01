package cli

import (
	"errors"
	"os"
	"strings"
	"time"

	"daisugi-verify/internal/capture"
	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
)

var capturesRootOpt = opt{names: []string{"--captures-root"}, value: true, metavar: "PATH"}

// capturesRoot is the --captures-root value as pathlib prints it.
func (e *Env) capturesRoot(p *parsed) string {
	if p.has("--captures-root") {
		return gateroot.PathStr(p.str("--captures-root", ""))
	}
	return gateroot.Join(e.dataHome(), "captures")
}

// pyRaise ends a command with an exception Python does not catch: the
// class name and its text, exit 1.
func (e *Env) pyRaise(cmd string, pe *capture.PyErr) error {
	e.errf("daisugi %s: %s: %s\n", cmd, pe.Type, pe.Msg)
	return exit(1)
}

// hookList is `daisugi hook list`: the captured sessions with their call
// counts, newest first.
func (e *Env) hookList(args []string) error {
	const cmd = "hook list"
	opts := []opt{capturesRootOpt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "List captured sessions with call counts.", opts)
	}
	sessions, err := capture.ListSessions(e.capturesRoot(p))
	var pe *capture.PyErr
	if errors.As(err, &pe) {
		return e.pyRaise(cmd, pe)
	}
	if errors.Is(err, capture.ErrUnreadable) {
		return e.refuse(cmd, err)
	}
	if err != nil {
		return e.failPy(cmd, err)
	}
	if p.flag("--json") {
		rows := make([]any, len(sessions))
		for i, s := range sessions {
			rows[i] = pyjson.NewObject().Set("session_id", s.ID).Set("calls", s.Calls).
				Set("first_at", s.FirstAt).Set("last_at", s.LastAt)
		}
		e.out("%s\n", pyjson.Dumps(rows, true))
		return nil
	}
	if len(sessions) == 0 {
		e.out("(no captured sessions)\n")
		return nil
	}
	e.out("%s  %6s  last_at\n", ljust("session_id", 40), "calls")
	for _, s := range sessions {
		e.out("%s  %6d  %s\n", ljust(s.ID, 40), s.Calls, pyStrOf(s.LastAt))
	}
	return nil
}

// ljust is format(s, "<n") on a str: padded with spaces to n code points.
func ljust(s string, n int) string {
	if k := pystr.Len(s); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}

// hookToTrace is `daisugi hook to-trace`: one captured session made a
// journal trace, verified against the envelope inferred from what it did.
func (e *Env) hookToTrace(args []string) error {
	const cmd = "hook to-trace"
	opts := []opt{capturesRootOpt,
		{names: []string{"--data-dir"}, value: true, metavar: "PATH"},
		{names: []string{"--task"}, value: true, metavar: "TEXT", help: "Override the task description; default uses the session id."},
		decomposeOpt}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " SESSION_ID", "Convert a captured session into a journal trace.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "SESSION_ID", "SESSION_ID")
	}
	id := p.args[0]
	name := id + ".jsonl"
	path := gateroot.PathStr(name)
	if !strings.HasPrefix(name, "/") {
		path = gateroot.Join(e.capturesRoot(p), gateroot.PathStr(name))
	}
	if _, err := os.Stat(path); err != nil {
		e.errf("error: no capture at %s\n", path)
		return exit(1)
	}
	dataDir := e.dataDir(p)
	decompose := p.flag("--allow-shell-decomposition")
	if !p.flagSet("--allow-shell-decomposition") {
		cfg, err := config.Load(gateroot.Join(dataDir, "config.yaml"))
		if err != nil {
			return e.configLoadErr(cmd, err)
		}
		decompose = cfg.ShellAllowDecomposition
	}
	task := p.str("--task", "")
	if task == "" {
		task = "captured session " + stemOf(path)
	}
	// Everything is read and checked before the journal is made, so a
	// capture this binary cannot convert changes nothing. Python makes the
	// journal before it reads the capture, so an exception it raises
	// leaves the journal made.
	conv, err := convertCapture(path, id, task, decompose)
	var pe *capture.PyErr
	var invalid *capture.InvalidPlan
	switch {
	case errors.As(err, &pe):
	case errors.As(err, &invalid):
		pe = &capture.PyErr{Type: "pydantic_core._pydantic_core.ValidationError", Msg: invalid.Text}
	case err != nil:
		if errors.Is(err, capture.ErrUnreadable) || errors.Is(err, tracejournal.ErrUnreadable) {
			return e.refuse(cmd, err)
		}
		return e.failPy(cmd, err)
	}
	traceHex, err := randHex8()
	if err != nil {
		return e.failPy(cmd, err)
	}
	j, err := tracejournal.Open(dataDir)
	if err != nil {
		return e.failPy(cmd, err)
	}
	defer j.Close()
	if pe != nil {
		return e.pyRaise(cmd, pe)
	}
	created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	traceID := created[:10] + "-" + traceHex
	if err := j.Log(conv.task, conv.env, conv.plan, conv.result, traceID, created); err != nil {
		if errors.Is(err, tracejournal.ErrUnreadable) {
			return e.refuse(cmd, err)
		}
		return e.failPy(cmd, err)
	}
	if err := j.MarkConverted(id, traceID, nowSeconds()); err != nil {
		return e.failPy(cmd, err)
	}
	e.out("%s\n", traceID)
	return nil
}

// stemOf is Path(path).stem.
func stemOf(path string) string {
	name := path[strings.LastIndexByte(path, '/')+1:]
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}
