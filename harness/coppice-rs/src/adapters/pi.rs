//! pi, headless: one long-lived `pi --mode rpc` process per pane, JSON
//! lines both ways. Text deltas of one message are joined into one event.
//! A dialog request (select, confirm, input, editor) blocks the pane with
//! an ask, and the next prompt answers it. Model text goes into the grid
//! as is.

use std::io::Write;
use std::os::fd::OwnedFd;
use std::process::{Child, ChildStdin, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde_json::{json, Value};

use super::{Adapter, Ev, EventsSlot, Lines, Out, Proc, StartOpts};
use crate::godec::{self, Any};
use crate::pane::Flag;
use crate::state::Ask;

pub const BINARY: &str = "pi";
const BIN_VAR: &str = "COPPICE_PI_BIN";

/// How long a dialog with no timeout of its own stays open.
const DEFAULT_ASK_DEADLINE: Duration = Duration::from_secs(90);

pub struct Pi;

impl Adapter for Pi {
    fn name(&self) -> &'static str {
        "pi"
    }

    fn start(&self, o: StartOpts) -> Result<Arc<dyn Proc>, String> {
        let bin = o.binary(BIN_VAR, BINARY);
        let mut argv = vec!["--mode".to_string(), "rpc".to_string()];
        argv.extend(o.argv.iter().cloned());
        let env = o.child_env(&o.env);
        let mut cmd = super::command(&bin, &argv, &o.cwd, env)
            .map_err(|e| format!("cannot start {bin}: {e}"))?;
        cmd.stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit());
        let mut child = super::spawn(&bin, &mut cmd)?;
        let stdin = child.stdin.take();
        let stdout: OwnedFd = child.stdout.take().map(OwnedFd::from).ok_or("no stdout")?;
        let (out, rx) = Out::new();
        let p = Arc::new(PiProc(Arc::new(Inner {
            pid: child.id() as i32,
            child: Mutex::new(Some(child)),
            stdin: Mutex::new(stdin),
            out,
            events: Mutex::new(Some(rx)),
            relay_done: Flag::default(),
            stop_once: Mutex::new(false),
            st: Mutex::new(PiState {
                session_file: o.resume.clone(),
                ..Default::default()
            }),
        })));
        let relay = p.0.clone();
        thread::spawn(move || relay.relay(stdout));
        if !o.resume.is_empty() {
            let _ =
                p.0.send(&json!({"sessionPath": o.resume, "type": "switch_session"}));
        } else {
            let _ = p.0.send(&json!({"type": "get_state"}));
        }
        Ok(p)
    }
}

#[derive(Default)]
struct PiState {
    pending_id: String,
    pending_method: String,
    /// Zero when the dialog carries no timeout of its own.
    pending_until: Option<SystemTime>,
    session_id: String,
    session_file: String,
    streaming: bool,
}

impl PiState {
    fn clear_pending(&mut self) {
        self.pending_id.clear();
        self.pending_method.clear();
        self.pending_until = None;
    }
}

struct PiProc(Arc<Inner>);

struct Inner {
    pid: i32,
    child: Mutex<Option<Child>>,
    stdin: Mutex<Option<ChildStdin>>,
    out: Out,
    events: EventsSlot,
    relay_done: Flag,
    stop_once: Mutex<bool>,
    st: Mutex<PiState>,
}

fn is_dialog_method(m: &str) -> bool {
    matches!(m, "select" | "confirm" | "input" | "editor")
}

fn unix(t: SystemTime) -> f64 {
    t.duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as f64)
        .unwrap_or(0.0)
}

impl Inner {
    fn stopped_err(&self) -> Result<(), String> {
        if self.out.stopped() || self.relay_done.is_set() {
            return Err("pi: this pane has stopped".into());
        }
        Ok(())
    }

    fn write(&self, b: &[u8]) -> Result<(), String> {
        let mut g = self.stdin.lock().unwrap_or_else(|e| e.into_inner());
        match g.as_mut() {
            Some(w) => w.write_all(b).map_err(|e| super::claude::go_write_err(&e)),
            None => Err("write |1: file already closed".into()),
        }
    }

    /// Writes one JSON line. Go marshals a map, so v's keys must already be
    /// in sorted order.
    fn send(&self, v: &Value) -> Result<(), String> {
        self.stopped_err()?;
        let mut b = crate::gojson::marshal(v);
        b.push('\n');
        self.write(b.as_bytes())
    }

    fn answer_pending(&self, text: &str) -> Result<(), String> {
        let (id, method) = {
            let mut s = self.st.lock().unwrap_or_else(|e| e.into_inner());
            let x = (s.pending_id.clone(), s.pending_method.clone());
            s.clear_pending();
            x
        };
        let resp = if method == "confirm" {
            let lower = text.trim().to_lowercase();
            json!({"confirmed": lower == "y" || lower == "yes", "id": id, "type": "extension_ui_response"})
        } else {
            json!({"id": id, "type": "extension_ui_response", "value": text})
        };
        self.send(&resp)
    }

    fn relay(self: Arc<Self>, stdout: OwnedFd) {
        let mut text = String::new();
        let emit = |ev: Ev| self.out.send(ev);
        let flush = |text: &mut String| -> bool {
            if text.is_empty() {
                return true;
            }
            let ev = Ev::text(std::mem::take(text));
            emit(ev)
        };
        let mut lines = Lines::new(stdout, Arc::new(AtomicBool::new(false)));
        'read: for line in lines.by_ref() {
            if String::from_utf8_lossy(&line).trim().is_empty() {
                continue;
            }
            let raw = match godec::decode_map(&line) {
                Ok(m) => m,
                Err(e) => {
                    if !emit(Ev::error(format!("cannot read an rpc line: {e}"))) {
                        break 'read;
                    }
                    continue;
                }
            };
            match raw.str_of("type").as_str() {
                "message_update" => {
                    if let Some(ame @ Any::Map(_)) = raw.get("assistantMessageEvent") {
                        if matches!(ame.get("type"), Some(Any::Str(t)) if t == "text_delta") {
                            text.push_str(&ame.str_of("delta"));
                        }
                    }
                }
                "message_end" => {
                    if !flush(&mut text) {
                        break 'read;
                    }
                }
                _ => {
                    if !flush(&mut text) {
                        break 'read;
                    }
                    for ev in self.parse(&raw) {
                        if !emit(ev) {
                            break 'read;
                        }
                    }
                }
            }
        }
        if let Some(e) = lines.err.clone() {
            emit(Ev::error(format!("stream read failed: {e}")));
        }
        flush(&mut text);
        let requested = self.out.stopped();
        let mut code = None;
        if let Some(mut c) = self.child.lock().unwrap_or_else(|e| e.into_inner()).take() {
            let _ = c.kill();
            if let Ok(st) = c.wait() {
                code = Some(super::exit_code(&st));
            }
        }
        let detail = match code {
            Some(n) if !requested => format!("pi exited: exit={n}"),
            _ => "pi exited".to_string(),
        };
        self.out.send_always(Ev::end(detail));
        self.out.close();
        self.relay_done.set();
    }

    fn parse(self: &Arc<Self>, raw: &Any) -> Vec<Ev> {
        match raw.str_of("type").as_str() {
            "agent_start" => {
                self.st.lock().unwrap_or_else(|e| e.into_inner()).streaming = true;
                vec![Ev::state(super::WORKING, "")]
            }
            "agent_end" => vec![Ev::state(super::IDLE, "")],
            "agent_settled" => {
                let mut s = self.st.lock().unwrap_or_else(|e| e.into_inner());
                s.streaming = false;
                s.clear_pending();
                vec![Ev::state(super::IDLE, "")]
            }
            "tool_execution_start" => {
                let args = raw.get("args").cloned().unwrap_or(Any::Nil).marshal();
                let id = raw.str_of("toolCallId");
                let detail = if id.is_empty() {
                    args
                } else {
                    format!("{id} {args}")
                };
                vec![Ev::tool(raw.str_of("toolName"), detail)]
            }
            "extension_ui_request" => {
                let method = raw.str_of("method");
                if !is_dialog_method(&method) {
                    return Vec::new();
                }
                let id = raw.str_of("id");
                let title = raw.str_of("title");
                let summary = if title.is_empty() {
                    raw.str_of("message")
                } else {
                    title
                };
                let now = SystemTime::now();
                let mut until = None;
                let mut deadline = now + DEFAULT_ASK_DEADLINE;
                if let Some(Any::F64(ms)) = raw.get("timeout") {
                    if *ms > 0.0 {
                        let u = now + Duration::from_millis(*ms as u64);
                        until = Some(u);
                        deadline = u;
                    }
                }
                {
                    let mut s = self.st.lock().unwrap_or_else(|e| e.into_inner());
                    s.pending_id = id.clone();
                    s.pending_method = method.clone();
                    s.pending_until = until;
                }
                vec![Ev {
                    ask: Some(Ask {
                        id,
                        tool: method,
                        summary: summary.clone(),
                        deadline: unix(deadline),
                        ..Default::default()
                    }),
                    ..Ev::state(super::BLOCKED, summary)
                }]
            }
            "extension_error" => {
                let mut detail = raw.str_of("error");
                let ev = raw.str_of("event");
                if !ev.is_empty() {
                    detail = format!("{ev}: {detail}");
                }
                vec![Ev::error(format!("extension error: {detail}"))]
            }
            "response" => {
                match raw.str_of("command").as_str() {
                    "get_state" => {
                        if let Some(data @ Any::Map(_)) = raw.get("data") {
                            let mut s = self.st.lock().unwrap_or_else(|e| e.into_inner());
                            s.session_id = data.str_of("sessionId");
                            let f = data.str_of("sessionFile");
                            if !f.is_empty() {
                                s.session_file = f;
                            }
                            if let Some(Any::Bool(b)) = data.get("isStreaming") {
                                s.streaming = *b;
                            }
                        }
                    }
                    "switch_session" => {
                        if let Some(Any::Bool(true)) = raw.get("success") {
                            let me = self.clone();
                            thread::spawn(move || {
                                let _ = me.send(&json!({"type": "get_state"}));
                            });
                        }
                    }
                    _ => {}
                }
                Vec::new()
            }
            _ => Vec::new(),
        }
    }
}

impl Proc for PiProc {
    fn prompt(&self, text: &str) -> Result<(), String> {
        let me = &self.0;
        let (pending, streaming) = {
            let mut s = me.st.lock().unwrap_or_else(|e| e.into_inner());
            if s.pending_until.is_some_and(|u| SystemTime::now() > u) {
                s.clear_pending();
            }
            (s.pending_id.clone(), s.streaming)
        };
        if !pending.is_empty() {
            return me.answer_pending(text);
        }
        let req = if streaming {
            json!({"message": text, "streamingBehavior": "steer", "type": "prompt"})
        } else {
            json!({"message": text, "type": "prompt"})
        };
        me.send(&req)
    }

    fn write_stdin(&self, b: &[u8]) -> Result<(), String> {
        self.0.stopped_err()?;
        self.0.write(b)
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
            .st
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .session_file
            .clone();
        (!s.is_empty()).then_some(s)
    }

    fn stop(&self) {
        let me = &self.0;
        {
            let mut once = me.stop_once.lock().unwrap_or_else(|e| e.into_inner());
            if !*once {
                *once = true;
                me.out.stop.store(true, Ordering::SeqCst);
                me.stdin.lock().unwrap_or_else(|e| e.into_inner()).take();
                super::kill_one(me.pid);
            }
        }
        me.relay_done.wait(None);
    }

    fn pids(&self) -> Vec<i32> {
        vec![self.0.pid]
    }
}

#[cfg(test)]
mod tests {
    use super::super::testutil::until;
    use super::*;
    use crate::adapters::Kind;

    fn fake() -> String {
        format!("{}/cases/headless/fake-pi.sh", env!("CARGO_MANIFEST_DIR"))
    }

    #[test]
    fn a_prompt_streams_one_text_and_a_dialog_blocks() {
        let o = StartOpts {
            bin: fake(),
            ..Default::default()
        };
        let p = Pi.start(o).unwrap();
        let rx = p.take_events().unwrap();
        p.prompt("hello").unwrap();
        let evs = until(&rx, Duration::from_secs(10), |e| e.state == "idle");
        let texts: Vec<&str> = evs
            .iter()
            .filter(|e| e.kind == Kind::Text)
            .map(|e| e.text.as_str())
            .collect();
        assert_eq!(texts, vec!["you said: hello"]);
        assert!(p.session_id().is_some());
        p.prompt("ask me").unwrap();
        let evs = until(&rx, Duration::from_secs(10), |e| e.state == "blocked");
        let ask = evs.last().unwrap().ask.clone().unwrap();
        assert_eq!(
            (ask.tool.as_str(), ask.summary.as_str()),
            ("confirm", "Run the tests?")
        );
        p.prompt("yes").unwrap();
        let evs = until(&rx, Duration::from_secs(10), |e| e.state == "idle");
        assert!(evs.iter().any(|e| e.text == "confirmed: true"), "{evs:?}");
        p.stop();
        let rest: Vec<Ev> = rx.iter().collect();
        assert_eq!(rest.last().unwrap().kind, Kind::End);
    }
}
