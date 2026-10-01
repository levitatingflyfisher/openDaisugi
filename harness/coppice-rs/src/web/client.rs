//! A one-shot HTTP/1.1 client for the voice server and ntfy: one request,
//! the connection closed after the reply. https checks the server against
//! the system's CA bundle, as Go's client does on Linux.

use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

/// A reply: its status, Content-Type and body.
pub struct Reply {
    pub status: u16,
    pub content_type: String,
    pub body: Vec<u8>,
}

/// Why a request failed.
#[derive(Debug)]
pub enum Fail {
    /// The URL is not one this client can send to.
    Build(String),
    /// The server could not be reached, or its reply could not be read.
    Reach(String),
}

impl std::fmt::Display for Fail {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Fail::Build(s) | Fail::Reach(s) => f.write_str(s),
        }
    }
}

/// The files Go's crypto/x509 reads for the system roots on Linux.
const CERT_FILES: &[&str] = &[
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/pki/tls/certs/ca-bundle.crt",
    "/etc/ssl/ca-bundle.pem",
    "/etc/pki/tls/cacert.pem",
    "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
    "/etc/ssl/cert.pem",
];

fn client_config() -> Option<Arc<rustls::ClientConfig>> {
    static C: OnceLock<Option<Arc<rustls::ClientConfig>>> = OnceLock::new();
    C.get_or_init(|| {
        let mut roots = rustls::RootCertStore::empty();
        let mut files: Vec<String> = Vec::new();
        if let Ok(f) = std::env::var("SSL_CERT_FILE") {
            files.push(f);
        } else {
            files.extend(CERT_FILES.iter().map(|s| s.to_string()));
        }
        for f in files {
            if let Ok(raw) = std::fs::read(&f) {
                for (kind, der) in super::der::pem_blocks(&raw) {
                    if kind == "CERTIFICATE" {
                        let _ = roots.add(rustls::pki_types::CertificateDer::from(der));
                    }
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

/// A failed read as the caller sees it: a read that ran out of time says
/// "timed out", whatever the operating system called it.
fn reach(e: &std::io::Error) -> Fail {
    Fail::Reach(reach_text(e))
}

/// The words for a failed connect or read: "timed out" for a timeout of
/// either kind, so a caller tells them from other failures as one.
fn reach_text(e: &std::io::Error) -> String {
    match e.kind() {
        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut => "timed out".into(),
        _ => e.to_string(),
    }
}

/// Reads r to its end or to cap bytes, giving up at deadline. ctl is the
/// socket under r: its read timeout is set to the time left before each
/// read.
fn read_within<R: Read>(
    r: &mut R,
    ctl: &TcpStream,
    deadline: Instant,
    cap: u64,
    raw: &mut Vec<u8>,
) -> std::io::Result<()> {
    let mut chunk = [0u8; 16384];
    while (raw.len() as u64) < cap {
        let Some(left) = deadline
            .checked_duration_since(Instant::now())
            .filter(|d| !d.is_zero())
        else {
            return Err(std::io::ErrorKind::TimedOut.into());
        };
        let _ = ctl.set_read_timeout(Some(left));
        let want = ((cap - raw.len() as u64).min(chunk.len() as u64)) as usize;
        match r.read(&mut chunk[..want]) {
            Ok(0) => return Ok(()),
            Ok(n) => raw.extend_from_slice(&chunk[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

/// Sends one request and reads the whole reply. headers are written in
/// the order given, after Host, User-Agent and Content-Length.
pub fn send(
    method: &str,
    url: &str,
    headers: &[(&str, &str)],
    body: &[u8],
    timeout: Duration,
) -> Result<Reply, Fail> {
    send_capped(method, url, headers, body, timeout, u64::MAX)
}

/// As send, but reads at most cap bytes of the reply, head included. A
/// reply that fills the cap is cut there, so the caller can tell it was
/// over the limit by its length. The whole exchange, the connection and
/// every read included, has `timeout` and no more, as Go's Client.Timeout
/// has: a server that sends a byte now and then does not stretch it.
pub fn send_capped(
    method: &str,
    url: &str,
    headers: &[(&str, &str)],
    body: &[u8],
    timeout: Duration,
    cap: u64,
) -> Result<Reply, Fail> {
    let (tls, rest) = if let Some(r) = url.strip_prefix("http://") {
        (false, r)
    } else if let Some(r) = url.strip_prefix("https://") {
        (true, r)
    } else {
        return Err(Fail::Build(format!("unsupported protocol scheme in {url}")));
    };
    let (hostport, target) = match rest.find(['/', '?']) {
        Some(i) if rest.as_bytes()[i] == b'/' => (&rest[..i], rest[i..].to_string()),
        Some(i) => (&rest[..i], format!("/{}", &rest[i..])),
        None => (rest, "/".to_string()),
    };
    let target = target.split('#').next().unwrap_or("/").to_string();
    let hostport = hostport.rsplit('@').next().unwrap_or(hostport);
    let (host, port) = if let Some(r) = hostport.strip_prefix('[') {
        let (h, p) = r.split_once(']').unwrap_or((r, ""));
        (h.to_string(), p.strip_prefix(':').unwrap_or("").to_string())
    } else {
        match hostport.rsplit_once(':') {
            Some((h, p)) => (h.to_string(), p.to_string()),
            None => (hostport.to_string(), String::new()),
        }
    };
    if host.is_empty() {
        return Err(Fail::Build(format!("no Host in request URL {url}")));
    }
    let port: u16 = if port.is_empty() {
        if tls {
            443
        } else {
            80
        }
    } else {
        port.parse()
            .map_err(|_| Fail::Build(format!("invalid port in {url}")))?
    };
    let addrs: Vec<std::net::SocketAddr> = (host.as_str(), port)
        .to_socket_addrs()
        .map_err(|e| Fail::Reach(e.to_string()))?
        .collect();
    let deadline = Instant::now() + timeout;
    let left = || {
        deadline
            .checked_duration_since(Instant::now())
            .filter(|d| !d.is_zero())
            .ok_or_else(|| Fail::Reach("timed out".into()))
    };
    let mut tcp = None;
    let mut last = String::from("no address");
    for a in addrs {
        match TcpStream::connect_timeout(&a, left()?) {
            Ok(s) => {
                tcp = Some(s);
                break;
            }
            Err(e) => last = reach_text(&e),
        }
    }
    let tcp = tcp.ok_or(Fail::Reach(last))?;
    // A handle on the same socket, to set its timeouts to the time left
    // before each handshake step, write and read, whether or not TLS wraps
    // it.
    let ctl = tcp.try_clone().map_err(|e| Fail::Reach(e.to_string()))?;
    let _ = ctl.set_read_timeout(Some(left()?));
    let _ = ctl.set_write_timeout(Some(left()?));
    let mut head = format!(
        "{method} {target} HTTP/1.1\r\nHost: {hostport}\r\nUser-Agent: Go-http-client/1.1\r\nContent-Length: {}\r\n",
        body.len()
    );
    for (k, v) in headers {
        head.push_str(&format!("{k}: {v}\r\n"));
    }
    head.push_str("Connection: close\r\n\r\n");
    let mut out = head.into_bytes();
    out.extend_from_slice(body);
    let mut raw = Vec::new();
    if tls {
        let cfg = client_config().ok_or(Fail::Reach("no TLS config".into()))?;
        let name = rustls::pki_types::ServerName::try_from(host.clone())
            .map_err(|e| Fail::Build(e.to_string()))?;
        let conn =
            rustls::ClientConnection::new(cfg, name).map_err(|e| Fail::Reach(e.to_string()))?;
        let mut s = rustls::StreamOwned::new(conn, tcp);
        // The handshake runs here, step by step, each with the time left: a
        // server that accepts and never speaks TLS ends at the deadline.
        while s.conn.is_handshaking() {
            let _ = ctl.set_read_timeout(Some(left()?));
            let _ = ctl.set_write_timeout(Some(left()?));
            s.conn.complete_io(&mut s.sock).map_err(|e| reach(&e))?;
        }
        let _ = ctl.set_write_timeout(Some(left()?));
        s.write_all(&out).map_err(|e| reach(&e))?;
        match read_within(&mut s, &ctl, deadline, cap, &mut raw) {
            Ok(()) => {}
            // A server that closes without close_notify still sent its
            // whole reply when the reply says how long it is.
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof && !raw.is_empty() => {}
            Err(e) => return Err(reach(&e)),
        }
    } else {
        let mut s = tcp;
        let _ = ctl.set_write_timeout(Some(left()?));
        s.write_all(&out).map_err(|e| reach(&e))?;
        read_within(&mut s, &ctl, deadline, cap, &mut raw).map_err(|e| reach(&e))?;
    }
    if raw.len() as u64 >= cap {
        // Cut at the cap: the body is whatever came before it.
        let at = raw
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map_or(0, |i| i + 4);
        let status = std::str::from_utf8(&raw[..at.min(16)])
            .ok()
            .and_then(|h| h.split(' ').nth(1))
            .and_then(|c| c.parse().ok())
            .unwrap_or(0);
        return Ok(Reply {
            status,
            content_type: String::new(),
            body: raw[at..].to_vec(),
        });
    }
    let (status, content_type, body) = super::voice::parse_response(&raw)
        .ok_or_else(|| Fail::Reach("malformed HTTP response".into()))?;
    Ok(Reply {
        status,
        content_type,
        body,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::TcpListener;

    /// A server that answers each connection with what serve does.
    fn server(serve: impl Fn(TcpStream) + Send + Sync + 'static) -> String {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = l.local_addr().unwrap();
        let serve = Arc::new(serve);
        std::thread::spawn(move || {
            for c in l.incoming().flatten() {
                let serve = serve.clone();
                std::thread::spawn(move || serve(c));
            }
        });
        format!("http://{addr}/x")
    }

    fn drain(c: &mut TcpStream) {
        let mut b = [0u8; 4096];
        let _ = c.read(&mut b);
    }

    /// A listener that accepts and never says a word, as a server that is
    /// not speaking TLS (or anything) would.
    fn silent() -> u16 {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            let mut held = vec![];
            for c in l.incoming().flatten() {
                held.push(c);
                if held.len() > 8 {
                    break;
                }
            }
            std::thread::sleep(Duration::from_secs(30));
        });
        port
    }

    #[test]
    fn a_tls_handshake_with_a_silent_server_ends_at_the_deadline() {
        let port = silent();
        let start = Instant::now();
        let r = send(
            "POST",
            &format!("https://127.0.0.1:{port}/x"),
            &[("Authorization", "Bearer sk-test")],
            b"{}",
            Duration::from_millis(600),
        );
        let took = start.elapsed();
        match r {
            Err(Fail::Reach(m)) => assert_eq!(m, "timed out"),
            _ => panic!("a silent https server did not time out"),
        }
        assert!(took < Duration::from_millis(1500), "took {took:?}");
    }

    #[test]
    fn a_silent_plain_server_ends_at_the_deadline_too() {
        let port = silent();
        let start = Instant::now();
        let r = send(
            "POST",
            &format!("http://127.0.0.1:{port}/x"),
            &[],
            b"{}",
            Duration::from_millis(600),
        );
        assert!(matches!(r, Err(Fail::Reach(ref m)) if m == "timed out"));
        assert!(start.elapsed() < Duration::from_millis(1500));
    }

    #[test]
    fn a_connect_that_times_out_says_timed_out_like_a_read() {
        let e = std::io::Error::new(std::io::ErrorKind::TimedOut, "connection timed out");
        assert_eq!(reach_text(&e), "timed out");
        let e = std::io::Error::from(std::io::ErrorKind::WouldBlock);
        assert_eq!(reach_text(&e), "timed out");
        let e = std::io::Error::from(std::io::ErrorKind::ConnectionRefused);
        assert_ne!(reach_text(&e), "timed out");
    }

    #[test]
    fn a_redirect_is_a_reply_and_is_not_followed() {
        let url = server(|mut c| {
            drain(&mut c);
            let _ = c.write_all(
                b"HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:1/elsewhere\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
            );
        });
        let r = send("POST", &url, &[], b"{}", Duration::from_secs(5)).unwrap();
        assert_eq!(r.status, 302);
    }

    #[test]
    fn the_whole_exchange_has_the_timeout_even_when_bytes_keep_coming() {
        let url = server(|mut c| {
            drain(&mut c);
            let _ = c.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n");
            for _ in 0..50 {
                std::thread::sleep(Duration::from_millis(100));
                if c.write_all(b"x").is_err() {
                    return;
                }
            }
        });
        let start = Instant::now();
        let r = send("POST", &url, &[], b"{}", Duration::from_millis(600));
        let took = start.elapsed();
        match r {
            Err(Fail::Reach(m)) => assert_eq!(m, "timed out"),
            _ => panic!("a trickling reply was not cut at the deadline"),
        }
        assert!(took < Duration::from_millis(1500), "took {took:?}");
    }
}
