//! `gateway_asgi.make_gateway_app`: the proxy itself, on a small HTTP/1.1
//! server with a thread for each connection.
//!
//! A request body is routed as the oracle routes it and sent upstream; the
//! answer comes back with the upstream's headers less the hop-by-hop ones.
//! A buffered answer is read whole, then sent; a stream passes through as
//! it arrives, each read written out at once. The upstream call is never
//! tied to the client connection: a client that goes away does not stop
//! the turn, which is read to its end and journaled.

use std::io::{self, Read, Write};
use std::net::{Shutdown, TcpStream};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use super::http::{self, decoded};
use super::pipeline::{now_ns, prepare_bytes, record, stream_flag, Gateway, Prepared, MODE_RULES};
use super::pyops::{latin1, loads_bytes, Raise};
use super::route::estimate_prefix_tokens;
use super::sniff::{normalize_openai_usage, Sniffer};
use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

/// The header Switchyard names the served target in.
pub const SELECTED_MODEL_HEADER: &str = "x-model-router-selected-model";
const COUNT_TOKENS_PATH: &str = "/v1/messages/count_tokens";

/// Hop-by-hop and framing headers, never forwarded.
const DROP_REQUEST: &[&str] =
    &["host", "content-length", "connection", "keep-alive", "transfer-encoding", "upgrade", "te", "trailers", "proxy-connection"];
const DROP_RESPONSE: &[&str] = &[
    "content-length",
    "connection",
    "keep-alive",
    "transfer-encoding",
    "upgrade",
    "te",
    "trailers",
    "proxy-connection",
    "content-encoding",
];

pub type Shared = Arc<Mutex<Gateway>>;

/// `ConfigReloader`: the Anthropic wire's gateway, rebuilt from config.yaml
/// when the file changes (checked at most every two seconds, on a
/// request), on SIGHUP and on POST /_reload.
pub struct Reloader {
    pub path: String,
    pub rebuild: Box<dyn Fn() -> Option<Gateway> + Send + Sync>,
    pub min_interval: Duration,
    st: Mutex<ReloadState>,
}

#[derive(Default)]
struct ReloadState {
    built: Option<Shared>,
    mtime: Option<i128>,
    failed_mtime: Option<i128>,
    checked_at: Option<Instant>,
}

impl Reloader {
    pub fn new(path: String, rebuild: Box<dyn Fn() -> Option<Gateway> + Send + Sync>, min_interval: Duration) -> Reloader {
        Reloader { path, rebuild, min_interval, st: Mutex::new(ReloadState::default()) }
    }

    fn stat_mtime(&self) -> i128 {
        std::fs::metadata(&self.path)
            .and_then(|m| m.modified())
            .ok()
            .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
            .map(|d| d.as_nanos() as i128)
            .unwrap_or(0)
    }

    fn load(&self, st: &mut ReloadState) -> bool {
        let m = self.stat_mtime();
        if st.built.is_some() && (st.mtime == Some(m) || st.failed_mtime == Some(m)) {
            return false;
        }
        match (self.rebuild)() {
            None => {
                st.failed_mtime = Some(m);
                false
            }
            Some(g) => {
                st.built = Some(Arc::new(Mutex::new(g)));
                st.mtime = Some(m);
                st.failed_mtime = None;
                true
            }
        }
    }

    /// `ConfigReloader.current()`.
    pub fn current(&self) -> Option<Shared> {
        let Ok(mut st) = self.st.lock() else { return None };
        let due = st.checked_at.is_none_or(|t| t.elapsed() >= self.min_interval);
        if st.built.is_none() || due {
            st.checked_at = Some(Instant::now());
            self.load(&mut st);
        }
        st.built.clone()
    }

    /// `ConfigReloader.reload()`: true when it rebuilt.
    pub fn reload(&self) -> bool {
        let Ok(mut st) = self.st.lock() else { return false };
        st.checked_at = Some(Instant::now());
        st.mtime = None;
        st.failed_mtime = None;
        self.load(&mut st)
    }
}

/// The gateway app.
pub struct Server {
    /// Serves the Anthropic wire when the reloader holds nothing.
    pub anthropic: Shared,
    pub reloader: Option<Arc<Reloader>>,
    /// Routes the OpenAI wire; None passes that wire through unrouted and
    /// unmetered.
    pub openai: Option<Shared>,
    pub upstream_base: String,
    pub openai_base: String,
    pub upstream_kind: String,
}

/// `is_openai_wire`.
pub fn is_openai_wire(path: &str) -> bool {
    path.trim_end_matches('/').ends_with("/chat/completions")
}

/// One request as the server read it.
pub struct Request {
    pub method: String,
    pub path: String,
    pub query: String,
    pub headers: Vec<(String, Vec<u8>)>,
    pub body: Vec<u8>,
    pub peer: String,
}

/// What the app writes back.
pub struct Out<'a> {
    s: &'a mut TcpStream,
    /// The client went away: nothing more is written, the turn goes on.
    gone: bool,
    started: bool,
    chunked: bool,
    pub close: bool,
}

fn reason(status: u16) -> &'static str {
    match status {
        200 => "OK",
        201 => "Created",
        204 => "No Content",
        301 => "Moved Permanently",
        302 => "Found",
        304 => "Not Modified",
        307 => "Temporary Redirect",
        400 => "Bad Request",
        401 => "Unauthorized",
        403 => "Forbidden",
        404 => "Not Found",
        405 => "Method Not Allowed",
        413 => "Request Entity Too Large",
        429 => "Too Many Requests",
        500 => "Internal Server Error",
        502 => "Bad Gateway",
        503 => "Service Unavailable",
        529 => "Site is overloaded",
        _ => "",
    }
}

impl Out<'_> {
    fn raw(&mut self, b: &[u8]) {
        if !self.gone && self.s.write_all(b).is_err() {
            self.gone = true;
        }
    }

    fn head(&mut self, status: u16, headers: &[(String, Vec<u8>)], length: Option<usize>) {
        let mut b = format!("HTTP/1.1 {status} {}\r\n", reason(status)).into_bytes();
        for (k, v) in headers {
            b.extend_from_slice(k.as_bytes());
            b.extend_from_slice(b": ");
            b.extend_from_slice(v);
            b.extend_from_slice(b"\r\n");
        }
        match length {
            Some(n) => b.extend_from_slice(format!("content-length: {n}\r\n").as_bytes()),
            None => b.extend_from_slice(b"transfer-encoding: chunked\r\n"),
        }
        if self.close {
            b.extend_from_slice(b"connection: close\r\n");
        }
        b.extend_from_slice(b"\r\n");
        self.started = true;
        self.chunked = length.is_none();
        self.raw(&b);
    }

    /// A whole answer in one write.
    fn whole(&mut self, status: u16, headers: &[(String, Vec<u8>)], body: &[u8]) {
        let mut b = format!("HTTP/1.1 {status} {}\r\n", reason(status)).into_bytes();
        for (k, v) in headers {
            b.extend_from_slice(k.as_bytes());
            b.extend_from_slice(b": ");
            b.extend_from_slice(v);
            b.extend_from_slice(b"\r\n");
        }
        b.extend_from_slice(format!("content-length: {}\r\n", body.len()).as_bytes());
        if self.close {
            b.extend_from_slice(b"connection: close\r\n");
        }
        b.extend_from_slice(b"\r\n");
        b.extend_from_slice(body);
        self.started = true;
        self.raw(&b);
    }

    fn chunk(&mut self, data: &[u8]) {
        if data.is_empty() {
            return;
        }
        let mut b = format!("{:x}\r\n", data.len()).into_bytes();
        b.extend_from_slice(data);
        b.extend_from_slice(b"\r\n");
        self.raw(&b);
    }

    fn end(&mut self) {
        if self.chunked {
            self.raw(b"0\r\n\r\n");
        }
    }

    /// Drops the connection mid answer, as the server does when the app
    /// raises after the answer started.
    fn abort(&mut self) {
        self.close = true;
        self.gone = true;
        let _ = self.s.shutdown(Shutdown::Both);
    }
}

fn send_json(out: &mut Out, status: u16, payload: &Object) {
    let body = pyjson::dumps(&Value::Obj(payload.clone()), true);
    out.whole(status, &[("content-type".into(), b"application/json".to_vec())], body.as_bytes());
}

fn err_obj(msg: &str) -> Object {
    let mut o = Object::new();
    o.set("error", msg);
    o
}

/// The server's answer when the app raised before the answer started.
fn internal_error(out: &mut Out) {
    out.close = true;
    out.whole(500, &[("content-type".into(), b"text/plain; charset=utf-8".to_vec())], b"Internal Server Error");
}

/// The loopback check the oracle makes on the client's host.
fn is_loopback(peer: &str) -> bool {
    peer == "127.0.0.1" || peer == "::1" || peer == "localhost"
}

fn forward_headers(r: &Request) -> Vec<(String, Vec<u8>)> {
    let mut h: Vec<(String, Vec<u8>)> = vec![];
    for (k, v) in &r.headers {
        let lk = k.to_ascii_lowercase();
        if DROP_REQUEST.contains(&lk.as_str()) {
            continue;
        }
        h.push((lk, v.clone()));
    }
    // httpx's own defaults, where the client sent none.
    for (k, v) in [("accept", "*/*"), ("accept-encoding", "gzip, deflate"), ("user-agent", "opendaisugi-gateway")] {
        if !h.iter().any(|(n, _)| n == k) {
            h.push((k.into(), v.as_bytes().to_vec()));
        }
    }
    h
}

/// The upstream's headers less the hop-by-hop ones, each name once with
/// its values joined by ", ", as httpx lists them.
fn response_headers(hs: &[(String, Vec<u8>)]) -> Vec<(String, Vec<u8>)> {
    let mut out: Vec<(String, Vec<u8>)> = vec![];
    for (k, v) in hs {
        let lk = k.to_ascii_lowercase();
        if DROP_RESPONSE.contains(&lk.as_str()) {
            continue;
        }
        match out.iter_mut().find(|(n, _)| *n == lk) {
            Some((_, have)) => {
                have.extend_from_slice(b", ");
                have.extend_from_slice(v);
            }
            None => out.push((lk, v.clone())),
        }
    }
    out
}

fn values_of<'a>(hs: &'a [(String, Vec<u8>)], name: &str) -> Vec<&'a [u8]> {
    hs.iter().filter(|(k, _)| k.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_slice()).collect()
}

/// `_served_target`: the header (its values joined) and the body's model
/// must both be present and agree.
fn served_target(header: &[&[u8]], body_model: Option<&str>) -> Option<String> {
    if header.is_empty() {
        return None;
    }
    let model = body_model?;
    let h = latin1(&header.join(&b", "[..]));
    if h.is_empty() || model.is_empty() {
        return None;
    }
    let h = text::strip(&h).to_string();
    (h == text::strip(model)).then_some(h)
}

/// `_extract_buffered_text`.
fn buffered_text(o: &Object) -> String {
    let content = match o.get("content") {
        None => return String::new(),
        Some(Value::List(l)) => l,
        Some(_) => return String::new(),
    };
    let mut b = String::new();
    for block in content {
        let Value::Obj(bo) = block else { continue };
        if !super::pyops::eq_str(bo.get("type"), "text") {
            continue;
        }
        match bo.get("text") {
            None => {}
            Some(Value::Str(t)) => b.push_str(t),
            Some(_) => return String::new(),
        }
    }
    b
}

/// `extract_openai_text`.
fn openai_text(o: &Object) -> String {
    let Some(choices) = o.get("choices").filter(|c| c.truthy()) else { return String::new() };
    let Value::List(c) = choices else { return String::new() };
    let Value::Obj(first) = &c[0] else { return String::new() };
    let empty = Value::Obj(Object::new());
    let msg = first.get("message").filter(|m| m.truthy()).unwrap_or(&empty);
    let Value::Obj(mo) = msg else { return String::new() };
    match mo.get("content") {
        Some(Value::Str(t)) => t.clone(),
        _ => String::new(),
    }
}

fn anthropic_error(msg: &str) -> Object {
    let mut inner = Object::new();
    inner.set("type", "invalid_request_error");
    inner.set("message", msg);
    let mut o = Object::new();
    o.set("type", "error");
    o.set("error", Value::Obj(inner));
    o
}

impl Server {
    /// The ASGI app for one request.
    pub fn serve(&self, r: &Request, out: &mut Out) {
        if r.path == "/_reload" {
            match &self.reloader {
                None => send_json(out, 404, &err_obj("no reloader configured")),
                Some(_) if !r.method.eq_ignore_ascii_case("POST") => send_json(out, 405, &err_obj("reload accepts POST")),
                Some(_) if !is_loopback(&r.peer) => send_json(out, 403, &err_obj("reload is loopback only")),
                Some(rl) => {
                    let mut o = Object::new();
                    o.set("reloaded", rl.reload());
                    send_json(out, 200, &o);
                }
            }
            return;
        }
        let openai = is_openai_wire(&r.path);
        let gw: Option<Shared> = if openai {
            self.openai.clone()
        } else {
            let mut g = Some(self.anthropic.clone());
            if let Some(rl) = &self.reloader {
                if let Some(cur) = rl.current() {
                    g = Some(cur);
                }
            }
            g
        };
        let count_tokens = r.path.trim_end_matches('/') == COUNT_TOKENS_PATH;
        if !openai && self.upstream_kind != "anthropic" && count_tokens {
            count_tokens_locally(out, &r.body);
            return;
        }
        let (outbound, prepared, stream) = match &gw {
            Some(g) => match g.lock() {
                Ok(mut g) => prepare_bytes(&mut g, &r.body),
                Err(_) => (r.body.clone(), None, false),
            },
            None => (r.body.clone(), None, stream_flag(&r.body)),
        };
        let mut rec = prepared.clone();
        if let Some(g) = &gw {
            if count_tokens && g.lock().map(|g| g.router_mode != MODE_RULES).unwrap_or(true) {
                rec = None;
            }
        }
        let base = if openai { &self.openai_base } else { &self.upstream_base };
        let mut url = format!("{}{}", base.trim_end_matches('/'), r.path);
        if !r.query.is_empty() {
            url.push('?');
            url.push_str(&r.query);
        }
        let mut attempts: Vec<(&[u8], bool)> = vec![(&outbound, false)];
        if prepared.as_ref().is_some_and(|p| p.decision.downgraded) {
            attempts.push((&r.body, true));
        }
        let headers = forward_headers(r);
        let n = attempts.len();
        for (i, (body, original)) in attempts.into_iter().enumerate() {
            let last = i == n - 1;
            let resp = match http::post(&url, &headers, body) {
                Ok(resp) => resp,
                Err(_) => {
                    internal_error(out);
                    return;
                }
            };
            if resp.status >= 400 && !last {
                // The answer is dropped with its connection.
                continue;
            }
            if stream {
                self.relay_stream(out, resp, gw.as_ref(), rec.as_ref(), original, openai);
            } else {
                self.relay_buffered(out, resp, gw.as_ref(), rec.as_ref(), original, openai);
            }
            return;
        }
    }

    fn relay_buffered(&self, out: &mut Out, resp: http::Response, gw: Option<&Shared>, p: Option<&Prepared>, original: bool, openai: bool) {
        let (status, hs, mut rd) = decoded(resp);
        let mut content = vec![];
        if rd.read_to_end(&mut content).is_err() {
            internal_error(out);
            return;
        }
        out.whole(status, &response_headers(&hs), &content);
        let mut usage = Value::Obj(Object::new());
        let mut answer = String::new();
        let mut body_model: Option<String> = None;
        match loads_bytes(&content) {
            // resp.json() raised RecursionError after the answer was sent:
            // the turn is not journaled.
            Err(Raise::TooDeep) => return,
            Ok(Value::Obj(o)) => {
                if openai {
                    usage = Value::Obj(normalize_openai_usage(o.get("usage")));
                    answer = openai_text(&o);
                } else {
                    let u = o.get("usage").cloned().unwrap_or_else(|| Value::Obj(Object::new()));
                    usage = if u.truthy() { u } else { Value::Obj(Object::new()) };
                    answer = buffered_text(&o);
                    if let Some(Value::Str(m)) = o.get("model") {
                        body_model = Some(m.clone());
                    }
                }
            }
            _ => {}
        }
        let served = served_target(&values_of(&hs, SELECTED_MODEL_HEADER), body_model.as_deref());
        self.record(gw, p, &usage, original, &answer, served.as_deref());
    }

    fn relay_stream(&self, out: &mut Out, resp: http::Response, gw: Option<&Shared>, p: Option<&Prepared>, original: bool, openai: bool) {
        let mut sn = Sniffer::new(openai);
        let (status, hs, mut rd) = decoded(resp);
        out.head(status, &response_headers(&hs), None);
        let mut buf = vec![0u8; 64 * 1024];
        loop {
            match rd.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => {
                    sn.feed(&buf[..n]);
                    if sn.raised {
                        out.abort();
                        return;
                    }
                    // A client that went away gets nothing more; the turn is
                    // still read to its end and journaled.
                    out.chunk(&buf[..n]);
                }
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(_) => {
                    // The upstream cut the stream after the answer started:
                    // the oracle's app raises and the connection is dropped.
                    out.abort();
                    return;
                }
            }
        }
        out.end();
        let model = if openai { None } else { sn.model.clone() };
        let served = served_target(&values_of(&hs, SELECTED_MODEL_HEADER), model.as_deref());
        self.record(gw, p, &Value::Obj(sn.usage.clone()), original, &sn.text, served.as_deref());
    }

    fn record(&self, gw: Option<&Shared>, p: Option<&Prepared>, usage: &Value, original: bool, answer: &str, served: Option<&str>) {
        let (Some(gw), Some(p)) = (gw, p) else { return };
        if let Ok(g) = gw.lock() {
            record(&g, p, usage, original, answer, served, now_ns());
        }
    }
}

fn count_tokens_locally(out: &mut Out, body: &[u8]) {
    let o = match loads_bytes(body) {
        Err(Raise::TooDeep) => return internal_error(out),
        Ok(Value::Obj(o)) => o,
        _ => return send_json(out, 400, &anthropic_error("count_tokens needs a JSON object body")),
    };
    match estimate_prefix_tokens(&Value::Obj(o)) {
        Err(Raise::TooDeep) => internal_error(out),
        Err(_) => send_json(out, 400, &anthropic_error("count_tokens body has a malformed system or messages field")),
        Ok(n) => {
            let mut r = Object::new();
            r.set("input_tokens", n);
            send_json(out, 200, &r);
        }
    }
}

// ---------------------------------------------------------------------------
// The HTTP/1.1 server
// ---------------------------------------------------------------------------

/// The server's life: set when it drains, and how many requests are in
/// flight.
#[derive(Default)]
pub struct Life {
    pub draining: AtomicBool,
    pub busy: AtomicUsize,
    pub open: AtomicUsize,
}

enum ReadErr {
    /// The client closed, or the server drains, before a request began.
    Idle,
    /// A request that is not HTTP this server reads.
    Bad,
}

/// Reads one request; an idle keep-alive connection is closed when the
/// server drains.
fn read_request(s: &mut TcpStream, buf: &mut Vec<u8>, life: &Life, peer: &str) -> Result<Request, ReadErr> {
    let _ = s.set_read_timeout(Some(Duration::from_millis(200)));
    let mut tmp = [0u8; 65536];
    let head_end = loop {
        if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
            break i;
        }
        if buf.len() > 1 << 20 {
            return Err(ReadErr::Bad);
        }
        match s.read(&mut tmp) {
            Ok(0) => return Err(ReadErr::Idle),
            Ok(n) => buf.extend_from_slice(&tmp[..n]),
            Err(e) if matches!(e.kind(), io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut) => {
                if buf.is_empty() && life.draining.load(Ordering::SeqCst) {
                    return Err(ReadErr::Idle);
                }
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
            Err(_) => return Err(ReadErr::Idle),
        }
    };
    let _ = s.set_read_timeout(None);
    let head: Vec<u8> = buf.drain(..head_end + 4).collect();
    let head = &head[..head_end];
    let mut lines = head.split(|&c| c == b'\n').map(|l| l.strip_suffix(b"\r").unwrap_or(l));
    let first = String::from_utf8_lossy(lines.next().ok_or(ReadErr::Bad)?).into_owned();
    let mut parts = first.split(' ');
    let method = parts.next().unwrap_or("").to_string();
    let target = parts.next().unwrap_or("").to_string();
    if method.is_empty() || !target.starts_with('/') || !target.bytes().all(|c| (0x21..0x7f).contains(&c)) {
        return Err(ReadErr::Bad);
    }
    // The path as the app sees it: percent-decoded, as uvicorn gives it.
    let (path, query) = match target.split_once('?') {
        Some((p, q)) => (http::unquote(p), q.to_string()),
        None => (http::unquote(&target), String::new()),
    };
    let mut headers = vec![];
    for l in lines {
        let Some(i) = l.iter().position(|&c| c == b':') else { continue };
        let name = String::from_utf8_lossy(&l[..i]).trim().to_string();
        let mut v = &l[i + 1..];
        while v.first().is_some_and(|c| *c == b' ' || *c == b'\t') {
            v = &v[1..];
        }
        while v.last().is_some_and(|c| *c == b' ' || *c == b'\t') {
            v = &v[..v.len() - 1];
        }
        headers.push((name, v.to_vec()));
    }
    let hv = |n: &str| -> Option<String> {
        headers.iter().rev().find(|(k, _)| k.eq_ignore_ascii_case(n)).map(|(_, v)| String::from_utf8_lossy(v).to_ascii_lowercase())
    };
    if hv("expect").as_deref() == Some("100-continue") {
        let _ = s.write_all(b"HTTP/1.1 100 Continue\r\n\r\n");
    }
    let mut body = vec![];
    let fill = |s: &mut TcpStream, buf: &mut Vec<u8>| -> Result<(), ReadErr> {
        let mut tmp = [0u8; 65536];
        match s.read(&mut tmp) {
            Ok(0) => Err(ReadErr::Bad),
            Ok(n) => {
                buf.extend_from_slice(&tmp[..n]);
                Ok(())
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => Ok(()),
            Err(_) => Err(ReadErr::Bad),
        }
    };
    if hv("transfer-encoding").is_some_and(|v| v.contains("chunked")) {
        loop {
            let i = loop {
                if let Some(i) = buf.windows(2).position(|w| w == b"\r\n") {
                    break i;
                }
                fill(s, buf)?;
            };
            let line = String::from_utf8_lossy(&buf[..i]).into_owned();
            buf.drain(..i + 2);
            let n = usize::from_str_radix(line.split(';').next().unwrap_or("").trim(), 16).map_err(|_| ReadErr::Bad)?;
            if n == 0 {
                loop {
                    let j = loop {
                        if let Some(j) = buf.windows(2).position(|w| w == b"\r\n") {
                            break j;
                        }
                        fill(s, buf)?;
                    };
                    buf.drain(..j + 2);
                    if j == 0 {
                        break;
                    }
                }
                break;
            }
            while buf.len() < n + 2 {
                fill(s, buf)?;
            }
            body.extend_from_slice(&buf[..n]);
            buf.drain(..n + 2);
        }
    } else if let Some(n) = hv("content-length") {
        let n: usize = n.trim().parse().map_err(|_| ReadErr::Bad)?;
        while buf.len() < n {
            fill(s, buf)?;
        }
        body = buf.drain(..n).collect();
    }
    Ok(Request { method, path, query, headers, body, peer: peer.to_string() })
}

/// Serves one connection until it closes, the client asks to close, or the
/// server drains.
pub fn handle(app: &Server, mut s: TcpStream, life: &Life) {
    let _ = s.set_nodelay(true);
    let peer = s.peer_addr().map(|a| a.ip().to_string()).unwrap_or_default();
    let mut buf = vec![];
    loop {
        let r = match read_request(&mut s, &mut buf, life, &peer) {
            Ok(r) => r,
            Err(ReadErr::Idle) => break,
            Err(ReadErr::Bad) => {
                let _ = s.write_all(b"HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
                break;
            }
        };
        life.busy.fetch_add(1, Ordering::SeqCst);
        let close = r
            .headers
            .iter()
            .any(|(k, v)| k.eq_ignore_ascii_case("connection") && String::from_utf8_lossy(v).to_ascii_lowercase().contains("close"));
        let mut out = Out { s: &mut s, gone: false, started: false, chunked: false, close };
        app.serve(&r, &mut out);
        let done = out.close || out.gone || !out.started;
        life.busy.fetch_sub(1, Ordering::SeqCst);
        if done || life.draining.load(Ordering::SeqCst) {
            break;
        }
    }
    let _ = s.shutdown(Shutdown::Both);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn answer_headers_drop_hop_by_hop_and_join_repeats() {
        let hs = vec![
            ("X-A".to_string(), b"1".to_vec()),
            ("Content-Encoding".to_string(), b"gzip".to_vec()),
            ("x-a".to_string(), b"2".to_vec()),
            ("Transfer-Encoding".to_string(), b"chunked".to_vec()),
        ];
        assert_eq!(response_headers(&hs), vec![("x-a".to_string(), b"1, 2".to_vec())]);
    }

    #[test]
    fn a_served_target_needs_the_header_and_the_body_to_agree() {
        assert_eq!(served_target(&[b" llama3 "], Some("llama3")), Some("llama3".into()));
        assert_eq!(served_target(&[b"llama3"], Some("other")), None);
        assert_eq!(served_target(&[], Some("llama3")), None);
        assert_eq!(served_target(&[b"llama3"], None), None);
    }

    #[test]
    fn only_the_exact_loopback_hosts_may_reload() {
        assert!(is_loopback("127.0.0.1") && is_loopback("::1"));
        assert!(!is_loopback("127.0.0.2") && !is_loopback("10.0.0.1"));
    }
}
