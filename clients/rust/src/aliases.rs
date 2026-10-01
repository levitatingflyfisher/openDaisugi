//! The oracle's alias registry (`opendaisugi/aliases.py`) and its shipped
//! system aliases (`system_aliases.py`): named, parameterized predicate
//! expressions in three tiers, registered with a static path check and a
//! Z3 vacuity check, and resolved by one structural substitution.
//!
//! No command of the oracle builds a registry, so no command here does
//! either: at every command an alias is unresolved (verify words it). The
//! registry is the library part `integrations/hermes.py` calls, measured
//! by `alias-probe` against the oracle (`clients/alias_compare.py`). The Go
//! `internal/aliases` is the reference.

use std::collections::HashMap;

use crate::gate::predicate::{parse_expression, vacuity_strict, Expr, Vac};
use crate::gate::pyjson::{loads, py_repr, py_type_name, Object, Value};
use crate::gate::Fault;

/// `aliases.Alias`; `expr` is the body as plain data.
#[derive(Clone, Debug)]
pub struct Alias {
    pub name: String,
    pub params: Vec<String>,
    pub expr: Value,
    pub tier: String,
    pub description: String,
}

impl Alias {
    /// `Alias.model_dump(mode="json")`.
    pub fn dump(&self) -> Value {
        Value::Obj(
            Object::new()
                .with("name", self.name.as_str())
                .with(
                    "params",
                    Value::List(self.params.iter().map(|p| Value::Str(p.clone())).collect()),
                )
                .with("expr", self.expr.clone())
                .with("tier", self.tier.as_str())
                .with("description", self.description.as_str()),
        )
    }
}

/// An exception the oracle raises: its class name and `str()`.
#[derive(Clone, Debug, PartialEq)]
pub struct Error {
    pub class: String,
    pub msg: String,
}

fn raise(class: &str, msg: String) -> Error {
    Error {
        class: class.into(),
        msg,
    }
}

fn tier_order(t: &str) -> i32 {
    match t {
        "envelope" => 0,
        "household" => 1,
        _ => 2,
    }
}

const PATH_OPS: [&str; 13] = [
    "equals",
    "not_equals",
    "in_set",
    "not_in_set",
    "matches",
    "not_matches",
    "numeric_range",
    "exists",
    "is_empty",
    "depends_on",
    "before",
    "alias",
    "llm_check",
];

/// `aliases.AliasRegistry`.
#[derive(Default)]
pub struct Registry {
    entries: HashMap<String, Vec<Alias>>,
}

fn repr(v: &Value) -> String {
    py_repr(v).unwrap_or_default()
}

/// `aliases._references_a_path`. Iterating a value that is no list
/// raises as Python does: a str or a dict iterates to strings, anything
/// else is not iterable.
fn references_a_path(expr: &Value) -> Result<bool, Error> {
    let Value::Obj(d) = expr else {
        return Ok(false);
    };
    let op = d.value("op").as_str().unwrap_or("");
    if PATH_OPS.contains(&op) {
        return Ok(true);
    }
    match op {
        "and" | "or" => match d.get("children") {
            None | Some(Value::Str(_)) | Some(Value::Obj(_)) => Ok(false),
            Some(Value::List(l)) | Some(Value::Tuple(l)) => {
                for c in l {
                    if references_a_path(c)? {
                        return Ok(true);
                    }
                }
                Ok(false)
            }
            Some(other) => Err(raise(
                "TypeError",
                format!("'{}' object is not iterable", py_type_name(other)),
            )),
        },
        "not" => references_a_path(d.value("child")),
        "implies" => Ok(references_a_path(d.value("a"))? || references_a_path(d.value("b"))?),
        "forall_steps" | "exists_step" | "forall_outputs" | "forall_writes" => {
            references_a_path(d.value("pred"))
        }
        _ => Ok(false),
    }
}

/// `aliases._names_an_alias`: an alias reference at any depth.
fn names_an_alias(expr: &Value) -> bool {
    match expr {
        Value::Obj(o) => {
            o.value("op").as_str() == Some("alias") || o.iter().any(|(_, v)| names_an_alias(v))
        }
        Value::List(l) | Value::Tuple(l) => l.iter().any(names_an_alias),
        _ => false,
    }
}

/// The `$name` placeholders for some names, longest name first, so `$p`
/// never takes the front of `$p_name`.
struct Placeholders(Vec<String>);

impl Placeholders {
    fn new(names: &[String]) -> Placeholders {
        let mut v = names.to_vec();
        v.sort_by_key(|n| std::cmp::Reverse(n.len()));
        Placeholders(v)
    }

    /// Each placeholder in `s` replaced by `repl(name)`, in one scan:
    /// replaced text is never scanned again.
    fn sub(&self, s: &str, repl: &dyn Fn(&str) -> String) -> String {
        let mut out = String::new();
        let mut i = 0;
        let b = s.as_bytes();
        while i < b.len() {
            if b[i] == b'$' {
                if let Some(n) = self.0.iter().find(|n| s[i + 1..].starts_with(n.as_str())) {
                    out.push_str(&repl(n));
                    i += 1 + n.len();
                    continue;
                }
            }
            let ch = s[i..].chars().next().expect("a char at a boundary");
            out.push(ch);
            i += ch.len_utf8();
        }
        out
    }

    fn search(&self, s: &str) -> bool {
        let found = std::cell::Cell::new(false);
        self.sub(s, &|_| {
            found.set(true);
            String::new()
        });
        found.get()
    }
}

fn has_placeholder(expr: &Value, params: &[String]) -> bool {
    if params.is_empty() {
        return false;
    }
    let p = Placeholders::new(params);
    fn walk(x: &Value, p: &Placeholders) -> bool {
        match x {
            Value::Str(s) => p.search(s),
            Value::Obj(o) => o.iter().any(|(_, v)| walk(v, p)),
            Value::List(l) | Value::Tuple(l) => l.iter().any(|v| walk(v, p)),
            _ => false,
        }
    }
    walk(expr, &p)
}

/// `parse_expression` on plain data: the expression, or the exception.
fn parse(v: &Value) -> Result<Expr, Error> {
    match parse_expression(v) {
        Ok(e) => Ok(e),
        Err(Fault::Raised(e)) => Err(raise(&e.kind, e.msg)),
        Err(Fault::Undecided(w)) => Err(raise("Undecided", w)),
    }
}

/// `str()` of a decoded JSON value.
fn py_str(v: &Value) -> String {
    match v {
        Value::Str(s) => s.clone(),
        other => repr(other),
    }
}

/// `re.escape`.
fn re_escape(s: &str) -> String {
    const SPECIAL: &str = "()[]{}?*+-|^$\\.&~# \t\n\r\u{b}\u{c}";
    let mut out = String::new();
    for c in s.chars() {
        if SPECIAL.contains(c) {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

/// `aliases._substitute_params`: one pass over the structure. A string
/// that is exactly `$name` becomes the argument itself (outside a regex
/// field); a `$name` inside a string becomes its text, and inside a regex
/// field its escaped text.
fn substitute(expr: &Value, args: &Object) -> Value {
    let names: Vec<String> = args.keys().to_vec();
    let p = Placeholders::new(&names);
    fn walk(x: &Value, args: &Object, p: &Placeholders, in_regex: bool) -> Value {
        match x {
            Value::Str(s) => {
                if p.0.is_empty() {
                    return x.clone();
                }
                if !in_regex {
                    if let Some(name) = s.strip_prefix('$') {
                        if let Some(v) = args.get(name) {
                            return v.clone();
                        }
                    }
                }
                let text = if in_regex {
                    p.sub(s, &|n| re_escape(&py_str(args.value(n))))
                } else {
                    p.sub(s, &|n| py_str(args.value(n)))
                };
                Value::Str(text)
            }
            Value::List(l) => Value::List(l.iter().map(|v| walk(v, args, p, in_regex)).collect()),
            Value::Tuple(l) => Value::Tuple(l.iter().map(|v| walk(v, args, p, in_regex)).collect()),
            Value::Obj(o) => {
                let mut out = Object::new();
                for (k, v) in o.iter() {
                    out.set(k, walk(v, args, p, k == "regex"));
                }
                Value::Obj(out)
            }
            other => other.clone(),
        }
    }
    walk(expr, args, &p, false)
}

/// `Expr.model_dump(mode="json")`: every field in model order.
pub fn dump(e: &Expr) -> Value {
    let o = Object::new().with("op", e.op());
    let o = match e {
        Expr::Equals { path, value } | Expr::NotEquals { path, value } => {
            o.with("path", path.as_str()).with("value", value.clone())
        }
        Expr::InSet { path, values } | Expr::NotInSet { path, values } => o
            .with("path", path.as_str())
            .with("values", Value::List(values.clone())),
        Expr::Matches { path, regex } | Expr::NotMatches { path, regex } => {
            o.with("path", path.as_str()).with("regex", regex.as_str())
        }
        Expr::NumericRange { path, min, max } => o
            .with("path", path.as_str())
            .with("min", Value::Float(*min))
            .with("max", Value::Float(*max)),
        Expr::LengthRange { path, min, max } => o
            .with("path", path.as_str())
            .with("min", Value::Int(min.clone()))
            .with(
                "max",
                max.as_ref()
                    .map(|m| Value::Int(m.clone()))
                    .unwrap_or(Value::Null),
            ),
        Expr::Exists { path } | Expr::IsEmpty { path } => o.with("path", path.as_str()),
        Expr::And(c) | Expr::Or(c) => o.with("children", Value::List(c.iter().map(dump).collect())),
        Expr::Not(c) => o.with("child", dump(c)),
        Expr::Implies(a, b) => o.with("a", dump(a)).with("b", dump(b)),
        Expr::ForallSteps(p)
        | Expr::ExistsStep(p)
        | Expr::ForallOutputs(p)
        | Expr::ForallWrites(p) => o.with("pred", dump(p)),
        Expr::DependsOn { a, b } | Expr::Before { a, b } => o
            .with("step_id_a", a.as_str())
            .with("step_id_b", b.as_str()),
        Expr::Alias { name, args } => o.with("name", name.as_str()).with("args", args.clone()),
        Expr::LlmCheck { rule } => o.with("rule", rule.as_str()),
    };
    Value::Obj(o)
}

impl Registry {
    /// `AliasRegistry()`.
    pub fn new() -> Registry {
        Registry::default()
    }

    /// `name in registry`.
    pub fn contains(&self, name: &str) -> bool {
        self.entries.contains_key(name)
    }

    /// `AliasRegistry.register`: adds the alias, or returns the oracle's
    /// exception and adds nothing. A system alias's name is taken for
    /// good, in either order.
    pub fn register(&mut self, a: Alias) -> Result<(), Error> {
        if let Some(taken) = self.entries.get(&a.name) {
            if !taken.is_empty() && (a.tier == "system" || taken.iter().any(|t| t.tier == "system"))
            {
                return Err(raise(
                    "ValueError",
                    format!(
                        "alias '{}' is a system alias name; a {} alias cannot share it, since it would \
                         redefine what the system word means",
                        a.name, a.tier
                    ),
                ));
            }
        }
        if !references_a_path(&a.expr)? {
            return Err(raise(
                "ValueError",
                format!(
                    "alias '{}' has no plan-path reference (looks vacuous); static check requires at \
                     least one Equals/NotEquals/Matches/... on a path",
                    a.name
                ),
            ));
        }
        vacuity(&a)?;
        self.entries.entry(a.name.clone()).or_default().push(a);
        Ok(())
    }

    /// `AliasRegistry.lookup`: the highest-precedence tier, the first
    /// registered among equals.
    pub fn lookup(&self, name: &str) -> Result<Alias, Error> {
        let Some(entries) = self.entries.get(name) else {
            return Err(raise("UnknownAliasError", repr(&Value::Str(name.into()))));
        };
        let mut best = &entries[0];
        for a in &entries[1..] {
            if tier_order(&a.tier) < tier_order(&best.tier) {
                best = a;
            }
        }
        Ok(best.clone())
    }

    /// `AliasRegistry.resolve` over a parsed expression.
    pub fn resolve(&self, e: &Expr) -> Result<Expr, Error> {
        self.resolve_in(e, &[])
    }

    fn resolve_in(&self, e: &Expr, seen: &[String]) -> Result<Expr, Error> {
        let r = |x: &Expr| self.resolve_in(x, seen).map(Box::new);
        Ok(match e {
            Expr::Alias { name, args } => {
                if seen.contains(name) {
                    return Err(raise(
                        "AliasCycleError",
                        format!("alias cycle detected: {name} -> ... -> {name}"),
                    ));
                }
                let a = self.lookup(name)?;
                let args = args.as_obj().cloned().unwrap_or_default();
                let missing: Vec<Value> = a
                    .params
                    .iter()
                    .filter(|p| args.get(p).is_none())
                    .map(|p| Value::Str(p.clone()))
                    .collect();
                if !missing.is_empty() {
                    return Err(raise(
                        "ValueError",
                        format!(
                            "alias '{name}' missing required args: {}",
                            repr(&Value::List(missing))
                        ),
                    ));
                }
                let sub = substitute(&a.expr, &args);
                let parsed = parse(&sub)?;
                let mut next = seen.to_vec();
                next.push(name.clone());
                return self.resolve_in(&parsed, &next);
            }
            Expr::And(c) => Expr::And(
                c.iter()
                    .map(|x| self.resolve_in(x, seen))
                    .collect::<Result<_, _>>()?,
            ),
            Expr::Or(c) => Expr::Or(
                c.iter()
                    .map(|x| self.resolve_in(x, seen))
                    .collect::<Result<_, _>>()?,
            ),
            Expr::Not(c) => Expr::Not(r(c)?),
            Expr::Implies(a, b) => Expr::Implies(r(a)?, r(b)?),
            Expr::ForallSteps(p) => Expr::ForallSteps(r(p)?),
            Expr::ExistsStep(p) => Expr::ExistsStep(r(p)?),
            Expr::ForallOutputs(p) => Expr::ForallOutputs(r(p)?),
            Expr::ForallWrites(p) => Expr::ForallWrites(r(p)?),
            other => other.clone(),
        })
    }
}

/// `AliasRegistry._vacuity`: tautologies and contradictions are refused,
/// a body that names another alias, or that parses only once a typed
/// placeholder is bound, is deferred, and any other error refuses.
fn vacuity(a: &Alias) -> Result<(), Error> {
    if names_an_alias(&a.expr) || a.expr.as_obj().is_none() {
        return Ok(());
    }
    let e = match parse(&a.expr) {
        Ok(e) => e,
        Err(_) if !a.params.is_empty() && has_placeholder(&a.expr, &a.params) => return Ok(()),
        Err(err) => {
            return Err(raise(
                "ValueError",
                format!(
                    "alias '{}' body is not a valid predicate: {}",
                    a.name, err.msg
                ),
            ))
        }
    };
    let verdict = vacuity_strict(&e).map_err(|w| {
        raise(
            "ValueError",
            format!(
                "alias '{}' could not be checked for vacuity, so it is not registered: {w}",
                a.name
            ),
        )
    })?;
    let word = match verdict {
        Vac::Tautology => "tautology",
        Vac::Contradiction => "contradiction",
        Vac::NonTrivial => return Ok(()),
    };
    Err(raise(
        "VacuousAliasError",
        format!(
            "alias '{}' is {word} (constrains nothing / never satisfiable); the predicate must be \
             non-trivial to be registered",
            a.name
        ),
    ))
}

/// `system_aliases.py`'s seven aliases, in its order, each body as the
/// raw dict it stores (typed placeholders such as `$max_scale` survive
/// until substitution).
const SYSTEM_SOURCE: &str = r#"[
{"name": "velocity_scale_bounded", "params": ["max_scale"],
 "description": "forall joint_move / cartesian_move steps: velocity_scale <= max_scale",
 "expr": {"op": "forall_steps", "pred": {"op": "implies",
   "a": {"op": "in_set", "path": "type", "values": ["joint_move", "cartesian_move"]},
   "b": {"op": "numeric_range", "path": "velocity_scale", "min": 0.0, "max": "$max_scale"}}}},
{"name": "never_impersonates", "params": ["principal"],
 "description": "outbound communication steps must not sign as the principal",
 "expr": {"op": "forall_steps", "pred": {"op": "implies",
   "a": {"op": "in_set", "path": "type", "values": ["email_send", "imessage_send", "shell"]},
   "b": {"op": "and", "children": [
     {"op": "not_equals", "path": "metadata.signature", "value": "$principal"},
     {"op": "not_matches", "path": "metadata.body", "regex": "(?i)(\\u2014|-)\\s*$principal"}]}}}},
{"name": "no_pii_regex", "params": [],
 "description": "forbid PII patterns (SSN, credit card) in content or metadata.body",
 "expr": {"op": "forall_steps", "pred": {"op": "and", "children": [
   {"op": "not_matches", "path": "content", "regex": "\\b\\d{3}-\\d{2}-\\d{4}\\b"},
   {"op": "not_matches", "path": "content", "regex": "\\b(?:\\d[ -]*?){13,16}\\b"},
   {"op": "not_matches", "path": "metadata.body", "regex": "\\b\\d{3}-\\d{2}-\\d{4}\\b"},
   {"op": "not_matches", "path": "metadata.body", "regex": "\\b(?:\\d[ -]*?){13,16}\\b"}]}}},
{"name": "no_secrets", "params": [],
 "description": "forbid common secret patterns (AWS keys, JWT, ssh private keys)",
 "expr": {"op": "forall_steps", "pred": {"op": "and", "children": [
   {"op": "not_matches", "path": "content", "regex": "AKIA[0-9A-Z]{16}"},
   {"op": "not_matches", "path": "content",
    "regex": "eyJ[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{10,}"},
   {"op": "not_matches", "path": "content", "regex": "-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----"},
   {"op": "not_matches", "path": "metadata.body", "regex": "AKIA[0-9A-Z]{16}"},
   {"op": "not_matches", "path": "metadata.body",
    "regex": "-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----"}]}}},
{"name": "pytest_passes", "params": [],
 "description": "POSTCONDITION-ONLY: at least one completed step's output matches /passed/. Do NOT use as an Invariant — metadata.output is unset at Stage 1 verify.",
 "expr": {"op": "exists_step", "pred": {"op": "matches", "path": "metadata.output", "regex": "passed"}}},
{"name": "no_network_writes", "params": [],
 "description": "reject any network step that is not a GET",
 "expr": {"op": "forall_steps", "pred": {"op": "implies",
   "a": {"op": "equals", "path": "type", "value": "network"},
   "b": {"op": "equals", "path": "method", "value": "GET"}}}},
{"name": "structured_approval", "params": [],
 "description": "council-member outputs must be structured approval JSON",
 "expr": {"op": "forall_steps", "pred": {"op": "matches", "path": "metadata.output",
   "regex": "^\\s*\\{[^}]*\"approve\"\\s*:\\s*(true|false)"}}}
]"#;

/// `system_aliases`' aliases, in the order `load_system_aliases`
/// registers them.
pub fn system() -> Vec<Alias> {
    let Ok(Value::List(items)) = loads(SYSTEM_SOURCE) else {
        panic!("the system aliases do not read");
    };
    items
        .iter()
        .map(|x| {
            let o = x.as_obj().expect("an alias is an object");
            Alias {
                name: o.value("name").as_str().unwrap_or("").into(),
                params: match o.value("params") {
                    Value::List(l) => l
                        .iter()
                        .filter_map(|p| p.as_str().map(String::from))
                        .collect(),
                    _ => vec![],
                },
                expr: o.value("expr").clone(),
                tier: "system".into(),
                description: o.value("description").as_str().unwrap_or("").into(),
            }
        })
        .collect()
}

/// `load_system_aliases`: each system alias registered in order; the
/// first refusal is returned.
pub fn load_system(r: &mut Registry) -> Result<(), Error> {
    for a in system() {
        r.register(a)?;
    }
    Ok(())
}

/// `registry.resolve(parse_expression(expr))`: the resolved expression's
/// dump, or the oracle's exception.
pub fn parse_and_resolve(r: &Registry, expr: &Value) -> Result<Value, Error> {
    let e = parse(expr)?;
    Ok(dump(&r.resolve(&e)?))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn val(text: &str) -> Value {
        loads(text).ok().expect("JSON")
    }

    /// Substitution is one pass: text an argument puts in is never
    /// scanned again, the longest name wins, and a regex field gets the
    /// escaped text.
    #[test]
    fn substitution_is_one_pass() {
        let args = val(r#"{"p": "$p_name", "p_name": "(x|y)+"}"#)
            .as_obj()
            .cloned()
            .unwrap();
        let expr = val(r#"{"value": "$p and $p_name", "whole": "$p", "regex": "^$p_name$"}"#);
        let got = crate::gate::pyjson::dumps(&substitute(&expr, &args), true);
        assert_eq!(
            got,
            r#"{"value": "$p_name and (x|y)+", "whole": "$p_name", "regex": "^\\(x\\|y\\)\\+$"}"#
        );
    }

    #[test]
    fn re_escape_is_pythons() {
        assert_eq!(re_escape("a.b-c é\t"), "a\\.b\\-c\\ é\\\t");
    }

    #[test]
    fn registry_laws() {
        let mut r = Registry::new();
        load_system(&mut r).unwrap();
        let eq = val(r#"{"op": "equals", "path": "type", "value": "shell"}"#);
        let household = |name: &str, expr: Value| Alias {
            name: name.into(),
            params: vec![],
            expr,
            tier: "household".into(),
            description: String::new(),
        };
        assert_eq!(
            r.register(household("no_secrets", eq)).unwrap_err().class,
            "ValueError"
        );
        r.register(household(
            "a",
            val(r#"{"op": "alias", "name": "a", "args": {}}"#),
        ))
        .unwrap();
        let e = parse_and_resolve(&r, &val(r#"{"op": "alias", "name": "a"}"#)).unwrap_err();
        assert_eq!(
            (e.class.as_str(), e.msg.as_str()),
            ("AliasCycleError", "alias cycle detected: a -> ... -> a")
        );
        let e = r.lookup("nope").unwrap_err();
        assert_eq!(
            (e.class.as_str(), e.msg.as_str()),
            ("UnknownAliasError", "'nope'")
        );
        let e = parse_and_resolve(&r, &val(r#"{"op": "alias", "name": "never_impersonates"}"#))
            .unwrap_err();
        assert_eq!(
            e.msg,
            "alias 'never_impersonates' missing required args: ['principal']"
        );
    }
}
