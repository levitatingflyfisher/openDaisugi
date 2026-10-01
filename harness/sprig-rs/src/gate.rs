//! The boundary: the executor runs a tool call only when the tool is known
//! and the gate allows it. Every other path refuses without side effects.
//! The gate itself is a separate process, `daisugi gate check`, which reads
//! a hook payload on stdin and allows with exit 0; anything else denies.

use crate::gostr::{index, trim_space};
use crate::json::{encode, jmap, jmap_any, Map, J};
use crate::session::SessionObserver;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::time::{Duration, Instant};

/// A model's request to run one tool. `id` is the tool_use id.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    /// None is a nil map: absent from the payload, it is written `null`.
    pub input: Option<Map>,
}

/// A gate's ruling: allow, or refuse with a reason.
#[derive(Debug, Clone, PartialEq)]
pub struct Verdict {
    pub allow: bool,
    pub reason: Vec<u8>,
}

/// Rules on a proposed call before it runs. Fail-closed.
pub trait Gate {
    fn check(&self, call: &ToolCall) -> Verdict;
}

/// The gate off: every call allowed.
pub struct AllowAll;

impl Gate for AllowAll {
    fn check(&self, _: &ToolCall) -> Verdict {
        Verdict {
            allow: true,
            reason: Vec::new(),
        }
    }
}

/// The --gate-cmd default of every entry point.
pub const DEFAULT_GATE_CMD: &str = "daisugi gate check --mode enforce";

/// The gate as a separate process.
pub struct DaisugiGate {
    pub command: Vec<Vec<u8>>,
    pub timeout: Duration,
    /// Names the run on every payload; empty sends none.
    pub session_id: Vec<u8>,
}

/// sprig's tool names in the words the envelope knows.
pub fn gate_tool_name(name: &str) -> &str {
    match name {
        "read" => "Read",
        "write" => "Write",
        "edit" => "Edit",
        "bash" => "Bash",
        other => other,
    }
}

/// The gate's verdict line out of its output: the last stderr line with
/// "openDaisugi gate:", else the last non-empty stderr line, else stdout.
pub fn gate_reason(stderr: &[u8], stdout: &[u8]) -> Vec<u8> {
    let lines: Vec<&[u8]> = trim_space(stderr).split(|&c| c == b'\n').collect();
    for l in lines.iter().rev() {
        let l = trim_space(l);
        if index(l, b"openDaisugi gate:").is_some() {
            return l.to_vec();
        }
    }
    for l in lines.iter().rev() {
        let l = trim_space(l);
        if !l.is_empty() {
            return l.to_vec();
        }
    }
    trim_space(stdout).to_vec()
}

impl DaisugiGate {
    /// The hook payload the gate reads on stdin.
    pub fn payload(&self, call: &ToolCall) -> Vec<u8> {
        let mut body = vec![
            ("tool_name", J::s(gate_tool_name(&call.name))),
            ("tool_input", jmap_any(&call.input)),
        ];
        if let Some(wd) = crate::osx::getwd() {
            body.push(("cwd", J::Str(wd)));
        }
        if !self.session_id.is_empty() {
            body.push(("session_id", J::s(&self.session_id)));
        }
        encode(&jmap(body))
    }
}

impl Gate for DaisugiGate {
    fn check(&self, call: &ToolCall) -> Verdict {
        let deny = |r: &[u8]| Verdict {
            allow: false,
            reason: r.to_vec(),
        };
        if self.command.is_empty() {
            return deny(b"no gate command configured (fail-closed)");
        }
        let payload = self.payload(call);
        let timeout = if self.timeout.is_zero() {
            Duration::from_secs(5)
        } else {
            self.timeout
        };
        let ran = crate::osx::Run {
            argv: &self.command,
            stdin: Some(&payload),
            combined: false,
            dir: None,
            timeout: Some(timeout),
        }
        .run();
        if ran.timed_out {
            return deny("gate timed out \u{2014} fail-closed".as_bytes());
        }
        let Some(err) = ran.err else {
            return Verdict {
                allow: true,
                reason: Vec::new(),
            };
        };
        if self.command[0] == b"daisugi" && ran.not_found {
            return deny(
                b"daisugi is not on PATH, so every tool call is denied. \
Install it: scripts/install.sh, or see the README quick start.",
            );
        }
        let mut reason = gate_reason(&ran.stderr, &ran.stdout);
        if reason.is_empty() {
            reason = format!("refused by the gate ({err})").into_bytes();
        }
        Verdict {
            allow: false,
            reason,
        }
    }
}

/// One capability the agent can invoke. It returns its output and, when it
/// failed, the error text.
pub trait Tool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>);
}

/// The outcome of a call through the gate.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Res {
    pub id: String,
    pub allowed: bool,
    pub output: Vec<u8>,
    pub reason: Vec<u8>,
    pub err: Option<Vec<u8>>,
}

/// What the model sees for a result: the output, a REFUSED line, or the
/// error and then the output.
pub fn result_text(r: &Res) -> Vec<u8> {
    if !r.allowed {
        return [b"REFUSED by the gate: ".as_slice(), &r.reason].concat();
    }
    match &r.err {
        Some(e) => [b"error: ".as_slice(), e, b"\n", &r.output].concat(),
        None => r.output.clone(),
    }
}

/// The guarded loop step.
pub struct Executor {
    tools: BTreeMap<String, Box<dyn Tool>>,
    gate: Box<dyn Gate>,
    pub session: Option<Rc<dyn SessionObserver>>,
}

impl Executor {
    pub fn new(tools: BTreeMap<String, Box<dyn Tool>>, gate: Box<dyn Gate>) -> Self {
        Executor {
            tools,
            gate,
            session: None,
        }
    }

    /// Runs the call only if the tool is known and the gate allows it, and
    /// reports the call, the verdict and the result to the session tree.
    pub fn execute(&self, call: &ToolCall) -> Res {
        let t0 = Instant::now();
        if let Some(s) = &self.session {
            s.on_tool_call(&call.id, &call.name, &call.input);
        }
        let refuse = |reason: Vec<u8>| {
            let res = Res {
                id: call.id.clone(),
                allowed: false,
                reason,
                ..Default::default()
            };
            if let Some(s) = &self.session {
                s.on_verdict(&call.id, false, &res.reason, t0.elapsed());
                s.on_tool_result(&call.id, false, &result_text(&res));
            }
            res
        };
        let Some(tool) = self.tools.get(&call.name) else {
            return refuse(format!("unknown tool: {}", call.name).into_bytes());
        };
        let v = self.gate.check(call);
        if !v.allow {
            return refuse(v.reason);
        }
        if let Some(s) = &self.session {
            s.on_verdict(&call.id, true, b"", t0.elapsed());
        }
        let (output, err) = tool.run(&call.input);
        let ok = err.is_none();
        let res = Res {
            id: call.id.clone(),
            allowed: true,
            output,
            reason: Vec::new(),
            err,
        };
        if let Some(s) = &self.session {
            s.on_tool_result(&call.id, ok, &result_text(&res));
        }
        res
    }

    /// The tool names, sorted.
    pub fn tool_names(&self) -> Vec<String> {
        self.tools.keys().cloned().collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::json::Any;
    use std::cell::RefCell;

    #[test]
    fn names_and_reasons() {
        assert_eq!(gate_tool_name("bash"), "Bash");
        assert_eq!(gate_tool_name("Write"), "Write");
        assert_eq!(gate_tool_name("zap"), "zap");
        let err = b"RuntimeWarning: x\nopenDaisugi gate: DENIED \xe2\x80\x94 no\n  tail  \n\n";
        assert_eq!(
            gate_reason(err, b"out"),
            "openDaisugi gate: DENIED \u{2014} no".as_bytes()
        );
        assert_eq!(gate_reason(b"first\n  second  \n\n", b"out"), b"second");
        assert_eq!(gate_reason(b"", b"  out  \n"), b"out");
    }

    #[test]
    fn payload_is_a_sorted_map() {
        let mut m = Map::new();
        m.insert("cmd".into(), Any::Str("ls <x>".into()));
        let g = DaisugiGate {
            command: vec![],
            timeout: Duration::ZERO,
            session_id: "s1".into(),
        };
        let call = ToolCall {
            id: "1".into(),
            name: "bash".into(),
            input: Some(m),
        };
        let p = String::from_utf8(g.payload(&call)).unwrap();
        assert!(p.starts_with("{\"cwd\":\""), "{p}");
        assert!(p.ends_with(
            "\"session_id\":\"s1\",\"tool_input\":{\"cmd\":\"ls \\u003cx\\u003e\"},\"tool_name\":\"Bash\"}"
        ));
        let g = DaisugiGate {
            command: vec![],
            timeout: Duration::ZERO,
            session_id: Vec::new(),
        };
        let call = ToolCall {
            id: "1".into(),
            name: "x".into(),
            input: None,
        };
        let p = String::from_utf8(g.payload(&call)).unwrap();
        assert!(
            p.ends_with("\"tool_input\":null,\"tool_name\":\"x\"}"),
            "{p}"
        );
    }

    #[test]
    fn process_gate_fails_closed() {
        let call = ToolCall {
            name: "read".into(),
            ..Default::default()
        };
        let gate = |cmd: &[&str]| DaisugiGate {
            command: cmd.iter().map(|s| s.as_bytes().to_vec()).collect(),
            timeout: Duration::from_secs(5),
            session_id: Vec::new(),
        };
        assert!(gate(&["true"]).check(&call).allow);
        let v = gate(&[]).check(&call);
        assert_eq!(v.reason, b"no gate command configured (fail-closed)");
        let v = gate(&["sh", "-c", "exit 3"]).check(&call);
        assert_eq!(v.reason, b"refused by the gate (exit status 3)");
        let v = gate(&["no-such-gate-here"]).check(&call);
        assert_eq!(
            v.reason,
            b"refused by the gate (exec: \"no-such-gate-here\": executable file not found in $PATH)"
        );
        let mut slow = gate(&["sleep", "3"]);
        slow.timeout = Duration::from_millis(200);
        assert_eq!(
            slow.check(&call).reason,
            "gate timed out \u{2014} fail-closed".as_bytes()
        );
    }

    struct Echo;
    impl Tool for Echo {
        fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
            match input.as_ref().and_then(|m| m.get("fail")) {
                Some(_) => (b"partial".to_vec(), Some(b"boom".to_vec())),
                None => (b"ran".to_vec(), None),
            }
        }
    }

    struct DenyBash;
    impl Gate for DenyBash {
        fn check(&self, call: &ToolCall) -> Verdict {
            Verdict {
                allow: call.name != "bash",
                reason: b"no bash".to_vec(),
            }
        }
    }

    #[derive(Default)]
    struct Log(RefCell<Vec<String>>);
    impl SessionObserver for Log {
        fn on_prompt(&self, _: &[u8]) {}
        fn on_assistant(
            &self,
            _: &str,
            _: &[u8],
            _: &crate::session::Usage,
            _: &[(String, String)],
        ) {
        }
        fn on_tool_call(&self, id: &str, name: &str, _: &Option<Map>) {
            self.0.borrow_mut().push(format!("call {id} {name}"));
        }
        fn on_verdict(&self, id: &str, allow: bool, reason: &[u8], _: Duration) {
            self.0.borrow_mut().push(format!(
                "verdict {id} {allow} {}",
                String::from_utf8_lossy(reason)
            ));
        }
        fn on_tool_result(&self, id: &str, ok: bool, summary: &[u8]) {
            self.0.borrow_mut().push(format!(
                "result {id} {ok} {}",
                String::from_utf8_lossy(summary)
            ));
        }
    }

    #[test]
    fn executor_refuses_and_reports() {
        let mut tools: BTreeMap<String, Box<dyn Tool>> = BTreeMap::new();
        tools.insert("bash".into(), Box::new(Echo));
        tools.insert("read".into(), Box::new(Echo));
        let log = Rc::new(Log::default());
        let mut ex = Executor::new(tools, Box::new(DenyBash));
        ex.session = Some(log.clone());
        let c = |id: &str, name: &str| ToolCall {
            id: id.into(),
            name: name.into(),
            input: None,
        };
        let r = ex.execute(&c("a", "zap"));
        assert_eq!(result_text(&r), b"REFUSED by the gate: unknown tool: zap");
        let r = ex.execute(&c("b", "bash"));
        assert!(!r.allowed);
        let r = ex.execute(&c("c", "read"));
        assert_eq!(result_text(&r), b"ran");
        let mut m = Map::new();
        m.insert("fail".into(), Any::Null);
        let r = ex.execute(&ToolCall {
            id: "d".into(),
            name: "read".into(),
            input: Some(m),
        });
        assert_eq!(result_text(&r), b"error: boom\npartial");
        assert_eq!(
            *log.0.borrow(),
            vec![
                "call a zap",
                "verdict a false unknown tool: zap",
                "result a false REFUSED by the gate: unknown tool: zap",
                "call b bash",
                "verdict b false no bash",
                "result b false REFUSED by the gate: no bash",
                "call c read",
                "verdict c true ",
                "result c true ran",
                "call d read",
                "verdict d true ",
                "result d false error: boom\npartial",
            ]
        );
        assert_eq!(ex.tool_names(), vec!["bash", "read"]);
    }
}
