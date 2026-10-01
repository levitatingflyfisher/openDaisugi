//! coppice tmux-mirror: one tmux window per open coppice pane, each running
//! coppice attach on its pane. The mirror drives tmux in control mode. It
//! creates, renames and kills only its own windows, and sets the session's
//! status-right to the roster count while it runs.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::process::CommandExt;
use std::path::Path;
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, SyncSender, TrySendError};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{Map, Value};

use crate::sys;

/// The longest window name the mirror sets, in runes.
pub const MAX_NAME: usize = 30;
/// The window option that holds the pane id of a window the mirror made.
pub const MARK_OPTION: &str = "@coppice_pane";
/// The session option that keeps the session's own status-right while a
/// mirror runs: "-" when it had none, "=" and the value when it had one.
pub const SAVED_OPTION: &str = "@coppice_saved_status";

#[derive(Clone, Debug, PartialEq)]
pub enum Kind {
    Reply,
    Output,
    WindowClose,
    Exit,
    Other,
}

/// One parsed unit of control-mode output.
#[derive(Clone, Debug)]
pub struct Event {
    pub kind: Kind,
    pub num: i64,
    pub client: bool,
    pub ok: bool,
    pub lines: Vec<String>,
    pub window: String,
    pub pane: String,
    pub data: String,
    pub raw: String,
}

impl Event {
    fn new(kind: Kind) -> Event {
        Event {
            kind,
            num: 0,
            client: false,
            ok: false,
            lines: Vec::new(),
            window: String::new(),
            pane: String::new(),
            data: String::new(),
            raw: String::new(),
        }
    }
}

/// Turns control-mode lines into events, one line at a time.
pub struct Parser {
    open: bool,
    block: Event,
}

impl Default for Parser {
    fn default() -> Self {
        Parser {
            open: false,
            block: Event::new(Kind::Reply),
        }
    }
}

/// Reads "%begin TIME NUM FLAGS" and the %end and %error lines like it.
fn guard(line: &str, word: &str) -> Option<(i64, i64)> {
    let f: Vec<&str> = line.split_whitespace().collect();
    if f.len() != 4 || f[0] != word {
        return None;
    }
    Some((f[2].parse().ok()?, f[3].parse().ok()?))
}

impl Parser {
    pub fn line(&mut self, line: &str) -> Option<Event> {
        let line = line.strip_suffix('\r').unwrap_or(line);
        if self.open {
            for (word, ok) in [("%end", true), ("%error", false)] {
                if let Some((num, flags)) = guard(line, word) {
                    if num == self.block.num {
                        let mut ev = std::mem::replace(&mut self.block, Event::new(Kind::Reply));
                        ev.ok = ok;
                        ev.client = flags & 1 == 1;
                        self.open = false;
                        return Some(ev);
                    }
                }
            }
            self.block.lines.push(line.to_string());
            return None;
        }
        if let Some((num, flags)) = guard(line, "%begin") {
            self.open = true;
            self.block = Event::new(Kind::Reply);
            self.block.num = num;
            self.block.client = flags & 1 == 1;
            return None;
        }
        let (word, rest) = line.split_once(' ').unwrap_or((line, ""));
        let mut ev = match word {
            "%window-close" | "%unlinked-window-close" => {
                let mut e = Event::new(Kind::WindowClose);
                e.window = rest
                    .split_once(' ')
                    .map(|x| x.0)
                    .unwrap_or(rest)
                    .to_string();
                e
            }
            "%output" => {
                let mut e = Event::new(Kind::Output);
                let (p, d) = rest.split_once(' ').unwrap_or((rest, ""));
                e.pane = p.to_string();
                e.data = d.to_string();
                e
            }
            "%exit" => Event::new(Kind::Exit),
            _ => Event::new(Kind::Other),
        };
        ev.raw = line.to_string();
        Some(ev)
    }
}

/// Sends commands to tmux -C and reads its replies, one in flight.
pub struct Client {
    w: Mutex<Option<ChildStdin>>,
    replies: Mutex<Receiver<Event>>,
    pub events: Receiver<Event>,
    done: Arc<AtomicBool>,
    done_rx: Mutex<Receiver<()>>,
    broken: AtomicBool,
    lock: Mutex<()>,
    timeout: Duration,
}

impl Client {
    pub fn new<R: Read + Send + 'static>(w: ChildStdin, r: R) -> Client {
        let (rtx, rrx) = mpsc::sync_channel::<Event>(1);
        let (etx, erx) = mpsc::sync_channel::<Event>(256);
        let (dtx, drx) = mpsc::channel::<()>();
        let done = Arc::new(AtomicBool::new(false));
        let d2 = done.clone();
        thread::spawn(move || {
            read_loop(r, &rtx, &etx);
            d2.store(true, Ordering::SeqCst);
            drop(rtx);
            drop(etx);
            let _ = dtx.send(());
        });
        Client {
            w: Mutex::new(Some(w)),
            replies: Mutex::new(rrx),
            events: erx,
            done,
            done_rx: Mutex::new(drx),
            broken: AtomicBool::new(false),
            lock: Mutex::new(()),
            timeout: Duration::from_secs(5),
        }
    }

    pub fn is_done(&self) -> bool {
        self.done.load(Ordering::SeqCst)
    }

    /// Waits up to d for tmux to end the client.
    pub fn wait_done(&self, d: Duration) -> bool {
        if self.is_done() {
            return true;
        }
        let rx = self.done_rx.lock().unwrap_or_else(|e| e.into_inner());
        matches!(
            rx.recv_timeout(d),
            Ok(()) | Err(RecvTimeoutError::Disconnected)
        ) || self.is_done()
    }

    /// None while the client can send.
    pub fn err(&self) -> Option<String> {
        if self.broken.load(Ordering::SeqCst) {
            return Some("tmux stopped answering the control client".into());
        }
        if self.is_done() {
            return Some(ERR_CLOSED.into());
        }
        None
    }

    /// Ends the client by closing tmux's stdin.
    pub fn close(&self) {
        self.w.lock().unwrap_or_else(|e| e.into_inner()).take();
    }

    /// Sends one command and returns the lines of its reply.
    pub fn exec(&self, cmd: &str) -> Result<Vec<String>, String> {
        if cmd.contains(['\r', '\n']) {
            return Err(format!(
                "a tmux command must be one line: {}",
                sys::go_quote(cmd)
            ));
        }
        let _g = self.lock.lock().unwrap_or_else(|e| e.into_inner());
        if self.broken.load(Ordering::SeqCst) {
            return Err("tmux stopped answering the control client".into());
        }
        if self.is_done() {
            return Err(ERR_CLOSED.into());
        }
        {
            let mut w = self.w.lock().unwrap_or_else(|e| e.into_inner());
            let Some(w) = w.as_mut() else {
                return Err("write |1: file already closed".into());
            };
            if let Err(e) = w
                .write_all(format!("{cmd}\n").as_bytes())
                .and_then(|_| w.flush())
            {
                return Err(format!("write |1: {}", sys::go_errno_text(&e)));
            }
        }
        let rx = self.replies.lock().unwrap_or_else(|e| e.into_inner());
        match rx.recv_timeout(self.timeout) {
            Ok(ev) => {
                if !ev.ok {
                    return Err(format!(
                        "tmux refused {}: {}",
                        sys::go_quote(cmd),
                        ev.lines.join(" ")
                    ));
                }
                Ok(ev.lines)
            }
            Err(RecvTimeoutError::Disconnected) => Err(ERR_CLOSED.into()),
            Err(RecvTimeoutError::Timeout) => {
                if self.is_done() {
                    return Err(ERR_CLOSED.into());
                }
                self.broken.store(true, Ordering::SeqCst);
                Err(format!("tmux did not answer {}", sys::go_quote(cmd)))
            }
        }
    }
}

/// The error of a command sent after tmux ended the client.
pub const ERR_CLOSED: &str = "tmux ended the control client";

fn read_loop<R: Read>(r: R, replies: &SyncSender<Event>, events: &SyncSender<Event>) {
    let mut p = Parser::default();
    let mut br = BufReader::new(r);
    let mut buf = Vec::new();
    loop {
        buf.clear();
        match br.read_until(b'\n', &mut buf) {
            Ok(0) | Err(_) => return,
            Ok(_) => {}
        }
        if buf.last() == Some(&b'\n') {
            buf.pop();
        }
        if buf.len() > 1024 * 1024 {
            return;
        }
        let line = String::from_utf8_lossy(&buf);
        let Some(ev) = p.line(&line) else { continue };
        match ev.kind {
            Kind::Reply if ev.client => {
                let _ = replies.try_send(ev);
            }
            Kind::Reply | Kind::Output => {}
            Kind::Exit => return,
            _ => {
                if let Err(TrySendError::Disconnected(_)) = events.try_send(ev) {
                    return;
                }
            }
        }
    }
}

/// One argument tmux reads as literal text.
fn quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// Each # doubled, so tmux reads no format in a name.
fn literal(s: &str) -> String {
    s.replace('#', "##")
}

pub fn list_cmd(session: &str) -> String {
    format!(
        "list-windows -t {} -F '#{{window_id}}\t#{{window_name}}\t#{{{MARK_OPTION}}}'",
        quote(session)
    )
}

fn line(args: &[&str]) -> String {
    args.iter().map(|a| quote(a)).collect::<Vec<_>>().join(" ")
}

pub fn create_cmd(session: &str, name: &str, exe: &str, socket: &str, pane: &str) -> String {
    format!(
        "new-window -d -P -F '#{{window_id}}' -t {} -n {} {}",
        quote(&format!("{session}:")),
        quote(&literal(name)),
        line(&[exe, "--socket", socket, "attach", pane])
    )
}

/// A tmux command as one control-mode line: the verb and the known flags
/// as they are, every other argument quoted.
fn ctl(args: &[&str]) -> String {
    args.iter()
        .enumerate()
        .map(|(i, a)| {
            if i == 0 || matches!(*a, "-q" | "-v" | "-t" | "-u" | "-w") {
                a.to_string()
            } else {
                quote(a)
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

pub fn mark_cmd(window: &str, pane: &str) -> String {
    format!(
        "set-option -w -t {} {MARK_OPTION} {}",
        quote(window),
        quote(pane)
    )
}

pub fn rename_cmd(window: &str, name: &str) -> String {
    format!(
        "rename-window -t {} {}",
        quote(window),
        quote(&literal(name))
    )
}

pub fn kill_cmd(window: &str) -> String {
    format!("kill-window -t {}", quote(window))
}

pub fn status_cmd(session: &str, l: &str) -> String {
    format!(
        "set-option -t {} status-right {}",
        quote(session),
        quote(&literal(l))
    )
}

fn restore_args(session: &str, saved: Option<&str>) -> Vec<Vec<String>> {
    let s = |v: &[&str]| v.iter().map(|x| x.to_string()).collect::<Vec<_>>();
    let set = match saved {
        Some(v) => s(&["set-option", "-t", session, "status-right", v]),
        None => s(&["set-option", "-u", "-t", session, "status-right"]),
    };
    vec![set, s(&["set-option", "-u", "-t", session, SAVED_OPTION])]
}

/// One row of pane.list, as far as the mirror reads it.
#[derive(Clone, Debug, Default)]
pub struct Pane {
    pub id: String,
    pub label: String,
    pub state: String,
    pub closed: bool,
    pub held: bool,
}

#[derive(Clone, Debug, PartialEq)]
pub struct Window {
    pub id: String,
    pub name: String,
    pub pane: String,
}

#[derive(Clone, Debug, PartialEq)]
pub enum Op {
    Create {
        pane: String,
        name: String,
    },
    Rename {
        window: String,
        pane: String,
        name: String,
    },
    Kill {
        window: String,
        pane: String,
    },
}

/// Reads what list_cmd prints. A line with fewer than three fields is
/// skipped.
pub fn parse_windows(lines: &[String]) -> Vec<Window> {
    let mut out = Vec::new();
    for l in lines {
        let f: Vec<&str> = l.split('\t').collect();
        if f.len() < 3 || !f[0].starts_with('@') {
            continue;
        }
        out.push(Window {
            id: f[0].to_string(),
            name: f[1..f.len() - 1].join("\t"),
            pane: f[f.len() - 1].to_string(),
        });
    }
    out
}

/// Every control rune and every rune that is not printable dropped.
fn clean(s: &str) -> String {
    let out: String = s
        .chars()
        .filter(|r| !(r.is_control() || (!sys::is_print_rune(*r) && *r != ' ') || *r == '\u{FFFD}'))
        .collect();
    out.trim_matches(|c: char| c.is_whitespace()).to_string()
}

/// The name of a pane's window: its label, or its id, cut to MAX_NAME.
pub fn window_name(p: &Pane) -> String {
    let mut name = clean(&p.label);
    if name.is_empty() {
        name = clean(&p.id);
    }
    let r: Vec<char> = name.chars().collect();
    if r.len() > MAX_NAME {
        name = r[..MAX_NAME - 1].iter().collect::<String>() + "…";
    }
    name
}

/// The ops that make the windows match the open panes: renames and kills
/// first, in window order, then creates, in pane order.
pub fn plan(panes: &[Pane], windows: &[Window]) -> Vec<Op> {
    let open: HashMap<&str, &Pane> = panes
        .iter()
        .filter(|p| !p.closed && !p.id.is_empty())
        .map(|p| (p.id.as_str(), p))
        .collect();
    let mut ops = Vec::new();
    let mut has: HashMap<String, bool> = HashMap::new();
    for w in windows {
        if w.pane.is_empty() {
            continue;
        }
        match open.get(w.pane.as_str()) {
            Some(p) if !has.contains_key(&w.pane) => {
                has.insert(w.pane.clone(), true);
                let name = window_name(p);
                if name != w.name {
                    ops.push(Op::Rename {
                        window: w.id.clone(),
                        pane: w.pane.clone(),
                        name,
                    });
                }
            }
            _ => ops.push(Op::Kill {
                window: w.id.clone(),
                pane: w.pane.clone(),
            }),
        }
    }
    for p in panes {
        if p.closed || p.id.is_empty() || has.contains_key(&p.id) {
            continue;
        }
        has.insert(p.id.clone(), true);
        ops.push(Op::Create {
            pane: p.id.clone(),
            name: window_name(p),
        });
    }
    ops
}

/// The roster count for status-right.
pub fn status_line(panes: &[Pane]) -> String {
    let (mut need, mut working) = (0, 0);
    for p in panes {
        if p.closed {
            continue;
        }
        if p.state == "blocked" && !p.held {
            need += 1;
        } else if p.state == "working" || p.state == "blocked" {
            working += 1;
        }
    }
    let mut parts = Vec::new();
    if need > 0 {
        parts.push(format!("{need} need you"));
    }
    if working > 0 {
        parts.push(format!("{working} working"));
    }
    if parts.is_empty() {
        return "quiet".into();
    }
    parts.join(" · ")
}

/// The rows of a pane.list result. A row with no id is skipped.
pub fn panes_from_list(res: &Map<String, Value>) -> Vec<Pane> {
    let mut out = Vec::new();
    for r in res
        .get("panes")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let Some(m) = r.as_object() else { continue };
        let s = |k: &str| m.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        let id = s("id");
        if id.is_empty() {
            continue;
        }
        out.push(Pane {
            id,
            label: s("label"),
            state: s("state"),
            closed: m.get("closed").and_then(Value::as_bool) == Some(true),
            held: matches!(m.get("held"), Some(Value::Object(_))),
        });
    }
    out
}

/// Runs tmux once with the given arguments, when the control client is
/// gone.
pub type OneShot<'a> = Box<dyn Fn(&[String]) -> Result<(), String> + 'a>;

/// Keeps one session's windows in line with the panes.
pub struct Mirror<'a> {
    pub tmux: &'a Client,
    pub session: String,
    pub exe: String,
    pub socket: String,
    pub one_shot: OneShot<'a>,
    started: bool,
    saved: Option<String>,
    owned: HashMap<String, String>,
    hidden: HashMap<String, bool>,
}

fn log(line: &str) {
    eprintln!("coppice tmux-mirror: {line}");
}

impl<'a> Mirror<'a> {
    /// Finds the session's own status-right, so stop can put it back.
    pub fn start(&mut self) -> Result<(), String> {
        let kept = self.tmux.exec(&ctl(&[
            "show-options",
            "-q",
            "-v",
            "-t",
            &self.session,
            SAVED_OPTION,
        ]))?;
        let saved = match kept.first() {
            Some(k) if k == "-" => None,
            Some(k) if k.starts_with('=') => Some(k[1..].to_string()),
            _ => {
                let lines = self.tmux.exec(&ctl(&[
                    "show-options",
                    "-v",
                    "-t",
                    &self.session,
                    "status-right",
                ]))?;
                let (saved, enc) = match lines.first() {
                    Some(v) => (Some(v.clone()), format!("={v}")),
                    None => (None, "-".to_string()),
                };
                self.tmux.exec(&ctl(&[
                    "set-option",
                    "-t",
                    &self.session,
                    SAVED_OPTION,
                    &enc,
                ]))?;
                saved
            }
        };
        self.started = true;
        self.saved = saved;
        Ok(())
    }

    /// Puts back the status-right start found. The windows stay.
    pub fn stop(&mut self) -> Result<(), String> {
        if !self.started {
            return Ok(());
        }
        let cmds = restore_args(&self.session, self.saved.as_deref());
        if self.tmux.err().is_none() {
            let mut res = Ok(());
            for c in &cmds {
                let refs: Vec<&str> = c.iter().map(String::as_str).collect();
                if let Err(e) = self.tmux.exec(&ctl(&refs)) {
                    res = Err(e);
                    break;
                }
            }
            if res.is_ok() || self.tmux.err().is_none() {
                return res;
            }
        }
        let mut args = cmds[0].clone();
        args.push(";".into());
        args.extend(cmds[1].iter().cloned());
        (self.one_shot)(&args)
    }

    /// Reads one notification: the close of a window the mirror owns hides
    /// its pane.
    pub fn handle(&mut self, ev: &Event) {
        if ev.kind != Kind::WindowClose {
            return;
        }
        if let Some(pane) = self.owned.remove(&ev.window) {
            self.hidden.insert(pane, true);
        }
    }

    /// Lists the windows, applies the plan, and sets the status line.
    pub fn sync(&mut self, panes: &[Pane]) -> Result<(), String> {
        let lines = self.tmux.exec(&list_cmd(&self.session))?;
        let windows = parse_windows(&lines);
        for w in &windows {
            if !w.pane.is_empty() {
                self.owned.insert(w.id.clone(), w.pane.clone());
            }
        }
        let shown: Vec<Pane> = panes
            .iter()
            .filter(|p| !self.hidden.contains_key(&p.id))
            .cloned()
            .collect();
        for op in plan(&shown, &windows) {
            if let Err(e) = self.apply(&op) {
                if let Some(c) = self.tmux.err() {
                    return Err(c);
                }
                log(&e);
            }
        }
        self.tmux
            .exec(&status_cmd(&self.session, &status_line(panes)))?;
        Ok(())
    }

    fn apply(&mut self, op: &Op) -> Result<(), String> {
        match op {
            Op::Rename { window, name, .. } => {
                self.tmux.exec(&rename_cmd(window, name)).map(|_| ())
            }
            Op::Kill { window, .. } => {
                self.owned.remove(window);
                self.tmux.exec(&kill_cmd(window)).map(|_| ())
            }
            Op::Create { pane, name } => {
                let lines = self.tmux.exec(&create_cmd(
                    &self.session,
                    name,
                    &self.exe,
                    &self.socket,
                    pane,
                ))?;
                if lines.len() != 1 || !lines[0].starts_with('@') {
                    let q: Vec<String> = lines.iter().map(|l| sys::go_quote(l)).collect();
                    return Err(format!(
                        "tmux gave no window id for {pane}: [{}]",
                        q.join(" ")
                    ));
                }
                let id = lines[0].trim().to_string();
                self.owned.insert(id.clone(), pane.clone());
                if let Err(e) = self.tmux.exec(&mark_cmd(&id, pane)) {
                    self.owned.remove(&id);
                    let _ = self.tmux.exec(&kill_cmd(&id));
                    return Err(e);
                }
                Ok(())
            }
        }
    }
}

/// The signals that stop the mirror, taken by one thread.
fn stop_signals() -> Receiver<()> {
    let (tx, rx) = mpsc::channel();
    // SAFETY: the set is built by sigemptyset before use, and only the
    // waiter thread takes the blocked signals.
    unsafe {
        let mut set: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut set);
        libc::sigaddset(&mut set, libc::SIGINT);
        libc::sigaddset(&mut set, libc::SIGTERM);
        libc::sigaddset(&mut set, libc::SIGHUP);
        libc::pthread_sigmask(libc::SIG_BLOCK, &set, std::ptr::null_mut());
        thread::spawn(move || loop {
            let mut sig = 0;
            libc::sigwait(&set, &mut sig);
            if tx.send(()).is_err() {
                return;
            }
        });
    }
    rx
}

/// The whole command: mirror the panes into a tmux session until stopped.
pub fn run(socket: &Path, mut session: String, tmux_socket: String) -> i32 {
    let tmux_pane = std::env::var("TMUX_PANE").unwrap_or_default();
    if session.is_empty() && tmux_pane.is_empty() {
        eprintln!("run coppice tmux-mirror inside tmux, or pass --session NAME");
        return 1;
    }
    let Some(tmux) = crate::pane::look_path("tmux") else {
        eprintln!("coppice tmux-mirror needs tmux on PATH");
        return 1;
    };
    let mut base: Vec<String> = Vec::new();
    if !tmux_socket.is_empty() {
        base.push("-S".into());
        base.push(tmux_socket.clone());
    }
    if session.is_empty() {
        let out = Command::new(&tmux)
            .args(&base)
            .args(["display-message", "-p", "-t", &tmux_pane, "#{session_id}"])
            .stderr(Stdio::inherit())
            .output();
        let ok = out.as_ref().map(|o| o.status.success()).unwrap_or(false);
        session = out
            .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
            .unwrap_or_default();
        if !ok || session.is_empty() {
            eprintln!("cannot find the tmux session of this pane. Pass --session NAME.");
            return 1;
        }
    }
    let exe = match std::env::current_exe() {
        Ok(e) => e.to_string_lossy().into_owned(),
        Err(e) => {
            eprintln!("cannot find the coppice program: {e}");
            return 1;
        }
    };
    let sock = match sys::abs_path(socket) {
        Ok(s) => s.to_string_lossy().into_owned(),
        Err(e) => {
            eprintln!("cannot read the socket path {}: {e}", socket.display());
            return 1;
        }
    };
    let Ok(mut cl) = crate::cli::dial_existing_pub(Path::new(&sock)) else {
        eprintln!("cannot reach the server at {sock}. Run: coppice server start");
        return 3;
    };
    let mut cmd = Command::new(&tmux);
    cmd.args(&base)
        .args(["-C", "attach-session", "-t", &session])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit());
    // SAFETY: setpgid and sigprocmask are async-signal-safe.
    unsafe {
        cmd.pre_exec(|| {
            libc::setpgid(0, 0);
            let mut set: libc::sigset_t = std::mem::zeroed();
            libc::sigemptyset(&mut set);
            libc::sigprocmask(libc::SIG_SETMASK, &set, std::ptr::null_mut());
            Ok(())
        });
    }
    let mut child: Child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("cannot start tmux: {e}");
            return 1;
        }
    };
    let sig = stop_signals();
    let stdin = child.stdin.take().expect("stdin");
    let stdout = child.stdout.take().expect("stdout");
    let tc = Client::new(stdin, stdout);
    let code = mirror_loop(&tc, &mut cl, &session, &exe, &sock, &tmux, &base, &sig);
    tc.close();
    let _ = child.wait();
    cl.close();
    code
}

#[allow(clippy::too_many_arguments)]
fn mirror_loop(
    tc: &Client,
    cl: &mut crate::cli::PubClient,
    session: &str,
    exe: &str,
    sock: &str,
    tmux: &str,
    base: &[String],
    sig: &Receiver<()>,
) -> i32 {
    if let Err(e) = tc.exec("refresh-client -f no-output") {
        eprintln!("tmux refused the control client: {e}");
        return 1;
    }
    let tmux = tmux.to_string();
    let base = base.to_vec();
    let mut m = Mirror {
        tmux: tc,
        session: session.to_string(),
        exe: exe.to_string(),
        socket: sock.to_string(),
        one_shot: Box::new(move |args: &[String]| {
            let st = Command::new(&tmux)
                .args(&base)
                .args(args)
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .status()
                .map_err(|e| e.to_string())?;
            if st.success() {
                Ok(())
            } else {
                Err(format!("exit status {}", st.code().unwrap_or(-1)))
            }
        }),
        started: false,
        saved: None,
        owned: HashMap::new(),
        hidden: HashMap::new(),
    };
    if let Err(e) = m.start() {
        eprintln!("tmux refused to show the status line of {session}: {e}");
        return 1;
    }
    let code = (|| {
        eprintln!(
            "coppice tmux-mirror: mirroring panes into tmux session {session}. ctrl-c stops it."
        );
        loop {
            let res = match cl.call("pane.list", Map::new()) {
                Ok(r) => r,
                Err(e) => return crate::cli::report_pub(cl, e),
            };
            if let Err(e) = m.sync(&panes_from_list(&res)) {
                if tc.wait_done(Duration::ZERO) {
                    eprintln!("tmux ended the mirror");
                    return 0;
                }
                eprintln!("coppice tmux-mirror: {e}");
                return 1;
            }
            let tick = Instant::now() + Duration::from_secs(1);
            loop {
                if sig.try_recv().is_ok() {
                    return 0;
                }
                if tc.is_done() {
                    eprintln!("tmux ended the mirror");
                    return 0;
                }
                let now = Instant::now();
                if now >= tick {
                    break;
                }
                match tc
                    .events
                    .recv_timeout((tick - now).min(Duration::from_millis(50)))
                {
                    Ok(ev) => {
                        m.handle(&ev);
                        if ev.kind == Kind::WindowClose {
                            break;
                        }
                    }
                    Err(RecvTimeoutError::Timeout) => {}
                    Err(RecvTimeoutError::Disconnected) => {
                        eprintln!("tmux ended the mirror");
                        return 0;
                    }
                }
            }
        }
    })();
    let _ = m.stop();
    code
}

#[cfg(test)]
mod tests {
    use super::*;

    fn events(name: &str) -> Vec<Event> {
        let raw = std::fs::read_to_string(format!(
            "{}/../coppice/testdata/tmux/{name}",
            env!("CARGO_MANIFEST_DIR")
        ))
        .unwrap();
        let mut p = Parser::default();
        raw.lines().filter_map(|l| p.line(l)).collect()
    }

    #[test]
    fn the_lifecycle_transcript_parses() {
        let evs = events("lifecycle.txt");
        let replies: Vec<&Event> = evs.iter().filter(|e| e.kind == Kind::Reply).collect();
        assert!(!replies[0].client);
        let list = replies
            .iter()
            .find(|e| e.client && e.lines.iter().any(|l| l.contains("w1:p1")))
            .unwrap();
        let wins = parse_windows(&list.lines);
        assert_eq!(
            wins[1],
            Window {
                id: "@1".into(),
                name: "b'x;y#z".into(),
                pane: "w1:p1".into()
            }
        );
        let bad = replies.iter().find(|e| !e.ok).unwrap();
        assert_eq!(
            bad.lines,
            vec!["parse error: unknown command: no-such-command"]
        );
        assert!(evs
            .iter()
            .any(|e| e.kind == Kind::WindowClose && e.window == "@1"));
        assert_eq!(evs.last().unwrap().kind, Kind::Exit);
    }

    #[test]
    fn the_window_close_transcript_parses() {
        let evs = events("window-close.txt");
        assert!(evs.iter().filter(|e| e.kind == Kind::WindowClose).count() >= 2);
        assert!(evs.iter().any(|e| e.kind == Kind::Output));
    }

    #[test]
    fn plan_kills_renames_then_creates() {
        let panes = vec![
            Pane {
                id: "w1:p1".into(),
                label: "a".into(),
                ..Default::default()
            },
            Pane {
                id: "w1:p2".into(),
                label: "".into(),
                ..Default::default()
            },
            Pane {
                id: "w1:p3".into(),
                closed: true,
                ..Default::default()
            },
        ];
        let wins = vec![
            Window {
                id: "@0".into(),
                name: "mine".into(),
                pane: "".into(),
            },
            Window {
                id: "@1".into(),
                name: "old".into(),
                pane: "w1:p1".into(),
            },
            Window {
                id: "@2".into(),
                name: "x".into(),
                pane: "w1:p3".into(),
            },
            Window {
                id: "@3".into(),
                name: "a".into(),
                pane: "w1:p1".into(),
            },
        ];
        assert_eq!(
            plan(&panes, &wins),
            vec![
                Op::Rename {
                    window: "@1".into(),
                    pane: "w1:p1".into(),
                    name: "a".into()
                },
                Op::Kill {
                    window: "@2".into(),
                    pane: "w1:p3".into()
                },
                Op::Kill {
                    window: "@3".into(),
                    pane: "w1:p1".into()
                },
                Op::Create {
                    pane: "w1:p2".into(),
                    name: "w1:p2".into()
                },
            ]
        );
    }

    #[test]
    fn names_and_status() {
        let long = Pane {
            id: "x".into(),
            label: "\u{1b}abcdefghijklmnopqrstuvwxyz0123456789".into(),
            ..Default::default()
        };
        assert_eq!(window_name(&long), "abcdefghijklmnopqrstuvwxyz012…");
        let ps = vec![
            Pane {
                id: "a".into(),
                state: "blocked".into(),
                ..Default::default()
            },
            Pane {
                id: "b".into(),
                state: "blocked".into(),
                held: true,
                ..Default::default()
            },
            Pane {
                id: "c".into(),
                state: "working".into(),
                ..Default::default()
            },
        ];
        assert_eq!(status_line(&ps), "1 need you · 2 working");
        assert_eq!(status_line(&[]), "quiet");
        assert_eq!(quote("it's"), "'it'\\''s'");
        assert_eq!(
            ctl(&["set-option", "-t", "s", "status-right", "-x"]),
            "set-option -t 's' 'status-right' '-x'"
        );
    }
}
