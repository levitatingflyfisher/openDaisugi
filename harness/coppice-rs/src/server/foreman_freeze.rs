//! A yield must stop the whole worker (FM-R-107). `daisugi tree run` starts
//! sprig, and sprig starts the model's tools, each in a process group of its
//! own, so a signal to the group of tree run alone leaves sprig at work. The
//! freeze walks the tree of processes below the worker, collects every group
//! it finds, and stops each one. The twin of Go's foreman_freeze.go.

use std::collections::{BTreeMap, BTreeSet};
use std::time::{Duration, Instant};

/// One process: its parent, its group and its state letter.
#[derive(Clone, Copy)]
pub struct ProcRow {
    pub ppid: i32,
    pub pgrp: i32,
    pub state: u8,
}

/// Reads /proc once. A process that ends while it is read is left out.
pub fn read_procs() -> BTreeMap<i32, ProcRow> {
    let mut out = BTreeMap::new();
    let Ok(rd) = std::fs::read_dir("/proc") else {
        return out;
    };
    for e in rd.flatten() {
        let Some(pid) = e.file_name().to_str().and_then(|n| n.parse::<i32>().ok()) else {
            continue;
        };
        let Ok(s) = std::fs::read_to_string(format!("/proc/{pid}/stat")) else {
            continue;
        };
        let Some(i) = s.rfind(')') else { continue };
        let f: Vec<&str> = s[i + 1..].split_whitespace().collect();
        if f.len() < 3 {
            continue;
        }
        out.insert(
            pid,
            ProcRow {
                ppid: f[1].parse().unwrap_or(0),
                pgrp: f[2].parse().unwrap_or(0),
                state: f[0].as_bytes()[0],
            },
        );
    }
    out
}

/// Root and every process below it, with the groups they are in.
pub fn tree_of(root: i32, procs: &BTreeMap<i32, ProcRow>) -> (Vec<i32>, BTreeSet<i32>) {
    let mut pids = vec![];
    let mut groups = BTreeSet::new();
    if !procs.contains_key(&root) {
        return (pids, groups);
    }
    let mut kids: BTreeMap<i32, Vec<i32>> = BTreeMap::new();
    for (p, r) in procs {
        kids.entry(r.ppid).or_default().push(*p);
    }
    let mut todo = vec![root];
    let mut seen = BTreeSet::from([root]);
    while !todo.is_empty() {
        let p = todo.remove(0);
        pids.push(p);
        groups.insert(procs[&p].pgrp);
        for k in kids.get(&p).into_iter().flatten() {
            if seen.insert(*k) {
                todo.push(*k);
            }
        }
    }
    (pids, groups)
}

/// A group a worker's freeze may signal: never 0 or 1, and never coppice's
/// own group.
fn safe_group(g: i32) -> bool {
    // SAFETY: getpgrp takes no argument.
    g > 1 && g != unsafe { libc::getpgrp() }
}

fn all_stopped(root: i32) -> bool {
    let procs = read_procs();
    let (pids, _) = tree_of(root, &procs);
    !pids.is_empty()
        && pids
            .iter()
            .all(|p| matches!(procs[p].state, b'T' | b't' | b'Z' | b'X'))
}

/// Stops every process group of the worker rooted at root and returns the
/// groups. It returns None, with everything resumed, when a process of the
/// tree did not stop: the caller must not count the room free.
pub fn freeze_tree(root: i32) -> Option<Vec<i32>> {
    if root <= 1 {
        return None;
    }
    let mut stopped = BTreeSet::new();
    let mut order = vec![];
    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        // A stop is a signal, and a stopped process may have forked just
        // before it: look again until the kernel shows the whole tree stopped.
        let (_, groups) = tree_of(root, &read_procs());
        for g in groups {
            if stopped.contains(&g) || !safe_group(g) {
                continue;
            }
            // SAFETY: kill takes plain integers.
            if unsafe { libc::kill(-g, libc::SIGSTOP) } == 0 {
                stopped.insert(g);
                order.push(g);
            }
        }
        if !order.is_empty() && all_stopped(root) {
            return Some(order);
        }
        if Instant::now() > deadline {
            thaw_tree(0, &order);
            return None;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
}

/// Resumes the groups a freeze recorded, and any group the tree has now.
pub fn thaw_tree(root: i32, groups: &[i32]) {
    let mut all: Vec<i32> = groups.to_vec();
    if root > 1 {
        all.extend(tree_of(root, &read_procs()).1);
    }
    let mut seen = BTreeSet::new();
    for g in all {
        if !seen.insert(g) || !safe_group(g) {
            continue;
        }
        // SAFETY: kill takes plain integers.
        unsafe { libc::kill(-g, libc::SIGCONT) };
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader};
    use std::os::unix::process::CommandExt;
    use std::process::{Command, Stdio};

    fn state(pid: i32) -> u8 {
        read_procs().get(&pid).map(|r| r.state).unwrap_or(0)
    }

    fn wait_for(what: &str, mut ok: impl FnMut() -> bool) {
        let end = Instant::now() + Duration::from_secs(5);
        while !ok() {
            assert!(Instant::now() < end, "timed out waiting for {what}");
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    /// FM-R-107: a grandchild in a session of its own (as sprig is) stops
    /// with the worker and goes on with it.
    #[test]
    fn a_freeze_stops_a_grandchild_in_its_own_group_and_a_thaw_goes_on() {
        let mut leader = Command::new("sh")
            .args(["-c", "setsid sleep 30 </dev/null >/dev/null 2>&1 & echo $!; wait"])
            .stdout(Stdio::piped())
            .process_group(0)
            .spawn()
            .unwrap();
        let root = leader.id() as i32;
        let mut line = String::new();
        BufReader::new(leader.stdout.take().unwrap()).read_line(&mut line).unwrap();
        let gc: i32 = line.trim().parse().unwrap();
        // SAFETY: getpgid takes a plain integer.
        wait_for("the grandchild's own group", || unsafe { libc::getpgid(gc) } == gc);
        assert_ne!(gc, root);
        let groups = freeze_tree(root).expect("the tree freezes");
        assert!(groups.contains(&gc) && groups.contains(&root), "{groups:?}");
        assert_eq!(state(gc), b'T', "the grandchild did not stop");
        assert_eq!(state(root), b'T', "the leader did not stop");
        thaw_tree(root, &groups);
        wait_for("the grandchild to go on", || state(gc) != b'T');
        wait_for("the leader to go on", || state(root) != b'T');
        // SAFETY: both pids are the test's own children.
        unsafe {
            libc::kill(gc, libc::SIGKILL);
            libc::kill(-root, libc::SIGKILL);
        }
        let _ = leader.wait();
    }

    #[test]
    fn a_freeze_never_signals_group_0_1_or_its_own() {
        assert!(!safe_group(0) && !safe_group(1));
        // SAFETY: getpgrp takes no argument.
        assert!(!safe_group(unsafe { libc::getpgrp() }));
        assert!(freeze_tree(0).is_none() && freeze_tree(1).is_none());
    }
}
