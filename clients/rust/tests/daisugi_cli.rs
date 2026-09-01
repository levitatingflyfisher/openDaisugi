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
    let (code, out, err) = run(daisugi(&home, &["journal"]), "");
    assert_eq!((code, out.as_str(), err.as_str()), (2, "", "daisugi journal is not in this binary yet.\n"));
    // The version is the build's: DAISUGI_VERSION, or the checkout's git
    // describe, never a fixed number.
    let (code, out, _) = run(daisugi(&home, &["--version"]), "");
    assert_eq!((code, out.as_str()), (0, format!("{}\n", env!("DAISUGI_VERSION")).as_str()));
    assert!(!out.trim().is_empty() && out.trim() != "0.43.0", "{out}");
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
    assert!(settings.contains(" gate check --mode shadow "), "{settings}");
    let (code, out, _) = run(daisugi(&home, &["gate", "status", "--json"]), "");
    assert_eq!(code, 0);
    assert_eq!(out, "{\"armed\": true, \"mode\": \"shadow\", \"mode_source\": \"global\", \"envelopes\": []}\n");
    // A second install finds the same hook and changes nothing.
    let (code, out, _) = run(daisugi(&home, &["install", "--gate", "--runtime", "claude", "--yes"]), "");
    assert_eq!(code, 0);
    assert!(out.contains("All runtimes were already configured"), "{out}");
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
    use std::os::unix::fs::PermissionsExt;
    let home = scratch("mise");
    std::fs::create_dir_all(home.join(".claude")).unwrap();
    let data = home.join("mise");
    let installed = data.join("installs/github-openDaisugi/0.44.0/daisugi");
    std::fs::create_dir_all(installed.parent().unwrap()).unwrap();
    std::fs::copy(env!("CARGO_BIN_EXE_daisugi"), &installed).unwrap();
    let shim = data.join("shims/daisugi");
    std::fs::create_dir_all(shim.parent().unwrap()).unwrap();
    std::fs::write(&shim, "#!/bin/sh\n").unwrap();
    std::fs::set_permissions(&shim, std::fs::Permissions::from_mode(0o755)).unwrap();
    let tools = home.join("tools");
    std::fs::create_dir_all(&tools).unwrap();
    std::fs::write(tools.join("mise"), format!("#!/bin/sh\necho '{}'\n", installed.display())).unwrap();
    std::fs::set_permissions(tools.join("mise"), std::fs::Permissions::from_mode(0o755)).unwrap();
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
    std::fs::write(&stub, format!("#!/bin/bash\nexec mise x {tool} -- daisugi \"$@\"\n")).unwrap();
    std::fs::set_permissions(&stub, std::fs::Permissions::from_mode(0o755)).unwrap();
    std::fs::write(
        tools.join("mise"),
        format!("#!/bin/sh\n[ \"$2\" = --tool ] && [ \"$3\" = '{tool}' ] && echo '{}' && exit 0\nexit 1\n", installed.display()),
    )
    .unwrap();
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
