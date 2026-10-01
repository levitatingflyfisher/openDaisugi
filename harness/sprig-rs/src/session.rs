//! The session tree: one append-only JSONL file per session, an id and a
//! parentId on every entry, the head as the last write wins. The rows are
//! the shape daisugi's gate writes on its own path.

use crate::json::{encode, jmap, jmap_any, Any, Map, J};
use std::cell::RefCell;
use std::os::unix::ffi::OsStrExt;
use std::time::Duration;

/// The four token buckets: fresh input, cache read, cache write, output.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Usage {
    pub fresh: i64,
    pub cache_read: i64,
    pub cache_write: i64,
    pub out: i64,
}

/// Receives every event the session tree records, in order. A write
/// failure is swallowed: the tree never stops the loop.
pub trait SessionObserver {
    fn on_prompt(&self, text: &[u8]);
    /// `uses` are the (id, name) of each tool call the turn asked for.
    fn on_assistant(&self, model: &str, text: &[u8], usage: &Usage, uses: &[(String, String)]);
    fn on_tool_call(&self, id: &str, name: &str, input: &Option<Map>);
    fn on_verdict(&self, id: &str, allow: bool, reason: &[u8], latency: Duration);
    fn on_tool_result(&self, id: &str, ok: bool, summary: &[u8]);
}

/// bufio.Scanner's largest token here: a line this long is an error.
const MAX_LINE: usize = 1 << 24;

/// One parsed row: the structural fields, and the whole row as data.
#[derive(Debug, Clone, PartialEq)]
pub struct Entry {
    pub typ: String,
    pub id: String,
    /// "" for a root entry.
    pub parent: String,
    pub ts: f64,
    pub data: Map,
}

/// Appends to one session's file and keeps the head in memory.
pub struct SessionWriter {
    path: Vec<u8>,
    head: RefCell<String>,
}

/// An 8-hex id from 4 random bytes.
pub fn new_id() -> String {
    crate::osx::random_hex(4)
}

const SUMMARY_LIMIT: usize = 400;

impl SessionWriter {
    /// Makes a fresh session file and writes its header; refuses a file
    /// that exists.
    pub fn create(dir: &[u8], session_id: &[u8], cwd: &[u8]) -> Result<Self, Vec<u8>> {
        crate::osx::mkdir_all(dir, 0o700)?;
        let path = crate::osx::join(dir, &[session_id, b".jsonl"].concat());
        if std::fs::metadata(std::ffi::OsStr::from_bytes(&path)).is_ok() {
            return Err([b"session file exists: ".as_slice(), &path].concat());
        }
        let w = SessionWriter {
            path,
            head: RefCell::new(String::new()),
        };
        let header = jmap(vec![
            ("type", J::s("session")),
            ("v", J::Int(1)),
            ("id", J::s(session_id)),
            ("harness", J::s("sprig")),
            ("cwd", J::s(cwd)),
            ("harnessSessionId", J::Null),
            ("transcriptPath", J::Null),
            ("parentSession", J::Null),
            ("parentEntry", J::Null),
            ("cacheKey", J::Null),
            ("ts", J::Float(crate::osx::now())),
        ]);
        w.write_raw(&header)?;
        let _ = std::fs::set_permissions(
            std::ffi::OsStr::from_bytes(&w.path),
            std::os::unix::fs::PermissionsExt::from_mode(0o600),
        );
        Ok(w)
    }

    /// Reopens a session file, the head restored from it.
    pub fn open(dir: &[u8], session_id: &[u8]) -> Result<Self, Vec<u8>> {
        let path = crate::osx::join(dir, &[session_id, b".jsonl"].concat());
        crate::osx::stat(&path)?;
        let entries = read_entries(&path)?;
        let head = head_of(&entries);
        Ok(SessionWriter {
            path,
            head: RefCell::new(head),
        })
    }

    pub fn path(&self) -> &[u8] {
        &self.path
    }

    pub fn head(&self) -> String {
        self.head.borrow().clone()
    }

    fn write_raw(&self, row: &J) -> Result<(), Vec<u8>> {
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        let p = std::ffi::OsStr::from_bytes(&self.path);
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .create(true)
            .mode(0o600)
            .open(p)
            .map_err(|e| crate::goerr::path_error("open", &self.path, &e))?;
        let mut b = encode(row);
        b.push(b'\n');
        f.write_all(&b)
            .map_err(|e| crate::goerr::path_error("write", &self.path, &e))
    }

    /// Writes one entry off the current head and moves the head to it.
    fn append(&self, typ: &str, data: Vec<(&str, J)>) {
        let id = new_id();
        let head = self.head();
        let mut row = vec![
            ("type", J::s(typ)),
            ("id", J::s(&id)),
            ("ts", J::Float(crate::osx::now())),
            (
                "parentId",
                if head.is_empty() {
                    J::Null
                } else {
                    J::s(&head)
                },
            ),
        ];
        row.extend(data);
        if self.write_raw(&jmap(row)).is_ok() {
            *self.head.borrow_mut() = id;
        }
    }
}

impl SessionObserver for SessionWriter {
    fn on_prompt(&self, text: &[u8]) {
        self.append("prompt", vec![("text", J::s(text))]);
    }

    fn on_assistant(&self, model: &str, text: &[u8], u: &Usage, uses: &[(String, String)]) {
        let tu = uses
            .iter()
            .map(|(id, name)| jmap(vec![("id", J::s(id)), ("name", J::s(name))]))
            .collect();
        self.append(
            "assistant",
            vec![
                ("model", J::s(model)),
                ("text", J::s(text)),
                (
                    "usage",
                    jmap(vec![
                        ("fresh", J::Int(u.fresh)),
                        ("cacheRead", J::Int(u.cache_read)),
                        ("cacheWrite", J::Int(u.cache_write)),
                        ("out", J::Int(u.out)),
                    ]),
                ),
                ("toolUses", J::Arr(tu)),
            ],
        );
    }

    fn on_tool_call(&self, id: &str, name: &str, input: &Option<Map>) {
        self.append(
            "tool_call",
            vec![
                ("toolUseId", J::s(id)),
                ("name", J::s(name)),
                ("input", jmap_any(input)),
                ("detail", J::Str(detail_of(name, input))),
                ("agentId", J::Null),
                ("agentType", J::Null),
            ],
        );
    }

    fn on_verdict(&self, id: &str, allow: bool, reason: &[u8], latency: Duration) {
        let decision = if allow { "allow" } else { "deny" };
        self.append(
            "verdict",
            vec![
                ("toolUseId", J::s(id)),
                ("decision", J::s(decision)),
                ("mode", J::s("enforce")),
                ("reason", J::s(reason)),
                ("clause", J::s(reason)),
                ("counterexample", J::Obj(Vec::new())),
                ("envelopeId", J::Null),
                ("planId", J::Null),
                (
                    "latencyMs",
                    J::Float(latency.as_micros() as i64 as f64 / 1000.0),
                ),
                ("answeredBy", J::Null),
            ],
        );
    }

    fn on_tool_result(&self, id: &str, ok: bool, summary: &[u8]) {
        self.append(
            "tool_result",
            vec![
                ("toolUseId", J::s(id)),
                ("ok", J::Bool(ok)),
                (
                    "summary",
                    J::Str(crate::gostr::truncate_runes(summary, SUMMARY_LIMIT)),
                ),
            ],
        );
    }
}

/// The one input field worth showing at a glance.
pub fn detail_of(name: &str, input: &Option<Map>) -> Vec<u8> {
    for k in ["command", "cmd", "path", "file_path", "url"] {
        if let Some(Any::Str(v)) = input.as_ref().and_then(|m| m.get(k)) {
            return v.clone().into_bytes();
        }
    }
    name.as_bytes().to_vec()
}

/// Parses the file, skipping blank and torn lines.
pub fn read_entries(path: &[u8]) -> Result<Vec<Entry>, Vec<u8>> {
    let all = match std::fs::File::open(std::ffi::OsStr::from_bytes(path)) {
        Ok(mut f) => {
            use std::io::Read;
            let mut b = Vec::new();
            f.read_to_end(&mut b)
                .map_err(|e| crate::goerr::path_error("read", path, &e))?;
            b
        }
        Err(e) => return Err(crate::goerr::path_error("open", path, &e)),
    };
    let mut out = Vec::new();
    let mut rest: &[u8] = &all;
    while !rest.is_empty() {
        let (mut line, next) = match rest.iter().position(|&c| c == b'\n') {
            Some(i) => (&rest[..i], &rest[i + 1..]),
            None => (rest, &rest[rest.len()..]),
        };
        if line.len() >= MAX_LINE {
            return Err(b"bufio.Scanner: token too long".to_vec());
        }
        rest = next;
        if line.last() == Some(&b'\r') {
            line = &line[..line.len() - 1];
        }
        if line.is_empty() {
            continue;
        }
        let mut raw: Option<Map> = None;
        if crate::json::unmarshal(line, |d, n| d.map_any(n, &mut raw)).is_err() {
            continue;
        }
        let Some(raw) = raw else {
            continue;
        };
        let s = |k: &str| match raw.get(k) {
            Some(Any::Str(x)) => x.clone(),
            _ => String::new(),
        };
        let typ = s("type");
        if typ.is_empty() {
            continue;
        }
        let ts = match raw.get("ts") {
            Some(Any::Num(f)) => *f,
            _ => 0.0,
        };
        out.push(Entry {
            id: s("id"),
            parent: s("parentId"),
            typ,
            ts,
            data: raw,
        });
    }
    Ok(out)
}

/// The current leaf: the last head's leafId or the last other entry's id.
pub fn head_of(entries: &[Entry]) -> String {
    let mut head = String::new();
    for e in entries {
        match e.typ.as_str() {
            "head" => {
                head = match e.data.get("leafId") {
                    Some(Any::Str(x)) => x.clone(),
                    _ => String::new(),
                }
            }
            "label" | "session" => {}
            _ => {
                if !e.id.is_empty() {
                    head = e.id.clone();
                }
            }
        }
    }
    head
}

/// Root to head.
pub fn head_path(path: &[u8]) -> Result<Vec<Entry>, Vec<u8>> {
    let entries = read_entries(path)?;
    let mut by_id: std::collections::HashMap<&str, &Entry> = std::collections::HashMap::new();
    for e in &entries {
        if !e.id.is_empty() && e.typ != "session" {
            by_id.insert(&e.id, e);
        }
    }
    let mut out: Vec<Entry> = Vec::new();
    let mut cur = head_of(&entries);
    while !cur.is_empty() {
        let Some(e) = by_id.get(cur.as_str()) else {
            break;
        };
        out.insert(0, (*e).clone());
        cur = e.parent.clone();
    }
    Ok(out)
}

/// The model-facing history a resume rebuilds from the head path.
pub fn history_from_entries(entries: &[Entry]) -> Vec<crate::agent::Message> {
    use crate::agent::Message;
    use crate::gate::ToolCall;
    let str_at = |m: &Map, k: &str| match m.get(k) {
        Some(Any::Str(x)) => x.clone(),
        _ => String::new(),
    };
    let mut tool_input: std::collections::HashMap<String, Option<Map>> =
        std::collections::HashMap::new();
    for e in entries {
        if e.typ != "tool_call" {
            continue;
        }
        let id = str_at(&e.data, "toolUseId");
        if id.is_empty() {
            continue;
        }
        let input = match e.data.get("input") {
            Some(Any::Map(m)) => Some(m.clone()),
            _ => None,
        };
        tool_input.insert(id, input);
    }
    let mut out = Vec::new();
    for e in entries {
        match e.typ.as_str() {
            "prompt" => out.push(Message {
                role: "user",
                text: str_at(&e.data, "text").into_bytes(),
                ..Default::default()
            }),
            "assistant" => {
                let mut msg = Message {
                    role: "assistant",
                    text: str_at(&e.data, "text").into_bytes(),
                    model: str_at(&e.data, "model"),
                    ..Default::default()
                };
                if let Some(Any::Arr(uses)) = e.data.get("toolUses") {
                    for u in uses {
                        let Any::Map(m) = u else {
                            continue;
                        };
                        let id = str_at(m, "id");
                        let input = tool_input.get(&id).cloned().flatten();
                        msg.calls.push(ToolCall {
                            name: str_at(m, "name"),
                            id,
                            input,
                        });
                    }
                }
                out.push(msg);
            }
            "tool_result" => out.push(Message {
                role: "tool",
                text: str_at(&e.data, "summary").into_bytes(),
                ..Default::default()
            }),
            _ => {}
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::ffi::OsStrExt;

    fn scratch(name: &str) -> Vec<u8> {
        let d = std::env::temp_dir().join(format!("sprig-sess-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        d.as_os_str().as_bytes().to_vec()
    }

    fn lines(path: &[u8]) -> Vec<String> {
        let s = std::fs::read_to_string(std::ffi::OsStr::from_bytes(path)).unwrap();
        s.lines().map(|l| l.to_string()).collect()
    }

    fn masked(l: &str) -> String {
        let mut out = String::new();
        let mut rest = l;
        while let Some(i) = rest.find("\"ts\":") {
            out.push_str(&rest[..i + 5]);
            rest = &rest[i + 5..];
            let end = rest.find([',', '}']).unwrap();
            out.push('T');
            rest = &rest[end..];
        }
        out.push_str(rest);
        out
    }

    #[test]
    fn writes_the_tree() {
        let dir = scratch("tree");
        let w = SessionWriter::create(&dir, b"s1", b"/w").unwrap();
        assert!(SessionWriter::create(&dir, b"s1", b"/w").is_err());
        w.on_prompt(b"task <1>");
        let p = w.head();
        let mut m = Map::new();
        m.insert("path".into(), Any::Str("f".into()));
        let input = Some(m);
        w.on_assistant(
            "haiku",
            b"",
            &Usage {
                fresh: 1,
                cache_read: 2,
                cache_write: 3,
                out: 4,
            },
            &[("t1".into(), "read".into())],
        );
        let a = w.head();
        w.on_tool_call("t1", "read", &input);
        w.on_verdict("t1", false, b"no\xff", Duration::from_micros(1500));
        w.on_tool_result("t1", false, &b"x".repeat(500));
        let l = lines(w.path());
        assert_eq!(l.len(), 6);
        assert_eq!(
            masked(&l[0]),
            "{\"cacheKey\":null,\"cwd\":\"/w\",\"harness\":\"sprig\",\"harnessSessionId\":null,\"id\":\"s1\",\
             \"parentEntry\":null,\"parentSession\":null,\"transcriptPath\":null,\"ts\":T,\"type\":\"session\",\"v\":1}"
        );
        assert_eq!(masked(&l[1]), format!("{{\"id\":\"{p}\",\"parentId\":null,\"text\":\"task \\u003c1\\u003e\",\"ts\":T,\"type\":\"prompt\"}}"));
        assert_eq!(
            masked(&l[2]),
            format!(
                "{{\"id\":\"{a}\",\"model\":\"haiku\",\"parentId\":\"{p}\",\"text\":\"\",\"toolUses\":[{{\"id\":\"t1\",\"name\":\"read\"}}],\
                 \"ts\":T,\"type\":\"assistant\",\"usage\":{{\"cacheRead\":2,\"cacheWrite\":3,\"fresh\":1,\"out\":4}}}}"
            )
        );
        assert!(l[3].contains("\"agentId\":null,\"agentType\":null,\"detail\":\"f\""));
        assert!(l[3].contains("\"input\":{\"path\":\"f\"},\"name\":\"read\""));
        assert!(l[4].contains("\"answeredBy\":null,\"clause\":\"no\\ufffd\",\"counterexample\":{},\"decision\":\"deny\",\"envelopeId\":null"));
        assert!(l[4].contains("\"latencyMs\":1.5,\"mode\":\"enforce\""));
        assert!(l[5].contains(&format!("\"summary\":\"{}\"", "x".repeat(400))));
        let back = SessionWriter::open(&dir, b"s1").unwrap();
        assert_eq!(back.head(), w.head());
        std::fs::remove_dir_all(std::ffi::OsStr::from_bytes(&dir)).unwrap();
    }

    #[test]
    fn resume_rebuilds_history() {
        let dir = scratch("resume");
        std::fs::create_dir_all(std::ffi::OsStr::from_bytes(&dir)).unwrap();
        let path = crate::osx::join(&dir, b"r.jsonl");
        let rows = [
            r#"{"type":"session","id":"r","ts":1}"#,
            r#"{"type":"prompt","id":"p1","parentId":null,"text":"one"}"#,
            r#"{"type":"assistant","id":"a1","parentId":"p1","text":"","model":"m","toolUses":[{"id":"t1","name":"read"},5,{"name":"bash"}]}"#,
            r#"{"type":"tool_call","id":"c1","parentId":"a1","toolUseId":"t1","input":{"path":"k"}}"#,
            "",
            "{\"type\":\"tool_result\",\"id\":\"r1\",\"parentId\":\"c1\",\"summary\":\"K\"}\r",
            r#"{"type":"prompt","id":"p2","parentId":"a1","text":"branch"}"#,
            r#"{"torn"#,
            r#"{"type":"label","id":"l1","parentId":"r1"}"#,
            r#"{"type":"head","id":"h1","leafId":"r1"}"#,
            r#"{"type":"x","n":1e400,"id":"bad"}"#,
        ];
        std::fs::write(std::ffi::OsStr::from_bytes(&path), rows.join("\n")).unwrap();
        let all = read_entries(&path).unwrap();
        assert_eq!(all.len(), 8);
        assert_eq!(head_of(&all), "r1");
        let hp = head_path(&path).unwrap();
        let ids: Vec<&str> = hp.iter().map(|e| e.id.as_str()).collect();
        assert_eq!(ids, vec!["p1", "a1", "c1", "r1"]);
        let h = history_from_entries(&hp);
        assert_eq!(h.len(), 3);
        assert_eq!(h[0].role, "user");
        assert_eq!(h[1].calls.len(), 2);
        assert_eq!(
            h[1].calls[0].input.as_ref().unwrap().get("path"),
            Some(&Any::Str("k".into()))
        );
        assert_eq!(
            (h[1].calls[1].id.as_str(), h[1].calls[1].name.as_str()),
            ("", "bash")
        );
        assert_eq!(h[1].calls[1].input, None);
        assert_eq!(h[2].text, b"K");
        std::fs::remove_dir_all(std::ffi::OsStr::from_bytes(&dir)).unwrap();
    }
}
