//! JSON text the way Go's encoding/json writes it, so a Rust reply has the
//! same bytes as a Go reply.
//!
//! Objects keep the order their keys were put in (serde_json's
//! preserve_order). A Go struct writes its fields in declared order and a Go
//! map sorts its keys, so a caller builds a struct reply in field order and
//! a map reply with [`map`], which sorts. Strings escape `<`, `>`, `&`,
//! U+2028 and U+2029, as Go does by default. Numbers keep the text they
//! were made with (serde_json's arbitrary_precision), and [`float`] makes
//! one with Go's float format.

use serde_json::{Map, Number, Value};

const HEX: &[u8; 16] = b"0123456789abcdef";

/// Appends s as a Go JSON string. Bytes that are not UTF-8 never reach
/// here: a Rust str is UTF-8.
pub fn push_str(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '"' => out.push_str("\\\""),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' | '>' | '&' => {
                let b = c as u8;
                out.push_str("\\u00");
                out.push(HEX[(b >> 4) as usize] as char);
                out.push(HEX[(b & 0xf) as usize] as char);
            }
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c if (c as u32) < 0x20 => {
                let b = c as u8;
                out.push_str("\\u00");
                out.push(HEX[(b >> 4) as usize] as char);
                out.push(HEX[(b & 0xf) as usize] as char);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// Appends v as Go's json.Marshal would write it.
pub fn push(out: &mut String, v: &Value) {
    match v {
        Value::Null => out.push_str("null"),
        Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
        Value::Number(n) => out.push_str(&n.to_string()),
        Value::String(s) => push_str(out, s),
        Value::Array(a) => {
            out.push('[');
            for (i, x) in a.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                push(out, x);
            }
            out.push(']');
        }
        Value::Object(m) => {
            out.push('{');
            for (i, (k, x)) in m.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                push_str(out, k);
                out.push(':');
                push(out, x);
            }
            out.push('}');
        }
    }
}

/// v as one line of Go JSON text.
pub fn marshal(v: &Value) -> String {
    let mut s = String::new();
    push(&mut s, v);
    s
}

fn newline(out: &mut String, indent: &str, depth: usize) {
    out.push('\n');
    for _ in 0..depth {
        out.push_str(indent);
    }
}

fn push_indent(out: &mut String, v: &Value, indent: &str, depth: usize) {
    match v {
        Value::Array(a) if !a.is_empty() => {
            out.push('[');
            for (i, x) in a.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                newline(out, indent, depth + 1);
                push_indent(out, x, indent, depth + 1);
            }
            newline(out, indent, depth);
            out.push(']');
        }
        Value::Object(m) if !m.is_empty() => {
            out.push('{');
            for (i, (k, x)) in m.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                newline(out, indent, depth + 1);
                push_str(out, k);
                out.push_str(": ");
                push_indent(out, x, indent, depth + 1);
            }
            newline(out, indent, depth);
            out.push('}');
        }
        _ => push(out, v),
    }
}

/// v as Go's json.MarshalIndent(v, "", indent) writes it: an empty object
/// or array stays on one line.
pub fn marshal_indent(v: &Value, indent: &str) -> String {
    let mut s = String::new();
    push_indent(&mut s, v, indent, 0);
    s
}

/// A Go float64 as encoding/json writes it: the shortest text that reads
/// back as f, in plain form between 1e-6 and 1e21 and in exponent form
/// outside, with no zero padding in the exponent.
pub fn float_text(f: f64) -> String {
    let abs = f.abs();
    if abs != 0.0 && (abs < 1e-6 || abs >= 1e21) {
        let s = format!("{f:e}");
        // Rust writes 1e21 and 1e-7; Go writes 1e+21 and 1e-7.
        match s.find('e') {
            Some(i) if !s[i + 1..].starts_with('-') => format!("{}e+{}", &s[..i], &s[i + 1..]),
            _ => s,
        }
    } else {
        let s = format!("{f}");
        // Rust writes -0 for negative zero, as Go does.
        s
    }
}

/// A JSON number with Go's float text.
pub fn float(f: f64) -> Value {
    if !f.is_finite() {
        // Go refuses to encode these. The callers only pass clock values.
        return Value::Null;
    }
    let text = float_text(f);
    Value::Number(text.parse::<Number>().unwrap_or_else(|_| Number::from(0)))
}

/// An object with its keys sorted, as Go writes a map[string]any.
pub fn map(pairs: Vec<(&str, Value)>) -> Value {
    let mut pairs = pairs;
    pairs.sort_by(|a, b| a.0.cmp(b.0));
    let mut m = Map::new();
    for (k, v) in pairs {
        m.insert(k.to_string(), v);
    }
    Value::Object(m)
}

/// An object with its keys in the order given, as Go writes a struct.
pub fn obj(pairs: Vec<(&str, Value)>) -> Value {
    let mut m = Map::new();
    for (k, v) in pairs {
        m.insert(k.to_string(), v);
    }
    Value::Object(m)
}

/// Raw JSON text the way Go's Marshal copies a json.RawMessage: whitespace
/// outside strings dropped, and `<`, `>`, `&`, U+2028 and U+2029 escaped.
/// The text must already be valid JSON.
pub fn compact(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len());
    let mut in_str = false;
    let mut esc = false;
    for c in raw.chars() {
        if in_str {
            match c {
                '<' | '>' | '&' => {
                    let b = c as u8;
                    out.push_str("\\u00");
                    out.push(HEX[(b >> 4) as usize] as char);
                    out.push(HEX[(b & 0xf) as usize] as char);
                }
                '\u{2028}' => out.push_str("\\u2028"),
                '\u{2029}' => out.push_str("\\u2029"),
                _ => out.push(c),
            }
            if esc {
                esc = false;
            } else if c == '\\' {
                esc = true;
            } else if c == '"' {
                in_str = false;
            }
            continue;
        }
        match c {
            ' ' | '\t' | '\n' | '\r' => {}
            '"' => {
                in_str = true;
                out.push(c);
            }
            _ => out.push(c),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn strings_escape_as_go_does() {
        let mut s = String::new();
        push_str(&mut s, "a<b>&\u{2028}\u{1}\u{8}\n\"\\é");
        let want = "\"a\\u003cb\\u003e\\u0026\\u2028\\u0001\\b\\n\\\"\\\\é\"";
        assert_eq!(s, want);
    }

    #[test]
    fn floats_follow_go() {
        assert_eq!(float_text(1790886519.113319), "1790886519.113319");
        assert_eq!(float_text(0.0366762), "0.0366762");
        assert_eq!(float_text(1e-7), "1e-7");
        assert_eq!(float_text(1e21), "1e+21");
        assert_eq!(float_text(3.0), "3");
        assert_eq!(float_text(0.0), "0");
    }

    #[test]
    fn map_sorts_and_obj_keeps_order() {
        assert_eq!(
            marshal(&map(vec![("b", json!(1)), ("a", json!(2))])),
            r#"{"a":2,"b":1}"#
        );
        assert_eq!(
            marshal(&obj(vec![("b", json!(1)), ("a", json!(2))])),
            r#"{"b":1,"a":2}"#
        );
    }

    #[test]
    fn compact_escapes_inside_strings_only() {
        assert_eq!(compact(" \"a<b\" "), "\"a\\u003cb\"");
        assert_eq!(compact("1e2"), "1e2");
        assert_eq!(compact("\"q\\\"<\""), "\"q\\\"\\u003c\"");
    }
}
