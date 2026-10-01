//! Pools and the queue (FM-R-93 to FM-R-101). A worker profile may name a
//! pool: several (machine, base URL) entries that serve ONE model. A spawn
//! takes the first entry with a free slot, else the job waits in a queue the
//! owner can read. This file holds the pure parts: the pools file, the pick
//! of an entry, the cool-down of an entry, and the queue file the server
//! writes and `coppice foreman queue` reads. The twin of Go's
//! internal/foreman/pool.go; the server half is server/foreman_pool.rs.

use std::collections::BTreeMap;
use std::io::Write;
use std::path::Path;

use serde::{Deserialize, Serialize};

use super::failover::{Verdict, COOL_MAX, COOL_START};

/// The file beside coppice.toml that holds the pools. It is a file of its
/// own so that a save of coppice.toml, which writes only the keys it knows,
/// can never drop a pool.
pub const POOLS_FILE: &str = "pools.toml";

/// How long an entry must have had no foreground job before a background
/// job may start on it, in seconds.
pub const DEFAULT_IDLE_AFTER: i64 = 120;

/// One machine of a pool.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct PoolEntry {
    pub name: String,
    pub base_url: String,
    pub slots: i64,
}

/// The entries that serve one model. Its name is the name of the worker
/// profile that a spawn gives.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Pool {
    pub name: String,
    pub model: String,
    pub entries: Vec<PoolEntry>,
}

/// The whole pools file.
#[derive(Clone, Debug, Default)]
pub struct PoolSet {
    pub idle_after: i64,
    pub pools: BTreeMap<String, Pool>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct PoolsDoc {
    idle_after: Option<String>,
    pool: Option<BTreeMap<String, PoolDoc>>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct PoolDoc {
    model: Option<String>,
    entries: Option<Vec<EntryDoc>>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct EntryDoc {
    name: Option<String>,
    base_url: Option<String>,
    slots: Option<i64>,
}

fn name_ok(s: &str) -> bool {
    !s.is_empty()
        && s
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'_' || b == b'-')
}

/// The host of an http(s) URL, or None when it is not one.
fn url_host(u: &str) -> Option<String> {
    let rest = u
        .strip_prefix("http://")
        .or_else(|| u.strip_prefix("https://"))?;
    let auth = rest.split(['/', '?', '#']).next().unwrap_or("");
    let auth = auth.rsplit('@').next().unwrap_or("");
    if auth.is_empty() {
        return None;
    }
    let host = if let Some(r) = auth.strip_prefix('[') {
        r.split(']').next().unwrap_or("")
    } else {
        auth.split(':').next().unwrap_or("")
    };
    Some(host.to_string())
}

impl PoolSet {
    /// An empty set with the default idle time.
    pub fn empty() -> PoolSet {
        PoolSet {
            idle_after: DEFAULT_IDLE_AFTER,
            pools: BTreeMap::new(),
        }
    }

    /// Reads the text of a pools file. path is only for the words of an
    /// error.
    pub fn parse(text: &str, path: &str) -> Result<PoolSet, String> {
        let d: PoolsDoc =
            toml::from_str(text).map_err(|e| format!("{path} does not read: {e}"))?;
        let mut set = PoolSet::empty();
        if let Some(s) = d.idle_after.filter(|s| !s.is_empty()) {
            match super::failover::parse_go_duration(&s).filter(|v| *v >= 0.0) {
                Some(v) => set.idle_after = v as i64,
                None => {
                    return Err(format!(
                        "idle_after is {} in {path}. Use a duration such as \"2m\"",
                        crate::sys::go_quote(&s)
                    ))
                }
            }
        }
        for (name, p) in d.pool.unwrap_or_default() {
            if name == "default" || !name_ok(&name) {
                return Err(format!(
                    "the pool {} in {path} is not allowed. A pool is named for a worker profile, and the Claude Code profile (default) has no base URL",
                    crate::sys::go_quote(&name)
                ));
            }
            let model = p.model.unwrap_or_default();
            if model.is_empty() {
                return Err(format!(
                    "the pool {} in {path} needs a model: all its entries serve that one model",
                    crate::sys::go_quote(&name)
                ));
            }
            let docs = p.entries.unwrap_or_default();
            if docs.is_empty() {
                return Err(format!(
                    "the pool {} in {path} has no entries",
                    crate::sys::go_quote(&name)
                ));
            }
            let mut entries = Vec::new();
            let mut seen = std::collections::HashSet::new();
            for e in docs {
                let url = e.base_url.unwrap_or_default();
                let Some(host) = url_host(&url).filter(|h| !h.is_empty()) else {
                    return Err(format!(
                        "an entry of the pool {} in {path} needs a base_url that starts with http:// or https://",
                        crate::sys::go_quote(&name)
                    ));
                };
                let ename = match e.name.filter(|n| !n.is_empty()) {
                    Some(n) => n,
                    None => host,
                };
                if !name_ok(&ename) || !seen.insert(ename.clone()) {
                    return Err(format!(
                        "the entry name {} of the pool {} in {path} is not allowed or is used twice",
                        crate::sys::go_quote(&ename),
                        crate::sys::go_quote(&name)
                    ));
                }
                let slots = e.slots.unwrap_or(0);
                if slots < 0 {
                    return Err(format!(
                        "the entry {} of the pool {} in {path} has slots below 0",
                        crate::sys::go_quote(&ename),
                        crate::sys::go_quote(&name)
                    ));
                }
                entries.push(PoolEntry {
                    name: ename,
                    base_url: url,
                    slots: if slots == 0 { 1 } else { slots },
                });
            }
            set.pools.insert(
                name.clone(),
                Pool {
                    name,
                    model,
                    entries,
                },
            );
        }
        Ok(set)
    }

    /// Reads the pools file. A file that is not there is no pools.
    pub fn load(path: &Path) -> Result<PoolSet, String> {
        match std::fs::read(path) {
            Ok(b) => PoolSet::parse(&String::from_utf8_lossy(&b), &path.display().to_string()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(PoolSet::empty()),
            Err(e) => Err(format!(
                "{} cannot be read: {}",
                path.display(),
                crate::sys::go_errno_text(&e)
            )),
        }
    }
}

/// What the server knows of one entry when it picks.
#[derive(Clone, Debug, Default)]
pub struct Load {
    pub running: i64,
    /// The foreground workers that run on the entry.
    pub fg_running: i64,
    /// The end of the entry's cool-down in Unix seconds; 0 when it is ready.
    pub until: i64,
    /// When a foreground worker last started or ended there; 0 for never.
    pub last_fg: i64,
}

impl Load {
    /// Whether the entry has had no foreground work for idle seconds.
    pub fn idle(&self, now: i64, idle: i64) -> bool {
        self.fg_running == 0 && (self.last_fg == 0 || now - self.last_fg >= idle)
    }
}

/// The first entry of p, in file order, that has a free slot and is not
/// cooling down. A background job also needs the entry to be idle. loads is
/// keyed by entry name; an entry with no load is empty.
pub fn pick_entry(
    p: &Pool,
    loads: &BTreeMap<String, Load>,
    now: i64,
    background: bool,
    idle: i64,
) -> Option<String> {
    for e in &p.entries {
        let l = loads.get(&e.name).cloned().unwrap_or_default();
        if l.until > now || l.running >= e.slots {
            continue;
        }
        if background && !l.idle(now, idle) {
            continue;
        }
        return Some(e.name.clone());
    }
    None
}

/// Entry cool-down for an entry that does not answer: it starts at
/// CONN_COOL_START and doubles on each repeat to CONN_COOL_MAX. A success
/// starts it over. A quota uses the reset the reply names, else the
/// failover bounds.
pub const CONN_COOL_START: i64 = 30;
pub const CONN_COOL_MAX: i64 = 600;

/// The cool-down state of one entry.
#[derive(Clone, Debug, Default)]
pub struct Health {
    pub until: i64,
    pub repeats: u32,
    pub reason: String,
}

impl Health {
    /// Cools the entry after a verdict of Out.
    pub fn fail(&mut self, now: i64, v: &Verdict) {
        if let Some(r) = v.reset {
            self.until = r;
            self.reason = v.reason.clone();
            self.repeats += 1;
            return;
        }
        let r = self.repeats.min(6);
        let d = if v.reason == "cannot connect" {
            (CONN_COOL_START << r).min(CONN_COOL_MAX)
        } else {
            (COOL_START << r).min(COOL_MAX)
        };
        self.until = now + d;
        self.reason = v.reason.clone();
        self.repeats += 1;
    }

    /// Starts the entry's back-off over.
    pub fn ok(&mut self) {
        *self = Health::default();
    }
}

/// The file in the work dir that holds the queue and the entries. The
/// server writes it; `coppice foreman queue` reads it.
pub const QUEUE_FILE: &str = "queue.json";

/// The dir in the work dir where `coppice foreman queue add` leaves one
/// file for each job. The server takes them.
pub const QUEUE_REQUEST_DIR: &str = "queue-in";

fn is_false(b: &bool) -> bool {
    !*b
}

/// One job of the queue, as the queue file shows it.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct QueueJob {
    pub label: String,
    pub project: String,
    pub pool: String,
    #[serde(skip_serializing_if = "is_false")]
    pub background: bool,
    /// waiting, paused or running.
    pub state: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub entry: String,
    pub added: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub note: String,
}

/// One entry of a pool, as the queue file shows it.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct QueueEntry {
    pub pool: String,
    pub name: String,
    pub base_url: String,
    pub slots: i64,
    pub running: i64,
    /// free, busy or cooling.
    pub state: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub until: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

/// A job that is not running: what the server needs to start it.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct HeldJob {
    pub label: String,
    pub project: String,
    pub pool: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    pub brief: String,
    #[serde(skip_serializing_if = "is_false")]
    pub background: bool,
    pub added: String,
    #[serde(skip_serializing_if = "is_false")]
    pub cut: bool,
}

/// The content of the queue file.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct QueueState {
    pub jobs: Vec<QueueJob>,
    pub entries: Vec<QueueEntry>,
    /// The background jobs that wait for an idle machine; the server reads
    /// them back at a start.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub held: Vec<HeldJob>,
}

/// Writes the queue file (0600) in dir, through a temp file.
pub fn write_queue(dir: &Path, q: &QueueState) -> Result<(), String> {
    super::fsutil::ensure_dir_plain(dir)?;
    let b = serde_json::to_vec(q).map_err(|e| e.to_string())?;
    let tmp = dir.join(format!("{QUEUE_FILE}.tmp"));
    crate::server::foreman_chat::write_0600(&tmp, &b)?;
    std::fs::rename(&tmp, dir.join(QUEUE_FILE)).map_err(|e| e.to_string())
}

/// Reads the queue file in dir. No file is an empty queue.
pub fn read_queue(dir: &Path) -> Result<QueueState, String> {
    match std::fs::read(dir.join(QUEUE_FILE)) {
        Ok(b) => serde_json::from_slice(&b).map_err(|e| {
            format!("{} does not read: {e}", dir.join(QUEUE_FILE).display())
        }),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(QueueState::default()),
        Err(e) => Err(crate::sys::go_errno_text(&e)),
    }
}

/// One background job that the owner adds by hand.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct QueueRequest {
    pub project: String,
    pub brief: String,
    pub profile: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
}

/// Leaves a request for the server in dir/queue-in. The file name is fresh,
/// so two adds never meet.
pub fn write_queue_request(dir: &Path, r: &QueueRequest, now_nanos: u128) -> Result<(), String> {
    let d = dir.join(QUEUE_REQUEST_DIR);
    super::fsutil::ensure_dir_plain(&d)?;
    let b = serde_json::to_vec(r).map_err(|e| e.to_string())?;
    let name = format!("{now_nanos}-{}.json", std::process::id());
    let tmp = d.join(format!(".{name}"));
    crate::server::foreman_chat::write_0600(&tmp, &b)?;
    std::fs::rename(&tmp, d.join(&name)).map_err(|e| e.to_string())
}

/// Renames path to <path>.broken-<UTC time> (the first free name) and returns
/// the new path. The rule for broken state is to keep it and start fresh
/// (FM-R-112); the newest few copies stay.
pub fn move_aside(path: &Path, now: std::time::SystemTime) -> Result<std::path::PathBuf, String> {
    let base = format!("{}.broken-{}", path.display(), super::chat::stamp_text(now));
    let to = std::path::PathBuf::from(super::chat::free_name(&base));
    std::fs::rename(path, &to).map_err(|e| crate::sys::go_errno_text(&e))?;
    crate::broken::prune(&to);
    Ok(to)
}

/// Reads the pools file. When it does not read it is moved to
/// <path>.broken-<time>, never deleted, and the result is no pools; the note
/// is the one line that says where the file went (FM-R-112). A file that is
/// not there is no pools and no note.
pub fn load_pools_or_aside(path: &Path, now: std::time::SystemTime) -> (PoolSet, String) {
    if let Ok(set) = PoolSet::load(path) {
        return (set, String::new());
    }
    let name = POOLS_FILE;
    match move_aside(path, now) {
        Ok(to) => (
            PoolSet::empty(),
            format!(
                "{name} did not read. It was moved to {} and the foreman goes on with no pools.",
                to.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()
            ),
        ),
        Err(_) => (
            PoolSet::empty(),
            format!("{name} did not read and could not be moved aside, so no pools are used."),
        ),
    }
}

/// Reads the queue file in dir. When it does not read it is moved aside the
/// same way and the queue starts empty; the note says where it went.
pub fn read_queue_or_aside(dir: &Path, now: std::time::SystemTime) -> (QueueState, String) {
    match read_queue(dir) {
        Ok(q) => (q, String::new()),
        Err(_) => match move_aside(&dir.join(QUEUE_FILE), now) {
            Ok(to) => (
                QueueState::default(),
                format!(
                    "{QUEUE_FILE} did not read. It was moved to {} and the queue starts empty.",
                    to.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()
                ),
            ),
            Err(_) => (
                QueueState::default(),
                format!("{QUEUE_FILE} did not read and could not be moved aside, so the queue starts empty."),
            ),
        },
    }
}

/// Reads and removes the requests in dir/queue-in, oldest first. A file
/// that does not read is moved aside to <name>.broken-<time> (never deleted)
/// and counted in the second value.
pub fn take_queue_requests(dir: &Path) -> (Vec<QueueRequest>, usize) {
    let d = dir.join(QUEUE_REQUEST_DIR);
    let Ok(rd) = std::fs::read_dir(&d) else {
        return (Vec::new(), 0);
    };
    let mut names: Vec<String> = rd
        .filter_map(|e| e.ok())
        .filter(|e| e.file_type().is_ok_and(|t| !t.is_dir()))
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|n| n.ends_with(".json") && !n.starts_with('.'))
        .collect();
    names.sort();
    let (mut reqs, mut bad) = (Vec::new(), 0);
    for n in names {
        let p = d.join(&n);
        let b = std::fs::read(&p);
        match b.ok().and_then(|b| serde_json::from_slice::<QueueRequest>(&b).ok()) {
            Some(r)
                if !r.project.is_empty()
                    && !r.brief.trim().is_empty()
                    && !r.profile.is_empty() =>
            {
                let _ = std::fs::remove_file(&p);
                reqs.push(r)
            }
            _ => {
                // A file that cannot be moved stays, and is not counted
                // again and again.
                if move_aside(&p, std::time::SystemTime::now()).is_ok() {
                    bad += 1;
                }
            }
        }
    }
    (reqs, bad)
}

/// Writes the queue the way `coppice foreman queue` shows it: one line for
/// each job, then one for each entry.
pub fn format_queue(w: &mut dyn Write, q: &QueueState) {
    if q.jobs.is_empty() {
        let _ = writeln!(w, "queue: empty");
    } else {
        let _ = writeln!(w, "queue:");
        for j in &q.jobs {
            let kind = if j.background { "background" } else { "foreground" };
            let on = if j.entry.is_empty() { "-" } else { &j.entry };
            let mut line = format!(
                "  {} | {} | {} | {kind} | {} | {on}",
                j.label, j.project, j.pool, j.state
            );
            if !j.note.is_empty() {
                line.push_str(&format!(" | {}", j.note));
            }
            let _ = writeln!(w, "{line}");
        }
    }
    if !q.entries.is_empty() {
        let _ = writeln!(w, "entries:");
        for e in &q.entries {
            let mut line = format!(
                "  {}/{} | {} of {} slots | {}",
                e.pool, e.name, e.running, e.slots, e.state
            );
            if !e.until.is_empty() {
                line.push_str(&format!(" until {}", e.until));
            }
            if !e.reason.is_empty() {
                line.push_str(&format!(" ({})", e.reason));
            }
            let _ = writeln!(w, "{line}");
        }
    }
}

/// coppice foreman queue, and queue add. dir is the foreman dir; the work
/// dir in it holds the queue file and the requests. It reads and writes
/// local files only and takes no lock.
pub fn run_queue(
    dir: &Path,
    args: &[String],
    req: &QueueRequest,
    as_json: bool,
    out: &mut dyn Write,
    errw: &mut dyn Write,
) -> i32 {
    let work = dir.join("work");
    if args.is_empty() {
        let q = match read_queue(&work) {
            Ok(q) => q,
            Err(e) => {
                let _ = writeln!(errw, "{e}");
                return 1;
            }
        };
        if as_json {
            let _ = writeln!(out, "{}", serde_json::to_string(&q).unwrap_or_default());
            return 0;
        }
        format_queue(out, &q);
        return 0;
    }
    if args.len() == 1 && args[0] == "add" {
        if req.project.is_empty() || req.brief.trim().is_empty() || req.profile.is_empty() {
            let _ = writeln!(
                errw,
                "queue add needs --project, --brief and --profile (the pool's name)"
            );
            return 1;
        }
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        if let Err(e) = write_queue_request(&work, req, nanos) {
            let _ = writeln!(
                errw,
                "cannot leave the request in {}: {e}",
                work.display()
            );
            return 1;
        }
        let _ = writeln!(
            out,
            "added: the server takes it as a background job within a few seconds"
        );
        return 0;
    }
    let _ = writeln!(errw, "queue takes no argument, or add");
    1
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::foreman::failover::Outcome;

    const POOL_TEXT: &str = r#"
idle_after = "90s"
[pool.desk]
model = "m1"
[[pool.desk.entries]]
base_url = "http://desk.tail1.ts.net:11434/v1"
slots = 2
[[pool.desk.entries]]
name = "lap"
base_url = "http://127.0.0.1:8080/v1"
"#;

    fn tmp(tag: &str) -> std::path::PathBuf {
        let d = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-pool-{}-{tag}", std::process::id())));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn broken_in(d: &Path, base: &str) -> Vec<std::path::PathBuf> {
        let mut v: Vec<_> = std::fs::read_dir(d)
            .unwrap()
            .flatten()
            .map(|e| e.path())
            .filter(|p| p.file_name().unwrap().to_string_lossy().starts_with(&format!("{base}.broken-")))
            .collect();
        v.sort();
        v
    }

    /// FM-R-112: a pools file that does not read is moved aside, once, and
    /// the foreman goes on with no pools.
    #[test]
    fn a_bad_pools_file_is_moved_aside_and_kept() {
        let d = tmp("pools");
        let p = d.join(POOLS_FILE);
        std::fs::write(&p, "this is = not [valid toml\n").unwrap();
        let (set, note) = load_pools_or_aside(&p, std::time::SystemTime::now());
        assert!(set.pools.is_empty());
        assert!(note.starts_with("pools.toml did not read. It was moved to pools.toml.broken-"), "{note}");
        assert!(!p.exists());
        let kept = broken_in(&d, POOLS_FILE);
        assert_eq!(kept.len(), 1);
        assert_eq!(std::fs::read_to_string(&kept[0]).unwrap(), "this is = not [valid toml\n");
        let (_, again) = load_pools_or_aside(&p, std::time::SystemTime::now());
        assert!(again.is_empty(), "{again}");
    }

    #[test]
    fn a_bad_queue_file_and_a_bad_request_are_moved_aside_not_deleted() {
        let d = tmp("queue");
        std::fs::write(d.join(QUEUE_FILE), "{broken").unwrap();
        let (q, note) = read_queue_or_aside(&d, std::time::SystemTime::now());
        assert!(q.jobs.is_empty() && note.contains("queue.json.broken-"), "{note}");
        assert_eq!(broken_in(&d, QUEUE_FILE).len(), 1);
        let inbox = d.join(QUEUE_REQUEST_DIR);
        std::fs::create_dir_all(&inbox).unwrap();
        std::fs::write(inbox.join("1-1.json"), "{not json").unwrap();
        std::fs::write(
            inbox.join("2-1.json"),
            r#"{"project":"p","brief":"b","profile":"desk"}"#,
        )
        .unwrap();
        let (reqs, bad) = take_queue_requests(&d);
        assert_eq!((reqs.len(), bad), (1, 1));
        assert_eq!(broken_in(&inbox, "1-1.json").len(), 1);
        assert!(!inbox.join("2-1.json").exists(), "a good request is taken");
    }

    #[test]
    fn parse_pools_fills_the_defaults() {
        let set = PoolSet::parse(POOL_TEXT, "p.toml").unwrap();
        let p = &set.pools["desk"];
        assert_eq!((set.idle_after, p.model.as_str(), p.entries.len()), (90, "m1", 2));
        assert_eq!(p.entries[0].name, "desk.tail1.ts.net");
        assert_eq!((p.entries[0].slots, p.entries[1].slots), (2, 1));
        let empty = PoolSet::parse("", "p.toml").unwrap();
        assert_eq!((empty.idle_after, empty.pools.len()), (DEFAULT_IDLE_AFTER, 0));
    }

    #[test]
    fn parse_pools_refuses_what_could_hide_a_model_switch() {
        for (name, text) in [
            ("no model", "[pool.a]\n[[pool.a.entries]]\nbase_url = \"http://h:1/v1\"\n"),
            ("no entries", "[pool.a]\nmodel = \"m\"\n"),
            ("the claude", "[pool.default]\nmodel = \"m\"\n[[pool.default.entries]]\nbase_url = \"http://h:1/v1\"\n"),
            ("bad url", "[pool.a]\nmodel = \"m\"\n[[pool.a.entries]]\nbase_url = \"ftp://h\"\n"),
            ("same name", "[pool.a]\nmodel = \"m\"\n[[pool.a.entries]]\nname = \"x\"\nbase_url = \"http://h:1/v1\"\n[[pool.a.entries]]\nname = \"x\"\nbase_url = \"http://g:1/v1\"\n"),
            ("bad idle", "idle_after = \"soon\"\n"),
            ("not toml", "[[["),
        ] {
            let e = PoolSet::parse(text, "p.toml").unwrap_err();
            assert!(e.contains("p.toml"), "{name}: {e}");
        }
    }

    fn loads(v: &[(&str, Load)]) -> BTreeMap<String, Load> {
        v.iter().map(|(k, l)| (k.to_string(), l.clone())).collect()
    }

    #[test]
    fn pick_entry_takes_the_first_free_slot_that_is_not_cooling() {
        let set = PoolSet::parse(POOL_TEXT, "p.toml").unwrap();
        let p = &set.pools["desk"];
        let now = 1_000_000;
        let (a, b) = (p.entries[0].name.as_str(), p.entries[1].name.as_str());
        assert_eq!(pick_entry(p, &BTreeMap::new(), now, false, 60).as_deref(), Some(a));
        let full = Load { running: 2, ..Default::default() };
        assert_eq!(pick_entry(p, &loads(&[(a, full)]), now, false, 60).as_deref(), Some(b));
        let cool = Load { until: now + 60, ..Default::default() };
        assert_eq!(pick_entry(p, &loads(&[(a, cool.clone())]), now, false, 60).as_deref(), Some(b));
        let taken = Load { running: 1, ..Default::default() };
        assert_eq!(pick_entry(p, &loads(&[(a, cool), (b, taken)]), now, false, 60), None);
        let back = Load { until: now - 1, ..Default::default() };
        assert_eq!(pick_entry(p, &loads(&[(a, back)]), now, false, 60).as_deref(), Some(a));
    }

    #[test]
    fn pick_entry_holds_background_jobs_for_an_idle_entry() {
        let set = PoolSet::parse(POOL_TEXT, "p.toml").unwrap();
        let p = &set.pools["desk"];
        let now = 1_000_000;
        let (a, b) = (p.entries[0].name.as_str(), p.entries[1].name.as_str());
        let mut l = loads(&[
            (a, Load { last_fg: now - 10, ..Default::default() }),
            (b, Load { fg_running: 1, running: 1, ..Default::default() }),
        ]);
        assert_eq!(pick_entry(p, &l, now, true, 120), None);
        assert_eq!(pick_entry(p, &l, now, false, 120).as_deref(), Some(a));
        l.insert(a.to_string(), Load { last_fg: now - 180, ..Default::default() });
        assert_eq!(pick_entry(p, &l, now, true, 120).as_deref(), Some(a));
    }

    #[test]
    fn health_backs_off_and_starts_over_on_success() {
        let now = 1_000_000;
        let conn = Verdict { outcome: Outcome::Out, reason: "cannot connect".into(), reset: None };
        let mut h = Health::default();
        h.fail(now, &conn);
        let first = h.until - now;
        h.fail(now, &conn);
        let second = h.until - now;
        assert_eq!((first, second), (CONN_COOL_START, 2 * CONN_COOL_START));
        for _ in 0..20 {
            h.fail(now, &conn);
        }
        assert_eq!(h.until - now, CONN_COOL_MAX);
        h.ok();
        h.fail(now, &conn);
        assert_eq!(h.until - now, CONN_COOL_START);
        h.ok();
        let named = Verdict { outcome: Outcome::Out, reason: "rate_limit_error".into(), reset: Some(now + 420) };
        h.fail(now, &named);
        assert_eq!(h.until, now + 420);
        h.ok();
        let plain = Verdict { outcome: Outcome::Out, reason: "rate_limit_error".into(), reset: None };
        h.fail(now, &plain);
        assert_eq!(h.until - now, COOL_START);
    }

    #[test]
    fn the_queue_file_and_the_requests_round_trip() {
        let dir = crate::testhome::guard(std::env::temp_dir().join(format!("pool-q-{}", std::process::id())));
        let _ = std::fs::remove_dir_all(&dir);
        assert_eq!(read_queue(&dir).unwrap().jobs.len(), 0);
        let q = QueueState {
            jobs: vec![
                QueueJob { label: "p-1".into(), project: "p".into(), pool: "desk".into(), state: "waiting".into(), added: "x".into(), ..Default::default() },
                QueueJob { label: "p-2".into(), project: "p".into(), pool: "desk".into(), background: true, state: "running".into(), entry: "a".into(), added: "y".into(), ..Default::default() },
            ],
            entries: vec![
                QueueEntry { pool: "desk".into(), name: "a".into(), slots: 1, running: 1, state: "busy".into(), ..Default::default() },
                QueueEntry { pool: "desk".into(), name: "b".into(), slots: 1, state: "cooling".into(), until: "2026-10-24T12:00:00Z".into(), reason: "cannot connect".into(), ..Default::default() },
            ],
            held: vec![],
        };
        write_queue(&dir, &q).unwrap();
        let got = read_queue(&dir).unwrap();
        assert_eq!((got.jobs.len(), got.entries[1].reason.as_str()), (2, "cannot connect"));
        let mut out = Vec::new();
        format_queue(&mut out, &got);
        let text = String::from_utf8(out).unwrap();
        for s in [
            "p-1 | p | desk | foreground | waiting | -",
            "p-2 | p | desk | background | running | a",
            "desk/a | 1 of 1 slots | busy",
            "desk/b | 0 of 1 slots | cooling until 2026-10-24T12:00:00Z (cannot connect)",
        ] {
            assert!(text.contains(s), "the queue lacks {s:?}:\n{text}");
        }
        for (i, brief) in ["one", "two"].iter().enumerate() {
            let r = QueueRequest { project: "p".into(), brief: brief.to_string(), profile: "desk".into(), model: String::new() };
            write_queue_request(&dir, &r, 1_000 + i as u128).unwrap();
        }
        let (reqs, bad) = take_queue_requests(&dir);
        assert_eq!((reqs.len(), bad, reqs[0].brief.as_str(), reqs[1].brief.as_str()), (2, 0, "one", "two"));
        assert_eq!(take_queue_requests(&dir).0.len(), 0);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
