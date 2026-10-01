//! The foreman's tools are served on a socket that lives one turn. The
//! turn runner starts the model with an MCP server (coppice foreman tools
//! --sock S) that relays each call to S, and nothing else. The listener
//! lives in the turn dir (0700), takes only the four verbs, and checks
//! SO_PEERCRED: the peer must descend from the process the turn runner
//! started, so a worker of the same user cannot call them. The public
//! socket and its role rule are not involved.
//!
//! The pane foreman's tools are served the same way on a standing socket
//! (foreman_standing.rs): it lives as long as the server, and its peer must
//! descend from the pane foreman's process. The twin of Go's
//! foreman_tools.go.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Weak};
use std::thread;
use std::time::Duration;

use serde_json::{Map, Value};

use super::foreman_chat::{ForemanChat, ERR_NO_CHAT};
use super::foreman_standing::STANDING_NO_MEMORY;
use super::foreman_turn::WaitGroup;
use super::Server;
use crate::foreman::chat as fchat;
use crate::foreman::tools::{ToolRequest, ToolResponse, MAX_TOOL_LINE, TOOL_NAMES};

/// The refusal a peer outside the turn's process tree gets.
pub const TOOLS_REFUSAL: &str =
    "refused: this socket serves the tools of one turn, to that turn's own process only.";

/// The tool socket of one turn, or the standing socket of the pane
/// foreman.
pub struct TurnSocket {
    s: Weak<Server>,
    fc: Arc<ForemanChat>,
    path: PathBuf,
    /// The pid the turn runner started, 0 until it has started.
    root: AtomicI64,
    /// For the standing socket: the root pid at each call, the pane
    /// foreman, which can change.
    root_fn: Option<Box<dyn Fn() -> i32 + Send + Sync>>,
    /// True for the pane foreman's socket. With no chat open, zoom and date
    /// answer a plain line, and no call is logged.
    pub standing: bool,
    closed: AtomicBool,
    /// The listener, shut down by close to end the accept loop.
    ln: Arc<UnixListener>,
    /// Set when the accept loop has returned.
    accepting_done: crate::pane::Flag,
    /// The calls in flight.
    wg: Arc<WaitGroup>,
}

/// How long close waits for a call in flight: the 30 s read deadline of a
/// call, and a little more.
const CLOSE_WAIT: Duration = Duration::from_secs(32);

impl TurnSocket {
    /// The socket the model's tool relay dials.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Tells the socket which process is the turn's. Until it does, every
    /// peer is refused.
    pub fn started(&self, pid: i32) {
        self.root.store(pid as i64, Ordering::SeqCst);
    }

    /// Stops serving and removes the socket. A call in flight finishes.
    pub fn close(&self) {
        if self.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        // Shutting the listener down wakes the accept loop at once: it sees
        // closed and returns. No connection is needed, so close never
        // waits on a peer.
        use std::os::unix::io::AsRawFd;
        // SAFETY: the listener is alive while self holds it.
        unsafe { libc::shutdown(self.ln.as_raw_fd(), libc::SHUT_RDWR) };
        self.accepting_done.wait(Some(Duration::from_secs(2)));
        self.wg.wait(CLOSE_WAIT);
        let _ = std::fs::remove_file(&self.path);
    }

    fn root_pid(&self) -> i32 {
        match &self.root_fn {
            Some(f) => f(),
            None => self.root.load(Ordering::SeqCst) as i32,
        }
    }

    /// Whether the peer may call: the same uid as this process, and a pid
    /// that is the root or lies below it. Anything that cannot be read is
    /// refused.
    fn allowed(&self, conn: &UnixStream) -> bool {
        let mut root = self.root_pid();
        // The runner names its process right after it starts it. A peer
        // that is faster than that waits a moment, then is refused. The
        // standing socket's root is running or is not.
        let mut wait = 0;
        while root <= 1 && wait < 200 && !self.standing {
            thread::sleep(Duration::from_millis(10));
            root = self.root_pid();
            wait += 1;
        }
        peer_ok(crate::sys::peer_cred(conn), root, crate::sys::getuid())
    }

    fn handle(&self, mut conn: UnixStream) {
        if !self.allowed(&conn) {
            write_response(&mut conn, &ToolResponse::err(TOOLS_REFUSAL));
            return;
        }
        let _ = conn.set_read_timeout(Some(Duration::from_secs(30)));
        let mut line = Vec::new();
        let mut r = BufReader::with_capacity(64 * 1024, (&conn).take(MAX_TOOL_LINE as u64 + 1));
        let _ = r.read_until(b'\n', &mut line);
        if line.is_empty() {
            return;
        }
        let Ok(req) = ToolRequest::decode(line.trim_ascii()) else {
            write_response(&mut conn, &ToolResponse::err("the request is not JSON"));
            return;
        };
        let args = crate::gojson::marshal(&crate::foreman::tools::sorted(&Value::Object(
            req.args.clone(),
        )));
        let res = self.call(&req);
        let logged = match &res {
            Ok(t) => t.clone(),
            Err(e) => format!("error: {e}"),
        };
        if let Some(s) = self.s.upgrade() {
            if !self.standing || self.fc.has_chat() {
                s.log_tool_call(&self.fc, &req.tool, &args, &logged);
            }
        }
        match res {
            Err(e) => write_response(&mut conn, &ToolResponse::err(e)),
            Ok(t) => write_response(
                &mut conn,
                &ToolResponse {
                    ok: true,
                    result: t,
                    error: String::new(),
                },
            ),
        }
    }

    /// Answers zoom and date when no chat is open. A turn's socket has one
    /// open, so for it this is an error. A foreman with its own memory is
    /// told so in one plain line.
    fn no_chat(&self) -> Result<String, String> {
        if self.standing {
            return Ok(STANDING_NO_MEMORY.into());
        }
        Err(ERR_NO_CHAT.into())
    }

    /// Runs one of the four verbs. Any other tool is refused.
    fn call(&self, req: &ToolRequest) -> Result<String, String> {
        let Some(s) = self.s.upgrade() else {
            return Err("this server has closed".into());
        };
        let a = &req.args;
        match req.tool.as_str() {
            "zoom" => {
                if a.contains_key("label") {
                    if a.contains_key("id") || a.contains_key("n") {
                        return Err("give a label, or an id and an n, not both".into());
                    }
                    let label = str_arg(a, "label", true)?;
                    return s.tool_zoom_worker(&self.fc, &label);
                }
                let id = int_arg(a, "id")?;
                let n = int_arg(a, "n")?;
                let g = self.fc.lock();
                match g.as_ref() {
                    None => self.no_chat(),
                    Some(c) => c.zoom(id, n),
                }
            }
            "date" => {
                let id = int_arg(a, "id")?;
                let g = self.fc.lock();
                match g.as_ref() {
                    None => self.no_chat(),
                    Some(c) => c.date(id),
                }
            }
            "spawn" => {
                let project = str_arg(a, "project", true)?;
                let brief = str_arg(a, "brief", true)?;
                let profile = str_arg(a, "profile", false)?;
                let model = str_arg(a, "model", false)?;
                if self.standing {
                    return s.tool_spawn_in(&self.fc, &project, &brief, &profile, &model);
                }
                s.tool_spawn(&project, &brief, &profile, &model)
            }
            "status" => {
                if self.standing {
                    s.load_workers(&self.fc);
                }
                Ok(s.tool_status())
            }
            _ => {
                let mut names = TOOL_NAMES.to_vec();
                names.sort();
                Err(format!(
                    "no tool named {}. The tools are {}",
                    crate::sys::go_quote(&req.tool),
                    names.join(", ")
                ))
            }
        }
    }
}

/// The peer rule of a tool socket: credentials that read, the uid of this
/// process, and a pid that is the root or lies below it. A root that is not
/// set, credentials that cannot be read, another uid, or a pid outside the
/// tree is refused.
pub fn peer_ok(cred: Option<(i32, u32, u32)>, root: i32, me: u32) -> bool {
    if root <= 1 {
        return false;
    }
    let Some((pid, uid, _)) = cred else {
        return false;
    };
    if uid != me || pid <= 0 {
        return false;
    }
    descends(pid, root)
}

/// Whether pid is root or lies below it in the process tree.
pub fn descends(pid: i32, root: i32) -> bool {
    let mut pid = pid;
    let mut i = 0;
    while i < 64 && pid > 1 {
        if pid == root {
            return true;
        }
        match crate::sys::proc_stat(pid) {
            Ok((ppid, _)) => pid = ppid,
            Err(_) => return false,
        }
        i += 1;
    }
    pid == root && root > 1
}

fn write_response(conn: &mut UnixStream, r: &ToolResponse) {
    let _ = conn.set_write_timeout(Some(Duration::from_secs(30)));
    let _ = conn.write_all(format!("{}\n", r.encode()).as_bytes());
}

/// A whole-number argument. JSON numbers read as Go's float64: 1.0 and 1e2
/// are whole numbers, and one past 2^53 is refused.
pub fn int_arg(args: &Map<String, Value>, name: &str) -> Result<i64, String> {
    let Some(v) = args.get(name) else {
        return Err(format!("{name} is missing"));
    };
    let f = match v {
        Value::Number(n) => n.as_f64(),
        _ => None,
    };
    match f {
        Some(f) if f == f.trunc() && f.abs() <= (1u64 << 53) as f64 => Ok(f as i64),
        _ => Err(format!("{name} must be a whole number")),
    }
}

/// A text argument; missing or null is "" unless need.
pub fn str_arg(args: &Map<String, Value>, name: &str, need: bool) -> Result<String, String> {
    match args.get(name) {
        None | Some(Value::Null) => {
            if need {
                Err(format!("{name} is missing"))
            } else {
                Ok(String::new())
            }
        }
        Some(Value::String(s)) => Ok(s.clone()),
        Some(_) => Err(format!("{name} must be text")),
    }
}

impl Server {
    /// Listens at <turn dir>/t-<n>.sock. A stale socket file of an earlier
    /// run is removed; anything else at that name is an error.
    pub fn open_turn_socket(
        self: &Arc<Server>,
        fc: &Arc<ForemanChat>,
        n: i64,
    ) -> Result<Arc<TurnSocket>, String> {
        let dir = self.turn_dir();
        let path = dir.join(format!("t-{n}.sock"));
        // The standing socket's rule (FR-R-9): a path of 100 bytes or more
        // is refused before anything is made, with words that say why.
        if path.as_os_str().len() >= 100 {
            return Err(format!(
                "the path {} is too long for a socket. Use a shorter data dir",
                path.display()
            ));
        }
        crate::foreman::fsutil::ensure_dir_plain(&dir)?;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))
            .map_err(|e| crate::sys::go_path_err("chmod", &dir, &e))?;
        self.listen_tools(fc, &path, false, None)
    }

    /// Listens at path, 0600, and serves the four verbs. A stale socket
    /// file of an earlier run is removed; anything else at that name is an
    /// error.
    pub fn listen_tools(
        self: &Arc<Server>,
        fc: &Arc<ForemanChat>,
        path: &Path,
        standing: bool,
        root_fn: Option<Box<dyn Fn() -> i32 + Send + Sync>>,
    ) -> Result<Arc<TurnSocket>, String> {
        if let Ok(m) = std::fs::symlink_metadata(path) {
            if !m.file_type().is_socket() {
                return Err(format!("{} is not a socket. Move it away", path.display()));
            }
            std::fs::remove_file(path).map_err(|e| crate::sys::go_path_err("remove", path, &e))?;
        }
        let ln = UnixListener::bind(path).map_err(|e| {
            format!(
                "listen unix {}: {}",
                path.display(),
                crate::sys::go_errno_text(&e)
            )
        })?;
        if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
            drop(ln);
            let _ = std::fs::remove_file(path);
            return Err(crate::sys::go_path_err("chmod", path, &e));
        }
        let wg = Arc::new(WaitGroup::default());
        let ln = Arc::new(ln);
        let ts = Arc::new(TurnSocket {
            s: Arc::downgrade(self),
            fc: fc.clone(),
            path: path.to_path_buf(),
            root: AtomicI64::new(0),
            root_fn,
            standing,
            closed: AtomicBool::new(false),
            ln: ln.clone(),
            accepting_done: crate::pane::Flag::default(),
            wg: wg.clone(),
        });
        let t = ts.clone();
        thread::spawn(move || {
            for conn in ln.incoming() {
                if t.closed.load(Ordering::SeqCst) {
                    break;
                }
                let Ok(conn) = conn else {
                    thread::sleep(Duration::from_millis(10));
                    continue;
                };
                t.wg.add();
                let t2 = t.clone();
                thread::spawn(move || {
                    t2.handle(conn);
                    t2.wg.done();
                });
            }
            t.accepting_done.set();
        });
        Ok(ts)
    }

    /// Logs one call and its result, word for word, as one tool message:
    /// name(args) on the first line, then the result.
    pub fn log_tool_call(&self, fc: &ForemanChat, tool: &str, args: &str, result: &str) {
        if let Err(e) = self.chat_append(fc, fchat::KIND_TOOL, &format!("{tool}({args})\n{result}"))
        {
            self.note_once(&format!(
                "The foreman's log could not take a tool call: {e}"
            ));
        }
    }
}
