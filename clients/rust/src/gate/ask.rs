//! `gate._maybe_ask` and the file protocol of `opendaisugi.ask`: a
//! would-deny is handed to a present operator for at most --ask-timeout.
//! No operator, no tool_use_id, a deny, a late or a forged answer all
//! leave the deny standing; only an answer that echoes this ask's nonce
//! and is no older than it can allow.

use super::decide::Decision;
use super::effects::UNDOABLE;
use super::paths::{os_error, os_path};
use super::pymodel::decode_utf8_strict;
use super::pyjson::{dumps, loads, py_str, LoadError, Object, Value};
use super::{random_hex, undecided, Fault, PyErr, Runner, R};
use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

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

/// `ask._mkdir`.
fn mkdir(p: &Path) -> R<()> {
    std::fs::DirBuilder::new().recursive(true).mode(0o700).create(p).map_err(|e| io(e, p))?;
    let _ = std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o700));
    Ok(())
}

/// `ask._write_json(path, body)`: through a `.tmp` file and `os.replace`.
fn write_json(path: &Path, body: &Object) -> R<()> {
    if let Some(parent) = path.parent() {
        mkdir(parent)?;
    }
    let tmp = path.with_extension("tmp");
    std::fs::write(&tmp, dumps(&Value::Obj(body.clone()), true)).map_err(|e| io(e, &tmp))?;
    let _ = std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o600));
    std::fs::rename(&tmp, path).map_err(|e| io(e, path))
}

/// `ask._read_json(path)`: a dict, or None for any OSError or ValueError.
fn read_json(path: &Path) -> R<Option<Object>> {
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        Err(_) => return Ok(None),
    };
    let text = match decode_utf8_strict(&raw) {
        Ok(t) => t,
        Err(_) => return Ok(None),
    };
    match loads(&text) {
        Ok(Value::Obj(o)) => Ok(Some(o)),
        Ok(_) => Ok(None),
        // json.loads raises RecursionError, which is no ValueError.
        Err(LoadError::Recursion) => undecided("an ask file nested past Python's recursion limit"),
        Err(_) => Ok(None),
    }
}

fn mtime(p: &Path) -> Option<f64> {
    let m = std::fs::metadata(p).ok()?.modified().ok()?;
    Some(m.duration_since(UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0))
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

/// `path.unlink()` with FileNotFoundError ignored and any other OSError
/// raised.
fn unlink_missing_ok(p: &Path) -> R<()> {
    match std::fs::remove_file(p) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(io(e, p)),
    }
}

/// `Path.glob("*.json")` in one directory: dotfiles included, directories
/// too.
fn json_names(dir: &Path) -> Vec<PathBuf> {
    match std::fs::read_dir(dir) {
        Ok(rd) => rd
            .filter_map(|e| e.ok())
            .map(|e| e.path())
            .filter(|p| p.file_name().and_then(|n| n.to_str()).is_some_and(|n| n.ends_with(".json")))
            .collect(),
        Err(_) => vec![],
    }
}

impl Runner {
    fn gate_root(&self) -> PathBuf {
        match os_path("open", &self.root) {
            Ok(p) => p,
            Err(_) => PathBuf::from(&self.root),
        }
    }

    /// `ask.operator_present(root)`.
    fn operator_present(&self) -> R<bool> {
        let p = self.gate_root().join(PRESENCE);
        let body = match read_json(&p)? {
            Some(b) => b,
            None => return Ok(false),
        };
        let age = match mtime(&p) {
            Some(m) => now() - m,
            None => return Ok(false),
        };
        if age > 15.0 {
            return Ok(false);
        }
        match body.value("pid") {
            v @ (Value::Int(_) | Value::Bool(_)) => pid_alive(v),
            _ => Ok(false),
        }
    }

    /// `ask._sweep_expired(root, now=now)`.
    fn sweep_expired(&self, at: f64) -> R<()> {
        let root = self.gate_root();
        let asks = root.join(ASKS);
        if asks.exists() {
            for p in json_names(&asks) {
                let body = read_json(&p)?;
                let deadline = match &body {
                    Some(b) => match b.get("deadline") {
                        None => 0.0,
                        Some(v) => py_float(v)?.unwrap_or(0.0),
                    },
                    None => 0.0,
                };
                if body.is_none() || deadline <= at {
                    unlink_missing_ok(&p)?;
                    if let Some(name) = p.file_name() {
                        unlink_missing_ok(&root.join(ANSWERS).join(name))?;
                    }
                }
            }
        }
        let answers = root.join(ANSWERS);
        if answers.exists() {
            for p in json_names(&answers) {
                if let Some(name) = p.file_name() {
                    if !asks.join(name).exists() {
                        unlink_missing_ok(&p)?;
                    }
                }
            }
        }
        Ok(())
    }

    /// `ask._consume(root, tool_use_id)`.
    fn consume(&self, tid: &str) -> R<()> {
        let root = self.gate_root();
        for sub in [ASKS, ANSWERS] {
            unlink_missing_ok(&root.join(sub).join(format!("{}.json", safe(tid))))?;
        }
        Ok(())
    }

    /// `ask.wait_answer(root, tool_use_id=tid, timeout_s=...)`.
    fn wait_answer(&self, tid: &str, timeout_s: f64) -> R<Option<Object>> {
        let root = self.gate_root();
        let name = format!("{}.json", safe(tid));
        let ask_path = root.join(ASKS).join(&name);
        let answer_path = root.join(ANSWERS).join(&name);
        let expected = read_json(&ask_path)?.map(|b| b.value("nonce").clone()).unwrap_or(Value::Null);
        let ask_mtime = mtime(&ask_path);
        let t_end = Instant::now() + Duration::from_secs_f64(timeout_s.clamp(0.0, 1e9));
        loop {
            if answer_path.exists() {
                let body = read_json(&answer_path)?;
                let answer_mtime = mtime(&answer_path);
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

    /// `gate._maybe_ask`.
    pub fn maybe_ask(&self, p: &Object, d: Decision, session_id: &Value) -> R<Decision> {
        let tool_use_id = p.value("tool_use_id");
        if !tool_use_id.truthy() || !self.operator_present()? {
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
        write_json(&self.gate_root().join(ASKS).join(format!("{}.json", safe(&tid))), &body)?;
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
        write_json(&self.gate_root().join(PROPOSALS).join(format!("{pid}.json")), &body)
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
}
