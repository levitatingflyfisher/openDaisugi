//! The gate parts of `opendaisugi.install`: the PreToolUse hook in Claude
//! Code's settings.json and Codex's hooks.json, and the pi and OpenCode
//! gate extensions. Every file written is the file Python writes, apart
//! from the hook command, which runs this binary instead of Python. The Go
//! client's `install` package is the reference.

use super::gateroot::{self, join, mkdir_all, parent, path_str};
use super::words::{gate_hook_kind, is_gate_hook, is_record_hook, GATE_HOOK_MARKER, KIND_NONE, KIND_UNKNOWN};
use crate::gate::pyjson::{dumps, dumps_indent, float_repr, loads, LoadError, Object, Value};
use crate::interpreter_parse::shlex_quote;
use std::collections::HashMap;
use std::fs;
use std::os::unix::fs::PermissionsExt;

/// Why an install step stopped.
#[derive(Debug)]
pub enum InstErr {
    /// A shape Python's apply raises on: the runtime fails, nothing is
    /// written for it, and the CLI names it and exits 1.
    Fail(String),
    /// A file this binary does not edit: the whole command is refused
    /// before anything is written.
    Unsupported,
}

impl std::fmt::Display for InstErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            InstErr::Fail(w) => write!(f, "{w}"),
            InstErr::Unsupported => write!(f, "a settings file holds a shape this binary does not edit yet"),
        }
    }
}

type R<T> = Result<T, InstErr>;

fn fail<T>(why: String) -> R<T> {
    Err(InstErr::Fail(why))
}

/// One agent harness install knows.
#[derive(Debug, Clone, PartialEq)]
pub struct Runtime {
    pub key: &'static str,
    pub name: &'static str,
}

/// `install._ALL_RUNTIMES`, in order.
pub const RUNTIMES: &[Runtime] = &[
    Runtime { key: "claude", name: "Claude Code" },
    Runtime { key: "codex", name: "Codex" },
    Runtime { key: "hermes", name: "Hermes" },
    Runtime { key: "openclaw", name: "OpenClaw" },
];

/// The gate gap Hermes and OpenClaw report.
pub const NOT_WIRED: &str = "Not wired: external/JS-shim gate not yet wired — follow-up";

/// `shutil.which` over the PATH in env.
pub fn look_path(env: &HashMap<String, String>, name: &str) -> Option<String> {
    for dir in env.get("PATH").map(String::as_str).unwrap_or("").split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let p = format!("{dir}/{name}");
        if let Ok(m) = fs::metadata(&p) {
            if !m.is_dir() && m.permissions().mode() & 0o111 != 0 {
                return Some(p);
            }
        }
    }
    None
}

/// `install.detect_runtimes`: Claude Code, Hermes and OpenClaw by their
/// home directory, Codex by its directory or `codex` on PATH.
pub fn detect(home: &str, env: &HashMap<String, String>) -> Vec<Runtime> {
    RUNTIMES
        .iter()
        .filter(|rt| {
            let found = gateroot::is_dir(&join(home, &format!(".{}", rt.key)));
            found || (rt.key == "codex" && look_path(env, "codex").is_some())
        })
        .cloned()
        .collect()
}

/// `install._select_runtimes`: each fragment by exact key or unique
/// prefix, deduplicated, in the order first named.
pub fn select(names: &[String]) -> Result<Vec<Runtime>, String> {
    let mut out: Vec<Runtime> = vec![];
    for raw in names {
        let frag = raw.trim_matches(|c: char| " \t\n\x0b\x0c\r\x1c\x1d\x1e\x1f".contains(c)).to_lowercase();
        let exact: Vec<Runtime> = RUNTIMES.iter().filter(|rt| rt.key == frag).cloned().collect();
        let matches =
            if exact.is_empty() { RUNTIMES.iter().filter(|rt| rt.key.starts_with(&frag)).cloned().collect() } else { exact };
        if matches.len() != 1 {
            return Err(format!(
                "--runtime {} matched {} runtimes; use one of: claude, codex, hermes, openclaw",
                crate::gate::py::text::repr(raw),
                matches.len()
            ));
        }
        if !out.iter().any(|r| r.name == matches[0].name) {
            out.push(matches[0].clone());
        }
    }
    Ok(out)
}

/// An InstallStep of the gate layer.
pub struct Step {
    pub description: String,
    pub target: String,
    pub supported: bool,
}

/// `Runtime.plan(home, {GATE}, enforce=..., ask=...)`.
pub fn plan(rt: &Runtime, home: &str, enforce: bool, ask: bool) -> Vec<Step> {
    let mode = if enforce { "ENFORCE" } else { "shadow" };
    match rt.key {
        "claude" => vec![Step {
            description: format!(
                "Install fail-closed gate PreToolUse hook ({mode}{})",
                if ask { " + operator ask" } else { "" }
            ),
            target: join(home, ".claude/settings.json"),
            supported: true,
        }],
        "codex" => vec![Step {
            description: format!(
                "Install gate PreToolUse hook ({mode}) — Codex hooks fail OPEN on hook crash/timeout: deny works, a dead gate does not block"
            ),
            target: join(home, ".codex/hooks.json"),
            supported: true,
        }],
        _ => vec![Step { description: NOT_WIRED.into(), target: String::new(), supported: false }],
    }
}

/// One planned change: a file written (after a backup when it existed),
/// or a warning with no write.
#[derive(Debug, Clone)]
pub struct Edit {
    pub path: String,
    pub existed: bool,
    pub content: String,
    pub warning: String,
}

/// What applying the gate layer to one runtime does.
pub struct Change {
    pub runtime: Runtime,
    pub mkdirs: Vec<String>,
    pub edits: Vec<Edit>,
    pub failed: bool,
    pub why: String,
}

impl Change {
    fn new(rt: &Runtime) -> Change {
        Change { runtime: rt.clone(), mkdirs: vec![], edits: vec![], failed: false, why: String::new() }
    }

    /// The paths the change reports, in order.
    pub fn written(&self) -> Vec<String> {
        self.edits.iter().filter(|e| e.warning.is_empty()).map(|e| e.path.clone()).collect()
    }
}

/// `gate.gate_settings_json`'s arguments.
pub struct HookOptions {
    pub mode: String,
    pub root: String,
    pub format: String,
    pub session: Option<String>,
    pub ask: bool,
}

/// The PreToolUse entry `gate_settings_json` builds, with the command
/// running `<self> gate check` instead of `python -m
/// opendaisugi.gate_client`. The flags after it are the same.
pub fn hook_entry(self_path: &str, o: &HookOptions) -> Object {
    let (hook_timeout, verify_timeout, ask_timeout) = (30i64, 10.0f64, 90.0f64);
    let inner = verify_timeout.min((hook_timeout as f64 - 5.0).max(1.0));
    let q = shlex_quote;
    let mut cmd = format!(
        "{GATE_HOOK_MARKER} {} gate check --mode {} --root {} --format {} --verify-timeout {}",
        q(self_path),
        q(&o.mode),
        q(&o.root),
        q(&o.format),
        float_repr(inner)
    );
    if let Some(s) = &o.session {
        cmd.push_str(&format!(" --session {}", q(s)));
    }
    let mut timeout = hook_timeout;
    if o.ask {
        cmd.push_str(&format!(" --ask --ask-timeout {}", ask_timeout as i64));
        timeout = timeout.max((ask_timeout + verify_timeout + 5.0) as i64);
    }
    // Default-deny at the process boundary: on Claude Code a hook exit
    // other than 2 does not block, so in enforce mode every other nonzero
    // exit of the gate is made a deny.
    if o.format == "claude" && o.mode == "enforce" {
        cmd.push_str(" || exit 2");
    }
    Object::new().with("matcher", "*").with(
        "hooks",
        Value::List(vec![Value::Obj(
            Object::new().with("type", "command").with("command", cmd).with("timeout", Value::Int(timeout.to_string())),
        )]),
    )
}

/// `gate_settings_json`: the hooks settings document, as json.dumps
/// writes it.
pub fn settings_json(self_path: &str, o: &HookOptions) -> String {
    let doc =
        Object::new().with("hooks", Object::new().with("PreToolUse", Value::List(vec![Value::Obj(hook_entry(self_path, o))])));
    dumps(&Value::Obj(doc), true)
}

/// The path this binary was run by, made absolute but with its symlinks
/// kept: the program an installed hook runs.
pub fn self_path(arg0: &str, path: &str) -> std::io::Result<String> {
    if arg0.contains('/') {
        return go_abs(arg0);
    }
    for dir in path.split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let p = format!("{dir}/{arg0}");
        if let Ok(m) = fs::metadata(&p) {
            if !m.is_dir() && m.permissions().mode() & 0o111 != 0 {
                return go_abs(&p);
            }
        }
    }
    std::env::current_exe()?.into_os_string().into_string().map_err(|_| std::io::Error::other("not UTF-8"))
}

/// `filepath.Abs`: joined to the working directory and cleaned.
fn go_abs(p: &str) -> std::io::Result<String> {
    let joined = if p.starts_with('/') { p.to_string() } else { format!("{}/{p}", gateroot::getwd()?) };
    let rooted = joined.starts_with('/');
    let mut out: Vec<&str> = vec![];
    for c in joined.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                out.pop();
            }
            c => out.push(c),
        }
    }
    Ok(format!("{}{}", if rooted { "/" } else { "" }, out.join("/")))
}

#[derive(PartialEq)]
enum ReadState {
    Ok,
    /// `path.exists()` is false.
    Missing,
    /// JSONDecodeError or OSError: caught and warned.
    Bad,
}

fn read_json(p: &str) -> R<(Value, ReadState)> {
    if fs::metadata(p).is_err() {
        return Ok((Value::Null, ReadState::Missing));
    }
    let raw = match fs::read(p) {
        Ok(r) => r,
        Err(_) => return Ok((Value::Null, ReadState::Bad)),
    };
    let text = match String::from_utf8(raw) {
        Ok(t) => t,
        // UnicodeDecodeError is not caught: the runtime fails.
        Err(_) => return fail(format!("{p} is not valid UTF-8")),
    };
    match loads(&text) {
        Ok(v) => Ok((v, ReadState::Ok)),
        // Python raises past these (not a JSONDecodeError): refused.
        Err(LoadError::Unsupported | LoadError::Recursion | LoadError::IntDigits) => Err(InstErr::Unsupported),
        Err(LoadError::Syntax(_)) => Ok((Value::Null, ReadState::Bad)),
    }
}

fn kind(v: &Value) -> &'static str {
    match v {
        Value::Obj(_) => "mapping",
        Value::List(_) => "list",
        Value::Str(_) => "string",
        Value::Null => "null",
        _ => "scalar",
    }
}

/// Python's `for x in v` over a JSON value, for a loop whose body calls
/// x.get.
fn iter_items(v: &Value) -> R<Vec<Value>> {
    match v {
        Value::List(l) => Ok(l.clone()),
        Value::Obj(o) if o.is_empty() => Ok(vec![]),
        Value::Str(s) if s.is_empty() => Ok(vec![]),
        other => fail(format!("a hooks value is a {}, not a list", kind(other))),
    }
}

/// Every hook of type "command" under entries, in order.
fn command_hooks(entries: &[Value]) -> R<Vec<Object>> {
    let mut out = vec![];
    for e in entries {
        let entry = match e {
            Value::Obj(o) => o,
            other => return fail(format!("a PreToolUse entry is a {}, not a mapping", kind(other))),
        };
        let hv = match entry.get("hooks") {
            None => continue,
            Some(h) => h,
        };
        for h in iter_items(hv)? {
            let hook = match h {
                Value::Obj(o) => o,
                other => return fail(format!("a hook is a {}, not a mapping", kind(&other))),
            };
            if hook.get("type") == Some(&Value::Str("command".into())) {
                out.push(hook);
            }
        }
    }
    Ok(out)
}

/// `settings.setdefault("hooks", {}).setdefault("PreToolUse", [])`: the
/// settings made to hold both, and the list.
fn pre_tool_use(settings: &mut Value) -> R<Vec<Value>> {
    let s = match settings {
        Value::Obj(o) => o,
        other => return fail(format!("the file holds a {}, not a mapping", kind(other))),
    };
    if s.get("hooks").is_none() {
        s.set("hooks", Object::new());
    }
    let hooks = match s.get_mut("hooks") {
        Some(Value::Obj(o)) => o,
        Some(other) => return fail(format!("\"hooks\" is a {}, not a mapping", kind(other))),
        None => return Err(InstErr::Unsupported),
    };
    if hooks.get("PreToolUse").is_none() {
        hooks.set("PreToolUse", Value::List(vec![]));
    }
    match hooks.value("PreToolUse") {
        Value::List(l) => Ok(l.clone()),
        // Iterating or appending to anything else raises.
        other => fail(format!("\"PreToolUse\" is a {}, not a list", kind(other))),
    }
}

/// Sets `settings["hooks"]["PreToolUse"]`, which `pre_tool_use` made sure
/// is there.
fn set_pre(settings: &mut Value, pre: Vec<Value>) {
    if let Value::Obj(s) = settings {
        if let Some(Value::Obj(h)) = s.get_mut("hooks") {
            h.set("PreToolUse", Value::List(pre));
        }
    }
}

fn dump_settings(v: &Value) -> String {
    format!("{}\n", dumps_indent(v, 2, true))
}

fn hook_command(entry: &Object) -> String {
    match entry.value("hooks") {
        Value::List(l) => match l.first() {
            Some(Value::Obj(h)) => h.value("command").as_str().unwrap_or("").to_string(),
            _ => String::new(),
        },
        _ => String::new(),
    }
}

/// `_patch_claude_gate` with this binary's hook command: `None` when the
/// exact hook is already installed, a warning when another gate hook is,
/// else the new settings.json.
pub fn plan_claude_gate(settings_path: &str, entry: &Object) -> R<Option<Edit>> {
    let gate_command = hook_command(entry);
    let (mut v, st) = read_json(settings_path)?;
    let existed = st != ReadState::Missing;
    if st == ReadState::Bad {
        return Ok(Some(Edit {
            path: settings_path.into(),
            existed: false,
            content: String::new(),
            warning: format!(
                "{settings_path} is not valid JSON; skipping gate hook installation to avoid overwriting your \
                 Claude Code settings (permissions/env). Fix the file and re-run `daisugi install`."
            ),
        }));
    }
    if st == ReadState::Missing {
        v = Value::Obj(Object::new());
    }
    let mut pre = pre_tool_use(&mut v)?;
    let cmds = command_hooks(&pre)?;
    let mut existing = vec![];
    for h in &cmds {
        let c = match h.get("command") {
            None => return fail("a hook of type \"command\" has no \"command\"".into()),
            Some(c) => c,
        };
        if matches!(c, Value::List(_) | Value::Obj(_)) {
            return fail(format!("a hook command is a {}", kind(c)));
        }
        existing.push(c.clone());
    }
    if existing.iter().any(|c| c.as_str() == Some(gate_command.as_str())) {
        return Ok(None);
    }
    if existing.iter().any(|c| gate_hook_kind(c) != KIND_NONE) {
        return Ok(Some(Edit {
            path: settings_path.into(),
            existed: false,
            content: String::new(),
            warning: format!(
                "a gate hook is already installed in {settings_path} with a different mode/config; run \
                 `daisugi install --uninstall` first if you want to change it (idempotent by presence, not content)."
            ),
        }));
    }
    pre.push(Value::Obj(entry.clone()));
    set_pre(&mut v, pre);
    Ok(Some(Edit { path: settings_path.into(), existed, content: dump_settings(&v), warning: String::new() }))
}

/// `_patch_codex_gate`: the gate entry with matcher ".*", added unless any
/// gate hook is there already.
pub fn plan_codex_gate(hooks_path: &str, entry: &Object) -> R<Option<Edit>> {
    let mut entry = entry.clone();
    entry.set("matcher", ".*");
    let (mut v, st) = read_json(hooks_path)?;
    let existed = st != ReadState::Missing;
    if st == ReadState::Bad {
        return Ok(Some(Edit {
            path: hooks_path.into(),
            existed: false,
            content: String::new(),
            warning: format!(
                "{hooks_path} is not valid JSON; skipping gate hook installation. Fix the file and re-run `daisugi install`."
            ),
        }));
    }
    if st == ReadState::Missing {
        v = Value::Obj(Object::new());
    }
    let mut pre = pre_tool_use(&mut v)?;
    let cmds = command_hooks(&pre)?;
    for h in &cmds {
        let c = h.value("command");
        if matches!(c, Value::List(_) | Value::Obj(_)) {
            return fail(format!("a hook command is a {}", kind(c)));
        }
    }
    if cmds.iter().any(|h| gate_hook_kind(h.value("command")) != KIND_NONE) {
        return Ok(None);
    }
    pre.push(Value::Obj(entry));
    set_pre(&mut v, pre);
    Ok(Some(Edit { path: hooks_path.into(), existed, content: dump_settings(&v), warning: String::new() }))
}

/// `_pop_json_hook`: every hook whose command `matches` accepts removed
/// from the given events, empty entries and events dropped. `prior` is an
/// earlier edit of the same file in this run, read instead of the file.
pub fn plan_pop_hook(
    settings_path: &str,
    matches: &dyn Fn(&Value) -> bool,
    events: &[&str],
    prior: Option<&Edit>,
) -> R<Option<Edit>> {
    let v = match prior.filter(|p| !p.content.is_empty()) {
        Some(p) => loads(&p.content).map_err(|_| InstErr::Unsupported)?,
        None => {
            let (v, st) = read_json(settings_path)?;
            if st != ReadState::Ok {
                return Ok(None);
            }
            v
        }
    };
    let mut s = match v {
        Value::Obj(o) => o,
        other => return fail(format!("the file holds a {}, not a mapping", kind(&other))),
    };
    let hooks_v = s.get("hooks").cloned().unwrap_or(Value::Obj(Object::new()));
    let mut hooks = match hooks_v {
        Value::Obj(o) => o,
        other => return fail(format!("\"hooks\" is a {}, not a mapping", kind(&other))),
    };
    // The any() over events, entries and hooks, in order: it stops at the
    // first match, so a bad shape after it is not reached.
    let mut found = false;
    'scan: for ev in events {
        let ents = hooks.value(ev);
        if !ents.truthy() {
            continue;
        }
        let list = match ents {
            Value::List(l) => l,
            other => return fail(format!("{ev:?} is a {}, not a list", kind(other))),
        };
        for e in list {
            let entry = match e {
                Value::Obj(o) => o,
                other => return fail(format!("a {ev} entry is a {}, not a mapping", kind(other))),
            };
            let hs_v = match entry.get("hooks") {
                None => continue,
                Some(h) => h,
            };
            for h in iter_items(hs_v)? {
                let hook = match &h {
                    Value::Obj(o) => o,
                    other => return fail(format!("a hook is a {}, not a mapping", kind(other))),
                };
                if matches(hook.value("command")) {
                    found = true;
                    break 'scan;
                }
            }
        }
    }
    if !found {
        return Ok(None);
    }
    // The rewrite walks every entry of every event. A shape it would
    // raise on here fails after the backup was made: refused instead.
    for ev in events {
        let ents = hooks.value(ev).clone();
        if !ents.truthy() {
            continue;
        }
        let list = match ents {
            Value::List(l) => l,
            _ => return Err(InstErr::Unsupported),
        };
        let mut kept = vec![];
        for e in list {
            let mut entry = match e {
                Value::Obj(o) => o,
                _ => return Err(InstErr::Unsupported),
            };
            let hs = match entry.get("hooks") {
                Some(h) => iter_items(h).map_err(|_| InstErr::Unsupported)?,
                None => vec![],
            };
            let mut left = vec![];
            for h in hs {
                let hook = match &h {
                    Value::Obj(o) => o,
                    _ => return Err(InstErr::Unsupported),
                };
                if !matches(hook.value("command")) {
                    left.push(h.clone());
                }
            }
            let any_left = !left.is_empty();
            entry.set("hooks", Value::List(left));
            if any_left {
                kept.push(Value::Obj(entry));
            }
        }
        if kept.is_empty() {
            hooks.remove(ev);
        } else {
            hooks.set(ev, Value::List(kept));
        }
    }
    if hooks.is_empty() {
        s.remove("hooks");
    } else if s.get("hooks").is_some() {
        s.set("hooks", hooks);
    }
    Ok(Some(Edit {
        path: settings_path.into(),
        existed: true,
        content: dump_settings(&Value::Obj(s)),
        warning: String::new(),
    }))
}

fn is_dir(p: &str) -> bool {
    gateroot::is_dir(p)
}

/// `Runtime.apply(home, {GATE}, ...)` decided without writing.
pub fn plan_apply(rt: &Runtime, home: &str, self_path: &str, root: &str, enforce: bool, ask: bool) -> R<Change> {
    let mut ch = Change::new(rt);
    let mode = if enforce { "enforce" } else { "shadow" };
    let opts = |ask: bool| HookOptions { mode: mode.into(), root: root.into(), format: "claude".into(), session: None, ask };
    let res = match rt.key {
        "claude" => plan_claude_gate(&join(home, ".claude/settings.json"), &hook_entry(self_path, &opts(ask))),
        "codex" => {
            ch.mkdirs = vec![join(home, ".codex")];
            plan_codex_gate(&join(home, ".codex/hooks.json"), &hook_entry(self_path, &opts(false)))
        }
        "hermes" => {
            ch.mkdirs = vec![join(home, ".hermes")];
            Ok(None)
        }
        _ => {
            ch.mkdirs = vec![join(home, ".openclaw/workspace")];
            Ok(None)
        }
    };
    let e = match res {
        Err(InstErr::Fail(why)) => {
            ch.failed = true;
            ch.why = why;
            return Ok(ch);
        }
        Err(e) => return Err(e),
        Ok(e) => e,
    };
    if let Some(e) = e {
        let is_write = e.warning.is_empty();
        ch.edits.push(e);
        if rt.key == "claude" && is_write && !is_dir(&join(home, ".claude")) {
            // Claude's apply does not make ~/.claude: the write raises.
            ch.failed = true;
            ch.why = format!("{} does not exist", join(home, ".claude"));
            ch.edits.clear();
        }
    }
    Ok(ch)
}

/// `install._refuse_unknown_gate_hooks`: why a reverse must not touch a
/// settings file holding a PreToolUse hook with the gate module in a form
/// not read, or `None`.
fn unknown_gate_hook(settings_path: &str) -> Option<String> {
    let (v, st) = read_json(settings_path).ok()?;
    if st != ReadState::Ok {
        return None;
    }
    let hooks = v.as_obj()?.value("hooks").as_obj()?.clone();
    let entries = match hooks.value("PreToolUse") {
        Value::List(l) => l.clone(),
        _ => return None,
    };
    for e in entries {
        let inner = match e.as_obj().map(|o| o.value("hooks")) {
            Some(Value::List(l)) => l.clone(),
            _ => continue,
        };
        for h in inner {
            let command = h.as_obj().map(|o| o.value("command").clone()).unwrap_or(Value::Null);
            if gate_hook_kind(&command) == KIND_UNKNOWN {
                let shown = crate::gate::pyjson::py_str(&command).unwrap_or_default();
                return Some(format!(
                    "{settings_path} holds a gate hook in a form this CLI does not read, so it is left in place: \
                     {shown}. Remove it by hand, then run this again"
                ));
            }
        }
    }
    None
}

/// The gate part of `Runtime.reverse`: the gate hook, and on Claude Code
/// the floor-report hooks, removed.
pub fn plan_reverse(rt: &Runtime, home: &str) -> R<Change> {
    let mut ch = Change::new(rt);
    let any_record = |c: &Value| is_record_hook(c, "");
    let gate = |c: &Value| is_gate_hook(c);
    let steps: Vec<(String, &dyn Fn(&Value) -> bool, Vec<&str>)> = match rt.key {
        "claude" => {
            let p = join(home, ".claude/settings.json");
            vec![
                (p.clone(), &gate, vec!["PreToolUse"]),
                (p, &any_record, vec!["Stop", "Notification", "SubagentStart", "SubagentStop"]),
            ]
        }
        "codex" => vec![(join(home, ".codex/hooks.json"), &gate, vec!["PreToolUse"])],
        _ => vec![],
    };
    if let Some(first) = steps.first() {
        if let Some(why) = unknown_gate_hook(&first.0) {
            // reverse() raises before it touches anything: the runtime is
            // left as it is and reported as failed, naming the hook.
            ch.failed = true;
            ch.why = why;
            return Ok(ch);
        }
    }
    let mut prior: Option<Edit> = None;
    for (path, m, events) in &steps {
        match plan_pop_hook(path, *m, events, prior.as_ref()) {
            // reverse() would raise, and uninstall would print the
            // exception's text, which this binary cannot reproduce.
            Err(InstErr::Fail(_)) => return Err(InstErr::Unsupported),
            Err(e) => return Err(e),
            Ok(Some(e)) => {
                ch.edits.push(e.clone());
                prior = Some(e);
            }
            Ok(None) => {}
        }
    }
    Ok(ch)
}

/// `install._backup`: a copy with the mode and times kept, named
/// `<name>.bak<ns>`, never over an existing backup.
pub fn backup(p: &str) -> std::io::Result<()> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0)
        .to_string();
    let mut dest = format!("{p}.bak{stamp}");
    let mut n = 1;
    while gateroot::lexists(&dest) {
        dest = format!("{p}.bak{stamp}.{n}");
        n += 1;
    }
    let data = fs::read(p)?;
    let meta = fs::metadata(p)?;
    let perm = meta.permissions().mode() & 0o777;
    let mut out = fs::OpenOptions::new().write(true).create_new(true).mode(perm).open(&dest)?;
    // A backup that could not be written whole is removed: a partial copy
    // must never pass for the file it was made from.
    let res = out
        .set_permissions(fs::Permissions::from_mode(perm))
        .and_then(|_| out.write_all(&data))
        .and_then(|_| out.sync_all());
    drop(out);
    if let Err(e) = res {
        let _ = fs::remove_file(&dest);
        return Err(e);
    }
    let mtime = meta.modified()?;
    let f = fs::File::options().write(true).open(&dest)?;
    f.set_times(fs::FileTimes::new().set_accessed(mtime).set_modified(mtime))
}

/// Performs a planned edit: a backup of the file as it stands when it
/// existed, then the write. Returns the file written when the path was a
/// symlink.
pub fn apply(e: &Edit) -> std::io::Result<Option<String>> {
    if !e.warning.is_empty() {
        return Ok(None);
    }
    if e.existed {
        backup(&e.path).map_err(|err| std::io::Error::other(format!("backup {}: {err}", e.path)))?;
    }
    gateroot::write_file(&e.path, &e.content)
}

/// Makes the directories and applies the edits in order. Returns
/// "<path> -> <file>" for each write that went through a symlink.
pub fn apply_change(ch: &Change) -> (Vec<String>, std::io::Result<()>) {
    let mut followed = vec![];
    for d in &ch.mkdirs {
        if let Err(e) = mkdir_all(d) {
            return (followed, Err(e));
        }
    }
    if ch.failed {
        return (followed, Ok(()));
    }
    for e in &ch.edits {
        match apply(e) {
            Err(err) => return (followed, Err(err)),
            Ok(Some(f)) => followed.push(format!("{} -> {f}", e.path)),
            Ok(None) => {}
        }
    }
    (followed, Ok(()))
}

/// The gate extensions, byte for byte the files Python ships.
pub const PI_EXTENSION: &str = include_str!("../../../../src/opendaisugi/harness_pi/extension/index.ts");
pub const OPENCODE_PLUGIN: &str = include_str!("../../../../src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts");

/// `install.pi_extension_dir`.
pub fn pi_dir(home: &str) -> String {
    join(home, ".pi/agent/extensions/daisugi-gate")
}

/// `install.opencode_plugin_path`: under $XDG_CONFIG_HOME when it is
/// absolute, else ~/.config.
pub fn opencode_path(home: &str, env: &HashMap<String, String>) -> String {
    let base = match env.get("XDG_CONFIG_HOME") {
        Some(x) if x.starts_with('/') => path_str(x),
        _ => join(home, ".config"),
    };
    join(&base, "opencode/plugins/daisugi-gate.ts")
}

/// `install.harness_extension_target`.
pub fn target(name: &str, home: &str, env: &HashMap<String, String>) -> String {
    if name == "opencode" {
        opencode_path(home, env)
    } else {
        join(&pi_dir(home), "index.ts")
    }
}

/// Why a harness extension was not written: the ValueError
/// `_install_opencode_plugin` raises, or an OS error.
pub enum HarnessErr {
    Value(String),
    Io(std::io::Error),
}

/// `install_harness_extension`: the file written unless it already holds
/// the same text; a symlink at the file is replaced, never written
/// through.
pub fn install_harness(name: &str, home: &str, env: &HashMap<String, String>) -> Result<Vec<String>, HarnessErr> {
    let out = target(name, home, env);
    let text = if name == "opencode" { OPENCODE_PLUGIN } else { PI_EXTENSION };
    if name == "opencode" {
        let p = parent(&out);
        if gateroot::is_link(&p) {
            return Err(HarnessErr::Value(format!(
                "{p} is a symlink, so the plugin would land somewhere else. Replace it with a directory, then run this again."
            )));
        }
    }
    mkdir_all(&parent(&out)).map_err(HarnessErr::Io)?;
    if gateroot::is_link(&out) {
        fs::remove_file(&out).map_err(HarnessErr::Io)?;
    }
    match fs::read(&out) {
        Ok(cur) if cur == text.as_bytes() => return Ok(vec![]),
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(HarnessErr::Io(e)),
    }
    gateroot::replace(&out, text).map_err(HarnessErr::Io)?;
    Ok(vec![out])
}

/// `uninstall_harness_extension`: pi's whole extension directory, or
/// OpenCode's one plugin file.
pub fn uninstall_harness(name: &str, home: &str, env: &HashMap<String, String>) -> std::io::Result<Vec<String>> {
    if name == "pi" {
        let d = pi_dir(home);
        if !gateroot::is_link(&d) && !gateroot::exists(&d) {
            return Ok(vec![]);
        }
        if gateroot::is_dir_no_follow(&d) {
            fs::remove_dir_all(&d)?;
        } else {
            fs::remove_file(&d)?;
        }
        return Ok(vec![d]);
    }
    let out = opencode_path(home, env);
    if gateroot::is_link(&out) || gateroot::exists(&out) {
        fs::remove_file(&out)?;
        return Ok(vec![out]);
    }
    Ok(vec![])
}

// A binary that mise installed runs from a versioned path, such as
// ~/.local/share/mise/installs/github-.../0.44.0/daisugi. A hook that
// names that path breaks when mise installs the next version and removes
// the old one: enforce mode then denies every call, shadow mode stops
// seeing them. mise's shim for daisugi, or the stub Omarchy writes in
// ~/.local/bin, stays in one place and runs whichever version is current,
// so the hook names it instead, when there is one and it runs this very
// binary. The Go install does the same (install.HookPath).

/// mise's data and shims directories, as mise documents them
/// (https://mise.jdx.dev/directories.html): MISE_DATA_DIR, else
/// $XDG_DATA_HOME/mise, else ~/.local/share/mise; the shims in
/// MISE_SHIMS_DIR, else <data>/shims.
pub fn mise_dirs(env: &HashMap<String, String>) -> (String, String) {
    let get = |k: &str| env.get(k).filter(|v| !v.is_empty()).cloned();
    let data = get("MISE_DATA_DIR")
        .or_else(|| get("XDG_DATA_HOME").map(|x| go_clean(&format!("{x}/mise"))))
        .or_else(|| get("HOME").map(|h| go_clean(&format!("{h}/.local/share/mise"))))
        .unwrap_or_default();
    let shims = get("MISE_SHIMS_DIR").unwrap_or_else(|| if data.is_empty() { String::new() } else { go_clean(&format!("{data}/shims")) });
    (data, shims)
}

/// `filepath.Clean`: no empty or "." parts, ".." folded, no trailing "/".
fn go_clean(p: &str) -> String {
    let rooted = p.starts_with('/');
    let mut out: Vec<&str> = vec![];
    for c in p.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                if out.last().is_some_and(|l| *l != "..") {
                    out.pop();
                } else if !rooted {
                    out.push("..");
                }
            }
            _ => out.push(c),
        }
    }
    let body = out.join("/");
    match (rooted, body.is_empty()) {
        (true, _) => format!("/{body}"),
        (false, true) => ".".into(),
        (false, false) => body,
    }
}

fn real(p: &str) -> Option<String> {
    fs::canonicalize(p).ok().and_then(|p| p.into_os_string().into_string().ok())
}

/// The program a hook should run for `me`, and whether it is a versioned
/// mise path the hook must be written again for after an upgrade. For a
/// binary under mise's installs directory it is, in order: mise's daisugi
/// shim, when it exists and `mise which daisugi` names this same binary;
/// else a daisugi stub on PATH in $XDG_BIN_HOME or ~/.local/bin that execs
/// `mise x <tool> -- daisugi` (what omarchy-mise-install writes), when
/// `mise which daisugi`, or `mise which --tool <tool> daisugi`, names this
/// same binary. `which(tool)` runs `mise which`, with --tool when tool is
/// not empty. The Go install does the same (install.HookPath).
pub fn hook_path(me: &str, env: &HashMap<String, String>, which: &dyn Fn(&str) -> Option<String>) -> (String, bool) {
    let Some(me_real) = real(me) else { return (me.to_string(), false) };
    let (data, shims) = mise_dirs(env);
    let under = !data.is_empty()
        && real(&go_clean(&format!("{data}/installs"))).is_some_and(|d| me_real.starts_with(&format!("{d}/")));
    if !under {
        return (me.to_string(), false);
    }
    let same = |tool: &str| which(tool).and_then(|w| real(w.trim())).is_some_and(|t| t == me_real);
    let shim = go_clean(&format!("{shims}/daisugi"));
    if fs::metadata(&shim).is_ok() && same("") {
        return (shim, false);
    }
    if let Some((stub, tool)) = mise_stub(env) {
        if same("") || (!tool.is_empty() && same(&tool)) {
            return (stub, false);
        }
    }
    (me.to_string(), true)
}

fn stub_exec() -> &'static regex::Regex {
    static RE: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        regex::Regex::new(r#"(?m)\bmise["']?\s+(?:x|exec)\s+["']?([^\s"']+)["']?\s+--\s+["']?daisugi\b"#).unwrap()
    })
}

/// A daisugi stub that runs daisugi through mise: a script named daisugi
/// in $XDG_BIN_HOME or ~/.local/bin, in a directory on PATH, with a
/// `mise x <tool> -- daisugi` line. Its path and the tool, or None.
pub fn mise_stub(env: &HashMap<String, String>) -> Option<(String, String)> {
    let get = |k: &str| env.get(k).filter(|v| !v.is_empty()).cloned();
    let mut dirs = vec![];
    if let Some(x) = get("XDG_BIN_HOME") {
        dirs.push(go_clean(&x));
    }
    if let Some(h) = get("HOME") {
        dirs.push(go_clean(&format!("{h}/.local/bin")));
    }
    let on_path: Vec<String> =
        get("PATH").unwrap_or_default().split(':').filter(|d| !d.is_empty()).map(go_clean).collect();
    for d in dirs {
        if !on_path.contains(&d) {
            continue;
        }
        let p = format!("{d}/daisugi");
        let Ok(mut f) = fs::File::open(&p) else { continue };
        let mut head = vec![];
        if std::io::Read::read_to_end(&mut std::io::Read::take(&mut f, 64 << 10), &mut head).is_err() {
            continue;
        }
        let text = String::from_utf8_lossy(&head);
        if !text.starts_with("#!") {
            continue;
        }
        if let Some(m) = stub_exec().captures(&text) {
            return Some((p, m[1].to_string()));
        }
    }
    None
}

/// `mise which daisugi`, with `--tool tool` when tool is not empty, using
/// the mise on env's PATH, for at most five seconds.
pub fn mise_which(env: &HashMap<String, String>, tool: &str) -> Option<String> {
    use std::process::{Command, Stdio};
    use std::time::{Duration, Instant};
    let mise = look_path(env, "mise")?;
    let mut args = vec!["which"];
    if !tool.is_empty() {
        args.extend(["--tool", tool]);
    }
    args.push("daisugi");
    let mut child = Command::new(mise)
        .args(&args)
        .env_clear()
        .envs(env)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        match child.try_wait() {
            Ok(Some(st)) if st.success() => break,
            Ok(Some(_)) | Err(_) => return None,
            Ok(None) if Instant::now() >= deadline => {
                let _ = child.kill();
                let _ = child.wait();
                return None;
            }
            Ok(None) => std::thread::sleep(Duration::from_millis(20)),
        }
    }
    let mut out = String::new();
    std::io::Read::read_to_string(child.stdout.as_mut()?, &mut out).ok()?;
    Some(out)
}

/// The one line install prints when the hook keeps a versioned mise path.
pub fn versioned_note(path: &str) -> String {
    format!("note: {path} is a versioned mise install. Run daisugi install --gate again after mise upgrades daisugi.")
}
