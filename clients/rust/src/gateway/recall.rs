//! The assured reuse a harness opts into (ADR-0012 §2C, §2D):
//! `gateway_recall.recall` and `gateway_answers.recall_answer`. Neither runs
//! inside the proxy: the oracle's gateway only captures answers, and a
//! harness reaches these through its MCP tools. They are here so the Rust
//! side has the same library, proven on the oracle's cases.
//!
//! recall fails closed: a reused plan is served only after it verifies
//! against the caller's envelope in the linked Z3, and a Z3 check that
//! answered unknown is a miss here (the oracle's lenient verify would keep
//! it as a warning; GW-11). A typed pathway's holes are bound with one
//! model call (`envgen::bind`, pathway_bind); with no model, or on any
//! failure, it takes the frozen template, as the oracle does (K1-7).

use super::answers::AnswerEntry;
use crate::distill::normalize_task;
use crate::garden::cosine;
use crate::gate::pyjson::Value;
use crate::pathways::find::{Cache, Embedder, FindErr};
use crate::pathways::potion;
use crate::pathways::store::Store;
use crate::pathways::verify::{check_envelope, verify};
use crate::pathways::PwErr;

/// RecallProvenance.
pub struct Provenance {
    pub pathway_id: String,
    pub similarity: f64,
    pub tier: &'static str,
    pub source_trace_count: usize,
    pub distilled_at: Value,
    pub hit_count: Value,
}

/// RecallResult.
pub struct RecallResult {
    pub hit: bool,
    pub reason: Option<&'static str>,
    /// The plan as `model_dump(mode="json")` gives it.
    pub plan: Value,
    pub provenance: Option<Provenance>,
}

fn miss(reason: &'static str) -> RecallResult {
    RecallResult { hit: false, reason: Some(reason), plan: Value::Null, provenance: None }
}

fn pw_text(e: PwErr) -> String {
    match e {
        PwErr::Unreadable(w) | PwErr::Invalid(w) | PwErr::NotYet(w) => w,
        PwErr::Import(c, m) => format!("[{c}] {m}"),
        PwErr::Io(e) => e.to_string(),
    }
}

/// `gateway_recall._DEFAULT_MODEL`, the model that binds.
pub const DEFAULT_MODEL: &str = "anthropic/claude-sonnet-4-20250514";

/// `gateway_recall.recall` for a store and the caller's envelope. `llm` is
/// the model client that binds a typed pathway (None: no model).
pub fn recall(
    store: &Store,
    key: &str,
    pe: &potion::Env,
    task: &str,
    envelope: &Value,
    z3_ms: u32,
    llm: Option<&mut crate::llm::Client>,
    model: &str,
) -> Result<RecallResult, String> {
    check_envelope(envelope).map_err(|e| format!("the caller's envelope does not parse: {}", pw_text(e)))?;
    let mut cache = Cache::default();
    let r = match store.find(task, key, pe, None, &mut cache) {
        Ok(r) => r,
        Err(FindErr::NotCarried(nc)) => return Err(nc.0),
        Err(FindErr::Err(e)) => return Err(pw_text(e)),
    };
    let Some((p, sim)) = r.matched else { return Ok(miss("no matching pathway")) };
    let (tier, tmpl) = match p.obj.value("parameters") {
        Value::List(l) if !l.is_empty() => {
            ("typed", Value::Obj(crate::envgen::bind::bind(llm, &p.obj, task, envelope, model, z3_ms)))
        }
        _ => ("frozen", p.obj.value("plan_template").clone()),
    };
    let out = verify(&tmpl, envelope, None, z3_ms).map_err(pw_text)?;
    if !out.violations.is_empty() || !out.timeouts.is_empty() {
        return Ok(miss("reuse failed verification against your envelope"));
    }
    let srcs = match p.obj.value("source_trace_ids") {
        Value::List(l) => l.len(),
        _ => 0,
    };
    Ok(RecallResult {
        hit: true,
        reason: None,
        plan: tmpl,
        provenance: Some(Provenance {
            pathway_id: p.id().to_string(),
            similarity: sim,
            tier,
            source_trace_count: srcs,
            distilled_at: p.obj.value("distilled_at").clone(),
            hit_count: p.obj.value("hit_count").clone(),
        }),
    })
}

/// DEFAULT_ANSWER_MAX_AGE_SECONDS: seven days.
pub const DEFAULT_MAX_AGE: f64 = 7.0 * 24.0 * 3600.0;

/// `gateway_answers.AnswerProvenance`.
pub struct AnswerProvenance {
    pub similarity: f64,
    pub age_seconds: f64,
    pub created_at: Value,
    pub ground_hash: Value,
}

/// `gateway_answers.AnswerResult`.
pub struct AnswerResult {
    pub hit: bool,
    pub reason: Option<&'static str>,
    pub answer: Value,
    pub provenance: Option<AnswerProvenance>,
}

fn answer_miss(reason: &'static str) -> AnswerResult {
    AnswerResult { hit: false, reason: Some(reason), answer: Value::Null, provenance: None }
}

/// A store whose values the freshness gates cannot read the way the oracle
/// reads them (a task or time that is not the type the gateway writes).
pub const ANSWERS_UNREAD: &str = "the answer store holds an entry this binary does not read";

fn float_of(v: &Value) -> Option<f64> {
    match v {
        Value::Float(f) => Some(*f),
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

/// `recall_answer`: the nearest past answer by the ask, served only when it
/// clears confidence, age and ground-shift.
pub fn recall_answer(
    task: &str,
    entries: &[AnswerEntry],
    now: f64,
    emb: &Embedder,
    threshold: f64,
    max_age: f64,
    ground: Option<&str>,
) -> Result<AnswerResult, String> {
    let cands: Vec<&AnswerEntry> = entries.iter().filter(|e| e.signature.truthy() && e.answer.truthy()).collect();
    if cands.is_empty() {
        return Ok(answer_miss("no answers in store"));
    }
    let q = emb.encode(&normalize_task(task));
    // The first best similarity above -1; with none, index -1: the last.
    let (mut best, mut best_sim) = (cands.len() - 1, -1.0f64);
    for c in &cands {
        if !matches!(c.task, Value::Str(_)) {
            return Err(ANSWERS_UNREAD.into());
        }
    }
    for (i, c) in cands.iter().enumerate() {
        let sim = cosine(&q, &emb.encode(&normalize_task(c.task.as_str().unwrap_or_default())));
        if sim > best_sim {
            best = i;
            best_sim = sim;
        }
    }
    let e = cands[best];
    if best_sim < threshold {
        return Ok(answer_miss("no sufficiently similar past answer"));
    }
    let created = float_of(&e.created_at).ok_or_else(|| ANSWERS_UNREAD.to_string())?;
    let age = now - created;
    if age > max_age {
        return Ok(answer_miss("cached answer too old"));
    }
    if let Some(g) = ground {
        if !matches!(e.ground_hash, Value::Null) && e.ground_hash.as_str() != Some(g) {
            return Ok(answer_miss("the answer's ground has changed"));
        }
    }
    Ok(AnswerResult {
        hit: true,
        reason: None,
        answer: e.answer.clone(),
        provenance: Some(AnswerProvenance {
            similarity: best_sim,
            age_seconds: age,
            created_at: e.created_at.clone(),
            ground_hash: e.ground_hash.clone(),
        }),
    })
}
