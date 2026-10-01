//! An HTTP/1.1 server with the behaviour of Go's net/http that the phone
//! server's routes reach: request parsing, the response framing (a body of
//! 2048 bytes or less gets a Content-Length, a longer one is chunked), a
//! sniffed Content-Type, Connection: close, http.Error, http.Redirect and
//! http.ServeContent with ranges and conditions.

use std::io::{self, BufRead, Read, Write};
use std::net::SocketAddr;

/// A stream a connection reads and writes: TCP, or TLS over TCP.
pub trait Stream: Read + Write + Send {}
impl<T: Read + Write + Send> Stream for T {}

/// One request.
pub struct Request {
    pub method: String,
    /// The path, unescaped (Go's URL.Path).
    pub path: String,
    /// The path as escaped (Go's URL.EscapedPath).
    pub escaped_path: String,
    pub raw_query: String,
    pub proto_minor: u8,
    /// Each header by its canonical key, values in order.
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    /// The body was longer than the limit and was not read whole.
    pub body_too_big: bool,
    pub remote: SocketAddr,
    pub local: SocketAddr,
    pub host: String,
}

impl Request {
    /// The first value of key, or "".
    pub fn header(&self, key: &str) -> &str {
        self.headers
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
            .unwrap_or("")
    }

    /// Every value of key.
    pub fn header_values(&self, key: &str) -> Vec<&str> {
        self.headers
            .iter()
            .filter(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
            .collect()
    }

    /// Go's URL.Query().Get(key), first value, unescaped.
    pub fn query(&self, key: &str) -> Option<String> {
        for part in self.raw_query.split('&') {
            if part.is_empty() || part.contains(';') {
                continue;
            }
            let (k, v) = part.split_once('=').unwrap_or((part, ""));
            if query_unescape(k).as_deref() == Some(key) {
                return query_unescape(v);
            }
        }
        None
    }
}

/// One response, built whole before it is written.
pub struct Response {
    pub status: u16,
    /// The handler's headers, by canonical key.
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    /// Close the connection after this response.
    pub close: bool,
}

impl Response {
    pub fn new(status: u16) -> Response {
        Response {
            status,
            headers: Vec::new(),
            body: Vec::new(),
            close: false,
        }
    }

    /// Go's Header.Set.
    pub fn set(&mut self, key: &str, value: &str) {
        self.headers.retain(|(k, _)| k != key);
        self.headers.push((key.to_string(), value.to_string()));
    }

    pub fn del(&mut self, key: &str) {
        self.headers.retain(|(k, _)| k != key);
    }

    pub fn get(&self, key: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
    }

    pub fn has(&self, key: &str) -> bool {
        self.headers.iter().any(|(k, _)| k == key)
    }
}

/// Go's http.StatusText.
pub fn status_text(code: u16) -> &'static str {
    match code {
        100 => "Continue",
        101 => "Switching Protocols",
        200 => "OK",
        204 => "No Content",
        206 => "Partial Content",
        301 => "Moved Permanently",
        302 => "Found",
        304 => "Not Modified",
        307 => "Temporary Redirect",
        308 => "Permanent Redirect",
        400 => "Bad Request",
        401 => "Unauthorized",
        403 => "Forbidden",
        404 => "Not Found",
        405 => "Method Not Allowed",
        409 => "Conflict",
        412 => "Precondition Failed",
        413 => "Request Entity Too Large",
        416 => "Requested Range Not Satisfiable",
        417 => "Expectation Failed",
        422 => "Unprocessable Entity",
        426 => "Upgrade Required",
        429 => "Too Many Requests",
        431 => "Request Header Fields Too Large",
        500 => "Internal Server Error",
        501 => "Not Implemented",
        502 => "Bad Gateway",
        503 => "Service Unavailable",
        504 => "Gateway Timeout",
        _ => "",
    }
}

/// Go's http.Error: the text and a newline, as text/plain, no sniffing.
pub fn error(resp: &mut Response, text: &str, code: u16) {
    resp.del("Content-Length");
    resp.set("Content-Type", "text/plain; charset=utf-8");
    resp.set("X-Content-Type-Options", "nosniff");
    resp.status = code;
    resp.body = format!("{text}\n").into_bytes();
}

/// Go's http.NotFound.
pub fn not_found(resp: &mut Response) {
    error(resp, "404 page not found", 404);
}

/// Go's htmlReplacer for http.Redirect's body.
fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&#34;")
        .replace('\'', "&#39;")
}

/// Go's http.Redirect for a URL that is already a path from the root.
pub fn redirect(req: &Request, resp: &mut Response, url: &str, code: u16) {
    resp.set("Location", url);
    if (req.method == "GET" || req.method == "HEAD") && !resp.has("Content-Type") {
        resp.set("Content-Type", "text/html; charset=utf-8");
    }
    resp.status = code;
    if req.method == "GET" {
        resp.body = format!(
            "<a href=\"{}\">{}</a>.\n\n",
            html_escape(url),
            status_text(code)
        )
        .into_bytes();
    }
}

/// Go's textproto.CanonicalMIMEHeaderKey for a key of token characters.
pub fn canonical_key(k: &str) -> String {
    let valid = k
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b));
    if !valid {
        return k.to_string();
    }
    let mut out = String::with_capacity(k.len());
    let mut upper = true;
    for c in k.chars() {
        if upper {
            out.push(c.to_ascii_uppercase());
        } else {
            out.push(c.to_ascii_lowercase());
        }
        upper = c == '-';
    }
    out
}

/// Go's url.unescape in path mode: None for a bad escape.
pub fn path_unescape(s: &str) -> Option<String> {
    unescape(s, false)
}

/// Go's url.QueryUnescape: a plus is a space.
pub fn query_unescape(s: &str) -> Option<String> {
    unescape(s, true)
}

fn unescape(s: &str, plus: bool) -> Option<String> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'%' => {
                let h = |c: u8| (c as char).to_digit(16);
                let (Some(x), Some(y)) = (h(*b.get(i + 1)?), h(*b.get(i + 2)?)) else {
                    return None;
                };
                out.push((x * 16 + y) as u8);
                i += 3;
            }
            b'+' if plus => {
                out.push(b' ');
                i += 1;
            }
            c => {
                out.push(c);
                i += 1;
            }
        }
    }
    String::from_utf8(out).ok()
}

/// Go's shouldEscape in path mode, for EscapedPath.
fn should_escape_path(c: u8) -> bool {
    !(c.is_ascii_alphanumeric()
        || matches!(
            c,
            b'-' | b'_'
                | b'.'
                | b'~'
                | b'$'
                | b'&'
                | b'+'
                | b','
                | b'/'
                | b':'
                | b';'
                | b'='
                | b'@'
        ))
}

fn escape_path(s: &str) -> String {
    let mut out = String::new();
    for &c in s.as_bytes() {
        if should_escape_path(c) {
            out.push_str(&format!("%{c:02X}"));
        } else {
            out.push(c as char);
        }
    }
    out
}

/// Go's validEncoded in path mode.
fn valid_encoded(s: &str) -> bool {
    for &c in s.as_bytes() {
        match c {
            b'!' | b'$' | b'&' | b'\'' | b'(' | b')' | b'*' | b'+' | b',' | b';' | b'=' | b':'
            | b'@' | b'[' | b']' | b'%' => {}
            _ => {
                if should_escape_path(c) {
                    return false;
                }
            }
        }
    }
    true
}

/// Splits a request target into the path, the escaped path and the raw
/// query, as Go's url.ParseRequestURI does for a path target.
fn parse_target(target: &str) -> Option<(String, String, String)> {
    if !target.starts_with('/') {
        return None;
    }
    let (p, q) = match target.split_once('?') {
        Some((p, q)) => (p, q.to_string()),
        None => (target, String::new()),
    };
    let path = path_unescape(p)?;
    let escaped = if p == escape_path(&path) {
        escape_path(&path)
    } else if valid_encoded(p) && path_unescape(p).as_deref() == Some(path.as_str()) {
        p.to_string()
    } else {
        escape_path(&path)
    };
    Some((path, escaped, q))
}

/// The most of a request body that is read: a voice clip's limit and one.
pub const MAX_BODY: usize = (8 << 20) + 1;

/// What reading a request found.
pub enum Got {
    Req(Box<Request>),
    /// The connection ended before a request began.
    Closed,
    /// A request Go would refuse with 400 before any handler, with the
    /// words Go writes.
    Bad(&'static str),
}

/// Reads the head of one request from r. The body is read after, by
/// read_body, once a 100 Continue has gone out when the client waits for
/// one.
pub fn read_request<R: BufRead>(
    r: &mut R,
    remote: SocketAddr,
    local: SocketAddr,
) -> io::Result<Got> {
    let mut line = String::new();
    loop {
        line.clear();
        let n = read_line(r, &mut line)?;
        if n == 0 {
            return Ok(Got::Closed);
        }
        // Go skips one empty line before a request.
        if !line.trim_end_matches(['\r', '\n']).is_empty() {
            break;
        }
    }
    let first = line.trim_end_matches(['\r', '\n']).to_string();
    let mut parts = first.splitn(3, ' ');
    let (Some(method), Some(target), Some(proto)) = (parts.next(), parts.next(), parts.next())
    else {
        return Ok(Got::Bad("malformed HTTP request"));
    };
    // Go's ParseHTTPVersion: HTTP/X.Y with one digit each.
    let pb = proto.as_bytes();
    let (major, minor) = if proto.len() == 8
        && proto.starts_with("HTTP/")
        && pb[6] == b'.'
        && pb[5].is_ascii_digit()
        && pb[7].is_ascii_digit()
    {
        (pb[5] - b'0', pb[7] - b'0')
    } else {
        return Ok(Got::Bad("malformed HTTP version"));
    };
    if method.is_empty() || !method.bytes().all(|b| b.is_ascii_graphic()) {
        return Ok(Got::Bad("invalid method"));
    }
    let mut headers: Vec<(String, String)> = Vec::new();
    let mut total = 0usize;
    loop {
        line.clear();
        let n = read_line(r, &mut line)?;
        if n == 0 {
            return Ok(Got::Closed);
        }
        total += n;
        if total > 1 << 20 {
            return Ok(Got::Bad("request header too large"));
        }
        let l = line.trim_end_matches(['\r', '\n']);
        if l.is_empty() {
            break;
        }
        let Some((k, v)) = l.split_once(':') else {
            return Ok(Got::Bad("malformed MIME header line"));
        };
        if k.is_empty() || k.contains(' ') || k.contains('\t') {
            return Ok(Got::Bad("malformed MIME header line"));
        }
        headers.push((canonical_key(k), v.trim_matches([' ', '\t']).to_string()));
    }
    let Some((path, escaped_path, raw_query)) = parse_target(target) else {
        return Ok(Got::Bad("malformed HTTP request"));
    };
    let hosts = headers.iter().filter(|(k, _)| k == "Host").count();
    if hosts > 1 {
        return Ok(Got::Bad("too many Host headers"));
    }
    if major != 1 {
        return Ok(Got::Bad("unsupported protocol version"));
    }
    let proto_minor = minor.min(1);
    if proto_minor == 1 && hosts == 0 {
        return Ok(Got::Bad("missing required Host header"));
    }
    if let Some((_, h)) = headers.iter().find(|(k, _)| k == "Host") {
        let ok = h
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-._~!$&'()*+,;=:[]%".contains(&b));
        if !ok {
            return Ok(Got::Bad("malformed Host header"));
        }
    }
    let host = headers
        .iter()
        .find(|(k, _)| k == "Host")
        .map(|(_, v)| v.clone())
        .unwrap_or_default();
    let req = Request {
        method: method.to_string(),
        path,
        escaped_path,
        raw_query,
        proto_minor,
        headers,
        body: Vec::new(),
        body_too_big: false,
        remote,
        local,
        host,
    };
    if !req.header("Transfer-Encoding").is_empty()
        && !req
            .header("Transfer-Encoding")
            .eq_ignore_ascii_case("chunked")
    {
        return Ok(Got::Bad("unsupported transfer encoding"));
    }
    let cl = req.header("Content-Length");
    if !cl.is_empty() && cl.parse::<u64>().is_err() {
        return Ok(Got::Bad("bad Content-Length"));
    }
    Ok(Got::Req(Box::new(req)))
}

/// Whether the client waits for a 100 Continue before it sends the body.
pub fn wants_continue(req: &Request) -> bool {
    req.header("Expect").eq_ignore_ascii_case("100-continue")
        && (req
            .header("Transfer-Encoding")
            .eq_ignore_ascii_case("chunked")
            || req.header("Content-Length").parse::<u64>().unwrap_or(0) > 0)
}

/// Reads the body of req from r: at most MAX_BODY bytes are kept, and the
/// rest is read and dropped, so the next request starts in the right
/// place.
pub fn read_body<R: BufRead>(r: &mut R, req: &mut Request) -> io::Result<()> {
    let mut line = String::new();
    if req
        .header("Transfer-Encoding")
        .eq_ignore_ascii_case("chunked")
    {
        let mut body = Vec::new();
        loop {
            line.clear();
            if read_line(r, &mut line)? == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
            let size_text = line.trim_end_matches(['\r', '\n']);
            let size_text = size_text.split(';').next().unwrap_or("").trim();
            let size = usize::from_str_radix(size_text, 16)
                .map_err(|_| io::Error::from(io::ErrorKind::InvalidData))?;
            if size == 0 {
                loop {
                    line.clear();
                    if read_line(r, &mut line)? == 0
                        || line.trim_end_matches(['\r', '\n']).is_empty()
                    {
                        break;
                    }
                }
                break;
            }
            let mut chunk = vec![0u8; size];
            r.read_exact(&mut chunk)?;
            let room = MAX_BODY.saturating_sub(body.len());
            if room < chunk.len() {
                req.body_too_big = true;
            }
            body.extend_from_slice(&chunk[..chunk.len().min(room)]);
            line.clear();
            read_line(r, &mut line)?;
        }
        req.body = body;
        return Ok(());
    }
    let n: u64 = req.header("Content-Length").parse().unwrap_or(0);
    let keep = (n as usize).min(MAX_BODY);
    let mut body = vec![0u8; keep];
    r.read_exact(&mut body)?;
    let mut left = n - keep as u64;
    if left > 0 {
        req.body_too_big = true;
        let mut sink = [0u8; 8192];
        while left > 0 {
            let k = (left as usize).min(sink.len());
            r.read_exact(&mut sink[..k])?;
            left -= k as u64;
        }
    }
    req.body = body;
    Ok(())
}

/// One line, cut at 1 MiB so a client cannot fill memory.
fn read_line<R: BufRead>(r: &mut R, out: &mut String) -> io::Result<usize> {
    let mut buf = Vec::new();
    let n = r.by_ref().take(1 << 20).read_until(b'\n', &mut buf)?;
    out.push_str(&String::from_utf8_lossy(&buf));
    Ok(n)
}

/// Whether a status may carry a body.
fn body_allowed(code: u16) -> bool {
    !((100..200).contains(&code) || code == 204 || code == 304)
}

/// Go's DetectContentType for the bodies the handlers here write.
pub fn sniff(data: &[u8]) -> &'static str {
    let data = &data[..data.len().min(512)];
    let first = data
        .iter()
        .position(|b| !matches!(b, b'\t' | b'\n' | 0x0c | b'\r' | b' '))
        .unwrap_or(data.len());
    let ws = &data[first..];
    let html_sigs: [&[u8]; 17] = [
        b"<!DOCTYPE HTML",
        b"<HTML",
        b"<HEAD",
        b"<SCRIPT",
        b"<IFRAME",
        b"<H1",
        b"<DIV",
        b"<FONT",
        b"<TABLE",
        b"<A",
        b"<STYLE",
        b"<TITLE",
        b"<B",
        b"<BODY",
        b"<BR",
        b"<P",
        b"<!--",
    ];
    for sig in html_sigs {
        if ws.len() > sig.len()
            && ws[..sig.len()].eq_ignore_ascii_case(sig)
            && (ws[sig.len()] == b' ' || ws[sig.len()] == b'>')
        {
            return "text/html; charset=utf-8";
        }
    }
    if ws.starts_with(b"<?xml") {
        return "text/xml; charset=utf-8";
    }
    if data.starts_with(b"%PDF-") {
        return "application/pdf";
    }
    if data.starts_with(b"\x89PNG\x0D\x0A\x1A\x0A") {
        return "image/png";
    }
    if data.starts_with(b"\xFE\xFF") || data.starts_with(b"\xFF\xFE") {
        return "text/plain; charset=utf-16be";
    }
    if data.starts_with(b"\xEF\xBB\xBF") {
        return "text/plain; charset=utf-8";
    }
    if data.iter().any(|&b| {
        b <= 0x08 || b == 0x0B || (0x0E..=0x1A).contains(&b) || (0x1C..=0x1F).contains(&b)
    }) {
        return "application/octet-stream";
    }
    "text/plain; charset=utf-8"
}

/// Go's http.TimeFormat of now.
pub fn http_date(secs: i64) -> String {
    let (y, mo, d, h, mi, s) = super::der::civil(secs);
    let days = secs.div_euclid(86400);
    let wd = ["Thu", "Fri", "Sat", "Sun", "Mon", "Tue", "Wed"][days.rem_euclid(7) as usize];
    let mon = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ][mo as usize - 1];
    format!("{wd}, {d:02} {mon} {y:04} {h:02}:{mi:02}:{s:02} GMT")
}

fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Whether the request asks for the connection to close after the reply.
pub fn wants_close(req: &Request) -> bool {
    let conn = req.header_values("Connection").join(",");
    let has = |t: &str| conn.split(',').any(|x| x.trim().eq_ignore_ascii_case(t));
    if req.proto_minor == 0 {
        return !has("keep-alive");
    }
    has("close")
}

/// Writes resp for req the way Go's response writer frames it. Returns
/// whether the connection must close after it.
pub fn write_response(w: &mut dyn Write, req: &Request, mut resp: Response) -> io::Result<bool> {
    let head = req.method == "HEAD";
    let code = resp.status;
    let mut close = resp.close || wants_close(req);
    let mut extra: Vec<(&str, String)> = Vec::new();
    let has_cl = resp.has("Content-Length");
    let mut chunked = false;
    if body_allowed(code) {
        if !has_cl && (!head || !resp.body.is_empty()) && resp.body.len() <= 2048 {
            extra.push(("Content-Length", resp.body.len().to_string()));
        } else if !has_cl && resp.body.len() > 2048 && !head {
            if req.proto_minor >= 1 {
                chunked = true;
            } else {
                close = true;
            }
        }
        if !resp.has("Content-Type") && !resp.body.is_empty() {
            extra.push(("Content-Type", sniff(&resp.body).to_string()));
        }
    } else {
        for k in ["Content-Type", "Content-Length", "Transfer-Encoding"] {
            if code == 304 || k != "Content-Type" {
                resp.del(k);
            }
        }
    }
    let switching = code == 101;
    if close && !switching {
        resp.del("Connection");
        if req.proto_minor >= 1 {
            extra.push(("Connection", "close".into()));
        }
    }
    if chunked {
        extra.push(("Transfer-Encoding", "chunked".into()));
    }
    let mut out = format!(
        "HTTP/1.{} {} {}\r\n",
        req.proto_minor.min(1),
        code,
        status_text(code)
    );
    let mut sorted = resp.headers.clone();
    sorted.sort_by(|a, b| a.0.cmp(&b.0));
    for (k, v) in &sorted {
        out.push_str(&format!("{k}: {v}\r\n"));
    }
    // Go writes Date, then Content-Length, then Content-Type, Connection
    // and Transfer-Encoding after the handler's own headers.
    out.push_str(&format!("Date: {}\r\n", http_date(now_secs())));
    for key in [
        "Content-Length",
        "Content-Type",
        "Connection",
        "Transfer-Encoding",
    ] {
        for (k, v) in &extra {
            if *k == key {
                out.push_str(&format!("{k}: {v}\r\n"));
            }
        }
    }
    out.push_str("\r\n");
    let mut bytes = out.into_bytes();
    if !head && body_allowed(code) {
        if chunked {
            bytes.extend_from_slice(format!("{:x}\r\n", resp.body.len()).as_bytes());
            bytes.extend_from_slice(&resp.body);
            bytes.extend_from_slice(b"\r\n0\r\n\r\n");
        } else {
            let n = match resp
                .get("Content-Length")
                .and_then(|v| v.parse::<usize>().ok())
            {
                Some(n) => n.min(resp.body.len()),
                None => resp.body.len(),
            };
            bytes.extend_from_slice(&resp.body[..n]);
        }
    }
    w.write_all(&bytes)?;
    w.flush()?;
    Ok(close)
}

/// Writes Go's answer to a request it could not read.
pub fn write_bad_request(w: &mut dyn Write, why: &str) -> io::Result<()> {
    const HEADERS: &str =
        "\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n";
    let out = match why {
        "request header too large" => {
            format!("HTTP/1.1 431 Request Header Fields Too Large{HEADERS}431 Request Header Fields Too Large")
        }
        "unsupported transfer encoding" => {
            format!("HTTP/1.1 501 Not Implemented{HEADERS}Unsupported transfer encoding")
        }
        "unsupported protocol version" => format!(
            "HTTP/1.1 505 HTTP Version Not Supported: {why}{HEADERS}505 HTTP Version Not Supported: {why}"
        ),
        // Go's badRequestError: the words go in the status line and the
        // body.
        "missing required Host header" | "malformed Host header" => {
            format!("HTTP/1.1 400 Bad Request: {why}{HEADERS}400 Bad Request: {why}")
        }
        _ => format!("HTTP/1.1 400 Bad Request{HEADERS}400 Bad Request"),
    };
    w.write_all(out.as_bytes())
}

/// One byte range of a body.
#[derive(Clone, Copy, Debug)]
pub struct Range {
    pub start: u64,
    pub length: u64,
}

/// Go's parseRange: Err(true) for a range that does not overlap, Err(false)
/// for one that does not parse.
pub fn parse_range(s: &str, size: u64) -> Result<Vec<Range>, bool> {
    if s.is_empty() {
        return Ok(Vec::new());
    }
    let Some(spec) = s.strip_prefix("bytes=") else {
        return Err(false);
    };
    let mut ranges = Vec::new();
    let mut no_overlap = false;
    for ra in spec.split(',') {
        let ra = ra.trim_matches([' ', '\t']);
        if ra.is_empty() {
            continue;
        }
        let Some((start, end)) = ra.split_once('-') else {
            return Err(false);
        };
        let (start, end) = (
            start.trim_matches([' ', '\t']),
            end.trim_matches([' ', '\t']),
        );
        let r = if start.is_empty() {
            if end.is_empty() || end.starts_with('-') {
                return Err(false);
            }
            let Ok(mut i) = end.parse::<i64>() else {
                return Err(false);
            };
            if i < 0 {
                return Err(false);
            }
            if i as u64 > size {
                i = size as i64;
            }
            Range {
                start: size - i as u64,
                length: i as u64,
            }
        } else {
            let Ok(i) = start.parse::<i64>() else {
                return Err(false);
            };
            if i < 0 {
                return Err(false);
            }
            if i as u64 >= size {
                no_overlap = true;
                continue;
            }
            let start = i as u64;
            let length = if end.is_empty() {
                size - start
            } else {
                let Ok(mut e) = end.parse::<i64>() else {
                    return Err(false);
                };
                if (start as i64) > e {
                    return Err(false);
                }
                if e as u64 >= size {
                    e = size as i64 - 1;
                }
                e as u64 - start + 1
            };
            Range { start, length }
        };
        ranges.push(r);
    }
    if no_overlap && ranges.is_empty() {
        return Err(true);
    }
    Ok(ranges)
}

/// Go's serveError: drops the headers that describe the content, then
/// http.Error.
fn serve_error(resp: &mut Response, text: &str, code: u16) {
    for k in ["Cache-Control", "Content-Encoding", "Etag", "Last-Modified"] {
        resp.del(k);
    }
    error(resp, text, code);
}

/// Go's http.ServeContent for a body held in memory, named name, last
/// changed at modtime (Unix seconds; None for no time, as an embedded file
/// has).
pub fn serve_content(
    req: &Request,
    resp: &mut Response,
    name: &str,
    modtime: Option<i64>,
    content: &[u8],
) {
    if let Some(t) = modtime {
        resp.set("Last-Modified", &http_date(t));
    }
    // If-Match, If-Unmodified-Since, If-None-Match: no file here has an
    // ETag, so only the time checks can apply.
    if let Some(t) = modtime {
        if let Some(ius) = parse_http_time(req.header("If-Unmodified-Since")) {
            if t > ius && req.header("If-Match").is_empty() {
                resp.status = 412;
                resp.body.clear();
                return;
            }
        }
    }
    if !req.header("If-None-Match").is_empty() {
        if req.method == "GET" || req.method == "HEAD" {
            if req.header("If-None-Match").trim() == "*" {
                not_modified(resp);
                return;
            }
        } else if req.header("If-None-Match").trim() == "*" {
            resp.status = 412;
            return;
        }
    } else if let Some(t) = modtime {
        if req.method == "GET" || req.method == "HEAD" {
            if let Some(ims) = parse_http_time(req.header("If-Modified-Since")) {
                if t <= ims {
                    not_modified(resp);
                    return;
                }
            }
        }
    }
    let mut range_header = req.header("Range").to_string();
    if !range_header.is_empty() && !req.header("If-Range").is_empty() {
        let ir = req.header("If-Range");
        let keep = match (modtime, parse_http_time(ir)) {
            (Some(t), Some(x)) if !ir.starts_with('"') && !ir.starts_with("W/") => t == x,
            _ => false,
        };
        if !keep {
            range_header.clear();
        }
    }
    let ctype = match resp.get("Content-Type") {
        Some(c) => c.to_string(),
        None => {
            let ext = match name.rfind('.') {
                Some(i) if !name[i..].contains('/') => &name[i..],
                _ => "",
            };
            let c = super::statics::type_by_extension(ext);
            let c = if c.is_empty() {
                sniff(content).to_string()
            } else {
                c
            };
            resp.set("Content-Type", &c);
            c
        }
    };
    let _ = ctype;
    let size = content.len() as u64;
    let mut ranges = match parse_range(&range_header, size) {
        Ok(r) => r,
        Err(true) if size == 0 => Vec::new(),
        Err(true) => {
            resp.set("Content-Range", &format!("bytes */{size}"));
            serve_error(resp, "invalid range: failed to overlap", 416);
            return;
        }
        Err(false) => {
            serve_error(resp, "invalid range", 416);
            return;
        }
    };
    if ranges.iter().map(|r| r.length).sum::<u64>() > size {
        ranges.clear();
    }
    let mut code = 200;
    let mut body = content;
    if ranges.len() == 1 {
        let r = ranges[0];
        body = &content[r.start as usize..(r.start + r.length) as usize];
        code = 206;
        resp.set(
            "Content-Range",
            &format!("bytes {}-{}/{size}", r.start, r.start + r.length - 1),
        );
    } else if ranges.len() > 1 {
        // Several ranges: Go sends multipart/byteranges with a random
        // boundary. This port sends the whole body (CP-R-38).
        ranges.clear();
    }
    resp.set("Accept-Ranges", "bytes");
    resp.set("Content-Length", &body.len().to_string());
    resp.status = code;
    resp.body = body.to_vec();
}

fn not_modified(resp: &mut Response) {
    resp.del("Content-Type");
    resp.del("Content-Length");
    resp.del("Content-Encoding");
    resp.status = 304;
    resp.body.clear();
}

/// Go's http.ParseTime for the three formats it reads.
pub fn parse_http_time(s: &str) -> Option<i64> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    let mon = |m: &str| -> Option<u32> {
        [
            "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
        ]
        .iter()
        .position(|x| *x == m)
        .map(|i| i as u32 + 1)
    };
    let hms = |t: &str| -> Option<(u32, u32, u32)> {
        let mut p = t.split(':');
        Some((
            p.next()?.parse().ok()?,
            p.next()?.parse().ok()?,
            p.next()?.parse().ok()?,
        ))
    };
    let f: Vec<&str> = s.split_whitespace().collect();
    // Mon, 02 Jan 2006 15:04:05 GMT
    if f.len() == 6 && f[0].ends_with(',') && f[5] == "GMT" {
        let (h, mi, se) = hms(f[4])?;
        return Some(super::der::unix(
            f[3].parse().ok()?,
            mon(f[2])?,
            f[1].parse().ok()?,
            h,
            mi,
            se,
        ));
    }
    // Monday, 02-Jan-06 15:04:05 GMT
    if f.len() == 4 && f[0].ends_with(',') && f[3] == "GMT" {
        let d: Vec<&str> = f[1].split('-').collect();
        if d.len() != 3 {
            return None;
        }
        let yy: i64 = d[2].parse().ok()?;
        let (h, mi, se) = hms(f[2])?;
        return Some(super::der::unix(
            if yy < 69 { 2000 + yy } else { 1900 + yy },
            mon(d[1])?,
            d[0].parse().ok()?,
            h,
            mi,
            se,
        ));
    }
    // Mon Jan _2 15:04:05 2006
    if f.len() == 5 {
        let (h, mi, se) = hms(f[3])?;
        return Some(super::der::unix(
            f[4].parse().ok()?,
            mon(f[1])?,
            f[2].parse().ok()?,
            h,
            mi,
            se,
        ));
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ranges_parse_as_go_parses_them() {
        let r = parse_range("bytes=0-9", 424).unwrap();
        assert_eq!((r[0].start, r[0].length), (0, 10));
        assert!(matches!(parse_range("bytes=999999-", 424), Err(true)));
        assert!(matches!(parse_range("bytes=x", 424), Err(false)));
        let r = parse_range("bytes=-5", 10).unwrap();
        assert_eq!((r[0].start, r[0].length), (5, 5));
    }

    #[test]
    fn keys_are_canonical() {
        assert_eq!(canonical_key("sec-websocket-key"), "Sec-Websocket-Key");
        assert_eq!(canonical_key("CONTENT-type"), "Content-Type");
    }

    #[test]
    fn dates_print_as_go_prints_them() {
        assert_eq!(http_date(0), "Thu, 01 Jan 1970 00:00:00 GMT");
        assert_eq!(
            parse_http_time("Mon, 02 Jan 2006 15:04:05 GMT"),
            Some(1136214245)
        );
    }

    #[test]
    fn targets_keep_their_escapes() {
        let (p, e, q) = parse_target("/app%2Ejs?v=2").unwrap();
        assert_eq!(
            (p.as_str(), e.as_str(), q.as_str()),
            ("/app.js", "/app%2Ejs", "v=2")
        );
        let (p, e, _) = parse_target("/a b").unwrap();
        assert_eq!((p.as_str(), e.as_str()), ("/a b", "/a%20b"));
    }
}
