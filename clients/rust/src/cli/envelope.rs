//! `Envelope(**in).model_dump()` for the envelopes `gate init` and
//! `gate register` write: every field in model order with its default
//! filled in. Only the coercions pydantic is known to make are made; a
//! value it may read another way is refused (`Bad::Unsure`) and nothing is
//! written. The Go client's `gateroot/envelope.go` is the reference.

use super::gateroot::{envelopes_dir, join, mkdir_private, safe_session_id, write_file_mode};
use crate::gate::pyjson::{dumps_indent_with, float_repr, Object, Value};
use regex::Regex;
use std::sync::OnceLock;

/// Why an envelope was not read.
#[derive(Debug)]
pub enum Bad {
    /// pydantic may coerce or refuse it in a way not modelled.
    Unsure,
    /// pydantic is certain to refuse it.
    Invalid(String),
    /// No randomness for the id.
    Io(String),
}

impl std::fmt::Display for Bad {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Bad::Unsure => write!(f, "the envelope holds a value this binary does not read yet"),
            Bad::Invalid(w) => write!(f, "invalid envelope: {w}"),
            Bad::Io(w) => write!(f, "{w}"),
        }
    }
}

type R<T> = Result<T, Bad>;

fn invalid<T>(why: String) -> R<T> {
    Err(Bad::Invalid(why))
}

/// `models.non_finite_error`'s refusal: an envelope with NaN, Infinity or
/// -Infinity in any number is invalid, never a null limit.
fn non_finite<T>(k: &str) -> R<T> {
    invalid(format!("{k}: Input should be a finite number"))
}

/// `gate._STARTER_SHELL_ALLOWLIST`, sorted.
pub const STARTER_ALLOWLIST: &[&str] = &[
    "cargo", "cat", "cd", "echo", "find", "git", "grep", "head", "ls", "npm", "printf", "pwd", "pytest", "python",
    "python3", "rg", "sort", "tail", "uniq", "wc", "which",
];

/// `FallbackStrategy.model`'s default.
pub const DEFAULT_FALLBACK_MODEL: &str = "anthropic/claude-sonnet-4-20250514";

fn new_id() -> R<String> {
    use std::io::Read;
    let mut b = [0u8; 4];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut b))
        .map_err(|e| Bad::Io(e.to_string()))?;
    Ok(format!("env_{}", b.iter().map(|x| format!("{x:02x}")).collect::<String>()))
}

fn strs(ss: &[String]) -> Value {
    Value::List(ss.iter().map(|s| Value::Str(s.clone())).collect())
}

/// `gate.starter_envelope(ws, stakes="medium", allow_shell_decomposition=d)`
/// as a model dump; `ws` is resolved.
pub fn starter(ws: &str, decompose: bool) -> R<Object> {
    let allow: Vec<String> = STARTER_ALLOWLIST.iter().map(|s| s.to_string()).collect();
    let pattern = vec![format!("{ws}/**")];
    let perms = Object::new()
        .with("file_read", strs(&pattern))
        .with("file_write", strs(&pattern))
        .with("shell", true)
        .with("shell_allowlist", strs(&allow))
        .with("shell_allow_decomposition", decompose)
        .with("network", false)
        .with("max_execution_time_s", Value::Int("60".into()))
        .with("max_output_size_mb", Value::Int("20".into()));
    let input = Object::new()
        .with("id", new_id()?)
        .with("generated_by", "opendaisugi.gate.starter_envelope")
        .with("task", format!("session in {ws}"))
        .with("permissions", perms)
        .with("stakes", "medium");
    validate(&input)
}

/// `gate.register_envelope`: the dump written to
/// `envelopes/<safe session or default>.json`, the directory 0700 and the
/// file 0600 from the moment it exists. Returns the path and, when that
/// path was a symlink, the file written.
pub fn register(env: &Object, session: &str, root: &str) -> (String, Option<String>, std::io::Result<()>) {
    let d = envelopes_dir(root);
    let name = if session.is_empty() { "default".to_string() } else { safe_session_id(session) };
    let p = join(&d, &format!("{name}.json"));
    if let Err(e) = mkdir_private(&d) {
        return (p, None, Err(e));
    }
    // pydantic's model_dump_json keeps non-ASCII text as is.
    let text = dumps_indent_with(&Value::Obj(env.clone()), 2, false, &float_json);
    match write_file_mode(&p, &text, 0o600) {
        Ok(f) => (p, f, Ok(())),
        Err(e) => (p, None, Err(e)),
    }
}

/// `Envelope(**in)` then `model_dump()`.
pub fn validate(input: &Object) -> R<Object> {
    let mut out = Object::new();
    let id = match opt_str(input, "id", false)? {
        Value::Null => Value::Str(new_id()?),
        v => v,
    };
    out.set("id", id);
    for k in ["generated_by", "task"] {
        match input.get(k) {
            None => return invalid(format!("{k}: field required")),
            Some(v) => {
                out.set(k, string(v, k)?);
            }
        }
    }
    let pv = match input.get("permissions") {
        None => return invalid("permissions: field required".into()),
        Some(v) => v,
    };
    out.set("permissions", permission(pv)?);
    for k in ["invariants", "postconditions"] {
        out.set(k, model_list(input, k)?);
    }
    out.set("fallback", fallback(input)?);
    out.set("parent_envelope", opt_str(input, "parent_envelope", true)?);
    out.set("tightening_only", boolean(input, "tightening_only", true)?);
    let summary = opt_str(input, "summary", true)?;
    if let Value::Str(s) = &summary {
        if s.chars().count() > 80 {
            return invalid("summary: at most 80 characters".into());
        }
    }
    out.set("summary", summary);
    out.set("cache_key", opt_str(input, "cache_key", true)?);
    out.set("stakes", literal(input, "stakes", "low", &["low", "medium", "high", "physical"])?);
    out.set("shell_interpreter_policy", literal(input, "shell_interpreter_policy", "surface", &["surface", "strict", "allow"])?);
    Ok(out)
}

fn kind_of(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Int(_) => "int",
        Value::Float(_) => "float",
        Value::Str(_) => "string",
        Value::List(_) | Value::Tuple(_) => "list",
        Value::Obj(_) => "object",
    }
}

fn string(v: &Value, k: &str) -> R<String> {
    match v {
        Value::Str(s) => Ok(s.clone()),
        other => invalid(format!("{k}: a string is required, not {}", kind_of(other))),
    }
}

fn opt_str(o: &Object, k: &str, nullable: bool) -> R<Value> {
    match o.get(k) {
        None => Ok(Value::Null),
        Some(Value::Null) if nullable => Ok(Value::Null),
        Some(Value::Null) => invalid(format!("{k}: a string is required, not null")),
        Some(v) => Ok(Value::Str(string(v, k)?)),
    }
}

/// pydantic's lax bool.
fn boolean(o: &Object, k: &str, def: bool) -> R<bool> {
    let v = match o.get(k) {
        None => return Ok(def),
        Some(v) => v,
    };
    match v {
        Value::Bool(b) => return Ok(*b),
        Value::Int(t) if t == "0" || t == "1" => return Ok(t == "1"),
        Value::Float(f) if *f == 0.0 || *f == 1.0 => return Ok(*f == 1.0),
        Value::Str(s) => match s.to_lowercase().as_str() {
            "true" | "yes" | "on" | "t" | "y" | "1" => return Ok(true),
            "false" | "no" | "off" | "f" | "n" | "0" => return Ok(false),
            _ => {}
        },
        _ => {}
    }
    invalid(format!("{k}: a bool is required, not {}", kind_of(v)))
}

fn re(which: usize) -> &'static Regex {
    static RES: OnceLock<Vec<Regex>> = OnceLock::new();
    &RES.get_or_init(|| {
        [
            r"^[+-]?[0-9](?:_?[0-9])*$",
            r"^([+-]?[0-9]+)\.0+$",
            r"^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$",
            r"^[0-9+\-_.eE \t\n\r\x0c\x0b]*$",
        ]
        .iter()
        .map(|p| Regex::new(p).expect("a fixed pattern"))
        .collect()
    })[which]
}

/// An integer's decimal text as Python prints `int(x)`.
fn int_text(digits: &str) -> Option<String> {
    let (neg, d) = match digits.strip_prefix('-') {
        Some(d) => (true, d),
        None => (false, digits.strip_prefix('+').unwrap_or(digits)),
    };
    if d.is_empty() || !d.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let d = d.trim_start_matches('0');
    if d.is_empty() {
        return Some("0".into());
    }
    Some(if neg { format!("-{d}") } else { d.to_string() })
}

/// Go's strings.TrimSpace: ASCII space and the Unicode White_Space set.
fn trim_space(s: &str) -> &str {
    s.trim_matches(|c: char| c.is_whitespace())
}

/// pydantic's lax int.
fn lax_int(v: &Value, k: &str) -> R<Value> {
    match v {
        Value::Int(_) => return Ok(v.clone()),
        Value::Bool(b) => return Ok(Value::Int(if *b { "1" } else { "0" }.into())),
        Value::Float(f) => {
            if f.is_infinite() || f.is_nan() || *f != f.trunc() {
                return invalid(format!("{k}: an integer is required, not {}", crate::gate::pyjson::py_float_repr(*f)));
            }
            if f.abs() > (1u64 << 53) as f64 {
                return Err(Bad::Unsure);
            }
            return Ok(Value::Int(format!("{}", *f as i64)));
        }
        Value::Str(x) => {
            let t = trim_space(x);
            if re(0).is_match(t) {
                if let Some(n) = int_text(&t.replace('_', "")) {
                    return Ok(Value::Int(n));
                }
            }
            if let Some(m) = re(1).captures(t) {
                if let Some(n) = int_text(&m[1]) {
                    return Ok(Value::Int(n));
                }
            }
            if re(3).is_match(x) {
                return Err(Bad::Unsure);
            }
        }
        _ => {}
    }
    invalid(format!("{k}: an integer is required, not {}", kind_of(v)))
}

fn integer(o: &Object, k: &str, def: &str) -> R<Value> {
    match o.get(k) {
        None => Ok(Value::Int(def.into())),
        Some(v) => lax_int(v, k),
    }
}

/// pydantic's lax float.
fn lax_float(v: &Value, k: &str) -> R<Value> {
    let f = match v {
        Value::Int(t) => match t.parse::<f64>() {
            Ok(f) if f.is_finite() => f,
            // An int too large for a float reads as an infinity.
            Ok(_) => return non_finite(k),
            _ => return Err(Bad::Unsure),
        },
        Value::Float(f) => {
            if f.is_infinite() || f.is_nan() {
                return non_finite(k);
            }
            *f
        }
        Value::Bool(b) => {
            if *b {
                1.0
            } else {
                0.0
            }
        }
        Value::Str(x) => {
            if matches!(crate::gate::pymodel::str_as_float(x), Ok(f) if !f.is_finite()) {
                return non_finite(k);
            }
            let t = trim_space(x);
            if re(2).is_match(t) {
                if let Ok(f) = t.parse::<f64>() {
                    if f.is_finite() {
                        return Ok(Value::Float(f));
                    }
                }
            }
            let l = x.to_lowercase();
            if re(3).is_match(x) || l.contains(['n', 'i', 'f']) {
                return Err(Bad::Unsure);
            }
            return invalid(format!("{k}: a number is required, not {}", kind_of(v)));
        }
        _ => return invalid(format!("{k}: a number is required, not {}", kind_of(v))),
    };
    Ok(Value::Float(f))
}

/// pydantic's JSON text for a float.
pub fn float_json(f: f64) -> String {
    let s = float_repr(f);
    if let Some(mant) = s.strip_suffix("e-05") {
        // Python goes to scientific notation below 1e-4, pydantic below
        // 1e-5: 2.5e-05 is written 0.000025.
        let (sign, mant) = match mant.strip_prefix('-') {
            Some(m) => ("-", m),
            None => ("", mant),
        };
        return format!("{sign}0.0000{}", mant.replacen('.', "", 1));
    }
    if let Some(i) = s.find('e') {
        let (mant, exp) = (&s[..i], &s[i + 1..]);
        let (sign, exp) = match exp.chars().next() {
            Some(c @ ('+' | '-')) => (c.to_string(), &exp[1..]),
            _ => (String::new(), exp),
        };
        let exp = exp.trim_start_matches('0');
        let exp = if exp.is_empty() { "0" } else { exp };
        return format!("{mant}e{sign}{exp}");
    }
    s
}

fn float_tuple(v: &Value, n: usize, k: &str) -> R<Value> {
    let l = match v {
        Value::List(l) => l,
        other => return invalid(format!("{k}: a list of {n} numbers is required, not {}", kind_of(other))),
    };
    if l.len() != n {
        return invalid(format!("{k}: a list of {n} numbers is required, not {}", l.len()));
    }
    Ok(Value::List(l.iter().map(|e| lax_float(e, k)).collect::<R<Vec<_>>>()?))
}

fn bounds(v: &Value, k: &str) -> R<Value> {
    match v {
        Value::List(l) if l.len() == 2 => Ok(Value::List(l.iter().map(|e| float_tuple(e, 3, k)).collect::<R<Vec<_>>>()?)),
        _ => invalid(format!("{k}: two lists of three numbers are required")),
    }
}

fn opt_float(p: &Object, k: &str) -> R<Value> {
    match p.get(k) {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(v) => lax_float(v, k),
    }
}

fn str_list(o: &Object, k: &str) -> R<Value> {
    match o.get(k) {
        None => Ok(Value::List(vec![])),
        Some(Value::List(l)) => {
            Ok(Value::List(l.iter().map(|e| string(e, &format!("{k}[]")).map(Value::Str)).collect::<R<Vec<_>>>()?))
        }
        Some(v) => invalid(format!("{k}: a list is required, not {}", kind_of(v))),
    }
}

fn literal(o: &Object, k: &str, def: &str, allowed: &[&str]) -> R<String> {
    let v = match o.get(k) {
        None => return Ok(def.into()),
        Some(v) => v,
    };
    let s = string(v, k)?;
    if allowed.contains(&s.as_str()) {
        return Ok(s);
    }
    invalid(format!("{k}: {s:?} is not one of {allowed:?}"))
}

fn object<'a>(v: &'a Value, k: &str) -> R<&'a Object> {
    match v {
        Value::Obj(o) => Ok(o),
        other => invalid(format!("{k}: an object is required, not {}", kind_of(other))),
    }
}

fn permission(v: &Value) -> R<Object> {
    let p = object(v, "permissions")?;
    let mut out = Object::new();
    out.set("file_read", str_list(p, "file_read")?);
    out.set("file_write", str_list(p, "file_write")?);
    out.set("network", boolean(p, "network", false)?);
    out.set("network_hosts", str_list(p, "network_hosts")?);
    out.set("shell", boolean(p, "shell", false)?);
    out.set("shell_allowlist", str_list(p, "shell_allowlist")?);
    out.set("shell_allow_decomposition", boolean(p, "shell_allow_decomposition", false)?);
    out.set("mcp_allowlist", str_list(p, "mcp_allowlist")?);
    out.set("custom_step_allowlist", str_list(p, "custom_step_allowlist")?);
    out.set("max_execution_time_s", integer(p, "max_execution_time_s", "30")?);
    out.set("max_output_size_mb", integer(p, "max_output_size_mb", "10")?);
    let wb = match p.get("workspace_bounds") {
        None | Some(Value::Null) => Value::Null,
        Some(x) => bounds(x, "workspace_bounds")?,
    };
    out.set("workspace_bounds", wb);
    let mut obstacles = vec![];
    if let Some(x) = p.get("obstacles") {
        let l = match x {
            Value::List(l) => l,
            other => return invalid(format!("obstacles: a list is required, not {}", kind_of(other))),
        };
        for e in l {
            obstacles.push(bounds(e, "obstacles[]")?);
        }
    }
    out.set("obstacles", Value::List(obstacles));
    out.set("velocity_limit", opt_float(p, "velocity_limit")?);
    let mut joints = Object::new();
    if let Some(x) = p.get("joint_limits") {
        let o = match x {
            Value::Obj(o) => o,
            other => return invalid(format!("joint_limits: an object is required, not {}", kind_of(other))),
        };
        for name in o.keys() {
            joints.set(name, float_tuple(o.value(name), 2, &format!("joint_limits.{name}"))?);
        }
    }
    out.set("joint_limits", joints);
    out.set("torque_limit", opt_float(p, "torque_limit")?);
    Ok(out)
}

fn fallback(input: &Object) -> R<Object> {
    let mut out = Object::new()
        .with("strategy", "tier2_recompute")
        .with("model", DEFAULT_FALLBACK_MODEL)
        .with("include_refinement", true);
    let v = match input.get("fallback") {
        None => return Ok(out),
        Some(v) => v,
    };
    let f = object(v, "fallback")?;
    for k in ["strategy", "model"] {
        if let Some(x) = f.get(k) {
            out.set(k, string(x, &format!("fallback.{k}"))?);
        }
    }
    out.set("include_refinement", boolean(f, "include_refinement", true)?);
    Ok(out)
}

fn model_list(input: &Object, k: &str) -> R<Value> {
    let l = match input.get(k) {
        None => return Ok(Value::List(vec![])),
        Some(Value::List(l)) => l,
        Some(_) => return Err(Bad::Unsure),
    };
    let mut out = vec![];
    for e in l {
        let o = object(e, &format!("{k}[]"))?;
        out.push(Value::Obj(if k == "invariants" { invariant(o)? } else { postcondition(o)? }));
    }
    Ok(Value::List(out))
}

fn required_str(o: &Object, k: &str, where_: &str) -> R<String> {
    match o.get(k) {
        None => invalid(format!("{where_}.{k}: field required")),
        Some(v) => string(v, &format!("{where_}.{k}")),
    }
}

fn invariant(o: &Object) -> R<Object> {
    let mut out = Object::new();
    out.set("type", required_str(o, "type", "invariants[]")?);
    for k in ["target", "scope"] {
        out.set(k, opt_str(o, k, true)?);
    }
    out.set("description", required_str(o, "description", "invariants[]")?);
    out.set("expr", expr(o)?);
    out.set("enforce", boolean(o, "enforce", true)?);
    Ok(out)
}

fn opt_int(o: &Object, k: &str) -> R<Value> {
    match o.get(k) {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(_) => integer(o, k, ""),
    }
}

fn postcondition(o: &Object) -> R<Object> {
    let mut out = Object::new();
    out.set("type", required_str(o, "type", "postconditions[]")?);
    out.set("path", opt_str(o, "path", true)?);
    for k in ["expected", "min", "max"] {
        out.set(k, opt_int(o, k)?);
    }
    out.set("description", opt_str(o, "description", true)?);
    out.set("expr", expr(o)?);
    out.set("enforce", boolean(o, "enforce", true)?);
    Ok(out)
}

/// An invariant's or postcondition's expr: any JSON value, its floats in
/// pydantic's form (written so by `register`).
fn expr(o: &Object) -> R<Value> {
    match o.get("expr") {
        None => Ok(Value::Null),
        Some(v) => pydantic_floats(v),
    }
}

fn pydantic_floats(v: &Value) -> R<Value> {
    Ok(match v {
        Value::Float(f) if f.is_infinite() || f.is_nan() => return non_finite("expr"),
        Value::List(l) => Value::List(l.iter().map(pydantic_floats).collect::<R<Vec<_>>>()?),
        Value::Obj(o) => {
            let mut out = Object::new();
            for k in o.keys() {
                out.set(k, pydantic_floats(o.value(k))?);
            }
            Value::Obj(out)
        }
        other => other.clone(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::loads;

    fn obj(text: &str) -> Object {
        match loads(text).unwrap() {
            Value::Obj(o) => o,
            _ => panic!("not an object"),
        }
    }

    /// An envelope with NaN, Infinity or -Infinity in any number is invalid
    /// (models.non_finite_error): register refuses it for certain.
    #[test]
    fn non_finite_numbers_are_invalid() {
        let big = format!("1{}", "0".repeat(400));
        for perms in [
            r#"{"velocity_limit": NaN}"#.to_string(),
            r#"{"torque_limit": Infinity}"#.to_string(),
            r#"{"velocity_limit": " nan "}"#.to_string(),
            r#"{"velocity_limit": "-Infinity"}"#.to_string(),
            r#"{"velocity_limit": "1e999"}"#.to_string(),
            format!(r#"{{"velocity_limit": {big}}}"#),
            r#"{"workspace_bounds": [[0, 0, 0], [1, -Infinity, 1]]}"#.to_string(),
            r#"{"joint_limits": {"j": [NaN, 1]}}"#.to_string(),
        ] {
            let e = validate(&obj(&format!(r#"{{"generated_by": "g", "task": "t", "permissions": {perms}}}"#)));
            assert!(matches!(e, Err(Bad::Invalid(_))), "{perms}: {e:?}");
        }
        let e = validate(&obj(
            r#"{"generated_by": "g", "task": "t", "permissions": {}, "invariants": [{"type": "t", "description": "d", "expr": [1, NaN]}]}"#,
        ));
        assert!(matches!(e, Err(Bad::Invalid(_))), "{e:?}");
        assert!(validate(&obj(r#"{"generated_by": "g", "task": "t", "permissions": {"velocity_limit": 2.5}}"#)).is_ok());
    }
}
