//! The model behind `claude -p`: the prompt on stdin, a neutral working
//! directory, `--output-format json` for one envelope with usage, and
//! sprig's text protocol inside the envelope's result: one fenced
//! ```sprig-tool JSON block calls a tool, plain text finishes.

use crate::agent::{Message, Model};
use crate::gate::ToolCall;
use crate::gostr::{index, trim_space, truncate_runes};
use crate::json::Map;
use crate::osx::Run;
use crate::session::Usage;
use std::time::Duration;

pub struct ClaudeCodeModel {
    pub binary: Vec<u8>,
    pub model: String,
    pub timeout: Duration,
}

impl ClaudeCodeModel {
    pub fn new() -> Self {
        ClaudeCodeModel {
            binary: b"claude".to_vec(),
            model: "haiku".into(),
            timeout: Duration::from_secs(120),
        }
    }
}

impl Default for ClaudeCodeModel {
    fn default() -> Self {
        Self::new()
    }
}

const PROMPT_HEAD: &[&str] = &[
    "You are sprig, a minimal coding agent. You have four tools:\n",
    "  read  {\"path\"}          \u{2014} return a file's contents\n",
    "  write {\"path\",\"content\"} \u{2014} write a file\n",
    "  edit  {\"path\",\"old\",\"new\"} \u{2014} replace a unique string\n",
    "  bash  {\"cmd\"}           \u{2014} run a shell command\n\n",
    "To CALL a tool, reply with ONLY this and nothing else:\n",
    "```sprig-tool\n{\"tool\":\"<name>\",\"input\":{...}}\n```\n",
    "To FINISH, reply with your final answer as plain text (no block).\n\n",
    "Example \u{2014} call a tool:\n",
    "```sprig-tool\n{\"tool\":\"read\",\"input\":{\"path\":\"main.go\"}}\n```\n",
    "Example \u{2014} finish (plain text, no block):\n",
    "main.go defines three functions: New, Run, and Close.\n\n",
    "Conversation so far:\n",
];

/// The Usage fields of a JSON usage object.
pub fn usage_fields(d: &mut crate::json::Dec, v: &crate::json::Node, u: &mut Usage) {
    let names = [
        "input_tokens",
        "cache_read_input_tokens",
        "cache_creation_input_tokens",
        "output_tokens",
    ];
    d.object(v, "", USAGE_TYPE, &names, |d, i, v| match i {
        0 => d.int(v, &mut u.fresh),
        1 => d.int(v, &mut u.cache_read),
        2 => d.int(v, &mut u.cache_write),
        _ => d.int(v, &mut u.out),
    });
}

/// Go's type text for the anonymous usage struct.
pub const USAGE_TYPE: &str = r#"struct { InputTokens int "json:\"input_tokens\""; CacheReadInputTokens int "json:\"cache_read_input_tokens\""; CacheCreationInputTokens int "json:\"cache_creation_input_tokens\""; OutputTokens int "json:\"output_tokens\"" }"#;

/// The fences a model reaches for, and whether the tool must be known.
const TOOL_FENCES: &[(&[u8], bool)] = &[
    (b"```sprig-tool", false),
    (b"```tool", false),
    (b"```json", true),
];

const KNOWN_TOOLS: &[&str] = &["read", "write", "edit", "bash"];

/// The first fenced block that decodes to a valid call.
fn parse_fenced_call(text: &[u8]) -> Option<ToolCall> {
    for (tag, require_known) in TOOL_FENCES {
        let Some(start) = index(text, tag) else {
            continue;
        };
        let rest = &text[start + tag.len()..];
        let Some(end) = index(rest, b"```") else {
            continue;
        };
        if let Some(c) = decode_call(&rest[..end], *require_known) {
            return Some(c);
        }
    }
    None
}

/// A JSON call object, {"tool","input"}.
fn decode_call(s: &[u8], require_known: bool) -> Option<ToolCall> {
    let mut tool = String::new();
    let mut input: Option<Map> = None;
    let r = crate::json::unmarshal(trim_space(s), |d, n| {
        d.object(
            n,
            "",
            "struct { Tool string; Input map[string]interface {} }",
            &["tool", "input"],
            |d, i, v| match i {
                0 => d.string(v, &mut tool),
                _ => d.map_any(v, &mut input),
            },
        )
    });
    if r.is_err() || tool.is_empty() {
        return None;
    }
    if require_known && !KNOWN_TOOLS.contains(&tool.as_str()) {
        return None;
    }
    Some(ToolCall {
        id: String::new(),
        name: tool,
        input: Some(input.unwrap_or_default()),
    })
}

/// The `claude -p` argv, without the prompt.
pub fn claude_args(binary: &[u8], model: &str) -> Vec<Vec<u8>> {
    let binary = if binary.is_empty() {
        b"claude".as_slice()
    } else {
        binary
    };
    let mut args: Vec<Vec<u8>> = vec![
        binary.to_vec(),
        b"-p".to_vec(),
        b"--output-format".to_vec(),
        b"json".to_vec(),
        b"--allowedTools".to_vec(),
        Vec::new(),
    ];
    if !model.is_empty() {
        args.push(format!("--model={model}").into_bytes());
    }
    args
}

/// The conversation and the tool protocol in one prompt.
pub fn format_prompt(history: &[Message]) -> Vec<u8> {
    let mut b: Vec<u8> = PROMPT_HEAD.concat().into_bytes();
    for msg in history {
        let role = if msg.role == "tool" {
            "tool-result"
        } else {
            msg.role
        };
        let text = if msg.text.is_empty() && !msg.calls.is_empty() {
            format!("(called {})", msg.calls[0].name).into_bytes()
        } else {
            msg.text.clone()
        };
        b.extend_from_slice(format!("[{role}] ").as_bytes());
        b.extend_from_slice(&text);
        b.push(b'\n');
    }
    b.extend_from_slice(b"\nYour reply:");
    b
}

/// The envelope's result text and usage, or why it is unreadable.
pub fn parse_envelope(raw: &[u8]) -> Result<(Vec<u8>, Usage), Vec<u8>> {
    let mut typ = String::new();
    let mut is_error = false;
    let mut result = String::new();
    let mut u = Usage::default();
    let r = crate::json::unmarshal(raw, |d, n| {
        d.object(
            n,
            "claudeCLIEnvelope",
            "sprig.claudeCLIEnvelope",
            &["type", "is_error", "result", "usage"],
            |d, i, v| match i {
                0 => d.string(v, &mut typ),
                1 => d.boolean(v, &mut is_error),
                2 => d.string(v, &mut result),
                _ => usage_fields(d, v, &mut u),
            },
        )
    });
    if r.is_err() || typ != "result" {
        let mut e = b"claude -p --output-format json gave an unreadable envelope: ".to_vec();
        e.extend_from_slice(&truncate_runes(raw, 200));
        return Err(e);
    }
    if is_error {
        let mut e = b"claude -p reported is_error: ".to_vec();
        e.extend_from_slice(&truncate_runes(result.as_bytes(), 200));
        return Err(e);
    }
    Ok((trim_space(result.as_bytes()).to_vec(), u))
}

/// A reply as a tool call, if it holds one, else a final answer.
pub fn parse_response(text: &[u8]) -> Message {
    let call = parse_fenced_call(text).or_else(|| decode_call(text, true));
    match call {
        Some(c) => Message {
            role: "assistant",
            calls: vec![c],
            ..Default::default()
        },
        None => Message {
            role: "assistant",
            text: trim_space(text).to_vec(),
            ..Default::default()
        },
    }
}

impl Model for ClaudeCodeModel {
    fn next(&mut self, history: &[Message]) -> Result<Message, Vec<u8>> {
        let prompt = format_prompt(history);
        let argv = claude_args(&self.binary, &self.model);
        let tmp = crate::osx::temp_dir();
        let timeout = if self.timeout.is_zero() {
            Duration::from_secs(120)
        } else {
            self.timeout
        };
        let ran = Run {
            argv: &argv,
            stdin: Some(&prompt),
            combined: false,
            dir: Some(&tmp),
            timeout: Some(timeout),
        }
        .run();
        if let Some(err) = ran.err {
            let err = if ran.timed_out {
                "context deadline exceeded".to_string()
            } else {
                err
            };
            return Err(format!("claude -p failed (quota/auth/binary): {err}").into_bytes());
        }
        let (text, usage) = parse_envelope(trim_space(&ran.stdout))?;
        let mut msg = parse_response(&text);
        msg.model = self.model.clone();
        msg.usage = usage;
        Ok(msg)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::json::Any;

    #[test]
    fn args_and_prompt() {
        let a = claude_args(b"", "m x");
        let want: Vec<&[u8]> = vec![
            b"claude",
            b"-p",
            b"--output-format",
            b"json",
            b"--allowedTools",
            b"",
            b"--model=m x",
        ];
        assert_eq!(a, want);
        assert_eq!(claude_args(b"c", "").len(), 6);
        let h = vec![
            Message {
                role: "user",
                text: b"task".to_vec(),
                ..Default::default()
            },
            Message {
                role: "assistant",
                calls: vec![ToolCall {
                    name: "read".into(),
                    ..Default::default()
                }],
                ..Default::default()
            },
            Message {
                role: "tool",
                text: b"out\xff".to_vec(),
                ..Default::default()
            },
        ];
        let p = format_prompt(&h);
        assert!(p.starts_with(b"You are sprig, a minimal coding agent. You have four tools:\n"));
        assert!(p.ends_with(b"Conversation so far:\n[user] task\n[assistant] (called read)\n[tool-result] out\xff\n\nYour reply:"));
    }

    #[test]
    fn envelopes() {
        let (t, u) = parse_envelope(
            br#"{"type":"result","result":"  hi  ","usage":{"input_tokens":1,"output_tokens":4}}"#,
        )
        .unwrap();
        assert_eq!(t, b"hi");
        assert_eq!(
            u,
            Usage {
                fresh: 1,
                cache_read: 0,
                cache_write: 0,
                out: 4
            }
        );
        assert_eq!(
            parse_envelope(b"{}").unwrap_err(),
            b"claude -p --output-format json gave an unreadable envelope: {}"
        );
        assert_eq!(
            parse_envelope(br#"{"type":"result","is_error":true,"result":"bad"}"#).unwrap_err(),
            b"claude -p reported is_error: bad"
        );
        let long = "é".repeat(250);
        let e = parse_envelope(long.as_bytes()).unwrap_err();
        let want = format!(
            "claude -p --output-format json gave an unreadable envelope: {}",
            "é".repeat(200)
        );
        assert_eq!(e, want.as_bytes());
    }

    #[test]
    fn responses() {
        let m = parse_response(b"x\n```sprig-tool\n{\"tool\":\"zap\",\"input\":{\"a\":1}}\n```");
        assert_eq!(m.calls[0].name, "zap");
        assert_eq!(
            m.calls[0].input.as_ref().unwrap().get("a"),
            Some(&Any::Num(1.0))
        );
        let m = parse_response(
            b"```json\n{\"tool\":\"zap\"}\n``` and ```tool\n{\"tool\":\"read\"}\n```",
        );
        assert_eq!(m.calls[0].name, "read");
        assert_eq!(m.calls[0].input, Some(Map::new()));
        let m = parse_response(b"```json\n{\"tool\":\"zap\"}\n```");
        assert!(m.calls.is_empty());
        let m = parse_response(b" {\"TOOL\":\"bash\",\"input\":{\"cmd\":\"ls\"}} ");
        assert_eq!(m.calls[0].name, "bash");
        let m = parse_response(b"  plain  ");
        assert_eq!(
            (m.role, m.text.as_slice()),
            ("assistant", b"plain".as_slice())
        );
        let m = parse_response(b"{\"tool\":\"read\",\"input\":[1]}");
        assert!(m.calls.is_empty());
    }
}
