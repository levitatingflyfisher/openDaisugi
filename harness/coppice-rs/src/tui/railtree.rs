//! The rail as a tree: one folding group per task, then one per project.

use std::collections::{HashMap, HashSet};

use crate::textwidth;

use super::model::{by_created, children_of, created_before, Model, RailGroup, Row};
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
        self.all().iter().filter(|r| r.needs_you()).count()
    }

    /// The header's state counts, as "1 needs · 2 working".
    pub fn counts(&self) -> String {
        let (mut needs, mut working, mut idle, mut done) = (0, 0, 0, 0);
        for r in self.all() {
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
    /// per project for the agents with no task. A subtask nests under its
    /// parent task, right after it, and a parent with no agent of its own
    /// still draws a header when a subtask under it has one.
    pub fn groups(&self) -> Vec<RailGroup> {
        let rows = self.visible();
        let mut projects: Vec<RailGroup> = Vec::new();
        let mut at_project: HashMap<String, usize> = HashMap::new();
        let mut top: Vec<Row> = rows.into_iter().filter(|r| r.parent.is_empty()).collect();
        top.sort_by(|a, b| by_created(&a.id, &b.id));
        let mut own: HashMap<String, Vec<Row>> = HashMap::new();
        let mut order: Vec<String> = Vec::new();
        for r in top {
            if !r.task.is_empty() {
                if !own.contains_key(&r.task) {
                    order.push(r.task.clone());
                }
                own.entry(r.task.clone()).or_default().push(r);
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
                        ..Default::default()
                    });
                    at_project.insert(key, projects.len() - 1);
                    projects.len() - 1
                }
            };
            projects[i].rows.push(r);
        }
        let mut out = self.task_groups(&own, &order);
        out.extend(projects);
        out
    }

    /// The task groups as a forest. A task is a root when it has no known
    /// parent that draws, and a task draws when it has an agent or a
    /// subtask that draws. Roots and siblings keep the order their first
    /// agent, subtasks included, was made in.
    fn task_groups(&self, own: &HashMap<String, Vec<Row>>, order: &[String]) -> Vec<RailGroup> {
        let scope = self.scope();
        let in_scope = if scope.is_empty() {
            None
        } else {
            Some(self.under(&scope))
        };
        let mut draws: HashSet<String> = HashSet::new();
        for id in order {
            draws.insert(id.clone());
            let mut seen: HashSet<String> = HashSet::new();
            seen.insert(id.clone());
            let mut cur = id.clone();
            loop {
                let Some(t) = self.task(&cur) else { break };
                if t.parent.is_empty()
                    || seen.contains(&t.parent)
                    || in_scope.as_ref().is_some_and(|s| !s.contains(&t.parent))
                    || !self.has_task(&t.parent)
                {
                    break;
                }
                draws.insert(t.parent.clone());
                seen.insert(t.parent.clone());
                cur = t.parent;
            }
        }
        let parent_of = |id: &str| -> String {
            match self.task(id) {
                Some(t) if t.parent != id && draws.contains(&t.parent) => t.parent,
                _ => String::new(),
            }
        };
        let mut ids: Vec<String> = draws.iter().cloned().collect();
        ids.sort();
        // The id of the oldest agent in the task or under it.
        fn first(
            id: &str,
            ids: &[String],
            own: &HashMap<String, Vec<Row>>,
            parent_of: &dyn Fn(&str) -> String,
            walked: &mut HashSet<String>,
        ) -> String {
            if !walked.insert(id.to_string()) {
                return String::new();
            }
            let mut best = own
                .get(id)
                .and_then(|rs| rs.first())
                .map(|r| r.id.clone())
                .unwrap_or_default();
            for k in ids {
                if parent_of(k) != id {
                    continue;
                }
                let f = first(k, ids, own, parent_of, walked);
                if !f.is_empty() && (best.is_empty() || created_before(&f, &best)) {
                    best = f;
                }
            }
            best
        }
        let mut first_of: HashMap<String, String> = HashMap::new();
        for id in &ids {
            let f = first(id, &ids, own, &parent_of, &mut HashSet::new());
            first_of.insert(id.clone(), f);
        }
        ids.sort_by(|a, b| {
            let (fa, fb) = (&first_of[a], &first_of[b]);
            if fa != fb {
                by_created(fa, fb)
            } else {
                a.cmp(b)
            }
        });
        let mut kids: HashMap<String, Vec<String>> = HashMap::new();
        let mut roots: Vec<String> = Vec::new();
        for id in &ids {
            let p = parent_of(id);
            if p.is_empty() {
                roots.push(id.clone());
            } else {
                kids.entry(p).or_default().push(id.clone());
            }
        }
        let mut out: Vec<RailGroup> = Vec::new();
        let mut done: HashSet<String> = HashSet::new();
        fn walk(
            m: &Model,
            id: &str,
            parent: &str,
            depth: usize,
            own: &HashMap<String, Vec<Row>>,
            kids: &HashMap<String, Vec<String>>,
            done: &mut HashSet<String>,
            out: &mut Vec<RailGroup>,
        ) -> Vec<Row> {
            if !done.insert(id.to_string()) {
                return Vec::new();
            }
            let at = out.len();
            let mine = own.get(id).cloned().unwrap_or_default();
            out.push(RailGroup {
                key: format!("task:{id}"),
                title: textwidth::printable(&m.task_label(id), 0),
                rows: mine.clone(),
                parent: if parent.is_empty() {
                    String::new()
                } else {
                    format!("task:{parent}")
                },
                depth,
                total: None,
            });
            let mut total = mine;
            if let Some(ks) = kids.get(id) {
                for k in ks {
                    total.extend(walk(m, k, id, depth + 1, own, kids, done, out));
                }
            }
            out[at].total = Some(total.clone());
            total
        }
        for id in &roots {
            walk(self, id, "", 0, own, &kids, &mut done, &mut out);
        }
        // A parent cycle has no root. Its tasks draw as roots.
        for id in &ids {
            walk(self, id, "", 0, own, &kids, &mut done, &mut out);
        }
        out
    }

    /// True when group g sits under a folded parent group.
    pub fn hidden(&self, g: &RailGroup, all: &[RailGroup]) -> bool {
        let mut p = g.parent.clone();
        let mut hops = 0;
        while !p.is_empty() && hops <= all.len() {
            if self.folded.get(&p).copied().unwrap_or(false) {
                return true;
            }
            p = all
                .iter()
                .find(|a| a.key == p)
                .map(|a| a.parent.clone())
                .unwrap_or_default();
            hops += 1;
        }
        false
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
    let mut text = format!(
        "{mark}{}{arrow} {}",
        "  ".repeat(g.depth.min(super::render::MAX_INDENT)),
        g.title
    );
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
