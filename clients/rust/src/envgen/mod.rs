//! The oracle's envelope generation (`envelope.py`): the Tier-0 pathway
//! lookup, the Tier-1 slot (`tier1.py`), the envelope cache
//! (`envelope_cache.py`), the Tier-2 model ladder with refinement hints,
//! and the checks around them (self-consistency, inheritance). It also
//! carries `pathway_bind` (bind a typed pathway's holes with one model
//! call, then verify again) and `compose` (pathways as skill steps).
//!
//! An envelope is the `model_dump()` value of `models.Envelope`, an
//! `Object` as `pathways::pmodel` validates it. The Go client's
//! `internal/envgen` is the reference.

pub mod bind;
pub mod cache;
pub mod compose;
pub mod inherit;
mod prompts_gen;
pub mod tier1;

pub use prompts_gen::{PROMPT_VERSION, SYSTEM_PROMPT, TIER1_PROMPT};

use crate::gate::py::text::{repr, repr_list, strip};
use crate::gate::pyjson::{Object, Value};
use crate::llm::{Call, CallError, Client, Schema};
use crate::pathways::find::{Cache as FindCache, FindErr};
use crate::pathways::pmodel::Id;
use crate::pathways::potion;
use crate::pathways::store::Store;
use crate::tracejournal::Journal;

use cache::{cache_key, Cache, CacheErr, KeyArgs};
use inherit::{verify_inheritance, InheritanceError};
use tier1::Tier1;

/// `generate_envelope`'s default model.
pub const DEFAULT_MODEL: &str = "anthropic/claude-sonnet-4-20250514";

/// `check_envelope_self_consistency`'s default budget, the one
/// generate_envelope uses.
pub const SELF_CONSISTENCY_TIMEOUT_MS: u32 = 500;

/// An exception the oracle raises out of the call: its class and text.
#[derive(Debug, Clone)]
pub struct PyError {
    pub class: String,
    pub msg: String,
}

impl PyError {
    pub fn new(class: impl Into<String>, msg: impl Into<String>) -> PyError {
        PyError { class: class.into(), msg: msg.into() }
    }
}

/// Why generation stopped.
#[derive(Debug)]
pub enum GenErr {
    /// The class and text the oracle raises.
    Py(PyError),
    /// `EnvelopeInheritanceError`.
    Inherit(InheritanceError),
    /// A cached envelope that does not validate (pydantic's
    /// ValidationError).
    CacheRow(String),
    /// A path of the oracle this binary does not take: the caller says
    /// "... is not in this binary yet." and changes nothing more.
    Unported(String),
    /// Something else failed (a database that does not open).
    Other(String),
}

impl From<CacheErr> for GenErr {
    fn from(e: CacheErr) -> Self {
        match e {
            CacheErr::Row(m) => GenErr::CacheRow(m),
            CacheErr::Sql(m) => GenErr::Other(m),
        }
    }
}

fn py(class: &str, msg: impl Into<String>) -> GenErr {
    GenErr::Py(PyError::new(class, msg))
}

pub(crate) fn obj(v: &Value) -> Object {
    v.as_obj().cloned().unwrap_or_default()
}

pub(crate) fn list_of(v: &Value) -> &[Value] {
    match v {
        Value::List(l) | Value::Tuple(l) => l,
        _ => &[],
    }
}

pub(crate) fn str_list(v: &Value) -> Vec<String> {
    list_of(v).iter().filter_map(|x| x.as_str().map(String::from)).collect()
}

pub(crate) fn float_of(v: &Value) -> Option<f64> {
    match v {
        Value::Float(f) => Some(*f),
        Value::Int(t) => t.parse::<f64>().ok(),
        Value::Bool(b) => Some(*b as i64 as f64),
        _ => None,
    }
}

/// `generate_envelope`'s arguments.
pub struct Options<'a> {
    pub task: String,
    pub context: Option<String>,
    pub parent: Option<Object>,
    pub summarize: bool,
    pub cache: Option<Cache>,
    /// The compiled-pathway store (Tier-0), with the matcher key and the
    /// potion environment. A threshold of None is find's default.
    pub store: Option<&'a Store>,
    pub matcher_key: String,
    pub potion: Option<&'a potion::Env>,
    pub threshold: Option<f64>,
    pub journal: Option<&'a Journal>,
    pub stakes: String,
    pub low_stakes: Option<Object>,
    /// The ladder; `single` is a bare-string model.
    pub models: Vec<String>,
    pub single: bool,
    pub thinking: String,
    pub tier1: Option<Tier1>,
    pub max_retries: usize,
    pub max_task_chars: usize,
}

impl Default for Options<'_> {
    fn default() -> Self {
        Options {
            task: String::new(),
            context: None,
            parent: None,
            summarize: false,
            cache: None,
            store: None,
            matcher_key: String::new(),
            potion: None,
            threshold: None,
            journal: None,
            stakes: "medium".into(),
            low_stakes: None,
            models: vec![DEFAULT_MODEL.into()],
            single: true,
            thinking: "standard".into(),
            tier1: None,
            max_retries: 3,
            max_task_chars: 4000,
        }
    }
}

/// The class of a failed model call: the HTTP client raises
/// ModelCallError, the claude-code client EnvelopeGenerationError.
fn err_class(c: &Client) -> &'static str {
    if c.backend() == "claude-code" {
        "EnvelopeGenerationError"
    } else {
        "ModelCallError"
    }
}

/// `thinking.thinking_kwargs` for the two keys the model clients send
/// (Gemini's thinking_config reaches no wire): the Messages wire's budget
/// and the chat wire's reasoning effort.
pub fn thinking_opts(model: &str, budget: &str) -> (i64, &'static str) {
    let m = model.to_lowercase();
    if m.starts_with("anthropic/") || m.starts_with("claude-") {
        return (if budget == "deep" { 16000 } else { 0 }, "");
    }
    let bare = m.strip_prefix("openai/").unwrap_or(&m);
    let b = bare.as_bytes();
    let reasoning = b.len() >= 2 && b[0] == b'o' && (b'1'..=b'9').contains(&b[1]);
    if reasoning {
        let effort = match budget {
            "light" => "low",
            "standard" => "medium",
            "deep" => "high",
            _ => "",
        };
        return (0, effort);
    }
    (0, "")
}

/// `check_envelope_self_consistency`, failing closed: a check that did
/// not finish, or an envelope it cannot read, is not consistent, as in the
/// oracle (K1-2). The text is the first violation's message.
pub fn self_consistent(env: &Object) -> (bool, String) {
    crate::pathways::verify::self_consistent(&Value::Obj(env.clone()), SELF_CONSISTENCY_TIMEOUT_MS)
}

fn timestamp(rec: &Object) -> f64 {
    float_of(rec.value("timestamp")).unwrap_or(0.0)
}

/// `_refinement_hints_block`.
pub fn hints_block(records: &[Object]) -> String {
    if records.is_empty() {
        return String::new();
    }
    let mut order: Vec<String> = vec![];
    let mut newest: std::collections::HashMap<String, f64> = Default::default();
    let mut stage: std::collections::HashMap<String, String> = Default::default();
    for rec in records {
        let ts = timestamp(rec);
        for v in list_of(rec.value("violations")) {
            let vo = obj(v);
            let msg = vo.value("message").as_str().unwrap_or_default().to_string();
            match newest.get(&msg) {
                None => {
                    order.push(msg.clone());
                    newest.insert(msg.clone(), ts);
                }
                Some(old) if ts > *old => {
                    newest.insert(msg.clone(), ts);
                }
                _ => {}
            }
            stage.entry(msg).or_insert_with(|| vo.value("stage").as_str().unwrap_or_default().to_string());
        }
    }
    // sorted(key=-ts) is stable: ties keep first-seen order.
    order.sort_by(|a, b| (-newest[a]).partial_cmp(&-newest[b]).unwrap_or(std::cmp::Ordering::Equal));
    order.truncate(10);
    let mut lines: Vec<String> = [
        "",
        "## Prior Rejections",
        "",
        "Previous plans verified against envelopes for this task were rejected.",
        "Generate an envelope that prevents these violations:",
        "",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect();
    for m in &order {
        lines.push(format!("- [{}] {m}", stage[m]));
    }
    lines.join("\n")
}

/// What generation gave: the envelope, and the stale-embeddings warning
/// Tier-0's find gave, if any.
pub struct Generated {
    pub envelope: Result<Object, GenErr>,
    pub find_warning: String,
}

impl Options<'_> {
    fn refinements(&self, key: &str) -> Vec<Object> {
        self.journal.and_then(|j| j.refinements_by_key(key).ok()).unwrap_or_default()
    }

    fn parent_id(&self) -> Option<String> {
        self.parent.as_ref().and_then(|p| p.value("id").as_str().map(String::from))
    }

    fn key(&self, model: &str, tier1: Option<&str>) -> String {
        let parent = self.parent_id();
        cache_key(&KeyArgs {
            task: &self.task,
            context: self.context.as_deref(),
            model,
            parent: parent.as_deref(),
            summarize: self.summarize,
            thinking: &self.thinking,
            tier1,
        })
    }

    /// Stamps parent_envelope and checks inheritance, as the oracle does
    /// for a Tier-1 or Tier-2 envelope.
    fn adopt(&self, env: &mut Object) -> Result<(), GenErr> {
        let Some(parent) = &self.parent else { return Ok(()) };
        env.set("parent_envelope", parent.value("id").clone());
        let vs = verify_inheritance(env, parent);
        if vs.is_empty() {
            Ok(())
        } else {
            Err(GenErr::Inherit(InheritanceError(vs)))
        }
    }
}

/// `generate_envelope`.
pub fn generate(o: &Options, c: &mut Client) -> Generated {
    let mut warning = String::new();
    let envelope = generate_inner(o, c, &mut warning);
    Generated { envelope, find_warning: warning }
}

fn generate_inner(o: &Options, c: &mut Client, warning: &mut String) -> Result<Object, GenErr> {
    if o.task.is_empty() || strip(&o.task).is_empty() {
        return Err(py("ValueError", "Task must be a non-empty string."));
    }
    let n = o.task.chars().count() + o.context.as_deref().map(|x| x.chars().count()).unwrap_or(0);
    if n > o.max_task_chars {
        return Err(py(
            "TaskTooLongError",
            format!(
                "Task + context is {n} chars (limit: {}). Summarize before passing, or increase max_task_chars.",
                o.max_task_chars
            ),
        ));
    }
    if o.stakes == "low" {
        let Some(low) = &o.low_stakes else {
            return Err(py(
                "LowStakesNotConfigured",
                "stakes='low' requires a configured envelope. Pass low_stakes_envelope=... or construct the facade via \
                 Daisugi.with_default_low_stakes().",
            ));
        };
        return Ok(low.clone());
    }
    if o.models.is_empty() {
        return Err(py("ValueError", "model must be a non-empty string or non-empty list of strings."));
    }

    let mut user = format!("Task: {}", o.task);
    if let Some(ctx) = o.context.as_deref().filter(|x| !x.is_empty()) {
        user.push_str(&format!("\n\nContext:\n{ctx}"));
    }
    if o.summarize {
        user.push_str(prompts_gen::SUMMARIZE_INSTRUCTION);
    }

    // Tier-0: a compiled pathway.
    if let (Some(store), true) = (o.store, o.stakes != "high") {
        let default_env;
        let pe = match o.potion {
            Some(p) => p,
            None => {
                default_env = potion::Env::from_process(Default::default(), String::new());
                &default_env
            }
        };
        match store.find(&o.task, &o.matcher_key, pe, o.threshold, &mut FindCache::default()) {
            Err(FindErr::NotCarried(_)) => return Err(GenErr::Unported(format!("the {} matcher", o.matcher_key))),
            // find raising is logged, and generation falls through.
            Err(FindErr::Err(_)) => {}
            Ok(r) => {
                *warning = r.warning;
                if let Some((p, _)) = r.matched {
                    store.increment_hit(p.id()).map_err(|e| GenErr::Other(e.to_string()))?;
                    let mut env = obj(p.obj.value("envelope"));
                    env.set("generated_by", format!("compiled-pathway:{}", p.id()));
                    return Ok(env);
                }
            }
        }
    }

    // Tier-1: the local-model slot.
    if let (Some(t1), true) = (&o.tier1, o.stakes != "high") {
        let first = &o.models[0];
        let t1key = o.key(first, Some(&t1.name));
        if let Some(cache) = &o.cache {
            if let Some(mut cached) = cache.get(&t1key)? {
                if cached.value("cache_key").is_null() {
                    cached.set("cache_key", t1key.as_str());
                }
                return Ok(cached);
            }
        }
        if let Some(mut env) = t1.generate(c, &o.task, o.context.as_deref()) {
            if self_consistent(&env).0 {
                env.set("generated_by", format!("tier1:{}", t1.name));
                o.adopt(&mut env)?;
                env.set("cache_key", t1key.as_str());
                if let Some(cache) = &o.cache {
                    cache.put(&env, &t1key);
                }
                return Ok(env);
            }
        }
    }

    // The cache, every rung up front.
    if let (Some(cache), true) = (&o.cache, o.stakes != "high") {
        for rung in &o.models {
            let key = o.key(rung, None);
            let Some(mut cached) = cache.get(&key)? else { continue };
            let at = cache.inserted_at(&key)?;
            let recs = o.refinements(&key);
            if let (Some(at), false) = (at, recs.is_empty()) {
                let newest = recs.iter().map(timestamp).fold(f64::NEG_INFINITY, f64::max);
                if newest > at {
                    cache.invalidate(&key);
                    break;
                }
            }
            if cached.value("cache_key").is_null() {
                cached.set("cache_key", key.as_str());
            }
            return Ok(cached);
        }
    }

    // The ladder.
    let (mut last_class, mut last_msg) = (String::new(), String::new());
    for rung in &o.models {
        let key = o.key(rung, None);
        let rung_user = format!("{user}{}", hints_block(&o.refinements(&key)));
        if let Err(e) = c.preflight_model(rung) {
            return Err(match e {
                CallError::Model(m) => py("LLMNotConfigured", m),
                other => GenErr::Other(format!("{other:?}")),
            });
        }
        let (thinking, effort) = thinking_opts(rung, &o.thinking);
        let mut call = Call::new(rung, SYSTEM_PROMPT, &rung_user, Schema { name: "Envelope", model: Id::Envelope }, o.max_retries);
        call.thinking_budget = thinking;
        call.reasoning_effort = effort;
        let mut env = match c.structured(&call) {
            Ok(env) => env,
            Err(CallError::Model(m)) => {
                last_class = err_class(c).to_string();
                last_msg = m;
                continue;
            }
            Err(CallError::Unported(why)) => return Err(GenErr::Unported(why)),
            Err(CallError::Os(e)) => return Err(GenErr::Other(e.to_string())),
        };
        let (ok, why) = self_consistent(&env);
        if !ok {
            last_class = "EnvelopeGenerationError".into();
            last_msg = format!("Model {} produced self-inconsistent envelope: {why}", repr(rung));
            continue;
        }
        o.adopt(&mut env)?;
        env.set("cache_key", key.as_str());
        if let Some(cache) = &o.cache {
            cache.put(&env, &key);
        }
        return Ok(env);
    }
    if o.single {
        return Err(py(&last_class, last_msg));
    }
    Err(py(
        "ModelLadderExhausted",
        format!("All models in ladder exhausted: {}. Last error ({last_class}): {last_msg}", repr_list(&o.models)),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::loads;
    use crate::pathways::verify::FORCE_Z3_UNKNOWN_TASK;

    fn env(task: &str, perms: &str) -> Object {
        match loads(&format!(r#"{{"id":"env_1","generated_by":"g","task":"{task}","permissions":{perms}}}"#)) {
            Ok(Value::Obj(o)) => o,
            _ => panic!(),
        }
    }

    /// K1-2: a self-consistency check that did not finish is not a pass.
    #[test]
    fn an_unfinished_check_is_not_consistent() {
        assert_eq!(self_consistent(&env("t", "{}")), (true, String::new()));
        assert_eq!(self_consistent(&env("t", r#"{"shell_allowlist":["ls"]}"#)).1, "Envelope is internally inconsistent");
        let (ok, why) = self_consistent(&env(FORCE_Z3_UNKNOWN_TASK, "{}"));
        assert!(!ok);
        assert_eq!(why, "Z3 self-consistency check exceeded 500ms");
    }

    #[test]
    fn thinking_kwargs() {
        assert_eq!(thinking_opts("anthropic/x", "deep"), (16000, ""));
        assert_eq!(thinking_opts("Anthropic/Claude-X", "deep"), (16000, ""));
        assert_eq!(thinking_opts("claude3", "deep"), (0, ""));
        assert_eq!(thinking_opts("openai/o3-mini", "light"), (0, "low"));
        assert_eq!(thinking_opts("o4", "deep"), (0, "high"));
        assert_eq!(thinking_opts("openai/gemini-2.5-pro", "deep"), (0, ""));
    }

    #[test]
    fn hints_rank_by_newest_and_keep_ten() {
        let rec = |ts: f64, msgs: &[(&str, &str)]| {
            let vs: Vec<String> = msgs.iter().map(|(s, m)| format!(r#"{{"stage":"{s}","message":"{m}"}}"#)).collect();
            obj(&loads(&format!(r#"{{"timestamp":{ts},"violations":[{}]}}"#, vs.join(","))).unwrap())
        };
        let h = hints_block(&[rec(7000.0, &[("b", "second"), ("a", "first")]), rec(7000.0, &[("c", "second"), ("a", "third")])]);
        assert!(h.ends_with("- [b] second\n- [a] first\n- [a] third"), "{h}");
    }
}
