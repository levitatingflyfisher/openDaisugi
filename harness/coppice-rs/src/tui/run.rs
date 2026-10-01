//! One run of the floor: the connections, the frame writer, the key
//! decoder, and the loop that drives them.

use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};
use std::io::{self, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender, TryRecvError};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Map, Value};

use crate::attach::{self, KeyReader};
use crate::tiles::{Layout, Tiles};

use super::facts::{decode_facts, hint_min_cols, FACTS_MIN_COLS};
use super::keys::{parse_sgr, read_key_from, GetByte, Key, ESC_WAIT};
use super::model::{ask_of, s, Model, Obj, Size};
use super::prompt::read_floor_state;
use super::render::{render, window_count};
use super::speak::Speaker;
use super::tree::decode_tasks;
use super::voice::{Clip, VoiceTarget};
use super::voicekey::{VoiceCheck, VoiceResult};

/// What a floor needs to run.
pub struct Options {
    pub socket: PathBuf,
    /// The terminal's columns and rows, read before every render.
    pub size: Box<dyn Fn() -> (i64, i64)>,
    pub cwd: String,
    pub default: String,
    pub data_dir: String,
    pub leave: u8,
    pub views: Vec<String>,
    pub web_url: String,
    pub talk: u8,
    /// Where frames go.
    pub out: Box<dyn Write>,
    /// True when stdin is a terminal: raw mode, the mouse and SIGWINCH.
    pub tty: bool,
}

pub const MOUSE_ON: &str = "\x1b[?1000h\x1b[?1006h";
pub const MOUSE_OFF: &str = "\x1b[?1000l\x1b[?1006l";
pub const PASTE_WAIT: Duration = ESC_WAIT;
pub const POLL_EVERY: Duration = Duration::from_secs(2);
pub const CALL_DEADLINE: Duration = Duration::from_secs(10);
pub const DOUBLE_CLICK: Duration = Duration::from_millis(400);
pub const KEY_PAUSE: Duration = Duration::from_millis(750);
pub const ENDED_EVERY: Duration = Duration::from_secs(10);

pub const CURSOR_SHOW: &str = "\x1b[?25h";
pub const CURSOR_HIDE: &str = "\x1b[?25l";

/// The words a server that is gone starts with.
pub const SERVER_GONE: &str = "the server went away";

/// Why a run or a call stopped.
#[derive(Clone, Debug, PartialEq)]
pub enum FloorErr {
    /// The server is gone: the floor ends with exit 3.
    Gone(String),
    Other(String),
}

impl FloorErr {
    pub fn text(&self) -> String {
        match self {
            FloorErr::Gone(s) | FloorErr::Other(s) => s.clone(),
        }
    }
}

fn gone_dial(socket: &std::path::Path) -> FloorErr {
    FloorErr::Gone(format!(
        "{SERVER_GONE}: cannot reach the server at {}. Run: coppice server start",
        socket.display()
    ))
}

fn io_text(e: &io::Error) -> String {
    if e.kind() == io::ErrorKind::UnexpectedEof {
        return "EOF".into();
    }
    if e.kind() == io::ErrorKind::WouldBlock || e.kind() == io::ErrorKind::TimedOut {
        return "i/o timeout".into();
    }
    e.to_string()
}

/// Go's encoding of one request: the keys sorted, as json.Marshal of a
/// map sorts them.
pub fn encode(req: &Map<String, Value>) -> String {
    let sorted: BTreeMap<&String, &Value> = req.iter().collect();
    let mut m = Map::new();
    for (k, v) in sorted {
        m.insert(k.clone(), v.clone());
    }
    let mut line = crate::cli::gofmt::marshal(&Value::Object(m));
    line.push('\n');
    line
}

/// Sends one request over a fresh connection and returns its result.
pub fn call(socket: &std::path::Path, cmd: &str, params: Option<Obj>) -> Result<Obj, FloorErr> {
    call_within(socket, cmd, params, CALL_DEADLINE)
}

/// call with its own deadline.
pub fn call_within(
    socket: &std::path::Path,
    cmd: &str,
    params: Option<Obj>,
    deadline: Duration,
) -> Result<Obj, FloorErr> {
    let Ok(conn) = UnixStream::connect(socket) else {
        return Err(gone_dial(socket));
    };
    let _ = conn.set_read_timeout(Some(deadline));
    let _ = conn.set_write_timeout(Some(deadline));
    let mut req = Map::new();
    req.insert("id".into(), json!("1"));
    req.insert("cmd".into(), json!(cmd));
    for (k, v) in params.unwrap_or_default() {
        req.insert(k, v);
    }
    if let Err(e) = (&conn).write_all(encode(&req).as_bytes()) {
        return Err(FloorErr::Other(io_text(&e)));
    }
    let mut dec = crate::proto::Decoder::new(BufReader::new(&conn));
    let line = dec.next_line().map_err(|e| FloorErr::Other(io_text(&e)))?;
    let v: Value = serde_json::from_slice(&line).map_err(|e| FloorErr::Other(e.to_string()))?;
    let r = v.as_object().cloned().unwrap_or_default();
    if r.get("ok").and_then(Value::as_bool) != Some(true) {
        if let Some(Value::Object(e)) = r.get("error") {
            return Err(FloorErr::Other(s(e, "message")));
        }
        return Err(FloorErr::Other(format!(
            "{cmd} failed with no error message"
        )));
    }
    Ok(match r.get("result") {
        Some(Value::Object(m)) => m.clone(),
        _ => Map::new(),
    })
}

/// A list of objects under key of a result.
pub fn list_of(res: &Obj, key: &str) -> Vec<Obj> {
    match res.get(key) {
        Some(Value::Array(a)) => a.iter().filter_map(|v| v.as_object().cloned()).collect(),
        _ => Vec::new(),
    }
}

/// Builds request params from pairs.
pub fn params(pairs: Vec<(&str, Value)>) -> Option<Obj> {
    let mut m = Map::new();
    for (k, v) in pairs {
        m.insert(k.to_string(), v);
    }
    Some(m)
}

pub fn now_seconds() -> f64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs_f64())
        .unwrap_or(0.0)
}

/// Writes a frame only when the lines or the cursor changed.
pub struct FrameWriter {
    pub last: Vec<String>,
    pub cols: i64,
    pub last_x: i64,
    pub last_y: i64,
    pub last_hidden: bool,
    pub clear: bool,
}

impl FrameWriter {
    pub fn new() -> FrameWriter {
        FrameWriter {
            last: Vec::new(),
            cols: 0,
            last_x: 0,
            last_y: 0,
            last_hidden: false,
            clear: false,
        }
    }

    /// The bytes of one frame, or None when nothing changed.
    pub fn frame(
        &mut self,
        lines: Vec<String>,
        cols: i64,
        x: i64,
        y: i64,
        hidden: bool,
    ) -> Option<String> {
        if !self.clear
            && self.cols == cols
            && lines == self.last
            && self.last_x == x
            && self.last_y == y
            && self.last_hidden == hidden
        {
            return None;
        }
        let mut b = String::new();
        if self.clear || self.cols != cols || lines.len() != self.last.len() {
            b.push_str("\x1b[2J");
        }
        b.push_str("\x1b[H");
        let n = lines.len();
        for (i, l) in lines.iter().enumerate() {
            b.push_str("\x1b[K");
            b.push_str(l);
            if i + 1 < n {
                b.push_str("\r\n");
            }
        }
        b.push_str(&format!("\x1b[{};{}H", y + 1, x + 1));
        if hidden {
            b.push_str(CURSOR_HIDE);
        } else if self.last_hidden {
            b.push_str(CURSOR_SHOW);
        }
        self.last = lines;
        self.cols = cols;
        self.clear = false;
        self.last_x = x;
        self.last_y = y;
        self.last_hidden = hidden;
        Some(b)
    }
}

impl Default for FrameWriter {
    fn default() -> Self {
        Self::new()
    }
}

/// One line from the event connection, or the end of it.
pub enum LineRes {
    Line(Vec<u8>),
    Err,
}

/// What comes back from work done on the side.
pub enum Async {
    Promoted(String),
    VoiceDone(VoiceResult),
    VoiceChecked(VoiceCheck),
}

/// Text for one pane.
pub struct Typed {
    pub pane: String,
    pub b: Vec<u8>,
}

/// The state of one run.
pub struct Floor<'k> {
    pub o: Options,
    pub m: Model,
    pub fw: FrameWriter,
    pub keys: &'k Receiver<Option<u8>>,
    pub lines: Receiver<LineRes>,
    pub conn: UnixStream,
    pub detaching: BTreeSet<String>,
    pub sizes: HashMap<String, Size>,
    pub sizing: BTreeSet<String>,
    pub kr: Option<KeyReader>,
    pub hold_at: Option<Instant>,
    pub text_q: Vec<Typed>,
    pub text_busy: bool,
    pub last_press: Option<Instant>,
    pub last_slot: i64,
    pub last_pane: String,
    pub async_tx: Sender<Async>,
    pub async_rx: Receiver<Async>,
    pub ended_due: bool,
    pub ended_read: Option<Instant>,
    pub replay: VecDeque<u8>,
    pub pause_until: Option<Instant>,
    pub pause_at: Option<Instant>,
    pub drag_from: String,
    pub sp: Speaker,
    pub rec: Option<Clip>,
    pub rec_for: String,
    pub rec_to: Option<VoiceTarget>,
    pub checking: bool,
    pub keys_to_talk: Option<Instant>,
    pub await_device: bool,
    pub hearing: i64,
    pub utf8_pending: Vec<u8>,
}

/// Reads key bytes: the replayed ones first, then the keys.
impl GetByte for Floor<'_> {
    fn get(&mut self, wait: Duration) -> Option<Option<u8>> {
        if let Some(b) = self.replay.pop_front() {
            return Some(Some(b));
        }
        match self.keys.recv_timeout(wait) {
            Ok(Some(b)) => Some(Some(b)),
            Ok(None) | Err(RecvTimeoutError::Disconnected) => Some(None),
            Err(RecvTimeoutError::Timeout) => None,
        }
    }
}

/// Set by the SIGWINCH handler while a floor runs on a terminal.
static RESIZED: AtomicBool = AtomicBool::new(false);

extern "C" fn on_winch(_: libc::c_int) {
    RESIZED.store(true, Ordering::SeqCst);
}

/// Runs the floor until the operator quits, the keys close, or the server
/// goes away.
pub fn run(o: Options, keys: &Receiver<Option<u8>>) -> Result<(), FloorErr> {
    let mut o = o;
    if o.default.is_empty() {
        o.default = "claude".into();
    }
    let _raw = if o.tty {
        let r = attach::Raw::on();
        let _ = o.out.write_all(MOUSE_ON.as_bytes());
        let _ = o.out.flush();
        // SAFETY: installs a handler that only stores to an atomic.
        unsafe {
            libc::signal(libc::SIGWINCH, on_winch as *const () as libc::sighandler_t);
        }
        Some(r)
    } else {
        None
    };
    let res = run_inner(o, keys);
    if _raw.is_some() {
        // SAFETY: restores the default action.
        unsafe {
            libc::signal(libc::SIGWINCH, libc::SIG_DFL);
        }
    }
    res
}

fn run_inner(mut o: Options, keys: &Receiver<Option<u8>>) -> Result<(), FloorErr> {
    let tty = o.tty;
    let restore = |out: &mut Box<dyn Write>| {
        if tty {
            let _ = out.write_all(format!("{MOUSE_OFF}{CURSOR_SHOW}").as_bytes());
            let _ = out.flush();
        }
    };
    let conn = match UnixStream::connect(&o.socket) {
        Ok(c) => c,
        Err(_) => {
            restore(&mut o.out);
            return Err(gone_dial(&o.socket));
        }
    };
    let _ = conn.set_read_timeout(Some(CALL_DEADLINE));
    let _ = conn.set_write_timeout(Some(CALL_DEADLINE));
    let gone = |e: &str| FloorErr::Gone(format!("{SERVER_GONE}: {e}"));
    let sub = encode(
        json!({"id": "sub", "cmd": "events.subscribe", "kinds": ["state"], "panes": "*"})
            .as_object()
            .expect("an object"),
    );
    if let Err(e) = (&conn).write_all(sub.as_bytes()) {
        restore(&mut o.out);
        return Err(gone(&io_text(&e)));
    }
    let reader = match conn.try_clone() {
        Ok(r) => r,
        Err(e) => {
            restore(&mut o.out);
            return Err(FloorErr::Other(e.to_string()));
        }
    };
    let mut dec = crate::proto::Decoder::new(BufReader::new(reader));
    loop {
        let line = match dec.next_line() {
            Ok(l) => l,
            Err(e) => {
                restore(&mut o.out);
                return Err(gone(&io_text(&e)));
            }
        };
        let Ok(v) = serde_json::from_slice::<Value>(&line) else {
            continue;
        };
        let Some(r) = v.as_object() else { continue };
        if r.get("id").and_then(Value::as_str) != Some("sub") {
            continue;
        }
        if r.get("ok").and_then(Value::as_bool) != Some(true) {
            let mut msg = "no error message".to_string();
            if let Some(Value::Object(e)) = r.get("error") {
                msg = s(e, "message");
            }
            restore(&mut o.out);
            return Err(FloorErr::Other(format!("events.subscribe: {msg}")));
        }
        break;
    }
    let more = encode(
        json!({"id": "sub-more", "cmd": "events.subscribe", "kinds": ["note", "child"], "panes": "*"})
            .as_object()
            .expect("an object"),
    );
    if let Err(e) = (&conn).write_all(more.as_bytes()) {
        restore(&mut o.out);
        return Err(gone(&io_text(&e)));
    }
    let _ = conn.set_read_timeout(None);
    let _ = conn.set_write_timeout(None);

    let (ltx, lrx) = mpsc::channel();
    thread::spawn(move || loop {
        match dec.next_line() {
            Ok(l) => {
                if ltx.send(LineRes::Line(l)).is_err() {
                    return;
                }
            }
            Err(_) => {
                let _ = ltx.send(LineRes::Err);
                return;
            }
        }
    });

    let (cols, _) = (o.size)();
    let (layout, locked) = read_floor_state(&o.data_dir);
    let mut ts = Tiles::new(layout, window_count(cols));
    ts.locked = locked;
    let (atx, arx) = mpsc::channel();
    let m = Model {
        default: o.default.clone(),
        tiles: Some(ts),
        leave: o.leave,
        talk: o.talk,
        header_y: -1,
        ended_y: -1,
        body_cur: -1,
        ..Default::default()
    };
    let mut f = Floor {
        o,
        m,
        fw: FrameWriter::new(),
        keys,
        lines: lrx,
        conn,
        detaching: BTreeSet::new(),
        sizes: HashMap::new(),
        sizing: BTreeSet::new(),
        kr: None,
        hold_at: None,
        text_q: Vec::new(),
        text_busy: false,
        last_press: None,
        last_slot: 0,
        last_pane: String::new(),
        async_tx: atx,
        async_rx: arx,
        ended_due: false,
        ended_read: None,
        replay: VecDeque::new(),
        pause_until: None,
        pause_at: None,
        drag_from: String::new(),
        sp: Speaker::default(),
        rec: None,
        rec_for: String::new(),
        rec_to: None,
        checking: false,
        keys_to_talk: None,
        await_device: false,
        hearing: 0,
        utf8_pending: Vec::new(),
    };
    let res = f.drive();
    f.stop_voice();
    f.detach_all();
    restore(&mut f.o.out);
    let _ = f.conn.shutdown(std::net::Shutdown::Both);
    res
}

impl Floor<'_> {
    fn size(&self) -> (i64, i64) {
        (self.o.size)()
    }

    pub fn cols(&self) -> i64 {
        self.size().0
    }

    /// Writes to the terminal.
    pub fn write_out(&mut self, s: &str) {
        let _ = self.o.out.write_all(s.as_bytes());
        let _ = self.o.out.flush();
    }

    fn drive(&mut self) -> Result<(), FloorErr> {
        self.seed_notes();
        self.refresh()?;
        self.read_facts();
        self.start_fill();
        self.render()?;
        let mut next_tick = Instant::now() + POLL_EVERY;
        loop {
            if let Some(b) = self.replay.pop_front() {
                if self.key(b)? {
                    return Ok(());
                }
                continue;
            }
            // Lines and work done on the side first, each drawn as it comes.
            let mut did = false;
            loop {
                match self.lines.try_recv() {
                    Ok(LineRes::Line(l)) => {
                        did = true;
                        self.on_line(&l)?;
                    }
                    Ok(LineRes::Err) | Err(TryRecvError::Disconnected) => {
                        return Err(FloorErr::Gone(format!(
                            "{SERVER_GONE}: the event connection closed"
                        )));
                    }
                    Err(TryRecvError::Empty) => break,
                }
            }
            while let Ok(a) = self.async_rx.try_recv() {
                did = true;
                match a {
                    Async::Promoted(msg) => self.m.message = msg,
                    Async::VoiceChecked(c) => self.voice_check_done(c),
                    Async::VoiceDone(r) => self.voice_heard(r)?,
                }
                self.render()?;
            }
            if RESIZED.swap(false, Ordering::SeqCst) {
                did = true;
                self.render()?;
            }
            let now = Instant::now();
            if matches!(self.pause_at, Some(t) if now >= t) {
                self.pause_at = None;
                self.m.paused.clear();
                self.render()?;
            }
            if matches!(self.hold_at, Some(t) if now >= t) {
                self.flush_hold()?;
                self.render()?;
            }
            if now >= next_tick {
                next_tick = now + POLL_EVERY;
                self.m.expire_ended(Instant::now());
                self.refresh()?;
                self.read_facts();
                self.render()?;
            }
            if did {
                continue;
            }
            let mut wait = Duration::from_millis(20);
            for t in [self.pause_at, self.hold_at, Some(next_tick)]
                .into_iter()
                .flatten()
            {
                wait = wait.min(t.saturating_duration_since(Instant::now()));
            }
            match self.keys.recv_timeout(wait) {
                Ok(Some(b)) => {
                    if self.key(b)? {
                        return Ok(());
                    }
                }
                Ok(None) | Err(RecvTimeoutError::Disconnected) => return Ok(()),
                Err(RecvTimeoutError::Timeout) => {}
            }
        }
    }

    fn on_line(&mut self, line: &[u8]) -> Result<(), FloorErr> {
        let Ok(Value::Object(ev)) = serde_json::from_slice::<Value>(line) else {
            return Ok(());
        };
        let mut changed = self.detach_reply(&ev);
        let input = self.input_reply(&ev)?;
        changed = changed || input;
        match ev.get("event").and_then(Value::as_str) {
            Some("state") => {
                self.m.event(&ev);
                changed = true;
                if ended(&ev) {
                    self.refresh()?;
                }
            }
            Some("note") => {
                self.m.add_note(&s(&ev, "text"));
                changed = true;
            }
            Some("child") => {
                self.m.child(&ev);
                changed = true;
            }
            Some("frame") => changed = self.apply_frame(&ev),
            _ => {}
        }
        if changed {
            self.render()?;
        }
        Ok(())
    }

    /// Gives the keys to the rail and drops every key already typed and
    /// every key for KEY_PAUSE.
    pub fn pause_keys(&mut self, text: &str) {
        self.set_typing("");
        loop {
            let (b, more, open) = self.next_key();
            if !more || !open {
                break;
            }
            let (_, closed) = read_key_from(self, b);
            if closed {
                break;
            }
        }
        let now = Instant::now();
        self.pause_until = Some(now + KEY_PAUSE);
        self.pause_at = Some(now + KEY_PAUSE);
        self.m.paused = text.to_string();
    }

    pub fn paused(&self) -> bool {
        matches!(self.pause_until, Some(t) if Instant::now() < t)
    }

    /// Runs before every render: the agent with the keys stays in a shown
    /// window, or the keys pause.
    fn keep_typing(&mut self, n: i64) {
        let id = self.m.typing.clone();
        if id.is_empty() || self.m.tiles.is_none() {
            return;
        }
        if !self.m.has(&id) {
            let name = self.m.name_of(&id);
            self.pause_keys(&format!("{name} ended. Keys paused."));
            self.m.cursor_to_ended(&id);
            return;
        }
        let ts = self.m.tiles.as_ref().expect("tiles");
        if let Some(slot) = ts.slot_of(&id) {
            if (slot as i64) < n {
                return;
            }
        }
        if n == 0 {
            let label = self.m.label_of(&id);
            self.pause_keys(&format!("{label} left the screen. Keys paused."));
            return;
        }
        let ts = self.m.tiles.as_mut().expect("tiles");
        let target = ts.focus.max(0).min(n - 1) as usize;
        while ts.slots.len() <= target {
            ts.slots.push(String::new());
        }
        if let Some(from) = ts.slot_of(&id) {
            ts.slots[from] = ts.slots[target].clone();
        }
        ts.slots[target] = id;
        ts.focus = target as i64;
    }

    /// Writes one request on the event connection.
    pub fn send(&mut self, req: Value) -> Result<(), FloorErr> {
        let _ = self.conn.set_write_timeout(Some(CALL_DEADLINE));
        let line = encode(req.as_object().expect("a request is an object"));
        let res = (&self.conn).write_all(line.as_bytes());
        let _ = self.conn.set_write_timeout(None);
        res.map_err(|e| FloorErr::Gone(format!("{SERVER_GONE}: {}", io_text(&e))))
    }

    /// Attaches every shown pane the floor does not watch yet, resizes the
    /// ones whose tile changed size, and detaches the ones no longer shown.
    fn sync_tiles(&mut self) -> Result<(), FloorErr> {
        let cols = self.cols();
        let mut shown: BTreeSet<String> = BTreeSet::new();
        if let Some(ts) = &self.m.tiles {
            for id in ts.shown(window_count(cols)) {
                if !id.is_empty() {
                    shown.insert(id);
                }
            }
        }
        if !self.m.typing.is_empty() && !shown.contains(&self.m.typing) {
            let name = self.m.name_of(&self.m.typing.clone());
            self.pause_keys(&format!("{name} left the screen. Keys paused."));
        }
        for id in &shown {
            let size = self.m.tile_sizes.get(id).copied();
            if self.m.screens.contains_key(id) {
                if let Some(size) = size {
                    if self.sizes.get(id) != Some(&size) && !self.sizing.contains(id) {
                        self.send(json!({
                            "id": format!("vr-{id}"), "cmd": "pane.resize", "pane": id,
                            "cols": size.cols, "rows": size.rows,
                        }))?;
                        self.sizes.insert(id.clone(), size);
                        self.sizing.insert(id.clone());
                    }
                }
                continue;
            }
            if self.detaching.contains(id) {
                continue;
            }
            let mut req = json!({"id": format!("va-{id}"), "cmd": "pane.attach", "pane": id});
            if let Some(size) = size {
                req["cols"] = json!(size.cols);
                req["rows"] = json!(size.rows);
            }
            self.send(req)?;
            self.m.screens.insert(id.clone(), attach::Screen::new(0, 0));
            if let Some(size) = size {
                self.sizes.insert(id.clone(), size);
                self.sizing.insert(id.clone());
            }
        }
        let watched: Vec<String> = self.m.screens.keys().cloned().collect();
        let mut watched = watched;
        watched.sort();
        for id in watched {
            if shown.contains(&id) {
                continue;
            }
            self.detach_tile(&id)?;
        }
        Ok(())
    }

    fn detach_tile(&mut self, id: &str) -> Result<(), FloorErr> {
        self.m.screens.remove(id);
        self.sizes.remove(id);
        self.sizing.remove(id);
        self.detaching.insert(id.to_string());
        self.send(json!({"id": format!("vd-{id}"), "cmd": "pane.detach", "pane": id}))
    }

    fn detach_reply(&mut self, ev: &Obj) -> bool {
        let id = s(ev, "id");
        match id.strip_prefix("vd-") {
            Some(p) => {
                self.detaching.remove(p);
                true
            }
            None => false,
        }
    }

    fn input_reply(&mut self, ev: &Obj) -> Result<bool, FloorErr> {
        let id = s(ev, "id");
        if let Some(pane) = id.strip_prefix("va-") {
            if self.m.screens.contains_key(pane) {
                self.sizing.remove(pane);
            }
            return Ok(true);
        }
        let is_text = id.starts_with("vt-");
        if !is_text && !id.starts_with("vr-") {
            return Ok(false);
        }
        let msg = reply_error(ev);
        if !msg.is_empty() {
            self.m.message = msg;
        }
        if !is_text {
            let pane = &id[3..];
            if self.m.screens.contains_key(pane) {
                self.sizing.remove(pane);
            }
            return Ok(true);
        }
        self.text_busy = false;
        self.pump_text()?;
        Ok(true)
    }

    /// Gives the keyboard to the tile of pane id, or to the roster.
    pub fn set_typing(&mut self, id: &str) {
        self.hold_at = None;
        if let Some(kr) = &mut self.kr {
            self.replay.extend(kr.held());
        }
        self.kr = None;
        self.utf8_pending.clear();
        self.m.typing = id.to_string();
        if id.is_empty() || self.m.is_headless(id) {
            return;
        }
        let mut kr = KeyReader::new(self.o.leave);
        kr.report_mouse = true;
        kr.drop_reports = true;
        self.kr = Some(kr);
    }

    fn arm_hold(&mut self) {
        self.hold_at = match &self.kr {
            Some(kr) if kr.pending() => kr.hold().map(|d| Instant::now() + d),
            _ => None,
        };
    }

    /// Takes byte b for the tile that owns the keyboard, and every byte
    /// already waiting after it.
    fn type_keys(&mut self, b: u8) -> Result<bool, FloorErr> {
        let mut b = b;
        let mut closed = false;
        loop {
            if self.talk_wants(b) {
                self.replay.push_front(b);
                break;
            }
            let Some(kr) = &mut self.kr else { break };
            let (act, out) = kr.feed(b);
            self.typed_action(act, &out)?;
            if act != attach::Action::None || self.m.typing.is_empty() {
                break;
            }
            let (next, more, open) = self.next_key();
            if !open {
                closed = true;
                break;
            }
            if !more {
                break;
            }
            b = next;
        }
        self.arm_hold();
        self.pump_text()?;
        Ok(closed)
    }

    /// Takes byte b for a headless window, editing its own input line.
    fn headless_keys(&mut self, b: u8) -> Result<bool, FloorErr> {
        let start = self.m.typing.clone();
        let mut b = b;
        let mut closed = false;
        loop {
            if self.talk_wants(b) {
                self.replay.push_front(b);
                break;
            }
            let (ks, cl) = read_key_from(self, b);
            if cl {
                closed = true;
            }
            for k in ks {
                if self.m.typing.is_empty() {
                    if self.handle(k)? {
                        return Ok(true);
                    }
                    continue;
                }
                if self.m.typing != start {
                    break;
                }
                self.headless_key(k)?;
            }
            if closed || self.m.typing != start {
                break;
            }
            let (next, more, open) = self.next_key();
            if !open {
                closed = true;
                break;
            }
            if !more {
                break;
            }
            b = next;
        }
        Ok(closed)
    }

    fn headless_key(&mut self, k: Key) -> Result<(), FloorErr> {
        let id = self.m.typing.clone();
        match k {
            Key::Mouse(c) => return self.click(c),
            Key::Esc => self.set_typing(""),
            Key::Left => self.headless_move(&id, -1),
            Key::Right => self.headless_move(&id, 1),
            Key::Byte(b) => {
                if b == self.m.leave {
                    self.set_typing("");
                } else if b == 0x15 {
                    self.m.set_headless_buf(&id, String::new(), 0);
                } else if b == 0x7f || b == 0x08 {
                    self.headless_backspace(&id);
                } else if b == b'\r' || b == b'\n' {
                    if self.more_waiting() {
                        self.headless_insert(&id, " ");
                        return Ok(());
                    }
                    return self.send_headless_line(&id);
                } else if (0x20..0x7f).contains(&b) {
                    self.headless_insert(&id, &(b as char).to_string());
                } else if b >= 0x80 {
                    self.feed_headless_rune(&id, b);
                }
            }
            _ => {}
        }
        Ok(())
    }

    pub fn headless_insert(&mut self, id: &str, text: &str) {
        let (line, cur) = self.m.headless_buf(id);
        let cur = cur.min(line.len());
        let new = format!("{}{text}{}", &line[..cur], &line[cur..]);
        self.m.set_headless_buf(id, new, cur + text.len());
    }

    fn feed_headless_rune(&mut self, id: &str, b: u8) {
        self.utf8_pending.push(b);
        if !attach::full_rune(&self.utf8_pending) {
            return;
        }
        match decode_rune(&self.utf8_pending) {
            None => {
                self.utf8_pending.remove(0);
            }
            Some(size) => {
                let text = String::from_utf8_lossy(&self.utf8_pending[..size]).into_owned();
                self.headless_insert(id, &text);
                self.utf8_pending.drain(..size);
            }
        }
    }

    fn headless_backspace(&mut self, id: &str) {
        let (line, cur) = self.m.headless_buf(id);
        if cur == 0 {
            return;
        }
        let size = line[..cur]
            .chars()
            .next_back()
            .map(char::len_utf8)
            .unwrap_or(1);
        let new = format!("{}{}", &line[..cur - size], &line[cur..]);
        self.m.set_headless_buf(id, new, cur - size);
    }

    fn headless_move(&mut self, id: &str, dir: i64) {
        let (line, cur) = self.m.headless_buf(id);
        if dir < 0 && cur > 0 {
            let size = line[..cur]
                .chars()
                .next_back()
                .map(char::len_utf8)
                .unwrap_or(1);
            self.m.set_headless_buf(id, line, cur - size);
        } else if dir > 0 && cur < line.len() {
            let size = line[cur..].chars().next().map(char::len_utf8).unwrap_or(1);
            self.m.set_headless_buf(id, line, cur + size);
        }
    }

    fn send_headless_line(&mut self, id: &str) -> Result<(), FloorErr> {
        let (text, _) = self.m.headless_buf(id);
        if text.is_empty() {
            return Ok(());
        }
        if let Err(e) = call(
            &self.o.socket,
            "agent.prompt",
            params(vec![("pane", json!(id)), ("text", json!(text))]),
        ) {
            return self.fail(e);
        }
        self.m.set_headless_buf(id, String::new(), 0);
        Ok(())
    }

    /// A key byte that is already waiting, without blocking: (byte, more,
    /// open).
    pub fn next_key(&mut self) -> (u8, bool, bool) {
        if let Some(b) = self.replay.pop_front() {
            return (b, true, true);
        }
        match self.keys.try_recv() {
            Ok(Some(b)) => (b, true, true),
            Ok(None) | Err(TryRecvError::Disconnected) => (0, false, false),
            Err(TryRecvError::Empty) => (0, false, true),
        }
    }

    /// Whether another byte follows within PASTE_WAIT. A peek, not a take.
    fn more_waiting(&mut self) -> bool {
        match self.get(PASTE_WAIT) {
            Some(Some(b)) => {
                self.replay.push_front(b);
                true
            }
            _ => false,
        }
    }

    fn flush_hold(&mut self) -> Result<(), FloorErr> {
        self.hold_at = None;
        let Some(kr) = &mut self.kr else {
            return Ok(());
        };
        let (act, out) = kr.flush();
        self.typed_action(act, &out)?;
        self.pump_text()
    }

    fn typed_action(&mut self, act: attach::Action, out: &[u8]) -> Result<(), FloorErr> {
        match act {
            attach::Action::Detach => self.set_typing(""),
            attach::Action::Mouse => {
                if let Some(c) = parse_sgr(out) {
                    return self.click(c);
                }
            }
            attach::Action::None => {
                if !self.m.typing.is_empty() && !out.is_empty() {
                    let pane = self.m.typing.clone();
                    self.queue_text(&pane, out);
                }
            }
        }
        Ok(())
    }

    pub fn queue_text(&mut self, pane: &str, b: &[u8]) {
        if let Some(last) = self.text_q.last_mut() {
            if last.pane == pane {
                last.b.extend_from_slice(b);
                return;
            }
        }
        self.text_q.push(Typed {
            pane: pane.to_string(),
            b: b.to_vec(),
        });
    }

    /// Sends the text at the head of the queue when no text waits for a
    /// reply.
    pub fn pump_text(&mut self) -> Result<(), FloorErr> {
        while !self.text_busy && !self.text_q.is_empty() {
            let head_pane = self.text_q[0].pane.clone();
            let (text, rest) = whole_runes(&self.text_q[0].b);
            let kept = !rest.is_empty() && self.text_q.len() == 1;
            if kept {
                self.text_q[0].b = rest;
            } else {
                self.text_q.remove(0);
            }
            if text.is_empty() {
                if kept {
                    return Ok(());
                }
                continue;
            }
            self.send(json!({
                "id": format!("vt-{head_pane}"), "cmd": "pane.send_text", "pane": head_pane,
                "text": text, "enter": false,
            }))?;
            self.text_busy = true;
        }
        Ok(())
    }

    pub fn detach_all(&mut self) {
        let mut ids: Vec<String> = self.m.screens.keys().cloned().collect();
        ids.sort();
        for id in ids {
            let _ = self.detach_tile(&id);
        }
    }

    fn apply_frame(&mut self, ev: &Obj) -> bool {
        let pane = s(ev, "pane");
        let Some(sc) = self.m.screens.get_mut(&pane) else {
            return false;
        };
        let Some(fr) = attach::parse_frame(ev) else {
            return false;
        };
        sc.apply(&fr);
        true
    }

    /// Fills the windows once, when the floor opens.
    fn start_fill(&mut self) {
        let n = window_count(self.cols());
        if n == 0 || self.m.tiles.is_none() {
            return;
        }
        self.fill_empty();
        let mut target = String::new();
        for r in self.m.tree_rows() {
            if r.needs_you() {
                if let Some(slot) = self.m.tiles.as_ref().and_then(|t| t.slot_of(&r.id)) {
                    if (slot as i64) < n {
                        target = r.id.clone();
                    }
                }
                break;
            }
        }
        if target.is_empty() {
            if let Some(first) = self
                .m
                .tiles
                .as_ref()
                .and_then(|t| t.shown(n).first().cloned())
            {
                target = first;
            }
        }
        if target.is_empty() {
            return;
        }
        if let Some(ts) = &mut self.m.tiles {
            ts.fill(&target);
        }
        self.m.cursor = self.m.index_of(&target, self.m.cursor);
        self.set_typing(&target);
    }

    /// Puts the agents in no window into the empty windows, in start order.
    pub fn fill_empty(&mut self) {
        let n = window_count(self.cols());
        if n == 0 || self.m.tiles.is_none() {
            return;
        }
        self.grow_slots(n);
        for id in self.m.start_order() {
            if self.m.tiles.as_ref().and_then(|t| t.slot_of(&id)).is_some() {
                continue;
            }
            if !self.put_in_empty(&id, n) {
                return;
            }
        }
    }

    fn put_in_empty(&mut self, id: &str, n: i64) -> bool {
        let Some(ts) = &mut self.m.tiles else {
            return false;
        };
        let mut i = 0usize;
        while (i as i64) < n && i < ts.slots.len() {
            if ts.slots[i].is_empty() {
                ts.slots[i] = id.to_string();
                return true;
            }
            i += 1;
        }
        if ts.layout == Layout::All && (ts.slots.len() as i64) < n {
            ts.slots.push(id.to_string());
            return true;
        }
        false
    }

    /// Gives a focus layout at least n slots.
    pub fn grow_slots(&mut self, n: i64) {
        let Some(ts) = &mut self.m.tiles else { return };
        if ts.layout != Layout::Focus {
            return;
        }
        while (ts.slots.len() as i64) < n {
            ts.slots.push(String::new());
        }
    }

    /// Takes one key byte. Ok(true) when the run should end.
    fn key(&mut self, b: u8) -> Result<bool, FloorErr> {
        if self.sp.recording || self.fenced() {
            return self.speak_key(b);
        }
        if !self.paused() && b == self.m.talk_key() {
            return self.speak_key(b);
        }
        if self.hearing == 0 && !self.checking {
            self.m.voice.clear();
        }
        if self.paused() {
            let (_, closed) = read_key_from(self, b);
            return Ok(closed);
        }
        self.m.key_pressed();
        if !self.m.typing.is_empty() {
            let closed = if self.m.is_headless(&self.m.typing.clone()) {
                self.headless_keys(b)?
            } else {
                self.type_keys(b)?
            };
            self.render()?;
            return Ok(closed);
        }
        let (ks, closed) = read_key_from(self, b);
        for k in ks {
            if self.handle(k)? {
                return Ok(true);
            }
            self.render()?;
        }
        Ok(closed)
    }

    /// Sorts an error from a call: a server that is gone ends the floor,
    /// anything else is the message line.
    pub fn fail(&mut self, err: FloorErr) -> Result<(), FloorErr> {
        match err {
            FloorErr::Gone(_) => Err(err),
            FloorErr::Other(msg) => {
                self.m.message = msg;
                Ok(())
            }
        }
    }

    /// Replaces the rows with a fresh pane.list, then the tasks.
    pub fn refresh(&mut self) -> Result<(), FloorErr> {
        let list = match call(&self.o.socket, "pane.list", None) {
            Ok(r) => list_of(&r, "panes"),
            Err(e) => return self.fail(e),
        };
        let was: BTreeSet<String> = self
            .m
            .rows
            .iter()
            .filter(|r| r.parent.is_empty())
            .map(|r| r.id.clone())
            .collect();
        let tasks = match call(&self.o.socket, "task.list", None) {
            Ok(r) => list_of(&r, "tasks"),
            Err(e) => {
                self.m.tasks.clear();
                self.m.apply(&list, now_seconds());
                return self.fail(e);
            }
        };
        let keep = self.m.selected_key();
        self.m.tasks = decode_tasks(&tasks);
        self.m.apply(&list, now_seconds());
        if self.ended_wanted(&was, Instant::now()) {
            self.refresh_ended(&was)?;
        }
        if was.contains(&keep) && !self.m.has(&keep) {
            self.m.cursor_to_ended(&keep);
        }
        self.fill_empty();
        Ok(())
    }

    fn ended_wanted(&mut self, was: &BTreeSet<String>, now: Instant) -> bool {
        let old = match self.ended_read {
            Some(t) => now.saturating_duration_since(t) >= ENDED_EVERY,
            None => true,
        };
        let mut want = self.ended_due || self.m.recent_open || old;
        for id in was {
            if !self.m.has(id) {
                want = true;
            }
        }
        if want {
            self.ended_due = false;
            self.ended_read = Some(now);
        }
        want
    }

    fn refresh_ended(&mut self, was: &BTreeSet<String>) -> Result<(), FloorErr> {
        let res = match call(
            &self.o.socket,
            "pane.list",
            params(vec![("ended", json!(true))]),
        ) {
            Ok(r) => r,
            Err(FloorErr::Other(m)) if m.starts_with("no command ") => return Ok(()),
            Err(e) => return self.fail(e),
        };
        let list = list_of(&res, "panes");
        self.m.apply_ended(&list, now_seconds());
        let ended = self.m.ended.clone();
        for r in &ended {
            if was.contains(&r.id) && !self.m.has(&r.id) {
                self.m.note_ended(r);
            }
        }
        Ok(())
    }

    /// Draws the floor, then brings the tile attaches in line.
    pub fn render(&mut self) -> Result<(), FloorErr> {
        let (cols, rows) = self.size();
        let n = window_count(cols);
        self.grow_slots(n);
        self.keep_typing(n);
        let lines = render(&mut self.m, cols, rows);
        let (x, y, hidden) = (self.m.cursor_x, self.m.cursor_y, self.m.cursor_hidden);
        if let Some(b) = self.fw.frame(lines, cols, x, y, hidden) {
            if let Err(e) = self.o.out.write_all(b.as_bytes()) {
                return Err(FloorErr::Other(e.to_string()));
            }
            let _ = self.o.out.flush();
        }
        self.sync_tiles()
    }

    /// Asks floor.facts for the header row, on a screen wide enough.
    pub fn read_facts(&mut self) {
        if self.cols() < FACTS_MIN_COLS.min(hint_min_cols()) {
            return;
        }
        if let Ok(res) = call(&self.o.socket, "floor.facts", None) {
            if let Some(fa) = decode_facts(&res) {
                self.m.facts = Some(fa);
            }
        }
    }

    /// Fills the notes from floor.notes.
    fn seed_notes(&mut self) {
        if let Ok(res) = call(&self.o.socket, "floor.notes", None) {
            for n in list_of(&res, "notes") {
                self.m.add_note(&s(&n, "text"));
            }
        }
    }

    /// Opens the peek on the row under the cursor, read fresh.
    pub fn peek(&mut self) -> Result<(), FloorErr> {
        let Some(r) = self.m.selected() else {
            return Ok(());
        };
        if !r.parent.is_empty() {
            self.m.message = super::model::CHILD_MESSAGE.into();
            return Ok(());
        }
        let res = match call(
            &self.o.socket,
            "pane.read",
            params(vec![("pane", json!(r.id)), ("source", json!("visible"))]),
        ) {
            Ok(r) => r,
            Err(e) => return self.fail(e),
        };
        let text = s(&res, "text");
        let list = match call(&self.o.socket, "pane.list", None) {
            Ok(r) => list_of(&r, "panes"),
            Err(e) => return self.fail(e),
        };
        let mut ask: Option<Obj> = None;
        for p in &list {
            if s(p, "id") == r.id {
                ask = ask_of(p).cloned();
            }
        }
        self.m.apply(&list, now_seconds());
        match self.m.selected() {
            Some(cur) if cur.id == r.id => {}
            _ => {
                self.m.message = format!("pane {} is gone", r.id);
                return Ok(());
            }
        }
        self.m.open_peek(&text, ask.as_ref());
        Ok(())
    }

    /// Hands the terminal to attach for pane id until the leave key brings
    /// it back.
    pub fn attach_to(&mut self, id: &str) -> Result<(), FloorErr> {
        if self.m.is_headless(id) {
            return self.attach_headless(id);
        }
        let attached = id.to_string();
        self.set_typing("");
        self.detach_all();
        self.write_out(CURSOR_SHOW);
        self.fw.last_hidden = false;
        if self.o.tty {
            self.write_out(MOUSE_OFF);
        }
        let (cols, rows) = self.size();
        let rx = std::mem::replace(&mut self.lines, mpsc::channel().1);
        let stop = std::sync::Arc::new(AtomicBool::new(false));
        let stop2 = stop.clone();
        let drain = thread::spawn(move || {
            let mut replied: Vec<String> = Vec::new();
            loop {
                if stop2.load(Ordering::SeqCst) {
                    return (rx, replied, false);
                }
                match rx.recv_timeout(Duration::from_millis(20)) {
                    Ok(LineRes::Line(l)) => {
                        if let Ok(Value::Object(ev)) = serde_json::from_slice::<Value>(&l) {
                            let id = s(&ev, "id");
                            if id.starts_with("vd-") || id.starts_with("vt-") {
                                replied.push(id);
                            }
                        }
                    }
                    Ok(LineRes::Err) | Err(RecvTimeoutError::Disconnected) => {
                        return (rx, replied, true)
                    }
                    Err(RecvTimeoutError::Timeout) => {}
                }
            }
        });
        let err = attach::run(attach::Options {
            socket: self.o.socket.clone(),
            pane: id.to_string(),
            cols,
            rows,
            leave: self.o.leave,
            keys: Some(self.keys),
            end_on_exit: true,
        });
        stop.store(true, Ordering::SeqCst);
        let (rx, replied, lost) = drain
            .join()
            .unwrap_or_else(|_| (mpsc::channel().1, vec![], true));
        self.lines = rx;
        for rid in replied {
            if rid.starts_with("vt-") {
                self.text_busy = false;
                continue;
            }
            self.detaching.remove(&rid[3..]);
        }
        let mut ended_here = false;
        let mut result: Result<(), FloorErr> = Ok(());
        match err {
            Ok(()) => {}
            Err(attach::Error::PaneEnded) => ended_here = true,
            Err(attach::Error::ServerGone(s)) => result = Err(FloorErr::Gone(s)),
            Err(attach::Error::Other(s)) => self.m.message = s,
        }
        let finish = |f: &mut Self| {
            f.fw.clear = true;
            if f.o.tty {
                f.write_out(MOUSE_ON);
            }
        };
        if lost {
            if ended_here {
                self.after_ended(&attached);
            }
            finish(self);
            return Err(FloorErr::Gone(format!(
                "{SERVER_GONE}: the event connection closed"
            )));
        }
        if let Err(e) = result {
            if ended_here {
                self.after_ended(&attached);
            }
            finish(self);
            return Err(e);
        }
        let mut out = self.pump_text();
        if out.is_ok() {
            out = self.refresh();
        }
        if ended_here {
            self.after_ended(&attached);
        }
        finish(self);
        out
    }

    fn after_ended(&mut self, id: &str) {
        let name = self.m.name_of(id);
        self.pause_keys(&format!("{name} ended. Keys paused."));
        self.m.cursor_to_ended(id);
    }

    /// Stands in for attach_to on a headless pane: it goes into a window.
    fn attach_headless(&mut self, id: &str) -> Result<(), FloorErr> {
        if self.type_in_tile(id) {
            return Ok(());
        }
        self.m.message = format!(
            "{} takes whole messages. Widen the terminal, or run: coppice agent prompt {id} \"...\"",
            self.m.label_of(id)
        );
        Ok(())
    }
}

/// The message of a reply with ok false, or "".
pub fn reply_error(ev: &Obj) -> String {
    match ev.get("ok") {
        Some(Value::Bool(false)) => {}
        _ => return String::new(),
    }
    if let Some(Value::Object(e)) = ev.get("error") {
        let msg = s(e, "message");
        if !msg.is_empty() {
            return msg;
        }
    }
    "the server refused with no message".into()
}

/// Whether a state event says a pane's process or headless session is
/// gone.
pub fn ended(ev: &Obj) -> bool {
    if s(ev, "state") != "done" {
        return false;
    }
    let src = s(ev, "source");
    src == "process" || src == "headless"
}

/// Go's utf8.DecodeRune on bytes that FullRune called whole: the size of
/// a valid rune, or None for a bad lead byte.
fn decode_rune(b: &[u8]) -> Option<usize> {
    for size in 1..=4.min(b.len()) {
        if let Ok(s) = std::str::from_utf8(&b[..size]) {
            if s.chars().count() == 1 {
                return Some(size);
            }
        }
    }
    None
}

/// Splits b into the text of its whole runes and the bytes of a rune not
/// yet complete at its end. A byte that can never start a valid rune is
/// dropped.
pub fn whole_runes(b: &[u8]) -> (String, Vec<u8>) {
    let mut out: Vec<u8> = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if !attach::full_rune(&b[i..]) {
            return (String::from_utf8_lossy(&out).into_owned(), b[i..].to_vec());
        }
        match decode_rune(&b[i..]) {
            None => i += 1,
            Some(size) => {
                out.extend_from_slice(&b[i..i + size]);
                i += size;
            }
        }
    }
    (String::from_utf8_lossy(&out).into_owned(), Vec::new())
}
