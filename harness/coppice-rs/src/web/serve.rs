//! The phone server: the routes, the guard in front of them, and the
//! listener with its TLS, the CA hand-off, the event ring and the push
//! watcher. Run until the stop flag is set.

use std::io::BufReader;
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};

use super::events::EventRing;
use super::http::{self, Got, Request, Response, Stream};
use super::push::Publisher;
use super::token::{Banlist, TokenStore};
use super::upstream::{self, CallError, Dialer};
use crate::godec::Any;
use crate::server::ask::{self, AnswerError};

/// A view plugin the server serves at /plugins/<id>/.
#[derive(Clone)]
pub struct View {
    pub id: String,
    pub title: String,
    pub page: String,
    pub files: crate::plugins::Files,
    pub ring: bool,
}

/// The views among the loaded plugins.
pub fn views_of(ps: &[crate::plugins::Plugin]) -> Vec<View> {
    ps.iter()
        .filter(|p| p.m.kind == crate::plugins::KIND_VIEW)
        .map(|p| View {
            id: p.m.id.clone(),
            title: if p.m.title.is_empty() {
                p.m.id.clone()
            } else {
                p.m.title.clone()
            },
            page: p.m.page.clone(),
            files: p.files.clone(),
            ring: p.m.ring,
        })
        .collect()
}

/// Everything the server needs.
pub struct Options {
    pub dial: Dialer,
    pub tokens: TokenStore,
    pub gate_root: PathBuf,
    pub push: Option<Arc<Publisher>>,
    pub voice_url: String,
    pub voice_token_file: String,
    pub views: Vec<View>,
    pub events: Option<Arc<EventRing>>,
}

pub struct Server {
    pub opts: Options,
    bans: Banlist,
}

/// The refusal every guarded route gives a request from a view frame.
pub const VIEW_REFUSAL: &str = "A view holds no token. It gets its data from the floor page.";
const BAD_TOKEN: &str = "Bad token. Run coppice web token to see the current one.";
const ASK_GONE: &str = "That ask is gone. The gate timed out, or another client answered it.";
const NO_ANSWER: &str = "coppice-server did not answer. Run coppice server status.";

/// Go's json.NewEncoder(w).Encode(body): the JSON and a newline.
pub fn write_json(resp: &mut Response, status: u16, body: &Value) {
    resp.set("Content-Type", "application/json");
    resp.status = status;
    resp.body = format!("{}\n", crate::gojson::marshal(body)).into_bytes();
}

pub fn write_err(resp: &mut Response, status: u16, msg: &str) {
    write_json(
        resp,
        status,
        &crate::gojson::map(vec![("error", json!(msg))]),
    );
}

fn write_any(resp: &mut Response, status: u16, body: &Any) {
    resp.set("Content-Type", "application/json");
    resp.status = status;
    resp.body = format!("{}\n", body.marshal()).into_bytes();
}

/// The routes, by pattern.
enum Route {
    Ws,
    TokenCheck,
    Panes,
    AskAnswer,
    PushTest,
    VoiceTranscribe,
    VoiceStatus,
    VoiceRetry,
    ViewLib(String),
    ViewFile(String, String),
    Views,
    Tasks,
    Events,
    Static,
}

/// One registered pattern that matched a path: its method ("" for any),
/// whether it ends in a multi wildcard, its segment count, and its route.
struct Hit {
    method: &'static str,
    multi: bool,
    segments: usize,
    route: Route,
}

/// One fixed route: its method, its segments, and the route.
type Fixed = (&'static str, &'static [&'static str], fn() -> Route);

/// Every pattern that matches path (Go's cleaned, escaped form), most
/// specific first.
fn matches(path: &str) -> Vec<Hit> {
    let mut out = Vec::new();
    // The segments, each unescaped; a trailing slash is a last "".
    let raw: Vec<&str> = path[1..].split('/').collect();
    let segs: Vec<String> = raw
        .iter()
        .map(|s| http::path_unescape(s).unwrap_or_else(|| s.to_string()))
        .collect();
    let s: Vec<&str> = segs.iter().map(|x| x.as_str()).collect();
    let exact = |want: &[&str]| s == want;
    let fixed: &[Fixed] = &[
        ("", &["ws"], || Route::Ws),
        ("GET", &["api", "token", "check"], || Route::TokenCheck),
        ("GET", &["api", "panes"], || Route::Panes),
        ("POST", &["api", "ask", "answer"], || Route::AskAnswer),
        ("POST", &["api", "push", "test"], || Route::PushTest),
        ("POST", &["api", "voice", "transcribe"], || {
            Route::VoiceTranscribe
        }),
        ("GET", &["api", "voice", "status"], || Route::VoiceStatus),
        ("POST", &["api", "voice", "retry"], || Route::VoiceRetry),
        ("GET", &["api", "views"], || Route::Views),
        ("GET", &["api", "tasks"], || Route::Tasks),
        ("GET", &["api", "events"], || Route::Events),
    ];
    for (m, want, r) in fixed {
        if exact(want) {
            out.push(Hit {
                method: m,
                multi: false,
                segments: want.len(),
                route: r(),
            });
        }
    }
    if path == "/" {
        out.push(Hit {
            method: "GET",
            multi: false,
            segments: 1,
            route: Route::Static,
        });
    } else if super::statics::file(&s.join("/")).is_some() && !s.contains(&"") {
        out.push(Hit {
            method: "GET",
            multi: false,
            segments: s.len(),
            route: Route::Static,
        });
    }
    if s.len() >= 3 && s[0] == "plugins" {
        let rest =
            || http::path_unescape(&raw[2..].join("/")).unwrap_or_else(|| raw[2..].join("/"));
        if s[1] == "_lib" {
            out.push(Hit {
                method: "GET",
                multi: true,
                segments: 3,
                route: Route::ViewLib(rest()),
            });
        } else if !s[1].is_empty() {
            out.push(Hit {
                method: "GET",
                multi: true,
                segments: 3,
                route: Route::ViewFile(s[1].to_string(), rest()),
            });
        }
    }
    out
}

fn method_ok(pattern: &str, method: &str) -> bool {
    pattern.is_empty() || pattern == method || (pattern == "GET" && method == "HEAD")
}

/// Go's exactMatch.
fn exact_match(h: &Hit, path: &str) -> bool {
    if !h.multi {
        return true;
    }
    if !path.ends_with('/') {
        return false;
    }
    h.segments == path.matches('/').count()
}

/// Go's cleanPath.
fn clean_path(p: &str) -> String {
    if p.is_empty() {
        return "/".into();
    }
    let p = if p.starts_with('/') {
        p.to_string()
    } else {
        format!("/{p}")
    };
    let mut np = crate::sys::clean(&p);
    if p.ends_with('/') && np != "/" {
        np.push('/');
    }
    np
}

/// What the router chose for a request.
enum Choice {
    Route(Route),
    Redirect(String),
    NotAllowed(String),
    NotFound,
}

fn choose(req: &Request) -> Choice {
    let path = clean_path(&req.escaped_path);
    let pick = |p: &str| -> Option<Hit> {
        matches(p)
            .into_iter()
            .find(|h| method_ok(h.method, &req.method))
    };
    let n = pick(&path);
    let is_exact = n.as_ref().is_some_and(|h| exact_match(h, &path));
    if !is_exact && !path.ends_with('/') && !path.is_empty() {
        let with = format!("{path}/");
        if let Some(h2) = pick(&with) {
            if exact_match(&h2, &with) {
                return Choice::Redirect(with_query(&with, &req.raw_query));
            }
        }
    }
    if path != req.escaped_path {
        return Choice::Redirect(with_query(&path, &req.raw_query));
    }
    match n {
        Some(h) => Choice::Route(h.route),
        None => {
            let mut methods: Vec<&str> = Vec::new();
            for h in matches(&path) {
                match h.method {
                    "GET" => methods.extend(["GET", "HEAD"]),
                    m if !m.is_empty() => methods.push(m),
                    _ => {}
                }
            }
            methods.sort();
            methods.dedup();
            if methods.is_empty() {
                Choice::NotFound
            } else {
                Choice::NotAllowed(methods.join(", "))
            }
        }
    }
}

/// The Location of a mux redirect: the path as a URL writes it, and the
/// query.
fn with_query(path: &str, q: &str) -> String {
    let mut u = String::new();
    for &c in path.as_bytes() {
        let keep = c.is_ascii_alphanumeric()
            || matches!(
                c,
                b'-' | b'_'
                    | b'.'
                    | b'~'
                    | b'$'
                    | b'&'
                    | b'+'
                    | b','
                    | b'/'
                    | b':'
                    | b';'
                    | b'='
                    | b'@'
            );
        if keep {
            u.push(c as char);
        } else {
            u.push_str(&format!("%{c:02X}"));
        }
    }
    if !q.is_empty() {
        u.push('?');
        u.push_str(q);
    }
    u
}

/// The host part of the remote address.
fn client_addr(req: &Request) -> String {
    super::ip_string(&req.remote.ip())
}

/// What the server does with one request.
pub enum Outcome {
    Reply(Response),
    /// Upgrade to a websocket: write the 101, then serve it with the name
    /// the token carries.
    Upgrade(Response, String),
}

impl Server {
    pub fn new(opts: Options) -> Server {
        Server {
            opts,
            bans: Banlist::new(),
        }
    }

    /// The four headers every response carries, set before routing.
    fn base_response() -> Response {
        let mut r = Response::new(200);
        r.set("Cache-Control", "no-cache");
        r.set("X-Content-Type-Options", "nosniff");
        r.set("Content-Security-Policy", super::statics::CSP);
        r.set("Referrer-Policy", "no-referrer");
        r
    }

    pub fn handle(&self, req: &Request) -> Outcome {
        let mut resp = Server::base_response();
        let route = match choose(req) {
            Choice::Redirect(to) => {
                http::redirect(req, &mut resp, &to, 307);
                return Outcome::Reply(resp);
            }
            Choice::NotAllowed(allow) => {
                resp.set("Allow", &allow);
                http::error(&mut resp, "Method Not Allowed", 405);
                return Outcome::Reply(resp);
            }
            Choice::NotFound => {
                http::not_found(&mut resp);
                return Outcome::Reply(resp);
            }
            Choice::Route(r) => r,
        };
        match route {
            Route::Ws => {
                let Some(name) = self.guard(req, &mut resp) else {
                    return Outcome::Reply(resp);
                };
                match super::ws::accept(req, resp) {
                    Ok(r) => return Outcome::Upgrade(r, name),
                    Err(r) => {
                        super::log("WARN", "web: upgrade refused", &[("err", &upgrade_err(&r))]);
                        return Outcome::Reply(r);
                    }
                }
            }
            Route::AskAnswer => self.handle_ask_answer(req, &mut resp),
            Route::ViewFile(id, file) => self.handle_view_file(req, &mut resp, &id, &file),
            Route::ViewLib(file) => self.handle_view_lib(req, &mut resp, &file),
            Route::Static => serve_static(req, &mut resp),
            r => {
                let Some(name) = self.guard(req, &mut resp) else {
                    return Outcome::Reply(resp);
                };
                match r {
                    Route::TokenCheck => write_json(
                        &mut resp,
                        200,
                        &crate::gojson::map(vec![("name", json!(name)), ("ok", json!(true))]),
                    ),
                    Route::Panes => self.proxy_list(&mut resp, "pane.list"),
                    Route::Tasks => self.proxy_list(&mut resp, "task.list"),
                    Route::PushTest => self.handle_push_test(&mut resp),
                    Route::VoiceTranscribe => self.handle_voice_transcribe(req, &mut resp),
                    Route::VoiceStatus => self.handle_voice_status(req, &mut resp),
                    Route::VoiceRetry => self.handle_voice_retry(req, &mut resp),
                    Route::Views => self.handle_views(&mut resp),
                    Route::Events => self.handle_events(req, &mut resp),
                    _ => http::not_found(&mut resp),
                }
            }
        }
        Outcome::Reply(resp)
    }

    /// Refuses a request from a view frame: Origin null, whatever token.
    fn refuse_view(req: &Request, resp: &mut Response) -> bool {
        if req.header("Origin") != "null" {
            return false;
        }
        write_err(resp, 403, VIEW_REFUSAL);
        true
    }

    /// The guard: Some(name) for a good token ("" for the operator's own);
    /// None with the refusal written.
    fn guard(&self, req: &Request, resp: &mut Response) -> Option<String> {
        if Server::refuse_view(req, resp) {
            return None;
        }
        let addr = client_addr(req);
        if self.bans.banned(&addr) {
            super::log(
                "WARN",
                "web: refused a banned address",
                &[("addr", &addr), ("path", &req.path)],
            );
            write_err(resp, 429, "Too many bad tokens. Wait one minute.");
            return None;
        }
        let tok = super::ws::bearer_from(req);
        if tok.is_empty() {
            write_err(resp, 401, BAD_TOKEN);
            return None;
        }
        match self.opts.tokens.who(&tok) {
            Some(name) => Some(name),
            None => {
                self.bans.fail(&addr);
                super::log(
                    "WARN",
                    "web: refused a bad token",
                    &[("addr", &addr), ("path", &req.path)],
                );
                write_err(resp, 401, BAD_TOKEN);
                None
            }
        }
    }

    fn proxy_list(&self, resp: &mut Response, cmd: &str) {
        match upstream::call(&self.opts.dial, vec![("cmd", json!(cmd))]) {
            Err(CallError::Refused { code, message }) => {
                super::log(
                    "WARN",
                    &format!("web: {cmd} refused"),
                    &[("code", &code), ("message", &message)],
                );
                write_json(
                    resp,
                    502,
                    &crate::gojson::map(vec![
                        ("error", json!(format!("server refused: {message}"))),
                        ("code", json!(code)),
                    ]),
                );
            }
            Err(CallError::Failed(e)) => {
                super::log("WARN", &format!("web: {cmd} failed"), &[("err", &e)]);
                write_err(resp, 502, NO_ANSWER);
            }
            Ok(msg) => match msg.get("result") {
                Some(r @ Any::Map(_)) => write_any(resp, 200, r),
                _ => {
                    super::log(
                        "WARN",
                        &format!("web: {cmd} answered ok with a result the phone cannot read"),
                        &[],
                    );
                    write_err(
                        resp,
                        502,
                        "coppice-server sent a reply the phone could not read. Run coppice server status.",
                    );
                }
            },
        }
    }

    fn handle_push_test(&self, resp: &mut Response) {
        let Some(p) = &self.opts.push else {
            write_err(
                resp,
                409,
                "Push is off. Restart the server with --ntfy URL and --ntfy-topic NAME.",
            );
            return;
        };
        if p.test().is_err() {
            write_err(
                resp,
                502,
                "ntfy did not accept the message. Check the URL and the token.",
            );
            return;
        }
        write_json(resp, 200, &crate::gojson::map(vec![("ok", json!(true))]));
    }

    fn handle_views(&self, resp: &mut Response) {
        let out: Vec<Value> = self
            .opts
            .views
            .iter()
            .map(|v| {
                crate::gojson::map(vec![
                    ("id", json!(v.id)),
                    ("title", json!(v.title)),
                    ("ring", json!(v.ring)),
                ])
            })
            .collect();
        write_json(
            resp,
            200,
            &crate::gojson::map(vec![("views", Value::Array(out))]),
        );
    }

    fn handle_events(&self, req: &Request, resp: &mut Response) {
        let mut since = 0.0;
        if let Some(raw) = req.query("since").filter(|s| !s.is_empty()) {
            match go_parse_float(&raw) {
                Some(v) if !v.is_nan() && v.is_finite() && v >= 0.0 => since = v,
                _ => {
                    write_err(resp, 400, "since must be a time in seconds, 0 or more.");
                    return;
                }
            }
        }
        let ring = match &self.opts.events {
            Some(r) if r.live() => r,
            _ => {
                write_err(
                    resp,
                    503,
                    "The event ring is not running. Run coppice server status.",
                );
                return;
            }
        };
        let events = ring.since(since);
        let mut body = String::from("{\"events\":[");
        for (i, e) in events.iter().enumerate() {
            if i > 0 {
                body.push(',');
            }
            body.push_str(&crate::gojson::compact(&String::from_utf8_lossy(e)));
        }
        body.push_str("],\"from\":");
        body.push_str(&crate::gojson::float_text(ring.from()));
        body.push_str("}\n");
        resp.set("Content-Type", "application/json");
        resp.status = 200;
        resp.body = body.into_bytes();
    }

    fn handle_view_file(&self, req: &Request, resp: &mut Response, id: &str, file: &str) {
        resp.set("Content-Security-Policy", &super::statics::view_csp());
        resp.set("Access-Control-Allow-Origin", "*");
        let Some(v) = self.opts.views.iter().find(|v| v.id == id) else {
            http::not_found(resp);
            return;
        };
        let name = if file.is_empty() {
            v.page.as_str()
        } else {
            file
        };
        if !valid_path(name)
            || name
                .split('/')
                .any(|p| p.starts_with('_') || p.starts_with('.'))
        {
            http::not_found(resp);
            return;
        }
        match crate::plugins::read_file(&v.files, name) {
            Some((body, mtime)) => {
                let base = name.rsplit('/').next().unwrap_or(name);
                http::serve_content(req, resp, base, mtime, &body);
            }
            None => http::not_found(resp),
        }
    }

    fn handle_view_lib(&self, req: &Request, resp: &mut Response, file: &str) {
        resp.set("Content-Security-Policy", &super::statics::view_csp());
        resp.set("Access-Control-Allow-Origin", "*");
        if !valid_path(file)
            || file.contains('/')
            || !file.ends_with(".js")
            || file.starts_with('_')
            || file.starts_with('.')
        {
            http::not_found(resp);
            return;
        }
        match crate::plugins::lib_file(file) {
            Some(b) => http::serve_content(req, resp, file, None, b),
            None => http::not_found(resp),
        }
    }

    /// guard for the answer route, with one more door: a lock-screen deny
    /// token, which may deny its own ask and nothing else.
    fn guard_answer(&self, req: &Request, resp: &mut Response) -> Option<(String, Option<String>)> {
        if Server::refuse_view(req, resp) {
            return None;
        }
        let addr = client_addr(req);
        if self.bans.banned(&addr) {
            super::log(
                "WARN",
                "web: refused a banned address",
                &[("addr", &addr), ("path", &req.path)],
            );
            write_err(resp, 429, "Too many bad tokens. Wait one minute.");
            return None;
        }
        let tok = super::ws::bearer_from(req);
        if tok.is_empty() {
            write_err(resp, 401, BAD_TOKEN);
            return None;
        }
        if let Some(name) = self.opts.tokens.who(&tok) {
            return Some((name, None));
        }
        if let Some(p) = &self.opts.push {
            if let Some(ask) = p.deny_grant(&tok) {
                return Some((String::new(), Some(ask)));
            }
            if p.deny_retired(&tok) {
                write_err(resp, 409, ASK_GONE);
                return None;
            }
        }
        self.bans.fail(&addr);
        super::log(
            "WARN",
            "web: refused a bad token",
            &[("addr", &addr), ("path", &req.path)],
        );
        write_err(resp, 401, BAD_TOKEN);
        None
    }

    fn handle_ask_answer(&self, req: &Request, resp: &mut Response) {
        let Some((name, deny_only)) = self.guard_answer(req, resp) else {
            return;
        };
        let body = &req.body[..req.body.len().min(64 << 10)];
        let Some(b) = decode_answer(body) else {
            write_err(resp, 400, "The request body is not JSON.");
            return;
        };
        let reason = if b.reason.is_empty() {
            "answered from the phone".to_string()
        } else {
            b.reason.clone()
        };
        if let Some(hello) = super::peer::peer_hello(req.remote, req.local) {
            if !super::peer::place_allows(&self.opts.dial, hello) {
                super::log(
                    "WARN",
                    "web: refused an answer from a pane",
                    &[("remote", &req.remote.to_string())],
                );
                write_err(resp, 403, super::peer::PANE_REFUSAL);
                return;
            }
        }
        if let Some(only) = &deny_only {
            if b.decision != "deny" || b.tool_use_id != *only {
                write_err(resp, 403, "This card can only deny its own ask.");
                return;
            }
        }
        let mut reply = ask::Reply {
            tool_use_id: b.tool_use_id.clone(),
            decision: b.decision.clone(),
            reason,
            scope: b.scope.clone(),
            confirm: b.confirm.clone(),
            tier: String::new(),
            name: String::new(),
            by: String::new(),
            who_from: String::new(),
        };
        if !name.is_empty() {
            reply.by = name.clone();
            reply.who_from = "token".into();
        }
        if b.decision == "allow" {
            match lookup_ask(&self.opts.dial, &b.tool_use_id, &b.pane) {
                Ok(f) if f.held => {
                    write_err(resp, 409, &super::push::at_the_floor(&f.pane));
                    return;
                }
                Err(LookErr::NoAsk) => {
                    write_err(resp, 409, ASK_GONE);
                    return;
                }
                Err(LookErr::Other(e)) => {
                    super::log("WARN", "web: could not read the ask's tier", &[("err", &e)]);
                    write_err(resp, 502, NO_ANSWER);
                    return;
                }
                Ok(f) => {
                    reply.tier = f.tier;
                    reply.name = f.name;
                }
            }
        }
        let res = ask::answer(&self.opts.gate_root, &reply);
        if let Err(AnswerError::NoAsk) = &res {
            if let Ok(f) = lookup_ask(&self.opts.dial, &b.tool_use_id, &b.pane) {
                if f.held {
                    write_err(resp, 409, &super::push::at_the_floor(&f.pane));
                    return;
                }
            }
        }
        match res {
            Err(AnswerError::BadDecision) => write_err(resp, 400, "Send decision allow or deny."),
            Err(AnswerError::BadScope) => write_err(resp, 400, "Send scope once or task."),
            Err(AnswerError::NoAsk) => write_err(resp, 409, ASK_GONE),
            Err(e @ AnswerError::NeedsName(_)) => write_err(resp, 403, &e.to_string()),
            Err(e @ AnswerError::PermanentTask) => {
                write_err(resp, 403, &format!("{e} with the pane name."))
            }
            Err(AnswerError::Io(e)) => {
                super::log("ERROR", "web: could not write the answer", &[("err", &e)]);
                write_err(
                    resp,
                    500,
                    "The answer could not be written. Check that the gate directory exists.",
                );
            }
            Ok(()) => {
                if let Some(p) = &self.opts.push {
                    p.retire_deny(&reply.tool_use_id);
                }
                super::log(
                    "INFO",
                    "web: answered an ask",
                    &[
                        ("decision", &reply.decision),
                        ("tool_use_id", &reply.tool_use_id),
                    ],
                );
                write_json(resp, 200, &crate::gojson::map(vec![("ok", json!(true))]));
            }
        }
    }
}

/// The words of a refused upgrade, from the refusal's body.
fn upgrade_err(r: &Response) -> String {
    format!(
        "failed to accept WebSocket connection: {}",
        String::from_utf8_lossy(&r.body).trim_end()
    )
}

/// Go's fs.ValidPath.
fn valid_path(name: &str) -> bool {
    if name == "." {
        return true;
    }
    !name.is_empty()
        && name
            .split('/')
            .all(|e| !e.is_empty() && e != "." && e != "..")
}

/// Go's strconv.ParseFloat(s, 64) for the forms a client sends: None for
/// an error, which an out-of-range value also is.
fn go_parse_float(s: &str) -> Option<f64> {
    let t = s.trim_start_matches(['+', '-']);
    let lower = t.to_ascii_lowercase();
    if lower == "inf" || lower == "infinity" {
        return Some(if s.starts_with('-') {
            f64::NEG_INFINITY
        } else {
            f64::INFINITY
        });
    }
    if lower == "nan" {
        return Some(f64::NAN);
    }
    if !s
        .bytes()
        .all(|b| b.is_ascii_digit() || matches!(b, b'.' | b'e' | b'E' | b'+' | b'-'))
    {
        return None;
    }
    let v: f64 = s.parse().ok()?;
    // Go refuses a value too large for a float64 with ErrRange.
    if v.is_infinite() {
        return None;
    }
    Some(v)
}

/// The answer route's body.
struct AnswerBody {
    tool_use_id: String,
    decision: String,
    reason: String,
    confirm: String,
    scope: String,
    pane: String,
}

static ANSWER_TY: crate::godec::StructTy = crate::godec::StructTy {
    name: "",
    text: "struct",
    fields: &[
        ("tool_use_id", crate::godec::Ty::Str),
        ("decision", crate::godec::Ty::Str),
        ("reason", crate::godec::Ty::Str),
        ("confirm", crate::godec::Ty::Str),
        ("scope", crate::godec::Ty::Str),
        ("pane", crate::godec::Ty::Str),
    ],
};

/// Go's json.Decoder.Decode of the first value of body into the answer
/// struct. None for any error.
fn decode_answer(body: &[u8]) -> Option<AnswerBody> {
    let first = first_value(body)?;
    let (g, err) = crate::godec::decode(first, &crate::godec::Ty::Struct(&ANSWER_TY));
    if err.is_some() {
        return None;
    }
    Some(AnswerBody {
        tool_use_id: g.f("tool_use_id").s(),
        decision: g.f("decision").s(),
        reason: g.f("reason").s(),
        confirm: g.f("confirm").s(),
        scope: g.f("scope").s(),
        pane: g.f("pane").s(),
    })
}

/// The bytes of the first JSON value in body, as a json.Decoder reads it:
/// leading space skipped, the value up to its end. None when body holds
/// no whole value.
fn first_value(body: &[u8]) -> Option<&[u8]> {
    let start = body.iter().position(|b| !b" \t\r\n".contains(b))?;
    let b = &body[start..];
    let mut depth = 0i32;
    let mut in_str = false;
    let mut esc = false;
    for (i, &c) in b.iter().enumerate() {
        if in_str {
            if esc {
                esc = false;
            } else if c == b'\\' {
                esc = true;
            } else if c == b'"' {
                in_str = false;
                if depth == 0 {
                    return Some(&b[..=i]);
                }
            }
            continue;
        }
        match c {
            b'"' => in_str = true,
            b'{' | b'[' => depth += 1,
            b'}' | b']' => {
                depth -= 1;
                if depth == 0 {
                    return Some(&b[..=i]);
                }
            }
            b' ' | b'\t' | b'\r' | b'\n' | b',' if depth == 0 => return Some(&b[..i]),
            _ => {}
        }
    }
    if depth == 0 && !in_str {
        return Some(b);
    }
    None
}

/// What an allow needs to know about an ask.
struct AskFacts {
    pane: String,
    tier: String,
    name: String,
    held: bool,
}

enum LookErr {
    NoAsk,
    Other(String),
}

/// Finds the pane whose state shows ask id, through agent.list.
fn lookup_ask(d: &Dialer, id: &str, pane: &str) -> Result<AskFacts, LookErr> {
    if id.is_empty() {
        return Err(LookErr::NoAsk);
    }
    let msg = upstream::call(d, vec![("cmd", json!("agent.list"))])
        .map_err(|e| LookErr::Other(e.to_string()))?;
    let Some(result @ Any::Map(_)) = msg.get("result") else {
        return Err(LookErr::Other("agent.list answered with no result".into()));
    };
    let Some(Any::Arr(agents)) = result.get("agents") else {
        return Err(LookErr::Other(
            "agent.list answered with no agents list".into(),
        ));
    };
    for row in agents {
        let Some(a @ Any::Map(_)) = row.get("ask") else {
            continue;
        };
        if !matches!(row.get("state"), Some(Any::Str(s)) if s == "blocked") {
            continue;
        }
        let pane_id = row.str_of("pane");
        if a.str_of("id") != id || pane_id.is_empty() || (!pane.is_empty() && pane_id != pane) {
            continue;
        }
        let label = row.str_of("label");
        return Ok(AskFacts {
            pane: pane_id.clone(),
            tier: a.str_of("tier"),
            name: if label.is_empty() { pane_id } else { label },
            held: a.str_of("holder") == "harness",
        });
    }
    Err(LookErr::NoAsk)
}

/// The page itself: Go's FileServer over the embedded files.
fn serve_static(req: &Request, resp: &mut Response) {
    if req.path.ends_with("/index.html") {
        let mut to = "./".to_string();
        if !req.raw_query.is_empty() {
            to.push('?');
            to.push_str(&req.raw_query);
        }
        resp.set("Location", &to);
        resp.status = 301;
        return;
    }
    let name = crate::sys::clean(&req.path);
    let rel = name.trim_start_matches('/');
    let (file, base) = if rel.is_empty() {
        ("index.html".to_string(), "index.html".to_string())
    } else {
        (
            rel.to_string(),
            rel.rsplit('/').next().unwrap_or(rel).to_string(),
        )
    };
    match super::statics::file(&file) {
        Some(b) => http::serve_content(req, resp, &base, None, b),
        None => http::not_found(resp),
    }
}

/// The phone server's settings, as web.json and the flags give them.
pub use super::config::Config;

/// A handle that stops what serve started.
pub struct Running {
    pub stop: Arc<AtomicBool>,
}

/// Serves until stop is set. The caller resolved every path.
pub fn serve(c: &Config, mut opts: Options, stop: Arc<AtomicBool>) -> Result<(), String> {
    let listen = if c.listen.is_empty() {
        ":8443".to_string()
    } else {
        c.listen.clone()
    };
    if !c.gate_root.is_empty() {
        opts.gate_root = PathBuf::from(&c.gate_root);
    }
    if !c.voice_url.is_empty() {
        opts.voice_url = c.voice_url.clone();
        opts.voice_token_file = c.voice_token_file.clone();
    }
    let source = if c.tls.is_empty() {
        super::tls::LOCALCA
    } else {
        c.tls.as_str()
    };
    let (cert_file, key_file) = super::tls::resolve(&super::tls::Options {
        source,
        cert_file: &c.cert_file,
        key_file: &c.key_file,
        ca_dir: &c.ca_dir,
        listen: &listen,
    })?;
    if !cert_file.is_empty() {
        let not_after = super::localca::load_leaf(std::path::Path::new(&cert_file))?;
        let warn = super::localca::expiry_warning(not_after, super::localca::now().0);
        if !warn.is_empty() {
            super::log("WARN", &format!("web: {warn}"), &[]);
        }
    }
    if !c.ntfy.is_empty() && !c.ntfy_topic.is_empty() {
        let p = Arc::new(Publisher::new(super::push::Config {
            base_url: c.ntfy.clone(),
            topic: c.ntfy_topic.clone(),
            token_env: c.ntfy_token_env.clone(),
            click_base: c.external_url.clone(),
        })?);
        opts.push = Some(p.clone());
        let d = opts.dial.clone();
        let st = stop.clone();
        std::thread::spawn(move || {
            if let Err(e) = p.watch(d, st.clone()) {
                if !st.load(Ordering::SeqCst) {
                    super::log("WARN", "web: the push watcher stopped", &[("err", &e)]);
                }
            }
        });
    }
    if opts.events.is_none() {
        let ring = Arc::new(EventRing::new());
        opts.events = Some(ring.clone());
        let d = opts.dial.clone();
        let st = stop.clone();
        std::thread::spawn(move || ring.run(d, st));
    }
    if source == super::tls::LOCALCA && !c.ca_listen.is_empty() && c.ca_listen != "off" {
        let pem = super::localca::CaDir::new(PathBuf::from(&c.ca_dir)).ca_pem()?;
        let ca_listen = c.ca_listen.clone();
        // Go logs this from the hand-off's own goroutine, which in
        // practice runs before the phone server logs (CP-R-33).
        super::log(
            "INFO",
            "web: serving the CA certificate",
            &[("listen", &ca_listen), ("path", "/ca.crt")],
        );
        let st = stop.clone();
        std::thread::spawn(move || {
            if let Err(e) = serve_ca_cert(&ca_listen, pem, st.clone()) {
                if !st.load(Ordering::SeqCst) {
                    super::log("WARN", "web: the CA hand-off stopped", &[("err", &e)]);
                }
            }
        });
    }
    let tls = if source == super::tls::OFF {
        None
    } else {
        Some(super::tls::server_config(&cert_file, &key_file)?)
    };
    let gate = opts.gate_root.to_string_lossy().into_owned();
    let server = Arc::new(Server::new(opts));
    super::log(
        "INFO",
        "web: serving the phone",
        &[("listen", &listen), ("tls", source), ("gate_root", &gate)],
    );
    let ln = listen_tcp(&listen)?;
    accept_loop(ln, &stop, |stream| {
        let s = server.clone();
        let tls = tls.clone();
        std::thread::spawn(move || serve_conn(stream, &s, tls));
    });
    Ok(())
}

/// Go's net.Listen("tcp", addr), with its words for a failure.
pub fn listen_tcp(addr: &str) -> Result<TcpListener, String> {
    let (host, port) = super::split_host_port(addr).map_err(|e| format!("listen tcp: {e}"))?;
    let port: u16 = port
        .parse()
        .map_err(|_| format!("listen tcp: lookup port {port}: invalid port"))?;
    let words =
        |e: std::io::Error| format!("listen tcp {addr}: bind: {}", crate::sys::go_errno_text(&e));
    if host.is_empty() {
        return TcpListener::bind(("::", port))
            .or_else(|_| TcpListener::bind(("0.0.0.0", port)))
            .map_err(words);
    }
    let ip: std::net::IpAddr = match super::parse_ip(&host) {
        Some(ip) => ip,
        None => {
            use std::net::ToSocketAddrs;
            (host.as_str(), port)
                .to_socket_addrs()
                .map_err(|_| format!("listen tcp: lookup {host}: no such host"))?
                .next()
                .map(|a| a.ip())
                .ok_or_else(|| format!("listen tcp: lookup {host}: no such host"))?
        }
    };
    TcpListener::bind(SocketAddr::new(ip, port)).map_err(words)
}

/// Accepts until stop is set, polling so a stop is seen within 100 ms.
fn accept_loop(ln: TcpListener, stop: &AtomicBool, mut each: impl FnMut(TcpStream)) {
    let _ = ln.set_nonblocking(true);
    while !stop.load(Ordering::SeqCst) {
        match ln.accept() {
            Ok((s, _)) => {
                let _ = s.set_nonblocking(false);
                each(s);
            }
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(20));
            }
            Err(_) => std::thread::sleep(Duration::from_millis(20)),
        }
    }
}

/// One connection: requests until it closes, or one websocket.
fn serve_conn(tcp: TcpStream, server: &Server, tls: Option<Arc<rustls::ServerConfig>>) {
    let (Ok(remote), Ok(local)) = (tcp.peer_addr(), tcp.local_addr()) else {
        return;
    };
    let Ok(ctl) = tcp.try_clone() else {
        return;
    };
    let _ = tcp.set_read_timeout(Some(Duration::from_secs(10)));
    let stream: Box<dyn Stream> = match tls {
        Some(cfg) => {
            let Ok(conn) = rustls::ServerConnection::new(cfg) else {
                return;
            };
            Box::new(rustls::StreamOwned::new(conn, tcp))
        }
        None => Box::new(tcp),
    };
    let mut r = BufReader::new(stream);
    loop {
        let mut req = match http::read_request(&mut r, remote, local) {
            Ok(Got::Req(req)) => req,
            Ok(Got::Bad(why)) => {
                let _ = http::write_bad_request(r.get_mut(), why);
                return;
            }
            _ => return,
        };
        if http::wants_continue(&req) {
            use std::io::Write;
            let w = r.get_mut();
            if w.write_all(b"HTTP/1.1 100 Continue\r\n\r\n")
                .and_then(|_| w.flush())
                .is_err()
            {
                return;
            }
        }
        if http::read_body(&mut r, &mut req).is_err() {
            return;
        }
        match server.handle(&req) {
            Outcome::Reply(resp) => match http::write_response(r.get_mut(), &req, resp) {
                Ok(false) => continue,
                _ => {
                    let _ = ctl.shutdown(std::net::Shutdown::Write);
                    return;
                }
            },
            Outcome::Upgrade(resp, name) => {
                if http::write_response(r.get_mut(), &req, resp).is_err() {
                    return;
                }
                let set = |t: Option<Duration>| {
                    let _ = ctl.set_read_timeout(t);
                };
                super::ws::serve(r, &server.opts.dial, &name, &req, &set);
                let _ = ctl.shutdown(std::net::Shutdown::Both);
                return;
            }
        }
    }
}

/// The CA hand-off: plain HTTP that serves /ca.crt and nothing else.
fn serve_ca_cert(listen: &str, pem: Vec<u8>, stop: Arc<AtomicBool>) -> Result<(), String> {
    let ln = listen_tcp(listen)?;
    let pem = Arc::new(pem);
    accept_loop(ln, &stop, |tcp| {
        let pem = pem.clone();
        std::thread::spawn(move || {
            let (Ok(remote), Ok(local)) = (tcp.peer_addr(), tcp.local_addr()) else {
                return;
            };
            let _ = tcp.set_read_timeout(Some(Duration::from_secs(5)));
            let mut r = BufReader::new(Box::new(tcp) as Box<dyn Stream>);
            loop {
                let mut req = match http::read_request(&mut r, remote, local) {
                    Ok(Got::Req(req)) => req,
                    Ok(Got::Bad(why)) => {
                        let _ = http::write_bad_request(r.get_mut(), why);
                        return;
                    }
                    _ => return,
                };
                if http::read_body(&mut r, &mut req).is_err() {
                    return;
                }
                let mut resp = Response::new(200);
                if req.path != "/ca.crt" {
                    http::not_found(&mut resp);
                } else {
                    resp.set("Content-Type", "application/x-x509-ca-cert");
                    resp.set(
                        "Content-Disposition",
                        "attachment; filename=\"coppice-ca.crt\"",
                    );
                    resp.set("Cache-Control", "no-store");
                    resp.set("X-Content-Type-Options", "nosniff");
                    resp.body = pem.to_vec();
                }
                match http::write_response(r.get_mut(), &req, resp) {
                    Ok(false) => continue,
                    _ => return,
                }
            }
        });
    });
    Ok(())
}

/// What the coppice server calls once its socket is up: starts the phone
/// server from web.json when it is enabled. Err when the file does not
/// read. The returned flag stops it.
pub fn auto_start(config_path: &std::path::Path, opts: Options) -> Result<Running, String> {
    let c = super::config::load(config_path)?;
    let stop = Arc::new(AtomicBool::new(false));
    if !c.enabled {
        return Ok(Running { stop });
    }
    let st = stop.clone();
    std::thread::spawn(move || {
        let res = serve(&c, opts, st.clone());
        if let Err(e) = res {
            if !st.load(Ordering::SeqCst) {
                super::log("ERROR", "web: the phone server did not run", &[("err", &e)]);
            }
        }
        // A failed listen must not leave the hand-off or the watchers
        // running.
        st.store(true, Ordering::SeqCst);
    });
    Ok(Running { stop })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clean_path_keeps_a_trailing_slash() {
        assert_eq!(clean_path("/a/../b/"), "/b/");
        assert_eq!(clean_path("//app.js"), "/app.js");
        assert_eq!(clean_path("/"), "/");
    }

    #[test]
    fn first_value_reads_one_value() {
        assert_eq!(
            first_value(b" {\"a\":\"}\"} tail").unwrap(),
            b"{\"a\":\"}\"}"
        );
        assert!(first_value(b"{\"a\":").is_none());
        assert_eq!(first_value(b"true x").unwrap(), b"true");
    }

    #[test]
    fn floats_parse_as_go_parses_them() {
        assert_eq!(go_parse_float("1.5"), Some(1.5));
        assert_eq!(go_parse_float("1e400"), None);
        assert!(go_parse_float("NaN").unwrap().is_nan());
        assert_eq!(go_parse_float("x"), None);
    }
}
