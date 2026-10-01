//! The voice supervisor: it starts `daisugi voice serve` on a loopback
//! port, watches it, and stops it with the server. One process runs at
//! most. A process that dies after it answered starts once more; when it
//! dies again, or never answers, voice is down with a reason until
//! voice.start runs.

use std::io::{Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream, ToSocketAddrs};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::process::CommandExt;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::sync::{Arc, Mutex, Weak};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use super::Server;
use crate::proto::{self, Request, Response};
use crate::sys;

/// What the server does about voice. The default starts nothing until
/// voice.start asks.
#[derive(Clone, Debug, Default)]
pub struct VoiceConfig {
    pub off: bool,
    pub off_reason: String,
    pub off_fix: String,
    pub url: String,
    pub token_file: String,
    pub args: Vec<String>,
}

/// The shell lines that install daisugi with its voice extra.
const INSTALL_VOICE: &str = "pip install 'opendaisugi[voice]'";
const UPGRADE_VOICE: &str = "pip install --upgrade 'opendaisugi[voice]'";

/// The flags the server passes to daisugi voice serve itself.
const MANAGED_FLAGS: &[&str] = &["--host", "--listen", "--port", "--token-file"];

/// Why a [voice] table cannot be used, or None when it can.
fn refuse_config(cfg: &VoiceConfig) -> Option<(String, String)> {
    if !cfg.url.is_empty() && cfg.token_file.is_empty() && !loopback_url(&cfg.url) {
        return Some((
            "Voice is off: [voice] url names another machine, and coppice sends it no web token.".into(),
            "Set [voice] token_file in coppice.toml to that server's token file, then start the coppice server again".into(),
        ));
    }
    for a in &cfg.args {
        for f in MANAGED_FLAGS {
            if a == f || a.starts_with(&format!("{f}=")) {
                return Some((
                    format!("Voice is off: [voice] args sets {f}, which coppice sets itself."),
                    format!("Take {f} out of [voice] args in coppice.toml, then start the coppice server again"),
                ));
            }
        }
    }
    None
}

/// The host and port of an http URL, as Go's url.Parse and Hostname read
/// them for the cases coppice meets.
fn host_port(raw: &str) -> Option<(String, Option<String>)> {
    let rest = raw.split_once("://").map(|x| x.1)?;
    let auth = rest.split(['/', '?', '#']).next().unwrap_or("");
    let auth = auth.rsplit_once('@').map(|x| x.1).unwrap_or(auth);
    if let Some(r) = auth.strip_prefix('[') {
        let (h, tail) = r.split_once(']')?;
        let port = tail.strip_prefix(':').map(String::from);
        return Some((h.to_string(), port));
    }
    match auth.rsplit_once(':') {
        Some((h, p)) => Some((h.to_string(), Some(p.to_string()))),
        None => Some((auth.to_string(), None)),
    }
}

/// Whether raw names localhost or a loopback address.
fn loopback_url(raw: &str) -> bool {
    let Some((host, _)) = host_port(raw) else {
        return false;
    };
    if host == "localhost" {
        return true;
    }
    match host.parse::<std::net::IpAddr>() {
        Ok(std::net::IpAddr::V4(a)) => a.is_loopback(),
        Ok(std::net::IpAddr::V6(a)) => {
            a.is_loopback() || a.to_ipv4_mapped().is_some_and(|v| v.is_loopback())
        }
        Err(_) => false,
    }
}

/// Whether base/health answers 200 within wait.
fn answers(base: &str, wait: Duration) -> bool {
    let Some((host, port)) = host_port(base) else {
        return false;
    };
    if !base.starts_with("http://") {
        return false;
    }
    let port = port.unwrap_or_else(|| "80".into());
    let deadline = Instant::now() + wait;
    let Ok(mut addrs) = (host.as_str(), port.parse::<u16>().unwrap_or(0)).to_socket_addrs() else {
        return false;
    };
    let Some(addr): Option<SocketAddr> = addrs.next() else {
        return false;
    };
    let Ok(mut c) = TcpStream::connect_timeout(&addr, wait) else {
        return false;
    };
    let left = deadline
        .saturating_duration_since(Instant::now())
        .max(Duration::from_millis(1));
    let _ = c.set_read_timeout(Some(left));
    let _ = c.set_write_timeout(Some(left));
    let hostport = if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    };
    let req = format!(
        "GET /health HTTP/1.1\r\nHost: {hostport}\r\nUser-Agent: Go-http-client/1.1\r\nConnection: close\r\n\r\n"
    );
    if c.write_all(req.as_bytes()).is_err() {
        return false;
    }
    let mut buf = Vec::new();
    let mut chunk = [0u8; 256];
    while !buf.windows(2).any(|w| w == b"\r\n") && buf.len() < 4096 {
        match c.read(&mut chunk) {
            Ok(0) | Err(_) => break,
            Ok(n) => buf.extend_from_slice(&chunk[..n]),
        }
    }
    let line = String::from_utf8_lossy(&buf);
    let mut parts = line.split(' ');
    matches!(parts.next(), Some(v) if v.starts_with("HTTP/")) && parts.next() == Some("200")
}

/// TokenBytes of fresh randomness, unpadded base64url.
fn new_token() -> Result<String, String> {
    let mut b = [0u8; 32];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut b))
        .map_err(|e| e.to_string())?;
    const A: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::new();
    for ch in b.chunks(3) {
        let n = ch
            .iter()
            .enumerate()
            .fold(0u32, |n, (i, c)| n | (*c as u32) << (16 - 8 * i));
        let k = ch.len() + 1;
        for i in 0..k {
            out.push(A[((n >> (18 - 6 * i)) & 63) as usize] as char);
        }
    }
    Ok(out)
}

/// The token in path, when there is one.
fn load_token(path: &std::path::Path) -> Option<String> {
    let t = std::fs::read_to_string(path).ok()?;
    let t = t.trim().to_string();
    (!t.is_empty()).then_some(t)
}

/// Writes a fresh token to path, mode 0600 in a directory mode 0700,
/// through a temp file renamed into place.
fn mint_token(path: &std::path::Path) -> Result<String, String> {
    let tok = new_token()?;
    let dir = path.parent().map(PathBuf::from).unwrap_or_default();
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&dir)
        .map_err(|e| sys::go_path_err("mkdir", &dir, &e))?;
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| sys::go_path_err("chmod", &dir, &e))?;
    let tmp = dir.join(format!(".web-token-{}", std::process::id()));
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&tmp)
        .map_err(|e| sys::go_path_err("open", &tmp, &e))?;
    f.write_all(tok.as_bytes())
        .map_err(|e| sys::go_path_err("write", &tmp, &e))?;
    drop(f);
    std::fs::rename(&tmp, path).map_err(|e| {
        let _ = std::fs::remove_file(&tmp);
        e.to_string()
    })?;
    Ok(tok)
}

/// The last line of path that is not empty and that coppice did not write
/// itself.
fn last_line(path: &std::path::Path) -> String {
    let Ok(b) = std::fs::read(path) else {
        return String::new();
    };
    let b = if b.len() > 8192 {
        &b[b.len() - 8192..]
    } else {
        &b[..]
    };
    let mut last = String::new();
    for line in String::from_utf8_lossy(b).split('\n') {
        let line = line.strip_suffix('\r').unwrap_or(line).trim();
        if line.starts_with("coppice: starting") {
            last.clear();
            continue;
        }
        if !line.is_empty() && !line.starts_with("coppice: ") {
            last = line.to_string();
        }
    }
    last
}

/// A free loopback port. The probe closes before daisugi binds.
fn free_port() -> Result<u16, String> {
    let ln = TcpListener::bind("127.0.0.1:0").map_err(|e| e.to_string())?;
    ln.local_addr().map(|a| a.port()).map_err(|e| e.to_string())
}

#[derive(Default)]
struct Sup {
    state: String,
    url: String,
    reason: String,
    fix: String,
    command: String,
    pid: Option<i32>,
    gen: u64,
    restarted: bool,
    stopping: bool,
    /// Set while a process runs: its exit is waited for on its own thread.
    exited: Option<Arc<crate::pane::Flag>>,
}

/// The voice supervisor of one server.
pub struct Voice {
    cfg: VoiceConfig,
    data_dir: PathBuf,
    health: Duration,
    every: Duration,
    sup: Mutex<Sup>,
    me: Weak<Voice>,
}

impl Voice {
    pub fn new(mut cfg: VoiceConfig, data_dir: PathBuf) -> Arc<Voice> {
        if !cfg.off {
            if let Some((reason, fix)) = refuse_config(&cfg) {
                cfg.off = true;
                cfg.off_reason = reason;
                cfg.off_fix = fix;
            }
        }
        let mut sup = Sup {
            state: "idle".into(),
            ..Default::default()
        };
        if cfg.off {
            sup.state = "off".into();
        } else if !cfg.url.is_empty() {
            sup.url = cfg.url.trim_end_matches('/').to_string();
        }
        Arc::new_cyclic(|me| Voice {
            cfg,
            data_dir,
            health: Duration::from_secs(180),
            every: Duration::from_millis(250),
            sup: Mutex::new(sup),
            me: me.clone(),
        })
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Sup> {
        self.sup.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn voice_dir(&self) -> PathBuf {
        self.data_dir.join("voice")
    }

    fn token_file(&self) -> PathBuf {
        if !self.cfg.url.is_empty() && !self.cfg.token_file.is_empty() {
            return PathBuf::from(&self.cfg.token_file);
        }
        if !self.cfg.url.is_empty() {
            return self.data_dir.join("web").join("token");
        }
        self.voice_dir().join("token")
    }

    fn log_path(&self) -> PathBuf {
        self.voice_dir().join("voice.log")
    }

    /// What voice.status answers. It never waits on the process; for a
    /// voice server elsewhere it asks /health, for at most one second.
    pub fn status(&self) -> Value {
        let tf = self.token_file().to_string_lossy().into_owned();
        if !self.cfg.url.is_empty() && !self.cfg.off {
            let url = self.lock().url.clone();
            let mut pairs = vec![
                ("state", json!("ready")),
                ("ready", json!(true)),
                ("url", json!(url)),
                ("token_file", json!(tf)),
                ("managed", json!(false)),
                ("reason", json!("")),
                ("fix", json!("")),
                ("command", json!("")),
            ];
            if !answers(&url, Duration::from_secs(1)) {
                pairs[0].1 = json!("down");
                pairs[1].1 = json!(false);
                pairs[5].1 = json!(format!("The voice server at {url} does not answer."));
                pairs[6].1 = json!("Start it on that machine");
            }
            return crate::gojson::map(pairs);
        }
        let s = self.lock();
        let mut reason = s.reason.clone();
        let mut fix = s.fix.clone();
        let mut url = String::new();
        if s.state == "ready" || s.state == "starting" {
            url = s.url.clone();
        }
        match s.state.as_str() {
            "off" => {
                reason = "Voice is off in coppice.toml.".into();
                fix = "Remove [voice] enabled = false there and start the coppice server again"
                    .into();
                if !self.cfg.off_reason.is_empty() {
                    reason = self.cfg.off_reason.clone();
                    fix = self.cfg.off_fix.clone();
                }
            }
            "idle" => {
                reason = "Voice is not started.".into();
                fix = String::new();
            }
            "starting" => {
                reason = "Voice is starting. The first start can take a minute.".into();
                fix = "Wait a moment".into();
            }
            _ => {}
        }
        let mut pairs = vec![
            ("state", json!(s.state)),
            ("ready", json!(s.state == "ready")),
            ("url", json!(url)),
            ("token_file", json!(tf)),
            ("managed", json!(true)),
            ("reason", json!(reason)),
            ("fix", json!(fix)),
            ("command", json!(s.command)),
        ];
        if let Some(pid) = s.pid {
            pairs.push(("pid", json!(pid)));
        }
        crate::gojson::map(pairs)
    }

    /// Starts the voice process unless one is starting or ready, or voice
    /// is off or runs elsewhere. It returns at once.
    pub fn ensure(&self) {
        if !self.cfg.url.is_empty() {
            return;
        }
        let mut s = self.lock();
        if s.stopping || matches!(s.state.as_str(), "off" | "starting" | "ready") {
            return;
        }
        s.restarted = false;
        self.begin(&mut s, true);
    }

    fn down(s: &mut Sup, reason: &str, fix: &str) {
        s.state = "down".into();
        s.reason = reason.into();
        s.fix = fix.into();
        s.url.clear();
        s.pid = None;
        s.command.clear();
    }

    fn begin(&self, s: &mut Sup, probe: bool) {
        s.gen += 1;
        s.state = "starting".into();
        s.reason.clear();
        s.fix.clear();
        s.url.clear();
        s.command.clear();
        let gen = s.gen;
        let me = self.me.upgrade().expect("voice");
        thread::spawn(move || me.start(gen, probe, 0));
    }

    /// One attempt, on its own thread, which then waits for the process:
    /// the kernel's parent-death signal follows the thread that started it.
    fn start(self: Arc<Voice>, gen: u64, probe: bool, tries: u32) {
        let fail_with = |reason: &str, fix: &str, command: &str| {
            let mut s = self.lock();
            if gen == s.gen && !s.stopping {
                Self::down(&mut s, reason, fix);
                s.command = command.into();
            }
        };
        let Some(bin) = crate::pane::look_path("daisugi") else {
            fail_with(
                "Voice needs daisugi, and daisugi is not on PATH.",
                "Install opendaisugi with its voice extra",
                INSTALL_VOICE,
            );
            return;
        };
        if probe {
            let ok = Command::new(&bin)
                .args(["voice", "serve", "--help"])
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .ok()
                .and_then(|mut c| {
                    let deadline = Instant::now() + Duration::from_secs(30);
                    loop {
                        match c.try_wait() {
                            Ok(Some(st)) => return Some(st.success()),
                            Ok(None) if Instant::now() < deadline => {
                                thread::sleep(Duration::from_millis(20))
                            }
                            _ => {
                                let _ = c.kill();
                                let _ = c.wait();
                                return Some(false);
                            }
                        }
                    }
                })
                .unwrap_or(false);
            if !ok {
                fail_with(
                    "This daisugi cannot serve voice.",
                    "Update opendaisugi and install its voice extra",
                    UPGRADE_VOICE,
                );
                return;
            }
        }
        let vd = self.voice_dir();
        if let Err(e) = std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&vd)
        {
            fail_with(
                &format!(
                    "Voice cannot make {}: {}.",
                    vd.display(),
                    sys::go_path_err("mkdir", &vd, &e)
                ),
                "Check that the coppice data directory is writable",
                "",
            );
            return;
        }
        let tf = self.token_file();
        if load_token(&tf).is_none() {
            if let Err(e) = mint_token(&tf) {
                fail_with(
                    &format!("Voice has no token file: {e}."),
                    "Check that the coppice data directory is writable",
                    "",
                );
                return;
            }
        }
        let port = match free_port() {
            Ok(p) => p,
            Err(e) => {
                fail_with(&format!("Voice found no free loopback port: {e}."), "", "");
                return;
            }
        };
        let lp = self.log_path();
        let logf = match std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .mode(0o600)
            .open(&lp)
        {
            Ok(f) => f,
            Err(e) => {
                fail_with(
                    &format!(
                        "Voice cannot write its log: {}.",
                        sys::go_path_err("open", &lp, &e)
                    ),
                    "Check that the coppice data directory is writable",
                    "",
                );
                return;
            }
        };
        let mut args: Vec<String> = vec![
            "voice".into(),
            "serve".into(),
            "--host".into(),
            "127.0.0.1".into(),
            "--port".into(),
            port.to_string(),
            "--token-file".into(),
            tf.to_string_lossy().into_owned(),
        ];
        args.extend(self.cfg.args.iter().cloned());
        let url = format!("http://127.0.0.1:{port}");
        let exited = Arc::new(crate::pane::Flag::default());
        let mut child = {
            let mut s = self.lock();
            if gen != s.gen || s.stopping {
                return;
            }
            let mut lf = &logf;
            let _ = writeln!(lf, "coppice: starting daisugi {}", args.join(" "));
            let mut cmd = Command::new(&bin);
            cmd.args(&args).stdin(Stdio::null());
            match (logf.try_clone(), logf.try_clone()) {
                (Ok(a), Ok(b)) => {
                    cmd.stdout(a).stderr(b);
                }
                _ => {
                    cmd.stdout(Stdio::null()).stderr(Stdio::null());
                }
            }
            // SAFETY: prctl is async-signal-safe.
            unsafe {
                cmd.pre_exec(|| {
                    libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM);
                    Ok(())
                });
            }
            match cmd.spawn() {
                Ok(c) => {
                    s.pid = Some(c.id() as i32);
                    s.url = url.clone();
                    s.exited = Some(exited.clone());
                    c
                }
                Err(e) => {
                    Self::down(
                        &mut s,
                        &format!(
                            "daisugi voice serve did not start: fork/exec {bin}: {}.",
                            sys::go_errno_text(&e)
                        ),
                        "",
                    );
                    return;
                }
            }
        };
        drop(logf);
        let pid = child.id() as i32;
        {
            let me = self.clone();
            let url = url.clone();
            let ex = exited.clone();
            thread::spawn(move || me.wait_healthy(gen, &url, &ex));
        }
        let st = child.wait();
        exited.set();
        self.on_exit(gen, pid, st, tries);
    }

    /// Polls /health until it answers, the process exits, or the health
    /// wait runs out. A process that never answers is stopped.
    fn wait_healthy(&self, gen: u64, url: &str, exited: &crate::pane::Flag) {
        let deadline = Instant::now() + self.health;
        while Instant::now() < deadline {
            if exited.wait(Some(self.every)) {
                return;
            }
            if answers(url, Duration::from_secs(1)) {
                let mut s = self.lock();
                if gen == s.gen && s.state == "starting" {
                    s.state = "ready".into();
                }
                return;
            }
        }
        let mut s = self.lock();
        if gen != s.gen || s.state != "starting" {
            return;
        }
        let pid = s.pid;
        s.gen += 1;
        let fix = format!("See {}", self.log_path().display());
        Self::down(&mut s, "Voice did not answer in time.", &fix);
        if let Some(pid) = pid {
            // SAFETY: signals one child process.
            unsafe {
                libc::kill(pid, libc::SIGKILL);
            }
        }
    }

    fn on_exit(
        &self,
        gen: u64,
        pid: i32,
        st: std::io::Result<std::process::ExitStatus>,
        tries: u32,
    ) {
        let mut s = self.lock();
        if s.stopping || gen != s.gen || s.pid != Some(pid) {
            return;
        }
        let was = s.state.clone();
        let last = last_line(&self.log_path());
        let code = st.ok().and_then(|x| x.code()).unwrap_or(-1);
        let see = format!("See {}", self.log_path().display());
        if was == "ready" && !s.restarted {
            s.restarted = true;
            self.begin(&mut s, false);
        } else if was == "ready" {
            Self::down(&mut s, "Voice stopped twice.", &see);
        } else if last.contains("not installed") {
            Self::down(
                &mut s,
                "Voice needs daisugi's voice extra.",
                &format!("Install it with {INSTALL_VOICE}"),
            );
            s.command = INSTALL_VOICE.into();
        } else if last.to_lowercase().contains("address already in use") && tries == 0 {
            s.gen += 1;
            s.state = "starting".into();
            let g = s.gen;
            let me = self.me.upgrade().expect("voice");
            thread::spawn(move || me.start(g, false, tries + 1));
        } else {
            let reason = if last.is_empty() {
                format!("Voice stopped as it started (exit {code}).")
            } else {
                format!(
                    "Voice stopped as it started: {}.",
                    last.strip_suffix('.').unwrap_or(&last)
                )
            };
            Self::down(&mut s, &reason, &see);
        }
    }

    /// Ends the voice process: an interrupt first, then a kill when it has
    /// not ended within two seconds. Nothing starts after a stop.
    pub fn stop(&self) {
        let (pid, exited) = {
            let mut s = self.lock();
            s.stopping = true;
            (s.pid, s.exited.clone())
        };
        let (Some(pid), Some(exited)) = (pid, exited) else {
            return;
        };
        if exited.is_set() {
            return;
        }
        // SAFETY: signals one child process.
        unsafe {
            libc::kill(pid, libc::SIGINT);
        }
        if !exited.wait(Some(Duration::from_secs(2))) {
            // SAFETY: as above.
            unsafe {
                libc::kill(pid, libc::SIGKILL);
            }
            exited.wait(Some(Duration::from_secs(10)));
        }
    }
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "voice.status",
        Arc::new(|s: &Arc<Server>, _c, r: &Request| -> Response {
            proto::ok_resp(&r.id, s.voice.status())
        }),
    );
    let _ = s.handle(
        "voice.start",
        Arc::new(|s: &Arc<Server>, _c, r: &Request| -> Response {
            s.voice.ensure();
            proto::ok_resp(&r.id, s.voice.status())
        }),
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls_and_tokens() {
        assert!(loopback_url("http://127.0.0.1:9/"));
        assert!(loopback_url("http://localhost"));
        assert!(loopback_url("http://[::1]:8/x"));
        assert!(!loopback_url("http://10.0.0.1:9"));
        let t = new_token().unwrap();
        assert_eq!(t.len(), 43);
        assert!(t
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'_'));
        let c = VoiceConfig {
            args: vec!["--port=1".into()],
            ..Default::default()
        };
        assert!(refuse_config(&c).unwrap().0.contains("--port"));
    }
}
