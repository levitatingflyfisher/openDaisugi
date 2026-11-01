//! The one coppice config file, coppice.toml: the harness Enter opens and
//! the command each harness runs. The server reads it on every request
//! that needs it, so an edit takes effect on the next pane. The command
//! line writes it back with [`save`], in the layout Go's BurntSushi/toml
//! encoder writes.

use std::collections::BTreeMap;
use std::io::Write;
use std::path::{Path, PathBuf};

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
    pub speak_url: String,
    pub speak_model: String,
    pub speak_voice: String,
    pub speak_profile: String,
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
    pub router: Option<String>,
    /// How long after the last layout change the server writes layout.json,
    /// as a duration such as "3s". "0s" writes at once.
    pub layout_save_settle: Option<String>,
    /// How many ".broken-<time>" copies of one name stay. 0 keeps all.
    pub broken_keep: Option<i64>,
    /// Whether a harness starts under `daisugi fence`: "off" (the
    /// default: the fence is opt-in, FN-R-33) never, "on" always, "auto"
    /// while the warden is on.
    pub fence: Option<String>,
    pub voice: Voice,
    pub steward: Steward,
}

/// The [steward] table. It sets how the floor's steward runs.
///
/// mode = "pane" (the default) runs the steward as a harness pane in its
/// own directory. mode = "chat" keeps the steward as one chat that never
/// ends: each sentence the operator says is logged, and a fresh model call
/// answers it. compactor_profile and compactor_model say where the chat's
/// summaries are made. turn_profile is where each turn runs: empty (the
/// default) or "default" is Claude Code, any other value names an operator
/// agent profile. turn_profiles is the same as an ordered list of accounts that
/// all serve ONE model: a turn tries the first account that is not cooling
/// down and moves to the next when one is out of quota or cannot be reached.
/// It never moves to another model. With more than one entry, turn_model
/// must name that model. turn_profile = "a" is
/// turn_profiles = ["a"]; setting both is an error; an empty name in the list
/// is Claude Code. turn_model is the model of every entry
/// (empty means Claude Code's own default, or the profile's own model). A turn may take
/// STEWARD_TURN_TIMEOUT; that is not a setting. Keys that are not settings
/// any more (calls, turn_timeout_s) still load and say nothing.
#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
#[serde(default)]
pub struct Steward {
    pub mode: String,
    pub compactor_profile: String,
    pub compactor_model: String,
    pub turn_profile: String,
    pub turn_profiles: Vec<String>,
    pub turn_model: String,
    /// The Pando R2 store the steward keeps its chat in:
    /// <data home>/pando/<store>. work is the default; home is the other
    /// named store. all is refused: no command reads two stores.
    pub store: String,
    /// The steward's working dir. Empty means the default
    /// ($XDG_STATE_HOME/coppice-steward). It must be an absolute path, or
    /// start with ~/, and sit outside the coppice config, state and data
    /// dirs, which the fence treats as secrets.
    pub dir: String,
}

/// The steward modes.
pub const STEWARD_PANE: &str = "pane";
pub const STEWARD_CHAT: &str = "chat";

/// The compactor's defaults.
pub const DEFAULT_COMPACTOR_PROFILE: &str = "default";
pub const DEFAULT_COMPACTOR_MODEL: &str = "haiku";

/// How long one steward turn may take.
pub const STEWARD_TURN_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(300);

/// The [steward] table with its defaults filled in.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct StewardSettings {
    pub mode: String,
    pub compactor_profile: String,
    /// As the file has it. Empty means haiku on the default profile, and
    /// the profile's own model on any other.
    pub compactor_model: String,
    /// The chain of turn profiles, in order, never empty. An empty name runs
    /// the turn on Claude Code; any other value is the name of an operator
    /// agent profile. turn_profile = "a" reads as ["a"], and neither key
    /// reads as [""].
    pub turn_profiles: Vec<String>,
    pub turn_model: String,
    /// The Pando R2 store name; work when the file names none.
    pub store: String,
    /// The steward's working dir, ~ expanded; empty means the default.
    pub dir: String,
}

impl Config {
    /// Reads the [steward] table. A value that is not allowed is an error
    /// that names the key and the file.
    pub fn steward_settings(&self) -> Result<StewardSettings, String> {
        let f = &self.steward;
        let mut s = StewardSettings {
            mode: STEWARD_PANE.into(),
            compactor_profile: DEFAULT_COMPACTOR_PROFILE.into(),
            compactor_model: f.compactor_model.clone(),
            turn_profiles: Vec::new(),
            turn_model: f.turn_model.clone(),
            store: pando::store::DEFAULT_STORE.into(),
            dir: String::new(),
        };
        if !f.dir.is_empty() {
            let mut d = f.dir.clone();
            if d == "~" || d.starts_with("~/") {
                let Some(home) = sys::home_dir() else {
                    return Err(format!(
                        "[steward] dir is {} in {}, and the home dir is unknown",
                        sys::go_quote(&f.dir),
                        path().display()
                    ));
                };
                d = format!("{}{}", home.to_string_lossy().trim_end_matches('/'), &d[1..]);
            }
            if !d.starts_with('/') {
                return Err(format!(
                    "[steward] dir is {} in {}. Use an absolute path, or one that starts with ~/",
                    sys::go_quote(&f.dir),
                    path().display()
                ));
            }
            s.dir = sys::clean(&d);
        }
        if !f.store.is_empty() {
            if let Err(e) = pando::store::store_dir(Path::new(""), &f.store) {
                return Err(format!(
                    "[steward] store is {} in {}: {}",
                    sys::go_quote(&f.store),
                    path().display(),
                    e
                ));
            }
            s.store = f.store.clone();
        }
        if !f.turn_profile.is_empty() && !f.turn_profiles.is_empty() {
            return Err(format!(
                "[steward] sets both turn_profile and turn_profiles in {}. Keep turn_profiles and delete turn_profile",
                path().display()
            ));
        }
        if f.turn_profiles.len() > 1 && f.turn_model.is_empty() {
            return Err(format!(
                "[steward] turn_profiles lists {} accounts in {}, so turn_model must name the one model they all serve",
                f.turn_profiles.len(),
                path().display()
            ));
        }
        let mut seen = std::collections::HashSet::new();
        for p in &f.turn_profiles {
            let name = if p == "default" { "" } else { p.as_str() };
            if !seen.insert(name.to_string()) {
                let shown = if p.is_empty() { "\"\" (Claude Code)".to_string() } else { p.clone() };
                return Err(format!(
                    "[steward] turn_profiles lists the account {} twice in {}. Each account may appear once in the turn chain (\"\" and \"default\" are the same login)",
                    shown,
                    path().display()
                ));
            }
        }
        s.turn_profiles = if f.turn_profiles.is_empty() {
            vec![f.turn_profile.clone()]
        } else {
            f.turn_profiles.clone()
        };
        if !f.mode.is_empty() {
            if f.mode != STEWARD_PANE && f.mode != STEWARD_CHAT {
                return Err(format!(
                    "[steward] mode is {} in {}. Use \"pane\" or \"chat\"",
                    sys::go_quote(&f.mode),
                    path().display()
                ));
            }
            s.mode = f.mode.clone();
        }
        if !f.compactor_profile.is_empty() {
            s.compactor_profile = f.compactor_profile.clone();
        }
        Ok(s)
    }
}

impl StewardSettings {
    /// The model the compactor asks for, given the model the compactor's
    /// profile names ("" for none).
    pub fn compactor_model_for(&self, profile_model: &str) -> String {
        if !self.compactor_model.is_empty() {
            return self.compactor_model.clone();
        }
        if self.compactor_profile != DEFAULT_COMPACTOR_PROFILE && !profile_model.is_empty() {
            return profile_model.to_string();
        }
        DEFAULT_COMPACTOR_MODEL.to_string()
    }
}

/// The wait after the last layout change.
pub const DEFAULT_LAYOUT_SAVE_SETTLE: std::time::Duration = std::time::Duration::from_secs(3);

/// Parses a Go-style duration such as "3s", "500ms" or "1m30s". A negative
/// value is zero. None when it does not parse.
fn parse_duration(s: &str) -> Option<std::time::Duration> {
    let s = s.trim();
    let (neg, mut rest) = match s.strip_prefix('-') {
        Some(r) => (true, r),
        None => (false, s.strip_prefix('+').unwrap_or(s)),
    };
    if rest == "0" {
        return Some(std::time::Duration::ZERO);
    }
    if rest.is_empty() {
        return None;
    }
    let mut total = 0f64;
    while !rest.is_empty() {
        let end = rest
            .find(|c: char| !(c.is_ascii_digit() || c == '.'))
            .unwrap_or(rest.len());
        let num: f64 = rest[..end].parse().ok()?;
        rest = &rest[end..];
        let uend = rest
            .find(|c: char| c.is_ascii_digit() || c == '.')
            .unwrap_or(rest.len());
        let unit = match &rest[..uend] {
            "ns" => 1e-9,
            "us" => 1e-6,
            "ms" => 1e-3,
            "s" => 1.0,
            "m" => 60.0,
            "h" => 3600.0,
            _ => return None,
        };
        rest = &rest[uend..];
        total += num * unit;
    }
    if neg {
        return Some(std::time::Duration::ZERO);
    }
    std::time::Duration::try_from_secs_f64(total).ok()
}

/// Where daisugi's router listens with no --host or --port.
pub const DEFAULT_ROUTER: &str = "http://127.0.0.1:8787";

/// The plugin list of a file with no plugins key.
pub fn default_plugins() -> Vec<String> {
    [
        "tree",
        "cards",
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
    /// The router base URL the floor compares with: the router key, else
    /// the default. Empty when the file says router = "".
    pub fn router_url(&self) -> String {
        match &self.router {
            None => DEFAULT_ROUTER.to_string(),
            Some(g) => g.clone(),
        }
    }

    /// The layout save wait: layout_save_settle when it parses, else 3s.
    pub fn layout_settle(&self) -> std::time::Duration {
        match &self.layout_save_settle {
            None => DEFAULT_LAYOUT_SAVE_SETTLE,
            Some(s) => parse_duration(s).unwrap_or(DEFAULT_LAYOUT_SAVE_SETTLE),
        }
    }

    /// The fence setting: "auto", "on" or "off". Unset is "auto": agent work
    /// runs under the fence while the warden is on, and a plain shell pane
    /// stays bare (ADR-0028). parse refuses an unknown value.
    pub fn fence_mode(&self) -> String {
        match self.fence.as_deref() {
            Some(m @ ("on" | "off" | "auto")) => m.to_string(),
            _ => "auto".to_string(),
        }
    }

    /// The broken copies to keep: broken_keep, else 5. 0 keeps all.
    pub fn broken_keep_n(&self) -> usize {
        match self.broken_keep {
            Some(n) if n >= 0 => n as usize,
            _ => crate::broken::DEFAULT_KEEP,
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

/// The top-level key and the table header from before the rename. The loader
/// does not accept either: a key it does not know would be dropped, and the
/// floor would run with a router or a steward the owner did not choose. The
/// owner edits the file once.
fn stale_names(text: &str) -> Result<(), String> {
    let mut top = true;
    for ln in text.split('\n') {
        let ln = ln.trim_end_matches('\r');
        let t = ln.trim();
        let compact: String = t.chars().filter(|c| !c.is_whitespace()).collect();
        if compact == "[foreman]" { // names-ok: the old table
            return Err("coppice.toml: [foreman] is now [steward]".to_string()); // names-ok: the old table
        }
        if t.starts_with('[') {
            top = false;
        }
        if top && ln.trim_start().strip_prefix("gateway").is_some_and(|r| r.trim_start().starts_with('=')) { // names-ok: the old key
            return Err("coppice.toml: gateway is now router".to_string()); // names-ok: the old key
        }
    }
    Ok(())
}

/// Reads the text of a config file.
pub fn parse(text: &str) -> Result<Config, String> {
    stale_names(text)?;
    // The fence key is checked on the text first, so a bare word or a
    // boolean gives the fence error, not a parse error (FN-R-37).
    let fence = fence_raw(text);
    if let Some(v) = &fence {
        if !matches!(v.as_str(), "\"on\"" | "\"off\"" | "\"auto\"" | "'on'" | "'off'" | "'auto'") {
            return Err(format!("{BAD_FENCE}{}.", first_line(v)));
        }
    }
    let bad_file = |e: String| {
        if fence.is_some() {
            format!("{BAD_FENCE_FILE}{}", first_line(&e))
        } else {
            e
        }
    };
    let raw: toml::Table = toml::from_str(text).map_err(|e| bad_file(format!("toml: {e}")))?;
    let mut c: Config = raw
        .clone()
        .try_into()
        .map_err(|e: toml::de::Error| bad_file(format!("toml: {e}")))?;
    check_fence(&c)?;
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

/// The start of the error of a fence value coppice does not know.
const BAD_FENCE: &str = "fence must be \"on\", \"off\" or \"auto\", not ";
/// The start of the error of a file that does not parse while it sets fence.
const BAD_FENCE_FILE: &str = "the file does not parse, and it sets fence, so no server starts: ";

fn first_line(s: &str) -> &str {
    s.split('\n').next().unwrap_or("")
}

/// A config error for a fence value that is not on, off or auto: a typo
/// never reads as a fence that is off (FN-R-33).
fn check_fence(c: &Config) -> Result<(), String> {
    match c.fence.as_deref() {
        None | Some("on" | "off" | "auto") => Ok(()),
        Some(v) => Err(format!("{BAD_FENCE}{}.", sys::go_quote(v))),
    }
}

/// `fenceRaw`: the raw text of the top-level fence value (before the first
/// table header), with a trailing comment cut, if there is one.
fn fence_raw(text: &str) -> Option<String> {
    for ln in text.split('\n') {
        let ln = ln.strip_suffix('\r').unwrap_or(ln);
        if ln.trim().starts_with('[') {
            return None;
        }
        let t = ln.trim_start_matches([' ', '\t']);
        let Some(rest) = t.strip_prefix("fence") else { continue };
        let rest = rest.trim_start_matches([' ', '\t']);
        let Some(v) = rest.strip_prefix('=') else { continue };
        let v = v.trim();
        if let Some(q) = v.chars().next().filter(|c| *c == '"' || *c == '\'') {
            if let Some(i) = v[1..].find(q) {
                return Some(v[..i + 2].to_string());
            }
            return Some(v.to_string());
        }
        let v = match v.find('#') {
            Some(i) => v[..i].trim(),
            None => v,
        };
        return Some(v.to_string());
    }
    None
}

/// The line a server start prints for a bad fence value, or None: the
/// file, the value and the ones allowed; the server does not start.
pub fn fence_refusal(loaded: &Result<Option<Config>, String>, path: &Path) -> Option<String> {
    match loaded {
        Err(e) if e.starts_with(BAD_FENCE) || e.starts_with(BAD_FENCE_FILE) => {
            Some(format!("cannot read {}: {}", path.display(), first_line(e)))
        }
        _ => None,
    }
}

/// One harness Discover found on PATH. `command` is what the config runs:
/// the harness name, or the real binary when the one on PATH is a mise
/// wrapper script (see `resolve_mise`). `note` says so, or is empty.
pub struct Found {
    pub name: String,
    pub path: String,
    pub state: String,
    pub command: String,
    pub note: String,
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

/// The largest file taken for a wrapper script. Real binaries are larger;
/// wrappers are three lines.
const MAX_WRAPPER_SIZE: u64 = 2048;

/// Why `mise which` was not asked or did not answer.
pub enum MiseErr {
    /// The home dir is unknown: mise would run in the caller's folder,
    /// where a mise.toml could change the answer.
    NoHome,
    Failed,
}

/// `mise which NAME`, run in the home dir.
pub fn mise_which(name: &str) -> Result<String, MiseErr> {
    let Some(home) = sys::home_dir().filter(|h| !h.as_os_str().is_empty()) else {
        return Err(MiseErr::NoHome);
    };
    let out = std::process::Command::new("mise")
        .arg("which")
        .arg(name)
        .current_dir(home)
        .stdin(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .output()
        .map_err(|_| MiseErr::Failed)?;
    if !out.status.success() {
        return Err(MiseErr::Failed);
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// The real binary behind `path` when it is a small shell script that
/// execs `mise x ... -- NAME`. The fence leaves mise no place to write its
/// state, so such a wrapper dies at once in a fenced pane. Detection reads
/// the file and never runs it; the one command run is `mise which NAME`,
/// and its answer must be an absolute path to a regular file that is not
/// the wrapper itself. The second value is true when the wrapper was left
/// as it is because the home dir is unknown.
pub fn resolve_mise(path: &str, which: &dyn Fn(&str) -> Result<String, MiseErr>) -> (Option<String>, bool) {
    use std::sync::OnceLock;
    static SHEBANG: OnceLock<regex::Regex> = OnceLock::new();
    static EXEC: OnceLock<regex::Regex> = OnceLock::new();
    let shebang = SHEBANG.get_or_init(|| {
        regex::Regex::new(r"^#![\t\n\f\r ]*(?:/usr/bin/env[\t\n\f\r ]+)?(?:[^\t\n\f\r ]*/)?(?:ba|da|z)?sh(?-u:\b)").unwrap()
    });
    let exec = EXEC.get_or_init(|| {
        regex::Regex::new(r#"^exec[\t\n\f\r ]+mise[\t\n\f\r ]+(?:x|exec)[\t\n\f\r ]+(?:[^\t\n\f\r ]+[\t\n\f\r ]+)*?--[\t\n\f\r ]+([A-Za-z0-9._-]+)(?:[\t\n\f\r ]+"\$@")?[\t\n\f\r ]*$"#).unwrap()
    });
    let Ok(m) = std::fs::metadata(path) else { return (None, false) };
    if !m.is_file() || m.len() > MAX_WRAPPER_SIZE {
        return (None, false);
    }
    let Ok(raw) = std::fs::read(path) else { return (None, false) };
    let text = String::from_utf8_lossy(&raw);
    let mut lines = text.lines();
    if !lines.next().is_some_and(|l| shebang.is_match(l)) {
        return (None, false);
    }
    let mut tool = String::new();
    for l in lines {
        if let Some(c) = exec.captures(l.trim()) {
            tool = c[1].to_string();
        }
    }
    let base = Path::new(path).file_name().map(|b| b.to_string_lossy().into_owned()).unwrap_or_default();
    if tool.is_empty() || tool != base {
        return (None, false);
    }
    let out = match which(&tool) {
        Err(MiseErr::NoHome) => return (None, true),
        Err(MiseErr::Failed) => return (None, false),
        Ok(o) => o,
    };
    if !out.starts_with('/') {
        return (None, false);
    }
    match std::fs::metadata(&out) {
        Ok(m) if m.is_file() => {}
        _ => return (None, false),
    }
    if let (Ok(a), Ok(b)) = (std::fs::canonicalize(&out), std::fs::canonicalize(path)) {
        if a == b {
            return (None, false);
        }
    }
    (Some(out), false)
}

/// The known harnesses found on PATH, in probe order.
pub fn discover() -> Vec<Found> {
    discover_with(&|n| crate::pane::look_path(n), &mise_which)
}

/// `discover` with the PATH lookup and the `mise which` call named.
pub fn discover_with(
    look: &dyn Fn(&str) -> Option<String>,
    which: &dyn Fn(&str) -> Result<String, MiseErr>,
) -> Vec<Found> {
    let mut out = Vec::new();
    for (name, state) in PROBES {
        let Some(path) = look(name).filter(|p| !p.is_empty()) else { continue };
        let mut f = Found {
            name: name.to_string(),
            path: path.clone(),
            state: state.to_string(),
            command: String::new(),
            note: String::new(),
        };
        match resolve_mise(&path, which) {
            (Some(real), _) => {
                f.note = format!(
                    "{name} on PATH is a mise wrapper ({path}). The config runs {real} instead, because mise cannot write its state under the fence."
                );
                f.command = real;
            }
            (None, true) => {
                f.note = format!(
                    "{name} on PATH is a mise wrapper ({path}), and the home dir is unknown, so mise was not asked for the real binary. The config runs {name} as it is; under the fence mise may not start."
                );
            }
            _ => {}
        }
        out.push(f);
    }
    out
}

/// The lines to print for the harnesses whose command differs from their
/// name.
pub fn found_notes(found: &[Found]) -> Vec<String> {
    found.iter().filter(|f| !f.note.is_empty()).map(|f| f.note.clone()).collect()
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
            command: if f.command.is_empty() { f.name.clone() } else { f.command.clone() },
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
    if let Some(g) = &c.router {
        e.key_eq(0, "router", &Elem::Str(g));
    }
    if let Some(s) = &c.layout_save_settle {
        e.key_eq(0, "layout_save_settle", &Elem::Str(s));
    }
    if let Some(n) = c.broken_keep {
        e.key_eq(0, "broken_keep", &Elem::Val(&toml::Value::Integer(n)));
    }
    if let Some(f) = &c.fence {
        e.key_eq(0, "fence", &Elem::Str(f));
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
    let f = &c.steward;
    if *f != Steward::default() {
        let key = ["steward"];
        e.table_head(&key);
        for (k, v) in [
            ("mode", &f.mode),
            ("compactor_profile", &f.compactor_profile),
            ("compactor_model", &f.compactor_model),
            ("turn_profile", &f.turn_profile),
        ] {
            if !v.is_empty() {
                e.key_eq(key.len(), k, &Elem::Str(v));
            }
        }
        if !f.turn_profiles.is_empty() {
            e.key_eq(key.len(), "turn_profiles", &Elem::Strs(&f.turn_profiles));
        }
        if !f.turn_model.is_empty() {
            e.key_eq(key.len(), "turn_model", &Elem::Str(&f.turn_model));
        }
        if !f.store.is_empty() {
            e.key_eq(key.len(), "store", &Elem::Str(&f.store));
        }
        if !f.dir.is_empty() {
            e.key_eq(key.len(), "dir", &Elem::Str(&f.dir));
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

    #[test]
    fn the_old_foreman_table_and_gateway_key_are_refused() { // names-ok
        assert_eq!(parse("[foreman]\nmode = \"chat\"\n").unwrap_err(), "coppice.toml: [foreman] is now [steward]"); // names-ok
        assert_eq!(parse("gateway = \"http://127.0.0.1:8787\"\n").unwrap_err(), "coppice.toml: gateway is now router"); // names-ok
        assert!(parse("[plugin.x]\ngateway = \"y\"\n").is_ok()); // names-ok: a key in a table is that table's own
    }
    use super::*;

    /// FN-R-33: an unknown fence value is a config error naming the value
    /// and the ones allowed, never a silent "off"; a server does not start.
    #[test]
    fn an_unknown_fence_value_is_a_config_error() {
        assert_eq!(parse("fence = \"yes\"\n").err().as_deref(), Some("fence must be \"on\", \"off\" or \"auto\", not \"yes\"."));
        assert!(parse("fence = \"auto\"\n").is_ok());
        let e = parse("fence = \"yes\"\n").err().unwrap();
        assert_eq!(
            fence_refusal(&Err(e), std::path::Path::new("/c/coppice.toml")).as_deref(),
            Some("cannot read /c/coppice.toml: fence must be \"on\", \"off\" or \"auto\", not \"yes\".")
        );
        // FN-R-37: every other shape, and a file that does not parse while
        // it holds a fence key, is the same error, one line.
        for (text, want) in [
            ("fence = on\n", "fence must be \"on\", \"off\" or \"auto\", not on."),
            ("fence = true\n", "fence must be \"on\", \"off\" or \"auto\", not true."),
            ("fence = 1\n", "fence must be \"on\", \"off\" or \"auto\", not 1."),
            ("fence = \"on\"\nbroken = \n", "the file does not parse, and it sets fence"),
        ] {
            let e = parse(text).err().unwrap_or_default();
            let r = fence_refusal(&Err(e.clone()), std::path::Path::new("/c")).unwrap_or_default();
            assert!(r.contains(want) && !r.contains('\n'), "{text:?}: {e:?} -> {r:?}");
        }
        assert_eq!(fence_refusal(&parse("broken = \n").map(Some), std::path::Path::new("/c")), None);
        assert!(parse("[harness.x]\ncommand = \"x\"\nfence = 1\n").map_or(true, |_| true));
        assert_eq!(fence_refusal(&parse("[harness.x]\ncommand = \"x\"\nfence = 1\n").map(Some), std::path::Path::new("/c")), None);
        assert_eq!(fence_refusal(&Err("toml: bad".into()), std::path::Path::new("/c")), None);
        assert_eq!(fence_refusal(&Ok(None), std::path::Path::new("/c")), None);
    }

    /// ADR-0028: with no fence key the mode is "auto"; "on" and "off" are
    /// the operator's own choice.
    #[test]
    fn the_fence_is_auto_unless_the_config_says_otherwise() {
        assert_eq!(Config::default().fence_mode(), "auto");
        for v in ["on", "auto", "off"] {
            let c = Config { fence: Some(v.into()), ..Default::default() };
            assert_eq!(c.fence_mode(), v);
        }
    }

    #[test]
    fn a_huge_layout_save_settle_falls_back_to_the_default() {
        let c = Config {
            layout_save_settle: Some("99999999999999999999h".into()),
            ..Default::default()
        };
        assert_eq!(c.layout_settle(), DEFAULT_LAYOUT_SAVE_SETTLE);
    }

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
        assert_eq!(c.router_url(), DEFAULT_ROUTER);
        assert!(c.harness_set);
    }

    #[test]
    fn a_wrong_type_refuses_the_file() {
        assert!(parse("default = 3\n").is_err());
        let c = parse("router = \"\"\nunknown = 1\n").unwrap();
        assert_eq!(c.router_url(), "");
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

    #[test]
    fn the_steward_table_defaults_to_pane_mode() {
        let s = Config::default().steward_settings().unwrap();
        assert_eq!(
            s,
            StewardSettings {
                mode: "pane".into(),
                compactor_profile: "default".into(),
                turn_profiles: vec![String::new()],
                store: "work".into(),
                ..Default::default()
            }
        );
        assert_eq!(s.compactor_model_for("x"), "haiku");
    }

    fn scratch(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("coppice-disc-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn put(p: &Path, body: &str) {
        use std::os::unix::fs::PermissionsExt;
        std::fs::write(p, body).unwrap();
        std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o755)).unwrap();
    }

    #[test]
    fn a_mise_wrapper_resolves_to_the_real_binary() {
        let dir = scratch("wrap");
        std::fs::create_dir_all(dir.join("real")).unwrap();
        let real = dir.join("real/claude");
        put(&real, "\x7fELF");
        let wrap = dir.join("claude");
        put(&wrap, "#!/usr/bin/env bash\nmise use -g node@lts claude\nexec mise x claude -- claude \"$@\"\n");
        let asked = std::cell::RefCell::new(Vec::new());
        let real_s = real.to_string_lossy().into_owned();
        let wrap_s = wrap.to_string_lossy().into_owned();
        let found = discover_with(
            &|n| (n == "claude").then(|| wrap_s.clone()),
            &|n| {
                asked.borrow_mut().push(n.to_string());
                Ok(real_s.clone())
            },
        );
        assert_eq!(found.len(), 1);
        assert_eq!(found[0].command, real_s);
        assert!(found[0].note.contains("mise wrapper"), "{}", found[0].note);
        assert_eq!(*asked.borrow(), vec!["claude".to_string()]);
        assert_eq!(from_found(&found, "claude").harness["claude"].command, real_s);
        assert_eq!(found_notes(&found).len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn only_a_narrow_mise_wrapper_is_resolved() {
        let dir = scratch("narrow");
        let never = |n: &str| -> Result<String, MiseErr> { panic!("mise asked about {n}") };
        let pad = "# pad\n".repeat(500);
        for (n, body) in [
            ("codex", "\x7fELF binary".to_string()),
            ("pi", "#!/bin/sh\nexec mise x other -- other \"$@\"\n".to_string()),
            ("sprig", "#!/bin/sh\necho hi\n".to_string()),
            ("opencode", format!("#!/bin/sh\n{pad}exec mise x opencode -- opencode \"$@\"\n")),
        ] {
            let p = dir.join(n);
            put(&p, &body);
            assert!(resolve_mise(&p.to_string_lossy(), &never).0.is_none(), "{n} resolved");
        }
        let c = dir.join("claude");
        put(&c, "#!/bin/bash\nexec mise x claude -- claude \"$@\"\n");
        let rel = |_: &str| -> Result<String, MiseErr> { Ok("claude".into()) };
        assert!(resolve_mise(&c.to_string_lossy(), &rel).0.is_none(), "a relative answer was taken");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn with_no_home_dir_mise_is_not_asked_and_the_note_says_so() {
        let dir = scratch("nohome");
        let w = dir.join("claude");
        put(&w, "#!/bin/bash\nexec mise x claude -- claude \"$@\"\n");
        let ws = w.to_string_lossy().into_owned();
        let found = discover_with(&|n| (n == "claude").then(|| ws.clone()), &|_| Err(MiseErr::NoHome));
        assert_eq!(found.len(), 1);
        assert!(found[0].command.is_empty());
        let notes = found_notes(&found);
        assert!(notes.len() == 1 && notes[0].contains("home dir is unknown") && notes[0].contains("mise was not asked"), "{notes:?}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_steward_dir_is_absolute_or_home_relative() {
        let c = parse("[steward]\ndir = \"/srv/st/../steward\"\n").unwrap();
        assert_eq!(c.steward_settings().unwrap().dir, "/srv/steward");
        assert!(encode(&c).contains("dir = \"/srv/st/../steward\""));
        let e = parse("[steward]\ndir = \"rel/dir\"\n").unwrap().steward_settings().unwrap_err();
        assert!(e.starts_with("[steward] dir is \"rel/dir\" in ") && e.ends_with("Use an absolute path, or one that starts with ~/"), "{e}");
        let c = parse("[steward]\ndir = \"~/st\"\n").unwrap();
        if let Some(h) = sys::home_dir() {
            assert_eq!(c.steward_settings().unwrap().dir, format!("{}/st", h.to_string_lossy().trim_end_matches('/')));
        }
    }

    #[test]
    fn the_steward_table_reads_and_stays_out_when_empty() {
        let c = parse("default = \"claude\"\n").unwrap();
        assert!(!encode(&c).contains("steward"));
        let text = "[steward]\nmode = \"chat\"\ncompactor_profile = \"desk\"\ncompactor_model = \"m1\"\nturn_profile = \"desk\"\nturn_model = \"sonnet\"\n";
        let c = parse(text).unwrap();
        assert_eq!(
            c.steward_settings().unwrap(),
            StewardSettings {
                mode: "chat".into(),
                compactor_profile: "desk".into(),
                compactor_model: "m1".into(),
                turn_profiles: vec!["desk".into()],
                turn_model: "sonnet".into(),
                store: "work".into(),
                dir: String::new(),
            }
        );
        // A save keeps the table, in the layout Go writes.
        assert_eq!(
            encode(&c),
            "default = \"\"\n\n[steward]\n  mode = \"chat\"\n  compactor_profile = \"desk\"\n  compactor_model = \"m1\"\n  turn_profile = \"desk\"\n  turn_model = \"sonnet\"\n"
        );
    }

    #[test]
    fn a_compactor_model_on_another_profile_follows_the_profile() {
        let mut s = StewardSettings {
            compactor_profile: "desk".into(),
            ..Default::default()
        };
        assert_eq!(s.compactor_model_for("llama3.3"), "llama3.3");
        s.compactor_model = "pinned".into();
        assert_eq!(s.compactor_model_for("llama3.3"), "pinned");
    }

    #[test]
    fn a_steward_mode_that_is_not_allowed_names_the_key() {
        let c = parse("[steward]\nmode = \"wide\"\n").unwrap();
        let e = c.steward_settings().unwrap_err();
        assert!(e.starts_with("[steward] mode is \"wide\" in "), "{e}");
        assert!(e.ends_with(". Use \"pane\" or \"chat\""), "{e}");
    }

    #[test]
    fn turn_profiles_is_an_ordered_chain_and_turn_profile_a_one_entry_chain() {
        let c = parse("[steward]\nturn_profiles = [\"\", \"desk\", \"grok\"]\nturn_model = \"opus\"\n").unwrap();
        assert_eq!(
            c.steward_settings().unwrap().turn_profiles,
            vec!["", "desk", "grok"]
        );
        let c = parse("[steward]\nturn_profile = \"desk\"\n").unwrap();
        assert_eq!(c.steward_settings().unwrap().turn_profiles, vec!["desk"]);
    }

    #[test]
    fn several_accounts_need_the_one_model_named() {
        let c = parse("[steward]\nturn_profiles = [\"\", \"desk\"]\n").unwrap();
        let e = c.steward_settings().unwrap_err();
        assert!(e.contains("turn_model") && e.contains("turn_profiles"), "{e}");
        let c = parse("[steward]\nturn_profiles = [\"desk\"]\n").unwrap();
        assert!(c.steward_settings().is_ok());
    }

    #[test]
    fn setting_both_turn_keys_names_both() {
        let c = parse("[steward]\nturn_profile = \"a\"\nturn_profiles = [\"b\"]\n").unwrap();
        let e = c.steward_settings().unwrap_err();
        assert!(e.contains("turn_profile") && e.contains("turn_profiles"), "{e}");
    }

    #[test]
    fn turn_profiles_refuse_the_same_account_twice() {
        for list in ["[\"\", \"default\"]", "[\"\", \"\"]", "[\"a\", \"b\", \"a\"]"] {
            let c = parse(&format!("[steward]\nturn_profiles = {list}\nturn_model = \"m\"\n")).unwrap();
            let e = c.steward_settings().unwrap_err();
            assert!(e.contains("twice"), "{list}: {e}");
        }
        let c = parse("[steward]\nturn_profiles = [\"\", \"a\"]\nturn_model = \"m\"\n").unwrap();
        assert!(c.steward_settings().is_ok());
    }

    #[test]
    fn turn_profiles_write_back_in_the_layout_go_writes() {
        let c = parse("[steward]\nmode = \"chat\"\nturn_profiles = [\"\", \"desk\"]\nturn_model = \"opus\"\n").unwrap();
        assert!(encode(&c).contains("turn_profiles"), "{}", encode(&c));
    }

    #[test]
    fn an_old_steward_table_with_cut_keys_still_loads() {
        let c = parse("[steward]\nmode = \"chat\"\ncalls = 3\nturn_timeout_s = 60\n").unwrap();
        assert_eq!(c.steward_settings().unwrap().mode, "chat");
    }
}
