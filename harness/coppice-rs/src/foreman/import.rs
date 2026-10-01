//! Import of old Claude Code and Codex chats (FM-R-71 to FM-R-77). The twin
//! of Go's internal/foreman import.go and importcli.go.
//!
//! Import is the owner's choice and never automatic. Two passes: `scan`
//! counts what a transcript tree holds, with no model call and no write;
//! the `Importer` then feeds the messages one at a time into the chat as
//! kind past. The queue of sessions and the choice live in import.json,
//! apart from the chat, so a pause and a restart need nothing from the
//! log. Every imported message carries an act id built from its
//! transcript, so a message is never logged twice.

use std::collections::{BTreeMap, VecDeque};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, SystemTime};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::chat::{self, date_text, is_day, parse_date, Chat};
use super::ctx::Ctx;
use super::fsutil::{self, clip};

/// The state file in the foreman dir.
pub const IMPORT_FILE: &str = "import.json";

pub const SRC_CLAUDE: &str = "claude";
pub const SRC_CODEX: &str = "codex";

/// The most bytes of a tool call or its result.
const TOOL_BYTES: usize = 2000;

/// What a scan or an import takes.
#[derive(Clone, Debug, Default)]
pub struct ImportOpts {
    /// "claude", "codex" or "all" (or "": all).
    pub from: String,
    /// Seconds since the epoch; messages before it are dropped. None keeps all.
    pub since: Option<i64>,
    /// Adds tool calls and their results. Thinking is never taken.
    pub tools: bool,
    /// Dirs whose sessions are never taken: a session whose cwd is under
    /// one is coppice's own (FM-R-78).
    pub skip: Vec<String>,
}

impl ImportOpts {
    fn takes(&self, src: &str) -> bool {
        self.from.is_empty() || self.from == "all" || self.from == src
    }
}

/// One message of a transcript, ready for the chat.
#[derive(Clone, Debug)]
pub struct ImportMsg {
    pub text: String,
    pub ts: i64,
    pub act: String,
}

/// One transcript file: its key, where it is, and how many new messages it
/// holds.
#[derive(Clone, Debug)]
pub struct ImportSession {
    pub key: String,
    pub src: String,
    pub path: PathBuf,
    pub new: usize,
    pub first: i64,
}

/// A time as RFC 3339 reads one, to the second, as seconds since the epoch.
pub fn parse_import_time(s: &str) -> Option<i64> {
    let b = s.as_bytes();
    if !s.is_ascii()
        || b.len() < 20
        || b[4] != b'-'
        || b[7] != b'-'
        || b[10] != b'T'
        || b[13] != b':'
        || b[16] != b':'
    {
        return None;
    }
    if !is_day(&s[..10]) {
        return None;
    }
    let num = |r: &[u8]| -> Option<i64> {
        r.iter()
            .all(u8::is_ascii_digit)
            .then(|| r.iter().fold(0i64, |n, c| n * 10 + (c - b'0') as i64))
    };
    let (h, mi, sec) = (num(&b[11..13])?, num(&b[14..16])?, num(&b[17..19])?);
    if h > 23 || mi > 59 || sec > 59 {
        return None;
    }
    let mut k = 19;
    if b[k] == b'.' {
        k += 1;
        let from = k;
        while k < b.len() && b[k].is_ascii_digit() {
            k += 1;
        }
        if k == from {
            return None;
        }
    }
    let off = match &b[k..] {
        b"Z" => 0,
        r if r.len() == 6 && (r[0] == b'+' || r[0] == b'-') && r[3] == b':' => {
            let (oh, om) = (num(&r[1..3])?, num(&r[4..6])?);
            if oh > 23 || om > 59 {
                return None;
            }
            let v = oh * 3600 + om * 60;
            if r[0] == b'-' {
                -v
            } else {
                v
            }
        }
        _ => return None,
    };
    let secs = day_secs(&s[..10])? + h * 3600 + mi * 60 + sec;
    Some(secs - off)
}

/// Seconds since the epoch of 00:00:00 UTC on a day YYYY-MM-DD.
fn day_secs(day: &str) -> Option<i64> {
    let t = parse_date(&format!("{day}T00:00:00Z"))?;
    Some(chat::unix_secs(t))
}

/// Seconds since the epoch of the date `YYYY-MM-DD`, if it is one.
pub fn parse_day(s: &str) -> Option<i64> {
    if !is_day(s) {
        return None;
    }
    day_secs(s)
}

/// A time as the log writes a date: 2006-01-02T15:04:05Z.
fn secs_text(secs: i64) -> String {
    let t = if secs >= 0 {
        SystemTime::UNIX_EPOCH + Duration::from_secs(secs as u64)
    } else {
        SystemTime::UNIX_EPOCH - Duration::from_secs((-secs) as u64)
    };
    date_text(t)
}

fn secs_time(secs: i64) -> SystemTime {
    if secs >= 0 {
        SystemTime::UNIX_EPOCH + Duration::from_secs(secs as u64)
    } else {
        SystemTime::UNIX_EPOCH - Duration::from_secs((-secs) as u64)
    }
}

/// One session waiting to be imported.
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq)]
#[serde(default)]
pub struct ImportQueued {
    pub key: String,
    pub src: String,
    pub path: String,
    /// How many new messages the scan found, and how many are logged.
    pub n: i64,
    pub fed: i64,
    /// The options this session was accepted with. A later accept with
    /// other options changes only the sessions it adds (FM-R-79).
    pub since: String,
    pub tools: bool,
}

impl ImportQueued {
    /// The scan options this session was queued with.
    pub fn opts(&self, st: &ImportState) -> ImportOpts {
        ImportOpts {
            from: st.from.clone(),
            since: parse_import_time(&self.since),
            tools: self.tools,
            skip: Vec::new(),
        }
    }
}

/// import.json.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
#[serde(default)]
pub struct ImportState {
    pub v: i64,
    pub offered: bool,
    pub decision: String,
    pub paused: bool,
    pub from: String,
    pub since: String,
    pub tools: bool,
    pub total: i64,
    pub done: i64,
    /// The time of the newest message logged for each session.
    pub last: BTreeMap<String, String>,
    pub queue: Vec<ImportQueued>,
}

impl Default for ImportState {
    fn default() -> Self {
        ImportState {
            v: 1,
            offered: false,
            decision: String::new(),
            paused: false,
            from: String::new(),
            since: String::new(),
            tools: false,
            total: 0,
            done: 0,
            last: BTreeMap::new(),
            queue: Vec::new(),
        }
    }
}

impl ImportState {
    /// What the owner sees: off, declined, running, paused or done.
    pub fn word(&self) -> &'static str {
        if self.decision == "declined" {
            "declined"
        } else if self.decision != "accepted" {
            "off"
        } else if self.paused {
            "paused"
        } else if self.queue.is_empty() {
            "done"
        } else {
            "running"
        }
    }

    /// The scan options the state holds.
    pub fn opts(&self) -> ImportOpts {
        ImportOpts {
            from: self.from.clone(),
            since: parse_import_time(&self.since),
            tools: self.tools,
            skip: Vec::new(),
        }
    }

    /// "done of total" for the status line.
    pub fn progress(&self) -> String {
        format!("{} of {}", self.done, plural(self.total, "message"))
    }
}

/// The status as floor.import and `--status --json` give it.
#[derive(Serialize)]
pub struct ImportStatus {
    pub state: String,
    pub offered: bool,
    pub paused: bool,
    pub done: i64,
    pub total: i64,
    pub queued: usize,
}

pub fn status_of(s: &ImportState) -> ImportStatus {
    ImportStatus {
        state: s.word().to_string(),
        offered: s.offered,
        paused: s.paused,
        done: s.done,
        total: s.total,
        queued: s.queue.len(),
    }
}

/// Reads import.json in dir. A missing file is the empty state. A file that
/// cannot be read as state is set aside as import.json.broken and the empty
/// state is returned, so import never stops coppice.
pub fn read_import_state(dir: &Path) -> Result<ImportState, String> {
    let path = dir.join(IMPORT_FILE);
    let data = match std::fs::read(&path) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(ImportState::default()),
        Err(e) => return Err(fsutil::path_err("open", &path, &e)),
    };
    match serde_json::from_slice::<ImportState>(&data) {
        Ok(s) if s.v == 1 => Ok(s),
        _ => {
            let to = chat::free_name(&format!("{}.broken", path.display()));
            let _ = std::fs::rename(&path, to);
            Ok(ImportState::default())
        }
    }
}

/// Reads import.json under a short lock, lets f change it, and writes it
/// back, so the command and the server never lose each other's change.
pub fn update_import_state(
    dir: &Path,
    f: impl FnOnce(&mut ImportState) -> Result<(), String>,
) -> Result<(), String> {
    use std::os::unix::fs::OpenOptionsExt;
    use std::os::unix::io::AsRawFd;
    fsutil::ensure_dir(dir)?;
    let lock_path = dir.join("import.lock");
    let lock = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .mode(fsutil::FILE_MODE)
        .open(&lock_path)
        .map_err(|e| fsutil::path_err("open", &lock_path, &e))?;
    // SAFETY: flock on a descriptor this function owns; it waits for the
    // holder, who keeps it for one read and one write.
    if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX) } != 0 {
        return Err(crate::sys::go_errno_text(&std::io::Error::last_os_error()));
    }
    let mut s = read_import_state(dir)?;
    f(&mut s)?;
    let mut data = serde_json::to_string(&s).map_err(|e| e.to_string())?;
    data.push('\n');
    fsutil::write_atomic(&dir.join(IMPORT_FILE), data.as_bytes())
}

// ---- transcripts ----

const CLAUDE_SKIP: [&str; 5] = [
    "<command-name>",
    "<command-message>",
    "<local-command-",
    "<system-reminder>",
    "Caveat: The messages below were generated by the user while running local commands.",
];
const CODEX_SKIP: [&str; 4] = [
    "<environment_context>",
    "<user_instructions>",
    "<INSTRUCTIONS>",
    "# AGENTS.md",
];

fn has_prefix(s: &str, ps: &[&str]) -> bool {
    ps.iter().any(|p| s.starts_with(p))
}

fn str_of(v: &Value) -> &str {
    v.as_str().unwrap_or("")
}

/// The text of a content value: a string, or the text of each block that
/// has one of the given types, joined by a blank line.
fn block_text(content: &Value, types: &[&str]) -> String {
    if let Some(s) = content.as_str() {
        return s.to_string();
    }
    let Some(list) = content.as_array() else {
        return String::new();
    };
    let mut parts: Vec<&str> = Vec::new();
    for b in list {
        if !b.is_object() {
            continue;
        }
        if types.contains(&str_of(&b["type"])) {
            if let Some(t) = b["text"].as_str() {
                parts.push(t);
            }
        }
    }
    parts.join("\n\n")
}

/// A tool call on one line: its name and the one field that says what it
/// does.
fn call_summary(name: &str, input: &Value) -> String {
    let mut line = name.to_string();
    for k in [
        "command",
        "file_path",
        "path",
        "pattern",
        "url",
        "query",
        "prompt",
    ] {
        let v = match &input[k] {
            Value::String(s) => s.clone(),
            Value::Array(a) => a.iter().map(|p| str_of(p)).collect::<Vec<_>>().join(" "),
            _ => String::new(),
        };
        if !v.is_empty() {
            line.push(' ');
            line.push_str(&v);
            break;
        }
    }
    clip(&line, TOOL_BYTES).to_string()
}

/// What Go's filepath.Base says.
fn go_base(s: &str) -> String {
    if s.is_empty() {
        return ".".into();
    }
    let t = s.trim_end_matches('/');
    if t.is_empty() {
        return "/".into();
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

pub(crate) struct SessionRead {
    pub key: String,
    pub msgs: Vec<ImportMsg>,
}

struct Adder<'a> {
    head: String,
    where_: String,
    since: Option<i64>,
    out: &'a mut Vec<ImportMsg>,
}

impl Adder<'_> {
    fn add(&mut self, role: &str, text: &str, ts: i64, act: String) {
        if text.trim().is_empty() {
            return;
        }
        if self.since.is_some_and(|s| ts < s) {
            return;
        }
        self.out.push(ImportMsg {
            text: format!("{}{}{}: {}", self.head, role, self.where_, text),
            ts,
            act,
        });
    }
}

/// Splits a transcript into lines as Go's bufio.ScanLines does.
fn lines_of(data: &[u8]) -> Vec<&[u8]> {
    let mut out: Vec<&[u8]> = data.split(|c| *c == b'\n').collect();
    if out.last().is_some_and(|l| l.is_empty()) {
        out.pop();
    }
    for l in out.iter_mut() {
        if l.last() == Some(&b'\r') {
            *l = &l[..l.len() - 1];
        }
    }
    out
}

fn parse_obj(ln: &[u8]) -> Option<Value> {
    serde_json::from_slice::<Value>(ln)
        .ok()
        .filter(Value::is_object)
}

/// Reads the importable messages of one transcript file. A line that is
/// not JSON, or has no time, is skipped.
pub(crate) fn read_session(path: &Path, src: &str, o: &ImportOpts) -> Result<SessionRead, String> {
    read_session_key(path, src, o, "")
}

#[cfg(test)]
thread_local! {
    /// Sees the path of every transcript read; tests prove where a read runs.
    pub(crate) static READ_HOOK: std::cell::RefCell<Option<Box<dyn Fn(&Path)>>> =
        std::cell::RefCell::new(None);
}

/// Whether dir is one of the dirs or inside one. Both are compared as
/// written and with symlinks resolved, because Claude Code records the
/// resolved cwd and a dir may be reached through a link (N-3).
fn under_any(dir: &str, dirs: &[String]) -> bool {
    if dir.is_empty() {
        return false;
    }
    let forms = path_forms(dir);
    dirs.iter().filter(|d| !d.is_empty()).any(|d| {
        path_forms(d).iter().any(|d| {
            forms
                .iter()
                .any(|f| f == d || f.starts_with(&format!("{}/", d.trim_end_matches('/'))))
        })
    })
}

/// The cleaned path, and the symlink-resolved one (the longest part that
/// exists, since a cwd may be gone by now) when it differs.
fn path_forms(p: &str) -> Vec<String> {
    let p = clean_path(p);
    let mut head = std::path::PathBuf::from(&p);
    let mut tail: Vec<std::ffi::OsString> = Vec::new();
    loop {
        if let Ok(mut r) = std::fs::canonicalize(&head) {
            for t in tail.iter().rev() {
                r.push(t);
            }
            let r = clean_path(&r.to_string_lossy());
            if r != p {
                return vec![p, r];
            }
            break;
        }
        let Some(name) = head.file_name().map(|n| n.to_os_string()) else {
            break;
        };
        tail.push(name);
        if !head.pop() {
            break;
        }
    }
    vec![p]
}

/// Go's filepath.Clean for the paths import compares.
fn clean_path(p: &str) -> String {
    let abs = p.starts_with('/');
    let mut parts: Vec<&str> = Vec::new();
    for seg in p.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                if parts.last().is_some_and(|l| *l != "..") {
                    parts.pop();
                } else if !abs {
                    parts.push("..");
                }
            }
            s => parts.push(s),
        }
    }
    let j = parts.join("/");
    match (abs, j.is_empty()) {
        (true, _) => format!("/{j}"),
        (false, true) => ".".into(),
        (false, false) => j,
    }
}

/// read_session with the queue key fixed by the caller, for a second file
/// that holds the same session id (FM-R-80). A session of coppice's own
/// (its cwd is under o.skip, or its first user message is a compactor
/// question) gives no message (FM-R-78).
pub(crate) fn read_session_key(
    path: &Path,
    src: &str,
    o: &ImportOpts,
    force: &str,
) -> Result<SessionRead, String> {
    #[cfg(test)]
    READ_HOOK.with(|h| {
        if let Some(f) = h.borrow().as_ref() {
            f(path)
        }
    });
    let data = std::fs::read(path).map_err(|e| fsutil::path_err("open", path, &e))?;
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let stem = name.strip_suffix(".jsonl").unwrap_or(&name).to_string();
    let mut key = format!("{src}:{stem}");
    let lines = lines_of(&data);
    let (mut cwd, mut sid) = (String::new(), String::new());
    for ln in &lines {
        let Some(v) = parse_obj(ln) else { continue };
        if src == SRC_CLAUDE {
            if sid.is_empty() {
                sid = str_of(&v["sessionId"]).to_string();
            }
            if cwd.is_empty() {
                cwd = str_of(&v["cwd"]).to_string();
            }
        } else if str_of(&v["type"]) == "session_meta" {
            let p = &v["payload"];
            if sid.is_empty() {
                sid = str_of(&p["id"]).to_string();
            }
            if cwd.is_empty() {
                cwd = str_of(&p["cwd"]).to_string();
            }
        }
    }
    if !sid.is_empty() {
        key = format!("{src}:{sid}");
        // A file whose name does not hold its session id (a copy, a moved or
        // forked transcript) is its own session for the queue.
        if !stem.contains(&sid) {
            key.push('~');
            key.push_str(&stem);
        }
    }
    if !force.is_empty() {
        key = force.to_string();
    }
    if under_any(&cwd, &o.skip) {
        return Ok(SessionRead {
            key,
            msgs: Vec::new(),
        });
    }
    let project = if !cwd.is_empty() {
        go_base(&cwd)
    } else if src == SRC_CLAUDE {
        let dir = path.parent().map(|d| d.to_string_lossy().into_owned());
        go_base(&dir.unwrap_or_default())
    } else {
        String::new()
    };
    let mut msgs = Vec::new();
    {
        let mut ad = Adder {
            head: format!("{src} "),
            where_: if project.is_empty() {
                String::new()
            } else {
                format!(" in {project}")
            },
            since: o.since,
            out: &mut msgs,
        };
        let mut seen_lines: std::collections::HashMap<String, usize> =
            std::collections::HashMap::new();
        for (n, ln) in lines.iter().enumerate() {
            let Some(v) = parse_obj(ln) else { continue };
            let Some(ts) = parse_import_time(str_of(&v["timestamp"])) else {
                continue;
            };
            if src == SRC_CLAUDE {
                read_claude_line(&v, n, &key, o, ts, &mut ad);
            } else {
                let act = codex_act(ln, &mut seen_lines, &sid, &key);
                read_codex_line(&v, act, o, ts, &mut ad);
            }
        }
    }
    // A compactor call of coppice itself: its one user message is the
    // compactor's question (FM-R-78).
    let user = format!("{src} user");
    if let Some(m) = msgs.iter().find(|m| m.text.starts_with(&user)) {
        if m.text.contains(super::compactor::COMPACTOR_MARKER) {
            msgs.clear();
        }
    }
    Ok(SessionRead { key, msgs })
}

fn read_claude_line(v: &Value, n: usize, key: &str, o: &ImportOpts, ts: i64, ad: &mut Adder) {
    let typ = str_of(&v["type"]);
    if typ != "user" && typ != "assistant" {
        return;
    }
    if v["isMeta"] == Value::Bool(true) || v["isSidechain"] == Value::Bool(true) {
        return;
    }
    let mut id = str_of(&v["uuid"]).to_string();
    if id.is_empty() {
        id = format!("{key}.{n}");
    }
    let base = format!("imp:claude:{id}");
    let content = &v["message"]["content"];
    let text = block_text(content, &["text"]);
    if !(typ == "user" && has_prefix(&text, &CLAUDE_SKIP)) {
        ad.add(typ, &text, ts, base.clone());
    }
    if !o.tools {
        return;
    }
    let Some(list) = content.as_array() else {
        return;
    };
    for (k, b) in list.iter().enumerate() {
        match str_of(&b["type"]) {
            "tool_use" => ad.add(
                "tool",
                &call_summary(str_of(&b["name"]), &b["input"]),
                ts,
                format!("{base}:t{k}"),
            ),
            "tool_result" => ad.add(
                "tool result",
                clip(&block_text(&b["content"], &["text"]), TOOL_BYTES),
                ts,
                format!("{base}:t{k}"),
            ),
            _ => {}
        }
    }
}

/// Names a Codex line by its session id and its own bytes, so a copied or
/// forked rollout that repeats a line brings it once (N-5). A line that
/// repeats inside one file gets a count after its hash.
fn codex_act(
    ln: &[u8],
    seen: &mut std::collections::HashMap<String, usize>,
    sid: &str,
    key: &str,
) -> String {
    let sid = if sid.is_empty() { key } else { sid };
    // Go's line scanner drops a trailing CR; so does the hash.
    let ln = ln.strip_suffix(b"\r").unwrap_or(ln);
    let h: String = crate::sha256::digest(ln)[..8]
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect();
    let k = seen.entry(h.clone()).or_insert(0);
    let n = *k;
    *k += 1;
    let mut act = format!("imp:codex:{sid}:{h}");
    if n > 0 {
        act.push_str(&format!(".{n}"));
    }
    act
}

fn read_codex_line(v: &Value, act: String, o: &ImportOpts, ts: i64, ad: &mut Adder) {
    if str_of(&v["type"]) != "response_item" {
        return;
    }
    let p = &v["payload"];
    match str_of(&p["type"]) {
        "message" => {
            let role = str_of(&p["role"]);
            if role != "user" && role != "assistant" {
                return;
            }
            let text = block_text(&p["content"], &["input_text", "output_text", "text"]);
            if role == "user" && has_prefix(text.trim(), &CODEX_SKIP) {
                return;
            }
            ad.add(role, &text, ts, act);
        }
        "function_call" => {
            if !o.tools {
                return;
            }
            let args: Value = serde_json::from_str(str_of(&p["arguments"])).unwrap_or(Value::Null);
            ad.add("tool", &call_summary(str_of(&p["name"]), &args), ts, act);
        }
        "function_call_output" => {
            if !o.tools {
                return;
            }
            ad.add(
                "tool result",
                clip(str_of(&p["output"]), TOOL_BYTES),
                ts,
                act,
            );
        }
        _ => {}
    }
}

/// A cheap signature of the transcript files under home: their count, the
/// newest mtime and the total size. It reads no file. A cached estimate is
/// fresh only while the signature is the same (F-1).
pub fn transcript_sig(home: &Path) -> String {
    let files = transcript_files(home, &ImportOpts::default());
    let (mut newest, mut size) = (0u128, 0u64);
    for (_, p) in &files {
        if let Ok(md) = std::fs::metadata(p) {
            if let Ok(t) = md.modified() {
                if let Ok(d) = t.duration_since(std::time::UNIX_EPOCH) {
                    newest = newest.max(d.as_nanos());
                }
            }
            size += md.len();
        }
    }
    format!("{}/{newest}/{size}", files.len())
}

/// The transcript files under home for the sources o takes, in path order.
fn transcript_files(home: &Path, o: &ImportOpts) -> Vec<(String, PathBuf)> {
    let mut out = Vec::new();
    if o.takes(SRC_CLAUDE) {
        let mut names: Vec<PathBuf> = Vec::new();
        if let Ok(rd) = std::fs::read_dir(home.join(".claude").join("projects")) {
            for d in rd.flatten() {
                if let Ok(rd2) = std::fs::read_dir(d.path()) {
                    for f in rd2.flatten() {
                        if f.file_name().to_string_lossy().ends_with(".jsonl") {
                            names.push(f.path());
                        }
                    }
                }
            }
        }
        names.sort_by(|a, b| a.as_os_str().cmp(b.as_os_str()));
        out.extend(names.into_iter().map(|p| (SRC_CLAUDE.to_string(), p)));
    }
    if o.takes(SRC_CODEX) {
        let mut names: Vec<PathBuf> = Vec::new();
        walk(&home.join(".codex").join("sessions"), &mut names);
        names.sort_by(|a, b| a.as_os_str().cmp(b.as_os_str()));
        out.extend(names.into_iter().map(|p| (SRC_CODEX.to_string(), p)));
    }
    out
}

fn walk(dir: &Path, out: &mut Vec<PathBuf>) {
    let Ok(rd) = std::fs::read_dir(dir) else {
        return;
    };
    for e in rd.flatten() {
        let Ok(t) = e.file_type() else { continue };
        let name = e.file_name().to_string_lossy().into_owned();
        if t.is_dir() {
            walk(&e.path(), out);
        } else if t.is_file() && name.starts_with("rollout-") && name.ends_with(".jsonl") {
            out.push(e.path());
        }
    }
}

/// The messages of r that are not in the chat. With the chat at hand the act
/// ids decide, exactly: a later import with tool calls on still brings the
/// calls that are older than the last message. Without it (the server's
/// estimate), a message older than the session's last logged time counts as
/// imported.
fn new_msgs(
    msgs: &[ImportMsg],
    last: Option<&String>,
    has: Option<&dyn Fn(&str) -> bool>,
) -> Vec<ImportMsg> {
    let lt = last.and_then(|l| parse_import_time(l));
    msgs.iter()
        .filter(|m| match has {
            Some(h) => !h(&m.act),
            None => lt.is_none_or(|t| m.ts >= t),
        })
        .cloned()
        .collect()
}

/// The estimate pass: reads the transcripts under home and lists the
/// sessions that hold new messages, oldest first (first message time, then
/// key). It calls no model and writes nothing. has says whether an act id
/// is in the chat already; None means none is. It stops between files when
/// ctx ends.
pub fn scan(
    ctx: &Ctx,
    home: &Path,
    o: &ImportOpts,
    st: &ImportState,
    has: Option<&dyn Fn(&str) -> bool>,
) -> Vec<ImportSession> {
    let mut out: Vec<ImportSession> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    for (src, path) in transcript_files(home, o) {
        if ctx.err().is_some() {
            return Vec::new();
        }
        let Ok(mut r) = read_session(&path, &src, o) else {
            continue;
        };
        if seen.contains(&r.key) {
            // Two files with one session id and one name: the second is its
            // own session, keyed by its path (FM-R-80).
            let sum = crate::sha256::digest(path.to_string_lossy().as_bytes());
            let short: String = sum[..4].iter().map(|b| format!("{b:02x}")).collect();
            let key = format!("{}~{short}", r.key);
            if seen.contains(&key) {
                continue;
            }
            r = match read_session_key(&path, &src, o, &key) {
                Ok(r) => r,
                Err(_) => continue,
            };
        }
        seen.insert(r.key.clone());
        let fresh = new_msgs(&r.msgs, st.last.get(&r.key), has);
        if fresh.is_empty() {
            continue;
        }
        out.push(ImportSession {
            key: r.key,
            src,
            path,
            new: fresh.len(),
            first: fresh[0].ts,
        });
    }
    out.sort_by(|a, b| a.first.cmp(&b.first).then_with(|| a.key.cmp(&b.key)));
    out
}

/// Sessions and messages.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct ImportCount {
    pub sessions: usize,
    pub messages: usize,
}

/// Counts the sessions and messages of a scan, all and by source.
pub fn totals(ss: &[ImportSession]) -> (ImportCount, BTreeMap<String, ImportCount>) {
    let mut all = ImportCount::default();
    let mut by: BTreeMap<String, ImportCount> = BTreeMap::new();
    for s in ss {
        all.sessions += 1;
        all.messages += s.new;
        let c = by.entry(s.src.clone()).or_default();
        c.sessions += 1;
        c.messages += s.new;
    }
    (all, by)
}

pub fn plural(n: i64, one: &str) -> String {
    if n == 1 {
        format!("{n} {one}")
    } else {
        format!("{n} {one}s")
    }
}

/// The estimate as the owner reads it.
pub fn describe(ss: &[ImportSession]) -> String {
    let (all, by) = totals(ss);
    let mut b = format!(
        "past chats: {} (about {})\n",
        all.sessions,
        plural(all.messages as i64, "message")
    );
    for src in [SRC_CLAUDE, SRC_CODEX] {
        if let Some(c) = by.get(src) {
            b.push_str(&format!(
                "  {src}: {}, {}\n",
                plural(c.sessions as i64, "chat"),
                plural(c.messages as i64, "message")
            ));
        }
    }
    b
}

/// Records the owner's choice to import and queues the sessions the scan
/// found that are not queued already. It resets the progress when nothing
/// was queued before.
pub fn accept(dir: &Path, o: &ImportOpts, ss: &[ImportSession]) -> Result<(), String> {
    update_import_state(dir, |st| {
        st.decision = "accepted".into();
        st.paused = false;
        st.offered = true;
        st.from = o.from.clone();
        st.tools = o.tools;
        st.since = o.since.map(secs_text).unwrap_or_default();
        if st.queue.is_empty() {
            st.total = 0;
            st.done = 0;
        }
        for s in ss {
            if st.queue.iter().any(|q| q.key == s.key) {
                continue;
            }
            st.queue.push(ImportQueued {
                key: s.key.clone(),
                src: s.src.clone(),
                path: s.path.to_string_lossy().into_owned(),
                n: s.new as i64,
                fed: 0,
                since: st.since.clone(),
                tools: o.tools,
            });
            st.total += s.new as i64;
        }
        Ok(())
    })
}

/// Records that the owner said no. The command still works later.
pub fn decline(dir: &Path) -> Result<(), String> {
    update_import_state(dir, |st| {
        st.decision = "declined".into();
        st.offered = true;
        Ok(())
    })
}

/// Pauses or resumes the queue.
pub fn set_paused(dir: &Path, paused: bool) -> Result<(), String> {
    update_import_state(dir, |st| {
        st.paused = paused;
        Ok(())
    })
}

/// Takes a finished session off the queue. A session that gave fewer
/// messages than the scan counted lowers the total, so done reaches total.
fn pop(st: &mut ImportState, key: &str) {
    if let Some(k) = st.queue.iter().position(|q| q.key == key) {
        let q = st.queue.remove(k);
        if q.fed < q.n {
            st.total -= q.n - q.fed;
        }
    }
}

// ---- the importer ----

/// Feeds the queue into a chat, one message at a time.
pub struct Importer {
    pub dir: PathBuf,
    /// Dirs whose sessions are never taken (FM-R-78).
    pub skip: Vec<String>,
    cur: String,
    /// raw is the session as read; pending is raw without the messages the
    /// chat holds. The read runs outside the chat lock (preload).
    raw: Vec<ImportMsg>,
    filtered: bool,
    pending: VecDeque<ImportMsg>,
}

impl Importer {
    pub fn new(dir: PathBuf) -> Importer {
        Importer {
            dir,
            skip: Vec::new(),
            cur: String::new(),
            raw: Vec::new(),
            filtered: false,
            pending: VecDeque::new(),
        }
    }

    /// Reads the queue's head session, with the options it was queued with.
    fn load(&mut self, q: &ImportQueued, st: &ImportState) {
        self.cur = q.key.clone();
        self.raw = Vec::new();
        self.pending = VecDeque::new();
        self.filtered = false;
        let mut o = q.opts(st);
        o.skip = self.skip.clone();
        if let Ok(r) = read_session_key(Path::new(&q.path), &q.src, &o, &q.key) {
            self.raw = r.msgs;
        }
    }

    /// Reads the head session ahead of step, so a long transcript is never
    /// parsed under the chat lock (FM-R-81).
    pub fn preload(&mut self) {
        let Ok(st) = read_import_state(&self.dir) else {
            return;
        };
        if st.decision != "accepted"
            || st.paused
            || st.queue.is_empty()
            || self.cur == st.queue[0].key
        {
            return;
        }
        let q = st.queue[0].clone();
        self.load(&q, &st);
    }

    /// Logs the next queued message. fed is false when there is nothing to
    /// log now: no choice made, paused, or the queue empty. A session whose
    /// file is gone is dropped from the queue. With load false it never
    /// reads a transcript: when the head session is not the one in hand, or
    /// a session ended, it says again and the caller preloads outside the
    /// chat lock and steps again (FM-R-81).
    fn step_in(&mut self, c: &mut Chat, load: bool) -> Result<(bool, bool), String> {
        let mut st = read_import_state(&self.dir)?;
        while st.decision == "accepted" && !st.paused && !st.queue.is_empty() {
            let q = st.queue[0].clone();
            if self.cur != q.key {
                if !load {
                    return Ok((false, true));
                }
                self.load(&q, &st);
            }
            if !self.filtered {
                let raw = std::mem::take(&mut self.raw);
                self.pending = raw
                    .into_iter()
                    .filter(|m| !c.import_done(&m.text, &m.act))
                    .collect();
                self.filtered = true;
            }
            let Some(m) = self.pending.pop_front() else {
                self.cur.clear();
                update_import_state(&self.dir, |s| {
                    pop(s, &q.key);
                    Ok(())
                })?;
                if !load {
                    // The next head is read outside the lock.
                    return Ok((false, true));
                }
                st = read_import_state(&self.dir)?;
                continue;
            };
            let (_, err) = c.append_imported(&m.text, secs_time(m.ts), &m.act);
            if let Some(e) = err {
                self.pending.push_front(m);
                return Err(e);
            }
            let stamp = secs_text(m.ts);
            let empty = self.pending.is_empty();
            update_import_state(&self.dir, |s| {
                let newer = s
                    .last
                    .get(&q.key)
                    .and_then(|l| parse_import_time(l))
                    .is_none_or(|lt| lt < m.ts);
                if newer {
                    s.last.insert(q.key.clone(), stamp);
                }
                for e in s.queue.iter_mut() {
                    if e.key == q.key {
                        e.fed += 1;
                        s.done += 1;
                    }
                }
                if empty {
                    pop(s, &q.key);
                }
                Ok(())
            })?;
            return Ok((true, false));
        }
        Ok((false, false))
    }

    /// Logs the next queued message when the chat has nothing left to build
    /// and idle says no live work waits (no turn runs, none is queued). It
    /// is the import side of the one compactor loop: the loop drains the
    /// live nodes first, then feeds one import message, then drains again,
    /// so a live message never waits behind more than the one import
    /// message in flight, and an import call never starts while a live node
    /// is pending (FM-R-72).
    pub fn feed(
        &mut self,
        slot: &Mutex<Option<Chat>>,
        idle: &dyn Fn() -> bool,
    ) -> Result<bool, String> {
        loop {
            // idle takes the server's turn lock, which talk holds while it
            // takes the chat lock: ask it before this call takes the chat
            // lock, never under it. A turn queued after the ask lets this
            // one message through.
            if !idle() {
                return Ok(false);
            }
            self.preload();
            let (fed, again) = {
                let mut g = slot.lock().unwrap_or_else(|e| e.into_inner());
                let Some(c) = g.as_mut() else {
                    return Ok(false);
                };
                if c.pending() > 0 {
                    return Ok(false);
                }
                self.step_in(c, false)?
            };
            if fed || !again {
                return Ok(fed);
            }
        }
    }
}

// ---- the command ----

/// The words of coppice foreman import.
#[derive(Default)]
pub struct ImportFlags {
    pub from: String,
    pub since: String,
    dry: bool,
    pause: bool,
    resume: bool,
    status: bool,
    tools: bool,
    chosen: bool,
    many: bool,
    /// Dirs whose sessions are never taken, besides the foreman dir.
    pub skip: Vec<String>,
}

impl ImportFlags {
    pub fn new() -> ImportFlags {
        ImportFlags {
            from: "all".into(),
            ..Default::default()
        }
    }

    pub fn set(&mut self, a: &str) {
        match a {
            "--tools" => {
                self.tools = true;
                return;
            }
            "--dry-run" => self.dry = true,
            "--pause" => self.pause = true,
            "--resume" => self.resume = true,
            "--status" => self.status = true,
            _ => {}
        }
        if self.chosen {
            self.many = true;
        }
        self.chosen = true;
    }
}

/// coppice foreman import. It reads transcripts under HOME and writes
/// import.json; the running server logs the messages (FM-R-71).
pub fn run_import(
    dir: &Path,
    f: &ImportFlags,
    as_json: bool,
    out: &mut dyn Write,
    errw: &mut dyn Write,
) -> i32 {
    run_import_in(dir, f, as_json, None, out, errw)
}

/// run_import with the home dir given; None reads $HOME. A test gives its
/// scratch home.
pub fn run_import_in(
    dir: &Path,
    f: &ImportFlags,
    as_json: bool,
    home: Option<&Path>,
    out: &mut dyn Write,
    errw: &mut dyn Write,
) -> i32 {
    if as_json && !f.status {
        // --json alone is not a mode: it must never start an import (IM-R-9).
        let _ = writeln!(errw, "--json goes with --status");
        return 1;
    }
    if f.many {
        let _ = writeln!(
            errw,
            "import takes one of --dry-run, --pause, --resume and --status"
        );
        return 1;
    }
    if f.from != "claude" && f.from != "codex" && f.from != "all" {
        let _ = writeln!(
            errw,
            "--from needs claude, codex or all, not {}",
            crate::sys::go_quote(&f.from)
        );
        return 1;
    }
    let mut o = ImportOpts {
        from: f.from.clone(),
        since: None,
        tools: f.tools,
        skip: {
            let mut v = f.skip.clone();
            v.push(dir.to_string_lossy().into_owned());
            v
        },
    };
    if !f.since.is_empty() {
        match parse_day(&f.since) {
            Some(t) => o.since = Some(t),
            None => {
                let _ = writeln!(
                    errw,
                    "--since needs a date like 2026-03-01, not {}",
                    crate::sys::go_quote(&f.since)
                );
                return 1;
            }
        }
    }
    if f.pause || f.resume {
        if let Err(e) = set_paused(dir, f.pause) {
            let _ = writeln!(
                errw,
                "cannot write the import state in {}: {e}",
                dir.display()
            );
            return 1;
        }
        let st = read_import_state(dir).unwrap_or_default();
        let mut line = format!("import {}", if f.pause { "paused" } else { "resumed" });
        if st.total > 0 {
            line.push_str(&format!(" ({})", st.progress()));
        }
        let _ = writeln!(out, "{line}");
        return 0;
    }
    if f.status {
        let st = match read_import_state(dir) {
            Ok(s) => s,
            Err(e) => {
                let _ = writeln!(
                    errw,
                    "cannot read the import state in {}: {e}",
                    dir.display()
                );
                return 1;
            }
        };
        if as_json {
            let _ = writeln!(
                out,
                "{}",
                serde_json::to_string(&status_of(&st)).unwrap_or_default()
            );
            return 0;
        }
        let _ = writeln!(out, "import: {}", st.word());
        if st.total > 0 {
            let _ = writeln!(out, "progress: {}", st.progress());
        }
        return 0;
    }
    let home = match home {
        Some(h) => h.to_path_buf(),
        None => match std::env::var_os("HOME").filter(|h| !h.is_empty()) {
            Some(h) => PathBuf::from(h),
            None => {
                let _ = writeln!(errw, "cannot find the home dir: $HOME is not defined");
                return 1;
            }
        },
    };
    import_at(dir, &home, &o, f, out, errw)
}

/// The scan and the queueing of coppice foreman import, for the transcripts
/// under home.
pub fn import_at(
    dir: &Path,
    home: &Path,
    o: &ImportOpts,
    f: &ImportFlags,
    out: &mut dyn Write,
    errw: &mut dyn Write,
) -> i32 {
    let st = match read_import_state(dir) {
        Ok(s) => s,
        Err(e) => {
            let _ = writeln!(
                errw,
                "cannot read the import state in {}: {e}",
                dir.display()
            );
            return 1;
        }
    };
    let chat = Chat::open_read_only(dir).ok();
    let has = chat.as_ref().map(|c| move |a: &str| c.has_act(a));
    let ss = scan(
        &Ctx::background(),
        home,
        o,
        &st,
        has.as_ref().map(|h| h as &dyn Fn(&str) -> bool),
    );
    let _ = write!(out, "{}", describe(&ss));
    if f.dry {
        let _ = writeln!(out, "nothing was changed");
        return 0;
    }
    if ss.is_empty() {
        let _ = writeln!(out, "nothing to import");
        return 0;
    }
    if let Err(e) = accept(dir, o, &ss) {
        let _ = writeln!(
            errw,
            "cannot write the import state in {}: {e}",
            dir.display()
        );
        return 1;
    }
    let _ = writeln!(
        out,
        "queued: they import one message at a time while coppice runs, never ahead of a live message"
    );
    0
}
