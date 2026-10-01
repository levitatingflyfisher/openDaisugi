//! The workspace, tab, pane and task tree. Ids look like "w1:p3": pane
//! numbers are workspace-scoped and never reused, because a client holds an
//! id across a close and must not silently start talking to a different
//! process. The tree is saved as layout.json, in the shape Go writes.

use std::collections::{BTreeMap, HashSet};
use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::Path;
use std::sync::{Mutex, RwLock};

use serde_json::{json, Value};

use crate::gojson;

/// The layout.json schema version this build reads and writes.
pub const SCHEMA_VERSION: i64 = 1;

pub const KIND_PTY: &str = "pty";
pub const KIND_HEADLESS: &str = "headless";

#[derive(Debug)]
pub enum LayoutError {
    NoWorkspace(String),
    NoTab(String),
    NoPane(String),
    NoTask(String),
    Cycle(String, String),
}

impl std::fmt::Display for LayoutError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            LayoutError::NoWorkspace(id) => write!(f, "no such workspace: {id}"),
            LayoutError::NoTab(id) => write!(f, "no such tab: {id}"),
            LayoutError::NoPane(id) => write!(f, "no such pane: {id}"),
            LayoutError::NoTask(id) => write!(f, "no such task: {id}"),
            LayoutError::Cycle(id, p) => write!(
                f,
                "a task cannot be moved under itself or one of its descendants: {id} under {p}"
            ),
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct Pane {
    pub id: String,
    pub workspace: String,
    pub tab: String,
    pub label: String,
    pub cwd: String,
    pub argv: Option<Vec<String>>,
    pub env: BTreeMap<String, String>,
    pub kind: String,
    pub harness: String,
    pub cols: i64,
    pub rows: i64,
    pub harness_session_id: String,
    pub closed: bool,
    pub exit_code: Option<i64>,
    pub ended_at: i64,
    pub extra_args: Vec<String>,
    pub parent_pane: String,
    pub fork_pending: bool,
    pub task_id: String,
}

impl Pane {
    /// The record as Go's json writes the struct, omitempty fields left out
    /// when empty.
    pub fn to_value(&self) -> Value {
        let mut p: Vec<(&str, Value)> = vec![
            ("id", json!(self.id)),
            ("workspace", json!(self.workspace)),
            ("tab", json!(self.tab)),
            ("label", json!(self.label)),
            ("cwd", json!(self.cwd)),
            ("argv", json!(self.argv)),
        ];
        if !self.env.is_empty() {
            p.push(("env", json!(self.env)));
        }
        p.push(("kind", json!(self.kind)));
        if !self.harness.is_empty() {
            p.push(("harness", json!(self.harness)));
        }
        p.push(("cols", json!(self.cols)));
        p.push(("rows", json!(self.rows)));
        if !self.harness_session_id.is_empty() {
            p.push(("harness_session_id", json!(self.harness_session_id)));
        }
        p.push(("closed", json!(self.closed)));
        if let Some(c) = self.exit_code {
            p.push(("exit_code", json!(c)));
        }
        if self.ended_at != 0 {
            p.push(("ended_at", json!(self.ended_at)));
        }
        if !self.extra_args.is_empty() {
            p.push(("extra_args", json!(self.extra_args)));
        }
        if !self.parent_pane.is_empty() {
            p.push(("parent_pane", json!(self.parent_pane)));
        }
        if self.fork_pending {
            p.push(("fork_pending", json!(true)));
        }
        if !self.task_id.is_empty() {
            p.push(("task_id", json!(self.task_id)));
        }
        gojson::obj(p)
    }

    /// The argv as a list, null when it never had one.
    pub fn argv_value(&self) -> Value {
        json!(self.argv)
    }
}

#[derive(Clone, Debug, Default)]
pub struct Task {
    pub id: String,
    pub label: String,
    pub cwd: String,
    pub worktree: String,
    pub parent: String,
    pub model: String,
    pub foreman: String,
    pub children: Vec<String>,
}

impl Task {
    fn to_value(&self) -> Value {
        let mut p: Vec<(&str, Value)> = vec![("id", json!(self.id)), ("label", json!(self.label))];
        for (k, v) in [
            ("cwd", &self.cwd),
            ("worktree", &self.worktree),
            ("parent", &self.parent),
            ("model", &self.model),
            ("foreman", &self.foreman),
        ] {
            if !v.is_empty() {
                p.push((k, json!(v)));
            }
        }
        p.push(("children", json!(self.children)));
        gojson::obj(p)
    }
}

#[derive(Clone, Debug, Default)]
pub struct Tab {
    pub id: String,
    pub workspace: String,
    pub label: String,
    pub pane_ids: Option<Vec<String>>,
}

#[derive(Clone, Debug, Default)]
pub struct Workspace {
    pub id: String,
    pub label: String,
    pub cwd: String,
    pub tab_ids: Option<Vec<String>>,
    pub next_pane: i64,
    pub next_tab: i64,
}

#[derive(Debug)]
struct Snapshot {
    v: i64,
    workspaces: BTreeMap<String, Workspace>,
    tabs: BTreeMap<String, Tab>,
    panes: BTreeMap<String, Pane>,
    tasks: BTreeMap<String, Task>,
    next_ws: i64,
    next_task: i64,
    current_ws: String,
    current_tab: String,
}

impl Snapshot {
    fn to_value(&self) -> Value {
        let ws: serde_json::Map<String, Value> = self
            .workspaces
            .iter()
            .map(|(k, w)| {
                (
                    k.clone(),
                    gojson::obj(vec![
                        ("id", json!(w.id)),
                        ("label", json!(w.label)),
                        ("cwd", json!(w.cwd)),
                        ("tab_ids", json!(w.tab_ids)),
                        ("next_pane", json!(w.next_pane)),
                        ("next_tab", json!(w.next_tab)),
                    ]),
                )
            })
            .collect();
        let tabs: serde_json::Map<String, Value> = self
            .tabs
            .iter()
            .map(|(k, t)| {
                (
                    k.clone(),
                    gojson::obj(vec![
                        ("id", json!(t.id)),
                        ("workspace", json!(t.workspace)),
                        ("label", json!(t.label)),
                        ("pane_ids", json!(t.pane_ids)),
                    ]),
                )
            })
            .collect();
        let panes: serde_json::Map<String, Value> = self
            .panes
            .iter()
            .map(|(k, p)| (k.clone(), p.to_value()))
            .collect();
        let tasks: serde_json::Map<String, Value> = self
            .tasks
            .iter()
            .map(|(k, t)| (k.clone(), t.to_value()))
            .collect();
        gojson::obj(vec![
            ("v", json!(self.v)),
            ("workspaces", Value::Object(ws)),
            ("tabs", Value::Object(tabs)),
            ("panes", Value::Object(panes)),
            ("tasks", Value::Object(tasks)),
            ("next_workspace", json!(self.next_ws)),
            ("next_task", json!(self.next_task)),
            ("current_workspace", json!(self.current_ws)),
            ("current_tab", json!(self.current_tab)),
        ])
    }
}

/// The highest N among ids of the form PREFIX then N, and whether any id
/// had that form. An id that does not parse is left out.
fn max_numeric_suffix<'a>(ids: impl Iterator<Item = &'a String>, prefix: &str) -> Option<i64> {
    ids.filter_map(|id| go_atoi(id.strip_prefix(prefix)?)).max()
}

/// strconv.Atoi: an optional sign, then decimal digits.
fn go_atoi(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

/// Reads one field of a JSON object the way Go's json.Unmarshal fills a
/// struct field: absent or null leaves the zero value, a wrong type is an
/// error.
struct Fields<'a>(&'a serde_json::Map<String, Value>);

impl Fields<'_> {
    fn get(&self, k: &str) -> Option<&Value> {
        self.0.get(k).filter(|v| !v.is_null())
    }
    fn s(&self, k: &str) -> Result<String, String> {
        match self.get(k) {
            None => Ok(String::new()),
            Some(Value::String(x)) => Ok(x.clone()),
            Some(_) => Err(format!("{k} is not a string")),
        }
    }
    fn i(&self, k: &str) -> Result<i64, String> {
        match self.get(k) {
            None => Ok(0),
            Some(v) => v.as_i64().ok_or_else(|| format!("{k} is not an integer")),
        }
    }
    fn opt_i(&self, k: &str) -> Result<Option<i64>, String> {
        self.get(k).map(|_| self.i(k)).transpose()
    }
    fn b(&self, k: &str) -> Result<bool, String> {
        match self.get(k) {
            None => Ok(false),
            Some(Value::Bool(x)) => Ok(*x),
            Some(_) => Err(format!("{k} is not a bool")),
        }
    }
    fn list(&self, k: &str) -> Result<Option<Vec<String>>, String> {
        match self.get(k) {
            None => Ok(None),
            Some(Value::Array(a)) => a
                .iter()
                .map(|x| {
                    x.as_str()
                        .map(String::from)
                        .ok_or_else(|| format!("{k} holds a non-string"))
                })
                .collect::<Result<Vec<_>, _>>()
                .map(Some),
            Some(_) => Err(format!("{k} is not a list")),
        }
    }
    fn obj(&self, k: &str) -> Result<Option<&serde_json::Map<String, Value>>, String> {
        match self.get(k) {
            None => Ok(None),
            Some(Value::Object(m)) => Ok(Some(m)),
            Some(_) => Err(format!("{k} is not an object")),
        }
    }
}

fn fields<'a>(v: &'a Value, what: &str) -> Result<Fields<'a>, String> {
    v.as_object()
        .map(Fields)
        .ok_or_else(|| format!("{what} is not an object"))
}

/// Each entry of a map field, read by f. Null entries are left out, as
/// Go keeps a nil pointer that no reader here could use.
fn map_of<T>(
    f: &Fields,
    k: &str,
    read: impl Fn(&Value) -> Result<T, String>,
) -> Result<BTreeMap<String, T>, String> {
    let mut out = BTreeMap::new();
    if let Some(m) = f.obj(k)? {
        for (id, v) in m {
            if !v.is_null() {
                out.insert(id.clone(), read(v)?);
            }
        }
    }
    Ok(out)
}

impl Pane {
    fn from_value(v: &Value) -> Result<Pane, String> {
        let f = fields(v, "a pane")?;
        let mut env = BTreeMap::new();
        if let Some(m) = f.obj("env")? {
            for (k, x) in m {
                match x {
                    Value::String(s) => {
                        env.insert(k.clone(), s.clone());
                    }
                    Value::Null => {
                        env.insert(k.clone(), String::new());
                    }
                    _ => return Err("env holds a non-string".into()),
                }
            }
        }
        Ok(Pane {
            id: f.s("id")?,
            workspace: f.s("workspace")?,
            tab: f.s("tab")?,
            label: f.s("label")?,
            cwd: f.s("cwd")?,
            argv: f.list("argv")?,
            env,
            kind: f.s("kind")?,
            harness: f.s("harness")?,
            cols: f.i("cols")?,
            rows: f.i("rows")?,
            harness_session_id: f.s("harness_session_id")?,
            closed: f.b("closed")?,
            exit_code: f.opt_i("exit_code")?,
            ended_at: f.i("ended_at")?,
            extra_args: f.list("extra_args")?.unwrap_or_default(),
            parent_pane: f.s("parent_pane")?,
            fork_pending: f.b("fork_pending")?,
            task_id: f.s("task_id")?,
        })
    }
}

impl Task {
    fn from_value(v: &Value) -> Result<Task, String> {
        let f = fields(v, "a task")?;
        Ok(Task {
            id: f.s("id")?,
            label: f.s("label")?,
            cwd: f.s("cwd")?,
            worktree: f.s("worktree")?,
            parent: f.s("parent")?,
            model: f.s("model")?,
            foreman: f.s("foreman")?,
            children: f.list("children")?.unwrap_or_default(),
        })
    }
}

impl Snapshot {
    /// The tree in a layout.json, with Go's repairs.
    fn from_value(v: &Value) -> Result<Snapshot, String> {
        let f = fields(v, "the layout")?;
        let mut workspaces = map_of(&f, "workspaces", |v| {
            let w = fields(v, "a workspace")?;
            Ok(Workspace {
                id: w.s("id")?,
                label: w.s("label")?,
                cwd: w.s("cwd")?,
                tab_ids: w.list("tab_ids")?,
                next_pane: w.i("next_pane")?,
                next_tab: w.i("next_tab")?,
            })
        })?;
        let tabs = map_of(&f, "tabs", |v| {
            let t = fields(v, "a tab")?;
            Ok(Tab {
                id: t.s("id")?,
                workspace: t.s("workspace")?,
                label: t.s("label")?,
                pane_ids: t.list("pane_ids")?,
            })
        })?;
        let panes = map_of(&f, "panes", Pane::from_value)?;
        let tasks = map_of(&f, "tasks", Task::from_value)?;
        let mut next_ws = f.i("next_workspace")?;
        if next_ws == 0 {
            next_ws = workspaces.len() as i64 + 1;
        }
        let mut next_task = f.i("next_task")?;
        match max_numeric_suffix(tasks.keys(), "t") {
            Some(max) if next_task <= max => next_task = max + 1,
            _ if next_task == 0 => next_task = 1,
            _ => {}
        }
        let mut v = f.i("v")?;
        if v == 0 {
            v = SCHEMA_VERSION;
        }
        for ws in workspaces.values_mut() {
            match max_numeric_suffix(panes.keys(), &format!("{}:p", ws.id)) {
                Some(max) if ws.next_pane <= max => ws.next_pane = max + 1,
                _ if ws.next_pane == 0 => ws.next_pane = 1,
                _ => {}
            }
            match max_numeric_suffix(tabs.keys(), &format!("{}:t", ws.id)) {
                Some(max) if ws.next_tab <= max => ws.next_tab = max + 1,
                _ if ws.next_tab == 0 => ws.next_tab = 1,
                _ => {}
            }
        }
        Ok(Snapshot {
            v,
            workspaces,
            tabs,
            panes,
            tasks,
            next_ws,
            next_task,
            current_ws: f.s("current_workspace")?,
            current_tab: f.s("current_tab")?,
        })
    }
}

/// The tree. Every read hands back a copy, so no caller can change a
/// record except through the methods here.
pub struct Tree {
    snap: RwLock<Snapshot>,
    save_mu: Mutex<()>,
}

impl Default for Tree {
    fn default() -> Self {
        Tree::new()
    }
}

/// Orders task ids by number, so t10 sorts after t9.
fn task_less(a: &str, b: &str) -> std::cmp::Ordering {
    let na = a.strip_prefix('t').unwrap_or(a).parse::<i64>();
    let nb = b.strip_prefix('t').unwrap_or(b).parse::<i64>();
    match (na, nb) {
        (Ok(x), Ok(y)) => x.cmp(&y),
        _ => a.cmp(b),
    }
}

impl Tree {
    pub fn new() -> Tree {
        Tree {
            snap: RwLock::new(Snapshot {
                v: SCHEMA_VERSION,
                workspaces: BTreeMap::new(),
                tabs: BTreeMap::new(),
                panes: BTreeMap::new(),
                tasks: BTreeMap::new(),
                next_ws: 1,
                next_task: 1,
                current_ws: String::new(),
                current_tab: String::new(),
            }),
            save_mu: Mutex::new(()),
        }
    }

    fn r(&self) -> std::sync::RwLockReadGuard<'_, Snapshot> {
        self.snap.read().unwrap_or_else(|e| e.into_inner())
    }

    fn w(&self) -> std::sync::RwLockWriteGuard<'_, Snapshot> {
        self.snap.write().unwrap_or_else(|e| e.into_inner())
    }

    pub fn create_workspace(&self, label: &str, cwd: &str) -> Workspace {
        let mut s = self.w();
        let ws = Workspace {
            id: format!("w{}", s.next_ws),
            label: label.into(),
            cwd: cwd.into(),
            tab_ids: None,
            next_pane: 1,
            next_tab: 1,
        };
        s.next_ws += 1;
        s.workspaces.insert(ws.id.clone(), ws.clone());
        if s.current_ws.is_empty() {
            s.current_ws = ws.id.clone();
        }
        ws
    }

    pub fn create_tab(&self, ws_id: &str, label: &str) -> Result<Tab, LayoutError> {
        let mut s = self.w();
        let ws = s
            .workspaces
            .get_mut(ws_id)
            .ok_or_else(|| LayoutError::NoWorkspace(ws_id.into()))?;
        let tab = Tab {
            id: format!("{}:t{}", ws.id, ws.next_tab),
            workspace: ws.id.clone(),
            label: label.into(),
            pane_ids: None,
        };
        ws.next_tab += 1;
        ws.tab_ids.get_or_insert_with(Vec::new).push(tab.id.clone());
        s.tabs.insert(tab.id.clone(), tab.clone());
        if s.current_tab.is_empty() {
            s.current_tab = tab.id.clone();
        }
        Ok(tab)
    }

    pub fn create_pane(&self, ws_id: &str, tab_id: &str, mut p: Pane) -> Result<Pane, LayoutError> {
        let mut s = self.w();
        if !s.workspaces.contains_key(ws_id) {
            return Err(LayoutError::NoWorkspace(ws_id.into()));
        }
        match s.tabs.get(tab_id) {
            Some(t) if t.workspace == ws_id => {}
            _ => return Err(LayoutError::NoTab(tab_id.into())),
        }
        let ws = s.workspaces.get_mut(ws_id).expect("checked above");
        p.id = format!("{}:p{}", ws.id, ws.next_pane);
        ws.next_pane += 1;
        p.workspace = ws.id.clone();
        p.tab = tab_id.into();
        if p.cols == 0 {
            p.cols = 120;
        }
        if p.rows == 0 {
            p.rows = 40;
        }
        s.tabs
            .get_mut(tab_id)
            .expect("checked above")
            .pane_ids
            .get_or_insert_with(Vec::new)
            .push(p.id.clone());
        s.panes.insert(p.id.clone(), p.clone());
        Ok(p)
    }

    pub fn pane(&self, id: &str) -> Option<Pane> {
        self.r().panes.get(id).cloned()
    }

    /// The only way to change a pane.
    pub fn update_pane(&self, id: &str, f: impl FnOnce(&mut Pane)) -> Result<(), LayoutError> {
        let mut s = self.w();
        let p = s
            .panes
            .get_mut(id)
            .ok_or_else(|| LayoutError::NoPane(id.into()))?;
        f(p);
        Ok(())
    }

    /// Every pane, sorted by id.
    pub fn panes(&self) -> Vec<Pane> {
        self.r().panes.values().cloned().collect()
    }

    pub fn workspaces(&self) -> Vec<Workspace> {
        self.r().workspaces.values().cloned().collect()
    }

    pub fn tabs(&self, ws_id: &str) -> Result<Vec<Tab>, LayoutError> {
        let s = self.r();
        let ws = s
            .workspaces
            .get(ws_id)
            .ok_or_else(|| LayoutError::NoWorkspace(ws_id.into()))?;
        Ok(ws
            .tab_ids
            .iter()
            .flatten()
            .filter_map(|id| s.tabs.get(id).cloned())
            .collect())
    }

    pub fn current(&self) -> (String, String) {
        let s = self.r();
        (s.current_ws.clone(), s.current_tab.clone())
    }

    /// Marks a pane ended, keeping the record. ended_at is stamped only the
    /// first time.
    pub fn close_pane(
        &self,
        id: &str,
        exit: Option<i64>,
        ended_at: i64,
    ) -> Result<(), LayoutError> {
        let mut s = self.w();
        let p = s
            .panes
            .get_mut(id)
            .ok_or_else(|| LayoutError::NoPane(id.into()))?;
        p.closed = true;
        if p.ended_at == 0 {
            p.ended_at = ended_at;
        }
        if exit.is_some() {
            p.exit_code = exit;
        }
        Ok(())
    }

    /// Drops a record outright. The workspace counter is left alone, so the
    /// number is never handed out again.
    pub fn remove_pane(&self, id: &str) -> Result<(), LayoutError> {
        let mut s = self.w();
        let tab = s
            .panes
            .get(id)
            .map(|p| p.tab.clone())
            .ok_or_else(|| LayoutError::NoPane(id.into()))?;
        if let Some(t) = s.tabs.get_mut(&tab) {
            if let Some(ids) = t.pane_ids.as_mut() {
                ids.retain(|x| x != id);
            }
        }
        s.panes.remove(id);
        Ok(())
    }

    /// Writes the tree atomically at mode 0600: a temp file in the same
    /// directory, synced, renamed over, and the directory synced.
    pub fn save(&self, path: &Path) -> std::io::Result<()> {
        let _g = self.save_mu.lock().unwrap_or_else(|e| e.into_inner());
        let body = gojson::marshal_indent(&self.r().to_value(), "  ");
        let dir = path.parent().unwrap_or(Path::new("."));
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)?;
        let tmp = dir.join(format!(".layout-{}.json.tmp", rand_suffix()));
        let res = (|| {
            let mut f = std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(&tmp)?;
            f.write_all(body.as_bytes())?;
            f.sync_all()?;
            drop(f);
            std::fs::rename(&tmp, path)
        })();
        if res.is_err() {
            let _ = std::fs::remove_file(&tmp);
            return res;
        }
        std::fs::File::open(dir)?.sync_all()
    }

    /// Reads layout.json into this tree, in place of what it holds, with
    /// the repairs Go's layout.Load makes: missing maps become empty, a
    /// zero schema becomes this build's, a newer one is refused, and every
    /// id counter is raised past the ids already in the file.
    pub fn load(&self, path: &Path) -> Result<(), String> {
        let b = std::fs::read(path).map_err(|e| crate::sys::go_path_err("open", path, &e))?;
        let v: Value =
            serde_json::from_slice(&b).map_err(|e| format!("layout file is not readable: {e}"))?;
        let snap =
            Snapshot::from_value(&v).map_err(|e| format!("layout file is not readable: {e}"))?;
        if snap.v > SCHEMA_VERSION {
            return Err(format!(
                "layout file is schema v{}, newer than this build's v{SCHEMA_VERSION}. Update coppice, or move the file aside and start fresh",
                snap.v
            ));
        }
        *self.w() = snap;
        Ok(())
    }

    pub fn create_task(&self, mut tk: Task) -> Result<Task, LayoutError> {
        let mut s = self.w();
        if !tk.parent.is_empty() && !s.tasks.contains_key(&tk.parent) {
            return Err(LayoutError::NoTask(tk.parent.clone()));
        }
        tk.id = format!("t{}", s.next_task);
        s.next_task += 1;
        tk.children = Vec::new();
        s.tasks.insert(tk.id.clone(), tk.clone());
        if !tk.parent.is_empty() {
            s.tasks
                .get_mut(&tk.parent)
                .expect("checked above")
                .children
                .push(tk.id.clone());
        }
        Ok(tk)
    }

    pub fn task(&self, id: &str) -> Option<Task> {
        self.r().tasks.get(id).cloned()
    }

    /// Every task, sorted by number.
    pub fn tasks(&self) -> Vec<Task> {
        let mut out: Vec<Task> = self.r().tasks.values().cloned().collect();
        out.sort_by(|a, b| task_less(&a.id, &b.id));
        out
    }

    fn descends(s: &Snapshot, root: &str, id: &str, seen: &mut HashSet<String>) -> bool {
        if !seen.insert(root.to_string()) {
            return false;
        }
        let Some(tk) = s.tasks.get(root) else {
            return false;
        };
        for c in &tk.children {
            if c == id || Tree::descends(s, c, id, seen) {
                return true;
            }
        }
        false
    }

    fn drop_child(s: &mut Snapshot, parent: &str, id: &str) {
        if let Some(p) = s.tasks.get_mut(parent) {
            p.children.retain(|c| c != id);
        }
    }

    pub fn set_foreman(&self, id: &str, pane: &str) -> Result<(), LayoutError> {
        let mut s = self.w();
        let tk = s
            .tasks
            .get_mut(id)
            .ok_or_else(|| LayoutError::NoTask(id.into()))?;
        tk.foreman = pane.into();
        Ok(())
    }

    /// Clears the foreman on every task that names pane.
    pub fn drop_foreman(&self, pane: &str) {
        let mut s = self.w();
        for tk in s.tasks.values_mut() {
            if tk.foreman == pane {
                tk.foreman.clear();
            }
        }
    }

    /// Puts a task under a new parent; an empty parent detaches it.
    pub fn move_task(&self, id: &str, parent: &str) -> Result<(), LayoutError> {
        let mut s = self.w();
        if !s.tasks.contains_key(id) {
            return Err(LayoutError::NoTask(id.into()));
        }
        if !parent.is_empty() {
            if !s.tasks.contains_key(parent) {
                return Err(LayoutError::NoTask(parent.into()));
            }
            if parent == id || Tree::descends(&s, id, parent, &mut HashSet::new()) {
                return Err(LayoutError::Cycle(id.into(), parent.into()));
            }
        }
        let cur = s.tasks[id].parent.clone();
        if cur == parent {
            return Ok(());
        }
        if !cur.is_empty() {
            Tree::drop_child(&mut s, &cur, id);
        }
        s.tasks.get_mut(id).expect("checked").parent = parent.into();
        if !parent.is_empty() {
            s.tasks
                .get_mut(parent)
                .expect("checked")
                .children
                .push(id.into());
        }
        Ok(())
    }

    fn collect(s: &Snapshot, id: &str, gone: &mut HashSet<String>) {
        gone.insert(id.to_string());
        if let Some(tk) = s.tasks.get(id) {
            for c in &tk.children {
                if !gone.contains(c) {
                    Tree::collect(s, c, gone);
                }
            }
        }
    }

    /// Removes a task and every descendant, and clears the task on each
    /// pane that worked for them.
    pub fn close_task(&self, id: &str) -> Result<(), LayoutError> {
        let mut s = self.w();
        let parent = s
            .tasks
            .get(id)
            .map(|t| t.parent.clone())
            .ok_or_else(|| LayoutError::NoTask(id.into()))?;
        if !parent.is_empty() {
            Tree::drop_child(&mut s, &parent, id);
        }
        let mut gone = HashSet::new();
        Tree::collect(&s, id, &mut gone);
        for t in &gone {
            s.tasks.remove(t);
        }
        for p in s.panes.values_mut() {
            if gone.contains(&p.task_id) {
                p.task_id.clear();
            }
        }
        Ok(())
    }

    /// The task and every descendant, the task first, then each child's
    /// subtree in order.
    pub fn subtree(&self, id: &str) -> Result<Vec<Task>, LayoutError> {
        let s = self.r();
        if !s.tasks.contains_key(id) {
            return Err(LayoutError::NoTask(id.into()));
        }
        let mut out = Vec::new();
        let mut seen = HashSet::new();
        fn walk(s: &Snapshot, id: &str, seen: &mut HashSet<String>, out: &mut Vec<Task>) {
            let Some(tk) = s.tasks.get(id) else {
                return;
            };
            if !seen.insert(id.to_string()) {
                return;
            }
            out.push(tk.clone());
            for c in &tk.children {
                walk(s, c, seen, out);
            }
        }
        walk(&s, id, &mut seen, &mut out);
        Ok(out)
    }
}

/// The state that needs a person most among states. Nothing folds to "".
pub fn worst(states: &[String]) -> String {
    let rank = |s: &str| match s {
        "blocked" => 5,
        "working" => 4,
        "idle" => 3,
        "unknown" => 2,
        "done" => 1,
        _ => 0,
    };
    let mut out = String::new();
    let mut best = -1;
    for s in states {
        let r = rank(s);
        if r > best {
            best = r;
            out = s.clone();
        }
    }
    out
}

/// Folds the state of every pane in the task and of every child task,
/// recursively, with worst.
pub fn task_state(tk: &Task, tree: &Tree, pane_state: &dyn Fn(&str) -> String) -> String {
    let mut seen = HashSet::new();
    seen.insert(tk.id.clone());
    task_state_in(tk, tree, pane_state, &mut seen)
}

fn task_state_in(
    tk: &Task,
    tree: &Tree,
    pane_state: &dyn Fn(&str) -> String,
    seen: &mut HashSet<String>,
) -> String {
    let mut states = Vec::new();
    for p in tree.panes() {
        if p.task_id == tk.id {
            states.push(pane_state(&p.id));
        }
    }
    for cid in &tk.children {
        if !seen.insert(cid.clone()) {
            continue;
        }
        if let Some(child) = tree.task(cid) {
            states.push(task_state_in(&child, tree, pane_state, seen));
        }
    }
    worst(&states)
}

fn rand_suffix() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let n = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("{}{}", std::process::id(), n % 1_000_000_007)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ids_are_workspace_scoped_and_never_reused() {
        let t = Tree::new();
        let ws = t.create_workspace("", "/");
        let tab = t.create_tab(&ws.id, "main").unwrap();
        let p1 = t.create_pane(&ws.id, &tab.id, Pane::default()).unwrap();
        assert_eq!(p1.id, "w1:p1");
        t.remove_pane(&p1.id).unwrap();
        let p2 = t.create_pane(&ws.id, &tab.id, Pane::default()).unwrap();
        assert_eq!(p2.id, "w1:p2");
        assert_eq!((p2.cols, p2.rows), (120, 40));
    }

    #[test]
    fn a_task_cannot_move_under_its_child() {
        let t = Tree::new();
        let a = t
            .create_task(Task {
                label: "a".into(),
                ..Default::default()
            })
            .unwrap();
        let b = t
            .create_task(Task {
                label: "b".into(),
                parent: a.id.clone(),
                ..Default::default()
            })
            .unwrap();
        assert!(matches!(
            t.move_task(&a.id, &b.id),
            Err(LayoutError::Cycle(..))
        ));
        t.move_task(&b.id, "").unwrap();
        assert!(t.task(&a.id).unwrap().children.is_empty());
        t.close_task(&a.id).unwrap();
        assert!(t.task(&b.id).is_some());
    }

    /// A scratch file under TMPDIR, which the test run sets.
    fn scratch_file(name: &str) -> std::path::PathBuf {
        let d = std::env::temp_dir().join(format!("layout-{name}-{}", std::process::id()));
        std::fs::create_dir_all(&d).unwrap();
        d.join("layout.json")
    }

    #[test]
    fn a_saved_tree_loads_back_the_same() {
        let t = Tree::new();
        let ws = t.create_workspace("w", "/a");
        let tab = t.create_tab(&ws.id, "main").unwrap();
        let mut env = BTreeMap::new();
        env.insert("A".to_string(), "1".to_string());
        let p = t
            .create_pane(
                &ws.id,
                &tab.id,
                Pane {
                    argv: Some(vec!["sh".into()]),
                    env,
                    kind: KIND_PTY.into(),
                    extra_args: vec!["x".into()],
                    ..Default::default()
                },
            )
            .unwrap();
        t.close_pane(&p.id, Some(3), 99).unwrap();
        let a = t
            .create_task(Task {
                label: "a".into(),
                ..Default::default()
            })
            .unwrap();
        t.create_task(Task {
            label: "b".into(),
            parent: a.id,
            ..Default::default()
        })
        .unwrap();
        let path = scratch_file("same");
        t.save(&path).unwrap();
        let back = Tree::new();
        back.load(&path).unwrap();
        assert_eq!(
            gojson::marshal_indent(&back.r().to_value(), "  "),
            gojson::marshal_indent(&t.r().to_value(), "  ")
        );
        let _ = std::fs::remove_dir_all(path.parent().unwrap());
    }

    #[test]
    fn load_repairs_counters_and_refuses_a_newer_schema() {
        let path = scratch_file("repair");
        std::fs::write(
            &path,
            r#"{"workspaces":{"w1":{"id":"w1","tab_ids":["w1:t2"],"next_pane":1}},
               "tabs":{"w1:t2":{"id":"w1:t2","workspace":"w1","pane_ids":["w1:p7"]}},
               "panes":{"w1:p7":{"id":"w1:p7","workspace":"w1","tab":"w1:t2","closed":true},
                        "w1:px":{"id":"w1:px"}},
               "tasks":{"t4":{"id":"t4","label":"x"}},"next_task":2}"#,
        )
        .unwrap();
        let t = Tree::new();
        t.load(&path).unwrap();
        {
            let s = t.r();
            assert_eq!(s.v, SCHEMA_VERSION);
            assert_eq!(s.next_ws, 2);
            assert_eq!(s.next_task, 5);
            assert_eq!(s.workspaces["w1"].next_pane, 8);
            assert_eq!(s.workspaces["w1"].next_tab, 3);
        }
        let ws = t.create_workspace("", "/");
        assert_eq!(ws.id, "w2");

        std::fs::write(&path, r#"{"v":2}"#).unwrap();
        let err = Tree::new().load(&path).unwrap_err();
        assert!(
            err.starts_with("layout file is schema v2, newer than this build's v1."),
            "{err}"
        );
        std::fs::write(&path, "{").unwrap();
        assert!(Tree::new()
            .load(&path)
            .unwrap_err()
            .starts_with("layout file is not readable: "));
        std::fs::write(&path, r#"{"panes":{"w1:p1":{"cols":"wide"}}}"#).unwrap();
        assert!(Tree::new()
            .load(&path)
            .unwrap_err()
            .starts_with("layout file is not readable: "));
        let _ = std::fs::remove_dir_all(path.parent().unwrap());
    }

    #[test]
    fn the_saved_shape_matches_go() {
        let t = Tree::new();
        let ws = t.create_workspace("", "/");
        t.create_tab(&ws.id, "main").unwrap();
        let v = t.r().to_value();
        let s = gojson::marshal_indent(&v, "  ");
        assert!(s.starts_with("{\n  \"v\": 1,\n  \"workspaces\": {\n    \"w1\": {"));
        assert!(s.contains("\"pane_ids\": null"));
        assert!(s.contains("\"tasks\": {}"));
    }
}
