//! The sprig child of `agentic_executor`: its tool names, the texts of a
//! failed run (the oracle's own), and the run itself, in its own process
//! group with the parent-death signal and descriptors at fixed numbers.
//! The Go `supervise.runGroupFds` is the reference.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::time::{Duration, Instant};

use crate::gate::py::text::{decode_utf8_replace, head, strip};

/// The host tools sprig has a tool for, and its name for each. Any other
/// requested tool is left out of sprig's wall.
pub fn sprig_tool(host: &str) -> Option<&'static str> {
    Some(match host {
        "Read" => "read",
        "Write" => "write",
        "Edit" => "edit",
        "Bash" => "bash",
        "Spawn" => "spawn",
        _ => return None,
    })
}

/// The failed step when a word of sprig's gate command would hold
/// whitespace: sprig splits `--gate-cmd` at whitespace.
pub const SPRIG_SPACE_TEXT: &str = "the sprig gate command has a word with a space in it, and sprig \
splits that command at spaces. Use a TMPDIR path and a daisugi path with no spaces.";

/// `_sprig_exit_text`: the exit code (or the signal) and the last line
/// sprig wrote to stderr.
pub fn exit_text(rc: i32, stderr: &[u8]) -> String {
    if rc < 0 {
        return format!("agentic sub-agent failed: sprig was killed by signal {}", -rc);
    }
    let text = format!("agentic sub-agent failed: sprig exited with code {rc}");
    let decoded = decode_utf8_replace(stderr);
    match decoded.split('\n').map(strip).filter(|s| !s.is_empty()).last() {
        Some(last) => format!("{text}: {}", head(last, 500)),
        None => text,
    }
}

/// `SPRIG_DRAIN_S`: how long the output is read after the timeout kill.
pub const SPRIG_DRAIN_S: u64 = 2;

/// What `run_group` saw.
pub struct Ran {
    pub rc: i32,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub timed_out: bool,
    /// The child wrote more than OUTPUT_CAP on stdout or stderr.
    pub too_big: bool,
}

/// `OUTPUT_CAP`: the most a child may write on stdout, and on stderr.
pub const OUTPUT_CAP: usize = 16 << 20;
/// `KILL_GRACE_S`.
const KILL_GRACE: Duration = Duration::from_secs(2);

/// `agentic_executor.kill_groups`: SIGTERM to each group in order, so a
/// sprig can kill its tools' groups, then SIGKILL to each one still there
/// after KILL_GRACE. Never a group of 1 or less, and never this process's
/// own group. `reap` is the child to wait on, so its exit is seen.
pub fn kill_groups(groups: &[i32], mut reap: Option<&mut std::process::Child>) {
    let mine = unsafe { libc::getpgrp() };
    let mut targets: Vec<i32> = vec![];
    for g in groups {
        if *g > 1 && *g != mine && !targets.contains(g) {
            targets.push(*g);
        }
    }
    let signal_all = |targets: &[i32], sig: i32| -> Vec<i32> {
        targets
            .iter()
            .copied()
            .filter(|g| unsafe { libc::kill(-g, sig) } == 0)
            .collect()
    };
    let mut alive = signal_all(&targets, libc::SIGTERM);
    let end = Instant::now() + KILL_GRACE;
    while !alive.is_empty() && Instant::now() < end {
        if let Some(c) = reap.as_mut() {
            let _ = c.try_wait();
        }
        alive.retain(|g| unsafe { libc::kill(-g, 0) } == 0);
        if !alive.is_empty() {
            std::thread::sleep(Duration::from_millis(20));
        }
    }
    signal_all(&alive, libc::SIGKILL);
}

/// `agentic_executor._run_sprig`: bin in a process group of its own, with
/// each of `fds` at its number (a pipe that holds its bytes) and the
/// parent-death signal (SIGTERM, so a sprig can kill its tools' groups).
/// `on_start` gets its pid, which is its group. The kill (MX-R-8): when
/// the child exits, writes more than OUTPUT_CAP or runs past the timeout,
/// `branch()`'s groups and then its own are killed (kill_groups). Output
/// a process outside the group still holds is read until the timeout,
/// and after a kill for SPRIG_DRAIN_S more. Err when it cannot start.
#[allow(clippy::too_many_arguments)]
pub fn run_group(
    bin: &str,
    args: &[String],
    dir: &str,
    env: &HashMap<String, String>,
    stdin: Vec<u8>,
    timeout_s: u64,
    fds: &[(i32, Vec<u8>)],
    on_start: Option<&dyn Fn(i32)>,
    branch: Option<&dyn Fn() -> Vec<i32>>,
) -> std::io::Result<Ran> {
    use std::os::fd::AsRawFd;
    use std::os::unix::process::{CommandExt, ExitStatusExt};
    use std::process::{Command, Stdio};
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;
    let mut cmd = Command::new(bin);
    cmd.args(args)
        .current_dir(dir)
        .env_clear()
        .envs(env)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
    // Each read end goes above 10, close on exec, so a pipe can never sit
    // at a number another descriptor is moved to; the child gets it only
    // at its own number.
    let mut moves: Vec<(i32, i32)> = vec![];
    let mut writers = vec![];
    for (fd, data) in fds {
        let (r, mut w) = std::io::pipe()?;
        let high = unsafe { libc::fcntl(r.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 10) };
        drop(r);
        if high < 0 {
            return Err(std::io::Error::last_os_error());
        }
        moves.push((high, *fd));
        let data = data.clone();
        writers.push(std::thread::spawn(move || {
            let _ = w.write_all(&data);
        }));
    }
    let parent = std::process::id() as i32;
    let placed = moves.clone();
    unsafe {
        cmd.pre_exec(move || {
            if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM, 0, 0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::getppid() != parent {
                return Err(std::io::Error::from_raw_os_error(libc::ESRCH));
            }
            for (high, fd) in &placed {
                if libc::dup2(*high, *fd) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
            }
            Ok(())
        });
    }
    let spawned = cmd.spawn();
    for (high, _) in &moves {
        unsafe {
            libc::close(*high);
        }
    }
    drop(writers);
    let mut child = spawned?;
    let pgid = child.id() as i32;
    if let Some(f) = on_start {
        f(pgid);
    }
    let mut sin = child.stdin.take();
    let feeder = std::thread::spawn(move || {
        if let Some(s) = sin.as_mut() {
            let _ = s.write_all(&stdin);
        }
    });
    let big = Arc::new(AtomicBool::new(false));
    let reader = |p: Option<Box<dyn Read + Send>>| {
        let big = big.clone();
        std::thread::spawn(move || {
            let mut b = Vec::new();
            if let Some(mut p) = p {
                let mut chunk = [0u8; 65536];
                loop {
                    match p.read(&mut chunk) {
                        Ok(0) => break,
                        Ok(n) if b.len() + n > OUTPUT_CAP => {
                            big.store(true, Ordering::SeqCst);
                            break;
                        }
                        Ok(n) => b.extend_from_slice(&chunk[..n]),
                        Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
                        Err(_) => break,
                    }
                }
            }
            b
        })
    };
    let out = reader(child.stdout.take().map(|p| Box::new(p) as Box<dyn Read + Send>));
    let err = reader(child.stderr.take().map(|p| Box::new(p) as Box<dyn Read + Send>));
    let limit = Duration::from_secs(timeout_s);
    let t0 = Instant::now();
    let mut timed_out = false;
    let mut killed_on_exit = false;
    let mut drain: Option<Instant> = None;
    let mut status = None;
    let kill = |child: &mut std::process::Child| {
        let mut groups = branch.map(|f| f()).unwrap_or_default();
        groups.push(pgid);
        kill_groups(&groups, Some(child));
    };
    loop {
        if status.is_none() {
            if let Ok(Some(s)) = child.try_wait() {
                status = Some(s);
            }
        }
        let ended = out.is_finished() && err.is_finished();
        if let Some(d) = drain {
            if ended || d.elapsed() >= Duration::from_secs(SPRIG_DRAIN_S) {
                break;
            }
        } else if big.load(Ordering::SeqCst) || t0.elapsed() >= limit {
            timed_out = !big.load(Ordering::SeqCst);
            kill(&mut child);
            drain = Some(Instant::now());
            continue;
        } else if status.is_some() && !killed_on_exit {
            killed_on_exit = true;
            kill(&mut child);
        } else if ended && status.is_some() {
            break;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
    let status = match status {
        Some(s) => Some(s),
        None => child.wait().ok(),
    };
    let too_big = big.load(Ordering::SeqCst);
    if !(out.is_finished() && err.is_finished()) {
        // A process that left the group still holds a pipe: the reader
        // threads are left to end when it does.
        return Ok(Ran {
            rc: -1,
            stdout: Vec::new(),
            stderr: Vec::new(),
            timed_out,
            too_big,
        });
    }
    let _ = feeder.join();
    let stdout = out.join().unwrap_or_default();
    let stderr = err.join().unwrap_or_default();
    let rc = match status {
        Some(s) => match (s.code(), s.signal()) {
            (Some(c), _) => c,
            (None, Some(sig)) => -sig,
            _ => -1,
        },
        None => -1,
    };
    Ok(Ran {
        rc,
        stdout,
        stderr,
        timed_out,
        too_big,
    })
}

#[cfg(test)]
mod kill_tests {
    use super::*;

    fn alive(pid: i32) -> bool {
        std::fs::read_to_string(format!("/proc/{pid}/stat"))
            .ok()
            .and_then(|s| {
                s.rsplit_once(')')
                    .map(|(_, r)| !r.trim_start().starts_with('Z'))
            })
            .unwrap_or(false)
    }

    fn gone(pids: &[i32]) -> bool {
        let end = Instant::now() + Duration::from_secs(5);
        while Instant::now() < end {
            if !pids.iter().any(|p| alive(*p)) {
                return true;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        for p in pids {
            unsafe {
                libc::kill(*p, libc::SIGKILL);
            }
        }
        false
    }

    fn env() -> HashMap<String, String> {
        std::env::vars().collect()
    }

    #[test]
    fn kill_groups_signals_then_kills() {
        use std::os::unix::process::{CommandExt, ExitStatusExt};
        let mut stubborn = std::process::Command::new("sh")
            .args(["-c", "trap '' TERM; while :; do sleep 0.05; done"])
            .process_group(0)
            .spawn()
            .unwrap();
        let mut plain = std::process::Command::new("sleep")
            .arg("60")
            .process_group(0)
            .spawn()
            .unwrap();
        std::thread::sleep(Duration::from_millis(300));
        let mine = unsafe { libc::getpgrp() };
        kill_groups(&[stubborn.id() as i32, plain.id() as i32, 1, mine], None);
        assert_eq!(stubborn.wait().unwrap().signal(), Some(libc::SIGKILL));
        assert_eq!(plain.wait().unwrap().signal(), Some(libc::SIGTERM));
    }

    #[test]
    fn a_child_that_exits_leaves_nothing_in_its_group() {
        let dir = std::env::temp_dir().join(format!("kill-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let pids = dir.join("pids");
        let script = format!(
            "sleep 300 </dev/null >/dev/null 2>&1 & echo $! > {}; echo '{{}}'",
            pids.display()
        );
        let started = std::cell::Cell::new(0);
        let on = |p: i32| started.set(p);
        let r = run_group(
            "sh",
            &["-c".into(), script],
            "/",
            &env(),
            vec![],
            10,
            &[],
            Some(&on),
            None,
        )
        .unwrap();
        assert_eq!(
            (r.rc, r.timed_out, r.too_big, r.stdout.as_slice()),
            (0, false, false, b"{}\n".as_slice())
        );
        assert!(started.get() > 1);
        let pid: i32 = std::fs::read_to_string(&pids)
            .unwrap()
            .trim()
            .parse()
            .unwrap();
        let _ = std::fs::remove_dir_all(&dir);
        assert!(gone(&[pid]));
    }

    #[test]
    fn a_timeout_kills_the_branch_groups() {
        use std::os::unix::process::CommandExt;
        let mut other = std::process::Command::new("sleep")
            .arg("300")
            .process_group(0)
            .spawn()
            .unwrap();
        let pid = other.id() as i32;
        let br = || vec![pid];
        let r = run_group(
            "sh",
            &["-c".into(), "sleep 300".into()],
            "/",
            &env(),
            vec![],
            1,
            &[],
            None,
            Some(&br),
        )
        .unwrap();
        assert!(r.timed_out);
        let _ = other.wait();
        assert!(gone(&[pid]));
    }

    #[test]
    fn output_past_16_mib_is_too_big() {
        let r = run_group(
            "sh",
            &["-c".into(), "head -c 17000000 /dev/zero; sleep 300".into()],
            "/",
            &env(),
            vec![],
            30,
            &[],
            None,
            None,
        )
        .unwrap();
        assert!(r.too_big);
    }
}
