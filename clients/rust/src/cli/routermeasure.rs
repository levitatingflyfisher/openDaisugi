//! `router_report`: the router's weekly measure and the delegate rule and
//! worker in force, for router status.

use std::collections::HashMap;

use num_bigint::BigInt;
use num_traits::ToPrimitive;
use regex::Regex;

use crate::delegate::{self, civil_from_days};
use crate::gate::py::text::{fsdecode, fsencode, strip};
use crate::gate::pyjson::{loads, Object, Value};

const MAX_WEEKS: usize = 8;

/// `router_report._int`: an integer of size at most 2**53, else 0.
fn m_int(v: &Value) -> i64 {
    let Value::Int(t) = v else { return 0 };
    let Ok(n) = t.parse::<BigInt>() else { return 0 };
    let limit = BigInt::from(1i64 << 53);
    if n > limit || n < -limit {
        return 0;
    }
    n.to_i64().unwrap_or(0)
}

/// `router_report._num`: a JSON number as a finite float, or None.
fn m_num(v: &Value) -> Option<f64> {
    let f = match v {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse::<BigInt>().ok()?.to_f64()?,
        _ => return None,
    };
    f.is_finite().then_some(f)
}

fn m_float(v: &Value) -> f64 {
    m_num(v).unwrap_or(0.0)
}

fn os_path(p: &str) -> Option<std::path::PathBuf> {
    use std::os::unix::ffi::OsStrExt;
    let b = fsencode(p).ok()?;
    if b.contains(&0) {
        return None;
    }
    Some(std::path::PathBuf::from(std::ffi::OsStr::from_bytes(&b)))
}

/// `router_report._read_jsonl`: each line that reads as a JSON object.
fn read_jsonl(path: &str) -> Vec<Object> {
    let Some(p) = os_path(path) else { return Vec::new() };
    let Ok(raw) = std::fs::read(p) else { return Vec::new() };
    // read_text: universal newlines, so a lone \r ends a line too.
    let text = String::from_utf8_lossy(&raw).replace("\r\n", "\n").replace('\r', "\n");
    let mut out = Vec::new();
    for line in text.split('\n') {
        if strip(line).is_empty() {
            continue;
        }
        if let Ok(Value::Obj(o)) = loads(line) {
            out.push(o);
        }
    }
    out
}

/// Days from 1970-01-01 to a civil date (Howard Hinnant's algorithm).
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y.rem_euclid(400);
    let mp = if m > 2 { m - 3 } else { m + 9 };
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

/// The ISO week of a day count from 1970-01-01, as `YYYY-Www`.
fn iso_week(days: i64) -> String {
    let wd = (days + 3).rem_euclid(7); // Monday is 0; 1970-01-01 was a Thursday.
    let thursday = days - wd + 3;
    let (y, _, _) = civil_from_days(thursday);
    let week = (thursday - days_from_civil(y, 1, 1)) / 7 + 1;
    format!("{y:04}-W{week:02}")
}

fn iso_stamp() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r"^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})Z$").expect("stamp")
    })
}

/// `router_report.week_of_iso`.
fn week_of_iso(v: &Value) -> Option<String> {
    let Value::Str(s) = v else { return None };
    let c = iso_stamp().captures(s)?;
    let n: Vec<i64> = (1..=6).map(|i| c[i].parse().unwrap_or(-1)).collect();
    let (y, m, d) = (n[0], n[1], n[2]);
    if y < 1 || !(1..=12).contains(&m) || n[3] > 23 || n[4] > 59 || n[5] > 59 {
        return None;
    }
    let leap = y % 4 == 0 && (y % 100 != 0 || y % 400 == 0);
    let days = [0, 31, if leap { 29 } else { 28 }, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][m as usize];
    if d < 1 || d > days {
        return None;
    }
    Some(iso_week(days_from_civil(y, m, d)))
}

/// `router_report.week_of_epoch`.
fn week_of_epoch(v: &Value) -> Option<String> {
    const FIRST: i64 = -62135596800;
    const LAST: i64 = 253402300799;
    let sec = match v {
        Value::Int(t) => {
            let n = t.parse::<BigInt>().ok()?;
            if n < BigInt::from(FIRST) || n > BigInt::from(LAST) {
                return None;
            }
            n.to_i64()?
        }
        Value::Float(f) => {
            if !f.is_finite() {
                return None;
            }
            let fl = f.floor();
            if fl < FIRST as f64 || fl > LAST as f64 {
                return None;
            }
            fl as i64
        }
        _ => return None,
    };
    Some(iso_week(sec.div_euclid(86400)))
}

/// One row of `router_report.weekly`.
#[derive(Default, Clone)]
pub struct Week {
    name: String,
    turns: i64,
    turns_down: i64,
    turn_saved: i64,
    turn_dollars: f64,
    turns_est: i64,
    turn_ms: f64,
    redirects: i64,
    not_redirected: i64,
    delegations: i64,
    ok: i64,
    delegate_ms: f64,
    quotes: i64,
    dropped: i64,
    worker_tokens: i64,
    worker_dollars: f64,
    worker_unpriced: i64,
    kept: i64,
    kept_dollars: f64,
    pass: i64,
    fail: i64,
    unknown: i64,
    tokens_saved: i64,
    dollars_saved: f64,
    estimated: bool,
    delegate_estimated: i64,
}

impl Week {
    pub fn object(&self) -> Object {
        Object::new()
            .with("week", self.name.as_str())
            .with("turns", self.turns)
            .with("turns_downgraded", self.turns_down)
            .with("turn_tokens_saved", self.turn_saved)
            .with("turn_dollars_saved", Value::Float(self.turn_dollars))
            .with("turns_estimated", self.turns_est)
            .with("turn_elapsed_ms", Value::Float(self.turn_ms))
            .with("escalations", 0i64)
            .with("redirects", self.redirects)
            .with("not_redirected", self.not_redirected)
            .with("delegations", self.delegations)
            .with("delegate_ok", self.ok)
            .with("delegate_elapsed_ms", Value::Float(self.delegate_ms))
            .with("quotes", self.quotes)
            .with("dropped", self.dropped)
            .with("worker_tokens", self.worker_tokens)
            .with("worker_dollars", Value::Float(self.worker_dollars))
            .with("worker_unpriced", self.worker_unpriced)
            .with("delegate_tokens_kept", self.kept)
            .with("delegate_dollars_kept", Value::Float(self.kept_dollars))
            .with("task_pass", self.pass)
            .with("task_fail", self.fail)
            .with("task_unknown", self.unknown)
            .with("delegate_estimated", self.delegate_estimated)
            .with("tokens_saved", self.tokens_saved)
            .with("dollars_saved", Value::Float(self.dollars_saved))
            .with("estimated", self.estimated)
    }

    /// `router_report.week_line`.
    pub fn line(&self) -> String {
        let est = if self.estimated { " (estimated)" } else { "" };
        let refused = self.delegations - self.ok;
        let unpriced = if self.worker_unpriced != 0 { format!(", {} unpriced", self.worker_unpriced) } else { String::new() };
        let each = if self.ok != 0 {
            format!(", {}s each", fixed(self.delegate_ms / 1000.0 / self.ok as f64, 1))
        } else {
            String::new()
        };
        format!(
            "{}: tokens saved {}{est}, billed saved ${}{est}; turns {} ({} routed cheaper), escalations 0; \
             delegations {} ({} ok{each}, {refused} refused), quotes {} kept, {} dropped, worker {} tokens ${}{unpriced}; \
             reads redirected {}, not redirected {}; tasks {} passed, {} failed, {} unknown",
            self.name,
            group(self.tokens_saved),
            fixed(self.dollars_saved, 4),
            self.turns,
            self.turns_down,
            self.delegations,
            self.ok,
            self.quotes,
            self.dropped,
            group(self.worker_tokens),
            fixed(self.worker_dollars, 4),
            self.redirects,
            self.not_redirected,
            self.pass,
            self.fail,
            self.unknown
        )
    }
}

/// Python's `f"{n:,}"`.
fn group(n: i64) -> String {
    let s = n.unsigned_abs().to_string();
    let mut out = String::new();
    for (i, c) in s.chars().enumerate() {
        if i > 0 && (s.len() - i) % 3 == 0 {
            out.push(',');
        }
        out.push(c);
    }
    if n < 0 {
        format!("-{out}")
    } else {
        out
    }
}

/// Python's `f"{x:.{prec}f}"`.
fn fixed(x: f64, prec: usize) -> String {
    if x.is_nan() {
        return "nan".into();
    }
    format!("{x:.prec$}")
}

/// `router_report.read_graft_events`.
fn graft_events(data_dir: &str) -> Vec<Object> {
    let dir = format!("{data_dir}/gate/audit");
    let Some(p) = os_path(&dir) else { return Vec::new() };
    let Ok(rd) = std::fs::read_dir(p) else { return Vec::new() };
    let mut names = Vec::new();
    for ent in rd {
        let Ok(ent) = ent else { return Vec::new() };
        use std::os::unix::ffi::OsStrExt;
        let n = fsdecode(ent.file_name().as_bytes());
        if n.ends_with(".jsonl") {
            names.push(n);
        }
    }
    names.sort();
    let mut out = Vec::new();
    for n in names {
        for rec in read_jsonl(&format!("{dir}/{n}")) {
            if matches!(rec.value("graft"), Value::Obj(_)) {
                out.push(rec);
            }
        }
    }
    out
}

/// `router_report.weekly`.
pub fn weekly(data_dir: &str) -> Vec<Week> {
    let mut weeks: HashMap<String, Week> = HashMap::new();
    fn row<'a>(weeks: &'a mut HashMap<String, Week>, name: String) -> &'a mut Week {
        weeks.entry(name.clone()).or_insert_with(|| Week { name, ..Default::default() })
    }
    for t in read_jsonl(&format!("{data_dir}/gateway/turns.jsonl")) {
        let Some(name) = week_of_iso(t.value("created_at")) else { continue };
        let r = row(&mut weeks, name);
        r.turns += 1;
        if t.value("downgraded") == &Value::Bool(true) {
            r.turns_down += 1;
        }
        r.turn_saved += m_int(t.value("frontier_tokens_saved"));
        r.turn_dollars += m_float(t.value("counterfactual_dollars")) - m_float(t.value("actual_dollars"));
        if t.value("estimated") == &Value::Bool(true) {
            r.turns_est += 1;
        }
        r.turn_ms += m_float(t.value("elapsed_ms"));
    }
    for e in graft_events(data_dir) {
        let Some(name) = week_of_epoch(e.value("at")) else { continue };
        let r = row(&mut weeks, name);
        let applied = matches!(e.value("graft"), Value::Obj(g) if g.value("applied") == &Value::Bool(true));
        if applied {
            r.redirects += 1;
        } else {
            r.not_redirected += 1;
        }
    }
    for d in read_jsonl(&delegate::journal_path(data_dir)) {
        let Some(name) = week_of_iso(d.value("at")) else { continue };
        let r = row(&mut weeks, name);
        r.delegations += 1;
        let ok = d.value("ok") == &Value::Bool(true);
        if ok && d.value("estimated") == &Value::Bool(true) {
            r.delegate_estimated += 1;
        }
        if ok {
            r.ok += 1;
            r.delegate_ms += m_float(d.value("elapsed_ms"));
        }
        r.quotes += m_int(d.value("quotes"));
        r.dropped += m_int(d.value("dropped"));
        r.worker_tokens += m_int(d.value("worker_input_tokens")) + m_int(d.value("worker_output_tokens"));
        if ok {
            match m_num(d.value("worker_dollars")) {
                Some(w) => r.worker_dollars += w,
                None => r.worker_unpriced += 1,
            }
        }
        r.kept += m_int(d.value("frontier_tokens_kept"));
        r.kept_dollars += m_float(d.value("frontier_dollars_kept"));
        match d.value("task_ok") {
            Value::Bool(true) => r.pass += 1,
            Value::Bool(false) => r.fail += 1,
            _ => r.unknown += 1,
        }
    }
    let mut names: Vec<String> = weeks.keys().cloned().collect();
    names.sort_by(|a, b| b.cmp(a));
    names.truncate(MAX_WEEKS);
    names
        .into_iter()
        .map(|n| {
            let mut r = weeks.remove(&n).expect("week");
            r.tokens_saved = r.turn_saved + r.kept;
            r.dollars_saved = r.turn_dollars + r.kept_dollars - r.worker_dollars;
            r.estimated = r.turns_est > 0 || r.delegate_estimated > 0;
            r
        })
        .collect()
}

/// `router_report.delegate_state`: the JSON object and the text lines, or
/// a hand-back.
pub fn delegate_state(data_dir: &str, env: &HashMap<String, String>) -> Result<(Object, Vec<String>), String> {
    let root = format!("{data_dir}/gate");
    let (rules, bad) = delegate::load_rules(&root);
    let rule = rules.iter().position(|r| r.acting());
    let rt = match delegate::load_default_envelope(&root).map_err(|u| u.0)? {
        Err(()) => delegate::Route {
            reason: "no worker: the gate's default envelope cannot be read".into(),
            ..Default::default()
        },
        Ok(e) => {
            let get = |k: &str| env.get(k).cloned();
            let allow = rule.is_some_and(|i| rules[i].allow_remote);
            delegate::route_delegate(data_dir, e.as_ref(), allow, &get)
        }
    };
    let mut o = Object::new();
    let mut lines = vec!["delegate (large reads):".to_string()];
    match rule {
        None => {
            o.set("rule", Value::Null);
            lines.push("  rule: none. Reads are not redirected (a rule file in <gate root>/grafts).".into());
        }
        Some(i) => {
            let r = &rules[i];
            o.set(
                "rule",
                Value::Obj(
                    Object::new()
                        .with("file", r.file.as_str())
                        .with("id", r.id.as_str())
                        .with("version", Value::Int(r.version.clone()))
                        .with("state", r.state.as_str())
                        .with("file_lines_over", Value::Int(r.min_lines.clone()))
                        .with("allow_remote", r.allow_remote),
                ),
            );
            let verb = match r.state.as_str() {
                "active" => "go to",
                "trial" => "go to, in the trial's graft arm only,",
                _ => "would go to (audit: not denied)",
            };
            lines.push(format!(
                "  rule: {} v{} ({}), {}: reads over {} lines {verb} the delegate tool",
                r.id, r.version, r.file, r.state, r.min_lines
            ));
        }
    }
    let mut unused = Vec::new();
    for (i, r) in rules.iter().enumerate() {
        if Some(i) == rule {
            continue;
        }
        let why = if r.acting() { "an earlier rule file is in force".to_string() } else { format!("its state is {}", r.state) };
        lines.push(format!("  rule {} ({}) is not in force: {why}", r.id, r.file));
        unused.push(Value::Obj(
            Object::new()
                .with("file", r.file.as_str())
                .with("id", r.id.as_str())
                .with("state", r.state.as_str())
                .with("why", why),
        ));
    }
    o.set("unused_rules", Value::List(unused));
    let mut bads = Vec::new();
    for (f, why) in &bad {
        lines.push(format!("  rule file {f} is not used: {why}"));
        bads.push(Value::Obj(Object::new().with("file", f.as_str()).with("why", why.as_str())));
    }
    o.set("bad_rules", Value::List(bads));
    if rt.ok {
        o.set("worker", Value::Obj(rt.as_object()));
        lines.push(format!(
            "  worker: {} ({}, {})",
            rt.model.clone().unwrap_or_default(),
            rt.tier.clone().unwrap_or_default(),
            rt.host.clone().unwrap_or_default()
        ));
    } else {
        o.set("worker", Value::Null);
        lines.push(format!("  worker: none. {}.", rt.reason));
        if rule.is_some() {
            lines.push("  With no worker, reads over the threshold go through the normal gate.".into());
        }
    }
    o.set("worker_reason", rt.reason.as_str());
    Ok((o, lines))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn weeks_and_numbers() {
        assert_eq!(week_of_iso(&Value::Str("2026-09-30T00:00:00Z".into())).as_deref(), Some("2026-W40"));
        assert_eq!(week_of_iso(&Value::Str("2026-02-30T00:00:00Z".into())), None);
        assert_eq!(week_of_epoch(&Value::Float(-0.5)).as_deref(), Some("1970-W01"));
        assert_eq!(week_of_epoch(&Value::Int("-62135596800".into())).as_deref(), Some("0001-W01"));
        assert_eq!(week_of_epoch(&Value::Float(253402300799.9)).as_deref(), Some("9999-W52"));
        assert_eq!(group(-1234567), "-1,234,567");
        assert_eq!(group(12), "12");
        assert_eq!(fixed(0.00006, 4), "0.0001");
    }
}

// ---------------------------------------------------------------------------
// The promotion meter (router_report.trial_state, SW-13 to SW-15)
// ---------------------------------------------------------------------------

const MIN_LABELED: i64 = 3;
const MAX_TRANSCRIPT_BYTES: u64 = 64 * 1024 * 1024;
pub const MAX_NOTE_CHARS: usize = 500;
const SUM_LIMIT: i64 = 1 << 53;

pub fn labels_path(data_dir: &str) -> String {
    format!("{data_dir}/router/labels.jsonl")
}

/// `router_report.session_ok`.
pub fn session_ok(s: &str) -> bool {
    crate::gate::pystr::safe_session_id(s) == s
}

/// `router_report.read_labels`: the last good row wins.
fn read_labels(data_dir: &str) -> HashMap<String, String> {
    let mut out = HashMap::new();
    for row in read_jsonl(&labels_path(data_dir)) {
        if let (Value::Str(s), Value::Str(o)) = (row.value("session"), row.value("outcome")) {
            if session_ok(s) && (o == "pass" || o == "fail") {
                out.insert(s.clone(), o.clone());
            }
        }
    }
    out
}

/// `router_report._count`: an integer from 0 to 2**53, else 0.
fn count_of(v: &Value) -> i64 {
    m_int(v).max(0)
}

/// `router_report._read_capped`.
fn read_capped(path: &str) -> Option<Vec<u8>> {
    use std::io::Read;
    use std::os::unix::fs::{FileTypeExt, OpenOptionsExt};
    let p = os_path(path)?;
    let mut f = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK | libc::O_CLOEXEC)
        .open(p)
        .ok()?;
    let md = f.metadata().ok()?;
    let ft = md.file_type();
    if !ft.is_file() || ft.is_fifo() || md.len() > MAX_TRANSCRIPT_BYTES {
        return None;
    }
    let mut out = Vec::new();
    let mut buf = vec![0u8; 1 << 20];
    loop {
        match f.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                out.extend_from_slice(&buf[..n]);
                if out.len() as u64 > MAX_TRANSCRIPT_BYTES {
                    return None;
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => return None,
        }
    }
    Some(out)
}

/// A session's billed cost.
struct Cost {
    dollars: f64,
    quota: i64,
    estimated: bool,
}

/// One message's price: `router_report.session_cost`'s sum term.
fn price_message(model: &str, i: i64, o: i64, cr: i64, cw: i64) -> (f64, bool) {
    let prices = crate::gateway::meter::default_prices();
    let (pr, known) = match prices.get(model) {
        Some(p) => (*p, true),
        None => (crate::gateway::meter::Price { input: 3.0, output: 15.0 }, false),
    };
    let a = i as f64 * pr.input;
    let b = (cr as f64 * pr.input) * 0.1;
    let c = (cw as f64 * pr.input) * 1.25;
    let d = o as f64 * pr.output;
    ((((a + b) + c) + d) / 1_000_000.0, known)
}

/// `router_report.session_cost`: None when the transcript cannot be read;
/// Err for one this binary does not read the way Python does.
fn session_cost(path: &str) -> Result<Option<Cost>, String> {
    if !path.starts_with('/') {
        return Ok(None);
    }
    let Some(data) = read_capped(path) else { return Ok(None) };
    let text = String::from_utf8_lossy(&data).into_owned();
    let mut order: Vec<String> = Vec::new();
    let mut msgs: HashMap<String, (String, Object)> = HashMap::new();
    for (n, line) in text.split('\n').enumerate() {
        if strip(line).is_empty() {
            continue;
        }
        let row = match crate::gate::pyjson::loads_py(line, 900) {
            Ok(Value::Obj(o)) => o,
            Ok(_) => continue,
            Err(e) if e.starts_with("maximum recursion depth") || e == "unsupported" => {
                return Err("a transcript line this binary does not read".into())
            }
            Err(_) => continue,
        };
        if row.value("type") != &Value::Str("assistant".into()) {
            continue;
        }
        let Value::Obj(msg) = row.value("message") else { continue };
        let Value::Obj(usage) = msg.value("usage") else { continue };
        let key = match msg.value("id") {
            Value::Str(id) => format!("id:{id}"),
            _ => format!("line:{n}"),
        };
        let model = match msg.value("model") {
            Value::Str(m) => m.clone(),
            _ => String::new(),
        };
        if !msgs.contains_key(&key) {
            order.push(key.clone());
        }
        msgs.insert(key, (model, usage.clone()));
    }
    let mut c = Cost { dollars: 0.0, quota: 0, estimated: false };
    for k in &order {
        let (model, u) = &msgs[k];
        let i = count_of(u.value("input_tokens"));
        let cr = count_of(u.value("cache_read_input_tokens"));
        let cw = count_of(u.value("cache_creation_input_tokens"));
        let o = count_of(u.value("output_tokens"));
        let (d, known) = price_message(model, i, o, cr, cw);
        if !known {
            c.estimated = true;
        }
        c.dollars += d;
        c.quota += i + cr + cw + o;
        if c.quota > SUM_LIMIT {
            return Err("a token count past 2**53 in sum, which this binary does not add".into());
        }
    }
    Ok(Some(c))
}

/// `router_report._read_audit`: every audit record, by file name then line.
fn read_audit(data_dir: &str) -> Vec<Object> {
    let dir = format!("{data_dir}/gate/audit");
    let Some(p) = os_path(&dir) else { return Vec::new() };
    let Ok(rd) = std::fs::read_dir(p) else { return Vec::new() };
    let mut names: Vec<String> = Vec::new();
    for ent in rd.flatten() {
        use std::os::unix::ffi::OsStrExt;
        let n = fsdecode(ent.file_name().as_bytes());
        if n.ends_with(".jsonl") {
            names.push(n);
        }
    }
    names.sort();
    let mut out = Vec::new();
    for n in names {
        out.extend(read_jsonl(&format!("{dir}/{n}")));
    }
    out
}

#[derive(Default)]
struct Arm {
    sessions: i64,
    labeled: i64,
    passed: i64,
    failed: i64,
    unlabeled: i64,
    cost_unknown: i64,
    dollars: f64,
    quota: i64,
    estimated: bool,
    rate: Option<f64>,
    per_success: Option<f64>,
    quota_per_success: Option<f64>,
}

fn opt_float(v: Option<f64>) -> Value {
    v.map(Value::Float).unwrap_or(Value::Null)
}

impl Arm {
    fn object(&self) -> Object {
        Object::new()
            .with("sessions", self.sessions)
            .with("labeled", self.labeled)
            .with("passed", self.passed)
            .with("failed", self.failed)
            .with("unlabeled", self.unlabeled)
            .with("cost_unknown", self.cost_unknown)
            .with("billed_dollars", Value::Float(self.dollars))
            .with("quota_tokens", self.quota)
            .with("estimated", self.estimated)
            .with("success_rate", opt_float(self.rate))
            .with("dollars_per_success", opt_float(self.per_success))
            .with("quota_per_success", opt_float(self.quota_per_success))
    }

    fn line(&self, name: &str) -> String {
        let est = if self.estimated { " (estimated)" } else { "" };
        let per = match (self.per_success, self.quota_per_success) {
            (Some(d), Some(q)) => format!("per success ${}{est}, {} quota tokens", fixed(d, 4), fixed(q, 0)),
            _ => "per success: unknown".to_string(),
        };
        let unknown = if self.cost_unknown != 0 { format!(", {} with no cost", self.cost_unknown) } else { String::new() };
        format!(
            "  {name} arm: {} sessions, {} labeled ({} passed, {} failed), {} unlabeled{unknown}; billed ${}{est}, {} quota tokens; {per}",
            self.sessions,
            self.labeled,
            self.passed,
            self.failed,
            self.unlabeled,
            fixed(self.dollars, 4),
            group(self.quota)
        )
    }
}

/// `router_report.trial_state`: None when no rule in audit or trial is in
/// force; Err for input this binary does not read the way Python does.
pub fn trial_state(data_dir: &str) -> Result<Option<(Object, Vec<String>)>, String> {
    let (rules, _) = delegate::load_rules(&format!("{data_dir}/gate"));
    let Some(rule) = rules.into_iter().find(|r| r.acting()) else { return Ok(None) };
    if rule.state != "audit" && rule.state != "trial" {
        return Ok(None);
    }
    let mut arms: HashMap<String, std::collections::BTreeSet<String>> = HashMap::new();
    let mut transcripts: HashMap<String, String> = HashMap::new();
    for rec in read_audit(data_dir) {
        let Value::Str(session) = rec.value("session_id") else { continue };
        if let Value::Str(tp) = rec.value("transcript_path") {
            if tp.starts_with('/') {
                transcripts.insert(session.clone(), tp.clone());
            }
        }
        let Value::Obj(g) = rec.value("graft") else { continue };
        let id_ok = matches!(g.value("rule_id"), Value::Str(s) if *s == rule.id);
        let arm = match g.value("arm") {
            Value::Str(a) if a == "graft" || a == "control" => a.clone(),
            _ => continue,
        };
        let ver_ok = matches!(g.value("version"), Value::Int(_)) && count_of(g.value("version")).to_string() == rule.version;
        if !id_ok || !ver_ok {
            continue;
        }
        arms.entry(session.clone()).or_default().insert(arm);
    }
    let labels = read_labels(data_dir);
    let (mut g, mut c) = (Arm::default(), Arm::default());
    let mut conflicting = 0i64;
    let mut sessions: Vec<&String> = arms.keys().collect();
    sessions.sort();
    for s in sessions {
        let set = &arms[s];
        if set.len() > 1 {
            conflicting += 1;
            continue;
        }
        let r = if set.contains("graft") { &mut g } else { &mut c };
        r.sessions += 1;
        let Some(outcome) = labels.get(s) else {
            r.unlabeled += 1;
            continue;
        };
        r.labeled += 1;
        if outcome == "pass" {
            r.passed += 1;
        } else {
            r.failed += 1;
        }
        let cost = match transcripts.get(s) {
            Some(tp) => session_cost(tp)?,
            None => None,
        };
        let Some(cost) = cost else {
            r.cost_unknown += 1;
            continue;
        };
        r.dollars += cost.dollars;
        r.quota += cost.quota;
        if r.quota > SUM_LIMIT {
            return Err("a token count past 2**53 in sum, which this binary does not add".into());
        }
        if cost.estimated {
            r.estimated = true;
        }
    }
    if rule.allow_remote {
        g.estimated = true;
    }
    for r in [&mut g, &mut c] {
        if r.labeled > 0 {
            r.rate = Some(r.passed as f64 / r.labeled as f64);
        }
        if r.passed > 0 && r.cost_unknown == 0 {
            r.per_success = Some(r.dollars / r.passed as f64);
            r.quota_per_success = Some(r.quota as f64 / r.passed as f64);
        }
    }
    let (verdict, text) = verdict_of(&rule, &g, &c);
    let o = Object::new()
        .with("rule", rule.id.as_str())
        .with("version", Value::Int(rule.version.clone()))
        .with("state", rule.state.as_str())
        .with("seed", Value::Int(rule.seed.to_string()))
        .with("min_labeled", MIN_LABELED)
        .with("arms", Object::new().with("graft", Value::Obj(g.object())).with("control", Value::Obj(c.object())))
        .with("conflicting", conflicting)
        .with("verdict", verdict)
        .with("verdict_text", text.as_str());
    let mut lines = vec![format!(
        "trial (rule {} v{}, {}, seed {}): promotion is the operator's; nothing is changed here.",
        rule.id, rule.version, rule.state, rule.seed
    )];
    lines.push(g.line("graft"));
    lines.push(c.line("control"));
    if conflicting != 0 {
        lines.push(format!("  {conflicting} sessions with both arms recorded are left out."));
    }
    lines.push(format!("  promotion would: {text}"));
    Ok(Some((o, lines)))
}

/// `router_report._verdict`.
fn verdict_of(rule: &delegate::Rule, g: &Arm, c: &Arm) -> (&'static str, String) {
    if rule.state == "audit" {
        return (
            "none",
            "nothing: the rule is in audit, so both arms only record. Set its state to trial to compare them.".into(),
        );
    }
    if g.labeled < MIN_LABELED || c.labeled < MIN_LABELED {
        return (
            "wait",
            format!(
                "wait: each arm needs {MIN_LABELED} labeled sessions (graft {}, control {}).",
                g.labeled, c.labeled
            ),
        );
    }
    let (gr, cr) = (g.rate.unwrap_or(0.0), c.rate.unwrap_or(0.0));
    if gr < cr {
        return (
            "retire",
            format!(
                "retire rule {}: the graft arm's success rate {} is below the control arm's {}.",
                rule.id,
                fixed(gr, 2),
                fixed(cr, 2)
            ),
        );
    }
    let (Some(gd), Some(cd)) = (g.per_success, c.per_success) else {
        return ("wait", "wait: a labeled session's billed cost is unknown, or an arm has no success.".into());
    };
    if gd < cd {
        return (
            "promote",
            format!(
                "promote: set rule {} to active (billed cost per success ${} against ${}).",
                rule.id,
                fixed(gd, 4),
                fixed(cd, 4)
            ),
        );
    }
    ("keep", format!("keep the trial: billed cost per success ${} is not below ${}.", fixed(gd, 4), fixed(cd, 4)))
}
