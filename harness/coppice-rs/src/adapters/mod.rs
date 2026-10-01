//! The headless harnesses. A headless pane wraps a harness process and
//! turns what it says into events: transcript text the server writes into
//! the pane's grid, and states with source headless.
//!
//! Every adapter keeps the rules Go's pane.Adapter states:
//!
//! 1. The child's environment is pane::build_env of the server's own, so
//!    a harness's gate hook finds COPPICE_SOCK, COPPICE_PANE and
//!    COPPICE_DATA_DIR.
//! 2. The server renders. An adapter never writes into the grid.
//! 3. stop signals and kills, and waits until the event stream has ended.
//!    Only the adapter's own threads send events.
//! 4. An end event is terminal. Model text goes into the grid as is, so
//!    the server's own "[end]" and "[error]" rows are advisory.

pub mod claude;
pub mod codex;
pub mod http;
pub mod opencode;
pub mod pi;
pub mod sprig;

use std::collections::BTreeMap;
use std::io;
use std::os::fd::{AsRawFd, OwnedFd};
use std::process::{Child, Command};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{channel, Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::pane::Flag;
use crate::state::Ask;

/// The kinds of event an adapter sends.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum Kind {
    #[default]
    Text,
    Tool,
    State,
    End,
    Error,
    /// A subagent inside the pane's harness.
    Child,
}

/// One thing a harness said.
#[derive(Clone, Debug, Default)]
pub struct Ev {
    pub kind: Kind,
    pub text: String,
    pub tool: String,
    pub state: String,
    pub detail: String,
    pub ask: Option<Ask>,
    pub child: String,
}

impl Ev {
    pub fn state(st: &str, detail: impl Into<String>) -> Ev {
        Ev {
            kind: Kind::State,
            state: st.into(),
            detail: detail.into(),
            ..Default::default()
        }
    }
    pub fn text(t: impl Into<String>) -> Ev {
        Ev {
            kind: Kind::Text,
            text: t.into(),
            ..Default::default()
        }
    }
    pub fn tool(tool: impl Into<String>, detail: impl Into<String>) -> Ev {
        Ev {
            kind: Kind::Tool,
            tool: tool.into(),
            detail: detail.into(),
            ..Default::default()
        }
    }
    pub fn error(detail: impl Into<String>) -> Ev {
        Ev {
            kind: Kind::Error,
            detail: detail.into(),
            ..Default::default()
        }
    }
    pub fn end(detail: impl Into<String>) -> Ev {
        Ev {
            kind: Kind::End,
            state: "done".into(),
            detail: detail.into(),
            ..Default::default()
        }
    }
}

/// The idle, working, blocked, done and unknown words.
pub const IDLE: &str = "idle";
pub const WORKING: &str = "working";
pub const BLOCKED: &str = "blocked";
pub const UNKNOWN: &str = "unknown";

/// How many root pids a proc keeps for one pane.
pub const PID_KEEP: usize = 64;

/// What a start needs.
#[derive(Clone, Default)]
pub struct StartOpts {
    pub cwd: String,
    pub env: BTreeMap<String, String>,
    /// Extra argv beyond the adapter's own.
    pub argv: Vec<String>,
    /// A harness session id to resume.
    pub resume: String,
    pub sock: String,
    pub pane_id: String,
    pub data_dir: String,
    /// The binary to run. Empty means the adapter's variable
    /// (COPPICE_CLAUDE_BIN and the like) or its own name. Tests set it, so
    /// no test changes the process environment.
    pub bin: String,
}

impl StartOpts {
    /// The binary: the explicit one, else the variable, else the name.
    pub fn binary(&self, var: &str, name: &str) -> String {
        if !self.bin.is_empty() {
            return self.bin.clone();
        }
        match std::env::var(var) {
            Ok(b) if !b.is_empty() => b,
            _ => name.to_string(),
        }
    }

    /// The child's environment: the server's own, the pane's extras and
    /// the coppice variables, as for a pty pane.
    pub fn child_env(&self, extra: &BTreeMap<String, String>) -> Vec<(String, String)> {
        crate::pane::build_env(
            std::env::vars().collect(),
            extra,
            &self.sock,
            &self.pane_id,
            &self.data_dir,
        )
    }
}

/// One running headless harness.
pub trait Proc: Send + Sync {
    fn prompt(&self, text: &str) -> Result<(), String>;
    /// A prompt that knows who typed it. operator is true only for a
    /// connection that may run agent.allow.
    fn prompt_from(&self, text: &str, _operator: bool) -> Result<(), String> {
        self.prompt(text)
    }
    fn write_stdin(&self, b: &[u8]) -> Result<(), String>;
    /// The event stream. The pump takes it once.
    fn take_events(&self) -> Option<Receiver<Ev>>;
    fn session_id(&self) -> Option<String>;
    /// Signals, kills and waits until the stream has ended.
    fn stop(&self);
    /// The root pid of every process the proc started for its pane.
    fn pids(&self) -> Vec<i32>;
    /// Whether the harness holds asks of its own.
    fn answerer(&self) -> bool {
        false
    }
    fn owns_ask(&self, _id: &str) -> bool {
        false
    }
    fn answer(&self, _id: &str, _allow: bool, _reason: &str) -> Result<(), String> {
        Err("this harness holds no asks".into())
    }
}

/// One kind of harness.
pub trait Adapter: Send + Sync {
    fn name(&self) -> &'static str;
    fn start(&self, o: StartOpts) -> Result<Arc<dyn Proc>, String>;
    /// The extra argv for a fork's first start, for an adapter that can
    /// fork. None when the harness cannot.
    fn fork_argv(&self, _session_id: &str) -> Option<Result<Vec<String>, String>> {
        None
    }
}

static REGISTRY: [&dyn Adapter; 5] = [
    &claude::Claude,
    &codex::Codex,
    &opencode::Opencode,
    &pi::Pi,
    &sprig::Sprig,
];

/// The adapter registered under name.
pub fn get(name: &str) -> Option<&'static dyn Adapter> {
    REGISTRY.iter().copied().find(|a| a.name() == name)
}

/// Every adapter name, sorted.
pub fn names() -> Vec<&'static str> {
    REGISTRY.iter().map(|a| a.name()).collect()
}

/// The sending side of a proc's stream, and its stop signal. A send after
/// stop is dropped, as Go's send selects on the stop channel. The stream
/// ends when every sender is gone.
#[derive(Clone)]
pub struct Out {
    tx: Arc<Mutex<Option<Sender<Ev>>>>,
    pub stop: Arc<AtomicBool>,
}

impl Out {
    pub fn new() -> (Out, Receiver<Ev>) {
        let (tx, rx) = channel();
        (
            Out {
                tx: Arc::new(Mutex::new(Some(tx))),
                stop: Arc::new(AtomicBool::new(false)),
            },
            rx,
        )
    }

    pub fn stopped(&self) -> bool {
        self.stop.load(Ordering::SeqCst)
    }

    /// Sends ev unless stop was signalled. False when it was not sent.
    pub fn send(&self, ev: Ev) -> bool {
        if self.stopped() {
            return false;
        }
        self.send_always(ev)
    }

    /// Sends ev even after stop, while the stream is open.
    pub fn send_always(&self, ev: Ev) -> bool {
        match &*self.tx.lock().unwrap_or_else(|e| e.into_inner()) {
            Some(tx) => tx.send(ev).is_ok(),
            None => false,
        }
    }

    /// Ends the stream.
    pub fn close(&self) {
        self.tx.lock().unwrap_or_else(|e| e.into_inner()).take();
    }
}

/// The receiving end, kept until the pump takes it.
pub type EventsSlot = Mutex<Option<Receiver<Ev>>>;

/// Go's exec error words for a binary that cannot start.
pub fn start_error(bin: &str, e: &io::Error) -> String {
    format!("cannot start {bin}: {}", exec_error(bin, e))
}

/// The error Go's exec.Cmd.Start gives for bin.
pub fn exec_error(bin: &str, e: &io::Error) -> String {
    if !bin.contains('/') {
        return format!(
            "exec: {}: executable file not found in $PATH",
            crate::sys::go_quote(bin)
        );
    }
    format!("fork/exec {bin}: {}", crate::sys::go_errno_text(e))
}

/// A Command for bin as Go's exec.Command builds one: a bare name is
/// found on the server's PATH first, so a miss is named as Go names it.
pub fn command(
    bin: &str,
    argv: &[String],
    cwd: &str,
    env: Vec<(String, String)>,
) -> Result<Command, String> {
    let path = if bin.contains('/') {
        bin.to_string()
    } else {
        match crate::pane::look_path(bin) {
            Some(p) => p,
            None => {
                return Err(format!(
                    "exec: {}: executable file not found in $PATH",
                    crate::sys::go_quote(bin)
                ))
            }
        }
    };
    let mut cmd = Command::new(path);
    cmd.args(argv).env_clear().envs(env);
    if !cwd.is_empty() {
        cmd.current_dir(cwd);
    }
    Ok(cmd)
}

/// Spawns cmd, naming a failure as Go's "cannot start BIN: ..." does.
pub fn spawn(bin: &str, cmd: &mut Command) -> Result<Child, String> {
    cmd.spawn().map_err(|e| start_error(bin, &e))
}

/// A pipe whose read end this process keeps, both ends close-on-exec.
pub fn pipe() -> io::Result<(OwnedFd, OwnedFd)> {
    let mut fds = [0i32; 2];
    // SAFETY: fds is a live array of two ints, as pipe2 needs.
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: pipe2 just returned these two fds, and nothing else owns
    // them.
    unsafe {
        use std::os::fd::FromRawFd;
        Ok((OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])))
    }
}

/// The cap on one line, as Go's bufio.Scanner buffer is set.
pub const MAX_LINE: usize = 8 << 20;

/// Reads lines as Go's bufio.Scanner with ScanLines does: a trailing \r
/// is dropped, a last line without an end still counts, and a line over
/// MAX_LINE stops the scan with "bufio.Scanner: token too long". Each read
/// waits at most 100 ms, so a cancel flag can end a read the writer keeps
/// open.
pub struct Lines {
    fd: OwnedFd,
    buf: Vec<u8>,
    eof: bool,
    pub err: Option<String>,
    cancel: Arc<AtomicBool>,
}

impl Lines {
    pub fn new(fd: OwnedFd, cancel: Arc<AtomicBool>) -> Lines {
        Lines {
            fd,
            buf: Vec::new(),
            eof: false,
            err: None,
            cancel,
        }
    }

    fn fill(&mut self) -> bool {
        loop {
            if self.cancel.load(Ordering::SeqCst) {
                self.eof = true;
                self.err = Some("read |0: file already closed".into());
                return false;
            }
            let mut pfd = libc::pollfd {
                fd: self.fd.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            };
            // SAFETY: pfd is a live pollfd and the count is one.
            let n = unsafe { libc::poll(&mut pfd, 1, 100) };
            if n < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                self.eof = true;
                self.err = Some(e.to_string());
                return false;
            }
            if n == 0 {
                continue;
            }
            let mut tmp = [0u8; 65536];
            // SAFETY: tmp is a live buffer of the length given.
            let r = unsafe {
                libc::read(
                    self.fd.as_raw_fd(),
                    tmp.as_mut_ptr() as *mut libc::c_void,
                    tmp.len(),
                )
            };
            if r < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                self.eof = true;
                self.err = Some(e.to_string());
                return false;
            }
            if r == 0 {
                self.eof = true;
                return false;
            }
            self.buf.extend_from_slice(&tmp[..r as usize]);
            return true;
        }
    }
}

impl Iterator for Lines {
    type Item = Vec<u8>;

    fn next(&mut self) -> Option<Vec<u8>> {
        loop {
            if let Some(i) = self.buf.iter().position(|&b| b == b'\n') {
                if i > MAX_LINE {
                    self.err = Some("bufio.Scanner: token too long".into());
                    return None;
                }
                let mut line: Vec<u8> = self.buf.drain(..=i).collect();
                line.pop();
                if line.last() == Some(&b'\r') {
                    line.pop();
                }
                return Some(line);
            }
            if self.buf.len() > MAX_LINE {
                self.err = Some("bufio.Scanner: token too long".into());
                return None;
            }
            if self.eof {
                if self.buf.is_empty() || self.err.is_some() {
                    return None;
                }
                let mut line = std::mem::take(&mut self.buf);
                if line.last() == Some(&b'\r') {
                    line.pop();
                }
                return Some(line);
            }
            self.fill();
        }
    }
}

/// A count of running turns, and a flag when it reaches zero.
#[derive(Default)]
pub struct Turns {
    n: Mutex<usize>,
    zero: Flag,
}

impl Turns {
    pub fn add(&self) {
        *self.n.lock().unwrap_or_else(|e| e.into_inner()) += 1;
    }
    pub fn done(&self) {
        let mut n = self.n.lock().unwrap_or_else(|e| e.into_inner());
        *n = n.saturating_sub(1);
        if *n == 0 {
            self.zero.set();
        }
    }
    /// Whether no turn runs now.
    pub fn idle(&self) -> bool {
        *self.n.lock().unwrap_or_else(|e| e.into_inner()) == 0
    }
    /// Waits until no turn runs.
    pub fn wait(&self) {
        loop {
            if *self.n.lock().unwrap_or_else(|e| e.into_inner()) == 0 {
                return;
            }
            self.zero.wait(Some(Duration::from_millis(50)));
        }
    }
}

/// Go's ExitCode: the code, or -1 for a process a signal ended.
pub fn exit_code(st: &std::process::ExitStatus) -> i64 {
    st.code().map(i64::from).unwrap_or(-1)
}

/// Kills a process group, never pgid 0, 1 or below.
pub fn kill_group(pid: i32) {
    if pid > 1 {
        // SAFETY: kill takes plain integers.
        unsafe {
            libc::kill(-pid, libc::SIGKILL);
        }
    }
}

/// Kills one process.
pub fn kill_one(pid: i32) {
    if pid > 0 {
        // SAFETY: kill takes plain integers.
        unsafe {
            libc::kill(pid, libc::SIGKILL);
        }
    }
}

#[cfg(test)]
pub(crate) mod testutil {
    use super::*;
    use std::time::Instant;

    /// Collects events until one matches f or the time runs out.
    pub fn until(rx: &Receiver<Ev>, d: Duration, f: impl Fn(&Ev) -> bool) -> Vec<Ev> {
        let deadline = Instant::now() + d;
        let mut out = Vec::new();
        while let Some(left) = deadline.checked_duration_since(Instant::now()) {
            match rx.recv_timeout(left) {
                Ok(ev) => {
                    let hit = f(&ev);
                    out.push(ev);
                    if hit {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
        out
    }

    /// The testdata dir of the Go coppice.
    pub fn testdata(name: &str) -> String {
        format!(
            "{}/../coppice/testdata/adapters/{name}",
            env!("CARGO_MANIFEST_DIR")
        )
    }

    /// A scratch dir under TMPDIR.
    pub fn scratch(tag: &str) -> std::path::PathBuf {
        let base = std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into());
        let d = std::path::PathBuf::from(base).join(format!(
            "coppice-rs-adapter-{tag}-{}-{:?}",
            std::process::id(),
            Instant::now()
        ));
        let d = std::path::PathBuf::from(d.to_string_lossy().replace(
            |c: char| !c.is_ascii_alphanumeric() && c != '/' && c != '-',
            "",
        ));
        std::fs::create_dir_all(&d).unwrap();
        d
    }
}
