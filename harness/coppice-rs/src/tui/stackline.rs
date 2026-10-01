//! The one line a window shows under its header: what the agent runs on,
//! its last gate verdict, and its tokens.

use crate::textwidth;

use super::model::{num, obj, s, Obj};

/// The stack, gate and tokens objects of one pane.list row as one line. A
/// field the server does not send is left out.
pub fn stack_text(p: &Obj) -> String {
    let mut parts: Vec<String> = Vec::new();
    if let Some(st) = obj(p, "stack") {
        for k in ["loop", "model", "router"] {
            let v = s(st, k);
            if !v.is_empty() {
                parts.push(v);
            }
        }
        if let Some(d) = obj(st, "daisugi") {
            let mode = s(d, "mode");
            if !mode.is_empty() {
                let mut text = format!("daisugi {mode}");
                if d.get("armed").and_then(serde_json::Value::as_bool) == Some(false) {
                    text.push_str(" disarmed");
                }
                parts.push(text);
            }
        }
    }
    if let Some(g) = obj(p, "gate") {
        let dec = s(g, "decision");
        if !dec.is_empty() {
            let mut text = format!("gate {dec}");
            let tool = s(g, "tool");
            if !tool.is_empty() {
                text.push(' ');
                text.push_str(&tool);
            }
            parts.push(text);
        }
    }
    if let Some(t) = obj(p, "tokens") {
        let mut toks = Vec::new();
        for (key, word) in [
            ("fresh", "fresh"),
            ("cache_read", "cache read"),
            ("cache_write", "cache write"),
            ("out", "out"),
        ] {
            if let Some(n) = num(t, key) {
                toks.push(format!("{} {word}", count_text(n)));
            }
        }
        if !toks.is_empty() {
            parts.push(format!("tokens {}", toks.join(" ")));
        }
    }
    parts
        .iter()
        .map(|p| textwidth::printable(p, 0))
        .collect::<Vec<_>>()
        .join(" · ")
}

/// A token count in few cells: 950, 12k, 1.2M.
pub fn count_text(n: f64) -> String {
    if n < 1000.0 {
        format!("{}", n as i64)
    } else if n < 1e6 {
        format!("{}k", (n / 1000.0) as i64)
    } else {
        format!("{:.1}M", n / 1e6)
    }
}
