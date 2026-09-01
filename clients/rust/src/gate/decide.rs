//! `gate.evaluate_call`, `gate._decide`, `gate._decide_patch` and
//! `gate.evaluate_record`: one parsed payload and its envelope in, one
//! decision out. Nothing here writes.

use super::check::{verify_late_reason, Violation};
use super::effects::{tier_for, Workspace, PERMANENT, SILENT};
use super::envelope::Envelope;
use super::frames::{depth, frame};
use super::py::text::repr;
use super::py::PyErr;
use super::pydantic::{check_str, non_finite, validation_error};
use super::pyjson::{py_str, Object, Value};
use super::record::{tool_name_of, Record, APPLY_PATCH_TOOL};
use super::rules::{Guard, FLOOR_REFUSAL, OPENCODE_REFUSAL, PANE_REFUSAL};
use super::search::SEARCH_REFUSAL;
use super::{catch, random_hex, Fault, Runner, R};

/// `gate.GateDecision`.
#[derive(Debug, Clone, Default)]
pub struct Decision {
    pub allow: bool,
    pub would_deny: bool,
    pub reason: String,
    pub tool_name: Option<String>,
    pub step_type: Option<String>,
    pub detail: String,
    pub elapsed_ms: f64,
    pub violations: Vec<Violation>,
    pub envelope_id: Option<String>,
    pub plan_id: Option<String>,
    pub pane_rule: bool,
    pub tier: String,
    /// An operator answered (`--ask`).
    pub ask: bool,
    /// The operator's edit of the call, when they allowed it with one.
    pub updated_input: Option<Value>,
    pub answered_by: Option<String>,
    pub who_from: Option<String>,
}

impl Decision {
    /// `<stage>: <message>` of the first violation, or the reason.
    pub fn clause(&self) -> String {
        match self.violations.first() {
            Some(v) => format!("{}: {}", v.stage, v.message),
            None => self.reason.clone(),
        }
    }

    pub fn counterexample(&self) -> Object {
        self.violations.first().and_then(|v| v.detail.clone()).unwrap_or_default()
    }
}

/// A hard-deny rule's answer. Each rule reads any error as a hit, so a
/// broken check denies.
fn hit(r: R<bool>) -> R<bool> {
    Ok(catch(r)?.unwrap_or(true))
}

fn tier_rank(t: &str) -> i32 {
    match t {
        SILENT => 0,
        super::effects::UNDOABLE => 1,
        _ => 2,
    }
}

/// `parse_apply_patch`'s heredoc: `^(?:cat\s+)?<<['"]?(\w+)['"]?\s*\n([\s\S]*?)\n\1\s*$`.
const PATCH_HEREDOC: &str = r#"^(?:cat\s+)?<<['"]?(\w+)['"]?\s*\n([\s\S]*?)\n\1\s*$"#;
const PATCH_HEADERS: &[&str] = &["*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"];

/// `hook.parse_apply_patch`: every path the patch names, or None.
fn parse_apply_patch(v: &Value) -> R<Option<Vec<String>>> {
    let _f = frame();
    let text = match v {
        Value::Str(s) => s,
        _ => return Ok(None),
    };
    let mut body = super::pystr::py_strip(text).to_string();
    let re = super::py::re::cached_compile(PATCH_HEREDOC)?;
    let cps: Vec<u32> = body.chars().map(super::py::re::pycp).collect();
    if let Some(m) = re.search_at(&cps, 0, None)? {
        if m.start == 0 {
            body = m.group(&cps, 2).unwrap_or_default();
        }
    }
    let lines: Vec<&str> = body.split('\n').collect();
    let stripped: Vec<&str> = lines.iter().map(|l| super::pystr::py_strip(l)).collect();
    let (begin, end) = match (
        stripped.iter().position(|l| *l == "*** Begin Patch"),
        stripped.iter().position(|l| *l == "*** End Patch"),
    ) {
        (Some(b), Some(e)) => (b, e),
        _ => return Ok(None),
    };
    if begin >= end {
        return Ok(None);
    }
    let mut paths = Vec::new();
    for line in &lines[begin + 1..end] {
        for head in PATCH_HEADERS {
            if let Some(rest) = line.strip_prefix(head) {
                let p = super::pystr::py_strip(rest);
                if !p.is_empty() {
                    paths.push(p.to_string());
                }
                break;
            }
        }
    }
    Ok(if paths.is_empty() { None } else { Some(paths) })
}

impl Runner {
    pub fn deny(&self, reason: &str) -> Decision {
        Decision {
            allow: self.mode == "shadow",
            would_deny: true,
            reason: reason.to_string(),
            elapsed_ms: self.elapsed_ms(),
            tier: PERMANENT.into(),
            ..Default::default()
        }
    }

    /// `gate.evaluate_call` for a payload that is an object: any error it
    /// raises is a deny.
    pub fn evaluate_call(&self, p: &Object, env: &Envelope) -> R<Decision> {
        let _f = frame();
        match catch(self.evaluate_call_body(p, env))? {
            Ok(d) => Ok(d),
            Err(e) => Ok(self.deny(&format!("gate internal error (denied fail-closed): {}", e.msg))),
        }
    }

    fn evaluate_call_body(&self, p: &Object, env: &Envelope) -> R<Decision> {
        let name_v = match tool_name_of(p) {
            None => return Ok(self.deny("no tool name in hook payload")),
            Some(v) => v.clone(),
        };
        let name = py_str(&name_v)?;
        if name_v == Value::Str(APPLY_PATCH_TOOL.into()) {
            return self.decide_patch(p, &name, env);
        }
        let rec = self.payload_to_record(p, &self.fmt)?;
        self.decide_record(p, rec.as_ref(), &name, env)
    }

    /// `gate._decide`: the hard-deny rules, then the envelope. The rules
    /// run under the verifier's budget: the oracle has no deadline there,
    /// but a host's hook timeout fails open, so a rule that runs too long
    /// denies instead (stricter than the oracle).
    pub fn decide_record(&self, p: &Object, rec: Option<&Record>, tool_name: &str, env: &Envelope) -> R<Decision> {
        let _f = frame();
        // gate.MAX_SHELL_COMMAND_CHARS: a longer command is denied unread.
        if let Some(r) = rec {
            if let (true, Value::Str(c)) = (r.step_type == "shell", r.raw("command")) {
                if c.chars().count() > super::MAX_SHELL_COMMAND_CHARS {
                    let mut d = self.deny(&format!(
                        "shell command is longer than {} characters; the gate does not read it",
                        super::MAX_SHELL_COMMAND_CHARS
                    ));
                    d.tool_name = Some(tool_name.to_string());
                    d.step_type = Some("shell".into());
                    d.detail = c.clone();
                    return Ok(d);
                }
            }
        }
        let cwd = super::pyjson::py_str_or_empty(p.value("cwd"))?;
        let owned_rec = rec.cloned();
        let owned_cwd = cwd.clone();
        let owned_p = p.clone();
        let base = depth();
        // The rules run with no deadline of their own, as in the oracle;
        // the worker's deadline bounds them.
        let refusal = self.run_unbounded(base, move |r| -> R<Option<&'static str>> {
            let rec = owned_rec.as_ref();
            if hit(r.pane_rule_hit(&owned_p, &owned_cwd, rec))? {
                return Ok(Some(PANE_REFUSAL));
            }
            if hit(r.floor_hit(&owned_cwd, rec, Guard::Floor))? {
                return Ok(Some(FLOOR_REFUSAL));
            }
            if hit(r.floor_hit(&owned_cwd, rec, Guard::Opencode))? {
                return Ok(Some(OPENCODE_REFUSAL));
            }
            if hit(r.search_above_secret_hit(&owned_p, &owned_cwd, rec))? {
                return Ok(Some(SEARCH_REFUSAL));
            }
            Ok(None)
        })?;
        if let Some(reason) = refusal {
            let mut d = Decision {
                allow: false,
                would_deny: true,
                reason: reason.into(),
                tool_name: Some(tool_name.to_string()),
                elapsed_ms: self.elapsed_ms(),
                pane_rule: true,
                tier: PERMANENT.into(),
                ..Default::default()
            };
            if let Some(rec) = rec {
                d.step_type = Some(rec.step_type.clone());
                d.detail = rec.rule_detail();
            }
            return Ok(d);
        }
        let rec = match rec {
            None => {
                let mut d = self.deny(&format!(
                    "unrecognized tool {} — not in the gate's classification map, denied by default",
                    repr(tool_name)
                ));
                d.tool_name = Some(tool_name.to_string());
                return Ok(d);
            }
            Some(r) => r,
        };
        // workspace_root runs here, in _decide, before evaluate_record: an
        // error it raises is evaluate_call's.
        let project_dir = self.env.get("CLAUDE_PROJECT_DIR").cloned();
        let cwd_v = p.value("cwd");
        let cwd_s = cwd_v.as_str().map(String::from);
        let root = self.workspace_root(cwd_s.as_deref(), project_dir.as_deref())?;
        self.evaluate_record(p, rec, env, root, cwd_s)
    }

    /// `gate._decide_patch`: one file write per path the patch names, each
    /// placed from the call's working directory.
    pub fn decide_patch(&self, p: &Object, tool_name: &str, env: &Envelope) -> R<Decision> {
        let _f = frame();
        let patch_text = match super::record::tool_input_of(p) {
            Value::Obj(inp) => inp.value("patchText").clone(),
            _ => Value::Null,
        };
        let paths = match parse_apply_patch(&patch_text)? {
            Some(ps) => ps,
            None => {
                let mut d = self.deny(
                    "apply_patch: the gate cannot read this patch, so it denies it. A patch needs Begin \
                     Patch and End Patch lines and at least one file header.",
                );
                d.tool_name = Some(tool_name.to_string());
                return Ok(d);
            }
        };
        let call_cwd = match p.value("cwd") {
            Value::Str(c) if super::paths::isabs(c) => c.clone(),
            _ => {
                let mut d = self.deny(
                    "apply_patch: the call names no absolute working directory, so the gate cannot \
                     place the patch's paths. It denies the patch.",
                );
                d.tool_name = Some(tool_name.to_string());
                return Ok(d);
            }
        };
        let mut decisions: Vec<Decision> = Vec::new();
        for raw in &paths {
            let path = super::paths::normpath(&super::paths::join2(&call_cwd, raw));
            let mut rec = Record {
                tool_name: "Write".into(),
                step_type: "file_write".into(),
                path: path.clone(),
                ..Default::default()
            };
            rec.obj.set("captured_at", Value::Null);
            rec.obj.set("session_id", super::record::safe_session_value(p.value("session_id"))?);
            rec.obj.set("tool_name", "Write");
            rec.obj.set("step_type", "file_write");
            rec.obj.set("path", path.as_str());
            rec.obj.set("content_len", 0i64);
            let j = super::record::join_of(p);
            for k in j.keys() {
                rec.obj.set(k, j.value(k).clone());
            }
            let d = self.decide_record(p, Some(&rec), tool_name, env)?;
            if d.pane_rule {
                return Ok(d);
            }
            decisions.push(d);
        }
        let mut chosen = decisions.len() - 1;
        let mut best = -1;
        for (i, d) in decisions.iter().enumerate() {
            if d.would_deny {
                let rank = tier_rank(&d.tier);
                if rank > best {
                    best = rank;
                    chosen = i;
                }
            }
        }
        let mut d = decisions.swap_remove(chosen);
        d.tool_name = Some(tool_name.to_string());
        Ok(d)
    }

    /// `hook._records_to_steps` for one record: the step pydantic builds,
    /// or the ValidationError it raises.
    fn check_step(&self, rec: &Record) -> R<()> {
        let _f = frame();
        let mut errs = Vec::new();
        let title = match rec.step_type.as_str() {
            "shell" => {
                check_str("command", &or_empty(rec.raw("command")), &mut errs)?;
                "ShellStep"
            }
            "file_read" => {
                check_str("path", &or_empty(rec.raw("path")), &mut errs)?;
                "FileReadStep"
            }
            "file_write" => {
                check_str("path", &or_empty(rec.raw("path")), &mut errs)?;
                "FileWriteStep"
            }
            "network" => {
                check_str("url", &or_empty(rec.raw("url")), &mut errs)?;
                "NetworkStep"
            }
            "mcp" => {
                // ActionPlan's after-validator (models.non_finite_error):
                // NaN or an infinity in the arguments makes the plan invalid.
                let loc = ["steps".to_string(), "0".to_string(), "arguments".to_string()];
                non_finite(&Value::Obj(rec.arguments.clone()), &loc, &mut errs)?;
                "ActionPlan"
            }
            _ => "MCPStep",
        };
        if errs.is_empty() {
            Ok(())
        } else {
            Err(Fault::Raised(validation_error(title, &errs)))
        }
    }

    /// `gate.evaluate_record` with its tier.
    fn evaluate_record(
        &self,
        p: &Object,
        rec: &Record,
        env: &Envelope,
        root: Option<Workspace>,
        call_cwd: Option<String>,
    ) -> R<Decision> {
        let _f = frame();
        let mut d = self.evaluate_record_inner(rec, env)?;
        let _ = p;
        d.tier = {
            // _tier: an allow is silent; a deny with no clause is permanent;
            // any other takes its effect class's tier. Any error is
            // permanent.
            let _t = frame();
            if !d.would_deny {
                SILENT.into()
            } else if d.violations.is_empty() {
                PERMANENT.into()
            } else {
                let class = match catch(self.effect_class(rec, root, call_cwd))? {
                    Ok(c) => c,
                    Err(_) => "unknown".into(),
                };
                tier_for(&class).into()
            }
        };
        Ok(d)
    }

    /// `gate._evaluate_record`: any error in it is a deny that names it.
    fn evaluate_record_inner(&self, rec: &Record, env: &Envelope) -> R<Decision> {
        let _f = frame();
        let detail = rec.detail();
        let with_record = |mut d: Decision| {
            d.tool_name = Some(rec.tool_name.clone());
            d.step_type = Some(rec.step_type.clone());
            d.detail = detail.clone();
            d
        };
        let outcome = catch((|| -> R<Decision> {
            self.check_step(rec)?;
            let plan_id = format!("plan_{}", random_hex(4)?);
            let (rec2, env2) = (rec.clone(), env.clone());
            let res = self.run_bounded(VERIFY_THREAD_FRAMES, move |r| r.verify_dispatch(&rec2, &env2))?;
            let (ok, vs, timeouts) = match res {
                None => return Err(Fault::Raised(PyErr::new("TimeoutError", verify_late_message(self.verify_timeout_s)))),
                Some(r) => r,
            };
            let mut d = if ok && !timeouts.is_empty() {
                // PW-15: never allow a call whose check could not finish.
                self.deny(&format!(
                    "the verifier could not finish a check in time; denied ({})",
                    timeouts.join("; ")
                ))
            } else if ok {
                Decision { allow: true, would_deny: false, reason: "verified in envelope".into(), ..Default::default() }
            } else {
                let parts: Vec<String> = vs.iter().map(|v| format!("{}: {}", v.stage, v.message)).collect();
                let summary = if parts.is_empty() { "verification failed".to_string() } else { parts.join("; ") };
                let mut d = self.deny(&summary);
                d.violations = vs;
                d
            };
            d.envelope_id = Some(env.id.clone());
            d.plan_id = Some(plan_id);
            d.elapsed_ms = self.elapsed_ms();
            Ok(d)
        })())?;
        Ok(match outcome {
            Ok(d) => with_record(d),
            Err(e) => {
                let (via, hint) = self.dispatch_words()?;
                with_record(self.deny(&format!("gate internal error{via} (denied fail-closed): {}.{hint}", e.msg)))
            }
        })
    }

    /// The words a failed verification's deny adds when config.yaml names
    /// a verifier client other than python.
    fn dispatch_words(&self) -> R<(String, String)> {
        let chosen = self.verifier_client()?;
        Ok(if chosen == "python" {
            (String::new(), String::new())
        } else {
            (
                format!(" with verifier_client={chosen}"),
                " Set verifier_client: python in config.yaml to rule out the client.".into(),
            )
        })
    }
}

/// `r.get(k) or ""`.
fn or_empty(v: &Value) -> Value {
    if v.truthy() {
        v.clone()
    } else {
        Value::Str(String::new())
    }
}

/// The Python frames on the verifier thread's stack where `_run` calls
/// `_dispatch_verify`: `_bootstrap`, `_bootstrap_inner`, `run`, `_run`.
pub const VERIFY_THREAD_FRAMES: i64 = 4;

/// `TimeoutError(f"verifier exceeded the gate's inner time budget ({timeout_s}s)")`.
fn verify_late_message(timeout_s: f64) -> String {
    let full = verify_late_reason(timeout_s);
    full.trim_start_matches("gate internal error (denied fail-closed): ").trim_end_matches('.').to_string()
}
