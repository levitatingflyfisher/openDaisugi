//! recall: find where a word was said. The twin of Go's pando recall.go.
//!
//! The matcher is the lexical one (PD-R-5), `crate::lexical::vector`. The
//! caller hands in its vector function (the CLI and coppice-rs both give
//! that one); this crate has no model.

use super::chat::{flat, Chat};
use super::fsutil::clip;
use super::order::NodeId;

/// Turns a text into an unnormalized vector.
pub type Embed = fn(&str) -> Vec<f64>;

/// One answer of recall: a node id (first+count), a score and one line of
/// text. It is never a whole raw message: the model zooms next.
#[derive(Clone, Debug, PartialEq)]
pub struct RecallHit {
    pub id: String,
    pub score: f64,
    pub line: String,
}

/// Caps the text of one hit.
pub const RECALL_LINE_BYTES: usize = 160;

/// The most hits one recall gives.
pub const RECALL_MAX: usize = 32;

fn norm(v: &[f64]) -> f64 {
    let mut s = 0.0;
    for x in v {
        s += x * x;
    }
    s.sqrt()
}

fn cosine(a: &[f64], b: &[f64], na: f64, nb: f64) -> f64 {
    if na == 0.0 || nb == 0.0 {
        return 0.0;
    }
    let mut d = 0.0;
    for i in 0..a.len() {
        d += a[i] * b[i];
    }
    d / (na * nb)
}

/// One hit before the cut: the branch it is in ("" for the store itself),
/// its node and the hit.
pub struct RecallCand {
    pub branch: String,
    pub first: i64,
    pub count: i64,
    pub hit: RecallHit,
}

/// Orders hits: the best score first, then the store before its branches
/// (by branch path), then the older id, then the smaller node.
pub fn sort_cands(all: &mut [RecallCand]) {
    all.sort_by(|a, b| {
        b.hit
            .score
            .partial_cmp(&a.hit.score)
            .unwrap_or(std::cmp::Ordering::Equal)
            .then(a.branch.cmp(&b.branch))
            .then(a.first.cmp(&b.first))
            .then(a.count.cmp(&b.count))
    });
}

impl Chat {
    /// Ranks the view's summary lines and the raw messages of the chat by
    /// the matcher and gives the best k. A hit with a score of 0 is left
    /// out. Ties go by id: the older first. A message that is its own
    /// level-0 view line counts once, as the message.
    pub fn recall(&self, query: &str, k: usize, embed: Embed) -> Vec<RecallHit> {
        let k = if k == 0 { 8 } else { k.min(RECALL_MAX) };
        let mut all = self.recall_cands(query, embed);
        all.truncate(k);
        all.into_iter().map(|c| c.hit).collect()
    }

    /// Every hit of the chat, best first.
    pub fn recall_cands(&self, query: &str, embed: Embed) -> Vec<RecallCand> {
        let q = embed(query);
        let nq = norm(&q);
        let mut all: Vec<RecallCand> = Vec::new();
        let mut add = |first: i64, count: i64, id: String, text: &str| {
            let v = embed(text);
            let s = cosine(&q, &v, nq, norm(&v));
            if s <= 0.0 {
                return;
            }
            let mut line = flat(text);
            if line.len() > RECALL_LINE_BYTES {
                line = clip(&line, RECALL_LINE_BYTES).to_string();
            }
            all.push(RecallCand {
                branch: String::new(),
                first,
                count,
                hit: RecallHit { id, score: s, line },
            });
        };
        for id in self.view_lines() {
            if id.l == 0 {
                continue;
            }
            if let Some(n) = self.node(id) {
                add(id.first(), id.count(), id.name(), &n.text);
            }
        }
        for i in 0..self.count() {
            if let Ok(m) = self.message(i) {
                add(i, 1, NodeId { l: 0, i }.name(), &m.text);
            }
        }
        sort_cands(&mut all);
        all
    }
}

/// "id<TAB>score<TAB>line", the score to three places.
pub fn format_hit(h: &RecallHit) -> String {
    format!("{}\t{:.3}\t{}", h.id, h.score, h.line)
}
