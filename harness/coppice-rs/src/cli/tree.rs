//! The task tree `coppice task list --tree` prints: each task, its panes
//! under it, then its child tasks, depth first.

use std::collections::{BTreeMap, HashMap, HashSet};

use serde_json::{Map, Value};

use crate::textwidth;

struct TaskRow {
    id: String,
    label: String,
    parent: String,
    worktree: String,
    state: String,
    panes: Vec<String>,
    ahead: Option<i64>,
}

struct PaneRow {
    label: String,
    state: String,
}

fn s(m: &Map<String, Value>, k: &str) -> String {
    m.get(k).and_then(Value::as_str).unwrap_or("").to_string()
}

fn rows(list: &[Value]) -> Vec<&Map<String, Value>> {
    list.iter().filter_map(Value::as_object).collect()
}

fn state_mark(state: &str) -> String {
    match state {
        "blocked" => "● needs you".into(),
        "working" => "● working".into(),
        "idle" => "○ idle".into(),
        "done" => "· done".into(),
        "" => String::new(),
        other => format!("· {other}"),
    }
}

/// Go's filepath.Base.
fn base(p: &str) -> String {
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return if p.is_empty() { ".".into() } else { "/".into() };
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

fn task_text(t: &TaskRow) -> String {
    let mut parts = vec![if t.label.is_empty() {
        t.id.clone()
    } else {
        t.label.clone()
    }];
    if !t.worktree.is_empty() {
        parts.push(base(&t.worktree));
    }
    if let Some(n) = t.ahead {
        parts.push(format!("+{n}"));
    }
    let mark = state_mark(&t.state);
    if !mark.is_empty() {
        parts.push(mark);
    }
    textwidth::printable(&parts.join("  "), 0)
}

fn pane_text(id: &str, r: &PaneRow) -> String {
    let mut label = textwidth::printable(&r.label, 0);
    if label.is_empty() {
        label = id.to_string();
    }
    let mark = state_mark(&r.state);
    if mark.is_empty() {
        label
    } else {
        format!("{label}  {mark}")
    }
}

/// The tree's lines, each cut to cols, from task.list's and pane.list's
/// rows. Closed panes are left out.
pub fn render(tasks: &[Value], panes: &[Value], cols: i64) -> Vec<String> {
    let tasks: Vec<TaskRow> = rows(tasks)
        .into_iter()
        .map(|t| TaskRow {
            id: s(t, "id"),
            label: s(t, "label"),
            parent: s(t, "parent"),
            worktree: s(t, "worktree"),
            state: s(t, "state"),
            panes: t
                .get("panes")
                .and_then(Value::as_array)
                .map(|a| {
                    a.iter()
                        .filter_map(|p| p.as_str().map(String::from))
                        .collect()
                })
                .unwrap_or_default(),
            ahead: t.get("ahead").and_then(Value::as_f64).map(|f| f as i64),
        })
        .collect();
    let mut pane_rows: HashMap<String, PaneRow> = HashMap::new();
    for p in rows(panes) {
        if p.get("closed").and_then(Value::as_bool) == Some(true) {
            continue;
        }
        pane_rows.insert(
            s(p, "id"),
            PaneRow {
                label: s(p, "label"),
                state: s(p, "state"),
            },
        );
    }
    let mut by_id: HashMap<&str, &TaskRow> = HashMap::new();
    for t in &tasks {
        by_id.insert(&t.id, t);
    }
    let mut children: BTreeMap<&str, Vec<&str>> = BTreeMap::new();
    let mut roots: Vec<&str> = Vec::new();
    for t in &tasks {
        if by_id.contains_key(t.parent.as_str()) && t.parent != t.id {
            children.entry(&t.parent).or_default().push(&t.id);
        } else {
            roots.push(&t.id);
        }
    }
    let mut lines = Vec::new();
    let mut seen = HashSet::new();
    #[allow(clippy::too_many_arguments)]
    fn walk<'a>(
        id: &'a str,
        indent: &str,
        last: bool,
        cols: i64,
        by_id: &HashMap<&str, &'a TaskRow>,
        children: &BTreeMap<&str, Vec<&'a str>>,
        pane_rows: &HashMap<String, PaneRow>,
        seen: &mut HashSet<&'a str>,
        lines: &mut Vec<String>,
    ) {
        if !seen.insert(id) {
            return;
        }
        let empty = TaskRow {
            id: String::new(),
            label: String::new(),
            parent: String::new(),
            worktree: String::new(),
            state: String::new(),
            panes: Vec::new(),
            ahead: None,
        };
        let t = by_id.get(id).copied().unwrap_or(&empty);
        let (branch, deeper) = if last {
            ("└─ ", "   ")
        } else {
            ("├─ ", "│  ")
        };
        lines.push(textwidth::truncate(
            &format!("{indent}{branch}{}", task_text(t)),
            cols,
        ));
        let here: Vec<(&String, &PaneRow)> = t
            .panes
            .iter()
            .filter_map(|p| pane_rows.get(p).map(|r| (p, r)))
            .collect();
        let kids = children.get(id).cloned().unwrap_or_default();
        for (i, (pid, r)) in here.iter().enumerate() {
            let b = if i == here.len() - 1 && kids.is_empty() {
                "└─ "
            } else {
                "├─ "
            };
            lines.push(textwidth::truncate(
                &format!("{indent}{deeper}{b}{}", pane_text(pid, r)),
                cols,
            ));
        }
        let next = format!("{indent}{deeper}");
        for (i, kid) in kids.iter().enumerate() {
            walk(
                kid,
                &next,
                i == kids.len() - 1,
                cols,
                by_id,
                children,
                pane_rows,
                seen,
                lines,
            );
        }
    }
    for (i, id) in roots.iter().enumerate() {
        walk(
            id,
            "",
            i == roots.len() - 1,
            cols,
            &by_id,
            &children,
            &pane_rows,
            &mut seen,
            &mut lines,
        );
    }
    lines
}
