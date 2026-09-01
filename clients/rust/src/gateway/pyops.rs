//! Python operations on decoded JSON, as the oracle's gateway meets them.
//!
//! The request bodies the gateway routes are any JSON a client sends, and
//! the oracle reads them with Python's duck typing: a body whose shape
//! makes a Python operation raise is forwarded untouched. The helpers here
//! model those operations on `pyjson::Value` and report a raise as
//! `Raise::Raise`. The Go client's `gateway/pyops.go` is the reference.

use num_bigint::BigInt;
use num_traits::{FromPrimitive, Signed, ToPrimitive, Zero};

use crate::gate::py::text::{self, as_surrogate, is_decimal, surrogate_char};
use crate::gate::pyjson::{self, loads, py_decode_error, LoadError, Object, PyDecodeFail, Value};

/// A Python exception the oracle meets.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Raise {
    /// An exception the oracle catches and does not print.
    Raise,
    /// RecursionError: the oracle does not catch it where it catches
    /// ValueError.
    TooDeep,
}

pub type PyR<T> = Result<T, Raise>;

/// The nesting past which `json.loads` raises RecursionError in the
/// oracle, and the same for recursion in Python code (`_block_chars`).
/// Neither is matched to the last level; no case sits within a few levels
/// of either (GW-5).
pub const MAX_JSON_DEPTH: usize = 9990;
pub const MAX_PY_DEPTH: usize = 960;

/// One item of `for x in v`: a list's item, or a str made for a dict's key
/// or a str's character.
pub enum Item<'a> {
    V(&'a Value),
    S(String),
}

impl Item<'_> {
    /// The item as a dict, or None.
    pub fn obj(&self) -> Option<&Object> {
        match self {
            Item::V(Value::Obj(o)) => Some(o),
            _ => None,
        }
    }
}

/// `v.get(key)`: v must be a dict.
pub fn get<'a>(v: &'a Value, key: &str) -> PyR<Option<&'a Value>> {
    match v {
        Value::Obj(o) => Ok(o.get(key)),
        _ => Err(Raise::Raise),
    }
}

/// `for x in v`: a list's items, a dict's keys, a str's characters.
pub fn iterate(v: &Value) -> PyR<Vec<Item<'_>>> {
    match v {
        Value::List(l) | Value::Tuple(l) => Ok(l.iter().map(Item::V).collect()),
        Value::Obj(o) => Ok(o.keys().iter().map(|k| Item::S(k.clone())).collect()),
        Value::Str(s) => Ok(s.chars().map(|c| Item::S(c.to_string())).collect()),
        _ => Err(Raise::Raise),
    }
}

/// `reversed(v)`: a list, a dict (its keys) and a str reverse.
pub fn reversed_of(v: &Value) -> PyR<Vec<Item<'_>>> {
    let mut items = iterate(v)?;
    items.reverse();
    Ok(items)
}

/// `len(v)` for a str, list or dict.
pub fn py_len(v: &Value) -> PyR<usize> {
    match v {
        Value::Str(s) => Ok(text::len(s)),
        Value::List(l) | Value::Tuple(l) => Ok(l.len()),
        Value::Obj(o) => Ok(o.len()),
        _ => Err(Raise::Raise),
    }
}

/// `v == s` for a str s: only an equal str is equal.
pub fn eq_str(v: Option<&Value>, s: &str) -> bool {
    matches!(v, Some(Value::Str(x)) if x == s)
}

/// Whether v can be a dict key: a list or a dict cannot.
pub fn hashable(v: &Value) -> bool {
    !matches!(v, Value::List(_) | Value::Obj(_))
}

/// `isinstance(v, int)`: a bool is an int.
pub fn is_py_int(v: &Value) -> bool {
    matches!(v, Value::Int(_) | Value::Bool(_))
}

/// A Python int (exact) or float.
enum Num {
    I(BigInt),
    F(f64),
}

fn num_of(v: &Value) -> Option<Num> {
    match v {
        Value::Bool(b) => Some(Num::I(BigInt::from(*b as u8))),
        Value::Int(t) => t.parse::<BigInt>().ok().map(Num::I),
        Value::Float(f) => Some(Num::F(*f)),
        _ => None,
    }
}

fn num_eq(a: &Num, b: &Num) -> bool {
    match (a, b) {
        (Num::I(x), Num::I(y)) => x == y,
        (Num::F(x), Num::F(y)) => x == y,
        (Num::I(i), Num::F(f)) | (Num::F(f), Num::I(i)) => {
            if !f.is_finite() || f.fract() != 0.0 {
                return false;
            }
            BigInt::from_f64(*f).is_some_and(|g| &g == i)
        }
    }
}

/// Python's `==` on two decoded JSON values.
pub fn py_equal(a: &Value, b: &Value) -> bool {
    if let (Some(x), Some(y)) = (num_of(a), num_of(b)) {
        return num_eq(&x, &y);
    }
    match (a, b) {
        (Value::Null, Value::Null) => true,
        (Value::Str(x), Value::Str(y)) => x == y,
        (Value::List(x), Value::List(y)) => x.len() == y.len() && x.iter().zip(y).all(|(p, q)| py_equal(p, q)),
        (Value::Obj(x), Value::Obj(y)) => {
            x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).is_some_and(|w| py_equal(v, w)))
        }
        _ => false,
    }
}

/// `float(n)`: correctly rounded, OverflowError past the largest double.
pub fn int_to_float(n: &BigInt) -> PyR<f64> {
    if let Some(i) = n.to_i64() {
        if i.unsigned_abs() < 1 << 53 {
            return Ok(i as f64);
        }
    }
    // Rust's decimal parse is correctly rounded, as CPython's conversion is.
    let f: f64 = n.to_string().parse().map_err(|_| Raise::Raise)?;
    if f.is_infinite() {
        return Err(Raise::Raise);
    }
    Ok(f)
}

/// `int(v)` on a decoded JSON value: an int is itself, a bool 0 or 1, a
/// float truncated (NaN and infinities raise), a str read as `int(s)`
/// reads it; anything else raises TypeError.
pub fn py_int(v: &Value) -> PyR<BigInt> {
    match v {
        Value::Bool(b) => Ok(BigInt::from(*b as u8)),
        Value::Int(t) => t.parse::<BigInt>().map_err(|_| Raise::Raise),
        Value::Float(f) => float_to_int(*f),
        Value::Str(s) => int_from_str(s),
        _ => Err(Raise::Raise),
    }
}

fn float_to_int(f: f64) -> PyR<BigInt> {
    if !f.is_finite() {
        return Err(Raise::Raise);
    }
    BigInt::from_f64(f.trunc()).ok_or(Raise::Raise)
}

/// The decimal digit value of c (Unicode category Nd), or None. Every Nd
/// block is ten consecutive code points starting at a zero.
fn decimal_value(c: char) -> Option<u32> {
    if c.is_ascii_digit() {
        return Some(c as u32 - '0' as u32);
    }
    if c.is_ascii() || !is_decimal(c) {
        return None;
    }
    let r = c as u32;
    let mut z = r;
    while r - z < 9 {
        match char::from_u32(z - 1) {
            Some(p) if is_decimal(p) => z -= 1,
            _ => break,
        }
    }
    Some(r - z)
}

/// `int(s)` in base 10: surrounding whitespace, one sign, decimal digits of
/// any script, single underscores between digits.
fn int_from_str(s: &str) -> PyR<BigInt> {
    let s = text::strip(s);
    let mut rs: &str = s;
    let mut neg = false;
    if let Some(r) = rs.strip_prefix('+') {
        rs = r;
    } else if let Some(r) = rs.strip_prefix('-') {
        rs = r;
        neg = true;
    }
    if rs.is_empty() {
        return Err(Raise::Raise);
    }
    let mut digits = String::new();
    let mut prev_underscore = true;
    for c in rs.chars() {
        if c == '_' {
            if prev_underscore {
                return Err(Raise::Raise);
            }
            prev_underscore = true;
            continue;
        }
        let d = decimal_value(c).ok_or(Raise::Raise)?;
        digits.push(char::from_digit(d, 10).ok_or(Raise::Raise)?);
        prev_underscore = false;
    }
    if prev_underscore || digits.len() > pyjson::MAX_INT_DIGITS {
        return Err(Raise::Raise);
    }
    let n: BigInt = digits.parse().map_err(|_| Raise::Raise)?;
    Ok(if neg { -n } else { n })
}

/// `a / b` for Python ints: the quotient correctly rounded to a float
/// (round half to even). `b` is not zero.
pub fn true_div(a: &BigInt, b: &BigInt) -> f64 {
    if let (Some(x), Some(y)) = (a.to_i64(), b.to_i64()) {
        if x.unsigned_abs() < 1 << 53 && y.unsigned_abs() < 1 << 53 {
            return x as f64 / y as f64;
        }
    }
    let neg = a.is_negative() != b.is_negative();
    let (a, b) = (a.abs(), b.abs());
    if a.is_zero() {
        return if neg { -0.0 } else { 0.0 };
    }
    // Scale so the quotient has 55 bits: 53 kept, a guard bit and one
    // more, with the remainder as the sticky bit.
    let k = 55 - (a.bits() as i64 - b.bits() as i64);
    let (num, den) = if k >= 0 { (&a << k as usize, b.clone()) } else { (a.clone(), &b << (-k) as usize) };
    let mut q = &num / &den;
    let mut sticky = !(&num % &den).is_zero();
    let mut e = -k;
    let limit = BigInt::from(1u64 << 54);
    while q >= limit {
        sticky |= (&q & BigInt::from(1)) != BigInt::zero();
        q >>= 1;
        e += 1;
    }
    let mut m = q.to_u64().unwrap_or(0);
    let guard = m & 1 != 0;
    m >>= 1;
    e += 1;
    if guard && (sticky || m & 1 != 0) {
        m += 1;
    }
    let f = if e > 1100 { f64::INFINITY } else if e < -1200 { 0.0 } else { m as f64 * 2f64.powi(e as i32) };
    if neg {
        -f
    } else {
        f
    }
}

/// Header bytes decoded as httpx decodes them: Latin-1.
pub fn latin1(b: &[u8]) -> String {
    b.iter().map(|&c| c as char).collect()
}

/// `json.loads` of a str, with the three ways it can fail kept apart.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum JsonErr {
    /// JSONDecodeError.
    Decode,
    /// A ValueError other than JSONDecodeError: an int past the digit limit.
    NotJson,
    /// RecursionError from nesting.
    TooDeep,
}

/// `json.loads(text)`.
pub fn loads_text(text: &str) -> Result<Value, JsonErr> {
    match py_decode_error(text, MAX_JSON_DEPTH) {
        Some(PyDecodeFail::TooDeep) => Err(JsonErr::TooDeep),
        Some(PyDecodeFail::Raised(m)) => {
            if m.starts_with("Exceeds the limit") {
                Err(JsonErr::NotJson)
            } else {
                Err(JsonErr::Decode)
            }
        }
        None => loads(text).map_err(|e| match e {
            LoadError::Recursion => JsonErr::TooDeep,
            LoadError::IntDigits => JsonErr::NotJson,
            _ => JsonErr::Decode,
        }),
    }
}

/// Whether text escapes to a character of the block that stands for the
/// lone surrogates. Such text is not decided by this port: the block's
/// characters would be read back as surrogates.
fn escapes_to_block(text: &str) -> bool {
    let lower = text.to_ascii_lowercase();
    lower.contains("\\udbfe") || lower.contains("\\udbff")
}

/// A real character of the block (not a lone surrogate the decoder
/// stands in for).
fn in_block(cp: u32) -> bool {
    cp >= text::SURROGATE_BASE && cp <= 0x10FFFF
}

/// `json.loads(b)` for bytes: the encoding detected from the first bytes
/// (UTF-32, UTF-16, UTF-8 with or without a BOM), decoded with
/// surrogatepass, then read as `json.loads` reads a str. A body that does
/// not decode, is not JSON, or is not decided by this port (it holds a
/// character of the surrogate block) is `Raise`; `TooDeep` is a
/// RecursionError.
pub fn loads_bytes(b: &[u8]) -> PyR<Value> {
    let text = decode_json_bytes(b).ok_or(Raise::Raise)?;
    if escapes_to_block(&text) {
        return Err(Raise::Raise);
    }
    match loads_text(&text) {
        Ok(v) => Ok(v),
        Err(JsonErr::TooDeep) => Err(Raise::TooDeep),
        Err(_) => Err(Raise::Raise),
    }
}

/// The text `json.loads` reads from bytes, with a lone surrogate as the
/// character that stands for it; None when the bytes do not decode.
fn decode_json_bytes(b: &[u8]) -> Option<String> {
    let has = |p: &[u8]| b.starts_with(p);
    if has(&[0, 0, 0xFE, 0xFF]) || has(&[0xFF, 0xFE, 0, 0]) {
        return decode_utf32(&b[4..], has(&[0xFF, 0xFE, 0, 0]));
    }
    if has(&[0xFE, 0xFF]) || has(&[0xFF, 0xFE]) {
        return decode_utf16(&b[2..], has(&[0xFF, 0xFE]));
    }
    if has(&[0xEF, 0xBB, 0xBF]) {
        return decode_surrogatepass(&b[3..]);
    }
    if b.len() >= 4 {
        if b[0] == 0 {
            return if b[1] != 0 { decode_utf16(b, false) } else { decode_utf32(b, false) };
        }
        if b[1] == 0 {
            return if b[2] != 0 || b[3] != 0 { decode_utf16(b, true) } else { decode_utf32(b, true) };
        }
    } else if b.len() == 2 {
        if b[0] == 0 {
            return decode_utf16(b, false);
        }
        if b[1] == 0 {
            return decode_utf16(b, true);
        }
    }
    decode_surrogatepass(b)
}

/// `b.decode("utf-8", "surrogatepass")`: UTF-8, with an encoded surrogate
/// read as that code point.
pub fn decode_surrogatepass(b: &[u8]) -> Option<String> {
    // A real character of the surrogate block is not decided (see
    // escapes_to_block): UTF-8 writes each as F4 8F, then A0 to BF.
    if b.windows(3).any(|w| w[0] == 0xF4 && w[1] == 0x8F && (0xA0..=0xBF).contains(&w[2])) {
        return None;
    }
    if let Ok(s) = std::str::from_utf8(b) {
        return Some(s.to_string());
    }
    let mut out = String::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b.len() - i >= 3 && b[i] == 0xED && (0xA0..=0xBF).contains(&b[i + 1]) && (0x80..=0xBF).contains(&b[i + 2]) {
            let cp = 0xD000 | ((b[i + 1] as u32 & 0x3F) << 6) | (b[i + 2] as u32 & 0x3F);
            out.push(surrogate_char(cp));
            i += 3;
            continue;
        }
        let end = (i + 4).min(b.len());
        let mut ok = false;
        for n in 1..=end - i {
            if let Ok(s) = std::str::from_utf8(&b[i..i + n]) {
                out.push_str(s);
                i += n;
                ok = true;
                break;
            }
        }
        if !ok {
            return None;
        }
    }
    Some(out)
}

/// Pushes a decoded code point; false for a real character of the
/// surrogate block, which is not decided.
fn push_cp(out: &mut String, cp: u32) -> bool {
    if in_block(cp) {
        return false;
    }
    out.push(text::cp_char(cp));
    true
}

/// The utf-16 codec with surrogatepass: a lone surrogate is kept; an odd
/// byte count fails.
fn decode_utf16(b: &[u8], le: bool) -> Option<String> {
    if b.len() % 2 != 0 {
        return None;
    }
    let units: Vec<u32> =
        b.chunks(2).map(|p| if le { p[0] as u32 | (p[1] as u32) << 8 } else { (p[0] as u32) << 8 | p[1] as u32 }).collect();
    let mut out = String::new();
    let mut i = 0;
    while i < units.len() {
        let u = units[i];
        if (0xD800..0xDC00).contains(&u) && i + 1 < units.len() && (0xDC00..=0xDFFF).contains(&units[i + 1]) {
            if !push_cp(&mut out, 0x10000 + ((u - 0xD800) << 10) + (units[i + 1] - 0xDC00)) {
                return None;
            }
            i += 2;
            continue;
        }
        if !push_cp(&mut out, u) {
            return None;
        }
        i += 1;
    }
    Some(out)
}

fn decode_utf32(b: &[u8], le: bool) -> Option<String> {
    if b.len() % 4 != 0 {
        return None;
    }
    let mut out = String::new();
    for p in b.chunks(4) {
        let u = if le {
            p[0] as u32 | (p[1] as u32) << 8 | (p[2] as u32) << 16 | (p[3] as u32) << 24
        } else {
            (p[0] as u32) << 24 | (p[1] as u32) << 16 | (p[2] as u32) << 8 | p[3] as u32
        };
        if u > 0x10FFFF || !push_cp(&mut out, u) {
            return None;
        }
    }
    Some(out)
}

/// `s.encode("utf-8", "surrogatepass")`: a lone surrogate written as its
/// three bytes.
pub fn encode_surrogatepass(s: &str) -> Vec<u8> {
    let mut out = Vec::with_capacity(s.len());
    let mut buf = [0u8; 4];
    for c in s.chars() {
        match as_surrogate(c) {
            Some(u) => out.extend_from_slice(&[0xE0 | (u >> 12) as u8, 0x80 | ((u >> 6) & 0x3F) as u8, 0x80 | (u & 0x3F) as u8]),
            None => out.extend_from_slice(c.encode_utf8(&mut buf).as_bytes()),
        }
    }
    out
}

/// `s.encode("utf-8")` in strict mode: a lone surrogate raises.
pub fn encode_utf8(s: &str) -> PyR<&[u8]> {
    if text::has_surrogate(s) {
        return Err(Raise::Raise);
    }
    Ok(s.as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn int_reads_as_python_does() {
        let i = |s: &str| int_from_str(s).ok().map(|n| n.to_string());
        assert_eq!(i(" 12 "), Some("12".into()));
        assert_eq!(i("1_000"), Some("1000".into()));
        assert_eq!(i("-٣"), Some("-3".into()));
        assert_eq!(i("_1"), None);
        assert_eq!(i("1__0"), None);
        assert_eq!(i("3.7"), None);
        assert_eq!(py_int(&Value::Float(3.7)).unwrap().to_string(), "3");
        assert!(py_int(&Value::Float(f64::INFINITY)).is_err());
    }

    #[test]
    fn true_div_rounds_as_python_does() {
        let b = |s: &str| s.parse::<BigInt>().unwrap();
        assert_eq!(true_div(&b("1"), &b("3")), 1.0 / 3.0);
        assert_eq!(true_div(&b("10000000000000000000000000000001"), &b("3")), 3.3333333333333335e30);
        assert_eq!(true_div(&b("9007199254740993"), &b("1")), 9007199254740992.0);
        assert_eq!(true_div(&b("9007199254740995"), &b("1")), 9007199254740996.0);
    }

    #[test]
    fn bytes_decode_as_json_loads_decodes_them() {
        assert_eq!(loads_bytes(b"\xef\xbb\xbf{\"a\": 1}").unwrap(), Value::Obj(Object::new().with("a", Value::Int("1".into()))));
        let v = loads_bytes(b"\"\xed\xa0\x80\"").unwrap();
        assert_eq!(v, Value::Str(surrogate_char(0xd800).to_string()));
        assert_eq!(encode_surrogatepass(v.as_str().unwrap()), b"\xed\xa0\x80");
        assert!(loads_bytes("\"\u{10F800}\"".as_bytes()).is_err());
        assert!(loads_bytes(b"\"\\udbfe\\udc00\"").is_err());
        let deep = "[".repeat(10_000) + &"]".repeat(10_000);
        assert_eq!(loads_bytes(deep.as_bytes()), Err(Raise::TooDeep));
    }

    #[test]
    fn lower_keeps_pythons_special_cases() {
        assert_eq!(text::lower("ΣΑΣ"), "σας");
        assert_eq!(text::lower("İ"), "i\u{307}");
    }
}
