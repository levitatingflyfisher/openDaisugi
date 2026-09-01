//! `decomposer.py`: a prompt decomposed by a model into a verified plan.

use std::cell::RefCell;
use std::rc::Rc;

use crate::gate::py::text::repr;
use crate::gate::pyjson::{Object, Value};
use crate::llm::{Call, CallError, Client, Schema};
use crate::pathways::pmodel::{steps, validate_model, Id, Mode};
use crate::pathways::verify::Outcome;
use crate::pathways::PwErr;
use crate::violation::Violation;

/// `decomposer._DEFAULT_MODEL`.
pub const DEFAULT_DECOMPOSE_MODEL: &str = "anthropic/claude-sonnet-4-20250514";

/// `decomposer.DECOMPOSER_SYSTEM_PROMPT`.
const DECOMPOSER_SYSTEM: &str = "You are a planning decomposer. Given a task, break it into the smallest useful
sequence of typed steps and return them as a DAG.

Step types:
- \"task\": a natural-language subtask for an LLM to reason about. Field: prompt.
  Use this for analysis, drafting, summarizing, deciding \u{2014} anything that is
  thinking rather than acting.
- \"skill\": invoke a reusable named skill/pathway. Field: skill_id (+ optional
  skill_input). Use when a distilled capability already covers the sub-goal.
- \"mcp\": call an external tool over MCP. Fields: server, tool (+ optional
  arguments).
- \"shell\": run one shell command (no pipes/;/&&). Field: command.
- \"file_read\"/\"file_write\": read/write one path. Fields: path (+ content).
- \"network\": one HTTP GET. Field: url.

Rules:
- Give every step a short unique id.
- Use depends_on (list of step ids) to encode ordering; independent steps may
  have no dependencies and will run in parallel-eligible order.
- Prefer \"task\" steps for reasoning and keep each step focused on one thing.
- Do not invent shell pipelines or chained commands; emit separate steps.
";

/// `decomposer._inventory_block` for the orchestrator's call: no skill
/// handlers and no declared MCP tools.
const INVENTORY: &str = "No skills or MCP tools are available in this environment. Decompose \
using ONLY 'task' steps (natural-language subtasks); do NOT emit \
'skill' or 'mcp' steps.\n\n";

/// `decomposer._TYPE_FIELDS`.
const TYPE_FIELDS: [&str; 10] = ["prompt", "skill_id", "skill_input", "server", "tool", "arguments", "command", "path", "content", "url"];

/// Why decomposition stopped.
#[derive(Debug)]
pub enum DecomposeErr {
    /// `exceptions.DecompositionError`; `no_steps` marks NoStepsError.
    Decomposition { msg: String, no_steps: bool },
    /// `LLMNotConfigured`, which the client raises before any call.
    NotConfigured(String),
    /// A plan this binary does not read the oracle's way.
    Unported(String),
}

fn failed(msg: impl Into<String>) -> DecomposeErr {
    DecomposeErr::Decomposition { msg: msg.into(), no_steps: false }
}

/// The text of a failed call, as translate_llm_error words it.
pub fn call_text(e: &CallError) -> String {
    match e {
        CallError::Model(m) => m.clone(),
        CallError::Os(e) => e.to_string(),
        CallError::Unported(w) => w.clone(),
    }
}

/// The whole-plan verify; a test swaps it.
pub type PlanCheck = fn(&Value, &Value, Option<bool>, u32) -> Result<Outcome, PwErr>;

/// `decomposer.decompose` with the orchestrator's arguments: a plan
/// verified against `env`, or why not. A Z3 check that does not finish
/// fails the decomposition (fail closed, K2-2).
pub fn decompose(c: &Rc<RefCell<Client>>, prompt: &str, model: &str, env: &Value, z3_ms: u32, check: PlanCheck) -> Result<Object, DecomposeErr> {
    if let Err(e) = c.borrow().preflight_model(model) {
        return Err(DecomposeErr::NotConfigured(call_text(&e)));
    }
    let user = format!("{INVENTORY}{prompt}");
    let schema = Schema { name: "DecomposedPlan", model: Id::DecomposedPlan };
    let reply = c
        .borrow_mut()
        .structured(&Call::new(model, DECOMPOSER_SYSTEM, &user, schema, 2))
        .map_err(|e| failed(format!("decomposition LLM call failed: {}", call_text(&e))))?;
    let items = match reply.value("steps") {
        Value::List(l) => l.clone(),
        _ => vec![],
    };
    if items.is_empty() {
        return Err(DecomposeErr::Decomposition { msg: "decomposition produced no steps".into(), no_steps: true });
    }
    let mut out_steps = vec![];
    for it in &items {
        let s = it.as_obj().cloned().unwrap_or_default();
        let mut payload = Object::new()
            .with("type", s.value("type").clone())
            .with("id", s.value("id").clone())
            .with("depends_on", s.value("depends_on").clone());
        for f in TYPE_FIELDS {
            let v = s.value(f);
            if !matches!(v, Value::Null) {
                payload.set(f, v.clone());
            }
        }
        let kind = s.value("type").as_str().unwrap_or("").to_string();
        let Some(i) = steps().order.iter().position(|t| *t == kind) else {
            return Err(DecomposeErr::Unported(format!("a step type {} this binary does not know", repr(&kind))));
        };
        match validate_model(Id::Step(i), &Value::Obj(payload), Mode::Python) {
            Ok(v) => out_steps.push(v),
            Err(e) => {
                return Err(failed(format!(
                    "step {} (type {}) is missing required fields: {}",
                    repr(s.value("id").as_str().unwrap_or("")),
                    repr(&kind),
                    e.text()
                )))
            }
        }
    }
    let plan_in = Object::new().with("source", "decomposer").with("task", prompt).with("steps", Value::List(out_steps));
    let plan = match validate_model(Id::ActionPlan, &Value::Obj(plan_in), Mode::Python) {
        Ok(Value::Obj(o)) => o,
        Ok(_) => return Err(failed("the decomposed plan does not validate")),
        Err(e) => return Err(failed(format!("the decomposed plan does not validate: {}", e.text()))),
    };
    let pv = Value::Obj(plan.clone());
    let dag = crate::pathways::verify::dag_violations(&pv)
        .map_err(|e| DecomposeErr::Unported(format!("the decomposed plan does not read: {e}")))?;
    if let Some(v) = dag.first() {
        return Err(failed(format!("decomposed plan is not a valid DAG: {}", v.message)));
    }
    let mut r = check(&pv, env, None, z3_ms).map_err(|e| DecomposeErr::Unported(format!("the decomposed plan does not read: {e}")))?;
    if r.violations.is_empty() && !r.timeouts.is_empty() {
        r.violations.push(Violation::plan("z3").msg(r.timeouts[0].clone()));
    }
    if let Some(v) = r.violations.first() {
        return Err(failed(format!(
            "decomposed plan failed verify against envelope (out of policy): [{}] {}",
            v.stage, v.message
        )));
    }
    Ok(plan)
}
