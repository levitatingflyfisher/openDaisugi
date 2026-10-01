//! The loop: ask the model, run any tools it asks for through the gate,
//! feed the results back, until it answers in plain text or the turn
//! budget runs out.

use crate::gate::{result_text, Executor, ToolCall};
use crate::session::{new_id, SessionObserver, Usage};
use std::rc::Rc;

/// Sent once when a reply is neither a clean tool call nor a plausible
/// final answer.
pub const CLARIFY_NUDGE: &str =
    "Your last reply was not a valid tool call and not a final answer. \
Reply with ONLY a ```sprig-tool block to call a tool, or your final answer as plain text.";

/// One turn of the conversation.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Message {
    /// "user", "assistant" or "tool".
    pub role: &'static str,
    pub text: Vec<u8>,
    pub calls: Vec<ToolCall>,
    pub model: String,
    pub usage: Usage,
}

/// The model: given the history, the next assistant turn, or an error.
pub trait Model {
    fn next(&mut self, history: &[Message]) -> Result<Message, Vec<u8>>;
    /// The model this backend asks for; empty when it does not say.
    fn model_name(&self) -> String {
        String::new()
    }
}

/// A model, a gated executor and a turn budget.
pub struct Agent {
    pub model: Box<dyn Model>,
    pub exec: Executor,
    pub max_turns: i64,
    pub session: Option<Rc<dyn SessionObserver>>,
    /// The history a resumed session starts from.
    pub seed: Vec<Message>,
    pub history: Vec<Message>,
    clarified: bool,
}

/// Whether a reply with no call is too unclear to accept: empty, or a
/// botched tool call.
pub fn needs_clarification(text: &[u8]) -> bool {
    let t = crate::gostr::trim_space(text);
    if t.is_empty() {
        return true;
    }
    [b"```sprig-tool".as_slice(), b"```tool"]
        .iter()
        .any(|f| crate::gostr::index(t, f).is_some())
}

impl Agent {
    pub fn new(model: Box<dyn Model>, exec: Executor, max_turns: i64) -> Self {
        Agent {
            model,
            exec,
            max_turns,
            session: None,
            seed: Vec::new(),
            history: Vec::new(),
            clarified: false,
        }
    }

    /// Runs the task to an answer, or to an error.
    pub fn run(&mut self, task: &[u8]) -> Result<Vec<u8>, Vec<u8>> {
        self.history = self.seed.clone();
        self.history.push(Message {
            role: "user",
            text: task.to_vec(),
            ..Default::default()
        });
        if let Some(s) = &self.session {
            s.on_prompt(task);
        }
        let mut turn: i64 = 0;
        while turn < self.max_turns {
            turn += 1;
            let mut msg = self.model.next(&self.history)?;
            for c in &mut msg.calls {
                if c.id.is_empty() {
                    c.id = new_id();
                }
            }
            self.history.push(msg.clone());
            if let Some(s) = &self.session {
                let uses: Vec<(String, String)> = msg
                    .calls
                    .iter()
                    .map(|c| (c.id.clone(), c.name.clone()))
                    .collect();
                s.on_assistant(&msg.model, &msg.text, &msg.usage, &uses);
            }
            if msg.calls.is_empty() {
                if !self.clarified && needs_clarification(&msg.text) {
                    self.clarified = true;
                    self.history.push(Message {
                        role: "user",
                        text: CLARIFY_NUDGE.as_bytes().to_vec(),
                        ..Default::default()
                    });
                    continue;
                }
                return Ok(msg.text);
            }
            self.clarified = false;
            for call in &msg.calls {
                let res = self.exec.execute(call);
                self.history.push(Message {
                    role: "tool",
                    text: result_text(&res),
                    ..Default::default()
                });
            }
        }
        Err(format!(
            "sprig: gave up after {} turns without finishing",
            self.max_turns
        )
        .into_bytes())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::{AllowAll, Tool};
    use crate::json::Map;
    use std::collections::BTreeMap;

    struct Script(Vec<Message>, Rc<std::cell::RefCell<Vec<usize>>>);
    impl Model for Script {
        fn next(&mut self, history: &[Message]) -> Result<Message, Vec<u8>> {
            self.1.borrow_mut().push(history.len());
            if self.0.is_empty() {
                return Err(b"no more".to_vec());
            }
            Ok(self.0.remove(0))
        }
    }

    struct Ok1;
    impl Tool for Ok1 {
        fn run(&self, _: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
            (b"out".to_vec(), None)
        }
    }

    fn text(t: &str) -> Message {
        Message {
            role: "assistant",
            text: t.as_bytes().to_vec(),
            ..Default::default()
        }
    }

    fn calling(name: &str) -> Message {
        let call = ToolCall {
            name: name.into(),
            input: Some(Map::new()),
            ..Default::default()
        };
        Message {
            role: "assistant",
            calls: vec![call],
            ..Default::default()
        }
    }

    fn agent(replies: Vec<Message>, turns: i64) -> (Agent, Rc<std::cell::RefCell<Vec<usize>>>) {
        let seen = Rc::new(std::cell::RefCell::new(Vec::new()));
        let mut tools: BTreeMap<String, Box<dyn Tool>> = BTreeMap::new();
        tools.insert("read".into(), Box::new(Ok1));
        let exec = Executor::new(tools, Box::new(AllowAll));
        (
            Agent::new(Box::new(Script(replies, seen.clone())), exec, turns),
            seen,
        )
    }

    #[test]
    fn clarify_rules() {
        assert!(needs_clarification(b"  \n"));
        assert!(needs_clarification(b"x ```tool y"));
        assert!(!needs_clarification(b"```json {}```"));
        assert!(!needs_clarification(b"done"));
    }

    #[test]
    fn loop_runs_tools_and_nudges_once() {
        let (mut a, seen) = agent(vec![calling("read"), text(""), text(""), text("end")], 10);
        assert_eq!(a.run(b"task").unwrap(), b"");
        assert_eq!(*seen.borrow(), vec![1, 3, 5]);
        assert_eq!(a.history.len(), 6);
        assert_eq!(a.history[2].text, b"out");
        assert_eq!(a.history[4].text, CLARIFY_NUDGE.as_bytes());
        assert_eq!(a.history[1].calls[0].id.len(), 8);
    }

    #[test]
    fn loop_gives_up() {
        let (mut a, _) = agent(vec![calling("read"), calling("zap")], 2);
        assert_eq!(
            a.run(b"t").unwrap_err(),
            b"sprig: gave up after 2 turns without finishing"
        );
        assert_eq!(a.history[4].text, b"REFUSED by the gate: unknown tool: zap");
        let (mut a, _) = agent(vec![], -1);
        assert_eq!(
            a.run(b"t").unwrap_err(),
            b"sprig: gave up after -1 turns without finishing"
        );
        let (mut a, _) = agent(vec![], 3);
        assert_eq!(a.run(b"t").unwrap_err(), b"no more");
    }
}
