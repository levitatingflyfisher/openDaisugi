//! pane.report_state and the agent verbs. apply_state is the one entry
//! point every state goes through: the process tick, a process exit, and a
//! gate hook's report.

use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::layout;
use crate::proto::{self, Request, Response};
use crate::state::{self, Event};
use crate::sys;

/// One agent.wait or agent.prompt --wait waiting for a state.
pub struct Waiter {
    pane: String,
    until: String,
    tx: Mutex<Option<SyncSender<Event>>>,
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "pane.report_state",
        Arc::new(|s, c, r| s.handle_report_state(c, r)),
    );
    let _ = s.handle("agent.list", Arc::new(|s, c, r| s.handle_agent_list(c, r)));
    let _ = s.handle("agent.get", Arc::new(|s, c, r| s.handle_agent_get(c, r)));
    let _ = s.handle(
        "agent.prompt",
        Arc::new(|s, c, r| s.handle_agent_prompt(c, r)),
    );
    let _ = s.handle("agent.wait", Arc::new(|s, c, r| s.handle_agent_wait(c, r)));
    let _ = s.handle("agent.read", Arc::new(|s, c, r| s.handle_agent_read(c, r)));
}

pub(super) fn cut(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

/// A session id pane.resume may put on a command line.
fn session_id_ok(sid: &str) -> bool {
    !sid.is_empty()
        && sid.len() <= 128
        && !sid.starts_with('-')
        && sid
            .chars()
            .all(|r| r.is_ascii_alphanumeric() || matches!(r, '.' | '_' | ':' | '-'))
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

fn or_unknown(s: &str) -> &str {
    if s.is_empty() {
        state::STATE_UNKNOWN
    } else {
        s
    }
}

/// The state event on the wire. held is set only by the server, when a
/// foreman hears the ask first.
pub fn state_event(ev: &Event, held: Option<&Value>) -> Value {
    let mut p: Vec<(&str, Value)> = vec![("event", json!("state"))];
    p.extend(ev.fields());
    if let Some(h) = held {
        p.push(("held", h.clone()));
    }
    gojson::obj(p)
}

fn wait_reply(pane: &str, ev: &Event) -> Value {
    let mut p = vec![
        ("pane", json!(pane)),
        ("state", json!(ev.state)),
        ("source", json!(ev.source)),
    ];
    if let Some(a) = &ev.ask {
        p.push(("ask", a.to_value()));
    }
    gojson::map(p)
}

impl Server {
    /// Validates, merges, stores and broadcasts one event, and returns the
    /// event that was stored. An event that fails validation becomes
    /// unknown from the same source, with the reason in detail.
    pub fn apply_state(&self, pane: &str, mut ev: Event) -> Event {
        if let Err(e) = ev.validate() {
            let harness = if ev.harness.is_empty() {
                "unknown".to_string()
            } else {
                ev.harness.clone()
            };
            ev = Event {
                v: 1,
                ts: Server::now_seconds(),
                session_id: pane.into(),
                harness,
                pane: Some(pane.into()),
                state: state::STATE_UNKNOWN.into(),
                source: ev.source.clone(),
                detail: cut(&format!("rejected state event: {e}"), state::DETAIL_MAX),
                ..Default::default()
            };
            eprintln!("coppice: pane {pane} sent an invalid state event: {e}");
        }
        let now = Server::now_seconds();
        if ev.state == state::STATE_BLOCKED {
            self.note_ask(pane, ev.ask.as_ref());
        }
        let prev = self.states.current(pane);
        let (merged, changed) = self.states.apply(pane, ev, now);
        if prev.is_some_and(|p| p.state == state::STATE_WORKING)
            && super::escalate::turn_ended(&merged)
        {
            self.went_idle(pane);
        }
        // The hold is decided before the event goes out, so the first
        // event a floor sees for a held ask already says who holds it.
        let esc = self.escalate(pane, &merged);
        self.notify_waiters(pane, &merged, now);
        if changed {
            let held = esc.held.as_ref().map(|(v, _)| v);
            self.broadcast("state", pane, &state_event(&merged, held));
        }
        if !esc.note.is_empty() {
            self.note_to(&esc.note, pane, &esc.foreman);
        }
        for (p, a) in &esc.release {
            self.surface(p, a);
        }
        if let Some((_, by)) = &esc.held {
            self.tell_holds(by);
        }
        if merged.state == state::STATE_IDLE {
            self.tell_holds(pane);
        }
        merged
    }

    fn notify_waiters(&self, pane: &str, ev: &Event, now: f64) {
        let eff = state::effective_state(ev, now);
        let mut g = self.waiters.lock().unwrap_or_else(|e| e.into_inner());
        g.retain(|w| {
            if w.pane == pane && (w.until.is_empty() || w.until == eff.state) {
                if let Some(tx) = w.tx.lock().unwrap_or_else(|e| e.into_inner()).as_ref() {
                    let _ = tx.try_send(eff.clone());
                }
                return false;
            }
            true
        });
    }

    pub(super) fn effective_current(&self, pane: &str) -> Option<Event> {
        self.effective_state(pane)
    }

    /// Blocks until the pane's state equals until, the timeout passes, or
    /// the client goes away. The waiter is registered before the current
    /// state is read, so no state is lost between the two.
    fn wait_state(
        &self,
        pane: &str,
        until: &str,
        timeout: Duration,
        c: &Arc<Client>,
    ) -> (Event, bool) {
        let (tx, rx): (SyncSender<Event>, Receiver<Event>) = sync_channel(1);
        let w = Arc::new(Waiter {
            pane: pane.into(),
            until: until.into(),
            tx: Mutex::new(Some(tx)),
        });
        self.waiters
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(w.clone());
        let drop_w = || {
            self.waiters
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .retain(|x| !Arc::ptr_eq(x, &w));
            w.tx.lock().unwrap_or_else(|e| e.into_inner()).take();
        };
        if let Some(ev) = self.effective_current(pane) {
            if until.is_empty() || ev.state == until {
                drop_w();
                return (ev, true);
            }
        }
        let deadline = std::time::Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(std::time::Instant::now());
            if left.is_zero() {
                drop_w();
                return (self.effective_current(pane).unwrap_or_default(), false);
            }
            match rx.recv_timeout(left.min(Duration::from_millis(100))) {
                Ok(ev) => {
                    drop_w();
                    return (ev, true);
                }
                Err(_) => {
                    if c.dead() {
                        drop_w();
                        return (self.effective_current(pane).unwrap_or_default(), false);
                    }
                }
            }
        }
    }

    fn handle_report_state(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.report_state needs pane. Run: coppice pane list",
            );
        }
        if self.tree.pane(&id).is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice pane list", sys::go_quote(&id)),
            );
        }
        let Some(raw) = r.raw("event") else {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.report_state needs event, one PaneStateEvent object.",
            );
        };
        let mut ev = match state::parse_event(raw) {
            Ok(e) => e,
            Err(e) => return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e),
        };
        let extras = match state::parse_report_extras(raw) {
            Ok(x) => x,
            Err(e) => return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e),
        };
        ev.pane = Some(id.clone());
        // No harness of this build holds an ask itself.
        if let Some(a) = ev.ask.as_mut() {
            a.holder.clear();
        }
        ev.who = None;
        let mut downgraded = false;
        if ev.source == state::SRC_OPERATOR && !c.operator(&id) {
            ev.source = state::SRC_GATE.into();
            downgraded = true;
            if let Err(e) = ev.validate() {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!(
                        "a gate blocked report needs an ask: {e}. Attach to this pane to report as operator instead."
                    ),
                );
            }
        }
        if ev.source == state::SRC_OPERATOR {
            let (name, from) = c.who();
            if !name.is_empty() && from != "pane" && from != "plugin" {
                ev.who = Some(name);
            }
        }
        let ro = c.role();
        let own = ro.pane && !ro.unknown && ro.plugin.is_empty() && ro.pane_id == id;
        self.note_report(&id, &ev.harness, &extras, own);
        if own {
            if let Some(sid) = &ev.harness_session_id {
                self.note_harness_session(&id, &ev.harness, sid);
            }
        }
        let merged = self.apply_state(&id, ev);
        let mut res = vec![
            ("pane", json!(id)),
            ("source", json!(merged.source)),
            ("state", json!(merged.state)),
            ("detail", json!(merged.detail)),
        ];
        if let Some(a) = &merged.ask {
            res.push(("ask", a.to_value()));
        }
        if downgraded {
            res.push((
                "note",
                json!("source became gate. Attach to this pane to speak as the operator."),
            ));
        }
        proto::ok_resp(&r.id, gojson::map(res))
    }

    /// Stores the session id a live pty pane's own hook reported, when the
    /// report names the pane's own harness.
    fn note_harness_session(&self, id: &str, harness: &str, sid: &str) {
        if !session_id_ok(sid) {
            return;
        }
        let Some(rec) = self.tree.pane(id) else {
            return;
        };
        if rec.kind != layout::KIND_PTY
            || rec.closed
            || rec.harness_session_id == sid
            || !same_harness(harness, &rec.harness)
        {
            return;
        }
        let _ = self.update_pane(id, |p| {
            if p.kind == layout::KIND_PTY && !p.closed {
                p.harness_session_id = sid.into();
            }
        });
    }

    /// The one view agent.get, agent.list and agent.read build of a pane.
    fn agent_row(&self, id: &str) -> Option<Vec<(&'static str, Value)>> {
        let rec = self.tree.pane(id)?;
        let mut row: Vec<(&'static str, Value)> = vec![
            ("pane", json!(rec.id)),
            ("label", json!(rec.label)),
            ("cwd", json!(rec.cwd)),
            ("kind", json!(rec.kind)),
            ("harness", json!(rec.harness)),
            ("closed", json!(rec.closed)),
        ];
        let mut st = (json!(state::STATE_UNKNOWN), Value::Null, json!(""));
        if rec.closed {
            row.push(("ended_at", json!(rec.ended_at)));
            row.push(("exit_code", json!(rec.exit_code)));
        }
        if !rec.parent_pane.is_empty() {
            row.push(("parent_pane", json!(rec.parent_pane)));
        }
        if let Some(eff) = self.effective_current(&rec.id) {
            st = (json!(eff.state), json!(eff.source), json!(eff.detail));
            row.push(("ts", gojson::float(eff.ts)));
            row.push(("session_id", json!(eff.session_id)));
            if let Some(a) = &eff.ask {
                row.push(("ask", a.to_value()));
            }
            if let Some(h) = &eff.harness_session_id {
                row.push(("harness_session_id", json!(h)));
            } else if !rec.harness_session_id.is_empty() {
                row.push(("harness_session_id", json!(rec.harness_session_id)));
            }
        } else if rec.closed {
            let detail = match rec.exit_code {
                Some(c) => format!("exit={c}"),
                None => "ended before this server started".into(),
            };
            st = (
                json!(state::STATE_DONE),
                json!(state::SRC_PROCESS),
                json!(detail),
            );
            row.push(("ts", gojson::float(rec.ended_at as f64)));
            row.push(("session_id", json!(rec.id)));
            if !rec.harness_session_id.is_empty() {
                row.push(("harness_session_id", json!(rec.harness_session_id)));
            }
        }
        row.push(("state", st.0));
        row.push(("source", st.1));
        row.push(("detail", st.2));
        Some(row)
    }

    fn handle_agent_list(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let want_ended = r.bool("ended").unwrap_or(false);
        let mut matched: Vec<layout::Pane> = self
            .tree
            .panes()
            .into_iter()
            .filter(|p| p.closed == want_ended)
            .collect();
        if want_ended {
            matched.sort_by(|a, b| b.ended_at.cmp(&a.ended_at).then_with(|| b.id.cmp(&a.id)));
        }
        let out: Vec<Value> = matched
            .iter()
            .filter_map(|p| self.agent_row(&p.id))
            .map(gojson::map)
            .collect();
        proto::ok_resp(&r.id, gojson::map(vec![("agents", Value::Array(out))]))
    }

    fn handle_agent_get(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        match self.agent_row(&id) {
            Some(row) => proto::ok_resp(&r.id, gojson::map(row)),
            None => proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice agent list", sys::go_quote(&id)),
            ),
        }
    }

    fn handle_agent_prompt(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let Some(text) = r.str("text") else {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, "agent.prompt needs text.");
        };
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return super::role::refuse_view_only(r, &id);
        }
        if let Some(a) = &lp.adapter {
            // A harness that holds asks of its own learns who typed, so a
            // pane's text can never answer one.
            if let Err(e) = a.prompt_from(&text, !c.role().no_allow) {
                return proto::err_resp(&r.id, proto::ERR_ADAPTER, &e);
            }
        } else if self.gated(&id) {
            // A sender that still waits when a drop comes hears of it as
            // its error.
            let from = if c.role().pane {
                c.role().pane_id
            } else {
                String::new()
            };
            match self.submit_prompt(&id, &text, &from, Some(c)) {
                Err(e) => return self.prompt_err(r, e),
                Ok(out) if !out.sent => {
                    return self.queued_prompt_reply(c, r, &id, &text, out);
                }
                Ok(_) => {}
            }
        } else if let Err(e) = lp.write(format!("{text}\r").as_bytes()) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
        }
        self.mark_input(&id, true);
        self.prompt_wait(c, r, &id, text.len())
    }

    pub(super) fn prompt_wait(
        &self,
        c: &Arc<Client>,
        r: &Request,
        id: &str,
        sent: usize,
    ) -> Response {
        if !r.bool("wait").unwrap_or(false) {
            return proto::ok_resp(
                &r.id,
                gojson::map(vec![("pane", json!(id)), ("sent", json!(sent))]),
            );
        }
        self.wait_reply_for(c, r, id)
    }

    fn wait_reply_for(&self, c: &Arc<Client>, r: &Request, id: &str) -> Response {
        let until = r.str("until").unwrap_or_else(|| state::STATE_IDLE.into());
        let ms = r.int("timeout_ms").filter(|m| *m > 0).unwrap_or(120000);
        let (ev, done) = self.wait_state(id, &until, Duration::from_millis(ms as u64), c);
        if !done {
            if c.dead() {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_TIMEOUT,
                    "client disconnected while waiting",
                );
            }
            return proto::err_resp(
                &r.id,
                proto::ERR_TIMEOUT,
                &format!(
                    "pane {id} is {} after {ms} ms, not {until}. Run: coppice agent get {id}",
                    or_unknown(&ev.state)
                ),
            );
        }
        proto::ok_resp(&r.id, wait_reply(id, &ev))
    }

    fn handle_agent_wait(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if self.tree.pane(&id).is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice agent list", sys::go_quote(&id)),
            );
        }
        self.wait_reply_for(c, r, &id)
    }

    fn handle_agent_read(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "this command needs pane. Run: coppice agent list",
            );
        }
        let no_such = || {
            proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice agent list", sys::go_quote(&id)),
            )
        };
        if self.tree.pane(&id).is_none() {
            return no_such();
        }
        let closed = || {
            proto::err_resp(
                &r.id,
                proto::ERR_PANE_CLOSED,
                &format!("pane {id} has closed. Create a new one with: coppice pane create"),
            )
        };
        let Some(lp) = self.live(&id) else {
            return closed();
        };
        let region = r
            .str("region")
            .or_else(|| r.str("source"))
            .unwrap_or_else(|| "recent".into());
        let text = match lp.grid.read(&region) {
            Ok(t) => t,
            Err(e) if e == crate::pane::GRID_CLOSED => return closed(),
            Err(e) => return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e),
        };
        let Some(mut row) = self.agent_row(&id) else {
            return no_such();
        };
        row.push(("text", json!(text)));
        row.push(("region", json!(region)));
        proto::ok_resp(&r.id, gojson::map(row))
    }
}
