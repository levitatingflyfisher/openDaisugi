//! Proxies from the environment, as the oracle's HTTP clients read them,
//! and the one connector every HTTP client in this crate opens a
//! connection with.
//!
//! Two rule sets, since the oracle has two kinds of client:
//!
//! - `Httpx`: httpx 0.28 with `trust_env`, as the gateway's upstream client
//!   and the model client (`llm_client.py`, the gate's llm_check) use it.
//!   `HTTP_PROXY`, `HTTPS_PROXY` and `ALL_PROXY`, and `NO_PROXY` as httpx
//!   turns each entry into a URL pattern. There is no bypass for loopback:
//!   a request to 127.0.0.1 goes through the proxy unless `NO_PROXY` names
//!   it.
//! - `Urllib`: `urllib.request.urlopen`, as the potion download and the
//!   Switchyard health probe use it. No `ALL_PROXY`; `NO_PROXY` as a list
//!   of DNS suffixes; an https request always goes through an HTTP CONNECT
//!   tunnel, whatever the proxy URL's scheme.
//!
//! Both read the variables as `urllib.request.getproxies` does: any
//! `<scheme>_proxy` in any case, a name that ends in lower-case `_proxy`
//! winning, and `HTTP_PROXY` dropped when `REQUEST_METHOD` is set.
//!
//! The connector sends HTTPS through a `CONNECT host:port` tunnel and then
//! TLS, and HTTP to the proxy in absolute form (the caller writes the
//! request line). Credentials in the proxy URL become a
//! `Proxy-Authorization: Basic` header. A proxy URL of the https scheme
//! gets TLS to the proxy first, as httpx does.

use std::io::{self, Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

/// httpx's error when a proxy URL names SOCKS and the socksio package is
/// not installed (it is not, in the oracle's environment).
pub const SOCKS_TEXT: &str = "Using SOCKS proxy, but the 'socksio' package is not installed. \
     Make sure to install httpx using `pip install httpx[socks]`.";

// ---------------------------------------------------------------------------
// Reading the variables
// ---------------------------------------------------------------------------

/// `urllib.request.getproxies_environment`, as scheme to value. `vars` in
/// the environment's order when `ordered`; otherwise two names that differ
/// only in case and hold different values make the answer depend on an
/// order this caller does not have, and that is an error (not ported).
pub fn getproxies(vars: &[(String, String)], ordered: bool) -> Result<Vec<(String, String)>, String> {
    let mut out: Vec<(String, String)> = Vec::new();
    let set = |out: &mut Vec<(String, String)>, k: &str, v: &str| {
        if let Some(e) = out.iter_mut().find(|(n, _)| n == k) {
            e.1 = v.to_string();
        } else {
            out.push((k.to_string(), v.to_string()));
        }
    };
    let mut seen1: Vec<(String, String)> = vec![];
    let mut seen2: Vec<(String, String)> = vec![];
    let mut env: Vec<(String, String, String)> = vec![];
    for (name, value) in vars {
        let chars: Vec<char> = name.chars().collect();
        if chars.len() > 5 && chars[chars.len() - 6] == '_' && chars[chars.len() - 5..].iter().collect::<String>().to_lowercase() == "proxy" {
            let proxy_name: String = chars[..chars.len() - 6].iter().collect::<String>().to_lowercase();
            env.push((name.clone(), value.clone(), proxy_name.clone()));
            if !value.is_empty() {
                if !ordered && !name.ends_with("_proxy") {
                    if seen1.iter().any(|(k, v)| *k == proxy_name && v != value) {
                        return Err(format!("{name} and another {proxy_name}_proxy with a different value"));
                    }
                    seen1.push((proxy_name.clone(), value.clone()));
                }
                set(&mut out, &proxy_name, value);
            }
        }
    }
    if vars.iter().any(|(k, _)| k == "REQUEST_METHOD") {
        out.retain(|(k, _)| k != "http");
    }
    for (name, value, proxy_name) in &env {
        if name.ends_with("_proxy") {
            if !ordered {
                if seen2.iter().any(|(k, v)| k == proxy_name && v != value) {
                    return Err(format!("{name} and another {proxy_name}_proxy with a different value"));
                }
                seen2.push((proxy_name.clone(), value.clone()));
            }
            if !value.is_empty() {
                set(&mut out, proxy_name, value);
            } else {
                out.retain(|(k, _)| k != proxy_name);
            }
        }
    }
    Ok(out)
}

fn get<'a>(p: &'a [(String, String)], k: &str) -> Option<&'a str> {
    p.iter().find(|(n, _)| n == k).map(|(_, v)| v.as_str())
}

/// Python's `str.strip()` with no argument.
fn py_strip(s: &str) -> &str {
    s.trim_matches(|c: char| c.is_whitespace() || ('\x1c'..='\x1f').contains(&c))
}

/// The process's variables, in their order, the ones that are not UTF-8
/// left out (as the binaries' environment maps leave them out).
pub fn process_vars() -> Vec<(String, String)> {
    std::env::vars_os().filter_map(|(k, v)| Some((k.into_string().ok()?, v.into_string().ok()?))).collect()
}

/// A map's variables in the process environment's order, and whether
/// that order is the one Python's `os.environ` would give: true when
/// every `*_proxy` variable of the map is in this process's environment
/// with the same value (the map was read from it). A variable the process
/// does not hold comes after, in name order.
pub fn ordered_vars(env: &std::collections::HashMap<String, String>) -> (Vec<(String, String)>, bool) {
    let mut out: Vec<(String, String)> = vec![];
    for (k, v) in process_vars() {
        if env.get(&k) == Some(&v) {
            out.push((k, v));
        }
    }
    let mut rest: Vec<(String, String)> =
        env.iter().filter(|(k, v)| !out.iter().any(|(ok, ov)| ok == *k && ov == *v)).map(|(k, v)| (k.clone(), v.clone())).collect();
    rest.sort();
    let proxy_name = |k: &str| k.len() > 5 && k.to_ascii_lowercase().ends_with("_proxy");
    let ordered = !rest.iter().any(|(k, _)| proxy_name(k));
    out.extend(rest);
    (out, ordered)
}

/// The httpx routing for a map of variables (`ordered_vars`).
pub fn httpx_for(env: &std::collections::HashMap<String, String>) -> Httpx {
    let (v, ordered) = ordered_vars(env);
    Httpx::from_vars(&v, ordered)
}

/// The urllib routing for a map of variables (`ordered_vars`).
pub fn urllib_for(env: &std::collections::HashMap<String, String>) -> Urllib {
    let (v, ordered) = ordered_vars(env);
    Urllib::from_vars(&v, ordered)
}

/// A map's variables, in no order.
pub fn map_vars(env: &std::collections::HashMap<String, String>) -> Vec<(String, String)> {
    env.iter().map(|(k, v)| (k.clone(), v.clone())).collect()
}

// ---------------------------------------------------------------------------
// What a request goes through
// ---------------------------------------------------------------------------

/// The first hop when it is a proxy.
#[derive(Clone, Debug, PartialEq)]
pub struct Proxy {
    /// TLS to the proxy itself (a proxy URL of the https scheme).
    pub tls: bool,
    pub host: String,
    pub port: u16,
    /// The `Proxy-Authorization` value, from the URL's credentials.
    pub auth: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Tunnel {
    /// httpcore's CONNECT: `Host` then `Accept: */*` then the credentials;
    /// any 2xx answer opens the tunnel; an IPv6 target without brackets.
    Httpx,
    /// http.client's CONNECT: the credentials then `Host`; only 200 opens
    /// the tunnel; an IPv6 target in brackets.
    Urllib,
}

#[derive(Clone, Debug, PartialEq)]
pub enum Route {
    Direct,
    /// An HTTP request in absolute form to the proxy.
    Forward(Proxy),
    /// A CONNECT tunnel through the proxy, then TLS to the origin.
    Tunnel(Proxy, Tunnel),
}

/// Why a request cannot be sent.
#[derive(Clone, Debug, PartialEq)]
pub enum Refusal {
    /// The oracle's client cannot be built: every request fails with this
    /// text (httpx's error for a SOCKS or unknown proxy scheme).
    Fails(String),
    /// A shape of variable this binary does not model.
    Unported(String),
    /// httpx.InvalidURL's text: the client cannot be built, and the error
    /// is not one the callers' except clauses name.
    Invalid(String),
}

/// The origin a request goes to: TLS or not, the host as the URL has it
/// (an IPv6 address without brackets) and the port.
#[derive(Clone, Debug)]
pub struct Origin {
    pub tls: bool,
    pub host: String,
    pub port: u16,
    /// Whether the URL wrote the port (urllib compares `host:port` too).
    pub explicit_port: bool,
}

// ---------------------------------------------------------------------------
// httpx
// ---------------------------------------------------------------------------

/// A parsed httpx URL pattern (`httpx._utils.URLPattern`).
#[derive(Clone, Debug)]
struct Pattern {
    scheme: String,
    host: String,
    port: Option<u16>,
}

impl Pattern {
    fn priority(&self) -> (u8, i64, i64) {
        (if self.port.is_some() { 0 } else { 1 }, -(self.host.chars().count() as i64), -(self.scheme.chars().count() as i64))
    }

    fn matches(&self, scheme: &str, host: &str, port: Option<u16>) -> bool {
        if !self.scheme.is_empty() && self.scheme != scheme {
            return false;
        }
        if !self.host.is_empty() {
            let ok = if let Some(d) = self.host.strip_prefix("*.") {
                host.len() > d.len() + 1 && host.ends_with(d) && host[..host.len() - d.len()].ends_with('.')
            } else if let Some(d) = self.host.strip_prefix('*') {
                host == d || (host.len() > d.len() + 1 && host.ends_with(d) && host[..host.len() - d.len()].ends_with('.'))
            } else {
                host == self.host
            };
            if !ok {
                return false;
            }
        }
        if self.port.is_some() && self.port != port {
            return false;
        }
        true
    }
}

fn default_port(scheme: &str) -> Option<u16> {
    match scheme {
        "http" | "ws" => Some(80),
        "https" | "wss" => Some(443),
        "ftp" => Some(21),
        _ => None,
    }
}

/// An httpx URL cut into scheme, userinfo, host and port, the scheme and
/// host lower-cased. Only the shapes this binary reads the same way httpx
/// does; anything else is not ported.
struct Url {
    scheme: String,
    userinfo: Option<String>,
    host: String,
    port: Option<u16>,
    /// The rest after the authority, as written.
    rest: String,
}

fn parse_url(s: &str) -> Result<Url, UrlErr> {
    let u = parse_httpx(s)?;
    let rest = u.rest();
    Ok(Url {
        scheme: u.scheme,
        userinfo: if u.userinfo.is_empty() { None } else { Some(u.userinfo) },
        host: u.host,
        port: u.port,
        rest,
    })
}

/// The refusal a parse error gives: httpx's InvalidURL fails every
/// request, as the client cannot be built; anything else is not modelled.
fn fails_or(e: UrlErr, why: &str) -> Httpx {
    match e {
        UrlErr::Invalid(msg) => Httpx { mounts: vec![], refusal: Some(Refusal::Invalid(msg)) },
        UrlErr::Unported(w) => Httpx::unported(format!("{why}: {w}")),
    }
}

/// `ipaddress.IPv4Address(h.split("/")[0])` succeeds.
fn is_ipv4(h: &str) -> bool {
    h.split('/').next().unwrap_or("").parse::<std::net::Ipv4Addr>().is_ok()
}

fn is_ipv6(h: &str) -> bool {
    h.split('/').next().unwrap_or("").parse::<std::net::Ipv6Addr>().is_ok()
}

/// The proxy routing httpx's `Client` builds from the environment.
#[derive(Clone, Debug)]
pub struct Httpx {
    mounts: Vec<(Pattern, Option<Proxy>)>,
    refusal: Option<Refusal>,
}

impl Httpx {
    pub fn none() -> Httpx {
        Httpx { mounts: vec![], refusal: None }
    }

    pub fn from_vars(vars: &[(String, String)], ordered: bool) -> Httpx {
        match getproxies(vars, ordered) {
            Ok(p) => Httpx::from_proxies(&p),
            Err(why) => Httpx { mounts: vec![], refusal: Some(Refusal::Unported(why)) },
        }
    }

    /// `httpx._utils.get_environment_proxies`, then the client's mounts.
    pub fn from_proxies(p: &[(String, String)]) -> Httpx {
        let mut keys: Vec<(String, Option<String>)> = vec![];
        let put = |keys: &mut Vec<(String, Option<String>)>, k: String, v: Option<String>| {
            if let Some(e) = keys.iter_mut().find(|(n, _)| *n == k) {
                e.1 = v;
            } else {
                keys.push((k, v));
            }
        };
        for scheme in ["http", "https", "all"] {
            if let Some(v) = get(p, scheme).filter(|v| !v.is_empty()) {
                let v = if v.contains("://") { v.to_string() } else { format!("http://{v}") };
                put(&mut keys, format!("{scheme}://"), Some(v));
            }
        }
        for entry in get(p, "no").unwrap_or("").split(',') {
            let h = py_strip(entry);
            if h == "*" {
                return Httpx::none();
            }
            if h.is_empty() {
                continue;
            }
            let key = if h.contains("://") {
                h.to_string()
            } else if is_ipv4(h) {
                format!("all://{h}")
            } else if is_ipv6(h) {
                format!("all://[{h}]")
            } else if h.to_lowercase() == "localhost" {
                format!("all://{h}")
            } else {
                format!("all://*{h}")
            };
            put(&mut keys, key, None);
        }
        // Proxy(url) for each, in order: the first bad scheme raises.
        let mut proxies: Vec<(String, Option<(Url, String)>)> = vec![];
        for (k, v) in keys {
            let parsed = match v {
                None => None,
                Some(v) => match parse_url(&v) {
                    Ok(u) => Some((u, v)),
                    Err(e) => return fails_or(e, "a proxy URL"),
                },
            };
            if let Some((u, v)) = &parsed {
                if !matches!(u.scheme.as_str(), "http" | "https" | "socks5" | "socks5h") {
                    return match unknown_scheme_text(u, v) {
                        Some(t) => Httpx { mounts: vec![], refusal: Some(Refusal::Fails(t)) },
                        None => Httpx::unported(format!("the proxy URL {v:?}")),
                    };
                }
            }
            proxies.push((k, parsed));
        }
        let mut mounts = vec![];
        for (k, parsed) in proxies {
            let pat = match pattern(&k) {
                Ok(p) => p,
                Err(e) => return fails_or(e, "a NO_PROXY entry"),
            };
            let proxy = match parsed {
                None => None,
                Some((u, v)) => {
                    if u.scheme.starts_with("socks5") {
                        return Httpx { mounts: vec![], refusal: Some(Refusal::Fails(SOCKS_TEXT.into())) };
                    }
                    if u.host.is_empty() || u.host.contains('%') {
                        // No host, or one httpx sends percent-encoded: where
                        // the connection fails is not modelled.
                        return Httpx::unported(format!("the proxy URL {v:?}"));
                    }
                    let auth = u.userinfo.as_ref().and_then(|ui| {
                        let (user, pass) = ui.split_once(':').unwrap_or((ui, ""));
                        let (user, pass) = (crate::gateway::http::unquote(user), crate::gateway::http::unquote(pass));
                        if user.is_empty() && pass.is_empty() {
                            None
                        } else {
                            Some(basic(&format!("{user}:{pass}")))
                        }
                    });
                    let tls = u.scheme == "https";
                    Some(Proxy { tls, port: u.port.unwrap_or(if tls { 443 } else { 80 }), host: u.host, auth })
                }
            };
            mounts.push((pat, proxy));
        }
        // sorted() is stable: equal priorities keep the dict's order.
        mounts.sort_by_key(|(p, _)| p.priority());
        Httpx { mounts, refusal: None }
    }

    fn unported(why: String) -> Httpx {
        Httpx { mounts: vec![], refusal: Some(Refusal::Unported(why)) }
    }

    /// Whether the client can be built at all.
    pub fn refusal(&self) -> Option<&Refusal> {
        self.refusal.as_ref()
    }

    /// `Client._transport_for_url`: the first mount whose pattern matches.
    pub fn route(&self, o: &Origin) -> Result<Route, Refusal> {
        if let Some(r) = &self.refusal {
            return Err(r.clone());
        }
        let scheme = if o.tls { "https" } else { "http" };
        // encode_host lower-cases a name, and keeps an IPv6 address as
        // written.
        let host = if o.host.contains(':') { o.host.clone() } else { o.host.to_ascii_lowercase() };
        let port = if Some(o.port) == default_port(scheme) { None } else { Some(o.port) };
        for (pat, proxy) in &self.mounts {
            if pat.matches(scheme, &host, port) {
                return Ok(match proxy {
                    None => Route::Direct,
                    Some(p) if !host.is_ascii() => return Err(Refusal::Unported(format!("the host {:?} through {}", o.host, p.host))),
                    Some(p) if o.tls => Route::Tunnel(p.clone(), Tunnel::Httpx),
                    Some(p) => Route::Forward(p.clone()),
                });
            }
        }
        Ok(Route::Direct)
    }
}

/// `URLPattern(key)`: the scheme, the host and the port, nothing else.
fn pattern(key: &str) -> Result<Pattern, UrlErr> {
    if !key.is_empty() && !key.contains(':') {
        return Err(UrlErr::Unported(format!("the proxy key {key:?}")));
    }
    let u = parse_url(key)?;
    if u.host.contains("xn--") {
        // URLPattern's host is the IDNA-decoded one: its length orders the
        // mounts.
        return Err(UrlErr::Unported(format!("the IDNA host in {key:?}")));
    }
    Ok(Pattern {
        scheme: if u.scheme == "all" { String::new() } else { u.scheme },
        host: if u.host == "*" { String::new() } else { u.host },
        port: u.port,
    })
}

/// httpx's `ValueError` text for a proxy URL of an unknown scheme, for
/// the shapes whose `repr(URL)` this binary writes as httpx does.
fn unknown_scheme_text(u: &Url, _raw: &str) -> Option<String> {
    if u.userinfo.is_some() || !u.rest.chars().all(|c| c.is_ascii_alphanumeric() || "/-._~".contains(c)) {
        return None;
    }
    let host = if u.host.contains(':') { format!("[{}]", u.host) } else { u.host.clone() };
    let port = u.port.map(|p| format!(":{p}")).unwrap_or_default();
    Some(format!("Unknown scheme for proxy URL URL('{}://{host}{port}{}')", u.scheme, u.rest))
}

fn basic(userpass: &str) -> String {
    format!("Basic {}", b64(userpass.as_bytes()))
}

fn b64(data: &[u8]) -> String {
    const A: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for c in data.chunks(3) {
        let n = (c[0] as u32) << 16 | (*c.get(1).unwrap_or(&0) as u32) << 8 | *c.get(2).unwrap_or(&0) as u32;
        out.push(A[(n >> 18) as usize & 63] as char);
        out.push(A[(n >> 12) as usize & 63] as char);
        out.push(if c.len() > 1 { A[(n >> 6) as usize & 63] as char } else { '=' });
        out.push(if c.len() > 2 { A[n as usize & 63] as char } else { '=' });
    }
    out
}

// ---------------------------------------------------------------------------
// urllib
// ---------------------------------------------------------------------------

/// The proxy routing `urllib.request.urlopen` does with the default
/// opener.
#[derive(Clone, Debug)]
pub struct Urllib {
    proxies: Vec<(String, String)>,
    refusal: Option<Refusal>,
}

impl Urllib {
    pub fn from_vars(vars: &[(String, String)], ordered: bool) -> Urllib {
        match getproxies(vars, ordered) {
            Ok(proxies) => Urllib { proxies, refusal: None },
            Err(why) => Urllib { proxies: vec![], refusal: Some(Refusal::Unported(why)) },
        }
    }

    /// `proxy_bypass_environment(host)`: `host` is the URL's netloc host,
    /// with its port when the URL wrote one.
    fn bypass(&self, host: &str) -> bool {
        let Some(no) = get(&self.proxies, "no") else { return false };
        if no == "*" {
            return true;
        }
        let host = host.to_lowercase();
        let hostonly = match host.rsplit_once(':') {
            Some((h, p)) if p.chars().all(|c| c.is_ascii_digit()) => h.to_string(),
            _ => host.clone(),
        };
        for name in no.split(',') {
            let name = py_strip(name);
            if name.is_empty() {
                continue;
            }
            let name = name.trim_start_matches('.').to_lowercase();
            if hostonly == name || host == name {
                return true;
            }
            let dotted = format!(".{name}");
            if hostonly.ends_with(&dotted) || host.ends_with(&dotted) {
                return true;
            }
        }
        false
    }

    pub fn route(&self, o: &Origin) -> Result<Route, Refusal> {
        if let Some(r) = &self.refusal {
            return Err(r.clone());
        }
        let scheme = if o.tls { "https" } else { "http" };
        let Some(value) = get(&self.proxies, scheme) else { return Ok(Route::Direct) };
        let bracketed = if o.host.contains(':') { format!("[{}]", o.host) } else { o.host.clone() };
        let netloc = if o.explicit_port { format!("{bracketed}:{}", o.port) } else { bracketed };
        if self.bypass(&netloc) {
            return Ok(Route::Direct);
        }
        let unported = || Refusal::Unported(format!("the proxy {scheme}_proxy={value:?}"));
        let (pscheme, user, pass, hostport) = parse_proxy(value).ok_or_else(unported)?;
        let hostport = crate::gateway::http::unquote(&hostport);
        if !o.host.is_ascii() || !hostport.is_ascii() {
            return Err(unported());
        }
        let default = if o.tls { 443 } else { 80 };
        let (host, port) = if let Some(h) = hostport.strip_prefix('[') {
            let (h, p) = h.split_once(']').ok_or_else(unported)?;
            match p.strip_prefix(':') {
                Some(p) if !p.is_empty() => (h.to_string(), p.parse::<u16>().map_err(|_| unported())?),
                _ if p.is_empty() || p == ":" => (h.to_string(), default),
                _ => return Err(unported()),
            }
        } else {
            match hostport.rsplit_once(':') {
                Some((h, "")) => (h.to_string(), default),
                Some((h, p)) => (h.to_string(), p.parse::<u16>().map_err(|_| unported())?),
                None => (hostport.clone(), default),
            }
        };
        if host.is_empty() {
            return Err(unported());
        }
        let auth = match (&user, &pass) {
            (Some(u), Some(p)) if !u.is_empty() && !p.is_empty() => {
                Some(basic(&format!("{}:{}", crate::gateway::http::unquote(u), crate::gateway::http::unquote(p))))
            }
            _ => None,
        };
        let proxy = Proxy { tls: false, host, port, auth };
        if o.tls {
            return Ok(Route::Tunnel(proxy, Tunnel::Urllib));
        }
        match pscheme.as_deref() {
            None | Some("http") => Ok(Route::Forward(proxy)),
            _ => Err(unported()),
        }
    }
}

/// `urllib.request._parse_proxy`: scheme, user, password, host:port.
fn parse_proxy(proxy: &str) -> Option<(Option<String>, Option<String>, Option<String>, String)> {
    // _splittype: a scheme of letters, digits and +-. before the first ':'.
    let (scheme, rest) = match proxy.split_once(':') {
        Some((s, r)) if !s.is_empty() && s.chars().all(|c| c.is_ascii_alphanumeric() || "+-.".contains(c)) => {
            (Some(s.to_lowercase()), r)
        }
        _ => (None, proxy),
    };
    let (scheme, authority) = if !rest.starts_with('/') {
        (None, proxy.to_string())
    } else {
        let r = rest.strip_prefix("//")?;
        let end = match r.find('@') {
            Some(at) => r[at..].find('/').map(|i| i + at),
            None => r.find('/'),
        };
        (scheme, r[..end.unwrap_or(r.len())].to_string())
    };
    let (userinfo, hostport) = match authority.rsplit_once('@') {
        Some((u, h)) => (Some(u.to_string()), h.to_string()),
        None => (None, authority),
    };
    let (user, pass) = match userinfo {
        Some(u) => match u.split_once(':') {
            Some((a, b)) => (Some(a.to_string()), Some(b.to_string())),
            None => (Some(u), None),
        },
        None => (None, None),
    };
    Some((scheme, user, pass, hostport))
}

// ---------------------------------------------------------------------------
// The connector
// ---------------------------------------------------------------------------

pub trait Stream: Read + Write + Send {}
impl<T: Read + Write + Send> Stream for T {}

/// A socket whose every read and write waits no longer than what is left
/// of an optional time limit.
pub struct Sock {
    s: TcpStream,
    limit: Option<(Instant, Duration)>,
}

impl Sock {
    fn arm(&self) -> io::Result<()> {
        if let Some((started, limit)) = self.limit {
            match limit.checked_sub(started.elapsed()).filter(|d| !d.is_zero()) {
                Some(d) => {
                    self.s.set_read_timeout(Some(d))?;
                    self.s.set_write_timeout(Some(d))?;
                }
                None => return Err(io::Error::from(io::ErrorKind::TimedOut)),
            }
        }
        Ok(())
    }
}

impl Read for Sock {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        self.arm()?;
        self.s.read(buf)
    }
}

impl Write for Sock {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.arm()?;
        self.s.write(buf)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.s.flush()
    }
}

/// TLS over a stream; a peer that closes without close_notify ends the
/// stream there.
struct Tls(rustls::StreamOwned<rustls::ClientConnection, Box<dyn Stream>>);

impl Read for Tls {
    fn read(&mut self, b: &mut [u8]) -> io::Result<usize> {
        match self.0.read(b) {
            Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => Ok(0),
            r => r,
        }
    }
}

impl Write for Tls {
    fn write(&mut self, b: &[u8]) -> io::Result<usize> {
        self.0.write(b)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.0.flush()
    }
}

/// An open connection.
pub struct Conn {
    pub io: Box<dyn Stream>,
    /// The TCP socket under it, for time limits and peeking.
    pub sock: TcpStream,
    /// Set when the request goes to a proxy in absolute form: the
    /// `Proxy-Authorization` value to send, if any.
    pub forward: Option<Option<String>>,
}

/// Why a connection could not be opened.
#[derive(Debug)]
pub enum Fail {
    /// The TCP connection to the first hop failed.
    Connect(io::Error),
    /// The proxy answered the CONNECT with this status and reason.
    Status(u16, String),
    /// Anything else: TLS, or the stream failing mid-way.
    Io(io::Error),
}

impl std::fmt::Display for Fail {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Fail::Connect(e) | Fail::Io(e) => write!(f, "{e}"),
            Fail::Status(c, r) => write!(f, "{c} {r}"),
        }
    }
}

impl Fail {
    pub fn into_io(self) -> io::Error {
        match self {
            Fail::Connect(e) | Fail::Io(e) => e,
            Fail::Status(c, r) => io::Error::other(format!("{c} {r}")),
        }
    }
}

/// How to open a connection.
#[derive(Clone, Default)]
pub struct Dial {
    /// Bounds the connect and every read and write, from its start.
    pub limit: Option<(Instant, Duration)>,
    /// Bounds each TCP connect (when there is no overall limit).
    pub connect_timeout: Option<Duration>,
    /// Replaces the system's roots for the origin's TLS (tests only).
    pub roots: Option<Arc<rustls::RootCertStore>>,
}

/// A TLS client config with the ring provider and `roots`, or the
/// system's root store.
pub fn tls_config(roots: Option<Arc<rustls::RootCertStore>>) -> io::Result<Arc<rustls::ClientConfig>> {
    fn build(roots: Arc<rustls::RootCertStore>) -> Result<Arc<rustls::ClientConfig>, String> {
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        Ok(Arc::new(
            rustls::ClientConfig::builder_with_provider(provider)
                .with_safe_default_protocol_versions()
                .map_err(|e| e.to_string())?
                .with_root_certificates(roots)
                .with_no_client_auth(),
        ))
    }
    if let Some(r) = roots {
        return build(r).map_err(io::Error::other);
    }
    static C: OnceLock<Result<Arc<rustls::ClientConfig>, String>> = OnceLock::new();
    C.get_or_init(|| {
        let roots = crate::gate::llm::system_roots().map_err(|_| "no root certificates on this system".to_string())?;
        build(roots)
    })
    .clone()
    .map_err(io::Error::other)
}

fn tcp(host: &str, port: u16, d: &Dial) -> io::Result<TcpStream> {
    let mut last = io::Error::other("no address to connect to");
    for a in (host, port).to_socket_addrs()? {
        let left = match d.limit {
            Some((started, limit)) => {
                Some(limit.checked_sub(started.elapsed()).filter(|x| !x.is_zero()).ok_or(io::Error::from(io::ErrorKind::TimedOut))?)
            }
            None => d.connect_timeout,
        };
        let r = match left {
            Some(t) => TcpStream::connect_timeout(&a, t),
            None => TcpStream::connect(a),
        };
        match r {
            Ok(s) => return Ok(s),
            Err(e) if e.kind() == io::ErrorKind::TimedOut => return Err(e),
            Err(e) => last = e,
        }
    }
    Err(last)
}

fn wrap_tls(io: Box<dyn Stream>, host: &str, cfg: Arc<rustls::ClientConfig>) -> io::Result<Box<dyn Stream>> {
    let name = rustls::pki_types::ServerName::try_from(host.to_string()).map_err(|e| io::Error::other(e.to_string()))?;
    let c = rustls::ClientConnection::new(cfg, name).map_err(|e| io::Error::other(e.to_string()))?;
    Ok(Box::new(Tls(rustls::StreamOwned::new(c, io))))
}

/// The CONNECT request line and headers.
pub fn connect_request(o: &Origin, p: &Proxy, style: Tunnel) -> String {
    let host = o.host.to_ascii_lowercase();
    match style {
        Tunnel::Httpx => {
            let target = format!("{host}:{}", o.port);
            let mut r = format!("CONNECT {target} HTTP/1.1\r\nHost: {target}\r\nAccept: */*\r\n");
            if let Some(a) = &p.auth {
                r.push_str(&format!("Proxy-Authorization: {a}\r\n"));
            }
            r.push_str("\r\n");
            r
        }
        Tunnel::Urllib => {
            let wrapped = if host.contains(':') { format!("[{host}]") } else { host.clone() };
            let mut r = format!("CONNECT {wrapped}:{} HTTP/1.1\r\n", o.port);
            if let Some(a) = &p.auth {
                r.push_str(&format!("Proxy-Authorization: {a}\r\n"));
            }
            r.push_str(&format!("Host: {host}:{}\r\n\r\n", o.port));
            r
        }
    }
}

/// Reads the proxy's answer to a CONNECT up to its blank line, a byte at
/// a time so nothing of the tunnel is read, and skips a body it declares.
fn read_connect_answer(io: &mut dyn Stream) -> io::Result<(u16, String)> {
    let mut head = Vec::new();
    let mut b = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") && !head.ends_with(b"\n\n") {
        if io.read(&mut b)? == 0 {
            return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "the proxy closed the connection"));
        }
        head.push(b[0]);
        if head.len() > 1 << 16 {
            return Err(io::Error::other("the proxy's answer is too long"));
        }
    }
    let text = String::from_utf8_lossy(&head).into_owned();
    let first = text.lines().next().unwrap_or("");
    let mut parts = first.splitn(3, ' ');
    let proto = parts.next().unwrap_or("");
    let code: u16 = match parts.next().and_then(|c| c.parse().ok()) {
        Some(c) if proto.starts_with("HTTP/") => c,
        _ => return Err(io::Error::other("the proxy answered something that is not HTTP")),
    };
    Ok((code, parts.next().unwrap_or("").trim().to_string()))
}

/// Opens a connection to `o` by `route`.
pub fn open(route: &Route, o: &Origin, d: &Dial) -> Result<Conn, Fail> {
    let (first_host, first_port) = match route {
        Route::Direct => (o.host.as_str(), o.port),
        Route::Forward(p) | Route::Tunnel(p, _) => (p.host.as_str(), p.port),
    };
    let s = tcp(first_host, first_port, d).map_err(Fail::Connect)?;
    let _ = s.set_nodelay(true);
    let sock = s.try_clone().map_err(Fail::Io)?;
    let mut io: Box<dyn Stream> = Box::new(Sock { s, limit: d.limit });
    if let Route::Forward(p) | Route::Tunnel(p, _) = route {
        if p.tls {
            io = wrap_tls(io, &p.host, tls_config(None).map_err(Fail::Io)?).map_err(Fail::Io)?;
        }
    }
    let mut forward = None;
    match route {
        Route::Direct => {}
        Route::Forward(p) => forward = Some(p.auth.clone()),
        Route::Tunnel(p, style) => {
            io.write_all(connect_request(o, p, *style).as_bytes()).and_then(|_| io.flush()).map_err(Fail::Io)?;
            let (code, reason) = read_connect_answer(io.as_mut()).map_err(Fail::Io)?;
            let ok = match style {
                Tunnel::Httpx => (200..300).contains(&code),
                Tunnel::Urllib => code == 200,
            };
            if !ok {
                return Err(Fail::Status(code, reason));
            }
        }
    }
    if o.tls {
        // The server name as httpx sends it: the host lower-cased.
        io = wrap_tls(io, &o.host.to_ascii_lowercase(), tls_config(d.roots.clone()).map_err(Fail::Io)?).map_err(Fail::Io)?;
    }
    Ok(Conn { io, sock, forward })
}

/// The request target: the path for a direct or tunnelled request; the
/// absolute URL for one sent to a proxy in the clear.
pub fn request_target(c: &Conn, o: &Origin, path: &str) -> String {
    if c.forward.is_none() {
        return path.to_string();
    }
    let scheme = if o.tls { "https" } else { "http" };
    let host = o.host.to_ascii_lowercase();
    let host = if host.contains(':') { format!("[{host}]") } else { host };
    if Some(o.port) == default_port(scheme) {
        format!("{scheme}://{host}{path}")
    } else {
        format!("{scheme}://{host}:{}{path}", o.port)
    }
}

/// The `Proxy-Authorization` header line a forwarded request carries.
pub fn auth_line(c: &Conn) -> String {
    match &c.forward {
        Some(Some(a)) => format!("Proxy-Authorization: {a}\r\n"),
        _ => String::new(),
    }
}

// ---------------------------------------------------------------------------
// The process's own rules
// ---------------------------------------------------------------------------

/// The httpx rules from this process's environment, read once.
pub fn process_httpx() -> &'static Httpx {
    static R: OnceLock<Httpx> = OnceLock::new();
    R.get_or_init(|| Httpx::from_vars(&process_vars(), true))
}

/// The urllib rules from this process's environment, read once.
pub fn process_urllib() -> &'static Urllib {
    static R: OnceLock<Urllib> = OnceLock::new();
    R.get_or_init(|| Urllib::from_vars(&process_vars(), true))
}

// ---------------------------------------------------------------------------
// httpx's URL parser (httpx/_urlparse.py, httpx 0.28) for the URLs a proxy
// setting holds. The Go client's `netproxy/urlparse.go` is the reference.
// ---------------------------------------------------------------------------

/// Why a URL gives no parts: httpx raises `InvalidURL` (its words), or
/// this binary does not read the URL the way httpx does.
#[derive(Clone, Debug, PartialEq)]
enum UrlErr {
    Invalid(String),
    Unported(String),
}

/// `urlparse(url)` for the parts a proxy route reads; `port` is None
/// when absent or the scheme's default.
#[derive(Clone, Debug)]
struct HttpxUrl {
    scheme: String,
    userinfo: String,
    host: String,
    port: Option<u16>,
    path: String,
    query: Option<String>,
    fragment: Option<String>,
}

const UNRESERVED: &str = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~";
const SUB_DELIMS: &str = "!$&'()*+,;=";

fn safe_range(exclude: &[u8]) -> String {
    (0x20u8..0x7f).filter(|b| !exclude.contains(b)).map(|b| b as char).collect()
}

fn url_res() -> &'static [regex::Regex; 5] {
    static R: OnceLock<[regex::Regex; 5]> = OnceLock::new();
    R.get_or_init(|| {
        [
            regex::Regex::new(r"^(?:(?P<scheme>[a-zA-Z][a-zA-Z0-9+.-]*)?:)?(?://(?P<authority>[^/?#]*))?(?P<path>[^?#]*)(?:\?(?P<query>[^#]*))?(?:#(?P<fragment>(?s:.*)))?").expect("pattern"),
            regex::Regex::new(r"^(?:(?P<userinfo>(?s:.*))@)?(?P<host>\[(?s:.*)\]|[^:@]*):?(?P<port>(?s:.*))?").expect("pattern"),
            regex::Regex::new(r"^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$").expect("pattern"),
            regex::Regex::new(r"%[A-Fa-f0-9]{2}").expect("pattern"),
            regex::Regex::new(r"^ *[+-]?[0-9]+(?:_[0-9]+)* *$").expect("pattern"),
        ]
    })
}

impl HttpxUrl {
    /// The path, query and fragment as str(URL) writes them.
    fn rest(&self) -> String {
        let mut s = self.path.clone();
        if let Some(q) = &self.query {
            s.push('?');
            s.push_str(q);
        }
        if let Some(f) = &self.fragment {
            s.push('#');
            s.push_str(f);
        }
        s
    }
}

/// `httpx._urlparse.urlparse(s)`.
/// The digit zeros of Unicode 15.0 (Python 3.12's unicodedata): each
/// starts a run of ten Nd characters with the values 0 to 9.
const ND_ZEROS: &[u32] = &[0x30, 0x660, 0x6f0, 0x7c0, 0x966, 0x9e6, 0xa66, 0xae6, 0xb66, 0xbe6, 0xc66, 0xce6, 0xd66, 0xde6, 0xe50, 0xed0, 0xf20, 0x1040, 0x1090, 0x17e0, 0x1810, 0x1946, 0x19d0, 0x1a80, 0x1a90, 0x1b50, 0x1bb0, 0x1c40, 0x1c50, 0xa620, 0xa8d0, 0xa900, 0xa9d0, 0xa9f0, 0xaa50, 0xabf0, 0xff10, 0x104a0, 0x10d30, 0x11066, 0x110f0, 0x11136, 0x111d0, 0x112f0, 0x11450, 0x114d0, 0x11650, 0x116c0, 0x11730, 0x118e0, 0x11950, 0x11c50, 0x11d50, 0x11da0, 0x11f50, 0x16a60, 0x16ac0, 0x16b50, 0x1d7ce, 0x1d7d8, 0x1d7e2, 0x1d7ec, 0x1d7f6, 0x1e140, 0x1e2f0, 0x1e4f0, 0x1e950, 0x1fbf0];

/// `text` with each decimal digit of another script written as its ASCII
/// digit, as int() reads it.
fn ascii_digits(text: &str) -> String {
    text.chars()
        .map(|c| {
            let r = c as u32;
            match ND_ZEROS.iter().find(|&&z| r >= z && r < z + 10) {
                Some(&z) => char::from_digit(r - z, 10).unwrap_or(c),
                None => c,
            }
        })
        .collect()
}

fn parse_httpx(s: &str) -> Result<HttpxUrl, UrlErr> {
    if s.chars().count() > 65536 {
        return Err(UrlErr::Invalid("URL too long".into()));
    }
    if let Some((i, c)) = s.chars().enumerate().find(|&(_, c)| (c as u32) < 0x20 || c as u32 == 0x7f) {
        return Err(UrlErr::Invalid(format!(
            "Invalid non-printable ASCII character in URL, {} at position {i}.",
            crate::gate::py::text::repr(&c.to_string())
        )));
    }
    let [url_re, auth_re, ipv4_re, _, port_re] = url_res();
    let m = url_re.captures(s).expect("the URL pattern matches every text");
    let g = |n: &str| m.name(n).map(|x| x.as_str().to_string());
    let scheme_raw = g("scheme").unwrap_or_default();
    let authority = g("authority").unwrap_or_default();
    let mut path = g("path").unwrap_or_default();
    let (query, frag) = (g("query"), g("fragment"));
    let a = auth_re.captures(&authority).expect("the authority pattern matches every text");
    let ga = |n: &str| a.name(n).map(|x| x.as_str().to_string()).unwrap_or_default();
    let (userinfo, host, port_text) = (ga("userinfo"), ga("host"), ga("port"));
    let scheme = scheme_raw.to_lowercase();
    let userinfo = quote_url(&userinfo, &safe_range(b"\x20\x22\x23\x3c\x3e\x3f\x60\x7b\x7d\x2f\x3b\x3d\x40\x5b\x5c\x5d\x5e\x7c"));
    let host = encode_host(&host, ipv4_re)?;
    let mut port = None;
    if !port_text.is_empty() {
        // int() reads a decimal digit of any script (Unicode Nd) as its
        // value.
        // An error quotes the port as the URL wrote it.
        let digits = ascii_digits(&port_text);
        if !digits.is_ascii() {
            return Err(UrlErr::Unported(format!("the port {port_text:?}")));
        }
        if !port_re.is_match(&digits) {
            return Err(UrlErr::Invalid(format!("Invalid port: {}", crate::gate::py::text::repr(&port_text))));
        }
        let n: i64 = digits.trim().replace('_', "").parse().map_err(|_| UrlErr::Unported(format!("the port {port_text:?}")))?;
        if n <= 0 || n > 65535 {
            return Err(UrlErr::Unported(format!("the port {port_text:?}")));
        }
        // normalize_port looks the default up under the scheme as written.
        let def = match scheme_raw.as_str() {
            "ftp" => Some(21),
            "http" | "ws" => Some(80),
            "https" | "wss" => Some(443),
            _ => None,
        };
        if def != Some(n) {
            port = Some(n as u16);
        }
    }
    let has_scheme = !scheme.is_empty();
    let has_authority = !userinfo.is_empty() || !host.is_empty() || port.is_some();
    if has_authority && !path.is_empty() && !path.starts_with('/') {
        return Err(UrlErr::Invalid("For absolute URLs, path must be empty or begin with '/'".into()));
    }
    if !has_scheme && !has_authority {
        if path.starts_with("//") {
            return Err(UrlErr::Invalid("Relative URLs cannot have a path starting with '//'".into()));
        }
        if path.starts_with(':') {
            return Err(UrlErr::Invalid("Relative URLs cannot have a path starting with ':'".into()));
        }
    }
    if has_scheme || has_authority {
        path = normalize_path(&path);
    }
    Ok(HttpxUrl {
        scheme,
        userinfo,
        host,
        port,
        path: quote_url(&path, &safe_range(b"\x20\x22\x23\x3c\x3e\x3f\x60\x7b\x7d")),
        query: query.map(|q| quote_url(&q, &safe_range(b"\x20\x22\x23\x3c\x3e"))),
        fragment: frag.map(|f| quote_url(&f, &safe_range(b"\x20\x22\x3c\x3e\x60"))),
    })
}

/// `httpx._urlparse.encode_host`.
fn encode_host(host: &str, ipv4_re: &regex::Regex) -> Result<String, UrlErr> {
    let r = crate::gate::py::text::repr;
    if host.is_empty() {
        return Ok(String::new());
    }
    if ipv4_re.is_match(host) {
        for part in host.split('.') {
            let bad = part.len() > 3 || (part.len() > 1 && part.starts_with('0')) || part.parse::<u32>().map(|n| n > 255).unwrap_or(true);
            if bad {
                return Err(UrlErr::Invalid(format!("Invalid IPv4 address: {}", r(host))));
            }
        }
        return Ok(host.into());
    }
    if host.len() >= 2 && host.starts_with('[') && host.ends_with(']') {
        let inner = &host[1..host.len() - 1];
        if inner.contains('%') {
            return Err(UrlErr::Unported(format!("the IPv6 address {}", r(host))));
        }
        return match inner.parse::<std::net::Ipv6Addr>() {
            Ok(_) => Ok(inner.into()),
            Err(_) => Err(UrlErr::Invalid(format!("Invalid IPv6 address: {}", r(host)))),
        };
    }
    if host.is_ascii() {
        return Ok(quote_url(&host.to_ascii_lowercase(), &format!("{SUB_DELIMS}\"`{{}}%|\\")));
    }
    idna_encode(host)
}

/// `httpx._urlparse.normalize_path`.
fn normalize_path(path: &str) -> String {
    if !path.contains('.') {
        return path.into();
    }
    let comps: Vec<&str> = path.split('/').collect();
    if !comps.iter().any(|c| *c == "." || *c == "..") {
        return path.into();
    }
    let mut out: Vec<&str> = vec![];
    for c in comps {
        match c {
            "." => {}
            ".." => {
                if !out.is_empty() && out != [""] {
                    out.pop();
                }
            }
            _ => out.push(c),
        }
    }
    out.join("/")
}

fn percent_encoded(s: &str, safe: &str) -> String {
    let ok = |c: char| UNRESERVED.contains(c) || safe.contains(c);
    if s.chars().all(ok) {
        return s.into();
    }
    let mut b = String::new();
    for c in s.chars() {
        if ok(c) {
            b.push(c);
        } else {
            let mut buf = [0u8; 4];
            for byte in c.encode_utf8(&mut buf).bytes() {
                b.push_str(&format!("%{byte:02X}"));
            }
        }
    }
    b
}

/// `httpx._urlparse.quote`: percent-encoding that keeps the '%xx'
/// escapes already there.
fn quote_url(s: &str, safe: &str) -> String {
    let pct = &url_res()[3];
    let mut b = String::new();
    let mut cur = 0;
    for m in pct.find_iter(s) {
        if m.start() != cur {
            b.push_str(&percent_encoded(&s[cur..m.start()], safe));
        }
        b.push_str(m.as_str());
        cur = m.end();
    }
    if cur != s.len() {
        b.push_str(&percent_encoded(&s[cur..], safe));
    }
    b
}

/// `idna.encode(host.lower()).decode("ascii")`, or the InvalidURL httpx
/// raises for an IDNAError, for hosts whose letters this binary encodes:
/// ASCII letters, digits and hyphens, and the lower-case Latin-1 letters
/// (all PVALID).
fn idna_encode(host: &str) -> Result<String, UrlErr> {
    let bad = || UrlErr::Invalid(format!("Invalid IDNA hostname: {}", crate::gate::py::text::repr(host)));
    let s = crate::gate::py::text::lower(host);
    let mut labels: Vec<&str> = s.split('.').collect();
    if labels.len() > 1 && labels.last() == Some(&"") {
        labels.pop();
    }
    let mut out = vec![];
    for label in labels {
        if label.is_empty() {
            return Err(bad());
        }
        for c in label.chars() {
            if c.is_ascii() {
                if !(c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-') {
                    return Err(bad());
                }
            } else if !((0xdf..=0xff).contains(&(c as u32)) && c != '\u{f7}') {
                return Err(UrlErr::Unported(format!("the host {host:?}")));
            }
        }
        if label.starts_with("xn--") {
            return Err(UrlErr::Unported(format!("the host {host:?}")));
        }
        if label.starts_with('-') || label.ends_with('-') {
            return Err(bad());
        }
        let cs: Vec<char> = label.chars().collect();
        if cs.len() >= 4 && cs[2] == '-' && cs[3] == '-' {
            return Err(bad());
        }
        let enc = if label.is_ascii() { label.to_string() } else { format!("xn--{}", punycode(label)) };
        if enc.len() > 63 {
            return Err(bad());
        }
        out.push(enc);
    }
    let mut res = out.join(".");
    if s.ends_with('.') {
        res.push('.');
    }
    if res.len() > 253 + usize::from(res.ends_with('.')) {
        return Err(bad());
    }
    Ok(res)
}

/// RFC 3492's encoding of a label.
fn punycode(label: &str) -> String {
    const BASE: u32 = 36;
    const TMIN: u32 = 1;
    const TMAX: u32 = 26;
    const SKEW: u32 = 38;
    const DAMP: u32 = 700;
    let cps: Vec<u32> = label.chars().map(|c| c as u32).collect();
    let mut out: Vec<u8> = cps.iter().filter(|&&c| c < 0x80).map(|&c| c as u8).collect();
    let b = out.len() as u32;
    let mut h = b;
    if b > 0 {
        out.push(b'-');
    }
    let digit = |d: u32| if d < 26 { b'a' + d as u8 } else { b'0' + (d - 26) as u8 };
    let adapt = |mut delta: u32, num: u32, first: bool| {
        delta /= if first { DAMP } else { 2 };
        delta += delta / num;
        let mut k = 0;
        while delta > ((BASE - TMIN) * TMAX) / 2 {
            delta /= BASE - TMIN;
            k += BASE;
        }
        k + (BASE - TMIN + 1) * delta / (delta + SKEW)
    };
    let (mut n, mut delta, mut bias) = (128u32, 0u32, 72u32);
    while (h as usize) < cps.len() {
        let m = *cps.iter().filter(|&&c| c >= n).min().unwrap_or(&n);
        delta += (m - n) * (h + 1);
        n = m;
        for &c in &cps {
            if c < n {
                delta += 1;
            }
            if c == n {
                let mut q = delta;
                let mut k = BASE;
                loop {
                    let t = if k <= bias { TMIN } else if k >= bias + TMAX { TMAX } else { k - bias };
                    if q < t {
                        break;
                    }
                    out.push(digit(t + (q - t) % (BASE - t)));
                    q = (q - t) / (BASE - t);
                    k += BASE;
                }
                out.push(digit(q));
                bias = adapt(delta, h + 1, h == b);
                delta = 0;
                h += 1;
            }
        }
        delta += 1;
        n += 1;
    }
    String::from_utf8(out).unwrap_or_default()
}

#[cfg(test)]
mod tests {
    /// The answers httpx 0.28's urlparse gives (read off the oracle's httpx).
    #[test]
    fn parse_httpx_as_httpx() {
        let ok: &[(&str, &str, &str, &str, Option<u16>, &str)] = &[
            ("http://127.0.0.1:+8080", "http", "", "127.0.0.1", Some(8080), ""),
            ("http://127.0.0.1: 8080", "http", "", "127.0.0.1", Some(8080), ""),
            ("http://bücher.localhost:1", "http", "", "xn--bcher-kva.localhost", Some(1), ""),
            ("http://Bücher.Example/", "http", "", "xn--bcher-kva.example", None, "/"),
            ("all://*x%y", "all", "", "*x%y", None, ""),
            ("http://p q:1", "http", "", "p%20q", Some(1), ""),
            ("HTTP://h:80", "http", "", "h", Some(80), ""),
            ("http://h:80", "http", "", "h", None, ""),
            ("http://u:p@h/a/./b/../c?q#f", "http", "u:p", "h", None, "/a/c?q#f"),
            ("http://münchen-straße.de", "http", "", "xn--mnchen-strae-v9a90b.de", None, ""),
            ("http://ab--cd.de", "http", "", "ab--cd.de", None, ""),
        ];
        for (input, scheme, userinfo, host, port, rest) in ok {
            let u = parse_httpx(input).unwrap_or_else(|e| panic!("{input}: {e:?}"));
            assert_eq!((u.scheme.as_str(), u.userinfo.as_str(), u.host.as_str(), u.port, u.rest().as_str()), (*scheme, *userinfo, *host, *port, *rest), "{input}");
        }
        for (input, msg) in [
            ("http://127.0.0.1:x", "Invalid port: 'x'"),
            ("http://127.0.0.01:1", "Invalid IPv4 address: '127.0.0.01'"),
            ("http://[::1:1", "Invalid port: ':1:1'"),
            ("http://bü_cher:1", "Invalid IDNA hostname: 'bü_cher'"),
            ("all://*a:b", "Invalid port: 'b'"),
            ("all://*bücher.example", "Invalid IDNA hostname: '*bücher.example'"),
        ] {
            assert_eq!(parse_httpx(input).unwrap_err(), UrlErr::Invalid(msg.into()), "{input}");
        }
        // A port int() reads another way, or one no socket takes, is refused.
        // A digit of another script is its value, as int() reads it.
        assert_eq!(parse_httpx("http://h:\u{ff18}\u{ff10}\u{0668}\u{0660}").map(|u| u.port).ok(), Some(Some(8080)));
        // An error quotes the port as written.
        assert!(matches!(parse_httpx("http://h:\u{ff18}x"), Err(UrlErr::Invalid(m)) if m == "Invalid port: '\u{ff18}x'"));
        for input in ["http://h:99999", "http://h:0", "http://h:-1", "http://h:\u{3000}80"] {
            assert!(matches!(parse_httpx(input), Err(UrlErr::Unported(_))), "{input}");
        }
    }

    /// A NO_PROXY entry httpx cannot parse stops the client; one with a
    /// '%' matches no host.
    #[test]
    fn no_proxy_entry_invalid_url() {
        let h = Httpx::from_vars(&v(&[("HTTP_PROXY", "http://127.0.0.1:1"), ("NO_PROXY", "a:b")]), true);
        assert_eq!(h.refusal(), Some(&Refusal::Invalid("Invalid port: 'b'".into())));
        let h = Httpx::from_vars(&v(&[("HTTP_PROXY", "http://127.0.0.1:1"), ("NO_PROXY", "x%y")]), true);
        assert_eq!(h.refusal(), None);
    }

    use super::*;

    fn v(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
        pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect()
    }

    fn o(url_tls: bool, host: &str, port: u16) -> Origin {
        Origin { tls: url_tls, host: host.into(), port, explicit_port: false }
    }

    fn hx(pairs: &[(&str, &str)]) -> Httpx {
        Httpx::from_vars(&v(pairs), true)
    }

    fn px(host: &str, port: u16) -> Proxy {
        Proxy { tls: false, host: host.into(), port, auth: None }
    }

    #[test]
    fn getproxies_prefers_lower_case_and_drops_http_under_cgi() {
        let p = getproxies(&v(&[("http_proxy", "a"), ("HTTP_PROXY", "b"), ("HTTPS_PROXY", "c"), ("no_proxy", "")]), true).unwrap();
        assert_eq!(get(&p, "http"), Some("a"));
        assert_eq!(get(&p, "https"), Some("c"));
        assert_eq!(get(&p, "no"), None);
        let p = getproxies(&v(&[("HTTP_PROXY", "b"), ("REQUEST_METHOD", "GET")]), true).unwrap();
        assert_eq!(get(&p, "http"), None);
        let p = getproxies(&v(&[("HTTP_PROXY", "b"), ("http_proxy", "")]), true).unwrap();
        assert_eq!(get(&p, "http"), None);
        assert!(getproxies(&v(&[("HTTP_PROXY", "b"), ("Http_Proxy", "c")]), false).is_err());
        assert!(getproxies(&v(&[("HTTP_PROXY", "b"), ("Http_Proxy", "b")]), false).is_ok());
    }

    #[test]
    fn httpx_picks_the_scheme_variable_then_all_proxy() {
        let r = hx(&[("HTTP_PROXY", "127.0.0.1:3128"), ("ALL_PROXY", "http://127.0.0.1:9")]);
        assert_eq!(r.route(&o(false, "a.localhost", 80)), Ok(Route::Forward(px("127.0.0.1", 3128))));
        assert_eq!(r.route(&o(true, "a.localhost", 443)), Ok(Route::Tunnel(px("127.0.0.1", 9), Tunnel::Httpx)));
        // No loopback bypass.
        assert_eq!(r.route(&o(false, "127.0.0.1", 8)), Ok(Route::Forward(px("127.0.0.1", 3128))));
        assert_eq!(hx(&[]).route(&o(true, "x", 443)), Ok(Route::Direct));
    }

    #[test]
    fn httpx_no_proxy_forms() {
        let base = [("ALL_PROXY", "http://p:1")];
        let with = |no: &str| {
            let mut e = base.to_vec();
            e.push(("NO_PROXY", no));
            hx(&e)
        };
        let direct = |r: &Httpx, tls: bool, h: &str, port: u16| r.route(&o(tls, h, port)) == Ok(Route::Direct);
        let r = with("example.com");
        assert!(direct(&r, true, "example.com", 443) && direct(&r, true, "www.example.com", 443));
        assert!(!direct(&r, true, "wwwexample.com", 443));
        let r = with(".example.com");
        assert!(!direct(&r, true, "example.com", 443) && direct(&r, true, "www.example.com", 443));
        let r = with("*");
        assert!(direct(&r, false, "anything", 80));
        let r = with("a.b, *, c");
        assert!(direct(&r, false, "anything", 80));
        let r = with("127.0.0.1");
        assert!(direct(&r, false, "127.0.0.1", 9) && !direct(&r, false, "127.0.0.2", 9));
        let r = with("192.168.0.0/16");
        assert!(direct(&r, false, "192.168.0.0", 9) && !direct(&r, false, "192.168.1.1", 9));
        let r = with("::1");
        assert!(direct(&r, false, "::1", 9));
        let r = with("LOCALHOST");
        assert!(direct(&r, false, "localhost", 9) && !direct(&r, false, "a.localhost", 9));
        // A port counts; a default port is dropped from the URL first.
        let r = with("example.com:8443");
        assert!(direct(&r, true, "example.com", 8443) && !direct(&r, true, "example.com", 443));
        let r = with("example.com:443");
        assert!(!direct(&r, true, "example.com", 443));
        let r = with("http://example.com");
        assert!(direct(&r, false, "example.com", 80) && !direct(&r, true, "example.com", 443));
        // A '%' stays in the pattern's host, as httpx keeps it: no host
        // matches it (PX-9).
        let r = with("a%2eb");
        assert!(r.refusal().is_none() && !direct(&r, false, "a.b", 80));
    }

    #[test]
    fn httpx_refuses_socks_and_unknown_schemes_as_it_does() {
        let r = hx(&[("HTTPS_PROXY", "socks5://127.0.0.1:9")]);
        assert_eq!(r.route(&o(false, "x", 80)), Err(Refusal::Fails(SOCKS_TEXT.into())));
        let r = hx(&[("HTTP_PROXY", "ftp://127.0.0.1:9"), ("HTTPS_PROXY", "socks5://h:1")]);
        assert_eq!(r.route(&o(false, "x", 80)), Err(Refusal::Fails("Unknown scheme for proxy URL URL('ftp://127.0.0.1:9')".into())));
        // NO_PROXY=* drops them before they are built.
        let r = hx(&[("HTTPS_PROXY", "socks5://h:1"), ("NO_PROXY", "*")]);
        assert_eq!(r.route(&o(false, "x", 80)), Ok(Route::Direct));
    }

    #[test]
    fn httpx_credentials_are_unquoted() {
        let r = hx(&[("HTTP_PROXY", "http://us%65r:p%40ss@127.0.0.1:3")]);
        let Ok(Route::Forward(p)) = r.route(&o(false, "x", 80)) else { panic!() };
        assert_eq!(p.auth.as_deref(), Some("Basic dXNlcjpwQHNz"));
        let r = hx(&[("HTTP_PROXY", "http://u:@127.0.0.1:3")]);
        let Ok(Route::Forward(p)) = r.route(&o(false, "x", 80)) else { panic!() };
        assert_eq!(p.auth.as_deref(), Some("Basic dTo="));
        let r = hx(&[("HTTPS_PROXY", "https://proxy.localhost")]);
        let Ok(Route::Tunnel(p, _)) = r.route(&o(true, "x", 443)) else { panic!() };
        assert!(p.tls && p.port == 443);
    }

    #[test]
    fn connect_lines_as_each_client_writes_them() {
        let p = Proxy { tls: false, host: "p".into(), port: 1, auth: Some("Basic dTpw".into()) };
        let v6 = o(true, "::1", 8443);
        assert_eq!(
            connect_request(&v6, &p, Tunnel::Httpx),
            "CONNECT ::1:8443 HTTP/1.1\r\nHost: ::1:8443\r\nAccept: */*\r\nProxy-Authorization: Basic dTpw\r\n\r\n"
        );
        assert_eq!(
            connect_request(&v6, &p, Tunnel::Urllib),
            "CONNECT [::1]:8443 HTTP/1.1\r\nProxy-Authorization: Basic dTpw\r\nHost: ::1:8443\r\n\r\n"
        );
    }

    #[test]
    fn urllib_rules() {
        let u = |pairs: &[(&str, &str)]| Urllib::from_vars(&v(pairs), true);
        let r = u(&[("ALL_PROXY", "http://p:1")]);
        assert_eq!(r.route(&o(true, "x", 443)), Ok(Route::Direct));
        let r = u(&[("https_proxy", "socks5://u:p@p:3")]);
        assert_eq!(
            r.route(&o(true, "hf.co", 443)),
            Ok(Route::Tunnel(Proxy { tls: false, host: "p".into(), port: 3, auth: Some("Basic dTpw".into()) }, Tunnel::Urllib))
        );
        let r = u(&[("https_proxy", "http://u@p")]);
        assert_eq!(r.route(&o(true, "hf.co", 443)), Ok(Route::Tunnel(px("p", 443), Tunnel::Urllib)));
        let r = u(&[("https_proxy", "p:3"), ("no_proxy", ".hf.co")]);
        assert_eq!(r.route(&o(true, "hf.co", 443)), Ok(Route::Direct));
        assert_eq!(r.route(&o(true, "cdn.hf.co", 443)), Ok(Route::Direct));
        assert_eq!(r.route(&o(true, "xhf.co", 443)), Ok(Route::Tunnel(px("p", 3), Tunnel::Urllib)));
        let r = u(&[("https_proxy", "p:3"), ("no_proxy", "a, *")]);
        assert_eq!(r.route(&o(true, "hf.co", 443)), Ok(Route::Tunnel(px("p", 3), Tunnel::Urllib)));
        let r = u(&[("https_proxy", "p:3"), ("no_proxy", "*")]);
        assert_eq!(r.route(&o(true, "hf.co", 443)), Ok(Route::Direct));
        let r = u(&[("http_proxy", "http://p:3"), ("no_proxy", "127.0.0.1:9")]);
        assert_eq!(r.route(&Origin { tls: false, host: "127.0.0.1".into(), port: 9, explicit_port: true }), Ok(Route::Direct));
        assert_eq!(r.route(&Origin { tls: false, host: "127.0.0.1".into(), port: 8, explicit_port: true }), Ok(Route::Forward(px("p", 3))));
    }

    #[test]
    fn base64_as_python_writes_it() {
        assert_eq!(b64(b""), "");
        assert_eq!(b64(b"u:"), "dTo=");
        assert_eq!(b64(b"user:p@ss"), "dXNlcjpwQHNz");
        assert_eq!(b64(b"ab"), "YWI=");
    }
}
