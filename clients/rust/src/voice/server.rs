//! `voice/server.py`: the voice bridge's HTTP server, reading requests as
//! `http.server` reads them, so its answers match the oracle's.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use num_bigint::BigInt;

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};
use crate::gateway::pyops;
use crate::llm::Client;

use super::arm::{deliver, extract_multipart_audio, latin1};
use super::audio::to_wav_16k_mono;
use super::cleanup::{clean_transcript, CleanupConfig, HttpTransport};
use super::coppice::{send_through_floor, FloorConfig, SendError};
use super::engine::Engine;
use super::pynum::py_int;
use super::ptt::read_token;
use super::wav::wav_duration_s;

pub const MAX_AUDIO_SECONDS: f64 = 60.0;
pub const MAX_CONTENT_LENGTH: i64 = 30_000_000;
pub const MAX_DELIVER_BYTES: i64 = 65536;
pub const MAX_DELIVER_TEXT_CHARS: usize = 20000;
const BAN_WINDOW: f64 = 60.0;
const BAN_THRESHOLD: usize = 3;
const READ_TIMEOUT: Duration = Duration::from_secs(30);
const NOT_FOUND: &str = "This server answers /health, /transcribe, and /deliver.";
const BAD_AUDIO: &str = "This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A.";

pub struct ServerConfig {
    pub home: String,
    pub token_file: String,
    pub armed_dir: String,
    pub engine: Engine,
    pub cleanup: CleanupConfig,
    pub floor: FloorConfig,
    pub llm: Client,
}

/// A failure before the server serves, with the exit code the command
/// line gives it: 3 for the oracle's RuntimeError, 1 for an OSError or a
/// ValueError.
pub struct StartError {
    pub code: i32,
    pub msg: String,
}

fn refuse_no_token(address: &str, token_file: &str) -> StartError {
    StartError {
        code: 3,
        msg: format!(
            "refusing to listen on {address} without a token. {token_file} is missing or unreadable. Run coppice web token first, or pass --token-file."
        ),
    }
}

pub struct Server {
    cfg: ServerConfig,
    listener: TcpListener,
    tls: Option<Arc<rustls::ServerConfig>>,
    failures: Mutex<HashMap<String, Vec<f64>>>,
}

fn now() -> f64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0)
}

fn tls_config(cert: &str, key: &str) -> Result<Arc<rustls::ServerConfig>, String> {
    use rustls::pki_types::pem::PemObject;
    use rustls::pki_types::{CertificateDer, PrivateKeyDer};
    let certs: Vec<CertificateDer<'static>> =
        CertificateDer::pem_file_iter(cert).map_err(|e| e.to_string())?.collect::<Result<_, _>>().map_err(|e| e.to_string())?;
    let key = PrivateKeyDer::from_pem_file(key).map_err(|e| e.to_string())?;
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let cfg = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .map_err(|e| e.to_string())?
        .with_no_client_auth()
        .with_single_cert(certs, key)
        .map_err(|e| e.to_string())?;
    Ok(Arc::new(cfg))
}

/// `str(OSError)` for a failed bind.
fn os_error_text(e: &std::io::Error) -> String {
    match e.raw_os_error() {
        Some(libc::EADDRINUSE) => "[Errno 98] Address already in use".into(),
        Some(libc::EACCES) => "[Errno 13] Permission denied".into(),
        Some(libc::EADDRNOTAVAIL) => "[Errno 99] Cannot assign requested address".into(),
        _ => e.to_string(),
    }
}

/// `build_server`: the token checked, the socket bound, the TLS pair
/// checked, the token checked again.
pub fn listen(host: &str, port: u16, cfg: ServerConfig, cert: Option<&str>, key: Option<&str>) -> Result<Server, StartError> {
    match read_token(&cfg.token_file) {
        Err(m) => return Err(StartError { code: 1, msg: m }),
        Ok(None) => return Err(refuse_no_token(host, &cfg.token_file)),
        Ok(Some(_)) => {}
    }
    let addr = if host.is_empty() { "0.0.0.0".to_string() } else { host.to_string() };
    let sa = std::net::ToSocketAddrs::to_socket_addrs(&(addr.as_str(), port))
        .ok()
        .and_then(|mut it| it.find(|a| a.is_ipv4()))
        .ok_or_else(|| StartError { code: 1, msg: "[Errno -2] Name or service not known".into() })?;
    let listener = TcpListener::bind(sa).map_err(|e| StartError { code: 1, msg: os_error_text(&e) })?;
    let mut tls = None;
    if cert.is_some() || key.is_some() {
        let (Some(c), Some(k)) = (cert, key) else {
            drop(listener);
            return Err(StartError { code: 1, msg: "pass both --tls-cert and --tls-key, or pass neither".into() });
        };
        tls = Some(tls_config(c, k).map_err(|m| StartError { code: 1, msg: m })?);
    }
    let bound = listener.local_addr().map(|a| a.ip().to_string()).unwrap_or_default();
    match read_token(&cfg.token_file) {
        Err(m) => return Err(StartError { code: 1, msg: m }),
        Ok(None) => return Err(refuse_no_token(&bound, &cfg.token_file)),
        Ok(Some(_)) => {}
    }
    Ok(Server { cfg, listener, tls, failures: Mutex::new(HashMap::new()) })
}

/// A connection, plain or TLS.
trait Conn: Read + Write + Send {}
impl<T: Read + Write + Send> Conn for T {}

struct Request {
    method: String,
    path: String,
    headers: Vec<(String, String)>,
    address: String,
}

impl Request {
    /// `self.headers.get(name)`: the first value, case-insensitive.
    fn header(&self, name: &str) -> Option<&str> {
        self.headers.iter().find(|(n, _)| n.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_str())
    }
}

struct Responder<'a> {
    w: &'a mut dyn Write,
    method: String,
}

fn status_text(code: u16) -> &'static str {
    match code {
        200 => "OK",
        400 => "Bad Request",
        401 => "Unauthorized",
        403 => "Forbidden",
        404 => "Not Found",
        411 => "Length Required",
        413 => "Request Entity Too Large",
        429 => "Too Many Requests",
        500 => "Internal Server Error",
        501 => "Not Implemented",
        503 => "Service Unavailable",
        _ => "",
    }
}

fn explain(code: u16) -> &'static str {
    match code {
        400 => "Bad request syntax or unsupported method",
        414 => "Requested URI is too long",
        431 => "The server refused this request because the request header fields are too large",
        501 => "Server does not support this operation",
        505 => "Cannot fulfill request",
        _ => "",
    }
}

fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;")
}

impl Responder<'_> {
    fn send(&mut self, code: u16, ctype: &str, body: &[u8]) {
        let head = format!(
            "HTTP/1.0 {code} {}\r\nServer: opendaisugi-voice/1\r\nContent-Type: {ctype}\r\nContent-Length: {}\r\n\r\n",
            status_text(code),
            body.len()
        );
        let _ = self.w.write_all(head.as_bytes());
        if self.method != "HEAD" {
            let _ = self.w.write_all(body);
        }
        let _ = self.w.flush();
    }

    fn json(&mut self, code: u16, o: Object) {
        let body = pyjson::dumps(&Value::Obj(o), true);
        self.send(code, "application/json", body.as_bytes());
    }

    fn error(&mut self, code: u16, error: &str, message: &str) {
        self.json(code, obj(&[("error", error.into()), ("message", message.into())]));
    }

    fn send_error(&mut self, code: u16, message: &str) {
        let body = format!(
            "<!DOCTYPE HTML>\n<html lang=\"en\">\n    <head>\n        <meta charset=\"utf-8\">\n        <title>Error response</title>\n    </head>\n    <body>\n        <h1>Error response</h1>\n        <p>Error code: {code}</p>\n        <p>Message: {}.</p>\n        <p>Error code explanation: {code} - {}.</p>\n    </body>\n</html>\n",
            html_escape(message),
            explain(code)
        );
        let head = format!(
            "HTTP/1.0 {code} {message}\r\nServer: opendaisugi-voice/1\r\nConnection: close\r\nContent-Type: text/html;charset=utf-8\r\nContent-Length: {}\r\n\r\n",
            body.len()
        );
        let _ = self.w.write_all(head.as_bytes());
        if self.method != "HEAD" {
            let _ = self.w.write_all(body.as_bytes());
        }
        let _ = self.w.flush();
    }
}

fn obj(kv: &[(&str, Value)]) -> Object {
    let mut o = Object::new();
    for (k, v) in kv {
        o.set(k, v.clone());
    }
    o
}

/// `rfile.readline(limit)`.
fn read_line(r: &mut dyn BufRead, limit: usize) -> Vec<u8> {
    let mut out = vec![];
    let mut byte = [0u8; 1];
    while out.len() < limit {
        match r.read(&mut byte) {
            Ok(1) => {
                out.push(byte[0]);
                if byte[0] == b'\n' {
                    break;
                }
            }
            _ => break,
        }
    }
    out
}

fn http_version(v: &str) -> Option<u32> {
    let rest = v.strip_prefix("HTTP/")?;
    let parts: Vec<&str> = rest.split('.').collect();
    if parts.len() != 2 {
        return None;
    }
    let mut major = 0;
    for (i, p) in parts.iter().enumerate() {
        if p.is_empty() || p.chars().count() > 10 || !p.chars().all(text::is_digit) {
            return None;
        }
        let n = py_int(p)?;
        if i == 0 {
            major = u32::try_from(n).unwrap_or(u32::MAX);
        }
    }
    Some(major)
}

fn header_name(n: &str) -> bool {
    n.bytes().all(|c| (0x21..=0x7e).contains(&c) && c != b':')
}

/// `parse_headers` then the email parser: at most 100 lines of at most
/// 65536 bytes; headers end at the first line that is not one.
fn read_headers(r: &mut dyn BufRead) -> Result<Vec<(String, String)>, (u16, &'static str)> {
    let mut lines = vec![];
    loop {
        let raw = read_line(r, 65537);
        if raw.len() > 65536 {
            return Err((431, "Line too long"));
        }
        let end = raw.is_empty() || raw == b"\r\n" || raw == b"\n";
        lines.push(latin1(&raw));
        if lines.len() > 100 {
            return Err((431, "Too many headers"));
        }
        if end {
            break;
        }
    }
    let mut out: Vec<(String, String)> = vec![];
    for ln in lines {
        if ln.is_empty() || ln == "\r\n" || ln == "\n" {
            break;
        }
        if (ln.starts_with(' ') || ln.starts_with('\t')) && !out.is_empty() {
            let last = out.last_mut().expect("not empty");
            last.1.push_str(ln.trim_end_matches(['\r', '\n']));
            continue;
        }
        let Some(i) = ln.find(':') else { break };
        if i == 0 || !header_name(&ln[..i]) {
            break;
        }
        let v = ln[i + 1..].trim_start_matches([' ', '\t']).trim_end_matches(['\r', '\n']);
        out.push((ln[..i].to_string(), v.to_string()));
    }
    Ok(out)
}

fn scheme_like(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty() && b[0].is_ascii_alphabetic() && b.iter().all(|c| c.is_ascii_alphanumeric() || matches!(c, b'+' | b'-' | b'.'))
}

/// urlsplit's path and query.
fn url_path(raw: &str) -> (String, String) {
    let mut s = raw.split('#').next().unwrap_or("").to_string();
    let mut query = String::new();
    if let Some(i) = s.find('?') {
        query = s[i + 1..].to_string();
        s.truncate(i);
    }
    let netloc_path = |rest: &str| -> String { rest.find('/').map(|j| rest[j..].to_string()).unwrap_or_default() };
    if let Some(rest) = s.strip_prefix("//") {
        return (netloc_path(rest), query);
    }
    if let Some(i) = s.find(':') {
        if i > 0 && scheme_like(&s[..i]) {
            let after = &s[i + 1..];
            if let Some(rest) = after.strip_prefix("//") {
                return (netloc_path(rest), query);
            }
            return (after.to_string(), query);
        }
    }
    (s, query)
}

fn unquote_plus(s: &str) -> String {
    let s = s.replace('+', " ");
    let b = s.as_bytes();
    let mut out = vec![];
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() && b[i + 1].is_ascii_hexdigit() && b[i + 2].is_ascii_hexdigit() {
            out.push(u8::from_str_radix(&s[i + 1..i + 3], 16).unwrap_or(0));
            i += 3;
            continue;
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// `parse_qs(query).get(name, ["0"])[0]`.
fn first_query_value(query: &str, name: &str) -> Option<String> {
    for part in query.split('&') {
        if part.is_empty() {
            continue;
        }
        let Some((k, v)) = part.split_once('=') else { continue };
        if v.is_empty() {
            continue;
        }
        if unquote_plus(k) == name {
            return Some(unquote_plus(v));
        }
    }
    None
}

/// A body the handler reads: a read deadline per read, as a socket
/// timeout gives.
fn read_body(r: &mut dyn BufRead, n: usize) -> Vec<u8> {
    let mut buf = vec![0u8; n];
    let mut got = 0;
    while got < n {
        match r.read(&mut buf[got..]) {
            Ok(0) | Err(_) => break,
            Ok(k) => got += k,
        }
    }
    buf.truncate(got);
    buf
}

fn content_length(req: &Request) -> Option<BigInt> {
    let raw = req.header("Content-Length").unwrap_or("0");
    let n = py_int(raw)?;
    (n >= BigInt::from(0)).then_some(n)
}

/// A handler's failure the oracle answers 500.
struct Internal;

impl Server {
    fn prune(map: &mut HashMap<String, Vec<f64>>, t: f64) {
        map.retain(|_, hits| {
            hits.retain(|h| t - h < BAN_WINDOW);
            !hits.is_empty()
        });
    }

    fn record_failure(&self, addr: &str) {
        let mut m = self.failures.lock().unwrap_or_else(|e| e.into_inner());
        let t = now();
        Self::prune(&mut m, t);
        m.entry(addr.to_string()).or_default().push(t);
    }

    fn banned(&self, addr: &str) -> bool {
        let mut m = self.failures.lock().unwrap_or_else(|e| e.into_inner());
        Self::prune(&mut m, now());
        m.get(addr).map(|h| h.len()).unwrap_or(0) >= BAN_THRESHOLD
    }

    /// Serves until `stop` reads true.
    pub fn serve(self: Arc<Self>, stop: &'static std::sync::atomic::AtomicBool) {
        let _ = self.listener.set_nonblocking(true);
        loop {
            if stop.load(std::sync::atomic::Ordering::SeqCst) {
                return;
            }
            match self.listener.accept() {
                Ok((s, peer)) => {
                    let _ = s.set_nonblocking(false);
                    let me = Arc::clone(&self);
                    std::thread::spawn(move || me.handle(s, peer.ip().to_string()));
                }
                Err(_) => std::thread::sleep(Duration::from_millis(20)),
            }
        }
    }

    fn handle(&self, s: TcpStream, address: String) {
        let _ = s.set_read_timeout(Some(READ_TIMEOUT));
        let mut conn: Box<dyn Conn> = match &self.tls {
            None => Box::new(s),
            Some(cfg) => match rustls::ServerConnection::new(Arc::clone(cfg)) {
                Ok(c) => Box::new(rustls::StreamOwned::new(c, s)),
                Err(_) => return,
            },
        };
        let mut reader = BufReader::new(&mut conn);
        let raw = read_line(&mut reader, 65537);
        let mut out: Vec<u8> = vec![];
        {
            let mut rs = Responder { w: &mut out, method: String::new() };
            self.answer(&mut reader, &mut rs, raw, address);
        }
        drop(reader);
        let _ = conn.write_all(&out);
        let _ = conn.flush();
    }

    fn answer(&self, r: &mut dyn BufRead, rs: &mut Responder, raw: Vec<u8>, address: String) {
        if raw.len() > 65536 {
            return rs.send_error(414, "Request-URI Too Long");
        }
        if raw.is_empty() {
            return;
        }
        let line = latin1(&raw).trim_end_matches(['\r', '\n']).to_string();
        let words = text::split(&line);
        if words.is_empty() {
            return;
        }
        if words.len() >= 3 {
            let version = words[words.len() - 1];
            match http_version(version) {
                None => return rs.send_error(400, &format!("Bad request version ({})", text::repr(version))),
                Some(m) if m >= 2 => {
                    let base = version.split_once('/').map(|x| x.1).unwrap_or("");
                    return rs.send_error(505, &format!("Invalid HTTP version ({base})"));
                }
                _ => {}
            }
        }
        if words.len() < 2 || words.len() > 3 {
            return rs.send_error(400, &format!("Bad request syntax ({})", text::repr(&line)));
        }
        let method = words[0].to_string();
        let mut path = words[1].to_string();
        rs.method = method.clone();
        if words.len() == 2 && method != "GET" {
            return rs.send_error(400, &format!("Bad HTTP/0.9 request type ({})", text::repr(&method)));
        }
        if path.starts_with("//") {
            path = format!("/{}", path.trim_start_matches('/'));
        }
        let headers = match read_headers(r) {
            Ok(h) => h,
            Err((code, msg)) => return rs.send_error(code, msg),
        };
        let req = Request { method, path, headers, address };
        match req.method.as_str() {
            "GET" => {
                let (p, _) = url_path(&req.path);
                if p == "/health" {
                    rs.json(200, obj(&[("ok", Value::Bool(true))]));
                } else {
                    rs.error(404, "not_found", NOT_FOUND);
                }
            }
            "POST" => self.post(r, rs, &req),
            m => rs.send_error(501, &format!("Unsupported method ({})", text::repr(m))),
        }
    }

    /// `_authorized`: Err when the oracle's compare_digest raises (a
    /// token that is not ASCII, or a token file that is not UTF-8), and the
    /// connection then closes with no reply.
    fn authorized(&self, req: &Request) -> Result<bool, ()> {
        let token = read_token(&self.cfg.token_file).map_err(|_| ())?;
        let h = req.header("Authorization").unwrap_or("");
        let supplied = h.strip_prefix("Bearer ").map(text::strip).unwrap_or("");
        if let Some(t) = &token {
            if !supplied.is_ascii() || !t.is_ascii() {
                return Err(());
            }
            if supplied.len() == t.len() && supplied.bytes().zip(t.bytes()).fold(0u8, |acc, (a, b)| acc | (a ^ b)) == 0 {
                return Ok(true);
            }
        }
        self.record_failure(&req.address);
        Ok(false)
    }

    fn post(&self, r: &mut dyn BufRead, rs: &mut Responder, req: &Request) {
        let (p, query) = url_path(&req.path);
        if req.header("Transfer-Encoding").is_some() && req.header("Content-Length").is_none() {
            return rs.error(411, "chunked_not_supported", "Send a Content-Length header. Chunked transfer encoding is not read.");
        }
        if self.banned(&req.address) {
            return rs.error(429, "too_many_failed_tokens", "Too many failed tokens from this address. Wait 60 seconds and try again.");
        }
        match self.authorized(req) {
            Err(()) => return,
            Ok(false) => {
                return rs.error(401, "unauthorized", "This request needs a bearer token. Run coppice web token on the box that runs this server.")
            }
            Ok(true) => {}
        }
        if p != "/transcribe" && p != "/deliver" {
            return rs.error(404, "not_found", NOT_FOUND);
        }
        if req.header("Origin").is_some() {
            return rs.error(
                403,
                "cross_origin_refused",
                "This server does not accept requests carrying an Origin header. Call it directly, not from a browser page.",
            );
        }
        if let Some(site) = req.header("Sec-Fetch-Site") {
            if site != "same-origin" && site != "none" {
                return rs.error(
                    403,
                    "cross_origin_refused",
                    "This server does not accept cross-site requests. Call it directly, not from a browser page.",
                );
            }
        }
        let res = if p == "/transcribe" {
            let want = first_query_value(&query, "cleanup").as_deref() == Some("1");
            self.transcribe(r, rs, req, want)
        } else {
            self.deliver(r, rs, req)
        };
        if res.is_err() {
            rs.error(500, "internal_error", "Something went wrong handling this request. Check the server log, then try again.");
        }
    }

    fn transcribe(&self, r: &mut dyn BufRead, rs: &mut Responder, req: &Request, want_cleanup: bool) -> Result<(), Internal> {
        let Some(n) = content_length(req) else {
            rs.error(400, "bad_content_length", "Send a valid Content-Length header with the request.");
            return Ok(());
        };
        if n > BigInt::from(MAX_CONTENT_LENGTH) {
            rs.error(413, "payload_too_large", &format!("The request body must be {MAX_CONTENT_LENGTH} bytes or smaller."));
            return Ok(());
        }
        let body = read_body(r, usize::try_from(n).unwrap_or(0));
        let ctype = req.header("Content-Type").unwrap_or("application/octet-stream");
        let audio = if ctype.starts_with("multipart/form-data") {
            match extract_multipart_audio(&body, ctype) {
                Ok((a, _)) => a,
                Err(_) => {
                    rs.error(400, "bad_audio", BAD_AUDIO);
                    return Ok(());
                }
            }
        } else {
            body
        };
        let wav = match to_wav_16k_mono(&audio) {
            Ok(w) => w,
            Err(e) if e.bad_audio() => {
                rs.error(400, "bad_audio", BAD_AUDIO);
                return Ok(());
            }
            Err(_) => return Err(Internal),
        };
        let duration = wav_duration_s(&wav).map_err(|_| Internal)?;
        if duration > MAX_AUDIO_SECONDS {
            rs.json(
                413,
                obj(&[
                    ("error", "audio_too_long".into()),
                    ("max_seconds", Value::Float(MAX_AUDIO_SECONDS)),
                    ("message", "The clip is longer than 60 seconds. Send a shorter clip.".into()),
                ]),
            );
            return Ok(());
        }
        let t = match self.cfg.engine.transcribe(&wav, None) {
            Ok(t) => t,
            Err(super::engine::TranscribeError::Engine(e)) => {
                rs.error(503, if e.loading { "engine_loading" } else { "engine_unavailable" }, &e.msg);
                return Ok(());
            }
            Err(super::engine::TranscribeError::Other(_)) => return Err(Internal),
        };
        let mut text_ = t.text.clone();
        let mut cleaned = false;
        let mut reason = None;
        if want_cleanup {
            let mut tr = HttpTransport { client: &self.cfg.llm, base_url: self.cfg.cleanup.base_url.clone(), timeout: 30.0 };
            let secs = now() as i64;
            let res = clean_transcript(&text_, &self.cfg.cleanup, &mut tr, secs).map_err(|_| Internal)?;
            text_ = res.text;
            cleaned = res.cleaned;
            reason = res.reason;
        }
        let mut o = obj(&[
            ("text", Value::Str(text_)),
            ("raw_text", Value::Str(t.text)),
            ("duration_s", Value::Float(t.duration_s)),
            ("rtf", Value::Float(t.rtf)),
            ("engine", self.cfg.engine.name.into()),
            ("cleaned", Value::Bool(cleaned)),
        ]);
        if let Some(rs_) = reason {
            o.set("reason", rs_);
        }
        rs.json(200, o);
        Ok(())
    }

    fn deliver(&self, r: &mut dyn BufRead, rs: &mut Responder, req: &Request) -> Result<(), Internal> {
        if !req.header("Content-Type").unwrap_or("").starts_with("application/json") {
            rs.error(400, "bad_content_type", "Send Content-Type: application/json.");
            return Ok(());
        }
        let Some(n) = content_length(req) else {
            rs.error(400, "bad_content_length", "Send a valid Content-Length header with the request.");
            return Ok(());
        };
        if n > BigInt::from(MAX_DELIVER_BYTES) {
            rs.error(413, "payload_too_large", &format!("The request body must be {MAX_DELIVER_BYTES} bytes or smaller."));
            return Ok(());
        }
        let mut body = read_body(r, usize::try_from(n).unwrap_or(0));
        if body.is_empty() {
            body = b"{}".to_vec();
        }
        let text_body = pyops::decode_json_bytes(&body).ok_or(Internal)?;
        let payload = match pyops::loads_text(&text_body) {
            Ok(v) => v,
            Err(pyops::JsonErr::Decode) => {
                rs.error(400, "bad_json", "Send a JSON object with pane, text, and mode.");
                return Ok(());
            }
            Err(_) => return Err(Internal),
        };
        let Value::Obj(po) = payload else { return Err(Internal) };
        let pane = po.get("pane");
        let text_v = po.get("text").cloned().unwrap_or(Value::Str(String::new()));
        let mode = po.get("mode").cloned().unwrap_or(Value::Str("preview".into()));
        let backend = po.get("backend");
        let pane_id = match pane {
            Some(Value::Str(s)) if !s.is_empty() => s.clone(),
            _ => {
                rs.error(400, "bad_request", "pane must be a non-empty string.");
                return Ok(());
            }
        };
        let text_s = match &text_v {
            Value::Str(s) if !s.is_empty() => s.clone(),
            _ => {
                rs.error(400, "bad_request", "text must be a non-empty string.");
                return Ok(());
            }
        };
        if text::len(&text_s) > MAX_DELIVER_TEXT_CHARS {
            rs.error(413, "text_too_long", &format!("text must be {MAX_DELIVER_TEXT_CHARS} characters or fewer."));
            return Ok(());
        }
        if let Some(b) = backend {
            if !matches!(b, Value::Null | Value::Str(_)) {
                rs.error(400, "bad_request", "backend must be a string.");
                return Ok(());
            }
        }
        let mode_s = match &mode {
            Value::Str(s) if s == "preview" || s == "send" => s.clone(),
            _ => {
                rs.error(400, "bad_request", "mode must be preview or send.");
                return Ok(());
            }
        };
        let res = deliver(&pane_id, &text_s, &mode_s, &self.cfg.armed_dir, None, now(), |t| {
            send_through_floor(&self.cfg.floor, &self.cfg.home, &pane_id, t).map(|_| ())
        });
        match res {
            Err(SendError::FloorNotAvailable(m)) => {
                rs.error(503, "no_pane_backend", &m);
                Ok(())
            }
            Err(SendError::Other(_)) => Err(Internal),
            Ok(d) => {
                let code = if d.delivered == "refused" { 403 } else { 200 };
                let reason = d.reason.map(Value::Str).unwrap_or(Value::Null);
                rs.json(code, obj(&[("delivered", d.delivered.into()), ("reason", reason)]));
                Ok(())
            }
        }
    }
}

/// `--listen` host:port, digits after the first colon.
pub fn parse_listen(listen: &str) -> Option<(String, u16)> {
    let (host, port) = listen.split_once(':')?;
    if port.is_empty() || !port.chars().all(text::is_decimal) {
        return None;
    }
    let n = py_int(port)?;
    Some((host.to_string(), u16::try_from(n).ok()?))
}

/// `server._is_loopback`.
pub fn is_loopback(address: &str) -> bool {
    if address == "localhost" {
        return true;
    }
    if let Ok(v4) = address.parse::<std::net::Ipv4Addr>() {
        return v4.is_loopback();
    }
    let a = address.split('%').next().unwrap_or("");
    if address.contains('%') && (a.is_empty() || address.ends_with('%')) {
        return false;
    }
    match a.parse::<std::net::Ipv6Addr>() {
        Ok(v6) => v6.is_loopback() || v6.to_ipv4_mapped().map(|m| m.is_loopback()).unwrap_or(false),
        Err(_) => false,
    }
}

/// voice ptt's --server check: urlsplit gives http or https and a host.
pub fn check_server_url(raw: &str) -> bool {
    let raw: String = raw.trim_start_matches(|c: char| c <= ' ').chars().filter(|c| !matches!(c, '\t' | '\r' | '\n')).collect();
    let Some(i) = raw.find(':') else { return false };
    if i == 0 || !scheme_like(&raw[..i]) {
        return false;
    }
    let scheme = raw[..i].to_ascii_lowercase();
    if scheme != "http" && scheme != "https" {
        return false;
    }
    let Some(rest) = raw[i + 1..].strip_prefix("//") else { return false };
    let netloc = match rest.find(['/', '?', '#']) {
        Some(j) => &rest[..j],
        None => rest,
    };
    if netloc.contains('[') != netloc.contains(']') {
        return false;
    }
    let hostinfo = netloc.rsplit('@').next().unwrap_or("");
    let host = if let Some(h) = hostinfo.strip_prefix('[') {
        h.split(']').next().unwrap_or("")
    } else {
        hostinfo.split(':').next().unwrap_or("")
    };
    !host.is_empty()
}
