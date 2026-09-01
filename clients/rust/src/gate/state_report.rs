//! `_state_report.report_state`: deliver one PaneStateEvent to coppice (or
//! Herdr) inside a 0.2 s total budget. Never fails the call.
//!
//! Two modes, as in the oracle:
//! - the environment mode, used by a gate running in the hook's own process:
//!   the process is the pane's own, so `COPPICE_SOCK` and `COPPICE_PANE` are
//!   its identity and coppice places the connection by its pid. One request,
//!   one reply read, nothing checked.
//! - the explicit mode, used when a caller's pane is named on a request
//!   (a resident gate serving many callers): the socket must be an absolute
//!   path to a socket this uid owns, the pane must look like a coppice pane
//!   id, the peer is checked with SO_PEERCRED after connect, the hello names
//!   the caller's real pid in `peer_pids`, and the report is sent only when
//!   the hello reply places the connection as that pane.

use super::paths::{isabs, lstat_uid_is_socket};
use super::pyjson::{dumps, loads, Object, Value};
use std::collections::HashMap;
use std::io::{Read, Write};
use std::os::fd::FromRawFd;
use std::os::unix::net::UnixStream;
use std::time::{Duration, Instant};

const BUDGET: Duration = Duration::from_millis(200);

/// `report_transcript_path`: a transcript path kept only when it is an
/// absolute path string.
pub fn transcript_path_for_report(raw: &Value) -> Option<String> {
    match raw {
        Value::Str(s) if !s.is_empty() && isabs(s) => Some(s.clone()),
        _ => None,
    }
}

/// `_valid_coppice_pane_id`: `w<1-9 digits>:p<1-9 digits>`, exactly.
pub fn valid_coppice_pane_id(pane: &str) -> bool {
    fn digits(s: &str) -> bool {
        (1..=9).contains(&s.len()) && s.bytes().all(|c| c.is_ascii_digit())
    }
    match pane.strip_prefix('w').and_then(|r| r.split_once(":p")) {
        Some((w, p)) => digits(w) && digits(p),
        None => false,
    }
}

/// `_valid_herdr_pane_id`.
pub fn valid_herdr_pane_id(pane: &str) -> bool {
    !pane.starts_with('-')
        && (1..=128).contains(&pane.len())
        && pane.bytes().all(|c| c.is_ascii_alphanumeric() || b"._:-".contains(&c))
}

fn uid() -> u32 {
    // SAFETY: getuid never fails and has no preconditions.
    unsafe { libc::getuid() }
}

/// `_coppice_sock_trustworthy`: an absolute path to a unix socket this uid
/// owns, not followed through a final symlink.
pub fn coppice_sock_trustworthy(sock: &str) -> bool {
    if sock.is_empty() || !isabs(sock) {
        return false;
    }
    matches!(lstat_uid_is_socket(sock), Some((owner, true)) if owner == uid())
}

struct Deadline(Instant);

impl Deadline {
    fn remaining(&self) -> Duration {
        self.0.saturating_duration_since(Instant::now()).max(Duration::from_millis(1))
    }
}

/// A unix stream connection made within the budget. A connect that would
/// block on a full backlog fails at once, as Python's timed socket does.
fn connect(path: &str, dl: &Deadline) -> Option<UnixStream> {
    let bytes = path.as_bytes();
    // SAFETY: a plain sockaddr_un filled within its bounds, and a socket fd
    // owned by the UnixStream built from it.
    unsafe {
        let mut addr: libc::sockaddr_un = std::mem::zeroed();
        if bytes.len() >= addr.sun_path.len() || bytes.contains(&0) {
            return None;
        }
        addr.sun_family = libc::AF_UNIX as libc::sa_family_t;
        for (i, b) in bytes.iter().enumerate() {
            addr.sun_path[i] = *b as libc::c_char;
        }
        let fd = libc::socket(libc::AF_UNIX, libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK, 0);
        if fd < 0 {
            return None;
        }
        let stream = UnixStream::from_raw_fd(fd);
        let len = (std::mem::size_of::<libc::sa_family_t>() + bytes.len() + 1) as libc::socklen_t;
        let rc = libc::connect(fd, &addr as *const _ as *const libc::sockaddr, len);
        if rc != 0 {
            let err = std::io::Error::last_os_error().raw_os_error();
            if err != Some(libc::EINPROGRESS) {
                return None;
            }
            let mut pfd = libc::pollfd { fd, events: libc::POLLOUT, revents: 0 };
            let ms = dl.remaining().as_millis().min(i32::MAX as u128) as i32;
            if libc::poll(&mut pfd, 1, ms) != 1 {
                return None;
            }
            let mut so_err: libc::c_int = 0;
            let mut sl = std::mem::size_of::<libc::c_int>() as libc::socklen_t;
            if libc::getsockopt(fd, libc::SOL_SOCKET, libc::SO_ERROR, &mut so_err as *mut _ as *mut libc::c_void, &mut sl)
                != 0
                || so_err != 0
            {
                return None;
            }
        }
        stream.set_nonblocking(false).ok()?;
        Some(stream)
    }
}

fn send_line(s: &mut UnixStream, v: &Object, dl: &Deadline) -> Option<()> {
    s.set_write_timeout(Some(dl.remaining())).ok()?;
    s.write_all(format!("{}\n", dumps(&Value::Obj(v.clone()), true)).as_bytes()).ok()
}

/// `_read_one_reply`: one reply line as a JSON object, or `None`.
fn read_one_reply(s: &mut UnixStream, dl: &Deadline) -> Option<Object> {
    let mut buf: Vec<u8> = Vec::new();
    let mut chunk = vec![0u8; 65536];
    while !buf.ends_with(b"\n") {
        s.set_read_timeout(Some(dl.remaining())).ok()?;
        let n = s.read(&mut chunk).ok()?;
        if n == 0 {
            return None;
        }
        buf.extend_from_slice(&chunk[..n]);
        if buf.len() > 65536 {
            return None;
        }
    }
    let text = String::from_utf8(buf).ok()?;
    match loads(text.trim_end_matches(['\n', '\r', ' ', '\t'])) {
        Ok(Value::Obj(o)) => Some(o),
        _ => None,
    }
}

/// The uid of the process on the far end of a connected unix socket.
fn peer_uid(s: &UnixStream) -> Option<u32> {
    use std::os::fd::AsRawFd;
    // SAFETY: getsockopt fills a ucred of the size it is given.
    unsafe {
        let mut cred: libc::ucred = std::mem::zeroed();
        let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
        let rc = libc::getsockopt(
            s.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut _ as *mut libc::c_void,
            &mut len,
        );
        if rc != 0 {
            return None;
        }
        Some(cred.uid)
    }
}

/// The environment mode: one `pane.report_state` request and one read of
/// the reply. Returns which sink took the event.
pub fn report_state_env(env: &HashMap<String, String>, ev: &Object) -> &'static str {
    let dl = Deadline(Instant::now() + BUDGET);
    let (sock, pane) = match (env.get("COPPICE_SOCK"), env.get("COPPICE_PANE")) {
        (Some(s), Some(p)) if !s.is_empty() && !p.is_empty() => (s, p),
        _ => {
            let herdr = ["HERDR_PANE_ID", "HERDR_PANE"].iter().find_map(|k| env.get(*k).filter(|v| !v.is_empty()));
            return match herdr {
                Some(h) => send_herdr_report(h, ev, &dl),
                None => "none",
            };
        }
    };
    let mut s = match connect(sock, &dl) {
        Some(s) => s,
        None => return "none",
    };
    let req = Object::new()
        .with("id", "r")
        .with("cmd", "pane.report_state")
        .with("pane", pane.as_str())
        .with("event", ev.clone());
    if send_line(&mut s, &req, &dl).is_none() {
        return "none";
    }
    let _ = s.set_read_timeout(Some(dl.remaining()));
    let mut buf = vec![0u8; 65536];
    match s.read(&mut buf) {
        Ok(_) => "coppice",
        Err(_) => "none",
    }
}

/// `shutil.which("herdr", path=env.get("PATH"))` on this process's PATH.
fn which_herdr() -> Option<std::path::PathBuf> {
    use std::os::unix::ffi::OsStrExt;
    let path = std::env::var_os("PATH").unwrap_or_else(|| std::ffi::OsString::from("/bin:/usr/bin"));
    if path.is_empty() {
        return None;
    }
    let mut seen: Vec<Vec<u8>> = Vec::new();
    for dir in path.as_bytes().split(|&b| b == b':') {
        if seen.iter().any(|d| d == dir) {
            continue;
        }
        seen.push(dir.to_vec());
        let d = std::path::Path::new(std::ffi::OsStr::from_bytes(dir));
        let f = if dir.is_empty() { std::path::PathBuf::from("herdr") } else { d.join("herdr") };
        let c = match std::ffi::CString::new(f.as_os_str().as_bytes()) {
            Ok(c) => c,
            Err(_) => continue,
        };
        // exists, X_OK, and not a directory
        let ok = unsafe { libc::access(c.as_ptr(), libc::F_OK | libc::X_OK) == 0 } && !f.is_dir() && f.exists();
        if ok {
            return Some(f);
        }
    }
    None
}

/// `_send_herdr_report`: the Herdr CLI leg, within what is left of the
/// budget.
fn send_herdr_report(pane: &str, ev: &Object, dl: &Deadline) -> &'static str {
    if !valid_herdr_pane_id(pane) {
        return "none";
    }
    let mapped = match ev.value("state").as_str() {
        Some("working") => "working",
        Some("blocked") => "blocked",
        Some("idle") | Some("done") => "idle",
        _ => return "none",
    };
    let bin = match which_herdr() {
        Some(b) => b,
        None => return "none",
    };
    let harness = match ev.value("harness") {
        Value::Str(s) => s.clone(),
        v if !v.truthy() => String::new(),
        v => super::pyjson::py_str(v).unwrap_or_default(),
    };
    let mut child = match std::process::Command::new(&bin)
        .args(["pane", "report-agent", "--source", "daisugi", "--agent", &harness, "--state", mapped, "--", pane])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .env_remove(super::TTY_ENV)
        .spawn()
    {
        Ok(c) => c,
        Err(_) => return "none",
    };
    let end = Instant::now() + dl.remaining();
    loop {
        match child.try_wait() {
            Ok(Some(_)) => return "herdr",
            Ok(None) if Instant::now() < end => std::thread::sleep(Duration::from_millis(1)),
            Ok(None) => {
                let _ = child.kill();
                let stop = Instant::now() + Duration::from_millis(50);
                while Instant::now() < stop {
                    if let Ok(Some(_)) = child.try_wait() {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(1));
                }
                return "none";
            }
            Err(_) => return "none",
        }
    }
}

/// The explicit mode's coppice leg (`_report_as_pane_via_coppice`).
pub fn report_as_pane(sock: &str, pane: &str, peer_pid: Option<i64>, ev: &Object) -> &'static str {
    let dl = Deadline(Instant::now() + BUDGET);
    if !coppice_sock_trustworthy(sock) || !valid_coppice_pane_id(pane) {
        return "none";
    }
    let mut s = match connect(sock, &dl) {
        Some(s) => s,
        None => return "none",
    };
    // A parent directory can be swapped between the lstat and the connect,
    // so the peer is checked again on the live connection.
    if peer_uid(&s) != Some(uid()) {
        return "none";
    }
    let mut hello = Object::new().with("id", "h").with("cmd", "hello").with("role", "pane").with("pane", pane);
    if let Some(pid) = peer_pid {
        hello.set("peer_pids", vec![Value::from(pid)]);
    }
    if send_line(&mut s, &hello, &dl).is_none() {
        return "none";
    }
    let reply = match read_one_reply(&mut s, &dl) {
        Some(r) => r,
        None => return "none",
    };
    if reply.value("ok") != &Value::Bool(true) {
        return "none";
    }
    match reply.value("result") {
        Value::Obj(r) if r.value("role").as_str() == Some("pane") && r.value("pane").as_str() == Some(pane) => {}
        _ => return "none",
    }
    let req = Object::new()
        .with("id", "r")
        .with("cmd", "pane.report_state")
        .with("pane", pane)
        .with("event", ev.clone());
    if send_line(&mut s, &req, &dl).is_none() {
        return "none";
    }
    match read_one_reply(&mut s, &dl) {
        Some(r) if r.value("ok") == &Value::Bool(true) => "coppice",
        _ => "none",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;
    use std::thread;

    #[test]
    fn pane_id_shapes() {
        assert!(valid_coppice_pane_id("w1:p3") && valid_coppice_pane_id("w123456789:p1"));
        for bad in ["w1:p3\n", "pane-7", "w:p1", "w1:p", "w1234567890:p1", "x1:p1", "w1:q1"] {
            assert!(!valid_coppice_pane_id(bad), "{bad:?}");
        }
        assert!(valid_herdr_pane_id("a.b:c-1") && !valid_herdr_pane_id("-x") && !valid_herdr_pane_id(""));
    }

    fn scratch() -> std::path::PathBuf {
        // A socket path must fit in sun_path, so this is short: TMPDIR, or
        // the system's temporary directory.
        let d = std::env::temp_dir().join(format!("dg-{}-{}", std::process::id(), random()));
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn random() -> u64 {
        std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos() as u64
    }

    /// A fake coppice: answers each line with `answer(line)`, records lines.
    fn serve(
        path: &std::path::Path,
        answer: fn(&Object) -> String,
    ) -> thread::JoinHandle<Vec<Object>> {
        let l = UnixListener::bind(path).unwrap();
        thread::spawn(move || {
            let (mut c, _) = l.accept().unwrap();
            c.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            let mut seen = Vec::new();
            let mut buf = Vec::new();
            let mut chunk = [0u8; 4096];
            loop {
                let n = match c.read(&mut chunk) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => n,
                };
                buf.extend_from_slice(&chunk[..n]);
                while let Some(i) = buf.iter().position(|&b| b == b'\n') {
                    let line: Vec<u8> = buf.drain(..=i).collect();
                    let o = match loads(std::str::from_utf8(&line).unwrap().trim()) {
                        Ok(Value::Obj(o)) => o,
                        _ => panic!("not an object"),
                    };
                    let a = answer(&o);
                    seen.push(o);
                    c.write_all(a.as_bytes()).unwrap();
                }
            }
            seen
        })
    }

    fn ev() -> Object {
        Object::new().with("state", "working")
    }

    #[test]
    fn explicit_mode_sends_hello_with_peer_pids_then_the_report() {
        let d = scratch();
        let sock = d.join("c.sock");
        let h = serve(&sock, |o| {
            if o.value("cmd").as_str() == Some("hello") {
                "{\"id\": \"h\", \"ok\": true, \"result\": {\"role\": \"pane\", \"pane\": \"w1:p2\"}}\n".into()
            } else {
                "{\"id\": \"r\", \"ok\": true}\n".into()
            }
        });
        let got = report_as_pane(sock.to_str().unwrap(), "w1:p2", Some(4242), &ev());
        assert_eq!(got, "coppice");
        let seen = h.join().unwrap();
        assert_eq!(seen.len(), 2);
        assert_eq!(
            dumps(&Value::Obj(seen[0].clone()), true),
            r#"{"id": "h", "cmd": "hello", "role": "pane", "pane": "w1:p2", "peer_pids": [4242]}"#
        );
        assert_eq!(seen[1].value("cmd").as_str(), Some("pane.report_state"));
        let _ = std::fs::remove_dir_all(d);
    }

    #[test]
    fn explicit_mode_sends_nothing_when_coppice_places_another_pane() {
        let d = scratch();
        let sock = d.join("c.sock");
        let h = serve(&sock, |_| "{\"id\": \"h\", \"ok\": true, \"result\": {\"role\": \"pane\", \"pane\": \"w1:p9\"}}\n".into());
        assert_eq!(report_as_pane(sock.to_str().unwrap(), "w1:p2", Some(1), &ev()), "none");
        let seen = h.join().unwrap();
        assert_eq!(seen.len(), 1, "the report must not follow a refused hello");
        let _ = std::fs::remove_dir_all(d);
    }

    #[test]
    fn explicit_mode_refuses_bad_claims_before_connecting() {
        assert_eq!(report_as_pane("rel/c.sock", "w1:p2", None, &ev()), "none");
        assert_eq!(report_as_pane("/nonexistent/c.sock", "w1:p2", None, &ev()), "none");
        let d = scratch();
        let f = d.join("plain");
        std::fs::write(&f, "x").unwrap();
        assert_eq!(report_as_pane(f.to_str().unwrap(), "w1:p2", None, &ev()), "none");
        let sock = d.join("c.sock");
        let _l = UnixListener::bind(&sock).unwrap();
        assert_eq!(report_as_pane(sock.to_str().unwrap(), "pane-7", None, &ev()), "none");
        let _ = std::fs::remove_dir_all(d);
    }

    #[test]
    fn environment_mode_sends_one_report_and_reads_one_reply() {
        let d = scratch();
        let sock = d.join("c.sock");
        let h = serve(&sock, |_| "{\"id\": \"r\", \"ok\": true}\n".into());
        let mut env = HashMap::new();
        env.insert("COPPICE_SOCK".to_string(), sock.to_str().unwrap().to_string());
        env.insert("COPPICE_PANE".to_string(), "pane-7".to_string());
        assert_eq!(report_state_env(&env, &ev()), "coppice");
        let seen = h.join().unwrap();
        assert_eq!(seen.len(), 1);
        assert_eq!(
            dumps(&Value::Obj(seen[0].clone()), true),
            r#"{"id": "r", "cmd": "pane.report_state", "pane": "pane-7", "event": {"state": "working"}}"#
        );
        let _ = std::fs::remove_dir_all(d);
    }
}
