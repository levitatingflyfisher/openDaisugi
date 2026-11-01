//! After `daisugi pando migrate`: the old log dir, and the chair's files in
//! the dir the migrate renamed. The twin of Go's internal/steward migrate.go.

use std::path::{Path, PathBuf};

use pando::import::IMPORT_FILE;
use pando::migrate::moved_sibling;

/// The file in the chair's dir that says the chair files of a moved dir
/// were copied in.
pub const ADOPTED_MARK: &str = "adopted-from-moved";

/// The old log dir (log_dir) when it still holds a chat that `daisugi
/// pando migrate` has not moved, and the new store holds none. Else None.
pub fn old_store(log_dir: &Path, store_dir: &Path) -> Option<PathBuf> {
    if !log_dir.join("main").is_dir() {
        return None;
    }
    if crate::sys::clean(&log_dir.to_string_lossy()) == crate::sys::clean(&store_dir.to_string_lossy()) {
        return None;
    }
    if store_dir.join("main").is_dir() {
        return None;
    }
    Some(log_dir.to_path_buf())
}

/// Copies the chair's files out of the dir `daisugi pando migrate` renamed
/// (log_dir + ".moved-<stamp>") into log_dir, once: turns, calls, the
/// failover state, the queue and the workers' logs. It copies and never
/// deletes. It returns one line to show, or "" when it did nothing.
pub fn adopt_chair_files(log_dir: &Path) -> String {
    if std::fs::metadata(log_dir.join(ADOPTED_MARK)).is_ok() {
        return String::new();
    }
    let Some(moved) = moved_sibling(log_dir) else {
        return String::new();
    };
    let Ok(rd) = std::fs::read_dir(&moved) else {
        return String::new();
    };
    if std::fs::create_dir_all(log_dir).is_err() {
        return String::new();
    }
    let _ = std::fs::set_permissions(
        log_dir,
        std::os::unix::fs::PermissionsExt::from_mode(0o700),
    );
    let skip = [
        "main", "tree", "view.json", IMPORT_FILE, "lock", "import.lock", "turns", "tools.sock",
    ];
    let mut copied = Vec::new();
    let mut names: Vec<_> = rd.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect();
    names.sort();
    for n in names {
        if skip.contains(&n.as_str()) || n.ends_with(".sock") {
            continue;
        }
        if std::fs::symlink_metadata(log_dir.join(&n)).is_ok() {
            continue;
        }
        if copy_tree(&moved.join(&n), &log_dir.join(&n)).is_ok() {
            copied.push(n);
        }
    }
    copied.sort();
    let _ = std::fs::write(
        log_dir.join(ADOPTED_MARK),
        format!("{}\n", moved.display()),
    );
    let _ = std::fs::set_permissions(
        log_dir.join(ADOPTED_MARK),
        std::os::unix::fs::PermissionsExt::from_mode(0o600),
    );
    if copied.is_empty() {
        return String::new();
    }
    format!(
        "Copied the steward's own files ({}) from {}. Nothing there was deleted.",
        copied.join(", "),
        moved.display()
    )
}

fn copy_tree(from: &Path, to: &Path) -> std::io::Result<()> {
    let md = std::fs::symlink_metadata(from)?;
    if md.is_dir() {
        std::fs::create_dir_all(to)?;
        std::fs::set_permissions(to, std::os::unix::fs::PermissionsExt::from_mode(0o700))?;
        for e in std::fs::read_dir(from)? {
            let e = e?;
            copy_tree(&e.path(), &to.join(e.file_name()))?;
        }
        Ok(())
    } else if md.is_file() {
        std::fs::write(to, std::fs::read(from)?)?;
        std::fs::set_permissions(to, std::os::unix::fs::PermissionsExt::from_mode(0o600))
    } else {
        Ok(())
    }
}
