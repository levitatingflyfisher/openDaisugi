//! `daisugi start`: the gate hook scoped to this directory, a starter
//! envelope keyed to it, the resident gate, then the plain live view
//! (K4-5); the gate-server step starts this binary's own `gate serve`, as
//! Python's start starts its own (G2-4). The Go client's
//! `startcmd.go` is the reference.

use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use super::config;
use super::envelope;
use super::gateroot::{self, join, parent, path_str};
use super::install::{self, HookOptions};
use super::servecmd::{probe, Probe};
use super::statuscmd::DATA_DIR_OPT;
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::py::text::len as py_len;

/// `start.StartStep`.
struct Step {
    key: &'static str,
    state: &'static str,
    text: String,
}

fn step(key: &'static str, state: &'static str, text: impl Into<String>) -> Step {
    Step { key, state, text: text.into() }
}

/// The view step as the oracle words it without the Textual extra: this
/// binary opens the plain live view (K4-5).
const VIEW_TEXT: &str = "open the live view (daisugi dashboard); install the tui extra for the full instrument";

/// `StartOptions.server_wait_s`.
const SERVER_WAIT: Duration = Duration::from_secs(2);

/// `start._session_key`: the resolved directory's name, made
/// filename-safe, and 8 hex digits of the SHA-256 of its path.
fn session_key(cwd: &str) -> std::io::Result<String> {
    let resolved = gateroot::resolve(cwd)?;
    let base = resolved.rsplit('/').next().unwrap_or(&resolved);
    let safe: String =
        base.chars().map(|c| if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') { c } else { '_' }).collect();
    let name = safe.trim_matches(|c| c == '.' || c == '_');
    let name = if name.is_empty() { "start" } else { name };
    let sum = crate::gate::sha256::hexdigest(resolved.as_bytes());
    Ok(format!("{name}-{}", &sum[..8]))
}

impl Env {
    pub(super) fn start(&mut self, args: &[String]) -> Res {
        const CMD: &str = "start";
        let opts = [
            Opt::flag(&["--enforce"], "Install the gate in enforce mode (default: audit)."),
            Opt::flag(&["--ask"], "Let the gate hand a would-deny to a present operator. Needs --enforce."),
            Opt::flag(&["--no-ui"], "Do the setup, do not open the view."),
            Opt::flag(&["--dry-run"], "Show the steps and their state; change nothing."),
            DATA_DIR_OPT,
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Get a gated session over this directory and watch it in the live view.",
                &opts,
            );
        }
        let (enforce, ask, no_ui, dry) = (p.flag("--enforce"), p.flag("--ask"), p.flag("--no-ui"), p.flag("--dry-run"));
        if ask && !enforce {
            self.errf(
                "the --ask flag only matters in --enforce mode.\n\
                 in audit mode nothing is ever denied, so there is nothing to ask about.\n\
                 pass --enforce --ask together, or drop --ask.\n",
            );
            return exit(2);
        }
        let cwd = match gateroot::getwd() {
            Ok(c) => path_str(&c),
            Err(e) => return self.fail(CMD, &e.to_string()),
        };
        let data_dir = self.data_dir_of(&p);
        let steps = self.start_steps(&cwd, &data_dir, enforce, ask, !dry, no_ui)?;
        let width = steps.iter().map(|s| py_len(s.key)).max().unwrap_or(0);
        let mut b = String::new();
        let mut failed = false;
        for s in &steps {
            b.push_str(&format!(
                "  {}{}  {}{}  {}\n",
                s.key,
                " ".repeat(width - py_len(s.key)),
                s.state,
                " ".repeat(7usize.saturating_sub(py_len(s.state))),
                s.text
            ));
            failed |= s.state == "failed";
        }
        self.out(&b);
        if failed {
            return exit(1);
        }
        if dry || no_ui {
            return Ok(());
        }
        if !self.quiet {
            self.errf("opening the live view…\n");
        }
        // Python's start does not catch Ctrl-C: click ends the view with
        // "Aborted!" and exit 1.
        self.run_live(CMD, &data_dir, 2.0, true)
    }

    fn start_steps(
        &mut self,
        cwd: &str,
        data_dir: &str,
        enforce: bool,
        ask: bool,
        act: bool,
        no_ui: bool,
    ) -> Result<Vec<Step>, Stop> {
        const CMD: &str = "start";
        let view = step("view", if no_ui { "skipped" } else { "would" }, VIEW_TEXT);
        let root = join(data_dir, "gate");
        let mode = if enforce { "enforce" } else { "audit" };
        let settings = join(cwd, ".claude/settings.json");
        let key = match session_key(cwd) {
            Ok(k) => k,
            Err(e) => return Err(self.fail(CMD, &e.to_string()).unwrap_err()),
        };
        let mut out = vec![];
        let Some(claude) = install::look_path(&self.env, "claude") else {
            out.push(step(
                "harness",
                "failed",
                "Tried to find the agent harness. No `claude` on PATH. Install Claude Code, then run `daisugi start` again.",
            ));
            out.push(step("hook", "skipped", "no harness to hook into"));
            out.push(step("envelope", "skipped", "no harness session to register an envelope for"));
            out.push(step("gate-server", "skipped", "no harness session needs it yet"));
            out.push(view);
            return Ok(out);
        };
        out.push(step("harness", "done", format!("Claude Code found at {claude}")));
        const UNREADABLE: &str = "a Claude Code settings.json holds hooks this binary does not read yet";
        let global = join(&self.home, ".claude/settings.json");
        let mut global_note = String::new();
        if global != settings {
            let gm = match config::installed_hook_mode(&global) {
                Ok(m) => m,
                Err(_) => return Err(self.refuse(CMD, UNREADABLE).unwrap_err()),
            };
            if !gm.is_empty() {
                global_note = format!(
                    " (note: a machine-global gate hook is ALSO installed at {global}, mode: {gm} — \
                     `daisugi install --gate` put it there; it fires on every directory's sessions, including this one)"
                );
            }
        }
        let installed = match config::installed_hook_mode(&settings) {
            Ok(m) => m,
            Err(_) => return Err(self.refuse(CMD, UNREADABLE).unwrap_err()),
        };
        // Everything the run writes is checked before the first write.
        let mut edit: Option<install::Edit> = None;
        if installed.is_empty() && act {
            let me = self.self_path().map_err(|e| self.fail(CMD, &e).unwrap_err())?;
            let env = self.env.clone();
            let (me, _) = install::hook_path(&me, &self.env, &|tool: &str| install::mise_which(&env, tool));
            let entry = install::hook_entry(
                &me,
                &HookOptions { mode: mode.into(), root: root.clone(), format: "claude".into(), session: Some(key.clone()), ask },
            );
            match install::plan_claude_gate(&settings, &entry) {
                Ok(e) => edit = e,
                Err(e) => return Err(self.refuse(CMD, &e.to_string()).unwrap_err()),
            }
        }
        let env_path = join(&gateroot::envelopes_dir(&root), &format!("{key}.json"));
        let env_exists = std::fs::metadata(&env_path).is_ok();
        let mut starter = None;
        if !env_exists && act {
            let ws = gateroot::resolve(cwd).map_err(|e| self.fail(CMD, &e.to_string()).unwrap_err())?;
            match envelope::starter(&ws, false) {
                Ok(o) => starter = Some(o),
                Err(e) => return Err(self.fail(CMD, &e.to_string()).unwrap_err()),
            }
        }
        if !installed.is_empty() {
            let hint = if installed != mode {
                format!(
                    "; it's already {installed} — to switch to {mode}, remove the gate hook line from {settings} and run \
                     `daisugi start` again"
                )
            } else {
                String::new()
            };
            out.push(step(
                "hook",
                "skipped",
                format!(
                    "a gate hook is already installed in {settings} (mode: {installed}, this directory only){hint}{global_note}"
                ),
            ));
        } else if act {
            if let Err(e) = std::fs::create_dir_all(parent(&settings)) {
                return Err(self.pw_err(CMD, crate::pathways::PwErr::Io(e)));
            }
            if let Some(e) = &edit {
                if !e.warning.is_empty() {
                    self.errf(&format!("warning: {}\n", e.warning));
                }
                let res = install::apply(e);
                if let Ok(followed) = &res {
                    self.note_file(&settings, followed);
                }
                if let Err(err) = res {
                    return Err(self.pw_err(CMD, crate::pathways::PwErr::Io(err)));
                }
            }
            out.push(step(
                "hook",
                "done",
                format!(
                    "installed the gate hook in {mode} mode into {settings} (this directory only){global_note} — a \
                     project-settings hook needs Claude Code to trust this folder before it fires; confirm it did with \
                     `daisugi gate report`"
                ),
            ));
        } else {
            out.push(step(
                "hook",
                "would",
                format!("install the gate hook in {mode} mode into {settings} (this directory only){global_note}"),
            ));
        }
        if env_exists {
            out.push(step("envelope", "skipped", format!("an envelope for this directory is already registered at {env_path}")));
        } else if act {
            let env = starter.unwrap_or_default();
            let (_, followed, res) = envelope::register(&env, &key, &root);
            if let Err(e) = res {
                return Err(self.pw_err(CMD, crate::pathways::PwErr::Io(e)));
            }
            self.note_file(&env_path, &followed);
            out.push(step("envelope", "done", format!("registered a starter envelope for {cwd} at {env_path}")));
        } else {
            out.push(step("envelope", "would", format!("register a starter envelope for {cwd}")));
        }
        let sock = join(&root, "gate.sock");
        // A socket file is not a running gate: a server stopped by SIGKILL
        // leaves it behind. Only one that answers is.
        let state = probe(&sock);
        if state == Probe::Live {
            out.push(step("gate-server", "skipped", "the resident gate is already running"));
        } else if act {
            if state == Probe::Stale {
                let _ = std::fs::remove_file(&sock);
            }
            // start._detach: this binary's `gate serve`, in a session of
            // its own, with no terminal; then the socket must appear.
            self.spawn_gate_server(&root);
            let t0 = Instant::now();
            while !gateroot::exists(&sock) && t0.elapsed() < SERVER_WAIT {
                std::thread::sleep(Duration::from_millis(50));
            }
            if gateroot::exists(&sock) {
                out.push(step("gate-server", "done", format!("started the resident gate on {sock}")));
            } else {
                out.push(step(
                    "gate-server",
                    "failed",
                    format!(
                        "started `daisugi gate serve` but {sock} never appeared after {}s. Run `daisugi gate serve --root \
                         {root}` in a terminal to see why it exited.",
                        SERVER_WAIT.as_secs()
                    ),
                ));
            }
        } else {
            out.push(step("gate-server", "would", "start the resident gate (daisugi gate serve)"));
        }
        out.push(view);
        Ok(out)
    }

    /// Starts `<self> gate serve --root root` detached, with this
    /// process's environment and no stdin, stdout or stderr. A spawn that
    /// fails is seen as a socket that never appears.
    fn spawn_gate_server(&self, root: &str) {
        use std::os::unix::process::CommandExt;
        let Ok(me) = self.self_path() else { return };
        let mut c = Command::new(me);
        c.args(["gate", "serve", "--root", root])
            .env_clear()
            .envs(&self.env)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        // SAFETY: setsid() is async-signal-safe and touches no memory.
        unsafe {
            c.pre_exec(|| {
                libc::setsid();
                Ok(())
            });
        }
        let _ = c.spawn();
    }
}
