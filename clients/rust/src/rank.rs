//! `opendaisugi.rank`: which of N attempts at one task is best, and how
//! sure we are. Elimination, quality tiers, Bradley-Terry over the judges'
//! votes that hold in both orders, with a weak prior and a seeded
//! bootstrap, the owner's answers as order constraints, and cost last; then
//! the choice record and its cards. The arithmetic follows the oracle step
//! by step (RK-R-5, RK-R-6). The Go client's `internal/rank` is the
//! reference.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use num_bigint::BigInt;
use regex::Regex;
use rusqlite::{Connection, OpenFlags};

use crate::gate::py::text::{decode_utf8_replace, strip};
use crate::gate::pyjson::{canonical_json_ascii, dumps, float_repr, loads_py, round, Object, Value};
use crate::gate::sha256::hexdigest;

pub const MAX_ATTEMPTS: usize = 8;
pub const MAX_BOOTSTRAP: i64 = 10000;
const MAX_ITER: usize = 500;
const TOL: f64 = 1e-10;
pub const DECAY_SECONDS: f64 = 7.0 * 86400.0;
const COST_KEYS: [&str; 3] = ["tokens", "wall_ms", "diff_lines"];

type R<T> = Result<T, String>;

/// `time.time()`.
pub fn now() -> f64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0)
}

/// `round(x, 9)`.
pub fn r9(x: f64) -> f64 {
    round(x, 9)
}

#[derive(Clone)]
pub struct Attempt {
    pub id: String,
    pub content_hash: String,
    pub edge_proof: Option<String>,
    pub started_by_parent: bool,
    pub ran_plan: bool,
    verify: Option<(bool, i64)>,
    gate_override: Option<i64>,
    tests: Vec<(String, bool, String)>,
    features: Vec<(String, bool, bool)>,
    cost: BTreeMap<&'static str, i64>,
    estimated: Vec<&'static str>,
    pub where_: Option<Value>,
    pub summary: String,
}

impl Attempt {
    /// The cost as the oracle keeps it: tokens, wall_ms, diff_lines when
    /// known, then estimated.
    pub fn cost_obj(&self) -> Object {
        let mut o = Object::new();
        for k in COST_KEYS {
            if let Some(v) = self.cost.get(k) {
                o.set(k, *v);
            }
        }
        if !self.estimated.is_empty() {
            o.set("estimated", Value::List(self.estimated.iter().map(|s| Value::from(*s)).collect()));
        }
        o
    }
}

#[derive(Clone, Debug)]
pub struct Comparison {
    pub a: String,
    pub b: String,
    pub a_hash: String,
    pub b_hash: String,
    pub shown: String,
    pub outcome: String,
    pub judge: String,
    pub pair_id: String,
}

impl Comparison {
    fn canon(&self) -> Value {
        let mut o = Object::new();
        o.set("a", self.a.as_str()).set("a_hash", self.a_hash.as_str()).set("b", self.b.as_str());
        o.set("b_hash", self.b_hash.as_str()).set("judge", self.judge.as_str()).set("outcome", self.outcome.as_str());
        o.set("pair_id", self.pair_id.as_str()).set("shown", self.shown.as_str());
        Value::Obj(o)
    }
}

#[derive(Clone)]
pub struct Policy {
    pub threshold: f64,
    pub prior: f64,
    pub bootstrap: usize,
    pub judges: Vec<String>,
}

pub type Pair = (String, String);

pub struct Ranking {
    pub id: String,
    pub task: String,
    pub project: String,
    pub attempts: Vec<Attempt>,
    pub comparisons: Option<Vec<Comparison>>,
    pub owner: Option<Vec<Pair>>,
    pub policy: Policy,
}

fn big(v: &Value) -> Option<BigInt> {
    match v {
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

fn count(v: &Value, what: &str) -> R<i64> {
    let lim = BigInt::from(1u64 << 53);
    match big(v) {
        Some(n) if n >= BigInt::from(0) && n <= lim => Ok(n.to_string().parse().unwrap_or(0)),
        _ => Err(format!("{what} must be a whole number from 0 to 2**53")),
    }
}

fn text(v: &Value, what: &str) -> R<String> {
    match v {
        Value::Str(s) => Ok(s.clone()),
        _ => Err(format!("{what} must be a string")),
    }
}

fn flag(v: &Value, what: &str) -> R<bool> {
    match v {
        Value::Bool(b) => Ok(*b),
        _ => Err(format!("{what} must be true or false")),
    }
}

fn obj<'a>(v: &'a Value, what: &str) -> R<&'a Object> {
    match v {
        Value::Obj(o) => Ok(o),
        _ => Err(format!("{what} must be an object")),
    }
}

fn list<'a>(v: &'a Value, what: &str) -> R<&'a Vec<Value>> {
    match v {
        Value::List(l) => Ok(l),
        _ => Err(format!("{what} must be a list")),
    }
}

/// A JSON number as a finite float (an int or a float, never a bool).
pub fn finite(v: &Value) -> Option<f64> {
    let f = match v {
        Value::Int(t) => t.parse::<f64>().ok()?,
        Value::Float(f) => *f,
        _ => return None,
    };
    f.is_finite().then_some(f)
}

fn number(v: &Value, what: &str) -> R<f64> {
    if !matches!(v, Value::Int(_) | Value::Float(_)) {
        return Err(format!("{what} must be a number"));
    }
    finite(v).ok_or_else(|| format!("{what} must be a finite number"))
}

/// `o.get(k, def)`: def when absent; the value (Null for null) when present.
fn get_def<'a>(o: &'a Object, k: &str, def: &'a Value) -> &'a Value {
    o.get(k).unwrap_or(def)
}

fn one_of(v: &Value, set: &[&str]) -> Option<String> {
    match v {
        Value::Str(s) if set.contains(&s.as_str()) => Some(s.clone()),
        _ => None,
    }
}

fn re(pat: &str) -> Regex {
    Regex::new(pat).unwrap()
}

fn parse_attempt(raw: &Value, i: usize) -> R<Attempt> {
    let o = obj(raw, &format!("attempts[{i}]"))?;
    let aid = match o.value("id") {
        Value::Str(s) if re(r"^[A-Za-z0-9._#-]{1,32}$").is_match(s) => s.clone(),
        _ => return Err(format!("attempts[{i}].id must be 1 to 32 of A-Z a-z 0-9 . _ # -")),
    };
    let w = format!("attempt {aid}");
    let h = match o.value("content_hash") {
        Value::Str(s) if !s.is_empty() => s.clone(),
        _ => return Err(format!("{w}: content_hash must be a string that is not empty")),
    };
    let mut a = Attempt {
        id: aid,
        content_hash: h,
        edge_proof: None,
        started_by_parent: false,
        ran_plan: false,
        verify: None,
        gate_override: None,
        tests: vec![],
        features: vec![],
        cost: BTreeMap::new(),
        estimated: vec![],
        where_: None,
        summary: String::new(),
    };
    let null = Value::Null;
    if !o.value("author").is_null() {
        text(o.value("author"), &format!("{w}: author"))?;
    }
    let ep = o.value("edge_proof");
    if !ep.is_null() {
        a.edge_proof = Some(one_of(ep, &["ok", "failed"]).ok_or_else(|| format!("{w}: edge_proof must be \"ok\", \"failed\" or absent"))?);
    }
    if !o.value("started_by_parent").is_null() {
        a.started_by_parent = flag(o.value("started_by_parent"), &format!("{w}: started_by_parent"))?;
    }
    if !o.value("ran_plan").is_null() {
        a.ran_plan = flag(o.value("ran_plan"), &format!("{w}: ran_plan"))?;
    }
    let zero = Value::Int("0".into());
    if !o.value("verify").is_null() {
        let v = obj(o.value("verify"), &format!("{w}: verify"))?;
        let ok = flag(v.value("ok"), &format!("{w}: verify.ok"))?;
        let n = count(get_def(v, "violations", &zero), &format!("{w}: verify.violations"))?;
        a.verify = Some((ok, n));
    }
    if !o.value("gate").is_null() {
        let g = obj(o.value("gate"), &format!("{w}: gate"))?;
        count(get_def(g, "denies", &zero), &format!("{w}: gate.denies"))?;
        a.gate_override = Some(count(get_def(g, "override_allows", &zero), &format!("{w}: gate.override_allows"))?);
    }
    let empty = Value::List(vec![]);
    let fals = Value::Bool(false);
    for (j, t) in list(get_def(o, "tests", &empty), &format!("{w}: tests"))?.iter().enumerate() {
        let to = obj(t, &format!("{w}: tests[{j}]"))?;
        let res = one_of(to.value("result"), &["pass", "fail", "not_run"])
            .ok_or_else(|| format!("{w}: tests[{j}].result must be \"pass\", \"fail\" or \"not_run\""))?;
        let name = text(to.value("name"), &format!("{w}: tests[{j}].name"))?;
        let req = flag(get_def(to, "required", &fals), &format!("{w}: tests[{j}].required"))?;
        a.tests.push((name, req, res));
    }
    for (j, f) in list(get_def(o, "features", &empty), &format!("{w}: features"))?.iter().enumerate() {
        let fo = obj(f, &format!("{w}: features[{j}]"))?;
        let name = text(fo.value("name"), &format!("{w}: features[{j}].name"))?;
        let req = flag(get_def(fo, "required", &fals), &format!("{w}: features[{j}].required"))?;
        let green = flag(fo.value("green"), &format!("{w}: features[{j}].green"))?;
        a.features.push((name, req, green));
    }
    let eo = Value::Obj(Object::new());
    let cost = obj(get_def(o, "cost", &eo), &format!("{w}: cost"))?;
    for k in COST_KEYS {
        let v = cost.value(k);
        if !v.is_null() {
            a.cost.insert(k, count(v, &format!("{w}: cost.{k}"))?);
        }
    }
    let est = list(get_def(cost, "estimated", &empty), &format!("{w}: cost.estimated"))?;
    for k in est {
        if one_of(k, &COST_KEYS).is_none() {
            return Err(format!("{w}: cost.estimated names tokens, wall_ms or diff_lines only"));
        }
    }
    for k in COST_KEYS {
        if est.iter().any(|e| e.as_str() == Some(k)) {
            a.estimated.push(k);
        }
    }
    let wv = o.value("where");
    if !wv.is_null() {
        a.where_ = Some(wv.clone());
    }
    let pv = o.value("previews");
    if !pv.is_null() {
        let po = obj(pv, &format!("{w}: previews"))?;
        if !po.value("summary").is_null() {
            a.summary = text(po.value("summary"), &format!("{w}: previews.summary"))?;
        }
    }
    let _ = &null;
    Ok(a)
}

/// `rank._comparison`.
pub fn parse_comparison(raw: &Value, i: usize) -> R<Comparison> {
    let w = format!("comparisons[{i}]");
    let o = obj(raw, &w)?;
    let shown = one_of(o.value("shown"), &["ab", "ba"]).ok_or_else(|| format!("{w}.shown must be \"ab\" or \"ba\""))?;
    let outcome = one_of(o.value("outcome"), &["a", "b", "tie", "skip"])
        .ok_or_else(|| format!("{w}.outcome must be \"a\", \"b\", \"tie\" or \"skip\""))?;
    let c = Comparison {
        a: text(o.value("a"), &format!("{w}.a"))?,
        b: text(o.value("b"), &format!("{w}.b"))?,
        a_hash: text(o.value("a_hash"), &format!("{w}.a_hash"))?,
        b_hash: text(o.value("b_hash"), &format!("{w}.b_hash"))?,
        shown,
        outcome,
        judge: text(o.value("judge"), &format!("{w}.judge"))?,
        pair_id: text(o.value("pair_id"), &format!("{w}.pair_id"))?,
    };
    if c.a == c.b {
        return Err(format!("{w} compares {} with itself", c.a));
    }
    Ok(c)
}

fn parse_policy(raw: &Value) -> R<Policy> {
    let mut p = Policy { threshold: 0.9, prior: 1.0, bootstrap: 1000, judges: vec![] };
    if raw.is_null() {
        return Ok(p);
    }
    let o = obj(raw, "policy")?;
    if !o.value("threshold").is_null() {
        p.threshold = number(o.value("threshold"), "policy.threshold")?;
        if !(0.5..=1.0).contains(&p.threshold) {
            return Err("policy.threshold must be from 0.5 to 1".into());
        }
    }
    if !o.value("prior").is_null() {
        p.prior = number(o.value("prior"), "policy.prior")?;
        if !(0.01..=100.0).contains(&p.prior) {
            return Err("policy.prior must be from 0.01 to 100".into());
        }
    }
    if !o.value("bootstrap").is_null() {
        match big(o.value("bootstrap")) {
            Some(n) if n >= BigInt::from(1) && n <= BigInt::from(MAX_BOOTSTRAP) => {
                p.bootstrap = n.to_string().parse().unwrap_or(1)
            }
            _ => return Err(format!("policy.bootstrap must be a whole number from 1 to {MAX_BOOTSTRAP}")),
        }
    }
    if !o.value("judges").is_null() {
        for (j, name) in list(o.value("judges"), "policy.judges")?.iter().enumerate() {
            p.judges.push(text(name, &format!("policy.judges[{j}]"))?);
        }
    }
    Ok(p)
}

/// `rank.parse`.
pub fn parse(doc: &Value) -> R<Ranking> {
    let o = obj(doc, "the attempts file")?;
    let rid = match o.value("ranking_id") {
        Value::Str(s) if re(r"^[A-Za-z0-9._:#-]{1,96}$").is_match(s) => s.clone(),
        _ => return Err("ranking_id must be 1 to 96 of A-Z a-z 0-9 . _ : # -".into()),
    };
    let task = if o.value("task").is_null() { String::new() } else { text(o.value("task"), "task")? };
    let project = if o.value("project").is_null() { String::new() } else { text(o.value("project"), "project")? };
    let raws = list(o.value("attempts"), "attempts")?;
    if raws.is_empty() || raws.len() > MAX_ATTEMPTS {
        return Err(format!("attempts must hold 1 to {MAX_ATTEMPTS} attempts"));
    }
    let mut attempts = vec![];
    for (i, a) in raws.iter().enumerate() {
        attempts.push(parse_attempt(a, i)?);
    }
    let mut seen = BTreeSet::new();
    for a in &attempts {
        if !seen.insert(a.id.clone()) {
            return Err(format!("attempt id {} is used twice", a.id));
        }
    }
    let comparisons = if o.value("comparisons").is_null() {
        None
    } else {
        let mut cs = vec![];
        for (i, c) in list(o.value("comparisons"), "comparisons")?.iter().enumerate() {
            cs.push(parse_comparison(c, i)?);
        }
        Some(cs)
    };
    let owner = if o.value("owner_constraints").is_null() {
        None
    } else {
        let mut out = vec![];
        for (i, pr) in list(o.value("owner_constraints"), "owner_constraints")?.iter().enumerate() {
            let pair = match pr {
                Value::List(l) if l.len() == 2 => match (&l[0], &l[1]) {
                    (Value::Str(x), Value::Str(y)) => Some((x.clone(), y.clone())),
                    _ => None,
                },
                _ => None,
            };
            let Some((x, y)) = pair else {
                return Err(format!("owner_constraints[{i}] must be a list of two attempt ids"));
            };
            for id in [&x, &y] {
                if !seen.contains(id) {
                    return Err(format!("owner_constraints[{i}] names no attempt {id}"));
                }
            }
            if x == y {
                return Err(format!("owner_constraints[{i}] puts {x} before itself"));
            }
            out.push((x, y));
        }
        Some(out)
    };
    let policy = parse_policy(o.value("policy"))?;
    Ok(Ranking { id: rid, task, project, attempts, comparisons, owner, policy })
}

// ---------------------------------------------------------------------------
// The fit
// ---------------------------------------------------------------------------

/// `rank.eliminate`.
pub fn eliminate(a: &Attempt) -> Vec<String> {
    let mut why = vec![];
    if a.edge_proof.as_deref() == Some("failed") {
        why.push("edge proof failed: never ran".to_string());
    } else if a.started_by_parent && a.edge_proof.is_none() {
        why.push("edge proof missing".to_string());
    }
    match a.verify {
        Some((false, n)) => why.push(format!("verify failed: {n} violations")),
        None if a.ran_plan => why.push("verify missing".to_string()),
        _ => {}
    }
    for (name, req, res) in &a.tests {
        if *req && res == "fail" {
            why.push(format!("required test failed: {name}"));
        } else if *req && res == "not_run" {
            why.push(format!("required test not run: {name}"));
        }
    }
    for (name, req, green) in &a.features {
        if *req && !*green {
            why.push(format!("required feature not green: {name}"));
        }
    }
    why
}

fn quality_key(a: &Attempt) -> (usize, usize) {
    let t = a.tests.iter().filter(|(_, req, res)| !*req && res == "pass").count();
    let f = a.features.iter().filter(|(_, req, g)| !*req && *g).count();
    (t, f)
}

#[derive(Clone, Debug)]
pub struct Vote {
    pub i: String,
    pub j: String,
    pub win: f64,
    pub judge: String,
}

fn verdict(c: &Comparison) -> Option<String> {
    match c.outcome.as_str() {
        "skip" => None,
        "tie" => Some("tie".into()),
        "a" => Some(c.a.clone()),
        _ => Some(c.b.clone()),
    }
}

fn sorted2(a: &str, b: &str) -> (String, String) {
    if b < a {
        (b.to_string(), a.to_string())
    } else {
        (a.to_string(), b.to_string())
    }
}

/// `rank.votes_from`.
pub fn votes_from(comps: &[Comparison], by_id: &HashMap<String, &Attempt>, warnings: &mut Vec<String>) -> Vec<Vote> {
    let mut groups: HashMap<(String, String), Vec<&Comparison>> = HashMap::new();
    let mut order: Vec<(String, String)> = vec![];
    for c in comps {
        if let Some(x) = [&c.a, &c.b].into_iter().find(|x| !by_id.contains_key(*x)) {
            warnings.push(format!("a comparison by {} names no attempt {x}; ignored", c.judge));
            continue;
        }
        if c.a_hash != by_id[&c.a].content_hash || c.b_hash != by_id[&c.b].content_hash {
            warnings.push(format!("a comparison by {} on {} and {} binds to a hash that changed; ignored", c.judge, c.a, c.b));
            continue;
        }
        let k = (c.judge.clone(), c.pair_id.clone());
        if !groups.contains_key(&k) {
            order.push(k.clone());
        }
        groups.entry(k).or_default().push(c);
    }
    let mut keep: HashMap<(String, String, String), (String, Vote)> = HashMap::new();
    for k in &order {
        let rows = &groups[k];
        let (pi, pj) = sorted2(&rows[0].a, &rows[0].b);
        let mut good = rows.len() == 2;
        if good {
            let shown: BTreeSet<&str> = rows.iter().map(|r| r.shown.as_str()).collect();
            good = shown.contains("ab") && shown.contains("ba");
            for r in rows {
                if sorted2(&r.a, &r.b) != (pi.clone(), pj.clone()) {
                    good = false;
                }
            }
        }
        if !good {
            warnings.push(format!("judge {} call {} does not hold the pair once in each order; no vote", k.0, k.1));
            continue;
        }
        let (Some(v1), Some(v2)) = (verdict(rows[0]), verdict(rows[1])) else { continue };
        let win = if v1 == v2 && v1 != "tie" {
            if v1 == pi {
                1.0
            } else {
                0.0
            }
        } else {
            0.5
        };
        let vk = (k.0.clone(), pi.clone(), pj.clone());
        if let Some((old, _)) = keep.get(&vk) {
            warnings.push(format!("judge {} voted more than once on {pi} and {pj}; one vote kept", k.0));
            if k.1 >= *old {
                continue;
            }
        }
        keep.insert(vk, (k.1.clone(), Vote { i: pi, j: pj, win, judge: k.0.clone() }));
    }
    let mut keys: Vec<_> = keep.keys().cloned().collect();
    keys.sort_by(|x, y| (&x.1, &x.2, &x.0).cmp(&(&y.1, &y.2, &y.0)));
    keys.into_iter().map(|k| keep[&k].1.clone()).collect()
}

/// `rank.bt_fit`: members in id order.
pub fn bt_fit(members: &[String], votes: &[Vote], prior: f64) -> HashMap<String, f64> {
    let idx: HashMap<&str, usize> = members.iter().enumerate().map(|(k, m)| (m.as_str(), k)).collect();
    let n = members.len();
    let mut wins = vec![prior / 2.0; n];
    let mut games = vec![vec![0.0f64; n]; n];
    for v in votes {
        let (a, b) = (idx[v.i.as_str()], idx[v.j.as_str()]);
        wins[a] += v.win;
        wins[b] += 1.0 - v.win;
        games[a][b] += 1.0;
        games[b][a] += 1.0;
    }
    let mut s = vec![1.0f64; n];
    for _ in 0..MAX_ITER {
        let mut next = vec![0.0f64; n];
        for a in 0..n {
            let mut denom = prior / (s[a] + 1.0);
            for b in 0..n {
                if b != a && games[a][b] != 0.0 {
                    denom += games[a][b] / (s[a] + s[b]);
                }
            }
            next[a] = wins[a] / denom;
        }
        let mut diff = 0.0f64;
        for a in 0..n {
            let d = (next[a] - s[a]).abs();
            if d > diff {
                diff = d;
            }
        }
        s = next;
        if diff < TOL {
            break;
        }
    }
    members.iter().map(|m| (m.clone(), s[idx[m.as_str()]])).collect()
}

fn p_ref(s: f64) -> f64 {
    s / (s + 1.0)
}

/// `rank.SplitMix64`.
pub struct SplitMix64(pub u64);

impl SplitMix64 {
    #[allow(clippy::should_implement_trait)]
    pub fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9E3779B97F4A7C15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58476D1CE4E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D049BB133111EB);
        z ^ (z >> 31)
    }
}

fn strs(v: &[String]) -> Value {
    Value::List(v.iter().map(Value::from).collect())
}

/// `rank.seed_of`.
pub fn seed_of(r: &Ranking, comps: &[Comparison], owner: &[Pair]) -> String {
    let mut atts = vec![];
    for a in &r.attempts {
        let mut o = Object::new();
        o.set("content_hash", a.content_hash.as_str()).set("cost", a.cost_obj());
        o.set("edge_proof", a.edge_proof.clone());
        let feats: Vec<Value> = a
            .features
            .iter()
            .map(|(n, r, g)| {
                let mut f = Object::new();
                f.set("name", n.as_str()).set("required", *r).set("green", *g);
                Value::Obj(f)
            })
            .collect();
        o.set("features", Value::List(feats)).set("id", a.id.as_str());
        o.set("ran_plan", a.ran_plan).set("started_by_parent", a.started_by_parent);
        let tests: Vec<Value> = a
            .tests
            .iter()
            .map(|(n, r, x)| {
                let mut t = Object::new();
                t.set("name", n.as_str()).set("required", *r).set("result", x.as_str());
                Value::Obj(t)
            })
            .collect();
        o.set("tests", Value::List(tests));
        let verify = a.verify.map(|(ok, n)| {
            let mut v = Object::new();
            v.set("ok", ok).set("violations", n);
            Value::Obj(v)
        });
        o.set("verify", verify);
        atts.push(Value::Obj(o));
    }
    let cs: Vec<Value> = comps.iter().map(|c| c.canon()).collect();
    let ow: Vec<Value> = owner.iter().map(|(w, l)| Value::List(vec![w.into(), l.into()])).collect();
    let mut pol = Object::new();
    pol.set("bootstrap", r.policy.bootstrap as i64).set("judges", strs(&r.policy.judges));
    pol.set("prior", r.policy.prior).set("threshold", r.policy.threshold);
    let mut doc = Object::new();
    doc.set("attempts", Value::List(atts)).set("comparisons", Value::List(cs)).set("owner_constraints", Value::List(ow));
    doc.set("policy", pol).set("ranking_id", r.id.as_str());
    hexdigest(canonical_json_ascii(&Value::Obj(doc)).as_bytes())[..16].to_string()
}

fn closure(edges: &[Pair]) -> BTreeSet<Pair> {
    let mut succ: BTreeMap<String, BTreeSet<String>> = BTreeMap::new();
    for (w, l) in edges {
        succ.entry(w.clone()).or_default().insert(l.clone());
    }
    let mut out = BTreeSet::new();
    for (start, next) in &succ {
        let mut todo: Vec<String> = next.iter().cloned().collect();
        let mut seen = BTreeSet::new();
        while let Some(x) = todo.pop() {
            if !seen.insert(x.clone()) {
                continue;
            }
            out.insert((start.clone(), x.clone()));
            if let Some(n) = succ.get(&x) {
                todo.extend(n.iter().cloned());
            }
        }
    }
    out
}

fn owner_edges(pairs: &[Pair], surv: &BTreeSet<String>, warnings: &mut Vec<String>) -> Vec<Pair> {
    let mut edges: Vec<Pair> = vec![];
    for p in pairs {
        if surv.contains(&p.0) && surv.contains(&p.1) && !edges.contains(p) {
            edges.push(p.clone());
        }
    }
    let clo = closure(&edges);
    let cyc: BTreeSet<String> = clo.iter().filter(|(x, y)| clo.contains(&(y.clone(), x.clone()))).map(|(x, _)| x.clone()).collect();
    if !cyc.is_empty() {
        let names: Vec<String> = cyc.iter().cloned().collect();
        warnings.push(format!("the owner's answers form a cycle among {}; none of them applied", names.join(", ")));
        edges.retain(|(w, l)| !(cyc.contains(w) && cyc.contains(l)));
    }
    edges
}

fn apply_owner(base: &[String], edges: &[Pair]) -> Vec<String> {
    let mut placed: Vec<String> = vec![];
    let mut left: Vec<String> = base.to_vec();
    while !left.is_empty() {
        let pos = left.iter().position(|x| edges.iter().filter(|(_, l)| l == x).all(|(w, _)| placed.contains(w)));
        let Some(i) = pos else { break };
        placed.push(left.remove(i));
    }
    placed
}

/// `rank._cost_key`: an unknown cost sorts after every known one.
fn cost_key(a: &Attempt) -> [i64; 6] {
    let mut k = [0i64; 6];
    for (i, name) in COST_KEYS.iter().enumerate() {
        match a.cost.get(name) {
            None => k[2 * i] = 1,
            Some(v) => k[2 * i + 1] = *v,
        }
    }
    k
}

/// `rank._join`.
pub fn join(items: &[String]) -> String {
    if items.len() <= 1 {
        return items.concat();
    }
    format!("{} and {}", items[..items.len() - 1].join(", "), items[items.len() - 1])
}

fn cmpf(a: f64, b: f64) -> std::cmp::Ordering {
    a.partial_cmp(&b).unwrap_or(std::cmp::Ordering::Equal)
}

/// `rank.fit`. comps and owner stand in for the file's own when it has none.
pub fn fit(r: &Ranking, comps: Vec<Comparison>, owner: Vec<Pair>, warnings: Vec<String>) -> Object {
    let mut warnings = warnings;
    let comps = r.comparisons.clone().unwrap_or(comps);
    let owner = r.owner.clone().unwrap_or(owner);
    let by_id: HashMap<String, &Attempt> = r.attempts.iter().map(|a| (a.id.clone(), a)).collect();
    let mut eliminated = vec![];
    let mut survivors: Vec<&Attempt> = vec![];
    for a in &r.attempts {
        let why = eliminate(a);
        if !why.is_empty() {
            let mut e = Object::new();
            e.set("id", a.id.as_str()).set("reasons", strs(&why));
            eliminated.push(Value::Obj(e));
            continue;
        }
        survivors.push(a);
        if let Some(n) = a.gate_override {
            if n > 0 {
                warnings.push(format!("{} was let out of its envelope {n} times by an operator allow; it is kept", a.id));
            }
        }
    }
    let seed = seed_of(r, &comps, &owner);
    let mut out = Object::new();
    out.set("ranking_id", r.id.as_str()).set("status", "none_survived").set("eliminated", Value::List(eliminated));
    out.set("quality_tiers", Value::List(vec![])).set("order", Value::List(vec![])).set("decided_by", Value::List(vec![]));
    out.set("scores", Object::new()).set("leader", Value::Null).set("confidence", Value::Null);
    out.set("label", "no_label").set("next", Value::Null).set("stop", "nothing_to_ask");
    out.set("owner_constraints", Value::List(vec![]));
    let finish = |mut out: Object, warnings: &[String]| -> Object {
        out.set("warnings", strs(warnings)).set("seed", seed.as_str());
        out
    };
    if survivors.is_empty() {
        return finish(out, &warnings);
    }
    let surv: BTreeSet<String> = survivors.iter().map(|a| a.id.clone()).collect();
    let sb: HashMap<String, &Attempt> = survivors.iter().map(|a| (a.id.clone(), *a)).collect();
    let mut keys: Vec<(usize, usize)> = vec![];
    for a in &survivors {
        let k = quality_key(a);
        if !keys.contains(&k) {
            keys.push(k);
        }
    }
    keys.sort_by(|x, y| y.cmp(x));
    let mut tiers: Vec<Vec<String>> = vec![];
    let mut tier_of: HashMap<String, usize> = HashMap::new();
    for (t, k) in keys.iter().enumerate() {
        let mut members: Vec<String> = survivors.iter().filter(|a| quality_key(a) == *k).map(|a| a.id.clone()).collect();
        members.sort();
        for m in &members {
            tier_of.insert(m.clone(), t);
        }
        tiers.push(members);
    }
    if survivors.len() > 1 && tiers.len() == 1 && keys[0] == (0, 0) {
        warnings.push("no optional test or feature separates the attempts; only opinion ranks them".into());
    }
    let votes: Vec<Vote> = votes_from(&comps, &by_id, &mut warnings)
        .into_iter()
        .filter(|v| surv.contains(&v.i) && surv.contains(&v.j) && tier_of[&v.i] == tier_of[&v.j])
        .collect();
    let edges = owner_edges(&owner, &surv, &mut warnings);
    let clo = closure(&edges);
    let prior = r.policy.prior;
    let mut point: HashMap<String, f64> = HashMap::new();
    let mut tier_votes: Vec<Vec<Vote>> = vec![];
    for members in &tiers {
        let tv: Vec<Vote> = votes.iter().filter(|v| members.contains(&v.i)).cloned().collect();
        point.extend(bt_fit(members, &tv, prior));
        tier_votes.push(tv);
    }
    let mut rng = SplitMix64(u64::from_str_radix(&seed, 16).unwrap_or(0));
    let b_n = r.policy.bootstrap;
    let mut samples: HashMap<String, Vec<f64>> = HashMap::new();
    let mut boots: Vec<HashMap<String, f64>> = Vec::with_capacity(b_n);
    for _ in 0..b_n {
        let mut refit = HashMap::new();
        for (members, tv) in tiers.iter().zip(&tier_votes) {
            if members.len() < 2 {
                refit.extend(bt_fit(members, &[], prior));
                continue;
            }
            let mut draw = vec![];
            for _ in 0..tv.len() {
                draw.push(tv[(rng.next() % tv.len() as u64) as usize].clone());
            }
            refit.extend(bt_fit(members, &draw, prior));
        }
        for m in &surv {
            samples.entry(m.clone()).or_default().push(p_ref(refit[m]));
        }
        boots.push(refit);
    }
    let mut beats: HashMap<(String, String), usize> = HashMap::new();
    for members in &tiers {
        for x in members {
            for y in members {
                if x != y {
                    let n = boots.iter().filter(|rf| r9(rf[x]) > r9(rf[y])).count();
                    beats.insert((x.clone(), y.clone()), n);
                }
            }
        }
    }
    let mut base: Vec<String> = vec![];
    let mut klass: HashMap<String, usize> = HashMap::new();
    let mut next_class = 0;
    for members in &tiers {
        let mut pref = members.clone();
        pref.sort_by(|a, b| cmpf(r9(p_ref(point[b])), r9(p_ref(point[a]))).then_with(|| a.cmp(b)));
        let mut groups: Vec<Vec<String>> = vec![vec![pref[0].clone()]];
        for k in 1..pref.len() {
            let share = r9(beats[&(pref[k - 1].clone(), pref[k].clone())] as f64 / b_n as f64);
            if share >= r.policy.threshold {
                groups.push(vec![pref[k].clone()]);
            } else {
                groups.last_mut().unwrap().push(pref[k].clone());
            }
        }
        for g in groups {
            let mut gs = g.clone();
            gs.sort_by(|a, b| cost_key(sb[a]).cmp(&cost_key(sb[b])).then_with(|| a.cmp(b)));
            for m in gs {
                klass.insert(m.clone(), next_class);
                base.push(m);
            }
            next_class += 1;
        }
    }
    let order = apply_owner(&base, &edges);
    let pos: HashMap<&String, usize> = base.iter().enumerate().map(|(k, m)| (m, k)).collect();
    let mut decided = vec![];
    for w in order.windows(2) {
        let (x, y) = (&w[0], &w[1]);
        let d = if edges.contains(&(x.clone(), y.clone())) || pos[x] > pos[y] {
            "owner"
        } else if tier_of[x] != tier_of[y] {
            "quality"
        } else if klass[x] != klass[y] {
            "preference"
        } else if cost_key(sb[x]) != cost_key(sb[y]) {
            "cost"
        } else {
            "tie"
        };
        decided.push(Value::from(d));
    }
    let lo_i = (5 * (b_n - 1)) / 100;
    let hi_i = (95 * (b_n - 1)) / 100;
    let mut scores = Object::new();
    for m in &order {
        let mut vals = samples[m].clone();
        vals.sort_by(|a, b| cmpf(*a, *b));
        let nv = votes.iter().filter(|v| &v.i == m || &v.j == m).count() as i64;
        let mut sc = Object::new();
        sc.set("strength", r9(p_ref(point[m]))).set("lo", r9(vals[lo_i])).set("hi", r9(vals[hi_i])).set("votes", nv);
        scores.set(m, sc);
    }
    let leader = order[0].clone();
    let rivals: Vec<String> = tiers[tier_of[&leader]].iter().filter(|m| **m != leader).cloned().collect();
    let confidence = if rivals.is_empty() {
        1.0
    } else {
        let n = boots
            .iter()
            .filter(|rf| rivals.iter().all(|m| clo.contains(&(leader.clone(), m.clone())) || r9(rf[&leader]) > r9(rf[m])))
            .count();
        r9(n as f64 / b_n as f64)
    };
    let owner_pick = edges.iter().any(|(w, _)| *w == leader);
    out.set("quality_tiers", Value::List(tiers.iter().map(|t| strs(t)).collect()));
    out.set("order", strs(&order)).set("decided_by", Value::List(decided)).set("scores", scores);
    out.set("leader", leader.as_str()).set("confidence", confidence);
    out.set("owner_constraints", Value::List(edges.iter().map(|(w, l)| Value::List(vec![w.into(), l.into()])).collect()));
    if survivors.len() == 1 {
        out.set("status", "single").set("label", leader.as_str()).set("stop", "nothing_to_ask");
        return finish(out, &warnings);
    }
    if owner_pick || confidence >= r.policy.threshold {
        out.set("status", "ranked").set("label", leader.as_str()).set("stop", "confident");
        return finish(out, &warnings);
    }
    out.set("status", "provisional").set("label", "no_label");
    let voted: BTreeSet<(String, String, String)> = votes.iter().map(|v| (v.judge.clone(), v.i.clone(), v.j.clone())).collect();
    let near = |m: &String| -> (f64, usize, String) {
        let p = r9(point[&leader] / (point[&leader] + point[m]));
        let nv = votes.iter().filter(|v| (v.i == leader && &v.j == m) || (&v.i == m && v.j == leader)).count();
        (r9((p - 0.5).abs()), nv, m.clone())
    };
    let mut cands = rivals.clone();
    cands.sort_by(|a, b| {
        let (x, y) = (near(a), near(b));
        cmpf(x.0, y.0).then(x.1.cmp(&y.1)).then(x.2.cmp(&y.2))
    });
    for m in &cands {
        let (i, j) = sorted2(&leader, m);
        for judge in &r.policy.judges {
            if !voted.contains(&(judge.clone(), i.clone(), j.clone())) {
                let p = r9(point[&leader] / (point[&leader] + point[m]));
                let mut nx = Object::new();
                nx.set("pair", Value::List(vec![leader.as_str().into(), m.into()])).set("judge", judge.as_str());
                nx.set("why", format!("P({leader} beats {m}) is {}, the nearest to even", float_repr(p)));
                out.set("next", nx).set("stop", Value::Null);
                return finish(out, &warnings);
            }
        }
    }
    out.set("stop", "judges_exhausted");
    finish(out, &warnings)
}

/// `rank.EXIT`.
pub fn exit_code(status: &str) -> i32 {
    match status {
        "ranked" | "single" => 0,
        "provisional" => 3,
        _ => 1,
    }
}

fn fstr(v: &Value) -> String {
    match v {
        Value::Float(f) => float_repr(*f),
        Value::Int(t) => t.clone(),
        Value::Str(s) => s.clone(),
        other => dumps(other, false),
    }
}

fn s_of(v: &Value) -> String {
    v.as_str().unwrap_or("").to_string()
}

/// `rank.fit_text`.
pub fn fit_text(res: &Object) -> String {
    let mut b = format!("Ranking {}: {}\n", s_of(res.value("ranking_id")), s_of(res.value("status")));
    let empty = vec![];
    let order = match res.value("order") {
        Value::List(l) => l,
        _ => &empty,
    };
    let decided = match res.value("decided_by") {
        Value::List(l) => l,
        _ => &empty,
    };
    let scores = res.value("scores").as_obj().cloned().unwrap_or_default();
    for (k, mv) in order.iter().enumerate() {
        let m = s_of(mv);
        let sc = scores.value(&m).as_obj().cloned().unwrap_or_default();
        let how = if k > 0 { format!(" [{}]", s_of(&decided[k - 1])) } else { String::new() };
        b.push_str(&format!(
            "  {}. {m}: strength {} ({} to {}), {} votes{how}\n",
            k + 1,
            fstr(sc.value("strength")),
            fstr(sc.value("lo")),
            fstr(sc.value("hi")),
            fstr(sc.value("votes"))
        ));
    }
    if let Value::List(el) = res.value("eliminated") {
        for e in el {
            let eo = e.as_obj().cloned().unwrap_or_default();
            let rs: Vec<String> = match eo.value("reasons") {
                Value::List(l) => l.iter().map(s_of).collect(),
                _ => vec![],
            };
            b.push_str(&format!("  out: {}: {}\n", s_of(eo.value("id")), rs.join("; ")));
        }
    }
    if !res.value("leader").is_null() {
        b.push_str(&format!(
            "Leader: {} (confidence {}); label: {}\n",
            s_of(res.value("leader")),
            fstr(res.value("confidence")),
            s_of(res.value("label"))
        ));
    } else {
        b.push_str("No attempt survived; nothing is chosen.\n");
    }
    if let Value::List(oc) = res.value("owner_constraints") {
        for p in oc {
            if let Value::List(pl) = p {
                b.push_str(&format!("Owner: {} before {}\n", s_of(&pl[0]), s_of(&pl[1])));
            }
        }
    }
    if let Value::Obj(n) = res.value("next") {
        let pair = match n.value("pair") {
            Value::List(l) => l.clone(),
            _ => vec![],
        };
        b.push_str(&format!("Next: judge {} on {} and {}: {}\n", s_of(n.value("judge")), s_of(&pair[0]), s_of(&pair[1]), s_of(n.value("why"))));
    } else {
        b.push_str(&format!("Stop: {}\n", s_of(res.value("stop"))));
    }
    if let Value::List(ws) = res.value("warnings") {
        for w in ws {
            b.push_str(&format!("Warning: {}\n", s_of(w)));
        }
    }
    b
}

// ---------------------------------------------------------------------------
// Choices and cards
// ---------------------------------------------------------------------------

pub const CHEAP: &str = "cheap";
pub const COSTLY: &str = "costly";
pub const FOLLOW_UP: &str = "follow_up_only";
pub const SORTS: [&str; 4] = ["reversibility", "impact", "date", "project"];

/// `rank.rankings_dir`.
pub fn rankings_dir(data_dir: &str) -> String {
    format!("{data_dir}/journal/rankings")
}

/// `rank._read_rows`: the JSON object rows of a JSONL file, and how many
/// lines did not read.
pub(crate) fn read_rows(path: &str) -> (Vec<Object>, usize) {
    let Ok(raw) = std::fs::read(path) else { return (vec![], 0) };
    let text = decode_utf8_replace(&raw).replace("\r\n", "\n").replace('\r', "\n");
    let mut rows = vec![];
    let mut bad = 0;
    for line in text.split('\n') {
        if strip(line).is_empty() {
            continue;
        }
        match loads_py(line, 900) {
            Ok(Value::Obj(o)) => rows.push(o),
            _ => bad += 1,
        }
    }
    (rows, bad)
}

/// `rank.journal_comparisons`.
pub fn journal_comparisons(data_dir: &str, ranking_id: &str) -> (Vec<Comparison>, usize) {
    let (rows, mut bad) = read_rows(&format!("{}/comparisons.jsonl", rankings_dir(data_dir)));
    let mut out = vec![];
    for row in rows {
        if row.value("ranking_id").as_str() != Some(ranking_id) {
            continue;
        }
        match parse_comparison(&Value::Obj(row), out.len()) {
            Ok(c) => out.push(c),
            Err(_) => bad += 1,
        }
    }
    (out, bad)
}

/// `rank.choice_id`.
pub fn choice_id(ranking_id: &str, survivors: &[(String, String)]) -> String {
    let mut s = survivors.to_vec();
    s.sort();
    let l: Vec<Value> = s.iter().map(|(a, b)| Value::List(vec![a.into(), b.into()])).collect();
    let text = canonical_json_ascii(&Value::List(vec![ranking_id.into(), Value::List(l)]));
    format!("ch_{}", &hexdigest(text.as_bytes())[..12])
}

/// `rank.Card`: the fold of one choice's rows.
pub struct Card {
    pub opened: Object,
    pub close: Option<Object>,
    pub answer: Option<Object>,
    /// The runs that resumed the plan while the choice was open, each once,
    /// in file order.
    pub resumed: Vec<String>,
}

impl Card {
    pub fn id(&self) -> String {
        s_of(self.opened.value("choice_id"))
    }
    fn ts(&self) -> f64 {
        finite(self.opened.value("ts")).unwrap_or(0.0)
    }
    pub fn chosen(&self) -> String {
        s_of(self.opened.value("chosen"))
    }
    pub fn survivors(&self) -> Vec<(String, String)> {
        let mut out = vec![];
        if let Some(Value::List(l)) = self.opened.value("options").as_obj().map(|o| o.value("survivors")) {
            for s in l {
                if let Some(so) = s.as_obj() {
                    out.push((s_of(so.value("id")), s_of(so.value("content_hash"))));
                }
            }
        }
        out
    }
    fn facts(&self) -> Object {
        self.opened.value("facts").as_obj().cloned().unwrap_or_default()
    }
}

fn valid_open(row: &Object) -> bool {
    for k in ["choice_id", "ranking_id", "chosen"] {
        if row.value(k).as_str().is_none() {
            return false;
        }
    }
    if finite(row.value("ts")).is_none() {
        return false;
    }
    let Some(opts) = row.value("options").as_obj() else { return false };
    let Value::List(surv) = opts.value("survivors") else { return false };
    surv.iter().all(|s| {
        s.as_obj().is_some_and(|o| o.value("id").as_str().is_some() && o.value("content_hash").as_str().is_some())
    })
}

/// `rank.read_cards`.
pub fn read_cards(data_dir: &str) -> Vec<Card> {
    let (rows, _) = read_rows(&format!("{}/choices.jsonl", rankings_dir(data_dir)));
    let mut cards: Vec<Card> = vec![];
    let mut at: HashMap<String, usize> = HashMap::new();
    for row in rows {
        let Some(cid) = row.value("choice_id").as_str().map(String::from) else { continue };
        let ev = row.value("event").as_str().unwrap_or("").to_string();
        if row.value("event").as_str() == Some("opened") {
            if !at.contains_key(&cid) && valid_open(&row) {
                at.insert(cid, cards.len());
                cards.push(Card { opened: row, close: None, answer: None, resumed: vec![] });
            }
            continue;
        }
        let Some(&k) = at.get(&cid) else { continue };
        if ev == "resumed" {
            if let Some(run) = row.value("run_id").as_str() {
                if !cards[k].resumed.iter().any(|r| r == run) {
                    cards[k].resumed.push(run.to_string());
                }
            }
            continue;
        }
        if !["confirmed", "overridden", "ignored"].contains(&ev.as_str()) {
            continue;
        }
        let how = row.value("how").as_str().map(String::from);
        let card = &mut cards[k];
        if ev == "confirmed" || ev == "overridden" {
            if !matches!(how.as_deref(), Some("answer") | Some("drop") | Some("permanent_ask")) {
                continue;
            }
            if ev == "overridden" && row.value("pick").as_str().is_none() {
                continue;
            }
            if card.answer.is_none() {
                card.answer = Some(row.clone());
            }
        } else if how.as_deref() != Some("decay") {
            continue;
        }
        if card.close.is_none() {
            card.close = Some(row);
        }
    }
    cards
}

/// `rank.owner_answers`.
pub fn owner_answers(data_dir: &str, ranking_id: &str, by_id: &HashMap<String, &Attempt>, warnings: &mut Vec<String>) -> Vec<Pair> {
    let mut out: Vec<Pair> = vec![];
    for card in read_cards(data_dir) {
        let Some(answer) = &card.answer else { continue };
        if card.opened.value("ranking_id").as_str() != Some(ranking_id) {
            continue;
        }
        let mut ids: Vec<String> = vec![];
        let mut hashes: HashMap<String, String> = HashMap::new();
        for (id, h) in card.survivors() {
            if !hashes.contains_key(&id) {
                ids.push(id.clone());
            }
            hashes.insert(id, h);
        }
        if ids.iter().any(|id| by_id.get(id).is_none_or(|a| a.content_hash != hashes[id])) {
            warnings.push(format!("the owner's answer on {} binds to attempts that changed; ignored", card.id()));
            continue;
        }
        let chosen = card.chosen();
        let pairs: Vec<Pair> = if answer.value("event").as_str() == Some("confirmed") {
            ids.iter().filter(|id| **id != chosen).map(|id| (chosen.clone(), id.clone())).collect()
        } else {
            let pick = s_of(answer.value("pick"));
            if !hashes.contains_key(&pick) || pick == chosen {
                continue;
            }
            vec![(pick, chosen.clone())]
        };
        for p in pairs {
            if !out.contains(&p) {
                out.push(p);
            }
        }
    }
    out
}

/// `rank._receipts`: (step, reversibility, timestamp) of a run's receipts,
/// read-only.
fn receipts(data_dir: &str, run_id: &str) -> Vec<(String, String, f64)> {
    let path = format!("{data_dir}/journal/index.db");
    if std::fs::metadata(&path).is_err() {
        return vec![];
    }
    let dsn = if path.starts_with("file:") { format!("./{path}") } else { path };
    let flags = OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX;
    let Ok(con) = Connection::open_with_flags(dsn, flags) else { return vec![] };
    let Ok(mut st) = con.prepare("SELECT step_id, reversibility, timestamp FROM receipts WHERE run_id = ? ORDER BY timestamp, step_id")
    else {
        return vec![];
    };
    let rows = st.query_map([run_id], |r| {
        let step: rusqlite::types::Value = r.get(0)?;
        let rev: rusqlite::types::Value = r.get(1)?;
        let at: rusqlite::types::Value = r.get(2)?;
        Ok((step, rev, at))
    });
    let Ok(rows) = rows else { return vec![] };
    let mut out = vec![];
    for row in rows {
        let Ok((step, rev, at)) = row else { return vec![] };
        use rusqlite::types::Value as V;
        let t = match at {
            V::Real(f) => f,
            V::Integer(n) => n as f64,
            _ => continue,
        };
        let s = match step {
            V::Text(s) => s,
            V::Integer(n) => n.to_string(),
            V::Real(f) => float_repr(f),
            V::Null => "None".into(),
            V::Blob(b) => String::from_utf8_lossy(&b).into_owned(),
        };
        let r = match rev {
            V::Text(s) => s,
            _ => String::new(),
        };
        out.push((s, r, t));
    }
    out
}

pub struct Switch {
    pub cost: &'static str,
    pub undo_steps: i64,
    pub fired_at: Option<f64>,
    pub text: String,
}

/// `rank.switch_cost`; `to` names one alternative.
pub fn switch_cost(card: &Card, data_dir: &str, to: Option<&str>) -> Switch {
    let facts = card.facts();
    let chosen = card.chosen();
    let mut alts: Vec<String> = card.survivors().into_iter().map(|(id, _)| id).filter(|id| *id != chosen).collect();
    if let Some(t) = to {
        alts = vec![t.to_string()];
    }
    let where_ = facts.value("where").as_obj().cloned().unwrap_or_default();
    let gone: Vec<String> = alts
        .iter()
        .filter(|a| {
            where_.value(a).as_obj().and_then(|w| w.value("path").as_str().map(String::from)).is_some_and(|p| std::fs::metadata(&p).is_err())
        })
        .cloned()
        .collect();
    let mut later = vec![];
    if let Some(run) = facts.value("run").as_obj() {
        if !run.is_empty() {
            if let (Some(rid), Value::List(down)) = (run.value("run_id").as_str(), run.value("downstream")) {
                let set: BTreeSet<&str> = down.iter().filter_map(|d| d.as_str()).collect();
                let mut runs = vec![rid.to_string()];
                runs.extend(card.resumed.iter().filter(|r| r.as_str() != rid).cloned());
                for run in &runs {
                    later.extend(receipts(data_dir, run).into_iter().filter(|r| set.contains(r.0.as_str())));
                }
                later.sort_by(|a, b| a.2.partial_cmp(&b.2).unwrap_or(std::cmp::Ordering::Equal).then_with(|| a.0.cmp(&b.0)));
            }
        }
    }
    let hard: Vec<_> = later.iter().filter(|r| r.1 != "none" && r.1 != "reversible").collect();
    let undo: Vec<_> = later.iter().filter(|r| r.1 == "reversible").collect();
    if let Some(h) = hard.first() {
        return Switch {
            cost: FOLLOW_UP,
            undo_steps: 0,
            fired_at: Some(h.2),
            text: format!("Cannot undo: step {} ran on this choice and cannot be undone. Switching starts a follow-up task.", h.0),
        };
    }
    if !alts.is_empty() && gone.len() == alts.len() {
        return Switch {
            cost: FOLLOW_UP,
            undo_steps: 0,
            fired_at: None,
            text: format!("Cannot undo: the place of {} is gone. Switching starts a follow-up task.", join(&gone)),
        };
    }
    if !undo.is_empty() {
        let names: Vec<String> = undo.iter().map(|r| r.0.clone()).collect::<BTreeSet<_>>().into_iter().collect();
        return Switch {
            cost: COSTLY,
            undo_steps: undo.len() as i64,
            fired_at: None,
            text: format!("Switching undoes the later steps {} and applies the alternative.", join(&names)),
        };
    }
    Switch {
        cost: CHEAP,
        undo_steps: 0,
        fired_at: None,
        text: "Switching applies the alternative; no later step that changes anything ran on this choice.".into(),
    }
}

/// `rank.decay`: the ignored row an open card's decay would append.
pub fn decay(card: &Card, data_dir: &str, now: f64) -> Option<Object> {
    let sc = switch_cost(card, data_dir, None);
    let mut fires: Vec<(f64, &str)> = vec![];
    if sc.cost == FOLLOW_UP {
        fires.push((sc.fired_at.unwrap_or(now), "follow_up_only"));
    }
    let due = card.ts() + DECAY_SECONDS;
    if now >= due {
        fires.push((due, "time"));
    }
    let (at, trigger) = fires.into_iter().min_by(|a, b| cmpf(a.0, b.0).then(a.1.cmp(b.1)))?;
    let mut o = Object::new();
    o.set("choice_id", card.id()).set("ranking_id", card.opened.value("ranking_id").clone());
    o.set("event", "ignored").set("how", "decay").set("trigger", trigger).set("fired_at", at).set("ts", now);
    Some(o)
}

/// `rank.append_rows`.
pub fn append_rows(data_dir: &str, rows: &[Object]) -> std::io::Result<()> {
    use std::io::Write;
    if rows.is_empty() {
        return Ok(());
    }
    let d = rankings_dir(data_dir);
    std::fs::create_dir_all(&d)?;
    let mut f = std::fs::OpenOptions::new().append(true).create(true).open(format!("{d}/choices.jsonl"))?;
    let mut text = String::new();
    for r in rows {
        text.push_str(&dumps(&Value::Obj(r.clone()), true));
        text.push('\n');
    }
    f.write_all(text.as_bytes())?;
    f.sync_all()
}

/// `rank.sweep`.
pub fn sweep(data_dir: &str, now: f64) -> Vec<Object> {
    read_cards(data_dir).iter().filter(|c| c.close.is_none()).filter_map(|c| decay(c, data_dir, now)).collect()
}

/// `rank.opened_row`.
pub fn opened_row(r: &Ranking, res: &Object, now: f64, run: Option<Object>) -> Object {
    let by_id: HashMap<&str, &Attempt> = r.attempts.iter().map(|a| (a.id.as_str(), a)).collect();
    let order: Vec<String> = match res.value("order") {
        Value::List(l) => l.iter().map(s_of).collect(),
        _ => vec![],
    };
    let mut surv = vec![];
    let mut surv_out = vec![];
    let (mut cost, mut summary, mut where_) = (Object::new(), Object::new(), Object::new());
    for m in &order {
        let a = by_id[m.as_str()];
        surv.push((m.clone(), a.content_hash.clone()));
        let mut s = Object::new();
        s.set("id", m.as_str()).set("content_hash", a.content_hash.as_str());
        surv_out.push(Value::Obj(s));
        cost.set(m, a.cost_obj());
        if !a.summary.is_empty() {
            summary.set(m, a.summary.as_str());
        }
        if let Some(w) = &a.where_ {
            where_.set(m, w.clone());
        }
    }
    let mut opts = Object::new();
    opts.set("survivors", Value::List(surv_out)).set("eliminated", res.value("eliminated").clone());
    let mut facts = Object::new();
    facts.set("order", res.value("order").clone()).set("decided_by", res.value("decided_by").clone());
    facts.set("quality_tiers", res.value("quality_tiers").clone()).set("confidence", res.value("confidence").clone());
    facts.set("cost", cost).set("summary", summary).set("where", where_).set("run", run);
    let mut o = Object::new();
    o.set("choice_id", choice_id(&r.id, &surv)).set("ranking_id", r.id.as_str()).set("project", r.project.as_str());
    o.set("task", r.task.as_str()).set("event", "opened").set("options", opts);
    o.set("chosen", res.value("leader").clone()).set("status", res.value("status").clone()).set("facts", facts).set("ts", now);
    o
}

fn why_text(d: &str, x: &str, y: &str) -> Option<String> {
    Some(match d {
        "quality" => format!("{x} passed more optional tests or features than {y}."),
        "preference" => format!("The judges preferred {x} to {y}."),
        "cost" => format!("Nothing but cost separated {x} and {y}; {x} cost less."),
        "tie" => format!("Nothing separated {x} and {y}; {x} came first by name."),
        "owner" => format!("You placed {x} before {y}."),
        _ => return None,
    })
}

fn named(facts: &Object, m: &str) -> String {
    match facts.value("summary").as_obj().map(|s| s.value(m)) {
        Some(Value::Str(s)) if !s.is_empty() => format!("{m} ({s})"),
        _ => m.to_string(),
    }
}

fn bounded(v: &Value) -> Option<i64> {
    let n = big(v)?;
    (n >= BigInt::from(0) && n <= BigInt::from(1u64 << 53)).then(|| n.to_string().parse().unwrap_or(0))
}

/// One card as the queue shows it, with its sort keys.
pub struct View {
    pub obj: Object,
    cost: &'static str,
    opened_at: f64,
    confidence: Option<f64>,
    impact: i64,
    project: String,
    id: String,
}

/// `rank.card_view`.
pub fn card_view(card: &Card, data_dir: &str, now: f64) -> View {
    let o = &card.opened;
    let opened_at = card.ts();
    let facts = card.facts();
    let chosen = card.chosen();
    let alts: Vec<String> = card.survivors().into_iter().map(|(id, _)| id).filter(|id| *id != chosen).collect();
    let mut elim = vec![];
    if let Some(Value::List(el)) = o.value("options").as_obj().map(|x| x.value("eliminated")) {
        for e in el {
            if let Some(id) = e.as_obj().and_then(|eo| eo.value("id").as_str()) {
                elim.push(id.to_string());
            }
        }
    }
    let alts_named: Vec<String> = alts.iter().map(|a| named(&facts, a)).collect();
    let mut head = format!("Kept {} over {}", named(&facts, &chosen), join(&alts_named));
    if !elim.is_empty() {
        head.push_str(&format!("; {} failed a required check", join(&elim)));
    }
    head.push('.');
    let mut why = String::new();
    if let (Value::List(order), Value::List(decided)) = (facts.value("order"), facts.value("decided_by")) {
        if order.len() > 1 && !decided.is_empty() {
            if let (Some(d), Some(x), Some(y)) = (decided[0].as_str(), order[0].as_str(), order[1].as_str()) {
                if let Some(t) = why_text(d, x, y) {
                    why = t;
                }
            }
        }
    }
    let conf = finite(facts.value("confidence"));
    if let Some(c) = conf {
        why = strip(&format!("{why} Confidence {}.", float_repr(r9(c)))).to_string();
    }
    let sc = switch_cost(card, data_dir, None);
    let costs = facts.value("cost").as_obj().cloned().unwrap_or_default();
    let lines_of = |m: &str| costs.value(m).as_obj().and_then(|c| bounded(c.value("diff_lines")));
    let mut impact = 0i64;
    if let Some(base) = lines_of(&chosen) {
        for a in &alts {
            if let Some(la) = lines_of(a) {
                impact = impact.max((la - base).abs());
            }
        }
    }
    let project = o.value("project").as_str().unwrap_or("").to_string();
    let days = ((opened_at + DECAY_SECONDS - now) / 86400.0).ceil().max(0.0) as i64;
    let mut sw = Object::new();
    sw.set("cost", sc.cost).set("undo_steps", sc.undo_steps).set("text", sc.text.as_str());
    let mut v = Object::new();
    v.set("choice_id", card.id()).set("ranking_id", o.value("ranking_id").clone()).set("project", project.as_str());
    v.set("task", o.value("task").as_str().unwrap_or("")).set("chosen", chosen.as_str());
    v.set("alternatives", strs(&alts)).set("eliminated", strs(&elim));
    v.set("status", o.value("status").as_str().map(String::from));
    v.set("confidence", conf).set("opened_at", opened_at).set("decays_in_days", days);
    v.set("headline", head).set("why", why).set("switch", sw).set("impact_lines", impact);
    View { obj: v, cost: sc.cost, opened_at, confidence: conf, impact, project, id: card.id() }
}

fn cost_rank(c: &str) -> i32 {
    match c {
        CHEAP => 0,
        COSTLY => 1,
        _ => 2,
    }
}

/// `rank.sort_cards`.
pub fn sort_cards(vs: &mut [View], by: &str) {
    vs.sort_by(|a, b| {
        let o = match by {
            "reversibility" => cost_rank(a.cost).cmp(&cost_rank(b.cost)).then(cmpf(a.opened_at, b.opened_at)),
            "impact" => b.impact.cmp(&a.impact).then(cmpf(a.confidence.unwrap_or(1.0), b.confidence.unwrap_or(1.0))),
            "date" => cmpf(b.opened_at, a.opened_at),
            _ => a.project.cmp(&b.project).then(cmpf(b.opened_at, a.opened_at)),
        };
        o.then(a.id.cmp(&b.id))
    });
}

/// `rank.queue`: the open cards, and how many decayed unread.
pub fn queue(data_dir: &str, now: f64, by: &str) -> (Vec<View>, i64) {
    let mut views = vec![];
    let mut decayed = 0;
    for card in read_cards(data_dir) {
        if card.close.is_some() {
            continue;
        }
        if decay(&card, data_dir, now).is_some() {
            decayed += 1;
            continue;
        }
        views.push(card_view(&card, data_dir, now));
    }
    sort_cards(&mut views, by);
    (views, decayed)
}

/// The queue as `rank queue --json` prints it.
pub fn queue_json(views: &[View], decayed: i64) -> Object {
    let mut o = Object::new();
    o.set("cards", Value::List(views.iter().map(|v| Value::Obj(v.obj.clone())).collect())).set("decayed", decayed);
    o
}

fn iso_utc(t: f64) -> String {
    let secs = t.floor() as i64;
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    // Civil from days (Howard Hinnant's algorithm).
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!("{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z", rem / 3600, rem % 3600 / 60, rem % 60)
}

/// `rank.queue_text`.
pub fn queue_text(views: &[View], decayed: i64) -> String {
    let mut b = if views.is_empty() { "No open cards.\n".to_string() } else { format!("{} open cards:\n", views.len()) };
    for v in views {
        let o = &v.obj;
        let where_ = if v.project.is_empty() { String::new() } else { format!(", {}", v.project) };
        b.push_str(&format!("{} ({}{where_}), opened {}\n", v.id, s_of(o.value("ranking_id")), iso_utc(v.opened_at)));
        b.push_str(&format!("  {}\n", s_of(o.value("headline"))));
        let why = s_of(o.value("why"));
        if !why.is_empty() {
            b.push_str(&format!("  Why: {why}\n"));
        }
        let sw = o.value("switch").as_obj().cloned().unwrap_or_default();
        b.push_str(&format!("  Switching ({}): {}\n", s_of(sw.value("cost")), s_of(sw.value("text"))));
        b.push_str(&format!("  Decays in {} days unless switching gets expensive first.\n", fstr(o.value("decays_in_days"))));
    }
    if decayed > 0 {
        b.push_str(&format!("{decayed} cards decayed unread; the next writer records them as ignored.\n"));
    }
    b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splitmix_known_values() {
        let mut g = SplitMix64(0);
        assert_eq!([g.next(), g.next(), g.next()], [0xE220A8397B1DCDAF, 0x6E789E6AA1B965F4, 0x06C45D188009454F]);
    }

    #[test]
    fn iso_of_a_known_time() {
        assert_eq!(iso_utc(1759276800.5), "2025-10-01T00:00:00Z");
        assert_eq!(iso_utc(0.0), "1970-01-01T00:00:00Z");
    }

    #[test]
    fn a_duplicate_id_is_refused() {
        let v = crate::gate::pyjson::loads(r#"{"ranking_id": "r", "attempts": [{"id": "a", "content_hash": "x"}, {"id": "a", "content_hash": "y"}]}"#).unwrap();
        assert_eq!(parse(&v).err().as_deref(), Some("attempt id a is used twice"));
    }

    #[test]
    fn switch_cost_counts_the_runs_that_resumed_the_card() {
        let dir = std::env::temp_dir().join(format!("rank-resumed-{}", std::process::id()));
        let dd = dir.to_string_lossy().to_string();
        std::fs::create_dir_all(dir.join("journal/rankings")).unwrap();
        let con = rusqlite::Connection::open(dir.join("journal/index.db")).unwrap();
        con.execute_batch(
            "CREATE TABLE receipts (run_id TEXT, step_id TEXT, reversibility TEXT, timestamp REAL);
             INSERT INTO receipts VALUES ('run_b', 'w', 'reversible', 10.0);
             INSERT INTO receipts VALUES ('run_c', 'x', 'irreversible', 20.0);",
        )
        .unwrap();
        drop(con);
        let opened = r#"{"choice_id": "ch_000000000002", "ranking_id": "r0", "event": "opened", "options": {"survivors": [{"id": "a", "content_hash": "h-a"}, {"id": "b", "content_hash": "h-b"}], "eliminated": []}, "chosen": "a", "status": "provisional", "facts": {"run": {"run_id": "run_a", "step": "t", "downstream": ["w", "x"]}}, "ts": 1e12}"#;
        let resumed = |run: &str| format!(r#"{{"choice_id": "ch_000000000002", "ranking_id": "r0", "event": "resumed", "run_id": {run}, "ts": 1.0}}"#);
        let write = |rows: Vec<String>| {
            std::fs::write(dir.join("journal/rankings/choices.jsonl"), rows.join("\n") + "\n").unwrap();
            let mut cards = read_cards(&dd);
            assert_eq!(cards.len(), 1);
            cards.remove(0)
        };
        let c = write(vec![opened.into()]);
        assert!(c.resumed.is_empty());
        assert_eq!(switch_cost(&c, &dd, None).cost, CHEAP);
        let c = write(vec![opened.into(), resumed("\"run_b\"")]);
        let sc = switch_cost(&c, &dd, None);
        assert_eq!((sc.cost, sc.undo_steps), (COSTLY, 1));
        let c = write(vec![opened.into(), resumed("\"run_b\""), resumed("\"run_b\""), resumed("7"), resumed("\"run_c\"")]);
        assert_eq!(c.resumed, vec!["run_b".to_string(), "run_c".to_string()]);
        let sc = switch_cost(&c, &dd, None);
        assert_eq!((sc.cost, sc.fired_at), (FOLLOW_UP, Some(20.0)));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
