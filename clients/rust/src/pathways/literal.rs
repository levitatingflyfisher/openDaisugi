//! `ast.literal_eval` for the literals a model writes when it answers with
//! a Python dict instead of JSON: dicts, lists and tuples (read as lists),
//! a str in single or double quotes with the common escapes, decimal ints
//! and floats with an optional sign, True, False and None. Any other text
//! is not read, including literal forms this subset leaves out (triple
//! quotes, prefixes, bytes, sets, complex numbers, implicit
//! concatenation, hex ints). The Go client's `pyjson.LiteralEval` is the
//! reference.

use crate::gate::pyjson::{Object, Value};

struct Fail;

type R<T> = Result<T, Fail>;

struct P<'a> {
    s: &'a [u8],
    t: &'a str,
    i: usize,
}

/// The value of `text`, or `None` where this subset does not read it.
pub fn literal_eval(text: &str) -> Option<Value> {
    let mut p = P { s: text.as_bytes(), t: text, i: 0 };
    let v = (|| -> R<Value> {
        p.ws();
        let v = p.value(0)?;
        p.ws();
        Ok(v)
    })()
    .ok()?;
    if p.i != p.s.len() {
        return None;
    }
    Some(v)
}

fn is_ident(c: u8) -> bool {
    c == b'_' || c.is_ascii_alphanumeric() || c >= 0x80
}

impl P<'_> {
    /// Skips what the tokenizer skips inside brackets: spaces, tabs, form
    /// feeds, newlines, comments and a backslash that ends a line.
    fn ws(&mut self) {
        while self.i < self.s.len() {
            match self.s[self.i] {
                b' ' | b'\t' | b'\n' | b'\r' | 0x0c => self.i += 1,
                b'#' => {
                    while self.i < self.s.len() && self.s[self.i] != b'\n' {
                        self.i += 1;
                    }
                }
                b'\\' if self.s[self.i..].starts_with(b"\\\n") => self.i += 2,
                _ => return,
            }
        }
    }

    fn peek(&self) -> R<u8> {
        self.s.get(self.i).copied().ok_or(Fail)
    }

    fn value(&mut self, depth: usize) -> R<Value> {
        if depth > 200 {
            return Err(Fail);
        }
        self.ws();
        let c = self.peek()?;
        match c {
            b'{' => {
                self.i += 1;
                let mut o = Object::new();
                loop {
                    self.ws();
                    if self.peek()? == b'}' {
                        self.i += 1;
                        return Ok(Value::Obj(o));
                    }
                    // Only str keys reach a model.
                    let k = match self.value(depth + 1)? {
                        Value::Str(k) => k,
                        _ => return Err(Fail),
                    };
                    self.ws();
                    if self.peek()? != b':' {
                        return Err(Fail);
                    }
                    self.i += 1;
                    let v = self.value(depth + 1)?;
                    o.set(&k, v);
                    self.ws();
                    if self.peek()? == b',' {
                        self.i += 1;
                        continue;
                    }
                    if self.peek()? != b'}' {
                        return Err(Fail);
                    }
                }
            }
            b'[' | b'(' => {
                let end = if c == b'(' { b')' } else { b']' };
                self.i += 1;
                let mut xs = vec![];
                let mut trailing = false;
                loop {
                    self.ws();
                    if self.peek()? == end {
                        self.i += 1;
                        // (x) without a comma is x itself, not a tuple.
                        if c == b'(' && xs.len() == 1 && !trailing {
                            return Ok(xs.pop().unwrap_or(Value::Null));
                        }
                        return Ok(Value::List(xs));
                    }
                    xs.push(self.value(depth + 1)?);
                    trailing = false;
                    self.ws();
                    if self.peek()? == b',' {
                        self.i += 1;
                        trailing = true;
                        continue;
                    }
                    if self.peek()? != end {
                        return Err(Fail);
                    }
                }
            }
            b'\'' | b'"' => Ok(Value::Str(self.string()?)),
            b'-' | b'+' => {
                self.i += 1;
                self.ws();
                let x = self.number()?;
                if c == b'+' {
                    return Ok(x);
                }
                Ok(match x {
                    Value::Int(t) if t == "0" => Value::Int(t),
                    Value::Int(t) => match t.strip_prefix('-') {
                        Some(d) => Value::Int(d.to_string()),
                        None => Value::Int(format!("-{t}")),
                    },
                    Value::Float(f) => Value::Float(-f),
                    other => other,
                })
            }
            b'0'..=b'9' | b'.' => self.number(),
            _ => {
                for (word, v) in [("True", Value::Bool(true)), ("False", Value::Bool(false)), ("None", Value::Null)] {
                    if self.s[self.i..].starts_with(word.as_bytes()) {
                        let after = self.i + word.len();
                        if after < self.s.len() && is_ident(self.s[after]) {
                            return Err(Fail);
                        }
                        self.i = after;
                        return Ok(v);
                    }
                }
                Err(Fail)
            }
        }
    }

    /// A decimal int or float literal, underscores between digits.
    fn number(&mut self) -> R<Value> {
        let start = self.i;
        while self.i < self.s.len() {
            let c = self.s[self.i];
            let sign_after_e =
                (c == b'+' || c == b'-') && self.i > start && matches!(self.s[self.i - 1], b'e' | b'E');
            if c.is_ascii_digit() || c == b'_' || c == b'.' || c == b'e' || c == b'E' || sign_after_e {
                self.i += 1;
            } else {
                break;
            }
        }
        if self.i < self.s.len() && is_ident(self.s[self.i]) {
            return Err(Fail);
        }
        let text = &self.t[start..self.i];
        if text.is_empty()
            || text.contains("__")
            || text.starts_with('_')
            || text.ends_with('_')
            || text.contains("_.")
            || text.contains("._")
        {
            return Err(Fail);
        }
        let clean: String = text.chars().filter(|c| *c != '_').collect();
        if !clean.contains(['.', 'e', 'E']) {
            // A decimal int: no leading zero unless the value is zero.
            if clean.len() > 1 && clean.starts_with('0') && clean.trim_matches('0') != "" {
                return Err(Fail);
            }
            if !clean.bytes().all(|b| b.is_ascii_digit()) {
                return Err(Fail);
            }
            let d = clean.trim_start_matches('0');
            return Ok(Value::Int(if d.is_empty() { "0".into() } else { d.to_string() }));
        }
        // Rust's float reader takes what Go's does; a value past the float
        // range is an infinity, as in Python.
        if !float_syntax(&clean) {
            return Err(Fail);
        }
        clean.parse::<f64>().map(Value::Float).map_err(|_| Fail)
    }

    /// A one-line quoted string with Python's escapes.
    fn string(&mut self) -> R<String> {
        let q = self.s[self.i];
        if self.s[self.i..].starts_with(&[q, q, q]) {
            return Err(Fail);
        }
        self.i += 1;
        let mut b = String::new();
        loop {
            let c = self.peek()?;
            if c == q {
                self.i += 1;
                return Ok(b);
            }
            if c == b'\n' {
                return Err(Fail);
            }
            if c != b'\\' {
                let ch = self.t[self.i..].chars().next().ok_or(Fail)?;
                b.push(ch);
                self.i += ch.len_utf8();
                continue;
            }
            self.i += 1;
            let e = self.peek()?;
            self.i += 1;
            match e {
                b'\n' => {}
                b'\\' | b'\'' | b'"' => b.push(e as char),
                b'n' => b.push('\n'),
                b't' => b.push('\t'),
                b'r' => b.push('\r'),
                b'a' => b.push('\u{7}'),
                b'b' => b.push('\u{8}'),
                b'f' => b.push('\u{c}'),
                b'v' => b.push('\u{b}'),
                b'x' | b'u' | b'U' => {
                    let n = match e {
                        b'x' => 2,
                        b'u' => 4,
                        _ => 8,
                    };
                    let hex = self.t.get(self.i..self.i + n).ok_or(Fail)?;
                    if !hex.bytes().all(|h| h.is_ascii_hexdigit()) {
                        return Err(Fail);
                    }
                    let r = u32::from_str_radix(hex, 16).map_err(|_| Fail)?;
                    if r > 0x10FFFF {
                        return Err(Fail);
                    }
                    self.i += n;
                    b.push(crate::gate::py::text::cp_char(r));
                }
                b'0'..=b'7' => {
                    let j = self.i - 1;
                    let mut k = j;
                    while k < self.s.len() && k < j + 3 && (b'0'..=b'7').contains(&self.s[k]) {
                        k += 1;
                    }
                    let r = u32::from_str_radix(&self.t[j..k], 8).map_err(|_| Fail)?;
                    self.i = k;
                    b.push(crate::gate::py::text::cp_char(r));
                }
                _ => {
                    // An unknown escape keeps the backslash (with a warning).
                    b.push('\\');
                    self.i -= 1;
                }
            }
        }
    }
}

/// digits [. digits] [e [+-] digits], with digits on one side of the dot.
fn float_syntax(s: &str) -> bool {
    let b = s.as_bytes();
    let mut i = 0;
    let int = b.iter().take_while(|c| c.is_ascii_digit()).count();
    i += int;
    let mut frac = 0;
    if i < b.len() && b[i] == b'.' {
        i += 1;
        frac = b[i..].iter().take_while(|c| c.is_ascii_digit()).count();
        i += frac;
    }
    if int + frac == 0 {
        return false;
    }
    if i < b.len() && (b[i] == b'e' || b[i] == b'E') {
        i += 1;
        if i < b.len() && (b[i] == b'+' || b[i] == b'-') {
            i += 1;
        }
        let e = b[i..].iter().take_while(|c| c.is_ascii_digit()).count();
        if e == 0 {
            return false;
        }
        i += e;
    }
    i == b.len()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::dumps;

    fn lit(s: &str) -> Option<String> {
        literal_eval(s).map(|v| dumps(&v, false))
    }

    #[test]
    fn reads_the_literals_a_model_writes() {
        assert_eq!(lit("{'a': 1, \"b\": [True, None, (2,)], 'c': -1.5e3}").unwrap(), r#"{"a": 1, "b": [true, null, [2]], "c": -1500.0}"#);
        assert_eq!(lit("(1)").unwrap(), "1");
        assert_eq!(lit("'\\x41\\n\\101'").unwrap(), r#""A\nA""#);
        assert_eq!(lit("-0").unwrap(), "0");
        assert_eq!(lit("1_000").unwrap(), "1000");
        assert_eq!(lit("{'a': }"), None);
        assert_eq!(lit("0x10"), None);
        assert_eq!(lit("'''x'''"), None);
        assert_eq!(lit("{1: 2}"), None);
        assert_eq!(lit("01"), None);
        assert_eq!(lit("{'a': 1} x"), None);
    }
}
