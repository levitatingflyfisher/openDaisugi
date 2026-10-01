//! coppice attach: one pane on the whole terminal, a status line under it,
//! and one key that leaves. Run dials the socket, sends pane.attach,
//! renders every frame and state event that arrives, and turns keystrokes
//! into forwarded text or the detach.

use std::io::{self, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender};
use std::sync::{Arc, Mutex, OnceLock};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Map, Value};

use crate::config;
use crate::textwidth;

/// How long a pressed leave key waits for a second press.
const LEAVE_HOLD: Duration = Duration::from_millis(300);
/// How long a held ESC waits for the rest of a mouse report.
const ESC_HOLD: Duration = Duration::from_millis(50);
/// The most bytes a mouse report may hold.
const MOUSE_REPORT_MAX: usize = 32;
/// How long Run waits for the server to take pane.attach.
const ATTACH_ACK_TIMEOUT: Duration = Duration::from_secs(5);
/// The one pane.list call for the pane's label.
const LABEL_FETCH_TIMEOUT: Duration = Duration::from_millis(500);
/// Bounds every send on the main connection.
const TEARDOWN_DEADLINE: Duration = Duration::from_secs(2);

#[derive(Debug, PartialEq, Clone, Copy)]
pub enum Action {
    None,
    Detach,
    Mouse,
}

/// Turns a byte stream into actions and the bytes to forward. It takes the
/// leave key for itself and drops whole mouse reports; every other byte
/// belongs to the harness.
pub struct KeyReader {
    pub report_mouse: bool,
    pub drop_reports: bool,
    leave: u8,
    armed: bool,
    held: Vec<u8>,
}

impl KeyReader {
    pub fn new(leave: u8) -> KeyReader {
        KeyReader {
            report_mouse: false,
            drop_reports: false,
            leave,
            armed: false,
            held: Vec::new(),
        }
    }

    pub fn pending(&self) -> bool {
        self.armed || !self.held.is_empty()
    }

    /// How long to wait before flush.
    pub fn hold(&self) -> Option<Duration> {
        if self.armed {
            Some(LEAVE_HOLD)
        } else if !self.held.is_empty() {
            Some(ESC_HOLD)
        } else {
            None
        }
    }

    /// The bytes held for a possible mouse report, emptied. A caller that
    /// stops reading through this reader passes them on.
    pub fn held(&mut self) -> Vec<u8> {
        std::mem::take(&mut self.held)
    }

    /// Resolves the hold: a held leave key is a detach, held bytes forward.
    pub fn flush(&mut self) -> (Action, Vec<u8>) {
        if self.armed {
            self.armed = false;
            return (Action::Detach, Vec::new());
        }
        (Action::None, std::mem::take(&mut self.held))
    }

    fn feed_held(&mut self, b: u8) -> (Action, Vec<u8>) {
        self.held.push(b);
        let n = self.held.len();
        let csi = n >= 3 && self.held[2] != b'<';
        if (n == 2 && b == b'[') || (n == 3 && b == b'<') {
            return (Action::None, Vec::new());
        }
        if self.drop_reports
            && csi
            && n <= MOUSE_REPORT_MAX
            && (b == b';' || b == b':' || b.is_ascii_digit() || (n == 3 && b == b'?'))
        {
            return (Action::None, Vec::new());
        }
        if self.drop_reports && csi && n > 3 && (b == b'u' || (b == b'c' && self.held[2] == b'?')) {
            self.held.clear();
            return (Action::None, Vec::new());
        }
        if n > 3 && !csi && (b == b'M' || b == b'm') {
            let report = std::mem::take(&mut self.held);
            if self.report_mouse {
                return (Action::Mouse, report);
            }
            return (Action::None, Vec::new());
        }
        if n > 3 && n <= MOUSE_REPORT_MAX && (b == b';' || b.is_ascii_digit()) {
            return (Action::None, Vec::new());
        }
        let typed = self.held[..n - 1].to_vec();
        self.held.clear();
        let (act, out) = self.feed(b);
        let mut all = typed;
        all.extend(out);
        (act, all)
    }

    /// Takes one byte.
    pub fn feed(&mut self, b: u8) -> (Action, Vec<u8>) {
        if self.armed {
            self.armed = false;
            if b == self.leave {
                return (Action::None, vec![self.leave]);
            }
            if b == 0x1b {
                self.held = vec![b];
            }
            return (Action::Detach, Vec::new());
        }
        if !self.held.is_empty() {
            return self.feed_held(b);
        }
        if b == self.leave {
            self.armed = true;
            return (Action::None, Vec::new());
        }
        if b == 0x1b {
            self.held = vec![b];
            return (Action::None, Vec::new());
        }
        (Action::None, vec![b])
    }
}

/// Assembles the bytes of one keystroke into a whole rune before it goes.
#[derive(Default)]
struct Utf8Buf {
    pending: Vec<u8>,
}

/// Go's utf8.FullRune.
pub fn full_rune(p: &[u8]) -> bool {
    let Some(&b0) = p.first() else { return false };
    let need = match b0 {
        0x00..=0x7f => 1,
        0xc2..=0xdf => 2,
        0xe0..=0xef => 3,
        0xf0..=0xf4 => 4,
        _ => return true,
    };
    if p.len() >= need {
        return true;
    }
    // A byte that cannot continue the rune makes it whole (and invalid).
    let (lo, hi) = match b0 {
        0xe0 => (0xa0, 0xbf),
        0xed => (0x80, 0x9f),
        0xf0 => (0x90, 0xbf),
        0xf4 => (0x80, 0x8f),
        _ => (0x80, 0xbf),
    };
    if p.len() > 1 && !(lo..=hi).contains(&p[1]) {
        return true;
    }
    p.len() > 2 && p[2..].iter().any(|b| !(0x80..=0xbf).contains(b))
}

impl Utf8Buf {
    fn feed(&mut self, b: u8) -> Option<Vec<u8>> {
        if b < 0x80 {
            self.pending.clear();
            return Some(vec![b]);
        }
        if self.pending.len() >= 4 {
            self.pending.clear();
        }
        self.pending.push(b);
        if !full_rune(&self.pending) {
            return None;
        }
        let out = std::mem::take(&mut self.pending);
        match std::str::from_utf8(&out) {
            Ok(_) => Some(out),
            // A whole sequence that decodes as an error on its first byte
            // was never a keystroke.
            Err(e) if e.valid_up_to() == 0 => None,
            Err(_) => Some(out),
        }
    }
}

#[derive(Clone, Default, PartialEq)]
pub struct Cell {
    pub text: String,
    pub fg: String,
    pub bg: String,
    pub attrs: u16,
}

/// The client's copy of the pane grid, rebuilt from frames.
pub struct Screen {
    cols: usize,
    rows: usize,
    grid: Vec<Vec<Cell>>,
    cursor: [i64; 2],
    resized: bool,
}

/// One frame as Go reads it.
pub struct Frame {
    pub cols: i64,
    pub rows: i64,
    pub cursor: [i64; 2],
    pub rows_changed: Vec<(i64, Vec<Cell>)>,
}

fn parse_cell(v: &Value) -> Option<Cell> {
    let a = v.as_array()?;
    if a.len() != 4 {
        return None;
    }
    Some(Cell {
        text: a[0].as_str()?.to_string(),
        fg: a[1].as_str()?.to_string(),
        bg: a[2].as_str()?.to_string(),
        attrs: u16::try_from(a[3].as_u64()?).ok()?,
    })
}

/// Reads a frame event. None when Go's decode would fail.
pub fn parse_frame(m: &Map<String, Value>) -> Option<Frame> {
    let int = |k: &str| -> Option<i64> {
        match m.get(k) {
            None | Some(Value::Null) => Some(0),
            Some(v) => v.as_i64(),
        }
    };
    let cursor = match m.get("cursor") {
        None | Some(Value::Null) => [0, 0],
        Some(Value::Array(a)) => {
            let x = a.first().map(|v| v.as_i64()).unwrap_or(Some(0))?;
            let y = a.get(1).map(|v| v.as_i64()).unwrap_or(Some(0))?;
            [x, y]
        }
        _ => return None,
    };
    let mut rows_changed = Vec::new();
    match m.get("rows_changed") {
        None | Some(Value::Null) => {}
        Some(Value::Object(rc)) => {
            for (k, v) in rc {
                let y: i64 = k.parse().ok()?;
                let cells = match v {
                    Value::Null => Vec::new(),
                    Value::Array(a) => a.iter().map(parse_cell).collect::<Option<Vec<_>>>()?,
                    _ => return None,
                };
                rows_changed.push((y, cells));
            }
        }
        _ => return None,
    }
    Some(Frame {
        cols: int("cols")?,
        rows: int("rows")?,
        cursor,
        rows_changed,
    })
}

impl Screen {
    pub fn new(cols: i64, rows: i64) -> Screen {
        let mut s = Screen {
            cols: 0,
            rows: 0,
            grid: Vec::new(),
            cursor: [0, 0],
            resized: false,
        };
        s.set_size(cols, rows);
        s
    }

    fn set_size(&mut self, cols: i64, rows: i64) {
        self.cols = cols.max(0) as usize;
        self.rows = rows.max(0) as usize;
        self.grid = vec![vec![Cell::default(); self.cols]; self.rows];
    }

    pub fn apply(&mut self, f: &Frame) {
        if f.cols.max(0) as usize != self.cols
            || f.rows.max(0) as usize != self.rows
            || f.cols < 0
            || f.rows < 0
        {
            self.set_size(f.cols, f.rows);
            self.resized = true;
        }
        for (y, row) in &f.rows_changed {
            if *y < 0 || *y as usize >= self.rows {
                continue;
            }
            let line = &mut self.grid[*y as usize];
            for (x, cell) in line.iter_mut().enumerate() {
                *cell = row.get(x).cloned().unwrap_or_default();
            }
        }
        self.cursor = f.cursor;
    }

    fn row_text(row: &[Cell], b: &mut String) {
        let mut last = Cell::default();
        let mut skip_spacer = false;
        for c in row {
            if skip_spacer && c.text.is_empty() {
                skip_spacer = false;
                continue;
            }
            skip_spacer = false;
            if c.fg != last.fg || c.bg != last.bg || c.attrs != last.attrs {
                b.push_str(&sgr(c));
                last = c.clone();
            }
            if c.text.is_empty() {
                b.push(' ');
            } else {
                b.push_str(&c.text);
                if textwidth::is_wide(&c.text) {
                    skip_spacer = true;
                }
            }
        }
        b.push_str("\x1b[0m");
    }

    /// The whole screen with colour, then the cursor.
    pub fn render(&mut self) -> String {
        let mut b = String::new();
        if self.resized {
            b.push_str("\x1b[2J");
            self.resized = false;
        }
        b.push_str("\x1b[H");
        let n = self.grid.len();
        for (y, row) in self.grid.iter().enumerate() {
            b.push_str("\x1b[K");
            Self::row_text(row, &mut b);
            if y + 1 < n {
                b.push_str("\r\n");
            }
        }
        b.push_str(&format!(
            "\x1b[{};{}H",
            self.cursor[1] + 1,
            self.cursor[0] + 1
        ));
        b
    }

    /// The pane's own cursor from the last frame: column, then row.
    pub fn cursor(&self) -> [i64; 2] {
        self.cursor
    }

    /// One string per grid row, colored as render colors, each ending
    /// with the reset.
    pub fn lines(&self) -> Vec<String> {
        self.grid
            .iter()
            .map(|row| {
                let mut b = String::new();
                Self::row_text(row, &mut b);
                b
            })
            .collect()
    }

    /// The plain content.
    #[cfg(test)]
    pub fn text(&self) -> String {
        self.grid
            .iter()
            .map(|row| {
                let s: String = row
                    .iter()
                    .map(|c| {
                        if c.text.is_empty() {
                            " "
                        } else {
                            c.text.as_str()
                        }
                    })
                    .collect();
                s.trim_end_matches(' ').to_string()
            })
            .collect::<Vec<_>>()
            .join("\n")
    }
}

/// One cell's attribute bits and colours as SGR codes.
fn sgr(c: &Cell) -> String {
    let mut parts = vec!["0"];
    for (bit, code) in [
        (1, "1"),
        (2, "2"),
        (4, "3"),
        (8, "4"),
        (16, "5"),
        (32, "7"),
        (64, "9"),
    ] {
        if c.attrs & bit != 0 {
            parts.push(code);
        }
    }
    let mut out = format!("\x1b[{}m", parts.join(";"));
    if let Some([r, g, b]) = parse_hex(&c.fg) {
        out.push_str(&format!("\x1b[38;2;{r};{g};{b}m"));
    }
    if let Some([r, g, b]) = parse_hex(&c.bg) {
        out.push_str(&format!("\x1b[48;2;{r};{g};{b}m"));
    }
    out
}

/// Go's Sscanf("#%02x%02x%02x") on a 7-byte string.
fn parse_hex(s: &str) -> Option<[u8; 3]> {
    let b = s.as_bytes();
    if b.len() != 7 || b[0] != b'#' {
        return None;
    }
    let mut out = [0u8; 3];
    for (i, o) in out.iter_mut().enumerate() {
        let pair = std::str::from_utf8(&b[1 + 2 * i..3 + 2 * i]).ok()?;
        if !pair.bytes().all(|c| c.is_ascii_hexdigit()) {
            return None;
        }
        *o = u8::from_str_radix(pair, 16).ok()?;
    }
    Some(out)
}

/// Go's unicode.IsSpace.
fn is_space(r: char) -> bool {
    r.is_whitespace()
}

/// A label or a harness name as one status line field: each run of white
/// space one space, other control and unprintable runes dropped.
fn field(s: &str) -> String {
    let mut b = String::new();
    let mut space = false;
    for r in s.chars() {
        if is_space(r) {
            space = true;
            continue;
        }
        if r.is_control() || !crate::sys::is_print_rune(r) {
            continue;
        }
        if space && !b.is_empty() {
            b.push(' ');
        }
        space = false;
        b.push(r);
    }
    b
}

/// What the status line shows of a state event.
#[derive(Clone, Default)]
pub struct StateView {
    pub state: String,
    pub source: String,
    pub ask: Option<(String, String)>,
}

/// The one line under the pane: the pane, its label, its harness, its
/// merged state and the source of that state, then the leave key.
pub fn status_line(
    pane: &str,
    label: &str,
    harness: &str,
    ev: Option<&StateView>,
    cols: i64,
    leave: &str,
) -> String {
    let cols = cols.max(0) as usize;
    let (mut state, mut source, mut extra) =
        ("unknown".to_string(), "none".to_string(), String::new());
    if let Some(e) = ev {
        state = e.state.clone();
        source = e.source.clone();
        if let Some((tool, summary)) = &e.ask {
            extra = format!(" {tool}");
            if !summary.is_empty() {
                extra.push_str(&format!(": {summary}"));
            }
        }
    }
    let (label, harness) = (field(label), field(harness));
    let mut parts = vec![pane.to_string()];
    if !label.is_empty() {
        parts.push(label);
    }
    if !harness.is_empty() {
        parts.push(harness);
    }
    parts.push(format!("{state} via {source}"));
    let mut line = parts.join("  ") + &extra;
    line.push_str(&format!("   {leave} leave"));
    let r: Vec<char> = line.chars().collect();
    if r.len() > cols {
        if cols <= 1 {
            return r[..cols].iter().collect();
        }
        line = r[..cols - 1].iter().collect::<String>() + "…";
    }
    line
}

#[derive(Debug)]
pub enum Error {
    /// The connection dropped, or never came up.
    ServerGone(String),
    /// With end_on_exit, the pane's process or session ended.
    PaneEnded,
    Other(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::ServerGone(s) | Error::Other(s) => f.write_str(s),
            Error::PaneEnded => f.write_str("the pane ended"),
        }
    }
}

const SERVER_GONE: &str = "the server went away";

pub struct Options<'k> {
    pub socket: PathBuf,
    pub pane: String,
    pub cols: i64,
    pub rows: i64,
    pub leave: u8,
    /// The key channel to read, when the caller owns the one reader on
    /// stdin. None takes the process's own reader.
    pub keys: Option<&'k Receiver<Option<u8>>>,
    /// Detach and return PaneEnded once the pane's process or headless
    /// session ends.
    pub end_on_exit: bool,
}

/// The raw mode of stdin, put back when dropped.
pub struct Raw(Option<libc::termios>);

impl Raw {
    pub fn on() -> Raw {
        // SAFETY: tcgetattr and tcsetattr read and write one termios.
        unsafe {
            if libc::isatty(0) != 1 {
                return Raw(None);
            }
            let mut t: libc::termios = std::mem::zeroed();
            if libc::tcgetattr(0, &mut t) != 0 {
                return Raw(None);
            }
            let old = t;
            // x/term's MakeRaw.
            t.c_iflag &= !(libc::IGNBRK
                | libc::BRKINT
                | libc::PARMRK
                | libc::ISTRIP
                | libc::INLCR
                | libc::IGNCR
                | libc::ICRNL
                | libc::IXON);
            t.c_oflag &= !libc::OPOST;
            t.c_lflag &= !(libc::ECHO | libc::ECHONL | libc::ICANON | libc::ISIG | libc::IEXTEN);
            t.c_cflag &= !(libc::CSIZE | libc::PARENB);
            t.c_cflag |= libc::CS8;
            t.c_cc[libc::VMIN] = 1;
            t.c_cc[libc::VTIME] = 0;
            libc::tcsetattr(0, libc::TCSANOW, &t);
            Raw(Some(old))
        }
    }
}

impl Drop for Raw {
    fn drop(&mut self) {
        if let Some(t) = self.0 {
            // SAFETY: as in on.
            unsafe {
                libc::tcsetattr(0, libc::TCSANOW, &t);
            }
        }
    }
}

/// The one reader on stdin, for the whole process: a byte at a time, then
/// None when stdin ends.
pub fn keys() -> &'static Mutex<Receiver<Option<u8>>> {
    static KEYS: OnceLock<Mutex<Receiver<Option<u8>>>> = OnceLock::new();
    KEYS.get_or_init(|| {
        let (tx, rx) = mpsc::channel();
        thread::spawn(move || {
            let mut stdin = io::stdin();
            let mut buf = [0u8; 1];
            loop {
                match stdin.read(&mut buf) {
                    Ok(1) => {
                        if tx.send(Some(buf[0])).is_err() {
                            return;
                        }
                    }
                    _ => {
                        let _ = tx.send(None);
                        return;
                    }
                }
            }
        });
        Mutex::new(rx)
    })
}

fn write_out(s: &str) {
    let mut o = io::stdout().lock();
    let _ = o.write_all(s.as_bytes());
    let _ = o.flush();
}

fn send(conn: &UnixStream, v: Value) {
    let _ = conn.set_write_timeout(Some(TEARDOWN_DEADLINE));
    let mut line = crate::cli::gofmt::marshal(&v);
    line.push('\n');
    let _ = (&*conn).write_all(line.as_bytes());
}

fn fetch_label(socket: PathBuf, pane: String) -> Option<String> {
    let c = UnixStream::connect(&socket).ok()?;
    let _ = c.set_read_timeout(Some(LABEL_FETCH_TIMEOUT));
    let _ = c.set_write_timeout(Some(LABEL_FETCH_TIMEOUT));
    (&c).write_all(b"{\"cmd\":\"pane.list\",\"id\":\"lbl\"}\n")
        .ok()?;
    let mut dec = crate::proto::Decoder::new(io::BufReader::new(c));
    let line = dec.next_line().ok()?;
    let v: Value = serde_json::from_slice(&line).ok()?;
    for p in v.get("result")?.get("panes")?.as_array()? {
        if p.get("id").and_then(Value::as_str) == Some(pane.as_str()) {
            return Some(
                p.get("label")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string(),
            );
        }
    }
    None
}

fn now_seconds() -> f64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs_f64())
        .unwrap_or(0.0)
}

/// The state event fields the status line reads, after the ask deadline:
/// a blocked event whose ask is past its deadline shows working.
fn effective(ev: &Map<String, Value>, now: f64) -> StateView {
    let s = |k: &str| ev.get(k).and_then(Value::as_str).unwrap_or("").to_string();
    let mut v = StateView {
        state: s("state"),
        source: s("source"),
        ask: None,
    };
    if let Some(Value::Object(a)) = ev.get("ask") {
        let deadline = a.get("deadline").and_then(Value::as_f64).unwrap_or(0.0);
        if v.state == "blocked" && now >= deadline {
            v.state = "working".into();
            return v;
        }
        let g = |k: &str| a.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        v.ask = Some((g("tool"), g("summary")));
    }
    v
}

enum Ev {
    Line(Vec<u8>),
    LineErr,
}

/// Attaches to one pane and renders it until the user leaves or the
/// server goes away.
pub fn run(o: Options) -> Result<(), Error> {
    match o.keys {
        Some(k) => run_with(&o, k),
        None => {
            let keys = keys().lock().unwrap_or_else(|e| e.into_inner());
            run_with(&o, &keys)
        }
    }
}

fn run_with(o: &Options, keys: &Receiver<Option<u8>>) -> Result<(), Error> {
    let Ok(conn) = UnixStream::connect(&o.socket) else {
        return Err(Error::ServerGone(format!(
            "{SERVER_GONE}: cannot reach the server at {}. Run: coppice server start",
            o.socket.display()
        )));
    };
    let _raw = Raw::on();
    let pane_rows = (o.rows - 1).max(0);
    let _ = conn.set_read_timeout(Some(ATTACH_ACK_TIMEOUT));
    send(
        &conn,
        json!({"id": "attach", "cmd": "pane.attach", "pane": o.pane, "cols": o.cols, "rows": pane_rows}),
    );
    let mut screen = Screen::new(o.cols, pane_rows);
    let mut last_state: Option<Map<String, Value>> = None;
    let mut harness = String::new();
    let label = Arc::new(Mutex::new(String::new()));
    {
        let (l, sock, pane) = (label.clone(), o.socket.clone(), o.pane.clone());
        thread::spawn(move || {
            if let Some(x) = fetch_label(sock, pane) {
                if !x.is_empty() {
                    *l.lock().unwrap_or_else(|e| e.into_inner()) = x;
                }
            }
        });
    }
    let we_closed = Arc::new(AtomicBool::new(false));
    let (tx, rx): (Sender<Ev>, Receiver<Ev>) = mpsc::channel();
    {
        let r = conn.try_clone().map_err(|e| Error::Other(e.to_string()))?;
        thread::spawn(move || {
            let mut dec = crate::proto::Decoder::new(io::BufReader::new(r));
            loop {
                match dec.next_line() {
                    Ok(l) => {
                        if tx.send(Ev::Line(l)).is_err() {
                            return;
                        }
                    }
                    Err(_) => {
                        let _ = tx.send(Ev::LineErr);
                        return;
                    }
                }
            }
        });
    }
    let leave = o.leave;
    let leave_name = config::key_name(leave);
    let mut k = KeyReader::new(leave);
    let mut ub = Utf8Buf::default();
    let mut text_q: Vec<u8> = Vec::new();
    let mut text_busy = false;
    let mut hold_until: Option<Instant> = None;
    let mut next_tick = Instant::now() + Duration::from_secs(1);

    let status = |screen: &Screen, last: &Option<Map<String, Value>>, harness: &str| -> String {
        let eff = last.as_ref().map(|e| effective(e, now_seconds()));
        let l = label.lock().unwrap_or_else(|e| e.into_inner()).clone();
        status_line(
            &o.pane,
            &l,
            harness,
            eff.as_ref(),
            screen.cols as i64,
            &leave_name,
        )
    };

    loop {
        // Lines first: they are what the screen shows.
        let mut progressed = false;
        loop {
            match rx.try_recv() {
                Ok(Ev::Line(raw)) => {
                    progressed = true;
                    let Ok(v) = serde_json::from_slice::<Value>(&raw) else {
                        continue;
                    };
                    let Some(m) = v.as_object() else { continue };
                    if let Some(okv) = m.get("ok") {
                        let Some(ok) = okv.as_bool() else { continue };
                        let id = m.get("id").and_then(Value::as_str).unwrap_or("");
                        if id == "t" {
                            text_busy = false;
                            pump_text(&conn, &o.pane, &mut text_q, &mut text_busy);
                        }
                        if !ok {
                            if let Some(Value::Object(e)) = m.get("error") {
                                let g = |k: &str| {
                                    e.get(k).and_then(Value::as_str).unwrap_or("").to_string()
                                };
                                let msg = format!("{}: {}", g("code"), g("message"));
                                if id == "attach" {
                                    return Err(Error::Other(msg));
                                }
                                write_message(&mut screen, &msg);
                                continue;
                            }
                        }
                        if ok && id == "attach" {
                            let _ = conn.set_read_timeout(None);
                        }
                        continue;
                    }
                    match m.get("event").and_then(Value::as_str).unwrap_or("") {
                        "frame" => {
                            let Some(f) = parse_frame(m) else { continue };
                            screen.apply(&f);
                            let mut s = screen.render();
                            s.push_str(&format!(
                                "\x1b[{};1H\x1b[7m\x1b[K{}\x1b[0m",
                                screen.rows + 1,
                                status(&screen, &last_state, &harness)
                            ));
                            s.push_str(&format!(
                                "\x1b[{};{}H",
                                screen.cursor[1] + 1,
                                screen.cursor[0] + 1
                            ));
                            write_out(&s);
                        }
                        "state" => {
                            if m.get("pane").and_then(Value::as_str) != Some(o.pane.as_str()) {
                                continue;
                            }
                            if let Some(h) = m.get("harness").and_then(Value::as_str) {
                                if !h.is_empty() {
                                    harness = h.to_string();
                                }
                            }
                            last_state = Some(m.clone());
                            let src = m.get("source").and_then(Value::as_str).unwrap_or("");
                            if o.end_on_exit
                                && m.get("state").and_then(Value::as_str) == Some("done")
                                && (src == "process" || src == "headless")
                            {
                                we_closed.store(true, Ordering::SeqCst);
                                send(
                                    &conn,
                                    json!({"id": "d", "cmd": "pane.detach", "pane": o.pane}),
                                );
                                let _ = conn.shutdown(std::net::Shutdown::Both);
                                return Err(Error::PaneEnded);
                            }
                            repaint(&screen, &status(&screen, &last_state, &harness));
                        }
                        _ => {}
                    }
                }
                Ok(Ev::LineErr) => {
                    if we_closed.load(Ordering::SeqCst) {
                        return Ok(());
                    }
                    return Err(Error::ServerGone(SERVER_GONE.into()));
                }
                Err(_) => break,
            }
        }
        if progressed {
            continue;
        }
        let now = Instant::now();
        if now >= next_tick {
            next_tick = now + Duration::from_secs(1);
            if let Some(e) = &last_state {
                if e.get("state").and_then(Value::as_str) == Some("blocked")
                    && matches!(e.get("ask"), Some(Value::Object(_)))
                {
                    repaint(&screen, &status(&screen, &last_state, &harness));
                }
            }
        }
        if let Some(t) = hold_until {
            if now >= t {
                hold_until = None;
                let (a, out) = k.flush();
                if !act(
                    a,
                    &out,
                    &conn,
                    &o.pane,
                    &we_closed,
                    &mut ub,
                    &mut text_q,
                    &mut text_busy,
                ) {
                    // The detach closed the connection; the reader ends.
                }
                continue;
            }
        }
        let mut wait = next_tick
            .saturating_duration_since(now)
            .min(Duration::from_millis(20));
        if let Some(t) = hold_until {
            wait = wait.min(t.saturating_duration_since(now));
        }
        if we_closed.load(Ordering::SeqCst) {
            // Wait for the reader to see the close.
            match rx.recv_timeout(Duration::from_secs(3)) {
                Ok(Ev::LineErr) | Err(_) => return Ok(()),
                Ok(Ev::Line(_)) => continue,
            }
        }
        match keys.recv_timeout(wait) {
            Ok(Some(b)) => {
                let (a, out) = k.feed(b);
                act(
                    a,
                    &out,
                    &conn,
                    &o.pane,
                    &we_closed,
                    &mut ub,
                    &mut text_q,
                    &mut text_busy,
                );
                hold_until = k.hold().map(|d| Instant::now() + d);
            }
            Ok(None) => {
                // No more keyboard: detach on the pane's behalf.
                we_closed.store(true, Ordering::SeqCst);
                send(
                    &conn,
                    json!({"id": "eof", "cmd": "pane.detach", "pane": o.pane}),
                );
                write_out(&format!(
                    "\x1b[{};1H\x1b[7m\x1b[K{}\x1b[0m",
                    o.rows, "stdin closed. Detached."
                ));
                let _ = conn.shutdown(std::net::Shutdown::Both);
            }
            Err(RecvTimeoutError::Timeout) => {}
            Err(RecvTimeoutError::Disconnected) => {
                thread::sleep(wait);
            }
        }
    }
}

#[allow(clippy::too_many_arguments)]
fn act(
    a: Action,
    out: &[u8],
    conn: &UnixStream,
    pane: &str,
    we_closed: &AtomicBool,
    ub: &mut Utf8Buf,
    text_q: &mut Vec<u8>,
    text_busy: &mut bool,
) -> bool {
    match a {
        Action::Detach => {
            we_closed.store(true, Ordering::SeqCst);
            send(conn, json!({"id": "d", "cmd": "pane.detach", "pane": pane}));
            let _ = conn.shutdown(std::net::Shutdown::Both);
            false
        }
        Action::Mouse => true,
        Action::None => {
            for b in out {
                if let Some(chunk) = ub.feed(*b) {
                    text_q.extend(chunk);
                }
            }
            pump_text(conn, pane, text_q, text_busy);
            true
        }
    }
}

/// Sends everything queued as one pane.send_text when no text waits for a
/// reply.
fn pump_text(conn: &UnixStream, pane: &str, q: &mut Vec<u8>, busy: &mut bool) {
    if *busy || q.is_empty() {
        return;
    }
    let text = String::from_utf8_lossy(q).into_owned();
    send(
        conn,
        json!({"id": "t", "cmd": "pane.send_text", "pane": pane, "text": text, "enter": false}),
    );
    q.clear();
    *busy = true;
}

fn repaint(screen: &Screen, status: &str) {
    write_out(&format!(
        "\x1b7\x1b[{};1H\x1b[7m\x1b[K{}\x1b[0m\x1b8",
        screen.rows + 1,
        status
    ));
}

/// Paints one line on the row above the status row, with no newline, and
/// marks the screen for a full clear on the next frame.
fn write_message(s: &mut Screen, msg: &str) {
    if s.rows == 0 || s.cols == 0 {
        return;
    }
    let top = s.rows;
    let r: Vec<char> = msg.chars().collect();
    let ln: String = if r.len() > s.cols {
        r[..s.cols].iter().collect()
    } else {
        msg.to_string()
    };
    write_out(&format!("\x1b7\x1b[{top};1H\x1b[K{ln}\x1b8"));
    s.resized = true;
}

/// The pipe a SIGWINCH handler writes one byte to.
static WINCH_FD: AtomicI32 = AtomicI32::new(-1);

extern "C" fn on_winch(_: libc::c_int) {
    let fd = WINCH_FD.load(Ordering::SeqCst);
    if fd >= 0 {
        // SAFETY: write is async-signal-safe.
        unsafe {
            libc::write(fd, b"w".as_ptr() as *const libc::c_void, 1);
        }
    }
}

/// Sends pane.resize on every SIGWINCH, over its own short connection.
pub struct ResizeWatch {
    stop_fd: i32,
    handle: Option<thread::JoinHandle<()>>,
}

impl ResizeWatch {
    pub fn start(socket: PathBuf, pane: String) -> ResizeWatch {
        let mut fds = [0i32; 2];
        // SAFETY: pipe fills two fds; signal installs a plain handler.
        unsafe {
            if libc::pipe(fds.as_mut_ptr()) != 0 {
                return ResizeWatch {
                    stop_fd: -1,
                    handle: None,
                };
            }
            WINCH_FD.store(fds[1], Ordering::SeqCst);
            libc::signal(libc::SIGWINCH, on_winch as *const () as libc::sighandler_t);
        }
        let rd = fds[0];
        let handle = thread::spawn(move || loop {
            let mut b = [0u8; 1];
            // SAFETY: reads one byte into b.
            let n = unsafe { libc::read(rd, b.as_mut_ptr() as *mut libc::c_void, 1) };
            if n <= 0 || b[0] == b'q' {
                // SAFETY: closing our own read end.
                unsafe {
                    libc::close(rd);
                }
                return;
            }
            let Some((w, h)) = crate::cli::term_size(1) else {
                continue;
            };
            if h <= 1 {
                continue;
            }
            let mut p = Map::new();
            p.insert("pane".into(), json!(pane));
            p.insert("cols".into(), json!(w));
            p.insert("rows".into(), json!(h - 1));
            if let Ok(c) = UnixStream::connect(&socket) {
                let _ = c.set_read_timeout(Some(Duration::from_secs(2)));
                let mut m = p.clone();
                m.insert("id".into(), json!("1"));
                m.insert("cmd".into(), json!("pane.resize"));
                send(&c, Value::Object(m));
                let mut dec = crate::proto::Decoder::new(io::BufReader::new(c));
                let _ = dec.next_line();
            }
        });
        ResizeWatch {
            stop_fd: fds[1],
            handle: Some(handle),
        }
    }

    pub fn stop(mut self) {
        if self.stop_fd >= 0 {
            // SAFETY: restores the default action and wakes the watcher.
            unsafe {
                libc::signal(libc::SIGWINCH, libc::SIG_DFL);
                WINCH_FD.store(-1, Ordering::SeqCst);
                libc::write(self.stop_fd, b"q".as_ptr() as *const libc::c_void, 1);
            }
        }
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
        if self.stop_fd >= 0 {
            // SAFETY: closing our own write end.
            unsafe {
                libc::close(self.stop_fd);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(serde::Deserialize)]
    struct Row {
        pane: String,
        label: String,
        harness: String,
        state: String,
        source: String,
        #[serde(default)]
        ask_tool: String,
        #[serde(default)]
        ask_summary: String,
        cols: i64,
        leave: String,
        line: String,
    }

    /// testdata/attach/status-lines.json, which Go's test and the Herdr
    /// bridge read too.
    #[test]
    fn status_line_matches_the_shared_fixture() {
        let raw = std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../coppice/testdata/attach/status-lines.json"
        ))
        .unwrap();
        let rows: Vec<Row> = serde_json::from_str(&raw).unwrap();
        assert!(!rows.is_empty());
        for (i, r) in rows.iter().enumerate() {
            let ev = if r.state.is_empty() {
                None
            } else {
                Some(StateView {
                    state: r.state.clone(),
                    source: r.source.clone(),
                    ask: if r.ask_tool.is_empty() {
                        None
                    } else {
                        Some((r.ask_tool.clone(), r.ask_summary.clone()))
                    },
                })
            };
            let got = status_line(&r.pane, &r.label, &r.harness, ev.as_ref(), r.cols, &r.leave);
            assert_eq!(got, r.line, "row {i}");
        }
    }

    #[test]
    fn the_leave_key_is_held_then_detaches() {
        let mut k = KeyReader::new(0);
        assert_eq!(k.feed(0), (Action::None, vec![]));
        assert_eq!(k.hold(), Some(LEAVE_HOLD));
        assert_eq!(k.flush(), (Action::Detach, vec![]));
        assert_eq!(k.feed(0), (Action::None, vec![]));
        assert_eq!(k.feed(0), (Action::None, vec![0]));
        assert_eq!(k.feed(b'a'), (Action::None, vec![b'a']));
        assert_eq!(k.feed(0), (Action::None, vec![]));
        assert_eq!(k.feed(b'x'), (Action::Detach, vec![]));
    }

    #[test]
    fn a_mouse_report_is_dropped_and_esc_forwards() {
        let mut k = KeyReader::new(0);
        for b in b"\x1b[<0;3;4" {
            assert_eq!(k.feed(*b), (Action::None, vec![]));
        }
        assert_eq!(k.feed(b'M'), (Action::None, vec![]));
        assert!(!k.pending());
        assert_eq!(k.feed(0x1b), (Action::None, vec![]));
        assert_eq!(k.flush(), (Action::None, vec![0x1b]));
        assert_eq!(k.feed(0x1b), (Action::None, vec![]));
        assert_eq!(k.feed(b'x'), (Action::None, vec![0x1b, b'x']));
    }

    #[test]
    fn runes_go_whole() {
        let mut u = Utf8Buf::default();
        assert_eq!(u.feed(0xc3), None);
        assert!(!full_rune(&[0xc3]) && !full_rune(&[0xe2, 0x82]) && full_rune(&[0xe2, 0x41]));
        assert_eq!(u.feed(0xa9), Some(vec![0xc3, 0xa9]));
        assert_eq!(u.feed(0xff), None);
        assert_eq!(u.feed(b'a'), Some(vec![b'a']));
    }

    #[test]
    fn a_frame_renders_with_a_wide_spacer() {
        let m: Map<String, Value> = serde_json::from_str(
            r##"{"event":"frame","cols":3,"rows":1,"cursor":[2,0],"rows_changed":{"0":[["中","","",0],["","","",0],["x","#ff0000","",1]]}}"##,
        )
        .unwrap();
        let f = parse_frame(&m).unwrap();
        let mut s = Screen::new(1, 1);
        s.apply(&f);
        assert_eq!(s.text(), "中 x");
        assert_eq!(
            s.render(),
            "\x1b[2J\x1b[H\x1b[K中\x1b[0;1m\x1b[38;2;255;0;0mx\x1b[0m\x1b[1;3H"
        );
    }
}
