//! The conformance wire protocol (`docs/spec/conformance.md`): one case
//! JSON per stdin line, one verdict JSON per stdout line, flushed per
//! line, order-independent (matched by `id`).

use crate::models::{self, ActionPlan, Envelope};
use crate::shell_decompose::ShellParser;
use crate::verify;
use serde_json::{json, Value};

const CONFORMANCE_VERSION: u64 = 1;

#[allow(clippy::large_enum_variant)] // one value per stdin line; not a hot allocation path
pub enum CaseBody {
    Verify { plan: ActionPlan, envelope: Envelope, strict: Option<bool> },
    Decompose { command: String },
}

fn parse_case(v: &Value) -> Result<(String, CaseBody), String> {
    let id = v.get("id").and_then(|x| x.as_str()).ok_or("case missing 'id'")?.to_string();
    let version = v.get("v").and_then(|x| x.as_u64()).unwrap_or(1);
    if version > CONFORMANCE_VERSION {
        return Err(format!("case v={version} exceeds the highest version this client speaks ({CONFORMANCE_VERSION})"));
    }
    let kind = v.get("kind").and_then(|x| x.as_str()).ok_or("case missing 'kind'")?;
    match kind {
        "verify" => {
            let plan_v = v.get("plan").ok_or("verify case missing 'plan'")?;
            let plan = models::parse_plan(plan_v)?;
            let envelope_v = v.get("envelope").ok_or("verify case missing 'envelope'")?.clone();
            let envelope: Envelope = serde_json::from_value(envelope_v).map_err(|e| format!("envelope: {e}"))?;
            let strict = v.get("options").and_then(|o| o.get("strict")).and_then(|s| s.as_bool());
            Ok((id, CaseBody::Verify { plan, envelope, strict }))
        }
        "decompose" => {
            let command = v.get("command").and_then(|x| x.as_str()).ok_or("decompose case missing 'command'")?.to_string();
            Ok((id, CaseBody::Decompose { command: command.to_string() }))
        }
        other => Err(format!("unknown case kind {other:?}")),
    }
}

/// Process one case line, returning the verdict JSON `Value` (never
/// panics — a malformed/unprocessable case yields an `{"id", "error"}`
/// verdict per the wire contract).
pub fn handle_line(line: &str, shell_parser: &mut ShellParser) -> Value {
    let raw: Value = match serde_json::from_str(line) {
        Ok(v) => v,
        Err(e) => return json!({"id": Value::Null, "error": format!("invalid JSON: {e}")}),
    };
    let id_for_error = raw.get("id").and_then(|x| x.as_str()).map(|s| s.to_string());
    match parse_case(&raw) {
        Err(e) => json!({"id": id_for_error, "error": e}),
        Ok((id, body)) => match body {
            CaseBody::Decompose { command } => {
                let d = shell_parser.decompose(&command);
                if d.ok {
                    json!({
                        "id": id, "ok": true,
                        "heads": d.heads, "commands": d.commands,
                        "reads": d.reads, "writes": d.writes,
                    })
                } else {
                    json!({"id": id, "ok": false})
                }
            }
            CaseBody::Verify { plan, envelope, strict } => match verify::verify(&plan, &envelope, strict, shell_parser) {
                Ok(outcome) => {
                    let violations: Vec<Value> = outcome
                        .violations
                        .iter()
                        .map(|v| json!({"stage": v.stage, "step": v.step}))
                        .collect();
                    json!({"id": id, "ok": outcome.ok, "violations": violations})
                }
                Err(e) => json!({"id": id, "error": e}),
            },
        },
    }
}
