//! The gate's side of the resident server (`gate_server.py`): one request
//! line read off gate.sock, the call decided as the caller's, and the
//! reply line; and `daisugi hook report` as the server runs it. The server
//! itself is in `cli::servecmd`.

use super::logs::Tree;
use super::paths::{path_join, path_parent, path_str};
use super::py::text::{repr, surrogate_char};
use super::pyjson::{self, dumps, py_decode_error, py_float_repr, Object, Value};
use super::pystr::safe_session_id;
use super::state_report::{report_explicit, valid_coppice_pane_id};
use std::collections::HashMap;

/// `gate_server._MAX_REQUEST`: a request line is read up to its newline or
/// this many bytes, whichever comes first.
pub const MAX_REQUEST_BYTES: usize = 4 * 1024 * 1024;

/// The nesting the oracle's JSON reader accepts on the server's request
/// thread before it raises RecursionError: 9,992 containers in all (found
/// by bisection against the Python server).
const REQUEST_DEPTH: usize = 9992;

/// The variables `serve()` drops from the server's own environment: it
/// serves every session, so it is no one pane.
pub const PANE_ENV_KEYS: [&str; 4] = ["COPPICE_SOCK", "COPPICE_PANE", "HERDR_PANE_ID", "HERDR_PANE"];

/// Marks a child a resident server started: its stdin is the request
/// (`encode_request`), not a hook payload. A parent `gate check` removes
/// it from its own child's environment, so no host setting it can make a
/// hook read its payload as a request.
pub const RESIDENT_ENV: &str = "DAISUGI_GATE_RESIDENT";

/// The pane identity a resident request names, as `gate.resident_call` is
/// entered with it. The four strings are set only when the request carried
/// a string for them (`_str_or_none`). `peer_pid` is never from the
/// request: the server reads it off the connection with SO_PEERCRED.
#[derive(Debug, Clone, Default)]
pub struct Caller {
    pub sock: Option<String>,
    pub pane: Option<String>,
    pub herdr_pane: Option<String>,
    pub data_dir: Option<String>,
    pub peer_pid: Option<i64>,
}

impl Caller {
    /// Whether `report_state` runs in its explicit mode for this caller.
    pub fn names_a_pane(&self) -> bool {
        self.sock.is_some() || self.pane.is_some() || self.herdr_pane.is_some()
    }

    /// The pane a resident call's event names: the caller's claim when it
    /// is shaped like a coppice pane id, else none.
    pub fn event_pane(&self) -> Value {
        match &self.pane {
            Some(p) if valid_coppice_pane_id(p) => Value::Str(p.clone()),
            _ => Value::Null,
        }
    }

    /// `report_state` in the explicit mode, for this caller.
    pub fn report(&self, ev: &Object) -> &'static str {
        report_explicit(self.sock.as_deref(), self.pane.as_deref(), self.herdr_pane.as_deref(), self.peer_pid, ev)
    }
}

/// One resident request, decoded.
#[derive(Debug, Clone, Default)]
pub struct Request {
    pub argv: Vec<String>,
    pub stdin: Vec<u8>,
    pub caller: Caller,
}

impl Request {
    /// `argv[:2] == ["hook", "report"]`: the request goes to `daisugi hook
    /// report`, not to the gate.
    pub fn is_hook_report(&self) -> bool {
        self.argv.len() >= 2 && self.argv[0] == "hook" && self.argv[1] == "report"
    }
}

/// The try block of `gate_server._Handler.handle`: the line read as
/// `json.loads` reads bytes, argv as `[str(a) for a in req["argv"]]`, stdin
/// as `base64.b64decode(req.get("stdin_b64", ""))`. `None` is the oracle's
/// except: a bad request. Deep input recurses: run it on a large stack.
pub fn parse_request(line: &[u8]) -> Option<Request> {
    let text = request_text(line)?;
    if py_decode_error(&text, REQUEST_DEPTH).is_some() {
        return None;
    }
    let obj = match pyjson::loads(&text) {
        Ok(Value::Obj(o)) => o,
        _ => return None,
    };
    let argv = match obj.get("argv")? {
        Value::List(l) | Value::Tuple(l) => l.iter().map(str_of).collect(),
        Value::Obj(o) => o.keys().to_vec(),
        Value::Str(s) => s.chars().map(String::from).collect(),
        _ => return None,
    };
    let stdin = match obj.get("stdin_b64") {
        None => vec![],
        Some(Value::Str(s)) => {
            // str input must be ASCII ("string argument should contain only
            // ASCII characters").
            if !s.is_ascii() {
                return None;
            }
            b64decode_lenient(s.as_bytes())?
        }
        Some(_) => return None,
    };
    let field = |k: &str| match obj.get(k) {
        Some(Value::Str(s)) => Some(s.clone()),
        _ => None,
    };
    Some(Request {
        argv,
        stdin,
        caller: Caller {
            sock: field("coppice_sock"),
            pane: field("coppice_pane"),
            herdr_pane: field("herdr_pane"),
            data_dir: field("coppice_data_dir"),
            peer_pid: None,
        },
    })
}

/// `str(v)` of a decoded JSON value, with no depth limit.
fn str_of(v: &Value) -> String {
    match v {
        Value::Str(s) => s.clone(),
        _ => {
            let mut b = String::new();
            repr_into(&mut b, v);
            b
        }
    }
}

fn repr_into(b: &mut String, v: &Value) {
    match v {
        Value::Null => b.push_str("None"),
        Value::Bool(true) => b.push_str("True"),
        Value::Bool(false) => b.push_str("False"),
        Value::Int(t) => b.push_str(t),
        Value::Float(f) => b.push_str(&py_float_repr(*f)),
        Value::Str(s) => b.push_str(&repr(s)),
        Value::List(l) | Value::Tuple(l) => {
            b.push('[');
            for (i, e) in l.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                repr_into(b, e);
            }
            b.push(']');
        }
        Value::Obj(o) => {
            b.push('{');
            for (i, (k, x)) in o.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                b.push_str(&repr(k));
                b.push_str(": ");
                repr_into(b, x);
            }
            b.push('}');
        }
    }
}

/// The decode `json.loads` does on bytes: UTF-8 with surrogatepass (an
/// encoded lone surrogate is kept, as the character that stands for it),
/// a UTF-8 BOM dropped. Python also reads UTF-16 and UTF-32 by their BOM
/// or zero bytes; this reads UTF-8 only, so such a request is a bad
/// request (ruling G2-2).
fn request_text(b: &[u8]) -> Option<String> {
    let starts = |p: &[u8]| b.len() >= p.len() && &b[..p.len()] == p;
    let mut b = b;
    if starts(b"\x00\x00\xfe\xff") || starts(b"\xff\xfe\x00\x00") || starts(b"\xfe\xff") || starts(b"\xff\xfe") {
        return None;
    }
    if starts(b"\xef\xbb\xbf") {
        b = &b[3..];
    } else if (b.len() >= 4 || b.len() == 2) && (b[0] == 0 || b[1] == 0) {
        return None;
    }
    let mut out = String::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match std::str::from_utf8(&b[i..]) {
            Ok(s) => {
                out.push_str(s);
                break;
            }
            Err(e) => {
                let good = e.valid_up_to();
                out.push_str(std::str::from_utf8(&b[i..i + good]).ok()?);
                i += good;
                // An encoded surrogate (ED A0..BF 80..BF) is what
                // surrogatepass lets through.
                if i + 2 < b.len() && b[i] == 0xED && (0xA0..=0xBF).contains(&b[i + 1]) && (0x80..=0xBF).contains(&b[i + 2])
                {
                    let unit = 0xD000 | ((b[i + 1] as u32 & 0x3F) << 6) | (b[i + 2] as u32 & 0x3F);
                    out.push(surrogate_char(unit));
                    i += 3;
                    continue;
                }
                return None;
            }
        }
    }
    Some(out)
}

/// `base64.b64decode(s)` with validate=False (CPython 3.12's
/// `binascii.a2b_base64` in non-strict mode): bytes outside the alphabet
/// are skipped, a pad after two or three data characters ends the data,
/// and data left one or two characters into a quad is an error.
pub fn b64decode_lenient(s: &[u8]) -> Option<Vec<u8>> {
    let mut out = Vec::with_capacity(s.len() * 3 / 4);
    let (mut quad, mut pads, mut left) = (0u8, 0u8, 0u8);
    for &c in s {
        if c == b'=' {
            if quad >= 2 {
                pads += 1;
                if quad + pads >= 4 {
                    return Some(out);
                }
            }
            continue;
        }
        let x = match c {
            b'A'..=b'Z' => c - b'A',
            b'a'..=b'z' => c - b'a' + 26,
            b'0'..=b'9' => c - b'0' + 52,
            b'+' => 62,
            b'/' => 63,
            _ => continue,
        };
        pads = 0;
        match quad {
            0 => {
                quad = 1;
                left = x;
            }
            1 => {
                quad = 2;
                out.push(left << 2 | x >> 4);
                left = x & 0x0f;
            }
            2 => {
                quad = 3;
                out.push(left << 4 | x >> 2);
                left = x & 0x03;
            }
            _ => {
                quad = 0;
                out.push(left << 6 | x);
                left = 0;
            }
        }
    }
    (quad == 0).then_some(out)
}

/// The handler's reply: `json.dumps` of {v, stdout, stderr, exit_code},
/// and a newline.
pub fn reply_line(stdout: &str, stderr: &str, exit: i64) -> Vec<u8> {
    let o = Object::new().with("v", 1i64).with("stdout", stdout).with("stderr", stderr).with("exit_code", exit);
    format!("{}\n", dumps(&Value::Obj(o), true)).into_bytes()
}

/// The reply to a request the handler cannot read.
pub fn bad_request() -> Vec<u8> {
    reply_line("", "openDaisugi gate: DENIED — bad request", 2)
}

/// A resident call as its child's stdin. argv words go as JSON strings,
/// which carry a NUL or a lone surrogate that an exec argv cannot.
pub fn encode_request(q: &Request) -> Vec<u8> {
    let c = &q.caller;
    let o = Object::new()
        .with("argv", q.argv.iter().map(|a| Value::Str(a.clone())).collect::<Vec<_>>())
        .with("stdin", hex(&q.stdin))
        .with("sock", c.sock.clone())
        .with("pane", c.pane.clone())
        .with("herdr_pane", c.herdr_pane.clone())
        .with("data_dir", c.data_dir.clone())
        .with("peer_pid", c.peer_pid);
    dumps(&Value::Obj(o), true).into_bytes()
}

/// `encode_request`'s bytes read back.
pub fn decode_request(b: &[u8]) -> Option<Request> {
    let text = std::str::from_utf8(b).ok()?;
    let o = match pyjson::loads(text) {
        Ok(Value::Obj(o)) => o,
        _ => return None,
    };
    let argv = match o.value("argv") {
        Value::List(l) => l.iter().map(|w| w.as_str().map(String::from)).collect::<Option<Vec<_>>>()?,
        _ => return None,
    };
    let field = |k: &str| o.value(k).as_str().map(String::from);
    let peer_pid = match o.value("peer_pid") {
        Value::Int(t) => t.parse().ok(),
        _ => None,
    };
    Some(Request {
        argv,
        stdin: unhex(o.value("stdin").as_str()?)?,
        caller: Caller {
            sock: field("sock"),
            pane: field("pane"),
            herdr_pane: field("herdr_pane"),
            data_dir: field("data_dir"),
            peer_pid,
        },
    })
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Option<Vec<u8>> {
    if s.len() % 2 != 0 {
        return None;
    }
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok()).collect()
}

// ---------------------------------------------------------------------------
// hook report
// ---------------------------------------------------------------------------

/// `_state_report.hook_report_argv` as the resident server runs it: argv
/// after "hook report", the event on `raw`, the caller's pane identity,
/// and the server's home for the default root. Returns the reply's
/// stdout, stderr and exit code. Deep input recurses: run it on a large
/// stack.
pub fn hook_report(argv: &[String], raw: &[u8], caller: &Caller, home: &str) -> (String, String, i32) {
    hook_report_with(argv, raw, home, &|row: &Object| {
        if caller.names_a_pane() {
            caller.report(row);
        }
    })
}

/// `_state_report.hook_report_argv` as the command line runs it: the pane
/// identity comes from `env`, the environment this process shares with its
/// caller, as `report_state` reads it by default.
pub fn hook_report_cli(argv: &[String], raw: &[u8], env: &HashMap<String, String>, home: &str) -> (String, String, i32) {
    hook_report_with(argv, raw, home, &|row: &Object| {
        let _ = super::state_report::report_state_env(env, row);
    })
}

fn hook_report_with(argv: &[String], raw: &[u8], home: &str, deliver: &dyn Fn(&Object)) -> (String, String, i32) {
    let (pane, root) = match parse_hook_report_argv(argv) {
        Some(x) => x,
        None => return (String::new(), "daisugi hook report: bad arguments".into(), 1),
    };
    let root = root.unwrap_or_else(|| {
        path_join(&crate::datahome::dir(|k| std::env::var(k).ok(), &path_str(home), crate::datahome::exists), "gate")
    });
    let text = match std::str::from_utf8(raw) {
        Ok(t) => t.to_string(),
        Err(_) => {
            return (String::new(), format!("daisugi hook report: not valid JSON: {}", utf8_error_text(raw)), 1);
        }
    };
    if let Err(m) = pyjson::loads_py(&text, REQUEST_DEPTH) {
        return (String::new(), format!("daisugi hook report: not valid JSON: {m}"), 1);
    }
    let mut row = match pyjson::loads(&text) {
        Ok(Value::Obj(o)) => o,
        _ => return (String::new(), "daisugi hook report: event must be a JSON object".into(), 1),
    };
    if matches!(row.value("source").as_str(), Some("gate") | Some("operator")) {
        row.set("source", "headless");
    }
    if let Some(p) = pane {
        row.set("pane", p);
    }
    if let Some(m) = validate_hook_report_row(&row) {
        return (String::new(), format!("daisugi hook report: {m}"), 1);
    }
    let _ = append_report_tree(&root, &row);
    deliver(&row);
    (String::new(), String::new(), 0)
}

/// Python's text for a UnicodeDecodeError of `raw.decode("utf-8")`.
pub fn utf8_error_text(raw: &[u8]) -> String {
    let e = match std::str::from_utf8(raw) {
        Err(e) => e,
        Ok(_) => return String::new(),
    };
    let pos = e.valid_up_to();
    match e.error_len() {
        None => {
            let n = raw.len() - pos;
            if n == 1 {
                format!("'utf-8' codec can't decode byte 0x{:02x} in position {pos}: unexpected end of data", raw[pos])
            } else {
                format!("'utf-8' codec can't decode bytes in position {pos}-{}: unexpected end of data", raw.len() - 1)
            }
        }
        Some(1) => {
            let b = raw[pos];
            let why = if (0x80..0xC2).contains(&b) || b >= 0xF5 { "invalid start byte" } else { "invalid continuation byte" };
            format!("'utf-8' codec can't decode byte 0x{b:02x} in position {pos}: {why}")
        }
        Some(n) => format!("'utf-8' codec can't decode bytes in position {pos}-{}: invalid continuation byte", pos + n - 1),
    }
}

/// `_build_hook_report_parser().parse_args(argv)` (Python 3.12.12):
/// --pane and --root, each taking one value, abbreviations allowed, no
/// --help, no positionals, and "--" kept as an argument, so unrecognized.
/// `None` where argparse would exit. The root is a Path.
pub fn parse_hook_report_argv(argv: &[String]) -> Option<(Option<String>, Option<String>)> {
    #[derive(Clone)]
    enum W {
        Arg(String),
        Opt(Option<&'static str>, Option<String>),
    }
    let mut words = vec![];
    for a in argv {
        if a == "--" {
            // No positional takes it: unrecognized.
            return None;
        }
        words.push(match classify_optional(a)? {
            Some((act, explicit)) => W::Opt(act, explicit),
            None => W::Arg(a.clone()),
        });
    }
    let (mut pane, mut root) = (None, None);
    let mut i = 0;
    while i < words.len() {
        let (act, explicit) = match &words[i] {
            W::Opt(Some(act), explicit) => (*act, explicit.clone()),
            _ => return None,
        };
        let val = match explicit {
            Some(v) => v,
            None => match words.get(i + 1) {
                Some(W::Arg(v)) => {
                    i += 1;
                    v.clone()
                }
                _ => return None,
            },
        };
        if act == "--pane" {
            pane = Some(val);
        } else {
            root = Some(path_str(&val));
        }
        i += 1;
    }
    Some((pane, root))
}

/// `ArgumentParser._parse_optional` for --pane and --root: `Some(None)` for
/// an argument, `Some(Some((action, explicit)))` for an option word (an
/// unknown one has no action), `None` for argparse's own error (an
/// ambiguous abbreviation).
#[allow(clippy::type_complexity)]
fn classify_optional(a: &str) -> Option<Option<(Option<&'static str>, Option<String>)>> {
    const OPTS: [&str; 2] = ["--pane", "--root"];
    if !a.starts_with('-') {
        return Some(None);
    }
    if let Some(o) = OPTS.iter().find(|o| **o == a) {
        return Some(Some((Some(o), None)));
    }
    if a.chars().count() == 1 {
        return Some(None);
    }
    if let Some((k, v)) = a.split_once('=') {
        if let Some(o) = OPTS.iter().find(|o| **o == k) {
            return Some(Some((Some(o), Some(v.to_string()))));
        }
    }
    if a.starts_with("--") {
        let (prefix, explicit) = match a.split_once('=') {
            Some((k, v)) => (k, Some(v.to_string())),
            None => (a, None),
        };
        let found: Vec<&'static str> = OPTS.iter().copied().filter(|o| o.starts_with(prefix)).collect();
        if found.len() > 1 {
            return None;
        }
        if let Some(o) = found.first() {
            return Some(Some((Some(o), explicit)));
        }
    }
    if is_negative_number(a) || a.contains(' ') {
        return Some(None);
    }
    Some(Some((None, None)))
}

/// argparse's `_negative_number_matcher`: `^-\d+$|^-\d*\.\d+$`.
fn is_negative_number(a: &str) -> bool {
    let body = match a.strip_prefix('-') {
        Some(b) => b,
        None => return false,
    };
    let digits = |s: &str| s.chars().all(|c| c.is_ascii_digit());
    if !body.is_empty() && digits(body) {
        return true;
    }
    match body.split_once('.') {
        Some((l, r)) => digits(l) && !r.is_empty() && digits(r),
        None => false,
    }
}

/// `_state_report._validate_hook_report_row`: `None` when the row
/// passes, else the ValueError's text.
pub fn validate_hook_report_row(row: &Object) -> Option<String> {
    for k in ["session_id", "harness", "state", "source", "ts"] {
        if row.get(k).is_none() {
            return Some(format!("event missing required field {}", repr(k)));
        }
    }
    for k in ["session_id", "harness", "state", "source"] {
        if row.value(k).as_str().is_none() {
            return Some(format!("{k} must be a string"));
        }
    }
    let number = |v: &Value| matches!(v, Value::Int(_) | Value::Float(_));
    if !number(row.value("ts")) {
        return Some("ts must be a number".into());
    }
    let state = row.value("state").as_str().unwrap_or_default();
    let source = row.value("source").as_str().unwrap_or_default();
    if !["idle", "working", "blocked", "done", "unknown"].contains(&state) {
        return Some(format!("unknown state {}", repr(state)));
    }
    if !["operator", "gate", "headless", "process", "manifest"].contains(&source) {
        return Some(format!("unknown source {}", repr(source)));
    }
    if state == "done" && source != "process" && source != "headless" {
        return Some("state 'done' may only come from source 'process' or 'headless'".into());
    }
    match row.get("detail") {
        None => {}
        Some(Value::Str(d)) => {
            if d.chars().count() > 200 {
                return Some("detail exceeds 200 characters".into());
            }
        }
        Some(_) => return Some("detail must be a string".into()),
    }
    if !matches!(row.value("harness_session_id"), Value::Null | Value::Str(_)) {
        return Some("harness_session_id must be a string".into());
    }
    if !matches!(row.value("pane"), Value::Null | Value::Str(_)) {
        return Some("pane must be a string".into());
    }
    let ask = row.value("ask");
    if !ask.is_null() {
        let a = match ask {
            Value::Obj(o) => o,
            _ => return Some("ask must be an object".into()),
        };
        if state != "blocked" {
            return Some("ask is only valid when state == 'blocked'".into());
        }
        for k in ["id", "tool", "summary", "deadline"] {
            if a.get(k).is_none() {
                return Some(format!("ask missing required field {}", repr(k)));
            }
        }
        for k in ["id", "tool", "summary"] {
            if a.value(k).as_str().is_none() {
                return Some(format!("ask {k} must be a string"));
            }
        }
        if !number(a.value("deadline")) {
            return Some("ask deadline must be a number".into());
        }
        if !matches!(a.value("tier"), Value::Null | Value::Str(_)) {
            return Some("ask tier must be a string".into());
        }
    }
    if state == "blocked" && source == "gate" && ask.is_null() {
        return Some("a gate blocked event needs an ask".into());
    }
    let v = row.get("v").cloned().unwrap_or(Value::Int("1".into()));
    match &v {
        Value::Int(t) if t == "1" => None,
        Value::Int(t) => Some(format!("unsupported event schema version {t}")),
        _ => {
            let mut b = String::new();
            repr_into(&mut b, &v);
            Some(format!("invalid schema version {b}"))
        }
    }
}

/// `hook_report_argv`'s tree step: `SessionTree.open_or_create` under the
/// root's parent, then `append("state", row)`. A step that raises in the
/// oracle stops the rest here too.
fn append_report_tree(root: &str, row: &Object) -> Result<(), super::PyErr> {
    let dir = path_join(&path_parent(root), "sessions");
    let sid = safe_session_id(row.value("session_id").as_str().unwrap_or_default());
    let header = Object::new()
        .with("v", 1i64)
        .with("harness", row.value("harness").clone())
        .with("cwd", "")
        .with("harnessSessionId", row.value("harness_session_id").clone())
        .with("transcriptPath", Value::Null)
        .with("parentSession", Value::Null)
        .with("parentEntry", Value::Null)
        .with("cacheKey", Value::Null);
    let mut t = Tree::open_or_create(&dir, &sid, header)?;
    t.append("state", row, None)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::py::text::as_surrogate;

    fn big_stack<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
        std::thread::Builder::new().stack_size(256 << 20).spawn(f).unwrap().join().unwrap()
    }

    fn vectors() -> serde_json::Value {
        let p = concat!(env!("CARGO_MANIFEST_DIR"), "/../go/internal/gate/testdata/resident_vectors.json");
        serde_json::from_str(&std::fs::read_to_string(p).unwrap()).unwrap()
    }

    #[test]
    fn b64decode_agrees_with_python() {
        for v in vectors()["b64decode"].as_array().unwrap() {
            let (text, want, ok) = (v[0].as_str().unwrap(), v[1].as_str().unwrap(), v[2].as_bool().unwrap());
            let got = b64decode_lenient(text.as_bytes());
            assert_eq!(got.is_some(), ok, "{text:?}");
            if let Some(g) = got {
                assert_eq!(hex(&g), want, "{text:?}");
            }
        }
    }

    #[test]
    fn hook_report_argv_agrees_with_python() {
        for v in vectors()["hook_report_argv"].as_array().unwrap() {
            let argv: Vec<String> = v[0].as_array().unwrap().iter().map(|a| a.as_str().unwrap().to_string()).collect();
            let got = parse_hook_report_argv(&argv);
            assert_eq!(got.is_some(), v[3].as_bool().unwrap(), "{argv:?}");
            if let Some((pane, root)) = got {
                assert_eq!(pane.as_deref(), v[1].as_str(), "{argv:?}");
                assert_eq!(root, v[2].as_str().map(path_str), "{argv:?}");
            }
        }
    }

    #[test]
    fn requests_parse_as_the_handler_reads_them() {
        big_stack(|| {
            let ok = |line: &str| parse_request(line.as_bytes());
            let q = ok("{\"v\": 1, \"argv\": [\"--format\", \"pi\"], \"stdin_b64\": \"e30=\"}\n").unwrap();
            assert_eq!(q.argv, ["--format", "pi"]);
            assert_eq!(q.stdin, b"{}");
            assert_eq!(ok("{\"argv\": \"ab\"}").unwrap().argv, ["a", "b"]);
            assert_eq!(ok("{\"argv\": {\"--x\": 1, \"y\": 2}}").unwrap().argv, ["--x", "y"]);
            assert_eq!(
                ok("{\"argv\": [true, null, 1, 1.5, -0.0, 1e100, [1, \"a\"], {\"k\": null}]}").unwrap().argv,
                ["True", "None", "1", "1.5", "-0.0", "1e+100", "[1, 'a']", "{'k': None}"]
            );
            for bad in [
                "{\"argv\": 5}",
                "{\"argv\": null}",
                "{\"v\": 1}",
                "[1]",
                "{\"argv\": [], \"stdin_b64\": 5}",
                "{\"argv\": [], \"stdin_b64\": \"\u{e9}\"}",
                "{\"argv\": [], \"stdin_b64\": \"abc\"}",
                "",
            ] {
                assert!(ok(bad).is_none(), "{bad:?}");
            }
            assert!(parse_request(b"\xef\xbb\xbf{\"argv\": []}").is_some());
            assert!(parse_request(b"\xff\xfe{\x00}\x00").is_none());
            assert!(parse_request(b"{\"argv\": [], \"x\": \"\xff\"}").is_none());
            let q = parse_request(b"{\"argv\": [\"\xed\xb2\x80\"]}").unwrap();
            assert_eq!(q.argv[0].chars().map(|c| as_surrogate(c)).collect::<Vec<_>>(), [Some(0xDC80)]);
            let deep = |n: usize| format!("{{\"argv\": [], \"x\": {}{}}}", "[".repeat(n), "]".repeat(n));
            assert!(ok(&deep(9991)).is_some());
            assert!(ok(&deep(9992)).is_none());
        });
    }

    #[test]
    fn caller_fields_are_strings_only() {
        let q = parse_request(
            b"{\"argv\": [], \"coppice_sock\": \"/s\", \"coppice_pane\": 3, \"herdr_pane\": null, \"coppice_data_dir\": \"/d\"}",
        )
        .unwrap();
        assert_eq!(q.caller.sock.as_deref(), Some("/s"));
        assert!(q.caller.pane.is_none() && q.caller.herdr_pane.is_none());
        assert_eq!(q.caller.data_dir.as_deref(), Some("/d"));
    }

    #[test]
    fn the_reply_is_pythons_json() {
        let s = format!("\u{e9}{}", surrogate_char(0xDC80));
        assert_eq!(
            String::from_utf8(reply_line("{\"continue\": true}", &s, 2)).unwrap(),
            "{\"v\": 1, \"stdout\": \"{\\\"continue\\\": true}\", \"stderr\": \"\\u00e9\\udc80\", \"exit_code\": 2}\n"
        );
        assert_eq!(
            String::from_utf8(bad_request()).unwrap(),
            "{\"v\": 1, \"stdout\": \"\", \"stderr\": \"openDaisugi gate: DENIED \\u2014 bad request\", \"exit_code\": 2}\n"
        );
    }

    #[test]
    fn a_request_round_trips_through_the_child() {
        let q = Request {
            argv: vec!["a\u{0}b".into(), surrogate_char(0xDC80).to_string(), "\u{e9}".into()],
            stdin: vec![0, 0xff, b'{'],
            caller: Caller { sock: Some("/s".into()), peer_pid: Some(42), ..Default::default() },
        };
        let back = decode_request(&encode_request(&q)).unwrap();
        assert_eq!(back.argv, q.argv);
        assert_eq!(back.stdin, q.stdin);
        assert_eq!(back.caller.sock.as_deref(), Some("/s"));
        assert!(back.caller.pane.is_none());
        assert_eq!(back.caller.peer_pid, Some(42));
    }

    #[test]
    fn hook_report_rows_are_checked_as_the_oracle_checks_them() {
        let check = |t: &str| match pyjson::loads(t).unwrap() {
            Value::Obj(o) => validate_hook_report_row(&o),
            _ => panic!(),
        };
        let base = "\"ts\": 1, \"session_id\": \"s\", \"harness\": \"pi\"";
        assert_eq!(check(&format!("{{{base}, \"state\": \"idle\", \"source\": \"headless\"}}")), None);
        assert_eq!(
            check(&format!("{{{base}, \"state\": \"idle\"}}")).as_deref(),
            Some("event missing required field 'source'")
        );
        assert_eq!(
            check(&format!("{{{base}, \"state\": \"idle\", \"source\": \"headless\", \"v\": true}}")).as_deref(),
            Some("invalid schema version True")
        );
        assert_eq!(
            check(&format!("{{{base}, \"state\": \"nap\", \"source\": \"headless\"}}")).as_deref(),
            Some("unknown state 'nap'")
        );
        assert_eq!(
            check(&format!("{{{base}, \"state\": \"idle\", \"source\": \"headless\", \"v\": 3}}")).as_deref(),
            Some("unsupported event schema version 3")
        );
    }

    #[test]
    fn utf8_errors_read_as_pythons() {
        assert_eq!(utf8_error_text(b"\xff{}"), "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte");
        assert_eq!(utf8_error_text(b"a\xc3"), "'utf-8' codec can't decode byte 0xc3 in position 1: unexpected end of data");
    }
}
