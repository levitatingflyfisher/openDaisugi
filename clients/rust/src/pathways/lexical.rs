//! The zero-model matcher of ADR-0019: signed feature hashing over
//! unigrams. It needs no model file and no network.
//!
//! It is `_search._LexicalEmbedder` of the Python oracle, byte for byte:
//! Python's `str.lower()`, ASCII `[a-z0-9_]+` runs, the same stopwords,
//! an 8-byte BLAKE2b per token, a bucket and a sign from that hash, and
//! the square root of the term count.

use super::blake2b;
use crate::gate::py::text::lower;

/// The provenance stamp. It changes when the feature scheme changes.
pub const IDENTITY: &str = "lexical-hash-v1";
/// The width of a vector.
pub const DIM: usize = 4096;
/// The reuse threshold, FPR-matched in ADR-0019.
pub const THRESHOLD: f64 = 0.25;

const STOPWORDS: &[&str] = &[
    "a", "an", "the", "and", "or", "of", "to", "in", "on", "for", "with", "by", "from", "at", "as", "is", "are", "be",
    "this", "that", "it", "its", "into", "via", "using", "use", "run", "make", "add", "fix", "update",
];

fn is_word_byte(c: u8) -> bool {
    c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'_'
}

/// The tokens of one text, stopwords dropped, in order.
pub fn tokens(text: &str) -> Vec<String> {
    let s = lower(text);
    let b = s.as_bytes();
    let mut out = vec![];
    let mut i = 0;
    while i < b.len() {
        if !is_word_byte(b[i]) {
            i += 1;
            continue;
        }
        let mut j = i;
        while j < b.len() && is_word_byte(b[j]) {
            j += 1;
        }
        let t = &s[i..j];
        if !STOPWORDS.contains(&t) {
            out.push(t.to_string());
        }
        i = j;
    }
    out
}

/// `int(blake2b(token, digest_size=8).hexdigest(), 16)`.
pub fn hash(token: &str) -> u64 {
    let d = blake2b::digest(token.as_bytes(), 8);
    let mut b = [0u8; 8];
    b.copy_from_slice(&d);
    u64::from_be_bytes(b)
}

/// `_lexical_vector`: the unnormalized vector of one text. Tokens add
/// into their buckets in the order Counter first saw them.
pub fn vector(text: &str) -> Vec<f64> {
    let mut vec = vec![0.0; DIM];
    let mut order: Vec<String> = vec![];
    let mut counts: std::collections::HashMap<String, usize> = std::collections::HashMap::new();
    for t in tokens(text) {
        let c = counts.entry(t.clone()).or_insert(0);
        if *c == 0 {
            order.push(t);
        }
        *c += 1;
    }
    for t in order {
        let h = hash(&t);
        let sign = if h >> 63 != 0 { 1.0 } else { -1.0 };
        vec[(h % DIM as u64) as usize] += sign * (counts[&t] as f64).sqrt();
    }
    vec
}

/// `_LexicalEmbedder.encode` for one text: the vector over its norm (at
/// least 1e-12).
pub fn encode(text: &str) -> Vec<f64> {
    let mut v = vector(text);
    let ss: f64 = v.iter().map(|x| x * x).sum();
    let n = ss.sqrt().max(1e-12);
    for x in &mut v {
        *x /= n;
    }
    v
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::{loads, Value};

    fn units() -> Value {
        let p = concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/pathways/units.json");
        loads(&std::fs::read_to_string(p).unwrap()).unwrap()
    }

    #[test]
    fn digests_match_the_oracle() {
        let u = units();
        let Value::List(pairs) = u.as_obj().unwrap().value("blake2b") else { panic!() };
        assert!(pairs.len() > 50);
        for p in pairs {
            let Value::List(p) = p else { panic!() };
            let (t, want) = (p[0].as_str().unwrap(), p[1].as_str().unwrap());
            assert_eq!(format!("{:016x}", hash(t)), want, "{t:?}");
        }
    }

    #[test]
    fn vectors_match_the_oracle() {
        let u = units();
        let Value::List(cases) = u.as_obj().unwrap().value("lexical") else { panic!() };
        for c in cases {
            let o = c.as_obj().unwrap();
            let text = o.value("text").as_str().unwrap();
            let Value::List(toks) = o.value("tokens") else { panic!() };
            let want: Vec<&str> = toks.iter().map(|t| t.as_str().unwrap()).collect();
            assert_eq!(tokens(text), want, "{text:?}");
            let v = encode(text);
            let Value::List(nz) = o.value("nz") else { panic!() };
            let mut got = vec![];
            for (i, x) in v.iter().enumerate() {
                if *x != 0.0 {
                    got.push((i, *x));
                }
            }
            assert_eq!(got.len(), nz.len(), "{text:?}");
            for (g, w) in got.iter().zip(nz) {
                let Value::List(w) = w else { panic!() };
                let wi: usize = match &w[0] {
                    Value::Int(t) => t.parse().unwrap(),
                    _ => panic!(),
                };
                let wx = match &w[1] {
                    Value::Float(f) => *f,
                    _ => panic!(),
                };
                assert_eq!(g.0, wi, "{text:?}");
                assert!((g.1 - wx).abs() < 1e-12, "{text:?}: {} vs {wx}", g.1);
            }
        }
    }
}
