//! `_model_fetch.fetch` for the pinned potion files: a file that matches
//! its hash is used as it is; any other is removed and downloaded again
//! into a temporary file beside it, checked, then renamed into place.
//!
//! The download is one HTTP/1.1 GET at a time over rustls (the ring
//! provider, the system's root store), following redirects, since the
//! Hugging Face `resolve/` URL answers with one to its file store. It goes
//! through a proxy as `urllib.request.urlopen` picks one.

use std::io::{Read, Write};
use std::time::Duration;

use crate::netproxy;

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
    let proxies = netproxy::urllib_for(&e.vars);
    let body = get(url, f.size, &proxies)?;
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
    explicit_port: bool,
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
    let (host, port, explicit_port) = match hostport.rsplit_once(':') {
        Some((h, p)) => (h.to_string(), p.parse::<u16>().map_err(|_| bad())?, true),
        None => (hostport.to_string(), if tls { 443 } else { 80 }, false),
    };
    if host.is_empty() {
        return Err(bad());
    }
    Ok(Url { tls, host, port, explicit_port, path })
}

/// The body of `url`, following redirects. A redirect never goes from
/// https to http. The body is at most `size` bytes.
pub fn get(url: &str, size: u64, proxies: &netproxy::Urllib) -> Result<Vec<u8>, String> {
    match get_reply(url, size, proxies)? {
        (_, 200, body) => Ok(body),
        (last, status, _) => Err(format!("GET {last}: {status}")),
    }
}

/// The URL last asked, the status and the body of `url`, following
/// redirects as `get` does. Any status other than a redirect is an
/// answer; Err is a request that got none.
pub fn get_reply(url: &str, size: u64, proxies: &netproxy::Urllib) -> Result<(String, u16, Vec<u8>), String> {
    let mut url = url.to_string();
    let first_tls = parse_url(&url)?.tls;
    for _ in 0..=MAX_HOPS {
        let u = parse_url(&url)?;
        if first_tls && !u.tls {
            return Err(format!("a redirect from https to http: {url}"));
        }
        let (status, headers, body) = get_once(&u, size, proxies)?;
        match status {
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
            _ => return Ok((url, status, body)),
        }
    }
    Err(format!("GET {url}: more than {MAX_HOPS} redirects"))
}

type Reply = (u16, Vec<(String, String)>, Vec<u8>);

/// Opens a connection for `u` (through a proxy as urllib picks one) and
/// sends the GET.
fn send(u: &Url, proxies: &netproxy::Urllib) -> Result<netproxy::Conn, String> {
    let origin = netproxy::Origin { tls: u.tls, host: u.host.clone(), port: u.port, explicit_port: u.explicit_port };
    let route = proxies.route(&origin).map_err(|r| match r {
        netproxy::Refusal::Fails(t) | netproxy::Refusal::Invalid(t) => t,
        netproxy::Refusal::Unported(w) => format!("a proxy setting this binary does not read as urllib does: {w}"),
    })?;
    let dial = netproxy::Dial { limit: None, connect_timeout: Some(Duration::from_secs(30)), roots: None };
    let mut conn = netproxy::open(&route, &origin, &dial).map_err(|e| match e {
        netproxy::Fail::Status(code, reason) => format!("Tunnel connection failed: {code} {reason}"),
        e => e.to_string(),
    })?;
    let _ = conn.sock.set_read_timeout(Some(Duration::from_secs(300)));
    let _ = conn.sock.set_write_timeout(Some(Duration::from_secs(60)));
    let default = if u.tls { 443 } else { 80 };
    let host = if u.port == default { u.host.clone() } else { format!("{}:{}", u.host, u.port) };
    let line = netproxy::request_target(&conn, &origin, &u.path);
    let req = format!(
        "GET {line} HTTP/1.1\r\n{}host: {host}\r\nuser-agent: opendaisugi\r\naccept-encoding: identity\r\nconnection: close\r\n\r\n",
        netproxy::auth_line(&conn)
    );
    conn.io.write_all(req.as_bytes()).and_then(|_| conn.io.flush()).map_err(|e| e.to_string())?;
    Ok(conn)
}

fn get_once(u: &Url, size: u64, proxies: &netproxy::Urllib) -> Result<Reply, String> {
    let mut conn = send(u, proxies)?;
    // Headers and a body of at most `size` bytes; more is refused.
    let cap = size as usize + 65_536;
    let mut raw = Vec::new();
    read_capped(&mut conn.io, &mut raw, cap).map_err(|e| e.to_string())?;
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
    if (200..300).contains(&status) {
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

/// `get` that writes the body to `out` as it arrives instead of holding
/// it, so a file of a gigabyte costs a buffer, not a gigabyte. The same
/// redirects, caps and error words as `get`; returns the bytes written.
pub fn get_to(url: &str, size: u64, proxies: &netproxy::Urllib, out: &mut dyn Write) -> Result<u64, String> {
    let mut url = url.to_string();
    let first_tls = parse_url(&url)?.tls;
    for _ in 0..=MAX_HOPS {
        let u = parse_url(&url)?;
        if first_tls && !u.tls {
            return Err(format!("a redirect from https to http: {url}"));
        }
        let conn = send(&u, proxies)?;
        let mut r = std::io::BufReader::with_capacity(65_536, conn.io);
        let (status, headers) = read_head(&mut r)?;
        match status {
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
            200 => return stream_body(&mut r, &headers, size, out),
            _ => return Err(format!("GET {url}: {status}")),
        }
    }
    Err(format!("GET {url}: more than {MAX_HOPS} redirects"))
}

/// The status line and headers (names in lower case), at most 64 KiB.
fn read_head(r: &mut impl std::io::BufRead) -> Result<(u16, Vec<(String, String)>), String> {
    let unreadable = || "a reply that could not be read".to_string();
    let mut total = 0usize;
    let mut status: Option<u16> = None;
    let mut headers = vec![];
    loop {
        let mut line = Vec::new();
        let n = r.read_until(b'\n', &mut line).map_err(|e| e.to_string())?;
        total += n;
        if n == 0 || total > 65_536 {
            return Err(unreadable());
        }
        let l = String::from_utf8_lossy(&line).trim_end_matches(['\r', '\n']).to_string();
        if status.is_none() {
            status = Some(l.split_whitespace().nth(1).and_then(|c| c.parse().ok()).ok_or_else(unreadable)?);
            continue;
        }
        if l.is_empty() {
            return Ok((status.unwrap_or(0), headers));
        }
        if let Some((k, v)) = l.split_once(':') {
            headers.push((k.trim().to_ascii_lowercase(), v.trim().to_string()));
        }
    }
}

/// A 200 reply's body, de-chunked, written to `out`, at most `size` bytes.
fn stream_body(r: &mut impl std::io::BufRead, headers: &[(String, String)], size: u64, out: &mut dyn Write) -> Result<u64, String> {
    let mut chunked = false;
    let mut length: Option<u64> = None;
    for (k, v) in headers {
        match k.as_str() {
            "transfer-encoding" => chunked = v.to_ascii_lowercase().contains("chunked"),
            "content-length" => length = v.parse().ok(),
            "content-encoding" if !v.eq_ignore_ascii_case("identity") => return Err(format!("a reply in the {v} encoding")),
            _ => {}
        }
    }
    let larger = || format!("a file larger than the pinned {size} bytes");
    let mut written = 0u64;
    let mut buf = vec![0u8; 65_536];
    // copy n bytes (None: to the end) from r to out.
    let mut copy = |r: &mut dyn Read, n: Option<u64>, written: &mut u64| -> Result<(), String> {
        let mut left = n;
        loop {
            let want = match left {
                Some(0) => return Ok(()),
                Some(k) => k.min(buf.len() as u64) as usize,
                None => buf.len(),
            };
            let got = r.read(&mut buf[..want]).map_err(|e| e.to_string())?;
            if got == 0 {
                return match left {
                    None => Ok(()),
                    Some(_) => Err("a reply cut short".into()),
                };
            }
            *written += got as u64;
            if *written > size {
                return Err(larger());
            }
            out.write_all(&buf[..got]).map_err(|e| e.to_string())?;
            if let Some(k) = left.as_mut() {
                *k -= got as u64;
            }
        }
    };
    if chunked {
        let bad = || "a chunked reply that could not be read".to_string();
        loop {
            let mut line = Vec::new();
            r.read_until(b'\n', &mut line).map_err(|e| e.to_string())?;
            let text = String::from_utf8_lossy(&line);
            let hex = text.trim().split(';').next().unwrap_or("").trim().to_string();
            let n = u64::from_str_radix(&hex, 16).map_err(|_| bad())?;
            if n == 0 {
                return Ok(written);
            }
            copy(r, Some(n), &mut written).map_err(|e| if e == "a reply cut short" { bad() } else { e })?;
            let mut crlf = Vec::new();
            r.read_until(b'\n', &mut crlf).map_err(|e| e.to_string())?;
            if crlf.iter().any(|b| !b" \t\r\n".contains(b)) {
                return Err(bad());
            }
        }
    }
    if let Some(n) = length {
        if n > size {
            return Err(larger());
        }
    }
    copy(r, length, &mut written)?;
    Ok(written)
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

    /// Records each write, to show a body arrives in pieces, never whole.
    struct Pieces(Vec<usize>, Vec<u8>);
    impl Write for Pieces {
        fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
            self.0.push(b.len());
            self.1.extend_from_slice(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn get_to_streams_a_body_in_pieces() {
        let body: Vec<u8> = (0..1_000_000u32).map(|i| (i % 251) as u8).collect();
        let mut plain = format!("HTTP/1.1 200 OK\r\ncontent-length: {}\r\n\r\n", body.len()).into_bytes();
        plain.extend_from_slice(&body);
        let mut chunked = b"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n".to_vec();
        for c in body.chunks(300_000) {
            chunked.extend_from_slice(format!("{:x}\r\n", c.len()).as_bytes());
            chunked.extend_from_slice(c);
            chunked.extend_from_slice(b"\r\n");
        }
        chunked.extend_from_slice(b"0\r\n\r\n");
        let redirect = b"HTTP/1.1 302 Found\r\nlocation: /b\r\ncontent-length: 0\r\n\r\n".to_vec();
        let port = serve(vec![plain, redirect, chunked]);
        let none = netproxy::urllib_for(&std::collections::HashMap::new());
        for path in ["a", "r"] {
            let mut sink = Pieces(vec![], vec![]);
            let n = get_to(&format!("http://127.0.0.1:{port}/{path}"), body.len() as u64 + 1, &none, &mut sink).unwrap();
            assert_eq!(n, body.len() as u64);
            assert_eq!(sink.1, body);
            assert!(sink.0.len() > 10 && sink.0.iter().all(|&k| k <= 65_536), "{path}: {} writes", sink.0.len());
        }
    }

    #[test]
    fn get_to_refuses_a_body_over_its_cap_or_cut_short() {
        let port = serve(vec![
            b"HTTP/1.1 200 OK\r\ncontent-length: 9\r\n\r\nabcdefghi".to_vec(),
            b"HTTP/1.1 200 OK\r\n\r\nabcdefghi".to_vec(),
            b"HTTP/1.1 200 OK\r\ncontent-length: 9\r\n\r\nabc".to_vec(),
            b"HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\n\r\n".to_vec(),
        ]);
        let none = netproxy::urllib_for(&std::collections::HashMap::new());
        let mut sink = Pieces(vec![], vec![]);
        let url = format!("http://127.0.0.1:{port}/x");
        assert_eq!(get_to(&url, 4, &none, &mut sink).unwrap_err(), "a file larger than the pinned 4 bytes");
        assert_eq!(get_to(&url, 4, &none, &mut sink).unwrap_err(), "a file larger than the pinned 4 bytes");
        assert_eq!(get_to(&url, 20, &none, &mut sink).unwrap_err(), "a reply cut short");
        assert_eq!(get_to(&url, 20, &none, &mut sink).unwrap_err(), format!("GET {url}: 404"));
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
