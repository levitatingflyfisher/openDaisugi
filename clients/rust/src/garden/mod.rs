//! `opendaisugi.gardener`: the pruner and the merger that keep the
//! pathway store healthy, with the decisions, reasons and store writes of
//! the Python oracle. The Go client's `internal/garden` is the reference.

use crate::gate::pyjson::{Object, Value};
use crate::pathways::importer::py_row;
use crate::pathways::pathway::Pathway;
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::pathways::store::Store;
use crate::pathways::PwErr;

/// `gardener.PruneConfig`.
pub struct PruneConfig {
    pub max_idle_days: f64,
    pub max_failure_ratio: f64,
    pub min_activations: i128,
}

impl Default for PruneConfig {
    fn default() -> Self {
        PruneConfig { max_idle_days: 30.0, max_failure_ratio: 0.5, min_activations: 5 }
    }
}

/// `gardener.PruneReport`: the removed ids in store order, each with its
/// reason.
#[derive(Default)]
pub struct PruneReport {
    pub removed: Vec<(String, String)>,
    pub kept: usize,
}

/// `gardener.MergeConfig`.
pub struct MergeConfig {
    pub similarity_threshold: f64,
    pub require_compatible_permissions: bool,
}

impl Default for MergeConfig {
    fn default() -> Self {
        MergeConfig { similarity_threshold: 0.92, require_compatible_permissions: true }
    }
}

/// `gardener.MergeReport`.
#[derive(Default)]
pub struct MergeReport {
    pub merged_pairs: Vec<(String, String)>,
    pub kept_ids: Vec<String>,
    pub removed_ids: Vec<String>,
}

/// A model int. Every stored int is a SQLite INTEGER, so it fits 64 bits;
/// a sum of two fits 128.
pub fn int_of(v: &Value) -> i128 {
    match v {
        Value::Int(t) => t.parse().unwrap_or(0),
        Value::Bool(b) => *b as i128,
        _ => 0,
    }
}

pub fn float_of(v: &Value) -> f64 {
    match v {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse().unwrap_or(0.0),
        _ => 0.0,
    }
}

/// Python's `a / b` for two ints: the quotient correctly rounded to a
/// float (round half to even), for any `a` and `b` a sum of two 64-bit
/// counts can be. `b` is not zero.
pub fn true_div(a: i128, b: i128) -> f64 {
    let neg = (a < 0) != (b < 0);
    let (a, b) = (a.unsigned_abs(), b.unsigned_abs());
    if a == 0 {
        return if neg { -0.0 } else { 0.0 };
    }
    let bits = |x: u128| 128 - x.leading_zeros() as i32;
    // Scale so the quotient has 55 bits: 53 kept, a guard bit and one
    // more, with the remainder as the sticky bit.
    let k = 55 - (bits(a) - bits(b));
    let (num, den) = if k >= 0 { (a << k, b) } else { (a, b << -k) };
    let (mut q, mut e, mut sticky) = (num / den, -k, num % den != 0);
    // q has 55 or 56 bits: take it to 54 (53 kept and a guard bit), each
    // dropped bit joining the sticky bit.
    while q >= 1u128 << 54 {
        sticky |= q & 1 != 0;
        q >>= 1;
        e += 1;
    }
    let guard = q & 1 != 0;
    q >>= 1;
    e += 1;
    if guard && (sticky || q & 1 != 0) {
        q += 1;
    }
    let f = q as f64 * 2f64.powi(e);
    if neg {
        -f
    } else {
        f
    }
}

/// Python's `format(x, ".Nf")`, `nan`, `inf` and `-inf` included.
pub fn format_f(x: f64, n: usize) -> String {
    if x.is_nan() {
        return "nan".into();
    }
    if x.is_infinite() {
        return if x > 0.0 { "inf".into() } else { "-inf".into() };
    }
    format!("{x:.n$}")
}

/// `gardener.prune`: the first matching reason wins (grace, then a
/// failure-dominated ratio, then staleness), each row deleted as it is
/// found unless `dry_run`.
pub fn prune(s: &Store, cfg: &PruneConfig, dry_run: bool, now: f64) -> Result<PruneReport, PwErr> {
    let mut rep = PruneReport::default();
    let idle_cutoff = now - cfg.max_idle_days * 86_400.0;
    for p in s.read_all(&|_| false)? {
        let o = &p.obj;
        let (hits, fails) = (int_of(o.value("hit_count")), int_of(o.value("failure_count")));
        let total = hits + fails;
        if total < cfg.min_activations {
            rep.kept += 1;
            continue;
        }
        let ratio = true_div(fails, if total == 0 { 1 } else { total });
        if ratio > cfg.max_failure_ratio {
            rep.removed.push((p.id().to_string(), format!("failure_dominated (ratio={})", format_f(ratio, 2))));
            if !dry_run {
                s.delete(p.id())?;
            }
            continue;
        }
        // `last_activation_at or distilled_at`: 0.0 is falsy, NaN is not.
        let mut freshness = float_of(o.value("last_activation_at"));
        if freshness == 0.0 {
            freshness = float_of(o.value("distilled_at"));
        }
        if freshness != 0.0 && freshness < idle_cutoff {
            rep.removed.push((p.id().to_string(), format!("stale (idle={}d)", format_f((now - freshness) / 86_400.0, 1))));
            if !dry_run {
                s.delete(p.id())?;
            }
            continue;
        }
        rep.kept += 1;
    }
    Ok(rep)
}

/// `_similarity.cosine_similarity`: the zero-norm guard and the clamp to
/// [-1, 1]. The sums run in order (GD-6).
pub fn cosine(a: &[f64], b: &[f64]) -> f64 {
    let dot: f64 = a.iter().zip(b).fold(0.0, |s, (x, y)| s + x * y);
    let mut denom = norm(a) * norm(b);
    if denom == 0.0 {
        denom = 1e-9;
    }
    (dot / denom).clamp(-1.0, 1.0)
}

pub fn norm(v: &[f64]) -> f64 {
    v.iter().fold(0.0, |s, x| s + x * x).sqrt()
}

fn vector(p: &Pathway) -> Vec<f64> {
    match p.obj.value("task_embedding") {
        Value::List(l) => l.iter().map(float_of).collect(),
        _ => vec![],
    }
}

fn permissions(p: &Pathway) -> &Object {
    static EMPTY: std::sync::OnceLock<Object> = std::sync::OnceLock::new();
    p.obj
        .value("envelope")
        .as_obj()
        .and_then(|e| e.value("permissions").as_obj())
        .unwrap_or_else(|| EMPTY.get_or_init(Object::new))
}

fn str_list(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l.iter().filter_map(|x| x.as_str().map(|s| s.to_string())).collect(),
        _ => vec![],
    }
}

/// `permissions.intersect_permissions`: the booleans ANDed, the four lists
/// intersected and sorted, the two ceilings the minimum, every other field
/// its default.
pub fn intersect_permissions(perms: &[&Object]) -> Object {
    if perms.len() == 1 {
        return perms[0].clone();
    }
    let mut merged = Object::new();
    for f in ["shell", "network", "shell_allow_decomposition"] {
        merged.set(f, perms.iter().all(|p| p.value(f).truthy()));
    }
    for f in ["shell_allowlist", "file_read", "file_write", "network_hosts"] {
        let mut inter: Option<Vec<String>> = None;
        for p in perms {
            let set = str_list(p.value(f));
            inter = Some(match inter {
                None => set,
                Some(cur) => cur.into_iter().filter(|x| set.contains(x)).collect(),
            });
        }
        let mut keys = inter.unwrap_or_default();
        keys.sort();
        keys.dedup();
        merged.set(f, Value::List(keys.into_iter().map(Value::Str).collect()));
    }
    for f in ["max_execution_time_s", "max_output_size_mb"] {
        let min = perms.iter().map(|p| p.value(f)).min_by(|a, b| cmp_int_text(a, b)).cloned().unwrap_or(Value::Null);
        merged.set(f, min);
    }
    match validate_model(Id::Permission, &Value::Obj(merged.clone()), Mode::Python) {
        Ok(Value::Obj(o)) => o,
        _ => merged,
    }
}

/// Orders two model ints by value, at any size.
fn cmp_int_text(a: &Value, b: &Value) -> std::cmp::Ordering {
    use std::cmp::Ordering::*;
    let (x, y) = match (a, b) {
        (Value::Int(x), Value::Int(y)) => (x.as_str(), y.as_str()),
        _ => return Equal,
    };
    let (nx, ny) = (x.starts_with('-'), y.starts_with('-'));
    let (dx, dy) = (x.trim_start_matches('-').trim_start_matches('0'), y.trim_start_matches('-').trim_start_matches('0'));
    let mag = dx.len().cmp(&dy.len()).then_with(|| dx.cmp(dy));
    match (nx && !dx.is_empty(), ny && !dy.is_empty()) {
        (false, false) => mag,
        (true, true) => mag.reverse(),
        (true, false) => Less,
        (false, true) => Greater,
    }
}

/// Python's `==` on `model_dump()` values: bool, int and float by value,
/// NaN equal to nothing.
pub fn py_equal(a: &Value, b: &Value) -> bool {
    crate::gate::predicate::py_eq(a, b)
}

fn permissions_compatible(a: &Object, b: &Object) -> bool {
    let merged = Value::Obj(intersect_permissions(&[a, b]));
    py_equal(&merged, &Value::Obj(a.clone())) && py_equal(&merged, &Value::Obj(b.clone()))
}

/// `merger._pick_winner`: more hits wins, then the newer distillation
/// (the first on an equal time). True when `a` wins.
fn a_wins(a: &Pathway, b: &Pathway) -> bool {
    let (ha, hb) = (int_of(a.obj.value("hit_count")), int_of(b.obj.value("hit_count")));
    if ha != hb {
        return ha > hb;
    }
    float_of(a.obj.value("distilled_at")) >= float_of(b.obj.value("distilled_at"))
}

/// `gardener.merge`: a greedy single pass over every pair in store order;
/// the loser's sources and counts fold into the winner, which is written
/// again (to the end of the table), and then the loser is deleted.
pub fn merge(s: &Store, cfg: &MergeConfig, dry_run: bool) -> Result<MergeReport, PwErr> {
    let mut rep = MergeReport::default();
    let mut all: Vec<Pathway> = s.read_all(&|_| true)?.into_iter().map(Pathway::full).collect();
    let vecs: Vec<Vec<f64>> = all.iter().map(vector).collect();
    let mut removed = vec![false; all.len()];
    for i in 0..all.len() {
        if removed[i] {
            continue;
        }
        for j in i + 1..all.len() {
            if removed[j] {
                continue;
            }
            let (a, b) = (&all[i], &all[j]);
            if a.obj.value("embedding_model") != b.obj.value("embedding_model")
                || a.obj.value("embedding_model_version") != b.obj.value("embedding_model_version")
            {
                continue;
            }
            if vecs[i].len() != vecs[j].len() {
                continue;
            }
            let sim = cosine(&vecs[i], &vecs[j]);
            if sim < cfg.similarity_threshold {
                continue;
            }
            if cfg.require_compatible_permissions && !permissions_compatible(permissions(a), permissions(b)) {
                continue;
            }
            let (w, l) = if a_wins(a, b) { (i, j) } else { (j, i) };
            rep.merged_pairs.push((all[w].id().to_string(), all[l].id().to_string()));
            removed[l] = true;
            if !dry_run {
                let mut union: Vec<String> = str_list(all[w].obj.value("source_trace_ids"));
                union.extend(str_list(all[l].obj.value("source_trace_ids")));
                union.sort();
                union.dedup();
                let sums: Vec<(&str, i128)> = ["hit_count", "failure_count"]
                    .iter()
                    .map(|k| (*k, int_of(all[w].obj.value(k)) + int_of(all[l].obj.value(k))))
                    .collect();
                let loser = all[l].id().to_string();
                let winner = &mut all[w];
                winner.obj.set("source_trace_ids", Value::List(union.into_iter().map(Value::Str).collect()));
                for (k, n) in sums {
                    winner.obj.set(k, Value::Int(n.to_string()));
                }
                s.put(&py_row(winner)?)?;
                s.delete(&loser)?;
            }
            if l == i {
                break;
            }
        }
    }
    for (k, p) in all.iter().enumerate() {
        if removed[k] {
            rep.removed_ids.push(p.id().to_string());
        } else {
            rep.kept_ids.push(p.id().to_string());
        }
    }
    rep.removed_ids.sort();
    rep.kept_ids.sort();
    Ok(rep)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn int_division_is_correctly_rounded() {
        // Below 2**53 a float division is exact-rounded; past it the
        // operands would round first.
        for (a, b) in [(1i128, 3i128), (2, 3), (5, 10), (3, 5), (7, 9), (0, 1)] {
            assert_eq!(true_div(a, b), a as f64 / b as f64, "{a}/{b}");
        }
        let big: i128 = (1i128 << 63) - 1;
        // Python: (2**63 - 1) / (2**64 - 2) == 0.5
        assert_eq!(true_div(big, 2 * big), 0.5);
        // Python: (2**62 + 1) / (2**63 + 3) == 0.5
        assert_eq!(true_div((1i128 << 62) + 1, (1i128 << 63) + 3), 0.5);
        // Python: (2**63 + 2**10 + 1) / 2**64 == 0.5000000000000001
        assert_eq!(true_div((1i128 << 63) + (1 << 10) + 1, 1i128 << 64), 0.5000000000000001);
        // Python: 3 / (2**64 - 1) == 1.6263032587282567e-19
        assert_eq!(true_div(3, (1i128 << 64) - 1), 1.6263032587282567e-19);
        // Python: (2**63 - 1) / (2**64 - 1) == 0.5
        assert_eq!(true_div(big, (1i128 << 64) - 1), 0.5);
        // Python: 1 / (2**64 - 1) == 5.421010862427522e-20
        assert_eq!(true_div(1, (1i128 << 64) - 1), 5.421010862427522e-20);
        // Python: (10**19 + 7) / (2 * 10**19 + 1) == 0.5
        let t = 10i128.pow(19);
        assert_eq!(true_div(t + 7, 2 * t + 1), 0.5);
    }

    #[test]
    fn format_f_matches_python() {
        assert_eq!(format_f(0.125, 2), "0.12");
        assert_eq!(format_f(0.375, 2), "0.38");
        assert_eq!(format_f(f64::NAN, 2), "nan");
        assert_eq!(format_f(f64::NEG_INFINITY, 1), "-inf");
        assert_eq!(format_f(299.5, 1), "299.5");
    }
}
