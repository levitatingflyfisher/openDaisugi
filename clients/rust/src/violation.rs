//! `opendaisugi.models.Violation`: `(stage, step)`, the normative half the
//! conformance wire compares, and the oracle's message, which `pathways
//! import` prints (VERIFICATION_FAILED names each violation). The detail
//! and the suggested remediation are not carried.

#[derive(Debug, Clone)]
pub struct Violation {
    pub stage: &'static str,
    pub step: Option<String>,
    pub message: String,
}

impl Violation {
    pub fn new(stage: &'static str, step: Option<String>) -> Self {
        Violation { stage, step, message: String::new() }
    }
    pub fn plan(stage: &'static str) -> Self {
        Violation { stage, step: None, message: String::new() }
    }
    pub fn step(stage: &'static str, step_id: impl Into<String>) -> Self {
        Violation { stage, step: Some(step_id.into()), message: String::new() }
    }
    /// The same violation, with the oracle's message.
    pub fn msg(mut self, message: impl Into<String>) -> Self {
        self.message = message.into();
        self
    }
}
