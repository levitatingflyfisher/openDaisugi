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

fn py_in(val: &Value, values: &[Value]) -> bool {
    values.iter().any(|v| py_eq(val, v))
}

/// `_resolve_path` — dict-only path walk (the pure-Python evaluation path
/// never resolves against a raw pydantic object; every scope handed to
/// `_eval_scalar` is already a dict). Returns `None` for "MISSING".
fn resolve_path<'a>(scope: &'a Value, path: &str) -> Option<&'a Value> {
    let mut cur = scope;
    for part in path.split('.') {
        cur = cur.as_object()?.get(part)?;
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
pub fn eval_scalar(expr: &Expr, scope: &Value) -> Result<bool, String> {
    match expr {
        Expr::Equals { path, value } => Ok(resolve_path(scope, path).is_some_and(|v| py_eq(v, value))),
        Expr::NotEquals { path, value } => Ok(resolve_path(scope, path).is_some_and(|v| !py_eq(v, value))),
        Expr::InSet { path, values } => Ok(resolve_path(scope, path).is_some_and(|v| py_in(v, values))),
        Expr::NotInSet { path, values } => Ok(resolve_path(scope, path).is_some_and(|v| !py_in(v, values))),
        Expr::Matches { path, regex } => {
            let re = compile_regex(regex)?;
            Ok(match resolve_path(scope, path) {
                Some(Value::String(s)) => re.is_match(s),
                _ => false,
            })
        }
        Expr::NotMatches { path, regex } => {
            let re = compile_regex(regex)?;
            Ok(match resolve_path(scope, path) {
                Some(Value::String(s)) => !re.is_match(s),
                Some(_) => true,
                None => true,
            })
        }
        Expr::NumericRange { path, min, max } => Ok(match resolve_path(scope, path) {
            Some(v) if is_numericish(v) => {
                let n = numeric_of(v);
                *min <= n && n <= *max
            }
            _ => false,
        }),
        Expr::LengthRange { path, min, max } => Ok(match resolve_path(scope, path).and_then(has_len) {
            Some(n) => {
                let n = n as i64;
                n >= *min && max.is_none_or(|m| n <= m)
            }
            None => false,
        }),
        Expr::Exists { path } => Ok(resolve_path(scope, path).is_some()),
        Expr::IsEmpty { path } => Ok(match resolve_path(scope, path) {
            None | Some(Value::Null) => true,
            Some(v) => has_len(v).map(|n| n == 0).unwrap_or(false),
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
    fn go(e: &Expr, steps: &[Value], stakes: &str) -> Result<bool, String> {
        match e {
            Expr::ForallSteps { pred } => {
                for st in steps {
                    if !eval_scalar(pred, st)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::ExistsStep { pred } => {
                for st in steps {
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
                for out in &outputs {
                    if !eval_scalar(pred, out)? {
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
                    if !go(c, steps, stakes)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::Or { children } => {
                for c in children {
                    if go(c, steps, stakes)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            Expr::Not { child } => Ok(!go(child, steps, stakes)?),
            Expr::Implies { a, b } => Ok(!go(a, steps, stakes)? || go(b, steps, stakes)?),
            other => {
                // Scalar at plan root: evaluate against a synthetic {"steps": [...]} scope.
                let mut m = serde_json::Map::new();
                m.insert("steps".to_string(), Value::Array(steps.to_vec()));
                eval_scalar(other, &Value::Object(m))
            }
        }
    }
    go(expr, steps, stakes)
}
