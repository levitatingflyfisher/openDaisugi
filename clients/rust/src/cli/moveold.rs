//! The one-time move after the rename (gate -> warden, gateway -> router). // names-ok
//! `install.move_old_state` in the Python oracle: the owner's state still
//! carries the old names; `daisugi install` moves each once and says one
//! line per move. Nothing is deleted. The old names below are read on
//! purpose; each such line carries a names-ok tag for the names check.

use super::install::{backup, dump_settings, pi_dir, opencode_path, read_json, ReadState};
use super::wardenroot;
use crate::warden::pyjson::Value;
use regex::Regex;
use std::collections::HashMap;
use std::fs;
use std::io::Write;
use std::sync::OnceLock;

/// A move that stops the run: exit 2, one line said.
#[derive(Debug)]
#[allow(dead_code)]
pub struct MoveRefused(pub String);

use super::config::RENAMED_KEYS as RENAMED_CONFIG_KEYS;

fn old_hook_rewrites() -> &'static Vec<(Regex, &'static str)> {
    static R: OnceLock<Vec<(Regex, &'static str)>> = OnceLock::new();
    R.get_or_init(|| {
        vec![
            (Regex::new(r"\bDAISUGI_GATE_").unwrap(), "DAISUGI_WARDEN_"), // names-ok: old env prefix
            (Regex::new(r"\bOPENDAISUGI_GATE_").unwrap(), "OPENDAISUGI_WARDEN_"), // names-ok: old env prefix
            (Regex::new(r"opendaisugi\.gate_client").unwrap(), "opendaisugi.warden_client"), // names-ok: old module
            (Regex::new(r"opendaisugi\.gate([^A-Za-z]|$)").unwrap(), "opendaisugi.warden$1"), // names-ok: old module
            (Regex::new(r"(daisugi) gate (check)").unwrap(), "$1 warden $2"), // names-ok: old verb
            (Regex::new(r"(\s)gate (check)").unwrap(), "${1}warden $2"), // names-ok: old verb
            (Regex::new(r"--gate-root").unwrap(), "--warden-root"), // names-ok: old flag
            (Regex::new(r"\bgate\.sock\b").unwrap(), "warden.sock"), // names-ok: old socket name
        ]
    })
}

/// `_new_hook_command`: an old-form warden hook in today's words, or None
/// when it is not one of ours. The mode stays.
fn new_hook_command(command: &str, data_dir: &str) -> Option<String> {
    let mut n = command.to_string();
    for (re, rep) in old_hook_rewrites() {
        n = re.replace_all(&n, *rep).into_owned();
    }
    // The hook reads the warden dir by path and install moves that dir, so
    // the path moves with it (plain, single or double quoted).
    let (old, new) = (format!("{data_dir}/gate"), format!("{data_dir}/warden")); // names-ok: old dir
    for q in ["", "'", "\""] {
        let re = Regex::new(&format!(r"(--root\s+){q}{}{q}(\s|$)", regex::escape(&old))).unwrap();
        n = re.replace_all(&n, |c: &regex::Captures| format!("{}{q}{new}{q}{}", &c[1], &c[2])).into_owned();
    }
    if n == command {
        return None;
    }
    super::words::warden_hook_args(&Value::Str(n.clone()))?;
    Some(n)
}

/// `_rewrite_old_hooks`: each old-form warden hook command in the file, in
/// place. True when a command changed.
fn rewrite_old_hooks(path: &str, data_dir: &str, dry_run: bool) -> bool {
    let Ok((mut v, ReadState::Ok)) = read_json(path) else { return false };
    let mut changed = false;
    if let Value::Obj(top) = &mut v {
        if let Some(Value::Obj(hooks)) = top.get_mut("hooks") {
            if let Some(Value::List(pre)) = hooks.get_mut("PreToolUse") {
                for e in pre.iter_mut() {
                    let Value::Obj(entry) = e else { continue };
                    let Some(Value::List(inner)) = entry.get_mut("hooks") else { continue };
                    for h in inner.iter_mut() {
                        let Value::Obj(hook) = h else { continue };
                        let Some(Value::Str(cmd)) = hook.get("command") else { continue };
                        if let Some(n) = new_hook_command(&cmd.clone(), data_dir) {
                            hook.set("command", Value::Str(n));
                            changed = true;
                        }
                    }
                }
            }
        }
    }
    if changed && !dry_run && backup(path).is_ok() {
        let _ = fs::write(path, dump_settings(&v));
    }
    changed
}

fn socket_listens(sock: &str) -> bool {
    let sock = sock.to_string();
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(std::os::unix::net::UnixStream::connect(&sock).is_ok());
    });
    rx.recv_timeout(std::time::Duration::from_millis(200)).unwrap_or(false)
}

fn is_dir(p: &str) -> bool {
    fs::symlink_metadata(p).map(|m| m.is_dir()).unwrap_or(false)
}

/// `UTC` stamp `YYYYMMDDTHHMMSSZ` for `secs` since the epoch.
fn stamp(secs: i64) -> String {
    let (days, rem) = (secs.div_euclid(86_400), secs.rem_euclid(86_400));
    let (y, m, d) = crate::spawn::civil_from_days(days);
    format!("{y:04}{m:02}{d:02}T{:02}{:02}{:02}Z", rem / 3600, rem % 3600 / 60, rem % 60)
}

/// `install.move_old_state`. The lines it says go to `out` and into the
/// result. A refusal returns `MoveRefused` after its line.
pub fn move_old_state(
    data_dir: &str,
    home: &str,
    env: &HashMap<String, String>,
    now_secs: i64,
    out: &mut dyn Write,
    dry_run: bool,
) -> Result<Vec<String>, MoveRefused> {
    let mut lines: Vec<String> = vec![];
    let mut say = |lines: &mut Vec<String>, text: String| {
        let _ = writeln!(out, "{text}");
        lines.push(text);
    };
    let verb = if dry_run { "would move" } else { "moved" };

    let old_w = format!("{data_dir}/gate"); // names-ok: old dir
    let new_w = format!("{data_dir}/warden");
    if is_dir(&old_w) {
        if wardenroot::lexists(&new_w) {
            let t = format!("both {old_w} and {new_w} exist; move one aside by hand, then run install again");
            say(&mut lines, t.clone());
            return Err(MoveRefused(t));
        }
        let sock = format!("{old_w}/gate.sock"); // names-ok: old socket
        if socket_listens(&sock) {
            let t = format!(
                "a gate server still listens on {sock}; stop it (the pid is in {old_w}/gate.lock) and run install again" // names-ok: old words
            );
            say(&mut lines, t.clone());
            return Err(MoveRefused(t));
        }
        say(&mut lines, format!("{verb} {old_w} to {new_w}"));
        if !dry_run {
            let _ = fs::rename(&old_w, &new_w);
        }
    }

    let old_r = format!("{data_dir}/gateway"); // names-ok: old dir
    let new_r = format!("{data_dir}/router");
    if is_dir(&old_r) {
        if !wardenroot::lexists(&new_r) {
            say(&mut lines, format!("{verb} {old_r} to {new_r}"));
            if !dry_run {
                let _ = fs::rename(&old_r, &new_r);
            }
        } else {
            // router/ already holds the delegation and label journals. The
            // old files join them, one by one; a name in both stops the move.
            let mut names: Vec<String> = fs::read_dir(&old_r)
                .map(|d| d.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect())
                .unwrap_or_default();
            names.sort();
            let clash: Vec<&String> = names.iter().filter(|n| wardenroot::lexists(&format!("{new_r}/{n}"))).collect();
            if !clash.is_empty() {
                let t = format!(
                    "both {old_r} and {new_r} hold {}; move one aside by hand, then run install again",
                    clash.iter().map(|s| s.as_str()).collect::<Vec<_>>().join(", ")
                );
                say(&mut lines, t.clone());
                return Err(MoveRefused(t));
            }
            say(&mut lines, format!("{verb} {old_r} into {new_r}"));
            if !dry_run {
                for n in &names {
                    let _ = fs::rename(format!("{old_r}/{n}"), format!("{new_r}/{n}"));
                }
                let _ = fs::remove_dir(&old_r);
            }
        }
    }

    let cfg = format!("{data_dir}/config.yaml");
    if let Ok(text) = fs::read_to_string(&cfg) {
        let mut n = text.clone();
        for (old, new) in RENAMED_CONFIG_KEYS {
            let re = Regex::new(&format!(r"(?m)^(\s*){}(\s*:)", regex::escape(old))).unwrap();
            if re.is_match(&n) {
                n = re.replace_all(&n, format!("${{1}}{new}${{2}}").as_str()).into_owned();
                say(&mut lines, format!("config: {old} is now {new}"));
            }
        }
        if n != text && !dry_run {
            let _ = fs::write(&cfg, n);
        }
    }

    for f in [format!("{home}/.claude/settings.json"), format!("{home}/.codex/hooks.json")] {
        if fs::metadata(&f).map(|m| m.is_file()).unwrap_or(false) && rewrite_old_hooks(&f, data_dir, dry_run) {
            say(&mut lines, format!("replaced the old warden hook in {f}"));
        }
    }

    let aside = format!("{data_dir}/moved-aside/{}", stamp(now_secs));
    let pi_old = format!("{}/daisugi-gate", parent_of(&pi_dir(home))); // names-ok: old extension dir
    let oc_new = opencode_path(home, env);
    let oc_old = format!("{}/daisugi-gate.ts", parent_of(&oc_new)); // names-ok: old plugin file
    for (old, name) in [(pi_old, "pi-daisugi-gate"), (oc_old, "opencode-daisugi-gate.ts")] { // names-ok: old names
        if !wardenroot::lexists(&old) {
            continue;
        }
        let dest = format!("{aside}/{name}");
        say(&mut lines, format!("{verb} aside {old} to {dest}"));
        if !dry_run {
            let _ = fs::create_dir_all(parent_of(&dest));
            let _ = fs::rename(&old, &dest);
        }
    }

    // The delegate tool is now spawn. An envelope that allows the old tool
    // name is the owner's to edit; install edits no envelope.
    let root = if wardenroot::lexists(&new_w) { new_w } else { old_w };
    let mut envs: Vec<String> = fs::read_dir(format!("{root}/envelopes"))
        .map(|d| d.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect())
        .unwrap_or_default();
    envs.sort();
    for f in envs.iter().filter(|f| f.ends_with(".json")) {
        let Ok(raw) = fs::read_to_string(format!("{root}/envelopes/{f}")) else { continue };
        let Ok(Value::Obj(doc)) = crate::warden::pyjson::loads(&raw) else { continue };
        let Some(Value::Obj(perm)) = doc.get("permissions") else { continue };
        let Some(Value::List(allow)) = perm.get("mcp_allowlist") else { continue };
        let has = |s: &str| allow.iter().any(|a| matches!(a, Value::Str(x) if x == s));
        if has("opendaisugi/delegate") && !has("opendaisugi/spawn") { // names-ok: old tool
            say(
                &mut lines,
                format!(
                    "envelope {} allows mcp opendaisugi/delegate; add opendaisugi/spawn (the tool was renamed)", // names-ok: old tool
                    f.trim_end_matches(".json")
                ),
            );
        }
    }
    Ok(lines)
}

fn parent_of(p: &str) -> String {
    match p.rfind('/') {
        Some(0) => "/".into(),
        Some(i) => p[..i].to_string(),
        None => ".".into(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> String {
        let d = std::env::temp_dir().join(format!("moveold-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&d);
        fs::create_dir_all(&d).unwrap();
        d.to_string_lossy().into_owned()
    }

    fn run(dir: &str, home: &str, dry: bool) -> Result<Vec<String>, MoveRefused> {
        let mut sink = Vec::new();
        move_old_state(dir, home, &HashMap::new(), 1_793_361_600, &mut sink, dry)
    }

    #[test]
    fn move_old_state_moves_dirs() {
        let d = scratch("dirs");
        fs::create_dir_all(format!("{d}/gate/envelopes")).unwrap(); // names-ok
        fs::create_dir_all(format!("{d}/gateway")).unwrap(); // names-ok
        let lines = run(&d, &format!("{d}/h"), false).unwrap();
        assert_eq!(lines, vec![format!("moved {d}/gate to {d}/warden"), format!("moved {d}/gateway to {d}/router")]); // names-ok
        assert!(fs::metadata(format!("{d}/warden/envelopes")).unwrap().is_dir());
        assert!(!wardenroot::lexists(&format!("{d}/gate"))); // names-ok
    }

    #[test]
    fn gateway_files_join_an_existing_router_dir() { // names-ok
        let d = scratch("join");
        fs::create_dir_all(format!("{d}/gateway")).unwrap(); // names-ok
        fs::write(format!("{d}/gateway/turns.jsonl"), "{}\n").unwrap(); // names-ok
        fs::create_dir_all(format!("{d}/router")).unwrap();
        fs::write(format!("{d}/router/delegations.jsonl"), "{}\n").unwrap();
        let lines = run(&d, &d, false).unwrap();
        assert_eq!(lines, vec![format!("moved {d}/gateway into {d}/router")]); // names-ok
        assert!(wardenroot::lexists(&format!("{d}/router/turns.jsonl")));
        assert!(!wardenroot::lexists(&format!("{d}/gateway"))); // names-ok
    }

    #[test]
    fn refuses_both() {
        let d = scratch("both");
        fs::create_dir_all(format!("{d}/gate")).unwrap(); // names-ok
        fs::create_dir_all(format!("{d}/warden")).unwrap();
        assert!(run(&d, &d, false).is_err());
        assert!(wardenroot::lexists(&format!("{d}/gate"))); // names-ok
    }

    #[test]
    fn refuses_live_socket() {
        let d = scratch("sock");
        fs::create_dir_all(format!("{d}/gate")).unwrap(); // names-ok
        let _l = std::os::unix::net::UnixListener::bind(format!("{d}/gate/gate.sock")).unwrap(); // names-ok
        let err = run(&d, &d, false).unwrap_err();
        assert!(err.0.contains("still listens"));
        assert!(wardenroot::lexists(&format!("{d}/gate"))); // names-ok
        assert!(!wardenroot::lexists(&format!("{d}/warden")));
    }

    #[test]
    fn rewrites_config_keys() {
        let d = scratch("cfg");
        fs::write(format!("{d}/config.yaml"), "gate_mode: enforce\ngateway_router: knn\ngateway_router_model: m\n").unwrap(); // names-ok
        let lines = run(&d, &d, false).unwrap();
        assert_eq!(fs::read_to_string(format!("{d}/config.yaml")).unwrap(), "warden_mode: enforce\nrouting_mode: knn\nrouting_model: m\n");
        assert!(lines.contains(&"config: gate_mode is now warden_mode".to_string())); // names-ok
        assert!(lines.contains(&"config: gateway_router is now routing_mode".to_string())); // names-ok
    }

    #[test]
    fn replaces_old_marker_hook() {
        let d = scratch("hook");
        fs::create_dir_all(format!("{d}/.claude")).unwrap();
        let old = "DAISUGI_GATE_HOOK=opendaisugi.gate python -I -m opendaisugi.gate_client check --mode enforce --root /x/gate"; // names-ok
        let body = format!("{{\"hooks\": {{\"PreToolUse\": [{{\"matcher\": \"*\", \"hooks\": [{{\"type\": \"command\", \"command\": \"{old}\"}}]}}]}}}}");
        fs::write(format!("{d}/.claude/settings.json"), body).unwrap();
        let lines = run(&format!("{d}/data"), &d, false).unwrap();
        assert_eq!(lines, vec![format!("replaced the old warden hook in {d}/.claude/settings.json")]);
        let text = fs::read_to_string(format!("{d}/.claude/settings.json")).unwrap();
        assert!(text.contains("DAISUGI_WARDEN_HOOK=opendaisugi.warden"));
        assert!(text.contains("--mode enforce"));
        assert!(!text.contains("DAISUGI_GATE")); // names-ok
    }

    #[test]
    fn the_old_hook_root_follows_the_moved_dir() {
        for quote in ["", "'", "\""] {
            let d = scratch("root");
            let data = format!("{d}/data");
            fs::create_dir_all(format!("{data}/gate/envelopes")).unwrap(); // names-ok
            fs::create_dir_all(format!("{d}/.claude")).unwrap();
            let old = format!(
                "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/bin/daisugi gate check --mode enforce --root {quote}{data}/gate{quote} --format claude || exit 2" // names-ok
            )
            .replace('"', "\\\"");
            let body = format!("{{\"hooks\": {{\"PreToolUse\": [{{\"matcher\": \"*\", \"hooks\": [{{\"type\": \"command\", \"command\": \"{old}\"}}]}}]}}}}");
            fs::write(format!("{d}/.claude/settings.json"), body).unwrap();
            run(&data, &d, false).unwrap();
            let text = fs::read_to_string(format!("{d}/.claude/settings.json")).unwrap();
            assert!(text.contains(&format!("--root {quote_e}{data}/warden{quote_e} ", quote_e = quote.replace('"', "\\\""))), "{quote}: {text}");
            assert!(text.contains("--mode enforce"));
            assert!(fs::metadata(format!("{data}/warden/envelopes")).is_ok());
            assert!(fs::metadata(format!("{data}/gate")).is_err()); // names-ok
            // A second pass moves nothing and refuses nothing.
            assert!(run(&data, &d, false).unwrap().is_empty());
        }
    }

    #[test]
    fn moves_old_extensions_aside() {
        let d = scratch("ext");
        let ext = format!("{d}/.pi/agent/extensions/daisugi-gate"); // names-ok
        fs::create_dir_all(&ext).unwrap();
        fs::write(format!("{ext}/index.ts"), "// old").unwrap();
        let lines = run(&format!("{d}/data"), &d, false).unwrap();
        let moved = format!("{d}/data/moved-aside/20261030T120000Z/pi-daisugi-gate"); // names-ok
        assert_eq!(lines, vec![format!("moved aside {ext} to {moved}")]);
        assert_eq!(fs::read_to_string(format!("{moved}/index.ts")).unwrap(), "// old");
        assert!(!wardenroot::lexists(&ext));
    }

    #[test]
    fn names_a_stale_delegate_allow() {
        let d = scratch("allow");
        fs::create_dir_all(format!("{d}/warden/envelopes")).unwrap();
        fs::write(format!("{d}/warden/envelopes/e1.json"), "{\"permissions\": {\"mcp_allowlist\": [\"opendaisugi/delegate\"]}}").unwrap(); // names-ok
        let lines = run(&d, &d, false).unwrap();
        assert_eq!(lines, vec!["envelope e1 allows mcp opendaisugi/delegate; add opendaisugi/spawn (the tool was renamed)".to_string()]); // names-ok
    }
}
