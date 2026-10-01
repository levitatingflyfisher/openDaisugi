//! Codex, headless: one `codex exec --json` process per prompt, and
//! `codex exec resume ID --json` once a session id is known. The pane
//! keeps one event stream across turns. Only one turn runs at a time, and
//! a second prompt while one runs is refused, never queued. This adapter
//! never sends an end event: each turn's process exits on its own and the
//! pane keeps taking prompts, so only stop ends a codex pane. Model text
//! goes into the grid as is.

use std::os::fd::OwnedFd;
use std::os::unix::process::CommandExt;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use super::{Adapter, Ev, EventsSlot, Lines, Out, Proc, StartOpts, Turns};
use crate::godec::{self, StructTy, Ty};
use crate::pane::Flag;

pub const BINARY: &str = "codex";
const BIN_VAR: &str = "COPPICE_CODEX_BIN";

pub struct Codex;

impl Adapter for Codex {
    fn name(&self) -> &'static str {
        "codex"
    }

    fn start(&self, o: StartOpts) -> Result<Arc<dyn Proc>, String> {
        let (out, rx) = Out::new();
        Ok(Arc::new(CodexProc(Arc::new(Inner {
            session: Mutex::new(o.resume.clone()),
            opts: o,
            out,
            events: Mutex::new(Some(rx)),
            turn: Mutex::new(TurnState::default()),
            turns: Arc::new(Turns::default()),
            relay_done: Flag::default(),
            stop_once: Mutex::new(false),
        }))))
    }
}

#[derive(Default)]
pub(crate) struct TurnState {
    pub active: bool,
    pub running: i32,
    pub pids: Vec<i32>,
    pub cancel: Option<Arc<AtomicBool>>,
}

/// The proc: a handle on the state its turn threads share.
struct CodexProc(Arc<Inner>);

struct Inner {
    opts: StartOpts,
    out: Out,
    events: EventsSlot,
    session: Mutex<String>,
    turn: Mutex<TurnState>,
    turns: Arc<Turns>,
    relay_done: Flag,
    stop_once: Mutex<bool>,
}

static ITEM: StructTy = StructTy {
    name: "",
    text: "struct { Type string \"json:\\\"type\\\"\"; Text string \"json:\\\"text\\\"\"; Command string \"json:\\\"command\\\"\"; ExitCode *int \"json:\\\"exit_code\\\"\" }",
    fields: &[
        ("type", Ty::Str),
        ("text", Ty::Str),
        ("command", Ty::Str),
        ("exit_code", Ty::PtrInt),
    ],
};

static REASON: StructTy = StructTy {
    name: "wireReason",
    text: "codex.wireReason",
    fields: &[("message", Ty::Str)],
};

static WIRE_LINE: StructTy = StructTy {
    name: "wireLine",
    text: "codex.wireLine",
    fields: &[
        ("type", Ty::Str),
        ("session_id", Ty::Str),
        ("thread_id", Ty::Str),
        ("item", Ty::Struct(&ITEM)),
        ("error", Ty::PtrStruct(&REASON)),
        ("message", Ty::Str),
    ],
};

impl Inner {
    fn begin_turn(&self) -> Result<(), String> {
        let mut t = self.turn.lock().unwrap_or_else(|e| e.into_inner());
        if self.out.stopped() {
            return Err("codex: this pane has stopped".into());
        }
        if t.active {
            return Err("codex: a turn is already running; wait for idle".into());
        }
        t.active = true;
        self.turns.add();
        Ok(())
    }

    fn end_turn(&self) {
        let mut t = self.turn.lock().unwrap_or_else(|e| e.into_inner());
        t.active = false;
        t.running = 0;
    }

    fn abort_turn(&self) {
        self.end_turn();
        self.turns.done();
    }

    /// One JSON line as events, and whether it ends the turn.
    fn parse(&self, line: &[u8]) -> (Vec<Ev>, bool) {
        let (w, err) = godec::decode(line, &Ty::Struct(&WIRE_LINE));
        if let Some(e) = err {
            return (
                vec![Ev::error(format!("cannot read a codex JSON line: {e}"))],
                false,
            );
        }
        let id = {
            let s = w.f("session_id").s();
            if s.is_empty() {
                w.f("thread_id").s()
            } else {
                s
            }
        };
        if !id.is_empty() {
            *self.session.lock().unwrap_or_else(|e| e.into_inner()) = id.clone();
        }
        let reason = || {
            let e = w.f("error");
            let m = e.f("message").s();
            if e.present() && !m.is_empty() {
                m
            } else {
                w.f("message").s()
            }
        };
        let ty = w.f("type").s();
        match ty.as_str() {
            "session.created" | "thread.started" => (
                vec![Ev::state(super::WORKING, format!("session {id}"))],
                false,
            ),
            "item.started" | "item.updated" => (Vec::new(), false),
            "item.completed" => {
                let item = w.f("item");
                let ity = item.f("type").s();
                let text = item.f("text").s();
                let ev = match ity.as_str() {
                    "assistant_message" | "agent_message" => Ev::text(text),
                    "command_execution" => {
                        let mut d = item.f("command").s();
                        if let Some(code) = item.f("exit_code").int() {
                            d = format!("{d} (exit {code})");
                        }
                        Ev::tool("shell", d)
                    }
                    "error" => Ev::error(if text.is_empty() {
                        "item error".to_string()
                    } else {
                        text
                    }),
                    _ => {
                        if !text.is_empty() {
                            Ev::text(text)
                        } else {
                            Ev::state(super::WORKING, ity)
                        }
                    }
                };
                (vec![ev], false)
            }
            "turn.completed" => (vec![Ev::state(super::IDLE, "turn complete")], true),
            "turn.failed" => {
                let mut detail = "turn failed".to_string();
                let r = reason();
                if !r.is_empty() {
                    detail = format!("{detail}: {r}");
                }
                (vec![Ev::state(super::IDLE, detail)], true)
            }
            "error" => {
                let r = reason();
                (vec![Ev::error(if r.is_empty() { ty } else { r })], false)
            }
            _ => (vec![Ev::state(super::WORKING, ty)], false),
        }
    }

    fn run_turn(
        self: Arc<Self>,
        mut child: std::process::Child,
        r: OwnedFd,
        cancel: Arc<AtomicBool>,
    ) {
        let pid = child.id() as i32;
        let mut completed = false;
        let mut lines = Lines::new(r, cancel);
        for line in lines.by_ref() {
            if line.is_empty() {
                continue;
            }
            let (evs, done) = self.parse(&line);
            if done && !completed {
                completed = true;
                self.end_turn();
            }
            for ev in evs {
                self.out.send(ev);
            }
        }
        let stopping = self.out.stopped();
        if let (Some(e), false) = (lines.err.clone(), stopping) {
            self.out.send(Ev::error(format!("stream read failed: {e}")));
        }
        drop(lines);
        if !completed {
            self.end_turn();
        }
        super::kill_group(pid);
        let code = child.wait().map(|st| super::exit_code(&st));
        if self.out.stopped() {
            self.turns.done();
            return;
        }
        if !completed {
            let detail = match code {
                Ok(n) => format!("turn ended without completion exit={n}"),
                Err(e) => format!("turn ended without completion: {e}"),
            };
            self.out.send(Ev::state(super::IDLE, detail));
        }
        self.turns.done();
    }

    fn stopped_err(&self) -> Result<(), String> {
        if self.out.stopped() || self.relay_done.is_set() {
            return Err("codex: this pane has stopped".into());
        }
        Ok(())
    }
}

/// Starts one turn's process for a one-process-per-turn adapter: its own
/// process group, stdout (and stderr when both) into a pipe this side
/// reads. The parent's copy of the write end is closed before it returns.
pub(crate) fn spawn_turn(
    bin: &str,
    argv: &[String],
    o: &StartOpts,
    stderr_too: bool,
) -> Result<(std::process::Child, OwnedFd), String> {
    let mut cmd = super::command(bin, argv, &o.cwd, o.child_env(&o.env))
        .map_err(|e| format!("cannot start {bin}: {e}"))?;
    let (r, w) = super::pipe().map_err(|e| e.to_string())?;
    let err_end = if stderr_too {
        Stdio::from(w.try_clone().map_err(|e| e.to_string())?)
    } else {
        Stdio::inherit()
    };
    cmd.stdin(Stdio::null())
        .stdout(Stdio::from(w))
        .stderr(err_end)
        .process_group(0);
    let child = super::spawn(bin, &mut cmd)?;
    // The command holds the write end; dropping it closes this side's copy,
    // so the read sees an end once the child and what it started let go.
    drop(cmd);
    Ok((child, r))
}

impl Proc for CodexProc {
    fn prompt(&self, text: &str) -> Result<(), String> {
        let me = &self.0;
        me.begin_turn()?;
        let sid = me.session.lock().unwrap_or_else(|e| e.into_inner()).clone();
        let mut argv: Vec<String> = if sid.is_empty() {
            vec!["exec".into(), "--json".into()]
        } else {
            vec!["exec".into(), "resume".into(), sid, "--json".into()]
        };
        argv.extend(me.opts.argv.iter().cloned());
        argv.push("--".into());
        argv.push(text.into());
        let bin = me.opts.binary(BIN_VAR, BINARY);
        let (child, r) = match spawn_turn(&bin, &argv, &me.opts, false) {
            Ok(x) => x,
            Err(e) => {
                me.abort_turn();
                return Err(e);
            }
        };
        let cancel = Arc::new(AtomicBool::new(false));
        {
            let mut t = me.turn.lock().unwrap_or_else(|e| e.into_inner());
            t.running = child.id() as i32;
            t.pids.push(child.id() as i32);
            if t.pids.len() > super::PID_KEEP {
                let n = t.pids.len() - super::PID_KEEP;
                t.pids.drain(..n);
            }
            t.cancel = Some(cancel.clone());
        }
        let me = me.clone();
        thread::spawn(move || me.run_turn(child, r, cancel));
        Ok(())
    }

    fn write_stdin(&self, _b: &[u8]) -> Result<(), String> {
        self.0.stopped_err()?;
        Err("codex: this harness does not support that. Use agent prompt.".into())
    }

    fn take_events(&self) -> Option<std::sync::mpsc::Receiver<Ev>> {
        self.0
            .events
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
    }

    fn session_id(&self) -> Option<String> {
        let s = self
            .0
            .session
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        (!s.is_empty()).then_some(s)
    }

    fn stop(&self) {
        let me = &self.0;
        stop_turns(&me.stop_once, &me.out, &me.turn, &me.turns, &me.relay_done);
    }

    fn pids(&self) -> Vec<i32> {
        self.0
            .turn
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .pids
            .clone()
    }
}

/// Stop for a one-process-per-turn adapter: signal, kill the running
/// turn's group, give the turn a second to end, then force its read
/// closed, wait for every turn, and end the stream.
pub(crate) fn stop_turns(
    once: &Mutex<bool>,
    out: &Out,
    turn: &Mutex<TurnState>,
    turns: &Turns,
    relay_done: &Flag,
) {
    let first = {
        let mut g = once.lock().unwrap_or_else(|e| e.into_inner());
        let f = !*g;
        *g = true;
        f
    };
    if first {
        let (running, cancel) = {
            let t = turn.lock().unwrap_or_else(|e| e.into_inner());
            out.stop.store(true, Ordering::SeqCst);
            (t.running, t.cancel.clone())
        };
        super::kill_group(running);
        let deadline = std::time::Instant::now() + Duration::from_secs(1);
        loop {
            if turns.idle() {
                break;
            }
            if std::time::Instant::now() >= deadline {
                if let Some(c) = cancel {
                    c.store(true, Ordering::SeqCst);
                }
                break;
            }
            thread::sleep(Duration::from_millis(20));
        }
        turns.wait();
        out.close();
        relay_done.set();
    }
    relay_done.wait(None);
}
