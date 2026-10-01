//! Who is behind a TCP connection from this box: the processes that hold
//! its client end, found through /proc/net/tcp and each process's fds. A
//! pane process may propose; it may not allow, here or on the socket.

use std::collections::HashSet;
use std::net::{IpAddr, SocketAddr};

use serde_json::{json, Value};

use super::upstream::{self, Dialer};
use crate::godec::Any;

/// The message a pane gets when it tries to answer an ask.
pub const PANE_REFUSAL: &str = "a pane can propose. It cannot allow.";

/// The id of the hello a websocket sends upstream.
pub const WEB_HELLO_ID: &str = "web-hello";

/// Whether ip is loopback or one of this host's own addresses.
fn is_local_ip(ip: &IpAddr) -> bool {
    if ip.is_loopback() {
        return true;
    }
    if let IpAddr::V6(v6) = ip {
        if v6.to_ipv4_mapped().is_some_and(|v4| v4.is_loopback()) {
            return true;
        }
    }
    match local_addrs() {
        Some(addrs) => addrs.iter().any(|a| same_ip(a, ip)),
        // With no list of our own addresses, every peer might be local.
        None => true,
    }
}

/// Go's IP.Equal: an IPv4 address equals its IPv4-mapped IPv6 form.
fn same_ip(a: &IpAddr, b: &IpAddr) -> bool {
    let v4 = |x: &IpAddr| match x {
        IpAddr::V4(v) => Some(*v),
        IpAddr::V6(v) => v.to_ipv4_mapped(),
    };
    match (v4(a), v4(b)) {
        (Some(x), Some(y)) => x == y,
        (None, None) => a == b,
        _ => false,
    }
}

/// This host's interface addresses.
fn local_addrs() -> Option<Vec<IpAddr>> {
    let mut out = Vec::new();
    // SAFETY: getifaddrs fills a list that freeifaddrs releases; each
    // entry is read only while the list lives.
    unsafe {
        let mut ifap: *mut libc::ifaddrs = std::ptr::null_mut();
        if libc::getifaddrs(&mut ifap) != 0 {
            return None;
        }
        let mut cur = ifap;
        while !cur.is_null() {
            let a = (*cur).ifa_addr;
            if !a.is_null() {
                match (*a).sa_family as i32 {
                    libc::AF_INET => {
                        let sin = &*(a as *const libc::sockaddr_in);
                        out.push(IpAddr::from(
                            u32::from_be(sin.sin_addr.s_addr).to_be_bytes(),
                        ));
                    }
                    libc::AF_INET6 => {
                        let sin6 = &*(a as *const libc::sockaddr_in6);
                        out.push(IpAddr::from(sin6.sin6_addr.s6_addr));
                    }
                    _ => {}
                }
            }
            cur = (*cur).ifa_next;
        }
        libc::freeifaddrs(ifap);
    }
    Some(out)
}

/// One address of /proc/net/tcp or tcp6: the IP in hex, each 32-bit word
/// in host order, a colon, and the port in hex.
fn parse_proc_addr(s: &str) -> Option<(IpAddr, u16)> {
    let (host, port) = s.split_once(':')?;
    if host.len() != 8 && host.len() != 32 {
        return None;
    }
    let raw: Vec<u8> = (0..host.len() / 2)
        .map(|i| u8::from_str_radix(&host[2 * i..2 * i + 2], 16))
        .collect::<Result<_, _>>()
        .ok()?;
    let mut ip = vec![0u8; raw.len()];
    for i in (0..raw.len()).step_by(4) {
        let w = u32::from_le_bytes([raw[i], raw[i + 1], raw[i + 2], raw[i + 3]]);
        ip[i..i + 4].copy_from_slice(&w.to_be_bytes());
    }
    let addr = if ip.len() == 4 {
        IpAddr::from([ip[0], ip[1], ip[2], ip[3]])
    } else {
        let mut b = [0u8; 16];
        b.copy_from_slice(&ip);
        IpAddr::from(b)
    };
    Some((addr, u16::from_str_radix(port, 16).ok()?))
}

/// The pids on this box that hold the client end of a connection, and
/// whether the peer is on this box at all. Err for a local peer that no
/// process could be found for.
pub fn local_peer_pids(remote: SocketAddr, local: SocketAddr) -> (Result<Vec<i32>, ()>, bool) {
    if !is_local_ip(&remote.ip()) {
        return (Ok(Vec::new()), false);
    }
    let mut inodes = HashSet::new();
    for f in ["/proc/net/tcp", "/proc/net/tcp6"] {
        let Ok(text) = std::fs::read_to_string(f) else {
            continue;
        };
        for line in text.lines().skip(1) {
            let fields: Vec<&str> = line.split_whitespace().collect();
            if fields.len() < 10 {
                continue;
            }
            let (Some((lip, lport)), Some((rip, rport))) =
                (parse_proc_addr(fields[1]), parse_proc_addr(fields[2]))
            else {
                continue;
            };
            if lport == remote.port()
                && rport == local.port()
                && same_ip(&lip, &remote.ip())
                && same_ip(&rip, &local.ip())
                && fields[9] != "0"
            {
                inodes.insert(fields[9].to_string());
            }
        }
    }
    if inodes.is_empty() {
        return (Err(()), true);
    }
    let Ok(procs) = std::fs::read_dir("/proc") else {
        return (Err(()), true);
    };
    let mut names: Vec<String> = procs
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .collect();
    // Go's os.ReadDir lists names in order.
    names.sort();
    let mut pids = Vec::new();
    for name in names {
        let Ok(pid) = name.parse::<i32>() else {
            continue;
        };
        let Ok(fds) = std::fs::read_dir(format!("/proc/{name}/fd")) else {
            continue;
        };
        for fd in fds.flatten() {
            let Ok(link) = std::fs::read_link(fd.path()) else {
                continue;
            };
            let link = link.to_string_lossy();
            if let Some(ino) = link
                .strip_prefix("socket:[")
                .and_then(|r| r.strip_suffix(']'))
            {
                if inodes.contains(ino) {
                    pids.push(pid);
                    break;
                }
            }
        }
    }
    if pids.is_empty() {
        return (Err(()), true);
    }
    (Ok(pids), true)
}

/// The part of a hello that names the local process a request came from:
/// None for a peer on another host, or when every holder is this server.
pub fn peer_hello(remote: SocketAddr, local: SocketAddr) -> Option<Vec<(&'static str, Value)>> {
    let (pids, is_local) = local_peer_pids(remote, local);
    if !is_local {
        return None;
    }
    let pids = match pids {
        Ok(p) if !p.is_empty() => p,
        _ => return Some(vec![("peer_unknown", json!(true))]),
    };
    let me = std::process::id() as i32;
    let others: Vec<i32> = pids.into_iter().filter(|p| *p != me).collect();
    if others.is_empty() {
        return None;
    }
    Some(vec![("peer_pids", json!(others))])
}

/// Asks the server whether the peer a hello names may answer an ask.
/// Any failure is a no.
pub fn place_allows(d: &Dialer, hello: Vec<(&'static str, Value)>) -> bool {
    let mut req = vec![("cmd", json!("hello"))];
    req.extend(hello);
    match upstream::call(d, req) {
        Ok(msg) => matches!(
            msg.get("result").and_then(|r| r.get("allow")),
            Some(Any::Bool(true))
        ),
        Err(_) => false,
    }
}

/// Whether a line from the server answers the web hello.
pub fn is_web_hello_reply(line: &[u8]) -> bool {
    if !String::from_utf8_lossy(line).contains(WEB_HELLO_ID) {
        return false;
    }
    match crate::godec::decode_map(line) {
        Ok(m) => matches!(m.get("id"), Some(Any::Str(s)) if s == WEB_HELLO_ID),
        Err(_) => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn proc_addresses_read_as_go_reads_them() {
        let (ip, port) = parse_proc_addr("0100007F:1F90").unwrap();
        assert_eq!(ip.to_string(), "127.0.0.1");
        assert_eq!(port, 8080);
        let (ip, _) = parse_proc_addr("00000000000000000000000001000000:0050").unwrap();
        assert_eq!(ip.to_string(), "::1");
        assert!(parse_proc_addr("zz:1").is_none());
    }
}
