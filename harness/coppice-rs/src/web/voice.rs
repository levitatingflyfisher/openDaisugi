//! Voice for the page: where a recorded clip goes and the bearer it
//! carries, what the page says when voice cannot run, and the relay of
//! the voice server's reply.

use serde_json::{json, Value};

use super::http::{Request, Response};
use super::serve::{write_err, write_json, Server};
use super::upstream::{self, CallError};
use crate::godec::Any;

/// The largest clip forwarded.
pub const MAX_VOICE_CLIP_BYTES: usize = 64 << 20;

/// The cap used until the voice server says its own (a voice server built
/// before /health named it). The voice server owns the number: `voice_cap`
/// reads it from /health.
pub const VOICE_MAX_CLIP_SECONDS: f64 = 1200.0;

/// The longest clip the voice server at `target` takes, from its /health
/// reply. It falls back to `VOICE_MAX_CLIP_SECONDS` when the server does not
/// answer or does not name a cap.
fn voice_cap(target: Option<&Target>) -> f64 {
    let Some(t) = target else { return VOICE_MAX_CLIP_SECONDS };
    let url = format!("{}/health", t.url.trim_end_matches('/'));
    let Ok(r) = super::client::send("GET", &url, &[], b"", std::time::Duration::from_secs(2)) else {
        return VOICE_MAX_CLIP_SECONDS;
    };
    serde_json::from_slice::<Value>(&r.body)
        .ok()
        .and_then(|v| v.get("max_seconds").and_then(Value::as_f64))
        .filter(|n| *n > 0.0)
        .unwrap_or(VOICE_MAX_CLIP_SECONDS)
}

/// What the page learns about voice.
#[derive(Default)]
pub struct State {
    pub ready: bool,
    pub state: String,
    pub message: String,
    pub action: String,
    pub command: String,
    pub away: bool,
    /// `{done, total}` in bytes while voice, starting, gets its speech model.
    pub fetch: Option<Value>,
    /// The longest clip the voice server takes, in seconds.
    pub max_seconds: f64,
}

impl State {
    fn json(&self) -> Value {
        let mut pairs = vec![
            ("ready", json!(self.ready)),
            ("state", json!(self.state)),
            ("message", json!(self.message)),
        ];
        if !self.action.is_empty() {
            pairs.push(("action", json!(self.action)));
        }
        if !self.command.is_empty() {
            pairs.push(("command", json!(self.command)));
        }
        if self.away {
            pairs.push(("away", json!(true)));
        }
        if let Some(f) = &self.fetch {
            pairs.push(("fetch", f.clone()));
        }
        if self.max_seconds.fract() == 0.0 {
            pairs.push(("max_seconds", json!(self.max_seconds as u64)));
        } else {
            pairs.push(("max_seconds", json!(self.max_seconds)));
        }
        crate::gojson::obj(pairs)
    }
}

/// Where a clip goes and the bearer it carries.
pub struct Target {
    pub url: String,
    pub token: String,
}

const OFF_BOX: &str = "Voice is off: --voice-url names another machine, and coppice sends it no web token. Start the web server again with --voice-token-file naming that server's token file.";

/// Whether raw names localhost or a loopback address.
fn loopback_url(raw: &str) -> bool {
    let Some(host) = url_hostname(raw) else {
        return false;
    };
    if host == "localhost" {
        return true;
    }
    super::ip_is_loopback(&host) == Some(true)
}

/// Go's URL.Hostname for an absolute URL: the host without its port or
/// brackets. None when raw has no scheme part.
pub fn url_hostname(raw: &str) -> Option<String> {
    let (_, rest) = raw.split_once("://")?;
    let auth = rest.split(['/', '?', '#']).next().unwrap_or("");
    let auth = auth.rsplit('@').next().unwrap_or(auth);
    if let Some(r) = auth.strip_prefix('[') {
        return Some(r.split(']').next().unwrap_or("").to_string());
    }
    Some(match auth.rfind(':') {
        Some(i) if auth[i + 1..].bytes().all(|b| b.is_ascii_digit()) => auth[..i].to_string(),
        _ => auth.to_string(),
    })
}

fn voice_message(reason: &str, fix: &str) -> String {
    if fix.is_empty() {
        format!("{reason} Press Retry.")
    } else {
        format!("{reason} {fix}, then press Retry.")
    }
}

impl Server {
    /// Where voice stands, and where a clip goes when it is ready.
    fn voice_now(&self, verb: &str) -> Result<(State, Option<Target>), String> {
        if self.opts.voice_url.is_empty() {
            let (st, t) = self.ask_voice(verb);
            return Ok((st, t));
        }
        if !self.opts.voice_token_file.is_empty() {
            return match std::fs::read(&self.opts.voice_token_file) {
                Err(_) => Ok((
                    State {
                        state: "off".into(),
                        away: true,
                        message: format!(
                            "Voice is off: the voice token file {} cannot be read.",
                            self.opts.voice_token_file
                        ),
                        ..Default::default()
                    },
                    None,
                )),
                Ok(raw) => Ok((
                    State {
                        ready: true,
                        state: "ready".into(),
                        ..Default::default()
                    },
                    Some(Target {
                        url: self.opts.voice_url.clone(),
                        token: String::from_utf8_lossy(&raw).trim().to_string(),
                    }),
                )),
            };
        }
        if !loopback_url(&self.opts.voice_url) {
            return Ok((
                State {
                    state: "off".into(),
                    away: true,
                    message: OFF_BOX.into(),
                    ..Default::default()
                },
                None,
            ));
        }
        let tok = self.opts.tokens.load()?;
        Ok((
            State {
                ready: true,
                state: "ready".into(),
                ..Default::default()
            },
            Some(Target {
                url: self.opts.voice_url.clone(),
                token: tok,
            }),
        ))
    }

    /// Runs voice.status or voice.start on the coppice server.
    fn ask_voice(&self, verb: &str) -> (State, Option<Target>) {
        let msg = match upstream::call(&self.opts.dial, vec![("cmd", json!(verb))]) {
            Ok(m) => m,
            Err(CallError::Refused { .. }) => {
                return (
                    State {
                        state: "down".into(),
                        action: "retry".into(),
                        message: voice_message(
                            "This coppice server is too old to start voice.",
                            "Stop it and start it again with this coppice",
                        ),
                        ..Default::default()
                    },
                    None,
                )
            }
            Err(CallError::Failed(_)) => {
                return (
                    State {
                        state: "down".into(),
                        action: "retry".into(),
                        message: voice_message("The coppice server did not answer.", ""),
                        ..Default::default()
                    },
                    None,
                )
            }
        };
        let res = msg.get("result").cloned().unwrap_or(Any::Nil);
        let s = |k: &str| res.str_of(k);
        let ready = matches!(res.get("ready"), Some(Any::Bool(true)));
        let mut st = State {
            ready,
            state: s("state"),
            command: s("command"),
            ..Default::default()
        };
        if st.state == "starting" {
            if let Some(f @ Any::Map(_)) = res.get("fetch") {
                st.fetch = serde_json::from_str(&f.marshal()).ok();
            }
        }
        if st.state == "off" {
            st.message = format!("{} {}.", s("reason"), s("fix"));
            st.away = true;
            return (st, None);
        }
        if !ready {
            st.message = voice_message(&s("reason"), &s("fix"));
            st.action = "retry".into();
            return (st, None);
        }
        let mut tok = String::new();
        let f = s("token_file");
        if !f.is_empty() {
            match std::fs::read(&f) {
                Ok(raw) => tok = String::from_utf8_lossy(&raw).trim().to_string(),
                Err(_) => {
                    return (
                        State {
                            state: "down".into(),
                            action: "retry".into(),
                            message: voice_message(
                                "Voice's token file cannot be read.",
                                "Start voice again",
                            ),
                            ..Default::default()
                        },
                        None,
                    )
                }
            }
        }
        (
            st,
            Some(Target {
                url: s("url"),
                token: tok,
            }),
        )
    }

    pub fn handle_voice_status(&self, _req: &Request, resp: &mut Response) {
        let (mut st, target) = self.voice_now("voice.status").unwrap_or_default_state();
        st.max_seconds = voice_cap(target.as_ref());
        write_json(resp, 200, &st.json());
    }

    pub fn handle_voice_retry(&self, _req: &Request, resp: &mut Response) {
        let (mut st, target) = self.voice_now("voice.start").unwrap_or_default_state();
        st.max_seconds = voice_cap(target.as_ref());
        write_json(resp, 200, &st.json());
    }

    pub fn handle_voice_transcribe(&self, req: &Request, resp: &mut Response) {
        let (st, target) = match self.voice_now("voice.status") {
            Ok(x) => x,
            Err(_) => {
                write_err(
                    resp,
                    500,
                    "No web token. Run coppice web token, then try again.",
                );
                return;
            }
        };
        let Some(target) = target else {
            let mut pairs = vec![("error", json!(st.message)), ("state", json!(st.state))];
            if !st.action.is_empty() {
                pairs.push(("action", json!(st.action)));
            }
            if !st.command.is_empty() {
                pairs.push(("command", json!(st.command)));
            }
            if st.away {
                pairs.push(("away", json!(true)));
            }
            write_json(resp, 503, &crate::gojson::map(pairs));
            return;
        };
        if req.body.len() > MAX_VOICE_CLIP_BYTES || req.body_too_big {
            write_err(
                resp,
                413,
                &format!(
                    "The clip is larger than {MAX_VOICE_CLIP_BYTES} bytes. Record a shorter clip."
                ),
            );
            resp.close = true;
            return;
        }
        let managed = self.opts.voice_url.is_empty();
        // Whether a job is already running is the voice server's to say: it
        // answers 409, and the relay passes that on. The wait below does not
        // follow the page, so a page that goes away leaves the job and the
        // server's lock exactly as the go server does.
        let mut dest = format!("{}/transcribe", target.url.trim_end_matches('/'));
        if !req.raw_query.is_empty() {
            dest.push('?');
            dest.push_str(&req.raw_query);
        }
        let ctype = req.header("Content-Type").to_string();
        match post(&dest, &ctype, &target.token, &req.body) {
            Err(PostError::Build) => write_err(
                resp,
                500,
                &format!(
                    "The request to the voice server at {} could not be built.",
                    target.url
                ),
            ),
            Err(PostError::Timeout) => {
                // The voice server did not see the cancel and may still be
                // working. Say that this was a wait that ran out, not a dead
                // server, so the owner does not start the same job twice.
                write_err(
                    resp,
                    504,
                    &format!(
                        "The clip took longer than {} minutes to turn into text. The voice server may still be working on it. Wait a few minutes before sending it again.",
                        voice_wait().as_secs() / 60
                    ),
                );
            }
            Err(PostError::Reach) => {
                if managed {
                    write_json(
                        resp,
                        503,
                        &crate::gojson::map(vec![
                            ("state", json!("down")),
                            ("action", json!("retry")),
                            (
                                "error",
                                json!(voice_message(
                                    &format!("Could not reach the voice server at {}.", target.url),
                                    ""
                                )),
                            ),
                        ]),
                    );
                } else {
                    write_err(
                        resp,
                        502,
                        &format!(
                            "Could not reach the voice server at {}. Run daisugi voice serve first.",
                            target.url
                        ),
                    );
                }
            }
            Ok((code, up_ctype, raw)) => relay(resp, code, &up_ctype, raw),
        }
    }
}

trait OrDefault {
    fn unwrap_or_default_state(self) -> (State, Option<Target>);
}

impl OrDefault for Result<(State, Option<Target>), String> {
    fn unwrap_or_default_state(self) -> (State, Option<Target>) {
        self.unwrap_or_else(|_| (State::default(), None))
    }
}

/// Writes the voice server's status and body back. A JSON object with a
/// message gets the message as its error and the old error as its code;
/// anything else goes out as it came.
fn relay(resp: &mut Response, code: u16, up_ctype: &str, raw: Vec<u8>) {
    let mut body = raw;
    if let Ok(m) = crate::godec::decode_map(&body) {
        if let Any::Map(mut pairs) = m {
            let msg = pairs
                .iter()
                .find(|(k, _)| k == "message")
                .and_then(|(_, v)| match v {
                    Any::Str(s) if !s.is_empty() => Some(s.clone()),
                    _ => None,
                });
            if let Some(msg) = msg {
                if let Some(code_v) = pairs
                    .iter()
                    .find(|(k, _)| k == "error")
                    .map(|(_, v)| v.clone())
                {
                    upsert(&mut pairs, "code", code_v);
                }
                upsert(&mut pairs, "error", Any::Str(msg));
                body = Any::Map(pairs).marshal().into_bytes();
            }
        }
        resp.set("Content-Type", "application/json");
    } else if !up_ctype.is_empty() {
        resp.set("Content-Type", up_ctype);
    }
    resp.status = code;
    resp.body = body;
}

fn upsert(pairs: &mut Vec<(String, Any)>, k: &str, v: Any) {
    match pairs.iter_mut().find(|(key, _)| key == k) {
        Some(slot) => slot.1 = v,
        None => pairs.push((k.to_string(), v)),
    }
}

enum PostError {
    Build,
    Timeout,
    Reach,
}

/// How long the proxy waits for the voice server by default: as long as the
/// server can run. whisper.cpp takes up to 120 s for each of 40 windows of a
/// 20 minute clip (80 minutes), and cleanup adds a few minutes. A shorter
/// wait threw the finished text away and left the server busy.
const VOICE_WAIT_DEFAULT_SECS: u64 = 90 * 60;

/// How long the proxy waits for the voice server. A test shortens it.
pub static VOICE_WAIT_SECS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(VOICE_WAIT_DEFAULT_SECS);

fn voice_wait() -> std::time::Duration {
    std::time::Duration::from_secs(VOICE_WAIT_SECS.load(std::sync::atomic::Ordering::Relaxed))
}

/// One POST to the voice server: its status, Content-Type and body.
fn post(
    url: &str,
    ctype: &str,
    token: &str,
    body: &[u8],
) -> Result<(u16, String, Vec<u8>), PostError> {
    let auth = format!("Bearer {token}");
    let mut headers = vec![("Authorization", auth.as_str())];
    if !ctype.is_empty() {
        headers.push(("Content-Type", ctype));
    }
    match super::client::send(
        "POST",
        url,
        &headers,
        body,
        voice_wait(),
    ) {
        Ok(r) => Ok((r.status, r.content_type, r.body)),
        Err(super::client::Fail::Build(_)) => Err(PostError::Build),
        Err(super::client::Fail::Reach(m)) if m == "timed out" => Err(PostError::Timeout),
        Err(super::client::Fail::Reach(_)) => Err(PostError::Reach),
    }
}

/// A whole HTTP/1.x response: its status, Content-Type and body.
pub fn parse_response(raw: &[u8]) -> Option<(u16, String, Vec<u8>)> {
    let end = raw.windows(4).position(|w| w == b"\r\n\r\n")?;
    let head = String::from_utf8_lossy(&raw[..end]);
    let mut lines = head.split("\r\n");
    let status: u16 = lines.next()?.split(' ').nth(1)?.parse().ok()?;
    let mut ctype = String::new();
    let mut chunked = false;
    let mut len = None;
    for l in lines {
        let Some((k, v)) = l.split_once(':') else {
            continue;
        };
        let (k, v) = (k.trim().to_ascii_lowercase(), v.trim());
        match k.as_str() {
            "content-type" => ctype = v.to_string(),
            "transfer-encoding" => chunked = v.eq_ignore_ascii_case("chunked"),
            "content-length" => len = v.parse::<usize>().ok(),
            _ => {}
        }
    }
    let rest = &raw[end + 4..];
    let body = if chunked {
        let mut out = Vec::new();
        let mut at = 0;
        loop {
            let le = rest[at..].windows(2).position(|w| w == b"\r\n")?;
            let size = usize::from_str_radix(
                String::from_utf8_lossy(&rest[at..at + le])
                    .split(';')
                    .next()?
                    .trim(),
                16,
            )
            .ok()?;
            at += le + 2;
            if size == 0 {
                break;
            }
            out.extend_from_slice(rest.get(at..at + size)?);
            at += size + 2;
        }
        out
    } else {
        match len {
            Some(n) => rest.get(..n)?.to_vec(),
            None => rest.to_vec(),
        }
    };
    Some((status, ctype, body))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hostnames_read_as_go_reads_them() {
        assert_eq!(url_hostname("http://127.0.0.1:7477/").unwrap(), "127.0.0.1");
        assert_eq!(url_hostname("http://[::1]:7/").unwrap(), "::1");
        assert_eq!(url_hostname("http://localhost").unwrap(), "localhost");
        assert!(loopback_url("http://127.0.0.1:1"));
        assert!(!loopback_url("http://voice.test:7477"));
    }

    #[test]
    fn a_reply_with_a_message_moves_it_to_error() {
        let mut r = Response::new(200);
        relay(
            &mut r,
            422,
            "application/json",
            br#"{"error":"no_speech","message":"No speech found.","at":1.50}"#.to_vec(),
        );
        assert_eq!(
            String::from_utf8(r.body).unwrap(),
            r#"{"at":1.5,"code":"no_speech","error":"No speech found.","message":"No speech found."}"#
        );
    }

    /// The tests that change or depend on the proxy's wait run one at a time.
    static WAIT_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    /// A Server whose voice server is `url`, and a POST of a clip to it.
    fn proxy(url: &str, name: &str) -> (Server, Request) {
        let dir = std::env::temp_dir().join(format!("coppice-rs-voice-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let tokens = super::super::token::TokenStore::new(dir.join("token"));
        tokens.mint().unwrap();
        let opts = super::super::serve::Options {
            dial: super::super::upstream::Dialer { path: dir.join("none.sock") },
            tokens,
            warden_root: dir.clone(),
            push: None,
            voice_url: url.into(),
            voice_token_file: String::new(),
            views: vec![],
            extra_data_dirs: vec![],
            events: None,
            listening: None,
            cert_check_every: None,
            wait_for_tailnet: false,
            tailnet_retry: None,
            serving_file: String::new(),
        };
        let addr: std::net::SocketAddr = "127.0.0.1:1".parse().unwrap();
        let req = Request {
            method: "POST".into(),
            path: "/api/voice/transcribe".into(),
            escaped_path: "/api/voice/transcribe".into(),
            raw_query: String::new(),
            proto_minor: 1,
            headers: vec![],
            body: b"clip".to_vec(),
            body_too_big: false,
            remote: addr,
            local: addr,
            host: "x".into(),
        };
        (Server::new(opts), req)
    }

    /// A voice server that waits `hold` before it answers.
    fn slow_upstream(hold: std::time::Duration) -> String {
        use std::io::{Read, Write};
        let ln = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}", ln.local_addr().unwrap());
        std::thread::spawn(move || {
            for conn in ln.incoming() {
                let Ok(mut c) = conn else { return };
                std::thread::spawn(move || {
                    let mut buf = [0u8; 4096];
                    let _ = c.read(&mut buf);
                    std::thread::sleep(hold);
                    let body = r#"{"text":"done"}"#;
                    let _ = c.write_all(
                        format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len())
                            .as_bytes(),
                    );
                });
            }
        });
        url
    }

    // The proxy waits as long as the voice server can run on a 20 minute clip
    // (40 windows of 120 s), so it never throws a finished text away.
    #[test]
    fn the_voice_wait_covers_the_longest_job_the_server_can_run() {
        assert!(VOICE_WAIT_DEFAULT_SECS >= 85 * 60);
    }

    // A wait that runs out is not a dead server: the answer says it timed
    // out, so the owner does not read "voice is down" and start the job again.
    #[test]
    fn a_wait_that_ran_out_says_so() {
        let _g = WAIT_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let url = slow_upstream(std::time::Duration::from_secs(3));
        let (s, req) = proxy(&url, "timeout");
        VOICE_WAIT_SECS.store(1, std::sync::atomic::Ordering::Relaxed);
        let mut resp = Response::new(200);
        s.handle_voice_transcribe(&req, &mut resp);
        VOICE_WAIT_SECS.store(VOICE_WAIT_DEFAULT_SECS, std::sync::atomic::Ordering::Relaxed);
        let body = String::from_utf8(resp.body).unwrap();
        assert_eq!(resp.status, 504, "{body}");
        assert!(body.contains("took longer than") && body.contains("may still be working"), "{body}");
    }

    /// A voice server that answers 409 to a second clip while one runs.
    fn busy_upstream(hold: std::time::Duration) -> String {
        use std::io::{Read, Write};
        let ln = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}", ln.local_addr().unwrap());
        let busy = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        std::thread::spawn(move || {
            for conn in ln.incoming() {
                let Ok(mut c) = conn else { return };
                let busy = busy.clone();
                std::thread::spawn(move || {
                    let mut buf = [0u8; 4096];
                    let _ = c.read(&mut buf);
                    let (status, body) = if busy.swap(true, std::sync::atomic::Ordering::SeqCst) {
                        ("409 Conflict", r#"{"error":"busy","message":"The voice server is still turning an earlier clip into text."}"#)
                    } else {
                        std::thread::sleep(hold);
                        busy.store(false, std::sync::atomic::Ordering::SeqCst);
                        ("200 OK", r#"{"text":"done"}"#)
                    };
                    let _ = c.write_all(
                        format!("HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len())
                            .as_bytes(),
                    );
                });
            }
        });
        url
    }

    // Two clips never run at once, and the voice server is the one that says
    // so: it answers 409 while it works, and the proxy passes that on.
    #[test]
    fn the_voice_servers_busy_answer_is_relayed() {
        let _g = WAIT_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let url = busy_upstream(std::time::Duration::from_millis(1500));
        let (s, req) = proxy(&url, "busy");
        let s = std::sync::Arc::new(s);
        let first = {
            let s = s.clone();
            let req = clone_req(&req);
            std::thread::spawn(move || {
                let mut r = Response::new(200);
                s.handle_voice_transcribe(&req, &mut r);
                r.status
            })
        };
        std::thread::sleep(std::time::Duration::from_millis(400));
        let mut second = Response::new(200);
        s.handle_voice_transcribe(&req, &mut second);
        let body = String::from_utf8(second.body).unwrap();
        assert_eq!(second.status, 409, "{body}");
        assert!(body.contains("still turning an earlier clip"), "{body}");
        assert_eq!(first.join().unwrap(), 200);
        let mut third = Response::new(200);
        s.handle_voice_transcribe(&req, &mut third);
        assert_eq!(third.status, 200);
    }

    // The voice server owns the cap: the status reply carries what its
    // /health says.
    #[test]
    fn the_cap_comes_from_the_voice_servers_health() {
        use std::io::{Read, Write};
        let ln = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}", ln.local_addr().unwrap());
        std::thread::spawn(move || {
            for conn in ln.incoming() {
                let Ok(mut c) = conn else { return };
                let mut buf = [0u8; 4096];
                let _ = c.read(&mut buf);
                let body = r#"{"ok":true,"max_seconds":600}"#;
                let _ = c.write_all(
                    format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len())
                        .as_bytes(),
                );
            }
        });
        let t = Target { url, token: "t".into() };
        assert_eq!(voice_cap(Some(&t)), 600.0);
        assert_eq!(voice_cap(None), VOICE_MAX_CLIP_SECONDS);
    }

    fn clone_req(r: &Request) -> Request {
        Request {
            method: r.method.clone(),
            path: r.path.clone(),
            escaped_path: r.escaped_path.clone(),
            raw_query: r.raw_query.clone(),
            proto_minor: r.proto_minor,
            headers: r.headers.clone(),
            body: r.body.clone(),
            body_too_big: r.body_too_big,
            remote: r.remote,
            local: r.local,
            host: r.host.clone(),
        }
    }

    #[test]
    fn the_status_reply_carries_the_clip_cap() {
        let st = State { ready: true, state: "ready".into(), max_seconds: 1200.0, ..Default::default() };
        let text = crate::gojson::marshal(&st.json());
        assert!(text.contains(r#""max_seconds":1200"#), "{text}");
    }
}
