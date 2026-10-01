//! A pane's state: the event on the wire, its checks, and the merge of the
//! sources. Every path into a pane's state goes through merge; every read
//! of a cached state with nothing fresh to merge against goes through
//! effective_state.

use std::collections::{BTreeMap, HashMap};
use std::sync::RwLock;

use serde_json::value::RawValue;
use serde_json::{json, Value};

use crate::gojson;

pub const STATE_IDLE: &str = "idle";
pub const STATE_WORKING: &str = "working";
pub const STATE_BLOCKED: &str = "blocked";
pub const STATE_DONE: &str = "done";
pub const STATE_UNKNOWN: &str = "unknown";

pub const SRC_OPERATOR: &str = "operator";
pub const SRC_GATE: &str = "gate";
pub const SRC_HEADLESS: &str = "headless";
pub const SRC_PROCESS: &str = "process";
pub const SRC_MANIFEST: &str = "manifest";

/// The cap on the free-text detail field, in runes.
pub const DETAIL_MAX: usize = 200;

pub const TIER_UNDOABLE: &str = "undoable";
pub const TIER_PERMANENT: &str = "permanent";

/// How long an incoming manifest event loses to a fresher, higher source.
pub const HOLD_WINDOW: f64 = 2.0;

fn valid_state(s: &str) -> bool {
    matches!(
        s,
        STATE_IDLE | STATE_WORKING | STATE_BLOCKED | STATE_DONE | STATE_UNKNOWN
    )
}

/// 0 for anything not in the enum, so an unknown source never wins.
pub fn source_rank(src: &str) -> i32 {
    match src {
        SRC_OPERATOR => 5,
        SRC_GATE => 4,
        SRC_HEADLESS => 3,
        SRC_PROCESS => 2,
        SRC_MANIFEST => 1,
        _ => 0,
    }
}

/// undoable for exactly "undoable", permanent for anything else.
pub fn normal_tier(t: &str) -> String {
    if t == TIER_UNDOABLE {
        TIER_UNDOABLE.into()
    } else {
        TIER_PERMANENT.into()
    }
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct Ask {
    pub id: String,
    pub tool: String,
    pub summary: String,
    pub deadline: f64,
    pub tier: String,
    pub holder: String,
}

impl Ask {
    pub fn to_value(&self) -> Value {
        let mut p = vec![
            ("id", json!(self.id)),
            ("tool", json!(self.tool)),
            ("summary", json!(self.summary)),
            ("deadline", gojson::float(self.deadline)),
            ("tier", json!(self.tier)),
        ];
        if !self.holder.is_empty() {
            p.push(("holder", json!(self.holder)));
        }
        gojson::obj(p)
    }
}

/// One state event. harness_session_id and pane are Option because a
/// sender may omit them or send null.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Event {
    pub v: i64,
    pub ts: f64,
    pub session_id: String,
    pub harness_session_id: Option<String>,
    pub harness: String,
    pub pane: Option<String>,
    pub state: String,
    pub source: String,
    pub ask: Option<Ask>,
    pub detail: String,
    pub who: Option<String>,
}

impl Event {
    /// The struct fields in Go's declared order, for embedding in a larger
    /// object.
    pub fn fields(&self) -> Vec<(&'static str, Value)> {
        let mut p: Vec<(&'static str, Value)> = vec![
            ("v", json!(self.v)),
            ("ts", gojson::float(self.ts)),
            ("session_id", json!(self.session_id)),
            ("harness_session_id", json!(self.harness_session_id)),
            ("harness", json!(self.harness)),
            ("pane", json!(self.pane)),
            ("state", json!(self.state)),
            ("source", json!(self.source)),
        ];
        if let Some(a) = &self.ask {
            p.push(("ask", a.to_value()));
        }
        p.push(("detail", json!(self.detail)));
        if let Some(w) = &self.who {
            p.push(("who", json!(w)));
        }
        p
    }

    /// The cross-field rules. It normalizes the ask's tier in place, as Go
    /// does through the ask pointer.
    pub fn validate(&mut self) -> Result<(), String> {
        if !valid_state(&self.state) {
            return Err(format!(
                "state {} is not one of idle working blocked done unknown",
                crate::sys::go_quote(&self.state)
            ));
        }
        if source_rank(&self.source) == 0 {
            return Err(format!(
                "source {} is not one of operator gate headless process manifest",
                crate::sys::go_quote(&self.source)
            ));
        }
        if self.state == STATE_DONE && self.source != SRC_PROCESS && self.source != SRC_HEADLESS {
            return Err(format!(
                "state done may only come from source process or headless, got {}",
                crate::sys::go_quote(&self.source)
            ));
        }
        let n = self.detail.chars().count();
        if n > DETAIL_MAX {
            return Err(format!("detail is {n} chars, the cap is {DETAIL_MAX}"));
        }
        if self.ask.is_some() && self.state != STATE_BLOCKED {
            return Err(format!(
                "ask is only valid on a blocked event, state is {}",
                crate::sys::go_quote(&self.state)
            ));
        }
        if let Some(a) = self.ask.as_mut() {
            a.tier = normal_tier(&a.tier);
        }
        if self.source == SRC_GATE && self.state == STATE_BLOCKED && self.ask.is_none() {
            return Err("a gate blocked event must carry the ask it is holding".into());
        }
        if self.who.is_some() && self.source != SRC_OPERATOR {
            return Err(format!(
                "who is only valid on an operator event, source is {}",
                crate::sys::go_quote(&self.source)
            ));
        }
        if self.v != 1 {
            return Err(format!("unsupported event schema version {}", self.v));
        }
        Ok(())
    }
}

type Row = BTreeMap<String, Box<RawValue>>;

fn is_null(r: &RawValue) -> bool {
    r.get() == "null"
}

fn string_from_raw(r: &RawValue, name: &str) -> Result<String, String> {
    if is_null(r) {
        return Err(format!("{name} must be a string"));
    }
    serde_json::from_str::<String>(r.get()).map_err(|_| format!("{name} must be a string"))
}

/// A JSON number as Go reads it into a float64: any number literal whose
/// value fits.
fn number_from_raw(r: &RawValue, name: &str) -> Result<f64, String> {
    let t = r.get();
    if is_null(r) || !t.starts_with(|c: char| c == '-' || c.is_ascii_digit()) {
        return Err(format!("{name} must be a number"));
    }
    match t.parse::<f64>() {
        Ok(f) if f.is_finite() => Ok(f),
        _ => Err(format!("{name} must be a number")),
    }
}

fn json_kind(t: &str) -> &'static str {
    match t.as_bytes().first() {
        Some(b'"') => "string",
        Some(b't') | Some(b'f') => "bool",
        Some(b'{') => "object",
        Some(b'[') => "array",
        _ => "number",
    }
}

fn int_from_raw(r: &RawValue) -> Result<i64, String> {
    let t = r.get();
    if is_null(r) {
        return Err("invalid schema version: null".into());
    }
    if json_kind(t) != "number" {
        return Err(format!(
            "invalid schema version: json: cannot unmarshal {} into Go value of type int",
            json_kind(t)
        ));
    }
    if t.contains(['.', 'e', 'E']) {
        return Err(format!(
            "invalid schema version: json: cannot unmarshal number {t} into Go value of type int"
        ));
    }
    t.parse::<i64>().map_err(|_| {
        format!(
            "invalid schema version: json: cannot unmarshal number {t} into Go value of type int"
        )
    })
}

fn optional_string(row: &Row, key: &str) -> Result<Option<String>, String> {
    match row.get(key) {
        None => Ok(None),
        Some(r) if is_null(r) => Ok(None),
        Some(r) => serde_json::from_str::<String>(r.get())
            .map(Some)
            .map_err(|_| format!("{key} must be a string")),
    }
}

fn parse_row(raw: &str) -> Result<Row, String> {
    serde_json::from_str::<Row>(raw)
        .map_err(|e| format!("state event is not a JSON object: {}", go_json_err(raw, &e)))
}

/// A rough copy of the Go json error a non-object line gives.
fn go_json_err(raw: &str, e: &serde_json::Error) -> String {
    let t = raw.trim();
    if serde_json::from_str::<Value>(t).is_ok() {
        return format!(
            "json: cannot unmarshal {} into Go value of type map[string]json.RawMessage",
            json_kind(t)
        );
    }
    e.to_string()
}

fn parse_ask(raw: &RawValue) -> Result<Ask, String> {
    let obj: Row =
        serde_json::from_str(raw.get()).map_err(|_| "ask must be a JSON object".to_string())?;
    let missing: Vec<&str> = ["id", "tool", "summary", "deadline"]
        .into_iter()
        .filter(|k| !obj.contains_key(*k))
        .collect();
    if !missing.is_empty() {
        return Err(format!("ask is missing field(s): {}", missing.join(", ")));
    }
    let id = string_from_raw(&obj["id"], "ask id")?;
    let tool = string_from_raw(&obj["tool"], "ask tool")?;
    let summary = string_from_raw(&obj["summary"], "ask summary")?;
    let deadline = number_from_raw(&obj["deadline"], "ask deadline")?;
    let mut tier = String::new();
    if let Some(r) = obj.get("tier") {
        if !is_null(r) {
            tier = string_from_raw(r, "ask tier")?;
        }
    }
    Ok(Ask {
        id,
        tool,
        summary,
        deadline,
        tier: normal_tier(&tier),
        holder: String::new(),
    })
}

/// Reads one event, checks every field's JSON type, then the cross-field
/// rules. A bad event is an error, never a default.
pub fn parse_event(raw: &str) -> Result<Event, String> {
    let row = parse_row(raw)?;
    let missing: Vec<&str> = ["session_id", "harness", "state", "source", "ts"]
        .into_iter()
        .filter(|k| !row.contains_key(*k))
        .collect();
    if !missing.is_empty() {
        return Err(format!(
            "event is missing required field(s): {}",
            missing.join(", ")
        ));
    }
    let session_id = string_from_raw(&row["session_id"], "session_id")?;
    let harness = string_from_raw(&row["harness"], "harness")?;
    let state = string_from_raw(&row["state"], "state")?;
    let source = string_from_raw(&row["source"], "source")?;
    let ts = number_from_raw(&row["ts"], "ts")?;
    let detail = match row.get("detail") {
        Some(r) => string_from_raw(r, "detail")?,
        None => String::new(),
    };
    let v = match row.get("v") {
        Some(r) => int_from_raw(r)?,
        None => 1,
    };
    let harness_session_id = optional_string(&row, "harness_session_id")?;
    let pane = optional_string(&row, "pane")?;
    let who = optional_string(&row, "who")?;
    let ask = match row.get("ask") {
        Some(r) if !is_null(r) => Some(parse_ask(r)?),
        _ => None,
    };
    let mut ev = Event {
        v,
        ts,
        session_id,
        harness_session_id,
        harness,
        pane,
        state,
        source,
        ask,
        detail,
        who,
    };
    ev.validate()?;
    Ok(ev)
}

/// The gate's decision on one tool call, as a gate hook reports it.
#[derive(Clone, Debug, Default)]
pub struct Verdict {
    pub decision: String,
    pub tool: String,
    pub clause: String,
}

/// The optional fields a gate hook sends beside the event.
#[derive(Clone, Debug, Default)]
pub struct ReportExtras {
    pub transcript_path: String,
    pub verdict: Option<Verdict>,
    pub mode: String,
}

fn cut_runes(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

pub fn parse_report_extras(raw: &str) -> Result<ReportExtras, String> {
    let row = parse_row(raw)?;
    let mut x = ReportExtras::default();
    if let Some(tp) = optional_string(&row, "transcript_path")? {
        if !tp.starts_with('/') {
            return Err("transcript_path must be an absolute path".into());
        }
        x.transcript_path = crate::sys::clean(&tp);
    }
    if let Some(mode) = optional_string(&row, "mode")? {
        if mode != "enforcing" && mode != "watching" {
            return Err(format!(
                "mode {} is not enforcing or watching",
                crate::sys::go_quote(&mode)
            ));
        }
        x.mode = mode;
    }
    if let Some(rv) = row.get("verdict") {
        if !is_null(rv) {
            let obj: Row = serde_json::from_str(rv.get())
                .map_err(|_| "verdict must be a JSON object".to_string())?;
            let rd = obj
                .get("decision")
                .ok_or_else(|| "verdict is missing decision".to_string())?;
            let decision = string_from_raw(rd, "verdict decision")?;
            if !matches!(decision.as_str(), "allow" | "deny" | "ask") {
                return Err(format!(
                    "verdict decision {} is not allow, deny or ask",
                    crate::sys::go_quote(&decision)
                ));
            }
            let tool = optional_string(&obj, "tool")
                .map_err(|_| "verdict tool must be a string".to_string())?;
            let clause = optional_string(&obj, "clause")
                .map_err(|_| "verdict clause must be a string".to_string())?;
            x.verdict = Some(Verdict {
                decision,
                tool: tool.map(|t| cut_runes(&t, DETAIL_MAX)).unwrap_or_default(),
                clause: clause
                    .map(|c| cut_runes(&c, DETAIL_MAX))
                    .unwrap_or_default(),
            });
        }
    }
    Ok(x)
}

fn ends_the_session(e: &Event) -> bool {
    e.state == STATE_DONE && (e.source == SRC_PROCESS || e.source == SRC_HEADLESS)
}

/// Folds one incoming event into a pane's current state. cur_received and
/// now are the server's own clock, never an event's ts.
pub fn merge(cur: Option<&Event>, cur_received: f64, mut inc: Event, now: f64) -> Event {
    if inc.state == STATE_DONE && !ends_the_session(&inc) {
        inc.state = STATE_UNKNOWN.into();
        inc.ask = None;
    }
    let Some(cur) = cur else {
        return inc;
    };
    if cur.state == STATE_DONE {
        return cur.clone();
    }
    if ends_the_session(&inc) {
        return inc;
    }
    if cur.state == STATE_BLOCKED && cur.source == SRC_GATE {
        if inc.source == SRC_GATE {
            return inc;
        }
        let expired = cur.ask.as_ref().is_some_and(|a| now >= a.deadline);
        if !expired {
            return cur.clone();
        }
    }
    if cur.state == STATE_BLOCKED
        && cur.source == SRC_MANIFEST
        && inc.source == SRC_PROCESS
        && inc.state == STATE_IDLE
    {
        return cur.clone();
    }
    if inc.source == SRC_MANIFEST
        && source_rank(&inc.source) < source_rank(&cur.source)
        && now - cur_received < HOLD_WINDOW
    {
        return cur.clone();
    }
    inc
}

/// The read-time half of merge's expiry: a blocked event whose ask's
/// deadline passed reads as working.
pub fn effective_state(cur: &Event, now: f64) -> Event {
    if cur.state != STATE_BLOCKED {
        return cur.clone();
    }
    match &cur.ask {
        Some(a) if now >= a.deadline => {
            let mut out = cur.clone();
            out.state = STATE_WORKING.into();
            out.ask = None;
            out.detail = "ask deadline passed".into();
            out
        }
        _ => cur.clone(),
    }
}

/// The per-pane current state.
#[derive(Default)]
pub struct Store {
    inner: RwLock<(HashMap<String, Event>, HashMap<String, f64>)>,
}

impl Store {
    /// Merges one event and reports whether the visible state, source or
    /// ask changed. The receive clock moves only on a change.
    pub fn apply(&self, pane: &str, inc: Event, now: f64) -> (Event, bool) {
        let mut g = self.inner.write().unwrap_or_else(|e| e.into_inner());
        let prev = g.0.get(pane).cloned();
        let prev_received = g.1.get(pane).copied().unwrap_or(0.0);
        let merged = merge(prev.as_ref(), prev_received, inc, now);
        let changed = match &prev {
            None => true,
            Some(p) => {
                p.state != merged.state
                    || p.source != merged.source
                    || match (&p.ask, &merged.ask) {
                        (None, None) => false,
                        (Some(a), Some(b)) => a.id != b.id,
                        _ => true,
                    }
            }
        };
        g.0.insert(pane.to_string(), merged.clone());
        if changed {
            g.1.insert(pane.to_string(), now);
        }
        (merged, changed)
    }

    pub fn current(&self, pane: &str) -> Option<Event> {
        self.inner
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .0
            .get(pane)
            .cloned()
    }

    /// When the server last took a changed event for pane, on its own
    /// clock.
    pub fn received(&self, pane: &str) -> Option<f64> {
        self.inner
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .1
            .get(pane)
            .copied()
    }

    pub fn forget(&self, pane: &str) {
        let mut g = self.inner.write().unwrap_or_else(|e| e.into_inner());
        g.0.remove(pane);
        g.1.remove(pane);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ev(state: &str, source: &str) -> Event {
        Event {
            v: 1,
            ts: 1.0,
            session_id: "s".into(),
            harness: "h".into(),
            state: state.into(),
            source: source.into(),
            ..Default::default()
        }
    }

    #[test]
    fn done_is_terminal() {
        let cur = ev(STATE_DONE, SRC_PROCESS);
        let out = merge(Some(&cur), 0.0, ev(STATE_WORKING, SRC_OPERATOR), 10.0);
        assert_eq!(out.state, STATE_DONE);
    }

    #[test]
    fn a_gate_hold_waits_for_the_gate_or_the_deadline() {
        let mut cur = ev(STATE_BLOCKED, SRC_GATE);
        cur.ask = Some(Ask {
            id: "a".into(),
            deadline: 100.0,
            ..Default::default()
        });
        let held = merge(Some(&cur), 0.0, ev(STATE_IDLE, SRC_OPERATOR), 50.0);
        assert_eq!(held.state, STATE_BLOCKED);
        let gone = merge(Some(&cur), 0.0, ev(STATE_IDLE, SRC_OPERATOR), 100.0);
        assert_eq!(gone.state, STATE_IDLE);
        assert_eq!(effective_state(&cur, 100.0).state, STATE_WORKING);
    }

    #[test]
    fn a_manifest_loses_inside_the_window_only() {
        let cur = ev(STATE_WORKING, SRC_PROCESS);
        assert_eq!(
            merge(Some(&cur), 10.0, ev(STATE_IDLE, SRC_MANIFEST), 11.0).source,
            SRC_PROCESS
        );
        assert_eq!(
            merge(Some(&cur), 10.0, ev(STATE_IDLE, SRC_MANIFEST), 12.0).source,
            SRC_MANIFEST
        );
    }

    #[test]
    fn parse_refuses_bad_fields() {
        assert!(parse_event(
            r#"{"v":1,"ts":1,"session_id":"s","harness":"h","state":"idle","source":"operator"}"#
        )
        .is_ok());
        assert_eq!(
            parse_event(r#"{"ts":1}"#).unwrap_err(),
            "event is missing required field(s): session_id, harness, state, source"
        );
        assert_eq!(
            parse_event(
                r#"{"v":1,"ts":1,"session_id":"s","harness":"h","state":"blocked","source":"gate"}"#
            )
            .unwrap_err(),
            "a gate blocked event must carry the ask it is holding"
        );
        assert_eq!(
            parse_event(
                r#"{"v":1,"ts":"1","session_id":"s","harness":"h","state":"idle","source":"gate"}"#
            )
            .unwrap_err(),
            "ts must be a number"
        );
    }
}
