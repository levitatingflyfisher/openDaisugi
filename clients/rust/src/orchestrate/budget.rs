//! `budget.py`: the token budget a run spends.

use crate::gate::pyjson::{round, Object, Value};
use crate::gateway::report::sum_floats;

/// `budget.APPROX_USD_PER_MTOK`, in its order.
const APPROX_USD_PER_MTOK: [(&str, f64); 3] = [("opus", 22.0), ("sonnet", 6.0), ("haiku", 1.5)];

fn price_per_mtok(model: &str) -> f64 {
    let m = model.to_lowercase();
    APPROX_USD_PER_MTOK.iter().find(|(k, _)| m.contains(k)).map(|(_, p)| *p).unwrap_or(0.0)
}

struct StepCost {
    model: String,
    tokens: i64,
    cost: Option<f64>,
}

/// `budget.BudgetTracker`.
#[derive(Default)]
pub struct Tracker {
    pub total: Option<i64>,
    pub strict: bool,
    spent: i64,
    costs: Vec<StepCost>,
}

/// Why `record` raised.
#[derive(Debug)]
pub enum RecordErr {
    /// `budget.BudgetExceeded`.
    Exceeded(String),
    /// `ValueError`.
    Value(String),
}

impl Tracker {
    pub fn new(total: Option<i64>, strict: bool) -> Tracker {
        Tracker { total, strict, ..Default::default() }
    }

    /// `BudgetTracker.remaining`: infinity when unlimited.
    pub fn remaining(&self) -> f64 {
        match self.total {
            None => f64::INFINITY,
            Some(t) => ((t - self.spent) as f64).max(0.0),
        }
    }

    /// `BudgetTracker.exhausted`.
    pub fn exhausted(&self) -> bool {
        self.total.is_some() && self.remaining() <= 0.0
    }

    /// `BudgetTracker.record`: the spend counted first, then in strict
    /// mode an overrun raised.
    pub fn record(&mut self, step_id: &str, model: &str, tokens: i64, cost: Option<f64>) -> Result<(), RecordErr> {
        if tokens < 0 {
            return Err(RecordErr::Value(format!("token count must be non-negative, got {tokens}")));
        }
        self.spent += tokens;
        self.costs.push(StepCost { model: model.into(), tokens, cost });
        if let (true, Some(total)) = (self.strict, self.total) {
            if self.spent > total {
                return Err(RecordErr::Exceeded(format!(
                    "recording {tokens} tokens for step '{step_id}' pushed spend to {} > budget {total}",
                    self.spent
                )));
            }
        }
        Ok(())
    }

    /// `BudgetTracker.report`.
    pub fn report(&self) -> Report {
        let mut order: Vec<String> = vec![];
        let mut by: Vec<i64> = vec![];
        for c in &self.costs {
            match order.iter().position(|m| *m == c.model) {
                Some(i) => by[i] += c.tokens,
                None => {
                    order.push(c.model.clone());
                    by.push(c.tokens);
                }
            }
        }
        let mut by_model = Object::new();
        for (m, t) in order.iter().zip(&by) {
            by_model.set(m, Value::Int(t.to_string()));
        }
        let (approx, approx_f) = if order.is_empty() {
            // sum() of nothing is the int 0, and round keeps it an int.
            (Value::Int("0".into()), 0.0)
        } else {
            let terms: Vec<f64> = order.iter().zip(&by).map(|(m, t)| price_per_mtok(m) * *t as f64 / 1_000_000.0).collect();
            let f = round(sum_floats(&terms), 4);
            (Value::Float(f), f)
        };
        let measured: Vec<f64> = self.costs.iter().filter_map(|c| c.cost).collect();
        let measured = if measured.is_empty() { None } else { Some(round(sum_floats(&measured), 6)) };
        Report {
            total: self.total,
            spent: self.spent,
            remaining: self.total.map(|_| self.remaining() as i64),
            step_count: self.costs.len(),
            by_model,
            approx,
            approx_f,
            measured,
        }
    }
}

/// `budget.BudgetReport`.
pub struct Report {
    pub total: Option<i64>,
    pub spent: i64,
    pub remaining: Option<i64>,
    pub step_count: usize,
    pub by_model: Object,
    pub approx: Value,
    pub approx_f: f64,
    pub measured: Option<f64>,
}

impl Report {
    /// `dataclasses.asdict(report)`.
    pub fn dump(&self) -> Object {
        let int = |n: Option<i64>| n.map(|n| Value::Int(n.to_string())).unwrap_or(Value::Null);
        Object::new()
            .with("total", int(self.total))
            .with("spent", Value::Int(self.spent.to_string()))
            .with("remaining", int(self.remaining))
            .with("step_count", Value::Int(self.step_count.to_string()))
            .with("by_model", Value::Obj(self.by_model.clone()))
            .with("approx_cost_usd", self.approx.clone())
            .with("measured_cost_usd", self.measured.map(Value::Float).unwrap_or(Value::Null))
    }
}
