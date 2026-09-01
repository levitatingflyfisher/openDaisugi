//! `llm_check.run_llm_check` on its litellm backend: the model call an
//! llm_check predicate makes, one POST to Anthropic's messages API, the
//! verdict read from the reply, and every failure worded as litellm 1.100.0
//! words it, since the failure text becomes the deny reason. The Go gate's
//! `llm.go` is the reference.
//!
//! An https URL goes over rustls (the ring provider) and trusts the
//! system's own root store, as the Go gate trusts Go's.
//!
//! Not decided here: a call through the claude-code backend (`claude -p`),
//! a model litellm would route elsewhere, settings that change litellm's
//! own behavior, a network or TLS failure other than a refused connection,
//! and a reply read in a way the port does not model.

use super::config::load_field;
use super::dispatch::py_which;
use super::paths::path_join;
use super::py::re as pyre;
use super::pyjson::{dumps, loads, py_decode_error, py_str, py_type_name, Object, PyDecodeFail, Value};
use super::{catch, undecided, Runner, R};
use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Arc, OnceLock};
use std::time::Duration;

const LITELLM_VERSION: &str = "1.100.0";
/// As the Go gate's llmJSONDepth.
const LLM_JSON_DEPTH: usize = 900;
const DEFAULT_MODEL: &str = "anthropic/claude-haiku-4-5-20251001";
const SYSTEM: &str = r#"You are a strict verifier. Answer in strict JSON: {"satisfied": true|false, "rationale": "short reason"}. No prose outside the JSON."#;
const MISSING_KEY: &str = "litellm.AuthenticationError: Missing Anthropic API Key - A call is being made to anthropic but no key is set either in the environment variables or via params. Please set `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` in your environment vars";

/// litellm's secret_redaction._SECRET_RE, with its flags (IGNORECASE).
const SECRET_PATTERN: &str = concat!(
    "(?i)",
    r#"-----BEGIN[A-Z \-]*PRIVATE KEY-----[\s\S]*?-----END[A-Z \-]*PRIVATE KEY-----|\bya29\.[A-Za-z0-9_.~+/-]+|(?:client_secret|azure_password|azure_username)\s+[^\s,'\"})\]{}>]+|(?:AKIA|ASIA)[0-9A-Z]{16}|Bearer\s+[A-Za-z0-9\-._~+/]{10,}=*|Basic\s+[A-Za-z0-9+/]{10,}={0,2}|sk-[A-Za-z0-9\-_]{13,}|(?:api[_-]?key)['\"]?\s*[:=]\s*['\"]?[^\s,'\"})\]{}>]{8,}|(?:x-api-key|api-key)['\"]?\s*[:=]\s*['\"]?[^\s,'\"})\]{}>]+|x-ak-[A-Za-z0-9\-_]{20,}|AIza[0-9A-Za-z\-_]{35}|(?<=[?&])key=[^\s&'\"]{8,}|(?:^|(?<=\W))\w*(?:password|passwd|client_secret|secret_key|_secret)['\"]?\s*[:=]\s*['\"]?[^\s,'\"})\]{}>]+|(?<=://)[^\s'\"]*:[^\s'\"@]+(?=@)|dapi[0-9a-f]{32}|litellm\.[A-Za-z0-9_]*_key['\"]?\s*[:=]\s*['\"]?[^\s,'\"})\]{}>]+|private_key['\"]?\s*[:=]\s*['\"]?(?:-----BEGIN[A-Z \-]*PRIVATE KEY-----[\s\S]*?-----END[A-Z \-]*PRIVATE KEY-----|[^\s,'\"})\]{}>]+)|(?:master_key|xai_key|database_url|db_url|connection_string|aws_secret_access_key|aws_session_token|aws_access_key_id|signing_key|encryption_key|auth_token|access_token|refresh_token|slack_webhook_url|webhook_url|database_connection_string|huggingface_token|jwt_secret)['\"]?\s*[:=]\s*['\"]?[^\s,'\"})\]{}>]+|\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*|[?&]sig=[A-Za-z0-9%+/=]+|\{[^{}]*"type"\s*:\s*"service_account"[^{}]*(?:\{[^{}]*\}[^{}]*)*\}"#
);

/// `LLMCheckResult`: satisfied, reason, errored.
pub struct LlmResult {
    pub satisfied: bool,
    pub reason: String,
    pub errored: bool,
}

/// An exception `_invoke_model` raises: its str().
enum Out<T> {
    Ok(T),
    Fail(String),
}

macro_rules! tryo {
    ($e:expr) => {
        match $e {
            Out::Ok(v) => v,
            Out::Fail(m) => return Ok(Out::Fail(m)),
        }
    };
}

impl Runner {
    /// `llm_check.run_llm_check(rule, payload)`.
    pub fn run_llm_check(&self, rule: &str, payload: &Object) -> R<LlmResult> {
        Ok(match self.invoke_model(rule, payload)? {
            Out::Ok((satisfied, reason)) => LlmResult { satisfied, reason, errored: false },
            Out::Fail(m) => LlmResult { satisfied: false, reason: format!("error: llm_check call failed: {m}"), errored: true },
        })
    }

    /// `llm.resolve_backend()`.
    fn llm_backend(&self) -> R<String> {
        if let Some(v) = self.env.get("OPENDAISUGI_LLM_BACKEND").filter(|v| !v.is_empty()) {
            return Ok(v.clone());
        }
        let cfg = path_join(&path_join(&self.path_home()?, ".opendaisugi"), "config.yaml");
        if let Ok(Some(b)) = catch(load_field(self, &cfg, "llm_backend", ""))? {
            let b = super::py::text::strip(&b).to_string();
            if !b.is_empty() {
                return Ok(b);
            }
        }
        let set = |k: &str| self.env.get(k).is_some_and(|v| !v.is_empty());
        if set("ANTHROPIC_API_KEY") || set("ANTHROPIC_AUTH_TOKEN") {
            return Ok("litellm".into());
        }
        if py_which("claude", self.env.get("PATH").map(String::as_str)).is_some() {
            return Ok("claude-code".into());
        }
        Ok("litellm".into())
    }

    /// What in the environment makes litellm or its HTTP client behave in a
    /// way the port does not model.
    fn llm_unported_env(&self) -> Option<String> {
        for (k, v) in &self.env {
            let u = k.to_uppercase();
            let set = !v.is_empty();
            if k.starts_with("LITELLM_") || k == "MINIMUM_CUSTOM_KEY_LENGTH" {
                return Some(k.clone());
            }
            if set && matches!(u.as_str(), "HTTP_PROXY" | "HTTPS_PROXY" | "ALL_PROXY" | "NO_PROXY") {
                return Some(k.clone());
            }
            if set
                && matches!(k.as_str(), "SSL_CERT_FILE" | "SSL_CERT_DIR" | "REQUESTS_CA_BUNDLE" | "CURL_CA_BUNDLE" | "SSL_VERIFY")
            {
                return Some(k.clone());
            }
        }
        None
    }

    /// `llm_check._invoke_model` on the litellm backend.
    fn invoke_model(&self, rule: &str, payload: &Object) -> R<Out<(bool, String)>> {
        let model = self.env.get("OPENDAISUGI_LLM_CHECK_MODEL").cloned().unwrap_or_else(|| DEFAULT_MODEL.into());
        if self.llm_backend()? == "claude-code" {
            return undecided("an llm_check through the claude-code backend, which runs claude -p");
        }
        if !model.starts_with("anthropic/") || model.len() == "anthropic/".len() {
            return undecided("an llm_check model litellm does not send to Anthropic");
        }
        if let Some(k) = self.llm_unported_env() {
            return undecided(format!("an llm_check under {k}, which changes how litellm calls the model"));
        }
        let pj: String = dumps(&Value::Obj(payload.clone()), true).chars().take(4000).collect();
        let user = format!("Rule:\n{rule}\n\nPlan payload (JSON):\n{pj}\n\nDoes the plan payload satisfy the rule?");
        let mut headers: Vec<(String, String)> = Vec::new();
        match (self.env.get("ANTHROPIC_API_KEY"), self.env.get("ANTHROPIC_AUTH_TOKEN")) {
            (Some(k), _) => {
                if !k.is_empty() {
                    headers.push(("x-api-key".into(), k.clone()));
                }
            }
            (None, Some(t)) => {
                if !t.is_empty() {
                    headers.push(("authorization".into(), format!("Bearer {t}")));
                }
            }
            (None, None) => return Ok(Out::Fail(MISSING_KEY.into())),
        }
        let mut base = self.env.get("ANTHROPIC_API_BASE").filter(|v| !v.is_empty()).cloned().unwrap_or_default();
        if base.is_empty() {
            base = self.env.get("ANTHROPIC_BASE_URL").cloned().unwrap_or_default();
        }
        if base.is_empty() {
            base = "https://api.anthropic.com/v1/messages".into();
        }
        if !base.ends_with("/v1/messages") {
            base.push_str("/v1/messages");
        }
        let body = Object::new()
            .with("model", model.trim_start_matches("anthropic/"))
            .with(
                "messages",
                Value::List(vec![Value::Obj(Object::new().with("role", "user").with(
                    "content",
                    Value::List(vec![Value::Obj(Object::new().with("type", "text").with("text", user.as_str()))]),
                ))]),
            )
            .with("temperature", 0i64)
            .with("max_tokens", 200i64)
            .with("system", Value::List(vec![Value::Obj(Object::new().with("type", "text").with("text", SYSTEM))]));
        headers.push(("user-agent".into(), format!("litellm/{LITELLM_VERSION}")));
        headers.push(("anthropic-version".into(), "2023-06-01".into()));
        headers.push(("accept".into(), "application/json".into()));
        headers.push(("content-type".into(), "application/json".into()));
        let (status, text) = tryo!(post(&base, &headers, dumps(&Value::Obj(body), true).as_bytes(), None)?);
        let content = tryo!(content_of(status, &text)?);
        // Nesting past 900 is left undecided: in the verifier's thread,
        // json.loads might raise RecursionError before that depth.
        match py_decode_error(&content, LLM_JSON_DEPTH) {
            None => {}
            Some(PyDecodeFail::TooDeep) => return undecided("an llm_check reply nested past what json.loads reads"),
            Some(PyDecodeFail::Raised(m)) => return Ok(Out::Fail(m)),
        }
        let parsed = match loads(&content) {
            Ok(v) => v,
            Err(_) => return undecided("an llm_check reply that json.loads refuses"),
        };
        let o = match parsed {
            Value::Obj(o) => o,
            other => return Ok(Out::Fail(format!("'{}' object has no attribute 'get'", py_type_name(&other)))),
        };
        let sat = o.get("satisfied").map(|v| v.truthy()).unwrap_or(false);
        let why = match o.get("rationale") {
            None => String::new(),
            Some(v) => py_str(v)?,
        };
        Ok(Out::Ok((sat, why)))
    }
}

/// The URL split as the port sends it: TLS or not, host, port, path.
fn target(url: &str) -> R<(bool, String, u16, String)> {
    let (scheme, rest) = match url.split_once("://") {
        Some(x) => x,
        None => return undecided("an llm_check URL the port does not send to"),
    };
    let tls = match scheme {
        "http" => false,
        "https" => true,
        _ => return undecided("an llm_check URL the port does not send to"),
    };
    let (hostport, path) = match rest.find('/') {
        Some(i) => (&rest[..i], rest[i..].to_string()),
        None => (rest, "/".to_string()),
    };
    if hostport.is_empty() || hostport.contains('@') || hostport.contains('[') {
        return undecided("an llm_check URL the port does not send to");
    }
    let (host, port) = match hostport.rsplit_once(':') {
        Some((h, p)) => match p.parse::<u16>() {
            Ok(n) => (h.to_string(), n),
            Err(_) => return undecided("an llm_check URL the port does not send to"),
        },
        None => (hostport.to_string(), if tls { 443 } else { 80 }),
    };
    if host.is_empty() {
        return undecided("an llm_check URL the port does not send to");
    }
    Ok((tls, host, port, path))
}

/// The system's root certificates, read once per process. A store that
/// yields no certificate at all leaves the call undecided.
pub(crate) fn system_roots() -> R<Arc<rustls::RootCertStore>> {
    static ROOTS: OnceLock<Option<Arc<rustls::RootCertStore>>> = OnceLock::new();
    let roots = ROOTS.get_or_init(|| {
        let mut store = rustls::RootCertStore::empty();
        let found = rustls_native_certs::load_native_certs();
        for c in found.certs {
            let _ = store.add(c);
        }
        if store.is_empty() {
            None
        } else {
            Some(Arc::new(store))
        }
    });
    match roots {
        Some(r) => Ok(r.clone()),
        None => undecided("an llm_check sent over TLS on a system with no root certificates"),
    }
}

/// A stream the request goes over: plain TCP, or TLS on top of it.
enum Conn {
    Plain(TcpStream),
    Tls(Box<rustls::StreamOwned<rustls::ClientConnection, TcpStream>>),
}

impl Conn {
    fn send(&mut self, bytes: &[u8]) -> std::io::Result<()> {
        match self {
            Conn::Plain(s) => s.write_all(bytes),
            Conn::Tls(s) => s.write_all(bytes).and_then(|_| s.flush()),
        }
    }

    /// Everything the server sends. A TLS peer that closes the socket with
    /// no close_notify ends the reply there; the framing check after this
    /// catches a reply cut short.
    fn read_all(&mut self, out: &mut Vec<u8>) -> std::io::Result<()> {
        match self {
            Conn::Plain(s) => s.read_to_end(out).map(|_| ()),
            Conn::Tls(s) => match s.read_to_end(out) {
                Ok(_) => Ok(()),
                Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => Ok(()),
                Err(e) => Err(e),
            },
        }
    }
}

/// One HTTP/1.1 POST, as litellm's client sends it: no redirects followed,
/// litellm's own 600 s timeout (the gate's --verify-timeout ends the wait
/// first). `roots` replaces the system store, for the tests only.
fn post(url: &str, headers: &[(String, String)], body: &[u8], roots: Option<Arc<rustls::RootCertStore>>) -> R<Out<(u16, String)>> {
    let (tls, host, port, path) = target(url)?;
    let tls_config = if tls {
        let roots = match roots {
            Some(r) => r,
            None => system_roots()?,
        };
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let config = match rustls::ClientConfig::builder_with_provider(provider).with_safe_default_protocol_versions() {
            Ok(b) => b.with_root_certificates(roots).with_no_client_auth(),
            Err(_) => return undecided("an llm_check sent over TLS the port cannot set up"),
        };
        let name = match rustls::pki_types::ServerName::try_from(host.clone()) {
            Ok(n) => n,
            Err(_) => return undecided("an llm_check URL the port does not send to"),
        };
        Some((Arc::new(config), name))
    } else {
        None
    };
    let addrs: Vec<_> = match (host.as_str(), port).to_socket_addrs() {
        Ok(a) => a.collect(),
        Err(e) => return undecided(format!("an llm_check call that failed at the network level ({e})")),
    };
    let mut stream = None;
    let mut refused = false;
    for a in &addrs {
        match TcpStream::connect_timeout(a, Duration::from_secs(600)) {
            Ok(s) => {
                stream = Some(s);
                break;
            }
            Err(e) if e.kind() == std::io::ErrorKind::ConnectionRefused => refused = true,
            Err(_) => {}
        }
    }
    let s = match stream {
        Some(s) => s,
        None if refused => {
            return Ok(Out::Fail(
                "litellm.InternalServerError: AnthropicException - [Errno 111] Connection refused. \
                 Handle with `litellm.InternalServerError`."
                    .into(),
            ))
        }
        None => return undecided("an llm_check call that failed at the network level"),
    };
    let _ = s.set_read_timeout(Some(Duration::from_secs(600)));
    let _ = s.set_write_timeout(Some(Duration::from_secs(600)));
    let mut conn = match tls_config {
        None => Conn::Plain(s),
        Some((config, name)) => match rustls::ClientConnection::new(config, name) {
            Ok(c) => Conn::Tls(Box::new(rustls::StreamOwned::new(c, s))),
            Err(_) => return undecided("an llm_check sent over TLS the port cannot set up"),
        },
    };
    let default_port = if tls { 443 } else { 80 };
    let host_header = if port == default_port { host.clone() } else { format!("{host}:{port}") };
    let mut req = format!("POST {path} HTTP/1.1\r\nhost: {host_header}\r\n");
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    req.push_str(&format!("content-length: {}\r\nconnection: close\r\n\r\n", body.len()));
    let mut out = req.into_bytes();
    out.extend_from_slice(body);
    let mut raw = Vec::new();
    if let Err(e) = conn.send(&out).and_then(|_| conn.read_all(&mut raw)) {
        return undecided(format!("an llm_check call that failed at the network level ({e})"));
    }
    let split = match raw.windows(4).position(|w| w == b"\r\n\r\n") {
        Some(i) => i,
        None => return undecided("an llm_check reply that could not be read"),
    };
    let head = String::from_utf8_lossy(&raw[..split]).into_owned();
    let mut body = raw[split + 4..].to_vec();
    let mut lines = head.split("\r\n");
    let status: u16 = match lines.next().and_then(|l| l.split_whitespace().nth(1)).and_then(|c| c.parse().ok()) {
        Some(c) => c,
        None => return undecided("an llm_check reply that could not be read"),
    };
    let mut ctype = String::new();
    let mut chunked = false;
    let mut length: Option<usize> = None;
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            let (k, v) = (k.trim().to_ascii_lowercase(), v.trim().to_string());
            match k.as_str() {
                "content-type" => ctype = v.to_ascii_lowercase(),
                "content-encoding" => return undecided("an llm_check reply in an encoding the port does not read"),
                "transfer-encoding" => chunked = v.to_ascii_lowercase().contains("chunked"),
                "content-length" => match v.parse() {
                    Ok(n) => length = Some(n),
                    Err(_) => return undecided("an llm_check reply that could not be read"),
                },
                _ => {}
            }
        }
    }
    if chunked {
        body = match dechunk(&body) {
            Some(b) => b,
            None => return undecided("an llm_check reply that could not be read"),
        };
    } else if let Some(n) = length {
        // A reply shorter than its length was cut short; httpx raises.
        if body.len() < n {
            return undecided("an llm_check reply that could not be read");
        }
        body.truncate(n);
    }
    // httpx decodes as UTF-8 unless the content type names another
    // charset; json.loads of bytes guesses UTF-16 or 32 from NULs and
    // strips a BOM. All of that is left undecided.
    if (ctype.contains("charset=") && !ctype.contains("charset=utf-8"))
        || body.starts_with(b"\xef\xbb\xbf")
        || body.contains(&0)
    {
        return undecided("an llm_check reply in an encoding the port does not read");
    }
    match String::from_utf8(body) {
        Ok(t) => Ok(Out::Ok((status, t))),
        Err(_) => undecided("an llm_check reply in an encoding the port does not read"),
    }
}

pub(crate) fn dechunk(b: &[u8]) -> Option<Vec<u8>> {
    let mut out = Vec::new();
    let mut i = 0;
    loop {
        let end = i + b[i..].windows(2).position(|w| w == b"\r\n")?;
        let size_text = std::str::from_utf8(&b[i..end]).ok()?;
        let size = usize::from_str_radix(size_text.split(';').next()?.trim(), 16).ok()?;
        i = end + 2;
        if size == 0 {
            return Some(out);
        }
        out.extend_from_slice(b.get(i..i + size)?);
        i += size + 2;
    }
}

/// litellm's redact_string.
fn redact(s: &str) -> R<String> {
    let re = pyre::cached_compile(SECRET_PATTERN)?;
    Ok(re.sub(s, |_, _| "REDACTED".to_string())?)
}

fn context_window_exceeded(s: &str) -> bool {
    let low = super::py::text::lower(s);
    if low.contains("string_above_max_length") || (low.contains("invalid 'user'") && low.contains("string too long")) {
        return false;
    }
    const PHRASES: &[&str] = &[
        "exceed context limit",
        "this model's maximum context length is",
        "string too long. expected a string with maximum length",
        "model's maximum context limit",
        "is longer than the model's context length",
        "input tokens exceed the configured limit",
        "`inputs` tokens + `max_new_tokens` must be",
        "exceeds the available context size",
        "exceeds the maximum number of tokens allowed",
    ];
    if PHRASES.iter().any(|p| low.contains(p)) {
        return true;
    }
    if low.contains("current length is") && low.contains("while limit is") {
        return true;
    }
    low.contains("maximum input length is") && low.contains("tokens")
}

/// `exception_type`'s anthropic branch: the str() of what litellm raises,
/// or None when no rule matched.
fn mapped(err: &str, status: u16) -> Option<String> {
    const TIMEOUT: &[&str] =
        &["Request Timeout Error", "Request timed out", "Timed out generating response", "The read operation timed out"];
    if TIMEOUT.iter().any(|p| err.contains(p)) {
        return Some(format!("litellm.Timeout: APITimeoutError - Request timed out. Error_str: {err}"));
    }
    if err.contains("prompt is too long") || err.contains("prompt: length") || context_window_exceeded(err) {
        return Some(format!("litellm.ContextWindowExceededError: litellm.BadRequestError: AnthropicError - {err}"));
    }
    if err.contains("overloaded_error") || err.contains("Overloaded") {
        return Some(format!("litellm.InternalServerError: AnthropicError - {err}"));
    }
    if err.contains("Invalid API Key") {
        return Some(format!("litellm.AuthenticationError: AnthropicError - {err}"));
    }
    if err.contains("content filtering policy") {
        return Some(format!("litellm.BadRequestError: litellm.ContentPolicyViolationError: AnthropicError - {err}"));
    }
    if err.contains("Client error '400 Bad Request'") {
        return Some(format!("litellm.BadRequestError: AnthropicError - {err}"));
    }
    let e = format!("AnthropicException - {err}");
    Some(match status {
        0 => return None,
        401 => format!("litellm.AuthenticationError: {e}"),
        403 => format!("litellm.PermissionDeniedError: {e}"),
        400 | 413 => format!("litellm.BadRequestError: {e}"),
        404 => format!("litellm.NotFoundError: {e}"),
        408 => format!("litellm.Timeout: {e}"),
        429 => format!("litellm.RateLimitError: {e}"),
        500 | 529 => format!("litellm.InternalServerError: {e}. Handle with `litellm.InternalServerError`."),
        502 => format!("litellm.BadGatewayError: AnthropicException BadGatewayError - {err}"),
        503 => format!("litellm.ServiceUnavailableError: {e}. Handle with `litellm.ServiceUnavailableError`."),
        504 => format!("litellm.Timeout: AnthropicException Timeout - {err}"),
        400..=499 => format!("litellm.BadRequestError: {e}"),
        500..=599 => format!("litellm.APIError: {e}"),
        300..=399 => format!("litellm.APIConnectionError: {e}"),
        _ => return None,
    })
}

/// The reply read as litellm reads it: an error status raises, a 200 is a
/// message whose text blocks, joined, are the content.
fn content_of(status: u16, text: &str) -> R<Out<String>> {
    if status != 200 {
        return match mapped(&redact(text)?, status) {
            Some(m) => Ok(Out::Fail(m)),
            None => undecided(format!("an llm_check reply with status {status}")),
        };
    }
    let v = match loads(text) {
        Ok(v) => v,
        Err(_) => return undecided("an llm_check reply litellm reads another way"),
    };
    let o = match v {
        Value::Obj(o) => o,
        _ => return undecided("an llm_check reply that is not a message"),
    };
    if o.get("error").is_some() {
        return undecided("an llm_check reply that carries an error");
    }
    for k in ["model", "stop_reason", "usage"] {
        if o.get(k).is_none() {
            return undecided(format!("an llm_check reply with no {k}"));
        }
    }
    let usage = match o.value("usage") {
        Value::Obj(u) => u.clone(),
        _ => return undecided("an llm_check reply whose usage is not an object"),
    };
    for k in ["input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"] {
        if let Some(x) = usage.get(k) {
            if !matches!(x, Value::Int(_)) {
                return undecided("an llm_check reply with a usage count that is not an int");
            }
        }
    }
    let blocks = match o.value("content") {
        Value::List(l) => l.clone(),
        _ => return undecided("an llm_check reply whose content is not a list"),
    };
    let mut out = String::new();
    for b in blocks {
        let blk = match b {
            Value::Obj(x) => x,
            _ => return undecided("an llm_check reply block that is not an object"),
        };
        match blk.value("type").as_str() {
            Some("text") => match blk.value("text") {
                Value::Str(t) => out.push_str(t),
                _ => return undecided("an llm_check text block with no text"),
            },
            Some("thinking") | Some("redacted_thinking") => {}
            _ => return undecided("an llm_check reply block of a kind the port does not read"),
        }
    }
    Ok(Out::Ok(out))
}

#[cfg(test)]
mod tests {
    //! The TLS path against a local fake server under a test CA made here.
    //! No real model and no network: every socket is on 127.0.0.1.
    use super::*;
    use crate::gate::Fault;
    use std::net::TcpListener;
    use std::thread::JoinHandle;

    struct Ca {
        roots: Arc<rustls::RootCertStore>,
        server: Arc<rustls::ServerConfig>,
    }

    /// A CA and a leaf for `name`, signed by it.
    fn ca_for(name: &str) -> Ca {
        let ca_key = rcgen::KeyPair::generate().unwrap();
        let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        let ca = rcgen::CertifiedIssuer::self_signed(ca_params, ca_key).unwrap();
        let leaf_key = rcgen::KeyPair::generate().unwrap();
        let leaf = rcgen::CertificateParams::new(vec![name.to_string()]).unwrap().signed_by(&leaf_key, &ca).unwrap();
        let mut roots = rustls::RootCertStore::empty();
        roots.add(ca.der().clone()).unwrap();
        let key = rustls::pki_types::PrivateKeyDer::Pkcs8(leaf_key.serialize_der().into());
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let server = rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(vec![leaf.der().clone(), ca.der().clone()], key)
            .unwrap();
        Ca { roots: Arc::new(roots), server: Arc::new(server) }
    }

    /// Reads one request (head and its content-length body).
    fn read_request(s: &mut impl Read) -> Vec<u8> {
        let mut buf = Vec::new();
        let mut b = [0u8; 4096];
        loop {
            if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                let head = String::from_utf8_lossy(&buf[..i]).to_ascii_lowercase();
                let n: usize = head
                    .lines()
                    .find_map(|l| l.strip_prefix("content-length:").map(|v| v.trim().parse().unwrap()))
                    .unwrap_or(0);
                if buf.len() >= i + 4 + n {
                    return buf;
                }
            }
            match s.read(&mut b) {
                Ok(0) | Err(_) => return buf,
                Ok(k) => buf.extend_from_slice(&b[..k]),
            }
        }
    }

    /// Serves one connection: `reply` as is, over TLS when `tls` is given.
    /// With `notify` false, the TLS side closes the socket with no
    /// close_notify. Returns the port and the request the server read.
    fn serve(tls: Option<Arc<rustls::ServerConfig>>, reply: Vec<u8>, notify: bool) -> (u16, JoinHandle<Vec<u8>>) {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        let h = std::thread::spawn(move || {
            let (mut sock, _) = l.accept().unwrap();
            match tls {
                None => {
                    let req = read_request(&mut sock);
                    sock.write_all(&reply).unwrap();
                    req
                }
                Some(cfg) => {
                    let conn = rustls::ServerConnection::new(cfg).unwrap();
                    let mut s = rustls::StreamOwned::new(conn, sock);
                    let req = read_request(&mut s);
                    if req.is_empty() {
                        return req;
                    }
                    s.write_all(&reply).unwrap();
                    s.flush().unwrap();
                    if notify {
                        s.conn.send_close_notify();
                        let _ = s.conn.write_tls(&mut s.sock);
                    }
                    req
                }
            }
        });
        (port, h)
    }

    fn headers() -> Vec<(String, String)> {
        vec![("x-api-key".into(), "sk-test".into()), ("content-type".into(), "application/json".into())]
    }

    fn call(scheme: &str, host: &str, port: u16, roots: &Arc<rustls::RootCertStore>) -> R<Out<(u16, String)>> {
        post(&format!("{scheme}://{host}:{port}/v1/messages"), &headers(), b"{\"a\": 1}", Some(roots.clone()))
    }

    fn show(r: R<Out<(u16, String)>>) -> String {
        match r {
            Ok(Out::Ok((st, t))) => format!("ok {st} {t}"),
            Ok(Out::Fail(m)) => format!("fail {m}"),
            Err(Fault::Undecided(w)) => format!("undecided {w}"),
            Err(Fault::Raised(e)) => format!("raised {}", e.msg),
        }
    }

    fn http(status: &str, extra: &str, body: &str) -> Vec<u8> {
        format!("HTTP/1.1 {status}\r\ncontent-type: application/json\r\n{extra}\r\n{body}").into_bytes()
    }

    const MSG: &str = r#"{"id":"m","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"{\"satisfied\": true, \"rationale\": \"ok\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}"#;

    #[test]
    fn https_reads_every_reply_as_http_does() {
        let ca = ca_for("localhost");
        let over = r#"{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}"#;
        let chunked = format!("{:x}\r\n{MSG}\r\n0\r\n\r\n", MSG.len());
        let replies = vec![
            http("200 OK", &format!("content-length: {}\r\n", MSG.len()), MSG),
            http("200 OK", "", MSG),
            http("529 Overloaded", &format!("content-length: {}\r\n", over.len()), over),
            http("401 Unauthorized", "content-length: 2\r\n", "{}"),
            http("200 OK", "transfer-encoding: chunked\r\n", &chunked),
            http("200 OK", "content-length: 99999\r\n", MSG),
        ];
        for reply in replies {
            let (p1, h1) = serve(None, reply.clone(), true);
            let plain = show(call("http", "localhost", p1, &ca.roots));
            let req_plain = h1.join().unwrap();
            let (p2, h2) = serve(Some(ca.server.clone()), reply.clone(), true);
            let tls = show(call("https", "localhost", p2, &ca.roots));
            let req_tls = h2.join().unwrap();
            assert_eq!(plain, tls);
            let strip = |r: &[u8], p: u16| String::from_utf8_lossy(r).replace(&format!(":{p}"), ":PORT");
            assert_eq!(strip(&req_plain, p1), strip(&req_tls, p2));
        }
    }

    #[test]
    fn a_decided_reply_over_https() {
        let ca = ca_for("localhost");
        let (p, h) = serve(Some(ca.server.clone()), http("200 OK", &format!("content-length: {}\r\n", MSG.len()), MSG), true);
        assert_eq!(show(call("https", "localhost", p, &ca.roots)), format!("ok 200 {MSG}"));
        h.join().unwrap();
    }

    #[test]
    fn no_close_notify_after_a_whole_reply_is_the_same_reply() {
        let ca = ca_for("localhost");
        let reply = http("200 OK", &format!("content-length: {}\r\n", MSG.len()), MSG);
        let (p, h) = serve(Some(ca.server.clone()), reply, false);
        assert_eq!(show(call("https", "localhost", p, &ca.roots)), format!("ok 200 {MSG}"));
        h.join().unwrap();
    }

    #[test]
    fn a_reply_cut_short_is_not_decided() {
        let ca = ca_for("localhost");
        let reply = http("200 OK", "content-length: 99999\r\n", MSG);
        let (p, h) = serve(Some(ca.server.clone()), reply, false);
        assert_eq!(show(call("https", "localhost", p, &ca.roots)), "undecided an llm_check reply that could not be read");
        h.join().unwrap();
    }

    #[test]
    fn an_untrusted_or_misnamed_server_is_not_decided() {
        let ca = ca_for("localhost");
        let other = ca_for("localhost");
        let (p, h) = serve(Some(ca.server.clone()), http("200 OK", "", MSG), true);
        let got = show(call("https", "localhost", p, &other.roots));
        assert!(got.starts_with("undecided an llm_check call that failed at the network level"), "{got}");
        h.join().unwrap();
        let (p, h) = serve(Some(ca.server.clone()), http("200 OK", "", MSG), true);
        let got = show(call("https", "127.0.0.1", p, &ca.roots));
        assert!(got.starts_with("undecided an llm_check call that failed at the network level"), "{got}");
        h.join().unwrap();
    }

    #[test]
    fn a_refused_https_port_is_litellms_refusal() {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let p = l.local_addr().unwrap().port();
        drop(l);
        let ca = ca_for("localhost");
        assert_eq!(
            show(call("https", "127.0.0.1", p, &ca.roots)),
            "fail litellm.InternalServerError: AnthropicException - [Errno 111] Connection refused. \
             Handle with `litellm.InternalServerError`."
        );
    }

    #[test]
    fn the_default_https_port_is_443() {
        let (tls, host, port, path) = target("https://api.anthropic.com/v1/messages").unwrap_or_else(|_| panic!());
        assert!(tls);
        assert_eq!((host.as_str(), port, path.as_str()), ("api.anthropic.com", 443, "/v1/messages"));
    }
}
