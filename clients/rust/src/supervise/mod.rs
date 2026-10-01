//! The oracle's supervised run (`opendaisugi/supervisor.py` and what it
//! calls): the plan verified before it runs, each step verified again,
//! approved, handed to its executor, the completed step checked against
//! the envelope's postconditions, a receipt written for every step that
//! ran, the receipts checked at the end, and the run written to the
//! journal.
//!
//! Steps and plans are `model_dump()` values (`Object`), the values the
//! oracle's pydantic models dump, so the journal holds what the oracle
//! writes. The Go client's `internal/supervise` is the reference.

pub mod agentic;
pub mod approval;
pub mod executors;
pub mod fallback;
pub mod supervisor;
#[cfg(test)]
pub(crate) mod tests;

use crate::gate::pyjson::{canonical_json_ascii, py_repr, Object, Value};

pub use approval::{Always, Approver, Decision, DefaultApproval};
pub use executors::{default_executors, DryRun, Executor, Shell};
pub use fallback::{recompute, Fallback};
pub use supervisor::{verification_dump, Hook, Prepared, Supervisor};

/// Run statuses (`run_session.RunStatus`).
pub const PENDING: &str = "pending";
pub const REJECTED: &str = "rejected";
pub const RUNNING: &str = "running";
pub const SUCCEEDED: &str = "succeeded";
pub const FAILED: &str = "failed";
pub const ABORTED: &str = "aborted";
pub const HALTED: &str = "halted_by_simplex";

/// `executor.ExecutorResult`.
#[derive(Debug, Clone, Default)]
pub struct ExecResult {
    pub rc: i32,
    pub stdout: String,
    pub duration_ms: f64,
    pub timed_out: bool,
    /// Set by a model-backed executor.
    pub model: Option<String>,
    pub tokens: Option<i64>,
    pub cost_usd: Option<f64>,
    /// The deed ledger's verdict, set by an executor that mutated (or
    /// refused to mutate) something.
    pub reversibility: Option<String>,
    pub reversal: Option<Object>,
}

/// `run_session.StepOutcome`.
#[derive(Debug, Clone, Default)]
pub struct Outcome {
    pub step_id: String,
    pub status: String,
    pub approved_by: Option<String>,
    pub rc: Option<i32>,
    pub stdout: String,
    pub duration_ms: f64,
    pub started_at: String,
    pub error: Option<String>,
    pub model_id: Option<String>,
    pub reversibility: Option<String>,
    pub reversal: Option<Object>,
}

/// An int as a JSON and YAML value.
pub fn py_int(n: i64) -> Value {
    Value::Int(n.to_string())
}

fn opt(s: &Option<String>) -> Value {
    match s {
        Some(s) => Value::Str(s.clone()),
        None => Value::Null,
    }
}

impl Outcome {
    /// `dataclasses.asdict(outcome)`, the reversal as its model dump. With
    /// `as_str` the reversal is what `json.dumps(default=str)` writes for
    /// the model asdict leaves in place: `str(model)`.
    pub fn dump(&self, as_str: bool) -> Object {
        let rev = match &self.reversal {
            None => Value::Null,
            Some(r) if as_str => {
                let parts: Vec<String> =
                    r.keys().iter().map(|k| format!("{k}={}", py_repr(r.value(k)).unwrap_or_default())).collect();
                Value::Str(parts.join(" "))
            }
            Some(r) => Value::Obj(r.clone()),
        };
        Object::new()
            .with("step_id", self.step_id.as_str())
            .with("status", self.status.as_str())
            .with("approved_by", opt(&self.approved_by))
            .with("rc", self.rc.map(|n| py_int(n as i64)).unwrap_or(Value::Null))
            .with("stdout", self.stdout.as_str())
            .with("duration_ms", self.duration_ms)
            .with("started_at", self.started_at.as_str())
            .with("error", opt(&self.error))
            .with("model_id", opt(&self.model_id))
            .with("reversibility", opt(&self.reversibility))
            .with("reversal", rev)
    }
}

/// `run_session.RunSession`.
#[derive(Debug, Clone, Default)]
pub struct Session {
    pub id: String,
    pub envelope_id: String,
    pub plan_id: String,
    pub status: String,
    pub verification: Object,
    pub steps: Vec<Outcome>,
    pub started_at: String,
    pub ended_at: Option<String>,
    pub trace_id: Option<String>,
    pub integrity_passed: Option<bool>,
    pub failed_step_id: Option<String>,
}

impl Session {
    /// The session as log_run writes it: asdict with the status as its
    /// value and the verification's JSON dump.
    pub fn dump(&self) -> Object {
        self.dump_as(false)
    }

    /// `_serialize_session`: the session `run --json` prints.
    pub fn json(&self) -> Object {
        self.dump_as(true)
    }

    fn dump_as(&self, as_str: bool) -> Object {
        let steps: Vec<Value> = self.steps.iter().map(|o| Value::Obj(o.dump(as_str))).collect();
        Object::new()
            .with("id", self.id.as_str())
            .with("envelope_id", self.envelope_id.as_str())
            .with("plan_id", self.plan_id.as_str())
            .with("status", self.status.as_str())
            .with("verification", Value::Obj(self.verification.clone()))
            .with("steps", Value::List(steps))
            .with("started_at", self.started_at.as_str())
            .with("ended_at", opt(&self.ended_at))
            .with("trace_id", opt(&self.trace_id))
            .with("integrity_passed", self.integrity_passed.map(Value::Bool).unwrap_or(Value::Null))
            .with("failed_step_id", opt(&self.failed_step_id))
    }
}

/// `models.compute_evidence_hash`: the SHA-256 of `json.dumps(evidence,
/// sort_keys=True, separators=(",", ":"), default=str)`.
pub fn evidence_hash(evidence: &Object) -> String {
    let text = canonical_json_ascii(&Value::Obj(evidence.clone()));
    crate::gate::sha256::hexdigest(text.as_bytes())
}

/// A step field that holds a string ("" when absent).
pub fn str_of(step: &Object, key: &str) -> String {
    step.value(key).as_str().unwrap_or("").to_string()
}

/// Eight random hex digits, as `uuid4().hex[:8]` gives them.
pub fn hex8() -> String {
    crate::gate::random_hex(4).unwrap_or_else(|_| "00000000".into())
}

/// `supervisor._now_iso`.
pub fn now_iso() -> String {
    let t = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0);
    format!("{}Z", crate::tracejournal::iso_seconds(t))
}

/// Milliseconds since `start`.
pub fn ms(start: std::time::Instant) -> f64 {
    start.elapsed().as_nanos() as f64 / 1e6
}
