//! `gateway_journal.GatewayJournal.load` and `summarize`, and
//! `gateway_report`'s calibration report and target-share table.

use num_bigint::BigInt;
use num_traits::{Signed, ToPrimitive, Zero};

use super::answers::{load_records, StoreErr};
use super::pipeline::EXTERNAL_TIER;
use super::pyops::{is_py_int, py_int, true_div};
use crate::gate::py::text;
use crate::gate::pyjson::Value;

/// Why the journal was not read the way the oracle reads it.
#[derive(Debug)]
pub enum JournalErr {
    /// A record whose values are not the types the gateway writes, or a
    /// line json.loads raises on (ruled GW-8): the binary refuses.
    Unread(String),
    Io(std::io::Error),
}

const PREFIX: &str = "the gateway journal holds a record this binary does not read";

fn unread(what: &str) -> JournalErr {
    JournalErr::Unread(format!("{PREFIX}: {what}"))
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

/// A loaded GatewayTurnRecord.
#[derive(Debug, Clone)]
pub struct JRecord {
    pub signature: String,
    pub task: String,
    pub tier: String,
    pub model: Value,
    pub downgraded: bool,
    pub input: BigInt,
    pub output: BigInt,
    pub cache_read: BigInt,
    pub cache_creation: BigInt,
    pub saved: BigInt,
    pub actual: f64,
    pub counterfactual: f64,
}

/// `GatewayJournal(path).load()`.
pub fn load_journal(path: &str) -> Result<Vec<JRecord>, JournalErr> {
    let objs = load_records(path, REQUIRED).map_err(|e| match e {
        StoreErr::Unreadable => unread("a line json.loads raises on"),
        StoreErr::Io(e) => JournalErr::Io(e),
    })?;
    let mut out = Vec::with_capacity(objs.len());
    for o in objs {
        let (Value::Str(sig), Value::Str(task), Value::Str(tier), Value::Bool(down)) =
            (o.value("signature"), o.value("task"), o.value("tier"), o.value("downgraded"))
        else {
            return Err(unread("signature, task, tier or downgraded"));
        };
        let mut ints = vec![];
        for k in ["input_tokens", "output_tokens", "frontier_tokens_saved", "cache_read_tokens", "cache_creation_tokens"] {
            match o.get(k) {
                None => ints.push(BigInt::zero()),
                Some(v) if is_py_int(v) => ints.push(py_int(v).unwrap_or_default()),
                Some(_) => return Err(unread(&format!("{k} is not an int"))),
            }
        }
        let (Value::Float(a), Value::Float(c)) = (o.value("actual_dollars"), o.value("counterfactual_dollars")) else {
            return Err(unread("a dollar figure is not a float"));
        };
        let mut it = ints.into_iter();
        let (input, output, saved, cache_read, cache_creation) =
            (it.next().unwrap(), it.next().unwrap(), it.next().unwrap(), it.next().unwrap(), it.next().unwrap());
        out.push(JRecord {
            signature: sig.clone(),
            task: task.clone(),
            tier: tier.clone(),
            model: o.value("model").clone(),
            downgraded: *down,
            input,
            output,
            cache_read,
            cache_creation,
            saved,
            actual: *a,
            counterfactual: *c,
        });
    }
    Ok(out)
}

/// Python 3.12's `sum()` over floats: compensated (Neumaier).
pub fn sum_floats(xs: &[f64]) -> f64 {
    let Some((first, rest)) = xs.split_first() else { return 0.0 };
    let mut f = first + 0.0;
    let mut c = 0.0;
    for &x in rest {
        let t = f + x;
        if f.abs() >= x.abs() {
            c += (f - t) + x;
        } else {
            c += (x - t) + f;
        }
        f = t;
    }
    if c != 0.0 && c.is_finite() {
        f += c;
    }
    f
}

/// GatewaySummary, less the repeat groups.
#[derive(Debug, Clone)]
pub struct Summary {
    pub turns: usize,
    pub downgraded: usize,
    pub local_turns: usize,
    pub frontier_in: BigInt,
    pub frontier_out: BigInt,
    pub frontier_saved: BigInt,
    pub dollars_saved: f64,
    pub blended: f64,
    pub cache_read: BigInt,
    pub cache_creation: BigInt,
    pub cache_hit_rate: f64,
    total_actual: f64,
    total_cf: f64,
}

/// `gateway_journal.summarize`.
pub fn summarize(rs: &[JRecord]) -> Summary {
    let mut s = Summary {
        turns: rs.len(),
        downgraded: 0,
        local_turns: 0,
        frontier_in: BigInt::zero(),
        frontier_out: BigInt::zero(),
        frontier_saved: BigInt::zero(),
        dollars_saved: 0.0,
        blended: 1.0,
        cache_read: BigInt::zero(),
        cache_creation: BigInt::zero(),
        cache_hit_rate: 0.0,
        total_actual: 0.0,
        total_cf: 0.0,
    };
    let (mut actual, mut cf) = (vec![], vec![]);
    let mut all = BigInt::zero();
    for r in rs {
        if r.downgraded {
            s.downgraded += 1;
            s.frontier_in += &r.input + &r.cache_read + &r.cache_creation;
            s.frontier_out += &r.output;
        }
        actual.push(r.actual);
        cf.push(r.counterfactual);
        s.cache_read += &r.cache_read;
        s.cache_creation += &r.cache_creation;
        all += &r.input + &r.cache_read + &r.cache_creation;
        if r.tier == "tier1-local" {
            s.local_turns += 1;
        }
    }
    s.total_actual = sum_floats(&actual);
    s.total_cf = sum_floats(&cf);
    s.dollars_saved = s.total_cf - s.total_actual;
    if s.total_actual > 0.0 {
        s.blended = s.total_cf / s.total_actual;
    }
    if !all.is_zero() {
        s.cache_hit_rate = true_div(&s.cache_read, &all);
    }
    s.frontier_saved = &s.frontier_in + &s.frontier_out;
    s
}

/// The part of a reuse candidate the report reads.
pub struct Candidate {
    pub count: usize,
    pub tokens: BigInt,
    pub dollars: f64,
}

/// CalibrationReport.
pub struct Report {
    pub summary: Summary,
    pub repeat_clusters: usize,
    pub recoverable_tokens: BigInt,
    pub recoverable_dollars: f64,
    pub combined_saved: BigInt,
    pub combined_multiplier: f64,
}

/// `int(x)` of a finite float.
fn trunc_int(f: f64) -> BigInt {
    num_traits::FromPrimitive::from_f64(f.trunc()).unwrap_or_default()
}

/// `build_report`, given the ranked reuse candidates.
pub fn build_report(rs: &[JRecord], cands: &[Candidate]) -> Report {
    let s = summarize(rs);
    let mut rec_tokens = BigInt::zero();
    let mut rec_dollars = 0.0;
    for c in cands {
        if c.count <= 1 {
            continue;
        }
        let n = BigInt::from(c.count);
        let num = &c.tokens * BigInt::from(c.count - 1);
        rec_tokens += trunc_int(true_div(&num, &n));
        rec_dollars += (c.dollars * (c.count - 1) as f64) / c.count as f64;
    }
    let combined_saved = &s.frontier_saved + &rec_tokens;
    let mut mult = 1.0;
    if s.total_actual > 0.0 {
        let d = s.total_actual - rec_dollars;
        let d = if 1e-9 > d { 1e-9 } else { d };
        mult = s.total_cf / d;
    }
    Report {
        summary: s,
        repeat_clusters: cands.len(),
        recoverable_tokens: rec_tokens,
        recoverable_dollars: rec_dollars,
        combined_saved,
        combined_multiplier: mult,
    }
}

/// TargetShare.
#[derive(Debug, Clone)]
pub struct Share {
    pub model: String,
    pub turns: usize,
    pub share: f64,
    pub input: BigInt,
    pub output: BigInt,
    pub saved: BigInt,
}

/// The switchyard-tier records; their model must be a str, as the gateway
/// writes it.
pub fn external_records(rs: &[JRecord]) -> Result<Vec<JRecord>, JournalErr> {
    let mut out = vec![];
    for r in rs {
        if r.tier != EXTERNAL_TIER {
            continue;
        }
        if !matches!(r.model, Value::Str(_)) {
            return Err(unread("a switchyard turn's model is not a str"));
        }
        out.push(r.clone());
    }
    Ok(out)
}

/// `build_target_share_table`.
pub fn share_table(rs: &[JRecord]) -> Result<Vec<Share>, JournalErr> {
    let ext = external_records(rs)?;
    if ext.is_empty() {
        return Ok(vec![]);
    }
    let mut out: Vec<Share> = vec![];
    for r in &ext {
        let m = r.model.as_str().unwrap_or_default();
        let i = match out.iter().position(|s| s.model == m) {
            Some(i) => i,
            None => {
                out.push(Share {
                    model: m.into(),
                    turns: 0,
                    share: 0.0,
                    input: BigInt::zero(),
                    output: BigInt::zero(),
                    saved: BigInt::zero(),
                });
                out.len() - 1
            }
        };
        let sh = &mut out[i];
        sh.turns += 1;
        sh.input += &r.input;
        sh.output += &r.output;
        sh.saved += &r.saved;
    }
    for sh in &mut out {
        sh.share = sh.turns as f64 / ext.len() as f64;
    }
    out.sort_by(|a, b| b.turns.cmp(&a.turns).then_with(|| code_points(&a.model).cmp(&code_points(&b.model))));
    Ok(out)
}

/// A str as Python orders it: by code point, a lone surrogate as itself.
fn code_points(s: &str) -> Vec<u32> {
    s.chars().map(|c| text::as_surrogate(c).unwrap_or(c as u32)).collect()
}

fn pad_r(s: &str, w: usize) -> String {
    let n = text::len(s);
    if n < w {
        format!("{s}{}", " ".repeat(w - n))
    } else {
        s.to_string()
    }
}

fn pad_l(s: &str, w: usize) -> String {
    let n = text::len(s);
    if n < w {
        format!("{}{s}", " ".repeat(w - n))
    } else {
        s.to_string()
    }
}

/// `format(n, ",")`.
pub fn commas(n: &BigInt) -> String {
    let s = n.abs().to_string();
    let mut b = String::new();
    let len = s.len();
    for (i, c) in s.chars().enumerate() {
        if i > 0 && (len - i) % 3 == 0 {
            b.push(',');
        }
        b.push(c);
    }
    if n.is_negative() {
        format!("-{b}")
    } else {
        b
    }
}

/// `format(x, ".Nf")`.
pub fn format_f(x: f64, n: usize) -> String {
    crate::garden::format_f(x, n)
}

/// `format(x, ".1%")`.
pub fn percent(x: f64) -> String {
    format!("{}%", format_f(x * 100.0, 1))
}

/// `format_target_share_table`.
pub fn format_share_table(table: &[Share]) -> Vec<String> {
    let mut w = "target".len();
    for r in table {
        w = w.max(text::len(&r.model));
    }
    let mut lines = vec![format!(
        "  {}  {}  {}  {}  {}  {}",
        pad_r("target", w),
        pad_l("turns", 6),
        pad_l("share", 6),
        pad_l("input", 10),
        pad_l("output", 9),
        pad_l("saved", 10)
    )];
    for r in table {
        lines.push(format!(
            "  {}  {}  {}  {}  {}  {}",
            pad_r(&r.model, w),
            pad_l(&r.turns.to_string(), 6),
            pad_l(&percent(r.share), 6),
            pad_l(&commas(&r.input), 10),
            pad_l(&commas(&r.output), 9),
            pad_l(&commas(&r.saved), 10)
        ));
    }
    lines
}

/// `format(repr(s), "62")` and the like: repr padded to w.
pub fn pad_repr(s: &str, w: usize) -> String {
    pad_r(&text::repr(s), w)
}

/// A BigInt as an i128 where it fits, for the distiller's worklist.
pub fn to_i128(n: &BigInt) -> Option<i128> {
    n.to_i128()
}
