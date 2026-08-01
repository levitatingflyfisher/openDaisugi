//! The normative half of `opendaisugi.models.Violation`: `(stage, step)`.
//! Everything else on the Python `Violation` (message, detail, suggested
//! remediation) is informative per the wire spec and is never compared, so
//! the Rust port never bothers to construct it.

#[derive(Debug, Clone)]
pub struct Violation {
    pub stage: &'static str,
    pub step: Option<String>,
}

impl Violation {
    pub fn new(stage: &'static str, step: Option<String>) -> Self {
        Violation { stage, step }
    }
    pub fn plan(stage: &'static str) -> Self {
        Violation { stage, step: None }
    }
    pub fn step(stage: &'static str, step_id: impl Into<String>) -> Self {
        Violation { stage, step: Some(step_id.into()) }
    }
}
