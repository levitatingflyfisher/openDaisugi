//! `~/.opendaisugi/config.yaml` as `opendaisugi.config.load_config` reads
//! it, for the settings the gate commands read, and the gate hook mode
//! installed in Claude Code's settings. Python validates the whole file
//! with pydantic: one bad field anywhere makes load_config raise. The Go
//! client's `config` package is the reference.

use super::words::{gate_hook_kind, gate_hook_mode, gate_hook_program, KIND_UNKNOWN};
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
}

impl Default for Config {
    fn default() -> Self {
        Config {
            gate_mode: "shadow".into(),
            shell_allow_decomposition: false,
            verifier_client: "python".into(),
            matcher_model: "all-MiniLM-L6-v2".into(),
            llm_backend: None,
            auto_tend: None,
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
    Ok(cfg)
}

/// `load_config(path)`: the defaults when the file is absent.
pub fn load(path: &str) -> Result<Config, ConfigErr> {
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        // Path.exists() is false, a dangling symlink included.
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Config::default()),
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
/// config.yaml beside the gate root, or shadow when that file does not
/// validate or names another mode.
pub fn gate_mode(config_path: &str) -> Result<String, ConfigErr> {
    match load(config_path) {
        Err(ConfigErr::Invalid) => Ok("shadow".into()),
        Err(e) => Err(e),
        Ok(c) if c.gate_mode == "shadow" || c.gate_mode == "enforce" => Ok(c.gate_mode),
        Ok(_) => Ok("shadow".into()),
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
            if gate_hook_kind(command) == KIND_UNKNOWN {
                unknown = true;
            }
            let mode = gate_hook_mode(command);
            if !mode.is_empty() && first.is_empty() {
                first = mode;
            }
        }
    }
    // A gate hook in a form not read may enforce: only a read enforce hook
    // outranks it, never a read shadow one.
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
        "shadow" => 1,
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
