//! `install`'s four default layers (skill, MCP, capture and instructions)
//! and their reverse, as `install.py` writes them, and the plan of every
//! layer in Python's apply order. The Go client's `install/layers.go` is
//! the twin.

use std::collections::HashSet;
use std::fs;

mod pandolayer;
pub use pandolayer::*;

use super::wardenroot::{self, join, mkdir_all, parent};
use super::install::{
    command_hooks, dump_settings, fail, plan, plan_claude_warden_on, plan_codex_warden, plan_pop_hook, read_json, Change,
    Edit, HookOptions, InstErr, ReadState, Runtime, Step, R,
};
use super::installrouter::{
    json_of, plan_claude_base_url, plan_codex_base_url_on, plan_codex_unpatch_on, plan_pop_env_key, plan_steps,
    read_text, strip_json5, DEFAULT_BASE_URL,
};
use super::words::{
    hook_command_programs, is_entry_path, is_entry_word, is_record_hook, is_warden_hook, record_hook_programs,
};
use crate::interpreter_parse::{shlex_quote, shlex_split};
use crate::warden::py::text::repr;
use crate::warden::pyjson::{dumps, loads, py_type_name, LoadError, Object, Value};

/// `install._SKILL_NAME`.
pub const SKILL_NAME: &str = "opendaisugi-checklist";

// The skill's files, byte for byte the package's.
include!("layer_assets.rs");

/// `install._CLAUDE_MD_MARKER`.
const MD_MARKER: &str = "<!-- opendaisugi-managed -->";

/// `install._CLAUDE_MD_BLOCK`.
fn md_block() -> String {
    format!(
        "{MD_MARKER}\n## openDaisugi — automatic pathway routing\n\nBefore planning any task with 3 or more steps, call the `find_pathway`\nMCP tool first. If similarity ≥ 0.85, use the returned cached plan via\n`run_plan` instead of re-planning from scratch. When a cached pathway is\nused, say so: \"Using cached opendaisugi pathway (similarity X.XX) —\nskipping re-plan.\"\n\nA job you will repeat goes through `verify_plan`, then `run_plan`. Make\none action in each tool call.\n\nIf no pathway matches, proceed normally.\n{MD_MARKER}\n"
    )
}

/// `install._CODEX_MCP_RE`: the block an install wrote, with any command (a
/// bare daisugi from an older install, or an absolute path).
fn codex_mcp_re() -> regex::Regex {
    regex::Regex::new(r#"\n\[mcp_servers\.opendaisugi\]\ncommand = "((?:[^"\\\n]|\\.)*)"\nargs = \["mcp", "serve"\]\n"#)
        .expect("a fixed pattern")
}

/// `install._codex_mcp_block`.
fn codex_mcp_block(self_path: &str) -> String {
    format!(
        "\n[mcp_servers.opendaisugi]\ncommand = {}\nargs = [\"mcp\", \"serve\"]\n",
        dumps(&Value::Str(self_path.into()), true)
    )
}

/// `install._record_command`: `<self> hook record <args>`.
fn record_command(self_path: &str, args: &str) -> String {
    format!("{} hook record {args}", shlex_quote(self_path))
}

/// `install.LEFT_AS_IS`: a note that says a hook was not rewritten contains
/// this phrase.
pub const LEFT_AS_IS: &str = "was left as it is";

/// `install._PLAIN_RECORD`: one plain command, a program word (bare, or one
/// single-quoted path) and plain words. No operator, redirection, quote,
/// expansion, newline or NAME=val prefix can match.
fn plain_record_re() -> regex::Regex {
    regex::Regex::new(
        r#"^([ \t]*)('[^']*'|[^ \t\n\r\f\v'"\\;|&<>()$`]+)((?:[ \t]+[A-Za-z0-9_./:=@%+,-]+)*[ \t]*)$"#,
    )
    .expect("a fixed pattern")
}

/// `install._rebind_record_command`: a daisugi hook record command pointed at
/// self, and whether it had to be left alone. Only one plain command is
/// rewritten, and only its program word; every other byte stays. A command
/// with an operator, a redirection, an env or NAME=val prefix, a quote or an
/// expansion comes back as it was with `left` true, unless its program is
/// already self. Any other command comes back as it was with `left` false.
fn rebind_record_command(command: &str, self_path: &str) -> (String, bool) {
    let programs = record_hook_programs(&Value::Str(command.into()));
    if programs.is_empty() {
        return (command.to_string(), false);
    }
    if let Some(m) = plain_record_re().captures(command) {
        let prog = &m[2];
        let word = if prog.starts_with('\'') { &prog[1..prog.len() - 1] } else { prog };
        let tail: Vec<&str> = m[3].split_whitespace().collect();
        if is_entry_path(word) && tail.len() >= 2 && tail[0] == "hook" && tail[1] == "record" {
            if word == self_path {
                return (command.to_string(), false);
            }
            if word.contains('/') && !self_path.contains('/') {
                // Install found no path to write. A bare word is less than
                // the path the hook has now: keep the path and say so.
                return (command.to_string(), true);
            }
            return (format!("{}{}{}", &m[1], shlex_quote(self_path), &m[3]), false);
        }
    }
    (command.to_string(), programs.iter().any(|p| p != self_path))
}

/// `install._keeps_path`: install would write a bare word over a command
/// that names a path. It never does.
fn keeps_path(old: &Value, want: &Value) -> bool {
    matches!((old, want), (Value::Str(o), Value::Str(w)) if o.contains('/') && !w.contains('/'))
}

/// `install._left_mcp_note`.
fn left_mcp_note(p: &str, old: &str, want: &str) -> String {
    format!(
        "The MCP command `{old}` in {p} {LEFT_AS_IS}: install found no daisugi on PATH, \
         so the only word it could write is `{want}`, and the entry already names a path."
    )
}

/// `install._left_hook_note`.
fn left_hook_note(runtime: &str, command: &str, self_path: &str) -> String {
    if plain_record_re().captures(command).is_some_and(|m| m[2].trim_matches('\'').contains('/')) && !self_path.contains('/') {
        return format!(
            "[{runtime}] The hook `{command}` {LEFT_AS_IS}: install found no daisugi on PATH, \
             so the only word it could write is `{self_path}`, and the hook already names a path."
        );
    }
    format!(
        "[{runtime}] The hook `{command}` {LEFT_AS_IS}: it is not one plain command, so install did not rewrite it. \
         If the agent cannot find daisugi, make its first word {}.",
        shlex_quote(self_path)
    )
}

/// `install._rebind_hook_entries`: every record hook in a list of
/// `{"hooks": [...]}` entries (or flat `{"command": ...}` entries) pointed
/// at self. It says whether one changed, and gives a note for each hook it
/// left as it is.
fn rebind_hook_entries(entries: &mut [Value], self_path: &str, runtime: &str) -> (bool, Vec<String>) {
    fn one(h: &mut Value, self_path: &str, runtime: &str, notes: &mut Vec<String>) -> bool {
        let Value::Obj(ho) = h else { return false };
        let Value::Str(cmd) = ho.value("command").clone() else { return false };
        if !is_record_hook(&Value::Str(cmd.clone()), "") {
            return false;
        }
        let (n, left) = rebind_record_command(&cmd, self_path);
        if left {
            notes.push(left_hook_note(runtime, &cmd, self_path));
        }
        if n == cmd {
            return false;
        }
        ho.set("command", n.as_str());
        true
    }
    let mut changed = false;
    let mut notes = vec![];
    for e in entries.iter_mut() {
        let Value::Obj(eo) = e else { continue };
        if eo.get("command").is_some() {
            let mut me = Value::Obj(eo.clone());
            if one(&mut me, self_path, runtime, &mut notes) {
                *eo = match me {
                    Value::Obj(o) => o,
                    _ => unreachable!(),
                };
                changed = true;
            }
        } else if let Some(Value::List(hs)) = eo.get_mut("hooks") {
            for h in hs.iter_mut() {
                changed |= one(h, self_path, runtime, &mut notes);
            }
        }
    }
    (changed, notes)
}

/// The edit of a file with the notes of left hooks on it, or an edit that
/// only carries them when nothing is written.
fn with_notes(e: Option<Edit>, p: &str, notes: &[String]) -> Option<Edit> {
    if notes.is_empty() {
        return e;
    }
    let text = notes.join("\n");
    match e {
        None => Some(Edit { path: p.into(), note_only: true, note: text, ..Edit::default() }),
        Some(mut e) => {
            e.note = if e.note.is_empty() { text } else { format!("{}\n{text}", e.note) };
            Some(e)
        }
    }
}

/// The SKILL.md `install --print-skill` prints.
pub fn skill_text() -> &'static str {
    SKILL_FILES.iter().find(|(r, _)| *r == "SKILL.md").map(|(_, t)| *t).unwrap_or("")
}

fn lexists(p: &str) -> bool {
    wardenroot::lexists(p)
}

/// `install._agents_skill_target`.
pub fn agents_skill_target(home: &str, fallback: &str) -> String {
    let agents = join(home, ".agents/skills");
    if lexists(&agents) || !lexists(&join(home, fallback)) {
        return join(&agents, SKILL_NAME);
    }
    join(&join(home, fallback), SKILL_NAME)
}

/// `install.RESTART_LINE`: said after an install writes.
pub const RESTART_LINE: &str =
    "Restart your agent so the hook loads. To check it works, use one tool, then run: daisugi hook list";

/// `install._walk_commands`: the program of every hook command in a
/// settings tree.
fn walk_commands(node: &Value, out: &mut Vec<String>) {
    match node {
        Value::Obj(o) => {
            if let Value::Str(_) = o.value("command") {
                out.extend(hook_command_programs(o.value("command")));
            }
            for (_, v) in o.iter() {
                walk_commands(v, out);
            }
        }
        Value::List(l) => {
            for v in l {
                walk_commands(v, out);
            }
        }
        _ => {}
    }
}

/// `install._mcp_command`: the command text at a key path.
fn mcp_command(node: &Value, path: &[&str]) -> Vec<String> {
    let mut node = node;
    for k in path {
        match node {
            Value::Obj(o) => node = o.value(k),
            _ => return vec![],
        }
    }
    match node {
        Value::Obj(o) => match o.value("command") {
            Value::Str(c) if !c.is_empty() => vec![c.clone()],
            _ => vec![],
        },
        _ => vec![],
    }
}

/// A JSON (or, with `json5`, a JSON5) file as a value; Null when it is not
/// there or does not read.
fn read_json_file(p: &str, json5: bool) -> Value {
    let Ok(text) = fs::read_to_string(p) else { return Value::Null };
    let text = if json5 { strip_json5(&text) } else { text };
    loads(&text).unwrap_or(Value::Null)
}

/// `install.written_programs`: the program word of each hook and MCP command
/// a runtime's config holds on disk, in file order, without repeats. It reads
/// the files, not what the install meant to write.
pub fn written_programs(rt: &Runtime, home: &str) -> Vec<String> {
    let j = |rel: &str| join(home, rel);
    let mut words: Vec<String> = vec![];
    match rt.key {
        "claude" => {
            walk_commands(&read_json_file(&j(".claude/settings.json"), false), &mut words);
            words.extend(mcp_command(&read_json_file(&j(".claude.json"), false), &["mcpServers", "opendaisugi"]));
        }
        "codex" => {
            if let Ok(text) = fs::read_to_string(j(".codex/config.toml")) {
                if let Some(m) = codex_mcp_re().captures(&format!("\n{text}")) {
                    if let Ok(Value::Str(s)) = loads(&format!("\"{}\"", &m[1])) {
                        words.push(s);
                    }
                }
            }
            walk_commands(&read_json_file(&j(".codex/hooks.json"), false), &mut words);
        }
        "hermes" => {
            if let Ok(Some(Some(cfg))) = yaml_of(&j(".hermes/config.yaml")) {
                words.extend(mcp_command(&Value::Obj(cfg.clone()), &["mcp_servers", "opendaisugi"]));
                walk_commands(cfg.value("hooks"), &mut words);
            }
        }
        "openclaw" => {
            words.extend(mcp_command(
                &read_json_file(&j(".openclaw/openclaw.json"), true),
                &["mcp", "servers", "opendaisugi"],
            ));
        }
        _ => {}
    }
    let mut seen = HashSet::new();
    words.retain(|w| seen.insert(w.clone()));
    words
}

/// `install._why_unrunnable`: why the agent cannot start a program word, or
/// None when it can.
fn why_unrunnable(word: &str) -> Option<&'static str> {
    use std::os::unix::fs::PermissionsExt;
    if word.contains('/') {
        return match fs::metadata(word) {
            Ok(m) if m.is_file() => {
                if m.permissions().mode() & 0o111 == 0 || !executable_by_us(word) {
                    Some("is not executable")
                } else {
                    None
                }
            }
            _ => Some("is not there"),
        };
    }
    if std::env::var("PATH").unwrap_or_default().split(':').any(|d| {
        fs::metadata(format!("{d}/{word}")).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0)
    }) {
        None
    } else {
        Some("is not on the PATH")
    }
}

/// `install.unrunnable_commands`: one plain line for each runtime whose hook
/// or MCP command, read back from the config on disk, names a program the
/// agent cannot start. Empty when every one can run.
pub fn unrunnable_commands(runtimes: &[&Runtime], home: &str) -> Vec<String> {
    let mut out = vec![];
    for rt in runtimes {
        let bad: Vec<String> = written_programs(rt, home)
            .iter()
            .filter_map(|w| why_unrunnable(w).map(|why| format!("The command {w} {why}")))
            .collect();
        if !bad.is_empty() {
            out.push(format!(
                "[{}] {}, so the agent cannot start its hook or MCP server. Install daisugi there, then run: daisugi install",
                rt.name,
                bad.join(", and ")
            ));
        }
    }
    out
}

/// `install.temporary_exe_note`: one line when the entry point lives in uv's
/// cache, where a cache clean removes it. None otherwise.
pub fn temporary_exe_note(self_path: &str) -> Option<String> {
    let mut paths = vec![self_path.to_string()];
    if let Ok(real) = fs::canonicalize(self_path) {
        paths.push(real.to_string_lossy().into_owned());
    }
    if paths.iter().any(|p| p.contains("/.cache/uv/")) {
        return Some(format!(
            "The path {self_path} is temporary: it is in uv's cache, and a cache clean removes it. Install daisugi \
             with: uv tool install opendaisugi, then run daisugi install again."
        ));
    }
    None
}

/// `os.access(path, os.X_OK)`.
fn executable_by_us(path: &str) -> bool {
    match std::ffi::CString::new(path) {
        Ok(c) => unsafe { libc::access(c.as_ptr(), libc::X_OK) == 0 },
        Err(_) => false,
    }
}

/// The layers an install writes beyond the four default ones.
#[derive(Clone, Default)]
pub struct Opts {
    pub router: bool,
    pub enforce: bool,
    pub ask: bool,
    pub url: String,
    /// The floor report the warden layer wires ("", "herdr" or "coppice").
    pub report: String,
    /// The layers to write (`--only`); `None` means the default five. The
    /// router layer is `router`, not an entry here.
    pub only: Option<Vec<String>>,
}

impl Opts {
    /// Whether the layer is selected.
    pub fn has(&self, layer: &str) -> bool {
        self.only.as_ref().map_or(true, |o| o.iter().any(|l| l == layer))
    }
}

fn skill_target(rt: &Runtime, home: &str) -> String {
    match rt.key {
        "claude" => agents_skill_target(home, ".claude/skills"),
        "codex" => agents_skill_target(home, ".codex/skills"),
        "hermes" => join(home, &format!(".hermes/skills/opendaisugi/{SKILL_NAME}")),
        _ => join(home, &format!(".openclaw/workspace/skills/{SKILL_NAME}")),
    }
}

/// `Runtime.plan` for the default layers and the opt-in ones: the
/// supported steps in layer order, then the gap notes.
pub fn plan_all_steps(rt: &Runtime, home: &str, o: &Opts) -> Vec<Step> {
    let j = |rel: &str| join(home, rel);
    let mut steps = vec![];
    let step = |layer: &'static str, d: &str, t: String| Step { layer, description: d.into(), target: t, supported: true };
    // A step is added only for a selected layer.
    let mut layer = |l: &'static str, d: &str, t: String| {
        if o.has(l) {
            steps.push(step(l, d, t));
        }
    };
    layer("skill", "Copy opendaisugi-checklist skill", skill_target(rt, home));
    match rt.key {
        "claude" => {
            layer("mcp", "Register MCP server \"opendaisugi\"", j(".claude.json"));
            layer("capture", "Add PreToolUse capture hook", j(".claude/settings.json"));
            layer("instructions", pathway_guidance_title(&j(".claude/CLAUDE.md")), j(".claude/CLAUDE.md"));
        }
        "codex" => {
            layer("mcp", "Register opendaisugi MCP server", j(".codex/config.toml"));
            layer("instructions", pathway_guidance_title(&j(".codex/AGENTS.md")), j(".codex/AGENTS.md"));
        }
        "hermes" => {
            layer("mcp", "Register opendaisugi MCP server", j(".hermes/config.yaml"));
            layer("capture", "Add pre_tool_call capture hook", j(".hermes/config.yaml"));
            layer("instructions", pathway_guidance_title(&j(".hermes/SOUL.md")), j(".hermes/SOUL.md"));
        }
        _ => {
            layer("mcp", "Register opendaisugi MCP server", j(".openclaw/openclaw.json"));
            layer("capture", "Install before_tool_call capture plugin", j(".openclaw/extensions/opendaisugi"));
            layer("instructions", pathway_guidance_title(&j(".openclaw/workspace/AGENTS.md")), j(".openclaw/workspace/AGENTS.md"));
        }
    }
    let mut gaps = vec![];
    for s in plan_steps(rt, home, o.router, o.enforce, o.ask, &o.url) {
        if s.layer == "warden" && !o.has("warden") {
            continue;
        }
        if !s.supported {
            gaps.push(s);
            continue;
        }
        let is_warden = s.layer == "warden";
        steps.push(s);
        if is_warden && rt.key == "claude" {
            if o.report == "herdr" {
                steps.push(step("warden", "Add Stop + Notification floor-report hooks (Herdr)", j(".claude/settings.json")));
            }
            if o.report == "herdr" || o.report == "coppice" {
                steps.push(step("warden", "Add SubagentStart + SubagentStop hooks (subagent rows)", j(".claude/settings.json")));
            }
        }
    }
    let _ = plan;
    if o.has_pando() {
        steps.extend(pando_plan_steps(rt, home));
    }
    steps.extend(gaps);
    steps
}

/// The last edit of `p` in the change, whose content later edits of `p`
/// are planned on.
fn prior_of(ch: &Change, p: &str) -> Option<Edit> {
    ch.edits.iter().rev().find(|e| e.path == p && e.kind.is_empty() && e.warning.is_empty() && !e.note_only).cloned()
}

fn text_of(p: &str, prior: Option<&Edit>) -> R<(String, bool)> {
    match prior {
        Some(e) => Ok((e.content.clone(), true)),
        None => read_text(p),
    }
}

fn load(text: &str) -> Result<Value, bool> {
    match loads(text) {
        Ok(v) => Ok(v),
        Err(LoadError::Syntax(_)) => Err(false),
        Err(_) => Err(true),
    }
}

fn mcp_entry(with_type: bool, self_path: &str) -> Object {
    let mut o = Object::new();
    if with_type {
        o.set("type", "stdio");
    }
    o.set("command", self_path);
    o.set("args", Value::List(vec![Value::Str("mcp".into()), Value::Str("serve".into())]));
    o
}

/// `_patch_mcp`: entry set at the key path in a JSON (or JSON5) file
/// unless one is there; a file that does not parse is left alone.
fn plan_mcp(p: &str, json5: bool, keys: &[&str], entry: Object, what: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    plan_mcp_named("opendaisugi", p, json5, keys, entry, what, prior)
}

/// `plan_mcp` for the server called name.
fn plan_mcp_named(name: &str, p: &str, json5: bool, keys: &[&str], entry: Object, what: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (text, existed) = text_of(p, prior)?;
    let mut cfg = Value::Obj(Object::new());
    if existed {
        cfg = match load(&text) {
            Ok(v) => v,
            Err(true) => return Err(InstErr::Unsupported),
            Err(false) if json5 => match load(&strip_json5(&text)) {
                Ok(v) => v,
                Err(true) => return Err(InstErr::Unsupported),
                Err(false) => return Ok(Some(warn(p, what))),
            },
            Err(false) => return Ok(Some(warn(p, what))),
        };
    }
    let mut leaf = &mut cfg;
    for k in keys {
        let Value::Obj(o) = leaf else {
            return fail(format!("'{}' object has no attribute 'setdefault'", py_type_name(leaf)));
        };
        if o.get(k).is_none() {
            o.set(k, Object::new());
        }
        leaf = o.get_mut(k).expect("just set");
    }
    let Value::Obj(lo) = leaf else { return Err(InstErr::Unsupported) };
    if lo.get(name).is_some() {
        // An entry from an older install names a bare daisugi. Point it at
        // the absolute program; every other key of it stays as it is.
        let want = entry.value("command").clone();
        let Some(Value::Obj(oo)) = lo.get_mut(name) else { return Ok(None) };
        if !is_entry_word(oo.value("command")) || *oo.value("command") == want {
            return Ok(None);
        }
        if keeps_path(oo.value("command"), &want) {
            if let (Value::Str(o), Value::Str(w)) = (oo.value("command"), &want) {
                return Ok(with_notes(None, p, &[left_mcp_note(p, o, w)]));
            }
        }
        oo.set("command", want);
    } else {
        lo.set(name, entry);
    }
    let mut e = Edit { path: p.into(), existed, content: dump_settings(&cfg), ..Edit::default() };
    if existed && json5 && (text.contains("//") || text.contains("/*")) {
        e.pre_warning = format!(
            "{p} contains JSON5 comments which will not survive the rewrite (the writer emits plain JSON). The \
             pre-write backup at {p}.bak* preserves the original text \u{2014} restore from it if you need the comments \
             back. Tracked as M7 in REVIEW_FINDINGS.md."
        );
    }
    Ok(Some(e))
}

fn warn(p: &str, what: &str) -> Edit {
    Edit {
        path: p.into(),
        warning: format!("{p} is not valid; skipping {what} to avoid overwriting user state. Fix the file and re-run `daisugi install`."),
        ..Edit::default()
    }
}

/// A Claude settings.json for a capture or report edit, or the warning
/// edit when it does not parse.
fn settings_of(p: &str, prior: Option<&Edit>, warning: String) -> R<Result<(Object, bool), Edit>> {
    let (v, st) = json_of(p, prior)?;
    if st == ReadState::Bad {
        return Ok(Err(Edit { path: p.into(), warning, ..Edit::default() }));
    }
    let existed = st != ReadState::Missing;
    match v {
        _ if !existed => Ok(Ok((Object::new(), false))),
        Value::Obj(o) => Ok(Ok((o, existed))),
        other => fail(format!("'{}' object has no attribute 'setdefault'", py_type_name(&other))),
    }
}

/// `hooks.setdefault(event, [])` as a list.
fn event_list(s: &mut Object, event: &str) -> R<Vec<Value>> {
    if s.get("hooks").is_none() {
        s.set("hooks", Object::new());
    }
    let Some(Value::Obj(hooks)) = s.get_mut("hooks") else {
        return fail(format!("'{}' object has no attribute 'setdefault'", py_type_name(s.value("hooks"))));
    };
    if hooks.get(event).is_none() {
        hooks.set(event, Value::List(vec![]));
    }
    match hooks.value(event) {
        Value::List(l) => Ok(l.clone()),
        _ => Err(InstErr::Unsupported),
    }
}

fn set_event(s: &mut Object, event: &str, list: Vec<Value>) {
    if let Some(Value::Obj(hooks)) = s.get_mut("hooks") {
        hooks.set(event, Value::List(list));
    }
}

fn command_entry(cmd: &str) -> Object {
    let mut h = Object::new();
    h.set("type", "command");
    h.set("command", cmd);
    let mut e = Object::new();
    e.set("hooks", Value::List(vec![Value::Obj(h)]));
    e
}

/// `_patch_claude_settings`.
fn plan_claude_capture(p: &str, prior: Option<&Edit>, self_path: &str) -> R<Option<Edit>> {
    let (mut s, existed) = match settings_of(
        p,
        prior,
        format!(
            "{p} is not valid JSON; skipping hook registration to avoid overwriting your Claude Code settings \
             (permissions/env). Fix the file and re-run `daisugi install`."
        ),
    )? {
        Ok(x) => x,
        Err(w) => return Ok(Some(w)),
    };
    let mut pre = event_list(&mut s, "PreToolUse")?;
    let mut cmds = vec![];
    for h in command_hooks(&pre)? {
        let Some(c) = h.get("command") else { return fail("'command'".into()) };
        if matches!(c, Value::List(_) | Value::Obj(_)) {
            return fail(format!("unhashable type: '{}'", py_type_name(c)));
        }
        cmds.push(c.clone());
    }
    let mut notes: Vec<String> = vec![];
    let mut changed = !cmds.iter().any(|c| is_record_hook(c, ""));
    if changed {
        let mut e = command_entry(&record_command(self_path, "--format claude"));
        let hooks = e.value("hooks").clone();
        e = Object::new();
        e.set("matcher", "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch");
        e.set("hooks", hooks);
        pre.push(Value::Obj(e));
        set_event(&mut s, "PreToolUse", pre);
    } else {
        let (c, n) = rebind_hook_entries(&mut pre, self_path, "Claude Code");
        notes.extend(n);
        if c {
            set_event(&mut s, "PreToolUse", pre);
            changed = true;
        }
    }
    // The v0.27.1 print-skill SessionStart hook is taken out.
    let ss = s.value("hooks").as_obj().map(|h| h.value("SessionStart").clone()).unwrap_or(Value::Null);
    if let Value::List(ss) = ss {
        let is_old = |h: &Value| h.as_obj().map(|o| o.value("command")) == Some(&Value::Str("daisugi install --print-skill".into()));
        let mut found = false;
        for e in &ss {
            let Value::Obj(eo) = e else { return Err(InstErr::Unsupported) };
            if let Value::List(hs) = eo.value("hooks") {
                found |= hs.iter().any(is_old);
            }
        }
        if found {
            let mut kept = vec![];
            for e in ss {
                let Value::Obj(mut eo) = e else { return Err(InstErr::Unsupported) };
                let hs = match eo.value("hooks") {
                    Value::List(hs) => hs.clone(),
                    _ => vec![],
                };
                let left: Vec<Value> = hs.into_iter().filter(|h| !is_old(h)).collect();
                let keep = !left.is_empty();
                eo.set("hooks", Value::List(left));
                if keep {
                    kept.push(Value::Obj(eo));
                }
            }
            if let Some(Value::Obj(hooks)) = s.get_mut("hooks") {
                if kept.is_empty() {
                    hooks.remove("SessionStart");
                } else {
                    hooks.set("SessionStart", Value::List(kept));
                }
            }
            changed = true;
        }
    }
    if !changed {
        return Ok(with_notes(None, p, &notes));
    }
    Ok(with_notes(
        Some(Edit { path: p.into(), existed, content: dump_settings(&Value::Obj(s)), ..Edit::default() }),
        p,
        &notes,
    ))
}

/// `_patch_claude_report_hooks` (Stop and Notification) or
/// `_patch_claude_subagent_hooks`.
fn plan_report_hooks(p: &str, prior: Option<&Edit>, subagent: bool, self_path: &str) -> R<Option<Edit>> {
    let what = if subagent { "subagent hook" } else { "floor-report hook" };
    let (mut s, existed) = match settings_of(
        p,
        prior,
        format!(
            "{p} is not valid JSON; skipping {what} registration to avoid overwriting your Claude Code settings. Fix \
             the file and re-run `daisugi install`."
        ),
    )? {
        Ok(x) => x,
        Err(w) => return Ok(Some(w)),
    };
    let evs: &[(&str, &str)] = if subagent {
        &[("SubagentStart", "subagent_start"), ("SubagentStop", "subagent_stop")]
    } else {
        &[("Stop", "stop"), ("Notification", "notification")]
    };
    let mut changed = false;
    let mut notes: Vec<String> = vec![];
    for (name, word) in evs {
        let mut list = event_list(&mut s, name)?;
        let have = command_hooks(&list)?.iter().any(|h| is_record_hook(h.value("command"), word));
        if !have {
            list.push(Value::Obj(command_entry(&record_command(self_path, &format!("--format claude --event {word}")))));
            set_event(&mut s, name, list);
            changed = true;
        } else {
            let (c, n) = rebind_hook_entries(&mut list, self_path, "Claude Code");
            notes.extend(n);
            if c {
                set_event(&mut s, name, list);
                changed = true;
            }
        }
    }
    if !changed {
        return Ok(with_notes(None, p, &notes));
    }
    Ok(with_notes(
        Some(Edit { path: p.into(), existed, content: dump_settings(&Value::Obj(s)), ..Edit::default() }),
        p,
        &notes,
    ))
}

/// Where the marker stands alone on its line (LF or CRLF ends). A marker
/// inside a line of prose is not one.
fn marker_starts(text: &str) -> Vec<usize> {
    let mut out = Vec::new();
    let mut from = 0;
    while let Some(i) = text[from..].find(MD_MARKER) {
        let i = from + i;
        let after = i + MD_MARKER.len();
        let at_start = i == 0 || text.as_bytes()[i - 1] == b'\n';
        let at_end = after == text.len() || text.as_bytes()[after] == b'\n' || text[after..].starts_with("\r\n");
        if at_start && at_end {
            out.push(i);
        }
        from = after;
    }
    out
}

/// The (start, end) of the marker-bounded block in `text`: the first two
/// markers that stand alone on a line. One alone is not a block.
fn managed_span(text: &str) -> Option<(usize, usize)> {
    let st = marker_starts(text);
    if st.len() < 2 {
        return None;
    }
    Some((st[0], st[1] + MD_MARKER.len()))
}

/// The current block with the line ends of the block it replaces.
fn block_for(span_text: &str) -> String {
    let cur = md_block();
    let cur = cur.trim_end_matches('\n');
    if span_text.contains("\r\n") {
        cur.replace('\n', "\r\n")
    } else {
        cur.to_string()
    }
}

/// The plan line for the instruction layer: what the install does to the
/// file. Update when the file holds an older managed block, a keep line when
/// the current block is there already, else Append. A file that cannot be
/// read reads as no block.
fn pathway_guidance_title(p: &str) -> &'static str {
    if let Ok((text, _)) = read_text(p) {
        if let Some((s, e)) = managed_span(&text) {
            if text[s..e] != *block_for(&text[s..e]) {
                return "Update pathway guidance";
            }
            return "Keep pathway guidance (already current)";
        }
    }
    "Append pathway guidance"
}

/// `_patch_instructions`: the managed block appended once; an older block is
/// replaced in place and the rest of the file is kept byte for byte, line
/// ends included. The block takes the line ends of the one it replaces. It
/// makes the file's directory and keeps no backup.
fn plan_instructions(p: &str) -> R<Option<Edit>> {
    let (text, _) = read_text(p)?;
    if let Some((s, e)) = managed_span(&text) {
        let cur = block_for(&text[s..e]);
        if text[s..e] == *cur {
            return Ok(None);
        }
        let content = format!("{}{cur}{}", &text[..s], &text[e..]);
        return Ok(Some(Edit { path: p.into(), kind: "md".into(), content, ..Edit::default() }));
    }
    if !marker_starts(&text).is_empty() {
        return Ok(None);
    }
    let sep = if text.is_empty() { "" } else { "\n\n" };
    let content = format!("{}{sep}{}", text.trim_end_matches('\n'), md_block());
    Ok(Some(Edit { path: p.into(), kind: "md".into(), content, ..Edit::default() }))
}

/// `_patch_hermes_soul`: the managed block in Hermes's SOUL.md. A missing
/// SOUL.md is left alone, with a warning, because a file made here would
/// hold only the block and replace Hermes's built-in identity.
fn plan_hermes_soul(p: &str) -> R<Option<Edit>> {
    if !std::path::Path::new(p).exists() {
        return Ok(Some(Edit {
            path: p.into(),
            warning: format!("{p} does not exist yet; skipping the Hermes instructions. Start Hermes once, then re-run `daisugi install`."),
            ..Edit::default()
        }));
    }
    plan_instructions(p)
}

/// `_patch_codex_config`.
fn plan_codex_mcp(p: &str, self_path: &str) -> R<Option<Edit>> {
    let (text, existed) = read_text(p)?;
    let block = codex_mcp_block(self_path);
    if text.contains("[mcp_servers.opendaisugi]") {
        // An older install wrote a bare daisugi. That block is replaced by
        // the absolute one; a block the user edited is left as it is.
        let re = codex_mcp_re();
        let Some(caps) = re.captures(&text) else { return Ok(None) };
        let whole = caps.get(0).expect("group 0");
        if whole.as_str() == block {
            return Ok(None);
        }
        let Ok(old) = loads(&format!("\"{}\"", &caps[1])) else { return Ok(None) };
        if !is_entry_word(&old) {
            return Ok(None);
        }
        if keeps_path(&old, &Value::Str(self_path.into())) {
            if let Value::Str(o) = &old {
                return Ok(with_notes(None, p, &[left_mcp_note(p, o, self_path)]));
            }
        }
        let content = format!("{}{block}{}", &text[..whole.start()], &text[whole.end()..]);
        return Ok(Some(Edit { path: p.into(), existed, content, ..Edit::default() }));
    }
    let sep = if text.is_empty() { "" } else { "\n" };
    let content = format!("{}{sep}{block}", text.trim_end_matches('\n'));
    Ok(Some(Edit { path: p.into(), existed, content, ..Edit::default() }))
}

/// `yaml.safe_load(path.read_text()) or {}` for a Hermes config: None when
/// the file is absent, `Some(None)` when it is not valid YAML (a
/// yaml.YAMLError, which the oracle warns about and skips); a value this
/// binary does not model is refused.
fn yaml_of(p: &str) -> R<Option<Option<Object>>> {
    let (text, existed) = read_text(p)?;
    if !existed {
        return Ok(None);
    }
    yaml_of_text(&text)
}

/// `yaml_of` after the earlier edits of this run: the text of the last edit
/// of p when there is one.
fn yaml_of_prior(p: &str, prior: Option<&Edit>) -> R<Option<Option<Object>>> {
    match prior {
        Some(e) => yaml_of_text(&e.content),
        None => yaml_of(p),
    }
}

fn yaml_of_text(text: &str) -> R<Option<Option<Object>>> {
    // read_text reads with universal newlines.
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    let v = match crate::pyyaml::load(&text) {
        Ok(v) => v,
        Err(crate::pyyaml::Fail::Unsupported(_)) => return Err(InstErr::Unsupported),
        // Not a yaml.YAMLError: the oracle's except does not catch it.
        Err(crate::pyyaml::Fail::Exc(e)) if e.kind == "ValueError" => return Err(InstErr::Unsupported),
        Err(crate::pyyaml::Fail::Exc(_)) => return Ok(Some(None)),
    };
    let Some(v) = crate::pyyaml::to_json(&v) else { return Err(InstErr::Unsupported) };
    match v {
        _ if !v.truthy() => Ok(Some(Some(Object::new()))),
        Value::Obj(o) => Ok(Some(Some(o))),
        _ => Err(InstErr::Unsupported),
    }
}

fn yaml_text(o: &Object) -> R<String> {
    crate::pathways::yamldump::safe_dump(&Value::Obj(o.clone())).map_err(|_| InstErr::Unsupported)
}

/// `_patch_hermes_config`.
fn plan_hermes_config(p: &str, with_mcp: bool, with_capture: bool, self_path: &str) -> R<Option<Edit>> {
    let existed = wardenroot::lexists(p);
    let mut cfg = match yaml_of(p)? {
        None => Object::new(),
        Some(Some(o)) => o,
        Some(None) => {
            return Ok(Some(Edit {
                path: p.into(),
                warning: format!(
                    "{p} is not valid YAML; skipping to avoid overwriting your Hermes config. Fix the file and re-run `daisugi install`."
                ),
                ..Edit::default()
            }))
        }
    };
    let mut changed = false;
    let mut notes: Vec<String> = vec![];
    if with_mcp {
        if cfg.get("mcp_servers").is_none() {
            cfg.set("mcp_servers", Object::new());
        }
        let Some(Value::Obj(mcp)) = cfg.get_mut("mcp_servers") else { return Err(InstErr::Unsupported) };
        if mcp.get("opendaisugi").is_none() {
            mcp.set("opendaisugi", mcp_entry(false, self_path));
            changed = true;
        } else if let Some(Value::Obj(oo)) = mcp.get_mut("opendaisugi") {
            if is_entry_word(oo.value("command")) && *oo.value("command") != Value::Str(self_path.into()) {
                if keeps_path(oo.value("command"), &Value::Str(self_path.into())) {
                    if let Value::Str(o) = oo.value("command") {
                        notes.push(left_mcp_note(p, o, self_path));
                    }
                } else {
                    oo.set("command", self_path);
                    changed = true;
                }
            }
        }
    }
    if with_capture {
        let mut list = event_list(&mut cfg, "pre_tool_call")?;
        let cmd = record_command(self_path, "--format hermes");
        if !list.iter().any(|h| h.as_obj().map(|o| o.value("command")) == Some(&Value::Str(cmd.clone()))) {
            let known = list.iter().any(|h| h.as_obj().is_some_and(|o| is_record_hook(o.value("command"), "")));
            if known {
                let (c, n) = rebind_hook_entries(&mut list, self_path, "Hermes");
                notes.extend(n);
                if c {
                    set_event(&mut cfg, "pre_tool_call", list);
                    changed = true;
                }
            } else {
                let mut h = Object::new();
                h.set("matcher", ".*");
                h.set("command", cmd.as_str());
                h.set("timeout", Value::Int("10".into()));
                list.push(Value::Obj(h));
                set_event(&mut cfg, "pre_tool_call", list);
                changed = true;
            }
        }
    }
    if !changed {
        return Ok(with_notes(None, p, &notes));
    }
    Ok(with_notes(Some(Edit { path: p.into(), existed, content: yaml_text(&cfg)?, ..Edit::default() }), p, &notes))
}

/// The error a write into a directory that is not there raises.
fn missing_dir(p: &str) -> R<()> {
    if !wardenroot::is_dir(&parent(p)) {
        return fail(format!("[Errno 2] No such file or directory: {}", repr(p)));
    }
    Ok(())
}

fn written_needs_dir(r: R<Option<Edit>>, p: &str) -> R<Option<Edit>> {
    let e = r?;
    if matches!(&e, Some(e) if e.warning.is_empty()) {
        missing_dir(p)?;
    }
    Ok(e)
}

/// `Runtime.apply(home, layers, ...)` decided without writing: every edit
/// in Python's order. A step Python raises on fails the runtime, with the
/// edits before it kept (`partial`).
pub fn plan_install(rt: &Runtime, home: &str, self_path: &str, root: &str, o: &Opts) -> R<Change> {
    let j = |rel: &str| join(home, rel);
    let mut ch = Change::new(rt);
    match rt.key {
        "codex" => ch.mkdirs = vec![j(".codex")],
        "hermes" => ch.mkdirs = vec![j(".hermes")],
        "openclaw" => ch.mkdirs = vec![j(".openclaw/workspace")],
        _ => {}
    }
    let mode = if o.enforce { "enforce" } else { "audit" };
    let hook = |ask: bool| HookOptions { mode: mode.into(), root: root.into(), format: "claude".into(), captures_root: None, session: None, ask };
    let url = if o.url.is_empty() { DEFAULT_BASE_URL.to_string() } else { o.url.clone() };
    type Step<'a> = Box<dyn Fn(&Change) -> R<Option<Edit>> + 'a>;
    let mut steps: Vec<Step> = vec![];
    let target = skill_target(rt, home);
    if o.has("skill") {
        steps.push(Box::new(move |_| Ok(Some(Edit { path: target.clone(), kind: "skill".into(), listed: true, ..Edit::default() }))));
    }
    let settings = j(".claude/settings.json");
    match rt.key {
        "claude" => {
            if o.has("mcp") {
                let cj = j(".claude.json");
                steps.push(Box::new(move |ch| plan_mcp(&cj, false, &["mcpServers"], mcp_entry(true, self_path), "MCP registration", prior_of(ch, &cj).as_ref())));
            }
            if o.has("capture") {
                let s1 = settings.clone();
                steps.push(Box::new(move |ch| written_needs_dir(plan_claude_capture(&s1, prior_of(ch, &s1).as_ref(), self_path), &s1)));
            }
            if o.has("instructions") {
                let md = j(".claude/CLAUDE.md");
                steps.push(Box::new(move |_| plan_instructions(&md)));
            }
            if o.has("warden") {
                let s2 = settings.clone();
                let entry = super::install::hook_entry(self_path, &hook(o.ask));
                let r1 = root.to_string();
                steps.push(Box::new(move |ch| {
                    super::install::check_warden_root(&r1)?;
                    written_needs_dir(plan_claude_warden_on(&s2, &entry, prior_of(ch, &s2).as_ref()), &s2)
                }));
                if o.report == "herdr" {
                    let s3 = settings.clone();
                    steps.push(Box::new(move |ch| plan_report_hooks(&s3, prior_of(ch, &s3).as_ref(), false, self_path)));
                }
                if o.report == "herdr" || o.report == "coppice" {
                    let s4 = settings.clone();
                    steps.push(Box::new(move |ch| plan_report_hooks(&s4, prior_of(ch, &s4).as_ref(), true, self_path)));
                }
            }
            if o.router {
                let s5 = settings.clone();
                let u = url.clone();
                steps.push(Box::new(move |ch| written_needs_dir(plan_claude_base_url(&s5, &u, prior_of(ch, &s5).as_ref()), &s5)));
            }
        }
        "codex" => {
            let toml = j(".codex/config.toml");
            if o.has("mcp") {
                let t1 = toml.clone();
                steps.push(Box::new(move |_| plan_codex_mcp(&t1, self_path)));
            }
            if o.has("instructions") {
                let md = j(".codex/AGENTS.md");
                steps.push(Box::new(move |_| plan_instructions(&md)));
            }
            if o.has("warden") {
                let hj = j(".codex/hooks.json");
                let entry = super::install::hook_entry(self_path, &hook(false));
                let r2 = root.to_string();
                steps.push(Box::new(move |_| {
                    super::install::check_warden_root(&r2)?;
                    plan_codex_warden(&hj, &entry)
                }));
            }
            if o.router {
                let u = url.clone();
                steps.push(Box::new(move |ch| plan_codex_base_url_on(&toml, &u, prior_of(ch, &toml).as_ref())));
            }
        }
        "hermes" => {
            if o.has("mcp") || o.has("capture") {
                let y = j(".hermes/config.yaml");
                let (with_mcp, with_capture) = (o.has("mcp"), o.has("capture"));
                steps.push(Box::new(move |_| plan_hermes_config(&y, with_mcp, with_capture, self_path)));
            }
            if o.has("instructions") {
                let md = j(".hermes/SOUL.md");
                steps.push(Box::new(move |_| plan_hermes_soul(&md)));
            }
        }
        _ => {
            let cfg = j(".openclaw/openclaw.json");
            if o.has("mcp") {
                let c1 = cfg.clone();
                steps.push(Box::new(move |ch| plan_mcp(&c1, true, &["mcp", "servers"], mcp_entry(false, self_path), "MCP registration", prior_of(ch, &c1).as_ref())));
            }
            if o.has("instructions") {
                let md = j(".openclaw/workspace/AGENTS.md");
                steps.push(Box::new(move |_| plan_instructions(&md)));
            }
            if o.has("capture") {
                let plug = j(".openclaw/extensions/opendaisugi");
                steps.push(Box::new(move |_| Ok(Some(Edit { path: plug.clone(), kind: "plugin".into(), listed: true, ..Edit::default() }))));
            }
            if o.router {
                let u = url.clone();
                steps.push(Box::new(move |ch| {
                    let mut prov = Object::new();
                    prov.set("baseUrl", u.as_str());
                    prov.set("api", "anthropic-messages");
                    plan_mcp(&cfg, true, &["models", "providers"], prov, "router provider registration", prior_of(ch, &cfg).as_ref())
                }));
            }
        }
    }
    if o.has_pando() {
        steps.extend(pando_install_steps(rt, home, self_path));
    }
    for s in &steps {
        match s(&ch) {
            Err(InstErr::Fail(why)) => {
                ch.failed = true;
                ch.why = why;
                ch.partial = true;
                return Ok(ch);
            }
            Err(e) => return Err(e),
            Ok(Some(e)) => ch.edits.push(e),
            Ok(None) => {}
        }
    }
    Ok(ch)
}

/// `skill_paths._clear`: a symlink or file unlinked, a directory removed
/// with what is in it.
fn clear_path(p: &str) -> std::io::Result<bool> {
    match fs::symlink_metadata(p) {
        Err(_) => Ok(false),
        Ok(m) if m.is_dir() => fs::remove_dir_all(p).map(|_| true),
        Ok(_) => fs::remove_file(p).map(|_| true),
    }
}

/// Whether `target` already holds this binary's skill, file for file.
fn same_skill(target: &str) -> bool {
    if !fs::symlink_metadata(target).is_ok_and(|m| m.is_dir()) {
        return false;
    }
    fn walk(dir: &str, base: &str, out: &mut Vec<(String, Vec<u8>)>) -> bool {
        let Ok(rd) = fs::read_dir(dir) else { return false };
        for e in rd.flatten() {
            let p = e.path().to_string_lossy().into_owned();
            let Ok(m) = fs::symlink_metadata(&p) else { return false };
            if m.file_type().is_symlink() {
                return false;
            }
            if m.is_dir() {
                if !walk(&p, base, out) {
                    return false;
                }
            } else {
                let rel = p[base.len() + 1..].to_string();
                out.push((rel, fs::read(&p).unwrap_or_default()));
            }
        }
        true
    }
    let mut got = vec![];
    if !walk(target, target, &mut got) || got.len() != SKILL_FILES.len() {
        return false;
    }
    got.iter().all(|(rel, b)| SKILL_FILES.iter().any(|(r, t)| r == rel && t.as_bytes() == b.as_slice()))
}

/// Performs the edits only this file plans.
pub fn apply_layer_edit(e: &Edit) -> std::io::Result<Option<String>> {
    match e.kind.as_str() {
        "skill" => {
            mkdir_all(&parent(&e.path))?;
            if same_skill(&e.path) {
                return Ok(None);
            }
            clear_path(&e.path)?;
            for (rel, text) in SKILL_FILES {
                let p = join(&e.path, rel);
                mkdir_all(&parent(&p))?;
                fs::write(&p, text)?;
            }
            Ok(None)
        }
        "plugin" => {
            mkdir_all(&e.path)?;
            for (rel, text) in OPENCLAW_PLUGIN {
                let p = join(&e.path, rel);
                if fs::symlink_metadata(&p).is_ok_and(|m| m.file_type().is_symlink()) {
                    fs::remove_file(&p)?;
                }
                wardenroot::write_file(&p, text)?;
            }
            Ok(None)
        }
        "md" => {
            mkdir_all(&parent(&e.path))?;
            wardenroot::write_file(&e.path, &e.content)
        }
        "remove" => clear_path(&e.path).map(|_| None),
        "md-unpatch" => {
            super::install::backup(&e.path)?;
            wardenroot::write_file(&e.path, &e.content)
        }
        "pando-plugin" | "pando-unit" | "pando-unit-remove" => apply_pando_edit(e).map(|_| None),
        k => Err(std::io::Error::other(format!("unknown edit kind {k}"))),
    }
}

fn plan_remove(p: &str, gone: &mut HashSet<String>) -> Option<Edit> {
    if gone.contains(p) || !lexists(p) {
        return None;
    }
    gone.insert(p.to_string());
    Some(Edit { path: p.into(), kind: "remove".into(), listed: true, ..Edit::default() })
}

/// `_unpatch_instructions`.
fn plan_unpatch_instructions(p: &str) -> R<Option<Edit>> {
    let (text, existed) = read_text(p).map_err(|_| InstErr::Unsupported)?;
    if !existed {
        return Ok(None);
    }
    let Some((start, end)) = managed_span(&text) else { return Ok(None) };
    let mut cleaned = format!("{}{}", &text[..start], &text[end..]).trim_end_matches('\n').to_string();
    if !cleaned.is_empty() {
        cleaned.push('\n');
    }
    Ok(Some(Edit { path: p.into(), kind: "md-unpatch".into(), content: cleaned, listed: true, ..Edit::default() }))
}

/// `_pop_json_mcp`.
fn plan_pop_mcp(p: &str, key: &str) -> R<Option<Edit>> {
    plan_pop_mcp_named("opendaisugi", p, key, None)
}

/// `_pop_json_mcp` for the server called name. A prior edit of the same file
/// in this change is what is popped from.
fn plan_pop_mcp_named(name: &str, p: &str, key: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (v, st) = match prior.filter(|e| !e.content.is_empty()) {
        Some(e) => (loads(&e.content).map_err(|_| InstErr::Unsupported)?, ReadState::Ok),
        None => match read_json(p) {
            Ok(x) => x,
            Err(InstErr::Fail(_)) => return Err(InstErr::Unsupported),
            Err(e) => return Err(e),
        },
    };
    if st != ReadState::Ok {
        return Ok(None);
    }
    let Value::Obj(mut cfg) = v else { return Err(InstErr::Unsupported) };
    let Some(m) = cfg.get_mut(key) else { return Ok(None) };
    let Value::Obj(m) = m else { return Err(InstErr::Unsupported) };
    if m.get(name).is_none() {
        return Ok(None);
    }
    m.remove(name);
    if m.is_empty() {
        cfg.remove(key);
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: dump_settings(&Value::Obj(cfg)), ..Edit::default() }))
}

/// The config.toml half of `CodexRuntime.reverse`.
fn plan_codex_mcp_remove(p: &str) -> R<Option<Edit>> {
    let (text, existed) = read_text(p).map_err(|_| InstErr::Unsupported)?;
    let re = codex_mcp_re();
    if !existed || !re.is_match(&text) {
        return Ok(None);
    }
    let mut cleaned = re.replace_all(&text, "").trim_end_matches('\n').to_string();
    if !cleaned.is_empty() {
        cleaned.push('\n');
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: cleaned, ..Edit::default() }))
}

/// The config.yaml half of `HermesRuntime.reverse`.
fn plan_hermes_reverse(p: &str, with_mcp: bool, with_capture: bool) -> R<Option<Edit>> {
    let Some(Some(mut cfg)) = yaml_of(p)? else { return Ok(None) };
    let mut changed = false;
    if let (true, Some(Value::Obj(servers))) = (with_mcp, cfg.get_mut("mcp_servers")) {
        if servers.get("opendaisugi").is_some() {
            servers.remove("opendaisugi");
            let empty = servers.is_empty();
            if empty {
                cfg.remove("mcp_servers");
            }
            changed = true;
        }
    }
    let mut drop_hooks = false;
    if let (true, Some(Value::Obj(hooks))) = (with_capture, cfg.get_mut("hooks")) {
        if let Some(Value::List(pre)) = hooks.get("pre_tool_call").cloned() {
            let kept: Vec<Value> =
                pre.iter().filter(|h| !h.as_obj().is_some_and(|o| is_record_hook(o.value("command"), ""))).cloned().collect();
            if kept.len() != pre.len() {
                changed = true;
                if kept.is_empty() {
                    hooks.remove("pre_tool_call");
                } else {
                    hooks.set("pre_tool_call", Value::List(kept));
                }
                drop_hooks = hooks.is_empty();
            }
        }
    }
    if drop_hooks {
        cfg.remove("hooks");
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: yaml_text(&cfg)?, ..Edit::default() }))
}

/// The openclaw.json half of `OpenClawRuntime.reverse`.
fn plan_openclaw_unconfig(p: &str, with_mcp: bool, with_base_url: bool) -> R<Option<Edit>> {
    let (text, existed) = read_text(p).map_err(|_| InstErr::Unsupported)?;
    if !existed {
        return Ok(None);
    }
    let v = match load(&text) {
        Ok(v) => v,
        Err(_) => match load(&strip_json5(&text)) {
            Ok(v) => v,
            Err(true) => return Err(InstErr::Unsupported),
            Err(false) => return Ok(None),
        },
    };
    let Value::Obj(mut cfg) = v else { return Ok(None) };
    let mut changed = false;
    for (outer, inner) in [("mcp", "servers"), ("models", "providers")] {
        if (outer == "mcp" && !with_mcp) || (outer == "models" && !with_base_url) {
            continue;
        }
        let Some(top) = cfg.get_mut(outer) else { continue };
        let Value::Obj(to) = top else { return Err(InstErr::Unsupported) };
        let Some(inn) = to.get_mut(inner) else { continue };
        let Value::Obj(io) = inn else { return Err(InstErr::Unsupported) };
        if io.get("opendaisugi").is_none() {
            continue;
        }
        io.remove("opendaisugi");
        if io.is_empty() {
            to.remove(inner);
            if to.is_empty() {
                cfg.remove(outer);
            }
        }
        changed = true;
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: dump_settings(&Value::Obj(cfg)), ..Edit::default() }))
}

/// `Runtime.reverse`: every managed change reversed, in Python's order. A
/// path an earlier runtime's reverse removed (`gone`) is not there by the
/// time this one runs.
pub fn plan_reverse_all(
    rt: &Runtime,
    home: &str,
    gone: &mut HashSet<String>,
    only: Option<&[String]>,
) -> R<Change> {
    let j = |rel: &str| join(home, rel);
    let mut ch = Change::new(rt);
    // Whether a layer is reversed: all of them, or the ones named.
    let has = |layer: &str| only.map_or(true, |o| o.iter().any(|l| l == layer));
    if (rt.key == "claude" || rt.key == "codex") && has("warden") {
        let f = if rt.key == "claude" { j(".claude/settings.json") } else { j(".codex/hooks.json") };
        if let Some(why) = super::install::unknown_warden_hook(&f) {
            ch.failed = true;
            ch.why = why;
            return Ok(ch);
        }
    }
    let any_record = |c: &Value| is_record_hook(c, "");
    let warden = |c: &Value| is_warden_hook(c);
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
        "claude" | "codex" => {
            let fallback = if rt.key == "claude" { ".claude/skills" } else { ".codex/skills" };
            if has("skill") {
                push(&mut ch, Ok(plan_remove(&j(&format!(".agents/skills/{SKILL_NAME}")), gone)))?;
                push(&mut ch, Ok(plan_remove(&join(&j(fallback), SKILL_NAME), gone)))?;
            }
            if rt.key == "claude" {
                let s = j(".claude/settings.json");
                if has("mcp") {
                    push(&mut ch, plan_pop_mcp(&j(".claude.json"), "mcpServers"))?;
                }
                if has("capture") {
                    let r = plan_pop_hook(&s, &any_record, &["PreToolUse"], prior_of(&ch, &s).as_ref());
                    push(&mut ch, r)?;
                }
                if has("warden") {
                    let r = plan_pop_hook(&s, &warden, &["PreToolUse"], prior_of(&ch, &s).as_ref());
                    push(&mut ch, r)?;
                    let r = plan_pop_hook(&s, &any_record, &["Stop", "Notification", "SubagentStart", "SubagentStop"], prior_of(&ch, &s).as_ref());
                    push(&mut ch, r)?;
                }
                if has("base_url") {
                    let r = plan_pop_env_key(&s, "ANTHROPIC_BASE_URL", prior_of(&ch, &s).as_ref());
                    push(&mut ch, r)?;
                }
                if has("instructions") {
                    push(&mut ch, plan_unpatch_instructions(&j(".claude/CLAUDE.md")))?;
                }
            } else {
                let toml = j(".codex/config.toml");
                if has("mcp") {
                    push(&mut ch, plan_codex_mcp_remove(&toml))?;
                }
                if has("warden") {
                    push(&mut ch, plan_pop_hook(&j(".codex/hooks.json"), &warden, &["PreToolUse"], None))?;
                }
                if has("base_url") {
                    let r = plan_codex_unpatch_on(&toml, prior_of(&ch, &toml).as_ref());
                    push(&mut ch, r)?;
                }
                if has("instructions") {
                    push(&mut ch, plan_unpatch_instructions(&j(".codex/AGENTS.md")))?;
                }
            }
        }
        "hermes" => {
            if has("skill") {
                push(&mut ch, Ok(plan_remove(&j(&format!(".hermes/skills/opendaisugi/{SKILL_NAME}")), gone)))?;
            }
            if has("instructions") {
                push(&mut ch, plan_unpatch_instructions(&j(".hermes/SOUL.md")))?;
            }
            if has("mcp") || has("capture") {
                push(&mut ch, plan_hermes_reverse(&j(".hermes/config.yaml"), has("mcp"), has("capture")))?;
            }
        }
        _ => {
            if has("skill") {
                push(&mut ch, Ok(plan_remove(&j(&format!(".openclaw/workspace/skills/{SKILL_NAME}")), gone)))?;
            }
            if has("mcp") || has("base_url") {
                push(&mut ch, plan_openclaw_unconfig(&j(".openclaw/openclaw.json"), has("mcp"), has("base_url")))?;
            }
            if has("capture") {
                push(&mut ch, Ok(plan_remove(&j(".openclaw/extensions/opendaisugi"), gone)))?;
            }
            if has("instructions") {
                push(&mut ch, plan_unpatch_instructions(&j(".openclaw/workspace/AGENTS.md")))?;
            }
        }
    }
    if has(LAYER_PANDO) {
        plan_pando_reverse(rt, home, gone, &mut ch)?;
    }
    Ok(ch)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The embedded skill and plugin are the files Python ships, every one.
    #[test]
    fn the_layer_assets_are_the_package_files() {
        for (dir, files) in [
            ("../../src/opendaisugi/skills/opendaisugi-checklist", SKILL_FILES),
            ("../../src/opendaisugi/install_assets/openclaw_plugin", OPENCLAW_PLUGIN),
        ] {
            let mut want = vec![];
            fn walk(d: &std::path::Path, base: &std::path::Path, out: &mut Vec<String>) {
                for e in std::fs::read_dir(d).unwrap().flatten() {
                    let p = e.path();
                    if p.is_dir() {
                        if p.file_name().unwrap() != "__pycache__" {
                            walk(&p, base, out);
                        }
                    } else {
                        out.push(p.strip_prefix(base).unwrap().to_string_lossy().into_owned());
                    }
                }
            }
            let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(dir);
            walk(&base, &base, &mut want);
            assert_eq!(want.len(), files.len(), "{dir}");
            for rel in want {
                let text = std::fs::read_to_string(base.join(&rel)).unwrap();
                assert!(files.iter().any(|(r, t)| *r == rel && *t == text), "{dir}/{rel}");
            }
        }
    }

    /// install._absolute_warden_root: a relative warden root is refused for the
    /// harnesses that bake a hook (claude, codex).
    #[test]
    fn a_relative_warden_root_is_refused_with_pythons_message() {
        let want = "the warden root 'rel/warden' is not an absolute path; set HOME, OPENDAISUGI_HOME or XDG_DATA_HOME to an absolute path";
        let o = Opts::default();
        // A real home, so the steps before the warden hook succeed. On disk
        // under the crate's target dir, not in /tmp.
        let home_dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("wardenroot-home-{}", std::process::id()));
        std::fs::create_dir_all(home_dir.join(".claude")).unwrap();
        std::fs::create_dir_all(home_dir.join(".codex")).unwrap();
        let home = home_dir.to_str().unwrap();
        for rt in &super::super::install::RUNTIMES[..2] {
            match plan_install(rt, home, "/bin/daisugi", "rel/warden", &o) {
                Err(super::super::install::InstErr::WardenRoot(m)) => assert_eq!(m, want, "{}", rt.key),
                other => panic!("{}: {:?}", rt.key, other.map(|c| c.edits.len())),
            }
            assert!(plan_install(rt, home, "/bin/daisugi", "/abs/warden", &o).is_ok());
        }
        let _ = std::fs::remove_dir_all(&home_dir);
    }

    fn scratch(tag: &str) -> std::path::PathBuf {
        let d = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("absolute-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn put(p: &std::path::Path, text: &str) {
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, text).unwrap();
    }

    fn rt(key: &str) -> Runtime {
        super::super::install::RUNTIMES.iter().find(|r| r.key == key).unwrap().clone()
    }

    fn edit_of<'a>(ch: &'a Change, name: &str) -> &'a Edit {
        ch.edits.iter().find(|e| e.path.ends_with(name)).unwrap_or_else(|| panic!("no edit of {name}"))
    }

    #[test]
    fn a_plain_command_swaps_only_its_program_word() {
        let exe = "/x/bin/daisugi";
        for (old, tail) in [
            ("daisugi hook record --format claude", " hook record --format claude"),
            ("/old/bin/daisugi-rs hook record --format claude", " hook record --format claude"),
            ("'/old path/daisugi' hook record --format claude", " hook record --format claude"),
            ("daisugi  hook record  --format   claude  ", "  hook record  --format   claude  "),
        ] {
            assert_eq!(rebind_record_command(old, exe), (format!("{exe}{tail}"), false), "{old}");
        }
        assert_eq!(
            rebind_record_command("daisugi hook record --format claude", "/opt/my tools/daisugi"),
            ("'/opt/my tools/daisugi' hook record --format claude".to_string(), false)
        );
    }

    #[test]
    fn anything_more_is_left_byte_for_byte_and_flagged() {
        for old in [
            "daisugi hook record --format claude || true",
            "daisugi hook record --format claude 2>/dev/null",
            "daisugi hook record --format claude >/dev/null",
            "daisugi hook record --format claude; exit 0",
            "daisugi hook record --format claude && true",
            "daisugi hook record --format claude | cat",
            "daisugi hook record --format claude &",
            "daisugi hook record --format claude $(true)",
            "daisugi hook record --format 'claude'",
            "env X=1 daisugi hook record --format claude",
            "X=1 daisugi hook record --format claude",
            "daisugi hook record --format claude\ntrue",
        ] {
            assert_eq!(rebind_record_command(old, "/x/bin/daisugi"), (old.to_string(), true), "{old:?}");
        }
    }

    #[test]
    fn a_path_is_never_rewritten_to_a_bare_word() {
        for old in [
            "/old/bin/daisugi hook record --format claude",
            "'/old path/daisugi-go' hook record --format claude",
        ] {
            assert_eq!(rebind_record_command(old, "daisugi"), (old.to_string(), true), "{old}");
        }
        assert_eq!(
            rebind_record_command("daisugi-go hook record --format claude", "daisugi"),
            ("daisugi hook record --format claude".to_string(), false)
        );
        assert_eq!(
            rebind_record_command("daisugi hook record --format claude", "daisugi"),
            ("daisugi hook record --format claude".to_string(), false)
        );
        assert_eq!(
            left_hook_note("Claude Code", "/old/bin/daisugi hook record --format claude", "daisugi"),
            "[Claude Code] The hook `/old/bin/daisugi hook record --format claude` was left as it is: install found no daisugi on PATH, so the only word it could write is `daisugi`, and the hook already names a path."
        );
        assert!(left_hook_note("Claude Code", "daisugi hook record || true", "daisugi").contains("not one plain command"));
    }

    #[test]
    fn a_command_that_needs_nothing_gets_no_note() {
        let exe = "/x/bin/daisugi";
        for c in [
            "/x/bin/daisugi hook record --format claude --event stop",
            "/x/bin/daisugi hook record --format claude || true",
            "env X=1 /x/bin/daisugi hook record --format claude",
            "other hook record",
            "daisugi-local hook record --format claude",
            "true || daisugi x",
        ] {
            assert_eq!(rebind_record_command(c, exe), (c.to_string(), false), "{c}");
        }
    }

    #[test]
    fn our_names_are_an_exact_set() {
        for (w, ours) in [
            ("daisugi", true),
            ("daisugi-py", true),
            ("daisugi-go", true),
            ("daisugi-rs", true),
            ("/x/y/daisugi-rs", true),
            ("daisugi-local", false),
            ("/home/u/bin/daisugi-local", false),
            ("daisugi-", false),
            ("daisugix", false),
            ("mydaisugi", false),
        ] {
            assert_eq!(is_entry_word(&Value::Str(w.into())), ours, "{w}");
        }
        assert!(!is_record_hook(&Value::Str("daisugi-local hook record --format claude".into()), ""));
    }

    #[test]
    fn a_compound_claude_hook_is_left_and_noted_with_its_runtime() {
        let home = scratch("compound");
        let old = "daisugi hook record --format claude || true";
        put(
            &home.join(".claude/settings.json"),
            &format!(r#"{{"hooks":{{"PreToolUse":[{{"hooks":[{{"type":"command","command":"{old}"}}]}}]}}}}"#),
        );
        let p = home.join(".claude/settings.json");
        let e = plan_claude_capture(p.to_str().unwrap(), None, "/x/bin/daisugi").unwrap().unwrap();
        assert!(e.note_only && e.content.is_empty(), "nothing is written");
        assert!(e.note.starts_with("[Claude Code] ") && e.note.contains(old) && e.note.contains(LEFT_AS_IS), "{}", e.note);
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn hermes_writes_nothing_when_nothing_was_rebound() {
        let home = scratch("hermes-left");
        let p = home.join(".hermes/config.yaml");
        put(&p, "mcp_servers:\n  opendaisugi:\n    command: /x/bin/daisugi\n    args:\n    - mcp\n    - serve\nhooks:\n  pre_tool_call:\n  - matcher: .*\n    command: daisugi hook record --format hermes || true\n    timeout: 10\n");
        let e = plan_hermes_config(p.to_str().unwrap(), true, true, "/x/bin/daisugi").unwrap().unwrap();
        assert!(e.note_only && e.content.is_empty(), "no rewrite, so no new backup");
        assert!(e.note.starts_with("[Hermes] ") && e.note.contains(LEFT_AS_IS), "{}", e.note);
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn the_path_note_needs_a_program_word_that_names_a_path() {
        let n = left_hook_note("Claude Code", "daisugi-go hook record --format claude", "daisugi");
        assert!(!n.contains("already names a path") && n.contains("not one plain command"), "{n}");
    }

    #[test]
    fn an_mcp_command_that_names_a_path_is_never_rewritten_to_a_bare_word() {
        let dir = scratch("mcp-path");
        let note = |p: &str| {
            format!("The MCP command `/old/bin/daisugi` in {p} was left as it is: install found no daisugi on PATH, so the only word it could write is `daisugi`, and the entry already names a path.")
        };
        let j = dir.join("claude.json");
        let js = j.to_str().unwrap();
        put(&j, r#"{"mcpServers": {"opendaisugi": {"command": "/old/bin/daisugi", "args": ["mcp", "serve"]}}}"#);
        let e = plan_mcp(js, false, &["mcpServers"], mcp_entry(true, "daisugi"), "MCP registration", None).unwrap().unwrap();
        assert!(e.note_only && e.content.is_empty() && e.note == note(js), "{e:?}");
        put(&j, r#"{"mcpServers": {"opendaisugi": {"command": "daisugi-go", "args": ["mcp", "serve"]}}}"#);
        let e = plan_mcp(js, false, &["mcpServers"], mcp_entry(true, "daisugi"), "MCP registration", None).unwrap().unwrap();
        assert!(e.note.is_empty() && e.content.contains("\"command\": \"daisugi\""), "{e:?}");
        let c = dir.join("config.toml");
        let cs = c.to_str().unwrap();
        put(&c, "\n[mcp_servers.opendaisugi]\ncommand = \"/old/bin/daisugi\"\nargs = [\"mcp\", \"serve\"]\n");
        let e = plan_codex_mcp(cs, "daisugi").unwrap().unwrap();
        assert!(e.note_only && e.note == note(cs), "{e:?}");
        let h = dir.join("config.yaml");
        let hs = h.to_str().unwrap();
        put(&h, "mcp_servers:\n  opendaisugi:\n    command: /old/bin/daisugi\n");
        let e = plan_hermes_config(hs, true, false, "daisugi").unwrap().unwrap();
        assert!(e.note_only && e.note == note(hs), "{e:?}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_user_wrapper_is_not_replaced() {
        let home = scratch("wrapper");
        let p = home.join(".codex/config.toml");
        let block = "model = \"x\"\n\n[mcp_servers.opendaisugi]\ncommand = \"/h/daisugi-local\"\nargs = [\"mcp\", \"serve\"]\n";
        put(&p, block);
        assert!(plan_codex_mcp(p.to_str().unwrap(), "/x/bin/daisugi").unwrap().is_none());
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn claude_entries_use_the_absolute_path_and_an_old_bare_entry_is_replaced() {
        let home = scratch("claude");
        put(
            &home.join(".claude/settings.json"),
            r#"{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"daisugi hook record --format claude"}]}]}}"#,
        );
        put(
            &home.join(".claude.json"),
            r#"{"mcpServers":{"opendaisugi":{"type":"stdio","command":"daisugi","args":["mcp","serve"],"env":{"K":"v"}}}}"#,
        );
        let h = home.to_str().unwrap();
        let ch = plan_install(&rt("claude"), h, "/x/bin/daisugi", &format!("{h}/.opendaisugi/warden"), &Opts::default()).unwrap();
        let settings = &edit_of(&ch, ".claude/settings.json").content;
        assert_eq!(settings.matches("hook record").count(), 1, "{settings}");
        assert!(settings.contains("\"command\": \"/x/bin/daisugi hook record --format claude\""), "{settings}");
        let mcp = &edit_of(&ch, ".claude.json").content;
        assert!(mcp.contains("\"command\": \"/x/bin/daisugi\"") && mcp.contains("\"K\": \"v\""), "{mcp}");
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn a_custom_mcp_command_is_left_alone() {
        let home = scratch("custom");
        put(&home.join(".claude.json"), r#"{"mcpServers":{"opendaisugi":{"command":"/my/wrapper","args":["mcp","serve"]}}}"#);
        std::fs::create_dir_all(home.join(".claude")).unwrap();
        let h = home.to_str().unwrap();
        let ch = plan_install(&rt("claude"), h, "/x/bin/daisugi", &format!("{h}/.opendaisugi/warden"), &Opts::default()).unwrap();
        // The pando layer adds its own server to the file; the opendaisugi entry stays as the user has it.
        let mcp = ch.edits.iter().rev().find(|e| e.path.ends_with(".claude.json")).map(|e| e.content.clone()).unwrap_or_default();
        let Value::Obj(cfg) = loads(&mcp).unwrap() else { panic!("{mcp}") };
        let Value::Obj(servers) = cfg.value("mcpServers").clone() else { panic!("{mcp}") };
        let Value::Obj(od) = servers.value("opendaisugi").clone() else { panic!("{mcp}") };
        assert_eq!(*od.value("command"), Value::Str("/my/wrapper".into()), "{mcp}");
        assert!(servers.get("pando").is_some(), "{mcp}");
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn a_codex_bare_block_is_replaced_and_both_forms_uninstall() {
        let home = scratch("codex");
        let p = home.join(".codex/config.toml");
        put(&p, "model = \"x\"\n\n[mcp_servers.opendaisugi]\ncommand = \"daisugi\"\nargs = [\"mcp\", \"serve\"]\n\n[other]\nk = 1\n");
        let e = plan_codex_mcp(p.to_str().unwrap(), "/x/bin/daisugi").unwrap().unwrap();
        assert_eq!(e.content.matches("[mcp_servers.opendaisugi]").count(), 1, "{}", e.content);
        assert!(e.content.contains("command = \"/x/bin/daisugi\"") && e.content.contains("[other]\nk = 1\n"), "{}", e.content);
        let gone = plan_codex_mcp_remove(p.to_str().unwrap()).unwrap().unwrap();
        assert!(!gone.content.contains("opendaisugi") && gone.content.contains("[other]"), "{}", gone.content);
        put(&p, "x = 1\n\n[mcp_servers.opendaisugi]\ncommand = \"/opt/x/daisugi\"\nargs = [\"mcp\", \"serve\"]\n");
        assert_eq!(plan_codex_mcp_remove(p.to_str().unwrap()).unwrap().unwrap().content, "x = 1\n");
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn hermes_bare_entries_are_rebound() {
        let home = scratch("hermes");
        let p = home.join(".hermes/config.yaml");
        put(&p, "mcp_servers:\n  opendaisugi:\n    command: daisugi\n    args:\n    - mcp\n    - serve\nhooks:\n  pre_tool_call:\n  - matcher: .*\n    command: daisugi hook record --format hermes\n    timeout: 10\n");
        let e = plan_hermes_config(p.to_str().unwrap(), true, true, "/x/bin/daisugi").unwrap().unwrap();
        assert_eq!(e.content.matches("hook record").count(), 1, "{}", e.content);
        assert!(e.content.contains("command: /x/bin/daisugi hook record --format hermes"), "{}", e.content);
        assert!(e.content.contains("command: /x/bin/daisugi\n"), "{}", e.content);
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn old_bare_and_absolute_record_hooks_are_both_record_hooks() {
        for c in [
            "daisugi hook record --format claude",
            "/opt/x/daisugi hook record --format claude",
            "/opt/x/daisugi-rs hook record --format claude --event stop",
        ] {
            assert!(is_record_hook(&Value::Str(c.into()), ""), "{c}");
        }
        assert!(!is_record_hook(&Value::Str("/opt/x/other hook record".into()), ""));
    }

    #[test]
    fn the_check_reads_the_hook_that_is_on_disk() {
        use std::os::unix::fs::PermissionsExt;
        let home = scratch("check");
        let h = home.to_str().unwrap();
        let (claude, hermes) = (rt("claude"), rt("hermes"));
        let gone = format!("{h}/gone/daisugi");
        put(
            &home.join(".claude/settings.json"),
            &format!(r#"{{"hooks":{{"PreToolUse":[{{"hooks":[{{"type":"command","command":"{gone} hook record --format claude"}}]}}]}}}}"#),
        );
        put(&home.join(".hermes/config.yaml"), &format!("mcp_servers:\n  opendaisugi:\n    command: {gone}\n    args:\n    - mcp\n    - serve\n"));
        let lines = unrunnable_commands(&[&claude, &hermes], h);
        assert_eq!(lines.len(), 2, "{lines:?}");
        assert!(lines[0].starts_with(&format!("[Claude Code] The command {gone} is not there, ")), "{lines:?}");
        assert!(lines[1].starts_with("[Hermes] "), "{lines:?}");
        // A file that is not executable.
        let plain = home.join("bin/daisugi");
        put(&plain, "x");
        std::fs::set_permissions(&plain, std::fs::Permissions::from_mode(0o644)).unwrap();
        put(
            &home.join(".claude/settings.json"),
            &format!(r#"{{"hooks":{{"PreToolUse":[{{"hooks":[{{"type":"command","command":"{} hook record --format claude"}}]}}]}}}}"#, plain.display()),
        );
        let lines = unrunnable_commands(&[&claude], h);
        assert!(lines.len() == 1 && lines[0].contains("is not executable"), "{lines:?}");
        std::fs::set_permissions(&plain, std::fs::Permissions::from_mode(0o755)).unwrap();
        assert!(unrunnable_commands(&[&claude], h).is_empty());
        // Nothing written, nothing to say.
        assert!(unrunnable_commands(&[&rt("codex")], h).is_empty());
        let _ = std::fs::remove_dir_all(&home);
    }

    #[test]
    fn a_uv_cache_path_is_called_temporary() {
        let home = scratch("uv");
        let exe = home.join(".cache/uv/archive-v0/abc/bin/daisugi");
        put(&exe, "x");
        let note = temporary_exe_note(exe.to_str().unwrap()).expect("a note");
        assert!(note.contains("temporary") && note.contains("uv tool install"), "{note}");
        let link = home.join("bin/daisugi");
        std::fs::create_dir_all(link.parent().unwrap()).unwrap();
        std::os::unix::fs::symlink(&exe, &link).unwrap();
        assert!(temporary_exe_note(link.to_str().unwrap()).is_some());
        assert!(temporary_exe_note("/usr/local/bin/daisugi").is_none());
        let _ = std::fs::remove_dir_all(&home);
    }
}

/// Whether a runtime wires a layer (`Runtime.unsupported_layers` leaves it
/// out). Hermes wires neither the warden nor the router base_url; OpenClaw
/// wires no warden.
pub fn wires(rt: &Runtime, layer: &str) -> bool {
    if layer == LAYER_PANDO && rt.key == "openclaw" {
        return false; // no pando hooks or plugin for OpenClaw yet
    }
    match rt.key {
        "hermes" => layer != "warden" && layer != "base_url",
        "openclaw" => layer != "warden",
        _ => true,
    }
}

/// `install.layer_present`: whether an uninstall of this one layer would
/// change something now. It plans the reverse and applies nothing. A layer
/// whose uninstall is refused counts as present, so a caller never claims to
/// have added what it could not read.
pub fn layer_present(rt: &Runtime, home: &str, layer: &str) -> bool {
    let mut gone = HashSet::new();
    match plan_reverse_all(rt, home, &mut gone, Some(&[layer.to_string()])) {
        Ok(ch) => ch.failed || !ch.written().is_empty(),
        Err(_) => true,
    }
}
