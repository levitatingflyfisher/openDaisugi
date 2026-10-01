//! Subagents: pane.report_child. A gate hook reports a subagent that
//! started or stopped inside a pane's harness. It is read only: coppice
//! cannot type into it or close it.

use std::collections::HashMap;
use std::sync::Arc;

use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::state;
use crate::sys;
use crate::textwidth;

/// How long a done subagent stays on its parent's row, in seconds.
const CHILD_DONE_KEEP: f64 = 60.0;
/// The most subagents one pane keeps. The oldest go first.
const CHILDREN_KEPT: usize = 32;
/// How many runes a child's id and label keep, as a note's text does.
const NOTE_MAX: usize = 200;

/// One subagent inside a pane's harness.
#[derive(Clone, Debug)]
pub struct Child {
    id: String,
    label: String,
    state: String,
    ts: f64,
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "pane.report_child",
        Arc::new(|s, c, r| s.handle_report_child(c, r)),
    );
}

impl Server {
    fn children_lock(&self) -> std::sync::MutexGuard<'_, HashMap<String, HashMap<String, Child>>> {
        self.children.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Records one subagent of pane and sends a child event. An empty
    /// label keeps the one the child had.
    pub fn report_child(&self, pane: &str, id: &str, st: &str, label: &str) {
        let now = Server::now_seconds();
        let label = textwidth::printable(label, NOTE_MAX);
        let id = textwidth::printable(id, NOTE_MAX);
        let ev = {
            let mut g = self.children_lock();
            let kids = g.entry(pane.to_string()).or_default();
            let c = kids.entry(id.clone()).or_insert_with(|| Child {
                id: id.clone(),
                label: String::new(),
                state: String::new(),
                ts: 0.0,
            });
            c.state = st.to_string();
            c.ts = now;
            if !label.is_empty() {
                c.label = label;
            }
            let child_label = c.label.clone();
            if kids.len() > CHILDREN_KEPT {
                // Go ranges over a map, so a tie on the oldest time picks
                // any one of them. The earliest id stands in for it here.
                let oldest = kids
                    .values()
                    .min_by(|a, b| a.ts.total_cmp(&b.ts).then_with(|| a.id.cmp(&b.id)))
                    .map(|k| k.id.clone());
                if let Some(o) = oldest {
                    kids.remove(&o);
                }
            }
            gojson::obj(vec![
                ("event", json!("child")),
                ("pane", json!(pane)),
                ("child", json!(id)),
                ("state", json!(st)),
                ("label", json!(child_label)),
                ("ts", gojson::float(now)),
            ])
        };
        self.broadcast("child", pane, &ev);
    }

    /// The subagents of one pane, oldest first. A done child older than
    /// CHILD_DONE_KEEP is dropped.
    pub fn children_of(&self, pane: &str) -> Vec<Value> {
        let now = Server::now_seconds();
        let mut g = self.children_lock();
        let Some(kids) = g.get_mut(pane) else {
            return Vec::new();
        };
        kids.retain(|_, k| !(k.state == state::STATE_DONE && now - k.ts > CHILD_DONE_KEEP));
        let mut list: Vec<&Child> = kids.values().collect();
        list.sort_by(|a, b| a.ts.total_cmp(&b.ts).then_with(|| a.id.cmp(&b.id)));
        list.iter()
            .map(|k| {
                gojson::map(vec![
                    ("id", json!(k.id)),
                    ("label", json!(k.label)),
                    ("state", json!(k.state)),
                    ("ts", gojson::float(k.ts)),
                ])
            })
            .collect()
    }

    /// Drops the subagents of a pane that closed.
    pub fn forget_children(&self, pane: &str) {
        self.children_lock().remove(pane);
    }

    fn handle_report_child(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.report_child needs pane. Run: coppice pane list",
            );
        }
        if self.tree.pane(&id).is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice pane list", sys::go_quote(&id)),
            );
        }
        let kid = r.s("child").trim().to_string();
        if kid.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.report_child needs child, the subagent id.",
            );
        }
        let st = r.s("state");
        if st != state::STATE_WORKING && st != state::STATE_DONE {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "state must be working or done",
            );
        }
        self.report_child(&id, &kid, &st, &r.s("label"));
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("pane", json!(id)),
                ("child", json!(kid)),
                ("state", json!(st)),
            ]),
        )
    }
}
