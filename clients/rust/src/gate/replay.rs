//! `gate.replay_captures`: a captured session through the gate, offline.
//! Every captured call is decided in audit terms against one envelope and
//! comes back as the audit log record the report reads. Nothing runs and
//! nothing is written. The Go client's `gate/replay.go` is the twin.

use std::collections::HashMap;
use std::sync::OnceLock;
use std::time::Instant;

use super::decide::Decision;
use super::envelope::{from_dump, Envelope};
use super::frames;
use super::paths::{os_error, os_path, path_join};
use super::py::text::{splitlines, strip};
use super::py::PyErr;
use super::pydantic::{validation_error, LineErr};
use super::pyjson::{loads, py_repr, py_str_or_empty, py_type_name, round, LoadError, Object, Value};
use super::pymodel::{decode_utf8_strict, universal_newlines};
use super::record::Record;
use super::{catch, Fault, Runner, R};

/// How a replay ended when it gave no records.
#[derive(Debug)]
pub enum ReplayErr {
    /// The oracle raises out of the command: the last line of its
    /// traceback.
    Crash(String),
    /// A call the port cannot decide the way the oracle does.
    Refused(String),
}

/// `replay_captures(caps_path, envelope)`: the records, in order. `dump`
/// is the validated envelope, `model_dump(mode="json")`.
pub fn replay(caps_path: &str, dump: Object, env: HashMap<String, String>) -> Result<Vec<Object>, ReplayErr> {
    let home = env.get("HOME").cloned().unwrap_or_default();
    let mut r = Runner {
        env,
        home,
        wd: OnceLock::new(),
        real_cache: Default::default(),
        root: String::new(),
        default_root: String::new(),
        mode: "audit".into(),
        fmt: "claude".into(),
        session: None,
        captures: None,
        verify_timeout_s: 10.0,
        ask: false,
        ask_timeout_s: 90.0,
        checkpoints: false,
        t0: Instant::now(),
        resident: false,
        caller: Default::default(),
    };
    match r.replay_all(caps_path, from_dump(dump)) {
        Ok(recs) => Ok(recs),
        Err(Fault::Raised(e)) => Err(ReplayErr::Crash(format!("{}: {}", e.kind, e.msg))),
        Err(Fault::Undecided(w)) => Err(ReplayErr::Refused(w)),
    }
}

impl Runner {
    fn replay_all(&mut self, caps_path: &str, env: Envelope) -> R<Vec<Object>> {
        // replay_captures runs at frame 4 of the CLI: typer's main, the
        // command and replay_captures sit under evaluate_record.
        let _f = frames::at(4);
        let home = self.path_home()?;
        self.default_root = path_join(&self.data_home_at(&home), "gate");
        self.root = self.default_root.clone();
        let raw = std::fs::read(os_path("open", caps_path)?).map_err(|e| Fault::from(os_error(&e, caps_path)))?;
        let text = decode_utf8_strict(&raw).map_err(|m| Fault::from(PyErr::new("UnicodeDecodeError", m)))?;
        let text = universal_newlines(&text);
        let mut out = vec![];
        for line in splitlines(&text) {
            if strip(line).is_empty() {
                continue;
            }
            let cap = match loads(line) {
                Ok(v) => v,
                Err(LoadError::Syntax(_)) => continue,
                Err(_) => return Err(Fault::Undecided("a captured line holds JSON this binary does not read".into())),
            };
            let cap = match cap {
                Value::Obj(o) => o,
                other => {
                    // _evaluate_record reads record.get before its try block.
                    return Err(Fault::Raised(PyErr::attribute(format!(
                        "'{}' object has no attribute 'get'",
                        py_type_name(&other)
                    ))));
                }
            };
            self.t0 = Instant::now();
            let (d, tool, step, detail) = self.replay_one(&cap, &env)?;
            let mut rec = Object::new()
                .with("at", cap.value("captured_at").clone())
                .with("session_id", cap.value("session_id").clone())
                .with("tool_name", tool)
                .with("step_type", step)
                .with("detail", Value::Str(detail))
                .with("mode", Value::Str("audit".into()))
                .with("allow", Value::Bool(d.allow))
                .with("would_deny", Value::Bool(d.would_deny))
                .with("reason", Value::Str(d.reason.clone()))
                .with("elapsed_ms", Value::Float(round(d.elapsed_ms, 3)));
            if !d.word_audit.is_empty() {
                rec.set("word_audit", Value::List(d.word_audit.iter().map(|w| Value::Str(w.clone())).collect()));
            }
            out.push(rec);
        }
        Ok(out)
    }

    /// `_evaluate_record` on a capture record read from a file: the
    /// decision, and the record's raw tool name, step type and detail.
    fn replay_one(&self, cap: &Object, env: &Envelope) -> R<(Decision, Value, Value, String)> {
        let tool = cap.value("tool_name").clone();
        let step_v = cap.get("step_type").cloned();
        let mut detail = String::new();
        for k in ["command", "path", "url"] {
            let v = cap.value(k);
            if v.truthy() {
                detail = py_str_or_empty(v)?;
                break;
            }
        }
        let step_out = step_v.clone().unwrap_or(Value::Null);
        let Some(step_v) = step_v else {
            // r["step_type"] raises KeyError inside the try.
            let d = self.deny(&self.internal_error("'step_type'")?);
            return Ok((d, tool, step_out, detail));
        };
        let step = match &step_v {
            Value::Str(s) if ["shell", "file_read", "file_write", "network", "mcp"].contains(&s.as_str()) => s.clone(),
            _ => {
                let d = self.deny(&format!("could not synthesize a step for tool {}", py_repr(&tool)?));
                return Ok((d, tool, step_out, detail));
            }
        };
        let mut obj = Object::new();
        for k in ["command", "path", "url"] {
            obj.set(k, cap.value(k).clone());
        }
        let mut rec = Record {
            tool_name: match &tool {
                Value::Str(s) => s.clone(),
                _ => String::new(),
            },
            step_type: step.clone(),
            command: py_str_or_empty(cap.value("command"))?,
            path: py_str_or_empty(cap.value("path"))?,
            url: py_str_or_empty(cap.value("url"))?,
            ..Default::default()
        };
        if step == "mcp" {
            let mut errs = vec![];
            let (server, tool_v, args) = (cap.value("mcp_server"), cap.value("mcp_tool"), cap.value("arguments"));
            for (name, v) in [("server", server), ("tool", tool_v)] {
                if v.truthy() && !matches!(v, Value::Str(_)) {
                    errs.push(LineErr::about(&[name], "Input should be a valid string", "string_type", v)?);
                }
            }
            if args.truthy() && !matches!(args, Value::Obj(_)) {
                errs.push(LineErr::about(&["arguments"], "Input should be a valid dictionary", "dict_type", args)?);
            }
            if !errs.is_empty() {
                let e = validation_error("MCPStep", &errs);
                let d = self.deny(&self.internal_error(&e.msg)?);
                return Ok((d, tool, step_out, detail));
            }
            rec.mcp_server = py_str_or_empty(server)?;
            rec.mcp_tool = py_str_or_empty(tool_v)?;
            rec.arguments = match args {
                Value::Obj(o) => o.clone(),
                _ => Object::new(),
            };
            obj.set("mcp_server", Value::Str(rec.mcp_server.clone()));
            obj.set("mcp_tool", Value::Str(rec.mcp_tool.clone()));
            obj.set("arguments", Value::Obj(rec.arguments.clone()));
        }
        rec.obj = obj;
        let d = match catch(self.evaluate_record(&Object::new(), &rec, env, None, None))? {
            Ok(d) => d,
            Err(e) => return Err(Fault::Raised(e)),
        };
        Ok((d, tool, step_out, detail))
    }

    /// `_evaluate_record`'s fail-closed reason for `msg`, naming the
    /// configured client when it is not python.
    fn internal_error(&self, msg: &str) -> R<String> {
        let chosen = self.verifier_client()?;
        Ok(if chosen == "python" {
            format!("gate internal error (denied fail-closed): {msg}.")
        } else {
            format!(
                "gate internal error with verifier_client={chosen} (denied fail-closed): {msg}. \
                 Set verifier_client: python in config.yaml to rule out the client."
            )
        })
    }
}
