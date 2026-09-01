//! A stored embedding, read for scoring: `task_embedding_json` as
//! `json.loads` and `list[float]` read it. The text `json.dumps` writes
//! for a list of floats is read in one pass, zeros cheaply; any other
//! text goes through the general reader.

use super::pmodel::{validate, Mode, Schema};
use crate::gate::pyjson::{loads, Value};

/// A stored vector: its length, its entries that are not zero, and the
/// sum of their squares in numpy's order.
#[derive(Debug, Clone, Default)]
pub struct Sparse {
    pub n: usize,
    pub idx: Vec<u32>,
    pub val: Vec<f64>,
    pub sumsq: f64,
}

impl Sparse {
    /// The dot product with a dense vector of the same length.
    pub fn dot(&self, q: &[f64]) -> f64 {
        let mut d = 0.0;
        for (j, &i) in self.idx.iter().enumerate() {
            d += self.val[j] * q[i as usize];
        }
        d
    }
}

/// The text of eight zeros in a row, as json.dumps writes them.
const ZEROS8: &[u8] = b"0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, ";

/// Reads `task_embedding_json`. `None` when it is not a list of numbers
/// pydantic takes as `list[float]`.
pub fn scan_embedding(s: &[u8]) -> Option<Sparse> {
    if let Some(sp) = scan_fast(s) {
        return Some(sp);
    }
    let text = std::str::from_utf8(s).ok()?;
    parse_embedding(text).map(|v| to_sparse(&v))
}

/// The floats of an embedding text, or `None`.
pub fn parse_embedding(text: &str) -> Option<Vec<f64>> {
    if let Some(sp) = scan_fast(text.as_bytes()) {
        let mut v = vec![0.0; sp.n];
        for (j, &i) in sp.idx.iter().enumerate() {
            v[i as usize] = sp.val[j];
        }
        // A -0.0 is kept as such by the general reader; the fast path
        // reads it as zero, so read such a text in full.
        if !text.contains("-0.0") {
            return Some(v);
        }
    }
    let raw = loads(text).ok()?;
    let out = validate("CompiledPathway", &Schema::List(Box::new(Schema::Float)), &raw, Mode::Python).ok()?;
    match out {
        Value::List(xs) => Some(
            xs.iter()
                .map(|x| match x {
                    Value::Float(f) => *f,
                    _ => 0.0,
                })
                .collect(),
        ),
        _ => None,
    }
}

pub fn to_sparse(vec: &[f64]) -> Sparse {
    let mut sp = Sparse { n: vec.len(), ..Default::default() };
    for (i, &x) in vec.iter().enumerate() {
        if x != 0.0 || x.is_nan() {
            sp.idx.push(i as u32);
            sp.val.push(x);
        }
    }
    sp.sumsq = pairwise_sq(&sp.idx, &sp.val, 0, sp.n);
    sp
}

/// numpy's pairwise sum (add.reduce along a row) of the squares of a
/// sparse vector's entries in [lo, lo+n): eight running sums in blocks of
/// at most 128, halves above that. The order of the sums decides the last
/// bit, which decides a near tie.
pub fn pairwise_sq(idx: &[u32], val: &[f64], lo: usize, n: usize) -> f64 {
    let a = idx.partition_point(|&i| (i as usize) < lo);
    let b = idx.partition_point(|&i| (i as usize) < lo + n);
    if a == b {
        return 0.0;
    }
    if n < 8 {
        let mut res = 0.0;
        for k in a..b {
            res += val[k] * val[k];
        }
        return res;
    }
    if n <= 128 {
        let mut r = [0.0f64; 8];
        let main = lo + n - n % 8;
        let mut k = a;
        while k < b && (idx[k] as usize) < main {
            r[(idx[k] as usize - lo) % 8] += val[k] * val[k];
            k += 1;
        }
        let mut res = ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]));
        while k < b {
            res += val[k] * val[k];
            k += 1;
        }
        return res;
    }
    let mut n2 = n / 2;
    n2 -= n2 % 8;
    pairwise_sq(idx, val, lo, n2) + pairwise_sq(idx, val, lo + n2, n - n2)
}

/// A JSON number with a fraction or an exponent, the form Python writes a
/// float in. An int token is left to the general reader.
fn float_token(t: &[u8]) -> bool {
    let n = t.len();
    let mut i = 0;
    if i < n && t[i] == b'-' {
        i += 1;
    }
    let digits = |i: &mut usize| {
        let k = *i;
        while *i < n && t[*i].is_ascii_digit() {
            *i += 1;
        }
        *i - k
    };
    if i < n && t[i] == b'0' {
        i += 1;
    } else if digits(&mut i) == 0 {
        return false;
    }
    let (mut frac, mut exp) = (false, false);
    if i < n && t[i] == b'.' {
        i += 1;
        if digits(&mut i) == 0 {
            return false;
        }
        frac = true;
    }
    if i < n && (t[i] == b'e' || t[i] == b'E') {
        i += 1;
        if i < n && (t[i] == b'+' || t[i] == b'-') {
            i += 1;
        }
        if digits(&mut i) == 0 {
            return false;
        }
        exp = true;
    }
    i == n && (frac || exp)
}

fn scan_fast(s: &[u8]) -> Option<Sparse> {
    let n = s.len();
    let mut sp = Sparse::default();
    if n < 2 || s[0] != b'[' || s[n - 1] != b']' {
        return None;
    }
    if n == 2 {
        return Some(sp);
    }
    let mut i = 1;
    let mut k: usize = 0;
    loop {
        while i + 40 <= n && &s[i..i + 40] == ZEROS8 {
            i += 40;
            k += 8;
        }
        if i + 3 < n && s[i] == b'0' && s[i + 1] == b'.' && s[i + 2] == b'0' && (s[i + 3] == b',' || s[i + 3] == b']') {
            i += 3;
        } else {
            let mut j = i;
            while j < n - 1 && s[j] != b',' {
                j += 1;
            }
            let tok = &s[i..j];
            if tok != b"-0.0" {
                if !float_token(tok) {
                    return None;
                }
                // float_token admits ASCII only.
                let f: f64 = std::str::from_utf8(tok).ok()?.parse().ok()?;
                if f != 0.0 || f.is_nan() {
                    sp.idx.push(k as u32);
                    sp.val.push(f);
                }
            }
            i = j;
        }
        k += 1;
        if i == n - 1 {
            sp.n = k;
            sp.sumsq = pairwise_sq(&sp.idx, &sp.val, 0, sp.n);
            return Some(sp);
        }
        if s[i] != b',' || i + 1 >= n || s[i + 1] != b' ' {
            return None;
        }
        i += 2;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_fast_reader_takes_what_json_dumps_writes() {
        let sp = scan_embedding(b"[0.0, 0.5, 0.0, -0.25]").unwrap();
        assert_eq!(sp.n, 4);
        assert_eq!(sp.idx, vec![1, 3]);
        assert_eq!(sp.sumsq, 0.25 + 0.0625);
        assert_eq!(scan_embedding(b"[]").unwrap().n, 0);
        // Ints and other spacing go through the general reader.
        let sp = scan_embedding(b"[1,0,2]").unwrap();
        assert_eq!((sp.n, sp.idx.clone()), (3, vec![0, 2]));
        assert!(scan_embedding(b"[\"a\"]").is_none());
        assert!(scan_embedding(b"{}").is_none());
        assert!(scan_embedding(b"nope").is_none());
    }
}
