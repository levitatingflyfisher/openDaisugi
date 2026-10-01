//! Failover of the chat-mode foreman (FM-R-45 to FM-R-54). The twin of Go's
//! server/foreman_failover.go.
//!
//! [foreman] turn_profiles is an ordered chain. A turn runs on the first
//! profile that is not cooling down. When that profile is out (a quota, an
//! overload, not reachable) it cools down and the same turn input goes to the
//! next one. When the cool-down ends, the next turn tries the first profile
//! again. The judge of a failure and the cool-down arithmetic are in
//! foreman/failover.rs; this file runs the turns over them.

use std::sync::Arc;
use std::time::Duration;

use super::foreman_chat::{ForemanChat, ForemanConfig};
use super::foreman_tools::TurnSocket;
use super::foreman_turn::{ClaudeTurn, SprigTurn, TurnInput, TurnResult, TurnRunner};
use super::Server;
use crate::foreman::chat as fchat;
use crate::foreman::failover::{classify, profile_name, safe_reason, Chain, Outcome};

/// Why a run over the turn chain failed. A timeout is its own kind: one
/// attempt took longer than the turn may. It does not move on, a slow model
/// is not an outage.
#[derive(Debug, PartialEq, Eq)]
pub enum ChainError {
    Timeout,
    Other(String),
}

impl std::fmt::Display for ChainError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ChainError::Timeout => f.write_str("the turn timed out"),
            ChainError::Other(s) => f.write_str(s),
        }
    }
}

impl From<String> for ChainError {
    fn from(s: String) -> Self {
        ChainError::Other(s)
    }
}

impl ForemanConfig {
    /// The chain of turn profiles: turn_profiles when it is set, else
    /// turn_profile as a chain of one.
    pub fn turn_chain(&self) -> Vec<String> {
        if !self.turn_profiles.is_empty() {
            return self.turn_profiles.clone();
        }
        vec![self.turn_profile.clone()]
    }
}

/// Whether a profile name means Claude Code.
fn is_claude_profile(p: &str) -> bool {
    p.is_empty() || p == "default"
}

/// What `run_chain` gives back.
pub struct ChainRun {
    pub res: TurnResult,
    /// The one note to log after the reply: a move, or a move back; empty
    /// when nothing moved.
    pub note: String,
}

/// Finds two models in a chain that must serve one: the turn model and the
/// model a profile names for itself. None when they agree.
pub fn model_mismatch(
    chain: &[String],
    turn_model: &str,
    models: &std::collections::HashMap<String, String>,
) -> Option<(String, String)> {
    for p in chain {
        if is_claude_profile(p) {
            continue;
        }
        if let Some(m) = models.get(p).filter(|m| !m.is_empty() && *m != turn_model) {
            return Some((turn_model.to_string(), m.clone()));
        }
    }
    None
}

/// A time of day for a note, in UTC like the log.
fn clock_text(secs: i64) -> String {
    let t = fchat::date_text(
        std::time::SystemTime::UNIX_EPOCH + Duration::from_secs(secs.max(0) as u64),
    );
    format!("{} UTC", &t[11..16])
}

/// `clock_text`, with the date when t is not on the day of now.
pub fn when_text(t: i64, now: i64) -> String {
    if t.div_euclid(86400) == now.div_euclid(86400) {
        return clock_text(t);
    }
    const MONTHS: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    let d = fchat::date_text(
        std::time::SystemTime::UNIX_EPOCH + Duration::from_secs(t.max(0) as u64),
    );
    let month: usize = d[5..7].parse().unwrap_or(1);
    let day: u32 = d[8..10].parse().unwrap_or(1);
    format!("{} {} {} UTC", MONTHS[month.clamp(1, 12) - 1], day, &d[11..16])
}

impl Server {
    pub(super) fn now_secs(&self) -> i64 {
        fchat::unix_secs(self.fm_now())
    }

    /// The runner of account i of the chain. Every account serves the one
    /// model, turn_model.
    fn runner_for(self: &Arc<Server>, _i: usize, profile: &str) -> Arc<dyn TurnRunner> {
        {
            let t = self.turns_lock();
            if let Some(f) = &t.runner_for {
                return f(profile);
            }
            if let Some(r) = &t.runner {
                return r.clone();
            }
        }
        let f = &self.cfg.foreman;
        let model = f.turn_model.clone();
        if is_claude_profile(profile) {
            return Arc::new(ClaudeTurn {
                bin: self.claude_bin(),
                model,
                self_cmd: f.self_cmd.clone(),
                fence: Arc::downgrade(self),
            });
        }
        Arc::new(SprigTurn {
            s: Arc::downgrade(self),
            profile: profile.to_string(),
            model,
            self_cmd: f.self_cmd.clone(),
        })
    }

    /// The chain of this server, loaded from the state file on first use and
    /// written back at each change.
    pub(super) fn failover_chain(&self, fc: &ForemanChat) -> Arc<Chain> {
        let c = {
            let mut t = self.turns_lock();
            if let Some(c) = &t.chain {
                return c.clone();
            }
            let c = Arc::new(Chain::load(
                &fc.dir,
                &self.cfg.foreman.turn_chain(),
                self.now_secs(),
            ));
            let _ = c.save(&fc.dir, self.now_secs());
            t.chain = Some(c.clone());
            c
        };
        let n = c.load_note();
        if !n.is_empty() {
            self.chain_note(fc, &n);
        }
        c
    }

    /// Runs after the chat opens and its lock is released: checks, once,
    /// in the chat, that the accounts of the turn chain serve one model
    /// (FM-R-54, FM-R-58). It stops with the chat.
    pub fn check_models_at_open(self: &Arc<Server>, fc: &ForemanChat) {
        let ctx = fc.cancel.with_timeout(Duration::from_secs(10));
        if let Ok(Some(bad)) = self.chain_models_problem(&ctx) {
            if fc.cancel.err().is_none() {
                self.chain_note(fc, &format!("{bad} Fix [foreman] in the config, then start coppice again."));
            }
        }
    }

    /// Names each account that cools and until when.
    fn out_list(&self, ch: &Chain) -> String {
        let now = self.now_secs();
        ch.status(now)
            .iter()
            .filter(|r| r.state == "cooling")
            .map(|r| {
                let until = fchat::parse_date(&r.until).map(fchat::unix_secs).unwrap_or(0);
                format!("account {} until {}", r.name, when_text(until, now))
            })
            .collect::<Vec<_>>()
            .join(" and ")
    }

    fn save_chain(&self, fc: &ForemanChat, c: &Chain) {
        if let Err(e) = c.save(&fc.dir, self.now_secs()) {
            self.note_once(&format!(
                "The foreman's failover state could not be saved: {e}"
            ));
        }
    }

    /// Waits secs, or until ctx ends; true when ctx ended first.
    fn failover_sleep(&self, ctx: &crate::foreman::ctx::Ctx, secs: i64) -> bool {
        let hook = self.turns_lock().sleep.clone();
        if let Some(f) = hook {
            return f(ctx, secs);
        }
        ctx.sleep(Duration::from_secs(secs.max(0) as u64))
    }

    /// Runs one turn over the chain. The same input goes to each profile in
    /// turn, never twice to one profile in a pass. A turn that ran a tool on
    /// a profile before it failed is not replayed on another: its effects
    /// would repeat. When every profile is cooling, the turn waits for the
    /// earliest end.
    pub fn run_chain(
        self: &Arc<Server>,
        ctx: &crate::foreman::ctx::Ctx,
        fc: &Arc<ForemanChat>,
        ts: &Arc<TurnSocket>,
        input: TurnInput,
    ) -> Result<ChainRun, ChainError> {
        let canceled = |ctx: &crate::foreman::ctx::Ctx| {
            ChainError::Other(ctx.err().map(|e| e.text().to_string()).unwrap_or_default())
        };
        let ch = self.failover_chain(fc);
        self.check_chain_models(ctx)?;
        let mut passes = 0;
        let mut i = ch.first(self.now_secs());
        loop {
            let Some(cur) = i else {
                let Some(until) = ch.earliest(self.now_secs()) else {
                    i = ch.first(self.now_secs());
                    if i.is_none() {
                        return Err("no turn profile can run".to_string().into());
                    }
                    continue;
                };
                passes += 1;
                let now = self.now_secs();
                // The wake hook is set before the note, so an owner message
                // in between still ends the pause (FM-R-55).
                let gap = self.turns_lock().pause_gap.clone();
                if let Some(h) = gap {
                    h();
                }
                let pctx = ctx.child();
                let (hook, waiting) = {
                    let mut t = self.turns_lock();
                    t.pause = Some(pctx.clone());
                    (t.pause_set.clone(), !t.queue.is_empty())
                };
                // A message that came before the hook was set is already
                // queued: it ends the pause too (FM-R-55).
                if waiting {
                    pctx.cancel();
                }
                if let Some(h) = hook {
                    h();
                }
                let note = if passes == 1 {
                    format!(
                        "The foreman is paused: every account is out ({}). It tries again at {}, or when you talk.",
                        self.out_list(&ch),
                        when_text(until, now)
                    )
                } else {
                    format!(
                        "The foreman is paused again (wait {passes} of this turn): every account is out again ({}). Each repeat waits longer, up to 2 hours. It tries again at {}, or when you talk.",
                        self.out_list(&ch),
                        when_text(until, now)
                    )
                };
                self.chain_note(fc, &note);
                let ended = self.failover_sleep(&pctx, until - now);
                self.turns_lock().pause = None;
                if ctx.err().is_some() {
                    return Err(canceled(ctx));
                }
                i = ch.first(self.now_secs());
                if ended && i.is_none() {
                    // The owner spoke and the accounts are still out.
                    return Err(format!("every account is still out ({})", self.out_list(&ch)).into());
                }
                if let Some(n) = i {
                    let why = if ended { "you spoke, so the pause ended" } else { "the pause ended" };
                    self.chain_note(
                        fc,
                        &format!(
                            "The foreman is no longer paused ({why}). It tries account {} now.",
                            profile_name(&ch.profile(n))
                        ),
                    );
                }
                continue;
            };
            let profile = ch.profile(cur);
            let before = ts.calls();
            let actx = ctx.with_timeout(self.cfg.foreman.turn_timeout);
            let res = self.runner_for(cur, &profile).run(&actx, input.clone());
            let timed_out = actx.own_deadline_passed();
            match res {
                Ok(res) => {
                    let prev = ch.last_ran().unwrap_or(0);
                    ch.mark_ok(cur);
                    ch.mark_ran(cur);
                    self.save_chain(fc, &ch);
                    return Ok(ChainRun {
                        res,
                        note: self.move_note(&ch, prev, cur),
                    });
                }
                Err(err) => {
                    if ctx.err().is_some() {
                        return Err(canceled(ctx));
                    }
                    if timed_out {
                        return Err(ChainError::Timeout);
                    }
                    let now = self.now_secs();
                    let name = profile_name(&profile);
                    let v = classify(&err.classify_text(), now);
                    match v.outcome {
                        Outcome::KeyProblem => {
                            ch.mark_failing(cur, &v.reason);
                            self.save_chain(fc, &ch);
                            return Err(format!(
                                "{} on {name}, so the foreman did not move to the next profile",
                                v.reason
                            )
                            .into());
                        }
                        Outcome::Out => {
                            let until = ch.mark_out(cur, &v, now);
                            self.save_chain(fc, &ch);
                            if ts.calls() > before || ts.logged_start() {
                                return Err(format!(
                                    "{name} ran out ({}) after it had used tools in this turn, so the turn was not run again on another profile: its effects could repeat. Until {}",
                                    v.reason,
                                    when_text(until, now)
                                )
                                .into());
                            }
                            i = ch.next(Some(cur), self.now_secs());
                        }
                        Outcome::NotOut => {
                            ch.mark_failing(cur, &safe_reason(&err.classify_text()));
                            self.save_chain(fc, &ch);
                            return Err(err.safe_text().into());
                        }
                    }
                }
            }
        }
    }

    /// The note for a turn that ran on profile idx when the last turn ran on
    /// prev: that the earlier profiles are out and until when, or that the
    /// foreman is back on a profile it prefers. Empty when nothing moved.
    fn move_note(&self, ch: &Chain, prev: usize, idx: usize) -> String {
        if idx == prev {
            return String::new();
        }
        let name = profile_name(&ch.profile(idx));
        if idx < prev {
            return format!("account {name} is back; this turn ran on account {name}.");
        }
        let mut outs: Vec<String> = Vec::new();
        for (j, r) in ch.status(self.now_secs()).iter().enumerate() {
            if j < idx && r.state == "cooling" {
                let until = fchat::parse_date(&r.until)
                    .map(fchat::unix_secs)
                    .unwrap_or(0);
                outs.push(format!(
                    "account {} is out until {}",
                    r.name,
                    when_text(until, self.now_secs())
                ));
            }
        }
        if outs.is_empty() {
            return String::new();
        }
        format!("{}; this turn ran on account {name}.", outs.join(" and "))
    }

    /// Finds a turn chain whose accounts serve two models. It reads the
    /// operator's profile table, so it needs the daisugi bundle, and it looks
    /// only at a turn chain of more than one account. FM-R-54. Delegation to
    /// other models is not in its reach.
    fn chain_models_problem(
        self: &Arc<Server>,
        ctx: &crate::foreman::ctx::Ctx,
    ) -> Result<Option<String>, String> {
        let chain = self.cfg.foreman.turn_chain();
        {
            let t = self.turns_lock();
            if chain.len() < 2 || t.runner_for.is_some() || t.runner.is_some() {
                return Ok(None);
            }
        }
        let table = self.operator_bundle(ctx)?;
        let models: std::collections::HashMap<String, String> = table
            .iter()
            .filter_map(|(p, e)| e.model.clone().map(|m| (p.clone(), m)))
            .collect();
        if let Some((a, b)) = model_mismatch(&chain, &self.cfg.foreman.turn_model, &models) {
            return Ok(Some(format!(
                "The foreman's turn chain serves two models: turn_model is {} and an account serves {}. The foreman never moves to another model; a turn chain holds only accounts of one model.",
                crate::sys::go_quote(&a),
                crate::sys::go_quote(&b)
            )));
        }
        Ok(None)
    }

    /// The backstop at each turn: refuses a turn chain of two models before
    /// any account runs.
    fn check_chain_models(
        self: &Arc<Server>,
        ctx: &crate::foreman::ctx::Ctx,
    ) -> Result<(), String> {
        match self.chain_models_problem(ctx)? {
            Some(bad) => Err(bad
                .replacen("The foreman's turn chain", "the turn chain", 1)
                .trim_end_matches('.')
                .to_string()),
            None => Ok(()),
        }
    }

    /// Logs a note in the middle of a turn.
    fn chain_note(&self, fc: &ForemanChat, text: &str) {
        match self.chat_append(fc, fchat::KIND_NOTE, text) {
            Ok(()) => self.note(text, ""),
            Err(e) => self.note_once(&format!("The foreman's log could not take a note: {e}")),
        }
    }

    /// The name of the profile the last turn ran on, for the stack bar;
    /// None before any turn has run.
    pub fn last_turn_profile(&self) -> Option<String> {
        let t = self.turns_lock();
        let c = t.chain.as_ref()?;
        c.last_ran().map(|i| profile_name(&c.profile(i)))
    }
}
