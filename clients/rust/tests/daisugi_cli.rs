//! The `daisugi` binary: `gate check` decides in a child it starts as
//! `daisugi gate check`, and the other commands keep the Python CLI's
//! contract. clients/cli_compare.py holds the full differential cases.

use std::io::Write;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn scratch(tag: &str) -> PathBuf {
    // Under target/, on disk: /tmp is RAM on some boxes.
    let dir = PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!("daisugi-cli-{}-{tag}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn daisugi(home: &PathBuf, args: &[&str]) -> Command {
    let mut c = Command::new(env!("CARGO_BIN_EXE_daisugi"));
    c.args(args)
        .env_clear()
        .env("HOME", home)
        .env("PATH", "/usr/bin:/bin")
        .current_dir(home)
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

/// Makes an executable file at `path` from `src` (a file) or `text`. A
/// child process writes it, so this process never holds a write handle to
/// it: a test that forks at that moment on another thread would carry the
/// handle into its child until the child's exec, and the exec of this
/// file would then fail with ETXTBSY.
fn put_exe(path: &std::path::Path, src: Option<&std::path::Path>, text: &str) {
    let mut c = Command::new("/bin/sh");
    c.env_clear().env("PATH", "/usr/bin:/bin").stdin(Stdio::piped());
    match src {
        Some(f) => c.args(["-c", "cp -- \"$1\" \"$2\" && chmod 755 -- \"$2\"", "sh"]).arg(f).arg(path),
        None => c.args(["-c", "cat > \"$1\" && chmod 755 -- \"$1\"", "sh"]).arg(path),
    };
    let mut child = c.spawn().unwrap();
    child.stdin.take().unwrap().write_all(text.as_bytes()).unwrap();
    assert!(child.wait().unwrap().success(), "could not make {}", path.display());
}

fn gate_root(home: &PathBuf) -> PathBuf {
    let root = home.join("gate");
    std::fs::create_dir_all(root.join("envelopes")).unwrap();
    std::fs::write(
        root.join("envelopes/default.json"),
        r#"{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"shell":true,"shell_allowlist":["ls"]}}"#,
    )
    .unwrap();
    root
}

const READ: &str = r#"{"session_id":"s","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}"#;
const RM: &str = r#"{"session_id":"s","tool_name":"Bash","tool_input":{"command":"rm x"},"cwd":"/work","tool_use_id":"t1"}"#;

#[test]
fn gate_check_answers_and_its_child_runs_gate_check() {
    let home = scratch("check");
    let root = gate_root(&home);
    let r = root.to_str().unwrap();
    let (code, out, err) = run(daisugi(&home, &["gate", "check", "--mode", "enforce", "--root", r]), READ);
    assert_eq!((code, out.as_str(), err.as_str()), (0, "{\"continue\": true}\n", ""));
    // A child killed while it waits for an operator is a deny.
    std::fs::write(root.join("operator.json"), format!(r#"{{"pid": {}, "at": 0}}"#, std::process::id())).unwrap();
    let mut p = daisugi(&home, &["gate", "check", "--mode", "enforce", "--root", r, "--ask", "--ask-timeout", "20"])
        .spawn()
        .unwrap();
    p.stdin.take().unwrap().write_all(RM.as_bytes()).unwrap();
    let start = Instant::now();
    let mut seen = false;
    while start.elapsed() < Duration::from_secs(10) && !seen {
        if root.join("asks").exists() {
            let kids = std::fs::read_to_string(format!("/proc/{0}/task/{0}/children", p.id())).unwrap_or_default();
            for k in kids.split_whitespace() {
                let argv = std::fs::read(format!("/proc/{k}/cmdline")).unwrap_or_default();
                assert!(argv.windows(12).any(|w| w == b"gate\0check\0-"), "the child is not `daisugi gate check`");
                unsafe { libc::kill(k.parse().unwrap(), libc::SIGKILL) };
                seen = true;
            }
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(seen, "the child never waited on the ask");
    let out = p.wait_with_output().unwrap();
    assert_eq!(out.status.code(), Some(2));
    assert!(String::from_utf8_lossy(&out.stderr).contains("DENIED"));
    let _ = std::fs::remove_dir_all(&home);
}

#[test]
fn the_child_variable_alone_makes_no_child() {
    let home = scratch("worker");
    let root = gate_root(&home);
    let mut c = daisugi(&home, &["gate", "check", "--mode", "enforce", "--root", root.to_str().unwrap()]);
    c.env("DAISUGI_GATE_WORKER", "set-by-someone");
    assert_eq!(run(c, READ), (0, "{\"continue\": true}\n".to_string(), String::new()));
    let _ = std::fs::remove_dir_all(&home);
}

#[test]
fn a_command_not_carried_says_so_in_one_line() {
    let home = scratch("notyet");
    let (code, out, err) = run(daisugi(&home, &["gate", "audit"]), "");
    assert_eq!((code, out.as_str(), err.as_str()), (2, "", "daisugi gate audit is not in this binary yet.\n"));
    // The version is the build's: DAISUGI_VERSION, or the checkout's git
    // describe, never a fixed number.
    let (code, out, _) = run(daisugi(&home, &["--version"]), "");
    assert_eq!((code, out.as_str()), (0, format!("{}\n", env!("DAISUGI_VERSION")).as_str()));
    assert!(!out.trim().is_empty() && out.trim() != "0.43.0", "{out}");
    let _ = std::fs::remove_dir_all(&home);
}

#[test]
fn the_everyday_commands_answer_on_a_fresh_home() {
    let home = scratch("everyday");
    let (code, out, _) = run(daisugi(&home, &["config", "--json"]), "");
    assert_eq!(code, 0);
    assert!(out.contains("\"gate_mode (resolved)\": {\n    \"value\": \"audit\""), "{out}");
    let (code, out, _) = run(daisugi(&home, &["journal", "stats", "--json"]), "");
    assert_eq!((code, out.as_str()), (0, "{\n  \"total\": 0,\n  \"passed\": 0,\n  \"failed\": 0,\n  \"avg_duration_ms\": 0.0\n}\n"));
    // The journal is made, as Journal() makes it.
    assert!(home.join(".opendaisugi/journal/index.db").exists());
    let (code, out, _) = run(daisugi(&home, &["status", "--json"]), "");
    assert_eq!(code, 0);
    // The default matcher is lexical, which the binary carries.
    assert!(out.contains("\"search_extra_installed\": true"), "{out}");
    // No claude on PATH: the harness step fails and nothing is written.
    let (code, out, _) = run(daisugi(&home, &["start", "--dry-run"]), "");
    assert_eq!(code, 1);
    assert!(out.starts_with("  harness      failed   Tried to find the agent harness."), "{out}");
    assert!(!home.join(".claude").exists());
    // Without --no-ui the failed harness step still ends start before the
    // view opens.
    let (code, out, _) = run(daisugi(&home, &["start"]), "");
    assert_eq!(code, 1);
    assert!(out.contains("  view         would    open the live view (daisugi dashboard)"), "{out}");
    let _ = std::fs::remove_dir_all(&home);
}

#[test]
fn journal_parse_and_ingest_journal_a_transcript() {
    let home = scratch("parse");
    let t = home.join("t.jsonl");
    std::fs::write(
        &t,
        concat!(
            r#"{"type":"user","message":{"role":"user","content":"list the files"}}"#,
            "\n",
            r#"{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls -la && pwd"}},{"type":"tool_use","name":"Read","input":{"file_path":"/w/a.txt"}}]}}"#,
            "\n"
        ),
    )
    .unwrap();
    let (code, out, _) = run(daisugi(&home, &["-q", "journal", "parse", "t.jsonl", "-o", "eps.yaml", "--min-tools", "1"]), "");
    assert_eq!((code, out.as_str()), (0, format!("Parsed 1 episodes to eps.yaml\n").as_str()));
    let eps = std::fs::read_to_string(home.join("eps.yaml")).unwrap();
    assert!(eps.contains("command: ls -la") && eps.contains("- s0") && eps.contains("path: /w/a.txt"), "{eps}");
    let (code, out, _) = run(daisugi(&home, &["journal", "ingest", "eps.yaml"]), "");
    assert_eq!(code, 0, "{out}");
    assert!(out.starts_with("ep_00  OK      \"list the files\" (3 steps)\n"), "{out}");
    let (code, out, _) = run(daisugi(&home, &["journal", "ingest", "eps.yaml"]), "");
    assert_eq!(code, 0);
    assert!(out.contains("  1 skipped (already in journal)"), "{out}");
    let (_, out, _) = run(daisugi(&home, &["journal", "stats"]), "");
    assert!(out.starts_with("total: 1\npassed: 1\nfailed: 0\n"), "{out}");
    let _ = std::fs::remove_dir_all(&home);
}

#[test]
fn install_writes_a_hook_status_reads_back() {
    let home = scratch("install");
    std::fs::create_dir_all(home.join(".claude")).unwrap();
    let (code, out, err) = run(daisugi(&home, &["install", "--gate", "--runtime", "claude", "--yes"]), "");
    assert_eq!(code, 0, "{out}{err}");
    let settings = std::fs::read_to_string(home.join(".claude/settings.json")).unwrap();
    assert!(settings.contains("DAISUGI_GATE_HOOK=opendaisugi.gate "), "{settings}");
    assert!(settings.contains(" gate check --mode audit "), "{settings}");
    let (code, out, _) = run(daisugi(&home, &["gate", "status", "--json"]), "");
    assert_eq!(code, 0);
    assert_eq!(
        out,
        format!(
            "{{\"armed\": true, \"mode\": \"audit\", \"mode_source\": \"global\", \"envelopes\": [], \
             \"dialect\": {{\"hash\": \"{}\", \"pin\": null, \"mode\": \"audit\", \"word_would_deny\": 0}}}}\n",
            daisugi_verify::dialect::DIALECT_HASH
        )
    );
    // A second install finds the same hooks and writes nothing; as the
    // oracle does, it still lists the skill it links (IN-1).
    let before = std::fs::read_to_string(home.join(".claude/settings.json")).unwrap();
    let (code, out, _) = run(daisugi(&home, &["install", "--gate", "--runtime", "claude", "--yes"]), "");
    assert_eq!(code, 0);
    assert!(out.contains("Done. Files modified:\n  ") && out.contains("opendaisugi-checklist\n\nRestart"), "{out}");
    assert_eq!(std::fs::read_to_string(home.join(".claude/settings.json")).unwrap(), before);
    let _ = std::fs::remove_dir_all(&home);
}

#[test]
fn enforce_install_needs_a_policy_first() {
    let home = scratch("enforce-policy");
    std::fs::create_dir_all(home.join(".claude")).unwrap();
    let (code, out, err) = run(daisugi(&home, &["install", "--gate", "--enforce", "--runtime", "claude", "--yes"]), "");
    assert_eq!(code, 1, "{out}{err}");
    assert!(err.starts_with("Enforce needs a policy first. Run: daisugi gate init --workspace DIR"), "{err}");
    assert!(!home.join(".claude/settings.json").exists());
    let envs = home.join(".opendaisugi/gate/envelopes");
    std::fs::create_dir_all(&envs).unwrap();
    std::fs::write(envs.join("default.json"), "{}").unwrap();
    let (code, out, err) = run(daisugi(&home, &["install", "--gate", "--enforce", "--runtime", "claude", "--yes"]), "");
    assert_eq!(code, 0, "{out}{err}");
    let _ = std::fs::remove_dir_all(&home);
}

/// Run from mise's versioned install path, install names mise's shim when
/// the shim runs this binary, else an Omarchy stub that runs it, else the
/// versioned path with a note; status warns once the named binary is gone.
#[test]
fn a_mise_install_hooks_the_shim_and_status_warns_on_a_gone_binary() {
    let home = scratch("mise");
    std::fs::create_dir_all(home.join(".claude")).unwrap();
    let data = home.join("mise");
    let installed = data.join("installs/github-openDaisugi/0.44.0/daisugi");
    std::fs::create_dir_all(installed.parent().unwrap()).unwrap();
    put_exe(&installed, Some(std::path::Path::new(env!("CARGO_BIN_EXE_daisugi"))), "");
    let shim = data.join("shims/daisugi");
    std::fs::create_dir_all(shim.parent().unwrap()).unwrap();
    put_exe(&shim, None, "#!/bin/sh\n");
    let tools = home.join("tools");
    std::fs::create_dir_all(&tools).unwrap();
    put_exe(&tools.join("mise"), None, &format!("#!/bin/sh\necho '{}'\n", installed.display()));
    let path = format!("{}:/usr/bin:/bin", tools.display());
    // Run the installed copy, as a mise stub would, not the cargo one.
    let installed_cmd = || {
        let mut d = Command::new(&installed);
        d.args(["install", "--gate", "--runtime", "claude", "--yes"])
            .env_clear()
            .env("HOME", &home)
            .env("PATH", &path)
            .env("MISE_DATA_DIR", &data)
            .current_dir(&home)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        d
    };
    let c = installed_cmd();
    let (code, out, err) = run(c, "");
    assert_eq!(code, 0, "{out}{err}");
    let settings = std::fs::read_to_string(home.join(".claude/settings.json")).unwrap();
    assert!(settings.contains(&format!("{} gate check", shim.display())), "{settings}");
    assert!(!err.contains("versioned mise install"), "{err}");

    // With no shim, an Omarchy stub on PATH in ~/.local/bin is next. This
    // mise answers only for the stub's own tool.
    std::fs::remove_file(home.join(".claude/settings.json")).unwrap();
    std::fs::remove_file(&shim).unwrap();
    let local = home.join(".local/bin");
    std::fs::create_dir_all(&local).unwrap();
    let stub = local.join("daisugi");
    let tool = "github:levitatingflyfisher/openDaisugi";
    put_exe(&stub, None, &format!("#!/bin/bash\nexec mise x {tool} -- daisugi \"$@\"\n"));
    put_exe(
        &tools.join("mise"),
        None,
        &format!("#!/bin/sh\n[ \"$2\" = --tool ] && [ \"$3\" = '{tool}' ] && echo '{}' && exit 0\nexit 1\n", installed.display()),
    );
    let mut d = installed_cmd();
    d.env("PATH", format!("{}:{}", local.display(), path));
    let (code, out, err) = run(d, "");
    assert_eq!(code, 0, "{out}{err}");
    let settings = std::fs::read_to_string(home.join(".claude/settings.json")).unwrap();
    assert!(settings.contains(&format!("{} gate check", stub.display())), "{settings}");
    assert!(!err.contains("versioned mise install"), "{err}");

    // With neither, the hook keeps the versioned path and says so.
    std::fs::remove_file(home.join(".claude/settings.json")).unwrap();
    std::fs::remove_file(&stub).unwrap();
    let d = installed_cmd();
    let (code, out, err) = run(d, "");
    assert_eq!(code, 0, "{out}{err}");
    assert!(err.contains("is a versioned mise install. Run daisugi install --gate again"), "{err}");

    // The upgrade removes the versioned binary: status warns.
    std::fs::remove_file(&installed).unwrap();
    let (code, _, err) = run(daisugi(&home, &["gate", "status"]), "");
    assert_eq!(code, 0);
    assert!(
        err.contains(&format!("runs {}, which does not exist, so the gate sees no call", installed.display())),
        "{err}"
    );
    let _ = std::fs::remove_dir_all(&home);
}

/// A pseudo-terminal pair: the side a program writes to and the side a
/// test reads. None when the box has no pty.
fn open_pty() -> Option<(std::fs::File, std::fs::File)> {
    use std::os::fd::FromRawFd;
    // SAFETY: the ptmx fd is ours; ptsname's buffer is read before any
    // other pty call on this thread.
    unsafe {
        let m = libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY);
        if m < 0 || libc::grantpt(m) != 0 || libc::unlockpt(m) != 0 {
            return None;
        }
        let name = libc::ptsname(m);
        if name.is_null() {
            return None;
        }
        let s = libc::open(name, libc::O_RDWR | libc::O_NOCTTY);
        if s < 0 {
            return None;
        }
        Some((std::fs::File::from_raw_fd(m), std::fs::File::from_raw_fd(s)))
    }
}

/// On a terminal the live view draws box glyphs, clears the screen before
/// each frame, lights one stage per frame, and ends on Ctrl-C with a
/// newline and exit 0.
#[test]
fn the_live_view_on_a_terminal() {
    use std::io::Read;
    let Some((mut master, slave)) = open_pty() else { return };
    let home = scratch("live");
    let mut c = daisugi(&home, &["dashboard", "--interval", "0.05"]);
    c.stdout(Stdio::from(slave));
    let mut child = c.spawn().unwrap();
    let mut all = Vec::new();
    let mut buf = vec![0u8; 65536];
    let deadline = Instant::now() + Duration::from_secs(20);
    while Instant::now() < deadline {
        let n = master.read(&mut buf).unwrap_or(0);
        all.extend_from_slice(&buf[..n]);
        if n == 0 || String::from_utf8_lossy(&all).matches("\x1b[H\x1b[2J").count() >= 3 {
            break;
        }
    }
    // SAFETY: a signal to our own child.
    unsafe {
        libc::kill(child.id() as i32, libc::SIGINT);
    }
    let status = child.wait().unwrap();
    assert_eq!(status.code(), Some(0));
    let out = String::from_utf8_lossy(&all).into_owned();
    assert!(out.contains("┌─ harness (agent host) ") && out.contains("▼ ●"), "{}", &out[..out.len().min(400)]);
    let frames: Vec<&str> = out.split("\x1b[H\x1b[2J").collect();
    assert!(frames.len() >= 3 && frames[1].find("▼ ●") != frames[2].find("▼ ●"), "the pulse did not move");
    let _ = std::fs::remove_dir_all(&home);
}

/// DAISUGI_PORT (PK-R-15): `install` run as daisugi-rs hands the whole
/// command, with its arguments, to the port the variable names, beside it;
/// with no such sibling it stops with one line and changes nothing.
#[test]
fn install_hands_over_to_the_port_daisugi_port_names() {
    let home = scratch("port");
    let bin = home.join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    put_exe(&bin.join("daisugi-rs"), Some(std::path::Path::new(env!("CARGO_BIN_EXE_daisugi"))), "");
    put_exe(&bin.join("daisugi"), None, "#!/bin/sh\necho \"go port: $* hop=$DAISUGI_PORT_HOP\"\n");
    let mut c = Command::new(bin.join("daisugi-rs"));
    c.args(["-q", "install", "--gate", "--dry-run"])
        .env_clear()
        .env("HOME", &home)
        .env("PATH", "/usr/bin:/bin")
        .env("DAISUGI_PORT", "go")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let (code, out, err) = run(c, "");
    assert_eq!((code, out.as_str()), (0, "go port: -q install --gate --dry-run hop=1\n"), "{err}");
    let mut c = Command::new(bin.join("daisugi-rs"));
    c.args(["install", "--dry-run"])
        .env_clear()
        .env("HOME", &home)
        .env("PATH", "/usr/bin:/bin")
        .env("DAISUGI_PORT", "python")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let (code, _, err) = run(c, "");
    assert_eq!(code, 2);
    assert_eq!(
        err,
        format!(
            "daisugi: DAISUGI_PORT is python, but there is no daisugi-py beside {}. Install it, or unset DAISUGI_PORT. Nothing was changed.\n",
            bin.join("daisugi-rs").display()
        )
    );
}
