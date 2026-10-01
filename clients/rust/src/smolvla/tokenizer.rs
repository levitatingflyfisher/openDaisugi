//! The minimal byte-level BPE the oracle runs: added tokens matched first,
//! then the ByteLevel pre-tokenizer, then BPE merges by rank, with the
//! vocabulary, merges and added tokens of the backbone's tokenizer.json.
//! The oracle's tokenizer is transformers' GPT2Tokenizer, which builds its
//! own pipeline (ByteLevel, no normalizer, no special tokens added) and
//! does not run the Digits pre-tokenizer the file also names (ruling
//! VL-R-5).

use std::collections::HashMap;

use serde_json::Value;

use super::tables_gen::{LETTER_RANGES, NUMBER_RANGES, SPACE_RANGES};

/// The checkpoint's tokenizer_max_length: the token ids the prefix graph
/// takes.
pub const WIDTH: usize = 48;

/// The backbone's pad_token (tokenizer_config.json).
const PAD_TOKEN: &str = "<|im_end|>";

fn within(table: &[(u32, u32)], c: char) -> bool {
    let c = c as u32;
    let i = table.partition_point(|&(_, hi)| hi < c);
    i < table.len() && table[i].0 <= c
}

fn is_letter(c: char) -> bool {
    within(LETTER_RANGES, c)
}
fn is_number(c: char) -> bool {
    within(NUMBER_RANGES, c)
}
fn is_space(c: char) -> bool {
    within(SPACE_RANGES, c)
}
fn is_other(c: char) -> bool {
    !is_letter(c) && !is_number(c) && !is_space(c)
}

pub struct Tokenizer {
    vocab: HashMap<String, i64>,
    ranks: HashMap<(String, String), usize>,
    /// Longest first.
    added: Vec<(String, i64)>,
    pad: i64,
    byte_ch: [char; 256],
}

impl Tokenizer {
    /// Read a tokenizer.json.
    pub fn load(path: &std::path::Path) -> Result<Tokenizer, String> {
        let b = std::fs::read(path).map_err(|e| format!("{}: {e}", path.display()))?;
        Tokenizer::parse(&b)
    }

    /// Parse a tokenizer.json. It refuses any file whose model this code
    /// does not model, rather than tokenize it wrongly.
    pub fn parse(b: &[u8]) -> Result<Tokenizer, String> {
        let f: Value = serde_json::from_slice(b).map_err(|e| format!("tokenizer.json: {e}"))?;
        let model = &f["model"];
        if model["type"] != "BPE" {
            return Err(format!(
                "tokenizer.json: model {} is not BPE",
                model["type"]
            ));
        }
        let set = |k: &str| !model[k].is_null();
        if set("dropout")
            || model["byte_fallback"] == true
            || model["ignore_merges"] == true
            || set("continuing_subword_prefix")
            || set("end_of_word_suffix")
        {
            return Err("tokenizer.json: a BPE option is not supported".into());
        }
        let mut vocab = HashMap::new();
        for (k, v) in model["vocab"]
            .as_object()
            .ok_or("tokenizer.json: no vocab")?
        {
            let id = v
                .as_i64()
                .ok_or("tokenizer.json: a vocab id is not an integer")?;
            vocab.insert(k.clone(), id);
        }
        let merges = model["merges"]
            .as_array()
            .ok_or("tokenizer.json: no merges")?;
        let mut ranks = HashMap::with_capacity(merges.len());
        for (i, m) in merges.iter().enumerate() {
            let pair = match m {
                Value::Array(l) if l.len() == 2 => match (l[0].as_str(), l[1].as_str()) {
                    (Some(a), Some(b)) => Some((a.to_string(), b.to_string())),
                    _ => None,
                },
                Value::String(s) if s.matches(' ').count() == 1 => {
                    let (a, b) = s.split_once(' ').expect("one space");
                    Some((a.to_string(), b.to_string()))
                }
                _ => None,
            }
            .ok_or_else(|| format!("tokenizer.json: merge {i} is not a pair"))?;
            ranks.entry(pair).or_insert(i);
        }
        let mut added = Vec::new();
        let mut pad = None;
        for a in f["added_tokens"]
            .as_array()
            .map(|v| v.as_slice())
            .unwrap_or(&[])
        {
            let content = a["content"].as_str().unwrap_or("").to_string();
            let id = a["id"]
                .as_i64()
                .ok_or("tokenizer.json: an added token has no id")?;
            if ["single_word", "lstrip", "rstrip", "normalized"]
                .iter()
                .any(|k| a[*k] == true)
                || content.is_empty()
            {
                return Err(format!(
                    "tokenizer.json: added token {content:?} has an option that is not supported"
                ));
            }
            if content == PAD_TOKEN {
                pad = Some(id);
            }
            added.push((content, id));
        }
        let pad = pad.ok_or_else(|| format!("tokenizer.json: no pad token {PAD_TOKEN}"))?;
        added.sort_by(|a, b| b.0.len().cmp(&a.0.len()));
        Ok(Tokenizer {
            vocab,
            ranks,
            added,
            pad,
            byte_ch: byte_chars(),
        })
    }

    /// The tokenizer's ids for text, with no special tokens added.
    pub fn encode(&self, text: &str) -> Result<Vec<i64>, String> {
        let mut ids = Vec::new();
        let mut start = 0;
        let mut i = 0;
        while i < text.len() {
            if let Some((content, id)) = self
                .added
                .iter()
                .find(|(c, _)| text[i..].starts_with(c.as_str()))
            {
                self.encode_plain(&text[start..i], &mut ids)?;
                ids.push(*id);
                i += content.len();
                start = i;
                continue;
            }
            i += text[i..].chars().next().map_or(1, char::len_utf8);
        }
        self.encode_plain(&text[start..], &mut ids)?;
        Ok(ids)
    }

    fn encode_plain(&self, s: &str, ids: &mut Vec<i64>) -> Result<(), String> {
        for word in pre_tokenize(s) {
            let sym: Vec<String> = word
                .bytes()
                .map(|b| self.byte_ch[b as usize].to_string())
                .collect();
            for piece in self.bpe(sym) {
                let id = self
                    .vocab
                    .get(&piece)
                    .ok_or_else(|| format!("tokenizer: {piece:?} is not in the vocabulary"))?;
                ids.push(*id);
            }
        }
        Ok(())
    }

    /// Merge the adjacent pair of lowest rank, leftmost first, until no
    /// pair has a rank.
    fn bpe(&self, mut sym: Vec<String>) -> Vec<String> {
        while sym.len() > 1 {
            let mut best: Option<(usize, usize)> = None;
            for i in 0..sym.len() - 1 {
                if let Some(&r) = self.ranks.get(&(sym[i].clone(), sym[i + 1].clone())) {
                    if best.is_none_or(|(b, _)| r < b) {
                        best = Some((r, i));
                    }
                }
            }
            let Some((_, at)) = best else { break };
            let next = sym.remove(at + 1);
            sym[at].push_str(&next);
        }
        sym
    }

    /// What lerobot's processors make of a task: a newline added when the
    /// text has none, the ids truncated on the left to WIDTH (the
    /// backbone's truncation_side), then padded on the right with the pad
    /// token; the mask is 1 for each real token.
    pub fn task(&self, text: &str) -> Result<([i64; WIDTH], [i64; WIDTH]), String> {
        let text = if text.ends_with('\n') {
            text.to_string()
        } else {
            format!("{text}\n")
        };
        let mut all = self.encode(&text)?;
        if all.len() > WIDTH {
            all.drain(..all.len() - WIDTH);
        }
        let mut ids = [self.pad; WIDTH];
        let mut mask = [0i64; WIDTH];
        for (i, id) in all.iter().enumerate() {
            ids[i] = *id;
            mask[i] = 1;
        }
        Ok((ids, mask))
    }
}

/// GPT-2's bytes_to_unicode: each byte as a printable char.
fn byte_chars() -> [char; 256] {
    let mut out = ['\0'; 256];
    let mut n = 0u32;
    for b in 0..256u32 {
        let printable =
            (0x21..=0x7E).contains(&b) || (0xA1..=0xAC).contains(&b) || (0xAE..=0xFF).contains(&b);
        out[b as usize] = if printable {
            char::from_u32(b).expect("a byte is a char")
        } else {
            n += 1;
            char::from_u32(255 + n).expect("below 0x200 is a char")
        };
    }
    out
}

const CONTRACTIONS: [&str; 7] = ["s", "t", "re", "ve", "m", "ll", "d"];

/// Split s by the ByteLevel pattern
///
/// `'s|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+(?!\S)|\s+`
///
/// written out by hand, as the Go port does.
pub(super) fn pre_tokenize(s: &str) -> Vec<String> {
    let r: Vec<char> = s.chars().collect();
    let mut out = Vec::new();
    let mut i = 0;
    while i < r.len() {
        let j = match_at(&r, i);
        out.push(r[i..j].iter().collect());
        i = j;
    }
    out
}

fn run(r: &[char], mut i: usize, f: fn(char) -> bool) -> usize {
    while i < r.len() && f(r[i]) {
        i += 1;
    }
    i
}

fn match_at(r: &[char], i: usize) -> usize {
    if r[i] == '\'' {
        for c in CONTRACTIONS {
            let n = c.chars().count();
            if r.len() > i + n && r[i + 1..=i + n].iter().copied().eq(c.chars()) {
                return i + 1 + n;
            }
        }
    }
    for f in [is_letter as fn(char) -> bool, is_number, is_other] {
        let k = if r[i] == ' ' && i + 1 < r.len() {
            i + 1
        } else {
            i
        };
        if f(r[k]) {
            return run(r, k, f);
        }
    }
    // \s+(?!\S), then \s+.
    let j = run(r, i, is_space);
    if j == r.len() || j - i == 1 {
        j
    } else {
        j - 1
    }
}
