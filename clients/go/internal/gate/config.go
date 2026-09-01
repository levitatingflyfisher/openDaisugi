package gate

import (
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// The config beside the gate root: gate.resolve_gate_mode and
// gate._configured_verifier_client each call config.load_config on
// root.parent / "config.yaml", and each turns any exception into its
// default.

type loadedConfig struct{ mode, client, llmBackend string }

// loadConfig is config.load_config(path) for the two fields the gate
// reads. It raises what load_config raises. A file in YAML the port does
// not read is not guessed at: the call is denied undecided.
func loadConfig(p string) loadedConfig {
	if !exists(p) {
		return loadedConfig{mode: "shadow", client: "python"}
	}
	v, exc, why := pyyaml.Load(readText(p))
	if why != nil {
		unported("config.yaml holds YAML the port does not read (" + why.Why + ")")
	}
	if exc != nil {
		panic(exc)
	}
	// `yaml.safe_load(...) or {}`, then Config(**known): the model ignores
	// every key that is not a field.
	raw, ok := v.(*pyjson.Object)
	if !ok {
		if v != nil {
			unported("config.yaml is not a mapping")
		}
		raw = pyjson.NewObject()
	}
	out, verr := pmodel.Validate("Config", pmodel.Config, raw, pmodel.Python)
	if verr != nil {
		panic(pystr.NewException("ValidationError", verr.String()))
	}
	o := out.(*pyjson.Object)
	backend, _ := o.Value("llm_backend").(string)
	return loadedConfig{mode: o.Value("gate_mode").(string), client: o.Value("verifier_client").(string),
		llmBackend: backend}
}

func (r *runner) configPath() string {
	return pathJoin(pathParent(r.root), "config.yaml")
}

// resolveMode is gate.resolve_gate_mode: --mode wins; else config's
// gate_mode when it is shadow or enforce; else shadow.
func (r *runner) resolveMode(explicit *string) string {
	if explicit != nil {
		return *explicit
	}
	var c loadedConfig
	if exc := catch(func() { c = loadConfig(r.configPath()) }); exc != nil {
		return "shadow"
	}
	if c.mode == "shadow" || c.mode == "enforce" {
		return c.mode
	}
	return "shadow"
}

// verifierClient is gate._configured_verifier_client's client: python
// when the config names none or cannot be read.
func (r *runner) verifierClient() string {
	var c loadedConfig
	if exc := catch(func() { c = loadConfig(r.configPath()) }); exc != nil {
		return "python"
	}
	if c.client == "" {
		return "python"
	}
	return c.client
}

// internalError is _evaluate_record's fail-closed reason for msg. It
// names the configured client, read again, when that is not python.
func (r *runner) internalError(msg string) string {
	chosen := r.verifierClient()
	if chosen == "python" {
		return "gate internal error (denied fail-closed): " + msg + "."
	}
	return "gate internal error with verifier_client=" + chosen + " (denied fail-closed): " + msg +
		". Set verifier_client: python in config.yaml to rule out the client."
}
