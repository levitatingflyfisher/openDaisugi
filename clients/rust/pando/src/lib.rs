//! Pando R2, the conversation memory: one chat that never ends. The twin of
//! Go's clients/go/pando.
//!
//! Every message is logged word for word, append only, one JSONL file per
//! day. A summary tree compresses the log: a node at level l holds one line
//! of at most 512 bytes for the 2^l messages it covers. The view is the
//! list of tree lines over the whole chat, fine near now and coarse in the
//! past, kept between 64 and 128 KiB. It is saved to view.json after each
//! change. A view.json that cannot be used is rebuilt by replay over the tree.
//!
//! The shape follows Victor Taelin's UniiChat recipe. The code is our own.
//! This crate is a library: it opens no socket and starts no server.

pub mod blake2b;
pub mod branch;
pub mod broken;
pub mod chat;
pub mod compactor;
pub mod ctx;
pub mod fsutil;
pub mod child;
pub mod cli;
pub mod import;
pub mod import_web;
pub mod lexical;
pub mod live;
pub mod mcp;
pub mod migrate;
pub mod order;
pub mod recall;
pub mod sha256;
pub mod store;
pub mod sys;
pub mod tend;

#[cfg(test)]
mod chat_tests;
#[cfg(test)]
mod golden_tests;
#[cfg(test)]
mod import_tests;
#[cfg(test)]
mod live_tests;
#[cfg(test)]
mod branch_tests;
#[cfg(test)]
mod pando_tests;
#[cfg(test)]
mod fix_r1_tests;
#[cfg(test)]
mod maxcalls_tests;
#[cfg(test)]
mod paid_tend_tests;
#[cfg(test)]
mod redo_tests;
#[cfg(test)]
mod skiplist_tests;

#[cfg(test)]
pub(crate) mod testhome {
    /// The chat tests write under the temp dir only; nothing to guard.
    pub fn guard(p: std::path::PathBuf) -> std::path::PathBuf {
        p
    }
}

pub use chat::{Chat, Message, Summarizer};
pub use order::NodeId;

/// The UTC civil date and time of Unix seconds: (year, month, day, hour,
/// minute, second).
pub fn civil(secs: i64) -> (i64, u32, u32, u32, u32, u32) {
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    // Howard Hinnant's days-to-civil.
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let y = if m <= 2 { y + 1 } else { y };
    (
        y,
        m,
        d,
        (rem / 3600) as u32,
        (rem % 3600 / 60) as u32,
        (rem % 60) as u32,
    )
}
