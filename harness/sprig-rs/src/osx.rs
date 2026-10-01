//! The operating system as Go's os, os/exec and path/filepath see it:
//! the same paths, the same lookups and the same error words.

use crate::goerr::{exit_text, path_error};
use std::ffi::OsStr;
use std::io::{Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::os::unix::process::CommandExt;
use std::path::Path;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn p(b: &[u8]) -> &Path {
    Path::new(OsStr::from_bytes(b))
}

/// Go's path/filepath.Clean.
pub fn clean(path: &[u8]) -> Vec<u8> {
    if path.is_empty() {
        return b".".to_vec();
    }
    let rooted = path[0] == b'/';
    let n = path.len();
    let mut out: Vec<u8> = Vec::with_capacity(n);
    let (mut r, mut dotdot) = (0, 0);
    if rooted {
        out.push(b'/');
        r = 1;
        dotdot = 1;
    }
    while r < n {
        if path[r] == b'/' {
            r += 1;
        } else if path[r] == b'.' && (r + 1 == n || path[r + 1] == b'/') {
            r += 1;
        } else if path[r] == b'.' && path[r + 1] == b'.' && (r + 2 == n || path[r + 2] == b'/') {
            r += 2;
            if out.len() > dotdot {
                let mut w = out.len() - 1;
                while w > dotdot && out[w] != b'/' {
                    w -= 1;
                }
                out.truncate(w);
            } else if !rooted {
                if !out.is_empty() {
                    out.push(b'/');
                }
                out.extend_from_slice(b"..");
                dotdot = out.len();
            }
        } else {
            if (rooted && out.len() != 1) || (!rooted && !out.is_empty()) {
                out.push(b'/');
            }
            while r < n && path[r] != b'/' {
                out.push(path[r]);
                r += 1;
            }
        }
    }
    if out.is_empty() {
        out.push(b'.');
    }
    out
}

/// Go's path/filepath.Join of two elements.
pub fn join(a: &[u8], b: &[u8]) -> Vec<u8> {
    match (a.is_empty(), b.is_empty()) {
        (true, true) => Vec::new(),
        (true, false) => clean(b),
        (false, true) => clean(a),
        (false, false) => clean(&[a, b"/", b].concat()),
    }
}

/// Go's path/filepath.Dir.
pub fn dir(path: &[u8]) -> Vec<u8> {
    let cut = path.iter().rposition(|&c| c == b'/').map_or(0, |i| i + 1);
    clean(&path[..cut])
}

/// Go's os.Getwd: $PWD when it names the same directory as ".", else
/// the kernel's answer.
pub fn getwd() -> Option<Vec<u8>> {
    if let Some(pwd) = std::env::var_os("PWD") {
        let pwd = pwd.as_bytes();
        if pwd.first() == Some(&b'/') {
            let dot = std::fs::metadata(".").ok()?;
            if let Ok(d) = std::fs::metadata(p(pwd)) {
                if d.dev() == dot.dev() && d.ino() == dot.ino() {
                    return Some(pwd.to_vec());
                }
            }
        }
    }
    std::env::current_dir()
        .ok()
        .map(|d| d.as_os_str().as_bytes().to_vec())
}

/// Go's os.TempDir.
pub fn temp_dir() -> Vec<u8> {
    match std::env::var_os("TMPDIR") {
        Some(d) if !d.is_empty() => d.as_bytes().to_vec(),
        _ => b"/tmp".to_vec(),
    }
}

/// Go's os.ReadFile.
pub fn read_file(path: &[u8]) -> Result<Vec<u8>, Vec<u8>> {
    let mut f = std::fs::File::open(p(path)).map_err(|e| path_error("open", path, &e))?;
    let mut b = Vec::new();
    f.read_to_end(&mut b)
        .map_err(|e| path_error("read", path, &e))?;
    Ok(b)
}

/// Go's os.WriteFile.
pub fn write_file(path: &[u8], data: &[u8], perm: u32) -> Result<(), Vec<u8>> {
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(perm)
        .open(p(path))
        .map_err(|e| path_error("open", path, &e))?;
    f.write_all(data).map_err(|e| path_error("write", path, &e))
}

/// Go's os.MkdirAll.
pub fn mkdir_all(path: &[u8], perm: u32) -> Result<(), Vec<u8>> {
    if let Ok(m) = std::fs::metadata(p(path)) {
        if m.is_dir() {
            return Ok(());
        }
        return Err(path_error(
            "mkdir",
            path,
            &std::io::Error::from_raw_os_error(20),
        ));
    }
    let mut i = path.len();
    while i > 0 && path[i - 1] == b'/' {
        i -= 1;
    }
    while i > 0 && path[i - 1] != b'/' {
        i -= 1;
    }
    let parent = if i == 0 { &path[..0] } else { &path[..i - 1] };
    if !parent.is_empty() {
        mkdir_all(parent, perm)?;
    }
    if let Err(e) = std::fs::DirBuilder::new().mode(perm).create(p(path)) {
        if let Ok(m) = std::fs::symlink_metadata(p(path)) {
            if m.is_dir() {
                return Ok(());
            }
        }
        return Err(path_error("mkdir", path, &e));
    }
    Ok(())
}

/// Go's os.Stat, for its error only.
pub fn stat(path: &[u8]) -> Result<std::fs::Metadata, Vec<u8>> {
    std::fs::metadata(p(path)).map_err(|e| path_error("stat", path, &e))
}

/// Go's exec.LookPath: the path to run, or exec.Error's text. `not_found`
/// is set when the error is exec.ErrNotFound.
pub struct LookErr {
    pub text: String,
    pub not_found: bool,
}

pub fn look_path(name: &[u8]) -> Result<Vec<u8>, LookErr> {
    let err = |why: &str, not_found: bool| LookErr {
        text: format!("exec: {}: {why}", crate::goerr::quote(name)),
        not_found,
    };
    let path = std::env::var_os("PATH").unwrap_or_default();
    let path = path.as_bytes();
    if !path.is_empty() {
        for d in path.split(|&c| c == b':') {
            let d: &[u8] = if d.is_empty() { b"." } else { d };
            let full = join(d, name);
            if executable(&full) {
                if full.first() != Some(&b'/') {
                    return Err(err(
                        "cannot run executable found relative to current directory",
                        false,
                    ));
                }
                return Ok(full);
            }
        }
    }
    Err(err("executable file not found in $PATH", true))
}

/// How a run ended.
pub struct Ran {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    /// None for a run that exited 0, else Go's error text.
    pub err: Option<String>,
    /// Set when the error is exec.ErrNotFound.
    pub not_found: bool,
    /// Set when the run outlived its timeout and was killed.
    pub timed_out: bool,
}

/// What exec.Command(argv...) runs and how. stdout and stderr go to one
/// pipe when `combined` is set (CombinedOutput); stdin is /dev/null when
/// `stdin` is None. `dir` is the working directory, if any.
pub struct Run<'a> {
    pub argv: &'a [Vec<u8>],
    pub stdin: Option<&'a [u8]>,
    pub combined: bool,
    pub dir: Option<&'a [u8]>,
    pub timeout: Option<Duration>,
}

impl Run<'_> {
    pub fn run(&self) -> Ran {
        let fail = |text: String, not_found: bool| Ran {
            stdout: Vec::new(),
            stderr: Vec::new(),
            err: Some(text),
            not_found,
            timed_out: false,
        };
        let name = &self.argv[0];
        let path = if name.contains(&b'/') {
            name.clone()
        } else {
            match look_path(name) {
                Ok(x) => x,
                Err(e) => return fail(e.text, e.not_found),
            }
        };
        if let Some(d) = self.dir {
            if let Err(e) = std::fs::metadata(p(d)) {
                return fail(
                    String::from_utf8_lossy(&path_error("chdir", d, &e)).into_owned(),
                    false,
                );
            }
        }
        let mut cmd = Command::new(OsStr::from_bytes(&path));
        cmd.arg0(OsStr::from_bytes(name));
        for a in &self.argv[1..] {
            cmd.arg(OsStr::from_bytes(a));
        }
        if let Some(d) = self.dir {
            cmd.current_dir(p(d));
        }
        cmd.stdin(if self.stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        });
        let mut joined = None;
        if self.combined {
            let (r, w) = match std::io::pipe() {
                Ok(x) => x,
                Err(e) => return fail(crate::goerr::io_reason(&e), false),
            };
            let w2 = match w.try_clone() {
                Ok(x) => x,
                Err(e) => return fail(crate::goerr::io_reason(&e), false),
            };
            cmd.stdout(w).stderr(w2);
            joined = Some(r);
        } else {
            cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
        }
        let started = Instant::now();
        let child = cmd.spawn();
        drop(cmd);
        let mut child = match child {
            Ok(c) => c,
            Err(e) => {
                return fail(
                    String::from_utf8_lossy(&path_error("fork/exec", &path, &e)).into_owned(),
                    false,
                )
            }
        };
        let feeder = match (child.stdin.take(), self.stdin) {
            (Some(mut w), Some(data)) => {
                let data = data.to_vec();
                Some(std::thread::spawn(move || {
                    let _ = w.write_all(&data);
                }))
            }
            _ => None,
        };
        let read = |mut r: Box<dyn Read + Send>| {
            std::thread::spawn(move || {
                let mut b = Vec::new();
                let _ = r.read_to_end(&mut b);
                b
            })
        };
        let (out_t, err_t) = match joined {
            Some(r) => (read(Box::new(r)), None),
            None => {
                let o = child
                    .stdout
                    .take()
                    .map(|x| Box::new(x) as Box<dyn Read + Send>);
                let e = child
                    .stderr
                    .take()
                    .map(|x| Box::new(x) as Box<dyn Read + Send>);
                (
                    read(o.unwrap_or_else(|| Box::new(std::io::empty()))),
                    Some(read(e.unwrap_or_else(|| Box::new(std::io::empty())))),
                )
            }
        };
        let mut timed_out = false;
        let mut nap = Duration::from_millis(1);
        let status = loop {
            match child.try_wait() {
                Ok(Some(st)) => break Some(st),
                Ok(None) => {}
                Err(_) => break None,
            }
            if let Some(t) = self.timeout {
                if started.elapsed() >= t {
                    let _ = child.kill();
                    timed_out = true;
                    break child.wait().ok();
                }
            }
            std::thread::sleep(nap);
            nap = (nap * 2).min(Duration::from_millis(10));
        };
        if let Some(f) = feeder {
            let _ = f.join();
        }
        let stdout = out_t.join().unwrap_or_default();
        let stderr = err_t
            .map(|t| t.join().unwrap_or_default())
            .unwrap_or_default();
        if let Some(t) = self.timeout {
            if started.elapsed() >= t {
                timed_out = true;
            }
        }
        let err = match status {
            Some(st) if st.success() => None,
            Some(st) => Some(exit_text(st)),
            None => Some("wait failed".to_string()),
        };
        Ran {
            stdout,
            stderr,
            err,
            not_found: false,
            timed_out,
        }
    }
}

extern "C" {
    fn faccessat(dirfd: i32, path: *const std::ffi::c_char, mode: i32, flags: i32) -> i32;
}

/// Go's findExecutable: not a directory, and executable for this process.
fn executable(path: &[u8]) -> bool {
    let Ok(m) = std::fs::metadata(p(path)) else {
        return false;
    };
    if m.is_dir() {
        return false;
    }
    let Ok(c) = std::ffi::CString::new(path) else {
        return false;
    };
    // AT_FDCWD is -100, X_OK is 1 and AT_EACCESS is 0x200 on Linux.
    let rc = unsafe { faccessat(-100, c.as_ptr(), 1, 0x200) };
    if rc == 0 {
        return true;
    }
    let e = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
    // ENOSYS (38) and EPERM (1): fall back to the mode bits, as Go does.
    (e == 38 || e == 1) && m.mode() & 0o111 != 0
}

/// Bytes from /dev/urandom, as hex.
pub fn random_hex(n: usize) -> String {
    let mut b = vec![0u8; n];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut b);
    }
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// Seconds since the epoch, as Go's float64(UnixNano())/1e9.
pub fn now() -> f64 {
    let d = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    d.as_nanos() as i64 as f64 / 1e9
}

/// stdout (1) or stderr (2) as a Write, through [`out`].
pub struct Fd(pub i32);

impl Write for Fd {
    fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
        out(self.0, b).map(|_| b.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

extern "C" {
    fn signal(signum: i32, handler: usize) -> usize;
    fn raise(sig: i32) -> i32;
}

/// Writes to stdout or stderr. A Go program that writes to a closed pipe
/// on fd 1 or 2 dies of SIGPIPE, so this does too.
pub fn out(fd: i32, b: &[u8]) -> std::io::Result<()> {
    let r = if fd == 1 {
        let mut o = std::io::stdout().lock();
        o.write_all(b).and_then(|_| o.flush())
    } else {
        std::io::stderr().lock().write_all(b)
    };
    if let Err(e) = &r {
        if e.kind() == std::io::ErrorKind::BrokenPipe {
            // SIGPIPE is 13 and SIG_DFL is 0 on Linux.
            unsafe {
                signal(13, 0);
                raise(13);
            }
        }
    }
    r
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn paths_follow_go() {
        assert_eq!(clean(b""), b".");
        assert_eq!(clean(b"a//b/./c/.."), b"a/b");
        assert_eq!(clean(b"/../x/"), b"/x");
        assert_eq!(clean(b"../../a"), b"../../a");
        assert_eq!(join(b"d/x/", b"../y.jsonl"), b"d/y.jsonl");
        assert_eq!(join(b"", b"a"), b"a");
        assert_eq!(dir(b"a/b/c.txt"), b"a/b");
        assert_eq!(dir(b"c.txt"), b".");
        assert_eq!(dir(b""), b".");
        assert_eq!(dir(b"/x"), b"/");
    }

    fn scratch(name: &str) -> Vec<u8> {
        let d = std::env::temp_dir().join(format!("sprig-osx-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d.as_os_str().as_bytes().to_vec()
    }

    #[test]
    fn files_follow_go() {
        let d = scratch("files");
        let f = join(&d, b"f");
        std::fs::write(p(&f), "x").unwrap();
        let under = join(&f, b"g/h");
        let want = [b"mkdir ".as_slice(), &f, b": not a directory"].concat();
        assert_eq!(mkdir_all(&under, 0o755).unwrap_err(), want);
        assert!(mkdir_all(&join(&d, b"a/b/c"), 0o755).is_ok());
        let missing = join(&d, b"nope");
        let want = [
            b"open ".as_slice(),
            &missing,
            b": no such file or directory",
        ]
        .concat();
        assert_eq!(read_file(&missing).unwrap_err(), want);
        let want = [b"read ".as_slice(), &d, b": is a directory"].concat();
        assert_eq!(read_file(&d).unwrap_err(), want);
        let want = [b"open ".as_slice(), &d, b": is a directory"].concat();
        assert_eq!(write_file(&d, b"x", 0o644).unwrap_err(), want);
        assert_eq!(
            read_file(b"").unwrap_err(),
            b"open : no such file or directory"
        );
        std::fs::remove_dir_all(p(&d)).unwrap();
    }

    #[test]
    fn runs_follow_go() {
        let sh = vec![
            b"sh".to_vec(),
            b"-c".to_vec(),
            b"echo a; echo b >&2; exit 3".to_vec(),
        ];
        let r = Run {
            argv: &sh,
            stdin: None,
            combined: true,
            dir: None,
            timeout: None,
        }
        .run();
        assert_eq!(r.stdout, b"a\nb\n");
        assert_eq!(r.err.as_deref(), Some("exit status 3"));
        let cat = vec![
            b"sh".to_vec(),
            b"-c".to_vec(),
            b"cat; echo; cat >&2 </dev/null".to_vec(),
        ];
        let r = Run {
            argv: &cat,
            stdin: Some(b"in"),
            combined: false,
            dir: None,
            timeout: None,
        }
        .run();
        assert_eq!(r.stdout, b"in\n");
        assert!(r.err.is_none());
        let none = vec![b"no-such-program-here".to_vec()];
        let r = Run {
            argv: &none,
            stdin: None,
            combined: false,
            dir: None,
            timeout: None,
        }
        .run();
        assert_eq!(
            r.err.as_deref(),
            Some("exec: \"no-such-program-here\": executable file not found in $PATH")
        );
        assert!(r.not_found);
        let bad = vec![b"./no/such".to_vec()];
        let r = Run {
            argv: &bad,
            stdin: None,
            combined: false,
            dir: None,
            timeout: None,
        }
        .run();
        assert_eq!(
            r.err.as_deref(),
            Some("fork/exec ./no/such: no such file or directory")
        );
        let slow = vec![b"sleep".to_vec(), b"5".to_vec()];
        let t = Duration::from_millis(200);
        let r = Run {
            argv: &slow,
            stdin: None,
            combined: false,
            dir: None,
            timeout: Some(t),
        }
        .run();
        assert!(r.timed_out);
        assert_eq!(r.err.as_deref(), Some("signal: killed"));
    }
}
