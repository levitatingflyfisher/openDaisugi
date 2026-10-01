//! The Recent fold: the agents that ended on their own, and the one line
//! the floor shows when one ends.

use std::time::{Duration, Instant};

use crate::textwidth;

use super::model::{boolean, children_of, num, s, Item, ItemKind, Model, Obj, Row};
use super::render::{
    age_text, pad, CLOSE_MARK, PROMPT_MARK, SGR_AMBER, SGR_FAINT, SGR_RESET, SGR_REVERSE,
};

/// How long the ended line of an agent that ended well stays.
pub const ENDED_CLEAR: Duration = Duration::from_secs(10);

impl Model {
    /// The ended agents the rail lists.
    pub fn recent(&self) -> Vec<Row> {
        let sc = self.scope();
        if sc.is_empty() {
            return self.ended.clone();
        }
        let keep = self.under(&sc);
        self.ended
            .iter()
            .filter(|r| keep.contains(&r.task))
            .cloned()
            .collect()
    }

    /// Every cursor stop on the rail, in order.
    pub fn items(&self) -> Vec<Item> {
        let mut out = Vec::new();
        let rows = self.visible();
        for g in self.groups() {
            let folded = self.folded.get(&g.key).copied().unwrap_or(false);
            let grows = g.rows.clone();
            out.push(Item {
                kind: ItemKind::Group,
                row: Row::default(),
                group: g,
            });
            if folded {
                continue;
            }
            for r in grows {
                let kids = children_of(&rows, &r.id);
                out.push(Item {
                    kind: ItemKind::Row,
                    row: r,
                    group: Default::default(),
                });
                for c in kids {
                    out.push(Item {
                        kind: ItemKind::Row,
                        row: c,
                        group: Default::default(),
                    });
                }
            }
        }
        let ended = self.recent();
        if ended.is_empty() {
            return out;
        }
        out.push(Item::of(ItemKind::Fold));
        if !self.recent_open {
            return out;
        }
        out.push(Item::of(ItemKind::ResumeAll));
        out.push(Item::of(ItemKind::ClearAll));
        for r in ended {
            out.push(Item {
                kind: ItemKind::Ended,
                row: r,
                group: Default::default(),
            });
        }
        out
    }

    /// Keeps the cursor off a group header it was not moved onto.
    pub fn norm(&mut self, all: &[Item]) {
        if self.at_header || self.cursor < 0 || self.cursor + 1 >= all.len() as i64 {
            return;
        }
        let c = self.cursor as usize;
        if all[c].kind == ItemKind::Group && all[c + 1].kind == ItemKind::Row {
            self.cursor += 1;
        }
    }

    /// Records whether the cursor now sits on a group header on purpose.
    pub fn landed(&mut self) {
        let all = self.items();
        self.at_header = self.cursor >= 0
            && self.cursor < all.len() as i64
            && all[self.cursor as usize].kind == ItemKind::Group;
    }

    /// The cursor stop under the cursor.
    pub fn selected_item(&mut self) -> Option<Item> {
        let all = self.items();
        self.norm(&all);
        if self.cursor < 0 || self.cursor >= all.len() as i64 {
            return None;
        }
        Some(all[self.cursor as usize].clone())
    }

    /// The key of the item under the cursor, or "".
    pub fn selected_key(&mut self) -> String {
        self.selected_item().map(|it| it.key()).unwrap_or_default()
    }

    /// Replaces the ended agents with a fresh pane.list ended true.
    pub fn apply_ended(&mut self, list: &[Obj], now: f64) {
        let keep = self.selected_key();
        self.ended = ended_rows_from(list, now);
        self.cursor = self.index_of(&keep, self.cursor);
    }

    /// Shows the ended line for r. A non-zero exit is sticky.
    pub fn note_ended(&mut self, r: &Row) {
        let sticky = matches!(r.exit_code, Some(c) if c != 0);
        if self.ended_sticky && !sticky {
            return;
        }
        self.ended_line = ended_text(r);
        self.ended_sticky = sticky;
        self.ended_at = Some(Instant::now());
    }

    /// Clears an ended line that is not sticky once it has shown for
    /// ENDED_CLEAR.
    pub fn expire_ended(&mut self, now: Instant) -> bool {
        if self.ended_line.is_empty() || self.ended_sticky {
            return false;
        }
        if let Some(at) = self.ended_at {
            if now.saturating_duration_since(at) < ENDED_CLEAR {
                return false;
            }
        }
        self.clear_ended();
        true
    }

    /// The label of pane id, live or ended, or the id.
    pub fn name_of(&self, id: &str) -> String {
        if self.has(id) {
            return self.label_of(id);
        }
        for r in &self.ended {
            if r.id == id {
                return self.label_of_ended(r);
            }
        }
        id.to_string()
    }

    /// An ended row's label, or its id.
    pub fn label_of_ended(&self, r: &Row) -> String {
        if !r.label.is_empty() {
            return textwidth::printable(&r.label, 0);
        }
        r.id.clone()
    }

    /// Puts the rail cursor on the Recent entry of pane id.
    pub fn cursor_to_ended(&mut self, id: &str) {
        if !self.recent().iter().any(|r| r.id == id) {
            return;
        }
        let key = if self.recent_open {
            format!("ended:{id}")
        } else {
            "recent".to_string()
        };
        self.cursor = self.index_of(&key, self.cursor);
    }

    /// The order the windows fill in when the floor opens.
    pub fn start_order(&self) -> Vec<String> {
        let rows = self.tree_rows();
        let mut out = Vec::new();
        let picks: [&dyn Fn(&Row) -> bool; 3] = [
            &|r: &Row| r.needs_you(),
            &|r: &Row| !r.needs_you() && r.state == "working",
            &|r: &Row| !r.needs_you() && r.state != "working",
        ];
        for pick in picks {
            for r in &rows {
                if r.parent.is_empty() && pick(r) {
                    out.push(r.id.clone());
                }
            }
        }
        out
    }

    /// Runs on every key.
    pub fn key_pressed(&mut self) {
        self.expire_ended(Instant::now());
    }

    pub fn clear_ended(&mut self) {
        self.ended_line.clear();
        self.ended_sticky = false;
    }

    /// Opens or closes the Recent fold.
    pub fn toggle_recent(&mut self) {
        let keep = self.selected_key();
        self.recent_open = !self.recent_open;
        if self.recent_open {
            self.clear_ended();
        }
        self.cursor = self.index_of(&keep, self.cursor);
    }
}

/// Turns a pane.list with ended true into rows.
pub fn ended_rows_from(list: &[Obj], now: f64) -> Vec<Row> {
    let mut out = Vec::new();
    for p in list {
        if boolean(p, "closed") != Some(true) {
            continue;
        }
        let mut r = Row {
            id: s(p, "id"),
            label: s(p, "label"),
            harness: s(p, "harness"),
            worktree: s(p, "cwd"),
            state: "ended".into(),
            task: s(p, "task"),
            ..Default::default()
        };
        if let Some(code) = num(p, "exit_code") {
            r.exit_code = Some(code as i64);
        }
        if let Some(at) = num(p, "ended_at") {
            if now > at {
                r.age = now - at;
            }
        }
        out.push(r);
    }
    out
}

/// The one line the floor shows when an agent ends on its own.
pub fn ended_text(r: &Row) -> String {
    let mut label = textwidth::printable(&r.label, 0);
    if label.is_empty() {
        label = r.id.clone();
    }
    match r.exit_code {
        None => format!("{label} ended"),
        Some(c) => format!("{label} ended ({})", exit_text(c)),
    }
}

/// How an exit code reads.
pub fn exit_text(code: i64) -> String {
    if code < 0 {
        return "killed".into();
    }
    format!("exit {code}")
}

/// How one Recent item draws on the rail.
pub fn recent_line(m: &Model, it: &Item, cursor: bool, focused: bool, cols: i64) -> String {
    let mark = if cursor { PROMPT_MARK } else { "  " };
    let mut color = SGR_FAINT.to_string();
    let text = match it.kind {
        ItemKind::Fold => {
            let arrow = if m.recent_open { "▾" } else { "▸" };
            color.clear();
            format!("{arrow} Recent ({})", m.recent().len())
        }
        ItemKind::ResumeAll => {
            color.clear();
            "  Resume all".into()
        }
        ItemKind::ClearAll => {
            color.clear();
            "  Clear all".into()
        }
        _ => {
            let mut label = textwidth::printable(&it.row.label, 0);
            if label.is_empty() {
                label = it.row.id.clone();
            }
            let exit = match it.row.exit_code {
                Some(c) => exit_text(c),
                None => "ended".into(),
            };
            if matches!(it.row.exit_code, Some(c) if c != 0) {
                color = SGR_AMBER.into();
            }
            format!(
                "  {}",
                [label, exit, format!("{} ago", age_text(it.row.age))].join("  ")
            )
        }
    };
    let mut line = format!("  {mark}{text}");
    if it.kind == ItemKind::Ended {
        line = format!(
            "{} {CLOSE_MARK}",
            pad(&textwidth::truncate(&line, cols - 2), cols - 2)
        );
    }
    if cursor && focused {
        color.push_str(SGR_REVERSE);
    }
    format!(
        "{color}{}{SGR_RESET}",
        pad(&textwidth::truncate(&line, cols), cols)
    )
}
