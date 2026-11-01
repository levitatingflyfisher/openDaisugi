//! A worker can ask for help (the help tool of sprig). need_info and
//! escalate end its run with a question: the worker then holds no process
//! and no model call. The question is one work message. The steward answers
//! with its reply tool, and the worker goes on as a new segment of the same
//! session, with the answer as its next message. A question not answered
//! within the project's help_wait_s ends the worker as given up, with the
//! question in its report. give_up ends it at once. Every call is
//! journaled with the worker label and the kind. The twin of Go's
//! steward_help.go.

use std::collections::BTreeSet;
use std::path::Path;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use super::steward_chat::StewardChat;
use super::steward_work::{
    envelope_dir, read_project_envelope, ProjectEnvelope, WorkState, WorkerRec, WORK_GAVE_UP,
    WORK_RUNNING, WORK_WAITING,
};
use super::Server;
use crate::state::Verdict;
use crate::steward::chat as fchat;
use crate::steward::ctx::Ctx;

/// The tool of a worker that can ask.
pub(super) const HELP_TOOL_NAME: &str = "help";

/// How long a question waits for its answer when the project's envelope
/// file names no help_wait_s.
pub const DEFAULT_HELP_WAIT_S: u64 = 3600;

/// The question a waiting worker put. The deadline is on the server's clock.
#[derive(Clone, Debug, Default, serde::Deserialize)]
#[serde(default)]
pub struct HelpAsk {
    pub kind: String,
    pub text: String,
    pub deadline: String,
}

impl Server {
    /// How long a question of this project waits.
    fn help_wait(&self, pe: Option<&ProjectEnvelope>) -> Duration {
        if let Some(d) = self.work_lock().help_wait_for {
            return d;
        }
        match pe {
            Some(p) if p.help_wait_s > 0 => Duration::from_secs(p.help_wait_s as u64),
            _ => Duration::from_secs(DEFAULT_HELP_WAIT_S),
        }
    }

    /// Handles a segment that ended by help.
    #[allow(clippy::too_many_arguments)]
    pub(super) fn worker_help(
        self: &Arc<Server>,
        ctx: &Ctx,
        fc: &Arc<StewardChat>,
        w: &WorkerRec,
        pe: &ProjectEnvelope,
        session: &str,
        kind: &str,
        text: &str,
    ) {
        if ctx.err().is_some() {
            return;
        }
        let tag = format!("[{}]", w.label);
        let text = text.trim();
        if !matches!(kind, "need_info" | "escalate" | "give_up") {
            let shown = super::messages::one_line(kind, 40);
            self.finish_worker(
                ctx,
                fc,
                &w.label,
                super::steward_work::WORK_FAILED,
                session,
                &format!("{tag} failed: the worker's help had the kind {shown}"),
            );
            return;
        }
        self.note_verdict(
            &w.label,
            &Verdict {
                decision: "help".into(),
                tool: "help".into(),
                clause: kind.into(),
            },
            "",
        );
        if kind == "give_up" {
            let report = format!(
                "{tag} gave up\n{}\n{}",
                quoted(text),
                self.changes_line(fc, &w.label, session)
            );
            self.finish_worker(ctx, fc, &w.label, WORK_GAVE_UP, session, &report);
            return;
        }
        let wait = self.help_wait(Some(pe));
        let deadline = fchat::date_text(self.fm_now() + wait);
        {
            let mut ws = self.work_lock();
            let file = self
                .worker_session_path(fc, session, w.fenced || self.table_fenced(&ws, &w.label))
                .to_string_lossy()
                .into_owned();
            if let Some(r) = ws.work.iter_mut().find(|r| r.label == w.label) {
                r.state = WORK_WAITING.into();
                r.session = session.into();
                r.session_file = file;
                r.ask = Some(HelpAsk {
                    kind: kind.into(),
                    text: text.into(),
                    deadline,
                });
            }
            self.arm_help_locked(&mut ws, fc, &w.label, wait);
            self.save_workers_locked(fc, &ws);
        }
        if kind == "escalate" {
            self.log_work(
                fc,
                &format!(
                    "{tag} asks the owner\n{}\n(This is for the owner. Tell the owner, then pass the answer on with reply.)",
                    quoted(text)
                ),
            );
            self.note(
                &format!(
                    "{} asks the owner: {}. Answer it in the steward's chat.",
                    w.label,
                    super::messages::one_line(text, 200)
                ),
                "",
            );
            return;
        }
        self.log_work(fc, &format!("{tag} asks a question\n{}\n(Answer it with reply.)", quoted(text)));
    }

    fn table_fenced(&self, ws: &WorkState, label: &str) -> bool {
        ws.work.iter().any(|r| r.label == label && r.fenced)
    }

    /// Starts the timer that ends a worker's wait. The caller holds the
    /// work lock. The timer is a thread that ends when the wait is over, the
    /// worker is answered (its generation changes) or the server closes.
    pub(super) fn arm_help_locked(
        self: &Arc<Server>,
        ws: &mut WorkState,
        fc: &Arc<StewardChat>,
        label: &str,
        d: Duration,
    ) {
        let gen = {
            let g = ws.help_gen.entry(label.to_string()).or_insert(0);
            *g += 1;
            *g
        };
        let ctx = ws.ctx.clone().unwrap_or_default();
        let (s, f, l) = (self.clone(), fc.clone(), label.to_string());
        thread::spawn(move || {
            let end = Instant::now() + d;
            loop {
                if ctx.err().is_some() {
                    return;
                }
                let left = end.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    break;
                }
                thread::sleep(left.min(Duration::from_millis(50)));
                if s.work_lock().help_gen.get(&l) != Some(&gen) {
                    return;
                }
            }
            s.expire_help(&f, &l, gen);
        });
    }

    /// The wait left of a waiting worker the table held at start, or the
    /// line to log when the time has passed: the worker is then given up.
    pub(super) fn rearm_help(&self, r: &mut WorkerRec) -> Result<Duration, String> {
        if let Some(a) = &r.ask {
            if let Some(at) = fchat::parse_date(&a.deadline) {
                if let Ok(rem) = at.duration_since(self.fm_now()) {
                    if !rem.is_zero() {
                        return Ok(rem);
                    }
                }
            }
        }
        let ask = r.ask.take();
        r.state = WORK_GAVE_UP.into();
        Err(match ask {
            None => format!(
                "[{}] gave up: its question was lost when coppice stopped",
                r.label
            ),
            Some(a) => format!(
                "[{}] gave up: no answer came in time\nquestion ({}):\n{}",
                r.label,
                a.kind,
                quoted(&a.text)
            ),
        })
    }

    /// Ends a waiting worker whose question went unanswered.
    fn expire_help(self: &Arc<Server>, fc: &Arc<StewardChat>, label: &str, gen: u64) {
        let (ask, session, ctx) = {
            let mut ws = self.work_lock();
            if ws.help_gen.get(label) != Some(&gen) {
                return;
            }
            let Some(r) = ws.work.iter_mut().find(|r| r.label == label) else {
                return;
            };
            if r.state != WORK_WAITING || r.ask.is_none() {
                return;
            }
            let ask = r.ask.take().unwrap_or_default();
            let session = r.session.clone();
            r.state = WORK_GAVE_UP.into();
            ws.help_gen.remove(label);
            let Some(ctx) = ws.ctx.clone() else { return };
            (ask, session, ctx)
        };
        let text = format!(
            "[{label}] gave up: no answer came in time\nquestion ({}):\n{}\n{}",
            ask.kind,
            quoted(&ask.text),
            self.changes_line(fc, label, &session)
        );
        self.finish_worker(&ctx, fc, label, WORK_GAVE_UP, &session, &text);
    }

    /// The steward's reply tool: it answers the question of a waiting
    /// worker, and the worker goes on with the answer as its next message.
    pub fn tool_reply(
        self: &Arc<Server>,
        fc: &Arc<StewardChat>,
        label: &str,
        answer: &str,
    ) -> Result<String, String> {
        if answer.trim().is_empty() {
            return Err("the answer is empty".into());
        }
        self.load_workers(fc);
        let project = {
            let ws = self.work_lock();
            let Some(r) = ws.work.iter().find(|r| r.label == label) else {
                return Err(format!(
                    "no worker named {}. status lists them",
                    crate::sys::go_quote(label)
                ));
            };
            if r.state != WORK_WAITING {
                return Err(format!(
                    "{label} is not waiting for an answer (it is {})",
                    r.state
                ));
            }
            r.project.clone()
        };
        let pe = read_project_envelope(
            &envelope_dir().join(format!("{project}.json")),
            &project,
        )?;
        let (rec, session, ctx) = {
            let mut ws = self.work_lock();
            if self.is_closed() {
                return Err("the server is closing".into());
            }
            let ctx = ws.ctx.clone().unwrap_or_default();
            let Some(i) = ws.work.iter().position(|r| r.label == label) else {
                return Err(format!(
                    "no worker named {}. status lists them",
                    crate::sys::go_quote(label)
                ));
            };
            if ws.work[i].state != WORK_WAITING {
                return Err(format!(
                    "{label} is not waiting for an answer (it is {})",
                    ws.work[i].state
                ));
            }
            ws.work[i].state = WORK_RUNNING.into();
            ws.work[i].ask = None;
            ws.help_gen.remove(label);
            let rec = ws.work[i].clone();
            let session = rec.session.clone();
            self.save_workers_locked(fc, &ws);
            self.work_wg.add();
            (rec, session, ctx)
        };
        let (s, f, answer) = (self.clone(), fc.clone(), answer.to_string());
        thread::spawn(move || {
            s.run_worker_step(&ctx, &f, rec, pe, &answer, "", &session);
            s.work_wg.done();
        });
        Ok(format!("answered {label}; it goes on"))
    }

    /// The line a given-up report ends with: the files the worker wrote or
    /// edited, from its session file, sorted.
    fn changes_line(&self, fc: &StewardChat, label: &str, session: &str) -> String {
        if session.is_empty() {
            return "changed: nothing".into();
        }
        let fenced = self.table_fenced(&self.work_lock(), label);
        changed_files(&self.worker_session_path(fc, session, fenced))
    }
}

/// Reads a sprig session file and lists the paths of its write and edit
/// calls.
pub fn changed_files(session_file: &Path) -> String {
    let Ok(body) = std::fs::read(session_file) else {
        return "changed: unknown (no session file)".into();
    };
    let mut seen = BTreeSet::new();
    for line in body.split(|&b| b == b'\n') {
        let Ok(v) = serde_json::from_slice::<serde_json::Value>(line) else {
            continue;
        };
        let name = v.get("name").and_then(|x| x.as_str()).unwrap_or("");
        if v.get("type").and_then(|x| x.as_str()) != Some("tool_call")
            || !(name == "write" || name == "edit")
        {
            continue;
        }
        if let Some(p) = v.pointer("/input/path").and_then(|x| x.as_str()) {
            if !p.is_empty() {
                seen.insert(p.to_string());
            }
        }
    }
    if seen.is_empty() {
        return "changed: nothing".into();
    }
    format!("changed: {}", seen.into_iter().collect::<Vec<_>>().join(", "))
}


/// A worker's own text with "> " before each line, so no line of it can
/// pass for a line coppice writes, such as another worker's "[label] asks a
/// question".
fn quoted(text: &str) -> String {
    text.replace("\r\n", "\n")
        .replace('\r', "\n")
        .split('\n')
        .map(|l| format!("> {l}"))
        .collect::<Vec<_>>()
        .join("\n")
}
