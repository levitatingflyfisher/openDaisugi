//! `PathwayStore.find(task)` with the configured matcher.

use super::pathway::{from_row, Emb, Pathway};
use super::potion;
use super::scan::{scan_embedding, to_sparse};
use super::store::{sql_err, Store};
use super::{lexical, PwErr};

/// `distiller._EMBEDDING_MODEL_VERSION`: the second half of a row's
/// provenance stamp.
pub const EMBEDDING_MODEL_VERSION: &str = "3";

/// The config keys of the backends this binary does not carry.
const MINILM: &str = "all-MiniLM-L6-v2";
const INT8: &str = "int8";

/// The backend a `matcher_model` config key selects.
pub enum Matcher {
    Lexical,
    Potion { identity: String },
}

impl Matcher {
    pub fn identity(&self) -> &str {
        match self {
            Matcher::Lexical => lexical::IDENTITY,
            Matcher::Potion { identity } => identity,
        }
    }

    pub fn threshold(&self) -> f64 {
        match self {
            Matcher::Lexical => lexical::THRESHOLD,
            Matcher::Potion { .. } => potion::THRESHOLD,
        }
    }
}

/// A matcher this binary does not carry. It is refused, never answered
/// with another matcher.
#[derive(Debug)]
pub struct NotCarried(pub String);

/// `effective_matcher` with `active_model_name`, for a binary that
/// carries lexical and potion. MiniLM and int8 are refused. Any other key
/// is a matcher nothing builds: Python's find answers None, and so does
/// this (`Ok(None)`).
pub fn select_matcher(key: &str, pe: &potion::Env) -> Result<Option<Matcher>, NotCarried> {
    match key {
        "lexical" => Ok(Some(Matcher::Lexical)),
        "potion" => Ok(Some(Matcher::Potion { identity: potion::resolve_model(pe) })),
        MINILM | INT8 => {
            let need = if key == INT8 { "onnxruntime" } else { "torch (sentence-transformers)" };
            Err(NotCarried(format!(
                "matcher not in this binary: matcher_model {} needs {need}, which this binary does not carry. \
                 Set matcher_model: lexical (no model) or potion (no torch) in config.yaml",
                go_quote(key)
            )))
        }
        _ => Ok(None),
    }
}

/// Go's `%q` of a plain string.
fn go_quote(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            '\r' => out.push_str("\\r"),
            c if (c as u32) < 0x20 || c as u32 == 0x7f => out.push_str(&format!("\\x{:02x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// What find did: the match, or none, and the one-time stale warning.
#[derive(Default)]
pub struct FindResult {
    pub matched: Option<(Pathway, f64)>,
    /// The stale-embeddings UserWarning text, or "".
    pub warning: String,
    /// Why there is no match when a backend could not run (Python's find
    /// says nothing; the probe reports it).
    pub reason: String,
}

pub enum FindErr {
    NotCarried(NotCarried),
    Err(PwErr),
}

impl From<PwErr> for FindErr {
    fn from(e: PwErr) -> Self {
        FindErr::Err(e)
    }
}

/// A loaded embedder.
pub enum Embedder {
    Lexical,
    Potion(Box<potion::Model>),
}

impl Embedder {
    pub fn encode(&self, text: &str) -> Vec<f64> {
        match self {
            Embedder::Lexical => lexical::encode(text),
            Embedder::Potion(m) => m.encode(text),
        }
    }
}

/// Holds the loaded model between finds, as `_embedder_cache` does.
#[derive(Default)]
pub struct Cache {
    potion: Option<(String, Box<potion::Model>)>,
}

impl Store {
    /// `PathwayStore.find(task)` with the configured backend. A threshold
    /// of `None` is the backend's own.
    pub fn find(
        &self,
        task: &str,
        key: &str,
        pe: &potion::Env,
        threshold: Option<f64>,
        cache: &mut Cache,
    ) -> Result<FindResult, FindErr> {
        if !self.any()? {
            return Ok(FindResult::default());
        }
        let m = match select_matcher(key, pe).map_err(FindErr::NotCarried)? {
            Some(m) => m,
            None => {
                return Ok(FindResult {
                    reason: format!("matcher_model {} is not a built embedder", go_quote(key)),
                    ..Default::default()
                })
            }
        };
        let threshold = threshold.unwrap_or_else(|| m.threshold());
        let q = match &m {
            Matcher::Lexical => lexical::encode(task),
            Matcher::Potion { identity } => {
                let hit = matches!(&cache.potion, Some((id, _)) if id == identity);
                if !hit {
                    match potion::open(identity, pe) {
                        Ok(model) => cache.potion = Some((identity.clone(), Box::new(model))),
                        Err(potion::NotAvailable(why)) => return Ok(FindResult { reason: why, ..Default::default() }),
                    }
                }
                match &cache.potion {
                    Some((_, model)) => model.encode(task),
                    None => return Ok(FindResult::default()),
                }
            }
        };
        self.find_in(&q, m.identity(), threshold).map_err(FindErr::Err)
    }

    fn find_in(&self, q: &[f64], identity: &str, threshold: f64) -> Result<FindResult, PwErr> {
        struct Scored {
            rowid: i64,
            score: f64,
            keep: bool,
            invalid: bool,
        }
        // numpy's norm of a vector is BLAS ddot, whose order of sums
        // depends on the CPU's kernel; the pairwise sum is the nearer of
        // the orders this binary can know.
        let qn = to_sparse(q).sumsq.sqrt();
        let parts = self.scan_parts("rowid, embedding_model, embedding_model_version, task_embedding_json", &|rows| {
            let mut out = vec![];
            let mut n = 0usize;
            while let Some(r) = rows.next().map_err(sql_err)? {
                n += 1;
                let rowid: i64 = r.get(0).map_err(sql_err)?;
                use rusqlite::types::ValueRef;
                let (ms, vs) = match (r.get_ref(1).map_err(sql_err)?, r.get_ref(2).map_err(sql_err)?) {
                    (ValueRef::Text(a), ValueRef::Text(b)) => (a, b),
                    _ => return Err(PwErr::Unreadable("a provenance column is not text".into())),
                };
                let compatible = (ms.is_empty() && vs.is_empty())
                    || (ms == identity.as_bytes() && vs == EMBEDDING_MODEL_VERSION.as_bytes());
                if !compatible {
                    continue;
                }
                let mut sc = Scored { rowid, score: 0.0, keep: false, invalid: false };
                match r.get_ref(3).map_err(sql_err)? {
                    ValueRef::Text(t) => match scan_embedding(t) {
                        None => sc.invalid = true,
                        Some(sp) => {
                            if sp.n == q.len() {
                                sc.keep = true;
                                let mut den = sp.sumsq.sqrt() * qn;
                                if den == 0.0 {
                                    den = 1e-9;
                                }
                                // clamp keeps a NaN, as numpy's clip does.
                                sc.score = (sp.dot(q) / den).clamp(-1.0, 1.0);
                            }
                        }
                    },
                    _ => sc.invalid = true,
                }
                out.push(sc);
            }
            Ok((out, n))
        })?;
        let mut total = 0usize;
        let mut results: Vec<Scored> = vec![];
        for (part, n) in parts {
            total += n;
            results.extend(part);
        }
        if results.is_empty() {
            return Ok(FindResult::default());
        }
        let mut res = FindResult::default();
        let stale = 1.0 - results.len() as f64 / total as f64;
        if stale >= 0.10 {
            res.warning = format!(
                "{} pathway(s) ({} of store) were embedded under a different model/version than the current \
                 distiller. They are excluded from find() — semantic comparison across embedding spaces would \
                 return wrong matches. Run `daisugi tend` to re-embed.",
                total - results.len(),
                percent(stale)
            );
        }
        let mut best: Option<usize> = None;
        for (i, sc) in results.iter().enumerate() {
            if sc.invalid {
                return Err(PwErr::Invalid(
                    "pydantic_core._pydantic_core.ValidationError: a stored embedding is not a list of numbers".into(),
                ));
            }
            if !sc.keep {
                continue;
            }
            match best {
                None => best = Some(i),
                Some(b) => {
                    let bs = results[b].score;
                    if sc.score > bs || (sc.score.is_nan() && !bs.is_nan()) {
                        best = Some(i);
                    }
                }
            }
        }
        let b = match best {
            Some(b) if !(results[b].score < threshold) => b,
            _ => return Ok(res),
        };
        let score = results[b].score;
        if score.is_nan() {
            return Err(PwErr::Unreadable("a similarity is NaN".into()));
        }
        let row = match self.row_by_rowid(results[b].rowid)? {
            Some(r) => r,
            None => return Err(PwErr::Invalid("RuntimeError: the matched row went away".into())),
        };
        let emb = match row.get("task_embedding_json") {
            Some(super::store::Col::Text(t)) => Emb::Text { ok: scan_embedding(t.as_bytes()).is_some(), text: Some(t.clone()) },
            Some(other) => Emb::Other(other.clone()),
            None => return Err(PwErr::Unreadable("the row has no task_embedding_json column".into())),
        };
        let p = from_row(&row, &emb)?;
        res.matched = Some((p, score));
        Ok(res)
    }
}

/// Python's `f"{x:.0%}"`: round half to even on the exact value.
fn percent(x: f64) -> String {
    format!("{:.0}%", x * 100.0)
}
