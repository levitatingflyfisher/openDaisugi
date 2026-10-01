//! The foreman's memory: one chat that never ends. The twin of Go's
//! internal/foreman.
//!
//! Every message is logged word for word, append only, one JSONL file per
//! day. A summary tree compresses the log: a node at level l holds one line
//! of at most 512 bytes for the 2^l messages it covers. The view is the
//! list of tree lines over the whole chat, fine near now and coarse in the
//! past, kept between 64 and 128 KiB. It is saved to view.json after each
//! change and never rebuilt from the log.
//!
//! The shape follows Victor Taelin's UniiChat recipe. The code is our own.
//! This module is a library: it opens no socket and starts no server.

pub mod chat;
pub mod cli;
pub mod compactor;
pub mod ctx;
pub mod fsutil;
pub mod order;
pub mod tools;
pub mod turns;

#[cfg(test)]
mod chat_tests;
#[cfg(test)]
mod golden_tests;

use std::path::{Path, PathBuf};

#[allow(unused_imports)]
pub use chat::{Chat, Message, Summarizer};
#[allow(unused_imports)]
pub use order::NodeId;

/// Where the foreman keeps its chat when no --data-dir is given: the data
/// home's foreman dir.
pub fn default_dir() -> PathBuf {
    crate::datahome::default().join("foreman")
}

/// The foreman's log dir for a coppice data dir. The default data dir (the
/// data home's coppice) uses default_dir, the foreman dir beside it; any
/// other data dir X uses X/foreman. The coppice server writes the log there
/// and coppice foreman reads it there.
pub fn log_dir(coppice_data_dir: &Path) -> PathBuf {
    let clean = crate::sys::clean(&coppice_data_dir.to_string_lossy());
    let def = crate::datahome::default().join("coppice");
    if clean == crate::sys::clean(&def.to_string_lossy()) {
        return default_dir();
    }
    coppice_data_dir.join("foreman")
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
        let dot = |exists: bool| move |p: &Path| exists && p == Path::new("/h/.opendaisugi");
        let cases: [(&[(&str, &str)], bool, &str); 4] = [
            (
                &[("OPENDAISUGI_HOME", "/o"), ("XDG_DATA_HOME", "/x")],
                false,
                "/o/foreman",
            ),
            (&[("XDG_DATA_HOME", "/x")], false, "/x/opendaisugi/foreman"),
            (&[("XDG_DATA_HOME", "/x")], true, "/h/.opendaisugi/foreman"),
            (&[], false, "/h/.opendaisugi/foreman"),
        ];
        for (pairs, exists, want) in cases {
            let got = crate::datahome::dir(env(pairs), home.clone(), dot(exists)).join("foreman");
            assert_eq!(got, PathBuf::from(want));
        }
    }

    #[test]
    fn a_data_dir_keeps_its_log_inside_it() {
        assert_eq!(
            log_dir(Path::new("/x/data")),
            PathBuf::from("/x/data/foreman")
        );
        let def = crate::datahome::default().join("coppice");
        assert_eq!(log_dir(&def), default_dir());
    }
}
