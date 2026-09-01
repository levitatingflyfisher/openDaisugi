package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/switchyard"
)

// planRouterUpdate is cli._plan_router_update: the config.yaml fields a
// --router choice sets, checked, or nil for no change. Every refusal
// exits 1 before anything is written.
func (e *Env) planRouterUpdate(p *parsed, gateway bool) (map[string]any, error) {
	efficient, capable := p.str("--efficient-model", ""), p.str("--capable-model", "")
	if !p.has("--router") {
		if efficient != "" || capable != "" || p.has("--api-key-env") {
			e.out("Note: --efficient-model, --capable-model and --api-key-env apply with --router switchyard.\n")
		}
		return nil, nil
	}
	router := p.str("--router", "")
	if !contains(routers, router) {
		return nil, e.fail3("unknown --router "+pystr.Repr(router)+".", "the gateway knows three choosers.",
			"choose one of: "+strings.Join(routers, ", "), 1)
	}
	if !gateway {
		e.out("Note: --router only applies with --gateway. This run writes no router config.\n")
		return nil, nil
	}
	update := map[string]any{"gateway_router": router}
	if router != "switchyard" {
		return update, e.checkRestate(update)
	}
	cfg, err := e.loadCfg("install", filepath.Join(e.home, ".opendaisugi", "config.yaml"))
	if err != nil {
		return nil, err
	}
	if efficient == "" && cfg.SwitchyardEfficientModel != nil {
		efficient = *cfg.SwitchyardEfficientModel
	}
	if efficient == "" {
		return nil, e.fail3("--router switchyard needs an efficient model.",
			"the stage router chooses between a capable tier and a cheaper one.",
			"add --efficient-model <id>: a local model id, or a claude-* id.", 1)
	}
	if capable == "" {
		capable = cfg.SwitchyardCapableModel
	}
	update["switchyard_efficient_model"] = efficient
	update["switchyard_capable_model"] = capable
	sc := syConfig(cfg)
	sc.EfficientModel, sc.CapableModel = &efficient, capable
	if p.has("--api-key-env") {
		if v := pystr.Strip(p.str("--api-key-env", "")); v != "" {
			update["switchyard_api_key_env"] = v
			sc.APIKeyEnv = &v
		} else {
			update["switchyard_api_key_env"] = nil
			sc.APIKeyEnv = nil
		}
	}
	// The key variable is checked when the gateway starts, in its own
	// environment, not in the shell that runs install.
	if _, err := switchyard.Render(switchyard.TargetsFromConfig(sc), cfg.SwitchyardRouteID,
		func(string) bool { return true }); err != nil {
		return nil, e.fail3("cannot build the Switchyard route: "+err.Error(),
			"the two tiers must name two different models.",
			"run again with a different --efficient-model or --capable-model.", 1)
	}
	return update, e.checkRestate(update)
}

// checkRestate refuses, before anything is written, a config.yaml this
// binary cannot write back the way save_config would. A file pydantic
// rejects is left to fail where Python fails, after the harness writes.
func (e *Env) checkRestate(update map[string]any) error {
	_, err := config.Dump(filepath.Join(e.home, ".opendaisugi", "config.yaml"), e.home, update)
	if err != nil && !errors.Is(err, config.ErrInvalid) {
		return e.refuse("install", fmt.Errorf("config.yaml is not one this binary rewrites: %w", err))
	}
	return nil
}

// applyRouterUpdate is cli._apply_router_update: the router fields saved,
// and for switchyard the TOML file written beside them.
func (e *Env) applyRouterUpdate(update map[string]any) error {
	cfgPath := filepath.Join(e.home, ".opendaisugi", "config.yaml")
	if err := config.Save(cfgPath, e.home, update); err != nil {
		if errors.Is(err, config.ErrInvalid) {
			e.errf("daisugi install: pydantic_core._pydantic_core.ValidationError: %s does not validate\n", cfgPath)
			return exit(1)
		}
		return e.refuse("install", fmt.Errorf("%s is not one this binary rewrites: %w", cfgPath, err))
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return e.fail("install", err)
	}
	if cfg.GatewayRouter != "switchyard" {
		e.out("Gateway router set to %s in %s. Restart `daisugi gateway` to use it.\n", cfg.GatewayRouter, cfgPath)
		return nil
	}
	targets := switchyard.TargetsFromConfig(syConfig(cfg))
	text, err := switchyard.Render(targets, cfg.SwitchyardRouteID, func(string) bool { return true })
	if err != nil {
		return e.fail("install", err)
	}
	tomlPath, err := switchyard.WriteConfig(gateroot.Join(e.home, ".opendaisugi"), text, "switchyard.toml")
	if err != nil {
		return e.fail("install", err)
	}
	auth := switchyard.AuthModes(targets)
	e.out("Switchyard router: wrote %s and set gateway_router in %s.\n", tomlPath, cfgPath)
	e.out("  capable tier %s: %s\n", targets.CapableID, auth.Capable)
	e.out("  efficient tier %s: %s\n", targets.EfficientID, auth.Efficient)
	if targets.APIKeyEnv != "" && e.env[targets.APIKeyEnv] == "" {
		e.out("  %s is not set in this shell. The gateway refuses to start until it is set in the shell that starts it.\n",
			targets.APIKeyEnv)
	}
	if switchyard.LookPath(switchyard.BinaryName, e.env["PATH"]) == "" {
		e.out("  switchyard-server is not on PATH yet. Install it with: %s\n", switchyard.InstallCmd)
	}
	e.out("  Start it with: daisugi gateway --router switchyard\n")
	return nil
}
