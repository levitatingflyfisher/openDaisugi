//! Ended panes and the restart. A pane whose process ended on its own keeps
//! its record, closed, for seven days: pane.forget removes it, and
//! pane.resume starts a new pane from it. A restart reads layout.json back
//! and brings back the tree, the labels, the working directories and the
//! id counters, and no process.

use std::sync::Arc;

use serde_json::json;

use super::panes::{unix_secs, ENDED_TTL_SECS};
use super::role::{clean_label, is_foreman_label, DEFAULT_FOREMAN_LABEL};
use super::{Client, RestoreReport, Server};
use crate::config;
use crate::gojson;
use crate::layout::{self, LayoutError};
use crate::proto::{self, Request, Response};
use crate::state::{self, Event};
use crate::sys;

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "pane.forget",
        Arc::new(|s, c, r| s.handle_pane_forget(c, r)),
    );
    let _ = s.handle(
        "pane.resume",
        Arc::new(|s, c, r| s.handle_pane_resume(c, r)),
    );
}

/// Go's %v of a list of strings: the items between brackets, with one
/// space between them.
fn go_list(v: &[String]) -> String {
    format!("[{}]", v.join(" "))
}

/// A pty pane's resume command line: the harness's command and args, then
/// resume_args with each "{session}" replaced by sid, then the pane's own
/// extra args. None when the config has no table for name, or the table has
/// no command or no resume_args.
fn resume_argv_for(name: &str, sid: &str, extra_args: &[String]) -> Option<Vec<String>> {
    let cfg = config::load().ok()??;
    let h = cfg.harness.get(name)?;
    if h.resume_args().is_empty() || h.command.is_empty() {
        return None;
    }
    let mut argv = vec![h.command.clone()];
    argv.extend(h.args().iter().cloned());
    argv.extend(h.resume_args().iter().map(|a| a.replace("{session}", sid)));
    argv.extend(extra_args.iter().cloned());
    Some(argv)
}

impl Server {
    /// Removes every ended record older than seven days. Returns how many.
    pub fn sweep_ended(&self) -> usize {
        let cutoff = unix_secs() - ENDED_TTL_SECS;
        let mut n = 0;
        for p in self.tree.panes() {
            if p.closed && p.ended_at != 0 && p.ended_at < cutoff {
                self.drop_record(&p.id);
                n += 1;
            }
        }
        n
    }

    fn handle_pane_forget(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let all = r.bool("ended").unwrap_or(false);
        let id = r.s("pane");
        if all && !id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.forget takes pane, or ended: true, not both.",
            );
        }
        if all {
            let mut n = 0;
            for p in self.tree.panes() {
                if p.closed {
                    self.drop_record(&p.id);
                    n += 1;
                }
            }
            return proto::ok_resp(&r.id, gojson::map(vec![("forgot", json!(n))]));
        }
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.forget needs pane, or ended: true to forget every ended record.",
            );
        }
        let Some(rec) = self.tree.pane(&id) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!(
                    "no pane {}. Run: coppice pane list --ended",
                    sys::go_quote(&id)
                ),
            );
        };
        if !rec.closed {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("pane {id} is still live. Close it first: coppice pane close {id}"),
            );
        }
        self.drop_record(&id);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("forgot", json!(true))]),
        )
    }

    /// Marks id as having a pane.resume in flight. False when it already
    /// has one.
    fn claim_resume(&self, id: &str) -> bool {
        self.resuming
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(id.to_string())
    }

    fn release_resume(&self, id: &str) {
        self.resuming
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(id);
    }

    fn handle_pane_resume(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        if self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a new pane.",
            );
        }
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.resume needs pane. Run: coppice pane list --ended",
            );
        }
        let cols = r.int("cols");
        let rows = r.int("rows");
        if cols.is_some_and(|c| c <= 0) || rows.is_some_and(|c| c <= 0) {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "pane.resume takes positive cols and rows, or neither.",
            );
        }
        if !self.claim_resume(&id) {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("pane {id} is already resuming."),
            );
        }
        let resp = self.resume_claimed(r, &id, cols, rows);
        self.release_resume(&id);
        resp
    }

    /// pane.resume once the id is claimed.
    fn resume_claimed(
        self: &Arc<Server>,
        r: &Request,
        id: &str,
        cols: Option<i64>,
        rows: Option<i64>,
    ) -> Response {
        let Some(rec) = self.tree.pane(id) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!(
                    "no pane {}. Run: coppice pane list --ended",
                    sys::go_quote(id)
                ),
            );
        };
        if !rec.closed {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("pane {id} is still live. Run: coppice pane list"),
            );
        }
        let mut argv = rec.argv.clone();
        let mut resumed = false;
        let mut session_id = String::new();
        let mut fork_pending = false;
        let mut parent_pane = String::new();
        if rec.kind == layout::KIND_HEADLESS && !rec.harness_session_id.is_empty() {
            session_id = rec.harness_session_id.clone();
            fork_pending = rec.fork_pending;
            parent_pane = rec.parent_pane.clone();
            resumed = true;
        } else if rec.kind == layout::KIND_PTY && !rec.harness_session_id.is_empty() {
            if let Some(a) = resume_argv_for(&rec.harness, &rec.harness_session_id, &rec.extra_args)
            {
                argv = Some(a);
                session_id = rec.harness_session_id.clone();
                resumed = true;
            }
        }
        let cols = cols.unwrap_or(rec.cols);
        let rows = rows.unwrap_or(rec.rows);
        // A record labelled foreman is the floor's foreman again only when
        // it is the one the server tracks. The talk lock is held to the
        // end, so a talk cannot start a foreman in between.
        let mut label = clean_label(&rec.label);
        let tracked = {
            let mut t = self.talk_lock();
            self.tracked_foreman(&mut t) == id
        };
        let talk_held = is_foreman_label(&label) || tracked;
        let mut talk = talk_held.then(|| self.talk_lock());
        if let Some(t) = talk.as_mut() {
            if self.tracked_foreman(t) != id && is_foreman_label(&label) {
                label = format!("{DEFAULT_FOREMAN_LABEL}-old");
            }
        }
        let created = self.tree.create_pane(
            &rec.workspace,
            &rec.tab,
            layout::Pane {
                label,
                cwd: rec.cwd.clone(),
                argv,
                env: rec.env.clone(),
                kind: rec.kind.clone(),
                harness: rec.harness.clone(),
                cols,
                rows,
                task_id: rec.task_id.clone(),
                harness_session_id: session_id,
                fork_pending,
                parent_pane,
                extra_args: rec.extra_args.clone(),
                ..Default::default()
            },
        );
        let new_rec = match created {
            Ok(p) => p,
            Err(e @ LayoutError::NoWorkspace(_)) => {
                return proto::err_resp(&r.id, proto::ERR_NO_SUCH_WORKSPACE, &e.to_string())
            }
            Err(e) => return proto::err_resp(&r.id, proto::ERR_NO_SUCH_TAB, &e.to_string()),
        };
        // A resumed session writes on in the transcripts the old pane
        // claimed, so they move to the new pane before its hook can report.
        if resumed {
            self.move_claims(id, &new_rec.id);
        }
        if let Err(e) = self.start_pane(&new_rec) {
            if resumed {
                self.move_claims(&new_rec.id, id);
            }
            // Nothing ran, so nothing stays of the new record. The ended
            // one is left as it was.
            let _ = self.tree.remove_pane(&new_rec.id);
            self.remove_live(&new_rec.id);
            self.states.forget(&new_rec.id);
            self.save_layout();
            return proto::err_resp(&r.id, e.code(), &e.msg);
        }
        self.drop_record(id);
        if let Some(t) = talk.as_mut() {
            self.follow_foreman_locked(t, id, &new_rec.id, resumed);
        }
        drop(talk);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("pane", json!(new_rec.id)),
                ("workspace", json!(new_rec.workspace)),
                ("tab", json!(new_rec.tab)),
                ("resumed", json!(resumed)),
            ]),
        )
    }

    /// Reads layout.json back. It refuses without the start lock, once
    /// serving has started, and while any pane is live. A pty pane comes
    /// back closed and done; a headless one, which this build cannot
    /// resume, comes back closed and unknown; one already closed is
    /// counted apart. The sweep of old ended records then runs once.
    pub fn restore(self: &Arc<Self>) -> Result<(), String> {
        if !self.holds_start_lock() {
            return Err("call AcquireStartLock before Restore. Two starts restoring the same layout would each spawn resumed panes.".into());
        }
        if self.serving.load(std::sync::atomic::Ordering::SeqCst) {
            return Err("cannot restore after Serve has started. Restore runs before the server accepts connections.".into());
        }
        let live = self.live.read().unwrap_or_else(|e| e.into_inner()).len();
        if live > 0 {
            return Err(format!(
                "cannot restore while {live} panes are live. Restore must run before any pane starts."
            ));
        }
        let path = self.cfg.data_dir.join("layout.json");
        if std::fs::symlink_metadata(&path).is_err_and(|e| e.kind() == std::io::ErrorKind::NotFound)
        {
            return Ok(());
        }
        if let Err(e) = self.tree.load(&path) {
            return Err(format!(
                "cannot read {}: {e}. Move it aside and start again to get a fresh workshop.",
                path.display()
            ));
        }
        let mut rep = RestoreReport::default();
        for rec in self.tree.panes() {
            rep.panes += 1;
            if rec.closed {
                rep.already_closed += 1;
                if rec.ended_at == 0 {
                    let _ = self.tree.close_pane(&rec.id, rec.exit_code, unix_secs());
                }
                continue;
            }
            if rec.kind == layout::KIND_HEADLESS && !rec.harness_session_id.is_empty() {
                if crate::adapters::get(&rec.harness).is_none() {
                    self.mark_restored(
                        &rec,
                        state::STATE_UNKNOWN,
                        &format!("no adapter named {}", rec.harness),
                    );
                    rep.marked_unknown += 1;
                    rep.notes.push(format!(
                        "{} used harness {}, which this build has no adapter for.",
                        rec.id,
                        sys::go_quote(&rec.harness)
                    ));
                    continue;
                }
                if let Err(e) = self.start_pane(&rec) {
                    self.mark_restored(
                        &rec,
                        state::STATE_UNKNOWN,
                        &format!("resume failed: {}", e.msg),
                    );
                    rep.marked_unknown += 1;
                    rep.notes.push(format!(
                        "{} did not resume: {}. Create a new pane in {}.",
                        rec.id, e.msg, rec.cwd
                    ));
                    continue;
                }
                rep.resumed += 1;
                if rec.fork_pending {
                    rep.notes.push(format!(
                        "{} asked its adapter to fork harness session {} again, because the fork had not reported its own session id.",
                        rec.id, rec.harness_session_id
                    ));
                } else {
                    rep.notes.push(format!(
                        "{} asked its adapter to resume harness session {}.",
                        rec.id, rec.harness_session_id
                    ));
                }
            } else if rec.kind == layout::KIND_PTY {
                self.mark_restored(
                    &rec,
                    state::STATE_DONE,
                    "the process did not survive the restart",
                );
                rep.marked_done += 1;
                rep.notes.push(format!(
                    "{} ran {}. Its process is gone. Create a new pane in {}.",
                    rec.id,
                    go_list(rec.argv.as_deref().unwrap_or_default()),
                    rec.cwd
                ));
            } else {
                self.mark_restored(
                    &rec,
                    state::STATE_UNKNOWN,
                    "no harness session id was recorded",
                );
                rep.marked_unknown += 1;
                rep.notes.push(format!(
                    "{} had no recorded harness session, so its outcome is unknown.",
                    rec.id
                ));
            }
        }
        *self.restore.lock().unwrap_or_else(|e| e.into_inner()) = rep;
        self.save_layout();
        self.sweep_ended();
        Ok(())
    }

    /// Closes the record and applies the restored state from the process
    /// source, through the same entry point every other source uses.
    fn mark_restored(&self, rec: &layout::Pane, st: &str, detail: &str) {
        let _ = self.tree.close_pane(&rec.id, rec.exit_code, unix_secs());
        self.tree.drop_foreman(&rec.id);
        let harness = if rec.harness.is_empty() {
            "shell".to_string()
        } else {
            rec.harness.clone()
        };
        self.apply_state(
            &rec.id,
            Event {
                v: 1,
                ts: Server::now_seconds(),
                session_id: rec.id.clone(),
                harness,
                pane: Some(rec.id.clone()),
                state: st.into(),
                source: state::SRC_PROCESS.into(),
                detail: super::agents::cut(detail, state::DETAIL_MAX),
                ..Default::default()
            },
        );
    }
}
