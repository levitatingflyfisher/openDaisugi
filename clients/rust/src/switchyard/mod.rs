//! NVIDIA NeMo Switchyard as the gateway's external model chooser:
//! `opendaisugi.router_switchyard`. It finds the switchyard-server binary,
//! writes its TOML deployment file, starts and stops it as a managed child
//! on loopback, and probes its health. The gateway stays the meter. The Go
//! client's `switchyard` package is the reference.

pub mod toml;

use std::io::{Read, Write};
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, Stdio};
use std::sync::{mpsc, Mutex};
use std::time::{Duration, Instant};

use crate::gate::paths::strerror;
use crate::gate::py::text::{self, repr};
use crate::gate::pyjson::{self, loads_py, Object, Value};
use toml::{parse, NotRead, Table, Tv};

pub const BINARY_NAME: &str = "switchyard-server";
pub const PINNED_TAG: &str = "v0.3.0";
pub const INSTALL_CMD: &str = "cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard --tag v0.3.0 switchyard-server";
pub const HEALTH_PATH: &str = "/health";
pub const DEFAULT_HOST: &str = "127.0.0.1";
pub const FORWARD_LOGIN: &str = "forwards your own login, as the gateway does";
pub const NO_CREDENTIAL: &str = "no credential: a local host";
const ANTHROPIC_API: &str = "https://api.anthropic.com";
const DEFAULT_LOCAL: &str = "http://127.0.0.1:11434";

/// `shutil.which(name, path=path)`.
pub fn look_path(name: &str, path: &str) -> Option<String> {
    use std::os::unix::fs::PermissionsExt;
    for dir in path.split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let p = format!("{}/{name}", dir.trim_end_matches('/'));
        let Ok(st) = std::fs::metadata(&p) else { continue };
        if st.is_dir() || st.permissions().mode() & 0o111 == 0 {
            continue;
        }
        let Ok(c) = std::ffi::CString::new(p.clone()) else { continue };
        if unsafe { libc::access(c.as_ptr(), libc::X_OK) } != 0 {
            continue;
        }
        return Some(p);
    }
    None
}

/// `check_prerequisite`: None when the binary is found.
pub fn prerequisite_problem(binary: Option<&str>) -> Option<String> {
    if binary.is_some() {
        return None;
    }
    Some(format!(
        "{BINARY_NAME} is not on PATH. The Switchyard proxy is a Rust binary, not a pip package. It needs a Rust \
         toolchain: https://rust-lang.org/tools/install. Install it with: {INSTALL_CMD}"
    ))
}

/// Runs `argv` and returns its stdout, or None when it will not start or
/// runs past `limit` (it is killed then).
fn output_within(argv: &[&str], limit: Duration) -> Option<Vec<u8>> {
    let mut child = Command::new(argv[0])
        .args(&argv[1..])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let mut out = child.stdout.take()?;
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        let mut b = vec![];
        let _ = out.read_to_end(&mut b);
        let _ = tx.send(b);
    });
    let end = Instant::now() + limit;
    loop {
        match child.try_wait() {
            Ok(Some(_)) => break,
            Ok(None) if Instant::now() < end => std::thread::sleep(Duration::from_millis(10)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return None;
            }
        }
    }
    rx.recv_timeout(end.saturating_duration_since(Instant::now()).max(Duration::from_millis(100))).ok()
}

/// `binary_version`: `switchyard-server --version`, stripped, or None when
/// it will not run.
pub fn version(binary: Option<&str>) -> Option<String> {
    let b = binary?;
    let out = output_within(&[b, "--version"], Duration::from_secs(5))?;
    let v = text::strip(&String::from_utf8_lossy(&out)).to_string();
    (!v.is_empty()).then_some(v)
}

/// SwitchyardTargets.
#[derive(Debug, Clone, Default)]
pub struct Targets {
    pub capable_id: String,
    pub efficient_id: String,
    pub efficient_format: String,
    pub efficient_base_url: String,
    pub capable_format: String,
    pub capable_base_url: String,
    pub efficient_local: bool,
    /// "" for none.
    pub api_key_env: String,
}

/// The part of config.yaml the router reads.
#[derive(Debug, Clone, Default)]
pub struct Config {
    pub route_id: String,
    pub capable_model: String,
    pub efficient_model: Option<String>,
    pub api_key_env: Option<String>,
    pub llm_base_url: Option<String>,
    pub llm_host_kind: Option<String>,
}

fn openai_root(base: &str) -> String {
    let b = base.trim_end_matches('/');
    if b.ends_with("/v1") {
        b.to_string()
    } else {
        format!("{b}/v1")
    }
}

/// `targets_from_config`; None when no efficient model is set.
pub fn targets_from_config(c: &Config) -> Option<Targets> {
    let eff = c.efficient_model.clone().filter(|e| !e.is_empty())?;
    let mut t = Targets {
        capable_id: c.capable_model.clone(),
        efficient_id: eff.clone(),
        capable_format: "anthropic_messages".into(),
        capable_base_url: ANTHROPIC_API.into(),
        efficient_local: true,
        api_key_env: c.api_key_env.clone().unwrap_or_default(),
        ..Targets::default()
    };
    if eff.starts_with("claude-") {
        t.efficient_format = "anthropic_messages".into();
        t.efficient_base_url = ANTHROPIC_API.into();
        t.efficient_local = false;
        return Some(t);
    }
    let (base, kind) = match &c.llm_base_url {
        Some(b) if !b.is_empty() => (b.clone(), c.llm_host_kind.clone().unwrap_or_default()),
        _ => (DEFAULT_LOCAL.to_string(), "ollama".to_string()),
    };
    if kind == "anthropic" {
        t.efficient_format = "anthropic_messages".into();
        t.efficient_base_url = base.trim_end_matches('/').to_string();
    } else {
        t.efficient_format = "openai_chat".into();
        t.efficient_base_url = openai_root(&base);
    }
    Some(t)
}

fn toml_str(s: &str) -> String {
    pyjson::dumps(&Value::Str(s.into()), true)
}

fn key_mode(env: &str) -> String {
    format!("uses {env}: billed per token to that key")
}

/// How each tier authenticates upstream.
#[derive(Debug, Clone, PartialEq)]
pub struct Auth {
    pub capable: String,
    pub efficient: String,
}

/// `client_auth_modes`.
pub fn auth_modes(t: &Targets) -> Auth {
    let cloud = if t.api_key_env.is_empty() { FORWARD_LOGIN.to_string() } else { key_mode(&t.api_key_env) };
    let eff = if t.efficient_local { NO_CREDENTIAL.to_string() } else { cloud.clone() };
    Auth { capable: cloud, efficient: eff }
}

fn client_lines(name: &str, format: &str, base: &str, auth: &str, none: bool) -> Vec<String> {
    let mut lines = vec![format!("[llm_clients.{name}]"), format!("format = {}", toml_str(format)), format!("base_url = {}", toml_str(base))];
    if !none {
        if auth == "forward" {
            lines.push("forward_auth = true".into());
        } else {
            lines.push(format!("api_key_env = {}", toml_str(auth)));
        }
    }
    lines.push(String::new());
    lines
}

/// `render_switchyard_toml` with the default picker and threshold;
/// `key_present` checks a named key variable.
pub fn render(t: &Targets, route_id: &str, key_present: &dyn Fn(&str) -> bool) -> Result<String, String> {
    if route_id.is_empty() {
        return Err("the route id must not be empty".into());
    }
    if t.capable_id == t.efficient_id {
        return Err(format!(
            "the capable and efficient tiers name the same model {}. Pick a different efficient model.",
            repr(&t.capable_id)
        ));
    }
    if !t.api_key_env.is_empty() && !key_present(&t.api_key_env) {
        return Err(format!(
            "switchyard_api_key_env names {}, which is not set. Export it before the gateway starts, or clear the \
             field to forward your login.",
            t.api_key_env
        ));
    }
    let cloud = if t.api_key_env.is_empty() { "forward".to_string() } else { t.api_key_env.clone() };
    let mut lines: Vec<String> = vec!["schema_version = 1".into(), String::new()];
    lines.extend(client_lines("capable", &t.capable_format, &t.capable_base_url, &cloud, false));
    lines.extend(client_lines("efficient", &t.efficient_format, &t.efficient_base_url, &cloud, t.efficient_local));
    for (name, id) in [("capable", &t.capable_id), ("efficient", &t.efficient_id)] {
        lines.push(format!("[targets.{name}]"));
        lines.push(format!("id = {}", toml_str(id)));
        lines.push(format!("llm_client = \"{name}\""));
        lines.push(String::new());
    }
    lines.push("[routes.daisugi]".into());
    lines.push(format!("id = {}", toml_str(route_id)));
    lines.push("type = \"stage_router\"".into());
    lines.push("capable_target = \"capable\"".into());
    lines.push("efficient_target = \"efficient\"".into());
    lines.push("picker = \"efficient_first\"".into());
    lines.push("confidence_threshold = 0.5".into());
    Ok(lines.join("\n") + "\n")
}

/// `write_switchyard_config`: `<dir>/<name>`, mode 0600.
pub fn write_config(dir: &str, text: &str, name: &str) -> std::io::Result<String> {
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
    let path = format!("{dir}/{name}");
    std::fs::create_dir_all(dir)?;
    let mut f = std::fs::OpenOptions::new().write(true).create(true).truncate(true).mode(0o600).open(&path)?;
    f.write_all(text.as_bytes())?;
    drop(f);
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
    Ok(path)
}

/// str(OSError) for a file operation that failed on `path`.
pub fn py_os_error(e: &std::io::Error, path: &str) -> String {
    match e.raw_os_error() {
        Some(n) => format!("[Errno {n}] {}: {}", strerror(n), repr(path)),
        None => e.to_string(),
    }
}

/// Why a route could not be metered.
#[derive(Debug)]
pub enum RouteErr {
    /// SwitchyardConfigError: the file does not name a meterable route.
    Meter(String),
    /// A file outside this binary's TOML reader (GW-9).
    NotRead(NotRead),
}

const TIER_KEYS: [[&str; 2]; 2] = [["capable_target", "efficient_target"], ["strong_target", "weak_target"]];

fn read_toml(path: &str) -> Result<Result<Table, NotRead>, std::io::Error> {
    let raw = std::fs::read(path)?;
    match String::from_utf8(raw) {
        Ok(t) => Ok(parse(&t)),
        Err(_) => Ok(Err(NotRead("the file is not UTF-8".into()))),
    }
}

fn find_route<'a>(doc: &'a Table, route_id: &str) -> Option<&'a Table> {
    let routes = doc.table("routes")?;
    for k in &routes.keys {
        if let Some(Tv::Table(r)) = routes.get(k) {
            if r.str("id") == Some(route_id) {
                return Some(r);
            }
        }
    }
    None
}

fn tier_pair(route: &Table) -> Option<[&'static str; 2]> {
    TIER_KEYS.iter().find(|tk| route.get(tk[0]).is_some() && route.get(tk[1]).is_some()).copied()
}

/// `route_targets_from_toml`: the (capable, efficient) ids of the route.
pub fn route_targets(path: &str, route_id: &str) -> Result<(String, String), RouteErr> {
    let doc = match read_toml(path) {
        Err(e) => return Err(RouteErr::Meter(format!("cannot read {path}: {}", py_os_error(&e, path)))),
        Ok(Err(nr)) => return Err(RouteErr::NotRead(nr)),
        Ok(Ok(d)) => d,
    };
    let no_route = || RouteErr::Meter(format!("{path} has no route with id {}", repr(route_id)));
    if doc.table("routes").is_none() || doc.table("targets").is_none() {
        return Err(no_route());
    }
    let route = find_route(&doc, route_id).ok_or_else(no_route)?;
    let pair = tier_pair(route).ok_or_else(|| {
        RouteErr::Meter(format!(
            "route {} in {path} does not name two tiers. The gateway meters capable_target and efficient_target, \
             or strong_target and weak_target.",
            repr(route_id)
        ))
    })?;
    let targets = doc.table("targets").expect("checked above");
    let mut ids = vec![];
    for key in pair {
        let name = route.get(key);
        let id = match name {
            Some(Tv::Str(s)) => targets.table(s).and_then(|t| t.str("id")),
            _ => None,
        };
        match id {
            Some(i) => ids.push(i.to_string()),
            None => {
                return Err(RouteErr::Meter(format!(
                    "route {} in {path} names target {}, which is missing",
                    repr(route_id),
                    tv_repr(name.unwrap_or(&Tv::Bool(false)), name.is_none())
                )))
            }
        }
    }
    if ids[0] == ids[1] {
        return Err(RouteErr::Meter(format!(
            "route {} in {path} uses the model {} for both tiers",
            repr(route_id),
            repr(&ids[0])
        )));
    }
    Ok((ids[0].clone(), ids[1].clone()))
}

/// `repr()` of a TOML value; `none` gives "None".
fn tv_repr(v: &Tv, none: bool) -> String {
    if none {
        return "None".into();
    }
    match v {
        Tv::Str(s) => repr(s),
        Tv::Bool(true) => "True".into(),
        Tv::Bool(false) => "False".into(),
        Tv::Int(n) => n.to_string(),
        Tv::Float(f) => pyjson::py_float_repr(*f),
        Tv::Arr(a) => format!("[{}]", a.iter().map(|x| tv_repr(x, false)).collect::<Vec<_>>().join(", ")),
        Tv::Table(t) => format!(
            "{{{}}}",
            t.keys.iter().map(|k| format!("{}: {}", repr(k), tv_repr(&t.vals[k], false))).collect::<Vec<_>>().join(", ")
        ),
    }
}

fn tv_truthy(v: &Tv) -> bool {
    match v {
        Tv::Str(s) => !s.is_empty(),
        Tv::Bool(b) => *b,
        Tv::Int(n) => *n != 0,
        Tv::Float(f) => *f != 0.0,
        Tv::Arr(a) => !a.is_empty(),
        Tv::Table(t) => !t.keys.is_empty(),
    }
}

fn tv_str(v: &Tv) -> String {
    match v {
        Tv::Str(s) => s.clone(),
        _ => tv_repr(v, false),
    }
}

/// `client_auth_from_toml`; None when the file or the route cannot be read.
pub fn auth_from_toml(path: &str, route_id: &str) -> Option<Auth> {
    let doc = read_toml(path).ok()?.ok()?;
    let Some(Tv::Table(routes)) = doc.get("routes") else { return None };
    let mut route = None;
    for k in &routes.keys {
        let Some(Tv::Table(r)) = routes.get(k) else { return None };
        if r.str("id") == Some(route_id) {
            route = Some(r);
            break;
        }
    }
    let route = route?;
    let empty = Table::default();
    let clients = match doc.get("llm_clients") {
        None => &empty,
        Some(Tv::Table(t)) => t,
        Some(_) => return None,
    };
    let pair = tier_pair(route)?;
    let Some(Tv::Table(targets)) = doc.get("targets") else { return None };
    let mut modes = vec![];
    for key in pair {
        let Some(Tv::Str(name)) = route.get(key) else { return None };
        let Some(Tv::Table(target)) = targets.get(name) else { return None };
        let Some(Tv::Str(cname)) = target.get("llm_client") else { return None };
        let client = match clients.get(cname) {
            None => &empty,
            Some(Tv::Table(c)) => c,
            Some(_) => return None,
        };
        let mode = match client.get("api_key_env") {
            Some(env) if tv_truthy(env) => key_mode(&tv_str(env)),
            _ if client.get("forward_auth") == Some(&Tv::Bool(true)) => FORWARD_LOGIN.to_string(),
            _ => "no credential".to_string(),
        };
        modes.push(mode);
    }
    Some(Auth { capable: modes[0].clone(), efficient: modes[1].clone() })
}

/// One child's per-port files.
pub struct Files {
    pub config: String,
    pub log: String,
    pub routing_log: String,
}

/// `child_files_for`.
pub fn child_files(data_dir: &str, port: i64) -> Files {
    let gw = format!("{data_dir}/gateway");
    Files {
        config: format!("{gw}/switchyard-{port}.toml"),
        log: format!("{gw}/switchyard-{port}.log"),
        routing_log: format!("{gw}/switchyard-routing-{port}.jsonl"),
    }
}

/// `state_path_for`.
pub fn state_path(data_dir: &str, port: i64) -> String {
    format!("{data_dir}/gateway/switchyard-{port}.json")
}

/// A state file as `read_state` reads it.
#[derive(Debug, Clone)]
pub struct State {
    pub obj: Object,
    pub pid: i64,
    pub host: String,
    pub port: i64,
}

/// `read_state`: None when the file is absent, not JSON, or pid, host or
/// port has the wrong type.
pub fn read_state(path: &str) -> Option<State> {
    let raw = std::fs::read(path).ok()?;
    let t = String::from_utf8(raw).ok()?;
    let Value::Obj(o) = loads_py(&t, 9990).ok()? else { return None };
    let (Value::Int(pid), Value::Int(port), Value::Str(host)) = (o.value("pid"), o.value("port"), o.value("host")) else {
        return None;
    };
    // A pid or port past 64 bits names no process; read as the oracle reads
    // it, the state is valid but its pid is not running.
    let (pn, qn) = match (pid.parse::<i64>(), port.parse::<i64>()) {
        (Ok(a), Ok(b)) => (a, b),
        _ => (i64::MAX, i64::MAX),
    };
    let host = host.clone();
    Some(State { obj: o, pid: pn, host, port: qn })
}

/// One listed state file.
pub struct StateFile {
    pub path: String,
    pub state: Option<State>,
}

/// `list_states`: every gateway/switchyard-*.json, sorted.
pub fn list_states(data_dir: &str) -> Vec<StateFile> {
    let dir = format!("{data_dir}/gateway");
    let Ok(rd) = std::fs::read_dir(&dir) else { return vec![] };
    let mut paths: Vec<String> = rd
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().to_str().map(String::from))
        .filter(|n| n.starts_with("switchyard-") && n.ends_with(".json"))
        .map(|n| format!("{dir}/{n}"))
        .collect();
    paths.sort();
    paths.into_iter().map(|p| StateFile { state: read_state(&p), path: p }).collect()
}

fn host_port(host: &str, port: i64) -> String {
    if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

/// `probe_health`: GET /health answers 200 within 0.5 s. The request is
/// urllib's, as the oracle's probe sends it, through a proxy as urllib
/// picks one.
pub fn probe_health(host: &str, port: i64) -> bool {
    let Ok(port) = u16::try_from(port) else { return false };
    let bare = host.trim_start_matches('[').trim_end_matches(']').to_string();
    let origin = crate::netproxy::Origin { tls: false, host: bare, port, explicit_port: true };
    let Ok(route) = crate::netproxy::process_urllib().route(&origin) else { return false };
    let dial = crate::netproxy::Dial { limit: Some((Instant::now(), Duration::from_millis(500))), connect_timeout: None, roots: None };
    let Ok(mut c) = crate::netproxy::open(&route, &origin, &dial) else { return false };
    let hp = host_port(host, port as i64);
    let target = crate::netproxy::request_target(&c, &origin, HEALTH_PATH);
    let req = format!(
        "GET {target} HTTP/1.1\r\nAccept-Encoding: identity\r\nHost: {hp}\r\nUser-Agent: Python-urllib/3.12\r\n{}\
         Connection: close\r\n\r\n",
        crate::netproxy::auth_line(&c)
    );
    if c.io.write_all(req.as_bytes()).is_err() {
        return false;
    }
    let mut head = vec![];
    let mut buf = [0u8; 1024];
    while !head.windows(2).any(|w| w == b"\r\n") {
        match c.io.read(&mut buf) {
            Ok(0) | Err(_) => return false,
            Ok(n) => head.extend_from_slice(&buf[..n]),
        }
    }
    let line = String::from_utf8_lossy(&head);
    line.split(' ').nth(1) == Some("200")
}

fn proc_cmdline(pid: i64) -> Option<String> {
    let raw = std::fs::read(format!("/proc/{pid}/cmdline")).ok()?;
    let raw: Vec<u8> = raw.into_iter().map(|b| if b == 0 { b' ' } else { b }).collect();
    Some(text::strip(&String::from_utf8_lossy(&raw)).to_string())
}

fn kill_zero(pid: i64) -> bool {
    if pid <= 0 || pid > i32::MAX as i64 {
        return false;
    }
    let r = unsafe { libc::kill(pid as i32, 0) };
    r == 0 || std::io::Error::last_os_error().raw_os_error() == Some(libc::EPERM)
}

/// `child_is_running`.
pub fn child_is_running(pid: i64) -> bool {
    match proc_cmdline(pid) {
        Some(line) => line.contains(BINARY_NAME),
        None => kill_zero(pid),
    }
}

/// `stop_switchyard` for a state file this process did not start.
pub fn stop(state_file: &str) -> String {
    if std::fs::metadata(state_file).is_err() {
        return "no switchyard-server state file; nothing to stop".into();
    }
    let st = read_state(state_file);
    let _ = std::fs::remove_file(state_file);
    let Some(st) = st else { return format!("the state file {state_file} was unreadable; removed it") };
    let pid = st.pid;
    let line = proc_cmdline(pid);
    if line.as_deref() == Some("") {
        return format!("switchyard-server, pid {pid}, already exited");
    }
    if line.as_ref().is_some_and(|l| !l.contains(BINARY_NAME)) {
        return format!("pid {pid} is not switchyard-server now; sent no signal and removed the state");
    }
    if pid <= 0 || pid > i32::MAX as i64 {
        return format!("switchyard-server, pid {pid}, was already stopped");
    }
    if unsafe { libc::kill(pid as i32, libc::SIGTERM) } != 0 {
        if std::io::Error::last_os_error().raw_os_error() == Some(libc::EPERM) {
            return format!("no permission to signal pid {pid}; sent no signal and removed the state");
        }
        return format!("switchyard-server, pid {pid}, was already stopped");
    }
    format!("sent SIGTERM to switchyard-server, pid {pid}")
}

/// A child that answered its health check.
pub struct Handle {
    pub pid: u32,
    pub host: String,
    pub port: i64,
    pub config_path: String,
    pub log_path: String,
    child: Mutex<Child>,
}

impl Handle {
    pub fn base_url(&self) -> String {
        format!("http://{}:{}", self.host, self.port)
    }

    /// Whether the child still runs. A child that ended is reaped here, so
    /// its pid is signalled only while it cannot name another process.
    fn alive(&self) -> bool {
        match self.child.lock() {
            Ok(mut c) => matches!(c.try_wait(), Ok(None)),
            Err(_) => false,
        }
    }

    fn signal(&self, sig: i32) -> bool {
        let Ok(mut c) = self.child.lock() else { return false };
        if !matches!(c.try_wait(), Ok(None)) {
            return false;
        }
        unsafe { libc::kill(self.pid as i32, sig) == 0 }
    }

    /// `stop_own_child` with a five-second wait: the child this gateway
    /// started, by its own handle. The state file goes only while it still
    /// names this pid.
    pub fn stop_own(&self, state_file: &str) -> String {
        if read_state(state_file).is_some_and(|st| st.pid == self.pid as i64) {
            let _ = std::fs::remove_file(state_file);
        }
        let sent = format!("sent SIGTERM to switchyard-server, pid {}", self.pid);
        if !self.alive() || !self.signal(libc::SIGTERM) {
            return sent;
        }
        let end = Instant::now() + Duration::from_secs(5);
        while self.alive() {
            if Instant::now() >= end {
                return format!("{sent}; it still drains");
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        sent
    }
}

/// Starts `argv` in its own session with its output on `log_path`. The
/// child gets SIGTERM from the kernel when the thread that forked it ends,
/// so the fork runs on a thread that lives as long as the process.
fn spawn(argv: &[String], log_path: &str) -> std::io::Result<Child> {
    let (tx, rx) = mpsc::channel();
    let argv = argv.to_vec();
    let log_path = log_path.to_string();
    std::thread::spawn(move || {
        let r = (|| {
            let mut cmd = Command::new(&argv[0]);
            cmd.args(&argv[1..]).stdin(Stdio::null());
            if !log_path.is_empty() {
                if let Some(d) = std::path::Path::new(&log_path).parent() {
                    let _ = std::fs::create_dir_all(d);
                }
                let f = std::fs::OpenOptions::new().append(true).create(true).open(&log_path)?;
                cmd.stdout(f.try_clone()?).stderr(f);
            }
            let parent = std::process::id() as i32;
            unsafe {
                cmd.pre_exec(move || {
                    if libc::setsid() < 0 {
                        return Err(std::io::Error::last_os_error());
                    }
                    if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM as libc::c_ulong, 0, 0, 0) != 0 {
                        return Err(std::io::Error::last_os_error());
                    }
                    // The parent may have died before the death signal was set.
                    if libc::getppid() != parent {
                        libc::_exit(0);
                    }
                    Ok(())
                });
            }
            cmd.spawn()
        })();
        let _ = tx.send(r);
        // This thread forked the child: it lives as long as the process.
        loop {
            std::thread::park();
        }
    });
    rx.recv().unwrap_or_else(|_| Err(std::io::Error::other("the spawning thread ended")))
}

fn log_tail(log_path: &str) -> String {
    if log_path.is_empty() {
        return String::new();
    }
    let Ok(raw) = std::fs::read(log_path) else { return String::new() };
    let t = String::from_utf8_lossy(&raw).into_owned();
    let lines: Vec<String> =
        text::splitlines(&t).into_iter().filter(|l| !text::strip(l).is_empty()).map(|l| format!("    {l}")).collect();
    let from = lines.len().saturating_sub(5);
    lines[from..].join("\n")
}

/// `start_switchyard`'s arguments.
pub struct StartOptions {
    pub config_path: String,
    pub binary: String,
    pub routing_log: String,
    pub log_path: String,
    pub state_path: String,
    pub route_id: String,
    pub port: i64,
    pub auth: Option<Auth>,
    pub wait: Duration,
    pub kill_grace: Duration,
}

/// `format(x, "g")` of a whole number of seconds and the like.
fn py_g(x: f64) -> String {
    if x.fract() == 0.0 && x.abs() < 1e16 {
        format!("{}", x as i64)
    } else {
        format!("{x}")
    }
}

/// `start_switchyard`: the child on 127.0.0.1, healthy, or None and the
/// reason.
pub fn start(o: &StartOptions) -> (Option<Handle>, String) {
    let host = DEFAULT_HOST;
    let resolved = if o.binary.is_empty() { BINARY_NAME.to_string() } else { o.binary.clone() };
    if probe_health(host, o.port) {
        return (
            None,
            format!(
                "something already answers on http://{host}:{}{HEALTH_PATH}. If it is a switchyard-server a gateway \
                 started earlier, stop it with `daisugi router stop`. Else pick another port with --switchyard-port.",
                o.port
            ),
        );
    }
    let mut argv = vec![resolved.clone(), "--config".into(), o.config_path.clone(), "--host".into(), host.into(), "--port".into(), o.port.to_string()];
    if !o.routing_log.is_empty() {
        argv.push("--routing-log-file".into());
        argv.push(o.routing_log.clone());
    }
    let shown = argv.join(" ");
    let dry_run = format!("{resolved} --config {} --dry-run", o.config_path);
    let child = match spawn(&argv, &o.log_path) {
        Ok(c) => c,
        Err(e) => return (None, format!("could not start `{shown}`: {}", py_os_error(&e, &resolved))),
    };
    let h = Handle {
        pid: child.id(),
        host: host.into(),
        port: o.port,
        config_path: o.config_path.clone(),
        log_path: o.log_path.clone(),
        child: Mutex::new(child),
    };
    let t0 = Instant::now();
    loop {
        if probe_health(host, o.port) {
            if !o.state_path.is_empty() {
                write_state(&o.state_path, &h, &o.route_id, o.auth.as_ref());
            }
            let msg = format!("started `{shown}`, pid {}, healthy on {}", h.pid, h.base_url());
            return (Some(h), msg);
        }
        if !h.alive() {
            let tail = log_tail(&o.log_path);
            let place = if tail.is_empty() { String::new() } else { format!(" Its log is {}:\n{tail}", o.log_path) };
            return (
                None,
                format!("`{shown}`, pid {}, exited before it answered.{place}\n  Check the config with: {dry_run}", h.pid),
            );
        }
        if t0.elapsed() >= o.wait {
            break;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    h.signal(libc::SIGTERM);
    let grace = Instant::now() + o.kill_grace;
    while h.alive() && Instant::now() < grace {
        std::thread::sleep(Duration::from_millis(50));
    }
    if h.alive() {
        h.signal(libc::SIGKILL);
    }
    let place = if o.log_path.is_empty() { String::new() } else { format!(" Its log is {}.", o.log_path) };
    (
        None,
        format!(
            "`{shown}`, pid {}, did not answer {HEALTH_PATH} in {}s, so it was stopped.{place}\n  Check the config with: {dry_run}",
            h.pid,
            py_g(o.wait.as_secs_f64())
        ),
    )
}

fn write_state(path: &str, h: &Handle, route_id: &str, auth: Option<&Auth>) {
    if let Some(d) = std::path::Path::new(path).parent() {
        let _ = std::fs::create_dir_all(d);
    }
    let mut o = Object::new();
    o.set("pid", h.pid as i64);
    o.set("host", h.host.as_str());
    o.set("port", h.port);
    o.set("config_path", h.config_path.as_str());
    o.set("log_path", if h.log_path.is_empty() { Value::Null } else { Value::Str(h.log_path.clone()) });
    o.set("route_id", if route_id.is_empty() { Value::Null } else { Value::Str(route_id.into()) });
    o.set(
        "auth",
        match auth {
            Some(a) => {
                let mut x = Object::new();
                x.set("capable", a.capable.as_str());
                x.set("efficient", a.efficient.as_str());
                Value::Obj(x)
            }
            None => Value::Null,
        },
    );
    let _ = std::fs::write(path, pyjson::dumps(&Value::Obj(o), true));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn route_targets_and_auth_read_the_rendered_file() {
        let dir = std::env::temp_dir().join(format!("sy-test-{}", std::process::id()));
        let dir = dir.to_str().unwrap().to_string();
        let t = targets_from_config(&Config {
            route_id: "daisugi".into(),
            capable_model: "claude-sonnet-5".into(),
            efficient_model: Some("llama3".into()),
            ..Config::default()
        })
        .unwrap();
        let text = render(&t, "daisugi", &|_| true).unwrap();
        let p = write_config(&dir, &text, "s.toml").unwrap();
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(std::fs::metadata(&p).unwrap().permissions().mode() & 0o777, 0o600);
        let (c, e) = route_targets(&p, "daisugi").unwrap();
        assert_eq!((c.as_str(), e.as_str()), ("claude-sonnet-5", "llama3"));
        let a = auth_from_toml(&p, "daisugi").unwrap();
        assert_eq!((a.capable.as_str(), a.efficient.as_str()), (FORWARD_LOGIN, "no credential"));
        assert!(route_targets(&format!("{dir}/none.toml"), "daisugi").is_err());
        let same = Targets { capable_id: "m".into(), efficient_id: "m".into(), ..Targets::default() };
        assert!(render(&same, "d", &|_| true).is_err());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
