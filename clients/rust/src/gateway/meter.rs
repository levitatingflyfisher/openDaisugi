//! The meter of `opendaisugi.gateway` (`price_turn`, `measure_turn`,
//! `_strictly_cheaper`) and `gateway_journal.record_turn`.

use std::collections::HashMap;

use num_bigint::BigInt;

use super::pyops::{get, hashable, int_to_float, py_equal, py_int, PyR, Raise};
use super::route::{sha256_hex16, Decision};
use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

/// (input, output) USD per million tokens.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Price {
    pub input: f64,
    pub output: f64,
}

/// A price table; its keys are model ids.
pub type Prices = HashMap<String, Price>;

/// `_PRICES_PER_MTOK`.
pub fn default_prices() -> Prices {
    let mut p = Prices::new();
    p.insert("claude-opus-4-8".into(), Price { input: 15.0, output: 75.0 });
    p.insert("claude-sonnet-5".into(), Price { input: 3.0, output: 15.0 });
    p.insert("claude-haiku-4-5".into(), Price { input: 1.0, output: 5.0 });
    p
}

const FALLBACK: Price = Price { input: 3.0, output: 15.0 };
const CACHE_READ_MULT: f64 = 0.1;
const CACHE_WRITE_MULT: f64 = 1.25;

/// `prices.get(model, fallback)`: an unhashable model raises.
fn lookup(p: &Prices, model: &Value) -> PyR<Price> {
    if !hashable(model) {
        return Err(Raise::Raise);
    }
    if let Value::Str(s) = model {
        if let Some(pr) = p.get(s) {
            return Ok(*pr);
        }
    }
    Ok(FALLBACK)
}

/// `model in prices`: an unhashable model raises.
fn has(p: &Prices, model: &Value) -> PyR<Option<Price>> {
    if !hashable(model) {
        return Err(Raise::Raise);
    }
    Ok(match model {
        Value::Str(s) => p.get(s).copied(),
        _ => None,
    })
}

/// `_strictly_cheaper`: both models priced and the served one cheaper on
/// input and on output.
pub fn strictly_cheaper(p: &Prices, served: &Value, requested: &Value) -> PyR<bool> {
    let Some(s) = has(p, served)? else { return Ok(false) };
    let Some(r) = has(p, requested)? else { return Ok(false) };
    Ok(s.input < r.input && s.output < r.output)
}

/// TurnCost.
#[derive(Debug, Clone)]
pub struct Cost {
    pub model: Value,
    pub input: BigInt,
    pub output: BigInt,
    pub cache_read: BigInt,
    pub cache_creation: BigInt,
    pub dollars: f64,
}

/// `price_turn`: every bucket priced, in Python's order of operations.
fn price_turn(p: &Prices, model: &Value, i: &BigInt, o: &BigInt, cr: &BigInt, cc: &BigInt) -> PyR<Cost> {
    let pr = lookup(p, model)?;
    let fin = int_to_float(i)?;
    let fcr = int_to_float(cr)?;
    let fcc = int_to_float(cc)?;
    let fout = int_to_float(o)?;
    let a = fin * pr.input;
    let b = (fcr * pr.input) * CACHE_READ_MULT;
    let c = (fcc * pr.input) * CACHE_WRITE_MULT;
    let d = fout * pr.output;
    let dollars = (((a + b) + c) + d) / 1_000_000.0;
    Ok(Cost {
        model: model.clone(),
        input: i.clone(),
        output: o.clone(),
        cache_read: cr.clone(),
        cache_creation: cc.clone(),
        dollars,
    })
}

/// TurnSaving.
#[derive(Debug, Clone)]
pub struct Saving {
    pub actual: Cost,
    pub counterfactual: Cost,
    pub estimated: bool,
}

impl Saving {
    /// `frontier_tokens_saved`: the whole turn when the served model differs
    /// from the counterfactual's.
    pub fn frontier_tokens_saved(&self) -> BigInt {
        if py_equal(&self.actual.model, &self.counterfactual.model) {
            return BigInt::from(0);
        }
        let a = &self.actual;
        &a.input + &a.cache_read + &a.cache_creation + &a.output
    }
}

/// `measure_turn`: the usage block's four buckets, `int()` each.
pub fn measure(p: &Prices, d: &Decision, usage: &Value) -> PyR<Saving> {
    let mut ints = vec![];
    for k in ["input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"] {
        let n = match get(usage, k)? {
            Some(v) => py_int(v)?,
            None => BigInt::from(0),
        };
        ints.push(n);
    }
    let actual = price_turn(p, &d.model, &ints[0], &ints[1], &ints[2], &ints[3])?;
    if !d.downgraded {
        return Ok(Saving { counterfactual: actual.clone(), actual, estimated: false });
    }
    let cf = price_turn(p, &d.requested_model, &ints[0], &ints[1], &ints[2], &ints[3])?;
    Ok(Saving { actual, counterfactual: cf, estimated: true })
}

/// `turn_signature`: SHA-256 of the stripped, lowered, whitespace-collapsed
/// ask, 16 hex digits. A lone surrogate raises.
pub fn turn_signature(task: &str) -> PyR<String> {
    let low = text::lower(text::strip(task));
    let mut norm = String::with_capacity(low.len());
    let mut in_run = false;
    for c in low.chars() {
        if text::is_space(c) {
            if !in_run {
                norm.push(' ');
            }
            in_run = true;
        } else {
            norm.push(c);
            in_run = false;
        }
    }
    Ok(sha256_hex16(super::pyops::encode_utf8(&norm)?))
}

/// GatewayTurnRecord, as the journal line holds it.
#[derive(Debug, Clone)]
pub struct Record {
    pub created_at: String,
    pub signature: String,
    pub task: String,
    pub tier: String,
    pub requested_model: Value,
    pub model: Value,
    pub difficulty: f64,
    pub downgraded: bool,
    pub estimated: bool,
    pub input: BigInt,
    pub output: BigInt,
    pub frontier_saved: BigInt,
    pub actual: f64,
    pub counterfactual: f64,
    pub cache_read: BigInt,
    pub cache_creation: BigInt,
    /// elapsed_ms: the turn's time in the proxy, or None.
    pub elapsed_ms: Option<f64>,
}

/// `record_turn` with created_at stamped now (UTC seconds).
pub fn record_turn(d: &Decision, s: &Saving, task: &str, ask: &str, now_secs: i64) -> PyR<Record> {
    let sig = if text::strip(ask).is_empty() { String::new() } else { turn_signature(ask)? };
    Ok(Record {
        created_at: format!("{}Z", crate::tracejournal::iso_seconds(now_secs)),
        signature: sig,
        task: task.into(),
        tier: d.tier.clone(),
        requested_model: d.requested_model.clone(),
        model: d.model.clone(),
        difficulty: d.difficulty,
        downgraded: d.downgraded,
        estimated: s.estimated,
        input: s.actual.input.clone(),
        output: s.actual.output.clone(),
        frontier_saved: s.frontier_tokens_saved(),
        actual: s.actual.dollars,
        counterfactual: s.counterfactual.dollars,
        cache_read: s.actual.cache_read.clone(),
        cache_creation: s.actual.cache_creation.clone(),
        elapsed_ms: None,
    })
}

pub fn big(n: &BigInt) -> Value {
    Value::Int(n.to_string())
}

impl Record {
    /// `json.dumps(asdict(record))`.
    pub fn json(&self) -> String {
        let mut o = Object::with_capacity(16);
        o.set("created_at", self.created_at.as_str());
        o.set("signature", self.signature.as_str());
        o.set("task", self.task.as_str());
        o.set("tier", self.tier.as_str());
        o.set("requested_model", self.requested_model.clone());
        o.set("model", self.model.clone());
        o.set("difficulty", self.difficulty);
        o.set("downgraded", self.downgraded);
        o.set("estimated", self.estimated);
        o.set("input_tokens", big(&self.input));
        o.set("output_tokens", big(&self.output));
        o.set("frontier_tokens_saved", big(&self.frontier_saved));
        o.set("actual_dollars", self.actual);
        o.set("counterfactual_dollars", self.counterfactual);
        o.set("cache_read_tokens", big(&self.cache_read));
        o.set("cache_creation_tokens", big(&self.cache_creation));
        o.set("elapsed_ms", self.elapsed_ms.map(Value::Float).unwrap_or(Value::Null));
        pyjson::dumps(&Value::Obj(o), true)
    }
}
