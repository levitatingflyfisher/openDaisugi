//! The screen model of the floor: what the roster shows and nothing about
//! how it is drawn.

use std::collections::{HashMap, HashSet};
use std::time::Instant;

use serde_json::{Map, Value};

use crate::attach::Screen;
use crate::textwidth;
use crate::tiles::Tiles;

use super::facts::Facts;
use super::stackline::stack_text;
use super::tree::{new_tree_view, TaskRow, TreeView};

/// One JSON object of a reply or an event.
pub type Obj = Map<String, Value>;

/// One pane on the roster.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Row {
    pub id: String,
    pub label: String,
    pub harness: String,
    pub worktree: String,
    pub state: String,
    pub source: String,
    pub line: String,
    pub age: f64,
    pub quiet_for: f64,
    /// The id of the task the pane works for, or "".
    pub task: String,
    /// Set on a subagent row: the id of the pane whose harness runs it.
    pub parent: String,
    pub child: String,
    /// Set while a foreman hears this row's ask first.
    pub held: Option<Held>,
    pub looking: Vec<String>,
    /// The one line a window header shows under the name.
    pub stack: String,
    /// Set on an ended row when the server knows the exit code.
    pub exit_code: Option<i64>,
    /// pty or headless, from pane.list's own kind field.
    pub kind: String,
    /// True while the pane shows Claude's folder trust screen.
    pub trust: bool,
}

/// The tier of an ask the operator allows with one key.
pub const TIER_UNDOABLE: &str = "undoable";

/// "undoable" for exactly that word, "permanent" for anything else.
pub fn normal_tier(tier: &str) -> String {
    if tier == TIER_UNDOABLE {
        return TIER_UNDOABLE.into();
    }
    "permanent".into()
}

/// Whether a state event's detail came from the trust rule.
pub fn is_trust_detail(detail: &str) -> bool {
    detail.contains("rule=coppice_first_run_trust ")
}

/// The Row kind of an agent that takes whole messages.
pub const KIND_HEADLESS: &str = "headless";

/// Who holds a row's ask.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Held {
    pub foreman: String,
    pub task_label: String,
    pub waited: f64,
}

/// A string field. A missing field or a JSON null is "".
pub fn s(m: &Obj, key: &str) -> String {
    m.get(key).and_then(Value::as_str).unwrap_or("").to_string()
}

/// A JSON number, or None.
pub fn num(m: &Obj, key: &str) -> Option<f64> {
    match m.get(key) {
        Some(Value::Number(n)) => n.as_f64(),
        _ => None,
    }
}

/// A boolean field, or None when it is not one.
pub fn boolean(m: &Obj, key: &str) -> Option<bool> {
    m.get(key).and_then(Value::as_bool)
}

/// An object field, or None.
pub fn obj<'a>(m: &'a Obj, key: &str) -> Option<&'a Obj> {
    m.get(key).and_then(Value::as_object)
}

fn held_of(m: &Obj, now: f64) -> Option<Held> {
    let h = obj(m, "held")?;
    let mut out = Held {
        foreman: s(h, "by"),
        task_label: s(h, "task_label"),
        waited: 0.0,
    };
    if out.task_label.is_empty() {
        out.task_label = s(h, "task");
    }
    if let Some(since) = num(h, "since") {
        if now > since {
            out.waited = now - since;
        }
    }
    Some(out)
}

impl Row {
    /// A blocked pane whose ask no foreman holds.
    pub fn needs_you(&self) -> bool {
        self.state == "blocked" && self.held.is_none() && self.parent.is_empty()
    }
}

fn looking_of(m: &Obj) -> Vec<String> {
    let mut out = Vec::new();
    if let Some(Value::Array(raw)) = m.get("looking") {
        for v in raw {
            if let Some(x) = v.as_str() {
                if !x.is_empty() {
                    out.push(x.to_string());
                }
            }
        }
    }
    out
}

/// What every action on a subagent row says.
pub const CHILD_MESSAGE: &str = "a subagent lives inside its parent. Enter on the parent.";

fn child_id(parent: &str, child: &str) -> String {
    format!("{parent}/{child}")
}

/// One titled group of rows.
#[derive(Clone, Debug, Default)]
pub struct Section {
    #[cfg_attr(not(test), allow(dead_code))]
    pub title: String,
    pub rows: Vec<Row>,
}

/// The open detail view on one pane.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct PeekView {
    pub pane: String,
    pub label: String,
    pub text: String,
    pub ask: String,
    pub tool: String,
    pub verdict: String,
    pub rule: i64,
    pub has_gate: bool,
    pub ask_id: String,
    pub tier: String,
    pub trust: bool,
}

impl PeekView {
    /// What the operator types to allow a permanent ask.
    pub fn name(&self) -> String {
        if !self.label.is_empty() {
            return self.label.clone();
        }
        self.pane.clone()
    }

    pub fn permanent(&self) -> bool {
        self.tier != TIER_UNDOABLE
    }
}

/// Whether the peek shows an ask the operator can answer.
pub fn answerable(p: &Option<PeekView>) -> bool {
    p.as_ref().is_some_and(|p| !p.ask_id.is_empty())
}

/// A pane size in cells.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Size {
    pub cols: i64,
    pub rows: i64,
}

/// The open project picker.
#[derive(Clone, Debug, Default)]
pub struct Picker {
    pub projects: Vec<Project>,
    pub cursor: i64,
}

/// One row of project.list.
#[derive(Clone, Debug, Default)]
pub struct Project {
    pub path: String,
    pub name: String,
    pub pinned: bool,
}

/// How many notes the floor draws.
pub const NOTES_SHOWN: usize = 3;

pub const TITLE_NEEDS_YOU: &str = "NEEDS YOU";
pub const TITLE_WORKING: &str = "WORKING";
pub const TITLE_DONE: &str = "DONE";

/// The whole state of the floor screen.
#[derive(Default)]
pub struct Model {
    pub rows: Vec<Row>,
    pub cursor: i64,
    pub peek: Option<PeekView>,
    /// The typed prompt line, as bytes: a key adds one byte at a time.
    pub prompt: Vec<u8>,
    pub message: String,
    pub default: String,
    pub need_you: i64,
    pub tiles: Option<Tiles>,
    pub screens: HashMap<String, Screen>,
    pub talking: bool,
    pub confirm: String,
    pub tasks: Vec<TaskRow>,
    pub path: Vec<String>,
    pub tree: Option<TreeView>,
    pub row_at: Vec<i64>,
    pub tile_at: Vec<i64>,
    pub tile_y: Vec<bool>,
    pub tile_sizes: HashMap<String, Size>,
    pub typing: String,
    pub headless_line: HashMap<String, String>,
    pub headless_cursor: HashMap<String, usize>,
    pub leave: u8,
    pub talk: u8,
    pub voice: String,
    pub notes: Vec<String>,
    pub ended: Vec<Row>,
    pub recent_open: bool,
    pub ended_line: String,
    pub ended_sticky: bool,
    pub ended_at: Option<Instant>,
    pub facts: Option<Facts>,
    pub paused: String,
    pub folded: HashMap<String, bool>,
    pub cursor_hidden: bool,
    pub win_rows: i64,
    pub picker: Option<Picker>,
    pub renaming: String,
    pub clear_all: bool,
    pub cursor_x: i64,
    pub cursor_y: i64,
    pub rail_w: i64,
    pub tile_x: Vec<i64>,
    pub tile_w: Vec<i64>,
    pub header_y: i64,
    pub ended_y: i64,
    pub at_header: bool,
    pub shown: i64,
    pub body_cur: i64,
}

/// Go's string conversion of bytes: each byte that is not part of a whole
/// UTF-8 rune reads as U+FFFD.
pub fn go_string(b: &[u8]) -> String {
    let mut out = String::new();
    let mut i = 0;
    while i < b.len() {
        match std::str::from_utf8(&b[i..]) {
            Ok(rest) => {
                out.push_str(rest);
                break;
            }
            Err(e) => {
                let good = e.valid_up_to();
                out.push_str(std::str::from_utf8(&b[i..i + good]).unwrap_or(""));
                out.push('\u{FFFD}');
                i += good + 1;
            }
        }
    }
    out
}

/// The length in bytes of the last rune of b, as Go's
/// utf8.DecodeLastRune reads it: 1 for a byte that ends no whole rune.
pub fn last_rune_len(b: &[u8]) -> usize {
    let n = b.len();
    if n == 0 {
        return 0;
    }
    for size in 2..=4usize {
        if size > n {
            break;
        }
        if let Ok(s) = std::str::from_utf8(&b[n - size..]) {
            if s.chars().count() == 1 {
                return size;
            }
        }
    }
    1
}

/// Sorts the pane rows into the three sections.
pub fn group(rows: &[Row]) -> Vec<Section> {
    let mut secs = vec![
        Section {
            title: TITLE_NEEDS_YOU.into(),
            rows: vec![],
        },
        Section {
            title: TITLE_WORKING.into(),
            rows: vec![],
        },
        Section {
            title: TITLE_DONE.into(),
            rows: vec![],
        },
    ];
    for r in rows {
        if !r.parent.is_empty() {
            continue;
        }
        if r.needs_you() {
            secs[0].rows.push(r.clone());
        } else if r.state == "done" {
            secs[2].rows.push(r.clone());
        } else {
            secs[1].rows.push(r.clone());
        }
    }
    for sec in &mut secs {
        sec.rows.sort_by(|a, b| by_created(&a.id, &b.id));
    }
    secs
}

/// The ordering createdBefore gives, for a stable sort.
pub fn by_created(a: &str, b: &str) -> std::cmp::Ordering {
    if created_before(a, b) {
        std::cmp::Ordering::Less
    } else if created_before(b, a) {
        std::cmp::Ordering::Greater
    } else {
        std::cmp::Ordering::Equal
    }
}

/// Orders two pane ids by when they were made: digit runs compare as
/// numbers.
pub fn created_before(a: &str, b: &str) -> bool {
    let (mut a, mut b) = (a.as_bytes(), b.as_bytes());
    while !a.is_empty() && !b.is_empty() {
        let (da, db) = (digit_run(a), digit_run(b));
        if da > 0 && db > 0 {
            let na = trim_zeros(&a[..da]);
            let nb = trim_zeros(&b[..db]);
            if na.len() != nb.len() {
                return na.len() < nb.len();
            }
            if na != nb {
                return na < nb;
            }
            a = &a[da..];
            b = &b[db..];
            continue;
        }
        if a[0] != b[0] {
            return a[0] < b[0];
        }
        a = &a[1..];
        b = &b[1..];
    }
    a.len() < b.len()
}

fn trim_zeros(x: &[u8]) -> &[u8] {
    let n = x.iter().take_while(|c| **c == b'0').count();
    &x[n..]
}

fn digit_run(x: &[u8]) -> usize {
    x.iter().take_while(|c| c.is_ascii_digit()).count()
}

/// The subagent rows of pane id, in list order.
pub fn children_of(rows: &[Row], id: &str) -> Vec<Row> {
    rows.iter().filter(|r| r.parent == id).cloned().collect()
}

/// The one line a row shows: the ask, with its tool, wins over the detail.
pub fn line_for(detail: &str, ask: Option<&Obj>) -> String {
    let Some(ask) = ask else {
        return detail.to_string();
    };
    let summary = s(ask, "summary");
    let tool = s(ask, "tool");
    if !tool.is_empty() {
        return format!("{tool}: {summary}");
    }
    summary
}

pub const BLOCKED_BY_GATE: &str = "Blocked. The gate gave no detail.";
pub const BLOCKED_OWN_QUESTION: &str = "Waiting on the agent's own question. Open it to answer.";
pub const TRUST_LINE: &str =
    "Asks to trust this folder. Peek with Space, then y trusts it or n is not now.";

fn trust_of(m: &Obj) -> bool {
    s(m, "state") == "blocked" && ask_of(m).is_none() && is_trust_detail(&s(m, "detail"))
}

fn event_line(m: &Obj) -> String {
    let ask = ask_of(m);
    if ask.is_none() && s(m, "state") == "blocked" {
        if s(m, "source") == "gate" {
            return BLOCKED_BY_GATE.into();
        }
        if trust_of(m) {
            return TRUST_LINE.into();
        }
        return BLOCKED_OWN_QUESTION.into();
    }
    line_for(&s(m, "detail"), ask)
}

/// The ask sub-map of a list row or a state event.
pub fn ask_of(m: &Obj) -> Option<&Obj> {
    obj(m, "ask")
}

/// Turns pane.list rows into roster rows. Closed panes are dropped.
pub fn rows_from(list: &[Obj], now: f64) -> Vec<Row> {
    let mut rows = Vec::new();
    for p in list {
        if boolean(p, "closed") == Some(true) {
            continue;
        }
        let mut r = Row {
            id: s(p, "id"),
            label: s(p, "label"),
            harness: s(p, "harness"),
            worktree: s(p, "cwd"),
            state: s(p, "state"),
            source: s(p, "source"),
            line: event_line(p),
            task: s(p, "task"),
            kind: s(p, "kind"),
            trust: trust_of(p),
            ..Default::default()
        };
        if let Some(ts) = num(p, "ts") {
            r.age = now - ts;
        }
        r.quiet_for = num(p, "quiet_for").unwrap_or(0.0);
        r.looking = looking_of(p);
        r.stack = stack_text(p);
        if r.state == "blocked" {
            r.held = held_of(p, now);
        }
        let (rid, rtask) = (r.id.clone(), r.task.clone());
        rows.push(r);
        if let Some(Value::Array(kids)) = p.get("children") {
            for k in kids {
                let Some(km) = k.as_object() else { continue };
                if s(km, "id").is_empty() {
                    continue;
                }
                let mut c = Row {
                    id: child_id(&rid, &s(km, "id")),
                    parent: rid.clone(),
                    child: s(km, "id"),
                    label: s(km, "label"),
                    state: s(km, "state"),
                    task: rtask.clone(),
                    ..Default::default()
                };
                if let Some(ts) = num(km, "ts") {
                    c.age = now - ts;
                }
                rows.push(c);
            }
        }
    }
    rows
}

/// Clamps a cursor inside 0 to n-1, and at 0 when there are no rows.
pub fn clamp(i: i64, n: i64) -> i64 {
    if n == 0 || i < 0 {
        return 0;
    }
    if i >= n {
        return n - 1;
    }
    i
}

impl Model {
    /// Keeps one note, and only the last NOTES_SHOWN.
    pub fn add_note(&mut self, text: &str) {
        self.notes.push(text.to_string());
        if self.notes.len() > NOTES_SHOWN {
            let cut = self.notes.len() - NOTES_SHOWN;
            self.notes.drain(..cut);
        }
    }

    /// The typed prompt as text.
    pub fn prompt_text(&self) -> String {
        go_string(&self.prompt)
    }

    /// The task and every descendant of it.
    pub fn under(&self, id: &str) -> HashSet<String> {
        let mut children: HashMap<&str, Vec<&str>> = HashMap::new();
        for t in &self.tasks {
            if !t.parent.is_empty() {
                children.entry(t.parent.as_str()).or_default().push(&t.id);
            }
        }
        let mut set = HashSet::new();
        let mut stack = vec![id.to_string()];
        // Depth first, as Go's walk: the order does not change the set.
        while let Some(cur) = stack.pop() {
            if !set.insert(cur.clone()) {
                continue;
            }
            if let Some(kids) = children.get(cur.as_str()) {
                for k in kids.iter().rev() {
                    stack.push(k.to_string());
                }
            }
        }
        set
    }

    /// The task the roster is pushed into, or "" on the top floor.
    pub fn scope(&self) -> String {
        self.path.last().cloned().unwrap_or_default()
    }

    /// The rows the roster shows.
    pub fn visible(&self) -> Vec<Row> {
        let sc = self.scope();
        if sc.is_empty() {
            return self.rows.clone();
        }
        let keep = self.under(&sc);
        self.rows
            .iter()
            .filter(|r| keep.contains(&r.task))
            .cloned()
            .collect()
    }

    /// The label of task id, or the id.
    pub fn task_label(&self, id: &str) -> String {
        for t in &self.tasks {
            if t.id == id && !t.label.is_empty() {
                return t.label.clone();
            }
        }
        id.to_string()
    }

    pub fn task(&self, id: &str) -> Option<TaskRow> {
        self.tasks.iter().find(|t| t.id == id).cloned()
    }

    pub fn has_task(&self, id: &str) -> bool {
        self.task(id).is_some()
    }

    /// The ids from the top task down to task id.
    pub fn chain(&self, id: &str) -> Option<Vec<String>> {
        if !self.has_task(id) {
            return None;
        }
        let mut out: Vec<String> = Vec::new();
        let mut seen = HashSet::new();
        let mut cur = id.to_string();
        while !cur.is_empty() && !seen.contains(&cur) {
            let Some(t) = self.task(&cur) else { break };
            seen.insert(cur.clone());
            out.insert(0, cur.clone());
            cur = t.parent;
        }
        Some(out)
    }

    /// Scopes the roster to task id and its descendants.
    pub fn push(&mut self, id: &str) {
        let Some(c) = self.chain(id) else { return };
        self.path = c;
        self.cursor = 0;
        self.rescope();
    }

    /// Goes up one level. The cursor stays on the same pane.
    pub fn pop(&mut self) {
        if self.path.is_empty() {
            return;
        }
        let keep = self.selected_key();
        self.path.pop();
        self.rescope();
        self.cursor = self.index_of(&keep, 0);
    }

    fn rescope(&mut self) {
        self.recount();
        let Some(p) = &self.peek else { return };
        let pane = p.pane.clone();
        if self.visible().iter().any(|r| r.id == pane) {
            return;
        }
        self.close_peek();
    }

    /// Opens the tree on the scope.
    pub fn new_tree(&self, cols: i64) -> TreeView {
        let sc = self.scope();
        let mut tasks = self.tasks.clone();
        if !sc.is_empty() {
            let keep = self.under(&sc);
            tasks = Vec::new();
            for t in &self.tasks {
                if keep.contains(&t.id) {
                    let mut t = t.clone();
                    if t.id == sc {
                        t.parent = String::new();
                    }
                    tasks.push(t);
                }
            }
        }
        new_tree_view(&tasks, &self.rows, cols)
    }

    /// Replaces the rows with a fresh pane.list.
    pub fn apply(&mut self, list: &[Obj], now: f64) {
        let keep = self.selected_key();
        let on_row = matches!(self.selected_item(), Some(it) if it.kind == ItemKind::Row);
        self.rows = rows_from(list, now);
        for i in 0..self.path.len() {
            let id = self.path[i].clone();
            if !self.has_task(&id) {
                self.message = format!("task {id} is gone");
                self.path.truncate(i);
                break;
            }
        }
        self.recount();
        let mut fallback = self.cursor;
        let n = self
            .items()
            .iter()
            .filter(|it| it.kind == ItemKind::Row || it.kind == ItemKind::Group)
            .count() as i64;
        if on_row && n > 0 {
            fallback = clamp(fallback, n);
        }
        self.cursor = self.index_of(&keep, fallback);
        if let Some(p) = &self.peek {
            if !self.has(&p.pane.clone()) {
                self.close_peek();
            }
        }
        self.vacate_gone();
    }

    fn vacate_gone(&mut self) {
        let Some(ts) = &self.tiles else { return };
        let gone: Vec<i64> = (0..ts.slots.len())
            .rev()
            .filter(|i| {
                let id = &ts.slots[*i];
                !id.is_empty() && !self.rows.iter().any(|r| &r.id == id)
            })
            .map(|i| i as i64)
            .collect();
        if let Some(ts) = &mut self.tiles {
            for i in gone {
                ts.close_slot(i);
            }
        }
    }

    /// Every pane id in cursor order, subagents left out.
    pub fn roster_order(&self) -> Vec<String> {
        self.tree_rows()
            .into_iter()
            .filter(|r| r.parent.is_empty())
            .map(|r| r.id)
            .collect()
    }

    pub fn row(&self, id: &str) -> Option<Row> {
        self.rows.iter().find(|r| r.id == id).cloned()
    }

    pub fn has(&self, id: &str) -> bool {
        self.rows.iter().any(|r| r.id == id)
    }

    pub fn is_headless(&self, id: &str) -> bool {
        self.rows
            .iter()
            .any(|r| r.id == id && r.kind == KIND_HEADLESS)
    }

    pub fn headless_buf(&self, id: &str) -> (String, usize) {
        (
            self.headless_line.get(id).cloned().unwrap_or_default(),
            self.headless_cursor.get(id).copied().unwrap_or(0),
        )
    }

    pub fn set_headless_buf(&mut self, id: &str, line: String, cursor: usize) {
        self.headless_line.insert(id.to_string(), line);
        self.headless_cursor.insert(id.to_string(), cursor);
    }

    /// The cursor position of the item with key id, or fallback clamped.
    pub fn index_of(&self, id: &str, fallback: i64) -> i64 {
        let all = self.items();
        if !id.is_empty() {
            if let Some(i) = all.iter().position(|it| it.key() == id) {
                return i as i64;
            }
        }
        let mut i = clamp(fallback, all.len() as i64);
        let iu = i as usize;
        if iu + 1 < all.len()
            && all[iu].kind == ItemKind::Group
            && all[iu + 1].kind == ItemKind::Row
        {
            i += 1;
        }
        i
    }

    /// Refreshes need_you from the rows the roster shows.
    pub fn recount(&mut self) {
        self.need_you = self.visible().iter().filter(|r| r.needs_you()).count() as i64;
    }

    /// Folds one state event into the row it names.
    pub fn event(&mut self, ev: &Obj) {
        let id = s(ev, "pane");
        if id.is_empty() {
            return;
        }
        let keep = self.selected_key();
        let mut found = false;
        for r in self.rows.iter_mut() {
            if r.id != id {
                continue;
            }
            r.state = s(ev, "state");
            r.source = s(ev, "source");
            r.line = event_line(ev);
            r.trust = trust_of(ev);
            r.age = 0.0;
            r.held = None;
            if r.state == "blocked" {
                r.held = held_of(ev, 0.0);
            }
            found = true;
        }
        if !found {
            return;
        }
        self.recount();
        self.cursor = self.index_of(&keep, self.cursor);
    }

    /// Folds one child event into the subagent rows of its parent.
    pub fn child(&mut self, ev: &Obj) {
        let (parent, kid) = (s(ev, "pane"), s(ev, "child"));
        if parent.is_empty() || kid.is_empty() {
            return;
        }
        let Some(p) = self.row(&parent) else { return };
        let id = child_id(&parent, &kid);
        for r in self.rows.iter_mut() {
            if r.id == id {
                r.state = s(ev, "state");
                let l = s(ev, "label");
                if !l.is_empty() {
                    r.label = l;
                }
                r.age = 0.0;
                return;
            }
        }
        let keep = self.selected_key();
        self.rows.push(Row {
            id,
            parent,
            child: kid,
            label: s(ev, "label"),
            state: s(ev, "state"),
            task: p.task,
            ..Default::default()
        });
        self.cursor = self.index_of(&keep, self.cursor);
    }

    /// The live row under the cursor.
    pub fn selected(&mut self) -> Option<Row> {
        match self.selected_item() {
            Some(it) if it.kind == ItemKind::Row => Some(it.row),
            _ => None,
        }
    }

    /// Shifts the cursor by delta.
    pub fn move_by(&mut self, delta: i64) {
        let all = self.items();
        self.norm(&all);
        self.cursor = clamp(self.cursor + delta, all.len() as i64);
        self.landed();
    }

    /// Moves the cursor to the next row that needs the operator.
    pub fn next_need(&mut self) {
        let secs = group(&self.visible());
        let mut want = &secs[0].rows;
        if want.is_empty() {
            want = &secs[1].rows;
        }
        let stop: HashSet<&str> = want.iter().map(|r| r.id.as_str()).collect();
        let at: Vec<String> = self
            .tree_rows()
            .into_iter()
            .filter(|r| stop.contains(r.id.as_str()))
            .map(|r| r.id)
            .collect();
        if at.is_empty() {
            return;
        }
        let mut next = at[0].clone();
        if let Some(cur) = self.selected() {
            for (k, id) in at.iter().enumerate() {
                if *id == cur.id {
                    next = at[(k + 1) % at.len()].clone();
                }
            }
        }
        let g = self.group_of(&next);
        if self.folded.get(&g).copied().unwrap_or(false) {
            self.folded.insert(g, false);
        }
        self.cursor = self.index_of(&next, self.cursor);
        self.at_header = false;
    }

    /// Opens the peek on the row under the cursor.
    pub fn open_peek(&mut self, read: &str, ask: Option<&Obj>) {
        let Some(r) = self.selected() else { return };
        let mut p = PeekView {
            pane: r.id.clone(),
            label: r.label.clone(),
            text: read.to_string(),
            trust: r.trust && ask.is_none(),
            ..Default::default()
        };
        if let Some(ask) = ask {
            p.ask = line_for("", Some(ask));
            p.tool = s(ask, "tool");
            p.ask_id = s(ask, "id");
            p.tier = normal_tier(&s(ask, "tier"));
            if let Some(gate) = obj(ask, "gate") {
                p.has_gate = true;
                p.verdict = s(gate, "verdict");
                p.rule = num(gate, "rule").unwrap_or(0.0) as i64;
            }
        }
        self.peek = Some(p);
    }

    pub fn close_peek(&mut self) {
        self.peek = None;
    }

    /// The label of pane id, or the id.
    pub fn label_of(&self, id: &str) -> String {
        if let Some(r) = self.row(id) {
            if !r.label.is_empty() {
                return r.label;
            }
        }
        id.to_string()
    }

    /// The text after the prompt mark.
    pub fn prompt_line(&self) -> String {
        if !self.confirm.is_empty() {
            return stop_question(&self.label_of(&self.confirm));
        }
        if self.clear_all {
            return clear_all_question(self.recent().len());
        }
        if !self.renaming.is_empty() {
            return format!(
                "rename {}: {}",
                textwidth::printable(&self.label_of(&self.renaming), 0),
                self.prompt_text()
            );
        }
        self.prompt_text()
    }
}

/// The one line Clear all asks.
pub fn clear_all_question(n: usize) -> String {
    if n == 1 {
        return "Forget 1 ended agent? Enter forgets it, Esc keeps it.".into();
    }
    format!("Forget {n} ended agents? Enter forgets them, Esc keeps them.")
}

/// The one line ctrl-w asks.
pub fn stop_question(label: &str) -> String {
    format!(
        "Stop {}? Enter stops it, Esc keeps it.",
        textwidth::printable(label, 0)
    )
}

/// What one cursor stop on the rail is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ItemKind {
    Row,
    Fold,
    ResumeAll,
    ClearAll,
    Ended,
    Group,
}

/// One folding group of the rail.
#[derive(Clone, Debug, Default)]
pub struct RailGroup {
    pub key: String,
    pub title: String,
    pub rows: Vec<Row>,
}

/// One cursor stop on the rail.
#[derive(Clone, Debug)]
pub struct Item {
    pub kind: ItemKind,
    pub row: Row,
    pub group: RailGroup,
}

impl Item {
    pub fn of(kind: ItemKind) -> Item {
        Item {
            kind,
            row: Row::default(),
            group: RailGroup::default(),
        }
    }

    pub fn key(&self) -> String {
        match self.kind {
            ItemKind::Row => self.row.id.clone(),
            ItemKind::Fold => "recent".into(),
            ItemKind::ResumeAll => "recent:resume".into(),
            ItemKind::ClearAll => "recent:clear".into(),
            ItemKind::Group => format!("group:{}", self.group.key),
            ItemKind::Ended => format!("ended:{}", self.row.id),
        }
    }
}
