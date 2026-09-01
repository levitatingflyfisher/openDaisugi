//! `hook._payload_to_record`: one hook payload as a normalized capture
//! record, with the payload's own values kept as they came, whatever their
//! JSON type, so every later step reads them as Python would.

use super::frames::frame;
use super::paths::{isabs, join2, normpath};
use super::py::PyErr;
use super::pyjson::{py_str_or_empty, py_type_name, Object, Value};
use super::pystr::{py_len, safe_session_id};
use super::{Runner, R};

/// OpenCode's apply_patch under the plugin's MCP-style name. It writes
/// files, so it is never treated as an MCP call.
pub const APPLY_PATCH_TOOL: &str = "mcp__opencode__apply_patch";

/// `hook.JOIN_KEYS`, in order.
pub const JOIN_KEYS: &[&str] =
    &["tool_use_id", "agent_id", "agent_type", "cwd", "transcript_path", "hook_event_name", "permission_mode"];

/// `hook._classify_tool`; `None` where Python returns None.
pub fn classify_tool(name: &str, fmt: &str) -> Option<&'static str> {
    if name.starts_with("mcp__") {
        return Some("mcp");
    }
    let t = match name {
        "Bash" | "shell" | "command" => Some("shell"),
        "Edit" | "Write" | "MultiEdit" => Some("file_write"),
        "Read" | "Glob" | "Grep" | "search" => Some("file_read"),
        "WebFetch" | "WebSearch" => Some("network"),
        _ => None,
    };
    if t.is_some() {
        return t;
    }
    if fmt == "pi" {
        return Some(match name {
            "bash" | "powershell" => "shell",
            "read" => "file_read",
            "write" | "edit" => "file_write",
            _ => "mcp",
        });
    }
    None
}

/// `hook._parse_mcp_tool_name`.
pub fn parse_mcp_tool_name(name: &str) -> Option<(String, String)> {
    let rest = name.strip_prefix("mcp__")?;
    let (server, tool) = rest.split_once("__")?;
    if server.is_empty() || tool.is_empty() {
        return None;
    }
    Some((server.to_string(), tool.to_string()))
}

/// One normalized capture record.
#[derive(Debug, Clone, Default)]
pub struct Record {
    /// The tool name (a str, or `_classify_tool` would have raised).
    pub tool_name: String,
    pub step_type: String,
    /// `str(record.get("command") or "")`, and the same for path and url:
    /// what the rules and the tier read.
    pub command: String,
    pub path: String,
    pub url: String,
    pub mcp_server: String,
    pub mcp_tool: String,
    pub arguments: Object,
    /// The record as Python builds it, key for key, with the payload's own
    /// values; captured_at is set when it is written.
    pub obj: Object,
}

impl Record {
    /// `record.get(k)`.
    pub fn raw(&self, k: &str) -> &Value {
        self.obj.value(k)
    }

    /// `str(record.get("command") or record.get("path") or "")`, the
    /// detail a hard-deny refusal names.
    pub fn rule_detail(&self) -> String {
        if !self.command.is_empty() {
            self.command.clone()
        } else {
            self.path.clone()
        }
    }

    /// `str(record.get("command") or record.get("path") or record.get("url")
    /// or "")`, the detail a verdict names.
    pub fn detail(&self) -> String {
        if !self.command.is_empty() {
            self.command.clone()
        } else if !self.path.is_empty() {
            self.path.clone()
        } else {
            self.url.clone()
        }
    }
}

/// The payload's tool name: tool_name, else tool, else name, by truthiness.
pub fn tool_name_of(p: &Object) -> Option<&Value> {
    ["tool_name", "tool", "name"].iter().map(|k| p.value(k)).find(|v| v.truthy())
}

/// `payload.get("tool_input") or payload.get("args") or ... or {}`.
pub fn tool_input_of(p: &Object) -> Value {
    for k in ["tool_input", "args", "input"] {
        let v = p.value(k);
        if v.truthy() {
            return v.clone();
        }
    }
    Value::Obj(Object::new())
}

/// `hook.join_keys`: the JOIN_KEYS the payload holds with a truthy value.
pub fn join_of(p: &Object) -> Object {
    let mut out = Object::new();
    for k in JOIN_KEYS {
        let v = p.value(k);
        if v.truthy() {
            out.set(k, v.clone());
        }
    }
    out
}

/// `hook._safe_session_id(raw)`: `str(raw or "")`, sanitized.
pub fn safe_session_value(v: &Value) -> R<String> {
    Ok(safe_session_id(&py_str_or_empty(v)?))
}

/// `inp.get(a) or inp.get(b) or ... or ""` on a dict.
fn first_truthy(inp: &Object, keys: &[&str]) -> Value {
    for k in keys {
        let v = inp.value(k);
        if v.truthy() {
            return v.clone();
        }
    }
    Value::Str(String::new())
}

/// `AttributeError` for `value.attr`, as Python words it.
fn no_attribute(v: &Value, attr: &str) -> PyErr {
    PyErr::attribute(format!("'{}' object has no attribute '{attr}'", py_type_name(v)))
}

/// `len(v)`.
pub fn py_len_of(v: &Value) -> Result<i64, PyErr> {
    match v {
        Value::Str(s) => Ok(py_len(s) as i64),
        Value::List(l) => Ok(l.len() as i64),
        Value::Obj(o) => Ok(o.len() as i64),
        other => Err(PyErr::type_err(format!("object of type '{}' has no len()", py_type_name(other)))),
    }
}

/// The dict a record step reads its fields from, or the AttributeError
/// Python raises on `inp.get`.
fn as_dict<'a>(inp: &'a Value) -> Result<&'a Object, PyErr> {
    match inp {
        Value::Obj(o) => Ok(o),
        other => Err(no_attribute(other, "get")),
    }
}

impl Runner {
    /// `hook._payload_to_record`. `None` where Python returns None.
    pub fn payload_to_record(&self, p: &Object, fmt: &str) -> R<Option<Record>> {
        let _f = frame();
        let name_v = match tool_name_of(p) {
            None => return Ok(None),
            Some(v) => v,
        };
        // _classify_tool calls name.startswith, which only a str has.
        let name = match name_v {
            Value::Str(s) => s.clone(),
            other => return Err(no_attribute(other, "startswith").into()),
        };
        let step = match classify_tool(&name, fmt) {
            None => return Ok(None),
            Some(s) => s,
        };
        let inp = tool_input_of(p);
        let mut rec = Record { tool_name: name.clone(), step_type: step.to_string(), ..Default::default() };
        rec.obj.set("captured_at", Value::Null);
        rec.obj.set("session_id", safe_session_value(p.value("session_id"))?);
        rec.obj.set("tool_name", name.as_str());
        rec.obj.set("step_type", step);
        match step {
            "shell" => {
                let d = as_dict(&inp)?;
                let v = first_truthy(d, &["command", "cmd"]);
                rec.command = py_str_or_empty(&v)?;
                rec.obj.set("command", v);
            }
            "file_read" | "file_write" => {
                let d = as_dict(&inp)?;
                let mut v = first_truthy(d, &["file_path", "path", "filePath", "pattern"]);
                if let (Value::Str(path), Value::Str(cwd)) = (&v, p.value("cwd")) {
                    if !path.is_empty()
                        && !isabs(path)
                        && !path.starts_with('~')
                        && !path.contains('$')
                        && isabs(cwd)
                    {
                        v = Value::Str(normpath(&join2(cwd, path)));
                    }
                }
                rec.path = py_str_or_empty(&v)?;
                rec.obj.set("path", v);
                if step == "file_write" {
                    let content = first_truthy(d, &["content", "new_string", "newString"]);
                    rec.obj.set("content_len", py_len_of(&content)?);
                }
            }
            "network" => {
                let d = as_dict(&inp)?;
                let v = first_truthy(d, &["url", "query"]);
                rec.url = py_str_or_empty(&v)?;
                rec.obj.set("url", v);
            }
            _ => {
                if name == APPLY_PATCH_TOOL {
                    return Ok(None);
                }
                let (server, tool) = match parse_mcp_tool_name(&name) {
                    Some(st) => st,
                    None if fmt == "pi" => ("pi".to_string(), name.clone()),
                    None => return Ok(None),
                };
                let args = match inp {
                    Value::Obj(o) => o,
                    _ => Object::new(),
                };
                rec.obj.set("mcp_server", server.as_str());
                rec.obj.set("mcp_tool", tool.as_str());
                rec.obj.set("arguments", args.clone());
                rec.mcp_server = server;
                rec.mcp_tool = tool;
                rec.arguments = args;
            }
        }
        let j = join_of(p);
        for k in j.keys() {
            rec.obj.set(k, j.value(k).clone());
        }
        Ok(Some(rec))
    }
}

/// The `_strings` helper of pane_rule and floor_config: every string inside
/// a value, to depth 8.
pub fn py_strings(v: &Value, depth: usize, out: &mut Vec<String>) {
    if depth > 8 {
        return;
    }
    match v {
        Value::Str(s) => out.push(s.clone()),
        Value::Obj(o) => {
            for k in o.keys() {
                py_strings(o.value(k), depth + 1, out);
            }
        }
        Value::List(l) => {
            for e in l {
                py_strings(e, depth + 1, out);
            }
        }
        _ => {}
    }
}
