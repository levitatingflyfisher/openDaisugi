package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// defaultLowStakes is defaults.DEFAULT_LOW_STAKES_ENVELOPE.
const defaultLowStakes = `{"id": "env_default_low_stakes", "generated_by": "opendaisugi-library-default",
"task": "<default low-stakes envelope>", "permissions": {"file_read": ["**"], "file_write": ["/tmp/**", "./out/**"],
"network": false, "network_hosts": [], "shell": false, "shell_allowlist": [], "max_execution_time_s": 30,
"max_output_size_mb": 10}, "invariants": [], "postconditions": [],
"summary": "Default low-stakes envelope (dev/sandbox use)"}`

// tilde is cli._tilde: a path under the home directory as ~/..., once
// resolved.
func (e *Env) tilde(p string) string {
	r, err := gateroot.Resolve(p)
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(e.home, r)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return p
	}
	return "~/" + rel
}

// echoResolvedAt is cli._echo_resolved(data_dir) for any data directory.
func (e *Env) echoResolvedAt(dataDir string) error {
	root := gateroot.Join(dataDir, "gate")
	gate := "disarmed"
	if !gateroot.IsDisarmed(root) {
		var err error
		if gate, err = config.GateMode(gateroot.Join(dataDir, "config.yaml")); err != nil {
			return err
		}
	}
	e.note("backend: %s · gate: %s · data: %s", e.backend(), gate, e.tilde(dataDir))
	return nil
}

// generateEnvelope is `daisugi generate-envelope`: one envelope for TASK
// from a model, as generate_envelope makes it with no cache, pathway
// store or Tier-1 slot.
func (e *Env) generateEnvelope(args []string) error {
	const cmd = "generate-envelope"
	opts := []opt{
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "LLM model, provider/model: anthropic/..., openai/... or ollama/..."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Root data directory. Unused by this command but accepted for consistency."},
		{names: []string{"--json"}, help: "Emit JSON instead of YAML."},
		{names: []string{"--stakes"}, value: true, metavar: "TEXT", help: "Stakes level: low (uses default), medium (cache), high (always fresh)."},
		{names: []string{"--low-stakes-envelope"}, value: true, metavar: "PATH", help: "Path to a JSON Envelope file; used when --stakes low is set."},
		{names: []string{"--thinking-budget"}, value: true, metavar: "TEXT", help: "Thinking budget: light, standard, deep (mapped per provider)."},
		{names: []string{"--llm"}, value: true, metavar: "TEXT", help: "LLM backend: api | claude-code. Default: auto-detect."},
		decomposeOpt,
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " TASK", "Generate a safety envelope for TASK via an LLM.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "TASK", "TASK")
	}
	task := p.args[0]
	model := p.str("--model", envgen.DefaultModel)
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	if v := p.str("--llm", ""); p.has("--llm") {
		if err := e.checkLLMFlag(v); err != nil {
			return err
		}
		e.env["OPENDAISUGI_LLM_BACKEND"] = v
		e.Environ = append(e.Environ, "OPENDAISUGI_LLM_BACKEND="+v)
	}
	stakes := p.str("--stakes", "medium")
	if stakes != "low" && stakes != "medium" && stakes != "high" {
		e.errf("Invalid --stakes value %s. Must be one of: high, low, medium\n", pystr.Repr(stakes))
		return exit(2)
	}
	thinking := p.str("--thinking-budget", "standard")
	if thinking != "light" && thinking != "standard" && thinking != "deep" {
		e.errf("Invalid --thinking-budget value %s. Must be one of: deep, light, standard\n", pystr.Repr(thinking))
		return exit(2)
	}
	if err := e.renamedBackendAt(cmd, dataDir); err != nil {
		return err
	}
	c := e.llmClient()
	if stakes != "low" {
		// A call this binary does not make the oracle's way is refused
		// before anything is said or asked.
		if err := c.Check(model); err != nil {
			return e.refuse(cmd, err)
		}
	}
	if err := e.echoResolvedAt(dataDir); err != nil {
		return e.refuse(cmd, err)
	}
	o := envgen.Options{Task: task, Models: []string{model}, Single: true, Stakes: stakes, Thinking: thinking,
		MaxRetries: 3, MaxTaskChars: 4000, LLM: c}
	if stakes == "low" {
		if p.has("--low-stakes-envelope") {
			path := p.str("--low-stakes-envelope", "")
			raw, err := os.ReadFile(path)
			if err != nil {
				return e.failPy(cmd, err)
			}
			if !utf8.Valid(raw) {
				e.errf("daisugi %s: UnicodeDecodeError: %s is not UTF-8\n", cmd, path)
				return exit(1)
			}
			v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, string(raw))
			if verr != nil {
				e.errf("daisugi %s: pydantic_core._pydantic_core.ValidationError: %s\n", cmd, verr.String())
				return exit(1)
			}
			o.LowStakes = v.(*pyjson.Object)
		} else {
			v, _ := pmodel.ValidateJSON("Envelope", pmodel.Envelope, defaultLowStakes)
			o.LowStakes = v.(*pyjson.Object)
		}
	}
	r, err := envgen.Generate(o)
	if err != nil {
		var pe *envgen.PyError
		if !errors.As(err, &pe) {
			return e.fail(cmd, err)
		}
		switch pe.Class {
		case "TaskTooLongError":
			e.errf("%s\n", pe.Msg)
			return exit(2)
		case "EnvelopeGenerationError", "ModelCallError":
			e.errf("Envelope generation failed: %s\n", pe.Msg)
			return exit(2)
		case "LLMNotConfigured":
			// main() prints an OpenDaisugiError as its text, exit 1.
			e.errf("%s\n", pystr.Strip(pe.Msg))
			return exit(1)
		}
		e.errf("daisugi %s: %s: %s\n", cmd, pe.Class, pe.Msg)
		return exit(1)
	}
	env := r.Envelope
	var decompose bool
	if p.flagSet("--allow-shell-decomposition") {
		decompose = p.flag("--allow-shell-decomposition")
	} else if decompose, err = e.decomposeDefault(gateroot.Join(dataDir, "gate"), cmd); err != nil {
		return err
	}
	if decompose {
		env = envgen.DeepCopy(env)
		if perms, ok := env.Value("permissions").(*pyjson.Object); ok {
			perms.Set("shell_allow_decomposition", true)
		}
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.DumpsIndent(env, 2, true))
		return nil
	}
	text, why := pyyaml.SafeDump(env)
	if why != nil {
		return e.refuse(cmd, fmt.Errorf("the envelope holds a value this binary does not write as YAML: %v", why))
	}
	e.out("%s\n", strings.TrimRight(text, " \t\n\r\x0b\x0c"))
	return nil
}

// renamedBackendAt is renamedBackend for _echo_resolved(data_dir).
func (e *Env) renamedBackendAt(cmd, dataDir string) error {
	if !llm.Renamed(e.backend()) {
		return nil
	}
	if !gateroot.IsDisarmed(gateroot.Join(dataDir, "gate")) {
		if _, err := config.GateMode(gateroot.Join(dataDir, "config.yaml")); err != nil {
			return e.refuse(cmd, err)
		}
	}
	e.errf("%s\n", llm.RenamedText)
	return exit(1)
}
