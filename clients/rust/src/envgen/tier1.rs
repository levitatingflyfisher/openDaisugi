//! `tier1.HTTPTier1Provider` and `local_setup.load_configured_tier1`: one
//! structured call to any model the client reaches, a local
//! OpenAI-compatible endpoint included. Any failure declines, and the
//! generator falls through to Tier-2.

use std::time::{Duration, Instant};

use super::prompts_gen::TIER1_PROMPT;
use super::{GenErr, PyError};
use crate::gate::pyjson::{loads_py, py_type_name, Object, Value};
use crate::llm::{Call, Client, Schema};
use crate::pathways::pmodel::Id;

/// `local_setup._TIER1_CONFIG`.
pub const CONFIG_FILE: &str = "local_tier1.json";

/// `HTTPTier1Provider`.
#[derive(Debug, Clone)]
pub struct Tier1 {
    pub model: String,
    pub base_url: Option<String>,
    pub api_key: Option<String>,
    /// The provider's name, part of the Tier-1 cache key.
    pub name: String,
    /// The whole call's limit (`asyncio.wait_for`).
    pub timeout: Duration,
    /// A base_url the config file gives as something other than a
    /// string: every call with it fails in the oracle, so it declines.
    bad_base: bool,
}

impl Tier1 {
    /// `HTTPTier1Provider(model, base_url=..., api_key=..., name=...)`. An
    /// unprefixed model with a base_url is an OpenAI-compatible one.
    pub fn new(model: &str, base_url: Option<String>, api_key: Option<String>, name: &str) -> Tier1 {
        let model = if base_url.is_some() && !model.contains('/') { format!("openai/{model}") } else { model.to_string() };
        let name = if name.is_empty() { format!("http:{model}") } else { name.to_string() };
        Tier1 { model, base_url, api_key, name, timeout: Duration::from_secs(30), bad_base: false }
    }

    /// `HTTPTier1Provider.generate_envelope`: the envelope, or None when
    /// the provider declines.
    pub fn generate(&self, c: &mut Client, task: &str, context: Option<&str>) -> Option<Object> {
        let mut user = format!("Task: {task}");
        if let Some(ctx) = context.filter(|c| !c.is_empty()) {
            user.push_str(&format!("\n\nContext:\n{ctx}"));
        }
        if c.preflight_model(&self.model).is_err() || self.bad_base {
            return None;
        }
        let mut call = Call::new(&self.model, TIER1_PROMPT, &user, Schema { name: "Envelope", model: Id::Envelope }, 1);
        call.deadline = Some(Instant::now() + self.timeout);
        call.base_url = self.base_url.as_deref().unwrap_or("");
        call.api_key = self.api_key.as_deref().unwrap_or("");
        c.structured(&call).ok()
    }
}

/// `load_configured_tier1(data_dir)`: the provider local_tier1.json
/// names, or None when there is none or it does not read.
pub fn load_configured(data_dir: &str) -> Result<Option<Tier1>, GenErr> {
    let raw = match std::fs::read(format!("{}/{CONFIG_FILE}", data_dir.trim_end_matches('/'))) {
        Ok(b) => b,
        Err(_) => return Ok(None),
    };
    let Ok(text) = String::from_utf8(raw) else {
        // read_text raises UnicodeDecodeError, which the oracle does not catch.
        return Err(GenErr::Py(PyError::new("UnicodeDecodeError", "the Tier-1 config is not UTF-8")));
    };
    let Ok(v) = loads_py(&text, 900) else { return Ok(None) };
    let Value::Obj(cfg) = v else {
        return Err(GenErr::Py(PyError::new("AttributeError", format!("'{}' object has no attribute 'get'", py_type_name(&v)))));
    };
    let m = cfg.value("model");
    if !m.truthy() {
        return Ok(None);
    }
    let (base, bad) = match cfg.value("base_url") {
        Value::Null => (None, false),
        Value::Str(b) => (Some(b.clone()), false),
        // "/" not in model still runs, and every call then fails.
        _ => (Some(String::new()), true),
    };
    let Value::Str(model) = m else {
        if base.is_some() {
            return Err(GenErr::Py(PyError::new("TypeError", format!("argument of type '{}' is not iterable", py_type_name(m)))));
        }
        // The oracle builds a provider named after the value, which then
        // always declines; this binary does not (K1-6).
        return Err(GenErr::Unported("a Tier-1 model that is not a string".into()));
    };
    let mut t = Tier1::new(model, base, None, "");
    t.bad_base = bad;
    Ok(Some(t))
}
