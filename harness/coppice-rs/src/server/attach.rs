//! pane.attach, pane.detach, events.subscribe, events.pause and
//! events.resume: what watches a pane and turns its grid into frames.

use std::collections::HashSet;
use std::sync::mpsc::{sync_channel, Receiver, SyncSender, TrySendError};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::pane::{Frame, FrameState};
use crate::proto::{self, Request, Response};

/// The 60 Hz ceiling: a redrawing spinner is not a frame per redraw.
const FRAME_INTERVAL: Duration = Duration::from_millis(16);
/// How many frames one attached pane may hold for one client before the
/// pump drops new ones.
const FRAME_BUFFER: usize = 64;

#[derive(Default)]
struct FlowState {
    paused: bool,
    want_full: bool,
    await_full: bool,
    pending: u64,
}

/// The frame path for one attached pane on one client.
pub struct PaneFlow {
    st: Mutex<FlowState>,
    /// Covers the drain's check of paused together with its write, so no
    /// frame follows an events.pause reply.
    send_mu: Mutex<()>,
}

struct Queued {
    frame: Frame,
    dropped: u64,
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle("pane.attach", Arc::new(|s, c, r| s.handle_attach(c, r)));
    let _ = s.handle("pane.detach", Arc::new(|s, c, r| s.handle_detach(c, r)));
    let _ = s.handle(
        "events.subscribe",
        Arc::new(|s, c, r| s.handle_subscribe(c, r)),
    );
    let _ = s.handle("events.pause", Arc::new(|s, c, r| s.handle_pause(c, r)));
    let _ = s.handle("events.resume", Arc::new(|s, c, r| s.handle_resume(c, r)));
}

impl Client {
    /// Writes one event line in this connection's framing.
    fn send_event(&self, ev: &Value) -> bool {
        match proto::encode_event(ev, self.framing()) {
            Some(line) => self.send_line(&line).is_ok(),
            None => false,
        }
    }

    fn drain_frames(self: Arc<Client>, flow: Arc<PaneFlow>, rx: Receiver<Queued>) {
        for mut q in rx {
            let _send = flow.send_mu.lock().unwrap_or_else(|e| e.into_inner());
            let discard = {
                let mut st = flow.st.lock().unwrap_or_else(|e| e.into_inner());
                let discard = st.paused || (st.await_full && !q.frame.full);
                if discard {
                    st.pending += q.dropped;
                } else {
                    st.await_full = false;
                    q.dropped += st.pending;
                    st.pending = 0;
                }
                discard
            };
            if discard {
                continue;
            }
            let mut ok = true;
            if q.dropped > 0 {
                ok = self.send_event(&gojson::obj(vec![
                    ("event", json!("frame_gap")),
                    ("pane", json!(q.frame.pane)),
                    ("dropped", json!(q.dropped)),
                ]));
            }
            if ok {
                ok = self.send_event(&q.frame.to_value());
            }
            if !ok {
                self.drop_client();
                return;
            }
        }
    }

    fn flow_for(&self, r: &Request) -> Result<(Arc<PaneFlow>, String), Response> {
        let id = r.s("pane");
        if id.is_empty() {
            return Err(proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "this command needs pane. Run: coppice pane list",
            ));
        }
        let flow = self
            .st
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .flows
            .get(&id)
            .cloned();
        match flow {
            Some(f) => Ok((f, id)),
            None => Err(proto::err_resp(
                &r.id,
                proto::ERR_NOT_ATTACHED,
                &format!("this client is not attached to {id}. Run: coppice attach {id}"),
            )),
        }
    }
}

impl Server {
    fn handle_attach(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        let view_only = r.bool("view_only").unwrap_or(false);
        if !view_only {
            if let (Some(cols), Some(rows)) = (r.int("cols"), r.int("rows")) {
                if cols > 0 && rows > 0 {
                    let _ = match &lp.pty {
                        Some(p) => p.resize(cols, rows),
                        None => lp.grid.resize(cols, rows),
                    };
                    let _ = self.update_pane(&id, |x| {
                        x.cols = cols;
                        x.rows = rows;
                    });
                }
            }
        }
        let started = {
            let mut st = c.st.lock().unwrap_or_else(|e| e.into_inner());
            if view_only {
                st.view_only.insert(id.clone());
            } else {
                st.view_only.remove(&id);
            }
            if st.attached.contains_key(&id) {
                None
            } else {
                let gen = self
                    .attach_gen
                    .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                st.attached.insert(id.clone(), gen);
                let flow = Arc::new(PaneFlow {
                    st: Mutex::new(FlowState::default()),
                    send_mu: Mutex::new(()),
                });
                st.flows.insert(id.clone(), flow.clone());
                st.subs.insert("state".into(), true);
                st.subs.insert("frame".into(), true);
                let covered = st.sub_panes.get("*").copied().unwrap_or(false)
                    || st.sub_panes.get(&id).copied().unwrap_or(false);
                if !covered {
                    st.auto_panes.insert(id.clone());
                }
                st.sub_panes.insert(id.clone(), true);
                Some((gen, flow))
            }
        };
        if let Some((gen, flow)) = started {
            let s = self.clone();
            let c2 = c.clone();
            let id2 = id.clone();
            thread::spawn(move || s.frame_pump(c2, id2, gen, flow));
            self.tell_presence(&id);
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("attached", json!(true))]),
        )
    }

    /// The sorted names of the people attached to pane id now.
    pub fn looking(&self, id: &str) -> Vec<String> {
        let mut seen = HashSet::new();
        let mut out = Vec::new();
        for c in self.clients() {
            if c.dead() || !c.operator(id) {
                continue;
            }
            let (name, from) = c.who();
            if name.is_empty() || from == "pane" || from == "plugin" || !seen.insert(name.clone()) {
                continue;
            }
            out.push(name);
        }
        out.sort();
        out
    }

    /// Sends who looks at pane id to every client that subscribed to
    /// presence, in order.
    pub fn tell_presence(&self, id: &str) {
        let _g = self.presence_mu.lock().unwrap_or_else(|e| e.into_inner());
        let ev = gojson::obj(vec![
            ("event", json!("presence")),
            ("pane", json!(id)),
            ("looking", json!(self.looking(id))),
        ]);
        self.broadcast("presence", id, &ev);
    }

    /// One pump per attached client per pane, on a fixed tick.
    fn frame_pump(self: Arc<Server>, c: Arc<Client>, pane: String, gen: u64, flow: Arc<PaneFlow>) {
        let (tx, rx): (SyncSender<Queued>, Receiver<Queued>) = sync_channel(FRAME_BUFFER);
        {
            let c2 = c.clone();
            let f2 = flow.clone();
            thread::spawn(move || c2.drain_frames(f2, rx));
        }
        let forget = || {
            let was = {
                let mut st = c.st.lock().unwrap_or_else(|e| e.into_inner());
                let was = st.attached.get(&pane) == Some(&gen);
                if was {
                    st.attached.remove(&pane);
                    st.flows.remove(&pane);
                    st.view_only.remove(&pane);
                }
                was
            };
            if was {
                self.tell_presence(&pane);
            }
        };
        let mut fs = FrameState::default();
        let mut dropped = 0u64;
        loop {
            thread::sleep(FRAME_INTERVAL);
            if c.dead() {
                forget();
                return;
            }
            let still =
                c.st.lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .attached
                    .get(&pane)
                    == Some(&gen);
            if !still {
                return;
            }
            let Some(lp) = self.live(&pane) else {
                forget();
                return;
            };
            let (paused, want_full) = {
                let mut st = flow.st.lock().unwrap_or_else(|e| e.into_inner());
                let pw = (st.paused, st.want_full);
                if !st.paused {
                    st.want_full = false;
                }
                pw
            };
            if paused {
                continue;
            }
            if want_full {
                fs.force_full();
            }
            let frame = match fs.next(&pane, &lp.grid) {
                Ok(Some(f)) => f,
                Ok(None) => continue,
                Err(_) => {
                    forget();
                    return;
                }
            };
            match tx.try_send(Queued { frame, dropped }) {
                Ok(()) => dropped = 0,
                Err(TrySendError::Full(_)) => {
                    dropped += 1;
                    flow.st.lock().unwrap_or_else(|e| e.into_inner()).want_full = true;
                }
                Err(TrySendError::Disconnected(_)) => {
                    forget();
                    return;
                }
            }
        }
    }

    fn handle_pause(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let (flow, id) = match c.flow_for(r) {
            Ok(x) => x,
            Err(resp) => return resp,
        };
        {
            let _send = flow.send_mu.lock().unwrap_or_else(|e| e.into_inner());
            flow.st.lock().unwrap_or_else(|e| e.into_inner()).paused = true;
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("paused", json!(true))]),
        )
    }

    fn handle_resume(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let (flow, id) = match c.flow_for(r) {
            Ok(x) => x,
            Err(resp) => return resp,
        };
        {
            let mut st = flow.st.lock().unwrap_or_else(|e| e.into_inner());
            st.paused = false;
            st.want_full = true;
            st.await_full = true;
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("paused", json!(false))]),
        )
    }

    fn handle_detach(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        let was = {
            let mut st = c.st.lock().unwrap_or_else(|e| e.into_inner());
            let was = st.attached.remove(&id).is_some();
            st.flows.remove(&id);
            st.view_only.remove(&id);
            if was && st.auto_panes.remove(&id) {
                st.sub_panes.remove(&id);
            }
            was
        };
        if !was {
            return proto::err_resp(
                &r.id,
                proto::ERR_NOT_ATTACHED,
                &format!("this client is not attached to {id}. Run: coppice attach {id}"),
            );
        }
        self.tell_presence(&id);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("attached", json!(false))]),
        )
    }

    fn handle_subscribe(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        const PANES_HINT: &str = "panes must be \"*\" or a non-empty list of pane ids";
        let kinds = match r.str_slice("kinds") {
            Some(k) if !k.is_empty() => k,
            _ => vec!["state".to_string(), "layout".to_string()],
        };
        for k in &kinds {
            if !matches!(
                k.as_str(),
                "state" | "layout" | "frame" | "note" | "child" | "presence" | "messages"
            ) {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!(
                        "kind {} is not state, layout, frame, note, child, presence or messages",
                        crate::sys::go_quote(k)
                    ),
                );
            }
        }
        let panes: Vec<String> = match r.raw("panes") {
            None => vec!["*".into()],
            Some("null") => vec![String::new()],
            Some(raw) => match serde_json::from_str::<String>(raw) {
                Ok(one) => vec![one],
                Err(_) => match r.str_slice("panes") {
                    Some(list) => list,
                    None => return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, PANES_HINT),
                },
            },
        };
        if panes.is_empty() || panes.iter().any(|p| p.is_empty()) {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, PANES_HINT);
        }
        {
            let mut st = c.st.lock().unwrap_or_else(|e| e.into_inner());
            for k in &kinds {
                st.subs.insert(k.clone(), true);
            }
            for p in &panes {
                st.sub_panes.insert(p.clone(), true);
                if p == "*" {
                    st.auto_panes.clear();
                } else {
                    st.auto_panes.remove(p);
                }
            }
        }
        if kinds.iter().any(|k| k == "messages") {
            self.watch_messages();
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("kinds", json!(kinds)), ("panes", json!(panes))]),
        )
    }
}
