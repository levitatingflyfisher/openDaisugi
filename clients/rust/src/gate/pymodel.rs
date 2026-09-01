//! pydantic's lax validation (pydantic 2.13, pydantic-core 2.46) for the
//! field types the envelope and predicate models use, in JSON mode
//! (`model_validate_json`) and in Python mode (`validate_python` of what
//! JSON gave). Each validator records pydantic's error at its location and
//! returns `None`; a model keeps validating its other fields, as pydantic
//! does, so every error is reported in pydantic's order.

use super::py::text::is_space;
use super::pydantic::LineErr;
use super::pyjson::{py_repr, py_type_name, Object, Value};
use super::R;

/// An input to validate: a JSON value as jiter parsed it (an object keeps
/// every key, repeats included) or a Python value.
#[derive(Debug, Clone, PartialEq)]
pub enum In {
    Null,
    Bool(bool),
    /// Normalized decimal text, and whether it fits an i64 (jiter's Int;
    /// otherwise its BigInt).
    Int(String, bool),
    Float(f64),
    Str(String),
    List(Vec<In>),
    Dict(Vec<(String, In)>),
}

fn fits_i64(t: &str) -> bool {
    t.parse::<i64>().is_ok()
}

impl In {
    /// A JSON value as jiter parsed it.
    pub fn from_json(v: &jiter::JsonValue) -> In {
        use jiter::JsonValue as J;
        match v {
            J::Null => In::Null,
            J::Bool(b) => In::Bool(*b),
            J::Int(i) => In::Int(i.to_string(), true),
            J::BigInt(b) => {
                let t = b.to_string();
                let f = fits_i64(&t);
                In::Int(t, f)
            }
            J::Float(f) => In::Float(*f),
            J::Str(s) => In::Str(s.to_string()),
            J::Array(a) => In::List(a.iter().map(In::from_json).collect()),
            J::Object(o) => In::Dict(o.iter().map(|(k, v)| (k.to_string(), In::from_json(v))).collect()),
        }
    }

    /// A Python value as `json.loads` or pydantic's `Any` gave it.
    pub fn from_value(v: &Value) -> In {
        match v {
            Value::Null => In::Null,
            Value::Bool(b) => In::Bool(*b),
            Value::Int(t) => In::Int(t.clone(), fits_i64(t)),
            Value::Float(f) => In::Float(*f),
            Value::Str(s) => In::Str(s.clone()),
            Value::List(l) | Value::Tuple(l) => In::List(l.iter().map(In::from_value).collect()),
            Value::Obj(o) => In::Dict(o.keys().iter().map(|k| (k.clone(), In::from_value(o.value(k)))).collect()),
        }
    }

    /// The Python value: a repeated key keeps its first place and its last
    /// value, as a dict built from the pairs does.
    pub fn to_value(&self) -> Value {
        match self {
            In::Null => Value::Null,
            In::Bool(b) => Value::Bool(*b),
            In::Int(t, _) => Value::Int(t.clone()),
            In::Float(f) => Value::Float(*f),
            In::Str(s) => Value::Str(s.clone()),
            In::List(l) => Value::List(l.iter().map(In::to_value).collect()),
            In::Dict(d) => {
                let mut o = Object::new();
                for (k, v) in d {
                    o.set(k, v.to_value());
                }
                Value::Obj(o)
            }
        }
    }

    /// A field of an object: the last value given for the key.
    pub fn get(&self, k: &str) -> Option<&In> {
        match self {
            In::Dict(d) => d.iter().rev().find(|(kk, _)| kk == k).map(|(_, v)| v),
            _ => None,
        }
    }
}

/// Where a validator is, and every error so far.
pub struct Val {
    /// JSON mode (`model_validate_json`) or Python mode.
    pub json: bool,
    pub errs: Vec<LineErr>,
}

pub type Loc = Vec<String>;

pub fn at(loc: &Loc, part: impl Into<String>) -> Loc {
    let mut l = loc.clone();
    l.push(part.into());
    l
}

impl Val {
    pub fn new(json: bool) -> Val {
        Val { json, errs: Vec::new() }
    }

    /// Record an error about `input`.
    pub fn err(&mut self, loc: &Loc, msg: impl Into<String>, typ: &str, input: &In) -> R<()> {
        let v = input.to_value();
        self.errs.push(LineErr {
            loc: loc.clone(),
            msg: msg.into(),
            typ: typ.to_string(),
            input: Some((py_repr(&v)?, py_type_name(&v).to_string())),
        });
        Ok(())
    }

    pub fn string(&mut self, loc: &Loc, x: &In) -> R<Option<String>> {
        match x {
            In::Str(s) => Ok(Some(s.clone())),
            _ => {
                self.err(loc, "Input should be a valid string", "string_type", x)?;
                Ok(None)
            }
        }
    }

    /// A `str` with `max_length`, counted in code points.
    pub fn string_max(&mut self, loc: &Loc, x: &In, max: usize) -> R<Option<String>> {
        let s = match self.string(loc, x)? {
            None => return Ok(None),
            Some(s) => s,
        };
        if s.chars().count() > max {
            self.err(loc, format!("String should have at most {max} characters"), "string_too_long", x)?;
            return Ok(None);
        }
        Ok(Some(s))
    }

    /// An `int`: its decimal text.
    pub fn int(&mut self, loc: &Loc, x: &In) -> R<Option<String>> {
        let r = match x {
            In::Int(t, _) => Ok(t.clone()),
            In::Bool(b) => Ok(if *b { "1" } else { "0" }.to_string()),
            In::Float(f) => float_to_int(*f),
            In::Str(s) => str_as_int(s),
            _ => Err(("Input should be a valid integer", "int_type")),
        };
        match r {
            Ok(t) => Ok(Some(t)),
            Err((msg, typ)) => {
                self.err(loc, msg, typ, x)?;
                Ok(None)
            }
        }
    }

    /// A `float`.
    pub fn float(&mut self, loc: &Loc, x: &In) -> R<Option<f64>> {
        let bad_type = ("Input should be a valid number", "float_type");
        let r = match x {
            In::Float(f) => Ok(*f),
            In::Bool(b) => Ok(if *b { 1.0 } else { 0.0 }),
            In::Int(t, _) => {
                // Both round to nearest; in Python mode an int too large
                // for a float is refused, in JSON mode it becomes inf.
                let f: f64 = t.parse().unwrap_or(f64::INFINITY);
                if !self.json && f.is_infinite() {
                    Err(bad_type)
                } else {
                    Ok(f)
                }
            }
            In::Str(s) => str_as_float(s),
            _ => Err(bad_type),
        };
        match r {
            Ok(f) => Ok(Some(f)),
            Err((msg, typ)) => {
                self.err(loc, msg, typ, x)?;
                Ok(None)
            }
        }
    }

    /// A `bool`.
    pub fn boolean(&mut self, loc: &Loc, x: &In) -> R<Option<bool>> {
        let parsing = ("Input should be a valid boolean, unable to interpret input", "bool_parsing");
        let bad_type = ("Input should be a valid boolean", "bool_type");
        let r = match x {
            In::Bool(b) => Ok(*b),
            In::Int(t, true) => match t.as_str() {
                "0" => Ok(false),
                "1" => Ok(true),
                _ => Err(parsing),
            },
            In::Int(_, false) => Err(bad_type),
            In::Float(f) => {
                if *f == 0.0 {
                    Ok(false)
                } else if *f == 1.0 {
                    Ok(true)
                } else {
                    Err(bad_type)
                }
            }
            In::Str(s) => match s.to_lowercase().as_str() {
                "0" | "off" | "f" | "false" | "n" | "no" => Ok(false),
                "1" | "on" | "t" | "true" | "y" | "yes" => Ok(true),
                _ => Err(parsing),
            },
            _ => Err(bad_type),
        };
        match r {
            Ok(b) => Ok(Some(b)),
            Err((msg, typ)) => {
                self.err(loc, msg, typ, x)?;
                Ok(None)
            }
        }
    }

    /// A `list[...]`: every item is validated.
    pub fn list<T>(
        &mut self,
        loc: &Loc,
        x: &In,
        mut item: impl FnMut(&mut Val, &Loc, &In) -> R<Option<T>>,
    ) -> R<Option<Vec<T>>> {
        let items = match x {
            In::List(l) => l,
            _ => {
                let msg = if self.json { "Input should be a valid array" } else { "Input should be a valid list" };
                self.err(loc, msg, "list_type", x)?;
                return Ok(None);
            }
        };
        let mut out = Vec::with_capacity(items.len());
        let mut ok = true;
        for (i, e) in items.iter().enumerate() {
            match item(self, &at(loc, i.to_string()), e)? {
                Some(v) => out.push(v),
                None => ok = false,
            }
        }
        Ok(if ok { Some(out) } else { None })
    }

    /// A fixed-size `tuple[...]`: a longer input is one `too_long` error;
    /// a shorter one is missing its last items.
    pub fn tuple<T>(
        &mut self,
        loc: &Loc,
        x: &In,
        n: usize,
        mut item: impl FnMut(&mut Val, &Loc, &In) -> R<Option<T>>,
    ) -> R<Option<Vec<T>>> {
        let items = match x {
            In::List(l) => l,
            _ => {
                self.err(loc, "Input should be a valid array", "tuple_type", x)?;
                return Ok(None);
            }
        };
        if items.len() > n {
            self.err(
                loc,
                format!("Tuple should have at most {n} items after validation, not {}", items.len()),
                "too_long",
                x,
            )?;
            return Ok(None);
        }
        let mut out = Vec::with_capacity(n);
        let mut ok = true;
        for i in 0..n {
            let l = at(loc, i.to_string());
            match items.get(i) {
                Some(e) => match item(self, &l, e)? {
                    Some(v) => out.push(v),
                    None => ok = false,
                },
                None => {
                    self.err(&l, "Field required", "missing", x)?;
                    ok = false;
                }
            }
        }
        Ok(if ok { Some(out) } else { None })
    }

    /// A `dict[str, ...]`: every pair, repeats included, is validated; a
    /// repeated key keeps its last value.
    pub fn dict<T>(
        &mut self,
        loc: &Loc,
        x: &In,
        mut item: impl FnMut(&mut Val, &Loc, &In) -> R<Option<T>>,
    ) -> R<Option<Vec<(String, T)>>> {
        let pairs = match x {
            In::Dict(d) => d,
            _ => {
                let msg = if self.json { "Input should be an object" } else { "Input should be a valid dictionary" };
                self.err(loc, msg, "dict_type", x)?;
                return Ok(None);
            }
        };
        let mut out: Vec<(String, T)> = Vec::new();
        let mut ok = true;
        for (k, v) in pairs {
            match item(self, &at(loc, k.clone()), v)? {
                Some(t) => match out.iter_mut().find(|(kk, _)| kk == k) {
                    Some(slot) => slot.1 = t,
                    None => out.push((k.clone(), t)),
                },
                None => ok = false,
            }
        }
        Ok(if ok { Some(out) } else { None })
    }

    /// A `Literal[...]` of strings.
    pub fn literal(&mut self, loc: &Loc, x: &In, options: &[&str]) -> R<Option<String>> {
        if let In::Str(s) = x {
            if options.contains(&s.as_str()) {
                return Ok(Some(s.clone()));
            }
        }
        let quoted: Vec<String> = options.iter().map(|o| format!("'{o}'")).collect();
        let listed = match quoted.len() {
            1 => quoted[0].clone(),
            n => format!("{} or {}", quoted[..n - 1].join(", "), quoted[n - 1]),
        };
        self.err(loc, format!("Input should be {listed}"), "literal_error", x)?;
        Ok(None)
    }

    /// `X | None`.
    pub fn nullable<T>(
        &mut self,
        loc: &Loc,
        x: &In,
        inner: impl FnOnce(&mut Val, &Loc, &In) -> R<Option<T>>,
    ) -> R<Option<Option<T>>> {
        if matches!(x, In::Null) {
            return Ok(Some(None));
        }
        Ok(inner(self, loc, x)?.map(Some))
    }

    /// A model given a non-object.
    pub fn model_type(&mut self, loc: &Loc, x: &In) -> R<()> {
        self.err(loc, "Input should be an object", "model_type", x)
    }

    /// A required field absent from `model`.
    pub fn missing(&mut self, loc: &Loc, model: &In) -> R<()> {
        self.err(loc, "Field required", "missing", model)
    }
}

pub type Refusal = (&'static str, &'static str);

pub fn float_to_int(f: f64) -> Result<String, Refusal> {
    if !f.is_finite() {
        return Err(("Input should be a finite number", "finite_number"));
    }
    if f.fract() != 0.0 {
        return Err(("Input should be a valid integer, got a number with a fractional part", "int_from_float"));
    }
    // Measured: +-2**63 and beyond are refused.
    if f >= 9.223_372_036_854_776e18 || f <= -9.223_372_036_854_776e18 {
        return Err(("Unable to parse input string as an integer, exceeded maximum size", "int_parsing_size"));
    }
    Ok((f as i64).to_string())
}

/// pydantic-core's `str_as_int`, as measured: trimmed, a `.` followed by
/// zeros only is dropped, then a sign and ASCII digits with single
/// underscores between digits.
pub fn str_as_int(s: &str) -> Result<String, Refusal> {
    let parsing = ("Input should be a valid integer, unable to parse string as an integer", "int_parsing");
    let t = s.trim();
    if t.len() > 4300 {
        return Err(("Unable to parse input string as an integer, exceeded maximum size", "int_parsing_size"));
    }
    let t = match t.split_once('.') {
        Some((before, after)) => {
            if after.is_empty() || !after.chars().all(|c| c == '0') {
                return Err(parsing);
            }
            before
        }
        None => t,
    };
    let (neg, digits) = match t.as_bytes().first() {
        Some(b'-') => (true, &t[1..]),
        Some(b'+') => (false, &t[1..]),
        _ => (false, t),
    };
    let b = digits.as_bytes();
    if b.is_empty() || !b[0].is_ascii_digit() || !b[b.len() - 1].is_ascii_digit() {
        return Err(parsing);
    }
    let mut clean = String::with_capacity(b.len());
    for (i, &c) in b.iter().enumerate() {
        if c == b'_' {
            if !b[i - 1].is_ascii_digit() || !b[i + 1].is_ascii_digit() {
                return Err(parsing);
            }
        } else if c.is_ascii_digit() {
            clean.push(c as char);
        } else {
            return Err(parsing);
        }
    }
    let clean = clean.trim_start_matches('0');
    Ok(if clean.is_empty() {
        "0".into()
    } else if neg {
        format!("-{clean}")
    } else {
        clean.to_string()
    })
}

/// pydantic-core's `str_as_float`: Rust's float syntax on the trimmed
/// text, else on the untrimmed text with its underscores removed (none at
/// either end, none doubled).
pub fn str_as_float(s: &str) -> Result<f64, Refusal> {
    let parsing = ("Input should be a valid number, unable to parse string as a number", "float_parsing");
    if let Ok(f) = s.trim().parse::<f64>() {
        return Ok(f);
    }
    if s.starts_with('_') || s.ends_with('_') || s.contains("__") {
        return Err(parsing);
    }
    s.replace('_', "").parse::<f64>().map_err(|_| parsing)
}

/// Python's universal newlines, as `read_text` applies them: `\r\n` and a
/// lone `\r` become `\n`.
pub fn universal_newlines(s: &str) -> String {
    if !s.contains('\r') {
        return s.to_string();
    }
    let mut out = String::with_capacity(s.len());
    let mut it = s.chars().peekable();
    while let Some(c) = it.next() {
        if c == '\r' {
            if it.peek() == Some(&'\n') {
                it.next();
            }
            out.push('\n');
        } else {
            out.push(c);
        }
    }
    out
}

/// `bytes.decode("utf-8")` strict: the text, or the message of the
/// UnicodeDecodeError Python raises.
pub fn decode_utf8_strict(b: &[u8]) -> Result<String, String> {
    match std::str::from_utf8(b) {
        Ok(s) => Ok(s.to_string()),
        Err(e) => {
            let start = e.valid_up_to();
            let (end, reason) = match e.error_len() {
                None => (b.len(), "unexpected end of data"),
                Some(n) => {
                    let lead = b[start];
                    let bad_start = (0x80..=0xC1).contains(&lead) || lead >= 0xF5;
                    (start + n, if bad_start { "invalid start byte" } else { "invalid continuation byte" })
                }
            };
            Err(if end - start == 1 {
                format!("'utf-8' codec can't decode byte 0x{:02x} in position {start}: {reason}", b[start])
            } else {
                format!("'utf-8' codec can't decode bytes in position {start}-{}: {reason}", end - 1)
            })
        }
    }
}

/// The number of bytes `io.TextIOWrapper` asks for at a time.
const TEXT_CHUNK: usize = 8192;

/// The maximal subpart of a UTF-8 sequence at `b[i]`: its length, whether
/// it is a whole code point, and whether its first byte can start none.
fn utf8_subpart(b: &[u8], i: usize) -> (usize, bool, bool) {
    let c = b[i];
    let (need, lo, hi) = match c {
        0x00..=0x7F => return (1, true, false),
        0xC2..=0xDF => (1, 0x80, 0xBF),
        0xE0 => (2, 0xA0, 0xBF),
        0xE1..=0xEC | 0xEE | 0xEF => (2, 0x80, 0xBF),
        0xED => (2, 0x80, 0x9F),
        0xF0 => (3, 0x90, 0xBF),
        0xF1..=0xF3 => (3, 0x80, 0xBF),
        0xF4 => (3, 0x80, 0x8F),
        _ => return (1, false, true),
    };
    let mut j = i + 1;
    for k in 0..need {
        if j >= b.len() {
            return (j - i, false, false);
        }
        let (l, h) = if k == 0 { (lo, hi) } else { (0x80, 0xBF) };
        if b[j] < l || b[j] > h {
            return (j - i, false, false);
        }
        j += 1;
    }
    (j - i, true, false)
}

/// The part of a text a line reader hands out before it needs more: every
/// line that ends in LF, CR or CRLF, less a CR at the very end.
fn whole_lines(text: &str) -> String {
    let t = text.strip_suffix('\r').unwrap_or(text);
    match t.rfind(['\r', '\n']) {
        Some(i) => t[..=i].to_string(),
        None => String::new(),
    }
}

/// A file opened with `open(path, encoding="utf-8")` and read line by
/// line. Bytes that are all UTF-8 give the whole text and no error.
///
/// Otherwise the reader decodes the file 8192 bytes at a time, and each
/// decode call sees the bytes of an incomplete code point it held back from
/// the call before, then the new chunk. The first call that meets bytes
/// that are not UTF-8 raises UnicodeDecodeError, and the position in its
/// text counts from the start of the bytes that call saw. The text returned
/// with the error is what the reader gave out before it: the whole lines of
/// the text decoded by the calls before (a CR at its end is held back, as
/// it may be half of a CRLF).
pub fn decode_stream(b: &[u8]) -> (String, Option<String>) {
    if let Ok(s) = std::str::from_utf8(b) {
        return (s.to_string(), None);
    }
    let mut done = 0; // the bytes decoded by the calls before
    let mut start = 0;
    loop {
        let end = (start + TEXT_CHUNK).min(b.len());
        let last = start >= b.len();
        let buf = &b[done..end];
        let mut i = 0;
        while i < buf.len() {
            let (n, complete, bad_start) = utf8_subpart(buf, i);
            if complete {
                i += n;
                continue;
            }
            let short = !bad_start && i + n >= buf.len();
            if short && !last {
                break; // held back for the next call
            }
            let reason = if bad_start {
                "invalid start byte"
            } else if short {
                "unexpected end of data"
            } else {
                "invalid continuation byte"
            };
            let msg = if n == 1 {
                format!("'utf-8' codec can't decode byte 0x{:02x} in position {i}: {reason}", buf[i])
            } else {
                format!("'utf-8' codec can't decode bytes in position {i}-{}: {reason}", i + n - 1)
            };
            // The bytes before `done` decoded whole, so they are UTF-8.
            let before = std::str::from_utf8(&b[..done]).unwrap_or("");
            return (whole_lines(before), Some(msg));
        }
        done += i;
        if last {
            // Every byte decoded: from_utf8 says this cannot be.
            return (String::from_utf8_lossy(b).into_owned(), None);
        }
        start += TEXT_CHUNK;
    }
}

/// Whether Python's `str.strip()` would remove `c` (for callers that need
/// Python's whitespace rather than Rust's).
pub fn py_space(c: char) -> bool {
    is_space(c)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn int_strings_read_as_pydantic_reads_them() {
        for (s, want) in [
            ("1_000", Some("1000")),
            ("0030", Some("30")),
            ("-0", Some("0")),
            ("1.00", Some("1")),
            ("\t7\n", Some("7")),
            ("+1_0.0", Some("10")),
            ("-00.00", Some("0")),
            ("1_0.00", Some("10")),
            ("-_1", None),
            ("1_.0", None),
            ("1.", None),
            (".0", None),
            ("1.0_0", None),
            ("1__0", None),
            ("1e3", None),
            ("١٢", None),
            ("+-1", None),
        ] {
            assert_eq!(str_as_int(s).ok().as_deref(), want, "{s:?}");
        }
    }

    #[test]
    fn float_strings_read_as_pydantic_reads_them() {
        for (s, want) in [
            ("1_0.5", Some(10.5)),
            ("-_1", Some(-1.0)),
            ("1_.0", Some(1.0)),
            ("\u{2003}7.5", Some(7.5)),
            (" 1_0", None),
            ("_1.0", None),
            ("1.00_", None),
            ("0x10", None),
            (".", None),
            ("infin", None),
        ] {
            assert_eq!(str_as_float(s).ok(), want, "{s:?}");
        }
        assert!(str_as_float("-nan").unwrap().is_nan());
        assert_eq!(str_as_float("+Infinity").unwrap(), f64::INFINITY);
    }

    #[test]
    fn decode_errors_read_as_pythons() {
        assert_eq!(
            decode_utf8_strict(b"ab\xff").unwrap_err(),
            "'utf-8' codec can't decode byte 0xff in position 2: invalid start byte"
        );
        assert_eq!(
            decode_utf8_strict(b"a\xe2\x82").unwrap_err(),
            "'utf-8' codec can't decode bytes in position 1-2: unexpected end of data"
        );
        assert_eq!(
            decode_utf8_strict(b"\xe2\x82x").unwrap_err(),
            "'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"
        );
    }

    /// testdata/stream.json of the Go client (its gen_stream.py writes it
    /// from the oracle's Python): for each input, the lines Python hands
    /// out before its UnicodeDecodeError, and the error's text.
    #[test]
    fn a_file_decodes_as_pythons_text_reader_decodes_it() {
        let p = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../go/internal/pystr/testdata/stream.json");
        let cases: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(p).unwrap()).unwrap();
        let cases = cases.as_array().unwrap();
        assert!(cases.len() >= 300, "only {} cases", cases.len());
        for (i, c) in cases.iter().enumerate() {
            let mut data = vec![];
            for part in c["parts"].as_array().unwrap() {
                let hex = part[0].as_str().unwrap();
                let b: Vec<u8> =
                    (0..hex.len()).step_by(2).map(|k| u8::from_str_radix(&hex[k..k + 2], 16).unwrap()).collect();
                for _ in 0..part[1].as_u64().unwrap() {
                    data.extend_from_slice(&b);
                }
            }
            let (text, err) = decode_stream(&data);
            // Python hands the lines out with universal newlines.
            let text = universal_newlines(&text);
            assert_eq!(
                crate::gate::sha256::hexdigest(text.as_bytes()),
                c["lines_sha256"].as_str().unwrap(),
                "case {i}: {} bytes of lines, Python {}",
                text.len(),
                c["lines_len"]
            );
            assert_eq!(err.as_deref(), c["error"].as_str(), "case {i}");
        }
    }
}
