//! `daisugi fence` against the real kernel: the built binary runs a shell
//! under the fence in a fake HOME, and the shell reports what it could do.
//! The same probes as the Go client's cmd/daisugi/fence_test.go.

use std::cell::RefCell;
use std::collections::HashMap;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};

// Test temp dirs go inside a private TMPDIR that is removed at exit.
#[path = "../src/testtmp.rs"]
mod testtmp;

static N: AtomicUsize = AtomicUsize::new(0);

struct World {
    base: PathBuf,
    home: String,
    ws: String,
    tmp: String,
    outside: String,
    /// Each resident gate the fence said it started: ended by pid on drop.
    servers: RefCell<Vec<i32>>,
}

impl Drop for World {
    fn drop(&mut self) {
        for pid in self.servers.borrow().iter() {
            unsafe { libc::kill(*pid, libc::SIGTERM) };
        }
        let _ = fs::remove_dir_all(&self.base);
    }
}

fn world() -> World {
    world_under(&std::env::temp_dir())
}

/// A world under dir, not under TMPDIR.
fn world_under(dir: &Path) -> World {
    let base = fs::canonicalize(dir)
        .unwrap()
        .join(format!("fr{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
    let s = |p: &str| format!("{}/{p}", base.display());
    let w = World {
        home: s("home"),
        ws: s("ws"),
        tmp: s("tmp"),
        outside: s("outside"),
        base: base.clone(),
        servers: RefCell::new(vec![]),
    };
    for d in [
        format!("{}/.ssh", w.home),
        format!("{}/.opendaisugi/gate/envelopes", w.home),
        format!("{}/.claude", w.home),
        format!("{}/other", w.home),
        w.ws.clone(),
        w.tmp.clone(),
        w.outside.clone(),
    ] {
        fs::create_dir_all(&d).unwrap();
    }
    for (p, body) in [
        (format!("{}/.ssh/key", w.home), "SECRET"),
        (format!("{}/other/file", w.home), "OTHER"),
        (format!("{}/.opendaisugi/gate/envelopes/default.json", w.home), r#"{"permissions":{"file_read":[],"file_write":[]}}"#),
        (format!("{}/victim", w.outside), "VICTIM"),
        (format!("{}/ok", w.ws), "OK"),
    ] {
        fs::write(p, body).unwrap();
    }
    w
}

fn env(w: &World, extra: &[(&str, &str)]) -> HashMap<String, String> {
    let mut m: HashMap<String, String> = [
        ("HOME", w.home.as_str()),
        ("TMPDIR", w.tmp.as_str()),
        ("PATH", "/usr/bin:/bin"),
        ("LANG", "C"),
    ]
    .iter()
    .map(|(k, v)| (k.to_string(), v.to_string()))
    .collect();
    for (k, v) in extra {
        m.insert(k.to_string(), v.to_string());
    }
    m
}

fn bin() -> String {
    // FENCE_TEST_BIN runs these tests against another build (the red half
    // of red then green).
    std::env::var("FENCE_TEST_BIN").unwrap_or_else(|_| env!("CARGO_BIN_EXE_daisugi").to_string())
}

fn run(w: &World, env: &HashMap<String, String>, args: &[&str]) -> (String, String, i32) {
    run_fd(w, env, None, args)
}

/// run, with `fd3` (when set) open as descriptor 3 in the fence process.
fn run_fd(w: &World, env: &HashMap<String, String>, fd3: Option<i32>, args: &[&str]) -> (String, String, i32) {
    let mut c = Command::new(bin());
    c.arg("fence").args(args).current_dir(&w.ws).env_clear().envs(env).stdin(Stdio::null());
    if let Some(fd) = fd3 {
        // SAFETY: dup2 is async-signal-safe.
        unsafe {
            c.pre_exec(move || {
                // dup2 onto itself keeps close-on-exec: clear it, so the
                // descriptor is open in the fence whatever number it had.
                if fd == 3 {
                    libc::fcntl(3, libc::F_SETFD, 0);
                } else {
                    libc::dup2(fd, 3);
                }
                Ok(())
            });
        }
    }
    let out = c.output().unwrap();
    let se = String::from_utf8_lossy(&out.stderr).into_owned();
    for line in se.lines() {
        if let Some(rest) = line.split("started the resident gate on ").nth(1) {
            if let Some(pid) = rest.split("(pid ").nth(1).and_then(|p| p.trim_end_matches(')').parse::<i32>().ok()) {
                if pid > 1 {
                    w.servers.borrow_mut().push(pid);
                }
            }
        }
    }
    (String::from_utf8_lossy(&out.stdout).into_owned(), se, out.status.code().unwrap_or(-1))
}

/// This world's workspace's dir under ~/.claude/projects.
fn claude_project(w: &World) -> String {
    let name: String = w.ws.chars().map(|c| if c.is_ascii_alphanumeric() { c } else { '-' }).collect();
    format!("{}/.claude/projects/{name}", w.home)
}

fn strip_started(se: &str) -> String {
    se.lines()
        .filter(|l| !l.starts_with("daisugi fence: started the resident gate on "))
        .map(|l| format!("{l}\n"))
        .collect()
}

fn probes(w: &World) -> String {
    let (h, o) = (&w.home, &w.outside);
    let ws = &w.ws;
    let lines = vec![
        "t() { n=$1; shift; if \"$@\" >/dev/null 2>&1; then echo \"$n ok\"; else echo \"$n denied\"; fi; }".to_string(),
        format!("t read_ws cat {ws}/ok"),
        format!("t write_ws sh -c 'echo x > {ws}/new'"),
        format!("t mkdir_ws mkdir {ws}/sub"),
        "t write_tmp sh -c 'echo x > \"$TMPDIR/f\"'".to_string(),
        format!("t write_shared_tmp sh -c 'echo x > {}/f'", w.tmp),
        "t read_slash_tmp ls /tmp".to_string(),
        "t write_devnull sh -c 'echo x > /dev/null'".to_string(),
        "t read_system cat /etc/hostname".to_string(),
        "t exec_system /usr/bin/true".to_string(),
        format!("t read_ssh cat {h}/.ssh/key"),
        format!("t list_ssh ls {h}/.ssh"),
        format!("t read_home_other cat {h}/other/file"),
        format!("t write_home sh -c 'echo x > {h}/newfile'"),
        format!("t write_outside sh -c 'echo x > {o}/new'"),
        format!("t clobber_outside sh -c 'echo x > {o}/victim'"),
        format!("t truncate_outside truncate -s 0 {o}/victim"),
        format!("t remove_outside rm -rf {o}"),
        format!("t read_outside cat {o}/victim"),
        format!("t write_gate_disarmed sh -c 'echo x > {h}/.opendaisugi/gate/DISARMED'"),
        format!("t write_gate_envelope sh -c 'echo x > {h}/.opendaisugi/gate/envelopes/default.json'"),
        format!("t read_gate_envelope cat {h}/.opendaisugi/gate/envelopes/default.json"),
        format!("t mkdir_gate mkdir {h}/.opendaisugi/gate/envelopes/x"),
        format!("t rename_in_gate mv {h}/.opendaisugi/gate/envelopes/default.json {h}/.opendaisugi/gate/envelopes/d2.json"),
        format!("t hardlink_ssh ln {h}/.ssh/key {ws}/k"),
        format!("t symlink_ssh_read sh -c 'ln -s {h}/.ssh/key {ws}/lnk && cat {ws}/lnk'"),
        format!("t move_ssh_out mv {h}/.ssh/key {ws}/k2"),
        format!("t move_ws_to_outside mv {ws}/ok {o}/ok"),
        format!("t child_reads_ssh sh -c 'sh -c \"cat {h}/.ssh/key\"'"),
        format!("t python_rmtree python3 -c \"import shutil; shutil.rmtree('{o}')\""),
        format!("t python_disarm python3 -c \"open('{h}/.opendaisugi/gate/DISARMED','w')\""),
    ];
    lines.join("\n")
}

fn parse(out: &str) -> HashMap<String, String> {
    out.lines()
        .filter_map(|l| {
            let f: Vec<&str> = l.split_whitespace().collect();
            (f.len() == 2).then(|| (f[0].to_string(), f[1].to_string()))
        })
        .collect()
}

#[test]
fn the_fence_holds_the_line() {
    let w = world();
    let script = probes(&w);
    let (so, se, code) = run(&w, &env(&w, &[]), &["--workspace", &w.ws, "--", "/bin/sh", "-c", &script]);
    assert_eq!(code, 0, "{so}{se}");
    let got = parse(&so);
    let ok = [
        "read_ws", "write_ws", "mkdir_ws", "write_tmp", "write_devnull", "read_system", "exec_system",
        "read_gate_envelope",
    ];
    let denied = [
        "read_ssh", "list_ssh", "read_home_other", "write_home", "write_outside", "clobber_outside",
        "truncate_outside", "remove_outside", "read_outside", "write_gate_disarmed", "write_shared_tmp",
        "read_slash_tmp",
        "write_gate_envelope", "mkdir_gate", "rename_in_gate", "hardlink_ssh", "symlink_ssh_read",
        "move_ssh_out", "move_ws_to_outside", "child_reads_ssh", "python_rmtree", "python_disarm",
    ];
    for n in ok {
        assert_eq!(got.get(n).map(String::as_str), Some("ok"), "{n}");
    }
    for n in denied {
        assert_eq!(got.get(n).map(String::as_str), Some("denied"), "{n}");
    }
    for p in [
        format!("{}/.opendaisugi/gate/DISARMED", w.home),
        format!("{}/new", w.outside),
        format!("{}/newfile", w.home),
        format!("{}/k", w.ws),
    ] {
        assert!(fs::symlink_metadata(&p).is_err(), "{p} exists after the run");
    }
    assert_eq!(fs::read_to_string(format!("{}/victim", w.outside)).unwrap(), "VICTIM");
    assert_eq!(fs::read_to_string(format!("{}/new", w.ws)).unwrap(), "x\n");
}

#[test]
fn no_fence_runs_the_harness_unfenced() {
    let w = world();
    let key = format!("{}/.ssh/key", w.home);
    let (so, _, code) = run(&w, &env(&w, &[]), &["--no-fence", "--", "/bin/cat", &key]);
    assert_eq!((code, so.as_str()), (0, "SECRET"));
}

/// FN-V-1: no variable turns the fence off; only --no-fence does.
#[test]
fn fn_v1_no_variable_turns_the_fence_off() {
    let w = world();
    let key = format!("{}/.ssh/key", w.home);
    let (so, _, _) = run(&w, &env(&w, &[("DAISUGI_FENCE", "off")]), &["--", "/bin/cat", &key]);
    assert_ne!(so, "SECRET", "DAISUGI_FENCE=off ran the harness unfenced");
}

/// FN-V-3: a secret is never named by being the workspace, a link to it,
/// the temp dir or a work dir.
#[test]
fn fn_v3_a_secret_as_workspace_or_temp_is_refused() {
    let w = world();
    let ssh = format!("{}/.ssh", w.home);
    let link = format!("{}/x", w.ws);
    std::os::unix::fs::symlink(&ssh, &link).unwrap();
    let key = format!("{ssh}/key");
    for args in [["--workspace", &ssh], ["--workspace", &link], ["--tmpdir", &ssh], ["--workdir", &link]] {
        let mut a: Vec<&str> = args.to_vec();
        a.extend(["--", "/bin/cat", &key]);
        let (so, se, code) = run(&w, &env(&w, &[]), &a);
        assert!(code == 1 && so != "SECRET" && se.contains("secret"), "{args:?}: {code} {so} {se}");
    }
}

/// FN-V-8: a rule root the plan did not resolve that links to a secret is
/// caught when the fence opens it; a harness path that links is caught by
/// the plan.
#[test]
fn fn_v8_a_rule_root_that_links_to_a_secret_is_refused() {
    let w = world();
    let cfg = format!("{}/.opendaisugi/config.yaml", w.home);
    std::os::unix::fs::symlink(format!("{}/.ssh/key", w.home), &cfg).unwrap();
    let (so, se, code) = run(&w, &env(&w, &[]), &["--", "/bin/cat", &cfg]);
    assert!(code == 1 && so != "SECRET" && se.contains("resolves to"), "{code} {so} {se}");
    let w2 = world();
    fs::create_dir_all(format!("{}/.config", w2.home)).unwrap();
    std::os::unix::fs::symlink(format!("{}/.ssh", w2.home), format!("{}/.config/claude", w2.home)).unwrap();
    let key = format!("{}/.config/claude/key", w2.home);
    let (so, se, code) = run(&w2, &env(&w2, &[]), &["--harness", "claude", "--", "/bin/cat", &key]);
    assert!(code == 1 && so != "SECRET", "{code} {so} {se}");
}

/// FN-V-2: sockets outside the granted dirs, abstract sockets and signals
/// to outside processes are stopped; the coppice socket still connects.
#[test]
fn fn_v2_sockets_and_signals_stay_inside() {
    use std::os::unix::net::UnixListener;
    let w = world();
    let cop = format!("{}/run/coppice", w.base.display());
    fs::create_dir_all(&cop).unwrap();
    let outside = UnixListener::bind(format!("{}/agent.sock", w.outside)).unwrap();
    let coppice = UnixListener::bind(format!("{cop}/server.sock")).unwrap();
    let abstract_name = format!("daisugi-fence-rs-{}", std::process::id());
    let abs = {
        use std::os::linux::net::SocketAddrExt;
        let addr = std::os::unix::net::SocketAddr::from_abstract_name(abstract_name.as_bytes()).unwrap();
        UnixListener::bind_addr(&addr).unwrap()
    };
    for l in [&outside, &coppice, &abs] {
        l.set_nonblocking(true).unwrap();
    }
    let mut victim = Command::new("/bin/sleep").arg("30").spawn().unwrap();
    let py = |addr: &str| format!("python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect({addr})'");
    let q = |s: String| s.replace('\'', "'\"'\"'");
    let script = [
        "t() { n=$1; shift; if \"$@\" >/dev/null 2>&1; then echo \"$n ok\"; else echo \"$n denied\"; fi; }".to_string(),
        format!("t outside_sock sh -c '{}'", q(py(&format!("\"{}/agent.sock\"", w.outside)))),
        format!("t coppice_sock sh -c '{}'", q(py(&format!("\"{cop}/server.sock\"")))),
        format!("t abstract_sock sh -c '{}'", q(py(&format!("\"\\0{abstract_name}\"")))),
        format!("t kill_outside kill -TERM {}", victim.id()),
        "t ssh_env sh -c 'test -z \"$SSH_AUTH_SOCK$DBUS_SESSION_BUS_ADDRESS$DISPLAY\"'".to_string(),
    ]
    .join("\n");
    let agent = format!("{}/agent.sock", w.outside);
    let e = env(&w, &[("SSH_AUTH_SOCK", &agent), ("DBUS_SESSION_BUS_ADDRESS", "unix:path=/x"), ("DISPLAY", ":0")]);
    let csock = format!("{cop}/server.sock");
    let (so, se, code) = run(&w, &e, &["--coppice-socket", &csock, "--", "/bin/sh", "-c", &script]);
    assert_eq!(code, 0, "{so}{se}");
    let got = parse(&so);
    for (k, v) in
        [("outside_sock", "denied"), ("coppice_sock", "ok"), ("abstract_sock", "denied"), ("kill_outside", "denied"), ("ssh_env", "ok")]
    {
        assert_eq!(got.get(k).map(String::as_str), Some(v), "{k}: {so}");
    }
    assert!(victim.try_wait().unwrap().is_none(), "the outside process was killed");
    let _ = victim.kill();
    let _ = victim.wait();
    let (_, se, code) = run(&w, &env(&w, &[("DAISUGI_FENCE_PROBE", "abi:5")]), &["--", "/bin/true"]);
    assert!(code == 1 && se.contains("--allow-weak-ipc"), "abi 5: {code} {se}");
    let (_, se, code) = run(&w, &env(&w, &[("DAISUGI_FENCE_PROBE", "abi:5")]), &["--allow-weak-ipc", "--", "/bin/true"]);
    assert_eq!(code, 0, "abi 5 weak ipc: {se}");
}

/// FN-V-10: a descriptor the caller left open does not reach the harness,
/// unless --keep-fd names it.
#[test]
fn fn_v10_inherited_descriptors_are_closed() {
    use std::os::fd::AsRawFd;
    let w = world();
    let f = fs::File::open(format!("{}/victim", w.outside)).unwrap();
    let fd = f.as_raw_fd();
    let (so, _, _) = run_fd(&w, &env(&w, &[]), Some(fd), &["--", "/bin/sh", "-c", "cat <&3"]);
    assert_ne!(so, "VICTIM", "fd 3 reached the harness");
    let (so, se, code) = run_fd(&w, &env(&w, &[]), Some(fd), &["--keep-fd", "3", "--", "/bin/sh", "-c", "cat <&3"]);
    assert_eq!((code, so.as_str()), (0, "VICTIM"), "{se}");
}

/// FN-V-9: the temp dir is private, on disk, 0700, and not /tmp; a dead
/// run's dir is swept; --allow /tmp:rw keeps the shared one.
#[test]
fn fn_v9_the_private_temp_dir() {
    // --allow /tmp:rw below must not reach the fake home's secrets, so the
    // world lives off /tmp whatever TMPDIR the run has: under /var/tmp.
    if !Path::new("/var/tmp").is_dir() {
        return;
    }
    let w = world_under(Path::new("/var/tmp"));
    let (so, se, code) = run(&w, &env(&w, &[]), &["--", "/bin/sh", "-c", "echo \"$TMPDIR\"; stat -c %a \"$TMPDIR\""]);
    assert_eq!(code, 0, "{se}");
    let f: Vec<&str> = so.split_whitespace().collect();
    assert!(f.len() == 2 && f[0].starts_with(&format!("{}/.local/share/opendaisugi/fence-tmp/", w.home)) && f[1] == "700", "{so}");
    let dead = format!("{}/.local/share/opendaisugi/fence-tmp/999999999", w.home);
    fs::create_dir_all(&dead).unwrap();
    run(&w, &env(&w, &[]), &["--", "/bin/true"]);
    assert!(!Path::new(&dead).exists(), "a dead run's temp dir stayed");
    let (so, _, code) = run(&w, &env(&w, &[]), &["--allow", "/tmp:rw", "--", "/bin/sh", "-c", "echo \"$TMPDIR\""]);
    assert_eq!((code, so.trim()), (0, w.tmp.as_str()));
}

/// FN-V-6: the harness's config, hooks and settings stay read only.
#[test]
fn fn_v6_harness_config_is_read_only() {
    let w = world();
    let c = format!("{}/.claude", w.home);
    fs::write(format!("{c}/settings.json"), "{}").unwrap();
    let script = format!(
        "t() {{ n=$1; shift; if \"$@\" >/dev/null 2>&1; then echo \"$n ok\"; else echo \"$n denied\"; fi; }}\n\
         t read_settings cat {c}/settings.json\n\
         t write_settings sh -c 'echo x > {c}/settings.json'\n\
         t plant_hook mkdir -p {c}/hooks\n\
         t plant_skill sh -c 'echo x > {c}/CLAUDE.md'\n\
         t write_projects sh -c 'echo x > {own}/s.jsonl'\n\
         t write_own_memory sh -c 'mkdir -p {own}/memory && echo x > {own}/memory/m.md'\n\
         t write_other_project sh -c 'echo x > {c}/projects/p'\n\
         t write_other_memory sh -c 'echo x > {c}/projects/-elsewhere/memory/MEMORY.md'\n",
        own = claude_project(&w)
    );
    // FN-R-31: only this workspace's project dir is writable; another
    // project's memory is not. Landlock cannot cut memory/ out of the own one.
    fs::create_dir_all(format!("{c}/projects/-elsewhere/memory")).unwrap();
    let (so, se, code) = run(&w, &env(&w, &[]), &["--harness", "claude", "--", "/bin/sh", "-c", &script]);
    assert_eq!(code, 0, "{se}");
    let got = parse(&so);
    for (k, v) in [
        ("read_settings", "ok"),
        ("write_settings", "denied"),
        ("plant_hook", "denied"),
        ("plant_skill", "denied"),
        ("write_projects", "ok"),
        ("write_own_memory", "ok"),
        ("write_other_project", "denied"),
        ("write_other_memory", "denied"),
    ] {
        assert_eq!(got.get(k).map(String::as_str), Some(v), "{k}: {so}");
    }
}

#[test]
fn the_fence_refuses_when_landlock_is_missing() {
    for probe in ["enosys", "eopnotsupp", "abi:2"] {
        let w = world();
        let marker = format!("{}/ran", w.ws);
        let script = format!("touch {marker}");
        let (_, se, code) = run(&w, &env(&w, &[("DAISUGI_FENCE_PROBE", probe)]), &["--", "/bin/sh", "-c", &script]);
        assert_eq!(code, 1, "{probe}");
        assert!(!Path::new(&marker).exists(), "{probe}: the harness ran");
        assert!(se.contains("--no-fence") && se.contains("fence = \"off\""), "{probe}: {se}");
    }
}

#[test]
fn the_fence_refuses_a_workspace_that_would_open_a_secret() {
    let w = world();
    let (_, se, code) = run(&w, &env(&w, &[]), &["--workspace", &w.home, "--", "/bin/true"]);
    assert_eq!(code, 1);
    assert!(se.contains("would reach the secret") && se.contains("--no-fence"), "{se}");
}

#[test]
fn print_json_lists_the_rules() {
    let w = world();
    let (so, se, code) =
        run(&w, &env(&w, &[]), &["--print", "--json", "--harness", "claude", "--workspace", &w.ws]);
    assert_eq!(code, 0, "{se}");
    let v: serde_json::Value = serde_json::from_str(&so).unwrap();
    let modes: HashMap<String, String> = v["rules"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| (r["path"].as_str().unwrap().to_string(), r["mode"].as_str().unwrap().to_string()))
        .collect();
    assert_eq!(modes.get(&w.ws).map(String::as_str), Some("rw"));
    assert!(!modes.contains_key(&w.tmp), "the shared temp dir is granted");
    assert_eq!(modes.get(&format!("{}/.claude", w.home)).map(String::as_str), Some("ro"));
    assert_eq!(modes.get(&claude_project(&w)).map(String::as_str), Some("rw"));
    assert!(!modes.contains_key(&format!("{}/.ssh", w.home)));
    assert_eq!(modes.get(&format!("{}/.opendaisugi/gate", w.home)).map(String::as_str), Some("sock"));
}

#[test]
fn allow_opens_only_what_it_names() {
    let w = world();
    let script = format!("cat {o}/victim; echo y > {o}/w", o = w.outside);
    let (_, _, code) = run(&w, &env(&w, &[]), &["--allow", &w.outside, "--", "/bin/sh", "-c", &script]);
    assert!(code != 0 && !Path::new(&format!("{}/w", w.outside)).exists());
    let rw = format!("{}:rw", w.outside);
    let (_, _, code) = run(&w, &env(&w, &[]), &["--allow", &rw, "--", "/bin/sh", "-c", &script]);
    assert!(code == 0 && Path::new(&format!("{}/w", w.outside)).exists());
}

/// The gate hook is a child of the harness, so it runs fenced: it asks the
/// resident gate (the Rust gate here, inside the Rust fence), which logs,
/// and the agent must still not touch the marker, the envelope, the
/// answers, the asks or the logs (FN-V-4).
#[test]
fn the_gate_hook_logs_inside_the_fence_and_the_agent_cannot_erase_the_log() {
    let w = world();
    let root = format!("{}/.opendaisugi/gate", w.home);
    fs::write(
        format!("{root}/envelopes/default.json"),
        r#"{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":[]}}"#,
    )
    .unwrap();
    fs::create_dir_all(format!("{root}/answers")).unwrap();
    let payload = r#"{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}"#;
    fs::write(format!("{}/allow.json", w.ws), payload).unwrap();
    // FENCE_TEST_GATE_BIN lets a scratch build of the fence borrow another
    // binary that has the gate.
    let bin = std::env::var("FENCE_TEST_GATE_BIN").unwrap_or_else(|_| env!("CARGO_BIN_EXE_daisugi").to_string());
    let check = format!("{bin} gate check --mode enforce --root {root} --format claude < {}/allow.json", w.ws);
    let (plain, plain_err, plain_code) = run(&w, &env(&w, &[]), &["--no-fence", "--", "/bin/sh", "-c", &check]);
    let bin_dir = Path::new(&bin).parent().unwrap().display().to_string();
    let (fenced, fenced_err, fenced_code) = run(
        &w,
        &env(&w, &[]),
        &["--workspace", &w.ws, "--allow", &bin_dir, "--", "/bin/sh", "-c", &check],
    );
    let fenced_err = strip_started(&fenced_err);
    assert_eq!((plain.as_str(), plain_err.as_str(), plain_code), (fenced.as_str(), fenced_err.as_str(), fenced_code));
    assert_eq!(fenced_code, 0, "{fenced_err}");
    assert!(Path::new(&format!("{root}/audit/s1.jsonl")).exists(), "no audit log");
    assert!(Path::new(&format!("{}/.opendaisugi/sessions/s1.jsonl", w.home)).exists(), "no session tree");
    let script = format!(
        "t() {{ n=$1; shift; if \"$@\" >/dev/null 2>&1; then echo \"$n ok\"; else echo \"$n denied\"; fi; }}\n\
         t disarm sh -c 'echo x > {root}/DISARMED'\n\
         t answer sh -c 'echo x > {root}/answers/a1.json'\n\
         t envelope sh -c 'echo x > {root}/envelopes/default.json'\n\
         t erase_audit rm {root}/audit/s1.jsonl\n\
         t truncate_audit truncate -s 0 {root}/audit/s1.jsonl\n\
         t rename_audit mv {root}/audit/s1.jsonl {root}/audit/gone\n\
         t append_audit sh -c 'echo forged >> {root}/audit/s1.jsonl'\n\
         t write_ask sh -c 'mkdir -p {root}/asks; echo x > {root}/asks/a.json'\n\
         t write_session sh -c 'echo x >> {home}/.opendaisugi/sessions/s1.jsonl'\n",
        home = w.home
    );
    let (so, se, code) = run(&w, &env(&w, &[]), &["--workspace", &w.ws, "--", "/bin/sh", "-c", &script]);
    assert_eq!(code, 0, "{se}");
    let got = parse(&so);
    assert_eq!(got.len(), 9, "{so}");
    for (k, v) in got {
        assert_eq!(v, "denied", "{k}");
    }
}

const ENVELOPE: &str = r#"{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":[]}}"#;

/// FN-V-4: with the server down a fenced hook denies; an ask posted from
/// inside the fence is answered by the operator and the answer consumed.
#[test]
fn fn_v4_the_fenced_hook_asks_the_resident_gate() {
    let w = world();
    let root = format!("{}/.opendaisugi/gate", w.home);
    fs::write(format!("{root}/envelopes/default.json"), ENVELOPE).unwrap();
    let gate_bin = std::env::var("FENCE_TEST_GATE_BIN").unwrap_or_else(|_| bin());
    let mut e = env(&w, &[]);
    e.insert("DAISUGI_FENCED".into(), "landlock".into());
    let out = Command::new(&gate_bin)
        .args(["gate", "check", "--mode", "enforce", "--root", &root, "--format", "claude"])
        .env_clear()
        .envs(&e)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .and_then(|mut c| {
            use std::io::Write;
            c.stdin.take().unwrap().write_all(br#"{"session_id":"s0","tool_name":"Read","tool_input":{"file_path":"/w"},"cwd":"/w"}"#)?;
            c.wait_with_output()
        })
        .unwrap();
    let se = String::from_utf8_lossy(&out.stderr);
    assert!(out.status.code() == Some(2) && se.contains("resident gate"), "server down: {:?} {se}", out.status);
    for d in ["asks", "answers"] {
        fs::create_dir_all(format!("{root}/{d}")).unwrap();
    }
    let payload = r#"{"session_id":"s9","tool_name":"Write","tool_use_id":"tu1","tool_input":{"file_path":"/work/a","content":"x"},"cwd":"/work"}"#;
    fs::write(format!("{}/ask.json", w.ws), payload).unwrap();
    let beat = {
        let root = root.clone();
        move || {
            let _ = fs::write(format!("{root}/operator.json"), format!("{{\"pid\":{}}}", std::process::id()));
        }
    };
    beat();
    let stop = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let answered = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let op = {
        let (root, stop, answered) = (root.clone(), stop.clone(), answered.clone());
        std::thread::spawn(move || {
            while !stop.load(Ordering::SeqCst) {
                std::thread::sleep(std::time::Duration::from_millis(100));
                beat();
                let Ok(raw) = fs::read_to_string(format!("{root}/asks/tu1.json")) else { continue };
                let Ok(v) = serde_json::from_str::<serde_json::Value>(&raw) else { continue };
                let Some(nonce) = v["nonce"].as_str() else { continue };
                let ans = format!(r#"{{"decision":"allow","nonce":"{nonce}","reason":"ok by test"}}"#);
                let _ = fs::write(format!("{root}/answers/tu1.json"), ans);
                answered.store(true, Ordering::SeqCst);
                return;
            }
        })
    };
    let check = format!(
        "{gate_bin} gate check --mode enforce --root {root} --format claude --ask --ask-timeout 20 < {}/ask.json",
        w.ws
    );
    let bin_dir = Path::new(&gate_bin).parent().unwrap().display().to_string();
    let (so, se, code) = run(&w, &env(&w, &[]), &["--workspace", &w.ws, "--allow", &bin_dir, "--", "/bin/sh", "-c", &check]);
    stop.store(true, Ordering::SeqCst);
    let _ = op.join();
    let audit = fs::read_to_string(format!("{root}/audit/s9.jsonl")).unwrap_or_default();
    assert!(code == 0 && audit.contains("allowed by operator: ok by test"), "{code} {so} {se} {audit}");
    assert!(answered.load(Ordering::SeqCst), "no ask was posted");
    assert!(!Path::new(&format!("{root}/answers/tu1.json")).exists(), "the answer was not consumed");
}

/// FN-V-7: an enforce-mode allow whose audit line cannot land is denied.
#[test]
fn fn_v7_a_read_only_audit_log_denies() {
    let w = world();
    let root = format!("{}/.opendaisugi/gate", w.home);
    fs::write(format!("{root}/envelopes/default.json"), ENVELOPE).unwrap();
    let gate_bin = std::env::var("FENCE_TEST_GATE_BIN").unwrap_or_else(|_| bin());
    let check = |mode: &str| {
        let mut c = Command::new(&gate_bin)
            .args(["gate", "check", "--mode", mode, "--root", &root, "--format", "claude"])
            .env_clear()
            .envs(env(&w, &[]))
            .stdin(Stdio::piped())
            .stderr(Stdio::piped())
            .stdout(Stdio::null())
            .spawn()
            .unwrap();
        use std::io::Write;
        c.stdin.take().unwrap().write_all(br#"{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/w/a"},"cwd":"/w"}"#).unwrap();
        let o = c.wait_with_output().unwrap();
        (o.status.code().unwrap_or(-1), String::from_utf8_lossy(&o.stderr).into_owned())
    };
    assert_eq!(check("enforce").0, 0);
    fs::set_permissions(format!("{root}/audit/s1.jsonl"), fs::Permissions::from_mode(0o444)).unwrap();
    let (code, se) = check("enforce");
    assert!(code == 2 && se.contains("cannot write its audit log"), "{code} {se}");
    assert_eq!(check("audit").0, 0);
}

/// FN-R-21: the fence, still outside its ruleset, records its run in the
/// gate root, so the resident gate binds the workspace for checkpoints. The
/// record is the harness's own pid, and nothing inside the fence writes it.
#[test]
fn fn_r21_the_fence_records_its_run() {
    let w = world();
    let root = format!("{}/.opendaisugi/gate", w.home);
    let script = format!(
        "cat {root}/fence-runs/$$.json; echo\n\
         if sh -c 'echo x > {root}/fence-runs/$$.json' 2>/dev/null; then echo wrote; else echo denied; fi\n\
         if sh -c 'echo x > {root}/fence-runs/1.json' 2>/dev/null; then echo wrote; else echo denied; fi"
    );
    let (so, se, code) = run(&w, &env(&w, &[]), &["--workspace", &w.ws, "--", "/bin/sh", "-c", &script]);
    assert_eq!(code, 0, "{se}");
    let lines: Vec<&str> = so.trim().lines().collect();
    assert!(
        lines.len() == 3 && lines[0].contains(&format!("\"workspace\":\"{}\"", w.ws)) && lines[1] == "denied" && lines[2] == "denied",
        "{so} {se}"
    );
}

/// FN-R-25 (review N-4): a foreman worker's tree run stays outside the
/// fence and only sprig runs under it: sprig cannot write the worker's gate
/// root, writes its own session dir, and its gate calls reach the resident
/// gate the fence starts on that root.
#[test]
fn fn_r25_a_workers_sprig_runs_fenced_and_cannot_write_its_gate_root() {
    let w = world();
    let work = format!("{}/work", w.base.display());
    let root = format!("{work}/gate");
    let env_file = format!("{}/env.json", w.base.display());
    let envelope = format!(
        r#"{{"id":"env_w","generated_by":"t","task":"t","permissions":{{"file_read":["{ws}/**"],"file_write":["{ws}/**"],"network":false,"shell":true,"shell_allowlist":["cat","ls"],"max_execution_time_s":60,"max_output_size_mb":20}}}}"#,
        ws = w.ws
    );
    fs::write(&env_file, &envelope).unwrap();
    let run_cmd = |stdin: &str, extra: &[(&str, &str)], args: &[&str]| -> (String, String, i32) {
        use std::io::Write;
        let mut c = Command::new(bin());
        c.args(args).current_dir(&w.ws).env_clear().envs(env(&w, extra)).stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::piped());
        let mut ch = c.spawn().unwrap();
        ch.stdin.take().unwrap().write_all(stdin.as_bytes()).unwrap();
        let out = ch.wait_with_output().unwrap();
        let se = String::from_utf8_lossy(&out.stderr).into_owned();
        for line in se.lines() {
            if let Some(rest) = line.split("started the resident gate on ").nth(1) {
                if let Some(pid) = rest.split("(pid ").nth(1).and_then(|p| p.trim_end_matches(')').parse::<i32>().ok()) {
                    w.servers.borrow_mut().push(pid);
                }
            }
        }
        (String::from_utf8_lossy(&out.stdout).into_owned(), se, out.status.code().unwrap_or(-1))
    };
    let (so, se, code) = run_cmd("", &[], &["tree", "root", &env_file, "--session", "project-x", "--data-dir", &work, "--root", &root]);
    assert_eq!(code, 0, "tree root: {so} {se}");
    let sprig = format!("{}/.bin/sprig", w.ws);
    fs::create_dir_all(format!("{}/.bin", w.ws)).unwrap();
    let script = format!(
        r#"#!/bin/sh
gate= sess=
while [ $# -gt 0 ]; do
  case "$1" in --gate-cmd) gate=$2; shift;; --session-dir) sess=$2; shift;; esac; shift
done
r() {{ if "$@" >/dev/null 2>&1; then printf ok; else printf denied; fi; }}
envw=$(r sh -c "echo x > {root}/envelopes/x.json")
disarm=$(r sh -c "echo x > {root}/DISARMED")
sessw=$(r sh -c "echo x > $sess/probe")
printf '{{"session_id":"s","tool_name":"Read","tool_input":{{"file_path":"{ws}/ok"}},"cwd":"{ws}"}}' > {ws}/p.json
if sh -c "$gate --format claude < {ws}/p.json" >/dev/null 2>&1; then gatev=allow; else gatev=deny; fi
printf '{{"answer": "env=%s disarm=%s sess=%s gate=%s", "turns": 1}}' "$envw" "$disarm" "$sessw" "$gatev"
"#,
        ws = w.ws
    );
    fs::write(&sprig, script).unwrap();
    fs::set_permissions(&sprig, std::os::unix::fs::PermissionsExt::from_mode(0o755)).unwrap();
    let fence = format!(r#"["{}","fence","--workspace","{}","--gate-root","{root}"]"#, bin(), w.ws);
    let req = format!(r#"{{"task":"t","envelope":{envelope},"tools":["Read","Bash"]}}"#);
    let (so, se, code) = run_cmd(
        &req,
        &[("DAISUGI_SPRIG", &sprig), ("DAISUGI_SPRIG_FENCE", &fence)],
        &["tree", "run", "--parent", "project-x", "--profile=default", "--data-dir", &work, "--root", &root, "-"],
    );
    assert!(code == 0 && so.contains(r#""answer": "env=denied disarm=denied sess=ok gate=allow""#), "{code}\n{so}\n{se}");
    let (_, _, code) = run_cmd("", &[], &["fence", "--workspace", &w.ws, "--gate-root", &root, "--allow", &format!("{work}:rw"), "--", "/bin/true"]);
    assert_ne!(code, 0, "--allow over the worker's gate root was not refused");
}

/// FN-R-32: the fence starts the resident gate with its captures root fixed
/// at <root>/captures, so a fenced run's allowed calls land there, written
/// by the server outside the fence.
#[test]
fn fn_r32_a_fenced_runs_captures_land() {
    let w = world();
    let root = format!("{}/.opendaisugi/gate", w.home);
    fs::write(format!("{root}/envelopes/default.json"), ENVELOPE).unwrap();
    let payload = r#"{"session_id":"s7","tool_name":"Read","tool_use_id":"tu7","tool_input":{"file_path":"/work/a"},"cwd":"/work"}"#;
    fs::write(format!("{}/call.json", w.ws), payload).unwrap();
    let gate_bin = std::env::var("FENCE_TEST_GATE_BIN").unwrap_or_else(|_| bin());
    let check = format!("{gate_bin} gate check --mode enforce --root {root} --format claude < {}/call.json", w.ws);
    let bin_dir = Path::new(&gate_bin).parent().unwrap().display().to_string();
    let (so, se, code) = run(&w, &env(&w, &[]), &["--workspace", &w.ws, "--allow", &bin_dir, "--", "/bin/sh", "-c", &check]);
    assert_eq!(code, 0, "{so} {se}");
    let n = fs::read_dir(format!("{root}/captures")).map(|d| d.count()).unwrap_or(0);
    assert!(n > 0, "no capture under {root}/captures: {se}");
}
