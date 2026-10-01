//! `install`'s four default layers (skill, MCP, capture and instructions)
//! and their reverse, as `install.py` writes them, and the plan of every
//! layer in Python's apply order. The Go client's `install/layers.go` is
//! the twin.

use std::collections::HashSet;
use std::fs;

use super::gateroot::{self, join, mkdir_all, parent};
use super::install::{
    command_hooks, dump_settings, fail, plan, plan_claude_gate_on, plan_codex_gate, plan_pop_hook, read_json, Change,
    Edit, HookOptions, InstErr, ReadState, Runtime, Step, R,
};
use super::installgw::{
    json_of, plan_claude_base_url, plan_codex_base_url_on, plan_codex_unpatch_on, plan_pop_env_key, plan_steps,
    read_text, strip_json5, DEFAULT_BASE_URL,
};
use super::words::{is_gate_hook, is_record_hook};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{loads, py_type_name, LoadError, Object, Value};

/// `install._SKILL_NAME`.
pub const SKILL_NAME: &str = "opendaisugi-checklist";

// The skill's files, byte for byte the package's.
include!("layer_assets.rs");

/// `install._CLAUDE_MD_MARKER`.
const MD_MARKER: &str = "<!-- opendaisugi-managed -->";

/// `install._CLAUDE_MD_BLOCK`.
fn md_block() -> String {
    format!(
        "{MD_MARKER}\n## openDaisugi — automatic pathway routing\n\nBefore planning any task with 3 or more steps, call the `find_pathway`\nMCP tool. If similarity ≥ 0.85, use the returned cached plan via\n`run_plan` instead of re-planning from scratch. When a cached pathway is\nused, note it explicitly: \"Using cached opendaisugi pathway (similarity\nX.XX) — skipping re-plan.\"\n\nIf no pathway matches, proceed normally. After execution, the run is\njournaled automatically and will feed distillation on the next\n`daisugi tend` cycle.\n{MD_MARKER}\n"
    )
}

/// `install._CODEX_MCP_BLOCK`.
const CODEX_MCP_BLOCK: &str = "\n[mcp_servers.opendaisugi]\ncommand = \"daisugi\"\nargs = [\"mcp\", \"serve\"]\n";

/// The SKILL.md `install --print-skill` prints.
pub fn skill_text() -> &'static str {
    SKILL_FILES.iter().find(|(r, _)| *r == "SKILL.md").map(|(_, t)| *t).unwrap_or("")
}

fn lexists(p: &str) -> bool {
    gateroot::lexists(p)
}

/// `install._agents_skill_target`.
pub fn agents_skill_target(home: &str, fallback: &str) -> String {
    let agents = join(home, ".agents/skills");
    if lexists(&agents) || !lexists(&join(home, fallback)) {
        return join(&agents, SKILL_NAME);
    }
    join(&join(home, fallback), SKILL_NAME)
}

/// The layers an install writes beyond the four default ones.
#[derive(Clone, Default)]
pub struct Opts {
    pub gate: bool,
    pub gateway: bool,
    pub enforce: bool,
    pub ask: bool,
    pub url: String,
    /// The floor report the gate layer wires ("", "herdr" or "coppice").
    pub report: String,
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
    steps.push(step("skill", "Symlink opendaisugi-checklist skill", skill_target(rt, home)));
    match rt.key {
        "claude" => {
            steps.push(step("mcp", "Register MCP server \"opendaisugi\"", j(".claude.json")));
            steps.push(step("capture", "Add PreToolUse capture hook", j(".claude/settings.json")));
            steps.push(step("instructions", "Append pathway guidance", j(".claude/CLAUDE.md")));
        }
        "codex" => {
            steps.push(step("mcp", "Register opendaisugi MCP server", j(".codex/config.toml")));
            steps.push(step("instructions", "Append pathway guidance", j(".codex/AGENTS.md")));
        }
        "hermes" => {
            steps.push(step("mcp", "Register opendaisugi MCP server", j(".hermes/config.yaml")));
            steps.push(step("capture", "Add pre_tool_call capture hook", j(".hermes/config.yaml")));
        }
        _ => {
            steps.push(step("mcp", "Register opendaisugi MCP server", j(".openclaw/openclaw.json")));
            steps.push(step("capture", "Install before_tool_call capture plugin", j(".openclaw/extensions/opendaisugi")));
            steps.push(step("instructions", "Append pathway guidance", j(".openclaw/workspace/AGENTS.md")));
        }
    }
    let mut gaps = vec![];
    for s in plan_steps(rt, home, o.gate, o.gateway, o.enforce, o.ask, &o.url) {
        if !s.supported {
            gaps.push(s);
            continue;
        }
        let is_gate = s.layer == "gate";
        steps.push(s);
        if is_gate && rt.key == "claude" {
            if o.report == "herdr" {
                steps.push(step("gate", "Add Stop + Notification floor-report hooks (Herdr)", j(".claude/settings.json")));
            }
            if o.report == "herdr" || o.report == "coppice" {
                steps.push(step("gate", "Add SubagentStart + SubagentStop hooks (subagent rows)", j(".claude/settings.json")));
            }
        }
    }
    let _ = plan;
    steps.extend(gaps);
    steps
}

/// The last edit of `p` in the change, whose content later edits of `p`
/// are planned on.
fn prior_of(ch: &Change, p: &str) -> Option<Edit> {
    ch.edits.iter().rev().find(|e| e.path == p && e.kind.is_empty() && e.warning.is_empty()).cloned()
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

fn mcp_entry(with_type: bool) -> Object {
    let mut o = Object::new();
    if with_type {
        o.set("type", "stdio");
    }
    o.set("command", "daisugi");
    o.set("args", Value::List(vec![Value::Str("mcp".into()), Value::Str("serve".into())]));
    o
}

/// `_patch_mcp`: entry set at the key path in a JSON (or JSON5) file
/// unless one is there; a file that does not parse is left alone.
fn plan_mcp(p: &str, json5: bool, keys: &[&str], entry: Object, what: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
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
    if lo.get("opendaisugi").is_some() {
        return Ok(None);
    }
    lo.set("opendaisugi", entry);
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
fn plan_claude_capture(p: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
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
    let mut changed = !cmds.iter().any(|c| is_record_hook(c, ""));
    if changed {
        let mut e = command_entry("daisugi hook record --format claude");
        let hooks = e.value("hooks").clone();
        e = Object::new();
        e.set("matcher", "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch");
        e.set("hooks", hooks);
        pre.push(Value::Obj(e));
        set_event(&mut s, "PreToolUse", pre);
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
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed, content: dump_settings(&Value::Obj(s)), ..Edit::default() }))
}

/// `_patch_claude_report_hooks` (Stop and Notification) or
/// `_patch_claude_subagent_hooks`.
fn plan_report_hooks(p: &str, prior: Option<&Edit>, subagent: bool) -> R<Option<Edit>> {
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
    for (name, word) in evs {
        let mut list = event_list(&mut s, name)?;
        let have = command_hooks(&list)?.iter().any(|h| is_record_hook(h.value("command"), word));
        if !have {
            list.push(Value::Obj(command_entry(&format!("daisugi hook record --format claude --event {word}"))));
            set_event(&mut s, name, list);
            changed = true;
        }
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed, content: dump_settings(&Value::Obj(s)), ..Edit::default() }))
}

/// `_patch_instructions`: the managed block appended once. It makes the
/// file's directory and keeps no backup.
fn plan_instructions(p: &str) -> R<Option<Edit>> {
    let (text, _) = read_text(p)?;
    if text.contains(MD_MARKER) {
        return Ok(None);
    }
    let sep = if text.is_empty() { "" } else { "\n\n" };
    let content = format!("{}{sep}{}", text.trim_end_matches('\n'), md_block());
    Ok(Some(Edit { path: p.into(), kind: "md".into(), content, ..Edit::default() }))
}

/// `_patch_codex_config`.
fn plan_codex_mcp(p: &str) -> R<Option<Edit>> {
    let (text, existed) = read_text(p)?;
    if text.contains("[mcp_servers.opendaisugi]") {
        return Ok(None);
    }
    let sep = if text.is_empty() { "" } else { "\n" };
    let content = format!("{}{sep}{CODEX_MCP_BLOCK}", text.trim_end_matches('\n'));
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
fn plan_hermes_config(p: &str) -> R<Option<Edit>> {
    let existed = gateroot::lexists(p);
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
    if cfg.get("mcp_servers").is_none() {
        cfg.set("mcp_servers", Object::new());
    }
    let Some(Value::Obj(mcp)) = cfg.get_mut("mcp_servers") else { return Err(InstErr::Unsupported) };
    if mcp.get("opendaisugi").is_none() {
        mcp.set("opendaisugi", mcp_entry(false));
        changed = true;
    }
    let mut list = event_list(&mut cfg, "pre_tool_call")?;
    const CMD: &str = "daisugi hook record --format hermes";
    if !list.iter().any(|h| h.as_obj().map(|o| o.value("command")) == Some(&Value::Str(CMD.into()))) {
        let mut h = Object::new();
        h.set("matcher", ".*");
        h.set("command", CMD);
        h.set("timeout", Value::Int("10".into()));
        list.push(Value::Obj(h));
        set_event(&mut cfg, "pre_tool_call", list);
        changed = true;
    }
    if !changed {
        return Ok(None);
    }
    Ok(Some(Edit { path: p.into(), existed, content: yaml_text(&cfg)?, ..Edit::default() }))
}

/// The error a write into a directory that is not there raises.
fn missing_dir(p: &str) -> R<()> {
    if !gateroot::is_dir(&parent(p)) {
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
    steps.push(Box::new(move |_| Ok(Some(Edit { path: target.clone(), kind: "skill".into(), listed: true, ..Edit::default() }))));
    let settings = j(".claude/settings.json");
    match rt.key {
        "claude" => {
            let cj = j(".claude.json");
            steps.push(Box::new(move |ch| plan_mcp(&cj, false, &["mcpServers"], mcp_entry(true), "MCP registration", prior_of(ch, &cj).as_ref())));
            let s1 = settings.clone();
            steps.push(Box::new(move |ch| written_needs_dir(plan_claude_capture(&s1, prior_of(ch, &s1).as_ref()), &s1)));
            let md = j(".claude/CLAUDE.md");
            steps.push(Box::new(move |_| plan_instructions(&md)));
            if o.gate {
                let s2 = settings.clone();
                let entry = super::install::hook_entry(self_path, &hook(o.ask));
                steps.push(Box::new(move |ch| written_needs_dir(plan_claude_gate_on(&s2, &entry, prior_of(ch, &s2).as_ref()), &s2)));
                if o.report == "herdr" {
                    let s3 = settings.clone();
                    steps.push(Box::new(move |ch| plan_report_hooks(&s3, prior_of(ch, &s3).as_ref(), false)));
                }
                if o.report == "herdr" || o.report == "coppice" {
                    let s4 = settings.clone();
                    steps.push(Box::new(move |ch| plan_report_hooks(&s4, prior_of(ch, &s4).as_ref(), true)));
                }
            }
            if o.gateway {
                let s5 = settings.clone();
                let u = url.clone();
                steps.push(Box::new(move |ch| written_needs_dir(plan_claude_base_url(&s5, &u, prior_of(ch, &s5).as_ref()), &s5)));
            }
        }
        "codex" => {
            let toml = j(".codex/config.toml");
            let t1 = toml.clone();
            steps.push(Box::new(move |_| plan_codex_mcp(&t1)));
            let md = j(".codex/AGENTS.md");
            steps.push(Box::new(move |_| plan_instructions(&md)));
            if o.gate {
                let hj = j(".codex/hooks.json");
                let entry = super::install::hook_entry(self_path, &hook(false));
                steps.push(Box::new(move |_| plan_codex_gate(&hj, &entry)));
            }
            if o.gateway {
                let u = url.clone();
                steps.push(Box::new(move |ch| plan_codex_base_url_on(&toml, &u, prior_of(ch, &toml).as_ref())));
            }
        }
        "hermes" => {
            let y = j(".hermes/config.yaml");
            steps.push(Box::new(move |_| plan_hermes_config(&y)));
        }
        _ => {
            let cfg = j(".openclaw/openclaw.json");
            let c1 = cfg.clone();
            steps.push(Box::new(move |ch| plan_mcp(&c1, true, &["mcp", "servers"], mcp_entry(false), "MCP registration", prior_of(ch, &c1).as_ref())));
            let md = j(".openclaw/workspace/AGENTS.md");
            steps.push(Box::new(move |_| plan_instructions(&md)));
            let plug = j(".openclaw/extensions/opendaisugi");
            steps.push(Box::new(move |_| Ok(Some(Edit { path: plug.clone(), kind: "plugin".into(), listed: true, ..Edit::default() }))));
            if o.gateway {
                let u = url.clone();
                steps.push(Box::new(move |ch| {
                    let mut prov = Object::new();
                    prov.set("baseUrl", u.as_str());
                    prov.set("api", "anthropic-messages");
                    plan_mcp(&cfg, true, &["models", "providers"], prov, "gateway provider registration", prior_of(ch, &cfg).as_ref())
                }));
            }
        }
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
                gateroot::write_file(&p, text)?;
            }
            Ok(None)
        }
        "md" => {
            mkdir_all(&parent(&e.path))?;
            gateroot::write_file(&e.path, &e.content)
        }
        "remove" => clear_path(&e.path).map(|_| None),
        "md-unpatch" => {
            super::install::backup(&e.path)?;
            gateroot::write_file(&e.path, &e.content)
        }
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
    let Some(start) = text.find(MD_MARKER) else { return Ok(None) };
    let after = start + MD_MARKER.len();
    let Some(second) = text[after..].find(MD_MARKER) else { return Ok(None) };
    let end = after + second + MD_MARKER.len();
    let mut cleaned = format!("{}{}", &text[..start], &text[end..]).trim_end_matches('\n').to_string();
    if !cleaned.is_empty() {
        cleaned.push('\n');
    }
    Ok(Some(Edit { path: p.into(), kind: "md-unpatch".into(), content: cleaned, listed: true, ..Edit::default() }))
}

/// `_pop_json_mcp`.
fn plan_pop_mcp(p: &str, key: &str) -> R<Option<Edit>> {
    let (v, st) = match read_json(p) {
        Ok(x) => x,
        Err(InstErr::Fail(_)) => return Err(InstErr::Unsupported),
        Err(e) => return Err(e),
    };
    if st != ReadState::Ok {
        return Ok(None);
    }
    let Value::Obj(mut cfg) = v else { return Err(InstErr::Unsupported) };
    let Some(m) = cfg.get_mut(key) else { return Ok(None) };
    let Value::Obj(m) = m else { return Err(InstErr::Unsupported) };
    if m.get("opendaisugi").is_none() {
        return Ok(None);
    }
    m.remove("opendaisugi");
    if m.is_empty() {
        cfg.remove(key);
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: dump_settings(&Value::Obj(cfg)), ..Edit::default() }))
}

/// The config.toml half of `CodexRuntime.reverse`.
fn plan_codex_mcp_remove(p: &str) -> R<Option<Edit>> {
    let (text, existed) = read_text(p).map_err(|_| InstErr::Unsupported)?;
    if !existed || !text.contains(CODEX_MCP_BLOCK.trim()) {
        return Ok(None);
    }
    let mut cleaned = text.replace(CODEX_MCP_BLOCK, "").trim_end_matches('\n').to_string();
    if !cleaned.is_empty() {
        cleaned.push('\n');
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: cleaned, ..Edit::default() }))
}

/// The config.yaml half of `HermesRuntime.reverse`.
fn plan_hermes_reverse(p: &str) -> R<Option<Edit>> {
    let Some(Some(mut cfg)) = yaml_of(p)? else { return Ok(None) };
    let mut changed = false;
    if let Some(Value::Obj(servers)) = cfg.get_mut("mcp_servers") {
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
    if let Some(Value::Obj(hooks)) = cfg.get_mut("hooks") {
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
fn plan_openclaw_unconfig(p: &str) -> R<Option<Edit>> {
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
pub fn plan_reverse_all(rt: &Runtime, home: &str, gone: &mut HashSet<String>) -> R<Change> {
    let j = |rel: &str| join(home, rel);
    let mut ch = Change::new(rt);
    if rt.key == "claude" || rt.key == "codex" {
        let f = if rt.key == "claude" { j(".claude/settings.json") } else { j(".codex/hooks.json") };
        if let Some(why) = super::install::unknown_gate_hook(&f) {
            ch.failed = true;
            ch.why = why;
            return Ok(ch);
        }
    }
    let any_record = |c: &Value| is_record_hook(c, "");
    let gate = |c: &Value| is_gate_hook(c);
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
            push(&mut ch, Ok(plan_remove(&j(&format!(".agents/skills/{SKILL_NAME}")), gone)))?;
            push(&mut ch, Ok(plan_remove(&join(&j(fallback), SKILL_NAME), gone)))?;
            if rt.key == "claude" {
                let s = j(".claude/settings.json");
                push(&mut ch, plan_pop_mcp(&j(".claude.json"), "mcpServers"))?;
                let r = plan_pop_hook(&s, &any_record, &["PreToolUse"], prior_of(&ch, &s).as_ref());
                push(&mut ch, r)?;
                let r = plan_pop_hook(&s, &gate, &["PreToolUse"], prior_of(&ch, &s).as_ref());
                push(&mut ch, r)?;
                let r = plan_pop_hook(&s, &any_record, &["Stop", "Notification", "SubagentStart", "SubagentStop"], prior_of(&ch, &s).as_ref());
                push(&mut ch, r)?;
                let r = plan_pop_env_key(&s, "ANTHROPIC_BASE_URL", prior_of(&ch, &s).as_ref());
                push(&mut ch, r)?;
                push(&mut ch, plan_unpatch_instructions(&j(".claude/CLAUDE.md")))?;
            } else {
                let toml = j(".codex/config.toml");
                push(&mut ch, plan_codex_mcp_remove(&toml))?;
                push(&mut ch, plan_pop_hook(&j(".codex/hooks.json"), &gate, &["PreToolUse"], None))?;
                let r = plan_codex_unpatch_on(&toml, prior_of(&ch, &toml).as_ref());
                push(&mut ch, r)?;
                push(&mut ch, plan_unpatch_instructions(&j(".codex/AGENTS.md")))?;
            }
        }
        "hermes" => {
            push(&mut ch, Ok(plan_remove(&j(&format!(".hermes/skills/opendaisugi/{SKILL_NAME}")), gone)))?;
            push(&mut ch, plan_hermes_reverse(&j(".hermes/config.yaml")))?;
        }
        _ => {
            push(&mut ch, Ok(plan_remove(&j(&format!(".openclaw/workspace/skills/{SKILL_NAME}")), gone)))?;
            push(&mut ch, plan_openclaw_unconfig(&j(".openclaw/openclaw.json")))?;
            push(&mut ch, Ok(plan_remove(&j(".openclaw/extensions/opendaisugi"), gone)))?;
            push(&mut ch, plan_unpatch_instructions(&j(".openclaw/workspace/AGENTS.md")))?;
        }
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
}
