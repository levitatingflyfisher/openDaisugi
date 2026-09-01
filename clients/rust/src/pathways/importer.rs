//! `portability.import_pathway`: the bundle read (JSON or skill markdown),
//! its pathway validated as `CompiledPathway.model_validate` does,
//! re-verified, checked storable, and written.

use super::dumped::load_dumped;
use super::export::BUNDLE_SCHEMA_VERSION;
use super::pathway::{dump_json, py_dumps, Pathway};
use super::pmodel::{self, Id, Mode};
use super::store::{PutRow, Store};
use super::verify;
use super::PwErr;
use crate::gate::py::text::{has_surrogate, is_space, repr};
use crate::gate::pyjson::{float_repr, loads, Value};

/// `str.lstrip()`: Python's white space, Unicode included.
pub fn lstrip(s: &str) -> &str {
    s.trim_start_matches(is_space)
}

/// What `parse_bundle` checks first: text that, with leading white space
/// dropped, starts with "---" is skill markdown.
pub fn is_skill(text: &str) -> bool {
    lstrip(text).starts_with("---")
}

/// `parse_bundle` for the JSON form.
pub fn parse_bundle(text: &str, source: &str) -> Result<Pathway, PwErr> {
    let text = lstrip(text);
    if !text.starts_with('{') {
        return Err(PwErr::import("SCHEMA_INCOMPATIBLE", "input is neither JSON nor skill markdown with YAML frontmatter"));
    }
    let bundle = loads(text).map_err(|_| PwErr::Invalid("json.decoder.JSONDecodeError: the bundle is not JSON".into()))?;
    from_bundle(&bundle, source)
}

fn from_bundle(bundle: &Value, source: &str) -> Result<Pathway, PwErr> {
    let o = match bundle {
        Value::Obj(o) => o,
        _ => return Err(PwErr::Invalid("AttributeError: the bundle has no .get".into())),
    };
    if let Some(sv) = o.get("schema_version") {
        let newer = match sv {
            Value::Bool(_) => false,
            Value::Int(t) => {
                let t = t.trim_start_matches('-');
                !sv_negative(sv) && (t.len() > 1 || t.parse::<i64>().unwrap_or(0) > BUNDLE_SCHEMA_VERSION)
            }
            Value::Float(f) => *f > BUNDLE_SCHEMA_VERSION as f64,
            _ => return Err(PwErr::Invalid("TypeError: '>' not supported for schema_version".into())),
        };
        if newer {
            let shown = match sv {
                Value::Int(t) => t.clone(),
                Value::Float(f) => crate::gate::pyjson::py_float_repr(*f),
                _ => String::new(),
            };
            return Err(PwErr::import(
                "SCHEMA_INCOMPATIBLE",
                format!("bundle schema_version={shown} is newer than this library ({BUNDLE_SCHEMA_VERSION})"),
            ));
        }
    }
    let raw = o.value("pathway");
    if raw.is_null() {
        return Err(PwErr::import("SCHEMA_INCOMPATIBLE", format!("bundle is missing 'pathway' key (source={source})")));
    }
    match pmodel::validate_model(Id::CompiledPathway, raw, Mode::Python) {
        Ok(Value::Obj(obj)) => Ok(Pathway::new(obj)),
        Ok(_) => Err(PwErr::Invalid("TypeError: the pathway is not a model".into())),
        Err(e) if e.unreadable_step() => Err(PwErr::Unreadable("a plan step is a string".into())),
        // parse_bundle's refusal: a NaN or an infinity in the envelope or
        // plan (models.non_finite_error) is one of these.
        Err(e) => Err(PwErr::import("SCHEMA_INCOMPATIBLE", format!("the pathway is not valid: {}", e.text()))),
    }
}

fn sv_negative(v: &Value) -> bool {
    matches!(v, Value::Int(t) if t.starts_with('-'))
}

/// `parse_bundle` for skill markdown: the frontmatter between the first
/// "---\n" and the next "\n---\n", read as yaml.safe_load reads it, and
/// its daisugi key taken as the bundle. A frontmatter not in the form
/// yaml.safe_dump writes is not read by this binary.
pub fn parse_skill(text: &str, source: &str) -> Result<Pathway, PwErr> {
    let text = lstrip(text);
    let rest = match text.split_once("---\n") {
        Some((_, r)) => r,
        None => return Err(PwErr::import("SCHEMA_INCOMPATIBLE", "unterminated YAML frontmatter")),
    };
    let front = match rest.split_once("\n---\n") {
        Some((f, _)) => f,
        None => return Err(PwErr::import("SCHEMA_INCOMPATIBLE", "unterminated YAML frontmatter")),
    };
    let data = load_dumped(front).map_err(|u| PwErr::NotYet(u.0))?;
    let o = match &data {
        Value::Obj(o) => o,
        _ => return Err(PwErr::NotYet("the frontmatter is not a mapping".into())),
    };
    match o.get("daisugi") {
        None => Err(PwErr::import("SCHEMA_INCOMPATIBLE", "skill frontmatter is missing the 'daisugi' key")),
        Some(b) => from_bundle(b, source),
    }
}

/// The re-verification import runs: `None` when the plan verifies, else
/// the refusal. A Z3 check that answered unknown refuses first
/// (VERIFICATION_TIMEOUT).
pub fn verify_pathway(p: &Pathway, timeout_ms: u32) -> Result<Option<PwErr>, PwErr> {
    let out = verify::verify(p.obj.value("plan_template"), p.obj.value("envelope"), None, timeout_ms)?;
    if !out.timeouts.is_empty() {
        return Ok(Some(PwErr::import(
            "VERIFICATION_TIMEOUT",
            format!("verifier timed out ({}); raise --z3-timeout-ms", out.timeouts.join("; ")),
        )));
    }
    if out.violations.is_empty() {
        return Ok(None);
    }
    let parts: Vec<String> = out.violations.iter().map(|v| format!("[{}] {}", v.stage, v.message)).collect();
    Ok(Some(PwErr::import(
        "VERIFICATION_FAILED",
        format!("plan template does not verify against declared envelope: {}", parts.join("; ")),
    )))
}

/// The deepest nesting of non-empty containers (models, dicts and lists,
/// the root model included) pydantic-core serializes; one more raises
/// "Circular reference detected (depth exceeded)".
const SERIALIZATION_DEPTH: usize = 257;

/// How deep pydantic-core's serializer recurses into `v`: one level for
/// each container it has to enter, so an empty list or dict counts as a
/// leaf.
fn depth(v: &Value) -> usize {
    match v {
        Value::List(l) if !l.is_empty() => 1 + l.iter().map(depth).max().unwrap_or(0),
        Value::Obj(o) if !o.is_empty() => 1 + o.keys().iter().map(|k| depth(o.value(k))).max().unwrap_or(0),
        _ => 0,
    }
}

/// A lone surrogate in a value. A key is not checked: pydantic writes a
/// surrogate in a key as its three bytes, each read back as U+FFFD.
fn value_surrogate(v: &Value) -> bool {
    match v {
        Value::Str(s) => has_surrogate(s),
        Value::List(l) => l.iter().any(value_surrogate),
        Value::Obj(o) => o.keys().iter().any(|k| value_surrogate(o.value(k))),
        _ => false,
    }
}

/// `portability._check_storable`: a pathway the store cannot write and
/// then read back is refused before anything is deleted or written.
pub fn storable(p: &Pathway) -> Result<(), PwErr> {
    let refuse = |why: &str| {
        PwErr::import("UNSTORABLE", format!("pathway {} cannot be stored and read back: {why}", repr(p.id())))
    };
    for k in ["id", "task_description", "embedding_model", "embedding_model_version", "structure_signature"] {
        if let Value::Str(s) = p.obj.value(k) {
            if has_surrogate(s) {
                return Err(refuse("a text holds a lone surrogate"));
            }
        }
    }
    for k in ["version", "hit_count", "failure_count"] {
        if let Value::Int(t) = p.obj.value(k) {
            if t.parse::<i64>().is_err() {
                return Err(refuse("an integer is past 64 bits"));
            }
        }
    }
    for k in ["distilled_at", "last_activation_at"] {
        if matches!(p.obj.value(k), Value::Float(f) if f.is_nan()) {
            return Err(refuse("a time is NaN"));
        }
    }
    for (k, id) in [("envelope", Id::Envelope), ("plan_template", Id::ActionPlan)] {
        let v = p.obj.value(k);
        if value_surrogate(v) {
            return Err(refuse("a text holds a lone surrogate"));
        }
        if depth(v) > SERIALIZATION_DEPTH {
            return Err(refuse("it nests deeper than it can be written"));
        }
        if pmodel::validate_json(id, &dump_json(v)).is_err() {
            return Err(refuse("it nests deeper than it can be read back"));
        }
    }
    Ok(())
}

fn int_of(v: &Value) -> i64 {
    match v {
        Value::Int(t) => t.parse().unwrap_or(0),
        _ => 0,
    }
}

fn float_of(v: &Value) -> f64 {
    match v {
        Value::Float(f) => *f,
        _ => 0.0,
    }
}

/// The row `put()` writes, each column as the oracle writes it.
pub fn put_row(p: &Pathway) -> Result<PutRow, PwErr> {
    storable(p)?;
    py_row(p)
}

/// The row `put()` writes, failing only where Python's sqlite3 fails to
/// bind a column. A caller that is not import (the gardener) meets the
/// oracle's own exception rather than import's refusal.
pub fn py_row(p: &Pathway) -> Result<PutRow, PwErr> {
    let o = &p.obj;
    // sqlite3 binds the columns in order and raises on the first it
    // cannot bind: a text with a lone surrogate, an int past 64 bits.
    for k in ["id", "task_description", "version", "hit_count", "embedding_model", "embedding_model_version", "failure_count", "structure_signature"] {
        match o.value(k) {
            Value::Str(t) if has_surrogate(t) => {
                return Err(PwErr::Invalid(format!("UnicodeEncodeError: 'utf-8' codec can't encode a surrogate in {k}")))
            }
            Value::Int(t) if t.parse::<i64>().is_err() => {
                return Err(PwErr::Invalid("OverflowError: Python int too large to convert to SQLite INTEGER".into()))
            }
            _ => {}
        }
    }
    for k in ["distilled_at", "last_activation_at"] {
        if matches!(o.value(k), Value::Float(f) if f.is_nan()) {
            // sqlite3 binds NaN as NULL, which the NOT NULL column refuses.
            return Err(PwErr::Invalid(format!("sqlite3.IntegrityError: NOT NULL constraint failed: pathways.{k}")));
        }
    }
    let s = |k: &str| o.value(k).as_str().unwrap_or("").to_string();
    let _ = float_repr;
    Ok(PutRow {
        id: p.id().to_string(),
        task: p.task().to_string(),
        embedding: py_dumps(o.value("task_embedding")),
        envelope: dump_json(o.value("envelope")),
        plan: dump_json(o.value("plan_template")),
        traces: py_dumps(o.value("source_trace_ids")),
        version: int_of(o.value("version")),
        hits: int_of(o.value("hit_count")),
        distilled_at: float_of(o.value("distilled_at")),
        model: s("embedding_model"),
        model_version: s("embedding_model_version"),
        last_activation: float_of(o.value("last_activation_at")),
        failures: int_of(o.value("failure_count")),
        signature: o.value("structure_signature").as_str().map(|x| x.to_string()),
        parameters: py_dumps(o.value("parameters")),
    })
}

impl Store {
    /// Import's write: `put()`, or with `overwrite` the old row deleted and
    /// the new one written in one transaction. It reports whether a row
    /// went.
    pub fn import_write(&mut self, p: &Pathway, overwrite: bool) -> Result<bool, PwErr> {
        let row = put_row(p)?;
        if overwrite {
            return self.replace(&row);
        }
        self.put(&row)?;
        Ok(false)
    }
}
