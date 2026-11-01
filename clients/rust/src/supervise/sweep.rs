//! `tree_branch._Sweep`: stop every process a branch child leaves behind,
//! before the parent reads a worktree. The child's own process group is
//! killed when it exits, but a process that made a new session is not in that
//! group. So the tree run makes itself the child subreaper while the children
//! run: every process they leave, at any depth, becomes a child of this
//! process when its parent ends. `clear` stops each child this process did
//! not have before, waits for it, and goes on until none is left. Linux only,
//! as the fence is.

use std::collections::HashSet;
use std::time::{Duration, Instant};

/// How long the sweep waits after SIGTERM before SIGKILL.
pub const KILL_GRACE_S: Duration = Duration::from_secs(2);
/// How long the sweep may take after that to stop what the children left.
pub const SWEEP_S: Duration = Duration::from_secs(5);

/// The refusal when a process a child left will not stop.
pub const SWEEP_TEXT: &str = "a process a branch child left could not be stopped; nothing of the branches was read";

/// This process's children now, as (pid, start time) pairs, so a pid used
/// again is not taken for one seen before.
fn children() -> HashSet<(i32, String)> {
    let me = std::process::id().to_string();
    let mut out = HashSet::new();
    let Ok(rd) = std::fs::read_dir("/proc") else { return out };
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().to_string();
        let Ok(pid) = name.parse::<i32>() else { continue };
        let Ok(data) = std::fs::read(format!("/proc/{name}/stat")) else { continue };
        let Some(i) = data.iter().rposition(|&b| b == b')') else { continue };
        if i + 2 > data.len() {
            continue;
        }
        let rest: Vec<&[u8]> = data[i + 2..].split(|b| b.is_ascii_whitespace()).filter(|f| !f.is_empty()).collect();
        if rest.len() > 19 && rest[1] == me.as_bytes() {
            out.insert((pid, String::from_utf8_lossy(rest[19]).to_string()));
        }
    }
    out
}

pub struct Sweep {
    on: bool,
    was: libc::c_int,
    before: HashSet<(i32, String)>,
}

impl Sweep {
    pub fn new() -> Sweep {
        let mut s = Sweep { on: false, was: 0, before: HashSet::new() };
        #[cfg(target_os = "linux")]
        unsafe {
            let mut was: libc::c_int = 0;
            libc::prctl(libc::PR_GET_CHILD_SUBREAPER, &mut was as *mut libc::c_int as libc::c_ulong, 0, 0, 0);
            if libc::prctl(libc::PR_SET_CHILD_SUBREAPER, 1 as libc::c_ulong, 0, 0, 0) != 0 {
                return s;
            }
            s.on = true;
            s.was = was;
            s.before = children();
        }
        s
    }

    /// Stop and reap what the children left: SIGTERM first, so a server (a
    /// resident warden the child's fence started) can shut down cleanly,
    /// then SIGKILL after KILL_GRACE_S. False when something is still there
    /// after that and SWEEP_S more.
    pub fn clear(&self) -> bool {
        if !self.on {
            return true;
        }
        let term_until = Instant::now() + KILL_GRACE_S;
        let end = term_until + SWEEP_S;
        let mut termed: HashSet<i32> = HashSet::new();
        let ok = loop {
            let left: Vec<i32> = children().difference(&self.before).map(|(p, _)| *p).collect();
            if left.is_empty() {
                break true;
            }
            let now = Instant::now();
            if now > end {
                break false;
            }
            for &pid in &left {
                if now < term_until {
                    if termed.insert(pid) {
                        unsafe {
                            libc::kill(pid, libc::SIGTERM);
                        }
                    }
                } else {
                    unsafe {
                        libc::kill(pid, libc::SIGKILL);
                    }
                }
            }
            for &pid in &left {
                let mut st: libc::c_int = 0;
                unsafe {
                    libc::waitpid(pid, &mut st, libc::WNOHANG);
                }
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        #[cfg(target_os = "linux")]
        unsafe {
            libc::prctl(libc::PR_SET_CHILD_SUBREAPER, self.was as libc::c_ulong, 0, 0, 0);
        }
        ok
    }
}
