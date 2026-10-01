//! The tests of the Rust foreman's server half: chat mode, turns, the tool
//! sockets and the security rules that must hold the same as in Go. Each
//! security rule has a test that fails without it.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use super::foreman_chat::{ForemanChat, ForemanConfig, Snap};
use super::foreman_tools::{descends, int_arg, peer_ok, TOOLS_REFUSAL};
use super::foreman_turn::*;
use super::{Config, Server};
use crate::foreman::chat::{self as fchat, Chat};
use crate::foreman::ctx::Ctx;
use crate::foreman::turns;

static N: AtomicUsize = AtomicUsize::new(0);

fn scratch(tag: &str) -> PathBuf {
    let d = std::env::temp_dir().join(format!(
        "coppice-rs-fmt-{}-{}-{tag}",
        std::process::id(),
        N.fetch_add(1, Ordering::SeqCst)
    ));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

/// Writes an executable script.
fn script(dir: &Path, name: &str, body: &str) -> String {
    let p = dir.join(name);
    std::fs::write(&p, body).unwrap();
    std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
    p.to_string_lossy().into_owned()
}

/// A chat-mode server whose turns run fakes in dir.
fn chat_server(dir: &Path, tweak: impl FnOnce(&mut ForemanConfig)) -> Arc<Server> {
    crate::foreman::fsutil::NO_SYNC.store(true, Ordering::Relaxed);
    let mut f = ForemanConfig {
        mode: "chat".into(),
        claude_bin: format!("{}/no-claude", dir.display()),
        sprig_bin: format!("{}/no-sprig", dir.display()),
        daisugi_bin: format!("{}/no-daisugi", dir.display()),
        self_cmd: vec!["/bin/false".into(), "foreman".into()],
        ..Default::default()
    };
    tweak(&mut f);
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.join("data"),
        start_dir: dir.to_path_buf(),
        voice: Default::default(),
        foreman: f,
    });
    s.start_foreman_chat();
    s
}

fn req(line: &str) -> crate::proto::Request {
    crate::proto::decode_line(line).0
}

fn wait_for(what: &str, mut ok: impl FnMut() -> bool) {
    let end = Instant::now() + Duration::from_secs(20);
    while Instant::now() < end {
        if ok() {
            return;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    panic!("timed out waiting for {what}");
}

fn turns_done(s: &Server, n: usize) -> bool {
    turns::read_turns(&s.foreman_log_dir())
        .unwrap_or_default()
        .iter()
        .filter(|r| r.status != turns::TURN_RUNNING)
        .count()
        >= n
}

fn log_of(fc: &ForemanChat) -> Vec<(String, String)> {
    let g = fc.lock();
    let c = g.as_ref().unwrap();
    (0..c.count())
        .map(|i| {
            let m = c.message(i).unwrap();
            (m.kind, m.text)
        })
        .collect()
}

/// A fake claude that records its argv, its stdin and its system prompt in
/// rec, then answers with reply.
fn fake_claude(dir: &Path, rec: &Path, reply: &str) -> String {
    std::fs::create_dir_all(rec).unwrap();
    script(
        dir,
        "fake-claude",
        &format!(
            r#"#!/bin/sh
rec='{rec}'
n=$(ls "$rec" | grep -c argv)
for a in "$@"; do printf '%s\n' "$a"; done > "$rec/argv-$n"
cat > "$rec/stdin-$n"
while [ $# -gt 0 ]; do
  if [ "$1" = --system-prompt-file ]; then cp "$2" "$rec/system-$n"; fi
  shift
done
printf '%s\n' '{{"type":"result","is_error":false,"result":"{reply}","usage":{{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":7,"cache_creation_input_tokens":1}}}}'
"#,
            rec = rec.display()
        ),
    )
}

#[test]
fn a_talk_in_chat_mode_logs_returns_at_once_and_runs_one_turn() {
    let dir = scratch("talk");
    let rec = dir.join("rec");
    let claude = fake_claude(&dir, &rec, "  Hello. I am the foreman. ");
    let s = chat_server(&dir, |f| {
        f.claude_bin = claude;
        f.turn_model = "sonnet".into();
    });
    let r = s.talk_chat(&req(r#"{"id":"1","cmd":"floor.talk"}"#), "hello foreman");
    assert!(r.ok, "{:?}", r.error);
    let v = r.result.unwrap();
    assert_eq!(v["mode"], "chat");
    assert_eq!(v["message"], "log/0");
    assert_eq!(v["queued"].as_i64(), Some(1));
    assert_eq!(v["label"], "foreman");
    wait_for("the turn", || turns_done(&s, 1));
    let fc = s.ensure_chat();
    assert_eq!(
        log_of(&fc),
        vec![
            ("user".to_string(), "hello foreman".to_string()),
            (
                "foreman".to_string(),
                "Hello. I am the foreman.".to_string()
            ),
        ]
    );
    let recs = turns::read_turns(&s.foreman_log_dir()).unwrap();
    assert_eq!(recs.len(), 2);
    assert_eq!(
        (recs[0].status.as_str(), recs[1].status.as_str()),
        ("running", "ok")
    );
    assert_eq!(
        recs[1].usage,
        Some(crate::foreman::compactor::CallUsage {
            input: 5,
            output: 2,
            cache_read: 7,
            cache_write: 1
        })
    );
    // The argv is exact, and the system prompt went in a file that is gone.
    let argv = std::fs::read_to_string(rec.join("argv-0")).unwrap();
    let argv: Vec<&str> = argv.lines().collect();
    let sys = s.turn_dir().join("t-0.system");
    let sock = s.turn_dir().join("t-0.sock");
    let me = vec!["/bin/false".to_string(), "foreman".to_string()];
    let want = claude_argv(
        "sonnet",
        &sys,
        &mcp_config_json(&me, &sock),
        &settings_json(&me),
    );
    assert_eq!(argv, want.iter().map(String::as_str).collect::<Vec<_>>());
    assert!(!sys.exists() && !sock.exists());
    // The layout: the page and an empty snapshot in the system prompt; the
    // tail, the state and the message on stdin.
    let system = std::fs::read_to_string(rec.join("system-0")).unwrap();
    assert_eq!(system, format!("{FOREMAN_TURN_PAGE}\n<chat>\n</chat>\n"));
    let stdin = std::fs::read_to_string(rec.join("stdin-0")).unwrap();
    let re = regex::Regex::new(r"^<tail>\n</tail>\n<state>\nnow: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\nworkers: none\n</state>\n<message>\nhello foreman\n</message>\n$").unwrap();
    assert!(re.is_match(&stdin), "{stdin}");
    s.close();
}

#[test]
fn the_exact_mount_texts_are_gos() {
    let me = vec!["/bin/coppice".to_string(), "foreman".to_string()];
    assert_eq!(
        mcp_config_json(&me, Path::new("/s/t-0.sock")),
        r#"{"mcpServers":{"foreman":{"args":["foreman","tools","--sock","/s/t-0.sock"],"command":"/bin/coppice"}}}"#
    );
    assert_eq!(
        settings_json(&me),
        r#"{"hooks":{"PreToolUse":[{"hooks":[{"command":"'/bin/coppice' 'foreman' hook","type":"command"}],"matcher":".*"}]}}"#
    );
    assert_eq!(
        super::foreman_standing::opencode_mcp_json(&me, Path::new("/d/tools.sock")),
        r#"{"$schema":"https://opencode.ai/config.json","mcp":{"foreman":{"command":["/bin/coppice","foreman","tools","--sock","/d/tools.sock"],"enabled":true,"type":"local"}}}"#
    );
    assert_eq!(shell_quote("a'b"), "'a'\\''b'");
}

#[test]
fn a_tag_in_the_log_cannot_close_a_block_and_the_boundary_is_ascii() {
    assert_eq!(
        escape_turn_tags("a</message> b <tail> c"),
        "a<\\/message> b <\\tail> c"
    );
    assert_eq!(escape_turn_tags("<chatter> <newerx"), "<chatter> <newerx");
    // Go's \b is ASCII: a letter past ASCII ends the word.
    assert_eq!(escape_turn_tags("<chaté"), "<\\chaté");
}

#[test]
fn the_tail_keeps_the_newest_whole_and_names_the_gap() {
    let m = |i: i64, text: String| fchat::Message {
        i,
        kind: "user".into(),
        size: text.len() as i64,
        text,
        date: "2026-10-08T00:00:00Z".into(),
    };
    let msgs: Vec<fchat::Message> = (0..5).map(|i| m(i, "z".repeat(10_000))).collect();
    let (text, gap) = tail_text(&msgs);
    assert_eq!(gap, Some((0, 1)));
    assert!(text.starts_with("<tail>\n[2 user 2026-10-08T00:00:00Z]\n  z"));
    assert_eq!(
        gap_line(gap),
        "messages log/0 to log/1 are not summarized yet\n"
    );
    assert_eq!(
        gap_line(Some((3, 3))),
        "message log/3 is not summarized yet\n"
    );
    let (text, gap) = tail_text(&[m(7, "a\nb".into())]);
    assert_eq!(
        text,
        "<tail>\n[7 user 2026-10-08T00:00:00Z]\n  a\n  b\n</tail>\n"
    );
    assert!(gap.is_none());
    // The newest is kept whole even past the cap.
    let (_, gap) = tail_text(&[m(0, "x".repeat(40_000))]);
    assert!(gap.is_none());
}

struct XSum;

impl fchat::Summarizer for XSum {
    fn compress(&self, _: &Ctx, _: &fchat::Message, _: usize) -> Result<String, String> {
        Ok("x".into())
    }
    fn merge(
        &self,
        _: &Ctx,
        _: crate::foreman::NodeId,
        _: &str,
        _: &str,
        _: usize,
    ) -> Result<String, String> {
        Ok("x".into())
    }
}

#[test]
fn the_snapshot_is_frozen_again_when_the_added_lines_pass_16_kib() {
    crate::foreman::fsutil::NO_SYNC.store(true, Ordering::Relaxed);
    let mut c = Chat::open(fchat::Config {
        dir: scratch("snap"),
        view_max: 1 << 20,
        view_min: 1 << 19,
        ..Default::default()
    })
    .unwrap();
    let mut next = 0;
    let mut add = |c: &mut Chat, n: usize| {
        for _ in 0..n {
            let (_, e) = c.append("work", &format!("[w] line {next} {}", "y".repeat(90)));
            assert!(e.is_none());
            next += 1;
        }
        c.drain(&Ctx::background(), &XSum).unwrap();
    };
    let mut snap = Snap::default();
    let mut split = |c: &Chat| split_view(&mut snap, &c.view_text(escape_turn_tags, -1));
    add(&mut c, 20);
    let (s1, a1) = split(&c);
    assert!(!s1.is_empty() && a1.is_empty());
    add(&mut c, 10);
    let (s2, a2) = split(&c);
    assert!(s2 == s1 && !a2.is_empty() && a2.len() <= SNAPSHOT_GROW_BYTES);
    add(&mut c, 200);
    let (s3, a3) = split(&c);
    assert!(s3 != s1 && a3.is_empty() && s3.starts_with(&s1));
    add(&mut c, 5);
    let (s4, a4) = split(&c);
    assert!(s4 == s3 && !a4.is_empty());
}

// The security rules. Each test fails without its rule.

#[test]
fn the_sprig_environment_is_an_allowlist() {
    let vars = [
        ("PATH", "/usr/bin"),
        ("HOME", "/h"),
        ("LANG", "C"),
        ("LC_ALL", "C"),
        ("XDG_DATA_HOME", "/x"),
        ("TMPDIR", "/t"),
        ("ANTHROPIC_API_KEY", "sk-leak"),
        ("OPENAI_API_KEY", "sk-leak"),
        ("ANTHROPIC_BASE_URL", "http://evil"),
        ("HTTPS_PROXY", "http://proxy"),
        ("SPRIG_MODEL", "m"),
        ("SPRIG_BACKEND", "b"),
        ("CLAUDE_CONFIG_DIR", "/c"),
        ("FAKE_SCRIPT", "s"),
    ]
    .iter()
    .map(|(k, v)| (k.into(), v.into()));
    let got = sprig_env_of(vars.clone(), &[]);
    let names: Vec<String> = got
        .iter()
        .map(|(k, _)| k.to_string_lossy().into_owned())
        .collect();
    assert_eq!(
        names,
        ["PATH", "HOME", "LANG", "LC_ALL", "XDG_DATA_HOME", "TMPDIR"]
    );
    let keep = sprig_env_of(vars, &["FAKE_SCRIPT".to_string()]);
    assert_eq!(keep.len(), 7);
}

#[test]
fn the_peer_rule_refuses_what_it_cannot_prove() {
    let me = crate::sys::getuid();
    let pid = crate::sys::getpid();
    // The root is the peer itself: served.
    assert!(peer_ok(Some((pid, me, 0)), pid, me));
    // No root yet: refused.
    assert!(!peer_ok(Some((pid, me, 0)), 0, me));
    // Credentials that cannot be read: refused.
    assert!(!peer_ok(None, pid, me));
    // Another uid: refused, even from the root.
    assert!(!peer_ok(Some((pid, me + 1, 0)), pid, me));
    // A pid outside the root's tree: refused.
    let mut child = std::process::Command::new("/bin/sleep")
        .arg("30")
        .spawn()
        .unwrap();
    let cpid = child.id() as i32;
    assert!(!peer_ok(Some((pid, me, 0)), cpid, me));
    assert!(!descends(pid, cpid));
    // The child lies below this process: served.
    assert!(descends(cpid, pid));
    assert!(peer_ok(Some((cpid, me, 0)), pid, me));
    let _ = child.kill();
    let _ = child.wait();
}

fn call(path: &Path, line: &str) -> String {
    let mut c = UnixStream::connect(path).unwrap();
    c.write_all(format!("{line}\n").as_bytes()).unwrap();
    let mut out = String::new();
    BufReader::new(&c).read_line(&mut out).unwrap();
    out
}

#[test]
fn the_turn_socket_serves_its_own_process_and_refuses_another() {
    let dir = scratch("sock");
    let s = chat_server(&dir, |_| {});
    let fc = s.ensure_chat();
    assert!(fc.has_chat(), "{}", fc.problem);
    let ts = s.open_turn_socket(&fc, 7).unwrap();
    let path = ts.path().to_path_buf();
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600);
    let dmode = std::fs::metadata(path.parent().unwrap())
        .unwrap()
        .permissions()
        .mode()
        & 0o777;
    assert_eq!(dmode, 0o700);
    // Before the turn names its process, every peer is refused.
    let t0 = Instant::now();
    let r = call(&path, r#"{"tool":"status","args":{}}"#);
    assert_eq!(
        r.trim(),
        format!("{{\"ok\":false,\"error\":\"{TOOLS_REFUSAL}\"}}")
    );
    assert!(t0.elapsed() >= Duration::from_millis(1500));
    // A root this process does not descend from: refused.
    let mut child = std::process::Command::new("/bin/sleep")
        .arg("30")
        .spawn()
        .unwrap();
    ts.started(child.id() as i32);
    let r = call(&path, r#"{"tool":"status","args":{}}"#);
    assert!(r.contains("refused"), "{r}");
    let _ = child.kill();
    let _ = child.wait();
    // This process as the root: served, and each call is logged.
    ts.started(crate::sys::getpid());
    assert_eq!(
        call(&path, r#"{"tool":"status","args":{}}"#).trim(),
        r#"{"ok":true,"result":"no workers"}"#
    );
    assert_eq!(
        call(&path, r#"{"tool":"date","args":{"id":5}}"#).trim(),
        r#"{"ok":false,"error":"foreman: no message 5"}"#
    );
    assert!(call(&path, r#"{"tool":"bash","args":{}}"#)
        .contains("no tool named \\\"bash\\\". The tools are date, spawn, status, zoom"));
    assert!(call(&path, r#"{"tool":"zoom","args":{"label":"x","id":0}}"#).contains("not both"));
    let log = log_of(&fc);
    assert_eq!(
        log[0],
        ("tool".to_string(), "status({})\nno workers".to_string())
    );
    assert_eq!(
        log[1],
        (
            "tool".to_string(),
            "date({\"id\":5})\nerror: foreman: no message 5".to_string()
        )
    );
    ts.close();
    assert!(!path.exists());
    assert!(UnixStream::connect(&path).is_err());
    s.close();
}

#[test]
fn a_stale_socket_is_removed_and_anything_else_is_kept() {
    let dir = scratch("stale");
    let s = chat_server(&dir, |_| {});
    let fc = s.ensure_chat();
    let td = s.turn_dir();
    std::fs::create_dir_all(&td).unwrap();
    let p = td.join("t-3.sock");
    drop(std::os::unix::net::UnixListener::bind(&p).unwrap());
    let ts = s.open_turn_socket(&fc, 3).unwrap();
    ts.close();
    std::fs::write(td.join("t-4.sock"), "x").unwrap();
    let e = s.open_turn_socket(&fc, 4).err().unwrap();
    assert!(e.ends_with("is not a socket. Move it away"), "{e}");
    s.close();
}

#[test]
fn a_standing_socket_path_of_100_bytes_or_more_is_refused() {
    let dir = scratch("long");
    let long = dir.join("x".repeat(110));
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.join("data"),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: ForemanConfig {
            log_dir: long.to_string_lossy().into_owned(),
            ..Default::default()
        },
    });
    let e = s.ensure_standing().err().unwrap();
    assert!(
        e.contains("is too long for a socket. Use a shorter data dir"),
        "{e}"
    );
    assert!(!long.join("tools.sock").exists());
    s.close();
}

#[test]
fn whole_numbers_read_as_go_reads_them() {
    let args: serde_json::Map<String, serde_json::Value> = serde_json::from_str(
        r#"{"a":3,"b":1.0,"c":1e2,"d":1.5,"e":"3","f":1e300,"h":9007199254740992}"#,
    )
    .unwrap();
    assert_eq!(int_arg(&args, "a"), Ok(3));
    assert_eq!(int_arg(&args, "b"), Ok(1));
    assert_eq!(int_arg(&args, "c"), Ok(100));
    assert_eq!(int_arg(&args, "d"), Err("d must be a whole number".into()));
    assert_eq!(int_arg(&args, "e"), Err("e must be a whole number".into()));
    assert_eq!(int_arg(&args, "f"), Err("f must be a whole number".into()));
    assert_eq!(int_arg(&args, "h"), Ok(9007199254740992));
    assert_eq!(int_arg(&args, "g"), Err("g is missing".into()));
}

/// A fake daisugi whose bundle names one profile with a key, and a fake
/// sprig that records its argv, its environment and descriptor 3.
fn sprig_fakes(dir: &Path, rec: &Path, profile: &str) -> (String, String) {
    std::fs::create_dir_all(rec).unwrap();
    let daisugi = script(
        dir,
        "fake-daisugi",
        &format!(
            "#!/bin/sh\n[ \"$1 $2 $3 $4\" = \"agent-profiles bundle --fd 3\" ] || exit 2\nprintf '%s' '{profile}' >&3\n"
        ),
    );
    let sprig = script(
        dir,
        "fake-sprig",
        &format!(
            r#"#!/bin/sh
rec='{rec}'
for a in "$@"; do printf '%s\n' "$a"; done > "$rec/argv"
env > "$rec/env"
case " $* " in *" --key-fd 3 "*) cat <&3 > "$rec/fd3";; esac
cat > "$rec/stdin"
printf '%s\n' '{{"answer":"from sprig","usage":{{"input_tokens":3}}}}'
"#,
            rec = rec.display()
        ),
    );
    (daisugi, sprig)
}

#[test]
fn the_key_reaches_sprig_on_descriptor_3_only() {
    let dir = scratch("key");
    let rec = dir.join("rec");
    let (daisugi, sprig) = sprig_fakes(
        &dir,
        &rec,
        r#"{"desk":{"wire":"anthropic","base_url":null,"model":"m-profile","key":"sk-secret-123"}}"#,
    );
    let s = chat_server(&dir, |f| {
        f.daisugi_bin = daisugi;
        f.sprig_bin = sprig;
        f.turn_profile = "desk".into();
    });
    let r = s.talk_chat(&req(r#"{"id":"1"}"#), "hello");
    assert!(r.ok);
    wait_for("the turn", || turns_done(&s, 1));
    let fc = s.ensure_chat();
    assert_eq!(
        log_of(&fc)[1],
        ("foreman".to_string(), "from sprig".to_string())
    );
    let argv = std::fs::read_to_string(rec.join("argv")).unwrap();
    let env = std::fs::read_to_string(rec.join("env")).unwrap();
    assert!(!argv.contains("sk-secret-123"), "the key is in argv");
    assert!(
        !env.contains("sk-secret-123"),
        "the key is in the environment"
    );
    assert_eq!(
        std::fs::read_to_string(rec.join("fd3")).unwrap(),
        "sk-secret-123"
    );
    let argv: Vec<&str> = argv.lines().collect();
    let sys = s.turn_dir().join("t-0.system");
    let sock = s.turn_dir().join("t-0.sock");
    let me = vec!["/bin/false".to_string(), "foreman".to_string()];
    let mcp = mcp_config_json(&me, &sock);
    let want = vec![
        "--wire",
        "anthropic",
        "--base-url",
        "https://api.anthropic.com",
        "--model",
        "m-profile",
        "--key-fd",
        "3",
        "--tools",
        "none",
        "--mcp-config",
        mcp.as_str(),
        "--system-file",
        sys.to_str().unwrap(),
        "--json",
        "--max-turns",
        "8",
        "-",
    ];
    assert_eq!(argv, want);
    // Nothing of the key in any file of the log dir.
    fn walk(d: &Path, out: &mut String) {
        for e in std::fs::read_dir(d).into_iter().flatten().flatten() {
            let p = e.path();
            if p.is_dir() {
                walk(&p, out);
            } else if let Ok(t) = std::fs::read_to_string(&p) {
                out.push_str(&t);
            }
        }
    }
    let mut all = String::new();
    walk(&s.foreman_log_dir(), &mut all);
    assert!(!all.contains("sk-secret-123"));
    s.close();
}

#[test]
fn a_keyless_profile_gets_no_key_fd_and_turn_model_beats_the_profile() {
    let dir = scratch("nokey");
    let rec = dir.join("rec");
    let (daisugi, sprig) = sprig_fakes(
        &dir,
        &rec,
        r#"{"local":{"wire":"openai","base_url":"http://127.0.0.1:9/v1","model":"m1","key":null}}"#,
    );
    let s = chat_server(&dir, |f| {
        f.daisugi_bin = daisugi;
        f.sprig_bin = sprig;
        f.turn_profile = "local".into();
        f.turn_model = "m2".into();
    });
    assert!(s.talk_chat(&req(r#"{"id":"1"}"#), "hi").ok);
    wait_for("the turn", || turns_done(&s, 1));
    let argv = std::fs::read_to_string(rec.join("argv")).unwrap();
    assert!(
        argv.starts_with(
            "--wire\nopenai\n--base-url\nhttp://127.0.0.1:9/v1\n--model\nm2\n--tools\n"
        ),
        "{argv}"
    );
    assert!(!rec.join("fd3").exists());
    s.close();
}

#[test]
fn a_missing_profile_and_a_claude_code_profile_fail_the_turn_with_one_note() {
    for (profile, want) in [
        ("gone", "The foreman's turn failed: the turn profile \"gone\" is not in the operator's agent profiles (a key variable it names may be unset). Say it again to try again."),
        ("cc", "The foreman's turn failed: the turn profile \"cc\" uses the claude-code wire. Leave turn_profile empty to run the turn on Claude Code. Say it again to try again."),
    ] {
        let dir = scratch("noprof");
        let rec = dir.join("rec");
        let (daisugi, sprig) = sprig_fakes(&dir, &rec, r#"{"cc":{"wire":"claude-code","base_url":null,"model":null,"key":null}}"#);
        let s = chat_server(&dir, |f| {
            f.daisugi_bin = daisugi;
            f.sprig_bin = sprig;
            f.turn_profile = profile.into();
        });
        assert!(s.talk_chat(&req(r#"{"id":"1"}"#), "hi").ok);
        wait_for("the turn", || turns_done(&s, 1));
        let fc = s.ensure_chat();
        assert_eq!(log_of(&fc)[1], ("note".to_string(), want.to_string()));
        assert!(!rec.join("argv").exists(), "sprig ran");
        s.close();
    }
}

#[test]
fn a_failed_turn_adds_one_note_and_is_not_retried() {
    let dir = scratch("fail");
    let claude = script(
        &dir,
        "fake-claude",
        "#!/bin/sh\nprintf 'first\\nthe last line names the fault\\n' >&2\nexit 1\n",
    );
    let s = chat_server(&dir, |f| f.claude_bin = claude);
    assert!(s.talk_chat(&req(r#"{"id":"1"}"#), "hi").ok);
    wait_for("the turn", || turns_done(&s, 1));
    std::thread::sleep(Duration::from_millis(200));
    let fc = s.ensure_chat();
    assert_eq!(
        log_of(&fc),
        vec![
            ("user".to_string(), "hi".to_string()),
            (
                "note".to_string(),
                "The foreman's turn failed: claude ended with an error: the last line names the fault. Say it again to try again.".to_string()
            ),
        ]
    );
    let recs = turns::read_turns(&s.foreman_log_dir()).unwrap();
    assert_eq!(recs.len(), 2);
    assert_eq!(recs[1].status, "failed");
    s.close();
}

#[test]
fn a_turn_that_times_out_is_killed_and_noted() {
    let dir = scratch("slow");
    let claude = script(&dir, "fake-claude", "#!/bin/sh\nexec sleep 30\n");
    let s = chat_server(&dir, |f| {
        f.claude_bin = claude;
        f.turn_timeout = Duration::from_millis(300);
    });
    assert!(s.talk_chat(&req(r#"{"id":"1"}"#), "hi").ok);
    wait_for("the turn", || turns_done(&s, 1));
    let fc = s.ensure_chat();
    assert_eq!(
        log_of(&fc)[1].1,
        "The foreman's turn timed out: it ran longer than 300ms and was stopped. Say it again to try again."
    );
    assert_eq!(
        turns::read_turns(&s.foreman_log_dir()).unwrap()[1].status,
        "timeout"
    );
    s.close();
}

#[test]
fn a_dangling_turn_is_closed_and_the_unanswered_line_is_noted_once() {
    let dir = scratch("dangle");
    let log = dir.join("data").join("foreman");
    {
        crate::foreman::fsutil::NO_SYNC.store(true, Ordering::Relaxed);
        let mut c = Chat::open(fchat::Config {
            dir: log.clone(),
            ..Default::default()
        })
        .unwrap();
        assert!(c.append("user", "are you there").1.is_none());
        turns::append_turn(
            &log,
            &turns::TurnRecord {
                msg: 0,
                started: "2026-10-08T00:00:00Z".into(),
                status: "running".into(),
                ..Default::default()
            },
        )
        .unwrap();
    }
    for _ in 0..2 {
        let s = chat_server(&dir, |_| {});
        s.close();
    }
    let s = chat_server(&dir, |_| {});
    let fc = s.ensure_chat();
    assert!(fc.has_chat(), "{}", fc.problem);
    let notes: Vec<_> = log_of(&fc)
        .into_iter()
        .filter(|(k, _)| k == "note")
        .collect();
    assert_eq!(
        notes,
        vec![(
            "note".to_string(),
            "The foreman did not answer log/0: coppice stopped first. Say it again.".to_string()
        )]
    );
    let recs = turns::read_turns(&log).unwrap();
    assert_eq!(recs.len(), 2);
    assert_eq!(recs[1].status, "failed");
    s.close();
}

#[test]
fn opening_the_chat_sweeps_only_old_files_of_turns_that_never_ended() {
    let dir = scratch("sweep");
    let td = dir.join("data").join("foreman").join("turns");
    std::fs::create_dir_all(&td).unwrap();
    for n in ["t-1.system", "t-1.view", "t-2.sock", "keep.txt"] {
        std::fs::write(td.join(n), "x").unwrap();
    }
    std::fs::create_dir_all(td.join("t-9.system")).unwrap();
    let old = std::time::SystemTime::now() - Duration::from_secs(3600);
    for n in ["t-1.system", "t-1.view", "t-2.sock", "keep.txt"] {
        std::fs::File::options()
            .write(true)
            .open(td.join(n))
            .unwrap()
            .set_modified(old)
            .unwrap();
    }
    std::fs::write(td.join("t-3.system"), "new").unwrap();
    let s = chat_server(&dir, |_| {});
    let mut left: Vec<String> = std::fs::read_dir(&td)
        .unwrap()
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .collect();
    left.sort();
    assert_eq!(left, ["keep.txt", "t-3.system", "t-9.system"]);
    s.close();
}

#[test]
fn a_talk_when_the_log_cannot_open_is_the_plain_error_and_the_floor_starts() {
    let dir = scratch("noopen");
    std::fs::create_dir_all(dir.join("data")).unwrap();
    // A file where the log dir should be.
    std::fs::write(dir.join("data").join("foreman"), "not a dir").unwrap();
    let s = chat_server(&dir, |_| {});
    let r = s.talk_chat(&req(r#"{"id":"1"}"#), "hi");
    assert!(!r.ok);
    let (_, msg) = r.error.unwrap();
    assert!(msg.starts_with("The foreman's chat in "), "{msg}");
    assert!(msg.ends_with("Fix that, then start coppice again. The rest of the floor works."));
    s.close();
}

#[test]
fn floor_chat_reads_the_log_and_maps_each_kind() {
    let dir = scratch("f14");
    let s = chat_server(&dir, |_| {});
    let fc = s.ensure_chat();
    for (k, t) in [
        ("user", "first"),
        ("foreman", "reply"),
        ("work", "[w-1] report\ndone"),
        ("note", "a note"),
        ("past", "an imported line"),
        ("tool", "zoom({\"id\":0,\"n\":1})\nfirst"),
    ] {
        s.chat_append(&fc, k, t).unwrap();
    }
    turns::append_turn(
        &fc.dir,
        &turns::TurnRecord {
            msg: 0,
            started: "2026-10-08T00:00:00Z".into(),
            status: "running".into(),
            ..Default::default()
        },
    )
    .unwrap();
    let r = s.floor_chat_log(&req(r#"{"id":"1","cmd":"floor.chat"}"#));
    let v = r.result.unwrap();
    assert_eq!(v["source"], "log");
    assert_eq!(v["more"], false);
    let rows: Vec<(String, String, String)> = v["messages"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| {
            (
                m["id"].as_str().unwrap().to_string(),
                m["role"].as_str().unwrap().to_string(),
                m.get("tool")
                    .and_then(|t| t.as_str())
                    .unwrap_or("")
                    .to_string(),
            )
        })
        .collect();
    let w = |a: &str, b: &str, c: &str| (a.to_string(), b.to_string(), c.to_string());
    assert_eq!(
        rows,
        vec![
            w("log/0", "owner", ""),
            w("log/1", "agent", ""),
            w("log/2", "work", ""),
            w("log/3", "note", ""),
            w("log/5", "agent", "zoom"),
        ]
    );
    assert_eq!(v["messages"][0]["read"], true);
    let r = s.floor_chat_log(&req(
        r#"{"id":"1","cmd":"floor.chat","past":true,"since":"log/3"}"#,
    ));
    let v = r.result.unwrap();
    let ids: Vec<&str> = v["messages"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, ["log/4", "log/5"]);
    s.close();
}

// Fix round 1: the ports of the Go tests the review named, and a check of
// the descriptors and the environment a sprig turn gets.

/// A pane-mode server in dir.
fn pane_server(dir: &Path) -> Arc<Server> {
    Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.join("data"),
        start_dir: dir.to_path_buf(),
        voice: Default::default(),
        foreman: ForemanConfig::default(),
    })
}

/// A pty pane that runs sh -c script in dir; its id.
fn pty_pane(s: &Arc<Server>, dir: &Path, script: &str) -> String {
    let line = crate::gojson::marshal(&serde_json::json!({
        "id": "c", "cmd": "pane.create", "kind": "pty", "cwd": dir.to_string_lossy(),
        "cmd_argv": ["sh", "-c", script], "cols": 80, "rows": 24,
    }));
    let r = s.pane_create(None, &req(&line));
    assert!(r.ok, "{:?}", r.error);
    r.result.unwrap()["pane"].as_str().unwrap().to_string()
}

/// Names pane id the floor's foreman, as start_foreman does.
fn track(s: &Arc<Server>, id: &str) {
    let mut t = s.talk_lock();
    s.set_foreman_pane(&mut t, id);
}

#[test]
fn the_default_profile_is_the_claude_runner() {
    let dir = scratch("defprof");
    let rec = dir.join("rec");
    let claude = fake_claude(&dir, &rec, "from claude");
    let marker = dir.join("daisugi-ran");
    let daisugi = script(
        &dir,
        "fake-daisugi",
        &format!("#!/bin/sh\ntouch '{}'\nexit 2\n", marker.display()),
    );
    let s = chat_server(&dir, |f| {
        f.claude_bin = claude;
        f.daisugi_bin = daisugi;
        f.turn_profile = "default".into();
    });
    assert!(s.talk_chat(&req(r#"{"id":"1"}"#), "hello").ok);
    wait_for("the turn", || turns_done(&s, 1));
    let fc = s.ensure_chat();
    assert_eq!(
        log_of(&fc)[1],
        ("foreman".to_string(), "from claude".to_string())
    );
    assert!(rec.join("argv-0").exists(), "claude did not run");
    assert!(
        !marker.exists(),
        "the default profile read the operator's bundle"
    );
    s.close();
}

/// A dialer that waits for each go file D/go<n>, asks the standing socket
/// for status, and writes the answer to D/out<n>. It runs in a session of
/// its own, so it outlives the pane that started it. It writes its pid to
/// D/pid, and it ends by itself 30 s after it started, whatever the test
/// does.
fn dialer(dir: &Path) -> String {
    script(
        dir,
        "dialer.py",
        r#"#!/usr/bin/python3
import os, socket, sys, time
sock, d = sys.argv[1], sys.argv[2]
with open(os.path.join(d, "pid"), "w") as f:
    f.write(str(os.getpid()))
end = time.time() + 30
for n in (1, 2, 3):
    go = os.path.join(d, "go%d" % n)
    while not os.path.exists(go):
        if time.time() > end:
            sys.exit(0)
        time.sleep(0.05)
    c = socket.socket(socket.AF_UNIX)
    c.connect(sock)
    c.sendall(b'{"tool":"status","args":{}}\n')
    out = b""
    while not out.endswith(b"\n"):
        b = c.recv(65536)
        if not b:
            break
        out += b
    c.close()
    with open(os.path.join(d, "out%d.tmp" % n), "wb") as f:
        f.write(out)
    os.rename(os.path.join(d, "out%d.tmp" % n), os.path.join(d, "out%d" % n))
"#,
    )
}

/// Ends the dialer when the test ends, by any path, a panic too: it lets
/// every wait go and kills the dialer's process group (its own session).
struct DialerGuard(PathBuf);

impl Drop for DialerGuard {
    fn drop(&mut self) {
        for n in 1..=3 {
            let _ = std::fs::write(self.0.join(format!("go{n}")), "go");
        }
        if let Some(pid) = std::fs::read_to_string(self.0.join("pid"))
            .ok()
            .and_then(|p| p.trim().parse::<i32>().ok())
            .filter(|p| *p > 1)
        {
            // SAFETY: kill takes plain integers.
            unsafe { libc::kill(-pid, libc::SIGKILL) };
        }
    }
}

fn ask(d: &Path, n: u32) -> String {
    std::fs::write(d.join(format!("go{n}")), "go").unwrap();
    let out = d.join(format!("out{n}"));
    wait_for("the dialer", || out.exists());
    std::fs::read_to_string(out).unwrap()
}

/// The standing socket reads its root at each call: a child of foreman A
/// is served while A is the foreman, and refused once the floor's foreman
/// is B, while A still runs (the step that tells a root read once from a
/// root read each call) and after A ended.
#[test]
fn a_child_of_an_earlier_foreman_is_refused() {
    let dir = scratch("earlier");
    let d = dir.join("d");
    std::fs::create_dir_all(&d).unwrap();
    let s = pane_server(&dir);
    let sock = s.standing_sock_path();
    let dial = dialer(&dir);
    let _guard = DialerGuard(d.clone());
    let a = pty_pane(
        &s,
        &dir,
        &format!(
            "setsid {dial} '{}' '{}' & wait",
            sock.display(),
            d.display()
        ),
    );
    track(&s, &a);
    assert_eq!(s.ensure_standing().unwrap(), sock);
    assert!(s.pane_foreman_pid() > 1);
    let served = ask(&d, 1);
    assert_eq!(served.trim(), r#"{"ok":true,"result":"no workers"}"#);
    // The floor's foreman is now B. A and its child still run.
    let b = pty_pane(&s, &dir, "exec sleep 60");
    track(&s, &b);
    wait_for("B to run", || s.pane_foreman_pid() > 1);
    let r = ask(&d, 2);
    assert!(
        r.contains("refused"),
        "a child of the earlier foreman was served: {r}"
    );
    // A ends; its child lives on, a stranger to B's tree.
    if let Some(lp) = s.live(&a) {
        if let Some(p) = &lp.pty {
            p.close();
        }
    }
    let r = ask(&d, 3);
    assert!(r.contains("refused"), "{r}");
    s.close();
}

#[test]
fn a_resumed_foreman_gets_the_socket() {
    let dir = scratch("resumed");
    let s = pane_server(&dir);
    let old = pty_pane(&s, &dir, "exec sleep 60");
    track(&s, &old);
    s.ensure_standing().unwrap();
    // A server restart: the socket is gone, the tracked foreman is not.
    s.close_standing();
    s.stand.lock().unwrap().shut = false;
    let sock = s.standing_sock_path();
    assert!(
        std::fs::symlink_metadata(&sock).is_err(),
        "the socket outlived its close"
    );
    let new = pty_pane(&s, &dir, "exec sleep 60");
    {
        let mut t = s.talk_lock();
        s.follow_foreman_locked(&mut t, &old, &new, true);
    }
    use std::os::unix::fs::FileTypeExt;
    let m = std::fs::symlink_metadata(&sock).expect("the resumed foreman has no socket");
    assert!(m.file_type().is_socket());
    s.close();
}

#[test]
fn a_report_queue_has_a_cap_of_its_own() {
    let dir = scratch("reports");
    let s = pane_server(&dir);
    let id = pty_pane(&s, &dir, "exec sleep 60");
    track(&s, &id);
    // The foreman waits on a question, so nothing is typed while the test
    // looks at the queue.
    let now = Server::now_seconds();
    s.states.apply(
        &id,
        crate::state::Event {
            v: 1,
            ts: now,
            session_id: id.clone(),
            harness: "sh".into(),
            pane: Some(id.clone()),
            state: crate::state::STATE_BLOCKED.into(),
            source: crate::state::SRC_OPERATOR.into(),
            ..Default::default()
        },
        now,
    );
    let max = super::foreman_standing::MAX_REPORTS;
    for i in 0..max + 5 {
        s.report_to_pane(&format!("[w-{i}] report"));
    }
    {
        let t = s.talk_lock();
        assert_eq!(t.reports.len(), max);
        assert_eq!(t.reports.front().map(String::as_str), Some("[w-5] report"));
        assert!(
            t.lines.is_empty(),
            "a report took a place in the owner's queue"
        );
    }
    let drops = s
        .notes
        .lock()
        .unwrap()
        .iter()
        .filter(|n| {
            n["text"]
                .as_str()
                .is_some_and(|t| t.starts_with("The foreman has too many worker reports waiting."))
        })
        .count();
    assert_eq!(drops, 5);
    s.close();
}

/// A fake sprig in Python that records the descriptors it got open, the
/// names of its variables, and whether the key reached it on descriptor 3
/// and nowhere else. It is not a shell script: a shell adds variables of
/// its own.
fn fd_sprig(dir: &Path, rec: &Path) -> String {
    std::fs::create_dir_all(rec).unwrap();
    script(
        dir,
        "fake-sprig",
        &format!(
            r#"#!/usr/bin/python3 -IS
import json, os, sys
fds = []
for fd in range(256):
    try:
        os.fstat(fd)
        fds.append(fd)
    except OSError:
        pass
key = os.read(3, 4096).decode() if 3 in fds else ""
sys.stdin.read()
rec = {{"fds": fds, "env": sorted(os.environ), "key": key,
       "key_in_env": any(key and key in v for v in os.environ.values()),
       "key_in_argv": any(key and key in a for a in sys.argv)}}
with open(os.path.join({rec:?}, "r-%d.json" % os.getpid()), "w") as f:
    json.dump(rec, f)
print(json.dumps({{"answer": "ok", "usage": {{}}}}))
"#,
            rec = rec.to_string_lossy()
        ),
    )
}

#[test]
fn two_sprig_turns_at_once_each_get_descriptors_0_to_3_and_the_allowlist_only() {
    let mut servers = Vec::new();
    let mut recs = Vec::new();
    for k in 0..2 {
        let dir = scratch(&format!("fds{k}"));
        let rec = dir.join("rec");
        let sprig = fd_sprig(&dir, &rec);
        let daisugi = script(
            &dir,
            "fake-daisugi",
            &format!(
                "#!/bin/sh\nprintf '%s' '{{\"desk\":{{\"wire\":\"openai\",\"base_url\":\"http://127.0.0.1:9/v1\",\"model\":\"m\",\"key\":\"sk-turn-{k}-secret\"}}}}' >&3\n"
            ),
        );
        servers.push(chat_server(&dir, |f| {
            f.sprig_bin = sprig;
            f.daisugi_bin = daisugi;
            f.turn_profile = "desk".into();
        }));
        recs.push(rec);
    }
    // Turns on both servers at once: a pipe of one turn that leaked into
    // another sprig would show as a descriptor past 3.
    for _ in 0..3 {
        let hs: Vec<_> = servers
            .iter()
            .map(|s| {
                let s = s.clone();
                std::thread::spawn(move || assert!(s.talk_chat(&req(r#"{"id":"1"}"#), "go").ok))
            })
            .collect();
        for h in hs {
            h.join().unwrap();
        }
    }
    for s in &servers {
        wait_for("the turns", || turns_done(s, 3));
    }
    let allowed = |n: &str| {
        [
            "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM", "TMPDIR",
        ]
        .contains(&n)
            || n.starts_with("LC_")
            || n.starts_with("XDG_")
    };
    for (k, rec) in recs.iter().enumerate() {
        let files: Vec<_> = std::fs::read_dir(rec).unwrap().flatten().collect();
        assert_eq!(files.len(), 3);
        for f in files {
            let v: serde_json::Value =
                serde_json::from_str(&std::fs::read_to_string(f.path()).unwrap()).unwrap();
            assert_eq!(v["fds"], serde_json::json!([0, 1, 2, 3]), "{v}");
            assert_eq!(v["key"], format!("sk-turn-{k}-secret"));
            assert_eq!(v["key_in_env"], false);
            assert_eq!(v["key_in_argv"], false);
            for n in v["env"].as_array().unwrap() {
                assert!(allowed(n.as_str().unwrap()), "sprig got {n}");
            }
        }
    }
    for s in servers {
        s.close();
    }
}

#[test]
fn a_long_turn_socket_path_is_refused() {
    let dir = scratch("longturn");
    let long = dir.join("x".repeat(110));
    let s = chat_server(&dir, |f| f.log_dir = long.to_string_lossy().into_owned());
    let fc = s.ensure_chat();
    assert!(fc.has_chat(), "{}", fc.problem);
    let e = s.open_turn_socket(&fc, 1).err().unwrap();
    assert!(
        e.contains("is too long for a socket. Use a shorter data dir"),
        "{e}"
    );
    assert!(
        !long.join("turns").exists(),
        "the turn dir was made for a socket that cannot open"
    );
    s.close();
}
