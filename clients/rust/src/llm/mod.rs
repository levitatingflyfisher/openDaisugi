//! The oracle's model call for structured output (`opendaisugi/llm.py`
//! with instructor, and `claude_code_llm.py`): one call that asks a model
//! for a reply of a given schema and validates it, with the oracle's two
//! backends.
//!
//! - claude-code: the local `claude -p --output-format json`, the prompt on
//!   stdin, in a neutral working directory, re-asked with the error on a
//!   reply that does not parse or validate.
//! - litellm: the Anthropic Messages API over HTTPS, the request bytes
//!   litellm sends for an anthropic/ model under instructor's JSON mode,
//!   re-asked as instructor re-asks.
//!
//! Every failure comes back as a `CallError::Model` whose text is what
//! `llm.translate_llm_error` makes of the oracle's exception. What the
//! oracle prints on the way (instructor's log lines on stderr, litellm's
//! banner on stdout) is kept in `stdout` and `stderr` for the command to
//! print in order. The API key is never printed. The Go client's
//! `internal/llm` is the reference.

mod http;
mod schemas_gen;

use std::collections::HashMap;
use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use crate::gate::py::text::{decode_utf8_replace, repr, strip};
use crate::gate::pyjson::{dumps, loads_py, py_repr, Object, Value};
use crate::pathways::pmodel::{self, model, non_finite, Id, Loc, Mode, ValidationError};

/// Why a call failed.
#[derive(Debug)]
pub enum CallError {
    /// The model call failed, worded as `translate_llm_error` words it.
    Model(String),
    /// Something outside the call failed (the working directory could
    /// not be made).
    Os(std::io::Error),
}

/// The one line the binary refuses a call it does not make with.
pub const UNSUPPORTED: &str = "only the claude-code backend and anthropic/ models on the litellm backend are in this binary";

/// The world a call runs in.
pub struct Client {
    env: HashMap<String, String>,
    home: String,
    /// The neutral working directory, made once per process.
    neutral_cwd: Option<String>,
    /// ClaudeCodeInstructorClient's timeout_s.
    pub claude_timeout: Duration,
    /// What the call printed on stdout and on stderr, for the command to
    /// print.
    pub stdout: String,
    pub stderr: String,
}

/// A response model: its name, as instructor and the schema texts name
/// it, and the model a reply is validated against.
#[derive(Clone, Copy)]
pub struct Schema {
    pub name: &'static str,
    pub model: Id,
}

/// One `chat.completions.create(model, response_model, messages,
/// max_retries)`.
pub struct Call<'a> {
    pub model: &'a str,
    pub system: &'a str,
    pub user: &'a str,
    pub response: Schema,
    pub max_retries: usize,
}

const SCHEMA_PREAMBLE: &str =
    "Respond with ONLY a JSON object that validates against the following schema.\nNo prose, no code fences, no explanation.\n\n";

/// `claude_code_llm._MAX_REASKS`.
const MAX_REASKS: usize = 3;

/// What litellm prints to stdout when a call raises.
const BANNER: &str = "\n\x1b[1;31mGive Feedback / Get Help: https://github.com/BerriAI/litellm/issues/new\x1b[0m\nLiteLLM.Info: If you need to debug this error, use `litellm._turn_on_debug()'.\n\n";

/// Python's `s[:n]` of a str: the first n code points.
fn head(s: &str, n: usize) -> &str {
    crate::gate::py::text::head(s, n)
}

fn model_err(msg: impl Into<String>) -> CallError {
    CallError::Model(msg.into())
}

impl Client {
    pub fn new(env: HashMap<String, String>, home: &str) -> Client {
        Client {
            env,
            home: home.to_string(),
            neutral_cwd: None,
            claude_timeout: Duration::from_secs(120),
            stdout: String::new(),
            stderr: String::new(),
        }
    }

    fn getenv(&self, k: &str) -> &str {
        self.env.get(k).map(|s| s.as_str()).unwrap_or("")
    }

    fn which_claude(&self) -> Option<std::path::PathBuf> {
        crate::gate::dispatch::py_which("claude", Some(self.env.get("PATH").map(|s| s.as_str()).unwrap_or("")))
    }

    /// `resolve_backend(None)`: OPENDAISUGI_LLM_BACKEND, then the
    /// llm_backend of ~/.opendaisugi/config.yaml, then auto-detection.
    pub fn backend(&self) -> String {
        let v = self.getenv("OPENDAISUGI_LLM_BACKEND");
        if !v.is_empty() {
            return v.to_string();
        }
        if let Ok(cfg) = crate::cli::config::load(&format!("{}/.opendaisugi/config.yaml", self.home)) {
            if let Some(b) = cfg.llm_backend {
                let s = strip(&b);
                if !s.is_empty() {
                    return s.to_string();
                }
            }
        }
        if !self.getenv("ANTHROPIC_API_KEY").is_empty() || !self.getenv("ANTHROPIC_AUTH_TOKEN").is_empty() {
            return "litellm".into();
        }
        if self.which_claude().is_some() {
            return "claude-code".into();
        }
        "litellm".into()
    }

    /// Refuses, before anything is written, a call this binary does not
    /// make: the litellm backend (or any name but claude-code, which the
    /// oracle sends to litellm) with a model that is not
    /// anthropic/<name>.
    pub fn check(&self, model: &str) -> Result<(), String> {
        if self.backend() == "claude-code" || model.starts_with("anthropic/") {
            return Ok(());
        }
        Err(format!("{UNSUPPORTED}: model {}", repr(model)))
    }

    /// `llm.preflight`: a missing key or binary is one plain error.
    fn preflight(&self, backend: &str, model: &str) -> Result<(), CallError> {
        match backend {
            "litellm" => {
                let anthropic = model.starts_with("anthropic/") || model.starts_with("claude");
                if anthropic && self.getenv("ANTHROPIC_API_KEY").is_empty() && self.getenv("ANTHROPIC_AUTH_TOKEN").is_empty() {
                    return Err(model_err(
                        "Tried to call the Anthropic API through litellm.\nNo ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n\
                         Set a key, or run with --llm claude-code to use the local claude CLI.",
                    ));
                }
            }
            "claude-code" => {
                if self.which_claude().is_none() {
                    return Err(model_err(
                        "Tried to use the claude-code backend.\nNo `claude` command is on PATH.\n\
                         Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm litellm.",
                    ));
                }
            }
            _ => {}
        }
        Ok(())
    }

    /// Makes the call and returns the validated reply as a `model_dump()`
    /// value.
    pub fn structured(&mut self, call: &Call) -> Result<Object, CallError> {
        let backend = self.backend();
        self.preflight(&backend, call.model)?;
        if backend == "claude-code" {
            self.claude(call)
        } else {
            self.litellm(call)
        }
    }

    // -----------------------------------------------------------------
    // claude-code
    // -----------------------------------------------------------------

    fn claude_args(&self) -> Vec<String> {
        let mut args = vec!["-p".to_string(), "--model=haiku".to_string()];
        let raw = self.getenv("DAISUGI_CLAUDE_ARGS").trim();
        if !raw.is_empty() {
            // A text shlex cannot split is ignored; the oracle logs that on
            // the opendaisugi logger, which prints nothing by default.
            if let Ok(extra) = crate::interpreter_parse::shlex_split(raw) {
                args.extend(extra);
            }
        }
        args.push("--output-format".into());
        args.push("json".into());
        args
    }

    /// `tempfile.mkdtemp(prefix="opendaisugi-claude-")`, once.
    fn cwd(&mut self) -> Result<String, CallError> {
        if let Some(d) = &self.neutral_cwd {
            return Ok(d.clone());
        }
        let base = match self.env.get("TMPDIR") {
            Some(t) if !t.is_empty() => t.clone(),
            _ => "/tmp".to_string(),
        };
        const NAMES: &[u8] = b"abcdefghijklmnopqrstuvwxyz0123456789_";
        for _ in 0..100 {
            let mut b = [0u8; 8];
            std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(&mut b)).map_err(CallError::Os)?;
            let suffix: String = b.iter().map(|x| NAMES[*x as usize % NAMES.len()] as char).collect();
            let d = format!("{base}/opendaisugi-claude-{suffix}");
            match std::fs::DirBuilder::new().mode_700().create(&d) {
                Ok(()) => {
                    self.neutral_cwd = Some(d.clone());
                    return Ok(d);
                }
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
                Err(e) => return Err(CallError::Os(e)),
            }
        }
        Err(CallError::Os(std::io::Error::from(std::io::ErrorKind::AlreadyExists)))
    }

    /// `call_claude_p_async`: stdout decoded and stripped, or the oracle's
    /// error for a failed run. A timeout is asyncio's TimeoutError.
    fn run_claude(&mut self, prompt: &str) -> Result<String, CallError> {
        let bin = match self.which_claude() {
            Some(b) => b,
            None => return Err(model_err("claude binary not found: 'claude'")),
        };
        let dir = self.cwd()?;
        let mut child = match Command::new(&bin)
            .args(self.claude_args())
            .current_dir(&dir)
            .env_clear()
            .envs(&self.env)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
        {
            Ok(c) => c,
            Err(_) => return Err(model_err("claude binary not found: 'claude'")),
        };
        let mut stdin = child.stdin.take();
        let input = prompt.as_bytes().to_vec();
        let writer = std::thread::spawn(move || {
            if let Some(s) = stdin.as_mut() {
                let _ = s.write_all(&input);
            }
            drop(stdin);
        });
        let mut out_pipe = child.stdout.take();
        let mut err_pipe = child.stderr.take();
        let out_reader = std::thread::spawn(move || {
            let mut b = vec![];
            if let Some(p) = out_pipe.as_mut() {
                let _ = p.read_to_end(&mut b);
            }
            b
        });
        let err_reader = std::thread::spawn(move || {
            let mut b = vec![];
            if let Some(p) = err_pipe.as_mut() {
                let _ = p.read_to_end(&mut b);
            }
            b
        });
        let started = Instant::now();
        let status = loop {
            match child.try_wait() {
                Ok(Some(s)) => break Some(s),
                Ok(None) if started.elapsed() >= self.claude_timeout => break None,
                Ok(None) => std::thread::sleep(Duration::from_millis(20)),
                Err(_) => break None,
            }
        };
        let status = match status {
            Some(s) => s,
            None => {
                // _terminate_and_reap: SIGTERM, two seconds, then SIGKILL.
                unsafe {
                    libc::kill(child.id() as i32, libc::SIGTERM);
                }
                let t = Instant::now();
                loop {
                    match child.try_wait() {
                        Ok(Some(_)) => break,
                        _ if t.elapsed() >= Duration::from_secs(2) => {
                            let _ = child.kill();
                            let _ = child.wait();
                            break;
                        }
                        _ => std::thread::sleep(Duration::from_millis(20)),
                    }
                }
                let _ = writer.join();
                return Err(model_err("TimeoutError"));
            }
        };
        let _ = writer.join();
        let out = out_reader.join().unwrap_or_default();
        let err = err_reader.join().unwrap_or_default();
        use std::os::unix::process::ExitStatusExt;
        let code = match (status.code(), status.signal()) {
            (Some(c), _) => c,
            (None, Some(sig)) => -sig,
            _ => -1,
        };
        if code != 0 {
            let e = &err[..err.len().min(500)];
            return Err(model_err(format!("claude -p exited {code}: {}", repr(&decode_utf8_replace(e)))));
        }
        Ok(strip(&decode_utf8_replace(&out)).to_string())
    }

    fn claude(&mut self, call: &Call) -> Result<Object, CallError> {
        let prompt = format!("[system]\n{}\n\n[user]\n{}", call.system, call.user);
        let augmented = format!(
            "{SCHEMA_PREAMBLE}<json_schema>\n{}\n</json_schema>\n\n{prompt}",
            schemas_gen::claude_schema(call.response.name)
        );
        let reasks = call.max_retries.min(MAX_REASKS);
        let mut attempt = augmented.clone();
        for i in 0..=reasks {
            let stdout = self.run_claude(&attempt)?;
            let text = result_text(&stdout)?;
            match parse_structured(&text, call.response) {
                Ok(o) => return Ok(o),
                Err(CallError::Model(msg)) => {
                    if i == reasks {
                        return Err(CallError::Model(msg));
                    }
                    attempt = format!(
                        "{augmented}\n\nYour last reply could not be used: {}\nReply again with ONLY one JSON object that validates against the schema.",
                        head(&msg, 1000)
                    );
                }
                Err(e) => return Err(e),
            }
        }
        Err(model_err("unreachable"))
    }

    // -----------------------------------------------------------------
    // litellm
    // -----------------------------------------------------------------

    fn max_tokens(&self, name: &str) -> i64 {
        if let Some((_, n)) = schemas_gen::ANTHROPIC_MAX_TOKENS.iter().find(|(k, _)| *k == name) {
            return *n;
        }
        if let Some(v) = self.env.get("DEFAULT_ANTHROPIC_CHAT_MAX_TOKENS") {
            if let Ok(n) = v.trim().parse::<i64>() {
                return n;
            }
        }
        schemas_gen::DEFAULT_MAX_TOKENS
    }

    fn timeout(&self) -> f64 {
        if let Some(v) = self.env.get("REQUEST_TIMEOUT") {
            if let Some(f) = crate::gate::argparse::py_float(v) {
                return f;
            }
        }
        6000.0
    }

    /// One request: the reply's text and whether it was cut at
    /// max_tokens, or the litellm error text.
    fn post(&self, payload: &[u8]) -> Result<(String, bool), String> {
        let mut base = self.getenv("ANTHROPIC_API_BASE");
        if base.is_empty() {
            base = self.getenv("ANTHROPIC_BASE_URL");
        }
        if base.is_empty() {
            base = "https://api.anthropic.com";
        }
        let mut url = base.to_string();
        if !url.ends_with("/v1/messages") {
            url.push_str("/v1/messages");
        }
        let timeout = self.timeout();
        let mut headers =
            vec![("anthropic-version", "2023-06-01".to_string()), ("accept", "application/json".into()), ("content-type", "application/json".into())];
        let key = self.getenv("ANTHROPIC_API_KEY");
        let token = self.getenv("ANTHROPIC_AUTH_TOKEN");
        if !key.is_empty() {
            headers.push(("x-api-key", key.to_string()));
        } else if !token.is_empty() {
            headers.push(("authorization", format!("Bearer {token}")));
        }
        let started = Instant::now();
        let (status, raw) = match http::post(&url, &headers, payload, timeout) {
            Ok(r) => r,
            Err(http::Fail::Timeout) => {
                let took = crate::gate::pyjson::round(started.elapsed().as_secs_f64(), 3);
                return Err(format!(
                    "litellm.Timeout: AnthropicException - litellm.Timeout: Connection timed out. Timeout passed={}, time taken={} seconds",
                    crate::gate::pyjson::float_repr(timeout),
                    crate::gate::pyjson::float_repr(took)
                ));
            }
            // The key never reaches this text: the error names the URL only.
            Err(http::Fail::Other(why)) => return Err(format!("litellm.APIConnectionError: AnthropicException - {why}")),
        };
        if status != 200 {
            return Err(api_error(status, &raw));
        }
        let o = match loads_py(&raw, 900) {
            Ok(Value::Obj(o)) => o,
            _ => return Err(format!("litellm.APIError: AnthropicException - {raw}")),
        };
        let mut parts = vec![];
        if let Value::List(blocks) = o.value("content") {
            for bl in blocks {
                if let Value::Obj(b) = bl {
                    if b.value("type") == &Value::Str("text".into()) {
                        if let Value::Str(t) = b.value("text") {
                            parts.push(t.clone());
                        }
                    }
                }
            }
        }
        Ok((parts.join(""), o.value("stop_reason") == &Value::Str("max_tokens".into())))
    }

    fn litellm(&mut self, call: &Call) -> Result<Object, CallError> {
        let name = call.model.strip_prefix("anthropic/").unwrap_or(call.model);
        let system = format!("{}\n\n{}", call.system, schemas_gen::instructor_system(call.response.name));
        let mut msgs: Vec<(&str, String)> = vec![("user", call.user.to_string())];
        let attempts = call.max_retries + 1;
        let mut first: Option<ValidationError> = None;
        let mut failed = 0;
        for n in 1..=attempts {
            let payload = body(name, &system, &msgs, self.max_tokens(name));
            let (text, cut) = match self.post(payload.as_bytes()) {
                Ok(r) => r,
                Err(e) => {
                    self.stdout.push_str(BANNER);
                    self.stderr.push_str(&format!("API call failed on attempt {n}: {e}\n"));
                    self.stderr.push_str(&format!("Max retries exceeded. Total attempts: {n}, Last error: {e}\n"));
                    return Err(model_err(match &first {
                        Some(f) => flatten(f, failed),
                        None => e,
                    }));
                }
            };
            if cut {
                return Err(model_err("The output is incomplete due to a max_tokens length limit."));
            }
            let verr = match validate_reply_json(call.response, &text) {
                Ok(o) => return Ok(o),
                Err(e) => e,
            };
            failed += 1;
            let shown = verr.text();
            if first.is_none() {
                first = Some(verr);
            }
            if n == attempts {
                self.stderr.push_str(&format!("Max retries exceeded. Total attempts: {n}, Last error: {shown}\n"));
                return Err(model_err(flatten(first.as_ref().expect("set above"), failed)));
            }
            msgs.push(("assistant", text));
            msgs.push(("user", format!("Correct your JSON ONLY RESPONSE, based on the following errors:\n{shown}")));
        }
        Err(model_err("unreachable"))
    }
}

/// A DirBuilder that makes the directory with mode 0700, as mkdtemp does.
trait Mode700 {
    fn mode_700(&mut self) -> &mut Self;
}

impl Mode700 for std::fs::DirBuilder {
    fn mode_700(&mut self) -> &mut Self {
        use std::os::unix::fs::DirBuilderExt;
        self.mode(0o700)
    }
}

/// `claude_code_llm._result_text`.
fn result_text(stdout: &str) -> Result<String, CallError> {
    let not_envelope = || model_err(format!("claude -p stdout was not its JSON envelope: {}", repr(head(stdout, 200))));
    let o = match loads_py(stdout, 900) {
        Ok(Value::Obj(o)) => o,
        _ => return Err(not_envelope()),
    };
    if o.get("type") != Some(&Value::Str("result".into())) {
        return Err(not_envelope());
    }
    if o.get("is_error").is_some_and(|e| e.truthy()) {
        let r = match o.get("result") {
            Some(Value::Str(s)) => s.clone(),
            Some(v) => py_repr(v).unwrap_or_default(),
            None => "None".into(),
        };
        return Err(model_err(format!("claude -p reported is_error: {}", repr(head(&r, 200)))));
    }
    match o.get("result") {
        Some(Value::Str(s)) => Ok(s.clone()),
        _ => Err(not_envelope()),
    }
}

/// `models.non_finite_error` on a validated reply: each number that is not
/// finite is a `finite_number` error at its place.
fn finite(s: Schema, out: &Value) -> Result<(), ValidationError> {
    let mut errs = vec![];
    non_finite(out, &Loc::Root, &mut errs);
    if errs.is_empty() {
        Ok(())
    } else {
        Err(ValidationError { title: s.name.into(), errs })
    }
}

fn validated(s: Schema, v: Value, mode: Mode) -> Result<Object, ValidationError> {
    let m = model(s.model);
    let (out, errs) = m.take(v, &Loc::Root, mode);
    if !errs.is_empty() {
        return Err(ValidationError { title: s.name.into(), errs });
    }
    finite(s, &out)?;
    match out {
        Value::Obj(o) => Ok(o),
        _ => Err(ValidationError { title: s.name.into(), errs: vec![] }),
    }
}

/// `model_validate_json(text)` of a reply, with the finite check.
fn validate_reply_json(s: Schema, text: &str) -> Result<Object, ValidationError> {
    let v = pmodel::parse_json(s.name, text)?;
    validated(s, v, Mode::Json)
}

/// `_parse_structured`: the first "{" to the last "}", read as JSON or as
/// a Python dict literal, validated in Python mode.
fn parse_structured(text: &str, s: Schema) -> Result<Object, CallError> {
    let (start, end) = match (text.find('{'), text.rfind('}')) {
        (Some(a), Some(b)) if b > a => (a, b),
        _ => return Err(model_err(format!("no JSON object in claude -p stdout: {}", repr(head(text, 200))))),
    };
    let body = &text[start..=end];
    let payload = match loads_py(body, 900) {
        Ok(v) => v,
        Err(derr) => match pmodel::decode_dict_text(body) {
            Some(d) => Value::Obj(d),
            None => return Err(model_err(format!("claude -p stdout was not valid JSON: {derr}"))),
        },
    };
    validated(s, payload, Mode::Python).map_err(|e| model_err(format!("claude -p output failed {} validation: {}", s.name, e.text())))
}

/// The request litellm sends: compact JSON, non-ASCII kept.
fn body(model: &str, system: &str, msgs: &[(&str, String)], max_tokens: i64) -> String {
    let js = |s: &str| dumps(&Value::Str(s.to_string()), false);
    let mut b = format!("{{\"model\":{},\"messages\":[", js(model));
    for (i, (role, text)) in msgs.iter().enumerate() {
        if i > 0 {
            b.push(',');
        }
        b.push_str(&format!("{{\"role\":{},\"content\":[{{\"type\":\"text\",\"text\":{}}}]}}", js(role), js(text)));
    }
    b.push_str(&format!("],\"system\":[{{\"type\":\"text\",\"text\":{}}}],\"max_tokens\":{max_tokens}}}", js(system)));
    b
}

/// The exception litellm raises for an HTTP status, worded as its str().
fn api_error(status: u16, text: &str) -> String {
    let (name, suffix) = match status {
        401 => ("AuthenticationError", ""),
        403 => ("PermissionDeniedError", ""),
        404 => ("NotFoundError", ""),
        408 => ("Timeout", ""),
        429 => ("RateLimitError", ""),
        502 => return format!("litellm.BadGatewayError: AnthropicException BadGatewayError - {text}"),
        503 => ("ServiceUnavailableError", ". Handle with `litellm.ServiceUnavailableError`."),
        s if s >= 500 => ("InternalServerError", ". Handle with `litellm.InternalServerError`."),
        _ => ("BadRequestError", ""),
    };
    format!("litellm.{name}: AnthropicException - {text}{suffix}")
}

/// `llm._flatten_retry`: the first attempt's cause and the count.
fn flatten(first: &ValidationError, n: usize) -> String {
    let text = first.text();
    let line = text.trim().split('\n').map(|l| l.trim()).find(|l| !l.is_empty()).unwrap_or("").to_string();
    format!("ValidationError: {line} ({n} attempts)")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_request_body_is_litellms() {
        let b = body("claude-x", "sys é", &[("user", "hi \"there\"".into())], 4096);
        assert_eq!(
            b,
            r#"{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"hi \"there\""}]}],"system":[{"type":"text","text":"sys é"}],"max_tokens":4096}"#
        );
    }

    #[test]
    fn a_reply_with_nan_is_schema_invalid() {
        let s = Schema { name: "Envelope", model: Id::Envelope };
        let e = validate_reply_json(s, r#"{"generated_by":"g","task":"t","permissions":{"velocity_limit":NaN}}"#).unwrap_err();
        assert!(e.text().contains("finite_number"), "{}", e.text());
        let e = parse_structured(r#"x {"generated_by":"g","task":"t","permissions":{"velocity_limit":NaN}} y"#, s).unwrap_err();
        match e {
            CallError::Model(m) => assert!(m.starts_with("claude -p output failed Envelope validation: 1 validation error for Envelope"), "{m}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn the_envelope_of_claude_is_read() {
        assert_eq!(result_text(r#"{"type":"result","is_error":false,"result":"ok"}"#).unwrap(), "ok");
        match result_text(r#"{"type":"result","is_error":true,"result":"Credit"}"#) {
            Err(CallError::Model(m)) => assert_eq!(m, "claude -p reported is_error: 'Credit'"),
            other => panic!("{other:?}"),
        }
        match result_text("not json") {
            Err(CallError::Model(m)) => assert_eq!(m, "claude -p stdout was not its JSON envelope: 'not json'"),
            other => panic!("{other:?}"),
        }
    }
}
