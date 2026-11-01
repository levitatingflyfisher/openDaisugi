//! Stores: the dirs under a data home that hold one Pando R2 chat each.
//! The twin of Go's clients/go/pando store.go.

use std::fmt;
use std::path::{Path, PathBuf};

/// The store a command uses when it names none.
pub const DEFAULT_STORE: &str = "work";

/// Why a store name or dir is refused.
#[derive(Clone, Debug, PartialEq)]
pub enum StoreError {
    /// --store all (PD-R-2): no command reads two stores.
    All,
    /// A name that is not a store name.
    Bad(String),
    /// A part of the path that is a symbolic link, or one that could not
    /// be checked: the whole message.
    Link(String),
}

impl fmt::Display for StoreError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            StoreError::All => f.write_str(ERR_STORE_ALL),
            StoreError::Bad(n) => write!(
                f,
                "bad store name {}: use a-z, 0-9 and -, 1 to 32 letters, starting with a letter",
                crate::sys::go_quote(n)
            ),
            StoreError::Link(m) => f.write_str(m),
        }
    }
}

/// The text of the answer to --store all.
pub const ERR_STORE_ALL: &str = "one store at a time: pando stores are separate on purpose.";

/// Whether name is a store name (PD-R-1): a letter, then letters, digits
/// and dashes, 32 at most.
pub fn valid_store(name: &str) -> bool {
    let b = name.as_bytes();
    if b.is_empty() || b.len() > 32 || !b[0].is_ascii_lowercase() {
        return false;
    }
    b[1..]
        .iter()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'-')
}

/// The dir of one store under a data home: <data_dir>/pando/<store>.
pub fn store_dir(data_dir: &Path, store: &str) -> Result<PathBuf, StoreError> {
    if store == "all" {
        return Err(StoreError::All);
    }
    if !valid_store(store) {
        return Err(StoreError::Bad(store.to_string()));
    }
    no_link(data_dir, &["pando", store]).map_err(StoreError::Link)?;
    Ok(data_dir.join("pando").join(store))
}

/// Checks each part of base/parts... in turn, below base, and refuses a
/// symbolic link: a link in a store or child path could reach another
/// store. base itself may be a link (a data home moved to another disk). A
/// part that does not exist ends the check.
pub fn no_link(base: &Path, parts: &[&str]) -> Result<(), String> {
    let mut p = base.to_path_buf();
    for part in parts {
        p = p.join(part);
        match std::fs::symlink_metadata(&p) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(e) => return Err(crate::fsutil::path_err("lstat", &p, &e)),
            Ok(m) if m.file_type().is_symlink() => {
                return Err(format!(
                    "pando: {} is a symbolic link; a store path may hold none",
                    p.display()
                ))
            }
            Ok(_) => {}
        }
    }
    Ok(())
}

/// Refuses a store dir whose parts are links: main, tree, the view, the
/// import queue, the children dir and the lock.
pub fn store_links(dir: &Path) -> Result<(), String> {
    for part in ["main", "tree", "view.json", crate::import::IMPORT_FILE, crate::child::CHILDREN_DIR, "lock"] {
        no_link(dir, &[part])?;
    }
    Ok(())
}

/// The parts of a store that a backup holds: the log, the tree, the view,
/// the import queue and the child stores. The lock is never one of them.
pub const BACKUP_PARTS: [&str; 5] = ["main", "tree", "view.json", crate::import::IMPORT_FILE, crate::child::CHILDREN_DIR];

/// Every path a backup holds for these stores, relative to the pando dir:
/// <store>/<part> for each store and part. `daisugi backup` and `coppice
/// backup` both use it, so the two hold the same files.
pub fn backup_rels(stores: &[String]) -> Vec<String> {
    let mut out = Vec::new();
    for s in stores {
        for p in BACKUP_PARTS {
            out.push(format!("{s}/{p}"));
        }
    }
    out
}

/// The stores a backup or a restore covers: each store in pando_dir (a dir
/// with a main dir and a good name), and each good store name that is the
/// first part of an archived path (relative to the pando dir). A restore
/// passes the archive's paths, so a store the target does not have yet is
/// put back; any other path stays outside the list, and the restore refuses
/// it. The list comes sorted.
pub fn backup_stores(pando_dir: &Path, archived: &[String]) -> Vec<String> {
    let mut out: Vec<String> = stores_in(pando_dir).unwrap_or_default();
    for p in archived {
        if let Some((first, rest)) = p.split_once('/') {
            if !rest.is_empty() && valid_store(first) && !out.iter().any(|s| s == first) {
                out.push(first.to_string());
            }
        }
    }
    out.sort();
    out
}

/// Sets how many .broken-<time> copies of one file the store keeps: a tree
/// file's bad lines, a view.json that did not read. 0 keeps all; the
/// default is 5. coppice sets it from its broken_keep setting. A whole
/// history moved aside is never removed, whatever this says.
pub fn set_broken_keep(n: usize) {
    crate::broken::set_keep(n);
}

/// The count set_broken_keep set. 0 means keep all.
pub fn broken_keep() -> usize {
    crate::broken::keep()
}

/// Names the stores under a data home: the dirs of <data_dir>/pando that
/// hold a main dir. They come sorted.
pub fn list_stores(data_dir: &Path) -> Result<Vec<String>, String> {
    stores_in(&data_dir.join("pando"))
}

/// list_stores on the pando dir itself. A link is never a store.
fn stores_in(root: &Path) -> Result<Vec<String>, String> {
    let root = root.to_path_buf();
    let rd = match std::fs::read_dir(&root) {
        Ok(rd) => rd,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(crate::fsutil::path_err("open", &root, &e)),
    };
    let mut out = Vec::new();
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        if !e.file_type().is_ok_and(|t| t.is_dir()) || !valid_store(&name) {
            continue;
        }
        if root.join(&name).join("main").is_dir() {
            out.push(name);
        }
    }
    out.sort();
    Ok(out)
}
