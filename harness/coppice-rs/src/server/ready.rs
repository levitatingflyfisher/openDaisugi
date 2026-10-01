//! The ready-prompt rules and pane.trust. A text prompt reaches an agent
//! only when its input box is ready: it goes at once to a pane that reads
//! idle, waits in a queue while the pane starts or works, and is refused
//! while the pane waits on a question. This applies only to a pty pane
//! whose harness has a manifest with an idle rule. Raw keys and text with
//! no Enter go through at once: they are the owner's own keys.

use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::atomic::{AtomicI32, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::json;

use super::detect::TRUST_RULE;
use super::{Client, Server};
use crate::detect::{self, Verdict};
use crate::gojson;
use crate::layout;
use crate::proto::{self, Request, Response};
use crate::state;

/// The pause between a prompt's text and its Enter, and between keys.
pub const PASTE_GAP: Duration = Duration::from_millis(150);
/// How long after the Enter the box is looked at again.
const RESUBMIT_WAIT: Duration = Duration::from_millis(800);
/// How long a pane must read ready before a queued prompt goes.
const READY_SETTLE: Duration = Duration::from_millis(300);
/// How often a queue looks at its pane's screen.
const PROMPT_POLL: Duration = Duration::from_millis(100);
/// The longest a queue waits on a pane that stays in one state and is
/// not working.
const PROMPT_QUEUE_WAIT: Duration = Duration::from_secs(10 * 60);
const MAX_PROMPT_QUEUE: usize = 16;
const MAX_PROMPT_BYTES: usize = 64 << 10;
/// How long after an answer to the trust screen that screen may still
/// show.
const TRUST_GRACE: Duration = Duration::from_secs(5);

pub const SCREEN_QUESTION_REFUSAL: &str =
    "{} is waiting on a question on its screen. Answer it first: open it.";
pub const QUEUED_NOTE: &str = "queued until {} is ready";

/// The states of a queued prompt's wait: no waiter, a waiter, and a
/// waiter the drop told.
const WAIT_OFF: i32 = 0;
const WAIT_ON: i32 = 1;
const WAIT_TOLD: i32 = 2;

/// Why a prompt did not go.
#[derive(Clone, Debug, PartialEq)]
pub enum PromptErr {
    Refused(String),
    /// The pane ended, or its record went away.
    Gone,
    /// The pane did not read ready in time.
    Timeout,
    /// The server closes.
    Stopped,
    Write(String),
}

struct QueuedPrompt {
    text: String,
    done: SyncSender<Result<(), PromptErr>>,
    from: String,
    client: Option<Arc<Client>>,
    wait: Arc<AtomicI32>,
}

#[derive(Default)]
struct PromptQueue {
    items: VecDeque<QueuedPrompt>,
    bytes: usize,
    running: bool,
    label: String,
}

/// What the prompt rules hold, under one lock.
#[derive(Default)]
pub struct PromptState {
    prompts: HashMap<String, PromptQueue>,
    sending: HashSet<String>,
    trusted: HashMap<String, (Instant, bool)>,
    trusting: HashSet<String>,
}

impl PromptState {
    fn prune_trust(&mut self) {
        self.trusted.retain(|_, (at, _)| at.elapsed() < TRUST_GRACE);
    }

    fn queue_idle(&self, id: &str) -> bool {
        let idle = match self.prompts.get(id) {
            None => true,
            Some(q) => q.items.is_empty() && !q.running,
        };
        idle && !self.sending.contains(id)
    }
}

/// What submit_prompt did.
pub struct PromptOutcome {
    pub sent: bool,
    pub queued: usize,
    done: Option<Receiver<Result<(), PromptErr>>>,
    wait: Option<Arc<AtomicI32>>,
}

fn ready(v: &Verdict) -> bool {
    v.matched && v.state == detect::STATE_IDLE
}

fn fill(tpl: &str, s: &str) -> String {
    tpl.replacen("{}", s, 1)
}

/// The refusal for a prompt to pane id, which waits on the question v
/// shows.
fn question_refusal(label: &str, id: &str, v: &Verdict) -> String {
    if v.rule_id == TRUST_RULE {
        return format!(
            "{label} is waiting on a question on its screen. Answer it first: coppice pane trust {id}, or open it."
        );
    }
    fill(SCREEN_QUESTION_REFUSAL, label)
}

/// Whether r is a text prompt: agent.prompt, pane.run, or pane.send_text
/// with text and Enter.
pub fn is_prompt_request(r: &Request) -> bool {
    match r.cmd.as_str() {
        "agent.prompt" | "pane.run" => true,
        "pane.send_text" => {
            let text = r.s("text");
            let enter = r.bool("enter");
            !text.is_empty() && enter.unwrap_or(true)
        }
        _ => false,
    }
}

/// Whether an input box holds the start of text, or a pasted-text mark.
pub fn box_holds(bx: &str, text: &str) -> bool {
    if bx.contains("[Pasted text") {
        return true;
    }
    let head = text.split('\n').next().unwrap_or("").trim();
    if head.is_empty() {
        return false;
    }
    let head: String = head.chars().take(24).collect();
    let words = |s: &str| s.split_whitespace().collect::<Vec<_>>().join(" ");
    words(bx).contains(&words(&head))
}

/// How many lines the cursor must go down (up when below zero) to reach
/// "Yes, I trust this folder". None when the screen shows no yes option or
/// no cursor mark.
pub fn trust_moves(screen: &str) -> Option<i64> {
    let re = detect::compile_pattern(
        r"(?i)^[\s│]*(❯)?\s*(?:\d\.\s*)?(yes, i trust this folder|no, exit)",
    )
    .ok()?;
    let (mut yes, mut sel, mut n) = (-1i64, -1i64, 0i64);
    for line in screen.split('\n') {
        let Some(m) = re.captures(line) else {
            continue;
        };
        if m.get(2)
            .is_some_and(|w| detect::go_lower(w.as_str()).starts_with("yes"))
        {
            yes = n;
        }
        if m.get(1).is_some_and(|c| !c.as_str().is_empty()) {
            sel = n;
        }
        n += 1;
    }
    if yes < 0 || sel < 0 {
        return None;
    }
    Some(yes - sel)
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle("pane.trust", Arc::new(|s, c, r| s.handle_trust(c, r)));
}

impl Server {
    fn prompt_lock(&self) -> std::sync::MutexGuard<'_, PromptState> {
        self.prompt.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn answered_trust(&self, id: &str) -> bool {
        let mut g = self.prompt_lock();
        g.prune_trust();
        g.trusted.get(id).is_some_and(|(_, yes)| *yes)
    }

    /// Reads pane id's screen against its harness's manifest. None when
    /// the pane is no pty pane, has no harness, its manifest has no idle
    /// rule, or no tick runs: then the prompt rules do not apply.
    pub fn screen_verdict(&self, id: &str) -> Option<Verdict> {
        let set = self.detect_set()?;
        let rec = self.tree.pane(id)?;
        if rec.kind != layout::KIND_PTY || rec.harness.is_empty() {
            return None;
        }
        let c = set.get(&rec.harness)?;
        if !c.has_idle_rule() {
            return None;
        }
        let lp = self.live(id)?;
        lp.pty.as_ref()?;
        match Server::detection_input(&lp) {
            Ok((input, _)) => Some(c.evaluate(&input)),
            Err(_) => Some(Verdict::default()),
        }
    }

    /// Whether prompts to pane id go through the prompt rules.
    pub fn gated(&self, id: &str) -> bool {
        self.screen_verdict(id).is_some()
    }

    /// Whether pane id waits on a question: its screen shows one, or
    /// another source says it is blocked, as a gate ask does.
    fn waits_on_question(&self, id: &str, v: &Verdict) -> bool {
        if v.matched && v.state == detect::STATE_BLOCKED {
            return true;
        }
        self.effective_current(id)
            .is_some_and(|ev| ev.state == state::STATE_BLOCKED && ev.source != state::SRC_MANIFEST)
    }

    /// Sends a prompt now when the pane is ready, queues it when it starts
    /// or works, and refuses it when it waits on a question.
    pub fn submit_prompt(
        self: &Arc<Server>,
        id: &str,
        text: &str,
        from: &str,
        c: Option<&Arc<Client>>,
    ) -> Result<PromptOutcome, PromptErr> {
        let label = self.label_of(id);
        if text.len() > MAX_PROMPT_BYTES {
            return Err(PromptErr::Refused(format!(
                "the prompt is {} bytes. A prompt may be {MAX_PROMPT_BYTES} bytes at most.",
                text.len()
            )));
        }
        let v = self.screen_verdict(id).unwrap_or_default();
        let answered = v.rule_id == TRUST_RULE && self.answered_trust(id);
        if self.waits_on_question(id, &v) && !answered {
            return Err(PromptErr::Refused(question_refusal(&label, id, &v)));
        }
        if ready(&v) && self.prompt_lock().queue_idle(id) && self.stays_ready(id) {
            let mut g = self.prompt_lock();
            if g.queue_idle(id) {
                g.prompts.remove(id);
                g.sending.insert(id.to_string());
                drop(g);
                let res = self.deliver_prompt(id, text);
                return res.map(|_| PromptOutcome {
                    sent: true,
                    queued: 0,
                    done: None,
                    wait: None,
                });
            }
        }
        let mut g = self.prompt_lock();
        let q = g
            .prompts
            .entry(id.to_string())
            .or_insert_with(|| PromptQueue {
                label: label.clone(),
                ..Default::default()
            });
        if q.items.len() >= MAX_PROMPT_QUEUE || q.bytes + text.len() > MAX_PROMPT_BYTES {
            let (n, b) = (q.items.len(), q.bytes);
            return Err(PromptErr::Refused(format!(
                "{label} has {n} prompts, {b} bytes, waiting already. A queue holds {MAX_PROMPT_QUEUE} prompts and {MAX_PROMPT_BYTES} bytes. Wait until it is ready, then send again."
            )));
        }
        let (tx, rx) = sync_channel(1);
        let wait = Arc::new(AtomicI32::new(WAIT_OFF));
        q.items.push_back(QueuedPrompt {
            text: text.to_string(),
            done: tx,
            from: from.to_string(),
            client: c.cloned(),
            wait: wait.clone(),
        });
        q.bytes += text.len();
        let queued = q.items.len();
        if !q.running {
            q.running = true;
            let s = self.clone();
            let id = id.to_string();
            thread::spawn(move || s.run_prompts(&id));
        }
        Ok(PromptOutcome {
            sent: false,
            queued,
            done: Some(rx),
            wait: Some(wait),
        })
    }

    /// Whether pane id still reads ready, with no question, at each poll
    /// for READY_SETTLE.
    fn stays_ready(&self, id: &str) -> bool {
        let end = Instant::now() + READY_SETTLE;
        while Instant::now() < end {
            if self.talk_stop.wait(Some(PROMPT_POLL)) {
                return false;
            }
            match self.screen_verdict(id) {
                Some(v) if ready(&v) && !self.waits_on_question(id, &v) => {}
                _ => return false,
            }
        }
        true
    }

    /// Writes text, waits PASTE_GAP, then writes the Enter on its own. The
    /// caller set sending for id; the check for a second Enter clears it.
    fn deliver_prompt(self: &Arc<Server>, id: &str, text: &str) -> Result<(), PromptErr> {
        let Some(lp) = self.live(id) else {
            self.clear_sending(id);
            return Err(PromptErr::Gone);
        };
        self.mark_input(id, true);
        if let Err(e) = lp.write(text.as_bytes()) {
            self.clear_sending(id);
            return Err(PromptErr::Write(e));
        }
        thread::sleep(PASTE_GAP);
        if let Err(e) = lp.write(b"\r") {
            self.clear_sending(id);
            return Err(PromptErr::Write(e));
        }
        let s = self.clone();
        let (id, text) = (id.to_string(), text.to_string());
        thread::spawn(move || s.resubmit(&id, &text));
        Ok(())
    }

    fn clear_sending(&self, id: &str) {
        self.prompt_lock().sending.remove(id);
    }

    /// After RESUBMIT_WAIT, one more Enter when the box still holds the
    /// start of text. Never more than one.
    fn resubmit(&self, id: &str, text: &str) {
        if !self.talk_stop.wait(Some(RESUBMIT_WAIT)) && self.still_in_box(id, text) {
            if let Some(lp) = self.live(id) {
                let _ = lp.write(b"\r");
            }
        }
        self.clear_sending(id);
    }

    fn still_in_box(&self, id: &str, text: &str) -> bool {
        match self.screen_verdict(id) {
            Some(v) if ready(&v) => {}
            _ => return false,
        }
        let Some(lp) = self.live(id) else {
            return false;
        };
        match Server::detection_input(&lp) {
            Ok((input, _)) => box_holds(&detect::region(&input, "prompt_box_body"), text),
            Err(_) => false,
        }
    }

    /// Sends pane id's queued prompts in order, each once the pane reads
    /// ready for READY_SETTLE. It drops the rest, and says so, when the
    /// pane ends or waits too long.
    fn run_prompts(self: Arc<Server>, id: &str) {
        let mut ready_since: Option<Instant> = None;
        let (mut wait_start, mut last_key) = (Instant::now(), String::new());
        loop {
            let sending = {
                let mut g = self.prompt_lock();
                match g.prompts.get(id) {
                    Some(q) if !q.items.is_empty() => {}
                    _ => {
                        g.prompts.remove(id);
                        return;
                    }
                }
                g.sending.contains(id)
            };
            let rec = match self.tree.pane(id) {
                Some(rec) if !rec.closed => rec,
                _ => {
                    self.drop_prompts(id, PromptErr::Gone);
                    return;
                }
            };
            if let Some(q) = self.prompt_lock().prompts.get_mut(id) {
                q.label = if rec.label.is_empty() {
                    id.to_string()
                } else {
                    rec.label.clone()
                };
            }
            let v = self.screen_verdict(id);
            let key = match &v {
                None => "unread".to_string(),
                Some(v) if self.waits_on_question(id, v) => detect::STATE_BLOCKED.into(),
                Some(v) => v.state.clone(),
            };
            if key != last_key || key == detect::STATE_WORKING {
                wait_start = Instant::now();
                last_key = key;
            }
            if wait_start.elapsed() > PROMPT_QUEUE_WAIT {
                self.drop_prompts(id, PromptErr::Timeout);
                return;
            }
            match &v {
                None => ready_since = None,
                Some(v) if sending || !ready(v) || self.waits_on_question(id, v) => {
                    ready_since = None
                }
                Some(_) => match ready_since {
                    None => ready_since = Some(Instant::now()),
                    Some(t) if t.elapsed() >= READY_SETTLE => {
                        ready_since = None;
                        self.send_head(id);
                        self.await_turn(id);
                        wait_start = Instant::now();
                        last_key.clear();
                        continue;
                    }
                    Some(_) => {}
                },
            }
            if self.talk_stop.wait(Some(PROMPT_POLL)) {
                self.drop_prompts(id, PromptErr::Stopped);
                return;
            }
        }
    }

    /// Takes the oldest prompt of pane id off its queue and sends it.
    fn send_head(self: &Arc<Server>, id: &str) {
        let head = {
            let mut g = self.prompt_lock();
            let Some(q) = g.prompts.get_mut(id) else {
                return;
            };
            let Some(head) = q.items.pop_front() else {
                return;
            };
            q.bytes -= head.text.len();
            g.sending.insert(id.to_string());
            head
        };
        let res = self.deliver_prompt(id, &head.text);
        let _ = head.done.try_send(res);
    }

    /// Waits, three seconds at most, for pane id to leave ready after a
    /// prompt, so the next prompt does not land before the harness took
    /// this one.
    fn await_turn(&self, id: &str) {
        let end = Instant::now() + Duration::from_secs(3);
        while Instant::now() < end {
            if self.talk_stop.wait(Some(PROMPT_POLL)) {
                return;
            }
            match self.screen_verdict(id) {
                Some(v) if ready(&v) => {}
                _ => return,
            }
        }
    }

    /// Ends pane id's queue: every prompt in it gets why, the floor gets
    /// one note, and each sender hears of it.
    fn drop_prompts(self: &Arc<Server>, id: &str, why: PromptErr) {
        let (lost, label) = {
            let mut g = self.prompt_lock();
            let mut label = id.to_string();
            let mut lost = Vec::new();
            if let Some(q) = g.prompts.get_mut(id) {
                lost = q.items.drain(..).collect::<Vec<_>>();
                if !q.label.is_empty() {
                    label = q.label.clone();
                }
                q.bytes = 0;
                q.running = false;
            }
            g.prompts.remove(id);
            if why == PromptErr::Gone {
                g.trusted.remove(id);
            }
            (lost, label)
        };
        for p in &lost {
            let _ = p.done.try_send(Err(why.clone()));
        }
        if lost.is_empty() || why == PromptErr::Stopped {
            return;
        }
        let n = if lost.len() > 1 {
            format!("{} prompts were", lost.len())
        } else {
            "1 prompt was".to_string()
        };
        let text = if why == PromptErr::Timeout {
            format!(
                "{label} was not ready in {} min, so {n} not sent.",
                PROMPT_QUEUE_WAIT.as_secs() / 60
            )
        } else {
            format!("{label} ended, so {n} not sent.")
        };
        self.note(&text, id);
        let mut told: HashSet<String> = HashSet::new();
        let mut clients: Vec<Arc<Client>> = Vec::new();
        for p in &lost {
            if p.wait
                .compare_exchange(WAIT_ON, WAIT_TOLD, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
            {
                continue;
            }
            if !p.from.is_empty() && p.from != id && told.insert(p.from.clone()) {
                self.note_to(&text, id, &p.from);
                if self.gated(&p.from) {
                    let _ = self.submit_prompt(&p.from, &format!("coppice: {text}"), "", None);
                }
            }
            if let Some(c) = &p.client {
                if !c.dead() && !clients.iter().any(|x| Arc::ptr_eq(x, c)) {
                    clients.push(c.clone());
                    let mut n: Vec<(&str, serde_json::Value)> = vec![
                        ("event", json!("note")),
                        ("text", json!(text)),
                        ("pane", json!(id)),
                    ];
                    if !p.from.is_empty() {
                        n.push(("to", json!(p.from)));
                    }
                    n.push(("ts", gojson::float(Server::now_seconds())));
                    c.emit(gojson::obj(n));
                }
            }
        }
    }

    /// pane.send_text and pane.run for a prompt to a gated pane: sent now,
    /// queued, or refused.
    pub fn prompt_reply(
        self: &Arc<Server>,
        c: &Arc<Client>,
        r: &Request,
        id: &str,
        text: &str,
    ) -> Response {
        let from = c.role().pane_id;
        let from = if c.role().pane { from } else { String::new() };
        match self.submit_prompt(id, text, &from, Some(c)) {
            Err(e) => self.prompt_err(r, e),
            Ok(out) if out.sent => {
                proto::ok_resp(&r.id, gojson::map(vec![("sent", json!(text.len() + 1))]))
            }
            Ok(out) => proto::ok_resp(
                &r.id,
                gojson::map(vec![
                    ("sent", json!(0)),
                    ("queued", json!(out.queued)),
                    ("note", json!(fill(QUEUED_NOTE, &self.label_of(id)))),
                ]),
            ),
        }
    }

    /// The reply for a submit_prompt error.
    pub fn prompt_err(&self, r: &Request, e: PromptErr) -> Response {
        match e {
            PromptErr::Refused(msg) => proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &msg),
            PromptErr::Gone => {
                let id = r.s("pane");
                proto::err_resp(
                    &r.id,
                    proto::ERR_PANE_CLOSED,
                    &format!("pane {id} has closed. Create a new one with: coppice pane create"),
                )
            }
            PromptErr::Timeout => {
                proto::err_resp(&r.id, proto::ERR_INTERNAL, "the pane was not ready in time")
            }
            PromptErr::Stopped => proto::err_resp(&r.id, proto::ERR_INTERNAL, "the server closes"),
            PromptErr::Write(e) => proto::err_resp(&r.id, proto::ERR_INTERNAL, &e),
        }
    }

    /// agent.prompt for a prompt that waits. Without wait it answers at
    /// once. With wait it first waits for the prompt to go, then for the
    /// state the request names.
    pub fn queued_prompt_reply(
        self: &Arc<Server>,
        c: &Arc<Client>,
        r: &Request,
        id: &str,
        text: &str,
        out: PromptOutcome,
    ) -> Response {
        let note = fill(QUEUED_NOTE, &self.label_of(id));
        if !r.bool("wait").unwrap_or(false) {
            return proto::ok_resp(
                &r.id,
                gojson::map(vec![
                    ("pane", json!(id)),
                    ("sent", json!(0)),
                    ("queued", json!(out.queued)),
                    ("note", json!(note)),
                ]),
            );
        }
        let ms = r.int("timeout_ms").filter(|m| *m > 0).unwrap_or(120000);
        let (Some(done), Some(wait)) = (out.done, out.wait) else {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, "the prompt has no queue");
        };
        wait.store(WAIT_ON, Ordering::SeqCst);
        let give_up = |resp: Response| -> Response {
            if wait
                .compare_exchange(WAIT_ON, WAIT_OFF, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
            {
                return resp;
            }
            match done.recv() {
                Ok(Err(e)) => self.prompt_err(r, e),
                _ => resp,
            }
        };
        let deadline = Instant::now() + Duration::from_millis(ms as u64);
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return give_up(proto::err_resp(
                    &r.id,
                    proto::ERR_TIMEOUT,
                    &format!(
                        "the prompt is still {note} after {ms} ms. It goes when {} is ready.",
                        self.label_of(id)
                    ),
                ));
            }
            if c.dead() {
                return give_up(proto::err_resp(
                    &r.id,
                    proto::ERR_TIMEOUT,
                    "client disconnected while waiting",
                ));
            }
            match done.recv_timeout(left.min(Duration::from_millis(50))) {
                Ok(Ok(())) => break,
                Ok(Err(e)) => return self.prompt_err(r, e),
                Err(_) => {}
            }
        }
        self.prompt_wait(c, r, id, text.len())
    }

    /// pane.trust: answers Claude Code's folder trust screen for the
    /// operator. trust true, the default, moves to "Yes, I trust this
    /// folder" and presses Enter; trust false presses Esc.
    fn handle_trust(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return super::role::refuse_view_only(r, &id);
        }
        match self.screen_verdict(&id) {
            Some(v) if v.matched && v.rule_id == TRUST_RULE => {}
            _ => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!("{} does not ask to trust a folder now.", self.label_of(&id)),
                )
            }
        }
        let trust = r.bool("trust").unwrap_or(true);
        {
            let mut g = self.prompt_lock();
            g.prune_trust();
            if g.trusting.contains(&id) || g.trusted.contains_key(&id) {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!(
                        "{} has an answer to its trust screen already. Wait a moment and look at it again.",
                        self.label_of(&id)
                    ),
                );
            }
            g.trusting.insert(id.clone());
        }
        let resp = self.answer_trust(r, &id, &lp, trust);
        self.prompt_lock().trusting.remove(&id);
        resp
    }

    fn answer_trust(
        self: &Arc<Server>,
        r: &Request,
        id: &str,
        lp: &super::panes::LivePane,
        trust: bool,
    ) -> Response {
        self.mark_input(id, true);
        if !trust {
            if let Err(e) = lp.write(b"\x1b") {
                return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
            }
            self.prompt_lock()
                .trusted
                .insert(id.to_string(), (Instant::now(), false));
            return proto::ok_resp(
                &r.id,
                gojson::map(vec![("pane", json!(id)), ("trusted", json!(false))]),
            );
        }
        let screen = match lp.grid.read("detection") {
            Ok(s) => s,
            Err(_) => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_PANE_CLOSED,
                    &format!("pane {id} has closed. Create a new one with: coppice pane create"),
                )
            }
        };
        let Some(mut moves) = trust_moves(&screen) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{} shows no option to trust the folder.", self.label_of(id)),
            );
        };
        let mut key: &[u8] = b"\x1b[B";
        if moves < 0 {
            key = b"\x1b[A";
            moves = -moves;
        }
        for _ in 0..moves {
            if let Err(e) = lp.write(key) {
                return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
            }
            thread::sleep(PASTE_GAP);
        }
        if let Err(e) = lp.write(b"\r") {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e);
        }
        self.prompt_lock()
            .trusted
            .insert(id.to_string(), (Instant::now(), true));
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("trusted", json!(true))]),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trust_moves_reads_the_cursor() {
        let s = " ❯ No, exit\n   Yes, I trust this folder\n";
        assert_eq!(trust_moves(s), Some(1));
        let s = " ❯ 1. Yes, I trust this folder\n   2. No, exit\n";
        assert_eq!(trust_moves(s), Some(0));
        assert_eq!(trust_moves("   Yes, I trust this folder\n"), None);
    }

    #[test]
    fn box_holds_the_start_of_a_long_line() {
        assert!(box_holds(
            "> fix the  flaky test in the",
            "fix the flaky test in the parser now"
        ));
        assert!(box_holds("[Pasted text #1 +3 lines]", "anything"));
        assert!(!box_holds("> ", "hello"));
        assert!(!box_holds("> x", "  \nhello"));
    }

    #[test]
    fn prompts_are_text_with_enter() {
        let (r, _, _) = proto::decode_line(r#"{"id":"1","cmd":"pane.send_text","text":"hi"}"#);
        assert!(is_prompt_request(&r));
        let (r, _, _) =
            proto::decode_line(r#"{"id":"1","cmd":"pane.send_text","text":"hi","enter":false}"#);
        assert!(!is_prompt_request(&r));
        let (r, _, _) = proto::decode_line(r#"{"id":"1","cmd":"pane.run","line":""}"#);
        assert!(is_prompt_request(&r));
    }
}
