//! `parsers.codex.CodexParser._read_messages`: a Codex rollout read into
//! the flat {role, content} messages the Claude Code parser reads.

use std::sync::OnceLock;

use regex::Regex;

use super::{get, lines, load_line, no_attr, or2, pystr_of, unhashable, Fail, Message, R};
use crate::gate::py::text::strip;
use crate::gate::pyjson::{py_type_name, Object, Value};
use crate::interpreter_parse::shlex_quote;

const ROLLOUT_KINDS: &[&str] = &["session_meta", "response_item", "event_msg", "turn_context", "compacted"];
const RESPONSE_ITEM_TYPES: &[&str] =
    &["message", "function_call", "function_call_output", "local_shell_call", "custom_tool_call", "web_search_call", "reasoning"];
const SHELL_TOOL_NAMES: &[&str] = &["shell", "exec_command", "container.exec"];
const SHELLS: &[&str] = &["bash", "sh", "zsh", "dash"];

fn re_patch_file() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$").expect("a fixed pattern"))
}

/// `codex._unwrap`: the rollout line peeled to (kind, item).
fn unwrap(line: &Object) -> R<Option<(String, Object)>> {
    let mut d = Value::Obj(line.clone());
    for _ in 0..4 {
        let Value::Obj(o) = d else { return Ok(None) };
        let kind_v = get(&o, "type");
        unhashable(&kind_v)?;
        let kind = kind_v.as_str().unwrap_or("").to_string();
        if ROLLOUT_KINDS.contains(&kind.as_str()) {
            if let Value::Obj(p) = get(&o, "payload") {
                return Ok(Some((kind, p)));
            }
        }
        if RESPONSE_ITEM_TYPES.contains(&kind.as_str()) {
            return Ok(Some(("response_item".into(), o)));
        }
        match get(&o, "item") {
            Value::Obj(it) => d = Value::Obj(it),
            _ => return Ok(None),
        }
    }
    Ok(None)
}

/// `_argv_to_command`: None for an argv that gives no command.
fn argv_to_command(argv: &Value) -> R<Option<String>> {
    if let Value::Str(s) = argv {
        let s = strip(s);
        return Ok((!s.is_empty()).then(|| s.to_string()));
    }
    let Value::List(list) = argv else { return Ok(None) };
    if list.is_empty() {
        return Ok(None);
    }
    let mut parts = Vec::with_capacity(list.len());
    for a in list {
        parts.push(pystr_of(a)?);
    }
    if parts.len() >= 3 && SHELLS.contains(&parts[0].as_str()) && parts[1..parts.len() - 1].iter().all(|p| p.starts_with('-')) {
        let last = strip(&parts[parts.len() - 1]);
        return Ok((!last.is_empty()).then(|| last.to_string()));
    }
    Ok(Some(parts.iter().map(|p| shlex_quote(p)).collect::<Vec<_>>().join(" ")))
}

/// `codex._text_of`.
fn text_of(content: &Value) -> String {
    match content {
        Value::Str(s) => s.clone(),
        Value::List(l) => {
            let parts: Vec<String> = l
                .iter()
                .filter_map(|b| match b {
                    Value::Obj(o) => get(o, "text").as_str().map(|s| s.to_string()),
                    _ => None,
                })
                .collect();
            strip(&parts.join("\n")).to_string()
        }
        _ => String::new(),
    }
}

fn tool_use(name: &str, input: Object) -> Value {
    Value::Obj(Object::new().with("type", "tool_use").with("name", name).with("input", input))
}

/// `_patch_write_blocks`: re.findall raises on a patch that is not a str.
fn patch_writes(p: &Value) -> R<Vec<Value>> {
    let Value::Str(patch) = p else {
        return Err(Fail::Parse(format!("expected string or bytes-like object, got '{}'", py_type_name(p))));
    };
    Ok(re_patch_file()
        .captures_iter(patch)
        .map(|c| tool_use("Write", Object::new().with("file_path", strip(&c[1])).with("content", "")))
        .collect())
}

/// `CodexParser._read_messages`.
pub fn codex_messages(text: &str) -> R<Vec<Message>> {
    let mut msgs: Vec<Message> = vec![];
    let mut last_user: Option<String> = None;
    fn add_user(msgs: &mut Vec<Message>, last_user: &mut Option<String>, t: &str) {
        let t = strip(t);
        if t.is_empty() || last_user.as_deref() == Some(t) {
            return;
        }
        *last_user = Some(t.to_string());
        msgs.push(Message { role: Value::Str("user".into()), content: Value::Str(t.into()), has_content: true });
    }
    fn add_tools(msgs: &mut Vec<Message>, blocks: Vec<Value>) {
        if !blocks.is_empty() {
            msgs.push(Message { role: Value::Str("assistant".into()), content: Value::List(blocks), has_content: true });
        }
    }
    for raw in lines(text) {
        let raw = strip(raw);
        if raw.is_empty() {
            continue;
        }
        let Some(Value::Obj(line)) = load_line(raw)? else { continue };
        let Some((kind, item)) = unwrap(&line)? else { continue };
        if kind == "event_msg" {
            if get(&item, "type").as_str() == Some("user_message") {
                match get(&item, "message") {
                    Value::Str(m) => add_user(&mut msgs, &mut last_user, &m),
                    other => add_user(&mut msgs, &mut last_user, &text_of(&other)),
                }
            }
            continue;
        }
        if kind != "response_item" {
            continue;
        }
        let itype = get(&item, "type").as_str().unwrap_or("").to_string();
        match itype.as_str() {
            "message" => {
                let t = text_of(&get(&item, "content"));
                if get(&item, "role").as_str() == Some("user") {
                    add_user(&mut msgs, &mut last_user, &t);
                } else if !t.is_empty() {
                    msgs.push(Message {
                        role: Value::Str("assistant".into()),
                        content: Value::List(vec![Value::Obj(Object::new().with("type", "text").with("text", t))]),
                        has_content: true,
                    });
                }
            }
            "function_call" => {
                let name_v = or2(get(&item, "name"), Value::Str(String::new()));
                let at = match or2(get(&item, "arguments"), Value::Str("{}".into())) {
                    Value::Str(s) => s,
                    other => {
                        return Err(Fail::Parse(format!(
                            "the JSON object must be str, bytes or bytearray, not {}",
                            py_type_name(&other)
                        )))
                    }
                };
                let Some(Value::Obj(args)) = load_line(&at)? else { continue };
                unhashable(&name_v)?;
                let name = name_v.as_str();
                if name.is_some_and(|n| SHELL_TOOL_NAMES.contains(&n)) {
                    if let Some(cmd) = argv_to_command(&get(&args, "command"))? {
                        add_tools(&mut msgs, vec![tool_use("Bash", Object::new().with("command", cmd))]);
                    }
                } else if name == Some("apply_patch") {
                    let p = or2(or2(get(&args, "input"), get(&args, "patch")), Value::Str(String::new()));
                    add_tools(&mut msgs, patch_writes(&p)?);
                } else if get(&item, "namespace").truthy() {
                    let full = format!("mcp__{}__{}", pystr_of(&get(&item, "namespace"))?, pystr_of(&name_v)?);
                    add_tools(&mut msgs, vec![tool_use(&full, args)]);
                }
            }
            "local_shell_call" => {
                let ao = match or2(get(&item, "action"), Value::Obj(Object::new())) {
                    Value::Obj(o) => o,
                    other => return Err(no_attr(&other, "get")),
                };
                if let Some(cmd) = argv_to_command(&get(&ao, "command"))? {
                    add_tools(&mut msgs, vec![tool_use("Bash", Object::new().with("command", cmd))]);
                }
            }
            "custom_tool_call" => {
                if get(&item, "name").as_str() == Some("apply_patch") {
                    add_tools(&mut msgs, patch_writes(&or2(get(&item, "input"), Value::Str(String::new())))?);
                }
            }
            "web_search_call" => {
                let ao = match or2(get(&item, "action"), Value::Obj(Object::new())) {
                    Value::Obj(o) => o,
                    other => return Err(no_attr(&other, "get")),
                };
                let q = get(&ao, "query");
                if q.truthy() {
                    add_tools(&mut msgs, vec![tool_use("WebSearch", Object::new().with("query", q))]);
                }
            }
            _ => {}
        }
    }
    Ok(msgs)
}
