//! `parsers.pi.PiParser._read_messages`: a pi session (JSONL v3) read into
//! the flat {role, content} messages the Claude Code parser reads. The
//! entry ids the oracle carries do not reach an episode, so they are not
//! kept here.

use super::codex::tool_use;
use super::{get, lines, load_line, Message, R};
use crate::warden::py::text::strip;
use crate::warden::pyjson::{Object, Value};

/// `_PI_TO_CLAUDE`.
fn pi_to_claude(name: &str) -> Option<&'static str> {
    Some(match name {
        "bash" => "Bash",
        "read" => "Read",
        "write" => "Write",
        "edit" => "Edit",
        "grep" => "Grep",
        "find" | "ls" => "Glob",
        _ => return None,
    })
}

/// `PiParser._assistant_blocks`.
fn assistant_blocks(content: &Value) -> Vec<Value> {
    let Value::List(list) = content else { return vec![] };
    let mut blocks = vec![];
    for b in list {
        let Value::Obj(block) = b else { continue };
        match get(block, "type").as_str() {
            Some("text") => {
                if let Value::Str(t) = get(block, "text") {
                    blocks.push(Value::Obj(Object::new().with("type", "text").with("text", t)));
                }
            }
            Some("toolCall") => {
                let Value::Str(name) = get(block, "name") else { continue };
                let mapped = match pi_to_claude(&name) {
                    Some(m) => m.to_string(),
                    None => format!("mcp__pi__{name}"),
                };
                let input = match get(block, "arguments") {
                    Value::Obj(o) => o,
                    _ => Object::new(),
                };
                blocks.push(tool_use(&mapped, input));
            }
            _ => {}
        }
    }
    blocks
}

/// `PiParser._read_messages`.
pub fn pi_messages(text: &str) -> R<Vec<Message>> {
    let mut msgs = vec![];
    for raw in lines(text) {
        let raw = strip(raw);
        if raw.is_empty() {
            continue;
        }
        let Some(Value::Obj(d)) = load_line(raw)? else { continue };
        if get(&d, "type").as_str() != Some("message") {
            continue;
        }
        let Value::Obj(msg) = get(&d, "message") else { continue };
        match get(&msg, "role").as_str() {
            Some("user") => {
                let content = msg.get("content").cloned().unwrap_or(Value::Str(String::new()));
                msgs.push(Message { role: Value::Str("user".into()), content, has_content: true });
            }
            Some("assistant") => {
                let blocks = assistant_blocks(&get(&msg, "content"));
                if !blocks.is_empty() {
                    msgs.push(Message {
                        role: Value::Str("assistant".into()),
                        content: Value::List(blocks),
                        has_content: true,
                    });
                }
            }
            _ => {}
        }
    }
    Ok(msgs)
}
