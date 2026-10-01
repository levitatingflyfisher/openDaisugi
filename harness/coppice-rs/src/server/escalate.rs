//! Asks held for a foreman. A task can name a foreman pane. An undoable
//! ask of a pane under that task is held: the foreman hears it first, for
//! a time, and may deny it. A permanent ask is never held, but the foreman
//! still hears of it. When the hold ends the ask goes to the operator.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use serde_json::{json, Value};

use super::ready::PASTE_GAP;
use super::Server;
use crate::gojson;
use crate::pane::Flag;
use crate::state::{self, Event};

/// How long a foreman holds a child's undoable ask.
pub const DEFAULT_HOLD_FOR: f64 = 120.0;
/// How long before the gate gives up on an ask a hold must end.
const HOLD_MARGIN: f64 = 30.0;
/// How many surfaced ask ids the server keeps per pane.
const SURFACED_KEPT: usize = 64;

/// One ask a foreman hears before the operator does.
struct Hold {
    pane: String,
    ask: String,
    foreman: String,
    task: String,
    since: f64,
    until: f64,
    /// The short name the server gives the hold.
    id: String,
    told: bool,
    sending: bool,
    cancel: Arc<Flag>,
}

/// What the hold rules keep, under one lock.
#[derive(Default)]
pub struct HoldState {
    holds: HashMap<String, Hold>,
    surfaced: HashMap<String, Vec<String>>,
    typed: HashSet<String>,
    open: HashMap<String, bool>,
    next_hold: u64,
}

impl HoldState {
    fn drop_hold(&mut self, id: &str) {
        if let Some(h) = self.holds.remove(id) {
            h.cancel.set();
        }
    }

    fn was_surfaced(&self, id: &str, ask: &str) -> bool {
        self.surfaced
            .get(id)
            .is_some_and(|l| l.iter().any(|a| a == ask))
    }

    fn mark_surfaced(&mut self, id: &str, ask: &str) {
        let list = self.surfaced.entry(id.to_string()).or_default();
        list.push(ask.to_string());
        if list.len() > SURFACED_KEPT {
            let cut = list.len() - SURFACED_KEPT;
            list.drain(..cut);
        }
    }

    fn unmark_surfaced(&mut self, id: &str, ask: &str) {
        if let Some(list) = self.surfaced.get_mut(id) {
            if let Some(i) = list.iter().position(|a| a == ask) {
                list.remove(i);
            }
        }
    }

    fn held_by(&self, foreman: &str) -> Vec<(String, String)> {
        self.holds
            .values()
            .filter(|h| h.foreman == foreman)
            .map(|h| (h.pane.clone(), h.ask.clone()))
            .collect()
    }
}

/// What escalate decided for one event.
#[derive(Default)]
pub struct Escalation {
    /// The held field the broadcast carries, and the foreman that holds.
    pub held: Option<(Value, String)>,
    pub note: String,
    pub foreman: String,
    /// The asks the pane held as a foreman, when it is blocked itself.
    pub release: Vec<(String, String)>,
}

/// Whether an ask id is plain enough to name in the line typed into a
/// foreman.
fn plain_ask(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 64
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

/// The one line typed into a foreman for a hold. It never holds the ask's
/// summary: the asking pane wrote that.
fn foreman_line(h: &Hold, now: f64) -> String {
    let ask = if plain_ask(&h.ask) {
        format!(" {}", h.ask)
    } else {
        String::new()
    };
    let left = ((h.until - now) as i64).max(0);
    format!(
        "coppice: hold {}: pane {} waits on ask{ask}, and you hear it first for {left} s. \
Read it: coppice agent get {}. Refuse it: coppice agent deny {} {}",
        h.id, h.pane, h.pane, h.pane, h.id
    )
}

/// Whether an idle ends a turn: only the gate and a headless harness know
/// that.
pub fn turn_ended(ev: &Event) -> bool {
    ev.state == state::STATE_IDLE
        && (ev.source == state::SRC_GATE || ev.source == state::SRC_HEADLESS)
}

/// Whether input ends with Enter.
pub fn ends_line(input: &str) -> bool {
    input.ends_with('\r') || input.ends_with('\n')
}

impl Server {
    fn holds_lock(&self) -> std::sync::MutexGuard<'_, HoldState> {
        self.holds.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn held_of(&self, h: &Hold) -> Value {
        let task_label = match self.tree.task(&h.task) {
            Some(tk) if !tk.label.is_empty() => tk.label,
            _ => h.task.clone(),
        };
        gojson::obj(vec![
            ("hold", json!(h.id)),
            ("by", json!(h.foreman)),
            ("foreman_label", json!(self.label_of(&h.foreman))),
            ("task", json!(h.task)),
            ("task_label", json!(task_label)),
            ("ask", json!(h.ask)),
            ("since", gojson::float(h.since)),
            ("until", gojson::float(h.until)),
        ])
    }

    /// The nearest foreman of pane id: its task's, or the nearest
    /// ancestor's that has one, with the task that named it.
    fn foreman_of(&self, id: &str) -> (String, String) {
        let Some(p) = self.tree.pane(id) else {
            return (String::new(), String::new());
        };
        let mut seen = HashSet::new();
        let mut cur = p.task_id;
        while !cur.is_empty() && seen.insert(cur.clone()) {
            let Some(tk) = self.tree.task(&cur) else {
                return (String::new(), String::new());
            };
            if !tk.foreman.is_empty() {
                return (tk.foreman, tk.id);
            }
            cur = tk.parent;
        }
        (String::new(), String::new())
    }

    /// Whether foreman can take an ask from asker: an open pane, not the
    /// asker, and not blocked itself.
    fn can_answer(&self, foreman: &str, asker: &str) -> bool {
        if foreman == asker {
            return false;
        }
        match self.tree.pane(foreman) {
            Some(p) if !p.closed => {}
            _ => return false,
        }
        !self
            .effective_state(foreman)
            .is_some_and(|e| e.state == state::STATE_BLOCKED)
    }

    /// Decides, before the event goes out, whether pane id's merged state
    /// is an ask its foreman hears first.
    pub fn escalate(&self, id: &str, ev: &Event) -> Escalation {
        let mut g = self.holds_lock();
        let release = if ev.state == state::STATE_BLOCKED {
            g.held_by(id)
        } else {
            Vec::new()
        };
        let mut out = self.escalate_ask(&mut g, id, ev);
        out.release = release;
        out
    }

    fn escalate_ask(&self, g: &mut HoldState, id: &str, ev: &Event) -> Escalation {
        let ask = match &ev.ask {
            Some(a) if ev.state == state::STATE_BLOCKED && !a.id.is_empty() => a,
            _ => {
                g.drop_hold(id);
                return Escalation::default();
            }
        };
        if let Some(cur) = g.holds.get(id) {
            if cur.ask == ask.id {
                return Escalation {
                    held: Some((self.held_of(cur), cur.foreman.clone())),
                    ..Default::default()
                };
            }
        }
        g.drop_hold(id);
        if g.was_surfaced(id, &ask.id) {
            return Escalation::default();
        }
        g.mark_surfaced(id, &ask.id);
        let (foreman, task) = self.foreman_of(id);
        if foreman.is_empty() || !self.can_answer(&foreman, id) {
            return Escalation::default();
        }
        let mut out = Escalation {
            foreman: foreman.clone(),
            ..Default::default()
        };
        let label = self.label_of(id);
        let summary = if ask.tool.is_empty() {
            ask.summary.clone()
        } else {
            format!("{}: {}", ask.tool, ask.summary)
        };
        if state::normal_tier(&ask.tier) != state::TIER_UNDOABLE {
            out.note = format!(
                "{label} asks {}: {summary}. It cannot be undone, so it goes to the operator now.",
                ask.id
            );
            return out;
        }
        let now = Server::now_seconds();
        let mut until = now + DEFAULT_HOLD_FOR;
        if ask.deadline > 0.0 {
            let last = ask.deadline - HOLD_MARGIN;
            if last < until {
                until = last;
            }
        }
        if until <= now {
            out.note = format!(
                "{label} asks {}: {summary}. The gate waits too little for a hold, so it goes to the operator now.",
                ask.id
            );
            return out;
        }
        g.unmark_surfaced(id, &ask.id);
        g.next_hold += 1;
        let cancel = Arc::new(Flag::default());
        let h = Hold {
            pane: id.to_string(),
            ask: ask.id.clone(),
            foreman: foreman.clone(),
            task,
            since: now,
            until,
            id: format!("h{}", g.next_hold),
            told: false,
            sending: false,
            cancel: cancel.clone(),
        };
        if let Some(me) = self.me.upgrade() {
            let (pane, ask_id) = (id.to_string(), ask.id.clone());
            let wait = Duration::from_secs_f64(until - now);
            thread::spawn(move || {
                if !cancel.wait(Some(wait)) {
                    me.surface(&pane, &ask_id);
                }
            });
        }
        out.held = Some((self.held_of(&h), foreman));
        out.note = format!(
            "{label} asks {}: {summary}. You hear it first. To refuse it, run: coppice agent deny {id} {}. It goes to the operator in {} s.",
            ask.id,
            ask.id,
            (until - now) as i64
        );
        g.holds.insert(id.to_string(), h);
        out
    }

    /// The held field for pane id, or None.
    pub fn held_for(&self, id: &str) -> Option<Value> {
        let g = self.holds_lock();
        g.holds.get(id).map(|h| self.held_of(h))
    }

    /// The foreman that holds ask on pane id, or "".
    pub fn holder(&self, id: &str, ask: &str) -> String {
        match self.holds_lock().holds.get(id) {
            Some(h) if h.ask == ask => h.foreman.clone(),
            _ => String::new(),
        }
    }

    /// The ask id that ask names on pane id: the held ask when ask is that
    /// hold's short id, ask itself otherwise.
    pub fn resolve_hold(&self, id: &str, ask: &str) -> String {
        match self.holds_lock().holds.get(id) {
            Some(h) if h.id == ask => h.ask.clone(),
            _ => ask.to_string(),
        }
    }

    /// Ends the hold on ask of pane id when it is still the one held, and
    /// sends the pane's state again with no hold.
    pub fn surface(&self, id: &str, ask: &str) {
        let foreman = {
            let mut g = self.holds_lock();
            match g.holds.get(id) {
                Some(h) if h.ask == ask => {}
                _ => return,
            }
            let h = g.holds.remove(id).expect("hold");
            h.cancel.set();
            g.mark_surfaced(id, ask);
            if let Some(ev) = self.states.current(id) {
                if ev.state == state::STATE_BLOCKED && ev.ask.as_ref().is_some_and(|a| a.id == ask)
                {
                    self.broadcast("state", id, &super::agents::state_event(&ev, None));
                }
            }
            h.foreman
        };
        self.note_to(
            &format!(
                "{}'s ask {ask} goes to the operator now.",
                self.label_of(id)
            ),
            id,
            &foreman,
        );
    }

    /// Sends to the operator every held ask whose pane's nearest foreman
    /// is no longer the one that holds it.
    pub fn recheck_holds(&self) {
        let gone: Vec<(String, String)> = {
            let g = self.holds_lock();
            g.holds
                .values()
                .filter(|h| self.foreman_of(&h.pane).0 != h.foreman)
                .map(|h| (h.pane.clone(), h.ask.clone()))
                .collect()
        };
        for (p, a) in gone {
            self.surface(&p, &a);
        }
    }

    /// Records that someone typed into pane id, and whether the input
    /// ended with Enter.
    pub fn mark_input(&self, id: &str, entered: bool) {
        let mut g = self.holds_lock();
        g.typed.insert(id.to_string());
        g.open.insert(id.to_string(), !entered);
    }

    /// Forgets pane id's input mark once a turn ended, unless a half line
    /// still sits in its input.
    pub fn went_idle(&self, id: &str) {
        let mut g = self.holds_lock();
        if g.open.get(id).copied().unwrap_or(false) {
            return;
        }
        g.typed.remove(id);
        g.open.remove(id);
    }

    /// Whether pane id's effective state is an idle that ends a turn.
    fn turn_idle(&self, id: &str) -> bool {
        self.effective_state(id).is_some_and(|e| turn_ended(&e))
    }

    /// Types one line into foreman for each ask it holds and was not told
    /// of, when foreman is idle and nobody typed into it since.
    pub fn tell_holds(&self, foreman: &str) {
        if !self.turn_idle(foreman) {
            return;
        }
        let tell: Vec<(String, String)> = {
            let mut g = self.holds_lock();
            if g.typed.contains(foreman) {
                return;
            }
            let mut out = Vec::new();
            for h in g.holds.values_mut() {
                if h.foreman == foreman && !h.told && !h.sending {
                    h.sending = true;
                    out.push((h.pane.clone(), h.id.clone()));
                }
            }
            out
        };
        let Some(me) = self.me.upgrade() else {
            return;
        };
        for (pane, hold) in tell {
            let s = me.clone();
            let f = foreman.to_string();
            thread::spawn(move || s.tell_one(&f, &pane, &hold));
        }
    }

    /// Writes one hold's line into foreman. The hold is marked told only
    /// when the write succeeds.
    fn tell_one(&self, foreman: &str, pane: &str, hold: &str) {
        let mut told = false;
        if self.turn_idle(foreman) {
            let line = {
                let mut g = self.holds_lock();
                let ok =
                    !g.typed.contains(foreman) && g.holds.get(pane).is_some_and(|h| h.id == hold);
                let line = g
                    .holds
                    .get(pane)
                    .map(|h| foreman_line(h, Server::now_seconds()));
                if ok {
                    g.typed.insert(foreman.to_string());
                    g.open.insert(foreman.to_string(), false);
                    line
                } else {
                    None
                }
            };
            if let Some(line) = line {
                told = self.type_into(foreman, &line).is_ok();
            }
        }
        let mut g = self.holds_lock();
        if let Some(h) = g.holds.get_mut(pane) {
            if h.id == hold {
                h.sending = false;
                h.told = told;
            }
        }
    }

    /// Sends one line with Enter to pane id, the Enter on its own after
    /// PASTE_GAP.
    pub fn type_into(&self, id: &str, line: &str) -> Result<(), String> {
        let Some(lp) = self.live(id) else {
            return Err("the foreman was not told".into());
        };
        if let Some(a) = &lp.adapter {
            return a.prompt(line);
        }
        lp.write(line.as_bytes())?;
        thread::sleep(PASTE_GAP);
        lp.write(b"\r")
    }

    /// For a pane that closed: its own hold ends, and every ask it held as
    /// a foreman goes to the operator at once.
    pub fn end_holds(&self, id: &str) {
        let gone = {
            let mut g = self.holds_lock();
            g.drop_hold(id);
            g.surfaced.remove(id);
            g.held_by(id)
        };
        for (p, a) in gone {
            self.surface(&p, &a);
        }
    }

    /// Stops every hold timer, so none fires after Close.
    pub fn stop_holds(&self) {
        let mut g = self.holds_lock();
        let ids: Vec<String> = g.holds.keys().cloned().collect();
        for id in ids {
            g.drop_hold(&id);
        }
    }

    /// Ends the hold on ask of pane id once someone answered it.
    pub fn end_hold(&self, id: &str, ask: &str) {
        let mut g = self.holds_lock();
        if g.holds.get(id).is_some_and(|h| h.ask == ask) {
            g.drop_hold(id);
            g.mark_surfaced(id, ask);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_foreman_line_names_only_plain_asks() {
        let h = Hold {
            pane: "w1:p2".into(),
            ask: "toolu_02".into(),
            foreman: "w1:p1".into(),
            task: "t1".into(),
            since: 0.0,
            until: 100.0,
            id: "h1".into(),
            told: false,
            sending: false,
            cancel: Arc::new(Flag::default()),
        };
        assert_eq!(
            foreman_line(&h, 40.5),
            "coppice: hold h1: pane w1:p2 waits on ask toolu_02, and you hear it first for 59 s. \
Read it: coppice agent get w1:p2. Refuse it: coppice agent deny w1:p2 h1"
        );
        let h = Hold {
            ask: "a b".into(),
            ..h
        };
        assert!(foreman_line(&h, 200.0).contains("waits on ask, and you hear it first for 0 s"));
    }
}
