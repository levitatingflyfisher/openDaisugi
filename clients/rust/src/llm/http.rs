//! One HTTP/1.1 POST with a time limit, over TCP or over rustls with the
//! ring provider and the system's root store (as the gate's llm_check
//! sends it).

use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::Arc;
use std::time::{Duration, Instant};

pub enum Fail {
    /// The time limit ran out.
    Timeout,
    /// Any other failure, worded by the binary.
    Other(String),
}

fn other(why: impl Into<String>) -> Fail {
    Fail::Other(why.into())
}

/// The URL split: TLS or not, host, port, path.
fn target(url: &str) -> Result<(bool, String, u16, String), Fail> {
    let bad = || other(format!("the URL {url} is not one this binary sends to"));
    let (scheme, rest) = url.split_once("://").ok_or_else(bad)?;
    let tls = match scheme {
        "http" => false,
        "https" => true,
        _ => return Err(bad()),
    };
    let (hostport, path) = match rest.find('/') {
        Some(i) => (&rest[..i], rest[i..].to_string()),
        None => (rest, "/".to_string()),
    };
    if hostport.is_empty() || hostport.contains('@') || hostport.contains('[') {
        return Err(bad());
    }
    let (host, port) = match hostport.rsplit_once(':') {
        Some((h, p)) => (h.to_string(), p.parse::<u16>().map_err(|_| bad())?),
        None => (hostport.to_string(), if tls { 443 } else { 80 }),
    };
    if host.is_empty() {
        return Err(bad());
    }
    Ok((tls, host, port, path))
}

fn io_fail(e: std::io::Error) -> Fail {
    match e.kind() {
        std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock => Fail::Timeout,
        _ => other(e.to_string()),
    }
}

/// POSTs `body` to `url` and returns the status and the reply's text,
/// decoded as UTF-8 with replacement. `timeout_s` bounds the connect and
/// every read and write, and the whole exchange.
pub fn post(url: &str, headers: &[(&str, String)], body: &[u8], timeout_s: f64) -> Result<(u16, String), Fail> {
    post_with(url, headers, body, timeout_s, None)
}

/// `post`, with `roots` in place of the system's root store (for the
/// tests only).
pub fn post_with(
    url: &str,
    headers: &[(&str, String)],
    body: &[u8],
    timeout_s: f64,
    roots: Option<Arc<rustls::RootCertStore>>,
) -> Result<(u16, String), Fail> {
    let (tls, host, port, path) = target(url)?;
    let limit = if timeout_s.is_finite() && timeout_s > 0.0 { Duration::from_secs_f64(timeout_s.min(1e9)) } else { Duration::from_secs(6000) };
    let started = Instant::now();
    let left = || limit.checked_sub(started.elapsed()).filter(|d| !d.is_zero());
    let addrs: Vec<_> = (host.as_str(), port).to_socket_addrs().map_err(|e| other(e.to_string()))?.collect();
    let mut stream = None;
    let mut last = String::from("no address to connect to");
    for a in &addrs {
        let d = left().ok_or(Fail::Timeout)?;
        match TcpStream::connect_timeout(a, d) {
            Ok(s) => {
                stream = Some(s);
                break;
            }
            Err(e) if e.kind() == std::io::ErrorKind::TimedOut => return Err(Fail::Timeout),
            Err(e) => last = e.to_string(),
        }
    }
    let s = stream.ok_or_else(|| other(last))?;
    let mut conn: Box<dyn ReadWrite> = if tls {
        let roots = match roots {
            Some(r) => r,
            None => crate::gate::llm::system_roots().map_err(|_| other("no root certificates on this system"))?,
        };
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let config = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .map_err(|e| other(e.to_string()))?
            .with_root_certificates(roots)
            .with_no_client_auth();
        let name = rustls::pki_types::ServerName::try_from(host.clone()).map_err(|e| other(e.to_string()))?;
        let c = rustls::ClientConnection::new(Arc::new(config), name).map_err(|e| other(e.to_string()))?;
        Box::new(rustls::StreamOwned::new(c, TimedStream { s, started, limit }))
    } else {
        Box::new(TimedStream { s, started, limit })
    };
    let default_port = if tls { 443 } else { 80 };
    let host_header = if port == default_port { host.clone() } else { format!("{host}:{port}") };
    let mut req = format!("POST {path} HTTP/1.1\r\nhost: {host_header}\r\n");
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    req.push_str(&format!("content-length: {}\r\nconnection: close\r\n\r\n", body.len()));
    let mut out = req.into_bytes();
    out.extend_from_slice(body);
    conn.write_all(&out).and_then(|_| conn.flush()).map_err(io_fail)?;
    let mut raw = Vec::new();
    match conn.read_to_end(&mut raw) {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof && tls => {}
        Err(e) => return Err(io_fail(e)),
    }
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
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            match k.trim().to_ascii_lowercase().as_str() {
                "transfer-encoding" => chunked = v.to_ascii_lowercase().contains("chunked"),
                "content-length" => length = v.trim().parse().ok(),
                _ => {}
            }
        }
    }
    if chunked {
        rest = crate::gate::llm::dechunk(&rest).ok_or_else(|| other("the reply's chunks are broken"))?;
    } else if let Some(n) = length {
        if rest.len() < n {
            return Err(other("the reply was cut short"));
        }
        rest.truncate(n);
    }
    Ok((status, crate::gate::py::text::decode_utf8_replace(&rest)))
}

trait ReadWrite: Read + Write {}
impl<T: Read + Write> ReadWrite for T {}

/// A socket whose every read and write waits no longer than what is left
/// of the time limit.
struct TimedStream {
    s: TcpStream,
    started: Instant,
    limit: Duration,
}

impl TimedStream {
    fn arm(&self) -> std::io::Result<()> {
        match self.limit.checked_sub(self.started.elapsed()).filter(|d| !d.is_zero()) {
            Some(d) => {
                self.s.set_read_timeout(Some(d))?;
                self.s.set_write_timeout(Some(d))
            }
            None => Err(std::io::Error::from(std::io::ErrorKind::TimedOut)),
        }
    }
}

impl Read for TimedStream {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        self.arm()?;
        self.s.read(buf)
    }
}

impl Write for TimedStream {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.arm()?;
        self.s.write(buf)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.s.flush()
    }
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
        let r = post_with(&format!("https://localhost:{port}/v1/messages"), &headers, b"{\"a\":1}", 10.0, Some(roots.clone()));
        let req = h.join().unwrap();
        match r {
            Ok((200, t)) => assert_eq!(t, "{}"),
            Ok((s, t)) => panic!("{s} {t}"),
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
        let r = post_with(&format!("http://127.0.0.1:{port}/v1/messages"), &headers, b"{}", 0.3, None);
        assert!(matches!(r, Err(Fail::Timeout)), "not a timeout");
        assert!(t.elapsed() < Duration::from_millis(1200));
        h.join().unwrap();

        // A refused connection is worded without the key.
        let port = {
            let l = TcpListener::bind("127.0.0.1:0").unwrap();
            l.local_addr().unwrap().port()
        };
        match post_with(&format!("http://127.0.0.1:{port}/v1/messages"), &headers, b"{}", 5.0, None) {
            Err(Fail::Other(why)) => assert!(!why.contains("sk-secret"), "{why}"),
            _ => panic!("a refused port answered"),
        }
    }
}
