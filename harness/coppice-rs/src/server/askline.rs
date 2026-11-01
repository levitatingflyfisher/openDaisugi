//! The ask an idle agent's last message ends on, for the floor's "needs you"
//! cards.

use std::time::UNIX_EPOCH;

use super::facts::Kind;
use super::messages::{is_hermes, one_line, read_messages, Message};
use super::Server;
use crate::{layout, state};

/// Caps the one-line ask a pane.list row carries.
const ASK_RUNES: usize = 200;

/// The ask a transcript ended on, in one line, or "". The last message must be
/// the agent's own words (not a tool call, and nothing from the owner after
/// it), and its last line must end in a question mark once markup is trimmed.
pub fn ask_line_of(msgs: &[Message]) -> String {
    for m in msgs.iter().rev() {
        if m.role == "note" {
            continue;
        }
        if m.role != "agent" || !m.tool.is_empty() {
            return String::new();
        }
        let text = m.text.trim();
        let last = text.rsplit('\n').next().unwrap_or("").trim();
        let core = last.trim_end_matches(|c| " *_`\"')]".contains(c));
        if core.ends_with('?') {
            return one_line(last, ASK_RUNES);
        }
        return String::new();
    }
    String::new()
}

impl Server {
    /// The ask pane `p`'s last message ends on, for a pane that waits idle.
    /// It reads a transcript file again only when the file changed. A pane
    /// with no file the server can read, a Hermes pane included, has none.
    pub(super) fn last_ask_line(&self, p: &layout::Pane, st: &str) -> Option<String> {
        if st != state::STATE_IDLE || p.closed || p.harness.is_empty() || is_hermes(p) || self.is_tracked_steward(&p.id) {
            return None;
        }
        let (path, kind) = self.transcript_of(p);
        if path.is_empty() || kind == Kind::Hermes {
            return None;
        }
        let md = std::fs::metadata(&path).ok().filter(|m| m.is_file())?;
        let size = md.len() as i64;
        let mod_ns = md
            .modified()
            .ok()
            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
            .map(|d| d.as_nanos() as i64)
            .unwrap_or(0);
        if let Some((s, m, line)) = self.ask_lines.lock().unwrap_or_else(|e| e.into_inner()).get(&path) {
            if *s == size && *m == mod_ns {
                return (!line.is_empty()).then(|| line.clone());
            }
        }
        let (msgs, _) = read_messages(&path, kind).ok()?;
        let line = ask_line_of(&msgs);
        let mut cache = self.ask_lines.lock().unwrap_or_else(|e| e.into_inner());
        if cache.len() > 256 {
            cache.clear();
        }
        cache.insert(path, (size, mod_ns, line.clone()));
        (!line.is_empty()).then_some(line)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn m(role: &str, text: &str, tool: &str) -> Message {
        Message { role: role.into(), text: text.into(), tool: tool.into(), ..Default::default() }
    }

    #[test]
    fn it_reads_the_question_the_agent_ended_on() {
        let a = |t: &str| m("agent", t, "");
        assert_eq!(ask_line_of(&[m("owner", "go", ""), a("Done.\nShould I push this now?")]), "Should I push this now?");
        assert_eq!(ask_line_of(&[a("Which one do you want, **A or B?**")]), "Which one do you want, **A or B?**");
        assert_eq!(ask_line_of(&[a("All done.")]), "");
        assert_eq!(ask_line_of(&[a("Why? Because it was slow.")]), "");
        assert_eq!(ask_line_of(&[a("Shall I go on?"), m("owner", "yes", "")]), "");
        assert_eq!(ask_line_of(&[a("Shall I go on?"), m("agent", "ls", "Bash")]), "");
        assert_eq!(ask_line_of(&[a("Shall I go on?"), m("note", "auto: x", "")]), "Shall I go on?");
        assert_eq!(ask_line_of(&[]), "");
        let long = format!("{}?", "word ".repeat(100));
        assert!(ask_line_of(&[a(&long)]).chars().count() <= ASK_RUNES + 1);
    }
}
