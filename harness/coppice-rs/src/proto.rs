//! The coppice wire format: one JSON object per line, both ways. Requests
//! carry an id, replies echo it, events carry none. A connection speaks the
//! native envelope until its first JSON-RPC 2.0 line, then JSON-RPC for the
//! rest of its life. PROTOCOL.md in harness/coppice states both shapes.

use std::collections::BTreeMap;
use std::io::{self, BufRead, Write};
use std::sync::Mutex;

use serde_json::value::RawValue;
use serde_json::Value;

use crate::gojson;

/// The protocol version server.status reports.
pub const VERSION: i64 = 1;

/// The longest wire line a client may send.
pub const MAX_LINE: usize = 1 << 20;

/// The closed set of error codes. A code outside it is a server bug.
pub const ERR_BAD_REQUEST: &str = "bad_request";
pub const ERR_NO_SUCH_PANE: &str = "no_such_pane";
pub const ERR_NO_SUCH_WORKSPACE: &str = "no_such_workspace";
pub const ERR_NO_SUCH_TAB: &str = "no_such_tab";
pub const ERR_PANE_CLOSED: &str = "pane_closed";
pub const ERR_SERVER_CLOSED: &str = "server_closed";
pub const ERR_NOT_ATTACHED: &str = "not_attached";
pub const ERR_ADAPTER: &str = "adapter_error";
pub const ERR_SPAWN_FAILED: &str = "spawn_failed";
pub const ERR_TIMEOUT: &str = "timeout";
pub const ERR_UNAUTHORIZED: &str = "unauthorized";
pub const ERR_INTERNAL: &str = "internal";

const VALID_CODES: &[&str] = &[
    ERR_BAD_REQUEST,
    ERR_NO_SUCH_PANE,
    ERR_NO_SUCH_WORKSPACE,
    ERR_NO_SUCH_TAB,
    ERR_PANE_CLOSED,
    ERR_SERVER_CLOSED,
    ERR_NOT_ATTACHED,
    ERR_ADAPTER,
    ERR_SPAWN_FAILED,
    ERR_TIMEOUT,
    ERR_UNAUTHORIZED,
    ERR_INTERNAL,
];

pub fn valid_code(c: &str) -> bool {
    VALID_CODES.contains(&c)
}

/// JSON-RPC 2.0 error codes.
pub const RPC_PARSE_ERROR: i64 = -32700;
pub const RPC_INVALID_REQUEST: i64 = -32600;
pub const RPC_METHOD_NOT_FOUND: i64 = -32601;
pub const RPC_SERVER_ERROR: i64 = -32000;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Framing {
    Native,
    JsonRpc,
}

/// One request. The parameters sit beside id and cmd on a native line, and
/// under params on a JSON-RPC line; either way they land in params, raw, and
/// the typed helpers read them as Go's json.Unmarshal would.
#[derive(Debug, Default)]
pub struct Request {
    pub id: String,
    pub cmd: String,
    pub params: BTreeMap<String, Box<RawValue>>,
    /// The id token exactly as the client sent it, or None.
    pub raw_id: Option<String>,
}

fn unmarshal_str(raw: &RawValue) -> Option<String> {
    // Go reads null into a string as no change: "", with no error.
    let t = raw.get();
    if t == "null" {
        return Some(String::new());
    }
    serde_json::from_str::<String>(t).ok()
}

/// Go's json.Unmarshal of one integer literal into an int: no fraction, no
/// exponent, in range. null is 0 with no error.
fn unmarshal_int(raw: &RawValue) -> Option<i64> {
    let t = raw.get();
    if t == "null" {
        return Some(0);
    }
    if t.starts_with('"') || t.contains(['.', 'e', 'E']) {
        return None;
    }
    t.parse::<i64>().ok()
}

impl Request {
    fn from_map(m: BTreeMap<String, Box<RawValue>>) -> Request {
        let mut r = Request::default();
        if let Some(raw) = m.get("id") {
            if let Some(s) = unmarshal_str(raw) {
                r.id = s;
            }
            r.raw_id = Some(raw.get().to_string());
        }
        if let Some(raw) = m.get("cmd") {
            if let Some(s) = unmarshal_str(raw) {
                r.cmd = s;
            }
        }
        r.params = m;
        r
    }

    pub fn str(&self, k: &str) -> Option<String> {
        unmarshal_str(self.params.get(k)?)
    }

    /// The string under k, or "" when it is absent or not a string.
    pub fn s(&self, k: &str) -> String {
        self.str(k).unwrap_or_default()
    }

    pub fn int(&self, k: &str) -> Option<i64> {
        unmarshal_int(self.params.get(k)?)
    }

    pub fn bool(&self, k: &str) -> Option<bool> {
        let t = self.params.get(k)?.get();
        match t {
            "true" => Some(true),
            "false" | "null" => Some(false),
            _ => None,
        }
    }

    /// A list of strings. null is an empty list with no error, as in Go.
    pub fn str_slice(&self, k: &str) -> Option<Vec<String>> {
        let t = self.params.get(k)?.get();
        if t == "null" {
            return Some(Vec::new());
        }
        let v: Vec<Value> = serde_json::from_str(t).ok()?;
        let mut out = Vec::with_capacity(v.len());
        for x in v {
            match x {
                Value::String(s) => out.push(s),
                // Go leaves a null element as "".
                Value::Null => out.push(String::new()),
                _ => return None,
            }
        }
        Some(out)
    }

    /// A map of strings to strings. null is an empty map with no error.
    pub fn str_map(&self, k: &str) -> Option<BTreeMap<String, String>> {
        let t = self.params.get(k)?.get();
        if t == "null" {
            return Some(BTreeMap::new());
        }
        let v: BTreeMap<String, Value> = serde_json::from_str(t).ok()?;
        let mut out = BTreeMap::new();
        for (key, x) in v {
            match x {
                Value::String(s) => {
                    out.insert(key, s);
                }
                Value::Null => {
                    out.insert(key, String::new());
                }
                _ => return None,
            }
        }
        Some(out)
    }

    pub fn raw(&self, k: &str) -> Option<&str> {
        self.params.get(k).map(|r| r.get())
    }
}

/// Every key and raw value of a JSON object, in document order, duplicates
/// kept. None when raw is not an object.
pub fn object_pairs(raw: &str) -> Option<Vec<(String, Box<RawValue>)>> {
    use serde::de::{Deserializer, MapAccess, Visitor};
    struct Pairs;
    impl<'de> Visitor<'de> for Pairs {
        type Value = Vec<(String, Box<RawValue>)>;
        fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
            f.write_str("an object")
        }
        fn visit_map<A: MapAccess<'de>>(self, mut m: A) -> Result<Self::Value, A::Error> {
            let mut out = Vec::new();
            while let Some((k, v)) = m.next_entry::<String, Box<RawValue>>()? {
                out.push((k, v));
            }
            Ok(out)
        }
    }
    let mut de = serde_json::Deserializer::from_str(raw);
    let v = de.deserialize_map(Pairs).ok()?;
    de.end().ok()?;
    Some(v)
}

/// Go's strings.EqualFold for the keys a struct field matches: equal under
/// simple case folding.
pub fn equal_fold(a: &str, b: &str) -> bool {
    fn fold(c: char) -> char {
        match c {
            '\u{17F}' => 's',
            '\u{212A}' => 'k',
            '\u{2126}' => 'ω',
            c => c.to_lowercase().next().unwrap_or(c),
        }
    }
    a.chars().count() == b.chars().count()
        && a.chars().zip(b.chars()).all(|(x, y)| fold(x) == fold(y))
}

/// The value Go's json.Unmarshal leaves in a string struct field named
/// field: keys match it with case folding, later keys win, and null leaves
/// the field as it was. Err when the raw is not an object or a matching
/// value is neither a string nor null.
pub fn struct_string_field(raw: &str, field: &str) -> Result<String, ()> {
    // Go reads null into a struct as no change at all.
    if raw.trim() == "null" {
        return Ok(String::new());
    }
    let pairs = object_pairs(raw).ok_or(())?;
    let mut out = String::new();
    for (k, v) in pairs {
        // Every key that matches the field exactly or under folding lands
        // in it, in order.
        if !equal_fold(&k, field) {
            continue;
        }
        let t = v.get();
        if t == "null" {
            continue;
        }
        out = serde_json::from_str::<String>(t).map_err(|_| ())?;
    }
    Ok(out)
}

/// One reply. raw_id and rpc_code never reach a native line.
#[derive(Debug, Clone)]
pub struct Response {
    pub id: String,
    pub ok: bool,
    pub result: Option<Value>,
    pub error: Option<(String, String)>,
    pub raw_id: Option<String>,
    pub rpc_code: i64,
}

pub fn ok_resp(id: &str, result: Value) -> Response {
    Response {
        id: id.to_string(),
        ok: true,
        result: Some(result),
        error: None,
        raw_id: None,
        rpc_code: 0,
    }
}

/// An error reply. A code outside the enum becomes internal with the
/// invented code in the message, so the bug is visible.
pub fn err_resp(id: &str, code: &str, msg: &str) -> Response {
    let (code, msg) = if valid_code(code) {
        (code.to_string(), msg.to_string())
    } else {
        (ERR_INTERNAL.to_string(), format!("{code}: {msg}"))
    };
    Response {
        id: id.to_string(),
        ok: false,
        result: None,
        error: Some((code, msg)),
        raw_id: None,
        rpc_code: 0,
    }
}

/// Why decode_line refused a line, with the JSON-RPC code to answer with.
#[derive(Debug)]
pub struct DecodeError {
    pub code: i64,
    pub message: String,
}

const NOT_JSON: &str = "this line is not JSON. Send one JSON object per line.";

fn is_jsonrpc(m: &BTreeMap<String, Box<RawValue>>) -> bool {
    m.get("jsonrpc")
        .map(|r| r.get() == "\"2.0\"")
        .unwrap_or(false)
}

/// Turns one wire line into a request and the framing it used. A refused
/// JSON-RPC line still returns the raw id a reply must use: the request's
/// own when it had a usable one, and null when not.
pub fn decode_line(line: &str) -> (Request, Framing, Option<DecodeError>) {
    let line = line.trim_matches(|c: char| c.is_ascii_whitespace());
    // Go decodes null into a map as a nil map: a native request with no
    // fields at all.
    if line == "null" {
        return (Request::default(), Framing::Native, None);
    }
    let m: BTreeMap<String, Box<RawValue>> = match serde_json::from_str(line) {
        Ok(m) => m,
        Err(_) => {
            let arr: Vec<Box<RawValue>> = match serde_json::from_str(line) {
                Ok(a) => a,
                Err(_) => {
                    return (
                        Request::default(),
                        Framing::Native,
                        Some(DecodeError {
                            code: RPC_PARSE_ERROR,
                            message: NOT_JSON.into(),
                        }),
                    )
                }
            };
            let mut f = Framing::Native;
            for el in arr {
                if let Ok(em) = serde_json::from_str::<BTreeMap<String, Box<RawValue>>>(el.get()) {
                    if is_jsonrpc(&em) {
                        f = Framing::JsonRpc;
                        break;
                    }
                }
            }
            let r = Request {
                raw_id: Some("null".into()),
                ..Default::default()
            };
            return (
                r,
                f,
                Some(DecodeError {
                    code: RPC_INVALID_REQUEST,
                    message: "coppice does not accept batches. Send one request per line.".into(),
                }),
            );
        }
    };
    if !is_jsonrpc(&m) {
        return (Request::from_map(m), Framing::Native, None);
    }
    let mut r = Request {
        raw_id: Some("null".into()),
        ..Default::default()
    };
    let id_raw = match m.get("id") {
        Some(raw) if raw.get() != "null" => raw,
        _ => {
            return (
                r,
                Framing::JsonRpc,
                Some(DecodeError {
                    code: RPC_INVALID_REQUEST,
                    message: "coppice does not accept notifications, send an id".into(),
                }),
            )
        }
    };
    let t = id_raw.get();
    if t.starts_with('"') {
        match serde_json::from_str::<String>(t) {
            Ok(s) => r.id = s,
            Err(_) => return (r, Framing::JsonRpc, Some(bad_id())),
        }
    } else if t.starts_with(|c: char| c == '-' || c.is_ascii_digit()) {
        r.id = t.to_string();
    } else {
        return (r, Framing::JsonRpc, Some(bad_id()));
    }
    r.raw_id = Some(t.to_string());
    let method = m
        .get("method")
        .and_then(|raw| serde_json::from_str::<String>(raw.get()).ok())
        .unwrap_or_default();
    if method.is_empty() {
        return (
            r,
            Framing::JsonRpc,
            Some(DecodeError {
                code: RPC_INVALID_REQUEST,
                message: "method must be a non-empty string".into(),
            }),
        );
    }
    r.cmd = method;
    if let Some(p) = m.get("params") {
        if p.get() != "null" {
            match serde_json::from_str::<BTreeMap<String, Box<RawValue>>>(p.get()) {
                Ok(pm) => r.params = pm,
                Err(_) => {
                    return (
                        r,
                        Framing::JsonRpc,
                        Some(DecodeError {
                            code: RPC_INVALID_REQUEST,
                            message: "params must be a JSON object".into(),
                        }),
                    )
                }
            }
        }
    }
    (r, Framing::JsonRpc, None)
}

fn bad_id() -> DecodeError {
    DecodeError {
        code: RPC_INVALID_REQUEST,
        message: "id must be a string or a number".into(),
    }
}

/// One reply in the given framing, without its newline.
pub fn encode_response(r: &Response, f: Framing) -> String {
    let mut out = String::new();
    if f == Framing::Native {
        out.push_str("{\"id\":");
        gojson::push_str(&mut out, &r.id);
        out.push_str(if r.ok {
            ",\"ok\":true"
        } else {
            ",\"ok\":false"
        });
        if let Some(res) = &r.result {
            // Go's omitempty drops a nil result only; a reply always has
            // one when ok.
            if !res.is_null() {
                out.push_str(",\"result\":");
                gojson::push(&mut out, res);
            }
        }
        if let Some((code, msg)) = &r.error {
            out.push_str(",\"error\":{\"code\":");
            gojson::push_str(&mut out, code);
            out.push_str(",\"message\":");
            gojson::push_str(&mut out, msg);
            out.push('}');
        }
        out.push('}');
        return out;
    }
    let id = match &r.raw_id {
        Some(raw) if !raw.is_empty() => gojson::compact(raw),
        _ => {
            let mut s = String::new();
            gojson::push_str(&mut s, &r.id);
            s
        }
    };
    out.push_str("{\"jsonrpc\":\"2.0\",\"id\":");
    out.push_str(&id);
    if r.ok {
        out.push_str(",\"result\":");
        match &r.result {
            Some(v) => gojson::push(&mut out, v),
            None => out.push_str("null"),
        }
        out.push('}');
        return out;
    }
    let code = if r.rpc_code == 0 {
        RPC_SERVER_ERROR
    } else {
        r.rpc_code
    };
    let (dcode, msg) = match &r.error {
        Some((c, m)) => (c.clone(), m.clone()),
        None => (
            ERR_INTERNAL.to_string(),
            "the server refused this request".to_string(),
        ),
    };
    out.push_str(",\"error\":{\"code\":");
    out.push_str(&code.to_string());
    out.push_str(",\"message\":");
    gojson::push_str(&mut out, &msg);
    out.push_str(",\"data\":{\"code\":");
    gojson::push_str(&mut out, &dcode);
    out.push_str("}}}");
    out
}

/// One event in the given framing, without its newline. JSON-RPC lifts the
/// event field into method and puts the other fields under params. None
/// when the event names no event.
pub fn encode_event(ev: &Value, f: Framing) -> Option<String> {
    if f == Framing::Native {
        return Some(gojson::marshal(ev));
    }
    let m = ev.as_object()?;
    let method = m.get("event")?.as_str()?;
    if method.is_empty() {
        return None;
    }
    // Go decodes the event into a map, so params come out sorted.
    let mut params: Vec<(&str, Value)> = m
        .iter()
        .filter(|(k, _)| k.as_str() != "event")
        .map(|(k, v)| (k.as_str(), v.clone()))
        .collect();
    params.sort_by(|a, b| a.0.cmp(b.0));
    let mut out = String::from("{\"jsonrpc\":\"2.0\",\"method\":");
    gojson::push_str(&mut out, method);
    out.push_str(",\"params\":");
    gojson::push(&mut out, &gojson::map(params));
    out.push('}');
    Some(out)
}

/// Reads one line at a time, without its newline. A line over MAX_LINE is
/// an error and the reader is finished.
pub struct Decoder<R: BufRead> {
    r: R,
}

impl<R: BufRead> Decoder<R> {
    pub fn new(r: R) -> Self {
        Decoder { r }
    }

    pub fn next_line(&mut self) -> io::Result<Vec<u8>> {
        let mut buf = Vec::new();
        loop {
            let (done, used) = {
                let avail = self.r.fill_buf()?;
                if avail.is_empty() {
                    if buf.is_empty() {
                        return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "eof"));
                    }
                    // Go's ReadLine returns a last line with no newline.
                    return Ok(buf);
                }
                match avail.iter().position(|&b| b == b'\n') {
                    Some(i) => {
                        buf.extend_from_slice(&avail[..i]);
                        (true, i + 1)
                    }
                    None => {
                        buf.extend_from_slice(avail);
                        (false, avail.len())
                    }
                }
            };
            self.r.consume(used);
            if buf.len() > MAX_LINE {
                return Err(io::Error::other(format!(
                    "line is over {MAX_LINE} bytes, send a smaller request"
                )));
            }
            if done {
                if buf.last() == Some(&b'\r') {
                    buf.pop();
                }
                return Ok(buf);
            }
        }
    }
}

/// Writes one line at a time under a lock, so the frame pump, the event
/// pump and the request handlers never interleave inside a line.
pub struct Encoder<W: Write> {
    w: Mutex<W>,
}

impl<W: Write> Encoder<W> {
    pub fn new(w: W) -> Self {
        Encoder { w: Mutex::new(w) }
    }

    pub fn send_line(&self, line: &str) -> io::Result<()> {
        let mut w = self.w.lock().unwrap_or_else(|e| e.into_inner());
        let mut b = Vec::with_capacity(line.len() + 1);
        b.extend_from_slice(line.as_bytes());
        b.push(b'\n');
        w.write_all(&b)?;
        w.flush()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_line_reads_id_cmd_and_params() {
        let (r, f, e) = decode_line(r#"{"id":"1","cmd":"pane.read","pane":"w1:p1","n":3}"#);
        assert!(e.is_none());
        assert_eq!(f, Framing::Native);
        assert_eq!(r.id, "1");
        assert_eq!(r.cmd, "pane.read");
        assert_eq!(r.str("pane").as_deref(), Some("w1:p1"));
        assert_eq!(r.int("n"), Some(3));
        assert_eq!(r.str("n"), None);
    }

    #[test]
    fn jsonrpc_refusals_carry_null_ids() {
        let (r, f, e) = decode_line(r#"{"jsonrpc":"2.0","method":"server.status"}"#);
        assert_eq!(f, Framing::JsonRpc);
        assert_eq!(r.raw_id.as_deref(), Some("null"));
        assert_eq!(e.unwrap().code, RPC_INVALID_REQUEST);
        let (_, f, e) = decode_line(r#"[{"jsonrpc":"2.0","id":8,"method":"x"}]"#);
        assert_eq!(f, Framing::JsonRpc);
        assert!(e.unwrap().message.contains("batches"));
        let (_, f, e) = decode_line("nope");
        assert_eq!(f, Framing::Native);
        assert_eq!(e.unwrap().code, RPC_PARSE_ERROR);
    }

    #[test]
    fn replies_encode_in_both_framings() {
        let mut r = err_resp("7", ERR_BAD_REQUEST, "x");
        assert_eq!(
            encode_response(&r, Framing::Native),
            r#"{"id":"7","ok":false,"error":{"code":"bad_request","message":"x"}}"#
        );
        r.raw_id = Some("7".into());
        r.rpc_code = RPC_METHOD_NOT_FOUND;
        assert_eq!(
            encode_response(&r, Framing::JsonRpc),
            r#"{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"x","data":{"code":"bad_request"}}}"#
        );
        let bad = err_resp("1", "made_up", "m");
        assert_eq!(bad.error.unwrap(), ("internal".into(), "made_up: m".into()));
    }
}
