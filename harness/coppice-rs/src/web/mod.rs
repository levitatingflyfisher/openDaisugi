//! The phone server, ported from Go's `internal/web`: the bearer token and
//! the ban list, the four ways to get a TLS certificate (a local CA among
//! them), the HTTP and websocket server, the page the Go tree ships, the
//! JSON routes under /api, the ask answer over the gate's files, view
//! plugins, the event ring, voice and push.
//!
//! The coppice socket is bound to the operator's uid and nothing else can
//! reach it. HTTPS has no such wall, so this module adds two: a bearer
//! token on every request, and TLS from a certificate the phone trusts.
//! Everything upstream goes through the socket path the caller names; a
//! browser speaks the same protocol a terminal speaks.

pub mod client;
pub mod config;
pub mod der;
pub mod events;
pub mod http;
pub mod localca;
pub mod peer;
pub mod push;
pub mod qr;
pub mod serve;
pub mod statics;
pub mod tls;
pub mod token;
pub mod upstream;
pub mod voice;
pub mod ws;

use std::io::Write;

/// One line through Go's default logger, as slog's default handler
/// writes it: the date and time, the level, the message as it is, then
/// each attribute as key=value with the text handler's quoting.
pub fn log(level: &str, msg: &str, attrs: &[(&str, &str)]) {
    let mut s = format!("{} {level} {msg}", log_time());
    for (k, v) in attrs {
        s.push_str(&format!(" {k}={}", crate::plugins::slog_value(v)));
    }
    s.push('\n');
    let mut e = std::io::stderr().lock();
    let _ = e.write_all(s.as_bytes());
}

/// Now in local time as Go's log package writes it: 2006/01/02 15:04:05.
fn log_time() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0) as libc::time_t;
    // SAFETY: localtime_r fills tm from secs.
    let tm = unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        libc::localtime_r(&secs, &mut tm);
        tm
    };
    format!(
        "{:04}/{:02}/{:02} {:02}:{:02}:{:02}",
        tm.tm_year + 1900,
        tm.tm_mon + 1,
        tm.tm_mday,
        tm.tm_hour,
        tm.tm_min,
        tm.tm_sec
    )
}

/// Go's net.SplitHostPort, for the forms a listen address takes.
pub fn split_host_port(hostport: &str) -> Result<(String, String), String> {
    let missing = || format!("address {hostport}: missing port in address");
    let too_many = || format!("address {hostport}: too many colons in address");
    let Some(i) = hostport.rfind(':') else {
        return Err(missing());
    };
    if let Some(inner) = hostport.strip_prefix('[') {
        let Some(end) = hostport.find(']') else {
            return Err(format!("address {hostport}: missing ']' in address"));
        };
        if end + 1 == hostport.len() {
            return Err(missing());
        }
        if end + 1 != i {
            if hostport.as_bytes()[end + 1] == b':' {
                return Err(too_many());
            }
            return Err(missing());
        }
        let host = &hostport[1..end];
        if inner.contains('[') || hostport[end + 1..].contains(']') {
            return Err(format!("address {hostport}: unexpected '[' in address"));
        }
        return Ok((host.to_string(), hostport[i + 1..].to_string()));
    }
    let host = &hostport[..i];
    if host.contains(':') {
        return Err(too_many());
    }
    if host.contains('[') || host.contains(']') {
        return Err(format!("address {hostport}: unexpected '[' in address"));
    }
    Ok((host.to_string(), hostport[i + 1..].to_string()))
}

/// Go's net.JoinHostPort.
pub fn join_host_port(host: &str, port: &str) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

/// Go's net.ParseIP followed by IsLoopback, for a host that is an address.
/// None when host is not an address.
pub fn ip_is_loopback(host: &str) -> Option<bool> {
    let ip: std::net::IpAddr = parse_ip(host)?;
    Some(match ip {
        std::net::IpAddr::V4(v4) => v4.is_loopback(),
        std::net::IpAddr::V6(v6) => {
            v6.is_loopback() || v6.to_ipv4_mapped().is_some_and(|v4| v4.is_loopback())
        }
    })
}

/// Go's net.ParseIP: dotted IPv4 or IPv6, with no zone. Go reads an IPv4
/// with a leading zero in a byte as an error, as Rust does.
pub fn parse_ip(s: &str) -> Option<std::net::IpAddr> {
    if s.contains('%') {
        return None;
    }
    s.parse().ok()
}

/// Go's net.IP.String for an address: IPv4 in dotted form, an IPv4-mapped
/// IPv6 address as IPv4, other IPv6 in Go's shortest form.
pub fn ip_string(ip: &std::net::IpAddr) -> String {
    match ip {
        std::net::IpAddr::V4(v4) => v4.to_string(),
        std::net::IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            Some(v4) => v4.to_string(),
            None => v6.to_string(),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn split_host_port_matches_go() {
        assert_eq!(
            split_host_port(":8443").unwrap(),
            ("".to_string(), "8443".to_string())
        );
        assert_eq!(
            split_host_port("[::1]:80").unwrap(),
            ("::1".to_string(), "80".to_string())
        );
        assert_eq!(
            split_host_port("nohost").unwrap_err(),
            "address nohost: missing port in address"
        );
        assert_eq!(
            split_host_port("a:b:c").unwrap_err(),
            "address a:b:c: too many colons in address"
        );
    }

    #[test]
    fn words_match_the_floor() {
        // testdata/words.json holds the words the page and the TUI show;
        // a push says a block with no ask in the same words.
        let raw = include_str!("../../../coppice/testdata/words.json");
        let w: serde_json::Value = serde_json::from_str(raw).unwrap();
        assert_eq!(w["blocked_by_gate"], push::BLOCKED_BY_GATE);
        assert_eq!(w["blocked_own_question"], push::BLOCKED_OWN_QUESTION);
    }
}
