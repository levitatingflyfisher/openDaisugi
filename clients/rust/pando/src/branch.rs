//! Branches (round 8b): one tree, shaped like an org chart. The twin of Go's
//! pando branch.go.
//!
//! A long-running agent gets a branch: a store of its own inside the store
//! above it, <store>/branches/<label>/, with the same layout. Everything it
//! says goes into its branch, never into the log above it. It sees its
//! branch and everything below it; whoever is above sees down. A branch is
//! not a second store (PD-R-2 stands): it lives inside one store.

use std::collections::HashMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use super::chat::{date_text, flat, Chat, Clock, Config, ERR_LOCKED};
use super::child::{child_dir, list_children, valid_child, CHILDREN_DIR};
use super::fsutil::{clip, ensure_dir_plain, json_str, write_atomic, FILE_MODE};
use super::live::{load_skip, open_wait, skip_list_path};
use super::recall::{Embed, RecallHit, RECALL_MAX};
use super::store::no_link;
use crate::sys::go_quote;

/// The dir under a store (or a branch) that holds its branches.
pub const BRANCHES_DIR: &str = "branches";
/// Marks a branch dir and holds its state.
pub const BRANCH_FILE: &str = "branch.json";
/// Names the branch of this process.
pub const BRANCH_VAR: &str = "DAISUGI_PANDO_BRANCH";
/// The deepest branch path.
pub const MAX_BRANCH_DEPTH: usize = 8;
pub const BRANCH_OPEN: &str = "open";
pub const BRANCH_CLOSED: &str = "closed";
/// The session map of a store: one "id<TAB>branch" line per session.
pub const ROUTE_FILE: &str = "branch.sessions";
/// Caps the text of one branch line in a view.
pub const BRANCH_LINE_BYTES: usize = 200;
/// Starts the branch lines of a view.
pub const BRANCH_LINES_HEAD: &str =
    "Branches below (the newest summary of each live branch; recall searches them, zoom with branch reads them):\n";

/// branch.json.
#[derive(Clone, Default, Debug)]
pub struct BranchInfo {
    pub path: String,
    pub created: String,
    pub state: String,
    pub closed: String,
}

impl BranchInfo {
    fn encode(&self) -> String {
        let mut s = format!(
            "{{\"path\":{},\"created\":{},\"state\":{}",
            json_str(&self.path),
            json_str(&self.created),
            json_str(&self.state)
        );
        if !self.closed.is_empty() {
            s.push_str(&format!(",\"closed\":{}", json_str(&self.closed)));
        }
        s.push_str("}\n");
        s
    }
}

/// Splits a branch path ("trellis/ui") into its labels.
pub fn parse_branch(path: &str) -> Result<Vec<String>, String> {
    if path.is_empty() {
        return Err("the branch is empty".into());
    }
    if path.starts_with('/') {
        return Err(format!(
            "bad branch {}: a branch is a path of labels inside the store, never an absolute path",
            go_quote(path)
        ));
    }
    let parts: Vec<String> = path.split('/').map(|s| s.to_string()).collect();
    if parts.len() > MAX_BRANCH_DEPTH {
        return Err(format!("bad branch {}: at most {} levels", go_quote(path), MAX_BRANCH_DEPTH));
    }
    for p in &parts {
        if p.is_empty() || p == "." || p == ".." || !valid_child(p) {
            return Err(format!(
                "bad branch {}: each part is a label of letters, digits, dot, dash and underscore, 1 to 64, starting with a letter or digit",
                go_quote(path)
            ));
        }
    }
    Ok(parts)
}

/// The path of sub below base ("" is the store).
pub fn join_branch(base: &str, sub: &str) -> String {
    if base.is_empty() {
        return sub.to_string();
    }
    if sub.is_empty() {
        return base.to_string();
    }
    format!("{base}/{sub}")
}

/// The dir of the branch path below the store (or branch) in store_dir.
/// Each level is checked: no part may be a symbolic link.
pub fn branch_dir(store_dir: &Path, path: &str) -> Result<PathBuf, String> {
    let parts = parse_branch(path)?;
    let mut d = store_dir.to_path_buf();
    for p in &parts {
        no_link(&d, &[BRANCHES_DIR, p])?;
        d = d.join(BRANCHES_DIR).join(p);
    }
    Ok(d)
}

/// branch_dir for a branch that exists: every level holds branch.json.
pub fn existing_branch(store_dir: &Path, path: &str) -> Result<PathBuf, String> {
    let d = branch_dir(store_dir, path)?;
    let parts = parse_branch(path)?;
    let mut cur = store_dir.to_path_buf();
    for p in &parts {
        cur = cur.join(BRANCHES_DIR).join(p);
        if !is_branch(&cur) {
            return Err(format!("no branch {path}"));
        }
    }
    Ok(d)
}

/// Whether dir is a branch: a real dir with a branch.json.
pub fn is_branch(dir: &Path) -> bool {
    std::fs::symlink_metadata(dir.join(BRANCH_FILE)).is_ok_and(|m| m.file_type().is_file())
}

/// Reads branch.json.
pub fn read_branch(dir: &Path) -> Result<BranchInfo, std::io::Error> {
    let b = std::fs::read(dir.join(BRANCH_FILE))?;
    let v: serde_json::Value = serde_json::from_slice(&b)
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e.to_string()))?;
    let s = |k: &str| v.get(k).and_then(|x| x.as_str()).unwrap_or("").to_string();
    let mut bi = BranchInfo { path: s("path"), created: s("created"), state: s("state"), closed: s("closed") };
    if bi.state.is_empty() {
        bi.state = BRANCH_OPEN.into();
    }
    Ok(bi)
}

fn write_branch(dir: &Path, bi: &BranchInfo) -> Result<(), String> {
    write_atomic(&dir.join(BRANCH_FILE), bi.encode().as_bytes())
}

fn now_of(now: &Option<Clock>) -> SystemTime {
    match now {
        Some(f) => f(),
        None => SystemTime::now(),
    }
}

/// Makes the branch and every missing level above it, each with its
/// branch.json, and returns its dir. A closed branch stays closed (F5).
pub fn ensure_branch(store_dir: &Path, path: &str, now: &Option<Clock>) -> Result<PathBuf, String> {
    ensure_branch_with(store_dir, path, now, false)
}

/// ensure_branch that also opens a closed branch again: a new lead.
pub fn reopen_branch(store_dir: &Path, path: &str, now: &Option<Clock>) -> Result<PathBuf, String> {
    ensure_branch_with(store_dir, path, now, true)
}

fn ensure_branch_with(store_dir: &Path, path: &str, now: &Option<Clock>, reopen: bool) -> Result<PathBuf, String> {
    let parts = parse_branch(path)?;
    branch_dir(store_dir, path)?;
    let mut cur = store_dir.to_path_buf();
    for (k, p) in parts.iter().enumerate() {
        cur = cur.join(BRANCHES_DIR).join(p);
        ensure_dir_plain(&cur)?;
        no_link(cur.parent().unwrap_or(store_dir), &[p])?;
        let here = parts[..=k].join("/");
        let bi = match read_branch(&cur) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => BranchInfo {
                path: here,
                created: date_text(now_of(now)),
                state: BRANCH_OPEN.into(),
                closed: String::new(),
            },
            Err(e) => return Err(e.to_string()),
            Ok(bi) if bi.state == BRANCH_OPEN || !reopen => continue,
            Ok(mut bi) => {
                bi.state = BRANCH_OPEN.into();
                bi.closed.clear();
                bi
            }
        };
        write_branch(&cur, &bi)?;
    }
    Ok(cur)
}

/// Marks a branch closed. Its messages stay and recall finds them.
pub fn close_branch(store_dir: &Path, path: &str, now: &Option<Clock>) -> Result<(), String> {
    let d = existing_branch(store_dir, path)?;
    let mut bi = read_branch(&d).map_err(|e| e.to_string())?;
    if bi.state == BRANCH_CLOSED {
        return Ok(());
    }
    bi.state = BRANCH_CLOSED.into();
    bi.closed = date_text(now_of(now));
    write_branch(&d, &bi)
}

/// Names the branches right below dir, sorted.
pub fn list_branches(dir: &Path) -> Vec<String> {
    if no_link(dir, &[BRANCHES_DIR]).is_err() {
        return Vec::new();
    }
    let Ok(rd) = std::fs::read_dir(dir.join(BRANCHES_DIR)) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        if !e.file_type().is_ok_and(|t| t.is_dir()) || !valid_child(&name) {
            continue;
        }
        if is_branch(&dir.join(BRANCHES_DIR).join(&name)) {
            out.push(name);
        }
    }
    out.sort();
    out
}

/// Names every branch below dir, depth first, each as its path from dir.
pub fn branch_tree(dir: &Path) -> Vec<String> {
    fn walk(d: &Path, base: &str, depth: usize, out: &mut Vec<String>) {
        if depth >= MAX_BRANCH_DEPTH {
            return;
        }
        for b in list_branches(d) {
            let p = join_branch(base, &b);
            out.push(p.clone());
            walk(&d.join(BRANCHES_DIR).join(&b), &p, depth + 1, out);
        }
    }
    let mut out = Vec::new();
    walk(dir, "", 0, &mut out);
    out
}

/// Whether dir holds a log.
pub fn has_main(dir: &Path) -> bool {
    dir.join("main").is_dir()
}

/// A store below a store: a worker's child store or a branch.
pub struct SubStore {
    pub rel: String,
    pub dir: PathBuf,
}

/// Every store below dir that tend builds: its children, then each branch
/// and the children and branches below it, depth first.
pub fn sub_stores(dir: &Path) -> Vec<SubStore> {
    let mut out = Vec::new();
    for ch in list_children(dir) {
        if let Ok(d) = child_dir(dir, &ch) {
            out.push(SubStore { rel: format!("{ch}/"), dir: d });
        }
    }
    fn walk(d: &Path, rel: &str, depth: usize, out: &mut Vec<SubStore>) {
        if depth >= MAX_BRANCH_DEPTH {
            return;
        }
        for b in list_branches(d) {
            let bd = d.join(BRANCHES_DIR).join(&b);
            let br = format!("{rel}{BRANCHES_DIR}/{b}/");
            if has_main(&bd) {
                out.push(SubStore { rel: br.clone(), dir: bd.clone() });
            }
            for ch in list_children(&bd) {
                if let Ok(cd) = child_dir(&bd, &ch) {
                    out.push(SubStore { rel: format!("{br}{CHILDREN_DIR}/{ch}/"), dir: cd });
                }
            }
            walk(&bd, &br, depth + 1, out);
        }
    }
    walk(dir, "", 0, &mut out);
    out
}

// ---- the scope of a process ----

/// What a process may read: dir and everything below it.
#[derive(Clone, Default, Debug)]
pub struct Scope {
    pub store: PathBuf,
    pub dir: PathBuf,
    pub branch: String,
    pub worker: bool,
    pub closed: bool,
    pub why: String,
}

/// The scope of a process with getenv over the store in store_dir. No
/// marker is the store. A worker with no branch is closed. A branch marker
/// that is bad, a link or not there is closed: never the store above.
pub fn scope_of(store_dir: &Path, getenv: Option<&dyn Fn(&str) -> String>) -> Scope {
    let mut sc = Scope { store: store_dir.to_path_buf(), dir: store_dir.to_path_buf(), ..Default::default() };
    let Some(g) = getenv else {
        return sc;
    };
    let skip = super::live::skip_on(getenv);
    let branch = g(BRANCH_VAR).trim().to_string();
    if branch.is_empty() {
        if skip {
            sc.closed = true;
            sc.why = super::mcp::CLOSED_TEXT.into();
            sc.dir = PathBuf::new();
        }
        return sc;
    }
    sc.branch = branch.clone();
    sc.worker = skip;
    match existing_branch(store_dir, &branch) {
        Ok(d) => sc.dir = d,
        Err(e) => {
            sc.closed = true;
            sc.why = format!("The memory is closed to this process: its branch {branch} cannot be read ({e}).");
            sc.dir = PathBuf::new();
        }
    }
    sc
}

impl Scope {
    /// A branch path given relative to the scope ("" is the scope itself).
    pub fn below(&self, sub: &str) -> Result<PathBuf, String> {
        if sub.is_empty() {
            return Ok(self.dir.clone());
        }
        existing_branch(&self.dir, sub)
    }
}

// ---- the session map ----

/// The session map: id -> branch, the first line for an id wins.
pub type Routes = HashMap<String, String>;

/// Reads the session map of a store. A missing file is empty.
pub fn load_routes(store_dir: &Path) -> Routes {
    let mut rt = Routes::new();
    let Ok(text) = std::fs::read(store_dir.join(ROUTE_FILE)) else {
        return rt;
    };
    for line in String::from_utf8_lossy(&text).lines() {
        let Some((id, br)) = line.split_once('\t') else {
            continue;
        };
        let (id, br) = (id.trim(), br.trim());
        if id.is_empty() || br.is_empty() || (parse_branch(br).is_err() && br != ROOT_CLAIM) {
            continue;
        }
        rt.entry(id.to_string()).or_insert_with(|| br.to_string());
    }
    rt
}

/// The branch of a session, by its id, its transcript path or the stem of
/// the path, or "".
pub fn routes_for(rt: &Routes, session_id: &str, path: &str) -> String {
    if rt.is_empty() {
        return String::new();
    }
    if !path.is_empty() {
        if let Some(b) = rt.get(path) {
            return b.clone();
        }
        let stem = Path::new(path).file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
        if let Some(b) = rt.get(stem.strip_suffix(".jsonl").unwrap_or(&stem)) {
            return b.clone();
        }
    }
    if !session_id.is_empty() {
        if let Some(b) = rt.get(session_id) {
            return b.clone();
        }
    }
    String::new()
}

/// The branch field of a line that claims a session for the store itself
/// (the owner's SessionStart writes it; G5).
pub const ROOT_CLAIM: &str = "-";

/// Puts ids on the map as the store's own; an id with a line keeps it.
pub fn claim_root(store_dir: &Path, ids: &[&str]) -> Result<(), String> {
    let mut have = load_routes(store_dir);
    let mut b = String::new();
    for id in ids {
        let id = id.trim();
        if id.is_empty() || id.contains(['\t', '\r', '\n']) || have.contains_key(id) {
            continue;
        }
        have.insert(id.to_string(), ROOT_CLAIM.to_string());
        b.push_str(&format!("{id}\t{ROOT_CLAIM}\n"));
    }
    if b.is_empty() {
        return Ok(());
    }
    append_routes(store_dir, &b)
}

fn append_routes(store_dir: &Path, text: &str) -> Result<(), String> {
    ensure_dir_plain(store_dir)?;
    no_link(store_dir, &[ROUTE_FILE])?;
    use std::os::unix::fs::OpenOptionsExt;
    let p = store_dir.join(ROUTE_FILE);
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .mode(FILE_MODE)
        .open(&p)
        .map_err(|e| super::fsutil::path_err("open", &p, &e))?;
    f.write_all(text.as_bytes()).map_err(|e| e.to_string())
}

/// Starts a dir route: "dir:<abs dir>".
pub const DIR_ROUTE_PREFIX: &str = "dir:";

/// The branch of the dir route that holds cwd, the longest dir first.
pub fn routes_for_cwd(rt: &Routes, cwd: &str) -> String {
    if cwd.is_empty() {
        return String::new();
    }
    let mut best = (String::new(), -1i64);
    for (k, b) in rt {
        let Some(d) = k.strip_prefix(DIR_ROUTE_PREFIX) else { continue };
        if d.is_empty() || !d.starts_with('/') {
            continue;
        }
        if super::import::under_any_pub(cwd, &[d.to_string()]) && d.len() as i64 > best.1 {
            best = (b.clone(), d.len() as i64);
        }
    }
    best.0
}

/// Whether the store's own log already holds messages of the session id
/// names (a Claude id or transcript path, or "hermes:<id>").
pub fn root_holds(store_dir: &Path, id: &str) -> bool {
    let (a, b): (String, Option<String>) = if let Some(sid) = id.strip_prefix("hermes:") {
        let q = json_str(&format!(":{sid}:"));
        ("\"act\":\"hermes:".to_string(), Some(q[1..q.len() - 1].to_string()))
    } else {
        let sid = if id.contains('/') {
            let base = Path::new(id).file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
            base.strip_suffix(".jsonl").unwrap_or(&base).to_string()
        } else {
            id.to_string()
        };
        if sid.is_empty() {
            return false;
        }
        let q = json_str(&sid);
        let whole = format!("\"chat\":{q}");
        let forked = format!("\"chat\":{}~", &q[..q.len() - 1]);
        return scan_main(store_dir, |l| l.contains(&whole) || l.contains(&forked));
    };
    let b = b.unwrap_or_default();
    scan_main(store_dir, |l| l.contains(&a) && l.contains(&b))
}

fn scan_main(store_dir: &Path, m: impl Fn(&str) -> bool) -> bool {
    let main = store_dir.join("main");
    let Ok(rd) = std::fs::read_dir(&main) else { return false };
    for e in rd.flatten() {
        let n = e.file_name().to_string_lossy().into_owned();
        // Day files only, as Go's dayFiles reads them.
        let day = n.strip_suffix(".jsonl").unwrap_or("");
        let is_day = day.len() == 10
            && day.bytes().enumerate().all(|(i, c)| if i == 4 || i == 7 { c == b'-' } else { c.is_ascii_digit() });
        if !is_day || !e.file_type().is_ok_and(|t| t.is_file()) {
            continue;
        }
        let Ok(b) = std::fs::read(main.join(&n)) else { continue };
        if String::from_utf8_lossy(&b).lines().any(|l| !l.is_empty() && m(l)) {
            return true;
        }
    }
    false
}

/// Puts ids on the session map of the store, for branch, in one O_APPEND
/// write. An id that is on the map already is not written again; an id the
/// store already holds is refused (the error names it).
pub fn add_route(store_dir: &Path, branch: &str, ids: &[&str]) -> Result<(), String> {
    parse_branch(branch)?;
    let mut have = load_routes(store_dir);
    let mut b = String::new();
    let mut held: Vec<String> = Vec::new();
    for id in ids {
        let id = id.trim();
        if id.is_empty() || id.contains(['\t', '\r', '\n']) || have.contains_key(id) {
            continue;
        }
        if !id.starts_with(DIR_ROUTE_PREFIX) && root_holds(store_dir, id) {
            held.push(id.to_string());
            continue;
        }
        have.insert(id.to_string(), branch.to_string());
        b.push_str(&format!("{id}\t{branch}\n"));
    }
    let refused = if held.is_empty() {
        None
    } else {
        Some(format!("the store already holds these sessions, so they stay the owner's: {}", held.join(", ")))
    };
    if b.is_empty() {
        return refused.map_or(Ok(()), Err);
    }
    append_routes(store_dir, &b)?;
    refused.map_or(Ok(()), Err)
}

/// Takes every line for id off the session map, by writing the file again.
pub fn remove_route(store_dir: &Path, id: &str) -> Result<bool, String> {
    no_link(store_dir, &[ROUTE_FILE])?;
    let p = store_dir.join(ROUTE_FILE);
    let data = match std::fs::read(&p) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(e) => return Err(super::fsutil::path_err("open", &p, &e)),
    };
    let mut keep = String::new();
    let mut found = false;
    for line in String::from_utf8_lossy(&data).split('\n') {
        if line.is_empty() {
            continue;
        }
        let k = line.split('\t').next().unwrap_or("");
        if k.trim() == id {
            found = true;
            continue;
        }
        keep.push_str(line);
        keep.push('\n');
    }
    if !found {
        return Ok(false);
    }
    write_atomic(&p, keep.as_bytes())?;
    Ok(true)
}

/// The session map in file order, the first line for an id only.
pub fn route_rows(store_dir: &Path) -> Vec<(String, String)> {
    let Ok(data) = std::fs::read(store_dir.join(ROUTE_FILE)) else { return Vec::new() };
    let mut seen = std::collections::HashSet::new();
    let mut out = Vec::new();
    for line in String::from_utf8_lossy(&data).split('\n') {
        let Some((id, br)) = line.split_once('\t') else { continue };
        let (id, br) = (id.trim(), br.trim());
        if id.is_empty() || br.is_empty() || seen.contains(id) || (parse_branch(br).is_err() && br != ROOT_CLAIM) {
            continue;
        }
        seen.insert(id.to_string());
        out.push((id.to_string(), br.to_string()));
    }
    out
}

/// Mints a session id and puts it on the session map for branch first.
pub fn new_branch_session(store_dir: &Path, branch: &str) -> Result<String, String> {
    let id = super::live::new_uuid().map_err(|e| e.to_string())?;
    add_route(store_dir, branch, &[&id])?;
    Ok(id)
}

/// The skip list of a store with its session map added, for the importer.
pub fn skip_with_routes(store_dir: &Path) -> std::collections::HashSet<String> {
    let mut s = load_skip(&skip_list_path(store_dir));
    for (k, b) in load_routes(store_dir) {
        if !k.starts_with(DIR_ROUTE_PREFIX) && b != ROOT_CLAIM {
            s.insert(k);
        }
    }
    s
}

/// Sends each session's messages to its branch: the follow service and the
/// hooks of a chat with no branch use it over the store they hold open.
pub struct Router {
    store: PathBuf,
    now: Option<Clock>,
    routes: Routes,
    open: HashMap<String, Chat>,
    held: HashMap<String, bool>,
    pub wait: Duration,
}

impl Router {
    /// The router of the store in dir. A branch or a child routes nothing.
    pub fn new(dir: &Path, now: Option<Clock>) -> Router {
        let routes = if is_branch(dir) { Routes::new() } else { load_routes(dir) };
        Router { store: dir.to_path_buf(), now, routes, open: HashMap::new(), held: HashMap::new(), wait: Duration::from_secs(3) }
    }

    /// The branch of a session, or "".
    pub fn branch(&self, session_id: &str, path: &str) -> String {
        routes_for(&self.routes, session_id, path)
    }

    /// The branch a session's lines go to: its own line, else the dir route
    /// of its cwd; "" when nothing maps it or the store already holds it.
    pub fn target(&mut self, key: &str, path: &str, cwd: &str) -> String {
        if self.routes.is_empty() {
            return String::new();
        }
        let mut br = routes_for(&self.routes, key, path);
        if br == ROOT_CLAIM {
            return String::new();
        }
        if br.is_empty() {
            br = routes_for_cwd(&self.routes, cwd);
        }
        if br.is_empty() {
            return br;
        }
        let store = self.store.clone();
        let h = *self.held.entry(key.to_string()).or_insert_with(|| root_holds(&store, key));
        if h {
            return String::new();
        }
        br
    }

    /// The branch chat that takes the messages of branch, made when
    /// missing and opened for writing once per router.
    pub fn chat(&mut self, branch: &str) -> Result<&mut Chat, String> {
        if !self.open.contains_key(branch) {
            let d = ensure_branch(&self.store, branch, &self.now)?;
            let now = self.now.clone();
            let c = open_wait(|| Config { dir: d.clone(), now: now.clone(), ..Default::default() }, self.wait)?;
            self.open.insert(branch.to_string(), c);
        }
        Ok(self.open.get_mut(branch).expect("opened"))
    }
}

impl Drop for Router {
    fn drop(&mut self) {
        for (_, mut c) in self.open.drain() {
            c.close();
        }
    }
}

// ---- the view's branch lines and the branch list ----

/// The part of a view that names the live branches right below dir.
pub fn branch_lines(dir: &Path) -> String {
    let mut b = String::new();
    for name in list_branches(dir) {
        let d = dir.join(BRANCHES_DIR).join(&name);
        match read_branch(&d) {
            Ok(bi) if bi.state == BRANCH_OPEN => {}
            _ => continue,
        }
        if b.is_empty() {
            b.push_str(BRANCH_LINES_HEAD);
        }
        b.push_str(&format!("- {}: {}\n", name, branch_newest(&d)));
    }
    b
}

fn branch_newest(dir: &Path) -> String {
    const NONE: &str = "(no message yet)";
    if !has_main(dir) {
        return NONE.into();
    }
    let Ok(c) = Chat::open_read_only(dir) else {
        return NONE.into();
    };
    if c.count() == 0 {
        return NONE.into();
    }
    let roots = c.roots();
    if let Some(n) = roots.last().and_then(|r| c.node(*r)) {
        return clip(&flat(&n.text), BRANCH_LINE_BYTES).to_string();
    }
    match c.message(c.count() - 1) {
        Ok(m) => clip(&flat(&m.text), BRANCH_LINE_BYTES).to_string(),
        Err(_) => NONE.into(),
    }
}

/// One line of `daisugi pando branches`.
pub struct BranchRow {
    pub path: String,
    pub state: String,
    pub messages: i64,
    pub newest: String,
}

/// Every branch below dir, depth first.
pub fn branch_rows(dir: &Path) -> Vec<BranchRow> {
    let mut out = Vec::new();
    for p in branch_tree(dir) {
        let Ok(d) = existing_branch(dir, &p) else {
            continue;
        };
        let state = read_branch(&d).map(|b| b.state).unwrap_or_default();
        let mut row = BranchRow { path: p, state, messages: 0, newest: branch_newest(&d) };
        if has_main(&d) {
            if let Ok(c) = Chat::open_read_only(&d) {
                row.messages = c.count();
            }
        }
        out.push(row);
    }
    out
}

/// Recall over the store (or branch) in dir and every branch below it. A
/// hit in a branch has the id "<branch>:<first>+<count>".
pub fn recall_tree(dir: &Path, query: &str, k: usize, embed: Embed) -> Result<Vec<RecallHit>, String> {
    let k = if k == 0 { 8 } else { k.min(RECALL_MAX) };
    let mut all = Vec::new();
    if has_main(dir) {
        let top = Chat::open_read_only(dir)?;
        all = top.recall_cands(query, embed);
    } else if let Err(e) = std::fs::metadata(dir) {
        return Err(super::fsutil::path_err("stat", dir, &e));
    }
    for b in branch_tree(dir) {
        let Ok(d) = existing_branch(dir, &b) else {
            continue;
        };
        if !has_main(&d) {
            continue;
        }
        let Ok(c) = Chat::open_read_only(&d) else {
            continue;
        };
        for mut h in c.recall_cands(query, embed) {
            h.branch = b.clone();
            h.hit.id = format!("{b}:{}", h.hit.id);
            all.push(h);
        }
    }
    super::recall::sort_cands(&mut all);
    all.truncate(k);
    Ok(all.into_iter().map(|c| c.hit).collect())
}

/// The Claude hook of a long-running agent (see Go's RunBranchHook).
pub fn run_branch_hook(
    store_dir: &Path,
    branch: &str,
    ev: &super::live::HookEvent,
    skip: &[String],
    now: Option<Clock>,
) -> String {
    if branch_dir(store_dir, branch).is_err() {
        return String::new();
    }
    match ev.event.as_str() {
        "SessionStart" => {
            let _ = add_route(store_dir, branch, &[&ev.session_id, &ev.transcript_path]);
            match existing_branch(store_dir, branch) {
                Ok(d) => super::live::run_hook(&d, ev, skip, now),
                Err(_) => String::new(),
            }
        }
        "UserPromptSubmit" | "Stop" | "SessionEnd" | "PreCompact" => {
            if ev.transcript_path.is_empty() {
                return String::new();
            }
            let _ = add_route(store_dir, branch, &[&ev.session_id, &ev.transcript_path]);
            match ensure_branch(store_dir, branch, &now) {
                Ok(d) => super::live::run_hook(&d, ev, skip, now),
                Err(_) => String::new(),
            }
        }
        _ => String::new(),
    }
}

/// Opens for writing the first store below dir with nodes to build. A
/// store another process holds is passed over.
pub fn pending_sub(dir: &Path, now: &Option<Clock>) -> Result<Option<Chat>, String> {
    for ss in sub_stores(dir) {
        match Chat::open_read_only(&ss.dir) {
            Ok(ro) if ro.pending() > 0 => {}
            _ => continue,
        }
        match Chat::open(Config { dir: ss.dir.clone(), now: now.clone(), ..Default::default() }) {
            Ok(c) => return Ok(Some(c)),
            Err(e) if e == ERR_LOCKED => continue,
            Err(e) => return Err(e),
        }
    }
    Ok(None)
}

/// Whether a store below dir has nodes to build.
pub fn any_pending_sub(dir: &Path) -> bool {
    sub_stores(dir)
        .iter()
        .any(|ss| Chat::open_read_only(&ss.dir).is_ok_and(|c| c.pending() > 0))
}
