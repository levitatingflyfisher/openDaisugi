//! `opendaisugi.models.Violation`: `(stage, step)`, the normative half the
//! conformance wire compares, and the oracle's message, which `pathways
//! import` prints (VERIFICATION_FAILED names each violation). The detail
//! and the suggested remediation are carried where this port words them
//! (the permissions and DAG stages); a caller that stores a violation
//! must refuse one whose detail is None.

use crate::gate::pyjson::{Object, Value};

#[derive(Debug, Clone)]
pub struct Violation {
    pub stage: &'static str,
    pub step: Option<String>,
    pub message: String,
    /// The oracle's detail dict, None where this port does not word it.
    pub detail: Option<Object>,
    /// The oracle's suggested_remediation.
    pub remediation: Option<String>,
}

impl Violation {
    pub fn new(stage: &'static str, step: Option<String>) -> Self {
        Violation { stage, step, message: String::new(), detail: None, remediation: None }
    }
    pub fn plan(stage: &'static str) -> Self {
        Violation { stage, step: None, message: String::new(), detail: None, remediation: None }
    }
    pub fn step(stage: &'static str, step_id: impl Into<String>) -> Self {
        Violation { stage, step: Some(step_id.into()), message: String::new(), detail: None, remediation: None }
    }
    /// The same violation, with the oracle's message.
    pub fn msg(mut self, message: impl Into<String>) -> Self {
        self.message = message.into();
        self
    }

    /// The same violation, with the oracle's detail and
    /// suggested_remediation.
    pub fn with(mut self, detail: Object, remediation: Option<String>) -> Self {
        self.detail = Some(detail);
        self.remediation = remediation;
        self
    }
}

/// A detail dict from key, value pairs in order.
pub fn kv(pairs: &[(&str, Value)]) -> Object {
    let mut o = Object::new();
    for (k, v) in pairs {
        o.set(k, v.clone());
    }
    o
}
