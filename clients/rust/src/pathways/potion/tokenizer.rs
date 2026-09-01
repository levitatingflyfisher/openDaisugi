//! The part of a tokenizers `tokenizer.json` that model2vec's
//! `encode_batch_fast(texts, add_special_tokens=False)` runs for a potion
//! model: added tokens split out of the raw text, BertNormalizer,
//! BertPreTokenizer and WordPiece. Any other component is refused at
//! load, never guessed at.

use std::collections::HashMap;

use super::tables::{NORM_CJK, NORM_MAPPED, NORM_REMOVED, NORM_SPACE, PRE_PUNCT, PRE_SPACE};

pub struct Tokenizer {
    vocab: HashMap<String, usize>,
    pub unk_id: usize,
    prefix: String,
    max_chars: usize,
    /// Longest first.
    added: Vec<(String, usize)>,
    all: HashMap<String, usize>,
}

fn is_null(v: Option<&serde_json::Value>) -> bool {
    matches!(v, None | Some(serde_json::Value::Null))
}

fn id_of(v: &serde_json::Value) -> Option<usize> {
    v.as_u64().map(|n| n as usize)
}

impl Tokenizer {
    /// Reads tokenizer.json.
    pub fn parse(data: &[u8]) -> Result<Tokenizer, String> {
        let raw: serde_json::Value = serde_json::from_slice(data).map_err(|e| format!("tokenizer.json: {e}"))?;
        let get = |k: &str| raw.get(k);
        if !is_null(get("truncation")) || !is_null(get("padding")) {
            return Err("tokenizer.json sets truncation or padding, which this binary does not model".into());
        }
        let norm = get("normalizer").cloned().unwrap_or(serde_json::Value::Null);
        let flag = |k: &str| norm.get(k).and_then(|v| v.as_bool()).unwrap_or(false);
        let strip = norm.get("strip_accents");
        let strip_ok = matches!(strip, None | Some(serde_json::Value::Null) | Some(serde_json::Value::Bool(true)));
        if norm.get("type").and_then(|t| t.as_str()) != Some("BertNormalizer")
            || !flag("clean_text")
            || !flag("handle_chinese_chars")
            || !flag("lowercase")
            || !strip_ok
        {
            return Err("tokenizer.json has a normalizer this binary does not model (only BertNormalizer with \
                        clean_text, handle_chinese_chars and lowercase)"
                .into());
        }
        let pre = get("pre_tokenizer").and_then(|p| p.get("type")).and_then(|t| t.as_str());
        if pre != Some("BertPreTokenizer") {
            return Err("tokenizer.json has a pre_tokenizer this binary does not model (only BertPreTokenizer)".into());
        }
        let model = get("model").cloned().unwrap_or(serde_json::Value::Null);
        let unk = model.get("unk_token").and_then(|u| u.as_str());
        let vocab_raw = model.get("vocab").and_then(|v| v.as_object());
        let (unk, vocab_raw) = match (model.get("type").and_then(|t| t.as_str()), unk, vocab_raw) {
            (Some("WordPiece"), Some(u), Some(v)) => (u.to_string(), v),
            _ => {
                return Err("tokenizer.json has a model this binary does not model (only WordPiece with an unk_token)".into())
            }
        };
        let mut vocab = HashMap::new();
        for (k, v) in vocab_raw {
            let id = id_of(v).ok_or("tokenizer.json: a vocabulary id is not a number")?;
            vocab.insert(k.clone(), id);
        }
        let unk_id = *vocab.get(&unk).ok_or_else(|| format!("tokenizer.json: the unk_token {unk:?} is not in the vocabulary"))?;
        let prefix = model.get("continuing_subword_prefix").and_then(|p| p.as_str()).unwrap_or("").to_string();
        let max_chars = model.get("max_input_chars_per_word").and_then(|p| p.as_u64()).unwrap_or(0) as usize;
        let mut all = vocab.clone();
        let mut added = vec![];
        if let Some(list) = get("added_tokens").and_then(|a| a.as_array()) {
            for a in list {
                let b = |k: &str| a.get(k).and_then(|v| v.as_bool()).unwrap_or(false);
                let content = a.get("content").and_then(|c| c.as_str()).unwrap_or("").to_string();
                if b("single_word") || b("lstrip") || b("rstrip") || b("normalized") || content.is_empty() {
                    return Err(format!("tokenizer.json has an added token ({content:?}) with options this binary does not model"));
                }
                let id = a.get("id").and_then(id_of).ok_or("tokenizer.json: an added token has no id")?;
                all.insert(content.clone(), id);
                added.push((content, id));
            }
        }
        added.sort_by(|a, b| b.0.len().cmp(&a.0.len()));
        Ok(Tokenizer { vocab, unk_id, prefix, max_chars, added, all })
    }

    /// `get_vocab()`: the model's vocabulary with the added tokens.
    pub fn vocab(&self) -> &HashMap<String, usize> {
        &self.all
    }

    /// `encode(text, add_special_tokens=False).ids`.
    pub fn encode(&self, text: &str) -> Vec<usize> {
        let mut ids = vec![];
        let mut text = text;
        while !text.is_empty() {
            match self.find_added(text) {
                None => {
                    self.segment(text, &mut ids);
                    break;
                }
                Some((at, content, id)) => {
                    self.segment(&text[..at], &mut ids);
                    ids.push(id);
                    text = &text[at + content.len()..];
                }
            }
        }
        ids
    }

    /// The leftmost added token in `text`, the longest one at that place.
    fn find_added<'a>(&'a self, text: &str) -> Option<(usize, &'a str, usize)> {
        let mut best: Option<(usize, &str, usize)> = None;
        for (c, id) in &self.added {
            if let Some(i) = text.find(c.as_str()) {
                if best.is_none_or(|b| i < b.0) {
                    best = Some((i, c, *id));
                }
            }
        }
        best
    }

    fn segment(&self, seg: &str, ids: &mut Vec<usize>) {
        if seg.is_empty() {
            return;
        }
        for word in pre_tokenize(&normalize(seg)) {
            self.word_piece(word, ids);
        }
    }

    /// WordPiece.tokenize for one word: greedy longest match, the prefix
    /// on every piece after the first, the whole word unknown when one
    /// piece has no match or the word is too long.
    fn word_piece(&self, word: &str, ids: &mut Vec<usize>) {
        if word.chars().count() > self.max_chars {
            ids.push(self.unk_id);
            return;
        }
        let mut sub = vec![];
        let mut start = 0;
        let mut buf = String::new();
        while start < word.len() {
            let mut end = word.len();
            let mut found = None;
            while start < end {
                let piece = &word[start..end];
                let key: &str = if start > 0 {
                    buf.clear();
                    buf.push_str(&self.prefix);
                    buf.push_str(piece);
                    &buf
                } else {
                    piece
                };
                if let Some(&id) = self.vocab.get(key) {
                    found = Some(id);
                    break;
                }
                let last = word[start..end].chars().next_back().map_or(1, |c| c.len_utf8());
                end -= last;
            }
            match found {
                None => {
                    ids.push(self.unk_id);
                    return;
                }
                Some(id) => {
                    sub.push(id);
                    start = end;
                }
            }
        }
        ids.extend(sub);
    }
}

fn in_table(tab: &[(u32, u32)], c: u32) -> bool {
    let i = tab.partition_point(|&(_, hi)| hi < c);
    i < tab.len() && tab[i].0 <= c
}

fn mapped(c: u32) -> Option<&'static str> {
    NORM_MAPPED.binary_search_by_key(&c, |&(k, _)| k).ok().map(|i| NORM_MAPPED[i].1)
}

/// BertNormalizer, one code point at a time.
pub fn normalize(s: &str) -> String {
    let mut b = String::with_capacity(s.len());
    for ch in s.chars() {
        let c = ch as u32;
        if in_table(NORM_REMOVED, c) {
        } else if in_table(NORM_SPACE, c) {
            b.push(' ');
        } else if in_table(NORM_CJK, c) {
            b.push(' ');
            b.push(ch);
            b.push(' ');
        } else if (0xAC00..=0xD7A3).contains(&c) {
            let n = c - 0xAC00;
            b.push(char::from_u32(0x1100 + n / 588).unwrap_or(ch));
            b.push(char::from_u32(0x1161 + (n % 588) / 28).unwrap_or(ch));
            let tail = 0x11A7 + n % 28;
            if tail != 0x11A7 {
                b.push(char::from_u32(tail).unwrap_or(ch));
            }
        } else if let Some(m) = mapped(c) {
            b.push_str(m);
        } else {
            b.push(ch);
        }
    }
    b
}

/// BertPreTokenizer: split on white space (dropped), then every
/// punctuation mark is a word of its own.
pub fn pre_tokenize(s: &str) -> Vec<&str> {
    let mut out = vec![];
    let mut start: Option<usize> = None;
    for (i, ch) in s.char_indices() {
        let c = ch as u32;
        if in_table(PRE_SPACE, c) {
            if let Some(st) = start.take() {
                out.push(&s[st..i]);
            }
        } else if in_table(PRE_PUNCT, c) {
            if let Some(st) = start.take() {
                out.push(&s[st..i]);
            }
            out.push(&s[i..i + ch.len_utf8()]);
        } else if start.is_none() {
            start = Some(i);
        }
    }
    if let Some(st) = start {
        out.push(&s[st..]);
    }
    out
}
