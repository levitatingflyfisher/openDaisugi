//! `voice/cleanup.py`: the optional fixed-prompt local-model pass,
//! journaled like any other turn.

use std::io::Write;

use num_bigint::BigInt;

use crate::gate::py::text;
use crate::gate::pyjson::{Object, Value};
use crate::gateway::meter::{measure, record_turn, Price, Prices};
use crate::gateway::route::{estimate_difficulty, Decision};
use crate::llm::{wire, Client};

pub const CLEANUP_PROMPT: &str =
    "Fix punctuation and obvious transcription errors. Keep the meaning and the words. Return only the corrected text.";
const NO_TEXT: &str = "The cleanup model returned no text. Check the model, then try again.";
const UNREACHABLE: &str = "The cleanup model failed to respond. Check that it is running, then try again.";
const UNWRITABLE: &str = "The transcript could not be journaled. Check the data directory, then try again.";
const ROUTE_REASON: &str = "voice cleanup runs on a fixed local model. It is never routed.";

#[derive(Clone)]
pub struct CleanupConfig {
    pub on: bool,
    pub model: Option<String>,
    pub base_url: Option<String>,
    pub data_dir: String,
}

pub struct CleanupResult {
    pub text: String,
    pub cleaned: bool,
    pub reason: Option<String>,
}

/// One blocking completion call: the corrected text and the input and
/// output token counts, or a failure.
pub trait Transport {
    fn complete(&mut self, model: &str, system: &str, user: &str) -> Result<(String, BigInt, BigInt), String>;
}

/// `HTTPCleanupTransport` over the llm client.
pub struct HttpTransport<'a> {
    pub client: &'a Client,
    pub base_url: Option<String>,
    pub timeout: f64,
}

impl Transport for HttpTransport<'_> {
    fn complete(&mut self, model: &str, system: &str, user: &str) -> Result<(String, BigInt, BigInt), String> {
        let mut call = model.to_string();
        let mut base = String::new();
        if let Some(b) = &self.base_url {
            base = b.clone();
            if !call.contains('/') {
                call = format!("openai/{call}");
            }
        }
        let msgs = vec![("system".to_string(), system.to_string()), ("user".to_string(), user.to_string())];
        let reply = self
            .client
            .complete_at(&call, &base, &msgs, &wire::Opts::default(), self.timeout)
            .map_err(|e| format!("{e:?}"))?;
        Ok((reply.text, BigInt::from(reply.input_tokens.unwrap_or(0)), BigInt::from(reply.output_tokens.unwrap_or(0))))
    }
}

/// The journal line of `_journal_cleanup`.
fn turn_line(model: &str, text_: &str, input: &BigInt, output: &BigInt, now_secs: i64) -> Result<String, String> {
    let d = Decision {
        tier: "tier1-local".into(),
        model: Value::Str(model.into()),
        requested_model: Value::Str(model.into()),
        difficulty: estimate_difficulty(text_),
        downgraded: false,
        reason: ROUTE_REASON.into(),
    };
    let mut usage = Object::new();
    usage.set("input_tokens", Value::Int(input.to_string()));
    usage.set("output_tokens", Value::Int(output.to_string()));
    let mut prices = Prices::new();
    prices.insert(model.to_string(), Price { input: 0.0, output: 0.0 });
    let s = measure(&prices, &d, &Value::Obj(usage)).map_err(|_| "the usage did not measure".to_string())?;
    let r = record_turn(&d, &s, text_, text_, now_secs).map_err(|_| "the record did not build".to_string())?;
    Ok(r.json())
}

fn append_line(path: &str, line: &str) -> std::io::Result<()> {
    if let Some(parent) = std::path::Path::new(path).parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut f = std::fs::OpenOptions::new().append(true).create(true).open(path)?;
    f.write_all(format!("{line}\n").as_bytes())
}

/// `cleanup.clean_transcript`. An error that is not a file error is Err,
/// which the server answers 500, as the oracle would.
pub fn clean_transcript(text_: &str, cfg: &CleanupConfig, tr: &mut dyn Transport, now_secs: i64) -> Result<CleanupResult, String> {
    let model = match &cfg.model {
        Some(m) if cfg.on && !m.is_empty() => m.clone(),
        _ => return Ok(CleanupResult { text: text_.into(), cleaned: false, reason: None }),
    };
    let (corrected, input, output) = match tr.complete(&model, CLEANUP_PROMPT, text_) {
        Ok(x) => x,
        Err(_) => return Ok(CleanupResult { text: text_.into(), cleaned: false, reason: Some(UNREACHABLE.into()) }),
    };
    let cleaned = text::strip(&corrected).to_string();
    if cleaned.is_empty() {
        return Ok(CleanupResult { text: text_.into(), cleaned: false, reason: Some(NO_TEXT.into()) });
    }
    let line = turn_line(&model, text_, &input, &output, now_secs)?;
    if append_line(&format!("{}/gateway/turns.jsonl", cfg.data_dir), &line).is_err() {
        return Ok(CleanupResult { text: cleaned, cleaned: false, reason: Some(UNWRITABLE.into()) });
    }
    Ok(CleanupResult { text: cleaned, cleaned: true, reason: None })
}
