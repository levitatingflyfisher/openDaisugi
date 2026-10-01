//! floor.machines: a read-only view of the boxes on the tailnet. Which are
//! up, what each serves, how fast each answers, and which profiles point at
//! it. It runs `tailscale status --json` once per call, reads the
//! operator's agent profiles for their names and base URLs only (never a
//! key), and opens one TCP connection to each profile's host and port. The
//! Go server says the same words; the cases compare them.

use std::io::Read;
use std::net::{IpAddr, TcpStream, ToSocketAddrs};
use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};

/// What a pane or a plugin gets for floor.machines.
pub const MACHINES_REFUSAL: &str = "only the operator reads the machines.";
const ABSENT_LINE: &str =
    "Tailscale is not on this box. Install Tailscale, then open Machines again.";
const OFF_LINE: &str =
    "Tailscale is not running on this box. Run tailscale up, then open Machines again.";
const PROFILES_LINE: &str =
    "The agent profiles are not known. Check that daisugi runs, then open Machines again.";

const STATUS_TIMEOUT: Duration = Duration::from_secs(3);
const BUNDLE_TIMEOUT: Duration = Duration::from_secs(3);
const DIAL_TIMEOUT: Duration = Duration::from_secs(1);
/// The most bytes a command may write.
const READ_CAP: u64 = 1 << 20;

/// One node of `tailscale status --json`: this box (Self) or a peer. The
/// names are tailscale's, which have no json tags.
#[derive(Deserialize, Default, Clone)]
#[serde(default, rename_all = "PascalCase")]
struct TsPeer {
    host_name: String,
    #[serde(rename = "DNSName")]
    dns_name: String,
    #[serde(rename = "OS")]
    os: String,
    online: bool,
    last_seen: String,
    #[serde(rename = "TailscaleIPs")]
    tailscale_ips: Vec<String>,
}

#[derive(Deserialize, Default)]
#[serde(default, rename_all = "PascalCase")]
struct TsStatus {
    backend_state: String,
    #[serde(rename = "Self")]
    self_: Option<TsPeer>,
    peer: std::collections::BTreeMap<String, TsPeer>,
}

/// A profile, or a voice server, that points at a machine.
struct Probe {
    name: String,
    address: String,
    dial: String,
    answers_ms: Option<u64>,
    key_set: Option<bool>,
}

struct Row {
    name: String,
    online: bool,
    os: String,
    is_self: bool,
    last_seen: String,
    names: Vec<String>,
    ips: Vec<String>,
    profiles: Vec<Probe>,
    voice: Option<Probe>,
    /// The speech server of [voice] speak_url, when it is here.
    speak: Option<Probe>,
}

/// The MagicDNS short name of a node: the first label of its DNS name,
/// else its host name, in lower case.
fn short_name(p: &TsPeer) -> String {
    let mut dns = p.dns_name.trim().to_lowercase();
    if let Some(d) = dns.strip_suffix('.') {
        dns = d.to_string();
    }
    if let Some(i) = dns.find('.') {
        if i > 0 {
            dns.truncate(i);
        }
    }
    if !dns.is_empty() {
        return dns;
    }
    p.host_name.trim().to_lowercase()
}

impl Row {
    fn new(p: &TsPeer, is_self: bool) -> Row {
        let name = short_name(p);
        let mut names = vec![name.clone()];
        let full = p.dns_name.trim().to_lowercase();
        let full = full.strip_suffix('.').unwrap_or(&full).to_string();
        if !full.is_empty() {
            names.push(full);
        }
        let ls = p.last_seen.trim();
        Row {
            name,
            online: p.online,
            os: p.os.clone(),
            is_self,
            last_seen: if ls.is_empty() || ls.starts_with("0001-01-01") {
                String::new()
            } else {
                ls.to_string()
            },
            names,
            ips: p.tailscale_ips.clone(),
            profiles: vec![],
            voice: None,
            speak: None,
        }
    }

    /// Whether host (lower case, no brackets) names the machine: its short
    /// or full MagicDNS name, or one of its tailnet addresses.
    fn has(&self, host: &str) -> bool {
        self.names.iter().any(|n| n == host)
            || self.ips.iter().any(|i| i.eq_ignore_ascii_case(host))
    }

    /// The host to connect to for a URL that names host: the machine's
    /// first IPv4 tailnet address for a MagicDNS name, else host as written.
    fn dial_host(&self, host: &str) -> String {
        if self.names.iter().any(|n| n == host) {
            for ip in &self.ips {
                if ip.parse::<std::net::Ipv4Addr>().is_ok() {
                    return ip.clone();
                }
            }
        }
        host.to_string()
    }
}

fn loopback_host(host: &str) -> bool {
    host == "localhost" || host.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback())
}

/// The lower-case host, and the port (the scheme's default when none) of a
/// base URL; None for a URL with no host or no usable port.
fn host_port(raw: &str) -> Option<(String, String)> {
    if raw.chars().any(|c| (c as u32) < 0x20 || c as u32 == 0x7f) {
        return None;
    }
    let colon = raw.find(':')?;
    let scheme = raw[..colon].to_lowercase();
    let rest = raw[colon + 1..].split('#').next().unwrap_or("");
    let rest = rest.strip_prefix("//")?;
    let end = rest.find(['/', '?']).unwrap_or(rest.len());
    let mut auth = &rest[..end];
    if let Some(at) = auth.rfind('@') {
        auth = &auth[at + 1..];
    }
    let (host, port) = if let Some(stripped) = auth.strip_prefix('[') {
        let close = stripped.find(']')?;
        let after = &stripped[close + 1..];
        let port = match after.strip_prefix(':') {
            Some(p) => p,
            None if after.is_empty() => "",
            None => return None,
        };
        (stripped[..close].to_string(), port.to_string())
    } else {
        match auth.rfind(':') {
            Some(i) => (auth[..i].to_string(), auth[i + 1..].to_string()),
            None => (auth.to_string(), String::new()),
        }
    };
    if host.is_empty() || !port.chars().all(|c| c.is_ascii_digit()) {
        return None;
    }
    let port = if port.is_empty() {
        match scheme.as_str() {
            "http" => "80".to_string(),
            "https" => "443".to_string(),
            _ => return None,
        }
    } else {
        port
    };
    Some((host.to_lowercase(), port))
}

fn join_host_port(host: &str, port: &str) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

/// The row a base URL names and a probe for its host and port. A loopback
/// host names this box when no machine claims the address.
fn locate(rows: &[Row], raw: &str) -> Option<(usize, Probe)> {
    let (host, port) = host_port(raw)?;
    for (i, r) in rows.iter().enumerate() {
        if r.has(&host) {
            return Some((
                i,
                Probe {
                    name: String::new(),
                    address: format!("{host}:{port}"),
                    dial: join_host_port(&r.dial_host(&host), &port),
                    answers_ms: None,
                    key_set: None,
                },
            ));
        }
    }
    if loopback_host(&host) && rows.first().is_some_and(|r| r.is_self) {
        return Some((
            0,
            Probe {
                name: String::new(),
                address: format!("{host}:{port}"),
                dial: join_host_port(&host, &port),
                answers_ms: None,
                key_set: None,
            },
        ));
    }
    None
}

/// Runs bin with args (in dir, when given), waits at most limit, and
/// returns what it wrote to stdout, at most 1 MiB. The command runs in a
/// process group that dies with the limit. Err is true when the program
/// was not found.
fn run(bin: &str, args: &[&str], dir: Option<&str>, limit: Duration) -> Result<Vec<u8>, bool> {
    let mut cmd = Command::new(bin);
    cmd.args(args)
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .stdout(Stdio::piped());
    if let Some(d) = dir {
        cmd.current_dir(d);
    }
    // SAFETY: setpgid is async-signal-safe.
    unsafe {
        cmd.pre_exec(|| {
            libc::setpgid(0, 0);
            Ok(())
        });
    }
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return Err(e.kind() == std::io::ErrorKind::NotFound),
    };
    let (tx, rx) = mpsc::channel::<Vec<u8>>();
    let mut source = child.stdout.take().expect("stdout is piped");
    thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = (&mut source).take(READ_CAP).read_to_end(&mut buf);
        let _ = tx.send(buf);
    });
    let start = Instant::now();
    let ok = loop {
        match child.try_wait() {
            Ok(Some(st)) => break st.success(),
            Ok(None) if start.elapsed() < limit => thread::sleep(Duration::from_millis(5)),
            _ => {
                // SAFETY: killing the group we made.
                unsafe { libc::kill(-(child.id() as i32), libc::SIGKILL) };
                let _ = child.kill();
                let _ = child.wait();
                break false;
            }
        }
    };
    let out = rx
        .recv_timeout(Duration::from_millis(200))
        .unwrap_or_default();
    if ok {
        Ok(out)
    } else {
        Err(false)
    }
}

/// "ok", "absent" or "off", and the status. Absent: no tailscale on PATH.
/// Off: it does not answer, or is not Running.
fn read_tailnet() -> (&'static str, TsStatus) {
    match run("tailscale", &["status", "--json"], None, STATUS_TIMEOUT) {
        Err(true) => ("absent", TsStatus::default()),
        Err(false) => ("off", TsStatus::default()),
        Ok(out) => match serde_json::from_slice::<TsStatus>(&out) {
            Ok(st) if st.backend_state == "Running" && st.self_.is_some() => ("ok", st),
            _ => ("off", TsStatus::default()),
        },
    }
}

/// The operator's agent profiles from `daisugi agent-profiles list --json`,
/// run in the foreman's neutral compactor directory as Go does: name, base
/// URL and whether the key is set, never a key. A profile with no base URL
/// is left out.
fn profile_rows() -> Option<Vec<(String, String, bool)>> {
    let bin = std::env::var("COPPICE_DAISUGI_BIN")
        .ok()
        .filter(|b| !b.is_empty())
        .unwrap_or_else(|| "daisugi".to_string());
    let dir = format!("{}/compactor", Server::foreman_dir());
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&dir)
            .ok()?;
    }
    let out = run(
        &bin,
        &["agent-profiles", "list", "--json"],
        Some(&dir),
        BUNDLE_TIMEOUT,
    )
    .ok()?;
    #[derive(Deserialize)]
    struct Row {
        name: String,
        base_url: Option<String>,
        key_set: bool,
    }
    let mut rows: Vec<Row> = serde_json::from_slice(&out).ok()?;
    rows.sort_by(|a, b| a.name.cmp(&b.name));
    Some(
        rows.into_iter()
            .filter_map(|r| {
                r.base_url
                    .filter(|u| !u.is_empty())
                    .map(|u| (r.name, u, r.key_set))
            })
            .collect(),
    )
}

fn probe_json(p: &Probe, named: bool) -> Value {
    let mut v = vec![];
    if named {
        v.push(("name", json!(p.name)));
    }
    v.push(("address", json!(p.address)));
    v.push(("answers", json!(p.answers_ms.is_some())));
    v.push(("answers_ms", json!(p.answers_ms)));
    if let Some(k) = p.key_set {
        v.push(("key_set", json!(k)));
    }
    Value::Object(v.into_iter().map(|(k, x)| (k.to_string(), x)).collect())
}

/// The time of a TCP connect to addr, or None. One total deadline covers
/// the name lookup and every address it gives (localhost is ::1 and
/// 127.0.0.1), so a server on either answers, as in Go.
fn dial(addr: &str) -> Option<u64> {
    let start = Instant::now();
    let deadline = start + DIAL_TIMEOUT;
    let (tx, rx) = mpsc::channel();
    let a = addr.to_string();
    thread::spawn(move || {
        let _ = tx.send(a.to_socket_addrs().map(|i| i.collect::<Vec<_>>()));
    });
    let addrs = rx.recv_timeout(DIAL_TIMEOUT).ok()?.ok()?;
    for sa in addrs {
        let left = deadline.checked_duration_since(Instant::now())?;
        if TcpStream::connect_timeout(&sa, left).is_ok() {
            return Some(start.elapsed().as_millis() as u64);
        }
    }
    None
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "floor.machines",
        Arc::new(|s, c, r| s.handle_floor_machines(c, r)),
    );
}

impl Server {
    /// floor.machines. See the module comment.
    fn handle_floor_machines(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let (state, st) = read_tailnet();
        // pools: the entries of the worker pools, with the state of each, so
        // a machine that does not answer shows here as cooling (FM-R-94).
        let pools: Vec<Value> = self
            .pool_entries_view()
            .iter()
            .filter_map(|e| serde_json::from_str::<Value>(&serde_json::to_string(e).ok()?).ok())
            .collect();
        let line = match state {
            "absent" => Some(ABSENT_LINE),
            "off" => Some(OFF_LINE),
            _ => None,
        };
        if let Some(line) = line {
            let mut res = vec![
                ("tailscale", json!(state)),
                ("machines", json!([])),
                ("line", json!(line)),
            ];
            if !pools.is_empty() {
                res.push(("pools", Value::Array(pools)));
            }
            return proto::ok_resp(&r.id, gojson::map(res));
        }
        let mut rows: Vec<Row> = vec![];
        if let Some(me) = &st.self_ {
            rows.push(Row::new(me, true));
        }
        let mut peers: Vec<Row> = st.peer.values().map(|p| Row::new(p, false)).collect();
        peers.sort_by(|a, b| {
            a.name
                .cmp(&b.name)
                .then_with(|| a.ips.join(",").cmp(&b.ips.join(",")))
        });
        rows.extend(peers);
        let mut profiles_line = false;
        match profile_rows() {
            Some(list) => {
                for (name, url, key_set) in list {
                    if let Some((i, mut p)) = locate(&rows, &url) {
                        p.name = name;
                        p.key_set = Some(key_set);
                        rows[i].profiles.push(p);
                    }
                }
            }
            None => profiles_line = true,
        }
        let voice = self.cfg.voice.url.trim().to_string();
        if !voice.is_empty() {
            if let Some((i, p)) = locate(&rows, &voice) {
                rows[i].voice = Some(p);
            }
        }
        let speak = self.cfg.voice.speak_url.trim().to_string();
        if !speak.is_empty() {
            if let Some((i, p)) = locate(&rows, &speak) {
                rows[i].speak = Some(p);
            }
        }
        // One connection per probe, all at once.
        let mut jobs = vec![];
        for (i, row) in rows.iter().enumerate() {
            for (j, p) in row.profiles.iter().enumerate() {
                jobs.push((i, Some(j), p.dial.clone()));
            }
            if let Some(p) = &row.voice {
                jobs.push((i, None, p.dial.clone()));
            }
            if let Some(p) = &row.speak {
                jobs.push((i, Some(usize::MAX), p.dial.clone()));
            }
        }
        let handles: Vec<_> = jobs
            .into_iter()
            .map(|(i, j, addr)| thread::spawn(move || (i, j, dial(&addr))))
            .collect();
        for h in handles {
            if let Ok((i, j, ms)) = h.join() {
                match j {
                    Some(usize::MAX) => {
                        if let Some(v) = rows[i].speak.as_mut() {
                            v.answers_ms = ms;
                        }
                    }
                    Some(j) => rows[i].profiles[j].answers_ms = ms,
                    None => {
                        if let Some(v) = rows[i].voice.as_mut() {
                            v.answers_ms = ms;
                        }
                    }
                }
            }
        }
        let machines: Vec<Value> = rows
            .iter()
            .map(|row| {
                let mut m = vec![
                    ("name", json!(row.name)),
                    ("online", json!(row.online)),
                    ("os", json!(row.os)),
                    ("self", json!(row.is_self)),
                ];
                if !row.last_seen.is_empty() {
                    m.push(("last_seen", json!(row.last_seen)));
                }
                m.push((
                    "profiles",
                    Value::Array(row.profiles.iter().map(|p| probe_json(p, true)).collect()),
                ));
                if let Some(v) = &row.voice {
                    m.push(("voice", probe_json(v, false)));
                }
                if let Some(v) = &row.speak {
                    m.push(("speak", probe_json(v, false)));
                }
                Value::Object(m.into_iter().map(|(k, x)| (k.to_string(), x)).collect())
            })
            .collect();
        let mut res = vec![
            ("tailscale", json!("ok")),
            ("machines", Value::Array(machines)),
        ];
        if profiles_line {
            res.push(("profiles_line", json!(PROFILES_LINE)));
        }
        if !pools.is_empty() {
            res.push(("pools", Value::Array(pools)));
        }
        proto::ok_resp(&r.id, gojson::map(res))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn peer(dns: &str, ips: &[&str]) -> TsPeer {
        TsPeer {
            dns_name: dns.into(),
            tailscale_ips: ips.iter().map(|s| s.to_string()).collect(),
            ..Default::default()
        }
    }

    #[test]
    fn a_base_url_gives_host_and_port() {
        assert_eq!(
            host_port("http://GPU.tail1.ts.net:8080/v1"),
            Some(("gpu.tail1.ts.net".into(), "8080".into()))
        );
        assert_eq!(
            host_port("https://a.example"),
            Some(("a.example".into(), "443".into()))
        );
        assert_eq!(
            host_port("http://[::1]:9/x"),
            Some(("::1".into(), "9".into()))
        );
        assert_eq!(host_port("ftp://a.example"), None);
        assert_eq!(host_port("no url"), None);
    }

    #[test]
    fn a_profile_goes_to_the_machine_its_name_or_address_names() {
        let rows = vec![
            Row::new(&peer("box.tail1.ts.net.", &["100.64.0.1"]), true),
            Row::new(
                &peer("gpu.tail1.ts.net.", &["100.64.0.2", "fd7a::2"]),
                false,
            ),
        ];
        assert_eq!(locate(&rows, "http://gpu:8000/v1").unwrap().0, 1);
        assert_eq!(
            locate(&rows, "http://gpu.tail1.ts.net:8000")
                .unwrap()
                .1
                .dial,
            "100.64.0.2:8000"
        );
        assert_eq!(locate(&rows, "http://100.64.0.2:1").unwrap().0, 1);
        assert_eq!(locate(&rows, "http://localhost:1").unwrap().0, 0);
        assert!(locate(&rows, "http://api.example.com:1").is_none());
    }
}
