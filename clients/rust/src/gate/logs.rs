//! Every line one call writes, in the Python gate's order, each step
//! best-effort as in Python: the shadow log (`gate._log_shadow`), the
//! session tree (`gate._log_tree`, `session_tree.SessionTree`), the
//! captures mirror (`hook.record_call`) and the coppice state report
//! (`gate._maybe_report_state`). A write that Python would fail on (a lone
//! surrogate in a UTF-8 file, a directory it cannot make) fails here at the
//! same step, with the same partial effect.

use super::decide::Decision;
use super::paths::{exists, path_join, path_parent};
use super::py::text::has_surrogate;
use super::py::PyErr;
use super::pyjson::{dumps, loads, round, LoadError, Object, Value};
use super::pystr::{py_head, py_strip, safe_session_id, splitlines};
use super::record::{safe_session_value, Record};
use super::state_report::{report_state_env, transcript_path_for_report};
use super::{catch, now_float, random_hex, Runner, R};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};

/// `gate._HARNESS_BY_FMT.get(fmt, fmt)`.
pub fn harness_of(f: &str) -> String {
    match f {
        "claude" => "claude-code",
        other => other,
    }
    .to_string()
}

/// `session_tree.ENTRY_TYPES` and `_NO_MOVE`.
const ENTRY_TYPES: &[&str] = &[
    "session", "prompt", "assistant", "tool_call", "verdict", "tool_result", "checkpoint", "compaction",
    "branch_summary", "label", "note", "head", "state",
];
const NO_MOVE: &[&str] = &["label", "head", "session", "state"];
const META_KEYS: &[&str] = &["type", "id", "parentId", "ts"];

fn opt(s: &Option<String>) -> Value {
    match s {
        Some(x) => Value::Str(x.clone()),
        None => Value::Null,
    }
}

/// An OSError from a file step, as Python raises it.
fn io(e: std::io::Error, path: &str) -> PyErr {
    super::paths::os_error(&e, path)
}

/// `Path.mkdir(parents=True, exist_ok=True, mode=0o700)`, then `os.chmod`
/// (`gate._mkdir_private`, and `SessionTree.create`'s own mkdir).
pub(crate) fn mkdir_private(d: &str) -> Result<(), PyErr> {
    let p = super::paths::os_path("mkdir", d)?;
    let parent = path_parent(d);
    if parent != d {
        if let Ok(pp) = super::paths::os_path("mkdir", &parent) {
            let _ = fs::create_dir_all(pp);
        }
    }
    match fs::DirBuilder::new().mode(0o700).create(&p) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists && p.is_dir() => {}
        Err(e) => return Err(io(e, d)),
    }
    let _ = fs::set_permissions(&p, fs::Permissions::from_mode(0o700));
    Ok(())
}

/// `open(path, "a", encoding="utf-8").write(text)`: the file is made
/// first, then the text is encoded, and a lone surrogate fails the write
/// with nothing written.
fn append_text(path: &str, text: &str) -> Result<(), PyErr> {
    let p = super::paths::os_path("open", path)?;
    let mut f = OpenOptions::new().append(true).create(true).mode(0o666).open(&p).map_err(|e| io(e, path))?;
    if has_surrogate(text) {
        let bytes = super::py::text::encode_utf8_strict(text);
        return Err(bytes.err().unwrap_or_else(|| PyErr::new("UnicodeEncodeError", "surrogates not allowed")));
    }
    f.write_all(text.as_bytes()).map_err(|e| io(e, path))
}

pub(crate) fn chmod_600(path: &str) {
    if let Ok(p) = super::paths::os_path("chmod", path) {
        let _ = fs::set_permissions(p, fs::Permissions::from_mode(0o600));
    }
}

/// One session tree file (`session_tree.SessionTree`).
pub struct Tree {
    path: String,
    /// The head cache: None until computed.
    head: Option<Value>,
}

impl Tree {
    /// `SessionTree.open`: `None` when the file does not exist.
    pub(crate) fn open(dir: &str, sid: &str) -> Result<Option<Tree>, PyErr> {
        let path = path_join(dir, &format!("{}.jsonl", safe_session_id(sid)));
        if !exists(&path).map_err(fault)? {
            return Ok(None);
        }
        Ok(Some(Tree { path, head: None }))
    }

    /// `SessionTree.head()`, cached as the oracle caches it.
    pub(crate) fn head(&mut self) -> Result<Value, PyErr> {
        if self.head.is_none() {
            self.head = Some(self.compute_head()?);
        }
        Ok(self.head.clone().unwrap_or(Value::Null))
    }

    /// `SessionTree.open_or_create`.
    fn open_or_create(dir: &str, sid: &str, header: Object) -> Result<Tree, PyErr> {
        let path = path_join(dir, &format!("{}.jsonl", safe_session_id(sid)));
        if exists(&path).map_err(fault)? {
            return Ok(Tree { path, head: None });
        }
        // create
        mkdir_private(dir)?;
        if exists(&path).map_err(fault)? {
            return Ok(Tree { path, head: None });
        }
        let mut row = Object::new().with("type", "session").with("id", safe_session_id(sid)).with("ts", now_float());
        for k in header.keys() {
            if !META_KEYS.contains(&k.as_str()) {
                row.set(k, header.value(k).clone());
            }
        }
        append_text(&path, &format!("{}\n", dumps(&Value::Obj(row), false)))?;
        chmod_600(&path);
        Ok(Tree { path, head: Some(Value::Null) })
    }

    /// `SessionTree._compute_head`.
    fn compute_head(&self) -> Result<Value, PyErr> {
        let p = super::paths::os_path("open", &self.path)?;
        let raw = match fs::read(&p) {
            Ok(r) => r,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Value::Null),
            Err(e) => return Err(io(e, &self.path)),
        };
        let text = match String::from_utf8(raw) {
            Ok(t) => t,
            Err(e) => {
                let pos = e.utf8_error().valid_up_to();
                let byte = e.as_bytes()[pos];
                return Err(PyErr::new(
                    "UnicodeDecodeError",
                    format!("'utf-8' codec can't decode byte 0x{byte:02x} in position {pos}: invalid start byte"),
                ));
            }
        };
        for line in splitlines(&text).into_iter().rev() {
            if py_strip(line).is_empty() {
                continue;
            }
            let row = match loads(line) {
                Ok(v) => v,
                Err(LoadError::Syntax(_)) => continue,
                Err(LoadError::IntDigits) => {
                    return Err(PyErr::value("Exceeds the limit (4300 digits) for integer string conversion"))
                }
                Err(_) => return Err(PyErr::recursion()),
            };
            let row = match row {
                Value::Obj(o) => o,
                _ => continue,
            };
            let t = match row.value("type") {
                Value::Str(s) => s.clone(),
                v @ (Value::List(_) | Value::Obj(_)) => {
                    return Err(PyErr::type_err(format!(
                        "unhashable type: '{}'",
                        super::pyjson::py_type_name(v)
                    )))
                }
                _ => continue,
            };
            if !ENTRY_TYPES.contains(&t.as_str()) {
                continue;
            }
            if t == "head" {
                return Ok(row.value("leafId").clone());
            }
            if !NO_MOVE.contains(&t.as_str()) && row.value("id").truthy() {
                return Ok(row.value("id").clone());
            }
        }
        Ok(Value::Null)
    }

    /// `SessionTree.append(type, data)`, parented on the head or on
    /// `parent`. Returns the new entry's id.
    pub(crate) fn append(&mut self, typ: &str, data: &Object, parent: Option<Value>) -> Result<String, PyErr> {
        let parent = match parent {
            Some(p) => p,
            None => {
                if self.head.is_none() {
                    self.head = Some(self.compute_head()?);
                }
                self.head.clone().unwrap_or(Value::Null)
            }
        };
        let id = random_hex(4).map_err(|_| PyErr::os("no randomness"))?;
        let mut row = Object::new()
            .with("type", typ)
            .with("id", id.as_str())
            .with("parentId", parent)
            .with("ts", now_float());
        for k in data.keys() {
            if !META_KEYS.contains(&k.as_str()) {
                row.set(k, data.value(k).clone());
            }
        }
        append_text(&self.path, &format!("{}\n", dumps(&Value::Obj(row), false)))?;
        if !NO_MOVE.contains(&typ) {
            self.head = Some(Value::Str(id.clone()));
        }
        Ok(id)
    }
}

fn fault(f: super::Fault) -> PyErr {
    match f {
        super::Fault::Raised(e) => e,
        super::Fault::Undecided(w) => PyErr::undecided(w),
    }
}

/// `gate._string_harness_session_id`.
fn string_session(p: &Object) -> Value {
    match p.value("session_id") {
        Value::Str(s) => Value::Str(s.clone()),
        _ => Value::Null,
    }
}

impl Runner {
    /// `gate._log_shadow`: best-effort.
    pub fn log_shadow(&self, session_id: &Value, d: &Decision, payload_session: &Value, join: Option<&Object>) -> R<()> {
        let sid = safe_session_value(session_id)?;
        let _ = catch(self.log_shadow_body(&sid, d, payload_session, join))?;
        Ok(())
    }

    fn log_shadow_body(&self, sid: &str, d: &Decision, payload_session: &Value, join: Option<&Object>) -> R<()> {
        let dir = path_join(&self.root, "shadow");
        mkdir_private(&dir)?;
        let path = path_join(&dir, &format!("{sid}.jsonl"));
        let fresh = !exists(&path)?;
        let vs: Vec<Value> = d.violations.iter().map(|v| Value::Obj(v.obj())).collect();
        let mut rec = Object::new()
            .with("at", now_float())
            .with("session_id", sid)
            .with("payload_session_id", payload_session.clone())
            .with("tool_name", opt(&d.tool_name))
            .with("step_type", opt(&d.step_type))
            .with("detail", d.detail.as_str())
            .with("mode", self.mode.as_str())
            .with("allow", d.allow)
            .with("would_deny", d.would_deny)
            .with("reason", d.reason.as_str())
            .with("elapsed_ms", round(d.elapsed_ms, 3))
            .with("clause", d.clause())
            .with("violations", vs)
            .with("envelope_id", opt(&d.envelope_id))
            .with("plan_id", opt(&d.plan_id))
            .with("ask", d.ask)
            .with("tier", d.tier.as_str());
        if d.ask {
            let key = if d.allow { "allowed_by" } else { "denied_by" };
            rec.set(key, d.answered_by.clone().unwrap_or_else(|| "local".into()));
            rec.set("who_from", d.who_from.clone().unwrap_or_else(|| "none".into()));
        }
        if let Some(j) = join {
            for k in j.keys() {
                rec.set(k, j.value(k).clone());
            }
        }
        append_text(&path, &format!("{}\n", dumps(&Value::Obj(rec), true)))?;
        if fresh {
            chmod_600(&path);
        }
        Ok(())
    }

    /// `gate._log_tree`: the tree it wrote to, or None on any failure.
    pub fn log_tree(&self, p: &Object, d: &Decision, session_id: &Value) -> R<Option<Tree>> {
        Ok(super::best_effort(self.log_tree_body(p, d, session_id)))
    }

    fn sessions_dir(&self) -> String {
        path_join(&path_parent(&self.root), "sessions")
    }

    fn tree_header(&self, p: &Object) -> R<Object> {
        Ok(Object::new()
            .with("v", 1i64)
            .with("harness", harness_of(&self.fmt))
            .with("cwd", super::pyjson::py_str_or_empty(p.value("cwd"))?)
            .with("harnessSessionId", string_session(p))
            .with("transcriptPath", p.value("transcript_path").clone())
            .with("parentSession", Value::Null)
            .with("parentEntry", Value::Null)
            .with("cacheKey", Value::Null))
    }

    fn log_tree_body(&self, p: &Object, d: &Decision, session_id: &Value) -> R<Tree> {
        let key = if session_id.truthy() { session_id } else { p.value("session_id") };
        let sid = safe_session_value(key)?;
        let mut tree = Tree::open_or_create(&self.sessions_dir(), &sid, self.tree_header(p)?)?;
        let name = match &d.tool_name {
            Some(n) if !n.is_empty() => Value::Str(n.clone()),
            _ => p.value("tool_name").clone(),
        };
        let call = Object::new()
            .with("toolUseId", p.value("tool_use_id").clone())
            .with("name", name)
            .with("stepType", opt(&d.step_type))
            .with("detail", d.detail.as_str())
            .with("agentId", p.value("agent_id").clone())
            .with("agentType", p.value("agent_type").clone());
        let call_id = tree.append("tool_call", &call, None)?;
        let verdict = Object::new()
            .with("toolUseId", p.value("tool_use_id").clone())
            .with("decision", if d.allow { "allow" } else { "deny" })
            .with("wouldDeny", d.would_deny)
            .with("mode", self.mode.as_str())
            .with("reason", d.reason.as_str())
            .with("clause", d.clause())
            .with("counterexample", d.counterexample())
            .with("envelopeId", opt(&d.envelope_id))
            .with("planId", opt(&d.plan_id))
            .with("latencyMs", round(d.elapsed_ms, 3))
            .with("answeredBy", if d.ask { Value::Str("operator".into()) } else { Value::Null })
            .with("tier", d.tier.as_str());
        tree.append("verdict", &verdict, Some(Value::Str(call_id)))?;
        Ok(tree)
    }

    /// `hook.record_call(payload, root=captures)`: best-effort.
    pub fn capture(&self, captures: &str, p: &Object) -> R<()> {
        let _ = super::best_effort(self.capture_body(captures, p));
        Ok(())
    }

    fn capture_body(&self, captures: &str, p: &Object) -> R<()> {
        let rec: Record = match self.payload_to_record(p, "claude")? {
            None => return Ok(()),
            Some(r) => r,
        };
        let mut obj = rec.obj.clone();
        obj.set("captured_at", now_float());
        mkdir_private(captures)?;
        let sid = obj.value("session_id").as_str().unwrap_or("no-session").to_string();
        let path = path_join(captures, &format!("{sid}.jsonl"));
        let fresh = !exists(&path)?;
        append_text(&path, &format!("{}\n", dumps(&Value::Obj(obj), true)))?;
        if fresh {
            chmod_600(&path);
        }
        Ok(())
    }

    /// `gate._maybe_report_state`: the `working` event for the floor and
    /// its copy in the session tree (the tree `_log_tree` opened, or a
    /// fresh open when that failed). Best-effort.
    pub fn report_state(&self, p: &Object, d: &Decision, session_id: &Value, tree: Option<Tree>) -> R<()> {
        let key = if session_id.truthy() { session_id } else { p.value("session_id") };
        let sid = match super::best_effort(safe_session_value(key)) {
            Some(s) => s,
            None => return Ok(()),
        };
        let detail = if d.allow { "verdict=allow".to_string() } else { format!("verdict=deny clause={}", d.clause()) };
        let clause = if d.would_deny { d.clause() } else { String::new() };
        let verdict = (if d.allow { "allow" } else { "deny" }, clause);
        self.report_and_append_state(p, &sid, "working", py_head(&detail, 200), None, d, verdict, tree)
    }

    /// `gate._report_blocked`: the `blocked` event for a posted ask, before
    /// the wait. Best-effort.
    pub fn report_blocked(&self, p: &Object, d: &Decision, session_id: &Value, tid: &str, deadline: f64) -> R<()> {
        let key = if session_id.truthy() { session_id } else { p.value("session_id") };
        let sid = match super::best_effort(safe_session_value(key)) {
            Some(s) => s,
            None => return Ok(()),
        };
        let tool = match &d.tool_name {
            Some(t) if !t.is_empty() => t.clone(),
            _ => "unknown".into(),
        };
        let ask = Object::new()
            .with("id", tid)
            .with("tool", tool.as_str())
            .with("summary", d.detail.as_str())
            .with("deadline", deadline)
            .with("tier", d.tier.as_str());
        let detail = format!("awaiting operator: {}", d.reason);
        self.report_and_append_state(p, &sid, "blocked", py_head(&detail, 200), Some(ask), d, ("ask", d.clause()), None)
    }

    /// `gate._report_and_append_state`: build the event, deliver it, and
    /// append it to the session tree, each step best-effort.
    #[allow(clippy::too_many_arguments)]
    fn report_and_append_state(
        &self,
        p: &Object,
        sid: &str,
        state: &str,
        detail: &str,
        ask: Option<Object>,
        d: &Decision,
        verdict: (&str, String),
        tree: Option<Tree>,
    ) -> R<()> {
        let tool = match &d.tool_name {
            Some(t) if !t.is_empty() => t.clone(),
            _ => "unknown".into(),
        };
        let mut ev = Object::new()
            .with("v", 1i64)
            .with("ts", now_float())
            .with("session_id", sid)
            .with("harness_session_id", string_session(p))
            .with("harness", harness_of(&self.fmt))
            .with("pane", self.pane_from_env())
            .with("state", state)
            .with("source", "gate")
            .with("detail", py_head(detail, 200));
        if let Some(a) = ask {
            ev.set("ask", a);
        }
        if let Some(tp) = transcript_path_for_report(p.value("transcript_path")) {
            ev.set("transcript_path", tp);
        }
        ev.set(
            "verdict",
            Object::new()
                .with("decision", verdict.0)
                .with("tool", py_head(&tool, 200))
                .with("clause", py_head(&verdict.1, 200)),
        );
        ev.set("mode", if self.mode == "enforce" { "enforcing" } else { "watching" });
        report_state_env(&self.env, &ev);
        let _ = super::best_effort((|| -> R<()> {
            let mut t = match tree {
                Some(t) => t,
                None => Tree::open_or_create(&self.sessions_dir(), sid, self.tree_header(p)?)?,
            };
            // json.loads(ev_json): the event as the wire carries it.
            let ev2 = match loads(&dumps(&Value::Obj(ev.clone()), true)) {
                Ok(Value::Obj(o)) => o,
                _ => ev.clone(),
            };
            t.append("state", &ev2, None)?;
            Ok(())
        })());
        Ok(())
    }

    /// `gate._pane_from_env`.
    pub fn pane_from_env(&self) -> Value {
        for k in ["COPPICE_PANE", "HERDR_PANE_ID", "HERDR_PANE", "TMUX_PANE"] {
            if let Some(v) = self.env.get(k) {
                if !v.is_empty() {
                    return Value::Str(v.clone());
                }
            }
        }
        Value::Null
    }
}
