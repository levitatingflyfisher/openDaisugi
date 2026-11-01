//! `voice/cleanup.py`: the optional fixed-prompt local-model pass,
//! journaled like any other turn.

use std::io::Write;

use num_bigint::BigInt;

use crate::warden::py::text;
use crate::warden::pyjson::{Object, Value};
use crate::router::meter::{measure, record_turn, Price, Prices};
use crate::router::route::{estimate_difficulty, Decision};
use crate::llm::{wire, Client};

pub const CLEANUP_PROMPT: &str =
    "Fix punctuation and obvious transcription errors. Keep the meaning and the words. Return only the corrected text.";
const NO_TEXT: &str = "The cleanup model returned no text. Check the model, then try again.";
const UNREACHABLE: &str = "The cleanup model failed to respond. Check that it is running, then try again.";
/// The reply of a cleanup model that cut the text: a small model context
/// drops the end of a long transcript.
const SHORT: &str = "The cleanup model returned much less text than it was given. The raw text is kept.";
/// How much of its source a cleaned chunk must keep, in percent.
pub const MIN_CLEANED_PERCENT: usize = 80;
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
    if cleaned.chars().count() * 100 < text_.chars().count() * MIN_CLEANED_PERCENT {
        return Ok(CleanupResult { text: text_.into(), cleaned: false, reason: Some(SHORT.into()) });
    }
    let line = turn_line(&model, text_, &input, &output, now_secs)?;
    if append_line(&format!("{}/router/turns.jsonl", cfg.data_dir), &line).is_err() {
        return Ok(CleanupResult { text: cleaned, cleaned: false, reason: Some(UNWRITABLE.into()) });
    }
    Ok(CleanupResult { text: cleaned, cleaned: true, reason: None })
}

/// Cleans a transcript window by window, so no one call is longer than a
/// small model can hold, and joins the results. A window that fails to clean
/// keeps its raw text; the others keep their cleaned text. A clip of one
/// window is cleaned whole. `cleaned` is true when any window was cleaned;
/// the reason is the first reason any window gave.
pub fn clean_parts(t: &super::engine::Transcript, cfg: &CleanupConfig, tr: &mut dyn Transport, now_secs: i64) -> Result<CleanupResult, String> {
    let begin = std::time::Instant::now();
    clean_parts_budget(t, cfg, tr, now_secs, &|| begin.elapsed().as_secs_f64(), CLEANUP_BUDGET_S)
}

/// All the time the cleanup calls of one clip share. A model that hangs
/// would otherwise cost its own timeout on every window of a long clip. Past
/// the budget, or after the model failed to answer once, the windows left
/// keep their raw text and the model is not called again.
pub const CLEANUP_BUDGET_S: f64 = 120.0;
const BUDGET: &str = "The cleanup took too long, so the rest of the text is raw. Check that the cleanup model is fast enough, then try again.";

/// `clean_parts` with its clock (seconds) and budget given.
pub fn clean_parts_budget(
    t: &super::engine::Transcript,
    cfg: &CleanupConfig,
    tr: &mut dyn Transport,
    now_secs: i64,
    clock: &dyn Fn() -> f64,
    budget_s: f64,
) -> Result<CleanupResult, String> {
    if t.parts.len() <= 1 {
        return clean_transcript(&t.text, cfg, tr, now_secs);
    }
    let mut out: Vec<String> = Vec::new();
    let mut cleaned = false;
    let mut reason: Option<String> = None;
    let started = clock();
    let mut stopped = false;
    for part in &t.parts {
        if text::strip(part).is_empty() || part == super::engine::GAP_MARKER {
            out.push(part.clone());
            continue;
        }
        if !stopped && clock() - started > budget_s {
            stopped = true;
            if reason.is_none() {
                reason = Some(BUDGET.into());
            }
        }
        if stopped {
            out.push(part.clone());
            continue;
        }
        let res = clean_transcript(part, cfg, tr, now_secs)?;
        out.push(res.text);
        cleaned = cleaned || res.cleaned;
        if res.reason.as_deref() == Some(UNREACHABLE) {
            stopped = true;
        }
        if reason.is_none() {
            reason = res.reason;
        }
    }
    Ok(CleanupResult { text: super::engine::join_texts(&out), cleaned, reason })
}

#[cfg(test)]
mod chunk_tests {
    use super::*;
    use super::super::engine::{Transcript, GAP_MARKER};

    struct Cut;
    impl Transport for Cut {
        fn complete(&mut self, _m: &str, _s: &str, user: &str) -> Result<(String, BigInt, BigInt), String> {
            Ok((user[..user.len() / 2].to_string(), BigInt::from(1), BigInt::from(1)))
        }
    }
    struct Tidy(Vec<String>);
    impl Transport for Tidy {
        fn complete(&mut self, _m: &str, _s: &str, user: &str) -> Result<(String, BigInt, BigInt), String> {
            self.0.push(user.to_string());
            let mut c = user.chars();
            let first = c.next().unwrap().to_uppercase().to_string();
            Ok((format!("{first}{}.", c.as_str()), BigInt::from(1), BigInt::from(1)))
        }
    }
    fn cfg() -> CleanupConfig {
        let dir = std::env::temp_dir().join(format!("clean-chunk-{}", std::process::id()));
        CleanupConfig { on: true, model: Some("m".into()), base_url: None, data_dir: dir.display().to_string() }
    }
    fn t(parts: &[&str]) -> Transcript {
        let parts: Vec<String> = parts.iter().map(|s| s.to_string()).collect();
        Transcript { text: parts.join(" "), duration_s: 60.0, rtf: 0.1, parts, gaps: 0 }
    }

    #[test]
    fn a_cut_cleanup_keeps_the_raw_text_of_that_window() {
        let res = clean_parts(&t(&["window one", "window two"]), &cfg(), &mut Cut, 0).unwrap();
        assert_eq!(res.text, "window one window two");
        assert!(!res.cleaned);
        assert!(res.reason.is_some());
    }

    #[test]
    fn cleanup_runs_per_window_and_skips_gaps() {
        let mut tidy = Tidy(vec![]);
        let res = clean_parts(&t(&["alpha", "beta", GAP_MARKER]), &cfg(), &mut tidy, 0).unwrap();
        assert_eq!(res.text, format!("Alpha. Beta. {GAP_MARKER}"));
        assert!(res.cleaned);
        assert_eq!(tidy.0, vec!["alpha", "beta"]);
    }

    struct Dead(usize);
    impl Transport for Dead {
        fn complete(&mut self, _m: &str, _s: &str, _u: &str) -> Result<(String, BigInt, BigInt), String> {
            self.0 += 1;
            Err("no route".into())
        }
    }

    #[test]
    fn cleanup_stops_calling_a_model_that_did_not_answer() {
        let mut dead = Dead(0);
        let res = clean_parts(&t(&["w0", "w1", "w2", "w3"]), &cfg(), &mut dead, 0).unwrap();
        assert_eq!(dead.0, 1);
        assert_eq!(res.text, "w0 w1 w2 w3");
        assert!(!res.cleaned);
        assert!(res.reason.unwrap().contains("failed to respond"));
    }

    struct Slow<'a>(&'a std::cell::Cell<f64>, usize);
    impl Transport for Slow<'_> {
        fn complete(&mut self, _m: &str, _s: &str, user: &str) -> Result<(String, BigInt, BigInt), String> {
            self.1 += 1;
            self.0.set(self.0.get() + 50.0);
            Ok((format!("{}.", user.to_uppercase()), BigInt::from(1), BigInt::from(1)))
        }
    }

    #[test]
    fn cleanup_past_its_time_budget_keeps_the_raw_text_of_the_rest() {
        let now = std::cell::Cell::new(1000.0);
        let mut slow = Slow(&now, 0);
        let parts = ["w0", "w1", "w2", "w3", "w4", "w5"];
        let res = clean_parts_budget(&t(&parts), &cfg(), &mut slow, 0, &|| now.get(), 120.0).unwrap();
        assert_eq!(slow.1, 3);
        assert_eq!(res.text, "W0. W1. W2. w3 w4 w5");
        assert!(res.cleaned);
        assert!(res.reason.unwrap().contains("took too long"));
    }
}
