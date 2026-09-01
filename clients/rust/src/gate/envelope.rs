//! A registered envelope, read as `Envelope.model_validate_json` reads it:
//! jiter parses the JSON exactly as pydantic-core does, and the models'
//! fields are validated with pydantic's lax rules and error text.

use super::paths::{exists, os_error, os_path, path_join};
use super::pydantic::{non_finite, validation_error, LineErr};
use super::pymodel::{at, decode_utf8_strict, universal_newlines, In, Loc, Val};
use super::pyjson::{py_repr, Object, Value};
use super::{random_hex, Fault, PyErr, Runner, R};

/// `models.Invariant` and `models.Postcondition`, as far as the verifier
/// reads them.
#[derive(Debug, Clone)]
pub struct Item {
    pub typ: String,
    /// `description`: a str for an invariant, `str | None` for a
    /// postcondition.
    pub description: Value,
    pub expr: Option<Value>,
    pub enforce: bool,
}

#[derive(Debug, Clone)]
pub struct Envelope {
    pub id: String,
    pub task: String,
    pub policy: String,
    pub stakes: String,
    pub file_read: Vec<String>,
    pub file_write: Vec<String>,
    pub network: bool,
    pub network_hosts: Vec<String>,
    pub shell: bool,
    pub shell_allowlist: Vec<String>,
    pub shell_allow_decomposition: bool,
    pub mcp_allowlist: Vec<String>,
    pub custom_step_allowlist: Vec<String>,
    /// `max_execution_time_s` as its decimal text: any size.
    pub max_execution_time_s: String,
    pub workspace_bounds: bool,
    pub velocity_limit: bool,
    pub invariants: Vec<Item>,
    pub postconditions: Vec<Item>,
    /// `envelope.model_dump(mode="json")`: every field, defaults included.
    pub dump: Object,
}

impl Runner {
    /// `gate.load_envelope`: the session's own file first, then default.
    /// `None` means none is registered.
    pub fn load_envelope(&self, session_id: &Value) -> R<Option<Envelope>> {
        let mut candidates = Vec::new();
        if session_id.truthy() {
            candidates.push(super::record::safe_session_value(session_id)?);
        }
        candidates.push("default".to_string());
        for name in candidates {
            let p = path_join(&path_join(&self.root, "envelopes"), &format!("{name}.json"));
            if exists(&p)? {
                return load_envelope_file(&p).map(Some);
            }
        }
        Ok(None)
    }
}

/// `Envelope.model_validate_json(path.read_text(encoding="utf-8"))`.
pub fn load_envelope_file(p: &str) -> R<Envelope> {
    let raw = std::fs::read(os_path("open", p)?).map_err(|e| Fault::from(os_error(&e, p)))?;
    let text = decode_utf8_strict(&raw).map_err(|m| Fault::from(PyErr::new("UnicodeDecodeError", m)))?;
    parse_envelope(&universal_newlines(&text))
}

/// `Envelope.model_validate_json(text)`.
pub fn parse_envelope(text: &str) -> R<Envelope> {
    let bytes = text.as_bytes();
    let json = match jiter::JsonValue::parse_with_config(bytes, true, jiter::PartialMode::Off) {
        Ok(v) => v,
        Err(e) => {
            let err = LineErr {
                loc: vec![],
                msg: format!("Invalid JSON: {}", e.description(bytes)),
                typ: "json_invalid".into(),
                input: Some((py_repr(&Value::Str(text.to_string()))?, "str".into())),
            };
            return Err(validation_error("Envelope", &[err]).into());
        }
    };
    let input = In::from_json(&json);
    let mut v = Val::new(true);
    let env = envelope(&mut v, &input)?;
    match env {
        Some(e) if v.errs.is_empty() => Ok(e),
        _ => Err(validation_error("Envelope", &v.errs).into()),
    }
}

/// A field of a model: its validated value, its default when absent, or
/// the required-field error. Values are as `model_dump(mode="json")`
/// gives them.
fn field(
    v: &mut Val,
    loc: &Loc,
    model: &In,
    name: &str,
    default: Option<Value>,
    f: impl FnOnce(&mut Val, &Loc, &In) -> R<Option<Value>>,
    out: &mut Option<Object>,
) -> R<()> {
    let l = at(loc, name);
    let got = match model.get(name) {
        Some(x) => f(v, &l, x)?,
        None => match default {
            Some(d) => Some(d),
            None => {
                v.missing(&l, model)?;
                None
            }
        },
    };
    match (got, out.as_mut()) {
        (Some(val), Some(o)) => {
            o.set(name, val);
        }
        _ => *out = None,
    }
    Ok(())
}

fn string(v: &mut Val, l: &Loc, e: &In) -> R<Option<Value>> {
    Ok(v.string(l, e)?.map(Value::Str))
}

fn boolean(v: &mut Val, l: &Loc, e: &In) -> R<Option<Value>> {
    Ok(v.boolean(l, e)?.map(Value::Bool))
}

fn int(v: &mut Val, l: &Loc, e: &In) -> R<Option<Value>> {
    Ok(v.int(l, e)?.map(Value::Int))
}

fn float(v: &mut Val, l: &Loc, e: &In) -> R<Option<Value>> {
    Ok(v.float(l, e)?.map(Value::Float))
}

fn opt(
    f: impl Fn(&mut Val, &Loc, &In) -> R<Option<Value>>,
) -> impl Fn(&mut Val, &Loc, &In) -> R<Option<Value>> {
    move |v, l, e| Ok(v.nullable(l, e, &f)?.map(|o| o.unwrap_or(Value::Null)))
}

fn str_list(v: &mut Val, loc: &Loc, x: &In) -> R<Option<Value>> {
    Ok(v.list(loc, x, string)?.map(Value::List))
}

fn vec3(v: &mut Val, loc: &Loc, x: &In) -> R<Option<Value>> {
    Ok(v.tuple(loc, x, 3, float)?.map(Value::List))
}

fn bounds(v: &mut Val, loc: &Loc, x: &In) -> R<Option<Value>> {
    Ok(v.tuple(loc, x, 2, vec3)?.map(Value::List))
}

/// `Any`: the Python value, kept as JSON gave it.
fn any(_: &mut Val, _: &Loc, e: &In) -> R<Option<Value>> {
    Ok(Some(e.to_value()))
}

fn literal(options: &'static [&'static str]) -> impl Fn(&mut Val, &Loc, &In) -> R<Option<Value>> {
    move |v, l, e| Ok(v.literal(l, e, options)?.map(Value::Str))
}

fn model(
    v: &mut Val,
    loc: &Loc,
    x: &In,
    fields: impl FnOnce(&mut Val, &mut Option<Object>) -> R<()>,
) -> R<Option<Value>> {
    if !matches!(x, In::Dict(_)) {
        v.model_type(loc, x)?;
        return Ok(None);
    }
    let mut out = Some(Object::new());
    fields(v, &mut out)?;
    Ok(out.map(Value::Obj))
}

fn envelope(v: &mut Val, x: &In) -> R<Option<Envelope>> {
    let root: Loc = vec![];
    let dump = model(v, &root, x, |v, o| {
        field(v, &root, x, "id", Some(Value::Null), string, o)?;
        field(v, &root, x, "generated_by", None, string, o)?;
        field(v, &root, x, "task", None, string, o)?;
        field(v, &root, x, "permissions", None, permission, o)?;
        field(v, &root, x, "invariants", Some(Value::List(vec![])), |v, l, e| items(v, l, e, true), o)?;
        field(v, &root, x, "postconditions", Some(Value::List(vec![])), |v, l, e| items(v, l, e, false), o)?;
        field(v, &root, x, "fallback", Some(fallback_default()), fallback, o)?;
        field(v, &root, x, "parent_envelope", Some(Value::Null), opt(string), o)?;
        field(v, &root, x, "tightening_only", Some(Value::Bool(true)), boolean, o)?;
        field(v, &root, x, "summary", Some(Value::Null), opt(|v, l, e| Ok(v.string_max(l, e, 80)?.map(Value::Str))), o)?;
        field(v, &root, x, "cache_key", Some(Value::Null), opt(string), o)?;
        field(v, &root, x, "stakes", Some(Value::Str("low".into())), literal(&["low", "medium", "high", "physical"]), o)?;
        field(
            v,
            &root,
            x,
            "shell_interpreter_policy",
            Some(Value::Str("surface".into())),
            literal(&["surface", "strict", "allow"]),
            o,
        )
    })?;
    let mut dump = match dump {
        Some(Value::Obj(d)) if v.errs.is_empty() => d,
        _ => return Ok(None),
    };
    // The model's after-validator (models.non_finite_error): each NaN,
    // Infinity or -Infinity in the validated envelope is an error at its
    // place, in document order. A NaN limit must never read as no limit.
    non_finite(&Value::Obj(dump.clone()), &root, &mut v.errs)?;
    if !v.errs.is_empty() {
        return Ok(None);
    }
    // `id` has a default factory: `env_` and eight hex digits.
    if dump.value("id").is_null() {
        dump.set("id", format!("env_{}", random_hex(4)?));
    }
    Ok(Some(from_dump(dump)))
}

fn fallback_default() -> Value {
    Value::Obj(
        Object::new()
            .with("strategy", "tier2_recompute")
            .with("model", "anthropic/claude-sonnet-4-20250514")
            .with("include_refinement", true),
    )
}

/// `models.Permission`, in its field order.
fn permission(v: &mut Val, loc: &Loc, x: &In) -> R<Option<Value>> {
    let empty = || Some(Value::List(vec![]));
    model(v, loc, x, |v, o| {
        field(v, loc, x, "file_read", empty(), str_list, o)?;
        field(v, loc, x, "file_write", empty(), str_list, o)?;
        field(v, loc, x, "network", Some(Value::Bool(false)), boolean, o)?;
        field(v, loc, x, "network_hosts", empty(), str_list, o)?;
        field(v, loc, x, "shell", Some(Value::Bool(false)), boolean, o)?;
        field(v, loc, x, "shell_allowlist", empty(), str_list, o)?;
        field(v, loc, x, "shell_allow_decomposition", Some(Value::Bool(false)), boolean, o)?;
        field(v, loc, x, "mcp_allowlist", empty(), str_list, o)?;
        field(v, loc, x, "custom_step_allowlist", empty(), str_list, o)?;
        field(v, loc, x, "max_execution_time_s", Some(Value::Int("30".into())), int, o)?;
        field(v, loc, x, "max_output_size_mb", Some(Value::Int("10".into())), int, o)?;
        field(v, loc, x, "workspace_bounds", Some(Value::Null), opt(bounds), o)?;
        field(v, loc, x, "obstacles", empty(), |v, l, e| Ok(v.list(l, e, bounds)?.map(Value::List)), o)?;
        field(v, loc, x, "velocity_limit", Some(Value::Null), opt(float), o)?;
        field(
            v,
            loc,
            x,
            "joint_limits",
            Some(Value::Obj(Object::new())),
            |v, l, e| {
                Ok(v.dict(l, e, |v, l, e| Ok(v.tuple(l, e, 2, float)?.map(Value::List)))?.map(|pairs| {
                    let mut o = Object::new();
                    for (k, val) in pairs {
                        o.set(&k, val);
                    }
                    Value::Obj(o)
                }))
            },
            o,
        )?;
        field(v, loc, x, "torque_limit", Some(Value::Null), opt(float), o)
    })
}

fn items(v: &mut Val, loc: &Loc, x: &In, invariant: bool) -> R<Option<Value>> {
    Ok(v.list(loc, x, |v, l, e| item(v, l, e, invariant))?.map(Value::List))
}

/// `models.Invariant` (`invariant`) or `models.Postcondition`.
fn item(v: &mut Val, loc: &Loc, x: &In, invariant: bool) -> R<Option<Value>> {
    model(v, loc, x, |v, o| {
        field(v, loc, x, "type", None, string, o)?;
        if invariant {
            field(v, loc, x, "target", Some(Value::Null), opt(string), o)?;
            field(v, loc, x, "scope", Some(Value::Null), opt(string), o)?;
            field(v, loc, x, "description", None, string, o)?;
        } else {
            field(v, loc, x, "path", Some(Value::Null), opt(string), o)?;
            field(v, loc, x, "expected", Some(Value::Null), opt(int), o)?;
            field(v, loc, x, "min", Some(Value::Null), opt(int), o)?;
            field(v, loc, x, "max", Some(Value::Null), opt(int), o)?;
            field(v, loc, x, "description", Some(Value::Null), opt(string), o)?;
        }
        field(v, loc, x, "expr", Some(Value::Null), any, o)?;
        field(v, loc, x, "enforce", Some(Value::Bool(true)), boolean, o)
    })
}

/// `models.FallbackStrategy`.
fn fallback(v: &mut Val, loc: &Loc, x: &In) -> R<Option<Value>> {
    model(v, loc, x, |v, o| {
        field(v, loc, x, "strategy", Some(Value::Str("tier2_recompute".into())), string, o)?;
        field(v, loc, x, "model", Some(Value::Str("anthropic/claude-sonnet-4-20250514".into())), string, o)?;
        field(v, loc, x, "include_refinement", Some(Value::Bool(true)), boolean, o)
    })
}

fn strs(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l.iter().filter_map(|x| x.as_str().map(str::to_string)).collect(),
        _ => vec![],
    }
}

fn to_item(v: &Value) -> Item {
    let o = v.as_obj().cloned().unwrap_or_default();
    let expr = o.value("expr");
    Item {
        typ: o.value("type").as_str().unwrap_or("").to_string(),
        description: o.value("description").clone(),
        expr: if expr.is_null() { None } else { Some(expr.clone()) },
        enforce: matches!(o.value("enforce"), Value::Bool(true)),
    }
}

/// The typed view the verifier reads, from the validated dump.
pub(crate) fn from_dump(dump: Object) -> Envelope {
    let s = |k: &str| dump.value(k).as_str().unwrap_or("").to_string();
    let p = dump.value("permissions").as_obj().cloned().unwrap_or_default();
    let b = |k: &str| matches!(p.value(k), Value::Bool(true));
    let items = |k: &str| match dump.value(k) {
        Value::List(l) => l.iter().map(to_item).collect(),
        _ => vec![],
    };
    Envelope {
        id: s("id"),
        task: s("task"),
        policy: s("shell_interpreter_policy"),
        stakes: s("stakes"),
        file_read: strs(p.value("file_read")),
        file_write: strs(p.value("file_write")),
        network: b("network"),
        network_hosts: strs(p.value("network_hosts")),
        shell: b("shell"),
        shell_allowlist: strs(p.value("shell_allowlist")),
        shell_allow_decomposition: b("shell_allow_decomposition"),
        mcp_allowlist: strs(p.value("mcp_allowlist")),
        custom_step_allowlist: strs(p.value("custom_step_allowlist")),
        max_execution_time_s: match p.value("max_execution_time_s") {
            Value::Int(t) => t.clone(),
            _ => "30".into(),
        },
        workspace_bounds: !p.value("workspace_bounds").is_null(),
        velocity_limit: !p.value("velocity_limit").is_null(),
        invariants: items("invariants"),
        postconditions: items("postconditions"),
        dump,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn err_of(text: &str) -> String {
        match parse_envelope(text) {
            Err(Fault::Raised(e)) => e.msg,
            other => panic!("expected an error, got {other:?}"),
        }
    }

    #[test]
    fn a_plain_envelope_reads() {
        let e = parse_envelope(r#"{"id":"e","generated_by":"g","task":"t","permissions":{"network":"yes","max_execution_time_s":"0030"}}"#)
            .unwrap();
        assert!(e.network);
        assert_eq!(e.max_execution_time_s, "30");
        assert_eq!(e.id, "e");
    }

    #[test]
    fn errors_read_as_pydantics() {
        let m = err_of(r#"{"generated_by":"g","permissions":{}}"#);
        assert!(m.starts_with("1 validation error for Envelope\ntask\n  Field required [type=missing, input_value={'generated_by': 'g', 'permissions': {}}, input_type=dict]"), "{m}");
        let m = err_of(r#"{"generated_by": "#);
        assert!(m.contains("\n  Invalid JSON: EOF while parsing a value at line 1 column 17 [type=json_invalid"), "{m}");
        let m = err_of(r#"{"generated_by":"g","task":"t","permissions":{"workspace_bounds":[[0,0,"x",9],[1,1,1]]}}"#);
        assert!(m.contains("permissions.workspace_bounds.0\n  Tuple should have at most 3 items after validation, not 4 [type=too_long"), "{m}");
    }

    // An envelope with NaN, Infinity or -Infinity in any number is invalid
    // (models.non_finite_error). The texts are the oracle's.
    #[test]
    fn non_finite_numbers_are_invalid() {
        let fin = "    For further information visit https://errors.pydantic.dev/2.13/v/finite_number";
        let m = err_of(r#"{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN}}"#);
        assert_eq!(
            m,
            format!(
                "1 validation error for Envelope\npermissions.velocity_limit\n  Input should be a finite number \
                 [type=finite_number, input_value=nan, input_type=float]\n{fin}"
            )
        );
        let m = err_of(
            r#"{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN, "joint_limits": {"j": [-Infinity, 1]}, "torque_limit": 1e999}}"#,
        );
        assert_eq!(
            m,
            format!(
                "3 validation errors for Envelope\npermissions.velocity_limit\n  Input should be a finite number \
                 [type=finite_number, input_value=nan, input_type=float]\n{fin}\npermissions.joint_limits.j.0\n  \
                 Input should be a finite number [type=finite_number, input_value=-inf, input_type=float]\n{fin}\n\
                 permissions.torque_limit\n  Input should be a finite number [type=finite_number, input_value=inf, \
                 input_type=float]\n{fin}"
            )
        );
        let m = err_of(r#"{"task": "t", "generated_by": "g", "permissions": {"torque_limit": " inf "}}"#);
        assert!(m.contains("permissions.torque_limit\n  Input should be a finite number [type=finite_number, input_value=inf, input_type=float]"), "{m}");
        let m = err_of(
            r#"{"task": "t", "generated_by": "g", "permissions": {}, "invariants": [{"type": "t", "description": "d", "expr": {"op": "equals", "path": "x", "value": [1, NaN]}}]}"#,
        );
        assert!(m.starts_with("1 validation error for Envelope\ninvariants.0.expr.value.1\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]"), "{m}");
        // Another error first: the after-validator does not run.
        let m = err_of(r#"{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN}, "stakes": "nope"}"#);
        assert!(m.starts_with("1 validation error for Envelope\nstakes\n"), "{m}");
    }
}
