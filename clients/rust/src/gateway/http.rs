//! The gateway's upstream client: HTTP/1.1 over TCP, or over rustls with
//! the ring provider and the system's root store, through a proxy when
//! the environment names one as httpx reads it. It follows no redirect,
//! has no timeout (agent turns run long, as the oracle's httpx client has
//! none), adds no compression of its own, and decodes gzip and deflate as
//! httpx does. A connection whose answer was read to its end is kept for
//! the next turn to the same host.

use std::collections::HashMap;
use std::io::{self, Read, Write};
use std::sync::{Mutex, OnceLock};

use crate::netproxy;

use miniz_oxide::inflate::stream::{inflate, InflateState};
use miniz_oxide::{DataFormat, MZError, MZFlush, MZStatus};

fn bad(why: &str) -> io::Error {
    io::Error::other(why.to_string())
}

/// A connection with its read buffer.
struct Wire {
    conn: netproxy::Conn,
    buf: Vec<u8>,
    pos: usize,
}

impl Wire {
    fn fill(&mut self) -> io::Result<usize> {
        if self.pos >= self.buf.len() {
            self.buf.clear();
            self.pos = 0;
        }
        let mut b = [0u8; 65536];
        let n = self.conn.io.read(&mut b)?;
        self.buf.extend_from_slice(&b[..n]);
        Ok(n)
    }

    fn avail(&self) -> &[u8] {
        &self.buf[self.pos..]
    }

    /// One line, without its CRLF; EOF before the line ends is an error.
    fn line(&mut self) -> io::Result<Vec<u8>> {
        loop {
            if let Some(i) = self.avail().iter().position(|&c| c == b'\n') {
                let mut l = self.avail()[..i].to_vec();
                self.pos += i + 1;
                if l.last() == Some(&b'\r') {
                    l.pop();
                }
                return Ok(l);
            }
            if self.avail().len() > 1 << 20 {
                return Err(bad("an upstream header line is too long"));
            }
            if self.fill()? == 0 {
                return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
            }
        }
    }

    /// Up to `max` bytes of the body; 0 at EOF.
    fn some(&mut self, out: &mut [u8], max: usize) -> io::Result<usize> {
        if self.avail().is_empty() && self.fill()? == 0 {
            return Ok(0);
        }
        let n = self.avail().len().min(max).min(out.len());
        out[..n].copy_from_slice(&self.avail()[..n]);
        self.pos += n;
        Ok(n)
    }
}

type Key = (bool, String, u16);

/// Idle connections, by host.
fn pool() -> &'static Mutex<HashMap<Key, Vec<Wire>>> {
    static P: OnceLock<Mutex<HashMap<Key, Vec<Wire>>>> = OnceLock::new();
    P.get_or_init(|| Mutex::new(HashMap::new()))
}

/// `urllib.parse.unquote`: each %XX escape to its byte, the bytes read as
/// UTF-8 with replacement; any other text kept.
pub fn unquote(s: &str) -> String {
    if !s.contains('%') {
        return s.to_string();
    }
    let b = s.as_bytes();
    let mut out = String::with_capacity(s.len());
    let mut pending: Vec<u8> = vec![];
    let mut i = 0;
    let hex = |c: u8| (c as char).to_digit(16);
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() {
            if let (Some(h), Some(l)) = (hex(b[i + 1]), hex(b[i + 2])) {
                pending.push((h * 16 + l) as u8);
                i += 3;
                continue;
            }
        }
        if !pending.is_empty() {
            out.push_str(&String::from_utf8_lossy(&pending));
            pending.clear();
        }
        let ch = s[i..].chars().next().expect("a char boundary");
        out.push(ch);
        i += ch.len_utf8();
    }
    if !pending.is_empty() {
        out.push_str(&String::from_utf8_lossy(&pending));
    }
    out
}

/// httpx's `quote`: every character outside the unreserved set and `safe`
/// written as %XX of its UTF-8, an existing %XX escape kept.
fn quote(s: &str, safe: &dyn Fn(char) -> bool) -> String {
    let keep = |c: char| c.is_ascii_alphanumeric() || "-._~".contains(c) || safe(c);
    let b = s.as_bytes();
    let mut out = String::with_capacity(s.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() && b[i + 1].is_ascii_hexdigit() && b[i + 2].is_ascii_hexdigit() {
            out.push_str(&s[i..i + 3]);
            i += 3;
            continue;
        }
        let ch = s[i..].chars().next().expect("a char boundary");
        if keep(ch) {
            out.push(ch);
        } else {
            let mut buf = [0u8; 4];
            for x in ch.encode_utf8(&mut buf).bytes() {
                out.push_str(&format!("%{x:02X}"));
            }
        }
        i += ch.len_utf8();
    }
    out
}

/// httpx's PATH_SAFE: printable ASCII less space " # < > ? ` { }.
fn path_safe(c: char) -> bool {
    (' '..='~').contains(&c) && !" \"#<>?`{}".contains(c)
}

/// httpx's QUERY_SAFE: printable ASCII less space " # < >.
fn query_safe(c: char) -> bool {
    (' '..='~').contains(&c) && !" \"#<>".contains(c)
}

/// The request target httpx sends for the part of a URL after its host:
/// the path up to `?` or `#`, the query up to `#`, each quoted as httpx
/// quotes it; a fragment is not sent.
fn request_target(rest: &str) -> String {
    let (before_frag, _) = rest.split_once('#').unwrap_or((rest, ""));
    let (path, query) = match before_frag.split_once('?') {
        Some((p, q)) => (p, Some(q)),
        None => (before_frag, None),
    };
    let mut t = if path.is_empty() { "/".to_string() } else { quote(path, &path_safe) };
    if let Some(q) = query.filter(|q| !q.is_empty()) {
        t.push('?');
        t.push_str(&quote(q, &query_safe));
    }
    t
}

/// The URL split: TLS or not, host, port, and the request target.
pub fn target(url: &str) -> io::Result<(bool, String, u16, String)> {
    let no = || bad(&format!("the URL {url} is not one this binary sends to"));
    let (scheme, rest) = url.split_once("://").ok_or_else(no)?;
    let tls = match scheme.to_ascii_lowercase().as_str() {
        "http" => false,
        "https" => true,
        _ => return Err(no()),
    };
    let (hostport, path) = match rest.find(['/', '?', '#']) {
        Some(i) => (&rest[..i], request_target(&rest[i..])),
        None => (rest, "/".to_string()),
    };
    if hostport.is_empty() || hostport.contains('@') {
        return Err(no());
    }
    let (host, port) = if let Some(h) = hostport.strip_prefix('[') {
        let (h, p) = h.split_once(']').ok_or_else(no)?;
        let port = match p.strip_prefix(':') {
            Some(p) => p.parse().map_err(|_| no())?,
            None if p.is_empty() => {
                if tls {
                    443
                } else {
                    80
                }
            }
            None => return Err(no()),
        };
        (h.to_string(), port)
    } else {
        match hostport.rsplit_once(':') {
            Some((h, p)) => (h.to_string(), p.parse::<u16>().map_err(|_| no())?),
            None => (hostport.to_string(), if tls { 443 } else { 80 }),
        }
    };
    if host.is_empty() {
        return Err(no());
    }
    Ok((tls, host, port, path))
}

fn connect(key: &Key) -> io::Result<Wire> {
    let (tls, host, port) = key;
    let origin = netproxy::Origin { tls: *tls, host: host.clone(), port: *port, explicit_port: true };
    let route = netproxy::process_httpx().route(&origin).map_err(|r| bad(&format!("{r:?}")))?;
    let conn = netproxy::open(&route, &origin, &netproxy::Dial::default()).map_err(netproxy::Fail::into_io)?;
    Ok(Wire { conn, buf: Vec::new(), pos: 0 })
}

/// How the body of an answer ends.
enum Framing {
    Length(u64),
    Chunked { left: u64, done: bool },
    Close,
    None,
}

/// An upstream answer: status, headers as sent (names and Latin-1 values
/// as bytes), and its body, read as it arrives.
pub struct Response {
    pub status: u16,
    pub headers: Vec<(String, Vec<u8>)>,
    pub body: Body,
}

impl Response {
    /// Every value of a header, in order.
    pub fn values(&self, name: &str) -> Vec<&[u8]> {
        self.headers.iter().filter(|(k, _)| k.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_slice()).collect()
    }
}

/// The raw body of an answer, its framing removed.
pub struct Body {
    wire: Option<Wire>,
    framing: Framing,
    key: Key,
    keep: bool,
}

impl Body {
    fn finish(&mut self) {
        if let Some(w) = self.wire.take() {
            if self.keep && w.avail().is_empty() {
                if let Ok(mut p) = pool().lock() {
                    let v = p.entry(self.key.clone()).or_default();
                    if v.len() < 8 {
                        v.push(w);
                    }
                }
            }
        }
    }
}

impl Read for Body {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        let Some(w) = self.wire.as_mut() else { return Ok(0) };
        let n = match &mut self.framing {
            Framing::None => 0,
            Framing::Close => w.some(out, usize::MAX)?,
            Framing::Length(left) => {
                if *left == 0 {
                    0
                } else {
                    let n = w.some(out, (*left).min(usize::MAX as u64) as usize)?;
                    if n == 0 {
                        return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
                    }
                    *left -= n as u64;
                    n
                }
            }
            Framing::Chunked { left, done } => {
                if *done {
                    0
                } else {
                    if *left == 0 {
                        let l = w.line()?;
                        let text = String::from_utf8_lossy(&l);
                        let size = text.split(';').next().unwrap_or("").trim();
                        *left = u64::from_str_radix(size, 16).map_err(|_| bad("a broken chunk size"))?;
                        if *left == 0 {
                            // Trailers, up to the empty line.
                            while !w.line()?.is_empty() {}
                            *done = true;
                        }
                    }
                    if *done {
                        0
                    } else {
                        let n = w.some(out, (*left).min(usize::MAX as u64) as usize)?;
                        if n == 0 {
                            return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
                        }
                        *left -= n as u64;
                        if *left == 0 {
                            if !w.line()?.is_empty() {
                                return Err(bad("a chunk without its end"));
                            }
                        }
                        n
                    }
                }
            }
        };
        let ended = match &self.framing {
            Framing::None => true,
            Framing::Close => n == 0,
            Framing::Length(l) => *l == 0,
            Framing::Chunked { done, .. } => *done,
        };
        if ended {
            if matches!(self.framing, Framing::Close) {
                self.keep = false;
            }
            self.finish();
        }
        Ok(n)
    }
}

/// Sends one POST and reads the answer's head. `headers` are sent as given
/// (names already lower case), with Host and Content-Length added.
pub fn post(url: &str, headers: &[(String, Vec<u8>)], body: &[u8]) -> io::Result<Response> {
    let (tls, host, port, path) = target(url)?;
    let key: Key = (tls, host.clone(), port);
    let default_port = if tls { 443 } else { 80 };
    let host_header = if port == default_port {
        if host.contains(':') {
            format!("[{host}]")
        } else {
            host.clone()
        }
    } else if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    };
    let origin = netproxy::Origin { tls, host: host.clone(), port, explicit_port: true };
    // Through a proxy in the clear, the request line holds the whole URL
    // and the proxy's credentials come first, as httpcore sends them.
    let build = |c: &netproxy::Conn| {
        let mut req = Vec::with_capacity(512 + body.len());
        let line = netproxy::request_target(c, &origin, &path);
        req.extend_from_slice(format!("POST {line} HTTP/1.1\r\n{}host: {host_header}\r\n", netproxy::auth_line(c)).as_bytes());
        for (k, v) in headers {
            req.extend_from_slice(k.as_bytes());
            req.extend_from_slice(b": ");
            req.extend_from_slice(v);
            req.extend_from_slice(b"\r\n");
        }
        req.extend_from_slice(format!("content-length: {}\r\n\r\n", body.len()).as_bytes());
        req.extend_from_slice(body);
        req
    };
    // A kept connection the server closed while it sat idle is dropped
    // before use. One that fails while the request is written goes once
    // more on a new connection; after the request is written, never, so a
    // turn is never sent twice.
    loop {
        let pooled = take_live(&key);
        let reused = pooled.is_some();
        let mut w = match pooled {
            Some(w) => w,
            None => connect(&key)?,
        };
        let req = build(&w.conn);
        match send_and_read_head(&mut w, &req) {
            Ok((status, hs)) => return Ok(finish_head(w, key, status, hs)),
            Err((written, e)) => {
                if reused && !written {
                    continue;
                }
                return Err(e);
            }
        }
    }
}

/// An idle kept connection that is still open: nothing to read and no
/// end of stream.
fn take_live(key: &Key) -> Option<Wire> {
    loop {
        let w = pool().lock().ok().and_then(|mut p| p.get_mut(key).and_then(|v| v.pop()))?;
        let sock = &w.conn.sock;
        if sock.set_nonblocking(true).is_err() {
            continue;
        }
        let mut b = [0u8; 1];
        let idle = matches!(sock.peek(&mut b), Err(e) if e.kind() == io::ErrorKind::WouldBlock);
        if idle && sock.set_nonblocking(false).is_ok() {
            return Some(w);
        }
    }
}

type Head = (u16, Vec<(String, Vec<u8>)>);

/// Writes the request and reads the answer's head. An error says whether
/// the request was written whole.
fn send_and_read_head(w: &mut Wire, req: &[u8]) -> Result<Head, (bool, io::Error)> {
    if let Err(e) = w.conn.io.write_all(req).and_then(|_| w.conn.io.flush()) {
        return Err((false, e));
    }
    loop {
        let first = w.line().map_err(|e| (true, e))?;
        let text = String::from_utf8_lossy(&first).into_owned();
        let mut parts = text.splitn(3, ' ');
        let proto = parts.next().unwrap_or("");
        let status: u16 = match parts.next().and_then(|c| c.parse().ok()) {
            Some(s) if proto.starts_with("HTTP/") => s,
            _ => return Err((true, bad("the upstream answered something that is not HTTP"))),
        };
        let mut hs = vec![];
        loop {
            let l = w.line().map_err(|e| (true, e))?;
            if l.is_empty() {
                break;
            }
            let Some(i) = l.iter().position(|&c| c == b':') else { continue };
            let name = String::from_utf8_lossy(&l[..i]).trim().to_string();
            let mut v = &l[i + 1..];
            while v.first().is_some_and(|c| *c == b' ' || *c == b'\t') {
                v = &v[1..];
            }
            while v.last().is_some_and(|c| *c == b' ' || *c == b'\t') {
                v = &v[..v.len() - 1];
            }
            hs.push((name, v.to_vec()));
        }
        // An interim answer (100 Continue) is followed by the real one.
        if (100..200).contains(&status) && status != 101 {
            continue;
        }
        return Ok((status, hs));
    }
}

fn finish_head(w: Wire, key: Key, status: u16, headers: Vec<(String, Vec<u8>)>) -> Response {
    let get = |n: &str| -> Vec<String> {
        headers
            .iter()
            .filter(|(k, _)| k.eq_ignore_ascii_case(n))
            .map(|(_, v)| String::from_utf8_lossy(v).to_ascii_lowercase())
            .collect()
    };
    let close = get("connection").iter().any(|v| v.split(',').any(|t| t.trim() == "close"));
    let chunked = get("transfer-encoding").iter().any(|v| v.contains("chunked"));
    let length = get("content-length").first().and_then(|v| v.trim().parse::<u64>().ok());
    let framing = if status == 204 || status == 304 || (100..200).contains(&status) {
        Framing::None
    } else if chunked {
        Framing::Chunked { left: 0, done: false }
    } else if let Some(n) = length {
        Framing::Length(n)
    } else {
        Framing::Close
    };
    let keep = !close && !matches!(framing, Framing::Close);
    let mut body = Body { wire: Some(w), framing, key, keep };
    if matches!(body.framing, Framing::None | Framing::Length(0)) {
        body.finish();
    }
    Response { status, headers, body }
}

// ---------------------------------------------------------------------------
// Content decoding, as httpx does it
// ---------------------------------------------------------------------------

/// The upstream body with the decoders httpx applies: gzip and deflate, in
/// the reverse of their listed order; any other encoding is passed through
/// as it came.
pub fn decoded(resp: Response) -> (u16, Vec<(String, Vec<u8>)>, Box<dyn Read + Send>) {
    let mut encs = vec![];
    for v in resp.values("content-encoding") {
        for e in String::from_utf8_lossy(v).split(',') {
            let e = e.trim().to_ascii_lowercase();
            if e == "gzip" || e == "deflate" {
                encs.push(e);
            }
        }
    }
    let Response { status, headers, body } = resp;
    let mut rd: Box<dyn Read + Send> = Box::new(body);
    for e in encs.iter().rev() {
        rd = Box::new(Inflater::new(rd, if e == "gzip" { Kind::Gzip } else { Kind::Deflate }));
    }
    (status, headers, rd)
}

#[derive(Clone, Copy, PartialEq)]
enum Kind {
    Gzip,
    Deflate,
}

/// A streaming decoder: each read passes on what the input read so far
/// decodes to.
struct Inflater {
    src: Box<dyn Read + Send>,
    kind: Kind,
    state: Option<Box<InflateState>>,
    input: Vec<u8>,
    pos: usize,
    src_done: bool,
    ended: bool,
    crc: u32,
    size: u32,
}

impl Inflater {
    fn new(src: Box<dyn Read + Send>, kind: Kind) -> Inflater {
        Inflater { src, kind, state: None, input: vec![], pos: 0, src_done: false, ended: false, crc: !0, size: 0 }
    }

    fn pull(&mut self) -> io::Result<usize> {
        if self.src_done {
            return Ok(0);
        }
        if self.pos > 0 && self.pos == self.input.len() {
            self.input.clear();
            self.pos = 0;
        }
        let mut b = [0u8; 65536];
        let n = self.src.read(&mut b)?;
        if n == 0 {
            self.src_done = true;
        }
        self.input.extend_from_slice(&b[..n]);
        Ok(n)
    }

    /// At least `n` unread input bytes, or false at the input's end.
    fn need(&mut self, n: usize) -> io::Result<bool> {
        while self.input.len() - self.pos < n {
            if self.pull()? == 0 {
                return Ok(false);
            }
        }
        Ok(true)
    }

    /// Reads the gzip header, or picks zlib or raw deflate. False when the
    /// input is empty.
    fn start(&mut self) -> io::Result<bool> {
        if !self.need(1)? {
            return Ok(false);
        }
        let truncated = || io::Error::from(io::ErrorKind::UnexpectedEof);
        match self.kind {
            Kind::Deflate => {
                let two = self.need(2)?;
                let h = &self.input[self.pos..];
                let zlib = two && ((h[0] as u16) << 8 | h[1] as u16) % 31 == 0 && h[0] & 0x0f == 8;
                self.state = Some(InflateState::new_boxed(if zlib { DataFormat::Zlib } else { DataFormat::Raw }));
            }
            Kind::Gzip => {
                if !self.need(10)? {
                    return Err(truncated());
                }
                let h = self.input[self.pos..self.pos + 10].to_vec();
                if h[0] != 0x1f || h[1] != 0x8b || h[2] != 8 {
                    return Err(bad("gzip: invalid header"));
                }
                let flags = h[3];
                let mut at = 10;
                if flags & 4 != 0 {
                    if !self.need(at + 2)? {
                        return Err(truncated());
                    }
                    let x = &self.input[self.pos + at..];
                    at += 2 + (x[0] as usize | (x[1] as usize) << 8);
                }
                for bit in [8u8, 16] {
                    if flags & bit != 0 {
                        loop {
                            if !self.need(at + 1)? {
                                return Err(truncated());
                            }
                            at += 1;
                            if self.input[self.pos + at - 1] == 0 {
                                break;
                            }
                        }
                    }
                }
                if flags & 2 != 0 {
                    at += 2;
                }
                if !self.need(at)? {
                    return Err(truncated());
                }
                self.pos += at;
                self.state = Some(InflateState::new_boxed(DataFormat::Raw));
            }
        }
        Ok(true)
    }

    fn trailer(&mut self) -> io::Result<()> {
        if self.kind != Kind::Gzip {
            return Ok(());
        }
        if !self.need(8)? {
            return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
        }
        let t = &self.input[self.pos..self.pos + 8];
        let crc = u32::from_le_bytes([t[0], t[1], t[2], t[3]]);
        let size = u32::from_le_bytes([t[4], t[5], t[6], t[7]]);
        if crc != !self.crc || size != self.size {
            return Err(bad("gzip: checksum error"));
        }
        self.pos += 8;
        Ok(())
    }
}

impl Read for Inflater {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        if self.ended || out.is_empty() {
            return Ok(0);
        }
        if self.state.is_none() && !self.start()? {
            self.ended = true;
            return Ok(0);
        }
        loop {
            if self.pos == self.input.len() && !self.src_done {
                self.pull()?;
            }
            let flush = if self.src_done { MZFlush::Finish } else { MZFlush::None };
            let st = self.state.as_mut().expect("started");
            let r = inflate(st, &self.input[self.pos..], out, flush);
            self.pos += r.bytes_consumed;
            let wrote = r.bytes_written;
            if self.kind == Kind::Gzip {
                self.crc = crc32_update(self.crc, &out[..wrote]);
                self.size = self.size.wrapping_add(wrote as u32);
            }
            match r.status {
                Ok(MZStatus::StreamEnd) => {
                    self.trailer()?;
                    self.ended = true;
                    return Ok(wrote);
                }
                Ok(_) => {}
                Err(MZError::Buf) => {}
                Err(_) => return Err(bad("the compressed body does not decode")),
            }
            if wrote > 0 {
                return Ok(wrote);
            }
            if self.src_done && self.pos == self.input.len() {
                return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
            }
        }
    }
}

fn crc32_update(crc: u32, data: &[u8]) -> u32 {
    static TABLE: OnceLock<[u32; 256]> = OnceLock::new();
    let t = TABLE.get_or_init(|| {
        let mut t = [0u32; 256];
        for (i, e) in t.iter_mut().enumerate() {
            let mut c = i as u32;
            for _ in 0..8 {
                c = if c & 1 != 0 { 0xEDB88320 ^ (c >> 1) } else { c >> 1 };
            }
            *e = c;
        }
        t
    });
    let mut c = crc;
    for &b in data {
        c = t[((c ^ b as u32) & 0xff) as usize] ^ (c >> 8);
    }
    c
}

#[cfg(test)]
mod tests {
    use super::*;

    fn all(kind: Kind, data: &[u8], step: usize) -> io::Result<Vec<u8>> {
        struct Drip(Vec<u8>, usize, usize);
        impl Read for Drip {
            fn read(&mut self, b: &mut [u8]) -> io::Result<usize> {
                let n = self.2.min(self.0.len() - self.1).min(b.len());
                b[..n].copy_from_slice(&self.0[self.1..self.1 + n]);
                self.1 += n;
                Ok(n)
            }
        }
        let mut r = Inflater::new(Box::new(Drip(data.to_vec(), 0, step)), kind);
        let mut out = vec![];
        r.read_to_end(&mut out)?;
        Ok(out)
    }

    #[test]
    fn gzip_zlib_and_raw_deflate_decode_in_any_split() {
        let text = b"hello there, hello there, hello there".to_vec();
        let raw = miniz_oxide::deflate::compress_to_vec(&text, 6);
        let z = miniz_oxide::deflate::compress_to_vec_zlib(&text, 6);
        let mut gz = vec![0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff];
        gz.extend_from_slice(&raw);
        gz.extend_from_slice(&(!crc32_update(!0, &text)).to_le_bytes());
        gz.extend_from_slice(&(text.len() as u32).to_le_bytes());
        for step in [1, 3, 1000] {
            assert_eq!(all(Kind::Deflate, &raw, step).unwrap(), text);
            assert_eq!(all(Kind::Deflate, &z, step).unwrap(), text);
            assert_eq!(all(Kind::Gzip, &gz, step).unwrap(), text);
        }
        assert!(all(Kind::Gzip, &gz[..gz.len() - 3], 7).is_err());
        assert_eq!(all(Kind::Gzip, b"", 7).unwrap(), b"");
        assert!(all(Kind::Gzip, b"not gzip at all", 7).is_err());
    }

    #[test]
    fn paths_decode_as_uvicorn_and_quote_as_httpx() {
        assert_eq!(unquote("/v1/a%2Fb%41"), "/v1/a/bA");
        assert_eq!(unquote("/v1/%zz%"), "/v1/%zz%");
        assert_eq!(unquote("/v1/%C3%A9%FF"), "/v1/\u{e9}\u{fffd}");
        assert_eq!(request_target("/v1/a b/\u{e9}/%zz?x=1 2#f"), "/v1/a%20b/%C3%A9/%zz?x=1%202");
        assert_eq!(request_target("/v1/messages?beta=true"), "/v1/messages?beta=true");
        assert_eq!(request_target("?q"), "/?q");
    }

    #[test]
    fn urls_split_as_the_client_reads_them() {
        assert_eq!(target("http://127.0.0.1:9/v1/x?b=1").unwrap(), (false, "127.0.0.1".into(), 9, "/v1/x?b=1".into()));
        assert_eq!(target("https://api.anthropic.com").unwrap(), (true, "api.anthropic.com".into(), 443, "/".into()));
        assert_eq!(target("http://[::1]:8/p").unwrap(), (false, "::1".into(), 8, "/p".into()));
        assert!(target("ftp://x/").is_err());
    }
}
