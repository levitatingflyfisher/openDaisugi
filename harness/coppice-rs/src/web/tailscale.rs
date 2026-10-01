//! What coppice web serve --tls tailscale does on its own, as Go's
//! internal/web/tailscale.go does it, with the same words: it reads this
//! box's MagicDNS name and tailnet addresses from tailscale status --json,
//! gets a certificate with tailscale cert when there is none or it has
//! under 30 days left, and listens on the tailnet addresses and loopback.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::SystemTime;

use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls::sign::CertifiedKey;

/// How much time a certificate must have left to be kept, in seconds.
pub const RENEW_WINDOW_SECS: i64 = 30 * 24 * 3600;

/// How often a running phone server checks its certificate again.
pub const CERT_CHECK_EVERY: std::time::Duration = std::time::Duration::from_secs(24 * 3600);

/// The one line for a box with no tailscale on PATH.
pub const NO_TAILSCALE: &str = "tailscale is not on PATH. Install Tailscale on this box, then run coppice web serve --tls tailscale again.";

const AGAIN: &str = "then run coppice web serve --tls tailscale again.";

/// This box as tailscale names it.
#[derive(Clone, Debug, Default)]
pub struct Tailscale {
    pub name: String,
    pub ips: Vec<String>,
}

/// The tailscale binary on PATH, as Go's exec.LookPath finds it.
pub fn look_path() -> Option<PathBuf> {
    look_path_in(&std::env::var_os("PATH")?)
}

/// look_path over the given PATH value.
fn look_path_in(path: &std::ffi::OsStr) -> Option<PathBuf> {
    use std::os::unix::fs::PermissionsExt;
    for dir in std::env::split_paths(path) {
        // Go's exec.LookPath refuses a result relative to the current
        // directory, so an empty or relative PATH entry is skipped.
        if !dir.is_absolute() {
            continue;
        }
        let p = dir.join("tailscale");
        if let Ok(m) = std::fs::metadata(&p) {
            if m.is_file() && m.permissions().mode() & 0o111 != 0 {
                return Some(p);
            }
        }
    }
    None
}

/// ": " and the last line of a command's error output that is not blank,
/// or "" when there is none.
fn last_line(s: &str) -> String {
    let t = s.trim();
    let last = t.rsplit('\n').next().unwrap_or("").trim();
    if last.is_empty() {
        String::new()
    } else {
        format!(": {last}")
    }
}

/// The strings of a JSON array, or None when it is not one of strings.
fn strings(v: &serde_json::Value) -> Option<Vec<String>> {
    match v {
        serde_json::Value::Null => Some(Vec::new()),
        serde_json::Value::Array(a) => a.iter().map(|x| x.as_str().map(String::from)).collect(),
        _ => None,
    }
}

/// Runs tailscale status --json. Each error is one line that names the fix.
pub fn read_tailscale() -> Result<Tailscale, String> {
    let Some(bin) = look_path() else {
        return Err(NO_TAILSCALE.into());
    };
    let out = std::process::Command::new(bin)
        .args(["status", "--json"])
        .stdin(std::process::Stdio::null())
        .output();
    let out = match out {
        Ok(o) if o.status.success() => o,
        Ok(o) => {
            return Err(format!(
                "tailscale status --json failed{}. Check that tailscaled runs, {AGAIN}",
                last_line(&String::from_utf8_lossy(&o.stderr))
            ))
        }
        Err(_) => {
            return Err(format!(
                "tailscale status --json failed. Check that tailscaled runs, {AGAIN}"
            ))
        }
    };
    let unreadable = || {
        format!(
            "tailscale status --json printed what coppice cannot read. Update Tailscale, {AGAIN}"
        )
    };
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).map_err(|_| unreadable())?;
    let state = match v.get("BackendState") {
        None | Some(serde_json::Value::Null) => String::new(),
        Some(serde_json::Value::String(s)) => s.clone(),
        Some(_) => return Err(unreadable()),
    };
    let domains =
        strings(v.get("CertDomains").unwrap_or(&serde_json::Value::Null)).ok_or_else(unreadable)?;
    let self_ = v.get("Self").unwrap_or(&serde_json::Value::Null);
    let (dns, ips) = match self_ {
        serde_json::Value::Null => (String::new(), Vec::new()),
        serde_json::Value::Object(o) => {
            let dns = match o.get("DNSName") {
                None | Some(serde_json::Value::Null) => String::new(),
                Some(serde_json::Value::String(s)) => s.clone(),
                Some(_) => return Err(unreadable()),
            };
            let ips = strings(o.get("TailscaleIPs").unwrap_or(&serde_json::Value::Null))
                .ok_or_else(unreadable)?;
            (dns, ips)
        }
        _ => return Err(unreadable()),
    };
    if state != "Running" {
        return Err(format!(
            "Tailscale is not running on this box (it says {state}). Run tailscale up, {AGAIN}"
        ));
    }
    if domains.is_empty() {
        return Err(format!("Tailscale HTTPS certificates are off for this tailnet. Turn on MagicDNS and HTTPS in the Tailscale admin console, {AGAIN}"));
    }
    let mut ts = Tailscale {
        name: domains[0].clone(),
        ips,
    };
    let name = dns.strip_suffix('.').unwrap_or(&dns);
    if domains.iter().any(|d| d == name) {
        ts.name = name.to_string();
    }
    if ts.ips.is_empty() {
        return Err(format!(
            "Tailscale gives this box no tailnet address. Run tailscale up, {AGAIN}"
        ));
    }
    Ok(ts)
}

/// What ensure_cert did: first when no certificate was there before, got
/// when tailscale cert ran, and the whole days the certificate has left.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct CertResult {
    pub first: bool,
    pub got: bool,
    pub days: i64,
}

/// Now, in nanoseconds since the epoch.
pub fn now_nanos() -> i128 {
    let (s, n) = super::localca::now();
    s as i128 * 1_000_000_000 + n as i128
}

/// The whole days from now (nanoseconds) to not_after (seconds), rounded
/// down, so a certificate that ran out an hour ago has -1.
pub fn cert_days(not_after: i64, now_ns: i128) -> i64 {
    let left = not_after as i128 * 1_000_000_000 - now_ns;
    left.div_euclid(86_400 * 1_000_000_000) as i64
}

/// The most read_small reads: a certificate, a key or the serving file
/// is a few KiB.
const SMALL_FILE_MAX: u64 = 64 << 10;

/// Reads a regular file of at most 64 KiB, so a device or a pipe named
/// where a certificate belongs never hangs or floods a reader.
pub fn read_small(path: &Path) -> Result<Vec<u8>, String> {
    use std::io::Read;
    let f = std::fs::File::open(path).map_err(|e| crate::sys::go_path_err("open", path, &e))?;
    let meta = f
        .metadata()
        .map_err(|e| crate::sys::go_path_err("stat", path, &e))?;
    if !meta.is_file() {
        return Err(format!("{} is not a regular file", path.display()));
    }
    if meta.len() > SMALL_FILE_MAX {
        return Err(format!("{} is larger than 64 KiB", path.display()));
    }
    let mut buf = Vec::new();
    f.take(SMALL_FILE_MAX)
        .read_to_end(&mut buf)
        .map_err(|e| crate::sys::go_path_err("read", path, &e))?;
    Ok(buf)
}

/// True when the file holds a PEM private key.
pub fn key_is_pem(key_file: &str) -> bool {
    match read_small(Path::new(key_file)) {
        Ok(raw) => super::der::pem_blocks(&raw)
            .iter()
            .any(|(k, _)| k.ends_with("PRIVATE KEY")),
        Err(_) => false,
    }
}

/// True when the certificate parses and has not run out, and the key file
/// holds a PEM private key.
pub fn valid_pair(cert_file: &str, key_file: &str, now_ns: i128) -> bool {
    match super::localca::load_leaf(Path::new(cert_file)) {
        Ok(not_after) => not_after as i128 * 1_000_000_000 > now_ns && key_is_pem(key_file),
        Err(_) => false,
    }
}

/// Makes dir 0700 when it is not there. A directory that is already
/// there keeps its mode: it may be shared.
fn mkdir_own(dir: &Path) -> Result<(), String> {
    if std::fs::metadata(dir).is_ok() {
        return Ok(());
    }
    super::config::mkdir_private(dir)
}

/// Refuses, in one line, a certificate path that holds something other
/// than a PEM certificate, or a key path that holds something other than
/// a PEM private key, so tailscale cert never writes over a file it did
/// not make. Paths that are not there pass.
pub fn check_pair_files(cert_file: &str, key_file: &str) -> Result<(), String> {
    if std::fs::metadata(cert_file).is_ok()
        && super::localca::load_leaf(Path::new(cert_file)).is_err()
    {
        return Err(format!("{cert_file} is not a PEM certificate, so tailscale cert will not write over it. Move it away or name another --cert, {AGAIN}"));
    }
    if std::fs::metadata(key_file).is_ok() && !key_is_pem(key_file) {
        return Err(format!("{key_file} is not a PEM private key, so tailscale cert will not write over it. Move it away or name another --key, {AGAIN}"));
    }
    Ok(())
}

/// Keeps the pair when its certificate has 30 days or more left, and else
/// runs tailscale cert for name into it. It never writes over a file that
/// is not a PEM certificate, or a key file that is not a PEM private key.
pub fn ensure_cert(
    name: &str,
    cert_file: &str,
    key_file: &str,
    now_ns: i128,
) -> Result<CertResult, String> {
    let mut res = CertResult::default();
    check_pair_files(cert_file, key_file)?;
    let cert = Path::new(cert_file);
    match std::fs::metadata(cert) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => res.first = true,
        _ => {
            if let Ok(not_after) = super::localca::load_leaf(cert) {
                let left = not_after as i128 * 1_000_000_000 - now_ns;
                if left >= RENEW_WINDOW_SECS as i128 * 1_000_000_000 && key_is_pem(key_file) {
                    res.days = cert_days(not_after, now_ns);
                    return Ok(res);
                }
            }
        }
    }
    for f in [cert_file, key_file] {
        let dir = Path::new(f).parent().map(PathBuf::from).unwrap_or_default();
        mkdir_own(&dir)?;
    }
    let Some(bin) = look_path() else {
        return Err(NO_TAILSCALE.into());
    };
    let out = std::process::Command::new(bin)
        .arg("cert")
        .arg(format!("--cert-file={cert_file}"))
        .arg(format!("--key-file={key_file}"))
        .arg(name)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .output();
    match out {
        Ok(o) if o.status.success() => {}
        Ok(o) => return Err(format!(
            "tailscale cert failed{}. If it says access denied, run sudo tailscale set --operator=$USER once, {AGAIN}",
            last_line(&String::from_utf8_lossy(&o.stderr))
        )),
        Err(_) => return Err(format!(
            "tailscale cert failed. If it says access denied, run sudo tailscale set --operator=$USER once, {AGAIN}"
        )),
    }
    let not_after = super::localca::load_leaf(cert).map_err(|e| {
        format!("tailscale cert wrote {cert_file}, but it is not a certificate: {e}")
    })?;
    res.got = true;
    res.days = cert_days(not_after, now_ns);
    Ok(res)
}

/// "day" or "days" for n.
pub fn plural(n: i64, unit: &str) -> String {
    if n == 1 {
        unit.to_string()
    } else {
        format!("{unit}s")
    }
}

/// The lines web serve prints about what ensure_cert did: none for a kept
/// certificate; the first one also names the public certificate logs.
pub fn cert_lines(name: &str, res: &CertResult) -> Vec<String> {
    if !res.got {
        return Vec::new();
    }
    let verb = if res.first {
        "Got a certificate"
    } else {
        "Renewed the certificate"
    };
    let mut lines = vec![format!(
        "{verb} for {name}. It runs out in {} {}.",
        res.days,
        plural(res.days, "day")
    )];
    if res.first {
        lines.push(format!(
            "This name is now in public certificate logs: {name}."
        ));
    }
    lines
}

/// Where the phone server listens under --tls tailscale: a listen address
/// with no host means each tailnet address and 127.0.0.1 on its port; one
/// with a host is used as given.
pub fn listen_addrs(listen: &str, ips: &[String]) -> Result<Vec<String>, String> {
    let Ok((host, port)) = super::split_host_port(listen) else {
        return Err(format!(
            "--listen {} is not host:port",
            crate::sys::go_quote(listen)
        ));
    };
    if !host.is_empty() {
        return Ok(vec![listen.to_string()]);
    }
    let mut out: Vec<String> = ips
        .iter()
        .map(|ip| super::join_host_port(ip, &port))
        .collect();
    out.push(super::join_host_port("127.0.0.1", &port));
    Ok(out)
}

/// The one line web serve --tls tailscale prints once it listens.
pub fn listening_line(url: &str, addrs: &[String], only: bool) -> String {
    let mut line = format!("Serving {url} on {}.", addrs.join(" and "));
    if only {
        line.push_str(" Only the tailnet and this box can reach it.");
    }
    line
}

/// True when listen is host:port with no host.
pub fn blank_host(listen: &str) -> bool {
    matches!(super::split_host_port(listen), Ok((h, _)) if h.is_empty())
}

/// Serves the pair on disk, and reads it again when the certificate file
/// changes, so a renewal reaches the next handshake with no restart.
#[derive(Debug)]
pub struct Reloader {
    cert_file: String,
    key_file: String,
    state: Mutex<Option<(SystemTime, u64, Arc<CertifiedKey>)>>,
}

impl Reloader {
    /// Loads the pair once, so a bad pair fails at start.
    pub fn new(cert_file: &str, key_file: &str) -> Result<Arc<Reloader>, String> {
        let r = Reloader {
            cert_file: cert_file.into(),
            key_file: key_file.into(),
            state: Mutex::new(None),
        };
        r.current()?;
        Ok(Arc::new(r))
    }

    fn load(&self) -> Result<Arc<CertifiedKey>, String> {
        let cert_pem = std::fs::read(&self.cert_file)
            .map_err(|e| crate::sys::go_path_err("open", Path::new(&self.cert_file), &e))?;
        let key_pem = std::fs::read(&self.key_file)
            .map_err(|e| crate::sys::go_path_err("open", Path::new(&self.key_file), &e))?;
        let certs: Vec<CertificateDer<'static>> = super::der::pem_blocks(&cert_pem)
            .into_iter()
            .filter(|(k, _)| k == "CERTIFICATE")
            .map(|(_, b)| CertificateDer::from(b))
            .collect();
        if certs.is_empty() {
            return Err("tls: failed to find any PEM data in certificate input".into());
        }
        let key = super::der::pem_blocks(&key_pem)
            .into_iter()
            .find_map(|(k, b)| match k.as_str() {
                "PRIVATE KEY" => Some(PrivateKeyDer::Pkcs8(b.into())),
                "EC PRIVATE KEY" => Some(PrivateKeyDer::Sec1(b.into())),
                "RSA PRIVATE KEY" => Some(PrivateKeyDer::Pkcs1(b.into())),
                _ => None,
            })
            .ok_or_else(|| "tls: failed to find any PEM data in key input".to_string())?;
        let provider = rustls::crypto::ring::default_provider();
        CertifiedKey::from_der(certs, key, &provider)
            .map(Arc::new)
            .map_err(|e| format!("tls: {e}"))
    }

    /// The pair to serve now. A pair that no longer loads keeps the last
    /// good one in service.
    pub fn current(&self) -> Result<Arc<CertifiedKey>, String> {
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        let meta = std::fs::metadata(&self.cert_file).ok();
        let stamp = meta
            .as_ref()
            .and_then(|m| m.modified().ok().map(|t| (t, m.len())));
        if let (Some((mt, len)), Some((smt, slen, ck))) = (stamp, st.as_ref()) {
            if mt == *smt && len == *slen {
                return Ok(ck.clone());
            }
        }
        match self.load() {
            Ok(ck) => {
                let (mt, len) = stamp.unwrap_or((SystemTime::UNIX_EPOCH, 0));
                *st = Some((mt, len, ck.clone()));
                Ok(ck)
            }
            Err(e) => match st.as_ref() {
                Some((_, _, ck)) => Ok(ck.clone()),
                None => Err(e),
            },
        }
    }
}

impl rustls::server::ResolvesServerCert for Reloader {
    fn resolve(&self, _: rustls::server::ClientHello<'_>) -> Option<Arc<CertifiedKey>> {
        self.current().ok()
    }
}

/// A rustls server config that serves what r has, with TLS 1.2 at least.
pub fn server_config(r: Arc<Reloader>) -> Result<Arc<rustls::ServerConfig>, String> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut cfg = rustls::ServerConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13, &rustls::version::TLS12])
        .map_err(|e| e.to_string())?
        .with_no_client_auth()
        .with_cert_resolver(r);
    cfg.alpn_protocols = vec![b"http/1.1".to_vec()];
    Ok(Arc::new(cfg))
}

/// The longest a saved phone server waits between two tries to bind its
/// tailnet address at boot.
pub const TAILNET_RETRY_MAX: std::time::Duration = std::time::Duration::from_secs(60);

/// The first wait.
pub const TAILNET_RETRY_FIRST: std::time::Duration = std::time::Duration::from_secs(2);

/// Doubles a wait, up to TAILNET_RETRY_MAX.
pub fn next_tailnet_retry(d: std::time::Duration) -> std::time::Duration {
    (d * 2).min(TAILNET_RETRY_MAX)
}

/// A tailnet address that would not bind, and why.
pub struct Skipped {
    pub addr: String,
    pub err: String,
}

/// True for an address the serve may bind as a tailnet address: one
/// inside Tailscale's ranges (100.64.0.0/10 and fd7a:115c:a1e0::/48), or a
/// loopback address, which reaches only this box. An empty, unspecified
/// or unparsable address is never one.
pub fn tailnet_addr(ip: &str) -> bool {
    let Some(a) = super::parse_ip(ip) else {
        return false;
    };
    let a = match a {
        std::net::IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            Some(v4) => std::net::IpAddr::V4(v4),
            None => std::net::IpAddr::V6(v6),
        },
        v4 => v4,
    };
    if a.is_unspecified() {
        return false;
    }
    if a.is_loopback() {
        return true;
    }
    match a {
        std::net::IpAddr::V4(v4) => {
            let o = v4.octets();
            o[0] == 100 && (o[1] & 0xc0) == 64
        }
        std::net::IpAddr::V6(v6) => {
            let s = v6.segments();
            s[0] == 0xfd7a && s[1] == 0x115c && s[2] == 0xa1e0
        }
    }
}

/// True when a listen address with a host names loopback or a tailnet
/// address.
pub fn listen_stays_on_tailnet(listen: &str) -> bool {
    match super::split_host_port(listen) {
        Ok((h, _)) => h == "localhost" || tailnet_addr(&h),
        Err(_) => false,
    }
}

/// One line for each address tailscale gave that is not a tailnet
/// address.
pub fn log_foreign(foreign: &[String]) {
    for ip in foreign {
        super::log(
            "WARN",
            "web: skipped an address that is not a tailnet address",
            &[("addr", ip)],
        );
    }
}

/// Binds each tailnet address on port: what it bound, their addresses,
/// the ones it skipped because they would not bind, and the ones it never
/// tried because they are not tailnet addresses.
pub fn bind_tailnet(
    ips: &[String],
    port: &str,
) -> (
    Vec<std::net::TcpListener>,
    Vec<String>,
    Vec<Skipped>,
    Vec<String>,
) {
    let (mut lns, mut addrs, mut skipped, mut foreign) =
        (Vec::new(), Vec::new(), Vec::new(), Vec::new());
    for ip in ips {
        if !tailnet_addr(ip) {
            foreign.push(ip.clone());
            continue;
        }
        let a = super::join_host_port(ip, port);
        match super::serve::listen_tcp(&a) {
            Ok(ln) => {
                lns.push(ln);
                addrs.push(a);
            }
            Err(err) => skipped.push(Skipped { addr: a, err }),
        }
    }
    (lns, addrs, skipped, foreign)
}

/// One line for each tailnet address that would not bind.
pub fn log_skipped(skipped: &[Skipped]) {
    for s in skipped {
        super::log(
            "WARN",
            "web: skipped a tailnet address it cannot bind",
            &[("addr", &s.addr), ("err", &s.err)],
        );
    }
}

/// The error for a serve that could bind no tailnet address.
pub fn no_tailnet_bind(skipped: &[Skipped]) -> String {
    let last = skipped
        .last()
        .map(|s| s.err.clone())
        .unwrap_or_else(|| "tailscale gives no address".into());
    format!("No tailnet address could be bound: {last}. Check that tailscale is up, {AGAIN}")
}

/// Where a running tailscale serve says which certificate it serves.
pub fn serving_path(data_dir: &Path) -> PathBuf {
    data_dir.join("web").join("serving.json")
}

/// Writes the serving file, mode 0600.
pub fn write_serving(path: &str, cert_file: &str, key_file: &str) -> Result<(), String> {
    if path.is_empty() {
        return Ok(());
    }
    let p = Path::new(path);
    if let Some(dir) = p.parent() {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)
            .map_err(|e| crate::sys::go_path_err("mkdir", dir, &e))?;
    }
    let body = crate::gojson::marshal(&crate::gojson::obj(vec![
        ("pid", serde_json::json!(std::process::id())),
        ("cert_file", serde_json::json!(cert_file)),
        ("key_file", serde_json::json!(key_file)),
    ]));
    super::config::write_file_mode(p, body.as_bytes(), 0o600)
}

/// The pid, certificate and key a serving file names.
fn read_serving(path: &Path) -> Option<(i64, String, String)> {
    let raw = read_small(path).ok()?;
    let v: serde_json::Value = serde_json::from_slice(&raw).ok()?;
    let pid = match v.get("pid") {
        None | Some(serde_json::Value::Null) => 0,
        Some(p) => p.as_i64()?,
    };
    let field = |k: &str| -> Option<String> {
        match v.get(k) {
            None | Some(serde_json::Value::Null) => Some(String::new()),
            Some(c) => c.as_str().map(String::from),
        }
    };
    Some((pid, field("cert_file")?, field("key_file")?))
}

/// Removes the serving file when this process wrote it.
pub fn remove_serving(path: &str) {
    if path.is_empty() {
        return;
    }
    if let Some((pid, _, _)) = read_serving(Path::new(path)) {
        if pid == std::process::id() as i64 {
            let _ = std::fs::remove_file(path);
        }
    }
}

/// The certificate a running serve names in the file at path; None when
/// there is no file, it does not read, or the process that wrote it is
/// gone.
pub fn serving_cert(path: &Path) -> Option<String> {
    serving_pair(path).map(|(c, _)| c)
}

/// The certificate and key a running serve names, as serving_cert says.
pub fn serving_pair(path: &Path) -> Option<(String, String)> {
    let (pid, cert, key) = read_serving(path)?;
    if pid <= 0 || pid > i32::MAX as i64 || cert.is_empty() {
        return None;
    }
    // SAFETY: kill with signal 0 only asks whether the process exists.
    let r = unsafe { libc::kill(pid as libc::pid_t, 0) };
    if r != 0 && std::io::Error::last_os_error().raw_os_error() != Some(libc::EPERM) {
        return None;
    }
    Some((cert, key))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The fake tailscale the web cases use, first on PATH. The tests that
    /// change PATH or the fake's knobs hold this lock, since the
    /// environment is the process's.
    static ENV: Mutex<()> = Mutex::new(());

    fn fake_dir() -> String {
        format!("{}/cases/web/fixtures/bin", env!("CARGO_MANIFEST_DIR"))
    }

    fn with_env<T>(vars: &[(&str, &str)], f: impl FnOnce() -> T) -> T {
        let _g = ENV.lock().unwrap_or_else(|e| e.into_inner());
        let keys = [
            "PATH",
            "FAKE_TS_NAME",
            "FAKE_TS_IPS",
            "FAKE_TS_STATE",
            "FAKE_TS_HTTPS",
            "FAKE_TS_DAYS",
            "FAKE_TS_LOG",
        ];
        let saved: Vec<_> = keys.iter().map(|k| (*k, std::env::var_os(k))).collect();
        for k in &keys[1..] {
            std::env::remove_var(k);
        }
        std::env::set_var("PATH", format!("{}:/usr/bin:/bin", fake_dir()));
        for (k, v) in vars {
            std::env::set_var(k, v);
        }
        let out = f();
        for (k, v) in saved {
            match v {
                Some(v) => std::env::set_var(k, v),
                None => std::env::remove_var(k),
            }
        }
        out
    }

    fn scratch(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("cop-ts-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn reads_the_name_and_the_tailnet_addresses() {
        let ts = with_env(&[("FAKE_TS_IPS", "127.0.0.2,fd7a:115c::1")], read_tailscale).unwrap();
        assert_eq!(ts.name, "box.tail1.ts.net");
        assert_eq!(ts.ips, vec!["127.0.0.2", "fd7a:115c::1"]);
    }

    #[test]
    fn no_tailscale_is_one_line_that_names_coppice_web_serve() {
        let d = scratch("nots");
        let err = with_env(&[("PATH", d.to_str().unwrap())], read_tailscale).unwrap_err();
        assert_eq!(err, NO_TAILSCALE);
        assert!(!NO_TAILSCALE.contains('\n') && NO_TAILSCALE.contains("coppice web serve"));
    }

    #[test]
    fn refuses_a_stopped_tailscale_and_https_off() {
        let err = with_env(&[("FAKE_TS_STATE", "NeedsLogin")], read_tailscale).unwrap_err();
        assert!(
            err.contains("NeedsLogin") && err.contains("tailscale up"),
            "{err}"
        );
        let err = with_env(&[("FAKE_TS_HTTPS", "off")], read_tailscale).unwrap_err();
        assert!(err.contains("HTTPS"), "{err}");
    }

    #[test]
    fn gets_keeps_and_renews_the_certificate() {
        let d = scratch("ensure");
        let log = d.join("ts.log");
        let cert = d.join("tls/t.crt");
        let key = d.join("tls/t.key");
        let (c, k) = (cert.to_str().unwrap(), key.to_str().unwrap());
        let logs = log.to_str().unwrap();
        let r = with_env(&[("FAKE_TS_LOG", logs), ("FAKE_TS_DAYS", "20")], || {
            ensure_cert("box.tail1.ts.net", c, k, now_nanos())
        })
        .unwrap();
        assert_eq!(
            r,
            CertResult {
                first: true,
                got: true,
                days: 20
            }
        );
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(d.join("tls"))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o700);
        let r = with_env(&[("FAKE_TS_LOG", logs)], || {
            ensure_cert("box.tail1.ts.net", c, k, now_nanos())
        })
        .unwrap();
        assert_eq!(
            r,
            CertResult {
                first: false,
                got: true,
                days: 90
            }
        );
        let r = with_env(&[("FAKE_TS_LOG", logs)], || {
            ensure_cert(
                "box.tail1.ts.net",
                c,
                k,
                now_nanos() + 59 * 86_400 * 1_000_000_000,
            )
        })
        .unwrap();
        assert_eq!(
            r,
            CertResult {
                first: false,
                got: false,
                days: 31
            }
        );
        let n = std::fs::read_to_string(&log)
            .unwrap()
            .matches("cert ")
            .count();
        assert_eq!(n, 2);
        let err = with_env(&[], || {
            ensure_cert("fail.tail1.ts.net", &format!("{c}.x"), k, now_nanos())
        })
        .unwrap_err();
        assert!(
            err.contains("HTTPS is off") && err.contains("coppice web serve --tls tailscale"),
            "{err}"
        );
    }

    /// The check a running phone server repeats renews a certificate that
    /// has fallen under the window.
    #[test]
    fn the_daily_check_renews_while_the_server_runs() {
        let d = scratch("daily");
        let log = d.join("ts.log");
        let (c, k) = (d.join("t.crt"), d.join("t.key"));
        let (c, k) = (
            c.to_str().unwrap().to_string(),
            k.to_str().unwrap().to_string(),
        );
        let logs = log.to_str().unwrap().to_string();
        with_env(&[("FAKE_TS_LOG", &logs), ("FAKE_TS_DAYS", "20")], || {
            let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
            let st = stop.clone();
            let (c2, k2) = (c.clone(), k.clone());
            let h = std::thread::spawn(move || {
                crate::web::serve::check_cert_daily(
                    &c2,
                    &k2,
                    std::time::Duration::from_millis(50),
                    &st,
                )
            });
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
            let mut n = 0;
            while std::time::Instant::now() < deadline {
                n = std::fs::read_to_string(&log)
                    .unwrap_or_default()
                    .matches("cert ")
                    .count();
                if n >= 2 {
                    break;
                }
                std::thread::sleep(std::time::Duration::from_millis(50));
            }
            stop.store(true, std::sync::atomic::Ordering::SeqCst);
            h.join().unwrap();
            assert!(n >= 2, "the certificate was checked {n} times");
        });
    }

    #[test]
    fn listen_addrs_keep_the_tailnet_and_loopback_for_a_blank_host() {
        let ips = vec!["100.64.0.1".to_string(), "fd7a:115c::1".to_string()];
        assert_eq!(
            listen_addrs(":8443", &ips).unwrap(),
            vec!["100.64.0.1:8443", "[fd7a:115c::1]:8443", "127.0.0.1:8443"]
        );
        assert_eq!(
            listen_addrs("0.0.0.0:9000", &ips).unwrap(),
            vec!["0.0.0.0:9000"]
        );
        assert_eq!(
            listen_addrs("nohost", &ips).unwrap_err(),
            "--listen \"nohost\" is not host:port"
        );
    }

    #[test]
    fn the_tailnet_backoff_doubles_to_sixty_seconds() {
        let mut d = TAILNET_RETRY_FIRST;
        let mut seen = Vec::new();
        for _ in 0..7 {
            seen.push(d.as_secs());
            d = next_tailnet_retry(d);
        }
        assert_eq!(seen, vec![2, 4, 8, 16, 32, 60, 60]);
    }

    #[test]
    fn the_serving_file_names_a_live_serve_only() {
        let d = scratch("serving");
        let p = d.join("web/serving.json");
        let ps = p.to_str().unwrap();
        write_serving(ps, "/x/own.crt", "/x/own.key").unwrap();
        assert_eq!(serving_cert(&p).as_deref(), Some("/x/own.crt"));
        assert_eq!(
            serving_pair(&p),
            Some(("/x/own.crt".to_string(), "/x/own.key".to_string()))
        );
        let body = std::fs::read_to_string(&p).unwrap();
        assert_eq!(
            body,
            format!(
                "{{\"pid\":{},\"cert_file\":\"/x/own.crt\",\"key_file\":\"/x/own.key\"}}",
                std::process::id()
            )
        );
        remove_serving(ps);
        assert!(!p.exists());
        std::fs::write(&p, r#"{"pid": 999999999, "cert_file": "/x.crt"}"#).unwrap();
        assert_eq!(serving_cert(&p), None);
        assert_eq!(serving_cert(&d.join("none.json")), None);
    }

    #[test]
    fn a_tailnet_address_that_will_not_bind_is_skipped() {
        let ips = vec![
            "127.0.0.2".to_string(),
            "fd7a:115c:a1e0::1".to_string(),
            "".to_string(),
            "0.0.0.0".to_string(),
        ];
        let (lns, addrs, skipped, foreign) = bind_tailnet(&ips, "0");
        assert_eq!(lns.len(), 1);
        assert_eq!(addrs, vec!["127.0.0.2:0"]);
        assert_eq!(skipped.len(), 1);
        assert_eq!(skipped[0].addr, "[fd7a:115c:a1e0::1]:0");
        assert!(
            skipped[0]
                .err
                .starts_with("listen tcp [fd7a:115c:a1e0::1]:0: bind: "),
            "{}",
            skipped[0].err
        );
        assert_eq!(foreign, vec!["", "0.0.0.0"]);
        let (_, _, none, _) = bind_tailnet(&ips[1..2], "0");
        assert!(no_tailnet_bind(&none)
            .starts_with("No tailnet address could be bound: listen tcp [fd7a:115c:a1e0::1]:0"));
    }

    #[test]
    fn only_tailscale_ranges_and_loopback_are_tailnet_addresses() {
        for (ip, want) in [
            ("100.64.0.1", true),
            ("100.127.255.255", true),
            ("100.128.0.1", false),
            ("100.63.255.255", false),
            ("fd7a:115c:a1e0::1", true),
            ("fd7a:115c:a1e0:ab12::1", true),
            ("fd7a:115c:a1e1::1", false),
            ("127.0.0.2", true),
            ("::1", true),
            ("", false),
            ("0.0.0.0", false),
            ("::", false),
            ("192.168.1.2", false),
            ("10.0.0.1", false),
            ("nonsense", false),
        ] {
            assert_eq!(tailnet_addr(ip), want, "{ip}");
        }
    }

    #[test]
    fn a_file_that_is_not_a_certificate_is_never_overwritten() {
        let d = scratch("notcert");
        let notes = d.join("notes.txt");
        std::fs::write(&notes, "my notes\n").unwrap();
        let n = notes.to_str().unwrap();
        let k = d.join("k.key");
        let err = with_env(&[], || {
            ensure_cert("box.tail1.ts.net", n, k.to_str().unwrap(), now_nanos())
        })
        .unwrap_err();
        assert_eq!(err, format!("{n} is not a PEM certificate, so tailscale cert will not write over it. Move it away or name another --cert, then run coppice web serve --tls tailscale again."));
        assert_eq!(std::fs::read_to_string(&notes).unwrap(), "my notes\n");
        let key = d.join("k2.key");
        std::fs::write(&key, "not a key\n").unwrap();
        let ks = key.to_str().unwrap();
        let c2 = d.join("c2.crt");
        let err = with_env(&[], || {
            ensure_cert("box.tail1.ts.net", c2.to_str().unwrap(), ks, now_nanos())
        })
        .unwrap_err();
        assert_eq!(err, format!("{ks} is not a PEM private key, so tailscale cert will not write over it. Move it away or name another --key, then run coppice web serve --tls tailscale again."));
        assert_eq!(std::fs::read_to_string(&key).unwrap(), "not a key\n");
    }

    #[test]
    fn a_directory_it_did_not_make_keeps_its_mode() {
        use std::os::unix::fs::PermissionsExt;
        let d = scratch("shared");
        let dir = d.join("shared");
        std::fs::create_dir(&dir).unwrap();
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755)).unwrap();
        let (c, k) = (dir.join("c.crt"), dir.join("c.key"));
        with_env(&[], || {
            ensure_cert(
                "box.tail1.ts.net",
                c.to_str().unwrap(),
                k.to_str().unwrap(),
                now_nanos(),
            )
        })
        .unwrap();
        let mode = std::fs::metadata(&dir).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o755);
    }

    #[test]
    fn an_empty_or_relative_path_entry_is_not_searched() {
        // cargo runs the tests in the crate directory, where this relative
        // entry holds the fake tailscale.
        let rel = std::ffi::OsStr::new("cases/web/fixtures/bin");
        assert!(std::path::Path::new("cases/web/fixtures/bin/tailscale").exists());
        assert_eq!(look_path_in(rel), None);
        assert_eq!(look_path_in(std::ffi::OsStr::new(":.:")), None);
        let abs = format!("{}/cases/web/fixtures/bin", env!("CARGO_MANIFEST_DIR"));
        assert!(look_path_in(std::ffi::OsStr::new(&abs)).is_some());
    }

    #[test]
    fn cert_days_is_whole_days_left() {
        let now = 1_000_000_000i64;
        let ns = now as i128 * 1_000_000_000;
        assert_eq!(cert_days(now + 3 * 86_400 - 1, ns), 2);
        assert_eq!(cert_days(now + 3 * 86_400, ns), 3);
        assert_eq!(cert_days(now + 3600, ns), 0);
        assert_eq!(cert_days(now - 3600, ns), -1);
    }

    #[test]
    fn the_reloader_serves_the_new_pair_after_a_renewal() {
        let d = scratch("reload");
        let (c, k) = (d.join("t.crt"), d.join("t.key"));
        let (c, k) = (c.to_str().unwrap(), k.to_str().unwrap());
        with_env(&[], || ensure_cert("box.tail1.ts.net", c, k, now_nanos())).unwrap();
        let r = Reloader::new(c, k).unwrap();
        let first = r.current().unwrap();
        std::fs::remove_file(c).unwrap();
        with_env(&[("FAKE_TS_DAYS", "40")], || {
            ensure_cert("box.tail1.ts.net", c, k, now_nanos())
        })
        .unwrap();
        let later = SystemTime::now() + std::time::Duration::from_secs(60);
        std::fs::File::options()
            .write(true)
            .open(c)
            .unwrap()
            .set_modified(later)
            .unwrap();
        let second = r.current().unwrap();
        assert_ne!(first.cert[0].as_ref(), second.cert[0].as_ref());
    }
}
