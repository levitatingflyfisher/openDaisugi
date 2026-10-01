//! coppice plugins. A plugin is a directory with a manifest.json. A view is
//! a page the web server serves; it holds nothing. A policy is a process
//! the server starts, which listens to events and holds only the verbs its
//! manifest names. No plugin may hold agent.allow or agent.deny.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Map, Value};

use crate::godec::{self, J};
use crate::sys;

mod shipped {
    include!(concat!(env!("OUT_DIR"), "/shipped.rs"));
}

/// The message a manifest gets when it asks for an allow verb.
pub const REFUSAL: &str = "a plugin can propose. It cannot allow.";
/// Ids that only the shipped plugins may use start with this.
pub const RESERVED_PREFIX: &str = "coppice.";
pub const KIND_VIEW: &str = "view";
pub const KIND_POLICY: &str = "policy";

/// One manifest.json.
#[derive(Clone, Debug, Default)]
pub struct Manifest {
    pub id: String,
    pub kind: String,
    pub page: String,
    pub run: String,
    pub listens: Vec<String>,
    pub needs: Vec<String>,
    pub title: String,
    pub about: String,
    pub ring: bool,
    /// The plugin's own settings, as Go reads them: numbers as float64.
    pub config: Map<String, Value>,
}

/// Where a plugin's files are.
#[derive(Clone, Debug)]
pub enum Files {
    /// A plugin the binary carries, by its id.
    Shipped(String),
    /// A directory on disk, inside the root it was found in.
    Dir { root: PathBuf, dir: PathBuf },
}

#[derive(Clone, Debug)]
pub struct Plugin {
    pub m: Manifest,
    /// The plugin directory on disk; empty for a shipped plugin.
    pub dir: String,
    /// The plugin's files; the phone server serves a view's page from here.
    pub files: Files,
    pub shipped: bool,
}

/// One plugin that did not load, and why.
pub struct Problem {
    pub id: String,
    pub dir: String,
    pub message: String,
}

impl std::fmt::Display for Problem {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.dir.is_empty() {
            write!(f, "plugin {}: {}", self.id, self.message)
        } else {
            write!(f, "plugin {} in {}: {}", self.id, self.dir, self.message)
        }
    }
}

/// Go's fs.ValidPath.
fn valid_path(name: &str) -> bool {
    if name == "." {
        return true;
    }
    if name.is_empty() {
        return false;
    }
    name.split('/')
        .all(|e| !e.is_empty() && e != "." && e != "..")
}

fn id_ok(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b[0].is_ascii_lowercase()
        && b.iter()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'-')
}

/// A verb name: words of a-z and _, joined by dots, at least two.
fn verb_ok(s: &str) -> bool {
    let parts: Vec<&str> = s.split('.').collect();
    parts.len() >= 2
        && parts.iter().all(|p| {
            let b = p.as_bytes();
            !b.is_empty()
                && b[0].is_ascii_lowercase()
                && b.iter().all(|c| c.is_ascii_lowercase() || *c == b'_')
        })
}

const LISTENABLE: &[&str] = &["state", "layout", "note", "child"];

/// The shipped file at rel, when there is one.
fn shipped_file(rel: &str) -> Option<&'static [u8]> {
    shipped::FILES
        .iter()
        .find(|(p, _)| *p == rel)
        .map(|(_, b)| *b)
}

fn shipped_has_dir(rel: &str) -> bool {
    let pre = format!("{rel}/");
    shipped::FILES.iter().any(|(p, _)| p.starts_with(&pre))
}

/// The files of the shipped tree under prefix, with their paths relative
/// to it.
pub fn shipped_under(prefix: &str) -> Vec<(&'static str, &'static [u8])> {
    let pre = format!("{prefix}/");
    shipped::FILES
        .iter()
        .filter_map(|(p, b)| p.strip_prefix(pre.as_str()).map(|r| (r, *b)))
        .collect()
}

/// path inside root, following links, or None when it leaves root.
fn inside(root: &Path, path: &Path) -> Result<Option<PathBuf>, std::io::Error> {
    let real = path.canonicalize()?;
    let r = root.canonicalize()?;
    Ok(real.starts_with(&r).then_some(real))
}

/// What fs.Stat says of name in a plugin's files: Some(is_dir), or None
/// when there is nothing there.
fn stat(files: &Files, name: &str) -> Option<bool> {
    match files {
        Files::Shipped(id) => {
            let rel = format!("{id}/{name}");
            if shipped_file(&rel).is_some() {
                Some(false)
            } else if shipped_has_dir(&rel) {
                Some(true)
            } else {
                None
            }
        }
        Files::Dir { root, dir } => {
            let p = inside(root, &dir.join(name)).ok().flatten()?;
            std::fs::metadata(p).ok().map(|m| m.is_dir())
        }
    }
}

/// A regular file of a plugin, for the web server: its bytes (at most
/// 8 MiB, as Go reads them) and its change time in Unix seconds, None for a
/// file the binary carries. None when there is no regular file there.
pub fn read_file(files: &Files, name: &str) -> Option<(Vec<u8>, Option<i64>)> {
    const LIMIT: u64 = 8 << 20;
    match files {
        Files::Shipped(id) => shipped_file(&format!("{id}/{name}"))
            .map(|b| (b[..b.len().min(LIMIT as usize)].to_vec(), None)),
        Files::Dir { root, dir } => {
            let p = inside(root, &dir.join(name)).ok().flatten()?;
            let md = std::fs::metadata(&p).ok()?;
            if !md.is_file() {
                return None;
            }
            use std::io::Read;
            let mut b = Vec::new();
            std::fs::File::open(&p)
                .ok()?
                .take(LIMIT)
                .read_to_end(&mut b)
                .ok()?;
            let mtime = md
                .modified()
                .ok()
                .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
                .map(|d| d.as_secs() as i64);
            Some((b, mtime))
        }
    }
}

/// A file of the library every view imports, the shipped _lib.
pub fn lib_file(name: &str) -> Option<&'static [u8]> {
    shipped_file(&format!("_lib/{name}"))
}

fn check_file(files: &Files, name: &str) -> String {
    if name.starts_with('/') || !valid_path(name) || name.contains('\\') {
        return format!("{name} must name a file inside the plugin directory.");
    }
    match stat(files, name) {
        Some(false) => String::new(),
        _ => format!("there is no file {name} in the plugin directory."),
    }
}

/// Go's json.Decoder with DisallowUnknownFields, decoding the first value
/// of raw into a Manifest.
fn decode_manifest(raw: &[u8]) -> Result<Manifest, String> {
    if raw
        .iter()
        .all(|c| matches!(c, b' ' | b'\t' | b'\r' | b'\n'))
    {
        return Err("EOF".into());
    }
    let first: &[u8] = match godec::check(raw) {
        Ok(()) => raw,
        Err(e) if e.contains("after top-level value") => {
            let mut it =
                serde_json::Deserializer::from_slice(raw).into_iter::<serde::de::IgnoredAny>();
            let _ = it.next();
            &raw[..it.byte_offset()]
        }
        Err(e) if e == "unexpected end of JSON input" => return Err("unexpected EOF".into()),
        Err(e) => return Err(e),
    };
    let doc = godec::parse(first)?;
    let mut m = Manifest::default();
    let J::Obj(kv) = &doc.root.v else {
        let kind = match &doc.root.v {
            J::Arr(_) => "array",
            J::Str(_) => "string",
            J::Num => "number",
            J::Bool(_) => "bool",
            _ => "null",
        };
        if matches!(doc.root.v, J::Null) {
            return Ok(m);
        }
        return Err(format!(
            "json: cannot unmarshal {kind} into Go value of type plugins.Manifest"
        ));
    };
    let fields = [
        "id", "kind", "page", "run", "listens", "needs", "title", "about", "ring", "config",
    ];
    let mut err: Option<String> = None;
    let save = |e: String, err: &mut Option<String>| {
        if err.is_none() {
            *err = Some(e);
        }
    };
    let word = |j: &J| match j {
        J::Null => "null",
        J::Bool(_) => "bool",
        J::Num => "number",
        J::Str(_) => "string",
        J::Arr(_) => "array",
        J::Obj(_) => "object",
    };
    for (k, n) in kv {
        let name = fields
            .iter()
            .find(|f| **f == k.as_str())
            .or_else(|| fields.iter().find(|f| f.eq_ignore_ascii_case(k)));
        let Some(name) = name else {
            save(
                format!("json: unknown field {}", sys::go_quote(k)),
                &mut err,
            );
            continue;
        };
        let type_err = |want: &str, j: &J| {
            format!(
                "json: cannot unmarshal {} into Go struct field Manifest.{name} of type {want}",
                word(j)
            )
        };
        match *name {
            "listens" | "needs" => {
                let list = match &n.v {
                    J::Null => {
                        if *name == "listens" {
                            m.listens.clear();
                        } else {
                            m.needs.clear();
                        }
                        continue;
                    }
                    J::Arr(items) => {
                        let mut out = Vec::new();
                        for it in items {
                            match &it.v {
                                J::Str(s) => out.push(s.clone()),
                                J::Null => out.push(String::new()),
                                j => {
                                    save(type_err("string", j), &mut err);
                                    out.push(String::new());
                                }
                            }
                        }
                        out
                    }
                    j => {
                        save(type_err("[]string", j), &mut err);
                        continue;
                    }
                };
                if *name == "listens" {
                    m.listens = list;
                } else {
                    m.needs = list;
                }
            }
            "ring" => match &n.v {
                J::Bool(b) => m.ring = *b,
                J::Null => {}
                j => save(type_err("bool", j), &mut err),
            },
            "config" => match &n.v {
                J::Obj(_) => {
                    let v: Value = serde_json::from_str(&doc.raw(n)).unwrap_or(Value::Null);
                    if let Some(Value::Object(o)) = crate::cli::gofmt::decode(&v) {
                        // A second config key merges into the first map.
                        for (ck, cv) in o {
                            m.config.insert(ck, cv);
                        }
                    }
                }
                J::Null => m.config.clear(),
                j => save(type_err("map[string]interface {}", j), &mut err),
            },
            _ => {
                let slot = match *name {
                    "id" => &mut m.id,
                    "kind" => &mut m.kind,
                    "page" => &mut m.page,
                    "run" => &mut m.run,
                    "title" => &mut m.title,
                    _ => &mut m.about,
                };
                match &n.v {
                    J::Str(s) => *slot = s.clone(),
                    J::Null => {}
                    j => save(type_err("string", j), &mut err),
                }
            }
        }
    }
    match err {
        Some(e) => Err(e),
        None => Ok(m),
    }
}

/// Reads and checks raw as the manifest of the directory dir_name.
fn parse_manifest(
    raw: &[u8],
    dir_name: &str,
    shipped: bool,
    files: &Files,
) -> Result<Manifest, String> {
    let m = decode_manifest(raw).map_err(|e| format!("manifest.json does not parse: {e}"))?;
    if (m.id.starts_with(RESERVED_PREFIX) || dir_name.starts_with(RESERVED_PREFIX)) && !shipped {
        return Err(format!(
            "the id prefix {RESERVED_PREFIX} is reserved for the plugins coppice ships."
        ));
    }
    if !id_ok(m.id.strip_prefix(RESERVED_PREFIX).unwrap_or(&m.id)) {
        return Err(
            "id must be lower case letters, digits and hyphens, and start with a letter.".into(),
        );
    }
    if m.id != dir_name {
        return Err(format!(
            "id {} does not match its directory {dir_name}.",
            m.id
        ));
    }
    if m.needs
        .iter()
        .any(|v| v == "agent.allow" || v == "agent.deny")
    {
        return Err(REFUSAL.into());
    }
    for v in &m.needs {
        if v == "pane.report_state" || v == "pane.report_child" {
            return Err("a plugin reports no state. Only a pane reports its own.".into());
        }
        if !verb_ok(v) {
            return Err(format!(
                "needs has {}, which is not a verb name.",
                sys::go_quote(v)
            ));
        }
    }
    for k in &m.listens {
        if !LISTENABLE.contains(&k.as_str()) {
            return Err(format!(
                "listens has {}. A policy may listen to state, layout, note or child.",
                sys::go_quote(k)
            ));
        }
    }
    let check = |msg: String| if msg.is_empty() { Ok(()) } else { Err(msg) };
    match m.kind.as_str() {
        KIND_VIEW => {
            if m.page.is_empty() {
                return Err("a view needs page, the html file it serves.".into());
            }
            if !m.run.is_empty() || !m.needs.is_empty() || !m.listens.is_empty() {
                return Err("a view holds no verbs and runs nothing. It reads through the floor page's API.".into());
            }
            check(check_file(files, &m.page))?;
        }
        KIND_POLICY => {
            if m.ring {
                return Err("only a view reads the ring. A policy listens to events.".into());
            }
            if m.run.is_empty() {
                return Err("a policy needs run, the program the server starts.".into());
            }
            if !m.page.is_empty() {
                return Err("a policy has no page. Make a view for that.".into());
            }
            check(check_file(files, &m.run))?;
        }
        _ => return Err("kind must be view or policy.".into()),
    }
    Ok(m)
}

/// One place plugins live.
enum Source {
    Shipped,
    Dir(PathBuf),
}

/// Reads plugin id from one source. Ok(None) when the source has no
/// directory for id.
fn load_one(src: &Source, id: &str) -> Result<Option<Plugin>, Problem> {
    let base_ok = !id.contains('/');
    if !valid_path(id) || id == "." || !base_ok {
        return Err(Problem {
            id: id.into(),
            dir: String::new(),
            message: "id must be lower case letters, digits and hyphens, and start with a letter."
                .into(),
        });
    }
    let (raw, dir, files, shipped) = match src {
        Source::Shipped => {
            let Some(b) = shipped_file(&format!("{id}/manifest.json")) else {
                return Ok(None);
            };
            (b.to_vec(), String::new(), Files::Shipped(id.into()), true)
        }
        Source::Dir(root) => {
            let dir = root.join(id);
            let dir_s = dir.to_string_lossy().into_owned();
            let prob = |message: String| Problem {
                id: id.into(),
                dir: dir_s.clone(),
                message,
            };
            let mpath = dir.join("manifest.json");
            match std::fs::symlink_metadata(&mpath) {
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
                Err(e) => {
                    return Err(prob(format!(
                        "cannot read manifest.json: open {id}/manifest.json: {}",
                        sys::go_errno_text(&e)
                    )))
                }
                Ok(_) => {}
            }
            match inside(root, &mpath) {
                Ok(Some(_)) => {}
                Ok(None) => {
                    return Err(prob(format!(
                        "cannot read manifest.json: openat {id}/manifest.json: path escapes from parent"
                    )))
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
                Err(e) => {
                    return Err(prob(format!(
                        "cannot read manifest.json: open {id}/manifest.json: {}",
                        sys::go_errno_text(&e)
                    )))
                }
            }
            let raw = match std::fs::read(&mpath) {
                Ok(b) => b,
                Err(e) => {
                    return Err(prob(format!(
                        "cannot read manifest.json: read {id}/manifest.json: {}",
                        sys::go_errno_text(&e)
                    )))
                }
            };
            (
                raw,
                dir_s,
                Files::Dir {
                    root: root.clone(),
                    dir: dir.clone(),
                },
                false,
            )
        }
    };
    match parse_manifest(&raw, id, shipped, &files) {
        Ok(m) => Ok(Some(Plugin {
            m,
            dir,
            files,
            shipped,
        })),
        Err(message) => Err(Problem {
            id: id.into(),
            dir,
            message,
        }),
    }
}

/// $XDG_CONFIG_HOME/coppice/plugins, beside coppice.toml.
pub fn user_dir() -> PathBuf {
    crate::config::path()
        .parent()
        .map(|p| p.join("plugins"))
        .unwrap_or_else(|| PathBuf::from("plugins"))
}

/// Loads the enabled ids: the shipped plugins first, then the operator's
/// own directory. A directory of the operator's that takes a shipped id is
/// a problem, and the shipped plugin loads.
pub fn load_enabled(enabled: &[String]) -> (Vec<Plugin>, Vec<Problem>) {
    let mut srcs = vec![Source::Shipped];
    let ud = user_dir();
    if std::fs::metadata(&ud).map(|m| m.is_dir()).unwrap_or(false) {
        srcs.push(Source::Dir(ud));
    }
    let mut out = Vec::new();
    let mut probs = Vec::new();
    let mut seen = HashSet::new();
    for id in enabled {
        if !seen.insert(id.clone()) {
            continue;
        }
        let mut found: Option<Plugin> = None;
        let mut prob: Option<Problem> = None;
        for src in &srcs {
            let r = load_one(src, id);
            let (p, pr) = match r {
                Ok(None) => continue,
                Ok(Some(p)) => (Some(p), None),
                Err(e) => (None, Some(e)),
            };
            if let (Some(f), Source::Dir(root)) = (&found, src) {
                if f.shipped {
                    probs.push(Problem {
                        id: id.clone(),
                        dir: root.join(id).to_string_lossy().into_owned(),
                        message: format!(
                            "it takes the id of the shipped plugin {id}. A directory of yours may not replace it. Give it another id."
                        ),
                    });
                    continue;
                }
            }
            if found.is_none() && prob.is_none() {
                found = p;
                prob = pr;
            }
        }
        match (found, prob) {
            (_, Some(p)) => probs.push(p),
            (Some(f), None) => out.push(f),
            (None, None) => probs.push(Problem {
                id: id.clone(),
                dir: String::new(),
                message: "it is enabled, but no plugin directory has that id.".into(),
            }),
        }
    }
    (out, probs)
}

/// $XDG_DATA_HOME, or ~/.local/share.
fn data_home() -> PathBuf {
    match std::env::var_os("XDG_DATA_HOME") {
        Some(d) if !d.is_empty() => PathBuf::from(d),
        _ => sys::home_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join(".local")
            .join("share"),
    }
}

/// Where the runner writes the shipped policies.
pub fn work_dir() -> PathBuf {
    data_home().join("coppice").join("plugins")
}

/// Replaces dir with files: each directory and file mode 0700.
fn write_tree(files: &[(&str, &[u8])], dir: &Path) -> Result<(), String> {
    match std::fs::remove_dir_all(dir) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(sys::go_path_err("unlinkat", dir, &e)),
    }
    let mk = |d: &Path| {
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(d)
            .map_err(|e| sys::go_path_err("mkdir", d, &e))
    };
    mk(dir)?;
    for (rel, body) in files {
        let target = dir.join(rel);
        if let Some(parent) = target.parent() {
            mk(parent)?;
        }
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o700)
            .open(&target)
            .map_err(|e| sys::go_path_err("open", &target, &e))?;
        f.write_all(body)
            .map_err(|e| sys::go_path_err("write", &target, &e))?;
    }
    Ok(())
}

/// The operator's settings as Go's toml decoder hands them to
/// json.Marshal.
pub fn toml_to_json(v: &toml::Value) -> Value {
    match v {
        toml::Value::String(s) => json!(s),
        toml::Value::Integer(i) => json!(i),
        toml::Value::Float(f) => crate::gojson::float(*f),
        toml::Value::Boolean(b) => json!(b),
        toml::Value::Datetime(d) => json!(d.to_string()),
        toml::Value::Array(a) => Value::Array(a.iter().map(toml_to_json).collect()),
        toml::Value::Table(t) => {
            let mut m = Map::new();
            for (k, x) in t {
                m.insert(k.clone(), toml_to_json(x));
            }
            Value::Object(m)
        }
    }
}

/// One line of Go's slog text handler.
fn slog_line(level: &str, msg: &str, attrs: &[(&str, &str)]) -> String {
    let mut s = format!("time={} level={level} msg={}", now_text(), slog_value(msg));
    for (k, v) in attrs {
        s.push_str(&format!(" {k}={}", slog_value(v)));
    }
    s
}

pub(crate) fn slog_value(v: &str) -> String {
    let needs = v.is_empty()
        || v.chars().any(|r| {
            if (r as u32) < 0x80 {
                let b = r as u8;
                b != b'\\' && (b == b' ' || b == b'=' || b == b'"' || !(0x20..0x7f).contains(&b))
            } else {
                r == '\u{FFFD}' || r.is_whitespace() || !sys::is_print_rune(r)
            }
        });
    if needs {
        sys::go_quote(v)
    } else {
        v.to_string()
    }
}

/// Now as Go's slog writes it: local time with milliseconds and the zone.
fn now_text() -> String {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    let secs = now.as_secs() as libc::time_t;
    // SAFETY: localtime_r fills tm from secs.
    let tm = unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        libc::localtime_r(&secs, &mut tm);
        tm
    };
    let off = tm.tm_gmtoff;
    let zone = if off == 0 {
        "Z".to_string()
    } else {
        let sign = if off < 0 { '-' } else { '+' };
        let a = off.abs();
        format!("{sign}{:02}:{:02}", a / 3600, (a % 3600) / 60)
    };
    format!(
        "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}.{:03}{zone}",
        tm.tm_year + 1900,
        tm.tm_mon + 1,
        tm.tm_mday,
        tm.tm_hour,
        tm.tm_min,
        tm.tm_sec,
        now.subsec_millis()
    )
}

fn log(level: &str, msg: &str, attrs: &[(&str, &str)]) {
    let line = slog_line(level, msg, attrs);
    let mut e = std::io::stderr().lock();
    let _ = e.write_all(format!("{line}\n").as_bytes());
}

/// Go's text for a process that ended badly.
fn exit_text(st: std::process::ExitStatus) -> Option<String> {
    if st.success() {
        return None;
    }
    if let Some(c) = st.code() {
        return Some(format!("exit status {c}"));
    }
    let sig = st.signal().unwrap_or(0);
    Some(format!("signal: {}", signal_name(sig)))
}

/// Go's syscall.Signal String for the common signals.
pub fn signal_name(sig: i32) -> String {
    match sig {
        libc::SIGHUP => "hangup".into(),
        libc::SIGINT => "interrupt".into(),
        libc::SIGQUIT => "quit".into(),
        libc::SIGILL => "illegal instruction".into(),
        libc::SIGABRT => "aborted".into(),
        libc::SIGKILL => "killed".into(),
        libc::SIGSEGV => "segmentation fault".into(),
        libc::SIGPIPE => "broken pipe".into(),
        libc::SIGTERM => "terminated".into(),
        libc::SIGUSR1 => "user defined signal 1".into(),
        libc::SIGUSR2 => "user defined signal 2".into(),
        n => format!("signal {n}"),
    }
}

/// How many failed starts in a row before the runner gives up.
const DEFAULT_TRIES: u32 = 5;

/// What the server does at a policy's start and end.
pub trait Host: Send + Sync {
    /// Runs start under the lock that places a new connection, and records
    /// the pid start returns.
    fn launch(
        &self,
        id: &str,
        start: &mut dyn FnMut() -> Result<i32, String>,
    ) -> Result<i32, String>;
    fn exited(&self, pid: i32);
}

#[derive(Default)]
struct RunState {
    stopped: bool,
    procs: HashSet<i32>,
    running: usize,
}

/// Starts each enabled policy as a process. One that exits 0 is finished.
/// One that fails is started again after a wait, up to five times in a row.
pub struct Runner {
    socket: String,
    work_dir: PathBuf,
    config: BTreeMap<String, toml::Table>,
    host: Arc<dyn Host>,
    st: Mutex<RunState>,
    cv: Condvar,
}

impl Runner {
    pub fn new(
        socket: String,
        config: BTreeMap<String, toml::Table>,
        host: Arc<dyn Host>,
    ) -> Arc<Runner> {
        Arc::new(Runner {
            socket,
            work_dir: work_dir(),
            config,
            host,
            st: Mutex::new(RunState::default()),
            cv: Condvar::new(),
        })
    }

    /// Starts every policy in ps. Views are skipped.
    pub fn start(self: &Arc<Runner>, ps: &[Plugin]) {
        if ps.iter().any(|p| p.m.kind == KIND_POLICY && p.shipped) {
            if let Err(e) = self.write_lib() {
                log(
                    "ERROR",
                    "plugins: cannot write the shared library",
                    &[("err", &e)],
                );
            }
        }
        for p in ps {
            if p.m.kind != KIND_POLICY {
                continue;
            }
            self.st.lock().unwrap_or_else(|e| e.into_inner()).running += 1;
            let r = self.clone();
            let p = p.clone();
            thread::spawn(move || {
                r.keep(&p);
                let mut st = r.st.lock().unwrap_or_else(|e| e.into_inner());
                st.running -= 1;
                r.cv.notify_all();
            });
        }
    }

    fn write_lib(&self) -> Result<(), String> {
        let wd = &self.work_dir;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(wd)
            .map_err(|e| sys::go_path_err("mkdir", wd, &e))?;
        std::fs::set_permissions(wd, std::fs::Permissions::from_mode(0o700))
            .map_err(|e| sys::go_path_err("chmod", wd, &e))?;
        write_tree(&shipped_under("_lib"), &wd.join("_lib"))
    }

    fn stopped(&self) -> bool {
        self.st.lock().unwrap_or_else(|e| e.into_inner()).stopped
    }

    fn keep(&self, p: &Plugin) {
        let mut failed = 0u32;
        loop {
            if self.stopped() {
                return;
            }
            let began = Instant::now();
            let err = self.run_once(p);
            if self.stopped() {
                return;
            }
            let Some(err) = err else {
                log("INFO", "plugins: plugin finished", &[("plugin", &p.m.id)]);
                return;
            };
            log(
                "WARN",
                "plugins: plugin exited",
                &[("plugin", &p.m.id), ("err", &err)],
            );
            if began.elapsed() >= Duration::from_secs(60) {
                failed = 0;
            }
            failed += 1;
            if failed >= DEFAULT_TRIES {
                log(
                    "ERROR",
                    &format!(
                        "plugins: gave up on plugin {} after {failed} tries. Fix it, then restart the server.",
                        p.m.id
                    ),
                    &[("plugin", &p.m.id)],
                );
                return;
            }
            let wait = if (1..=6).contains(&failed) {
                Duration::from_secs(1 << (failed - 1)).min(Duration::from_secs(30))
            } else {
                Duration::from_secs(30)
            };
            let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
            let (st, _) = self
                .cv
                .wait_timeout_while(st, wait, |s| !s.stopped)
                .unwrap_or_else(|e| e.into_inner());
            if st.stopped {
                return;
            }
        }
    }

    fn command_for(&self, p: &Plugin, dir: &Path) -> Vec<String> {
        let path = dir.join(&p.m.run).to_string_lossy().into_owned();
        if p.m.run.ends_with(".py") {
            let py = if crate::pane::look_path("python3").is_some() {
                "python3"
            } else if crate::pane::look_path("python").is_some() {
                "python"
            } else {
                "python3"
            };
            return vec![py.into(), path];
        }
        vec![path]
    }

    fn env(&self, p: &Plugin) -> Vec<(String, String)> {
        let drop = [
            "COPPICE_PANE",
            "COPPICE_SOCK",
            "COPPICE_SOCKET",
            "COPPICE_PLUGIN",
            "COPPICE_PLUGIN_CONFIG",
        ];
        let mut out: Vec<(String, String)> = std::env::vars_os()
            .filter(|(k, _)| !drop.iter().any(|d| k == *d))
            .map(|(k, v)| {
                (
                    k.to_string_lossy().into_owned(),
                    v.to_string_lossy().into_owned(),
                )
            })
            .collect();
        let mut cfg = p.m.config.clone();
        if let Some(t) = self.config.get(&p.m.id) {
            for (k, v) in t {
                cfg.insert(k.clone(), toml_to_json(v));
            }
        }
        let raw = crate::cli::gofmt::marshal(&Value::Object(cfg));
        out.push(("COPPICE_SOCKET".into(), self.socket.clone()));
        out.push(("COPPICE_SOCK".into(), self.socket.clone()));
        out.push(("COPPICE_PLUGIN".into(), p.m.id.clone()));
        out.push(("COPPICE_PLUGIN_CONFIG".into(), raw));
        out
    }

    fn prepare(&self, p: &Plugin) -> Result<PathBuf, String> {
        if !p.shipped {
            return Ok(PathBuf::from(&p.dir));
        }
        let dir = self.work_dir.join(&p.m.id);
        write_tree(&shipped_under(&p.m.id), &dir)?;
        Ok(dir)
    }

    /// Prepares p, starts it, and waits for it to exit. None when it
    /// finished, else Go's words for why not. The process is started and
    /// waited for on this thread, so its parent-death signal follows the
    /// thread that lives as long as the wait.
    fn run_once(&self, p: &Plugin) -> Option<String> {
        let dir = match self.prepare(p) {
            Ok(d) => d,
            Err(e) => return Some(e),
        };
        let argv = self.command_for(p, &dir);
        let program = if argv[0].contains('/') {
            argv[0].clone()
        } else {
            match crate::pane::look_path(&argv[0]) {
                Some(x) => x,
                None => {
                    return Some(format!(
                        "exec: {}: executable file not found in $PATH",
                        sys::go_quote(&argv[0])
                    ))
                }
            }
        };
        let mut cmd = Command::new(&program);
        cmd.args(&argv[1..])
            .current_dir(&dir)
            .env_clear()
            .envs(self.env(p))
            .stdin(Stdio::null())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit());
        // SAFETY: setsid and prctl are async-signal-safe.
        unsafe {
            cmd.pre_exec(|| {
                libc::setsid();
                libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL);
                Ok(())
            });
        }
        let mut child = None;
        let res = self.host.launch(&p.m.id, &mut || {
            let mut st = self.st.lock().unwrap_or_else(|e| e.into_inner());
            if st.stopped {
                return Err("the runner stopped".into());
            }
            let c = cmd.spawn().map_err(|e| {
                let path = std::fs::canonicalize(&program)
                    .map(|_| program.clone())
                    .unwrap_or_else(|_| program.clone());
                format!("fork/exec {path}: {}", sys::go_errno_text(&e))
            })?;
            let pid = c.id() as i32;
            st.procs.insert(pid);
            child = Some(c);
            Ok(pid)
        });
        let pid = match res {
            Ok(pid) => pid,
            Err(e) => {
                if let Some(mut c) = child {
                    // SAFETY: signals the group the child leads.
                    unsafe {
                        libc::kill(-(c.id() as i32), libc::SIGKILL);
                    }
                    let _ = c.wait();
                    self.forget(c.id() as i32);
                }
                return Some(e);
            }
        };
        let mut c = child.expect("a launched child");
        let st = c.wait();
        kill_session(pid);
        self.forget(pid);
        self.host.exited(pid);
        match st {
            Ok(s) => exit_text(s),
            Err(e) => Some(e.to_string()),
        }
    }

    fn forget(&self, pid: i32) {
        self.st
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .procs
            .remove(&pid);
    }

    /// Ends every policy and waits for the runner: a terminate signal to
    /// each process group, then a kill after two seconds.
    pub fn stop(&self) {
        let pids: Vec<i32> = {
            let mut st = self.st.lock().unwrap_or_else(|e| e.into_inner());
            if st.stopped {
                return;
            }
            st.stopped = true;
            self.cv.notify_all();
            st.procs.iter().copied().collect()
        };
        for pid in &pids {
            // SAFETY: signals the group the policy leads.
            unsafe {
                libc::kill(-pid, libc::SIGTERM);
            }
        }
        let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        let (st, timeout) = self
            .cv
            .wait_timeout_while(st, Duration::from_secs(2), |s| s.running > 0)
            .unwrap_or_else(|e| e.into_inner());
        if !timeout.timed_out() {
            return;
        }
        for pid in &st.procs {
            // SAFETY: as above.
            unsafe {
                libc::kill(-pid, libc::SIGKILL);
            }
        }
        drop(st);
        for pid in &pids {
            kill_session(*pid);
        }
        let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        drop(self.cv.wait_while(st, |s| s.running > 0));
    }
}

/// Kills every process in the session sid names, found by a scan of
/// /proc. It never signals sid alone: once reaped, that pid may be another
/// process.
pub fn kill_session(sid: i32) {
    let Ok(rd) = std::fs::read_dir("/proc") else {
        return;
    };
    for e in rd.flatten() {
        let Ok(pid) = e.file_name().to_string_lossy().parse::<i32>() else {
            continue;
        };
        if let Ok((_, s)) = sys::proc_stat(pid) {
            if s == sid {
                // SAFETY: signals one process.
                unsafe {
                    libc::kill(pid, libc::SIGKILL);
                }
            }
        }
    }
}

/// The verbs and events each policy in ps holds.
pub fn policy_specs(ps: &[Plugin]) -> HashMap<String, (Vec<String>, Vec<String>)> {
    ps.iter()
        .filter(|p| p.m.kind == KIND_POLICY)
        .map(|p| (p.m.id.clone(), (p.m.needs.clone(), p.m.listens.clone())))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_embedded_tree_leaves_out_underscore_and_dot_names() {
        assert!(shipped_file("tree/manifest.json").is_some());
        assert!(shipped_file("_lib/floor_client.py").is_some());
        assert!(shipped::FILES.iter().all(|(p, _)| {
            p.split('/')
                .skip(1)
                .all(|e| !e.starts_with('_') && !e.starts_with('.'))
        }));
        assert!(!shipped::FILES.iter().any(|(p, _)| p.starts_with("nvim/")));
        // Every shipped manifest loads.
        let ids: Vec<String> = shipped::FILES
            .iter()
            .filter_map(|(p, _)| p.strip_suffix("/manifest.json").map(String::from))
            .collect();
        assert_eq!(ids.len(), 13);
        for id in ids {
            let r = load_one(&Source::Shipped, &id);
            assert!(matches!(r, Ok(Some(_))), "{id}");
        }
    }

    #[test]
    fn manifests_fail_in_go_words() {
        assert_eq!(decode_manifest(b"  \n").unwrap_err(), "EOF");
        assert_eq!(decode_manifest(b"{\"id\": ").unwrap_err(), "unexpected EOF");
        assert_eq!(
            decode_manifest(b"{\"id\": 3}").unwrap_err(),
            "json: cannot unmarshal number into Go struct field Manifest.id of type string"
        );
        assert_eq!(
            decode_manifest(b"{\"bogus\": 1}").unwrap_err(),
            "json: unknown field \"bogus\""
        );
        let m = decode_manifest(b"{\"ID\": \"x\", \"ring\": true} junk").unwrap();
        assert_eq!(m.id, "x");
        assert!(m.ring);
    }

    #[test]
    fn slog_quotes_as_go_does() {
        assert_eq!(slog_value("probe"), "probe");
        assert_eq!(slog_value("exit status 1"), "\"exit status 1\"");
        assert_eq!(slog_value(""), "\"\"");
        assert_eq!(slog_value("a=b"), "\"a=b\"");
        assert!(verb_ok("pane.send_keys") && !verb_ok("nope") && !verb_ok("a..b"));
    }
}
