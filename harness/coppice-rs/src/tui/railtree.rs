//! The rail as a tree: one folding group per task, then one per project.

use std::collections::HashMap;

use crate::textwidth;

use super::model::{by_created, children_of, Model, RailGroup, Row};
use super::render::{pad, PROMPT_MARK, SGR_AMBER, SGR_RESET, SGR_REVERSE};

/// The base name of a row's directory, or "no directory".
pub fn project_name(r: &Row) -> String {
    if r.worktree.is_empty() {
        return "no directory".into();
    }
    base_name(&r.worktree)
}

/// Go's filepath.Base.
pub fn base_name(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return "/".into();
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

impl RailGroup {
    /// How many agents of the group need you.
    pub fn needs(&self) -> usize {
        self.rows.iter().filter(|r| r.needs_you()).count()
    }

    /// The header's state counts, as "1 needs · 2 working".
    pub fn counts(&self) -> String {
        let (mut needs, mut working, mut idle, mut done) = (0, 0, 0, 0);
        for r in &self.rows {
            if r.needs_you() {
                needs += 1;
            } else if r.state == "working" || r.held.is_some() {
                working += 1;
            } else if r.state == "done" {
                done += 1;
            } else {
                idle += 1;
            }
        }
        let mut parts = Vec::new();
        for (n, word) in [
            (needs, "needs"),
            (working, "working"),
            (idle, "idle"),
            (done, "done"),
        ] {
            if n > 0 {
                parts.push(format!("{n} {word}"));
            }
        }
        parts.join(" · ")
    }
}

impl Model {
    /// The rail tree: one group per task that has a live agent, then one
    /// per project for the agents with no task.
    pub fn groups(&self) -> Vec<RailGroup> {
        let rows = self.visible();
        let mut out: Vec<RailGroup> = Vec::new();
        let mut projects: Vec<RailGroup> = Vec::new();
        let mut at: HashMap<String, usize> = HashMap::new();
        let mut at_project: HashMap<String, usize> = HashMap::new();
        let mut top: Vec<Row> = rows.into_iter().filter(|r| r.parent.is_empty()).collect();
        top.sort_by(|a, b| by_created(&a.id, &b.id));
        for r in top {
            if !r.task.is_empty() {
                let key = format!("task:{}", r.task);
                let i = match at.get(&key) {
                    Some(i) => *i,
                    None => {
                        out.push(RailGroup {
                            key: key.clone(),
                            title: textwidth::printable(&self.task_label(&r.task), 0),
                            rows: vec![],
                        });
                        at.insert(key, out.len() - 1);
                        out.len() - 1
                    }
                };
                out[i].rows.push(r);
                continue;
            }
            let name = project_name(&r);
            let key = format!("project:{name}");
            let i = match at_project.get(&key) {
                Some(i) => *i,
                None => {
                    projects.push(RailGroup {
                        key: key.clone(),
                        title: textwidth::printable(&name, 0),
                        rows: vec![],
                    });
                    at_project.insert(key, projects.len() - 1);
                    projects.len() - 1
                }
            };
            projects[i].rows.push(r);
        }
        out.extend(projects);
        out
    }

    /// Every live row in rail order, folded groups too, each pane's
    /// subagents after it.
    pub fn tree_rows(&self) -> Vec<Row> {
        let rows = self.visible();
        let mut out = Vec::new();
        for g in self.groups() {
            for r in g.rows {
                let kids = children_of(&rows, &r.id);
                out.push(r);
                out.extend(kids);
            }
        }
        out
    }

    /// The key of the group that holds pane id, or "".
    pub fn group_of(&self, id: &str) -> String {
        for g in self.groups() {
            if g.rows.iter().any(|r| r.id == id) {
                return g.key;
            }
        }
        String::new()
    }

    /// Folds or unfolds one group. The cursor stays on its header.
    pub fn toggle_group(&mut self, key: &str) {
        let v = !self.folded.get(key).copied().unwrap_or(false);
        self.folded.insert(key.to_string(), v);
        self.cursor = self.index_of(&format!("group:{key}"), self.cursor);
        self.at_header = true;
    }
}

/// How a group header draws.
pub fn group_line(m: &Model, g: &RailGroup, cursor: bool, focused: bool, cols: i64) -> String {
    let mark = if cursor { PROMPT_MARK } else { "  " };
    let arrow = if m.folded.get(&g.key).copied().unwrap_or(false) {
        "▸"
    } else {
        "▾"
    };
    let mut text = format!("{mark}{arrow} {}", g.title);
    let c = g.counts();
    if !c.is_empty() {
        text.push_str("  ");
        text.push_str(&c);
    }
    let mut color = String::new();
    if g.needs() > 0 {
        color = SGR_AMBER.into();
    }
    if cursor && focused {
        color.push_str(SGR_REVERSE);
    }
    format!("{color}{}{SGR_RESET}", pad(&text, cols))
}
