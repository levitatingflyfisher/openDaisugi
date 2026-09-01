//! A stored row read as `PathwayStore._row_to_pathway` reads it, and a
//! pathway written back as pydantic writes it.

use rusqlite::types::ValueRef;

use super::pmodel::{self, take, Id, Mode, Schema};
use super::scan::{parse_embedding, scan_embedding};
use super::store::{sql_err, text, Col, Row, Store};
use super::PwErr;
use crate::gate::py::text::{as_surrogate, has_surrogate};
use crate::gate::pyjson::{dumps, dumps_indent_with, float_repr, loads, Object, Value};

/// A validated CompiledPathway: its `model_dump()`, fields in order.
/// `task_embedding` is Null until a command needs its floats (`full`).
#[derive(Debug, Clone)]
pub struct Pathway {
    pub obj: Object,
    emb_text: Option<String>,
}

impl Pathway {
    pub fn new(obj: Object) -> Pathway {
        Pathway { obj, emb_text: None }
    }

    pub fn id(&self) -> &str {
        self.obj.value("id").as_str().unwrap_or("")
    }

    pub fn task(&self) -> &str {
        self.obj.value("task_description").as_str().unwrap_or("")
    }

    /// Sets task_embedding to its floats, for a command that prints it.
    pub fn full(mut self) -> Pathway {
        if !self.obj.value("task_embedding").is_null() {
            return self;
        }
        let vec = self.emb_text.as_deref().and_then(parse_embedding).unwrap_or_default();
        self.obj.set("task_embedding", Value::List(vec.into_iter().map(Value::Float).collect()));
        self
    }
}

fn invalid_validation(e: pmodel::ValidationError) -> PwErr {
    if e.unreadable_step() {
        return PwErr::Unreadable("a plan step is a string".into());
    }
    PwErr::Invalid(format!("pydantic_core._pydantic_core.ValidationError: {}", e.text()))
}

fn col_value(c: &Col) -> Result<Value, PwErr> {
    Ok(match c {
        Col::Null => Value::Null,
        Col::Int(i) => Value::Int(i.to_string()),
        Col::Real(f) => Value::Float(*f),
        Col::Text(s) => Value::Str(s.clone()),
        // A BLOB is bytes in Python; pydantic's str and float read bytes
        // by rules this port does not carry.
        Col::Blob => return Err(PwErr::Unreadable("a column holds a BLOB".into())),
    })
}

/// `json.loads(v)`: a str is parsed; any other type raises TypeError.
fn json_loads(v: &Value) -> Result<Value, PwErr> {
    match v {
        Value::Str(s) => loads(s).map_err(|_| PwErr::Invalid("json.decoder.JSONDecodeError: the text is not JSON".into())),
        _ => Err(PwErr::Invalid("TypeError: the JSON object must be str, bytes or bytearray".into())),
    }
}

/// The embedding column, checked on its borrowed bytes.
#[derive(Debug, Clone)]
pub enum Emb {
    /// A TEXT value: whether it is a list of numbers, and its text when a
    /// command needs it (or when it is not).
    Text { ok: bool, text: Option<String> },
    Other(Col),
}

impl Emb {
    pub fn read(v: ValueRef<'_>, keep: bool) -> Result<Emb, PwErr> {
        match v {
            ValueRef::Text(b) => {
                let ok = scan_embedding(b).is_some();
                let t = if keep || !ok { Some(text(b)?) } else { None };
                if !ok {
                    // Undecodable text is still an error, as Python raises.
                }
                Ok(Emb::Text { ok, text: t })
            }
            other => Ok(Emb::Other(Col::from_ref(other)?)),
        }
    }
}

/// `_row_to_pathway`, in Python's order: the embedding's json.loads, the
/// envelope and the plan read with model_validate_json, the other JSON
/// columns loaded, then CompiledPathway's own validation of each field.
pub fn from_row(r: &Row, emb: &Emb) -> Result<Pathway, PwErr> {
    let get = |name: &str| -> Result<Value, PwErr> {
        match r.get(name) {
            Some(c) => col_value(c),
            None => Err(PwErr::Unreadable(format!("the row has no {name} column"))),
        }
    };
    // json.loads(task_embedding_json).
    let (emb_ok, emb_text) = match emb {
        Emb::Text { ok, text } => {
            if !ok {
                json_loads(&Value::Str(text.clone().unwrap_or_default()))?;
            }
            (*ok, text.clone())
        }
        Emb::Other(c) => {
            json_loads(&col_value(c)?)?;
            (false, None)
        }
    };
    let mut models = vec![];
    for (col, id) in [("envelope_json", Id::Envelope), ("plan_template_json", Id::ActionPlan)] {
        let v = get(col)?;
        let s = match v {
            Value::Str(s) => s,
            _ => {
                return Err(PwErr::Invalid(
                    "pydantic_core._pydantic_core.ValidationError: JSON input should be string, bytes or bytearray".into(),
                ))
            }
        };
        models.push(pmodel::validate_json(id, &s).map_err(invalid_validation)?);
    }
    let traces = json_loads(&get("source_trace_ids_json")?)?;
    let params_raw = get("parameters_json")?;
    let params = if params_raw.truthy() { json_loads(&params_raw)? } else { Value::List(vec![]) };

    // CompiledPathway(...): each field in its order.
    let field = |s: &Schema, v: Value| -> Result<Value, PwErr> {
        take("CompiledPathway", s, v, Mode::Python).map_err(invalid_validation)
    };
    let mut out = Object::new();
    out.set("id", field(&Schema::Str, get("id")?)?);
    out.set("task_description", field(&Schema::Str, get("task_description")?)?);
    if !emb_ok {
        return Err(PwErr::Invalid(
            "pydantic_core._pydantic_core.ValidationError: task_embedding is not a list of numbers".into(),
        ));
    }
    out.set("task_embedding", Value::Null);
    out.set("embedding_model", field(&Schema::Str, get("embedding_model")?)?);
    out.set("embedding_model_version", field(&Schema::Str, get("embedding_model_version")?)?);
    let mut it = models.into_iter();
    out.set("envelope", it.next().unwrap_or(Value::Null));
    out.set("plan_template", it.next().unwrap_or(Value::Null));
    out.set("source_trace_ids", field(&Schema::List(Box::new(Schema::Str)), traces)?);
    out.set("version", field(&Schema::Int, get("version")?)?);
    out.set("hit_count", field(&Schema::Int, get("hit_count")?)?);
    out.set("distilled_at", field(&Schema::Float, get("distilled_at")?)?);
    out.set("last_activation_at", field(&Schema::Float, get("last_activation_at")?)?);
    out.set("failure_count", field(&Schema::Int, get("failure_count")?)?);
    out.set("activation_count", Value::Int("0".into()));
    out.set("structure_signature", field(&Schema::Nullable(Box::new(Schema::Str)), get("structure_signature")?)?);
    out.set("parameters", field(&Schema::List(Box::new(Schema::Model(Id::PathwayParameter))), params)?);
    Ok(Pathway { obj: out, emb_text })
}

impl Store {
    /// `list_all()`: every row read as a pathway, in table order. `keep`
    /// names the pathways whose embedding a command will print. The error
    /// is the first row's in table order, as Python's loop meets it.
    pub fn read_all(&self, keep: &(dyn Fn(&str) -> bool + Sync)) -> Result<Vec<Pathway>, PwErr> {
        let cols = self.all_columns();
        let parts = self.scan_parts(&cols, &|rows| {
            let mut out: Vec<Result<Pathway, PwErr>> = vec![];
            let names: Vec<String> = rows.as_ref().map(|s| s.column_names().iter().map(|n| n.to_string()).collect()).unwrap_or_default();
            let ei = names.iter().position(|n| n == "task_embedding_json");
            while let Some(r) = rows.next().map_err(sql_err)? {
                let res = (|| -> Result<Pathway, PwErr> {
                    let mut row = Row::default();
                    for (i, n) in names.iter().enumerate() {
                        if Some(i) == ei {
                            continue;
                        }
                        row.cols.push((n.clone(), Col::from_ref(r.get_ref(i).map_err(sql_err)?)?));
                    }
                    let id_keep = match row.get("id") {
                        Some(Col::Text(s)) => keep(s),
                        _ => false,
                    };
                    let emb = match ei {
                        Some(i) => Emb::read(r.get_ref(i).map_err(sql_err)?, id_keep)?,
                        None => return Err(PwErr::Unreadable("the row has no task_embedding_json column".into())),
                    };
                    from_row(&row, &emb)
                })();
                out.push(res);
            }
            Ok(out)
        })?;
        let mut all = vec![];
        for part in parts {
            for r in part {
                all.push(r?);
            }
        }
        Ok(all)
    }
}

// ---------------------------------------------------------------------------
// pydantic's JSON
// ---------------------------------------------------------------------------

/// pydantic's text for a float: its own exponent form, and null for NaN
/// and the infinities.
pub fn float_json(f: f64) -> String {
    if !f.is_finite() {
        return "null".into();
    }
    crate::cli::envelope::float_json(f)
}

/// A dict key as pydantic's JSON writes it: a lone surrogate becomes its
/// three UTF-8-style bytes, which read back as three U+FFFD.
fn key_text(k: &str) -> String {
    if !has_surrogate(k) {
        return k.to_string();
    }
    k.chars().map(|c| if as_surrogate(c).is_some() { "\u{FFFD}\u{FFFD}\u{FFFD}".to_string() } else { c.to_string() }).collect()
}

fn pydantic_keys(v: &Value) -> Value {
    match v {
        Value::List(l) => Value::List(l.iter().map(pydantic_keys).collect()),
        Value::Obj(o) => {
            let mut out = Object::new();
            for k in o.keys() {
                out.set(&key_text(k), pydantic_keys(o.value(k)));
            }
            Value::Obj(out)
        }
        other => other.clone(),
    }
}

/// `model_dump_json()`: compact, non-ASCII kept.
pub fn dump_json(v: &Value) -> String {
    let mut b = String::new();
    compact(&mut b, v);
    b
}

fn compact(b: &mut String, v: &Value) {
    match v {
        Value::Float(f) => b.push_str(&float_json(*f)),
        Value::List(l) => {
            b.push('[');
            for (i, e) in l.iter().enumerate() {
                if i > 0 {
                    b.push(',');
                }
                compact(b, e);
            }
            b.push(']');
        }
        Value::Obj(o) => {
            b.push('{');
            for (i, k) in o.keys().iter().enumerate() {
                if i > 0 {
                    b.push(',');
                }
                b.push_str(&dumps(&Value::Str(key_text(k)), false));
                b.push(':');
                compact(b, o.value(k));
            }
            b.push('}');
        }
        other => b.push_str(&dumps(other, false)),
    }
}

/// `model_dump_json(indent=n)`.
pub fn dump_json_indent(v: &Value, n: usize) -> String {
    dumps_indent_with(&pydantic_keys(v), n, false, &float_json)
}

/// `model_dump(mode="json")`: the same values, with NaN and the
/// infinities as None.
pub fn json_mode(v: &Value) -> Value {
    crate::gate::pyjson::any_json(v)
}

/// `json.dumps(v)` with its default ensure_ascii.
pub fn py_dumps(v: &Value) -> String {
    dumps(v, true)
}

/// `json.dumps(v, indent=n)`.
pub fn py_dumps_indent(v: &Value, n: usize) -> String {
    dumps_indent_with(v, n, true, &float_repr)
}
