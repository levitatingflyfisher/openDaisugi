//! `urllib.parse.urlsplit` of Python 3.12, and `.hostname`, with the
//! ValueErrors it raises: the bracketed-host checks (on the port of
//! `ipaddress`) and the NFKC check on a non-ASCII netloc.

use super::py::text::{lower, repr};
use super::PyErr;

/// `urlsplit(url)`: scheme, netloc, path, query, fragment.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Split {
    pub scheme: String,
    pub netloc: String,
    pub path: String,
    pub query: String,
    pub fragment: String,
}

fn value_error(msg: impl Into<String>) -> PyErr {
    PyErr::value(msg)
}

/// `_WHATWG_C0_CONTROL_OR_SPACE`.
fn c0_or_space(c: char) -> bool {
    (c as u32) <= 0x20
}

fn scheme_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '+' || c == '-' || c == '.'
}

/// `urllib.parse.urlsplit(url)`.
pub fn urlsplit(url: &str) -> Result<Split, PyErr> {
    let url = url.trim_start_matches(c0_or_space);
    let url: String = url.chars().filter(|c| !matches!(c, '\t' | '\r' | '\n')).collect();
    let mut scheme = String::new();
    let mut rest: &str = &url;
    if let Some(i) = url.find(':') {
        let first = url.chars().next().unwrap_or(' ');
        if i > 0 && first.is_ascii() && first.is_ascii_alphabetic() && url[..i].chars().all(scheme_char) {
            scheme = url[..i].to_ascii_lowercase();
            rest = &url[i + 1..];
        }
    }
    let mut netloc = String::new();
    let mut rest = rest.to_string();
    if rest.starts_with("//") {
        let after = &rest[2..];
        let delim = after.find(['/', '?', '#']).unwrap_or(after.len());
        netloc = after[..delim].to_string();
        rest = after[delim..].to_string();
        let (open, close) = (netloc.contains('['), netloc.contains(']'));
        if open != close {
            return Err(value_error("Invalid IPv6 URL"));
        }
        if open && close {
            check_bracketed_netloc(&netloc)?;
        }
    }
    let mut fragment = String::new();
    if let Some((a, b)) = rest.split_once('#') {
        fragment = b.to_string();
        rest = a.to_string();
    }
    let mut query = String::new();
    if let Some((a, b)) = rest.split_once('?') {
        query = b.to_string();
        rest = a.to_string();
    }
    check_netloc(&netloc)?;
    Ok(Split { scheme, netloc, path: rest, query, fragment })
}

/// Code points whose NFKC form holds `/`, `?`, `#`, `@` or `:` (Unicode
/// 15.0.0, as Python 3.12's unicodedata; no composition can absorb one).
const NFKC_DELIMS: &[(u32, u32)] = &[
    (0x2047, 0x2049),
    (0x2100, 0x2101),
    (0x2105, 0x2106),
    (0x2A74, 0x2A74),
    (0xFE13, 0xFE13),
    (0xFE16, 0xFE16),
    (0xFE55, 0xFE56),
    (0xFE5F, 0xFE5F),
    (0xFE6B, 0xFE6B),
    (0xFF03, 0xFF03),
    (0xFF0F, 0xFF0F),
    (0xFF1A, 0xFF1A),
    (0xFF1F, 0xFF20),
];

/// `urllib.parse._checknetloc`.
fn check_netloc(netloc: &str) -> Result<(), PyErr> {
    if netloc.is_ascii() {
        return Ok(());
    }
    let hit = netloc.chars().any(|c| NFKC_DELIMS.iter().any(|&(a, b)| (a..=b).contains(&(c as u32))));
    if hit {
        return Err(value_error(format!(
            "netloc '{netloc}' contains invalid characters under NFKC normalization"
        )));
    }
    Ok(())
}

/// `str.partition(sep)`.
fn partition<'a>(s: &'a str, sep: char) -> (&'a str, bool, &'a str) {
    match s.find(sep) {
        Some(i) => (&s[..i], true, &s[i + sep.len_utf8()..]),
        None => (s, false, ""),
    }
}

/// `str.rpartition(sep)[2]`.
fn after_last(s: &str, sep: char) -> &str {
    match s.rfind(sep) {
        Some(i) => &s[i + sep.len_utf8()..],
        None => s,
    }
}

/// `urllib.parse._check_bracketed_netloc`.
fn check_bracketed_netloc(netloc: &str) -> Result<(), PyErr> {
    let host_and_port = after_last(netloc, '@');
    let (before, open, bracketed) = partition(host_and_port, '[');
    let hostname = if open {
        if !before.is_empty() {
            return Err(value_error("Invalid IPv6 URL"));
        }
        let (h, _, port) = partition(bracketed, ']');
        if !port.is_empty() && !port.starts_with(':') {
            return Err(value_error("Invalid IPv6 URL"));
        }
        h
    } else {
        partition(host_and_port, ':').0
    };
    check_bracketed_host(hostname)
}

/// `urllib.parse._check_bracketed_host`.
fn check_bracketed_host(hostname: &str) -> Result<(), PyErr> {
    if hostname.starts_with('v') {
        // re.match(r"\Av[a-fA-F0-9]+\..+\Z", hostname); `.` stops only at
        // a newline, and urlsplit removed those.
        let rest = &hostname[1..];
        let hex = rest.chars().take_while(|c| c.is_ascii_hexdigit()).count();
        let ok = hex > 0 && rest[hex..].starts_with('.') && rest[hex + 1..].chars().count() > 0;
        if !ok {
            return Err(value_error("IPvFuture address is invalid"));
        }
        return Ok(());
    }
    if is_ipv4(hostname) {
        return Err(value_error("An IPv4 address cannot be in brackets"));
    }
    if !is_ipv6(hostname) {
        return Err(value_error(format!("{} does not appear to be an IPv4 or IPv6 address", repr(hostname))));
    }
    Ok(())
}

/// `ipaddress.IPv4Address(s)` succeeds.
fn is_ipv4(s: &str) -> bool {
    if s.is_empty() || s.contains('/') {
        return false;
    }
    let octets: Vec<&str> = s.split('.').collect();
    octets.len() == 4 && octets.iter().all(|o| ipv4_octet(o))
}

fn ipv4_octet(o: &str) -> bool {
    if o.is_empty() || !o.bytes().all(|b| b.is_ascii_digit()) || o.len() > 3 {
        return false;
    }
    if o != "0" && o.starts_with('0') {
        return false;
    }
    o.parse::<u32>().map(|v| v <= 255).unwrap_or(false)
}

/// `ipaddress.IPv6Address(s)` succeeds.
fn is_ipv6(s: &str) -> bool {
    if s.contains('/') {
        return false;
    }
    let (addr, sep, scope) = partition(s, '%');
    if sep && (scope.is_empty() || scope.contains('%')) {
        return false;
    }
    if addr.is_empty() {
        return false;
    }
    let mut parts: Vec<String> = addr.split(':').map(str::to_string).collect();
    if parts.len() < 3 {
        return false;
    }
    if parts.last().is_some_and(|p| p.contains('.')) {
        let last = parts.pop().unwrap_or_default();
        if !is_ipv4(&last) {
            return false;
        }
        let v: Vec<u32> = last.split('.').map(|o| o.parse().unwrap_or(0)).collect();
        let n = (v[0] << 24) | (v[1] << 16) | (v[2] << 8) | v[3];
        parts.push(format!("{:x}", (n >> 16) & 0xFFFF));
        parts.push(format!("{:x}", n & 0xFFFF));
    }
    if parts.len() > 9 {
        return false;
    }
    let mut skip = None;
    for (i, p) in parts.iter().enumerate().take(parts.len() - 1).skip(1) {
        if p.is_empty() {
            if skip.is_some() {
                return false;
            }
            skip = Some(i);
        }
    }
    let (hi, lo) = match skip {
        Some(k) => {
            let mut hi = k;
            let mut lo = parts.len() - k - 1;
            if parts[0].is_empty() {
                hi -= 1;
                if hi != 0 {
                    return false;
                }
            }
            if parts[parts.len() - 1].is_empty() {
                lo -= 1;
                if lo != 0 {
                    return false;
                }
            }
            if hi + lo > 7 {
                return false;
            }
            (hi, lo)
        }
        None => {
            if parts.len() != 8 || parts[0].is_empty() || parts[7].is_empty() {
                return false;
            }
            (8, 0)
        }
    };
    let hextet = |h: &str| !h.is_empty() && h.len() <= 4 && h.bytes().all(|b| b.is_ascii_hexdigit());
    parts[..hi].iter().all(|h| hextet(h)) && parts[parts.len() - lo..].iter().all(|h| hextet(h))
}

/// `.hostname` of a split: the host lowercased, a zone after `%` kept.
pub fn hostname(netloc: &str) -> Option<String> {
    let hostinfo = after_last(netloc, '@');
    let (_, open, bracketed) = partition(hostinfo, '[');
    let host = if open { partition(bracketed, ']').0 } else { partition(hostinfo, ':').0 };
    if host.is_empty() {
        return None;
    }
    let (h, pct, zone) = partition(host, '%');
    Some(if pct { format!("{}%{zone}", lower(h)) } else { lower(h) })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn host(u: &str) -> Result<(String, Option<String>), String> {
        urlsplit(u).map(|s| (s.scheme.clone(), hostname(&s.netloc))).map_err(|e| e.msg)
    }

    #[test]
    fn urls_split_as_python_splits_them() {
        assert_eq!(host("https://Example.COM:8080/x").unwrap(), ("https".into(), Some("example.com".into())));
        assert_eq!(host("http://[::1]:8080/").unwrap(), ("http".into(), Some("::1".into())));
        assert_eq!(host("http://[fe80::1%tESt]/").unwrap(), ("http".into(), Some("fe80::1%tESt".into())));
        assert_eq!(host("http://[::1/").unwrap_err(), "Invalid IPv6 URL");
        assert_eq!(host("http://[1.2.3.4]/").unwrap_err(), "An IPv4 address cannot be in brackets");
        assert_eq!(host("http://[vx]/").unwrap_err(), "IPvFuture address is invalid");
        assert_eq!(host("http://[v1.x]/").unwrap().1, Some("v1.x".into()));
        assert_eq!(host("http://[zz]/").unwrap_err(), "'zz' does not appear to be an IPv4 or IPv6 address");
        assert_eq!(host("http://a[::1]/").unwrap_err(), "Invalid IPv6 URL");
        assert_eq!(
            host("http://\u{ff1a}.test/").unwrap_err(),
            "netloc '\u{ff1a}.test' contains invalid characters under NFKC normalization"
        );
        assert_eq!(host("https://\u{c9}X.test").unwrap().1, Some("\u{e9}x.test".into()));
        assert_eq!(host("example.com/x").unwrap(), (String::new(), None));
    }

    #[test]
    fn ipv6_is_ipaddress() {
        for (s, ok) in [
            ("::1", true),
            ("::", true),
            ("1::", true),
            ("1:2:3:4:5:6:7:8", true),
            ("1:2:3:4:5:6:7::", true),
            ("::ffff:1.2.3.4", true),
            ("1:2:3:4:5:6:7:8:9", false),
            (":1:2:3:4:5:6:7", false),
            ("1::2::3", false),
            ("12345::", false),
            ("::1%", false),
            ("::1%a%b", false),
            ("::1.2.3.04", false),
        ] {
            assert_eq!(is_ipv6(s), ok, "{s}");
        }
    }
}
