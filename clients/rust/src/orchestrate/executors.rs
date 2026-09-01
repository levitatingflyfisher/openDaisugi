//! The orchestrator's executors (`orchestrator.BudgetAwareDelegatingExecutor`,
//! `delegating_executor.py`, `orchestration_executors.py`).

use std::cell::RefCell;
use std::rc::Rc;
use std::time::Instant;

use super::budget::{RecordErr, Tracker};
use super::decompose::call_text;
use super::sizing::{size_step, Ladder, Sizing};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{Object, Value};
use crate::llm::Client;
use crate::supervise::{ms, str_of, ExecResult, Executor};

/// `orchestrator._task_step_prompt`.
pub fn task_prompt(step: &Object) -> String {
    match step.value("prompt") {
        Value::Str(p) if !p.is_empty() => format!("{p}\n\nComplete this subtask and respond with a direct, complete answer."),
        _ => crate::pathways::pathway::dump_json(&Value::Obj(step.clone())),
    }
}

const MAX_RETRIES: usize = 2;

/// `BudgetAwareDelegatingExecutor` over `DelegatingExecutor(json_mode=False)`:
/// each task step sized against what the budget has left, asked of its
/// model with up to two retries, and its spend recorded.
pub struct TaskExecutor {
    pub llm: Rc<RefCell<Client>>,
    pub tracker: Rc<RefCell<Tracker>>,
    pub ladder: Ladder,
    /// The realized sizing of each step run, in run order.
    pub live: Rc<RefCell<Vec<Sizing>>>,
}

impl Executor for TaskExecutor {
    fn run(&mut self, step: &Object, timeout_s: u64, max_out: usize) -> Result<ExecResult, String> {
        let pm = str_of(step, "preferred_model");
        let rem = self.tracker.borrow().remaining();
        let sz = size_step(step, &self.ladder, Some(rem), &pm);
        self.live.borrow_mut().push(sz.clone());
        if self.tracker.borrow().strict && !sz.affordable {
            return Ok(ExecResult {
                rc: 1,
                stdout: format!("budget exhausted: cannot afford step '{}' at any tier", str_of(step, "id")),
                ..Default::default()
            });
        }
        let mut sized = step.clone();
        sized.set("preferred_model", sz.model.as_str());
        let res = self.delegate(&sized, timeout_s, max_out);
        let tokens = res.tokens.unwrap_or(sz.est_tokens);
        let model = res.model.clone().unwrap_or(sz.model.clone());
        match self.tracker.borrow_mut().record(&str_of(step, "id"), &model, tokens, res.cost_usd) {
            Ok(()) | Err(RecordErr::Exceeded(_)) => Ok(res),
            Err(RecordErr::Value(m)) => Err(m),
        }
    }
}

impl TaskExecutor {
    /// `DelegatingExecutor.run`.
    fn delegate(&self, step: &Object, timeout_s: u64, max_out: usize) -> ExecResult {
        delegate(&self.llm, step, &task_prompt(step), timeout_s, max_out)
    }
}

/// `DelegatingExecutor(json_mode=False).run` with the prompt its template
/// gave: the step's preferred model, up to two retries.
pub fn delegate(llm: &Rc<RefCell<Client>>, step: &Object, prompt: &str, timeout_s: u64, max_out: usize) -> ExecResult {
    {
        let max_tokens = 256.max(max_out as i64 / 4);
        let started = Instant::now();
        let mut model = str_of(step, "preferred_model");
        if model.is_empty() {
            model = "haiku".into();
        }
        let prompt = prompt.to_string();
        let claude = llm.borrow().backend() == "claude-code";
        let mut last_err = String::new();
        let (mut tokens, mut cost) = (None, None);
        for _ in 0..=MAX_RETRIES {
            tokens = None;
            cost = None;
            let got = if claude {
                llm.borrow_mut().metered(&prompt, &model, timeout_s as f64).map(|m| {
                    tokens = m.tokens;
                    cost = m.cost;
                    m.text
                })
            } else {
                llm
                    .borrow()
                    .complete_timeout(&model, &[("user".to_string(), prompt.clone())], max_tokens, timeout_s as f64)
                    .map(|r| {
                        tokens = r.tokens;
                        r.text
                    })
            };
            match got {
                Ok(content) => {
                    return ExecResult {
                        rc: 0,
                        stdout: content,
                        duration_ms: ms(started),
                        model: Some(model),
                        tokens,
                        cost_usd: cost,
                        ..Default::default()
                    }
                }
                Err(e) => {
                    last_err = call_text(&e);
                    if last_err.is_empty() {
                        last_err = "Exception".into();
                    }
                }
            }
        }
        ExecResult {
            rc: 1,
            stdout: format!("delegating_executor: exhausted retries: {last_err}"),
            duration_ms: ms(started),
            model: Some(model),
            tokens,
            cost_usd: cost,
            ..Default::default()
        }
    }
}

/// `orchestration_executors.SkillExecutor` with no handler registered:
/// every skill step fails.
pub struct SkillExecutor;

impl Executor for SkillExecutor {
    fn run(&mut self, step: &Object, _: u64, _: usize) -> Result<ExecResult, String> {
        Ok(ExecResult { rc: 1, stdout: format!("no handler registered for skill {}", repr(&str_of(step, "skill_id"))), ..Default::default() })
    }
}

/// `orchestration_executors.MCPExecutor` with no transport.
pub struct McpExecutor;

impl Executor for McpExecutor {
    fn run(&mut self, _: &Object, _: u64, _: usize) -> Result<ExecResult, String> {
        Ok(ExecResult { rc: 1, stdout: "no MCP transport configured; pass MCPExecutor(transport=...)".into(), ..Default::default() })
    }
}
