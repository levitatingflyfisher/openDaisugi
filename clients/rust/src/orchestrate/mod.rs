//! The oracle's orchestrator (`opendaisugi/orchestrator.py`,
//! `decomposer.py`, `model_sizer.py`, `budget.py`, `synthesizer.py` and
//! `delegating_executor.py`): a prompt decomposed by a model into a
//! verified plan, each step sized to the cheapest capable model within a
//! token budget, the plan run under the supervisor, and the step outputs
//! put together into one answer. The Go client's `internal/orchestrate` is
//! the reference.

pub mod budget;
pub mod decompose;
pub mod executors;
pub mod sizing;
pub mod synth;
#[cfg(test)]
mod tests;

use std::cell::RefCell;
use std::collections::{BTreeMap, HashMap};
use std::rc::Rc;
use std::time::Instant;

use crate::gate::pyjson::{Object, Value};
use crate::llm::Client;
use crate::pathways::find::Cache as FindCache;
use crate::pathways::potion;
use crate::pathways::store::Store;
use crate::supervise::{self, Always, Executor, Fallback, Session, Supervisor};
use crate::tracejournal::dag::steps as plan_steps;
use crate::tracejournal::Journal;
use crate::violation::Violation;

pub use budget::{Report, Tracker};
pub use decompose::{decompose, DecomposeErr, PlanCheck, DEFAULT_DECOMPOSE_MODEL};
pub use sizing::{build_ladder, Ladder, Sizing};

/// `Orchestrator`'s settings and `orchestrate`'s arguments, as the facade
/// and the CLI pass them.
pub struct Options<'a> {
    pub llm: Rc<RefCell<Client>>,
    pub prompt: String,
    /// The envelope's `model_dump()`.
    pub env: Object,
    pub budget: Option<i64>,
    pub strict_budget: bool,
    pub synth_llm: bool,
    /// Tier-0 reuse.
    pub store: Option<&'a Store>,
    pub matcher_key: String,
    pub potion: Option<&'a potion::Env>,
    pub journal: Option<&'a Journal>,
    pub decompose_model: String,
    pub z3_timeout_ms: u32,
    pub step_timeout_s: u64,
    pub ladder: Ladder,
    /// The envelope's fallback handler; None halts.
    pub fallback: Option<Fallback>,
    /// The shell steps' environment (with --llm's setting).
    pub shell_env: HashMap<String, String>,
    /// The whole-plan verify; a test swaps it.
    pub check: PlanCheck,
}

/// `orchestrator.OrchestrationResult`.
pub struct Outcome {
    pub prompt: String,
    pub plan: Object,
    pub session: Session,
    pub answer: String,
    pub sizings: Vec<Sizing>,
    pub budget: Report,
    pub reused: bool,
    pub used_llm: bool,
    pub log_error: Option<String>,
}

impl Options<'_> {
    /// `Orchestrator._maybe_reuse`: a distilled pathway's plan for the
    /// prompt, bound when it is typed, or None.
    fn maybe_reuse(&self) -> Option<Object> {
        let store = self.store?;
        let pe = self.potion?;
        let r = store.find(&self.prompt, &self.matcher_key, pe, None, &mut FindCache::default()).ok()?;
        let (p, _) = r.matched?;
        let typed = matches!(p.obj.value("parameters"), Value::List(l) if !l.is_empty());
        if typed {
            let env = Value::Obj(self.env.clone());
            let mut c = self.llm.borrow_mut();
            return Some(crate::envgen::bind::bind(Some(&mut *c), &p.obj, &self.prompt, &env, &self.decompose_model, self.z3_timeout_ms));
        }
        p.obj.value("plan_template").as_obj().cloned()
    }
}

/// `Orchestrator.orchestrate`. An error is a decomposition that failed, or
/// a run this binary cannot finish the oracle's way.
pub fn run(mut o: Options) -> Result<Outcome, DecomposeErr> {
    let tracker = Rc::new(RefCell::new(Tracker::new(o.budget, o.strict_budget)));
    let env_v = Value::Obj(o.env.clone());
    let mut reused = false;
    let mut plan = o.maybe_reuse();
    if let Some(p) = &plan {
        if crate::envgen::bind::plan_verifies(p, &env_v, o.z3_timeout_ms) {
            reused = true;
        } else {
            plan = None;
        }
    }
    let mut plan = match plan {
        Some(p) if reused => p,
        _ => decompose(&o.llm, &o.prompt, &o.decompose_model, &env_v, o.z3_timeout_ms, o.check)?,
    };
    let planned = {
        let ss: Vec<Object> = plan_steps(&plan).into_iter().cloned().collect();
        sizing::size_plan(&ss, &o.ladder)
    };
    if let Value::List(l) = plan.get_mut("steps").map(|v| std::mem::replace(v, Value::Null)).unwrap_or(Value::Null) {
        let mut out = vec![];
        let mut i = 0;
        for s in l {
            match s {
                Value::Obj(mut st) => {
                    if st.value("type").as_str() == Some("task") {
                        st.set("preferred_model", planned[i].model.as_str());
                    }
                    i += 1;
                    out.push(Value::Obj(st));
                }
                other => out.push(other),
            }
        }
        plan.set("steps", Value::List(out));
    }
    let live = Rc::new(RefCell::new(vec![]));
    let task = executors::TaskExecutor { llm: o.llm.clone(), tracker: tracker.clone(), ladder: o.ladder.clone(), live: live.clone() };
    let mut exs: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
    for (k, ex) in supervise::default_executors(o.shell_env.clone()) {
        exs.insert(k, ex);
    }
    exs.insert("task".into(), Box::new(task));
    exs.insert("skill".into(), Box::new(executors::SkillExecutor));
    exs.insert("mcp".into(), Box::new(executors::McpExecutor));
    let pv = Value::Obj(plan.clone());
    let t0 = Instant::now();
    let mut vr = (o.check)(&pv, &env_v, None, o.z3_timeout_ms)
        .map_err(|e| DecomposeErr::Unported(format!("the plan does not read: {e}")))?;
    let d = supervise::ms(t0);
    if vr.violations.is_empty() && !vr.timeouts.is_empty() {
        vr.violations.push(Violation::plan("z3").msg(vr.timeouts[0].clone()).with(Object::new(), None));
    }
    let verification =
        supervise::verification_dump(&vr.violations, &vr.warnings, vr.warnings_unmodeled, o.env.value("id"), plan.value("id"), d)
            .map_err(DecomposeErr::Unported)?;
    let mut sup = Supervisor::new(exs, Box::new(Always), o.journal);
    sup.z3_timeout_ms = o.z3_timeout_ms;
    sup.step_timeout_s = o.step_timeout_s;
    sup.fallback = o.fallback.take();
    let sess = sup.run(&plan, &o.env, verification);
    let log_error = sup.log_err.take();
    drop(sup);
    let realized: Vec<Sizing> = live.borrow().clone();
    let sizings: Vec<Sizing> = planned
        .into_iter()
        .map(|s| realized.iter().rev().find(|r| r.step_id == s.step_id).cloned().unwrap_or(s))
        .collect();
    let use_llm = o.synth_llm && !tracker.borrow().exhausted();
    let ss: Vec<Object> = plan_steps(&plan).into_iter().cloned().collect();
    let (answer, used_llm) = synth::synthesize(&o.llm, &o.prompt, &sess, &ss, use_llm);
    let budget = tracker.borrow().report();
    Ok(Outcome { prompt: o.prompt.clone(), plan, session: sess, answer, sizings, budget, reused, used_llm, log_error })
}
