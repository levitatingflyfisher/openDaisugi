//! `daisugi install`: every layer of install, its reverse, and `--harness`.

use super::gateroot::{self, join, parent};
use super::install::{self, Change, HarnessErr, InstErr, Runtime};
use super::installgw::DEFAULT_BASE_URL;
use super::layers;
use crate::gate::pyjson::Value;
use super::{exit, parse_args, Env, Opt, Parsed, Res};

const INSTALL_OPTS: &[Opt] = &[
    Opt::flag(&["--dry-run"], "Show what would change without writing anything."),
    Opt::flag(&["--yes", "-y"], "Skip confirmation prompt."),
    Opt::flag(&["--print-skill"], "Print the opendaisugi-checklist skill content to stdout."),
    Opt::flag(&["--uninstall"], "Reverse all managed changes."),
    Opt::many(&["--runtime"], "TEXT", "Target named runtime(s) only, e.g. --runtime claude."),
    Opt::many(&["--harness"], "TEXT", "Install a loop harness's gate extension. Supported: pi, opencode."),
    Opt::pair(&["--gate"], "--no-gate", "Also install the fail-closed verify hook (opt-in; audit by default)."),
    Opt::pair(&["--enforce"], "--audit", "Gate mode: --enforce denies out-of-envelope calls; --audit only observes."),
    Opt::hidden(&["--shadow"]),
    Opt::pair(&["--ask"], "--no-ask", "Hand an enforce-mode would-deny to a present operator for a bounded time."),
    Opt::pair(&["--gateway"], "--no-gateway", "Also point the harness at the local token-saving gateway (opt-in)."),
    Opt::pair(&["--allow-shell-decomposition"], "--no-allow-shell-decomposition", "Let the envelope admit compound shell (ADR-0010)."),
    Opt::val(&["--base-url"], "TEXT", "Gateway base_url to wire in when --gateway is set."),
    Opt::val(&["--report"], "TEXT", "Report gate state to a floor host: herdr or coppice."),
    Opt::val(&["--router"], "TEXT", "Who picks the model for each gateway turn: rules, switchyard, or off."),
    Opt::val(&["--efficient-model"], "TEXT", "The model id of Switchyard's efficient tier."),
    Opt::val(&["--capable-model"], "TEXT", "The model id of Switchyard's capable tier."),
    Opt::val(&["--api-key-env"], "TEXT", "An environment variable holding an Anthropic API key for Switchyard's cloud tiers."),
];

/// What install --gate --enforce says when no envelope is registered:
/// Python's cli.ENFORCE_NEEDS_POLICY.
pub const ENFORCE_NEEDS_POLICY: &str = "Enforce needs a policy first. Run: daisugi gate init --workspace DIR \
     for a starter envelope, then this command again. Or install in audit mode: daisugi install --gate";

impl Env {
    pub(super) fn install(&mut self, args: &[String]) -> Res {
        self.port_hop(args)?;
        let p = match parse_args(args, INSTALL_OPTS, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("install", m),
        };
        if p.help {
            return self.cmd_help(
                "install",
                "",
                "Wire openDaisugi into every detected agent harness: the skill, the MCP server, the capture hook and \
                 the pathway instructions; --gate adds the gate hook, --gateway points the harness at the \
                 token-saving gateway, --harness writes the pi or OpenCode extension.",
                INSTALL_OPTS,
            );
        }
        if p.flag("--shadow") {
            // `cli._shadow_moved`: the old name of --audit, one line.
            return self.usage("install", "Invalid value for '--shadow': shadow mode is now audit mode: use --audit".into());
        }
        if p.flag("--print-skill") {
            self.out(&format!("{}\n", layers::skill_text()));
            return Ok(());
        }
        let harness = p.list("--harness");
        if !harness.is_empty() {
            return self.install_harness(&harness, &p);
        }
        let (gate, gw) = (p.flag("--gate"), p.flag("--gateway"));
        let mut base_url = p.str("--base-url", "");
        if base_url.is_empty() {
            base_url = DEFAULT_BASE_URL.into();
        }
        let names = p.list("--runtime");
        let runtimes: Vec<Runtime> = if !names.is_empty() {
            match install::select(&names) {
                Ok(r) => r,
                Err(m) => {
                    self.errf(&format!("Error: {m}\n"));
                    return exit(2);
                }
            }
        } else {
            install::detect(&self.home, &self.env)
        };
        if runtimes.is_empty() {
            self.out("No supported agent runtimes detected (Claude Code, Codex, Hermes, OpenClaw).\n");
            self.out("Install one and re-run, or see docs/hook-integration.md for manual setup.\n");
            return Ok(());
        }
        if p.flag("--uninstall") {
            return self.uninstall_all(&runtimes);
        }
        let (enforce, ask) = (p.flag("--enforce"), p.flag("--ask"));
        let root = join(&self.data_home(), "gate");
        // With no envelope registered, an enforce hook denies every call, so
        // the agent can do nothing at all. Refuse before anything is written.
        if gate && enforce {
            match gateroot::envelopes(&root) {
                Err(why) => return self.refuse("install", &why),
                Ok(envs) if envs.is_empty() => {
                    self.errf(&format!("{ENFORCE_NEEDS_POLICY}\n"));
                    return exit(1);
                }
                Ok(_) => {}
            }
        }
        if enforce && !gate {
            self.out("Note: --enforce only applies with --gate; no gate hook will be installed.\n");
        }
        if ask && !(gate && enforce) {
            self.out("Note: --ask only applies with --gate --enforce; no operator ask will be wired.\n");
        }
        let effective_ask = ask && gate && enforce;
        let mut report = p.str("--report", "");
        if p.has("--report") && report != "herdr" && report != "coppice" {
            self.errf("Error: --report must be herdr or coppice. Run: daisugi install --gate --report herdr\n");
            return exit(1);
        }
        if p.has("--report") && !gate {
            self.out("Note: --report needs --gate. This run installs no floor-report hooks.\n");
        }
        if !gate {
            report.clear();
        }
        // The router choice is checked before anything is written, so a bad
        // value leaves the harness and config.yaml as they were.
        let router = self.plan_router_update(&p, gw)?;
        let me = match self.self_path() {
            Ok(s) => s,
            Err(e) => return self.fail("install", &e),
        };
        // A mise install runs from a versioned path; the hook names mise's
        // shim, or an Omarchy stub, which survive an upgrade, when one runs
        // this binary.
        let env = self.env.clone();
        let (me, versioned) = install::hook_path(&me, &self.env, &|tool: &str| install::mise_which(&env, tool));
        if versioned && gate {
            self.errf(&format!("{}\n", install::versioned_note(&me)));
        }
        let home = self.home.clone();
        let o = layers::Opts { gate, gateway: gw, enforce, ask: effective_ask, url: base_url.clone(), report: report.clone() };
        // Decide every change before printing the plan: a file this binary
        // cannot edit refuses the run with nothing written.
        let plan_all = || -> Result<Vec<Change>, InstErr> {
            runtimes.iter().map(|rt| layers::plan_install(rt, &home, &me, &root, &o)).collect()
        };
        let planned = match plan_all() {
            Ok(c) => c,
            Err(e) => return self.refuse("install", &e.to_string()),
        };
        self.out("\nDetected runtimes:\n");
        for ch in &planned {
            if ch.failed {
                self.out(&format!("  ! {}: {}\n", ch.runtime.name, ch.why));
            } else {
                self.out(&format!("  ✓ {}\n", ch.runtime.name));
            }
        }
        self.out("\nopenDaisugi will make these changes:\n\n");
        let mut gaps = vec![];
        for rt in &runtimes {
            self.out(&format!("[{}]\n", rt.name));
            for s in layers::plan_all_steps(rt, &self.home, &o) {
                let target = if s.target.is_empty() { String::new() } else { format!("  → {}", s.target) };
                let marker = if s.supported {
                    "+"
                } else {
                    gaps.push(format!("  [{}] {}: {}", rt.name, s.layer, s.description));
                    "!"
                };
                self.out(&format!("  {marker} [{}] {}{target}\n", s.layer, s.description));
            }
            self.out("\n");
        }
        if !gaps.is_empty() {
            self.out(&format!("Honest gaps (selected but not wired for this harness):\n{}\n\n", gaps.join("\n")));
        }
        self.out("Skill is discovered on demand — zero added tokens for simple sessions.\n");
        let captures = self.tilde(&join(&self.data_home(), "captures"));
        self.out(&format!("Tool calls are captured to {captures}/ for distillation.\n\n"));
        if gate {
            self.out(
                "The gate checks each call against a registered envelope — this install writes the hook, not the \
                 policy. Run `daisugi gate init` to register one (add --allow-shell-decomposition to admit `a && b` \
                 and pipes).\n\n",
            );
        }
        if let Some(u) = &router {
            let r = u.iter().find(|(k, _)| k == "gateway_router").and_then(|(_, v)| v.as_str()).unwrap_or_default().to_string();
            self.out(&format!("Router: set gateway_router to {r}.\n"));
        }
        if p.flag("--dry-run") {
            self.out("Dry run — no files written.\n");
            return Ok(());
        }
        if !p.flag("--yes") && !self.confirm("Proceed?")? {
            self.out("Aborted.\n");
            return Ok(());
        }
        // Plan again: the files may have changed while the prompt waited.
        let changes = match plan_all() {
            Ok(c) => c,
            Err(e) => return self.refuse("install", &e.to_string()),
        };
        let (mut written, mut failed) = (vec![], vec![]);
        for ch in &changes {
            for ed in &ch.edits {
                if !ed.warning.is_empty() {
                    self.errf(&format!("warning: {}\n", ed.warning));
                }
                if !ed.pre_warning.is_empty() {
                    self.errf(&format!("warning: {}\n", ed.pre_warning));
                }
            }
            let (followed, res) = install::apply_change(ch);
            self.note_followed(&followed);
            if let Err(e) = res {
                return self.fail("install", &e.to_string());
            }
            // A runtime whose apply raised lists nothing, even the files
            // it wrote before it raised, as Python's does.
            if ch.failed {
                failed.push(format!("{}: {}", ch.runtime.name, ch.why));
            } else {
                written.extend(ch.written());
            }
        }
        if !written.is_empty() {
            self.out("\nDone. Files modified:\n");
            for f in &written {
                self.out(&format!("  {f}\n"));
            }
            self.out("\nRestart your agent session to pick up the changes.\n");
        } else if failed.is_empty() {
            self.out("\nAll runtimes were already configured — nothing changed.\n");
        }
        for f in &failed {
            self.errf(&format!("Failed: {f}. Nothing was written for it.\n"));
        }
        let cfg_path = join(&self.data_home(), "config.yaml");
        if p.flag_set("--allow-shell-decomposition") {
            let allow = p.flag("--allow-shell-decomposition");
            self.save_install_config(&cfg_path, vec![("shell_allow_decomposition".into(), Value::Bool(allow))])?;
            let state = if allow { "on" } else { "off" };
            self.out(&format!(
                "Compound-shell decomposition default: {state} ({cfg_path}) — config is your data, so it survives \
                 --uninstall; edit or delete the file to reset it.\n"
            ));
        }
        if let Some(u) = &router {
            self.apply_router_update(u)?;
        }
        if !report.is_empty() {
            self.save_install_config(&cfg_path, vec![("floor_report".into(), Value::Str(report.clone()))])?;
            if report == "coppice" {
                self.out(&format!(
                    "Floor report set to coppice. Saved in {cfg_path}. The coppice server is built. Nothing reports to it yet.\n"
                ));
            }
        }
        // Ask once whether to distil repeated tasks in the background.
        // Interactive only: a --yes install leaves consent unasked.
        if !p.flag("--yes") {
            let cfg = match super::config::load(&cfg_path) {
                Ok(c) => c,
                Err(e) => return self.refuse("install", &format!("{cfg_path} is not one this binary rewrites: {e}")),
            };
            if cfg.auto_tend.is_none() {
                let yes = self.confirm_default(
                    "\nLet openDaisugi distil your repeated tasks in the background, so reuse compounds \
                     automatically? (only affects cost — the guard enforces safety either way)",
                    true,
                )?;
                self.save_install_config(&cfg_path, vec![("auto_tend".into(), Value::Bool(yes))])?;
                if yes {
                    self.out(
                        "Background distillation is ON — no cron needed: your capture hook kicks off a rate-limited \
                         tend on its own. (You can also run `daisugi hook auto-tend` any time.)\n",
                    );
                } else {
                    self.out("Left OFF — run `daisugi tend` yourself whenever you want it.\n");
                }
            }
        }
        if !failed.is_empty() {
            return exit(1);
        }
        Ok(())
    }

    /// `save_config(load_config(path).model_copy(update))`.
    fn save_install_config(&mut self, path: &str, update: Vec<(String, Value)>) -> Result<(), super::Stop> {
        use super::config::{ConfigErr, SaveErr};
        match super::config::save(path, &self.home.clone(), &update) {
            Ok(()) => Ok(()),
            Err(SaveErr::Config(ConfigErr::Invalid | ConfigErr::Yaml(_))) => {
                self.errf(&format!("daisugi install: pydantic_core._pydantic_core.ValidationError: {path} does not validate\n"));
                Err(super::Stop::Exit(1))
            }
            Err(SaveErr::Config(e)) => Err(self.refuse("install", &format!("{path} is not one this binary rewrites: {e}")).unwrap_err()),
            Err(SaveErr::Io(e)) => Err(self.fail("install", &e.to_string()).unwrap_err()),
        }
    }

    fn note_followed(&mut self, followed: &[String]) {
        for f in followed {
            if let Some((link, target)) = f.split_once(" -> ") {
                self.note_file(link, &Some(target.to_string()));
            }
        }
    }

    /// `install.uninstall`: every managed change reversed for each
    /// runtime.
    fn uninstall_all(&mut self, runtimes: &[Runtime]) -> Res {
        let mut changes = vec![];
        let mut gone = std::collections::HashSet::new();
        for rt in runtimes {
            match layers::plan_reverse_all(rt, &self.home, &mut gone) {
                Ok(c) => changes.push(c),
                Err(e) => return self.refuse("install --uninstall", &e.to_string()),
            }
        }
        let (mut written, mut failed) = (vec![], vec![]);
        for ch in &changes {
            if ch.failed {
                failed.push(format!("{}: {}", ch.runtime.name, ch.why));
                continue;
            }
            let (followed, res) = install::apply_change(ch);
            self.note_followed(&followed);
            if let Err(e) = res {
                return self.fail("install --uninstall", &e.to_string());
            }
            written.extend(ch.written());
        }
        let names: Vec<&str> = runtimes.iter().map(|r| r.name).collect();
        self.out(&format!("Uninstalled from: {}\n", names.join(", ")));
        if !failed.is_empty() {
            self.out(&format!("Failures (left untouched): {}\n", failed.join("; ")));
        }
        if !written.is_empty() {
            self.out("\nReverted:\n");
            for f in &written {
                self.out(&format!("  {f}\n"));
            }
        }
        if !failed.is_empty() {
            return exit(1);
        }
        Ok(())
    }

    fn install_harness(&mut self, names: &[String], p: &Parsed) -> Res {
        for h in names {
            if h != "pi" && h != "opencode" {
                self.errf(&format!("error: unknown harness {}. Supported: pi, opencode.\n", crate::gate::py::text::repr(h)));
                return exit(2);
            }
        }
        let restart = |h: &str| {
            if h == "pi" {
                "Restart pi, or run /reload in an interactive session, for it to take effect."
            } else {
                "Restart OpenCode for it to take effect."
            }
        };
        let label = |h: &str| if h == "pi" { "pi" } else { "OpenCode" };
        if p.flag("--uninstall") {
            for h in names {
                let mut target = install::target(h, &self.home, &self.env);
                if h == "pi" {
                    target = parent(&target);
                }
                if p.flag("--dry-run") {
                    self.out(&format!("[{h}] would remove {target}\n"));
                    continue;
                }
                match install::uninstall_harness(h, &self.home, &self.env) {
                    Err(e) => return self.fail("install", &e.to_string()),
                    Ok(removed) if !removed.is_empty() => self.out(&format!("[{h}] removed {} file(s).\n", removed.len())),
                    Ok(_) => self.out(&format!("[{h}] nothing was installed.\n")),
                }
            }
            return Ok(());
        }
        if p.flag("--dry-run") {
            for h in names {
                let t = install::target(h, &self.home, &self.env);
                self.out(&format!("[{h}] would write {t}\n"));
            }
            return Ok(());
        }
        for h in names {
            match install::install_harness(h, &self.home, &self.env) {
                Err(HarnessErr::Value(m)) => {
                    self.errf(&format!("[{h}] error: {m}\n"));
                    return exit(1);
                }
                Err(HarnessErr::Io(e)) => return self.fail("install", &e.to_string()),
                Ok(_) => self.out(&format!("[{h}] gate extension installed. {}\n", restart(h))),
            }
        }
        for h in names {
            self.out(&format!(
                "{} asks the gate in-process. The gate only watches until you set gate_mode: enforce. \
                 Start it with `daisugi start`.\n",
                label(h)
            ));
        }
        Ok(())
    }

    /// Hands the whole install to the port DAISUGI_PORT names, when that
    /// is not this one (ruling PK-R-15): runs that binary in place of
    /// this process, with the same arguments. Returns only when there is
    /// no hand-over.
    fn port_hop(&mut self, args: &[String]) -> Res {
        use std::os::unix::process::CommandExt;
        let me = match self.self_path() {
            Ok(s) => s,
            Err(e) => return self.fail("install", &e),
        };
        let target = match super::port::port_hop("rust", &me, &self.env) {
            Ok(None) => return Ok(()),
            Ok(Some(t)) => t,
            Err(msg) => {
                self.errf(&format!("daisugi: {msg}\n"));
                return exit(2);
            }
        };
        let mut argv: Vec<String> = vec![];
        if self.quiet {
            argv.push("-q".into());
        }
        if self.plain {
            argv.push("--plain".into());
        }
        argv.push("install".into());
        argv.extend(args.iter().cloned());
        self.flush();
        let err = std::process::Command::new(&target).args(&argv).env(super::port::PORT_HOP_ENV, "1").exec();
        self.errf(&format!("daisugi: cannot run {target}: {err}\n"));
        exit(1)
    }
}
