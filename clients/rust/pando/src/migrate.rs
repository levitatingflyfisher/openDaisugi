//! migrate: move a coppice steward dir into a store and rename the old dir
//! aside, never delete (PD-R-6). The twin of Go's pando migrate.go.

use std::path::{Path, PathBuf};
use std::time::SystemTime;

use super::chat::{stamp_text, Chat, ERR_LOCKED};
use super::fsutil;
use super::import::IMPORT_FILE;
use super::store::store_dir;

/// The answer when the old dir is open in another process.
pub const ERR_OPEN_ELSEWHERE: &str = "coppice has this chat open; stop it first";

/// The answer when the new store already holds a log.
pub const ERR_TARGET_EXISTS: &str = "the new store already holds a log; nothing was copied";

/// What the old dir's name gets, then a UTC stamp.
pub const MOVED_INFIX: &str = ".moved-";

/// What migrate did.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Report {
    /// What was copied: main, tree, view.json, import.json.
    pub copied: Vec<String>,
    /// The chair's files that stayed in the moved dir.
    pub kept: Vec<String>,
    /// Where the old dir went.
    pub moved_to: String,
    pub messages: i64,
    /// The dir was moved by an earlier run.
    pub already: bool,
}

/// The newest "<from>.moved-*" beside from, or None.
pub fn moved_sibling(from: &Path) -> Option<PathBuf> {
    let parent = from.parent().unwrap_or(Path::new("."));
    let base = from.file_name()?.to_string_lossy().into_owned() + MOVED_INFIX;
    let mut ms: Vec<PathBuf> = std::fs::read_dir(parent)
        .ok()?
        .flatten()
        .filter(|e| e.file_name().to_string_lossy().starts_with(&base))
        .map(|e| e.path())
        .collect();
    ms.sort();
    ms.pop()
}

fn is_dir(p: &Path) -> bool {
    p.is_dir()
}

/// Names the temp dir a migrate copies into: a dot, the store, this infix,
/// then a UTC stamp. The dot keeps it out of every store list, so nothing
/// opens a copy that is not whole.
pub const MIGRATING_INFIX: &str = ".migrating-";

#[cfg(test)]
thread_local! {
    /// Runs after each file the migrate copies. Tests use it to stop a
    /// migrate part way.
    pub(crate) static MIGRATE_HOOK: std::cell::RefCell<Option<Box<dyn Fn(&str)>>> = std::cell::RefCell::new(None);
}

fn after_copy(_stage: &str) {
    #[cfg(test)]
    MIGRATE_HOOK.with(|h| {
        if let Some(f) = h.borrow().as_ref() {
            f(_stage)
        }
    });
}

/// Moves a coppice steward dir into the store <data_dir>/pando/<store>
/// (PD-R-6). It copies main/, tree/, view.json and import.json into a temp
/// dir beside the store (.<store>.migrating-<stamp>), which it holds the
/// store lock in. It opens the copy and checks that it holds the same
/// messages and the same view. Only then does one rename put the whole copy
/// in place as the store, and the lock it holds is the store's lock from
/// then on. Last it renames from to from + ".moved-" + the UTC stamp of now.
/// It deletes nothing. A failed or cut migrate leaves no store: the copy
/// stays in the temp dir (the error names it) and the old dir stays where
/// it is. The chair's files stay in the renamed dir. A second run, with
/// from gone and a moved sibling beside it, reports that sibling and does
/// nothing.
pub fn migrate(
    from: &Path,
    data_dir: &Path,
    store: &str,
    now: SystemTime,
) -> Result<Report, String> {
    let dst = store_dir(data_dir, store).map_err(|e| e.to_string())?;
    if !is_dir(&from.join("main")) {
        if let Some(m) = moved_sibling(from) {
            return Ok(Report {
                moved_to: m.to_string_lossy().into_owned(),
                already: true,
                ..Default::default()
            });
        }
        return Err(format!("no steward log at {}", from.display()));
    }
    let _lock = match super::chat::lock_dir(&from.join("lock")) {
        Ok(f) => f,
        Err(e) if e == ERR_LOCKED => return Err(ERR_OPEN_ELSEWHERE.into()),
        Err(e) => return Err(e),
    };
    if is_dir(&dst.join("main")) {
        return Err(ERR_TARGET_EXISTS.into());
    }
    let src = Chat::open_read_only(from)?;
    let parent = dst.parent().unwrap_or(Path::new(".")).to_path_buf();
    fsutil::ensure_dir(&parent)?;
    let stamp = stamp_text(now);
    let tmp = PathBuf::from(super::chat::free_name(
        &parent.join(format!(".{store}{MIGRATING_INFIX}{stamp}")).to_string_lossy(),
    ));
    fsutil::ensure_dir(&tmp)?;
    let _tlock = super::chat::lock_dir(&tmp.join("lock"))?;
    let left = |e: String| -> String {
        format!(
            "{e}. No store was made; the partial copy stays in {} and the old dir stays where it is",
            tmp.display()
        )
    };
    let mut rep = Report::default();
    for d in ["main", "tree"] {
        let rd = match std::fs::read_dir(from.join(d)) {
            Ok(rd) => rd,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
            Err(e) => return Err(left(fsutil::path_err("open", &from.join(d), &e))),
        };
        fsutil::ensure_dir(&tmp.join(d)).map_err(left)?;
        let mut names: Vec<_> = rd.flatten().collect();
        names.sort_by_key(|e| e.file_name());
        for e in names {
            if e.path().is_dir() {
                continue;
            }
            copy_file(&e.path(), &tmp.join(d).join(e.file_name())).map_err(left)?;
            after_copy(&format!("{d}/{}", e.file_name().to_string_lossy()));
        }
        rep.copied.push(d.to_string());
    }
    for f in ["view.json", IMPORT_FILE] {
        if std::fs::metadata(from.join(f)).is_err() {
            continue;
        }
        copy_file(&from.join(f), &tmp.join(f)).map_err(left)?;
        rep.copied.push(f.to_string());
    }
    let cp = Chat::open_read_only(&tmp).map_err(|e| left(format!("the copy does not open: {e}")))?;
    if cp.count() != src.count() || cp.view_json() != src.view_json() {
        return Err(left(format!(
            "the copy does not match: {} messages, want {}",
            cp.count(),
            src.count()
        )));
    }
    rep.messages = cp.count();
    // Publish. An empty dir in the way goes (rename does not replace one on
    // every system); a dir that holds anything stays, and the rename fails.
    if let Ok(m) = std::fs::symlink_metadata(&dst) {
        if !m.is_dir() {
            return Err(left(format!("{} is in the way", dst.display())));
        }
        let _ = std::fs::remove_dir(&dst);
        if std::fs::symlink_metadata(&dst).is_ok() {
            return Err(left(format!(
                "the copy could not be put in place: rename {} {}: file exists",
                tmp.display(),
                dst.display()
            )));
        }
    }
    std::fs::rename(&tmp, &dst).map_err(|e| {
        left(format!(
            "the copy could not be put in place: rename {} {}: {}",
            tmp.display(),
            dst.display(),
            crate::sys::go_errno_text(&e)
        ))
    })?;
    fsutil::sync_dir(&parent)?;
    let skip = ["main", "tree", "view.json", IMPORT_FILE, "lock", "import.lock"];
    if let Ok(rd) = std::fs::read_dir(from) {
        for e in rd.flatten() {
            let n = e.file_name().to_string_lossy().into_owned();
            if !skip.contains(&n.as_str()) {
                rep.kept.push(n);
            }
        }
    }
    rep.kept.sort();
    let to = super::chat::free_name(&format!(
        "{}{MOVED_INFIX}{stamp}",
        from.display()
    ));
    std::fs::rename(from, &to).map_err(|e| format!("rename {} {}: {}", from.display(), to, crate::sys::go_errno_text(&e)))?;
    rep.moved_to = to;
    Ok(rep)
}

fn copy_file(from: &Path, to: &Path) -> Result<(), String> {
    let b = std::fs::read(from).map_err(|e| fsutil::path_err("open", from, &e))?;
    fsutil::write_atomic(to, &b)
}

/// Whether name is the name of a moved dir.
pub fn is_moved(name: &str) -> bool {
    name.contains(MOVED_INFIX)
}
