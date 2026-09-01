//! The oracle's model call for structured output
//! (`opendaisugi/llm_client.py`, and `claude_code_llm.py`): one call that
//! asks a model for a reply of a given schema and validates it, with the
//! oracle's backends.
//!
//! - claude-code: the local `claude -p --output-format json`, the prompt on
//!   stdin, in a neutral working directory, re-asked with the error on a
//!   reply that does not parse or validate.
//! - the HTTP backend (named api): the Anthropic Messages API or
//!   OpenAI-compatible chat completions, chosen by the model name, the
//!   request bytes `llm_client.py` sends, re-asked as it re-asks.
//!
//! Every failure comes back as a `CallError::Model` whose text is the
//! oracle's. Nothing is printed. The API key is never in an error. The Go
//! client's `internal/llm` is the reference.

pub mod http;
mod schemas_gen;
pub mod wire;

use std::collections::HashMap;
use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use crate::gate::py::text::{decode_utf8_replace, repr, strip};
use crate::gate::pyjson::{loads_py, py_repr, Object, Value};
use crate::pathways::pmodel::{self, model, non_finite, Id, Loc, Mode, ValidationError};

/// Why a call failed.
#[derive(Debug)]
pub enum CallError {
    /// The model call failed, worded as `translate_llm_error` words it.
    Model(String),
    /// Something outside the call failed (the working directory could
    /// not be made).
    Os(std::io::Error),
    /// A call this binary does not make the oracle's way (a proxy setting
    /// or a reply it does not read as httpx does).
    Unported(String),
}

/// The one line the binary refuses a call it does not make with.
pub const UNSUPPORTED: &str = "this model call is not in this binary";

/// `llm.RENAMED_BACKEND_TEXT`: the one line that refuses the old name of
/// the api backend.
pub const RENAMED_TEXT: &str = "The LLM backend 'litellm' is now named 'api'. Use 'api'.";

/// The old name of the api backend, as resolve_backend tests it: with
/// surrounding whitespace stripped.
pub fn renamed(backend: &str) -> bool {
    strip(backend) == "litellm"
}

/// The world a call runs in.
pub struct Client {
    env: HashMap<String, String>,
    /// The proxies httpx reads from `env` (LLM-11).
    proxies: crate::netproxy::Httpx,
    home: String,
    /// The neutral working directory, made once per process.
    neutral_cwd: Option<String>,
    /// ClaudeCodeInstructorClient's timeout_s.
    pub claude_timeout: Duration,
}

/// A response model: its name, as instructor and the schema texts name
/// it, and the model a reply is validated against.
#[derive(Clone, Copy)]
pub struct Schema {
    pub name: &'static str,
    pub model: Id,
}

/// One `chat.completions.create(model, response_model, messages,
/// max_retries)`. `thinking_budget` and `reasoning_effort` are the
/// thinking kwargs, `base_url` and `api_key` the base_url and api_key
/// arguments (empty: unset); the claude-code backend ignores all four, as
/// the oracle's does. A `deadline` bounds the whole call, re-asks
/// included, as asyncio.wait_for does around it: past it the call fails
/// with "TimeoutError".
pub struct Call<'a> {
    pub model: &'a str,
    pub system: &'a str,
    pub user: &'a str,
    pub response: Schema,
    pub max_retries: usize,
    pub thinking_budget: i64,
    pub reasoning_effort: &'a str,
    pub base_url: &'a str,
    pub api_key: &'a str,
    pub deadline: Option<Instant>,
    /// A call that passes no max_retries: the HTTP client's default is 3,
    /// the claude-code client's 0.
    pub default_retries: bool,
}

impl<'a> Call<'a> {
    /// A call with no thinking kwargs, base_url, api_key or deadline.
    pub fn new(model: &'a str, system: &'a str, user: &'a str, response: Schema, max_retries: usize) -> Call<'a> {
        Call {
            model,
            system,
            user,
            response,
            max_retries,
            thinking_budget: 0,
            reasoning_effort: "",
            base_url: "",
            api_key: "",
            deadline: None,
            default_retries: false,
        }
    }
}

/// The time left before a deadline, or the "TimeoutError" of a call past
/// it. No deadline leaves `limit` as it is.
fn left(deadline: Option<Instant>, limit: Duration) -> Result<Duration, CallError> {
    match deadline {
        None => Ok(limit),
        Some(d) => {
            let now = Instant::now();
            if now >= d {
                Err(model_err("TimeoutError"))
            } else {
                Ok(limit.min(d - now))
            }
        }
    }
}

const SCHEMA_PREAMBLE: &str =
    "Respond with ONLY a JSON object that validates against the following schema.\nNo prose, no code fences, no explanation.\n\n";

/// `claude_code_llm._MAX_REASKS`.
const MAX_REASKS: usize = 3;

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
            proxies: crate::netproxy::Httpx::from_vars(&crate::netproxy::map_vars(&env), false),
            env,
            home: home.to_string(),
            neutral_cwd: None,
            claude_timeout: Duration::from_secs(120),
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
    /// Where resolve_backend raises on the old name, this returns it; the
    /// caller checks `renamed`.
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
            return "api".into();
        }
        if self.which_claude().is_some() {
            return "claude-code".into();
        }
        "api".into()
    }

    /// Refuses, before anything is written, a call this binary does not
    /// make the oracle's way: on the HTTP backend, a proxy setting it does
    /// not read as httpx does.
    pub fn check(&self, _model: &str) -> Result<(), String> {
        // The old backend name fails in preflight, as the oracle fails.
        let b = self.backend();
        if b == "claude-code" || renamed(&b) {
            return Ok(());
        }
        if let Some(crate::netproxy::Refusal::Unported(why)) = self.proxies.refusal() {
            return Err(format!("{UNSUPPORTED}, under a proxy setting it reads as httpx does: {why}"));
        }
        Ok(())
    }

    /// `llm.preflight(None, model=model)` for the backend in effect: the
    /// old backend name, or a missing key or binary, as one error. The
    /// oracle raises it as LLMNotConfigured, which is not a failed call.
    pub fn preflight_model(&self, model: &str) -> Result<(), CallError> {
        self.preflight(&self.backend(), model)
    }

    /// `llm.preflight`: the old backend name, or a missing key or binary,
    /// is one plain error.
    fn preflight(&self, backend: &str, model: &str) -> Result<(), CallError> {
        if renamed(backend) {
            return Err(model_err(RENAMED_TEXT));
        }
        match backend {
            "api" => {
                let anthropic = model.starts_with("anthropic/") || model.starts_with("claude");
                if anthropic && self.getenv("ANTHROPIC_API_KEY").is_empty() && self.getenv("ANTHROPIC_AUTH_TOKEN").is_empty() {
                    return Err(model_err(
                        "Tried to call the Anthropic API.\nNo ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n\
                         Set a key, or run with --llm claude-code to use the local claude CLI.",
                    ));
                }
            }
            "claude-code" => {
                if self.which_claude().is_none() {
                    return Err(model_err(
                        "Tried to use the claude-code backend.\nNo `claude` command is on PATH.\n\
                         Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm api.",
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
            self.api(call)
        }
    }

    /// `llm_client.complete(model, messages, json_object=True)` on the
    /// HTTP backend: one request with the oracle's bytes and its reply's
    /// text. No preflight, no re-ask and no cut check, as the oracle makes
    /// none there.
    pub fn complete_json(&self, model: &str, msgs: &[(String, String)]) -> Result<String, CallError> {
        let get = |k: &str| self.env.get(k).cloned();
        let w = wire::resolve(model, "", "", &get).map_err(CallError::Model)?;
        let timeout = wire::timeout(&get);
        let payload = wire::body(&w, msgs, &wire::Opts { json_object: true, ..Default::default() });
        let (status, text) = wire::post(&w, payload.as_bytes(), timeout, &self.proxies).map_err(|e| match e {
            wire::PostErr::Model(m) => CallError::Model(m),
            wire::PostErr::Unported(why) => CallError::Unported(format!("{UNSUPPORTED}: {why}")),
        })?;
        Ok(wire::read_reply(&w, status, &text).map_err(CallError::Model)?.text)
    }

    /// `llm_client.complete(model, messages, max_tokens=...)` on the HTTP
    /// backend with an explicit timeout in seconds: one request and its
    /// reply, token count included. No preflight and no re-ask.
    pub fn complete_timeout(&self, model: &str, msgs: &[(String, String)], max_tokens: i64, timeout: f64) -> Result<wire::Reply, CallError> {
        let get = |k: &str| self.env.get(k).cloned();
        let w = wire::resolve(model, "", "", &get).map_err(CallError::Model)?;
        let payload = wire::body(&w, msgs, &wire::Opts { max_tokens: Some(max_tokens), ..Default::default() });
        let (status, text) = wire::post(&w, payload.as_bytes(), timeout, &self.proxies).map_err(|e| match e {
            wire::PostErr::Model(m) => CallError::Model(m),
            wire::PostErr::Unported(why) => CallError::Unported(format!("{UNSUPPORTED}: {why}")),
        })?;
        wire::read_reply(&w, status, &text).map_err(CallError::Model)
    }

    /// `llm_client.complete(model, messages, base_url=..., ...)` on the
    /// HTTP backend: one request and its reply, with a base URL ("" for
    /// none), the body options and a timeout in seconds.
    pub fn complete_at(
        &self,
        model: &str,
        base_url: &str,
        msgs: &[(String, String)],
        opts: &wire::Opts,
        timeout: f64,
    ) -> Result<wire::Reply, CallError> {
        let get = |k: &str| self.env.get(k).cloned();
        let w = wire::resolve(model, base_url, "", &get).map_err(CallError::Model)?;
        let payload = wire::body(&w, msgs, opts);
        let (status, text) = wire::post(&w, payload.as_bytes(), timeout, &self.proxies).map_err(|e| match e {
            wire::PostErr::Model(m) => CallError::Model(m),
            wire::PostErr::Unported(why) => CallError::Unported(format!("{UNSUPPORTED}: {why}")),
        })?;
        wire::read_reply(&w, status, &text).map_err(CallError::Model)
    }

    /// `call_claude_p_metered`: `claude -p --model=MODEL
    /// [DAISUGI_CLAUDE_ARGS] --output-format json` with the prompt on stdin,
    /// run as subprocess.run runs it (killed at the timeout), and the
    /// reply's text with Claude Code's own token count and cost (None when
    /// the reply does not say). A stdout that is not JSON is the text
    /// itself, with no meter.
    pub fn metered(&mut self, prompt: &str, model: &str, timeout_s: f64) -> Result<Metered, CallError> {
        let Some(bin) = self.which_claude() else {
            return Err(model_err("claude binary not found: 'claude'"));
        };
        let dir = self.cwd()?;
        let mut args = vec!["-p".to_string(), format!("--model={model}")];
        let raw = self.getenv("DAISUGI_CLAUDE_ARGS").trim();
        if !raw.is_empty() {
            if let Ok(extra) = crate::interpreter_parse::shlex_split(raw) {
                args.extend(extra);
            }
        }
        args.push("--output-format".into());
        args.push("json".into());
        let limit = Duration::from_secs_f64(timeout_s.clamp(0.0, 1e9));
        let (code, out, err) = match self.spawn(&bin, &args, &dir, prompt, limit, false)? {
            Some(r) => r,
            None => {
                return Err(model_err(format!(
                    "claude -p timed out after {}s",
                    crate::gate::pyjson::float_repr(timeout_s)
                )))
            }
        };
        if code != 0 {
            let e = &err[..err.len().min(500)];
            return Err(model_err(format!("claude -p exited {code}: {}", repr(&decode_utf8_replace(e)))));
        }
        let raw = strip(&decode_utf8_replace(&out)).to_string();
        let v = match loads_py(&raw, 900) {
            Ok(v) => v,
            Err(_) => return Ok(Metered { text: raw, tokens: None, cost: None }),
        };
        let o = match v {
            Value::Obj(o) => o,
            other => {
                return Err(model_err(format!("'{}' object has no attribute 'get'", crate::gate::pyjson::py_type_name(&other))));
            }
        };
        if o.get("is_error").is_some_and(|e| e.truthy()) {
            let r = match o.get("result") {
                Some(Value::Str(s)) => s.clone(),
                Some(v) => py_repr(v).unwrap_or_default(),
                None => "None".into(),
            };
            return Err(model_err(format!("claude -p reported is_error: {}", repr(head(&r, 200)))));
        }
        let text = match o.get("result") {
            Some(Value::Str(s)) => s.clone(),
            Some(v) if v.truthy() => py_repr(v).unwrap_or_default(),
            _ => String::new(),
        };
        let mut tokens = None;
        if let Value::Obj(usage) = o.value("usage") {
            let mut sum: i64 = 0;
            let mut have = false;
            for f in ["input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"] {
                match usage.value(f) {
                    Value::Int(t) => {
                        if let Ok(x) = t.parse::<i64>() {
                            sum = sum.wrapping_add(x);
                            have = true;
                        }
                    }
                    Value::Bool(b) => {
                        sum += *b as i64;
                        have = true;
                    }
                    Value::Float(x) => {
                        sum = sum.wrapping_add(*x as i64);
                        have = true;
                    }
                    _ => {}
                }
            }
            if have {
                tokens = Some(sum);
            }
        }
        let cost = match o.value("total_cost_usd") {
            Value::Float(x) => Some(*x),
            Value::Int(t) => t.parse::<f64>().ok(),
            _ => None,
        };
        Ok(Metered { text, tokens, cost })
    }

    /// Runs `bin args` in `dir` with `input` on stdin: the exit code (a
    /// signal as its negative), stdout and stderr, or None past `limit`.
    /// With `grace` a process past its limit gets SIGTERM and two seconds
    /// before SIGKILL; without it, SIGKILL at once, as subprocess.run does.
    fn spawn(
        &self,
        bin: &std::path::Path,
        args: &[String],
        dir: &str,
        input: &str,
        limit: Duration,
        grace: bool,
    ) -> Result<Option<(i32, Vec<u8>, Vec<u8>)>, CallError> {
        let mut child = match Command::new(bin)
            .args(args)
            .current_dir(dir)
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
        let data = input.as_bytes().to_vec();
        let writer = std::thread::spawn(move || {
            if let Some(s) = stdin.as_mut() {
                let _ = s.write_all(&data);
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
                Ok(None) if started.elapsed() >= limit => break None,
                Ok(None) => std::thread::sleep(Duration::from_millis(20)),
                Err(_) => break None,
            }
        };
        let Some(status) = status else {
            if grace {
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
            } else {
                let _ = child.kill();
                let _ = child.wait();
            }
            let _ = writer.join();
            return Ok(None);
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
        Ok(Some((code, out, err)))
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
    fn run_claude(&mut self, prompt: &str, limit: Duration) -> Result<String, CallError> {
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
                Ok(None) if started.elapsed() >= limit => break None,
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
        let retries = if call.default_retries { 0 } else { call.max_retries };
        let reasks = retries.min(MAX_REASKS);
        let mut attempt = augmented.clone();
        for i in 0..=reasks {
            let limit = left(call.deadline, self.claude_timeout)?;
            let stdout = self.run_claude(&attempt, limit)?;
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
    // the HTTP backend
    // -----------------------------------------------------------------

    fn api(&mut self, call: &Call) -> Result<Object, CallError> {
        let get = |k: &str| self.env.get(k).cloned();
        let w = wire::resolve(call.model, call.base_url, call.api_key, &get).map_err(CallError::Model)?;
        let timeout = wire::timeout(&get);
        let mut msgs: Vec<(String, String)> = vec![
            ("system".into(), format!("{}\n\n{}", call.system, schemas_gen::json_mode_system(call.response.name))),
            ("user".into(), call.user.to_string()),
        ];
        let attempts = if call.default_retries { 3 } else { call.max_retries } + 1;
        let mut first: Option<ValidationError> = None;
        for n in 1..=attempts {
            let opts = wire::Opts {
                json_object: true,
                thinking_budget: call.thinking_budget,
                reasoning_effort: call.reasoning_effort.to_string(),
                ..Default::default()
            };
            let payload = wire::body(&w, &msgs, &opts);
            let limit = left(call.deadline, Duration::from_secs_f64(timeout.min(1e9)))?.as_secs_f64();
            let posted = wire::post(&w, payload.as_bytes(), limit, &self.proxies);
            if posted.is_err() && call.deadline.is_some_and(|d| Instant::now() >= d) {
                return Err(model_err("TimeoutError"));
            }
            let (status, text) = posted.map_err(|e| match e {
                wire::PostErr::Model(m) => CallError::Model(m),
                wire::PostErr::Unported(why) => CallError::Unported(format!("{UNSUPPORTED}: {why}")),
            })?;
            let reply = wire::read_reply(&w, status, &text).map_err(CallError::Model)?;
            if reply.cut {
                return Err(model_err(wire::CUT_TEXT));
            }
            let verr = match validate_reply_json(call.response, &reply.text) {
                Ok(o) => return Ok(o),
                Err(e) => e,
            };
            let shown = verr.text();
            if first.is_none() {
                first = Some(verr);
            }
            if n == attempts {
                return Err(model_err(wire::clean(&flatten(first.as_ref().expect("set above"), n))));
            }
            msgs.push(("assistant".into(), reply.text));
            msgs.push(("user".into(), format!("{}{shown}", wire::REASK_TEXT)));
        }
        Err(model_err("unreachable"))
    }
}

/// What `call_claude_p_metered` gives: the reply's text, and Claude
/// Code's own token count and cost when the reply says them.
pub struct Metered {
    pub text: String,
    pub tokens: Option<i64>,
    pub cost: Option<f64>,
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

/// `llm_client.flatten`: the first attempt's error and the count.
fn flatten(first: &ValidationError, n: usize) -> String {
    let text = first.text();
    let line = text.trim().split('\n').map(|l| l.trim()).find(|l| !l.is_empty()).unwrap_or("").to_string();
    format!("ValidationError: {line} ({n} attempts)")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_old_backend_name_fails_the_call() {
        for v in ["litellm", " litellm "] {
            let env: HashMap<String, String> = [("OPENDAISUGI_LLM_BACKEND", v), ("ANTHROPIC_API_KEY", "sk-test")]
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect();
            let mut c = Client::new(env, "/nonexistent-home");
            assert!(c.check("anthropic/x").is_ok(), "{v:?}");
            let s = Schema { name: "Envelope", model: Id::Envelope };
            let call = Call::new("anthropic/x", "", "go", s, 0);
            match c.structured(&call) {
                Err(CallError::Model(m)) => assert_eq!(m, RENAMED_TEXT, "{v:?}"),
                other => panic!("{v:?}: {other:?}"),
            }
        }
    }

    #[test]
    fn auto_detection_names_api() {
        let env: HashMap<String, String> =
            [("ANTHROPIC_API_KEY".to_string(), "sk-test".to_string()), ("PATH".to_string(), String::new())].into();
        assert_eq!(Client::new(env, "/nonexistent-home").backend(), "api");
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
