//! The model over the Messages API wire, with native tool use: a tiny
//! system prompt and the four tool schemas. It needs ANTHROPIC_API_KEY, so
//! it is off unless SPRIG_BACKEND=api. The gate is unchanged: every call
//! still goes through the executor.

use crate::agent::{Message, Model};
use crate::gate::ToolCall;
use crate::json::{jmap, Map, J};
use crate::session::Usage;
use std::os::unix::ffi::OsStrExt;
use std::time::Duration;

pub struct ApiModel {
    pub key: Vec<u8>,
    pub model: String,
    pub base_url: String,
    pub timeout: Duration,
    pub max_tokens: i64,
    /// One id for the whole run, sent on every request.
    pub session: String,
    /// The wall: the tools the request offers. Empty offers all four.
    pub tools: Vec<String>,
    /// Sends no tool schema (--tools none).
    pub no_tools: bool,
    /// The system prompt in place of sprig's, when `system_set`.
    pub system: Vec<u8>,
    pub system_set: bool,
    /// More tools (from MCP servers) the request offers after the wall's.
    /// With `no_tools` they are the only tools.
    pub extra: Vec<crate::mcp_client::McpToolDef>,
}

/// The line that opens a run with no tools.
pub const NO_TOOLS_PROMPT: &str =
    "You are sprig, a minimal agent. Reply with your final answer as plain text.";

/// The system prompt of a request: the --system-file bytes when one was
/// given, else sprig's own for the wall (or for no tools).
pub fn system_for(wall: &[String], no_tools: bool, system: &[u8], system_set: bool) -> Vec<u8> {
    if system_set {
        system.to_vec()
    } else if no_tools {
        NO_TOOLS_PROMPT.as_bytes().to_vec()
    } else {
        system_prompt_for(wall).into_bytes()
    }
}

/// The system prompt naming the tools of the wall. The full wall gives
/// SYSTEM_PROMPT itself.
pub fn system_prompt_for(wall: &[String]) -> String {
    format!(
        "You are sprig, a minimal coding agent. Use the tools ({}) to complete the task. \
When finished, reply with your final answer as plain text.",
        wall.join(", ")
    )
}

/// The tools of the wall, in sprig's order, in tool-definition form.
pub fn api_tools_for(wall: &[String]) -> J {
    let J::Arr(all) = api_tools() else {
        return J::Arr(Vec::new());
    };
    let names = crate::tools::WALL_TOOL_NAMES;
    J::Arr(
        all.into_iter()
            .zip(names.iter())
            .filter(|(_, n)| wall.iter().any(|w| w == *n))
            .map(|(t, _)| t)
            .collect(),
    )
}

pub const SYSTEM_PROMPT: &str = "You are sprig, a minimal coding agent. Use the tools \
(read, write, edit, bash) to complete the task. When finished, reply with your \
final answer as plain text.";

fn env(k: &str) -> Vec<u8> {
    std::env::var_os(k)
        .map(|v| v.as_bytes().to_vec())
        .unwrap_or_default()
}

/// One content block of a reply.
#[derive(Debug, Clone, Default)]
struct Block {
    typ: String,
    text: String,
    id: String,
    name: String,
    input: Option<Map>,
}

/// Go's type texts for the reply's anonymous structs, for its errors.
const BLOCK_TYPE: &str = r#"struct { Type string "json:\"type\""; Text string "json:\"text\""; ID string "json:\"id\""; Name string "json:\"name\""; Input map[string]interface {} "json:\"input\"" }"#;
const CONTENT_TYPE: &str = r#"[]struct { Type string "json:\"type\""; Text string "json:\"text\""; ID string "json:\"id\""; Name string "json:\"name\""; Input map[string]interface {} "json:\"input\"" }"#;
const API_TYPE: &str = r#"struct { Model string "json:\"model\""; Content []struct { Type string "json:\"type\""; Text string "json:\"text\""; ID string "json:\"id\""; Name string "json:\"name\""; Input map[string]interface {} "json:\"input\"" } "json:\"content\""; Usage struct { InputTokens int "json:\"input_tokens\""; CacheReadInputTokens int "json:\"cache_read_input_tokens\""; CacheCreationInputTokens int "json:\"cache_creation_input_tokens\""; OutputTokens int "json:\"output_tokens\"" } "json:\"usage\"" }"#;
const ERROR_TYPE: &str =
    r#"struct { Error struct { Message string "json:\"message\"" } "json:\"error\"" }"#;
const ERROR_INNER_TYPE: &str = r#"struct { Message string "json:\"message\"" }"#;

/// One apiBlock as Go writes it: its fields in order, the empty ones left
/// out (omitempty).
fn block(
    typ: &str,
    text: &[u8],
    id: &str,
    name: &str,
    input: &Option<Map>,
    tool_use_id: &str,
    content: &[u8],
) -> J {
    let mut o: Vec<(String, J)> = vec![("type".into(), J::s(typ))];
    if !text.is_empty() {
        o.push(("text".into(), J::s(text)));
    }
    if !id.is_empty() {
        o.push(("id".into(), J::s(id)));
    }
    if !name.is_empty() {
        o.push(("name".into(), J::s(name)));
    }
    if let Some(m) = input {
        if !m.is_empty() {
            o.push(("input".into(), crate::json::jmap_any(input)));
        }
    }
    if !tool_use_id.is_empty() {
        o.push(("tool_use_id".into(), J::s(tool_use_id)));
    }
    if !content.is_empty() {
        o.push(("content".into(), J::s(content)));
    }
    J::Obj(o)
}

/// Reads ANTHROPIC_API_KEY (required), ANTHROPIC_BASE_URL and SPRIG_MODEL.
pub fn new_api_model() -> Result<ApiModel, Vec<u8>> {
    new_api_model_for(b"", false, "")
}

/// The base URL when neither --base-url nor ANTHROPIC_BASE_URL is set.
pub const DEFAULT_ANTHROPIC_BASE: &str = "https://api.anthropic.com";

/// new_api_model with the key and the base given. With key_set, key is
/// the key and no key is read from the environment. base is --base-url;
/// "" falls back to ANTHROPIC_BASE_URL, then the default. A key is
/// required only for the default base.
pub fn new_api_model_for(key: &[u8], key_set: bool, base: &str) -> Result<ApiModel, Vec<u8>> {
    let key = if key_set {
        key.to_vec()
    } else {
        env("ANTHROPIC_API_KEY")
    };
    let mut base = base.to_string();
    if base.is_empty() {
        base = String::from_utf8_lossy(&env("ANTHROPIC_BASE_URL")).into_owned();
    }
    let mut base = base.trim_end_matches('/').to_string();
    if base.is_empty() {
        base = DEFAULT_ANTHROPIC_BASE.to_string();
    }
    if key.is_empty() && base == DEFAULT_ANTHROPIC_BASE {
        return Err("the api backend needs ANTHROPIC_API_KEY \u{2014} E talks the raw API and breaks the exposure freeze, so set a key to opt in"
            .as_bytes()
            .to_vec());
    }
    let mut model = String::from("claude-haiku-4-5");
    let o = String::from_utf8_lossy(&env("SPRIG_MODEL")).into_owned();
    if !o.is_empty() {
        model = o;
    }
    Ok(ApiModel {
        key,
        model,
        base_url: base,
        timeout: Duration::from_secs(120),
        max_tokens: 4096,
        session: format!("sprig-{}", crate::osx::random_hex(8)),
        tools: Vec::new(),
        no_tools: false,
        system: Vec::new(),
        system_set: false,
        extra: Vec::new(),
    })
}

/// A cache mark, the last field of a block.
fn marked(b: J) -> J {
    match b {
        J::Obj(mut kv) => {
            // One mark per block: a block that has one keeps it.
            if kv.iter().any(|(k, _)| k == "cache_control") {
                return J::Obj(kv);
            }
            kv.push((
                "cache_control".into(),
                jmap(vec![("type", J::s("ephemeral"))]),
            ));
            J::Obj(kv)
        }
        other => other,
    }
}

/// Whether a block is a text block with no text.
fn is_empty_text(b: &J) -> bool {
    match b {
        J::Obj(kv) => {
            kv.iter().any(|(k, v)| k == "type" && matches!(v, J::Str(t) if t == b"text"))
                && !kv.iter().any(|(k, _)| k == "text")
        }
        _ => false,
    }
}

/// sprig's history as Messages-API messages. With `mark_end`, the last
/// block of the request that has something in it is marked for the cache.
pub fn build_messages(history: &[Message], mark_end: bool) -> J {
    // Each message is (role, blocks); None blocks is a nil list.
    let mut msgs: Vec<(&'static str, Option<Vec<J>>)> = Vec::new();
    let (mut n_use, mut n_res) = (0, 0);
    let mut i = 0;
    while i < history.len() {
        let m = &history[i];
        match m.role {
            "user" => {
                let b = block("text", &m.text, "", "", &None, "", b"");
                let add = vec![b];
                match msgs.last_mut() {
                    Some(("user", Some(blocks))) => blocks.extend(add),
                    _ => msgs.push(("user", Some(add))),
                }
                i += 1;
            }
            "assistant" => {
                let mut blocks: Option<Vec<J>> = None;
                if !m.text.is_empty() {
                    blocks
                        .get_or_insert_with(Vec::new)
                        .push(block("text", &m.text, "", "", &None, "", b""));
                }
                for c in &m.calls {
                    let input = Some(c.input.clone().unwrap_or_default());
                    let id = format!("toolu_{n_use}");
                    blocks
                        .get_or_insert_with(Vec::new)
                        .push(block("tool_use", b"", &id, &c.name, &input, "", b""));
                    n_use += 1;
                }
                msgs.push(("assistant", blocks));
                i += 1;
            }
            "tool" => {
                let mut blocks: Option<Vec<J>> = None;
                while i < history.len() && history[i].role == "tool" {
                    let id = format!("toolu_{n_res}");
                    blocks.get_or_insert_with(Vec::new).push(block(
                        "tool_result",
                        b"",
                        "",
                        "",
                        &None,
                        &id,
                        &history[i].text,
                    ));
                    n_res += 1;
                    i += 1;
                }
                msgs.push(("user", blocks));
            }
            _ => i += 1,
        }
    }
    if mark_end {
        // An empty text block cannot carry a mark: the last block with
        // something in it does.
        if let Some((_, Some(blocks))) = msgs.last_mut() {
            if let Some(i) = blocks.iter().rposition(|b| !is_empty_text(b)) {
                let b = blocks.remove(i);
                blocks.insert(i, marked(b));
            }
        }
    }
    J::Arr(
        msgs.into_iter()
            .map(|(role, blocks)| {
                J::Obj(vec![
                    ("role".into(), J::s(role)),
                    ("content".into(), blocks.map_or(J::Null, J::Arr)),
                ])
            })
            .collect(),
    )
}

/// The four tools in Anthropic tool-definition form.
pub fn api_tools() -> J {
    let obj = |required: &[&str], fields: &[&str]| {
        let props = fields
            .iter()
            .map(|f| (*f, jmap(vec![("type", J::s("string"))])))
            .collect();
        jmap(vec![
            ("type", J::s("object")),
            ("properties", jmap(props)),
            (
                "required",
                J::Arr(required.iter().map(|r| J::s(r)).collect()),
            ),
        ])
    };
    let tool = |name: &str, desc: &str, schema: J| {
        jmap(vec![
            ("name", J::s(name)),
            ("description", J::s(desc)),
            ("input_schema", schema),
        ])
    };
    J::Arr(vec![
        tool(
            "read",
            "Return a file's contents.",
            obj(&["path"], &["path"]),
        ),
        tool(
            "write",
            "Write content to a file.",
            obj(&["path", "content"], &["path", "content"]),
        ),
        tool(
            "edit",
            "Replace a unique occurrence of old with new.",
            obj(&["path", "old", "new"], &["path", "old", "new"]),
        ),
        tool("bash", "Run a shell command.", obj(&["cmd"], &["cmd"])),
        tool(
            "spawn",
            "Start a child agent on a task inside a narrower envelope, wait, and return its answer.",
            spawn_schema(),
        ),
    ])
}

/// The spawn tool's input: the task and the child's envelope, and
/// optionally the agent profile, the model, the child's tools, its token
/// and turn reservation and its turn limit.
pub fn spawn_schema() -> J {
    let ty = |t: &str| jmap(vec![("type", J::s(t))]);
    jmap(vec![
        ("type", J::s("object")),
        (
            "properties",
            jmap(vec![
                ("task", ty("string")),
                ("envelope", ty("object")),
                ("profile", ty("string")),
                ("model", ty("string")),
                (
                    "tools",
                    jmap(vec![("type", J::s("array")), ("items", ty("string"))]),
                ),
                ("tokens", ty("integer")),
                ("turns", ty("integer")),
                ("max_turns", ty("integer")),
            ]),
        ),
        ("required", J::Arr(vec![J::s("task"), J::s("envelope")])),
    ])
}

/// A Messages-API reply as a sprig turn.
pub fn parse_api_response(data: &[u8]) -> Result<Message, Vec<u8>> {
    let mut model = String::new();
    let mut content: crate::json::GoSlice<Block> = Default::default();
    let mut usage = Usage::default();
    let r = crate::json::unmarshal(data, |d, n| {
        d.object(
            n,
            "",
            API_TYPE,
            &["model", "content", "usage"],
            |d, i, v| match i {
                0 => d.string(v, &mut model),
                1 => d.slice(v, &mut content, CONTENT_TYPE, |d, v, b| {
                    d.object(
                        v,
                        "",
                        BLOCK_TYPE,
                        &["type", "text", "id", "name", "input"],
                        |d, i, v| match i {
                            0 => d.string(v, &mut b.typ),
                            1 => d.string(v, &mut b.text),
                            2 => d.string(v, &mut b.id),
                            3 => d.string(v, &mut b.name),
                            _ => d.map_any(v, &mut b.input),
                        },
                    )
                }),
                _ => crate::model_claude::usage_fields(d, v, &mut usage),
            },
        )
    });
    if let Err(e) = r {
        return Err(format!("bad api response: {e}").into_bytes());
    }
    let mut calls = Vec::new();
    let mut text = Vec::new();
    for b in content.items() {
        match b.typ.as_str() {
            "tool_use" => calls.push(ToolCall {
                id: b.id.clone(),
                name: b.name.clone(),
                input: Some(b.input.clone().unwrap_or_default()),
            }),
            "text" => text.extend_from_slice(b.text.as_bytes()),
            _ => {}
        }
    }
    if !calls.is_empty() {
        return Ok(Message {
            role: "assistant",
            calls,
            model,
            usage,
            ..Default::default()
        });
    }
    Ok(Message {
        role: "assistant",
        text: crate::gostr::trim_space(&text).to_vec(),
        model,
        usage,
        ..Default::default()
    })
}

/// Whether an error body carries a message a person can read.
pub fn has_error_text(data: &[u8]) -> bool {
    let mut msg = String::new();
    let r = crate::json::unmarshal(data, |d, n| {
        d.object(n, "", ERROR_TYPE, &["error"], |d, _, v| {
            d.object(v, "", ERROR_INNER_TYPE, &["message"], |d, _, v| {
                d.string(v, &mut msg)
            })
        })
    });
    r.is_ok() && !crate::gostr::trim_space(msg.as_bytes()).is_empty()
}

impl ApiModel {
    /// The request body for this history.
    pub fn request_body(&self, history: &[Message]) -> Vec<u8> {
        let max = if self.max_tokens == 0 {
            4096
        } else {
            self.max_tokens
        };
        let wall: Vec<String> = if self.tools.is_empty() {
            crate::tools::ALL_TOOL_NAMES
                .iter()
                .map(|t| t.to_string())
                .collect()
        } else {
            self.tools.clone()
        };
        let system = system_for(&wall, self.no_tools, &self.system, self.system_set);
        let system = if self.system_set && !system.is_empty() {
            J::Arr(vec![marked(block("text", &system, "", "", &None, "", b""))])
        } else {
            J::Str(system)
        };
        let mut fields = vec![
            ("model", J::s(&self.model)),
            ("max_tokens", J::Int(max)),
            ("system", system),
            (
                "messages",
                build_messages(
                    history,
                    self.system_set && (!self.no_tools || !self.extra.is_empty()),
                ),
            ),
        ];
        let mut tools = match (self.no_tools, api_tools_for(&wall)) {
            (false, J::Arr(t)) => t,
            _ => Vec::new(),
        };
        for e in &self.extra {
            tools.push(jmap(vec![
                ("name", J::s(&e.name)),
                ("description", J::s(&e.description)),
                ("input_schema", e.schema.clone()),
            ]));
        }
        if !tools.is_empty() {
            fields.push(("tools", J::Arr(tools)));
        }
        crate::json::encode(&jmap(fields))
    }
}

impl Model for ApiModel {
    fn model_name(&self) -> String {
        self.model.clone()
    }

    fn held_key(&self) -> Vec<u8> {
        self.key.clone()
    }

    fn next(&mut self, history: &[Message]) -> Result<Message, Vec<u8>> {
        let body = self.request_body(history);
        let mut headers: Vec<(&str, &[u8])> = vec![
            ("Anthropic-Version", b"2023-06-01"),
            ("Content-Type", b"application/json"),
        ];
        if !self.key.is_empty() {
            headers.push(("X-Api-Key", &self.key));
        }
        if !self.session.is_empty() {
            headers.push(("X-Opencode-Session", self.session.as_bytes()));
        }
        let timeout = if self.timeout.is_zero() {
            Duration::from_secs(120)
        } else {
            self.timeout
        };
        let url = format!("{}/v1/messages", self.base_url);
        let (status, data) = crate::http::post(&url, &headers, &body, timeout).map_err(|e| {
            if e == crate::http::TOO_BIG {
                format!("anthropic api: {e}").into_bytes()
            } else {
                e.into_bytes()
            }
        })?;
        if status != 200 {
            if status == 400 && !has_error_text(&data) {
                return Err(format!(
                    "{} refused this request and gave no reason. Some providers refuse topics by policy. \
Set SPRIG_MODEL to another model and try again",
                    self.model
                )
                .into_bytes());
            }
            let mut e = format!("anthropic api {status}: ").into_bytes();
            e.extend_from_slice(crate::gostr::trim_space(&data));
            return Err(e);
        }
        parse_api_response(&data)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::json::{encode, Any};

    fn msg(role: &'static str, text: &str) -> Message {
        Message {
            role,
            text: text.as_bytes().to_vec(),
            ..Default::default()
        }
    }

    #[test]
    fn wall_follows_go() {
        let all: Vec<String> = crate::tools::ALL_TOOL_NAMES
            .iter()
            .map(|t| t.to_string())
            .collect();
        assert_eq!(system_prompt_for(&all), SYSTEM_PROMPT);
        let wall: Vec<String> = crate::tools::WALL_TOOL_NAMES
            .iter()
            .map(|t| t.to_string())
            .collect();
        assert_eq!(encode(&api_tools_for(&wall)), encode(&api_tools()));
        let two = vec!["read".to_string(), "edit".to_string()];
        assert!(system_prompt_for(&two).contains("(read, edit)"));
        let J::Arr(t) = api_tools_for(&two) else {
            panic!()
        };
        assert_eq!(t.len(), 2);
    }

    #[test]
    fn marks_follow_go() {
        let count = |h: &[Message], end: bool| {
            let s = String::from_utf8(encode(&build_messages(h, end))).unwrap();
            s.matches("cache_control").count()
        };
        let mut h = vec![msg("user", "go")];
        assert_eq!(count(&h, true), 1);
        assert_eq!(count(&h, false), 0);
        for _ in 0..3 {
            h.push(Message {
                role: "assistant",
                calls: vec![ToolCall {
                    name: "read".into(),
                    ..Default::default()
                }],
                ..Default::default()
            });
            h.push(msg("tool", "A"));
            // Only the end of the request moves; there is never a second mark.
            assert_eq!(count(&h, true), 1);
        }
    }

    #[test]
    fn an_empty_last_block_gets_no_mark() {
        let h = [msg("user", "before"), msg("user", "")];
        let s = String::from_utf8(encode(&build_messages(&h, true))).unwrap();
        // The block with text is marked; the empty one is not.
        assert_eq!(s.matches("cache_control").count(), 1);
        assert!(s.ends_with("{\"type\":\"text\"}]}]"), "{s}");
    }

    #[test]
    fn messages_follow_go() {
        let mut m = Map::new();
        m.insert("path".into(), Any::Str("k".into()));
        let call = |name: &str, input: Option<Map>| ToolCall {
            name: name.into(),
            input,
            ..Default::default()
        };
        let h = vec![
            msg("user", "task"),
            Message {
                role: "assistant",
                text: b"thinking".to_vec(),
                calls: vec![call("read", Some(m)), call("bash", None)],
                ..Default::default()
            },
            msg("tool", "K"),
            msg("tool", ""),
            msg("assistant", ""),
            msg("user", "nudge"),
            Message {
                role: "user",
                text: b"more\xff".to_vec(),
                ..Default::default()
            },
        ];
        let got = String::from_utf8(encode(&build_messages(&h, false))).unwrap();
        assert_eq!(
            got,
            "[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"task\"}]},\
             {\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"thinking\"},\
             {\"type\":\"tool_use\",\"id\":\"toolu_0\",\"name\":\"read\",\"input\":{\"path\":\"k\"}},\
             {\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"bash\"}]},\
             {\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"toolu_0\",\"content\":\"K\"},\
             {\"type\":\"tool_result\",\"tool_use_id\":\"toolu_1\"}]},\
             {\"role\":\"assistant\",\"content\":null},\
             {\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"nudge\"},{\"type\":\"text\",\"text\":\"more\\ufffd\"}]}]"
        );
        let tools = String::from_utf8(encode(&api_tools())).unwrap();
        assert!(tools.starts_with("[{\"description\":\"Return a file's contents.\",\"input_schema\":{\"properties\":{\"path\":{\"type\":\"string\"}},\"required\":[\"path\"],\"type\":\"object\"},\"name\":\"read\"}"));
    }

    #[test]
    fn replies_follow_go() {
        let r = parse_api_response(br#"{"model":"m","content":[{"type":"text","text":" a "},{"type":"text","text":"b "}],"usage":{"input_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3,"output_tokens":4}}"#).unwrap();
        assert_eq!(
            (r.text.as_slice(), r.model.as_str()),
            (b"a b".as_slice(), "m")
        );
        assert_eq!(
            r.usage,
            Usage {
                fresh: 1,
                cache_read: 2,
                cache_write: 3,
                out: 4
            }
        );
        let r = parse_api_response(br#"{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"u1","name":"read","input":null}]}"#).unwrap();
        assert_eq!((r.calls[0].id.as_str(), r.text.len()), ("u1", 0));
        assert_eq!(r.calls[0].input, Some(Map::new()));
        assert_eq!(
            parse_api_response(br#"{"content":[{"type":5}]}"#).unwrap_err(),
            b"bad api response: json: cannot unmarshal number into Go struct field .content.type of type string"
        );
        let e = parse_api_response(b"[]").unwrap_err();
        assert!(e.starts_with(b"bad api response: json: cannot unmarshal array into Go value of type struct { Model string \"json:\\\"model\\\"\"; Content []struct { Type string"));
        assert_eq!(
            parse_api_response(b"").unwrap_err(),
            b"bad api response: unexpected end of JSON input"
        );
        assert!(has_error_text(br#"{"error":{"message":"x"}}"#));
        assert!(!has_error_text(br#"{"error":{"message":"  "}}"#));
        assert!(!has_error_text(b"oops"));
    }
}
