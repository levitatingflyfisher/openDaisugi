//! Predicate expression algebra — port of `opendaisugi/predicate.py`
//! (parsing) and the pure-Python evaluation half of
//! `opendaisugi/predicate_z3.py` (`evaluate_predicate` / `_eval_scalar`).
//!
//! `evaluate_predicate` is what `verify._check_predicate_item` actually
//! calls at Stage 2b — it runs over a *concrete* plan, so it never needs Z3;
//! only `check_vacuity` (tautology/contradiction classification, `z3_bridge.rs`)
//! needs the solver.

use serde_json::Value;

#[derive(Debug, Clone)]
pub enum Expr {
    Equals { path: String, value: Value },
    NotEquals { path: String, value: Value },
    InSet { path: String, values: Vec<Value> },
    NotInSet { path: String, values: Vec<Value> },
    Matches { path: String, regex: String },
    NotMatches { path: String, regex: String },
    NumericRange { path: String, min: f64, max: f64 },
    LengthRange { path: String, min: i64, max: Option<i64> },
    Exists { path: String },
    IsEmpty { path: String },
    And { children: Vec<Expr> },
    Or { children: Vec<Expr> },
    Not { child: Box<Expr> },
    Implies { a: Box<Expr>, b: Box<Expr> },
    ForallSteps { pred: Box<Expr> },
    ExistsStep { pred: Box<Expr> },
    ForallOutputs { pred: Box<Expr> },
    DependsOn { step_id_a: String, step_id_b: String },
    Before { step_id_a: String, step_id_b: String },
    AliasRef { name: String },
    LLMCheck { rule: String },
}

fn s(v: &Value, key: &str) -> Result<String, String> {
    v.get(key)
        .and_then(|x| x.as_str())
        .map(String::from)
        .ok_or_else(|| format!("expression missing string field '{key}'"))
}

fn f(v: &Value, key: &str) -> Result<f64, String> {
    v.get(key).and_then(|x| x.as_f64()).ok_or_else(|| format!("expression missing numeric field '{key}'"))
}

fn child(v: &Value, key: &str) -> Result<Expr, String> {
    parse_expression(v.get(key).ok_or_else(|| format!("expression missing field '{key}'"))?)
}

/// Parse a JSON `Expression` dict (discriminated on `"op"`). Mirrors
/// `predicate.parse_expression`.
pub fn parse_expression(v: &Value) -> Result<Expr, String> {
    let op = v.get("op").and_then(|x| x.as_str()).ok_or("expression missing 'op'")?;
    Ok(match op {
        "equals" => Expr::Equals { path: s(v, "path")?, value: v.get("value").cloned().unwrap_or(Value::Null) },
        "not_equals" => Expr::NotEquals { path: s(v, "path")?, value: v.get("value").cloned().unwrap_or(Value::Null) },
        "in_set" => Expr::InSet {
            path: s(v, "path")?,
            values: v.get("values").and_then(|x| x.as_array()).cloned().unwrap_or_default(),
        },
        "not_in_set" => Expr::NotInSet {
            path: s(v, "path")?,
            values: v.get("values").and_then(|x| x.as_array()).cloned().unwrap_or_default(),
        },
        "matches" => Expr::Matches { path: s(v, "path")?, regex: s(v, "regex")? },
        "not_matches" => Expr::NotMatches { path: s(v, "path")?, regex: s(v, "regex")? },
        "numeric_range" => Expr::NumericRange { path: s(v, "path")?, min: f(v, "min")?, max: f(v, "max")? },
        "length_range" => Expr::LengthRange {
            path: s(v, "path")?,
            min: v.get("min").and_then(|x| x.as_i64()).unwrap_or(0),
            max: v.get("max").and_then(|x| x.as_i64()),
        },
        "exists" => Expr::Exists { path: s(v, "path")? },
        "is_empty" => Expr::IsEmpty { path: s(v, "path")? },
        "and" => Expr::And {
            children: v
                .get("children")
                .and_then(|x| x.as_array())
                .ok_or("'and' missing children")?
                .iter()
                .map(parse_expression)
                .collect::<Result<_, _>>()?,
        },
        "or" => Expr::Or {
            children: v
                .get("children")
                .and_then(|x| x.as_array())
                .ok_or("'or' missing children")?
                .iter()
                .map(parse_expression)
                .collect::<Result<_, _>>()?,
        },
        "not" => Expr::Not { child: Box::new(child(v, "child")?) },
        "implies" => Expr::Implies { a: Box::new(child(v, "a")?), b: Box::new(child(v, "b")?) },
        "forall_steps" => Expr::ForallSteps { pred: Box::new(child(v, "pred")?) },
        "exists_step" => Expr::ExistsStep { pred: Box::new(child(v, "pred")?) },
        "forall_outputs" => Expr::ForallOutputs { pred: Box::new(child(v, "pred")?) },
        "depends_on" => Expr::DependsOn { step_id_a: s(v, "step_id_a")?, step_id_b: s(v, "step_id_b")? },
        "before" => Expr::Before { step_id_a: s(v, "step_id_a")?, step_id_b: s(v, "step_id_b")? },
        "alias" => Expr::AliasRef { name: s(v, "name")? },
        "llm_check" => Expr::LLMCheck { rule: s(v, "rule")? },
        other => return Err(format!("unknown predicate op {other:?}")),
    })
}

// --- Python-semantics value comparison --------------------------------------------

/// A value as the oracle's predicate stage holds it: JSON, except that
/// a step's `model_dump()` keeps each tuple-typed field as a tuple, which
/// equals no list.
#[derive(Debug, Clone)]
pub enum Py {
    Json(Value),
    Tuple(Vec<Value>),
    Obj(Vec<(String, Py)>),
    List(Vec<Py>),
}

/// The step fields each step type declares as a tuple.
fn tuple_fields(step_type: &str) -> &'static [&'static str] {
    match step_type {
        "cartesian_move" => &["target_position", "target_orientation"],
        "vla" => &["target_pose"],
        _ => &[],
    }
}

/// A step's raw JSON as its `model_dump()`: each tuple-typed field that
/// holds a list is a tuple.
pub fn dumped_step(step: &Value) -> Py {
    let fields = step.get("type").and_then(|t| t.as_str()).map(tuple_fields).unwrap_or(&[]);
    match step.as_object() {
        Some(o) if !fields.is_empty() => Py::Obj(
            o.iter()
                .map(|(k, v)| {
                    let x = match v {
                        Value::Array(l) if fields.contains(&k.as_str()) => Py::Tuple(l.clone()),
                        other => Py::Json(other.clone()),
                    };
                    (k.clone(), x)
                })
                .collect(),
        ),
        _ => Py::Json(step.clone()),
    }
}

fn is_numericish(v: &Value) -> bool {
    matches!(v, Value::Bool(_) | Value::Number(_))
}

fn numeric_of(v: &Value) -> f64 {
    match v {
        Value::Bool(b) => if *b { 1.0 } else { 0.0 },
        Value::Number(n) => n.as_f64().unwrap_or(f64::NAN),
        _ => f64::NAN,
    }
}

/// Python `==` semantics across JSON-representable types: bool/int/float
/// compare numerically (`True == 1` is True in Python); strings/arrays/
/// objects compare structurally with the same rule applied recursively.
pub fn py_eq(a: &Value, b: &Value) -> bool {
    if is_numericish(a) && is_numericish(b) {
        return numeric_of(a) == numeric_of(b);
    }
    match (a, b) {
        (Value::Null, Value::Null) => true,
        (Value::String(x), Value::String(y)) => x == y,
        (Value::Array(x), Value::Array(y)) => x.len() == y.len() && x.iter().zip(y).all(|(p, q)| py_eq(p, q)),
        (Value::Object(x), Value::Object(y)) => {
            x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).is_some_and(|w| py_eq(v, w)))
        }
        _ => false,
    }
}

/// `==` of a step value against a JSON value: a tuple equals none.
fn py_eq_py(a: &Py, b: &Value) -> bool {
    match (a, b) {
        (Py::Json(x), y) => py_eq(x, y),
        (Py::Tuple(_), _) => false,
        (Py::List(x), Value::Array(y)) => x.len() == y.len() && x.iter().zip(y).all(|(p, q)| py_eq_py(p, q)),
        (Py::Obj(x), Value::Object(y)) => {
            x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).is_some_and(|w| py_eq_py(v, w)))
        }
        _ => false,
    }
}

/// A resolved path: a step value, or a JSON value inside one.
enum At<'a> {
    Py(&'a Py),
    Json(&'a Value),
}

impl<'a> At<'a> {
    /// The JSON value, where the value is plain JSON.
    fn json(&self) -> Option<&'a Value> {
        match *self {
            At::Json(v) | At::Py(Py::Json(v)) => Some(v),
            _ => None,
        }
    }

    fn eq(&self, b: &Value) -> bool {
        match self {
            At::Json(v) => py_eq(v, b),
            At::Py(p) => py_eq_py(p, b),
        }
    }

    fn is_in(&self, values: &[Value]) -> bool {
        values.iter().any(|v| self.eq(v))
    }

    fn len(&self) -> Option<usize> {
        match *self {
            At::Json(v) | At::Py(Py::Json(v)) => has_len(v),
            At::Py(Py::Tuple(l)) => Some(l.len()),
            At::Py(Py::List(l)) => Some(l.len()),
            At::Py(Py::Obj(o)) => Some(o.len()),
        }
    }
}

/// `_resolve_path` — dict-only path walk (the pure-Python evaluation path
/// never resolves against a raw pydantic object; every scope handed to
/// `_eval_scalar` is already a dict). Returns `None` for "MISSING".
fn resolve_path<'a>(scope: &'a Py, path: &str) -> Option<At<'a>> {
    let mut cur = At::Py(scope);
    for part in path.split('.') {
        cur = match cur {
            At::Py(Py::Obj(o)) => At::Py(o.iter().find(|(k, _)| k == part).map(|(_, v)| v)?),
            other => At::Json(other.json()?.as_object()?.get(part)?),
        };
    }
    Some(cur)
}

fn has_len(v: &Value) -> Option<usize> {
    match v {
        Value::String(s) => Some(s.chars().count()),
        Value::Array(a) => Some(a.len()),
        Value::Object(o) => Some(o.len()),
        _ => None,
    }
}

fn compile_regex(pattern: &str) -> Result<regex::Regex, String> {
    regex::Regex::new(pattern).map_err(|e| format!("bad regex {pattern:?}: {e}"))
}

/// `_eval_scalar` — evaluate a per-step scalar predicate against one scope
/// (a JSON object: a step dict, an output dict, or the `{"steps": [...]}`
/// synthetic wrapper).
pub fn eval_scalar(expr: &Expr, scope: &Py) -> Result<bool, String> {
    match expr {
        Expr::Equals { path, value } => Ok(resolve_path(scope, path).is_some_and(|v| v.eq(value))),
        Expr::NotEquals { path, value } => Ok(resolve_path(scope, path).is_some_and(|v| !v.eq(value))),
        Expr::InSet { path, values } => Ok(resolve_path(scope, path).is_some_and(|v| v.is_in(values))),
        Expr::NotInSet { path, values } => Ok(resolve_path(scope, path).is_some_and(|v| !v.is_in(values))),
        Expr::Matches { path, regex } => {
            let re = compile_regex(regex)?;
            Ok(match resolve_path(scope, path).as_ref().and_then(At::json) {
                Some(Value::String(s)) => re.is_match(s),
                _ => false,
            })
        }
        Expr::NotMatches { path, regex } => {
            let re = compile_regex(regex)?;
            Ok(match resolve_path(scope, path).as_ref().and_then(At::json) {
                Some(Value::String(s)) => !re.is_match(s),
                _ => true,
            })
        }
        Expr::NumericRange { path, min, max } => Ok(match resolve_path(scope, path).as_ref().and_then(At::json) {
            Some(v) if is_numericish(v) => {
                let n = numeric_of(v);
                *min <= n && n <= *max
            }
            _ => false,
        }),
        Expr::LengthRange { path, min, max } => Ok(match resolve_path(scope, path).and_then(|v| v.len()) {
            Some(n) => {
                let n = n as i64;
                n >= *min && max.is_none_or(|m| n <= m)
            }
            None => false,
        }),
        Expr::Exists { path } => Ok(resolve_path(scope, path).is_some()),
        Expr::IsEmpty { path } => Ok(match resolve_path(scope, path) {
            None => true,
            Some(v) if v.json().is_some_and(|j| j.is_null()) => true,
            Some(v) => v.len().map(|n| n == 0).unwrap_or(false),
        }),
        Expr::And { children } => {
            for c in children {
                if !eval_scalar(c, scope)? {
                    return Ok(false);
                }
            }
            Ok(true)
        }
        Expr::Or { children } => {
            for c in children {
                if eval_scalar(c, scope)? {
                    return Ok(true);
                }
            }
            Ok(false)
        }
        Expr::Not { child } => Ok(!eval_scalar(child, scope)?),
        Expr::Implies { a, b } => Ok(!eval_scalar(a, scope)? || eval_scalar(b, scope)?),
        Expr::LLMCheck { .. } => Err("LLMCheck must be evaluated via evaluate_llm_check, not eval_scalar".into()),
        Expr::AliasRef { name } => Err(format!("unresolved alias reference '{name}'; resolve aliases before evaluation")),
        other => Err(format!("unknown predicate op: {other:?}")),
    }
}

/// `evaluate_predicate` — the plan-level entry point. `steps` are each
/// step's full raw JSON object (equivalent to `StepBase.model_dump()`).
/// `stakes` is the envelope's stakes (only used to fail-closed LLMCheck at
/// physical stakes, matching the oracle — LLMCheck is otherwise not
/// reproducible offline and does not appear in the corpus).
pub fn evaluate_predicate(expr: &Expr, steps: &[Value], stakes: &str) -> Result<bool, String> {
    let dumped: Vec<Py> = steps.iter().map(dumped_step).collect();
    go(expr, steps, &dumped, stakes)
}

fn go(e: &Expr, steps: &[Value], dumped: &[Py], stakes: &str) -> Result<bool, String> {
    {
        match e {
            Expr::ForallSteps { pred } => {
                for st in dumped {
                    if !eval_scalar(pred, st)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::ExistsStep { pred } => {
                for st in dumped {
                    if eval_scalar(pred, st)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            Expr::ForallOutputs { pred } => {
                let outputs: Vec<Value> = steps
                    .iter()
                    .filter_map(|st| st.get("metadata").and_then(|m| m.get("output")))
                    .filter(|o| !o.is_null())
                    .map(|o| {
                        let mut m = serde_json::Map::new();
                        m.insert("output".to_string(), o.clone());
                        Value::Object(m)
                    })
                    .collect();
                for out in outputs {
                    if !eval_scalar(pred, &Py::Json(out))? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::DependsOn { step_id_a, step_id_b } => {
                for st in steps {
                    if st.get("id").and_then(|x| x.as_str()) == Some(step_id_a.as_str()) {
                        let deps = st.get("depends_on").and_then(|x| x.as_array());
                        return Ok(deps.is_some_and(|d| d.iter().any(|x| x.as_str() == Some(step_id_b.as_str()))));
                    }
                }
                Ok(false)
            }
            Expr::Before { step_id_a, step_id_b } => {
                let ids: Vec<Option<&str>> = steps.iter().map(|st| st.get("id").and_then(|x| x.as_str())).collect();
                let ia = ids.iter().position(|x| *x == Some(step_id_a.as_str()));
                let ib = ids.iter().position(|x| *x == Some(step_id_b.as_str()));
                match (ia, ib) {
                    (Some(a), Some(b)) => Ok(a < b),
                    _ => Ok(false),
                }
            }
            Expr::LLMCheck { .. } => {
                if stakes == "physical" {
                    return Err("llm_check blocked for physical stakes — use sound primitives only".into());
                }
                Err("llm_check is not reproducible offline (network/model call)".into())
            }
            Expr::And { children } => {
                for c in children {
                    if !go(c, steps, dumped, stakes)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::Or { children } => {
                for c in children {
                    if go(c, steps, dumped, stakes)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            Expr::Not { child } => Ok(!go(child, steps, dumped, stakes)?),
            Expr::Implies { a, b } => Ok(!go(a, steps, dumped, stakes)? || go(b, steps, dumped, stakes)?),
            other => {
                // Scalar at plan root: evaluate against a synthetic {"steps": [...]} scope.
                let scope = Py::Obj(vec![("steps".to_string(), Py::List(dumped.to_vec()))]);
                eval_scalar(other, &scope)
            }
        }
    }
}

#[cfg(test)]
mod tuple_tests {
    use super::*;

    /// The offline verifier reads a step as its model_dump(): a
    /// cartesian_move's target_position is a tuple, which equals no list.
    #[test]
    fn a_tuple_field_equals_no_list() {
        let steps: Vec<Value> = vec![serde_json::json!({"id": "c1", "type": "cartesian_move", "target_position": [1.0, 2.0, 3.0], "target_orientation": null})];
        let run = |e: Value| evaluate_predicate(&parse_expression(&e).unwrap(), &steps, "low").unwrap();
        let fs = |pred: Value| serde_json::json!({"op": "forall_steps", "pred": pred});
        assert!(!run(fs(serde_json::json!({"op": "equals", "path": "target_position", "value": [1.0, 2.0, 3.0]}))));
        assert!(run(fs(serde_json::json!({"op": "not_equals", "path": "target_position", "value": [1.0, 2.0, 3.0]}))));
        assert!(!run(fs(serde_json::json!({"op": "in_set", "path": "target_position", "values": [[1.0, 2.0, 3.0]]}))));
        assert!(run(fs(serde_json::json!({"op": "not_in_set", "path": "target_position", "values": [[1.0, 2.0, 3.0]]}))));
        assert!(run(fs(serde_json::json!({"op": "length_range", "path": "target_position", "min": 3, "max": 3}))));
        assert!(run(fs(serde_json::json!({"op": "is_empty", "path": "target_orientation"}))));
        assert!(run(fs(serde_json::json!({"op": "exists", "path": "target_position"}))));
        let whole = serde_json::json!({"op": "equals", "path": "steps", "value": [
            {"id": "c1", "type": "cartesian_move", "target_position": [1.0, 2.0, 3.0], "target_orientation": null}]});
        assert!(!run(whole));
        // Another step type keeps its list.
        let shell = vec![serde_json::json!({"id": "s", "type": "shell", "command": "ls", "target_position": [1]})];
        let e = fs(serde_json::json!({"op": "equals", "path": "target_position", "value": [1]}));
        assert!(evaluate_predicate(&parse_expression(&e).unwrap(), &shell, "low").unwrap());
    }
}
