//! `gate._maybe_ask` and the file protocol of `opendaisugi.ask`: a
//! would-deny is handed to a present operator for at most --ask-timeout.
//! No operator, no tool_use_id, a deny, a late or a forged answer all
//! leave the deny standing; only an answer that echoes this ask's nonce
//! and is no older than it can allow.

use super::decide::Decision;
use super::envelope::Envelope;
use super::effects::UNDOABLE;
use super::paths::{os_error, os_path, path_str};
use super::statefile::{self, Miss};
use super::statewrite;
use super::pymodel::decode_utf8_strict;
use super::pyjson::{dumps, loads, py_str, LoadError, Object, Value};
use super::{random_hex, undecided, Fault, PyErr, Runner, R};
use std::os::fd::{AsRawFd, OwnedFd};
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

/// `gate.EDIT_REFUSAL`.
pub const EDIT_REFUSAL: &str = "the operator's edit does not pass the gate";

const PRESENCE: &str = "operator.json";
const ASKS: &str = "asks";
const ANSWERS: &str = "answers";
const PROPOSALS: &str = "proposals";

/// `ask._safe(raw)`: `str(raw or "")` with every character but ASCII
/// letters, digits, `.`, `_` and `-` made `_`, dots stripped from the
/// ends, at most 128 characters, or `none`.
fn safe(raw: &str) -> String {
    let s: String =
        raw.chars().map(|c| if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') { c } else { '_' }).collect();
    let s: String = s.trim_matches('.').chars().take(128).collect();
    if s.is_empty() {
        "none".into()
    } else {
        s
    }
}

fn io(e: std::io::Error, p: &Path) -> Fault {
    Fault::Raised(os_error(&e, &p.to_string_lossy()))
}

/// `ask._load`: the JSON object in an open file and its mtime, both from
/// the fd. A file that is not readable JSON, or not an object, is None.
fn load(fd: OwnedFd) -> R<(Option<Object>, f64)> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    let have = unsafe { libc::fstat(fd.as_raw_fd(), &mut st) } == 0;
    let mtime = if have { st.st_mtime as f64 + st.st_mtime_nsec as f64 / 1e9 } else { 0.0 };
    let raw = match statefile::read_all(fd) {
        Ok(r) => r,
        Err(_) => return Ok((None, mtime)),
    };
    let text = match decode_utf8_strict(&raw) {
        Ok(t) => t,
        Err(_) => return Ok((None, mtime)),
    };
    match loads(&text) {
        Ok(Value::Obj(o)) => Ok((Some(o), mtime)),
        Ok(_) => Ok((None, mtime)),
        // json.loads raises RecursionError, which is no ValueError.
        Err(LoadError::Recursion) => undecided("an ask file nested past Python's recursion limit"),
        Err(_) => Ok((None, mtime)),
    }
}

/// `os.kill(pid, 0)` for `_pid_alive`, with the OverflowError Python's
/// argument conversion raises.
fn pid_alive(pid: &Value) -> R<bool> {
    let n: i128 = match pid {
        Value::Bool(b) => *b as i128,
        Value::Int(t) => match t.parse::<i128>() {
            Ok(n) => n,
            Err(_) => return Err(PyErr::new("OverflowError", "Python int too large to convert to C long").into()),
        },
        _ => return Ok(false),
    };
    if n > i64::MAX as i128 || n < i64::MIN as i128 {
        return Err(PyErr::new("OverflowError", "Python int too large to convert to C long").into());
    }
    if n > i32::MAX as i128 {
        return Err(PyErr::new("OverflowError", "signed integer is greater than maximum").into());
    }
    if n < i32::MIN as i128 {
        return Err(PyErr::new("OverflowError", "signed integer is less than minimum").into());
    }
    let r = unsafe { libc::kill(n as i32, 0) };
    if r == 0 {
        return Ok(true);
    }
    let errno = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
    Ok(errno != libc::ESRCH)
}

/// Python's `float(x)` for an ask's deadline: `None` for the TypeError or
/// ValueError `_sweep_expired` catches.
fn py_float(v: &Value) -> R<Option<f64>> {
    Ok(match v {
        Value::Bool(b) => Some(if *b { 1.0 } else { 0.0 }),
        Value::Int(t) => {
            let f: f64 = t.parse().unwrap_or(f64::INFINITY);
            if f.is_infinite() {
                return Err(PyErr::new("OverflowError", "int too large to convert to float").into());
            }
            Some(f)
        }
        Value::Float(f) => Some(*f),
        Value::Str(s) => super::argparse::py_float(s),
        _ => None,
    })
}

/// `ask.who_of(by, who_from)`.
pub fn who_of(by: &Value, who_from: &Value) -> (String, String) {
    let local = ("local".to_string(), "none".to_string());
    let (by, wf) = match (by, who_from) {
        (Value::Str(b), Value::Str(w)) => (b.as_str(), w.as_str()),
        _ => return local,
    };
    let charset = |s: &str, extra: &str, max: usize| {
        let n = s.chars().count();
        n >= 1 && n <= max && s.chars().all(|c| c.is_ascii_alphanumeric() || extra.contains(c))
    };
    let ok = match wf {
        "token" | "socket" => charset(by, "_-", 32),
        "pane" => by == "pane" || by.strip_prefix("pane:").is_some_and(|r| charset(r, "._:-", 128)),
        "plugin" => by.strip_prefix("plugin:").is_some_and(|r| charset(r, "._:-", 128)),
        _ => false,
    };
    if ok {
        (by.to_string(), wf.to_string())
    } else {
        local
    }
}

fn now() -> f64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0)
}

/// `os.unlink(name, dir_fd=fd)` with FileNotFoundError ignored and any other
/// OSError raised.
fn unlink_at_missing_ok(dirfd: i32, name: &str, shown: &Path) -> R<()> {
    let c = match std::ffi::CString::new(name) {
        Ok(c) => c,
        Err(_) => return Ok(()),
    };
    if unsafe { libc::unlinkat(dirfd, c.as_ptr(), 0) } == 0 {
        return Ok(());
    }
    let e = std::io::Error::last_os_error();
    if e.kind() == std::io::ErrorKind::NotFound {
        Ok(())
    } else {
        Err(io(e, shown))
    }
}

/// True when `name` is an entry in the directory `dirfd`, a dangling link
/// included.
fn entry_exists(dirfd: i32, name: &str) -> bool {
    let Ok(c) = std::ffi::CString::new(name) else {
        return false;
    };
    let fd = unsafe { libc::openat(dirfd, c.as_ptr(), libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC) };
    if fd < 0 {
        return false;
    }
    unsafe { libc::close(fd) };
    true
}

impl Runner {
    fn gate_root(&self) -> PathBuf {
        match os_path("open", &self.root) {
            Ok(p) => p,
            Err(_) => PathBuf::from(&self.root),
        }
    }

    /// The gate root as the statefile routines spell it.
    fn ask_root(&self) -> String {
        path_str(&self.root)
    }

    /// `ask._read`: the body and, for a file that opened, its mtime, through
    /// the checked open (GR-R-10). Missing and refused both read as no file.
    fn ask_read(&self, dirs: &[&str], name: &str) -> R<(Option<Object>, Option<f64>)> {
        match statefile::probe(&self.ask_root(), dirs, name, None) {
            Err(_) => Ok((None, None)),
            Ok(fd) => {
                let (body, m) = load(fd)?;
                Ok((body, Some(m)))
            }
        }
    }

    /// `ask._write`: the body written through the directory fd, a link at
    /// the name replaced. False when the gate refuses the path.
    fn ask_write(&self, sub: &str, name: &str, body: &Object) -> R<bool> {
        let text = dumps(&Value::Obj(body.clone()), true);
        match statewrite::write_file(&self.ask_root(), &[sub], name, &text, true) {
            Ok(_) => Ok(true),
            Err(e) if statewrite::write_refused(&e).is_some() => Ok(false),
            Err(e) => Err(io(e, &self.gate_root().join(sub).join(name))),
        }
    }

    /// `ask.channel_ok`: false when asks/ or answers/ exists and the gate
    /// refuses it. Missing is fine: the first ask makes them.
    fn channel_ok(&self) -> bool {
        for sub in [ASKS, ANSWERS] {
            if let Err(Miss::Refused(_)) = statefile::probe_dirs(&self.ask_root(), &[sub]) {
                return false;
            }
        }
        true
    }

    /// `ask._drop`: `<root>/<sub>/<name>` removed through the directory fd.
    /// A directory the gate refuses is left alone; any other failure raises.
    fn ask_drop(&self, sub: &str, name: &str) -> R<()> {
        match statewrite::unlink_file(&self.ask_root(), &[sub], name) {
            Ok(_) => Ok(()),
            Err(e) if statewrite::write_refused(&e).is_some() => Ok(()),
            Err(e) => Err(io(e, &self.gate_root().join(sub).join(name))),
        }
    }

    /// `ask.operator_present(root)`, read through the checked open.
    fn operator_present(&self) -> R<bool> {
        let (body, mtime) = self.ask_read(&[], PRESENCE)?;
        let (Some(body), Some(m)) = (body, mtime) else {
            return Ok(false);
        };
        if now() - m > 15.0 {
            return Ok(false);
        }
        match body.value("pid") {
            v @ (Value::Int(_) | Value::Bool(_)) => pid_alive(v),
            _ => Ok(false),
        }
    }

    /// `ask._sweep_expired(root, now=now)`.
    fn sweep_expired(&self, at: f64) -> R<()> {
        let root = self.ask_root();
        let afds = statefile::probe_dirs(&root, &[ASKS]).ok();
        let asks_fd = afds.as_ref().and_then(|f| f.last()).map(|f| f.as_raw_fd());
        if let Some(afd) = asks_fd {
            let note_dir = statefile::note_dir(&root, &[ASKS]);
            for name in statewrite::list_json(afd) {
                let body = match statefile::probe_file_in(afd, &note_dir, &name, None) {
                    Ok(fd) => load(fd)?.0,
                    Err(_) => None,
                };
                let deadline = match &body {
                    Some(b) => match b.get("deadline") {
                        None => 0.0,
                        Some(v) => py_float(v)?.unwrap_or(0.0),
                    },
                    None => 0.0,
                };
                if body.is_none() || deadline <= at {
                    unlink_at_missing_ok(afd, &name, &self.gate_root().join(ASKS).join(&name))?;
                    self.ask_drop(ANSWERS, &name)?;
                }
            }
        }
        if let Ok(wfds) = statefile::probe_dirs(&root, &[ANSWERS]) {
            if let Some(wfd) = wfds.last().map(|f| f.as_raw_fd()) {
                for name in statewrite::list_json(wfd) {
                    if asks_fd.is_none_or(|a| !entry_exists(a, &name)) {
                        unlink_at_missing_ok(wfd, &name, &self.gate_root().join(ANSWERS).join(&name))?;
                    }
                }
            }
        }
        Ok(())
    }

    /// `ask._consume(root, tool_use_id)`.
    fn consume(&self, tid: &str) -> R<()> {
        for sub in [ASKS, ANSWERS] {
            self.ask_drop(sub, &format!("{}.json", safe(tid)))?;
        }
        Ok(())
    }

    /// `ask.wait_answer(root, tool_use_id=tid, timeout_s=...)`. An answer
    /// file that is there but refused, or a refused answers/, is not an
    /// answer: the wait ends with none.
    fn wait_answer(&self, tid: &str, timeout_s: f64) -> R<Option<Object>> {
        let name = format!("{}.json", safe(tid));
        let (ask_body, ask_mtime) = self.ask_read(&[ASKS], &name)?;
        let expected = ask_body.map(|b| b.value("nonce").clone()).unwrap_or(Value::Null);
        let t_end = Instant::now() + Duration::from_secs_f64(timeout_s.clamp(0.0, 1e9));
        loop {
            let probed = statefile::probe(&self.ask_root(), &[ANSWERS], &name, None);
            if !matches!(probed, Err(Miss::Missing)) {
                let (body, answer_mtime) = match probed {
                    Ok(fd) => {
                        let (b, m) = load(fd)?;
                        (b, Some(m))
                    }
                    Err(_) => (None, None),
                };
                let valid = match &body {
                    Some(b) => {
                        let decision = b.value("decision");
                        (decision == &Value::Str("allow".into()) || decision == &Value::Str("deny".into()))
                            && !expected.is_null()
                            && super::predicate::py_eq(b.value("nonce"), &expected)
                            && match (ask_mtime, answer_mtime) {
                                (Some(a), Some(b)) => b >= a,
                                _ => false,
                            }
                    }
                    None => false,
                };
                self.consume(tid)?;
                if !valid {
                    return Ok(None);
                }
                let b = body.unwrap_or_default();
                let mut out = Object::new();
                for k in b.keys() {
                    if k != "nonce" {
                        out.set(k, b.value(k).clone());
                    }
                }
                return Ok(Some(out));
            }
            if Instant::now() >= t_end {
                self.consume(tid)?;
                return Ok(None);
            }
            std::thread::sleep(Duration::from_millis(200));
        }
    }

    /// `gate._recheck_edit`: the operator's edit is a new call, so it goes
    /// through the whole decision again. An edit that would be denied, or
    /// that is not a JSON object, is denied, with no second ask.
    pub fn recheck_edit(&self, p: &Object, d: Decision, env: &Envelope) -> R<Decision> {
        let edit = match &d.updated_input {
            Some(Value::Obj(o)) => o.clone(),
            _ => {
                let mut d = d;
                d.allow = self.mode == "audit";
                d.reason = format!("{EDIT_REFUSAL}: the edit is not a JSON object");
                d.updated_input = None;
                return Ok(d);
            }
        };
        let mut edited = p.clone();
        edited.set("tool_input", Value::Obj(edit));
        let again = self.evaluate_call(&edited, env)?;
        if !again.would_deny {
            return Ok(d);
        }
        let mut out = again;
        out.ask = true;
        out.reason = format!("{EDIT_REFUSAL}: {}", out.reason);
        out.answered_by = d.answered_by;
        out.who_from = d.who_from;
        Ok(out)
    }

    /// `gate._maybe_ask`.
    pub fn maybe_ask(&self, p: &Object, d: Decision, session_id: &Value) -> R<Decision> {
        let tool_use_id = p.value("tool_use_id");
        if !tool_use_id.truthy() || !self.operator_present()? {
            return Ok(d);
        }
        // GR-R-10: an asks/ or answers/ the gate refuses (a link, another
        // user's, writable by others) takes no ask, so the deny stands.
        if !self.channel_ok() {
            return Ok(d);
        }
        if !self.ask_timeout_s.is_finite() {
            // int(timeout_s) in the reason raises, and an infinite wait
            // is no answer the port can give.
            return undecided("an --ask-timeout that is not finite");
        }
        let tid = py_str(tool_use_id)?;
        let deadline = now() + self.ask_timeout_s;
        // post_ask
        self.sweep_expired(now())?;
        let tool_name = match &d.tool_name {
            Some(t) => Value::Str(t.clone()),
            None => Value::Null,
        };
        let body = Object::new()
            .with("toolUseId", tid.as_str())
            .with("nonce", random_hex(16)?)
            .with("postedAt", now())
            .with("deadline", deadline)
            .with("sessionId", p.value("session_id").clone())
            .with("toolName", tool_name)
            .with("detail", d.detail.as_str())
            .with("reason", d.reason.as_str())
            .with("clause", d.clause())
            .with("counterexample", d.counterexample())
            .with("toolInput", p.value("tool_input").clone())
            .with("tier", d.tier.as_str());
        if !self.ask_write(ASKS, &format!("{}.json", safe(&tid)), &body)? {
            return Ok(d);
        }
        let _ = super::best_effort(self.report_blocked(p, &d, session_id, &tid, deadline));
        let remaining = (deadline - now()).max(0.0);
        let reply = self.wait_answer(&tid, remaining)?;
        let (by, who_from) = match &reply {
            Some(r) => {
                let (b, w) = who_of(r.value("by"), r.value("whoFrom"));
                (Some(b), Some(w))
            }
            None => (None, None),
        };
        let mut d = d;
        if let Some(r) = &reply {
            if r.value("decision") == &Value::Str("allow".into()) {
                let why = if r.value("reason").truthy() { py_str(r.value("reason"))? } else { "no reason given".into() };
                if r.value("scope") == &Value::Str("task".into()) && d.tier == UNDOABLE {
                    let _ = super::best_effort(self.propose_for_task(p, &d, &tid));
                }
                d.allow = true;
                d.ask = true;
                d.reason = format!("allowed by operator: {why}");
                d.updated_input = if r.value("updatedInput").truthy() { Some(r.value("updatedInput").clone()) } else { None };
                d.answered_by = by;
                d.who_from = who_from;
                return Ok(d);
            }
        }
        let why = if reply.is_some() {
            "operator denied".to_string()
        } else {
            format!("operator did not answer within {} s", self.ask_timeout_s.trunc() as i128)
        };
        d.ask = reply.is_some();
        d.reason = format!("{} ({why})", d.reason);
        d.answered_by = by;
        d.who_from = who_from;
        Ok(d)
    }

    /// `gate._propose_for_task`.
    fn propose_for_task(&self, p: &Object, d: &Decision, tid: &str) -> R<()> {
        let pid = random_hex(4)?;
        let body = Object::new()
            .with("id", pid.as_str())
            .with("kind", "allow-pattern")
            .with("scope", "task")
            .with("expiresAt", now() + 30.0 * 86400.0)
            .with("createdAt", now())
            .with("sessionId", p.value("session_id").clone())
            .with("toolUseId", tid)
            .with("toolInput", p.value("tool_input").clone())
            .with("clause", d.clause())
            .with("tier", d.tier.as_str());
        self.ask_write(PROPOSALS, &format!("{pid}.json"), &body).map(|_| ())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn who_of_checks_the_name() {
        let s = |x: &str| Value::Str(x.into());
        assert_eq!(who_of(&s("ann"), &s("token")), ("ann".into(), "token".into()));
        assert_eq!(who_of(&s("pane:w1:p2"), &s("pane")), ("pane:w1:p2".into(), "pane".into()));
        assert_eq!(who_of(&s("pane"), &s("pane")), ("pane".into(), "pane".into()));
        assert_eq!(who_of(&s("a b"), &s("token")), ("local".into(), "none".into()));
        assert_eq!(who_of(&s("x"), &s("nobody")), ("local".into(), "none".into()));
        assert_eq!(who_of(&Value::Null, &s("token")), ("local".into(), "none".into()));
    }

    #[test]
    fn safe_names() {
        assert_eq!(safe("toolu_01.x"), "toolu_01.x");
        assert_eq!(safe("..a/b.."), "a_b");
        assert_eq!(safe(""), "none");
        assert_eq!(safe("é"), "_");
    }

    // GR-R-10: the ask channel is read and written through no link. A
    // planted link at operator.json, asks/ or answers/ used to let a
    // concurrent workspace write supply an allow. Each test plants one and
    // needs the refusal. Python twin: tests/test_ask_channel_state.py.

    use crate::gate::statefile::tests::{dir, file, mode};
    use std::os::unix::fs::symlink;

    fn runner(root: &str) -> Runner {
        Runner {
            env: Default::default(),
            home: String::new(),
            wd: Default::default(),
            real_cache: Default::default(),
            root: root.to_string(),
            default_root: String::new(),
            mode: "enforce".into(),
            fmt: "claude".into(),
            session: None,
            captures: None,
            verify_timeout_s: 1.0,
            ask: true,
            ask_timeout_s: 1.0,
            checkpoints: false,
            t0: Instant::now(),
            resident: false,
            caller: Default::default(),
        }
    }

    fn gate_root() -> (PathBuf, String) {
        let r = dir().join("gate");
        std::fs::create_dir_all(&r).unwrap();
        mode(&r, 0o700);
        let s = r.to_str().unwrap().to_string();
        (r, s)
    }

    fn presence(p: &Path, m: u32) {
        std::fs::write(p, format!("{{\"pid\": {}, \"at\": 0}}", std::process::id())).unwrap();
        mode(p, m);
    }

    fn post_one(r: &Runner, root: &Path) -> String {
        let body = Object::new().with("tier", "permanent");
        assert!(r.ask_write(ASKS, "t1.json", &body.with("nonce", "n1")).unwrap());
        let raw = std::fs::read_to_string(root.join("asks/t1.json")).unwrap();
        assert!(raw.contains("n1"));
        "n1".into()
    }

    fn forged(nonce: &str) -> String {
        format!("{{\"toolUseId\": \"t1\", \"decision\": \"allow\", \"nonce\": \"{nonce}\"}}")
    }

    #[test]
    fn a_plain_presence_file_counts() {
        let (root, s) = gate_root();
        presence(&root.join("operator.json"), 0o600);
        assert!(runner(&s).operator_present().unwrap());
    }

    #[test]
    fn an_operator_file_that_is_linked_or_shared_is_no_operator() {
        let (root, s) = gate_root();
        let r = runner(&s);
        let op = root.join("operator.json");
        let real = dir().join("elsewhere.json");
        presence(&real, 0o600);
        symlink(&real, &op).unwrap();
        assert!(!r.operator_present().unwrap(), "symlink");
        std::fs::remove_file(&op).unwrap();
        presence(&op, 0o666);
        assert!(!r.operator_present().unwrap(), "world writable");
        mode(&op, 0o600);
        std::fs::hard_link(&op, dir().join("second")).unwrap();
        assert!(!r.operator_present().unwrap(), "hard link");
    }

    #[test]
    fn a_symlinked_asks_dir_takes_no_ask() {
        let (root, s) = gate_root();
        let r = runner(&s);
        let real = dir();
        symlink(&real, root.join("asks")).unwrap();
        assert!(!r.channel_ok());
        assert!(!r.ask_write(ASKS, "t1.json", &Object::new()).unwrap());
        assert_eq!(std::fs::read_dir(&real).unwrap().count(), 0);
    }

    #[test]
    fn a_symlinked_answers_dir_is_not_honored_and_the_wait_ends_at_once() {
        let (root, s) = gate_root();
        let r = runner(&s);
        let nonce = post_one(&r, &root);
        let real = dir();
        std::fs::write(real.join("t1.json"), forged(&nonce)).unwrap();
        mode(&real.join("t1.json"), 0o600);
        symlink(&real, root.join("answers")).unwrap();
        assert!(!r.channel_ok());
        let t0 = Instant::now();
        assert!(r.wait_answer("t1", 30.0).unwrap().is_none());
        assert!(t0.elapsed() < Duration::from_secs(5));
        assert!(real.join("t1.json").exists(), "removed through the link");
    }

    #[test]
    fn an_answer_that_is_linked_shared_or_hard_linked_is_not_honored() {
        let (root, s) = gate_root();
        let r = runner(&s);
        for name in ["symlink", "world writable", "hard link"] {
            let _ = std::fs::remove_dir_all(root.join("asks"));
            let _ = std::fs::remove_dir_all(root.join("answers"));
            let nonce = post_one(&r, &root);
            std::fs::create_dir(root.join("answers")).unwrap();
            mode(&root.join("answers"), 0o700);
            let p = root.join("answers/t1.json");
            match name {
                "symlink" => {
                    let real = dir().join("real.json");
                    file(&real, 0o600);
                    std::fs::write(&real, forged(&nonce)).unwrap();
                    symlink(&real, &p).unwrap();
                }
                "world writable" => {
                    std::fs::write(&p, forged(&nonce)).unwrap();
                    mode(&p, 0o666);
                }
                _ => {
                    std::fs::write(&p, forged(&nonce)).unwrap();
                    mode(&p, 0o600);
                    std::fs::hard_link(&p, dir().join("second")).unwrap();
                }
            }
            assert!(r.wait_answer("t1", 2.0).unwrap().is_none(), "{name}: a forged answer was honored");
        }
    }

    #[test]
    fn a_real_answer_is_still_honored() {
        let (root, s) = gate_root();
        let r = runner(&s);
        let nonce = post_one(&r, &root);
        let body = Object::new().with("toolUseId", "t1").with("decision", "allow").with("nonce", nonce.as_str());
        assert!(r.ask_write(ANSWERS, "t1.json", &body).unwrap());
        let got = r.wait_answer("t1", 2.0).unwrap().unwrap();
        assert_eq!(got.value("decision"), &Value::Str("allow".into()));
        assert!(!root.join("asks/t1.json").exists());
    }

    #[test]
    fn the_sweep_removes_a_planted_ask_link_and_reads_no_target() {
        let (root, s) = gate_root();
        let r = runner(&s);
        let real = dir().join("real-ask.json");
        std::fs::write(&real, format!("{{\"deadline\": {}}}", now() + 600.0)).unwrap();
        mode(&real, 0o600);
        std::fs::create_dir(root.join("asks")).unwrap();
        mode(&root.join("asks"), 0o700);
        symlink(&real, root.join("asks/x.json")).unwrap();
        r.sweep_expired(now()).unwrap();
        assert!(std::fs::symlink_metadata(root.join("asks/x.json")).is_err());
        assert!(real.exists());
    }
}
