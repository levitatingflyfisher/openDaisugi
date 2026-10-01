//! What the floor knows about a pane beyond its state: the router its
//! model calls go through, the gate verdict and mode its hook last
//! reported, and its tokens and model from the transcripts it may read;
//! and floor.facts, the floor's header row.

use std::collections::{HashMap, VecDeque};
use std::fs::File;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{json, Value};

use super::{Client, Server};
use crate::config;
use crate::gojson;
use crate::layout;
use crate::proto::{self, Request, Response};
use crate::state::{self, ReportExtras};

/// How long the floor trusts what it read about the gateway.
const OUTSIDE_TTL: Duration = Duration::from_secs(5);
/// The bound on the check that the gateway answers.
const GATEWAY_DIAL_TIMEOUT: Duration = Duration::from_millis(300);
/// The most bytes one poll reads.
const TAIL_CAP: u64 = 4 << 20;
/// How many message ids a tailer remembers to count a split message once.
const ID_KEEP: usize = 1024;
/// How many transcript files one pane reads.
const MAX_FILES_PER_PANE: usize = 8;
/// How many let-go files a pane remembers.
const RETIRED_KEEP: usize = 64;
/// The least time between two reads of one pane's transcript.
const FACTS_EVERY: Duration = Duration::from_secs(1);
/// How long a row waits for a read it started.
const POLL_WAIT: Duration = Duration::from_millis(50);
/// A read longer than this marks its path refused.
const POLL_DEADLINE: Duration = Duration::from_secs(5);
/// The daisugi mode of a harness pane no gate hook reported for.
const MODE_OFF: &str = "off";

/// Token use summed over an agent's turns.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Tokens {
    pub fresh: i64,
    pub cache_read: i64,
    pub cache_write: i64,
    pub out: i64,
}

impl Tokens {
    fn add(&mut self, b: Tokens) {
        self.fresh += b.fresh;
        self.cache_read += b.cache_read;
        self.cache_write += b.cache_write;
        self.out += b.out;
    }

    fn sub(&mut self, b: Tokens) {
        self.fresh -= b.fresh;
        self.cache_read -= b.cache_read;
        self.cache_write -= b.cache_write;
        self.out -= b.out;
    }

    fn to_value(self) -> Value {
        gojson::obj(vec![
            ("fresh", json!(self.fresh)),
            ("cache_read", json!(self.cache_read)),
            ("cache_write", json!(self.cache_write)),
            ("out", json!(self.out)),
        ])
    }
}

fn non_neg(n: i64) -> i64 {
    n.max(0)
}

fn truncate(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

/// The line format a tailer reads.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Kind {
    /// A Claude Code transcript. An assistant entry carries message.id,
    /// message.model and message.usage, repeated on each line of a split
    /// message.
    Claude,
    /// A sprig session tree. An assistant entry carries a unique id, model
    /// and usage; a verdict entry carries the gate's decision.
    Sprig,
}

/// The last verdict known for a pane. at is unix seconds.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct GateFact {
    pub decision: String,
    pub tool: String,
    pub clause: String,
    pub at: f64,
}

impl GateFact {
    fn to_value(&self) -> Value {
        gojson::obj(vec![
            ("decision", json!(self.decision)),
            ("tool", json!(self.tool)),
            ("clause", json!(self.clause)),
            ("at", gojson::float(self.at)),
        ])
    }
}

/// What a tailer knew after its last poll.
#[derive(Clone, Debug, Default)]
struct TailSnap {
    ok: bool,
    refused: bool,
    sum: Tokens,
    model: String,
    verdict: Option<GateFact>,
    mode: String,
    off: u64,
    dev: u64,
    ino: u64,
    skipping: bool,
}

/// Reads one transcript from the start, then only what was added since.
pub struct Tailer {
    path: String,
    kind: Kind,
    cap: u64,
    off: u64,
    dev: u64,
    ino: u64,
    opened: bool,
    skipping: bool,
    refused: bool,
    ok: bool,
    sum: Tokens,
    model: String,
    by_id: HashMap<String, Tokens>,
    ids: VecDeque<String>,
    verdict: Option<GateFact>,
    mode: String,
    last_call_id: String,
    last_call_name: String,
}

/// Opens path for reading only when it is a regular file: never through a
/// symlink at the last step, never waiting on a FIFO. refuse is true when
/// the path exists and is not a regular file.
fn open_regular(path: &str) -> Result<(File, u64, u64), (bool, std::io::Error)> {
    let f = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|e| (e.kind() != std::io::ErrorKind::NotFound, e))?;
    let m = f.metadata().map_err(|e| (false, e))?;
    if !m.file_type().is_file() {
        return Err((
            true,
            std::io::Error::other(format!("{path} is not a regular file")),
        ));
    }
    Ok((f, m.dev(), m.ino()))
}

#[derive(Deserialize)]
struct ClaudeUsage {
    #[serde(default)]
    input_tokens: i64,
    #[serde(default)]
    cache_read_input_tokens: i64,
    #[serde(default)]
    cache_creation_input_tokens: i64,
    #[serde(default)]
    output_tokens: i64,
}

#[derive(Deserialize)]
struct ClaudeMessage {
    #[serde(default)]
    id: String,
    #[serde(default)]
    model: String,
    usage: Option<ClaudeUsage>,
}

#[derive(Deserialize)]
struct ClaudeEntry {
    #[serde(default, rename = "type")]
    kind: String,
    message: Option<ClaudeMessage>,
}

#[derive(Deserialize)]
struct SprigUsage {
    #[serde(default)]
    fresh: i64,
    #[serde(default, rename = "cacheRead")]
    cache_read: i64,
    #[serde(default, rename = "cacheWrite")]
    cache_write: i64,
    #[serde(default)]
    out: i64,
}

#[derive(Deserialize)]
struct SprigAssistant {
    #[serde(default, rename = "type")]
    kind: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    model: String,
    usage: Option<SprigUsage>,
}

#[derive(Deserialize)]
struct SprigGate {
    #[serde(default, rename = "type")]
    kind: String,
    #[serde(default)]
    ts: f64,
    #[serde(default, rename = "toolUseId")]
    tool_use_id: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    decision: String,
    #[serde(default)]
    mode: String,
    #[serde(default)]
    clause: String,
}

fn contains(hay: &[u8], needle: &[u8]) -> bool {
    hay.windows(needle.len()).any(|w| w == needle)
}

impl Tailer {
    pub fn new(path: &str, kind: Kind) -> Tailer {
        Tailer {
            path: path.to_string(),
            kind,
            cap: TAIL_CAP,
            off: 0,
            dev: 0,
            ino: 0,
            opened: false,
            skipping: false,
            refused: false,
            ok: false,
            sum: Tokens::default(),
            model: String::new(),
            by_id: HashMap::new(),
            ids: VecDeque::new(),
            verdict: None,
            mode: String::new(),
            last_call_id: String::new(),
            last_call_name: String::new(),
        }
    }

    fn snapshot(&self) -> TailSnap {
        TailSnap {
            ok: self.ok,
            refused: self.refused,
            sum: self.sum,
            model: self.model.clone(),
            verdict: self.verdict.clone(),
            mode: self.mode.clone(),
            off: self.off,
            dev: self.dev,
            ino: self.ino,
            skipping: self.skipping,
        }
    }

    /// Forgets everything read, for a file cut short or replaced.
    fn reset(&mut self) {
        self.off = 0;
        self.skipping = false;
        self.sum = Tokens::default();
        self.model.clear();
        self.by_id.clear();
        self.ids.clear();
        self.verdict = None;
        self.mode.clear();
        self.last_call_id.clear();
        self.last_call_name.clear();
    }

    /// Reads the bytes added since the last poll, at most cap of them, and
    /// folds each complete line in. A line not yet ended waits.
    pub fn poll(&mut self) -> Result<(), String> {
        if self.refused {
            return Err("this path is not a regular file and is never read".into());
        }
        let (mut f, dev, ino) = match open_regular(&self.path) {
            Ok(x) => x,
            Err((refuse, e)) => {
                if refuse {
                    self.refused = true;
                    self.reset();
                    self.ok = false;
                }
                return Err(e.to_string());
            }
        };
        let size = f.metadata().map_err(|e| e.to_string())?.len();
        if self.opened && (dev != self.dev || ino != self.ino || size < self.off) {
            self.reset();
        }
        self.opened = true;
        self.dev = dev;
        self.ino = ino;
        self.ok = true;
        if size <= self.off {
            return Ok(());
        }
        let n = (size - self.off).min(self.cap);
        let mut buf = vec![0u8; n as usize];
        use std::io::{Seek, SeekFrom};
        f.seek(SeekFrom::Start(self.off))
            .map_err(|e| e.to_string())?;
        let mut got = 0;
        while got < buf.len() {
            match f.read(&mut buf[got..]) {
                Ok(0) => break,
                Ok(k) => got += k,
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
                Err(e) => return Err(e.to_string()),
            }
        }
        buf.truncate(got);
        let mut start = 0;
        if self.skipping {
            match buf.iter().position(|&b| b == b'\n') {
                None => {
                    self.off += buf.len() as u64;
                    return Ok(());
                }
                Some(i) => {
                    start = i + 1;
                    self.skipping = false;
                }
            }
        }
        let Some(last) = buf[start..].iter().rposition(|&b| b == b'\n') else {
            if start == 0 && buf.len() as u64 >= self.cap {
                self.skipping = true;
                self.off += buf.len() as u64;
                return Ok(());
            }
            self.off += start as u64;
            return Ok(());
        };
        let end = start + last + 1;
        let body = buf[start..end - 1].to_vec();
        for line in body.split(|&b| b == b'\n') {
            self.line(line);
        }
        self.off += end as u64;
        Ok(())
    }

    fn line(&mut self, b: &[u8]) {
        match self.kind {
            Kind::Claude => {
                if contains(b, b"\"assistant\"") {
                    self.claude_line(b);
                }
            }
            Kind::Sprig => {
                if contains(b, b"\"assistant\"") {
                    self.sprig_assistant(b);
                } else if contains(b, b"\"tool_call\"") || contains(b, b"\"verdict\"") {
                    self.sprig_gate(b);
                }
            }
        }
    }

    fn claude_line(&mut self, b: &[u8]) {
        let Ok(e) = serde_json::from_slice::<ClaudeEntry>(b) else {
            return;
        };
        let Some(m) = e.message.filter(|_| e.kind == "assistant") else {
            return;
        };
        if !m.model.is_empty() && m.model != "<synthetic>" {
            self.model = truncate(&m.model, 200);
        }
        if let Some(u) = m.usage {
            self.count(
                &m.id,
                Tokens {
                    fresh: non_neg(u.input_tokens),
                    cache_read: non_neg(u.cache_read_input_tokens),
                    cache_write: non_neg(u.cache_creation_input_tokens),
                    out: non_neg(u.output_tokens),
                },
            );
        }
    }

    fn sprig_assistant(&mut self, b: &[u8]) {
        let Ok(e) = serde_json::from_slice::<SprigAssistant>(b) else {
            return;
        };
        if e.kind != "assistant" {
            return;
        }
        if !e.model.is_empty() {
            self.model = truncate(&e.model, 200);
        }
        if let Some(u) = e.usage {
            self.count(
                &e.id,
                Tokens {
                    fresh: non_neg(u.fresh),
                    cache_read: non_neg(u.cache_read),
                    cache_write: non_neg(u.cache_write),
                    out: non_neg(u.out),
                },
            );
        }
    }

    fn sprig_gate(&mut self, b: &[u8]) {
        let Ok(e) = serde_json::from_slice::<SprigGate>(b) else {
            return;
        };
        match e.kind.as_str() {
            "tool_call" => {
                self.last_call_id = e.tool_use_id;
                self.last_call_name = e.name;
            }
            "verdict" => {
                if e.decision != "allow" && e.decision != "deny" {
                    return;
                }
                let mut v = GateFact {
                    decision: e.decision,
                    tool: String::new(),
                    clause: truncate(&e.clause, 200),
                    at: e.ts,
                };
                if !e.tool_use_id.is_empty() && e.tool_use_id == self.last_call_id {
                    v.tool = truncate(&self.last_call_name, 200);
                }
                self.verdict = Some(v);
                match e.mode.as_str() {
                    "enforce" => self.mode = "enforcing".into(),
                    "audit" | "shadow" => self.mode = "watching".into(),
                    _ => {}
                }
            }
            _ => {}
        }
    }

    /// Adds one entry's usage. An id counted already replaces its count.
    fn count(&mut self, id: &str, u: Tokens) {
        if id.is_empty() {
            self.sum.add(u);
            return;
        }
        if let Some(prev) = self.by_id.get(id).copied() {
            self.sum.sub(prev);
            self.sum.add(u);
            self.by_id.insert(id.to_string(), u);
            return;
        }
        self.sum.add(u);
        self.by_id.insert(id.to_string(), u);
        self.ids.push_back(id.to_string());
        if self.ids.len() > ID_KEEP {
            if let Some(old) = self.ids.pop_front() {
                self.by_id.remove(&old);
            }
        }
    }
}

/// One file a pane reads: its tailer, and the copy rows read.
struct TailEntry {
    tailer: Arc<Mutex<Tailer>>,
    kind: Kind,
    snap: TailSnap,
}

/// A file let go of: where reading stopped and its tokens.
#[derive(Clone)]
struct RetiredAt {
    off: u64,
    dev: u64,
    ino: u64,
    skipping: bool,
    sum: Tokens,
}

/// What the floor knows about one pane beyond its state.
#[derive(Default)]
pub struct PaneFacts {
    mode: String,
    verdict: Option<GateFact>,
    path: String,
    files: HashMap<String, TailEntry>,
    order: Vec<String>,
    retired: HashMap<String, RetiredAt>,
    retired_order: Vec<String>,
    carried: Tokens,
    model: String,
    polling: bool,
    last_poll: Option<Instant>,
    /// When a report came or tokens grew, as unix seconds.
    seen: Option<f64>,
}

impl PaneFacts {
    /// Makes path the file this pane reads now. It returns the paths it
    /// forgot for good.
    fn use_path(&mut self, path: &str, kind: Kind) -> Vec<String> {
        self.path = path.to_string();
        if self.files.contains_key(path) {
            self.order.retain(|p| p != path);
            self.order.push(path.to_string());
            return Vec::new();
        }
        let mut t = Tailer::new(path, kind);
        if let Some(at) = self.retired.get(path).cloned() {
            t.off = at.off;
            t.dev = at.dev;
            t.ino = at.ino;
            t.skipping = at.skipping;
            t.opened = true;
            t.sum = at.sum;
            t.ok = true;
            self.forget_retired(path);
        }
        let snap = t.snapshot();
        self.files.insert(
            path.to_string(),
            TailEntry {
                tailer: Arc::new(Mutex::new(t)),
                kind,
                snap,
            },
        );
        self.order.push(path.to_string());
        let mut forgot = Vec::new();
        if self.order.len() <= MAX_FILES_PER_PANE {
            return forgot;
        }
        let old = self.order.remove(0);
        let Some(sn) = self.files.remove(&old).map(|e| e.snap) else {
            return forgot;
        };
        if sn.refused {
            return vec![old];
        }
        self.retired.insert(
            old.clone(),
            RetiredAt {
                off: sn.off,
                dev: sn.dev,
                ino: sn.ino,
                skipping: sn.skipping,
                sum: sn.sum,
            },
        );
        self.retired_order.push(old);
        if self.retired_order.len() > RETIRED_KEEP {
            let gone = self.retired_order[0].clone();
            if let Some(r) = self.retired.get(&gone) {
                self.carried.add(r.sum);
            }
            self.forget_retired(&gone);
            forgot.push(gone);
        }
        forgot
    }

    fn forget_retired(&mut self, path: &str) {
        self.retired.remove(path);
        self.retired_order.retain(|p| p != path);
    }

    /// Forgets path and what it counted: another pane counts it now.
    fn drop_path(&mut self, path: &str) {
        self.forget_retired(path);
        if self.files.remove(path).is_none() {
            return;
        }
        self.order.retain(|p| p != path);
        if self.path == path {
            self.path.clear();
        }
    }

    /// The pane's summed tokens. None while no file was ever read.
    fn tokens(&self) -> Option<Tokens> {
        let mut sum = self.carried;
        let mut ok = self.carried != Tokens::default();
        for e in self.files.values() {
            if e.snap.ok {
                ok = true;
            }
            sum.add(e.snap.sum);
        }
        for r in self.retired.values() {
            ok = true;
            sum.add(r.sum);
        }
        ok.then_some(sum)
    }
}

/// A copy of one pane's facts for a row.
#[derive(Default)]
struct FactsView {
    model: String,
    tokens: Option<Tokens>,
    mode: String,
    verdict: Option<GateFact>,
    seen: Option<f64>,
}

/// The facts of every pane, and which pane reads each transcript, under
/// one lock.
#[derive(Default)]
pub struct FactsMaps {
    facts: HashMap<String, Arc<Mutex<PaneFacts>>>,
    claims: HashMap<String, String>,
}

/// What the floor last read about the gateway.
#[derive(Default)]
pub struct Outside {
    read_at: Option<Instant>,
    gateway: String,
    router: String,
    dial_at: Option<Instant>,
    dial_for: String,
    answers: bool,
}

/// The providers' own API addresses.
const VENDOR_APIS: [&str; 2] = ["https://api.anthropic.com", "https://api.openai.com"];

/// A URL's scheme, host and port, the way Go's url.Parse reads them.
/// Every loopback name is one host, and a missing port is the scheme's.
fn endpoint(raw: &str) -> Option<String> {
    let (scheme, host, port) = split_url(raw)?;
    let scheme = scheme.to_lowercase();
    let port = if port.is_empty() {
        match scheme.as_str() {
            "http" => "80".to_string(),
            "https" => "443".to_string(),
            _ => return None,
        }
    } else {
        port
    };
    let mut host = host.to_lowercase();
    if is_loopback_name(&host) {
        host = "loopback".into();
    }
    Some(format!("{scheme}://{host}:{port}"))
}

/// The scheme, the host name without brackets, and the port of a URL with
/// an authority. None when Go's url.Parse fails or finds no host.
fn split_url(raw: &str) -> Option<(String, String, String)> {
    if raw.chars().any(|c| (c as u32) < 0x20 || c as u32 == 0x7f) {
        return None;
    }
    let colon = raw.find(':')?;
    let scheme = &raw[..colon];
    let mut sc = scheme.chars();
    let first = sc.next()?;
    if !first.is_ascii_alphabetic()
        || !sc.all(|c| c.is_ascii_alphanumeric() || c == '+' || c == '-' || c == '.')
    {
        return None;
    }
    let rest = raw[colon + 1..].split('#').next().unwrap_or("");
    let rest = rest.strip_prefix("//")?;
    let end = rest.find(['/', '?']).unwrap_or(rest.len());
    let mut auth = &rest[..end];
    if let Some(at) = auth.rfind('@') {
        auth = &auth[at + 1..];
    }
    if auth.is_empty() {
        return None;
    }
    let (host, port) = if let Some(stripped) = auth.strip_prefix('[') {
        let close = stripped.find(']')?;
        let host = &stripped[..close];
        let after = &stripped[close + 1..];
        let port = match after.strip_prefix(':') {
            Some(p) => p,
            None if after.is_empty() => "",
            None => return None,
        };
        (host.to_string(), port.to_string())
    } else {
        match auth.rfind(':') {
            Some(i) => (auth[..i].to_string(), auth[i + 1..].to_string()),
            None => (auth.to_string(), String::new()),
        }
    };
    if !port.chars().all(|c| c.is_ascii_digit()) {
        return None;
    }
    Some((scheme.to_string(), host, port))
}

fn is_loopback_name(host: &str) -> bool {
    if host == "localhost" {
        return true;
    }
    match host.parse::<std::net::IpAddr>() {
        Ok(std::net::IpAddr::V4(v)) => v.is_loopback(),
        Ok(std::net::IpAddr::V6(v)) => {
            v.is_loopback() || v.to_ipv4_mapped().is_some_and(|m| m.is_loopback())
        }
        Err(_) => false,
    }
}

fn same_endpoint(a: &str, b: &str) -> bool {
    match (endpoint(a), endpoint(b)) {
        (Some(x), Some(y)) => x == y,
        _ => false,
    }
}

/// The gateway_router key of daisugi's config.yaml, read from its one
/// line shape. "" when there is none.
fn daisugi_router(path: &Path) -> String {
    let Ok(text) = std::fs::read_to_string(path) else {
        return String::new();
    };
    for line in text.lines() {
        if let Some(rest) = line.strip_prefix("gateway_router:") {
            let rest = rest.split('#').next().unwrap_or("");
            return rest.trim().trim_matches(['"', '\'']).to_string();
        }
    }
    String::new()
}

fn canon_harness(h: &str) -> &str {
    if h == "claude-code" {
        "claude"
    } else {
        h
    }
}

fn same_harness(reported: &str, pane: &str) -> bool {
    !pane.is_empty() && canon_harness(reported) == canon_harness(pane)
}

/// The local calendar date of a unix time, as libc's localtime reads it.
fn local_date(secs: f64) -> (i32, i32, i32) {
    let t = secs.floor() as libc::time_t;
    // SAFETY: localtime_r writes only into tm, which lives on this frame.
    unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        if libc::localtime_r(&t, &mut tm).is_null() {
            return (0, 0, 0);
        }
        (tm.tm_year, tm.tm_mon, tm.tm_mday)
    }
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "floor.facts",
        Arc::new(|s, c, r| s.handle_floor_facts(c, r)),
    );
}

impl Server {
    fn facts_maps(&self) -> std::sync::MutexGuard<'_, FactsMaps> {
        self.facts.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn outside_lock(&self) -> std::sync::MutexGuard<'_, Outside> {
        self.outside.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The gateway base URL and what routes behind it, read at most every
    /// OUTSIDE_TTL.
    fn gateway_facts(&self) -> (String, String) {
        let mut g = self.outside_lock();
        if let Some(at) = g.read_at {
            if at.elapsed() < OUTSIDE_TTL {
                return (g.gateway.clone(), g.router.clone());
            }
        }
        let gateway = match config::load() {
            Ok(Some(c)) => c.gateway_url(),
            Ok(None) => config::Config::default().gateway_url(),
            Err(_) => String::new(),
        };
        let cfg_yaml = self
            .gate_root()
            .parent()
            .map(|p| p.join("config.yaml"))
            .unwrap_or_default();
        let router = if daisugi_router(&cfg_yaml) == "switchyard" {
            "switchyard"
        } else {
            "gateway"
        };
        g.read_at = Some(Instant::now());
        g.gateway = gateway.clone();
        g.router = router.to_string();
        (gateway, router.to_string())
    }

    /// Whether something accepts a connection at the gateway's address.
    /// Only a loopback address is dialed, and the answer is kept for
    /// OUTSIDE_TTL. None when there is no loopback gateway to dial.
    fn gateway_answers(&self, gateway: &str) -> Option<bool> {
        let (_, host, _) = split_url(gateway)?;
        if !is_loopback_name(&host.to_lowercase()) {
            return None;
        }
        let e = endpoint(gateway)?;
        let port: u16 = e[e.rfind(':')? + 1..].parse().ok()?;
        {
            let g = self.outside_lock();
            if g.dial_for == gateway && g.dial_at.is_some_and(|t| t.elapsed() < OUTSIDE_TTL) {
                return Some(g.answers);
            }
        }
        let host = if host == "localhost" {
            "127.0.0.1".to_string()
        } else {
            host
        };
        let answers = match host.parse::<std::net::IpAddr>() {
            Ok(ip) => std::net::TcpStream::connect_timeout(
                &std::net::SocketAddr::new(ip, port),
                GATEWAY_DIAL_TIMEOUT,
            )
            .is_ok(),
            Err(_) => false,
        };
        let mut g = self.outside_lock();
        g.dial_at = Some(Instant::now());
        g.dial_for = gateway.to_string();
        g.answers = answers;
        Some(answers)
    }

    /// One variable as the pane's process sees it.
    fn pane_env(&self, p: &layout::Pane, key: &str) -> String {
        if let Some(v) = p.env.get(key) {
            return v.clone();
        }
        std::env::var(key).unwrap_or_default()
    }

    /// Who routes a pane's model calls: the gateway's router when its base
    /// URL is the gateway, "direct" for anything else, "" when none is set.
    fn router_of(&self, p: &layout::Pane) -> String {
        let bases: Vec<String> = ["ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"]
            .iter()
            .map(|k| self.pane_env(p, k).trim().to_string())
            .filter(|v| !v.is_empty())
            .collect();
        if bases.is_empty() {
            return String::new();
        }
        let (gateway, router) = self.gateway_facts();
        if gateway.is_empty() {
            for b in &bases {
                if VENDOR_APIS.iter().any(|v| same_endpoint(b, v)) {
                    return "direct".into();
                }
            }
            return String::new();
        }
        if bases.iter().any(|b| same_endpoint(b, &gateway)) {
            return router;
        }
        "direct".into()
    }

    fn disarmed(&self) -> bool {
        std::fs::symlink_metadata(self.gate_root().join("DISARMED")).is_ok()
    }

    fn facts_for(&self, id: &str, make: bool) -> Option<Arc<Mutex<PaneFacts>>> {
        let mut g = self.facts_maps();
        if let Some(pf) = g.facts.get(id) {
            return Some(pf.clone());
        }
        if !make {
            return None;
        }
        let pf = Arc::new(Mutex::new(PaneFacts::default()));
        g.facts.insert(id.to_string(), pf.clone());
        Some(pf)
    }

    /// Gives path to pane id, unless another live pane reported it. A
    /// path an ended pane held moves to id; from names that pane.
    fn claim(&self, id: &str, path: &str) -> (bool, String) {
        let mut g = self.facts_maps();
        let mut from = String::new();
        if let Some(owner) = g.claims.get(path) {
            if owner != id {
                if self.tree.pane(owner).is_some_and(|r| !r.closed) {
                    return (false, String::new());
                }
                from = owner.clone();
            }
        }
        g.claims.insert(path.to_string(), id.to_string());
        (true, from)
    }

    fn release(&self, id: &str, paths: &[String]) {
        if paths.is_empty() {
            return;
        }
        let mut g = self.facts_maps();
        for p in paths {
            if g.claims.get(p).is_some_and(|o| o == id) {
                g.claims.remove(p);
            }
        }
    }

    /// Whether path can be a Claude Code transcript of pane p: a .jsonl
    /// file under the projects directory of the Claude config directory p
    /// sees.
    fn claude_transcript_path(&self, p: &layout::Pane, path: &str) -> bool {
        if !path.ends_with(".jsonl") {
            return false;
        }
        let mut dir = self.pane_env(p, "CLAUDE_CONFIG_DIR");
        if dir.is_empty() {
            let home = self.pane_env(p, "HOME");
            if home.is_empty() {
                return false;
            }
            dir = format!("{home}/.claude");
        }
        if !dir.starts_with('/') {
            return false;
        }
        let mut roots = vec![crate::sys::clean(&dir)];
        if let Ok(r) = std::fs::canonicalize(&dir) {
            let r = r.to_string_lossy().into_owned();
            if r != roots[0] {
                roots.push(r);
            }
        }
        roots.iter().any(|r| {
            let base = crate::sys::clean(&format!("{r}/projects"));
            path.starts_with(&format!("{base}/"))
        })
    }

    /// Keeps what one report said beyond the state: the mode, the verdict,
    /// and, from the pane's own connection on a Claude pane, the
    /// transcript path.
    pub fn note_report(&self, id: &str, harness: &str, x: &ReportExtras, own: bool) {
        let now = Server::now_seconds();
        let (mut claimed, mut from) = (false, String::new());
        if let Some(rec) = self.tree.pane(id) {
            if own
                && !x.transcript_path.is_empty()
                && canon_harness(harness) == "claude"
                && same_harness(harness, &rec.harness)
                && self.claude_transcript_path(&rec, &x.transcript_path)
            {
                (claimed, from) = self.claim(id, &x.transcript_path);
            }
        }
        if !from.is_empty() {
            if let Some(old) = self.facts_for(&from, false) {
                old.lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .drop_path(&x.transcript_path);
            }
        }
        let forgot = {
            let pf = self.facts_for(id, true).expect("made");
            let mut pf = pf.lock().unwrap_or_else(|e| e.into_inner());
            if !x.mode.is_empty() {
                pf.mode = x.mode.clone();
            }
            if let Some(v) = &x.verdict {
                pf.verdict = Some(GateFact {
                    decision: v.decision.clone(),
                    tool: v.tool.clone(),
                    clause: v.clause.clone(),
                    at: now,
                });
            }
            let forgot = if claimed {
                pf.use_path(&x.transcript_path, Kind::Claude)
            } else {
                Vec::new()
            };
            pf.seen = Some(now);
            forgot
        };
        self.release(id, &forgot);
    }

    /// Reads one tailer off the request path, then publishes what it
    /// found. A read longer than POLL_DEADLINE marks the path refused.
    fn run_poll(
        pf: Arc<Mutex<PaneFacts>>,
        path: String,
        t: Arc<Mutex<Tailer>>,
        done: Arc<crate::pane::Flag>,
    ) {
        let start = Instant::now();
        let sn = {
            let mut tl = t.lock().unwrap_or_else(|e| e.into_inner());
            let _ = tl.poll();
            if start.elapsed() > POLL_DEADLINE {
                tl.refused = true;
                tl.ok = false;
            }
            tl.snapshot()
        };
        {
            let mut g = pf.lock().unwrap_or_else(|e| e.into_inner());
            g.polling = false;
            let same = g
                .files
                .get(&path)
                .is_some_and(|e| Arc::ptr_eq(&e.tailer, &t));
            if same {
                let grew = g.files.get(&path).is_some_and(|e| e.snap.sum != sn.sum);
                if grew {
                    g.seen = Some(Server::now_seconds());
                }
                if !sn.model.is_empty() {
                    g.model = sn.model.clone();
                }
                if let Some(e) = g.files.get_mut(&path) {
                    e.snap = sn;
                }
            }
        }
        done.set();
    }

    /// A copy of a pane's facts, first starting a read of its transcript
    /// when the last one began at least FACTS_EVERY ago and none is in
    /// flight. A row waits POLL_WAIT for that read, then shows what is
    /// known.
    fn view_facts(&self, p: &layout::Pane) -> FactsView {
        // A headless sprig pane's session tree is the transcript; no hook
        // reports it.
        let sprig_path = if p.harness == "sprig" && p.kind == layout::KIND_HEADLESS {
            crate::adapters::sprig::tree_path(
                p.argv.as_deref().unwrap_or_default(),
                &p.harness_session_id,
            )
        } else {
            None
        };
        let Some(pf) = self.facts_for(&p.id, sprig_path.is_some()) else {
            return FactsView::default();
        };
        let mut done = None;
        {
            let mut g = pf.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(sp) = &sprig_path {
                if g.path != *sp {
                    let _ = g.use_path(sp, Kind::Sprig);
                }
            }
            let path = g.path.clone();
            let due = g.last_poll.is_none_or(|t| t.elapsed() >= FACTS_EVERY);
            if let Some(e) = g.files.get(&path) {
                if !p.closed && !g.polling && !e.snap.refused && due {
                    let t = e.tailer.clone();
                    g.polling = true;
                    g.last_poll = Some(Instant::now());
                    let flag = Arc::new(crate::pane::Flag::default());
                    done = Some(flag.clone());
                    let pf2 = pf.clone();
                    thread::spawn(move || Server::run_poll(pf2, path, t, flag));
                }
            }
        }
        if let Some(d) = done {
            d.wait(Some(POLL_WAIT));
        }
        let g = pf.lock().unwrap_or_else(|e| e.into_inner());
        let mut v = FactsView {
            model: g.model.clone(),
            mode: g.mode.clone(),
            verdict: g.verdict.clone(),
            seen: g.seen,
            tokens: g.tokens(),
        };
        if let Some(e) = g.files.get(&g.path) {
            if e.kind == Kind::Sprig {
                if v.mode.is_empty() {
                    v.mode = e.snap.mode.clone();
                }
                if let Some(tv) = &e.snap.verdict {
                    let mut c = tv.clone();
                    let now = Server::now_seconds();
                    if c.at > now {
                        c.at = now;
                    }
                    if v.verdict.as_ref().is_none_or(|x| c.at >= x.at) {
                        v.verdict = Some(c);
                    }
                }
            }
        }
        v
    }

    /// Drops the facts of every pane no longer in the tree.
    pub fn prune_facts(&self) {
        let mut g = self.facts_maps();
        g.facts.retain(|id, _| self.tree.pane(id).is_some());
        let FactsMaps { facts, claims } = &mut *g;
        claims.retain(|_, id| facts.contains_key(id));
    }

    /// A row's stack object: loop, model, router and daisugi, each only
    /// when known.
    fn stack_of(&self, p: &layout::Pane, v: &FactsView) -> Option<Value> {
        let mut st: Vec<(&str, Value)> = Vec::new();
        if !p.harness.is_empty() {
            st.push(("loop", json!(p.harness)));
        }
        if !v.model.is_empty() {
            st.push(("model", json!(v.model)));
        }
        let router = self.router_of(p);
        if !router.is_empty() {
            st.push(("router", json!(router)));
        }
        if !p.harness.is_empty() && !p.closed {
            let mode = if v.mode.is_empty() { MODE_OFF } else { &v.mode };
            st.push((
                "daisugi",
                gojson::map(vec![
                    ("mode", json!(mode)),
                    ("armed", json!(!self.disarmed())),
                ]),
            ));
        }
        (!st.is_empty()).then(|| gojson::map(st))
    }

    /// Puts stack, gate and tokens on one pane.list row, each only when
    /// known.
    pub fn add_facts(&self, row: &mut Vec<(&'static str, Value)>, p: &layout::Pane) {
        let v = self.view_facts(p);
        if let Some(st) = self.stack_of(p, &v) {
            row.push(("stack", st));
        }
        if let Some(g) = &v.verdict {
            row.push(("gate", g.to_value()));
        }
        if let Some(t) = v.tokens {
            row.push(("tokens", t.to_value()));
        }
    }

    /// Whether a harness settings file under the home names the gate.
    fn gate_installed(&self) -> bool {
        let Some(home) = crate::sys::home_dir() else {
            return false;
        };
        [".claude/settings.json", ".codex/hooks.json"]
            .iter()
            .any(|f| {
                std::fs::read(home.join(f))
                    .is_ok_and(|b| String::from_utf8_lossy(&b).contains("opendaisugi.gate"))
            })
    }

    /// floor.facts: the facts a floor shows in its header row.
    fn handle_floor_facts(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        self.prune_facts();
        let now = Server::now_seconds();
        let today = local_date(now);
        let mut counts: HashMap<&str, i64> = [(MODE_OFF, 0), ("watching", 0), ("enforcing", 0)]
            .into_iter()
            .collect();
        let (mut harnesses, mut working, mut needing) = (0, 0, 0);
        let mut tokens_today = Tokens::default();
        for p in self.tree.panes() {
            let v = self.view_facts(&p);
            if let (Some(t), Some(seen)) = (v.tokens, v.seen) {
                if local_date(seen) == today {
                    tokens_today.add(t);
                }
            }
            if p.closed {
                continue;
            }
            if let Some(eff) = self.effective_state(&p.id) {
                // A blocked pane whose foreman hears the ask first counts
                // as working, as every floor counts it from pane.list.
                let held = eff.state == state::STATE_BLOCKED && self.held_for(&p.id).is_some();
                if eff.state == state::STATE_WORKING || held {
                    working += 1;
                } else if eff.state == state::STATE_BLOCKED {
                    needing += 1;
                }
            }
            if !p.harness.is_empty() {
                let mode: &'static str = match v.mode.as_str() {
                    "watching" => "watching",
                    "enforcing" => "enforcing",
                    _ => MODE_OFF,
                };
                *counts.entry(mode).or_default() += 1;
                harnesses += 1;
            }
        }
        let rank = |m: &str| match m {
            "watching" => 1,
            "enforcing" => 2,
            _ => 0,
        };
        let mut mode = MODE_OFF;
        if harnesses > 0 {
            mode = "enforcing";
            for (m, n) in &counts {
                if *n > 0 && rank(m) < rank(mode) {
                    mode = m;
                }
            }
        }
        let mut res: Vec<(&str, Value)> = vec![
            (
                "daisugi",
                gojson::map(vec![
                    ("mode", json!(mode)),
                    ("armed", json!(!self.disarmed())),
                    ("enforcing", json!(counts["enforcing"])),
                    ("watching", json!(counts["watching"])),
                    ("off", json!(counts[MODE_OFF])),
                    ("installed", json!(self.gate_installed())),
                ]),
            ),
            ("working", json!(working)),
            ("needing_you", json!(needing)),
            ("tokens_today", tokens_today.to_value()),
        ];
        let fid = {
            let mut t = self.talk_lock();
            self.tracked_foreman(&mut t)
        };
        if self.foreman_state(&fid).0 {
            res.push((
                "foreman",
                gojson::map(vec![
                    ("pane", json!(fid)),
                    ("label", json!(self.label_of(&fid))),
                ]),
            ));
        }
        let (gateway, _) = self.gateway_facts();
        if !gateway.is_empty() {
            let mut gw: Vec<(&str, Value)> = vec![("url", json!(gateway))];
            if let Some(a) = self.gateway_answers(&gateway) {
                gw.push(("answers", json!(a)));
            }
            res.push(("gateway", gojson::map(gw)));
        }
        proto::ok_resp(&r.id, gojson::map(res))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture(name: &str) -> String {
        format!(
            "{}/../coppice/testdata/facts/{name}",
            env!("CARGO_MANIFEST_DIR")
        )
    }

    fn scratch(name: &str) -> std::path::PathBuf {
        let d =
            std::env::temp_dir().join(format!("coppice-rs-facts-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn endpoints_compare_by_scheme_host_and_port() {
        assert!(same_endpoint(
            "http://localhost:8787/v1",
            "http://127.0.0.1:8787"
        ));
        assert!(same_endpoint(
            "https://api.anthropic.com",
            "https://API.anthropic.com:443/x"
        ));
        assert!(!same_endpoint(
            "http://127.0.0.1:8787",
            "http://127.0.0.1:8788"
        ));
        assert!(!same_endpoint("not a url", "not a url"));
        assert!(same_endpoint("http://[::1]:9/", "http://localhost:9"));
    }

    #[test]
    fn a_claude_transcript_counts_each_message_once() {
        let mut t = Tailer::new(&fixture("claude-transcript.jsonl"), Kind::Claude);
        t.poll().unwrap();
        let want = Tokens {
            fresh: 13,
            cache_read: 300,
            cache_write: 20,
            out: 12,
        };
        assert_eq!(t.sum, want);
        assert_eq!(t.model, "claude-fake-2");
    }

    #[test]
    fn a_claude_transcript_reads_on_from_where_it_stopped() {
        let d = scratch("append");
        let p = d.join("t.jsonl");
        std::fs::copy(fixture("claude-transcript.jsonl"), &p).unwrap();
        let mut t = Tailer::new(p.to_str().unwrap(), Kind::Claude);
        t.poll().unwrap();
        let more = std::fs::read(fixture("claude-transcript-more.jsonl")).unwrap();
        let mut f = std::fs::OpenOptions::new().append(true).open(&p).unwrap();
        std::io::Write::write_all(&mut f, &more).unwrap();
        t.poll().unwrap();
        let want = Tokens {
            fresh: 14,
            cache_read: 1300,
            cache_write: 22,
            out: 15,
        };
        assert_eq!((t.sum, t.model.as_str()), (want, "claude-fake-3"));
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_line_not_yet_ended_waits() {
        let d = scratch("partial");
        let p = d.join("t.jsonl");
        let line = r#"{"type":"assistant","message":{"id":"m1","model":"claude-fake-1","usage":{"input_tokens":2,"output_tokens":1}}}"#;
        std::fs::write(&p, line).unwrap();
        let mut t = Tailer::new(p.to_str().unwrap(), Kind::Claude);
        t.poll().unwrap();
        assert_eq!((t.off, t.model.as_str()), (0, ""));
        std::fs::write(&p, format!("{line}\n")).unwrap();
        t.poll().unwrap();
        assert_eq!(t.model, "claude-fake-1");
        assert_eq!((t.sum.fresh, t.sum.out), (2, 1));
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_line_longer_than_the_cap_is_skipped() {
        let d = scratch("long");
        let p = d.join("t.jsonl");
        let long = format!(
            "{}{}\"}}}}\n",
            r#"{"type":"assistant","message":{"id":"big","model":"too-big","text":""#,
            "x".repeat(300)
        );
        let good = r#"{"type":"assistant","message":{"id":"m2","model":"claude-fake-2","usage":{"output_tokens":4}}}"#;
        std::fs::write(&p, format!("{long}{good}\n")).unwrap();
        let mut t = Tailer::new(p.to_str().unwrap(), Kind::Claude);
        t.cap = 100;
        for _ in 0..10 {
            t.poll().unwrap();
        }
        assert_eq!((t.model.as_str(), t.sum.out), ("claude-fake-2", 4));
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_path_that_is_not_a_regular_file_is_refused() {
        let d = scratch("refuse");
        let real = d.join("real.jsonl");
        std::fs::copy(fixture("claude-transcript.jsonl"), &real).unwrap();
        let link = d.join("link.jsonl");
        std::os::unix::fs::symlink(&real, &link).unwrap();
        for p in [link, d.clone()] {
            let mut t = Tailer::new(p.to_str().unwrap(), Kind::Claude);
            assert!(t.poll().is_err());
            assert!(t.refused);
            assert_eq!((t.sum, t.model.as_str()), (Tokens::default(), ""));
        }
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_sprig_tree_counts_tokens_and_the_last_verdict() {
        let mut t = Tailer::new(&fixture("sprig-tree.jsonl"), Kind::Sprig);
        t.poll().unwrap();
        let want = Tokens {
            fresh: 5,
            cache_read: 90,
            cache_write: 8,
            out: 5,
        };
        assert_eq!((t.sum, t.model.as_str()), (want, "sprig-fake-2"));
        let mut t = Tailer::new(&fixture("sprig-tree-writer-shape.jsonl"), Kind::Sprig);
        t.poll().unwrap();
        let want = Tokens {
            fresh: 11,
            cache_read: 22,
            cache_write: 33,
            out: 44,
        };
        assert_eq!((t.sum, t.model.as_str()), (want, "haiku"));
    }

    #[test]
    fn a_pane_forgets_its_oldest_file_past_the_cap() {
        let mut pf = PaneFacts::default();
        for i in 0..=MAX_FILES_PER_PANE {
            let forgot = pf.use_path(&format!("/x/{i}.jsonl"), Kind::Claude);
            assert!(forgot.is_empty());
        }
        assert_eq!(pf.files.len(), MAX_FILES_PER_PANE);
        assert!(pf.retired.contains_key("/x/0.jsonl"));
        pf.use_path("/x/0.jsonl", Kind::Claude);
        assert!(!pf.retired.contains_key("/x/0.jsonl"));
        assert!(pf.files.contains_key("/x/0.jsonl"));
    }
}
