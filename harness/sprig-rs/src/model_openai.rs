//! The model over the OpenAI chat-completions wire, with native function
//! calling: OpenAI, Ollama, llama.cpp, vLLM, and gateways in front of
//! other providers. The gate is unchanged: every returned call still goes
//! through the executor.

use crate::agent::{Message, Model};
use crate::gate::ToolCall;
use crate::json::{jmap, jmap_any, Map, Node, J, V};
use crate::session::Usage;
use std::os::unix::ffi::OsStrExt;
use std::time::Duration;

/// The base URL when --base-url is not given.
pub const DEFAULT_OPENAI_BASE: &str = "https://api.openai.com/v1";

pub struct OpenAiModel {
    /// Empty sends no Authorization header.
    pub key: Vec<u8>,
    pub model: String,
    pub base_url: String,
    pub timeout: Duration,
    pub max_tokens: i64,
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

fn env(k: &str) -> Vec<u8> {
    std::env::var_os(k)
        .map(|v| v.as_bytes().to_vec())
        .unwrap_or_default()
}

/// The OpenAI-wire backend. With key_set, key is the key and no key is
/// read from the environment; else OPENAI_API_KEY is read. A key is
/// required only for the default base.
pub fn new_openai_model(
    key: &[u8],
    key_set: bool,
    base: &str,
    model: &str,
) -> Result<OpenAiModel, Vec<u8>> {
    let key = if key_set {
        key.to_vec()
    } else {
        env("OPENAI_API_KEY")
    };
    let mut base = base.to_string();
    if base.is_empty() {
        base = DEFAULT_OPENAI_BASE.to_string();
        if key.is_empty() {
            return Err(format!(
                "the openai wire needs a key for {DEFAULT_OPENAI_BASE}: pass --key-fd, or set --base-url to a server that needs none"
            )
            .into_bytes());
        }
    }
    let mut model = model.to_string();
    if model.is_empty() {
        model = String::from_utf8_lossy(&env("SPRIG_MODEL")).into_owned();
    }
    if model.is_empty() {
        return Err(b"the openai wire needs a model: pass --model, or set SPRIG_MODEL".to_vec());
    }
    Ok(OpenAiModel {
        key,
        model,
        base_url: base.trim_end_matches('/').to_string(),
        timeout: Duration::from_secs(120),
        max_tokens: 4096,
        tools: Vec::new(),
        no_tools: false,
        system: Vec::new(),
        system_set: false,
        extra: Vec::new(),
    })
}

/// The wall's tools as OpenAI function definitions, in sprig's order.
pub fn openai_tools_for(wall: &[String]) -> J {
    let J::Arr(tools) = crate::model_api::api_tools_for(wall) else {
        return J::Null;
    };
    if tools.is_empty() {
        return J::Null;
    }
    J::Arr(
        tools
            .into_iter()
            .map(|t| {
                let J::Obj(kv) = t else { return J::Null };
                let get = |k: &str| {
                    kv.iter()
                        .find(|(n, _)| n == k)
                        .map(|(_, v)| v.clone())
                        .unwrap_or(J::Null)
                };
                jmap(vec![
                    ("type", J::s("function")),
                    (
                        "function",
                        jmap(vec![
                            ("name", get("name")),
                            ("description", get("description")),
                            ("parameters", get("input_schema")),
                        ]),
                    ),
                ])
            })
            .collect(),
    )
}

/// sprig's history as chat messages: the system prompt first, each call
/// with a synthetic id call_N, the Nth tool result paired with the Nth
/// call, and a call's arguments as a JSON string.
pub fn openai_messages(history: &[Message], system: &[u8]) -> J {
    let mut msgs = vec![jmap(vec![
        ("role", J::s("system")),
        ("content", J::s(system)),
    ])];
    let (mut n_use, mut n_res) = (0, 0);
    for m in history {
        match m.role {
            "user" => msgs.push(jmap(vec![
                ("role", J::s("user")),
                ("content", J::s(&m.text)),
            ])),
            "assistant" => {
                let mut kv = vec![("role", J::s("assistant")), ("content", J::s(&m.text))];
                if !m.calls.is_empty() {
                    if m.text.is_empty() {
                        kv[1].1 = J::Null;
                    }
                    let calls = m
                        .calls
                        .iter()
                        .map(|c| {
                            let input = Some(c.input.clone().unwrap_or_default());
                            let args = crate::json::encode(&jmap_any(&input));
                            let id = format!("call_{n_use}");
                            n_use += 1;
                            jmap(vec![
                                ("id", J::s(id)),
                                ("type", J::s("function")),
                                (
                                    "function",
                                    jmap(vec![
                                        ("name", J::s(&c.name)),
                                        ("arguments", J::Str(args)),
                                    ]),
                                ),
                            ])
                        })
                        .collect();
                    kv.push(("tool_calls", J::Arr(calls)));
                }
                msgs.push(jmap(kv));
            }
            "tool" => {
                msgs.push(jmap(vec![
                    ("role", J::s("tool")),
                    ("tool_call_id", J::s(format!("call_{n_res}"))),
                    ("content", J::s(&m.text)),
                ]));
                n_res += 1;
            }
            _ => {}
        }
    }
    J::Arr(msgs)
}

impl OpenAiModel {
    /// The request body for this history.
    pub fn request_body(&self, history: &[Message]) -> Vec<u8> {
        let wall: Vec<String> = if self.tools.is_empty() {
            crate::tools::ALL_TOOL_NAMES
                .iter()
                .map(|t| t.to_string())
                .collect()
        } else {
            self.tools.clone()
        };
        let max = if self.max_tokens == 0 {
            4096
        } else {
            self.max_tokens
        };
        let system =
            crate::model_api::system_for(&wall, self.no_tools, &self.system, self.system_set);
        let mut fields = vec![
            ("model", J::s(&self.model)),
            ("max_tokens", J::Int(max)),
            (
                "messages",
                openai_messages(history, &system),
            ),
        ];
        let mut tools = match (self.no_tools, openai_tools_for(&wall)) {
            (false, J::Arr(t)) => t,
            _ => Vec::new(),
        };
        for e in &self.extra {
            tools.push(jmap(vec![
                ("type", J::s("function")),
                (
                    "function",
                    jmap(vec![
                        ("name", J::s(&e.name)),
                        ("description", J::s(&e.description)),
                        ("parameters", e.schema.clone()),
                    ]),
                ),
            ]));
        }
        if !tools.is_empty() {
            fields.push(("tools", J::Arr(tools)));
        }
        crate::json::encode(&jmap(fields))
    }
}

/// The last value an object holds under key.
fn field<'a>(n: Option<&'a Node>, key: &str) -> Option<&'a Node> {
    match &n?.v {
        V::Obj(kv) => kv.iter().rev().find(|(k, _)| k == key).map(|(_, v)| v),
        _ => None,
    }
}

/// A JSON integer as Python reads one: no fraction or exponent, 64 bits.
fn json_int(n: Option<&Node>) -> Option<i64> {
    let V::Num(t) = &n?.v else { return None };
    let digits = t.strip_prefix('-').unwrap_or(t);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    t.parse::<i64>().ok()
}

fn str_of(n: Option<&Node>) -> String {
    match n.map(|n| &n.v) {
        Some(V::Str(s)) => s.clone(),
        _ => String::new(),
    }
}

/// The wire's usage on sprig's four counts, as the gateway maps it.
fn openai_usage(u: Option<&Node>) -> Usage {
    let mut out = Usage::default();
    let cached = json_int(field(field(u, "prompt_tokens_details"), "cached_tokens")).unwrap_or(0);
    if let Some(prompt) = json_int(field(u, "prompt_tokens")) {
        out.cache_read = cached;
        if prompt < 0 {
            // Kept negative, so the loop refuses the turn.
            out.fresh = prompt;
        } else if cached >= 0 {
            out.fresh = prompt.saturating_sub(cached).max(0);
        }
    }
    if let Some(c) = json_int(field(u, "completion_tokens")) {
        out.out = c;
    }
    out
}

/// A call's arguments: a JSON string that holds an object (or null, or
/// nothing), or an object as some servers send it.
fn openai_arguments(a: Option<&Node>) -> Option<Map> {
    let decode = |n: &Node| -> Option<Map> {
        let mut d = crate::json::Dec::default();
        let any = d.any(n);
        if d.err.is_some() {
            return None;
        }
        match any {
            crate::json::Any::Map(m) => Some(m),
            _ => None,
        }
    };
    match a.map(|n| &n.v) {
        None | Some(V::Null) => Some(Map::new()),
        Some(V::Obj(_)) => decode(a?),
        Some(V::Str(s)) => {
            if crate::gostr::trim_space(s.as_bytes()).is_empty() {
                return Some(Map::new());
            }
            let n = crate::json::parse(s.as_bytes()).ok()?;
            match n.v {
                V::Null => Some(Map::new()),
                V::Obj(_) => decode(&n),
                _ => None,
            }
        }
        _ => None,
    }
}

/// A chat completion as a sprig turn.
pub fn parse_openai_response(data: &[u8]) -> Result<Message, Vec<u8>> {
    let root =
        crate::json::parse(data).map_err(|e| format!("bad openai response: {e}").into_bytes())?;
    // Go decodes the whole reply into any first: a number out of float64's
    // range is an error there.
    let mut d = crate::json::Dec::default();
    let _ = d.any(&root);
    if let Some(e) = d.err {
        return Err(format!("bad openai response: {e}").into_bytes());
    }
    let r = Some(&root);
    let model = str_of(field(r, "model"));
    let usage = openai_usage(field(r, "usage"));
    let msg = match field(r, "choices").map(|n| &n.v) {
        Some(V::Arr(cs)) if !cs.is_empty() => field(Some(&cs[0]), "message"),
        _ => None,
    };
    let mut calls = Vec::new();
    if let Some(V::Arr(tcs)) = field(msg, "tool_calls").map(|n| &n.v) {
        for tc in tcs {
            let id = str_of(field(Some(tc), "id"));
            let f = field(Some(tc), "function");
            let name = str_of(field(f, "name"));
            let Some(input) = openai_arguments(field(f, "arguments")) else {
                return Err(format!(
                    "openai api: the arguments of tool call {} are not a JSON object",
                    crate::goerr::quote(name.as_bytes())
                )
                .into_bytes());
            };
            calls.push(ToolCall {
                id,
                name,
                input: Some(input),
            });
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
    let text = str_of(field(msg, "content"));
    Ok(Message {
        role: "assistant",
        text: crate::gostr::trim_space(text.as_bytes()).to_vec(),
        model,
        usage,
        ..Default::default()
    })
}

impl Model for OpenAiModel {
    fn model_name(&self) -> String {
        self.model.clone()
    }

    fn held_key(&self) -> Vec<u8> {
        self.key.clone()
    }

    fn next(&mut self, history: &[Message]) -> Result<Message, Vec<u8>> {
        let body = self.request_body(history);
        let bearer = [b"Bearer ".as_slice(), &self.key].concat();
        let mut headers: Vec<(&str, &[u8])> = vec![("Content-Type", b"application/json")];
        if !self.key.is_empty() {
            headers.insert(0, ("Authorization", &bearer));
        }
        let timeout = if self.timeout.is_zero() {
            Duration::from_secs(120)
        } else {
            self.timeout
        };
        let url = format!("{}/chat/completions", self.base_url);
        let (status, data) = crate::http::post(&url, &headers, &body, timeout).map_err(|e| {
            if e == crate::http::TOO_BIG {
                format!("openai api: {e}").into_bytes()
            } else {
                e.into_bytes()
            }
        })?;
        if status != 200 {
            let mut e = format!("openai api {status}: ").into_bytes();
            e.extend_from_slice(crate::gostr::trim_space(&data));
            return Err(e);
        }
        parse_openai_response(&data)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn messages_follow_go() {
        let mut call = ToolCall {
            name: "read".into(),
            ..Default::default()
        };
        let mut m = Map::new();
        m.insert("path".into(), crate::json::Any::Str("a<b".into()));
        call.input = Some(m);
        let h = vec![
            Message {
                role: "user",
                text: b"go".to_vec(),
                ..Default::default()
            },
            Message {
                role: "assistant",
                calls: vec![
                    call,
                    ToolCall {
                        name: "bash".into(),
                        ..Default::default()
                    },
                ],
                ..Default::default()
            },
            Message {
                role: "tool",
                text: b"A".to_vec(),
                ..Default::default()
            },
            Message {
                role: "tool",
                text: b"B".to_vec(),
                ..Default::default()
            },
            Message {
                role: "assistant",
                text: b"done".to_vec(),
                ..Default::default()
            },
        ];
        let got = String::from_utf8(crate::json::encode(&openai_messages(&h, b"sys"))).unwrap();
        let want = r#"[{"content":"sys","role":"system"},{"content":"go","role":"user"},{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{\"path\":\"a\\u003cb\"}","name":"read"},"id":"call_0","type":"function"},{"function":{"arguments":"{}","name":"bash"},"id":"call_1","type":"function"}]},{"content":"A","role":"tool","tool_call_id":"call_0"},{"content":"B","role":"tool","tool_call_id":"call_1"},{"content":"done","role":"assistant"}]"#;
        assert_eq!(got, want);
    }

    #[test]
    fn replies_follow_go() {
        let m = parse_openai_response(br#"{"model":"llama3.3:70b","choices":[{"message":{"content":"","tool_calls":[{"id":"c9","type":"function","function":{"name":"read","arguments":"{\"path\":\"x\",\"n\":2}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}"#).unwrap();
        assert_eq!(m.calls[0].id, "c9");
        assert_eq!(
            m.usage,
            Usage {
                fresh: 60,
                cache_read: 40,
                cache_write: 0,
                out: 7
            }
        );
        assert_eq!(
            parse_openai_response(br#"{"choices":[{"message":{"tool_calls":[{"function":{"name":"read","arguments":"[1]"}}]}}]}"#).unwrap_err(),
            b"openai api: the arguments of tool call \"read\" are not a JSON object"
        );
        let m = parse_openai_response(
            br#"{"choices":[{"message":{"content":"  hi  "}}],"usage":{"prompt_tokens":2.5}}"#,
        )
        .unwrap();
        assert_eq!(
            (m.text.as_slice(), m.usage),
            (b"hi".as_slice(), Usage::default())
        );
    }
}
