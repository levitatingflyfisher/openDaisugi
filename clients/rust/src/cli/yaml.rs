//! YAML as PyYAML's SafeLoader reads it, inside a subset modelled exactly:
//! block mappings and sequences at any depth, flow sequences and mappings
//! on one line (or a whole flow document, such as JSON), plain scalars
//! (continued over more-indented lines), single- and double-quoted
//! scalars on one line, comments, and one leading "---". Everything else
//! (anchors, aliases, tags, block scalars, directives, several documents,
//! tabs, complex keys) is `Unsupported`, for the caller to refuse. The Go
//! client's `config/yaml.go` is the reference.

use regex::Regex;
use std::collections::HashMap;
use std::sync::OnceLock;

/// YAML outside the modelled subset.
#[derive(Debug, Clone, PartialEq)]
pub struct Unsupported;

type R<T> = Result<T, Unsupported>;

/// The type PyYAML gives a node.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Null,
    Bool,
    Int,
    Float,
    Str,
    Seq,
    Map,
}

/// One node. A mapping's keys keep their first position; a repeated key
/// takes its last value. Only string keys are kept by name; `non_str_keys`
/// counts the others.
#[derive(Debug, Clone, PartialEq)]
pub struct Node {
    pub kind: Kind,
    pub b: bool,
    /// An Int's digits, a Float's text, a Str's value.
    pub text: String,
    pub items: Vec<Node>,
    pub keys: Vec<String>,
    pub map: HashMap<String, Node>,
    pub non_str_keys: usize,
}

impl Node {
    pub fn of(kind: Kind) -> Node {
        Node { kind, b: false, text: String::new(), items: vec![], keys: vec![], map: HashMap::new(), non_str_keys: 0 }
    }

    fn text(kind: Kind, t: &str) -> Node {
        let mut n = Node::of(kind);
        n.text = t.to_string();
        n
    }

    fn boolean(b: bool) -> Node {
        let mut n = Node::of(Kind::Bool);
        n.b = b;
        n
    }
}

#[derive(Clone)]
struct Line {
    indent: usize,
    text: String,
}

struct Parser {
    lines: Vec<Line>,
    i: usize,
}

/// Reads one document.
pub fn parse(text: &str) -> R<Node> {
    if text.starts_with('\u{feff}') {
        return Err(Unsupported);
    }
    for r in text.chars() {
        let c = r as u32;
        if r == '\t'
            || r == '\r'
            || c == 0x7f
            || (c < 0x20 && r != '\n')
            || (0x80..=0x9f).contains(&c)
            || matches!(c, 0xfeff | 0xfffe | 0xffff | 0x2028 | 0x2029 | 0x85)
        {
            return Err(Unsupported);
        }
    }
    let mut p = Parser { lines: vec![], i: 0 };
    let mut started = false;
    for raw in text.split('\n') {
        let t = raw.trim_start_matches(' ');
        if t.is_empty() || t.starts_with('#') {
            continue;
        }
        let ind = raw.len() - t.len();
        if ind == 0 && (t.starts_with("---") || t.starts_with("...") || t.starts_with('%')) {
            if !started && (t == "---" || t.starts_with("--- #")) {
                started = true;
                continue;
            }
            return Err(Unsupported);
        }
        started = true;
        p.lines.push(Line { indent: ind, text: t.trim_end_matches(' ').to_string() });
    }
    if p.lines.is_empty() {
        return Ok(Node::of(Kind::Null));
    }
    let first = p.lines[0].text.as_bytes()[0];
    if first == b'[' || first == b'{' {
        // A flow document, such as JSON: its line breaks fold to spaces.
        let joined: Vec<&str> = p.lines.iter().map(|l| l.text.as_str()).collect();
        let joined = joined.join(" ");
        let (v, rest) = flow_node(&joined)?;
        if !only_comment(rest) {
            return Err(Unsupported);
        }
        return Ok(v);
    }
    let ind = p.lines[0].indent;
    let v = p.block(ind)?;
    if p.i != p.lines.len() {
        return Err(Unsupported);
    }
    Ok(v)
}

fn is_seq_line(t: &str) -> bool {
    t == "-" || t.starts_with("- ")
}

/// "key: rest" or "key:" in a block line. `None` when the line is not a
/// mapping entry.
fn split_key(t: &str) -> R<Option<(Node, String)>> {
    let b = t.as_bytes();
    if b.is_empty() {
        return Ok(None);
    }
    match b[0] {
        b'?' | b'&' | b'*' | b'!' | b'|' | b'>' | b'%' | b'@' | b'`' => return Err(Unsupported),
        b'\'' | b'"' => {
            let end = quoted_end(t)?;
            let after = &t[end..];
            if after == ":" || after.starts_with(": ") {
                let k = quoted(&t[..end])?;
                return Ok(Some((k, after[1..].trim_start_matches(' ').to_string())));
            }
            return Ok(None);
        }
        b'[' | b'{' => return Ok(None),
        _ => {}
    }
    // A plain key ends at the first ": " or a final ":".
    let k = match t.find(": ") {
        Some(k) => k,
        None if t.ends_with(':') => t.len() - 1,
        None => return Ok(None),
    };
    if let Some(c) = t.find(" #") {
        if c < k {
            return Ok(None);
        }
    }
    let key_text = t[..k].trim_end_matches(' ');
    if key_text.is_empty() || key_text.contains(['[', ']', '{', '}', ',', '#']) || key_text.starts_with("- ") {
        return Err(Unsupported);
    }
    let key = plain(key_text)?;
    Ok(Some((key, t[k + 1..].trim_start_matches(' ').to_string())))
}

impl Parser {
    fn block(&mut self, ind: usize) -> R<Node> {
        let l = self.lines[self.i].clone();
        if l.indent != ind {
            return Err(Unsupported);
        }
        if is_seq_line(&l.text) {
            return self.sequence(ind);
        }
        if split_key(&l.text)?.is_some() {
            return self.mapping(ind);
        }
        // A scalar node on its own lines.
        self.i += 1;
        self.scalar_with_continuation(&l.text, ind as isize - 1)
    }

    fn mapping(&mut self, ind: usize) -> R<Node> {
        let mut m = Node::of(Kind::Map);
        while self.i < self.lines.len() {
            let l = self.lines[self.i].clone();
            if l.indent < ind {
                break;
            }
            if l.indent > ind {
                return Err(Unsupported);
            }
            let (key, rest) = match split_key(&l.text)? {
                Some(x) => x,
                None => return Err(Unsupported),
            };
            self.i += 1;
            let v = if rest.is_empty() || rest.starts_with('#') {
                self.nested(ind, true)?
            } else {
                self.scalar_with_continuation(&rest, ind as isize)?
            };
            if key.kind != Kind::Str {
                if key.kind == Kind::Seq || key.kind == Kind::Map {
                    return Err(Unsupported);
                }
                m.non_str_keys += 1;
                continue;
            }
            if !m.map.contains_key(&key.text) {
                m.keys.push(key.text.clone());
            }
            m.map.insert(key.text, v);
        }
        Ok(m)
    }

    /// The value of "key:" with nothing after it.
    fn nested(&mut self, ind: usize, in_mapping: bool) -> R<Node> {
        if self.i >= self.lines.len() {
            return Ok(Node::of(Kind::Null));
        }
        let l = self.lines[self.i].clone();
        if l.indent > ind {
            return self.block(l.indent);
        }
        if in_mapping && l.indent == ind && is_seq_line(&l.text) {
            return self.sequence(ind);
        }
        Ok(Node::of(Kind::Null))
    }

    fn sequence(&mut self, ind: usize) -> R<Node> {
        let mut s = Node::of(Kind::Seq);
        while self.i < self.lines.len() {
            let l = self.lines[self.i].clone();
            if l.indent != ind || !is_seq_line(&l.text) {
                if l.indent > ind {
                    return Err(Unsupported);
                }
                break;
            }
            let rest = l.text[1..].trim_start_matches(' ').to_string();
            if rest.is_empty() || rest.starts_with('#') {
                self.i += 1;
                let v = self.nested(ind, false)?;
                s.items.push(v);
                continue;
            }
            // "- key: v" or "- - x": a block node whose first line starts
            // at the column after "- ".
            let col = ind + (l.text.len() - rest.len());
            if split_key(&rest)?.is_some() || is_seq_line(&rest) {
                self.lines[self.i] = Line { indent: col, text: rest };
                let v = self.block(col)?;
                s.items.push(v);
                continue;
            }
            self.i += 1;
            let v = self.scalar_with_continuation(&rest, ind as isize)?;
            s.items.push(v);
        }
        Ok(s)
    }

    /// A value written after "key: " or "- ".
    fn scalar_with_continuation(&mut self, t: &str, parent: isize) -> R<Node> {
        match t.as_bytes()[0] {
            b'[' | b'{' => {
                let (v, rest) = flow_node(t)?;
                if !only_comment(rest) {
                    return Err(Unsupported);
                }
                return Ok(v);
            }
            b'\'' | b'"' => {
                let end = quoted_end(t)?;
                if !only_comment(&t[end..]) {
                    return Err(Unsupported);
                }
                return quoted(&t[..end]);
            }
            b'&' | b'*' | b'!' | b'|' | b'>' | b'%' | b'@' | b'`' | b'?' | b',' | b']' | b'}' | b'#' => {
                return Err(Unsupported)
            }
            _ => {}
        }
        if let Some(c) = t.find(" #") {
            // A comment ends the scalar; a continuation after it is not read.
            let text = t[..c].trim_end_matches(' ');
            if self.i < self.lines.len() && self.lines[self.i].indent as isize > parent {
                return Err(Unsupported);
            }
            return plain_value(text);
        }
        let mut parts = vec![t.to_string()];
        while self.i < self.lines.len() && self.lines[self.i].indent as isize > parent {
            let c = self.lines[self.i].text.clone();
            if c.contains(": ") || c.ends_with(':') || c.contains(" #") || is_seq_line(&c) {
                return Err(Unsupported);
            }
            parts.push(c);
            self.i += 1;
        }
        let joined = parts.join(" ");
        if parts.len() > 1 {
            // A multi-line plain scalar is always a string.
            plain_ok(&joined)?;
            return Ok(Node::text(Kind::Str, &joined));
        }
        plain_value(&joined)
    }
}

fn plain_value(t: &str) -> R<Node> {
    if t.contains(": ") || t.ends_with(':') {
        return Err(Unsupported); // "mapping values are not allowed here"
    }
    plain(t)
}

fn plain_ok(t: &str) -> R<()> {
    let b = t.as_bytes();
    if b.is_empty() {
        return Err(Unsupported);
    }
    if b"-?:,[]{}#&*!|>'\"%@`".contains(&b[0]) && !(b[0] == b'-' && b.len() > 1 && b[1] != b' ') {
        return Err(Unsupported);
    }
    Ok(())
}

fn re(which: usize) -> &'static Regex {
    static RES: OnceLock<Vec<Regex>> = OnceLock::new();
    &RES.get_or_init(|| {
        [
            r"^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$",
            r"^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$",
            r"^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$",
            r"^[-+]?(?:0|[1-9][0-9]*)$",
            r"^[-+]?[0-9]+\.[0-9]*(?:[eE][-+][0-9]+)?$",
        ]
        .iter()
        .map(|p| Regex::new(p).expect("a fixed pattern"))
        .collect()
    })[which]
}

/// A plain scalar resolved with PyYAML's implicit resolvers. Forms it
/// resolves to types not carried here (octal, hex, sexagesimal,
/// timestamps, merge keys) are refused.
fn plain(t: &str) -> R<Node> {
    plain_ok(t)?;
    match t {
        "~" | "null" | "Null" | "NULL" => return Ok(Node::of(Kind::Null)),
        "yes" | "Yes" | "YES" | "true" | "True" | "TRUE" | "on" | "On" | "ON" => return Ok(Node::boolean(true)),
        "no" | "No" | "NO" | "false" | "False" | "FALSE" | "off" | "Off" | "OFF" => return Ok(Node::boolean(false)),
        "<<" | "=" => return Err(Unsupported),
        _ => {}
    }
    if re(0).is_match(t) {
        if re(3).is_match(t) {
            return Ok(Node::text(Kind::Int, t.strip_prefix('+').unwrap_or(t)));
        }
        return Err(Unsupported);
    }
    if re(1).is_match(t) {
        if re(4).is_match(t) {
            return Ok(Node::text(Kind::Float, t));
        }
        // .nan and .inf, written as Rust's float parser reads them.
        match t.to_lowercase().trim_start_matches(['+', '-']) {
            ".nan" => return Ok(Node::text(Kind::Float, "NaN")),
            ".inf" if t.starts_with('-') => return Ok(Node::text(Kind::Float, "-inf")),
            ".inf" => return Ok(Node::text(Kind::Float, "inf")),
            _ => {}
        }
        return Err(Unsupported);
    }
    if re(2).is_match(t) {
        return Err(Unsupported);
    }
    Ok(Node::text(Kind::Str, t))
}

/// The index after the closing quote of the quoted scalar `t` starts with.
fn quoted_end(t: &str) -> R<usize> {
    let b = t.as_bytes();
    let q = b[0];
    let mut i = 1;
    while i < b.len() {
        if q == b'\'' && b[i] == b'\'' {
            if i + 1 < b.len() && b[i + 1] == b'\'' {
                i += 2;
                continue;
            }
            return Ok(i + 1);
        } else if q == b'"' && b[i] == b'\\' {
            i += 1;
        } else if q == b'"' && b[i] == b'"' {
            return Ok(i + 1);
        }
        i += 1;
    }
    // A quoted scalar over several lines is outside the subset.
    Err(Unsupported)
}

/// A whole one-line quoted scalar, decoded.
fn quoted(t: &str) -> R<Node> {
    let body = &t[1..t.len() - 1];
    if t.starts_with('\'') {
        return Ok(Node::text(Kind::Str, &body.replace("''", "'")));
    }
    let b = body.as_bytes();
    let mut out: Vec<u8> = vec![];
    let mut i = 0;
    while i < b.len() {
        let c = b[i];
        if c != b'\\' {
            out.push(c);
            i += 1;
            continue;
        }
        i += 1;
        if i >= b.len() {
            return Err(Unsupported);
        }
        match b[i] {
            b'0' => out.push(0),
            b'a' => out.push(7),
            b'b' => out.push(8),
            b't' => out.push(b'\t'),
            b'n' => out.push(b'\n'),
            b'v' => out.push(11),
            b'f' => out.push(12),
            b'r' => out.push(b'\r'),
            b'e' => out.push(27),
            b' ' | b'"' | b'\\' | b'/' => out.push(b[i]),
            e @ (b'x' | b'u' | b'U') => {
                let n = match e {
                    b'x' => 2,
                    b'u' => 4,
                    _ => 8,
                };
                if i + 1 + n > b.len() {
                    return Err(Unsupported);
                }
                let hex = std::str::from_utf8(&b[i + 1..i + 1 + n]).map_err(|_| Unsupported)?;
                if !hex.bytes().all(|x| x.is_ascii_hexdigit()) {
                    return Err(Unsupported);
                }
                let code = u32::from_str_radix(hex, 16).map_err(|_| Unsupported)?;
                let ch = char::from_u32(code).ok_or(Unsupported)?;
                let mut buf = [0u8; 4];
                out.extend_from_slice(ch.encode_utf8(&mut buf).as_bytes());
                i += n;
            }
            _ => return Err(Unsupported),
        }
        i += 1;
    }
    Ok(Node::text(Kind::Str, &String::from_utf8(out).map_err(|_| Unsupported)?))
}

/// One flow collection or scalar from the start of `t`, and what follows.
fn flow_node(t: &str) -> R<(Node, &str)> {
    let t = t.trim_start_matches(' ');
    if t.is_empty() {
        return Err(Unsupported);
    }
    match t.as_bytes()[0] {
        b'[' => {
            let mut s = Node::of(Kind::Seq);
            let mut rest = t[1..].trim_start_matches(' ');
            loop {
                if let Some(r) = rest.strip_prefix(']') {
                    return Ok((s, r));
                }
                let (v, r) = flow_node(rest)?;
                if r.trim_start_matches(' ').starts_with(": ") {
                    return Err(Unsupported); // a single-pair mapping
                }
                s.items.push(v);
                rest = r.trim_start_matches(' ');
                if let Some(r) = rest.strip_prefix(',') {
                    rest = r.trim_start_matches(' ');
                    continue;
                }
                if !rest.starts_with(']') {
                    return Err(Unsupported);
                }
            }
        }
        b'{' => {
            let mut m = Node::of(Kind::Map);
            let mut rest = t[1..].trim_start_matches(' ');
            loop {
                if let Some(r) = rest.strip_prefix('}') {
                    return Ok((m, r));
                }
                let (k, r) = flow_node(rest)?;
                let r = r.trim_start_matches(' ');
                let r = match r.strip_prefix(':') {
                    Some(r) => r,
                    None => return Err(Unsupported),
                };
                let (v, r2) = flow_node(r)?;
                match k.kind {
                    Kind::Str => {
                        if !m.map.contains_key(&k.text) {
                            m.keys.push(k.text.clone());
                        }
                        m.map.insert(k.text, v);
                    }
                    Kind::Seq | Kind::Map => return Err(Unsupported),
                    _ => m.non_str_keys += 1,
                }
                rest = r2.trim_start_matches(' ');
                if let Some(r) = rest.strip_prefix(',') {
                    rest = r.trim_start_matches(' ');
                    continue;
                }
                if !rest.starts_with('}') {
                    return Err(Unsupported);
                }
            }
        }
        b'\'' | b'"' => {
            let end = quoted_end(t)?;
            let v = quoted(&t[..end])?;
            Ok((v, &t[end..]))
        }
        _ => {
            // A plain scalar in flow context ends at , [ ] { } or ": ".
            let b = t.as_bytes();
            let mut end = b.len();
            for i in 0..b.len() {
                let c = b[i];
                if b",[]{}".contains(&c)
                    || (c == b':' && (i + 1 == b.len() || b[i + 1] == b' ' || b",[]{}".contains(&b[i + 1])))
                    || (c == b'#' && i > 0 && b[i - 1] == b' ')
                {
                    end = i;
                    break;
                }
            }
            let text = t[..end].trim_end_matches(' ');
            if text.is_empty() {
                return Err(Unsupported);
            }
            let v = plain(text)?;
            Ok((v, &t[end..]))
        }
    }
}

fn only_comment(s: &str) -> bool {
    let t = s.trim_start_matches(' ');
    t.is_empty() || (t.starts_with('#') && t.len() < s.len())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// PyYAML reads .nan and .inf as floats; their text is one Rust's float
    /// parser reads, so register can refuse an envelope holding one.
    #[test]
    fn nan_and_inf_are_floats() {
        for (t, want) in [(".nan", f64::NAN), (".NaN", f64::NAN), (".inf", f64::INFINITY), ("+.inf", f64::INFINITY), ("-.INF", f64::NEG_INFINITY), (".Inf", f64::INFINITY)] {
            let n = plain(t).unwrap_or_else(|_| panic!("{t} refused"));
            assert!(matches!(n.kind, Kind::Float), "{t}");
            let f: f64 = n.text.parse().unwrap();
            assert!(if want.is_nan() { f.is_nan() } else { f == want }, "{t}: {f}");
        }
        for t in ["nan", "inf", "NaN", "+.nan", ".nAn"] {
            assert!(matches!(plain(t).map(|n| n.kind), Ok(Kind::Str)), "{t}");
        }
    }
}
