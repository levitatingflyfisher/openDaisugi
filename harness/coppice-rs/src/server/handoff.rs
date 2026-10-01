//! Moving a conversation to another loop (F12b), the Rust twin of
//! handoff.go. agent.move reads the transcript the way pane.messages does,
//! builds a brief from its tail with no tool rows, and starts a new agent of
//! the chosen harness beside the old one, in the same directory. The old
//! agent stays until the owner closes it.

use std::sync::Arc;

use serde_json::{json, Map, Value};

use super::messages::{one_line, read_messages, Message};
use super::swap::{harness_word, swap_refusal};
use super::{role, Client, Server};
use crate::adapters;
use crate::config;
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::sys;

/// What a pane, a plugin and an unplaced peer get for agent.move.
pub const MOVE_REFUSAL: &str = "only the operator moves an agent's conversation.";

/// The most runes of message text a brief carries, and the most messages.
/// The preface is not counted.
const BRIEF_RUNES: usize = 12000;
const BRIEF_MESSAGES: usize = 40;

/// The first prompt of the agent a conversation moves to: a preface, then
/// the newest owner and agent texts that fit the budget, oldest first. A
/// row with a tool name is left out whole, even its one-line input, so no
/// command or path of a tool reaches the new agent. Returns the text, how
/// many messages went in, and whether older ones were left out.
pub fn build_brief(from: &str, cwd: &str, msgs: &[Message]) -> (String, usize, bool) {
    let keep: Vec<&Message> = msgs
        .iter()
        .filter(|m| {
            m.tool.is_empty() && (m.role == "owner" || m.role == "agent") && !m.text.is_empty()
        })
        .collect();
    let mut runes = 0;
    let mut first = keep.len();
    while first > 0 && keep.len() - first < BRIEF_MESSAGES {
        let n = keep[first - 1].text.chars().count();
        if runes + n > BRIEF_RUNES {
            break;
        }
        runes += n;
        first -= 1;
    }
    let trimmed = first > 0;
    let keep = &keep[first..];
    if keep.is_empty() {
        return (String::new(), 0, trimmed);
    }
    let mut b = String::new();
    b.push_str(&format!(
        "You take over a conversation from another coding agent ({}) in this directory: {}.\n",
        one_line(from, 64),
        one_line(cwd, 400)
    ));
    b.push_str("Its latest messages follow, oldest first. Tool output is left out, so read the files you need. ");
    b.push_str("Do not repeat finished work. When you have read this, say in one line where things stand, then wait for the owner.\n");
    for m in keep {
        b.push_str(&format!("\n{}: {}\n", m.role, m.text));
    }
    (b, keep.len(), trimmed)
}

/// The size of text in tokens, about: four bytes to a token.
pub fn brief_tokens(text: &str) -> usize {
    text.len().div_ceil(4)
}

/// The kind of pane a harness starts as: a terminal agent when
/// coppice.toml has a table for it, else a headless one when an adapter is
/// built.
fn move_kind(harness: &str) -> Option<&'static str> {
    if let Ok(Some(cfg)) = config::load() {
        if cfg
            .harness
            .get(harness)
            .is_some_and(|h| !h.command.is_empty())
        {
            return Some("pty");
        }
    }
    adapters::get(harness).map(|_| "headless")
}

/// A request built from a verb name and its params.
fn request(id: &str, cmd: &str, params: Vec<(&str, Value)>) -> Request {
    let mut m = Map::new();
    m.insert("id".into(), json!(id));
    m.insert("cmd".into(), json!(cmd));
    for (k, v) in params {
        m.insert(k.into(), v);
    }
    proto::decode_line(&Value::Object(m).to_string()).0
}

/// A reason with a full stop on it.
fn end_sentence(s: &str) -> String {
    let s = s.trim();
    if s.is_empty() || s.ends_with('.') {
        s.to_string()
    } else {
        format!("{s}.")
    }
}

impl Server {
    /// agent.move {pane, harness, preview?}. With preview it says what a
    /// move would carry and starts nothing. Without, it starts a new agent
    /// of harness in the pane's directory and sends it the brief. The brief
    /// waits in the ready queue of a terminal agent, as any prompt does.
    pub(super) fn handle_move(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        // The brief is the chat's text, so a named token gets what
        // pane.messages gives it: a refusal, preview included.
        if c.token_named() {
            return proto::err_resp(
                &r.id,
                proto::ERR_UNAUTHORIZED,
                super::messages::CHAT_REFUSAL,
            );
        }
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        let harness = r.s("harness");
        if !role::valid_harness(&harness) {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "harness must be a name of at most {} letters, digits, '.', '_' or '-', as in coppice.toml.",
                    role::HARNESS_MAX
                ),
            );
        }
        let preview = r.bool("preview").unwrap_or(false);
        if !preview && self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a new pane.",
            );
        }
        let Some(rec) = self.tree.pane(&id) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice pane list", sys::go_quote(&id)),
            );
        };
        let label = self.label_of(&id);
        if rec.closed {
            return proto::err_resp(
                &r.id,
                proto::ERR_PANE_CLOSED,
                &format!("{label} has ended."),
            );
        }
        if rec.harness.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &swap_refusal("loop", &label, &harness_word("")),
            );
        }
        if rec.harness == harness {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{label} already runs on {harness}."),
            );
        }
        let Some(kind) = move_kind(&harness) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "no harness named {} in {}. Run coppice once to write it, or add [harness.{}] with command = {}.",
                    sys::go_quote(&harness),
                    config::path().display(),
                    harness,
                    sys::go_quote(&harness)
                ),
            );
        };
        let (path, tkind) = self.transcript_of(&rec);
        if path.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{label} ({id}) has no transcript to move. Its hook reported none."),
            );
        }
        let Ok((msgs, _)) = read_messages(&path, tkind) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!("cannot read the transcript of {label} ({id})."),
            );
        };
        let (brief, used, trimmed) = build_brief(&rec.harness, &rec.cwd, &msgs);
        if used == 0 {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{label} has no conversation to move yet. Wait for its first reply."),
            );
        }
        let mut res = vec![
            ("from", json!(rec.harness)),
            ("harness", json!(harness)),
            ("tokens", json!(brief_tokens(&brief))),
            ("messages", json!(used)),
            ("trimmed", json!(trimmed)),
        ];
        if let Some(conv) = self.conversation_tokens(&rec) {
            res.push(("conversation", json!(conv)));
        }
        if preview {
            return proto::ok_resp(&r.id, gojson::map(res));
        }

        let mut create = vec![
            ("cwd", json!(rec.cwd)),
            ("harness", json!(harness)),
            ("kind", json!(kind)),
            ("workspace", json!(rec.workspace)),
            ("tab", json!(rec.tab)),
            ("cols", json!(rec.cols)),
            ("rows", json!(rec.rows)),
        ];
        if !rec.task_id.is_empty() {
            create.push(("task", json!(rec.task_id)));
        }
        let made = self.pane_create(Some(c), &request(&r.id, "pane.create", create));
        if !made.ok {
            return made;
        }
        let m = made.result.clone().unwrap_or(Value::Null);
        let new_id = m
            .get("pane")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();
        let sent = self.send_brief(c, &r.id, &new_id, &harness, &brief);
        if !sent.ok {
            return sent;
        }
        res.push(("pane", json!(new_id)));
        res.push((
            "workspace",
            m.get("workspace").cloned().unwrap_or(Value::Null),
        ));
        res.push(("tab", m.get("tab").cloned().unwrap_or(Value::Null)));
        // "now": the agent has the brief. "typed": a terminal agent with no
        // screen rule got it typed at once, and nothing says it landed.
        let mut when = json!("now");
        if kind == "pty" && !self.gated(&new_id) {
            when = json!("typed");
        }
        if let Some(p) = sent.result.as_ref() {
            if let Some(q) = p.get("queued") {
                when = json!("queued");
                res.push(("queued", q.clone()));
                res.push(("note", p.get("note").cloned().unwrap_or(Value::Null)));
            }
        }
        res.push(("when", when));
        proto::ok_resp(&r.id, gojson::map(res))
    }
}

impl Server {
    /// Sends the brief to the new agent new_id. When the agent cannot take
    /// it, the agent goes and the reply says so: a blank start is not what
    /// the owner asked for (F12B-R-6). Returns the prompt's reply on success.
    pub(super) fn send_brief(
        self: &Arc<Server>,
        c: &Arc<Client>,
        id: &str,
        new_id: &str,
        harness: &str,
        brief: &str,
    ) -> Response {
        let sent = self.handle_agent_prompt(
            c,
            &request(
                id,
                "agent.prompt",
                vec![("pane", json!(new_id)), ("text", json!(brief))],
            ),
        );
        if sent.ok {
            return sent;
        }
        if let Some(nrec) = self.tree.pane(new_id) {
            self.close_record(&nrec);
        }
        let why = sent.error.as_ref().map(|e| e.1.clone()).unwrap_or_default();
        proto::err_resp(
            id,
            proto::ERR_ADAPTER,
            &format!(
                "{harness} could not take the brief, so no new agent started: {}",
                end_sentence(&why)
            ),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn m(role: &str, text: &str, tool: &str) -> Message {
        Message {
            role: role.into(),
            text: text.into(),
            tool: tool.into(),
            ..Default::default()
        }
    }

    #[test]
    fn the_brief_keeps_the_newest_within_its_budget_and_no_tool_rows() {
        let mut msgs = Vec::new();
        for i in 0..60 {
            let role = if i % 2 == 0 { "owner" } else { "agent" };
            msgs.push(m(role, &format!("{} #{}", "w".repeat(400), i), ""));
            if i % 3 == 0 {
                msgs.push(m("agent", "TOOL-ROW", "Bash"));
            }
        }
        let (text, used, trimmed) = build_brief("claude", "/work/x", &msgs);
        assert!(trimmed && used < 60 && used >= 10, "used {used}");
        assert!(!text.contains("TOOL-ROW"));
        assert!(text.chars().count() < BRIEF_RUNES + 1000);
        assert!(text.trim_end().ends_with("#59"));
        assert!(text.contains("/work/x") && text.contains("(claude)"));
        let (_, used, _) = build_brief("claude", "/w", &[m("agent", "", "Bash")]);
        assert_eq!(used, 0);
    }

    #[test]
    fn a_token_is_about_four_bytes() {
        assert_eq!(brief_tokens(""), 0);
        assert_eq!(brief_tokens("abcd"), 1);
        assert_eq!(brief_tokens("abcde"), 2);
    }
}
