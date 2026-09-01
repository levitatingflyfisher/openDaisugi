//! The supervised run's side of the journal: `Journal.log_run`,
//! `append_receipt`, `receipts_for_run` and `write_refinement`. The Go
//! client's `tracejournal/runs.go` is the reference.

use std::collections::BTreeSet;

use rusqlite::params;

use super::{dag, unreadable, Journal};
use crate::gate::pyjson::{any_json, dumps, Object, Value};
use crate::pathways::store::{sql_err, Col};
use crate::pathways::yamldump::safe_dump;
use crate::pathways::PwErr;

/// What log_run reads off a RunSession.
pub struct Run<'a> {
    pub task: &'a str,
    /// `model_dump()` values of the envelope and the plan.
    pub env: &'a Object,
    pub plan: &'a Object,
    /// JSON dumps: the verification, and the session as asdict writes it.
    pub verification: &'a Object,
    pub session: Object,
    pub run_id: &'a str,
    pub status: &'a str,
    /// The first step whose status is "failed".
    pub failed_step_id: Option<String>,
    pub total_duration_ms: f64,
}

/// The YAML body log_run writes.
pub fn run_body(r: &Run, trace_id: &str, created_at: &str) -> Result<String, PwErr> {
    let payload = Object::new()
        .with("id", trace_id)
        .with("created_at", created_at)
        .with("task", r.task)
        .with("envelope", any_json(&Value::Obj(r.env.clone())))
        .with("plan", any_json(&Value::Obj(r.plan.clone())))
        .with("result", Value::Obj(r.verification.clone()))
        .with("run", Value::Obj(r.session.clone()));
    safe_dump(&Value::Obj(payload)).map_err(|w| unreadable(w.0))
}

/// `Receipt` as append_receipt writes it.
pub struct Receipt {
    pub step_id: String,
    pub run_id: String,
    pub timestamp: f64,
    pub evidence: Object,
    pub evidence_hash: String,
    pub verify_result: bool,
    pub verify_details: String,
    pub model_id: Option<String>,
    pub effect_class: String,
    pub reversibility: String,
    pub reversal_json: Option<String>,
}

fn float_of(v: &Value) -> f64 {
    match v {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse::<f64>().unwrap_or(0.0),
        _ => 0.0,
    }
}

impl Journal {
    /// `Journal.log_run`: the YAML body first, then the index row with the
    /// run's columns. A failed insert removes the body.
    pub fn log_run(&self, r: &Run, trace_id: &str, created_at: &str) -> Result<(), PwErr> {
        let text = run_body(r, trace_id, created_at)?;
        let path = self.trace_path(trace_id);
        std::fs::write(&path, text).map_err(PwErr::Io)?;
        let sig = dag::structure_signature(r.plan);
        let ok = matches!(r.verification.value("ok"), Value::Bool(true)) as i64;
        let violations = match r.verification.value("violations") {
            Value::List(l) => Value::List(l.clone()),
            _ => Value::List(vec![]),
        };
        let res = self.con.execute(
            "INSERT INTO traces \
             (id, created_at, task, plan_id, envelope_id, ok, duration_ms, \
              violations_json, run_id, run_status, failed_step_id, \
              total_duration_ms, structure_signature) \
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            params![
                trace_id,
                created_at,
                r.task,
                r.plan.value("id").as_str().unwrap_or(""),
                r.env.value("id").as_str().unwrap_or(""),
                ok,
                float_of(r.verification.value("duration_ms")),
                dumps(&violations, true),
                r.run_id,
                r.status,
                r.failed_step_id,
                r.total_duration_ms,
                sig
            ],
        );
        if let Err(e) = res {
            let _ = std::fs::remove_file(&path);
            return Err(sql_err(e));
        }
        Ok(())
    }

    /// `Journal.append_receipt`: INSERT OR REPLACE on (run_id, step_id).
    pub fn append_receipt(&self, r: &Receipt) -> Result<(), PwErr> {
        self.con
            .execute(
                "INSERT OR REPLACE INTO receipts \
                 (run_id, step_id, timestamp, evidence_hash, verify_result, \
                 verify_details, evidence_json, model_id, \
                 effect_class, reversibility, reversal_json) \
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                params![
                    r.run_id,
                    r.step_id,
                    r.timestamp,
                    r.evidence_hash,
                    r.verify_result as i64,
                    r.verify_details,
                    dumps(&Value::Obj(r.evidence.clone()), true),
                    r.model_id,
                    r.effect_class,
                    r.reversibility,
                    r.reversal_json
                ],
            )
            .map(|_| ())
            .map_err(sql_err)
    }

    /// The step ids of `receipts_for_run(run_id)`.
    pub fn receipt_steps(&self, run_id: &str) -> Result<BTreeSet<String>, PwErr> {
        let mut st =
            self.con.prepare("SELECT step_id FROM receipts WHERE run_id = ? ORDER BY timestamp ASC").map_err(sql_err)?;
        let rows = st.query_map([run_id], |r| r.get::<_, String>(0)).map_err(sql_err)?;
        rows.collect::<Result<_, _>>().map_err(sql_err)
    }

    /// `Journal.write_refinement`: best effort; a failure is dropped as the
    /// oracle logs and drops it.
    pub fn write_refinement(&self, session_id: &str, record_json: &str, cache_key: &Value) {
        let key: Option<String> = cache_key.as_str().map(String::from);
        let _ = self.con.execute(
            "INSERT INTO refinement_log (session_id, record_json, inserted_at, cache_key) VALUES (?, ?, ?, ?)",
            params![session_id, record_json, now(), key],
        );
    }
}

/// One row `receipts_for_run` reads, as the Receipt model holds it.
#[derive(Debug, Clone)]
pub struct ReceiptRow {
    pub step_id: String,
    pub run_id: String,
    pub timestamp: f64,
    pub evidence_hash: String,
    pub verify_result: bool,
    pub verify_details: String,
    /// A str or None.
    pub model_id: Option<String>,
    /// Each a str or None.
    pub effect_class: Option<String>,
    pub reversibility: Option<String>,
    /// The reversal handle as its JSON text reads, or None.
    pub reversal: Option<Object>,
    /// The evidence object as its JSON text reads.
    pub evidence: Object,
}

/// A receipts row the Receipt model would not read the way this binary
/// reads it.
fn receipt_row_err() -> PwErr {
    PwErr::Unreadable("a receipts row this binary does not read as a Receipt".into())
}

/// A TEXT column that must hold a JSON object.
fn json_object_col(c: &Col) -> Result<Object, PwErr> {
    let Col::Text(t) = c else { return Err(receipt_row_err()) };
    match crate::gate::pyjson::loads(t) {
        Ok(Value::Obj(o)) => Ok(o),
        _ => Err(receipt_row_err()),
    }
}

impl Journal {
    /// `receipts_for_run(run_id)`: the run's receipts, oldest first.
    pub fn receipts(&self, run_id: &str) -> Result<Vec<ReceiptRow>, PwErr> {
        let mut st = self
            .con
            .prepare(
                "SELECT step_id, run_id, timestamp, evidence_json, evidence_hash, \
                 verify_result, verify_details, model_id, \
                 effect_class, reversibility, reversal_json FROM receipts WHERE run_id = ? \
                 ORDER BY timestamp ASC",
            )
            .map_err(sql_err)?;
        let mut rows = st.query([run_id]).map_err(sql_err)?;
        let mut out = vec![];
        while let Some(r) = rows.next().map_err(sql_err)? {
            let mut c = Vec::with_capacity(11);
            for i in 0..11 {
                c.push(Col::from_ref(r.get_ref(i).map_err(sql_err)?)?);
            }
            let text = |c: &Col| match c {
                Col::Text(t) => Ok(t.clone()),
                _ => Err(receipt_row_err()),
            };
            let timestamp = match c[2] {
                Col::Real(f) => f,
                Col::Int(i) => i as f64,
                _ => return Err(receipt_row_err()),
            };
            let Col::Int(vr) = c[5] else { return Err(receipt_row_err()) };
            let model_id = match &c[7] {
                Col::Null => None,
                Col::Text(t) => Some(t.clone()),
                _ => return Err(receipt_row_err()),
            };
            let evidence = json_object_col(&c[3])?;
            for x in [&c[8], &c[9]] {
                if !matches!(x, Col::Null | Col::Text(_)) {
                    return Err(receipt_row_err());
                }
            }
            let reversal = match &c[10] {
                Col::Null => None,
                Col::Text(t) if t.is_empty() => None,
                // A reversal handle: the model reads it; this binary
                // checks only that it is a JSON object, the form the
                // supervisor writes.
                Col::Text(t) => match crate::gate::pyjson::loads(t) {
                    Ok(Value::Obj(o)) => Some(o),
                    _ => return Err(receipt_row_err()),
                },
                _ => return Err(receipt_row_err()),
            };
            let opt_text = |c: &Col| match c {
                Col::Text(t) => Some(t.clone()),
                _ => None,
            };
            out.push(ReceiptRow {
                step_id: text(&c[0])?,
                run_id: text(&c[1])?,
                timestamp,
                evidence_hash: text(&c[4])?,
                verify_result: vr != 0,
                verify_details: text(&c[6])?,
                model_id,
                effect_class: opt_text(&c[8]),
                reversibility: opt_text(&c[9]),
                reversal,
                evidence,
            });
        }
        Ok(out)
    }
}

/// `time.time()`.
pub fn now() -> f64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0)
}
