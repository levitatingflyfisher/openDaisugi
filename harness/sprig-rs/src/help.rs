//! The help tool: a worker asks its parent, or stops. A valid call records
//! the kind and the text and ends the run; the warden is not asked about
//! it, since it has no effect in the world.

use crate::json::{Any, Map};
use crate::warden::Tool;
use std::cell::RefCell;
use std::rc::Rc;

/// The three kinds of the help tool, in the order the descriptions name them.
pub const HELP_KINDS: &[&str] = &["need_info", "escalate", "give_up"];

/// What the model reads about the help tool.
pub const HELP_DESCRIPTION: &str = "Ask your parent for help, or stop. kind is need_info, escalate or give_up; text is your message. Call it when you are stuck after a few tries, when the brief lacks a fact you need, when the next step needs the owner's decision, or when the task cannot be done inside your envelope. Never guess past a missing fact: ask. need_info: ask one clear question; the run pauses and goes on when the answer comes. escalate: the same, for a decision that belongs to the owner; it goes to the owner. give_up: stop now; text is your final report, with what you changed.";

/// The kind and the text of a help call that ended the run.
pub type HelpCall = Rc<RefCell<Option<(String, Vec<u8>)>>>;

pub struct HelpTool {
    pub call: HelpCall,
}

impl Tool for HelpTool {
    fn unwardened(&self) -> bool {
        true
    }

    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        let str_of = |k: &str| -> Vec<u8> {
            match input.as_ref().and_then(|m| m.get(k)) {
                Some(Any::Str(s)) => s.clone().into_bytes(),
                _ => Vec::new(),
            }
        };
        let kind = str_of("kind");
        let text = crate::gostr::trim_space(&str_of("text")).to_vec();
        let Some(kind) = HELP_KINDS.iter().find(|k| k.as_bytes() == kind.as_slice()) else {
            return (
                Vec::new(),
                Some(format!("help: kind is one of {}", HELP_KINDS.join(", ")).into_bytes()),
            );
        };
        if text.is_empty() {
            return (
                Vec::new(),
                Some(b"help: text is empty. Say what you need or what you did".to_vec()),
            );
        }
        if self.call.borrow().is_some() {
            return (
                Vec::new(),
                Some(b"help: you already called help in this run".to_vec()),
            );
        }
        *self.call.borrow_mut() = Some((kind.to_string(), text));
        if *kind == "give_up" {
            return (b"Your report was sent. The run ends now.".to_vec(), None);
        }
        (
            b"Your question was sent. The run pauses here. The answer comes as the next message."
                .to_vec(),
            None,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent::{Agent, Message};
    use crate::warden::{Executor, ToolCall, Verdict, Warden};
    use std::collections::BTreeMap;

    fn input(kind: &str, text: &str) -> Option<Map> {
        let mut m = Map::new();
        m.insert("kind".into(), Any::Str(kind.into()));
        m.insert("text".into(), Any::Str(text.into()));
        Some(m)
    }

    #[test]
    fn a_bad_kind_or_no_text_is_an_error_the_model_sees() {
        let call: HelpCall = Rc::new(RefCell::new(None));
        let t = HelpTool { call: call.clone() };
        assert_eq!(
            t.run(&input("shout", "x")).1.unwrap(),
            b"help: kind is one of need_info, escalate, give_up"
        );
        assert_eq!(
            t.run(&input("need_info", "  ")).1.unwrap(),
            b"help: text is empty. Say what you need or what you did"
        );
        assert!(call.borrow().is_none());
        assert!(t.run(&input("give_up", " done ")).1.is_none());
        assert_eq!(call.borrow().clone().unwrap(), ("give_up".to_string(), b"done".to_vec()));
        assert_eq!(
            t.run(&input("need_info", "again")).1.unwrap(),
            b"help: you already called help in this run"
        );
    }

    struct DenyAll;
    impl Warden for DenyAll {
        fn check(&self, _: &ToolCall) -> Verdict {
            Verdict {
                allow: false,
                reason: b"no".to_vec(),
            }
        }
    }

    struct One(Option<Message>);
    impl crate::agent::Model for One {
        fn next(&mut self, _: &[Message]) -> Result<Message, Vec<u8>> {
            Ok(self.0.take().expect("asked twice"))
        }
    }

    #[test]
    fn help_ends_the_run_and_skips_the_warden() {
        let call: HelpCall = Rc::new(RefCell::new(None));
        let mut tools: BTreeMap<String, Box<dyn Tool>> = BTreeMap::new();
        tools.insert("help".into(), Box::new(HelpTool { call: call.clone() }));
        // A warden that denies everything: help must still run.
        let exec = Executor::new(tools, Box::new(DenyAll));
        let msg = Message {
            role: "assistant",
            calls: vec![ToolCall {
                name: "help".into(),
                input: input("escalate", "owner call"),
                ..Default::default()
            }],
            ..Default::default()
        };
        let mut a = Agent::new(Box::new(One(Some(msg))), exec, 5);
        a.help = Some(call.clone());
        assert_eq!(a.run(b"task").unwrap(), b"owner call");
        assert_eq!(call.borrow().clone().unwrap().0, "escalate");
    }
}
