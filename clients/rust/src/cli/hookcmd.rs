//! `daisugi hook record`: read a hook payload from stdin, record it, and
//! print the host's continue contract. It never blocks the host: past the
//! options, nothing it reads or cannot write changes the contract or the
//! exit code.

use std::collections::HashMap;
use std::io::Read;
use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};

use super::gardencmd::{now_seconds, read_stamp};
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::pyjson::float_repr;

const EVENTS: &[&str] = &[
    "pre_tool_use",
    "stop",
    "notification",
    "subagent_start",
    "subagent_stop",
];

/// `hook._spawn_detached_auto_tend`: `daisugi hook auto-tend --data-dir
/// <dir>` in a session of its own, its streams on /dev/null, never waited
/// on.
fn spawn_auto_tend(env: &HashMap<String, String>, data_dir: &str) {
    let Some(bin) = super::install::look_path(env, "daisugi") else {
        return;
    };
    let mut cmd = Command::new(bin);
    cmd.args(["hook", "auto-tend", "--data-dir", data_dir])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    // SAFETY: setsid is async-signal-safe and touches no memory of ours.
    unsafe {
        cmd.pre_exec(|| {
            libc::setsid();
            Ok(())
        });
    }
    if let Ok(child) = cmd.spawn() {
        drop(child);
    }
}

impl Env {
    pub(super) fn hook_record(&mut self, args: &[String]) -> Res {
        const CMD: &str = "hook record";
        let opts = [
            Opt::val(&["--captures-root"], "PATH", ""),
            Opt::val(&["--format"], "TEXT", "Host runtime stdout contract: claude | codex | hermes | openclaw."),
            Opt::val(
                &["--event"],
                "TEXT",
                "Which host hook this is wired to: pre_tool_use (default, records the tool call) | stop (session \
                 went idle) | notification (a permission prompt or an idle-prompt notification) | subagent_start | \
                 subagent_stop, which report a subagent row under the pane in coppice.",
            ),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Read a hook payload from stdin, record it, return the host's continue contract.",
                &opts,
            );
        }
        let root = p.str(
            "--captures-root",
            &format!("{}/.opendaisugi/captures", self.home),
        );
        let fmt = p.str("--format", "claude");
        let event = p.str("--event", "pre_tool_use");
        if !EVENTS.contains(&event.as_str()) {
            self.errf(
                "Error: --event must be one of pre_tool_use, stop, notification, subagent_start, subagent_stop.\n",
            );
            return exit(1);
        }
        let mut raw = Vec::new();
        if std::io::stdin().lock().read_to_end(&mut raw).is_err() {
            raw.clear();
        }
        let contract = crate::gate::hookrecord::hook_record(&raw, &root, &fmt, &event, &self.env);
        self.out(&format!("{contract}\n"));
        if event == "pre_tool_use" {
            let data_dir = crate::gate::paths::path_parent(&crate::gate::paths::path_str(&root));
            let env = self.env.clone();
            let _ = std::panic::catch_unwind(|| {
                maybe_trigger_tend(&env, &data_dir, now_seconds(), spawn_auto_tend)
            });
        }
        Ok(())
    }
}

/// `hook.maybe_trigger_background_tend`: at most once per 1800 s, and only
/// with the user's consent, start a detached auto-tend. Any error ends it
/// quietly with no spawn.
fn maybe_trigger_tend(
    env: &HashMap<String, String>,
    data_dir: &str,
    now: f64,
    spawn: fn(&HashMap<String, String>, &str),
) {
    const MIN_INTERVAL: f64 = 1800.0;
    let stamp = format!("{data_dir}/.hook-record-tend-trigger");
    let Ok(last) = read_stamp(&stamp) else { return };
    if now - last < MIN_INTERVAL {
        return;
    }
    // A config this binary cannot read counts as no consent.
    match super::config::load(&format!("{data_dir}/config.yaml")) {
        Ok(c) if c.auto_tend == Some(true) => {}
        _ => return,
    }
    if std::fs::create_dir_all(data_dir).is_err() {
        return;
    }
    if std::fs::write(&stamp, float_repr(now)).is_err() {
        return;
    }
    spawn(env, data_dir);
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    static SPAWNED: Mutex<Vec<String>> = Mutex::new(Vec::new());

    fn record_spawn(_: &HashMap<String, String>, d: &str) {
        SPAWNED.lock().unwrap().push(d.to_string());
    }

    #[test]
    fn the_trigger_spawns_once_with_consent_and_never_without() {
        let d = std::env::temp_dir().join(format!("daisugi-trigger-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        let dd = d.to_str().unwrap().to_string();
        let env = HashMap::new();
        maybe_trigger_tend(&env, &dd, 1e9, record_spawn);
        assert!(SPAWNED.lock().unwrap().is_empty());
        std::fs::write(format!("{dd}/config.yaml"), "auto_tend: true\n").unwrap();
        maybe_trigger_tend(&env, &dd, 1e9, record_spawn);
        maybe_trigger_tend(&env, &dd, 1e9 + 60.0, record_spawn);
        assert_eq!(*SPAWNED.lock().unwrap(), vec![dd.clone()]);
        assert_eq!(
            std::fs::read_to_string(format!("{dd}/.hook-record-tend-trigger")).unwrap(),
            "1000000000.0"
        );
    }
}
