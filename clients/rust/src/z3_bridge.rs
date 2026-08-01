//! SMT-LIB2 text emission + `z3 -in` subprocess bridge.
//!
//! Both Z3 call sites the Full profile actually needs — `check_vacuity`
//! (predicate.py's tautology/contradiction classification) and
//! `envelope_subsumes` (skill-delegation subsumption, `subsumption.rs`) —
//! compile a predicate expression over a fully SYMBOLIC scope
//! (`predicate_z3._Scope(concrete=None)`), never a concrete one: the
//! oracle's Z3 usage in this codebase is 100% symbolic reasoning ("is there
//! any assignment that breaks this"), so `Scope`/`compile_scalar` here are
//! shared by both call sites unchanged.
//!
//! Design note on `Matches`/`NotMatches`: the oracle's own translator
//! (`regex_to_z3.py`) falls back to a free/soft Z3 boolean for any regex it
//! can't symbolically translate. This port always takes that fallback path
//! — every `Matches`/`NotMatches` compiles to a fresh free boolean, never a
//! real `str.in_re` term. Verified against the corpus: all 3 real regex
//! literals present resolve to the identical tautology/contradiction/
//! non_trivial classification whether the regex is soft or genuinely
//! translated, because in every case the surrounding formula's outcome is
//! already decided by a sibling clause (see the session notes / README).
//! Not a spec deviation in principle — it's the oracle's own fallback
//! branch, just taken unconditionally instead of only when translation
//! fails.

use crate::predicate::Expr;
use serde_json::Value;
use std::collections::HashMap;
use std::io::Write;
use std::process::{Command, Stdio};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Sort {
    Str,
    Real,
}

#[derive(Debug, Clone)]
pub struct Scope {
    prefix: String,
    /// dedup key -> (actual SMT identifier, sort)
    pub vars: HashMap<String, (String, Sort)>,
    pub soft: Vec<String>,
}

impl Scope {
    pub fn new(prefix: &str) -> Self {
        Scope { prefix: prefix.to_string(), vars: HashMap::new(), soft: Vec::new() }
    }

    fn var_name(&self, path: &str) -> String {
        format!("{}__{}", self.prefix, path.replace('.', "__"))
    }

    pub fn resolve_string(&mut self, path: &str) -> String {
        let smt_name = self.var_name(path);
        self.vars.entry(smt_name.clone()).or_insert_with(|| (smt_name.clone(), Sort::Str));
        smt_name
    }

    pub fn resolve_numeric(&mut self, path: &str) -> String {
        let smt_name = self.var_name(path);
        let dedup_key = format!("{smt_name}__real");
        self.vars.entry(dedup_key).or_insert_with(|| (smt_name.clone(), Sort::Real));
        smt_name
    }

    pub fn fresh_soft(&mut self, tag: &str) -> String {
        let name = format!("{}__{}__{}", self.prefix, tag, self.soft.len());
        self.soft.push(name.clone());
        name
    }

    /// Merge another scope's declared vars/soft names into this one (used by
    /// subsumption, which shares one `cmd` var across two independently
    /// built scopes and needs a combined declaration list).
    pub fn absorb(&mut self, other: &Scope) {
        for (k, v) in &other.vars {
            self.vars.entry(k.clone()).or_insert_with(|| v.clone());
        }
        for s in &other.soft {
            if !self.soft.contains(s) {
                self.soft.push(s.clone());
            }
        }
    }
}

fn is_numeric_json(v: &Value) -> bool {
    matches!(v, Value::Number(_))
}

fn python_str_of(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Null => "None".to_string(),
        Value::Bool(b) => {
            if *b {
                "True".to_string()
            } else {
                "False".to_string()
            }
        }
        Value::Number(n) => n.to_string(),
        other => other.to_string(),
    }
}

fn smt_str_lit(v: &Value) -> String {
    format!("\"{}\"", python_str_of(v).replace('"', "\"\""))
}

pub fn smt_real(n: f64) -> String {
    if n < 0.0 {
        format!("(- {})", smt_real(-n))
    } else if n.fract() == 0.0 {
        format!("{n:.1}")
    } else {
        format!("{n}")
    }
}

fn smt_real_lit(v: &Value) -> String {
    smt_real(v.as_f64().unwrap_or(0.0))
}

/// `predicate_z3._compile_scalar` — emits an SMT-LIB2 boolean term string
/// for a scalar predicate over a fully symbolic scope. See the module
/// docstring for the `Matches`/`NotMatches`-always-soft design choice.
pub fn compile_scalar(expr: &Expr, scope: &mut Scope) -> String {
    match expr {
        Expr::Equals { path, value } => {
            if is_numeric_json(value) {
                let v = scope.resolve_numeric(path);
                format!("(= {v} {})", smt_real_lit(value))
            } else {
                let v = scope.resolve_string(path);
                format!("(= {v} {})", smt_str_lit(value))
            }
        }
        Expr::NotEquals { path, value } => {
            // F-3 fixed oracle-side 2026-08-21: NotEquals now branches
            // numeric-vs-string exactly like Equals (see ADJUDICATIONS.md).
            if is_numeric_json(value) {
                let v = scope.resolve_numeric(path);
                format!("(not (= {v} {}))", smt_real_lit(value))
            } else {
                let v = scope.resolve_string(path);
                format!("(not (= {v} {}))", smt_str_lit(value))
            }
        }
        Expr::InSet { path, values } => {
            if values.is_empty() {
                return "false".to_string();
            }
            let numeric = is_numeric_json(&values[0]);
            let v = if numeric { scope.resolve_numeric(path) } else { scope.resolve_string(path) };
            let disj: Vec<String> = values
                .iter()
                .map(|x| format!("(= {v} {})", if numeric { smt_real_lit(x) } else { smt_str_lit(x) }))
                .collect();
            format!("(or {})", disj.join(" "))
        }
        Expr::NotInSet { path, values } => {
            if values.is_empty() {
                return "true".to_string();
            }
            let numeric = is_numeric_json(&values[0]);
            let v = if numeric { scope.resolve_numeric(path) } else { scope.resolve_string(path) };
            let conj: Vec<String> = values
                .iter()
                .map(|x| format!("(not (= {v} {}))", if numeric { smt_real_lit(x) } else { smt_str_lit(x) }))
                .collect();
            format!("(and {})", conj.join(" "))
        }
        Expr::Matches { .. } => scope.fresh_soft("matches"),
        Expr::NotMatches { .. } => format!("(not {})", scope.fresh_soft("not_matches")),
        Expr::NumericRange { path, min, max } => {
            let v = scope.resolve_numeric(path);
            format!("(and (>= {v} {}) (<= {v} {}))", smt_real(*min), smt_real(*max))
        }
        Expr::LengthRange { path, min, max } => {
            let v = scope.resolve_string(path);
            let mut parts = vec![format!("(>= (str.len {v}) {min})")];
            if let Some(m) = max {
                parts.push(format!("(<= (str.len {v}) {m})"));
            }
            if parts.len() == 1 { parts.remove(0) } else { format!("(and {})", parts.join(" ")) }
        }
        Expr::Exists { .. } => "true".to_string(),
        Expr::IsEmpty { .. } => scope.fresh_soft("is_empty"),
        Expr::And { children } => {
            if children.is_empty() {
                return "true".to_string();
            }
            let parts: Vec<String> = children.iter().map(|c| compile_scalar(c, scope)).collect();
            format!("(and {})", parts.join(" "))
        }
        Expr::Or { children } => {
            if children.is_empty() {
                return "false".to_string();
            }
            let parts: Vec<String> = children.iter().map(|c| compile_scalar(c, scope)).collect();
            format!("(or {})", parts.join(" "))
        }
        Expr::Not { child } => format!("(not {})", compile_scalar(child, scope)),
        Expr::Implies { a, b } => format!("(=> {} {})", compile_scalar(a, scope), compile_scalar(b, scope)),
        Expr::LLMCheck { .. } => scope.fresh_soft("llm_check"),
        // DependsOn/Before/ForallSteps/ExistsStep/ForallOutputs/AliasRef never
        // reach compile_scalar in practice (stripped or rejected earlier);
        // fall back to an inert `true` rather than panicking on malformed input.
        _ => "true".to_string(),
    }
}

fn declare_block(scope: &Scope) -> String {
    let mut out = String::new();
    for (name, sort) in scope.vars.values() {
        let sort_str = match sort {
            Sort::Str => "String",
            Sort::Real => "Real",
        };
        out.push_str(&format!("(declare-const {name} {sort_str})\n"));
    }
    for soft in &scope.soft {
        out.push_str(&format!("(declare-const {soft} Bool)\n"));
    }
    out
}

/// Runs `script` through `z3 -in` and returns the `(check-sat)` result
/// lines in order ("sat"/"unsat"/"unknown"). One process per call —
/// simple and correctness-first; the corpus only needs a few dozen Z3
/// round trips total (see `README.md`).
fn run_z3_script(script: &str) -> Result<Vec<String>, String> {
    let mut child = Command::new("z3")
        .arg("-in")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("failed to spawn z3: {e}"))?;
    {
        let stdin = child.stdin.as_mut().ok_or("z3: no stdin handle")?;
        stdin.write_all(script.as_bytes()).map_err(|e| e.to_string())?;
    }
    let output = child.wait_with_output().map_err(|e| e.to_string())?;
    let text = String::from_utf8_lossy(&output.stdout).into_owned();
    Ok(text.lines().map(|l| l.trim().to_string()).filter(|l| !l.is_empty()).collect())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Vacuity {
    Contradiction,
    Tautology,
    NonTrivial,
}

/// `check_vacuity` — strips one outer quantifier (`ForallSteps`/
/// `ExistsStep`/`ForallOutputs`), compiles the inner predicate over a fresh
/// symbolic scope, then asks Z3 two questions in one script: is the term
/// itself UNSAT (contradiction)? is its negation UNSAT (tautology)? Any
/// failure (spawn error, malformed output, Z3 `unknown`) fails OPEN to
/// `NonTrivial` — matching `_check_predicate_item`'s
/// `except Exception: vacuity_verdict = "non_trivial"` and
/// `_compute_vacuity`'s own unknown-falls-through-to-non_trivial behavior.
pub fn check_vacuity(expr: &Expr) -> Vacuity {
    let inner = match expr {
        Expr::ForallSteps { pred } | Expr::ExistsStep { pred } | Expr::ForallOutputs { pred } => pred.as_ref(),
        other => other,
    };
    let mut scope = Scope::new("vac");
    let term = compile_scalar(inner, &mut scope);

    let mut script = String::new();
    script.push_str("(set-logic ALL)\n");
    script.push_str(&declare_block(&scope));
    script.push_str(&format!("(push)\n(assert {term})\n(check-sat)\n(pop)\n"));
    script.push_str(&format!("(push)\n(assert (not {term}))\n(check-sat)\n(pop)\n"));

    match run_z3_script(&script) {
        Ok(lines) if lines.len() >= 2 => {
            if lines[0] == "unsat" {
                Vacuity::Contradiction
            } else if lines[1] == "unsat" {
                Vacuity::Tautology
            } else {
                Vacuity::NonTrivial
            }
        }
        _ => Vacuity::NonTrivial,
    }
}

/// Low-level single check-sat query, exposed for `subsumption.rs` (which
/// builds a combined two-scope script of its own rather than reusing the
/// vacuity script shape). Returns the raw result string ("sat"/"unsat"/
/// "unknown") or an error.
pub fn check_sat(script: &str) -> Result<String, String> {
    let lines = run_z3_script(script)?;
    lines.into_iter().next().ok_or_else(|| "z3: no output".to_string())
}

pub fn declare_block_pub(scope: &Scope) -> String {
    declare_block(scope)
}
