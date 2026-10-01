//! The task verbs. A task is the unit of work above the pane: a label, a
//! parent, a model, a cwd, and maybe a git worktree of its own.

use std::collections::HashSet;
use std::sync::Arc;

use serde_json::{json, Value};

use super::role::clean_label;
use super::{Client, Server};
use crate::gojson;
use crate::layout::{self, LayoutError};
use crate::proto::{self, Request, Response};
use crate::state;
use crate::sys;
use crate::worktree;

/// The first label of every path: the floor itself.
pub const ROOT_LABEL: &str = "floor";

/// The sentence every refusal about a worktree ends with: the two ways out
/// of a tree coppice will not delete.
const KEEP_WORKTREE_HINT: &str = "Commit them, or pass keep_worktree: true.";

/// The refusal every verb gives for an id that names no task.
pub fn no_task(req_id: &str, id: &str) -> Response {
    proto::err_resp(
        req_id,
        proto::ERR_BAD_REQUEST,
        &format!("no task {id}. Run: coppice task list"),
    )
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "task.create",
        Arc::new(|s, c, r| s.handle_task_create(c, r)),
    );
    let _ = s.handle("task.list", Arc::new(|s, c, r| s.handle_task_list(c, r)));
    let _ = s.handle("task.close", Arc::new(|s, c, r| s.handle_task_close(c, r)));
    let _ = s.handle("task.move", Arc::new(|s, c, r| s.handle_task_move(c, r)));
    let _ = s.handle(
        "task.set_foreman",
        Arc::new(|s, c, r| s.handle_set_foreman(c, r)),
    );
}

impl Server {
    fn handle_set_foreman(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("task");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "task.set_foreman needs task. Run: coppice task list",
            );
        }
        if self.tree.task(&id).is_none() {
            return no_task(&r.id, &id);
        }
        let pane = r.s("pane");
        if !pane.is_empty() {
            let Some(p) = self.tree.pane(&pane) else {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_NO_SUCH_PANE,
                    &format!("no pane {}. Run: coppice pane list", sys::go_quote(&pane)),
                );
            };
            if p.closed {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!("pane {pane} is closed. A foreman must be open."),
                );
            }
        }
        if self.tree.set_foreman(&id, &pane).is_err() {
            return no_task(&r.id, &id);
        }
        self.save_layout();
        self.recheck_holds();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("task", json!(id)), ("foreman", json!(pane))]),
        )
    }

    fn handle_task_create(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let label = clean_label(&r.s("label"));
        if label.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "task.create needs label. Pass the task's name.",
            );
        }
        let parent = r.s("parent");
        if !parent.is_empty() && self.tree.task(&parent).is_none() {
            return no_task(&r.id, &parent);
        }
        let cwd = r.s("cwd");
        let mut path = String::new();
        let mut repo = String::new();
        // The worktree is made before the record, so a failed add leaves
        // nothing.
        if r.bool("worktree") == Some(true) {
            if cwd.is_empty() {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    "task.create with worktree needs cwd. Pass a directory inside the repo.",
                );
            }
            repo = match worktree::toplevel(&cwd) {
                Ok(top) => top,
                Err(_) => {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_BAD_REQUEST,
                        &format!("worktree needs a git repo. {cwd} is not inside one."),
                    )
                }
            };
            path = match worktree::add(&repo, &label) {
                Ok(p) => p,
                Err(e) => return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e),
            };
        }
        let rec = match self.tree.create_task(layout::Task {
            label: label.clone(),
            parent: parent.clone(),
            cwd,
            worktree: path.clone(),
            model: r.s("model"),
            ..Default::default()
        }) {
            Ok(t) => t,
            Err(e) => {
                if !path.is_empty() {
                    let _ = worktree::remove(&repo, &label, false);
                }
                return match e {
                    LayoutError::NoTask(_) => no_task(&r.id, &parent),
                    e => proto::err_resp(&r.id, proto::ERR_INTERNAL, &e.to_string()),
                };
            }
        };
        self.save_layout();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("task", json!(rec.id)), ("worktree", json!(path))]),
        )
    }

    fn task_row(&self, tk: &layout::Task, pane_state: &dyn Fn(&str) -> String) -> Value {
        let panes: Vec<String> = self
            .tree
            .panes()
            .into_iter()
            .filter(|p| p.task_id == tk.id)
            .map(|p| p.id)
            .collect();
        let mut row = vec![
            ("id", json!(tk.id)),
            ("label", json!(tk.label)),
            ("parent", json!(tk.parent)),
            ("cwd", json!(tk.cwd)),
            ("worktree", json!(tk.worktree)),
            ("model", json!(tk.model)),
            ("foreman", json!(tk.foreman)),
            (
                "state",
                json!(layout::task_state(tk, &self.tree, pane_state)),
            ),
            ("panes", json!(panes)),
        ];
        // ahead is there only when git could count the worktree's commits
        // past its upstream.
        if !tk.worktree.is_empty() {
            if let Ok((ahead, _, _)) = worktree::status(&tk.worktree) {
                row.push(("ahead", json!(ahead)));
            }
        }
        gojson::map(row)
    }

    /// The scope a list verb names: with no scope, every task and the root
    /// path; with one, the task and its descendants, and the labels from
    /// the root down to it.
    pub fn scope_of(
        &self,
        r: &Request,
    ) -> Result<(Option<HashSet<String>>, Vec<String>), Response> {
        let mut path = vec![ROOT_LABEL.to_string()];
        let id = r.s("scope");
        if id.is_empty() {
            return Ok((None, path));
        }
        let sub = self.tree.subtree(&id).map_err(|_| no_task(&r.id, &id))?;
        let keep: HashSet<String> = sub.into_iter().map(|t| t.id).collect();
        let mut chain = Vec::new();
        let mut seen = HashSet::new();
        let mut cur = id;
        while !cur.is_empty() && seen.insert(cur.clone()) {
            let Some(tk) = self.tree.task(&cur) else {
                break;
            };
            chain.insert(
                0,
                if tk.label.is_empty() {
                    tk.id.clone()
                } else {
                    tk.label.clone()
                },
            );
            cur = tk.parent;
        }
        path.extend(chain);
        Ok((Some(keep), path))
    }

    fn handle_task_list(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let (keep, path) = match self.scope_of(r) {
            Ok(x) => x,
            Err(resp) => return resp,
        };
        // A blocked pane whose ask a foreman holds folds as working: the
        // task waits on its foreman, not on the operator.
        let pane_state = |id: &str| -> String {
            match self.effective_state(id) {
                None => state::STATE_UNKNOWN.into(),
                Some(ev) if ev.state == state::STATE_BLOCKED && self.held_for(id).is_some() => {
                    state::STATE_WORKING.into()
                }
                Some(ev) => ev.state,
            }
        };
        let out: Vec<Value> = self
            .tree
            .tasks()
            .iter()
            .filter(|t| keep.as_ref().is_none_or(|k| k.contains(&t.id)))
            .map(|t| self.task_row(t, &pane_state))
            .collect();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("tasks", Value::Array(out)), ("path", json!(path))]),
        )
    }

    fn handle_task_close(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("task");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "task.close needs task. Run: coppice task list",
            );
        }
        let Ok(subtree) = self.tree.subtree(&id) else {
            return no_task(&r.id, &id);
        };
        // Every worktree in the subtree is checked for uncommitted changes
        // before anything happens. Then the panes close, then the
        // worktrees go, then the records. Branches are never deleted.
        let keep = r.bool("keep_worktree").unwrap_or(false);
        if !keep {
            for tk in subtree.iter().filter(|t| !t.worktree.is_empty()) {
                match worktree::dirty(&tk.worktree) {
                    Err(e) => {
                        return proto::err_resp(
                            &r.id,
                            proto::ERR_BAD_REQUEST,
                            &format!(
                                "task {id} has a worktree at {} that git cannot read: {e}. Pass keep_worktree: true to leave it.",
                                tk.worktree
                            ),
                        )
                    }
                    Ok(true) => {
                        return proto::err_resp(
                            &r.id,
                            proto::ERR_BAD_REQUEST,
                            &format!(
                                "task {id} has uncommitted changes in {}. {KEEP_WORKTREE_HINT}",
                                tk.worktree
                            ),
                        )
                    }
                    Ok(false) => {}
                }
            }
        }
        let gone: HashSet<String> = subtree.iter().map(|t| t.id.clone()).collect();
        let mut closed = Vec::new();
        for p in self.tree.panes() {
            if !gone.contains(&p.task_id) || p.closed {
                continue;
            }
            self.close_record(&p);
            closed.push(p.id);
        }
        if !keep {
            for tk in subtree.iter().filter(|t| !t.worktree.is_empty()) {
                let removed = worktree::repo(&tk.worktree)
                    .and_then(|repo| worktree::remove_at(&repo, &tk.worktree));
                if let Err(e) = removed {
                    self.save_layout();
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_INTERNAL,
                        &format!(
                            "task {id}: its panes closed, but git kept the worktree at {}: {e}. {KEEP_WORKTREE_HINT}",
                            tk.worktree
                        ),
                    );
                }
            }
        }
        if self.tree.close_task(&id).is_err() {
            self.save_layout();
            return no_task(&r.id, &id);
        }
        self.save_layout();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("task", json!(id)),
                ("closed", json!(true)),
                ("panes", json!(closed)),
            ]),
        )
    }

    fn handle_task_move(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("task");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "task.move needs task. Run: coppice task list",
            );
        }
        let parent = r.s("parent");
        match self.tree.move_task(&id, &parent) {
            Ok(()) => {}
            Err(LayoutError::Cycle(..)) => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!(
                        "task {id} cannot move under {parent}: that is itself or one of its own descendants."
                    ),
                )
            }
            Err(_) => {
                let missing = if self.tree.task(&id).is_some() {
                    parent.clone()
                } else {
                    id.clone()
                };
                return no_task(&r.id, &missing);
            }
        }
        self.save_layout();
        self.recheck_holds();
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("task", json!(id)), ("parent", json!(parent))]),
        )
    }
}
