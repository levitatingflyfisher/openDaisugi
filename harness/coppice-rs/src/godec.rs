//! JSON read the way Go's encoding/json reads it into a struct, so an
//! adapter that turns a bad line into an error event names it in Go's
//! words.
//!
//! Three parts. `check` is Go's scanner: it finds the first syntax error
//! and words it as Go does ("invalid character 'x' looking for beginning
//! of value", "unexpected end of JSON input"). `parse` builds a tree that
//! keeps each value's raw text, which a json.RawMessage field copies as
//! is. `decode` fills a struct by a field table, as Go's decoder does: a
//! key matches a field by exact name or else without regard to case, null
//! leaves a field as it was, and the first value of the wrong kind is
//! the error, worded with the struct and the field path.

/// Go's limit on nesting.
const MAX_DEPTH: usize = 10000;

/// Go's quoteChar: a byte as the scanner names it in an error.
fn quote_char(c: u8) -> String {
    match c {
        b'\'' => "'\\''".into(),
        b'"' => "'\"'".into(),
        _ => {
            // string(c) of a byte is the rune with that value.
            let s = crate::sys::go_quote(&char::from(c).to_string());
            format!("'{}'", &s[1..s.len() - 1])
        }
    }
}

#[derive(Clone, Copy, PartialEq)]
enum St {
    BeginValueOrEmpty,
    BeginValue,
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
enum Ps {
    ObjectKey,
    ObjectValue,
    ArrayValue,
}

fn is_space(c: u8) -> bool {
    matches!(c, b' ' | b'\t' | b'\r' | b'\n')
}

struct Scanner {
    step: St,
    stack: Vec<Ps>,
    end_top: bool,
}

impl Scanner {
    fn err(c: u8, ctx: &str) -> Result<(), String> {
        Err(format!("invalid character {} {ctx}", quote_char(c)))
    }

    fn push(&mut self, p: Ps) -> Result<(), String> {
        self.stack.push(p);
        if self.stack.len() > MAX_DEPTH {
            return Err("exceeded max depth".into());
        }
        Ok(())
    }

    fn begin_value(&mut self, c: u8) -> Result<(), String> {
        if is_space(c) {
            return Ok(());
        }
        self.step = match c {
            b'{' => {
                self.push(Ps::ObjectKey)?;
                St::BeginStringOrEmpty
            }
            b'[' => {
                self.push(Ps::ArrayValue)?;
                St::BeginValueOrEmpty
            }
            b'"' => St::InString,
            b'-' => St::Neg,
            b'0' => St::Zero,
            b't' => St::Lit(b"rue", 0, "true"),
            b'f' => St::Lit(b"alse", 0, "false"),
            b'n' => St::Lit(b"ull", 0, "null"),
            b'1'..=b'9' => St::One,
            _ => return Self::err(c, "looking for beginning of value"),
        };
        Ok(())
    }

    fn end_value(&mut self, c: u8) -> Result<(), String> {
        let Some(&top) = self.stack.last() else {
            self.step = St::EndTop;
            self.end_top = true;
            return self.end_top_step(c);
        };
        if is_space(c) {
            self.step = St::EndValue;
            return Ok(());
        }
        match top {
            Ps::ObjectKey => {
                if c == b':' {
                    *self.stack.last_mut().unwrap() = Ps::ObjectValue;
                    self.step = St::BeginValue;
                    return Ok(());
                }
                Self::err(c, "after object key")
            }
            Ps::ObjectValue => {
                if c == b',' {
                    *self.stack.last_mut().unwrap() = Ps::ObjectKey;
                    self.step = St::BeginString;
                    return Ok(());
                }
                if c == b'}' {
                    self.pop();
                    return Ok(());
                }
                Self::err(c, "after object key:value pair")
            }
            Ps::ArrayValue => {
                if c == b',' {
                    self.step = St::BeginValue;
                    return Ok(());
                }
                if c == b']' {
                    self.pop();
                    return Ok(());
                }
                Self::err(c, "after array element")
            }
        }
    }

    fn pop(&mut self) {
        self.stack.pop();
        if self.stack.is_empty() {
            self.step = St::EndTop;
            self.end_top = true;
        } else {
            self.step = St::EndValue;
        }
    }

    fn end_top_step(&mut self, c: u8) -> Result<(), String> {
        if !is_space(c) {
            return Self::err(c, "after top-level value");
        }
        Ok(())
    }

    fn feed(&mut self, c: u8) -> Result<(), String> {
        match self.step {
            St::BeginValueOrEmpty => {
                if is_space(c) {
                    return Ok(());
                }
                if c == b']' {
                    return self.end_value(c);
                }
                self.begin_value(c)
            }
            St::BeginValue => self.begin_value(c),
            St::BeginStringOrEmpty => {
                if is_space(c) {
                    return Ok(());
                }
                if c == b'}' {
                    *self.stack.last_mut().unwrap() = Ps::ObjectValue;
                    return self.end_value(c);
                }
                self.begin_string(c)
            }
            St::BeginString => self.begin_string(c),
            St::EndValue => self.end_value(c),
            St::EndTop => self.end_top_step(c),
            St::InString => {
                if c == b'"' {
                    self.step = St::EndValue;
                } else if c == b'\\' {
                    self.step = St::InStringEsc;
                } else if c < 0x20 {
                    return Self::err(c, "in string literal");
                }
                Ok(())
            }
            St::InStringEsc => {
                self.step = match c {
                    b'b' | b'f' | b'n' | b'r' | b't' | b'\\' | b'/' | b'"' => St::InString,
                    b'u' => St::InStringEscU(0),
                    _ => return Self::err(c, "in string escape code"),
                };
                Ok(())
            }
            St::InStringEscU(n) => {
                if !c.is_ascii_hexdigit() {
                    return Self::err(c, "in \\u hexadecimal character escape");
                }
                self.step = if n == 3 {
                    St::InString
                } else {
                    St::InStringEscU(n + 1)
                };
                Ok(())
            }
            St::Neg => {
                self.step = match c {
                    b'0' => St::Zero,
                    b'1'..=b'9' => St::One,
                    _ => return Self::err(c, "in numeric literal"),
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
                    self.step = St::Dot0;
                    return Ok(());
                }
                Self::err(c, "after decimal point in numeric literal")
            }
            St::Dot0 => {
                if c.is_ascii_digit() {
                    return Ok(());
                }
                if c == b'e' || c == b'E' {
                    self.step = St::E;
                    return Ok(());
                }
                self.end_value(c)
            }
            St::E => {
                if c == b'+' || c == b'-' {
                    self.step = St::ESign;
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
            St::Lit(rest, i, word) => {
                if c != rest[i] {
                    return Self::err(
                        c,
                        &format!("in literal {word} (expecting {})", quote_char(rest[i])),
                    );
                }
                self.step = if i + 1 == rest.len() {
                    St::EndValue
                } else {
                    St::Lit(rest, i + 1, word)
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
            self.step = St::InString;
            return Ok(());
        }
        Self::err(c, "looking for beginning of object key string")
    }

    fn zero(&mut self, c: u8) -> Result<(), String> {
        if c == b'.' {
            self.step = St::Dot;
            return Ok(());
        }
        if c == b'e' || c == b'E' {
            self.step = St::E;
            return Ok(());
        }
        self.end_value(c)
    }

    fn e_sign(&mut self, c: u8) -> Result<(), String> {
        if c.is_ascii_digit() {
            self.step = St::E0;
            return Ok(());
        }
        Self::err(c, "in exponent of numeric literal")
    }
}

/// Go's checkValid: Ok when data is one JSON value, else the scanner's
/// words for the first fault.
pub fn check(data: &[u8]) -> Result<(), String> {
    let mut s = Scanner {
        step: St::BeginValue,
        stack: Vec::new(),
        end_top: false,
    };
    for &c in data {
        s.feed(c)?;
    }
    if s.end_top {
        return Ok(());
    }
    // Go feeds one space at the end, which ends a number at the top.
    s.feed(b' ')?;
    if s.end_top {
        return Ok(());
    }
    Err("unexpected end of JSON input".into())
}

/// One parsed value with the span of its raw text.
#[derive(Clone, Debug)]
pub struct Node {
    pub v: J,
    pub start: usize,
    pub end: usize,
}

#[derive(Clone, Debug)]
pub enum J {
    Null,
    Bool(bool),
    Num,
    Str(String),
    Arr(Vec<Node>),
    Obj(Vec<(String, Node)>),
}

/// One rune of b at i as Go's utf8.DecodeRune reads it: None for a byte
/// that starts no valid sequence.
fn decode_rune(b: &[u8], i: usize) -> Option<(char, usize)> {
    let n = match b[i] {
        0x00..=0x7f => 1,
        0xc2..=0xdf => 2,
        0xe0..=0xef => 3,
        0xf0..=0xf4 => 4,
        _ => return None,
    };
    let end = i + n;
    if end > b.len() {
        return None;
    }
    std::str::from_utf8(&b[i..end])
        .ok()
        .and_then(|s| s.chars().next())
        .map(|c| (c, n))
}

fn hex4(b: &[u8]) -> Option<u32> {
    if b.len() < 6 || b[0] != b'\\' || b[1] != b'u' {
        return None;
    }
    let s = std::str::from_utf8(&b[2..6]).ok()?;
    u32::from_str_radix(s, 16).ok()
}

/// Go's unquote of the string literal in b (quotes included).
fn unquote(b: &[u8]) -> String {
    let s = &b[1..b.len() - 1];
    let mut out = String::with_capacity(s.len());
    let mut r = 0;
    while r < s.len() {
        let c = s[r];
        if c == b'\\' {
            let e = s[r + 1];
            match e {
                b'"' | b'\\' | b'/' | b'\'' => {
                    out.push(e as char);
                    r += 2;
                }
                b'b' => {
                    out.push('\u{8}');
                    r += 2;
                }
                b'f' => {
                    out.push('\u{c}');
                    r += 2;
                }
                b'n' => {
                    out.push('\n');
                    r += 2;
                }
                b'r' => {
                    out.push('\r');
                    r += 2;
                }
                b't' => {
                    out.push('\t');
                    r += 2;
                }
                _ => {
                    let rr = hex4(&s[r..]).unwrap_or(0xfffd);
                    r += 6;
                    if (0xd800..0xe000).contains(&rr) {
                        if let Some(rr1) = hex4(&s[r..]) {
                            if (0xd800..0xdc00).contains(&rr) && (0xdc00..0xe000).contains(&rr1) {
                                let v = (((rr - 0xd800) << 10) | (rr1 - 0xdc00)) + 0x10000;
                                out.push(char::from_u32(v).unwrap_or('\u{fffd}'));
                                r += 6;
                                continue;
                            }
                        }
                        out.push('\u{fffd}');
                    } else {
                        out.push(char::from_u32(rr).unwrap_or('\u{fffd}'));
                    }
                }
            }
            continue;
        }
        match decode_rune(s, r) {
            Some((ch, n)) => {
                out.push(ch);
                r += n;
            }
            None => {
                out.push('\u{fffd}');
                r += 1;
            }
        }
    }
    out
}

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
                    self.i += 1; // ':'
                    let v = self.value();
                    kv.push((k, v));
                    self.ws();
                    if self.b[self.i] == b',' {
                        self.i += 1;
                    }
                }
                J::Obj(kv)
            }
            b'[' => {
                self.i += 1;
                let mut items = Vec::new();
                loop {
                    self.ws();
                    if self.b[self.i] == b']' {
                        self.i += 1;
                        break;
                    }
                    items.push(self.value());
                    self.ws();
                    if self.b[self.i] == b',' {
                        self.i += 1;
                    }
                }
                J::Arr(items)
            }
            b'"' => J::Str(self.string()),
            b't' => {
                self.i += 4;
                J::Bool(true)
            }
            b'f' => {
                self.i += 5;
                J::Bool(false)
            }
            b'n' => {
                self.i += 4;
                J::Null
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
                J::Num
            }
        };
        Node {
            v,
            start,
            end: self.i,
        }
    }

    fn string(&mut self) -> String {
        let start = self.i;
        self.i += 1;
        while self.b[self.i] != b'"' {
            if self.b[self.i] == b'\\' {
                self.i += 1;
            }
            self.i += 1;
        }
        self.i += 1;
        unquote(&self.b[start..self.i])
    }
}

/// A checked value and its source.
pub struct Doc {
    pub src: Vec<u8>,
    pub root: Node,
}

impl Doc {
    /// The raw text of a node, as a json.RawMessage holds it.
    pub fn raw(&self, n: &Node) -> String {
        String::from_utf8_lossy(&self.src[n.start..n.end]).into_owned()
    }

    /// The text of a number node.
    pub fn num(&self, n: &Node) -> &str {
        std::str::from_utf8(&self.src[n.start..n.end]).unwrap_or("")
    }
}

/// Checks and parses data. The error is Go's syntax error text.
pub fn parse(data: &[u8]) -> Result<Doc, String> {
    check(data)?;
    let mut p = Parser { b: data, i: 0 };
    let root = p.value();
    Ok(Doc {
        src: data.to_vec(),
        root,
    })
}

/// The Go type a field decodes into.
#[derive(Clone, Copy)]
pub enum Ty {
    Str,
    PtrInt,
    PtrInt64,
    PtrBool,
    Raw,
    Struct(&'static StructTy),
    PtrStruct(&'static StructTy),
    /// A slice of the element type, with the slice's Go type text.
    Slice(&'static Ty, &'static str),
}

/// A Go struct type: its name as reflect's Name() gives it (empty for an
/// anonymous struct), its type text as String() gives it, and its fields
/// by JSON name.
pub struct StructTy {
    pub name: &'static str,
    pub text: &'static str,
    pub fields: &'static [(&'static str, Ty)],
}

/// A decoded value. A struct is the fields that were set.
#[derive(Clone, Debug, Default)]
pub enum G {
    #[default]
    Unset,
    Nil,
    Str(String),
    Int(i64),
    Bool(bool),
    Raw(String),
    Struct(Vec<(&'static str, G)>),
    Slice(Vec<G>),
}

static EMPTY: G = G::Unset;

impl G {
    /// The field named name, or Unset.
    pub fn f(&self, name: &str) -> &G {
        if let G::Struct(fs) = self {
            for (k, v) in fs.iter().rev() {
                if *k == name {
                    return v;
                }
            }
        }
        &EMPTY
    }
    /// The string, or "" for any other value.
    pub fn s(&self) -> String {
        match self {
            G::Str(s) => s.clone(),
            G::Raw(s) => s.clone(),
            _ => String::new(),
        }
    }
    pub fn int(&self) -> Option<i64> {
        match self {
            G::Int(n) => Some(*n),
            _ => None,
        }
    }
    pub fn boolean(&self) -> Option<bool> {
        match self {
            G::Bool(b) => Some(*b),
            _ => None,
        }
    }
    /// Whether the value is set and not nil: a pointer field that holds
    /// something.
    pub fn present(&self) -> bool {
        !matches!(self, G::Unset | G::Nil)
    }
    pub fn items(&self) -> &[G] {
        match self {
            G::Slice(v) => v,
            _ => &[],
        }
    }
}

fn kind_word(j: &J) -> &'static str {
    match j {
        J::Null => "null",
        J::Bool(_) => "bool",
        J::Num => "number",
        J::Str(_) => "string",
        J::Arr(_) => "array",
        J::Obj(_) => "object",
    }
}

fn ty_text(t: &Ty) -> &'static str {
    match t {
        Ty::Str => "string",
        Ty::PtrInt => "int",
        Ty::PtrInt64 => "int64",
        Ty::PtrBool => "bool",
        Ty::Raw => "json.RawMessage",
        Ty::Struct(s) | Ty::PtrStruct(s) => s.text,
        Ty::Slice(_, text) => text,
    }
}

/// Go's foldName for a key: lower case, with the two runes that fold to
/// ASCII letters taken as those letters.
fn fold(s: &str) -> String {
    s.chars()
        .map(|c| match c {
            '\u{212a}' => 'k',
            '\u{17f}' => 's',
            c => c.to_lowercase().next().unwrap_or(c),
        })
        .collect()
}

struct Dec<'a> {
    doc: &'a Doc,
    err: Option<String>,
    stack: Vec<&'static str>,
    cur: Option<&'static StructTy>,
}

impl Dec<'_> {
    fn save(&mut self, value: &str, ty: &str) {
        if self.err.is_some() {
            return;
        }
        let msg = if self.cur.is_some() || !self.stack.is_empty() {
            format!(
                "json: cannot unmarshal {value} into Go struct field {}.{} of type {ty}",
                self.cur.map(|s| s.name).unwrap_or(""),
                self.stack.join(".")
            )
        } else {
            format!("json: cannot unmarshal {value} into Go value of type {ty}")
        };
        self.err = Some(msg);
    }

    fn value(&mut self, n: &Node, t: &Ty, old: G) -> G {
        match (t, &n.v) {
            (Ty::Raw, _) => G::Raw(self.doc.raw(n)),
            (Ty::PtrInt | Ty::PtrInt64 | Ty::PtrBool | Ty::PtrStruct(_), J::Null) => G::Nil,
            (_, J::Null) => old,
            (Ty::Str, J::Str(s)) => G::Str(s.clone()),
            (Ty::PtrBool, J::Bool(b)) => G::Bool(*b),
            (Ty::PtrInt | Ty::PtrInt64, J::Num) => {
                let text = self.doc.num(n);
                match text.parse::<i64>() {
                    Ok(v) => G::Int(v),
                    Err(_) => {
                        self.save(&format!("number {text}"), ty_text(t));
                        G::Int(0)
                    }
                }
            }
            (Ty::Struct(st) | Ty::PtrStruct(st), J::Obj(kv)) => {
                let base = match old {
                    G::Struct(fs) => fs,
                    _ => Vec::new(),
                };
                G::Struct(self.object(kv, st, base))
            }
            (Ty::Slice(elem, _), J::Arr(items)) => {
                let mut out = Vec::new();
                for it in items {
                    out.push(self.value(it, elem, G::Unset));
                }
                G::Slice(out)
            }
            (_, j) => {
                self.save(kind_word(j), ty_text(t));
                match t {
                    Ty::PtrInt | Ty::PtrInt64 => G::Int(0),
                    Ty::PtrBool => G::Bool(false),
                    Ty::PtrStruct(_) => G::Struct(Vec::new()),
                    _ => old,
                }
            }
        }
    }

    fn object(
        &mut self,
        kv: &[(String, Node)],
        st: &'static StructTy,
        mut set: Vec<(&'static str, G)>,
    ) -> Vec<(&'static str, G)> {
        for (k, v) in kv {
            let field = st.fields.iter().find(|(name, _)| name == k).or_else(|| {
                let fk = fold(k);
                st.fields.iter().find(|(name, _)| fold(name) == fk)
            });
            let Some((name, ty)) = field else {
                continue;
            };
            let saved_cur = self.cur;
            let depth = self.stack.len();
            self.stack.push(name);
            self.cur = Some(st);
            let old = set
                .iter()
                .rev()
                .find(|(n, _)| n == name)
                .map(|(_, g)| g.clone())
                .unwrap_or_default();
            let g = self.value(v, ty, old);
            set.retain(|(n, _)| n != name);
            set.push((name, g));
            self.stack.truncate(depth);
            self.cur = saved_cur;
        }
        set
    }
}

/// Go's json.Unmarshal(data, &v) for a value of type t: the decoded value,
/// and the error Go returns (a syntax error, or the first type error).
pub fn decode(data: &[u8], t: &Ty) -> (G, Option<String>) {
    let doc = match parse(data) {
        Ok(d) => d,
        Err(e) => return (G::Unset, Some(e)),
    };
    decode_doc(&doc, &doc.root, t)
}

/// decode for one node of a parsed document.
pub fn decode_doc(doc: &Doc, n: &Node, t: &Ty) -> (G, Option<String>) {
    let mut d = Dec {
        doc,
        err: None,
        stack: Vec::new(),
        cur: None,
    };
    let g = d.value(n, t, G::Unset);
    (g, d.err)
}

/// A value decoded into Go's `any`: a map, a slice, a float64, a string,
/// a bool or nil.
#[derive(Clone, Debug)]
pub enum Any {
    Nil,
    Bool(bool),
    F64(f64),
    Str(String),
    Arr(Vec<Any>),
    /// Keys in first-seen order; a repeated key keeps its last value.
    Map(Vec<(String, Any)>),
}

impl Any {
    pub fn get(&self, k: &str) -> Option<&Any> {
        match self {
            Any::Map(m) => m.iter().find(|(key, _)| key == k).map(|(_, v)| v),
            _ => None,
        }
    }
    /// Go's `s, _ := v.(string)`.
    pub fn s(&self) -> String {
        match self {
            Any::Str(s) => s.clone(),
            _ => String::new(),
        }
    }
    /// v.(string) of the value under k, "" when it is absent or not a
    /// string.
    pub fn str_of(&self, k: &str) -> String {
        self.get(k).map(Any::s).unwrap_or_default()
    }

    /// Go's json.Marshal of the value: map keys sorted, floats in Go's
    /// format, strings escaped as Go escapes them.
    pub fn marshal(&self) -> String {
        let mut out = String::new();
        self.push(&mut out);
        out
    }

    fn push(&self, out: &mut String) {
        match self {
            Any::Nil => out.push_str("null"),
            Any::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            Any::F64(f) => out.push_str(&crate::gojson::float_text(*f)),
            Any::Str(s) => crate::gojson::push_str(out, s),
            Any::Arr(a) => {
                out.push('[');
                for (i, x) in a.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    x.push(out);
                }
                out.push(']');
            }
            Any::Map(m) => {
                let mut keys: Vec<&(String, Any)> = m.iter().collect();
                keys.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
                out.push('{');
                for (i, (k, v)) in keys.into_iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    crate::gojson::push_str(out, k);
                    out.push(':');
                    v.push(out);
                }
                out.push('}');
            }
        }
    }
}

fn to_any(doc: &Doc, n: &Node, err: &mut Option<String>) -> Any {
    match &n.v {
        J::Null => Any::Nil,
        J::Bool(b) => Any::Bool(*b),
        J::Num => {
            let text = doc.num(n);
            match text.parse::<f64>() {
                Ok(f) if f.is_finite() => Any::F64(f),
                _ => {
                    if err.is_none() {
                        *err = Some(format!(
                            "json: cannot unmarshal number {text} into Go value of type float64"
                        ));
                    }
                    Any::Nil
                }
            }
        }
        J::Str(s) => Any::Str(s.clone()),
        J::Arr(items) => Any::Arr(items.iter().map(|x| to_any(doc, x, err)).collect()),
        J::Obj(kv) => {
            let mut m: Vec<(String, Any)> = Vec::new();
            for (k, v) in kv {
                let val = to_any(doc, v, err);
                if let Some(slot) = m.iter_mut().find(|(key, _)| key == k) {
                    slot.1 = val;
                } else {
                    m.push((k.clone(), val));
                }
            }
            Any::Map(m)
        }
    }
}

/// Go's json.Unmarshal into a map[string]any. A JSON null is a nil map,
/// which reads as empty.
pub fn decode_map(data: &[u8]) -> Result<Any, String> {
    let doc = parse(data)?;
    match &doc.root.v {
        J::Obj(_) => {
            let mut err = None;
            let v = to_any(&doc, &doc.root, &mut err);
            match err {
                Some(e) => Err(e),
                None => Ok(v),
            }
        }
        J::Null => Ok(Any::Map(Vec::new())),
        j => Err(format!(
            "json: cannot unmarshal {} into Go value of type map[string]interface {{}}",
            kind_word(j)
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn syntax_errors_read_as_go_writes_them() {
        let cases: &[(&str, &str)] = &[
            ("", "unexpected end of JSON input"),
            (
                "not json",
                "invalid character 'o' in literal null (expecting 'u')",
            ),
            (
                "hello",
                "invalid character 'h' looking for beginning of value",
            ),
            ("{\"a\":1", "unexpected end of JSON input"),
            ("{\"a\" 1}", "invalid character '1' after object key"),
            (
                "{\"a\":1 2}",
                "invalid character '2' after object key:value pair",
            ),
            ("[1 2]", "invalid character '2' after array element"),
            (
                "{1:2}",
                "invalid character '1' looking for beginning of object key string",
            ),
            ("{} x", "invalid character 'x' after top-level value"),
            ("\"a\u{1}\"", "invalid character '\\x01' in string literal"),
            ("\"\\q\"", "invalid character 'q' in string escape code"),
            (
                "\"\\u12x4\"",
                "invalid character 'x' in \\u hexadecimal character escape",
            ),
            ("-x", "invalid character 'x' in numeric literal"),
            (
                "1.x",
                "invalid character 'x' after decimal point in numeric literal",
            ),
            (
                "1ex",
                "invalid character 'x' in exponent of numeric literal",
            ),
            (
                "tru",
                "invalid character ' ' in literal true (expecting 'e')",
            ),
            ("-", "invalid character ' ' in numeric literal"),
            ("\"ab", "unexpected end of JSON input"),
            (
                "'a'",
                "invalid character '\\'' looking for beginning of value",
            ),
            ("12", ""),
            ("  {\"a\": [1, 2.5e3, true, null, \"x\"]}  ", ""),
        ];
        for (input, want) in cases {
            let got = check(input.as_bytes()).err().unwrap_or_default();
            assert_eq!(&got, want, "{input:?}");
        }
    }

    static INNER: StructTy = StructTy {
        name: "",
        text: "struct { N *int \"json:\\\"n\\\"\" }",
        fields: &[("n", Ty::PtrInt)],
    };
    static OUTER: StructTy = StructTy {
        name: "row",
        text: "pkg.row",
        fields: &[
            ("type", Ty::Str),
            ("ok", Ty::PtrBool),
            ("inner", Ty::Struct(&INNER)),
            ("raw", Ty::Raw),
        ],
    };

    #[test]
    fn decode_follows_go() {
        let (g, e) = decode(
            br#"{"TYPE":"a","type":"b","ok":null,"raw":{"x": 1},"inner":{"n":3}}"#,
            &Ty::Struct(&OUTER),
        );
        assert!(e.is_none(), "{e:?}");
        assert_eq!(g.f("type").s(), "b");
        assert!(!g.f("ok").present());
        assert_eq!(g.f("raw").s(), "{\"x\": 1}");
        assert_eq!(g.f("inner").f("n").int(), Some(3));
        let (_, e) = decode(br#"{"type":5}"#, &Ty::Struct(&OUTER));
        assert_eq!(
            e.unwrap(),
            "json: cannot unmarshal number into Go struct field row.type of type string"
        );
        let (_, e) = decode(br#"{"inner":{"n":"x"}}"#, &Ty::Struct(&OUTER));
        assert_eq!(
            e.unwrap(),
            "json: cannot unmarshal string into Go struct field .inner.n of type int"
        );
        let (_, e) = decode(br#"{"inner":{"n":1.5}}"#, &Ty::Struct(&OUTER));
        assert_eq!(
            e.unwrap(),
            "json: cannot unmarshal number 1.5 into Go struct field .inner.n of type int"
        );
        let (g, e) = decode(b"[1]", &Ty::Struct(&OUTER));
        assert_eq!(
            e.unwrap(),
            "json: cannot unmarshal array into Go value of type pkg.row"
        );
        assert!(!g.present());
        let (g, e) = decode(b"null", &Ty::Struct(&OUTER));
        assert!(e.is_none());
        assert_eq!(g.f("type").s(), "");
    }

    #[test]
    fn strings_decode_as_go_decodes_them() {
        let (g, _) = decode(
            b"{\"type\":\"a\\u00e9\\ud83d\\ude00\\ud800x\xff\"}",
            &Ty::Struct(&OUTER),
        );
        assert_eq!(g.f("type").s(), "a\u{e9}\u{1f600}\u{fffd}x\u{fffd}");
    }

    #[test]
    fn a_map_marshals_again_as_go_does() {
        let m = decode_map(br#"{"b":1.0,"a":"<x>","c":[1e21,0.000001,1e-7],"b":2}"#).unwrap();
        assert_eq!(
            m.marshal(),
            r#"{"a":"\u003cx\u003e","b":2,"c":[1e+21,0.000001,1e-7]}"#
        );
        assert_eq!(
            decode_map(b"[1]").unwrap_err(),
            "json: cannot unmarshal array into Go value of type map[string]interface {}"
        );
    }
}
