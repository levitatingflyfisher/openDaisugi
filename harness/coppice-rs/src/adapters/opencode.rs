//! OpenCode, headless: one `opencode serve` per pane on a loopback port,
//! with a random password. Prompts go over HTTP; the state comes from its
//! event stream. OpenCode holds permission prompts and questions of its
//! own: agent.allow and agent.deny answer a permission prompt, the
//! operator's next prompt answers a question, and an ask nobody answers
//! is rejected at its deadline. The adapter refuses to start unless the
//! gate plugin this coppice ships sits in OpenCode's config dir. Model
//! text goes into the grid as is.

use std::collections::{BTreeMap, HashSet};
use std::io::{BufRead, BufReader, Read};
use std::os::fd::OwnedFd;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{channel, Receiver, RecvTimeoutError, Sender};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde_json::json;

use super::http::{path_escape, Client};
use super::{Adapter, Ev, EventsSlot, Lines, Out, Proc, StartOpts};
use crate::godec::{self, StructTy, Ty};
use crate::pane::Flag;
use crate::state::Ask;

pub const BINARY: &str = "opencode";
const BIN_VAR: &str = "COPPICE_OPENCODE_BIN";
const SERVER_USER: &str = "opencode";
const PURE_ENV: &str = "OPENCODE_PURE";
const READY_TIMEOUT: Duration = Duration::from_secs(20);
const STREAM_GRACE: Duration = Duration::from_millis(300);
const ASK_TICK: Duration = Duration::from_secs(1);
const ASK_DEADLINE: Duration = Duration::from_secs(90);
const GATE_PLUGIN_SHA256: &str = "769010ed81c8beb23a09d445f0ad3d5a4ce8e754a1fd4937abc791ff9fbf2cc3";
const MAX_PLUGIN_BYTES: u64 = 1 << 20;

pub struct Opencode;

fn refuse_pure(argv: &[String]) -> Result<(), String> {
    if argv
        .iter()
        .any(|a| a == "--pure" || a.starts_with("--pure="))
    {
        return Err(
            "opencode: --pure makes OpenCode skip the gate plugin. Remove --pure from the pane's argv"
                .into(),
        );
    }
    Ok(())
}

/// The listen line's address, which must be http://127.0.0.1:PORT.
fn parse_listen_line(line: &str) -> Result<String, String> {
    let Some(i) = line.find("listening on ") else {
        return Err(format!(
            "opencode: not a listen line: {}",
            crate::sys::go_quote(line)
        ));
    };
    let rest = &line[i + "listening on ".len()..];
    let addr: String = rest.chars().take_while(|c| !c.is_whitespace()).collect();
    if addr.is_empty() {
        return Err(format!(
            "opencode: not a listen line: {}",
            crate::sys::go_quote(line)
        ));
    }
    let refuse = || {
        format!(
            "opencode: the server listens on {addr}. The adapter talks only to http://127.0.0.1:<port>. Remove --hostname from the pane's argv"
        )
    };
    let Some(hostport) = addr.strip_prefix("http://") else {
        return Err(refuse());
    };
    let (hostport, path) = match hostport.find('/') {
        Some(j) => (&hostport[..j], &hostport[j..]),
        None => (hostport, ""),
    };
    if !(path.is_empty() || path == "/") || hostport.contains('@') || addr.contains('?') {
        return Err(refuse());
    }
    let Some((host, port)) = hostport.rsplit_once(':') else {
        return Err(refuse());
    };
    match port.parse::<u32>() {
        Ok(p)
            if host == "127.0.0.1"
                && p > 0
                && p < 65536
                && port.bytes().all(|b| b.is_ascii_digit()) =>
        {
            Ok(format!("http://127.0.0.1:{p}"))
        }
        _ => Err(refuse()),
    }
}

/// Where OpenCode loads the gate plugin from, by the child's own HOME and
/// XDG_CONFIG_HOME.
fn gate_plugin_path(env: &[(String, String)]) -> Result<String, String> {
    let mut home = String::new();
    let mut xdg = String::new();
    for (k, v) in env {
        match k.as_str() {
            "HOME" => home = v.clone(),
            "XDG_CONFIG_HOME" => xdg = v.clone(),
            _ => {}
        }
    }
    let base = if xdg.starts_with('/') {
        xdg
    } else if home.starts_with('/') {
        format!("{home}/.config")
    } else {
        return Err("opencode: neither XDG_CONFIG_HOME nor HOME names a config directory, so the adapter cannot find the gate plugin".into());
    };
    Ok(crate::sys::clean(&format!(
        "{base}/opencode/plugins/daisugi-gate.ts"
    )))
}

fn require_gate_plugin(env: &[(String, String)]) -> Result<(), String> {
    let p = gate_plugin_path(env)?;
    let fix = "Run: daisugi install --harness opencode";
    let md = std::fs::metadata(&p);
    let ok = md
        .as_ref()
        .is_ok_and(|m| m.is_file() && m.len() > 0 && m.len() <= MAX_PLUGIN_BYTES);
    if !ok {
        return Err(format!(
            "opencode: the gate plugin is not at {p}, so OpenCode would run ungated. {fix}"
        ));
    }
    let b = std::fs::read(&p).map_err(|e| {
        format!(
            "opencode: cannot read the gate plugin at {p}: {}. {fix}",
            crate::sys::go_path_err("open", std::path::Path::new(&p), &e)
        )
    })?;
    if crate::sha256::hex(&b) != GATE_PLUGIN_SHA256 {
        return Err(format!(
            "opencode: the gate plugin at {p} is not the one this coppice ships. {fix}"
        ));
    }
    Ok(())
}

fn random_token() -> Result<String, String> {
    let mut b = [0u8; 24];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut b))
        .map_err(|e| format!("opencode: make a password: {e}"))?;
    Ok(b.iter().map(|x| format!("{x:02x}")).collect())
}

impl Adapter for Opencode {
    fn name(&self) -> &'static str {
        "opencode"
    }

    fn start(&self, o: StartOpts) -> Result<Arc<dyn Proc>, String> {
        refuse_pure(&o.argv)?;
        let token = random_token()?;
        let bin = o.binary(BIN_VAR, BINARY);
        let mut argv: Vec<String> = ["serve", "--hostname", "127.0.0.1", "--port", "0"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        argv.extend(o.argv.iter().cloned());
        let mut extra: BTreeMap<String, String> = o.env.clone();
        extra.remove(PURE_ENV);
        extra.insert("OPENCODE_SERVER_PASSWORD".into(), token.clone());
        extra.insert("OPENCODE_SERVER_USERNAME".into(), SERVER_USER.into());
        let base: Vec<(String, String)> = std::env::vars().filter(|(k, _)| k != PURE_ENV).collect();
        let env = crate::pane::build_env(base, &extra, &o.sock, &o.pane_id, &o.data_dir);
        let lookup = super::command(&bin, &argv, &o.cwd, env.clone());
        require_gate_plugin(&env)?;
        let mut cmd = lookup.map_err(|e| format!("cannot start {bin}: {e}"))?;
        let (r, w) = super::pipe().map_err(|e| format!("opencode: stdout pipe: {e}"))?;
        cmd.stdin(Stdio::null())
            .stdout(Stdio::from(w))
            .stderr(Stdio::inherit());
        let mut child = super::spawn(&bin, &mut cmd)?;
        drop(cmd);
        let pid = child.id() as i32;
        let exited = Arc::new(Flag::default());
        let code = Arc::new(Mutex::new(None));
        {
            let exited = exited.clone();
            let code = code.clone();
            thread::spawn(move || {
                if let Ok(st) = child.wait() {
                    *code.lock().unwrap_or_else(|e| e.into_inner()) = Some(super::exit_code(&st));
                }
                exited.set();
            });
        }
        let fail = |e: String| -> Result<Arc<dyn Proc>, String> {
            super::kill_one(pid);
            exited.wait(None);
            Err(e)
        };
        let base = match wait_listen(r, &exited) {
            Ok(b) => b,
            Err(e) => return fail(e),
        };
        let c = Client::new(&base, SERVER_USER, &token);
        if let Err(e) = wait_ready(&c) {
            return fail(e);
        }
        let mut initial = Vec::new();
        let sid = if !o.resume.is_empty() && session_exists(&c, &o.resume) {
            o.resume.clone()
        } else {
            if !o.resume.is_empty() {
                initial.push(Ev::error(format!(
                    "opencode has no session {}. This pane starts a new one.",
                    o.resume
                )));
            }
            match create_session(&c, "coppice pane") {
                Ok(s) => s,
                Err(e) => return fail(e),
            }
        };
        let resp = match c.send("GET", "/event", None, Some("text/event-stream"), None) {
            Ok(r) if r.code == 200 => r,
            Ok(r) => return fail(format!("opencode: open the event stream: {}", r.status)),
            Err(e) => return fail(format!("opencode: open the event stream: {e}")),
        };
        let (out, rx) = Out::new();
        let (rej_tx, rej_rx) = channel();
        let p = Arc::new(OpencodeProc(Arc::new(Inner {
            pid,
            client: c,
            tr: Mutex::new(Translator::new(&sid)),
            session: sid,
            pane_id: o.pane_id.clone(),
            out,
            events: Mutex::new(Some(rx)),
            relay_done: Flag::default(),
            exited,
            code,
            stop_once: Mutex::new(false),
            reject_err: Mutex::new(rej_tx),
        })));
        initial.push(Ev::state(super::IDLE, ""));
        let inner = p.0.clone();
        let body = resp.body;
        thread::spawn(move || inner.relay(body, initial, rej_rx));
        Ok(p)
    }
}

/// Reads the server's stdout until its listen line.
fn wait_listen(r: OwnedFd, exited: &Arc<Flag>) -> Result<String, String> {
    let (tx, rx) = channel::<Result<String, String>>();
    thread::spawn(move || {
        let mut sent = false;
        for line in Lines::new(r, Arc::new(AtomicBool::new(false))) {
            if sent {
                continue;
            }
            let text = String::from_utf8_lossy(&line).into_owned();
            if !text.contains("listening on") {
                continue;
            }
            let _ = tx.send(parse_listen_line(&text));
            sent = true;
        }
        if !sent {
            let _ = tx.send(Err(
                "opencode: the server closed stdout before it listened".into()
            ));
        }
    });
    let deadline = Instant::now() + READY_TIMEOUT;
    loop {
        match rx.recv_timeout(Duration::from_millis(50)) {
            Ok(r) => return r,
            Err(RecvTimeoutError::Disconnected) => {
                return Err("opencode: the server closed stdout before it listened".into())
            }
            Err(RecvTimeoutError::Timeout) => {}
        }
        if exited.is_set() {
            // The last lines may still be on their way.
            if let Ok(r) = rx.recv_timeout(Duration::from_millis(50)) {
                return r;
            }
            return Err("opencode: the server exited before it listened".into());
        }
        if Instant::now() >= deadline {
            return Err("opencode: the server did not listen within 20s".into());
        }
    }
}

fn wait_ready(c: &Client) -> Result<(), String> {
    let deadline = Instant::now() + READY_TIMEOUT;
    loop {
        if let Ok(mut resp) = c.send(
            "GET",
            "/global/health",
            None,
            None,
            Some(Duration::from_secs(10)),
        ) {
            let mut sink = Vec::new();
            let _ = (&mut resp.body).take(1 << 16).read_to_end(&mut sink);
            if resp.code == 200 {
                return Ok(());
            }
        }
        if Instant::now() >= deadline {
            return Err(
                "opencode: the server did not answer /global/health: context deadline exceeded"
                    .into(),
            );
        }
        thread::sleep(Duration::from_millis(100));
    }
}

static SESSION_INFO: StructTy = StructTy {
    name: "",
    text: "struct { ID string \"json:\\\"id\\\"\" }",
    fields: &[("id", Ty::Str)],
};

fn create_session(c: &Client, title: &str) -> Result<String, String> {
    let body = crate::gojson::marshal(&json!({ "title": title }));
    let b = c.call("create a session", "POST", "/session", Some(&body))?;
    let (g, err) = godec::decode(&b, &Ty::Struct(&SESSION_INFO));
    if let Some(e) = err {
        return Err(format!("opencode: read the new session: {e}"));
    }
    let id = g.f("id").s();
    if id.is_empty() {
        return Err("opencode: the new session has no id".into());
    }
    Ok(id)
}

fn session_exists(c: &Client, id: &str) -> bool {
    match c.send(
        "GET",
        &format!("/session/{}", path_escape(id)),
        None,
        None,
        Some(Duration::from_secs(10)),
    ) {
        Ok(mut r) => {
            let mut sink = Vec::new();
            let _ = (&mut r.body).take(1 << 20).read_to_end(&mut sink);
            r.code == 200
        }
        Err(_) => false,
    }
}

fn prompt_async(c: &Client, sid: &str, text: &str) -> Result<(), String> {
    let body = crate::gojson::marshal(&json!({"parts": [{"text": text, "type": "text"}]}));
    c.call(
        "send a prompt",
        "POST",
        &format!("/session/{}/prompt_async", path_escape(sid)),
        Some(&body),
    )
    .map(|_| ())
}

fn reply_permission(c: &Client, id: &str, reply: &str, message: &str) -> Result<(), String> {
    let body = if message.is_empty() {
        json!({ "reply": reply })
    } else {
        json!({"message": message, "reply": reply})
    };
    c.call(
        "answer a permission prompt",
        "POST",
        &format!("/permission/{}/reply", path_escape(id)),
        Some(&crate::gojson::marshal(&body)),
    )
    .map(|_| ())
}

fn reply_question(c: &Client, id: &str, answer: &str) -> Result<(), String> {
    let body = crate::gojson::marshal(&json!({ "answers": [[answer]] }));
    c.call(
        "answer a question",
        "POST",
        &format!("/question/{}/reply", path_escape(id)),
        Some(&body),
    )
    .map(|_| ())
}

fn reject_question(c: &Client, id: &str) -> Result<(), String> {
    c.call(
        "reject a question",
        "POST",
        &format!("/question/{}/reject", path_escape(id)),
        None,
    )
    .map(|_| ())
}

/// One open ask.
#[derive(Clone)]
struct OpenAsk {
    id: String,
    /// "permission" or "question".
    kind: &'static str,
    tool: String,
    summary: String,
    count: usize,
    deadline: SystemTime,
}

/// The event stream turned into pane events: only this pane's session and
/// its subagent sessions count.
struct Translator {
    session: String,
    children: HashSet<String>,
    asks: Vec<OpenAsk>,
    tools: HashSet<String>,
}

static BUS_EVENT: StructTy = StructTy {
    name: "busEvent",
    text: "opencode.busEvent",
    fields: &[("type", Ty::Str), ("properties", Ty::Raw)],
};

static SCOPE_INFO: StructTy = StructTy {
    name: "",
    text: "struct { ID string; ParentID string }",
    fields: &[("id", Ty::Str), ("parentID", Ty::Str)],
};
static SCOPE: StructTy = StructTy {
    name: "",
    text: "scope",
    fields: &[("sessionID", Ty::Str), ("info", Ty::Struct(&SCOPE_INFO))],
};
static STATUS_INNER: StructTy = StructTy {
    name: "",
    text: "status",
    fields: &[("type", Ty::Str)],
};
static STATUS: StructTy = StructTy {
    name: "",
    text: "status",
    fields: &[("status", Ty::Struct(&STATUS_INNER))],
};
static ERR_DATA: StructTy = StructTy {
    name: "",
    text: "data",
    fields: &[("message", Ty::Str)],
};
static ERR_INNER: StructTy = StructTy {
    name: "",
    text: "error",
    fields: &[("name", Ty::Str), ("data", Ty::Struct(&ERR_DATA))],
};
static SESSION_ERROR: StructTy = StructTy {
    name: "",
    text: "session error",
    fields: &[("error", Ty::Struct(&ERR_INNER))],
};
static STR: Ty = Ty::Str;
static PERMISSION: StructTy = StructTy {
    name: "",
    text: "permission",
    fields: &[
        ("id", Ty::Str),
        ("permission", Ty::Str),
        ("patterns", Ty::Slice(&STR, "[]string")),
    ],
};
static QUESTION_ITEM: StructTy = StructTy {
    name: "",
    text: "question",
    fields: &[("question", Ty::Str)],
};
static QUESTION_ITEM_TY: Ty = Ty::Struct(&QUESTION_ITEM);
static QUESTION: StructTy = StructTy {
    name: "",
    text: "question",
    fields: &[
        ("id", Ty::Str),
        ("questions", Ty::Slice(&QUESTION_ITEM_TY, "[]question")),
    ],
};
static REPLIED: StructTy = StructTy {
    name: "",
    text: "replied",
    fields: &[("requestID", Ty::Str)],
};
static PART_TIME: StructTy = StructTy {
    name: "",
    text: "time",
    fields: &[("end", Ty::PtrInt64)],
};
static PART_STATE: StructTy = StructTy {
    name: "",
    text: "state",
    fields: &[("status", Ty::Str), ("input", Ty::Raw)],
};
static PART_INNER: StructTy = StructTy {
    name: "",
    text: "part",
    fields: &[
        ("type", Ty::Str),
        ("text", Ty::Str),
        ("callID", Ty::Str),
        ("tool", Ty::Str),
        ("time", Ty::PtrStruct(&PART_TIME)),
        ("state", Ty::Struct(&PART_STATE)),
    ],
};
static PART: StructTy = StructTy {
    name: "",
    text: "part",
    fields: &[("part", Ty::Struct(&PART_INNER))],
};

/// One "data:" line of the stream as an event type and its raw
/// properties. Anything else is skipped.
fn parse_sse_line(line: &str) -> Option<(String, String)> {
    let payload = line.strip_prefix("data:")?;
    let (g, err) = godec::decode(payload.trim().as_bytes(), &Ty::Struct(&BUS_EVENT));
    let ty = g.f("type").s();
    if err.is_some() || ty.is_empty() {
        return None;
    }
    Some((ty, g.f("properties").s()))
}

fn decode_ok(raw: &str, st: &'static StructTy) -> Option<godec::G> {
    let (g, err) = godec::decode(raw.as_bytes(), &Ty::Struct(st));
    if err.is_some() {
        return None;
    }
    Some(g)
}

fn unix(t: SystemTime) -> f64 {
    t.duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as f64)
        .unwrap_or(0.0)
}

impl Translator {
    fn new(session: &str) -> Translator {
        Translator {
            session: session.to_string(),
            children: HashSet::new(),
            asks: Vec::new(),
            tools: HashSet::new(),
        }
    }

    fn ours(&self, sid: &str) -> bool {
        !sid.is_empty() && (sid == self.session || self.children.contains(sid))
    }

    fn open(&self) -> (Vec<String>, Vec<String>) {
        let now = SystemTime::now();
        let mut perms = Vec::new();
        let mut questions = Vec::new();
        for a in &self.asks {
            if now > a.deadline {
                continue;
            }
            if a.kind == "permission" {
                perms.push(a.id.clone());
            } else {
                questions.push(a.id.clone());
            }
        }
        (perms, questions)
    }

    fn get(&self, id: &str) -> Option<OpenAsk> {
        let now = SystemTime::now();
        self.asks
            .iter()
            .find(|a| a.id == id && now <= a.deadline)
            .cloned()
    }

    fn remove(&mut self, id: &str) {
        self.asks.retain(|a| a.id != id);
    }

    fn expire(&mut self) -> Vec<OpenAsk> {
        let now = SystemTime::now();
        let (gone, keep): (Vec<OpenAsk>, Vec<OpenAsk>) =
            self.asks.drain(..).partition(|a| now > a.deadline);
        self.asks = keep;
        gone
    }

    fn blocked_on(a: &OpenAsk) -> Ev {
        Ev {
            ask: Some(Ask {
                id: a.id.clone(),
                tool: a.tool.clone(),
                summary: a.summary.clone(),
                deadline: unix(a.deadline),
                ..Default::default()
            }),
            ..Ev::state(super::BLOCKED, a.summary.clone())
        }
    }

    fn add(&mut self, mut a: OpenAsk) -> Vec<Ev> {
        a.deadline = SystemTime::now() + ASK_DEADLINE;
        self.remove(&a.id.clone());
        let ev = Self::blocked_on(&a);
        self.asks.push(a);
        vec![ev]
    }

    fn resolved(&mut self, id: &str) -> Vec<Ev> {
        self.remove(id);
        match self.asks.last() {
            Some(a) => vec![Self::blocked_on(a)],
            None => vec![Ev::state(super::WORKING, "")],
        }
    }

    fn clear(&mut self) {
        self.asks.clear();
        self.tools.clear();
    }

    fn translate(&mut self, ty: &str, props: &str) -> Vec<Ev> {
        let Some(scope) = decode_ok(props, &SCOPE) else {
            return Vec::new();
        };
        let sid = scope.f("sessionID").s();
        if ty == "session.created" {
            let id = scope.f("info").f("id").s();
            if !id.is_empty() && self.ours(&scope.f("info").f("parentID").s()) {
                self.children.insert(id);
            }
            return Vec::new();
        }
        if !self.ours(&sid) {
            return Vec::new();
        }
        let own = sid == self.session;
        match ty {
            "session.status" => {
                if !own {
                    return Vec::new();
                }
                let Some(p) = decode_ok(props, &STATUS) else {
                    return Vec::new();
                };
                match p.f("status").f("type").s().as_str() {
                    "busy" | "retry" => vec![Ev::state(super::WORKING, "")],
                    "idle" => {
                        self.clear();
                        vec![Ev::state(super::IDLE, "")]
                    }
                    _ => Vec::new(),
                }
            }
            "session.idle" => {
                if !own {
                    return Vec::new();
                }
                self.clear();
                vec![Ev::state(super::IDLE, "")]
            }
            "session.error" => {
                if !own {
                    return Vec::new();
                }
                let (p, _) = godec::decode(props.as_bytes(), &Ty::Struct(&SESSION_ERROR));
                let e = p.f("error");
                let detail = format!("{}: {}", e.f("name").s(), e.f("data").f("message").s());
                vec![Ev::state(
                    super::UNKNOWN,
                    format!("session error {}", detail.trim()),
                )]
            }
            "permission.asked" => {
                let Some(p) = decode_ok(props, &PERMISSION) else {
                    return Vec::new();
                };
                let id = p.f("id").s();
                if id.is_empty() {
                    return Vec::new();
                }
                let patterns: Vec<String> = p.f("patterns").items().iter().map(|x| x.s()).collect();
                self.add(OpenAsk {
                    id,
                    kind: "permission",
                    tool: p.f("permission").s(),
                    summary: patterns.join(", "),
                    count: 0,
                    deadline: SystemTime::now(),
                })
            }
            "question.asked" => {
                let Some(p) = decode_ok(props, &QUESTION) else {
                    return Vec::new();
                };
                let id = p.f("id").s();
                if id.is_empty() {
                    return Vec::new();
                }
                let qs = p.f("questions").items();
                let summary = qs.first().map(|q| q.f("question").s()).unwrap_or_default();
                self.add(OpenAsk {
                    id,
                    kind: "question",
                    tool: "question".into(),
                    summary,
                    count: qs.len(),
                    deadline: SystemTime::now(),
                })
            }
            "permission.replied" | "question.replied" | "question.rejected" => {
                let Some(p) = decode_ok(props, &REPLIED) else {
                    return Vec::new();
                };
                self.resolved(&p.f("requestID").s())
            }
            "message.part.updated" => {
                if !own {
                    return Vec::new();
                }
                self.part(props)
            }
            _ => Vec::new(),
        }
    }

    fn part(&mut self, raw: &str) -> Vec<Ev> {
        let Some(p) = decode_ok(raw, &PART) else {
            return Vec::new();
        };
        let part = p.f("part");
        match part.f("type").s().as_str() {
            "text" => {
                let t = part.f("time");
                let text = part.f("text").s();
                if !t.present() || !t.f("end").present() || text.is_empty() {
                    return Vec::new();
                }
                vec![Ev::text(text)]
            }
            "tool" => {
                let call = part.f("callID").s();
                if part.f("state").f("status").s() != "running" || call.is_empty() {
                    return Vec::new();
                }
                if !self.tools.insert(call.clone()) {
                    return Vec::new();
                }
                let input = part.f("state").f("input").s();
                vec![Ev::tool(part.f("tool").s(), format!("{call} {input}"))]
            }
            _ => Vec::new(),
        }
    }
}

struct OpencodeProc(Arc<Inner>);

struct Inner {
    pid: i32,
    client: Client,
    tr: Mutex<Translator>,
    session: String,
    pane_id: String,
    out: Out,
    events: EventsSlot,
    relay_done: Flag,
    exited: Arc<Flag>,
    code: Arc<Mutex<Option<i64>>>,
    stop_once: Mutex<bool>,
    reject_err: Mutex<Sender<String>>,
}

/// What the relay waits on.
enum Msg {
    Line(String, String),
    Closed(Option<String>),
}

impl Inner {
    fn stopped_err(&self) -> Result<(), String> {
        if self.out.stopped() || self.relay_done.is_set() {
            return Err("opencode: this pane has stopped".into());
        }
        Ok(())
    }

    fn pane_name(&self) -> String {
        if self.pane_id.is_empty() {
            "PANE".into()
        } else {
            self.pane_id.clone()
        }
    }

    fn reject_expired(self: Arc<Self>, a: OpenAsk) {
        let r = if a.kind == "question" {
            reject_question(&self.client, &a.id)
        } else {
            reply_permission(
                &self.client,
                &a.id,
                "reject",
                "no answer from the operator before the deadline",
            )
        };
        if let Err(e) = r {
            let detail = format!(
                "ask {} passed its deadline and the reject failed: {e}. OpenCode may still wait on it",
                a.id
            );
            if !self.out.stopped() {
                let _ = self
                    .reject_err
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .send(detail);
            }
        }
    }

    fn relay(self: Arc<Self>, body: super::http::Body, initial: Vec<Ev>, rej: Receiver<String>) {
        let (tx, rx) = channel::<Msg>();
        let quit = Arc::new(AtomicBool::new(false));
        {
            let quit = quit.clone();
            thread::spawn(move || {
                let mut r = BufReader::new(body);
                let mut err = None;
                loop {
                    let mut buf = Vec::new();
                    match r.read_until(b'\n', &mut buf) {
                        Ok(0) => break,
                        Ok(_) => {
                            if buf.last() == Some(&b'\n') {
                                buf.pop();
                            }
                            if buf.last() == Some(&b'\r') {
                                buf.pop();
                            }
                            let text = String::from_utf8_lossy(&buf).into_owned();
                            if let Some((ty, props)) = parse_sse_line(&text) {
                                if quit.load(Ordering::SeqCst)
                                    || tx.send(Msg::Line(ty, props)).is_err()
                                {
                                    return;
                                }
                            }
                        }
                        Err(e) => {
                            err = Some(e.to_string());
                            break;
                        }
                    }
                }
                let _ = tx.send(Msg::Closed(err));
            });
        }
        self.pump(&rx, &rej, initial);
        quit.store(true, Ordering::SeqCst);
        let requested = self.out.stopped();
        super::kill_one(self.pid);
        self.exited.wait(None);
        let code = *self.code.lock().unwrap_or_else(|e| e.into_inner());
        let detail = match code {
            Some(n) if !requested => format!("opencode exited: exit={n}"),
            _ => "opencode exited".to_string(),
        };
        self.out.send_always(Ev::end(detail));
        self.out.close();
        self.relay_done.set();
    }

    fn pump(self: &Arc<Self>, rx: &Receiver<Msg>, rej: &Receiver<String>, initial: Vec<Ev>) {
        for ev in initial {
            if !self.out.send(ev) {
                return;
            }
        }
        let mut next_tick = Instant::now() + ASK_TICK;
        let mut lines_open = true;
        loop {
            if self.out.stopped() || self.exited.is_set() {
                return;
            }
            if Instant::now() >= next_tick {
                next_tick = Instant::now() + ASK_TICK;
                let gone = self.tr.lock().unwrap_or_else(|e| e.into_inner()).expire();
                for a in gone {
                    let me = self.clone();
                    thread::spawn(move || me.reject_expired(a));
                }
            }
            if let Ok(detail) = rej.try_recv() {
                if !self.out.send(Ev::state(super::UNKNOWN, detail)) {
                    return;
                }
                continue;
            }
            if !lines_open {
                thread::sleep(Duration::from_millis(50));
                continue;
            }
            match rx.recv_timeout(Duration::from_millis(50)) {
                Ok(Msg::Line(ty, props)) => {
                    let evs = self
                        .tr
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .translate(&ty, &props);
                    for ev in evs {
                        if !self.out.send(ev) {
                            return;
                        }
                    }
                }
                Ok(Msg::Closed(err)) => {
                    lines_open = false;
                    if self.exited.wait(Some(STREAM_GRACE)) || self.out.stopped() {
                        return;
                    }
                    let mut detail = "the event stream ended while opencode still runs".to_string();
                    if let Some(e) = err {
                        detail = format!("{detail}: {e}");
                    }
                    if !self.out.send(Ev::state(super::UNKNOWN, detail)) {
                        return;
                    }
                }
                Err(RecvTimeoutError::Timeout) => {}
                Err(RecvTimeoutError::Disconnected) => lines_open = false,
            }
        }
    }
}

impl Proc for OpencodeProc {
    fn prompt(&self, text: &str) -> Result<(), String> {
        self.prompt_from(text, false)
    }

    fn prompt_from(&self, text: &str, operator: bool) -> Result<(), String> {
        let me = &self.0;
        me.stopped_err()?;
        let (perms, questions) = me.tr.lock().unwrap_or_else(|e| e.into_inner()).open();
        if let Some(id) = perms.first() {
            let pn = me.pane_name();
            return Err(format!(
                "opencode waits on its permission prompt {id}. A prompt does not answer it. Run: coppice agent allow {pn} {id}, or: coppice agent deny {pn} {id}"
            ));
        }
        if let Some(q) = questions.first() {
            if !operator {
                return Err(
                    "opencode waits on a question for the operator. A pane cannot answer it".into(),
                );
            }
            let a = me.tr.lock().unwrap_or_else(|e| e.into_inner()).get(q);
            let Some(a) = a else {
                return prompt_async(&me.client, &me.session, text);
            };
            if a.count == 1 {
                reply_question(&me.client, &a.id, text)?;
                me.tr
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .remove(&a.id);
                return Ok(());
            }
            reject_question(&me.client, &a.id)?;
            me.tr
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .remove(&a.id);
        }
        prompt_async(&me.client, &me.session, text)
    }

    fn write_stdin(&self, _b: &[u8]) -> Result<(), String> {
        self.0.stopped_err()?;
        Err("opencode: this harness does not support that. Send a prompt instead.".into())
    }

    fn take_events(&self) -> Option<Receiver<Ev>> {
        self.0
            .events
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
    }

    fn session_id(&self) -> Option<String> {
        (!self.0.session.is_empty()).then(|| self.0.session.clone())
    }

    fn stop(&self) {
        let me = &self.0;
        {
            let mut once = me.stop_once.lock().unwrap_or_else(|e| e.into_inner());
            if !*once {
                *once = true;
                me.out.stop.store(true, Ordering::SeqCst);
                super::kill_one(me.pid);
            }
        }
        me.relay_done.wait(None);
    }

    fn pids(&self) -> Vec<i32> {
        vec![self.0.pid]
    }

    fn answerer(&self) -> bool {
        true
    }

    fn owns_ask(&self, id: &str) -> bool {
        self.0
            .tr
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get(id)
            .is_some()
    }

    fn answer(&self, id: &str, allow: bool, reason: &str) -> Result<(), String> {
        let me = &self.0;
        me.stopped_err()?;
        let Some(a) = me.tr.lock().unwrap_or_else(|e| e.into_inner()).get(id) else {
            return Err(format!("opencode has no open ask {id}"));
        };
        if a.kind == "question" && allow {
            return Err(format!(
                "ask {id} is a question. Type the answer into the pane"
            ));
        }
        if a.kind == "question" {
            reject_question(&me.client, id)?;
        } else if allow {
            reply_permission(&me.client, id, "once", "")?;
        } else {
            reply_permission(&me.client, id, "reject", reason)?;
        }
        me.tr.lock().unwrap_or_else(|e| e.into_inner()).remove(id);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn listen_lines() {
        assert_eq!(
            parse_listen_line("opencode server listening on http://127.0.0.1:4096").unwrap(),
            "http://127.0.0.1:4096"
        );
        assert!(
            parse_listen_line("opencode server listening on http://0.0.0.0:4096")
                .unwrap_err()
                .contains("Remove --hostname")
        );
        assert!(parse_listen_line("listening on http://127.0.0.1:0").is_err());
    }

    #[test]
    fn the_captured_stream_translates_as_go_does() {
        let raw = std::fs::read_to_string(format!(
            "{}/../coppice/internal/adapters/opencode/testdata/session_lifecycle.sse",
            env!("CARGO_MANIFEST_DIR")
        ))
        .unwrap();
        let mut tr = Translator::new("ses_f3107821dffeLaGMxGh2BNticC");
        let mut got = Vec::new();
        for line in raw.lines() {
            if let Some((ty, props)) = parse_sse_line(line) {
                for ev in tr.translate(&ty, &props) {
                    got.push(format!(
                        "{:?}|{}|{}|{}",
                        ev.kind, ev.state, ev.text, ev.tool
                    ));
                }
            }
        }
        assert!(got.contains(&"State|working||".to_string()), "{got:?}");
        assert_eq!(got.last().unwrap(), "State|idle||");
        assert!(!got.iter().any(|g| g.contains("say hi")), "{got:?}");
    }
}
