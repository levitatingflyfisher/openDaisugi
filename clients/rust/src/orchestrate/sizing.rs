//! `model_sizer.py`: each step sized to the cheapest capable model.

use crate::gate::pyjson::{Object, Value};
use crate::gateway::route::{estimate_difficulty, DEFAULT_CHEAP_MODEL, DEFAULT_FRONTIER_MODEL};

/// `model_sizer.ModelRung`.
#[derive(Debug, Clone, PartialEq)]
pub struct Rung {
    pub name: String,
    pub model: String,
    pub max_difficulty: f64,
    pub est_tokens: i64,
}

/// `model_sizer.ModelLadder`.
pub type Ladder = Vec<Rung>;

fn rung(name: &str, model: &str, max_difficulty: f64, est_tokens: i64) -> Rung {
    Rung { name: name.into(), model: model.into(), max_difficulty, est_tokens }
}

/// `model_sizer.build_ladder`: a local rung only when a local model is
/// named, then cheap and frontier.
pub fn build_ladder(local: &str) -> Ladder {
    let mut l = vec![];
    if !local.is_empty() {
        l.push(rung("local", local, 0.35, 1200));
    }
    l.push(rung("cheap", DEFAULT_CHEAP_MODEL, 0.65, 2000));
    l.push(rung("frontier", DEFAULT_FRONTIER_MODEL, 1.0, 4500));
    l
}

fn for_difficulty(l: &Ladder, d: f64) -> &Rung {
    l.iter().find(|r| d <= r.max_difficulty).unwrap_or(&l[l.len() - 1])
}

/// `model_sizer.StepSizing`.
#[derive(Debug, Clone)]
pub struct Sizing {
    pub step_id: String,
    pub difficulty: f64,
    pub tier: String,
    pub model: String,
    pub est_tokens: i64,
    pub downgraded: bool,
    pub affordable: bool,
}

impl Sizing {
    /// `dataclasses.asdict(sizing)`.
    pub fn dump(&self) -> Object {
        Object::new()
            .with("step_id", self.step_id.as_str())
            .with("difficulty", self.difficulty)
            .with("tier", self.tier.as_str())
            .with("model", self.model.as_str())
            .with("est_tokens", Value::Int(self.est_tokens.to_string()))
            .with("downgraded", self.downgraded)
            .with("affordable", self.affordable)
    }
}

fn type_base(kind: &str) -> f64 {
    match kind {
        "task" => 0.3,
        "shell" => 0.1,
        "file_read" => 0.05,
        "file_write" => 0.1,
        "network" => 0.1,
        "skill" => 0.0,
        "mcp" => 0.1,
        _ => 0.3,
    }
}

fn py_min(a: f64, b: f64) -> f64 {
    if b < a {
        b
    } else {
        a
    }
}

fn py_max(a: f64, b: f64) -> f64 {
    if b > a {
        b
    } else {
        a
    }
}

/// `model_sizer.estimate_step_difficulty`.
pub fn step_difficulty(step: &Object) -> f64 {
    let kind = step.value("type").as_str().unwrap_or("");
    let mut base = type_base(kind);
    if kind == "task" {
        base = py_max(base, estimate_difficulty(step.value("prompt").as_str().unwrap_or("")));
    }
    let deps = match step.value("depends_on") {
        Value::List(l) => l.len(),
        _ => 0,
    };
    base += py_min(deps as f64 * 0.05, 0.2);
    py_min(base, 1.0)
}

/// `model_sizer.size_step`. `remaining` is the budget's remaining, or None
/// with no budget.
pub fn size_step(step: &Object, l: &Ladder, remaining: Option<f64>, target: &str) -> Sizing {
    let d = step_difficulty(step);
    let mut chosen = match (!target.is_empty()).then(|| l.iter().find(|r| r.model == target)).flatten() {
        Some(r) => r.clone(),
        None => for_difficulty(l, d).clone(),
    };
    let (mut downgraded, mut affordable) = (false, true);
    if let Some(rem) = remaining {
        if chosen.est_tokens as f64 > rem {
            match l.iter().find(|r| r.est_tokens as f64 <= rem) {
                Some(cheaper) => {
                    chosen = cheaper.clone();
                    downgraded = true;
                }
                None => {
                    let cheapest = l[0].clone();
                    downgraded = chosen != cheapest;
                    chosen = cheapest;
                    affordable = false;
                }
            }
        }
    }
    let id = match step.value("id") {
        Value::Str(s) => s.clone(),
        _ => "?".into(),
    };
    Sizing {
        step_id: id,
        difficulty: d,
        tier: chosen.name,
        model: chosen.model,
        est_tokens: chosen.est_tokens,
        downgraded,
        affordable,
    }
}

/// `model_sizer.size_plan` with no budget.
pub fn size_plan(steps: &[Object], l: &Ladder) -> Vec<Sizing> {
    steps.iter().map(|s| size_step(s, l, None, "")).collect()
}
