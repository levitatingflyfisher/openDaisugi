//! The zero-model matcher of ADR-0019: signed feature hashing over
//! unigrams. It needs no model file and no network. It lives in the pando
//! crate so that coppice-rs ranks recall with the same matcher as the CLI.
//!
//! It is `_search._LexicalEmbedder` of the Python oracle, byte for byte:
//! the lowercased text, ASCII `[a-z0-9_]+` runs, the same stopwords,
//! an 8-byte BLAKE2b per token, a bucket and a sign from that hash, and
//! the square root of the term count.

use super::blake2b;

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
    // Only ASCII word bytes make a token, so Rust's lowercase gives the same
    // tokens as Python's str.lower().
    let s = text.to_lowercase();
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

