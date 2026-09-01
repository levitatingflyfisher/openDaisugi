//! One tool-call hook answered the way `python -m opendaisugi.gate` does,
//! with the same flags, stdin, stdout, stderr, exit code and log lines:
//! the whole of `daisugi-gate`, and `daisugi gate check`.
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
//! DAISUGI_GATE_TRACE names a file that gets one JSON line per call saying
//! how the call was answered, and why. It never changes stdout, stderr or
//! the exit code.

use super::{self as gate, pyjson, GateResult};
use std::ffi::{OsStr, OsString};
use std::io::{Read, Write};
use std::os::fd::FromRawFd;
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::time::{Duration, Instant};

/// Names the child's nonce. It is removed from the environment the child
/// decides with, so nothing the gate starts inherits it.
const CHILD_ENV: &str = "DAISUGI_GATE_WORKER";

/// The fd the child writes its frame to.
const FRAME_FD: i32 = 3;

/// Answers one hook call and ends the process. `args` are the gate's own
/// flags; `prefix` is what goes before them to start this binary as the
/// child (nothing for `daisugi-gate`, `gate check` for `daisugi`).
pub fn main(args: Vec<OsString>, prefix: &[&str]) -> ! {
    // stderr is part of the gate's contract: a panic must never print there.
    std::panic::set_hook(Box::new(|_| {}));
    let nonce = std::env::var(CHILD_ENV).unwrap_or_default();
    if !nonce.is_empty() && fd_is_pipe(FRAME_FD) {
        child(&args, &nonce);
    }
    // The deny this process gives if anything below fails, built before
    // stdin is read.
    let fallback = gate::backstop_deny(&args, "the gate's parent process failed on this call");
    let res = std::panic::catch_unwind(|| {
        let stdin = read_stdin();
        let trace = std::env::var_os("DAISUGI_GATE_TRACE").filter(|t| !t.is_empty());
        let (res, how) = run_child(&args, prefix, &stdin);
        if let Some(t) = &trace {
            write_trace(t, how, &res.why, res.exit);
        }
        res
    })
    .unwrap_or(fallback);
    emit(&res);
}

/// stdin, at most one byte past the payload cap: a longer payload is denied
/// unread by the child, as the oracle denies it.
fn read_stdin() -> Vec<u8> {
    let mut stdin = Vec::new();
    let limit = (gate::MAX_PAYLOAD_BYTES + 1) as u64;
    if std::io::stdin().take(limit).read_to_end(&mut stdin).is_err() {
        stdin.clear();
    }
    stdin
}

/// Writes a result and ends the process with its code, running no exit
/// handlers: a thread may still be inside Z3, and its static teardown must
/// not race it and turn a clean answer into a crash.
fn emit(res: &GateResult) -> ! {
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
    // Nothing the gate starts may hold the frame pipe open.
    unsafe { libc::fcntl(FRAME_FD, libc::F_SETFD, libc::FD_CLOEXEC) };
    let stdin = read_stdin();
    let res = match std::panic::catch_unwind(|| gate::run_process(args.to_vec(), &stdin)) {
        Ok(r) => r,
        Err(_) => {
            let mut r = gate::backstop_deny(args, "the Rust gate failed on this call");
            r.why = "a panic".into();
            r
        }
    };
    let frame = serde_json::json!({
        "stdout": hex(&res.stdout),
        "stderr": hex(&res.stderr),
        "exit": res.exit,
        "native": res.native,
        "why": res.why,
        "done": true,
        "nonce": nonce,
    });
    let mut f = unsafe { std::fs::File::from_raw_fd(FRAME_FD) };
    let _ = f.write_all(frame.to_string().as_bytes());
    drop(f);
    emit(&gate::backstop_deny(args, "this process decided the call for its parent gate, which gives the verdict"))
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
fn run_child(args: &[OsString], prefix: &[&str], stdin: &[u8]) -> (GateResult, &'static str) {
    let deny = |why: String| {
        let mut res = gate::backstop_deny(args, &why);
        res.why = why;
        (res, "backstop")
    };
    match std::env::current_exe() {
        Ok(exe) => run_child_as(&exe, args, prefix, stdin),
        Err(e) => deny(format!("the gate cannot find its own binary: {e}")),
    }
}

/// `run_child` with the child's binary named.
fn run_child_as(exe: &std::path::Path, args: &[OsString], prefix: &[&str], stdin: &[u8]) -> (GateResult, &'static str) {
    let deny = |why: String| {
        let mut res = gate::backstop_deny(args, &why);
        res.why = why;
        (res, "backstop")
    };
    let mut fds = [0i32; 2];
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        return deny("the gate could not make its frame pipe".into());
    }
    let (frame_r, frame_w) = (fds[0], fds[1]);
    let nonce = random_nonce();
    let mut cmd = Command::new(exe);
    // Python sizes argparse's text by the terminal on its stdout. The
    // child's stdout is a pipe, so it gets this process's width.
    cmd.env_remove(gate::TTY_ENV);
    if let Some(cols) = tty_columns() {
        cmd.env(gate::TTY_ENV, cols.to_string());
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
    let spawned = cmd.spawn();
    unsafe { libc::close(frame_w) };
    let mut child = match spawned {
        Ok(c) => c,
        Err(e) => {
            unsafe { libc::close(frame_r) };
            return deny(format!("the gate's child did not start: {e}"));
        }
    };
    let deadline_s = gate::child_deadline_s(args);
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
            let _ = child.kill();
            let _ = child.wait();
            return deny("the gate could not start a thread".to_string());
        }
    };
    // The child closes the frame pipe once its frame is written, and has
    // done all its work by then: only its own deny and its exit are left.
    // So a complete frame is the answer at once, and the child is killed,
    // so it cannot outlive this process.
    if let Some(frame) = read_until(&mut frame_file, deadline) {
        if let Some(res) = parse_frame(&frame, &nonce) {
            let _ = child.kill();
            let how = if res.native { "native" } else { "undecided" };
            return (res, how);
        }
    }
    let status = match wait_until(&mut child, deadline, deadline_s) {
        Err(why) => return deny(why),
        Ok(s) => s,
    };
    let stderr = err_t.join().unwrap_or_default();
    use std::os::unix::process::ExitStatusExt;
    let first = String::from_utf8_lossy(&stderr).lines().next().unwrap_or("").chars().take(200).collect::<String>();
    match (status.code(), status.signal()) {
        (_, Some(sig)) => deny(format!("the gate's child was ended by signal {sig}")),
        (Some(code), _) => deny(format!("the gate's child wrote no complete answer (exit {code}: {first})")),
        _ => deny("the gate's child wrote no complete answer".into()),
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
        if now >= deadline {
            return None;
        }
        let ms = (deadline - now).as_millis().saturating_add(1).min(i32::MAX as u128) as i32;
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
            Err(e) => break Err(format!("the gate lost its child: {e}")),
        }
        let now = Instant::now();
        if now >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            break Err(format!("no answer within {deadline_s} s"));
        }
        if pidfd >= 0 {
            // Round up, so the last wait does not end just short of the deadline.
            let ms = (deadline - now).as_millis().saturating_add(1).min(i32::MAX as u128) as i32;
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
fn parse_frame(raw: &[u8], nonce: &str) -> Option<GateResult> {
    let v: serde_json::Value = serde_json::from_slice(raw).ok()?;
    if v.get("done")?.as_bool()? != true || v.get("nonce")?.as_str()? != nonce {
        return None;
    }
    let exit = v.get("exit")?.as_i64()?;
    if exit != 0 && exit != 2 {
        return None;
    }
    Some(GateResult {
        stdout: unhex(v.get("stdout")?.as_str()?)?,
        stderr: unhex(v.get("stderr")?.as_str()?)?,
        exit: exit as i32,
        native: v.get("native")?.as_bool()?,
        why: v.get("why")?.as_str()?.to_string(),
    })
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

    fn claude() -> Vec<OsString> {
        vec!["--format".into(), "claude".into()]
    }

    /// A complete frame is the answer as soon as the pipe closes, even
    /// when the child then hangs, and the child does not outlive the call.
    #[test]
    fn a_complete_frame_answers_at_once() {
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

    #[test]
    fn a_child_past_the_deadline_is_killed() {
        let mut c = Command::new("sleep").arg("30").spawn().unwrap();
        let t0 = Instant::now();
        let r = wait_until(&mut c, t0 + Duration::from_millis(200), 0.2);
        assert_eq!(r.unwrap_err(), "no answer within 0.2 s");
        let took = t0.elapsed();
        assert!(took >= Duration::from_millis(200) && took < Duration::from_secs(5), "{took:?}");
        // Killed and reaped: there is nothing left to wait for.
        assert!(c.try_wait().unwrap().is_some());
    }

    #[test]
    fn a_child_that_ends_is_seen_at_once() {
        let mut c = Command::new("sh").args(["-c", "sleep 0.05; exit 3"]).spawn().unwrap();
        let t0 = Instant::now();
        let s = wait_until(&mut c, t0 + Duration::from_secs(20), 20.0).unwrap();
        assert_eq!(s.code(), Some(3));
        assert!(t0.elapsed() < Duration::from_secs(5));
    }
}
