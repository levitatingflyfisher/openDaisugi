//! `daisugi install`: every layer of install, its reverse, and `--harness`.

use super::wardenroot::{self, join, parent};
use super::install::{self, Change, HarnessErr, InstErr, Runtime};
use super::installrouter::DEFAULT_BASE_URL;
use super::layers;
use std::sync::{Arc, Mutex};

type UnitLines = Arc<Mutex<Vec<(bool, String)>>>;
use crate::warden::pyjson::Value;
use super::{exit, parse_args, Env, Opt, Parsed, Res};

const INSTALL_OPTS: &[Opt] = &[
    Opt::flag(&["--dry-run"], "Show what would change without writing anything."),
    Opt::flag(&["--yes", "-y"], "Skip confirmation prompt."),
    Opt::flag(&["--print-skill"], "Print the opendaisugi-checklist skill content to stdout."),
    Opt::flag(&["--list"], "List the components install can write, and stop."),
    Opt::flag(&["--uninstall"], "Reverse all managed changes."),
    Opt::many(&["--runtime"], "TEXT", "Target named runtime(s) only, e.g. --runtime claude."),
    Opt::many(&["--harness"], "TEXT", "Install a loop harness's warden extension. Supported: pi, opencode."),
    Opt::many(
        &["--only"],
        "TEXT",
        "Install only these components, comma-separated or repeated: skill, mcp, capture, instructions, warden, pando, router. Default: all but router. See --list.",
    ),
    Opt::hidden(&["--warden"]),
    Opt::pair(&["--enforce"], "--audit", "Warden mode: --enforce denies out-of-envelope calls; --audit records them and allows."),
    Opt::hidden(&["--shadow"]),
    Opt::pair(&["--ask"], "--no-ask", "Hand an enforce-mode would-deny to a present operator for a bounded time."),
    Opt::pair(&["--router"], "--no-router", "Also point the harness at the local token-saving router (opt-in)."),
    Opt::pair(&["--allow-shell-decomposition"], "--no-allow-shell-decomposition", "Let the envelope admit compound shell (ADR-0010)."),
    Opt::val(&["--base-url"], "TEXT", "Router base_url to wire in when --router is set."),
    Opt::val(&["--report"], "TEXT", "Report warden state to a floor host: herdr or coppice."),
    Opt::val(&["--routing-mode"], "TEXT", "Who picks the model for each router turn: rules, switchyard, knn, or off."),
    Opt::val(&["--efficient-model"], "TEXT", "The model id of Switchyard's efficient tier."),
    Opt::val(&["--capable-model"], "TEXT", "The model id of Switchyard's capable tier."),
    Opt::val(&["--api-key-env"], "TEXT", "An environment variable holding an Anthropic API key for Switchyard's cloud tiers."),
];

/// What install --enforce says when no envelope is registered:
/// Python's cli.ENFORCE_NEEDS_POLICY.
pub const ENFORCE_NEEDS_POLICY: &str = "Enforce needs a policy first. Run: daisugi warden init --workspace DIR \
     for a starter envelope, then this command again. Or run: daisugi install (audit mode records every call and blocks none)";

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
                "Wire openDaisugi into every detected agent harness: the skill, the MCP server, the capture hook, \
                 the pathway instructions and the warden hook in audit mode; --enforce makes the warden deny, --router \
                 points the harness at the token-saving router, --harness writes the pi or OpenCode extension.",
                INSTALL_OPTS,
            );
        }
        if p.flag("--shadow") {
            // `cli._shadow_moved`: the old name of --audit, one line.
            return self.usage("install", "Invalid value for '--shadow': shadow mode is now audit mode: use --audit".into());
        }
        if p.flag("--warden") {
            // `cli._warden_flag_moved`: the warden hook is part of every install, one line.
            return self.usage(
                "install",
                "Invalid value for '--warden': the warden hook is part of every install: drop --warden (add --enforce to make it deny)"
                    .into(),
            );
        }
        if p.flag("--print-skill") {
            self.out(&format!("{}\n", layers::skill_text()));
            return Ok(());
        }
        if p.flag("--list") {
            self.out(&format!("{}\n", super::components::list_components()));
            return Ok(());
        }
        let only_names = p.list("--only");
        let only: Option<Vec<String>> = if only_names.is_empty() {
            None
        } else {
            match super::components::component_layers(&only_names) {
                Ok(l) => Some(l),
                Err(m) => {
                    self.errf(&format!("{m}\n"));
                    return exit(2);
                }
            }
        };
        if let Some(o) = &only {
            if !p.flag("--uninstall") && p.list("--harness").is_empty() {
                if p.flag("--enforce") && !o.iter().any(|l| l == "warden") {
                    self.errf("--enforce needs the warden component: add warden to --only\n");
                    return exit(2);
                }
                if p.flag("--router") && !o.iter().any(|l| l == "base_url") {
                    self.errf("--router is a component: add router to --only, or drop --router\n");
                    return exit(2);
                }
            }
        }
        if !p.flag("--uninstall") {
            // The names of the warden and the router changed: move the owner's
            // own state once, before anything new is written.
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_secs() as i64)
                .unwrap_or(0);
            let mut sink = Vec::new();
            let moved = super::moveold::move_old_state(&self.data_home(), &self.home, &self.env, now, &mut sink, p.flag("--dry-run"));
            self.out(&String::from_utf8_lossy(&sink));
            if moved.is_err() {
                return exit(2);
            }
        }
        let harness = p.list("--harness");
        if !harness.is_empty() {
            if only.is_some() {
                self.out("Note: --only does not apply to --harness.\n");
            }
            return self.install_harness(&harness, &p);
        }
        let gw = p.flag("--router") || only.as_ref().is_some_and(|o| o.iter().any(|l| l == "base_url"));
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
            return self.uninstall_all(&runtimes, only.as_deref());
        }
        let (enforce, ask) = (p.flag("--enforce"), p.flag("--ask"));
        let root = join(&self.data_home(), "warden");
        // With no envelope registered, an enforce hook denies every call, so
        // the agent can do nothing at all. Refuse before anything is written.
        if enforce {
            match wardenroot::envelopes(&root) {
                Err(why) => return self.refuse("install", &why),
                Ok(envs) if envs.is_empty() => {
                    self.errf(&format!("{ENFORCE_NEEDS_POLICY}\n"));
                    return exit(1);
                }
                Ok(_) => {}
            }
        }
        if ask && !enforce {
            self.out("Note: --ask only applies with --enforce; no operator ask will be wired.\n");
        }
        let effective_ask = ask && enforce;
        let report = p.str("--report", "");
        if p.has("--report") && report != "herdr" && report != "coppice" {
            self.errf("Error: --report must be herdr or coppice. Run: daisugi install --report herdr\n");
            return exit(1);
        }
        // The routing mode is checked before anything is written, so a bad
        // value leaves the harness and config.yaml as they were.
        let router = self.plan_routing_update(&p, gw)?;
        let me = match self.entry_path() {
            Ok(s) => s,
            Err(e) => return self.fail("install", &e),
        };
        // A mise install runs from a versioned path; the hook names mise's
        // shim, or an Omarchy stub, which survive an upgrade, when one runs
        // this binary.
        let env = self.env.clone();
        let (me, versioned) = install::hook_path(&me, &self.env, &|tool: &str| install::mise_which(&env, tool));
        if versioned {
            self.errf(&format!("{}\n", install::versioned_note(&me)));
        }
        let home = self.home.clone();
        let o = layers::Opts { router: gw, enforce, ask: effective_ask, url: base_url.clone(), report: report.clone(), only: only.clone() };
        // Decide every change before printing the plan: a file this binary
        // cannot edit refuses the run with nothing written.
        let plan_all = || -> Result<Vec<Change>, InstErr> {
            let mut changes = runtimes.iter().map(|rt| layers::plan_install(rt, &home, &me, &root, &o)).collect::<Result<Vec<_>, _>>()?;
            layers::dedupe_unit_edits(&mut changes);
            Ok(changes)
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
        let mut warden_wired = false;
        let mut unit_shown = false;
        for rt in &runtimes {
            self.out(&format!("[{}]\n", rt.name));
            for s in layers::plan_all_steps(rt, &self.home, &o) {
                if s.layer == layers::LAYER_PANDO && s.target.ends_with(layers::PANDO_UNIT_NAME) {
                    if unit_shown {
                        continue; // Claude Code and Hermes share the one unit
                    }
                    unit_shown = true;
                }
                let target = if s.target.is_empty() { String::new() } else { format!("  → {}", s.target) };
                if s.layer == "warden" && s.supported {
                    warden_wired = true;
                }
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
        if o.has("capture") {
            let captures = self.tilde(&join(&self.data_home(), "captures"));
            self.out(&format!("Tool calls are captured to {captures}/ for distillation.\n\n"));
        } else {
            self.out("\n");
        }
        if warden_wired {
            self.out(
                "The warden checks each call against a registered envelope — this install writes the hook, not the \
                 policy. Run `daisugi warden init` to register one (add --allow-shell-decomposition to admit `a && b` \
                 and pipes).\n\n",
            );
        }
        if let Some(u) = &router {
            let r = u.iter().find(|(k, _)| k == "routing_mode").and_then(|(_, v)| v.as_str()).unwrap_or_default().to_string();
            self.out(&format!("Router: set routing_mode to {r}.\n"));
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
        let (mut written, mut failed, mut notes) = (vec![], vec![], vec![]);
        let unit_lines = self.arm_unit_hook();
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
                layers::set_unit_hook(None);
                return self.fail("install", &e.to_string());
            }
            // A runtime whose apply raised lists nothing, even the files
            // it wrote before it raised, as Python's does.
            if ch.failed {
                failed.push(format!("{}: {}", ch.runtime.name, ch.why));
            } else {
                written.extend(ch.written());
            }
            notes.extend(ch.edits.iter().filter(|e| !e.note.is_empty()).map(|e| e.note.clone()));
        }
        layers::set_unit_hook(None);
        self.say_unit_lines(&unit_lines);
        if !written.is_empty() {
            self.out("\nDone. Files modified:\n");
            for f in &written {
                self.out(&format!("  {f}\n"));
            }
            for n in &notes {
                self.out(&format!("{n}\n"));
            }
            self.out(&format!("\n{}\n", layers::RESTART_LINE));
        } else if failed.is_empty() {
            // A hook install left as it is was not configured by this run.
            if notes.iter().any(|n| n.contains(layers::LEFT_AS_IS)) {
                self.out("\n");
            } else {
                self.out("\nAll runtimes were already configured — nothing changed.\n");
            }
            for n in &notes {
                self.out(&format!("{n}\n"));
            }
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
            self.apply_routing_update(u)?;
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
        let ok_runtimes: Vec<&install::Runtime> = changes.iter().filter(|c| !c.failed).map(|c| &c.runtime).collect();
        let broken = layers::unrunnable_commands(&ok_runtimes, &self.home.clone());
        for line in &broken {
            self.errf(&format!("{line}\n"));
        }
        if let Some(line) = layers::temporary_exe_note(&me) {
            self.errf(&format!("{line}\n"));
        }
        if !failed.is_empty() || !broken.is_empty() {
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

    /// Sets the hook that runs systemctl for the pando follow service, and
    /// returns where its lines go. The real systemctl runs only for the real
    /// home of the user who runs it (never for a scratch HOME, so a test or a
    /// compare run cannot touch the owner's services) and only with
    /// DAISUGI_NO_SYSTEMCTL unset. A failure is one line and goes on: the
    /// files are written either way.
    fn arm_unit_hook(&self) -> UnitLines {
        let lines: UnitLines = Arc::new(Mutex::new(vec![]));
        if let Some(h) = layers::test_unit_hook() {
            layers::set_unit_hook(Some(h));
            return lines;
        }
        if self.env.get("DAISUGI_NO_SYSTEMCTL").is_some_and(|v| !v.is_empty()) {
            return lines;
        }
        let real = crate::warden::paths::pw_home().is_some_and(|h| h.trim_end_matches('/') == self.home.trim_end_matches('/'));
        let Some(bin) = install::look_path(&self.env, "systemctl") else { return lines };
        if !real {
            return lines;
        }
        let env = self.env.clone();
        let sink = lines.clone();
        let run = move |args: &[&str]| -> Result<(), String> {
            let out = std::process::Command::new(&bin)
                .arg("--user")
                .args(args)
                .env_clear()
                .envs(&env)
                .output()
                .map_err(|e| format!("systemctl --user {}: {e}", args[0]))?;
            if out.status.success() {
                Ok(())
            } else {
                let text = String::from_utf8_lossy(&out.stderr);
                Err(format!("systemctl --user {}: {}: {}", args[0], out.status, text.lines().next().unwrap_or("")))
            }
        };
        layers::set_unit_hook(Some(Arc::new(move |action: &str, unit: &str| -> Result<(), String> {
            let r = match action {
                "enable" => run(&["daemon-reload"]).and_then(|_| run(&["enable", "--now", unit])),
                "disable" => run(&["disable", "--now", unit]),
                _ => run(&["daemon-reload"]),
            };
            let by_hand = match action {
                "enable" => "enable --now",
                "disable" => "disable --now",
                _ => "daemon-reload",
            };
            let mut s = sink.lock().unwrap_or_else(|e| e.into_inner());
            match &r {
                Ok(()) if action == "enable" => s.push((false, format!("Pando follow service is enabled and running ({unit})."))),
                Ok(()) => {}
                Err(e) => s.push((true, format!("Pando follow service: {e}. Do it yourself: systemctl --user {by_hand} {unit}"))),
            }
            r
        })));
        lines
    }

    fn say_unit_lines(&mut self, lines: &UnitLines) {
        let all: Vec<(bool, String)> = lines.lock().unwrap_or_else(|e| e.into_inner()).clone();
        for (is_err, l) in all {
            if is_err {
                self.errf(&format!("{l}\n"));
            } else {
                self.out(&format!("{l}\n"));
            }
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
    pub(super) fn uninstall_all(&mut self, runtimes: &[Runtime], only: Option<&[String]>) -> Res {
        let mut changes = vec![];
        let mut gone = std::collections::HashSet::new();
        for rt in runtimes {
            match layers::plan_reverse_all(rt, &self.home, &mut gone, only) {
                Ok(c) => changes.push(c),
                Err(e) => return self.refuse("install --uninstall", &e.to_string()),
            }
        }
        layers::dedupe_unit_edits(&mut changes);
        let (mut written, mut failed) = (vec![], vec![]);
        let unit_lines = self.arm_unit_hook();
        for ch in &changes {
            if ch.failed {
                failed.push(format!("{}: {}", ch.runtime.name, ch.why));
                continue;
            }
            let (followed, res) = install::apply_change(ch);
            self.note_followed(&followed);
            if let Err(e) = res {
                layers::set_unit_hook(None);
                return self.fail("install --uninstall", &e.to_string());
            }
            written.extend(ch.written());
        }
        layers::set_unit_hook(None);
        self.say_unit_lines(&unit_lines);
        let names: Vec<&str> = runtimes.iter().map(|r| r.name).collect();
        let what = only.map(|o| format!("{} ", super::components::names_of(o).join(", "))).unwrap_or_default();
        self.out(&format!("Uninstalled {what}from: {}\n", names.join(", ")));
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
                self.errf(&format!("error: unknown harness {}. Supported: pi, opencode.\n", crate::warden::py::text::repr(h)));
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
                Ok(_) => self.out(&format!("[{h}] warden extension installed. {}\n", restart(h))),
            }
        }
        for h in names {
            self.out(&format!(
                "{} asks the warden in-process. The warden only watches until you set warden_mode: enforce. \
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

#[cfg(test)]
mod pando_unit_tests {
    use super::*;
    use std::collections::HashMap;

    // The unit hook is process-wide: one test at a time, with the layer's tests.
    use super::layers::HOOK_TEST_LOCK as ONE;

    fn env_for(home: &std::path::Path) -> Env {
        let mut env = HashMap::new();
        env.insert("HOME".to_string(), home.to_string_lossy().into_owned());
        env.insert("PATH".to_string(), "/nonexistent".to_string());
        Env { args: vec![], env, home: String::new(), out: vec![], err: vec![], tend_failed: false, raised: false, quiet: false, plain: false }
    }

    fn run(home: &std::path::Path, args: &[&str]) -> (Result<(), super::super::Stop>, String) {
        let mut e = env_for(home);
        e.home = home.to_string_lossy().into_owned();
        let args: Vec<String> = args.iter().map(|s| s.to_string()).collect();
        // The install verb itself, not `run`: `run` sets the process-wide
        // llm_check world once, and a lib test that does so changes what
        // every later verify in the same process decides.
        let r = e.install(&args[1..]);
        (r, String::from_utf8_lossy(&e.out).into_owned())
    }

    #[test]
    fn install_starts_the_pando_service_once_and_uninstall_stops_it() {
        let _g = ONE.lock().unwrap_or_else(|e| e.into_inner());
        let home = std::env::temp_dir().join(format!("pando-unit-t-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&home);
        std::fs::create_dir_all(home.join(".claude")).unwrap();
        std::fs::create_dir_all(home.join(".hermes")).unwrap();
        let calls: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(vec![]));
        let c2 = calls.clone();
        layers::set_test_unit_hook(Some(Arc::new(move |a: &str, _u: &str| {
            c2.lock().unwrap().push(a.to_string());
            Ok(())
        })));
        let base = ["install", "--runtime", "claude", "--runtime", "hermes", "--only", "pando"];
        // The dry run says what it would do and calls nothing; the unit is listed once.
        let dry: Vec<&str> = base.iter().copied().chain(["--dry-run"]).collect();
        let (r, out) = run(&home, &dry);
        assert!(r.is_ok());
        assert!(out.contains("enable and start it") && calls.lock().unwrap().is_empty(), "{out}");
        assert_eq!(out.matches("follow user service").count(), 1, "{out}");
        let yes: Vec<&str> = base.iter().copied().chain(["--yes"]).collect();
        let (r, out) = run(&home, &yes);
        assert!(r.is_ok(), "{out}");
        assert_eq!(calls.lock().unwrap().join(","), "enable", "{out}");
        calls.lock().unwrap().clear();
        let un: Vec<&str> = base.iter().copied().chain(["--uninstall"]).collect();
        let (r, out) = run(&home, &un);
        layers::set_test_unit_hook(None);
        assert!(r.is_ok(), "{out}");
        assert_eq!(calls.lock().unwrap().join(","), "disable,reload");
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn a_scratch_home_never_runs_systemctl() {
        let _g = ONE.lock().unwrap_or_else(|e| e.into_inner());
        layers::set_test_unit_hook(None);
        let e = env_for(&std::env::temp_dir());
        let lines = e.arm_unit_hook();
        assert!(lines.lock().unwrap().is_empty());
        assert!(!layers::unit_hook_is_set(), "a home that is not the user's own must never reach systemctl");
    }
}
