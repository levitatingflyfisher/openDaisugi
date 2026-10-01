//! The coppice daemon: one unix socket, one uid, one operator, one
//! instance. It owns the pane tree and the merged state, and it hands both
//! to command handlers registered by name.

use std::collections::{HashMap, HashSet};
use std::io::{self, BufReader, Write};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, AtomicU8, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender, TrySendError};
use std::sync::{Arc, Condvar, Mutex, RwLock, Weak};
use std::thread;
use std::time::{Duration, Instant, SystemTime};

use serde_json::{json, Value};

use crate::gojson;
use crate::layout;
use crate::proto::{self, Framing, Request, Response};
use crate::state;
use crate::sys;

mod agents;
pub mod ask;
mod attach;
mod children;
mod detect;
mod ended;
mod escalate;
mod facts;
pub mod foreman;
pub mod lock;
mod messages;
mod panes;
pub mod plugins;
mod projects;
mod ready;
mod role;
mod tasks;
pub mod voice;

#[cfg(test)]
mod datadir_tests;

pub use panes::LivePane;

/// The foreman's directory, for the test that keeps tests out of the real
/// home.
#[cfg(test)]
pub fn foreman_dir_for_tests() -> String {
    Server::foreman_dir()
}
pub use role::PeerFacts;

/// How long a stuck write may hold a connection's teardown open.
const WRITE_TEARDOWN_DEADLINE: Duration = Duration::from_secs(1);
/// How long Close's teardown of a connection waits for its handlers.
const HANDLER_TEARDOWN_WAIT: Duration = Duration::from_secs(2);
/// How long an ordinary connection's teardown waits for its handlers.
const PEER_CLOSE_HANDLER_WAIT: Duration = Duration::from_secs(130);

/// How many unsent events one client may fall behind by before it is
/// dropped.
pub const EVENT_QUEUE: usize = 256;

/// The whole of a server's configuration. There is no field that skips the
/// uid check.
#[derive(Clone, Debug, Default)]
pub struct Config {
    pub socket_path: PathBuf,
    pub data_dir: PathBuf,
    /// Where a pane.create with no cwd falls back to. Empty means the
    /// directory this process started in.
    pub start_dir: PathBuf,
    /// Whether the server runs a voice server of its own.
    pub voice: voice::VoiceConfig,
}

/// What a restart achieved, as server.status reports it.
#[derive(Clone, Debug, Default)]
pub struct RestoreReport {
    pub panes: i64,
    pub already_closed: i64,
    pub resumed: i64,
    pub marked_done: i64,
    pub marked_unknown: i64,
    pub notes: Vec<String>,
}

impl RestoreReport {
    fn to_value(&self) -> Value {
        gojson::obj(vec![
            ("panes", json!(self.panes)),
            ("already_closed", json!(self.already_closed)),
            ("resumed", json!(self.resumed)),
            ("marked_done", json!(self.marked_done)),
            ("marked_unknown", json!(self.marked_unknown)),
            ("notes", json!(self.notes)),
        ])
    }
}

/// The user-facing sentence on what a restart brings back.
pub const RESTART_NOTE: &str =
    "A restart brings back the layout, the labels and the working directories. \
It does not bring back running processes. Panes come back closed or unknown, never idle. \
A headless pane resumes only when its adapter recorded a harness session id.";

pub type Handler = Arc<dyn Fn(&Arc<Server>, &Arc<Client>, &Request) -> Response + Send + Sync>;

/// A writer that arms a fresh write deadline before every write once its
/// client has started tearing down.
struct TeardownWriter {
    s: UnixStream,
    tearing: Arc<AtomicBool>,
}

impl Write for TeardownWriter {
    fn write(&mut self, b: &[u8]) -> io::Result<usize> {
        if self.tearing.load(Ordering::SeqCst) {
            let _ = self.s.set_write_timeout(Some(WRITE_TEARDOWN_DEADLINE));
        }
        self.s.write(b)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.s.flush()
    }
}

/// One connection. Events go through a bounded queue drained by one
/// thread; replies go straight to the encoder.
pub struct Client {
    enc: proto::Encoder<TeardownWriter>,
    framing: AtomicU8,
    tearing: Arc<AtomicBool>,
    dead: Mutex<bool>,
    dead_cv: Condvar,
    events: SyncSender<Value>,
    pub st: Mutex<ClientState>,
}

/// What a connection holds under its lock.
#[derive(Default)]
pub struct ClientState {
    /// The panes this connection is attached to, each with the number of
    /// its attach, so a stale frame pump can tell it is stale.
    pub attached: HashMap<String, u64>,
    pub flows: HashMap<String, Arc<attach::PaneFlow>>,
    pub subs: HashMap<String, bool>,
    pub sub_panes: HashMap<String, bool>,
    /// The sub_panes entries pane.attach added on its own.
    pub auto_panes: HashSet<String>,
    /// The attached panes this connection may only watch.
    pub view_only: HashSet<String>,
    pub facts: PeerFacts,
    pub hello_role: String,
    pub hello_pane: String,
    pub hello_plugin: String,
    pub name: String,
    pub name_from: String,
    pub name_fixed: bool,
}

impl Client {
    fn new(stream: UnixStream) -> (Arc<Client>, Receiver<Value>) {
        let tearing = Arc::new(AtomicBool::new(false));
        let (tx, rx) = sync_channel(EVENT_QUEUE);
        let c = Arc::new(Client {
            enc: proto::Encoder::new(TeardownWriter {
                s: stream,
                tearing: tearing.clone(),
            }),
            framing: AtomicU8::new(0),
            tearing,
            dead: Mutex::new(false),
            dead_cv: Condvar::new(),
            events: tx,
            st: Mutex::new(ClientState::default()),
        });
        (c, rx)
    }

    pub fn framing(&self) -> Framing {
        if self.framing.load(Ordering::SeqCst) == 1 {
            Framing::JsonRpc
        } else {
            Framing::Native
        }
    }

    fn set_jsonrpc(&self) {
        self.framing.store(1, Ordering::SeqCst);
    }

    /// Writes one reply in the given framing.
    pub fn send(&self, r: &Response, f: Framing) -> io::Result<()> {
        self.enc.send_line(&proto::encode_response(r, f))
    }

    /// Writes one already-encoded line.
    pub fn send_line(&self, line: &str) -> io::Result<()> {
        self.enc.send_line(line)
    }

    /// Queues one event. A client that has fallen EVENT_QUEUE events behind
    /// is dropped rather than allowed to stall the server.
    pub fn emit(&self, v: Value) {
        if self.dead() {
            return;
        }
        match self.events.try_send(v) {
            Ok(()) => {}
            Err(TrySendError::Full(_)) | Err(TrySendError::Disconnected(_)) => self.drop_client(),
        }
    }

    /// Marks the client gone. Safe to call more than once.
    pub fn drop_client(&self) {
        let mut d = self.dead.lock().unwrap_or_else(|e| e.into_inner());
        *d = true;
        self.dead_cv.notify_all();
    }

    pub fn dead(&self) -> bool {
        *self.dead.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn pump(self: Arc<Client>, rx: Receiver<Value>) {
        loop {
            if self.dead() {
                return;
            }
            match rx.recv_timeout(Duration::from_millis(200)) {
                Ok(v) => match proto::encode_event(&v, self.framing()) {
                    Some(line) if self.send_line(&line).is_ok() => {}
                    _ => {
                        self.drop_client();
                        return;
                    }
                },
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {}
                Err(_) => return,
            }
        }
    }
}

/// The daemon.
pub struct Server {
    pub cfg: Config,
    started: Instant,
    handlers: RwLock<HashMap<String, Handler>>,
    serving: AtomicBool,
    closed: AtomicBool,
    inflight: AtomicI64,
    next_client: AtomicU64,
    clients: Mutex<HashMap<u64, Arc<Client>>>,
    lock: Mutex<Option<lock::StartLock>>,
    listening: Mutex<bool>,
    close_done: Mutex<bool>,
    close_cv: Condvar,
    restore: Mutex<RestoreReport>,
    detect_warnings: Mutex<Vec<String>>,

    pub tree: layout::Tree,
    pub states: state::Store,
    live: RwLock<HashMap<String, Arc<LivePane>>>,
    waiters: Mutex<Vec<Arc<agents::Waiter>>>,
    /// Every ask each pane reported, with its deadline and tier.
    asks: Mutex<HashMap<String, HashMap<String, role::NotedAsk>>>,
    /// The last notes, oldest first.
    notes: Mutex<Vec<Value>>,
    /// What each pane's hook reported beyond its state, and the
    /// transcripts each pane reads.
    facts: Mutex<facts::FactsMaps>,
    /// The gateway address and router, read at most every few seconds,
    /// and whether it answered.
    outside: Mutex<facts::Outside>,
    /// This server, for the threads a handler starts.
    me: Weak<Server>,
    /// The manifests the tick and the prompt rules read.
    detect_set: RwLock<Option<Arc<crate::detect::Set>>>,
    /// Stops the manifest tick.
    tick_stop: Arc<crate::pane::Flag>,
    /// The prompt queues and the trust answers.
    prompt: Mutex<ready::PromptState>,
    /// The asks held for a foreman and the input marks.
    holds: Mutex<escalate::HoldState>,
    /// The owner's words on their way to the floor's foreman.
    talk: Mutex<foreman::TalkQueue>,
    /// The chat files this process checked. Taken after talk, never
    /// before it.
    chat: Mutex<messages::ChatState>,
    /// Starts the watch for transcript growth once.
    watch_once: std::sync::Once,
    /// Stops the talk and prompt threads.
    talk_stop: Arc<crate::pane::Flag>,
    /// The subagents of each pane.
    children: Mutex<HashMap<String, HashMap<String, children::Child>>>,
    /// Held from a default label's pick until its record is in the tree.
    label_mu: Mutex<()>,
    /// Makes first creates on a fresh server share one workspace.
    place_mu: Mutex<()>,
    /// Orders presence events.
    presence_mu: Mutex<()>,
    /// Numbers attaches, so a stale frame pump knows it is stale.
    attach_gen: AtomicU64,
    /// Stops the hourly sweep of ended records.
    sweep_stop: Arc<crate::pane::Flag>,
    /// The ended records a pane.resume is in flight for.
    resuming: Mutex<HashSet<String>>,
    /// The enabled policies and the pids of the running ones.
    plug: Mutex<plugins::PlugState>,
    /// daisugi voice serve, for the floor.
    pub voice: Arc<voice::Voice>,
    /// True once close has shut the data dir. Every write into the data
    /// dir runs through write_data, which holds this lock for reading and
    /// checks it. Close takes it for writing to set it, so it waits for
    /// each write already in flight, and no write starts after it.
    data_shut: RwLock<bool>,
    /// Called by every write into the data dir just before the write.
    /// Unset in every real server. A test sets it before it starts any
    /// thread, and uses it to hold a write in flight while close runs.
    before_data_write: std::sync::OnceLock<Box<dyn Fn() + Send + Sync>>,
}

impl Server {
    pub fn new(mut cfg: Config) -> Arc<Server> {
        // Every pane is told the data dir, and the gate takes only an
        // absolute path there, so a relative one is made absolute once.
        if let Ok(abs) = sys::abs_path(&cfg.data_dir) {
            cfg.data_dir = abs;
        }
        if cfg.start_dir.as_os_str().is_empty() {
            if let Ok(wd) = std::env::current_dir() {
                cfg.start_dir = wd;
            }
        }
        let voice = voice::Voice::new(cfg.voice.clone(), cfg.data_dir.clone());
        let s = Arc::new_cyclic(|me| Server {
            cfg,
            started: Instant::now(),
            handlers: RwLock::new(HashMap::new()),
            serving: AtomicBool::new(false),
            closed: AtomicBool::new(false),
            inflight: AtomicI64::new(0),
            next_client: AtomicU64::new(1),
            clients: Mutex::new(HashMap::new()),
            lock: Mutex::new(None),
            listening: Mutex::new(false),
            close_done: Mutex::new(false),
            close_cv: Condvar::new(),
            restore: Mutex::new(RestoreReport::default()),
            detect_warnings: Mutex::new(Vec::new()),
            tree: layout::Tree::new(),
            states: state::Store::default(),
            live: RwLock::new(HashMap::new()),
            waiters: Mutex::new(Vec::new()),
            asks: Mutex::new(HashMap::new()),
            notes: Mutex::new(Vec::new()),
            facts: Mutex::new(facts::FactsMaps::default()),
            outside: Mutex::new(facts::Outside::default()),
            me: me.clone(),
            detect_set: RwLock::new(None),
            tick_stop: Arc::new(crate::pane::Flag::default()),
            prompt: Mutex::new(ready::PromptState::default()),
            holds: Mutex::new(escalate::HoldState::default()),
            talk: Mutex::new(foreman::TalkQueue::default()),
            chat: Mutex::new(messages::ChatState::default()),
            watch_once: std::sync::Once::new(),
            talk_stop: Arc::new(crate::pane::Flag::default()),
            children: Mutex::new(HashMap::new()),
            label_mu: Mutex::new(()),
            place_mu: Mutex::new(()),
            presence_mu: Mutex::new(()),
            attach_gen: AtomicU64::new(1),
            sweep_stop: Arc::new(crate::pane::Flag::default()),
            resuming: Mutex::new(HashSet::new()),
            plug: Mutex::new(plugins::PlugState::default()),
            voice,
            data_shut: RwLock::new(false),
            before_data_write: std::sync::OnceLock::new(),
        });
        let _ = s.handle("server.status", Arc::new(|s, c, r| s.handle_status(c, r)));
        panes::register(&s);
        tasks::register(&s);
        attach::register(&s);
        agents::register(&s);
        role::register(&s);
        children::register(&s);
        facts::register(&s);
        ready::register(&s);
        foreman::register(&s);
        messages::register(&s);
        ended::register(&s);
        projects::register(&s);
        voice::register(&s);
        s
    }

    /// Loads the manifests once, bundled then the operator's overrides,
    /// and starts the tick and pane.explain over them. A manifest that
    /// does not load is skipped with a warning server.status shows.
    pub fn start_detection(self: &Arc<Server>) {
        let set = Arc::new(crate::detect::Set::load(
            crate::detect::override_dir().as_deref(),
        ));
        *self
            .detect_warnings
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = set.warnings();
        *self.detect_set.write().unwrap_or_else(|e| e.into_inner()) = Some(set);
        detect::register(self);
    }

    /// The gate directory beside the data dir, where agent.allow and
    /// agent.deny answer.
    pub fn gate_root(&self) -> PathBuf {
        PathBuf::from(sys::clean(&format!(
            "{}/../gate",
            self.cfg.data_dir.to_string_lossy()
        )))
    }

    /// Registers a handler. Every handler is registered before Serve.
    pub fn handle(&self, name: &str, h: Handler) -> Result<(), String> {
        if self.serving.load(Ordering::SeqCst) {
            return Err(format!(
                "cannot register command {name:?}: Serve has already started. Register every handler before Serve"
            ));
        }
        self.handlers
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .insert(name.to_string(), h);
        Ok(())
    }

    /// Takes the single-instance lock. Call it first, before Listen.
    pub fn acquire_start_lock(&self) -> Result<(), String> {
        let mut g = self.lock.lock().unwrap_or_else(|e| e.into_inner());
        if g.is_some() {
            return Ok(());
        }
        match lock::acquire(&self.cfg.data_dir) {
            Ok(l) => {
                *g = Some(l);
                Ok(())
            }
            Err(lock::LockError::Held(pid)) => {
                let who = if pid > 0 {
                    format!("pid {pid}")
                } else {
                    "another process".to_string()
                };
                Err(format!(
                    "another coppice server holds the start lock: coppice server already running ({who}). Run: coppice server status"
                ))
            }
            Err(lock::LockError::Io(e)) => Err(e.to_string()),
        }
    }

    fn holds_start_lock(&self) -> bool {
        self.lock
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .is_some()
    }

    fn release_start_lock(&self) {
        let l = self.lock.lock().unwrap_or_else(|e| e.into_inner()).take();
        drop(l);
    }

    /// Binds the socket at 0600 in a 0700 directory. The start lock makes
    /// removing a stale socket file safe.
    pub fn listen(&self) -> Result<UnixListener, String> {
        if !self.holds_start_lock() {
            return Err("call AcquireStartLock before Listen. The lock is what makes removing a stale socket safe".into());
        }
        let mut l = self.listening.lock().unwrap_or_else(|e| e.into_inner());
        if *l {
            return Err("this server is already listening".into());
        }
        let sock = &self.cfg.socket_path;
        let dir = sock.parent().unwrap_or(Path::new("."));
        std::fs::create_dir_all(dir).map_err(|e| sys::go_path_err("mkdir", dir, &e))?;
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
            .map_err(|e| sys::go_path_err("chmod", dir, &e))?;
        let _ = std::fs::remove_file(sock);
        let ln = UnixListener::bind(sock)
            .map_err(|e| format!("listen unix {}: {}", sock.display(), sys::go_errno_text(&e)))?;
        if let Err(e) = std::fs::set_permissions(sock, std::fs::Permissions::from_mode(0o600)) {
            return Err(sys::go_path_err("chmod", sock, &e));
        }
        *l = true;
        Ok(ln)
    }

    /// Accepts connections until Close. Each connection is checked: a peer
    /// whose uid is not this process's own is refused.
    pub fn serve(self: &Arc<Server>, ln: UnixListener) {
        self.serving.store(true, Ordering::SeqCst);
        let me = sys::getuid();
        for conn in ln.incoming() {
            if self.is_closed() {
                return;
            }
            let conn = match conn {
                Ok(c) => c,
                Err(_) => {
                    if self.is_closed() {
                        return;
                    }
                    thread::sleep(Duration::from_millis(10));
                    continue;
                }
            };
            let s = self.clone();
            thread::spawn(move || {
                let pid = match sys::peer_cred(&conn) {
                    Some((pid, uid, _)) if uid == me => pid,
                    _ => {
                        let r = proto::err_resp(
                            "",
                            proto::ERR_UNAUTHORIZED,
                            "this socket serves one user. Run coppice as the user that started the server.",
                        );
                        let _ = (&conn).write_all(
                            (proto::encode_response(&r, Framing::Native) + "\n").as_bytes(),
                        );
                        return;
                    }
                };
                let facts = s.facts_of(Some(pid));
                s.serve_conn(conn, facts);
            });
        }
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::SeqCst)
    }

    fn command_list(&self) -> String {
        let h = self.handlers.read().unwrap_or_else(|e| e.into_inner());
        let mut names: Vec<&String> = h.keys().collect();
        names.sort();
        names
            .iter()
            .map(|s| s.as_str())
            .collect::<Vec<_>>()
            .join(", ")
    }

    fn serve_conn(self: &Arc<Server>, conn: UnixStream, facts: PeerFacts) {
        let reader = match conn.try_clone() {
            Ok(r) => r,
            Err(_) => return,
        };
        let id = self.next_client.fetch_add(1, Ordering::SeqCst);
        let (c, rx) = Client::new(conn);
        c.st.lock().unwrap_or_else(|e| e.into_inner()).facts = facts;
        {
            let pc = c.clone();
            thread::spawn(move || pc.pump(rx));
        }
        self.clients
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(id, c.clone());
        let inflight_here = Arc::new((Mutex::new(0i64), Condvar::new()));
        let mut d = proto::Decoder::new(BufReader::with_capacity(64 << 10, reader));
        while let Ok(line) = d.next_line() {
            if c.dead() || self.is_closed() {
                break;
            }
            let text = String::from_utf8_lossy(&line).into_owned();
            if text.trim().is_empty() {
                continue;
            }
            let (req, line_framing, err) = proto::decode_line(&text);
            if line_framing == Framing::JsonRpc {
                c.set_jsonrpc();
            }
            let f = c.framing();
            if let Some(de) = err {
                let mut resp = proto::err_resp(&req.id, proto::ERR_BAD_REQUEST, &de.message);
                resp.raw_id = Some(req.raw_id.clone().unwrap_or_else(|| "null".into()));
                resp.rpc_code = de.code;
                let _ = c.send(&resp, f);
                continue;
            }
            if f == Framing::JsonRpc && line_framing == Framing::Native {
                let mut resp = proto::err_resp(
                    &req.id,
                    proto::ERR_BAD_REQUEST,
                    "this connection speaks JSON-RPC 2.0. Send jsonrpc 2.0 on every line.",
                );
                resp.raw_id = Some(req.raw_id.clone().unwrap_or_else(|| "null".into()));
                resp.rpc_code = proto::RPC_INVALID_REQUEST;
                let _ = c.send(&resp, f);
                continue;
            }
            if req.cmd == "hello" {
                let h = self
                    .handlers
                    .read()
                    .unwrap_or_else(|e| e.into_inner())
                    .get("hello")
                    .cloned();
                if let Some(h) = h {
                    let mut resp = h(self, &c, &req);
                    resp.raw_id = req.raw_id.clone();
                    let _ = c.send(&resp, f);
                    continue;
                }
            }
            let h = self
                .handlers
                .read()
                .unwrap_or_else(|e| e.into_inner())
                .get(&req.cmd)
                .cloned();
            let h = match h {
                Some(h) => h,
                None => {
                    let mut resp = proto::err_resp(
                        &req.id,
                        proto::ERR_BAD_REQUEST,
                        &format!(
                            "no command {}. Known commands: {}",
                            sys::go_quote(&req.cmd),
                            self.command_list()
                        ),
                    );
                    resp.raw_id = req.raw_id.clone();
                    resp.rpc_code = proto::RPC_METHOD_NOT_FOUND;
                    let _ = c.send(&resp, f);
                    continue;
                }
            };
            // Each request runs on its own thread, so a long wait never
            // holds up the other verbs on this connection.
            {
                let (m, _) = &*inflight_here;
                *m.lock().unwrap_or_else(|e| e.into_inner()) += 1;
            }
            self.inflight.fetch_add(1, Ordering::SeqCst);
            let s = self.clone();
            let c2 = c.clone();
            let here = inflight_here.clone();
            thread::spawn(move || {
                let (mut resp, allowed) = s.guard(&c2, &req);
                if allowed {
                    s.note_verb(&c2, &req);
                    resp = h(&s, &c2, &req);
                }
                resp.raw_id = req.raw_id.clone();
                let _ = c2.send(&resp, f);
                s.inflight.fetch_sub(1, Ordering::SeqCst);
                let (m, cv) = &*here;
                *m.lock().unwrap_or_else(|e| e.into_inner()) -= 1;
                cv.notify_all();
            });
        }
        // Teardown: drop first, so a waiting handler sees it, then wait,
        // bounded, for this connection's own handlers.
        c.drop_client();
        self.clients
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(&id);
        self.on_client_gone(&c);
        c.tearing.store(true, Ordering::SeqCst);
        let wait = if self.is_closed() {
            HANDLER_TEARDOWN_WAIT
        } else {
            PEER_CLOSE_HANDLER_WAIT
        };
        let (m, cv) = &*inflight_here;
        let g = m.lock().unwrap_or_else(|e| e.into_inner());
        drop(cv.wait_timeout_while(g, wait, |n| *n > 0));
    }

    /// The people this client counted as looking stop counting.
    fn on_client_gone(self: &Arc<Server>, c: &Arc<Client>) {
        let gone: Vec<String> =
            c.st.lock()
                .unwrap_or_else(|e| e.into_inner())
                .attached
                .keys()
                .cloned()
                .collect();
        for id in gone {
            self.tell_presence(&id);
        }
    }

    /// Every live client.
    pub fn clients(&self) -> Vec<Arc<Client>> {
        self.clients
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect()
    }

    /// Sends one event to every client that asked for its kind and pane.
    pub fn broadcast(&self, kind: &str, pane_id: &str, v: &Value) {
        let clients: Vec<Arc<Client>> = self
            .clients
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect();
        for c in clients {
            let want = {
                let st = c.st.lock().unwrap_or_else(|e| e.into_inner());
                st.subs.get(kind).copied().unwrap_or(false)
                    && (st.sub_panes.get("*").copied().unwrap_or(false)
                        || st.sub_panes.get(pane_id).copied().unwrap_or(false))
            };
            if want {
                c.emit(v.clone());
            }
        }
    }

    pub fn detection_warnings(&self) -> Vec<String> {
        self.detect_warnings
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    fn handle_status(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let panes = self.tree.panes();
        let live = panes.iter().filter(|p| !p.closed).count();
        let restore = self
            .restore
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .to_value();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("protocol", json!(proto::VERSION)),
                ("pid", json!(std::process::id())),
                ("socket", json!(self.cfg.socket_path.to_string_lossy())),
                ("data_dir", json!(self.cfg.data_dir.to_string_lossy())),
                ("uptime_s", json!(self.started.elapsed().as_secs())),
                ("panes", json!(panes.len())),
                ("panes_live", json!(live)),
                ("resumable", json!(self.resumable_count())),
                ("restart_note", json!(RESTART_NOTE)),
                ("restore", restore),
                ("detection_warnings", json!(self.detection_warnings())),
            ]),
        )
    }

    /// Stops serving, drops every client, waits (bounded) for in-flight
    /// handlers, then releases the start lock. A second call waits for the
    /// first to finish.
    pub fn close(self: &Arc<Server>) {
        if self.closed.swap(true, Ordering::SeqCst) {
            let g = self.close_done.lock().unwrap_or_else(|e| e.into_inner());
            drop(self.close_cv.wait_while(g, |done| !*done));
            return;
        }
        // Wake the accept loop: a connection to our own socket returns
        // from accept, and the loop sees closed.
        let _ = UnixStream::connect(&self.cfg.socket_path);
        // Go's listener removes its socket file when it closes, so a
        // client that dials after the stop finds no socket at all.
        if *self.listening.lock().unwrap_or_else(|e| e.into_inner()) {
            let _ = std::fs::remove_file(&self.cfg.socket_path);
        }
        let clients: Vec<Arc<Client>> = self
            .clients
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect();
        for c in clients {
            c.drop_client();
        }
        self.talk_stop.set();
        self.tick_stop.set();
        self.sweep_stop.set();
        self.stop_holds();
        self.teardown_live_panes();
        let deadline = Instant::now() + HANDLER_TEARDOWN_WAIT;
        while self.inflight.load(Ordering::SeqCst) > 0 && Instant::now() < deadline {
            thread::sleep(Duration::from_millis(5));
        }
        // A create that was already running when close began is caught by
        // this second pass.
        self.teardown_live_panes();
        // The voice process goes before the lock, so a server started right
        // after this one never finds the old voice server still running.
        self.voice.stop();
        // No write into the data dir may outlive the server: this waits
        // for any write still in flight and refuses every later one,
        // before the lock is released.
        self.shut_data();
        self.release_start_lock();
        let mut g = self.close_done.lock().unwrap_or_else(|e| e.into_inner());
        *g = true;
        self.close_cv.notify_all();
    }

    /// Runs one write into the data dir, unless close has shut it. True
    /// when the write ran. A write must not call write_data again: a second
    /// read lock behind a waiting close can deadlock.
    pub(crate) fn write_data(&self, write: impl FnOnce()) -> bool {
        let shut = self.data_shut.read().unwrap_or_else(|e| e.into_inner());
        if *shut {
            return false;
        }
        if let Some(h) = self.before_data_write.get() {
            h();
        }
        write();
        true
    }

    /// Waits for every write into the data dir in flight and refuses every
    /// later one.
    fn shut_data(&self) {
        *self.data_shut.write().unwrap_or_else(|e| e.into_inner()) = true;
    }

    /// Seconds since the epoch, as a float, the way Go stamps events.
    pub fn now_seconds() -> f64 {
        SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .map(|d| d.as_nanos() as f64 / 1e9)
            .unwrap_or(0.0)
    }
}
