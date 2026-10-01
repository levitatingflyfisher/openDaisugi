//! The client side of the socket: one request, the matching reply. The
//! local path and the ssh path use the same code, since --stdio is the
//! same JSON lines on a different pipe.

use std::io::{self, BufReader, Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::os::unix::net::UnixStream;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Map, Value};

use super::gofmt;
use crate::proto::Decoder;
use crate::sys;

/// How long Do waits for one line when the request carries no timeout_ms.
const DO_READ_DEADLINE: Duration = Duration::from_secs(150);
/// Added on top of a request's own timeout_ms.
const READ_DEADLINE_MARGIN: Duration = Duration::from_secs(10);

/// Why a call failed.
pub enum DoErr {
    /// The connection itself failed: exit 3.
    Transport,
    /// Anything else, a refusal the server sent among them: exit 1.
    Other(String),
}

/// The log a detached server writes.
pub fn log_path(data_dir: &Path) -> PathBuf {
    data_dir.join("server.log")
}

pub struct Client {
    w: Box<dyn Write + Send>,
    dec: Decoder<BufReader<Box<dyn Read + Send>>>,
    stream: Option<UnixStream>,
    fixed_deadline: bool,
    n: u64,
    child: Option<Child>,
    stderr_buf: Option<Arc<Mutex<Vec<u8>>>>,
    stderr_thread: Option<thread::JoinHandle<()>>,
    closed: bool,
}

impl Client {
    fn from_stream(s: UnixStream, fixed: bool) -> io::Result<Client> {
        let r = s.try_clone()?;
        let w = s.try_clone()?;
        Ok(Client {
            w: Box::new(w),
            dec: Decoder::new(BufReader::new(Box::new(r) as Box<dyn Read + Send>)),
            stream: Some(s),
            fixed_deadline: fixed,
            n: 0,
            child: None,
            stderr_buf: None,
            stderr_thread: None,
            closed: false,
        })
    }

    /// The hello a client inside a pane sends first. A hello to another
    /// host names no pane.
    fn hello(&mut self, remote: bool) -> io::Result<()> {
        let pane = std::env::var("COPPICE_PANE").unwrap_or_default();
        if pane.is_empty() {
            return Ok(());
        }
        let mut pairs = vec![
            ("id", json!("0")),
            ("cmd", json!("hello")),
            ("role", json!("pane")),
        ];
        if !remote {
            pairs.push(("pane", json!(pane)));
        }
        self.send(&crate::gojson::marshal(&crate::gojson::map(pairs)))
    }

    fn send(&mut self, line: &str) -> io::Result<()> {
        let mut b = line.as_bytes().to_vec();
        b.push(b'\n');
        self.w.write_all(&b)?;
        self.w.flush()
    }

    /// Sends one request and reads until the matching reply. Events and
    /// other lines are skipped.
    pub fn call(
        &mut self,
        cmd: &str,
        params: Map<String, Value>,
    ) -> Result<Map<String, Value>, DoErr> {
        for k in params.keys() {
            if k == "id" || k == "cmd" {
                return Err(DoErr::Other(format!(
                    "cmd {cmd}: params must not set {}, it is reserved for the request itself",
                    sys::go_quote(k)
                )));
            }
        }
        self.n += 1;
        let id = self.n.to_string();
        let mut req = params.clone();
        req.insert("id".into(), json!(id));
        req.insert("cmd".into(), json!(cmd));
        let line = gofmt::marshal(&Value::Object(req));
        if self.send(&line).is_err() {
            return Err(DoErr::Transport);
        }
        let deadline = read_deadline_for(&params);
        loop {
            if let (Some(s), false) = (&self.stream, self.fixed_deadline) {
                let _ = s.set_read_timeout(Some(deadline));
            }
            let raw = match self.dec.next_line() {
                Ok(l) => l,
                Err(e) => {
                    return Err(match e.kind() {
                        io::ErrorKind::Other => DoErr::Other(e.to_string()),
                        _ => DoErr::Transport,
                    })
                }
            };
            let text = String::from_utf8_lossy(&raw);
            let Ok(v) = serde_json::from_str::<Value>(&text) else {
                continue;
            };
            let Some(m) = v.as_object() else { continue };
            // Go reads the line into a struct: a field of the wrong type
            // fails the whole line, which is then skipped.
            let id_ok = matches!(
                m.get("id"),
                None | Some(Value::String(_)) | Some(Value::Null)
            );
            let ev_ok = matches!(
                m.get("event"),
                None | Some(Value::String(_)) | Some(Value::Null)
            );
            let ok_ok = matches!(m.get("ok"), None | Some(Value::Bool(_)) | Some(Value::Null));
            let res_ok = matches!(
                m.get("result"),
                None | Some(Value::Object(_)) | Some(Value::Null)
            );
            let err_ok = match m.get("error") {
                None | Some(Value::Null) => true,
                Some(Value::Object(e)) => {
                    matches!(
                        e.get("code"),
                        None | Some(Value::String(_)) | Some(Value::Null)
                    ) && matches!(
                        e.get("message"),
                        None | Some(Value::String(_)) | Some(Value::Null)
                    )
                }
                _ => false,
            };
            if !(id_ok && ev_ok && ok_ok && res_ok && err_ok) {
                continue;
            }
            let event = m.get("event").and_then(Value::as_str).unwrap_or("");
            let rid = m.get("id").and_then(Value::as_str).unwrap_or("");
            if !event.is_empty() || rid != id {
                continue;
            }
            if m.get("ok").and_then(Value::as_bool) != Some(true) {
                let Some(Value::Object(e)) = m.get("error") else {
                    return Err(DoErr::Other(
                        "internal: the server sent a failure with no error".into(),
                    ));
                };
                let s = |k: &str| e.get(k).and_then(Value::as_str).unwrap_or("").to_string();
                return Err(DoErr::Other(format!("{}: {}", s("code"), s("message"))));
            }
            let Some(res) = m.get("result").map(gofmt::decode) else {
                return Ok(Map::new());
            };
            let Some(res) = res else { continue };
            return Ok(match res {
                Value::Object(o) => o,
                _ => Map::new(),
            });
        }
    }

    /// Closes the connection, and for a remote one waits for ssh, so its
    /// stderr is whole before stderr_tail reads it.
    pub fn close(&mut self) {
        if self.closed {
            return;
        }
        self.closed = true;
        if let Some(s) = &self.stream {
            let _ = s.shutdown(std::net::Shutdown::Both);
        }
        // Dropping the writer closes ssh's stdin.
        self.w = Box::new(io::sink());
        if let Some(mut c) = self.child.take() {
            while self.dec.next_line().is_ok() {}
            let _ = c.wait();
        }
        if let Some(h) = self.stderr_thread.take() {
            let _ = h.join();
        }
    }

    /// The tail of a remote child's stderr, after close.
    pub fn stderr_tail(&self) -> String {
        match &self.stderr_buf {
            Some(b) => String::from_utf8_lossy(&b.lock().unwrap_or_else(|e| e.into_inner()))
                .trim()
                .to_string(),
            None => String::new(),
        }
    }
}

impl Drop for Client {
    fn drop(&mut self) {
        self.close();
    }
}

fn read_deadline_for(params: &Map<String, Value>) -> Duration {
    match params.get("timeout_ms").and_then(|v| v.as_f64()) {
        Some(ms) if ms > 0.0 => Duration::from_millis(ms as u64) + READ_DEADLINE_MARGIN,
        _ => DO_READ_DEADLINE,
    }
}

fn wait_for_socket(socket: &Path, d: Duration) -> Option<UnixStream> {
    let deadline = Instant::now() + d;
    while Instant::now() < deadline {
        if let Ok(c) = UnixStream::connect(socket) {
            return Some(c);
        }
        thread::sleep(Duration::from_millis(50));
    }
    None
}

/// Re-execs this binary as a detached foreground server, its output in
/// the data dir's log, outside any pane's COPPICE_PANE and COPPICE_SOCK.
pub fn start_background_server(socket: &Path, data_dir: &Path) -> Result<(), String> {
    if std::env::var_os("COPPICE_NO_AUTOSTART").is_some_and(|v| !v.is_empty()) {
        return Err("autostart is off. COPPICE_NO_AUTOSTART is set".into());
    }
    let me = std::env::current_exe().map_err(|e| e.to_string())?;
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(data_dir)
        .map_err(|e| sys::go_path_err("mkdir", data_dir, &e))?;
    let lp = log_path(data_dir);
    let log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .open(&lp)
        .map_err(|e| sys::go_path_err("open", &lp, &e))?;
    let log2 = log.try_clone().map_err(|e| e.to_string())?;
    let mut cmd = Command::new(me);
    cmd.arg("--socket")
        .arg(socket)
        .arg("--data-dir")
        .arg(data_dir)
        .args(["server", "start", "--foreground"])
        .stdin(Stdio::null())
        .stdout(log)
        .stderr(log2)
        .env_remove("COPPICE_PANE")
        .env_remove("COPPICE_SOCK");
    // SAFETY: setsid is async-signal-safe and touches no Rust state.
    unsafe {
        cmd.pre_exec(|| {
            libc::setsid();
            Ok(())
        });
    }
    let mut child = cmd.spawn().map_err(|e| e.to_string())?;
    thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

/// Connects to socket, starting a server once when nothing listens.
fn dial_or_start(socket: &Path, data_dir: &Path) -> Result<UnixStream, String> {
    if let Ok(c) = UnixStream::connect(socket) {
        return Ok(c);
    }
    if let Err(e) = start_background_server(socket, data_dir) {
        return Err(format!(
            "cannot reach the server at {} and cannot start one: {e}. Run: coppice server start --foreground",
            socket.display()
        ));
    }
    wait_for_socket(socket, Duration::from_secs(5)).ok_or_else(|| {
        format!(
            "started a server but {} never accepted a connection. Check {}. Run: coppice server start --foreground",
            socket.display(),
            log_path(data_dir).display()
        )
    })
}

/// Connects, autostarting a server against data_dir when none listens.
pub fn dial(socket: &Path, data_dir: &Path) -> Result<Client, String> {
    let s = dial_or_start(socket, data_dir)?;
    let mut c = Client::from_stream(s, false).map_err(|e| e.to_string())?;
    c.hello(false).map_err(|e| e.to_string())?;
    Ok(c)
}

/// Connects only when something already listens. A deadline, when set,
/// bounds the whole connection.
pub fn dial_existing(socket: &Path, deadline: Option<Duration>) -> Result<Client, String> {
    let s = UnixStream::connect(socket).map_err(|e| e.to_string())?;
    if let Some(d) = deadline {
        let _ = s.set_read_timeout(Some(d));
        let _ = s.set_write_timeout(Some(d));
    }
    let mut c = Client::from_stream(s, deadline.is_some()).map_err(|e| e.to_string())?;
    c.hello(false).map_err(|e| e.to_string())?;
    Ok(c)
}

/// The command that runs a thin server on target.
fn remote_argv(target: &str) -> Result<Vec<String>, String> {
    match target.strip_prefix("ssh://") {
        Some(rest) if !rest.is_empty() => Ok(vec![
            "ssh".into(),
            rest.into(),
            "coppice".into(),
            "--stdio".into(),
        ]),
        _ => Err(format!(
            "remote target {} must look like ssh://host",
            sys::go_quote(target)
        )),
    }
}

/// Runs coppice --stdio on target over ssh and speaks the protocol across
/// its stdin and stdout.
pub fn dial_remote(target: &str) -> Result<Client, String> {
    let argv = remote_argv(target)?;
    let Some(ssh) = crate::pane::look_path(&argv[0]) else {
        return Err(format!(
            "cannot run {}: exec: {}: executable file not found in $PATH",
            argv.join(" "),
            sys::go_quote(&argv[0])
        ));
    };
    let mut child = Command::new(ssh)
        .args(&argv[1..])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("cannot run {}: {e}", argv.join(" ")))?;
    let w = child.stdin.take().expect("stdin");
    let r = child.stdout.take().expect("stdout");
    let mut err = child.stderr.take().expect("stderr");
    let buf = Arc::new(Mutex::new(Vec::new()));
    let b2 = buf.clone();
    let h = thread::spawn(move || {
        let mut tmp = Vec::new();
        let _ = err.read_to_end(&mut tmp);
        b2.lock().unwrap_or_else(|e| e.into_inner()).extend(tmp);
    });
    let mut c = Client {
        w: Box::new(w),
        dec: Decoder::new(BufReader::new(Box::new(r) as Box<dyn Read + Send>)),
        stream: None,
        fixed_deadline: false,
        n: 0,
        child: Some(child),
        stderr_buf: Some(buf),
        stderr_thread: Some(h),
        closed: false,
    };
    if let Err(e) = c.hello(true) {
        c.close();
        return Err(e.to_string());
    }
    Ok(c)
}

/// The whole of --stdio: one client's bytes to the running server and
/// back. It builds no server.
pub fn proxy(socket: &Path, data_dir: &Path) -> Result<(), String> {
    let conn = dial_or_start(socket, data_dir)?;
    let mut from = conn.try_clone().map_err(|e| e.to_string())?;
    let out = thread::spawn(move || -> io::Result<()> {
        let mut stdout = io::stdout();
        let mut buf = [0u8; 32 * 1024];
        loop {
            let n = from.read(&mut buf)?;
            if n == 0 {
                return Ok(());
            }
            stdout.write_all(&buf[..n])?;
            stdout.flush()?;
        }
    });
    let mut to = conn;
    let in_err = io::copy(&mut io::stdin().lock(), &mut to).err();
    let _ = to.shutdown(std::net::Shutdown::Write);
    let out_err = out.join().unwrap_or(Ok(())).err();
    if let Some(e) = in_err {
        return Err(e.to_string());
    }
    if let Some(e) = out_err {
        return Err(e.to_string());
    }
    Ok(())
}
