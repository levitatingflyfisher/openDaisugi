//! A one-shot HTTP/1.1 client for the voice server and ntfy: one request,
//! the connection closed after the reply. https checks the server against
//! the system's CA bundle, as Go's client does on Linux.

use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Arc, OnceLock};
use std::time::Duration;

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

/// Sends one request and reads the whole reply. headers are written in
/// the order given, after Host, User-Agent and Content-Length.
pub fn send(
    method: &str,
    url: &str,
    headers: &[(&str, &str)],
    body: &[u8],
    timeout: Duration,
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
    let mut tcp = None;
    let mut last = String::from("no address");
    for a in addrs {
        match TcpStream::connect_timeout(&a, timeout) {
            Ok(s) => {
                tcp = Some(s);
                break;
            }
            Err(e) => last = e.to_string(),
        }
    }
    let tcp = tcp.ok_or(Fail::Reach(last))?;
    let _ = tcp.set_read_timeout(Some(timeout));
    let _ = tcp.set_write_timeout(Some(timeout));
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
        s.write_all(&out).map_err(|e| Fail::Reach(e.to_string()))?;
        match s.read_to_end(&mut raw) {
            Ok(_) => {}
            // A server that closes without close_notify still sent its
            // whole reply when the reply says how long it is.
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof && !raw.is_empty() => {}
            Err(e) => return Err(Fail::Reach(e.to_string())),
        }
    } else {
        let mut s = tcp;
        s.write_all(&out).map_err(|e| Fail::Reach(e.to_string()))?;
        s.read_to_end(&mut raw)
            .map_err(|e| Fail::Reach(e.to_string()))?;
    }
    let (status, content_type, body) = super::voice::parse_response(&raw)
        .ok_or_else(|| Fail::Reach("malformed HTTP response".into()))?;
    Ok(Reply {
        status,
        content_type,
        body,
    })
}
