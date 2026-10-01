//! The task tree: the tree view on the floor and `coppice task list
//! --tree`.

use std::collections::{HashMap, HashSet};

use crate::textwidth;

use super::model::{num, s, Obj, Row};
use super::railtree::base_name;

/// One task as task.list reports it.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct TaskRow {
    pub id: String,
    pub label: String,
    pub parent: String,
    pub cwd: String,
    pub worktree: String,
    pub model: String,
    pub foreman: String,
    pub state: String,
    pub panes: Vec<String>,
    pub ahead: i64,
    pub has_ahead: bool,
}

/// The open tree: its lines, the task id each line stands for or "" for a
/// pane line, and the cursor, which rests on a task line.
#[derive(Clone, Debug, Default)]
pub struct TreeView {
    pub lines: Vec<String>,
    pub task_at: Vec<String>,
    pub cursor: i64,
}

/// Turns task.list rows into TaskRows.
pub fn decode_tasks(list: &[Obj]) -> Vec<TaskRow> {
    list.iter()
        .map(|t| {
            let mut r = TaskRow {
                id: s(t, "id"),
                label: s(t, "label"),
                parent: s(t, "parent"),
                cwd: s(t, "cwd"),
                worktree: s(t, "worktree"),
                model: s(t, "model"),
                foreman: s(t, "foreman"),
                state: s(t, "state"),
                ..Default::default()
            };
            if let Some(serde_json::Value::Array(raw)) = t.get("panes") {
                for p in raw {
                    if let Some(id) = p.as_str() {
                        r.panes.push(id.to_string());
                    }
                }
            }
            if let Some(n) = num(t, "ahead") {
                r.ahead = n as i64;
                r.has_ahead = true;
            }
            r
        })
        .collect()
}

/// The dot and word a tree line ends with.
pub fn state_mark(state: &str) -> String {
    match state {
        "blocked" => "● needs you".into(),
        "working" => "● working".into(),
        "idle" => "○ idle".into(),
        "done" => "· done".into(),
        "" => String::new(),
        other => format!("· {other}"),
    }
}

fn task_text(t: &TaskRow) -> String {
    let mut parts = vec![if t.label.is_empty() {
        t.id.clone()
    } else {
        t.label.clone()
    }];
    if !t.worktree.is_empty() {
        parts.push(base_name(&t.worktree));
    }
    if t.has_ahead {
        parts.push(format!("+{}", t.ahead));
    }
    let mark = state_mark(&t.state);
    if !mark.is_empty() {
        parts.push(mark);
    }
    textwidth::printable(&parts.join("  "), 0)
}

fn pane_text(r: &Row) -> String {
    let mut label = textwidth::printable(&r.label, 0);
    if label.is_empty() {
        label = r.id.clone();
    }
    let mark = state_mark(&r.state);
    if !mark.is_empty() {
        return format!("{label}  {mark}");
    }
    label
}

struct Walk<'a> {
    by_id: HashMap<&'a str, &'a TaskRow>,
    children: HashMap<&'a str, Vec<&'a str>>,
    pane_rows: HashMap<&'a str, &'a Row>,
    seen: HashSet<&'a str>,
    cols: i64,
    lines: Vec<String>,
    task_at: Vec<String>,
}

impl<'a> Walk<'a> {
    fn walk(&mut self, id: &'a str, indent: &str, last: bool) {
        if !self.seen.insert(id) {
            return;
        }
        let t = self.by_id.get(id).copied();
        let empty = TaskRow::default();
        let t = t.unwrap_or(&empty);
        let (branch, deeper) = if last {
            ("└─ ", "   ")
        } else {
            ("├─ ", "│  ")
        };
        self.lines.push(textwidth::truncate(
            &format!("{indent}{branch}{}", task_text(t)),
            self.cols,
        ));
        self.task_at.push(id.to_string());
        let panes_here: Vec<&Row> = t
            .panes
            .iter()
            .filter_map(|pid| self.pane_rows.get(pid.as_str()).copied())
            .collect();
        let kids = self.children.get(id).cloned().unwrap_or_default();
        for (i, r) in panes_here.iter().enumerate() {
            let b = if i == panes_here.len() - 1 && kids.is_empty() {
                "└─ "
            } else {
                "├─ "
            };
            self.lines.push(textwidth::truncate(
                &format!("{indent}{deeper}{b}{}", pane_text(r)),
                self.cols,
            ));
            self.task_at.push(String::new());
        }
        let n = kids.len();
        for (i, kid) in kids.into_iter().enumerate() {
            self.walk(kid, &format!("{indent}{deeper}"), i == n - 1);
        }
    }
}

/// One line per task and one per pane under its task, depth first.
pub fn tree_lines(tasks: &[TaskRow], panes: &[Row], cols: i64) -> (Vec<String>, Vec<String>) {
    let mut w = Walk {
        by_id: HashMap::new(),
        children: HashMap::new(),
        pane_rows: HashMap::new(),
        seen: HashSet::new(),
        cols,
        lines: Vec::new(),
        task_at: Vec::new(),
    };
    for t in tasks {
        w.by_id.insert(&t.id, t);
    }
    let mut roots = Vec::new();
    for t in tasks {
        if w.by_id.contains_key(t.parent.as_str()) && t.parent != t.id {
            w.children.entry(t.parent.as_str()).or_default().push(&t.id);
        } else {
            roots.push(t.id.as_str());
        }
    }
    for r in panes {
        w.pane_rows.insert(&r.id, r);
    }
    let n = roots.len();
    for (i, id) in roots.into_iter().enumerate() {
        w.walk(id, "", i == n - 1);
    }
    (w.lines, w.task_at)
}

/// The tasks as a text tree, every line cut to cols.
#[cfg(test)]
pub fn render_tree(tasks: &[TaskRow], panes: &[Row], cols: i64) -> Vec<String> {
    tree_lines(tasks, panes, cols).0
}

/// Opens the tree with the cursor on the first task line.
pub fn new_tree_view(tasks: &[TaskRow], panes: &[Row], cols: i64) -> TreeView {
    let (lines, task_at) = tree_lines(tasks, panes, cols);
    TreeView {
        lines,
        task_at,
        cursor: 0,
    }
}

impl TreeView {
    /// Shifts the cursor delta task lines, skipping pane lines.
    pub fn move_by(&mut self, delta: i64) {
        let (step, mut delta) = if delta < 0 { (-1, -delta) } else { (1, delta) };
        while delta > 0 {
            let mut next = self.cursor;
            loop {
                next += step;
                if next < 0 || next >= self.task_at.len() as i64 {
                    return;
                }
                if !self.task_at[next as usize].is_empty() {
                    break;
                }
            }
            self.cursor = next;
            delta -= 1;
        }
    }

    /// The task id under the cursor, or "".
    pub fn selected(&self) -> String {
        if self.cursor < 0 || self.cursor >= self.task_at.len() as i64 {
            return String::new();
        }
        self.task_at[self.cursor as usize].clone()
    }
}
