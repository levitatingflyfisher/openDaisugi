//! The foreman in chat mode: one chat that never ends. The owner's words
//! are logged, the compactor builds the summary tree, and one fresh model
//! run answers each owner message. The twin of Go's foreman_chat.go.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, SystemTime};

use serde_json::{json, Value};

use super::messages::{self, Message as ChatMessage};
use super::Server;
use crate::config;
use crate::foreman::chat::{self as fchat, Chat, ViewLine};
use crate::foreman::compactor::{CallError, CallUsage, ModelSummarizer};
use crate::foreman::ctx::Ctx;
use crate::foreman::turns::{self, TurnRecord};
use crate::gojson;
use crate::pane::Flag;
use crate::proto::{self, Request, Response};

/// A clock for the chat mode; None is the system clock. A test sets one.
#[derive(Clone, Default)]
pub struct ForemanClock(pub Option<fchat::Clock>);

impl std::fmt::Debug for ForemanClock {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(if self.0.is_some() {
            "test clock"
        } else {
            "system clock"
        })
    }
}

/// The [foreman] table as the server uses it.
#[derive(Clone, Debug, Default)]
pub struct ForemanConfig {
    /// "pane" (the default) or "chat".
    pub mode: String,
    /// Where the chat's summaries are made. compactor_model "" is haiku on
    /// the default profile and the profile's own model on any other.
    pub compactor_profile: String,
    pub compactor_model: String,
    /// Where each turn runs: "" is Claude Code, any other value is the name
    /// of an operator agent profile.
    pub turn_profile: String,
    /// The chain of turn profiles, in order. When it is set it wins over
    /// turn_profile; see turn_chain.
    pub turn_profiles: Vec<String>,
    /// The one model of every account in the chain; "" is Claude
    /// Code's own default, or the profile's own model. A chain of
    /// several accounts needs it (the config checks).
    pub turn_model: String,
    /// How long one turn may take; zero means FOREMAN_TURN_TIMEOUT. A test
    /// sets it small.
    pub turn_timeout: Duration,
    /// The bounds of the chat's view; zero means the defaults. A test sets
    /// them small.
    pub view_max: usize,
    pub view_min: usize,
    /// When set, the foreman's log dir. Empty means the rule the coppice
    /// foreman command reads by.
    pub log_dir: String,
    /// Why the [foreman] table did not read, or "". The server then runs in
    /// pane mode and says so in a note.
    pub problem: String,
    /// The clock of the chat, its turns and its workers.
    pub clock: ForemanClock,
    /// The binaries the chat mode starts; empty means $COPPICE_CLAUDE_BIN,
    /// $COPPICE_SPRIG_BIN and $COPPICE_DAISUGI_BIN, else the plain names. A
    /// test sets them; a test never lets them fall back to the real ones.
    pub claude_bin: String,
    pub sprig_bin: String,
    pub daisugi_bin: String,
    /// The command that runs `coppice foreman` (the MCP relay and the
    /// hook); empty means this executable. A test sets it.
    pub self_cmd: Vec<String>,
    /// More variable names a sprig that coppice starts may get. A real
    /// server keeps none; a test that runs fakes which read their script
    /// from the environment names them.
    pub sprig_env_keep: Vec<String>,
    /// How long the compactor waits after a failed drain with no wait of
    /// its own; zero is a minute.
    pub compact_retry: Duration,
    /// Caps one compactor call; zero is the default.
    pub compact_timeout: Duration,
    /// The home dir import reads old Claude Code and Codex chats under; empty
    /// means $HOME. A test sets it.
    pub import_home: String,
    /// How often the compactor loop looks at import.json for a change the
    /// command made; zero means 5 s. A test sets it small.
    pub import_poll: Duration,
}

impl ForemanConfig {
    /// The server's view of settings read from coppice.toml.
    pub fn of(s: &config::ForemanSettings) -> ForemanConfig {
        ForemanConfig {
            mode: s.mode.clone(),
            compactor_profile: s.compactor_profile.clone(),
            compactor_model: s.compactor_model.clone(),
            turn_profiles: s.turn_profiles.clone(),
            turn_model: s.turn_model.clone(),
            ..Default::default()
        }
    }

    /// The table with its defaults filled in.
    pub fn fill(&mut self) {
        if self.mode.is_empty() {
            self.mode = config::FOREMAN_PANE.into();
        }
        if self.compactor_profile.is_empty() {
            self.compactor_profile = config::DEFAULT_COMPACTOR_PROFILE.into();
        }
        if self.turn_timeout.is_zero() {
            self.turn_timeout = config::FOREMAN_TURN_TIMEOUT;
        }
    }
}

/// What coppice.toml's [foreman] table asks of the server. A file that does
/// not read, or a value that is not allowed, leaves the foreman in pane
/// mode and says why in a note: the floor always starts.
pub fn foreman_config() -> ForemanConfig {
    let cfg = match config::load() {
        Err(e) => {
            return ForemanConfig {
                problem: format!(
                    "The foreman stays in pane mode: {} does not read ({e}). Fix the file, then start the coppice server again.",
                    config::path().display()
                ),
                ..Default::default()
            }
        }
        Ok(c) => c.unwrap_or_default(),
    };
    match cfg.foreman_settings() {
        Ok(s) => ForemanConfig::of(&s),
        Err(e) => ForemanConfig {
            problem: format!("The foreman stays in pane mode: {e}."),
            ..Default::default()
        },
    }
}

/// The chat is not open.
pub const ERR_NO_CHAT: &str = "the foreman's chat is not open";

/// A wake-up for the compactor: set by wake, taken by wait.
#[derive(Default)]
pub struct Kick {
    m: Mutex<bool>,
    cv: Condvar,
}

impl Kick {
    pub fn wake(&self) {
        *self.m.lock().unwrap_or_else(|e| e.into_inner()) = true;
        self.cv.notify_all();
    }

    /// Waits up to d, or until woken or ctx ends. True when woken.
    fn wait(&self, ctx: &Ctx, d: Duration) -> bool {
        let end = std::time::Instant::now() + d;
        let mut g = self.m.lock().unwrap_or_else(|e| e.into_inner());
        loop {
            if *g {
                *g = false;
                return true;
            }
            let now = std::time::Instant::now();
            if ctx.err().is_some() || now >= end {
                return false;
            }
            let step = (end - now).min(Duration::from_millis(50));
            g = self
                .cv
                .wait_timeout(g, step)
                .unwrap_or_else(|e| e.into_inner())
                .0;
        }
    }
}

/// The frozen view of a turn's system prompt: the lines of the snapshot,
/// and whether one was frozen.
#[derive(Default)]
pub struct Snap {
    pub lines: Vec<ViewLine>,
    pub set: bool,
}

/// The foreman's chat while the server runs in chat mode, or the table of
/// the pane foreman's workers (with no chat).
pub struct ForemanChat {
    pub dir: PathBuf,
    /// The chat lock and the chat: every call on the chat holds it.
    pub chat: Mutex<Option<Chat>>,
    /// Why the chat is not open, in plain words, or "".
    pub problem: String,
    sum: Option<Arc<ModelSummarizer>>,
    /// The snapshot of the claude runner; taken after the chat lock.
    pub snap: Mutex<Snap>,
    /// The lines of <state> for tool calls whose outcome is unknown after a
    /// stop or a crash (FM-R-66). The next turn shows them once.
    pub unknown: Mutex<Vec<String>>,
    kick: Kick,
    pub(super) cancel: Ctx,
    done: Flag,
    /// Jobs that run beside the chat; close waits for them.
    pub bg: super::foreman_turn::WaitGroup,
    /// Feeds the import queue (FM-R-72); only the compactor loop uses it.
    pub(super) imp: Mutex<Option<crate::foreman::import::Importer>>,
    /// The last estimate of what could be imported, and whether there is one.
    pub(super) est: Mutex<(bool, Vec<crate::foreman::import::ImportSession>)>,
    /// The scan in flight, if any: one scan at a time, and a caller that
    /// wants one joins it (FM-R-83). gen counts the accepts, so a scan that
    /// began before one does not set the estimate. Lock order: est_flight,
    /// then est.
    pub(super) est_flight: Mutex<EstFlight>,
    pub(super) est_cv: Condvar,
    /// Runs at the start of each scan; tests hold one open.
    pub(super) scan_hold: Mutex<Option<Arc<dyn Fn() + Send + Sync>>>,
    /// When the floor was last told that the import moved.
    pub(super) imp_event: Mutex<Option<std::time::Instant>>,
}

/// The poll the knob gives (see poll_every). "off" is a poll so long that
/// only a verb wakes the loop.
pub(super) fn poll_from_env(hooks: Option<&str>, v: Option<&str>) -> Duration {
    let five = Duration::from_secs(5);
    if hooks != Some("1") {
        return five;
    }
    match v {
        Some("off") => Duration::from_secs(365 * 24 * 3600),
        Some(v) => v
            .parse::<u64>()
            .ok()
            .filter(|ms| *ms > 0)
            .map_or(five, |ms| Duration::from_millis(ms.clamp(10, 5000))),
        None => five,
    }
}

/// The state of the estimate scan.
#[derive(Default)]
pub(super) struct EstFlight {
    pub running: bool,
    pub gen: u64,
    /// How many scans ran.
    pub scans: u64,
    /// The transcript signature before, and the time of, the scan that set
    /// the estimate: accept reuses it only while both are fresh (F-1).
    pub sig: String,
    pub at: Option<std::time::Instant>,
}

/// Frees the scan flight when dropped, also when the scan panics (F-2).
pub(super) struct FlightGuard<'a>(pub &'a ForemanChat);

impl Drop for FlightGuard<'_> {
    fn drop(&mut self) {
        let mut fl = self.0.est_flight.lock().unwrap_or_else(|e| e.into_inner());
        fl.running = false;
        self.0.est_cv.notify_all();
    }
}

impl ForemanChat {
    /// True when a node waits to be built.
    pub(super) fn has_pending(&self) -> bool {
        self.lock().as_ref().is_some_and(|c| c.pending() > 0)
    }

    /// A table with no chat: the pane foreman's, or a closed server's.
    pub fn empty(dir: PathBuf, problem: &str) -> Arc<ForemanChat> {
        let fc = ForemanChat {
            dir,
            chat: Mutex::new(None),
            problem: problem.to_string(),
            sum: None,
            snap: Mutex::new(Snap::default()),
            unknown: Mutex::new(Vec::new()),
            kick: Kick::default(),
            cancel: Ctx::background(),
            done: Flag::default(),
            bg: Default::default(),
            imp: Mutex::new(None),
            est: Mutex::new((false, Vec::new())),
            est_flight: Mutex::new(EstFlight::default()),
            est_cv: Condvar::new(),
            scan_hold: Mutex::new(None),
            imp_event: Mutex::new(None),
        };
        fc.done.set();
        Arc::new(fc)
    }

    /// Adds a state line for a tool call whose outcome is unknown.
    pub fn add_unknown(&self, line: String) {
        self.unknown
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(line);
    }

    /// The state lines of unknown tool outcomes. They stay until a model
    /// has seen them (drop_unknown).
    pub fn peek_unknown(&self) -> Vec<String> {
        self.unknown.lock().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// Removes the first n state lines: a model has seen them.
    pub fn drop_unknown(&self, n: usize) {
        let mut g = self.unknown.lock().unwrap_or_else(|e| e.into_inner());
        let n = n.min(g.len());
        g.drain(..n);
    }

    pub fn lock(&self) -> std::sync::MutexGuard<'_, Option<Chat>> {
        self.chat.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Whether the chat is open.
    pub fn has_chat(&self) -> bool {
        self.lock().is_some()
    }

    /// Asks the compactor to look for work.
    pub fn wake(&self) {
        self.kick.wake();
    }
}

/// What New sets up for the chat mode.
#[derive(Default)]
pub struct ChatModeState {
    pub fc: Option<Arc<ForemanChat>>,
    pub shut: bool,
}

/// The binaries the chat mode starts.
fn env_bin(over: &str, env: &str, def: &str) -> String {
    if !over.is_empty() {
        return over.to_string();
    }
    match std::env::var(env) {
        Ok(b) if !b.is_empty() => b,
        _ => def.to_string(),
    }
}

/// The last line of a process's complaint, the one that names the fault,
/// cut at 200 bytes; err's text when there is none.
pub fn first_line(text: &str, err: &str) -> String {
    let text = text.trim();
    let line = match text.rfind('\n') {
        Some(i) => &text[i + 1..],
        None => text,
    };
    if line.is_empty() && !err.is_empty() {
        return err.to_string();
    }
    let mut n = line.len().min(200);
    while n > 0 && !line.is_char_boundary(n) {
        n -= 1;
    }
    line[..n].to_string()
}

/// One profile in `daisugi agent-profiles bundle`.
#[derive(Default, serde::Deserialize)]
pub struct BundleEntry {
    #[serde(default)]
    pub wire: String,
    #[serde(default)]
    pub base_url: Option<String>,
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub key: Option<String>,
}

impl Drop for BundleEntry {
    fn drop(&mut self) {
        if let Some(k) = self.key.as_mut() {
            // SAFETY: the bytes are overwritten with zeros, which is UTF-8.
            unsafe { k.as_bytes_mut().fill(0) };
        }
    }
}

impl Server {
    /// Whether the foreman runs as a chat.
    pub fn chat_mode(&self) -> bool {
        self.cfg.foreman.mode == config::FOREMAN_CHAT
    }

    /// The chat mode's clock.
    pub fn fm_now(&self) -> SystemTime {
        match &self.cfg.foreman.clock.0 {
            Some(f) => f(),
            None => SystemTime::now(),
        }
    }

    /// The chat mode's clock as a message date.
    pub fn fm_date(&self) -> String {
        fchat::date_text(self.fm_now())
    }

    pub fn claude_bin(&self) -> String {
        env_bin(&self.cfg.foreman.claude_bin, "COPPICE_CLAUDE_BIN", "claude")
    }

    pub fn sprig_bin(&self) -> String {
        env_bin(&self.cfg.foreman.sprig_bin, "COPPICE_SPRIG_BIN", "sprig")
    }

    pub fn daisugi_bin(&self) -> String {
        env_bin(
            &self.cfg.foreman.daisugi_bin,
            "COPPICE_DAISUGI_BIN",
            "daisugi",
        )
    }

    /// The foreman's log dir, by the rule the CLI reader uses.
    pub fn foreman_log_dir(&self) -> PathBuf {
        if !self.cfg.foreman.log_dir.is_empty() {
            return PathBuf::from(&self.cfg.foreman.log_dir);
        }
        crate::foreman::log_dir(&self.cfg.data_dir)
    }

    /// Where chat-mode turns work: a dir of their own under the log dir, so
    /// each data dir has its own.
    pub fn turn_dir(&self) -> PathBuf {
        self.foreman_log_dir().join("turns")
    }

    fn chat_mode_lock(&self) -> std::sync::MutexGuard<'_, ChatModeState> {
        self.fm.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Opens the foreman's chat when the server runs in chat mode. It is
    /// idempotent, and floor.talk calls it if the caller did not. The floor
    /// always starts: a log that cannot be opened is a plain error in a
    /// note and in each floor.talk reply.
    pub fn start_foreman_chat(self: &Arc<Server>) {
        if self.chat_mode() {
            self.ensure_chat();
        }
    }

    pub fn ensure_chat(self: &Arc<Server>) -> Arc<ForemanChat> {
        let mut st = self.chat_mode_lock();
        if let Some(fc) = &st.fc {
            return fc.clone();
        }
        if st.shut {
            return ForemanChat::empty(PathBuf::new(), "this server has closed");
        }
        // The open finishes under the lock, so a second caller waits for a
        // chat whose dangling turns are closed and whose workers are read.
        let fc = self.open_chat();
        let fresh = fc.has_chat();
        if fresh {
            self.after_chat_open(&fc);
            fc.bg.add();
        }
        st.fc = Some(fc.clone());
        drop(st);
        if fresh {
            // The model check runs a subprocess, so it runs after the chat
            // lock is released: a slow daisugi cannot stall floor.talk
            // (FM-R-58).
            let (s, f) = (self.clone(), fc.clone());
            thread::spawn(move || {
                s.check_models_at_open(&f);
                s.offer_import(&f);
                f.bg.done();
            });
        }
        fc
    }

    fn open_chat(self: &Arc<Server>) -> Arc<ForemanChat> {
        let dir = self.foreman_log_dir();
        let me = Arc::downgrade(self);
        let mut sum = ModelSummarizer::new(Box::new(
            move |ctx: &Ctx, system: &str, prompt: &str| match me.upgrade() {
                Some(s) => s.compactor_call(ctx, system, prompt),
                None => Err("this server has closed".into()),
            },
        ));
        sum.usage_dir = Some(dir.clone());
        sum.now = self.cfg.foreman.clock.0.clone();
        sum.timeout = self.cfg.foreman.compact_timeout;
        let opened = Chat::open(fchat::Config {
            dir: dir.clone(),
            now: self.cfg.foreman.clock.0.clone(),
            view_max: self.cfg.foreman.view_max,
            view_min: self.cfg.foreman.view_min,
            ..Default::default()
        });
        let chat = match opened {
            Ok(c) => c,
            Err(e) => {
                let problem = format!(
                    "The foreman's chat in {} could not be opened: {e}. Fix that, then start coppice again. The rest of the floor works.",
                    dir.display()
                );
                self.note(&problem, "");
                return ForemanChat::empty(dir, &problem);
            }
        };
        let notice = chat.notice().to_string();
        let dir_for_imp = dir.clone();
        let fc = Arc::new(ForemanChat {
            dir,
            chat: Mutex::new(Some(chat)),
            problem: String::new(),
            sum: Some(Arc::new(sum)),
            snap: Mutex::new(Snap::default()),
            unknown: Mutex::new(Vec::new()),
            kick: Kick::default(),
            cancel: Ctx::background(),
            done: Flag::default(),
            bg: Default::default(),
            imp: Mutex::new(Some({
                let mut imp = crate::foreman::import::Importer::new(dir_for_imp);
                imp.skip = self.import_skip();
                imp
            })),
            est: Mutex::new((false, Vec::new())),
            est_flight: Mutex::new(EstFlight::default()),
            est_cv: Condvar::new(),
            scan_hold: Mutex::new(None),
            imp_event: Mutex::new(None),
        });
        self.sweep_turn_dir();
        if !notice.is_empty() {
            if let Some(c) = fc.lock().as_mut() {
                let _ = c.append(fchat::KIND_NOTE, &notice);
            }
            self.note(&notice, "");
        }
        let (s, f) = (self.clone(), fc.clone());
        thread::spawn(move || s.compactor_loop(f));
        // The queue has a loop of its own: a compactor that waits to retry
        // a failed summary must not hold back a queued job.
        let (s, f) = (self.clone(), fc.clone());
        fc.bg.add();
        thread::spawn(move || {
            s.pool_loop(&f);
            f.bg.done();
        });
        fc.wake();
        fc
    }

    /// The steps of an open that may append to the chat: they run once the
    /// chat is in place, so an append finds it.
    fn after_chat_open(self: &Arc<Server>, fc: &Arc<ForemanChat>) {
        let (unknown, started, unreadable) = self.settle_dangling_calls(fc);
        self.close_dangling_turns(fc, &unknown, &started, unreadable);
        self.load_workers(fc);
        self.failover_chain(fc);
    }

    /// Removes the files a turn that did not end left in this data dir's
    /// turn dir: its system prompt, its view and its socket. A file newer
    /// than the turn timeout plus a minute stays, and so do a directory and
    /// a file of another name.
    fn sweep_turn_dir(&self) {
        let dir = self.turn_dir();
        // A clock too early to step back that far has no file old enough.
        let Some(cutoff) = self
            .fm_now()
            .checked_sub(self.cfg.foreman.turn_timeout + Duration::from_secs(60))
        else {
            return;
        };
        let Ok(rd) = std::fs::read_dir(&dir) else {
            return;
        };
        let mut names: Vec<PathBuf> = rd.flatten().map(|e| e.path()).collect();
        names.sort();
        for p in names {
            let n = p
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            let matches = n.starts_with("t-")
                && (n.ends_with(".system") || n.ends_with(".view") || n.ends_with(".sock"));
            if !matches {
                continue;
            }
            if let Ok(m) = std::fs::symlink_metadata(&p) {
                let old = m.modified().is_ok_and(|t| t < cutoff);
                if !m.is_dir() && old {
                    let _ = std::fs::remove_file(&p);
                }
            }
        }
    }

    /// How often the compactor loop looks at import.json. A test sets
    /// import_poll. COPPICE_IMPORT_POLL_MS is a test knob for the compare
    /// cases: read only when COPPICE_TEST_HOOKS=1, a number of ms clamped to
    /// 10..5000, or "off" for no poll, so a verb is the only thing that
    /// wakes the loop (FM-R-84).
    pub(super) fn poll_every(&self) -> Duration {
        if !self.cfg.foreman.import_poll.is_zero() {
            return self.cfg.foreman.import_poll;
        }
        poll_from_env(
            std::env::var("COPPICE_TEST_HOOKS").ok().as_deref(),
            std::env::var("COPPICE_IMPORT_POLL_MS").ok().as_deref(),
        )
    }

    /// Builds the nodes that need a model, whenever it is woken, until the
    /// chat closes. A failed drain leaves the nodes pending; it is tried
    /// again after a wait or at the next wake.
    fn compactor_loop(self: Arc<Server>, fc: Arc<ForemanChat>) {
        let ctx = fc.cancel.clone();
        let Some(sum) = fc.sum.clone() else {
            fc.done.set();
            return;
        };
        let poll = self.poll_every();
        loop {
            let woke = fc.kick.wait(&ctx, poll);
            if ctx.err().is_some() {
                fc.done.set();
                return;
            }
            // The import command or a click may have changed import.json
            // while the loop was idle.
            // Or a turn ended with an import node held back.
            if !woke && !fc.import_wanted() && !fc.has_pending() {
                continue;
            }
            loop {
                let res = fchat::drain_locked_gated(&fc.chat, &ctx, &*sum, &|| !self.turns_idle());
                // Live nodes first: only a drained chat feeds the import
                // queue, one message at a time (FM-R-72).
                if res.is_ok() && ctx.err().is_none() && self.import_feed(&fc) {
                    continue;
                }
                let Err(e) = res else { break };
                if ctx.err().is_some() {
                    break;
                }
                self.note_once(&format!(
                    "The foreman could not summarize its chat: {e}. Nothing is lost. It tries again."
                ));
                let mut wait = self.cfg.foreman.compact_retry;
                if wait.is_zero() {
                    wait = Duration::from_secs(60);
                }
                if let Some(d) = fc.lock().as_ref().and_then(Chat::retry_in) {
                    wait = d.max(Duration::from_secs(1));
                }
                fc.kick.wait(&ctx, wait);
                if ctx.err().is_some() {
                    break;
                }
            }
            if ctx.err().is_some() {
                fc.done.set();
                return;
            }
        }
    }

    /// Stops the compactor and closes the chat. A call to the model in
    /// flight is cancelled; its node stays pending for the next start. It
    /// waits for the compactor to stop, so no node is written after it
    /// returns.
    pub fn close_foreman_chat(&self) {
        let fc = {
            let mut st = self.chat_mode_lock();
            st.shut = true;
            st.fc.clone()
        };
        let Some(fc) = fc else { return };
        fc.cancel.cancel();
        fc.kick.wake();
        fc.done.wait(None);
        fc.bg.wait(Duration::from_secs(15));
        let chat = fc.lock().take();
        if let Some(mut c) = chat {
            c.close();
        }
    }

    /// Logs one message under the chat lock, wakes the compactor and tells
    /// subscribers there are new messages.
    pub fn chat_append(&self, fc: &ForemanChat, kind: &str, text: &str) -> Result<(), String> {
        let err = {
            let mut g = fc.lock();
            let Some(c) = g.as_mut() else {
                return Err(ERR_NO_CHAT.into());
            };
            c.append(kind, text).1
        };
        fc.wake();
        match err {
            None => {
                self.send_messages_event("", true);
                Ok(())
            }
            Some(e) => Err(e),
        }
    }

    /// Where the compactor keeps its fixed system texts, in the foreman's
    /// own dir.
    pub fn compactor_dir(&self) -> PathBuf {
        PathBuf::from(Server::foreman_dir()).join("compactor")
    }

    /// Writes the fixed system text a call uses to a file named by its hash
    /// and returns the path. The dir is 0700, the file 0600.
    fn system_file(&self, system: &str) -> Result<PathBuf, String> {
        use std::os::unix::fs::PermissionsExt;
        let dir = self.compactor_dir();
        crate::foreman::fsutil::ensure_dir_plain(&dir)?;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))
            .map_err(|e| crate::sys::go_path_err("chmod", &dir, &e))?;
        let sum = crate::sha256::digest(system.as_bytes());
        let hex: String = sum[..6].iter().map(|b| format!("{b:02x}")).collect();
        let path = dir.join(format!("sys-{hex}.txt"));
        if std::fs::read(&path).is_ok_and(|b| b == system.as_bytes()) {
            return Ok(path);
        }
        let tmp = dir.join(format!("sys-{hex}.txt.tmp"));
        write_0600(&tmp, system.as_bytes())?;
        std::fs::rename(&tmp, &path).map_err(|e| {
            format!(
                "rename {} {}: {}",
                tmp.display(),
                path.display(),
                crate::sys::go_errno_text(&e)
            )
        })?;
        Ok(path)
    }

    /// Reads the operator's profile table. The bytes live in this call only.
    pub fn operator_bundle(
        &self,
        ctx: &Ctx,
    ) -> Result<std::collections::HashMap<String, BundleEntry>, String> {
        let mut data = self.bundle_bytes(ctx)?;
        let table = serde_json::from_slice::<std::collections::HashMap<String, BundleEntry>>(&data);
        data.fill(0);
        table.map_err(|_| {
            "daisugi agent-profiles bundle wrote something that is not a profile table".into()
        })
    }

    /// The model call of the compactor: one sprig run that offers the model
    /// no tool. A profile other than default gets its key on descriptor 3,
    /// from the operator's bundle, for this call only.
    pub(super) fn compactor_call(
        &self,
        ctx: &Ctx,
        system: &str,
        prompt: &str,
    ) -> Result<(String, CallUsage), CallError> {
        let sys_path = self
            .system_file(system)
            .map_err(|e| format!("cannot write the compactor's system text: {e}"))?;
        let f = &self.cfg.foreman;
        let (mut wire, mut base, mut profile_model) =
            ("claude-code".to_string(), String::new(), String::new());
        let mut key: Option<String> = None;
        if f.compactor_profile != config::DEFAULT_COMPACTOR_PROFILE {
            let mut table = self.operator_bundle(ctx)?;
            let Some(e) = table.get_mut(&f.compactor_profile) else {
                return Err(format!(
                    "the compactor profile {} is not in the operator's agent profiles (a key variable it names may be unset)",
                    crate::sys::go_quote(&f.compactor_profile)
                )
                .into());
            };
            wire = e.wire.clone();
            key = e.key.take();
            base = e.base_url.clone().unwrap_or_default();
            profile_model = e.model.clone().unwrap_or_default();
        }
        let model = config::ForemanSettings {
            compactor_profile: f.compactor_profile.clone(),
            compactor_model: f.compactor_model.clone(),
            ..Default::default()
        }
        .compactor_model_for(&profile_model);
        let mut args: Vec<String> = [
            "--tools",
            "none",
            "--system-file",
            &sys_path.to_string_lossy(),
            "--json",
            "--max-turns",
            "1",
            "--wire",
            &wire,
            "--model",
            &model,
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        if base.is_empty() {
            base = default_base(&wire).to_string();
        }
        if !base.is_empty() {
            args.extend(["--base-url".to_string(), base]);
        }
        let key_bytes = key.map(String::into_bytes).filter(|k| !k.is_empty());
        if key_bytes.is_some() {
            args.extend(["--key-fd".to_string(), "3".to_string()]);
        }
        args.push("-".into());
        let dir = self.compactor_dir();
        crate::foreman::fsutil::ensure_dir_plain(&dir)?;
        let dir_text = dir.to_string_lossy().into_owned();
        let wrap = self.fence_cmd("sprig", &dir_text, &[dir_text.clone()], key_bytes.is_some())?;
        let (out, err) = super::foreman_turn::run_cmd(
            ctx,
            super::foreman_turn::CmdOpts {
                dir: dir.clone(),
                name: self.sprig_bin(),
                args,
                wrap,
                stdin: Some(prompt.as_bytes().to_vec()),
                // The key moves to the writer, which zeroes it.
                fd3: key_bytes,
                env: Some(self.sprig_env()),
                ..Default::default()
            },
        );
        if let Some(e) = ctx.err() {
            return Err(e.text().into());
        }
        if err.is_some() {
            let why = err.map(|e| e.text()).unwrap_or_default();
            return Err(format!(
                "the compactor's model call failed: {}",
                first_line(&out.stderr, &why)
            )
            .into());
        }
        if out.code != 0 {
            // The run started and failed. The bounded text of both streams
            // goes to the classifier only (an outage delays, anything else
            // is clipped after one retry: FM-R-64) and is never shown.
            return Err(CallError {
                msg: format!(
                    "the compactor's model call failed: {}",
                    first_line(&out.stderr, "")
                ),
                detail: super::foreman_turn::fail_detail(&out.stderr, &out.stdout),
                stderr: crate::foreman::failover::tail(&out.stderr, crate::foreman::failover::CLASSIFY_BYTES).to_string(),
                ran: true,
            });
        }
        let v: Value = serde_json::from_str(&out.stdout).map_err(|_| CallError {
            msg: "the compactor's model call gave an answer that is not JSON".to_string(),
            detail: String::new(),
            stderr: String::new(),
            ran: true,
        })?;
        let answer = v
            .get("answer")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        let mut usage = CallUsage::of_answer(v.get("usage"));
        let stop = ["stop", "stop_reason"]
            .iter()
            .filter_map(|k| v.get(*k).and_then(Value::as_str))
            .find(|s| !s.is_empty())
            .unwrap_or("");
        usage.stop = crate::foreman::compactor::intern_stop(stop);
        Ok((answer, usage))
    }

    /// Takes the owner's words in chat mode: logs them as a user message,
    /// queues the turn that answers it, and returns at once.
    pub fn talk_chat(self: &Arc<Server>, r: &Request, text: &str) -> Response {
        let fc = self.ensure_chat();
        if !fc.has_chat() {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &fc.problem);
        }
        // One lock across the room check, the append and the queue push:
        // two talks cannot swap places, and a line that is refused is not
        // logged.
        let mut t = self.turns_lock();
        let act = r.s("act");
        if !act.is_empty() {
            // A repeat of a known act returns the first answer and queues
            // no turn (FM-R-65). The check comes before the room check: a
            // repeat is not a new turn.
            let (first, same) = match fc.lock().as_ref() {
                Some(c) => {
                    let first = c.act_msg(&act);
                    (first, first.is_some() && c.act_matches(&act, text))
                }
                None => (None, false),
            };
            if first.is_some() && !same {
                // An act id is the sender's key for one sentence. Other
                // words under a known id would be dropped as a repeat, with
                // no word to the sender (FM-R-65).
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!("act {act} was used for other words. Send a new id for a new sentence."),
                );
            }
            if let Some(first) = first {
                let queued = t
                    .queue
                    .iter()
                    .position(|&m| m == first)
                    .map_or(0, |k| k + 1);
                return proto::ok_resp(
                    &r.id,
                    gojson::map(vec![
                        ("mode", json!("chat")),
                        ("message", json!(format!("log/{first}"))),
                        ("queued", json!(queued)),
                        ("label", json!(super::foreman::DEFAULT_FOREMAN_LABEL)),
                        ("already", json!(true)),
                    ]),
                );
            }
        }
        if let Err(e) = self.turn_room(&t) {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e);
        }
        let (ids, err) = match fc.lock().as_mut() {
            Some(c) => c.append_act(fchat::KIND_USER, text, None, &act),
            None => (Vec::new(), Some(ERR_NO_CHAT.to_string())),
        };
        fc.wake();
        let Some(&last) = ids.last() else {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!(
                    "the owner's words could not be logged: {}",
                    err.unwrap_or_default()
                ),
            );
        };
        // The turn state is locked here, so cancel its pause through it.
        if let Some(p) = t.pause.clone() {
            p.cancel();
        }
        self.send_messages_event("", true);
        // The turn answers the last part of a long message; the tail holds
        // all of it.
        if ids.len() > 1 {
            t.parts.insert(last, ids.clone());
        }
        let queued = self.queue_turn_locked(&mut t, &fc, last);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("mode", json!("chat")),
                ("message", json!(format!("log/{last}"))),
                ("queued", json!(queued)),
                ("label", json!(super::foreman::DEFAULT_FOREMAN_LABEL)),
            ]),
        )
    }

    /// Settles the turn records a stop or a crash left. A record with no end
    /// is closed as failed. When the newest owner line is past the newest
    /// turn that ended on its own, one note says it was not answered.
    /// Nothing is retried: a turn hours late could start workers.
    pub(super) fn close_dangling_turns(
        &self,
        fc: &ForemanChat,
        unknown: &std::collections::HashSet<i64>,
        started: &std::collections::HashSet<i64>,
        unreadable: bool,
    ) {
        let recs = turns::read_turns(&fc.dir).unwrap_or_default();
        let mut ended = std::collections::HashSet::new();
        let mut last_done = -1i64;
        for r in &recs {
            if r.status != turns::TURN_RUNNING {
                ended.insert(r.msg);
                last_done = last_done.max(r.msg);
                // A message the turn heard at a tool call is not unanswered.
                for id in &r.mid {
                    last_done = last_done.max(*id);
                }
            }
        }
        let now = self.fm_date();
        let mut closed = std::collections::HashSet::new();
        for r in &recs {
            if r.status == turns::TURN_RUNNING && !ended.contains(&r.msg) {
                ended.insert(r.msg);
                closed.insert(r.msg);
                let mut r = r.clone();
                r.ended = now.clone();
                r.status = turns::TURN_FAILED.into();
                let _ = turns::append_turn(&fc.dir, &r);
            }
        }
        let mut newest = -1i64;
        if let Some(c) = fc.lock().as_ref() {
            for i in (0..c.count()).rev() {
                if c.message(i).is_ok_and(|m| m.kind == fchat::KIND_USER) {
                    newest = i;
                    break;
                }
            }
        }
        if newest > last_done {
            let mut text = format!(
                "The foreman did not answer log/{newest}: coppice stopped first. Say it again."
            );
            if started.contains(&newest) {
                // A tool call of this turn started, and may have ended before
                // the crash: a repeat could run it twice (FM-R-66).
                text = format!(
                    "The foreman did not answer log/{newest}: coppice stopped first. A tool call ran in this turn. Check the workers before you say it again."
                );
            }
            if unreadable {
                // The call log could not be read: any call may have run.
                text = format!(
                    "The foreman did not finish log/{newest}: coppice stopped first. Whether a tool call ran is unknown, so the line is not run again. Check the workers first."
                );
            }
            if unknown.contains(&newest) {
                // A tool call of this turn has an unknown outcome: no
                // invitation to repeat it (FM-R-66).
                text = format!(
                    "The foreman did not finish log/{newest}: coppice stopped first, while a tool call ran. Its outcome is unknown, so it is not run again. Check the workers first."
                );
            }
            let _ = self.chat_append(fc, fchat::KIND_NOTE, &text);
            self.note(&text, "");
            // The note is the answer. A record says so, so a later start does
            // not note the same line again; a line whose own running record
            // was just closed is already counted.
            if !closed.contains(&newest) {
                let _ = turns::append_turn(
                    &fc.dir,
                    &TurnRecord {
                        msg: newest,
                        started: now.clone(),
                        ended: now,
                        status: turns::TURN_FAILED.into(),
                        usage: None,
                        mid: Vec::new(),
                    },
                );
            }
        }
    }

    /// floor.chat in chat mode: the messages come from the foreman's log,
    /// read through the server's own writer under the chat lock, so a line
    /// shows as soon as it is logged. Chat files from before the log began
    /// are merged in before its first message, with no transcripts. past
    /// messages are left out unless the request sends past: true. An owner
    /// line is read once turns.jsonl shows its turn started.
    pub fn floor_chat_log(self: &Arc<Server>, r: &Request) -> Response {
        let fc = self.ensure_chat();
        if !fc.has_chat() {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &fc.problem);
        }
        let want_past = r.bool("past").unwrap_or(false);
        let since = r.s("since");
        let limit = messages::limit_of(r);
        let recs = turns::read_turns(&fc.dir).unwrap_or_default();
        let last_started = recs.iter().map(|t| t.msg).max().unwrap_or(-1);
        let (delivered, queued): (std::collections::HashSet<i64>, std::collections::HashSet<i64>) = {
            let t = self.turns_lock();
            (t.delivered.clone(), t.queue.iter().copied().collect())
        };
        // A restart forgets the memory: the end record of a turn keeps the
        // messages it heard.
        let mut delivered = delivered;
        for t in &recs {
            delivered.extend(t.mid.iter().copied());
        }
        let (mut lo, mut since_log) = (0i64, false);
        if let Some(rest) = since.strip_prefix("log/") {
            if let Some(n) = crate::foreman::order::go_parse_int(rest).filter(|n| *n >= 0) {
                lo = n + 1;
                since_log = true;
            }
        }
        // A since that names an older chat file line needs the whole log.
        let need_all = !since.is_empty() && !since_log;
        let mut newest: Vec<ChatMessage> = Vec::new();
        let mut first_at = 0.0;
        let mut i;
        {
            let g = fc.lock();
            let Some(c) = g.as_ref() else {
                return proto::err_resp(&r.id, proto::ERR_INTERNAL, ERR_NO_CHAT);
            };
            let count = c.count();
            if count > 0 {
                if let Ok(m) = c.message(0) {
                    first_at = messages::rfc3339_seconds(&m.date);
                }
            }
            i = count - 1;
            while i >= lo && (need_all || newest.len() < limit) {
                if let Ok(m) = c.message(i) {
                    if !(m.kind == fchat::KIND_PAST && !want_past) {
                        if let Some(mut row) =
                            log_message(i, &m, last_started >= i || delivered.contains(&i))
                        {
                            if m.kind == fchat::KIND_USER {
                                if delivered.contains(&i) {
                                    row.delivery = "delivered mid-turn".into();
                                } else if queued.contains(&i) {
                                    row.delivery = "queued".into();
                                }
                            }
                            newest.push(row);
                        }
                    }
                }
                i -= 1;
            }
        }
        let reached_start = i < lo;
        newest.reverse();
        let reply = |msgs: &[ChatMessage], more: bool| {
            let list: Vec<Value> = msgs.iter().map(ChatMessage::to_value).collect();
            proto::ok_resp(
                &r.id,
                gojson::map(vec![
                    ("messages", Value::Array(list)),
                    ("source", json!("log")),
                    ("more", json!(more)),
                ]),
            )
        };
        if since_log || !reached_start {
            let (out, _) = messages::pick(&newest, "", limit, false);
            let more = !reached_start && newest.len() >= limit;
            return reply(&out, more);
        }
        let (old, cut, unread) = self.read_chat().unwrap_or((Vec::new(), false, Vec::new()));
        if !unread.is_empty() {
            self.note_once(&format!(
                "cannot read the chat file {}. The chat shows the rest.",
                unread.join(", ")
            ));
        }
        let mut all: Vec<ChatMessage> = old
            .into_iter()
            .filter(|m| first_at == 0.0 || m.at < first_at)
            .collect();
        all.extend(newest);
        let (out, more) = messages::pick(&all, &since, limit, cut);
        reply(&out, more)
    }
}

/// The base URL of a wire whose profile names none, the one agent steps
/// use. The claude-code wire has none.
pub fn default_base(wire: &str) -> &'static str {
    match wire {
        "anthropic" => "https://api.anthropic.com",
        "openai" => "https://api.openai.com/v1",
        _ => "",
    }
}

/// Writes data to path, mode 0600.
pub fn write_0600(path: &Path, data: &[u8]) -> Result<(), String> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)
        .and_then(|mut f| f.write_all(data))
        .map_err(|e| crate::sys::go_path_err("open", path, &e))
}

/// One log message as floor.chat gives it: user is owner, foreman is agent,
/// work is work, note is note, past is past, and a tool is an agent line
/// with its name only.
fn log_message(i: i64, m: &fchat::Message, read: bool) -> Option<ChatMessage> {
    let mut out = ChatMessage {
        id: format!("log/{i}"),
        at: messages::rfc3339_seconds(&m.date),
        ..Default::default()
    };
    match m.kind.as_str() {
        fchat::KIND_USER => {
            out.role = "owner".into();
            out.read = Some(read);
        }
        fchat::KIND_FOREMAN => out.role = "agent".into(),
        fchat::KIND_WORK => out.role = "work".into(),
        fchat::KIND_NOTE => out.role = "note".into(),
        fchat::KIND_PAST => out.role = "past".into(),
        fchat::KIND_TOOL => {
            // A tool use shows its name only. The result stays in the log.
            let first = m.text.split('\n').next().unwrap_or("");
            let name = first.split('(').next().unwrap_or("");
            out.role = "agent".into();
            out.tool = messages::one_line(name, messages::TOOL_RUNES);
            return (!out.tool.is_empty()).then_some(out);
        }
        _ => return None,
    }
    out.text = messages::clean_message(&m.text);
    (!out.text.is_empty()).then_some(out)
}
