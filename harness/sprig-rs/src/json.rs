//! JSON as Go's encoding/json reads and writes it.
//!
//! Reading has three steps, as json.Unmarshal has: the whole input is
//! checked first (Go's scanner, with its error words), then parsed into
//! [`Node`]s, then decoded into the caller's types through [`Dec`], which
//! follows Go's rules: a key matches a field exactly or case-folded, the
//! last duplicate key wins, a decode into a map or a struct keeps what it
//! already holds, a slice reuses its elements, `null` leaves a string or a
//! number as it is, and a type mismatch is saved (the first one only) while
//! the decode goes on. Strings are unquoted as Go does: each byte that is
//! not UTF-8 and each lone surrogate becomes U+FFFD.
//!
//! Writing follows json.Marshal: map keys sorted, `<`, `>`, `&`, U+2028 and
//! U+2029 escaped, each byte that is not UTF-8 written as the six bytes
//! `\ufffd`, and Go's float format.

use std::collections::BTreeMap;

/// A parsed value and the span of its text in the input.
#[derive(Debug, Clone, PartialEq)]
pub struct Node {
    pub v: V,
    pub start: usize,
    pub end: usize,
}

#[derive(Debug, Clone, PartialEq)]
pub enum V {
    Null,
    Bool(bool),
    /// The number's text as written.
    Num(String),
    Str(String),
    Arr(Vec<Node>),
    /// Keys in input order, duplicates kept.
    Obj(Vec<(String, Node)>),
}

/// A value decoded into Go's `any`.
#[derive(Debug, Clone, PartialEq)]
pub enum Any {
    Null,
    Bool(bool),
    Num(f64),
    Str(String),
    Arr(Vec<Any>),
    Map(Map),
}

/// Go's map[string]any. A nil map is `None` where a field holds one.
pub type Map = BTreeMap<String, Any>;

/// Go's scanner over the whole input: Ok, or the SyntaxError text.
pub fn check_valid(data: &[u8]) -> Result<(), String> {
    let mut sc = Scanner::default();
    for &c in data {
        sc.step(c)?;
    }
    sc.eof()
}

/// Checks data and parses it.
pub fn parse(data: &[u8]) -> Result<Node, String> {
    check_valid(data)?;
    let mut p = Parser { b: data, i: 0 };
    Ok(p.value())
}

/// A Go slice being decoded into: `buf` is the backing array (its length is
/// the capacity, so elements past `len` are the ones a reuse finds again).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct GoSlice<T> {
    pub buf: Vec<T>,
    pub len: usize,
}

impl<T> GoSlice<T> {
    pub fn items(&self) -> &[T] {
        &self.buf[..self.len]
    }
}

/// The decode state: the first type error and where it happened.
#[derive(Default)]
pub struct Dec {
    pub err: Option<String>,
    strukt: Option<String>,
    fields: Vec<String>,
}

impl Dec {
    /// Saves a type error, unless one is saved already.
    pub fn type_err(&mut self, value: &str, ty: &str) {
        if self.err.is_some() {
            return;
        }
        self.err = Some(match &self.strukt {
            Some(st) => format!(
                "json: cannot unmarshal {value} into Go struct field {st}.{} of type {ty}",
                self.fields.join(".")
            ),
            None if !self.fields.is_empty() => format!(
                "json: cannot unmarshal {value} into Go struct field .{} of type {ty}",
                self.fields.join(".")
            ),
            None => format!("json: cannot unmarshal {value} into Go value of type {ty}"),
        });
    }

    /// Decodes an object into a struct whose JSON field names are `names`;
    /// `each` gets the index of the matched field and its value, inside
    /// that field's error context. `strukt` is the struct's Go name ("" for
    /// an anonymous one) and `ty` its Go type text.
    pub fn object(
        &mut self,
        n: &Node,
        strukt: &str,
        ty: &str,
        names: &[&str],
        mut each: impl FnMut(&mut Dec, usize, &Node),
    ) {
        match &n.v {
            V::Null => {}
            V::Obj(kv) => {
                for (k, v) in kv {
                    let Some(i) = field_index(k, names) else {
                        continue;
                    };
                    let saved = self.strukt.replace(strukt.to_string());
                    self.fields.push(names[i].to_string());
                    each(self, i, v);
                    self.fields.pop();
                    self.strukt = saved;
                }
            }
            other => self.type_err(kind(other), ty),
        }
    }

    pub fn string(&mut self, n: &Node, out: &mut String) {
        match &n.v {
            V::Null => {}
            V::Str(x) => *out = x.clone(),
            other => self.type_err(kind(other), "string"),
        }
    }

    pub fn int(&mut self, n: &Node, out: &mut i64) {
        match &n.v {
            V::Null => {}
            V::Num(t) => match parse_int(t) {
                Some(x) => *out = x,
                None => self.type_err(&format!("number {t}"), "int"),
            },
            other => self.type_err(kind(other), "int"),
        }
    }

    pub fn boolean(&mut self, n: &Node, out: &mut bool) {
        match &n.v {
            V::Null => {}
            V::Bool(x) => *out = *x,
            other => self.type_err(kind(other), "bool"),
        }
    }

    /// A map[string]any field.
    pub fn map_any(&mut self, n: &Node, out: &mut Option<Map>) {
        match &n.v {
            V::Null => *out = None,
            V::Obj(kv) => {
                let m = out.get_or_insert_with(Map::new);
                for (k, v) in kv {
                    let a = self.any(v);
                    m.insert(k.clone(), a);
                }
            }
            other => self.type_err(kind(other), "map[string]interface {}"),
        }
    }

    /// A value decoded into `any`.
    pub fn any(&mut self, n: &Node) -> Any {
        match &n.v {
            V::Null => Any::Null,
            V::Bool(b) => Any::Bool(*b),
            V::Num(t) => match t.parse::<f64>() {
                Ok(f) if f.is_finite() => Any::Num(f),
                _ => {
                    self.type_err(&format!("number {t}"), "float64");
                    Any::Null
                }
            },
            V::Str(x) => Any::Str(x.clone()),
            V::Arr(xs) => Any::Arr(xs.iter().map(|x| self.any(x)).collect()),
            V::Obj(kv) => {
                let mut m = Map::new();
                for (k, v) in kv {
                    let a = self.any(v);
                    m.insert(k.clone(), a);
                }
                Any::Map(m)
            }
        }
    }

    /// A slice field; `ty` is its Go type text, `elem` decodes one element
    /// into the place it is given.
    pub fn slice<T: Default>(
        &mut self,
        n: &Node,
        out: &mut GoSlice<T>,
        ty: &str,
        mut elem: impl FnMut(&mut Dec, &Node, &mut T),
    ) {
        match &n.v {
            V::Null => {
                out.buf.clear();
                out.len = 0;
            }
            V::Arr(xs) => {
                let mut i = 0;
                for x in xs {
                    if i >= out.buf.len() {
                        out.buf.truncate(out.len);
                        out.buf.push(T::default());
                    }
                    if i >= out.len {
                        out.len = i + 1;
                    }
                    elem(self, x, &mut out.buf[i]);
                    i += 1;
                }
                if i < out.len {
                    out.len = i;
                }
                if i == 0 {
                    out.buf.clear();
                    out.len = 0;
                }
            }
            other => self.type_err(kind(other), ty),
        }
    }
}

/// Go's json.Unmarshal: the syntax error, else runs `f` on the parsed value
/// and returns the first type error it saved.
pub fn unmarshal(data: &[u8], f: impl FnOnce(&mut Dec, &Node)) -> Result<(), String> {
    let n = parse(data)?;
    let mut d = Dec::default();
    f(&mut d, &n);
    match d.err {
        Some(e) => Err(e),
        None => Ok(()),
    }
}

/// A value to write.
#[derive(Debug, Clone, PartialEq)]
pub enum J {
    Null,
    Bool(bool),
    Int(i64),
    Float(f64),
    Str(Vec<u8>),
    /// Raw JSON text, as a json.RawMessage is written: compacted.
    Raw(Vec<u8>),
    Arr(Vec<J>),
    /// Keys in the order given: a Go struct's field order.
    Obj(Vec<(String, J)>),
}

impl J {
    pub fn s(v: impl AsRef<[u8]>) -> J {
        J::Str(v.as_ref().to_vec())
    }
}

/// A Go map: the keys sorted.
pub fn jmap(pairs: Vec<(&str, J)>) -> J {
    let mut v: Vec<(String, J)> = pairs.into_iter().map(|(k, v)| (k.to_string(), v)).collect();
    v.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    J::Obj(v)
}

impl From<&Any> for J {
    fn from(a: &Any) -> J {
        match a {
            Any::Null => J::Null,
            Any::Bool(b) => J::Bool(*b),
            Any::Num(f) => J::Float(*f),
            Any::Str(s) => J::s(s),
            Any::Arr(xs) => J::Arr(xs.iter().map(J::from).collect()),
            Any::Map(m) => J::Obj(m.iter().map(|(k, v)| (k.clone(), J::from(v))).collect()),
        }
    }
}

/// A map[string]any field: nil is `null`.
pub fn jmap_any(m: &Option<Map>) -> J {
    match m {
        None => J::Null,
        Some(m) => J::from(&Any::Map(m.clone())),
    }
}

/// Go's float64 text in JSON.
pub fn float_text(f: f64) -> String {
    let abs = f.abs();
    if abs != 0.0 && !(1e-6..1e21).contains(&abs) {
        let s = format!("{f:e}");
        match s.find('e') {
            Some(i) if !s[i + 1..].starts_with('-') => format!("{}e+{}", &s[..i], &s[i + 1..]),
            _ => s,
        }
    } else {
        format!("{f}")
    }
}

/// Appends s as a Go JSON string.
pub fn push_str(out: &mut Vec<u8>, s: &[u8]) {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    out.push(b'"');
    let mut i = 0;
    while i < s.len() {
        let c = s[i];
        if c < 0x80 {
            match c {
                b'\\' => out.extend_from_slice(b"\\\\"),
                b'"' => out.extend_from_slice(b"\\\""),
                b'\n' => out.extend_from_slice(b"\\n"),
                b'\r' => out.extend_from_slice(b"\\r"),
                b'\t' => out.extend_from_slice(b"\\t"),
                0x08 => out.extend_from_slice(b"\\b"),
                0x0c => out.extend_from_slice(b"\\f"),
                c if c < 0x20 || c == b'<' || c == b'>' || c == b'&' => {
                    out.extend_from_slice(b"\\u00");
                    out.push(HEX[(c >> 4) as usize]);
                    out.push(HEX[(c & 0xf) as usize]);
                }
                c => out.push(c),
            }
            i += 1;
            continue;
        }
        let (r, n) = crate::gostr::decode_rune(&s[i..]);
        if n == 1 && r == '\u{fffd}' {
            out.extend_from_slice(b"\\ufffd");
        } else if r == '\u{2028}' {
            out.extend_from_slice(b"\\u2028");
        } else if r == '\u{2029}' {
            out.extend_from_slice(b"\\u2029");
        } else {
            out.extend_from_slice(&s[i..i + n]);
        }
        i += n;
    }
    out.push(b'"');
}

/// Go's json.Marshal of v.
pub fn encode(v: &J) -> Vec<u8> {
    let mut out = Vec::new();
    push(&mut out, v);
    out
}

// --- the scanner: Go's encoding/json/scanner.go, state for state ---------

#[derive(Clone, Copy, PartialEq)]
enum St {
    BeginValue,
    BeginValueOrEmpty,
    BeginStringOrEmpty,
    BeginString,
    EndValue,
    EndTop,
    InString,
    InStringEsc,
    InStringEscU(u8),
    Neg,
    One,
    Zero,
    Dot,
    Dot0,
    E,
    ESign,
    E0,
    Lit(&'static [u8], usize, &'static str),
}

#[derive(Clone, Copy, PartialEq)]
enum Parse {
    ObjectKey,
    ObjectValue,
    ArrayValue,
}

struct Scanner {
    st: St,
    stack: Vec<Parse>,
    end_top: bool,
}

impl Default for Scanner {
    fn default() -> Self {
        Scanner {
            st: St::BeginValue,
            stack: Vec::new(),
            end_top: false,
        }
    }
}

const MAX_DEPTH: usize = 10000;

fn is_space(c: u8) -> bool {
    matches!(c, b' ' | b'\t' | b'\r' | b'\n')
}

fn bad(c: u8, ctx: &str) -> String {
    format!("invalid character {} {ctx}", crate::goerr::quote_char(c))
}

impl Scanner {
    fn push(&mut self, p: Parse, c: u8) -> Result<(), String> {
        self.stack.push(p);
        if self.stack.len() > MAX_DEPTH {
            return Err(bad(c, "exceeded max depth"));
        }
        Ok(())
    }

    fn pop(&mut self) {
        self.stack.pop();
        if self.stack.is_empty() {
            self.end_top = true;
            self.st = St::EndTop;
        } else {
            self.st = St::EndValue;
        }
    }

    fn eof(&mut self) -> Result<(), String> {
        if self.end_top {
            return Ok(());
        }
        self.step(b' ')?;
        if self.end_top {
            return Ok(());
        }
        Err("unexpected end of JSON input".into())
    }

    fn begin_value(&mut self, c: u8) -> Result<(), String> {
        if is_space(c) {
            return Ok(());
        }
        self.st = match c {
            b'{' => {
                self.push(Parse::ObjectKey, c)?;
                St::BeginStringOrEmpty
            }
            b'[' => {
                self.push(Parse::ArrayValue, c)?;
                St::BeginValueOrEmpty
            }
            b'"' => St::InString,
            b'-' => St::Neg,
            b'0' => St::Zero,
            b'1'..=b'9' => St::One,
            b't' => St::Lit(b"rue", 0, "true"),
            b'f' => St::Lit(b"alse", 0, "false"),
            b'n' => St::Lit(b"ull", 0, "null"),
            _ => return Err(bad(c, "looking for beginning of value")),
        };
        Ok(())
    }

    fn end_value(&mut self, c: u8) -> Result<(), String> {
        let Some(&top) = self.stack.last() else {
            self.st = St::EndTop;
            self.end_top = true;
            return self.end_top_step(c);
        };
        if is_space(c) {
            self.st = St::EndValue;
            return Ok(());
        }
        match top {
            Parse::ObjectKey => {
                if c == b':' {
                    *self.stack.last_mut().unwrap() = Parse::ObjectValue;
                    self.st = St::BeginValue;
                    return Ok(());
                }
                Err(bad(c, "after object key"))
            }
            Parse::ObjectValue => match c {
                b',' => {
                    *self.stack.last_mut().unwrap() = Parse::ObjectKey;
                    self.st = St::BeginString;
                    Ok(())
                }
                b'}' => {
                    self.pop();
                    Ok(())
                }
                _ => Err(bad(c, "after object key:value pair")),
            },
            Parse::ArrayValue => match c {
                b',' => {
                    self.st = St::BeginValue;
                    Ok(())
                }
                b']' => {
                    self.pop();
                    Ok(())
                }
                _ => Err(bad(c, "after array element")),
            },
        }
    }

    fn end_top_step(&mut self, c: u8) -> Result<(), String> {
        if !is_space(c) {
            return Err(bad(c, "after top-level value"));
        }
        Ok(())
    }

    fn step(&mut self, c: u8) -> Result<(), String> {
        match self.st {
            St::BeginValue => self.begin_value(c),
            St::BeginValueOrEmpty => {
                if is_space(c) {
                    return Ok(());
                }
                if c == b']' {
                    self.pop();
                    return Ok(());
                }
                self.begin_value(c)
            }
            St::BeginStringOrEmpty => {
                if is_space(c) {
                    return Ok(());
                }
                if c == b'}' {
                    *self.stack.last_mut().unwrap() = Parse::ObjectValue;
                    self.pop();
                    return Ok(());
                }
                self.begin_string(c)
            }
            St::BeginString => self.begin_string(c),
            St::EndValue => self.end_value(c),
            St::EndTop => self.end_top_step(c),
            St::InString => {
                match c {
                    b'"' => self.st = St::EndValue,
                    b'\\' => self.st = St::InStringEsc,
                    c if c < 0x20 => return Err(bad(c, "in string literal")),
                    _ => {}
                }
                Ok(())
            }
            St::InStringEsc => {
                self.st = match c {
                    b'b' | b'f' | b'n' | b'r' | b't' | b'\\' | b'/' | b'"' => St::InString,
                    b'u' => St::InStringEscU(0),
                    _ => return Err(bad(c, "in string escape code")),
                };
                Ok(())
            }
            St::InStringEscU(k) => {
                if !c.is_ascii_hexdigit() {
                    return Err(bad(c, "in \\u hexadecimal character escape"));
                }
                self.st = if k == 3 {
                    St::InString
                } else {
                    St::InStringEscU(k + 1)
                };
                Ok(())
            }
            St::Neg => {
                self.st = match c {
                    b'0' => St::Zero,
                    b'1'..=b'9' => St::One,
                    _ => return Err(bad(c, "in numeric literal")),
                };
                Ok(())
            }
            St::One => {
                if c.is_ascii_digit() {
                    return Ok(());
                }
                self.zero(c)
            }
            St::Zero => self.zero(c),
            St::Dot => {
                if c.is_ascii_digit() {
                    self.st = St::Dot0;
                    return Ok(());
                }
                Err(bad(c, "after decimal point in numeric literal"))
            }
            St::Dot0 => {
                if c.is_ascii_digit() {
                    return Ok(());
                }
                if c == b'e' || c == b'E' {
                    self.st = St::E;
                    return Ok(());
                }
                self.end_value(c)
            }
            St::E => {
                if c == b'+' || c == b'-' {
                    self.st = St::ESign;
                    return Ok(());
                }
                self.e_sign(c)
            }
            St::ESign => self.e_sign(c),
            St::E0 => {
                if c.is_ascii_digit() {
                    return Ok(());
                }
                self.end_value(c)
            }
            St::Lit(rest, k, word) => {
                if c != rest[k] {
                    let ctx = format!("in literal {word} (expecting '{}')", rest[k] as char);
                    return Err(bad(c, &ctx));
                }
                self.st = if k + 1 == rest.len() {
                    St::EndValue
                } else {
                    St::Lit(rest, k + 1, word)
                };
                Ok(())
            }
        }
    }

    fn begin_string(&mut self, c: u8) -> Result<(), String> {
        if is_space(c) {
            return Ok(());
        }
        if c == b'"' {
            self.st = St::InString;
            return Ok(());
        }
        Err(bad(c, "looking for beginning of object key string"))
    }

    fn zero(&mut self, c: u8) -> Result<(), String> {
        match c {
            b'.' => {
                self.st = St::Dot;
                Ok(())
            }
            b'e' | b'E' => {
                self.st = St::E;
                Ok(())
            }
            _ => self.end_value(c),
        }
    }

    fn e_sign(&mut self, c: u8) -> Result<(), String> {
        if c.is_ascii_digit() {
            self.st = St::E0;
            return Ok(());
        }
        Err(bad(c, "in exponent of numeric literal"))
    }
}

// --- the parser, over input the scanner passed ---------------------------

struct Parser<'a> {
    b: &'a [u8],
    i: usize,
}

impl Parser<'_> {
    fn ws(&mut self) {
        while self.i < self.b.len() && is_space(self.b[self.i]) {
            self.i += 1;
        }
    }

    fn value(&mut self) -> Node {
        self.ws();
        let start = self.i;
        let v = match self.b[self.i] {
            b'{' => {
                self.i += 1;
                let mut kv = Vec::new();
                loop {
                    self.ws();
                    if self.b[self.i] == b'}' {
                        self.i += 1;
                        break;
                    }
                    let k = self.string();
                    self.ws();
                    self.i += 1; // the colon
                    let v = self.value();
                    kv.push((k, v));
                    self.ws();
                    if self.b[self.i] == b',' {
                        self.i += 1;
                    }
                }
                V::Obj(kv)
            }
            b'[' => {
                self.i += 1;
                let mut xs = Vec::new();
                loop {
                    self.ws();
                    if self.b[self.i] == b']' {
                        self.i += 1;
                        break;
                    }
                    xs.push(self.value());
                    self.ws();
                    if self.b[self.i] == b',' {
                        self.i += 1;
                    }
                }
                V::Arr(xs)
            }
            b'"' => V::Str(self.string()),
            b't' => {
                self.i += 4;
                V::Bool(true)
            }
            b'f' => {
                self.i += 5;
                V::Bool(false)
            }
            b'n' => {
                self.i += 4;
                V::Null
            }
            _ => {
                while self.i < self.b.len()
                    && matches!(
                        self.b[self.i],
                        b'-' | b'+' | b'.' | b'e' | b'E' | b'0'..=b'9'
                    )
                {
                    self.i += 1;
                }
                V::Num(String::from_utf8_lossy(&self.b[start..self.i]).into_owned())
            }
        };
        Node {
            v,
            start,
            end: self.i,
        }
    }

    fn hex4(&self, at: usize) -> Option<u32> {
        if at + 6 > self.b.len() || self.b[at] != b'\\' || self.b[at + 1] != b'u' {
            return None;
        }
        let h = std::str::from_utf8(&self.b[at + 2..at + 6]).ok()?;
        u32::from_str_radix(h, 16).ok()
    }

    /// Go's unquote: the string starting at the opening quote.
    fn string(&mut self) -> String {
        self.i += 1;
        let mut out = String::new();
        loop {
            let c = self.b[self.i];
            match c {
                b'"' => {
                    self.i += 1;
                    return out;
                }
                b'\\' => {
                    let e = self.b[self.i + 1];
                    self.i += 2;
                    match e {
                        b'b' => out.push('\u{8}'),
                        b'f' => out.push('\u{c}'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => {
                            let r = self.hex4(self.i - 2).unwrap_or(0xfffd);
                            self.i += 4;
                            if (0xd800..0xe000).contains(&r) {
                                if let Some(r2) = self.hex4(self.i) {
                                    if (0xd800..0xdc00).contains(&r)
                                        && (0xdc00..0xe000).contains(&r2)
                                    {
                                        let cp = 0x10000 + ((r - 0xd800) << 10) + (r2 - 0xdc00);
                                        out.push(char::from_u32(cp).unwrap_or('\u{fffd}'));
                                        self.i += 6;
                                        continue;
                                    }
                                }
                                out.push('\u{fffd}');
                            } else {
                                out.push(char::from_u32(r).unwrap_or('\u{fffd}'));
                            }
                        }
                        other => out.push(other as char),
                    }
                }
                _ => {
                    let (r, n) = crate::gostr::decode_rune(&self.b[self.i..]);
                    out.push(r);
                    self.i += n;
                }
            }
        }
    }
}

// --- decode helpers -------------------------------------------------------

fn kind(v: &V) -> &'static str {
    match v {
        V::Null => "null",
        V::Bool(_) => "bool",
        V::Num(_) => "number",
        V::Str(_) => "string",
        V::Arr(_) => "array",
        V::Obj(_) => "object",
    }
}

/// strconv.ParseInt(t, 10, 64) on a JSON number's text.
fn parse_int(t: &str) -> Option<i64> {
    let digits = t.strip_prefix('-').unwrap_or(t);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    t.parse::<i64>().ok()
}

/// Go's foldName for one rune: upper(lower(r)), on simple case mappings.
fn fold_char(c: char) -> char {
    if c.is_ascii() {
        return c.to_ascii_uppercase();
    }
    if c == '\u{130}' {
        return 'I';
    }
    let mut lo = c.to_lowercase();
    let l = match (lo.next(), lo.next()) {
        (Some(l), None) => l,
        _ => c,
    };
    let mut up = l.to_uppercase();
    match (up.next(), up.next()) {
        (Some(u), None) => u,
        _ => l,
    }
}

fn fold(s: &str) -> String {
    s.chars().map(fold_char).collect()
}

/// The field a key names: an exact match first, else a case-folded one.
fn field_index(key: &str, names: &[&str]) -> Option<usize> {
    if let Some(i) = names.iter().position(|n| *n == key) {
        return Some(i);
    }
    let k = fold(key);
    names.iter().position(|n| fold(n) == k)
}

// --- the encoder ----------------------------------------------------------

/// Go's compact of raw JSON, with HTML escaping, as Marshal copies a
/// json.RawMessage. Bytes inside strings are copied as they are.
fn push_raw(out: &mut Vec<u8>, raw: &[u8]) {
    let mut in_str = false;
    let mut esc = false;
    let mut i = 0;
    while i < raw.len() {
        let c = raw[i];
        if in_str {
            match c {
                b'<' | b'>' | b'&' => {
                    out.extend_from_slice(format!("\\u00{c:02x}").as_bytes());
                }
                0xe2 if raw.get(i + 1) == Some(&0x80)
                    && matches!(raw.get(i + 2), Some(0xa8 | 0xa9)) =>
                {
                    out.extend_from_slice(if raw[i + 2] == 0xa8 {
                        b"\\u2028"
                    } else {
                        b"\\u2029"
                    });
                    i += 3;
                    continue;
                }
                _ => out.push(c),
            }
            if esc {
                esc = false;
            } else if c == b'\\' {
                esc = true;
            } else if c == b'"' {
                in_str = false;
            }
        } else if !is_space(c) {
            if c == b'"' {
                in_str = true;
            }
            out.push(c);
        }
        i += 1;
    }
}

fn push(out: &mut Vec<u8>, v: &J) {
    match v {
        J::Null => out.extend_from_slice(b"null"),
        J::Bool(b) => out.extend_from_slice(if *b { b"true" } else { b"false" }),
        J::Int(i) => out.extend_from_slice(i.to_string().as_bytes()),
        J::Float(f) => out.extend_from_slice(float_text(*f).as_bytes()),
        J::Str(s) => push_str(out, s),
        J::Raw(r) => push_raw(out, r),
        J::Arr(xs) => {
            out.push(b'[');
            for (i, x) in xs.iter().enumerate() {
                if i > 0 {
                    out.push(b',');
                }
                push(out, x);
            }
            out.push(b']');
        }
        J::Obj(kv) => {
            out.push(b'{');
            for (i, (k, x)) in kv.iter().enumerate() {
                if i > 0 {
                    out.push(b',');
                }
                push_str(out, k.as_bytes());
                out.push(b':');
                push(out, x);
            }
            out.push(b'}');
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn syn(s: &[u8]) -> String {
        check_valid(s).unwrap_err()
    }

    #[test]
    fn syntax_errors_are_gos() {
        assert_eq!(syn(b""), "unexpected end of JSON input");
        assert_eq!(syn(b"   "), "unexpected end of JSON input");
        assert_eq!(syn(b"{"), "unexpected end of JSON input");
        assert_eq!(
            syn(b"x"),
            "invalid character 'x' looking for beginning of value"
        );
        assert_eq!(
            syn(b"{\"a\":1}x"),
            "invalid character 'x' after top-level value"
        );
        assert_eq!(
            syn(b"{\"a\":1 2}"),
            "invalid character '2' after object key:value pair"
        );
        assert_eq!(syn(b"{\"a\" 1}"), "invalid character '1' after object key");
        assert_eq!(
            syn(b"[1,]"),
            "invalid character ']' looking for beginning of value"
        );
        assert_eq!(syn(b"[1 2]"), "invalid character '2' after array element");
        assert_eq!(
            syn(b"{nope"),
            "invalid character 'n' looking for beginning of object key string"
        );
        assert_eq!(syn(b"01"), "invalid character '1' after top-level value");
        assert_eq!(
            syn(b"1."),
            "invalid character ' ' after decimal point in numeric literal"
        );
        assert_eq!(syn(b"-"), "invalid character ' ' in numeric literal");
        assert_eq!(
            syn(b"1e"),
            "invalid character ' ' in exponent of numeric literal"
        );
        assert_eq!(
            syn(b"\"\x01\""),
            "invalid character '\\x01' in string literal"
        );
        assert_eq!(
            syn(b"\"\\q\""),
            "invalid character 'q' in string escape code"
        );
        assert_eq!(
            syn(b"\"\\u12x4\""),
            "invalid character 'x' in \\u hexadecimal character escape"
        );
        assert_eq!(
            syn(b"tru"),
            "invalid character ' ' in literal true (expecting 'e')"
        );
        assert_eq!(
            syn(b"nul"),
            "invalid character ' ' in literal null (expecting 'l')"
        );
        assert_eq!(
            syn(b"fx"),
            "invalid character 'x' in literal false (expecting 'a')"
        );
        assert_eq!(
            syn(b"\xc3\xa9"),
            "invalid character 'Ã' looking for beginning of value"
        );
        assert!(check_valid(b" {\"a\":[1,-0.5e+3,true,false,null,\"\\u00e9\"]} ").is_ok());
        let deep = "[".repeat(10001);
        assert_eq!(
            syn(deep.as_bytes()),
            "invalid character '[' exceeded max depth"
        );
    }

    #[test]
    fn strings_unquote_as_go() {
        let n = parse(b"\"a\\ud800b\\udc00\\ud83d\\ude00 \xff\xe2\x82 c\\ud800\\u0041\"").unwrap();
        assert_eq!(
            n.v,
            V::Str("a\u{fffd}b\u{fffd}\u{1f600} \u{fffd}\u{fffd}\u{fffd} c\u{fffd}A".into())
        );
        let n = parse(b"{\"k\" : [ 1 , 2 ] }").unwrap();
        match n.v {
            V::Obj(kv) => {
                assert_eq!(kv[0].0, "k");
                assert_eq!((kv[0].1.start, kv[0].1.end), (7, 16));
            }
            _ => panic!(),
        }
    }

    #[derive(Default, Debug)]
    struct Step {
        id: String,
        needs: GoSlice<String>,
    }

    #[derive(Default, Debug)]
    struct Flow {
        name: String,
        steps: GoSlice<Step>,
    }

    fn flow(data: &[u8]) -> (Flow, Result<(), String>) {
        let mut f = Flow::default();
        let r = unmarshal(data, |d, n| {
            d.object(
                n,
                "",
                "weave.Workflow",
                &["name", "steps"],
                |d, i, v| match i {
                    0 => d.string(v, &mut f.name),
                    _ => d.slice(v, &mut f.steps, "[]weave.Step", |d, v, s| {
                        d.object(
                            v,
                            "Step",
                            "weave.Step",
                            &["id", "needs"],
                            |d, i, v| match i {
                                0 => d.string(v, &mut s.id),
                                _ => d.slice(v, &mut s.needs, "[]string", |d, v, x| d.string(v, x)),
                            },
                        )
                    }),
                },
            )
        });
        (f, r)
    }

    #[test]
    fn type_errors_are_gos() {
        let (_, r) = flow(b"[]");
        assert_eq!(
            r.unwrap_err(),
            "json: cannot unmarshal array into Go value of type weave.Workflow"
        );
        let (_, r) = flow(b"{\"name\":5}");
        assert_eq!(
            r.unwrap_err(),
            "json: cannot unmarshal number into Go struct field .name of type string"
        );
        let (_, r) = flow(b"{\"steps\":[{\"id\":5}]}");
        assert_eq!(
            r.unwrap_err(),
            "json: cannot unmarshal number into Go struct field Step.steps.id of type string"
        );
        let (_, r) = flow(b"{\"steps\":[{\"needs\":[1]}]}");
        assert_eq!(
            r.unwrap_err(),
            "json: cannot unmarshal number into Go struct field Step.steps.needs of type string"
        );
        let (_, r) = flow(b"{\"steps\":{}}");
        assert_eq!(
            r.unwrap_err(),
            "json: cannot unmarshal object into Go struct field .steps of type []weave.Step"
        );
    }

    #[test]
    fn keys_fold_and_last_wins() {
        let (f, r) = flow("{\"STEPS\":[{\"ID\":\"a\"}],\"Name\":\"N\",\"name\":\"n2\",\"\u{17f}teps\":[{\"id\":\"k\"}]}".as_bytes());
        assert!(r.is_ok());
        assert_eq!(f.name, "n2");
        assert_eq!(f.steps.items().len(), 1);
        assert_eq!(f.steps.items()[0].id, "k");
    }

    #[test]
    fn slices_reuse_elements() {
        let (f, _) = flow(b"{\"steps\":[{\"id\":\"a\",\"needs\":[\"x\"]},{\"id\":\"b\"}],\"steps\":[{\"needs\":null}]}");
        assert_eq!(f.steps.len, 1);
        assert_eq!(f.steps.items()[0].id, "a");
        assert_eq!(f.steps.items()[0].needs.len, 0);
        let (f, _) = flow(b"{\"steps\":[{\"id\":\"a\"}],\"steps\":[]}");
        assert_eq!(f.steps.len, 0);
    }

    #[test]
    fn ints_and_any() {
        let mut d = Dec::default();
        let mut x = 7i64;
        d.int(&parse(b"-0").unwrap(), &mut x);
        assert_eq!(x, 0);
        d.int(&parse(b"1e2").unwrap(), &mut x);
        assert_eq!(
            d.err.as_deref(),
            Some("json: cannot unmarshal number 1e2 into Go value of type int")
        );
        let mut d = Dec::default();
        let a = d.any(&parse(b"{\"a\":[1,{\"b\":1e400}],\"a\":2.50}").unwrap());
        assert_eq!(
            d.err.as_deref(),
            Some("json: cannot unmarshal number 1e400 into Go value of type float64")
        );
        let mut m = Map::new();
        m.insert("a".into(), Any::Num(2.5));
        assert_eq!(a, Any::Map(m));
        let mut d = Dec::default();
        let mut m: Option<Map> = None;
        d.map_any(&parse(b"{\"a\":1}").unwrap(), &mut m);
        d.map_any(&parse(b"{\"b\":2}").unwrap(), &mut m);
        assert_eq!(m.as_ref().unwrap().len(), 2);
        d.map_any(&parse(b"null").unwrap(), &mut m);
        assert_eq!(m, None);
    }

    #[test]
    fn encode_follows_go() {
        let a = Any::Arr(vec![
            Any::Num(100.0),
            Any::Num(12345678901234567890.0),
            Any::Num(-0.0),
            Any::Num(0.0000001),
            Any::Num(0.000001),
            Any::Num(1e21),
            Any::Num(1.5),
            Any::Str("<&>\u{2028}".into()),
        ]);
        assert_eq!(
            String::from_utf8(encode(&J::from(&a))).unwrap(),
            "[100,12345678901234567000,-0,1e-7,0.000001,1e+21,1.5,\"\\u003c\\u0026\\u003e\\u2028\"]"
        );
        let j = jmap(vec![
            ("b", J::s(b"x\xffy\x01\x08\x0c\n\"\\")),
            ("a", J::Raw(b" { \"a\" : [1, \"<\"] } ".to_vec())),
        ]);
        assert_eq!(
            String::from_utf8(encode(&j)).unwrap(),
            "{\"a\":{\"a\":[1,\"\\u003c\"]},\"b\":\"x\\ufffdy\\u0001\\b\\f\\n\\\"\\\\\"}"
        );
        assert_eq!(encode(&jmap_any(&None)), b"null");
        assert_eq!(
            encode(&J::Obj(vec![
                ("z".into(), J::Int(-3)),
                ("a".into(), J::Bool(true))
            ])),
            b"{\"z\":-3,\"a\":true}"
        );
    }
}
