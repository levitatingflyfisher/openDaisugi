//! The pane, tab and workspace verbs, and the life of a pty pane: its
//! start, its process tick, its exit, and its close.

use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde_json::{json, Value};

use super::role::{self, clean_label, is_foreman_label, role_or_operator, valid_harness};
use super::{Client, Server};
use crate::adapters;
use crate::config;
use crate::gojson;
use crate::layout::{self, LayoutError};
use crate::pane::{self, Flag, Grid, Pty, SpawnOpts, GRID_CLOSED};
use crate::proto::{self, Request, Response};
use crate::state::{self, Event};
use crate::sys;

/// How long a pty pane may go without output and still count as working.
const PROCESS_QUIET: f64 = 5.0;
/// How often the process tick looks at a pty pane's grid.
const PROCESS_TICK_EVERY: Duration = Duration::from_secs(1);
/// How long one teardown pass waits for every pane it found to stop.
const CLOSE_TEARDOWN_DEADLINE: Duration = Duration::from_secs(2);
/// How long a process exit waits for the reader to drain the last bytes.
const DRAIN_WAIT: Duration = Duration::from_millis(500);
/// How many recent directories the server remembers.
const MAX_RECENT_DIRS: usize = 10;
/// How long an ended record stays before the sweep removes it.
pub(super) const ENDED_TTL_SECS: i64 = 7 * 24 * 3600;

/// How long the pump waits, after an end event, for an adapter's stream
/// to close once told to stop. One deadline for the whole drain.
const STOP_DRAIN_WAIT: Duration = Duration::from_secs(1);

/// A record in the tree joined to what only exists while the server runs.
pub struct LivePane {
    pub info: Mutex<layout::Pane>,
    pub grid: Arc<Grid>,
    pub pty: Option<Arc<Pty>>,
    /// The headless harness behind a headless pane.
    pub adapter: Option<Arc<dyn adapters::Proc>>,
    started: SystemTime,
    /// The pty child's start time when it spawned. A process with the same
    /// pid and another start time is not this pane's.
    pty_start: u64,
    harness: String,
    tick_stop: Arc<Flag>,
    proc_state: Mutex<String>,
    /// Set when the thread that reports this pane's end (watch_exit for a
    /// pty pane, pump_adapter for a headless one) has returned. Both write
    /// into the data dir, so a teardown waits for it.
    gone: Arc<Flag>,
}

/// Sets a pane's gone flag when its end thread returns, by any path.
struct SetOnDrop(Arc<Flag>);

impl Drop for SetOnDrop {
    fn drop(&mut self) {
        self.0.set();
    }
}

impl LivePane {
    /// When the server started this pane.
    pub(super) fn started_at(&self) -> SystemTime {
        self.started
    }

    /// Whether pid is still this pane's pty child: the pane did not end,
    /// and the pid's start time is the one it spawned with.
    pub(super) fn pty_pid_ok(&self, pid: i32) -> bool {
        if self.gone.is_set() {
            return false;
        }
        self.pty_start == 0 || sys::proc_start_time(pid) == Some(self.pty_start)
    }

    pub fn write(&self, b: &[u8]) -> Result<(), String> {
        match (&self.pty, &self.adapter) {
            (Some(p), _) => p.write(b).map(|_| ()),
            (None, Some(a)) => a.write_stdin(b),
            _ => Err("this pane has nothing to write to".into()),
        }
    }
}

pub(super) fn unix_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// The closed set pane.send_keys accepts.
fn named_key(k: &str) -> Option<String> {
    let s = match k {
        "enter" | "return" => "\r",
        "tab" => "\t",
        "esc" | "escape" => "\x1b",
        "space" => " ",
        "backspace" => "\x7f",
        "up" => "\x1b[A",
        "down" => "\x1b[B",
        "right" => "\x1b[C",
        "left" => "\x1b[D",
        "ctrl+space" => "\x00",
        "shift+tab" => "\x1b[Z",
        "home" => "\x1b[H",
        "end" => "\x1b[F",
        "pgup" => "\x1b[5~",
        "pgdn" => "\x1b[6~",
        "delete" => "\x1b[3~",
        "insert" => "\x1b[2~",
        "f1" => "\x1bOP",
        "f2" => "\x1bOQ",
        "f3" => "\x1bOR",
        "f4" => "\x1bOS",
        "f5" => "\x1b[15~",
        "f6" => "\x1b[17~",
        "f7" => "\x1b[18~",
        "f8" => "\x1b[19~",
        "f9" => "\x1b[20~",
        "f10" => "\x1b[21~",
        "f11" => "\x1b[23~",
        "f12" => "\x1b[24~",
        _ => {
            let c = k.strip_prefix("ctrl+")?;
            let mut it = c.chars();
            let ch = it.next()?;
            if it.next().is_some() || !ch.is_ascii_lowercase() {
                return None;
            }
            return Some(((ch as u8 - b'a' + 1) as char).to_string());
        }
    };
    Some(s.to_string())
}

fn key_names() -> String {
    let mut out: Vec<String> = [
        "enter",
        "return",
        "tab",
        "esc",
        "escape",
        "space",
        "backspace",
        "up",
        "down",
        "right",
        "left",
        "ctrl+space",
        "shift+tab",
        "home",
        "end",
        "pgup",
        "pgdn",
        "delete",
        "insert",
        "f1",
        "f2",
        "f3",
        "f4",
        "f5",
        "f6",
        "f7",
        "f8",
        "f9",
        "f10",
        "f11",
        "f12",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect();
    for c in 'a'..='z' {
        out.push(format!("ctrl+{c}"));
    }
    out.sort();
    out.join(", ")
}

fn no_pane(r: &Request, id: &str) -> Response {
    proto::err_resp(
        &r.id,
        proto::ERR_NO_SUCH_PANE,
        &format!("no pane {}. Run: coppice pane list", sys::go_quote(id)),
    )
}

fn pane_closed(r: &Request, id: &str) -> Response {
    proto::err_resp(
        &r.id,
        proto::ERR_PANE_CLOSED,
        &format!("pane {id} has closed. Create a new one with: coppice pane create"),
    )
}

macro_rules! h {
    ($s:expr, $name:expr, $f:ident) => {
        let _ = $s.handle($name, Arc::new(|s, c, r| s.$f(c, r)));
    };
}

pub fn register(s: &Arc<Server>) {
    h!(s, "workspace.create", handle_workspace_create);
    h!(s, "workspace.list", handle_workspace_list);
    h!(s, "tab.create", handle_tab_create);
    h!(s, "tab.list", handle_tab_list);
    h!(s, "pane.create", handle_pane_create);
    h!(s, "pane.split", handle_pane_create);
    h!(s, "pane.list", handle_pane_list);
    h!(s, "pane.send_text", handle_send_text);
    h!(s, "pane.send_keys", handle_send_keys);
    h!(s, "pane.run", handle_run);
    h!(s, "pane.read", handle_read);
    h!(s, "pane.resize", handle_resize);
    h!(s, "pane.close", handle_close);
    h!(s, "pane.wait_output", handle_wait_output);
    h!(s, "pane.fork", handle_pane_fork);
    h!(s, "pane.rename", handle_pane_rename);
    h!(s, "session.list", handle_pane_list);
    h!(s, "session.stop", handle_close);
    let sw = s.clone();
    thread::spawn(move || sw.run_ended_sweep());
}

impl Server {
    pub fn live(&self, id: &str) -> Option<Arc<LivePane>> {
        self.live
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .get(id)
            .cloned()
    }

    fn put_live(&self, id: &str, lp: Arc<LivePane>) {
        self.live
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .insert(id.to_string(), lp);
    }

    pub(super) fn remove_live(&self, id: &str) {
        let lp = self
            .live
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .remove(id);
        if let Some(lp) = lp {
            lp.tick_stop.set();
        }
    }

    /// Stops every live pane the snapshot finds, all at once, under one
    /// deadline.
    pub fn teardown_live_panes(&self) {
        let lps: Vec<Arc<LivePane>> = self
            .live
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect();
        if lps.is_empty() {
            return;
        }
        let done = Arc::new(Flag::default());
        let left = Arc::new(Mutex::new(lps.len()));
        for lp in lps {
            let done = done.clone();
            let left = left.clone();
            thread::spawn(move || {
                if let Some(p) = &lp.pty {
                    p.close();
                    p.done.wait(None);
                }
                if let Some(a) = &lp.adapter {
                    a.stop();
                }
                // The pane's own end thread may be in the middle of a
                // write into the data dir. The teardown is not done before
                // it returns.
                lp.gone.wait(None);
                let mut n = left.lock().unwrap_or_else(|e| e.into_inner());
                *n -= 1;
                if *n == 0 {
                    done.set();
                }
            });
        }
        done.wait(Some(CLOSE_TEARDOWN_DEADLINE));
    }

    fn refresh_live_info(&self, id: &str) {
        let g = self.live.read().unwrap_or_else(|e| e.into_inner());
        if let (Some(rec), Some(lp)) = (self.tree.pane(id), g.get(id)) {
            *lp.info.lock().unwrap_or_else(|e| e.into_inner()) = rec;
        }
    }

    fn pane_info(&self, id: &str) -> Option<layout::Pane> {
        self.live(id)
            .map(|lp| lp.info.lock().unwrap_or_else(|e| e.into_inner()).clone())
    }

    /// Changes a pane's record, refreshes the live copy, and saves.
    pub fn update_pane(
        &self,
        id: &str,
        f: impl FnOnce(&mut layout::Pane),
    ) -> Result<(), LayoutError> {
        self.tree.update_pane(id, f)?;
        self.refresh_live_info(id);
        self.save_layout();
        Ok(())
    }

    /// Marks a pane ended on its own and keeps its record.
    fn close_pane(&self, id: &str, exit: Option<i64>) -> Result<(), LayoutError> {
        self.tree.close_pane(id, exit, unix_secs())?;
        self.forget_children(id);
        self.forget_asks(id);
        self.tree.drop_foreman(id);
        self.refresh_live_info(id);
        self.save_layout();
        Ok(())
    }

    /// Erases a pane's record for good, and everything the server keeps
    /// about it.
    pub(super) fn drop_record(&self, id: &str) {
        if let Some(lp) = self.live(id) {
            lp.grid.close();
        }
        self.remove_live(id);
        let _ = self.tree.remove_pane(id);
        self.tree.drop_foreman(id);
        self.forget_children(id);
        self.forget_asks(id);
        self.states.forget(id);
        self.save_layout();
    }

    /// Stops the pane's process and removes its record at once.
    pub fn close_record(&self, rec: &layout::Pane) {
        if let Some(lp) = self.live(&rec.id) {
            if let Some(p) = &lp.pty {
                p.close();
            }
            if let Some(a) = &lp.adapter {
                a.stop();
            }
        }
        self.drop_record(&rec.id);
    }

    pub fn save_layout(&self) {
        self.write_data(|| {
            let _ = self.tree.save(&self.cfg.data_dir.join("layout.json"));
        });
    }

    /// The state a reader sees for one pane now.
    pub fn effective_state(&self, id: &str) -> Option<Event> {
        self.states
            .current(id)
            .map(|ev| state::effective_state(&ev, Server::now_seconds()))
    }

    /// How many ended records pane.resume would resume rather than start
    /// fresh.
    pub fn resumable_count(&self) -> usize {
        let cfg = config::load();
        let mut n = 0;
        for p in self.tree.panes() {
            if !p.closed || p.harness_session_id.is_empty() {
                continue;
            }
            match p.kind.as_str() {
                layout::KIND_HEADLESS => n += 1,
                layout::KIND_PTY => {
                    if let Ok(Some(c)) = &cfg {
                        if c.harness
                            .get(&p.harness)
                            .is_some_and(|h| !h.resume_args().is_empty())
                        {
                            n += 1;
                        }
                    }
                }
                _ => {}
            }
        }
        n
    }

    /// Removes ended records older than seven days, every hour.
    fn run_ended_sweep(self: Arc<Server>) {
        loop {
            if self.sweep_stop.wait(Some(Duration::from_secs(3600))) || self.is_closed() {
                return;
            }
            self.sweep_ended();
        }
    }

    /// Resolves a pane id to something writable.
    pub(super) fn live_pane(&self, r: &Request) -> Result<Arc<LivePane>, Response> {
        let id = r.s("pane");
        if id.is_empty() {
            return Err(proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "this command needs pane. Run: coppice pane list",
            ));
        }
        let rec = self.tree.pane(&id).ok_or_else(|| no_pane(r, &id))?;
        if rec.closed {
            return Err(pane_closed(r, &id));
        }
        self.live(&id).ok_or_else(|| pane_closed(r, &id))
    }

    fn handle_workspace_create(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let ws = self.tree.create_workspace(&r.s("label"), &r.s("cwd"));
        let tab = match self.tree.create_tab(&ws.id, "main") {
            Ok(t) => t,
            Err(e) => return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e.to_string()),
        };
        self.save_layout();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("workspace", json!(ws.id)), ("tab", json!(tab.id))]),
        )
    }

    fn handle_workspace_list(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let out: Vec<Value> = self
            .tree
            .workspaces()
            .into_iter()
            .map(|w| {
                gojson::map(vec![
                    ("id", json!(w.id)),
                    ("label", json!(w.label)),
                    ("cwd", json!(w.cwd)),
                    ("tabs", json!(w.tab_ids.map(|t| t.len()).unwrap_or(0))),
                ])
            })
            .collect();
        proto::ok_resp(&r.id, gojson::map(vec![("workspaces", Value::Array(out))]))
    }

    fn handle_tab_create(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let ws_id = match r.str("workspace") {
            Some(w) => w,
            None => self.tree.current().0,
        };
        if ws_id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_WORKSPACE,
                "there is no workspace yet. Run: coppice workspace create --cwd .",
            );
        }
        match self.tree.create_tab(&ws_id, &r.s("label")) {
            Ok(tab) => {
                self.save_layout();
                proto::ok_resp(
                    &r.id,
                    gojson::map(vec![("tab", json!(tab.id)), ("workspace", json!(ws_id))]),
                )
            }
            Err(e) => proto::err_resp(&r.id, proto::ERR_NO_SUCH_WORKSPACE, &e.to_string()),
        }
    }

    fn handle_tab_list(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let ws_id = match r.str("workspace") {
            Some(w) => w,
            None => self.tree.current().0,
        };
        match self.tree.tabs(&ws_id) {
            Ok(tabs) => {
                let out: Vec<Value> = tabs
                    .into_iter()
                    .map(|t| {
                        gojson::map(vec![
                            ("id", json!(t.id)),
                            ("label", json!(t.label)),
                            ("panes", json!(t.pane_ids.map(|p| p.len()).unwrap_or(0))),
                        ])
                    })
                    .collect();
                proto::ok_resp(&r.id, gojson::map(vec![("tabs", Value::Array(out))]))
            }
            Err(e) => proto::err_resp(&r.id, proto::ERR_NO_SUCH_WORKSPACE, &e.to_string()),
        }
    }

    /// The foreman's own working directory, never a project.
    pub(super) fn foreman_dir() -> String {
        if let Some(st) = std::env::var_os("XDG_STATE_HOME").filter(|v| !v.is_empty()) {
            return format!("{}/coppice/foreman", st.to_string_lossy());
        }
        match sys::home_dir() {
            Some(h) => format!("{}/.local/state/coppice/foreman", h.to_string_lossy()),
            None => ".local/state/coppice/foreman".into(),
        }
    }

    fn is_foreman_dir(dir: &str) -> bool {
        if dir.is_empty() {
            return false;
        }
        let real = |p: &str| {
            std::fs::canonicalize(p)
                .map(|x| x.to_string_lossy().into_owned())
                .unwrap_or_else(|_| p.to_string())
        };
        sys::clean(&real(dir)) == sys::clean(&real(&Server::foreman_dir()))
    }

    pub(super) fn recent_dirs(&self) -> Vec<String> {
        std::fs::read(self.cfg.data_dir.join("recent-dirs.json"))
            .ok()
            .and_then(|b| serde_json::from_slice::<Vec<String>>(&b).ok())
            .unwrap_or_default()
    }

    /// Puts dir at the front of the recent directory list.
    fn record_recent_dir(&self, dir: &str) {
        if dir.is_empty() || Server::is_foreman_dir(dir) {
            return;
        }
        let dir = std::fs::canonicalize(dir)
            .map(|x| x.to_string_lossy().into_owned())
            .unwrap_or_else(|_| dir.to_string());
        let _g = self.label_mu.lock().unwrap_or_else(|e| e.into_inner());
        let mut out = vec![dir.clone()];
        out.extend(self.recent_dirs().into_iter().filter(|d| *d != dir));
        out.truncate(MAX_RECENT_DIRS);
        let body = gojson::marshal(&json!(out));
        let path = self.cfg.data_dir.join("recent-dirs.json");
        let tmp = self
            .cfg
            .data_dir
            .join(format!("recent-dirs.json.{}", std::process::id()));
        self.write_data(|| {
            // As Go's os.CreateTemp: the file is 0600 in a 0700 directory.
            let made = {
                use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
                std::fs::DirBuilder::new()
                    .recursive(true)
                    .mode(0o700)
                    .create(&self.cfg.data_dir)
                    .and_then(|_| {
                        std::fs::OpenOptions::new()
                            .write(true)
                            .create_new(true)
                            .mode(0o600)
                            .open(&tmp)
                    })
                    .and_then(|mut f| std::io::Write::write_all(&mut f, body.as_bytes()))
            };
            if made.is_err() || std::fs::rename(&tmp, &path).is_err() {
                let _ = std::fs::remove_file(&tmp);
            }
        });
    }

    /// Where a pane with no cwd of its own starts: near's directory, the
    /// newest recent one, the first pinned project, then the start dir.
    fn default_cwd(&self, near_cwd: &str) -> String {
        let is_dir = |p: &str| !p.is_empty() && std::fs::metadata(p).is_ok_and(|m| m.is_dir());
        if is_dir(near_cwd) && !Server::is_foreman_dir(near_cwd) {
            return near_cwd.to_string();
        }
        for d in self.recent_dirs() {
            if is_dir(&d) {
                return d;
            }
        }
        if let Ok(Some(c)) = config::load() {
            for p in c.projects {
                if is_dir(&p) {
                    return p;
                }
            }
        }
        let sd = self.cfg.start_dir.to_string_lossy().into_owned();
        if is_dir(&sd) {
            return sd;
        }
        String::new()
    }

    fn live_label_taken(&self, label: &str) -> bool {
        self.tree
            .panes()
            .iter()
            .any(|p| !p.closed && p.label == label)
    }

    /// The workspace and tab a new pane goes in.
    fn place_for(&self, r: &Request, cwd: &str) -> Result<(String, String), String> {
        let ws_name = r.str("workspace");
        let tab_name = r.str("tab");
        if let (Some(w), Some(t)) = (&ws_name, &tab_name) {
            return Ok((w.clone(), t.clone()));
        }
        let _g = self.place_mu.lock().unwrap_or_else(|e| e.into_inner());
        let (mut ws_id, mut tab_id) = self.tree.current();
        if let Some(w) = &ws_name {
            ws_id = w.clone();
        }
        if let Some(t) = &tab_name {
            tab_id = t.clone();
        }
        if ws_id.is_empty() || (tab_id.is_empty() && ws_name.is_none()) {
            let ws = self.tree.create_workspace("", cwd);
            let tab = self
                .tree
                .create_tab(&ws.id, "main")
                .map_err(|e| e.to_string())?;
            return Ok((ws.id, tab.id));
        }
        Ok((ws_id, tab_id))
    }

    fn handle_pane_create(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        self.pane_create(Some(c), r)
    }

    /// The name a pane gets when its request named none:
    /// harness-basename, then -2, -3 and on, the first one no live pane
    /// has. The caller holds label_mu.
    fn default_label(&self, harness: &str, cwd: &str) -> String {
        let h = if harness.is_empty() { "shell" } else { harness };
        // Go's filepath.Base, then "root" for "", "." and "/".
        let trimmed = cwd.trim_end_matches('/');
        let base = match trimmed.rsplit('/').next() {
            Some(b) if !trimmed.is_empty() && b != "." => b.to_string(),
            _ => "root".to_string(),
        };
        let mut base = crate::textwidth::printable(&format!("{h}-{base}"), role::LABEL_MAX - 4)
            .trim()
            .to_string();
        if is_foreman_label(&base) {
            base.push_str("-agent");
        }
        if !self.live_label_taken(&base) {
            return base;
        }
        let mut n = 2;
        loop {
            let candidate = format!("{base}-{n}");
            if !self.live_label_taken(&candidate) {
                return candidate;
            }
            n += 1;
        }
    }

    /// pane.create for a client, or for the server itself with none.
    pub(super) fn pane_create(
        self: &Arc<Server>,
        c: Option<&Arc<Client>>,
        r: &Request,
    ) -> Response {
        if self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a new pane.",
            );
        }
        let mut cwd = r.s("cwd");
        let task_id = r.s("task");
        if !task_id.is_empty() {
            let Some(tk) = self.tree.task(&task_id) else {
                return super::tasks::no_task(&r.id, &task_id);
            };
            if !tk.worktree.is_empty() && !cwd.is_empty() && cwd != tk.worktree {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!(
                        "a pane in task {} runs in its worktree {}. Drop cwd.",
                        tk.id, tk.worktree
                    ),
                );
            } else if !tk.worktree.is_empty() {
                cwd = tk.worktree.clone();
            } else if cwd.is_empty() {
                cwd = tk.cwd.clone();
            }
        }
        let mut kind = layout::KIND_PTY;
        if let Some(k) = r.str("kind") {
            kind = match k.as_str() {
                "pty" => layout::KIND_PTY,
                "headless" => layout::KIND_HEADLESS,
                _ => {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_BAD_REQUEST,
                        &format!("kind {} is not pty or headless", sys::go_quote(&k)),
                    )
                }
            };
        }
        let given = r.str_slice("cmd_argv");
        // Go keeps an absent, null or unreadable cmd_argv as a nil slice,
        // which a headless pane saves and lists as null.
        let argv_nil = given.is_none() || r.raw("cmd_argv") == Some("null");
        let mut argv = given.unwrap_or_default();
        let mut harness = r.s("harness");
        if !harness.is_empty() && !valid_harness(&harness) {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "harness must be a name of at most {} letters, digits, '.', '_' or '-', as in coppice.toml.",
                    role::HARNESS_MAX
                ),
            );
        }
        let mut near_cwd = String::new();
        if let Some(near) = r.str("near").filter(|n| !n.is_empty()) {
            match self.tree.pane(&near) {
                Some(p) => near_cwd = p.cwd,
                None => {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_NO_SUCH_PANE,
                        &format!(
                            "no pane {} to start near. Run: coppice pane list",
                            sys::go_quote(&near)
                        ),
                    )
                }
            }
        }
        let auto_default = cwd.is_empty() && argv.is_empty() && kind == layout::KIND_PTY;
        if auto_default {
            let resolved = self.default_cwd(&near_cwd);
            if !resolved.is_empty() {
                cwd = resolved;
            }
            if harness.is_empty() {
                if let Ok(Some(cfg)) = config::load() {
                    if !cfg.default.is_empty() {
                        harness = cfg.default;
                    }
                }
            }
        }
        if cwd.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.create needs cwd. Pass the directory the pane should start in.",
            );
        }
        let mut extra_args = Vec::new();
        if kind == layout::KIND_HEADLESS && harness.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "a headless pane needs harness. Known adapters: {}",
                    adapters::names().join(", ")
                ),
            );
        }
        if kind == layout::KIND_PTY && argv.is_empty() && !harness.is_empty() {
            let extra = r.str_slice("args").unwrap_or_default();
            let resolved = match config::load() {
                Ok(Some(cfg)) => cfg
                    .harness
                    .get(&harness)
                    .filter(|h| !h.command.is_empty())
                    .map(|h| {
                        let mut a = vec![h.command.clone()];
                        a.extend(h.args().iter().cloned());
                        a.extend(extra.iter().cloned());
                        a
                    }),
                _ => None,
            };
            match resolved {
                Some(a) => {
                    argv = a;
                    extra_args = extra;
                }
                None => {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_BAD_REQUEST,
                        &format!(
                            "no harness named {} in {}. Run coppice once to write it, or add [harness.{}] with command = {}.",
                            sys::go_quote(&harness),
                            config::path().display(),
                            harness,
                            sys::go_quote(&harness)
                        ),
                    )
                }
            }
        }
        if kind == layout::KIND_PTY && argv.is_empty() {
            if auto_default && harness.is_empty() {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!(
                        "no default harness in {}. Run coppice open HARNESS once to set one.",
                        config::path().display()
                    ),
                );
            }
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "a pty pane needs cmd_argv. Put the command after -- on the command line.",
            );
        }
        let (ws_id, tab_id) = match self.place_for(r, &cwd) {
            Ok(x) => x,
            Err(e) => return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e),
        };
        let cols = r.int("cols").unwrap_or(120);
        let rows = r.int("rows").unwrap_or(40);
        let env = r.str_map("env").unwrap_or_default();
        let mut label = clean_label(&r.s("label"));
        let needs_label = label.is_empty() && !harness.is_empty() && !cwd.is_empty();
        let ro = role_or_operator(c);
        if ro.no_allow && !label.is_empty() && self.live_label_taken(&label) {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, role::LABEL_TAKEN_REFUSAL);
        }
        let label_guard =
            needs_label.then(|| self.label_mu.lock().unwrap_or_else(|e| e.into_inner()));
        if needs_label {
            label = self.default_label(&harness, &cwd);
        }
        if ro.no_allow && is_foreman_label(&label) {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, role::FOREMAN_LABEL_REFUSAL);
        }
        let created = self.tree.create_pane(
            &ws_id,
            &tab_id,
            layout::Pane {
                label,
                cwd,
                argv: if argv_nil && argv.is_empty() {
                    None
                } else {
                    Some(argv)
                },
                env,
                kind: kind.into(),
                harness,
                cols,
                rows,
                task_id,
                extra_args,
                ..Default::default()
            },
        );
        drop(label_guard);
        let rec = match created {
            Ok(p) => p,
            Err(e @ LayoutError::NoWorkspace(_)) => {
                return proto::err_resp(&r.id, proto::ERR_NO_SUCH_WORKSPACE, &e.to_string())
            }
            Err(e) => return proto::err_resp(&r.id, proto::ERR_NO_SUCH_TAB, &e.to_string()),
        };
        if let Err(e) = self.start_pane(&rec) {
            // Nothing ran, so nothing stays. The pane number is spent.
            let _ = self.tree.remove_pane(&rec.id);
            self.remove_live(&rec.id);
            self.states.forget(&rec.id);
            self.save_layout();
            return proto::err_resp(&r.id, e.code(), &e.msg);
        }
        self.save_layout();
        self.record_recent_dir(&rec.cwd);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("pane", json!(rec.id)),
                ("workspace", json!(ws_id)),
                ("tab", json!(tab_id)),
            ]),
        )
    }

    /// Builds the grid and, for a pty pane, the process; for a headless
    /// pane, the adapter. A headless record with fork_pending starts as a
    /// fork of the session in harness_session_id: the adapter's fork argv
    /// is added for this start only, never saved.
    pub(super) fn start_pane(self: &Arc<Server>, rec: &layout::Pane) -> Result<(), StartError> {
        if !rec.cwd.is_empty() && !std::fs::metadata(&rec.cwd).is_ok_and(|m| m.is_dir()) {
            return Err(StartError::spawn(format!(
                "the directory {} does not exist. Pick another directory.",
                rec.cwd
            )));
        }
        let g = Arc::new(Grid::new(rec.cols, rec.rows).map_err(StartError::spawn)?);
        let harness = if rec.harness.is_empty() {
            "shell".to_string()
        } else {
            rec.harness.clone()
        };
        let mut lp = LivePane {
            info: Mutex::new(rec.clone()),
            grid: g.clone(),
            pty: None,
            adapter: None,
            started: SystemTime::now(),
            pty_start: 0,
            harness,
            tick_stop: Arc::new(Flag::default()),
            proc_state: Mutex::new(String::new()),
            gone: Arc::new(Flag::default()),
        };
        if rec.kind == layout::KIND_HEADLESS {
            let Some(a) = adapters::get(&rec.harness) else {
                g.close();
                return Err(StartError::bad(format!(
                    "no adapter registered for this harness {}. Known adapters: {}",
                    sys::go_quote(&rec.harness),
                    adapters::names().join(", ")
                )));
            };
            let mut argv = rec.argv.clone().unwrap_or_default();
            if rec.fork_pending {
                let Some(extra) = a.fork_argv(&rec.harness_session_id) else {
                    g.close();
                    return Err(StartError::spawn(format!(
                        "{} cannot fork a session, and this pane is a fork of {} that never reported its own session id",
                        rec.harness, rec.parent_pane
                    )));
                };
                match extra {
                    Ok(x) => argv.extend(x),
                    Err(e) => {
                        g.close();
                        return Err(StartError::spawn(e));
                    }
                }
            }
            let proc = match a.start(adapters::StartOpts {
                cwd: rec.cwd.clone(),
                env: rec.env.clone(),
                argv,
                resume: rec.harness_session_id.clone(),
                sock: self.cfg.socket_path.to_string_lossy().into_owned(),
                pane_id: rec.id.clone(),
                data_dir: self.cfg.data_dir.to_string_lossy().into_owned(),
                bin: String::new(),
            }) {
                Ok(p) => p,
                Err(e) => {
                    g.close();
                    return Err(StartError::spawn(e));
                }
            };
            lp.adapter = Some(proc.clone());
            let gone = SetOnDrop(lp.gone.clone());
            let lp = Arc::new(lp);
            self.put_live(&rec.id, lp);
            let s = self.clone();
            let id = rec.id.clone();
            let h = rec.harness.clone();
            thread::spawn(move || {
                let _gone = gone;
                s.pump_adapter(&id, &h, proc, g)
            });
            return Ok(());
        }
        if rec.kind != layout::KIND_PTY {
            g.close();
            return Err(StartError::bad(format!(
                "no such pane kind {}",
                sys::go_quote(&rec.kind)
            )));
        }
        let argv = rec.argv.clone().unwrap_or_default();
        if let Some(a0) = argv.first() {
            if !a0.contains('/') && pane::look_path(a0).is_none() {
                g.close();
                return Err(StartError::spawn(format!(
                    "{a0} is not on PATH. Install it or give a full path."
                )));
            }
        }
        let p = match Pty::start(
            SpawnOpts {
                cwd: rec.cwd.clone(),
                argv,
                env: rec.env.clone(),
                cols: rec.cols,
                rows: rec.rows,
                sock: self.cfg.socket_path.to_string_lossy().into_owned(),
                pane_id: rec.id.clone(),
                data_dir: self.cfg.data_dir.to_string_lossy().into_owned(),
            },
            g.clone(),
        ) {
            Ok(p) => Arc::new(p),
            Err(e) => {
                g.close();
                return Err(StartError::spawn(e));
            }
        };
        lp.pty_start = sys::proc_start_time(p.pid()).unwrap_or(0);
        lp.pty = Some(p.clone());
        let lp = Arc::new(lp);
        self.put_live(&rec.id, lp.clone());
        {
            let s = self.clone();
            let id = rec.id.clone();
            let gone = SetOnDrop(lp.gone.clone());
            thread::spawn(move || {
                let _gone = gone;
                s.watch_exit(&id, p)
            });
        }
        // The first process fact lands before pane.create answers.
        self.process_tick(&rec.id, &lp);
        {
            let s = self.clone();
            let id = rec.id.clone();
            thread::spawn(move || s.run_process_ticks(&id, lp));
        }
        Ok(())
    }

    /// Turns one adapter's events into transcript rows and states with
    /// source headless. The record closes before done is posted. An end
    /// event is terminal: the pump stops the adapter and drains what it
    /// still sends, under one deadline, before it closes the record.
    fn pump_adapter(
        self: Arc<Server>,
        id: &str,
        harness: &str,
        p: Arc<dyn adapters::Proc>,
        g: Arc<Grid>,
    ) {
        let Some(rx) = p.take_events() else {
            return;
        };
        let mut grid_closed = false;
        let mut write = |ev: &adapters::Ev| {
            if grid_closed {
                return;
            }
            if write_transcript(&g, ev).is_err() {
                grid_closed = true;
                eprintln!(
                    "coppice: pane {id}: its grid is closed, no longer rendering its headless transcript (state keeps flowing)"
                );
            }
        };
        let mut saw_end = false;
        let mut end_detail = String::new();
        for ev in rx.iter() {
            if ev.kind == adapters::Kind::Child {
                if !ev.child.is_empty() {
                    self.report_child(id, &ev.child, &ev.state, &ev.text);
                }
                continue;
            }
            write(&ev);
            if ev.kind == adapters::Kind::End {
                saw_end = true;
                end_detail = super::agents::cut(&ev.detail, state::DETAIL_MAX);
                break;
            }
            let st = state_of(&ev);
            let mut e = Event {
                v: 1,
                ts: Server::now_seconds(),
                session_id: id.into(),
                harness: harness.into(),
                pane: Some(id.into()),
                state: st.into(),
                source: state::SRC_HEADLESS.into(),
                detail: super::agents::cut(&ev.detail, state::DETAIL_MAX),
                ..Default::default()
            };
            if st == state::STATE_BLOCKED {
                e.ask = ev.ask.clone();
                if p.answerer() {
                    if let Some(a) = e.ask.as_mut() {
                        a.holder = "harness".into();
                    }
                }
            }
            if let Some(sid) = p.session_id() {
                e.harness_session_id = Some(sid.clone());
                if !self.is_closed() {
                    if let Some(rec) = self.tree.pane(id) {
                        if rec.harness_session_id != sid {
                            let _ = self.update_pane(id, |x| {
                                x.harness_session_id = sid.clone();
                                x.fork_pending = false;
                            });
                        }
                    }
                }
            }
            self.apply_state(id, e);
        }
        let mut detail = "adapter stream ended".to_string();
        if saw_end {
            p.stop();
            let deadline = Instant::now() + STOP_DRAIN_WAIT;
            while let Some(left) = deadline.checked_duration_since(Instant::now()) {
                match rx.recv_timeout(left) {
                    Ok(ev) => write(&ev),
                    Err(_) => break,
                }
            }
            detail = end_detail;
        }
        if self.is_closed() {
            return;
        }
        let _ = self.close_pane(id, None);
        self.apply_state(
            id,
            Event {
                v: 1,
                ts: Server::now_seconds(),
                session_id: id.into(),
                harness: harness.into(),
                pane: Some(id.into()),
                state: state::STATE_DONE.into(),
                source: state::SRC_HEADLESS.into(),
                detail,
                ..Default::default()
            },
        );
        if self.tree.pane(id).is_none() {
            self.states.forget(id);
        }
    }

    /// Applies one process fact: working when the grid took bytes within
    /// PROCESS_QUIET, else idle, and only when the fact changed.
    fn process_tick(self: &Arc<Server>, id: &str, lp: &Arc<LivePane>) {
        let now = Server::now_seconds();
        let (st, detail) = match lp.grid.last_write() {
            None => (state::STATE_IDLE, "no output yet"),
            Some(ns) if now - (ns as f64 / 1e9) < PROCESS_QUIET => {
                (state::STATE_WORKING, "output in the last 5 s")
            }
            Some(_) => (state::STATE_IDLE, "no output for 5 s"),
        };
        {
            let mut ps = lp.proc_state.lock().unwrap_or_else(|e| e.into_inner());
            if *ps == st {
                return;
            }
            *ps = st.to_string();
        }
        self.apply_state(
            id,
            Event {
                v: 1,
                ts: Server::now_seconds(),
                session_id: id.into(),
                harness: lp.harness.clone(),
                pane: Some(id.into()),
                state: st.into(),
                source: state::SRC_PROCESS.into(),
                detail: detail.into(),
                ..Default::default()
            },
        );
    }

    fn run_process_ticks(self: Arc<Server>, id: &str, lp: Arc<LivePane>) {
        loop {
            if lp.tick_stop.wait(Some(PROCESS_TICK_EVERY)) {
                return;
            }
            if lp.pty.as_ref().is_some_and(|p| p.done.is_set()) || self.is_closed() {
                return;
            }
            self.process_tick(id, &lp);
        }
    }

    /// Turns a process exit into the done only the server can produce.
    fn watch_exit(self: Arc<Server>, id: &str, p: Arc<Pty>) {
        p.done.wait(None);
        p.drained.wait(Some(DRAIN_WAIT));
        if self.is_closed() {
            return;
        }
        let code = p.exit_code().unwrap_or(0);
        let _ = self.close_pane(id, Some(code));
        let harness = match self.tree.pane(id) {
            Some(r) if !r.harness.is_empty() => r.harness,
            _ => "shell".into(),
        };
        self.apply_state(
            id,
            Event {
                v: 1,
                ts: Server::now_seconds(),
                session_id: id.into(),
                harness,
                pane: Some(id.into()),
                state: state::STATE_DONE.into(),
                source: state::SRC_PROCESS.into(),
                detail: format!("exit={code}"),
                ..Default::default()
            },
        );
        if self.tree.pane(id).is_none() {
            self.states.forget(id);
        }
    }

    fn handle_pane_list(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let (keep, path) = match self.scope_of(r) {
            Ok(x) => x,
            Err(resp) => return resp,
        };
        let want_ended = r.bool("ended").unwrap_or(false);
        self.prune_facts();
        let mut matched: Vec<layout::Pane> = self
            .tree
            .panes()
            .into_iter()
            .filter(|p| keep.as_ref().is_none_or(|k| k.contains(&p.task_id)))
            .filter(|p| p.closed == want_ended)
            .collect();
        if want_ended {
            sort_ended_newest_first(&mut matched);
        }
        let out: Vec<Value> = matched.iter().map(|p| self.pane_list_row(p)).collect();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("panes", Value::Array(out)), ("path", json!(path))]),
        )
    }

    fn pane_list_row(&self, p: &layout::Pane) -> Value {
        let mut row: Vec<(&'static str, Value)> = vec![
            ("id", json!(p.id)),
            ("label", json!(p.label)),
            ("cwd", json!(p.cwd)),
            ("cmd", p.argv_value()),
            ("kind", json!(p.kind)),
            ("harness", json!(p.harness)),
            ("workspace", json!(p.workspace)),
            ("tab", json!(p.tab)),
            ("closed", json!(p.closed)),
            ("cols", json!(p.cols)),
            ("rows", json!(p.rows)),
        ];
        let mut st = (json!(state::STATE_UNKNOWN), Value::Null, json!(""));
        if p.closed {
            row.push(("ended_at", json!(p.ended_at)));
            row.push(("exit_code", json!(p.exit_code)));
        }
        if !p.parent_pane.is_empty() {
            row.push(("parent_pane", json!(p.parent_pane)));
        }
        if !p.task_id.is_empty() {
            row.push(("task", json!(p.task_id)));
        }
        let kids = self.children_of(&p.id);
        if !kids.is_empty() && !p.closed {
            row.push(("children", Value::Array(kids)));
        }
        let names = self.looking(&p.id);
        if !names.is_empty() {
            row.push(("looking", json!(names)));
        }
        if let Some(lp) = self.live(&p.id) {
            let last = lp
                .grid
                .last_write()
                .map(|ns| ns as f64 / 1e9)
                .unwrap_or_else(|| {
                    lp.started
                        .duration_since(UNIX_EPOCH)
                        .map(|d| d.as_nanos() as f64 / 1e9)
                        .unwrap_or(0.0)
                });
            row.push(("quiet_for", gojson::float(Server::now_seconds() - last)));
        }
        if let Some(eff) = self.effective_state(&p.id) {
            st = (json!(eff.state), json!(eff.source), json!(eff.detail));
            row.push(("ts", gojson::float(eff.ts)));
            row.push(("session_id", json!(eff.session_id)));
            if let Some(a) = &eff.ask {
                row.push(("ask", a.to_value()));
            }
            if eff.state == state::STATE_BLOCKED {
                if let Some(h) = self.held_for(&p.id) {
                    row.push(("held", h));
                }
            }
        } else if p.closed {
            let detail = match p.exit_code {
                Some(c) => format!("exit={c}"),
                None => "ended before this server started".into(),
            };
            st = (
                json!(state::STATE_DONE),
                json!(state::SRC_PROCESS),
                json!(detail),
            );
            row.push(("ts", gojson::float(p.ended_at as f64)));
            row.push(("session_id", json!(p.id)));
        }
        row.push(("state", st.0));
        row.push(("source", st.1));
        row.push(("detail", st.2));
        self.add_facts(&mut row, p);
        gojson::map(row)
    }

    fn handle_send_text(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let Some(mut text) = r.str("text") else {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.send_text needs text. Pass \"\" explicitly to send a bare Enter.",
            );
        };
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        if super::ready::is_prompt_request(r) && self.gated(&id) {
            return self.prompt_reply(c, r, &id, &text);
        }
        if r.bool("enter").unwrap_or(true) {
            text.push('\r');
        }
        if !id.is_empty() {
            self.mark_input(&id, super::escalate::ends_line(&text));
        }
        if let Err(e) = lp.write(text.as_bytes()) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
        }
        proto::ok_resp(&r.id, gojson::map(vec![("sent", json!(text.len()))]))
    }

    fn handle_send_keys(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        let keys = match r.str_slice("keys") {
            Some(k) if !k.is_empty() => k,
            _ => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!("pane.send_keys needs keys. Known keys: {}", key_names()),
                )
            }
        };
        let mut out = String::new();
        for k in &keys {
            if let Some(seq) = named_key(&k.to_lowercase()) {
                out.push_str(&seq);
                continue;
            }
            if k.chars().count() == 1 {
                out.push_str(k);
                continue;
            }
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "key {} is not known. Known keys: {}",
                    sys::go_quote(k),
                    key_names()
                ),
            );
        }
        if !id.is_empty() {
            self.mark_input(&id, super::escalate::ends_line(&out));
        }
        if let Err(e) = lp.write(out.as_bytes()) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
        }
        proto::ok_resp(&r.id, gojson::map(vec![("sent", json!(keys.len()))]))
    }

    fn handle_run(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let Some(line) = r.str("line") else {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.run needs line. Pass the line to type.",
            );
        };
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        if self.gated(&id) {
            return self.prompt_reply(c, r, &id, &line);
        }
        if !id.is_empty() {
            self.mark_input(&id, true);
        }
        if let Err(e) = lp.write(format!("{line}\r").as_bytes()) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
        }
        proto::ok_resp(&r.id, gojson::map(vec![("sent", json!(line.len() + 1))]))
    }

    fn handle_read(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "this command needs pane. Run: coppice pane list",
            );
        }
        if self.tree.pane(&id).is_none() {
            return no_pane(r, &id);
        }
        let Some(lp) = self.live(&id) else {
            return pane_closed(r, &id);
        };
        let src = r.str("source").unwrap_or_else(|| "visible".into());
        match lp.grid.read(&src) {
            Ok(text) => proto::ok_resp(
                &r.id,
                gojson::map(vec![("text", json!(text)), ("source", json!(src))]),
            ),
            Err(e) if e == GRID_CLOSED => pane_closed(r, &id),
            Err(e) => proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e),
        }
    }

    fn handle_resize(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        let (cols, rows) = match (r.int("cols"), r.int("rows")) {
            (Some(c), Some(rw)) if c > 0 && rw > 0 => (c, rw),
            _ => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    "pane.resize needs positive cols and rows.",
                )
            }
        };
        let res = match &lp.pty {
            Some(p) => p.resize(cols, rows),
            None => lp.grid.resize(cols, rows),
        };
        if let Err(e) = res {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
        }
        if let Some(info) = self.pane_info(&id) {
            let _ = self.update_pane(&info.id, |x| {
                x.cols = cols;
                x.rows = rows;
            });
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("cols", json!(cols)), ("rows", json!(rows))]),
        )
    }

    fn handle_close(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let mut id = r.s("pane");
        if id.is_empty() {
            id = r.s("session");
        }
        let Some(rec) = self.tree.pane(&id) else {
            return no_pane(r, &id);
        };
        self.close_record(&rec);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(rec.id)), ("closed", json!(true))]),
        )
    }

    fn handle_wait_output(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        let Some(rec) = self.tree.pane(&id) else {
            return no_pane(r, &id);
        };
        let contains = r.str("contains");
        let want_state = r.str("state");
        if contains.is_none() && want_state.is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.wait_output needs contains or state.",
            );
        }
        let ms = r.int("timeout_ms").filter(|m| *m > 0).unwrap_or(30000);
        let deadline = Instant::now() + Duration::from_millis(ms as u64);
        while Instant::now() < deadline {
            if c.dead() {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_TIMEOUT,
                    "client disconnected while waiting",
                );
            }
            let Some(lp) = self.live(&rec.id) else {
                return pane_closed(r, &rec.id);
            };
            if let Some(ws) = &want_state {
                if let Some(eff) = self.effective_state(&rec.id) {
                    if &eff.state == ws {
                        let closed = self.pane_info(&rec.id).map(|i| i.closed).unwrap_or(false);
                        return proto::ok_resp(
                            &r.id,
                            gojson::map(vec![
                                ("matched", json!("state")),
                                ("state", json!(eff.state)),
                                ("closed", json!(closed)),
                            ]),
                        );
                    }
                }
            }
            if let Some(want) = &contains {
                if let Ok(text) = lp.grid.read("recent") {
                    if text.contains(want.as_str()) {
                        return proto::ok_resp(
                            &r.id,
                            gojson::map(vec![("matched", json!("text"))]),
                        );
                    }
                }
            }
            thread::sleep(Duration::from_millis(50));
        }
        proto::err_resp(
            &r.id,
            proto::ERR_TIMEOUT,
            &format!(
                "pane {} did not match within {ms} ms. Run: coppice pane read {}",
                rec.id, rec.id
            ),
        )
    }

    /// pane.fork starts a new headless pane that continues the session of
    /// a live one. The parent's adapter must be able to fork and must have
    /// reported a session id of its own. The child copies the parent's
    /// harness, cwd, kind, env, argv, size, workspace and tab, borrows the
    /// parent's session id as its resume target, and carries fork_pending
    /// until its adapter names a session id of its own.
    fn handle_pane_fork(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        if self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a new pane.",
            );
        }
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        let Some(info) = self.pane_info(&id) else {
            return pane_closed(r, &id);
        };
        let Some(proc) = lp
            .adapter
            .clone()
            .filter(|_| info.kind == layout::KIND_HEADLESS)
        else {
            return proto::err_resp(
                &r.id,
                proto::ERR_ADAPTER,
                "a pty pane cannot fork a session",
            );
        };
        let cannot = || {
            proto::err_resp(
                &r.id,
                proto::ERR_ADAPTER,
                &format!("{} cannot fork a session yet", info.harness),
            )
        };
        let Some(a) = adapters::get(&info.harness) else {
            return cannot();
        };
        let mut sid = proc.session_id().unwrap_or_default();
        if sid.is_empty() {
            sid = info.harness_session_id.clone();
        }
        // A pending fork's adapter may answer with the borrowed id, so the
        // flag is the check, not the id.
        let probe = a.fork_argv(&sid);
        if probe.is_none() {
            return cannot();
        }
        if sid.is_empty() || info.fork_pending {
            return proto::err_resp(
                &r.id,
                proto::ERR_ADAPTER,
                &format!(
                    "{} has not reported a session id yet, wait for the first turn",
                    info.harness
                ),
            );
        }
        if let Some(Err(e)) = probe {
            return proto::err_resp(&r.id, proto::ERR_ADAPTER, &e);
        }
        let mut label = clean_label(&r.s("label"));
        if label.is_empty() {
            label = clean_label(&format!("{} fork", info.label));
        }
        let created = self.tree.create_pane(
            &info.workspace,
            &info.tab,
            layout::Pane {
                label,
                cwd: info.cwd.clone(),
                env: info.env.clone(),
                argv: info.argv.clone(),
                kind: info.kind.clone(),
                harness: info.harness.clone(),
                cols: info.cols,
                rows: info.rows,
                harness_session_id: sid,
                parent_pane: id.clone(),
                fork_pending: true,
                ..Default::default()
            },
        );
        let rec = match created {
            Ok(p) => p,
            Err(e @ LayoutError::NoWorkspace(_)) => {
                return proto::err_resp(&r.id, proto::ERR_NO_SUCH_WORKSPACE, &e.to_string())
            }
            Err(e) => return proto::err_resp(&r.id, proto::ERR_NO_SUCH_TAB, &e.to_string()),
        };
        if let Err(e) = self.start_pane(&rec) {
            let _ = self.tree.remove_pane(&rec.id);
            self.remove_live(&rec.id);
            self.states.forget(&rec.id);
            self.save_layout();
            return proto::err_resp(&r.id, proto::ERR_SPAWN_FAILED, &e.msg);
        }
        self.save_layout();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("pane", json!(rec.id)),
                ("workspace", json!(rec.workspace)),
                ("tab", json!(rec.tab)),
                ("parent_pane", json!(id)),
            ]),
        )
    }

    fn handle_pane_rename(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.rename needs pane. Run: coppice pane list",
            );
        }
        let given = r.str("label");
        let label = clean_label(&given.clone().unwrap_or_default());
        if given.is_none() || label.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.rename needs label, at least one character. A blank label is refused, not cleared.",
            );
        }
        let Some(cur) = self.tree.pane(&id) else {
            return no_pane(r, &id);
        };
        if role_or_operator(Some(c)).no_allow {
            if is_foreman_label(&label) {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_UNAUTHORIZED,
                    role::FOREMAN_LABEL_REFUSAL,
                );
            }
            if label != cur.label && self.live_label_taken(&label) {
                return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, role::LABEL_TAKEN_REFUSAL);
            }
        }
        if let Err(e) = self.update_pane(&id, |p| p.label = label.clone()) {
            return proto::err_resp(&r.id, proto::ERR_NO_SUCH_PANE, &e.to_string());
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("label", json!(label))]),
        )
    }
}

/// Why a pane did not start, and whether that is the caller's mistake
/// (bad_request) or a spawn that failed (spawn_failed).
pub struct StartError {
    pub bad: bool,
    pub msg: String,
}

impl StartError {
    fn bad(msg: String) -> StartError {
        StartError { bad: true, msg }
    }
    fn spawn(msg: String) -> StartError {
        StartError { bad: false, msg }
    }
    pub fn code(&self) -> &'static str {
        if self.bad {
            proto::ERR_BAD_REQUEST
        } else {
            proto::ERR_SPAWN_FAILED
        }
    }
}

/// One adapter event as a grid row. Every row ends at column zero.
fn write_transcript(g: &Grid, ev: &adapters::Ev) -> Result<(), String> {
    use adapters::Kind;
    let mut line = match ev.kind {
        Kind::Text => ev.text.clone(),
        Kind::Tool => format!("\x1b[36m{}\x1b[0m {}", ev.tool, ev.detail),
        Kind::State => {
            let mut l = format!("\x1b[33m[{}]\x1b[0m {}", ev.state, ev.detail);
            if let Some(a) = &ev.ask {
                l.push_str(&format!(" {}: {}", a.tool, a.summary));
            }
            l
        }
        Kind::End => format!("\x1b[32m[end]\x1b[0m {}", ev.detail),
        Kind::Error => format!("\x1b[31m[error]\x1b[0m {}", ev.detail),
        Kind::Child => format!("[child] {}", ev.detail),
    };
    line = line.trim_end_matches('\n').replace('\n', "\r\n");
    line.push_str("\r\n");
    g.write(line.as_bytes()).map(|_| ())
}

/// The state a headless event implies. A broken stream is unknown, never
/// idle.
fn state_of(ev: &adapters::Ev) -> &str {
    use adapters::Kind;
    match ev.kind {
        Kind::Text | Kind::Tool => state::STATE_WORKING,
        Kind::State if ev.state.is_empty() => state::STATE_UNKNOWN,
        Kind::State => &ev.state,
        Kind::End if ev.state.is_empty() => state::STATE_DONE,
        Kind::End => &ev.state,
        _ => state::STATE_UNKNOWN,
    }
}

/// Ended records newest first, by end time, then id.
fn sort_ended_newest_first(panes: &mut [layout::Pane]) {
    panes.sort_by(|a, b| b.ended_at.cmp(&a.ended_at).then_with(|| b.id.cmp(&a.id)));
}
