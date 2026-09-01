//! The paths no CLI case reaches: the per-step rejection and the
//! recompute fallback (K2-1), and a Z3 check that does not finish (K2-2).

use std::collections::{BTreeMap, HashMap};

use super::executors::Executor;
use super::fallback::{recompute_with, Hooks};
use super::*;
use crate::gate::pyjson::{loads, Object, Value};
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::pathways::verify::Outcome as Verified;
use crate::pathways::PwErr;
use crate::tracejournal::Journal;
use crate::violation::Violation;

pub(crate) fn scratch(tag: &str) -> String {
    let n = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos();
    let d = std::env::temp_dir().join(format!("dg-k2-{tag}-{}-{n}", std::process::id()));
    std::fs::create_dir_all(&d).unwrap();
    d.to_string_lossy().into_owned()
}

pub(crate) fn model(id: Id, text: &str) -> Object {
    match validate_model(id, &loads(text).unwrap(), Mode::Json) {
        Ok(Value::Obj(o)) => o,
        other => panic!("{text}: {other:?}"),
    }
}

/// An executor that notes each step it runs.
struct Recorder(std::rc::Rc<std::cell::RefCell<Vec<String>>>);

impl Executor for Recorder {
    fn run(&mut self, step: &Object, _: u64, _: usize) -> Result<ExecResult, String> {
        self.0.borrow_mut().push(str_of(step, "id"));
        Ok(ExecResult { rc: 0, stdout: "ok".into(), ..Default::default() })
    }
}

struct Setup {
    plan: Object,
    env: Object,
    dir: String,
    journal: Journal,
    ran: std::rc::Rc<std::cell::RefCell<Vec<String>>>,
}

const ONE_STEP: &str = r#"[{"id": "s1", "type": "shell", "command": "echo hi"}]"#;

fn setup(steps: &str) -> Setup {
    let env = model(
        Id::Envelope,
        r#"{"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {"shell": true, "shell_allowlist": ["echo"]}}"#,
    );
    let plan = model(Id::ActionPlan, &format!(r#"{{"id": "plan_00000001", "source": "t", "task": "t", "steps": {steps}}}"#));
    let dir = scratch("sup");
    let journal = Journal::open(&dir).unwrap();
    Setup { plan, env, dir, journal, ran: Default::default() }
}

impl Setup {
    fn supervisor(&self, fallback: Option<Fallback>, vs: crate::pathways::verify::StepCheck) -> Supervisor<'_> {
        let mut ex: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
        ex.insert("shell".into(), Box::new(Recorder(self.ran.clone())));
        let mut s = Supervisor::new(ex, Box::new(Always), Some(&self.journal));
        s.fallback = fallback;
        s.verify_step = vs;
        s
    }

    fn ok_verification(&self) -> Object {
        verification_dump(&[], &[], false, self.env.value("id"), self.plan.value("id"), 1.0).unwrap()
    }

    fn count(&self, table: &str) -> i64 {
        let c = rusqlite::Connection::open(format!("{}/journal/index.db", self.dir)).unwrap();
        c.query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |r| r.get(0)).unwrap()
    }
}

impl Drop for Setup {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn verified(violations: Vec<Violation>, timeouts: Vec<String>) -> Verified {
    Verified { violations, timeouts, warnings: vec![], warnings_unmodeled: false }
}

fn reject_all(_: &Value, _: &Value, _: u32) -> Result<Verified, PwErr> {
    Ok(verified(vec![Violation::plan("permissions").msg("no")], vec![]))
}

fn unknown(_: &Value, _: &Value, _: u32) -> Result<Verified, PwErr> {
    Ok(verified(vec![], vec!["Z3 returned unknown".into()]))
}

fn only_first(plan: &Value, _: &Value, _: u32) -> Result<Verified, PwErr> {
    let first = plan.as_obj().unwrap().value("steps");
    let Value::List(l) = first else { panic!() };
    if l[0].as_obj().unwrap().value("id").as_str() == Some("s1") {
        return Ok(verified(vec![Violation::plan("permissions").msg("no")], vec![]));
    }
    Ok(verified(vec![], vec![]))
}

/// A step the per-step check rejects halts the run: nothing runs, one
/// refinement is written, and the run is journaled.
#[test]
fn a_per_step_rejection_halts() {
    let s = setup(ONE_STEP);
    let sess = s.supervisor(None, reject_all).run(&s.plan, &s.env, s.ok_verification());
    assert_eq!(sess.status, HALTED);
    assert_eq!(sess.steps.len(), 1);
    assert_eq!(sess.steps[0].status, "rejected_halted");
    assert_eq!(sess.steps[0].error.as_deref(), Some("rejected: no"));
    assert!(s.ran.borrow().is_empty());
    assert_eq!((s.count("refinement_log"), s.count("receipts"), s.count("traces")), (1, 0, 1));
}

/// A Z3 check that does not finish in the per-step check fails closed.
#[test]
fn a_per_step_z3_unknown_fails_closed() {
    let s = setup(ONE_STEP);
    let sess = s.supervisor(None, unknown).run(&s.plan, &s.env, s.ok_verification());
    assert_eq!(sess.status, HALTED);
    assert!(s.ran.borrow().is_empty());
    assert_eq!(sess.steps[0].error.as_deref(), Some("rejected: Z3 returned unknown"));
}

fn shell_step(text: &str) -> Object {
    let i = crate::pathways::pmodel::steps().order.iter().position(|t| *t == "shell").unwrap();
    model(Id::Step(i), text)
}

/// A recomputed step that passes the per-step check runs in place of the
/// rejected one; one that fails it halts.
#[test]
fn a_recomputed_step() {
    let replacement = shell_step(r#"{"id": "s1b", "type": "shell", "command": "echo ok"}"#);
    let fb = |r: Object| -> Fallback { Box::new(move |_, _| Some((r.clone(), Object::new()))) };
    let s = setup(ONE_STEP);
    let sess = s.supervisor(Some(fb(replacement.clone())), only_first).run(&s.plan, &s.env, s.ok_verification());
    assert_eq!(sess.status, SUCCEEDED);
    assert_eq!(sess.steps.len(), 2);
    assert_eq!(sess.steps[0].status, "rejected_recomputed");
    assert_eq!(*s.ran.borrow(), vec!["s1b".to_string()]);
    let s2 = setup(ONE_STEP);
    let sess = s2.supervisor(Some(fb(replacement)), reject_all).run(&s2.plan, &s2.env, s2.ok_verification());
    assert_eq!(sess.status, HALTED);
    assert!(s2.ran.borrow().is_empty());
    assert!(sess.steps[1].error.as_deref().unwrap().starts_with("recomputed step rejected: "));
}

fn verify_unknown(_: &Value, _: &Value, _: Option<bool>, _: u32) -> Result<Verified, PwErr> {
    Ok(verified(vec![], vec!["Z3 returned unknown".into()]))
}

/// Recompute keeps a replacement only when a one-step plan of it
/// verifies; a Z3 check that does not finish there halts (fail closed).
#[test]
fn recompute_verifies_the_replacement() {
    let s = setup(ONE_STEP);
    let hooks = |verify| Hooks {
        preflight: Box::new(|_| true),
        structured: Box::new(|call| {
            assert_eq!(call.response.name, "ShellStep");
            assert!(call.user.contains("Rejected step:"));
            Some(shell_step(r#"{"id": "s1", "type": "shell", "command": "echo ok"}"#))
        }),
        verify,
    };
    let step = crate::tracejournal::dag::steps(&s.plan)[0].clone();
    let mut fb = recompute_with(hooks(crate::pathways::verify::verify), s.env.clone(), 500);
    assert!(fb(&step, &[]).is_some(), "a replacement that verifies was dropped");
    let mut fb = recompute_with(hooks(verify_unknown), s.env.clone(), 500);
    assert!(fb(&step, &[]).is_none(), "a replacement whose check did not finish was kept");
}

/// The receipt's hash is compute_evidence_hash's: the oracle's value for
/// this evidence.
#[test]
fn the_evidence_hash_is_the_oracles() {
    let ev = Object::new().with("rc", py_int(0)).with("stdout", "hé\n").with("duration_ms", 1.5).with("status", "succeeded");
    assert_eq!(evidence_hash(&ev), "77ee6301ac882338165aea615acf9539bfcc7cf02683c45fa735d823d76a03b2");
}

/// The default approval: the allowlist, then DAISUGI_APPROVE, then no
/// terminal means deny.
#[test]
fn the_default_approval() {
    let s = setup(ONE_STEP);
    let mut step = crate::tracejournal::dag::steps(&s.plan)[0].clone();
    let mut d = DefaultApproval { env: HashMap::new(), terminal: false };
    let got = d.decide(&step, &s.env).unwrap();
    assert!(got.approved && got.approved_by == "allowlist");
    step.set("command", "echo hi > x");
    let got = d.decide(&step, &s.env).unwrap();
    assert!(!got.approved && got.approved_by == "denied");
    d.env.insert("DAISUGI_APPROVE".into(), " NEVER ".into());
    let got = d.decide(&step, &s.env).unwrap();
    assert!(!got.approved && got.approved_by == "env");
    d.env.insert("DAISUGI_APPROVE".into(), "maybe".into());
    assert!(d.decide(&step, &s.env).unwrap_err().starts_with("ValueError: DAISUGI_APPROVE='maybe'"));
}
