//! `daisugi install --gate` and `--harness`: the gate half of install.

use super::gateroot::{self, join, parent};
use super::install::{self, Change, HarnessErr, InstErr, Runtime};
use super::{exit, parse_args, Env, Opt, Parsed, Res};

const INSTALL_OPTS: &[Opt] = &[
    Opt::flag(&["--dry-run"], "Show what would change without writing anything."),
    Opt::flag(&["--yes", "-y"], "Skip confirmation prompt."),
    Opt::flag(&["--print-skill"], "Not in this binary yet."),
    Opt::flag(&["--uninstall"], "With --gate, remove the gate hooks; with --harness, the extensions."),
    Opt::many(&["--runtime"], "TEXT", "Target named runtime(s) only, e.g. --runtime claude."),
    Opt::many(&["--harness"], "TEXT", "Install a loop harness's gate extension. Supported: pi, opencode."),
    Opt::pair(&["--gate"], "--no-gate", "Install the fail-closed verify hook (shadow by default)."),
    Opt::pair(&["--enforce"], "--shadow", "Gate mode: --enforce denies out-of-envelope calls; --shadow only observes."),
    Opt::pair(&["--ask"], "--no-ask", "Not in this binary yet."),
    Opt::pair(&["--gateway"], "--no-gateway", "Not in this binary yet."),
    Opt::pair(&["--allow-shell-decomposition"], "--no-allow-shell-decomposition", "Not in this binary yet."),
    Opt::val(&["--base-url"], "TEXT", "Not in this binary yet."),
    Opt::val(&["--report"], "TEXT", "Not in this binary yet."),
    Opt::val(&["--router"], "TEXT", "Not in this binary yet."),
    Opt::val(&["--efficient-model"], "TEXT", "Not in this binary yet."),
    Opt::val(&["--capable-model"], "TEXT", "Not in this binary yet."),
    Opt::val(&["--api-key-env"], "TEXT", "Not in this binary yet."),
];

/// What install --gate --enforce says when no envelope is registered:
/// Python's cli.ENFORCE_NEEDS_POLICY.
pub const ENFORCE_NEEDS_POLICY: &str = "Enforce needs a policy first. Run: daisugi gate init --workspace DIR \
     for a starter envelope, then this command again. Or install in shadow mode: daisugi install --gate";

/// The install flags this binary does not carry. A run that names one is
/// refused before anything is read or written.
const NOT_YET_FLAGS: &[&str] = &[
    "--print-skill",
    "--gateway",
    "--allow-shell-decomposition",
    "--base-url",
    "--report",
    "--router",
    "--efficient-model",
    "--capable-model",
    "--api-key-env",
    "--ask",
];

impl Env {
    pub(super) fn install(&mut self, args: &[String]) -> Res {
        let p = match parse_args(args, INSTALL_OPTS, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("install", m),
        };
        if p.help {
            return self.cmd_help(
                "install",
                "",
                "Wire the gate into agent harnesses: --gate writes the gate hook (Claude Code, Codex); --harness \
                 writes the pi or OpenCode extension. The skill, MCP, capture and instruction layers are not in this \
                 binary yet.",
                INSTALL_OPTS,
            );
        }
        for f in NOT_YET_FLAGS {
            // --no-gateway changes nothing; --no-allow-shell-decomposition
            // writes config.yaml in Python, so it is refused too.
            if p.has(f) || p.flag(f) || (*f == "--allow-shell-decomposition" && p.flag_set(f)) {
                return self.not_yet(&format!("daisugi install {f}"));
            }
        }
        let harness = p.list("--harness");
        if !harness.is_empty() {
            return self.install_harness(&harness, &p);
        }
        if !p.flag("--gate") {
            if p.flag("--uninstall") {
                return self.not_yet("daisugi install --uninstall without --gate");
            }
            return self.not_yet("daisugi install without --gate or --harness");
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
            return self.uninstall_gate(&runtimes);
        }
        let (enforce, ask) = (p.flag("--enforce"), false);
        let root = join(&self.home, ".opendaisugi/gate");
        // With no envelope registered, an enforce hook denies every call, so
        // the agent can do nothing at all. Refuse before anything is written.
        if enforce {
            match gateroot::envelopes(&root) {
                Err(why) => return self.refuse("install", &why),
                Ok(envs) if envs.is_empty() => {
                    self.errf(&format!("{ENFORCE_NEEDS_POLICY}\n"));
                    return exit(1);
                }
                Ok(_) => {}
            }
        }
        let me = match self.self_path() {
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
        // Decide every change before printing the plan: a settings file
        // this binary cannot edit refuses the run with nothing written.
        let plan_all = || -> Result<Vec<Change>, InstErr> {
            runtimes.iter().map(|rt| install::plan_apply(rt, &home, &me, &root, enforce, ask)).collect()
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
            for s in install::plan(rt, &self.home, enforce, ask) {
                let target = if s.target.is_empty() { String::new() } else { format!("  → {}", s.target) };
                let marker = if s.supported {
                    "+"
                } else {
                    gaps.push(format!("  [{}] gate: {}", rt.name, s.description));
                    "!"
                };
                self.out(&format!("  {marker} [gate] {}{target}\n", s.description));
            }
            self.out("\n");
        }
        if !gaps.is_empty() {
            self.out(&format!("Honest gaps (selected but not wired for this harness):\n{}\n\n", gaps.join("\n")));
        }
        self.out(
            "This binary installs the gate layer only. The skill, MCP, capture and instruction\n\
             layers come from the Python CLI's `daisugi install`.\n\n",
        );
        self.out(
            "The gate checks each call against a registered envelope — this install writes the hook, not the policy. \
             Run `daisugi gate init` to register one (add --allow-shell-decomposition to admit `a && b` and pipes).\n\n",
        );
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
            }
            let (followed, res) = install::apply_change(ch);
            self.note_followed(&followed);
            if let Err(e) = res {
                return self.fail("install", &e.to_string());
            }
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
        // A runtime that failed wrote nothing: name it and exit 1, never
        // report it as configured.
        for f in &failed {
            self.errf(&format!("Failed: {f}. Nothing was written for it.\n"));
        }
        if !failed.is_empty() {
            return exit(1);
        }
        Ok(())
    }

    fn note_followed(&mut self, followed: &[String]) {
        for f in followed {
            if let Some((link, target)) = f.split_once(" -> ") {
                self.note_file(link, &Some(target.to_string()));
            }
        }
    }

    fn uninstall_gate(&mut self, runtimes: &[Runtime]) -> Res {
        let mut changes = vec![];
        for rt in runtimes {
            match install::plan_reverse(rt, &self.home) {
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
            // A runtime left untouched, and why: the run exits 1.
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
                "{} asks the resident gate (`daisugi gate serve`), which this binary does not serve yet: run it from \
                 the Python CLI. The gate only watches until you set gate_mode: enforce.\n",
                label(h)
            ));
        }
        Ok(())
    }
}
