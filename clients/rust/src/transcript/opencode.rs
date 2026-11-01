//! `parsers.opencode.OpenCodeParser._read_messages`: one OpenCode session,
//! read from its storage tree into the flat {role, content} messages the
//! Claude Code parser reads. The session file names the session; the
//! messages and parts sit beside it under `storage/message/<sid>/` and
//! `storage/part/<messageID>/`.

use std::path::{Path, PathBuf};

use super::codex::tool_use;
use super::{get, load_line, Fail, Message, R};
use crate::warden::py::text::strip;
use crate::warden::pyjson::{Object, Value};
use crate::warden::pymodel::decode_stream;

/// `_OPENCODE_TO_CLAUDE`.
fn opencode_to_claude(name: &str) -> Option<&'static str> {
    Some(match name {
        "bash" => "Bash",
        "read" => "Read",
        "write" => "Write",
        "edit" => "Edit",
        "glob" => "Glob",
        "grep" => "Grep",
        "webfetch" => "WebFetch",
        "websearch" => "WebSearch",
        _ => return None,
    })
}

/// `_json_names`: the sorted names of the .json files in a directory, or
/// none when it is not a directory.
fn json_names(dir: &Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(dir) else { return vec![] };
    let mut names: Vec<String> = entries
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().into_string().ok())
        .filter(|n| n.ends_with(".json"))
        .filter(|n| std::fs::metadata(dir.join(n)).map(|m| m.is_file()).unwrap_or(false))
        .collect();
    names.sort();
    names
}

/// `_load_object`: the file's JSON object, or None for a file that cannot
/// be read, is not UTF-8, is not JSON or is not an object.
fn load_object(path: &Path) -> R<Option<Object>> {
    let Ok(raw) = std::fs::read(path) else { return Ok(None) };
    let (text, dec) = decode_stream(&raw);
    if dec.is_some() {
        return Ok(None);
    }
    match load_line(&text) {
        Ok(Some(Value::Obj(o))) => Ok(Some(o)),
        Ok(_) => Ok(None),
        Err(Fail::Parse(_)) => Ok(None), // a ValueError the oracle catches
        Err(e) => Err(e),
    }
}

/// `_created`: time.created as a sort key, 0 when unusable.
fn created(msg: &Object) -> f64 {
    let Value::Obj(t) = get(msg, "time") else { return 0.0 };
    match get(&t, "created") {
        Value::Int(text) => match text.parse::<i64>() {
            Ok(n) if (n as f64).abs() <= (1u64 << 53) as f64 => n as f64,
            _ => 0.0,
        },
        Value::Float(f) if f.is_finite() => f,
        _ => 0.0,
    }
}

/// `OpenCodeParser._tool_block`.
fn tool_block(part: &Object) -> Option<Value> {
    if get(part, "type").as_str() != Some("tool") {
        return None;
    }
    let Value::Str(tool) = get(part, "tool") else { return None };
    let Value::Obj(state) = get(part, "state") else { return None };
    if matches!(get(&state, "status").as_str(), Some("pending") | Some("error")) {
        return None;
    }
    let mut input = match get(&state, "input") {
        Value::Obj(o) => o,
        _ => Object::new(),
    };
    if let Value::Str(fp) = get(&input, "filePath") {
        if input.get("file_path").is_none() {
            input.set("file_path", fp);
        }
    }
    let mapped = match opencode_to_claude(&tool) {
        Some(m) => m.to_string(),
        None => format!("mcp__opencode__{tool}"),
    };
    Some(tool_use(&mapped, input))
}

/// `OpenCodeParser._read_messages` over the session file at `path`.
pub fn opencode_messages(path: &str) -> R<Vec<Message>> {
    let p = Path::new(path);
    // Path(path).parent.parent.parent
    let mut storage = p.to_path_buf();
    for _ in 0..3 {
        storage = match storage.parent() {
            Some(q) if !q.as_os_str().is_empty() => q.to_path_buf(),
            _ => PathBuf::from("."),
        };
    }
    let name = p.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    let sid = name.strip_suffix(".json").unwrap_or(&name).to_string();
    let msg_dir = storage.join("message").join(&sid);
    let names = json_names(&msg_dir);
    if names.is_empty() {
        return Err(Fail::Parse(format!("no OpenCode messages for session {sid}")));
    }
    let mut all: Vec<(f64, String, Object)> = vec![];
    for n in names {
        if let Some(d) = load_object(&msg_dir.join(&n))? {
            all.push((created(&d), n, d));
        }
    }
    all.sort_by(|a, b| a.0.partial_cmp(&b.0).unwrap_or(std::cmp::Ordering::Equal).then_with(|| a.1.cmp(&b.1)));
    let mut msgs = vec![];
    for (_, n, msg) in &all {
        // The layout names the message file by its id, so the file stem is the
        // id. The id inside the file is third-party data and never a path.
        let mid = n.strip_suffix(".json").unwrap_or(n).to_string();
        let part_dir = storage.join("part").join(&mid);
        let mut parts: Vec<Object> = vec![];
        let part_names = if matches!(mid.as_str(), "" | "." | "..") { vec![] } else { json_names(&part_dir) };
        for pn in part_names {
            if let Some(d) = load_object(&part_dir.join(&pn))? {
                parts.push(d);
            }
        }
        match get(msg, "role").as_str() {
            Some("user") => {
                let texts: Vec<String> = parts
                    .iter()
                    .filter(|p| get(p, "type").as_str() == Some("text"))
                    .filter_map(|p| match get(p, "text") {
                        Value::Str(t) => Some(t),
                        _ => None,
                    })
                    .collect();
                let text = strip(&texts.join("\n")).to_string();
                if !text.is_empty() {
                    msgs.push(Message { role: Value::Str("user".into()), content: Value::Str(text), has_content: true });
                }
            }
            Some("assistant") => {
                let blocks: Vec<Value> = parts.iter().filter_map(tool_block).collect();
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
