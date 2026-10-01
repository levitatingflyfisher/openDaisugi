//! Import of old Claude Code and Codex chats (FM-R-71 to FM-R-77), the
//! server's part: floor.import, the offer, and the feed in the compactor
//! loop. The twin of Go's server/foreman_import.go.
//!
//! The owner chooses the import, with a click or with `coppice foreman
//! import`; the server never starts it. The compactor loop feeds the queue
//! one message at a time, after the live nodes and never while a turn runs.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde_json::json;

use super::foreman_chat::{FlightGuard, ForemanChat};
use super::{Client, Server};
use crate::foreman::chat as fchat;
use crate::foreman::ctx::Ctx;
use crate::foreman::import::{self, ImportOpts};
use crate::proto::{self, Request, Response};

/// What a pane or a plugin gets for floor.import. A pane must never
/// trigger an import of the owner's chat history.
pub const IMPORT_REFUSAL: &str = "only the operator imports old chats into the foreman.";

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "floor.import",
        Arc::new(|s, c, r| s.handle_floor_import(c, r)),
    );
}

impl ForemanChat {
    /// True when the owner chose to import and the queue holds work that is
    /// not paused.
    pub(super) fn import_wanted(&self) -> bool {
        import::read_import_state(&self.dir)
            .is_ok_and(|st| st.decision == "accepted" && !st.paused && !st.queue.is_empty())
    }
}

impl Server {
    /// The home dir whose transcripts import reads.
    pub(super) fn import_home_dir(&self) -> PathBuf {
        let h = &self.cfg.foreman.import_home;
        if !h.is_empty() {
            return PathBuf::from(h);
        }
        std::env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or_default()
    }

    /// The dirs whose sessions import never takes: coppice's own (FM-R-78).
    pub(super) fn import_skip(&self) -> Vec<String> {
        vec![
            Server::foreman_dir(),
            self.foreman_log_dir().to_string_lossy().into_owned(),
            self.cfg.data_dir.to_string_lossy().into_owned(),
        ]
    }

    /// The scan options the state holds, with the skip dirs.
    fn import_opts(&self, st: &import::ImportState) -> ImportOpts {
        let mut o = st.opts();
        o.skip = self.import_skip();
        o
    }

    /// True when no turn runs and none waits for its turn.
    pub(super) fn turns_idle(&self) -> bool {
        let t = self.turns_lock();
        !t.running && t.queue.is_empty()
    }

    /// Logs one queued message when the chat is drained and no turn runs. It
    /// reports whether it logged one, so the loop drains again.
    pub(super) fn import_feed(self: &Arc<Server>, fc: &Arc<ForemanChat>) -> bool {
        if !fc.import_wanted() {
            return false;
        }
        let res = {
            let mut g = fc.imp.lock().unwrap_or_else(|e| e.into_inner());
            let Some(imp) = g.as_mut() else { return false };
            imp.feed(&fc.chat, &|| self.turns_idle())
        };
        match res {
            Err(e) => {
                if fc.cancel.err().is_none() {
                    self.note_once(&format!(
                        "The foreman could not import a past message: {e}. Nothing is lost. It tries again."
                    ));
                }
                false
            }
            Ok(fed) => {
                if fed {
                    // The floor reads progress when the chat changes. A
                    // message every 2 s is enough; past messages are not in
                    // the floor's chat.
                    let due = {
                        let mut g = fc.imp_event.lock().unwrap_or_else(|e| e.into_inner());
                        let due = g.is_none_or(|t| t.elapsed() > Duration::from_secs(2));
                        if due {
                            *g = Some(Instant::now());
                        }
                        due
                    };
                    if due {
                        self.send_messages_event("", true);
                    }
                }
                fed
            }
        }
    }

    /// Says whether the caller runs the scan: Some(gen) when it must (and
    /// is now the one scan in flight), None when one runs already
    /// (FM-R-83).
    pub(super) fn est_begin(&self, fc: &ForemanChat) -> Option<u64> {
        let mut fl = fc.est_flight.lock().unwrap_or_else(|e| e.into_inner());
        if fl.running {
            return None;
        }
        fl.running = true;
        fl.scans += 1;
        Some(fl.gen)
    }

    /// Runs the scan a started caller owns. A scan that began before an
    /// accept does not set the estimate. The flight is freed even when the
    /// scan panics (F-2).
    pub(super) fn est_run(
        &self,
        fc: &ForemanChat,
        ctx: &Ctx,
        gen: u64,
    ) -> Vec<import::ImportSession> {
        let _guard = FlightGuard(fc);
        let hold = fc.scan_hold.lock().unwrap_or_else(|e| e.into_inner()).clone();
        if let Some(h) = hold {
            h();
        }
        let st = import::read_import_state(&fc.dir).unwrap_or_default();
        let home = self.import_home_dir();
        let sig = import::transcript_sig(&home);
        let ss = import::scan(ctx, &home, &self.import_opts(&st), &st, None);
        {
            let mut fl = fc.est_flight.lock().unwrap_or_else(|e| e.into_inner());
            if ctx.err().is_none() && gen == fl.gen {
                *fc.est.lock().unwrap_or_else(|e| e.into_inner()) = (true, ss.clone());
                fl.sig = sig;
                fl.at = Some(Instant::now());
            }
        }
        if ctx.err().is_some() {
            return Vec::new();
        }
        ss
    }

    /// Whether the cached estimate may stand for an accept: it exists, is
    /// under a minute old, and the transcript files are as they were when it
    /// was scanned (F-1).
    fn estimate_fresh(&self, fc: &ForemanChat) -> bool {
        let (sig, at) = {
            let fl = fc.est_flight.lock().unwrap_or_else(|e| e.into_inner());
            (fl.sig.clone(), fl.at)
        };
        let ok = fc.est.lock().unwrap_or_else(|e| e.into_inner()).0;
        ok && at.is_some_and(|t| t.elapsed() < ESTIMATE_MAX_AGE)
            && import::transcript_sig(&self.import_home_dir()) == sig
    }

    /// What could be imported now, scanned once and kept. The scan reads
    /// every transcript, so it runs beside the chat and never under its
    /// lock, and only one runs at a time: a caller that finds one running
    /// waits for it and shares what it found.
    pub(super) fn import_estimate(
        &self,
        fc: &ForemanChat,
        ctx: &Ctx,
        force: bool,
    ) -> Vec<import::ImportSession> {
        loop {
            {
                let g = fc.est.lock().unwrap_or_else(|e| e.into_inner());
                if g.0 && !force {
                    return g.1.clone();
                }
            }
            if let Some(gen) = self.est_begin(fc) {
                return self.est_run(fc, ctx, gen);
            }
            if !self.est_wait(fc, ctx) {
                return Vec::new();
            }
            let g = fc.est.lock().unwrap_or_else(|e| e.into_inner());
            if g.0 {
                return g.1.clone();
            }
            // The scan we joined began before an accept and set no
            // estimate: what it found is old. Ask again.
        }
    }

    /// Waits until no scan runs. False when ctx ended first.
    fn est_wait(&self, fc: &ForemanChat, ctx: &Ctx) -> bool {
        let mut fl = fc.est_flight.lock().unwrap_or_else(|e| e.into_inner());
        while fl.running {
            if ctx.err().is_some() {
                return false;
            }
            fl = fc
                .est_cv
                .wait_timeout(fl, Duration::from_millis(50))
                .unwrap_or_else(|e| e.into_inner())
                .0;
        }
        true
    }

    /// Scans with options of the caller's own, in the one flight: it waits
    /// for a running scan, then holds the flight, so two full parses never
    /// run at once. It sets no estimate (the options are not the state's).
    pub(super) fn scan_with(&self, fc: &ForemanChat, ctx: &Ctx, o: &ImportOpts) -> Vec<import::ImportSession> {
        loop {
            if self.est_begin(fc).is_some() {
                let _guard = FlightGuard(fc);
                let hold = fc.scan_hold.lock().unwrap_or_else(|e| e.into_inner()).clone();
                if let Some(h) = hold {
                    h();
                }
                let st = import::read_import_state(&fc.dir).unwrap_or_default();
                let ss = import::scan(ctx, &self.import_home_dir(), o, &st, None);
                // A cancelled scan gives nothing (F-4).
                return if ctx.err().is_some() { Vec::new() } else { ss };
            }
            if !self.est_wait(fc, ctx) {
                return Vec::new();
            }
        }
    }

    /// Puts the offer in the chat once, when old chats exist and the owner
    /// has not chosen yet. It runs after the chat opens, off the chat lock.
    pub(super) fn offer_import(self: &Arc<Server>, fc: &Arc<ForemanChat>) {
        let st = import::read_import_state(&fc.dir).unwrap_or_default();
        if st.offered || !st.decision.is_empty() {
            return;
        }
        let ss = self.import_estimate(fc, &fc.cancel, true);
        let (all, _) = import::totals(&ss);
        if all.sessions == 0 {
            return;
        }
        let mut first = false;
        let res = import::update_import_state(&fc.dir, |st| {
            first = !st.offered && st.decision.is_empty();
            st.offered = true;
            Ok(())
        });
        if res.is_err() || !first {
            return;
        }
        let text = format!(
            "I can bring in {} (about {}) from Claude Code and Codex. Press Import on the floor, or run: coppice foreman import. Nothing is read into the chat until you do. Imported messages are summarized by the foreman's model like any chat message.",
            import::plural(all.sessions as i64, "past chat"),
            import::plural(all.messages as i64, "message"),
        );
        let _ = self.chat_append(fc, fchat::KIND_NOTE, &text);
    }

    fn import_status(self: &Arc<Server>, fc: &Arc<ForemanChat>) -> serde_json::Value {
        let st = import::read_import_state(&fc.dir).unwrap_or_default();
        let s = import::status_of(&st);
        let (ok, ss) = {
            let g = fc.est.lock().unwrap_or_else(|e| e.into_inner());
            (g.0, g.1.clone())
        };
        let (mut sessions, mut messages, mut estimating) = (0usize, 0usize, false);
        if st.decision == "declined" {
            // The owner said no: no transcript is read for the floor's sake.
        } else if !ok {
            estimating = true;
            // Reading every transcript takes a while: start it, answer now.
            // One scan runs at a time; a status call that finds one running
            // only says estimating.
            if fc.cancel.err().is_none() {
                if let Some(gen) = self.est_begin(fc) {
                    fc.bg.add();
                    let (srv, f) = (self.clone(), fc.clone());
                    std::thread::spawn(move || {
                        srv.est_run(&f, &f.cancel, gen);
                        srv.send_messages_event("", true);
                        f.bg.done();
                    });
                }
            }
        } else {
            let (all, _) = import::totals(&ss);
            sessions = all.sessions;
            messages = all.messages;
        }
        json!({
            "state": s.state, "offered": s.offered, "paused": s.paused,
            "done": s.done, "total": s.total, "queued": s.queued,
            "sessions": sessions, "messages": messages, "estimating": estimating,
        })
    }

    /// floor.import: status, accept, decline, pause or resume. accept takes
    /// optional from, since and tools like the command.
    fn handle_floor_import(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        self.floor_import(r)
    }

    /// floor.import without the connection, for the tests.
    pub(super) fn floor_import(self: &Arc<Server>, r: &Request) -> Response {
        if !self.chat_mode() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "import needs the foreman in chat mode: set mode = \"chat\" in [foreman].",
            );
        }
        let fc = self.ensure_chat();
        if !fc.has_chat() {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &fc.problem);
        }
        let action = r.s("action");
        let mut nothing = false;
        let res: Result<(), String> = match action.as_str() {
            "" | "status" => Ok(()),
            "progress" => {
                // The state alone, with no scan: the TUI's bar asks this often.
                let st = import::read_import_state(&fc.dir).unwrap_or_default();
                let s = import::status_of(&st);
                return proto::ok_resp(
                    &r.id,
                    json!({
                        "state": s.state, "offered": s.offered, "paused": s.paused,
                        "done": s.done, "total": s.total, "queued": s.queued,
                        "sessions": 0, "messages": 0, "estimating": false,
                    }),
                );
            }
            "accept" => {
                let mut from = r.s("from");
                if from.is_empty() {
                    from = "all".into();
                }
                if from != "claude" && from != "codex" && from != "all" {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_BAD_REQUEST,
                        "floor.import from must be claude, codex or all.",
                    );
                }
                let mut o = ImportOpts {
                    from,
                    since: None,
                    tools: r.bool("tools").unwrap_or(false),
                    skip: self.import_skip(),
                };
                let since = r.s("since");
                if !since.is_empty() {
                    match import::parse_day(&since) {
                        Some(t) => o.since = Some(t),
                        None => {
                            return proto::err_resp(
                                &r.id,
                                proto::ERR_BAD_REQUEST,
                                "floor.import since must be a date like 2026-03-01.",
                            )
                        }
                    }
                }
                let st = import::read_import_state(&fc.dir).unwrap_or_default();
                // The Import button shares the single-flight scan (IM-R-12);
                // options that differ from the estimate's scan on their own,
                // still one at a time.
                let ss = if same_scan(&o, &self.import_opts(&st)) {
                    // The cache serves only while fresh; else this joins or
                    // starts a scan (F-1).
                    self.import_estimate(&fc, &fc.cancel, !self.estimate_fresh(&fc))
                } else {
                    self.scan_with(&fc, &fc.cancel, &o)
                };
                if fc.cancel.err().is_some() {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_INTERNAL,
                        "the import was cancelled: coppice is stopping. Nothing was queued.",
                    );
                }
                nothing = ss.is_empty();
                let res = if ss.is_empty() {
                    Ok(())
                } else {
                    import::accept(&fc.dir, &o, &ss)
                };
                {
                    let mut fl = fc.est_flight.lock().unwrap_or_else(|e| e.into_inner());
                    fl.gen += 1;
                    fc.est.lock().unwrap_or_else(|e| e.into_inner()).0 = false;
                }
                res
            }
            "decline" => import::decline(&fc.dir),
            "pause" => import::set_paused(&fc.dir, true),
            "resume" => import::set_paused(&fc.dir, false),
            _ => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    "floor.import action must be status, progress, accept, decline, pause or resume.",
                )
            }
        };
        if let Err(e) = res {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!("cannot write the import state in {}: {e}", fc.dir.display()),
            );
        }
        if action == "accept" || action == "resume" {
            fc.wake();
        }
        let mut rep = self.import_status(&fc);
        if nothing {
            rep["note"] = json!("nothing new to import");
        }
        proto::ok_resp(&r.id, rep)
    }
}

/// How long a cached estimate may serve an accept.
const ESTIMATE_MAX_AGE: Duration = Duration::from_secs(60);

/// Whether o scans what the shared estimate scans.
fn same_scan(o: &ImportOpts, base: &ImportOpts) -> bool {
    let norm = |f: &str| if f.is_empty() { "all".to_string() } else { f.to_string() };
    norm(&o.from) == norm(&base.from) && o.tools == base.tools && o.since == base.since
}
