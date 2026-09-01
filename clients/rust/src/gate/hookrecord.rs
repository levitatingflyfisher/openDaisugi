//! The payload half of `daisugi hook record`: `hook.record_and_contract`
//! and `hook.record_lifecycle_event`. Both always return the host's allow
//! contract. Every write is best-effort, as in Python: an error, or an
//! input this port does not model, skips the write and never changes the
//! contract.

use super::logs::{harness_of, Tree};
use super::paths::{path_join, path_parent, path_str};
use super::pyjson::{dumps, loads, py_str_or_empty, Object, Value};
use super::pystr::{py_head, py_strip};
use super::record::safe_session_value;
use super::state_report::{report_child_env, report_state_env, transcript_path_for_report};
use super::{best_effort, now_float, Runner, R};
use std::collections::HashMap;
use std::sync::OnceLock;
use std::time::Instant;

/// `hook._BLOCKING_NOTIFICATIONS`.
const BLOCKING: &[&str] = &[
    "permission_prompt",
    "elicitation_dialog",
    "elicitation_url_dialog",
    "agent_needs_input",
];

/// Runs `daisugi hook record` on `raw`, the hook's stdin, and returns the
/// contract the host reads on stdout. `event` is one of the five events
/// the command takes; `captures_root` is the --captures-root text.
pub fn hook_record(
    raw: &[u8],
    captures_root: &str,
    fmt: &str,
    event: &str,
    env: &HashMap<String, String>,
) -> String {
    let root = path_str(captures_root);
    let r = Runner {
        env: env.clone(),
        home: env.get("HOME").cloned().unwrap_or_default(),
        wd: OnceLock::new(),
        real_cache: Default::default(),
        root: String::new(),
        default_root: String::new(),
        mode: String::new(),
        fmt: fmt.to_string(),
        session: None,
        captures: None,
        verify_timeout_s: 0.0,
        ask: false,
        ask_timeout_s: 0.0,
        checkpoints: false,
        t0: Instant::now(),
        resident: false,
        caller: Default::default(),
    };
    // A panic in a write is the oracle's swallowed exception.
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| match event {
        "stop" | "notification" | "subagent_start" | "subagent_stop" => {
            let sessions = path_join(&path_parent(&root), "sessions");
            let _ = best_effort(r.lifecycle(raw, event, &sessions));
        }
        _ => {
            if let Some(Value::Obj(p)) = hook_payload(raw).filter(|v| v.truthy()) {
                let _ = r.capture(&root, &p);
            }
        }
    }));
    super::stdout_for(fmt, false, "")
}

/// `json.loads(raw.decode("utf-8", "replace"))` when the text is not
/// blank, else {}; `None` when json.loads raises.
fn hook_payload(raw: &[u8]) -> Option<Value> {
    let text = String::from_utf8_lossy(raw).into_owned();
    if py_strip(&text).is_empty() {
        return Some(Value::Obj(Object::new()));
    }
    loads(&text).ok()
}

impl Runner {
    /// `record_lifecycle_event`'s report and session-tree entry.
    fn lifecycle(&self, raw: &[u8], event: &str, sessions: &str) -> R<()> {
        let v = hook_payload(raw).unwrap_or(Value::Obj(Object::new()));
        if let Some(state) = match event {
            "subagent_start" => Some("working"),
            "subagent_stop" => Some("done"),
            _ => None,
        } {
            // hook._report_subagent.
            if let Value::Obj(p) = &v {
                if let Value::Str(id) = p.value("agent_id") {
                    if !id.is_empty() {
                        let label = match p.value("agent_type") {
                            Value::Str(s) => s.as_str(),
                            _ => "",
                        };
                        report_child_env(&self.env, id, state, label);
                    }
                }
            }
            return Ok(());
        }
        let p = match &v {
            Value::Obj(p) if p.value("session_id").truthy() => p,
            _ => return Ok(()),
        };
        let sid = safe_session_value(p.value("session_id"))?;
        let (state, detail, ask) = if event == "stop" {
            ("idle", "session stop".to_string(), None)
        } else {
            let message = py_str_or_empty(p.value("message"))?;
            let nt = p.value("notification_type");
            let is_permission = if nt.truthy() {
                match nt {
                    // `in` a frozenset raises TypeError on an unhashable
                    // value: nothing is reported.
                    Value::List(_) | Value::Obj(_) => return Ok(()),
                    Value::Str(s) => BLOCKING.contains(&s.as_str()),
                    _ => false,
                }
            } else {
                super::py::text::lower(&message).contains("permission")
            };
            if is_permission {
                let tool = if nt.truthy() {
                    py_str_or_empty(nt)?
                } else {
                    "notification".to_string()
                };
                let ask = Object::new()
                    .with("id", "harness")
                    .with("tool", tool.as_str())
                    .with("summary", message.as_str())
                    .with("deadline", now_float() + 90.0);
                (
                    "blocked",
                    py_head(&format!("notification: {message}"), 200).to_string(),
                    Some(ask),
                )
            } else if message.is_empty() {
                ("idle", "notification".to_string(), None)
            } else {
                (
                    "idle",
                    py_head(&format!("notification: {message}"), 200).to_string(),
                    None,
                )
            }
        };
        let mut ev = Object::new()
            .with("v", 1i64)
            .with("ts", now_float())
            .with("session_id", sid.as_str())
            .with("harness_session_id", p.value("session_id").clone())
            .with("harness", harness_of(&self.fmt))
            .with("pane", Value::Null)
            .with("state", state)
            .with("source", "headless")
            .with("detail", py_head(&detail, 200));
        if let Some(a) = ask {
            ev.set("ask", a);
        }
        if let Some(tp) = transcript_path_for_report(p.value("transcript_path")) {
            ev.set("transcript_path", tp);
        }
        report_state_env(&self.env, &ev);
        let _ = best_effort((|| -> R<()> {
            let header = Object::new()
                .with("v", 1i64)
                .with("harness", harness_of(&self.fmt))
                .with("cwd", py_str_or_empty(p.value("cwd"))?)
                .with("harnessSessionId", p.value("session_id").clone())
                .with("transcriptPath", p.value("transcript_path").clone())
                .with("parentSession", Value::Null)
                .with("parentEntry", Value::Null)
                .with("cacheKey", Value::Null);
            let mut t = Tree::open_or_create(sessions, &sid, header)?;
            // json.loads(ev_json): the event as the wire carries it.
            let ev2 = match loads(&dumps(&Value::Obj(ev.clone()), true)) {
                Ok(Value::Obj(o)) => o,
                _ => ev.clone(),
            };
            t.append("state", &ev2, None)?;
            Ok(())
        })());
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Write};
    use std::os::unix::net::UnixListener;

    /// A fresh scratch directory of the test's own.
    fn scratch(name: &str) -> String {
        let d = std::env::temp_dir().join(format!("dg-hookrec-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d.to_str().unwrap().to_string()
    }

    fn env(home: &str) -> HashMap<String, String> {
        HashMap::from([
            ("HOME".to_string(), home.to_string()),
            ("PATH".to_string(), "/nonexistent".to_string()),
        ])
    }

    #[test]
    fn records_a_call_and_answers_the_contract() {
        let d = scratch("record");
        let h = d.as_str();
        let root = format!("{h}/caps");
        let out = hook_record(
            br#"{"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "ls"}}"#,
            &root,
            "claude",
            "pre_tool_use",
            &env(h),
        );
        assert_eq!(out, r#"{"continue": true}"#);
        let text = std::fs::read_to_string(format!("{root}/s1.jsonl")).unwrap();
        assert!(text.contains(r#""command": "ls""#), "{text}");
    }

    #[test]
    fn never_fails_on_its_input() {
        let d = scratch("odd");
        let h = d.as_str();
        let deep = "[".repeat(5000);
        for input in [
            "",
            "{",
            "[1]",
            r#"{"tool_name": 5}"#,
            "\u{fffd}",
            deep.as_str(),
        ] {
            for ev in [
                "pre_tool_use",
                "stop",
                "notification",
                "subagent_start",
                "subagent_stop",
            ] {
                assert_eq!(
                    hook_record(input.as_bytes(), &format!("{h}/c"), "hermes", ev, &env(h)),
                    "{}"
                );
            }
        }
    }

    #[test]
    fn a_stop_reports_to_coppice_and_writes_the_tree() {
        let h = scratch("stop");
        let sock = format!("{h}/c.sock");
        let ln = UnixListener::bind(&sock).unwrap();
        let t = std::thread::spawn(move || {
            let (mut c, _) = ln.accept().unwrap();
            let mut line = String::new();
            BufReader::new(c.try_clone().unwrap())
                .read_line(&mut line)
                .unwrap();
            c.write_all(b"{\"ok\": true}\n").unwrap();
            line
        });
        let mut e = env(&h);
        e.insert("COPPICE_SOCK".into(), sock.clone());
        e.insert("COPPICE_PANE".into(), "w1:p2".into());
        hook_record(
            br#"{"session_id": "s9"}"#,
            &format!("{h}/.opendaisugi/captures"),
            "claude",
            "stop",
            &e,
        );
        let line = t.join().unwrap();
        for want in [
            r#""cmd": "pane.report_state""#,
            r#""pane": "w1:p2""#,
            r#""source": "headless""#,
        ] {
            assert!(line.contains(want), "{line}");
        }
        assert!(std::path::Path::new(&format!("{h}/.opendaisugi/sessions/s9.jsonl")).exists());
    }
}
