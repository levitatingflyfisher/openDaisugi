//! `opendaisugi/llm_client.py` for the HTTP backend: which wire a model
//! name takes, the URL and credential, the exact request bytes, the reply,
//! and the error texts (rulings LLM-2 to LLM-8, LLM-14). The gate's
//! llm_check and the structured call both use it.

use std::sync::OnceLock;

use super::http;
use crate::gate::py::text::{head, repr};
use crate::gate::pyjson::{dumps, loads_py, Value};
use crate::netproxy;

pub const DEFAULT_MAX_TOKENS: i64 = 8192;
pub const DEFAULT_TIMEOUT: f64 = 600.0;
pub const CUT_TEXT: &str = "The output is incomplete due to a max_tokens length limit.";
pub const NO_KEY_TEXT: &str = "no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set";
pub const REASK_TEXT: &str = "Correct your JSON ONLY RESPONSE, based on the following errors:\n";

fn key_re() -> &'static regex::Regex {
    static R: OnceLock<regex::Regex> = OnceLock::new();
    R.get_or_init(|| regex::Regex::new(r"(sk-[a-zA-Z0-9_-]{4})[a-zA-Z0-9_-]{8,}([a-zA-Z0-9_-]{4})").expect("key pattern"))
}

fn userinfo_re() -> &'static regex::Regex {
    static R: OnceLock<regex::Regex> = OnceLock::new();
    R.get_or_init(|| regex::Regex::new(r"://[^/@]*@").expect("userinfo pattern"))
}

/// `llm_client.clean`: key-shaped tokens shortened, URL credentials dropped.
pub fn clean(s: &str) -> String {
    let s = userinfo_re().replace_all(s, "://");
    key_re().replace_all(&s, "${1}...${2}").into_owned()
}

/// Where one call goes: `kind` is `messages` or `chat`.
#[derive(Debug, Clone)]
pub struct Wire {
    pub kind: &'static str,
    pub model: String,
    pub url: String,
    pub headers: Vec<(&'static str, String)>,
}

fn first(get: &dyn Fn(&str) -> Option<String>, names: &[&str]) -> String {
    for n in names {
        if let Some(v) = get(n) {
            if !v.is_empty() {
                return v;
            }
        }
    }
    String::new()
}

fn or(a: &str, b: &str) -> String {
    if a.is_empty() {
        b.to_string()
    } else {
        a.to_string()
    }
}

fn join(base: &str, suffix: &str) -> String {
    let base = base.trim_end_matches('/');
    if base.ends_with(suffix) {
        base.to_string()
    } else {
        format!("{base}{suffix}")
    }
}

fn ollama_url(base: &str) -> String {
    let base = base.trim_end_matches('/');
    if base.ends_with("/chat/completions") {
        base.to_string()
    } else if base.ends_with("/v1") {
        format!("{base}/chat/completions")
    } else {
        format!("{base}/v1/chat/completions")
    }
}

/// `llm_client.resolve_wire`. The error is the oracle's text.
pub fn resolve(model: &str, base_url: &str, api_key: &str, get: &dyn Fn(&str) -> Option<String>) -> Result<Wire, String> {
    let mut h: Vec<(&'static str, String)> = vec![
        ("accept", "application/json".into()),
        ("accept-encoding", "identity".into()),
        ("content-type", "application/json".into()),
        ("user-agent", "opendaisugi".into()),
    ];
    if model.starts_with("anthropic/") || model.starts_with("claude") {
        let base = or(base_url, &first(get, &["ANTHROPIC_API_BASE", "ANTHROPIC_BASE_URL"]));
        let url = join(&or(&base, "https://api.anthropic.com"), "/v1/messages");
        h.push(("anthropic-version", "2023-06-01".into()));
        let key = or(api_key, &first(get, &["ANTHROPIC_API_KEY"]));
        if !key.is_empty() {
            h.push(("x-api-key", key));
        } else {
            let tok = first(get, &["ANTHROPIC_AUTH_TOKEN"]);
            if tok.is_empty() {
                return Err(clean(NO_KEY_TEXT));
            }
            h.push(("authorization", format!("Bearer {tok}")));
        }
        let name = model.strip_prefix("anthropic/").unwrap_or(model);
        return Ok(Wire { kind: "messages", model: name.into(), url, headers: h });
    }
    if let Some(name) = model.strip_prefix("openai/") {
        let base = or(base_url, &first(get, &["OPENAI_API_BASE", "OPENAI_BASE_URL"]));
        let url = join(&or(&base, "https://api.openai.com/v1"), "/chat/completions");
        let key = or(api_key, &first(get, &["OPENAI_API_KEY"]));
        if !key.is_empty() {
            h.push(("authorization", format!("Bearer {key}")));
        }
        return Ok(Wire { kind: "chat", model: name.into(), url, headers: h });
    }
    for prefix in ["ollama/", "ollama_chat/"] {
        if let Some(name) = model.strip_prefix(prefix) {
            let base = or(base_url, &first(get, &["OLLAMA_API_BASE"]));
            if !api_key.is_empty() {
                h.push(("authorization", format!("Bearer {api_key}")));
            }
            let url = ollama_url(&or(&base, "http://localhost:11434"));
            return Ok(Wire { kind: "chat", model: name.into(), url, headers: h });
        }
    }
    Err(clean(&format!("no model wire for {}: name it anthropic/<model>, openai/<model> or ollama/<model>", repr(model))))
}

/// `llm_client.resolve_timeout` with no timeout from the call.
pub fn timeout(get: &dyn Fn(&str) -> Option<String>) -> f64 {
    if let Some(v) = get("OPENDAISUGI_LLM_TIMEOUT") {
        if let Some(t) = crate::gate::argparse::py_float(&v) {
            if t.is_finite() && t > 0.0 {
                return t;
            }
        }
    }
    DEFAULT_TIMEOUT
}

/// The optional keys of a request. `thinking_budget` > 0 is the Messages
/// wire's {"type": "enabled", "budget_tokens": N}, which also raises the
/// default max_tokens by N; `reasoning_effort` is the chat wire's
/// reasoning_effort, left out when empty.
#[derive(Default)]
pub struct Opts {
    pub max_tokens: Option<i64>,
    pub temperature: Option<i64>,
    pub json_object: bool,
    pub thinking_budget: i64,
    pub reasoning_effort: String,
}

fn js(s: &str) -> String {
    dumps(&Value::Str(s.to_string()), true)
}

fn list(msgs: &[&(String, String)]) -> String {
    let items: Vec<String> =
        msgs.iter().map(|(role, content)| format!("{{\"role\":{},\"content\":{}}}", js(role), js(content))).collect();
    format!("[{}]", items.join(","))
}

/// `llm_client.request_body`: compact JSON, every non-ASCII character
/// escaped.
pub fn body(w: &Wire, msgs: &[(String, String)], o: &Opts) -> String {
    let mut b = format!("{{\"model\":{}", js(&w.model));
    if w.kind == "messages" {
        let system: Vec<&str> = msgs.iter().filter(|(r, _)| r == "system").map(|(_, c)| c.as_str()).collect();
        let rest: Vec<&(String, String)> = msgs.iter().filter(|(r, _)| r != "system").collect();
        b.push_str(&format!(",\"max_tokens\":{}", o.max_tokens.unwrap_or(DEFAULT_MAX_TOKENS + o.thinking_budget.max(0))));
        if !system.is_empty() {
            b.push_str(&format!(",\"system\":{}", js(&system.join("\n\n"))));
        }
        b.push_str(&format!(",\"messages\":{}", list(&rest)));
        if let Some(t) = o.temperature {
            b.push_str(&format!(",\"temperature\":{t}"));
        }
        if o.thinking_budget > 0 {
            b.push_str(&format!(",\"thinking\":{{\"type\":\"enabled\",\"budget_tokens\":{}}}", o.thinking_budget));
        }
    } else {
        let all: Vec<&(String, String)> = msgs.iter().collect();
        b.push_str(&format!(",\"messages\":{}", list(&all)));
        if let Some(n) = o.max_tokens {
            b.push_str(&format!(",\"max_tokens\":{n}"));
        }
        if let Some(t) = o.temperature {
            b.push_str(&format!(",\"temperature\":{t}"));
        }
        if o.json_object {
            b.push_str(",\"response_format\":{\"type\":\"json_object\"}");
        }
        if !o.reasoning_effort.is_empty() {
            b.push_str(&format!(",\"reasoning_effort\":{}", js(&o.reasoning_effort)));
        }
    }
    b.push('}');
    b
}

/// `llm_client.Reply`.
pub struct Reply {
    pub text: String,
    pub cut: bool,
    /// The tokens the server reports: input plus output on the Messages
    /// wire, total_tokens on the chat wire; None when it reports none.
    pub tokens: Option<i64>,
    /// `Reply.input_tokens` and `output_tokens`: input_tokens and
    /// output_tokens on the Messages wire, prompt_tokens and
    /// completion_tokens on the chat wire.
    pub input_tokens: Option<i64>,
    pub output_tokens: Option<i64>,
}

/// `llm_client._int`: an int that is not a bool, else None.
fn usage_int(usage: &Value, key: &str) -> Option<i64> {
    match usage {
        Value::Obj(o) => match o.value(key) {
            Value::Int(t) => t.parse::<i64>().ok(),
            _ => None,
        },
        _ => None,
    }
}

fn unreadable(text: &str) -> String {
    clean(&format!("the model reply could not be read: {}", head(text, 200)))
}

/// `llm_client.read_reply`. The error is the oracle's text.
pub fn read_reply(w: &Wire, status: u16, text: &str) -> Result<Reply, String> {
    if !(200..=299).contains(&status) {
        return Err(clean(&format!("the model server answered HTTP {status}: {}", head(text, 500))));
    }
    let o = match loads_py(text, 900) {
        Ok(Value::Obj(o)) => o,
        _ => return Err(unreadable(text)),
    };
    let s = |v: &Value, want: &str| matches!(v, Value::Str(x) if x == want);
    if w.kind == "messages" {
        let Value::List(blocks) = o.value("content") else {
            return Err(unreadable(text));
        };
        let mut parts = String::new();
        for bl in blocks {
            if let Value::Obj(b) = bl {
                if s(b.value("type"), "text") {
                    if let Value::Str(t) = b.value("text") {
                        parts.push_str(t);
                    }
                }
            }
        }
        let usage = o.value("usage");
        let tokens = match (usage_int(usage, "input_tokens"), usage_int(usage, "output_tokens")) {
            (Some(i), Some(out)) => Some(i.wrapping_add(out)),
            _ => None,
        };
        return Ok(Reply {
            text: parts,
            cut: s(o.value("stop_reason"), "max_tokens"),
            tokens,
            input_tokens: usage_int(usage, "input_tokens"),
            output_tokens: usage_int(usage, "output_tokens"),
        });
    }
    let Value::List(choices) = o.value("choices") else {
        return Err(unreadable(text));
    };
    let Some(Value::Obj(ch)) = choices.first() else {
        return Err(unreadable(text));
    };
    let Value::Obj(msg) = ch.value("message") else {
        return Err(unreadable(text));
    };
    let content = match msg.get("content") {
        None | Some(Value::Null) => String::new(),
        Some(Value::Str(t)) => t.clone(),
        Some(_) => return Err(unreadable(text)),
    };
    let usage = o.value("usage");
    Ok(Reply {
        text: content,
        cut: s(ch.value("finish_reason"), "length"),
        tokens: usage_int(usage, "total_tokens"),
        input_tokens: usage_int(usage, "prompt_tokens"),
        output_tokens: usage_int(usage, "completion_tokens"),
    })
}

/// Why a POST did not come back with a reply.
pub enum PostErr {
    /// The oracle's text.
    Model(String),
    /// A call this binary does not make the oracle's way.
    Unported(String),
}

/// One POST as httpx sends it: the proxies the environment names, no
/// redirect followed. The status and the body decoded as UTF-8 with
/// replacement.
pub fn post(w: &Wire, body: &[u8], timeout: f64, proxies: &netproxy::Httpx) -> Result<(u16, String), PostErr> {
    let headers: Vec<(&str, String)> = w.headers.iter().map(|(k, v)| (*k, v.clone())).collect();
    let m = |s: String| PostErr::Model(clean(&s));
    match http::post(&w.url, &headers, body, timeout, proxies) {
        Ok(a) => match a.encoding {
            Some(e) if !e.eq_ignore_ascii_case("identity") => Err(PostErr::Unported(format!("a reply with content-encoding {e}"))),
            _ => Ok((a.status, a.text)),
        },
        Err(http::Fail::Timeout) => Err(m(format!("the model call to {} timed out", w.url))),
        Err(http::Fail::Proxy(code, reason)) => Err(m(format!("the proxy refused the model call: {code} {reason}"))),
        Err(http::Fail::Fails(t)) => Err(m(format!("the model call could not start: {t}"))),
        // httpx.InvalidURL is not an error post() catches: it reaches the
        // caller as it is.
        Err(http::Fail::Invalid(t)) => Err(m(t)),
        Err(http::Fail::Unported(why)) => Err(PostErr::Unported(why)),
        Err(http::Fail::Other(_) | http::Fail::Refused | http::Fail::Write(_)) => {
            Err(m(format!("could not reach the model server at {}", w.url)))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn env(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let m: HashMap<String, String> = pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect();
        move |k: &str| m.get(k).cloned()
    }

    /// The same bytes as tests/test_llm.py::test_request_bodies_are_compact_ascii_json.
    #[test]
    fn the_request_bodies() {
        let w = resolve("anthropic/m", "", "", &env(&[("ANTHROPIC_API_KEY", "k")])).unwrap();
        let msgs = vec![("system".to_string(), "S é".to_string()), ("user".to_string(), "U 😀".to_string())];
        assert_eq!(
            body(&w, &msgs, &Opts { max_tokens: Some(200), temperature: Some(0), json_object: false, ..Default::default() }),
            r#"{"model":"m","max_tokens":200,"system":"S \u00e9","messages":[{"role":"user","content":"U \ud83d\ude00"}],"temperature":0}"#
        );
        let c = resolve("ollama/m", "", "", &env(&[])).unwrap();
        assert_eq!(
            body(&c, &msgs, &Opts { max_tokens: None, temperature: None, json_object: true, ..Default::default() }),
            r#"{"model":"m","messages":[{"role":"system","content":"S \u00e9"},{"role":"user","content":"U \ud83d\ude00"}],"response_format":{"type":"json_object"}}"#
        );
    }

    /// The same URLs as tests/test_llm.py::test_urls.
    #[test]
    fn the_urls() {
        for (model, base, url) in [
            ("anthropic/m", "", "https://api.anthropic.com/v1/messages"),
            ("anthropic/m", "http://h:1/", "http://h:1/v1/messages"),
            ("anthropic/m", "http://h:1/v1/messages/", "http://h:1/v1/messages"),
            ("anthropic/m", "http://h:1/V1/MESSAGES", "http://h:1/V1/MESSAGES/v1/messages"),
            ("openai/m", "", "https://api.openai.com/v1/chat/completions"),
            ("openai/m", "http://h:8080/v1", "http://h:8080/v1/chat/completions"),
            ("openai/m", "http://h:8080/v1/chat/completions", "http://h:8080/v1/chat/completions"),
            ("ollama/m", "", "http://localhost:11434/v1/chat/completions"),
            ("ollama/m", "http://h:11434/", "http://h:11434/v1/chat/completions"),
            ("ollama/m", "http://h:11434/v1", "http://h:11434/v1/chat/completions"),
        ] {
            assert_eq!(resolve(model, base, "", &env(&[("ANTHROPIC_API_KEY", "k")])).unwrap().url, url);
        }
        assert_eq!(
            resolve("gpt-4o", "", "", &env(&[])).unwrap_err(),
            "no model wire for 'gpt-4o': name it anthropic/<model>, openai/<model> or ollama/<model>"
        );
        assert_eq!(resolve("anthropic/m", "", "", &env(&[("ANTHROPIC_API_KEY", "")])).unwrap_err(), NO_KEY_TEXT);
    }

    #[test]
    fn clean_and_timeout() {
        assert_eq!(clean("at http://u:secret@h:1/x key sk-abcdefghijklmnopqrstuvwxyz0123"), "at http://h:1/x key sk-abcd...0123");
        for (v, want) in [(" 1.5 ", 1.5), ("0", 600.0), ("-1", 600.0), ("nan", 600.0), ("inf", 600.0), ("x", 600.0)] {
            assert_eq!(timeout(&env(&[("OPENDAISUGI_LLM_TIMEOUT", v)])), want, "{v}");
        }
    }

    #[test]
    fn replies() {
        let c = resolve("ollama/m", "", "", &env(&[])).unwrap();
        let r = read_reply(&c, 200, r#"{"choices":[{"message":{"content":null},"finish_reason":"length"}]}"#).ok().unwrap();
        assert!(r.text.is_empty() && r.cut);
        assert_eq!(read_reply(&c, 404, r#"{"error": "x"}"#).err().unwrap(), r#"the model server answered HTTP 404: {"error": "x"}"#);
        assert_eq!(read_reply(&c, 200, r#"{"choices": []}"#).err().unwrap(), r#"the model reply could not be read: {"choices": []}"#);
    }
}
