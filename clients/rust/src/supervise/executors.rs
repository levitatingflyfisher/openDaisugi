//! `executor.py`: the step executors a supervised run hands steps to.
//! Each returns what `ExecutorResult` holds; an `Err` is an exception
//! from the executor, which the supervisor records as a failed step.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::process::CommandExt;
use std::time::{Duration, Instant};

use super::{ms, str_of, ExecResult};
use crate::gate::py::text::{decode_utf8_replace, repr, repr_list};
use crate::gate::pyjson::{py_repr, Object, Value};

/// `executor.StepExecutor`.
pub trait Executor {
    /// Runs one step. `configure` has been called with the envelope first.
    fn run(&mut self, step: &Object, timeout_s: u64, max_output_bytes: usize) -> Result<ExecResult, String>;
    /// `configure_from_envelope`, for an executor that has one.
    fn configure(&mut self, _env: &Object) {}
    /// A truthy `parallel_safe`: this executor's steps may run at once
    /// (the step types of `_PARALLEL_SAFE_TYPES` are safe whatever this
    /// says).
    fn parallel_safe(&self) -> bool {
        false
    }
    /// The step's run as a job another thread may take, for an executor
    /// whose steps may run at once; None runs it in order instead.
    fn job(&mut self, _step: &Object, _timeout_s: u64, _max_output_bytes: usize) -> Option<Job> {
        None
    }
    /// Called in order, on this thread, with a job's result.
    fn job_done(&mut self, _step: &Object, _res: &ExecResult) -> Result<(), String> {
        Ok(())
    }
}

/// One step's run, to be made on another thread.
pub type Job = Box<dyn FnOnce() -> Result<ExecResult, String> + Send>;

/// `executor.MAX_REVERSAL_BYTES`.
const MAX_REVERSAL_BYTES: usize = 1_000_000;

/// The OSError subclass Python raises for an errno.
fn exc_name(errno: i32) -> &'static str {
    match errno {
        libc::ENOENT => "FileNotFoundError",
        libc::EISDIR => "IsADirectoryError",
        libc::ENOTDIR => "NotADirectoryError",
        libc::EACCES | libc::EPERM => "PermissionError",
        libc::EEXIST => "FileExistsError",
        libc::ECONNREFUSED => "ConnectionRefusedError",
        libc::ECONNRESET => "ConnectionResetError",
        _ => "OSError",
    }
}

/// `os.strerror(errno)`.
pub fn strerror(errno: i32) -> String {
    unsafe {
        let p = libc::strerror(errno);
        if p.is_null() {
            return format!("Unknown error {errno}");
        }
        std::ffi::CStr::from_ptr(p).to_string_lossy().into_owned()
    }
}

/// `(type(e).__name__, str(e))` for an OSError on `path`, or None for an
/// error that carries no errno.
pub fn os_error(e: &std::io::Error, path: &str) -> Option<(&'static str, String)> {
    let errno = e.raw_os_error()?;
    Some((exc_name(errno), format!("[Errno {errno}] {}: {}", strerror(errno), repr(path))))
}

fn done(rc: i32, stdout: impl Into<String>, start: Instant) -> ExecResult {
    ExecResult { rc, stdout: stdout.into(), duration_ms: ms(start), ..Default::default() }
}

/// `executor.DryRunExecutor`.
pub struct DryRun;

impl Executor for DryRun {
    fn run(&mut self, step: &Object, _: u64, _: usize) -> Result<ExecResult, String> {
        let r = |v: &Value| py_repr(v).unwrap_or_default();
        let msg = match str_of(step, "type").as_str() {
            "shell" => format!("[dry-run] would shell: {}", repr(&str_of(step, "command"))),
            "file_read" => format!("[dry-run] would file_read: {}", str_of(step, "path")),
            "file_write" => {
                format!("[dry-run] would file_write: {} ({} bytes)", str_of(step, "path"), str_of(step, "content").len())
            }
            "network" => format!("[dry-run] would network: GET {}", str_of(step, "url")),
            "task" => format!("[dry-run] would task (delegate to LLM): {}", repr(&str_of(step, "prompt"))),
            "skill" => format!("[dry-run] would skill: {} input={}", str_of(step, "skill_id"), r(step.value("skill_input"))),
            "mcp" => format!(
                "[dry-run] would mcp: {}/{} args={}",
                str_of(step, "server"),
                str_of(step, "tool"),
                r(step.value("arguments"))
            ),
            "agentic" => format!(
                "[dry-run] would agentic: {} in {} tools={}",
                repr(&str_of(step, "prompt")),
                str_of(step, "workspace"),
                r(step.value("tools"))
            ),
            _ => format!("[dry-run] unknown step kind: {}", repr(&str_of(step, "type"))),
        };
        Ok(ExecResult { rc: 0, stdout: msg, ..Default::default() })
    }
}

/// `executor._glob_base`.
fn glob_base(glob: &str) -> String {
    let cut = glob.find(['*', '?', '[']);
    let Some(cut) = cut else { return glob.to_string() };
    let prefix = &glob[..cut];
    let base = match prefix.rfind('/') {
        Some(i) => &prefix[..i],
        None => prefix,
    };
    if base.is_empty() {
        "/".into()
    } else {
        base.to_string()
    }
}

/// `os.path.abspath`.
fn abspath(p: &str) -> String {
    let joined = if p.starts_with('/') {
        p.to_string()
    } else {
        match std::env::current_dir() {
            Ok(d) => format!("{}/{p}", d.to_string_lossy()),
            Err(_) => p.to_string(),
        }
    };
    normpath(&joined)
}

/// `os.path.normpath` of an absolute path.
fn normpath(p: &str) -> String {
    let mut parts: Vec<&str> = vec![];
    for c in p.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            c => parts.push(c),
        }
    }
    format!("/{}", parts.join("/"))
}

/// `os.path.realpath` (not strict): symlinks resolved as far as the path
/// exists, the rest joined as written.
fn realpath(p: &str) -> String {
    let abs = abspath(p);
    if let Ok(r) = std::fs::canonicalize(&abs) {
        return r.to_string_lossy().into_owned();
    }
    let mut dir = abs.clone();
    let mut rest = String::new();
    loop {
        let (parent, name) = match dir.rfind('/') {
            Some(0) => ("/".to_string(), dir[1..].to_string()),
            Some(i) => (dir[..i].to_string(), dir[i + 1..].to_string()),
            None => return abs,
        };
        rest = if rest.is_empty() { name } else { format!("{name}/{rest}") };
        if parent == dir {
            return abs;
        }
        dir = parent;
        if let Ok(r) = std::fs::canonicalize(&dir) {
            let r = r.to_string_lossy().into_owned();
            return if r == "/" { format!("/{rest}") } else { format!("{r}/{rest}") };
        }
        if dir == "/" {
            return abs;
        }
    }
}

/// `executor._resolved_path_escape`.
fn resolved_path_escape(target: &str, globs: &[String]) -> Option<String> {
    let real = realpath(target);
    for g in globs {
        let base = realpath(&glob_base(g));
        let prefix = format!("{}/", base.trim_end_matches('/'));
        if real == base || real.starts_with(&prefix) {
            return None;
        }
    }
    Some(format!("resolved path {} is outside permitted globs {} (symlink escape)", repr(&real), repr_list(globs)))
}

fn globs_of(env: &Object, key: &str) -> Vec<String> {
    match env.value("permissions") {
        Value::Obj(p) => match p.value(key) {
            Value::List(l) => l.iter().map(|g| g.as_str().unwrap_or("").to_string()).collect(),
            _ => vec![],
        },
        _ => vec![],
    }
}

/// `executor.FileReadExecutor`.
#[derive(Default, Clone)]
pub struct FileRead {
    globs: Option<Vec<String>>,
}

impl Executor for FileRead {
    fn job(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Option<Job> {
        let mut me = self.clone();
        let step = step.clone();
        Some(Box::new(move || me.run(&step, timeout_s, max_out)))
    }
    fn configure(&mut self, env: &Object) {
        self.globs = Some(globs_of(env, "file_read"));
    }

    fn run(&mut self, step: &Object, _: u64, max_out: usize) -> Result<ExecResult, String> {
        let start = Instant::now();
        let path = str_of(step, "path");
        if let Some(g) = &self.globs {
            if let Some(esc) = resolved_path_escape(&path, g) {
                return Ok(done(2, format!("file_read refused: {esc}"), start));
            }
        }
        match read_capped(&path, max_out) {
            Ok((buf, truncated)) => {
                let mut out = decode_utf8_replace(&buf);
                if truncated {
                    out.push_str("\n... [truncated]");
                }
                Ok(done(0, out, start))
            }
            Err(e) => match os_error(&e, &path) {
                Some((name @ ("FileNotFoundError" | "IsADirectoryError" | "PermissionError"), text)) => {
                    Ok(done(1, format!("{name}: {text}"), start))
                }
                Some((_, text)) => Err(text),
                None => Err(e.to_string()),
            },
        }
    }
}

/// FileReadExecutor's chunked read: at most `max_out` bytes, and whether
/// there was more. A directory fails with EISDIR, as open() does.
fn read_capped(path: &str, max_out: usize) -> std::io::Result<(Vec<u8>, bool)> {
    let mut f = std::fs::File::open(path)?;
    if f.metadata().map(|m| m.is_dir()).unwrap_or(false) {
        return Err(std::io::Error::from_raw_os_error(libc::EISDIR));
    }
    let mut buf = Vec::new();
    let mut chunk = vec![0u8; 64 * 1024];
    loop {
        let want = (64 * 1024).min(max_out + 1 - buf.len().min(max_out + 1)).max(1);
        let n = f.read(&mut chunk[..want])?;
        buf.extend_from_slice(&chunk[..n]);
        if buf.len() > max_out {
            buf.truncate(max_out);
            return Ok((buf, true));
        }
        if n == 0 {
            return Ok((buf, false));
        }
    }
}

/// `os.path.dirname`.
pub fn py_dirname(p: &str) -> String {
    let i = p.rfind('/').map(|i| i + 1).unwrap_or(0);
    let head = &p[..i];
    if !head.is_empty() && head.chars().any(|c| c != '/') {
        head.trim_end_matches('/').to_string()
    } else {
        head.to_string()
    }
}

/// `executor._dirs_to_create`.
fn dirs_to_create(parent: &str) -> Vec<Value> {
    let mut dirs = vec![];
    let mut p = abspath(parent);
    loop {
        if std::fs::metadata(&p).map(|m| m.is_dir()).unwrap_or(false) {
            break;
        }
        dirs.push(Value::Str(p.clone()));
        let next = match p.rfind('/') {
            Some(0) => "/".to_string(),
            Some(i) => p[..i].to_string(),
            None => break,
        };
        if next == p {
            break;
        }
        p = next;
    }
    dirs
}

fn reversal_handle(path: &str, existed: bool, prior: Option<String>, created: Vec<Value>) -> Object {
    Object::new()
        .with("kind", "file_write")
        .with("path", path)
        .with("prior_existed", existed)
        .with("prior_content", prior.map(Value::Str).unwrap_or(Value::Null))
        .with("created_dirs", Value::List(created))
        .with("note", "")
}

/// `FileWriteExecutor._capture_pre_image`: the reversal handle, or None
/// when a write to `path` could not be undone.
pub fn capture_pre_image(path: &str, parent: &str) -> Option<Object> {
    let created = dirs_to_create(parent);
    if std::fs::metadata(path).is_err() {
        return Some(reversal_handle(path, false, None, created));
    }
    let f = std::fs::File::open(path).ok()?;
    let mut raw = vec![];
    f.take(MAX_REVERSAL_BYTES as u64 + 1).read_to_end(&mut raw).ok()?;
    if raw.len() > MAX_REVERSAL_BYTES {
        return None;
    }
    let prior = String::from_utf8(raw).ok()?;
    Some(reversal_handle(path, true, Some(prior), vec![]))
}

/// `executor.FileWriteExecutor`.
#[derive(Default, Clone)]
pub struct FileWrite {
    globs: Option<Vec<String>>,
}

impl Executor for FileWrite {
    fn job(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Option<Job> {
        let mut me = self.clone();
        let step = step.clone();
        Some(Box::new(move || me.run(&step, timeout_s, max_out)))
    }
    fn configure(&mut self, env: &Object) {
        self.globs = Some(globs_of(env, "file_write"));
    }

    fn run(&mut self, step: &Object, _: u64, _: usize) -> Result<ExecResult, String> {
        let start = Instant::now();
        let path = str_of(step, "path");
        let refused = |msg: String| {
            Ok(ExecResult { rc: 2, stdout: msg, duration_ms: ms(start), reversibility: Some("none".into()), ..Default::default() })
        };
        if let Some(g) = &self.globs {
            if let Some(esc) = resolved_path_escape(&path, g) {
                return refused(format!("file_write refused: {esc}"));
            }
        }
        let mut parent = py_dirname(&path);
        if parent.is_empty() {
            parent = ".".into();
        }
        if std::fs::symlink_metadata(&path).map(|m| m.file_type().is_symlink()).unwrap_or(false) {
            return refused(format!("symlink at target rejected: {path}"));
        }
        let os_err = |e: &std::io::Error, target: &str, tmp: Option<&str>| {
            if let Some(t) = tmp {
                let _ = std::fs::remove_file(t);
            }
            let (name, text) = os_error(e, target).unwrap_or(("OSError", e.to_string()));
            Ok(ExecResult {
                rc: 1,
                stdout: format!("{name}: {text}"),
                duration_ms: ms(start),
                reversibility: Some("irreversible".into()),
                ..Default::default()
            })
        };
        let reversal = capture_pre_image(&path, &parent);
        if let Err(e) = mkdir_all(&parent) {
            return os_err(&e.1, &e.0, None);
        }
        let rb = crate::gate::random_hex(8).unwrap_or_else(|_| "0000000000000000".into());
        let base = path.rsplit('/').next().unwrap_or(&path).to_string();
        let tmp = format!("{}/.daisugi-tmp-{rb}-{base}", parent.trim_end_matches('/'));
        let tmp = if parent == "/" { format!("/.daisugi-tmp-{rb}-{base}") } else { tmp };
        let f = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o644)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(&tmp);
        let mut f = match f {
            Ok(f) => f,
            Err(e) => return os_err(&e, &tmp, None),
        };
        let data = str_of(step, "content").into_bytes();
        let w = f.write_all(&data).and_then(|_| f.sync_all());
        drop(f);
        if let Err(e) = w {
            return os_err(&e, &tmp, Some(&tmp));
        }
        if let Err(e) = std::fs::rename(&tmp, &path) {
            let _ = std::fs::remove_file(&tmp);
            let (name, text) = match e.raw_os_error() {
                Some(n) => (exc_name(n), format!("[Errno {n}] {}: {} -> {}", strerror(n), repr(&tmp), repr(&path))),
                None => ("OSError", e.to_string()),
            };
            return Ok(ExecResult {
                rc: 1,
                stdout: format!("{name}: {text}"),
                duration_ms: ms(start),
                reversibility: Some("irreversible".into()),
                ..Default::default()
            });
        }
        let parent_real = realpath(&parent);
        let final_real = realpath(&path);
        if !(final_real == parent_real || final_real.starts_with(&format!("{parent_real}/"))) {
            let _ = std::fs::remove_file(&final_real);
            return Ok(ExecResult {
                rc: 2,
                stdout: format!("symlink escape detected: {path} -> {final_real}"),
                duration_ms: ms(start),
                reversibility: Some("irreversible".into()),
                ..Default::default()
            });
        }
        let mut res = done(0, format!("wrote {} bytes to {path}", data.len()), start);
        match reversal {
            Some(r) => {
                res.reversibility = Some("reversible".into());
                res.reversal = Some(r);
            }
            None => res.reversibility = Some("irreversible".into()),
        }
        Ok(res)
    }
}

/// `os.makedirs(parent, exist_ok=True)`: the path that failed and why.
fn mkdir_all(parent: &str) -> Result<(), (String, std::io::Error)> {
    if std::fs::metadata(parent).map(|m| m.is_dir()).unwrap_or(false) {
        return Ok(());
    }
    // Make each missing ancestor in turn, so the error names the one that
    // failed, as makedirs does.
    let abs = parent.to_string();
    let mut todo = vec![];
    let mut p = abs.clone();
    loop {
        if std::fs::metadata(&p).is_ok() || p.is_empty() || p == "/" || p == "." {
            break;
        }
        todo.push(p.clone());
        match p.rfind('/') {
            Some(0) => p = "/".into(),
            Some(i) => p = p[..i].to_string(),
            None => break,
        }
    }
    for d in todo.iter().rev() {
        match std::fs::create_dir(d) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists && std::fs::metadata(d).map(|m| m.is_dir()).unwrap_or(false) => {}
            Err(e) => return Err((d.clone(), e)),
        }
    }
    Ok(())
}

/// The scheme `urlparse(url)` reads, lowered.
fn url_scheme(url: &str) -> String {
    let Some(i) = url.find(':') else { return String::new() };
    let s = &url[..i];
    let mut cs = s.chars();
    match cs.next() {
        Some(c) if c.is_ascii_alphabetic() => {}
        _ => return String::new(),
    }
    if cs.all(|c| c.is_ascii_alphanumeric() || "+-.".contains(c)) {
        s.to_ascii_lowercase()
    } else {
        String::new()
    }
}

/// `executor.NetworkExecutor`: one GET, no proxy, no redirect followed.
#[derive(Clone)]
pub struct Network;

enum GetFail {
    Timeout,
    Errno(i32),
    Other(String),
}

/// One GET: the status, the reason and at most `cap` + 1 bytes of body.
fn http_get(url: &str, headers: &[(String, String)], timeout_s: u64, cap: usize) -> Result<(u16, String, Vec<u8>), GetFail> {
    use crate::netproxy::{self, Dial, Origin, Route};
    let (tls, host, port, path) = crate::gateway::http::target(url).map_err(|e| GetFail::Other(e.to_string()))?;
    let origin = Origin { tls, host: host.clone(), port, explicit_port: true };
    let limit = Duration::from_secs(timeout_s.max(1));
    let dial = Dial { limit: Some((Instant::now(), limit)), connect_timeout: None, roots: None };
    let io_fail = |e: std::io::Error| match (e.kind(), e.raw_os_error()) {
        (std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock, _) => GetFail::Timeout,
        (_, Some(n)) => GetFail::Errno(n),
        _ => GetFail::Other(e.to_string()),
    };
    let mut conn = match netproxy::open(&Route::Direct, &origin, &dial) {
        Ok(c) => c,
        Err(netproxy::Fail::Connect(e) | netproxy::Fail::Io(e)) => return Err(io_fail(e)),
        Err(netproxy::Fail::Status(c, r)) => return Err(GetFail::Other(format!("{c} {r}"))),
    };
    let default_port = if tls { 443 } else { 80 };
    let bracketed = if host.contains(':') { format!("[{host}]") } else { host.clone() };
    let host_header = if port == default_port { bracketed } else { format!("{bracketed}:{port}") };
    let mut req = format!("GET {path} HTTP/1.1\r\nHost: {host_header}\r\n");
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    req.push_str("Accept-Encoding: identity\r\nConnection: close\r\n\r\n");
    conn.io.write_all(req.as_bytes()).and_then(|_| conn.io.flush()).map_err(io_fail)?;
    let mut raw = Vec::new();
    conn.io.read_to_end(&mut raw).map_err(io_fail)?;
    let split = raw.windows(4).position(|w| w == b"\r\n\r\n").ok_or_else(|| GetFail::Other("the reply has no header end".into()))?;
    let head = String::from_utf8_lossy(&raw[..split]).into_owned();
    let mut rest = raw[split + 4..].to_vec();
    let mut lines = head.split("\r\n");
    let first = lines.next().unwrap_or("");
    let mut parts = first.splitn(3, ' ');
    let _ = parts.next();
    let status: u16 = parts.next().and_then(|c| c.parse().ok()).ok_or_else(|| GetFail::Other("the reply has no status".into()))?;
    let reason = parts.next().unwrap_or("").trim().to_string();
    let mut chunked = false;
    let mut length: Option<usize> = None;
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            match k.trim().to_ascii_lowercase().as_str() {
                "transfer-encoding" => chunked = v.to_ascii_lowercase().contains("chunked"),
                "content-length" => length = v.trim().parse().ok(),
                _ => {}
            }
        }
    }
    if chunked {
        rest = crate::gate::llm::dechunk(&rest).ok_or_else(|| GetFail::Other("the reply's chunks are broken".into()))?;
    } else if let Some(n) = length {
        rest.truncate(n);
    }
    rest.truncate(cap.saturating_add(1));
    Ok((status, reason, rest))
}

impl Executor for Network {
    fn job(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Option<Job> {
        let mut me = self.clone();
        let step = step.clone();
        Some(Box::new(move || me.run(&step, timeout_s, max_out)))
    }
    fn run(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Result<ExecResult, String> {
        let start = Instant::now();
        let url = str_of(step, "url");
        let scheme = url_scheme(&url);
        if scheme != "http" && scheme != "https" {
            return Ok(done(2, format!("refused non-http(s) URL scheme {}: {url}", repr(&scheme)), start));
        }
        let mut headers = vec![];
        if let Value::Obj(h) = step.value("headers") {
            for k in h.keys() {
                headers.push((k.clone(), h.value(k).as_str().unwrap_or("").to_string()));
            }
        }
        match http_get(&url, &headers, timeout_s, max_out) {
            Err(GetFail::Timeout) => Ok(ExecResult {
                rc: 2,
                stdout: "TimeoutError: timed out".into(),
                duration_ms: ms(start),
                timed_out: true,
                ..Default::default()
            }),
            Err(GetFail::Errno(n)) => Ok(done(2, format!("URLError: [Errno {n}] {}", strerror(n)), start)),
            Err(GetFail::Other(why)) => Ok(done(2, format!("URLError: {why}"), start)),
            Ok((status, reason, body)) => {
                if status >= 300 {
                    let mut out = format!("HTTP {status}: {reason}");
                    if !body.is_empty() {
                        out.push('\n');
                        out.push_str(&decode_utf8_replace(&body));
                    }
                    return Ok(done(1, out, start));
                }
                let out = if body.len() > max_out {
                    format!("{}\n... [truncated]", decode_utf8_replace(&body[..max_out]))
                } else {
                    decode_utf8_replace(&body)
                };
                Ok(done(0, out, start))
            }
        }
    }
}

/// `executor.SubprocessExecutor`: /bin/sh -c in a new session, stderr
/// merged into stdout, the group killed on a timeout or when the output
/// passes its cap.
#[derive(Clone)]
pub struct Shell {
    /// The child's environment.
    pub env: HashMap<String, String>,
}

impl Executor for Shell {
    fn job(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Option<Job> {
        let mut me = self.clone();
        let step = step.clone();
        Some(Box::new(move || me.run(&step, timeout_s, max_out)))
    }
    fn run(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Result<ExecResult, String> {
        let start = Instant::now();
        let mut fds = [0i32; 2];
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
            return Err(std::io::Error::last_os_error().to_string());
        }
        use std::os::fd::FromRawFd;
        let reader = unsafe { std::fs::File::from_raw_fd(fds[0]) };
        let writer = unsafe { std::fs::File::from_raw_fd(fds[1]) };
        let writer2 = writer.try_clone().map_err(|e| e.to_string())?;
        let mut cmd = std::process::Command::new("/bin/sh");
        cmd.arg("-c")
            .arg(str_of(step, "command"))
            .env_clear()
            .envs(&self.env)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::from(writer))
            .stderr(std::process::Stdio::from(writer2));
        unsafe {
            cmd.pre_exec(|| {
                if libc::setsid() == -1 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let mut child = cmd.spawn().map_err(|e| e.to_string())?;
        // The parent's copies of the write end close with cmd.
        drop(cmd);
        let pgid = child.id() as i32;
        // The reader appends to the buffer as it reads, so the output read
        // so far is there even when a writer outlives the step and the
        // reader is left.
        let shared = std::sync::Arc::new(std::sync::Mutex::new((Vec::<u8>::new(), false)));
        let (tx, rx) = std::sync::mpsc::channel::<()>();
        let mut reader = reader;
        let sink = shared.clone();
        std::thread::spawn(move || {
            let mut chunk = vec![0u8; 64 * 1024];
            loop {
                let room = max_out.saturating_sub(sink.lock().map(|g| g.0.len()).unwrap_or(max_out));
                if room == 0 {
                    break;
                }
                let want = (64 * 1024).min(room);
                match reader.read(&mut chunk[..want]) {
                    Ok(0) | Err(_) => {
                        let _ = tx.send(());
                        return;
                    }
                    Ok(n) => {
                        if let Ok(mut g) = sink.lock() {
                            g.0.extend_from_slice(&chunk[..n]);
                        }
                    }
                }
            }
            unsafe {
                libc::kill(-pgid, libc::SIGKILL);
            }
            if let Ok(mut g) = sink.lock() {
                g.1 = true;
            }
            let _ = tx.send(());
        });
        let limit = Duration::from_secs(timeout_s);
        let mut timed_out = false;
        let status = loop {
            match child.try_wait() {
                Ok(Some(s)) => break Some(s),
                Ok(None) if start.elapsed() >= limit => {
                    timed_out = true;
                    break kill_group(pgid, &mut child);
                }
                Ok(None) => std::thread::sleep(Duration::from_millis(5)),
                Err(_) => break None,
            }
        };
        // Read to EOF. A background child the shell left can hold the pipe
        // open after the shell exits, so the wait is bounded by the step's
        // own time; past it the group is killed and the output read so far
        // is kept.
        if rx.recv_timeout(limit.saturating_sub(start.elapsed())).is_err() {
            unsafe {
                libc::kill(-pgid, libc::SIGKILL);
            }
            let _ = rx.recv_timeout(Duration::from_secs(2));
        }
        let (buf, truncated) = shared.lock().map(|g| (g.0.clone(), g.1)).unwrap_or_default();
        use std::os::unix::process::ExitStatusExt;
        let rc = match status {
            Some(s) => match (s.code(), s.signal()) {
                (Some(c), _) => c,
                (None, Some(sig)) => -sig,
                _ => -1,
            },
            None => -1,
        };
        let mut text = decode_utf8_replace(&buf);
        if truncated {
            text.push_str("\n... [truncated]");
        }
        Ok(ExecResult { rc, stdout: text, duration_ms: ms(start), timed_out, ..Default::default() })
    }
}

/// `SubprocessExecutor._kill_group`: SIGTERM to the group, two seconds,
/// then SIGKILL; the child's exit status.
fn kill_group(pgid: i32, child: &mut std::process::Child) -> Option<std::process::ExitStatus> {
    if unsafe { libc::kill(-pgid, libc::SIGTERM) } != 0 {
        return child.wait().ok();
    }
    let t = Instant::now();
    while t.elapsed() < Duration::from_secs(2) {
        if let Ok(Some(s)) = child.try_wait() {
            return Some(s);
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    unsafe {
        libc::kill(-pgid, libc::SIGKILL);
    }
    child.wait().ok()
}

/// `executor.default_executors`, the shell step with `env`.
pub fn default_executors(env: HashMap<String, String>) -> Vec<(String, Box<dyn Executor>)> {
    vec![
        ("shell".into(), Box::new(Shell { env }) as Box<dyn Executor>),
        ("file_read".into(), Box::new(FileRead::default())),
        ("file_write".into(), Box::new(FileWrite::default())),
        ("network".into(), Box::new(Network)),
    ]
}
