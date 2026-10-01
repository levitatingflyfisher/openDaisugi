//! The websocket: one browser, one connection to the coppice server, and
//! JSON lines copied through untouched both ways. The handshake checks
//! and the close codes and words are those of coder/websocket v1.8.15,
//! which the Go server uses, with compression off as it is there.

use std::io::{self, BufRead, BufReader, Read, Write};
use std::sync::mpsc::TryRecvError;
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use super::http::{self, Request, Response, Stream};
use super::peer;
use super::upstream::{Dialer, Session, MAX_REQUEST_BYTES};

/// The only subprotocol the server names.
pub const SUBPROTOCOL: &str = "daisugi.v1";
/// A subprotocol offer that carries the token.
pub const BEARER_PREFIX: &str = "daisugi.bearer.";

const STATUS_PROTOCOL_ERROR: u16 = 1002;
const STATUS_MESSAGE_TOO_BIG: u16 = 1009;
const STATUS_INTERNAL_ERROR: u16 = 1011;

/// The token from Authorization, then from a subprotocol offer.
pub fn bearer_from(req: &Request) -> String {
    let h = req.header("Authorization");
    if let Some(t) = h.strip_prefix("Bearer ") {
        return t.trim().to_string();
    }
    for raw in req.header_values("Sec-Websocket-Protocol") {
        for part in raw.split(',') {
            let part = part.trim();
            if let Some(t) = part.strip_prefix(BEARER_PREFIX) {
                return t.to_string();
            }
        }
    }
    String::new()
}

/// The comma-separated tokens of every value of key.
fn header_tokens(req: &Request, key: &str) -> Vec<String> {
    let mut out = Vec::new();
    for v in req.header_values(key) {
        for t in v.trim().split(',') {
            out.push(t.trim().to_string());
        }
    }
    out
}

fn has_token(req: &Request, key: &str, token: &str) -> bool {
    header_tokens(req, key)
        .iter()
        .any(|t| t.eq_ignore_ascii_case(token))
}

/// Go's url.Parse, far enough to find an Origin's host: the error words
/// for a bad escape, and the host after the scheme.
fn origin_host(origin: &str) -> Result<String, String> {
    let b = origin.as_bytes();
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' {
            let ok = b.get(i + 1).is_some_and(|c| c.is_ascii_hexdigit())
                && b.get(i + 2).is_some_and(|c| c.is_ascii_hexdigit());
            if !ok {
                let end = (i + 3).min(b.len());
                return Err(format!(
                    "parse {}: invalid URL escape {}",
                    crate::sys::go_quote(origin),
                    crate::sys::go_quote(&origin[i..end])
                ));
            }
            i += 3;
            continue;
        }
        i += 1;
    }
    let Some((scheme, rest)) = origin.split_once("://") else {
        return Ok(String::new());
    };
    if scheme.is_empty() {
        return Err(format!(
            "parse {}: missing protocol scheme",
            crate::sys::go_quote(origin)
        ));
    }
    let auth = rest.split(['/', '?', '#']).next().unwrap_or("");
    let auth = auth.rsplit('@').next().unwrap_or(auth);
    // Go's parseHost: the port after the host must be digits.
    let port_part = if auth.starts_with('[') {
        match auth.find(']') {
            Some(i) => &auth[i + 1..],
            None => {
                return Err(format!(
                    "parse {}: missing ']' in host",
                    crate::sys::go_quote(origin)
                ))
            }
        }
    } else {
        match auth.find(':') {
            Some(i) => &auth[i..],
            None => "",
        }
    };
    let valid = port_part.is_empty()
        || (port_part.starts_with(':') && port_part[1..].bytes().all(|b| b.is_ascii_digit()));
    if !valid {
        return Err(format!(
            "parse {}: invalid port {} after host",
            crate::sys::go_quote(origin),
            crate::sys::go_quote(port_part)
        ));
    }
    Ok(auth.to_string())
}

/// Checks the upgrade as coder/websocket's Accept does. Ok carries the 101
/// response to write; Err the refusal.
pub fn accept(req: &Request, mut resp: Response) -> Result<Response, Response> {
    let q = crate::sys::go_quote;
    let fail = |mut r: Response, code: u16, msg: String| {
        http::error(&mut r, &msg, code);
        r
    };
    if req.proto_minor < 1 {
        return Err(fail(
            resp,
            426,
            format!(
                "WebSocket protocol violation: handshake request must be at least HTTP/1.1: {}",
                q("HTTP/1.0")
            ),
        ));
    }
    if !has_token(req, "Connection", "Upgrade") {
        resp.set("Connection", "Upgrade");
        resp.set("Upgrade", "websocket");
        return Err(fail(
            resp,
            426,
            format!(
                "WebSocket protocol violation: Connection header {} does not contain Upgrade",
                q(req.header("Connection"))
            ),
        ));
    }
    if !has_token(req, "Upgrade", "websocket") {
        resp.set("Connection", "Upgrade");
        resp.set("Upgrade", "websocket");
        return Err(fail(
            resp,
            426,
            format!(
                "WebSocket protocol violation: Upgrade header {} does not contain websocket",
                q(req.header("Upgrade"))
            ),
        ));
    }
    if req.method != "GET" {
        return Err(fail(
            resp,
            405,
            format!(
                "WebSocket protocol violation: handshake request method is not GET but {}",
                q(&req.method)
            ),
        ));
    }
    if req.header("Sec-Websocket-Version") != "13" {
        resp.set("Sec-Websocket-Version", "13");
        return Err(fail(
            resp,
            400,
            format!(
                "unsupported WebSocket protocol version (only 13 is supported): {}",
                q(req.header("Sec-Websocket-Version"))
            ),
        ));
    }
    let keys = req.header_values("Sec-Websocket-Key");
    if keys.is_empty() {
        return Err(fail(
            resp,
            400,
            "WebSocket protocol violation: missing Sec-WebSocket-Key".into(),
        ));
    }
    if keys.len() > 1 {
        return Err(fail(
            resp,
            400,
            "WebSocket protocol violation: multiple Sec-WebSocket-Key headers".into(),
        ));
    }
    let key = keys[0].trim().to_string();
    match super::der::unbase64(&key) {
        Some(v) if v.len() == 16 && !key.contains(char::is_whitespace) => {}
        _ => {
            return Err(fail(
                resp,
                400,
                format!(
                    "WebSocket protocol violation: invalid Sec-WebSocket-Key {}, must be a 16 byte base64 encoded string",
                    q(&key)
                ),
            ))
        }
    }
    let origin = req.header("Origin");
    if !origin.is_empty() {
        match origin_host(origin) {
            Err(e) => {
                return Err(fail(
                    resp,
                    403,
                    format!("failed to parse Origin header {}: {e}", q(origin)),
                ))
            }
            Ok(host) => {
                if !req.host.eq_ignore_ascii_case(&host) {
                    let msg = if host.is_empty() {
                        format!(
                            "request Origin {} is not a valid URL with a host",
                            q(origin)
                        )
                    } else {
                        format!(
                            "request Origin {} is not authorized for Host {}",
                            q(&host),
                            q(&req.host)
                        )
                    };
                    return Err(fail(resp, 403, msg));
                }
            }
        }
    }
    resp.set("Upgrade", "websocket");
    resp.set("Connection", "Upgrade");
    let accept = format!("{key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11");
    resp.set(
        "Sec-Websocket-Accept",
        &super::der::base64(&crate::sha1::digest(accept.as_bytes())),
    );
    for cp in header_tokens(req, "Sec-Websocket-Protocol") {
        if cp.eq_ignore_ascii_case(SUBPROTOCOL) {
            resp.set("Sec-Websocket-Protocol", &cp);
            break;
        }
    }
    resp.status = 101;
    Ok(resp)
}

/// One frame read from the browser.
struct Frame {
    fin: bool,
    opcode: u8,
    payload: Vec<u8>,
}

/// The connection after the handshake, read with a short timeout so one
/// thread can both read the browser and write the server's lines.
struct Conn {
    io: BufReader<Box<dyn Stream>>,
    buf: Vec<u8>,
    close_sent: bool,
    close_got: bool,
}

/// Why reading stopped.
enum Stop {
    /// The browser closed, or the stream ended or failed.
    Gone,
    /// A protocol error: a close frame with this code and reason was
    /// written.
    Protocol,
}

impl Conn {
    fn write_frame(&mut self, opcode: u8, payload: &[u8]) -> io::Result<()> {
        let mut out = vec![0x80 | opcode];
        let n = payload.len();
        if n < 126 {
            out.push(n as u8);
        } else if n < 1 << 16 {
            out.push(126);
            out.extend_from_slice(&(n as u16).to_be_bytes());
        } else {
            out.push(127);
            out.extend_from_slice(&(n as u64).to_be_bytes());
        }
        out.extend_from_slice(payload);
        let w = self.io.get_mut();
        w.write_all(&out)?;
        w.flush()
    }

    fn write_close(&mut self, code: u16, reason: &str) {
        if self.close_sent {
            return;
        }
        self.close_sent = true;
        let mut p = code.to_be_bytes().to_vec();
        p.extend_from_slice(reason.as_bytes());
        let _ = self.write_frame(8, &p);
    }

    /// Fills buf with what the stream has, waiting at most the read
    /// timeout. Ok(false) when nothing came; Err when the stream ended.
    fn fill(&mut self) -> io::Result<bool> {
        let mut chunk = [0u8; 16384];
        match self.io.read(&mut chunk) {
            Ok(0) => Err(io::ErrorKind::UnexpectedEof.into()),
            Ok(n) => {
                self.buf.extend_from_slice(&chunk[..n]);
                Ok(true)
            }
            Err(e)
                if e.kind() == io::ErrorKind::WouldBlock || e.kind() == io::ErrorKind::TimedOut =>
            {
                Ok(false)
            }
            Err(e) => Err(e),
        }
    }

    /// One whole frame from buf, if buf holds one. Err for a frame the
    /// server refuses before its payload, with the close already written.
    fn take_frame(&mut self, limit_left: Option<u64>) -> Result<Option<Frame>, Stop> {
        let b = &self.buf;
        if b.len() < 2 {
            return Ok(None);
        }
        let fin = b[0] & 0x80 != 0;
        let rsv = (b[0] >> 4) & 7;
        let opcode = b[0] & 0x0f;
        let masked = b[1] & 0x80 != 0;
        let mut n = (b[1] & 0x7f) as u64;
        let mut at = 2;
        if n == 126 {
            if b.len() < 4 {
                return Ok(None);
            }
            n = u16::from_be_bytes([b[2], b[3]]) as u64;
            at = 4;
        } else if n == 127 {
            if b.len() < 10 {
                return Ok(None);
            }
            n = u64::from_be_bytes([b[2], b[3], b[4], b[5], b[6], b[7], b[8], b[9]]);
            at = 10;
        }
        if rsv != 0 {
            let r = |i: u8| rsv & (4 >> i) != 0;
            self.write_close(
                STATUS_PROTOCOL_ERROR,
                &format!(
                    "received header with unexpected rsv bits set: {}:{}:{}",
                    r(0),
                    r(1),
                    r(2)
                ),
            );
            return Err(Stop::Protocol);
        }
        if !masked {
            return Err(Stop::Gone);
        }
        if let Some(left) = limit_left {
            if (opcode <= 2) && n >= left {
                // The read limit: coder/websocket reads the message up to
                // the limit, then refuses with this close.
                if b.len() < at + 4 + left as usize {
                    return Ok(None);
                }
                self.buf.drain(..at + 4 + left as usize);
                self.write_close(
                    STATUS_MESSAGE_TOO_BIG,
                    &format!("read limited at {} bytes", MAX_REQUEST_BYTES),
                );
                return Err(Stop::Protocol);
            }
        }
        if opcode >= 8 && (n > 125 || !fin) {
            let reason = if n > 125 {
                format!("received control frame payload with invalid length: {n}")
            } else {
                "received fragmented control frame".to_string()
            };
            self.write_close(STATUS_PROTOCOL_ERROR, &reason);
            return Err(Stop::Protocol);
        }
        let need = at + 4 + n as usize;
        if b.len() < need {
            return Ok(None);
        }
        let key = [b[at], b[at + 1], b[at + 2], b[at + 3]];
        let payload: Vec<u8> = b[at + 4..need]
            .iter()
            .enumerate()
            .map(|(i, x)| x ^ key[i % 4])
            .collect();
        self.buf.drain(..need);
        Ok(Some(Frame {
            fin,
            opcode,
            payload,
        }))
    }
}

/// Runs the websocket after its 101: the hello upstream first, then lines
/// both ways until either side goes.
pub fn serve(
    io: BufReader<Box<dyn Stream>>,
    dial: &Dialer,
    name: &str,
    req: &Request,
    set_timeout: &dyn Fn(Option<Duration>),
) {
    set_timeout(Some(Duration::from_millis(20)));
    let mut c = Conn {
        buf: io.buffer().to_vec(),
        io,
        close_sent: false,
        close_got: false,
    };
    // What the BufReader held is now in buf.
    let held = c.buf.len();
    c.io.consume(held);
    let sess = match dial.open() {
        Ok(s) => s,
        Err(e) => {
            super::log(
                "WARN",
                "web: coppice-server is not reachable",
                &[("err", &e)],
            );
            close(
                &mut c,
                STATUS_INTERNAL_ERROR,
                "coppice-server is not reachable",
            );
            return;
        }
    };
    let mut hello: Vec<(&str, Value)> = vec![
        ("id", json!(peer::WEB_HELLO_ID)),
        ("cmd", json!("hello")),
        ("name_from", json!("token")),
    ];
    if !name.is_empty() {
        hello.push(("name", json!(name)));
    }
    if let Some(p) = peer::peer_hello(req.remote, req.local) {
        hello.extend(p);
    }
    let line = crate::gojson::marshal(&crate::gojson::map(hello));
    if let Err(e) = sess.send(line.as_bytes()) {
        super::log(
            "WARN",
            "web: could not name the client upstream",
            &[("err", &e)],
        );
        sess.close();
        close(
            &mut c,
            STATUS_INTERNAL_ERROR,
            "coppice-server is not reachable",
        );
        return;
    }
    if let Some(reason) = await_hello(&sess) {
        super::log(
            "WARN",
            "web: the server did not accept the client's hello",
            &[("reason", reason)],
        );
        sess.close();
        close(&mut c, STATUS_INTERNAL_ERROR, reason);
        return;
    }
    pump(&mut c, &sess);
    sess.close();
}

/// Reads the reply to the web hello. None when the server accepted it;
/// else the close reason.
fn await_hello(sess: &Session) -> Option<&'static str> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let left = deadline.saturating_duration_since(Instant::now());
        match sess.lines.recv_timeout(left) {
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                return Some("coppice-server did not answer")
            }
            Err(_) => return Some("coppice-server closed the connection"),
            Ok(line) => {
                if !peer::is_web_hello_reply(&line) {
                    continue;
                }
                let ok = crate::godec::decode_map(&line)
                    .map(|m| matches!(m.get("ok"), Some(crate::godec::Any::Bool(true))))
                    .unwrap_or(false);
                return if ok {
                    None
                } else {
                    Some("coppice-server refused this client")
                };
            }
        }
    }
}

/// Copies lines both ways until the browser or the server goes.
fn pump(c: &mut Conn, sess: &Session) {
    // A data message being read: its opcode and bytes so far.
    let mut msg: Option<(u8, Vec<u8>)> = None;
    // A message may hold this many bytes less one: at this many, the
    // limit reader refuses.
    let limit = MAX_REQUEST_BYTES as u64;
    loop {
        // The server's lines to the browser.
        loop {
            match sess.lines.try_recv() {
                Ok(line) => {
                    if c.write_frame(1, &line).is_err() {
                        return;
                    }
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => {
                    close(
                        c,
                        STATUS_INTERNAL_ERROR,
                        "coppice-server closed the connection",
                    );
                    return;
                }
            }
        }
        // The browser's frames to the server.
        loop {
            let used = msg.as_ref().map(|(_, b)| b.len() as u64).unwrap_or(0);
            let frame = match c.take_frame(Some(limit.saturating_sub(used))) {
                Ok(Some(f)) => f,
                Ok(None) => break,
                Err(_) => {
                    sess.close();
                    close(
                        c,
                        STATUS_INTERNAL_ERROR,
                        "coppice-server closed the connection",
                    );
                    return;
                }
            };
            match frame.opcode {
                8 => {
                    c.close_got = true;
                    let p = &frame.payload;
                    if p.is_empty() {
                        // No status: the echo carries none either.
                        if !c.close_sent {
                            c.close_sent = true;
                            let _ = c.write_frame(8, &[]);
                        }
                    } else if p.len() < 2 {
                        c.write_close(
                            STATUS_PROTOCOL_ERROR,
                            &format!(
                                "received invalid close payload: close payload {} too small, cannot even contain the 2 byte status code",
                                crate::sys::go_quote(&String::from_utf8_lossy(p))
                            ),
                        );
                    } else {
                        let code = u16::from_be_bytes([p[0], p[1]]);
                        if valid_wire_close_code(code) {
                            let reason = String::from_utf8_lossy(&p[2..]).into_owned();
                            c.write_close(code, &reason);
                        } else {
                            c.write_close(
                                STATUS_PROTOCOL_ERROR,
                                &format!(
                                    "received invalid close payload: invalid status code StatusCode({code})"
                                ),
                            );
                        }
                    }
                    return;
                }
                9 => {
                    if c.write_frame(10, &frame.payload).is_err() {
                        return;
                    }
                }
                10 => {}
                0 => {
                    let Some((op, mut bytes)) = msg.take() else {
                        c.write_close(
                            STATUS_PROTOCOL_ERROR,
                            "received continuation frame without text or binary frame",
                        );
                        return;
                    };
                    bytes.extend_from_slice(&frame.payload);
                    if frame.fin {
                        if !deliver(op, &bytes, sess) {
                            close(
                                c,
                                STATUS_INTERNAL_ERROR,
                                "coppice-server closed the connection",
                            );
                            return;
                        }
                    } else {
                        msg = Some((op, bytes));
                    }
                }
                1 | 2 => {
                    if msg.is_some() {
                        c.write_close(
                            STATUS_PROTOCOL_ERROR,
                            "received new data message without finishing the previous message",
                        );
                        return;
                    }
                    if frame.fin {
                        if !deliver(frame.opcode, &frame.payload, sess) {
                            close(
                                c,
                                STATUS_INTERNAL_ERROR,
                                "coppice-server closed the connection",
                            );
                            return;
                        }
                    } else {
                        msg = Some((frame.opcode, frame.payload));
                    }
                }
                op => {
                    c.write_close(
                        STATUS_PROTOCOL_ERROR,
                        &format!("received unknown opcode opcode({op})"),
                    );
                    return;
                }
            }
        }
        match c.fill() {
            Ok(_) => {}
            Err(_) => return,
        }
    }
}

/// coder/websocket's validWireCloseCode.
fn valid_wire_close_code(code: u16) -> bool {
    match code {
        1004 | 1005 | 1006 | 1015 => false,
        1000..=1014 => true,
        3000..=4999 => true,
        _ => false,
    }
}

/// Sends a text message upstream; a binary one is dropped. False when the
/// send failed.
fn deliver(op: u8, bytes: &[u8], sess: &Session) -> bool {
    if op != 1 {
        return true;
    }
    if let Err(e) = sess.send(bytes) {
        super::log(
            "WARN",
            "web: could not forward a line to coppice-server",
            &[("err", &e)],
        );
        return false;
    }
    true
}

/// coder/websocket's Close: write the close frame, then wait up to five
/// seconds for the browser's own close before the connection ends.
fn close(c: &mut Conn, code: u16, reason: &str) {
    if c.close_sent {
        return;
    }
    c.write_close(code, reason);
    if c.close_got {
        return;
    }
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        loop {
            match c.take_frame(None) {
                Ok(Some(f)) if f.opcode == 8 => return,
                Ok(Some(_)) => continue,
                Ok(None) => break,
                Err(_) => return,
            }
        }
        if c.fill().is_err() {
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_accept_key_is_rfc_6455s() {
        let k = "dGhlIHNhbXBsZSBub25jZQ==258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
        assert_eq!(
            super::super::der::base64(&crate::sha1::digest(k.as_bytes())),
            "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
        );
    }

    #[test]
    fn origins_parse_as_go_parses_them() {
        assert_eq!(origin_host("https://evil.test").unwrap(), "evil.test");
        assert_eq!(origin_host("http://a:1/x").unwrap(), "a:1");
        assert!(origin_host("%zz")
            .unwrap_err()
            .contains("invalid URL escape \"%zz\""));
    }
}
