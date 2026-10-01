//! The chat window on the desk: the foreman's window draws the messages of
//! floor.chat, with an input line of its own that sends floor.talk. The
//! header word Screen shows the foreman's terminal, and Chat returns.

use serde_json::json;

use crate::textwidth;

use super::model::{s, Model, Obj};
use super::render::{pad, wrap_words, SGR_ACCENT, SGR_AMBER, SGR_FAINT, SGR_RESET};
use super::run::{call, list_of, params, Floor};

/// One line of the chat with the foreman, from floor.chat. Role is owner,
/// agent, note, work or past. Tool is set on a tool use line.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct ChatMsg {
    pub id: String,
    pub role: String,
    pub text: String,
    pub tool: String,
}

/// How many messages one floor.chat read asks for.
pub const CHAT_LIMIT: i64 = 50;

/// The foot line of the foreman's window while its input line is empty.
pub const CHAT_PLACEHOLDER: &str = "Tell the foreman. Type and press Enter.";

/// What the foreman's window says before any message.
pub const CHAT_EMPTY: &str = "No messages yet. Type a sentence below. It goes to the foreman.";

/// What the foreman's window says when the server refuses floor.chat: it
/// counts a process inside one of its own panes as a pane, and a pane may
/// not read the chat.
pub const CHAT_REFUSED: &str = "The server will not show this chat here. This floor runs inside a coppice pane, and a pane may not read the chat or talk to the foreman. Run the floor outside a coppice pane.";

/// The state word of the foreman's rail row while it holds a reply and no
/// window shows it.
pub const REPLY_MARK: &str = "reply";

/// The header words that switch the foreman's window between its chat and
/// its terminal.
pub const SCREEN_WORD: &str = "Screen";
pub const CHAT_WORD: &str = "Chat";

impl Model {
    /// True when pane id is the foreman and its window draws the chat.
    pub fn chat_window(&self, id: &str) -> bool {
        !id.is_empty() && id == self.foreman_id && !self.chat_screen && self.has(id)
    }

    /// True when pane id's window takes whole lines in an input line of its
    /// own: a headless agent, and the foreman's chat.
    pub fn takes_line(&self, id: &str) -> bool {
        self.is_headless(id) || self.chat_window(id)
    }

    /// The header word of the foreman's window: Screen while it draws the
    /// chat, Chat while it draws the terminal. Other windows have none.
    pub fn toggle_word(&self, id: &str) -> &'static str {
        if id.is_empty() || id != self.foreman_id || !self.has(id) {
            ""
        } else if self.chat_screen {
            CHAT_WORD
        } else {
            SCREEN_WORD
        }
    }

    /// True when pane id is the foreman and holds a reply no window showed,
    /// so it fills right after the agents that need you.
    pub fn foreman_first(&self, id: &str) -> bool {
        !id.is_empty() && id == self.foreman_id && self.chat_unseen() && !self.chat_screen
    }

    /// True when pane id is the foreman with an unseen reply and no window
    /// to show it. Its rail row says so.
    pub fn reply_waiting(&self, id: &str) -> bool {
        self.foreman_first(id) && !self.tiles.as_ref().is_some_and(|t| t.slot_of(id).is_some())
    }

    /// True when the foreman has a reply no chat window showed.
    pub fn chat_unseen(&self) -> bool {
        let last = last_reply(&self.chat);
        !self.foreman_id.is_empty() && !last.is_empty() && last != self.chat_seen
    }
}

/// The id of the newest message the foreman wrote in words, or "". A tool
/// use is not a reply, and neither is the owner's line.
pub fn last_reply(msgs: &[ChatMsg]) -> String {
    msgs.iter()
        .rev()
        .find(|m| m.role == "agent" && m.tool.is_empty() && !m.id.is_empty())
        .map(|m| m.id.clone())
        .unwrap_or_default()
}

/// Reads the messages of a floor.chat reply.
pub fn decode_chat(res: &Obj) -> Vec<ChatMsg> {
    list_of(res, "messages")
        .iter()
        .map(|d| ChatMsg {
            id: s(d, "id"),
            role: s(d, "role"),
            text: s(d, "text"),
            tool: s(d, "tool"),
        })
        .collect()
}

fn style_who(who: &str, line: String) -> String {
    if who.is_empty() || !line.starts_with(who) {
        return line;
    }
    format!("{SGR_ACCENT}{who}{SGR_RESET}{}", &line[who.len()..])
}

/// The chat as w-cell lines, oldest first: one block per message, its
/// speaker on the first line. A tool use is one dim line.
pub fn chat_rows(msgs: &[ChatMsg], w: i64) -> Vec<String> {
    let mut out = Vec::new();
    for c in msgs {
        let who = match c.role.as_str() {
            "owner" => "you ",
            "agent" => "foreman ",
            _ => "",
        };
        if !c.tool.is_empty() {
            let t = textwidth::printable(&c.tool, 0);
            out.push(format!(
                "{SGR_FAINT}{}{SGR_RESET}",
                pad(&textwidth::truncate(&format!("  · {t}"), w), w)
            ));
            continue;
        }
        let text = textwidth::printable(&c.text, 0);
        if text.is_empty() {
            continue;
        }
        let who_w = textwidth::width(who) as i64;
        let room = (w - who_w).max(4);
        for (i, l) in wrap_words(&text, room, 1 << 10).iter().enumerate() {
            let lead = if i == 0 {
                who.to_string()
            } else {
                " ".repeat(who_w as usize)
            };
            let line = pad(&textwidth::truncate(&format!("{lead}{l}"), w), w);
            let line = if i == 0 && !who.is_empty() {
                style_who(who, line)
            } else if c.role == "note" || c.role == "past" {
                format!("{SGR_FAINT}{line}{SGR_RESET}")
            } else {
                line
            };
            out.push(line);
        }
    }
    out
}

/// The body of the foreman's window: the newest messages that fit h lines,
/// w cells wide.
pub fn chat_lines(m: &Model, w: i64, h: i64) -> Vec<String> {
    if h <= 0 {
        return Vec::new();
    }
    let mut rows = chat_rows(&m.chat, w);
    if !m.chat_err.is_empty() && m.chat.is_empty() {
        rows = wrap_words(CHAT_REFUSED, w, 1 << 10)
            .iter()
            .map(|l| {
                format!(
                    "{SGR_AMBER}{}{SGR_RESET}",
                    pad(&textwidth::truncate(l, w), w)
                )
            })
            .collect();
    }
    if rows.is_empty() {
        rows = vec![format!(
            "{SGR_FAINT}{}{SGR_RESET}",
            pad(&textwidth::truncate(CHAT_EMPTY, w), w)
        )];
    }
    if rows.len() as i64 > h {
        rows.drain(..rows.len() - h as usize);
    }
    rows
}

impl Floor<'_> {
    /// Turns the foreman's window between its chat and its terminal. The
    /// caller gives the keys to the window after it.
    pub fn toggle_screen(&mut self) {
        self.m.chat_screen = !self.m.chat_screen;
    }

    /// Asks floor.chat for the newest messages. A failed read keeps the
    /// last chat. With no foreman it reads nothing. A reply no window
    /// showed then claims a window.
    pub fn read_chat(&mut self) {
        if self.m.foreman_id.is_empty() {
            self.m.chat.clear();
            return;
        }
        let res = match call(
            &self.o.socket,
            "floor.chat",
            params(vec![("limit", json!(CHAT_LIMIT))]),
        ) {
            Ok(r) => r,
            Err(e) => {
                if e.text().contains("only the operator") {
                    self.m.chat_err = CHAT_REFUSED.to_string();
                }
                return;
            }
        };
        self.m.chat = decode_chat(&res);
        self.m.chat_err.clear();
    }

    /// Names pane id as the floor's foreman, or none for "". The chat and
    /// the Screen choice start over. A window that has the keys and now
    /// takes lines, or no longer does, gets its key reader set again.
    pub fn set_foreman(&mut self, id: &str) {
        let old = std::mem::replace(&mut self.m.foreman_id, id.to_string());
        self.m.chat_screen = false;
        self.m.chat.clear();
        self.m.chat_err.clear();
        let typing = self.m.typing.clone();
        if !typing.is_empty() && (typing == old || typing == id) {
            self.set_typing(&typing);
        }
    }
}
