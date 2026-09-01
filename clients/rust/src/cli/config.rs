//! `~/.opendaisugi/config.yaml` as `opendaisugi.config.load_config` reads
//! it, for the settings the gate commands read, and the gate hook mode
//! installed in Claude Code's settings. Python validates the whole file
//! with pydantic: one bad field anywhere makes load_config raise. The Go
//! client's `config` package is the reference.

use super::words::{gate_hook_kind, gate_hook_mode, gate_hook_program, is_stale_mode_hook, KIND_UNKNOWN};
use super::yaml::{self, Kind, Node};
use crate::gate::pyjson::{loads, LoadError, Value};
use regex::Regex;
use std::sync::OnceLock;

/// Why a config was not read.
#[derive(Debug, Clone, PartialEq)]
pub enum ConfigErr {
    /// YAML outside the modelled subset: refuse, never guess.
    Unsupported,
    /// A file load_config is certain to refuse.
    Invalid,
}

impl std::fmt::Display for ConfigErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConfigErr::Unsupported => write!(f, "config.yaml uses YAML this binary does not read yet"),
            ConfigErr::Invalid => write!(f, "config.yaml does not validate"),
        }
    }
}

#[derive(Clone, Copy, PartialEq)]
enum T {
    Str,
    OptStr,
    Int,
    OptInt,
    Bool,
    OptBool,
    Path,
    Floor,
}

/// `opendaisugi.config.Config.model_fields`, in order, with each field's
/// type. A field Python validates and this table does not know would be
/// read as valid here; the Go client's test keeps its copy equal to
/// Python's, and this one is the same list.
const FIELDS: &[(&str, T)] = &[
    ("model", T::Str),
    ("max_task_chars", T::Int),
    ("z3_timeout_ms", T::Int),
    ("data_dir", T::Path),
    ("auto_tend", T::OptBool),
    ("gateway_local_model", T::OptStr),
    ("gateway_router", T::Str),
    ("switchyard_route_id", T::Str),
    ("switchyard_capable_model", T::Str),
    ("switchyard_efficient_model", T::OptStr),
    ("switchyard_api_key_env", T::OptStr),
    ("shell_allow_decomposition", T::Bool),
    ("gate_mode", T::Str),
    ("gate_ask", T::Bool),
    ("dialect_enforce", T::OptStr),
    ("verifier_client", T::Str),
    ("matcher_model", T::Str),
    ("llm_backend", T::OptStr),
    ("llm_base_url", T::OptStr),
    ("llm_host_kind", T::OptStr),
    ("llm_host_model", T::OptStr),
    ("llm_context_window", T::OptInt),
    ("envelope_source", T::Str),
    ("pathway_store_backend", T::Str),
    ("floor_report", T::OptStr),
    ("floor", T::Floor),
    ("voice_engine", T::Str),
    ("voice_model", T::Str),
    ("voice_device", T::Str),
    ("voice_compute_type", T::Str),
    ("voice_cleanup", T::Bool),
    ("voice_cleanup_model", T::OptStr),
    ("voice_cleanup_base_url", T::OptStr),
    ("voice_server_url", T::Str),
];

/// `FloorConfig.model_fields`.
const FLOOR_FIELDS: &[(&str, T)] =
    &[("backend", T::Str), ("notify_cmd", T::OptStr), ("tmux_socket", T::OptStr), ("coppice_socket", T::OptStr)];

/// The fields the gate commands read.
#[derive(Debug, Clone)]
pub struct Config {
    pub gate_mode: String,
    pub shell_allow_decomposition: bool,
    pub verifier_client: String,
    /// The matcher `find` runs (`_search.selected_matcher`).
    pub matcher_model: String,
    /// `llm_backend`, None when unset or null.
    pub llm_backend: Option<String>,
    /// `auto_tend`, None when unset or null.
    pub auto_tend: Option<bool>,
    /// The gateway's fields; an optional one is None when unset or null.
    pub gateway_local_model: Option<String>,
    pub gateway_router: String,
    pub switchyard_route_id: String,
    pub switchyard_capable_model: String,
    pub switchyard_efficient_model: Option<String>,
    pub switchyard_api_key_env: Option<String>,
    pub llm_base_url: Option<String>,
    pub llm_host_kind: Option<String>,
    pub llm_host_model: Option<String>,
    /// `llm_context_window`, None when unset or null.
    pub llm_context_window: Option<num_bigint::BigInt>,
    pub voice_engine: String,
    /// `dialect_enforce`, None when unset or null.
    pub dialect_enforce: Option<String>,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            gate_mode: "audit".into(),
            shell_allow_decomposition: false,
            verifier_client: "python".into(),
            matcher_model: "lexical".into(),
            llm_backend: None,
            auto_tend: None,
            gateway_local_model: None,
            gateway_router: "rules".into(),
            switchyard_route_id: "daisugi".into(),
            switchyard_capable_model: "claude-sonnet-5".into(),
            switchyard_efficient_model: None,
            switchyard_api_key_env: None,
            llm_base_url: None,
            llm_host_kind: None,
            llm_host_model: None,
            llm_context_window: None,
            voice_engine: "faster-whisper".into(),
            dialect_enforce: None,
        }
    }
}

#[derive(Clone, Copy, PartialEq, PartialOrd)]
enum Verdict {
    Ok,
    Invalid,
    Unsure,
}

fn re(which: usize) -> &'static Regex {
    static RES: OnceLock<Vec<Regex>> = OnceLock::new();
    &RES.get_or_init(|| {
        [r"^[+-]?[0-9](?:_?[0-9])*$", r"^[+-]?[0-9]+\.0+$", r"^[0-9+\-_.eE \t\n\r\x0c\x0b]*$"]
            .iter()
            .map(|p| Regex::new(p).expect("a fixed pattern"))
            .collect()
    })[which]
}

fn is_true_word(s: &str) -> bool {
    matches!(s, "true" | "yes" | "on" | "t" | "y" | "1")
}

fn is_false_word(s: &str) -> bool {
    matches!(s, "false" | "no" | "off" | "f" | "n" | "0")
}

/// pydantic's int from a string.
fn int_str(s: &str) -> Verdict {
    let t = s.trim_matches(|c: char| c.is_whitespace());
    if re(0).is_match(t) || re(1).is_match(t) {
        return Verdict::Ok;
    }
    if !re(2).is_match(s) {
        return Verdict::Invalid;
    }
    Verdict::Unsure
}

/// pydantic's bool of a value `check` has passed.
fn bool_of(v: &Node) -> bool {
    match v.kind {
        Kind::Bool => v.b,
        Kind::Int => v.text == "1",
        Kind::Float => v.text.parse::<f64>().map(|f| f == 1.0).unwrap_or(false),
        Kind::Str => is_true_word(&v.text.to_lowercase()),
        _ => false,
    }
}

/// pydantic's int of a value `check` has passed.
fn int_of(v: &Node) -> num_bigint::BigInt {
    use num_bigint::BigInt;
    let parse = |t: &str| t.parse::<BigInt>().unwrap_or_default();
    match v.kind {
        Kind::Bool => BigInt::from(v.b as i64),
        Kind::Int => parse(&v.text.replace('_', "")),
        Kind::Float => BigInt::from(v.text.parse::<f64>().map(|f| f as i64).unwrap_or(0)),
        Kind::Str => {
            let t = v.text.trim().replace('_', "");
            let t = t.split('.').next().unwrap_or("");
            parse(t.strip_prefix('+').unwrap_or(t))
        }
        _ => BigInt::default(),
    }
}

/// pydantic's lax validation of one value for one field type.
fn check(v: &Node, t: T) -> Verdict {
    let optional = matches!(t, T::OptStr | T::OptInt | T::OptBool);
    if v.kind == Kind::Null {
        return if optional { Verdict::Ok } else { Verdict::Invalid };
    }
    match t {
        T::Str | T::OptStr | T::Path => {
            if v.kind == Kind::Str {
                Verdict::Ok
            } else {
                Verdict::Invalid
            }
        }
        T::Int | T::OptInt => match v.kind {
            Kind::Int | Kind::Bool => Verdict::Ok,
            Kind::Float => match v.text.parse::<f64>() {
                Err(_) => Verdict::Unsure,
                Ok(f) => {
                    // Go's float64(int64(f)) == f.
                    let i = if f.is_nan() { i64::MIN } else { f as i64 };
                    if f.is_finite() && (i as f64) == f && f.abs() < 9.2e18 {
                        Verdict::Ok
                    } else {
                        Verdict::Invalid
                    }
                }
            },
            Kind::Str => int_str(&v.text),
            _ => Verdict::Invalid,
        },
        T::Bool | T::OptBool => match v.kind {
            Kind::Bool => Verdict::Ok,
            Kind::Int => {
                if v.text == "0" || v.text == "1" {
                    Verdict::Ok
                } else {
                    Verdict::Invalid
                }
            }
            Kind::Float => match v.text.parse::<f64>() {
                Ok(f) if f == 0.0 || f == 1.0 => Verdict::Ok,
                _ => Verdict::Invalid,
            },
            Kind::Str => {
                let l = v.text.to_lowercase();
                if is_true_word(&l) || is_false_word(&l) {
                    Verdict::Ok
                } else {
                    Verdict::Invalid
                }
            }
            _ => Verdict::Invalid,
        },
        T::Floor => {
            if v.kind != Kind::Map {
                return Verdict::Invalid;
            }
            if v.non_str_keys > 0 {
                return Verdict::Unsure;
            }
            let mut worst = Verdict::Ok;
            for (name, ft) in FLOOR_FIELDS {
                if let Some(sub) = v.map.get(*name) {
                    let r = check(sub, *ft);
                    if r > worst {
                        worst = r;
                    }
                }
            }
            worst
        }
    }
}

/// `Config(**raw)` of a parsed top-level mapping.
fn from_map(m: &Node) -> Result<Config, ConfigErr> {
    let mut cfg = Config::default();
    let (mut any_invalid, mut any_unsure) = (false, false);
    for (name, t) in FIELDS {
        if let Some(v) = m.map.get(*name) {
            match check(v, *t) {
                Verdict::Invalid => any_invalid = true,
                Verdict::Unsure => any_unsure = true,
                Verdict::Ok => {}
            }
        }
    }
    // One invalid field makes pydantic raise, whatever the others hold.
    if any_invalid {
        return Err(ConfigErr::Invalid);
    }
    if any_unsure {
        return Err(ConfigErr::Unsupported);
    }
    if let Some(v) = m.map.get("gate_mode") {
        cfg.gate_mode = v.text.clone();
    }
    if let Some(v) = m.map.get("matcher_model") {
        cfg.matcher_model = v.text.clone();
    }
    if let Some(v) = m.map.get("verifier_client") {
        cfg.verifier_client = v.text.clone();
    }
    if let Some(v) = m.map.get("shell_allow_decomposition") {
        cfg.shell_allow_decomposition = bool_of(v);
    }
    if let Some(v) = m.map.get("llm_backend") {
        if v.kind == Kind::Str {
            cfg.llm_backend = Some(v.text.clone());
        }
    }
    if let Some(v) = m.map.get("auto_tend") {
        if v.kind != Kind::Null {
            cfg.auto_tend = Some(bool_of(v));
        }
    }
    for (name, dst) in [
        ("gateway_router", &mut cfg.gateway_router),
        ("switchyard_route_id", &mut cfg.switchyard_route_id),
        ("switchyard_capable_model", &mut cfg.switchyard_capable_model),
    ] {
        if let Some(v) = m.map.get(name) {
            *dst = v.text.clone();
        }
    }
    for (name, dst) in [
        ("gateway_local_model", &mut cfg.gateway_local_model),
        ("switchyard_efficient_model", &mut cfg.switchyard_efficient_model),
        ("switchyard_api_key_env", &mut cfg.switchyard_api_key_env),
        ("llm_base_url", &mut cfg.llm_base_url),
        ("llm_host_kind", &mut cfg.llm_host_kind),
        ("llm_host_model", &mut cfg.llm_host_model),
        ("dialect_enforce", &mut cfg.dialect_enforce),
    ] {
        if let Some(v) = m.map.get(name) {
            if v.kind == Kind::Str {
                *dst = Some(v.text.clone());
            }
        }
    }
    if let Some(v) = m.map.get("voice_engine") {
        cfg.voice_engine = v.text.clone();
    }
    if let Some(v) = m.map.get("llm_context_window") {
        if v.kind != Kind::Null {
            cfg.llm_context_window = Some(int_of(v));
        }
    }
    Ok(cfg)
}

/// `load_config(path)`: the defaults when the file is absent.
pub fn load(path: &str) -> Result<Config, ConfigErr> {
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        // Path.exists() is false, a dangling symlink and a path through a
        // file included.
        Err(e) if e.kind() == std::io::ErrorKind::NotFound || e.raw_os_error() == Some(libc::ENOTDIR) => {
            return Ok(Config::default())
        }
        Err(_) => return Err(ConfigErr::Unsupported),
    };
    let text = String::from_utf8(raw).map_err(|_| ConfigErr::Unsupported)?;
    let v = yaml::parse(&text).map_err(|_| ConfigErr::Unsupported)?;
    match v.kind {
        Kind::Map => from_map(&v),
        Kind::Null => Ok(Config::default()),
        _ => {
            // `yaml.safe_load(...) or {}`: a falsy scalar is an empty
            // config; anything else has no .items() and raises.
            let falsy = match v.kind {
                Kind::Bool => !v.b,
                Kind::Int => v.text.trim_start_matches('-').trim_start_matches('0').is_empty(),
                Kind::Str => v.text.is_empty(),
                Kind::Seq => v.items.is_empty(),
                Kind::Float => v.text.parse::<f64>().map(|f| f == 0.0).unwrap_or(false),
                _ => false,
            };
            if falsy {
                Ok(Config::default())
            } else {
                Err(ConfigErr::Invalid)
            }
        }
    }
}

/// `gate.resolve_gate_mode(None, root=...)`: the gate_mode of the
/// config.yaml beside the gate root, or audit when that file does not
/// validate or names another mode.
pub fn gate_mode(config_path: &str) -> Result<String, ConfigErr> {
    match load(config_path) {
        Err(ConfigErr::Invalid) => Ok("audit".into()),
        Err(e) => Err(e),
        Ok(c) if c.gate_mode == "audit" || c.gate_mode == "enforce" => Ok(c.gate_mode),
        Ok(_) => Ok("audit".into()),
    }
}

/// `gate._configured_dialect_pin`: the dialect_enforce of the config.yaml
/// beside the gate root, or None when it names none or the file does not
/// validate.
pub fn dialect_pin(config_path: &str) -> Result<Option<String>, ConfigErr> {
    match load(config_path) {
        Err(ConfigErr::Invalid) => Ok(None),
        Err(e) => Err(e),
        Ok(c) => Ok(c.dialect_enforce.filter(|p| !p.is_empty())),
    }
}

/// `config.installed_hook_mode`: the --mode of the first gate hook in a
/// settings.json PreToolUse list, "" when none, and "unknown" when a gate
/// hook is in a form not read and no read hook enforces. `Err` where
/// Python would raise instead of answering.
pub fn installed_hook_mode(settings_path: &str) -> Result<String, ConfigErr> {
    let raw = match std::fs::read(settings_path) {
        Ok(r) => r,
        Err(_) => return Ok(String::new()),
    };
    let text = match String::from_utf8(raw) {
        Ok(t) => t,
        Err(_) => return Ok(String::new()),
    };
    let data = match loads(&text) {
        Ok(Value::Obj(o)) => o,
        Ok(_) => return Err(ConfigErr::Unsupported), // data.get raises
        Err(LoadError::Unsupported) => return Err(ConfigErr::Unsupported),
        Err(LoadError::Recursion) => return Err(ConfigErr::Unsupported),
        Err(_) => return Ok(String::new()),
    };
    let hooks_v = data.value("hooks");
    if !hooks_v.truthy() {
        return Ok(String::new());
    }
    let hooks = match hooks_v {
        Value::Obj(o) => o,
        _ => return Err(ConfigErr::Unsupported),
    };
    let pre_v = hooks.value("PreToolUse");
    if !pre_v.truthy() {
        return Ok(String::new());
    }
    let pre = match pre_v {
        Value::List(l) => l,
        _ => return Err(ConfigErr::Unsupported),
    };
    let (mut first, mut unknown) = (String::new(), false);
    for e in pre {
        let entry = match e {
            Value::Obj(o) => o,
            _ => return Err(ConfigErr::Unsupported),
        };
        let hs_v = entry.value("hooks");
        if !hs_v.truthy() {
            continue;
        }
        let hs = match hs_v {
            Value::List(l) => l,
            _ => return Err(ConfigErr::Unsupported),
        };
        for h in hs {
            let hook = match h {
                Value::Obj(o) => o,
                _ => return Err(ConfigErr::Unsupported),
            };
            let command = hook.value("command");
            if gate_hook_kind(command) == KIND_UNKNOWN || is_stale_mode_hook(command) {
                // A --mode shadow hook denies every call, so it counts as
                // a hook that may enforce, never as audit.
                unknown = true;
            }
            let mode = gate_hook_mode(command);
            if !mode.is_empty() && first.is_empty() {
                first = mode;
            }
        }
    }
    // A gate hook in a form not read may enforce: only a read enforce hook
    // outranks it, never a read audit one.
    if first == "enforce" || !unknown {
        return Ok(first);
    }
    Ok(KIND_UNKNOWN.into())
}

/// One gate hook whose program is not there.
#[derive(Debug, Clone, PartialEq)]
pub struct GoneHook {
    pub program: String,
    pub mode: String,
}

/// `config.missing_hook_programs`: each gate hook in the settings file
/// whose program is an absolute path that is not there, such as a binary a
/// package manager removed on upgrade. That hook fails on every call.
/// Anything that is not the usual shape is skipped.
pub fn missing_hook_programs(settings_path: &str) -> Vec<GoneHook> {
    let Ok(raw) = std::fs::read(settings_path) else { return vec![] };
    let Ok(text) = String::from_utf8(raw) else { return vec![] };
    let Ok(Value::Obj(data)) = loads(&text) else { return vec![] };
    let Value::Obj(hooks) = data.value("hooks") else { return vec![] };
    let Value::List(pre) = hooks.value("PreToolUse") else { return vec![] };
    let mut out: Vec<GoneHook> = vec![];
    for e in pre {
        let Value::Obj(entry) = e else { continue };
        let Value::List(hs) = entry.value("hooks") else { continue };
        for h in hs {
            let Value::Obj(hook) = h else { continue };
            let command = hook.value("command");
            let program = gate_hook_program(command);
            if !program.starts_with('/') || std::fs::metadata(&program).is_ok() {
                continue;
            }
            let g = GoneHook { program, mode: gate_hook_mode(command) };
            if !out.contains(&g) {
                out.push(g);
            }
        }
    }
    out
}

/// `config.stale_mode_hooks`: whether the settings file holds a
/// PreToolUse gate hook that passes --mode shadow. Anything that is not
/// the usual shape is skipped.
pub fn stale_mode_hooks(settings_path: &str) -> bool {
    let Ok(raw) = std::fs::read(settings_path) else { return false };
    let Ok(text) = String::from_utf8(raw) else { return false };
    let Ok(Value::Obj(data)) = loads(&text) else { return false };
    let Value::Obj(hooks) = data.value("hooks") else { return false };
    let Value::List(pre) = hooks.value("PreToolUse") else { return false };
    for e in pre {
        let Value::Obj(entry) = e else { continue };
        let Value::List(hs) = entry.value("hooks") else { continue };
        for h in hs {
            if let Value::Obj(hook) = h {
                if is_stale_mode_hook(hook.value("command")) {
                    return true;
                }
            }
        }
    }
    false
}

/// `config.stale_mode_warning`: what gate status says about a gate hook
/// that passes --mode shadow.
pub fn stale_mode_warning(settings_path: &str) -> String {
    format!(
        "warning: the gate hook in {settings_path} passes --mode shadow, which is now --mode audit, \
         so the gate refuses every call. Run: daisugi install --gate --uninstall, then daisugi install --gate"
    )
}

/// `config.missing_hook_warning`: what gate status says about a gate hook
/// whose program is gone.
pub fn missing_hook_warning(settings_path: &str, g: &GoneHook) -> String {
    if g.mode == "enforce" {
        format!(
            "warning: the gate hook in {settings_path} runs {}, which does not exist, so every call is denied. \
             Run: daisugi install --gate --enforce",
            g.program
        )
    } else {
        format!(
            "warning: the gate hook in {settings_path} runs {}, which does not exist, so the gate sees no call. \
             Run: daisugi install --gate",
            g.program
        )
    }
}

/// `config.EffectiveHook`.
#[derive(Debug, Default)]
pub struct EffectiveHook {
    pub mode: String,
    pub cwd_mode: String,
    pub global_mode: String,
}

/// `config.effective_hook_mode`: the stricter of the machine-global hook
/// and the one in cwd's .claude/settings.json.
pub fn effective_hook_mode(home: &str, cwd: &str) -> Result<EffectiveHook, ConfigErr> {
    let global_path = if home == "/" { "/.claude/settings.json".to_string() } else { format!("{home}/.claude/settings.json") };
    let cwd_path = if cwd == "/" { "/.claude/settings.json".to_string() } else { format!("{cwd}/.claude/settings.json") };
    let g = installed_hook_mode(&global_path)?;
    let c = if cwd_path != global_path { installed_hook_mode(&cwd_path)? } else { String::new() };
    let rank = |m: &str| match m {
        "enforce" => 3,
        KIND_UNKNOWN => 2,
        "audit" => 1,
        _ => 0,
    };
    let mut mode = String::new();
    // max() keeps the first of equal keys: cwd before global.
    for m in [&c, &g] {
        if !m.is_empty() && (mode.is_empty() || rank(m) > rank(&mode)) {
            mode = m.clone();
        }
    }
    Ok(EffectiveHook { mode, cwd_mode: c, global_mode: g })
}

/// `config.hook_source_label`.
pub fn source_label(eff: &EffectiveHook) -> &'static str {
    match (!eff.cwd_mode.is_empty(), !eff.global_mode.is_empty()) {
        (true, true) => "project+global",
        (true, false) => "project",
        (false, true) => "global",
        _ => "",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn type_name(t: T) -> &'static str {
        match t {
            T::Str => "<class 'str'>",
            T::OptStr => "str | None",
            T::Int => "<class 'int'>",
            T::OptInt => "int | None",
            T::Bool => "<class 'bool'>",
            T::OptBool => "bool | None",
            T::Path => "<class 'pathlib.Path'>",
            T::Floor => "<class 'opendaisugi.config.FloorConfig'>",
        }
    }

    /// The field tables equal Python's `Config.model_fields` and
    /// `FloorConfig.model_fields`, which clients/cli_cases.py writes to
    /// the Go client's testdata. A field Python validates and a table here
    /// lacks would let a file Python refuses read as valid.
    #[test]
    fn the_field_tables_are_pythons() {
        let raw = include_str!("../../../go/internal/config/testdata/config_fields.json");
        let v: serde_json::Value = serde_json::from_str(raw).expect("config_fields.json");
        for (key, table) in [("config", FIELDS), ("floor", FLOOR_FIELDS)] {
            let py: Vec<(String, String)> = v[key]
                .as_array()
                .expect("a list")
                .iter()
                .map(|p| (p[0].as_str().unwrap_or("").to_string(), p[1].as_str().unwrap_or("").to_string()))
                .collect();
            let ours: Vec<(String, String)> =
                table.iter().map(|(n, t)| (n.to_string(), type_name(*t).to_string())).collect();
            assert_eq!(py, ours, "{key}");
        }
    }
}

// ---------------------------------------------------------------------------
// save_config(load_config(path).model_copy(update=...)): the whole Config,
// every field as pydantic dumps it in JSON mode, keys sorted, written with
// yaml.safe_dump. A value this module cannot turn into the form pydantic
// would dump is Unsupported: the caller then refuses before it writes.
// ---------------------------------------------------------------------------

use crate::gate::pyjson::Object;

fn floor_object(backend: Value, coppice: Value, notify: Value, tmux: Value) -> Value {
    let mut o = Object::new();
    o.set("backend", backend);
    o.set("coppice_socket", coppice);
    o.set("notify_cmd", notify);
    o.set("tmux_socket", tmux);
    Value::Obj(o)
}

/// `Config()` as `model_dump(mode="json")` gives it; data_dir is filled
/// from the home directory.
fn defaults(home: &str) -> Vec<(&'static str, Value)> {
    let s = |x: &str| Value::Str(x.into());
    let i = |x: &str| Value::Int(x.into());
    vec![
        ("model", s("anthropic/claude-sonnet-4-20250514")),
        ("max_task_chars", i("4000")),
        ("z3_timeout_ms", i("500")),
        ("data_dir", s(&super::gateroot::path_str(&format!("{home}/.opendaisugi")))),
        ("auto_tend", Value::Null),
        ("gateway_local_model", Value::Null),
        ("gateway_router", s("rules")),
        ("switchyard_route_id", s("daisugi")),
        ("switchyard_capable_model", s("claude-sonnet-5")),
        ("switchyard_efficient_model", Value::Null),
        ("switchyard_api_key_env", Value::Null),
        ("shell_allow_decomposition", Value::Bool(false)),
        ("gate_mode", s("audit")),
        ("gate_ask", Value::Bool(false)),
        ("dialect_enforce", Value::Null),
        ("verifier_client", s("python")),
        ("matcher_model", s("lexical")),
        ("llm_backend", Value::Null),
        ("llm_base_url", Value::Null),
        ("llm_host_kind", Value::Null),
        ("llm_host_model", Value::Null),
        ("llm_context_window", Value::Null),
        ("envelope_source", s("evidence-inferred")),
        ("pathway_store_backend", s("sqlite")),
        ("floor_report", Value::Null),
        ("floor", floor_object(s("auto"), Value::Null, Value::Null, Value::Null)),
        ("voice_engine", s("faster-whisper")),
        ("voice_model", s("tiny.en")),
        ("voice_device", s("cpu")),
        ("voice_compute_type", s("int8")),
        ("voice_cleanup", Value::Bool(false)),
        ("voice_cleanup_model", Value::Null),
        ("voice_cleanup_base_url", Value::Null),
        ("voice_server_url", s("http://127.0.0.1:7477")),
    ]
}

fn big_int(text: &str) -> Result<Value, ConfigErr> {
    let t = text.strip_prefix('+').unwrap_or(text);
    let n: num_bigint::BigInt = t.parse().map_err(|_| ConfigErr::Unsupported)?;
    Ok(Value::Int(n.to_string()))
}

/// The value pydantic keeps for `v` in a field of type `t`.
fn dumped(v: &Node, t: T) -> Result<Value, ConfigErr> {
    if v.kind == Kind::Null {
        return Ok(Value::Null);
    }
    match t {
        T::Str | T::OptStr => Ok(Value::Str(v.text.clone())),
        T::Path => Ok(Value::Str(super::gateroot::path_str(&v.text))),
        T::Int | T::OptInt => match v.kind {
            Kind::Int => big_int(&v.text.replace('_', "")),
            Kind::Bool => Ok(Value::Int(if v.b { "1" } else { "0" }.into())),
            Kind::Float => {
                let f: f64 = v.text.parse().map_err(|_| ConfigErr::Unsupported)?;
                if !f.is_finite() || f.fract() != 0.0 || f.abs() >= 9.2e18 {
                    return Err(ConfigErr::Unsupported);
                }
                Ok(Value::Int((f as i64).to_string()))
            }
            Kind::Str => {
                let mut s = v.text.trim().replace('_', "");
                if re(1).is_match(&s) {
                    s.truncate(s.find('.').unwrap_or(s.len()));
                }
                big_int(&s)
            }
            _ => Err(ConfigErr::Unsupported),
        },
        T::Bool | T::OptBool => Ok(Value::Bool(bool_of(v))),
        T::Floor => {
            let get = |name: &str, def: Value, ft: T| -> Result<Value, ConfigErr> {
                match v.map.get(name) {
                    Some(sub) => dumped(sub, ft),
                    None => Ok(def),
                }
            };
            Ok(floor_object(
                get("backend", Value::Str("auto".into()), T::Str)?,
                get("coppice_socket", Value::Null, T::OptStr)?,
                get("notify_cmd", Value::Null, T::OptStr)?,
                get("tmux_socket", Value::Null, T::OptStr)?,
            ))
        }
    }
}

/// The text `save` would write for `load_config(path)` updated with
/// `updates` (str values, or None).
pub fn dump(path: &str, home: &str, updates: &[(String, Value)]) -> Result<String, ConfigErr> {
    let mut vals = defaults(home);
    match std::fs::read(path) {
        Ok(raw) => {
            let text = String::from_utf8(raw).map_err(|_| ConfigErr::Unsupported)?;
            let doc = yaml::parse(&text).map_err(|_| ConfigErr::Unsupported)?;
            if doc.kind == Kind::Map {
                from_map(&doc)?;
                for (name, t) in FIELDS {
                    if let Some(v) = doc.map.get(*name) {
                        let x = dumped(v, *t)?;
                        if let Some(slot) = vals.iter_mut().find(|(k, _)| k == name) {
                            slot.1 = x;
                        }
                    }
                }
            } else {
                // A falsy top level is an empty config; anything else raises.
                load(path)?;
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => return Err(ConfigErr::Unsupported),
    }
    let mut all: Vec<(String, Value)> = vals.into_iter().map(|(k, v)| (k.to_string(), v)).collect();
    for (k, v) in updates {
        match all.iter_mut().find(|(n, _)| n == k) {
            Some(slot) => slot.1 = v.clone(),
            None => all.push((k.clone(), v.clone())),
        }
    }
    all.sort_by(|a, b| a.0.cmp(&b.0));
    let mut o = Object::with_capacity(all.len());
    for (k, v) in all {
        o.set(&k, v);
    }
    crate::pathways::yamldump::safe_dump(&Value::Obj(o)).map_err(|_| ConfigErr::Unsupported)
}

/// Writes `load_config(path).model_copy(update=updates)` to path as
/// `save_config` does.
pub fn save(path: &str, home: &str, updates: &[(String, Value)]) -> Result<(), SaveErr> {
    let text = dump(path, home, updates).map_err(SaveErr::Config)?;
    std::fs::create_dir_all(super::gateroot::parent(path)).map_err(SaveErr::Io)?;
    super::gateroot::write_file(path, &text).map_err(SaveErr::Io)?;
    Ok(())
}

/// Why a save did not happen.
#[derive(Debug)]
pub enum SaveErr {
    Config(ConfigErr),
    Io(std::io::Error),
}

// ---------------------------------------------------------------------------
// resolved_config(path): what `daisugi config` shows.
// ---------------------------------------------------------------------------

/// `config.ResolvedField`: one setting as the running code sees it, and
/// where it came from.
pub struct Row {
    pub key: String,
    pub value: String,
    pub source: String,
}

impl Row {
    pub fn new(key: &str, value: &str, source: &str) -> Row {
        Row { key: key.into(), value: value.into(), source: source.into() }
    }
}

/// Why `rows` gave no rows.
#[derive(Debug)]
pub enum RowsErr {
    Config(ConfigErr),
    /// load_config on a file whose top level is truthy but not a mapping:
    /// `raw.items()` raises AttributeError on this type.
    Attr(&'static str),
}

impl std::fmt::Display for RowsErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RowsErr::Config(e) => write!(f, "{e}"),
            RowsErr::Attr(t) => write!(f, "AttributeError: '{t}' object has no attribute 'items'"),
        }
    }
}

fn kind_name(k: Kind) -> &'static str {
    match k {
        Kind::Bool => "bool",
        Kind::Int => "int",
        Kind::Float => "float",
        Kind::Str => "str",
        Kind::Seq => "list",
        _ => "dict",
    }
}

/// str() of a value `dumped` holds.
fn py_str_of(v: &Value) -> String {
    match v {
        Value::Null => "None".into(),
        Value::Bool(true) => "True".into(),
        Value::Bool(false) => "False".into(),
        Value::Int(t) => t.clone(),
        Value::Str(s) => s.clone(),
        _ => String::new(),
    }
}

/// The Config part of `resolved_config(path)`: every field in
/// model_fields order with str() of its value and "file" or "default", the
/// floor group as its leaves; `unknown_config_keys(path)`, sorted; and
/// `load_config(path)`.
pub fn rows(path: &str, home: &str) -> Result<(Vec<Row>, Vec<String>, Config), RowsErr> {
    let mut doc = Node::of(Kind::Null);
    match std::fs::read(path) {
        Ok(raw) => {
            let text = String::from_utf8(raw).map_err(|_| RowsErr::Config(ConfigErr::Unsupported))?;
            let v = yaml::parse(&text).map_err(|_| RowsErr::Config(ConfigErr::Unsupported))?;
            match v.kind {
                Kind::Map => doc = v,
                Kind::Null => {}
                k => match load(path) {
                    Err(ConfigErr::Invalid) => return Err(RowsErr::Attr(kind_name(k))),
                    Err(e) => return Err(RowsErr::Config(e)),
                    // A falsy scalar: an empty config, and no raw keys.
                    Ok(_) => {}
                },
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => return Err(RowsErr::Config(ConfigErr::Unsupported)),
    }
    let cfg = load(path).map_err(RowsErr::Config)?;
    // unknown_config_keys names a key that is not a string by its str(),
    // which the reader does not keep.
    if doc.non_str_keys > 0 {
        return Err(RowsErr::Config(ConfigErr::Unsupported));
    }
    let defs = defaults(home);
    let mut out = vec![];
    let mut unknown = vec![];
    for (name, t) in FIELDS {
        let v = doc.map.get(*name);
        if *t == T::Floor {
            if let Some(v) = v {
                if v.non_str_keys > 0 {
                    return Err(RowsErr::Config(ConfigErr::Unsupported));
                }
            }
            for (fname, ft) in FLOOR_FIELDS {
                let (val, source) = match v.and_then(|v| v.map.get(*fname)) {
                    Some(sv) => (py_str_of(&dumped(sv, *ft).map_err(RowsErr::Config)?), "file"),
                    None => ((if *fname == "backend" { "auto" } else { "None" }).to_string(), "default"),
                };
                out.push(Row::new(&format!("floor.{fname}"), &val, source));
            }
            if let Some(v) = v {
                if v.kind == Kind::Map {
                    for k in &v.keys {
                        if !FLOOR_FIELDS.iter().any(|(n, _)| n == k) {
                            unknown.push(format!("floor.{k}"));
                        }
                    }
                }
            }
            continue;
        }
        let (val, source) = match v {
            Some(v) => (dumped(v, *t).map_err(RowsErr::Config)?, "file"),
            None => (defs.iter().find(|(k, _)| k == name).map(|(_, v)| v.clone()).unwrap_or(Value::Null), "default"),
        };
        let s = if *name == "llm_backend" && val == Value::Null { "auto".to_string() } else { py_str_of(&val) };
        out.push(Row::new(name, &s, source));
    }
    for k in &doc.keys {
        if !FIELDS.iter().any(|(n, _)| n == k) {
            unknown.push(k.clone());
        }
    }
    unknown.sort();
    Ok((out, unknown, cfg))
}
