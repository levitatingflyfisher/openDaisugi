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
use std::sync::Mutex;
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

/// Whether a tool has effects outside the chat: only these write an intent
/// line first.
fn acting(tool: &str) -> bool {
    tool == "spawn"
}

/// Cuts the arguments shown in a note at 80 bytes.
fn clip_args(args: &str) -> String {
    if args.len() <= 80 {
        return args.to_string();
    }
    format!("{}...", crate::foreman::fsutil::clip(args, 80))
}

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
    /// The tool requests the socket has taken from its turn. A turn that
    /// made any is not run again on another profile.
    calls: AtomicI64,
    /// The owner message the turn answers; -1 for the standing socket.
    turn: AtomicI64,
    /// The owner messages this turn took at its tool calls (FM-R-101).
    mid: Mutex<Vec<i64>>,
    /// Made for this turn. The owner's words reach the model in a block that
    /// carries it; the model trusts only a block whose nonce is the one in
    /// its own state (FM-R-109).
    nonce: String,
    /// Counts the write-ahead call lines.
    seq: AtomicI64,
    /// Names the socket in a call id: c<turn>, or s<start>-<random> for the
    /// standing socket (FM-R-66).
    tag: String,
    /// When set, runs a tool in place of `call`. A test uses it to look at
    /// the log at the moment of the effect.
    #[cfg(test)]
    #[allow(clippy::type_complexity)]
    pub call_fn: Mutex<Option<Arc<dyn Fn(&ToolRequest) -> Result<String, String> + Send + Sync>>>,
}

/// Names the standing socket in a call id. The clock alone can repeat when
/// two sockets open in one second, so random bytes follow it.
pub fn standing_tag() -> String {
    use std::hash::{BuildHasher, Hasher};
    let r = std::collections::hash_map::RandomState::new()
        .build_hasher()
        .finish();
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs());
    format!("s{secs}-{:08x}", r as u32)
}

/// 96 random bits as 24 hex digits: the mark of the owner's block in one turn
/// (FM-R-109).
pub fn new_nonce() -> String {
    use std::io::Read as _;
    let mut b = [0u8; 12];
    if std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(&mut b)).is_err() {
        // No randomness: a clock-based mark still differs from a forged one
        // that was written before the turn began.
        let n = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_nanos());
        return format!("{n:024x}");
    }
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn message_now_tag() -> &'static regex::Regex {
    static RE: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| regex::Regex::new(r"</?message-now(?-u:\b)").unwrap())
}

/// Puts a backslash after the "<" of each message-now tag in a tool result,
/// so that text from a worker or a file cannot pass for the owner's block
/// (FM-R-109).
pub fn escape_message_now(s: &str) -> String {
    message_now_tag()
        .replace_all(s, |c: &regex::Captures| format!("<\\{}", &c[0][1..]))
        .into_owned()
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

    /// Whether the write-ahead log holds a start line for this turn. It
    /// reads the file, not the memory of the process, so a turn with a tool
    /// start is not replayed even when the count was lost (FM-R-50, FM-R-66).
    pub fn logged_start(&self) -> bool {
        let turn = self.turn.load(Ordering::SeqCst);
        if turn < 0 {
            return false;
        }
        let recs = crate::foreman::calls::read_calls(&self.fc.dir).unwrap_or_default();
        crate::foreman::calls::turn_has_tool_start(&recs, turn)
    }

    /// Writes the start line of an acting call and returns its id.
    fn start_call(&self, s: &Server, tool: &str, args: &str) -> Result<String, (String, String)> {
        use crate::foreman::calls as c;
        let n = self.seq.fetch_add(1, Ordering::SeqCst) + 1;
        let id = format!("{}.{n}", self.tag);
        c::append_call(
            &self.fc.dir,
            &c::CallRecord {
                call: id.clone(),
                phase: c::CALL_START.into(),
                turn: self.turn.load(Ordering::SeqCst),
                tool: tool.into(),
                args: args.into(),
                at: s.fm_date(),
                ..Default::default()
            },
        )
        .map_err(|e| (id.clone(), e))?;
        Ok(id)
    }

    /// Writes the end line. A failure to write it leaves the call dangling,
    /// which the next open reports as unknown: the safe side.
    fn end_call(&self, s: &Server, id: &str, failed: bool) {
        use crate::foreman::calls as c;
        let _ = c::append_call(
            &self.fc.dir,
            &c::CallRecord {
                call: id.into(),
                phase: c::CALL_END.into(),
                outcome: (if failed { c::CALL_FAILED } else { c::CALL_OK }).into(),
                at: s.fm_date(),
                ..Default::default()
            },
        );
    }

    /// Whether the socket has written a start line: a tool call ran, or
    /// was about to, in this turn.
    pub fn wrote_calls(&self) -> bool {
        self.seq.load(Ordering::SeqCst) > 0
    }

    /// How many tool requests the socket has taken.
    pub fn calls(&self) -> i64 {
        self.calls.load(Ordering::SeqCst)
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
            let _ = write_response(&mut conn, &ToolResponse::err(TOOLS_REFUSAL));
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
            let _ = write_response(&mut conn, &ToolResponse::err("the request is not JSON"));
            return;
        };
        let args = crate::gojson::marshal(&crate::foreman::tools::sorted(&Value::Object(
            req.args.clone(),
        )));
        let logging = !self.standing || self.fc.has_chat();
        // An acting call writes its intent first, fsynced, and then the
        // effect runs. When the line cannot be written there is no effect
        // (FM-R-66).
        let mut call_id = String::new();
        if logging && acting(&req.tool) {
            let refuse = |conn: &mut UnixStream, why: &str| {
                let _ = write_response(
                    conn,
                    &ToolResponse::err(format!(
                        "the call was not started: the foreman's log could not take its intent line ({why})"
                    )),
                );
            };
            let Some(s) = self.s.upgrade() else {
                refuse(&mut conn, "this server has closed");
                return;
            };
            match self.start_call(&s, &req.tool, &args) {
                Ok(id) => call_id = id,
                Err((id, e)) => {
                    // The line may be on disk (a failed directory sync).
                    // Close it as failed, if that can be written, so the
                    // next open does not report a call that never ran. If
                    // it cannot be written, the open reports it as
                    // unknown: the safe side.
                    self.end_call(&s, &id, true);
                    refuse(&mut conn, &e);
                    return;
                }
            }
        }
        // The hook is cloned out of its lock, so calls on one socket run at
        // the same time, as in Go.
        #[cfg(test)]
        let hook = self.call_fn.lock().unwrap_or_else(|e| e.into_inner()).clone();
        #[cfg(not(test))]
        let hook: Option<std::convert::Infallible> = None;
        #[cfg(test)]
        let res = match hook {
            Some(f) => f(&req),
            None => self.call(&req),
        };
        #[cfg(not(test))]
        let res = {
            let _ = hook;
            self.call(&req)
        };
        if !call_id.is_empty() {
            if let Some(s) = self.s.upgrade() {
                self.end_call(&s, &call_id, res.is_err());
            }
        }
        let logged = match &res {
            Ok(t) => t.clone(),
            Err(e) => format!("error: {e}"),
        };
        if let Some(s) = self.s.upgrade() {
            if logging {
                s.log_tool_call(&self.fc, &req.tool, &args, &logged);
            }
        }
        // An owner message that came while this turn ran reaches it here, at
        // the end of a tool call, as a block after the result (FM-R-101).
        // The log keeps the result as it was.
        let (extra, taken) = match self.s.upgrade() {
            Some(s) if !self.standing => s.deliver_mid(self),
            _ => (String::new(), Vec::new()),
        };
        // Text a worker or a file wrote can hold a forged message-now tag; it
        // is neutralised before the real block is added (FM-R-109).
        let wrote = match res {
            Err(e) => write_response(
                &mut conn,
                &ToolResponse::err(format!("{}{extra}", escape_message_now(&e))),
            ),
            Ok(t) => write_response(
                &mut conn,
                &ToolResponse {
                    ok: true,
                    result: format!("{}{extra}", escape_message_now(&t)),
                    error: String::new(),
                },
            ),
        };
        if !taken.is_empty() {
            let Some(s) = self.s.upgrade() else { return };
            if wrote.is_err() {
                // The model did not get the block: the messages go next turn
                // (FM-R-111).
                s.requeue_delivered(&taken);
                return;
            }
            self.add_mid(&taken);
            s.send_messages_event("", true);
        }
    }

    /// The nonce that marks the owner's block in this turn.
    pub fn nonce(&self) -> &str {
        &self.nonce
    }

    /// The owner message this turn answers.
    pub fn turn_id(&self) -> i64 {
        self.turn.load(Ordering::SeqCst)
    }

    /// The owner messages this turn took, in order.
    pub fn mid_ids(&self) -> Vec<i64> {
        self.mid.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    pub fn add_mid(&self, ids: &[i64]) {
        self.mid.lock().unwrap_or_else(|e| e.into_inner()).extend_from_slice(ids);
    }

    /// A message of the chat, under the chat lock.
    pub fn chat_message(&self, id: i64) -> Result<crate::foreman::chat::Message, String> {
        match self.fc.lock().as_ref() {
            Some(c) => c.message(id),
            None => Err(ERR_NO_CHAT.into()),
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
                    if a.contains_key("offset") || a.contains_key("size") {
                        return Err("offset and size go with an id and an n, not a label".into());
                    }
                    let label = str_arg(a, "label", true)?;
                    return s.tool_zoom_worker(&self.fc, &label);
                }
                let id = int_arg(a, "id")?;
                let n = int_arg(a, "n")?;
                let offset = if a.contains_key("offset") {
                    int_arg(a, "offset")?
                } else {
                    0
                };
                let size = if a.contains_key("size") {
                    int_arg(a, "size")?
                } else {
                    0
                };
                let g = self.fc.lock();
                match g.as_ref() {
                    None => self.no_chat(),
                    Some(c) => c.zoom_page(id, n, offset, size),
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
                let background = match a.get("background") {
                    None => false,
                    Some(Value::Bool(b)) => *b,
                    Some(_) => return Err("background must be true or false".into()),
                };
                if self.standing {
                    return s.tool_spawn_opts(&self.fc, &project, &brief, &profile, &model, background);
                }
                s.tool_spawn_background(&project, &brief, &profile, &model, background)
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

/// Writes one reply. The result says whether the model's side got it: a
/// message added to a reply that was not written was not delivered.
fn write_response(conn: &mut UnixStream, r: &ToolResponse) -> std::io::Result<()> {
    let _ = conn.set_write_timeout(Some(Duration::from_secs(30)));
    conn.write_all(format!("{}\n", r.encode()).as_bytes())
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
        self.listen_tools(fc, &path, false, None, n, format!("c{n}"))
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
        turn: i64,
        tag: String,
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
            calls: AtomicI64::new(0),
            turn: AtomicI64::new(turn),
            mid: Mutex::new(Vec::new()),
            nonce: new_nonce(),
            seq: AtomicI64::new(0),
            tag,
            #[cfg(test)]
            call_fn: Mutex::new(None),
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
                // A call counts from the accept, so one in flight blocks a
                // replay.
                t.calls.fetch_add(1, Ordering::SeqCst);
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

    /// Runs when the chat opens. Each start line with no end is a call whose
    /// outcome is unknown: it gets one note in the chat, one line for the
    /// state of the next turn, and last an end line of outcome unknown. If
    /// the end line cannot be written the call is reported again at the next
    /// open, which is the safe side. If the call log cannot be read, nothing
    /// is known about any turn: one note says so and the second value is
    /// true. The first names the turns that had such a call. It is never run
    /// again (FM-R-66).
    pub fn settle_dangling_calls(
        self: &Arc<Server>,
        fc: &ForemanChat,
    ) -> (
        std::collections::HashSet<i64>,
        std::collections::HashSet<i64>,
        bool,
    ) {
        use crate::foreman::calls as c;
        let mut unknown = std::collections::HashSet::new();
        let mut started = std::collections::HashSet::new();
        // A torn tail is cut back so the next start line is whole (FM-R-67).
        let _ = c::repair_calls(&fc.dir);
        let recs = match c::read_calls(&fc.dir) {
            Ok(r) => r,
            Err(e) => {
                let note = format!(
                    "The foreman's call log could not be read ({e}). Whether a tool call ran before coppice stopped is unknown. Check the workers before you ask for anything again."
                );
                let _ = self.chat_append(fc, fchat::KIND_NOTE, &note);
                self.note(&note, "");
                fc.add_unknown("the call log could not be read, so a tool call may have run".into());
                return (unknown, started, true);
            }
        };
        // The one read of the file serves the dangling calls and the turns
        // that started a tool call (close_dangling_turns).
        for r in &recs {
            if r.phase == c::CALL_START && r.turn >= 0 {
                started.insert(r.turn);
            }
        }
        let now = self.fm_date();
        for r in c::dangling_calls(&recs) {
            let line = format!(
                "{}({}) started at {}, outcome unknown",
                r.tool,
                clip_args(&r.args),
                r.at
            );
            let note = format!(
                "{line}. It is not run again. Check the workers before you ask for it again."
            );
            let _ = self.chat_append(fc, fchat::KIND_NOTE, &note);
            self.note(&note, "");
            fc.add_unknown(line);
            if r.turn >= 0 {
                unknown.insert(r.turn);
            }
            let _ = c::append_call(
                &fc.dir,
                &c::CallRecord {
                    call: r.call.clone(),
                    phase: c::CALL_END.into(),
                    outcome: c::CALL_UNKNOWN.into(),
                    at: now.clone(),
                    ..Default::default()
                },
            );
        }
        (unknown, started, false)
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
