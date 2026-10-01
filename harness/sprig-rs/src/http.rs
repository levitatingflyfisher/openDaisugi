//! One HTTP/1.1 POST and its reply, for the API backend: http, or https
//! checked against the system's CA bundle as Go's client does on Linux.
//! Errors read as Go's net/http words them.

use std::io::{Read, Write};
use std::net::{SocketAddr, TcpStream, ToSocketAddrs};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

/// The files Go's crypto/x509 reads for the system roots on Linux.
const CERT_FILES: &[&str] = &[
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/pki/tls/certs/ca-bundle.crt",
    "/etc/ssl/ca-bundle.pem",
    "/etc/pki/tls/cacert.pem",
    "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
    "/etc/ssl/cert.pem",
];

fn unbase64(s: &[u8]) -> Option<Vec<u8>> {
    let val = |c: u8| -> Option<u32> {
        Some(match c {
            b'A'..=b'Z' => (c - b'A') as u32,
            b'a'..=b'z' => (c - b'a' + 26) as u32,
            b'0'..=b'9' => (c - b'0' + 52) as u32,
            b'+' => 62,
            b'/' => 63,
            _ => return None,
        })
    };
    let clean: Vec<u8> = s
        .iter()
        .copied()
        .filter(|b| !b.is_ascii_whitespace())
        .collect();
    if clean.len() % 4 != 0 {
        return None;
    }
    let mut out = Vec::new();
    for q in clean.chunks(4) {
        let pad = q.iter().rev().take_while(|&&c| c == b'=').count();
        if pad > 2 {
            return None;
        }
        let mut n = 0u32;
        for (i, &c) in q.iter().enumerate() {
            n <<= 6;
            if i < 4 - pad {
                n |= val(c)?;
            }
        }
        out.push((n >> 16) as u8);
        if pad < 2 {
            out.push((n >> 8) as u8);
        }
        if pad < 1 {
            out.push(n as u8);
        }
    }
    Some(out)
}

/// The CERTIFICATE blocks of a PEM file.
fn pem_certs(text: &[u8]) -> Vec<Vec<u8>> {
    let mut out = Vec::new();
    let begin = b"-----BEGIN CERTIFICATE-----";
    let end = b"-----END CERTIFICATE-----";
    let mut rest = text;
    while let Some(i) = crate::gostr::index(rest, begin) {
        let body = &rest[i + begin.len()..];
        let Some(j) = crate::gostr::index(body, end) else {
            break;
        };
        if let Some(der) = unbase64(&body[..j]) {
            out.push(der);
        }
        rest = &body[j + end.len()..];
    }
    out
}

fn client_config() -> Option<Arc<rustls::ClientConfig>> {
    static C: OnceLock<Option<Arc<rustls::ClientConfig>>> = OnceLock::new();
    C.get_or_init(|| {
        let mut roots = rustls::RootCertStore::empty();
        let files: Vec<String> = match std::env::var("SSL_CERT_FILE") {
            Ok(f) => vec![f],
            Err(_) => CERT_FILES.iter().map(|s| s.to_string()).collect(),
        };
        for f in files {
            if let Ok(raw) = std::fs::read(&f) {
                for der in pem_certs(&raw) {
                    let _ = roots.add(rustls::pki_types::CertificateDer::from(der));
                }
                if !roots.is_empty() {
                    break;
                }
            }
        }
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let cfg = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .ok()?
            .with_root_certificates(roots)
            .with_no_client_auth();
        Some(Arc::new(cfg))
    })
    .clone()
}

/// The parts of a URL a request needs.
#[derive(Debug, PartialEq)]
pub struct Url {
    pub tls: bool,
    /// host[:port] as written, for the Host header.
    pub hostport: String,
    pub host: String,
    pub port: u16,
    /// The path and query.
    pub target: String,
}

fn go_quote(s: &str) -> String {
    crate::goerr::quote(s.as_bytes())
}

/// The response in buf once it is whole: its status and body. `eof` says
/// the server has closed, which ends a body with no length.
fn complete(buf: &[u8], eof: bool) -> Option<(u16, Vec<u8>)> {
    let mut start = 0;
    loop {
        let rest = &buf[start..];
        let end = crate::gostr::index(rest, b"\r\n\r\n")?;
        let head = String::from_utf8_lossy(&rest[..end]).into_owned();
        let mut lines = head.split("\r\n");
        let status_line = lines.next()?;
        let mut parts = status_line.splitn(3, ' ');
        if !parts.next()?.starts_with("HTTP/") {
            return None;
        }
        let status: u16 = parts.next()?.parse().ok()?;
        let body = &rest[end + 4..];
        if (100..200).contains(&status) && status != 101 {
            start += end + 4;
            continue;
        }
        let mut length: Option<usize> = None;
        let mut chunked = false;
        for l in lines {
            let Some((k, v)) = l.split_once(':') else {
                continue;
            };
            let (k, v) = (k.trim().to_ascii_lowercase(), v.trim());
            if k == "content-length" {
                length = v.parse().ok();
            } else if k == "transfer-encoding" && v.to_ascii_lowercase().contains("chunked") {
                chunked = true;
            }
        }
        if chunked {
            let mut out = Vec::new();
            let mut r = body;
            loop {
                let nl = crate::gostr::index(r, b"\r\n")?;
                let size_text = String::from_utf8_lossy(&r[..nl]);
                let size_text = size_text.split(';').next().unwrap_or("").trim();
                let n = usize::from_str_radix(size_text, 16).ok()?;
                r = &r[nl + 2..];
                if n == 0 {
                    return Some((status, out));
                }
                if r.len() < n + 2 {
                    return None;
                }
                out.extend_from_slice(&r[..n]);
                r = &r[n + 2..];
            }
        }
        if status == 204 || status == 304 {
            return Some((status, Vec::new()));
        }
        return match length {
            Some(n) if body.len() >= n => Some((status, body[..n].to_vec())),
            Some(_) if eof => Some((status, body.to_vec())),
            Some(_) => None,
            None if eof => Some((status, body.to_vec())),
            None => None,
        };
    }
}

/// Sends the request and reads until the response is whole. Err(None)
/// is the deadline; Ok(None) a reply that is not HTTP.
fn exchange<S: Read + Write>(
    s: &mut S,
    req: &[u8],
    deadline: Instant,
    set_timeout: &dyn Fn(&mut S, Duration),
) -> Result<Option<(u16, Vec<u8>)>, Option<String>> {
    let left = deadline.saturating_duration_since(Instant::now());
    set_timeout(s, left.max(Duration::from_millis(1)));
    s.write_all(req).map_err(|e| Some(io_text(&e)))?;
    let mut buf = Vec::new();
    let mut chunk = [0u8; 16384];
    loop {
        if let Some(r) = complete(&buf, false) {
            return Ok(Some(r));
        }
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Err(None);
        }
        set_timeout(s, left);
        match s.read(&mut chunk) {
            Ok(0) => return Ok(complete(&buf, true)),
            Ok(n) => buf.extend_from_slice(&chunk[..n]),
            Err(e)
                if matches!(
                    e.kind(),
                    std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
                ) =>
            {
                return Err(None)
            }
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => {
                return Ok(complete(&buf, true))
            }
            Err(e) => return Err(Some(io_text(&e))),
        }
    }
}

fn io_text(e: &std::io::Error) -> String {
    match e.raw_os_error() {
        Some(_) => crate::goerr::io_reason(e),
        None => e.to_string(),
    }
}

fn exchange_tcp(
    s: &mut TcpStream,
    req: &[u8],
    deadline: Instant,
) -> Result<Option<(u16, Vec<u8>)>, Option<String>> {
    exchange(s, req, deadline, &|s: &mut TcpStream, d| {
        let _ = s.set_read_timeout(Some(d));
        let _ = s.set_write_timeout(Some(d));
    })
}

/// Splits a URL, or gives the error text Go's client gives for it.
pub fn split_url(url: &str) -> Result<Url, String> {
    let b = url.as_bytes();
    let mut scheme = String::new();
    let mut rest = url;
    for (i, &c) in b.iter().enumerate() {
        if c.is_ascii_alphabetic() {
            continue;
        }
        if c.is_ascii_digit() || c == b'+' || c == b'-' || c == b'.' {
            if i == 0 {
                break;
            }
            continue;
        }
        if c == b':' {
            if i == 0 {
                return Err(format!("parse {}: missing protocol scheme", go_quote(url)));
            }
            scheme = url[..i].to_ascii_lowercase();
            rest = &url[i + 1..];
        }
        break;
    }
    let shown = if scheme.is_empty() {
        url.to_string()
    } else {
        format!("{scheme}:{rest}")
    };
    let tls = match scheme.as_str() {
        "http" => false,
        "https" => true,
        _ => {
            return Err(format!(
                "Post {}: unsupported protocol scheme {}",
                go_quote(&shown),
                go_quote(&scheme)
            ))
        }
    };
    let Some(after) = rest.strip_prefix("//") else {
        return Err(format!(
            "Post {}: http: no Host in request URL",
            go_quote(&shown)
        ));
    };
    let end = after.find(['/', '?', '#']).unwrap_or(after.len());
    let authority = &after[..end];
    let mut target = after[end..].split('#').next().unwrap_or("").to_string();
    if !target.starts_with('/') {
        target.insert(0, '/');
    }
    let hostport = authority
        .rsplit('@')
        .next()
        .unwrap_or(authority)
        .to_string();
    let (host, port) = if let Some(r) = hostport.strip_prefix('[') {
        let (h, p) = r.split_once(']').unwrap_or((r, ""));
        (h.to_string(), p.to_string())
    } else {
        match hostport.rsplit_once(':') {
            Some((h, p)) => (h.to_string(), format!(":{p}")),
            None => (hostport.clone(), String::new()),
        }
    };
    let port = match port.as_str() {
        "" | ":" => {
            if tls {
                443
            } else {
                80
            }
        }
        p => match p[1..].parse::<u16>() {
            Ok(n) if p[1..].bytes().all(|c| c.is_ascii_digit()) => n,
            _ => {
                return Err(format!(
                    "parse {}: invalid port {} after host",
                    go_quote(url),
                    go_quote(p)
                ))
            }
        },
    };
    if host.is_empty() {
        return Err(format!(
            "Post {}: http: no Host in request URL",
            go_quote(&shown)
        ));
    }
    Ok(Url {
        tls,
        hostport,
        host,
        port,
        target,
    })
}

/// Reads an HTTP/1.1 response: its status and body (by Content-Length,
/// chunked, or to the end).
pub fn parse_response(raw: &[u8]) -> Option<(u16, Vec<u8>)> {
    complete(raw, true)
}

/// POSTs body to url with these headers (written after Host, User-Agent
/// and Content-Length, in the order given), and returns the status and the
/// body, or Go's error text.
pub fn post(
    url: &str,
    headers: &[(&str, &[u8])],
    body: &[u8],
    timeout: Duration,
) -> Result<(u16, Vec<u8>), String> {
    let u = split_url(url)?;
    let shown = if u.tls {
        format!("https{}", &url[url.find(':').unwrap_or(0)..])
    } else {
        format!("http{}", &url[url.find(':').unwrap_or(0)..])
    };
    let fail = |inner: String| format!("Post {}: {inner}", go_quote(&shown));
    let deadline = Instant::now() + timeout;
    let timed_out = || {
        fail("context deadline exceeded (Client.Timeout exceeded while awaiting headers)".into())
    };
    let addrs: Vec<SocketAddr> = match (u.host.as_str(), u.port).to_socket_addrs() {
        Ok(a) => a.collect(),
        Err(_) => return Err(fail(format!("dial tcp: lookup {}: no such host", u.host))),
    };
    let mut tcp = None;
    let mut last = String::from("dial tcp: no address");
    for a in addrs {
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Err(timed_out());
        }
        match TcpStream::connect_timeout(&a, left) {
            Ok(s) => {
                tcp = Some(s);
                break;
            }
            Err(e) if e.kind() == std::io::ErrorKind::TimedOut => return Err(timed_out()),
            Err(e) => last = format!("dial tcp {a}: connect: {}", crate::goerr::io_reason(&e)),
        }
    }
    let Some(tcp) = tcp else {
        return Err(fail(last));
    };
    let mut head = format!(
        "POST {} HTTP/1.1\r\nHost: {}\r\nUser-Agent: Go-http-client/1.1\r\nContent-Length: {}\r\n",
        u.target,
        u.hostport,
        body.len()
    )
    .into_bytes();
    for (k, v) in headers {
        head.extend_from_slice(k.as_bytes());
        head.extend_from_slice(b": ");
        head.extend_from_slice(v);
        head.extend_from_slice(b"\r\n");
    }
    head.extend_from_slice(b"\r\n");
    head.extend_from_slice(body);
    let reply = if u.tls {
        let cfg = client_config().ok_or_else(|| fail("tls: no root certificates".into()))?;
        let name = rustls::pki_types::ServerName::try_from(u.host.clone())
            .map_err(|e| fail(e.to_string()))?;
        let conn = rustls::ClientConnection::new(cfg, name).map_err(|e| fail(e.to_string()))?;
        let mut s = rustls::StreamOwned::new(conn, tcp);
        exchange(
            &mut s,
            &head,
            deadline,
            &|s: &mut rustls::StreamOwned<rustls::ClientConnection, TcpStream>, d| {
                let _ = s.sock.set_read_timeout(Some(d));
                let _ = s.sock.set_write_timeout(Some(d));
            },
        )
    } else {
        let mut s = tcp;
        exchange_tcp(&mut s, &head, deadline)
    };
    match reply {
        Ok(Some(r)) => Ok(r),
        Ok(None) => Err(fail("malformed HTTP response".into())),
        Err(Some(e)) => Err(fail(e)),
        Err(None) => Err(timed_out()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls() {
        let u = split_url("http://127.0.0.1:8080/x/v1/messages").unwrap();
        assert_eq!(
            u,
            Url {
                tls: false,
                hostport: "127.0.0.1:8080".into(),
                host: "127.0.0.1".into(),
                port: 8080,
                target: "/x/v1/messages".into()
            }
        );
        assert_eq!(split_url("HTTPS://h/v1").unwrap().port, 443);
        assert_eq!(
            split_url("ftp://h/v1/messages").unwrap_err(),
            "Post \"ftp://h/v1/messages\": unsupported protocol scheme \"ftp\""
        );
        assert_eq!(
            split_url("nohost/v1/messages").unwrap_err(),
            "Post \"nohost/v1/messages\": unsupported protocol scheme \"\""
        );
        assert_eq!(
            split_url("http:///v1/messages").unwrap_err(),
            "Post \"http:///v1/messages\": http: no Host in request URL"
        );
    }

    #[test]
    fn responses() {
        let r = b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabcdef";
        assert_eq!(parse_response(r), Some((200, b"abc".to_vec())));
        let r = b"HTTP/1.1 500 X\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2;x=1\r\nde\r\n0\r\n\r\n";
        assert_eq!(parse_response(r), Some((500, b"abcde".to_vec())));
        let r = b"HTTP/1.0 404 Not Found\r\nServer: x\r\n\r\nall of it";
        assert_eq!(parse_response(r), Some((404, b"all of it".to_vec())));
    }

    #[test]
    fn refused_reads_as_go() {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        drop(l);
        let url = format!("http://127.0.0.1:{port}/v1/messages");
        let e = post(&url, &[], b"{}", Duration::from_secs(5)).unwrap_err();
        assert_eq!(
            e,
            format!("Post \"{url}\": dial tcp 127.0.0.1:{port}: connect: connection refused")
        );
    }
}
