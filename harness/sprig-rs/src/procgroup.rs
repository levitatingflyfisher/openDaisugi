//! The process groups of sprig's bash and gate processes (the Go
//! module's procgroup.go). Each runs in a group of its own with SIGKILL as
//! its parent-death signal; every group that may still have members is
//! killed when sprig ends, however it ends, so nothing a tool left behind
//! outlives sprig. The table is fixed atomics, so the signal handler can
//! read it. Each group also notes the run that started it, so a run kills
//! only its own groups when it ends (tests run several at once).

use std::cell::Cell;
use std::sync::atomic::{AtomicI32, AtomicU64, Ordering};

const SLOTS: usize = 256;
static GROUPS: [AtomicI32; SLOTS] = [const { AtomicI32::new(0) }; SLOTS];
static OWNERS: [AtomicU64; SLOTS] = [const { AtomicU64::new(0) }; SLOTS];
static NEXT_RUN: AtomicU64 = AtomicU64::new(1);

thread_local! {
    static RUN: Cell<u64> = const { Cell::new(0) };
}

/// Starts a run on this thread: the groups its tools start are its own.
pub fn begin() {
    RUN.with(|r| r.set(NEXT_RUN.fetch_add(1, Ordering::SeqCst)));
}

extern "C" {
    fn kill(pid: i32, sig: i32) -> i32;
    fn _exit(code: i32) -> !;
    fn signal(signum: i32, handler: usize) -> usize;
    /// prctl(2); only PR_SET_PDEATHSIG is used.
    pub fn prctl(option: i32, arg2: u64, arg3: u64, arg4: u64, arg5: u64) -> i32;
}

/// PR_SET_PDEATHSIG.
pub const PR_SET_PDEATHSIG: i32 = 1;
/// SIGKILL.
pub const SIGKILL: i32 = 9;
/// SIGTERM.
pub const SIGTERM: i32 = 15;

/// Records a tool's group. A full table records nothing more.
pub fn add(pgid: i32) {
    let run = RUN.with(Cell::get);
    for (i, s) in GROUPS.iter().enumerate() {
        if s.compare_exchange(0, pgid, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
        {
            OWNERS[i].store(run, Ordering::SeqCst);
            return;
        }
    }
}

fn free(i: usize) -> i32 {
    OWNERS[i].store(0, Ordering::SeqCst);
    GROUPS[i].swap(0, Ordering::SeqCst)
}

/// Forgets a group with no member left.
pub fn forget_if_empty(pgid: i32) -> bool {
    if unsafe { kill(-pgid, 0) } == 0 {
        return false;
    }
    for (i, s) in GROUPS.iter().enumerate() {
        if s.load(Ordering::SeqCst) == pgid {
            free(i);
        }
    }
    true
}

/// Kills a whole group.
pub fn kill_group(pgid: i32, sig: i32) {
    if pgid > 1 {
        unsafe {
            kill(-pgid, sig);
        }
    }
}

/// Kills one process.
pub fn kill_pid(pid: i32) {
    if pid > 1 {
        unsafe {
            kill(pid, SIGKILL);
        }
    }
}

/// Kills every group this thread's run started and forgets them (Go's
/// KillToolGroups).
pub fn kill_own() {
    let run = RUN.with(Cell::get);
    for (i, s) in GROUPS.iter().enumerate() {
        if s.load(Ordering::SeqCst) != 0 && OWNERS[i].load(Ordering::SeqCst) == run {
            kill_group(free(i), SIGKILL);
        }
    }
}

/// Kills every recorded group (the signal handler's job).
fn kill_all() {
    for s in &GROUPS {
        let g = s.load(Ordering::SeqCst);
        if g > 1 {
            unsafe {
                kill(-g, SIGKILL);
            }
        }
    }
}

extern "C" fn on_signal(sig: i32) {
    kill_all();
    unsafe { _exit(128 + sig) }
}

/// SIGTERM, SIGINT and SIGHUP kill the tool groups before sprig exits with
/// 128 and the signal's number.
pub fn on_signal_kill_all() {
    unsafe {
        for sig in [SIGTERM, 2, 1] {
            signal(sig, on_signal as extern "C" fn(i32) as usize);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_bash_call_has_a_group_of_its_own_and_dies_with_sprig() {
        use crate::gate::Tool;
        let dir = std::env::temp_dir().join(format!("pg-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let pid_file = dir.join("pid");
        let cmd = format!(
            "sleep 300 </dev/null >/dev/null 2>&1 & echo $! > {}; ps -o pgid= -p $$; echo $$",
            pid_file.display()
        );
        begin();
        let mut input = crate::json::Map::new();
        input.insert("cmd".into(), crate::json::Any::Str(cmd));
        let (out, err) = crate::tools::BashTool.run(&Some(input));
        assert!(err.is_none(), "{:?}", err);
        let text = String::from_utf8(out).unwrap();
        let f: Vec<&str> = text.split_whitespace().collect();
        assert_eq!(f.len(), 2);
        assert_eq!(f[0], f[1]);
        let pid: i32 = std::fs::read_to_string(&pid_file)
            .unwrap()
            .trim()
            .parse()
            .unwrap();
        kill_own();
        let stat = format!("/proc/{pid}/stat");
        let end = std::time::Instant::now() + std::time::Duration::from_secs(5);
        let alive = || {
            std::fs::read_to_string(&stat)
                .map(|s| {
                    s.rsplit_once(')')
                        .map(|(_, r)| !r.trim_start().starts_with('Z'))
                })
                .ok()
                .flatten()
                .unwrap_or(false)
        };
        while alive() && std::time::Instant::now() < end {
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        let still = alive();
        if still {
            unsafe {
                kill(pid, SIGKILL);
            }
        }
        let _ = std::fs::remove_dir_all(&dir);
        assert!(!still, "the background process outlived sprig");
    }
}
