package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// configLoadErr ends a command whose config.yaml load_config raises on:
// exit 1 with the exception Python's traceback ends in, or the refusal
// when the file is outside the YAML this binary reads.
func (e *Env) configLoadErr(cmd string, err error) error {
	var attr *config.AttributeError
	switch {
	case errors.As(err, &attr):
		e.errf("daisugi %s: %s\n", cmd, attr.Error())
		return exit(1)
	case errors.Is(err, config.ErrInvalid):
		e.errf("daisugi %s: pydantic_core._pydantic_core.ValidationError: the config file does not validate\n", cmd)
		return exit(1)
	}
	return e.refuse(cmd, fmt.Errorf("the config file is not one this binary reads: %w", err))
}

// configCmd is `daisugi config`: every setting as daisugi will use it,
// and where it came from (config.resolved_config).
func (e *Env) configCmd(args []string) error {
	opts := []opt{jsonOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("config", err)
	}
	if p.help {
		return e.cmdHelp("config", "", "Show every setting as daisugi will use it, and where each one came from.", opts)
	}
	path := e.home + "/.opendaisugi/config.yaml"
	if e.home == "/" {
		path = "/.opendaisugi/config.yaml"
	}
	rows, unknown, cfg, err := config.Rows(path, e.home)
	if err != nil {
		return e.configLoadErr("config", err)
	}
	// llm_backend (resolved): the environment, then the file, then what
	// runs on this box.
	backendEnv := e.env["OPENDAISUGI_LLM_BACKEND"]
	backendFile := ""
	if cfg.LLMBackend != nil {
		backendFile = pystr.Strip(*cfg.LLMBackend)
	}
	backend, source := backendEnv, "env"
	switch {
	case backendEnv != "":
	case backendFile != "":
		backend, source = backendFile, "file"
	default:
		source = "auto"
		backend = "api"
		if e.env["ANTHROPIC_API_KEY"] == "" && e.env["ANTHROPIC_AUTH_TOKEN"] == "" {
			if _, err := install.LookPath(e.env)("claude"); err == nil {
				backend = "claude-code"
			}
		}
	}
	rows = append(rows, config.Row{Key: "llm_backend (resolved)", Value: backend, Source: source})
	eff, err := config.EffectiveHookMode(e.home, e.cwd())
	if err != nil {
		return e.refuse("config", fmt.Errorf("a Claude Code settings.json holds hooks this binary does not read yet"))
	}
	if eff.Mode != "" {
		rows = append(rows, config.Row{Key: "gate_mode (resolved)", Value: eff.Mode, Source: config.SourceLabel(eff)})
		if eff.CwdMode != "" && eff.GlobalMode != "" {
			rows = append(rows,
				config.Row{Key: "gate_mode (project)", Value: eff.CwdMode, Source: "project"},
				config.Row{Key: "gate_mode (global)", Value: eff.GlobalMode, Source: "global"},
				config.Row{Key: "gate_mode (coexistence)",
					Value: "both fire; the verdict is the intersection — either one enforcing denies", Source: "info"})
		}
	} else {
		src := "default"
		for _, r := range rows {
			if r.Key == "gate_mode" {
				src = r.Source
			}
		}
		rows = append(rows, config.Row{Key: "gate_mode (resolved)", Value: cfg.GateMode, Source: src})
	}
	if p.flag("--json") {
		body := pyjson.NewObject()
		for _, r := range rows {
			body.Set(r.Key, pyjson.NewObject().Set("value", r.Value).Set("source", r.Source))
		}
		body.Set("_meta", pyjson.NewObject().Set("path", path).Set("unknown_keys_ignored", strsAny(orEmptyStr(unknown))))
		e.out("%s\n", pyjson.DumpsIndent(body, 2, true))
		return nil
	}
	width := 0
	for _, r := range rows {
		if n := pystr.Len(r.Key); n > width {
			width = n
		}
	}
	note := ""
	if _, err := os.Stat(path); err != nil {
		note = "  (not present)"
	}
	e.out("config file: %s%s\n", path, note)
	for _, r := range rows {
		e.out("  %s%s  %s  (%s)\n", r.Key, strings.Repeat(" ", width-pystr.Len(r.Key)), r.Value, r.Source)
	}
	if len(unknown) > 0 {
		e.out("  unknown keys ignored: %s\n", strings.Join(unknown, ", "))
	}
	return nil
}

func orEmptyStr(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}
