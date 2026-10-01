//! The one coppice config file, coppice.toml: the harness Enter opens and
//! the command each harness runs. The server reads it on every request
//! that needs it, so an edit takes effect on the next pane. The command
//! line writes it back with [`save`], in the layout Go's BurntSushi/toml
//! encoder writes.

use std::collections::BTreeMap;
use std::io::Write;
use std::path::PathBuf;

use serde::Deserialize;

use crate::sys;

/// One [harness.<name>] table. An absent args or resume_args list is None
/// and an empty one is Some, since a save keeps the two apart.
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct Harness {
    pub command: String,
    pub args: Option<Vec<String>>,
    pub state: String,
    pub resume_args: Option<Vec<String>>,
}

impl Harness {
    pub fn args(&self) -> &[String] {
        self.args.as_deref().unwrap_or(&[])
    }

    pub fn resume_args(&self) -> &[String] {
        self.resume_args.as_deref().unwrap_or(&[])
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct Keys {
    pub leave: String,
    pub talk: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct Voice {
    pub enabled: Option<bool>,
    pub url: String,
    pub token_file: String,
    pub args: Vec<String>,
}

/// The whole file. Unknown keys are ignored, as BurntSushi/toml ignores
/// them; a key of the wrong type makes the whole file unreadable, as there.
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct Config {
    pub default: String,
    pub harness: BTreeMap<String, Harness>,
    /// Whether the file has a harness table at all, even an empty one.
    #[serde(skip)]
    pub harness_set: bool,
    pub plugins: Option<Vec<String>>,
    pub keys: Keys,
    pub plugin: BTreeMap<String, toml::Table>,
    pub projects: Vec<String>,
    pub gateway: Option<String>,
    pub voice: Voice,
}

/// Where daisugi's gateway listens with no --host or --port.
pub const DEFAULT_GATEWAY: &str = "http://127.0.0.1:8787";

/// The plugin list of a file with no plugins key.
pub fn default_plugins() -> Vec<String> {
    [
        "tree",
        "minimap",
        "kanban",
        "colony",
        "shift-log",
        "herdr-grid",
        "inbox",
        "notify-ntfy",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect()
}

impl Config {
    /// The gateway base URL the floor compares with: the gateway key, else
    /// the default. Empty when the file says gateway = "".
    pub fn gateway_url(&self) -> String {
        match &self.gateway {
            None => DEFAULT_GATEWAY.to_string(),
            Some(g) => g.clone(),
        }
    }

    /// The plugin ids that run: the plugins list, else the default list.
    pub fn enabled_plugins(&self) -> Vec<String> {
        match &self.plugins {
            None => default_plugins(),
            Some(p) => p.clone(),
        }
    }

    /// False only when the file says [voice] enabled = false.
    pub fn voice_on(&self) -> bool {
        self.voice.enabled != Some(false)
    }

    /// The byte that leaves a pane.
    pub fn leave_key(&self) -> Result<u8, String> {
        if self.keys.leave.trim().is_empty() {
            return Ok(DEFAULT_LEAVE);
        }
        parse_key(&self.keys.leave).map_err(|e| {
            format!(
                "bad [keys] leave {} in {}: {e}",
                sys::go_quote(&self.keys.leave),
                path().display()
            )
        })
    }
}

/// ctrl-space, the key that leaves a pane when the file names none.
pub const DEFAULT_LEAVE: u8 = 0x00;

/// ctrl-\, the key that records a voice clip when the file names none.
pub const DEFAULT_TALK: u8 = 0x1c;

impl Config {
    /// The byte that records a voice clip. An empty [keys] talk gives
    /// DEFAULT_TALK; a key parse_key refuses, the leave key, or a key the
    /// rail uses is an error that names the file.
    pub fn talk_key(&self, leave: u8) -> Result<u8, String> {
        let name = self.keys.talk.trim();
        if name.is_empty() {
            if DEFAULT_TALK == leave {
                return Err(format!(
                    "[keys] leave in {} is ctrl-\\, the default talk key. Set [keys] talk to another ctrl key",
                    path().display()
                ));
            }
            return Ok(DEFAULT_TALK);
        }
        let res = parse_key(name).and_then(|b| {
            if b == leave || matches!(b, 0x00 | 0x03 | 0x14 | 0x17) {
                Err("the talk key is one ctrl key that is not the leave key, ctrl-space, ctrl-c, ctrl-t or ctrl-w".to_string())
            } else {
                Ok(b)
            }
        });
        res.map_err(|e| {
            format!(
                "bad [keys] talk {} in {}: {e}",
                sys::go_quote(&self.keys.talk),
                path().display()
            )
        })
    }
}

const KEY_NAME_ERROR: &str = "the leave key is one ctrl key, like ctrl-]";

/// Reads a key name: ctrl-space or ctrl-@, ctrl-a to ctrl-z, ctrl-\,
/// ctrl-], ctrl-^ and ctrl-_, with a plus or a minus after ctrl, in any
/// case. ctrl-[, ctrl-i, ctrl-j and ctrl-m are refused.
pub fn parse_key(name: &str) -> Result<u8, String> {
    let name = name.trim().to_lowercase();
    let rest = name
        .strip_prefix("ctrl-")
        .or_else(|| name.strip_prefix("ctrl+"));
    if let Some(r) = rest {
        if r == "space" || r == "@" {
            return Ok(0);
        }
    }
    let Some(rest) = rest else {
        return Err(KEY_NAME_ERROR.into());
    };
    if rest.len() != 1 {
        return Err(KEY_NAME_ERROR.into());
    }
    let c = rest.as_bytes()[0];
    let b = match c {
        b'a'..=b'z' => c - b'a' + 1,
        b'['..=b'_' => c - b'[' + 0x1b,
        _ => return Err(KEY_NAME_ERROR.into()),
    };
    if matches!(b, 0x09 | 0x0a | 0x0d | 0x1b) {
        return Err(KEY_NAME_ERROR.into());
    }
    Ok(b)
}

/// The name parse_key reads for byte b.
pub fn key_name(b: u8) -> String {
    match b {
        0 => "ctrl-space".into(),
        0x01..=0x1a => format!("ctrl-{}", (b'a' + b - 1) as char),
        0x1b..=0x1f => format!("ctrl-{}", (b'[' + b - 0x1b) as char),
        _ => "?".into(),
    }
}

/// $XDG_CONFIG_HOME/coppice/coppice.toml, or ~/.config/coppice/coppice.toml
/// when XDG_CONFIG_HOME is empty.
pub fn path() -> PathBuf {
    let base = match std::env::var_os("XDG_CONFIG_HOME") {
        Some(b) if !b.is_empty() => PathBuf::from(b),
        _ => sys::home_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join(".config"),
    };
    base.join("coppice").join("coppice.toml")
}

/// Reads the file. Ok(None) when it does not exist; Err when it cannot be
/// read or does not parse.
pub fn load() -> Result<Option<Config>, String> {
    let p = path();
    let b = match std::fs::read(&p) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(sys::go_path_err("open", &p, &e)),
    };
    let text = String::from_utf8(b).map_err(|e| format!("toml: {e}"))?;
    parse(&text).map(Some)
}

/// Reads the text of a config file.
pub fn parse(text: &str) -> Result<Config, String> {
    let raw: toml::Table = toml::from_str(text).map_err(|e| format!("toml: {e}"))?;
    let mut c: Config = raw
        .clone()
        .try_into()
        .map_err(|e: toml::de::Error| format!("toml: {e}"))?;
    c.harness_set = raw.contains_key("harness");
    // The plugin tables are kept as the parser read them, dates as dates.
    c.plugin.clear();
    if let Some(toml::Value::Table(t)) = raw.get("plugin") {
        for (id, v) in t {
            if let toml::Value::Table(pt) = v {
                c.plugin.insert(id.clone(), pt.clone());
            }
        }
    }
    Ok(c)
}

/// One harness Discover found on PATH.
pub struct Found {
    pub name: String,
    pub path: String,
    pub state: String,
}

/// The harnesses coppice knows how to own, in the order Discover looks.
const PROBES: &[(&str, &str)] = &[
    ("claude", "hooks"),
    ("codex", "hooks"),
    ("pi", "rpc"),
    ("sprig", "hooks"),
    ("opencode", "sse"),
];

/// What Discover teaches when PATH holds no harness.
pub const APP_ONLY_NOTE: &str = "No harness on PATH. Codex Desktop, Cursor, and Antigravity are apps, and coppice cannot own their panes. Install claude, codex, pi, sprig, or opencode.";

/// The known harnesses found on PATH, in probe order.
pub fn discover() -> Vec<Found> {
    let mut out = Vec::new();
    for (name, state) in PROBES {
        if let Some(path) = crate::pane::look_path(name) {
            out.push(Found {
                name: name.to_string(),
                path,
                state: state.to_string(),
            });
        }
    }
    out
}

/// A config with one harness table per found harness and def as the
/// default.
pub fn from_found(found: &[Found], def: &str) -> Config {
    let mut c = Config {
        default: def.to_string(),
        harness_set: true,
        ..Default::default()
    };
    for f in found {
        let mut h = Harness {
            command: f.name.clone(),
            state: f.state.clone(),
            ..Default::default()
        };
        if f.name == "claude" {
            h.resume_args = Some(vec!["--resume".into(), "{session}".into()]);
        }
        c.harness.insert(f.name.clone(), h);
    }
    c
}

/// The first line of every file save writes.
const HEADER: &str = "# coppice config. Edit it and save. The next pane reads it.\n";

/// Writes c to the config path: the directory 0700, the file 0600, through
/// a temp file in the same directory renamed into place.
pub fn save(c: &Config) -> Result<(), String> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    let p = path();
    let dir = p.parent().map(PathBuf::from).unwrap_or_default();
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&dir)
        .map_err(|e| sys::go_path_err("mkdir", &dir, &e))?;
    let body = format!("{HEADER}{}", encode(c));
    let mut n: u32 = std::process::id().wrapping_mul(2654435761);
    let (tmp, mut f) = loop {
        n = n.wrapping_mul(1664525).wrapping_add(1013904223);
        let t = dir.join(format!("coppice.toml.{n}"));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&t)
        {
            Ok(f) => break (t, f),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(sys::go_path_err("open", &t, &e)),
        }
    };
    let res = (|| {
        f.set_permissions(std::fs::Permissions::from_mode(0o600))
            .map_err(|e| sys::go_path_err("chmod", &tmp, &e))?;
        f.write_all(body.as_bytes())
            .map_err(|e| sys::go_path_err("write", &tmp, &e))?;
        drop(f);
        std::fs::rename(&tmp, &p).map_err(|e| {
            format!(
                "rename {} {}: {}",
                tmp.display(),
                p.display(),
                sys::go_errno_text(&e)
            )
        })
    })();
    if res.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    res
}

/// The file's text, as BurntSushi/toml encodes Go's Config: the plain keys
/// in field order, then each table in field order, a blank line before each
/// top-level table, each level indented two spaces. A nil list or map is
/// left out; an omitempty field is left out when empty.
pub fn encode(c: &Config) -> String {
    let mut e = Enc::default();
    e.key_eq(0, "default", &Elem::Str(&c.default));
    if let Some(p) = &c.plugins {
        e.key_eq(0, "plugins", &Elem::Strs(p));
    }
    if !c.projects.is_empty() {
        e.key_eq(0, "projects", &Elem::Strs(&c.projects));
    }
    if let Some(g) = &c.gateway {
        e.key_eq(0, "gateway", &Elem::Str(g));
    }
    if c.harness_set {
        e.table_head(&["harness"]);
        for (name, h) in &c.harness {
            let key = ["harness", name.as_str()];
            e.table_head(&key);
            e.key_eq(key.len(), "command", &Elem::Str(&h.command));
            if let Some(a) = &h.args {
                e.key_eq(key.len(), "args", &Elem::Strs(a));
            }
            e.key_eq(key.len(), "state", &Elem::Str(&h.state));
            if let Some(a) = &h.resume_args {
                e.key_eq(key.len(), "resume_args", &Elem::Strs(a));
            }
        }
    }
    if !(c.keys.leave.is_empty() && c.keys.talk.is_empty()) {
        let key = ["keys"];
        e.table_head(&key);
        e.key_eq(key.len(), "leave", &Elem::Str(&c.keys.leave));
        if !c.keys.talk.is_empty() {
            e.key_eq(key.len(), "talk", &Elem::Str(&c.keys.talk));
        }
    }
    if !c.plugin.is_empty() {
        e.table_head(&["plugin"]);
        for (id, t) in &c.plugin {
            let key = vec!["plugin".to_string(), id.clone()];
            e.table(&key, t);
        }
    }
    let v = &c.voice;
    if !(v.enabled.is_none() && v.url.is_empty() && v.token_file.is_empty() && v.args.is_empty()) {
        let key = ["voice"];
        e.table_head(&key);
        if let Some(b) = v.enabled {
            e.key_eq(key.len(), "enabled", &Elem::Val(&toml::Value::Boolean(b)));
        }
        if !v.url.is_empty() {
            e.key_eq(key.len(), "url", &Elem::Str(&v.url));
        }
        if !v.token_file.is_empty() {
            e.key_eq(key.len(), "token_file", &Elem::Str(&v.token_file));
        }
        if !v.args.is_empty() {
            e.key_eq(key.len(), "args", &Elem::Strs(&v.args));
        }
    }
    e.out
}

enum Elem<'a> {
    Str(&'a str),
    Strs(&'a [String]),
    Val(&'a toml::Value),
}

#[derive(Default)]
struct Enc {
    out: String,
}

fn indent(depth: usize) -> String {
    "  ".repeat(depth)
}

/// A key bare when it can be, quoted when not.
fn key_text(k: &str) -> String {
    if !k.is_empty()
        && k.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
    {
        k.to_string()
    } else {
        quote(k)
    }
}

/// A TOML basic string, as BurntSushi/toml quotes one.
fn quote(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{8}' => out.push_str("\\b"),
            '\t' => out.push_str("\\t"),
            '\n' => out.push_str("\\n"),
            '\u{c}' => out.push_str("\\f"),
            '\r' => out.push_str("\\r"),
            c if (c as u32) < 0x20 || c as u32 == 0x7f => {
                out.push_str(&format!("\\u{:04x}", c as u32))
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

fn float_text(f: f64) -> String {
    if f.is_nan() {
        return "nan".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { "inf".into() } else { "-inf".into() };
    }
    let s = format!("{f}");
    if s.contains('.') {
        s
    } else {
        format!("{s}.0")
    }
}

fn is_table_array(a: &[toml::Value]) -> bool {
    !a.is_empty() && a.iter().all(|v| v.is_table())
}

fn is_sub(v: &toml::Value) -> bool {
    match v {
        toml::Value::Table(_) => true,
        toml::Value::Array(a) => is_table_array(a),
        _ => false,
    }
}

fn value_text(v: &toml::Value) -> String {
    match v {
        toml::Value::String(s) => quote(s),
        toml::Value::Integer(i) => i.to_string(),
        toml::Value::Float(f) => float_text(*f),
        toml::Value::Boolean(b) => b.to_string(),
        toml::Value::Datetime(d) => d.to_string(),
        toml::Value::Array(a) => {
            let parts: Vec<String> = a.iter().map(value_text).collect();
            format!("[{}]", parts.join(", "))
        }
        toml::Value::Table(t) => {
            let mut keys: Vec<&String> = t.keys().filter(|k| !is_sub(&t[*k])).collect();
            keys.sort();
            let mut subs: Vec<&String> = t.keys().filter(|k| is_sub(&t[*k])).collect();
            subs.sort();
            let parts: Vec<String> = keys
                .into_iter()
                .chain(subs)
                .map(|k| format!("{} = {}", key_text(k), value_text(&t[k])))
                .collect();
            format!("{{{}}}", parts.join(", "))
        }
    }
}

impl Enc {
    fn key_eq(&mut self, depth: usize, k: &str, v: &Elem) {
        let text = match v {
            Elem::Str(s) => quote(s),
            Elem::Strs(a) => {
                let parts: Vec<String> = a.iter().map(|s| quote(s)).collect();
                format!("[{}]", parts.join(", "))
            }
            Elem::Val(v) => value_text(v),
        };
        self.out
            .push_str(&format!("{}{} = {text}\n", indent(depth), key_text(k)));
    }

    fn head<S: AsRef<str>>(path: &[S]) -> String {
        path.iter()
            .map(|p| key_text(p.as_ref()))
            .collect::<Vec<_>>()
            .join(".")
    }

    fn table_head<S: AsRef<str>>(&mut self, path: &[S]) {
        if path.len() == 1 {
            self.out.push('\n');
        }
        self.out.push_str(&format!(
            "{}[{}]\n",
            indent(path.len() - 1),
            Self::head(path)
        ));
    }

    /// A map value: its header, its plain keys, then its tables.
    fn table(&mut self, path: &[String], t: &toml::Table) {
        self.table_head(path);
        self.body(path, t);
    }

    fn body(&mut self, path: &[String], t: &toml::Table) {
        let mut keys: Vec<&String> = t.keys().collect();
        keys.sort();
        for k in &keys {
            let v = &t[*k];
            if !is_sub(v) {
                self.key_eq(path.len(), k, &Elem::Val(v));
            }
        }
        for k in &keys {
            let v = &t[*k];
            let mut sub = path.to_vec();
            sub.push(k.to_string());
            match v {
                toml::Value::Table(st) => self.table(&sub, st),
                toml::Value::Array(a) if is_table_array(a) => {
                    for el in a {
                        if let toml::Value::Table(st) = el {
                            self.out.push('\n');
                            self.out.push_str(&format!(
                                "{}[[{}]]\n",
                                indent(sub.len() - 1),
                                Self::head(&sub)
                            ));
                            self.body(&sub, st);
                        }
                    }
                }
                _ => {}
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_harness_table_reads() {
        let c = parse(
            "default = \"claude\"\n[harness.claude]\ncommand = \"claude\"\nargs = [\"-x\"]\nresume_args = [\"--resume\", \"{session}\"]\n",
        )
        .unwrap();
        assert_eq!(c.default, "claude");
        let h = &c.harness["claude"];
        assert_eq!(h.command, "claude");
        assert_eq!(h.args(), ["-x"]);
        assert_eq!(c.gateway_url(), DEFAULT_GATEWAY);
        assert!(c.harness_set);
    }

    #[test]
    fn a_wrong_type_refuses_the_file() {
        assert!(parse("default = 3\n").is_err());
        let c = parse("gateway = \"\"\nunknown = 1\n").unwrap();
        assert_eq!(c.gateway_url(), "");
    }

    #[test]
    fn save_keeps_nil_apart_from_empty() {
        let c = parse("plugins = []\n[harness]\n[harness.b]\ncommand = \"b\"\nargs = []\n[harness.a]\ncommand = \"a\"\n").unwrap();
        assert_eq!(
            encode(&c),
            "default = \"\"\nplugins = []\n\n[harness]\n  [harness.a]\n    command = \"a\"\n    state = \"\"\n  [harness.b]\n    command = \"b\"\n    args = []\n    state = \"\"\n"
        );
    }

    #[test]
    fn keys_parse_as_go_reads_them() {
        assert_eq!(parse_key("ctrl-]"), Ok(0x1d));
        assert_eq!(parse_key(" CTRL+Space "), Ok(0));
        assert_eq!(parse_key("ctrl-a"), Ok(1));
        assert!(parse_key("ctrl-m").is_err());
        assert!(parse_key("ctrl-[").is_err());
        assert!(parse_key("alt-a").is_err());
        assert_eq!(key_name(0x1d), "ctrl-]");
        assert_eq!(key_name(0), "ctrl-space");
        assert_eq!(key_name(0x7f), "?");
    }
}
