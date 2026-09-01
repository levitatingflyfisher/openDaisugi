//! The oracle's within-instance batch compilation (`opendaisugi/batch.py`):
//! an agent declares a program, its items, the write footprint and an
//! acceptance check; the whole write set is proved inside the envelope
//! and the footprint before any item runs, programs that could leave an
//! irreversible effect are refused, the acceptance is checked on a sample
//! that is then undone from the deed ledger, and every item runs under
//! the supervisor with per-element rollback. The Go client's
//! `internal/batch` is the reference.

use crate::deeds::{self, Report};
use crate::envgen::bind::apply_bindings;
use crate::gate::py::text::repr_list;
use crate::gate::pyjson::{Object, Value};
use crate::glob_engine::path_matches_any;
use crate::lerr::{LErr, PyError, LR};
use crate::pathways::pathway::dump_json;
use crate::pathways::pmodel::{validate_json, validate_model, Id, Mode, ValidationError};
use crate::supervise::{py_int, Session, SUCCEEDED};

const BATCHABLE: &[&str] = &["file_write", "file_read", "network"];

/// A validated BatchDeclaration dump.
#[derive(Debug, Clone)]
pub struct Decl(pub Object);

fn list(v: &Value) -> &[Value] {
    match v {
        Value::List(l) => l,
        _ => &[],
    }
}

fn strs(v: &Value) -> Vec<String> {
    list(v)
        .iter()
        .filter_map(|x| x.as_str().map(str::to_string))
        .collect()
}

impl Decl {
    fn program(&self) -> Object {
        self.0
            .value("program")
            .as_obj()
            .cloned()
            .unwrap_or_default()
    }

    fn steps(&self) -> Vec<Object> {
        list(self.program().value("steps"))
            .iter()
            .filter_map(|s| s.as_obj().cloned())
            .collect()
    }

    /// `decl.model_dump_json()`.
    pub fn dump_json(&self) -> String {
        dump_json(&Value::Obj(self.0.clone()))
    }
}

/// `BatchDeclaration.model_validate(v)` (Python mode).
pub fn validate(v: &Value) -> Result<Decl, ValidationError> {
    match validate_model(Id::BatchDeclaration, v, Mode::Python)? {
        Value::Obj(o) => Ok(Decl(o)),
        _ => unreachable!("a model validates to an object"),
    }
}

/// `BatchDeclaration.model_validate_json(text)`.
pub fn validate_text(text: &str) -> Result<Decl, ValidationError> {
    match validate_json(Id::BatchDeclaration, text)? {
        Value::Obj(o) => Ok(Decl(o)),
        _ => unreachable!("a model validates to an object"),
    }
}

/// `BatchClassification`.
#[derive(Debug, Clone)]
pub struct Classification {
    pub batchable: bool,
    pub non_batchable: Vec<Object>,
}

impl Classification {
    /// `asdict`.
    pub fn dump(&self) -> Object {
        Object::new().with("batchable", self.batchable).with(
            "non_batchable",
            Value::List(self.non_batchable.iter().cloned().map(Value::Obj).collect()),
        )
    }

    /// `", ".join(sorted({type ...}))`.
    pub fn kinds(&self) -> String {
        let mut k: Vec<String> = self
            .non_batchable
            .iter()
            .map(|x| x.value("type").as_str().unwrap_or("").to_string())
            .collect();
        k.sort();
        k.dedup();
        k.join(", ")
    }
}

/// `classify_declaration`.
pub fn classify(d: &Decl) -> Classification {
    let mut nb = vec![];
    for s in d.steps() {
        let t = s.value("type").as_str().unwrap_or("").to_string();
        if !BATCHABLE.contains(&t.as_str()) {
            nb.push(
                Object::new()
                    .with("id", s.value("id").clone())
                    .with("type", t),
            );
        }
    }
    Classification {
        batchable: nb.is_empty(),
        non_batchable: nb,
    }
}

/// `resolve_items`: each item's bound plan, or None where the binding
/// fails (its index recorded). A ValueError from the binding is a
/// failure; any other exception is returned, as the oracle raises it.
pub fn resolve(d: &Decl) -> LR<(Vec<Option<Object>>, Vec<usize>)> {
    let params: Vec<Object> = list(d.0.value("parameters"))
        .iter()
        .filter_map(|p| p.as_obj().cloned())
        .collect();
    let program = d.program();
    let (mut plans, mut bad) = (vec![], vec![]);
    for (i, it) in list(d.0.value("items")).iter().enumerate() {
        let item = it.as_obj().cloned().unwrap_or_default();
        let plan = match apply_bindings(&program, &params, &item) {
            Ok(p) => p,
            Err(e) if e.class == "ValueError" => None,
            Err(e) => return Err(PyError::new(e.class, e.msg).into()),
        };
        if plan.is_none() {
            bad.push(i);
        }
        plans.push(plan);
    }
    Ok((plans, bad))
}

fn writes_of(plan: &Object) -> Vec<String> {
    list(plan.value("steps"))
        .iter()
        .filter_map(|s| s.as_obj())
        .filter(|s| s.value("type").as_str() == Some("file_write"))
        .map(|s| s.value("path").as_str().unwrap_or("").to_string())
        .collect()
}

/// `FootprintProof`.
#[derive(Debug, Clone, Default)]
pub struct Proof {
    pub ok: bool,
    pub writes: Vec<String>,
    pub out_of_envelope: Vec<String>,
    pub under_declared: Vec<String>,
    pub bad_bindings: Vec<usize>,
    pub reason: String,
    pub vacuous: bool,
}

fn str_values(xs: &[String]) -> Value {
    Value::List(xs.iter().map(Value::from).collect())
}

impl Proof {
    /// `asdict`.
    pub fn dump(&self) -> Object {
        Object::new()
            .with("ok", self.ok)
            .with("resolved_writes", str_values(&self.writes))
            .with("out_of_envelope", str_values(&self.out_of_envelope))
            .with("under_declared", str_values(&self.under_declared))
            .with(
                "bad_bindings",
                Value::List(
                    self.bad_bindings
                        .iter()
                        .map(|b| py_int(*b as i64))
                        .collect(),
                ),
            )
            .with("reason", self.reason.as_str())
            .with("vacuous", self.vacuous)
    }
}

/// `repr(list[str])`, `[]` when empty.
pub fn list_repr(xs: &[String]) -> String {
    repr_list(xs)
}

/// `prove_footprint`: every write each item resolves to checked with the
/// runtime gate's own path matcher against the envelope's file_write
/// globs and the declared footprint.
pub fn prove(d: &Decl, env_file_write: &[String]) -> LR<Proof> {
    let (plans, bad) = resolve(d)?;
    let foot = strs(d.0.value("footprint"));
    let mut p = Proof {
        bad_bindings: bad.clone(),
        ..Default::default()
    };
    for plan in plans.iter().flatten() {
        for w in writes_of(plan) {
            if !path_matches_any(&w, env_file_write) {
                p.out_of_envelope.push(w.clone());
            }
            if !path_matches_any(&w, &foot) {
                p.under_declared.push(w.clone());
            }
            p.writes.push(w);
        }
    }
    p.ok = bad.is_empty() && p.out_of_envelope.is_empty() && p.under_declared.is_empty();
    let mut reasons = vec![];
    if !bad.is_empty() {
        let idx: Vec<String> = bad.iter().map(|b| b.to_string()).collect();
        reasons.push(format!(
            "{} item(s) failed to bind (unbound hole or head change): [{}]",
            bad.len(),
            idx.join(", ")
        ));
    }
    if !p.out_of_envelope.is_empty() {
        reasons.push(format!(
            "{} write(s) outside the envelope: {}",
            p.out_of_envelope.len(),
            list_repr(&p.out_of_envelope)
        ));
    }
    if !p.under_declared.is_empty() {
        reasons.push(format!(
            "{} write(s) outside the declared footprint F: {}",
            p.under_declared.len(),
            list_repr(&p.under_declared)
        ));
    }
    p.reason = reasons.join("; ");
    p.vacuous = p.writes.is_empty();
    Ok(p)
}

/// `would_be_reversible`: a symlink target is not a reason to refuse;
/// otherwise the executor's own pre-image capture says.
pub fn would_be_reversible(path: &str) -> bool {
    if std::fs::symlink_metadata(path)
        .map(|m| m.file_type().is_symlink())
        .unwrap_or(false)
    {
        return true;
    }
    let mut parent = crate::supervise::executors::py_dirname(path);
    if parent.is_empty() {
        parent = ".".into();
    }
    crate::supervise::executors::capture_pre_image(path, &parent).is_some()
}

/// `[w for w in writes if not would_be_reversible(w)]`.
pub fn irreversible(writes: &[String]) -> Vec<String> {
    writes
        .iter()
        .filter(|w| !would_be_reversible(w))
        .cloned()
        .collect()
}

// -- the net-token meter ------------------------------------------------------

/// `_DEFAULT_TOKENS_PER_CALL`.
pub const DEFAULT_TOKENS_PER_CALL: i64 = 2000;

/// `NetTokenLedger`.
#[derive(Debug, Clone)]
pub struct Ledger {
    pub label: String,
    pub baseline: String,
    pub output_saved: i64,
    pub calls_saved: i64,
    pub per_call: i64,
    pub spec_injected: i64,
}

impl Ledger {
    /// `model_dump(mode="json")`, the computed fields last.
    pub fn dump(&self) -> Object {
        let net = self.output_saved + self.calls_saved * self.per_call - self.spec_injected;
        Object::new()
            .with("label", self.label.as_str())
            .with("baseline", self.baseline.as_str())
            .with("output_tokens_saved", py_int(self.output_saved))
            .with("calls_saved", py_int(self.calls_saved))
            .with("tokens_per_call", py_int(self.per_call))
            .with("spec_input_injected", py_int(self.spec_injected))
            .with("evidence_not_proof", true)
            .with("net", py_int(net))
            .with("net_positive", net > 0)
    }
}

/// `NetTokenLedger.within_instance`. `per_call` and `spec` None take the
/// defaults: 2000, and the declaration's JSON length / 4 (at least 1).
pub fn within_instance(
    d: &Decl,
    output_saved: i64,
    calls_saved: i64,
    per_call: Option<i64>,
    spec: Option<i64>,
) -> Ledger {
    Ledger {
        label: "within-instance".into(),
        baseline: "honest-script (a competent agent already scripts the bulk job)".into(),
        output_saved,
        calls_saved,
        per_call: per_call.unwrap_or(DEFAULT_TOKENS_PER_CALL),
        spec_injected: spec.unwrap_or_else(|| ((d.dump_json().chars().count() / 4) as i64).max(1)),
    }
}

/// `TwoLedgerReport`'s dump with no cross-instance ledger.
pub fn two_ledgers(within: &Ledger) -> Object {
    Object::new().with("within_instance", within.dump()).with("cross_instance", Value::Null).with(
        "note",
        "Two ledgers reported separately and never merged (roadmap Stage 9). The within-instance win is the proven \
         blast radius; the cross-instance (persistence + generalization) win is Stage 4's at-scale question.",
    )
}

// -- execution ----------------------------------------------------------------

/// `BatchResult`.
#[derive(Debug, Clone, Default)]
pub struct BatchResult {
    pub status: String,
    pub reason: String,
    pub classification: Option<Classification>,
    pub proof: Option<Proof>,
    pub sample_ok: bool,
    pub executed: i64,
    pub reversals: Vec<Object>,
    pub rollback: Option<Report>,
    pub ledger: Option<Object>,
}

impl BatchResult {
    /// The probe's view of the result: each part as asdict or model_dump
    /// writes it.
    pub fn dump(&self) -> Object {
        Object::new()
            .with("status", self.status.as_str())
            .with("reason", self.reason.as_str())
            .with(
                "classification",
                self.classification.as_ref().map(|c| Value::Obj(c.dump())),
            )
            .with("proof", self.proof.as_ref().map(|p| Value::Obj(p.dump())))
            .with("sample_ok", self.sample_ok)
            .with("executed", py_int(self.executed))
            .with(
                "reversals",
                Value::List(self.reversals.iter().cloned().map(Value::Obj).collect()),
            )
            .with(
                "rollback",
                self.rollback.as_ref().map(|r| Value::Obj(r.dump())),
            )
            .with("ledger", self.ledger.clone().map(Value::Obj))
    }
}

fn handles_of(s: &Session) -> Vec<Object> {
    s.steps.iter().filter_map(|o| o.reversal.clone()).collect()
}

/// `_rollback`: newest first; an undo that fails is reported as skipped
/// with the error, never as undone.
fn undo(handles: &[Object]) -> LR<Report> {
    let mut rep = Report::default();
    for h in handles.iter().rev() {
        let h = deeds::parse_handle(&Value::Obj(h.clone()))?;
        match deeds::apply(&h) {
            Ok(()) => rep.undone.push(h.path),
            Err(LErr::Io(e, p)) => {
                let text = match crate::supervise::executors::os_error(&e, &p) {
                    Some((_, t)) => t,
                    None => return Err(LErr::Io(e, p)),
                };
                rep.skipped.push(
                    Object::new()
                        .with("path", h.path)
                        .with("reason", format!("rollback failed: {text}")),
                );
            }
            Err(e) => return Err(e),
        }
    }
    Ok(rep)
}

fn acceptance_holds(q: Option<&Object>, s: &Session, plan: &Object) -> bool {
    if s.steps.iter().any(|o| o.status != SUCCEEDED) {
        return false;
    }
    let Some(q) = q else { return true };
    match q.value("type").as_str().unwrap_or("") {
        "succeeded" | "ran" => true,
        "rc_zero" | "rc0" => s.steps.iter().all(|o| o.rc.is_none_or(|rc| rc == 0)),
        t @ ("file_exists" | "file_nonempty") => {
            let paths = match q.value("path") {
                Value::Str(p) if !p.is_empty() => vec![p.clone()],
                _ => writes_of(plan),
            };
            paths.iter().all(|p| {
                if p.is_empty() {
                    return false;
                }
                match std::fs::metadata(p) {
                    Ok(m) => t != "file_nonempty" || m.len() != 0,
                    Err(_) => false,
                }
            })
        }
        _ => false,
    }
}

fn step_path(plan: &Object, id: &str) -> String {
    list(plan.value("steps"))
        .iter()
        .filter_map(|s| s.as_obj())
        .find(|s| s.value("id").as_str() == Some(id))
        .and_then(|s| s.value("path").as_str().map(str::to_string))
        .unwrap_or_default()
}

/// `run_batch`. `run` runs one bound plan under the supervisor
/// (`Supervisor.run`). `sample_k` None takes the declaration's sample_k.
pub fn run(
    d: &Decl,
    env_file_write: &[String],
    run: &mut dyn FnMut(&Object) -> LR<Session>,
    sample_k: Option<i64>,
) -> LR<BatchResult> {
    let cls = classify(d);
    if !cls.batchable {
        return Ok(BatchResult {
            status: "rejected".into(),
            reason: format!(
                "program contains non-batchable step kind(s): {}",
                cls.kinds()
            ),
            classification: Some(cls),
            ..Default::default()
        });
    }
    let proof = prove(d, env_file_write)?;
    if !proof.ok {
        return Ok(BatchResult {
            status: "rejected".into(),
            reason: format!("footprint not provable: {}", proof.reason),
            classification: Some(cls),
            proof: Some(proof),
            ..Default::default()
        });
    }
    let irr = irreversible(&proof.writes);
    if !irr.is_empty() {
        return Ok(BatchResult {
            status: "rejected".into(),
            reason: format!(
                "targets whose write would be irreversible cannot enter a batch: {}",
                list_repr(&irr)
            ),
            classification: Some(cls),
            proof: Some(proof),
            ..Default::default()
        });
    }
    let plans: Vec<Object> = resolve(d)?.0.into_iter().flatten().collect();
    let n = plans.len() as i64;
    let mut k = sample_k.unwrap_or_else(|| match d.0.value("sample_k") {
        Value::Int(t) => t.parse().unwrap_or(0),
        _ => 0,
    });
    if k < 0 {
        k = (k + n).max(0);
    }
    let k = k.min(n) as usize;
    let acc = d.0.value("acceptance").as_obj().cloned();
    let mut sample = vec![];
    let mut sample_ok = true;
    for plan in &plans[..k] {
        let s = run(plan)?;
        sample.extend(handles_of(&s));
        if s.status != SUCCEEDED || !acceptance_holds(acc.as_ref(), &s, plan) {
            sample_ok = false;
            break;
        }
    }
    let sample_report = undo(&sample)?;
    if !sample_ok {
        return Ok(BatchResult {
            status: "rejected".into(),
            reason: "acceptance postcondition Q failed on the sampled fork".into(),
            classification: Some(cls),
            proof: Some(proof),
            rollback: Some(sample_report),
            ..Default::default()
        });
    }
    let mut done: Vec<Object> = vec![];
    let mut executed = 0;
    for plan in &plans {
        let s = run(plan)?;
        if s.status != SUCCEEDED {
            let rep = undo(&done)?;
            return Ok(BatchResult {
                status: "halted".into(),
                reason: format!(
                    "element did not succeed ({}); rolled back and halted",
                    s.status
                ),
                classification: Some(cls),
                proof: Some(proof),
                sample_ok: true,
                executed,
                rollback: Some(rep),
                ..Default::default()
            });
        }
        let irreversible: Vec<&crate::supervise::Outcome> = s
            .steps
            .iter()
            .filter(|o| o.reversibility.as_deref() == Some("irreversible"))
            .collect();
        if !irreversible.is_empty() {
            let mut all = done.clone();
            all.extend(handles_of(&s));
            let mut rep = undo(&all)?;
            for o in irreversible {
                rep.skipped.push(
                    Object::new()
                        .with("step_id", o.step_id.as_str())
                        .with("path", step_path(plan, &o.step_id))
                        .with("reason", "irreversible"),
                );
            }
            return Ok(BatchResult {
                status: "halted".into(),
                reason: "an element produced an irreversible deed; halted before the rest".into(),
                classification: Some(cls),
                proof: Some(proof),
                sample_ok: true,
                executed,
                rollback: Some(rep),
                ..Default::default()
            });
        }
        done.extend(handles_of(&s));
        executed += 1;
    }
    let ledger = two_ledgers(&within_instance(d, 0, 0, None, None));
    Ok(BatchResult {
        status: "succeeded".into(),
        reason: String::new(),
        classification: Some(cls),
        proof: Some(proof),
        sample_ok: true,
        executed,
        reversals: done,
        rollback: None,
        ledger: Some(ledger),
    })
}

/// `rollback_result`: the rollback already made, or the succeeded batch
/// undone now from its handles.
pub fn rollback_result(r: &BatchResult) -> LR<Report> {
    match &r.rollback {
        Some(rep) => Ok(rep.clone()),
        None => undo(&r.reversals),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::loads;

    fn decl(text: &str) -> Decl {
        validate(&loads(text).unwrap()).unwrap()
    }

    const THREE: &str = r#"{"program": {"id": "plan_b0000001", "source": "script", "task": "batch",
        "steps": [{"id": "w", "type": "file_write", "path": "/out/x.txt", "content": "hi"}]},
        "parameters": [{"name": "p", "step_index": 0, "step_id": "w", "field": "path", "head": "/out"}],
        "items": [{"p": "/out/a.txt"}, {"p": "/out/b.txt"}, {"q": "x"}],
        "footprint": ["/out/*"]}"#;

    #[test]
    fn a_proof_names_what_did_not_bind_and_what_escapes() {
        let d = decl(THREE);
        let p = prove(&d, &["/out/**".into()]).unwrap();
        assert!(!p.ok);
        assert_eq!(p.writes, ["/out/a.txt", "/out/b.txt"]);
        assert_eq!(p.bad_bindings, [2]);
        assert_eq!(
            p.reason,
            "1 item(s) failed to bind (unbound hole or head change): [2]"
        );
        let p = prove(&d, &["/elsewhere/**".into()]).unwrap();
        assert_eq!(p.out_of_envelope.len(), 2);
        assert!(p
            .reason
            .contains("2 write(s) outside the envelope: ['/out/a.txt', '/out/b.txt']"));
    }

    #[test]
    fn a_shell_step_is_not_batchable() {
        let d = decl(
            r#"{"program": {"id": "plan_b0000001", "source": "script", "task": "batch",
            "steps": [{"id": "s", "type": "shell", "command": "make"}, {"id": "m", "type": "mcp", "server": "x", "tool": "t"}]},
            "items": [{}]}"#,
        );
        let c = classify(&d);
        assert!(!c.batchable);
        assert_eq!(c.kinds(), "mcp, shell");
        let mut never = |_: &Object| -> LR<Session> { panic!("no item may run") };
        let r = run(&d, &[], &mut never, None).unwrap();
        assert_eq!(r.status, "rejected");
    }

    #[test]
    fn the_ledger_counts_the_declaration_when_no_spec_is_given() {
        let d = decl(THREE);
        let l = within_instance(&d, 10, 1, None, None);
        assert_eq!(l.spec_injected, (d.dump_json().chars().count() / 4) as i64);
        let dump = l.dump();
        assert_eq!(dump.value("tokens_per_call"), &py_int(2000));
        assert_eq!(dump.value("net"), &py_int(10 + 2000 - l.spec_injected));
    }
}
