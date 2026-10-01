//! Push through the operator's own ntfy: a card when a pane has just
//! become blocked, with a deny button whose token can deny that one ask
//! and nothing else.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::json;

use super::upstream::{self, Dialer};
use crate::godec::Any;

/// One pane stays quiet this long after it buzzes the phone.
const DEBOUNCE: Duration = Duration::from_secs(5);
/// A retired deny token is still known this long after its ask's deadline.
const RETIRED_KEEP: f64 = 24.0 * 3600.0;
const RETIRED_MAX: usize = 1024;
/// How long a deny token lives when its ask names no deadline.
const DENY_NO_DEADLINE: f64 = 600.0;

/// What a push for a blocked pane with no ask says, by what found the
/// block. They match the floor's own words.
pub const BLOCKED_BY_GATE: &str = "Blocked. The gate gave no detail.";
pub const BLOCKED_OWN_QUESTION: &str = "Waiting on the agent's own question. Open it to answer.";

/// What the answer route says about an ask a harness holds.
pub fn at_the_floor(pane: &str) -> String {
    format!("The harness holds this ask itself. Open {pane} on the floor and answer it there.")
}

/// The ntfy topic.
#[derive(Clone)]
pub struct Config {
    pub base_url: String,
    pub topic: String,
    pub token_env: String,
    pub click_base: String,
}

struct Grant {
    ask: String,
    pane: String,
    deadline: f64,
}

struct State {
    last: HashMap<String, String>,
    sent: HashMap<String, Instant>,
    denies: HashMap<String, Grant>,
    retired: HashMap<String, f64>,
}

pub struct Publisher {
    cfg: Config,
    st: Mutex<State>,
}

/// The ask of a state event.
struct Ask {
    id: String,
    summary: String,
    deadline: f64,
    tier: String,
    gate: Option<(String, Option<i64>)>,
    holder: String,
}

/// The part of a state event push reads.
struct Event {
    pane: String,
    state: String,
    source: String,
    detail: String,
    ask: Option<Ask>,
    held: bool,
}

fn now_secs() -> f64 {
    crate::server::Server::now_seconds()
}

/// Printable ASCII only, one line, runs of spaces as one, cut to max.
pub fn ascii_header(s: &str, max: usize) -> String {
    let mut b = String::new();
    let mut last_space = false;
    for r in s.chars() {
        let r = if (r as u32) < 0x20 || (r as u32) > 0x7e {
            ' '
        } else {
            r
        };
        if r == ' ' {
            if last_space {
                continue;
            }
            last_space = true;
        } else {
            last_space = false;
        }
        b.push(r);
    }
    let out = b.trim_matches(|c: char| c.is_whitespace()).to_string();
    if out.len() <= max {
        return out;
    }
    if max <= 3 {
        return out[..max].to_string();
    }
    format!(
        "{}...",
        out[..max - 3].trim_matches(|c: char| c.is_whitespace())
    )
}

impl Publisher {
    /// A publisher for cfg, or the refusal when the URL or the topic is
    /// missing.
    pub fn new(cfg: Config) -> Result<Publisher, String> {
        if cfg.base_url.is_empty() || cfg.topic.is_empty() {
            return Err("push needs --ntfy URL and --ntfy-topic NAME".into());
        }
        let cfg = Config {
            base_url: cfg.base_url.trim_end_matches('/').to_string(),
            click_base: cfg.click_base.trim_end_matches('/').to_string(),
            ..cfg
        };
        Ok(Publisher {
            cfg,
            st: Mutex::new(State {
                last: HashMap::new(),
                sent: HashMap::new(),
                denies: HashMap::new(),
                retired: HashMap::new(),
            }),
        })
    }

    fn on_state(&self, ev: &Event, label: &str) {
        let mut st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        retire_stale(&mut st, ev);
        let was = st.last.get(&ev.pane).cloned().unwrap_or_default();
        let now = Instant::now();
        let mut state = ev.state.clone();
        if state == "blocked" && ev.held {
            state = "held".into();
        }
        st.last.insert(ev.pane.clone(), state.clone());
        if state != "blocked" || was == "blocked" {
            return;
        }
        if let Some(last) = st.sent.get(&ev.pane) {
            if now.duration_since(*last) < DEBOUNCE {
                return;
            }
        }
        st.sent.insert(ev.pane.clone(), now);
        let tok = self.mint_deny(&mut st, ev);
        drop(st);

        let label = if label.is_empty() { &ev.pane } else { label };
        let held = ev.ask.as_ref().is_some_and(|a| a.holder == "harness");
        let body = if held {
            format!(
                "Wants {}. Open the floor to answer it.",
                ev.ask.as_ref().map(|a| a.summary.as_str()).unwrap_or("")
            )
        } else if let Some(a) = ev.ask.as_ref().filter(|a| !a.summary.is_empty()) {
            format!("Wants {}. {}", a.summary, gate_says(&a.gate))
        } else if !ev.detail.is_empty() && ev.source != "manifest" {
            ev.detail.clone()
        } else if ev.source == "gate" {
            BLOCKED_BY_GATE.into()
        } else {
            BLOCKED_OWN_QUESTION.into()
        };
        let click = self.click_url(ev);
        let actions = self.actions(ev, &click, &tok);
        if let Err(e) = self.publish(&format!("{label} needs you."), &body, &click, &actions) {
            super::log(
                "WARN",
                "web: ntfy publish failed",
                &[
                    ("pane", &ev.pane),
                    ("base", &self.cfg.base_url),
                    ("err", &e),
                ],
            );
        }
    }

    fn click_url(&self, ev: &Event) -> String {
        if self.cfg.click_base.is_empty() {
            return String::new();
        }
        let mut click = format!(
            "{}/#/pane/{}",
            self.cfg.click_base,
            crate::adapters::http::path_escape(&ev.pane)
        );
        if let Some(a) = &ev.ask {
            if !a.id.is_empty() && crate::server::ask::safe_id(&a.id) == a.id {
                click.push_str("?ask=");
                click.push_str(&a.id);
            }
        }
        click
    }

    fn actions(&self, ev: &Event, click: &str, tok: &str) -> String {
        let Some(a) = &ev.ask else {
            return String::new();
        };
        if a.holder == "harness" {
            if click.is_empty() {
                return String::new();
            }
            return format!("view, Look, {click}");
        }
        if click.is_empty() || tok.is_empty() {
            return String::new();
        }
        let body = format!(
            "{{\"tool_use_id\":\"{}\",\"decision\":\"deny\",\"reason\":\"denied from the lock screen\"}}",
            a.id
        );
        let mut parts = vec![
            format!(
                "http, Deny, {}/api/ask/answer, method=POST, headers.Authorization=Bearer {tok}, headers.Content-Type=application/json, body='{body}', clear=true",
                self.cfg.click_base
            ),
            format!("view, Look, {click}"),
        ];
        if a.tier == "undoable" {
            parts.push(format!("view, Allow, {click}"));
        }
        parts.join("; ")
    }

    fn mint_deny(&self, st: &mut State, ev: &Event) -> String {
        let Some(a) = &ev.ask else {
            return String::new();
        };
        if a.holder == "harness"
            || self.cfg.click_base.is_empty()
            || a.id.is_empty()
            || crate::server::ask::safe_id(&a.id) != a.id
        {
            return String::new();
        }
        let Ok(b) = super::token::random_bytes(32) else {
            return String::new();
        };
        let tok: String = b.iter().map(|x| format!("{x:02x}")).collect();
        let deadline = if a.deadline <= 0.0 {
            now_secs() + DENY_NO_DEADLINE
        } else {
            a.deadline
        };
        st.denies.insert(
            tok.clone(),
            Grant {
                ask: a.id.clone(),
                pane: ev.pane.clone(),
                deadline,
            },
        );
        tok
    }

    /// Whether token was a deny token whose ask is gone, within a day.
    pub fn deny_retired(&self, token: &str) -> bool {
        if token.is_empty() {
            return false;
        }
        let mut st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        let now = now_secs();
        st.retired.retain(|_, until| *until > now);
        st.retired.keys().fold(false, |found, t| {
            found | super::token::ct_eq(t.as_bytes(), token.as_bytes())
        })
    }

    /// The ask a deny token may deny.
    pub fn deny_grant(&self, token: &str) -> Option<String> {
        if token.is_empty() {
            return None;
        }
        let mut st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        let now = now_secs();
        let expired: Vec<String> = st
            .denies
            .iter()
            .filter(|(_, g)| now >= g.deadline)
            .map(|(t, _)| t.clone())
            .collect();
        for t in expired {
            retire(&mut st, &t);
        }
        let mut found = None;
        for (t, g) in &st.denies {
            if super::token::ct_eq(t.as_bytes(), token.as_bytes()) {
                found = Some(g.ask.clone());
            }
        }
        found
    }

    /// Drops every deny token for ask, once it is answered.
    pub fn retire_deny(&self, ask: &str) {
        let mut st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        let toks: Vec<String> = st
            .denies
            .iter()
            .filter(|(_, g)| g.ask == ask)
            .map(|(t, _)| t.clone())
            .collect();
        for t in toks {
            retire(&mut st, &t);
        }
    }

    /// One message, to prove the topic works.
    pub fn test(&self) -> Result<(), String> {
        let click = self.cfg.click_base.clone();
        self.publish(
            "coppice test",
            "Push works. This came from your own server.",
            &click,
            "",
        )
    }

    fn publish(&self, title: &str, body: &str, click: &str, actions: &str) -> Result<(), String> {
        let url = format!("{}/{}", self.cfg.base_url, self.cfg.topic);
        let title = ascii_header(title, 100);
        let click = ascii_header(click, 2000);
        let actions = ascii_header(actions, 4000);
        let mut headers: Vec<(&str, String)> = vec![("Title", title)];
        if !click.is_empty() {
            headers.push(("Click", click));
        }
        if !actions.is_empty() {
            headers.push(("Actions", actions));
        }
        if !self.cfg.token_env.is_empty() {
            if let Ok(t) = std::env::var(&self.cfg.token_env) {
                if !t.is_empty() {
                    headers.push(("Authorization", format!("Bearer {t}")));
                }
            }
        }
        let h: Vec<(&str, &str)> = headers.iter().map(|(k, v)| (*k, v.as_str())).collect();
        let reply = super::client::send(
            "POST",
            &url,
            &h,
            ascii_header(body, 500).as_bytes(),
            Duration::from_secs(10),
        )
        .map_err(|e| format!("ntfy request failed: {e}"))?;
        if reply.status >= 300 {
            return Err(format!("ntfy answered {}", reply.status));
        }
        Ok(())
    }

    /// Subscribes to state events and feeds on_state until stop is set.
    pub fn watch(self: Arc<Self>, d: Dialer, stop: Arc<AtomicBool>) -> Result<(), String> {
        let mut labels: HashMap<String, String> = HashMap::new();
        let refresh = |labels: &mut HashMap<String, String>| {
            let Ok(msg) = upstream::call(&d, vec![("cmd", json!("pane.list"))]) else {
                return;
            };
            let panes = msg
                .get("result")
                .and_then(|r| r.get("panes"))
                .cloned()
                .unwrap_or(Any::Nil);
            if let Any::Arr(items) = panes {
                for it in items {
                    let id = it.str_of("id");
                    if !id.is_empty() {
                        labels.insert(id, it.str_of("label"));
                    }
                }
            }
        };
        refresh(&mut labels);
        let s = d.open()?;
        let sub = crate::gojson::marshal(&crate::gojson::map(vec![
            ("id", json!("push-sub")),
            ("cmd", json!("events.subscribe")),
            ("panes", json!("*")),
            ("kinds", json!(["state"])),
        ]));
        s.send(sub.as_bytes())?;
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut subscribed = false;
        loop {
            if stop.load(Ordering::SeqCst) {
                return Err("context canceled".into());
            }
            if !subscribed && Instant::now() >= deadline {
                return Err("coppice-server did not answer events.subscribe in 5 s".into());
            }
            let line = match s.lines.recv_timeout(Duration::from_millis(100)) {
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => continue,
                Err(_) => return Err("coppice-server closed the event stream".into()),
                Ok(l) => l,
            };
            let Ok(m) = crate::godec::decode_map(&line) else {
                continue;
            };
            if !subscribed && m.str_of("id") == "push-sub" {
                if !matches!(m.get("ok"), Some(Any::Bool(true))) {
                    return Err(upstream::refusal(&m).to_string());
                }
                subscribed = true;
                continue;
            }
            if m.str_of("event") != "state" {
                continue;
            }
            let Some(ev) = read_event(&m) else {
                continue;
            };
            if !labels.contains_key(&ev.pane) {
                refresh(&mut labels);
                labels.entry(ev.pane.clone()).or_default();
            }
            let label = labels.get(&ev.pane).cloned().unwrap_or_default();
            self.on_state(&ev, &label);
        }
    }
}

fn num(v: Option<&Any>) -> f64 {
    match v {
        Some(Any::F64(f)) => *f,
        _ => 0.0,
    }
}

fn read_event(m: &Any) -> Option<Event> {
    let pane = m.str_of("pane");
    if pane.is_empty() {
        return None;
    }
    let ask = match m.get("ask") {
        Some(a @ Any::Map(_)) => Some(Ask {
            id: a.str_of("id"),
            summary: a.str_of("summary"),
            deadline: num(a.get("deadline")),
            tier: a.str_of("tier"),
            gate: match a.get("gate") {
                Some(g @ Any::Map(_)) => Some((
                    g.str_of("verdict"),
                    match g.get("rule") {
                        Some(Any::F64(f)) => Some(*f as i64),
                        _ => None,
                    },
                )),
                _ => None,
            },
            holder: a.str_of("holder"),
        }),
        _ => None,
    };
    Some(Event {
        pane,
        state: m.str_of("state"),
        source: m.str_of("source"),
        detail: m.str_of("detail"),
        ask,
        held: matches!(m.get("held"), Some(Any::Map(_))),
    })
}

fn gate_says(g: &Option<(String, Option<i64>)>) -> String {
    match g {
        Some((v, _)) if v == "allow" => "Gate says yes.".into(),
        Some((_, Some(rule))) => format!("Gate says no, rule {rule}."),
        _ => "Gate says no.".into(),
    }
}

fn retire_stale(st: &mut State, ev: &Event) {
    let toks: Vec<String> = st
        .denies
        .iter()
        .filter(|(_, g)| g.pane == ev.pane)
        .filter(|(_, g)| {
            ev.state != "blocked" || ev.ask.as_ref().map(|a| a.id.as_str()) != Some(g.ask.as_str())
        })
        .map(|(t, _)| t.clone())
        .collect();
    for t in toks {
        retire(st, &t);
    }
}

fn retire(st: &mut State, tok: &str) {
    let Some(g) = st.denies.remove(tok) else {
        return;
    };
    let now = now_secs();
    let until = g.deadline.max(now);
    st.retired.insert(tok.to_string(), until + RETIRED_KEEP);
    st.retired.retain(|_, v| *v > now);
    while st.retired.len() > RETIRED_MAX {
        let oldest = st
            .retired
            .iter()
            .min_by(|a, b| a.1.total_cmp(b.1))
            .map(|(k, _)| k.clone());
        match oldest {
            Some(k) => {
                st.retired.remove(&k);
            }
            None => break,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn headers_are_scrubbed_as_go_scrubs_them() {
        assert_eq!(
            ascii_header("git push\norigin  main", 100),
            "git push origin main"
        );
        assert_eq!(ascii_header("abcdefgh", 6), "abc...");
        assert_eq!(ascii_header("héllo", 100), "h llo");
    }
}
