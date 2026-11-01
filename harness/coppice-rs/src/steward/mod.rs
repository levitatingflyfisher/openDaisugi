//! The steward's memory: one chat that never ends. The twin of Go's
//! internal/steward.
//!
//! Every message is logged word for word, append only, one JSONL file per
//! day. A summary tree compresses the log: a node at level l holds one line
//! of at most 512 bytes for the 2^l messages it covers. The view is the
//! list of tree lines over the whole chat, fine near now and coarse in the
//! past, kept between 64 and 128 KiB. It is saved to view.json after each
//! change. A view.json that cannot be used is rebuilt by replay over the tree.
//!
//! The shape follows Victor Taelin's UniiChat recipe. The code is our own.
//! This module is a library: it opens no socket and starts no server.

pub mod calls;
pub mod cli;
pub mod failover;
pub mod migrate;
pub mod pool;
pub mod tools;
pub mod turns;

// The store, the compactor and the importer live in the pando crate.
pub use pando::{chat, compactor, ctx, fsutil, import, order};

#[cfg(test)]
mod chair_tests;
#[cfg(test)]
mod failover_tests;

use std::path::{Path, PathBuf};

#[allow(unused_imports)]
pub use chat::{Chat, Message, Summarizer};
#[allow(unused_imports)]
pub use order::NodeId;

/// The steward's log directory name on disk. It keeps the name it had before
/// the rename. ADR-0023 moves this store to `<data home>/pando/<store>/` with
/// `daisugi pando migrate`; one move, not two. The backup areas carry the same
/// name, so an old backup restores.
macro_rules! steward_dir {
    () => {
        "foreman" // names-ok: the on-disk name until ADR-0023 moves the store
    };
}
pub(crate) use steward_dir;
pub const DIR_NAME: &str = steward_dir!();

/// Where the steward keeps its chat when no --data-dir is given: the data
/// home's steward dir.
pub fn default_dir() -> PathBuf {
    crate::datahome::default().join(DIR_NAME)
}

/// The steward's log dir for a coppice data dir. The default data dir (the
/// data home's coppice) uses default_dir, the steward dir beside it; any
/// other data dir X uses X/DIR_NAME. The coppice server writes the log there
/// and coppice steward reads it there.
pub fn log_dir(coppice_data_dir: &Path) -> PathBuf {
    let clean = crate::sys::clean(&coppice_data_dir.to_string_lossy());
    let def = crate::datahome::default().join("coppice");
    if clean == crate::sys::clean(&def.to_string_lossy()) {
        return default_dir();
    }
    coppice_data_dir.join(DIR_NAME)
}

/// The data home that holds pando/<store> for a coppice data dir. The
/// default data dir uses the data home itself, so coppice and `daisugi
/// pando` find the same store. Any other data dir X keeps its stores in
/// X/pando, so a run with its own --data-dir never touches the real store.
pub fn store_root(coppice_data_dir: &Path) -> PathBuf {
    let clean = crate::sys::clean(&coppice_data_dir.to_string_lossy());
    let def = crate::datahome::default();
    if clean == crate::sys::clean(&def.join("coppice").to_string_lossy()) {
        return def;
    }
    coppice_data_dir.to_path_buf()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_default_dir_follows_the_data_home() {
        let home = Some(PathBuf::from("/h"));
        let env = |pairs: &'static [(&'static str, &'static str)]| {
            move |k: &str| {
                pairs
                    .iter()
                    .find(|(n, _)| *n == k)
                    .map(|(_, v)| std::ffi::OsString::from(v))
            }
        };
        let has = |held: &'static str| move |p: &Path| p == Path::new(held);
        let cases: [(&[(&str, &str)], &'static str, &str); 5] = [
            (
                &[("OPENDAISUGI_HOME", "/o"), ("XDG_DATA_HOME", "/x")],
                "",
                "/o/foreman", // names-ok: the log dir name on disk
            ),
            (&[("XDG_DATA_HOME", "/x")], "", "/x/opendaisugi/foreman"), // names-ok: the log dir name on disk
            (&[("XDG_DATA_HOME", "/x")], "/h/.opendaisugi/warden", "/x/opendaisugi/foreman"), // names-ok: the log dir name on disk
            (&[("XDG_DATA_HOME", "/x")], "/h/.opendaisugi/coppice", "/h/.opendaisugi/foreman"), // names-ok: the log dir name on disk
            (&[], "", "/h/.opendaisugi/foreman"), // names-ok: the log dir name on disk
        ];
        for (pairs, held, want) in cases {
            let got = crate::datahome::dir(env(pairs), home.clone(), has(held)).join(DIR_NAME);
            assert_eq!(got, PathBuf::from(want));
        }
    }

    #[test]
    fn a_data_dir_keeps_its_log_inside_it() {
        assert_eq!(
            log_dir(Path::new("/x/data")),
            PathBuf::from("/x/data/foreman") // names-ok: the log dir name on disk
        );
        let def = crate::datahome::default().join("coppice");
        assert_eq!(log_dir(&def), default_dir());
    }
}
