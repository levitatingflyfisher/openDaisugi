//! `yaml.safe_load` for text in the form `safe_dump` writes: block
//! mappings and sequences, empty flow collections, and plain,
//! single-quoted and double-quoted scalars, folded over lines as the
//! emitter folds them. It reads the text, then dumps what it read and
//! accepts the reading only when that gives the same text back (a final
//! line break aside), so a text in any other form, whatever safe_load
//! would make of it, is Unsupported rather than guessed at. The Go
//! client's `pyyaml/dumped.go` is the reference.

use super::yamldump::{resolve_tag, resolvers, safe_dump, Unsupported};
use crate::gate::py::text::cp_char;
use crate::gate::pyjson::{Object, Value};

type R<T> = Result<T, Unsupported>;

fn unsupported<T>(why: &str) -> R<T> {
    Err(Unsupported(why.into()))
}

/// The first character yaml.reader rejects, if any: anything outside
/// [\t\n\r\x20-\x7e\x85\xa0-퟿-�] and the astral planes.
/// A lone surrogate is rejected too.
fn non_printable(s: &str) -> bool {
    s.chars().any(|c| {
        let r = c as u32;
        if crate::gate::py::text::as_surrogate(c).is_some() {
            return true;
        }
        !(r == 9
            || r == 10
            || r == 13
            || (0x20..=0x7e).contains(&r)
            || r == 0x85
            || (0xa0..=0xd7ff).contains(&r)
            || (0xe000..=0xfffd).contains(&r)
            || (0x10000..=0x10ffff).contains(&r))
    })
}

pub fn load_dumped(text: &str) -> R<Value> {
    if non_printable(text) {
        return unsupported("a character the YAML reader rejects");
    }
    let mut r = Reader { lines: text.split('\n').map(|l| l.to_string()).collect(), i: 0 };
    if r.next()?.is_none() {
        return unsupported("an empty document");
    }
    let v = r.block(0)?;
    if r.next()?.is_some() {
        return unsupported("text after the document");
    }
    let again = safe_dump(&v)?;
    if again != text && again != format!("{text}\n") {
        return unsupported("a text not in the form yaml.safe_dump writes");
    }
    Ok(v)
}

struct Reader {
    lines: Vec<String>,
    i: usize,
}

fn indent_of(l: &str) -> usize {
    l.len() - l.trim_start_matches(' ').len()
}

fn is_seq_item(c: &str) -> bool {
    c == "-" || c.starts_with("- ")
}

impl Reader {
    /// The indent of the next line with content, or None at the end.
    fn next(&mut self) -> R<Option<usize>> {
        while self.i < self.lines.len() {
            let l = &self.lines[self.i];
            if l.contains(['\t', '\r']) {
                return unsupported("a tab or a carriage return");
            }
            if l.trim_start_matches(' ').is_empty() {
                self.i += 1;
                continue;
            }
            return Ok(Some(indent_of(l)));
        }
        Ok(None)
    }

    /// The collection whose first line is the next one, at an indent of
    /// at least `min`.
    fn block(&mut self, min: usize) -> R<Value> {
        let k = match self.next()? {
            Some(k) if k >= min => k,
            _ => return unsupported("a block that is not indented"),
        };
        if is_seq_item(&self.lines[self.i][k..]) {
            return self.sequence(k);
        }
        self.mapping(k)
    }

    fn sequence(&mut self, k: usize) -> R<Value> {
        let mut out = vec![];
        loop {
            let at = self.next()?;
            if at != Some(k) || !is_seq_item(&self.lines[self.i][k..]) {
                return Ok(Value::List(out));
            }
            if self.lines[self.i].len() == k + 1 {
                return unsupported("an empty sequence item");
            }
            out.push(self.inline(k + 2, k)?);
        }
    }

    /// The node that starts at column `col` of the current line, inside a
    /// parent at indent `parent`.
    fn inline(&mut self, col: usize, parent: usize) -> R<Value> {
        let s = self.lines[self.i][col..].to_string();
        if is_seq_item(&s) {
            self.lines[self.i] = format!("{}{s}", " ".repeat(col));
            return self.sequence(col);
        }
        if s == "[]" {
            self.i += 1;
            return Ok(Value::List(vec![]));
        }
        if s == "{}" {
            self.i += 1;
            return Ok(Value::Obj(Object::new()));
        }
        if is_mapping_entry(&s) {
            self.lines[self.i] = format!("{}{s}", " ".repeat(col));
            return self.mapping(col);
        }
        self.scalar(col, parent)
    }

    fn mapping(&mut self, k: usize) -> R<Value> {
        let mut out = Object::new();
        loop {
            let at = self.next()?;
            if at != Some(k) || is_seq_item(&self.lines[self.i][k..]) {
                return Ok(Value::Obj(out));
            }
            let l = self.lines[self.i].clone();
            let s = &l[k..];
            let (key, rest) = match s.as_bytes()[0] {
                b'\'' | b'"' => {
                    let end = match quoted_end(s) {
                        Some(e) if e < s.len() && s.as_bytes()[e] == b':' => e,
                        _ => return unsupported("a key that is not a simple key"),
                    };
                    let (key, _) = decode_line(&s[1..end - 1], s.as_bytes()[0] == b'"', true)?;
                    (key, s[end + 1..].to_string())
                }
                b'?' => return unsupported("a complex key"),
                _ => {
                    let c = match s.find(": ") {
                        Some(c) => c,
                        None => {
                            if !s.ends_with(':') {
                                return unsupported("a line with no key");
                            }
                            s.len() - 1
                        }
                    };
                    let key = s[..c].to_string();
                    if resolve_tag(&key) != "str" {
                        return unsupported("a key that is not a str");
                    }
                    (key, s[c + 1..].to_string())
                }
            };
            if out.get(&key).is_some() {
                return unsupported("a repeated key");
            }
            if rest.is_empty() {
                self.i += 1;
                let nx = self.next()?;
                match nx {
                    Some(n) if n > k => {
                        let v = self.block(n)?;
                        out.set(&key, v);
                    }
                    Some(n) if n == k && is_seq_item(&self.lines[self.i][k..]) => {
                        let v = self.sequence(k)?;
                        out.set(&key, v);
                    }
                    _ => {
                        out.set(&key, Value::Null);
                    }
                }
                continue;
            }
            if !rest.starts_with(' ') {
                return unsupported("a key not followed by a space");
            }
            let col = l.len() - rest.len() + 1;
            let v = self.inline(col, k)?;
            out.set(&key, v);
        }
    }

    /// A scalar from column `col` of the current line; its continuation
    /// lines are indented past `parent`.
    fn scalar(&mut self, col: usize, parent: usize) -> R<Value> {
        let s = self.lines[self.i][col..].to_string();
        if s.starts_with('\'') || s.starts_with('"') {
            return self.quoted(col, parent).map(Value::Str);
        }
        let mut parts = vec![s.trim_end_matches(' ').to_string()];
        self.i += 1;
        while self.i < self.lines.len() {
            let c = &self.lines[self.i];
            if c.trim_start_matches(' ').is_empty() || indent_of(c) <= parent {
                break;
            }
            parts.push(c.trim_matches(' ').to_string());
            self.i += 1;
        }
        resolve(&parts.join(" "))
    }

    /// A quoted scalar over as many lines as it takes.
    fn quoted(&mut self, col: usize, parent: usize) -> R<String> {
        let l = self.lines[self.i].clone();
        let q = l.as_bytes()[col];
        let double = q == b'"';
        let mut pieces: Vec<String> = vec![];
        let mut cur = l[col + 1..].to_string();
        loop {
            if let Some(end) = close_in(&cur, q) {
                pieces.push(cur[..end].to_string());
                if !cur[end + 1..].trim_end_matches(' ').is_empty() {
                    return unsupported("text after a quoted value");
                }
                self.i += 1;
                return fold_quoted(&pieces, double);
            }
            pieces.push(cur.clone());
            self.i += 1;
            if self.i >= self.lines.len() {
                return unsupported("a quoted value that does not close");
            }
            cur = self.lines[self.i].clone();
            if !cur.trim_start_matches(' ').is_empty() && indent_of(&cur) <= parent {
                return unsupported("a quoted value's line that is not indented");
            }
        }
    }
}

/// The text is a key and a colon. A quoted key is read to its end; a
/// plain key ends at ": " or a final ":".
fn is_mapping_entry(s: &str) -> bool {
    if s.is_empty() {
        return false;
    }
    let b = s.as_bytes();
    if b[0] == b'\'' || b[0] == b'"' {
        return matches!(quoted_end(s), Some(end) if end < s.len() && b[end] == b':');
    }
    s.contains(": ") || s.ends_with(':')
}

/// The index just past a one-line quoted token.
fn quoted_end(s: &str) -> Option<usize> {
    let b = s.as_bytes();
    let q = b[0];
    let mut i = 1;
    while i < b.len() {
        if q == b'"' && b[i] == b'\\' {
            i += 1;
        } else if b[i] == q && q == b'\'' && i + 1 < b.len() && b[i + 1] == b'\'' {
            i += 1;
        } else if b[i] == q {
            return Some(i + 1);
        }
        i += 1;
    }
    None
}

/// The index of the closing quote in one line of a quoted scalar.
fn close_in(s: &str, q: u8) -> Option<usize> {
    let b = s.as_bytes();
    let mut i = 0;
    while i < b.len() {
        if q == b'"' && b[i] == b'\\' {
            i += 1;
        } else if q == b'\'' && b[i] == b'\'' && i + 1 < b.len() && b[i + 1] == b'\'' {
            i += 1;
        } else if b[i] == q {
            return Some(i);
        }
        i += 1;
    }
    None
}

/// Joins the lines of a quoted scalar as YAML folds a flow scalar: white
/// space around a line break goes, a single break is a space, each empty
/// line between is a line feed, and in double quotes a break after a
/// backslash is nothing.
fn fold_quoted(pieces: &[String], double: bool) -> R<String> {
    let mut out = String::new();
    let (mut fold, mut empties) = (false, 0usize);
    for (n, s) in pieces.iter().enumerate() {
        let last = n == pieces.len() - 1;
        let s: &str = if n > 0 { s.trim_start_matches(' ') } else { s };
        if n > 0 && !last && s.is_empty() && fold {
            empties += 1;
            continue;
        }
        if fold {
            if empties == 0 {
                out.push(' ');
            } else {
                out.push_str(&"\n".repeat(empties));
            }
        }
        empties = 0;
        let (text, escaped_break) = decode_line(s, double, last)?;
        out.push_str(&text);
        fold = !last && !escaped_break;
    }
    Ok(out)
}

fn escape(e: u8) -> Option<&'static str> {
    Some(match e {
        b'0' => "\x00",
        b'a' => "\x07",
        b'b' => "\x08",
        b't' | b'\t' => "\t",
        b'n' => "\n",
        b'v' => "\x0b",
        b'f' => "\x0c",
        b'r' => "\r",
        b'e' => "\x1b",
        b' ' => " ",
        b'"' => "\"",
        b'/' => "/",
        b'\\' => "\\",
        b'N' => "\u{85}",
        b'_' => "\u{a0}",
        b'L' => "\u{2028}",
        b'P' => "\u{2029}",
        _ => return None,
    })
}

/// One line of a quoted scalar: its escapes, and its trailing white space
/// dropped when a plain line break follows. It reports a final
/// backslash, which escapes the line break.
fn decode_line(s: &str, double: bool, last: bool) -> R<(String, bool)> {
    let b = s.as_bytes();
    let mut out: Vec<u8> = vec![];
    let mut trail = 0usize;
    let mut i = 0;
    while i < b.len() {
        let c = b[i];
        if !double && c == b'\'' && i + 1 < b.len() && b[i + 1] == b'\'' {
            out.push(b'\'');
            i += 2;
            trail = 0;
            continue;
        }
        if double && c == b'\\' {
            if i + 1 >= b.len() {
                if last {
                    return unsupported("a backslash before the closing quote");
                }
                return Ok((String::from_utf8_lossy(&out).into_owned(), true));
            }
            let e = b[i + 1];
            if let Some(rep) = escape(e) {
                out.extend_from_slice(rep.as_bytes());
                i += 2;
                trail = 0;
                continue;
            }
            let width = match e {
                b'x' => 2,
                b'u' => 4,
                b'U' => 8,
                _ => return unsupported("an escape the reader does not read"),
            };
            if i + 2 + width > b.len() {
                return unsupported("an escape the reader does not read");
            }
            let hex = &s[i + 2..i + 2 + width];
            if !hex.bytes().all(|x| x.is_ascii_hexdigit()) {
                return unsupported("an escape the reader does not read");
            }
            let n = u32::from_str_radix(hex, 16).map_err(|_| Unsupported("an escape the reader does not read".into()))?;
            if n > 0x10ffff {
                return unsupported("an escape the reader does not read");
            }
            let mut buf = [0u8; 4];
            out.extend_from_slice(cp_char(n).encode_utf8(&mut buf).as_bytes());
            i += 2 + width;
            trail = 0;
            continue;
        }
        out.push(c);
        trail = if c == b' ' { trail + 1 } else { 0 };
        i += 1;
    }
    if !last {
        out.truncate(out.len() - trail);
    }
    match String::from_utf8(out) {
        Ok(t) => Ok((t, false)),
        Err(_) => unsupported("a line that is not UTF-8"),
    }
}

/// The implicit resolver and SafeConstructor for a plain scalar.
fn resolve(v: &str) -> R<Value> {
    let r = resolvers();
    if r.bool_re.is_match(v) {
        return Ok(Value::Bool(matches!(v.to_lowercase().as_str(), "yes" | "true" | "on")));
    }
    if r.float_re.is_match(v) {
        return yaml_float(v);
    }
    if r.int_re.is_match(v) {
        return yaml_int(v);
    }
    if v == "<<" {
        return unsupported("a merge key");
    }
    if r.null_re.is_match(v) {
        return Ok(Value::Null);
    }
    if r.time_re.is_match(v) {
        // A datetime value; safe_dump never writes one as plain text.
        return unsupported("a timestamp");
    }
    if v == "=" {
        return unsupported("a value SafeConstructor raises on");
    }
    Ok(Value::Str(v.to_string()))
}

fn yaml_int(v: &str) -> R<Value> {
    let v = v.replace('_', "");
    let (neg, v) = match v.as_bytes().first() {
        Some(b'-') => (true, &v[1..]),
        Some(b'+') => (false, &v[1..]),
        _ => (false, &v[..]),
    };
    // Only the decimal form round-trips through safe_dump; the others
    // are refused when dumped again, so they need not be read exactly.
    if v == "0" {
        return Ok(Value::Int("0".into()));
    }
    if v.starts_with("0b") || v.starts_with("0x") || v.starts_with('0') || v.contains(':') {
        return unsupported("an int not in decimal form");
    }
    if v.is_empty() || !v.bytes().all(|b| b.is_ascii_digit()) {
        return unsupported("an int with no digits");
    }
    Ok(Value::Int(if neg { format!("-{v}") } else { v.to_string() }))
}

fn yaml_float(v: &str) -> R<Value> {
    let v = v.replace('_', "").to_lowercase();
    let (sign, v) = match v.as_bytes().first() {
        Some(b'-') => (-1.0, &v[1..]),
        Some(b'+') => (1.0, &v[1..]),
        _ => (1.0, &v[..]),
    };
    if v == ".inf" {
        return Ok(Value::Float(sign * f64::INFINITY));
    }
    if v == ".nan" {
        return Ok(Value::Float(f64::NAN));
    }
    if v.contains(':') {
        return unsupported("a base 60 float");
    }
    match v.parse::<f64>() {
        Ok(f) => Ok(Value::Float(sign * f)),
        Err(_) => unsupported("a float the reader does not read"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_dump_reads_back() {
        let mut inner = Object::new();
        inner.set("a", Value::Str("x y".into()));
        inner.set("b", Value::List(vec![Value::Int("1".into()), Value::Float(2.5), Value::Null, Value::Bool(true)]));
        inner.set("long", Value::Str("word ".repeat(40).trim_end().to_string()));
        inner.set("uni", Value::Str("café\nnext line  ".into()));
        let mut o = Object::new();
        o.set("daisugi", Value::Obj(inner));
        let v = Value::Obj(o);
        let text = safe_dump(&v).unwrap();
        let back = load_dumped(&text).unwrap();
        assert_eq!(back, v, "{text}");
    }

    #[test]
    fn other_yaml_is_unsupported() {
        assert!(load_dumped("a: &x 1\n").is_err());
        assert!(load_dumped("a:   1\n").is_err());
        assert!(load_dumped("a: 0x10\n").is_err());
    }
}
