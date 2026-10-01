//! The resident engine protocol, daisugi-voice-1, and how the server runs
//! the child. The one definition is src/opendaisugi/voice/resident.py;
//! this file follows it.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::process::ExitStatusExt;
use std::process::{ChildStdin, Command, Stdio};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

use super::engine::EngineError;

pub const PROTOCOL: &str = "daisugi-voice-1";
pub const MAX_FRAME_BYTES: u64 = 64 * 1024 * 1024;
pub const LOAD_TIMEOUT_S: f64 = 300.0;
pub const CLIP_TIMEOUT_S: f64 = 120.0;
pub const STOP_GRACE_S: f64 = 2.0;
const STDERR_LINES: usize = 20;

const LOADING: &str = "The speech engine is loading its model. Try again in a moment.";
const AGAIN: &str = "It is starting again. Try again in a moment.";

/// One request: the clip's length, big-endian, then the clip.
pub fn frame(wav: &[u8]) -> Vec<u8> {
    let mut out = (wav.len() as u32).to_be_bytes().to_vec();
    out.extend_from_slice(wav);
    out
}

/// `resident.parse_line`: the line as a JSON object, or None.
pub fn parse_line(raw: &[u8]) -> Option<Object> {
    let t = std::str::from_utf8(raw).ok()?;
    match pyjson::loads_py(t, 900) {
        Ok(Value::Obj(o)) => Some(o),
        _ => None,
    }
}

/// `resident.is_ready`.
pub fn is_ready(o: Option<&Object>) -> bool {
    matches!(o.and_then(|o| o.get("ready")), Some(Value::Str(s)) if s == PROTOCOL)
}

/// `resident.reply_kind`: "text", "error", or None.
pub fn reply_kind(o: Option<&Object>) -> Option<&'static str> {
    let o = o?;
    if matches!(o.get("text"), Some(Value::Str(_))) {
        return Some("text");
    }
    if matches!(o.get("error"), Some(Value::Str(_))) {
        return Some("error");
    }
    None
}

fn str_field(o: &Object, k: &str) -> String {
    match o.get(k) {
        Some(Value::Str(s)) => s.clone(),
        _ => String::new(),
    }
}

/// `resident.exit_reason`. `rc` is negative for a signal.
pub fn exit_reason(rc: i32, tail: &[String]) -> String {
    let what = if rc < 0 { format!("killed by signal {}", -rc) } else { format!("exited {rc}") };
    match tail.last().map(|l| text::head(text::strip(l), 200).to_string()) {
        Some(last) if !last.is_empty() => format!("{what}: {last}"),
        _ => what,
    }
}

/// `format(f, "g")`.
pub fn py_g(f: f64) -> String {
    if f == 0.0 {
        return "0".into();
    }
    let sci = format!("{f:.5e}");
    let (mant, exp) = sci.split_once('e').unwrap_or((&sci, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    let trim = |s: &str| -> String {
        if s.contains('.') {
            s.trim_end_matches('0').trim_end_matches('.').to_string()
        } else {
            s.to_string()
        }
    };
    if (-4..6).contains(&exp) {
        let decimals = (5 - exp).max(0) as usize;
        trim(&format!("{f:.decimals$}"))
    } else {
        let sign = if exp < 0 { '-' } else { '+' };
        format!("{}e{sign}{:02}", trim(mant), exp.abs())
    }
}

/// The child's command line, its extra environment, the name messages use,
/// and the two timeouts in seconds.
#[derive(Clone)]
pub struct ResidentConfig {
    pub binary: String,
    pub argv: Vec<String>,
    pub env: Vec<(String, String)>,
    pub load_timeout_s: f64,
    pub clip_timeout_s: f64,
}

struct Child {
    pid: i32,
    stdin: Mutex<Option<ChildStdin>>,
    lines: Mutex<Receiver<Vec<u8>>>,
    /// The tail of stderr, and the exit code once the child has ended.
    end: Mutex<(Vec<String>, Option<i32>)>,
    ended: Condvar,
    ready: Mutex<bool>,
}

impl Child {
    fn kill(&self) {
        if self.end.lock().unwrap().1.is_none() {
            // SAFETY: kill(2) on a child this process has not reaped yet.
            unsafe { libc::kill(self.pid, libc::SIGKILL) };
        }
    }

    /// How the child ended. It waits until it has.
    fn reason(&self) -> String {
        let mut g = self.end.lock().unwrap();
        while g.1.is_none() {
            g = self.ended.wait(g).unwrap();
        }
        exit_reason(g.1.unwrap_or(0), &g.0)
    }

    fn wait_end(&self, limit: Duration) -> bool {
        let g = self.end.lock().unwrap();
        let (g, _) = self.ended.wait_timeout_while(g, limit, |g| g.1.is_none()).unwrap();
        g.1.is_some()
    }
}

#[derive(PartialEq, Clone, Copy)]
enum State {
    New,
    Loading,
    Ready,
    Failed,
    Stopped,
}

impl State {
    fn name(self) -> &'static str {
        match self {
            State::New => "new",
            State::Loading => "loading",
            State::Ready => "ready",
            State::Failed => "failed",
            State::Stopped => "stopped",
        }
    }
}

struct Shared {
    state: State,
    child: Option<Arc<Child>>,
    why: String,
}

struct Inner {
    cfg: ResidentConfig,
    shared: Mutex<Shared>,
    changed: Condvar,
    clip: Mutex<()>,
}

/// `resident.ResidentEngine`.
#[derive(Clone)]
pub struct Resident(Arc<Inner>);

fn unavailable(msg: String) -> EngineError {
    EngineError { unknown: false, loading: false, msg }
}

/// Python's OSError.strerror for an exec failure.
fn strerror(e: &std::io::Error) -> String {
    match e.raw_os_error() {
        Some(code) => {
            // SAFETY: strerror returns a static string for any code.
            let s = unsafe { std::ffi::CStr::from_ptr(libc::strerror(code)) };
            s.to_string_lossy().into_owned()
        }
        None => e.to_string(),
    }
}

fn secs(f: f64) -> Duration {
    Duration::from_secs_f64(f.max(0.0))
}

impl Resident {
    pub fn new(cfg: ResidentConfig) -> Resident {
        Resident(Arc::new(Inner {
            cfg,
            shared: Mutex::new(Shared { state: State::New, child: None, why: String::new() }),
            changed: Condvar::new(),
            clip: Mutex::new(()),
        }))
    }

    fn spawn(&self) -> std::io::Result<Arc<Child>> {
        let cfg = &self.0.cfg;
        let mut cmd = Command::new(&cfg.argv[0]);
        cmd.args(&cfg.argv[1..]).stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::piped());
        for (k, v) in &cfg.env {
            cmd.env(k, v);
        }
        let mut proc = cmd.spawn()?;
        let stdout = proc.stdout.take().expect("piped stdout");
        let stderr = proc.stderr.take().expect("piped stderr");
        let (tx, rx) = mpsc::channel();
        let child = Arc::new(Child {
            pid: proc.id() as i32,
            stdin: Mutex::new(proc.stdin.take()),
            lines: Mutex::new(rx),
            end: Mutex::new((Vec::new(), None)),
            ended: Condvar::new(),
            ready: Mutex::new(false),
        });
        let errc = child.clone();
        let err_thread = std::thread::spawn(move || {
            let mut rd = BufReader::new(stderr);
            let mut buf = Vec::new();
            loop {
                buf.clear();
                match rd.read_until(b'\n', &mut buf) {
                    Ok(0) | Err(_) => return,
                    Ok(_) => {
                        let line = text::decode_utf8_replace(buf.strip_suffix(b"\n").unwrap_or(&buf));
                        if !text::strip(&line).is_empty() {
                            let mut g = errc.end.lock().unwrap();
                            g.0.push(line);
                            if g.0.len() > STDERR_LINES {
                                g.0.remove(0);
                            }
                        }
                    }
                }
            }
        });
        let me = self.clone();
        let outc = child.clone();
        std::thread::spawn(move || {
            let mut rd = BufReader::new(stdout);
            loop {
                let mut buf = Vec::new();
                match rd.read_until(b'\n', &mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(_) => {
                        if tx.send(buf).is_err() {
                            break;
                        }
                    }
                }
            }
            let _ = err_thread.join();
            let status = proc.wait();
            let rc = match status {
                Ok(s) => match s.signal() {
                    Some(sig) => -sig,
                    None => s.code().unwrap_or(0),
                },
                Err(_) => 0,
            };
            {
                let mut g = outc.end.lock().unwrap();
                g.1 = Some(rc);
            }
            outc.ended.notify_all();
            drop(tx);
            me.exited(&outc);
        });
        Ok(child)
    }

    /// Waits for the ready line: None when the child is ready, else why not.
    fn load(&self, c: &Arc<Child>) -> Option<String> {
        let got = c.lines.lock().unwrap().recv_timeout(secs(self.0.cfg.load_timeout_s));
        match got {
            Err(RecvTimeoutError::Timeout) => {
                c.kill();
                Some(format!("it did not load its model in {} seconds", py_g(self.0.cfg.load_timeout_s)))
            }
            Err(RecvTimeoutError::Disconnected) => Some(c.reason()),
            Ok(raw) => {
                let o = parse_line(&raw);
                if is_ready(o.as_ref()) {
                    return None;
                }
                c.kill();
                if let Some(Value::Str(e)) = o.as_ref().and_then(|o| o.get("error")) {
                    return Some(text::head(text::strip(e), 200).to_string());
                }
                Some(format!("its first line was not the {PROTOCOL} ready line"))
            }
        }
    }

    /// Starts the child and waits until it is ready.
    pub fn start(&self) -> Result<(), EngineError> {
        let bin = &self.0.cfg.binary;
        let c = self.spawn().map_err(|e| unavailable(format!("{bin} did not start: {}", strerror(&e))))?;
        {
            let mut g = self.0.shared.lock().unwrap();
            g.child = Some(c.clone());
            g.state = State::Loading;
        }
        let why = self.load(&c);
        {
            let mut g = self.0.shared.lock().unwrap();
            if why.is_none() {
                *c.ready.lock().unwrap() = true;
                g.state = State::Ready;
            } else {
                g.state = State::Stopped;
                g.child = None;
            }
        }
        self.0.changed.notify_all();
        match why {
            None => Ok(()),
            Some(w) => Err(unavailable(format!("{bin} did not start: {w}"))),
        }
    }

    fn is_current(g: &Shared, old: Option<&Arc<Child>>) -> bool {
        match (&g.child, old) {
            (None, None) => true,
            (Some(a), Some(b)) => Arc::ptr_eq(a, b),
            _ => false,
        }
    }

    /// Starts a new child in place of `old`, loading in the background,
    /// unless `old` is no longer the current child.
    fn restart(&self, old: Option<&Arc<Child>>) {
        let c = {
            let mut g = self.0.shared.lock().unwrap();
            if g.state == State::Stopped || !Self::is_current(&g, old) {
                return;
            }
            if let Some(o) = old {
                o.kill();
            }
            match self.spawn() {
                Ok(c) => {
                    g.child = Some(c.clone());
                    g.state = State::Loading;
                    c
                }
                Err(e) => {
                    g.child = None;
                    g.state = State::Failed;
                    g.why = format!("{} did not start: {}", self.0.cfg.binary, strerror(&e));
                    drop(g);
                    self.0.changed.notify_all();
                    return;
                }
            }
        };
        let me = self.clone();
        std::thread::spawn(move || {
            let why = me.load(&c);
            let mut g = me.0.shared.lock().unwrap();
            if g.state == State::Stopped || !Self::is_current(&g, Some(&c)) {
                return;
            }
            match why {
                None => {
                    *c.ready.lock().unwrap() = true;
                    g.state = State::Ready;
                }
                Some(w) => {
                    c.kill();
                    g.child = None;
                    g.state = State::Failed;
                    g.why = w;
                }
            }
            drop(g);
            me.0.changed.notify_all();
        });
    }

    fn exited(&self, c: &Arc<Child>) {
        let again = {
            let g = self.0.shared.lock().unwrap();
            Self::is_current(&g, Some(c)) && g.state == State::Ready && *c.ready.lock().unwrap()
        };
        if again {
            self.restart(Some(c));
        }
    }

    /// Waits while a child loads; the state then.
    pub fn wait_ready(&self, timeout_s: f64) -> &'static str {
        let g = self.0.shared.lock().unwrap();
        let (g, _) = self.0.changed.wait_timeout_while(g, secs(timeout_s), |g| g.state == State::Loading).unwrap();
        g.state.name()
    }

    fn lost(&self, c: &Arc<Child>, what: &str) -> EngineError {
        self.restart(Some(c));
        unavailable(format!("The speech engine {what}. {AGAIN}"))
    }

    /// The child's text for one clip. `Err(Ok(e))` is an EngineError
    /// (loading or unavailable); `Err(Err(msg))` a clip the child refused.
    pub fn transcribe_text(&self, wav: &[u8]) -> Result<String, Result<EngineError, String>> {
        let _clip = self.0.clip.lock().unwrap();
        let (state, c, why) = {
            let g = self.0.shared.lock().unwrap();
            (g.state, g.child.clone(), g.why.clone())
        };
        match state {
            State::Loading => {
                return Err(Ok(EngineError { unknown: false, loading: true, msg: LOADING.into() }));
            }
            State::Failed => {
                self.restart(None);
                return Err(Ok(unavailable(format!(
                    "The speech engine did not start again: {why}. The next clip tries again."
                ))));
            }
            _ => {}
        }
        let Some(c) = c.filter(|_| state == State::Ready) else {
            return Err(Ok(unavailable("The speech engine has stopped.".into())));
        };
        // The clip timeout covers the write too: a child that stops reading
        // its stdin would block a clip bigger than the pipe.
        let deadline = Instant::now() + secs(self.0.cfg.clip_timeout_s);
        let (wtx, wrx) = mpsc::channel::<()>();
        let (wc, data) = (c.clone(), frame(wav));
        std::thread::spawn(move || {
            if let Some(w) = wc.stdin.lock().unwrap().as_mut() {
                let _ = w.write_all(&data).and_then(|_| w.flush());
            }
            let _ = wtx.send(());
        });
        if let Err(RecvTimeoutError::Timeout) = wrx.recv_timeout(secs(self.0.cfg.clip_timeout_s)) {
            return Err(Ok(self.lost(&c, &format!("did not answer in {} seconds", py_g(self.0.cfg.clip_timeout_s)))));
        }
        let left = deadline.saturating_duration_since(Instant::now());
        let got = c.lines.lock().unwrap().recv_timeout(left);
        match got {
            Err(RecvTimeoutError::Timeout) => {
                Err(Ok(self.lost(&c, &format!("did not answer in {} seconds", py_g(self.0.cfg.clip_timeout_s)))))
            }
            Err(RecvTimeoutError::Disconnected) => {
                let r = c.reason();
                Err(Ok(self.lost(&c, &format!("stopped during a clip ({r})"))))
            }
            Ok(raw) => {
                let o = parse_line(&raw);
                match reply_kind(o.as_ref()) {
                    Some("text") => Ok(str_field(o.as_ref().unwrap(), "text")),
                    Some(_) => {
                        let e = str_field(o.as_ref().unwrap(), "error");
                        Err(Err(format!("{}: {}", self.0.cfg.binary, text::head(text::strip(&e), 200))))
                    }
                    None => Err(Ok(self.lost(&c, "gave a reply that is not one JSON object with text"))),
                }
            }
        }
    }

    /// Closes the child's stdin, waits a little, then kills it.
    pub fn stop(&self) {
        let c = {
            let mut g = self.0.shared.lock().unwrap();
            g.state = State::Stopped;
            g.child.take()
        };
        self.0.changed.notify_all();
        let Some(c) = c else { return };
        // A writer stuck on a child that stopped reading holds the stdin
        // lock; such a child is killed instead.
        match c.stdin.try_lock() {
            Ok(mut g) => drop(g.take()),
            Err(_) => c.kill(),
        }
        if !c.wait_end(secs(STOP_GRACE_S)) {
            c.kill();
            c.wait_end(Duration::from_secs(30));
        }
    }

    /// The current child's process id.
    pub fn pid(&self) -> Option<i32> {
        self.0.shared.lock().unwrap().child.as_ref().map(|c| c.pid)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_frame_is_the_big_endian_length_then_the_clip() {
        assert_eq!(frame(b"abc"), b"\x00\x00\x00\x03abc".to_vec());
    }

    #[test]
    fn reply_lines() {
        let k = |l: &[u8]| reply_kind(parse_line(l).as_ref());
        assert_eq!(k(b"{\"text\": \"hi\", \"decode_ms\": 3}\n"), Some("text"));
        assert_eq!(k(b"{\"error\": \"bad\"}"), Some("error"));
        assert_eq!(k(b"{\"text\": 3}"), None);
        assert_eq!(k(b"[1]"), None);
        assert_eq!(k(b"\xff"), None);
        assert!(is_ready(parse_line(b"{\"ready\": \"daisugi-voice-1\"}").as_ref()));
        assert!(!is_ready(parse_line(b"{\"ready\": \"daisugi-voice-2\"}").as_ref()));
    }

    #[test]
    fn exit_reasons() {
        assert_eq!(exit_reason(4, &["a".into(), "boom ".into()]), "exited 4: boom");
        assert_eq!(exit_reason(-9, &[]), "killed by signal 9");
    }

    /// A fake engine in /bin/sh that answers every frame with one line. It
    /// cannot read binary frames, so it reads the 4-byte head and the clip
    /// with dd; good enough for a 16-byte clip.
    fn sh_engine(dir: &std::path::Path, body: &str) -> String {
        let p = dir.join("fake");
        std::fs::write(&p, format!("#!/bin/sh\n{body}")).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
        p.to_string_lossy().into_owned()
    }

    fn engine(argv0: String, load: f64) -> Resident {
        Resident::new(ResidentConfig {
            binary: "fake-cli".into(),
            argv: vec![argv0],
            env: vec![],
            load_timeout_s: load,
            clip_timeout_s: 10.0,
        })
    }

    #[test]
    fn load_clip_crash_and_restart() {
        let dir = std::env::temp_dir().join(format!("resident-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let fake = sh_engine(
            &dir,
            "echo '{\"ready\": \"daisugi-voice-1\"}'\n\
             /bin/dd bs=20 count=1 status=none of=/dev/null\n\
             echo '{\"text\": \"one\"}'\n\
             /bin/dd bs=20 count=1 status=none of=/dev/null\n\
             echo died >&2\nexit 7\n",
        );
        let r = engine(fake, 10.0);
        r.start().unwrap();
        assert_eq!(r.transcribe_text(&[0u8; 16]).unwrap(), "one");
        match r.transcribe_text(&[0u8; 16]) {
            Err(Ok(e)) => assert!(e.msg.contains("stopped during a clip (exited 7: died). It is starting again."), "{}", e.msg),
            _ => panic!("want unavailable"),
        }
        assert_eq!(r.wait_ready(10.0), "ready");
        r.stop();
        match r.transcribe_text(&[0u8; 16]) {
            Err(Ok(e)) => assert_eq!(e.msg, "The speech engine has stopped."),
            _ => panic!("want stopped"),
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn load_failures_name_the_reason() {
        let dir = std::env::temp_dir().join(format!("resident-fail-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let fake = sh_engine(&dir, "echo '{\"error\": \"the model /m did not load\"}'\nexit 3\n");
        assert_eq!(engine(fake, 10.0).start().unwrap_err().msg, "fake-cli did not start: the model /m did not load");
        let fake = sh_engine(&dir, "echo one >&2\necho boom >&2\nexit 4\n");
        assert_eq!(engine(fake, 10.0).start().unwrap_err().msg, "fake-cli did not start: exited 4: boom");
        let fake = sh_engine(&dir, "/bin/sleep 5\n");
        let t0 = Instant::now();
        assert_eq!(
            engine(fake, 0.5).start().unwrap_err().msg,
            "fake-cli did not start: it did not load its model in 0.5 seconds"
        );
        assert!(t0.elapsed() < Duration::from_secs(4));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
