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
    /// The key this backend sends, for redaction; empty when none.
    fn held_key(&self) -> Vec<u8> {
        Vec::new()
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
    /// Caps the tokens the run may use, when cap_tokens is set: before
    /// each model call the loop stops once the four usage counts of its
    /// turns, plus extra_tokens, reach it. One turn may overshoot.
    pub max_tokens: i64,
    pub cap_tokens: bool,
    /// The tokens spent outside this loop's own turns (the branches of
    /// the children it started). None is none.
    pub extra_tokens: Option<Box<dyn Fn() -> i64>>,
    /// Where the loop keeps its own tokens and model turns so far, for the
    /// spawn tool; None keeps them nowhere.
    pub own_use: Option<Rc<std::cell::Cell<(i64, i64)>>>,
    /// A run that offers the model no tool (--tools none): a reply that
    /// calls one fails the run.
    pub no_tools: bool,
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
            max_tokens: 0,
            cap_tokens: false,
            extra_tokens: None,
            own_use: None,
            no_tools: false,
            clarified: false,
        }
    }

    fn note_own_use(&self) {
        if let Some(c) = &self.own_use {
            let extra = self.extra_tokens.as_ref().map_or(0, |f| f());
            c.set((self.tokens_used().wrapping_sub(extra), self.model_turns()));
        }
    }

    /// The four usage counts of every assistant turn so far, plus
    /// extra_tokens.
    pub fn tokens_used(&self) -> i64 {
        let mut n = 0i64;
        for m in self.history.iter().filter(|m| m.role == "assistant") {
            let u = &m.usage;
            for c in [u.fresh, u.out, u.cache_read, u.cache_write] {
                n = sat_add(n, c);
            }
        }
        sat_add(n, self.extra_tokens.as_ref().map_or(0, |f| f()))
    }

    /// The number of assistant turns in the history.
    pub fn model_turns(&self) -> i64 {
        self.history
            .iter()
            .filter(|m| m.role == "assistant")
            .count() as i64
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
            if self.cap_tokens {
                let used = self.tokens_used();
                if used >= self.max_tokens {
                    return Err(
                        format!("used {used} tokens of the {} allowed", self.max_tokens)
                            .into_bytes(),
                    );
                }
            }
            let mut msg = self.model.next(&self.history)?;
            let u = &msg.usage;
            if u.fresh < 0 || u.out < 0 || u.cache_read < 0 || u.cache_write < 0 {
                return Err(b"the model reported a negative token count".to_vec());
            }
            for c in &mut msg.calls {
                if c.id.is_empty() {
                    c.id = new_id();
                }
            }
            self.history.push(msg.clone());
            self.note_own_use();
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
            if self.no_tools {
                for c in &msg.calls {
                    if !self.exec.has(&c.name) {
                        let q = crate::goerr::quote(c.name.as_bytes());
                        let why = if self.exec.tool_names().is_empty() {
                            "and this run offers no tools"
                        } else {
                            "which this run does not offer"
                        };
                        return Err(format!("the model called the tool {q}, {why}").into_bytes());
                    }
                }
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

/// Adds a count to a sum of counts: a negative count adds nothing, and
/// the sum holds at i64::MAX instead of wrapping (Go's satAdd).
pub fn sat_add(a: i64, b: i64) -> i64 {
    if b <= 0 {
        a
    } else {
        a.saturating_add(b)
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
    fn a_negative_count_fails_the_turn_and_sums_saturate() {
        for u in [
            Usage {
                out: -1000000,
                ..Default::default()
            },
            Usage {
                cache_read: -1,
                ..Default::default()
            },
            Usage {
                fresh: -5,
                out: 9,
                ..Default::default()
            },
            Usage {
                cache_write: -1,
                ..Default::default()
            },
        ] {
            let mut t = text("x");
            t.usage = u;
            let (mut a, _) = agent(vec![t], 9);
            assert_eq!(
                a.run(b"go").unwrap_err(),
                b"the model reported a negative token count"
            );
        }
        let huge = (1i64 << 62) - 1;
        let mut call = calling("read");
        call.usage = Usage {
            fresh: huge,
            out: huge,
            cache_read: huge,
            cache_write: huge,
        };
        let (mut a, seen) = agent(vec![call.clone(), call.clone(), call], 9);
        a.max_tokens = 100;
        a.cap_tokens = true;
        assert_eq!(
            a.run(b"go").unwrap_err(),
            b"used 9223372036854775807 tokens of the 100 allowed"
        );
        assert_eq!(seen.borrow().len(), 1);
    }

    #[test]
    fn token_cap_follows_go() {
        let mut call = calling("read");
        call.usage = Usage {
            fresh: 3,
            out: 4,
            cache_read: 2,
            cache_write: 1,
        };
        let (mut a, seen) = agent(vec![call.clone(), call.clone(), call], 9);
        a.max_tokens = 15;
        a.cap_tokens = true;
        assert_eq!(
            a.run(b"go").unwrap_err(),
            b"used 20 tokens of the 15 allowed"
        );
        assert_eq!(seen.borrow().len(), 2);
        let (mut a, seen) = agent(vec![text("x")], 9);
        a.cap_tokens = true;
        assert_eq!(a.run(b"go").unwrap_err(), b"used 0 tokens of the 0 allowed");
        assert!(seen.borrow().is_empty());
        let (mut a, _) = agent(vec![text("x")], 9);
        a.cap_tokens = true;
        a.max_tokens = 5;
        a.extra_tokens = Some(Box::new(|| 5));
        assert_eq!(a.run(b"go").unwrap_err(), b"used 5 tokens of the 5 allowed");
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
