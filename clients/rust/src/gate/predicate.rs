//! The verifier's predicate stage for the gate's one-step plan:
//! `predicate.parse_expression` (pydantic's tagged union, Python mode),
//! `vacuity.check_vacuity` on the linked Z3 with `regex_to_z3`, and
//! `predicate_z3.evaluate_predicate` with Python's equality, `re` and
//! attribute lookup.

use super::check::Violation;
use super::envelope::{Envelope, Item};
use super::py::re::{self as pyre, At, Cat, Node, RepKind, SetItem, MAXREPEAT};
use super::py::text::cp_char;
use super::pydantic::validation_error;
use super::pymodel::{at, In, Loc, Val};
use super::pyjson::{py_float_repr, py_str, Object, Value};
use super::{catch, undecided, Fault, PyErr, R};
use crate::z3py::{with_main, Check, Ctx, Sort, Term, Z3Error};
use std::collections::HashMap;

/// A validated predicate expression.
#[derive(Debug, Clone)]
pub enum Expr {
    Equals { path: String, value: Value },
    NotEquals { path: String, value: Value },
    InSet { path: String, values: Vec<Value> },
    NotInSet { path: String, values: Vec<Value> },
    Matches { path: String, regex: String },
    NotMatches { path: String, regex: String },
    NumericRange { path: String, min: f64, max: f64 },
    LengthRange { path: String, min: String, max: Option<String> },
    Exists { path: String },
    IsEmpty { path: String },
    And(Vec<Expr>),
    Or(Vec<Expr>),
    Not(Box<Expr>),
    Implies(Box<Expr>, Box<Expr>),
    ForallSteps(Box<Expr>),
    ExistsStep(Box<Expr>),
    ForallOutputs(Box<Expr>),
    DependsOn { a: String, b: String },
    Before { a: String, b: String },
    Alias { name: String, args: Value },
    LlmCheck { rule: String },
}

impl Expr {
    /// The `op` literal.
    pub fn op(&self) -> &'static str {
        match self {
            Expr::Equals { .. } => "equals",
            Expr::NotEquals { .. } => "not_equals",
            Expr::InSet { .. } => "in_set",
            Expr::NotInSet { .. } => "not_in_set",
            Expr::Matches { .. } => "matches",
            Expr::NotMatches { .. } => "not_matches",
            Expr::NumericRange { .. } => "numeric_range",
            Expr::LengthRange { .. } => "length_range",
            Expr::Exists { .. } => "exists",
            Expr::IsEmpty { .. } => "is_empty",
            Expr::And(_) => "and",
            Expr::Or(_) => "or",
            Expr::Not(_) => "not",
            Expr::Implies(..) => "implies",
            Expr::ForallSteps(_) => "forall_steps",
            Expr::ExistsStep(_) => "exists_step",
            Expr::ForallOutputs(_) => "forall_outputs",
            Expr::DependsOn { .. } => "depends_on",
            Expr::Before { .. } => "before",
            Expr::Alias { .. } => "alias",
            Expr::LlmCheck { .. } => "llm_check",
        }
    }
}

const TAGS: &[&str] = &[
    "equals",
    "not_equals",
    "in_set",
    "not_in_set",
    "matches",
    "not_matches",
    "numeric_range",
    "length_range",
    "exists",
    "is_empty",
    "and",
    "or",
    "not",
    "implies",
    "forall_steps",
    "exists_step",
    "forall_outputs",
    "depends_on",
    "before",
    "alias",
    "llm_check",
];

const UNION_TITLE: &str = "tagged-union[Equals,NotEquals,InSet,NotInSet,Matches,NotMatches,NumericRange,\
LengthRange,Exists,IsEmpty,And,Or,Not,Implies,ForallSteps,ExistsStep,ForallOutputs,DependsOn,Before,AliasRef,LLMCheck]";

/// `predicate.parse_expression(data)` of a dict: the expression, or the
/// ValidationError pydantic raises.
pub fn parse_expression(data: &Value) -> R<Expr> {
    let mut v = Val::new(false);
    let x = In::from_value(data);
    let e = union(&mut v, &vec![], &x)?;
    match e {
        Some(e) if v.errs.is_empty() => Ok(e),
        _ => Err(validation_error(UNION_TITLE, &v.errs).into()),
    }
}

fn req<T>(
    v: &mut Val,
    loc: &Loc,
    model: &In,
    name: &str,
    f: impl FnOnce(&mut Val, &Loc, &In) -> R<Option<T>>,
) -> R<Option<T>> {
    let l = at(loc, name);
    match model.get(name) {
        Some(x) => f(v, &l, x),
        None => {
            v.missing(&l, model)?;
            Ok(None)
        }
    }
}

fn opt<T>(
    v: &mut Val,
    loc: &Loc,
    model: &In,
    name: &str,
    default: T,
    f: impl FnOnce(&mut Val, &Loc, &In) -> R<Option<T>>,
) -> R<Option<T>> {
    match model.get(name) {
        Some(x) => f(v, &at(loc, name), x),
        None => Ok(Some(default)),
    }
}

fn s(v: &mut Val, l: &Loc, x: &In) -> R<Option<String>> {
    v.string(l, x)
}

fn any(_: &mut Val, _: &Loc, x: &In) -> R<Option<Value>> {
    Ok(Some(x.to_value()))
}

fn child(v: &mut Val, l: &Loc, x: &In) -> R<Option<Box<Expr>>> {
    Ok(union(v, l, x)?.map(Box::new))
}

fn children(v: &mut Val, l: &Loc, x: &In) -> R<Option<Vec<Expr>>> {
    v.list(l, x, union)
}

/// The discriminated union on `op`.
fn union(v: &mut Val, loc: &Loc, x: &In) -> R<Option<Expr>> {
    if !matches!(x, In::Dict(_)) {
        v.err(loc, "Input should be a valid dictionary or object to extract fields from", "model_attributes_type", x)?;
        return Ok(None);
    }
    let tag = match x.get("op") {
        None => {
            v.err(loc, "Unable to extract tag using discriminator 'op'", "union_tag_not_found", x)?;
            return Ok(None);
        }
        Some(t) => t,
    };
    let tag = match tag {
        In::Str(t) if TAGS.contains(&t.as_str()) => t.clone(),
        other => {
            let shown = py_str(&other.to_value())?;
            let expected: Vec<String> = TAGS.iter().map(|t| format!("'{t}'")).collect();
            v.err(
                loc,
                format!(
                    "Input tag '{shown}' found using 'op' does not match any of the expected tags: {}",
                    expected.join(", ")
                ),
                "union_tag_invalid",
                x,
            )?;
            return Ok(None);
        }
    };
    let l = at(loc, tag.clone());
    let l = &l;
    Ok(match tag.as_str() {
        "equals" | "not_equals" => {
            let path = req(v, l, x, "path", s)?;
            let value = req(v, l, x, "value", any)?;
            match (path, value) {
                (Some(path), Some(value)) if tag == "equals" => Some(Expr::Equals { path, value }),
                (Some(path), Some(value)) => Some(Expr::NotEquals { path, value }),
                _ => None,
            }
        }
        "in_set" | "not_in_set" => {
            let path = req(v, l, x, "path", s)?;
            let values = req(v, l, x, "values", |v, l, e| v.list(l, e, any))?;
            match (path, values) {
                (Some(path), Some(values)) if tag == "in_set" => Some(Expr::InSet { path, values }),
                (Some(path), Some(values)) => Some(Expr::NotInSet { path, values }),
                _ => None,
            }
        }
        "matches" | "not_matches" => {
            let path = req(v, l, x, "path", s)?;
            let regex = req(v, l, x, "regex", s)?;
            match (path, regex) {
                (Some(path), Some(regex)) if tag == "matches" => Some(Expr::Matches { path, regex }),
                (Some(path), Some(regex)) => Some(Expr::NotMatches { path, regex }),
                _ => None,
            }
        }
        "numeric_range" => {
            let path = req(v, l, x, "path", s)?;
            let min = req(v, l, x, "min", |v, l, e| v.float(l, e))?;
            let max = req(v, l, x, "max", |v, l, e| v.float(l, e))?;
            match (path, min, max) {
                (Some(path), Some(min), Some(max)) => Some(Expr::NumericRange { path, min, max }),
                _ => None,
            }
        }
        "length_range" => {
            let path = req(v, l, x, "path", s)?;
            let min = opt(v, l, x, "min", "0".to_string(), |v, l, e| v.int(l, e))?;
            let max = opt(v, l, x, "max", None, |v, l, e| v.nullable(l, e, |v, l, e| v.int(l, e)))?;
            match (path, min, max) {
                (Some(path), Some(min), Some(max)) => Some(Expr::LengthRange { path, min, max }),
                _ => None,
            }
        }
        "exists" => req(v, l, x, "path", s)?.map(|path| Expr::Exists { path }),
        "is_empty" => req(v, l, x, "path", s)?.map(|path| Expr::IsEmpty { path }),
        "and" => req(v, l, x, "children", children)?.map(Expr::And),
        "or" => req(v, l, x, "children", children)?.map(Expr::Or),
        "not" => req(v, l, x, "child", child)?.map(Expr::Not),
        "implies" => {
            let a = req(v, l, x, "a", child)?;
            let b = req(v, l, x, "b", child)?;
            match (a, b) {
                (Some(a), Some(b)) => Some(Expr::Implies(a, b)),
                _ => None,
            }
        }
        "forall_steps" => req(v, l, x, "pred", child)?.map(Expr::ForallSteps),
        "exists_step" => req(v, l, x, "pred", child)?.map(Expr::ExistsStep),
        "forall_outputs" => req(v, l, x, "pred", child)?.map(Expr::ForallOutputs),
        "depends_on" | "before" => {
            let a = req(v, l, x, "step_id_a", s)?;
            let b = req(v, l, x, "step_id_b", s)?;
            match (a, b) {
                (Some(a), Some(b)) if tag == "depends_on" => Some(Expr::DependsOn { a, b }),
                (Some(a), Some(b)) => Some(Expr::Before { a, b }),
                _ => None,
            }
        }
        "alias" => {
            let name = req(v, l, x, "name", s)?;
            let args = opt(v, l, x, "args", Value::Obj(Object::new()), |v, l, e| {
                Ok(v.dict(l, e, any)?.map(|pairs| {
                    let mut o = Object::new();
                    for (k, val) in pairs {
                        o.set(&k, val);
                    }
                    Value::Obj(o)
                }))
            })?;
            match (name, args) {
                (Some(name), Some(args)) => Some(Expr::Alias { name, args }),
                _ => None,
            }
        }
        "llm_check" => req(v, l, x, "rule", s)?.map(|rule| Expr::LlmCheck { rule }),
        _ => None,
    })
}

// ---------------------------------------------------------------------
// The predicate item pipeline (verify._check_predicate_item)
// ---------------------------------------------------------------------

const RECOGNIZED_OPAQUE_TYPES: &[&str] =
    &["end_effector_in_workspace", "joint_limits_respected", "velocity_bounded", "no_obstacle_penetration"];
const RECOGNIZED_STAGE2_POSTCONDITION_TYPES: &[&str] = &["exit_code", "file_exists", "file_size_range"];

/// An expression after `_normalize_expr`: validated, or a raw non-dict
/// value that no stage can read.
enum Norm {
    Expr(Expr),
    Raw(Value),
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Verdict {
    Tautology,
    Contradiction,
    NonTrivial,
}

/// The predicate stage's state for one verify call: the vacuity cache.
pub struct Stage<'a> {
    /// None outside the gate: an llm_check there is not decided, since the
    /// binary asks a model only for a gate call.
    runner: Option<&'a super::Runner>,
    env: &'a Envelope,
    steps: Vec<Value>,
    strict: bool,
    cache: HashMap<String, Verdict>,
}

fn violation(message: String, detail: Object) -> Violation {
    Violation { stage: "predicate".into(), message, detail: Some(detail), remediation: None }
}

impl<'a> Stage<'a> {
    pub fn new(runner: &'a super::Runner, env: &'a Envelope, step: Value, strict: bool) -> Self {
        Stage { runner: Some(runner), env, steps: vec![step], strict, cache: HashMap::new() }
    }

    /// The stage for a whole plan (each step as its `model_dump()`), with
    /// no model to ask.
    pub fn for_plan(env: &'a Envelope, steps: Vec<Value>, strict: bool) -> Self {
        Stage { runner: None, env, steps, strict, cache: HashMap::new() }
    }

    /// `verify._check_predicate_invariants`.
    pub fn run(&mut self) -> R<Vec<Violation>> {
        let mut vs = Vec::new();
        let env = self.env;
        for inv in &env.invariants {
            vs.extend(self.item("invariant", inv)?);
        }
        for pc in &env.postconditions {
            vs.extend(self.item("postcondition", pc)?);
        }
        Ok(vs)
    }

    fn item(&mut self, label: &str, it: &Item) -> R<Vec<Violation>> {
        if !it.enforce {
            return Ok(vec![]);
        }
        let t = it.typ.as_str();
        let norm = match &it.expr {
            None => None,
            Some(v @ Value::Obj(_)) => Some(Norm::Expr(parse_expression(v)?)),
            Some(v) => Some(Norm::Raw(v.clone())),
        };
        let norm = match norm {
            Some(n) => n,
            None => {
                if label == "invariant" {
                    let missing = if t == "end_effector_in_workspace" && !self.env.workspace_bounds {
                        Some("workspace_bounds")
                    } else if t == "velocity_bounded" && !self.env.velocity_limit {
                        Some("velocity_limit")
                    } else {
                        None
                    };
                    if let Some(m) = missing {
                        return Ok(vec![violation(
                            format!(
                                "invariant '{t}' is declared but its backing permission ({m}) is absent; \
                                 the check no-ops and the invariant is unenforced \u{2014} add the bound or remove the invariant"
                            ),
                            Object::new().with(label, t).with("reason", "robotics_invariant_unbacked"),
                        )]);
                    }
                }
                let discharged = (label == "invariant" && RECOGNIZED_OPAQUE_TYPES.contains(&t))
                    || (label == "postcondition" && RECOGNIZED_STAGE2_POSTCONDITION_TYPES.contains(&t));
                if self.strict && !discharged {
                    return Ok(vec![violation(
                        format!(
                            "{label} '{t}' declares a safety property with no verifiable expr; \
                             cannot be discharged under strict mode"
                        ),
                        Object::new().with(label, t).with("reason", "opaque_unrecognized").with(
                            "suggested_remediation",
                            "add an `expr` to make it verifiable, or set enforce=False to keep it as documentation",
                        ),
                    )]);
                }
                return Ok(vec![]);
            }
        };
        if let Norm::Expr(Expr::Alias { name, .. }) = &norm {
            return Ok(vec![violation(
                format!("{label} '{t}' references unresolved alias '{name}'; pass an AliasRegistry via aliases= to verify()"),
                Object::new()
                    .with(label, t)
                    .with("reason", "unresolved_alias")
                    .with("alias", name.as_str())
                    .with(
                        "suggested_remediation",
                        "register the alias in an AliasRegistry and pass aliases= to verify()",
                    ),
            )]);
        }
        let verdict = match &norm {
            Norm::Expr(e) => self.vacuity(e)?,
            // A raw value has no model_dump_json and no compiled form.
            Norm::Raw(_) => Verdict::NonTrivial,
        };
        if verdict == Verdict::Contradiction {
            return Ok(vec![violation(
                format!(
                    "{label} '{t}' can never be satisfied (unsatisfiable); the envelope can never pass \u{2014} fix the predicate"
                ),
                Object::new().with(label, t).with("reason", "contradiction").with(
                    "suggested_remediation",
                    format!("this {label} is unsatisfiable (always false); the envelope can never pass \u{2014} fix the predicate"),
                ),
            )]);
        }
        if verdict == Verdict::Tautology && self.strict {
            return Ok(vec![violation(
                format!("{label} '{t}' is a tautology (constrains nothing); tighten the predicate or remove it"),
                Object::new().with(label, t).with("reason", "tautology").with(
                    "suggested_remediation",
                    format!("this {label} constrains nothing; tighten the predicate or remove it"),
                ),
            )]);
        }
        let ok = match &norm {
            Norm::Expr(e) => catch(self.evaluate(e))?,
            Norm::Raw(v) => Err(PyErr::value(format!("unknown predicate op: '{}'", super::pyjson::py_type_name(v)))),
        };
        match ok {
            Err(e) => Ok(vec![violation(
                format!("{label} '{t}' evaluation error: {}", e.msg),
                Object::new().with(label, t),
            )]),
            Ok(false) => Ok(vec![violation(
                format!("{label} '{t}' violated"),
                Object::new().with(label, t).with("description", it.description.clone()),
            )]),
            Ok(true) => Ok(vec![]),
        }
    }

    // -----------------------------------------------------------------
    // evaluate_predicate
    // -----------------------------------------------------------------

    fn evaluate(&self, e: &Expr) -> R<bool> {
        match e {
            Expr::ForallSteps(p) => {
                for s in &self.steps {
                    if !eval_scalar(p, s)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::ExistsStep(p) => {
                for s in &self.steps {
                    if eval_scalar(p, s)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            Expr::ForallOutputs(p) => {
                for s in &self.steps {
                    let out = match s.as_obj().and_then(|o| o.get("metadata")) {
                        Some(Value::Obj(m)) => m.value("output").clone(),
                        _ => Value::Null,
                    };
                    if out.is_null() {
                        continue;
                    }
                    let scope = Value::Obj(Object::new().with("output", out));
                    if !eval_scalar(p, &scope)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::DependsOn { a, b } => {
                for s in &self.steps {
                    let o = s.as_obj();
                    if o.map(|o| o.value("id")) == Some(&Value::Str(a.clone())) {
                        let deps = o.map(|o| o.value("depends_on").clone()).unwrap_or(Value::Null);
                        return Ok(match deps {
                            Value::List(l) => l.iter().any(|d| py_eq(d, &Value::Str(b.clone()))),
                            _ => false,
                        });
                    }
                }
                Ok(false)
            }
            Expr::Before { a, b } => {
                let ids: Vec<Value> =
                    self.steps.iter().map(|s| s.as_obj().map(|o| o.value("id").clone()).unwrap_or(Value::Null)).collect();
                let pa = ids.iter().position(|i| py_eq(i, &Value::Str(a.clone())));
                let pb = ids.iter().position(|i| py_eq(i, &Value::Str(b.clone())));
                Ok(match (pa, pb) {
                    (Some(x), Some(y)) => x < y,
                    _ => false,
                })
            }
            Expr::LlmCheck { rule } => {
                if self.env.stakes == "physical" {
                    return Err(PyErr::value("llm_check blocked for physical stakes \u{2014} use sound primitives only").into());
                }
                let payload = Object::new()
                    .with("task", self.env.task.as_str())
                    .with("steps", Value::List(self.steps.clone()));
                let res = match self.runner {
                    Some(r) => r.run_llm_check(rule, &payload)?,
                    None => return undecided("an llm_check predicate, which import would ask a model about"),
                };
                // Fail closed: a failed call raises, and the item is a
                // violation.
                if res.errored {
                    return Err(PyErr::value(res.reason).into());
                }
                Ok(res.satisfied)
            }
            Expr::And(cs) => {
                for c in cs {
                    if !self.evaluate(c)? {
                        return Ok(false);
                    }
                }
                Ok(true)
            }
            Expr::Or(cs) => {
                for c in cs {
                    if self.evaluate(c)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            Expr::Not(c) => Ok(!self.evaluate(c)?),
            Expr::Implies(a, b) => Ok(!self.evaluate(a)? || self.evaluate(b)?),
            other => {
                let scope = Value::Obj(Object::new().with("steps", Value::List(self.steps.clone())));
                eval_scalar(other, &scope)
            }
        }
    }

    // -----------------------------------------------------------------
    // vacuity.check_vacuity
    // -----------------------------------------------------------------

    fn vacuity(&mut self, e: &Expr) -> R<Verdict> {
        let key = cache_key(e);
        if let Some(v) = self.cache.get(&key) {
            return Ok(*v);
        }
        // Any exception in the check is "non_trivial".
        let v = catch(compute_vacuity(e))?.unwrap_or(Verdict::NonTrivial);
        self.cache.insert(key, v);
        Ok(v)
    }
}

/// A vacuity verdict, for the conformance client.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Vac {
    Tautology,
    Contradiction,
    NonTrivial,
}

/// `check_vacuity(parse_expression(raw))` with `_check_predicate_item`'s
/// fallback: any exception is non_trivial. A ValidationError is returned
/// as an error, as is a pattern the port does not decide.
pub fn vacuity_of(raw: &Value) -> Result<Vac, String> {
    let e = match parse_expression(raw) {
        Ok(e) => e,
        Err(Fault::Raised(e)) => return Err(e.msg),
        Err(Fault::Undecided(w)) => return Err(w),
    };
    match catch(compute_vacuity(&e)) {
        Ok(Ok(Verdict::Contradiction)) => Ok(Vac::Contradiction),
        Ok(Ok(Verdict::Tautology)) => Ok(Vac::Tautology),
        Ok(_) => Ok(Vac::NonTrivial),
        Err(Fault::Undecided(w)) => Err(w),
        Err(Fault::Raised(e)) => Err(e.msg),
    }
}

/// `expr.model_dump_json()` up to what makes two keys equal: pydantic
/// writes NaN and the infinities as null.
fn cache_key(e: &Expr) -> String {
    fn val(v: &Value, out: &mut String) {
        match v {
            Value::Null => out.push_str("null"),
            Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            Value::Int(t) => {
                out.push('i');
                out.push_str(t)
            }
            Value::Float(f) => flt(*f, out),
            Value::Str(s) => {
                out.push('s');
                out.push_str(&s.len().to_string());
                out.push(':');
                out.push_str(s);
            }
            Value::List(l) | Value::Tuple(l) => {
                out.push('[');
                for x in l {
                    val(x, out);
                    out.push(',');
                }
                out.push(']');
            }
            Value::Obj(o) => {
                out.push('{');
                for k in o.keys() {
                    out.push_str(&k.len().to_string());
                    out.push(':');
                    out.push_str(k);
                    val(o.value(k), out);
                    out.push(',');
                }
                out.push('}');
            }
        }
    }
    fn flt(f: f64, out: &mut String) {
        if f.is_finite() {
            out.push('f');
            out.push_str(&py_float_repr(f));
        } else {
            out.push_str("null");
        }
    }
    fn st(s: &str, out: &mut String) {
        val(&Value::Str(s.to_string()), out)
    }
    fn go(e: &Expr, out: &mut String) {
        out.push('(');
        out.push_str(e.op());
        out.push(' ');
        match e {
            Expr::Equals { path, value } | Expr::NotEquals { path, value } => {
                st(path, out);
                val(value, out);
            }
            Expr::InSet { path, values } | Expr::NotInSet { path, values } => {
                st(path, out);
                val(&Value::List(values.clone()), out);
            }
            Expr::Matches { path, regex } | Expr::NotMatches { path, regex } => {
                st(path, out);
                st(regex, out);
            }
            Expr::NumericRange { path, min, max } => {
                st(path, out);
                flt(*min, out);
                flt(*max, out);
            }
            Expr::LengthRange { path, min, max } => {
                st(path, out);
                out.push_str(min);
                out.push(',');
                out.push_str(max.as_deref().unwrap_or("null"));
            }
            Expr::Exists { path } | Expr::IsEmpty { path } => st(path, out),
            Expr::And(cs) | Expr::Or(cs) => {
                for c in cs {
                    go(c, out);
                }
            }
            Expr::Not(c) | Expr::ForallSteps(c) | Expr::ExistsStep(c) | Expr::ForallOutputs(c) => go(c, out),
            Expr::Implies(a, b) => {
                go(a, out);
                go(b, out);
            }
            Expr::DependsOn { a, b } | Expr::Before { a, b } => {
                st(a, out);
                st(b, out);
            }
            Expr::Alias { name, args } => {
                st(name, out);
                val(args, out);
            }
            Expr::LlmCheck { rule } => st(rule, out),
        }
        out.push(')');
    }
    let mut out = String::new();
    go(e, &mut out);
    out
}

// ---------------------------------------------------------------------
// Python values: paths, equality, len
// ---------------------------------------------------------------------

/// What `_resolve_path` returns.
enum Got {
    Missing,
    Val(Value),
    /// A bound method of a builtin: equal to no JSON value, no `__len__`,
    /// not a str, not a number.
    Method,
}

const STR_METHODS: &str = "__add__ __contains__ __delattr__ __dir__ __eq__ __format__ __ge__ __getattribute__ __getitem__ __getnewargs__ __getstate__ __gt__ __hash__ __init__ __init_subclass__ __iter__ __le__ __len__ __lt__ __mod__ __mul__ __ne__ __new__ __reduce__ __reduce_ex__ __repr__ __rmod__ __rmul__ __setattr__ __sizeof__ __str__ __subclasshook__ capitalize casefold center count encode endswith expandtabs find format format_map index isalnum isalpha isascii isdecimal isdigit isidentifier islower isnumeric isprintable isspace istitle isupper join ljust lower lstrip maketrans partition removeprefix removesuffix replace rfind rindex rjust rpartition rsplit rstrip split splitlines startswith strip swapcase title translate upper zfill";
const INT_METHODS: &str = "__abs__ __add__ __and__ __bool__ __ceil__ __delattr__ __dir__ __divmod__ __eq__ __float__ __floor__ __floordiv__ __format__ __ge__ __getattribute__ __getnewargs__ __getstate__ __gt__ __hash__ __index__ __init__ __init_subclass__ __int__ __invert__ __le__ __lshift__ __lt__ __mod__ __mul__ __ne__ __neg__ __new__ __or__ __pos__ __pow__ __radd__ __rand__ __rdivmod__ __reduce__ __reduce_ex__ __repr__ __rfloordiv__ __rlshift__ __rmod__ __rmul__ __ror__ __round__ __rpow__ __rrshift__ __rshift__ __rsub__ __rtruediv__ __rxor__ __setattr__ __sizeof__ __str__ __sub__ __subclasshook__ __truediv__ __trunc__ __xor__ as_integer_ratio bit_count bit_length conjugate from_bytes is_integer to_bytes";
const FLOAT_METHODS: &str = "__abs__ __add__ __bool__ __ceil__ __delattr__ __dir__ __divmod__ __eq__ __float__ __floor__ __floordiv__ __format__ __ge__ __getattribute__ __getformat__ __getnewargs__ __getstate__ __gt__ __hash__ __init__ __init_subclass__ __int__ __le__ __lt__ __mod__ __mul__ __ne__ __neg__ __new__ __pos__ __pow__ __radd__ __rdivmod__ __reduce__ __reduce_ex__ __repr__ __rfloordiv__ __rmod__ __rmul__ __round__ __rpow__ __rsub__ __rtruediv__ __setattr__ __sizeof__ __str__ __sub__ __subclasshook__ __truediv__ __trunc__ as_integer_ratio conjugate fromhex hex is_integer";
const NONE_METHODS: &str = "__bool__ __delattr__ __dir__ __eq__ __format__ __ge__ __getattribute__ __getstate__ __gt__ __hash__ __init__ __init_subclass__ __le__ __lt__ __ne__ __new__ __reduce__ __reduce_ex__ __repr__ __setattr__ __sizeof__ __str__ __subclasshook__";
const TUPLE_METHODS: &str = "__add__ __class_getitem__ __contains__ __delattr__ __dir__ __eq__ __format__ __ge__ __getattribute__ __getitem__ __getnewargs__ __getstate__ __gt__ __hash__ __init__ __init_subclass__ __iter__ __le__ __len__ __lt__ __mul__ __ne__ __new__ __reduce__ __reduce_ex__ __repr__ __rmul__ __setattr__ __sizeof__ __str__ __subclasshook__ count index";
const LIST_METHODS: &str = "__add__ __class_getitem__ __contains__ __delattr__ __delitem__ __dir__ __eq__ __format__ __ge__ __getattribute__ __getitem__ __getstate__ __gt__ __iadd__ __imul__ __init__ __init_subclass__ __iter__ __le__ __len__ __lt__ __mul__ __ne__ __new__ __reduce__ __reduce_ex__ __repr__ __reversed__ __rmul__ __setattr__ __setitem__ __sizeof__ __str__ __subclasshook__ append clear copy count extend index insert pop remove reverse sort";

fn has_word(table: &str, name: &str) -> bool {
    table.split(' ').any(|w| w == name)
}

/// `getattr(v, part, _MISSING)` for a value that is not a dict.
fn getattr(v: &Value, part: &str) -> R<Got> {
    let methods = match v {
        Value::Str(_) => STR_METHODS,
        Value::Int(_) | Value::Bool(_) => INT_METHODS,
        Value::Float(_) => FLOAT_METHODS,
        Value::Null => NONE_METHODS,
        Value::List(_) => LIST_METHODS,
        Value::Tuple(_) => TUPLE_METHODS,
        Value::Obj(_) => unreachable_dict(),
    };
    if has_word(methods, part) {
        return Ok(Got::Method);
    }
    match (v, part) {
        (Value::Int(t), "real" | "numerator") => Ok(Got::Val(Value::Int(t.clone()))),
        (Value::Bool(b), "real" | "numerator") => Ok(Got::Val(Value::Int(if *b { "1" } else { "0" }.into()))),
        (Value::Int(_) | Value::Bool(_), "imag") => Ok(Got::Val(Value::Int("0".into()))),
        (Value::Int(_) | Value::Bool(_), "denominator") => Ok(Got::Val(Value::Int("1".into()))),
        (Value::Float(f), "real") => Ok(Got::Val(Value::Float(*f))),
        (Value::Float(_), "imag") => Ok(Got::Val(Value::Float(0.0))),
        (Value::Null, "__doc__") | (Value::List(_), "__hash__") => Ok(Got::Val(Value::Null)),
        (_, "__class__" | "__doc__") => undecided("a predicate path reads a Python type or docstring"),
        _ => Ok(Got::Missing),
    }
}

fn unreachable_dict() -> &'static str {
    ""
}

/// `predicate_z3._resolve_path(obj, path)`.
fn resolve_path(obj: &Value, path: &str) -> R<Got> {
    let mut cur = Got::Val(obj.clone());
    for part in path.split('.') {
        cur = match cur {
            Got::Missing => return Ok(Got::Missing),
            Got::Method => return undecided("a predicate path reads an attribute of a method"),
            Got::Val(Value::Obj(o)) => match o.get(part) {
                None => return Ok(Got::Missing),
                Some(v) => Got::Val(v.clone()),
            },
            Got::Val(v) => match getattr(&v, part)? {
                Got::Missing => return Ok(Got::Missing),
                g => g,
            },
        };
    }
    Ok(cur)
}

/// An int's decimal text against a float, exactly, as Python compares.
fn int_eq_float(t: &str, f: f64) -> bool {
    if !f.is_finite() || f.fract() != 0.0 {
        return false;
    }
    let ft = format!("{f:.0}");
    let norm = |s: &str| -> String {
        let (neg, d) = match s.strip_prefix('-') {
            Some(d) => (true, d),
            None => (false, s),
        };
        let d = d.trim_start_matches('0');
        if d.is_empty() {
            "0".into()
        } else if neg {
            format!("-{d}")
        } else {
            d.to_string()
        }
    };
    norm(t) == norm(&ft)
}

fn num_of(v: &Value) -> Option<Result<String, f64>> {
    match v {
        Value::Bool(b) => Some(Ok(if *b { "1" } else { "0" }.into())),
        Value::Int(t) => Some(Ok(t.clone())),
        Value::Float(f) => Some(Err(*f)),
        _ => None,
    }
}

/// Python's `a == b` for values JSON gave.
pub fn py_eq(a: &Value, b: &Value) -> bool {
    if let (Some(x), Some(y)) = (num_of(a), num_of(b)) {
        return match (x, y) {
            (Ok(s), Ok(t)) => s == t,
            (Ok(s), Err(f)) | (Err(f), Ok(s)) => int_eq_float(&s, f),
            (Err(f), Err(g)) => f == g,
        };
    }
    match (a, b) {
        (Value::Null, Value::Null) => true,
        (Value::Str(x), Value::Str(y)) => x == y,
        (Value::List(x), Value::List(y)) | (Value::Tuple(x), Value::Tuple(y)) => {
            x.len() == y.len() && x.iter().zip(y).all(|(p, q)| py_eq(p, q))
        }
        (Value::Obj(x), Value::Obj(y)) => {
            x.len() == y.len() && x.keys().iter().all(|k| y.get(k).is_some_and(|w| py_eq(x.value(k), w)))
        }
        _ => false,
    }
}

/// `len(v)` when `v` has `__len__`.
fn py_len(v: &Value) -> Option<usize> {
    match v {
        Value::Str(s) => Some(s.chars().count()),
        Value::List(l) | Value::Tuple(l) => Some(l.len()),
        Value::Obj(o) => Some(o.len()),
        _ => None,
    }
}

/// `n < t`, `n > t` for an int text `t`.
fn cmp_int(n: usize, t: &str) -> std::cmp::Ordering {
    use std::cmp::Ordering::*;
    if t.starts_with('-') {
        return Greater;
    }
    let t = t.trim_start_matches('0');
    let n = n.to_string();
    if n == "0" && t.is_empty() {
        return Equal;
    }
    match n.len().cmp(&t.len()) {
        Equal => n.as_str().cmp(t),
        o => o,
    }
}

/// `float(v)` of an int or bool value.
fn to_float(v: &Value) -> R<f64> {
    match v {
        Value::Bool(b) => Ok(if *b { 1.0 } else { 0.0 }),
        Value::Float(f) => Ok(*f),
        Value::Int(t) => {
            let f: f64 = t.parse().unwrap_or(f64::INFINITY);
            if f.is_infinite() {
                return Err(PyErr::new("OverflowError", "int too large to convert to float").into());
            }
            Ok(f)
        }
        _ => Ok(f64::NAN),
    }
}

/// `re.search(pattern, s) is not None`, with the error `re` raises. The
/// gate drops the FutureWarning a possible nested set prints.
fn re_search(pattern: &str, s: &str) -> R<bool> {
    Ok(pyre::search_bounded(pattern, s)?)
}

/// `predicate_z3._eval_scalar(expr, scope)`.
fn eval_scalar(e: &Expr, scope: &Value) -> R<bool> {
    match e {
        Expr::Equals { path, value } => Ok(match resolve_path(scope, path)? {
            Got::Val(v) => py_eq(&v, value),
            _ => false,
        }),
        Expr::NotEquals { path, value } => Ok(match resolve_path(scope, path)? {
            Got::Missing => false,
            Got::Method => true,
            Got::Val(v) => !py_eq(&v, value),
        }),
        Expr::InSet { path, values } => Ok(match resolve_path(scope, path)? {
            Got::Val(v) => values.iter().any(|x| py_eq(&v, x)),
            _ => false,
        }),
        Expr::NotInSet { path, values } => Ok(match resolve_path(scope, path)? {
            Got::Missing => false,
            Got::Method => true,
            Got::Val(v) => !values.iter().any(|x| py_eq(&v, x)),
        }),
        Expr::Matches { path, regex } => match resolve_path(scope, path)? {
            Got::Val(Value::Str(s)) => re_search(regex, &s),
            _ => Ok(false),
        },
        Expr::NotMatches { path, regex } => match resolve_path(scope, path)? {
            Got::Val(Value::Str(s)) => Ok(!re_search(regex, &s)?),
            _ => Ok(true),
        },
        Expr::NumericRange { path, min, max } => match resolve_path(scope, path)? {
            Got::Val(v @ (Value::Int(_) | Value::Bool(_) | Value::Float(_))) => {
                let f = to_float(&v)?;
                Ok(*min <= f && f <= *max)
            }
            _ => Ok(false),
        },
        Expr::LengthRange { path, min, max } => {
            let n = match resolve_path(scope, path)? {
                Got::Val(v) => match py_len(&v) {
                    Some(n) => n,
                    None => return Ok(false),
                },
                _ => return Ok(false),
            };
            if cmp_int(n, min) == std::cmp::Ordering::Less {
                return Ok(false);
            }
            if let Some(m) = max {
                if cmp_int(n, m) == std::cmp::Ordering::Greater {
                    return Ok(false);
                }
            }
            Ok(true)
        }
        Expr::Exists { path } => Ok(!matches!(resolve_path(scope, path)?, Got::Missing)),
        Expr::IsEmpty { path } => Ok(match resolve_path(scope, path)? {
            Got::Missing | Got::Val(Value::Null) => true,
            Got::Method => false,
            Got::Val(v) => py_len(&v) == Some(0),
        }),
        Expr::And(cs) => {
            for c in cs {
                if !eval_scalar(c, scope)? {
                    return Ok(false);
                }
            }
            Ok(true)
        }
        Expr::Or(cs) => {
            for c in cs {
                if eval_scalar(c, scope)? {
                    return Ok(true);
                }
            }
            Ok(false)
        }
        Expr::Not(c) => Ok(!eval_scalar(c, scope)?),
        Expr::Implies(a, b) => Ok(!eval_scalar(a, scope)? || eval_scalar(b, scope)?),
        Expr::LlmCheck { .. } => {
            Err(PyErr::value("LLMCheck must be evaluated via evaluate_llm_check, not _eval_scalar").into())
        }
        Expr::Alias { name, .. } => Err(PyErr::value(format!(
            "unresolved alias reference '{name}'; resolve aliases before evaluation"
        ))
        .into()),
        other => Err(PyErr::value(format!("unknown predicate op: '{}'", other.op())).into()),
    }
}

// ---------------------------------------------------------------------
// Vacuity: _compile_scalar over one symbolic step, and regex_to_z3
// ---------------------------------------------------------------------

/// A z3py exception raised while building or solving: caught by the
/// vacuity check like any other.
fn z3e(e: Z3Error) -> Fault {
    Fault::Raised(PyErr::new("Z3Exception", e.0))
}

struct Scope {
    vars: HashMap<String, Term>,
}

fn var_name(path: &str) -> String {
    // z3py passes the name as a C string: it ends at a NUL.
    let n = format!("vac__{}", path.replace('.', "__"));
    match n.find('\0') {
        Some(i) => n[..i].to_string(),
        None => n,
    }
}

impl Scope {
    fn string(&mut self, c: &Ctx, path: &str) -> R<Term> {
        let name = var_name(path);
        let full = format!("vac__{}", path.replace('.', "__"));
        if let Some(t) = self.vars.get(&full) {
            return Ok(*t);
        }
        let t = c.constant(&name, Sort::Str).map_err(z3e)?;
        self.vars.insert(full, t);
        Ok(t)
    }

    fn numeric(&mut self, c: &Ctx, path: &str) -> R<Term> {
        let name = var_name(path);
        let key = format!("vac__{}__real", path.replace('.', "__"));
        if let Some(t) = self.vars.get(&key) {
            return Ok(*t);
        }
        let t = c.constant(&name, Sort::Real).map_err(z3e)?;
        self.vars.insert(key, t);
        Ok(t)
    }
}

/// `predicate_z3._z3_lit(value)`.
fn lit(c: &Ctx, v: &Value) -> R<Term> {
    let r = match v {
        Value::Bool(b) => c.bool_val(*b),
        Value::Int(t) => c.numeral(t, Sort::Int),
        Value::Float(f) => c.numeral(&py_float_repr(*f), Sort::Real),
        other => c.string_val_chars(py_str(other)?.chars().map(pyre::pycp)),
    };
    r.map_err(z3e)
}

fn is_num(v: &Value) -> bool {
    matches!(v, Value::Int(_) | Value::Float(_))
}

/// `vacuity._compute_vacuity`.
fn compute_vacuity(e: &Expr) -> R<Verdict> {
    let inner = match e {
        Expr::ForallSteps(p) | Expr::ExistsStep(p) | Expr::ForallOutputs(p) => p,
        other => other,
    };
    with_main(|c| vacuity_on(c, inner)).map_err(z3e)?
}

fn vacuity_on(c: &mut Ctx, inner: &Expr) -> R<Verdict> {
    let mut scope = Scope { vars: HashMap::new() };
    let mut soft = 0usize;
    let term = compile_scalar(c, inner, &mut scope, &mut soft)?;
    if c.check(500, &[term]).map_err(z3e)? == Check::Unsat {
        return Ok(Verdict::Contradiction);
    }
    let neg = c.not(term).map_err(z3e)?;
    if c.check(500, &[neg]).map_err(z3e)? == Check::Unsat {
        return Ok(Verdict::Tautology);
    }
    Ok(Verdict::NonTrivial)
}

fn soft_bool(c: &Ctx, kind: &str, soft: &mut usize) -> R<Term> {
    let name = format!("vac__{kind}__{soft}");
    *soft += 1;
    c.constant(&name, Sort::Bool).map_err(z3e)
}

/// `predicate_z3._compile_scalar` with a fully symbolic scope.
fn compile_scalar(c: &Ctx, e: &Expr, scope: &mut Scope, soft: &mut usize) -> R<Term> {
    let z = |r: Result<Term, Z3Error>| r.map_err(z3e);
    match e {
        Expr::Equals { path, value } | Expr::NotEquals { path, value } => {
            let var = if is_num(value) { scope.numeric(c, path)? } else { scope.string(c, path)? };
            let l = lit(c, value)?;
            if matches!(e, Expr::Equals { .. }) {
                z(c.eq(var, l))
            } else {
                z(c.ne(var, l))
            }
        }
        Expr::InSet { path, values } | Expr::NotInSet { path, values } => {
            let var = if values.first().is_some_and(is_num) { scope.numeric(c, path)? } else { scope.string(c, path)? };
            let inset = matches!(e, Expr::InSet { .. });
            if values.is_empty() {
                return z(c.bool_val(!inset));
            }
            let mut ts = Vec::with_capacity(values.len());
            for v in values {
                let l = lit(c, v)?;
                ts.push(if inset { z(c.eq(var, l))? } else { z(c.ne(var, l))? });
            }
            if inset {
                z(c.or(&ts))
            } else {
                z(c.and(&ts))
            }
        }
        Expr::Matches { path, regex } | Expr::NotMatches { path, regex } => {
            let var = scope.string(c, path)?;
            let pos = matches!(e, Expr::Matches { .. });
            match translate(c, regex)? {
                None => {
                    let b = soft_bool(c, if pos { "matches" } else { "not_matches" }, soft)?;
                    if pos {
                        Ok(b)
                    } else {
                        z(c.not(b))
                    }
                }
                Some(re) => {
                    let m = z(c.in_re(var, re))?;
                    if pos {
                        Ok(m)
                    } else {
                        z(c.not(m))
                    }
                }
            }
        }
        Expr::NumericRange { path, min, max } => {
            let var = scope.numeric(c, path)?;
            let lo = z(c.numeral(&py_float_repr(*min), Sort::Real))?;
            let a = z(c.ge(var, lo))?;
            let hi = z(c.numeral(&py_float_repr(*max), Sort::Real))?;
            let b = z(c.le(var, hi))?;
            z(c.and(&[a, b]))
        }
        Expr::LengthRange { path, min, max } => {
            let var = scope.string(c, path)?;
            let len = z(c.length(var))?;
            let lo = z(c.numeral(min, Sort::Int))?;
            let mut bounds = vec![z(c.ge(len, lo))?];
            if let Some(m) = max {
                let hi = z(c.numeral(m, Sort::Int))?;
                bounds.push(z(c.le(len, hi))?);
            }
            if bounds.len() > 1 {
                z(c.and(&bounds))
            } else {
                Ok(bounds[0])
            }
        }
        Expr::Exists { .. } => z(c.bool_val(true)),
        Expr::IsEmpty { .. } => soft_bool(c, "is_empty", soft),
        Expr::And(cs) | Expr::Or(cs) => {
            let and = matches!(e, Expr::And(_));
            if cs.is_empty() {
                return z(c.bool_val(and));
            }
            let mut ts = Vec::with_capacity(cs.len());
            for ch in cs {
                ts.push(compile_scalar(c, ch, scope, soft)?);
            }
            if and {
                z(c.and(&ts))
            } else {
                z(c.or(&ts))
            }
        }
        Expr::Not(ch) => {
            let t = compile_scalar(c, ch, scope, soft)?;
            z(c.not(t))
        }
        Expr::Implies(a, b) => {
            let x = compile_scalar(c, a, scope, soft)?;
            let y = compile_scalar(c, b, scope, soft)?;
            z(c.implies(x, y))
        }
        Expr::LlmCheck { .. } => soft_bool(c, "llm_check", soft),
        Expr::Alias { name, .. } => Err(PyErr::value(format!(
            "unresolved alias reference '{name}'; resolve aliases before compilation"
        ))
        .into()),
        _ => Err(PyErr::value("unknown scalar predicate op").into()),
    }
}

/// `regex_to_z3.translate(pattern)`: the regex, `None` for an
/// UnsupportedRegexError, or another exception z3py raises.
fn translate(c: &Ctx, pattern: &str) -> R<Option<Term>> {
    let parsed = match pyre::parse(pattern, 0) {
        Ok(p) => p,
        Err(e) if e.is("Undecided") => return Err(e.into()),
        Err(_) => return Ok(None),
    };
    if parsed.flags & !pyre::flags::UNICODE != 0 {
        return Ok(None);
    }
    let mut items: &[Node] = &parsed.nodes;
    let mut anchor_start = false;
    let mut anchor_end = false;
    if let Some(Node::At(At::Beginning | At::BeginningString)) = items.first() {
        anchor_start = true;
        items = &items[1..];
    }
    if let Some(Node::At(At::End | At::EndString)) = items.last() {
        anchor_end = true;
        items = &items[..items.len() - 1];
    }
    let body = match seq(c, items)? {
        None => return Ok(None),
        Some(b) => b,
    };
    let z = |r: Result<Term, Z3Error>| r.map_err(z3e);
    let left = if anchor_start { z(c.re_lit([]))? } else { z(c.re_star(any_char(c)?))? };
    let right = if anchor_end { z(c.re_lit([]))? } else { z(c.re_star(any_char(c)?))? };
    Ok(Some(z(c.re_concat(&[left, body, right]))?))
}

fn any_char(c: &Ctx) -> R<Term> {
    let z = |r: Result<Term, Z3Error>| r.map_err(z3e);
    let all = z(c.re_range(0, 0xFFFF))?;
    let nl = z(c.re_lit([0x0A]))?;
    let not_nl = z(c.re_complement(nl))?;
    z(c.re_intersect(&[all, not_nl]))
}

fn complement_char(c: &Ctx, r: Term) -> R<Term> {
    let a = any_char(c)?;
    let n = c.re_complement(r).map_err(z3e)?;
    c.re_intersect(&[a, n]).map_err(z3e)
}

fn category(c: &Ctx, cat: Cat) -> R<Term> {
    let z = |r: Result<Term, Z3Error>| r.map_err(z3e);
    let digit = || z(c.re_range('0' as u32, '9' as u32));
    let word = || -> R<Term> {
        let parts = [
            z(c.re_range('a' as u32, 'z' as u32))?,
            z(c.re_range('A' as u32, 'Z' as u32))?,
            z(c.re_range('0' as u32, '9' as u32))?,
            z(c.re_lit(['_' as u32]))?,
        ];
        z(c.re_union(&parts))
    };
    let space = || -> R<Term> {
        let parts = [z(c.re_lit([0x20]))?, z(c.re_lit([0x09]))?, z(c.re_lit([0x0A]))?, z(c.re_lit([0x0D]))?];
        z(c.re_union(&parts))
    };
    match cat {
        Cat::Digit => digit(),
        Cat::NotDigit => complement_char(c, digit()?),
        Cat::Word => word(),
        Cat::NotWord => complement_char(c, word()?),
        Cat::Space => space(),
        Cat::NotSpace => complement_char(c, space()?),
    }
}

/// `_pattern_to_regex`: `None` is an UnsupportedRegexError.
fn seq(c: &Ctx, items: &[Node]) -> R<Option<Term>> {
    let mut pieces = Vec::new();
    for n in items {
        if let Node::At(a) = n {
            if matches!(a, At::Beginning | At::End | At::BeginningString | At::EndString) {
                continue;
            }
            return Ok(None);
        }
        match node(c, n)? {
            None => return Ok(None),
            Some(t) => pieces.push(t),
        }
    }
    let z = |r: Result<Term, Z3Error>| r.map_err(z3e);
    Ok(Some(match pieces.len() {
        0 => z(c.re_lit([]))?,
        1 => pieces[0],
        _ => z(c.re_concat(&pieces))?,
    }))
}

/// A repeat count past which the port does not build `[inner] * lo`.
const MAX_CONCAT: u64 = 10_000;

/// `_node_to_regex`.
fn node(c: &Ctx, n: &Node) -> R<Option<Term>> {
    let z = |r: Result<Term, Z3Error>| r.map_err(z3e);
    Ok(Some(match n {
        Node::Literal(cp) => z(c.re_lit([*cp]))?,
        Node::NotLiteral(cp) => {
            let l = z(c.re_lit([*cp]))?;
            complement_char(c, l)?
        }
        Node::Any => any_char(c)?,
        Node::In(items) => {
            let negate = matches!(items.first(), Some(SetItem::Negate));
            let items = if negate { &items[1..] } else { &items[..] };
            let mut pieces = Vec::new();
            for it in items {
                pieces.push(match it {
                    SetItem::Literal(cp) => z(c.re_lit([*cp]))?,
                    SetItem::Range(lo, hi) => z(c.re_range(*lo, *hi))?,
                    SetItem::Category(cat) => category(c, *cat)?,
                    SetItem::Negate => return Ok(None),
                });
            }
            if pieces.is_empty() {
                return Ok(None);
            }
            let u = if pieces.len() == 1 { pieces[0] } else { z(c.re_union(&pieces))? };
            if negate {
                complement_char(c, u)?
            } else {
                u
            }
        }
        Node::Branch(alts) => {
            let mut pieces = Vec::new();
            for a in alts {
                match seq(c, a)? {
                    None => return Ok(None),
                    Some(t) => pieces.push(t),
                }
            }
            if pieces.is_empty() {
                return Ok(None);
            }
            if pieces.len() == 1 {
                pieces[0]
            } else {
                z(c.re_union(&pieces))?
            }
        }
        Node::Repeat { kind: RepKind::Max | RepKind::Min, min, max, body } => {
            let inner = match seq(c, body)? {
                None => return Ok(None),
                Some(t) => t,
            };
            let (lo, hi) = (*min, *max);
            if hi == MAXREPEAT {
                if lo == 0 {
                    return Ok(Some(z(c.re_star(inner))?));
                }
                if lo == 1 {
                    return Ok(Some(z(c.re_plus(inner))?));
                }
                if lo > MAX_CONCAT {
                    return undecided("a regex repeat too long to expand");
                }
                let mut v = vec![inner; lo as usize];
                v.push(z(c.re_star(inner))?);
                return Ok(Some(z(c.re_concat(&v))?));
            }
            if lo == 0 && hi == 1 {
                return Ok(Some(z(c.re_option(inner))?));
            }
            if lo == hi {
                if lo == 0 {
                    return Ok(Some(z(c.re_lit([]))?));
                }
                if lo > MAX_CONCAT {
                    return undecided("a regex repeat too long to expand");
                }
                return Ok(Some(z(c.re_concat(&vec![inner; lo as usize]))?));
            }
            z(c.re_loop(inner, lo as u32, hi as u32))?
        }
        Node::Subpattern { add, del, body, .. } => {
            if *add != 0 || *del != 0 {
                return Ok(None);
            }
            return seq(c, body);
        }
        _ => return Ok(None),
    }))
}

#[allow(dead_code)]
fn show_cp(cp: u32) -> char {
    cp_char(cp)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(j: &str) -> Result<Expr, String> {
        let v = super::super::pyjson::loads(j).unwrap();
        parse_expression(&v).map_err(|e| match e {
            Fault::Raised(p) => p.msg,
            Fault::Undecided(u) => u,
        })
    }

    #[test]
    fn union_errors_read_as_pydantics() {
        let m = parse(r#"{"op": "forall_steps", "body": {"op": "not_equals", "path": "type", "value": "network"}}"#)
            .unwrap_err();
        assert_eq!(
            m,
            format!(
                "1 validation error for {UNION_TITLE}\nforall_steps.pred\n  Field required [type=missing, \
                 input_value={{'op': 'forall_steps', 'b...e', 'value': 'network'}}}}, input_type=dict]\n    \
                 For further information visit https://errors.pydantic.dev/2.13/v/missing"
            )
        );
        let m = parse(r#"{"path": "x"}"#).unwrap_err();
        assert!(m.contains("]\n  Unable to extract tag using discriminator 'op' [type=union_tag_not_found"), "{m}");
        let m = parse(r#"{"op": "and", "children": [{"op": "equals"}, 5]}"#).unwrap_err();
        assert!(m.starts_with(&format!("3 validation errors for {UNION_TITLE}\nand.children.0.equals.path\n")), "{m}");
    }

    fn vac(j: &str) -> Verdict {
        let e = parse(j).unwrap();
        catch(compute_vacuity(&e)).unwrap().unwrap_or(Verdict::NonTrivial)
    }

    #[test]
    fn vacuity_on_the_linked_z3() {
        assert_eq!(
            vac(r#"{"op":"and","children":[{"op":"equals","path":"p","value":"a"},{"op":"equals","path":"p","value":"b"}]}"#),
            Verdict::Contradiction
        );
        assert_eq!(
            vac(r#"{"op":"or","children":[{"op":"equals","path":"p","value":"a"},{"op":"not_equals","path":"p","value":"a"}]}"#),
            Verdict::Tautology
        );
        assert_eq!(vac(r#"{"op":"matches","path":"p","regex":"^a+$"}"#), Verdict::NonTrivial);
        assert_eq!(
            vac(r#"{"op":"and","children":[{"op":"matches","path":"p","regex":"^[0-9]+$"},{"op":"equals","path":"p","value":"x"}]}"#),
            Verdict::Contradiction
        );
        // a{1} is Concat of one term: z3py raises, and the check says non_trivial.
        assert_eq!(
            vac(r#"{"op":"and","children":[{"op":"matches","path":"p","regex":"a{1}"},{"op":"equals","path":"p","value":"x"},{"op":"equals","path":"p","value":"y"}]}"#),
            Verdict::NonTrivial
        );
        // A lookahead is soft: the rest still decides.
        assert_eq!(
            vac(r#"{"op":"and","children":[{"op":"matches","path":"p","regex":"(?=a)"},{"op":"equals","path":"p","value":"x"},{"op":"equals","path":"p","value":"y"}]}"#),
            Verdict::Contradiction
        );
    }

    #[test]
    fn python_equality() {
        let v = |j: &str| super::super::pyjson::loads(j).unwrap();
        assert!(py_eq(&v("1"), &v("1.0")));
        assert!(py_eq(&v("true"), &v("1")));
        assert!(!py_eq(&v("9007199254740993"), &v("9007199254740992.0")));
        assert!(py_eq(&v(r#"{"a":[1,2]}"#), &v(r#"{"a":[1.0,2]}"#)));
        assert!(!py_eq(&v("NaN"), &v("NaN")));
    }
}
