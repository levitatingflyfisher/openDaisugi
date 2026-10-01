//! `opendaisugi.route_model`: the smart end of the router slot. A model that
//! picks among the allowed names, and a model that grades a finished turn.
//! Both go through the gateway's own upstream path. The prompts are fixed
//! text; the bytes match the Python gateway's. The Go client's
//! `gateway/model.go` is the reference.

use std::io::Read;
use std::time::Duration;

use super::http;
use super::pyops::loads_bytes;
use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

const TEXT_LIMIT: usize = 2000;
const ASK_TIMEOUT: Duration = Duration::from_secs(15);

fn helper_body(model: &str, prompt: &str, max_tokens: i64) -> Vec<u8> {
    let mut msg = Object::new();
    msg.set("role", "user");
    msg.set("content", prompt);
    let mut body = Object::new();
    body.set("model", model);
    body.set("max_tokens", Value::Int(max_tokens.to_string()));
    body.set("messages", Value::List(vec![Value::Obj(msg)]));
    pyjson::dumps(&Value::Obj(body), true).into_bytes()
}

fn clip(s: &str) -> String {
    s.chars().take(TEXT_LIMIT).collect()
}

/// `picker_body`.
pub fn picker_body(router_model: &str, task: &str, candidates: &[String]) -> Vec<u8> {
    let prompt = format!(
        "Pick the model for this task. Allowed models: {}.\nReply with one name from the list and nothing else.\n\nTask:\n{}",
        candidates.join(", "),
        clip(task)
    );
    helper_body(router_model, &prompt, 20)
}

/// `parse_pick`: the allowed name the model answered, or None.
pub fn parse_pick(reply: Option<&str>, candidates: &[String]) -> Option<String> {
    let answer = text::strip(reply?).trim_matches(|c| "`'\".".contains(c)).to_string();
    candidates.iter().any(|c| *c == answer).then_some(answer)
}

/// `grader_body`.
pub fn grader_body(grader_model: &str, task: &str, answer: &str) -> Vec<u8> {
    let prompt = format!(
        "Task:\n{}\n\nAnswer:\n{}\n\nDid the answer do the task? Reply with one word: good or bad.",
        clip(task),
        clip(answer)
    );
    helper_body(grader_model, &prompt, 5)
}

/// `parse_grade`: good or bad, or None.
pub fn parse_grade(reply: Option<&str>) -> Option<&'static str> {
    let word = text::strip(reply?).trim_matches(|c| "`'\".".contains(c)).to_lowercase();
    match word.as_str() {
        "good" => Some("good"),
        "bad" => Some("bad"),
        _ => None,
    }
}

/// `reply_text`: the text of a Messages reply.
pub fn reply_text(v: &Value) -> Option<String> {
    let Value::Obj(o) = v else { return None };
    let Some(Value::List(content)) = o.get("content") else { return None };
    let mut out = String::new();
    let mut found = false;
    for b in content {
        let Value::Obj(bo) = b else { continue };
        if !super::pyops::eq_str(bo.get("type"), "text") {
            continue;
        }
        if let Some(Value::Str(t)) = bo.get("text") {
            out.push_str(t);
            found = true;
        }
    }
    found.then_some(out)
}

/// `off`: None when unset or "off".
pub fn off(v: Option<&str>) -> String {
    match v.map(text::strip) {
        Some(s) if !s.is_empty() && s.to_lowercase() != "off" => s.to_string(),
        _ => String::new(),
    }
}

/// The router model's answer, as `prepare` takes it: the picked name, or
/// the reason there is none.
#[derive(Debug, Clone, Default)]
pub struct Choice {
    pub name: String,
    pub why: String,
}

/// One short question through the upstream; the reply text, or None.
pub fn ask(url: &str, headers: &[(String, Vec<u8>)], body: &[u8]) -> Option<String> {
    let (url, headers, body) = (url.to_string(), headers.to_vec(), body.to_vec());
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let heads: Vec<(String, Vec<u8>)> =
            headers.into_iter().filter(|(k, _)| !k.eq_ignore_ascii_case("content-length")).collect();
        let got = http::post_timed(&url, &heads, &body, Some(ASK_TIMEOUT)).ok().and_then(|resp| {
            let (status, _hs, mut rd) = http::decoded(resp);
            let mut content = vec![];
            if status >= 400 || rd.read_to_end(&mut content).is_err() {
                return None;
            }
            reply_text(&loads_bytes(&content).ok()?)
        });
        let _ = tx.send(got);
    });
    rx.recv_timeout(ASK_TIMEOUT).ok().flatten()
}
