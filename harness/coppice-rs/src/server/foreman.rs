//! floor.talk and floor.foreman: the owner's words on their way to the
//! floor's foreman. With no foreman running, a talk starts one first: the
//! default harness, in the foreman's own directory, labelled foreman. A
//! new foreman gets its page, then the words, in order, one thread at a
//! time.

use std::collections::VecDeque;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::json;

use super::ready::PASTE_GAP;
use super::{Client, Server};
use crate::config;
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::state::{self, Event};

/// The label of the floor's foreman.
pub const DEFAULT_FOREMAN_LABEL: &str = "foreman";
/// How long the talk queue waits on the foreman before it posts a note.
pub const DEFAULT_FOREMAN_WAIT: Duration = Duration::from_secs(30);
/// How often the talk queue looks at the foreman's state.
const TALK_POLL: Duration = Duration::from_millis(100);
/// How many sentences may wait for the foreman.
const MAX_TALK_QUEUE: usize = 32;
/// The data dir file that names the pane the server started as foreman.
const FOREMAN_FILE: &str = "foreman.json";

/// The page that makes a pane the foreman of the floor.
pub const FOREMAN_PAGE: &str = include_str!("../../../coppice/skills/foreman/SKILL.md");

/// The line before the page when it is pasted into the floor's foreman.
const FLOOR_HEADER: &str =
    "You are the foreman of this floor. The operator talks to you from the floor, \
and each sentence they say comes to you as a prompt. Your working directory is your own \
scratch space under the state directory, outside the floor's own files and outside every \
project. Here is your page.";

/// What a foreman gets when the page cannot go in as one paste.
const FLOOR_LINE: &str =
    "Run: coppice skill foreman. Follow it. You are the foreman of this floor. Say ready.";

/// Why the talk thread stopped.
#[derive(Debug, PartialEq)]
enum TalkErr {
    /// The foreman's process ended or its record went away.
    Gone,
    /// The server closes.
    Stopped,
}

/// The owner's words on their way to the foreman, under one lock.
#[derive(Default)]
pub struct TalkQueue {
    /// The foreman the words go to, or "".
    pane: String,
    /// True once pane has its page.
    paged: bool,
    lines: VecDeque<String>,
    running: bool,
    /// The pane the server started as foreman, and whether the data dir
    /// was read for it.
    foreman: String,
    loaded: bool,
}

/// text as one line of printable words.
pub(super) fn talk_text(text: &str) -> String {
    let mapped: String = text
        .chars()
        .filter_map(|r| match r {
            '\t' | '\n' | '\r' | '\u{b}' | '\u{c}' => Some(' '),
            r if (r as u32) < 0x20 || r as u32 == 0x7f => None,
            r => Some(r),
        })
        .collect();
    mapped.split_whitespace().collect::<Vec<_>>().join(" ")
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle("floor.talk", Arc::new(|s, c, r| s.handle_floor_talk(c, r)));
    let _ = s.handle(
        "floor.foreman",
        Arc::new(|s, c, r| s.handle_floor_foreman(c, r)),
    );
}

impl Server {
    pub fn talk_lock(&self) -> std::sync::MutexGuard<'_, TalkQueue> {
        self.talk.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The pane the server started as foreman, read once from the data
    /// dir, or "". The caller holds the talk lock.
    pub fn tracked_foreman(&self, t: &mut TalkQueue) -> String {
        if !t.loaded {
            t.loaded = true;
            let v = std::fs::read(self.cfg.data_dir.join(FOREMAN_FILE))
                .ok()
                .and_then(|b| serde_json::from_slice::<serde_json::Value>(&b).ok());
            if let Some(serde_json::Value::Object(m)) = v {
                if let Some(serde_json::Value::String(p)) = m.get("pane") {
                    t.foreman = p.clone();
                }
            }
        }
        t.foreman.clone()
    }

    /// Records id as the floor's foreman, in memory and in the data dir.
    fn set_foreman_pane(&self, t: &mut TalkQueue, id: &str) {
        t.foreman = id.to_string();
        t.loaded = true;
        let body = gojson::marshal(&gojson::map(vec![("pane", json!(id))]));
        let path = self.cfg.data_dir.join(FOREMAN_FILE);
        let tmp = self.cfg.data_dir.join(format!("{FOREMAN_FILE}.tmp"));
        let mut ok = true;
        let ran = self.write_data(|| {
            use std::os::unix::fs::OpenOptionsExt;
            ok = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)
                .and_then(|mut f| std::io::Write::write_all(&mut f, body.as_bytes()))
                .and_then(|_| std::fs::rename(&tmp, &path))
                .is_ok();
        });
        if ran && !ok {
            self.note(
                &format!(
                    "cannot write {}. The foreman is known until the server stops.",
                    path.display()
                ),
                id,
            );
        }
    }

    /// Makes new_id the floor's foreman when it resumes the tracked
    /// foreman's ended record old, so the next talk goes to it and no
    /// second foreman starts. A fresh start gets its page now. The caller
    /// holds the talk lock.
    pub(super) fn follow_foreman_locked(
        self: &Arc<Server>,
        t: &mut TalkQueue,
        old: &str,
        new_id: &str,
        resumed: bool,
    ) {
        if old.is_empty() || !t.loaded || t.foreman != old {
            return;
        }
        self.set_foreman_pane(t, new_id);
        t.pane = new_id.to_string();
        t.paged = resumed;
        if !resumed {
            self.run_talk_locked(t);
        }
    }

    /// Whether pane id is a live pane, and whether it is an ended one.
    pub fn foreman_state(&self, id: &str) -> (bool, bool) {
        if id.is_empty() {
            return (false, false);
        }
        match self.tree.pane(id) {
            Some(p) => (!p.closed, p.closed),
            None => (false, false),
        }
    }

    fn handle_floor_talk(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let text = talk_text(&r.s("text"));
        if text.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "floor.talk needs text: the words for the foreman.",
            );
        }
        if self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot take words for the foreman.",
            );
        }
        let mut t = self.talk_lock();
        if t.lines.len() >= MAX_TALK_QUEUE {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "the foreman has {} sentences waiting already. Look at its window, then talk again.",
                    t.lines.len()
                ),
            );
        }
        let mut id = self.tracked_foreman(&mut t);
        let (live, ended) = self.foreman_state(&id);
        let (mut started, mut note) = (false, String::new());
        if !live {
            id = match self.start_foreman(&mut t, &r.id, "") {
                Ok(id) => id,
                Err(resp) => return resp,
            };
            started = true;
            if ended {
                note = "The foreman had ended. A new one starts, and your words go to it.".into();
            }
        } else {
            if id != t.pane {
                t.pane = id.clone();
                t.paged = true;
            }
            if self
                .effective_current(&id)
                .is_some_and(|ev| ev.state == state::STATE_BLOCKED)
            {
                note = "The foreman waits on a question. Answer it in its window, and your words go after.".into();
            }
        }
        // The owner's words go to the chat file before they are typed, so
        // they show while the foreman starts and outlive a foreman that ends.
        self.write_chat("user", &id, &text);
        t.lines.push_back(text);
        let queued = t.lines.len();
        self.run_talk_locked(&mut t);
        if !note.is_empty() {
            self.note(&note, &id);
        }
        let mut out = vec![
            ("pane", json!(id)),
            ("label", json!(self.label_of(&id))),
            ("started", json!(started)),
            ("queued", json!(queued)),
        ];
        if !note.is_empty() {
            out.push(("note", json!(note)));
        }
        proto::ok_resp(&r.id, gojson::map(out))
    }

    fn handle_floor_foreman(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        if self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a foreman.",
            );
        }
        let harness = r.s("harness");
        let mut t = self.talk_lock();
        let id = self.tracked_foreman(&mut t);
        if !id.is_empty() && self.foreman_state(&id).0 {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "the foreman {} ({id}) runs already. Close it first, or talk to it.",
                    self.label_of(&id)
                ),
            );
        }
        let id = match self.start_foreman(&mut t, &r.id, &harness) {
            Ok(id) => id,
            Err(resp) => return resp,
        };
        self.run_talk_locked(&mut t);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("pane", json!(id)),
                ("label", json!(self.label_of(&id))),
                ("started", json!(true)),
            ]),
        )
    }

    /// Starts the talk thread unless it runs. The caller holds the lock.
    fn run_talk_locked(self: &Arc<Server>, t: &mut TalkQueue) {
        if !t.running {
            t.running = true;
            let s = self.clone();
            thread::spawn(move || s.run_talk());
        }
    }

    /// Starts the floor's foreman with harness, or the default harness,
    /// records it, and points the queue at it. The caller holds the lock.
    fn start_foreman(
        self: &Arc<Server>,
        t: &mut TalkQueue,
        req_id: &str,
        harness: &str,
    ) -> Result<String, Response> {
        let mut harness = harness.to_string();
        if harness.is_empty() {
            match config::load() {
                Ok(Some(c)) if !c.default.is_empty() => harness = c.default,
                _ => {
                    return Err(proto::err_resp(
                        req_id,
                        proto::ERR_BAD_REQUEST,
                        &format!(
                            "no default harness in {}, so no foreman can start. Run coppice open HARNESS once to set one.",
                            config::path().display()
                        ),
                    ))
                }
            }
        }
        let dir = Server::foreman_dir();
        let mut made = Ok(());
        if !self.write_data(|| {
            use std::os::unix::fs::DirBuilderExt;
            made = std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(&dir);
        }) {
            return Err(proto::err_resp(
                req_id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a foreman.",
            ));
        }
        if let Err(e) = made {
            return Err(proto::err_resp(
                req_id,
                proto::ERR_INTERNAL,
                &format!(
                    "cannot make the foreman's directory {dir}: {}",
                    crate::sys::go_path_err("mkdir", std::path::Path::new(&dir), &e)
                ),
            ));
        }
        let line = gojson::marshal(&gojson::map(vec![
            ("id", json!(req_id)),
            ("cmd", json!("pane.create")),
            ("kind", json!("pty")),
            ("harness", json!(harness)),
            ("cwd", json!(dir)),
            ("label", json!(DEFAULT_FOREMAN_LABEL)),
        ]));
        let (req, _, _) = proto::decode_line(&line);
        let mut resp = self.pane_create(None, &req);
        if !resp.ok {
            resp.id = req_id.to_string();
            return Err(resp);
        }
        let id = resp
            .result
            .as_ref()
            .and_then(|v| v.get("pane"))
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        if !self.tracked_foreman(t).is_empty() {
            self.write_chat("note", &id, super::messages::NEW_FOREMAN_NOTE);
        }
        self.set_foreman_pane(t, &id);
        t.pane = id.clone();
        t.paged = false;
        Ok(id)
    }

    /// Types the queued words into the foreman, one at a time, until the
    /// queue is empty. A new foreman first gets its page.
    fn run_talk(self: Arc<Server>) {
        loop {
            let (id, paged) = {
                let mut t = self.talk_lock();
                if t.lines.is_empty() && (t.paged || t.pane.is_empty()) {
                    t.running = false;
                    return;
                }
                (t.pane.clone(), t.paged)
            };
            let err;
            if !paged {
                match self.page_foreman(&id) {
                    Ok(()) => {
                        let mut t = self.talk_lock();
                        if t.pane == id {
                            t.paged = true;
                        }
                        continue;
                    }
                    Err(e) => err = e,
                }
            } else {
                let q = format!(
                    "{} waits on a question. Answer it in its window, and your words go after.",
                    self.label_of(&id)
                );
                match self.settle(&id, |ev| ev.state != state::STATE_BLOCKED, false, &q) {
                    Ok(()) => {
                        let line = {
                            let mut t = self.talk_lock();
                            if t.pane != id || t.lines.is_empty() {
                                continue;
                            }
                            t.lines.pop_front().unwrap_or_default()
                        };
                        if self.type_into(&id, &line).is_ok() {
                            self.mark_input(&id, true);
                            continue;
                        }
                        err = TalkErr::Gone;
                    }
                    Err(e) => err = e,
                }
            }
            let lost = {
                let mut t = self.talk_lock();
                if err == TalkErr::Stopped {
                    t.running = false;
                    return;
                }
                if t.pane != id {
                    continue;
                }
                let lost = t.lines.len();
                t.pane.clear();
                t.paged = false;
                t.lines.clear();
                t.running = false;
                lost
            };
            let label = self.label_of(&id);
            if !paged {
                self.note(&format!("{label} ended before it was ready, so your words were not sent. Its last screen is in Recent. Talk again to start a new one."), &id);
            } else if lost > 0 {
                self.note(
                    &format!("{label} ended, so your last words were not sent. Talk again to start a new one."),
                    &id,
                );
            }
            return;
        }
    }

    /// Gives a new foreman its page once it drew its screen and went
    /// idle, then waits for the turn the page starts to end.
    fn page_foreman(&self, id: &str) -> Result<(), TalkErr> {
        let not_ready = format!(
            "{} is not ready yet. If it asks something, answer it in its window. Your words wait.",
            self.label_of(id)
        );
        let idle = |ev: &Event| ev.state == state::STATE_IDLE;
        self.settle(id, idle, true, &not_ready)?;
        if self.send_page(id).is_err() {
            return Err(TalkErr::Gone);
        }
        let end = Instant::now() + DEFAULT_FOREMAN_WAIT;
        while Instant::now() < end {
            if self
                .effective_current(id)
                .is_some_and(|ev| ev.state != state::STATE_IDLE)
            {
                break;
            }
            self.talk_sleep(id)?;
        }
        self.settle(id, idle, false, &not_ready)
    }

    /// Types the page into pane id: one paste when its program has
    /// bracketed paste on, else the line that has it print the page.
    fn send_page(&self, id: &str) -> Result<(), String> {
        let Some(lp) = self.live(id) else {
            return Err("the foreman ended".into());
        };
        self.mark_input(id, true);
        if let Some(a) = &lp.adapter {
            return a.prompt(&format!("{FLOOR_HEADER}\n\n{FOREMAN_PAGE}"));
        }
        if !lp.grid.paste_mode() {
            return lp.write(format!("{FLOOR_LINE}\r").as_bytes());
        }
        lp.write(format!("\x1b[200~{FLOOR_HEADER}\n\n{FOREMAN_PAGE}\x1b[201~").as_bytes())?;
        thread::sleep(PASTE_GAP);
        lp.write(b"\r")
    }

    /// Waits until pane id's state passes ok. With fresh, the state must
    /// also be newer than the first thing the pane drew. It posts note once
    /// when the wait passes the foreman wait.
    fn settle(
        &self,
        id: &str,
        ok: impl Fn(&Event) -> bool,
        fresh: bool,
        note: &str,
    ) -> Result<(), TalkErr> {
        let start = Instant::now();
        let mut noted = false;
        loop {
            let lp = self.live(id);
            match (&lp, self.tree.pane(id)) {
                (Some(_), Some(rec)) if !rec.closed => {}
                _ => return Err(TalkErr::Gone),
            }
            let ev = self.effective_current(id);
            let mut newer = !fresh;
            if fresh {
                if let (Some(lp), Some(ev)) = (&lp, &ev) {
                    if let Some(first) = lp.grid.first_write() {
                        newer = ev.ts * 1e9 >= first as f64;
                    }
                }
            }
            if let Some(ev) = &ev {
                if newer && ok(ev) {
                    return Ok(());
                }
            }
            if !noted && start.elapsed() > DEFAULT_FOREMAN_WAIT {
                noted = true;
                self.note(note, id);
            }
            self.talk_sleep(id)?;
        }
    }

    /// Waits one poll. It fails when the server closes or the foreman
    /// ended.
    fn talk_sleep(&self, id: &str) -> Result<(), TalkErr> {
        if self.talk_stop.wait(Some(TALK_POLL)) {
            return Err(TalkErr::Stopped);
        }
        match self.tree.pane(id) {
            Some(rec) if !rec.closed => Ok(()),
            _ => Err(TalkErr::Gone),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn talk_is_one_line_of_printable_words() {
        assert_eq!(
            talk_text("  hello\tforeman\u{7}\n now "),
            "hello foreman now"
        );
        assert_eq!(talk_text("\u{1b}[31m"), "[31m");
        assert_eq!(talk_text(" \n\t "), "");
    }
}
