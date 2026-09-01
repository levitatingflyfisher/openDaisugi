//! Python's stack depth, as the oracle would have it.
//!
//! Python raises RecursionError when a call would put more than 1000
//! frames on its stack. The oracle's recursive steps (the shell walk, the
//! symlink resolver, the glob matcher) meet that limit at a depth that
//! depends on how many frames are already on the stack at the call. So
//! each port of an oracle function opens a `frame()` for the length of
//! its call, as Python pushes one, and each step that recurses reads
//! `depth()` to know where the oracle would stop.
//!
//! The count is per thread, as Python's is. A thread the port starts for
//! work the oracle does on its own stack is given the oracle's depth at
//! that point (`at`).
//!
//! DAISUGI_GATE_DEPTH_TRACE names a file that gets one line per recursive
//! step with the depth it saw, so the counts can be checked against the
//! oracle's.

use std::cell::Cell;
use std::io::Write;

thread_local! {
    static DEPTH: Cell<i64> = const { Cell::new(0) };
}

/// One Python frame, popped when dropped.
#[must_use]
pub struct Frame(i64);

impl Drop for Frame {
    fn drop(&mut self) {
        DEPTH.with(|d| d.set(self.0));
    }
}

/// Pushes one frame.
pub fn frame() -> Frame {
    DEPTH.with(|d| {
        let prev = d.get();
        d.set(prev + 1);
        Frame(prev)
    })
}

/// Pushes `n` frames at once (a call through helpers the port folds into
/// one function).
pub fn frames(n: i64) -> Frame {
    DEPTH.with(|d| {
        let prev = d.get();
        d.set(prev + n);
        Frame(prev)
    })
}

/// Sets the depth for the length of the guard: the frames the oracle has
/// on its stack where the port starts a step on a thread of its own.
pub fn at(n: i64) -> Frame {
    DEPTH.with(|d| {
        let prev = d.get();
        d.set(n);
        Frame(prev)
    })
}

/// The frames on the stack now, the current function's included.
pub fn depth() -> i64 {
    DEPTH.with(|d| d.get())
}

/// Writes one depth trace line when DAISUGI_GATE_DEPTH_TRACE is set.
pub fn trace(site: &str) {
    if let Some(p) = std::env::var_os("DAISUGI_GATE_DEPTH_TRACE").filter(|p| !p.is_empty()) {
        if let Ok(mut f) = std::fs::OpenOptions::new().append(true).create(true).open(p) {
            let _ = writeln!(f, "{site} depth {}", depth());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frames_push_and_pop() {
        let _a = at(8);
        {
            let _f = frame();
            assert_eq!(depth(), 9);
            let _g = frames(2);
            assert_eq!(depth(), 11);
        }
        assert_eq!(depth(), 8);
    }
}
