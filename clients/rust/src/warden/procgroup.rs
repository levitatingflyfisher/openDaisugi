//! A child with a deadline can start processes of its own: git runs an
//! alias or a helper, a verifier client runs a shell. A kill of the
//! child's pid alone leaves those running, and they can hold the child's
//! pipes open or hang for ever (a git call blocked on a fifo in the repo's
//! config did, for a day). So each such child starts in a process group of
//! its own, and the whole group is killed.

use std::os::unix::process::CommandExt;
use std::process::{Child, Command};

/// Makes `cmd` start in a new process group and die (SIGKILL from the
/// kernel) when the thread that started it ends. That covers the case
/// where this process is itself killed while it waits: nothing is left to
/// kill the group then. Every caller waits for its child before the
/// starting thread can end.
pub fn in_group(cmd: &mut Command) {
    let parent = std::process::id() as libc::pid_t;
    cmd.process_group(0);
    // SAFETY: only async-signal-safe calls (prctl, getppid) run between
    // fork and exec.
    unsafe {
        cmd.pre_exec(move || {
            if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL as libc::c_ulong, 0, 0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            // The parent can end before the prctl above takes effect.
            if libc::getppid() != parent {
                return Err(std::io::Error::from_raw_os_error(libc::ESRCH));
            }
            Ok(())
        });
    }
}

/// SIGKILLs `child`'s process group and `child`, then reaps `child`. The
/// group is killed before the reap, so its id (the child's pid) cannot yet
/// name another process. A pid of 1 or less is refused, since kill(-1) and
/// kill(0) reach far more than one group.
pub fn kill_group(child: &mut Child) {
    signal_group(child);
    let _ = child.wait();
}

/// `kill_group` without the reap: SIGKILL to `child`'s group and `child`.
pub fn signal_group(child: &mut Child) {
    let pid = child.id() as libc::pid_t;
    if pid > 1 {
        unsafe { libc::kill(-pid, libc::SIGKILL) };
    }
    let _ = child.kill();
}

/// Helpers for tests that check no process outlives a call.
#[cfg(test)]
pub mod testing {
    use std::time::{Duration, SystemTime, UNIX_EPOCH};

    /// A sleep duration no other process on the box uses.
    pub fn marker() -> String {
        let n = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().subsec_nanos();
        format!("97.{n:09}")
    }

    /// Every live (not zombie) process whose argv contains `s`.
    pub fn live_pids(s: &str) -> Vec<i32> {
        let me = std::process::id() as i32;
        let mut out = Vec::new();
        for e in std::fs::read_dir("/proc").into_iter().flatten().flatten() {
            let pid: i32 = match e.file_name().to_str().and_then(|n| n.parse().ok()) {
                Some(p) => p,
                None => continue,
            };
            if pid == me {
                continue;
            }
            let cmd = std::fs::read(e.path().join("cmdline")).unwrap_or_default();
            if !cmd.windows(s.len()).any(|w| w == s.as_bytes()) {
                continue;
            }
            let st = std::fs::read_to_string(e.path().join("stat")).unwrap_or_default();
            match st.rfind(')') {
                Some(i) if !st[i..].starts_with(") Z") => out.push(pid),
                _ => {}
            }
        }
        out
    }

    /// Kills every process whose argv contains `s`.
    pub fn kill_all(s: &str) {
        for p in live_pids(s) {
            unsafe { libc::kill(p, libc::SIGKILL) };
        }
    }

    /// Waits up to two seconds (a parent-death signal is sent
    /// asynchronously) for every process naming `s` to end. Survivors are
    /// killed, and the list of them returned.
    pub fn left_after(s: &str) -> Vec<i32> {
        for _ in 0..100 {
            if live_pids(s).is_empty() {
                return vec![];
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        let left = live_pids(s);
        kill_all(s);
        left
    }
}
