//! A translation of Python 3.12's tomllib (`_parser.py` and `_re.py`, by
//! Taneli Hukkinen, MIT), one function at a time, so a text gives the
//! value `tomllib.loads` gives, or a `TomlDecodeError` where it raises one.
//! Positions are byte offsets; an error's line and column count
//! characters, as Python's do. The Go client's `switchyard/toml.go` is
//! the reference.

use std::collections::HashMap;

/// `tomllib.TOMLDecodeError`.
#[derive(Debug, Clone, PartialEq)]
pub struct TomlDecodeError(pub String, pub bool);

impl TomlDecodeError {
    /// Whether this is the ValueError int() raises past Python's 4300-digit
    /// limit, which is not a TOMLDecodeError and which the oracle's readers
    /// do not catch.
    pub fn is_value_error(&self) -> bool {
        self.1
    }
}

impl std::fmt::Display for TomlDecodeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

/// A TOML date, time or date-time.
#[derive(Debug, Clone, PartialEq)]
pub struct DateTime {
    /// "date", "time" or "datetime".
    pub kind: &'static str,
    pub year: i32,
    pub month: u32,
    pub day: u32,
    pub hour: u32,
    pub minute: u32,
    pub second: u32,
    pub us: u32,
    /// None (no zone), Some(0) (UTC) or Some(minutes east).
    pub offset: Option<i32>,
}

impl DateTime {
    fn hms(&self) -> String {
        let mut s = format!("{}, {}", self.hour, self.minute);
        if self.second != 0 || self.us != 0 {
            s += &format!(", {}", self.second);
        }
        if self.us != 0 {
            s += &format!(", {}", self.us);
        }
        s
    }

    /// `repr()` of the value.
    pub fn repr(&self) -> String {
        match self.kind {
            "date" => format!("datetime.date({}, {}, {})", self.year, self.month, self.day),
            "time" => format!("datetime.time({})", self.hms()),
            _ => {
                let mut s = format!("datetime.datetime({}, {}, {}, {}", self.year, self.month, self.day, self.hms());
                match self.offset {
                    None => {}
                    Some(0) => s += ", tzinfo=datetime.timezone.utc",
                    Some(m) => {
                        let secs = m as i64 * 60;
                        let days = secs.div_euclid(86400);
                        let rest = secs.rem_euclid(86400);
                        let td = match (days, rest) {
                            (0, r) => format!("seconds={r}"),
                            (d, 0) => format!("days={d}"),
                            (d, r) => format!("days={d}, seconds={r}"),
                        };
                        s += &format!(", tzinfo=datetime.timezone(datetime.timedelta({td}))");
                    }
                }
                s + ")"
            }
        }
    }

    /// `str()` of the value: isoformat, with a space in a date-time.
    pub fn str(&self) -> String {
        let date = format!("{:04}-{:02}-{:02}", self.year, self.month, self.day);
        let mut tm = format!("{:02}:{:02}:{:02}", self.hour, self.minute, self.second);
        if self.us != 0 {
            tm += &format!(".{:06}", self.us);
        }
        match self.kind {
            "date" => date,
            "time" => tm,
            _ => {
                let mut s = format!("{date} {tm}");
                if let Some(m) = self.offset {
                    let (sign, m) = if m < 0 { ('-', -m) } else { ('+', m) };
                    s += &format!("{sign}{:02}:{:02}", m / 60, m % 60);
                }
                s
            }
        }
    }
}

/// A TOML value.
#[derive(Debug, Clone, PartialEq)]
pub enum Tv {
    Str(String),
    Int(i64),
    /// An integer past 64 bits: tomllib's int() keeps any size.
    BigInt(String),
    Float(f64),
    Bool(bool),
    DateTime(DateTime),
    Arr(Vec<Tv>),
    Table(Table),
}

/// A TOML table (a Python dict): keys in insertion order.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Table {
    pub keys: Vec<String>,
    pub vals: HashMap<String, Tv>,
}

impl Table {
    pub fn get(&self, k: &str) -> Option<&Tv> {
        self.vals.get(k)
    }

    pub fn table(&self, k: &str) -> Option<&Table> {
        match self.vals.get(k) {
            Some(Tv::Table(t)) => Some(t),
            _ => None,
        }
    }

    pub fn str(&self, k: &str) -> Option<&str> {
        match self.vals.get(k) {
            Some(Tv::Str(s)) => Some(s),
            _ => None,
        }
    }

    fn put(&mut self, k: &str, v: Tv) {
        if !self.vals.contains_key(k) {
            self.keys.push(k.to_string());
        }
        self.vals.insert(k.to_string(), v);
    }
}

const FROZEN: u8 = 0;
const EXPLICIT_NEST: u8 = 1;

#[derive(Default)]
struct FlagNode {
    flags: [bool; 2],
    recursive: [bool; 2],
    nested: HashMap<String, FlagNode>,
}

#[derive(Default)]
struct Flags {
    root: HashMap<String, FlagNode>,
    pending: Vec<(Vec<String>, u8)>,
}

impl Flags {
    fn add_pending(&mut self, key: &[String], flag: u8) {
        if !self.pending.iter().any(|(k, f)| k == key && *f == flag) {
            self.pending.push((key.to_vec(), flag));
        }
    }

    fn finalize_pending(&mut self) {
        for (k, f) in std::mem::take(&mut self.pending) {
            self.set(&k, f, false);
        }
    }

    fn unset_all(&mut self, key: &[String]) {
        let mut cont = &mut self.root;
        for k in &key[..key.len() - 1] {
            match cont.get_mut(k) {
                Some(n) => cont = &mut n.nested,
                None => return,
            }
        }
        cont.remove(&key[key.len() - 1]);
    }

    fn set(&mut self, key: &[String], flag: u8, recursive: bool) {
        let mut cont = &mut self.root;
        for k in &key[..key.len() - 1] {
            cont = &mut cont.entry(k.clone()).or_default().nested;
        }
        let n = cont.entry(key[key.len() - 1].clone()).or_default();
        if recursive {
            n.recursive[flag as usize] = true;
        } else {
            n.flags[flag as usize] = true;
        }
    }

    fn is(&self, key: &[String], flag: u8) -> bool {
        if key.is_empty() {
            return false;
        }
        let mut cont = &self.root;
        for k in &key[..key.len() - 1] {
            match cont.get(k) {
                None => return false,
                Some(n) => {
                    if n.recursive[flag as usize] {
                        return true;
                    }
                    cont = &n.nested;
                }
            }
        }
        match cont.get(&key[key.len() - 1]) {
            Some(n) => n.flags[flag as usize] || n.recursive[flag as usize],
            None => false,
        }
    }
}

/// NestedDict.get_or_create_nest; None is its KeyError.
fn get_or_create_nest<'a>(root: &'a mut Table, key: &[String], access_lists: bool) -> Option<&'a mut Table> {
    let mut cont = root;
    for k in key {
        if !cont.vals.contains_key(k) {
            cont.put(k, Tv::Table(Table::default()));
        }
        let is_list = matches!(cont.vals.get(k), Some(Tv::Arr(_)));
        let v = cont.vals.get_mut(k).expect("just put");
        let v = if is_list && access_lists {
            match v {
                Tv::Arr(l) => l.last_mut()?,
                _ => return None,
            }
        } else {
            v
        };
        match v {
            Tv::Table(t) => cont = t,
            _ => return None,
        }
    }
    Some(cont)
}

fn append_nest_to_list(root: &mut Table, key: &[String]) -> Option<()> {
    let cont = get_or_create_nest(root, &key[..key.len() - 1], true)?;
    let last = &key[key.len() - 1];
    match cont.vals.get_mut(last) {
        Some(Tv::Arr(l)) => l.push(Tv::Table(Table::default())),
        Some(_) => return None,
        None => cont.put(last, Tv::Arr(vec![Tv::Table(Table::default())])),
    }
    Some(())
}

struct P<'a> {
    src: &'a str,
    b: &'a [u8],
}

type R<T> = Result<T, TomlDecodeError>;

fn is_bare(c: u8) -> bool {
    c.is_ascii_alphanumeric() || c == b'-' || c == b'_'
}

fn is_ctl(c: u8) -> bool {
    c < 32 || c == 127
}

fn illegal_basic(c: u8) -> bool {
    is_ctl(c) && c != b'\t'
}

fn illegal_multiline(c: u8) -> bool {
    is_ctl(c) && c != b'\t' && c != b'\n'
}

/// `repr()` of a str, for an error message.
fn py_repr(s: &str) -> String {
    crate::gate::py::text::repr(s)
}

fn key_repr(k: &[String]) -> String {
    let parts: Vec<String> = k.iter().map(|s| py_repr(s)).collect();
    if parts.len() == 1 {
        format!("({},)", parts[0])
    } else {
        format!("({})", parts.join(", "))
    }
}

impl<'a> P<'a> {
    fn at(&self, pos: usize) -> Option<u8> {
        self.b.get(pos).copied()
    }

    fn starts(&self, pos: usize, s: &str) -> bool {
        self.b.get(pos..).is_some_and(|r| r.starts_with(s.as_bytes()))
    }

    fn err(&self, pos: usize, msg: &str) -> TomlDecodeError {
        let coord = if pos >= self.b.len() {
            "end of document".to_string()
        } else {
            let before = &self.src[..pos];
            let line = before.matches('\n').count() + 1;
            let col = if line == 1 {
                before.chars().count() + 1
            } else {
                let nl = before.rfind('\n').expect("a line past the first");
                self.src[nl..pos].chars().count()
            };
            format!("line {line}, column {col}")
        };
        TomlDecodeError(format!("{msg} (at {coord})"), false)
    }

    fn char_repr(&self, pos: usize) -> String {
        let c = self.src[pos..].chars().next().unwrap_or('\0');
        py_repr(&c.to_string())
    }

    fn skip_ws(&self, mut pos: usize) -> usize {
        while matches!(self.at(pos), Some(b' ' | b'\t')) {
            pos += 1;
        }
        pos
    }

    fn skip_ws_nl(&self, mut pos: usize) -> usize {
        while matches!(self.at(pos), Some(b' ' | b'\t' | b'\n')) {
            pos += 1;
        }
        pos
    }

    fn skip_until(&self, pos: usize, expect: &str, error_on: fn(u8) -> bool, error_on_eof: bool) -> R<usize> {
        let new_pos = match self.src[pos..].find(expect) {
            Some(i) => pos + i,
            None => {
                if error_on_eof {
                    return Err(self.err(self.b.len(), &format!("Expected {}", py_repr(expect))));
                }
                self.b.len()
            }
        };
        for i in pos..new_pos {
            if error_on(self.b[i]) {
                return Err(self.err(i, &format!("Found invalid character {}", self.char_repr(i))));
            }
        }
        Ok(new_pos)
    }

    fn skip_comment(&self, pos: usize) -> R<usize> {
        if self.at(pos) == Some(b'#') {
            return self.skip_until(pos + 1, "\n", illegal_basic, false);
        }
        Ok(pos)
    }

    fn skip_comments_and_array_ws(&self, mut pos: usize) -> R<usize> {
        loop {
            let before = pos;
            pos = self.skip_ws_nl(pos);
            pos = self.skip_comment(pos)?;
            if pos == before {
                return Ok(pos);
            }
        }
    }

    fn create_dict_rule(&self, pos: usize, root: &mut Table, flags: &mut Flags) -> R<(usize, Vec<String>)> {
        let pos = self.skip_ws(pos + 1);
        let (pos, key) = self.parse_key(pos)?;
        if flags.is(&key, EXPLICIT_NEST) || flags.is(&key, FROZEN) {
            return Err(self.err(pos, &format!("Cannot declare {} twice", key_repr(&key))));
        }
        flags.set(&key, EXPLICIT_NEST, false);
        if get_or_create_nest(root, &key, true).is_none() {
            return Err(self.err(pos, "Cannot overwrite a value"));
        }
        if !self.starts(pos, "]") {
            return Err(self.err(pos, "Expected ']' at the end of a table declaration"));
        }
        Ok((pos + 1, key))
    }

    fn create_list_rule(&self, pos: usize, root: &mut Table, flags: &mut Flags) -> R<(usize, Vec<String>)> {
        let pos = self.skip_ws(pos + 2);
        let (pos, key) = self.parse_key(pos)?;
        if flags.is(&key, FROZEN) {
            return Err(self.err(pos, &format!("Cannot mutate immutable namespace {}", key_repr(&key))));
        }
        flags.unset_all(&key);
        flags.set(&key, EXPLICIT_NEST, false);
        if append_nest_to_list(root, &key).is_none() {
            return Err(self.err(pos, "Cannot overwrite a value"));
        }
        if !self.starts(pos, "]]") {
            return Err(self.err(pos, "Expected ']]' at the end of an array declaration"));
        }
        Ok((pos + 2, key))
    }

    fn key_value_rule(&self, pos: usize, root: &mut Table, flags: &mut Flags, header: &[String]) -> R<usize> {
        let (pos, key, value) = self.parse_key_value_pair(pos)?;
        let (parent, stem) = (&key[..key.len() - 1], &key[key.len() - 1]);
        let abs_parent: Vec<String> = header.iter().chain(parent).cloned().collect();
        for i in 1..key.len() {
            let cont_key: Vec<String> = header.iter().chain(&key[..i]).cloned().collect();
            if flags.is(&cont_key, EXPLICIT_NEST) {
                return Err(self.err(pos, &format!("Cannot redefine namespace {}", key_repr(&cont_key))));
            }
            flags.add_pending(&cont_key, EXPLICIT_NEST);
        }
        if flags.is(&abs_parent, FROZEN) {
            return Err(self.err(pos, &format!("Cannot mutate immutable namespace {}", key_repr(&abs_parent))));
        }
        let frozen = matches!(value, Tv::Table(_) | Tv::Arr(_));
        let Some(nest) = get_or_create_nest(root, &abs_parent, true) else {
            return Err(self.err(pos, "Cannot overwrite a value"));
        };
        if nest.vals.contains_key(stem) {
            return Err(self.err(pos, "Cannot overwrite a value"));
        }
        nest.put(stem, value);
        if frozen {
            let full: Vec<String> = header.iter().chain(&key).cloned().collect();
            flags.set(&full, FROZEN, true);
        }
        Ok(pos)
    }

    fn parse_key_value_pair(&self, pos: usize) -> R<(usize, Vec<String>, Tv)> {
        let (pos, key) = self.parse_key(pos)?;
        if self.at(pos) != Some(b'=') {
            return Err(self.err(pos, "Expected '=' after a key in a key/value pair"));
        }
        let pos = self.skip_ws(pos + 1);
        let (pos, value) = self.parse_value(pos)?;
        Ok((pos, key, value))
    }

    fn parse_key(&self, pos: usize) -> R<(usize, Vec<String>)> {
        let (mut pos, part) = self.parse_key_part(pos)?;
        let mut key = vec![part];
        pos = self.skip_ws(pos);
        loop {
            if self.at(pos) != Some(b'.') {
                return Ok((pos, key));
            }
            pos = self.skip_ws(pos + 1);
            let (p2, part) = self.parse_key_part(pos)?;
            key.push(part);
            pos = self.skip_ws(p2);
        }
    }

    fn parse_key_part(&self, pos: usize) -> R<(usize, String)> {
        match self.at(pos) {
            Some(c) if is_bare(c) => {
                let mut end = pos;
                while self.at(end).is_some_and(is_bare) {
                    end += 1;
                }
                Ok((end, self.src[pos..end].to_string()))
            }
            Some(b'\'') => self.parse_literal_str(pos),
            Some(b'"') => self.parse_basic_str(pos + 1, false),
            _ => Err(self.err(pos, "Invalid initial character for a key part")),
        }
    }

    fn parse_array(&self, pos: usize) -> R<(usize, Vec<Tv>)> {
        let mut array = vec![];
        let mut pos = self.skip_comments_and_array_ws(pos + 1)?;
        if self.starts(pos, "]") {
            return Ok((pos + 1, array));
        }
        loop {
            let (p2, val) = self.parse_value(pos)?;
            array.push(val);
            pos = self.skip_comments_and_array_ws(p2)?;
            match self.at(pos) {
                Some(b']') => return Ok((pos + 1, array)),
                Some(b',') => {}
                _ => return Err(self.err(pos, "Unclosed array")),
            }
            pos = self.skip_comments_and_array_ws(pos + 1)?;
            if self.starts(pos, "]") {
                return Ok((pos + 1, array));
            }
        }
    }

    fn parse_inline_table(&self, pos: usize) -> R<(usize, Table)> {
        let mut nested = Table::default();
        let mut flags = Flags::default();
        let mut pos = self.skip_ws(pos + 1);
        if self.starts(pos, "}") {
            return Ok((pos + 1, nested));
        }
        loop {
            let (p2, key, value) = self.parse_key_value_pair(pos)?;
            pos = p2;
            let (parent, stem) = (&key[..key.len() - 1], &key[key.len() - 1]);
            if flags.is(&key, FROZEN) {
                return Err(self.err(pos, &format!("Cannot mutate immutable namespace {}", key_repr(&key))));
            }
            let frozen = matches!(value, Tv::Table(_) | Tv::Arr(_));
            let Some(nest) = get_or_create_nest(&mut nested, parent, false) else {
                return Err(self.err(pos, "Cannot overwrite a value"));
            };
            if nest.vals.contains_key(stem) {
                return Err(self.err(pos, &format!("Duplicate inline table key {}", py_repr(stem))));
            }
            nest.put(stem, value);
            pos = self.skip_ws(pos);
            match self.at(pos) {
                Some(b'}') => return Ok((pos + 1, nested)),
                Some(b',') => {}
                _ => return Err(self.err(pos, "Unclosed inline table")),
            }
            if frozen {
                flags.set(&key, FROZEN, true);
            }
            pos = self.skip_ws(pos + 1);
        }
    }

    fn parse_basic_str_escape(&self, pos: usize, multiline: bool) -> R<(usize, String)> {
        let id = self.b.get(pos..(pos + 2).min(self.b.len())).unwrap_or(b"");
        let mut pos = pos + 2;
        if multiline && (id == b"\\ " || id == b"\\\t" || id == b"\\\n") {
            if id != b"\\\n" {
                pos = self.skip_ws(pos);
                match self.at(pos) {
                    None => return Ok((pos, String::new())),
                    Some(b'\n') => pos += 1,
                    Some(_) => return Err(self.err(pos, "Unescaped '\\' in a string")),
                }
            }
            return Ok((self.skip_ws_nl(pos), String::new()));
        }
        match id {
            b"\\u" => self.parse_hex_char(pos, 4),
            b"\\U" => self.parse_hex_char(pos, 8),
            b"\\b" => Ok((pos, "\u{8}".into())),
            b"\\t" => Ok((pos, "\t".into())),
            b"\\n" => Ok((pos, "\n".into())),
            b"\\f" => Ok((pos, "\u{c}".into())),
            b"\\r" => Ok((pos, "\r".into())),
            b"\\\"" => Ok((pos, "\"".into())),
            b"\\\\" => Ok((pos, "\\".into())),
            _ => Err(self.err(pos.min(self.b.len()), "Unescaped '\\' in a string")),
        }
    }

    fn parse_hex_char(&self, pos: usize, n: usize) -> R<(usize, String)> {
        // src[pos:pos+n] counts characters; a non-ASCII one is never a hex
        // digit, so bytes give the same answer.
        let hex = self.b.get(pos..(pos + n).min(self.b.len())).unwrap_or(b"");
        if hex.len() != n || !hex.iter().all(u8::is_ascii_hexdigit) {
            return Err(self.err(pos.min(self.b.len()), "Invalid hex value"));
        }
        let v = u32::from_str_radix(std::str::from_utf8(hex).unwrap_or("0"), 16).unwrap_or(u32::MAX);
        let pos = pos + n;
        match char::from_u32(v) {
            Some(c) => Ok((pos, c.to_string())),
            None => Err(self.err(pos, "Escaped character is not a Unicode scalar value")),
        }
    }

    fn parse_literal_str(&self, pos: usize) -> R<(usize, String)> {
        let start = pos + 1;
        let end = self.skip_until(start, "'", illegal_basic, true)?;
        Ok((end + 1, self.src[start..end].to_string()))
    }

    fn parse_multiline_str(&self, pos: usize, literal: bool) -> R<(usize, String)> {
        let mut pos = pos + 3;
        if self.starts(pos, "\n") {
            pos += 1;
        }
        let (delim, mut result);
        if literal {
            delim = "'";
            let end = self.skip_until(pos, "'''", illegal_multiline, true)?;
            result = self.src[pos..end].to_string();
            pos = end + 3;
        } else {
            delim = "\"";
            let (p2, r) = self.parse_basic_str(pos, true)?;
            pos = p2;
            result = r;
        }
        if !self.starts(pos, delim) {
            return Ok((pos, result));
        }
        pos += 1;
        result.push_str(delim);
        if !self.starts(pos, delim) {
            return Ok((pos, result));
        }
        pos += 1;
        result.push_str(delim);
        Ok((pos, result))
    }

    fn parse_basic_str(&self, mut pos: usize, multiline: bool) -> R<(usize, String)> {
        let error_on = if multiline { illegal_multiline } else { illegal_basic };
        let mut result = String::new();
        let mut start = pos;
        loop {
            let Some(c) = self.at(pos) else {
                return Err(self.err(pos, "Unterminated string"));
            };
            if c == b'"' {
                if !multiline {
                    result.push_str(&self.src[start..pos]);
                    return Ok((pos + 1, result));
                }
                if self.starts(pos, "\"\"\"") {
                    result.push_str(&self.src[start..pos]);
                    return Ok((pos + 3, result));
                }
                pos += 1;
                continue;
            }
            if c == b'\\' {
                result.push_str(&self.src[start..pos]);
                let (p2, esc) = self.parse_basic_str_escape(pos, multiline)?;
                result.push_str(&esc);
                pos = p2;
                start = pos;
                continue;
            }
            if error_on(c) {
                return Err(self.err(pos, &format!("Illegal character {}", self.char_repr(pos))));
            }
            pos += 1;
        }
    }

    fn parse_value(&self, pos: usize) -> R<(usize, Tv)> {
        let rest = &self.src[pos.min(self.src.len())..];
        match self.at(pos) {
            Some(b'"') => {
                return if self.starts(pos, "\"\"\"") {
                    self.parse_multiline_str(pos, false).map(|(p, s)| (p, Tv::Str(s)))
                } else {
                    self.parse_basic_str(pos + 1, false).map(|(p, s)| (p, Tv::Str(s)))
                };
            }
            Some(b'\'') => {
                return if self.starts(pos, "'''") {
                    self.parse_multiline_str(pos, true).map(|(p, s)| (p, Tv::Str(s)))
                } else {
                    self.parse_literal_str(pos).map(|(p, s)| (p, Tv::Str(s)))
                };
            }
            Some(b't') if rest.starts_with("true") => return Ok((pos + 4, Tv::Bool(true))),
            Some(b'f') if rest.starts_with("false") => return Ok((pos + 5, Tv::Bool(false))),
            Some(b'[') => return self.parse_array(pos).map(|(p, a)| (p, Tv::Arr(a))),
            Some(b'{') => return self.parse_inline_table(pos).map(|(p, t)| (p, Tv::Table(t))),
            _ => {}
        }
        if let Some((end, dt)) = match_datetime(rest.as_bytes()) {
            if !valid_date(&dt) {
                return Err(self.err(pos, "Invalid date or datetime"));
            }
            return Ok((pos + end, Tv::DateTime(dt)));
        }
        if let Some((end, dt)) = match_local_time(rest.as_bytes()) {
            return Ok((pos + end, Tv::DateTime(dt)));
        }
        if let Some((end, is_float)) = match_number(rest.as_bytes()) {
            let text: String = rest[..end].chars().filter(|c| *c != '_').collect();
            if is_float {
                // float() of the match; a value past the range is inf.
                return Ok((pos + end, Tv::Float(text.parse::<f64>().unwrap_or(f64::NAN))));
            }
            let digits = text.trim_start_matches(['+', '-']);
            if !digits.starts_with("0x") && !digits.starts_with("0o") && !digits.starts_with("0b") && digits.len() > 4300 {
                // int(text, 0) past Python's 4300-digit limit.
                return Err(TomlDecodeError(
                    format!(
                        "Exceeds the limit (4300 digits) for integer string conversion: value has {} digits; use sys.set_int_max_str_digits() to increase the limit",
                        digits.len()
                    ),
                    true,
                ));
            }
            return Ok((pos + end, parse_int(&text)));
        }
        for (s, v) in [("inf", f64::INFINITY), ("nan", f64::NAN)] {
            if rest.starts_with(s) {
                return Ok((pos + 3, Tv::Float(v)));
            }
        }
        for (s, v) in [("-inf", f64::NEG_INFINITY), ("+inf", f64::INFINITY), ("-nan", f64::NAN), ("+nan", f64::NAN)] {
            if rest.starts_with(s) {
                return Ok((pos + 4, Tv::Float(v)));
            }
        }
        Err(self.err(pos, "Invalid value"))
    }
}

/// `int(text, 0)`: an i64 when it fits, else the decimal text.
fn parse_int(text: &str) -> Tv {
    let neg = text.starts_with('-');
    let t = text.trim_start_matches(['+', '-']);
    let (base, digits) = match t.get(..2) {
        Some("0x") => (16, &t[2..]),
        Some("0o") => (8, &t[2..]),
        Some("0b") => (2, &t[2..]),
        _ => (10, t),
    };
    // Little-endian limbs of 10^9, so any size converts.
    let mut limbs: Vec<u64> = vec![0];
    for c in digits.chars() {
        let mut carry = c.to_digit(base).unwrap_or(0) as u64;
        for l in limbs.iter_mut() {
            let v = *l * base as u64 + carry;
            *l = v % 1_000_000_000;
            carry = v / 1_000_000_000;
        }
        while carry > 0 {
            limbs.push(carry % 1_000_000_000);
            carry /= 1_000_000_000;
        }
    }
    let mut s = limbs.last().copied().unwrap_or(0).to_string();
    for l in limbs.iter().rev().skip(1) {
        s.push_str(&format!("{l:09}"));
    }
    let s = if neg && s != "0" { format!("-{s}") } else { s };
    match s.parse::<i64>() {
        Ok(n) => Tv::Int(n),
        Err(_) => Tv::BigInt(s),
    }
}

fn digits(s: &[u8], i: usize, n: usize) -> Option<u32> {
    let d = s.get(i..i + n)?;
    if !d.iter().all(u8::is_ascii_digit) {
        return None;
    }
    std::str::from_utf8(d).ok()?.parse().ok()
}

/// `_TIME_RE_STR` at the start of s: the length matched.
fn match_time(s: &[u8], dt: &mut DateTime) -> Option<usize> {
    let h = digits(s, 0, 2)?;
    if s.get(2) != Some(&b':') {
        return None;
    }
    let m = digits(s, 3, 2)?;
    if s.get(5) != Some(&b':') {
        return None;
    }
    let sec = digits(s, 6, 2)?;
    if h > 23 || m > 59 || sec > 59 {
        return None;
    }
    dt.hour = h;
    dt.minute = m;
    dt.second = sec;
    let mut n = 8;
    if s.get(n) == Some(&b'.') && s.get(n + 1).is_some_and(u8::is_ascii_digit) {
        let mut k = n + 1;
        while s.get(k).is_some_and(u8::is_ascii_digit) {
            k += 1;
        }
        let frac = &s[n + 1..k.min(n + 7)];
        let mut text = String::from_utf8_lossy(frac).into_owned();
        while text.len() < 6 {
            text.push('0');
        }
        dt.us = text.parse().unwrap_or(0);
        n = k;
    }
    Some(n)
}

/// `RE_DATETIME.match`: a date, then optionally a time and an offset.
fn match_datetime(s: &[u8]) -> Option<(usize, DateTime)> {
    let year = digits(s, 0, 4)?;
    if s.get(4) != Some(&b'-') || s.get(7) != Some(&b'-') {
        return None;
    }
    let (month, day) = (digits(s, 5, 2)?, digits(s, 8, 2)?);
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return None;
    }
    let dt = DateTime { kind: "date", year: year as i32, month, day, hour: 0, minute: 0, second: 0, us: 0, offset: None };
    if matches!(s.get(10), Some(b'T' | b't' | b' ')) {
        let mut t = dt.clone();
        if let Some(k) = s.get(11..).and_then(|r| match_time(r, &mut t)) {
            t.kind = "datetime";
            let mut e = 11 + k;
            if matches!(s.get(e), Some(b'Z' | b'z')) {
                t.offset = Some(0);
                e += 1;
            } else if matches!(s.get(e), Some(b'+' | b'-')) && s.get(e + 3) == Some(&b':') {
                if let (Some(oh), Some(om)) = (digits(s, e + 1, 2), digits(s, e + 4, 2)) {
                    if oh <= 23 && om <= 59 {
                        let mins = (oh * 60 + om) as i32;
                        t.offset = Some(if s[e] == b'-' { -mins } else { mins });
                        e += 6;
                    }
                }
            }
            return Some((e, t));
        }
    }
    Some((10, dt))
}

fn match_local_time(s: &[u8]) -> Option<(usize, DateTime)> {
    let mut dt = DateTime { kind: "time", year: 0, month: 0, day: 0, hour: 0, minute: 0, second: 0, us: 0, offset: None };
    let n = match_time(s, &mut dt)?;
    Some((n, dt))
}

/// Whether datetime() takes the date: the day exists in the month.
fn valid_date(dt: &DateTime) -> bool {
    let leap = dt.year % 4 == 0 && (dt.year % 100 != 0 || dt.year % 400 == 0);
    let days = [31, if leap { 29 } else { 28 }, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][dt.month as usize - 1];
    dt.year >= 1 && dt.day <= days
}

/// `RE_NUMBER.match`: the length matched and whether the float part is
/// there.
fn match_number(s: &[u8]) -> Option<(usize, bool)> {
    let tail = |mut i: usize, digit: &dyn Fn(u8) -> bool| {
        // digit (_? digit)*
        while i < s.len() {
            if digit(s[i]) {
                i += 1;
            } else if s[i] == b'_' && s.get(i + 1).is_some_and(|c| digit(*c)) {
                i += 2;
            } else {
                break;
            }
        }
        i
    };
    if s.len() >= 3 && s[0] == b'0' {
        let digit: Option<&dyn Fn(u8) -> bool> = match s[1] {
            b'x' => Some(&|c: u8| c.is_ascii_hexdigit()),
            b'b' => Some(&|c: u8| c == b'0' || c == b'1'),
            b'o' => Some(&|c: u8| (b'0'..=b'7').contains(&c)),
            _ => None,
        };
        if let Some(d) = digit {
            if d(s[2]) {
                return Some((tail(3, d), false));
            }
        }
    }
    let dec = |c: u8| c.is_ascii_digit();
    let mut i = 0;
    if matches!(s.first(), Some(b'+' | b'-')) {
        i += 1;
    }
    match s.get(i) {
        Some(b'0') => i += 1,
        Some(c) if (b'1'..=b'9').contains(c) => i = tail(i + 1, &dec),
        _ => return None,
    }
    let float_start = i;
    if s.get(i) == Some(&b'.') && s.get(i + 1).is_some_and(u8::is_ascii_digit) {
        i = tail(i + 2, &dec);
    }
    if matches!(s.get(i), Some(b'e' | b'E')) {
        let mut j = i + 1;
        if matches!(s.get(j), Some(b'+' | b'-')) {
            j += 1;
        }
        if s.get(j).is_some_and(u8::is_ascii_digit) {
            i = tail(j + 1, &dec);
        }
    }
    Some((i, i > float_start))
}

/// `tomllib.loads(text)`.
pub fn parse(text: &str) -> Result<Table, TomlDecodeError> {
    let src = text.replace("\r\n", "\n");
    let p = P { src: &src, b: src.as_bytes() };
    let mut root = Table::default();
    let mut flags = Flags::default();
    let mut header: Vec<String> = vec![];
    let mut pos = 0;
    loop {
        pos = p.skip_ws(pos);
        let Some(c) = p.at(pos) else { break };
        if c == b'\n' {
            pos += 1;
            continue;
        }
        if is_bare(c) || c == b'"' || c == b'\'' {
            pos = p.key_value_rule(pos, &mut root, &mut flags, &header)?;
            pos = p.skip_ws(pos);
        } else if c == b'[' {
            flags.finalize_pending();
            let (p2, key) = if p.at(pos + 1) == Some(b'[') {
                p.create_list_rule(pos, &mut root, &mut flags)?
            } else {
                p.create_dict_rule(pos, &mut root, &mut flags)?
            };
            header = key;
            pos = p.skip_ws(p2);
        } else if c != b'#' {
            return Err(p.err(pos, "Invalid statement"));
        }
        pos = p.skip_comment(pos)?;
        match p.at(pos) {
            None => break,
            Some(b'\n') => pos += 1,
            Some(_) => return Err(p.err(pos, "Expected newline or end of document after a statement")),
        }
    }
    Ok(root)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::{dumps, float_repr, loads_py, Object, Value};

    fn typed(v: &Tv) -> Value {
        let one = |k: &str, v: Value| Value::Obj(Object::new().with(k, v));
        match v {
            Tv::Bool(b) => Value::Bool(*b),
            Tv::Int(n) => one("int", Value::Str(n.to_string())),
            Tv::BigInt(s) => one("int", Value::Str(s.clone())),
            Tv::Float(f) => one(
                "float",
                Value::Str(match float_repr(*f).as_str() {
                    "Infinity" => "inf".into(),
                    "-Infinity" => "-inf".into(),
                    "NaN" => "nan".into(),
                    r => r.to_string(),
                }),
            ),
            Tv::Str(s) => one("str", Value::Str(s.clone())),
            Tv::DateTime(d) => one("datetime", Value::Str(d.repr())),
            Tv::Arr(a) => one("array", Value::List(a.iter().map(typed).collect())),
            Tv::Table(t) => one(
                "table",
                Value::List(t.keys.iter().map(|k| Value::List(vec![Value::Str(k.clone()), typed(&t.vals[k])])).collect()),
            ),
        }
    }

    /// clients/fixtures/toml (clients/toml_cases.py): tomllib.loads on
    /// thousands of texts. The reader gives the same document, or a
    /// TOMLDecodeError where tomllib raises one.
    #[test]
    fn parse_agrees_with_tomllib() {
        let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/toml/cases.jsonl");
        let text = std::fs::read_to_string(path).expect("clients/fixtures/toml (clients/toml_cases.py)");
        let mut bad = vec![];
        let mut n = 0;
        for line in text.lines().filter(|l| !l.is_empty()) {
            let c = loads_py(line, 900).expect("a case line");
            let c = c.as_obj().expect("a case object");
            let t = c.value("text").as_str().expect("text");
            let want = dumps(c.value("expect"), true);
            let got = match parse(t) {
                Ok(doc) => dumps(&typed(&Tv::Table(doc)), true),
                Err(e) => {
                    let class = if e.is_value_error() { "ValueError" } else { "TOMLDecodeError" };
                    dumps(&Value::Obj(Object::new().with("exc", class).with("msg", e.0.as_str())), true)
                }
            };
            n += 1;
            if got != want {
                bad.push(format!("{t:?}:\n got  {got}\n want {want}"));
            }
        }
        assert!(n >= 3000, "only {n} cases");
        assert!(bad.is_empty(), "{} of {n} disagree:\n{}", bad.len(), bad[..bad.len().min(20)].join("\n"));
    }
}
