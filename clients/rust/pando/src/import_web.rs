//! The claude.ai web export, --map, and the proposal file. The twin of Go's
//! pando import_web.go.

use std::collections::HashMap;
use std::io::BufRead;
use std::path::Path;

use serde_json::Value;

use super::fsutil;
use super::import::{
    new_msgs, parse_import_time, plural, ImportMsg, ImportOpts, ImportSession, SessionRead, SRC_WEB,
};
use super::store::{valid_store, DEFAULT_STORE, ERR_STORE_ALL};

/// One --map OLD=NEW word (PD-R-9).
#[derive(Clone, Debug, PartialEq)]
pub struct PathMap {
    pub old: String,
    pub new: String,
}

/// Reads "OLD=NEW".
pub fn parse_map(s: &str) -> Result<PathMap, String> {
    match s.find('=') {
        Some(k) if k > 0 && k < s.len() - 1 => Ok(PathMap {
            old: crate::sys::clean(&s[..k]),
            new: crate::sys::clean(&s[k + 1..]),
        }),
        _ => Err(format!("--map needs OLD=NEW, not {}", crate::sys::go_quote(s))),
    }
}

/// Reads the OLD=NEW words the state saved. A bad word is left out.
pub fn parse_maps(words: &[String]) -> Vec<PathMap> {
    words.iter().filter_map(|w| parse_map(w).ok()).collect()
}

impl ImportOpts {
    /// Rewrites the front of a project path by the first map whose old is
    /// the path or a parent of it by path element. Other paths stay.
    pub fn project(&self, p: &str) -> String {
        if p.is_empty() {
            return String::new();
        }
        let cp = crate::sys::clean(p);
        for m in &self.map {
            if cp == m.old {
                return m.new.clone();
            }
            let prefix = format!("{}/", m.old.trim_end_matches('/'));
            if cp.starts_with(&prefix) {
                return crate::sys::clean(&format!("{}/{}", m.new, &cp[m.old.len()..]));
            }
        }
        p.to_string()
    }
}

struct WebMessage {
    uuid: String,
    sender: String,
    text: String,
    created: String,
}

struct WebConversation {
    uuid: String,
    name: String,
    messages: Vec<WebMessage>,
}

fn s_of(v: &Value, k: &str) -> String {
    v.get(k).and_then(|x| x.as_str()).unwrap_or("").to_string()
}

fn message_text(m: &Value) -> String {
    let t = s_of(m, "text");
    if !t.trim().is_empty() {
        return t;
    }
    let mut parts = Vec::new();
    if let Some(cs) = m.get("content").and_then(|c| c.as_array()) {
        for c in cs {
            if s_of(c, "type") == "text" {
                let t = s_of(c, "text");
                if !t.is_empty() {
                    parts.push(t);
                }
            }
        }
    }
    parts.join("\n\n")
}

fn u16le(b: &[u8], at: usize) -> Option<usize> {
    Some(u16::from_le_bytes(b.get(at..at + 2)?.try_into().ok()?) as usize)
}

fn u32le(b: &[u8], at: usize) -> Option<usize> {
    Some(u32::from_le_bytes(b.get(at..at + 4)?.try_into().ok()?) as usize)
}

/// The bytes of conversations.json in a zip.
fn zip_conversations(data: &[u8]) -> Result<Vec<u8>, String> {
    let bad = || "zip: not a valid zip file".to_string();
    let start = data.len().saturating_sub(22 + 65535);
    let eocd = (start..data.len().saturating_sub(21))
        .rev()
        .find(|&i| data[i..].starts_with(&[0x50, 0x4b, 0x05, 0x06]))
        .ok_or_else(bad)?;
    let n = u16le(data, eocd + 10).ok_or_else(bad)?;
    let mut at = u32le(data, eocd + 16).ok_or_else(bad)?;
    for _ in 0..n {
        if !data.get(at..).is_some_and(|d| d.starts_with(&[0x50, 0x4b, 0x01, 0x02])) {
            return Err(bad());
        }
        let method = u16le(data, at + 10).ok_or_else(bad)?;
        let csize = u32le(data, at + 20).ok_or_else(bad)?;
        let usize_ = u32le(data, at + 24).ok_or_else(bad)?;
        let nl = u16le(data, at + 28).ok_or_else(bad)?;
        let el = u16le(data, at + 30).ok_or_else(bad)?;
        let cl = u16le(data, at + 32).ok_or_else(bad)?;
        let lh = u32le(data, at + 42).ok_or_else(bad)?;
        let name = String::from_utf8_lossy(data.get(at + 46..at + 46 + nl).ok_or_else(bad)?).into_owned();
        at += 46 + nl + el + cl;
        if name != "conversations.json" && !name.ends_with("/conversations.json") {
            continue;
        }
        if !data.get(lh..).is_some_and(|d| d.starts_with(&[0x50, 0x4b, 0x03, 0x04])) {
            return Err(bad());
        }
        let ln = u16le(data, lh + 26).ok_or_else(bad)?;
        let le = u16le(data, lh + 28).ok_or_else(bad)?;
        let body = data.get(lh + 30 + ln + le..lh + 30 + ln + le + csize).ok_or_else(bad)?;
        return match method {
            0 => Ok(body.to_vec()),
            8 => miniz_oxide::inflate::decompress_to_vec_with_limit(body, usize_.max(1) + (1 << 20))
                .map_err(|e| format!("zip: {e:?}")),
            m => Err(format!("zip: unsupported compression algorithm {m}")),
        };
    }
    Err("the export holds no conversations.json".into())
}

/// Reads conversations.json from a zip or from the file itself.
fn read_web_file(path: &Path) -> Result<Vec<WebConversation>, String> {
    let raw = std::fs::read(path).map_err(|e| fsutil::path_err("open", path, &e))?;
    let json = if path.to_string_lossy().to_lowercase().ends_with(".zip") {
        zip_conversations(&raw)?
    } else {
        raw
    };
    let v: Value = serde_json::from_slice(&json)
        .map_err(|e| format!("conversations.json is not a list of chats: {e}"))?;
    let list = v
        .as_array()
        .ok_or("conversations.json is not a list of chats: json: cannot unmarshal object into Go value of type []pando.webConversation")?;
    let mut out = Vec::new();
    for c in list {
        let messages = c
            .get("chat_messages")
            .and_then(|m| m.as_array())
            .map(|ms| {
                ms.iter()
                    .map(|m| WebMessage {
                        uuid: s_of(m, "uuid"),
                        sender: s_of(m, "sender"),
                        text: message_text(m),
                        created: s_of(m, "created_at"),
                    })
                    .collect()
            })
            .unwrap_or_default();
        out.push(WebConversation {
            uuid: s_of(c, "uuid"),
            name: s_of(c, "name"),
            messages,
        });
    }
    Ok(out)
}

/// The queue key of a chat: its conversation uuid.
pub fn web_key(uuid: &str) -> String {
    format!("{SRC_WEB}:{uuid}")
}

fn web_messages(c: &WebConversation, o: &ImportOpts) -> SessionRead {
    let key = web_key(&c.uuid);
    let mut r = SessionRead {
        key: key.clone(),
        msgs: Vec::new(),
        cwd: String::new(),
    };
    let where_ = if c.name.trim().is_empty() {
        String::new()
    } else {
        format!(" in {}", c.name.split_whitespace().collect::<Vec<_>>().join(" "))
    };
    let mut seen: HashMap<String, usize> = HashMap::new();
    for m in &c.messages {
        let role = if m.sender == "human" { "user" } else { m.sender.as_str() };
        if m.text.trim().is_empty() {
            continue;
        }
        let Some(ts) = parse_import_time(&m.created) else {
            continue;
        };
        if o.since.is_some_and(|s| ts < s) {
            continue;
        }
        let mut act = format!("{key}:{}", m.uuid);
        let k = *seen.get(&act).unwrap_or(&0);
        *seen.entry(act.clone()).or_insert(0) += 1;
        if k > 0 {
            act.push_str(&format!(".{k}"));
        }
        r.msgs.push(ImportMsg {
            text: format!("web {role}{where_}: {}", m.text),
            ts,
            act,
        });
    }
    r
}

/// Reads one chat of an export by its key.
pub(crate) fn read_web_session(path: &Path, key: &str, o: &ImportOpts) -> Result<SessionRead, String> {
    let cs = read_web_file(path)?;
    for c in &cs {
        if web_key(&c.uuid) == key {
            return Ok(web_messages(c, o));
        }
    }
    Err(format!("chat {key} is not in {}", path.display()))
}

/// Lists the chats of a claude.ai export, oldest first: one session per
/// chat, keyed by its conversation uuid; a message is keyed by that and its
/// own uuid, so a message uuid that two chats share is logged once in each
/// (PD-R-8). new counts the messages the log does not hold. A chat with none
/// is still listed, so the proposal can say why it is skipped. has says
/// whether an act id is in the store already; None means none is.
pub fn scan_web(
    path: &Path,
    o: &ImportOpts,
    has: Option<&dyn Fn(&str) -> bool>,
) -> Result<Vec<ImportSession>, String> {
    let cs = read_web_file(path)?;
    let mut out = Vec::new();
    for c in &cs {
        let r = web_messages(c, o);
        let fresh = new_msgs(&r.msgs, None, has);
        let first = if !fresh.is_empty() {
            fresh[0].ts
        } else if !r.msgs.is_empty() {
            r.msgs[0].ts
        } else {
            0
        };
        out.push(ImportSession {
            key: r.key.clone(),
            src: SRC_WEB.into(),
            path: path.to_path_buf(),
            new: fresh.len(),
            first,
            title: c.name.clone(),
            total: r.msgs.len(),
        });
    }
    out.sort_by(|a, b| a.first.cmp(&b.first).then_with(|| a.key.cmp(&b.key)));
    Ok(out)
}

/// Writes one line per chat: key, proposed store, reason and title, split
/// by tabs. Nothing is written to any store (PD-R-7).
pub fn write_proposal(w: &mut dyn std::io::Write, ss: &[ImportSession]) -> std::io::Result<()> {
    for s in ss {
        let (store, reason) = if s.total == 0 {
            ("skip".to_string(), "the chat is empty".to_string())
        } else if s.new == 0 {
            ("skip".to_string(), "every message is in the log already".to_string())
        } else {
            (
                DEFAULT_STORE.to_string(),
                format!("{} to import", plural(s.new as i64, "message")),
            )
        };
        let title = s.title.split_whitespace().collect::<Vec<_>>().join(" ");
        writeln!(w, "{}\t{}\t{}\t{}", s.key, store, reason, title)?;
    }
    Ok(())
}

/// One accepted line.
#[derive(Clone, Debug, PartialEq)]
pub struct ProposalChoice {
    pub key: String,
    pub store: String,
}

/// Reads a proposal file the owner has checked. A store of "skip" is kept
/// as is; "all" and a bad name are refused (PD-R-2).
pub fn read_proposal(r: &mut dyn BufRead) -> Result<Vec<ProposalChoice>, String> {
    let mut out = Vec::new();
    for (k, line) in r.lines().enumerate() {
        let n = k + 1;
        let line = line.map_err(|e| e.to_string())?;
        let line = line.trim_end_matches('\r');
        if line.trim().is_empty() || line.starts_with('#') {
            continue;
        }
        let f: Vec<&str> = line.split('\t').collect();
        if f.len() < 2 {
            return Err(format!("line {n}: need a key and a store, split by a tab"));
        }
        let st = f[1].trim();
        if st == "all" {
            return Err(format!("line {n}: {ERR_STORE_ALL}"));
        }
        if st != "skip" && !valid_store(st) {
            return Err(format!("line {n}: bad store name {}", crate::sys::go_quote(st)));
        }
        out.push(ProposalChoice {
            key: f[0].to_string(),
            store: st.to_string(),
        });
    }
    Ok(out)
}
