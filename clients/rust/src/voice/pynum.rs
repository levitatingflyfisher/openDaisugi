//! Python's `int()` and `float()` of a str.

use num_bigint::BigInt;

use crate::gate::py::text;

/// `float(s)`: None where Python raises ValueError.
pub fn py_float(s: &str) -> Option<f64> {
    crate::gate::argparse::py_float(s)
}

/// `int(s)`: None where Python raises ValueError. Unicode digits and
/// spaces count, as `_PyUnicode_TransformDecimalAndSpaceToASCII` maps them,
/// and one underscore may sit between two digits.
pub fn py_int(s: &str) -> Option<BigInt> {
    let mut a = String::with_capacity(s.len());
    for c in s.chars() {
        if text::is_space(c) {
            a.push(' ');
        } else if c.is_ascii() {
            a.push(c);
        } else if text::is_decimal(c) {
            a.push(char::from_digit(decimal_value(c)?, 10)?);
        } else {
            return None;
        }
    }
    let t = a.trim_matches(' ');
    let (neg, body) = match t.as_bytes().first() {
        Some(b'-') => (true, &t[1..]),
        Some(b'+') => (false, &t[1..]),
        _ => (false, t),
    };
    let b = body.as_bytes();
    if b.is_empty() || !b[0].is_ascii_digit() || !b[b.len() - 1].is_ascii_digit() {
        return None;
    }
    let mut digits = String::with_capacity(b.len());
    for (i, &c) in b.iter().enumerate() {
        if c == b'_' {
            if !(b[i - 1].is_ascii_digit() && b[i + 1].is_ascii_digit()) {
                return None;
            }
            continue;
        }
        if !c.is_ascii_digit() {
            return None;
        }
        digits.push(c as char);
    }
    let n: BigInt = digits.parse().ok()?;
    Some(if neg { -n } else { n })
}

/// The value of a Unicode decimal digit: its offset from the zero of its
/// run of ten.
fn decimal_value(c: char) -> Option<u32> {
    let cp = c as u32;
    for k in 0..10 {
        let zero = cp.checked_sub(k)?;
        if let Some(z) = char::from_u32(zero) {
            if text::is_decimal(z) && (zero == 0 || !char::from_u32(zero - 1).map(text::is_decimal).unwrap_or(false)) {
                return Some(k);
            }
        }
    }
    None
}
