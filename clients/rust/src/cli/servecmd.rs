//! `daisugi gate serve`: the resident gate (`gate_server.py`). One JSON
//! request line per connection on `<root>/gate.sock`, one reply line, then
//! the connection is closed. Each gate call is decided in a child process
//! of this binary, so no crash of one call ends the server or reads as an
//! allow.

use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::hook::run_resident_child;
use crate::gate::resident::{self, Request, MAX_REQUEST_BYTES, PANE_ENV_KEYS};
use std::io::{Read, Write};
use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::{Arc, Condvar, Mutex};

const ROOT_OPT: Opt = Opt::val(&["--root"], "PATH", "Gate state directory (envelopes, audit log, disarm marker).");

/// The calls decided at once. Each is a child process of this binary; a
/// call past the bound waits for a free one.
const MAX_CHILDREN: usize = 32;

/// A connection's thread stack: a request may nest 9,992 deep, and the
/// reader and `str()` recurse.
const CONN_STACK: usize = 256 << 20;

impl Env {
    pub(super) fn gate_serve(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate serve", m),
        };
        if p.help {
            return self.cmd_help("gate serve", "", "Run the resident gate in the foreground (Ctrl-C to stop).", &opts);
        }
        let root = if p.has("--root") { path_str(&p.str("--root", "")) } else { join(&self.data_home(), "gate") };
        let sock = join(&root, "gate.sock");
        self.flush();
        // A live gate keeps its socket; a second server never takes it over.
        if probe(&sock) == Probe::Live {
            self.errf(&format!("gate: a resident gate already answers on {sock}; stop it first\n"));
            return exit(1);
        }
        let _ = std::io::stderr().write_all(format!("gate: serving on {sock} (Ctrl-C to stop)\n").as_bytes());
        // This process serves every session, so it is no one pane: the
        // calls it decides (its children inherit this environment) and the
        // reports it sends never read these.
        for k in PANE_ENV_KEYS {
            std::env::remove_var(k);
            self.env.remove(k);
        }
        let listener = match listen(&root, &sock) {
            Ok(l) => l,
            Err(e) => {
                self.errf(&format!("daisugi gate serve: {e}\n"));
                return exit(1);
            }
        };
        on_stop_signals(&sock);
        let home = self.home.clone();
        let slots = Arc::new((Mutex::new(0usize), Condvar::new()));
        for conn in listener.incoming() {
            let c = match conn {
                Ok(c) => c,
                Err(_) => {
                    std::thread::sleep(std::time::Duration::from_millis(10));
                    continue;
                }
            };
            let (home, slots) = (home.clone(), slots.clone());
            // A connection whose thread cannot start is closed with no
            // reply, which every client reads as a deny.
            let _ = std::thread::Builder::new().stack_size(CONN_STACK).spawn(move || serve_conn(c, &home, &slots));
        }
        Ok(())
    }
}

/// `serve()`'s start: the root made (0700), a stale socket removed, a new
/// one bound (0600).
fn listen(root: &str, sock: &str) -> std::io::Result<UnixListener> {
    mkdir_py(root)?;
    // mkdir's mode is a no-op on a directory that already exists.
    std::fs::set_permissions(root, std::fs::Permissions::from_mode(0o700))?;
    // sock.exists() follows a symlink; whatever is there is unlinked.
    if std::fs::metadata(sock).is_ok() {
        let c = std::ffi::CString::new(sock).map_err(|_| std::io::Error::from_raw_os_error(libc::EINVAL))?;
        if unsafe { libc::unlink(c.as_ptr()) } != 0 {
            return Err(std::io::Error::last_os_error());
        }
    }
    // The root is 0700 from here on, so nobody else can reach the socket
    // before the chmod below. The umask is not changed: it is shared by
    // every thread of the process, and a directory another thread makes
    // meanwhile would lose its search bit.
    let l = UnixListener::bind(sock)?;
    std::fs::set_permissions(sock, std::fs::Permissions::from_mode(0o600))?;
    Ok(l)
}

/// `Path.mkdir(parents=True, exist_ok=True, mode=0o700)`: the parents with
/// the default mode, the directory itself 0700, and an error when the path
/// is there but is no directory.
fn mkdir_py(d: &str) -> std::io::Result<()> {
    let mut b = std::fs::DirBuilder::new();
    b.mode(0o700);
    let first = b.create(d);
    let err = match first {
        Ok(()) => return Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            if let Some(parent) = std::path::Path::new(d).parent().filter(|p| !p.as_os_str().is_empty()) {
                std::fs::create_dir_all(parent)?;
            }
            match b.create(d) {
                Ok(()) => return Ok(()),
                Err(e) => e,
            }
        }
        Err(e) => e,
    };
    if std::path::Path::new(d).is_dir() {
        return Ok(());
    }
    Err(err)
}

/// What `probe` finds at the socket path.
#[derive(Debug, PartialEq, Eq)]
pub(super) enum Probe {
    None,
    Live,
    Stale,
}

/// `gate_server.probe`: nothing at the path, a server that accepts a
/// connection (or whose queue is full), or something nobody listens on
/// (the socket of a dead server, a regular file). It sends an empty
/// request and reads the reply, so a Python server never writes to a
/// client that already left.
pub(super) fn probe(sock: &str) -> Probe {
    use std::os::fd::FromRawFd;
    if std::fs::metadata(sock).is_err() {
        return Probe::None;
    }
    let bytes = sock.as_bytes();
    // SAFETY: a zeroed sockaddr_un is valid; the path is copied in bounds.
    unsafe {
        let mut addr: libc::sockaddr_un = std::mem::zeroed();
        if bytes.len() >= addr.sun_path.len() {
            return Probe::Stale;
        }
        addr.sun_family = libc::AF_UNIX as libc::sa_family_t;
        for (i, b) in bytes.iter().enumerate() {
            addr.sun_path[i] = *b as libc::c_char;
        }
        let fd = libc::socket(libc::AF_UNIX, libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC, 0);
        if fd < 0 {
            return Probe::Stale;
        }
        let len = std::mem::size_of::<libc::sockaddr_un>() as libc::socklen_t;
        if libc::connect(fd, &addr as *const _ as *const libc::sockaddr, len) != 0 {
            let err = std::io::Error::last_os_error().raw_os_error();
            libc::close(fd);
            return if err == Some(libc::EAGAIN) || err == Some(libc::EWOULDBLOCK) { Probe::Live } else { Probe::Stale };
        }
        let mut c = UnixStream::from_raw_fd(fd);
        let _ = c.set_nonblocking(false);
        let _ = c.set_read_timeout(Some(std::time::Duration::from_secs(1)));
        let _ = c.shutdown(std::net::Shutdown::Write);
        let mut sink = Vec::new();
        let _ = c.read_to_end(&mut sink);
    }
    Probe::Live
}

static mut NEWLINE: [u8; 1] = [b'\n'];
/// The socket this server bound, for the signal handlers: its path as a C
/// string and its inode.
static SOCK_PATH: std::sync::atomic::AtomicPtr<libc::c_char> = std::sync::atomic::AtomicPtr::new(std::ptr::null_mut());
static SOCK_INO: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

/// Removes the socket this server bound, and never one another server
/// bound at the same path since. Only async-signal-safe calls.
fn unlink_if_ours() {
    let p = SOCK_PATH.load(std::sync::atomic::Ordering::SeqCst);
    let ino = SOCK_INO.load(std::sync::atomic::Ordering::SeqCst);
    if p.is_null() || ino == 0 {
        return;
    }
    // SAFETY: p is a leaked CString that lives for the whole process.
    unsafe {
        let mut st: libc::stat = std::mem::zeroed();
        if libc::lstat(p, &mut st) == 0 && st.st_ino as u64 == ino {
            libc::unlink(p);
        }
    }
}

extern "C" fn stop_int(_: libc::c_int) {
    // Only async-signal-safe calls: a newline on stderr, the socket
    // removed, then the end.
    unsafe {
        libc::write(2, std::ptr::addr_of!(NEWLINE) as *const libc::c_void, 1);
    }
    unlink_if_ours();
    unsafe { libc::_exit(0) }
}

extern "C" fn stop_term(_: libc::c_int) {
    unlink_if_ours();
    unsafe { libc::_exit(0) }
}

/// Ctrl-C prints a newline and ends the server, as the typer command's
/// KeyboardInterrupt does; SIGTERM ends it with no newline. Either way the
/// socket goes with it: one left behind reads as a running gate to the
/// next `start`. A SIGINT this process was started with ignored stays
/// ignored, as in Python. A SIGTERM ignored at start does not, as in Go,
/// whose runtime cannot keep it ignored.
fn on_stop_signals(sock: &str) {
    use std::os::unix::fs::MetadataExt;
    if let (Ok(c), Ok(st)) = (std::ffi::CString::new(sock), std::fs::symlink_metadata(sock)) {
        SOCK_INO.store(st.ino(), std::sync::atomic::Ordering::SeqCst);
        SOCK_PATH.store(c.into_raw(), std::sync::atomic::Ordering::SeqCst);
    }
    unsafe {
        libc::signal(libc::SIGTERM, stop_term as extern "C" fn(libc::c_int) as libc::sighandler_t);
        let mut old: libc::sigaction = std::mem::zeroed();
        if libc::sigaction(libc::SIGINT, std::ptr::null(), &mut old) != 0 || old.sa_sigaction == libc::SIG_IGN {
            return;
        }
        libc::signal(libc::SIGINT, stop_int as extern "C" fn(libc::c_int) as libc::sighandler_t);
    }
}

/// Answers one connection: `_Handler.handle`.
fn serve_conn(mut c: UnixStream, home: &str, slots: &(Mutex<usize>, Condvar)) {
    let peer = peer_pid(&c);
    let line = read_request_line(&mut c);
    let mut q: Request = match resident::parse_request(&line) {
        Some(q) => q,
        None => {
            let _ = c.write_all(&resident::bad_request());
            return;
        }
    };
    q.caller.peer_pid = peer;
    let reply = if q.is_hook_report() {
        let (out, err, code) = resident::hook_report(&q.argv[2..], &q.stdin, &q.caller, home);
        resident::reply_line(&out, &err, code as i64)
    } else {
        {
            let (n, cv) = slots;
            let mut n = n.lock().unwrap_or_else(|e| e.into_inner());
            while *n >= MAX_CHILDREN {
                n = cv.wait(n).unwrap_or_else(|e| e.into_inner());
            }
            *n += 1;
        }
        let res = run_resident_child(&q);
        {
            let (n, cv) = slots;
            let mut n = n.lock().unwrap_or_else(|e| e.into_inner());
            *n -= 1;
            cv.notify_one();
        }
        if res.help {
            // argparse's --help is SystemExit(0) in the oracle's handler:
            // the connection ends with no reply.
            return;
        }
        resident::reply_line(&res.out_stdout, &res.out_stderr, res.exit as i64)
    };
    let _ = c.write_all(&reply);
}

/// `rfile.readline(_MAX_REQUEST)`: up to and with the first newline, or
/// MAX_REQUEST_BYTES, or what came before the client stopped sending.
fn read_request_line(c: &mut UnixStream) -> Vec<u8> {
    let mut buf = Vec::new();
    let mut chunk = vec![0u8; 65536];
    while buf.len() < MAX_REQUEST_BYTES {
        let want = chunk.len().min(MAX_REQUEST_BYTES - buf.len());
        match c.read(&mut chunk[..want]) {
            Ok(0) => break,
            Ok(n) => {
                if let Some(i) = chunk[..n].iter().position(|&b| b == b'\n') {
                    buf.extend_from_slice(&chunk[..=i]);
                    return buf;
                }
                buf.extend_from_slice(&chunk[..n]);
            }
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(_) => break,
        }
    }
    buf
}

/// The pid SO_PEERCRED names for an accepted connection
/// (`gate_server._peer_pid`).
fn peer_pid(c: &UnixStream) -> Option<i64> {
    use std::os::fd::AsRawFd;
    // SAFETY: getsockopt fills a ucred of the size it is given.
    unsafe {
        let mut cred: libc::ucred = std::mem::zeroed();
        let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
        let rc = libc::getsockopt(
            c.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut _ as *mut libc::c_void,
            &mut len,
        );
        (rc == 0).then_some(cred.pid as i64)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn listen_replaces_a_stale_file_and_keeps_the_modes() {
        let d = std::env::temp_dir().join(format!("daisugi-serve-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        let root = d.join("a").join("gate");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o755)).unwrap();
        std::fs::write(root.join("gate.sock"), "stale").unwrap();
        let (r, s) = (root.to_str().unwrap().to_string(), root.join("gate.sock").to_str().unwrap().to_string());
        let l = listen(&r, &s).unwrap();
        use std::os::unix::fs::FileTypeExt;
        let st = std::fs::symlink_metadata(&s).unwrap();
        assert!(st.file_type().is_socket());
        assert_eq!(st.permissions().mode() & 0o777, 0o600);
        assert_eq!(std::fs::metadata(&r).unwrap().permissions().mode() & 0o777, 0o700);
        drop(l);
        // Closing the listener leaves the socket file; the stop signals
        // remove it (unlink_if_ours).
        assert!(std::fs::symlink_metadata(&s).is_ok());
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn probe_tells_nothing_stale_and_live_apart() {
        let d = std::env::temp_dir().join(format!("daisugi-probe-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        let p = d.join("g.sock");
        let s = p.to_str().unwrap();
        assert_eq!(probe(s), Probe::None);
        drop(UnixListener::bind(&p).unwrap());
        assert_eq!(probe(s), Probe::Stale);
        std::fs::remove_file(&p).unwrap();
        std::fs::write(&p, "x").unwrap();
        assert_eq!(probe(s), Probe::Stale);
        std::fs::remove_file(&p).unwrap();
        let live = UnixListener::bind(&p).unwrap();
        assert_eq!(probe(s), Probe::Live);
        drop(live);
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn listen_refuses_a_root_that_is_a_file() {
        let d = std::env::temp_dir().join(format!("daisugi-serve-f-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(d.join("gate"), "x").unwrap();
        let r = d.join("gate").to_str().unwrap().to_string();
        assert!(listen(&r, &format!("{r}/gate.sock")).is_err());
        let _ = std::fs::remove_dir_all(&d);
    }
}
