//! The foreman's failover (FM-R-45 to FM-R-54). The twin of Go's
//! internal/foreman failover.go.
//!
//! A turn tries an ordered chain of profiles. A profile that is out of
//! quota, overloaded or not reachable cools down, and the turn moves to the
//! next one; when the cool-down ends the next turn tries the first profile
//! again. This file holds the two pure parts, with no clock of its own and
//! no I/O but one small state file: the classifier that reads a failed turn,
//! and the chain with its cool-downs. All times are Unix seconds, whole.

use std::path::Path;
use std::sync::{Mutex, OnceLock};

use regex::Regex;

use super::fsutil::json_str;

/// What a failed turn means for the chain.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    /// The failure is not an outage. The turn fails as it always did and
    /// the chain does not move.
    NotOut,
    /// The profile is out of quota, overloaded or not reachable. It cools
    /// down and the turn moves on.
    Out,
    /// HTTP 401 or 403. A key is wrong, which a move would hide, so the turn
    /// fails and the chain does not move.
    KeyProblem,
}

/// What `classify` makes of a failed turn.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Verdict {
    pub outcome: Outcome,
    /// A short plain phrase: "HTTP 429", "usage limit reached".
    pub reason: String,
    /// When the reply says the profile is usable again, in Unix seconds.
    pub reset: Option<i64>,
}

/// How much of a failure's text the classifier reads: the last 4 KiB, where
/// a runner puts the code and the headers.
pub const CLASSIFY_BYTES: usize = 4096;

/// The last n bytes of s, cut at a character start.
pub fn tail(s: &str, n: usize) -> &str {
    if s.len() <= n {
        return s;
    }
    let mut i = s.len() - n;
    while !s.is_char_boundary(i) {
        i += 1;
    }
    &s[i..]
}

static KEY_REASON: OnceLock<Regex> = OnceLock::new();

/// The only form of a failure that is saved or shown. It is information
/// only, so it is never raw error text: a fixed phrase of the classifier (a
/// quota text, "cannot connect", the key-refused line), else "HTTP nnn" when
/// the text names a status, else "the turn failed". A key cannot survive it,
/// whatever its shape (FM-R-60).
pub fn safe_reason(s: &str) -> String {
    let s = s.trim();
    if re(
        &KEY_REASON,
        r"^HTTP \d{3}: the key was refused\. That is a key problem, not a quota$",
    )
    .is_match(s)
        || s == "cannot connect"
        || OUT_TEXTS.contains(&s)
    {
        return s.to_string();
    }
    if let Some(c) = re(
        &STATUS,
        r"(?i)\b(?:api|http|status|status code|error)[: ]+(\d{3})\b",
    )
    .captures(s)
    {
        return format!("HTTP {}", &c[1]);
    }
    "the turn failed".to_string()
}

/// The known texts of a quota or a usage limit, lower case, matched anywhere
/// in the failure in any case. A new provider text is one line here and one
/// case in the test, which walks this list.
pub const OUT_TEXTS: &[&str] = &[
    // Anthropic API.
    "rate_limit_error",
    "overloaded_error",
    "credit balance is too low",
    "reached your specified api usage limits",
    // OpenAI API and its compatible servers.
    "insufficient_quota",
    "you exceeded your current quota",
    "rate_limit_exceeded",
    "rate limit reached for",
    // Others that speak the same wire.
    "resource_exhausted",
    // Claude Code (claude -p).
    "usage limit reached",
    "claude ai usage limit",
    "5-hour limit reached",
    "weekly limit reached",
    "you've hit your limit",
    "limit reached \u{2219} resets",
];

/// The texts of a profile that cannot be reached, lower case.
const CONN_TEXTS: &[&str] = &[
    "connection refused",
    "no such host",
    "no route to host",
    "network is unreachable",
    "tcp connect error",
    "dns error",
    "failed to lookup address",
    "temporary failure in name resolution",
    "connect: connection timed out",
];

/// Caps a reset time that a reply names.
pub const MAX_NAMED_RESET: i64 = 7 * 24 * 3600;

fn re(cell: &'static OnceLock<Regex>, pat: &str) -> &'static Regex {
    cell.get_or_init(|| Regex::new(pat).expect("a fixed pattern"))
}

static STATUS: OnceLock<Regex> = OnceLock::new();
static DIAL: OnceLock<Regex> = OnceLock::new();
static RETRY: OnceLock<Regex> = OnceLock::new();
static ANTH: OnceLock<Regex> = OnceLock::new();
static OAI: OnceLock<Regex> = OnceLock::new();
static EPOCH: OnceLock<Regex> = OnceLock::new();

/// Reads the text of a failed turn. A key problem (401, 403) comes first so
/// that a bad key is never taken for a quota. Then come the codes of an
/// outage (429, 529, 503), the known quota texts, and a profile that cannot
/// be connected to. Everything else (400, a model error, a tool error, a bad
/// answer) is NotOut. now is the caller's clock in Unix seconds.
pub fn classify(text: &str, now: i64) -> Verdict {
    let text = tail(text, CLASSIFY_BYTES);
    let low = text.to_lowercase();
    let codes: Vec<&str> = re(
        &STATUS,
        r"(?i)\b(?:api|http|status|status code|error)[: ]+(\d{3})\b",
    )
    .captures_iter(text)
    .map(|c| c.get(1).map(|m| m.as_str()).unwrap_or(""))
    .collect();
    for c in &codes {
        if *c == "401" || *c == "403" {
            return Verdict {
                outcome: Outcome::KeyProblem,
                reason: format!("HTTP {c}: the key was refused. That is a key problem, not a quota"),
                reset: None,
            };
        }
    }
    for c in &codes {
        if *c == "429" || *c == "529" || *c == "503" {
            return out(&format!("HTTP {c}"), text, now);
        }
    }
    for t in OUT_TEXTS {
        if low.contains(t) {
            return out(t, text, now);
        }
    }
    for t in CONN_TEXTS {
        if low.contains(t) {
            return out("cannot connect", text, now);
        }
    }
    if re(&DIAL, r"(?i)dial tcp[^\n]*i/o timeout").is_match(text) {
        return out("cannot connect", text, now);
    }
    Verdict {
        outcome: Outcome::NotOut,
        reason: String::new(),
        reset: None,
    }
}

fn out(reason: &str, text: &str, now: i64) -> Verdict {
    Verdict {
        outcome: Outcome::Out,
        reason: reason.into(),
        reset: reset_of(text, now),
    }
}

/// The time a reply names for the end of the limit. Retry-After wins; else
/// the earliest of the rate-limit reset headers; else a Claude Code epoch
/// after the limit text. A time that is not after now is ignored, and one
/// past MAX_NAMED_RESET is cut to it.
fn reset_of(text: &str, now: i64) -> Option<i64> {
    if let Some(c) = re(&RETRY, r"(?im)^\s*retry-after:\s*(\S[^\r\n]*)$").captures(text) {
        if let Some(t) = parse_delay_or_date(c[1].trim(), now) {
            return clamp_reset(t, now);
        }
    }
    let mut cands: Vec<i64> = Vec::new();
    for c in re(&ANTH, r"(?im)\banthropic-ratelimit-[a-z-]*-reset:\s*(\S+)").captures_iter(text) {
        if let Some(t) = crate::server::saved::parse_rfc3339_secs(&c[1]) {
            cands.push(t);
        }
    }
    for c in re(&OAI, r"(?im)\bx-ratelimit-reset-[a-z]+:\s*(\S+)").captures_iter(text) {
        if let Some(t) = parse_delay_or_date(&c[1], now) {
            cands.push(t);
        }
    }
    if let Some(c) = re(&EPOCH, r"(?i)limit reached\|(\d{9,11})\b").captures(text) {
        if let Ok(n) = c[1].parse::<i64>() {
            cands.push(n);
        }
    }
    cands
        .into_iter()
        .filter(|t| *t > now)
        .min()
        .and_then(|t| clamp_reset(t, now))
}

fn clamp_reset(t: i64, now: i64) -> Option<i64> {
    if t <= now {
        return None;
    }
    Some(t.min(now + MAX_NAMED_RESET))
}

/// A delay in seconds ("90", "1.5"), a Go-style duration ("6m0s"), or an
/// HTTP date; a number past 1e9 is an epoch. Whole seconds, rounded down.
fn parse_delay_or_date(s: &str, now: i64) -> Option<i64> {
    if let Ok(f) = s.parse::<f64>() {
        if !f.is_finite() {
            return None;
        }
        if f > 1e9 {
            return Some(f.floor() as i64);
        }
        return Some(now + f.floor() as i64);
    }
    if let Some(d) = parse_go_duration(s) {
        return Some(now + d.floor() as i64);
    }
    parse_http_date(s)
}

/// A Go time.ParseDuration string, in seconds.
pub(crate) fn parse_go_duration(s: &str) -> Option<f64> {
    let (neg, mut rest) = match s.as_bytes().first()? {
        b'-' => (true, &s[1..]),
        b'+' => (false, &s[1..]),
        _ => (false, s),
    };
    if rest.is_empty() {
        return None;
    }
    let mut total = 0.0f64;
    while !rest.is_empty() {
        let end = rest
            .find(|c: char| !(c.is_ascii_digit() || c == '.'))
            .unwrap_or(rest.len());
        let (num, tail) = rest.split_at(end);
        if num.is_empty() || num == "." {
            return None;
        }
        let v: f64 = num.parse().ok()?;
        let uend = tail
            .find(|c: char| c.is_ascii_digit() || c == '.')
            .unwrap_or(tail.len());
        let (unit, next) = tail.split_at(uend);
        let scale = match unit {
            "ns" => 1e-9,
            "us" | "\u{b5}s" | "\u{3bc}s" => 1e-6,
            "ms" => 1e-3,
            "s" => 1.0,
            "m" => 60.0,
            "h" => 3600.0,
            _ => return None,
        };
        total += v * scale;
        rest = next;
    }
    if total > 9.2e9 {
        return None;
    }
    Some(if neg { -total } else { total })
}

/// An HTTP date in the IMF-fixdate form, "Wed, 21 Oct 2026 12:30:00 GMT".
fn parse_http_date(s: &str) -> Option<i64> {
    let parts: Vec<&str> = s.split(' ').collect();
    if parts.len() != 6 || parts[5] != "GMT" || !parts[0].ends_with(',') {
        return None;
    }
    const DAYS: [&str; 7] = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
    const MONTHS: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    if !DAYS.contains(&&parts[0][..parts[0].len() - 1]) {
        return None;
    }
    let d: i64 = parts[1].parse().ok().filter(|_| parts[1].len() == 2)?;
    let m = MONTHS.iter().position(|x| *x == parts[2])? as i64 + 1;
    let y: i64 = parts[3].parse().ok().filter(|_| parts[3].len() == 4)?;
    let hms: Vec<&str> = parts[4].split(':').collect();
    if hms.len() != 3 || hms.iter().any(|p| p.len() != 2) {
        return None;
    }
    let (h, mi, sec): (i64, i64, i64) = (
        hms[0].parse().ok()?,
        hms[1].parse().ok()?,
        hms[2].parse().ok()?,
    );
    if !(1..=31).contains(&d) || h > 23 || mi > 59 || sec > 59 {
        return None;
    }
    // Howard Hinnant's days_from_civil.
    let yy = if m <= 2 { y - 1 } else { y };
    let era = yy.div_euclid(400);
    let yoe = yy - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    Some((era * 146097 + doe - 719468) * 86400 + h * 3600 + mi * 60 + sec)
}

/// A cool-down with no named reset starts at COOL_START and doubles on each
/// repeat to COOL_MAX; a success starts it over.
pub const COOL_START: i64 = 15 * 60;
pub const COOL_MAX: i64 = 2 * 3600;

/// The file in the log dir that holds the chain's state. The server writes
/// it; `coppice foreman status` reads it.
pub const FAILOVER_FILE: &str = "failover.json";

/// How a profile reads to the operator: Claude Code for the empty name and
/// "default".
pub fn profile_name(p: &str) -> String {
    if p.is_empty() || p == "default" {
        "Claude".into()
    } else {
        p.into()
    }
}

#[derive(Clone, Debug, Default)]
struct Slot {
    profile: String,
    until: i64,
    repeats: i64,
    reason: String,
    failing: String,
}

#[derive(Debug)]
struct Inner {
    slots: Vec<Slot>,
    last_ran: Option<usize>,
    load_note: String,
}

/// One profile of `coppice foreman status --json`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StatusRow {
    pub profile: String,
    pub name: String,
    /// "ready", "cooling" (until `until`) or "failing" (`reason`).
    pub state: String,
    pub until: String,
    pub reason: String,
}

impl StatusRow {
    fn json(&self, repeats: Option<i64>) -> String {
        let mut s = format!(
            "{{\"profile\":{},\"name\":{},\"state\":{}",
            json_str(&self.profile),
            json_str(&self.name),
            json_str(&self.state)
        );
        if !self.until.is_empty() {
            s.push_str(&format!(",\"until\":{}", json_str(&self.until)));
        }
        if !self.reason.is_empty() {
            s.push_str(&format!(",\"reason\":{}", json_str(&self.reason)));
        }
        if let Some(r) = repeats.filter(|r| *r != 0) {
            s.push_str(&format!(",\"repeats\":{r}"));
        }
        s.push('}');
        s
    }
}

/// The ordered turn profiles with the state of each. Safe for concurrent
/// use.
#[derive(Debug)]
pub struct Chain {
    inner: Mutex<Inner>,
}

/// The state-file spelling of a time, for the tests.
#[cfg(test)]
pub fn tests_date(t: i64) -> String {
    date(t)
}

fn date(t: i64) -> String {
    crate::foreman::chat::date_text(
        std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(t.max(0) as u64),
    )
}

fn parse_date(s: &str) -> Option<i64> {
    crate::foreman::chat::parse_date(s).map(crate::foreman::chat::unix_secs)
}

impl Chain {
    /// A chain of profiles, all ready, none run yet.
    pub fn new(profiles: &[String]) -> Chain {
        Chain {
            inner: Mutex::new(Inner {
                slots: profiles
                    .iter()
                    .map(|p| Slot {
                        profile: p.clone(),
                        ..Default::default()
                    })
                    .collect(),
                last_ran: None,
                load_note: String::new(),
            }),
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The one note about a state file that held a bad end time, or "". The
    /// server logs it once.
    pub fn load_note(&self) -> String {
        self.lock().load_note.clone()
    }

    /// The name of profile i, as the config has it.
    pub fn profile(&self, i: usize) -> String {
        self.lock().slots[i].profile.clone()
    }

    /// The first profile that is not cooling down at now.
    pub fn first(&self, now: i64) -> Option<usize> {
        self.next(None, now)
    }

    /// The first profile after i that is not cooling down at now.
    pub fn next(&self, i: Option<usize>, now: i64) -> Option<usize> {
        let g = self.lock();
        let from = i.map_or(0, |i| i + 1);
        (from..g.slots.len()).find(|j| g.slots[*j].until <= now)
    }

    /// The soonest end of a cool-down, for when every profile is out.
    pub fn earliest(&self, now: i64) -> Option<i64> {
        self.lock()
            .slots
            .iter()
            .filter(|s| s.until > now)
            .map(|s| s.until)
            .min()
    }

    /// Cools profile i down and returns when the cool-down ends: at the
    /// verdict's reset when it has one, else COOL_START doubled by each
    /// repeat up to COOL_MAX.
    pub fn mark_out(&self, i: usize, v: &Verdict, now: i64) -> i64 {
        let mut g = self.lock();
        let s = &mut g.slots[i];
        let mut d = COOL_START;
        let mut n = 0;
        while n < s.repeats && d < COOL_MAX {
            d *= 2;
            n += 1;
        }
        d = d.min(COOL_MAX);
        s.repeats += 1;
        s.until = v.reset.unwrap_or(now + d);
        s.reason = v.reason.clone();
        s.failing.clear();
        s.until
    }

    /// A success starts the doubling over.
    pub fn mark_ok(&self, i: usize) {
        let mut g = self.lock();
        let p = g.slots[i].profile.clone();
        g.slots[i] = Slot {
            profile: p,
            ..Default::default()
        };
    }

    /// Records that the last turn on profile i failed for a reason that is
    /// not an outage. The chain does not move for it.
    pub fn mark_failing(&self, i: usize, reason: &str) {
        self.lock().slots[i].failing = safe_reason(reason);
    }

    /// Records that the last turn ran on profile i.
    pub fn mark_ran(&self, i: usize) {
        self.lock().last_ran = Some(i);
    }

    /// The profile the last turn ran on.
    pub fn last_ran(&self) -> Option<usize> {
        self.lock().last_ran
    }

    /// The chain with each profile's state at now.
    pub fn status(&self, now: i64) -> Vec<StatusRow> {
        self.lock()
            .slots
            .iter()
            .map(|s| {
                let mut r = StatusRow {
                    profile: s.profile.clone(),
                    name: profile_name(&s.profile),
                    state: "ready".into(),
                    until: String::new(),
                    reason: String::new(),
                };
                if s.until > now {
                    r.state = "cooling".into();
                    r.until = date(s.until);
                    r.reason = safe_reason(&s.reason);
                } else if !s.failing.is_empty() {
                    r.state = "failing".into();
                    r.reason = safe_reason(&s.failing);
                }
                r
            })
            .collect()
    }

    fn doc(&self, rows: Vec<String>) -> String {
        let mut s = format!("{{\"chain\":[{}]", rows.join(","));
        if let Some(i) = self.last_ran() {
            s.push_str(&format!(
                ",\"last_ran\":{}",
                json_str(&profile_name(&self.profile(i)))
            ));
        }
        s.push_str("}\n");
        s
    }

    /// The status as one JSON line.
    pub fn status_json(&self, now: i64) -> String {
        self.doc(self.status(now).iter().map(|r| r.json(None)).collect())
    }

    /// Writes the chain's state to FAILOVER_FILE in dir, 0600, by a rename.
    pub fn save(&self, dir: &Path, now: i64) -> Result<(), String> {
        let rows = self.status(now);
        let rows: Vec<String> = {
            let g = self.lock();
            rows.into_iter()
                .zip(g.slots.iter())
                .map(|(mut r, sl)| {
                    // The end time stays after the cool-down ends, so a
                    // reload sees it end.
                    r.until.clear();
                    r.reason.clear();
                    if sl.until != 0 {
                        r.until = date(sl.until);
                        r.reason = sl.reason.clone();
                    }
                    if !sl.failing.is_empty() {
                        r.reason = sl.failing.clone();
                    }
                    r.json(Some(sl.repeats))
                })
                .collect()
        };
        let text = self.doc(rows);
        static SEQ: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
        let tmp = dir.join(format!(
            ".failover-{}-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, std::sync::atomic::Ordering::SeqCst),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.subsec_nanos())
        ));
        {
            use std::io::Write;
            use std::os::unix::fs::OpenOptionsExt;
            let mut f = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)
                .map_err(|e| e.to_string())?;
            f.write_all(text.as_bytes()).map_err(|e| e.to_string())?;
        }
        std::fs::rename(&tmp, dir.join(FAILOVER_FILE)).map_err(|e| {
            let _ = std::fs::remove_file(&tmp);
            e.to_string()
        })
    }

    /// Reads the state file in dir for profiles. A missing file, a file that
    /// does not read, or one for a different turn chain gives a fresh chain:
    /// the state is a convenience, never a reason to refuse to start. An end
    /// time that does not parse, or lies more than MAX_NAMED_RESET after now,
    /// is dropped (the profile counts as ready) and load_note says so; a past
    /// one is ready.
    pub fn load(dir: &Path, profiles: &[String], now: i64) -> Chain {
        let c = Chain::new(profiles);
        let Ok(data) = std::fs::read_to_string(dir.join(FAILOVER_FILE)) else {
            return c;
        };
        let Ok(v) = serde_json::from_str::<serde_json::Value>(&data) else {
            return c;
        };
        let Some(rows) = v.get("chain").and_then(|x| x.as_array()) else {
            return c;
        };
        if rows.len() != profiles.len() {
            return c;
        }
        let name_of = |r: &serde_json::Value| {
            r.get("profile")
                .and_then(|x| x.as_str())
                .map(str::to_string)
        };
        if rows
            .iter()
            .zip(profiles)
            .any(|(r, p)| name_of(r).as_deref() != Some(p.as_str()))
        {
            return c;
        }
        {
            let mut g = c.lock();
            let mut bad: Vec<String> = Vec::new();
            for (i, r) in rows.iter().enumerate() {
                let s = &mut g.slots[i];
                s.repeats = r.get("repeats").and_then(|x| x.as_i64()).unwrap_or(0);
                let reason = r.get("reason").and_then(|x| x.as_str()).unwrap_or("");
                s.reason = reason.into();
                if let Some(u) = r.get("until").and_then(|x| x.as_str()).filter(|u| !u.is_empty()) {
                    match parse_date(u) {
                        Some(t) if t <= now + MAX_NAMED_RESET => s.until = t,
                        _ => {
                            s.repeats = 0;
                            s.reason.clear();
                            bad.push(format!("account {}", profile_name(&profiles[i])));
                        }
                    }
                }
                if r.get("state").and_then(|x| x.as_str()) == Some("failing") {
                    s.failing = reason.into();
                    s.reason.clear();
                }
            }
            if !bad.is_empty() {
                g.load_note = format!(
                    "The foreman's saved end time for {} was not a time within 7 days, so it was dropped and the account counts as ready.",
                    bad.join(" and ")
                );
            }
            if let Some(name) = v.get("last_ran").and_then(|x| x.as_str()) {
                g.last_ran = profiles.iter().position(|p| profile_name(p) == name);
            }
        }
        c
    }

    /// Reads the state file in dir as the chain of the profiles it names;
    /// None when the file is missing or does not read.
    pub fn read(dir: &Path, now: i64) -> Option<Chain> {
        let data = std::fs::read_to_string(dir.join(FAILOVER_FILE)).ok()?;
        let v: serde_json::Value = serde_json::from_str(&data).ok()?;
        let profiles: Vec<String> = v
            .get("chain")?
            .as_array()?
            .iter()
            .map(|r| {
                r.get("profile")
                    .and_then(|x| x.as_str())
                    .unwrap_or("")
                    .to_string()
            })
            .collect();
        Some(Chain::load(dir, &profiles, now))
    }
}
