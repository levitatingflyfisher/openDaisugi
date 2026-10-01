//! What a pane is: a grid fed by bytes, with a pty process behind it. A
//! client reads it as plain text or as frames.

use std::collections::BTreeMap;
use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde_json::{json, Value};

use crate::gojson;
use crate::sys;
use crate::vt;

/// How far back `read recent` reaches, in lines.
pub const RECENT_ROWS: usize = 200;
/// The per-pane scrollback, in lines.
const SCROLLBACK_LINES: usize = 2000;

pub const GRID_CLOSED: &str = "this grid is closed";

const PASTE_ON: &str = "\x1b[?2004h";
const PASTE_OFF: &str = "\x1b[?2004l";

fn now_nanos() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

struct GridInner {
    term: vt::Term,
    cols: i64,
    rows: i64,
    closed: bool,
}

/// One terminal, safe for concurrent use.
pub struct Grid {
    inner: Mutex<GridInner>,
    last_write: AtomicI64,
    first_write: AtomicI64,
    paste: AtomicBool,
}

impl Grid {
    pub fn new(cols: i64, rows: i64) -> Result<Grid, String> {
        if cols <= 0 || rows <= 0 {
            return Err(format!("grid size must be positive, got {cols}x{rows}"));
        }
        let term = vt::Term::new(cols as u16, rows as u16, SCROLLBACK_LINES)?;
        Ok(Grid {
            inner: Mutex::new(GridInner {
                term,
                cols,
                rows,
                closed: false,
            }),
            last_write: AtomicI64::new(0),
            first_write: AtomicI64::new(0),
            paste: AtomicBool::new(false),
        })
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, GridInner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub fn write(&self, p: &[u8]) -> Result<usize, String> {
        let g = self.lock();
        if g.closed {
            return Err(GRID_CLOSED.into());
        }
        g.term.feed(p)?;
        if !p.is_empty() {
            let now = now_nanos();
            self.last_write.store(now, Ordering::SeqCst);
            let _ = self
                .first_write
                .compare_exchange(0, now, Ordering::SeqCst, Ordering::SeqCst);
        }
        let s = String::from_utf8_lossy(p);
        let on = s.rfind(PASTE_ON);
        let off = s.rfind(PASTE_OFF);
        if on != off {
            // Go's LastIndex is -1 when absent, so a present one wins.
            let on_i = on.map(|x| x as i64).unwrap_or(-1);
            let off_i = off.map(|x| x as i64).unwrap_or(-1);
            self.paste.store(on_i > off_i, Ordering::SeqCst);
        }
        Ok(p.len())
    }

    /// When the grid first took bytes, in unix nanoseconds, or None.
    pub fn first_write(&self) -> Option<i64> {
        match self.first_write.load(Ordering::SeqCst) {
            0 => None,
            n => Some(n),
        }
    }

    /// Whether the program in the pane has bracketed paste on.
    pub fn paste_mode(&self) -> bool {
        self.paste.load(Ordering::SeqCst)
    }

    /// The terminal title, or "" once closed.
    pub fn title(&self) -> String {
        let g = self.lock();
        if g.closed {
            return String::new();
        }
        g.term.title()
    }

    /// The last OSC 9;4 payload, or "" once closed.
    pub fn progress(&self) -> String {
        let g = self.lock();
        if g.closed {
            return String::new();
        }
        g.term.progress()
    }

    /// When the grid last took bytes, in unix nanoseconds, or None.
    pub fn last_write(&self) -> Option<i64> {
        match self.last_write.load(Ordering::SeqCst) {
            0 => None,
            n => Some(n),
        }
    }

    pub fn resize(&self, cols: i64, rows: i64) -> Result<(), String> {
        if cols <= 0 || rows <= 0 {
            return Err(format!("grid size must be positive, got {cols}x{rows}"));
        }
        let mut g = self.lock();
        if g.closed {
            return Err(GRID_CLOSED.into());
        }
        g.term.resize(cols as u16, rows as u16)?;
        g.cols = cols;
        g.rows = rows;
        Ok(())
    }

    /// The pane as plain text: visible, recent or detection.
    pub fn read(&self, src: &str) -> Result<String, String> {
        let g = self.lock();
        if g.closed {
            return Err(GRID_CLOSED.into());
        }
        match src {
            "visible" => g.term.plain_screen(),
            "recent" => Ok(last_lines(&g.term.plain_all()?, RECENT_ROWS)),
            "detection" => g.term.plain_screen_unwrapped(),
            other => Err(format!(
                "read source {} is not visible, recent or detection",
                sys::go_quote(other)
            )),
        }
    }

    /// One atomic view: the cells, the cursor, and the size they were taken
    /// at.
    #[allow(clippy::type_complexity)]
    pub fn snapshot(&self) -> Result<(Vec<Vec<WireCell>>, [i64; 2], i64, i64), String> {
        let g = self.lock();
        if g.closed {
            return Err(GRID_CLOSED.into());
        }
        let vp = g.term.viewport()?;
        let rows: Vec<Vec<WireCell>> = vp
            .into_iter()
            .map(|row| {
                row.into_iter()
                    .map(|c| WireCell {
                        text: c.text,
                        fg: hex_of(c.fg),
                        bg: hex_of(c.bg),
                        attrs: c.attrs,
                    })
                    .collect()
            })
            .collect();
        let (cx, cy, _) = g.term.cursor();
        let nrows = rows.len() as i64;
        let cols = rows.first().map(|r| r.len() as i64).unwrap_or(0);
        Ok((rows, [cx as i64, cy as i64], cols, nrows))
    }

    /// Frees the terminal. A second call does nothing.
    pub fn close(&self) {
        let mut g = self.lock();
        if g.closed {
            return;
        }
        g.closed = true;
        g.term.close();
    }
}

fn last_lines(s: &str, n: usize) -> String {
    let lines: Vec<&str> = s.trim_end_matches('\n').split('\n').collect();
    let start = lines.len().saturating_sub(n);
    lines[start..].join("\n")
}

fn hex_of(c: vt::Rgb) -> String {
    match c {
        None => String::new(),
        Some((r, g, b)) => format!("#{r:02x}{g:02x}{b:02x}"),
    }
}

/// One grid cell on the wire: [text, fg, bg, attrs].
#[derive(Clone, Debug, PartialEq)]
pub struct WireCell {
    pub text: String,
    pub fg: String,
    pub bg: String,
    pub attrs: u16,
}

impl WireCell {
    fn to_value(&self) -> Value {
        json!([self.text, self.fg, self.bg, self.attrs])
    }
}

fn hash_row(row: &[WireCell]) -> u64 {
    // FNV-1a, 64 bit, as Go's hash/fnv New64a.
    let mut h: u64 = 0xcbf29ce484222325;
    let mut put = |b: &[u8]| {
        for &x in b {
            h ^= x as u64;
            h = h.wrapping_mul(0x100000001b3);
        }
    };
    for c in row {
        put(c.text.as_bytes());
        put(&[0]);
        put(c.fg.as_bytes());
        put(&[0]);
        put(c.bg.as_bytes());
        put(&[c.attrs as u8, (c.attrs >> 8) as u8, 0]);
    }
    h
}

/// One client's view of one pane: the sequence and the row hashes are per
/// client, because two clients attach at different moments.
#[derive(Default)]
pub struct FrameState {
    seq: u64,
    hashes: Vec<u64>,
    cols: i64,
    rows: i64,
    cursor: [i64; 2],
    force: bool,
}

/// One render update.
pub struct Frame {
    pub pane: String,
    pub seq: u64,
    pub cols: i64,
    pub rows: i64,
    pub cursor: [i64; 2],
    pub rows_changed: Vec<(usize, Vec<WireCell>)>,
    pub full: bool,
}

impl Frame {
    pub fn to_value(&self) -> Value {
        // Go writes a map with int keys sorted as text.
        let mut keys: Vec<(String, Value)> = self
            .rows_changed
            .iter()
            .map(|(y, row)| {
                (
                    y.to_string(),
                    Value::Array(row.iter().map(WireCell::to_value).collect()),
                )
            })
            .collect();
        keys.sort_by(|a, b| a.0.cmp(&b.0));
        let mut m = serde_json::Map::new();
        for (k, v) in keys {
            m.insert(k, v);
        }
        let mut p = vec![
            ("event", json!("frame")),
            ("pane", json!(self.pane)),
            ("seq", json!(self.seq)),
            ("cols", json!(self.cols)),
            ("rows", json!(self.rows)),
            ("cursor", json!(self.cursor)),
            ("rows_changed", Value::Object(m)),
        ];
        if self.full {
            p.push(("full", json!(true)));
        }
        gojson::obj(p)
    }
}

impl FrameState {
    pub fn force_full(&mut self) {
        self.force = true;
    }

    /// The frame to send this client, or None when nothing changed.
    pub fn next(&mut self, pane_id: &str, g: &Grid) -> Result<Option<Frame>, String> {
        let (cells, cursor, cols, rows) = g.snapshot()?;
        let full = self.force
            || self.seq == 0
            || self.cols != cols
            || self.rows != rows
            || self.hashes.len() != cells.len();
        let mut changed = Vec::new();
        let mut next = Vec::with_capacity(cells.len());
        for (y, row) in cells.into_iter().enumerate() {
            let h = hash_row(&row);
            next.push(h);
            if full || h != self.hashes[y] {
                changed.push((y, row));
            }
        }
        if changed.is_empty() && cursor == self.cursor {
            return Ok(None);
        }
        self.seq += 1;
        self.hashes = next;
        self.cols = cols;
        self.rows = rows;
        self.cursor = cursor;
        self.force = false;
        Ok(Some(Frame {
            pane: pane_id.into(),
            seq: self.seq,
            cols,
            rows,
            cursor,
            rows_changed: changed,
            full,
        }))
    }
}

/// Whether k names the session a process runs in rather than a setting.
fn session_marker(k: &str) -> bool {
    if matches!(
        k,
        "CLAUDECODE" | "CLAUDE_PID" | "HERDR_PANE" | "HERDR_PANE_ID"
    ) {
        return true;
    }
    let Some(_) = k.strip_prefix("CLAUDE_CODE_") else {
        return false;
    };
    [
        "SESSION",
        "MESSAGING",
        "BRIDGE",
        "CHILD",
        "ENTRYPOINT",
        "EXECPATH",
        "SSE_PORT",
    ]
    .iter()
    .any(|p| k.contains(p))
}

/// The server's environment with the pane's extras and the coppice
/// variables over it. An empty coppice variable is removed, never left as
/// inherited.
pub fn build_env(
    base: Vec<(String, String)>,
    extra: &BTreeMap<String, String>,
    sock: &str,
    pane_id: &str,
    data_dir: &str,
) -> Vec<(String, String)> {
    let mut merged: BTreeMap<String, String> = BTreeMap::new();
    let mut order: Vec<String> = Vec::new();
    let mut set = |k: &str, v: &str| {
        if !merged.contains_key(k) {
            order.push(k.to_string());
        }
        merged.insert(k.to_string(), v.to_string());
    };
    for (k, v) in &base {
        if !session_marker(k) {
            set(k, v);
        }
    }
    // Go ranges a map, in no fixed order; the variables are distinct keys,
    // so only the order in the environment block differs.
    for (k, v) in extra {
        set(k, v);
    }
    set("TERM", "xterm-256color");
    set("COPPICE_SOCK", sock);
    set("COPPICE_PANE", pane_id);
    set("COPPICE_DATA_DIR", data_dir);
    let mut out = Vec::new();
    for k in order {
        let v = merged[&k].clone();
        if matches!(
            k.as_str(),
            "COPPICE_SOCK" | "COPPICE_PANE" | "COPPICE_DATA_DIR"
        ) && v.is_empty()
        {
            continue;
        }
        out.push((k, v));
    }
    out
}

/// Go's exec.LookPath: the first executable regular file named name in a
/// PATH directory.
pub fn look_path(name: &str) -> Option<String> {
    use std::os::unix::fs::PermissionsExt;
    let path = std::env::var("PATH").unwrap_or_default();
    for dir in path.split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let p = format!("{dir}/{name}");
        if let Ok(md) = std::fs::metadata(&p) {
            if !md.is_dir() && md.permissions().mode() & 0o111 != 0 {
                // A relative hit is refused, as Go's exec.ErrDot refuses it.
                if !p.starts_with('/') {
                    return None;
                }
                return Some(p);
            }
        }
    }
    None
}

/// Everything a pty pane needs to start.
pub struct SpawnOpts {
    pub cwd: String,
    pub argv: Vec<String>,
    pub env: BTreeMap<String, String>,
    pub cols: i64,
    pub rows: i64,
    pub sock: String,
    pub pane_id: String,
    pub data_dir: String,
}

/// Signals a pty's done and drained moments.
#[derive(Default)]
pub struct Flag {
    m: Mutex<bool>,
    cv: Condvar,
}

impl Flag {
    pub fn set(&self) {
        *self.m.lock().unwrap_or_else(|e| e.into_inner()) = true;
        self.cv.notify_all();
    }
    pub fn is_set(&self) -> bool {
        *self.m.lock().unwrap_or_else(|e| e.into_inner())
    }
    /// Waits up to d. True when set.
    pub fn wait(&self, d: Option<Duration>) -> bool {
        let g = self.m.lock().unwrap_or_else(|e| e.into_inner());
        match d {
            None => {
                let g = self
                    .cv
                    .wait_while(g, |s| !*s)
                    .unwrap_or_else(|e| e.into_inner());
                *g
            }
            Some(d) => {
                let (g, _) = self
                    .cv
                    .wait_timeout_while(g, d, |s| !*s)
                    .unwrap_or_else(|e| e.into_inner());
                *g
            }
        }
    }
}

/// A process on a pseudo-terminal, its output feeding a grid. The child
/// leads its own session, so its pgid is its pid and a close signals the
/// whole group.
pub struct Pty {
    pid: i32,
    master: Mutex<Option<File>>,
    writer: Mutex<Option<File>>,
    grid: Arc<Grid>,
    pub done: Arc<Flag>,
    pub drained: Arc<Flag>,
    code: Arc<Mutex<Option<i64>>>,
    closing: Arc<AtomicBool>,
    close_once: Mutex<bool>,
}

fn openpty(cols: i64, rows: i64) -> std::io::Result<(OwnedFd, OwnedFd)> {
    // SAFETY: posix_openpt and friends take no Rust pointers but the name
    // buffer, which is live and sized.
    unsafe {
        let m = libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC);
        if m < 0 {
            return Err(std::io::Error::last_os_error());
        }
        let master = OwnedFd::from_raw_fd(m);
        if libc::grantpt(m) != 0 || libc::unlockpt(m) != 0 {
            return Err(std::io::Error::last_os_error());
        }
        let mut name = [0 as libc::c_char; 128];
        if libc::ptsname_r(m, name.as_mut_ptr(), name.len()) != 0 {
            return Err(std::io::Error::last_os_error());
        }
        let s = libc::open(
            name.as_ptr(),
            libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC,
        );
        if s < 0 {
            return Err(std::io::Error::last_os_error());
        }
        let slave = OwnedFd::from_raw_fd(s);
        let ws = libc::winsize {
            ws_row: rows as u16,
            ws_col: cols as u16,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        libc::ioctl(slave.as_raw_fd(), libc::TIOCSWINSZ, &ws);
        Ok((master, slave))
    }
}

impl Pty {
    /// Starts argv on a fresh pty. The child gets the coppice variables
    /// and an empty signal mask, leads its own session, and has the pty as
    /// its controlling terminal.
    pub fn start(o: SpawnOpts, grid: Arc<Grid>) -> Result<Pty, String> {
        if o.argv.is_empty() {
            return Err("pane needs a command to run".into());
        }
        if o.sock.is_empty() || o.pane_id.is_empty() {
            return Err("a pane needs both a socket path and a pane id in SpawnOpts".into());
        }
        let cols = if o.cols <= 0 { 120 } else { o.cols };
        let rows = if o.rows <= 0 { 40 } else { o.rows };
        let argv0 = &o.argv[0];
        // A bare name resolves on PATH, as exec.Command does.
        let path = if argv0.contains('/') {
            argv0.clone()
        } else {
            look_path(argv0).unwrap_or_else(|| argv0.clone())
        };
        let (master, slave) = openpty(cols, rows).map_err(|e| {
            format!(
                "cannot start {argv0}: open /dev/ptmx: {}",
                sys::go_errno_text(&e)
            )
        })?;
        let mk = |f: &OwnedFd| -> Result<Stdio, String> {
            f.try_clone()
                .map(Stdio::from)
                .map_err(|e| format!("cannot start {argv0}: {}", sys::go_errno_text(&e)))
        };
        let (sin, sout, serr) = (mk(&slave)?, mk(&slave)?, mk(&slave)?);
        drop(slave);
        let base: Vec<(String, String)> = std::env::vars_os()
            .map(|(k, v)| {
                (
                    k.to_string_lossy().into_owned(),
                    v.to_string_lossy().into_owned(),
                )
            })
            .collect();
        let env = build_env(base, &o.env, &o.sock, &o.pane_id, &o.data_dir);
        let mut cmd = Command::new(&path);
        cmd.arg0(argv0)
            .args(&o.argv[1..])
            .env_clear()
            .envs(env)
            .current_dir(&o.cwd)
            .stdin(sin)
            .stdout(sout)
            .stderr(serr);
        // SAFETY: only async-signal-safe calls run between fork and exec.
        unsafe {
            cmd.pre_exec(|| {
                let mut set: libc::sigset_t = std::mem::zeroed();
                libc::sigemptyset(&mut set);
                libc::pthread_sigmask(libc::SIG_SETMASK, &set, std::ptr::null_mut());
                if libc::setsid() < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                if libc::ioctl(0, libc::TIOCSCTTY, 0) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let child: Child = cmd.spawn().map_err(|e| {
            format!(
                "cannot start {argv0}: fork/exec {path}: {}",
                sys::go_errno_text(&e)
            )
        })?;
        let pid = child.id() as i32;
        let reader = File::from(master);
        let writer = reader
            .try_clone()
            .map_err(|e| format!("cannot start {argv0}: {}", sys::go_errno_text(&e)))?;
        let master_keep = reader
            .try_clone()
            .map_err(|e| format!("cannot start {argv0}: {}", sys::go_errno_text(&e)))?;
        let done = Arc::new(Flag::default());
        let drained = Arc::new(Flag::default());
        let code = Arc::new(Mutex::new(None));
        let closing = Arc::new(AtomicBool::new(false));
        {
            let g = grid.clone();
            let drained = drained.clone();
            let closing = closing.clone();
            let mut r = reader;
            thread::spawn(move || {
                let mut buf = vec![0u8; 32 << 10];
                loop {
                    // A close wakes this loop within one poll, whoever
                    // still holds the pty's other end.
                    if closing.load(Ordering::SeqCst) {
                        break;
                    }
                    let mut pfd = libc::pollfd {
                        fd: r.as_raw_fd(),
                        events: libc::POLLIN,
                        revents: 0,
                    };
                    // SAFETY: pfd is one live pollfd.
                    let n = unsafe { libc::poll(&mut pfd, 1, 100) };
                    if n < 0 {
                        if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted
                        {
                            continue;
                        }
                        break;
                    }
                    if n == 0 {
                        continue;
                    }
                    match r.read(&mut buf) {
                        Ok(0) => break,
                        Ok(n) => {
                            let _ = g.write(&buf[..n]);
                        }
                        Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                        Err(_) => break,
                    }
                }
                drained.set();
            });
        }
        {
            let done = done.clone();
            let code = code.clone();
            let mut child = child;
            thread::spawn(move || {
                let c = match child.wait() {
                    Ok(st) => match st.code() {
                        Some(c) => c as i64,
                        // Go's ExitCode is -1 for a process a signal ended.
                        None => {
                            let _ = st.signal();
                            -1
                        }
                    },
                    Err(_) => -1,
                };
                *code.lock().unwrap_or_else(|e| e.into_inner()) = Some(c);
                done.set();
            });
        }
        Ok(Pty {
            pid,
            master: Mutex::new(Some(master_keep)),
            writer: Mutex::new(Some(writer)),
            grid,
            done,
            drained,
            code,
            closing,
            close_once: Mutex::new(false),
        })
    }

    pub fn pid(&self) -> i32 {
        self.pid
    }

    pub fn write(&self, b: &[u8]) -> Result<usize, String> {
        let mut g = self.writer.lock().unwrap_or_else(|e| e.into_inner());
        match g.as_mut() {
            None => Err(format!("write /dev/ptmx: {}", "file already closed")),
            Some(f) => f
                .write_all(b)
                .map(|_| b.len())
                .map_err(|e| format!("write /dev/ptmx: {}", sys::go_errno_text(&e))),
        }
    }

    /// Resizes the grid first, then the child's window, so a redraw on
    /// SIGWINCH finds the grid already the new size.
    pub fn resize(&self, cols: i64, rows: i64) -> Result<(), String> {
        self.grid.resize(cols, rows)?;
        let g = self.master.lock().unwrap_or_else(|e| e.into_inner());
        let Some(m) = g.as_ref() else {
            return Err("file already closed".into());
        };
        let ws = libc::winsize {
            ws_row: rows as u16,
            ws_col: cols as u16,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        // SAFETY: the descriptor is open while the lock is held.
        let rc = unsafe { libc::ioctl(m.as_raw_fd(), libc::TIOCSWINSZ, &ws) };
        if rc != 0 {
            return Err(sys::go_errno_text(&std::io::Error::last_os_error()));
        }
        Ok(())
    }

    pub fn exit_code(&self) -> Option<i64> {
        *self.code.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Tears down the whole process group: SIGHUP, a short grace, then
    /// SIGKILL for whatever is left. Then the master closes and the reader
    /// drains. A second call does nothing.
    pub fn close(&self) {
        let mut once = self.close_once.lock().unwrap_or_else(|e| e.into_inner());
        if *once {
            return;
        }
        *once = true;
        if self.pid > 0 {
            // SAFETY: kill takes no pointers.
            unsafe {
                libc::kill(-self.pid, libc::SIGHUP);
            }
            thread::sleep(Duration::from_millis(200));
            // SAFETY: as above.
            unsafe {
                if libc::kill(-self.pid, 0) == 0 {
                    libc::kill(-self.pid, libc::SIGKILL);
                }
            }
        }
        self.closing.store(true, Ordering::SeqCst);
        self.writer.lock().unwrap_or_else(|e| e.into_inner()).take();
        self.master.lock().unwrap_or_else(|e| e.into_inner()).take();
        self.drained.wait(None);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn env_drops_session_markers_and_sets_coppice_vars() {
        let base = vec![
            ("PATH".to_string(), "/bin".to_string()),
            ("CLAUDECODE".to_string(), "1".to_string()),
            ("COPPICE_PANE".to_string(), "w0:p0".to_string()),
        ];
        let env = build_env(base, &BTreeMap::new(), "/s.sock", "w1:p1", "");
        let m: BTreeMap<_, _> = env.into_iter().collect();
        assert_eq!(m.get("COPPICE_PANE").map(String::as_str), Some("w1:p1"));
        assert!(!m.contains_key("CLAUDECODE"));
        assert!(!m.contains_key("COPPICE_DATA_DIR"));
        assert_eq!(m.get("TERM").map(String::as_str), Some("xterm-256color"));
    }

    #[test]
    fn a_frame_carries_only_changed_rows() {
        let g = Grid::new(10, 3).unwrap();
        let mut fs = FrameState::default();
        let f = fs.next("p", &g).unwrap().unwrap();
        assert!(f.full && f.rows_changed.len() == 3);
        assert!(fs.next("p", &g).unwrap().is_none());
        g.write(b"x").unwrap();
        let f = fs.next("p", &g).unwrap().unwrap();
        assert!(!f.full);
        assert_eq!(f.rows_changed.len(), 1);
        assert_eq!(g.read("visible").unwrap(), "x");
    }
}
