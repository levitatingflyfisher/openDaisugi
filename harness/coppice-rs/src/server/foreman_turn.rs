//! A turn is one fresh model run. The owner's message is logged, then one
//! run answers it over [tools][system][view][state][message]: the four
//! tools (MCP), the fixed system page, the chat's view, a short state block
//! and the message. The run keeps no session. Only its result text is
//! logged, as the foreman's reply; a thought never is. The twin of Go's
//! foreman_turn.go, foreman_turn_sprig.go and foreman_snapshot.go.

use std::collections::VecDeque;
use std::io::{Read, Write};
use std::os::unix::io::FromRawFd;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::sync::{Arc, Condvar, Mutex, OnceLock};
use std::thread;
use std::time::Duration;

use serde_json::{json, Value};

use super::foreman_chat::{default_base, first_line, ForemanChat, ERR_NO_CHAT};
use super::Server;
use crate::config;
use crate::foreman::chat::{self as fchat, Message, ViewLine};
use crate::foreman::compactor::{go_duration, CallUsage};
use crate::foreman::ctx::{Ctx, CtxErr};
use crate::foreman::tools::TOOL_NAMES;
use crate::foreman::turns::{self, TurnRecord};
use crate::gojson;

/// The system page of one turn of the chat-mode foreman.
pub const FOREMAN_TURN_PAGE: &str = include_str!("../../../coppice/skills/foreman-turn/SKILL.md");

/// How many bytes of lines may sit in <newer> before the snapshot is frozen
/// again.
pub const SNAPSHOT_GROW_BYTES: usize = 16 << 10;

/// Caps the raw tail of a turn's input.
pub const TURN_TAIL_BYTES: usize = 32 << 10;

/// How many model calls one sprig turn may make: a few tool calls and the
/// answer.
pub const TURN_MAX_TURNS: usize = 8;

/// The most turns that may wait.
const MAX_TURN_QUEUE: usize = 32;

/// The built-in tools of Claude Code that a turn turns off by name. The
/// hook of the turn refuses anything not named mcp__foreman__, so a
/// built-in that a later release adds is refused too.
pub const FOREMAN_BUILT_INS: [&str; 25] = [
    "Agent",
    "AskUserQuestion",
    "Bash",
    "BashOutput",
    "Edit",
    "ExitPlanMode",
    "Glob",
    "Grep",
    "KillShell",
    "LS",
    "Monitor",
    "MultiEdit",
    "NotebookEdit",
    "NotebookRead",
    "Read",
    "Skill",
    "SlashCommand",
    "Task",
    "TaskOutput",
    "TaskStop",
    "TodoWrite",
    "ToolSearch",
    "WebFetch",
    "WebSearch",
    "Write",
];

/// What a turn runner is given. Both runners use one layout: the system
/// prompt is the fixed page and the frozen snapshot of the view, and the
/// stdin is the view lines added since, the tail, the state and the
/// message.
pub struct TurnInput {
    pub system: Vec<u8>,
    pub stdin: Vec<u8>,
    /// The per-turn tool socket. The runner must call started with the pid
    /// of the process it starts, so the socket serves only that process
    /// and its children.
    pub socket: PathBuf,
    /// The working directory of the run: the turn dir.
    pub dir: PathBuf,
    /// Names the turn; the runner may use it to name its own files.
    pub n: i64,
    pub started: Arc<dyn Fn(i32) + Send + Sync>,
}

/// What a turn gives back.
#[derive(Clone, Debug, Default)]
pub struct TurnResult {
    /// The model's final answer. Nothing else of the run is kept.
    pub text: String,
    pub usage: CallUsage,
}

/// Runs one turn.
pub trait TurnRunner: Send + Sync {
    fn run(&self, ctx: &Ctx, input: TurnInput) -> Result<TurnResult, String>;
}

/// The queue of owner messages that wait for a turn.
#[derive(Default)]
pub struct TurnState {
    pub queue: VecDeque<i64>,
    pub running: bool,
    /// The runner; None means the one the [foreman] table picks.
    pub runner: Option<Arc<dyn TurnRunner>>,
}

/// Counts the threads that run turns, so close can wait for them.
#[derive(Default)]
pub struct WaitGroup {
    n: Mutex<i64>,
    cv: Condvar,
}

impl WaitGroup {
    pub fn add(&self) {
        *self.n.lock().unwrap_or_else(|e| e.into_inner()) += 1;
    }
    pub fn done(&self) {
        let mut g = self.n.lock().unwrap_or_else(|e| e.into_inner());
        *g -= 1;
        self.cv.notify_all();
    }
    /// Waits until the count is zero, or d passes. True when it is zero.
    pub fn wait(&self, d: Duration) -> bool {
        let g = self.n.lock().unwrap_or_else(|e| e.into_inner());
        let (g, _) = self
            .cv
            .wait_timeout_while(g, d, |n| *n > 0)
            .unwrap_or_else(|e| e.into_inner());
        *g <= 0
    }
}

/// One command the server runs for the foreman.
#[derive(Default)]
pub struct CmdOpts {
    pub dir: PathBuf,
    pub name: String,
    pub args: Vec<String>,
    /// Written to the command's standard input.
    pub stdin: Option<Vec<u8>>,
    /// Written to a pipe the command gets as descriptor 3, after it
    /// started.
    pub fd3: Option<Vec<u8>>,
    /// Called once with the pid, after the key is on its way.
    pub started: Option<Arc<dyn Fn(i32) + Send + Sync>>,
    /// The environment; None is the server's own.
    pub env: Option<Vec<(std::ffi::OsString, std::ffi::OsString)>>,
    /// How long the command has to stop after its group gets SIGTERM when
    /// the context ends. Zero kills the group at once.
    pub grace: Duration,
}

/// What a command gave.
#[derive(Default, Debug)]
pub struct CmdOut {
    pub stdout: String,
    pub stderr: String,
    pub code: i32,
}

/// Why a command failed: it did not start, or it ended in a way that is
/// not an exit code.
#[derive(Debug, PartialEq)]
pub enum RunErr {
    Start(String),
    Wait(String),
}

impl RunErr {
    pub fn text(&self) -> String {
        match self {
            RunErr::Start(s) | RunErr::Wait(s) => s.clone(),
        }
    }
}

/// In a child before exec: makes fd the child's descriptor 3, open across
/// the exec. dup2(3, 3) would leave close-on-exec set, so a descriptor that
/// is already 3 has the flag cleared instead.
pub fn fd_as_3(fd: i32) -> std::io::Result<()> {
    // SAFETY: plain calls on integer descriptors, async-signal-safe.
    unsafe {
        if fd == 3 {
            let fl = libc::fcntl(3, libc::F_GETFD);
            if fl < 0 || libc::fcntl(3, libc::F_SETFD, fl & !libc::FD_CLOEXEC) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            close_from_4();
            return Ok(());
        }
        if libc::dup2(fd, 3) < 0 {
            return Err(std::io::Error::last_os_error());
        }
    }
    close_from_4();
    Ok(())
}

/// In a child before exec: marks every descriptor above 3 close-on-exec,
/// so the program the child execs gets 0 to 3 and nothing else. A server
/// started from a shell or a CI runner can hold descriptors it inherited
/// without the flag. It marks them rather than closing them: std reports a
/// failed exec to the parent through a pipe of its own above 3, and that
/// pipe must stay open until the exec.
pub fn close_from_4() {
    // SAFETY: close_range and fcntl take plain integers and are
    // async-signal-safe.
    unsafe {
        const CLOSE_RANGE_CLOEXEC: u32 = 4;
        if libc::syscall(libc::SYS_close_range, 4u32, u32::MAX, CLOSE_RANGE_CLOEXEC) == 0 {
            return;
        }
        let max = libc::sysconf(libc::_SC_OPEN_MAX);
        let max = if max <= 0 {
            1024
        } else {
            max.min(65536) as i32
        };
        for fd in 4..max {
            let fl = libc::fcntl(fd, libc::F_GETFD);
            if fl >= 0 {
                libc::fcntl(fd, libc::F_SETFD, fl | libc::FD_CLOEXEC);
            }
        }
    }
}

/// The program path a start uses, as Go's exec.LookPath finds it on the
/// server's own PATH, or the error Go's Start gives.
fn look_path(name: &str) -> Result<PathBuf, String> {
    use std::os::unix::fs::PermissionsExt;
    if name.contains('/') {
        return Ok(PathBuf::from(name));
    }
    let path = std::env::var_os("PATH").unwrap_or_default();
    for d in std::env::split_paths(&path) {
        let d = if d.as_os_str().is_empty() {
            PathBuf::from(".")
        } else {
            d
        };
        let p = d.join(name);
        if std::fs::metadata(&p).is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0) {
            return Ok(p);
        }
    }
    Err(format!(
        "exec: {}: executable file not found in $PATH",
        crate::sys::go_quote(name)
    ))
}

/// Runs a command and returns its stdout, the head of its stderr and its
/// exit code. When the context ends the whole process group is stopped:
/// SIGTERM first when grace is set, then SIGKILL.
pub fn run_cmd(ctx: &Ctx, mut o: CmdOpts) -> (CmdOut, Option<RunErr>) {
    let prog = match look_path(&o.name) {
        Ok(p) => p,
        Err(e) => {
            if let Some(k) = o.fd3.as_mut() {
                k.fill(0);
            }
            return (CmdOut::default(), Some(RunErr::Start(e)));
        }
    };
    let mut cmd = Command::new(&prog);
    cmd.args(&o.args)
        .current_dir(&o.dir)
        .stdin(if o.stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
    if let Some(env) = &o.env {
        cmd.env_clear();
        cmd.envs(env.iter().map(|(k, v)| (k, v)));
    }
    let mut key_pipe: Option<(i32, i32)> = None;
    if o.fd3.is_some() {
        let mut fds = [0i32; 2];
        // SAFETY: fds points at two ints; pipe2 fills them.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
            let e = std::io::Error::last_os_error();
            return (
                CmdOut::default(),
                Some(RunErr::Start(format!(
                    "pipe: {}",
                    crate::sys::go_errno_text(&e)
                ))),
            );
        }
        let r = fds[0];
        key_pipe = Some((fds[0], fds[1]));
        // SAFETY: fd_as_3 runs in the child before exec and makes only
        // async-signal-safe calls.
        unsafe {
            cmd.pre_exec(move || fd_as_3(r));
        }
    }
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            if let Some(k) = o.fd3.as_mut() {
                k.fill(0);
            }
            if let Some((r, w)) = key_pipe {
                // SAFETY: both ends are ours and still open.
                unsafe {
                    libc::close(r);
                    libc::close(w);
                }
            }
            let text = match e.kind() {
                std::io::ErrorKind::NotFound | std::io::ErrorKind::PermissionDenied => format!(
                    "fork/exec {}: {}",
                    prog.display(),
                    crate::sys::go_errno_text(&e)
                ),
                _ => format!(
                    "fork/exec {}: {}",
                    prog.display(),
                    crate::sys::go_errno_text(&e)
                ),
            };
            return (CmdOut::default(), Some(RunErr::Start(text)));
        }
    };
    let pid = child.id() as i32;
    let mut key_writer = None;
    if let Some((r, w)) = key_pipe {
        // SAFETY: the read end is ours; the child has its copy as fd 3.
        unsafe { libc::close(r) };
        // The key moves to the writer, which zeroes it once it is written;
        // no copy stays in the options.
        let key = o.fd3.take().unwrap_or_default();
        key_writer = Some(thread::spawn(move || {
            // SAFETY: w is the write end this thread now owns.
            let mut f = unsafe { std::fs::File::from_raw_fd(w) };
            let mut key = key;
            let _ = f.write_all(&key);
            key.fill(0);
        }));
    }
    if let Some(input) = o.stdin.clone() {
        if let Some(mut si) = child.stdin.take() {
            thread::spawn(move || {
                let _ = si.write_all(&input);
            });
        }
    }
    let (out_tx, out_rx) = mpsc::channel::<Vec<u8>>();
    if let Some(mut so) = child.stdout.take() {
        thread::spawn(move || {
            let mut b = Vec::new();
            let _ = so.read_to_end(&mut b);
            let _ = out_tx.send(b);
        });
    }
    let (err_tx, err_rx) = mpsc::channel::<Vec<u8>>();
    if let Some(mut se) = child.stderr.take() {
        thread::spawn(move || {
            // The head of stderr is kept; the rest is read and dropped.
            let mut b = Vec::new();
            let mut buf = [0u8; 4096];
            loop {
                match se.read(&mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        let room = 4096usize.saturating_sub(b.len());
                        b.extend_from_slice(&buf[..n.min(room)]);
                    }
                }
            }
            let _ = err_tx.send(b);
        });
    }
    if let Some(f) = &o.started {
        f(pid);
    }
    // The watch: when the context ends, the group is stopped.
    let finished = Arc::new(crate::pane::Flag::default());
    let watch = {
        let (ctx, finished, grace) = (ctx.clone(), finished.clone(), o.grace);
        thread::spawn(move || loop {
            if finished.wait(Some(Duration::from_millis(20))) {
                return;
            }
            if ctx.err().is_some() {
                // SAFETY: kill takes plain integers.
                if grace.is_zero() {
                    unsafe { libc::kill(-pid, libc::SIGKILL) };
                } else {
                    unsafe { libc::kill(-pid, libc::SIGTERM) };
                    if !finished.wait(Some(grace)) {
                        unsafe { libc::kill(-pid, libc::SIGKILL) };
                    }
                }
                return;
            }
        })
    };
    let status = child.wait();
    finished.set();
    let _ = watch.join();
    if let Some(h) = key_writer {
        // The caller may clear the key once run_cmd returns; the writer is
        // done.
        let _ = h.join();
    }
    let delay = o.grace + Duration::from_secs(2);
    let stdout = out_rx.recv_timeout(delay);
    let stderr = err_rx.recv_timeout(delay);
    let mut res = CmdOut {
        stdout: String::from_utf8_lossy(stdout.as_deref().unwrap_or(&[])).into_owned(),
        stderr: String::from_utf8_lossy(stderr.as_deref().unwrap_or(&[])).into_owned(),
        code: 0,
    };
    let status = match status {
        Ok(s) => s,
        Err(e) => {
            return (res, Some(RunErr::Wait(crate::sys::go_errno_text(&e))));
        }
    };
    use std::os::unix::process::ExitStatusExt;
    res.code = status.code().unwrap_or(-1);
    if status.signal().is_none() && (stdout.is_err() || stderr.is_err()) {
        return (
            res,
            Some(RunErr::Wait(
                "exec: WaitDelay expired before I/O complete".into(),
            )),
        );
    }
    (res, None)
}

/// Go's text for an exit that is not 0: "exit status N" or "signal: killed".
fn exit_text(code: i32) -> String {
    if code == -1 {
        return "signal: killed".into();
    }
    format!("exit status {code}")
}

/// The command that runs `coppice foreman`: self_cmd when set, else this
/// executable.
pub fn self_cmd(over: &[String]) -> Vec<String> {
    if !over.is_empty() {
        return over.to_vec();
    }
    // A binary that was replaced while the server runs reads as
    // "/path/coppice (deleted)". The path of the new file is the same
    // without that suffix.
    let exe = std::env::current_exe()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_default();
    let exe = exe.strip_suffix(" (deleted)").unwrap_or(&exe).to_string();
    let exe = if exe.is_empty() {
        "coppice".to_string()
    } else {
        exe
    };
    vec![exe, "foreman".into()]
}

/// One word quoted for sh.
pub fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// The --mcp-config of a turn: one server, the relay.
pub fn mcp_config_json(self_cmd: &[String], sock: &Path) -> String {
    let mut args: Vec<String> = self_cmd[1..].to_vec();
    args.extend([
        "tools".to_string(),
        "--sock".to_string(),
        sock.to_string_lossy().into_owned(),
    ]);
    gojson::marshal(&gojson::map(vec![(
        "mcpServers",
        gojson::map(vec![(
            "foreman",
            gojson::map(vec![("command", json!(self_cmd[0])), ("args", json!(args))]),
        )]),
    )]))
}

/// The --settings of a turn: the hook that refuses every tool but the
/// foreman's four.
pub fn settings_json(self_cmd: &[String]) -> String {
    let mut words: Vec<String> = self_cmd.iter().map(|w| shell_quote(w)).collect();
    words.push("hook".into());
    let hook = gojson::map(vec![
        ("type", json!("command")),
        ("command", json!(words.join(" "))),
    ]);
    gojson::marshal(&gojson::map(vec![(
        "hooks",
        gojson::map(vec![(
            "PreToolUse",
            json!([gojson::map(vec![
                ("matcher", json!(".*")),
                ("hooks", json!([hook]))
            ])]),
        )]),
    )]))
}

/// The claude argv of a turn. Three walls keep the built-ins out: --tools
/// "" turns them all off, --disallowedTools names them, and the hook
/// refuses any tool that is not one of the four.
pub fn claude_argv(model: &str, system_file: &Path, mcp: &str, settings: &str) -> Vec<String> {
    let allowed: Vec<String> = TOOL_NAMES
        .iter()
        .map(|n| format!("mcp__foreman__{n}"))
        .collect();
    let mut args: Vec<String> = vec![
        "-p".into(),
        "--output-format".into(),
        "json".into(),
        "--system-prompt-file".into(),
        system_file.to_string_lossy().into_owned(),
        "--mcp-config".into(),
        mcp.into(),
        "--strict-mcp-config".into(),
        "--tools".into(),
        String::new(),
        "--allowedTools".into(),
        allowed.join(","),
        "--disallowedTools".into(),
        FOREMAN_BUILT_INS.join(","),
        "--settings".into(),
        settings.into(),
        "--no-session-persistence".into(),
    ];
    if !model.is_empty() {
        args.extend(["--model".to_string(), model.to_string()]);
    }
    args
}

/// The system prompt of a turn: the fixed page, then the frozen view.
pub fn claude_system(snapshot: &str) -> String {
    format!("{FOREMAN_TURN_PAGE}\n<chat>\n{snapshot}</chat>\n")
}

/// The stdin of a turn: the view lines added since the snapshot, then the
/// tail, the state and the message.
pub fn turn_stdin(added: &str, rest: &str) -> String {
    if added.is_empty() {
        return rest.to_string();
    }
    format!("<newer>\n{added}</newer>\n{rest}")
}

/// Writes the system prompt of a turn to t-<n>.system in the turn dir:
/// created new, never through a link, mode 0600.
pub fn write_system_file(dir: &Path, n: i64, text: &[u8]) -> Result<PathBuf, String> {
    use std::os::unix::fs::OpenOptionsExt;
    let path = dir.join(format!("t-{n}.system"));
    let _ = std::fs::remove_file(&path);
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .custom_flags(libc::O_NOFOLLOW)
        .mode(0o600)
        .open(&path)
        .map_err(|e| crate::sys::go_path_err("open", &path, &e))?;
    if let Err(e) = f.write_all(text) {
        let _ = std::fs::remove_file(&path);
        return Err(crate::sys::go_path_err("write", &path, &e));
    }
    Ok(path)
}

/// Removes the turn's system file when the turn ends.
struct RemoveOnDrop(PathBuf);

impl Drop for RemoveOnDrop {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

/// A turn on claude -p.
pub struct ClaudeTurn {
    pub bin: String,
    pub model: String,
    pub self_cmd: Vec<String>,
}

fn usage_of(v: &Value) -> CallUsage {
    CallUsage::of_answer(v.get("usage"))
}

impl TurnRunner for ClaudeTurn {
    fn run(&self, ctx: &Ctx, input: TurnInput) -> Result<TurnResult, String> {
        let me = self_cmd(&self.self_cmd);
        let sys_file = write_system_file(&input.dir, input.n, &input.system)
            .map_err(|e| format!("cannot write the turn's system prompt: {e}"))?;
        let _rm = RemoveOnDrop(sys_file.clone());
        let args = claude_argv(
            &self.model,
            &sys_file,
            &mcp_config_json(&me, &input.socket),
            &settings_json(&me),
        );
        let (out, err) = run_cmd(
            ctx,
            CmdOpts {
                dir: input.dir.clone(),
                name: self.bin.clone(),
                args,
                stdin: Some(input.stdin),
                started: Some(input.started.clone()),
                ..Default::default()
            },
        );
        if let Some(RunErr::Start(e)) = &err {
            return Err(format!("cannot start {}: {e}", self.bin));
        }
        if err.is_some() || out.code != 0 {
            if let Some(e) = ctx.err() {
                return Err(e.text().into());
            }
            let why = match &err {
                Some(e) => e.text(),
                None => exit_text(out.code),
            };
            return Err(format!(
                "claude ended with an error: {}",
                first_line(&out.stderr, &why)
            ));
        }
        let v: Value = serde_json::from_str(&out.stdout).unwrap_or(Value::Null);
        if v.get("type").and_then(Value::as_str) != Some("result") {
            return Err("claude gave an answer that is not a result".into());
        }
        let result = v.get("result").and_then(Value::as_str).unwrap_or("");
        if v.get("is_error").and_then(Value::as_bool) == Some(true) {
            return Err(format!(
                "claude reported an error: {}",
                first_line(result, "")
            ));
        }
        Ok(TurnResult {
            text: result.to_string(),
            usage: usage_of(&v),
        })
    }
}

/// A turn on an operator profile through sprig.
pub struct SprigTurn {
    pub s: std::sync::Weak<Server>,
    pub profile: String,
    pub model: String,
    pub self_cmd: Vec<String>,
}

impl TurnRunner for SprigTurn {
    fn run(&self, ctx: &Ctx, input: TurnInput) -> Result<TurnResult, String> {
        let Some(s) = self.s.upgrade() else {
            return Err("this server has closed".into());
        };
        let mut table = s.operator_bundle(ctx)?;
        let Some(e) = table.get_mut(&self.profile) else {
            return Err(format!(
                "the turn profile {} is not in the operator's agent profiles (a key variable it names may be unset)",
                crate::sys::go_quote(&self.profile)
            ));
        };
        if e.wire == "claude-code" {
            return Err(format!(
                "the turn profile {} uses the claude-code wire. Leave turn_profile empty to run the turn on Claude Code",
                crate::sys::go_quote(&self.profile)
            ));
        }
        let mut model = self.model.clone();
        if model.is_empty() {
            model = e.model.clone().unwrap_or_default();
        }
        let sys_path = write_system_file(&input.dir, input.n, &input.system)
            .map_err(|e| format!("cannot write the turn's system prompt: {e}"))?;
        let _rm = RemoveOnDrop(sys_path.clone());
        let mut args = vec!["--wire".to_string(), e.wire.clone()];
        let mut base = default_base(&e.wire).to_string();
        if let Some(b) = e.base_url.as_ref().filter(|b| !b.is_empty()) {
            base = b.clone();
        }
        if !base.is_empty() {
            args.extend(["--base-url".to_string(), base]);
        }
        if !model.is_empty() {
            args.extend(["--model".to_string(), model]);
        }
        let key = e
            .key
            .take()
            .filter(|k| !k.is_empty())
            .map(String::into_bytes);
        drop(table);
        if key.is_some() {
            args.extend(["--key-fd".to_string(), "3".to_string()]);
        }
        args.extend([
            "--tools".to_string(),
            "none".to_string(),
            "--mcp-config".to_string(),
            mcp_config_json(&self_cmd(&self.self_cmd), &input.socket),
            "--system-file".to_string(),
            sys_path.to_string_lossy().into_owned(),
            "--json".to_string(),
            "--max-turns".to_string(),
            TURN_MAX_TURNS.to_string(),
            "-".to_string(),
        ]);
        let (out, err) = run_cmd(
            ctx,
            CmdOpts {
                dir: input.dir.clone(),
                name: s.sprig_bin(),
                args,
                stdin: Some(input.stdin),
                fd3: key,
                env: Some(s.sprig_env()),
                started: Some(input.started.clone()),
                ..Default::default()
            },
        );
        if let Some(e) = ctx.err() {
            return Err(e.text().into());
        }
        match err {
            Some(RunErr::Wait(e)) => return Err(format!("sprig did not finish cleanly: {e}")),
            Some(RunErr::Start(e)) => return Err(format!("cannot start sprig: {e}")),
            None => {}
        }
        if out.code != 0 {
            return Err(format!(
                "sprig ended with an error: {}",
                first_line(&out.stderr, &exit_text(out.code))
            ));
        }
        let v: Value = serde_json::from_str(&out.stdout)
            .map_err(|_| "sprig gave an answer that is not JSON".to_string())?;
        Ok(TurnResult {
            text: v
                .get("answer")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
            usage: usage_of(&v),
        })
    }
}

/// The allowlist environment of a sprig coppice starts, from the variables
/// given: PATH, HOME, USER, LOGNAME, SHELL, LANG, LC_*, TZ, TERM, TMPDIR
/// and XDG_*, and the names in keep (none in a real server).
pub fn sprig_env_of(
    vars: impl Iterator<Item = (std::ffi::OsString, std::ffi::OsString)>,
    keep: &[String],
) -> Vec<(std::ffi::OsString, std::ffi::OsString)> {
    const NAMES: [&str; 9] = [
        "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM", "TMPDIR",
    ];
    vars.filter(|(k, _)| {
        let k = k.to_string_lossy();
        NAMES.contains(&k.as_ref())
            || k.starts_with("LC_")
            || k.starts_with("XDG_")
            || keep.iter().any(|n| *n == *k)
    })
    .collect()
}

/// The tags of a turn's input, opening or closing. Go's \b is ASCII.
fn turn_tag() -> &'static regex::Regex {
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| regex::Regex::new(r"</?(chat|newer|tail|state|message)(?-u:\b)").unwrap())
}

/// Puts a backslash after the "<" of each tag of the input, so text from
/// the log cannot close the block it sits in or open another.
pub fn escape_turn_tags(s: &str) -> String {
    turn_tag()
        .replace_all(s, |c: &regex::Captures| format!("<\\{}", &c[0][1..]))
        .into_owned()
}

/// Two spaces before each line of text, so a header of the tail can only
/// appear at column 0.
fn indent(text: &str) -> String {
    text.split('\n')
        .map(|l| format!("  {l}"))
        .collect::<Vec<_>>()
        .join("\n")
}

/// The messages the view does not cover, word for word, newest kept: at
/// most TURN_TAIL_BYTES, and always the newest whole. A message the cap
/// drops is in neither the view nor the tail: the gap names the first and
/// last of those.
pub fn tail_text(msgs: &[Message]) -> (String, Option<(i64, i64)>) {
    let mut parts: VecDeque<String> = VecDeque::new();
    let mut size = 0;
    let mut dropped = 0;
    for i in (0..msgs.len()).rev() {
        let m = &msgs[i];
        let p = format!(
            "[{} {} {}]\n{}\n",
            m.i,
            m.kind,
            m.date,
            indent(&escape_turn_tags(&m.text))
        );
        if size > 0 && size + p.len() > TURN_TAIL_BYTES {
            dropped = i + 1;
            break;
        }
        size += p.len();
        parts.push_front(p);
    }
    let text = format!(
        "<tail>\n{}</tail>\n",
        parts.into_iter().collect::<Vec<_>>().join("\n")
    );
    if dropped == 0 {
        return (text, None);
    }
    (text, Some((msgs[0].i, msgs[dropped - 1].i)))
}

/// The line of <state> that names the messages the model cannot see, or "".
pub fn gap_line(gap: Option<(i64, i64)>) -> String {
    match gap {
        None => String::new(),
        Some((a, b)) if a == b => format!("message log/{a} is not summarized yet\n"),
        Some((a, b)) => format!("messages log/{a} to log/{b} are not summarized yet\n"),
    }
}

/// Which lines of the view are the frozen snapshot and which were added
/// since. The first turn freezes a snapshot. After that it is frozen again
/// when its lines are no longer the first lines of the view, or when the
/// lines added since pass SNAPSHOT_GROW_BYTES.
pub fn split_view(snap: &mut super::foreman_chat::Snap, lines: &[ViewLine]) -> (String, String) {
    let mut keep = snap.set && snap.lines.len() <= lines.len();
    if keep {
        keep = snap.lines.iter().zip(lines).all(|(a, b)| a == b);
    }
    if keep {
        let grown: usize = lines[snap.lines.len()..].iter().map(|l| l.text.len()).sum();
        keep = grown <= SNAPSHOT_GROW_BYTES;
    }
    if !keep {
        snap.lines = lines.to_vec();
        snap.set = true;
    }
    let (mut snapshot, mut added) = (String::new(), String::new());
    for (i, l) in lines.iter().enumerate() {
        if i < snap.lines.len() {
            snapshot.push_str(&l.text);
        } else {
            added.push_str(&l.text);
        }
    }
    (snapshot, added)
}

impl Server {
    pub fn turns_lock(&self) -> std::sync::MutexGuard<'_, TurnState> {
        self.turns.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The environment of every sprig that coppice starts, the turn and the
    /// compactor: an allowlist, built here and nowhere else. PATH, HOME,
    /// USER, LOGNAME, SHELL, LANG, LC_*, TZ, TERM, TMPDIR and XDG_*; nothing
    /// else. No key variable, no proxy, no base URL and no model name
    /// reaches sprig, its model call or its MCP relay from coppice's own
    /// environment.
    pub fn sprig_env(&self) -> Vec<(std::ffi::OsString, std::ffi::OsString)> {
        sprig_env_of(std::env::vars_os(), &self.cfg.foreman.sprig_env_keep)
    }

    /// The input of the turn that answers message msg: the system prompt
    /// (the page and the frozen snapshot of the view) and the stdin (the
    /// lines added to the snapshot, the raw tail, the state and the
    /// message).
    fn turn_input(&self, fc: &ForemanChat, msg: &Message) -> Result<(Vec<u8>, Vec<u8>), String> {
        let workers = self.workers_state();
        let g = fc.lock();
        let Some(c) = g.as_ref() else {
            return Err(ERR_NO_CHAT.into());
        };
        let tail = c.tail()?;
        // The answered message is in <message> alone: not in the tail, and
        // not as a line of the view.
        let kept: Vec<Message> = tail.into_iter().filter(|m| m.i != msg.i).collect();
        let (tail_txt, gap) = tail_text(&kept);
        let mut b = tail_txt;
        b.push_str(&format!(
            "<state>\nnow: {}\n{}{workers}</state>\n",
            self.fm_date(),
            gap_line(gap)
        ));
        b.push_str(&format!(
            "<message>\n{}\n</message>\n",
            escape_turn_tags(&msg.text)
        ));
        let lines = c.view_text(escape_turn_tags, msg.i);
        let mut snap = fc.snap.lock().unwrap_or_else(|e| e.into_inner());
        let (snapshot, added) = split_view(&mut snap, &lines);
        Ok((
            claude_system(&snapshot).into_bytes(),
            turn_stdin(&added, &b).into_bytes(),
        ))
    }

    /// Adds the turn for owner message msg and starts the thread that runs
    /// the turns, one at a time, in order. The caller holds the turns lock
    /// and has checked for room.
    pub fn queue_turn_locked(
        self: &Arc<Server>,
        t: &mut TurnState,
        fc: &Arc<ForemanChat>,
        msg: i64,
    ) -> usize {
        t.queue.push_back(msg);
        if !t.running {
            t.running = true;
            self.turns_wg.add();
            let (s, f) = (self.clone(), fc.clone());
            thread::spawn(move || s.run_turns(f));
        }
        t.queue.len()
    }

    /// The refusal for a talk when MAX_TURN_QUEUE turns wait, or when the
    /// server has closed. The caller holds the turns lock.
    pub fn turn_room(&self, t: &TurnState) -> Result<(), String> {
        if self.is_closed() {
            return Err("this server has closed".into());
        }
        if t.queue.len() >= MAX_TURN_QUEUE {
            return Err(format!(
                "the foreman has {} turns waiting already. Wait for it to answer, then talk again",
                t.queue.len()
            ));
        }
        Ok(())
    }

    fn turn_runner(self: &Arc<Server>) -> Arc<dyn TurnRunner> {
        if let Some(r) = &self.turns_lock().runner {
            return r.clone();
        }
        let f = &self.cfg.foreman;
        // The default profile is Claude Code, as it is for the compactor.
        if !f.turn_profile.is_empty() && f.turn_profile != config::DEFAULT_COMPACTOR_PROFILE {
            return Arc::new(SprigTurn {
                s: Arc::downgrade(self),
                profile: f.turn_profile.clone(),
                model: f.turn_model.clone(),
                self_cmd: f.self_cmd.clone(),
            });
        }
        Arc::new(ClaudeTurn {
            bin: self.claude_bin(),
            model: f.turn_model.clone(),
            self_cmd: f.self_cmd.clone(),
        })
    }

    /// Runs the queued turns, oldest first, until the queue is empty or the
    /// server closes.
    fn run_turns(self: Arc<Server>, fc: Arc<ForemanChat>) {
        loop {
            let msg = {
                let mut t = self.turns_lock();
                match t.queue.pop_front() {
                    None => {
                        t.running = false;
                        break;
                    }
                    Some(m) => m,
                }
            };
            if self.talk_stop.is_set() {
                self.turns_lock().running = false;
                break;
            }
            self.run_turn(&fc, msg);
        }
        self.turns_wg.done();
    }

    /// Runs the turn for owner message msg and logs what came of it. The
    /// end record of turns.jsonl is the last thing a turn writes: by then
    /// the tool socket is closed and the reply or the note is in the log.
    fn run_turn(self: &Arc<Server>, fc: &Arc<ForemanChat>, msg_id: i64) {
        let mut rec = TurnRecord {
            msg: msg_id,
            started: self.fm_date(),
            status: turns::TURN_RUNNING.into(),
            ..Default::default()
        };
        let _ = turns::append_turn(&fc.dir, &rec);
        self.send_messages_event("", true);
        let mut ts: Option<Arc<super::foreman_tools::TurnSocket>> = None;
        let mut end = |ts: &mut Option<Arc<super::foreman_tools::TurnSocket>>,
                       status: &str,
                       reply: &str,
                       note: &str,
                       usage: Option<CallUsage>| {
            if let Some(t) = ts.take() {
                t.close();
            }
            if !reply.is_empty() {
                if let Err(e) = self.chat_append(fc, fchat::KIND_FOREMAN, reply) {
                    self.note_once(&format!("The foreman's log could not take its reply: {e}"));
                }
            }
            if !note.is_empty() {
                if let Err(e) = self.chat_append(fc, fchat::KIND_NOTE, note) {
                    self.note_once(&format!("The foreman's log could not take a note: {e}"));
                }
                self.note(note, "");
            }
            rec.ended = self.fm_date();
            rec.status = status.into();
            rec.usage = usage;
            let _ = turns::append_turn(&fc.dir, &rec);
            self.send_messages_event("", true);
        };
        let fail_note =
            |why: &str| format!("The foreman's turn failed: {why} Say it again to try again.");
        let msg = match fc.lock().as_ref() {
            Some(c) => c.message(msg_id),
            None => Err(ERR_NO_CHAT.to_string()),
        };
        let msg = match msg {
            Ok(m) => m,
            Err(e) => return end(&mut ts, turns::TURN_FAILED, "", &fail_note(&e), None),
        };
        let (system, stdin) = match self.turn_input(fc, &msg) {
            Ok(x) => x,
            Err(e) => return end(&mut ts, turns::TURN_FAILED, "", &fail_note(&e), None),
        };
        match self.open_turn_socket(fc, msg_id) {
            Ok(t) => ts = Some(t),
            Err(e) => {
                return end(
                    &mut ts,
                    turns::TURN_FAILED,
                    "",
                    &fail_note(&format!("the tool socket could not open: {e}")),
                    None,
                )
            }
        }
        let sock = ts
            .as_ref()
            .map(|t| t.path().to_path_buf())
            .unwrap_or_default();
        let ctx = Ctx::background().with_timeout(self.cfg.foreman.turn_timeout);
        let done = Arc::new(crate::pane::Flag::default());
        {
            let (stop, ctx, done) = (self.talk_stop.clone(), ctx.clone(), done.clone());
            thread::spawn(move || loop {
                if stop.wait(Some(Duration::from_millis(50))) {
                    ctx.cancel();
                    return;
                }
                if done.is_set() || ctx.err().is_some() {
                    return;
                }
            });
        }
        let started = {
            let t = ts.clone();
            Arc::new(move |pid: i32| {
                if let Some(t) = &t {
                    t.started(pid)
                }
            })
        };
        let res = self.turn_runner().run(
            &ctx,
            TurnInput {
                system,
                stdin,
                socket: sock,
                dir: self.turn_dir(),
                n: msg_id,
                started,
            },
        );
        done.set();
        match res {
            Err(e) => {
                if self.talk_stop.is_set() {
                    return end(
                        &mut ts,
                        turns::TURN_FAILED,
                        "",
                        "The foreman's turn was stopped when coppice stopped. Say it again to try again.",
                        None,
                    );
                }
                if ctx.err() == Some(CtxErr::DeadlineExceeded) {
                    return end(
                        &mut ts,
                        turns::TURN_TIMEOUT,
                        "",
                        &format!(
                            "The foreman's turn timed out: it ran longer than {} and was stopped. Say it again to try again.",
                            go_duration(self.cfg.foreman.turn_timeout)
                        ),
                        None,
                    );
                }
                end(
                    &mut ts,
                    turns::TURN_FAILED,
                    "",
                    &fail_note(&format!("{e}.")),
                    None,
                )
            }
            Ok(r) => {
                let text = r.text.trim().to_string();
                end(&mut ts, turns::TURN_OK, &text, "", Some(r.usage))
            }
        }
    }
}
