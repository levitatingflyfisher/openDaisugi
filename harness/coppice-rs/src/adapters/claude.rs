//! Claude Code, headless: one long-lived `claude -p` process per pane that
//! speaks stream-json both ways. This adapter never reports blocked: in -p
//! mode Claude's permission decisions reach coppice through the gate hook,
//! not this stream. Model text goes into the grid as is.

use std::io::Write;
use std::os::fd::OwnedFd;
use std::process::{Child, ChildStdin, Stdio};
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex};
use std::thread;

use serde_json::json;

use super::{Adapter, Ev, EventsSlot, Kind, Lines, Out, Proc, StartOpts};
use crate::godec::{self, StructTy, Ty};
use crate::pane::Flag;

/// The command run for a headless Claude pane.
pub const BINARY: &str = "claude";
const BIN_VAR: &str = "COPPICE_CLAUDE_BIN";

pub struct Claude;

impl Adapter for Claude {
    fn name(&self) -> &'static str {
        "claude"
    }

    fn start(&self, o: StartOpts) -> Result<Arc<dyn Proc>, String> {
        let bin = o.binary(BIN_VAR, BINARY);
        let mut argv: Vec<String> = [
            "-p",
            "--output-format",
            "stream-json",
            "--input-format",
            "stream-json",
            "--verbose",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        if !o.resume.is_empty() {
            argv.push("--resume".into());
            argv.push(o.resume.clone());
        }
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
        let p = Arc::new(ClaudeProc {
            pid: child.id() as i32,
            child: Mutex::new(Some(child)),
            stdin: Mutex::new(stdin),
            out,
            events: Mutex::new(Some(rx)),
            relay_done: Arc::new(Flag::default()),
            session: Mutex::new(o.resume.clone()),
            stop_once: Mutex::new(false),
        });
        let relay = p.clone();
        thread::spawn(move || relay.relay(stdout));
        Ok(p)
    }

    fn fork_argv(&self, session_id: &str) -> Option<Result<Vec<String>, String>> {
        if session_id.is_empty() {
            return Some(Err("claude cannot fork without a session id".into()));
        }
        Some(Ok(vec!["--fork-session".into()]))
    }
}

struct ClaudeProc {
    pid: i32,
    child: Mutex<Option<Child>>,
    stdin: Mutex<Option<ChildStdin>>,
    out: Out,
    events: EventsSlot,
    relay_done: Arc<Flag>,
    session: Mutex<String>,
    stop_once: Mutex<bool>,
}

static WIRE_MESSAGE: StructTy = StructTy {
    name: "",
    text: "struct { Content json.RawMessage \"json:\\\"content\\\"\" }",
    fields: &[("content", Ty::Raw)],
};

static WIRE_LINE: StructTy = StructTy {
    name: "wireLine",
    text: "claude.wireLine",
    fields: &[
        ("type", Ty::Str),
        ("subtype", Ty::Str),
        ("session_id", Ty::Str),
        ("result", Ty::Str),
        ("message", Ty::Struct(&WIRE_MESSAGE)),
    ],
};

static CONTENT_BLOCK: StructTy = StructTy {
    name: "contentBlock",
    text: "claude.contentBlock",
    fields: &[
        ("type", Ty::Str),
        ("text", Ty::Str),
        ("id", Ty::Str),
        ("name", Ty::Str),
        ("input", Ty::Raw),
        ("tool_use_id", Ty::Str),
        ("content", Ty::Raw),
    ],
};

static BLOCKS: Ty = Ty::Struct(&CONTENT_BLOCK);
static BLOCK_LIST: Ty = Ty::Slice(&BLOCKS, "[]claude.contentBlock");

static SUBAGENT_HOOK: StructTy = StructTy {
    name: "subagentHook",
    text: "claude.subagentHook",
    fields: &[
        ("hook_event_name", Ty::Str),
        ("agent_id", Ty::Str),
        ("agent_type", Ty::Str),
    ],
};

/// One content block: its type and fields.
struct Block {
    ty: String,
    text: String,
    id: String,
    name: String,
    input: String,
    tool_use_id: String,
    content: String,
}

/// message.content: an array of blocks, or a bare string as one text
/// block. Anything else is an error.
fn content_blocks(raw: &str) -> Result<Vec<Block>, String> {
    if raw.is_empty() {
        return Ok(Vec::new());
    }
    let (g, err) = godec::decode(raw.as_bytes(), &BLOCK_LIST);
    if err.is_none() {
        return Ok(g
            .items()
            .iter()
            .map(|b| Block {
                ty: b.f("type").s(),
                text: b.f("text").s(),
                id: b.f("id").s(),
                name: b.f("name").s(),
                input: b.f("input").s(),
                tool_use_id: b.f("tool_use_id").s(),
                content: b.f("content").s(),
            })
            .collect());
    }
    let (g, err) = godec::decode(raw.as_bytes(), &Ty::Str);
    if err.is_none() {
        return Ok(vec![Block {
            ty: "text".into(),
            text: g.s(),
            id: String::new(),
            name: String::new(),
            input: String::new(),
            tool_use_id: String::new(),
            content: String::new(),
        }]);
    }
    Err("message.content is neither an array of blocks nor a string".into())
}

/// A SubagentStart or SubagentStop hook payload as a child event.
pub(crate) fn subagent_event(line: &[u8]) -> Option<Ev> {
    let (h, err) = godec::decode(line, &Ty::Struct(&SUBAGENT_HOOK));
    if err.is_some() {
        return None;
    }
    let name = h.f("hook_event_name").s();
    let state = match name.as_str() {
        "SubagentStart" => super::WORKING,
        "SubagentStop" => "done",
        _ => return None,
    };
    let id = h.f("agent_id").s();
    if id.is_empty() {
        return Some(Ev::error(format!("{name} with no agent_id")));
    }
    Some(Ev {
        kind: Kind::Child,
        child: id,
        state: state.into(),
        text: h.f("agent_type").s(),
        ..Default::default()
    })
}

impl ClaudeProc {
    fn relay(self: Arc<Self>, stdout: OwnedFd) {
        let mut lines = Lines::new(stdout, Arc::new(AtomicBool::new(false)));
        'read: for line in lines.by_ref() {
            if line.is_empty() {
                continue;
            }
            for ev in self.parse(&line) {
                if !self.out.send(ev) {
                    break 'read;
                }
            }
        }
        if let Some(e) = lines.err.clone() {
            self.out.send(Ev::error(format!("stream read failed: {e}")));
        }
        let requested = self.out.stopped();
        let mut code = None;
        if let Some(mut c) = self.child.lock().unwrap_or_else(|e| e.into_inner()).take() {
            let _ = c.kill();
            if let Ok(st) = c.wait() {
                code = Some(super::exit_code(&st));
            }
        }
        let detail = match code {
            Some(n) if !requested => format!("claude exited: exit={n}"),
            _ => "claude exited".to_string(),
        };
        self.out.send_always(Ev::end(detail));
        self.out.close();
        self.relay_done.set();
    }

    fn parse(&self, line: &[u8]) -> Vec<Ev> {
        let (w, err) = godec::decode(line, &Ty::Struct(&WIRE_LINE));
        if let Some(e) = err {
            return vec![Ev::error(format!("cannot read a stream-json line: {e}"))];
        }
        if let Some(ev) = subagent_event(line) {
            return vec![ev];
        }
        let ty = w.f("type").s();
        match ty.as_str() {
            "system" => {
                let sid = w.f("session_id").s();
                if !sid.is_empty() {
                    *self.session.lock().unwrap_or_else(|e| e.into_inner()) = sid;
                }
                let sub = w.f("subtype").s();
                if sub == "init" {
                    return Vec::new();
                }
                vec![Ev::state(super::WORKING, sub)]
            }
            "assistant" => {
                let blocks = match content_blocks(&w.f("message").f("content").s()) {
                    Ok(b) => b,
                    Err(e) => return vec![Ev::error(format!("cannot read message.content: {e}"))],
                };
                let mut out = Vec::new();
                let mut unmodelled = Vec::new();
                for c in blocks {
                    match c.ty.as_str() {
                        "text" => out.push(Ev::text(c.text)),
                        "tool_use" => {
                            let mut detail = c.input.clone();
                            if !c.id.is_empty() {
                                detail = format!("{} {detail}", c.id);
                            }
                            out.push(Ev::tool(c.name, detail));
                        }
                        _ => unmodelled.push(c.ty),
                    }
                }
                if out.is_empty() {
                    out.push(Ev::state(super::WORKING, unmodelled.join(",")));
                }
                out
            }
            "user" => {
                let blocks = match content_blocks(&w.f("message").f("content").s()) {
                    Ok(b) => b,
                    Err(e) => return vec![Ev::error(format!("cannot read message.content: {e}"))],
                };
                blocks
                    .into_iter()
                    .filter(|c| c.ty == "tool_result")
                    .map(|c| Ev::text(format!("tool result {}: {}", c.tool_use_id, c.content)))
                    .collect()
            }
            "result" => {
                let sub = w.f("subtype").s();
                if sub == "success" {
                    return vec![Ev::state(super::IDLE, w.f("result").s())];
                }
                vec![Ev::error(format!("result subtype {sub}"))]
            }
            _ => vec![Ev::state(super::WORKING, ty)],
        }
    }

    fn stopped_err(&self) -> Result<(), String> {
        if self.out.stopped() || self.relay_done.is_set() {
            return Err("claude: this pane has stopped".into());
        }
        Ok(())
    }

    fn write(&self, b: &[u8]) -> Result<(), String> {
        let mut g = self.stdin.lock().unwrap_or_else(|e| e.into_inner());
        match g.as_mut() {
            Some(w) => w.write_all(b).map_err(|e| go_write_err(&e)),
            None => Err("write |1: file already closed".into()),
        }
    }
}

/// Go's text for a failed pipe write.
pub(crate) fn go_write_err(e: &std::io::Error) -> String {
    format!("write |1: {}", crate::sys::go_errno_text(e))
}

impl Proc for ClaudeProc {
    fn prompt(&self, text: &str) -> Result<(), String> {
        self.stopped_err()?;
        // Go marshals a map, so the keys come out sorted.
        let msg = json!({
            "message": {"content": [{"text": text, "type": "text"}], "role": "user"},
            "type": "user",
        });
        let mut b = crate::gojson::marshal(&msg);
        b.push('\n');
        self.write(b.as_bytes())
    }

    fn write_stdin(&self, b: &[u8]) -> Result<(), String> {
        self.stopped_err()?;
        self.write(b)
    }

    fn take_events(&self) -> Option<std::sync::mpsc::Receiver<Ev>> {
        self.events.lock().unwrap_or_else(|e| e.into_inner()).take()
    }

    fn session_id(&self) -> Option<String> {
        let s = self
            .session
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        (!s.is_empty()).then_some(s)
    }

    fn stop(&self) {
        {
            let mut once = self.stop_once.lock().unwrap_or_else(|e| e.into_inner());
            if !*once {
                *once = true;
                self.out
                    .stop
                    .store(true, std::sync::atomic::Ordering::SeqCst);
                self.stdin.lock().unwrap_or_else(|e| e.into_inner()).take();
                super::kill_one(self.pid);
            }
        }
        self.relay_done.wait(None);
    }

    fn pids(&self) -> Vec<i32> {
        vec![self.pid]
    }
}

#[cfg(test)]
mod tests {
    use super::super::testutil::{testdata, until};
    use super::*;
    use std::time::Duration;

    fn start(env: &[(&str, &str)], resume: &str) -> Arc<dyn Proc> {
        let mut o = StartOpts {
            bin: testdata("fake-claude.sh"),
            resume: resume.into(),
            ..Default::default()
        };
        for (k, v) in env {
            o.env.insert(k.to_string(), v.to_string());
        }
        Claude.start(o).unwrap()
    }

    #[test]
    fn a_prompt_replays_the_fixture_as_events() {
        let p = start(
            &[("COPPICE_CLAUDE_FIXTURE", &testdata("claude-stream.jsonl"))],
            "",
        );
        let rx = p.take_events().unwrap();
        p.prompt("go").unwrap();
        let evs = until(&rx, Duration::from_secs(10), |e| e.state == "idle");
        let kinds: Vec<(Kind, String)> = evs
            .iter()
            .map(|e| (e.kind, format!("{}{}{}", e.text, e.tool, e.detail)))
            .collect();
        assert_eq!(
            kinds,
            vec![
                (Kind::Text, "I will list the files.".into()),
                (Kind::Tool, "Bashtoolu_01 {\"command\":\"ls\"}".into()),
                (Kind::Text, "tool result toolu_01: \"a.txt\"".into()),
                (Kind::State, "Listed one file.".into()),
            ]
        );
        assert_eq!(
            p.session_id().as_deref(),
            Some("11111111-2222-3333-4444-555555555555")
        );
        p.stop();
        let rest: Vec<Ev> = rx.iter().collect();
        assert_eq!(rest.last().unwrap().kind, Kind::End);
        assert_eq!(rest.last().unwrap().detail, "claude exited");
        assert_eq!(p.prompt("x").unwrap_err(), "claude: this pane has stopped");
    }

    #[test]
    fn a_natural_exit_names_its_code() {
        let p = start(
            &[
                ("COPPICE_CLAUDE_FIXTURE", &testdata("claude-stream.jsonl")),
                ("COPPICE_CLAUDE_EXIT", "3"),
            ],
            "",
        );
        let rx = p.take_events().unwrap();
        p.prompt("go").unwrap();
        let evs: Vec<Ev> = until(&rx, Duration::from_secs(10), |e| e.kind == Kind::End);
        assert_eq!(evs.last().unwrap().detail, "claude exited: exit=3");
        p.stop();
    }

    #[test]
    fn lines_parse_as_go_parses_them() {
        let p = ClaudeProc {
            pid: 0,
            child: Mutex::new(None),
            stdin: Mutex::new(None),
            out: Out::new().0,
            events: Mutex::new(None),
            relay_done: Arc::new(Flag::default()),
            session: Mutex::new(String::new()),
            stop_once: Mutex::new(false),
        };
        let one = |l: &str| {
            p.parse(l.as_bytes())
                .into_iter()
                .map(|e| {
                    format!(
                        "{:?}|{}|{}|{}|{}",
                        e.kind, e.state, e.text, e.tool, e.detail
                    )
                })
                .collect::<Vec<_>>()
        };
        assert_eq!(
            one("nope"),
            vec!["Error||||cannot read a stream-json line: invalid character 'o' in literal null (expecting 'u')"]
        );
        assert_eq!(
            one(r#"{"type":"assistant","message":{"content":"hi"}}"#),
            vec!["Text||hi||"]
        );
        assert_eq!(
            one(r#"{"type":"assistant","message":{"content":[{"type":"thinking"}]}}"#),
            vec!["State|working|||thinking"]
        );
        assert_eq!(
            one(r#"{"type":"assistant","message":{"content":5}}"#),
            vec!["Error||||cannot read message.content: message.content is neither an array of blocks nor a string"]
        );
        assert_eq!(
            one(r#"{"type":"result","subtype":"error_max_turns"}"#),
            vec!["Error||||result subtype error_max_turns"]
        );
        assert_eq!(one(r#"{"type":5}"#), vec!["Error||||cannot read a stream-json line: json: cannot unmarshal number into Go struct field wireLine.type of type string"]);
        assert_eq!(
            one(r#"{"hook_event_name":"SubagentStart","agent_id":"a1","agent_type":"Explore"}"#),
            vec!["Child|working|Explore||"]
        );
        assert_eq!(
            one(r#"{"type":"stream_event"}"#),
            vec!["State|working|||stream_event"]
        );
    }
}
