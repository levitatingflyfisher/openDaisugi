//! Child stores: a worker's whole transcript as a small store of its own,
//! under the store that spawned it. The twin of Go's pando child.go.
//!
//! A child is never merged into the parent's tree and has no view in the
//! parent's. A child lives under the store that spawned it, so a home child
//! is under home and a work child under work.

use std::path::{Path, PathBuf};

use super::chat::{Chat, Clock, Config, RedoSet, Summarizer, ERR_LOCKED};
use super::ctx::Ctx;
use super::tend::{redo_calls, tend_calls, TendRun};

/// The dir under a store that holds the transcript trees of its workers:
/// <store>/children/<label>/.
pub const CHILDREN_DIR: &str = "children";

/// Whether label can name a child store: a letter or digit, then letters,
/// digits, dot, dash and underscore, 64 at most; not "..".
pub fn valid_child(label: &str) -> bool {
    let b = label.as_bytes();
    if b.is_empty() || b.len() > 64 || label == ".." {
        return false;
    }
    if !b[0].is_ascii_alphanumeric() {
        return false;
    }
    b[1..]
        .iter()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'-'))
}

/// The dir of the child store for a worker label. The children dir and
/// the label must not be symbolic links: a link could reach another store.
pub fn child_dir(store_dir: &Path, label: &str) -> Result<PathBuf, String> {
    if !valid_child(label) {
        return Err(format!(
            "bad worker label {}: use letters, digits, dot, dash and underscore, 1 to 64, starting with a letter or digit",
            crate::sys::go_quote(label)
        ));
    }
    crate::store::no_link(store_dir, &[CHILDREN_DIR, label])?;
    Ok(store_dir.join(CHILDREN_DIR).join(label))
}

/// Names the child stores of a store, sorted.
pub fn list_children(store_dir: &Path) -> Vec<String> {
    let root = store_dir.join(CHILDREN_DIR);
    let Ok(rd) = std::fs::read_dir(&root) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        if !e.file_type().is_ok_and(|t| t.is_dir()) || !valid_child(&name) {
            continue;
        }
        if root.join(&name).join("main").is_dir() {
            out.push(name);
        }
    }
    out.sort();
    out
}

/// Opens (and makes) the child store of a worker for writing.
pub fn open_child(store_dir: &Path, label: &str, mut cfg: Config) -> Result<Chat, String> {
    cfg.dir = child_dir(store_dir, label)?;
    Chat::open(cfg)
}

/// Builds the pending nodes of one child store with s. A child that
/// another process has open is left for that process (0, None).
pub fn tend_child(
    ctx: &Ctx,
    store_dir: &Path,
    label: &str,
    s: &dyn Summarizer,
    max: usize,
    now: Option<Clock>,
) -> (usize, Option<String>) {
    let mut run = TendRun::default();
    let err = tend_child_calls(ctx, store_dir, label, s, max, now, &mut run);
    (run.built, err)
}

fn tend_child_calls(
    ctx: &Ctx,
    store_dir: &Path,
    label: &str,
    s: &dyn Summarizer,
    max: usize,
    now: Option<Clock>,
    run: &mut TendRun,
) -> Option<String> {
    let d = match child_dir(store_dir, label) {
        Ok(d) => d,
        Err(e) => return Some(e),
    };
    let mut c = match Chat::open(Config {
        dir: d,
        now,
        ..Default::default()
    }) {
        Ok(c) => c,
        Err(e) if e == ERR_LOCKED => return None,
        Err(e) => return Some(e),
    };
    let r = tend_calls(ctx, &mut c, s, max, run);
    c.close();
    r
}

/// Builds the pending nodes of every child store of a store with s, the
/// same summarizer and the same rule as the parent. max caps the model
/// calls across all children in this run (0 means all). It returns how many
/// nodes it built and the first error.
pub fn tend_children(
    ctx: &Ctx,
    store_dir: &Path,
    s: &dyn Summarizer,
    max: usize,
    now: Option<Clock>,
) -> (usize, Option<String>) {
    let mut run = TendRun::default();
    let err = tend_children_calls(ctx, store_dir, s, max, now, &mut run);
    (run.built, err)
}

/// `tend_children` that counts into run. max caps the model calls of this
/// run across the children, counted from run.spent at the start.
pub fn tend_children_calls(
    ctx: &Ctx,
    store_dir: &Path,
    s: &dyn Summarizer,
    max: usize,
    now: Option<Clock>,
    run: &mut TendRun,
) -> Option<String> {
    let start = run.spent;
    for ss in super::branch::sub_stores(store_dir) {
        let mut left = 0;
        if max > 0 {
            if run.spent - start >= max {
                break;
            }
            left = max - (run.spent - start);
        }
        if let Some(e) = tend_dir_calls(ctx, &ss.dir, s, left, now.clone(), run) {
            return Some(e);
        }
    }
    None
}

/// Builds the pending nodes of the store in dir with s. A store another
/// process has open is left for that process (0, None).
pub fn tend_dir(ctx: &Ctx, dir: &Path, s: &dyn Summarizer, max: usize, now: Option<Clock>) -> (usize, Option<String>) {
    let mut run = TendRun::default();
    let err = tend_dir_calls(ctx, dir, s, max, now, &mut run);
    (run.built, err)
}

fn tend_dir_calls(
    ctx: &Ctx,
    dir: &Path,
    s: &dyn Summarizer,
    max: usize,
    now: Option<Clock>,
    run: &mut TendRun,
) -> Option<String> {
    let mut c = match Chat::open(Config {
        dir: dir.to_path_buf(),
        now,
        ..Default::default()
    }) {
        Ok(c) => c,
        Err(e) if e == ERR_LOCKED => return None,
        Err(e) => return Some(e),
    };
    let r = tend_calls(ctx, &mut c, s, max, run);
    c.close();
    r
}

/// `redo_calls` over the stores below a store: its children and its
/// branches with theirs (sub_stores).
pub fn redo_children(
    ctx: &Ctx,
    store_dir: &Path,
    s: &dyn Summarizer,
    set: RedoSet,
    max: usize,
    now: Option<Clock>,
    run: &mut TendRun,
) -> Option<String> {
    let start = run.spent;
    for ss in super::branch::sub_stores(store_dir) {
        let mut left = 0;
        if max > 0 {
            if run.spent - start >= max {
                break;
            }
            left = max - (run.spent - start);
        }
        let d = ss.dir;
        let mut c = match Chat::open(Config {
            dir: d.clone(),
            now: now.clone(),
            ..Default::default()
        }) {
            Ok(c) => c,
            Err(e) if e == ERR_LOCKED => {
                run.locked.push(d);
                continue;
            }
            Err(e) => return Some(e),
        };
        let r = redo_calls(ctx, &mut c, s, set, left, run);
        c.close();
        if r.is_some() {
            return r;
        }
    }
    None
}

impl Chat {
    /// Names the root nodes of a child store, for the work message and for
    /// zoom: "label:first+count" for each, oldest first.
    pub fn child_roots(&self, label: &str) -> Vec<String> {
        self.roots()
            .into_iter()
            .map(|r| format!("{label}:{}", r.name()))
            .collect()
    }
}
