//! `fallback.py`: what a run does with a step the per-step check rejects.

use std::time::Instant;

use super::{hex8, ms, str_of, supervisor::verification_dump};
use crate::gate::pyjson::{any_json, dumps_indent, Object, Value};
use crate::llm::{Call, Client, Schema};
use crate::pathways::pmodel::{steps, Id};
use crate::pathways::verify::Outcome;
use crate::pathways::PwErr;
use crate::violation::Violation;

/// A fallback handler: given a rejected step and its violations, a
/// replacement step and its verification's dump (recomputed), or None
/// (halted).
pub type Fallback = Box<dyn FnMut(&Object, &[Violation]) -> Option<(Object, Object)>>;

/// `fallback._RECOMPUTE_SYSTEM_PROMPT`.
const RECOMPUTE_SYSTEM: &str = "You are a step-repair agent. A step in an action plan was rejected by a \
safety verifier. Your job is to produce a SINGLE replacement step that \
accomplishes the same goal while respecting the envelope's constraints.\n\n\
Output a JSON object matching the step schema. Do NOT output an envelope \
or a full plan \u{2014} just one step.\n";

/// `RecomputeHandler._build_prompt`.
fn recompute_prompt(step: &Object, env: &Object, violations: &[Violation]) -> String {
    let perms = any_json(env.value("permissions"));
    let mut parts = vec![
        format!("Rejected step:\n{}", dumps_indent(&any_json(&Value::Obj(step.clone())), 2, true)),
        format!("\nEnvelope permissions:\n{}", dumps_indent(&perms, 2, true)),
    ];
    let include = matches!(env.value("fallback").as_obj().map(|f| f.value("include_refinement")), Some(Value::Bool(true)));
    if include && !violations.is_empty() {
        let lines: Vec<String> = violations.iter().map(|v| format!("- [{}] {}", v.stage, v.message)).collect();
        parts.push(format!("\nViolations:\n{}", lines.join("\n")));
    }
    parts.push("\nProduce a replacement step that accomplishes the same goal within these permissions.".into());
    parts.join("\n")
}

/// The model call and the verify a recompute makes; a test swaps them.
pub struct Hooks {
    pub preflight: Box<dyn FnMut(&str) -> bool>,
    pub structured: Box<dyn FnMut(&Call) -> Option<Object>>,
    pub verify: fn(&Value, &Value, Option<bool>, u32) -> Result<Outcome, PwErr>,
}

/// `fallback.RecomputeHandler` on a real client.
pub fn recompute(client: Client, env: Object, z3_ms: u32) -> Fallback {
    let c = std::rc::Rc::new(std::cell::RefCell::new(client));
    let c2 = c.clone();
    recompute_with(
        Hooks {
            preflight: Box::new(move |m| c.borrow().preflight_model(m).is_ok()),
            structured: Box::new(move |call| c2.borrow_mut().structured(call).ok()),
            verify: crate::pathways::verify::verify,
        },
        env,
        z3_ms,
    )
}

/// `fallback.RecomputeHandler`: one model call for a replacement of the
/// step's own type, kept only when a one-step plan of it verifies against
/// the envelope. Any failure halts; a Z3 check that does not finish there
/// halts too (fail closed).
pub fn recompute_with(mut h: Hooks, env: Object, z3_ms: u32) -> Fallback {
    Box::new(move |step: &Object, violations: &[Violation]| {
        let model = env.value("fallback").as_obj().map(|f| str_of(f, "model")).unwrap_or_default();
        let kind = str_of(step, "type");
        let i = steps().order.iter().position(|t| *t == kind)?;
        if !(h.preflight)(&model) {
            return None;
        }
        let user = recompute_prompt(step, &env, violations);
        let schema = Schema { name: steps().models[i].name, model: Id::Step(i) };
        let reply = (h.structured)(&Call::new(&model, RECOMPUTE_SYSTEM, &user, schema, 2))?;
        let task = env.value("task").clone();
        let plan = Object::new()
            .with("id", format!("plan_{}", hex8()))
            .with("source", "recompute")
            .with("task", task)
            .with("steps", Value::List(vec![Value::Obj(reply.clone())]));
        let t0 = Instant::now();
        let r = (h.verify)(&Value::Obj(plan.clone()), &Value::Obj(env.clone()), None, z3_ms).ok()?;
        if !r.violations.is_empty() || !r.timeouts.is_empty() {
            return None;
        }
        let dump = verification_dump(&r.violations, &r.warnings, r.warnings_unmodeled, env.value("id"), plan.value("id"), ms(t0)).ok()?;
        Some((reply, dump))
    })
}
