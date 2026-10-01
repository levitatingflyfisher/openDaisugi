//! `opendaisugi/tree.py`: the delegation tree's edge rule, its ledger,
//! and the operator's asks. The Go client's `internal/tree` is the
//! reference.
//!
//! An envelope here is a validated dump (`pmodel` Envelope): every field
//! in model order with its default, and no deadline key when it has none.

use std::collections::{BTreeSet, HashMap, HashSet};
use std::io::Write;

use num_bigint::BigInt;
use regex::Regex;

use crate::cli::gateroot::{envelopes_dir, join, safe_session_id};
use crate::gate::pyjson::{canonical_json_ascii, dumps, Object, Value};
use crate::pathways::pmodel::{validate_json, Id};

/// `tree.DEFAULT_TIMEOUT_MS`.
pub const DEFAULT_TIMEOUT_MS: u32 = 2000;
/// `tree.ASK_AFTER`.
pub const ASK_AFTER: usize = 3;

const METACHARS: [&str; 9] = [";", "|", "&", "`", "<", ">", "\n", "\r", "$("];

fn stakes_rank(s: &str) -> i32 {
    match s {
        "medium" => 1,
        "high" => 2,
        "physical" => 3,
        _ => 0,
    }
}

fn policy_rank(s: &str) -> i32 {
    match s {
        "strict" => 0,
        "allow" => 2,
        _ => 1,
    }
}

/// `tree.valid_session`.
pub fn valid_session(s: &str) -> bool {
    let re = Regex::new(r"^[A-Za-z0-9_-](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9_-])?$").unwrap();
    re.is_match(s) && s != "default" && s != "no-session"
}

/// `tree._q`: `json.dumps` of a value.
pub fn q(v: &Value) -> String {
    dumps(v, true)
}

fn qs(s: &str) -> String {
    q(&Value::Str(s.to_string()))
}

fn ql(l: &[String]) -> String {
    q(&Value::List(l.iter().map(|s| Value::Str(s.clone())).collect()))
}

fn strs(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l.iter().filter_map(|x| x.as_str().map(String::from)).collect(),
        _ => vec![],
    }
}

fn list(v: &Value) -> &[Value] {
    match v {
        Value::List(l) | Value::Tuple(l) => l,
        _ => &[],
    }
}

fn obj(v: &Value) -> Object {
    v.as_obj().cloned().unwrap_or_default()
}

fn num(v: &Value) -> f64 {
    match v {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse::<f64>().unwrap_or(0.0),
        _ => 0.0,
    }
}

fn big(v: &Value) -> BigInt {
    match v {
        Value::Int(t) => t.parse().unwrap_or_default(),
        _ => BigInt::default(),
    }
}

fn truthy(v: &Value) -> bool {
    matches!(v, Value::Bool(true))
}

/// `tree.EdgeResult`, without the counterexample (Python only).
pub struct EdgeResult {
    pub holds: bool,
    pub reasons: Vec<String>,
    /// The child as it registers; None with no envelope.
    pub child: Option<Object>,
    pub inherited: bool,
}

impl EdgeResult {
    /// `EdgeResult.doc()`.
    pub fn doc(&self) -> Object {
        let deadline = self.child.as_ref().map(|c| c.value("deadline").clone()).unwrap_or(Value::Null);
        Object::new()
            .with("holds", self.holds)
            .with("reasons", Value::List(self.reasons.iter().map(|r| Value::Str(r.clone())).collect()))
            .with("deadline", deadline)
            .with("deadline_inherited", self.inherited)
    }

    /// `tree.edge_text`.
    pub fn text(&self) -> String {
        let mut lines = vec![];
        if self.holds {
            lines.push("edge ok: the child fits inside the parent.".to_string());
            if let (true, Some(c)) = (self.inherited, &self.child) {
                lines.push(format!("The child takes the parent's deadline {}.", q(c.value("deadline"))));
            }
        } else {
            lines.push("edge refused:".to_string());
            lines.extend(self.reasons.iter().map(|r| format!("  {r}")));
        }
        lines.join("\n") + "\n"
    }
}

fn bounds(v: &Value) -> ([f64; 3], [f64; 3]) {
    let mut lo = [0.0; 3];
    let mut hi = [0.0; 3];
    let l = list(v);
    for (i, corner) in l.iter().take(2).enumerate() {
        for (k, x) in list(corner).iter().take(3).enumerate() {
            if i == 0 {
                lo[k] = num(x);
            } else {
                hi[k] = num(x);
            }
        }
    }
    (lo, hi)
}

fn pair(v: &Value) -> (f64, f64) {
    let l = list(v);
    if l.len() == 2 {
        (num(&l[0]), num(&l[1]))
    } else {
        (0.0, 0.0)
    }
}

fn box_key(v: &Value) -> Vec<u64> {
    let (lo, hi) = bounds(v);
    lo.iter().chain(hi.iter()).map(|f| if *f == 0.0 { 0.0f64.to_bits() } else { f.to_bits() }).collect()
}

fn robot_reason(parent: &Object, child: &Object) -> Option<String> {
    let o = obj(parent.value("permissions"));
    let i = obj(child.value("permissions"));
    let ow = o.value("workspace_bounds");
    if !ow.is_null() {
        let iw = i.value("workspace_bounds");
        let (olo, ohi) = bounds(ow);
        let mut bad = iw.is_null();
        if !bad {
            let (ilo, ihi) = bounds(iw);
            bad = (0..3).any(|k| ilo[k] < olo[k] || ihi[k] > ohi[k]);
        }
        if bad {
            return Some("robot: the child's workspace_bounds are not inside the parent's".into());
        }
    }
    for axis in ["velocity_limit", "torque_limit"] {
        let ov = o.value(axis);
        if !ov.is_null() {
            let iv = i.value(axis);
            if iv.is_null() || num(iv) > num(ov) {
                return Some(format!("robot: the child's {axis} is not within the parent's"));
            }
        }
    }
    let oj = obj(o.value("joint_limits"));
    let ij = obj(i.value("joint_limits"));
    for j in oj.keys() {
        let (olo, ohi) = pair(oj.value(j));
        let bad = match ij.get(j) {
            None => true,
            Some(iv) => {
                let (ilo, ihi) = pair(iv);
                ilo < olo || ihi > ohi
            }
        };
        if bad {
            return Some(format!("robot: the child's range for joint {} is not within the parent's", qs(j)));
        }
    }
    let have: HashSet<Vec<u64>> = list(i.value("obstacles")).iter().map(box_key).collect();
    let missing: HashSet<Vec<u64>> = list(o.value("obstacles")).iter().map(box_key).filter(|k| !have.contains(k)).collect();
    if !missing.is_empty() {
        return Some(format!("robot: the child drops {} of the parent's obstacles", missing.len()));
    }
    None
}

fn scope_reasons(parent: &Object, child: &Object, timeout_ms: u32) -> Vec<String> {
    use crate::subsumption::{glob_unsupported, pattern_fits, Fit};
    let mut out = vec![];
    let o = obj(parent.value("permissions"));
    let i = obj(child.value("permissions"));
    if truthy(i.value("shell_allow_decomposition")) && !truthy(o.value("shell_allow_decomposition")) {
        out.push("shell_allow_decomposition: the child allows compound commands; the parent does not".to_string());
    }
    for label in ["file_read", "file_write", "mcp_allowlist"] {
        let inner = strs(i.value(label));
        let outer = strs(o.value(label));
        if inner.is_empty() {
            continue;
        }
        if let Some(bad) = outer.iter().find(|g| glob_unsupported(g)) {
            out.push(format!("{label}: the parent's pattern {} has a shape the proof cannot read", qs(bad)));
            continue;
        }
        for p in &inner {
            match pattern_fits(p, &outer, timeout_ms) {
                Fit::Fits => continue,
                Fit::Unfinished => out.push(format!("{label}: the proof for the child's {} did not finish", qs(p))),
                Fit::DoesNotFit => out.push(format!("{label}: the child's {} is not inside the parent's", qs(p))),
            }
            break;
        }
    }
    if truthy(i.value("network")) {
        let ohosts = strs(o.value("network_hosts"));
        let ihosts = strs(i.value("network_hosts"));
        if !truthy(o.value("network")) {
            out.push("network: the child uses the network; the parent does not".to_string());
        } else if !ohosts.is_empty() {
            if ihosts.is_empty() {
                out.push(format!("network_hosts: the child allows any host; the parent allows only {}", ql(&ohosts)));
            } else {
                let known: HashSet<String> = ohosts.iter().map(|h| h.to_lowercase()).collect();
                let extra: Vec<String> = ihosts.iter().filter(|h| !known.contains(&h.to_lowercase())).cloned().collect();
                if !extra.is_empty() {
                    out.push(format!("network_hosts: the child adds {}", ql(&extra)));
                }
            }
        }
    }
    if truthy(i.value("shell")) {
        if !truthy(o.value("shell")) {
            out.push("shell: the child runs shell commands; the parent does not".to_string());
        } else {
            let heads = strs(o.value("shell_allowlist"));
            let extra: Vec<String> = strs(i.value("shell_allowlist"))
                .into_iter()
                .filter(|h| {
                    !METACHARS.iter().any(|m| h.contains(m)) && !heads.iter().any(|p| h == p || h.starts_with(&format!("{p} ")))
                })
                .collect();
            if !extra.is_empty() {
                out.push(format!("shell_allowlist: the child adds {}", ql(&extra)));
            }
        }
        if parent.value("shell_interpreter_policy").as_str() == Some("strict") {
            let names: BTreeSet<String> = strs(i.value("shell_allowlist"))
                .into_iter()
                .filter(|n| crate::models::SHELL_INTERPRETERS.contains(&n.as_str()))
                .collect();
            if !names.is_empty() {
                let names: Vec<String> = names.into_iter().collect();
                out.push(format!(
                    "shell_interpreter_policy: the parent is strict and the child allows the interpreters {}",
                    ql(&names)
                ));
            }
        }
    }
    let opaque: Vec<String> = list(child.value("invariants"))
        .iter()
        .map(obj)
        .filter(|x| {
            let t = x.value("type").as_str().unwrap_or("");
            truthy(x.value("enforce")) && x.value("expr").is_null() && !crate::z3_checks::RECOGNIZED_OPAQUE_TYPES.contains(&t)
        })
        .map(|x| x.value("type").as_str().unwrap_or("").to_string())
        .collect();
    if !opaque.is_empty() {
        out.push(format!("invariants: the child's {} have no expr, so they cannot be proved", ql(&opaque)));
    }
    out
}

/// A copy of `o` with `k` set: a new key goes last, as `model_copy` dumps
/// a field that was absent in model order, and deadline is the last field.
pub fn with_key(o: &Object, k: &str, v: Value) -> Object {
    let mut out = o.clone();
    out.set(k, v);
    out
}

/// `tree.edge_ok`. None for a parent or a child is a missing envelope.
pub fn edge_ok(parent: Option<&Object>, child: Option<&Object>, timeout_ms: u32) -> EdgeResult {
    let Some(parent) = parent else {
        return EdgeResult { holds: false, reasons: vec!["envelope: the parent has no envelope".into()], child: None, inherited: false };
    };
    let Some(child) = child else {
        return EdgeResult { holds: false, reasons: vec!["envelope: the child declares no envelope".into()], child: None, inherited: false };
    };
    let mut reasons = vec![];
    let ps = parent.value("stakes").as_str().unwrap_or("").to_string();
    let cs = child.value("stakes").as_str().unwrap_or("").to_string();
    if stakes_rank(&cs) < stakes_rank(&ps) {
        reasons.push(format!("stakes: the child's {} is lower than the parent's {}", qs(&cs), qs(&ps)));
    }
    let op = obj(parent.value("permissions"));
    let ip = obj(child.value("permissions"));
    let known: HashSet<String> = strs(op.value("custom_step_allowlist")).into_iter().collect();
    let extra: BTreeSet<String> = strs(ip.value("custom_step_allowlist")).into_iter().filter(|s| !known.contains(s)).collect();
    if !extra.is_empty() {
        let extra: Vec<String> = extra.into_iter().collect();
        reasons.push(format!("custom_step_allowlist: the child adds {}", ql(&extra)));
    }
    for name in ["max_execution_time_s", "max_output_size_mb"] {
        let ov = big(op.value(name));
        let iv = big(ip.value(name));
        if iv > ov {
            reasons.push(format!("{name}: the child's {iv} is more than the parent's {ov}"));
        }
    }
    let mut inherited = false;
    let mut effective = child.clone();
    let pd = parent.value("deadline");
    if !pd.is_null() {
        let cd = child.value("deadline");
        if cd.is_null() {
            effective = with_key(child, "deadline", pd.clone());
            inherited = true;
        } else if num(cd) > num(pd) {
            reasons.push(format!("deadline: the child's {} is after the parent's {}", q(cd), q(pd)));
        }
    }
    let ppol = parent.value("shell_interpreter_policy").as_str().unwrap_or("").to_string();
    let cpol = child.value("shell_interpreter_policy").as_str().unwrap_or("").to_string();
    if policy_rank(&cpol) > policy_rank(&ppol) {
        reasons.push(format!(
            "shell_interpreter_policy: the child's {} is looser than the parent's {}",
            qs(&cpol),
            qs(&ppol)
        ));
    }
    for label in ["invariants", "postconditions"] {
        let have: HashSet<String> = list(child.value(label)).iter().map(canonical_json_ascii).collect();
        let missing: Vec<String> = list(parent.value(label))
            .iter()
            .filter(|x| truthy(obj(x).value("enforce")) && !have.contains(&canonical_json_ascii(x)))
            .map(|x| obj(x).value("type").as_str().unwrap_or("").to_string())
            .collect();
        if !missing.is_empty() {
            reasons.push(format!("{label}: the child drops the parent's {}", ql(&missing)));
        }
    }
    if let Some(r) = robot_reason(parent, child) {
        reasons.push(r);
    }
    reasons.extend(scope_reasons(parent, child, timeout_ms));
    let exprs = [parent, child].iter().any(|e| {
        list(e.value("invariants")).iter().map(obj).any(|x| truthy(x.value("enforce")) && !x.value("expr").is_null())
    });
    // The typed copy reads an int past 64 bits as a float, so the proof's
    // numeral would be wrong: such an edge is not proved (fails closed).
    let wide = [parent, &effective].iter().any(|e| crate::pathways::verify::past_u64(&Value::Obj((*e).clone())));
    if reasons.is_empty() && exprs && wide {
        reasons.push("proof: an int past 64 bits, which this binary does not prove over exactly".into());
    }
    if reasons.is_empty() && exprs {
        let typed = |o: &Object| serde_json::from_str::<crate::models::Envelope>(&dumps(&Value::Obj(o.clone()), false));
        match (typed(parent), typed(&effective)) {
            (Ok(pe), Ok(ce)) => match crate::subsumption::envelope_subsumes(&pe, &ce, true, Some(timeout_ms)) {
                Ok(r) if r.unknown => reasons.push(format!("proof: the proof did not finish in {timeout_ms} ms")),
                Ok(r) if !r.holds => reasons.push("proof: the child admits a step the parent does not".into()),
                Ok(_) => {}
                Err(_) => reasons.push(format!("proof: the proof did not finish in {timeout_ms} ms")),
            },
            _ => reasons.push(format!("proof: the proof did not finish in {timeout_ms} ms")),
        }
    }
    EdgeResult { holds: reasons.is_empty(), reasons, child: Some(effective), inherited }
}

// ---------------------------------------------------------------------------
// The tree ledger
// ---------------------------------------------------------------------------

/// `tree.TreeError`: what the command tried, why not, and the fix.
#[derive(Debug)]
pub struct TreeError {
    pub what: String,
    pub why: String,
    pub fix: String,
    pub code: i32,
}

/// What a ledger command can meet: a refusal, or an I/O error.
#[derive(Debug)]
pub enum Fail {
    Tree(TreeError),
    Io(String),
}

impl From<std::io::Error> for Fail {
    fn from(e: std::io::Error) -> Self {
        Fail::Io(e.to_string())
    }
}

fn te(what: &str, why: &str, fix: &str, code: i32) -> Fail {
    Fail::Tree(TreeError { what: what.into(), why: why.into(), fix: fix.into(), code })
}

/// `tree.ledger_path`.
pub fn ledger_path(data_dir: &str) -> String {
    format!("{data_dir}/tree/ledger.jsonl")
}

fn max_count() -> BigInt {
    BigInt::from(1u64 << 53)
}

/// `tree._count`.
fn count(v: &Value) -> bool {
    match v {
        Value::Null => true,
        Value::Int(t) => t.parse::<BigInt>().is_ok_and(|n| n >= BigInt::default() && n <= max_count()),
        _ => false,
    }
}

fn finite(v: &Value) -> bool {
    crate::rank::finite(v).is_some()
}

fn is_str(o: &Object, k: &str) -> bool {
    o.value(k).as_str().is_some()
}

fn all_strs(v: &Value) -> bool {
    matches!(v, Value::List(l) if l.iter().all(|x| x.as_str().is_some()))
}

/// `tree._fits`.
fn fits(row: &Object) -> bool {
    if !finite(row.value("ts")) {
        return false;
    }
    let deadline_ok = row.value("deadline").is_null() || finite(row.value("deadline"));
    match row.value("event").as_str() {
        Some("root") => {
            is_str(row, "session")
                && is_str(row, "envelope_id")
                && count(row.value("tokens"))
                && count(row.value("turns"))
                && deadline_ok
        }
        Some("spawn") => {
            is_str(row, "parent")
                && is_str(row, "session")
                && is_str(row, "envelope_id")
                && count(row.value("tokens"))
                && count(row.value("turns"))
                && deadline_ok
                && matches!(row.value("proved"), Value::Bool(_))
                && (row.value("profile").is_null() || is_str(row, "profile"))
                && (row.value("model").is_null() || is_str(row, "model"))
                && count(row.value("parent_tokens_used"))
                && count(row.value("parent_turns_used"))
        }
        Some("refused") => is_str(row, "parent") && is_str(row, "session") && all_strs(row.value("reasons")),
        Some("ask") => {
            is_str(row, "ask_id")
                && is_str(row, "parent")
                && is_str(row, "session")
                && count(row.value("tokens"))
                && count(row.value("turns"))
                && row.value("envelope").as_obj().is_some()
                && all_strs(row.value("reasons"))
        }
        Some("answer") => {
            is_str(row, "ask_id")
                && matches!(row.value("answer").as_str(), Some("allow") | Some("deny"))
        }
        Some("end") => {
            is_str(row, "session")
                && count(row.value("tokens_used"))
                && count(row.value("turns_used"))
        }
        Some("group") => {
            is_str(row, "session")
                && matches!(row.value("pgid"), Value::Int(t)
                    if t.parse::<BigInt>().is_ok_and(|n| n > BigInt::from(1) && n <= BigInt::from(MAX_PGID)))
        }
        _ => false,
    }
}

/// `tree.MAX_PGID`: the largest process group id a group row may hold
/// (pid_t is 32 bits).
pub const MAX_PGID: i64 = (1 << 31) - 1;

/// `tree.Node`.
#[derive(Clone, Debug)]
pub struct Node {
    pub session: String,
    pub parent: Option<String>,
    pub envelope_id: String,
    pub tokens: Option<BigInt>,
    pub turns: Option<BigInt>,
    pub deadline: Option<f64>,
    pub proved: bool,
    pub state: String,
    pub tokens_used: Option<BigInt>,
    pub turns_used: Option<BigInt>,
    pub children: Vec<String>,
    pub refused: usize,
    /// The agent profile and model the starter ran it on, as its spawn row
    /// holds them.
    pub profile: Option<String>,
    pub model: Option<String>,
    /// Its own use as the latest spawn under it reported it.
    pub own_tokens_used: Option<BigInt>,
    pub own_turns_used: Option<BigInt>,
    /// The process group its starter put it in, as its group row holds
    /// it; 0 for none.
    pub pgid: i32,
}

impl Node {
    fn own_used(&self, axis: &str) -> Option<&BigInt> {
        if axis == "tokens" {
            self.own_tokens_used.as_ref()
        } else {
            self.own_turns_used.as_ref()
        }
    }
    fn set_own_used(&mut self, axis: &str, v: Option<BigInt>) {
        if axis == "tokens" {
            self.own_tokens_used = v;
        } else {
            self.own_turns_used = v;
        }
    }
    fn budget(&self, axis: &str) -> Option<&BigInt> {
        if axis == "tokens" {
            self.tokens.as_ref()
        } else {
            self.turns.as_ref()
        }
    }
    fn used(&self, axis: &str) -> Option<&BigInt> {
        if axis == "tokens" {
            self.tokens_used.as_ref()
        } else {
            self.turns_used.as_ref()
        }
    }
}

/// `tree.Tree`.
#[derive(Default)]
pub struct Tree {
    pub nodes: HashMap<String, Node>,
    pub roots: Vec<String>,
    pub asks: HashMap<String, Object>,
    pub ask_order: Vec<String>,
    pub answers: HashMap<String, String>,
    pub skipped: usize,
    /// Each parent's children, refusals and asks.
    pub proposals: HashMap<String, usize>,
}

impl Tree {
    /// `Tree.running`.
    pub fn running(&self, s: &str) -> Option<&Node> {
        self.nodes.get(s).filter(|n| n.state == "running")
    }

    /// `Tree.charge`: what a node costs its parent, its branch total once
    /// it ended with a report, else its whole reservation.
    pub fn charge(&self, n: &Node, axis: &str) -> BigInt {
        if n.state == "ended" {
            if let Some(u) = n.used(axis) {
                return u.clone();
            }
        }
        // No report of its own: the larger of the reservation and the
        // children's charges, so an overrun below it still counts.
        let mut below = BigInt::default();
        for c in &n.children {
            below += self.charge(&self.nodes[c], axis);
        }
        if let Some(r) = n.budget(axis) {
            if *r > below {
                below = r.clone();
            }
        }
        below.min(max_count())
    }

    /// `Tree.branch_groups`: the recorded process groups of the running
    /// nodes below session, deepest first.
    pub fn branch_groups(&self, session: &str) -> Vec<i32> {
        fn walk(t: &Tree, sid: &str, out: &mut Vec<i32>) {
            for c in &t.nodes[sid].children {
                walk(t, c, out);
                let n = &t.nodes[c];
                if n.state == "running" && n.pgid != 0 {
                    out.push(n.pgid);
                }
            }
        }
        let mut out = vec![];
        if self.nodes.contains_key(session) {
            walk(self, session, &mut out);
        }
        out
    }

    /// `Tree.left`: what the node may still reserve for children, below
    /// zero when a branch overran.
    pub fn left(&self, n: &Node, axis: &str) -> Option<BigInt> {
        let b = n.budget(axis)?;
        let mut spent = BigInt::default();
        for c in &n.children {
            spent += self.charge(&self.nodes[c], axis);
        }
        if let Some(o) = n.own_used(axis) {
            spent += o.clone();
        }
        Some(b - spent)
    }

    /// `Tree.branch_total`: own plus each child's charge, capped at 2**53;
    /// None when the node has no report of its own.
    pub fn branch_total(&self, n: &Node, axis: &str, own: &Option<BigInt>) -> Option<BigInt> {
        let mut total = own.clone()?;
        for c in &n.children {
            total += self.charge(&self.nodes[c], axis);
        }
        if total > max_count() {
            total = max_count();
        }
        Some(total)
    }

    /// `Tree.open_asks`, in the order the asks were made.
    pub fn open_asks(&self) -> Vec<&Object> {
        self.ask_order.iter().filter(|k| !self.answers.contains_key(*k)).map(|k| &self.asks[k]).collect()
    }
}

fn opt_big(v: &Value) -> Option<BigInt> {
    if v.is_null() {
        None
    } else {
        Some(big(v))
    }
}

/// `tree.read_tree`.
pub fn read_tree(data_dir: &str) -> Tree {
    let mut t = Tree::default();
    let (rows, bad) = crate::rank::read_rows(&ledger_path(data_dir));
    t.skipped = bad;
    for row in rows {
        if !fits(&row) {
            t.skipped += 1;
            continue;
        }
        match row.value("event").as_str().unwrap_or("") {
            ev @ ("root" | "spawn") => {
                let sid = row.value("session").as_str().unwrap_or("").to_string();
                let parent = if ev == "spawn" { row.value("parent").as_str().map(String::from) } else { None };
                if t.nodes.contains_key(&sid) || parent.as_ref().is_some_and(|p| t.running(p).is_none()) {
                    t.skipped += 1;
                    continue;
                }
                let deadline = if row.value("deadline").is_null() { None } else { Some(num(row.value("deadline"))) };
                t.nodes.insert(
                    sid.clone(),
                    Node {
                        session: sid.clone(),
                        parent: parent.clone(),
                        envelope_id: row.value("envelope_id").as_str().unwrap_or("").to_string(),
                        tokens: opt_big(row.value("tokens")),
                        turns: opt_big(row.value("turns")),
                        deadline,
                        proved: !matches!(row.value("proved"), Value::Bool(false)),
                        state: "running".into(),
                        tokens_used: None,
                        turns_used: None,
                        children: vec![],
                        refused: 0,
                        profile: if ev == "spawn" { row.value("profile").as_str().map(String::from) } else { None },
                        model: if ev == "spawn" { row.value("model").as_str().map(String::from) } else { None },
                        own_tokens_used: None,
                        own_turns_used: None,
                        pgid: 0,
                    },
                );
                match parent {
                    None => t.roots.push(sid),
                    Some(p) => {
                        *t.proposals.entry(p.clone()).or_default() += 1;
                        let pn = t.nodes.get_mut(&p).unwrap();
                        pn.children.push(sid);
                        pn.refused = 0;
                        for axis in ["tokens", "turns"] {
                            let u = row.value(&format!("parent_{axis}_used"));
                            if !u.is_null() {
                                pn.set_own_used(axis, opt_big(u));
                            }
                        }
                    }
                }
            }
            "refused" => {
                let pname = row.value("parent").as_str().unwrap_or("").to_string();
                match t.nodes.get_mut(&pname) {
                    Some(p) => {
                        p.refused += 1;
                        *t.proposals.entry(pname).or_default() += 1;
                    }
                    None => t.skipped += 1,
                }
            }
            "ask" => {
                let id = row.value("ask_id").as_str().unwrap_or("").to_string();
                if t.asks.contains_key(&id) || !t.nodes.contains_key(row.value("parent").as_str().unwrap_or("")) {
                    t.skipped += 1;
                    continue;
                }
                *t.proposals.entry(row.value("parent").as_str().unwrap_or("").to_string()).or_default() += 1;
                t.asks.insert(id.clone(), row.clone());
                t.ask_order.push(id);
            }
            "answer" => {
                let id = row.value("ask_id").as_str().unwrap_or("").to_string();
                let Some(a) = t.asks.get(&id) else {
                    t.skipped += 1;
                    continue;
                };
                if t.answers.contains_key(&id) {
                    t.skipped += 1;
                    continue;
                }
                let parent = a.value("parent").as_str().unwrap_or("").to_string();
                t.answers.insert(id, row.value("answer").as_str().unwrap_or("").to_string());
                if let Some(p) = t.nodes.get_mut(&parent) {
                    p.refused = 0;
                }
            }
            "end" => {
                let sid = row.value("session").as_str().unwrap_or("").to_string();
                if t.running(&sid).is_none() {
                    t.skipped += 1;
                    continue;
                }
                let n = t.nodes.get_mut(&sid).unwrap();
                n.state = "ended".into();
                n.tokens_used = opt_big(row.value("tokens_used"));
                n.turns_used = opt_big(row.value("turns_used"));
            }
            "group" => {
                let sid = row.value("session").as_str().unwrap_or("").to_string();
                if t.running(&sid).is_none() {
                    t.skipped += 1;
                    continue;
                }
                let g = match row.value("pgid") {
                    Value::Int(x) => x.parse().unwrap_or(0),
                    _ => 0,
                };
                t.nodes.get_mut(&sid).unwrap().pgid = g;
            }
            _ => {}
        }
    }
    t
}

/// `tree.record_group`: the process group a starter put session in, so
/// a kill of an ancestor's branch reaches it.
pub fn record_group(data_dir: &str, session: &str, pgid: i32) -> Result<(), Fail> {
    locked(data_dir, || {
        let row = Object::new()
            .with("event", "group")
            .with("session", session)
            .with("pgid", Value::Int(pgid.to_string()))
            .with("ts", now());
        append(data_dir, &[row])
    })
}

/// `tree._locked`: the fold, the check and the append under one lock on
/// `tree/ledger.lock`.
fn locked<T>(data_dir: &str, f: impl FnOnce() -> Result<T, Fail>) -> Result<T, Fail> {
    use std::os::fd::AsRawFd;
    let d = format!("{data_dir}/tree");
    std::fs::create_dir_all(&d)?;
    let file = std::fs::OpenOptions::new().create(true).append(true).open(format!("{d}/ledger.lock"))?;
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX) } != 0 {
        return Err(std::io::Error::last_os_error().into());
    }
    let out = f();
    unsafe {
        libc::flock(file.as_raw_fd(), libc::LOCK_UN);
    }
    out
}

fn append(data_dir: &str, rows: &[Object]) -> Result<(), Fail> {
    let mut f = std::fs::OpenOptions::new().create(true).append(true).open(ledger_path(data_dir))?;
    let mut text = String::new();
    for r in rows {
        text.push_str(&dumps(&Value::Obj(r.clone()), true));
        text.push('\n');
    }
    f.write_all(text.as_bytes())?;
    f.sync_all()?;
    Ok(())
}

/// `tree.now`.
pub fn now() -> f64 {
    crate::rank::now()
}

fn check_session(s: &str, what: &str) -> Result<(), Fail> {
    if valid_session(s) {
        return Ok(());
    }
    Err(te(
        what,
        &format!("The session id {} is not 1 to 64 of A-Z a-z 0-9 . _ - (no dot at either end), or it is default.", qs(s)),
        "Pick another session id.",
        2,
    ))
}

fn check_counts(counts: &[(&str, &Option<BigInt>)]) -> Result<(), Fail> {
    for (name, v) in counts {
        if let Some(n) = v {
            if *n < BigInt::default() || *n > max_count() {
                return Err(te(
                    "Tried to read the budget.",
                    &format!("--{name} must be a whole number from 0 to 2**53."),
                    "Fix the option and run again.",
                    2,
                ));
            }
        }
    }
    Ok(())
}

fn envelope_file(gate_root: &str, session: &str) -> String {
    join(&envelopes_dir(gate_root), &format!("{session}.json"))
}

fn exists(p: &str) -> bool {
    std::fs::metadata(p).is_ok()
}

/// `tree.load_registered`: the envelope registered for a session, or None
/// when there is none or it does not read as one.
pub fn load_registered(session: &str, gate_root: &str) -> Option<Object> {
    let name = if session.is_empty() { "default".to_string() } else { safe_session_id(session) };
    let text = std::fs::read_to_string(join(&envelopes_dir(gate_root), &format!("{name}.json"))).ok()?;
    match validate_json(Id::Envelope, &text) {
        Ok(Value::Obj(o)) => Some(o),
        _ => None,
    }
}

fn big_value(b: &Option<BigInt>) -> Value {
    match b {
        Some(n) => Value::Int(n.to_string()),
        None => Value::Null,
    }
}

fn register(env: &Object, session: &str, gate_root: &str) -> Result<String, Fail> {
    let (path, _, res) = crate::cli::envelope::register(env, session, gate_root);
    res?;
    Ok(path)
}

/// `tree.make_root`.
pub fn make_root(data_dir: &str, gate_root: &str, env: &Object, session: &str, tokens: Option<BigInt>, turns: Option<BigInt>) -> Result<String, Fail> {
    let what = format!("Tried to make session {session} a root of the tree.");
    check_session(session, &what)?;
    check_counts(&[("tokens", &tokens), ("turns", &turns)])?;
    locked(data_dir, || {
        let t = read_tree(data_dir);
        if t.nodes.contains_key(session) || exists(&envelope_file(gate_root, session)) {
            return Err(te(&what, &format!("Session {session} is in use already."), "Pick another session id.", 1));
        }
        let row = Object::new()
            .with("event", "root")
            .with("session", session)
            .with("envelope_id", env.value("id").clone())
            .with("tokens", big_value(&tokens))
            .with("turns", big_value(&turns))
            .with("deadline", env.value("deadline").clone())
            .with("ts", now());
        append(data_dir, &[row])?;
        register(env, session, gate_root)
    })
}

/// `tree.Spawn`.
pub struct Spawn {
    pub status: &'static str,
    pub reasons: Vec<String>,
    pub path: String,
    pub ask_id: String,
    pub inherited: Option<Value>,
    /// The child's session, minted when none was given.
    pub session: String,
}

/// `tree.spawn`'s keywords past the budget: the child's profile and model,
/// and the parent's own use so far.
#[derive(Default, Clone)]
pub struct SpawnOpts {
    pub profile: Option<String>,
    pub model: Option<String>,
    pub parent_tokens_used: Option<BigInt>,
    pub parent_turns_used: Option<BigInt>,
}

/// `tree.mint_session`: n- and 12 hex of SHA-256 over the JSON text
/// `[parent, n]`.
pub fn mint_session(parent: &str, n: usize) -> String {
    let doc = Value::List(vec![parent.into(), Value::Int(n.to_string())]);
    let hexed = crate::gate::sha256::hexdigest(canonical_json_ascii(&doc).as_bytes());
    format!("n-{}", &hexed[..12])
}

fn ask_id(parent: &str, session: &str, env: &Object, n: usize) -> String {
    let doc = Value::List(vec![parent.into(), session.into(), Value::Obj(env.clone()), Value::Int(n.to_string())]);
    let hexed = crate::gate::sha256::hexdigest(canonical_json_ascii(&doc).as_bytes());
    format!("ask_{}", &hexed[..12])
}

fn budget_reasons(t: &Tree, parent: &Node, tokens: &Option<BigInt>, turns: &Option<BigInt>) -> Vec<String> {
    let mut out = vec![];
    for (axis, want) in [("tokens", tokens), ("turns", turns)] {
        let Some(left) = t.left(parent, axis) else { continue };
        match want {
            None => out.push(format!("{axis}: the parent has a budget, so the child must ask for an amount")),
            Some(w) if *w > left => out.push(format!("{axis}: the child asks for {w}; the parent has {left} left")),
            _ => {}
        }
    }
    out
}

fn strs_value(l: &[String]) -> Value {
    Value::List(l.iter().map(|s| Value::Str(s.clone())).collect())
}

/// `tree.spawn` with no profile, model or parent use.
#[allow(clippy::too_many_arguments)]
pub fn spawn(
    data_dir: &str,
    gate_root: &str,
    env: &Object,
    parent: &str,
    session: &str,
    tokens: Option<BigInt>,
    turns: Option<BigInt>,
    timeout_ms: u32,
) -> Result<Spawn, Fail> {
    spawn_opts(data_dir, gate_root, env, parent, Some(session), tokens, turns, timeout_ms, SpawnOpts::default())
}

/// `tree.spawn`. A session of None is minted (mint_session).
#[allow(clippy::too_many_arguments)]
pub fn spawn_opts(
    data_dir: &str,
    gate_root: &str,
    env: &Object,
    parent: &str,
    session: Option<&str>,
    tokens: Option<BigInt>,
    turns: Option<BigInt>,
    timeout_ms: u32,
    o: SpawnOpts,
) -> Result<Spawn, Fail> {
    let mut session_s = session.unwrap_or("None").to_string();
    let mut what = format!("Tried to start session {session_s} under {parent}.");
    if session.is_some() {
        check_session(&session_s, &what)?;
    }
    check_session(parent, &what)?;
    check_counts(&[
        ("tokens", &tokens),
        ("turns", &turns),
        ("parent-tokens-used", &o.parent_tokens_used),
        ("parent-turns-used", &o.parent_turns_used),
    ])?;
    if o.model.as_deref().is_some_and(|m| {
        m.chars()
            .any(|c| (c as u32) < 32 || (127..=159).contains(&(c as u32)))
    }) {
        return Err(te(
            "Tried to read the model id.",
            "The model id holds a control character.",
            "Name the model with no control character.",
            2,
        ));
    }
    locked(data_dir, || {
        let mut t = read_tree(data_dir);
        if session.is_none() {
            let mut n = t.proposals.get(parent).copied().unwrap_or(0);
            session_s = mint_session(parent, n);
            while t.nodes.contains_key(&session_s) || exists(&envelope_file(gate_root, &session_s)) {
                n += 1;
                session_s = mint_session(parent, n);
            }
            what = format!("Tried to start session {session_s} under {parent}.");
        }
        let session = session_s.as_str();
        let what = what.as_str();
        if let Some(pn) = t.nodes.get_mut(parent).filter(|n| n.state == "running") {
            if o.parent_tokens_used.is_some() {
                pn.own_tokens_used = o.parent_tokens_used.clone();
            }
            if o.parent_turns_used.is_some() {
                pn.own_turns_used = o.parent_turns_used.clone();
            }
        }
        let t = t;
        let Some(pnode) = t.running(parent) else {
            return Err(te(
                &what,
                &format!("{parent} is not a running node of the tree."),
                "Start the child under a running node; daisugi tree status lists them.",
                1,
            ));
        };
        if t.nodes.contains_key(session) || exists(&envelope_file(gate_root, session)) {
            return Err(te(&what, &format!("Session {session} is in use already."), "Pick another session id.", 1));
        }
        if t.open_asks().iter().any(|a| a.value("session").as_str() == Some(session)) {
            return Err(te(
                &what,
                &format!("An ask for session {session} is open."),
                "Wait for the operator's answer, or pick another session id.",
                1,
            ));
        }
        let penv = load_registered(parent, gate_root);
        let mut res = None;
        let mut reasons = if penv.as_ref().is_some_and(|p| p.value("stakes").as_str() == Some("physical")) {
            vec!["stakes: the parent's stakes are physical, so it cannot start an agent".to_string()]
        } else {
            let r = edge_ok(penv.as_ref(), Some(env), timeout_ms);
            let reasons = r.reasons.clone();
            res = Some(r);
            reasons
        };
        reasons.extend(budget_reasons(&t, pnode, &tokens, &turns));
        let ts = now();
        if !reasons.is_empty() {
            if pnode.refused >= ASK_AFTER {
                let id = ask_id(parent, session, env, t.asks.len());
                let row = Object::new()
                    .with("event", "ask")
                    .with("ask_id", id.as_str())
                    .with("parent", parent)
                    .with("session", session)
                    .with("tokens", big_value(&tokens))
                    .with("turns", big_value(&turns))
                    .with("envelope", Value::Obj(env.clone()))
                    .with("reasons", strs_value(&reasons))
                    .with("ts", ts);
                append(data_dir, &[row])?;
                return Ok(Spawn {
                    status: "asked",
                    reasons,
                    path: String::new(),
                    ask_id: id,
                    inherited: None,
                    session: session.to_string(),
                });
            }
            let row = Object::new()
                .with("event", "refused")
                .with("parent", parent)
                .with("session", session)
                .with("reasons", strs_value(&reasons))
                .with("ts", ts);
            append(data_dir, &[row])?;
            return Ok(Spawn {
                status: "refused",
                reasons,
                path: String::new(),
                ask_id: String::new(),
                inherited: None,
                session: session.to_string(),
            });
        }
        let res = res.expect("the edge ran");
        let penv = penv.expect("the edge holds, so the parent has an envelope");
        let child = with_key(res.child.as_ref().expect("the edge holds"), "parent_envelope", penv.value("id").clone());
        let mut row = Object::new()
            .with("event", "spawn")
            .with("parent", parent)
            .with("session", session)
            .with("envelope_id", child.value("id").clone())
            .with("tokens", big_value(&tokens))
            .with("turns", big_value(&turns))
            .with("deadline", child.value("deadline").clone())
            .with("proved", true);
        if let Some(p) = &o.profile {
            row = row.with("profile", p.as_str());
        }
        if let Some(m) = &o.model {
            row = row.with("model", m.as_str());
        }
        if o.parent_tokens_used.is_some() {
            row = row.with("parent_tokens_used", big_value(&o.parent_tokens_used));
        }
        if o.parent_turns_used.is_some() {
            row = row.with("parent_turns_used", big_value(&o.parent_turns_used));
        }
        let row = row.with("ts", ts);
        append(data_dir, &[row])?;
        let path = register(&child, session, gate_root)?;
        let inherited = if res.inherited { Some(child.value("deadline").clone()) } else { None };
        Ok(Spawn { status: "started", reasons: vec![], path, ask_id: String::new(), inherited, session: session.to_string() })
    })
}

/// `tree.end`: the node that ended.
pub fn end(data_dir: &str, gate_root: &str, session: &str, tokens_used: Option<BigInt>, turns_used: Option<BigInt>) -> Result<Node, Fail> {
    let what = format!("Tried to end session {session}.");
    check_session(session, &what)?;
    check_counts(&[("tokens-used", &tokens_used), ("turns-used", &turns_used)])?;
    locked(data_dir, || {
        let t = read_tree(data_dir);
        let Some(node) = t.running(session) else {
            return Err(te(
                &what,
                &format!("{session} is not a running node of the tree."),
                "daisugi tree status lists the running nodes.",
                1,
            ));
        };
        let busy: Vec<String> = node.children.iter().filter(|c| t.nodes[*c].state == "running").cloned().collect();
        if !busy.is_empty() {
            return Err(te(&what, &format!("It has running children: {}.", busy.join(", ")), "End them first.", 1));
        }
        let tokens_total = t.branch_total(node, "tokens", &tokens_used);
        let turns_total = t.branch_total(node, "turns", &turns_used);
        let row = Object::new()
            .with("event", "end")
            .with("session", session)
            .with("tokens_used", big_value(&tokens_total))
            .with("turns_used", big_value(&turns_total))
            .with("ts", now());
        append(data_dir, &[row])?;
        match std::fs::remove_file(envelope_file(gate_root, session)) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => return Err(e.into()),
            _ => {}
        }
        let mut n = node.clone();
        n.state = "ended".into();
        n.tokens_used = tokens_total;
        n.turns_used = turns_total;
        Ok(n)
    })
}

/// `tree.end_subtree`: end every running node under session (and
/// session, with include_self), leaves first, each with no report, so each
/// keeps its whole reservation. Returns the sessions it ended.
pub fn end_subtree(data_dir: &str, gate_root: &str, session: &str, include_self: bool) -> Vec<String> {
    let t = read_tree(data_dir);
    let mut order = vec![];
    fn walk(t: &Tree, sid: &str, top: &str, include_self: bool, order: &mut Vec<String>) {
        for c in &t.nodes[sid].children {
            walk(t, c, top, include_self, order);
        }
        if t.nodes[sid].state == "running" && (include_self || sid != top) {
            order.push(sid.to_string());
        }
    }
    if t.nodes.contains_key(session) {
        walk(&t, session, session, include_self, &mut order);
    }
    for sid in &order {
        let _ = end(data_dir, gate_root, sid, None, None);
    }
    order
}

/// `tree.answer`: the ask, and the registered path on an allow.
pub fn answer(data_dir: &str, gate_root: &str, id: &str, verdict: &str) -> Result<(Object, Option<String>), Fail> {
    let what = format!("Tried to answer ask {id}.");
    if verdict != "allow" && verdict != "deny" {
        return Err(te(&what, "The answer must be allow or deny.", "Run it again with allow or deny.", 2));
    }
    locked(data_dir, || {
        let t = read_tree(data_dir);
        let Some(ask) = t.asks.get(id).cloned() else {
            return Err(te(&what, &format!("There is no ask {id}."), "daisugi tree status lists the open asks.", 1));
        };
        if let Some(a) = t.answers.get(id) {
            return Err(te(&what, &format!("It is answered already: {a}."), "An answer stands.", 1));
        }
        let ts = now();
        let row = Object::new().with("event", "answer").with("ask_id", id).with("answer", verdict).with("ts", ts);
        if verdict == "deny" {
            append(data_dir, &[row])?;
            return Ok((ask, None));
        }
        let parent = ask.value("parent").as_str().unwrap_or("").to_string();
        let session = ask.value("session").as_str().unwrap_or("").to_string();
        let Some(pnode) = t.running(&parent) else {
            return Err(te(&what, &format!("{parent} is not a running node of the tree."), "Deny the ask.", 1));
        };
        if t.nodes.contains_key(&session) || exists(&envelope_file(gate_root, &session)) {
            return Err(te(&what, &format!("Session {session} is in use already."), "Deny the ask.", 1));
        }
        let over = budget_reasons(&t, pnode, &opt_big(ask.value("tokens")), &opt_big(ask.value("turns")));
        if !over.is_empty() {
            return Err(te(&what, &format!("The budget does not fit: {}.", over.join("; ")), "Deny the ask.", 1));
        }
        let proposed = match validate_json(Id::Envelope, &dumps(ask.value("envelope"), false)) {
            Ok(Value::Obj(o)) => o,
            _ => return Err(Fail::Io("the ask's envelope is not a valid envelope".into())),
        };
        // AT-5 holds under an allow too: a physical parent starts no agent.
        if load_registered(&parent, gate_root).is_some_and(|p| p.value("stakes").as_str() == Some("physical")) {
            return Err(te(&what, "stakes: the parent's stakes are physical, so it cannot start an agent.", "Deny the ask.", 1));
        }
        // An allow never widens the tree past its root: the child must fit
        // inside the root of its branch.
        let mut root = parent.clone();
        while let Some(p) = t.nodes[&root].parent.clone() {
            root = p;
        }
        let edge = edge_ok(load_registered(&root, gate_root).as_ref(), Some(&proposed), DEFAULT_TIMEOUT_MS);
        let mut child = match (edge.holds, edge.child) {
            (true, Some(c)) => c,
            _ => {
                return Err(te(
                    &what,
                    &format!("The child does not fit inside the root {root}: {}.", edge.reasons.join("; ")),
                    "Deny the ask, or register the child as a root of its own.",
                    1,
                ))
            }
        };
        if let Some(penv) = load_registered(&parent, gate_root) {
            child = with_key(&child, "parent_envelope", penv.value("id").clone());
        }
        let spawn = Object::new()
            .with("event", "spawn")
            .with("parent", parent.as_str())
            .with("session", session.as_str())
            .with("envelope_id", child.value("id").clone())
            .with("tokens", ask.value("tokens").clone())
            .with("turns", ask.value("turns").clone())
            .with("deadline", child.value("deadline").clone())
            .with("proved", false)
            .with("ts", ts);
        append(data_dir, &[row, spawn])?;
        let path = register(&child, &session, gate_root)?;
        Ok((ask, Some(path)))
    })
}

fn axis_doc(n: &Node, t: &Tree, axis: &str) -> Object {
    Object::new()
        .with("budget", big_value(&n.budget(axis).cloned()))
        .with("used", big_value(&n.used(axis).cloned()))
        .with("left", big_value(&t.left(n, axis)))
}

/// `tree.status_doc`.
pub fn status_doc(data_dir: &str) -> Object {
    let t = read_tree(data_dir);
    let mut nodes = vec![];
    fn walk(t: &Tree, sid: &str, nodes: &mut Vec<Value>) {
        let n = &t.nodes[sid];
        let mut doc = Object::new()
                .with("session", n.session.as_str())
                .with("parent", n.parent.clone().map(Value::Str).unwrap_or(Value::Null))
                .with("state", n.state.as_str())
                .with("proved", n.proved)
                .with("envelope_id", n.envelope_id.as_str())
                .with("deadline", n.deadline.map(Value::Float).unwrap_or(Value::Null))
                .with("tokens", axis_doc(n, t, "tokens"))
                .with("turns", axis_doc(n, t, "turns"))
                .with("refused", Value::Int(n.refused.to_string()))
                .with("children", strs_value(&n.children));
        if let Some(p) = &n.profile {
            doc.set("profile", p.as_str());
        }
        if let Some(m) = &n.model {
            doc.set("model", m.as_str());
        }
        nodes.push(Value::Obj(doc));
        for c in &n.children {
            walk(t, c, nodes);
        }
    }
    for r in &t.roots {
        walk(&t, r, &mut nodes);
    }
    let asks: Vec<Value> = t
        .open_asks()
        .into_iter()
        .map(|a| {
            Value::Obj(
                Object::new()
                    .with("ask_id", a.value("ask_id").clone())
                    .with("parent", a.value("parent").clone())
                    .with("session", a.value("session").clone())
                    .with("tokens", a.value("tokens").clone())
                    .with("turns", a.value("turns").clone())
                    .with("reasons", a.value("reasons").clone()),
            )
        })
        .collect();
    Object::new().with("nodes", Value::List(nodes)).with("asks", Value::List(asks)).with("skipped", Value::Int(t.skipped.to_string()))
}

fn int_text(v: &Value) -> String {
    big(v).to_string()
}

fn amount(a: &Object, state: &str) -> String {
    if a.value("budget").is_null() {
        return "-".into();
    }
    let b = int_text(a.value("budget"));
    if state == "ended" {
        let used = if a.value("used").is_null() { "?".to_string() } else { int_text(a.value("used")) };
        let mut over = String::new();
        if !a.value("used").is_null() {
            let d = big(a.value("used")) - big(a.value("budget"));
            if d > BigInt::default() {
                over = format!(", over by {d}");
            }
        }
        return format!("{b} (used {used}{over})");
    }
    let left = big(a.value("left"));
    if left < BigInt::default() {
        return format!("{b} (over by {})", -left);
    }
    format!("{b} (left {})", int_text(a.value("left")))
}

/// `tree.status_text`.
pub fn status_text(doc: &Object) -> String {
    let mut lines = vec![];
    let mut depth: HashMap<String, usize> = HashMap::new();
    for x in list(doc.value("nodes")) {
        let n = obj(x);
        let d = match n.value("parent").as_str() {
            Some(p) => depth.get(p).copied().unwrap_or(0) + 1,
            None => 0,
        };
        let s = n.value("session").as_str().unwrap_or("").to_string();
        depth.insert(s.clone(), d);
        let state = n.value("state").as_str().unwrap_or("").to_string();
        let mut label = state.clone();
        if !truthy(n.value("proved")) {
            label.push_str(", operator allow, not proved");
        }
        let mut line = format!(
            "{}{s} ({label}) tokens {}, turns {}",
            "  ".repeat(d),
            amount(&obj(n.value("tokens")), &state),
            amount(&obj(n.value("turns")), &state)
        );
        if !n.value("deadline").is_null() {
            line.push_str(&format!(", deadline {}", q(n.value("deadline"))));
        }
        if let Some(p) = n.value("profile").as_str() {
            line.push_str(&format!(", profile {p}"));
        }
        if let Some(m) = n.value("model").as_str() {
            line.push_str(&format!(", model {m}"));
        }
        let refused = int_text(n.value("refused"));
        if refused != "0" {
            line.push_str(&format!(", {refused} refused"));
        }
        lines.push(line);
    }
    if lines.is_empty() {
        lines.push("The tree is empty.".to_string());
    }
    let asks = list(doc.value("asks"));
    if !asks.is_empty() {
        lines.push("Open asks:".to_string());
        for x in asks {
            let a = obj(x);
            let first = strs(a.value("reasons")).into_iter().next().unwrap_or_default();
            lines.push(format!(
                "  {}: {} asks to start {}: {first}",
                a.value("ask_id").as_str().unwrap_or(""),
                a.value("parent").as_str().unwrap_or(""),
                a.value("session").as_str().unwrap_or("")
            ));
        }
        lines.push("Answer one with: daisugi tree answer ASK allow|deny".to_string());
    }
    let skipped = int_text(doc.value("skipped"));
    if skipped != "0" {
        lines.push(format!("{skipped} ledger rows did not read or fit; skipped."));
    }
    lines.join("\n") + "\n"
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(text: &str) -> Object {
        match validate_json(Id::Envelope, text) {
            Ok(Value::Obj(o)) => o,
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn every_failing_part_is_reported_in_order() {
        let parent = env(r#"{"id": "p", "generated_by": "t", "task": "t", "stakes": "high", "deadline": 100,
            "permissions": {"shell": true, "shell_allowlist": ["git"]}}"#);
        let child = env(r#"{"id": "c", "generated_by": "t", "task": "t", "stakes": "low", "deadline": 200,
            "permissions": {"shell": true, "shell_allowlist": ["git status", "rm"], "max_execution_time_s": 60}}"#);
        let r = edge_ok(Some(&parent), Some(&child), DEFAULT_TIMEOUT_MS);
        assert!(!r.holds);
        assert_eq!(
            r.reasons,
            vec![
                r#"stakes: the child's "low" is lower than the parent's "high""#,
                "max_execution_time_s: the child's 60 is more than the parent's 30",
                "deadline: the child's 200.0 is after the parent's 100.0",
                r#"shell_allowlist: the child adds ["rm"]"#,
            ]
        );
    }

    fn scratch() -> String {
        use std::sync::atomic::{AtomicU64, Ordering};
        static N: AtomicU64 = AtomicU64::new(0);
        let d = std::env::temp_dir().join(format!("tree-test-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
        let _ = std::fs::remove_dir_all(&d);
        d.to_string_lossy().to_string()
    }

    fn grow(tokens: Option<i64>) -> (String, String) {
        let d = scratch();
        let g = format!("{d}/gate");
        let root = env(r#"{"id": "env_root", "generated_by": "t", "task": "t", "permissions": {"shell": true, "shell_allowlist": ["git"]}}"#);
        make_root(&d, &g, &root, "top", tokens.map(BigInt::from), None).unwrap();
        (d, g)
    }

    fn kid(d: &str, g: &str, parent: &str, session: Option<&str>, tokens: i64, o: SpawnOpts) -> Spawn {
        let k = env(r#"{"id": "env_kid", "generated_by": "t", "task": "t", "permissions": {"shell": true, "shell_allowlist": ["git"]}}"#);
        spawn_opts(d, g, &k, parent, session, Some(BigInt::from(tokens)), None, DEFAULT_TIMEOUT_MS, o).unwrap()
    }

    #[test]
    fn branch_totals_overruns_and_minted_sessions_follow_the_oracle() {
        let (d, g) = grow(Some(1000));
        kid(&d, &g, "top", Some("a"), 600, SpawnOpts::default());
        kid(&d, &g, "a", Some("b"), 300, SpawnOpts::default());
        kid(&d, &g, "b", Some("c"), 100, SpawnOpts::default());
        end(&d, &g, "c", Some(BigInt::from(40)), Some(BigInt::from(1))).unwrap();
        end(&d, &g, "b", Some(BigInt::from(10)), Some(BigInt::from(1))).unwrap();
        let a = end(&d, &g, "a", Some(BigInt::from(5)), Some(BigInt::from(1))).unwrap();
        assert_eq!(a.tokens_used, Some(BigInt::from(55)));
        assert_eq!(a.turns_used, Some(BigInt::from(3)));
        let t = read_tree(&d);
        assert_eq!(t.left(&t.nodes["top"], "tokens"), Some(BigInt::from(945)));
        let _ = std::fs::remove_dir_all(&d);

        let (d, g) = grow(Some(100));
        kid(&d, &g, "top", Some("a"), 60, SpawnOpts::default());
        end(&d, &g, "a", Some(BigInt::from(150)), None).unwrap();
        let text = status_text(&status_doc(&d));
        assert!(text.contains("top (running) tokens 100 (over by 50)"), "{text}");
        assert!(text.contains("a (ended) tokens 60 (used 150, over by 90)"), "{text}");
        let _ = std::fs::remove_dir_all(&d);

        let (d, g) = grow(None);
        let opts = SpawnOpts { profile: Some("desk".into()), model: Some("llama3.3:70b".into()), ..Default::default() };
        let s = kid(&d, &g, "top", None, 1, opts);
        // The oracle mints n-798c5d0e353b for the first child of top.
        assert_eq!(s.session, "n-798c5d0e353b");
        assert_eq!(s.session, mint_session("top", 0));
        assert!(status_text(&status_doc(&d)).contains(", profile desk, model llama3.3:70b"));
        assert_eq!(end_subtree(&d, &g, "top", true), vec!["n-798c5d0e353b".to_string(), "top".to_string()]);
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_node_with_no_report_rolls_up_its_childrens_overrun() {
        let (d, g) = grow(Some(1000));
        kid(&d, &g, "top", Some("c"), 100, SpawnOpts::default());
        kid(&d, &g, "c", Some("g"), 80, SpawnOpts::default());
        end(&d, &g, "g", Some(BigInt::from(150)), None).unwrap();
        let left = || {
            let t = read_tree(&d);
            t.left(&t.nodes["top"], "tokens").unwrap()
        };
        assert_eq!(left(), BigInt::from(850));
        end(&d, &g, "c", None, None).unwrap();
        assert_eq!(left(), BigInt::from(850));
        kid(&d, &g, "top", Some("x"), 10, SpawnOpts::default());
        kid(&d, &g, "x", Some("y"), 5, SpawnOpts::default());
        kid(&d, &g, "y", Some("z"), 1, SpawnOpts::default());
        end(&d, &g, "z", Some(BigInt::from(400)), None).unwrap();
        end_subtree(&d, &g, "x", true);
        assert_eq!(left(), BigInt::from(450));
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_child_with_no_deadline_takes_the_parents() {
        let parent = env(r#"{"id": "p", "generated_by": "t", "task": "t", "deadline": 100, "permissions": {}}"#);
        let child = env(r#"{"id": "c", "generated_by": "t", "task": "t", "permissions": {}}"#);
        let r = edge_ok(Some(&parent), Some(&child), DEFAULT_TIMEOUT_MS);
        assert!(r.holds && r.inherited);
        assert_eq!(q(r.child.unwrap().value("deadline")), "100.0");
        assert!(child.value("deadline").is_null());
    }

    #[test]
    fn a_kept_invariant_is_proved_by_z3() {
        let inv = r#"{"type": "no_push", "description": "d", "expr": {"op": "not_equals", "path": "command", "value": "git push"}}"#;
        let parent = env(&format!(r#"{{"id": "p", "generated_by": "t", "task": "t", "invariants": [{inv}], "permissions": {{"shell": true, "shell_allowlist": ["git"]}}}}"#));
        let child = env(&format!(r#"{{"id": "c", "generated_by": "t", "task": "t", "invariants": [{inv}], "permissions": {{"shell": true, "shell_allowlist": ["git"]}}}}"#));
        let typed = serde_json::from_str::<crate::models::Envelope>(&dumps(&Value::Obj(parent.clone()), false));
        assert!(typed.is_ok(), "{:?}", typed.err());
        let pe = typed.unwrap();
        let r = crate::subsumption::envelope_subsumes(&pe, &pe, true, Some(2000));
        assert!(r.is_ok(), "{:?}", r.err());
        let r = edge_ok(Some(&parent), Some(&child), DEFAULT_TIMEOUT_MS);
        assert!(r.holds, "{:?}", r.reasons);
    }
}
