//! `supervisor.Supervisor`: one plan run step by step under the envelope.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use super::executors::Executor;
use super::fallback::Fallback;
use super::{hex8, now_iso, py_int, str_of, Approver, Decision, ExecResult, Outcome, Session};
use super::{ABORTED, FAILED, HALTED, PENDING, REJECTED, RUNNING, SUCCEEDED};
use crate::gate::py::text::{head, strip};
use crate::gate::pyjson::{Object, Value};
use crate::pathways::verify::{Outcome as Verified, StepCheck};
use crate::tracejournal::dag::{steps, topo_order};
use crate::tracejournal::runs::{now, Receipt, Run};
use crate::tracejournal::Journal;
use crate::violation::Violation;

/// `supervisor._READ_ONLY_KINDS`.
const READ_ONLY_KINDS: [&str; 2] = ["file_read", "network"];

/// `supervisor.Supervisor` with its CLI settings.
pub struct Supervisor<'a> {
    pub executors: BTreeMap<String, Box<dyn Executor>>,
    pub approval: Box<dyn Approver>,
    pub journal: Option<&'a Journal>,
    pub z3_timeout_ms: u32,
    pub step_timeout_s: u64,
    pub max_output_bytes: usize,
    pub strict: Option<bool>,
    /// The envelope's fallback handler; None halts.
    pub fallback: Option<Fallback>,
    /// The error log_run met, if any: the run is then not journaled, and
    /// the caller fails as the oracle raises.
    pub log_err: Option<String>,
    /// `verify.verify_step`; a test swaps it.
    pub verify_step: StepCheck,
    /// A runner's per-step hook (weave); None changes nothing.
    pub hook: Option<&'a mut dyn Hook>,
    /// `max_parallel`: above 1, a dependency level's parallel-safe steps
    /// run at once, at most this many at a time.
    pub max_parallel: usize,
}

/// What a hook's `prepare` returns for a step.
pub enum Prepared {
    /// Verify and run this step (it may be a filled copy).
    Run(Object),
    /// Record this outcome in the step's place and go on.
    Skip(Outcome),
    /// Record this outcome and stop the run with this status.
    Stop(Outcome, &'static str),
}

/// `supervisor.StepHook`: a runner's hook into each step.
pub trait Hook {
    fn prepare(&mut self, step: &Object) -> Prepared;
    /// Gets each step that passed its per-step verify; an outcome it
    /// returns stops the run there with the status.
    fn checked(&mut self, step: &Object) -> Option<(Outcome, &'static str)>;
    /// Called just before a step's executor runs; a reason it returns
    /// stops the run there, and the step does not run.
    fn started(&mut self, step: &Object, run_id: &str) -> Option<String>;
    /// Gets each executed step's outcome; returns the outcome to record.
    fn finish(&mut self, step: &Object, out: Outcome) -> Outcome;
    /// `prefetchable`: a step this says false for is never prefetched.
    fn prefetchable(&mut self, _step: &Object) -> bool {
        true
    }
}

/// `Supervisor._PARALLEL_SAFE_TYPES`.
const PARALLEL_SAFE_TYPES: &[&str] = &["shell", "file_read", "file_write", "network"];

impl<'a> Supervisor<'a> {
    pub fn new(executors: BTreeMap<String, Box<dyn Executor>>, approval: Box<dyn Approver>, journal: Option<&'a Journal>) -> Self {
        Supervisor {
            executors,
            approval,
            journal,
            z3_timeout_ms: 500,
            step_timeout_s: 30,
            max_output_bytes: 10 * 1024 * 1024,
            strict: None,
            fallback: None,
            log_err: None,
            verify_step: crate::pathways::verify::verify_step,
            hook: None,
            max_parallel: 1,
        }
    }
}

/// `VerificationResult.model_dump(mode="json")` of a verify. An error when
/// a violation's detail is one this binary does not word, or a warning may
/// be missing: the caller refuses.
pub fn verification_dump(
    violations: &[Violation],
    warnings: &[String],
    warnings_unmodeled: bool,
    env_id: &Value,
    plan_id: &Value,
    duration_ms: f64,
) -> Result<Object, String> {
    let mut vs = vec![];
    for v in violations {
        let Some(detail) = &v.detail else {
            return Err(format!("a violation ({}) whose detail this binary does not write yet", v.message));
        };
        vs.push(Value::Obj(
            Object::new()
                .with("stage", v.stage)
                .with("message", v.message.as_str())
                .with("detail", Value::Obj(detail.clone()))
                .with("suggested_remediation", v.remediation.clone().map(Value::Str).unwrap_or(Value::Null)),
        ));
    }
    if warnings_unmodeled {
        return Err("a verifier warning this binary does not word yet".into());
    }
    Ok(Object::new()
        .with("ok", violations.is_empty())
        .with("violations", Value::List(vs))
        .with("warnings", Value::List(warnings.iter().map(|w| Value::Str(w.clone())).collect()))
        .with("envelope_id", env_id.clone())
        .with("plan_id", plan_id.clone())
        .with("duration_ms", duration_ms)
        .with("client", "python")
        .with("fallback", Value::Null)
        .with("client_verdict", Value::Null))
}

/// A one-step plan for the per-step checks, as the oracle builds it.
fn singleton(source: &str, task: &Value, step: Object) -> Value {
    Value::Obj(
        Object::new()
            .with("id", format!("plan_{}", hex8()))
            .with("source", source)
            .with("task", task.clone())
            .with("steps", Value::List(vec![Value::Obj(step)])),
    )
}

fn isolated(step: &Object) -> Object {
    let mut c = step.clone();
    c.set("depends_on", Value::List(vec![]));
    c
}

/// What the per-step check found: ok, and its violations.
struct StepResult {
    ok: bool,
    violations: Vec<Violation>,
}

impl Supervisor<'_> {
    fn step_check(&self, step: &Object, env: &Object) -> StepResult {
        let plan = singleton("per-step-verify", env.value("task"), isolated(step));
        match (self.verify_step)(&plan, &Value::Obj(env.clone()), self.z3_timeout_ms) {
            Err(e) => StepResult {
                ok: false,
                violations: vec![Violation::plan("permissions").msg(format!("the step does not read: {e}"))],
            },
            Ok(Verified { mut violations, timeouts, .. }) => {
                let mut ok = violations.is_empty();
                if ok && !timeouts.is_empty() {
                    // A check that did not finish is a failure here (fail
                    // closed).
                    ok = false;
                    violations.push(Violation::plan("z3").msg(timeouts[0].clone()));
                }
                StepResult { ok, violations }
            }
        }
    }

    /// `Supervisor.run` with the whole-plan verification already made
    /// (`verification` is its JSON dump). `plan` and `env` are
    /// `model_dump()` values.
    pub fn run(&mut self, plan: &Object, env: &Object, verification: Object) -> Session {
        let mut sess = Session {
            id: format!("run_{}", hex8()),
            envelope_id: str_of(env, "id"),
            plan_id: str_of(plan, "id"),
            status: PENDING.into(),
            verification,
            started_at: now_iso(),
            ..Default::default()
        };
        let task = str_of(plan, "task");
        if !matches!(sess.verification.value("ok"), Value::Bool(true)) {
            sess.status = REJECTED.into();
            sess.ended_at = Some(now_iso());
            self.journal_session(&mut sess, plan, env, &task);
            return sess;
        }
        sess.status = RUNNING.into();
        let mut level_list: Option<Vec<Vec<Object>>> = None;
        let mut step_level: HashMap<String, usize> = HashMap::new();
        let got: Result<Vec<Object>, _> = if self.max_parallel > 1 {
            crate::tracejournal::dag::levels(plan).map(|lv| {
                let lv: Vec<Vec<Object>> = lv.into_iter().map(|l| l.into_iter().cloned().collect()).collect();
                let mut flat = vec![];
                for (i, l) in lv.iter().enumerate() {
                    for st in l {
                        step_level.insert(str_of(st, "id"), i);
                        flat.push(st.clone());
                    }
                }
                level_list = Some(lv);
                flat
            })
        } else {
            topo_order(plan).map(|o| o.into_iter().cloned().collect())
        };
        let ordered: Vec<Object> = match got {
            Ok(o) => o,
            Err(_) => {
                // A plan that verified has no cycle; fail closed if it has.
                sess.status = FAILED.into();
                sess.ended_at = Some(now_iso());
                self.journal_session(&mut sess, plan, env, &task);
                return sess;
            }
        };
        for ex in self.executors.values_mut() {
            ex.configure(env);
        }
        let mut completed = true;
        let mut prefetched: HashMap<String, ExecResult> = HashMap::new();
        let mut prefetched_approved: HashMap<String, Decision> = HashMap::new();
        let mut prefetched_level: Option<usize> = None;
        for step in ordered {
            let mut step = step;
            if let Some(lv) = &level_list {
                // Parallel mode: on entering a new dependency level, run its
                // independent parallel-safe steps at once. The loop still
                // approves, receipts and halts on each step in order.
                let l = step_level.get(&str_of(&step, "id")).copied().unwrap_or(0);
                if prefetched_level != Some(l) {
                    let level = lv[l].clone();
                    (prefetched, prefetched_approved) = self.prefetch(&level, env, &sess.id);
                    prefetched_level = Some(l);
                }
            }
            let pre = prefetched.get(&str_of(&step, "id")).cloned();
            if let (Some(h), true) = (self.hook.as_mut(), pre.is_none()) {
                match h.prepare(&step) {
                    Prepared::Run(next) => step = next,
                    Prepared::Skip(o) => {
                        sess.steps.push(o);
                        continue;
                    }
                    Prepared::Stop(o, status) => {
                        sess.steps.push(o);
                        sess.status = status.into();
                        completed = false;
                        break;
                    }
                }
            }
            // A prefetched step already passed its verify and ran.
            let res = if pre.is_some() { StepResult { ok: true, violations: vec![] } } else { self.step_check(&step, env) };
            if !res.ok {
                let Some(replacement) = self.on_rejection(&sess.id, &step, &res.violations, env) else {
                    let msg = match res.violations.first() {
                        Some(v) => format!("rejected: {}", v.message),
                        None => "rejected".into(),
                    };
                    sess.steps.push(Outcome {
                        step_id: str_of(&step, "id"),
                        status: "rejected_halted".into(),
                        started_at: now_iso(),
                        error: Some(msg),
                        ..Default::default()
                    });
                    sess.status = HALTED.into();
                    completed = false;
                    break;
                };
                sess.steps.push(Outcome {
                    step_id: str_of(&step, "id"),
                    status: "rejected_recomputed".into(),
                    started_at: now_iso(),
                    ..Default::default()
                });
                step = replacement;
                let again = self.step_check(&step, env);
                if !again.ok {
                    let msg = match again.violations.first() {
                        Some(v) => format!("recomputed step rejected: {}", v.message),
                        None => "recomputed step rejected".into(),
                    };
                    sess.steps.push(Outcome {
                        step_id: str_of(&step, "id"),
                        status: "rejected_halted".into(),
                        started_at: now_iso(),
                        error: Some(msg),
                        ..Default::default()
                    });
                    sess.status = HALTED.into();
                    completed = false;
                    break;
                }
            }
            if let (Some(h), true) = (self.hook.as_mut(), pre.is_none()) {
                if let Some((o, status)) = h.checked(&step) {
                    sess.steps.push(o);
                    sess.status = status.into();
                    completed = false;
                    break;
                }
            }
            let decision = match self.approval.decide(&step, env) {
                Ok(d) => d,
                Err(e) => {
                    sess.steps.push(Outcome {
                        step_id: str_of(&step, "id"),
                        status: "aborted".into(),
                        started_at: now_iso(),
                        error: Some(format!("approval error: {e}")),
                        ..Default::default()
                    });
                    sess.status = ABORTED.into();
                    completed = false;
                    break;
                }
            };
            let started = now_iso();
            if !decision.approved {
                sess.steps.push(Outcome {
                    step_id: str_of(&step, "id"),
                    status: "aborted".into(),
                    approved_by: Some(decision.approved_by.clone()),
                    started_at: started,
                    error: Some(format!("approval denied: {}", decision.reason)),
                    ..Default::default()
                });
                sess.status = ABORTED.into();
                completed = false;
                break;
            }
            if let (Some(h), true) = (self.hook.as_mut(), pre.is_none()) {
                if let Some(why) = h.started(&step, &sess.id) {
                    // The runner could not record that the step starts, so
                    // the step does not run.
                    sess.steps.push(Outcome {
                        step_id: str_of(&step, "id"),
                        status: ABORTED.into(),
                        approved_by: Some(decision.approved_by.clone()),
                        started_at: started,
                        error: Some(why),
                        ..Default::default()
                    });
                    sess.status = ABORTED.into();
                    completed = false;
                    break;
                }
            }
            let mut out = self.execute_one(&step, started, &decision, pre);
            if out.status == SUCCEEDED {
                if let Some(msg) = self.stage2(&step, &out, env) {
                    out.status = FAILED.into();
                    out.error = Some(format!("stage2 rejection: {msg}"));
                }
            }
            if let Some(h) = self.hook.as_mut() {
                out = h.finish(&step, out);
            }
            self.write_receipt(&step, &out, &sess.id);
            let failed = out.status == FAILED;
            sess.steps.push(out);
            if failed {
                sess.status = FAILED.into();
                sess.failed_step_id = Some(str_of(&step, "id"));
                completed = false;
                break;
            }
        }
        if completed {
            sess.status = SUCCEEDED.into();
        }
        if let (Some(lv), Some(l)) = (&level_list, prefetched_level) {
            // A prefetched step of the last level that ran but that the
            // loop never reached gets its outcome and receipt.
            let done: Vec<String> = sess.steps.iter().map(|o| o.step_id.clone()).collect();
            for step in &lv[l] {
                let id = str_of(step, "id");
                let (Some(r), Some(d)) = (prefetched.get(&id), prefetched_approved.get(&id)) else { continue };
                if done.contains(&id) {
                    continue;
                }
                let out = self.execute_one(step, now_iso(), d, Some(r.clone()));
                self.write_receipt(step, &out, &sess.id);
                sess.steps.push(out);
            }
        }
        sess.ended_at = Some(now_iso());
        self.check_integrity(&mut sess, plan);
        self.journal_session(&mut sess, plan, env, &task);
        sess
    }

    /// The postcondition check on a completed step: the first violation's
    /// message, or None.
    fn stage2(&self, step: &Object, out: &Outcome, env: &Object) -> Option<String> {
        let mut c = step.clone();
        let mut md = step.value("metadata").as_obj().cloned().unwrap_or_default();
        md.set("output", out.stdout.as_str());
        md.set("rc", out.rc.map(|n| py_int(n as i64)).unwrap_or(Value::Null));
        c.set("metadata", Value::Obj(md));
        let vs = crate::pathways::verify::verify_completed_step(&Value::Obj(c), &Value::Obj(env.clone()), self.strict);
        vs.into_iter().next()
    }

    fn on_rejection(&mut self, run_id: &str, step: &Object, violations: &[Violation], env: &Object) -> Option<Object> {
        let mut replacement = None;
        if let Some(fb) = self.fallback.as_mut() {
            replacement = fb(step, violations);
        }
        if let Some(j) = self.journal {
            let (action, rstep, rver) = match &replacement {
                Some((s, v)) => ("recomputed", Value::Obj(s.clone()), Value::Obj(v.clone())),
                None => ("halted", Value::Null, Value::Null),
            };
            let vs: Vec<Value> = violations
                .iter()
                .map(|v| {
                    Value::Obj(
                        Object::new()
                            .with("stage", v.stage)
                            .with("message", v.message.as_str())
                            .with("detail", Value::Obj(v.detail.clone().unwrap_or_default()))
                            .with("suggested_remediation", v.remediation.clone().map(Value::Str).unwrap_or(Value::Null)),
                    )
                })
                .collect();
            let rec = Object::new()
                .with("step", Value::Obj(step.clone()))
                .with("violations", Value::List(vs))
                .with("z3_counterexample", Value::Null)
                .with("envelope_id", env.value("id").clone())
                .with("fallback_action", action)
                .with("recomputed_step", rstep)
                .with("recomputed_verification", rver)
                .with("timestamp", now())
                .with("cache_key", env.value("cache_key").clone());
            j.write_refinement(run_id, &crate::pathways::pathway::dump_json(&Value::Obj(rec)), env.value("cache_key"));
        }
        replacement.map(|(s, _)| s)
    }

    /// `Supervisor._parallel_safe`.
    fn parallel_safe(&mut self, step: &Object) -> bool {
        if let Some(h) = self.hook.as_mut() {
            if !h.prefetchable(step) {
                return false;
            }
        }
        let kind = str_of(step, "type");
        PARALLEL_SAFE_TYPES.contains(&kind.as_str()) || self.executors.get(&kind).is_some_and(|e| e.parallel_safe())
    }

    /// `Supervisor._prefetch_independent`: a level's parallel-safe steps
    /// that pass their verify and approval, run at once (at most
    /// `max_parallel` at a time) when there are two or more. The results
    /// of the runs that ended without an executor error, and their
    /// approvals.
    fn prefetch(&mut self, level: &[Object], env: &Object, run_id: &str) -> (HashMap<String, ExecResult>, HashMap<String, Decision>) {
        let none = || (HashMap::new(), HashMap::new());
        let mut cands: Vec<Object> = vec![];
        let mut approvals: HashMap<String, Decision> = HashMap::new();
        for step in level {
            if !self.parallel_safe(step) || !self.step_check(step, env).ok {
                continue;
            }
            match self.approval.decide(step, env) {
                Ok(d) if d.approved => {
                    approvals.insert(str_of(step, "id"), d);
                    cands.push(step.clone());
                }
                _ => continue,
            }
        }
        if cands.len() < 2 {
            return none();
        }
        if let Some(h) = self.hook.as_mut() {
            // A step whose start mark fails is left to the main loop.
            cands.retain(|c| h.started(c, run_id).is_none());
            if cands.len() < 2 {
                return none();
            }
        }
        let n = cands.len();
        let mut results: Vec<Option<Result<ExecResult, String>>> = (0..n).map(|_| None).collect();
        let mut jobs: std::collections::VecDeque<(usize, super::executors::Job)> = Default::default();
        for (i, c) in cands.iter().enumerate() {
            let Some(ex) = self.executors.get_mut(&str_of(c, "type")) else { continue };
            match ex.job(c, self.step_timeout_s, self.max_output_bytes) {
                Some(j) => jobs.push_back((i, j)),
                None => results[i] = Some(ex.run(c, self.step_timeout_s, self.max_output_bytes)),
            }
        }
        let queue = std::sync::Mutex::new(jobs);
        let done = std::sync::Mutex::new(Vec::new());
        std::thread::scope(|sc| {
            for _ in 0..self.max_parallel.min(n) {
                sc.spawn(|| loop {
                    let next = queue.lock().map(|mut q| q.pop_front()).unwrap_or(None);
                    let Some((i, job)) = next else { break };
                    let r = job();
                    if let Ok(mut d) = done.lock() {
                        d.push((i, r));
                    }
                });
            }
        });
        for (i, r) in done.into_inner().unwrap_or_default() {
            results[i] = Some(r);
        }
        let mut out = HashMap::new();
        let mut approved = HashMap::new();
        for (i, c) in cands.iter().enumerate() {
            let Some(Ok(r)) = results[i].take() else { continue };
            if let Some(ex) = self.executors.get_mut(&str_of(c, "type")) {
                if ex.job_done(c, &r).is_err() {
                    continue;
                }
            }
            let id = str_of(c, "id");
            if let Some(d) = approvals.remove(&id) {
                approved.insert(id.clone(), d);
            }
            out.insert(id, r);
        }
        (out, approved)
    }

    fn execute_one(&mut self, step: &Object, started: String, d: &Decision, pre: Option<ExecResult>) -> Outcome {
        let kind = str_of(step, "type");
        let r = match (pre, self.executors.get_mut(&kind)) {
            (Some(r), _) => r,
            (None, ex) => match ex {
            None => ExecResult { rc: 1, stdout: format!("no executor for kind '{kind}'"), ..Default::default() },
            Some(ex) => match ex.run(step, self.step_timeout_s, self.max_output_bytes) {
                Ok(r) => r,
                Err(e) => {
                    return Outcome {
                        step_id: str_of(step, "id"),
                        status: FAILED.into(),
                        approved_by: Some(d.approved_by.clone()),
                        started_at: started,
                        error: Some(format!("executor error: {e}")),
                        ..Default::default()
                    }
                }
            },
            },
        };
        let status = if r.rc == 0 && !r.timed_out { SUCCEEDED } else { FAILED };
        let error = if r.timed_out {
            Some("timed out".to_string())
        } else if r.rc != 0 {
            let detail = strip(&r.stdout);
            if detail.is_empty() {
                Some(format!("exit {}", r.rc))
            } else {
                Some(format!("exit {}: {}", r.rc, head(detail, 500)))
            }
        } else {
            None
        };
        Outcome {
            step_id: str_of(step, "id"),
            status: status.into(),
            approved_by: Some(d.approved_by.clone()),
            rc: Some(r.rc),
            stdout: r.stdout,
            duration_ms: r.duration_ms,
            started_at: started,
            error,
            model_id: r.model,
            reversibility: r.reversibility,
            reversal: r.reversal,
        }
    }

    /// `Supervisor._write_step_receipt`.
    fn write_receipt(&self, step: &Object, out: &Outcome, run_id: &str) {
        let Some(j) = self.journal else { return };
        let mut evidence = Object::new()
            .with("rc", out.rc.map(|n| py_int(n as i64)).unwrap_or(Value::Null))
            .with("stdout", out.stdout.as_str())
            .with("duration_ms", out.duration_ms)
            .with("status", out.status.as_str());
        if let Some(e) = out.error.as_ref().filter(|e| !e.is_empty()) {
            evidence.set("error", e.as_str());
        }
        let mut ok = out.status == SUCCEEDED;
        let mut details = String::new();
        if let (Value::Obj(pc), true) = (step.value("postcondition"), ok) {
            let p = str_of(pc, "path");
            if !p.is_empty() {
                if evidence.get(&p).is_some() {
                    details = format!("evidence.{p} present");
                } else {
                    ok = false;
                    details = format!("postcondition expected evidence.{p}");
                }
            } else {
                details = "no structural check configured".into();
            }
        }
        let kind = str_of(step, "type");
        let rev = match &out.reversibility {
            Some(r) => r.clone(),
            None if READ_ONLY_KINDS.contains(&kind.as_str()) => "none".into(),
            None => "irreversible".into(),
        };
        let reversal_json = out.reversal.as_ref().map(|r| crate::pathways::pathway::dump_json(&Value::Obj(r.clone())));
        // A receipt that cannot be written is left out; the integrity
        // check then fails the run's integrity, as the oracle's would.
        let _ = j.append_receipt(&Receipt {
            step_id: str_of(step, "id"),
            run_id: run_id.into(),
            timestamp: now(),
            evidence_hash: super::evidence_hash(&evidence),
            evidence,
            verify_result: ok,
            verify_details: details,
            model_id: out.model_id.clone(),
            effect_class: kind,
            reversibility: rev,
            reversal_json,
        });
    }

    /// `Supervisor._check_run_integrity`.
    fn check_integrity(&self, sess: &mut Session, plan: &Object) {
        let Some(j) = self.journal else { return };
        if sess.status == REJECTED || sess.status == PENDING {
            return;
        }
        let got = match j.receipt_steps(&sess.id) {
            Ok(g) => g,
            Err(_) => {
                sess.integrity_passed = None;
                return;
            }
        };
        let mut expected: BTreeSet<String> = BTreeSet::new();
        if sess.status == SUCCEEDED {
            for st in steps(plan) {
                expected.insert(str_of(st, "id"));
            }
        } else if sess.status == FAILED && sess.failed_step_id.is_some() {
            let ordered = topo_order(plan).unwrap_or_else(|_| steps(plan));
            let failed = sess.failed_step_id.clone().unwrap_or_default();
            for st in ordered {
                let id = str_of(st, "id");
                expected.insert(id.clone());
                if id == failed {
                    break;
                }
            }
        } else if sess.status == ABORTED || sess.status == HALTED {
            for o in &sess.steps {
                if o.status == SUCCEEDED || o.status == FAILED {
                    expected.insert(o.step_id.clone());
                }
            }
        }
        // A step a runner skipped on its earlier receipt ran in another run.
        for o in &sess.steps {
            if o.status == "skipped" {
                expected.remove(&o.step_id);
            }
        }
        sess.integrity_passed = Some(expected.iter().all(|id| got.contains(id)));
    }

    fn journal_session(&mut self, sess: &mut Session, plan: &Object, env: &Object, task: &str) {
        let Some(j) = self.journal else { return };
        let failed = sess.steps.iter().find(|o| o.status == FAILED).map(|o| o.step_id.clone());
        let durations: Vec<f64> = sess.steps.iter().map(|o| o.duration_ms).collect();
        let created = now_iso();
        let trace_id = format!("{}-{}", &created[..10], hex8());
        let r = Run {
            task,
            env,
            plan,
            verification: &sess.verification,
            session: sess.dump(),
            run_id: &sess.id,
            status: &sess.status,
            failed_step_id: failed,
            total_duration_ms: crate::gateway::report::sum_floats(&durations),
        };
        match j.log_run(&r, &trace_id, &created) {
            Ok(()) => sess.trace_id = Some(trace_id),
            Err(e) => self.log_err = Some(e.to_string()),
        }
    }
}
