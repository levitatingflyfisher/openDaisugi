//! One tool-call hook answered the way `python -m opendaisugi.warden` does,
//! with the same flags, stdin, stdout, stderr, exit code and log lines:
//! the whole of `daisugi-warden`, and `daisugi warden check`.
//!
//! The binary runs twice. The parent reads at most one byte past the
//! payload cap from stdin, starts itself as a child with the same argv and
//! a fresh nonce, and waits. The child decides the call and writes its
//! verdict, with the nonce, only to a pipe the parent passes as its fd 3;
//! on its own stdout and exit code it writes the format's deny. So a child
//! started by anyone else reads as a deny to any host, and a process that
//! merely sets the child's variable, with no such pipe, is a parent.
//!
//! The parent gives the child's verdict only when a complete frame with
//! its nonce came back before the deadline. Any other end (a crash, a
//! signal, a stack overflow, running out of memory, running past the
//! deadline) becomes a deny in the host format argparse reads. A host
//! reads an abnormal exit as an allow, so no path may end in one.
//!
//! DAISUGI_WARDEN_TRACE names a file that gets one JSON line per call saying
//! how the call was answered, and why. It never changes stdout, stderr or
//! the exit code.

use super::resident::{self, RESIDENT_ENV};
use super::{self as warden, py, pyjson, statefile, WardenResult};
use std::ffi::{OsStr, OsString};
use std::io::{Read, Write};
use std::os::fd::FromRawFd;
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::time::{Duration, Instant};

/// Names the child's nonce. It is removed from the environment the child
/// decides with, so nothing the warden starts inherits it.
const CHILD_ENV: &str = "DAISUGI_WARDEN_WORKER";

/// The fd the child writes its frame to.
const FRAME_FD: i32 = 3;

/// The last SIGINT or SIGTERM a hook process took while it waited for its
/// child (0 when none). A process that dies by a signal gives its host no
/// verdict, and a host reads that as an allow, so the call is denied.
static SIGNALLED: std::sync::atomic::AtomicI32 = std::sync::atomic::AtomicI32::new(0);

extern "C" fn on_signal(sig: libc::c_int) {
    SIGNALLED.store(sig, std::sync::atomic::Ordering::SeqCst);
}

pub(super) fn signalled() -> i32 {
    SIGNALLED.load(std::sync::atomic::Ordering::SeqCst)
}

/// Takes SIGINT and SIGTERM while it lives, and puts the old actions back.
struct SignalWatch {
    old: [libc::sigaction; 2],
}

impl SignalWatch {
    fn new() -> SignalWatch {
        SIGNALLED.store(0, std::sync::atomic::Ordering::SeqCst);
        let mut old: [libc::sigaction; 2] = unsafe { std::mem::zeroed() };
        for (i, sig) in [libc::SIGINT, libc::SIGTERM].into_iter().enumerate() {
            unsafe {
                let mut act: libc::sigaction = std::mem::zeroed();
                act.sa_sigaction = on_signal as extern "C" fn(libc::c_int) as usize;
                libc::sigemptyset(&mut act.sa_mask);
                // No SA_RESTART: a wait ends when the signal comes.
                act.sa_flags = 0;
                libc::sigaction(sig, &act, &mut old[i]);
            }
        }
        SignalWatch { old }
    }
}

impl Drop for SignalWatch {
    fn drop(&mut self) {
        for (i, sig) in [libc::SIGINT, libc::SIGTERM].into_iter().enumerate() {
            unsafe { libc::sigaction(sig, &self.old[i], std::ptr::null_mut()) };
        }
        // The flag belongs to the watch: a later wait must not read this one's signal.
        SIGNALLED.store(0, std::sync::atomic::Ordering::SeqCst);
    }
}

/// The longest one poll waits, so a signal is seen within this time.
const POLL_SLICE_MS: i32 = 50;

/// Answers one hook call and ends the process. `args` are the warden's own
/// flags; `prefix` is what goes before them to start this binary as the
/// child (nothing for `daisugi-warden`, `warden check` for `daisugi`).
pub fn main(args: Vec<OsString>, prefix: &[&str]) -> ! {
    // stderr is part of the warden's contract: a panic must never print there.
    std::panic::set_hook(Box::new(|_| {}));
    let nonce = std::env::var(CHILD_ENV).unwrap_or_default();
    if !nonce.is_empty() && fd_is_pipe(FRAME_FD) {
        child(&args, &nonce);
    }
    // The deny this process gives if anything below fails, built before
    // stdin is read.
    let fallback = warden::backstop_deny(&args, "the warden's parent process failed on this call");
    let res = std::panic::catch_unwind(|| {
        let stdin = read_stdin();
        if super::fenced::fenced() {
            // Inside daisugi fence the warden's state is read only: the
            // resident warden, outside the fence, decides and writes.
            return fenced_with_watch(&args, &stdin);
        }
        let trace = std::env::var_os("DAISUGI_WARDEN_TRACE").filter(|t| !t.is_empty());
        let (res, how) = run_child(&args, prefix, &stdin);
        if let Some(t) = &trace {
            write_trace(t, how, &res.why, res.exit);
        }
        res
    })
    .unwrap_or(fallback);
    emit(&res);
}

/// The fenced check under the same signal watch as the hook parents: a host
/// that times the hook out sends a signal while the resident warden is stuck,
/// and a process that dies by it gives no verdict, which a host reads as go
/// on. The wait on the socket sees the flag and gives the mode's verdict.
fn fenced_with_watch(args: &[OsString], stdin: &[u8]) -> WardenResult {
    let _watch = SignalWatch::new();
    super::fenced::fenced_check(args, stdin)
}

/// stdin, at most one byte past the payload cap: a longer payload is denied
/// unread by the child, as the oracle denies it.
fn read_stdin() -> Vec<u8> {
    let mut stdin = Vec::new();
    let limit = (warden::MAX_PAYLOAD_BYTES + 1) as u64;
    if std::io::stdin().take(limit).read_to_end(&mut stdin).is_err() {
        stdin.clear();
    }
    stdin
}

/// Writes a result and ends the process with its code, running no exit
/// handlers: a thread may still be inside Z3, and its static teardown must
/// not race it and turn a clean answer into a crash.
fn emit(res: &WardenResult) -> ! {
    let _ = std::io::stdout().write_all(&res.stdout);
    let _ = std::io::stdout().flush();
    let _ = std::io::stderr().write_all(&res.stderr);
    let _ = std::io::stderr().flush();
    unsafe { libc::_exit(res.exit) }
}

fn fd_is_pipe(fd: i32) -> bool {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    unsafe { libc::fstat(fd, &mut st) == 0 && (st.st_mode & libc::S_IFMT) == libc::S_IFIFO }
}

/// The child: decide, write the frame to fd 3, then give the format's deny
/// on its own stdout and exit code.
fn child(args: &[OsString], nonce: &str) -> ! {
    std::env::remove_var(CHILD_ENV);
    let resident = std::env::var_os(RESIDENT_ENV).is_some();
    std::env::remove_var(RESIDENT_ENV);
    // Nothing the warden starts may hold the frame pipe open.
    unsafe { libc::fcntl(FRAME_FD, libc::F_SETFD, libc::FD_CLOEXEC) };
    let stdin = read_stdin();
    // A note about a refused state file (GR-R-3) is kept for the answer.
    statefile::collect_notes();
    let mut res = if resident {
        // A resident server's child: stdin is the request, not a payload.
        resident_child(&stdin)
    } else {
        match std::panic::catch_unwind(|| warden::run_process(args.to_vec(), &stdin)) {
            Ok(r) => r,
            Err(_) => {
                let mut r = warden::backstop_deny(args, "the Rust warden failed on this call");
                r.why = "a panic".into();
                r
            }
        }
    };
    let notes = statefile::take_notes();
    if resident {
        res.notes = notes;
    } else if !notes.is_empty() {
        let mut s = notes;
        s.extend_from_slice(&res.stderr);
        res.stderr = s;
    }
    let frame = serde_json::json!({
        "notes": hex(&res.notes),
        "stdout": hex(&res.stdout),
        "stderr": hex(&res.stderr),
        "out_stdout": hex(res.out_stdout.as_bytes()),
        "out_stderr": hex(res.out_stderr.as_bytes()),
        "help": res.help,
        "exit": res.exit,
        "native": res.native,
        "why": res.why,
        "done": true,
        "nonce": nonce,
    });
    let mut f = unsafe { std::fs::File::from_raw_fd(FRAME_FD) };
    let _ = f.write_all(frame.to_string().as_bytes());
    drop(f);
    emit(&warden::backstop_deny(args, "this process decided the call for its parent warden, which gives the verdict"))
}

/// Decides a resident server's call in this child: the request on stdin,
/// the environment the server gave (already pane-free).
fn resident_child(stdin: &[u8]) -> WardenResult {
    let q = match resident::decode_request(stdin) {
        Some(q) => q,
        None => {
            let mut r = warden::backstop_deny_text(&[], "the Rust warden could not read its server's request");
            r.why = "resident request".into();
            return r;
        }
    };
    let mut env = std::collections::HashMap::new();
    for (k, v) in std::env::vars_os() {
        use std::os::unix::ffi::OsStrExt;
        env.insert(py::text::fsdecode(k.as_bytes()), py::text::fsdecode(v.as_bytes()));
    }
    match std::panic::catch_unwind(|| warden::run_resident(&q, &env)) {
        Ok(r) => r,
        Err(_) => {
            let mut r = warden::backstop_deny_text(&q.argv, "the Rust warden failed on this call");
            r.why = "a panic".into();
            r
        }
    }
}

/// Decides a resident call in a child of this binary (`daisugi warden
/// check`, fed the request on stdin), as a hook call is decided: any
/// abnormal end of the child is the format's deny.
pub fn run_resident_child(q: &resident::Request) -> WardenResult {
    let deny = |why: String| {
        let mut res = warden::backstop_deny_text(&q.argv, &why);
        res.why = why;
        (res, "backstop")
    };
    let exe = match std::env::current_exe() {
        Ok(e) => e,
        Err(e) => return deny(format!("the warden cannot find its own binary: {e}")).0,
    };
    let deadline_s = warden::child_deadline_text(&q.argv);
    spawn_child(&exe, &[], &["warden", "check"], &resident::encode_request(q), deadline_s, &deny, true).0
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Option<Vec<u8>> {
    if s.len() % 2 != 0 {
        return None;
    }
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok()).collect()
}

fn random_nonce() -> String {
    let mut b = [0u8; 16];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut b);
    }
    hex(&b)
}

/// Starts the child and returns its verdict, or a deny, and how the call
/// was answered for the trace.
fn run_child(args: &[OsString], prefix: &[&str], stdin: &[u8]) -> (WardenResult, &'static str) {
    let deny = |why: String| {
        let mut res = warden::backstop_deny(args, &why);
        res.why = why;
        (res, "backstop")
    };
    match std::env::current_exe() {
        Ok(exe) => run_child_as(&exe, args, prefix, stdin),
        Err(e) => deny(format!("the warden cannot find its own binary: {e}")),
    }
}

/// `run_child` with the child's binary named.
fn run_child_as(exe: &std::path::Path, args: &[OsString], prefix: &[&str], stdin: &[u8]) -> (WardenResult, &'static str) {
    let deny = |why: String| {
        let mut res = warden::backstop_deny(args, &why);
        res.why = why;
        (res, "backstop")
    };
    spawn_child(exe, args, prefix, stdin, warden::child_deadline_s(args), &deny, false)
}

/// Starts `exe prefix args` as the child, feeds it `stdin`, and returns
/// its verdict, or `deny`'s. A resident child gets RESIDENT_ENV; any other
/// child never does, whatever this process was started with.
fn spawn_child(
    exe: &std::path::Path,
    args: &[OsString],
    prefix: &[&str],
    stdin: &[u8],
    deadline_s: f64,
    deny: &dyn Fn(String) -> (WardenResult, &'static str),
    resident: bool,
) -> (WardenResult, &'static str) {
    let mut fds = [0i32; 2];
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        return deny("the warden could not make its frame pipe".into());
    }
    let (frame_r, frame_w) = (fds[0], fds[1]);
    // A hook process denies on an interrupt or a terminate; the resident
    // server keeps its own signal behaviour.
    let _watch = if resident { None } else { Some(SignalWatch::new()) };
    // The mode the child checks in: a signal ends in that mode's verdict.
    let audit = !resident && warden::mode_for(args) == "audit";
    let nonce = random_nonce();
    let mut cmd = Command::new(exe);
    // Python sizes argparse's text by the terminal on its stdout. The
    // child's stdout is a pipe, so it gets this process's width.
    cmd.env_remove(warden::TTY_ENV);
    if let Some(cols) = tty_columns() {
        cmd.env(warden::TTY_ENV, cols.to_string());
    }
    cmd.env_remove(RESIDENT_ENV);
    if resident {
        cmd.env(RESIDENT_ENV, "1");
    }
    cmd.args(prefix)
        .args(args)
        .env(CHILD_ENV, &nonce)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    unsafe {
        cmd.pre_exec(move || {
            // dup2 clears close-on-exec on the new fd.
            if libc::dup2(frame_w, FRAME_FD) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    super::procgroup::in_group(&mut cmd);
    let spawned = cmd.spawn();
    unsafe { libc::close(frame_w) };
    let mut child = match spawned {
        Ok(c) => c,
        Err(e) => {
            unsafe { libc::close(frame_r) };
            return deny(format!("the warden's child did not start: {e}"));
        }
    };
    let deadline = Instant::now() + Duration::from_secs_f64(deadline_s);
    let mut frame_file = unsafe { std::fs::File::from_raw_fd(frame_r) };
    // The pipe threads are not joined on the answer path: once the frame
    // is in, nothing the child still does can change the verdict.
    let spawn = |name: &str| std::thread::Builder::new().name(name.into());
    let mut sin = child.stdin.take();
    let data = stdin.to_vec();
    let feed = spawn("feed").spawn(move || {
        if let Some(s) = sin.as_mut() {
            let _ = s.write_all(&data);
        }
    });
    let mut out_pipe = child.stdout.take();
    let out_t = spawn("stdout").spawn(move || drain(out_pipe.as_mut().map(|p| p as &mut dyn Read)));
    let mut err_pipe = child.stderr.take();
    let err_t = spawn("stderr").spawn(move || drain(err_pipe.as_mut().map(|p| p as &mut dyn Read)));
    let err_t = match (feed, out_t, err_t) {
        (Ok(_), Ok(_), Ok(e)) => e,
        _ => {
            super::procgroup::kill_group(&mut child);
            return deny("the warden could not start a thread".to_string());
        }
    };
    // The child closes the frame pipe once its frame is written, and has
    // done all its work by then: only its own deny and its exit are left.
    // So a complete frame is the answer at once, and the child and its
    // process group are killed and reaped, so none of them outlives this
    // call.
    if let Some(frame) = read_until(&mut frame_file, deadline) {
        if let Some(res) = parse_frame(&frame, &nonce) {
            super::procgroup::kill_group(&mut child);
            let how = if res.native { "native" } else { "undecided" };
            return (res, how);
        }
    }
    let status = match wait_until(&mut child, deadline, deadline_s) {
        // A signal in audit mode: an audit warden never blocks, so the
        // format's allow body and exit 0 (the oracle's audit escape).
        Err(why) if signalled() != 0 && audit => {
            let mut res = warden::audit_escape(args, &why);
            res.native = true;
            return (res, "signal");
        }
        Err(why) => return deny(why),
        Ok(s) => s,
    };
    let stderr = err_t.join().unwrap_or_default();
    use std::os::unix::process::ExitStatusExt;
    let first = String::from_utf8_lossy(&stderr).lines().next().unwrap_or("").chars().take(200).collect::<String>();
    match (status.code(), status.signal()) {
        (_, Some(sig)) => deny(format!("the warden's child was ended by signal {sig}")),
        (Some(code), _) => deny(format!("the warden's child wrote no complete answer (exit {code}: {first})")),
        _ => deny("the warden's child wrote no complete answer".into()),
    }
}

/// Everything read from `f` up to its end, or `None` when the deadline
/// comes first.
fn read_until(f: &mut std::fs::File, deadline: Instant) -> Option<Vec<u8>> {
    use std::os::fd::AsRawFd;
    let mut b = Vec::new();
    let mut chunk = [0u8; 65536];
    loop {
        let now = Instant::now();
        if now >= deadline || signalled() != 0 {
            return None;
        }
        let ms = (deadline - now).as_millis().saturating_add(1).min(POLL_SLICE_MS as u128) as i32;
        let mut p = libc::pollfd { fd: f.as_raw_fd(), events: libc::POLLIN, revents: 0 };
        match unsafe { libc::poll(&mut p, 1, ms) } {
            0 => continue,
            n if n < 0 => {
                if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                return Some(b);
            }
            _ => {}
        }
        match f.read(&mut chunk) {
            Ok(0) => return Some(b),
            Ok(n) => b.extend_from_slice(&chunk[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(_) => return Some(b),
        }
    }
}

/// Waits for the child until the deadline, and kills it there. A pidfd
/// wakes poll() the moment the child ends; with none (a kernel before
/// 5.3), the child is looked at every millisecond.
fn wait_until(child: &mut Child, deadline: Instant, deadline_s: f64) -> Result<ExitStatus, String> {
    // The child is not reaped yet, so its pid still names it.
    let pidfd = unsafe { libc::syscall(libc::SYS_pidfd_open, child.id() as libc::pid_t, 0) } as i32;
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break Ok(s),
            Ok(None) => {}
            Err(e) => break Err(format!("the warden lost its child: {e}")),
        }
        let now = Instant::now();
        let sig = signalled();
        if sig != 0 {
            super::procgroup::kill_group(child);
            break Err(format!("the warden was ended by signal {sig}"));
        }
        if now >= deadline {
            super::procgroup::kill_group(child);
            break Err(format!("no answer within {deadline_s} s"));
        }
        if pidfd >= 0 {
            // Round up, so the last wait does not end just short of the deadline.
            let ms = (deadline - now).as_millis().saturating_add(1).min(POLL_SLICE_MS as u128) as i32;
            let mut p = libc::pollfd { fd: pidfd, events: libc::POLLIN, revents: 0 };
            unsafe { libc::poll(&mut p, 1, ms) };
        } else {
            std::thread::sleep(Duration::from_millis(1));
        }
    };
    if pidfd >= 0 {
        unsafe { libc::close(pidfd) };
    }
    status
}

fn drain(p: Option<&mut dyn Read>) -> Vec<u8> {
    let mut b = Vec::new();
    if let Some(p) = p {
        let _ = p.read_to_end(&mut b);
    }
    b
}

/// The child's verdict, when the frame is complete and carries the nonce.
fn parse_frame(raw: &[u8], nonce: &str) -> Option<WardenResult> {
    let v: serde_json::Value = serde_json::from_slice(raw).ok()?;
    if v.get("done")?.as_bool()? != true || v.get("nonce")?.as_str()? != nonce {
        return None;
    }
    let exit = v.get("exit")?.as_i64()?;
    if exit != 0 && exit != 2 {
        return None;
    }
    Some(WardenResult {
        stdout: unhex(v.get("stdout")?.as_str()?)?,
        stderr: unhex(v.get("stderr")?.as_str()?)?,
        exit: exit as i32,
        native: v.get("native")?.as_bool()?,
        why: v.get("why")?.as_str()?.to_string(),
        // A frame from an older child has no outcome text; no reply is
        // built from it (only a resident server reads these).
        out_stdout: text(v.get("out_stdout"))?,
        out_stderr: text(v.get("out_stderr"))?,
        help: v.get("help").and_then(|h| h.as_bool()).unwrap_or(false),
        notes: v.get("notes").and_then(|n| n.as_str()).and_then(unhex).unwrap_or_default(),
    })
}

/// A hex-coded UTF-8 frame field, empty when absent.
fn text(v: Option<&serde_json::Value>) -> Option<String> {
    match v {
        None => Some(String::new()),
        Some(x) => String::from_utf8(unhex(x.as_str()?)?).ok(),
    }
}

/// `os.get_terminal_size(1).columns`, when stdout is a terminal.
fn tty_columns() -> Option<u16> {
    let mut ws: libc::winsize = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::ioctl(1, libc::TIOCGWINSZ, &mut ws) };
    (rc == 0).then_some(ws.ws_col)
}

fn write_trace(path: &OsStr, how: &str, why: &str, exit: i32) {
    use std::os::unix::fs::OpenOptionsExt;
    let line = pyjson::Object::new().with("path", how).with("why", why).with("exit", exit as i64);
    if let Ok(mut f) = std::fs::OpenOptions::new().append(true).create(true).mode(0o600).open(path) {
        let _ = f.write_all(format!("{}\n", pyjson::dumps(&pyjson::Value::Obj(line), false)).as_bytes());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A script that plays the child: it writes its pid beside itself,
    /// writes `body`, with its nonce for NONCE, to the frame pipe, closes
    /// it, then runs `after`.
    fn fake_child(tag: &str, body: &str, after: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("dg-hook-{}-{tag}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let exe = dir.join("child.sh");
        let script = format!(
            "#!/bin/sh\necho $$ > \"$(dirname \"$0\")/pid\"\nprintf '%s' \"$(printf '%s' '{body}' | sed \"s/NONCE/${CHILD_ENV}/\")\" >&3\nexec 3>&-\n{after}\n"
        );
        std::fs::write(&exe, script).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&exe, std::fs::Permissions::from_mode(0o700)).unwrap();
        exe
    }

    /// The signal flag is the process's, so the tests that wait on a child
    /// take turns.
    fn serial() -> std::sync::MutexGuard<'static, ()> {
        static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
        LOCK.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn claude() -> Vec<OsString> {
        vec!["--format".into(), "claude".into()]
    }

    /// A complete frame is the answer as soon as the pipe closes, even
    /// when the child then hangs, and the child does not outlive the call.
    #[test]
    fn a_complete_frame_answers_at_once() {
        let _g = serial();
        let body = r#"{"stdout":"7b7d0a","stderr":"","exit":0,"native":true,"why":"","done":true,"nonce":"NONCE"}"#;
        let exe = fake_child("answers", body, "exec sleep 30");
        let t0 = Instant::now();
        let (res, how) = run_child_as(&exe, &claude(), &[], b"");
        assert!(t0.elapsed() < Duration::from_secs(5), "{:?}", t0.elapsed());
        assert_eq!((res.exit, res.stdout.as_slice(), how), (0, &b"{}\n"[..], "native"));
        let pid_file = exe.parent().unwrap().join("pid");
        let gone = (0..100).any(|_| {
            let pid = std::fs::read_to_string(&pid_file).unwrap_or_default();
            let st = std::fs::read_to_string(format!("/proc/{}/stat", pid.trim())).unwrap_or_default();
            if !pid.trim().is_empty() && (st.is_empty() || st.contains(") Z ")) {
                return true;
            }
            std::thread::sleep(Duration::from_millis(20));
            false
        });
        let _ = std::fs::remove_dir_all(exe.parent().unwrap());
        assert!(gone, "the child is still running after its frame was taken");
    }

    /// A frame with the wrong nonce, or cut short, is no answer.
    #[test]
    fn an_incomplete_frame_is_a_deny() {
        let _g = serial();
        for (i, body) in [
            r#"{"stdout":"","stderr":"","exit":0,"native":true,"why":"","done":true,"nonce":"other"}"#,
            r#"{"stdout":"","stderr":"","exit":0,"native":true,"#,
            r#"{"stdout":"","stderr":"","exit":0,"native":true,"why":"","done":false,"nonce":"NONCE"}"#,
        ]
        .iter()
        .enumerate()
        {
            let exe = fake_child(&format!("bad{i}"), body, "exit 0");
            let (res, how) = run_child_as(&exe, &claude(), &[], b"");
            let _ = std::fs::remove_dir_all(exe.parent().unwrap());
            assert_eq!((res.exit, how), (2, "backstop"), "{body}");
        }
    }

    /// An interrupt or a terminate while the child works is a deny with
    /// exit 2 in enforce mode, not the parent's death by signal. The child is
    /// killed. In audit mode it is the format's allow body and exit 0.
    #[test]
    fn a_signal_during_a_check_is_a_deny() {
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let (res, how) = signal_during_a_check(sig, "enforce");
            assert_eq!((res.exit, how), (2, "backstop"), "signal {sig}");
            assert!(res.why.contains("signal"), "{}", res.why);
        }
    }

    #[test]
    fn a_signal_during_a_check_in_audit_mode_allows() {
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let (res, how) = signal_during_a_check(sig, "audit");
            assert_eq!((res.exit, how), (0, "signal"), "signal {sig}");
            assert_eq!(res.out_stdout, r#"{"continue": true}"#);
            assert!(res.why.contains("signal"), "{}", res.why);
        }
    }

    /// Holds a child that never answers and signals this process once the
    /// child is running (its pid file exists), so no clock decides it.
    fn signal_during_a_check(sig: libc::c_int, mode: &str) -> (WardenResult, &'static str) {
        let _g = serial();
        signal_run(sig, mode, &["--mode", mode], |_| {})
    }

    /// `signal_run` with `flags` as the warden's own flags after `--format
    /// claude`, `--root <dir>/root` added, and `setup(dir)` run first. The
    /// config file of that root is `<dir>/config.yaml`. The caller holds
    /// `serial()`.
    fn signal_run(sig: libc::c_int, tag: &str, flags: &[&str], setup: impl FnOnce(&std::path::Path)) -> (WardenResult, &'static str) {
        let dir = std::env::temp_dir().join(format!("dg-hook-{}-sig{sig}-{tag}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        setup(&dir);
        let exe = dir.join("child.sh");
        std::fs::write(&exe, "#!/bin/sh\necho $$ > \"$(dirname \"$0\")/pid\"\nexec sleep 30\n").unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&exe, std::fs::Permissions::from_mode(0o700)).unwrap();
        let pid = std::process::id() as libc::pid_t;
        let pidfile = dir.join("pid");
        let sender = std::thread::spawn(move || {
            while !pidfile.exists() {
                std::thread::sleep(Duration::from_millis(10));
            }
            std::thread::sleep(Duration::from_millis(100)); // the parent is in its wait by now
            unsafe { libc::kill(pid, sig) };
        });
        let mut args = claude();
        args.extend(flags.iter().map(OsString::from));
        args.extend(["--root"].map(OsString::from));
        args.push(dir.join("root").into_os_string());
        let t0 = Instant::now();
        let out = run_child_as(&exe, &args, &[], b"");
        sender.join().unwrap();
        let _ = std::fs::remove_dir_all(&dir);
        assert!(t0.elapsed() < Duration::from_secs(5), "{:?}", t0.elapsed());
        out
    }

    /// No `--mode`, `warden_mode: enforce` in the root's config: enforce, so
    /// a signal denies with exit 2.
    #[test]
    fn a_signal_with_no_mode_flag_and_config_enforce_denies() {
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let _g = serial();
            let (res, how) = signal_run(sig, "cfg-enforce", &[], |d| std::fs::write(d.join("config.yaml"), "warden_mode: enforce\n").unwrap());
            assert_eq!((res.exit, how), (2, "backstop"), "signal {sig}");
            assert!(res.why.contains("signal"), "{}", res.why);
        }
    }

    /// No `--mode` and no config: the default is audit, so a signal allows.
    #[test]
    fn a_signal_with_no_mode_flag_and_no_config_allows() {
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let _g = serial();
            let (res, how) = signal_run(sig, "no-cfg", &[], |_| {});
            assert_eq!((res.exit, how), (0, "signal"), "signal {sig}");
            assert_eq!(res.out_stdout, r#"{"continue": true}"#);
        }
    }

    /// Enforce from config, and the calling pane's "watching" marker: the
    /// hook is audit, so a signal allows. The pane comes from the process
    /// environment, so the test sets it and puts it back.
    #[test]
    fn a_signal_in_a_watched_pane_allows_where_enforce_would_deny() {
        const SOCK: &str = "/run/coppice/floor-signal.sock";
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let _g = serial();
            let old: Vec<_> = ["COPPICE_PANE", "COPPICE_SOCK"].iter().map(|k| (*k, std::env::var_os(k))).collect();
            std::env::set_var("COPPICE_PANE", "w1:p1");
            std::env::set_var("COPPICE_SOCK", SOCK);
            let (res, how) = signal_run(sig, "watched", &[], |d| {
                std::fs::write(d.join("config.yaml"), "warden_mode: enforce\n").unwrap();
                let floor = d.join("root").join("agents").join(crate::warden::config::agent_mode_floor(SOCK).unwrap());
                std::fs::create_dir_all(&floor).unwrap();
                let marker = floor.join("w1:p1.mode");
                std::fs::write(&marker, b"watching\n").unwrap();
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&marker, std::fs::Permissions::from_mode(0o600)).unwrap();
            });
            for (k, v) in old {
                match v {
                    Some(v) => std::env::set_var(k, v),
                    None => std::env::remove_var(k),
                }
            }
            assert_eq!((res.exit, how), (0, "signal"), "signal {sig}");
            assert_eq!(res.out_stdout, r#"{"continue": true}"#);
        }
    }

    /// The fenced hook waits on a resident warden that takes the request and
    /// never answers. A signal then ends in the mode's verdict: audit allows
    /// with exit 0, enforce denies with exit 2. The signal goes out once the
    /// socket has the request, so no clock decides it.
    fn fenced_signal_run(sig: libc::c_int, flags: &[&str], setup: impl FnOnce(&std::path::Path)) -> WardenResult {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join(format!("dg-hook-{}-fenced{sig}-{}", std::process::id(), flags.join("")));
        let _ = std::fs::remove_dir_all(&dir);
        let root = dir.join("root");
        std::fs::create_dir_all(&root).unwrap();
        setup(&dir);
        let sock = root.join("warden.sock");
        let ln = std::os::unix::net::UnixListener::bind(&sock).unwrap();
        std::fs::set_permissions(&sock, std::fs::Permissions::from_mode(0o600)).unwrap();
        let pid = std::process::id() as libc::pid_t;
        let holder = std::thread::spawn(move || {
            let (mut c, _) = ln.accept().unwrap();
            let mut buf = [0u8; 65536];
            let _ = c.read(&mut buf);
            unsafe { libc::kill(pid, sig) };
            // Held open until the hook has answered and dropped its end.
            let _ = c.read(&mut buf);
        });
        let mut args = claude();
        args.extend(flags.iter().map(OsString::from));
        args.push(OsString::from("--root"));
        args.push(root.clone().into_os_string());
        let t0 = Instant::now();
        let res = fenced_with_watch(&args, b"{}");
        assert!(t0.elapsed() < Duration::from_secs(5), "{:?}", t0.elapsed());
        holder.join().unwrap();
        let _ = std::fs::remove_dir_all(&dir);
        res
    }

    #[test]
    fn a_fenced_check_ends_in_the_modes_verdict_on_a_signal() {
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let _g = serial();
            let res = fenced_signal_run(sig, &["--mode", "enforce"], |_| {});
            assert_eq!(res.exit, 2, "enforce, signal {sig}");
            assert!(String::from_utf8_lossy(&res.stderr).contains("DENIED"), "{:?}", res.stderr);
            assert!(res.why.contains("signal"), "{}", res.why);
            let res = fenced_signal_run(sig, &["--mode", "audit"], |_| {});
            assert_eq!(res.exit, 0, "audit, signal {sig}");
            assert_eq!(res.out_stdout, r#"{"continue": true}"#);
            assert!(!String::from_utf8_lossy(&res.stderr).contains("DENIED"));
        }
    }

    /// No `--mode`: the config decides (`mode_for`), as it does for the
    /// guarded parent.
    #[test]
    fn a_fenced_signal_with_no_mode_flag_follows_the_config() {
        for sig in [libc::SIGINT, libc::SIGTERM] {
            let _g = serial();
            let res = fenced_signal_run(sig, &[], |d| std::fs::write(d.join("config.yaml"), "warden_mode: enforce\n").unwrap());
            assert_eq!(res.exit, 2, "config enforce, signal {sig}");
            let res = fenced_signal_run(sig, &[], |_| {});
            assert_eq!(res.exit, 0, "no config, signal {sig}");
        }
    }

    /// `mode_for_env` on the same three cases, with the environment given.
    #[test]
    fn the_mode_for_a_call_follows_flag_then_config_then_marker() {
        use std::collections::HashMap;
        let dir = std::env::temp_dir().join(format!("dg-hook-{}-modefor", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let root = dir.join("root");
        let argv = |flags: &[&str]| -> Vec<OsString> {
            let mut a = claude();
            a.extend(flags.iter().map(OsString::from));
            a.push("--root".into());
            a.push(root.clone().into_os_string());
            a
        };
        let env: HashMap<String, String> = HashMap::new();
        // No flag, no config: audit.
        assert_eq!(warden::mode_for_env(&argv(&[]), env.clone()), "audit");
        // The flag wins.
        assert_eq!(warden::mode_for_env(&argv(&["--mode", "enforce"]), env.clone()), "enforce");
        // Config enforce, no flag: enforce.
        std::fs::write(dir.join("config.yaml"), "warden_mode: enforce\n").unwrap();
        assert_eq!(warden::mode_for_env(&argv(&[]), env.clone()), "enforce");
        assert_eq!(warden::mode_for_env(&argv(&["--mode", "audit"]), env.clone()), "audit");
        // A watching marker lowers enforce to audit, for that pane only.
        const SOCK: &str = "/run/coppice/floor-modefor.sock";
        let floor = root.join("agents").join(crate::warden::config::agent_mode_floor(SOCK).unwrap());
        std::fs::create_dir_all(&floor).unwrap();
        let marker = floor.join("w1:p1.mode");
        std::fs::write(&marker, b"watching\n").unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&marker, std::fs::Permissions::from_mode(0o600)).unwrap();
        let on = |pane: &str| -> HashMap<String, String> {
            HashMap::from([("COPPICE_PANE".to_string(), pane.to_string()), ("COPPICE_SOCK".to_string(), SOCK.to_string())])
        };
        assert_eq!(warden::mode_for_env(&argv(&[]), on("w1:p1")), "audit");
        assert_eq!(warden::mode_for_env(&argv(&[]), on("w1:p2")), "enforce");
        // An unreadable mode reads as the --mode text, else enforce.
        std::fs::write(dir.join("config.yaml"), "warden_mode: [unclosed\n").unwrap();
        assert_eq!(warden::mode_for_env(&argv(&["--mode", "bogus"]), env.clone()), "enforce");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_child_past_the_deadline_is_killed() {
        let _g = serial();
        let mut c = Command::new("sleep").arg("30").spawn().unwrap();
        let t0 = Instant::now();
        let r = wait_until(&mut c, t0 + Duration::from_millis(200), 0.2);
        assert_eq!(r.unwrap_err(), "no answer within 0.2 s");
        let took = t0.elapsed();
        assert!(took >= Duration::from_millis(200) && took < Duration::from_secs(5), "{took:?}");
        // Killed and reaped: there is nothing left to wait for.
        assert!(c.try_wait().unwrap().is_some());
    }

    /// The child is killed at the deadline together with everything it
    /// started.
    #[test]
    fn the_childs_group_is_killed_at_the_deadline() {
        let _g = serial();
        use crate::warden::procgroup::testing::{left_after, marker};
        let m = marker();
        let dir = std::env::temp_dir().join(format!("dg-hook-{}-group", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let exe = dir.join("child.sh");
        std::fs::write(&exe, format!("#!/bin/sh\nsleep {m} &\nexec sleep 31\n")).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&exe, std::fs::Permissions::from_mode(0o700)).unwrap();
        let args: Vec<OsString> = vec!["--format".into(), "claude".into(), "--verify-timeout".into(), "0".into()];
        let t0 = Instant::now();
        let (res, how) = run_child_as(&exe, &args, &[], b"");
        let took = t0.elapsed();
        let left = left_after(&m);
        let _ = std::fs::remove_dir_all(&dir);
        assert_eq!((res.exit, how), (2, "backstop"));
        assert!(took < Duration::from_secs(10), "{took:?}");
        assert!(left.is_empty(), "processes from the call outlived it: {left:?}");
    }

    /// A child that has answered is killed together with everything it
    /// started.
    #[test]
    fn the_childs_group_is_killed_after_its_frame() {
        let _g = serial();
        use crate::warden::procgroup::testing::{left_after, marker};
        let m = marker();
        let body = r#"{"stdout":"7b7d0a","stderr":"","exit":0,"native":true,"why":"","done":true,"nonce":"NONCE"}"#;
        let exe = fake_child("groupframe", body, &format!("sleep {m} &\nexec sleep 31"));
        let (res, how) = run_child_as(&exe, &claude(), &[], b"");
        let left = left_after(&m);
        let _ = std::fs::remove_dir_all(exe.parent().unwrap());
        assert_eq!((res.exit, how), (0, "native"));
        assert!(left.is_empty(), "processes from the call outlived it: {left:?}");
    }

    #[test]
    fn a_child_that_ends_is_seen_at_once() {
        let _g = serial();
        let mut c = Command::new("sh").args(["-c", "sleep 0.05; exit 3"]).spawn().unwrap();
        let t0 = Instant::now();
        let s = wait_until(&mut c, t0 + Duration::from_secs(20), 20.0).unwrap();
        assert_eq!(s.code(), Some(3));
        assert!(t0.elapsed() < Duration::from_secs(5));
    }
}
