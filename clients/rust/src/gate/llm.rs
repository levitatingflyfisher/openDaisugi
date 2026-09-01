//! `llm_check.run_llm_check` on the HTTP backend: the model call an
//! llm_check predicate makes, one plain call through the model client
//! (`crate::llm::wire`, the oracle's `llm_client.py`), the verdict read from
//! the reply, and every failure worded as the oracle words it, since the
//! failure text becomes the deny reason (LLM-12). The Go gate's `llm.go` is
//! the reference.
//!
//! An https URL goes over rustls (the ring provider) and trusts the
//! system's own root store, as the Go gate trusts Go's.
//!
//! Not decided here: a call through the claude-code backend (`claude -p`),
//! settings that change how httpx calls the model, and a reply read in a
//! way the port does not model.

use super::config::load_field;
use super::dispatch::py_which;
use super::paths::path_join;
use super::pyjson::{dumps, loads, py_decode_error, py_str, py_type_name, Object, PyDecodeFail, Value};
use super::{catch, undecided, Runner, R};
use crate::llm::wire;
use std::io::{Read, Write};
use std::sync::{Arc, OnceLock};
use std::time::Duration;

/// As the Go gate's llmJSONDepth.
const LLM_JSON_DEPTH: usize = 900;
const DEFAULT_MODEL: &str = "anthropic/claude-haiku-4-5-20251001";
const SYSTEM: &str = r#"You are a strict verifier. Answer in strict JSON: {"satisfied": true|false, "rationale": "short reason"}. No prose outside the JSON."#;

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
            return Ok("api".into());
        }
        if py_which("claude", self.env.get("PATH").map(String::as_str)).is_some() {
            return Ok("claude-code".into());
        }
        Ok("api".into())
    }

    /// A setting that makes httpx behave in a way the port does not model.
    fn llm_unported_env(&self) -> Option<String> {
        ["SSL_CERT_FILE", "SSL_CERT_DIR"].iter().find(|k| self.env.get(**k).is_some_and(|v| !v.is_empty())).map(|k| k.to_string())
    }

    /// `llm_check._invoke_model` on the HTTP backend.
    fn invoke_model(&self, rule: &str, payload: &Object) -> R<Out<(bool, String)>> {
        let model = self.env.get("OPENDAISUGI_LLM_CHECK_MODEL").cloned().unwrap_or_else(|| DEFAULT_MODEL.into());
        let backend = self.llm_backend()?;
        if crate::llm::renamed(&backend) {
            // resolve_backend raises on the old name: the check fails closed.
            return Ok(Out::Fail(crate::llm::RENAMED_TEXT.into()));
        }
        if backend == "claude-code" {
            return undecided("an llm_check through the claude-code backend, which runs claude -p");
        }
        if let Some(k) = self.llm_unported_env() {
            return undecided(format!("an llm_check under {k}, which changes how httpx calls the model"));
        }
        let pj: String = dumps(&Value::Obj(payload.clone()), true).chars().take(4000).collect();
        let user = format!("Rule:\n{rule}\n\nPlan payload (JSON):\n{pj}\n\nDoes the plan payload satisfy the rule?");
        let get = |k: &str| self.env.get(k).cloned();
        let w = match wire::resolve(&model, "", "", &get) {
            Ok(w) => w,
            Err(m) => return Ok(Out::Fail(m)),
        };
        let msgs = vec![("system".to_string(), SYSTEM.to_string()), ("user".to_string(), user)];
        let body = wire::body(&w, &msgs, &wire::Opts { max_tokens: Some(200), temperature: Some(0), json_object: false, ..Default::default() });
        let headers: Vec<(String, String)> = w.headers.iter().map(|(k, v)| (k.to_string(), v.clone())).collect();
        let proxies = crate::netproxy::Httpx::from_vars(&crate::netproxy::map_vars(&self.env), false);
        let (status, text) = tryo!(post(&w.url, &headers, body.as_bytes(), wire::timeout(&get), &proxies, None)?);
        let content = match wire::read_reply(&w, status, &text) {
            Ok(r) => r.text,
            Err(m) => return Ok(Out::Fail(m)),
        };
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

/// One HTTP/1.1 POST, as the model client sends it: no redirects followed,
/// its timeout (the gate's --verify-timeout usually ends the wait first),
/// through a proxy as httpx picks one. `roots` replaces the system store,
/// for the tests only.
fn post(
    url: &str,
    headers: &[(String, String)],
    body: &[u8],
    timeout: f64,
    proxies: &crate::netproxy::Httpx,
    roots: Option<Arc<rustls::RootCertStore>>,
) -> R<Out<(u16, String)>> {
    use crate::netproxy;
    let (tls, host, port, path) = target(url)?;
    if tls && roots.is_none() {
        system_roots()?;
    }
    let origin = netproxy::Origin { tls, host: host.clone(), port, explicit_port: true };
    let route = match proxies.route(&origin) {
        Ok(r) => r,
        Err(netproxy::Refusal::Fails(t)) => return Ok(Out::Fail(wire::clean(&format!("the model call could not start: {t}")))),
        Err(netproxy::Refusal::Unported(w)) => return undecided(format!("an llm_check under a proxy setting the port does not read ({w})")),
    };
    let limit = Duration::from_secs_f64(timeout.min(1e9));
    let dial = netproxy::Dial { limit: None, connect_timeout: Some(limit), roots };
    let failed = |e: &std::io::Error| {
        let text = if matches!(e.kind(), std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock) {
            format!("the model call to {url} timed out")
        } else {
            format!("could not reach the model server at {url}")
        };
        Ok(Out::Fail(wire::clean(&text)))
    };
    let mut conn = match netproxy::open(&route, &origin, &dial) {
        Ok(c) => c,
        Err(netproxy::Fail::Connect(e) | netproxy::Fail::Io(e)) => return failed(&e),
        Err(netproxy::Fail::Status(code, reason)) => {
            return Ok(Out::Fail(format!("the proxy refused the model call: {code} {reason}")))
        }
    };
    let _ = conn.sock.set_read_timeout(Some(limit));
    let _ = conn.sock.set_write_timeout(Some(limit));
    let default_port = if tls { 443 } else { 80 };
    let host_header = if port == default_port { host.clone() } else { format!("{host}:{port}") };
    let line = netproxy::request_target(&conn, &origin, &path);
    let mut req = format!("POST {line} HTTP/1.1\r\n{}host: {host_header}\r\n", netproxy::auth_line(&conn));
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    req.push_str(&format!("content-length: {}\r\nconnection: close\r\n\r\n", body.len()));
    let mut out = req.into_bytes();
    out.extend_from_slice(body);
    let mut raw = Vec::new();
    if let Err(e) = conn.io.write_all(&out).and_then(|_| conn.io.flush()).and_then(|_| conn.io.read_to_end(&mut raw).map(|_| ())) {
        return failed(&e);
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
    let mut chunked = false;
    let mut length: Option<usize> = None;
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            let (k, v) = (k.trim().to_ascii_lowercase(), v.trim().to_string());
            match k.as_str() {
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
    // The model client decodes the bytes as UTF-8 with replacement,
    // whatever the content type says. A BOM or a NUL is left undecided:
    // how json.loads reads them is not modelled here.
    if body.starts_with(b"\xef\xbb\xbf") || body.contains(&0) {
        return undecided("an llm_check reply in an encoding the port does not read");
    }
    Ok(Out::Ok((status, super::py::text::decode_utf8_replace(&body))))
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
        post(&format!("{scheme}://{host}:{port}/v1/messages"), &headers(), b"{\"a\": 1}", 600.0, &crate::netproxy::Httpx::none(), Some(roots.clone()))
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

    /// A proxy that answers each connection with `reply` and returns
    /// what it read up to the blank line.
    fn fake_proxy(reply: &'static [u8]) -> (u16, JoinHandle<String>) {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        let h = std::thread::spawn(move || {
            let (mut s, _) = l.accept().unwrap();
            let mut buf = vec![];
            let mut b = [0u8; 4096];
            while !buf.windows(4).any(|w| w == b"\r\n\r\n") {
                let n = s.read(&mut b).unwrap();
                if n == 0 {
                    break;
                }
                buf.extend_from_slice(&b[..n]);
            }
            s.write_all(reply).unwrap();
            let text = String::from_utf8_lossy(&buf).into_owned();
            text[..text.find("\r\n\r\n").unwrap_or(text.len())].to_string()
        });
        (port, h)
    }

    #[test]
    fn a_proxy_is_used_as_httpx_uses_it() {
        let ca = ca_for("localhost");
        let vars = |k: &str, port: u16| vec![(k.to_string(), format!("http://u:p@127.0.0.1:{port}"))];
        // https: a CONNECT, and a refusal worded as the model client words it.
        let (port, h) = fake_proxy(b"HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\n\r\n");
        let proxies = crate::netproxy::Httpx::from_vars(&vars("HTTPS_PROXY", port), true);
        let r = post("https://API.localhost/v1/messages", &headers(), b"{}", 600.0, &proxies, Some(ca.roots.clone()));
        assert_eq!(
            h.join().unwrap(),
            "CONNECT api.localhost:443 HTTP/1.1\r\nHost: api.localhost:443\r\nAccept: */*\r\nProxy-Authorization: Basic dTpw"
        );
        assert_eq!(show(r), "fail the proxy refused the model call: 403 Forbidden");
        // http: the whole URL on the request line, the credentials first.
        let reply = b"HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 2\r\n\r\n{}";
        let (port, h) = fake_proxy(reply);
        let proxies = crate::netproxy::Httpx::from_vars(&vars("HTTP_PROXY", port), true);
        let r = post("http://127.0.0.1:9/v1/messages", &headers(), b"{}", 600.0, &proxies, None);
        let req = h.join().unwrap();
        assert!(req.starts_with("POST http://127.0.0.1:9/v1/messages HTTP/1.1\r\nProxy-Authorization: Basic dTpw\r\nhost: 127.0.0.1:9\r\n"), "{req}");
        assert_eq!(show(r), "ok 200 {}");
    }

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
    fn an_untrusted_or_misnamed_server_cannot_be_reached() {
        let ca = ca_for("localhost");
        let other = ca_for("localhost");
        let (p, h) = serve(Some(ca.server.clone()), http("200 OK", "", MSG), true);
        let got = show(call("https", "localhost", p, &other.roots));
        assert!(got.starts_with("fail could not reach the model server at https://"), "{got}");
        h.join().unwrap();
        let (p, h) = serve(Some(ca.server.clone()), http("200 OK", "", MSG), true);
        let got = show(call("https", "127.0.0.1", p, &ca.roots));
        assert!(got.starts_with("fail could not reach the model server at https://"), "{got}");
        h.join().unwrap();
    }

    #[test]
    fn a_refused_https_port_cannot_be_reached() {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let p = l.local_addr().unwrap().port();
        drop(l);
        let ca = ca_for("localhost");
        assert_eq!(show(call("https", "127.0.0.1", p, &ca.roots)), format!("fail could not reach the model server at https://127.0.0.1:{p}/v1/messages"));
    }

    #[test]
    fn the_default_https_port_is_443() {
        let (tls, host, port, path) = target("https://api.anthropic.com/v1/messages").unwrap_or_else(|_| panic!());
        assert!(tls);
        assert_eq!((host.as_str(), port, path.as_str()), ("api.anthropic.com", 443, "/v1/messages"));
    }
}
