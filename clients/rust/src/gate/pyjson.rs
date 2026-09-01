//! JSON read and written the way Python's `json` module does it, so the
//! gate can write byte for byte the lines the Python gate writes and read
//! a payload the way `json.loads` reads it.
//!
//! Differences from serde_json that matter here:
//! - objects keep their key order; a repeated key keeps its first place and
//!   takes its last value, as `dict(pairs)` does
//! - integers keep their exact digits; floats print as Python's `repr`
//! - `NaN`, `Infinity` and `-Infinity` are read and written as Python does
//! - `dumps` uses Python's default separators (`", "` and `": "`) and can
//!   escape every non-ASCII character (`ensure_ascii`)

use std::collections::HashMap;

/// A decoded JSON value.
#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    /// An integer as its normalized decimal text, so any size round-trips.
    Int(String),
    Float(f64),
    Str(String),
    List(Vec<Value>),
    Obj(Object),
    /// A Python tuple: a pydantic `model_dump()` keeps a tuple-typed
    /// field as one. It equals no list, and it writes as a JSON array.
    /// Only the predicate stage builds one.
    Tuple(Vec<Value>),
}

/// A JSON object with its key order. Keys and values sit in two parallel
/// lists; an object past `INDEX_AT` keys also keeps a hash index, so a
/// small object (nearly every one) costs no hashing at all.
#[derive(Debug, Clone, Default)]
pub struct Object {
    keys: Vec<String>,
    vals: Vec<Value>,
    index: Option<HashMap<String, usize>>,
}

/// The size past which an object keeps a hash index of its keys.
const INDEX_AT: usize = 16;

impl PartialEq for Object {
    fn eq(&self, other: &Self) -> bool {
        self.keys == other.keys && self.vals == other.vals
    }
}

impl Object {
    pub fn new() -> Self {
        Object::default()
    }

    /// An object with room for `n` keys.
    pub fn with_capacity(n: usize) -> Self {
        Object { keys: Vec::with_capacity(n), vals: Vec::with_capacity(n), index: None }
    }

    fn pos(&self, k: &str) -> Option<usize> {
        match &self.index {
            Some(ix) => ix.get(k).copied(),
            None => self.keys.iter().position(|x| x == k),
        }
    }

    fn reindex(&mut self) {
        self.index = if self.keys.len() > INDEX_AT {
            Some(self.keys.iter().enumerate().map(|(i, k)| (k.clone(), i)).collect())
        } else {
            None
        };
    }

    /// Adds a key at the end, or replaces the value of an existing key in
    /// place.
    pub fn set(&mut self, k: &str, v: impl Into<Value>) -> &mut Self {
        let v = v.into();
        match self.pos(k) {
            Some(i) => self.vals[i] = v,
            None => self.push_new(k.to_string(), v),
        }
        self
    }

    /// Adds a key the object does not hold yet, at the end.
    pub fn push_new(&mut self, k: String, v: Value) {
        debug_assert!(self.pos(&k).is_none());
        if let Some(ix) = &mut self.index {
            ix.insert(k.clone(), self.keys.len());
        }
        self.keys.push(k);
        self.vals.push(v);
        if self.index.is_none() && self.keys.len() > INDEX_AT {
            self.reindex();
        }
    }

    /// Takes a value out, for a reader that consumes the object: the key
    /// stays, holding None, so the object must not be read afterwards.
    pub fn take_value(&mut self, k: &str) -> Option<Value> {
        let i = self.pos(k)?;
        Some(std::mem::replace(&mut self.vals[i], Value::Null))
    }

    /// The builder form of `set`.
    pub fn with(mut self, k: &str, v: impl Into<Value>) -> Self {
        self.set(k, v);
        self
    }

    pub fn get(&self, k: &str) -> Option<&Value> {
        self.pos(k).map(|i| &self.vals[i])
    }

    pub fn get_mut(&mut self, k: &str) -> Option<&mut Value> {
        self.pos(k).map(move |i| &mut self.vals[i])
    }

    /// `del d[k]` when the key is there.
    pub fn remove(&mut self, k: &str) {
        if let Some(i) = self.pos(k) {
            self.keys.remove(i);
            self.vals.remove(i);
            self.reindex();
        }
    }

    /// `dict.get`: the value, or `Null` when the key is absent.
    pub fn value(&self, k: &str) -> &Value {
        static NULL: Value = Value::Null;
        self.get(k).unwrap_or(&NULL)
    }

    pub fn keys(&self) -> &[String] {
        &self.keys
    }

    /// The pairs, in order.
    pub fn iter(&self) -> impl Iterator<Item = (&String, &Value)> {
        self.keys.iter().zip(self.vals.iter())
    }

    pub fn len(&self) -> usize {
        self.keys.len()
    }

    pub fn is_empty(&self) -> bool {
        self.keys.is_empty()
    }
}

impl From<&str> for Value {
    fn from(s: &str) -> Self {
        Value::Str(s.to_string())
    }
}
impl From<String> for Value {
    fn from(s: String) -> Self {
        Value::Str(s)
    }
}
impl From<&String> for Value {
    fn from(s: &String) -> Self {
        Value::Str(s.clone())
    }
}
impl From<bool> for Value {
    fn from(b: bool) -> Self {
        Value::Bool(b)
    }
}
impl From<i64> for Value {
    fn from(n: i64) -> Self {
        Value::Int(n.to_string())
    }
}
impl From<f64> for Value {
    fn from(f: f64) -> Self {
        Value::Float(f)
    }
}
impl From<Object> for Value {
    fn from(o: Object) -> Self {
        Value::Obj(o)
    }
}
impl From<Vec<Value>> for Value {
    fn from(v: Vec<Value>) -> Self {
        Value::List(v)
    }
}
impl<T: Into<Value>> From<Option<T>> for Value {
    fn from(o: Option<T>) -> Self {
        match o {
            Some(v) => v.into(),
            None => Value::Null,
        }
    }
}

impl Value {
    /// Python truthiness of a decoded value.
    pub fn truthy(&self) -> bool {
        match self {
            Value::Null => false,
            Value::Bool(b) => *b,
            Value::Int(t) => t.trim_start_matches('-').trim_start_matches('0') != "",
            Value::Float(f) => *f != 0.0,
            Value::Str(s) => !s.is_empty(),
            Value::List(l) | Value::Tuple(l) => !l.is_empty(),
            Value::Obj(o) => !o.is_empty(),
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Value::Str(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_obj(&self) -> Option<&Object> {
        match self {
            Value::Obj(o) => Some(o),
            _ => None,
        }
    }

    pub fn is_null(&self) -> bool {
        matches!(self, Value::Null)
    }
}

/// Why a load failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LoadError {
    /// Input a stricter reader (pydantic's) reads differently from
    /// `json.loads`: a repeated key, or a NaN or Infinity literal.
    Unsupported,
    /// Python raises JSONDecodeError.
    Syntax(&'static str),
    /// Python raises ValueError: an integer past its digit limit.
    IntDigits,
    /// Python raises RecursionError: nesting past its C recursion limit.
    Recursion,
}

/// `json.loads` gives up past this many nested arrays and objects: its C
/// scanner hits Python's C recursion limit there (measured on the oracle's
/// gate path).
pub const MAX_DEPTH: usize = 9994;

/// Python 3.12 refuses to convert an integer string of more digits than
/// this (`sys.get_int_max_str_digits()`), and `json.loads` raises.
pub const MAX_INT_DIGITS: usize = 4300;

/// `json.loads(text)`.
pub fn loads(text: &str) -> Result<Value, LoadError> {
    load(text, false)
}

/// `loads` that also reports a repeated key or a NaN or Infinity literal as
/// unsupported, for input Python reads with another JSON reader (pydantic).
pub fn loads_strict(text: &str) -> Result<Value, LoadError> {
    load(text, true)
}

fn load(text: &str, strict: bool) -> Result<Value, LoadError> {
    if text.starts_with('\u{feff}') {
        return Err(LoadError::Syntax("unexpected UTF-8 BOM"));
    }
    let mut p = Parser { s: text.as_bytes(), i: 0, strict };
    p.ws();
    let v = p.value()?;
    p.ws();
    if p.i != p.s.len() {
        return Err(LoadError::Syntax("extra data"));
    }
    Ok(v)
}

struct Parser<'a> {
    s: &'a [u8],
    i: usize,
    strict: bool,
}

const SYNTAX: LoadError = LoadError::Syntax("expecting value");

impl Parser<'_> {
    fn ws(&mut self) {
        while self.i < self.s.len() && matches!(self.s[self.i], b' ' | b'\t' | b'\n' | b'\r') {
            self.i += 1;
        }
    }

    fn rest(&self) -> &[u8] {
        &self.s[self.i..]
    }

    /// One scalar at the cursor.
    fn scalar(&mut self) -> Result<Value, LoadError> {
        if self.i >= self.s.len() {
            return Err(SYNTAX);
        }
        let c = self.s[self.i];
        let rest = self.rest();
        if c == b'"' {
            self.i += 1;
            return self.string().map(Value::Str);
        }
        if rest.starts_with(b"null") {
            self.i += 4;
            return Ok(Value::Null);
        }
        if rest.starts_with(b"true") {
            self.i += 4;
            return Ok(Value::Bool(true));
        }
        if rest.starts_with(b"false") {
            self.i += 5;
            return Ok(Value::Bool(false));
        }
        if self.strict && (c == b'N' || c == b'I' || rest.starts_with(b"-I")) {
            return Err(LoadError::Unsupported);
        }
        if rest.starts_with(b"NaN") {
            self.i += 3;
            return Ok(Value::Float(f64::NAN));
        }
        if rest.starts_with(b"Infinity") {
            self.i += 8;
            return Ok(Value::Float(f64::INFINITY));
        }
        if rest.starts_with(b"-Infinity") {
            self.i += 9;
            return Ok(Value::Float(f64::NEG_INFINITY));
        }
        if c == b'-' || c.is_ascii_digit() {
            return self.number();
        }
        Err(SYNTAX)
    }

    /// `"key":` at the cursor, after an object's `{` or `,`.
    fn key(&mut self) -> Result<String, LoadError> {
        self.ws();
        if self.i >= self.s.len() || self.s[self.i] != b'"' {
            return Err(SYNTAX);
        }
        self.i += 1;
        let k = self.string()?;
        self.ws();
        if self.i >= self.s.len() || self.s[self.i] != b':' {
            return Err(SYNTAX);
        }
        self.i += 1;
        self.ws();
        Ok(k)
    }

    /// One value, nested containers included, with a stack of its own, so
    /// no input can overflow the process's stack. Past MAX_DEPTH nested
    /// containers Python's scanner gives up, and so does this.
    fn value(&mut self) -> Result<Value, LoadError> {
        enum Open {
            List(Vec<Value>),
            Obj(Object, String),
        }
        let mut stack: Vec<Open> = Vec::new();
        loop {
            // Read the start of a value.
            let mut val = match self.s.get(self.i) {
                Some(b'[') | Some(b'{') => {
                    if stack.len() + 1 > MAX_DEPTH {
                        return Err(LoadError::Recursion);
                    }
                    let is_list = self.s[self.i] == b'[';
                    self.i += 1;
                    self.ws();
                    if is_list {
                        if self.s.get(self.i) == Some(&b']') {
                            self.i += 1;
                            Value::List(vec![])
                        } else {
                            stack.push(Open::List(vec![]));
                            continue;
                        }
                    } else if self.s.get(self.i) == Some(&b'}') {
                        self.i += 1;
                        Value::Obj(Object::new())
                    } else {
                        let k = self.key()?;
                        stack.push(Open::Obj(Object::new(), k));
                        continue;
                    }
                }
                _ => self.scalar()?,
            };
            // Hand the value to the containers it closes.
            loop {
                match stack.last_mut() {
                    None => return Ok(val),
                    Some(Open::List(items)) => {
                        items.push(val);
                        self.ws();
                        match self.s.get(self.i) {
                            Some(b',') => {
                                self.i += 1;
                                self.ws();
                                break;
                            }
                            Some(b']') => {
                                self.i += 1;
                                val = match stack.pop() {
                                    Some(Open::List(items)) => Value::List(items),
                                    _ => return Err(SYNTAX),
                                };
                            }
                            _ => return Err(SYNTAX),
                        }
                    }
                    Some(Open::Obj(o, k)) => {
                        if self.strict && o.get(k).is_some() {
                            return Err(LoadError::Unsupported);
                        }
                        o.set(k, val);
                        self.ws();
                        match self.s.get(self.i) {
                            Some(b',') => {
                                self.i += 1;
                                let nk = self.key()?;
                                *k = nk;
                                break;
                            }
                            Some(b'}') => {
                                self.i += 1;
                                val = match stack.pop() {
                                    Some(Open::Obj(o, _)) => Value::Obj(o),
                                    _ => return Err(SYNTAX),
                                };
                            }
                            _ => return Err(SYNTAX),
                        }
                    }
                }
            }
        }
    }

    /// Python's NUMBER_RE: `(-?(?:0|[1-9]\d*))(\.\d+)?([eE][-+]?\d+)?`
    fn number(&mut self) -> Result<Value, LoadError> {
        let s = self.s;
        let start = self.i;
        if s[self.i] == b'-' {
            self.i += 1;
        }
        if self.i >= s.len() || !s[self.i].is_ascii_digit() {
            return Err(SYNTAX);
        }
        if s[self.i] == b'0' {
            self.i += 1;
        } else {
            while self.i < s.len() && s[self.i].is_ascii_digit() {
                self.i += 1;
            }
        }
        let int_end = self.i;
        let mut is_float = false;
        if self.i + 1 < s.len() && s[self.i] == b'.' && s[self.i + 1].is_ascii_digit() {
            self.i += 2;
            while self.i < s.len() && s[self.i].is_ascii_digit() {
                self.i += 1;
            }
            is_float = true;
        }
        if self.i < s.len() && (s[self.i] == b'e' || s[self.i] == b'E') {
            let mut j = self.i + 1;
            if j < s.len() && (s[j] == b'+' || s[j] == b'-') {
                j += 1;
            }
            if j < s.len() && s[j].is_ascii_digit() {
                while j < s.len() && s[j].is_ascii_digit() {
                    j += 1;
                }
                self.i = j;
                is_float = true;
            }
        }
        let text = std::str::from_utf8(&s[start..self.i]).map_err(|_| SYNTAX)?;
        if !is_float {
            let t = &text[..int_end - start];
            let neg = t.starts_with('-');
            let digits = t.trim_start_matches('-');
            if digits.len() > MAX_INT_DIGITS {
                // Python raises ValueError here.
                return Err(LoadError::IntDigits);
            }
            let d = digits.trim_start_matches('0');
            if d.is_empty() {
                return Ok(Value::Int("0".into()));
            }
            return Ok(Value::Int(if neg { format!("-{d}") } else { d.to_string() }));
        }
        // Rust's parse gives inf on overflow, as Python's float() does.
        text.parse::<f64>().map(Value::Float).map_err(|_| SYNTAX)
    }

    fn hex4(&self, at: usize) -> Option<u32> {
        if at + 4 > self.s.len() {
            return None;
        }
        let mut r = 0u32;
        for &c in &self.s[at..at + 4] {
            r <<= 4;
            r |= match c {
                b'0'..=b'9' => (c - b'0') as u32,
                b'a'..=b'f' => (c - b'a' + 10) as u32,
                b'A'..=b'F' => (c - b'A' + 10) as u32,
                _ => return None,
            };
        }
        Some(r)
    }

    fn string(&mut self) -> Result<String, LoadError> {
        let mut out: Vec<u8> = Vec::new();
        loop {
            if self.i >= self.s.len() {
                return Err(LoadError::Syntax("unterminated string"));
            }
            let c = self.s[self.i];
            match c {
                b'"' => {
                    self.i += 1;
                    // The text is valid UTF-8 and escapes write whole
                    // characters, so this cannot fail.
                    return String::from_utf8(out).map_err(|_| LoadError::Syntax("invalid UTF-8"));
                }
                b'\\' => {
                    if self.i + 1 >= self.s.len() {
                        return Err(LoadError::Syntax("unterminated string"));
                    }
                    let e = self.s[self.i + 1];
                    self.i += 2;
                    let simple = match e {
                        b'"' => Some(b'"'),
                        b'\\' => Some(b'\\'),
                        b'/' => Some(b'/'),
                        b'b' => Some(8),
                        b'f' => Some(12),
                        b'n' => Some(b'\n'),
                        b'r' => Some(b'\r'),
                        b't' => Some(b'\t'),
                        b'u' => None,
                        _ => return Err(LoadError::Syntax("invalid escape")),
                    };
                    if let Some(b) = simple {
                        out.push(b);
                        continue;
                    }
                    let r = self.hex4(self.i).ok_or(LoadError::Syntax("invalid \\uXXXX escape"))?;
                    self.i += 4;
                    let ch = if (0xd800..0xe000).contains(&r) {
                        let mut pair = None;
                        if r < 0xdc00 && self.i + 6 <= self.s.len() && self.s[self.i] == b'\\' && self.s[self.i + 1] == b'u' {
                            if let Some(r2) = self.hex4(self.i + 2) {
                                if (0xdc00..=0xdfff).contains(&r2) {
                                    self.i += 6;
                                    pair = char::from_u32(0x10000 + ((r - 0xd800) << 10) + (r2 - 0xdc00));
                                }
                            }
                        }
                        // A lone surrogate is a valid Python str: it is kept
                        // as the character that stands for it.
                        pair.unwrap_or_else(|| super::py::text::surrogate_char(r))
                    } else {
                        char::from_u32(r).ok_or(LoadError::Syntax("invalid \\uXXXX escape"))?
                    };
                    let mut buf = [0u8; 4];
                    out.extend_from_slice(ch.encode_utf8(&mut buf).as_bytes());
                }
                c if c < 0x20 => return Err(LoadError::Syntax("invalid control character")),
                _ => {
                    out.push(c);
                    self.i += 1;
                }
            }
        }
    }

}

/// `json.dumps(v, ensure_ascii=ascii)` with the default separators.
pub fn dumps(v: &Value, ascii: bool) -> String {
    let mut b = String::new();
    encode(&mut b, v, ascii);
    b
}

fn encode(b: &mut String, v: &Value, ascii: bool) {
    match v {
        Value::Null => b.push_str("null"),
        Value::Bool(true) => b.push_str("true"),
        Value::Bool(false) => b.push_str("false"),
        Value::Int(t) => b.push_str(t),
        Value::Float(f) => b.push_str(&float_repr(*f)),
        Value::Str(s) => write_string(b, s, ascii),
        Value::List(l) | Value::Tuple(l) => {
            b.push('[');
            for (i, e) in l.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                encode(b, e, ascii);
            }
            b.push(']');
        }
        Value::Obj(o) => {
            b.push('{');
            for (i, (k, x)) in o.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                write_string(b, k, ascii);
                b.push_str(": ");
                encode(b, x, ascii);
            }
            b.push('}');
        }
    }
}

/// `json.dumps(v, indent=indent, ensure_ascii=ascii)`.
pub fn dumps_indent(v: &Value, indent: usize, ascii: bool) -> String {
    dumps_indent_with(v, indent, ascii, &float_repr)
}

/// `dumps_indent` with each float written by `float` (pydantic writes
/// some floats another way than json.dumps).
pub fn dumps_indent_with(v: &Value, indent: usize, ascii: bool, float: &dyn Fn(f64) -> String) -> String {
    fn nl(b: &mut String, unit: usize, level: usize) {
        b.push('\n');
        b.push_str(&" ".repeat(unit * level));
    }
    fn go(b: &mut String, v: &Value, ascii: bool, unit: usize, level: usize, float: &dyn Fn(f64) -> String) {
        match v {
            Value::Float(f) => b.push_str(&float(*f)),
            Value::List(l) | Value::Tuple(l) if l.is_empty() => b.push_str("[]"),
            Value::List(l) | Value::Tuple(l) => {
                b.push('[');
                for (i, e) in l.iter().enumerate() {
                    if i > 0 {
                        b.push(',');
                    }
                    nl(b, unit, level + 1);
                    go(b, e, ascii, unit, level + 1, float);
                }
                nl(b, unit, level);
                b.push(']');
            }
            Value::Obj(o) if o.is_empty() => b.push_str("{}"),
            Value::Obj(o) => {
                b.push('{');
                for (i, (k, x)) in o.iter().enumerate() {
                    if i > 0 {
                        b.push(',');
                    }
                    nl(b, unit, level + 1);
                    write_string(b, k, ascii);
                    b.push_str(": ");
                    go(b, x, ascii, unit, level + 1, float);
                }
                nl(b, unit, level);
                b.push('}');
            }
            other => encode(b, other, ascii),
        }
    }
    let mut b = String::new();
    go(&mut b, v, ascii, indent, 0, float);
    b
}

/// `json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False)`:
/// keys in code point order, a lone surrogate kept as itself.
pub fn canonical_json(v: &Value) -> String {
    fn go(b: &mut String, v: &Value) {
        match v {
            Value::List(l) | Value::Tuple(l) => {
                b.push('[');
                for (i, e) in l.iter().enumerate() {
                    if i > 0 {
                        b.push(',');
                    }
                    go(b, e);
                }
                b.push(']');
            }
            Value::Obj(o) => {
                let mut keys: Vec<&String> = o.keys.iter().collect();
                let cps = |s: &str| -> Vec<u32> { s.chars().map(super::py::re::pycp).collect() };
                keys.sort_by_key(|k| cps(k));
                b.push('{');
                for (i, k) in keys.iter().enumerate() {
                    if i > 0 {
                        b.push(',');
                    }
                    write_string(b, k, false);
                    b.push(':');
                    go(b, o.value(k));
                }
                b.push('}');
            }
            other => encode(b, other, false),
        }
    }
    let mut b = String::new();
    go(&mut b, v);
    b
}

/// A serde_json value as `json.loads` would give it: an integer keeps its
/// digits, any other number is a float.
pub fn from_serde(v: &serde_json::Value) -> Value {
    match v {
        serde_json::Value::Null => Value::Null,
        serde_json::Value::Bool(b) => Value::Bool(*b),
        serde_json::Value::Number(n) => {
            if n.is_i64() || n.is_u64() {
                Value::Int(n.to_string())
            } else {
                Value::Float(n.as_f64().unwrap_or(f64::NAN))
            }
        }
        serde_json::Value::String(s) => Value::Str(s.clone()),
        serde_json::Value::Array(a) => Value::List(a.iter().map(from_serde).collect()),
        serde_json::Value::Object(o) => {
            let mut out = Object::new();
            for (k, x) in o {
                out.set(k, from_serde(x));
            }
            Value::Obj(out)
        }
    }
}

/// A value pydantic serializes as `Any` in JSON mode: NaN and the
/// infinities become null.
pub fn any_json(v: &Value) -> Value {
    match v {
        Value::Float(f) if !f.is_finite() => Value::Null,
        Value::List(l) => Value::List(l.iter().map(any_json).collect()),
        Value::Obj(o) => {
            let mut out = Object::new();
            for (k, x) in o.iter() {
                out.set(k, any_json(x));
            }
            Value::Obj(out)
        }
        other => other.clone(),
    }
}

fn write_u(b: &mut String, r: u32) {
    b.push_str(&format!("\\u{r:04x}"));
}

fn write_string(b: &mut String, s: &str, ascii: bool) {
    b.push('"');
    for ch in s.chars() {
        match ch {
            '"' => b.push_str("\\\""),
            '\\' => b.push_str("\\\\"),
            '\n' => b.push_str("\\n"),
            '\r' => b.push_str("\\r"),
            '\t' => b.push_str("\\t"),
            '\u{8}' => b.push_str("\\b"),
            '\u{c}' => b.push_str("\\f"),
            c if (c as u32) < 0x20 => write_u(b, c as u32),
            c if (c as u32) < 0x7f => b.push(c),
            c if !ascii => b.push(c),
            c if super::py::text::as_surrogate(c).is_some() => {
                write_u(b, super::py::text::as_surrogate(c).unwrap_or(0));
            }
            c => {
                let mut units = [0u16; 2];
                for u in c.encode_utf16(&mut units) {
                    write_u(b, *u as u32);
                }
            }
        }
    }
    b.push('"');
}

/// Python's `repr(float)`: the shortest digits that round-trip, in fixed
/// notation when the decimal exponent is in [-4, 16), else in scientific
/// notation with at least two exponent digits. Inside `json.dumps` NaN and
/// the infinities print as `NaN`, `Infinity` and `-Infinity`.
pub fn float_repr(f: f64) -> String {
    if f.is_nan() {
        return "NaN".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { "Infinity".into() } else { "-Infinity".into() };
    }
    if f == 0.0 {
        return if f.is_sign_negative() { "-0.0".into() } else { "0.0".into() };
    }
    // Rust's `{:e}` gives the shortest round-trip digits, e.g. "-1.2345e6".
    let e = format!("{f:e}");
    let neg = e.starts_with('-');
    let e = e.trim_start_matches('-');
    let (mant, exp) = e.split_once('e').unwrap_or((e, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    let digits: String = mant.chars().filter(|c| *c != '.').collect();
    let out = if (-4..16).contains(&exp) {
        if exp >= 0 {
            let e1 = exp as usize + 1;
            if digits.len() <= e1 {
                format!("{digits}{}.0", "0".repeat(e1 - digits.len()))
            } else {
                format!("{}.{}", &digits[..e1], &digits[e1..])
            }
        } else {
            format!("0.{}{digits}", "0".repeat((-exp - 1) as usize))
        }
    } else {
        let mut m = digits[..1].to_string();
        if digits.len() > 1 {
            m.push('.');
            m.push_str(&digits[1..]);
        }
        let sign = if exp < 0 { '-' } else { '+' };
        format!("{m}e{sign}{:02}", exp.abs())
    };
    if neg {
        format!("-{out}")
    } else {
        out
    }
}

/// Python's `round(x, n)` for the small `n` the gate uses: the float nearest
/// the correctly rounded decimal.
pub fn round(x: f64, n: usize) -> f64 {
    format!("{x:.n$}").parse().unwrap_or(x)
}

/// Python's `repr(float)` outside JSON: `nan`, `inf`, `-inf`, else as
/// `float_repr`.
pub fn py_float_repr(f: f64) -> String {
    if f.is_nan() {
        return "nan".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { "inf".into() } else { "-inf".into() };
    }
    float_repr(f)
}

/// Nesting past which `py_repr` does not decide: Python's repr of a very
/// deep value hits its C recursion limit at a depth the port does not
/// pin down.
const REPR_MAX_DEPTH: usize = 5000;

/// Python's `repr()` of a value `json.loads` returned.
pub fn py_repr(v: &Value) -> super::R<String> {
    let mut out = String::new();
    repr_into(&mut out, v, 0)?;
    Ok(out)
}

fn repr_into(b: &mut String, v: &Value, depth: usize) -> super::R<()> {
    if depth > REPR_MAX_DEPTH {
        return super::undecided("the repr of a value nested very deep");
    }
    match v {
        Value::Null => b.push_str("None"),
        Value::Bool(true) => b.push_str("True"),
        Value::Bool(false) => b.push_str("False"),
        Value::Int(t) => b.push_str(t),
        Value::Float(f) => b.push_str(&py_float_repr(*f)),
        Value::Str(s) => b.push_str(&super::py::text::repr(s)),
        Value::List(l) => {
            b.push('[');
            for (i, e) in l.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                repr_into(b, e, depth + 1)?;
            }
            b.push(']');
        }
        Value::Tuple(l) => {
            b.push('(');
            for (i, e) in l.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                repr_into(b, e, depth + 1)?;
            }
            if l.len() == 1 {
                b.push(',');
            }
            b.push(')');
        }
        Value::Obj(o) => {
            b.push('{');
            for (i, (k, x)) in o.iter().enumerate() {
                if i > 0 {
                    b.push_str(", ");
                }
                b.push_str(&super::py::text::repr(k));
                b.push_str(": ");
                repr_into(b, x, depth + 1)?;
            }
            b.push('}');
        }
    }
    Ok(())
}

/// Python's `str()` of a value `json.loads` returned: a string is itself,
/// anything else its repr.
pub fn py_str(v: &Value) -> super::R<String> {
    match v {
        Value::Str(s) => Ok(s.clone()),
        _ => py_repr(v),
    }
}

/// `str(v or "")`.
pub fn py_str_or_empty(v: &Value) -> super::R<String> {
    if v.truthy() {
        py_str(v)
    } else {
        Ok(String::new())
    }
}

/// The name of a value's Python type, as error messages print it.
pub fn py_type_name(v: &Value) -> &'static str {
    match v {
        Value::Null => "NoneType",
        Value::Bool(_) => "bool",
        Value::Int(_) => "int",
        Value::Float(_) => "float",
        Value::Str(_) => "str",
        Value::List(_) => "list",
        Value::Tuple(_) => "tuple",
        Value::Obj(_) => "dict",
    }
}

/// Python's `repr(str)`.
pub fn repr(s: &str) -> Option<String> {
    Some(super::py::text::repr(s))
}

/// Python's `repr(list[str])`.
pub fn repr_list(ss: &[String]) -> Option<String> {
    Some(super::py::text::repr_list(ss))
}


/// `json.loads(text)` with Python's error: the str() of the exception it
/// raises. Nesting past `max_depth` is the RecursionError a deep stack
/// would raise.
pub fn loads_py(text: &str, max_depth: usize) -> Result<Value, String> {
    match py_decode_error(text, max_depth) {
        Some(PyDecodeFail::Raised(m)) => Err(m),
        Some(PyDecodeFail::TooDeep) => Err("maximum recursion depth exceeded while decoding a JSON object from a unicode string".into()),
        None => loads(text).map_err(|e| match e {
            LoadError::Syntax(m) => m.to_string(),
            LoadError::IntDigits => format!("Exceeds the limit ({MAX_INT_DIGITS} digits) for integer string conversion"),
            LoadError::Recursion => "maximum recursion depth exceeded while decoding a JSON object from a unicode string".into(),
            LoadError::Unsupported => "unsupported".into(),
        }),
    }
}

/// Why `json.loads` of a str refuses it, in Python's own words.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PyDecodeFail {
    /// The str() of what json.loads raises: a JSONDecodeError
    /// ("<msg>: line L column C (char P)", P in code points), or the
    /// ValueError of an integer past the digit limit.
    Raised(String),
    /// Nesting past the caller's `max_depth`, where Python might raise
    /// RecursionError instead.
    TooDeep,
}

/// Where `json.loads(text)` fails, if it does, as CPython 3.12's C scanner
/// (`_json.c`) fails: the same message at the same position. The Go
/// gate's `pyjson.LoadsPy` is the reference. Reads no values; `loads`
/// does that once this finds nothing wrong.
pub fn py_decode_error(text: &str, max_depth: usize) -> Option<PyDecodeFail> {
    let s: Vec<char> = text.chars().collect();
    if s.first() == Some(&'\u{feff}') {
        return Some(PyDecodeFail::Raised(err_at(&s, "Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)));
    }
    let mut d = PyScan { s: &s, depth: 0, max: max_depth };
    match d.whole() {
        Ok(()) => None,
        Err(ScanStop::Iter(i)) => Some(PyDecodeFail::Raised(err_at(&s, "Expecting value", i))),
        Err(ScanStop::Fail(m, i)) => Some(PyDecodeFail::Raised(err_at(&s, m, i))),
        Err(ScanStop::Digits(n)) => Some(PyDecodeFail::Raised(format!(
            "Exceeds the limit ({MAX_INT_DIGITS} digits) for integer string conversion: value has {n} digits; use sys.set_int_max_str_digits() to increase the limit"
        ))),
        Err(ScanStop::Deep) => Some(PyDecodeFail::TooDeep),
    }
}

fn err_at(s: &[char], msg: &str, pos: usize) -> String {
    let mut line = 1;
    let mut last: isize = -1;
    for (i, c) in s.iter().enumerate().take(pos) {
        if *c == '\n' {
            line += 1;
            last = i as isize;
        }
    }
    format!("{msg}: line {line} column {} (char {pos})", pos as isize - last)
}

enum ScanStop {
    /// StopIteration(idx): "Expecting value" at idx.
    Iter(usize),
    Fail(&'static str, usize),
    Digits(usize),
    Deep,
}

struct PyScan<'a> {
    s: &'a [char],
    depth: usize,
    max: usize,
}

impl PyScan<'_> {
    /// raw_decode of the whole text.
    fn whole(&mut self) -> Result<(), ScanStop> {
        let idx = self.ws(0);
        let end = self.scan(idx)?;
        let end = self.ws(end);
        if end != self.s.len() {
            return Err(ScanStop::Fail("Extra data", end));
        }
        Ok(())
    }

    fn ws(&self, mut i: usize) -> usize {
        while i < self.s.len() && matches!(self.s[i], ' ' | '\t' | '\n' | '\r') {
            i += 1;
        }
        i
    }

    fn has(&self, i: usize, lit: &str) -> bool {
        let n = lit.chars().count();
        i + n <= self.s.len() && self.s[i..i + n].iter().copied().eq(lit.chars())
    }

    /// scan_once_unicode: the index after the value at idx.
    fn scan(&mut self, idx: usize) -> Result<usize, ScanStop> {
        if idx >= self.s.len() {
            return Err(ScanStop::Iter(idx));
        }
        let c = self.s[idx];
        match c {
            '"' => self.string(idx + 1),
            '{' | '[' => {
                self.depth += 1;
                if self.depth > self.max {
                    return Err(ScanStop::Deep);
                }
                let r = if c == '{' { self.object(idx + 1) } else { self.array(idx + 1) };
                self.depth -= 1;
                r
            }
            'n' if self.has(idx, "null") => Ok(idx + 4),
            't' if self.has(idx, "true") => Ok(idx + 4),
            'f' if self.has(idx, "false") => Ok(idx + 5),
            'N' if self.has(idx, "NaN") => Ok(idx + 3),
            'I' if self.has(idx, "Infinity") => Ok(idx + 8),
            '-' if self.has(idx, "-Infinity") => Ok(idx + 9),
            _ => self.number(idx),
        }
    }

    /// _match_number_unicode.
    fn number(&self, start: usize) -> Result<usize, ScanStop> {
        let s = self.s;
        let end = s.len() - 1;
        let d = |i: usize| s[i].is_ascii_digit();
        let mut idx = start;
        if s[idx] == '-' {
            idx += 1;
            if idx > end {
                return Err(ScanStop::Iter(start));
            }
        }
        if ('1'..='9').contains(&s[idx]) {
            idx += 1;
            while idx <= end && d(idx) {
                idx += 1;
            }
        } else if s[idx] == '0' {
            idx += 1;
        } else {
            return Err(ScanStop::Iter(start));
        }
        let mut is_float = false;
        if idx < end && s[idx] == '.' && d(idx + 1) {
            is_float = true;
            idx += 2;
            while idx <= end && d(idx) {
                idx += 1;
            }
        }
        if idx < end && (s[idx] == 'e' || s[idx] == 'E') {
            let e = idx;
            idx += 1;
            if idx < end && (s[idx] == '-' || s[idx] == '+') {
                idx += 1;
            }
            while idx <= end && d(idx) {
                idx += 1;
            }
            if d(idx - 1) {
                is_float = true;
            } else {
                idx = e;
            }
        }
        if !is_float {
            let digits = idx - start - usize::from(s[start] == '-');
            if digits > MAX_INT_DIGITS {
                return Err(ScanStop::Digits(digits));
            }
        }
        Ok(idx)
    }

    /// scanstring_unicode (strict), from just after the opening quote.
    fn string(&self, mut end: usize) -> Result<usize, ScanStop> {
        let s = self.s;
        let begin = end - 1;
        loop {
            if end >= s.len() {
                return Err(ScanStop::Fail("Unterminated string starting at", begin));
            }
            let c = s[end];
            if c == '"' {
                return Ok(end + 1);
            }
            if (c as u32) < 0x20 {
                return Err(ScanStop::Fail("Invalid control character at", end));
            }
            if c != '\\' {
                end += 1;
                continue;
            }
            end += 1;
            if end >= s.len() {
                return Err(ScanStop::Fail("Unterminated string starting at", begin));
            }
            let c = s[end];
            if c != 'u' {
                if !matches!(c, '"' | '\\' | '/' | 'b' | 'f' | 'n' | 'r' | 't') {
                    return Err(ScanStop::Fail("Invalid \\escape", end - 1));
                }
                end += 1;
                continue;
            }
            end += 1;
            // The C scanner wants a character after the four digits.
            if end + 4 >= s.len() {
                return Err(ScanStop::Fail("Invalid \\uXXXX escape", end - 1));
            }
            let u = self.hex4(end)?;
            end += 4;
            if (0xD800..=0xDBFF).contains(&u) && end + 6 < s.len() && s[end] == '\\' && s[end + 1] == 'u' {
                let u2 = self.hex4(end + 2)?;
                if (0xDC00..=0xDFFF).contains(&u2) {
                    end += 6;
                }
            }
        }
    }

    fn hex4(&self, at: usize) -> Result<u32, ScanStop> {
        let mut v = 0u32;
        for k in 0..4 {
            match self.s[at + k].to_digit(16) {
                Some(x) if self.s[at + k].is_ascii_hexdigit() => v = v * 16 + x,
                _ => return Err(ScanStop::Fail("Invalid \\uXXXX escape", at - 1)),
            }
        }
        Ok(v)
    }

    /// _parse_object_unicode, from just after the '{'.
    fn object(&mut self, idx: usize) -> Result<usize, ScanStop> {
        let n = self.s.len();
        let mut idx = self.ws(idx);
        if idx < n && self.s[idx] == '}' {
            return Ok(idx + 1);
        }
        loop {
            if idx >= n || self.s[idx] != '"' {
                return Err(ScanStop::Fail("Expecting property name enclosed in double quotes", idx));
            }
            let next = self.string(idx + 1)?;
            idx = self.ws(next);
            if idx >= n || self.s[idx] != ':' {
                return Err(ScanStop::Fail("Expecting ':' delimiter", idx));
            }
            idx = self.ws(idx + 1);
            let next = self.scan(idx)?;
            idx = self.ws(next);
            if idx < n && self.s[idx] == '}' {
                return Ok(idx + 1);
            }
            if idx >= n || self.s[idx] != ',' {
                return Err(ScanStop::Fail("Expecting ',' delimiter", idx));
            }
            idx = self.ws(idx + 1);
        }
    }

    /// _parse_array_unicode, from just after the '['.
    fn array(&mut self, idx: usize) -> Result<usize, ScanStop> {
        let n = self.s.len();
        let mut idx = self.ws(idx);
        if idx < n && self.s[idx] == ']' {
            return Ok(idx + 1);
        }
        loop {
            let next = self.scan(idx)?;
            idx = self.ws(next);
            if idx < n && self.s[idx] == ']' {
                return Ok(idx + 1);
            }
            if idx >= n || self.s[idx] != ',' {
                return Err(ScanStop::Fail("Expecting ',' delimiter", idx));
            }
            idx = self.ws(idx + 1);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> serde_json::Value {
        let p = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../go/internal/pyjson/testdata/pyjson.json");
        serde_json::from_str(&std::fs::read_to_string(p).unwrap()).unwrap()
    }

    #[test]
    fn loads_and_dumps_match_python() {
        let f = fixture();
        for c in f["loads"].as_array().unwrap() {
            let text = c["text"].as_str().unwrap();
            let ok = c["ok"].as_bool().unwrap();
            let got = loads(text);
            assert_eq!(got.is_ok(), ok, "loads({text:?}) = {got:?}");
            if let Ok(v) = got {
                assert_eq!(dumps(&v, true), c["ascii"].as_str().unwrap(), "ascii {text:?}");
                assert_eq!(dumps(&v, false), c["utf8"].as_str().unwrap(), "utf8 {text:?}");
            }
        }
    }

    /// Python's `float.hex()` form, e.g. `-0x1.999999999999ap-4`.
    fn hex_float(s: &str) -> f64 {
        let (neg, s) = match s.strip_prefix('-') {
            Some(r) => (true, r),
            None => (false, s),
        };
        match s {
            "inf" => return if neg { f64::NEG_INFINITY } else { f64::INFINITY },
            "nan" => return f64::NAN,
            _ => {}
        }
        let s = s.strip_prefix("0x").unwrap();
        let (mant, exp) = s.split_once('p').unwrap();
        let (int, frac) = mant.split_once('.').unwrap_or((mant, ""));
        let mut m = u64::from_str_radix(int, 16).unwrap() as f64;
        for (i, c) in frac.chars().enumerate() {
            m += c.to_digit(16).unwrap() as f64 / 16f64.powi(i as i32 + 1);
        }
        let x = m * 2f64.powi(exp.parse::<i32>().unwrap());
        if neg {
            -x
        } else {
            x
        }
    }

    #[test]
    fn float_repr_and_round_match_python() {
        let f = fixture();
        for c in f["floats"].as_array().unwrap() {
            let x = hex_float(c["bits"].as_str().unwrap());
            assert_eq!(float_repr(x), c["repr"].as_str().unwrap(), "repr of {x:e}");
        }
        for c in f["rounds"].as_array().unwrap() {
            let x = hex_float(c["bits"].as_str().unwrap());
            let n = c["n"].as_u64().unwrap() as usize;
            assert_eq!(float_repr(round(x, n)), c["repr"].as_str().unwrap(), "round({x}, {n})");
        }
    }

    #[test]
    fn repr_matches_python() {
        let f = fixture();
        for c in f["reprs"].as_array().unwrap() {
            let s = c["s"].as_str().unwrap();
            assert_eq!(repr(s).unwrap(), c["repr"].as_str().unwrap(), "repr({s:?})");
        }
    }

    #[test]
    fn strict_refuses_what_pydantic_reads_differently() {
        for t in [r#"{"a": 1, "a": 2}"#, r#"{"a": NaN}"#, "[Infinity]", "[-Infinity]"] {
            assert_eq!(loads_strict(t), Err(LoadError::Unsupported), "{t}");
        }
        assert!(loads_strict(r#"{"a": [1, -2.5e3, "x"]}"#).is_ok());
    }

    #[test]
    fn lone_surrogates_long_integers_and_deep_nesting_are_pythons() {
        let v = loads(r#""a\ud800""#).unwrap();
        assert_eq!(v, Value::Str(format!("a{}", super::super::py::text::surrogate_char(0xd800))));
        assert_eq!(dumps(&v, true), r#""a\ud800""#);
        assert!(matches!(loads(&"1".repeat(4301)), Err(LoadError::IntDigits)));
        assert!(matches!(loads(&format!("-{}", "1".repeat(4301))), Err(LoadError::IntDigits)));
        assert!(loads(&format!("{}{}", "[".repeat(9994), "]".repeat(9994))).is_ok());
        assert!(loads(&format!("{}{}", "[".repeat(9995), "]".repeat(9995))).is_err());
        assert!(loads(&"1".repeat(4300)).is_ok());
        // A float has no digit limit in Python.
        assert!(loads(&format!("{}.5", "1".repeat(4301))).is_ok());
    }

    #[test]
    fn truthiness_is_pythons() {
        let v = loads(r#"[0, -0, 0.0, "", [], {}, null, false, 1, "x", [0], {"a": 0}, true]"#).unwrap();
        let want = [false, false, false, false, false, false, false, false, true, true, true, true, true];
        if let Value::List(l) = v {
            for (i, e) in l.iter().enumerate() {
                assert_eq!(e.truthy(), want[i], "#{i}");
            }
        } else {
            panic!()
        }
    }

    #[test]
    fn py_decode_error_is_pythons_json_decode_error() {
        // Each message is the str() of what CPython 3.12's json.loads raised.
        let cases: &[(&str, Option<&str>)] = &[
            ("{\"a\":1,}", Some("Expecting property name enclosed in double quotes: line 1 column 8 (char 7)")),
            ("[1,]", Some("Expecting value: line 1 column 4 (char 3)")),
            ("maybe", Some("Expecting value: line 1 column 1 (char 0)")),
            ("\u{feff}{}", Some("Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)")),
            ("\"a\\x\"", Some("Invalid \\escape: line 1 column 3 (char 2)")),
            ("[1 2]", Some("Expecting ',' delimiter: line 1 column 4 (char 3)")),
            ("{\"a\" 1}", Some("Expecting ':' delimiter: line 1 column 6 (char 5)")),
            ("11111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111", Some("Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit")),
            ("-11111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111", Some("Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit")),
            ("\"\\ud800", Some("Invalid \\uXXXX escape: line 1 column 3 (char 2)")),
            ("\"\\u12\"", Some("Invalid \\uXXXX escape: line 1 column 3 (char 2)")),
            ("[-]", Some("Expecting value: line 1 column 2 (char 1)")),
            ("\"a\u{1}\"", Some("Invalid control character at: line 1 column 3 (char 2)")),
            ("{\"a\":1} x", Some("Extra data: line 1 column 9 (char 8)")),
            ("", Some("Expecting value: line 1 column 1 (char 0)")),
            ("  \n [", Some("Expecting value: line 2 column 3 (char 5)")),
            ("{\"a\":1", Some("Expecting ',' delimiter: line 1 column 7 (char 6)")),
            ("[1.5e]", Some("Expecting ',' delimiter: line 1 column 5 (char 4)")),
            ("-", Some("Expecting value: line 1 column 1 (char 0)")),
            ("\"\\ud800\\uZZZZ \"", Some("Invalid \\uXXXX escape: line 1 column 9 (char 8)")),
            ("\u{e9}\n\n  x", Some("Expecting value: line 1 column 1 (char 0)")),
            ("{\"k\": [1, 2, {\"z\": nul}]}", Some("Expecting value: line 1 column 20 (char 19)")),
            ("[NaN, Infinity, -Infinity, 1e400]", None),
            ("{\"a\": 1, \"a\": 2}", None),
            ("\"\\ud800\"", None),
            ("[1e5, -0, 0.5, 2E-3]", None),
            ("01", Some("Extra data: line 1 column 2 (char 1)")),
            ("\"unterminated", Some("Unterminated string starting at: line 1 column 1 (char 0)")),
            ("[[[[[]]]]]", None),
        ];
        for (text, want) in cases {
            let got = match py_decode_error(text, 900) {
                None => None,
                Some(PyDecodeFail::Raised(m)) => Some(m),
                Some(PyDecodeFail::TooDeep) => Some("too deep".to_string()),
            };
            assert_eq!(got.as_deref(), *want, "{text:?}");
        }
        assert_eq!(py_decode_error(&"[".repeat(901), 900), Some(PyDecodeFail::TooDeep));
    }
}
