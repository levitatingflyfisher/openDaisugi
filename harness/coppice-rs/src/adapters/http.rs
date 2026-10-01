//! A small HTTP/1.1 client for the opencode adapter: loopback only, basic
//! auth, a JSON body, and a body read by length, by chunks or to the end.
//! The event stream reads its body as it comes.

use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::time::Duration;

/// The base64 of b, for the basic auth header.
fn base64(b: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for c in b.chunks(3) {
        let n = (c[0] as u32) << 16
            | (*c.get(1).unwrap_or(&0) as u32) << 8
            | *c.get(2).unwrap_or(&0) as u32;
        out.push(T[(n >> 18) as usize & 63] as char);
        out.push(T[(n >> 12) as usize & 63] as char);
        out.push(if c.len() > 1 {
            T[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if c.len() > 2 {
            T[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}

/// Go's url.PathEscape.
pub fn path_escape(s: &str) -> String {
    let mut out = String::new();
    for &b in s.as_bytes() {
        let keep = b.is_ascii_alphanumeric()
            || matches!(
                b,
                b'-' | b'_' | b'.' | b'~' | b'$' | b'&' | b'+' | b'=' | b':' | b'@'
            );
        if keep {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// A response: its status line text after the protocol, its code, and
/// its body reader.
pub struct Response {
    pub status: String,
    pub code: u16,
    pub body: Body,
}

/// How the body ends.
enum Framing {
    Length(u64),
    Chunked { left: u64, done: bool },
    Close,
}

/// A response body.
pub struct Body {
    r: BufReader<TcpStream>,
    framing: Framing,
}

impl Read for Body {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match &mut self.framing {
            Framing::Close => self.r.read(buf),
            Framing::Length(left) => {
                if *left == 0 {
                    return Ok(0);
                }
                let n = buf.len().min(*left as usize);
                let got = self.r.read(&mut buf[..n])?;
                if got == 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "unexpected EOF",
                    ));
                }
                *left -= got as u64;
                Ok(got)
            }
            Framing::Chunked { left, done } => {
                if *done {
                    return Ok(0);
                }
                if *left == 0 {
                    let mut line = String::new();
                    self.r.read_line(&mut line)?;
                    if line.trim().is_empty() {
                        line.clear();
                        self.r.read_line(&mut line)?;
                    }
                    let size = line.trim().split(';').next().unwrap_or("");
                    let n = u64::from_str_radix(size, 16).map_err(|_| {
                        io::Error::new(io::ErrorKind::InvalidData, "malformed chunked encoding")
                    })?;
                    if n == 0 {
                        *done = true;
                        return Ok(0);
                    }
                    *left = n;
                }
                let n = buf.len().min(*left as usize);
                let got = self.r.read(&mut buf[..n])?;
                if got == 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "unexpected EOF",
                    ));
                }
                *left -= got as u64;
                Ok(got)
            }
        }
    }
}

/// A client for one base URL, http://127.0.0.1:PORT.
pub struct Client {
    pub base: String,
    host: String,
    auth: String,
}

impl Client {
    pub fn new(base: &str, user: &str, password: &str) -> Client {
        Client {
            base: base.to_string(),
            host: base.trim_start_matches("http://").to_string(),
            auth: base64(format!("{user}:{password}").as_bytes()),
        }
    }

    /// Sends one request. timeout bounds the connect and every read; None
    /// waits as long as it takes.
    pub fn send(
        &self,
        method: &str,
        path: &str,
        body: Option<&str>,
        accept: Option<&str>,
        timeout: Option<Duration>,
    ) -> Result<Response, String> {
        let url = format!("{}{path}", self.base);
        let word = method[..1].to_string() + &method[1..].to_lowercase();
        let fail = |e: &dyn std::fmt::Display| format!("{word} \"{url}\": {e}");
        let addr = self
            .host
            .parse::<std::net::SocketAddr>()
            .map_err(|e| fail(&e))?;
        let s = match timeout {
            Some(t) => TcpStream::connect_timeout(&addr, t),
            None => TcpStream::connect(addr),
        }
        .map_err(|e| {
            fail(&format!(
                "dial tcp {addr}: connect: {}",
                crate::sys::go_errno_text(&e)
            ))
        })?;
        s.set_read_timeout(timeout).map_err(|e| fail(&e))?;
        let mut req = format!(
            "{method} {path} HTTP/1.1\r\nHost: {}\r\nUser-Agent: Go-http-client/1.1\r\nAuthorization: Basic {}\r\nConnection: close\r\n",
            self.host, self.auth
        );
        if let Some(a) = accept {
            req.push_str(&format!("Accept: {a}\r\n"));
        }
        match body {
            Some(b) => {
                req.push_str(&format!(
                    "Content-Length: {}\r\nContent-Type: application/json\r\n\r\n{b}",
                    b.len()
                ));
            }
            None if method == "POST" => req.push_str("Content-Length: 0\r\n\r\n"),
            None => req.push_str("\r\n"),
        }
        (&s).write_all(req.as_bytes()).map_err(|e| fail(&e))?;
        let mut r = BufReader::new(s);
        let mut line = String::new();
        r.read_line(&mut line).map_err(|e| fail(&e))?;
        let line = line.trim_end();
        let (_, rest) = line
            .split_once(' ')
            .ok_or_else(|| fail(&format!("malformed HTTP response {line:?}")))?;
        let status = rest.trim_start().to_string();
        let code: u16 = status
            .get(..3)
            .and_then(|c| c.parse().ok())
            .ok_or_else(|| fail(&format!("malformed HTTP status code {status:?}")))?;
        let mut framing = Framing::Close;
        loop {
            let mut h = String::new();
            if r.read_line(&mut h).map_err(|e| fail(&e))? == 0 {
                break;
            }
            let h = h.trim_end();
            if h.is_empty() {
                break;
            }
            if let Some((k, v)) = h.split_once(':') {
                let k = k.trim().to_ascii_lowercase();
                let v = v.trim();
                if k == "content-length" {
                    if let Ok(n) = v.parse() {
                        if !matches!(framing, Framing::Chunked { .. }) {
                            framing = Framing::Length(n);
                        }
                    }
                } else if k == "transfer-encoding" && v.eq_ignore_ascii_case("chunked") {
                    framing = Framing::Chunked {
                        left: 0,
                        done: false,
                    };
                }
            }
        }
        if method == "HEAD" || code == 204 || code == 304 {
            framing = Framing::Length(0);
        }
        Ok(Response {
            status,
            code,
            body: Body { r, framing },
        })
    }

    /// One call: the body of a 2xx, else "opencode: WHAT: STATUS: BODY".
    pub fn call(
        &self,
        what: &str,
        method: &str,
        path: &str,
        body: Option<&str>,
    ) -> Result<Vec<u8>, String> {
        let resp = self
            .send(method, path, body, None, Some(Duration::from_secs(10)))
            .map_err(|e| format!("opencode: {what}: {e}"))?;
        let mut b = Vec::new();
        let _ = resp.body.take(1 << 20).read_to_end(&mut b);
        if !(200..=299).contains(&resp.code) {
            let text = String::from_utf8_lossy(&b);
            return Err(format!(
                "opencode: {what}: {}: {}",
                resp.status,
                text.trim()
            ));
        }
        Ok(b)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_and_path_escape() {
        assert_eq!(base64(b"opencode:pw"), "b3BlbmNvZGU6cHc=");
        assert_eq!(base64(b"ab"), "YWI=");
        assert_eq!(path_escape("ses_1/a b?c"), "ses_1%2Fa%20b%3Fc");
    }
}
