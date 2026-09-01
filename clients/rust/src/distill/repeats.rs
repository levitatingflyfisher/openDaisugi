//! `gateway_journal.load`, `gateway_cluster.cluster_repeats` and
//! `gateway_distill.rank_reuse_candidates`: the ranked reuse worklist
//! `distill-repeats` prints.

use std::collections::HashMap;

use crate::garden::{cosine, format_f};
use crate::gate::py::text::{split, strip};
use crate::gate::pyjson::{loads_py, Value};
use crate::pathways::find::Embedder;
use crate::pathways::PwErr;

use super::normalize_task;

/// The part of a GatewayTurnRecord the worklist reads.
#[derive(Clone)]
pub struct Turn {
    pub signature: String,
    pub task: String,
    pub tokens: i128,
    pub dollars: f64,
}

const REQUIRED: &[&str] = &[
    "created_at",
    "signature",
    "task",
    "tier",
    "requested_model",
    "model",
    "difficulty",
    "downgraded",
    "estimated",
    "input_tokens",
    "output_tokens",
    "frontier_tokens_saved",
    "actual_dollars",
    "counterfactual_dollars",
];

/// `GatewayJournal(path).load()`: each line a record, a line that is not
/// JSON or not a record skipped. A record whose values the worklist cannot
/// add up the Python way is refused.
pub fn load_turns(path: &str) -> Result<Vec<Turn>, PwErr> {
    let raw = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(vec![]),
        Err(e) => return Err(PwErr::Io(e)),
    };
    let text = String::from_utf8(raw).map_err(|_| PwErr::Unreadable("the gateway journal is not UTF-8".into()))?;
    let mut out = vec![];
    for line in crate::gate::py::text::splitlines(&text) {
        let line = strip(line);
        if line.is_empty() {
            continue;
        }
        let o = match loads_py(line, 900) {
            Ok(Value::Obj(o)) => o,
            _ => continue,
        };
        if REQUIRED.iter().any(|f| o.get(f).is_none()) {
            continue;
        }
        out.push(turn_of(&o)?);
    }
    Ok(out)
}

fn turn_of(o: &crate::gate::pyjson::Object) -> Result<Turn, PwErr> {
    let bad = |what: &str| PwErr::Unreadable(format!("a gateway turn's {what} is not the type the worklist adds up"));
    let (sig, task) = match (o.value("signature"), o.value("task")) {
        (Value::Str(s), Value::Str(t)) => (s.clone(), t.clone()),
        _ => return Err(bad("signature or task")),
    };
    let mut tokens: i128 = 0;
    for f in ["input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens"] {
        match o.get(f) {
            None => {}
            Some(Value::Int(t)) => {
                let n: i128 = t.parse().map_err(|_| bad(f))?;
                tokens = tokens.checked_add(n).ok_or_else(|| bad(f))?;
            }
            Some(Value::Bool(b)) => tokens += *b as i128,
            Some(_) => return Err(bad(f)),
        }
    }
    let dollars = match o.value("actual_dollars") {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse::<f64>().map_err(|_| bad("actual_dollars"))?,
        _ => return Err(bad("actual_dollars")),
    };
    Ok(Turn { signature: sig, task, tokens, dollars })
}

/// A ReuseCandidate with its cluster.
pub struct Candidate {
    pub representative: String,
    pub count: usize,
    pub tokens: i128,
    pub dollars: f64,
    pub reusable: bool,
}

/// `rank_reuse_candidates(records, pathway_store=find)`: the repeat
/// clusters of the signed turns, ranked by tokens then dollars.
pub fn rank_reuse(turns: &[Turn], emb: &Embedder, threshold: f64, find: &mut dyn FnMut(&str) -> bool) -> Vec<Candidate> {
    let signed: Vec<&Turn> = turns.iter().filter(|t| !t.signature.is_empty()).collect();
    if signed.is_empty() {
        return vec![];
    }
    let mut counts: HashMap<&str, usize> = HashMap::new();
    let mut task_by_sig: HashMap<&str, &str> = HashMap::new();
    let mut distinct: Vec<&str> = vec![];
    for t in &signed {
        let c = counts.entry(&t.signature).or_insert(0);
        if *c == 0 {
            distinct.push(&t.signature);
        }
        *c += 1;
        task_by_sig.insert(&t.signature, &t.task);
    }
    let vec: HashMap<&str, Vec<f64>> = distinct.iter().map(|s| (*s, emb.encode(&normalize_task(task_by_sig[s])))).collect();
    let mut members: Vec<Vec<&str>> = vec![];
    let mut centroids: Vec<Vec<f64>> = vec![];
    for s in &distinct {
        let v = &vec[s];
        let (mut best, mut best_sim) = (None, -1.0f64);
        for (i, c) in centroids.iter().enumerate() {
            let sim = cosine(v, c);
            if sim > best_sim {
                best = Some(i);
                best_sim = sim;
            }
        }
        match best {
            Some(b) if best_sim >= threshold => {
                members[b].push(s);
                let mut c = vec![0.0; v.len()];
                for m in &members[b] {
                    for (j, x) in vec[m].iter().enumerate() {
                        if j < c.len() {
                            c[j] += x;
                        }
                    }
                }
                let n = members[b].len() as f64;
                centroids[b] = c.into_iter().map(|x| x / n).collect();
            }
            _ => {
                members.push(vec![s]);
                centroids.push(v.clone());
            }
        }
    }
    struct Cl<'a> {
        rep: &'a str,
        sigs: Vec<&'a str>,
        count: usize,
    }
    let mut clusters: Vec<Cl> = vec![];
    for ms in &members {
        let total: usize = ms.iter().map(|s| counts[s]).sum();
        if total <= 1 {
            continue;
        }
        let mut rep = ms[0];
        for s in &ms[1..] {
            if counts[s] > counts[rep] {
                rep = s;
            }
        }
        clusters.push(Cl { rep: task_by_sig[rep], sigs: ms.clone(), count: total });
    }
    clusters.sort_by(|a, b| b.count.cmp(&a.count));
    let mut by_sig: HashMap<&str, Vec<&Turn>> = HashMap::new();
    for t in turns.iter().filter(|t| !t.signature.is_empty()) {
        by_sig.entry(&t.signature).or_default().push(t);
    }
    let mut out = vec![];
    for c in &clusters {
        let mut tokens: i128 = 0;
        let mut dollars = 0.0;
        for s in &c.sigs {
            for t in by_sig.get(s).map(|v| v.as_slice()).unwrap_or(&[]) {
                tokens += t.tokens;
            }
        }
        for s in &c.sigs {
            for t in by_sig.get(s).map(|v| v.as_slice()).unwrap_or(&[]) {
                dollars += t.dollars;
            }
        }
        out.push(Candidate { representative: c.rep.to_string(), count: c.count, tokens, dollars, reusable: find(c.rep) });
    }
    // sort(key=(tokens, dollars), reverse=True) keeps equal items in order.
    out.sort_by(|a, b| {
        b.tokens.cmp(&a.tokens).then_with(|| b.dollars.partial_cmp(&a.dollars).unwrap_or(std::cmp::Ordering::Equal))
    });
    out
}

impl Candidate {
    /// One printed worklist line.
    pub fn row(&self, rank: usize) -> String {
        let mut task = split(&self.representative).join(" ");
        if task.chars().count() > 70 {
            task = format!("{}...", task.chars().take(67).collect::<String>());
        }
        let reusable = if self.reusable { "yes" } else { "no" };
        format!(
            "{rank:4}  {:4}  {:>10}  ${:>8}  {reusable:>9}  {task}",
            self.count,
            commas(self.tokens),
            dollar_text(self.dollars)
        )
    }
}

/// `format(n, ",")`.
fn commas(n: i128) -> String {
    let s = n.unsigned_abs().to_string();
    let mut b = String::new();
    for (i, c) in s.chars().enumerate() {
        if i > 0 && (s.len() - i) % 3 == 0 {
            b.push(',');
        }
        b.push(c);
    }
    if n < 0 {
        format!("-{b}")
    } else {
        b
    }
}

fn dollar_text(x: f64) -> String {
    format_f(x, 2)
}
