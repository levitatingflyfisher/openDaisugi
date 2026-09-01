//! The oracle's distiller (`opendaisugi/distiller.py`): successful journal
//! traces clustered by the active matcher, each cluster intersected,
//! salvaged or generalized by a model, verified, and stored as a compiled
//! pathway. The Go client's `internal/distill` is the reference.
//!
//! Every stored pathway is verified with the linked Z3 first, and the
//! distiller fails closed: a violation, a Z3 error, or a Z3 check that did
//! not finish drops the cluster.

pub mod params;
pub mod repeats;

use std::collections::{HashMap, HashSet};
use std::sync::OnceLock;

use regex::Regex;

use crate::garden::{format_f, intersect_permissions};
use crate::gate::py::text::strip;
use crate::gate::pyjson::{dumps, Object, Value};
use crate::llm::{Call, CallError, Client, Schema};
use crate::pathways::find::{Embedder, Matcher, EMBEDDING_MODEL_VERSION};
use crate::pathways::potion;
use crate::pathways::importer::py_row;
use crate::pathways::pathway::{dump_json_indent, Pathway};
use crate::pathways::pmodel::Id;
use crate::pathways::store::{Col, Store};
use crate::pathways::PwErr;
use crate::tracejournal::{dag, Distillable, Journal, LoadError, Record};

/// Where the distiller keeps what it makes: the store file, or the
/// `PathwayStore(":memory:")` of a dry run.
pub enum Sink<'a> {
    Disk(&'a Store),
    Mem(Vec<Pathway>),
}

impl Sink<'_> {
    fn all(&self) -> Result<Vec<Pathway>, PwErr> {
        match self {
            Sink::Disk(s) => s.read_all(&|_| false),
            Sink::Mem(ps) => Ok(ps.clone()),
        }
    }

    /// `put()`: INSERT OR REPLACE by id.
    fn put(&mut self, p: Pathway) -> Result<(), PwErr> {
        match self {
            Sink::Disk(s) => s.put(&py_row(&p)?),
            Sink::Mem(ps) => {
                ps.retain(|q| q.id() != p.id());
                ps.push(p);
                Ok(())
            }
        }
    }
}

/// The Distiller's settings.
pub struct Options {
    pub model: String,
    pub min_traces: i128,
    pub lookback_days: i128,
    pub threshold: f64,
    pub validation_split: f64,
    pub structure_weight: f64,
    /// The active matcher's provenance stamp, which rows are stamped with
    /// and checked against.
    pub identity: String,
    /// verify()'s default.
    pub z3_timeout_ms: u32,
}

/// `TendReport`.
#[derive(Default)]
pub struct Report {
    pub created: usize,
    pub updated: usize,
    pub skipped: usize,
    pub pathways: Vec<String>,
    pub warnings: Vec<String>,
}

/// Why tend stopped.
#[derive(Debug)]
pub enum TendErr {
    Pw(PwErr),
    /// The model call failed outside the call itself.
    Os(std::io::Error),
}

impl From<PwErr> for TendErr {
    fn from(e: PwErr) -> Self {
        TendErr::Pw(e)
    }
}

/// What the distiller read before anything was written.
#[derive(Default)]
pub struct Prepared {
    records: HashMap<String, Record>,
    load_errors: HashMap<String, LoadError>,
    refinements: HashMap<String, Vec<Object>>,
}

/// Reads, before anything is written, every trace body and refinement
/// record the run may need, so that one this binary cannot read refuses
/// the run with nothing changed. Load errors Python meets are kept and
/// reported where Python reports them.
pub fn prepare(j: &Journal, traces: &[Distillable]) -> Result<Prepared, PwErr> {
    let mut p = Prepared::default();
    for t in traces {
        match j.load_trace(&t.trace_id)? {
            Ok(r) => {
                p.records.insert(t.trace_id.clone(), r);
            }
            Err(e) => {
                p.load_errors.insert(t.trace_id.clone(), e);
            }
        }
        if let Some(run) = t.run_id.as_deref().filter(|r| !r.is_empty()) {
            if !p.refinements.contains_key(run) {
                let recs = j.refinements(run)?;
                p.refinements.insert(run.to_string(), recs);
            }
        }
    }
    Ok(p)
}

fn preamble() -> &'static [Regex] {
    static RES: OnceLock<Vec<Regex>> = OnceLock::new();
    RES.get_or_init(|| {
        [
            r"(?m)^[ \t]*Base directory for this skill:[^\n]*\n?",
            r"(?m)^[ \t]*###[ \t]+Skill:[^\n]*\n?",
            r"(?m)^[ \t]*Path:[ \t]+(?:plugin|bundled):[^\n]*\n?",
            r"<command-name>[^<]*</command-name>|<command-message>[^<]*</command-message>|<command-args>[^<]*</command-args>",
        ]
        .iter()
        .map(|p| Regex::new(p).expect("a fixed pattern"))
        .collect()
    })
}

/// `_normalize_task_for_embedding`.
pub fn normalize_task(task: &str) -> String {
    let mut s = task.to_string();
    for re in preamble() {
        s = re.replace_all(&s, "").into_owned();
    }
    let s = strip(&s);
    if s.is_empty() {
        task.to_string()
    } else {
        s.to_string()
    }
}

/// `secrets.token_hex(4)`. A failed read is an error, never a fixed id.
pub fn token_hex() -> Result<String, PwErr> {
    use std::io::Read;
    let mut b = [0u8; 4];
    std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(&mut b)).map_err(PwErr::Io)?;
    Ok(b.iter().map(|x| format!("{x:02x}")).collect())
}

fn norm(v: &[f64]) -> f64 {
    v.iter().fold(0.0, |s, x| s + x * x).sqrt()
}

/// `vecs[members].mean(axis=0)`: each column summed row by row, then
/// divided by the count.
fn mean_rows(vecs: &[Vec<f64>], members: &[usize]) -> Vec<f64> {
    let mut out = vec![0.0; vecs[members[0]].len()];
    for m in members {
        for (j, x) in vecs[*m].iter().enumerate() {
            if j < out.len() {
                out[j] += x;
            }
        }
    }
    let n = members.len() as f64;
    out.iter().map(|x| x / n).collect()
}

struct Cluster {
    members: Vec<usize>,
    centroid: Vec<f64>,
}

/// `_cluster_with_centroids`.
fn cluster_with_centroids(vecs: &[Vec<f64>], threshold: f64) -> Vec<Cluster> {
    let mut cs: Vec<Cluster> = vec![];
    for (i, v) in vecs.iter().enumerate() {
        let mut vn = norm(v);
        if vn == 0.0 {
            vn = 1e-9;
        }
        let (mut best, mut best_sim) = (None, -1.0f64);
        for (ci, c) in cs.iter().enumerate() {
            let mut cn = norm(&c.centroid);
            if cn == 0.0 {
                cn = 1e-9;
            }
            let mut dot = 0.0;
            for (j, x) in v.iter().enumerate() {
                dot += (x / vn) * (c.centroid.get(j).copied().unwrap_or(0.0) / cn);
            }
            if dot > best_sim {
                best_sim = dot;
                best = Some(ci);
            }
        }
        match best {
            Some(b) if best_sim >= threshold => {
                cs[b].members.push(i);
                cs[b].centroid = mean_rows(vecs, &cs[b].members);
            }
            _ => cs.push(Cluster { members: vec![i], centroid: v.clone() }),
        }
    }
    cs
}

const GENERALIZE_SYSTEM: &str = "You are a planner distilling multiple successful runs into a reusable template.\n\
Produce a generalized task description and a concrete plan template.\n\
The plan template must be a valid ActionPlan with REAL (not placeholder) values \u{2014}\n\
concrete paths, commands, URLs; use the cluster's most representative values.\n\
At reuse, only the fields that varied across the cluster are re-bound (a typed,\n\
re-verified data bind, ADR-0008); the rest run as-is. No <placeholder> tokens.";

const IMPROVE_SYSTEM: &str = "You are tightening an envelope that was too restrictive for some valid plans.\n\
Widen ONLY the specific permissions that blocked the failing plans.\n\
Do not loosen anything else. Return a revised Envelope.";

const MAX_PITFALLS: usize = 20;

/// One tend run's inputs.
pub struct Distiller<'a> {
    pub prepared: Prepared,
    pub sink: Sink<'a>,
    /// The active matcher; None for one this binary does not carry (the
    /// command checked the run will not need its embedder).
    pub matcher: Option<Matcher>,
    pub pe: &'a potion::Env,
    /// The embedder, loaded on first use, or why it could not be.
    pub emb: std::cell::OnceCell<Result<Embedder, String>>,
    pub llm: &'a mut Client,
    pub opt: Options,
    pub now: &'a dyn Fn() -> f64,
}

impl Distiller<'_> {
    /// The embedder's encode over the task texts.
    fn embed(&self, texts: &[String], normalize: bool) -> Result<Vec<Vec<f64>>, String> {
        let m = match &self.matcher {
            Some(m) => m,
            None => return Err("the matcher's embedder is not in this binary".into()),
        };
        let emb = match self.emb.get_or_init(|| load_embedder(m, self.pe)) {
            Ok(e) => e,
            Err(why) => return Err(why.clone()),
        };
        Ok(texts.iter().map(|t| if normalize { emb.encode(&normalize_task(t)) } else { emb.encode(t) }).collect())
    }

    /// `PathwayStore.reembed_stale` with the distiller's embedder.
    fn reembed_stale(&mut self) -> Result<usize, PwErr> {
        let store = match &self.sink {
            Sink::Disk(s) => *s,
            Sink::Mem(_) => return Ok(0),
        };
        let rows = store.provenance()?;
        let is = |c: &Col, s: &str| matches!(c, Col::Text(t) if t == s);
        let stale: Vec<&[Col; 4]> =
            rows.iter().filter(|r| !is(&r[2], &self.opt.identity) || !is(&r[3], EMBEDDING_MODEL_VERSION)).collect();
        if stale.is_empty() {
            return Ok(0);
        }
        let mut tasks = vec![];
        for r in &stale {
            match &r[1] {
                Col::Text(t) => tasks.push(t.clone()),
                _ => return Err(PwErr::Unreadable("a task_description is not text".into())),
            }
        }
        let vecs = match self.embed(&tasks, true) {
            Ok(v) => v,
            // Logged on the opendaisugi logger, which prints nothing by
            // default.
            Err(_) => return Ok(0),
        };
        for (r, v) in stale.iter().zip(vecs) {
            let id = match &r[0] {
                Col::Text(t) => t.clone(),
                _ => return Err(PwErr::Unreadable("an id is not text".into())),
            };
            let text = dumps(&Value::List(v.into_iter().map(Value::Float).collect()), true);
            store.update_embedding(&id, &text, &self.opt.identity, EMBEDDING_MODEL_VERSION)?;
        }
        Ok(stale.len())
    }

    /// `Distiller.tend`.
    pub fn tend(&mut self, traces: &[Distillable]) -> Result<Report, TendErr> {
        let mut rep = Report::default();
        let n = self.reembed_stale()?;
        if n > 0 {
            rep.warnings.push(format!("re-embedded {n} pathways under the current embedder."));
        }
        if (traces.len() as i128) < self.opt.min_traces {
            rep.skipped = traces.len();
            rep.warnings.push(format!(
                "tend: only {} successful trace(s) in the last {} days, below min_traces={}; no pathways distilled.",
                traces.len(),
                self.opt.lookback_days,
                self.opt.min_traces
            ));
            return Ok(rep);
        }
        let tasks: Vec<String> = traces.iter().map(|t| t.task.clone()).collect();
        let sigs: Vec<String> = traces.iter().map(|t| t.signature.clone().unwrap_or_default()).collect();
        let task_vecs = match self.embed(&tasks, true) {
            Ok(v) => v,
            Err(why) => {
                rep.skipped = traces.len();
                rep.warnings.push(format!("tend: {why} Built the verified journal but distilled 0 pathways."));
                return Ok(rep);
            }
        };
        let vecs = if self.opt.structure_weight > 0.0 {
            let struct_vecs = self.embed(&sigs, false).map_err(|w| PwErr::Invalid(format!("RuntimeError: {w}")))?;
            let tw = (1.0 - self.opt.structure_weight).powf(0.5);
            let sw = self.opt.structure_weight.powf(0.5);
            task_vecs
                .iter()
                .zip(&struct_vecs)
                .map(|(t, s)| t.iter().map(|x| x * tw).chain(s.iter().map(|x| x * sw)).collect())
                .collect()
        } else {
            task_vecs.clone()
        };
        for c in cluster_with_centroids(&vecs, self.opt.threshold) {
            if (c.members.len() as i128) < self.opt.min_traces {
                rep.skipped += 1;
                continue;
            }
            let ct: Vec<Distillable> = c.members.iter().map(|i| traces[*i].clone()).collect();
            let existing = self.find_existing_covering(&ct)?;
            if let Some(e) = &existing {
                if !has_new_traces(e, &ct) {
                    rep.skipped += 1;
                    continue;
                }
            }
            let centroid = mean_rows(&task_vecs, &c.members);
            match self.distill_cluster(&ct, centroid, &mut rep.warnings)? {
                None => rep.skipped += 1,
                Some(p) => {
                    rep.pathways.push(p.id().to_string());
                    self.sink.put(p)?;
                    if existing.is_none() {
                        rep.created += 1;
                    } else {
                        rep.updated += 1;
                    }
                }
            }
        }
        Ok(rep)
    }

    fn find_existing_covering(&self, ct: &[Distillable]) -> Result<Option<Pathway>, PwErr> {
        let ids: HashSet<&str> = ct.iter().map(|t| t.trace_id.as_str()).collect();
        for p in self.sink.all()? {
            if sources(&p).iter().any(|s| ids.contains(s.as_str())) {
                return Ok(Some(p));
            }
        }
        Ok(None)
    }

    fn load_records(&self, ts: &[Distillable], warnings: &mut Vec<String>) -> Vec<Record> {
        let mut out = vec![];
        for t in ts {
            match self.prepared.records.get(&t.trace_id) {
                Some(r) => out.push(r.clone()),
                None => {
                    let why = self.prepared.load_errors.get(&t.trace_id).map(|e| e.msg.clone()).unwrap_or_default();
                    warnings.push(format!("load_trace({}) failed: {why}", t.trace_id));
                }
            }
        }
        out
    }

    /// One verify() as the distiller reads it: None when it passes, else
    /// why it does not (the first violation, or a Z3 check that did not
    /// finish). What this binary cannot verify is a failure too, with its
    /// reason: the distiller never stores it.
    fn verified(&self, plan: &Object, env: &Object) -> Result<Option<String>, PwErr> {
        verify_for_store(plan, env, self.opt.z3_timeout_ms)
    }

    fn validate_envelope(&self, env: &Object, plans: &[Object]) -> Result<(f64, Vec<Object>), PwErr> {
        if plans.is_empty() {
            return Ok((0.0, vec![]));
        }
        let mut failing = vec![];
        let mut passed = 0usize;
        for p in plans {
            if self.verified(p, env)?.is_none() {
                passed += 1;
            } else {
                failing.push(p.clone());
            }
        }
        Ok((passed as f64 / plans.len() as f64, failing))
    }

    /// A structured call: the reply, or the warning text of a failed call.
    fn ask(&mut self, system: &str, user: &str, response: Schema) -> Result<Result<Object, String>, TendErr> {
        let model = self.opt.model.clone();
        let r = self.llm.structured(&Call { model: &model, system, user, response, max_retries: 2 });
        match r {
            Ok(o) => Ok(Ok(o)),
            Err(CallError::Model(m)) => Ok(Err(m)),
            Err(CallError::Os(e)) => Err(TendErr::Os(e)),
        }
    }

    fn distill_cluster(&mut self, ct: &[Distillable], centroid: Vec<f64>, warnings: &mut Vec<String>) -> Result<Option<Pathway>, TendErr> {
        let split = ((ct.len() as f64 * self.opt.validation_split) as usize).clamp(1, ct.len());
        let (train, test) = ct.split_at(split);
        let train_recs = self.load_records(train, warnings);
        let test_recs = if test.is_empty() { vec![] } else { self.load_records(test, warnings) };
        if train_recs.is_empty() {
            warnings.push("cluster skipped: no loadable train traces after errors".into());
            return Ok(None);
        }
        let perms: Vec<&Object> =
            train_recs.iter().filter_map(|r| r.envelope.value("permissions").as_obj()).collect();
        let rep = &train_recs[train_recs.len() - 1];
        let mut env = rep.envelope.clone();
        env.set("permissions", intersect_permissions(&perms));
        env.set("generated_by", "distilled");

        let all_plans: Vec<Object> = train_recs.iter().chain(&test_recs).map(|r| r.plan.clone()).collect();
        let (divergent, salvage_params) = match params::plan_divergence(&all_plans) {
            Ok(x) => x,
            Err(e) => {
                warnings.push(format!("divergence analysis failed (continuing frozen): {}", e.msg));
                (vec![], vec![])
            }
        };
        let mut workspace = None;
        if !divergent.is_empty() {
            let file_read = str_list(env.value("permissions").as_obj().map(|p| p.value("file_read")).unwrap_or(&Value::Null));
            workspace = params::salvage_workspace(&file_read);
            if workspace.is_none() {
                warnings.push(
                    "delegated salvage skipped: no file_read glob gives the leaf a workspace; falling back to frozen generalization"
                        .into(),
                );
            }
        }
        if let Some(ws) = workspace {
            let salvaged = params::build_delegated_template(&rep.plan, &divergent, &all_plans, &ws)
                .map_err(|e| PwErr::Invalid(format!("{}: {}", e.typ, e.msg)))?;
            match self.verified(&salvaged, &env)? {
                None => {
                    let sig = dag::structure_signature(&salvaged);
                    let task = ct[ct.len() - 1].task.clone();
                    return Ok(Some(self.pathway(&task, centroid, env, salvaged, ct, sig, salvage_params)?));
                }
                Some(why) => warnings.push(format!(
                    "delegated salvage template failed verification ({why}) \u{2014} falling back to frozen generalization"
                )),
            }
        }
        let mut pitfalls: Vec<String> = vec![];
        let mut seen: HashSet<String> = HashSet::new();
        for t in ct {
            let run = match t.run_id.as_deref().filter(|r| !r.is_empty()) {
                Some(r) => r,
                None => continue,
            };
            // _extract_pitfalls per run, then the global dedupe.
            for rec in self.prepared.refinements.get(run).map(|v| v.as_slice()).unwrap_or(&[]) {
                if let Value::List(vs) = rec.value("violations") {
                    for v in vs {
                        let vo = match v.as_obj() {
                            Some(o) => o,
                            None => continue,
                        };
                        let msg = format!(
                            "[{}] {}",
                            vo.value("stage").as_str().unwrap_or(""),
                            vo.value("message").as_str().unwrap_or("")
                        );
                        if seen.insert(msg.clone()) {
                            pitfalls.push(msg);
                        }
                    }
                }
            }
        }
        if pitfalls.len() > MAX_PITFALLS {
            let more = pitfalls.len() - MAX_PITFALLS;
            pitfalls.truncate(MAX_PITFALLS);
            pitfalls.push(format!("... ({more} more pitfall(s) truncated)"));
        }
        let block = if pitfalls.is_empty() {
            "(none recorded)".to_string()
        } else {
            pitfalls.iter().map(|p| format!("- {p}")).collect::<Vec<_>>().join("\n")
        };
        let user = format!(
            "Representative plan:\n{}\n\nEnvelope constraints:\n{}\n\nKnown pitfalls from past rejections:\n{block}\n\n\
             Produce: a generalized task_description (broad enough for similar tasks) and a plan_template with concrete representative values.",
            dump_json_indent(&Value::Obj(rep.plan.clone()), 2),
            dump_json_indent(&Value::Obj(env.clone()), 2)
        );
        let gen = match self.ask(GENERALIZE_SYSTEM, &user, Schema { name: "GeneralizedTemplate", model: Id::GeneralizedTemplate })? {
            Ok(g) => g,
            Err(m) => {
                warnings.push(format!("cluster generalization failed: {m}"));
                return Ok(None);
            }
        };
        let test_plans: Vec<Object> = test_recs.iter().map(|r| r.plan.clone()).collect();
        let (mut score, failing) = self.validate_envelope(&env, &test_plans)?;
        if score < 0.5 && !failing.is_empty() {
            let blocks: Vec<String> = failing
                .iter()
                .map(|p| format!("Plan {}:\n{}", p.value("id").as_str().unwrap_or(""), dump_json_indent(&Value::Obj(p.clone()), 2)))
                .collect();
            let iu = format!(
                "Current envelope (too tight):\n{}\n\nPlans that should have passed but were rejected:\n{}\n\n\
                 Return a revised envelope that passes these plans while keeping\nall other permissions tight.",
                dump_json_indent(&Value::Obj(env.clone()), 2),
                blocks.join("\n\n")
            );
            match self.ask(IMPROVE_SYSTEM, &iu, Schema { name: "Envelope", model: Id::Envelope })? {
                Err(m) => warnings.push(format!("cluster improvement pass failed: {m}")),
                Ok(improved) => {
                    let (new_score, _) = self.validate_envelope(&improved, &test_plans)?;
                    if new_score > score {
                        env = improved;
                        score = new_score;
                    } else {
                        warnings.push(format!(
                            "cluster improvement pass did not increase score ({} \u{2192} {})",
                            format_f(score, 2),
                            format_f(new_score, 2)
                        ));
                    }
                }
            }
        }
        let _ = score;
        let template = match gen.value("plan_template") {
            Value::Obj(o) => o.clone(),
            _ => return Err(PwErr::Invalid("ValidationError: the reply has no plan_template".into()).into()),
        };
        if let Some(why) = self.verified(&template, &env)? {
            warnings.push(format!("distilled plan_template does not verify against its envelope ({why}); dropping cluster"));
            return Ok(None);
        }
        let sig = dag::structure_signature(&template);
        let ps = match params::diff_plans_for_parameters(&all_plans).and_then(|p| params::rekey_to_template(p, &template)) {
            Ok(p) => p,
            Err(e) => {
                warnings.push(format!("parameter diff failed (kept frozen): {}", e.msg));
                vec![]
            }
        };
        let task = gen.value("task_description").as_str().unwrap_or("").to_string();
        Ok(Some(self.pathway(&task, centroid, env, template, ct, sig, ps)?))
    }

    #[allow(clippy::too_many_arguments)]
    fn pathway(
        &self,
        task: &str,
        centroid: Vec<f64>,
        env: Object,
        plan: Object,
        ct: &[Distillable],
        sig: Option<String>,
        params: Vec<Value>,
    ) -> Result<Pathway, PwErr> {
        let mut o = Object::new();
        o.set("id", format!("pathway_{}", token_hex()?));
        o.set("task_description", task);
        o.set("task_embedding", Value::List(centroid.into_iter().map(Value::Float).collect()));
        o.set("embedding_model", self.opt.identity.as_str());
        o.set("embedding_model_version", EMBEDDING_MODEL_VERSION);
        o.set("envelope", env);
        o.set("plan_template", plan);
        o.set("source_trace_ids", Value::List(ct.iter().map(|t| Value::Str(t.trace_id.clone())).collect()));
        o.set("version", Value::Int("1".into()));
        o.set("hit_count", Value::Int("0".into()));
        o.set("distilled_at", (self.now)());
        o.set("last_activation_at", 0.0);
        o.set("failure_count", Value::Int("0".into()));
        o.set("activation_count", Value::Int("0".into()));
        o.set("structure_signature", sig);
        o.set("parameters", Value::List(params));
        Ok(Pathway::new(o))
    }
}

/// One verify() as the distiller reads it, on the linked Z3: None when
/// the plan passes, else why it does not (the first violation, or a Z3
/// check that did not finish). What this binary cannot verify is a
/// failure too, with its reason: the distiller never stores it.
pub fn verify_for_store(plan: &Object, env: &Object, z3_timeout_ms: u32) -> Result<Option<String>, PwErr> {
    match crate::pathways::verify::verify(&Value::Obj(plan.clone()), &Value::Obj(env.clone()), None, z3_timeout_ms) {
        Ok(o) => {
            if !o.timeouts.is_empty() {
                return Ok(Some("verifier timed out; raise the Z3 timeout".into()));
            }
            Ok(o.violations.first().map(|v| v.message.clone()))
        }
        Err(PwErr::Unreadable(why)) => Ok(Some(format!("this binary cannot verify it: {why}"))),
        Err(e) => Err(e),
    }
}

/// The matcher's embedder: the lexical one, or the potion model loaded.
pub fn load_embedder(m: &Matcher, pe: &potion::Env) -> Result<Embedder, String> {
    match m {
        Matcher::Lexical => Ok(Embedder::Lexical),
        Matcher::Potion { identity } => match potion::open(identity, pe) {
            Ok(model) => Ok(Embedder::Potion(Box::new(model))),
            Err(potion::NotAvailable(why)) => Err(why),
        },
    }
}

fn sources(p: &Pathway) -> Vec<String> {
    str_list(p.obj.value("source_trace_ids"))
}

fn has_new_traces(p: &Pathway, ct: &[Distillable]) -> bool {
    let known: HashSet<String> = sources(p).into_iter().collect();
    ct.iter().any(|t| !known.contains(&t.trace_id))
}

/// A validated list[str] value as strings.
pub fn str_list(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l.iter().filter_map(|x| x.as_str().map(|s| s.to_string())).collect(),
        _ => vec![],
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pathways::pmodel::{validate_json, Id};

    fn obj(id: Id, text: &str) -> Object {
        match validate_json(id, text) {
            Ok(Value::Obj(o)) => o,
            other => panic!("{:?}", other.err().map(|e| e.text())),
        }
    }

    /// Nothing is stored that the linked Z3 did not pass: a Z3 check that
    /// answers unknown, a violation, or a template the binary cannot
    /// verify each give a reason, never a pass.
    #[test]
    fn a_template_is_stored_only_when_it_verifies() {
        let plan = obj(Id::ActionPlan, r#"{"id":"p","source":"s","task":"t","steps":[{"id":"a","type":"shell","command":"make test"}]}"#);
        let env = |task: &str, extra: &str| {
            obj(
                Id::Envelope,
                &format!(r#"{{"generated_by":"g","task":"{task}","permissions":{{"shell":true,"shell_allowlist":["make"]{extra}}}}}"#),
            )
        };
        assert_eq!(verify_for_store(&plan, &env("t", ""), 500).unwrap(), None);
        let unknown = env(crate::pathways::verify::FORCE_Z3_UNKNOWN_TASK, "");
        assert_eq!(verify_for_store(&plan, &unknown, 500).unwrap().as_deref(), Some("verifier timed out; raise the Z3 timeout"));
        let huge = env("t", r#","max_execution_time_s":100000000000000000000"#);
        assert_eq!(verify_for_store(&plan, &huge, 500).unwrap().as_deref(), Some("Envelope is internally inconsistent"));
        let narrow = obj(Id::Envelope, r#"{"generated_by":"g","task":"t","permissions":{"shell":true,"shell_allowlist":["pytest"]}}"#);
        assert!(verify_for_store(&plan, &narrow, 500).unwrap().is_some());
        let llm = obj(
            Id::Envelope,
            r#"{"generated_by":"g","task":"t","permissions":{"shell":true,"shell_allowlist":["make"]},
                "invariants":[{"type":"rule","description":"d","expr":{"op":"llm_check","rule":"be nice"}}]}"#,
        );
        let why = verify_for_store(&plan, &llm, 500).unwrap().unwrap();
        assert!(why.starts_with("this binary cannot verify it: "), "{why}");
    }

    #[test]
    fn a_preamble_is_not_embedded() {
        assert_eq!(normalize_task("Base directory for this skill: /x\n### Skill: y\n<command-name>/b</command-name>build it"), "build it");
        assert_eq!(normalize_task("   "), "   ");
    }
}
