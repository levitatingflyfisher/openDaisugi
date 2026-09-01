//! `install --router`: `cli._plan_router_update` and `_apply_router_update`.

use super::config::{self, ConfigErr, SaveErr};
use super::gatewaycmd::ROUTERS;
use super::routercmd::sy_config;
use super::{Env, Parsed, Stop};
use crate::gate::py::text::{repr, strip};
use crate::gate::pyjson::Value;
use crate::switchyard as sy;

pub type Update = Vec<(String, Value)>;

impl Env {
    /// `cli._plan_router_update`: the config.yaml fields a --router choice
    /// sets, checked, or None for no change. Every refusal exits 1 before
    /// anything is written.
    pub(super) fn plan_router_update(&mut self, p: &Parsed, gateway: bool) -> Result<Option<Update>, Stop> {
        let mut efficient = p.str("--efficient-model", "");
        let mut capable = p.str("--capable-model", "");
        if !p.has("--router") {
            if !efficient.is_empty() || !capable.is_empty() || p.has("--api-key-env") {
                self.out("Note: --efficient-model, --capable-model and --api-key-env apply with --router switchyard.\n");
            }
            return Ok(None);
        }
        let router = p.str("--router", "");
        if !ROUTERS.contains(&router.as_str()) {
            return Err(self.fail3(
                &format!("unknown --router {}.", repr(&router)),
                "the gateway knows three choosers.",
                &format!("choose one of: {}", ROUTERS.join(", ")),
                1,
            ));
        }
        if !gateway {
            self.out("Note: --router only applies with --gateway. This run writes no router config.\n");
            return Ok(None);
        }
        let mut update: Update = vec![("gateway_router".into(), Value::Str(router.clone()))];
        if router != "switchyard" {
            self.check_restate(&update)?;
            return Ok(Some(update));
        }
        let cfg = self.load_cfg("install", &format!("{}/.opendaisugi/config.yaml", self.home))?;
        if efficient.is_empty() {
            if let Some(e) = &cfg.switchyard_efficient_model {
                efficient = e.clone();
            }
        }
        if efficient.is_empty() {
            return Err(self.fail3(
                "--router switchyard needs an efficient model.",
                "the stage router chooses between a capable tier and a cheaper one.",
                "add --efficient-model <id>: a local model id, or a claude-* id.",
                1,
            ));
        }
        if capable.is_empty() {
            capable = cfg.switchyard_capable_model.clone();
        }
        update.push(("switchyard_efficient_model".into(), Value::Str(efficient.clone())));
        update.push(("switchyard_capable_model".into(), Value::Str(capable.clone())));
        let mut sc = sy_config(&cfg);
        sc.efficient_model = Some(efficient);
        sc.capable_model = capable;
        if p.has("--api-key-env") {
            let v = strip(&p.str("--api-key-env", "")).to_string();
            if v.is_empty() {
                update.push(("switchyard_api_key_env".into(), Value::Null));
                sc.api_key_env = None;
            } else {
                update.push(("switchyard_api_key_env".into(), Value::Str(v.clone())));
                sc.api_key_env = Some(v);
            }
        }
        // The key variable is checked when the gateway starts, in its own
        // environment, not in the shell that runs install.
        let targets = sy::targets_from_config(&sc).unwrap_or_default();
        if let Err(why) = sy::render(&targets, &cfg.switchyard_route_id, &|_| true) {
            return Err(self.fail3(
                &format!("cannot build the Switchyard route: {why}"),
                "the two tiers must name two different models.",
                "run again with a different --efficient-model or --capable-model.",
                1,
            ));
        }
        self.check_restate(&update)?;
        Ok(Some(update))
    }

    /// Refuses, before anything is written, a config.yaml this binary
    /// cannot write back the way save_config would. A file pydantic rejects
    /// is left to fail where Python fails, after the harness writes.
    fn check_restate(&mut self, update: &Update) -> Result<(), Stop> {
        let path = format!("{}/.opendaisugi/config.yaml", self.home);
        match config::dump(&path, &self.home, update) {
            Ok(_) | Err(ConfigErr::Invalid) => Ok(()),
            Err(e) => Err(self.refuse("install", &format!("config.yaml is not one this binary rewrites: {e}")).unwrap_err()),
        }
    }

    /// `cli._apply_router_update`: the router fields saved, and for
    /// switchyard the TOML file written beside them.
    pub(super) fn apply_router_update(&mut self, update: &Update) -> Result<(), Stop> {
        let cfg_path = format!("{}/.opendaisugi/config.yaml", self.home);
        match config::save(&cfg_path, &self.home, update) {
            Ok(()) => {}
            Err(SaveErr::Config(ConfigErr::Invalid)) => {
                self.errf(&format!("daisugi install: pydantic_core._pydantic_core.ValidationError: {cfg_path} does not validate\n"));
                return Err(Stop::Exit(1));
            }
            Err(SaveErr::Config(e)) => {
                return Err(self.refuse("install", &format!("{cfg_path} is not one this binary rewrites: {e}")).unwrap_err())
            }
            Err(SaveErr::Io(e)) => return Err(self.fail("install", &e.to_string()).unwrap_err()),
        }
        let cfg = match config::load(&cfg_path) {
            Ok(c) => c,
            Err(e) => return Err(self.fail("install", &e.to_string()).unwrap_err()),
        };
        if cfg.gateway_router != "switchyard" {
            self.out(&format!(
                "Gateway router set to {} in {cfg_path}. Restart `daisugi gateway` to use it.\n",
                cfg.gateway_router
            ));
            return Ok(());
        }
        let targets = sy::targets_from_config(&sy_config(&cfg)).unwrap_or_default();
        let text = match sy::render(&targets, &cfg.switchyard_route_id, &|_| true) {
            Ok(t) => t,
            Err(why) => return Err(self.fail("install", &why).unwrap_err()),
        };
        let toml_path = match sy::write_config(&format!("{}/.opendaisugi", self.home), &text, "switchyard.toml") {
            Ok(p) => p,
            Err(e) => return Err(self.fail("install", &e.to_string()).unwrap_err()),
        };
        let auth = sy::auth_modes(&targets);
        self.out(&format!("Switchyard router: wrote {toml_path} and set gateway_router in {cfg_path}.\n"));
        self.out(&format!("  capable tier {}: {}\n", targets.capable_id, auth.capable));
        self.out(&format!("  efficient tier {}: {}\n", targets.efficient_id, auth.efficient));
        if !targets.api_key_env.is_empty() && self.env.get(&targets.api_key_env).is_none_or(|v| v.is_empty()) {
            self.out(&format!(
                "  {} is not set in this shell. The gateway refuses to start until it is set in the shell that starts it.\n",
                targets.api_key_env
            ));
        }
        let path = self.env.get("PATH").cloned().unwrap_or_default();
        if sy::look_path(sy::BINARY_NAME, &path).is_none() {
            self.out(&format!("  switchyard-server is not on PATH yet. Install it with: {}\n", sy::INSTALL_CMD));
        }
        self.out("  Start it with: daisugi gateway --router switchyard\n");
        Ok(())
    }
}
