//! `opendaisugi/agent_profiles.py`: the operator's table of where a child
//! agent runs. The Go client's `internal/agentprof` is the reference.
//!
//! A profile names a wire (claude-code, anthropic or openai), an optional
//! base URL, an optional default model and an optional key_env. The root
//! starter reads each key_env once ([`resolve`]); below it a key moves
//! only through an inherited pipe. No profile is limited by a model's
//! origin.

use std::collections::BTreeMap;

use crate::cli::yaml::{Kind, Node};
use crate::warden::pyjson::{canonical_json_ascii, loads_py, Object, Value};

/// The built-in profile's name.
pub const DEFAULT: &str = "default";
/// The wires a profile may name.
pub const WIRES: &[&str] = &["claude-code", "anthropic", "openai"];
const FIELDS: &[&str] = &["wire", "base_url", "model", "key_env", "max_tokens", "timeout_s", "thinking"];
/// `agent_profiles.MAX_TOKENS_LIMIT`.
pub const MAX_TOKENS_LIMIT: usize = 1_000_000;
/// `agent_profiles.TIMEOUT_LIMIT`: the most seconds a profile may let one
/// pando summary call take.
pub const TIMEOUT_LIMIT: usize = 86_400;

/// The server a wire talks to when the profile names none.
pub fn default_base(wire: &str) -> &'static str {
    if wire == "openai" {
        "https://api.openai.com/v1"
    } else {
        "https://api.anthropic.com"
    }
}

/// The variables that can hold a provider key or point a key elsewhere: a
/// profiled sprig child gets none of them.
pub const AMBIENT: &[&str] = &[
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "OPENAI_API_KEY",
    "SPRIG_API_KEY",
    "ANTHROPIC_BASE_URL",
    "SPRIG_BACKEND",
    "SPRIG_MODEL",
];

/// `agent_profiles.is_key_name`: a variable name that may hold a
/// credential, `*_API_KEY` or `*_TOKEN`. No child of the executor gets one.
pub fn is_key_name(name: &str) -> bool {
    name.ends_with("_API_KEY") || name.ends_with("_TOKEN")
}

/// `agent_profiles.valid_name`.
pub fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'-'))
}

fn valid_var(name: &str) -> bool {
    let mut b = name.bytes();
    matches!(b.next(), Some(c) if c.is_ascii_alphabetic() || c == b'_')
        && b.all(|c| c.is_ascii_alphanumeric() || c == b'_')
}

/// `agent_profiles.Profile`; None fields are None.
#[derive(Clone, Debug, Default)]
pub struct Profile {
    pub name: String,
    pub wire: String,
    pub base_url: Option<String>,
    pub model: Option<String>,
    pub key_env: Option<String>,
    pub key: Option<String>,
    /// The most tokens one pando summary call may produce; None for the
    /// summarizer's default. Only pando reads it, and the bundle does not
    /// carry it.
    pub max_tokens: Option<usize>,
    /// The most seconds one pando summary call may take; None for the
    /// summarizer's default (120). Only pando reads it, and the bundle does
    /// not carry it.
    pub timeout_s: Option<usize>,
    /// off, low, medium or high; None leaves the server's default. Only
    /// pando reads it, and the bundle does not carry it.
    pub thinking: Option<String>,
}

/// The values a profile's `thinking` may take.
pub const THINKING: &[&str] = &["off", "low", "medium", "high"];

/// A thinking value. YAML 1.1 reads a bare off as false, so false counts as
/// off.
fn thinking_of(v: &Node) -> Option<String> {
    match v.kind {
        Kind::Bool if !v.b => Some("off".into()),
        Kind::Str if THINKING.contains(&v.text.as_str()) => Some(v.text.clone()),
        _ => None,
    }
}

/// The built-in profile.
pub fn builtin() -> Profile {
    Profile {
        name: DEFAULT.into(),
        wire: "claude-code".into(),
        ..Default::default()
    }
}

fn opt(s: &Option<String>) -> Value {
    s.clone().map(Value::Str).unwrap_or(Value::Null)
}

impl Profile {
    fn entry(&self) -> Value {
        Value::Obj(
            Object::new()
                .with("wire", self.wire.as_str())
                .with("base_url", opt(&self.base_url))
                .with("model", opt(&self.model))
                .with("key_env", opt(&self.key_env))
                .with("key", opt(&self.key)),
        )
    }
}

/// The resolved table: the valid profiles with their keys read, and why
/// each other text-named entry is not a profile.
#[derive(Clone, Debug, Default)]
pub struct Profiles {
    pub valid: BTreeMap<String, Profile>,
    pub invalid: BTreeMap<String, String>,
}

impl Profiles {
    /// The built-in and the valid names, sorted.
    pub fn names(&self) -> Vec<String> {
        let mut out: Vec<String> = std::iter::once(DEFAULT.to_string())
            .chain(self.valid.keys().cloned())
            .collect();
        out.sort();
        out
    }

    /// Every key_env the table names, sorted.
    pub fn key_names(&self) -> Vec<String> {
        let mut out: Vec<String> = self
            .valid
            .values()
            .filter_map(|p| p.key_env.clone())
            .filter(|k| !k.is_empty())
            .collect();
        out.sort();
        out.dedup();
        out
    }

    /// `Profiles.lookup`: the profile a step names (None is the built-in),
    /// or why not.
    pub fn lookup(&self, name: Option<&str>) -> Result<Profile, String> {
        let Some(n) = name.filter(|n| *n != DEFAULT) else {
            return Ok(builtin());
        };
        if let Some(why) = self.invalid.get(n) {
            return Err(format!("agent profile '{n}' is not valid: {why}"));
        }
        let Some(p) = self.valid.get(n) else {
            return Err(format!(
                "no agent profile named '{n}'; the profiles are {}",
                self.names().join(", ")
            ));
        };
        if let Some(k) = p.key_env.as_ref().filter(|k| !k.is_empty()) {
            if p.key.is_none() {
                return Err(format!("agent profile '{n}' names {k}, which is not set"));
            }
        }
        Ok(p.clone())
    }

    /// `Profiles.bundle`: the table with its keys, as
    /// json.dumps(sort_keys=True, separators=(",", ":")).
    pub fn bundle(&self) -> Vec<u8> {
        let mut doc = Object::new().with(DEFAULT, builtin().entry());
        for (n, p) in &self.valid {
            doc.set(n, p.entry());
        }
        canonical_json_ascii(&Value::Obj(doc)).into_bytes()
    }
}

/// `agent_profiles.list_rows`: one row for each valid profile, by name,
/// with no key and no key variable name. `key_set` is false only when the
/// profile names a key variable that is not set.
pub fn list_rows(ps: &Profiles) -> Vec<Value> {
    ps.valid
        .iter()
        .map(|(n, p)| {
            Value::Obj(
                Object::new()
                    .with("name", n.as_str())
                    .with("wire", p.wire.as_str())
                    .with("base_url", opt(&p.base_url))
                    .with("model", opt(&p.model))
                    .with("key_set", Value::Bool(p.key_env.is_none() || p.key.is_some())),
            )
        })
        .collect()
}

/// `agent_profiles.operator_bundle`: the bundle an operator-side root
/// starter hands to tree run, and one line for each profile left out of it
/// (an invalid one, and one whose key_env is not set). The lines name the
/// profile and the variable, never a key.
pub fn operator_bundle(ps: &Profiles) -> (Vec<u8>, Vec<String>) {
    let mut names: Vec<&String> = ps.valid.keys().chain(ps.invalid.keys()).collect();
    names.sort();
    names.dedup();
    let mut notes = Vec::new();
    let mut doc = Object::new().with(DEFAULT, builtin().entry());
    for n in names {
        if let Some(why) = ps.invalid.get(n) {
            notes.push(format!("agent profile '{n}' is left out: {why}"));
            continue;
        }
        let p = &ps.valid[n];
        if let Some(k) = p.key_env.as_ref().filter(|k| !k.is_empty()) {
            if p.key.is_none() {
                notes.push(format!("agent profile '{n}' is left out: {k} is not set"));
                continue;
            }
        }
        doc.set(n, p.entry());
    }
    (canonical_json_ascii(&Value::Obj(doc)).into_bytes(), notes)
}

fn str_val(v: &Node) -> Result<Option<String>, ()> {
    match v.kind {
        Kind::Null => Ok(None),
        Kind::Str => Ok(Some(v.text.clone())),
        _ => Err(()),
    }
}

/// `agent_profiles._check`.
fn check(name: &str, doc: &Node) -> Option<String> {
    if name == DEFAULT {
        return Some("default is built in".into());
    }
    if !valid_name(name) {
        return Some("its name is not 1 to 64 of A-Z a-z 0-9 . _ -".into());
    }
    if doc.kind != Kind::Map {
        return Some("it is not a mapping".into());
    }
    let mut extra: Vec<&String> = doc
        .keys
        .iter()
        .filter(|k| !FIELDS.contains(&k.as_str()))
        .collect();
    extra.sort();
    if let Some(k) = extra.first() {
        return Some(format!("it has an unknown field {k}"));
    }
    let wire = doc
        .map
        .get("wire")
        .filter(|w| w.kind == Kind::Str)
        .map(|w| w.text.as_str());
    let Some(wire) = wire.filter(|w| WIRES.contains(w)) else {
        return Some("wire must be claude-code, anthropic or openai".into());
    };
    for f in ["base_url", "model", "key_env"] {
        if let Some(v) = doc.map.get(f) {
            if str_val(v).is_err() {
                return Some(format!("{f} must be text"));
            }
        }
    }
    if let Some(v) = doc.map.get("key_env") {
        if v.kind == Kind::Str && !valid_var(&v.text) {
            return Some("key_env must be a variable name".into());
        }
    }
    let max_tokens = doc.map.get("max_tokens").filter(|v| v.kind != Kind::Null);
    if let Some(v) = max_tokens {
        if max_tokens_of(v).is_none() {
            return Some(format!(
                "max_tokens must be a whole number from 1 to {MAX_TOKENS_LIMIT}"
            ));
        }
    }
    if let Some(v) = doc.map.get("timeout_s").filter(|v| v.kind != Kind::Null) {
        if timeout_s_of(v).is_none() {
            return Some(format!(
                "timeout_s must be a whole number from 1 to {TIMEOUT_LIMIT}"
            ));
        }
    }
    let thinking = doc.map.get("thinking").filter(|v| v.kind != Kind::Null);
    if let Some(v) = thinking {
        if thinking_of(v).is_none() {
            return Some("thinking must be off, low, medium or high".into());
        }
    }
    if wire == "claude-code" {
        if thinking.is_some() {
            return Some("the claude-code wire takes no thinking".into());
        }
        if doc
            .map
            .get("base_url")
            .is_some_and(|v| v.kind != Kind::Null)
        {
            return Some("the claude-code wire takes no base_url".into());
        }
        if doc.map.get("key_env").is_some_and(|v| v.kind != Kind::Null) {
            return Some("the claude-code wire takes no key_env".into());
        }
        if max_tokens.is_some() {
            return Some("the claude-code wire takes no max_tokens".into());
        }
    }
    None
}

/// A timeout_s value: a whole number from 1 to `TIMEOUT_LIMIT`.
fn timeout_s_of(v: &Node) -> Option<usize> {
    if v.kind != Kind::Int {
        return None;
    }
    v.text
        .parse::<usize>()
        .ok()
        .filter(|n| (1..=TIMEOUT_LIMIT).contains(n))
}

/// A max_tokens value: a whole number from 1 to `MAX_TOKENS_LIMIT`. A bool,
/// a float and text are not one.
fn max_tokens_of(v: &Node) -> Option<usize> {
    if v.kind != Kind::Int {
        return None;
    }
    v.text
        .parse::<usize>()
        .ok()
        .filter(|n| (1..=MAX_TOKENS_LIMIT).contains(n))
}

/// `agent_profiles.resolve`: read the table raw (None for none) and each
/// key_env from lookup, once. An empty key counts as not set.
pub fn resolve(raw: Option<&Node>, lookup: &dyn Fn(&str) -> Option<String>) -> Profiles {
    let mut ps = Profiles::default();
    let Some(raw) = raw.filter(|r| r.kind == Kind::Map) else {
        return ps;
    };
    for name in &raw.keys {
        let doc = &raw.map[name];
        if let Some(why) = check(name, doc) {
            ps.invalid.insert(name.clone(), why);
            continue;
        }
        let get = |f: &str| doc.map.get(f).and_then(|v| str_val(v).ok().flatten());
        let key_env = get("key_env");
        let key = key_env
            .as_ref()
            .filter(|k| !k.is_empty())
            .and_then(|k| lookup(k))
            .filter(|k| !k.is_empty());
        ps.valid.insert(
            name.clone(),
            Profile {
                name: name.clone(),
                wire: doc.map["wire"].text.clone(),
                base_url: get("base_url"),
                model: get("model"),
                key_env,
                key,
                max_tokens: doc.map.get("max_tokens").and_then(max_tokens_of),
                timeout_s: doc.map.get("timeout_s").and_then(timeout_s_of),
                thinking: doc.map.get("thinking").and_then(thinking_of),
            },
        );
    }
    ps
}

/// `agent_profiles.from_bundle`: the table a starter got on its keys
/// descriptor. A bundle that does not read is an empty table.
pub fn from_bundle(data: &[u8]) -> Profiles {
    let mut ps = Profiles::default();
    let Ok(text) = std::str::from_utf8(data) else {
        return ps;
    };
    let Ok(Value::Obj(doc)) = loads_py(text, 900) else {
        return ps;
    };
    for (name, e) in doc.iter() {
        let Value::Obj(e) = e else { continue };
        if name == DEFAULT || !valid_name(name) {
            continue;
        }
        let Some(wire) = e.value("wire").as_str().filter(|w| WIRES.contains(w)) else {
            continue;
        };
        let s = |k: &str| e.value(k).as_str().map(String::from);
        ps.valid.insert(
            name.clone(),
            Profile {
                name: name.clone(),
                wire: wire.into(),
                base_url: s("base_url"),
                model: s("model"),
                key_env: s("key_env"),
                key: s("key"),
                max_tokens: None,
                timeout_s: None,
                thinking: None,
            },
        );
    }
    ps
}

/// `agent_profiles.configured_table`: config.yaml's agent_profiles as
/// written, or None. A config that does not read has no table.
pub fn configured_table(data_home: &str) -> Option<Node> {
    let raw = std::fs::read(format!("{data_home}/config.yaml")).ok()?;
    let text = String::from_utf8(raw).ok()?;
    let doc = crate::cli::yaml::parse(&text).ok()?;
    if doc.kind != Kind::Map {
        return None;
    }
    doc.map.get("agent_profiles").cloned()
}

#[cfg(test)]
mod tests {
    use super::*;

    const TABLE: &str = "agent_profiles:\n  oai: {wire: openai, base_url: 'http://127.0.0.1:9/v1', key_env: OAI_KEY}\n  local: {wire: anthropic, base_url: 'http://127.0.0.1:8', model: 'llama3.3:70b'}\n  nokey: {wire: openai, key_env: NOT_SET_HERE}\n  bad: {wire: grpc}\n  cc1: {wire: claude-code, key_env: X}\n  default: {wire: openai}\n";

    fn table() -> Node {
        crate::cli::yaml::parse(TABLE).unwrap().map["agent_profiles"].clone()
    }

    #[test]
    fn resolve_and_lookup_follow_the_oracle() {
        let t = table();
        let ps = resolve(Some(&t), &|k| (k == "OAI_KEY").then(|| "sk-1".to_string()));
        assert_eq!(ps.names(), vec!["default", "local", "nokey", "oai"]);
        assert_eq!(
            ps.invalid["bad"],
            "wire must be claude-code, anthropic or openai"
        );
        assert_eq!(ps.invalid["cc1"], "the claude-code wire takes no key_env");
        assert_eq!(ps.invalid["default"], "default is built in");
        assert_eq!(
            ps.lookup(Some("zzz")).unwrap_err(),
            "no agent profile named 'zzz'; the profiles are default, local, nokey, oai"
        );
        assert_eq!(
            ps.lookup(Some("nokey")).unwrap_err(),
            "agent profile 'nokey' names NOT_SET_HERE, which is not set"
        );
        assert_eq!(ps.lookup(None).unwrap().wire, "claude-code");
        assert_eq!(ps.key_names(), vec!["NOT_SET_HERE", "OAI_KEY"]);
        let back = from_bundle(&ps.bundle());
        assert_eq!(back.valid["oai"].key.as_deref(), Some("sk-1"));
        assert!(String::from_utf8(ps.bundle()).unwrap().starts_with(
            r#"{"default":{"base_url":null,"key":null,"key_env":null,"model":null,"wire":"claude-code"},"local":"#
        ));
    }

    const MAX_TOKENS: &str = "agent_profiles:\n  big: {wire: openai, max_tokens: 4096}\n  none: {wire: anthropic, max_tokens: null}\n  plain: {wire: openai}\n  top: {wire: openai, max_tokens: 1000000}\n  zero: {wire: openai, max_tokens: 0}\n  neg: {wire: openai, max_tokens: -5}\n  text: {wire: openai, max_tokens: '400'}\n  flag: {wire: openai, max_tokens: true}\n  half: {wire: openai, max_tokens: 1.5}\n  huge: {wire: openai, max_tokens: 1000001}\n  cc: {wire: claude-code, max_tokens: 400}\n";

    #[test]
    fn max_tokens_follows_the_oracle() {
        let t = crate::cli::yaml::parse(MAX_TOKENS).unwrap().map["agent_profiles"].clone();
        let ps = resolve(Some(&t), &|_| None);
        let bad = "max_tokens must be a whole number from 1 to 1000000";
        for n in ["zero", "neg", "text", "flag", "half", "huge"] {
            assert_eq!(ps.invalid[n], bad, "{n}");
        }
        assert_eq!(ps.invalid["cc"], "the claude-code wire takes no max_tokens");
        assert_eq!(ps.valid["big"].max_tokens, Some(4096));
        assert_eq!(ps.valid["top"].max_tokens, Some(1_000_000));
        assert_eq!(ps.valid["none"].max_tokens, None);
        assert_eq!(ps.valid["plain"].max_tokens, None);
        assert!(!String::from_utf8(ps.bundle()).unwrap().contains("max_tokens"));
    }

    const TIMEOUT: &str = "agent_profiles:\n  slow: {wire: openai, timeout_s: 240}\n  none: {wire: anthropic, timeout_s: null}\n  plain: {wire: openai}\n  top: {wire: openai, timeout_s: 86400}\n  zero: {wire: openai, timeout_s: 0}\n  neg: {wire: openai, timeout_s: -5}\n  text: {wire: openai, timeout_s: '400'}\n  flag: {wire: openai, timeout_s: true}\n  half: {wire: openai, timeout_s: 1.5}\n  huge: {wire: openai, timeout_s: 86401}\n  cc: {wire: claude-code, timeout_s: 300}\n  cczero: {wire: claude-code, timeout_s: 0}\n";

    const THINKING_TABLE: &str = "agent_profiles:\n  a: {wire: openai, thinking: off}\n  b: {wire: anthropic, thinking: high}\n  c: {wire: openai, thinking: null}\n  d: {wire: openai}\n  e: {wire: openai, thinking: none}\n  f: {wire: openai, thinking: true}\n  g: {wire: openai, thinking: 3}\n  h: {wire: openai, thinking: High}\n  cc: {wire: claude-code, thinking: off}\n";

    #[test]
    fn thinking_follows_the_oracle() {
        let t = crate::cli::yaml::parse(THINKING_TABLE).unwrap().map["agent_profiles"].clone();
        let ps = resolve(Some(&t), &|_| None);
        let bad = "thinking must be off, low, medium or high";
        for n in ["e", "f", "g", "h"] {
            assert_eq!(ps.invalid[n], bad, "{n}");
        }
        assert_eq!(ps.invalid["cc"], "the claude-code wire takes no thinking");
        assert_eq!(ps.valid["a"].thinking.as_deref(), Some("off"));
        assert_eq!(ps.valid["b"].thinking.as_deref(), Some("high"));
        assert_eq!(ps.valid["c"].thinking, None);
        assert_eq!(ps.valid["d"].thinking, None);
        assert!(!String::from_utf8(ps.bundle()).unwrap().contains("thinking"));
    }

    #[test]
    fn timeout_s_follows_the_oracle() {
        let t = crate::cli::yaml::parse(TIMEOUT).unwrap().map["agent_profiles"].clone();
        let ps = resolve(Some(&t), &|_| None);
        let bad = "timeout_s must be a whole number from 1 to 86400";
        for n in ["zero", "neg", "text", "flag", "half", "huge", "cczero"] {
            assert_eq!(ps.invalid[n], bad, "{n}");
        }
        assert_eq!(ps.valid["slow"].timeout_s, Some(240));
        assert_eq!(ps.valid["top"].timeout_s, Some(86_400));
        assert_eq!(ps.valid["cc"].timeout_s, Some(300));
        assert_eq!(ps.valid["none"].timeout_s, None);
        assert_eq!(ps.valid["plain"].timeout_s, None);
        assert!(!String::from_utf8(ps.bundle()).unwrap().contains("timeout_s"));
    }
}
