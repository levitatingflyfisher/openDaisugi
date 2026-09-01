//! `approval.py`: who approves a step before it runs.

use std::collections::HashMap;
use std::io::{BufRead, Write};

use super::str_of;
use crate::gate::py::text::{lower, repr, split, strip};
use crate::gate::pyjson::{Object, Value};

/// `approval.ApprovalDecision`.
#[derive(Debug, Clone)]
pub struct Decision {
    pub approved: bool,
    pub approved_by: String,
    pub reason: String,
}

fn decision(approved: bool, by: &str, reason: impl Into<String>) -> Decision {
    Decision { approved, approved_by: by.into(), reason: reason.into() }
}

/// `approval.ApprovalStrategy`. An error is an exception the strategy
/// raised, worded "Type: message".
pub trait Approver {
    fn decide(&mut self, step: &Object, env: &Object) -> Result<Decision, String>;
}

/// `CallbackStrategy(lambda step, env: True)`, the orchestrator's approval.
pub struct Always;

impl Approver for Always {
    fn decide(&mut self, _: &Object, _: &Object) -> Result<Decision, String> {
        Ok(decision(true, "callback", "user-supplied callback approved"))
    }
}

/// `approval.default_strategy()`: the allowlist, then DAISUGI_APPROVE,
/// then a prompt on a terminal, then deny.
pub struct DefaultApproval {
    pub env: HashMap<String, String>,
    /// Whether stdin and stdout are both terminals.
    pub terminal: bool,
}

const VALID_APPROVE: [&str; 4] = ["always", "never", "interactive", "auto"];

impl Approver for DefaultApproval {
    fn decide(&mut self, step: &Object, env: &Object) -> Result<Decision, String> {
        if str_of(step, "type") == "shell" {
            let command = str_of(step, "command");
            let cmd = strip(&command);
            if !cmd.is_empty() && !crate::verify::shell_metachar_hit(cmd) {
                let first = split(cmd)[0].to_string();
                if let Value::Obj(perms) = env.value("permissions") {
                    if let Value::List(allow) = perms.value("shell_allowlist") {
                        if allow.iter().any(|a| a.as_str() == Some(first.as_str())) {
                            return Ok(decision(true, "allowlist", format!("'{first}' is in shell_allowlist")));
                        }
                    }
                }
            }
        }
        let raw = self.env.get("DAISUGI_APPROVE").cloned().unwrap_or_default();
        let value = lower(strip(&raw));
        if !value.is_empty() {
            if !VALID_APPROVE.contains(&value.as_str()) {
                let mut sorted: Vec<String> = VALID_APPROVE.iter().map(|s| s.to_string()).collect();
                sorted.sort();
                return Err(format!(
                    "ValueError: DAISUGI_APPROVE={} is not a valid value; expected one of {}",
                    repr(&value),
                    crate::gate::py::text::repr_list(&sorted)
                ));
            }
            match value.as_str() {
                "always" => return Ok(decision(true, "env", "DAISUGI_APPROVE=always")),
                "never" => return Ok(decision(false, "env", "DAISUGI_APPROVE=never")),
                _ => {}
            }
        }
        if !self.terminal {
            return Ok(decision(
                false,
                "denied",
                "no TTY available; set DAISUGI_APPROVE=always for non-interactive approval",
            ));
        }
        let mut what = str_of(step, "command");
        for k in ["path", "url", "id"] {
            if what.is_empty() {
                what = str_of(step, k);
            }
        }
        let mut out = std::io::stdout();
        let _ = write!(out, "Approve step {} ({what})? [y/N] ", repr(&str_of(step, "id")));
        let _ = out.flush();
        let mut line = String::new();
        let n = std::io::stdin().lock().read_line(&mut line).unwrap_or(0);
        if n == 0 {
            // input() at end of file raises EOFError.
            return Err("EOFError: EOF when reading a line".into());
        }
        let answer = lower(strip(line.strip_suffix('\n').unwrap_or(&line)));
        let ok = answer == "y" || answer == "yes";
        Ok(decision(ok, "tty", format!("user answered {}", repr(&answer))))
    }
}

/// Whether a file descriptor is a terminal.
pub fn is_terminal(fd: i32) -> bool {
    unsafe { libc::isatty(fd) == 1 }
}
