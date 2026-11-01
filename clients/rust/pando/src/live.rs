//! Round 7: the store is live. The twin of Go's pando live.go, follow.go and
//! followcli.go. A message enters the log when it is said, every agent writes
//! into the one store, and each line keeps its harness, chat and person.

use std::collections::HashMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use super::chat::{flat, Chat, Config, Meta, Summarizer, Clock, ERR_LOCKED, KIND_STEWARD, KIND_USER};
use super::ctx::Ctx;

use super::import::{read_session, secs_time, ImportOpts, SRC_CLAUDE};
use super::tend::{tend_calls, TendRun};

/// The variable that marks a process as not a chat of the owner's: a worker
/// the steward started, or a model call daisugi makes for itself. It gets no
/// view and logs nothing.
pub const SKIP_VAR: &str = "DAISUGI_PANDO_SKIP";

/// Whether the value of SKIP_VAR says to skip. "" and "0" mean no.
pub fn skip_value_on(v: &str) -> bool {
    let v = v.trim();
    !v.is_empty() && v != "0"
}

/// Whether this process is not a chat of the owner's.
pub fn skip_on(getenv: Option<&dyn Fn(&str) -> String>) -> bool {
    getenv.is_some_and(|g| skip_value_on(&g(SKIP_VAR)))
}

/// The dirs whose Claude Code sessions the live feed never reads: the store
/// itself and the coppice data dir, where the steward's pool workers run. A
/// worker's transcript is its own. The steward dir is not here: workers carry
/// the skip marker and list, and a Claude Code pane steward is logged like
/// any other chat.
pub fn skip_dirs(store_dir: &Path, home: &Path, getenv: Option<&dyn Fn(&str) -> String>) -> Vec<String> {
    let get = |k: &str| getenv.map(|g| g(k)).unwrap_or_default();
    let mut state = get("XDG_STATE_HOME");
    if state.is_empty() && !home.as_os_str().is_empty() {
        state = home.join(".local").join("state").to_string_lossy().into_owned();
    }
    let mut out = vec![store_dir.to_string_lossy().into_owned()];
    let d = get("COPPICE_DATA_DIR");
    if !d.is_empty() {
        out.push(d);
    }
    // The coppice data dir sits in the daisugi data home, which is
    // <data>/pando/<store> above the store: the one this process was given,
    // the XDG one and the old ~/.opendaisugi.
    if let Some(data) = store_dir.parent().and_then(|p| p.parent()) {
        if !data.as_os_str().is_empty() {
            out.push(data.join("coppice").to_string_lossy().into_owned());
        }
    }
    let xdg = get("XDG_DATA_HOME");
    if Path::new(&xdg).is_absolute() {
        out.push(Path::new(&xdg).join("opendaisugi").join("coppice").to_string_lossy().into_owned());
    } else if !home.as_os_str().is_empty() {
        out.push(home.join(".local").join("share").join("opendaisugi").join("coppice").to_string_lossy().into_owned());
    }
    if !home.as_os_str().is_empty() {
        out.push(home.join(".opendaisugi").join("coppice").to_string_lossy().into_owned());
    }
    if !state.is_empty() {
        out.push(Path::new(&state).join("coppice").to_string_lossy().into_owned());
    }
    out
}

/// The file beside the stores, <data>/pando/skip.sessions, that names the
/// Claude Code sessions the live feed never logs: the chats of workers and
/// internal model calls. One session id or transcript path per line; the file
/// only grows. A process marked SKIP_VAR writes its own session here from its
/// hooks, and a spawner that starts `claude` for a worker writes the id it
/// chose before the child runs. A cwd list cannot do this: a worker runs in
/// the project, where the owner works too.
pub const SKIP_LIST_NAME: &str = "skip.sessions";

/// The variable that names the skip list for a spawner's child tree.
pub const SKIP_LIST_VAR: &str = "DAISUGI_PANDO_SKIPLIST";

/// The skip list of the stores in store_dir (their parent).
pub fn skip_list_path(store_dir: &Path) -> PathBuf {
    store_dir.parent().unwrap_or(Path::new("")).join(SKIP_LIST_NAME)
}

/// The skip list under a data home.
pub fn skip_list_in(data_dir: &Path) -> PathBuf {
    data_dir.join("pando").join(SKIP_LIST_NAME)
}

/// Adds ids (session ids or transcript paths) to the list in one append
/// write, so two processes cannot split a line. It takes no store lock.
pub fn append_skip(path: &Path, ids: &[&str]) -> std::io::Result<()> {
    let mut b = String::new();
    for id in ids {
        let id = id.trim();
        if id.is_empty() || id.contains(['\r', '\n']) {
            continue;
        }
        b.push_str(id);
        b.push('\n');
    }
    if b.is_empty() {
        return Ok(());
    }
    if let Some(d) = path.parent() {
        std::fs::create_dir_all(d)?;
    }
    let mut f = std::fs::OpenOptions::new().create(true).append(true).mode_0600().open(path)?;
    f.write_all(b.as_bytes())
}

trait Mode0600 {
    fn mode_0600(&mut self) -> &mut Self;
}
impl Mode0600 for std::fs::OpenOptions {
    fn mode_0600(&mut self) -> &mut Self {
        use std::os::unix::fs::OpenOptionsExt;
        self.mode(0o600)
    }
}

/// The ids on the list. A missing or unreadable file is an empty set.
pub fn load_skip(path: &Path) -> std::collections::HashSet<String> {
    let mut set = std::collections::HashSet::new();
    if let Ok(t) = std::fs::read_to_string(path) {
        for l in t.lines() {
            let l = l.trim();
            if !l.is_empty() {
                set.insert(l.to_string());
            }
        }
    }
    set
}

/// Whether the session is listed, by its id, its file name stem or its
/// transcript path.
pub fn skip_has(set: &std::collections::HashSet<String>, session_id: &str, path: &str) -> bool {
    if set.is_empty() {
        return false;
    }
    if !path.is_empty() {
        if set.contains(path) {
            return true;
        }
        let base = Path::new(path).file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
        if set.contains(base.strip_suffix(".jsonl").unwrap_or(&base)) {
            return true;
        }
    }
    !session_id.is_empty() && set.contains(session_id)
}

/// A version 4 uuid, the form `claude --session-id` takes.
pub fn new_uuid() -> std::io::Result<String> {
    use std::io::Read;
    let mut b = [0u8; 16];
    std::fs::File::open("/dev/urandom")?.read_exact(&mut b)?;
    b[6] = b[6] & 0x0f | 0x40;
    b[8] = b[8] & 0x3f | 0x80;
    let h: String = b.iter().map(|x| format!("{x:02x}")).collect();
    Ok(format!("{}-{}-{}-{}-{}", &h[0..8], &h[8..12], &h[12..16], &h[16..20], &h[20..]))
}

/// Mints a session id and puts it on the skip list under data_dir before it
/// returns. A spawner that starts `claude` for a worker or an internal call
/// passes the id with --session-id: the transcript is listed before it
/// exists, and no poll can take its first line.
pub fn new_skipped_session(data_dir: &Path) -> std::io::Result<String> {
    let id = new_uuid()?;
    append_skip(&skip_list_in(data_dir), &[&id])?;
    Ok(id)
}

/// The flags for one `claude -p` call: when `list` names a skip list, a fresh
/// session id is written to it and passed as --session-id. The list holds the
/// id before the child runs, so no follow poll can take the first line of the
/// transcript. Empty when there is no list or it cannot be written (the
/// child's hooks then list the session, a few ms later). Empty too when
/// `marker`, the value of the skip marker in the child's environment, is not
/// on: only a marked process lists its sessions (as Go's SessionArgs).
pub fn session_args(list: Option<&str>, marker: &str) -> Vec<String> {
    let Some(path) = list.filter(|p| !p.is_empty()).filter(|_| skip_value_on(marker)) else { return Vec::new() };
    let Ok(id) = new_uuid() else { return Vec::new() };
    if append_skip(Path::new(path), &[&id]).is_err() {
        return Vec::new();
    }
    vec!["--session-id".into(), id]
}

/// Puts the session of a hook event on the skip list. It never fails the hook.
/// A hook runs on every event of a long session, so an id that is listed
/// already is not written again: the list grows by a session, not by a turn.
pub fn skip_session(list: &Path, ev: &HookEvent) {
    let have = load_skip(list);
    let add: Vec<&str> = [ev.session_id.as_str(), ev.transcript_path.as_str()]
        .into_iter()
        .map(str::trim)
        .filter(|id| !id.is_empty() && !have.contains(*id))
        .collect();
    let _ = append_skip(list, &add);
}

/// The view text for the store in dir, built from view.json, the tree and the
/// last 20 messages only (the full log is not decoded). The same bytes the
/// full read gives; any store that is not plain gets the full read. The twin
/// of Go's ViewContextOf.
pub fn view_context_of(dir: &Path) -> Result<String, String> {
    if let Some(c) = Chat::open_for_context(dir, 20) {
        return Ok(c.view_context());
    }
    Ok(Chat::open_read_only(dir)?.view_context())
}

/// An error text clipped to one short line. A provider may echo the prompt in
/// an error body, and the prompt is store text: the journal must not carry it.
pub fn clip_err(err: &str) -> String {
    super::fsutil::clip(&flat(err), 200).to_string()
}

/// The Src of a Claude Code message.
pub const CLAUDE_SRC: &str = "claude-code";
/// The file in the store dir where a hook writes its errors.
pub const HOOK_LOG_NAME: &str = "hook.log";
/// The state file of follow, in the store dir.
pub const FOLLOW_FILE: &str = "follow.json";
/// Where the background profile is kept, under <data>/pando.
pub const PROFILE_FILE: &str = "profile";

/// One message to log as it is said.
pub struct Live {
    pub kind: String,
    pub text: String,
    pub at: Option<SystemTime>,
    pub act: String,
    pub meta: Meta,
}

impl Chat {
    /// Logs m unless its act is in the log already; true when it was new.
    pub fn append_live_msg(&mut self, m: &Live) -> Result<bool, String> {
        self.append_live(&m.kind, &m.text, m.at, &m.act, &m.meta)
    }

    /// Reads one Claude Code transcript and logs the user and assistant
    /// messages the log lacks. The act id is the importer's, so an import
    /// and the live feed never log a message twice. Returns how many it
    /// added.
    pub fn ingest_claude(&mut self, path: &Path, skip: &[String]) -> Result<usize, String> {
        // The list is read for each file: a worker's hook may add its session
        // at any time, and the file is small.
        let o = ImportOpts {
            from: SRC_CLAUDE.into(),
            skip: skip.to_vec(),
            skip_sessions: load_skip(&skip_list_path(&self.dir())),
            ..Default::default()
        };
        let r = read_session(path, SRC_CLAUDE, &o)?;
        let chat = r.key.strip_prefix(&format!("{SRC_CLAUDE}:")).unwrap_or(&r.key).to_string();
        // A session on the store's session map belongs to a branch: its
        // messages go there and never into this log.
        let mut router = None;
        let mut branch = String::new();
        if !r.msgs.is_empty() {
            let mut rt = super::branch::Router::new(&self.dir(), self.now_clock());
            let sid = chat.split('~').next().unwrap_or("");
            branch = rt.target(sid, &path.to_string_lossy(), &r.cwd);
            router = Some(rt);
        }
        let mut n = 0;
        for m in &r.msgs {
            let kind = if m.text.starts_with("claude assistant") { KIND_STEWARD } else { KIND_USER };
            let live = Live {
                kind: kind.into(),
                text: m.text.clone(),
                at: Some(secs_time(m.ts)),
                act: m.act.clone(),
                meta: Meta { src: CLAUDE_SRC.into(), chat: chat.clone(), who: "owner".into() },
            };
            let fresh = if branch.is_empty() {
                self.append_live_msg(&live)?
            } else {
                router.as_mut().expect("router").chat(&branch)?.append_live_msg(&live)?
            };
            if fresh {
                n += 1;
            }
        }
        Ok(n)
    }

    /// The text a SessionStart hook prints as context: the view with a short
    /// note on the tools. "" when the store holds no message.
    pub fn view_context(&self) -> String {
        let lines = super::branch::branch_lines(self.dir());
        if self.count() == 0 && lines.is_empty() {
            return String::new();
        }
        let mut b = String::new();
        if super::branch::is_branch(self.dir()) {
            let bi = super::branch::read_branch(self.dir()).unwrap_or_default();
            b.push_str(&format!(
                "Pando memory: the view of the branch {}, the chats of this agent's own part of the tree. ",
                bi.path
            ));
        } else {
            b.push_str("Pando memory: the view of every chat the owner and the household had with any agent. ");
        }
        b.push_str("Each line is a node named first+count. Use the pando tools: recall(query) finds where a word was said, ");
        b.push_str("zoom(id, n) opens a node, date(id) gives the time, view() shows this again. ");
        b.push_str("Fine detail is near now; the past is coarse.\n\n");
        b.push_str(&self.render());
        let n = self.not_in_view();
        if n > 0 {
            let from = if n > 20 { self.count() - 20 } else { self.count() - n };
            b.push_str("Not in the view yet:\n");
            for i in from..self.count() {
                if let Ok(m) = self.message(i) {
                    b.push_str(&format!("{}: {}\n", i, super::fsutil::clip(&flat(&m.text), 300)));
                }
            }
        }
        b.push_str(&lines);
        b
    }
}

/// Opens the store for writing, waiting up to wait for another writer.
pub fn open_wait(cfg: impl Fn() -> Config, wait: Duration) -> Result<Chat, String> {
    let deadline = std::time::Instant::now() + wait;
    loop {
        match Chat::open(cfg()) {
            Err(e) if e == ERR_LOCKED && std::time::Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(100));
            }
            r => return r,
        }
    }
}

/// The JSON a Claude Code hook gets on standard input.
#[derive(Default)]
pub struct HookEvent {
    pub event: String,
    pub session_id: String,
    pub transcript_path: String,
}

/// Reads the event; garbage is an empty event, never an error.
pub fn read_hook_event(input: &str) -> HookEvent {
    let v: serde_json::Value = serde_json::from_str(input).unwrap_or(serde_json::Value::Null);
    let s = |k: &str| v.get(k).and_then(|x| x.as_str()).unwrap_or("").to_string();
    HookEvent { event: s("hook_event_name"), session_id: s("session_id"), transcript_path: s("transcript_path") }
}

fn log_err(dir: &Path, event: &str, err: &str) {
    let _ = std::fs::create_dir_all(dir);
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(dir.join(HOOK_LOG_NAME)) {
        let _ = writeln!(f, "{} {}: {}", super::chat::date_text(SystemTime::now()), event, err);
    }
}

/// Handles one Claude Code hook call and returns the text for standard
/// output. It never fails: an error goes to hook.log in the store.
pub fn run_hook(dir: &Path, ev: &HookEvent, skip: &[String], now: Option<Clock>) -> String {
    // A panic is logged, never raised: a non-zero exit shows the user a notice.
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| run_hook_inner(dir, ev, skip, now))) {
        Ok(out) => out,
        Err(p) => {
            let msg = p
                .downcast_ref::<&str>()
                .map(|s| s.to_string())
                .or_else(|| p.downcast_ref::<String>().cloned())
                .unwrap_or_else(|| "unknown".into());
            log_err(dir, &ev.event, &format!("panic: {msg}"));
            String::new()
        }
    }
}

fn run_hook_inner(dir: &Path, ev: &HookEvent, skip: &[String], now: Option<Clock>) -> String {
    match ev.event.as_str() {
        "SessionStart" => {
            let text = match view_context_of(dir) {
                Ok(t) => t,
                Err(e) => {
                    if dir.exists() {
                        log_err(dir, &ev.event, &e);
                    }
                    return String::new();
                }
            };
            if text.is_empty() {
                return String::new();
            }
            // As Go's json.Marshal writes the map: keys sorted, HTML escaped.
            let v = serde_json::json!({"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": text}});
            format!("{}\n", super::mcp::marshal(&v))
        }
        "UserPromptSubmit" | "Stop" | "SessionEnd" | "PreCompact" => {
            if ev.transcript_path.is_empty() {
                return String::new();
            }
            let dirb = dir.to_path_buf();
            let mut c = match open_wait(
                || Config { dir: dirb.clone(), now: now.clone(), ..Default::default() },
                Duration::from_secs(3),
            ) {
                Ok(c) => c,
                Err(e) => {
                    log_err(dir, &ev.event, &e);
                    return String::new();
                }
            };
            if let Err(e) = c.ingest_claude(Path::new(&ev.transcript_path), skip) {
                log_err(dir, &ev.event, &e);
            }
            c.close();
            String::new()
        }
        _ => String::new(),
    }
}

/// A place messages come from. Poll logs the messages said since the time
/// given and returns how many were new.
pub trait Source {
    fn name(&self) -> String;
    fn poll(&mut self, c: &mut Chat, since: Option<SystemTime>) -> Result<usize, String>;
    /// For a source with a cursor for each part (a Hermes profile: the id of
    /// the last message it logged). Follow keeps them in follow.json, so a
    /// part that fails stalls itself only.
    fn cursors(&self) -> HashMap<String, i64> {
        HashMap::new()
    }
    fn set_cursors(&mut self, _cursors: HashMap<String, i64>) {}
}

/// Polls the Claude Code transcripts under root: the .jsonl files of each
/// project dir whose size or time changed since the last poll.
pub struct ClaudeSource {
    pub root: PathBuf,
    pub skip: Vec<String>,
    seen: HashMap<PathBuf, (u128, u64)>,
}

impl ClaudeSource {
    pub fn new(root: PathBuf) -> Self {
        ClaudeSource { root, skip: Vec::new(), seen: HashMap::new() }
    }
}

impl Source for ClaudeSource {
    fn name(&self) -> String {
        "claude-code".into()
    }

    fn poll(&mut self, c: &mut Chat, since: Option<SystemTime>) -> Result<usize, String> {
        let rd = match std::fs::read_dir(&self.root) {
            Ok(rd) => rd,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(0),
            Err(e) => return Err(format!("{}: {e}", self.root.display())),
        };
        let mut total = 0;
        let mut first_err: Option<String> = None;
        for proj in rd.flatten() {
            if !proj.path().is_dir() {
                continue;
            }
            let Ok(files) = std::fs::read_dir(proj.path()) else { continue };
            for f in files.flatten() {
                let p = f.path();
                if p.extension().and_then(|e| e.to_str()) != Some("jsonl") || !p.is_file() {
                    continue;
                }
                let Ok(md) = f.metadata() else { continue };
                let mtime = md.modified().unwrap_or(SystemTime::UNIX_EPOCH);
                if since.is_some_and(|s| mtime < s) {
                    continue;
                }
                let sig = (
                    mtime.duration_since(SystemTime::UNIX_EPOCH).map_or(0, |d| d.as_nanos()),
                    md.len(),
                );
                if self.seen.get(&p) == Some(&sig) {
                    continue;
                }
                match c.ingest_claude(&p, &self.skip) {
                    Ok(n) => {
                        self.seen.insert(p, sig);
                        total += n;
                    }
                    Err(e) => {
                        first_err.get_or_insert(format!("{}: {e}", p.display()));
                    }
                }
            }
        }
        match first_err {
            Some(e) => Err(e),
            None => Ok(total),
        }
    }
}

/// What a follow does.
pub struct FollowOpts<'a> {
    pub dir: PathBuf,
    pub sources: Vec<Box<dyn Source>>,
    pub interval: Duration,
    /// Builds the pending nodes, one model call at a time. None means nodes
    /// wait for a profile.
    pub summarizer: Option<Box<dyn Summarizer>>,
    pub overlap: Duration,
    pub now: Option<Clock>,
    pub log: &'a mut dyn Write,
    pub lock_wait: Duration,
    no_model: bool,
    backoff: Duration,
    retry_at: Option<SystemTime>,
}

impl<'a> FollowOpts<'a> {
    pub fn new(dir: PathBuf, log: &'a mut dyn Write) -> Self {
        FollowOpts {
            dir,
            sources: Vec::new(),
            interval: Duration::from_secs(3),
            summarizer: None,
            overlap: Duration::from_secs(120),
            now: None,
            log,
            lock_wait: Duration::from_secs(20),
            no_model: false,
            backoff: Duration::ZERO,
            retry_at: None,
        }
    }

    fn clock(&self) -> SystemTime {
        match &self.now {
            Some(f) => f(),
            None => SystemTime::now(),
        }
    }
}

/// What follow keeps for one source. since is the start of the last cycle in
/// which the whole source was read without an error: a failing source does
/// not move the time of the others.
#[derive(Default, Clone)]
struct SrcState {
    since: String,
    cursors: HashMap<String, i64>,
}

fn read_follow_state(dir: &Path) -> std::collections::BTreeMap<String, SrcState> {
    let mut out = std::collections::BTreeMap::new();
    let Ok(b) = std::fs::read_to_string(dir.join(FOLLOW_FILE)) else { return out };
    let Ok(v) = serde_json::from_str::<serde_json::Value>(&b) else { return out };
    let Some(srcs) = v.get("sources").and_then(|x| x.as_object()) else { return out };
    for (name, sv) in srcs {
        let mut st = SrcState { since: sv.get("since").and_then(|x| x.as_str()).unwrap_or("").to_string(), ..Default::default() };
        if let Some(cs) = sv.get("cursors").and_then(|x| x.as_object()) {
            for (k, n) in cs {
                if let Some(n) = n.as_i64() {
                    st.cursors.insert(k.clone(), n);
                }
            }
        }
        out.insert(name.clone(), st);
    }
    out
}

fn write_follow_state(dir: &Path, st: &std::collections::BTreeMap<String, SrcState>) -> Result<(), String> {
    let mut srcs = serde_json::Map::new();
    for (name, s) in st {
        let mut o = serde_json::Map::new();
        if !s.since.is_empty() {
            o.insert("since".into(), s.since.clone().into());
        }
        if !s.cursors.is_empty() {
            let mut keys: Vec<&String> = s.cursors.keys().collect();
            keys.sort();
            let mut cs = serde_json::Map::new();
            for k in keys {
                cs.insert(k.clone(), s.cursors[k].into());
            }
            o.insert("cursors".into(), cs.into());
        }
        srcs.insert(name.clone(), o.into());
    }
    let body = format!("{}\n", serde_json::json!({ "sources": srcs }));
    let tmp = dir.join(format!("{FOLLOW_FILE}.tmp"));
    std::fs::write(&tmp, body).map_err(|e| e.to_string())?;
    std::fs::rename(&tmp, dir.join(FOLLOW_FILE)).map_err(|e| e.to_string())
}

/// One cycle: poll every source, then build at most one node. Returns
/// (added, built, more).
pub fn follow_once(ctx: &Ctx, o: &mut FollowOpts) -> Result<(usize, bool, bool), String> {
    let start = o.clock();
    let mut state = read_follow_state(&o.dir);
    let dir = o.dir.clone();
    let now = o.now.clone();
    let mut c = open_wait(|| Config { dir: dir.clone(), now: now.clone(), ..Default::default() }, o.lock_wait)?;
    let mut added = 0;
    for s in o.sources.iter_mut() {
        let mut ss = state.get(&s.name()).cloned().unwrap_or_default();
        let since = super::import::parse_import_time(&ss.since).map(secs_time).unwrap_or(start); // the first run starts now; history is the importer's
        let from = since.checked_sub(o.overlap).unwrap_or(SystemTime::UNIX_EPOCH);
        s.set_cursors(ss.cursors.clone());
        let r = s.poll(&mut c, Some(from));
        ss.cursors = s.cursors();
        match r {
            Ok(n) => {
                added += n;
                ss.since = super::chat::date_text(start);
            }
            Err(e) => {
                if ss.since.is_empty() {
                    ss.since = super::chat::date_text(since); // a failing source keeps its own time
                }
                let _ = writeln!(o.log, "follow: {}: {}", s.name(), clip_err(&e));
            }
        }
        state.insert(s.name(), ss);
    }
    if let Err(e) = write_follow_state(&o.dir, &state) {
        let _ = writeln!(o.log, "follow: {e}");
    }
    // The store's own nodes first, then those of its branches (and their
    // workers), one model call per cycle either way.
    let mut on_sub = false;
    if c.pending() == 0 {
        match super::branch::pending_sub(&o.dir, &o.now) {
            Ok(Some(sub)) => {
                c.close();
                c = sub;
                on_sub = true;
            }
            _ => {
                c.close();
                return Ok((added, false, false));
            }
        }
    }
    let Some(summ) = o.summarizer.as_ref() else {
        if !o.no_model {
            o.no_model = true;
            let _ = writeln!(o.log, "follow: summary nodes wait for a profile: set pando.profile or pass --profile");
        }
        c.close();
        return Ok((added, false, false));
    };
    if o.retry_at.is_some_and(|t| o.clock() < t) {
        c.close();
        return Ok((added, false, true));
    }
    let before = c.pending();
    let mut run = TendRun::default();
    let terr = tend_calls(ctx, &mut c, summ.as_ref(), 1, &mut run);
    if let Some(e) = terr {
        if ctx.err().is_none() {
            o.backoff = (o.backoff * 2).max(Duration::from_secs(15)).min(Duration::from_secs(300));
            o.retry_at = Some(o.clock() + o.backoff);
            let _ = writeln!(o.log, "follow: the model failed, trying again in {}s: {}", o.backoff.as_secs(), clip_err(&e));
            c.close();
            return Ok((added, false, true));
        }
    }
    o.backoff = Duration::ZERO;
    o.retry_at = None;
    let built = c.pending() < before || run.built > 0;
    let mut more = c.pending() > 0;
    c.close();
    if on_sub && !more {
        more = super::branch::any_pending_sub(&o.dir);
    }
    Ok((added, built, more))
}

/// Runs follow_once until ctx ends.
pub fn follow(ctx: &Ctx, o: &mut FollowOpts) -> Result<(), String> {
    std::fs::create_dir_all(&o.dir).map_err(|e| e.to_string())?;
    while ctx.err().is_none() {
        let mut more = false;
        match follow_once(ctx, o) {
            Ok((_, _, m)) => more = m,
            Err(e) => {
                if ctx.err().is_none() {
                    let _ = writeln!(o.log, "follow: {e}");
                }
            }
        }
        let wait = if more && o.retry_at.map_or(true, |t| o.clock() > t) { Duration::ZERO } else { o.interval };
        let end = std::time::Instant::now() + wait;
        while ctx.err().is_none() && std::time::Instant::now() < end {
            std::thread::sleep(Duration::from_millis(100).min(wait));
        }
    }
    Ok(())
}

/// The agent profile that builds nodes in the background: the
/// DAISUGI_PANDO_PROFILE variable (getenv), else the profile file. "" is none.
pub fn read_profile(data_dir: &Path, getenv: Option<&dyn Fn(&str) -> String>) -> String {
    if let Some(g) = getenv {
        let v = g("DAISUGI_PANDO_PROFILE");
        if !v.trim().is_empty() {
            return v.trim().to_string();
        }
    }
    std::fs::read_to_string(data_dir.join("pando").join(PROFILE_FILE))
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

pub(crate) fn write_profile(data_dir: &Path, name: &str) -> Result<(), String> {
    let p = data_dir.join("pando").join(PROFILE_FILE);
    std::fs::create_dir_all(p.parent().unwrap_or(Path::new("."))).map_err(|e| e.to_string())?;
    std::fs::write(&p, format!("{name}\n")).map_err(|e| e.to_string())
}
