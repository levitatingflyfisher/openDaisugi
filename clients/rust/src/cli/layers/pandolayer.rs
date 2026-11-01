//! The pando layer (round 7): Pando R2 is there in every harness with no
//! setup step. It registers `daisugi pando serve` as the MCP server "pando",
//! puts the memory view into the context of a new chat, logs a Claude Code
//! chat as it is said, and runs `daisugi pando follow` as a user service so
//! Hermes chats and background tends keep going. It is on by default; an
//! `--only` list leaves it out unless it names the pando layer. The Go
//! client's `install/pandolayer.go` is the twin.

use std::collections::HashSet;
use std::sync::{Arc, Mutex};

use super::*;

/// The pando layer's name.
pub const LAYER_PANDO: &str = "pando";
/// The MCP server name the layer registers.
pub const PANDO_MCP_NAME: &str = "pando";
/// The systemd user unit's file name.
pub const PANDO_UNIT_NAME: &str = "daisugi-pando.service";
/// What every hook command of the layer ends with.
const PANDO_HOOK_MARKER: &str = " pando hook";
/// The first line of a unit this layer wrote. A unit file without it is the
/// owner's own.
const PANDO_UNIT_MARK: &str = "# Managed by daisugi install.";
/// The note install prints for the unit.
pub const PANDO_UNIT_NOTE: &str = "Pando follow service file written. If systemd does not run it for you, start it yourself: \
                                   systemctl --user enable --now daisugi-pando.service";

impl Opts {
    /// Whether the pando layer is selected: a default install, or `--only`
    /// that names it.
    pub fn has_pando(&self) -> bool {
        self.has(LAYER_PANDO)
    }
}

fn pando_mcp_entry(with_type: bool, self_path: &str) -> Object {
    let mut o = Object::new();
    if with_type {
        o.set("type", "stdio");
    }
    o.set("command", self_path);
    o.set("args", Value::List(vec![Value::Str("pando".into()), Value::Str("serve".into())]));
    o
}

fn pando_hook_command(self_path: &str) -> String {
    format!("{}{PANDO_HOOK_MARKER}", shlex_quote(self_path))
}

pub fn is_pando_hook(c: &Value) -> bool {
    matches!(c, Value::Str(s) if s.trim().ends_with(PANDO_HOOK_MARKER))
}

/// The Claude Code events the layer hooks. SessionEnd catches the last
/// reply: the transcript may lag the Stop hook.
pub const PANDO_HOOK_EVENTS: [&str; 4] = ["SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"];

/// Whether an event's hook runs in the background. Only SessionStart prints
/// context, so only it must be waited for. SessionEnd is waited for too: it
/// is the end of the chat and holds nothing up, and Claude Code kills a
/// background hook at exit in -p mode, so the last reply would be lost. Its
/// own timeout (10 s) is the wait.
fn pando_hook_async(ev: &str) -> bool {
    ev != "SessionStart" && ev != "SessionEnd"
}

fn pando_hook_entry(ev: &str, self_path: &str) -> Object {
    let mut h = Object::new();
    h.set("type", "command");
    h.set("command", pando_hook_command(self_path).as_str());
    h.set("timeout", Value::Int("10".into()));
    if pando_hook_async(ev) {
        h.set("async", true);
    }
    let mut e = Object::new();
    e.set("hooks", Value::List(vec![Value::Obj(h)]));
    e
}

/// Brings an existing pando hook to the shape the layer wants.
fn pando_hook_fresh(ev: &str, self_path: &str, ho: &mut Object) -> bool {
    let mut changed = false;
    let want = Value::Str(pando_hook_command(self_path));
    if *ho.value("command") != want && !keeps_path(ho.value("command"), &want) {
        ho.set("command", want);
        changed = true;
    }
    let is_async = matches!(ho.value("async"), Value::Bool(true));
    if is_async != pando_hook_async(ev) {
        if pando_hook_async(ev) {
            ho.set("async", true);
        } else {
            ho.remove("async");
        }
        changed = true;
    }
    changed
}

/// Adds the pando hook to SessionStart, UserPromptSubmit, Stop and
/// SessionEnd of a Claude Code settings.json, unless it is there.
pub fn plan_pando_claude_hooks(p: &str, prior: Option<&Edit>, self_path: &str) -> R<Option<Edit>> {
    let (mut s, existed) = match settings_of(
        p,
        prior,
        format!(
            "{p} is not valid JSON; skipping the pando hooks to avoid overwriting your Claude Code settings. Fix the \
             file and re-run `daisugi install`."
        ),
    )? {
        Ok(x) => x,
        Err(w) => return Ok(Some(w)),
    };
    let mut changed = false;
    for ev in PANDO_HOOK_EVENTS {
        let mut list = event_list(&mut s, ev)?;
        let mut have = false;
        for e in list.iter_mut() {
            let Value::Obj(eo) = e else { return Err(InstErr::Unsupported) };
            if let Some(Value::List(hs)) = eo.get_mut("hooks") {
                for h in hs.iter_mut() {
                    if let Value::Obj(ho) = h {
                        if is_pando_hook(ho.value("command")) {
                            have = true;
                            if pando_hook_fresh(ev, self_path, ho) {
                                changed = true;
                            }
                        }
                    }
                }
            }
        }
        if !have {
            list.push(Value::Obj(pando_hook_entry(ev, self_path)));
            changed = true;
        }
        set_event(&mut s, ev, list);
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed, content: dump_settings(&Value::Obj(s)), ..Edit::default() }))
}

const PANDO_TOML_HEADER: &str = "[mcp_servers.pando]";

/// The layer's block in a config.toml as a range of lines: the header line
/// and the line after the block. A header counts only when it is a whole
/// line (a comment that holds the text does not). The block runs to the next
/// table header that is not one of its own sub-tables.
fn pando_block_lines(lines: &[&str]) -> Option<(usize, usize)> {
    let start = lines.iter().position(|l| {
        let t = l.trim();
        t == PANDO_TOML_HEADER || t.starts_with(&format!("{PANDO_TOML_HEADER} #")) || t.starts_with(&format!("{PANDO_TOML_HEADER}#"))
    })?;
    let mut end = lines.len();
    for (i, l) in lines.iter().enumerate().skip(start + 1) {
        let t = l.trim();
        if t.starts_with('[') && !t.starts_with("[mcp_servers.pando.") {
            end = i;
            break;
        }
    }
    Some((start, end))
}

/// Adds `[mcp_servers.pando]` to a Codex config.toml.
pub fn plan_pando_codex_mcp(p: &str, self_path: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (text, existed) = text_of(p, prior)?;
    let lines: Vec<&str> = text.split('\n').collect();
    if pando_block_lines(&lines).is_some() {
        return Ok(None);
    }
    let block = format!(
        "\n{PANDO_TOML_HEADER}\ncommand = {}\nargs = [\"pando\", \"serve\"]\n",
        dumps(&Value::Str(self_path.into()), true)
    );
    let sep = if text.is_empty() { "" } else { "\n" };
    Ok(Some(Edit { path: p.into(), existed, content: format!("{}{sep}{block}", text.trim_end_matches('\n')), ..Edit::default() }))
}

/// Takes the block out again.
pub fn plan_pando_codex_mcp_remove(p: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (text, existed) = text_of(p, prior).map_err(|_| InstErr::Unsupported)?;
    let lines: Vec<&str> = text.split('\n').collect();
    let Some((start, end)) = pando_block_lines(&lines) else { return Ok(None) };
    if !existed {
        return Ok(None);
    }
    let kept: Vec<&str> = lines[..start].iter().chain(lines[end..].iter()).copied().collect();
    let mut cleaned = kept.join("\n").trim_end_matches('\n').to_string();
    if !cleaned.is_empty() {
        cleaned.push('\n');
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: cleaned, ..Edit::default() }))
}

/// The Hermes dirs the layer writes into: the root (HERMES_HOME, else
/// ~/.hermes) and each profile dir that has a config.yaml or a state.db.
pub fn hermes_homes(home: &str) -> Vec<String> {
    let root = crate::hermesdb::root(&std::env::var("HERMES_HOME").unwrap_or_default(), home).to_string_lossy().into_owned();
    let mut out = vec![root.clone()];
    let mut names: Vec<String> = fs::read_dir(join(&root, "profiles"))
        .map(|rd| rd.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect())
        .unwrap_or_default();
    // read_dir order of Go's os.ReadDir is by name.
    names.sort();
    for n in names {
        let d = join(&join(&root, "profiles"), &n);
        if !wardenroot::is_dir(&d) {
            continue;
        }
        if ["config.yaml", "state.db"].iter().any(|f| fs::symlink_metadata(join(&d, f)).is_ok()) {
            out.push(d);
        }
    }
    out
}

/// Sets the pando MCP server and enables the pando plugin in a Hermes
/// config.yaml.
pub fn plan_pando_hermes_config(p: &str, self_path: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let existed = prior.is_some() || wardenroot::lexists(p);
    let mut cfg = match yaml_of_prior(p, prior)? {
        None => Object::new(),
        Some(Some(o)) => o,
        Some(None) => {
            return Ok(Some(Edit {
                path: p.into(),
                warning: format!(
                    "{p} is not valid YAML; skipping pando to avoid overwriting your Hermes config. Fix the file and \
                     re-run `daisugi install`."
                ),
                ..Edit::default()
            }))
        }
    };
    let mut changed = false;
    if cfg.get("mcp_servers").is_none() {
        cfg.set("mcp_servers", Object::new());
    }
    let Some(Value::Obj(mcp)) = cfg.get_mut("mcp_servers") else { return Err(InstErr::Unsupported) };
    if mcp.get(PANDO_MCP_NAME).is_none() {
        mcp.set(PANDO_MCP_NAME, pando_mcp_entry(false, self_path));
        changed = true;
    }
    if cfg.get("plugins").is_none() {
        cfg.set("plugins", Object::new());
    }
    let Some(Value::Obj(plugins)) = cfg.get_mut("plugins") else { return Err(InstErr::Unsupported) };
    let mut list = match plugins.get("enabled") {
        None | Some(Value::Null) => vec![],
        Some(Value::List(l)) => l.clone(),
        Some(_) => return Err(InstErr::Unsupported),
    };
    if !list.iter().any(|x| *x == Value::Str("pando".into())) {
        list.push(Value::Str("pando".into()));
        plugins.set("enabled", Value::List(list));
        changed = true;
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed, content: yaml_text(&cfg)?, ..Edit::default() }))
}

/// Takes the MCP server and the plugin entry out.
pub fn plan_pando_hermes_reverse(p: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let Some(Some(mut cfg)) = yaml_of_prior(p, prior)? else { return Ok(None) };
    let mut changed = false;
    let mut drop_servers = false;
    if let Some(Value::Obj(servers)) = cfg.get_mut("mcp_servers") {
        if servers.get(PANDO_MCP_NAME).is_some() {
            servers.remove(PANDO_MCP_NAME);
            drop_servers = servers.is_empty();
            changed = true;
        }
    }
    if drop_servers {
        cfg.remove("mcp_servers");
    }
    let mut drop_plugins = false;
    if let Some(Value::Obj(plugins)) = cfg.get_mut("plugins") {
        if let Some(Value::List(list)) = plugins.get("enabled").cloned() {
            let kept: Vec<Value> = list.iter().filter(|x| **x != Value::Str("pando".into())).cloned().collect();
            if kept.len() != list.len() {
                changed = true;
                if kept.is_empty() {
                    plugins.remove("enabled");
                } else {
                    plugins.set("enabled", Value::List(kept));
                }
                drop_plugins = plugins.is_empty();
            }
        }
    }
    if drop_plugins {
        cfg.remove("plugins");
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: yaml_text(&cfg)?, ..Edit::default() }))
}

/// The manifest of the Hermes plugin.
const PANDO_PLUGIN_YAML: &str = "name: pando
version: \"1.0\"
description: Puts the Pando memory view into the context of every turn. Managed by daisugi install.
provides_hooks:
  - pre_llm_call
";

/// The plugin. The program path is written at install.
const PANDO_PLUGIN_PY: &str = "# Managed by daisugi install. Puts the Pando memory view into the context of
# every turn: Hermes appends what pre_llm_call returns to the user message,
# and does not keep it, so a later turn needs it again. The text is the same
# bytes until the memory changes. It reads the memory through the daisugi
# program and never fails a turn. A process marked DAISUGI_PANDO_SKIP gets
# nothing (the program prints nothing then). The session id goes to the
# program too: a long-running agent's chat (DAISUGI_PANDO_BRANCH) is then
# logged into its branch of the memory, never into the store above it.
import subprocess

DAISUGI = %s


def _view(session_id=None, is_first_turn=False, **kwargs):
    argv = [DAISUGI, \"pando\", \"context\"]
    if session_id:
        argv += [\"--session\", str(session_id)]
    try:
        out = subprocess.run(argv, capture_output=True, text=True, timeout=10)
    except Exception:
        return None
    text = (out.stdout or \"\").strip()
    if out.returncode != 0 or not text:
        return None
    return {\"context\": text}


def register(ctx):
    ctx.register_hook(\"pre_llm_call\", _view)
";

fn pando_plugin_files(self_path: &str) -> Vec<(&'static str, String)> {
    vec![
        ("plugin.yaml", PANDO_PLUGIN_YAML.to_string()),
        ("__init__.py", PANDO_PLUGIN_PY.replacen("%s", &dumps(&Value::Str(self_path.into()), true), 1)),
    ]
}

/// Writes the plugin into dir/plugins/pando, unless it is there as wanted.
pub fn plan_pando_hermes_plugin(dir: &str, self_path: &str) -> Option<Edit> {
    let target = join(&join(dir, "plugins"), "pando");
    let current = pando_plugin_files(self_path)
        .iter()
        .all(|(rel, want)| fs::read_to_string(join(&target, rel)).is_ok_and(|b| b == *want));
    if current {
        return None;
    }
    Some(Edit { path: target, kind: "pando-plugin".into(), content: self_path.into(), listed: true, ..Edit::default() })
}

/// Where the user service file goes.
pub fn pando_unit_path(home: &str) -> String {
    join(home, &format!(".config/systemd/user/{PANDO_UNIT_NAME}"))
}

fn pando_unit(self_path: &str) -> String {
    format!(
        "{PANDO_UNIT_MARK}\n[Unit]\nDescription=openDaisugi Pando memory follower\n\n[Service]\nExecStart={} pando follow\n\
         Restart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n",
        shlex_quote(self_path)
    )
}

/// Writes the user service file.
pub fn plan_pando_unit(home: &str, self_path: &str) -> R<Option<Edit>> {
    let p = pando_unit_path(home);
    let (text, existed) = read_text(&p)?;
    let want = pando_unit(self_path);
    if text == want {
        return Ok(None);
    }
    Ok(Some(Edit {
        path: p,
        existed,
        kind: "pando-unit".into(),
        content: want,
        listed: true,
        note: PANDO_UNIT_NOTE.into(),
        ..Edit::default()
    }))
}

/// What runs systemctl for the service: action is "enable" (daemon-reload,
/// then enable --now), "disable" (disable --now) or "reload" (daemon-reload
/// after the file is gone). None (the default, and in every test) means
/// install only writes the file. The install command sets it for a real home
/// and never for a dry run. A failure is the hook's to report, in one line;
/// it never fails the install.
pub type UnitHook = Arc<dyn Fn(&str, &str) -> Result<(), String> + Send + Sync>;

static UNIT_HOOK: Mutex<Option<UnitHook>> = Mutex::new(None);

/// Sets (or clears) the hook.
pub fn set_unit_hook(h: Option<UnitHook>) {
    *UNIT_HOOK.lock().unwrap_or_else(|e| e.into_inner()) = h;
}

static TEST_UNIT_HOOK: Mutex<Option<UnitHook>> = Mutex::new(None);

/// A hook that stands in for systemctl in tests of the install command.
#[cfg(test)]
pub fn set_test_unit_hook(h: Option<UnitHook>) {
    *TEST_UNIT_HOOK.lock().unwrap_or_else(|e| e.into_inner()) = h;
}

/// The test hook, when one is set.
pub fn test_unit_hook() -> Option<UnitHook> {
    TEST_UNIT_HOOK.lock().unwrap_or_else(|e| e.into_inner()).clone()
}

/// Tests that apply an install share the process-wide unit hook: one at a time.
#[cfg(test)]
pub static HOOK_TEST_LOCK: Mutex<()> = Mutex::new(());

/// Whether a unit hook is armed (tests).
#[cfg(test)]
pub fn unit_hook_is_set() -> bool {
    UNIT_HOOK.lock().unwrap_or_else(|e| e.into_inner()).is_some()
}

fn run_unit_hook(action: &str) {
    let h = UNIT_HOOK.lock().unwrap_or_else(|e| e.into_inner()).clone();
    if let Some(h) = h {
        let _ = h(action, PANDO_UNIT_NAME);
    }
}

/// Keeps one pando unit edit across the runtimes of one install: Claude Code
/// and Hermes share the file, so the second plan of it is dropped.
pub fn dedupe_unit_edits(changes: &mut [Change]) {
    let mut seen: HashSet<String> = HashSet::new();
    for ch in changes.iter_mut() {
        ch.edits.retain(|e| {
            if e.kind == "pando-unit" || e.kind == "pando-unit-remove" {
                return seen.insert(format!("{}{}", e.kind, e.path));
            }
            true
        });
    }
}

fn is_our_unit(p: &str) -> bool {
    fs::read_to_string(p).is_ok_and(|b| b.starts_with(PANDO_UNIT_MARK))
}

/// Performs the edits of this layer.
pub fn apply_pando_edit(e: &Edit) -> std::io::Result<()> {
    match e.kind.as_str() {
        "pando-plugin" => {
            mkdir_all(&e.path)?;
            for (rel, text) in pando_plugin_files(&e.content) {
                wardenroot::write_file(&join(&e.path, rel), &text)?;
            }
        }
        "pando-unit" => {
            mkdir_all(&parent(&e.path))?;
            // A unit the owner edited by hand is kept as a backup first.
            if fs::symlink_metadata(&e.path).is_ok() && !is_our_unit(&e.path) {
                super::super::install::backup(&e.path)?;
            }
            wardenroot::write_file(&e.path, &e.content)?;
            run_unit_hook("enable");
        }
        "pando-unit-remove" => {
            run_unit_hook("disable");
            clear_path(&e.path)?;
            run_unit_hook("reload");
        }
        _ => {}
    }
    Ok(())
}

type PlanFn<'a> = Box<dyn Fn(&Change) -> R<Option<Edit>> + 'a>;

/// Plans the layer's edits for one runtime, in order.
pub fn pando_install_steps<'a>(rt: &Runtime, home: &str, self_path: &'a str) -> Vec<PlanFn<'a>> {
    let j = |rel: &str| join(home, rel);
    let mut steps: Vec<PlanFn<'a>> = vec![];
    match rt.key {
        "claude" => {
            let cj = j(".claude.json");
            steps.push(Box::new(move |ch| {
                plan_mcp_named(PANDO_MCP_NAME, &cj, false, &["mcpServers"], pando_mcp_entry(true, self_path), "pando MCP registration", prior_of(ch, &cj).as_ref())
            }));
            let settings = j(".claude/settings.json");
            steps.push(Box::new(move |ch| written_needs_dir(plan_pando_claude_hooks(&settings, prior_of(ch, &settings).as_ref(), self_path), &settings)));
        }
        "codex" => {
            let t = j(".codex/config.toml");
            steps.push(Box::new(move |ch| plan_pando_codex_mcp(&t, self_path, prior_of(ch, &t).as_ref())));
        }
        "hermes" => {
            for d in hermes_homes(home) {
                let cfg = join(&d, "config.yaml");
                steps.push(Box::new(move |ch| plan_pando_hermes_config(&cfg, self_path, prior_of(ch, &cfg).as_ref())));
                steps.push(Box::new(move |_| Ok(plan_pando_hermes_plugin(&d, self_path))));
            }
        }
        _ => {}
    }
    if rt.key == "claude" || rt.key == "hermes" {
        let h = home.to_string();
        steps.push(Box::new(move |_| plan_pando_unit(&h, self_path)));
    }
    steps
}

/// Plans the layer's removal for one runtime into ch. A path an earlier
/// runtime's reverse removed (`gone`) is not there by the time this one runs.
pub fn plan_pando_reverse(rt: &Runtime, home: &str, gone: &mut HashSet<String>, ch: &mut Change) -> R<()> {
    let j = |rel: &str| join(home, rel);
    let push = |ch: &mut Change, r: R<Option<Edit>>| -> R<()> {
        match r {
            Err(InstErr::Fail(_)) => Err(InstErr::Unsupported),
            Err(e) => Err(e),
            Ok(Some(e)) => {
                ch.edits.push(e);
                Ok(())
            }
            Ok(None) => Ok(()),
        }
    };
    match rt.key {
        "claude" => {
            let cj = j(".claude.json");
            let r = plan_pop_mcp_named(PANDO_MCP_NAME, &cj, "mcpServers", prior_of(ch, &cj).as_ref());
            push(ch, r)?;
            let settings = j(".claude/settings.json");
            let r = plan_pop_hook(&settings, &is_pando_hook, &PANDO_HOOK_EVENTS, prior_of(ch, &settings).as_ref());
            push(ch, r)?;
        }
        "codex" => {
            let t = j(".codex/config.toml");
            let r = plan_pando_codex_mcp_remove(&t, prior_of(ch, &t).as_ref());
            push(ch, r)?;
        }
        "hermes" => {
            for d in hermes_homes(home) {
                let cfg = join(&d, "config.yaml");
                let r = plan_pando_hermes_reverse(&cfg, prior_of(ch, &cfg).as_ref());
                push(ch, r)?;
                push(ch, Ok(plan_remove(&join(&join(&d, "plugins"), "pando"), gone)))?;
            }
        }
        _ => {}
    }
    if rt.key == "claude" || rt.key == "hermes" {
        let p = pando_unit_path(home);
        // Not there, or the owner's own file: leave it.
        if !gone.contains(&p) && fs::symlink_metadata(&p).is_ok() && is_our_unit(&p) {
            gone.insert(p.clone());
            ch.edits.push(Edit { path: p, kind: "pando-unit-remove".into(), listed: true, ..Edit::default() });
        }
    }
    Ok(())
}

/// The dry-run lines of the layer for one runtime.
pub fn pando_plan_steps(rt: &Runtime, home: &str) -> Vec<Step> {
    let j = |rel: &str| join(home, rel);
    let mut out = vec![];
    let mut add = |desc: &str, target: String| {
        out.push(Step { layer: LAYER_PANDO, description: desc.into(), target, supported: true });
    };
    match rt.key {
        "claude" => {
            add("Register MCP server \"pando\" (the memory: recall, zoom, date, view)", j(".claude.json"));
            add(
                "Add pando memory hooks (SessionStart view; UserPromptSubmit, Stop and SessionEnd log the chat)",
                j(".claude/settings.json"),
            );
        }
        "codex" => add("Register MCP server \"pando\"", j(".codex/config.toml")),
        "hermes" => {
            for d in hermes_homes(home) {
                add("Register MCP server \"pando\" and enable the pando plugin (memory view in every turn)", join(&d, "config.yaml"));
                add("Install the pando plugin", join(&join(&d, "plugins"), "pando"));
            }
        }
        _ => {}
    }
    if rt.key == "claude" || rt.key == "hermes" {
        add("Add the pando follow user service (live log of every chat, background summaries); enable and start it", pando_unit_path(home));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::super::super::install::{apply_change, RUNTIMES};
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    static N: AtomicUsize = AtomicUsize::new(0);
    // The unit hook and HERMES_HOME are process-wide; one test at a time.

    fn scratch() -> std::path::PathBuf {
        let d = std::env::temp_dir().join(format!("pandolayer-t-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
        let _ = fs::remove_dir_all(&d);
        for sub in [".claude", ".hermes", ".hermes/profiles/household"] {
            fs::create_dir_all(d.join(sub)).unwrap();
        }
        fs::write(d.join(".hermes/profiles/household/config.yaml"), "model: x\n").unwrap();
        fs::write(d.join(".claude/settings.json"), r#"{"permissions":{"allow":["Bash"]}}"#).unwrap();
        d
    }

    fn rt(k: &str) -> Runtime {
        RUNTIMES.iter().find(|r| r.key == k).unwrap().clone()
    }

    fn only_pando() -> Opts {
        Opts { only: Some(vec![LAYER_PANDO.into()]), ..Opts::default() }
    }

    fn run_only(r: &Runtime, home: &str) {
        let ch = plan_install(r, home, "/opt/bin/daisugi", &format!("{home}/w"), &only_pando()).unwrap();
        assert!(!ch.failed, "{}", ch.why);
        let (_, res) = apply_change(&ch);
        res.unwrap();
    }

    fn lock() -> std::sync::MutexGuard<'static, ()> {
        let g = HOOK_TEST_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        std::env::remove_var("HERMES_HOME");
        g
    }

    fn rd(p: &std::path::Path) -> String {
        fs::read_to_string(p).unwrap_or_default()
    }

    fn hook_of(settings: &str, ev: &str) -> Object {
        let v = loads(settings).unwrap();
        let Value::Obj(o) = v else { panic!() };
        let Value::Obj(hooks) = o.value("hooks").clone() else { panic!() };
        let Value::List(list) = hooks.value(ev).clone() else { panic!("no {ev}") };
        for e in list {
            if let Value::Obj(eo) = e {
                if let Value::List(hs) = eo.value("hooks") {
                    for h in hs {
                        if let Value::Obj(ho) = h {
                            if is_pando_hook(ho.value("command")) {
                                return ho.clone();
                            }
                        }
                    }
                }
            }
        }
        panic!("no pando hook for {ev} in {settings}");
    }

    #[test]
    fn claude_gets_the_mcp_server_four_hooks_and_keeps_the_rest() {
        let _g = lock();
        let home = scratch();
        let h = home.to_str().unwrap();
        run_only(&rt("claude"), h);
        let mcp = rd(&home.join(".claude.json"));
        assert!(mcp.contains("\"pando\"") && mcp.contains("\"serve\"") && !mcp.contains("opendaisugi"), "{mcp}");
        let set = rd(&home.join(".claude/settings.json"));
        for ev in ["SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"] {
            assert!(set.contains(ev), "{ev}");
        }
        assert_eq!(set.matches("/opt/bin/daisugi pando hook").count(), 4);
        assert!(set.contains("\"permissions\""));
        assert!(fs::metadata(pando_unit_path(h)).is_ok());
        // Idempotent: a second plan is empty.
        let ch = plan_install(&rt("claude"), h, "/opt/bin/daisugi", &format!("{h}/w"), &only_pando()).unwrap();
        assert!(ch.edits.is_empty(), "{} edits", ch.edits.len());
        // Reverse takes out the pando entries and nothing else.
        let mut gone = HashSet::new();
        let rv = plan_reverse_all(&rt("claude"), h, &mut gone, Some(&[LAYER_PANDO.to_string()])).unwrap();
        apply_change(&rv).1.unwrap();
        let set = rd(&home.join(".claude/settings.json"));
        let mcp = rd(&home.join(".claude.json"));
        assert!(!set.contains("pando") && !mcp.contains("pando") && set.contains("\"permissions\""), "{set} | {mcp}");
        assert!(fs::metadata(pando_unit_path(h)).is_err());
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn logging_hooks_are_async_and_the_view_hook_is_not() {
        let _g = lock();
        let home = scratch();
        run_only(&rt("claude"), home.to_str().unwrap());
        let set = rd(&home.join(".claude/settings.json"));
        assert!(hook_of(&set, "SessionStart").get("async").is_none());
        for ev in ["UserPromptSubmit", "Stop"] {
            assert_eq!(*hook_of(&set, ev).value("async"), Value::Bool(true), "{ev}");
        }
        // The end of the chat is waited for, so the last reply is not lost.
        assert!(hook_of(&set, "SessionEnd").get("async").is_none());
        assert_eq!(*hook_of(&set, "SessionEnd").value("timeout"), Value::Int("10".into()));
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn session_end_of_the_first_build_is_brought_to_the_foreground() {
        let _g = lock();
        let home = scratch();
        fs::write(
            home.join(".claude/settings.json"),
            r#"{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"/opt/bin/daisugi pando hook","timeout":10,"async":true}]}]}}"#,
        )
        .unwrap();
        run_only(&rt("claude"), home.to_str().unwrap());
        let set = rd(&home.join(".claude/settings.json"));
        assert!(hook_of(&set, "SessionEnd").get("async").is_none());
        assert_eq!(set.matches(" pando hook").count(), 4, "{set}");
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn hooks_of_the_first_build_are_brought_up() {
        let _g = lock();
        let home = scratch();
        fs::write(
            home.join(".claude/settings.json"),
            r#"{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/opt/bin/daisugi pando hook","timeout":10}]}],"SessionStart":[{"hooks":[{"type":"command","command":"/opt/bin/daisugi pando hook","timeout":10,"async":true}]}]}}"#,
        )
        .unwrap();
        run_only(&rt("claude"), home.to_str().unwrap());
        let set = rd(&home.join(".claude/settings.json"));
        assert_eq!(*hook_of(&set, "Stop").value("async"), Value::Bool(true));
        assert!(hook_of(&set, "SessionStart").get("async").is_none());
        assert_eq!(set.matches(" pando hook").count(), 4, "{set}");
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn hermes_covers_every_profile_asks_every_turn_and_is_idempotent() {
        let _g = lock();
        let home = scratch();
        let h = home.to_str().unwrap();
        run_only(&rt("hermes"), h);
        for d in [".hermes", ".hermes/profiles/household"] {
            let cfg = rd(&home.join(d).join("config.yaml"));
            assert!(cfg.contains("pando") && cfg.contains("enabled"), "{d}: {cfg}");
            let py = rd(&home.join(d).join("plugins/pando/__init__.py"));
            assert!(py.contains("\"/opt/bin/daisugi\"") && py.contains("pre_llm_call"), "{py}");
            assert!(!py.contains("if not is_first_turn"), "the plugin skips later turns:\n{py}");
            assert!(home.join(d).join("plugins/pando/plugin.yaml").is_file());
        }
        assert!(rd(&home.join(".hermes/profiles/household/config.yaml")).contains("model: x"));
        let ch = plan_install(&rt("hermes"), h, "/opt/bin/daisugi", &format!("{h}/w"), &only_pando()).unwrap();
        assert!(ch.edits.is_empty(), "second plan: {} edits", ch.edits.len());
        let mut gone = HashSet::new();
        let rv = plan_reverse_all(&rt("hermes"), h, &mut gone, Some(&[LAYER_PANDO.to_string()])).unwrap();
        apply_change(&rv).1.unwrap();
        assert!(!home.join(".hermes/plugins/pando").exists());
        assert!(!rd(&home.join(".hermes/profiles/household/config.yaml")).contains("pando"));
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn the_codex_block_is_found_by_line_and_removed_with_its_sub_tables() {
        let _g = lock();
        let home = scratch();
        fs::create_dir_all(home.join(".codex")).unwrap();
        let p = home.join(".codex/config.toml");
        let ps = p.to_str().unwrap();
        fs::write(&p, "# add [mcp_servers.pando] later\nmodel = \"x\"\n").unwrap();
        let e = plan_pando_codex_mcp(ps, "/opt/bin/daisugi", None).unwrap().unwrap();
        assert_eq!(e.content.matches("\n[mcp_servers.pando]\n").count(), 1, "{}", e.content);
        fs::write(&p, "model = \"x\"\n\n[mcp_servers.pando]\ncommand = \"d\"\n\n[mcp_servers.pando.env]\nA = \"1\"\n\n[[profiles]]\nname = \"p\"\n").unwrap();
        let r = plan_pando_codex_mcp_remove(ps, None).unwrap().unwrap();
        assert!(!r.content.contains("pando") && r.content.contains("[[profiles]]") && r.content.contains("model = \"x\""), "{}", r.content);
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn the_hermes_dir_follows_hermes_home() {
        let _g = lock();
        let elsewhere = scratch().join("hh");
        fs::create_dir_all(&elsewhere).unwrap();
        std::env::set_var("HERMES_HOME", &elsewhere);
        let got = hermes_homes("/nonexistent-home");
        std::env::remove_var("HERMES_HOME");
        assert_eq!(got[0], elsewhere.to_string_lossy());
    }

    #[test]
    fn the_owners_own_unit_is_backed_up_and_never_removed() {
        let _g = lock();
        let home = scratch();
        let h = home.to_str().unwrap();
        let p = pando_unit_path(h);
        fs::create_dir_all(parent(&p)).unwrap();
        fs::write(&p, "[Service]\nExecStart=/my/own\n").unwrap();
        let e = plan_pando_unit(h, "/opt/bin/daisugi").unwrap().unwrap();
        apply_pando_edit(&e).unwrap();
        let baks: Vec<_> = fs::read_dir(parent(&p)).unwrap().flatten().filter(|d| d.file_name().to_string_lossy().contains(".bak")).collect();
        assert_eq!(baks.len(), 1);
        assert!(rd(&baks[0].path()).contains("/my/own"));
        fs::write(&p, "[Service]\nExecStart=/my/own\n").unwrap();
        let mut gone = HashSet::new();
        let rv = plan_reverse_all(&rt("claude"), h, &mut gone, Some(&[LAYER_PANDO.to_string()])).unwrap();
        assert!(rv.edits.iter().all(|e| e.kind != "pando-unit-remove"), "uninstall would remove a unit that is not ours");
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn the_unit_hook_enables_on_install_and_disables_on_uninstall() {
        let _g = lock();
        let home = scratch();
        let h = home.to_str().unwrap();
        let calls: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(vec![]));
        let c2 = calls.clone();
        set_unit_hook(Some(Arc::new(move |a: &str, u: &str| {
            c2.lock().unwrap().push(format!("{a} {u}"));
            Ok(())
        })));
        run_only(&rt("claude"), h);
        assert_eq!(calls.lock().unwrap().join(","), format!("enable {PANDO_UNIT_NAME}"));
        calls.lock().unwrap().clear();
        let mut gone = HashSet::new();
        let rv = plan_reverse_all(&rt("claude"), h, &mut gone, Some(&[LAYER_PANDO.to_string()])).unwrap();
        apply_change(&rv).1.unwrap();
        set_unit_hook(None);
        assert_eq!(calls.lock().unwrap().join(","), format!("disable {PANDO_UNIT_NAME},reload {PANDO_UNIT_NAME}"));
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn the_unit_is_planned_once_when_claude_and_hermes_are_both_selected() {
        let _g = lock();
        let home = scratch();
        let h = home.to_str().unwrap();
        let mut changes: Vec<Change> = ["claude", "hermes"]
            .iter()
            .map(|k| plan_install(&rt(k), h, "/opt/bin/daisugi", &format!("{h}/w"), &only_pando()).unwrap())
            .collect();
        dedupe_unit_edits(&mut changes);
        let n = changes.iter().flat_map(|c| c.edits.iter()).filter(|e| e.kind == "pando-unit").count();
        assert_eq!(n, 1);
        let _ = fs::remove_dir_all(&home);
    }

    // Two layers write the same file (config.yaml, config.toml): the second
    // plans on what the first wrote, so a default install keeps both.
    #[test]
    fn a_default_install_keeps_the_opendaisugi_and_the_pando_entries_in_one_file() {
        let _g = lock();
        let home = scratch();
        let h = home.to_str().unwrap();
        fs::create_dir_all(home.join(".codex")).unwrap();
        for k in ["codex", "hermes"] {
            let ch = plan_install(&rt(k), h, "/opt/bin/daisugi", &format!("{h}/w"), &Opts::default()).unwrap();
            assert!(!ch.failed, "{k}: {}", ch.why);
            apply_change(&ch).1.unwrap();
        }
        let cfg = rd(&home.join(".hermes/config.yaml"));
        let toml = rd(&home.join(".codex/config.toml"));
        for want in ["opendaisugi", "pando"] {
            assert!(cfg.contains(want) && toml.contains(want), "lost {want}:\n{cfg}\n---\n{toml}");
        }
        assert!(cfg.contains("pre_tool_call"), "lost the capture hook:\n{cfg}");
        for k in ["codex", "hermes"] {
            let ch = plan_install(&rt(k), h, "/opt/bin/daisugi", &format!("{h}/w"), &Opts::default()).unwrap();
            // The two shared config files are not planned again. (The skill copy and the
            // instructions file are always planned, and no-ops when current.)
            let what: Vec<String> = ch
                .edits
                .iter()
                .filter(|e| e.path.ends_with("config.yaml") || e.path.ends_with("config.toml") || e.kind.starts_with("pando"))
                .map(|e| format!("{} [{}]", e.path, e.kind))
                .collect();
            assert!(what.is_empty(), "{k} second plan: {what:?}");
            let mut gone = HashSet::new();
            let rv = plan_reverse_all(&rt(k), h, &mut gone, None).unwrap();
            apply_change(&rv).1.unwrap();
        }
        let cfg = rd(&home.join(".hermes/config.yaml"));
        let toml = rd(&home.join(".codex/config.toml"));
        assert!(
            !cfg.contains("pando") && !cfg.contains("opendaisugi") && !toml.contains("pando") && !toml.contains("opendaisugi"),
            "an entry is left:\n{cfg}\n---\n{toml}"
        );
        let _ = fs::remove_dir_all(&home);
    }

    #[test]
    fn pando_is_a_component_so_only_can_name_it() {
        let got = super::super::super::components::component_layers(&["pando".to_string()]).unwrap();
        assert_eq!(got, vec!["pando".to_string()]);
        assert!(super::super::super::components::list_components().contains("pando"));
        assert!(!wires(&rt("openclaw"), LAYER_PANDO) && wires(&rt("claude"), LAYER_PANDO));
    }
}
