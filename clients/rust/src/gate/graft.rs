//! `gate._maybe_graft` and `gate._decide_delegate_call`: the large-read
//! graft and the gate's check of the delegate MCP call.

use num_bigint::BigInt;

use super::decide::Decision;
use super::effects::{Workspace, PERMANENT};
use super::envelope::Envelope;
use super::frames::frame;
use super::paths::{isabs, normpath, path_parent};
use super::pyjson::{Object, Value};
use super::record::{join_of, safe_session_value, tool_input_of, tool_name_of, Record};
use super::{Runner, R};
use crate::delegate;

fn router_envelope(env: &Envelope) -> delegate::Envelope {
    delegate::Envelope {
        stakes: env.stakes.clone(),
        network: env.network,
        network_hosts: env.network_hosts.clone(),
    }
}

impl Runner {
    fn route_delegate(&self, env: &Envelope, allow_remote: bool) -> delegate::Route {
        let get = |k: &str| self.env.get(k).cloned();
        delegate::route_delegate(&path_parent(&self.root), Some(&router_envelope(env)), allow_remote, &get)
    }

    /// The one-field record `_decide_delegate_call` builds.
    fn synthetic_record(&self, p: &Object, tool: &str, step: &str, key: &str, value: &str) -> R<Record> {
        let mut obj = Object::new();
        obj.set("captured_at", Value::Null);
        obj.set("session_id", safe_session_value(p.value("session_id"))?);
        obj.set("tool_name", tool);
        obj.set("step_type", step);
        obj.set(key, value);
        let j = join_of(p);
        for k in j.keys() {
            obj.set(k, j.value(k).clone());
        }
        let mut rec = Record { tool_name: tool.into(), step_type: step.into(), obj, ..Default::default() };
        if key == "path" {
            rec.path = value.into();
        } else {
            rec.url = value.into();
        }
        Ok(rec)
    }

    /// `gate._decide_delegate_call`: an allowed delegate MCP call must also
    /// pass as a read of its absolute path, and as a network send when the
    /// router's worker is remote.
    pub fn decide_delegate_call(
        &self,
        p: &Object,
        rec: &Record,
        d: Decision,
        env: &Envelope,
        root: Option<Workspace>,
        call_cwd: Option<String>,
    ) -> R<Decision> {
        let _f = frame();
        let tool_name = rec.tool_name.clone();
        let path = match rec.arguments.value("path") {
            Value::Str(s) if isabs(s) => s.clone(),
            other => {
                let mut out = self.deny(
                    "delegate: the path must be an absolute path, so the gate checks the file the delegate reads",
                );
                out.tool_name = Some(tool_name);
                out.step_type = Some("mcp".into());
                out.detail = match other {
                    Value::Str(s) => s.clone(),
                    _ => String::new(),
                };
                return Ok(out);
            }
        };
        if env.stakes == "physical" {
            let mut out = self.deny("delegate: the delegate is refused under physical stakes");
            out.tool_name = Some(tool_name);
            out.step_type = Some("mcp".into());
            out.detail = path;
            return Ok(out);
        }
        let norm = normpath(&path);
        let read = self.synthetic_record(p, "Read", "file_read", "path", &norm)?;
        let mut got = self.evaluate_record(p, &read, env, root.clone(), call_cwd.clone())?;
        if got.would_deny {
            got.tool_name = Some(tool_name);
            got.reason = format!("delegate reads {norm}: {}", got.reason);
            return Ok(got);
        }
        let allow_remote = delegate::acting_rule(&self.root).is_some_and(|r| r.allow_remote);
        let rt = self.route_delegate(env, allow_remote);
        if rt.ok && rt.tier.as_deref() == Some("remote") {
            let url = rt.url.clone().unwrap_or_default();
            let send = self.synthetic_record(p, "WebFetch", "network", "url", &url)?;
            let mut got = self.evaluate_record(p, &send, env, root, call_cwd)?;
            if got.would_deny {
                got.tool_name = Some(tool_name);
                got.reason = format!("delegate sends {norm} to {}: {}", rt.host.unwrap_or_default(), got.reason);
                got.tier = PERMANENT.into();
                return Ok(got);
            }
        }
        Ok(d)
    }

    /// `gate._maybe_graft`: a deny_redirect graft on an allowed whole read
    /// of a large file.
    pub fn maybe_graft(&self, p: &Object, d: Decision, env: &Envelope) -> R<Decision> {
        let _f = frame();
        if self.fmt != "claude" || !d.allow || d.would_deny {
            return Ok(d);
        }
        let is_read = matches!(tool_name_of(p), Some(Value::Str(s)) if s == "Read");
        if !is_read || d.step_type.as_deref() != Some("file_read") {
            return Ok(d);
        }
        let Some(rule) = delegate::acting_rule(&self.root) else {
            return Ok(d);
        };
        let min_lines = rule.min_lines_big();
        if let Value::Obj(inp) = tool_input_of(p) {
            if let Value::Int(t) = inp.value("limit") {
                let n = delegate::big(t);
                if n > BigInt::from(0) && n <= min_lines {
                    return Ok(d);
                }
            }
        }
        let Some(rec) = self.payload_to_record(p, &self.fmt)? else {
            return Ok(d);
        };
        let path = match rec.raw("path") {
            Value::Str(s) if isabs(s) => s.clone(),
            _ => return Ok(d),
        };
        let m = match delegate::measure(&path) {
            Ok(m) if BigInt::from(m.lines) > min_lines => m,
            _ => return Ok(d),
        };
        let mut graft = rule
            .as_object()
            .with("lines", Value::Int(m.lines.to_string()))
            .with("file_lines_over", Value::Int(rule.min_lines.clone()));
        let rt = self.route_delegate(env, rule.allow_remote);
        let mut out = d;
        if !rt.ok {
            graft.set("applied", false);
            graft.set("why", rt.reason.as_str());
            out.graft = Some(graft);
            return Ok(out);
        }
        graft.set("worker", Value::Obj(rt.as_object()));
        // The envelope must allow the delegate call the reason names, or
        // the model would only meet a second deny.
        let arguments = Object::new().with("path", path.as_str());
        let mut obj = Object::new();
        obj.set("captured_at", Value::Null);
        obj.set("session_id", safe_session_value(p.value("session_id"))?);
        obj.set("tool_name", delegate::TOOL);
        obj.set("step_type", "mcp");
        obj.set("mcp_server", delegate::SERVER);
        obj.set("mcp_tool", delegate::NAME);
        obj.set("arguments", Value::Obj(arguments.clone()));
        let j = join_of(p);
        for k in j.keys() {
            obj.set(k, j.value(k).clone());
        }
        let call = Record {
            tool_name: delegate::TOOL.into(),
            step_type: "mcp".into(),
            mcp_server: delegate::SERVER.into(),
            mcp_tool: delegate::NAME.into(),
            arguments,
            obj,
            ..Default::default()
        };
        let call_cwd = p.value("cwd").as_str().map(String::from);
        let got = self.evaluate_record(p, &call, env, None, call_cwd)?;
        if got.would_deny {
            graft.set("applied", false);
            graft.set("why", format!("the envelope would not allow the delegate tool: {}", got.reason));
            out.graft = Some(graft);
            return Ok(out);
        }
        if rule.state != "active" {
            graft.set("applied", false);
            graft.set("why", "the rule is in audit: the read is not denied");
            out.graft = Some(graft);
            return Ok(out);
        }
        if self.mode == "audit" {
            // An audit-mode gate never denies for cost (RP-4).
            graft.set("applied", false);
            graft.set("why", "the gate is in audit mode: the read is not denied");
            out.graft = Some(graft);
            return Ok(out);
        }
        graft.set("applied", true);
        graft.set("why", Value::Null);
        out.allow = false;
        out.would_deny = false;
        out.reason = redirect_reason(m.lines, &rule.min_lines);
        out.graft = Some(graft);
        Ok(out)
    }
}

/// `gate._graft_redirect_reason`.
fn redirect_reason(lines: usize, min_lines: &str) -> String {
    format!(
        "this file has {lines} lines, over the {min_lines}-line threshold for a whole read. Call the delegate \
         tool ({}) with the file's absolute path and your question: a worker model reads the file and returns \
         exact quotes. For exact text before an edit, read the part you need with Read and a limit of at most \
         {min_lines} lines",
        delegate::TOOL
    )
}
