//! One HTTP/1.1 POST with a time limit, over TCP or over rustls with the
//! ring provider and the system's root store, through a proxy when the
//! environment names one as httpx reads it (the oracle's model client is
//! httpx, LLM-11).

use std::io::{Read, Write};
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::netproxy;

#[derive(Debug)]
pub enum Fail {
    /// The time limit ran out.
    Timeout,
    /// The proxy answered the CONNECT with this status and reason.
    Proxy(u16, String),
    /// httpx cannot build a client for the proxy setting: its text.
    Fails(String),
    /// httpx.InvalidURL from a proxy setting: its text.
    Invalid(String),
    /// A proxy setting this binary does not read as httpx does, or a URL
    /// it does not send to.
    Unported(String),
    /// The server refused the connection (ECONNREFUSED).
    Refused,
    /// Writing the body out failed: the error's text.
    Write(String),
    /// Any other failure: the connection failed or broke.
    Other(String),
}

fn other(why: impl Into<String>) -> Fail {
    Fail::Other(why.into())
}

/// The URL split: TLS or not, host (an IPv6 address without brackets),
/// port, path.
fn target(url: &str) -> Result<(bool, String, u16, String), Fail> {
    let bad = || Fail::Unported(format!("the URL {url} is not one this binary sends to"));
    if !(url.starts_with("http://") || url.starts_with("https://")) || url.contains('@') {
        return Err(bad());
    }
    crate::gateway::http::target(url).map_err(|_| bad())
}

fn io_fail(e: std::io::Error) -> Fail {
    match e.kind() {
        std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock => Fail::Timeout,
        std::io::ErrorKind::ConnectionRefused => Fail::Refused,
        _ => other(e.to_string()),
    }
}

/// What came back: the status, the reply's text decoded as UTF-8 with
/// replacement, and its content-encoding header, if any.
pub struct Answer {
    pub status: u16,
    pub text: String,
    pub encoding: Option<String>,
}

/// POSTs `body` to `url`. `timeout_s` bounds the connect and every read
/// and write, and the whole exchange.
pub fn post(url: &str, headers: &[(&str, String)], body: &[u8], timeout_s: f64, proxies: &netproxy::Httpx) -> Result<Answer, Fail> {
    post_with(url, headers, body, timeout_s, proxies, None)
}

/// `post`, with `roots` in place of the system's root store (for the
/// tests only).
pub fn post_with(
    url: &str,
    headers: &[(&str, String)],
    body: &[u8],
    timeout_s: f64,
    proxies: &netproxy::Httpx,
    roots: Option<Arc<rustls::RootCertStore>>,
) -> Result<Answer, Fail> {
    let (status, raw, encoding, _) = exchange("POST", url, headers, Some(body), timeout_s, proxies, roots)?;
    Ok(Answer { status, text: crate::gate::py::text::decode_utf8_replace(&raw), encoding })
}

/// A GET of `url`: the status and the raw body.
pub fn get(url: &str, headers: &[(&str, String)], timeout_s: f64, proxies: &netproxy::Httpx) -> Result<(u16, Vec<u8>), Fail> {
    exchange("GET", url, headers, None, timeout_s, proxies, None).map(|(s, b, _, _)| (s, b))
}

/// A GET of `url`: the status, the raw body, its content-encoding and its
/// link header, if any.
pub fn get_head(
    url: &str,
    headers: &[(&str, String)],
    timeout_s: f64,
    proxies: &netproxy::Httpx,
) -> Result<(u16, Vec<u8>, Option<String>, Option<String>), Fail> {
    exchange("GET", url, headers, None, timeout_s, proxies, None)
}

/// A POST of `url`: the status and the raw body.
pub fn post_raw(url: &str, headers: &[(&str, String)], body: &[u8], timeout_s: f64, proxies: &netproxy::Httpx) -> Result<(u16, Vec<u8>), Fail> {
    exchange("POST", url, headers, Some(body), timeout_s, proxies, None).map(|(s, b, _, _)| (s, b))
}

/// One request of any method: the status, every header (name, value) in
/// order, and the raw body (dechunked; none for a HEAD).
pub fn request(
    method: &str,
    url: &str,
    headers: &[(&str, String)],
    timeout_s: f64,
    proxies: &netproxy::Httpx,
) -> Result<(u16, Vec<(String, String)>, Vec<u8>), Fail> {
    let mut all = vec![];
    let (status, body, _, _) = exchange_with(method, url, headers, None, timeout_s, proxies, None, Some(&mut all))?;
    Ok((status, all, body))
}

/// One request: the status, the raw body (dechunked), and its
/// content-encoding and link headers, if any.
fn exchange(
    method: &str,
    url: &str,
    headers: &[(&str, String)],
    body: Option<&[u8]>,
    timeout_s: f64,
    proxies: &netproxy::Httpx,
    roots: Option<Arc<rustls::RootCertStore>>,
) -> Result<(u16, Vec<u8>, Option<String>, Option<String>), Fail> {
    exchange_with(method, url, headers, body, timeout_s, proxies, roots, None)
}

/// Opens the connection (time limits as `dial` sets them) and sends the
/// request.
fn send(
    method: &str,
    url: &str,
    headers: &[(&str, String)],
    body: Option<&[u8]>,
    dial: &netproxy::Dial,
    proxies: &netproxy::Httpx,
) -> Result<netproxy::Conn, Fail> {
    let (tls, host, port, path) = target(url)?;
    let origin = netproxy::Origin { tls, host: host.clone(), port, explicit_port: true };
    let route = match proxies.route(&origin) {
        Ok(r) => r,
        Err(netproxy::Refusal::Fails(w)) => return Err(Fail::Fails(w)),
        Err(netproxy::Refusal::Invalid(w)) => return Err(Fail::Invalid(w)),
        Err(netproxy::Refusal::Unported(w)) => return Err(Fail::Unported(w)),
    };
    let mut conn = match netproxy::open(&route, &origin, dial) {
        Ok(c) => c,
        Err(netproxy::Fail::Connect(e) | netproxy::Fail::Io(e)) => return Err(io_fail(e)),
        Err(netproxy::Fail::Status(c, r)) => return Err(Fail::Proxy(c, r)),
    };
    let default_port = if tls { 443 } else { 80 };
    let bracketed = if host.contains(':') { format!("[{host}]") } else { host.clone() };
    let host_header = if port == default_port { bracketed } else { format!("{bracketed}:{port}") };
    let line = netproxy::request_target(&conn, &origin, &path);
    let mut req = format!("{method} {line} HTTP/1.1\r\n{}host: {host_header}\r\n", netproxy::auth_line(&conn));
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    match body {
        Some(b) => req.push_str(&format!("content-length: {}\r\nconnection: close\r\n\r\n", b.len())),
        None => req.push_str("connection: close\r\n\r\n"),
    }
    let mut out = req.into_bytes();
    out.extend_from_slice(body.unwrap_or_default());
    conn.io.write_all(&out).and_then(|_| conn.io.flush()).map_err(io_fail)?;
    Ok(conn)
}

/// One request whose 200 body is written to `out` as it arrives, so a
/// file of gigabytes costs a buffer, not its size: the status, every
/// header, and the bytes written. Another status's body is not read. Each
/// read waits at most five minutes; there is no overall limit.
pub fn request_to(
    method: &str,
    url: &str,
    headers: &[(&str, String)],
    proxies: &netproxy::Httpx,
    out: &mut dyn Write,
) -> Result<(u16, Vec<(String, String)>, u64), Fail> {
    let dial = netproxy::Dial { limit: None, connect_timeout: Some(Duration::from_secs(30)), roots: None };
    let conn = send(method, url, headers, None, &dial, proxies)?;
    let _ = conn.sock.set_read_timeout(Some(Duration::from_secs(300)));
    let mut r = std::io::BufReader::with_capacity(65_536, conn.io);
    let mut status: Option<u16> = None;
    let mut all = vec![];
    let mut total = 0usize;
    loop {
        let mut line = Vec::new();
        let n = std::io::BufRead::read_until(&mut r, b'\n', &mut line).map_err(io_fail)?;
        total += n;
        if n == 0 || total > 65_536 {
            return Err(other("the reply has no header end"));
        }
        let l = String::from_utf8_lossy(&line).trim_end_matches(['\r', '\n']).to_string();
        if status.is_none() {
            status = Some(l.split_whitespace().nth(1).and_then(|c| c.parse().ok()).ok_or_else(|| other("the reply has no status"))?);
            continue;
        }
        if l.is_empty() {
            break;
        }
        if let Some((k, v)) = l.split_once(':') {
            all.push((k.trim().to_string(), v.trim().to_string()));
        }
    }
    let status = status.unwrap_or(0);
    if method == "HEAD" || status != 200 {
        return Ok((status, all, 0));
    }
    let header = |name: &str| all.iter().find(|(k, _)| k.eq_ignore_ascii_case(name)).map(|(_, v)| v.clone());
    let chunked = header("transfer-encoding").is_some_and(|v| v.to_ascii_lowercase().contains("chunked"));
    let length: Option<u64> = header("content-length").and_then(|v| v.parse().ok());
    let mut written = 0u64;
    let mut buf = vec![0u8; 65_536];
    // Copies n bytes (None: to the end) from r to out.
    let mut copy = |r: &mut dyn Read, n: Option<u64>, written: &mut u64| -> Result<(), Fail> {
        let mut left = n;
        loop {
            let want = match left {
                Some(0) => return Ok(()),
                Some(k) => k.min(buf.len() as u64) as usize,
                None => buf.len(),
            };
            let got = r.read(&mut buf[..want]).map_err(io_fail)?;
            if got == 0 {
                return match left {
                    None => Ok(()),
                    Some(_) => Err(other("the reply was cut short")),
                };
            }
            out.write_all(&buf[..got]).map_err(|e| Fail::Write(e.to_string()))?;
            *written += got as u64;
            if let Some(k) = left.as_mut() {
                *k -= got as u64;
            }
        }
    };
    if chunked {
        loop {
            let mut line = Vec::new();
            std::io::BufRead::read_until(&mut r, b'\n', &mut line).map_err(io_fail)?;
            let text = String::from_utf8_lossy(&line);
            let hex = text.trim().split(';').next().unwrap_or("").trim().to_string();
            let n = u64::from_str_radix(&hex, 16).map_err(|_| other("the reply's chunks are broken"))?;
            if n == 0 {
                return Ok((status, all, written));
            }
            copy(&mut r, Some(n), &mut written)?;
            let mut crlf = Vec::new();
            std::io::BufRead::read_until(&mut r, b'\n', &mut crlf).map_err(io_fail)?;
        }
    }
    copy(&mut r, length, &mut written)?;
    Ok((status, all, written))
}

#[allow(clippy::too_many_arguments)]
fn exchange_with(
    method: &str,
    url: &str,
    headers: &[(&str, String)],
    body: Option<&[u8]>,
    timeout_s: f64,
    proxies: &netproxy::Httpx,
    roots: Option<Arc<rustls::RootCertStore>>,
    mut all: Option<&mut Vec<(String, String)>>,
) -> Result<(u16, Vec<u8>, Option<String>, Option<String>), Fail> {
    let limit = if timeout_s.is_finite() && timeout_s > 0.0 { Duration::from_secs_f64(timeout_s.min(1e9)) } else { Duration::from_secs(6000) };
    let dial = netproxy::Dial { limit: Some((Instant::now(), limit)), connect_timeout: None, roots };
    let mut conn = send(method, url, headers, body, &dial, proxies)?;
    let mut raw = Vec::new();
    conn.io.read_to_end(&mut raw).map_err(io_fail)?;
    let split = raw.windows(4).position(|w| w == b"\r\n\r\n").ok_or_else(|| other("the reply has no header end"))?;
    let head = String::from_utf8_lossy(&raw[..split]).into_owned();
    let mut rest = raw[split + 4..].to_vec();
    let mut lines = head.split("\r\n");
    let status: u16 = lines
        .next()
        .and_then(|l| l.split_whitespace().nth(1))
        .and_then(|c| c.parse().ok())
        .ok_or_else(|| other("the reply has no status"))?;
    let mut chunked = false;
    let mut length: Option<usize> = None;
    let mut encoding = None;
    let mut link = None;
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            if let Some(all) = all.as_deref_mut() {
                all.push((k.trim().to_string(), v.trim().to_string()));
            }
            match k.trim().to_ascii_lowercase().as_str() {
                "transfer-encoding" => chunked = v.to_ascii_lowercase().contains("chunked"),
                "content-length" => length = v.trim().parse().ok(),
                "content-encoding" => encoding = Some(v.trim().to_string()),
                "link" => link = Some(v.trim().to_string()),
                _ => {}
            }
        }
    }
    if method == "HEAD" {
        rest.clear();
    } else if chunked {
        rest = crate::gate::llm::dechunk(&rest).ok_or_else(|| other("the reply's chunks are broken"))?;
    } else if let Some(n) = length {
        if rest.len() < n {
            return Err(other("the reply was cut short"));
        }
        rest.truncate(n);
    }
    Ok((status, rest, encoding, link))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::TcpListener;

    fn tls_pair() -> (Arc<rustls::RootCertStore>, Arc<rustls::ServerConfig>) {
        let ca_key = rcgen::KeyPair::generate().unwrap();
        let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        let ca = rcgen::CertifiedIssuer::self_signed(ca_params, ca_key).unwrap();
        let leaf_key = rcgen::KeyPair::generate().unwrap();
        let leaf = rcgen::CertificateParams::new(vec!["localhost".to_string()]).unwrap().signed_by(&leaf_key, &ca).unwrap();
        let mut roots = rustls::RootCertStore::empty();
        roots.add(ca.der().clone()).unwrap();
        let key = rustls::pki_types::PrivateKeyDer::Pkcs8(leaf_key.serialize_der().into());
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let server = rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(vec![leaf.der().clone(), ca.der().clone()], key)
            .unwrap();
        (Arc::new(roots), Arc::new(server))
    }

    /// One request over TLS: the exact body and headers go out, and the
    /// reply comes back. A server that answers too late is a timeout, and
    /// the key is in no error text.
    #[test]
    fn a_request_over_tls_and_a_timeout() {
        let (roots, server) = tls_pair();
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        let h = std::thread::spawn(move || {
            let (sock, _) = l.accept().unwrap();
            let mut s = rustls::StreamOwned::new(rustls::ServerConnection::new(server).unwrap(), sock);
            let mut buf = vec![];
            let mut b = [0u8; 4096];
            while !String::from_utf8_lossy(&buf).contains("{\"a\":1}") {
                let k = s.read(&mut b).unwrap();
                buf.extend_from_slice(&b[..k]);
            }
            s.write_all(b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\n{}").unwrap();
            s.flush().unwrap();
            s.conn.send_close_notify();
            let _ = s.conn.write_tls(&mut s.sock);
            String::from_utf8_lossy(&buf).into_owned()
        });
        let headers = [("x-api-key", "sk-secret-value".to_string())];
        let r = post_with(&format!("https://localhost:{port}/v1/messages"), &headers, b"{\"a\":1}", 10.0, &netproxy::Httpx::none(), Some(roots.clone()));
        let req = h.join().unwrap();
        match r {
            Ok(a) if a.status == 200 => assert_eq!(a.text, "{}"),
            Ok(a) => panic!("{} {}", a.status, a.text),
            Err(_) => panic!("the request failed"),
        }
        assert!(req.starts_with("POST /v1/messages HTTP/1.1\r\nhost: localhost:"), "{req}");
        assert!(req.contains("x-api-key: sk-secret-value\r\n") && req.contains("content-length: 7\r\n"), "{req}");

        // A server that never answers: the time limit ends the wait.
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        let h = std::thread::spawn(move || {
            let (sock, _) = l.accept().unwrap();
            std::thread::sleep(Duration::from_millis(1500));
            drop(sock);
        });
        let t = Instant::now();
        let r = post_with(&format!("http://127.0.0.1:{port}/v1/messages"), &headers, b"{}", 0.3, &netproxy::Httpx::none(), None);
        assert!(matches!(r, Err(Fail::Timeout)), "not a timeout");
        assert!(t.elapsed() < Duration::from_millis(1200));
        h.join().unwrap();

        // A refused connection is a failure the caller words.
        let port = {
            let l = TcpListener::bind("127.0.0.1:0").unwrap();
            l.local_addr().unwrap().port()
        };
        match post_with(&format!("http://127.0.0.1:{port}/v1/messages"), &headers, b"{}", 5.0, &netproxy::Httpx::none(), None) {
            Err(Fail::Refused) => {}
            _ => panic!("a refused port answered"),
        }
    }

    /// One reply from a fresh listener, then the port.
    fn serve_once(reply: Vec<u8>) -> u16 {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            let (mut sock, _) = l.accept().unwrap();
            let mut buf = [0u8; 4096];
            let _ = sock.read(&mut buf);
            let _ = sock.write_all(&reply);
        });
        port
    }

    /// request_to writes a 200 body out (plain, chunked), reads no body of
    /// another status, and calls a body cut short a failure; a closed
    /// connection is not a refused one.
    #[test]
    fn request_to_streams_a_body() {
        let big = vec![b'x'; 300_000];
        let mut reply = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\nX-Repo-Commit: c\r\n\r\n", big.len()).into_bytes();
        reply.extend_from_slice(&big);
        let port = serve_once(reply);
        let mut out = Vec::new();
        let (status, headers, n) = request_to("GET", &format!("http://127.0.0.1:{port}/f"), &[], &netproxy::Httpx::none(), &mut out).unwrap();
        assert_eq!((status, n, out.len()), (200, 300_000, 300_000));
        assert!(headers.iter().any(|(k, v)| k == "X-Repo-Commit" && v == "c"));

        let port = serve_once(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n".to_vec());
        let mut out = Vec::new();
        let (_, _, n) = request_to("GET", &format!("http://127.0.0.1:{port}/f"), &[], &netproxy::Httpx::none(), &mut out).unwrap();
        assert_eq!((n, out.as_slice()), (5, &b"abcde"[..]));

        let port = serve_once(b"HTTP/1.1 404 Not Found\r\nContent-Length: 3\r\n\r\nno!".to_vec());
        let mut out = Vec::new();
        let (status, _, n) = request_to("GET", &format!("http://127.0.0.1:{port}/f"), &[], &netproxy::Httpx::none(), &mut out).unwrap();
        assert_eq!((status, n, out.len()), (404, 0, 0));

        let port = serve_once(b"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nabcd".to_vec());
        let mut out = Vec::new();
        let r = request_to("GET", &format!("http://127.0.0.1:{port}/f"), &[], &netproxy::Httpx::none(), &mut out);
        assert!(matches!(r, Err(Fail::Other(_))), "a cut-short body passed");

        let port = serve_once(Vec::new());
        let r = request_to("GET", &format!("http://127.0.0.1:{port}/f"), &[], &netproxy::Httpx::none(), &mut Vec::new());
        assert!(matches!(r, Err(Fail::Other(_))), "a closed connection was not a failure");
    }
}
