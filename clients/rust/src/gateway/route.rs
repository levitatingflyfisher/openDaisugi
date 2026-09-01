//! `opendaisugi.routing`'s difficulty heuristic and `opendaisugi.gateway`'s
//! `route_turn` with the text helpers it reads.

use super::pyops::{eq_str, get, iterate, py_len, reversed_of, Item, PyR, Raise, MAX_PY_DEPTH};
use crate::gate::py::text;
use crate::gate::pyjson::Value;

pub const DEFAULT_CHEAP_MODEL: &str = "claude-haiku-4-5";
pub const DEFAULT_FRONTIER_MODEL: &str = "claude-opus-4-8";
const HARD_THRESHOLD: f64 = 0.5;
/// STICKY_PREFIX_THRESHOLD_TOKENS (ADR-0015).
pub const STICKY_PREFIX_THRESHOLD: i64 = 4096;

const HARD_SIGNALS: &[&str] = &[
    "architect", "architecture", "design", "refactor", "migrat", "concurren", "deadlock", "race condition",
    "distributed", "consensus", "optimi", "security", "vulnerab", "schema", "algorithm", "prove", "proof", "debug",
    "root cause", "thread-saf", "scal",
];

/// `min(a, b)`: a unless b is smaller.
fn py_min(a: f64, b: f64) -> f64 {
    if b < a {
        b
    } else {
        a
    }
}

/// `routing.estimate_difficulty`: a 0..1 score from the task's length and
/// its hard-signal words.
pub fn estimate_difficulty(task: &str) -> f64 {
    let t = text::lower(task);
    let length = py_min(text::len(&t) as f64 / 400.0, 0.5);
    let hits = HARD_SIGNALS.iter().filter(|s| t.contains(*s)).count();
    let signal = py_min(hits as f64 * 0.25, 0.6);
    py_min(length + signal, 1.0)
}

/// RouteDecision. The models hold whatever JSON value the body named,
/// since the oracle never checks its type.
#[derive(Debug, Clone)]
pub struct Decision {
    pub tier: String,
    pub model: Value,
    pub requested_model: Value,
    pub difficulty: f64,
    pub downgraded: bool,
    pub reason: String,
}

/// `_message_text`: a str content, or the text of its text blocks joined
/// with spaces.
pub fn message_text(content: Option<&Value>) -> PyR<String> {
    match content {
        None => Ok(String::new()),
        Some(Value::Str(s)) => Ok(s.clone()),
        Some(Value::List(l)) => {
            let mut out: Vec<&str> = vec![];
            for b in l {
                let Value::Obj(o) = b else { continue };
                if !eq_str(o.get("type"), "text") {
                    continue;
                }
                match o.get("text") {
                    None => {}
                    Some(p) if !p.truthy() => {}
                    Some(Value::Str(s)) => out.push(s),
                    Some(_) => return Err(Raise::Raise),
                }
            }
            Ok(out.join(" "))
        }
        Some(_) => Ok(String::new()),
    }
}

static EMPTY_LIST: Value = Value::List(Vec::new());

fn messages_value(body: &Value) -> PyR<&Value> {
    Ok(get(body, "messages")?.unwrap_or(&EMPTY_LIST))
}

/// The role of one message; a message that is not a dict raises.
fn role_is_user(msg: &Item) -> PyR<bool> {
    match msg.obj() {
        Some(o) => Ok(eq_str(o.get("role"), "user")),
        None => Err(Raise::Raise),
    }
}

/// `_latest_user_text`: the latest user message with text.
pub fn latest_user_text(body: &Value) -> PyR<String> {
    for msg in reversed_of(messages_value(body)?)? {
        if !role_is_user(&msg)? {
            continue;
        }
        let t = message_text(msg.obj().and_then(|o| o.get("content")))?;
        if !text::strip(&t).is_empty() {
            return Ok(t);
        }
    }
    Ok(String::new())
}

/// `_new_user_text`: the last user message's own text.
pub fn new_user_text(body: &Value) -> PyR<String> {
    for msg in reversed_of(messages_value(body)?)? {
        if !role_is_user(&msg)? {
            continue;
        }
        return message_text(msg.obj().and_then(|o| o.get("content")));
    }
    Ok(String::new())
}

/// `conversation_key`: the first user text's SHA-256, 16 hex digits, or
/// "no-user-text".
pub fn conversation_key(body: &Value) -> PyR<String> {
    for msg in iterate(messages_value(body)?)? {
        if !role_is_user(&msg)? {
            continue;
        }
        let t = message_text(msg.obj().and_then(|o| o.get("content")))?;
        let s = text::strip(&t);
        if !s.is_empty() {
            let b = super::pyops::encode_utf8(s)?;
            return Ok(sha256_hex16(b));
        }
    }
    Ok("no-user-text".into())
}

/// `_block_chars`.
fn block_chars(block: &Value, depth: usize) -> PyR<usize> {
    if depth > MAX_PY_DEPTH {
        return Err(Raise::TooDeep);
    }
    match block {
        Value::Str(s) => Ok(text::len(s)),
        Value::Obj(b) => {
            let mut n = 0;
            if let Some(t) = b.get("text") {
                if t.truthy() {
                    n = py_len(t)?;
                }
            }
            match b.get("content") {
                Some(Value::Str(s)) => n += text::len(s),
                Some(Value::List(l)) => {
                    for x in l {
                        n += block_chars(x, depth + 1)?;
                    }
                }
                _ => {}
            }
            Ok(n)
        }
        _ => Ok(0),
    }
}

/// `estimate_prefix_tokens`: the system prompt's and every message's
/// characters, divided by four.
pub fn estimate_prefix_tokens(body: &Value) -> PyR<i64> {
    let mut chars = 0usize;
    match get(body, "system")? {
        Some(Value::Str(s)) => chars += text::len(s),
        Some(Value::List(l)) => {
            for b in l {
                chars += block_chars(b, 1)?;
            }
        }
        _ => {}
    }
    for msg in iterate(messages_value(body)?)? {
        let o = msg.obj().ok_or(Raise::Raise)?;
        match o.get("content") {
            Some(Value::Str(s)) => chars += text::len(s),
            Some(Value::List(l)) => {
                for b in l {
                    chars += block_chars(b, 1)?;
                }
            }
            _ => {}
        }
    }
    Ok((chars / 4) as i64)
}

/// `format(x, ".2f")`.
pub fn f2(x: f64) -> String {
    crate::garden::format_f(x, 2)
}

/// `route_turn` with the default sticky threshold; sticky is the
/// conversation's last routed model, None when there is none.
pub fn route_turn(body: &Value, cheap: &str, local: &str, sticky: Option<&Value>) -> PyR<Decision> {
    let requested = get(body, "model")?.cloned().unwrap_or_else(|| Value::Str(String::new()));
    let t = latest_user_text(body)?;
    let mk = |tier: &str, model: Value, d: f64, down: bool, reason: String| Decision {
        tier: tier.into(),
        model,
        requested_model: requested.clone(),
        difficulty: d,
        downgraded: down,
        reason,
    };
    if text::strip(&t).is_empty() {
        return Ok(mk("tier2-frontier", requested.clone(), 0.0, false, "no routing signal; keep the requested model".into()));
    }
    let d = estimate_difficulty(&t);
    if d >= HARD_THRESHOLD {
        return Ok(mk("tier2-frontier", requested.clone(), d, false, format!("hard turn ({}); keep the requested model", f2(d))));
    }
    if !local.is_empty() {
        return Ok(mk(
            "tier1-local",
            Value::Str(local.into()),
            d,
            !eq_str(Some(&requested), local),
            format!(
                "easy turn ({}); route to the local model \u{2014} zero quota, cache stickiness never blocks the local rung",
                f2(d)
            ),
        ));
    }
    if eq_str(sticky, cheap) {
        return Ok(mk(
            "tier1-cheap",
            Value::Str(cheap.into()),
            d,
            !eq_str(Some(&requested), cheap),
            format!("easy turn ({}); sticky to the cheap model \u{2014} this conversation's warm cache is the cheap one", f2(d)),
        ));
    }
    let prefix = estimate_prefix_tokens(body)?;
    if prefix >= STICKY_PREFIX_THRESHOLD {
        return Ok(mk(
            "tier2-frontier",
            requested.clone(),
            d,
            false,
            format!(
                "easy turn ({}) but sticky: ~{prefix} prefix tokens cached against the requested model \u{2014} \
                 forfeiting 0.1x reads (and re-writing on return) costs more than the downgrade saves",
                f2(d)
            ),
        ));
    }
    Ok(mk(
        "tier1-cheap",
        Value::Str(cheap.into()),
        d,
        !eq_str(Some(&requested), cheap),
        format!("easy turn ({}); route to the cheap model", f2(d)),
    ))
}

/// The SHA-256 of b, its first 16 hex digits, as the gateway's short ids
/// take it.
pub fn sha256_hex16(b: &[u8]) -> String {
    crate::gate::sha256::hexdigest(b)[..16].to_string()
}
