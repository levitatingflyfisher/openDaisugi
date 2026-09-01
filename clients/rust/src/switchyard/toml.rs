//! The part of TOML a Switchyard deployment file uses: comments, `[table]`
//! and `[a.b]` headers, bare, quoted and dotted keys, and values that are
//! basic or literal strings, integers, floats, booleans, one-line arrays
//! and inline tables. Anything else (multi-line strings, dates, arrays of
//! tables) is refused, never guessed at: `NotRead` says the file is outside
//! this reader.

use std::collections::{HashMap, HashSet};

/// A file this reader does not read.
#[derive(Debug, Clone, PartialEq)]
pub struct NotRead(pub String);

impl std::fmt::Display for NotRead {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "not TOML this binary reads: {}", self.0)
    }
}

/// A TOML value.
#[derive(Debug, Clone, PartialEq)]
pub enum Tv {
    Str(String),
    Int(i64),
    Float(f64),
    Bool(bool),
    Arr(Vec<Tv>),
    Table(Table),
}

/// A TOML table: keys in file order.
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

    fn set(&mut self, k: &str, v: Tv) -> Result<(), NotRead> {
        if self.vals.contains_key(k) {
            return Err(NotRead(format!("key {k:?} is defined twice")));
        }
        self.keys.push(k.to_string());
        self.vals.insert(k.to_string(), v);
        Ok(())
    }

    /// The table under k, made when absent.
    fn sub(&mut self, k: &str) -> Result<&mut Table, NotRead> {
        if !self.vals.contains_key(k) {
            self.keys.push(k.to_string());
            self.vals.insert(k.to_string(), Tv::Table(Table::default()));
        }
        match self.vals.get_mut(k) {
            Some(Tv::Table(t)) => Ok(t),
            _ => Err(NotRead(format!("{k:?} is not a table"))),
        }
    }

    fn walk(&mut self, keys: &[String]) -> Result<&mut Table, NotRead> {
        let mut t = self;
        for k in keys {
            t = t.sub(k)?;
        }
        Ok(t)
    }
}

/// Reads text as `tomllib.loads` does, within this reader's part of TOML.
pub fn parse(text: &str) -> Result<Table, NotRead> {
    let mut root = Table::default();
    let mut cur: Vec<String> = vec![];
    let mut declared: HashSet<Vec<String>> = HashSet::new();
    let text = text.replace("\r\n", "\n");
    for (n, raw) in text.split('\n').enumerate() {
        let mut p = P { s: raw.as_bytes(), i: 0, line: n + 1 };
        p.ws();
        if p.done() || p.peek() == b'#' {
            continue;
        }
        if p.peek() == b'[' {
            p.i += 1;
            if p.peek() == b'[' {
                return Err(p.err("arrays of tables"));
            }
            let keys = p.key()?;
            p.ws();
            if p.peek() != b']' {
                return Err(p.err("a table header"));
            }
            p.i += 1;
            p.end()?;
            if !declared.insert(keys.clone()) {
                return Err(p.err("a table declared twice"));
            }
            root.walk(&keys)?;
            cur = keys;
            continue;
        }
        let keys = p.key()?;
        p.ws();
        if p.peek() != b'=' {
            return Err(p.err("a key = value line"));
        }
        p.i += 1;
        p.ws();
        let v = p.value(0)?;
        p.end()?;
        let t = root.walk(&cur)?;
        let t = t.walk(&keys[..keys.len() - 1])?;
        t.set(&keys[keys.len() - 1], v)?;
    }
    Ok(root)
}

struct P<'a> {
    s: &'a [u8],
    i: usize,
    line: usize,
}

fn is_bare(c: u8) -> bool {
    c.is_ascii_alphanumeric() || c == b'_' || c == b'-'
}

fn is_ctl(c: u8) -> bool {
    (c < 0x20 && c != b'\t') || c == 0x7f
}

impl P<'_> {
    fn err(&self, what: &str) -> NotRead {
        NotRead(format!("line {} holds {what} this reader does not read", self.line))
    }

    fn done(&self) -> bool {
        self.i >= self.s.len()
    }

    fn peek(&self) -> u8 {
        if self.done() {
            0
        } else {
            self.s[self.i]
        }
    }

    fn ws(&mut self) {
        while !self.done() && (self.s[self.i] == b' ' || self.s[self.i] == b'\t') {
            self.i += 1;
        }
    }

    fn end(&mut self) -> Result<(), NotRead> {
        self.ws();
        if self.done() || self.peek() == b'#' {
            return Ok(());
        }
        Err(self.err("text after a value"))
    }

    fn rest_starts(&self, p: &[u8]) -> bool {
        self.s[self.i..].starts_with(p)
    }

    fn key(&mut self) -> Result<Vec<String>, NotRead> {
        let mut keys = vec![];
        loop {
            self.ws();
            let c = self.peek();
            if c == b'"' {
                keys.push(self.basic()?);
            } else if c == b'\'' {
                keys.push(self.literal()?);
            } else if is_bare(c) {
                let st = self.i;
                while !self.done() && is_bare(self.peek()) {
                    self.i += 1;
                }
                keys.push(String::from_utf8_lossy(&self.s[st..self.i]).into_owned());
            } else {
                return Err(self.err("a key"));
            }
            self.ws();
            if self.peek() != b'.' {
                return Ok(keys);
            }
            self.i += 1;
        }
    }

    fn basic(&mut self) -> Result<String, NotRead> {
        if self.rest_starts(b"\"\"\"") {
            return Err(self.err("a multi-line string"));
        }
        self.i += 1;
        let mut b: Vec<u8> = vec![];
        loop {
            if self.done() {
                return Err(self.err("an unclosed string"));
            }
            let c = self.s[self.i];
            if c == b'"' {
                self.i += 1;
                return String::from_utf8(b).map_err(|_| self.err("a string that is not UTF-8"));
            } else if c == b'\\' {
                self.i += 1;
                if self.done() {
                    return Err(self.err("an unclosed string"));
                }
                let e = self.s[self.i];
                self.i += 1;
                match e {
                    b'b' => b.push(8),
                    b't' => b.push(b'\t'),
                    b'n' => b.push(b'\n'),
                    b'f' => b.push(12),
                    b'r' => b.push(b'\r'),
                    b'"' => b.push(b'"'),
                    b'\\' => b.push(b'\\'),
                    b'u' | b'U' => {
                        let n = if e == b'U' { 8 } else { 4 };
                        if self.i + n > self.s.len() {
                            return Err(self.err("a bad escape"));
                        }
                        let hex = std::str::from_utf8(&self.s[self.i..self.i + n]).map_err(|_| self.err("a bad escape"))?;
                        let ch = u32::from_str_radix(hex, 16)
                            .ok()
                            .filter(|_| hex.bytes().all(|x| x.is_ascii_hexdigit()))
                            .and_then(char::from_u32)
                            .ok_or_else(|| self.err("a bad escape"))?;
                        let mut buf = [0u8; 4];
                        b.extend_from_slice(ch.encode_utf8(&mut buf).as_bytes());
                        self.i += n;
                    }
                    _ => return Err(self.err("a bad escape")),
                }
            } else if is_ctl(c) {
                return Err(self.err("a control character in a string"));
            } else {
                b.push(c);
                self.i += 1;
            }
        }
    }

    fn literal(&mut self) -> Result<String, NotRead> {
        if self.rest_starts(b"'''") {
            return Err(self.err("a multi-line string"));
        }
        self.i += 1;
        let st = self.i;
        while !self.done() && self.peek() != b'\'' {
            if is_ctl(self.peek()) {
                return Err(self.err("a control character in a string"));
            }
            self.i += 1;
        }
        if self.done() {
            return Err(self.err("an unclosed string"));
        }
        let s = String::from_utf8(self.s[st..self.i].to_vec()).map_err(|_| self.err("a string that is not UTF-8"))?;
        self.i += 1;
        Ok(s)
    }

    fn value(&mut self, depth: usize) -> Result<Tv, NotRead> {
        if depth > 32 {
            return Err(self.err("nesting"));
        }
        match self.peek() {
            b'"' => return self.basic().map(Tv::Str),
            b'\'' => return self.literal().map(Tv::Str),
            b'[' => {
                self.i += 1;
                let mut out = vec![];
                loop {
                    self.ws();
                    if self.peek() == b']' {
                        self.i += 1;
                        return Ok(Tv::Arr(out));
                    }
                    out.push(self.value(depth + 1)?);
                    self.ws();
                    if self.peek() == b',' {
                        self.i += 1;
                        continue;
                    }
                    if self.peek() == b']' {
                        self.i += 1;
                        return Ok(Tv::Arr(out));
                    }
                    return Err(self.err("an array"));
                }
            }
            b'{' => {
                self.i += 1;
                let mut t = Table::default();
                self.ws();
                if self.peek() == b'}' {
                    self.i += 1;
                    return Ok(Tv::Table(t));
                }
                loop {
                    let keys = self.key()?;
                    self.ws();
                    if self.peek() != b'=' {
                        return Err(self.err("an inline table"));
                    }
                    self.i += 1;
                    self.ws();
                    let v = self.value(depth + 1)?;
                    t.walk(&keys[..keys.len() - 1])?.set(&keys[keys.len() - 1], v)?;
                    self.ws();
                    if self.peek() == b',' {
                        self.i += 1;
                        continue;
                    }
                    if self.peek() == b'}' {
                        self.i += 1;
                        return Ok(Tv::Table(t));
                    }
                    return Err(self.err("an inline table"));
                }
            }
            _ => {}
        }
        let st = self.i;
        while !self.done() && !b" \t,]}#".contains(&self.peek()) {
            self.i += 1;
        }
        let word = String::from_utf8_lossy(&self.s[st..self.i]).into_owned();
        match word.as_str() {
            "true" => return Ok(Tv::Bool(true)),
            "false" => return Ok(Tv::Bool(false)),
            _ => {}
        }
        let clean = word.replace('_', "");
        if !clean.is_empty() && !word.contains("__") && !word.starts_with('_') && !word.ends_with('_') {
            let digits = clean.trim_start_matches(['+', '-']);
            if !digits.is_empty()
                && digits.bytes().all(|c| c.is_ascii_digit())
                && (digits.len() == 1 || !digits.starts_with('0'))
            {
                if let Ok(n) = clean.parse::<i64>() {
                    return Ok(Tv::Int(n));
                }
            }
            if digits.contains(['.', 'e', 'E'])
                && digits.bytes().all(|c| c.is_ascii_digit() || b".eE+-".contains(&c))
                && digits.as_bytes()[0].is_ascii_digit()
            {
                if let Ok(f) = clean.parse::<f64>() {
                    return Ok(Tv::Float(f));
                }
            }
        }
        Err(self.err("a value"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_a_deployment_file() {
        let doc = parse(
            "schema_version = 1\n\n[llm_clients.capable] # c\nformat = \"anthropic_messages\"\nforward_auth = true\n\n\
             [targets.\"a b\"]\nid = 'x\\y'\nn = 1_000\nf = 0.5\nl = [1, \"s\", ]\ni = { k = \"v\", m.n = 2 }\n\
             [routes.r]\nid = \"daisugi\"\ncapable_target = \"a b\"\n",
        )
        .unwrap();
        let ab = doc.table("targets").unwrap().table("a b").unwrap();
        assert_eq!(ab.str("id"), Some("x\\y"));
        assert_eq!(ab.get("n"), Some(&Tv::Int(1000)));
        assert_eq!(ab.get("f"), Some(&Tv::Float(0.5)));
        let cap = doc.table("llm_clients").unwrap().table("capable").unwrap();
        assert_eq!(cap.get("forward_auth"), Some(&Tv::Bool(true)));
    }

    #[test]
    fn refuses_what_it_does_not_read() {
        for text in [
            "a = \"\"\"x\"\"\"\n",
            "[[t]]\n",
            "a = 1979-05-27\n",
            "a = 1\na = 2\n",
            "[t]\n[t]\n",
            "a = \"\\q\"\n",
            "a = 0x10\n",
            "a = 01\n",
            "[routes\n",
        ] {
            assert!(parse(text).is_err(), "{text:?}");
        }
    }
}
