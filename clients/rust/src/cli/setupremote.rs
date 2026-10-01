//! `daisugi tiers setup --remote`: `model_host.probe`, `record` and
//! `describe_host`. One round trip per candidate wire, the first that
//! answers wins, and nothing is recorded for a host no wire identified.
//! The Go client's `cli/setupremote.go` is the twin.

use super::config;
use super::gateroot::join;
use super::{Env, Res};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{dumps, loads, LoadError, Object, Value};
use crate::llm::http;
use crate::netproxy;

const OLLAMA_DEFAULT_PORT: u16 = 11434;
/// `model_host.CONTEXT_FLOOR`.
const CONTEXT_FLOOR: i64 = 32768;
const KINDS: [&str; 3] = ["ollama", "openai", "anthropic"];
/// `model_host._OPENAI_CONTEXT_KEYS`.
const OPENAI_CONTEXT_KEYS: [&str; 4] = ["context_length", "context_window", "max_model_len", "n_ctx_train"];
const PROBE_MODEL: &str = "daisugi-probe";
const UNREAD: &str = "the host answered with a value this binary does not read";

fn backend(kind: &str) -> Option<&'static str> {
    match kind {
        "ollama" => Some("ollama"),
        "openai" => Some("openai-compatible"),
        "anthropic" => Some("anthropic-compatible"),
        _ => None,
    }
}

/// `model_host.HostInfo`.
struct HostInfo {
    base_url: String,
    kind: String,
    chosen: Option<String>,
    context: Option<i64>,
    warnings: Vec<String>,
    reachable: bool,
}

/// `model_host._Probed`.
#[derive(Default)]
struct Probed {
    chosen: Option<String>,
    context: Option<i64>,
}

/// `model_host.context_floor_warning`.
fn context_floor_warning(ctx: Option<i64>) -> Option<String> {
    match ctx {
        None => Some("context window unknown; set it with `--context 32768` if you know it".into()),
        Some(c) if c < CONTEXT_FLOOR => {
            Some(format!("{c} tokens is below the 32K floor agentic coding needs; expect truncation"))
        }
        _ => None,
    }
}

enum RemoteErr {
    /// Python raises ValueError: its text.
    Value(String),
    /// A value this binary does not read the way the oracle does.
    Unread,
}

/// `model_host.parse_remote`.
fn parse_remote(spec: &str) -> Result<(String, Option<i64>), RemoteErr> {
    if spec.contains("://") {
        return Err(RemoteErr::Value(format!(
            "give host[:port] without a scheme, for example box:11434; got {}",
            repr(spec)
        )));
    }
    if let Some((host, port)) = spec.rsplit_once(':') {
        if !port.is_ascii() {
            // str.isdigit counts other scripts' digits: not read here.
            return Err(RemoteErr::Unread);
        }
        if !port.is_empty() && port.bytes().all(|c| c.is_ascii_digit()) {
            let n: i64 = port.parse().map_err(|_| RemoteErr::Unread)?;
            return Ok((host.to_string(), Some(n)));
        }
    }
    Ok((spec.to_string(), None))
}

fn base_url(host: &str, port: Option<i64>) -> String {
    match port {
        Some(p) => format!("http://{host}:{p}"),
        None if host.contains(':') => format!("http://{host}"),
        None => format!("http://{host}:{OLLAMA_DEFAULT_PORT}"),
    }
}

/// The probe's requests, as httpx sends them, through the proxies httpx
/// would use.
struct HostClient {
    proxies: netproxy::Httpx,
    answered: bool,
}

impl HostClient {
    /// `model_host._send`: the status and the body, None on any transport
    /// error. Any response marks the host answered.
    fn send(&mut self, url: &str, body: Option<&[u8]>) -> Option<(u16, Vec<u8>)> {
        // The client library's own words; the compare holds them as
        // {CLIENT} (SR-1).
        let mut headers = vec![
            ("accept", "*/*".to_string()),
            ("accept-encoding", "identity".to_string()),
            ("user-agent", "opendaisugi".to_string()),
        ];
        let r = match body {
            Some(b) => {
                headers.push(("content-type", "application/json".to_string()));
                http::post_raw(url, &headers, b, 3.0, &self.proxies)
            }
            None => http::get(url, &headers, 3.0, &self.proxies),
        };
        let r = r.ok()?;
        self.answered = true;
        Some(r)
    }
}

/// `resp.json()`: the value, Ok(None) where it raises.
fn json_body(b: &[u8]) -> Result<Option<Value>, RemoteErr> {
    let Ok(text) = std::str::from_utf8(b) else { return Ok(None) };
    match loads(text) {
        Ok(v) => Ok(Some(v)),
        Err(LoadError::Unsupported) => Err(RemoteErr::Unread),
        Err(_) => Ok(None),
    }
}

/// `isinstance(v, int)` and its value; a bool is refused, since Python
/// counts it an int.
fn py_int(v: &Value) -> Result<Option<i64>, RemoteErr> {
    match v {
        Value::Bool(_) => Err(RemoteErr::Unread),
        Value::Int(t) => t.parse().map(Some).map_err(|_| RemoteErr::Unread),
        _ => Ok(None),
    }
}

/// `[m[key] for m in xs if isinstance(m, dict) and m.get(key)]`.
fn names_of(xs: &[Value], key: &str) -> Result<Vec<String>, RemoteErr> {
    let mut out = vec![];
    for x in xs {
        let Value::Obj(o) = x else { continue };
        let v = o.value(key);
        if !v.truthy() {
            continue;
        }
        match v {
            Value::Str(s) => out.push(s.clone()),
            _ => return Err(RemoteErr::Unread),
        }
    }
    Ok(out)
}

/// `_ollama_num_ctx`: `\bnum_ctx\s+(\d+)` in parameters, then modelfile.
fn num_ctx(show: &Object) -> Result<Option<i64>, RemoteErr> {
    static RE: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let re =
        RE.get_or_init(|| regex::Regex::new(r"(?-u:\b)num_ctx[ \t\n\r\x0b\x0c]+([0-9]+)").expect("a fixed pattern"));
    for f in ["parameters", "modelfile"] {
        if let Value::Str(s) = show.value(f) {
            if let Some(m) = re.captures(s) {
                return m[1].parse().map(Some).map_err(|_| RemoteErr::Unread);
            }
        }
    }
    Ok(None)
}

impl HostClient {
    fn probe_ollama(&mut self, base: &str) -> Result<Option<Probed>, RemoteErr> {
        let Some((200, b)) = self.send(&format!("{base}/api/tags"), None) else { return Ok(None) };
        let Some(Value::Obj(data)) = json_body(&b)? else { return Ok(None) };
        let Value::List(list) = data.value("models") else { return Ok(None) };
        let models = names_of(list, "name")?;
        let mut p = Probed::default();
        let Some(chosen) = models.first().cloned() else { return Ok(Some(p)) };
        p.chosen = Some(chosen.clone());
        let body = format!("{{\"model\":{}}}", dumps(&Value::Str(chosen), false));
        if let Some((200, b)) = self.send(&format!("{base}/api/show"), Some(body.as_bytes())) {
            if let Some(Value::Obj(show)) = json_body(&b)? {
                p.context = match num_ctx(&show)? {
                    Some(n) if n != 0 => Some(n),
                    _ => {
                        let mut found = None;
                        if let Value::Obj(info) = show.value("model_info") {
                            for (k, v) in info.iter() {
                                if k.ends_with(".context_length") {
                                    if let Some(n) = py_int(v)? {
                                        found = Some(n);
                                        break;
                                    }
                                }
                            }
                        }
                        found
                    }
                };
            }
        }
        Ok(Some(p))
    }

    fn probe_openai(&mut self, base: &str) -> Result<Option<Probed>, RemoteErr> {
        let Some((200, b)) = self.send(&format!("{base}/v1/models"), None) else { return Ok(None) };
        let Some(Value::Obj(data)) = json_body(&b)? else { return Ok(None) };
        let Value::List(entries) = data.value("data") else { return Ok(None) };
        let models = names_of(entries, "id")?;
        let mut p = Probed { chosen: models.first().cloned(), context: None };
        for e in entries {
            let Value::Obj(o) = e else { continue };
            // e.get("id") == chosen: chosen None matches an entry with no id.
            let matches = match (&p.chosen, o.value("id")) {
                (None, Value::Null) => true,
                (Some(c), Value::Str(s)) => c == s,
                _ => false,
            };
            if !matches {
                continue;
            }
            for k in OPENAI_CONTEXT_KEYS {
                if let Some(n) = py_int(o.value(k))? {
                    p.context = Some(n);
                    break;
                }
            }
            break;
        }
        Ok(Some(p))
    }

    fn probe_anthropic(&mut self, base: &str) -> Result<Option<Probed>, RemoteErr> {
        let body = format!(
            "{{\"model\":\"{PROBE_MODEL}\",\"max_tokens\":1,\"messages\":[{{\"role\":\"user\",\"content\":\"hi\"}}]}}"
        );
        let Some((200, b)) = self.send(&format!("{base}/v1/messages"), Some(body.as_bytes())) else {
            return Ok(None);
        };
        let Some(Value::Obj(data)) = json_body(&b)? else { return Ok(None) };
        if !matches!(data.value("type"), Value::Str(t) if t == "message") {
            return Ok(None);
        }
        Ok(Some(match data.value("model") {
            Value::Str(m) if !m.is_empty() && m != PROBE_MODEL => Probed { chosen: Some(m.clone()), context: None },
            _ => Probed::default(),
        }))
    }
}

/// `model_host.probe`.
fn probe(
    env: &std::collections::HashMap<String, String>,
    host: &str,
    port: Option<i64>,
    kind: &str,
) -> Result<HostInfo, RemoteErr> {
    if kind != "auto" && backend(kind).is_none() {
        return Err(RemoteErr::Value(format!(
            "unknown host kind {}; choose one of: auto, {}",
            repr(kind),
            KINDS.join(", ")
        )));
    }
    let base = base_url(host, port);
    let order: Vec<&str> = if kind == "auto" { KINDS.to_vec() } else { vec![kind] };
    let mut c = HostClient { proxies: netproxy::httpx_for(env), answered: false };
    for k in &order {
        let p = match *k {
            "ollama" => c.probe_ollama(&base)?,
            "openai" => c.probe_openai(&base)?,
            _ => c.probe_anthropic(&base)?,
        };
        let Some(p) = p else { continue };
        return Ok(HostInfo {
            base_url: base,
            kind: k.to_string(),
            chosen: p.chosen,
            context: p.context,
            warnings: context_floor_warning(p.context).into_iter().collect(),
            reachable: true,
        });
    }
    let w = if c.answered {
        format!(
            "could not identify a model server at {base}; it answered on none of: {}. Check the port, or pass --kind for the wire it speaks.",
            order.join(", ")
        )
    } else {
        format!("could not reach {base}; no request got a response. Check that the server runs and that this machine can reach it.")
    };
    Ok(HostInfo {
        base_url: base,
        kind: "unknown".into(),
        chosen: None,
        context: None,
        warnings: vec![w],
        reachable: c.answered,
    })
}

impl Env {
    /// The `--remote` branch of `tiers setup`.
    pub(super) fn setup_remote(
        &mut self,
        cmd: &str,
        data_dir: &str,
        remote: &str,
        kind: &str,
        model: Option<String>,
        context: Option<i64>,
    ) -> Res {
        let info = match parse_remote(remote).and_then(|(h, p)| probe(&self.env, &h, p, kind)) {
            Ok(i) => i,
            Err(RemoteErr::Unread) => return self.refuse(cmd, UNREAD),
            Err(RemoteErr::Value(w)) => {
                return Err(self.fail3(
                    &w,
                    "--remote takes a bare HOST[:PORT], and --kind takes one of the listed wires.",
                    "run: daisugi tiers setup --remote HOST[:PORT] --kind auto",
                    1,
                ))
            }
        };
        let Some(back) = backend(&info.kind) else {
            let reason =
                info.warnings.last().cloned().unwrap_or_else(|| format!("unknown host kind {}", repr(&info.kind)));
            return Err(self.fail3(
                &format!("cannot record {}: {reason}", info.base_url),
                "nothing was written. A recorded guess would poison the config.",
                "check that the host is reachable and serves Ollama, an OpenAI-compatible /v1, or an \
                 Anthropic-compatible /v1/messages. Or pass --kind explicitly.",
                if info.reachable { 1 } else { 3 },
            ));
        };
        let path = join(data_dir, "config.yaml");
        if let Err(e) = config::load(&path) {
            return self.refuse(cmd, &format!("{path} is not one this binary rewrites: {e}"));
        }
        let host_model = match model {
            Some(m) if !m.is_empty() => Some(m),
            _ => info.chosen.clone(),
        };
        let ctx = context.or(info.context);
        let opt_str = |v: &Option<String>| v.clone().map(Value::Str).unwrap_or(Value::Null);
        let update = vec![
            ("llm_base_url".to_string(), Value::Str(info.base_url.clone())),
            ("llm_host_kind".to_string(), Value::Str(info.kind.clone())),
            ("llm_host_model".to_string(), opt_str(&host_model)),
            ("llm_context_window".to_string(), ctx.map(|c| Value::Int(c.to_string())).unwrap_or(Value::Null)),
            ("llm_backend".to_string(), Value::Str(back.to_string())),
        ];
        match config::save(&path, &self.home, &update) {
            Ok(()) => {}
            Err(config::SaveErr::Io(e)) => return self.fail(cmd, &e.to_string()),
            Err(config::SaveErr::Config(e)) => {
                return self.refuse(cmd, &format!("{path} is not one this binary rewrites: {e}"))
            }
        }
        // describe_host
        let mut b = format!("host: {} ({})\n", info.base_url, info.kind);
        b.push_str(&format!("model: {}\n", host_model.as_deref().filter(|m| !m.is_empty()).unwrap_or("unset")));
        match ctx {
            Some(c) if c != 0 => b.push_str(&format!("context window: {c} tokens\n")),
            _ => b.push_str("context window: unknown\n"),
        }
        if let Some(w) = context_floor_warning(ctx) {
            b.push_str(&format!("warning: {w}\n"));
        }
        let env: Vec<(&str, String)> = match info.kind.as_str() {
            "ollama" => vec![("ANTHROPIC_BASE_URL", info.base_url.clone()), ("ANTHROPIC_AUTH_TOKEN", "ollama".into())],
            "anthropic" => vec![("ANTHROPIC_BASE_URL", info.base_url.clone())],
            _ => vec![],
        };
        if env.is_empty() {
            b.push_str(
                "this host speaks the OpenAI wire only. Point an OpenAI wire harness at it directly, \
                 or route it through `daisugi gateway --openai-upstream`.\n",
            );
        } else {
            b.push_str("point your harness at it:\n");
            for (k, v) in env {
                b.push_str(&format!("  {k}={v}\n"));
            }
            b.push_str(&format!(
                "or run: daisugi gateway --upstream {} and point the harness at the gateway. \
                 The gateway answers count_tokens itself. Most self-hosted servers do not.\n",
                info.base_url
            ));
        }
        self.echo(&b);
        Ok(())
    }
}
