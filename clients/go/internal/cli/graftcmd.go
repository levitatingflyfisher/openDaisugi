package cli

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/delegate"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

var graftHelp = `Usage: daisugi graft [OPTIONS] COMMAND [ARGS]...

  Install, show and remove the gate's graft rule for large reads.

Commands:
  install  Write the large-read graft rule into <data-dir>/gate/grafts.
  status   Show the graft rule files, the one in force, and any rival...
  remove   Remove a graft rule file.
`

var graftRuleID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// graft is `daisugi graft`: graft_install through its three commands.
func (e *Env) graft(args []string) error {
	if len(args) == 0 {
		e.out("%s", graftHelp)
		return exit(2)
	}
	if args[0] == "--help" {
		e.out("%s", graftHelp)
		return nil
	}
	switch args[0] {
	case "install":
		return e.graftInstall(args[1:])
	case "status":
		return e.graftStatus(args[1:])
	case "remove":
		return e.graftRemove(args[1:])
	}
	e.errf("Usage: daisugi graft [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi graft --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

// graftSettingsFiles is graft_install.settings_files.
func (e *Env) graftSettingsFiles() []string {
	cwd := e.cwd()
	var out []string
	for _, p := range []string{
		gateroot.Join(gateroot.Join(e.home, ".claude"), "settings.json"),
		gateroot.Join(gateroot.Join(cwd, ".claude"), "settings.json"),
		gateroot.Join(gateroot.Join(cwd, ".claude"), "settings.local.json"),
		gateroot.Join(gateroot.Join(e.home, ".codex"), "hooks.json"),
	} {
		seen := false
		for _, q := range out {
			seen = seen || q == p
		}
		if !seen {
			out = append(out, p)
		}
	}
	return out
}

type rivalHook struct{ file, what string }

var errGraftJSON = errors.New("a settings file holds JSON this binary does not read")

// graftRivals is graft_install.rival_hooks.
func (e *Env) graftRivals() ([]rivalHook, error) {
	var out []rivalHook
	for _, path := range e.graftSettingsFiles() {
		enc, ferr := pystr.FSEncode(path)
		if ferr != nil {
			out = append(out, rivalHook{path, "not readable JSON"})
			continue
		}
		raw, err := os.ReadFile(string(enc))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			out = append(out, rivalHook{path, "not readable JSON"})
			continue
		}
		text, derr := pystr.DecodeStrict(raw)
		if derr != nil {
			out = append(out, rivalHook{path, "not readable JSON"})
			continue
		}
		data, err := pyjson.Loads(text)
		if err != nil {
			if errors.Is(err, pyjson.ErrUnsupported) {
				return nil, errGraftJSON
			}
			out = append(out, rivalHook{path, "not readable JSON"})
			continue
		}
		obj, _ := data.(*pyjson.Object)
		if obj == nil {
			continue
		}
		hooks, _ := obj.Value("hooks").(*pyjson.Object)
		if hooks == nil {
			continue
		}
		entries, _ := hooks.Value("PreToolUse").([]any)
		for _, ent := range entries {
			eo, _ := ent.(*pyjson.Object)
			if eo == nil {
				continue
			}
			inner, _ := eo.Value("hooks").([]any)
			for _, h := range inner {
				ho, _ := h.(*pyjson.Object)
				if ho != nil {
					c := ho.Value("command")
					if config.GateHookKind(c) == config.KindGate || config.IsRecordHook(c, "") {
						continue
					}
				}
				var what string
				if ho != nil {
					if c, ok := ho.Value("command").(string); ok {
						what = pyjson.Dumps(c, true)
					}
				}
				if what == "" {
					what = pyjson.Dumps(h, true)
				}
				out = append(out, rivalHook{path, what})
			}
		}
	}
	return out, nil
}

func graftRefusal(r rivalHook) string {
	if r.what == "not readable JSON" {
		return fmt.Sprintf("Refused: %s is not readable JSON, so its PreToolUse hooks cannot be "+
			"checked. No graft was installed; the gate is not changed.", r.file)
	}
	return fmt.Sprintf("Refused: %s has a PreToolUse hook that may rewrite a tool's input: %s. "+
		"No graft was installed; the gate is not changed.", r.file, r.what)
}

func (e *Env) graftDataDir(p *parsed) string {
	return gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
}

func (e *Env) graftIDOK(p *parsed) (string, error) {
	id := p.str("--id", "big-read")
	if !graftRuleID.MatchString(id) {
		e.echoErr("Error: --id must be 1 to 64 of A-Z a-z 0-9 . _ -\n")
		return "", exit(2)
	}
	return id, nil
}

// graftPriorVersion is graft_install._prior_version.
func graftPriorVersion(path, id string) *big.Int {
	enc, ferr := pystr.FSEncode(path)
	if ferr != nil {
		return new(big.Int)
	}
	raw, err := os.ReadFile(string(enc))
	if err != nil {
		return new(big.Int)
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return new(big.Int)
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return new(big.Int)
	}
	r, _ := delegate.ParseRule(v, filepath.Base(path))
	if r == nil || r.ID != id {
		return new(big.Int)
	}
	n, ok := new(big.Int).SetString(r.Version.Text, 10)
	if !ok {
		return new(big.Int)
	}
	return n
}

func (e *Env) graftInstall(args []string) error {
	const cmd = "graft install"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--id"}, value: true, metavar: "TEXT", help: "The rule's id and file name."},
		{names: []string{"--state"}, value: true, metavar: "TEXT", help: "audit (only records) or active."},
		{names: []string{"--file-lines-over"}, value: true, metavar: "INTEGER", help: "Redirect reads of more lines."},
		{names: []string{"--allow-remote"}, help: "Let the router pick a remote worker the envelope grants."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Write the large-read graft rule into <data-dir>/gate/grafts.", opts)
	}
	lines, err := clickInt(p, "--file-lines-over", 350)
	if err != nil {
		return e.usage(cmd, err)
	}
	id, err := e.graftIDOK(p)
	if err != nil {
		return err
	}
	state := p.str("--state", "audit")
	if state != "audit" && state != "active" {
		e.echoErr("Error: --state must be audit or active.\n")
		return exit(2)
	}
	if lines < 1 {
		e.echoErr("Error: --file-lines-over must be 1 or more.\n")
		return exit(2)
	}
	rivals, err := e.graftRivals()
	if err != nil {
		return e.refuse(cmd, err)
	}
	if len(rivals) > 0 {
		e.echoErr("%s\n", graftRefusal(rivals[0]))
		return exit(1)
	}
	root := gateroot.Join(e.graftDataDir(p), "gate")
	dir := gateroot.Join(root, "grafts")
	path := gateroot.Join(dir, id+".json")
	version := graftPriorVersion(path, id)
	version.Add(version, big.NewInt(1))
	rule := pyjson.NewObject().Set("id", id).Set("version", pyjson.Int{Text: version.String()}).
		Set("shape", "deny_redirect").Set("state", state).
		Set("match", pyjson.NewObject().Set("tool", "Read").Set("file_lines_over", pyjson.Int{Text: fmt.Sprint(lines)})).
		Set("redirect", pyjson.NewObject().Set("tool", "delegate")).
		Set("worker", pyjson.NewObject().Set("choose", "router").Set("allow_remote", p.flag("--allow-remote")))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return e.fail(cmd, err)
	}
	tmp := gateroot.Join(dir, "."+id+".json.tmp")
	if err := os.WriteFile(tmp, []byte(pyjson.DumpsIndent(rule, 2, true)+"\n"), 0o666); err != nil {
		return e.fail(cmd, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return e.fail(cmd, err)
	}
	e.echo("Installed graft rule %s (version %s, state %s, reads over %d lines): %s\n", id, version.String(), state, lines, path)
	return nil
}

func (e *Env) graftStatus(args []string) error {
	const cmd = "graft status"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show the graft rule files, the one in force, and any rival PreToolUse hook.", opts)
	}
	root := gateroot.Join(e.graftDataDir(p), "gate")
	rules, bad, err := delegate.LoadRules(root)
	if err != nil {
		return e.refuse(cmd, err)
	}
	rivals, err := e.graftRivals()
	if err != nil {
		return e.refuse(cmd, err)
	}
	var acting *delegate.Rule
	for _, r := range rules {
		if r.Acting() {
			acting = r
			break
		}
	}
	outRules := []any{}
	for _, r := range rules {
		var why any
		switch {
		case r == acting:
		case !r.Acting():
			why = "state " + r.State + " does not act"
		default:
			why = acting.File + " acts first"
		}
		outRules = append(outRules, pyjson.NewObject().Set("file", r.File).Set("id", r.ID).
			Set("version", r.Version).Set("state", r.State).Set("file_lines_over", r.MinLines).
			Set("allow_remote", r.AllowRemote).Set("in_force", r == acting).Set("why", why))
	}
	unused := []any{}
	for _, b := range bad {
		unused = append(unused, pyjson.NewObject().Set("file", b.File).Set("why", b.Why))
	}
	rh := []any{}
	for _, r := range rivals {
		rh = append(rh, pyjson.NewObject().Set("file", r.file).Set("hook", r.what))
	}
	dir := gateroot.Join(root, "grafts")
	if p.flag("--json") {
		doc := pyjson.NewObject().Set("dir", dir).Set("rules", outRules).Set("unused", unused).Set("rival_hooks", rh)
		e.echo("%s\n", pyjson.DumpsIndent(doc, 2, true))
		return nil
	}
	text := ""
	if len(rules) > 0 || len(bad) > 0 {
		text += "Graft rules in " + dir + ":\n"
	} else {
		text += "No graft rules in " + dir + ".\n"
	}
	for _, r := range rules {
		tail := "in force"
		if r != acting {
			if !r.Acting() {
				tail = "not in force: state " + r.State + " does not act"
			} else {
				tail = "not in force: " + acting.File + " acts first"
			}
		}
		text += fmt.Sprintf("  %s: %s v%s, state %s, reads over %s lines, %s\n", r.File, r.ID, r.Version.Text,
			r.State, r.MinLines.Text, tail)
	}
	for _, b := range bad {
		text += fmt.Sprintf("  %s: not used: %s\n", b.File, b.Why)
	}
	if len(rivals) > 0 {
		text += "PreToolUse hooks that may rewrite input (install refuses):\n"
		for _, r := range rivals {
			text += fmt.Sprintf("  %s: %s\n", r.file, r.what)
		}
	} else {
		text += "PreToolUse hooks that may rewrite input: none\n"
	}
	e.echo("%s", text)
	return nil
}

func (e *Env) graftRemove(args []string) error {
	const cmd = "graft remove"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--id"}, value: true, metavar: "TEXT", help: "The rule's id and file name."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Remove a graft rule file. The gate is not changed.", opts)
	}
	id, err := e.graftIDOK(p)
	if err != nil {
		return err
	}
	path := gateroot.Join(gateroot.Join(gateroot.Join(e.graftDataDir(p), "gate"), "grafts"), id+".json")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			e.echo("No graft rule %s at %s.\n", id, path)
			return nil
		}
		return e.fail(cmd, err)
	}
	e.echo("Removed graft rule %s: %s\n", id, path)
	return nil
}
