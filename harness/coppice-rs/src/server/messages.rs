//! The chat read path: pane.messages reads one pane's transcript, and
//! floor.chat reads the owner's lines from the chat files merged with the
//! foreman's own replies. A messages event tells subscribers that new
//! messages may exist. See PROTOCOL.md, "The chat".

use std::collections::{HashMap, HashSet};
use std::io::{Read, Seek, SeekFrom, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use serde_json::{json, Value};

use super::facts::{open_regular, Kind};
use super::role::Role;
use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};

/// The message a pane, a plugin or an unplaced peer gets for
/// pane.messages of a pane that is not its own.
pub const MESSAGES_REFUSAL: &str =
    "a pane may read only its own messages. The operator reads any pane's.";
/// The message a named web token gets for floor.chat and pane.messages.
pub const CHAT_REFUSAL: &str = "the chat is for the owner's own sign-in.";
/// The chat line the server writes when it starts a foreman after an
/// earlier one.
pub const NEW_FOREMAN_NOTE: &str = "A new foreman started.";

/// The most bytes one read of a transcript takes, from its end, and the
/// most bytes floor.chat reads from the chat files in all.
const MESSAGES_WINDOW: u64 = 4 << 20;
const MESSAGES_DEFAULT: i64 = 100;
const MESSAGES_MAX: i64 = 500;
const MESSAGE_RUNES: usize = 2000;
const TOOL_RUNES: usize = 200;
const CHAT_FOREMEN: usize = 4;
const CHAT_FILES_MAX: usize = 31;
const MESSAGES_EVERY: Duration = Duration::from_millis(500);

fn window() -> u64 {
    MESSAGES_WINDOW
}

/// One message of a transcript or of the chat. Role is owner, agent or
/// note.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Message {
    pub id: String,
    pub at: f64,
    pub role: String,
    pub text: String,
    pub tool: String,
    pub pane: String,
    /// Set on floor.chat's owner lines only: true once that foreman's
    /// transcript records the line as the owner's.
    pub read: Option<bool>,
}

impl Message {
    fn to_value(&self) -> Value {
        let mut v = vec![
            ("id", json!(self.id)),
            ("at", gojson::float(self.at)),
            ("role", json!(self.role)),
            ("text", json!(self.text)),
        ];
        if !self.tool.is_empty() {
            v.push(("tool", json!(self.tool)));
        }
        if !self.pane.is_empty() {
            v.push(("pane", json!(self.pane)));
        }
        if let Some(read) = self.read {
            v.push(("read", json!(read)));
        }
        gojson::obj(v)
    }
}

/// Sets read on each owner line of the chat. A line is read when its
/// foreman's transcript records the same words as the owner's, at or after
/// the time the line was written. Each recorded message reads one line at
/// most, in order, so an older message with the same words never reads a
/// new line, and a line typed while a turn holds stays unread until its
/// own turn records it.
fn mark_read(lines: &mut [Message], heard: &mut HashMap<String, Vec<Message>>) {
    for h in heard.values_mut() {
        // A stable sort, as Go's sort.SliceStable.
        h.sort_by(|a, b| a.at.partial_cmp(&b.at).unwrap_or(std::cmp::Ordering::Equal));
    }
    let mut next: HashMap<String, usize> = HashMap::new();
    for line in lines.iter_mut() {
        if line.role != "owner" {
            continue;
        }
        let mut read = false;
        let key = super::foreman::talk_text(&line.text);
        if let Some(h) = heard.get(&line.pane) {
            let from = next.get(&line.pane).copied().unwrap_or(0);
            for (j, m) in h.iter().enumerate().skip(from) {
                if m.at >= line.at && super::foreman::talk_text(&m.text) == key {
                    read = true;
                    next.insert(line.pane.clone(), j + 1);
                    break;
                }
            }
        }
        line.read = Some(read);
    }
}

/// The chat files this process checked before its first write to each.
#[derive(Default)]
pub struct ChatState {
    checked: HashSet<String>,
    /// Each note floor.chat wrote, so a file that stays unreadable makes
    /// one note, not one per read.
    noted: HashSet<String>,
}

/// Renames path to a free <path>.broken-<UTC time> name, mode 0600, and
/// returns the new name, or path when the move failed.
pub fn move_aside_file(path: &Path) -> String {
    let base = format!("{}.broken-{}", path.display(), utc_stamp());
    let mut target = base.clone();
    let mut n = 2;
    while std::fs::symlink_metadata(&target).is_ok() {
        target = format!("{base}-{n}");
        n += 1;
    }
    if std::fs::rename(path, &target).is_err() {
        return path.display().to_string();
    }
    let _ = std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o600));
    target
}

/// The UTC time now as 20060102T150405Z.
fn utc_stamp() -> String {
    let (y, mo, d, h, mi, s) = crate::web::der::civil(now_parts().0);
    format!("{y:04}{mo:02}{d:02}T{h:02}{mi:02}{s:02}Z")
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "pane.messages",
        Arc::new(|s, c, r| s.handle_pane_messages(c, r)),
    );
    let _ = s.handle("floor.chat", Arc::new(|s, c, r| s.handle_floor_chat(c, r)));
}

impl Client {
    /// True when the connection's hello said its name came from a token
    /// and gave a name: a named web token.
    pub fn token_named(&self) -> bool {
        let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        st.name_fixed && !st.name.is_empty()
    }
}

/// The limit of a request: absent or below 1 is the default, above the
/// most is the most.
fn limit_of(r: &Request) -> usize {
    match r.int("limit") {
        Some(n) if n >= 1 => n.min(MESSAGES_MAX) as usize,
        _ => MESSAGES_DEFAULT as usize,
    }
}

/// Keeps the messages after the one whose id is since, or all when since
/// is "" or names none, and of those the newest limit. more is true when
/// older messages were left out.
pub fn pick(msgs: &[Message], since: &str, limit: usize, cut: bool) -> (Vec<Message>, bool) {
    let (mut from, mut found) = (0, false);
    if !since.is_empty() {
        for (i, m) in msgs.iter().enumerate() {
            if m.id == since {
                from = i + 1;
                found = true;
            }
        }
    }
    let mut out = &msgs[from..];
    let mut more = cut && !found;
    if out.len() > limit {
        out = &out[out.len() - limit..];
        more = true;
    }
    (out.to_vec(), more)
}

fn reply(r: &Request, msgs: &[Message], more: bool) -> Response {
    let list: Vec<Value> = msgs.iter().map(|m| m.to_value()).collect();
    proto::ok_resp(
        &r.id,
        gojson::map(vec![
            ("messages", Value::Array(list)),
            ("source", json!("transcript")),
            ("more", json!(more)),
        ]),
    )
}

/// One complete line of a file and the byte offset it starts at.
struct FileLine {
    off: u64,
    b: Vec<u8>,
}

/// An error of a read: not_found when the file is missing.
struct ReadErr {
    not_found: bool,
}

/// Reads the complete lines in the last window bytes of path, never
/// through a symlink and never a file that is not regular. A line the read
/// starts inside, and a last line with no line feed, are left out.
fn tail_lines(path: &str, window: u64) -> Result<(Vec<FileLine>, u64, u64), ReadErr> {
    let (mut f, _, _) = open_regular(path).map_err(|(_, e)| ReadErr {
        not_found: e.kind() == std::io::ErrorKind::NotFound,
    })?;
    let size = f
        .metadata()
        .map_err(|_| ReadErr { not_found: false })?
        .len();
    let start = size.saturating_sub(window);
    let mut buf = Vec::with_capacity((size - start) as usize);
    f.seek(SeekFrom::Start(start))
        .map_err(|_| ReadErr { not_found: false })?;
    f.take(size - start)
        .read_to_end(&mut buf)
        .map_err(|_| ReadErr { not_found: false })?;
    let mut pos = 0usize;
    if start > 0 {
        match buf.iter().position(|&c| c == b'\n') {
            Some(i) => pos = i + 1,
            None => return Ok((Vec::new(), start, size)),
        }
    }
    let mut out = Vec::new();
    while pos < buf.len() {
        let Some(i) = buf[pos..].iter().position(|&c| c == b'\n') else {
            break;
        };
        out.push(FileLine {
            off: start + pos as u64,
            b: buf[pos..pos + i].to_vec(),
        });
        pos += i + 1;
    }
    Ok((out, start, size))
}

/// A JSON object line, read as Go reads one into a map: bytes that are not
/// UTF-8 become U+FFFD first.
fn object(b: &[u8]) -> Option<serde_json::Map<String, Value>> {
    match serde_json::from_str::<Value>(&String::from_utf8_lossy(b)) {
        Ok(Value::Object(m)) => Some(m),
        _ => None,
    }
}

fn str_of<'a>(m: &'a serde_json::Map<String, Value>, k: &str) -> &'a str {
    m.get(k).and_then(|v| v.as_str()).unwrap_or("")
}

fn is_true(m: &serde_json::Map<String, Value>, k: &str) -> bool {
    matches!(m.get(k), Some(Value::Bool(true)))
}

/// Reads the messages of the last window bytes of path. cut is true when
/// the read did not start at the start of the file. A missing file has no
/// messages.
pub fn read_messages(path: &str, kind: Kind) -> Result<(Vec<Message>, bool), ()> {
    if kind == Kind::Claude && super::facts::verify_real(path).is_err() {
        return Err(());
    }
    read_messages_in(path, kind, window())
}

/// read_messages with the read window given, so a test can make it small
/// without a global other tests read at the same time.
fn read_messages_in(path: &str, kind: Kind, window: u64) -> Result<(Vec<Message>, bool), ()> {
    let (lines, start, _) = match tail_lines(path, window) {
        Ok(v) => v,
        Err(e) if e.not_found => return Ok((Vec::new(), false)),
        Err(_) => return Err(()),
    };
    let mut out = Vec::new();
    for l in &lines {
        let mut got = match kind {
            Kind::Sprig => sprig_messages(&l.b),
            Kind::Claude => claude_messages(&l.b),
        };
        for (i, m) in got.iter_mut().enumerate() {
            m.id = l.off.to_string();
            if i > 0 {
                m.id.push_str(&format!(".{i}"));
            }
        }
        out.extend(got);
    }
    Ok((out, start > 0))
}

/// The leads of an owner text that records a command and its output.
const COMMAND_RECORDS: [&str; 3] = ["<local-command-", "<bash-", "<command-"];

/// Reads one Claude Code transcript line.
fn claude_messages(b: &[u8]) -> Vec<Message> {
    let Some(e) = object(b) else {
        return Vec::new();
    };
    let typ = str_of(&e, "type");
    if typ != "user" && typ != "assistant" {
        return Vec::new();
    }
    if is_true(&e, "isMeta") || is_true(&e, "isSidechain") || is_true(&e, "isCompactSummary") {
        return Vec::new();
    }
    if e.contains_key("toolUseResult") {
        return Vec::new();
    }
    let Some(Value::Object(msg)) = e.get("message") else {
        return Vec::new();
    };
    let at = rfc3339_seconds(str_of(&e, "timestamp"));
    let role = if typ == "assistant" { "agent" } else { "owner" };
    let mut out = Vec::new();
    let mut add = |text: &str, tool: String| {
        let text = clean_message(text);
        if role == "owner" && COMMAND_RECORDS.iter().any(|p| text.starts_with(p)) {
            return;
        }
        if text.is_empty() && tool.is_empty() {
            return;
        }
        out.push(Message {
            at,
            role: role.into(),
            text,
            tool,
            ..Default::default()
        });
    };
    match msg.get("content") {
        Some(Value::String(c)) => add(c, String::new()),
        Some(Value::Array(blocks)) => {
            for x in blocks {
                let Value::Object(blk) = x else {
                    continue;
                };
                match str_of(blk, "type") {
                    "text" => add(str_of(blk, "text"), String::new()),
                    "tool_use" if role == "agent" => add(
                        &tool_summary(blk.get("input"), ""),
                        one_line(str_of(blk, "name"), TOOL_RUNES),
                    ),
                    _ => {}
                }
            }
        }
        _ => {}
    }
    out
}

/// Reads one sprig session tree line.
fn sprig_messages(b: &[u8]) -> Vec<Message> {
    let Some(e) = object(b) else {
        return Vec::new();
    };
    let at = e.get("ts").and_then(|v| v.as_f64()).unwrap_or(0.0);
    let mut m = match str_of(&e, "type") {
        "prompt" => Message {
            role: "owner".into(),
            text: clean_message(str_of(&e, "text")),
            ..Default::default()
        },
        "assistant" => Message {
            role: "agent".into(),
            text: clean_message(str_of(&e, "text")),
            ..Default::default()
        },
        "tool_call" => Message {
            role: "agent".into(),
            text: tool_summary(e.get("input"), str_of(&e, "detail")),
            tool: one_line(str_of(&e, "name"), TOOL_RUNES),
            ..Default::default()
        },
        _ => return Vec::new(),
    };
    if m.text.is_empty() && m.tool.is_empty() {
        return Vec::new();
    }
    m.at = at;
    vec![m]
}

/// Days from 1970-01-01 to a civil date (Howard Hinnant's algorithm).
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

fn days_in_month(y: i64, m: i64) -> i64 {
    match m {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        _ if (y % 4 == 0 && y % 100 != 0) || y % 400 == 0 => 29,
        _ => 28,
    }
}

/// An RFC 3339 time, YYYY-MM-DDTHH:MM:SS[.fraction](Z|+HH:MM|-HH:MM), as
/// unix seconds, or 0 when it does not parse. The same sum as Go's
/// float64(t.UnixNano()) / 1e9.
pub fn rfc3339_seconds(v: &str) -> f64 {
    fn num(b: &[u8]) -> Option<i64> {
        if b.is_empty() || !b.iter().all(|c| c.is_ascii_digit()) {
            return None;
        }
        Some(b.iter().fold(0, |n, c| n * 10 + (c - b'0') as i64))
    }
    let parse = || -> Option<f64> {
        let b = v.as_bytes();
        if b.len() < 20
            || b[4] != b'-'
            || b[7] != b'-'
            || b[10] != b'T'
            || b[13] != b':'
            || b[16] != b':'
        {
            return None;
        }
        let (y, mo, d) = (num(&b[0..4])?, num(&b[5..7])?, num(&b[8..10])?);
        let (h, mi, s) = (num(&b[11..13])?, num(&b[14..16])?, num(&b[17..19])?);
        if !(1..=12).contains(&mo)
            || d < 1
            || d > days_in_month(y, mo)
            || h > 23
            || mi > 59
            || s > 59
        {
            return None;
        }
        let mut rest = &b[19..];
        let mut nanos = 0i64;
        if rest.first() == Some(&b'.') {
            let n = rest[1..].iter().take_while(|c| c.is_ascii_digit()).count();
            if n == 0 {
                return None;
            }
            let digits = &rest[1..1 + n];
            for i in 0..9 {
                nanos = nanos * 10 + digits.get(i).map_or(0, |c| (c - b'0') as i64);
            }
            rest = &rest[1 + n..];
        }
        let off = match rest {
            [b'Z'] => 0,
            [sign @ (b'+' | b'-'), h1, h2, b':', m1, m2] => {
                let (oh, om) = (num(&[*h1, *h2])?, num(&[*m1, *m2])?);
                if oh > 23 || om > 59 {
                    return None;
                }
                let o = oh * 3600 + om * 60;
                if *sign == b'-' {
                    -o
                } else {
                    o
                }
            }
            _ => return None,
        };
        let secs = days_from_civil(y, mo, d) * 86400 + h * 3600 + mi * 60 + s - off;
        Some((secs * 1_000_000_000 + nanos) as f64 / 1e9)
    };
    parse().unwrap_or(0.0)
}

/// text with its control characters dropped but the line feed, and a tab
/// as a space.
fn drop_controls(text: &str) -> String {
    text.chars()
        .filter_map(|c| match c {
            '\t' => Some(' '),
            '\n' => Some('\n'),
            c if (c as u32) < 0x20 || c as u32 == 0x7f || (0x80..=0x9f).contains(&(c as u32)) => {
                None
            }
            // A format character (a bidi mark, a zero-width space) or a
            // line or paragraph separator changes how a line shows.
            '\u{2028}' | '\u{2029}' => None,
            c if super::role::is_cf(c) => None,
            c => Some(c),
        })
        .collect()
}

/// text cut at n runes, the cut marked.
fn cap_runes(text: &str, n: usize) -> String {
    if text.chars().count() <= n {
        return text.to_string();
    }
    let mut s: String = text.chars().take(n).collect();
    s.push('…');
    s
}

/// A message's text: no control characters but the line feed, a tab as a
/// space, trimmed, cut at MESSAGE_RUNES.
pub fn clean_message(text: &str) -> String {
    cap_runes(drop_controls(text).trim(), MESSAGE_RUNES)
}

/// text as one line, each run of white space one space, cut at n runes.
pub fn one_line(text: &str, n: usize) -> String {
    let d = drop_controls(text);
    cap_runes(&d.split_whitespace().collect::<Vec<_>>().join(" "), n)
}

const SUMMARY_KEYS: [&str; 7] = [
    "command",
    "file_path",
    "path",
    "pattern",
    "url",
    "query",
    "description",
];

/// A one-line summary of a tool's input: the first summary key that holds
/// a string with words, else detail.
pub fn tool_summary(input: Option<&Value>, detail: &str) -> String {
    if let Some(Value::Object(m)) = input {
        for k in SUMMARY_KEYS {
            if let Some(Value::String(v)) = m.get(k) {
                let l = one_line(v, TOOL_RUNES);
                if !l.is_empty() {
                    return l;
                }
            }
        }
    }
    one_line(detail, TOOL_RUNES)
}

/// Whether name is a day's chat file name, YYYY-MM-DD.jsonl.
fn chat_day_name(name: &str) -> bool {
    let b = name.as_bytes();
    b.len() == 16
        && name.ends_with(".jsonl")
        && b[4] == b'-'
        && b[7] == b'-'
        && b[..10]
            .iter()
            .enumerate()
            .all(|(i, c)| i == 4 || i == 7 || c.is_ascii_digit())
}

/// Unix seconds and nanoseconds now.
fn now_parts() -> (i64, u32) {
    let d = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    (d.as_secs() as i64, d.subsec_nanos())
}

/// What one read of a chat file found.
struct ChatRead {
    msgs: Vec<Message>,
    used: u64,
    /// The offset just past the last complete line, or None when the read
    /// holds no line feed and did not start at the start.
    end: Option<u64>,
    damaged: bool,
    size: u64,
}

/// Reads the complete lines in the last budget bytes of one chat file.
fn read_chat_file(path: &str, name: &str, budget: u64) -> Result<ChatRead, ReadErr> {
    let (lines, start, size) = tail_lines(path, budget)?;
    let mut r = ChatRead {
        msgs: Vec::new(),
        used: size - start,
        end: if start == 0 { Some(0) } else { None },
        damaged: false,
        size,
    };
    let day = name.trim_end_matches(".jsonl");
    for l in &lines {
        r.end = Some(l.off + l.b.len() as u64 + 1);
        let Some(rec) = object(&l.b) else {
            r.damaged = true;
            r.msgs.clear();
            return Ok(r);
        };
        let role = match str_of(&rec, "kind") {
            "user" => "owner",
            "note" => "note",
            _ => continue,
        };
        let text = clean_message(str_of(&rec, "text"));
        if text.is_empty() {
            continue;
        }
        r.msgs.push(Message {
            id: format!("chat/{day}/{}", l.off),
            at: rfc3339_seconds(str_of(&rec, "date")),
            role: role.into(),
            text,
            tool: String::new(),
            pane: str_of(&rec, "pane").to_string(),
            read: None,
        });
    }
    Ok(r)
}

/// The message a pane, a plugin or an unplaced peer gets for resuming or
/// forking a sprig session that belongs to another pane.
pub const SPRIG_REFUSAL: &str =
    "that sprig session belongs to another pane. Only the operator resumes or forks it.";

impl Server {
    /// Refuses a pane.create that resumes, or a pane.fork that forks, a
    /// sprig session another pane's record names, for a connection that is
    /// not the operator.
    pub fn sprig_guard(&self, ro: &Role, r: &Request) -> Option<Response> {
        if !ro.no_allow {
            return None;
        }
        let refuse = || {
            Some(proto::err_resp(
                &r.id,
                proto::ERR_UNAUTHORIZED,
                SPRIG_REFUSAL,
            ))
        };
        match r.cmd.as_str() {
            "pane.create" => {
                if r.s("harness") != "sprig" {
                    return None;
                }
                let argv = r.str_slice("cmd_argv").unwrap_or_default();
                let rid = crate::adapters::sprig::flag_value(&argv, "resume")?;
                let path = crate::adapters::sprig::tree_path(&argv, &rid)?;
                let owner = self.sprig_owner(&path);
                if !owner.is_empty() && owner != ro.pane_id {
                    return refuse();
                }
                None
            }
            "pane.fork" => {
                let id = r.s("pane");
                let p = self.tree.pane(&id)?;
                if p.harness == "sprig"
                    && p.kind == crate::layout::KIND_HEADLESS
                    && id != ro.pane_id
                {
                    return refuse();
                }
                None
            }
            _ => None,
        }
    }

    /// The pane whose record names the sprig session tree path, compared by
    /// real path, or "".
    fn sprig_owner(&self, path: &str) -> String {
        let want = super::facts::real_transcript_path(path);
        for p in self.tree.panes() {
            if p.harness != "sprig" || p.kind != crate::layout::KIND_HEADLESS {
                continue;
            }
            if let Some(tp) = crate::adapters::sprig::tree_path(
                p.argv.as_deref().unwrap_or_default(),
                &p.harness_session_id,
            ) {
                if tp == path || super::facts::real_transcript_path(&tp) == want {
                    return p.id;
                }
            }
        }
        String::new()
    }

    /// Refuses floor.chat and pane.messages for a connection that may not
    /// read them.
    pub fn chat_guard(&self, c: &Arc<Client>, ro: &Role, r: &Request) -> Option<Response> {
        if r.cmd != "floor.chat" && r.cmd != "pane.messages" {
            return None;
        }
        if r.cmd == "pane.messages" && ro.no_allow {
            let id = r.s("pane");
            let own = ro.pane
                && !ro.unknown
                && ro.plugin.is_empty()
                && !ro.pane_id.is_empty()
                && ro.pane_id == id;
            if !own {
                return Some(proto::err_resp(
                    &r.id,
                    proto::ERR_UNAUTHORIZED,
                    MESSAGES_REFUSAL,
                ));
            }
        }
        if c.token_named() {
            return Some(proto::err_resp(
                &r.id,
                proto::ERR_UNAUTHORIZED,
                CHAT_REFUSAL,
            ));
        }
        None
    }

    fn is_tracked_foreman(&self, id: &str) -> bool {
        let mut t = self.talk_lock();
        !id.is_empty() && self.tracked_foreman(&mut t) == id
    }

    fn handle_pane_messages(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        let Some(p) = self.tree.pane(&id) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!(
                    "no pane {}. Run: coppice pane list",
                    crate::sys::go_quote(&id)
                ),
            );
        };
        let (path, kind) = self.transcript_of(&p);
        if path.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "{} ({id}) has no transcript to read. Its hook reported none.",
                    self.label_of(&id)
                ),
            );
        }
        // A pane reading its own messages reads only a file that began
        // after it did.
        if c.role().pane && kind == Kind::Claude {
            if let Some(refusal) = self.check_first_record(&id, &path) {
                return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, refusal);
            }
        }
        let Ok((msgs, cut)) = read_messages(&path, kind) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!(
                    "cannot read the transcript of {} ({id}).",
                    self.label_of(&id)
                ),
            );
        };
        let (out, more) = pick(&msgs, &r.s("since"), limit_of(r), cut);
        reply(r, &out, more)
    }

    fn chat_dir(&self) -> PathBuf {
        self.cfg.data_dir.join("chat")
    }

    fn chat_lock(&self) -> std::sync::MutexGuard<'_, ChatState> {
        self.chat.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn send_messages_event(&self, pane: &str, chat: bool) {
        let mut v = vec![("event", json!("messages")), ("pane", json!(pane))];
        if chat {
            v.push(("chat", json!(true)));
        }
        v.push(("ts", gojson::float(Server::now_seconds())));
        self.broadcast("messages", pane, &gojson::obj(v));
    }

    /// Appends one line to today's chat file, the day in UTC, then sends a
    /// messages event. A write that fails leaves a note and nothing else.
    /// The caller holds the talk lock.
    pub fn write_chat(&self, kind: &str, pane: &str, text: &str) {
        let (secs, nanos) = now_parts();
        let (y, mo, d, h, mi, s) = crate::web::der::civil(secs);
        let path = self.chat_dir().join(format!("{y:04}-{mo:02}-{d:02}.jsonl"));
        let date = format!(
            "{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}.{:03}Z",
            nanos / 1_000_000
        );
        let mut line = gojson::marshal(&gojson::obj(vec![
            ("kind", json!(kind)),
            ("date", json!(date)),
            ("pane", json!(pane)),
            ("text", json!(text)),
        ]));
        line.push('\n');
        let mut res = Ok(());
        let wrote = {
            let mut st = self.chat_lock();
            self.write_data(|| res = self.append_chat(&mut st, &path, line.as_bytes()))
        };
        if !wrote {
            return;
        }
        if res.is_err() {
            self.note(
                &format!(
                    "cannot write the chat file {}. The words still go to the foreman.",
                    path.display()
                ),
                pane,
            );
            return;
        }
        self.send_messages_event(pane, true);
    }

    /// Writes line to the end of path, the directory 0700 and the file
    /// 0600. Before its first write to a file it checks the file. The
    /// caller holds the chat lock.
    fn append_chat(&self, st: &mut ChatState, path: &Path, line: &[u8]) -> std::io::Result<()> {
        use std::os::unix::fs::DirBuilderExt;
        let dir = path.parent().unwrap_or(Path::new("."));
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)?;
        let m = std::fs::symlink_metadata(dir)?;
        if !m.is_dir() {
            return Err(std::io::Error::other("the chat dir is not a directory"));
        }
        if m.mode() & 0o777 != 0o700 {
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
        let key = path.to_string_lossy().to_string();
        if !st.checked.contains(&key) {
            self.check_chat_file(st, path)?;
            st.checked.insert(key.clone());
        }
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .create(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW)
            .open(path)?;
        let m = f.metadata()?;
        if !m.file_type().is_file() {
            return Err(std::io::Error::other("the chat file is not a regular file"));
        }
        if m.mode() & 0o777 != 0o600 {
            f.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        }
        if let Err(e) = f.write_all(line) {
            st.checked.remove(&key);
            return Err(e);
        }
        Ok(())
    }

    /// Reads path as floor.chat does: a damaged file moves aside, and a
    /// last line with no line feed is cut off. A missing file is fine.
    fn check_chat_file(&self, st: &mut ChatState, path: &Path) -> std::io::Result<()> {
        let name = path
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .unwrap_or_default();
        let r = match read_chat_file(&path.to_string_lossy(), &name, window()) {
            Ok(r) => r,
            Err(e) if e.not_found => return Ok(()),
            Err(_) => return Err(std::io::Error::other("the chat file cannot be read")),
        };
        if r.damaged {
            return self.move_chat_aside(st, path);
        }
        if let Some(end) = r.end {
            if end < r.size {
                return self.move_tail_aside(path, end);
            }
        }
        Ok(())
    }

    /// Moves the bytes of path past end, a line a crash left half
    /// written, to <path>.broken-<UTC time> (0600), then cuts them from
    /// path, and leaves one note that names where they went. No byte is
    /// lost.
    fn move_tail_aside(&self, path: &Path, end: u64) -> std::io::Result<()> {
        let mut f = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(path)?;
        let m = f.metadata()?;
        if !m.file_type().is_file() {
            return Err(std::io::Error::other("the chat file is not a regular file"));
        }
        let mut tail = Vec::new();
        f.seek(SeekFrom::Start(end))?;
        (&mut f).take(m.len() - end).read_to_end(&mut tail)?;
        let base = format!("{}.broken-{}", path.display(), utc_stamp());
        let mut target = base.clone();
        let mut n = 2;
        let mut out = loop {
            match std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .custom_flags(libc::O_NOFOLLOW)
                .open(&target)
            {
                Ok(o) => break o,
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                    target = format!("{base}-{n}");
                    n += 1;
                }
                Err(e) => return Err(e),
            }
        };
        out.write_all(&tail)?;
        // The copy is on disk before any byte leaves the chat file.
        out.sync_all()?;
        drop(out);
        std::fs::File::open(path.parent().unwrap_or(Path::new(".")))?.sync_all()?;
        f.set_len(end)?;
        self.note(
            &format!("A chat file ended in a half-written line. The line moved to {target}."),
            "",
        );
        Ok(())
    }

    /// Writes text as a note the first time this process sees it.
    fn note_once(&self, text: &str) {
        let first = self.chat_lock().noted.insert(text.to_string());
        if first {
            self.note(text, "");
        }
    }

    /// Moves a damaged chat file to <name>.broken-<UTC time>, mode 0600,
    /// and leaves one note that names where it went.
    fn move_chat_aside(&self, st: &mut ChatState, path: &Path) -> std::io::Result<()> {
        let base = format!("{}.broken-{}", path.display(), utc_stamp());
        let mut target = base.clone();
        let mut n = 2;
        while std::fs::symlink_metadata(&target).is_ok() {
            target = format!("{base}-{n}");
            n += 1;
        }
        std::fs::rename(path, &target)?;
        st.checked.remove(&path.to_string_lossy().to_string());
        let _ = std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o600));
        self.note(
            &format!("A chat file could not be read. It moved to {target}. The chat starts fresh."),
            "",
        );
        Ok(())
    }

    /// Reads the newest chat files, oldest message first. cut is true when
    /// it left a file or a part of one unread.
    fn read_chat(&self) -> Result<(Vec<Message>, bool, Vec<String>), ()> {
        let mut st = self.chat_lock();
        let dir = self.chat_dir();
        let ents = match std::fs::read_dir(&dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok((Vec::new(), false, Vec::new()))
            }
            Err(_) => return Ok((Vec::new(), true, vec![dir.display().to_string()])),
        };
        let mut unread = Vec::new();
        let mut names = Vec::new();
        for e in ents {
            let Ok(e) = e else {
                return Ok((Vec::new(), true, vec![dir.display().to_string()]));
            };
            let name = e.file_name().to_string_lossy().to_string();
            if chat_day_name(&name) {
                names.push(name);
            }
        }
        names.sort();
        let (mut budget, mut cut, mut read) = (window(), false, 0usize);
        let mut files: Vec<Vec<Message>> = Vec::new();
        for name in names.iter().rev() {
            if budget == 0 || read >= CHAT_FILES_MAX {
                cut = true;
                break;
            }
            let path = dir.join(name);
            let r = match read_chat_file(&path.to_string_lossy(), name, budget) {
                Ok(r) => r,
                Err(e) if e.not_found => continue,
                Err(_) => {
                    // Moved nowhere: an I/O error is no damage. The chat
                    // shows the rest, and the note says what it left out.
                    unread.push(path.display().to_string());
                    cut = true;
                    continue;
                }
            };
            if r.damaged {
                if self.move_chat_aside(&mut st, &path).is_err() {
                    // It could not move aside: leave it, and the chat shows
                    // the rest with a note.
                    unread.push(path.display().to_string());
                    cut = true;
                }
                continue;
            }
            read += 1;
            if r.used < r.size {
                cut = true;
            }
            budget = budget.saturating_sub(r.used);
            files.push(r.msgs);
        }
        let mut out = Vec::new();
        for f in files.into_iter().rev() {
            out.extend(f);
        }
        Ok((out, cut, unread))
    }

    /// The transcripts floor.chat reads for foreman id, each with its
    /// number among the pane's transcripts, oldest 0: a sprig pane's
    /// session tree, else every transcript the pane claimed, kept in the
    /// data dir across a restart, the newest CHAT_FOREMEN of them.
    fn chat_sources(&self, id: &str) -> Vec<(String, Kind, String)> {
        if let Some(p) = self.tree.pane(id) {
            if p.harness == "sprig" && p.kind == crate::layout::KIND_HEADLESS {
                let (path, kind) = self.transcript_of(&p);
                if path.is_empty() {
                    return Vec::new();
                }
                return vec![(path, kind, "0".into())];
            }
        }
        let claimed = self.claimed_by(id);
        let mut out = Vec::new();
        for k in (0..claimed.len()).rev() {
            if out.len() >= CHAT_FOREMEN {
                break;
            }
            out.insert(0, (claimed[k].clone(), Kind::Claude, k.to_string()));
        }
        out
    }

    fn handle_floor_chat(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let Ok((lines, mut cut, unread)) = self.read_chat() else {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!("cannot read the chat in {}.", self.chat_dir().display()),
            );
        };
        if !unread.is_empty() {
            self.note_once(&format!(
                "cannot read the chat file {}. The chat shows the rest.",
                unread.join(", ")
            ));
        }
        // The tracked foreman comes first even when no line names it yet,
        // as for one that floor.foreman or the terminal floor started.
        let mut foremen: Vec<String> = Vec::new();
        let tracked = {
            let mut t = self.talk_lock();
            self.tracked_foreman(&mut t)
        };
        if !tracked.is_empty() {
            foremen.push(tracked);
        }
        for m in lines.iter().rev() {
            if foremen.len() >= CHAT_FOREMEN {
                break;
            }
            if !m.pane.is_empty() && !foremen.contains(&m.pane) {
                foremen.push(m.pane.clone());
            }
        }
        let mut lines = lines;
        let mut replies: Vec<Message> = Vec::new();
        let mut heard: HashMap<String, Vec<Message>> = HashMap::new();
        for id in foremen.iter().rev() {
            for (path, kind, n) in self.chat_sources(id) {
                let Ok((msgs, tcut)) = read_messages(&path, kind) else {
                    cut = true;
                    self.note_once(&format!(
                        "cannot read the foreman's transcript {path}. The chat shows the rest."
                    ));
                    continue;
                };
                cut = cut || tcut;
                for mut m in msgs {
                    if m.role == "owner" && m.tool.is_empty() {
                        heard.entry(id.clone()).or_default().push(m.clone());
                    }
                    if m.role != "agent" {
                        continue;
                    }
                    m.id = format!("{id}/{n}/{}", m.id);
                    m.pane = id.clone();
                    replies.push(m);
                }
            }
        }
        mark_read(&mut lines, &mut heard);
        let mut all = lines;
        all.extend(replies);
        // A stable sort, as Go's sort.SliceStable.
        all.sort_by(|a, b| a.at.partial_cmp(&b.at).unwrap_or(std::cmp::Ordering::Equal));
        let (out, more) = pick(&all, &r.s("since"), limit_of(r), cut);
        reply(r, &out, more)
    }

    /// Starts the watch for transcript growth, once. It takes the first
    /// look before it returns.
    pub fn watch_messages(self: &Arc<Server>) {
        self.watch_once.call_once(|| {
            let mut marks = HashMap::new();
            self.look_messages(&mut marks, false);
            let s = self.clone();
            thread::spawn(move || loop {
                if s.talk_stop.wait(Some(MESSAGES_EVERY)) {
                    return;
                }
                s.look_messages(&mut marks, true);
            });
        });
    }

    /// Looks at every pane's transcript once and, with send, sends a
    /// messages event for each pane whose file changed in size or time, or
    /// that got or changed its file.
    fn look_messages(&self, marks: &mut HashMap<String, (String, u64, i64)>, send: bool) {
        let mut live = HashSet::new();
        for p in self.tree.panes() {
            live.insert(p.id.clone());
            let (path, _) = self.transcript_of(&p);
            let mut m = (path.clone(), 0u64, 0i64);
            if !path.is_empty() {
                if let Ok(md) = std::fs::symlink_metadata(&path) {
                    if md.file_type().is_file() {
                        m.1 = md.len();
                        m.2 = md.mtime() * 1_000_000_000 + md.mtime_nsec();
                    }
                }
            }
            let old = marks.insert(p.id.clone(), m.clone());
            let changed = match old {
                Some(o) => o != m,
                None => !path.is_empty(),
            };
            if send && changed {
                self.send_messages_event(&p.id, self.is_tracked_foreman(&p.id));
            }
        }
        marks.retain(|id, _| live.contains(id));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture(name: &str) -> String {
        format!(
            "{}/../coppice/testdata/transcripts/{name}",
            env!("CARGO_MANIFEST_DIR")
        )
    }

    fn scratch(name: &str) -> PathBuf {
        let d =
            std::env::temp_dir().join(format!("coppice-rs-messages-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn ids(msgs: &[Message]) -> Vec<(String, String, String, String)> {
        msgs.iter()
            .map(|m| (m.id.clone(), m.role.clone(), m.text.clone(), m.tool.clone()))
            .collect()
    }

    fn w(id: &str, role: &str, text: &str, tool: &str) -> (String, String, String, String) {
        (id.into(), role.into(), text.into(), tool.into())
    }

    #[test]
    fn a_claude_transcript_gives_owner_and_agent_messages_and_no_output() {
        let (msgs, cut) = read_messages(&fixture("claude-chat.jsonl"), Kind::Claude).unwrap();
        assert!(!cut);
        assert_eq!(
            ids(&msgs),
            vec![
                w("67", "owner", "fix the fake test, it fails on CI", ""),
                w(
                    "669",
                    "agent",
                    "The test reads a clock.\nI give it a fake one.[31m",
                    ""
                ),
                w("923", "agent", "Running the suite.", ""),
                w("923.1", "agent", "pytest tests/test_fake.py -q", "Bash"),
                w("2276", "owner", "now push it, please", ""),
                w("2494", "agent", "/fake/src/clock.py", "Read"),
                w("2494.1", "agent", "", "Glob"),
                w("2792", "agent", "A fake reply with no time.", ""),
            ]
        );
        assert_eq!(msgs[0].at, 4070908800.0);
        assert_eq!(msgs[5].at, 4070905210.0);
        assert_eq!(msgs[7].at, 0.0);
        assert!(!format!("{msgs:?}").contains("FAKE-"));
    }

    #[test]
    fn a_sprig_tree_gives_messages() {
        let (msgs, _) = read_messages(&fixture("sprig-chat.jsonl"), Kind::Sprig).unwrap();
        assert_eq!(
            ids(&msgs),
            vec![
                w("0", "owner", "a fake task", ""),
                w("82", "agent", "Looking at it.", ""),
                w("295", "agent", "ls -la", "bash"),
                w("702", "agent", "fetch the fake page", "web"),
                w("957", "agent", "Done.", ""),
            ]
        );
        assert_eq!(msgs[0].at, 1.5);
    }

    #[test]
    fn since_and_limit_pick_the_newest() {
        let (msgs, _) = read_messages(&fixture("claude-chat.jsonl"), Kind::Claude).unwrap();
        let (out, more) = pick(&msgs, "923.1", 100, false);
        assert_eq!(out.len(), 4);
        assert!(!more);
        let (out, more) = pick(&msgs, "", 2, false);
        assert_eq!(out[0].id, "2494.1");
        assert!(more);
        let (out, more) = pick(&msgs, "no-such-id", 100, true);
        assert_eq!(out.len(), 8);
        assert!(more);
        let (out, more) = pick(&msgs, "2792", 100, true);
        assert!(out.is_empty() && !more);
    }

    #[test]
    fn the_read_takes_only_the_tail() {
        let (msgs, cut) =
            read_messages_in(&fixture("claude-chat.jsonl"), Kind::Claude, 300).unwrap();
        assert!(cut);
        assert_eq!(
            ids(&msgs),
            vec![w("2792", "agent", "A fake reply with no time.", "")]
        );
    }

    #[test]
    fn text_is_cleaned_and_capped() {
        let long = "é".repeat(2100);
        assert_eq!(clean_message(&long), format!("{}…", "é".repeat(2000)));
        assert_eq!(
            clean_message("\x1b]0;x\x07a\tb\r\n\x00c\u{85} "),
            "]0;xa b\nc"
        );
        let input = json!({"command": "x".repeat(300)});
        assert_eq!(
            tool_summary(Some(&input), ""),
            format!("{}…", "x".repeat(200))
        );
        let input = json!({"command": " ", "path": "a\n\tb"});
        assert_eq!(tool_summary(Some(&input), "d"), "a b");
        assert_eq!(tool_summary(None, "do\nit"), "do it");
        let (msgs, cut) = read_messages("/nonexistent/none.jsonl", Kind::Claude).unwrap();
        assert!(msgs.is_empty() && !cut);
    }

    #[test]
    fn rfc3339_reads_as_go_does() {
        assert_eq!(rfc3339_seconds("2026-01-01T00:00:00Z"), 1767225600.0);
        assert_eq!(rfc3339_seconds("2026-01-01T00:00:01.500Z"), 1767225601.5);
        assert_eq!(rfc3339_seconds("2026-01-01T00:00:10+01:00"), 1767222010.0);
        assert_eq!(rfc3339_seconds("2026-02-30T00:00:00Z"), 0.0);
        assert_eq!(rfc3339_seconds("not a time"), 0.0);
    }

    #[test]
    fn chat_day_names() {
        assert!(chat_day_name("2026-10-09.jsonl"));
        assert!(!chat_day_name("2026-10-09.jsonl.broken-20261009T000000Z"));
        assert!(!chat_day_name("x026-10-09.jsonl"));
    }

    fn server(dir: &Path) -> Arc<Server> {
        Server::new(super::super::Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.to_path_buf(),
            start_dir: dir.to_path_buf(),
            voice: Default::default(),
        })
    }

    #[test]
    fn a_chat_line_is_written_0600_in_a_0700_dir() {
        let dir = scratch("write");
        let s = server(&dir);
        s.write_chat("user", "w1:p1", "hello <foreman>");
        let (lines, cut, _) = s.read_chat().unwrap();
        assert!(!cut);
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0].role, "owner");
        assert_eq!(lines[0].text, "hello <foreman>");
        assert_eq!(lines[0].pane, "w1:p1");
        let chat = dir.join("chat");
        assert_eq!(std::fs::metadata(&chat).unwrap().mode() & 0o777, 0o700);
        let f = std::fs::read_dir(&chat)
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .path();
        assert_eq!(std::fs::metadata(&f).unwrap().mode() & 0o777, 0o600);
        let text = std::fs::read_to_string(&f).unwrap();
        // Go escapes < and > in a JSON string; so must the line here.
        let want = format!("\"text\":\"hello {b}u003cforeman{b}u003e\"", b = '\\');
        assert!(text.contains(&want), "{text}");
        assert!(text.starts_with(r#"{"kind":"user","date":""#), "{text}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_damaged_chat_file_moves_aside_and_a_half_line_is_cut() {
        let dir = scratch("damage");
        let s = server(&dir);
        let chat = dir.join("chat");
        std::fs::create_dir_all(&chat).unwrap();
        std::fs::write(
            chat.join("2026-01-01.jsonl"),
            "{\"kind\":\"user\",\"date\":\"2026-01-01T00:00:00.000Z\",\"pane\":\"w1:p1\",\"text\":\"kept\"}\n{\"kind\":\"us",
        )
        .unwrap();
        std::fs::write(
            chat.join("2026-01-02.jsonl"),
            "{\"kind\":\"user\",\"date\":\"2026-01-02T00:00:00.000Z\",\"pane\":\"w1:p1\",\"text\":\"lost\"}\nnot a chat line\n",
        )
        .unwrap();
        let (lines, _, _) = s.read_chat().unwrap();
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0].text, "kept");
        assert_eq!(lines[0].id, "chat/2026-01-01/0");
        assert!(!chat.join("2026-01-02.jsonl").exists());
        let moved: Vec<_> = std::fs::read_dir(&chat)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().to_string())
            .filter(|n| n.starts_with("2026-01-02.jsonl.broken-"))
            .collect();
        assert_eq!(moved.len(), 1, "{moved:?}");
        // Before its first write the writer moves the half line out: no
        // byte is cut away.
        let mut st = ChatState::default();
        s.check_chat_file(&mut st, &chat.join("2026-01-01.jsonl"))
            .unwrap();
        let text = std::fs::read_to_string(chat.join("2026-01-01.jsonl")).unwrap();
        assert!(text.ends_with("\"kept\"}\n"), "{text}");
        let tails: Vec<_> = std::fs::read_dir(&chat)
            .unwrap()
            .map(|e| e.unwrap().path())
            .filter(|p| p.to_string_lossy().contains("2026-01-01.jsonl.broken-"))
            .collect();
        assert_eq!(tails.len(), 1, "{tails:?}");
        assert_eq!(
            std::fs::read_to_string(&tails[0]).unwrap(),
            "{\"kind\":\"us"
        );
        assert_eq!(std::fs::metadata(&tails[0]).unwrap().mode() & 0o777, 0o600);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A chat file that cannot be read moves nowhere: the rest of the chat
    /// reads, and the read says a part was left out.
    #[test]
    fn a_damaged_file_that_cannot_move_leaves_the_rest_readable() {
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        let dir = scratch("nomove");
        let s = server(&dir);
        let chat = dir.join("chat");
        std::fs::create_dir_all(&chat).unwrap();
        std::fs::write(
            chat.join("2026-01-01.jsonl"),
            "{\"kind\":\"user\",\"date\":\"2026-01-01T00:00:00.000Z\",\"pane\":\"w1:p1\",\"text\":\"kept\"}\n",
        )
        .unwrap();
        let bad = chat.join("2026-01-02.jsonl");
        std::fs::write(&bad, "not a chat line\n").unwrap();
        std::fs::set_permissions(&chat, std::fs::Permissions::from_mode(0o500)).unwrap();
        let got = s.read_chat();
        std::fs::set_permissions(&chat, std::fs::Permissions::from_mode(0o700)).unwrap();
        let (lines, cut, unread) = got.unwrap();
        assert_eq!(lines.len(), 1);
        assert!(cut);
        assert_eq!(unread, vec![bad.display().to_string()]);
        assert!(bad.exists());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn an_io_error_moves_nothing() {
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        let dir = scratch("ioerr");
        let s = server(&dir);
        let chat = dir.join("chat");
        std::fs::create_dir_all(&chat).unwrap();
        let bad = chat.join("2026-01-01.jsonl");
        std::fs::write(&bad, "{\"kind\":\"user\",\"date\":\"2026-01-01T00:00:00.000Z\",\"pane\":\"w1:p1\",\"text\":\"x\"}\n").unwrap();
        std::fs::set_permissions(&bad, std::fs::Permissions::from_mode(0)).unwrap();
        std::fs::write(
            chat.join("2026-01-02.jsonl"),
            "{\"kind\":\"user\",\"date\":\"2026-01-02T00:00:00.000Z\",\"pane\":\"w1:p1\",\"text\":\"kept\"}\n",
        )
        .unwrap();
        let (lines, cut, unread) = s.read_chat().unwrap();
        assert_eq!(lines.len(), 1);
        assert!(cut);
        assert_eq!(unread, vec![bad.display().to_string()]);
        assert!(std::fs::symlink_metadata(&bad).is_ok(), "the file moved");
        let moved = std::fs::read_dir(&chat)
            .unwrap()
            .filter(|e| {
                e.as_ref()
                    .unwrap()
                    .file_name()
                    .to_string_lossy()
                    .contains(".broken-")
            })
            .count();
        assert_eq!(moved, 0);
        let _ = std::fs::set_permissions(&bad, std::fs::Permissions::from_mode(0o600));
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Format characters and the line and paragraph separators go.
    #[test]
    fn format_characters_are_dropped() {
        assert_eq!(
            clean_message("a\u{202e}b\u{200b}c\u{2028}d\u{2029}e\u{feff}f"),
            "abcdef"
        );
    }

    /// A claim outlives the server, in a 0600 file in the data dir, and no
    /// other pane takes it.
    #[test]
    fn a_claim_outlives_the_server() {
        let dir = scratch("claims");
        let s = server(&dir);
        assert_eq!(s.claim_for_tests("w1:p1", "/x/projects/a.jsonl"), None);
        assert_eq!(s.claim_for_tests("w1:p1", "/x/projects/a.jsonl"), None);
        let file = dir.join(super::super::facts::CLAIMS_FILE);
        assert_eq!(std::fs::metadata(&file).unwrap().mode() & 0o777, 0o600);
        let s2 = server(&dir);
        assert_eq!(
            s2.claimed_by("w1:p1"),
            vec!["/x/projects/a.jsonl".to_string()]
        );
        assert_eq!(
            s2.claim_for_tests("w1:p2", "/x/projects/a.jsonl"),
            Some(super::super::facts::TRANSCRIPT_TAKEN_REFUSAL)
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
