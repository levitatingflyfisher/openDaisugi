//! The parent turns any abnormal end of its child into a deny in the call's
//! host contract: exit 2 with a DENIED line, or exit 0 with a block body
//! for the formats that read stdout. A child started by anyone else, and a
//! process that only sets the child's variable, never read as an allow.

use std::io::Write;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn root(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dg-backstop-{}-{tag}", std::process::id()));
    let root = dir.join("gate");
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(root.join("envelopes")).unwrap();
    std::fs::write(
        root.join("envelopes/default.json"),
        r#"{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"shell":true,"shell_allowlist":["ls"]}}"#,
    )
    .unwrap();
    root
}

fn cmd(root: &PathBuf, extra: &[&str]) -> Command {
    let mut c = Command::new(env!("CARGO_BIN_EXE_daisugi-gate"));
    c.args(["--mode", "enforce", "--root", root.to_str().unwrap()])
        .args(extra)
        .env_clear()
        .env("HOME", "/home/user")
        .env("PATH", "/usr/bin:/bin")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    c
}

fn run(mut c: Command, stdin: &str) -> (i32, String, String) {
    let mut child = c.spawn().unwrap();
    child.stdin.take().unwrap().write_all(stdin.as_bytes()).unwrap();
    let out = child.wait_with_output().unwrap();
    (
        out.status.code().unwrap_or(-1),
        String::from_utf8_lossy(&out.stdout).into_owned(),
        String::from_utf8_lossy(&out.stderr).into_owned(),
    )
}

const READ: &str = r#"{"session_id":"s","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}"#;
const RM: &str = r#"{"session_id":"s","tool_name":"Bash","tool_input":{"command":"rm x"},"cwd":"/work","tool_use_id":"t1"}"#;

/// The pids of `pid`'s children.
fn children(pid: u32) -> Vec<u32> {
    std::fs::read_to_string(format!("/proc/{pid}/task/{pid}/children"))
        .unwrap_or_default()
        .split_whitespace()
        .filter_map(|x| x.parse().ok())
        .collect()
}

/// A child killed while it waits for an operator: the parent denies in the
/// format argparse reads, abbreviated or repeated --format included.
#[test]
fn a_child_that_dies_is_a_deny_in_every_format() {
    for (i, fmt) in [["--format", "claude"], ["--format", "hermes"], ["--form", "openclaw"], ["--format", "pi"]]
        .iter()
        .enumerate()
    {
        let r = root(&format!("kill{i}"));
        // A live operator, so the child waits on the ask.
        std::fs::write(r.join("operator.json"), format!(r#"{{"pid": {}, "at": 0}}"#, std::process::id())).unwrap();
        let mut c = cmd(&r, &["--ask", "--ask-timeout", "20"]);
        c.args(*fmt);
        let mut p = c.spawn().unwrap();
        p.stdin.take().unwrap().write_all(RM.as_bytes()).unwrap();
        let start = Instant::now();
        let mut killed = false;
        while start.elapsed() < Duration::from_secs(10) && !killed {
            if r.join("asks").exists() {
                for k in children(p.id()) {
                    unsafe { libc::kill(k as i32, libc::SIGKILL) };
                    killed = true;
                }
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(killed, "{fmt:?}: no child to kill");
        let out = p.wait_with_output().unwrap();
        let (code, so, se) =
            (out.status.code().unwrap_or(-1), String::from_utf8_lossy(&out.stdout), String::from_utf8_lossy(&out.stderr));
        match fmt[1] {
            "hermes" => assert!(code == 0 && so.contains(r#""decision": "block""#), "{so}"),
            "openclaw" => assert!(code == 0 && so.contains(r#""block": true"#), "{so}"),
            _ => assert!(code == 2 && so.is_empty() && se.starts_with("openDaisugi gate: DENIED"), "{se}"),
        }
        assert!(start.elapsed() < Duration::from_secs(15));
    }
}

#[test]
fn a_child_that_answers_is_passed_on_unchanged() {
    let r = root("answer");
    let (code, out, err) = run(cmd(&r, &["--format", "claude"]), READ);
    assert_eq!((code, out.as_str(), err.as_str()), (0, "{\"continue\": true}\n", ""));
}

/// The child's variable alone, with no frame pipe, makes a parent: the call
/// is still decided, with the backstop.
#[test]
fn the_child_variable_alone_is_ignored() {
    let r = root("var");
    let mut c = cmd(&r, &["--format", "claude"]);
    c.env("DAISUGI_GATE_WORKER", "n");
    let (code, out, _) = run(c, READ);
    assert_eq!((code, out.as_str()), (0, "{\"continue\": true}\n"));
}

/// A child started by anyone else, with a pipe on fd 3, writes its verdict
/// only to that pipe: on its own stdout and exit code it denies.
#[test]
fn a_child_started_by_mistake_reads_as_a_deny() {
    let r = root("mistake");
    let mut fds = [0i32; 2];
    assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
    let w = fds[1];
    let mut c = cmd(&r, &["--format", "claude"]);
    c.env("DAISUGI_GATE_WORKER", "n");
    unsafe {
        use std::os::unix::process::CommandExt;
        c.pre_exec(move || {
            libc::dup2(w, 3);
            Ok(())
        });
    }
    let (code, out, err) = run(c, READ);
    unsafe {
        libc::close(fds[0]);
        libc::close(fds[1]);
    }
    assert!(code == 2 && out.is_empty() && err.starts_with("openDaisugi gate: DENIED"), "{code} {out} {err}");
}

#[test]
fn a_deep_chain_ends_in_a_contract_code() {
    let r = root("deep");
    let line = vec!["ls"; 10_000].join(" && ");
    let payload = format!(r#"{{"session_id":"s","tool_name":"Bash","tool_input":{{"command":"{line}"}},"cwd":"/work"}}"#);
    let (code, _, err) = run(cmd(&r, &["--format", "claude"]), &payload);
    assert_eq!(code, 2, "{err}");
}

#[test]
fn a_payload_past_the_cap_is_denied_unread() {
    let r = root("big");
    let big = format!(r#"{{"session_id":"s","x":"{}"}}"#, "a".repeat(16 * 1024 * 1024));
    let (code, _, err) = run(cmd(&r, &["--format", "claude"]), &big);
    assert!(code == 2 && err.contains("hook payload is larger than 16777216 bytes"), "{err}");
}
