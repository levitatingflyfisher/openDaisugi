//! `_model_fetch.fetch` for the pinned potion files: a file that matches
//! its hash is used as it is; any other is removed and downloaded again
//! into a temporary file beside it, checked, then renamed into place.
//!
//! The download is one HTTP/1.1 GET at a time over rustls (the ring
//! provider, the system's root store), following redirects, since the
//! Hugging Face `resolve/` URL answers with one to its file store.

use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::Arc;
use std::time::Duration;

use super::{Env, Pinned};
use crate::gate::sha256;

/// The most redirects one download follows.
const MAX_HOPS: usize = 8;

pub fn fetch(url: &str, f: &Pinned, dest: &str, e: &Env) -> Result<(), String> {
    if let Ok(b) = std::fs::read(dest) {
        if sha256::hexdigest(&b) == f.sha256 {
            return Ok(());
        }
    }
    if e.no_fetch {
        return Err(format!("{dest} is missing or does not match its pinned sha256"));
    }
    let dir = std::path::Path::new(dest).parent().map(|p| p.to_path_buf()).unwrap_or_default();
    std::fs::create_dir_all(&dir).map_err(|err| err.to_string())?;
    let _ = std::fs::remove_file(dest);
    let name = std::path::Path::new(dest).file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
    let note = if f.size >= 1 << 20 { format!(" of about {}MB", f.size >> 20) } else { String::new() };
    (e.notice)(&format!("fetching {name}{note} from {url} ..."));
    let body = get(url, f.size)?;
    let got = sha256::hexdigest(&body);
    if got != f.sha256 {
        return Err(format!("{name}: sha256 {got}, not the pinned {}", f.sha256));
    }
    // A temporary file of its own, so two processes fetching at once
    // never write into one file; each renames a checked file into place.
    let part = dir.join(format!("{name}.part-{}", crate::gate::random_hex(8).unwrap_or_else(|_| "0".into())));
    let write = || -> std::io::Result<()> {
        let mut out = std::fs::File::create(&part)?;
        out.write_all(&body)?;
        out.sync_all()?;
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&part, std::fs::Permissions::from_mode(0o644))?;
        std::fs::rename(&part, dest)
    };
    let r = write();
    let _ = std::fs::remove_file(&part);
    r.map_err(|err| err.to_string())
}

struct Url {
    tls: bool,
    host: String,
    port: u16,
    path: String,
}

fn parse_url(url: &str) -> Result<Url, String> {
    let bad = || format!("a URL this binary does not fetch from: {url}");
    let (scheme, rest) = url.split_once("://").ok_or_else(bad)?;
    let tls = match scheme {
        "https" => true,
        "http" => false,
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
    Ok(Url { tls, host, port, path })
}

enum Conn {
    Plain(TcpStream),
    Tls(Box<rustls::StreamOwned<rustls::ClientConnection, TcpStream>>),
}

/// The body of `url`, following redirects. A redirect never goes from
/// https to http. The body is at most `size` bytes.
pub fn get(url: &str, size: u64) -> Result<Vec<u8>, String> {
    let mut url = url.to_string();
    let first_tls = parse_url(&url)?.tls;
    for _ in 0..=MAX_HOPS {
        let u = parse_url(&url)?;
        if first_tls && !u.tls {
            return Err(format!("a redirect from https to http: {url}"));
        }
        let (status, headers, body) = get_once(&u, size)?;
        match status {
            200 => return Ok(body),
            301 | 302 | 303 | 307 | 308 => {
                let loc = headers
                    .iter()
                    .find(|(k, _)| k == "location")
                    .map(|(_, v)| v.clone())
                    .ok_or_else(|| format!("GET {url}: {status} with no Location"))?;
                url = if loc.contains("://") {
                    loc
                } else if loc.starts_with('/') {
                    let scheme = if u.tls { "https" } else { "http" };
                    let default = if u.tls { 443 } else { 80 };
                    let hp = if u.port == default { u.host.clone() } else { format!("{}:{}", u.host, u.port) };
                    format!("{scheme}://{hp}{loc}")
                } else {
                    return Err(format!("GET {url}: a redirect this binary does not follow"));
                };
            }
            _ => return Err(format!("GET {url}: {status}")),
        }
    }
    Err(format!("GET {url}: more than {MAX_HOPS} redirects"))
}

type Reply = (u16, Vec<(String, String)>, Vec<u8>);

fn get_once(u: &Url, size: u64) -> Result<Reply, String> {
    let addrs: Vec<_> = (u.host.as_str(), u.port).to_socket_addrs().map_err(|e| e.to_string())?.collect();
    let mut stream = None;
    let mut last = String::from("no address");
    for a in &addrs {
        match TcpStream::connect_timeout(a, Duration::from_secs(30)) {
            Ok(s) => {
                stream = Some(s);
                break;
            }
            Err(e) => last = e.to_string(),
        }
    }
    let s = stream.ok_or(last)?;
    let _ = s.set_read_timeout(Some(Duration::from_secs(300)));
    let _ = s.set_write_timeout(Some(Duration::from_secs(60)));
    let mut conn = if u.tls {
        let roots = crate::gate::llm::system_roots().map_err(|_| "no root certificates on this system".to_string())?;
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let config = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .map_err(|e| e.to_string())?
            .with_root_certificates(roots)
            .with_no_client_auth();
        let name = rustls::pki_types::ServerName::try_from(u.host.clone()).map_err(|e| e.to_string())?;
        let c = rustls::ClientConnection::new(Arc::new(config), name).map_err(|e| e.to_string())?;
        Conn::Tls(Box::new(rustls::StreamOwned::new(c, s)))
    } else {
        Conn::Plain(s)
    };
    let default = if u.tls { 443 } else { 80 };
    let host = if u.port == default { u.host.clone() } else { format!("{}:{}", u.host, u.port) };
    let req = format!(
        "GET {} HTTP/1.1\r\nhost: {host}\r\nuser-agent: opendaisugi\r\naccept-encoding: identity\r\nconnection: close\r\n\r\n",
        u.path
    );
    // Headers and a body of at most `size` bytes; more is refused.
    let cap = size as usize + 65_536;
    let mut raw = Vec::new();
    let r = match &mut conn {
        Conn::Plain(s) => s.write_all(req.as_bytes()).and_then(|_| read_capped(s, &mut raw, cap)),
        Conn::Tls(s) => s.write_all(req.as_bytes()).and_then(|_| s.flush()).and_then(|_| match read_capped(s, &mut raw, cap) {
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => Ok(()),
            other => other,
        }),
    };
    r.map_err(|e| e.to_string())?;
    let split = raw.windows(4).position(|w| w == b"\r\n\r\n").ok_or("a reply that could not be read")?;
    let head = String::from_utf8_lossy(&raw[..split]).into_owned();
    let mut body = raw[split + 4..].to_vec();
    let mut lines = head.split("\r\n");
    let status: u16 = lines
        .next()
        .and_then(|l| l.split_whitespace().nth(1))
        .and_then(|c| c.parse().ok())
        .ok_or("a reply that could not be read")?;
    let mut headers = vec![];
    let mut chunked = false;
    let mut length: Option<usize> = None;
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            let (k, v) = (k.trim().to_ascii_lowercase(), v.trim().to_string());
            match k.as_str() {
                "transfer-encoding" => chunked = v.to_ascii_lowercase().contains("chunked"),
                "content-length" => length = v.parse().ok(),
                "content-encoding" if !v.eq_ignore_ascii_case("identity") => {
                    return Err(format!("a reply in the {v} encoding"))
                }
                _ => {}
            }
            headers.push((k, v));
        }
    }
    if status == 200 {
        if chunked {
            body = crate::gate::llm::dechunk(&body).ok_or("a chunked reply that could not be read")?;
        } else if let Some(n) = length {
            if body.len() < n {
                return Err("a reply cut short".into());
            }
            body.truncate(n);
        }
        if body.len() as u64 > size {
            return Err(format!("a file larger than the pinned {size} bytes"));
        }
    }
    Ok((status, headers, body))
}

fn read_capped(s: &mut impl Read, out: &mut Vec<u8>, cap: usize) -> std::io::Result<()> {
    let mut buf = [0u8; 65_536];
    loop {
        let n = s.read(&mut buf)?;
        if n == 0 {
            return Ok(());
        }
        out.extend_from_slice(&buf[..n]);
        if out.len() > cap {
            return Err(std::io::Error::other("a reply larger than the pinned size"));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::TcpListener;

    /// A local server that answers each connection with the next reply.
    fn serve(replies: Vec<Vec<u8>>) -> u16 {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            for reply in replies {
                let (mut s, _) = l.accept().unwrap();
                let mut buf = [0u8; 4096];
                let mut req = vec![];
                while !req.windows(4).any(|w| w == b"\r\n\r\n") {
                    let n = s.read(&mut buf).unwrap();
                    if n == 0 {
                        break;
                    }
                    req.extend_from_slice(&buf[..n]);
                }
                s.write_all(&reply).unwrap();
            }
        });
        port
    }

    fn env(dir: &str, base: String, lines: std::rc::Rc<std::cell::RefCell<Vec<String>>>) -> Env {
        let mut vars = std::collections::HashMap::new();
        vars.insert("XDG_CACHE_HOME".to_string(), dir.to_string());
        Env { vars, home: "/nonexistent".into(), notice: Box::new(move |s| lines.borrow_mut().push(s.to_string())), no_fetch: false, base }
    }

    #[test]
    fn a_redirected_chunked_download_is_checked_and_kept() {
        let body = b"{\"x\": 1}".to_vec();
        let sum = sha256::hexdigest(&body);
        let pinned = Pinned { name: "config.json", sha256: Box::leak(sum.into_boxed_str()), size: body.len() as u64 };
        let chunked = format!("HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n{:x}\r\n{}\r\n0\r\n\r\n", body.len(), String::from_utf8_lossy(&body));
        let port = serve(vec![b"HTTP/1.1 302 Found\r\nlocation: /real/config.json\r\ncontent-length: 0\r\n\r\n".to_vec(), chunked.into_bytes()]);
        let dir = std::env::temp_dir().join(format!("potion-fetch-{}", std::process::id()));
        let d = dir.to_string_lossy().into_owned();
        let lines = std::rc::Rc::new(std::cell::RefCell::new(vec![]));
        let e = env(&d, format!("http://127.0.0.1:{port}/"), lines.clone());
        let dest = format!("{d}/config.json");
        fetch(&format!("http://127.0.0.1:{port}/config.json"), &pinned, &dest, &e).unwrap();
        assert_eq!(std::fs::read(&dest).unwrap(), body);
        assert_eq!(lines.borrow().len(), 1);
        assert!(lines.borrow()[0].starts_with("fetching config.json from http://127.0.0.1:"));
        // A second fetch finds the checked file and says nothing.
        fetch("http://127.0.0.1:1/never", &pinned, &dest, &e).unwrap();
        assert_eq!(lines.borrow().len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_wrong_hash_or_an_oversized_body_is_refused() {
        let pinned = Pinned { name: "config.json", sha256: "00", size: 4 };
        let port = serve(vec![b"HTTP/1.1 200 OK\r\ncontent-length: 4\r\n\r\nabcd".to_vec(), b"HTTP/1.1 200 OK\r\ncontent-length: 9\r\n\r\nabcdefghi".to_vec()]);
        let dir = std::env::temp_dir().join(format!("potion-fetch-bad-{}", std::process::id()));
        let d = dir.to_string_lossy().into_owned();
        let lines = std::rc::Rc::new(std::cell::RefCell::new(vec![]));
        let e = env(&d, String::new(), lines);
        let dest = format!("{d}/config.json");
        let err = fetch(&format!("http://127.0.0.1:{port}/a"), &pinned, &dest, &e).unwrap_err();
        assert!(err.contains("not the pinned"), "{err}");
        let err = fetch(&format!("http://127.0.0.1:{port}/b"), &pinned, &dest, &e).unwrap_err();
        assert!(err.contains("larger than"), "{err}");
        assert!(!std::path::Path::new(&dest).exists());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn no_fetch_refuses_a_missing_file() {
        let pinned = Pinned { name: "config.json", sha256: "00", size: 4 };
        let lines = std::rc::Rc::new(std::cell::RefCell::new(vec![]));
        let mut e = env("/nonexistent-dir", String::new(), lines);
        e.no_fetch = true;
        let err = fetch("http://127.0.0.1:1/x", &pinned, "/nonexistent-dir/config.json", &e).unwrap_err();
        assert!(err.contains("missing or does not match"), "{err}");
    }
}
