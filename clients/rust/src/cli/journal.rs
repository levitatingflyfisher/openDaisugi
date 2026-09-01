//! The gate's verdict journal: the shadow log each gate call appends one
//! JSON line to, under `<root>/shadow/<session>.jsonl`. `build` is
//! `gate.shadow_report`; `Report::text` is what `daisugi gate report`
//! prints. The Go client's `journal` package is the reference.

use super::gateroot::{join, safe_session_id, shadow_dir};
use crate::gate::py::text::{repr, splitlines, strip};
use crate::gate::pyjson::{dumps_indent, loads, py_float_repr, LoadError, Object, Value};

/// Why a report was not made.
#[derive(Debug)]
pub enum JournalErr {
    /// A log line holds a value not printed the way Python prints it.
    Unsupported,
    /// A log Python's own report fails on: it raises instead of reporting.
    Crash(String),
}

impl std::fmt::Display for JournalErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            JournalErr::Unsupported => write!(f, "the shadow log holds a value this binary does not print yet"),
            JournalErr::Crash(w) => write!(f, "{w}"),
        }
    }
}

type R<T> = Result<T, JournalErr>;

/// The dict `shadow_report` returns. `fp` holds indexes into `denied`.
pub struct Report {
    pub records: Vec<Object>,
    pub denied: Vec<Object>,
    pub fp: Vec<usize>,
    pub reasons: Object,
}

/// The files `shadow_report` reads: one session's file, or every *.jsonl
/// in the shadow directory.
pub fn files(root: &str, session: Option<&str>) -> R<Vec<String>> {
    let d = shadow_dir(root);
    if let Some(s) = session {
        return Ok(vec![join(&d, &format!("{}.jsonl", safe_session_id(s)))]);
    }
    if std::fs::metadata(&d).is_err() {
        return Ok(vec![]);
    }
    let entries = std::fs::read_dir(&d).map_err(|e| JournalErr::Crash(e.to_string()))?;
    let mut out = vec![];
    for e in entries {
        let e = e.map_err(|e| JournalErr::Crash(e.to_string()))?;
        let name = e.file_name().into_string().map_err(|_| JournalErr::Unsupported)?;
        if name.ends_with(".jsonl") {
            out.push(join(&d, &name));
        }
    }
    out.sort();
    Ok(out)
}

/// `shadow_report` over the given files.
pub fn build(files: &[String]) -> R<Report> {
    let mut rep = Report { records: vec![], denied: vec![], fp: vec![], reasons: Object::new() };
    for f in files {
        if std::fs::metadata(f).is_err() {
            continue;
        }
        let raw = std::fs::read(f).map_err(|e| JournalErr::Crash(format!("cannot read {f}: {e}")))?;
        let text = String::from_utf8(raw).map_err(|_| JournalErr::Crash(format!("{f} is not valid UTF-8")))?;
        for line in splitlines(&text) {
            if strip(line).is_empty() {
                continue;
            }
            match loads(line) {
                Ok(Value::Obj(o)) => rep.records.push(o),
                // JSON but not a record: skipped like a line that is not JSON.
                Ok(_) => {}
                Err(LoadError::Unsupported | LoadError::Recursion) => return Err(JournalErr::Unsupported),
                Err(_) => {}
            }
        }
    }
    for r in &rep.records {
        if !r.value("would_deny").truthy() {
            continue;
        }
        let reason = r.value("reason");
        let text = if reason.truthy() {
            match reason {
                Value::Str(s) => s.clone(),
                _ => return Err(JournalErr::Crash("a denied record's reason is not a string".into())),
            }
        } else {
            String::new()
        };
        let key: String = text.chars().take(120).collect();
        let n = match rep.reasons.get(&key) {
            Some(Value::Int(t)) => t.parse::<i64>().unwrap_or(0),
            _ => 0,
        };
        rep.reasons.set(&key, Value::Int((n + 1).to_string()));
        rep.denied.push(r.clone());
        if text.contains("metacharacters") || text.starts_with("unrecognized tool") {
            rep.fp.push(rep.denied.len() - 1);
        }
    }
    Ok(rep)
}

impl Report {
    /// `json.dumps(rep, indent=2)`.
    pub fn json(&self) -> String {
        let objs = |l: Vec<Object>| Value::List(l.into_iter().map(Value::Obj).collect());
        let fp: Vec<Object> = self.fp.iter().map(|i| self.denied[*i].clone()).collect();
        let o = Object::new()
            .with("calls", self.records.len() as i64)
            .with("allowed", (self.records.len() - self.denied.len()) as i64)
            .with("would_deny", self.denied.len() as i64)
            .with("reasons", self.reasons.clone())
            .with("denied", objs(self.denied.clone()))
            .with("false_positive_candidates", objs(fp));
        dumps_indent(&Value::Obj(o), 2, true)
    }

    /// The plain report: a counts line, then one line per denied call.
    pub fn text(&self) -> R<String> {
        let mut b = format!(
            "calls={} allowed={} would_deny={} false_positive_candidates={}\n",
            self.records.len(),
            self.records.len() - self.denied.len(),
            self.denied.len(),
            self.fp.len()
        );
        for (i, r) in self.denied.iter().enumerate() {
            let fp = if self.fp.contains(&i) { " [FP-candidate]" } else { "" };
            let tool = py_str(r.value("tool_name"))?;
            let detail = r.get("detail").cloned().unwrap_or(Value::Str(String::new()));
            let dr = match &detail {
                Value::Str(s) => repr(s),
                other => py_str(other)?,
            };
            let reason = py_str(r.value("reason"))?;
            b.push_str(&format!("  DENY{fp} {tool} {dr}: {reason}\n"));
        }
        Ok(b)
    }
}

/// `str(v)` for the JSON values whose str is modelled.
fn py_str(v: &Value) -> R<String> {
    Ok(match v {
        Value::Null => "None".into(),
        Value::Bool(true) => "True".into(),
        Value::Bool(false) => "False".into(),
        Value::Int(t) => t.clone(),
        Value::Float(f) => py_float_repr(*f),
        Value::Str(s) => s.clone(),
        _ => return Err(JournalErr::Unsupported),
    })
}
