//! The live feed: the Rust twin of Go's live_test.go and follow_test.go.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use super::chat::*;
use super::chat_tests::{temp_dir, Fake};
use super::cli::{run, CliEnv};
use super::ctx::Ctx;
use super::live::*;

fn line(typ: &str, uuid: &str, ts: &str, text: &str) -> String {
    format!(
        "{{\"type\":\"{typ}\",\"uuid\":\"{uuid}\",\"sessionId\":\"sess1\",\"cwd\":\"/work/proj\",\"timestamp\":\"{ts}\",\"message\":{{\"role\":\"{typ}\",\"content\":[{{\"type\":\"text\",\"text\":\"{text}\"}}]}}}}"
    )
}

fn write_tr(p: &Path, lines: &[String]) {
    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
    std::fs::write(p, lines.join("\n") + "\n").unwrap();
}

fn cfg(dir: &Path) -> Config {
    Config { dir: dir.to_path_buf(), ..Default::default() }
}

fn now_text() -> String {
    date_text(SystemTime::now())
}

#[test]
fn ingest_claude_is_live_idempotent_and_keeps_the_source() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let tr = root.join("t").join("sess1.jsonl");
    write_tr(&tr, &[line("user", "u1", "2026-11-05T10:00:00Z", "where is the config"), line("assistant", "a1", "2026-11-05T10:00:05Z", "in /etc/x")]);
    let mut c = Chat::open(cfg(&dir)).unwrap();
    assert_eq!(c.ingest_claude(&tr, &[]).unwrap(), 2);
    write_tr(
        &tr,
        &[
            line("user", "u1", "2026-11-05T10:00:00Z", "where is the config"),
            line("assistant", "a1", "2026-11-05T10:00:05Z", "in /etc/x"),
            line("user", "u2", "2026-11-05T10:01:00Z", "thanks"),
        ],
    );
    assert_eq!(c.ingest_claude(&tr, &[]).unwrap(), 1);
    assert_eq!(c.count(), 3);
    let m = c.message(0).unwrap();
    assert_eq!((m.kind.as_str(), m.src.as_str(), m.chat.as_str(), m.who.as_str()), ("user", "claude-code", "sess1", "owner"));
    assert_eq!(c.message(1).unwrap().kind, "steward");
    c.close();
    let mut c2 = Chat::open(cfg(&dir)).unwrap();
    assert_eq!(c2.ingest_claude(&tr, &[]).unwrap(), 0);
    assert_eq!(c2.count(), 3);
    c2.close();
}

#[test]
fn the_hook_prints_the_view_at_session_start_and_never_fails() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let tr = root.join("s.jsonl");
    write_tr(&tr, &[line("user", "u1", "2026-11-05T10:00:00Z", "remember the blue door")]);
    let ev = |e: &str, t: &str| HookEvent { event: e.into(), transcript_path: t.into(), ..Default::default() };
    assert_eq!(run_hook(&dir, &ev("SessionStart", ""), &[], None), "");
    assert_eq!(run_hook(&dir, &ev("Stop", tr.to_str().unwrap()), &[], None), "");
    let out = run_hook(&dir, &ev("SessionStart", ""), &[], None);
    assert!(out.contains("additionalContext") && out.contains("blue door"), "{out}");
    assert_eq!(run_hook(&dir, &ev("Stop", "/nonexistent/x.jsonl"), &[], None), "");
    let log = std::fs::read_to_string(dir.join(HOOK_LOG_NAME)).unwrap();
    assert!(log.contains("Stop"), "{log}");
    assert_eq!(read_hook_event("not json").event, "");
}

#[test]
fn the_view_tool_shows_the_view_and_the_tail() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let mut c = Chat::open(cfg(&dir)).unwrap();
    c.append(KIND_USER, "the blue door code is 4417");
    c.close();
    let ro = Chat::open_read_only(&dir).unwrap();
    let t = super::mcp::tool_view(&ro, &Default::default()).unwrap();
    assert!(t.contains("4417"), "{t}");
    assert!(super::mcp::read_tools().len() == 4);
}

struct FakeSource(Vec<Live>);

impl Source for FakeSource {
    fn name(&self) -> String {
        "fake".into()
    }
    fn poll(&mut self, c: &mut Chat, _: Option<SystemTime>) -> Result<usize, String> {
        let mut n = 0;
        for m in &self.0 {
            if c.append_live_msg(m)? {
                n += 1;
            }
        }
        Ok(n)
    }
}

fn big() -> String {
    "word ".repeat(300)
}

#[test]
fn follow_logs_both_sources_once_and_builds_nodes_with_a_profile() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let proj = root.join("projects").join("-work-proj");
    write_tr(&proj.join("sess1.jsonl"), &[line("user", "u1", &now_text(), &big()), line("assistant", "a1", &now_text(), "ok")]);
    let mut log: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.clone(), &mut log);
    o.summarizer = Some(Box::new(Fake::default()));
    o.sources.push(Box::new(ClaudeSource::new(root.join("projects"))));
    o.sources.push(Box::new(FakeSource(vec![Live {
        kind: KIND_USER.into(),
        text: "hermes user alice in mattermost: hi".into(),
        at: None,
        act: "hermes:p:s:1".into(),
        meta: Meta { src: "hermes:p".into(), chat: "s".into(), who: "alice".into() },
    }])));
    let ctx = Ctx::background();
    let (added, _, _) = follow_once(&ctx, &mut o).unwrap();
    assert_eq!(added, 3);
    for _ in 0..5 {
        let (added, _, _) = follow_once(&ctx, &mut o).unwrap();
        assert_eq!(added, 0);
    }
    drop(o);
    let c = Chat::open_read_only(&dir).unwrap();
    assert_eq!((c.count(), c.pending()), (3, 0));
    let m = c.message(2).unwrap();
    assert_eq!((m.who.as_str(), m.src.as_str()), ("alice", "hermes:p"));
}

#[test]
fn follow_with_no_profile_only_logs_and_says_once() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    write_tr(&root.join("projects").join("p").join("s.jsonl"), &[line("user", "u1", &now_text(), &big())]);
    let mut log: Vec<u8> = Vec::new();
    {
        let mut o = FollowOpts::new(dir.clone(), &mut log);
        o.sources.push(Box::new(ClaudeSource::new(root.join("projects"))));
        let ctx = Ctx::background();
        for _ in 0..3 {
            follow_once(&ctx, &mut o).unwrap();
        }
    }
    let text = String::from_utf8(log).unwrap();
    assert_eq!(text.matches("wait for a profile").count(), 1, "{text}");
    let c = Chat::open_read_only(&dir).unwrap();
    assert!(c.count() == 1 && c.pending() > 0);
}

#[test]
fn an_outage_delays_nodes_and_never_clips() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    write_tr(&root.join("projects").join("p").join("s.jsonl"), &[line("user", "u1", &now_text(), &big())]);
    let fake = Arc::new(Fake::default());
    fake.fail.store(true, std::sync::atomic::Ordering::SeqCst);
    struct Shared(Arc<Fake>);
    impl Summarizer for Shared {
        fn compress(&self, c: &Ctx, m: &Message, l: usize) -> Result<String, String> {
            self.0.compress(c, m, l)
        }
        fn merge(&self, c: &Ctx, id: super::order::NodeId, a: &str, b: &str, l: usize) -> Result<String, String> {
            self.0.merge(c, id, a, b, l)
        }
    }
    let clock = Arc::new(std::sync::Mutex::new(SystemTime::now()));
    let ck = clock.clone();
    let now: Clock = Arc::new(move || *ck.lock().unwrap());
    let mut log: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.clone(), &mut log);
    o.now = Some(now);
    o.summarizer = Some(Box::new(Shared(fake.clone())));
    o.sources.push(Box::new(ClaudeSource::new(root.join("projects"))));
    let ctx = Ctx::background();
    let (_, _, more) = follow_once(&ctx, &mut o).unwrap();
    assert!(more);
    let before = fake.compress.load(std::sync::atomic::Ordering::SeqCst);
    fake.fail.store(false, std::sync::atomic::Ordering::SeqCst);
    follow_once(&ctx, &mut o).unwrap();
    assert_eq!(fake.compress.load(std::sync::atomic::Ordering::SeqCst), before, "called inside the back-off");
    *clock.lock().unwrap() += Duration::from_secs(360);
    for _ in 0..4 {
        follow_once(&ctx, &mut o).unwrap();
    }
    drop(o);
    let c = Chat::open_read_only(&dir).unwrap();
    assert_eq!((c.pending(), c.clipped_count()), (0, 0));
}

#[test]
fn two_appenders_share_the_store() {
    let root = temp_dir();
    let dir: PathBuf = root.join("pando").join("work");
    let mut c = Chat::open(cfg(&dir)).unwrap();
    let h = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(300));
        c.close();
    });
    let d2 = dir.clone();
    let mut c2 = open_wait(move || cfg(&d2), Duration::from_secs(5)).unwrap();
    c2.close();
    h.join().unwrap();
}

fn cli_run(data: &Path, argv: &[&str], stdin: &str) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(stdin.as_bytes().to_vec());
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: data.join("nohome"),
            new_summarizer: None,
            progress: None,
            now: None,
            embed: |_| vec![0.0; 4],
            hermes_source: None,
            getenv: None,
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

#[test]
fn the_profile_verb_sets_shows_and_clears() {
    let data = temp_dir();
    let d = data.to_str().unwrap();
    assert!(cli_run(&data, &["profile", "--data-dir", d], "").1.contains("none"));
    assert_eq!(cli_run(&data, &["profile", "pando-g12", "--data-dir", d], "").0, 0);
    assert_eq!(cli_run(&data, &["profile", "--data-dir", d], "").1.trim(), "pando-g12");
    assert_eq!(read_profile(&data, Some(&|_| "from-env".to_string())), "from-env");
    cli_run(&data, &["profile", "none", "--data-dir", d], "");
    assert_eq!(read_profile(&data, None), "");
}

#[test]
fn the_context_and_hook_verbs_work_and_a_missing_store_hook_exits_zero() {
    let data = temp_dir();
    let d = data.to_str().unwrap();
    let (code, out, _) = cli_run(&data, &["hook", "--data-dir", d], "{\"hook_event_name\":\"SessionStart\"}");
    assert_eq!((code, out.as_str()), (0, ""));
    let dir = data.join("pando").join("home");
    let mut c = Chat::open(cfg(&dir)).unwrap();
    c.append(KIND_USER, "milk is low");
    c.close();
    let (_, out, _) = cli_run(&data, &["context", "--store", "home", "--data-dir", d], "");
    assert!(out.contains("milk is low"), "{out}");
}

// Round 7 fix: only the owner's chats enter the memory, and each source
// keeps its own place.

fn cli_env_run(data: &Path, home: &Path, env: &[(&str, &str)], argv: &[&str], stdin: &str) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(stdin.as_bytes().to_vec());
    let map: std::collections::HashMap<String, String> = env.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect();
    let genv = move |k: &str| map.get(k).cloned().unwrap_or_default();
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: home.to_path_buf(),
            new_summarizer: None,
            progress: None,
            now: None,
            embed: |_| vec![0.0; 4],
            hermes_source: None,
            getenv: Some(&genv),
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

#[test]
fn a_hook_in_a_skipped_process_logs_nothing_and_prints_no_view() {
    let data = temp_dir();
    let d = data.to_str().unwrap();
    let dir = data.join("pando").join("work");
    let mut c = Chat::open(cfg(&dir)).unwrap();
    c.append(KIND_USER, "the owner said the blue door code is 4417");
    c.close();
    let tr = data.join("worker.jsonl");
    write_tr(&tr, &[line("user", "w1", "2026-11-05T10:00:00Z", "worker only line")]);
    let stop = format!("{{\"hook_event_name\":\"Stop\",\"transcript_path\":{:?}}}", tr.to_str().unwrap());
    let start = "{\"hook_event_name\":\"SessionStart\"}";
    let skip = [("DAISUGI_PANDO_SKIP", "1")];
    let home = data.join("nohome");
    assert_eq!(cli_env_run(&data, &home, &skip, &["hook", "--data-dir", d], start), (0, String::new(), String::new()));
    assert_eq!(cli_env_run(&data, &home, &skip, &["hook", "--data-dir", d], &stop).1, "");
    assert_eq!(cli_env_run(&data, &home, &skip, &["context", "--data-dir", d], "").1, "");
    assert!(!cli_env_run(&data, &home, &[], &["log", "--data-dir", d], "").1.contains("worker only line"));
    // The owner's chat, in the same store, is logged and gets the view. The
    // worker's own session is on the skip list for good, so the owner's chat
    // is another transcript.
    let otr = data.join("owner.jsonl");
    write_tr(&otr, &[line("user", "o1", "2026-11-05T10:00:00Z", "owner line")]);
    let ostop = format!("{{\"hook_event_name\":\"Stop\",\"transcript_path\":{:?}}}", otr.to_str().unwrap());
    assert_eq!(cli_env_run(&data, &home, &[], &["hook", "--data-dir", d], &ostop).0, 0);
    let log = cli_env_run(&data, &home, &[], &["log", "--data-dir", d], "").1;
    assert!(log.contains("owner line") && !log.contains("worker only line"), "{log}");
    let out = cli_env_run(&data, &home, &[("DAISUGI_PANDO_SKIP", "0")], &["hook", "--data-dir", d], start).1;
    assert!(out.contains("4417"), "{out}");
}

#[test]
fn a_transcript_under_the_coppice_data_dir_is_not_logged() {
    let home = temp_dir();
    let worktree = home.join(".local/state/coppice/chat/work/wt");
    let none = |_: &str| String::new();
    for (cwd, logged) in [(worktree.to_str().unwrap().to_string(), false), ("/work/proj".to_string(), true)] {
        let root = temp_dir();
        let dir = root.join("pando").join("work");
        let l = format!(
            "{{\"type\":\"user\",\"uuid\":\"u-x\",\"sessionId\":\"s\",\"cwd\":{cwd:?},\"timestamp\":\"2026-11-05T10:00:00Z\",\"message\":{{\"role\":\"user\",\"content\":[{{\"type\":\"text\",\"text\":\"hello\"}}]}}}}"
        );
        let tr = root.join("s.jsonl");
        write_tr(&tr, &[l]);
        let mut c = Chat::open(cfg(&dir)).unwrap();
        let skip = skip_dirs(&dir, &home, Some(&none));
        let n = c.ingest_claude(&tr, &skip).unwrap();
        c.close();
        assert_eq!(n == 1, logged, "cwd {cwd}");
    }
}

/// A Claude Code pane steward runs in the steward dir. Its chat is the
/// owner's, so follow logs it (workers there carry the marker and the list).
#[test]
fn a_claude_code_pane_steward_is_logged() {
    let home = temp_dir();
    let steward = home.join(".local/state/coppice-steward");
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let cwd = steward.to_str().unwrap();
    let l = format!(
        "{{\"type\":\"user\",\"uuid\":\"u-st\",\"sessionId\":\"s\",\"cwd\":{cwd:?},\"timestamp\":\"2026-11-05T10:00:00Z\",\"message\":{{\"role\":\"user\",\"content\":[{{\"type\":\"text\",\"text\":\"hello steward\"}}]}}}}"
    );
    let tr = root.join("s.jsonl");
    write_tr(&tr, &[l]);
    let none = |_: &str| String::new();
    let mut c = Chat::open(cfg(&dir)).unwrap();
    let n = c.ingest_claude(&tr, &skip_dirs(&dir, &home, Some(&none))).unwrap();
    c.close();
    assert_eq!(n, 1);
}

#[test]
fn skip_dirs_name_the_coppice_dirs() {
    let env = |k: &str| match k {
        "COPPICE_DATA_DIR" => "/d/coppice".to_string(),
        "XDG_STATE_HOME" => "/s".to_string(),
        "XDG_DATA_HOME" => "/x".to_string(),
        _ => String::new(),
    };
    let got = skip_dirs(Path::new("/data/pando/work"), Path::new("/home/u"), Some(&env)).join(" ");
    for w in [
        "/data/pando/work",
        "/d/coppice",
        "/s/coppice",
        "/data/coppice",
        "/x/opendaisugi/coppice",
        "/home/u/.opendaisugi/coppice",
    ] {
        assert!(got.contains(w), "{got} lacks {w}");
    }
    // With no XDG_DATA_HOME the default one is named.
    let none = |_: &str| String::new();
    let got = skip_dirs(Path::new("/data/pando/work"), Path::new("/home/u"), Some(&none)).join(" ");
    assert!(got.contains("/home/u/.local/share/opendaisugi/coppice"), "{got}");
}

struct FailingSource;

impl Source for FailingSource {
    fn name(&self) -> String {
        "bad".into()
    }
    fn poll(&mut self, _: &mut Chat, _: Option<SystemTime>) -> Result<usize, String> {
        Err("permission denied".into())
    }
}

#[test]
fn a_failing_source_keeps_its_own_time_and_does_not_stall_the_others() {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let t0 = SystemTime::now();
    let clock = Arc::new(std::sync::Mutex::new(t0));
    let c2 = clock.clone();
    let mut log: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.clone(), &mut log);
    o.now = Some(Arc::new(move || *c2.lock().unwrap()));
    o.sources.push(Box::new(FakeSource(vec![Live {
        kind: KIND_USER.into(),
        text: "ok".into(),
        at: None,
        act: "g:1".into(),
        meta: Meta { src: "g".into(), chat: "".into(), who: "".into() },
    }])));
    o.sources.push(Box::new(FailingSource));
    let ctx = Ctx::background();
    follow_once(&ctx, &mut o).unwrap();
    *clock.lock().unwrap() = t0 + Duration::from_secs(600);
    follow_once(&ctx, &mut o).unwrap();
    drop(o);
    let st: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(dir.join(FOLLOW_FILE)).unwrap()).unwrap();
    let since = |n: &str| st["sources"][n]["since"].as_str().unwrap().to_string();
    assert!(since("fake") > since("bad"), "{st}");
    assert!(String::from_utf8_lossy(&log).contains("follow: bad:"));
}

#[test]
fn an_error_line_is_one_short_line() {
    let long = "secret store text ".repeat(100);
    let got = clip_err(&format!("the model said:\n{long}"));
    assert!(!got.contains('\n') && got.len() <= 200, "{got}");
}

#[test]
fn all_goes_only_with_follow_or_hermes_import() {
    let data = temp_dir();
    let d = data.to_str().unwrap();
    let mut c = Chat::open(cfg(&data.join("pando").join("work"))).unwrap();
    c.append(KIND_USER, "x");
    c.close();
    let (code, _, e) = cli_run(&data, &["status", "--all", "--data-dir", d], "");
    assert!(code == 2 && e.contains("--all goes with"), "{code} {e}");
    let (code, _, e) = cli_run(&data, &["follow", "--once", "--all", "--hermes-profile", "x", "--data-dir", d], "");
    assert!(code == 2 && e.contains("exclude each other"), "{code} {e}");
}

#[test]
fn a_claude_root_that_cannot_be_read_is_an_error_and_a_missing_one_is_not() {
    use std::os::unix::fs::PermissionsExt;
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let mut c = Chat::open(cfg(&dir)).unwrap();
    assert_eq!(ClaudeSource::new(root.join("absent")).poll(&mut c, None), Ok(0));
    let locked = root.join("locked");
    std::fs::create_dir_all(&locked).unwrap();
    std::fs::set_permissions(&locked, std::fs::Permissions::from_mode(0o000)).unwrap();
    let r = ClaudeSource::new(locked.clone()).poll(&mut c, None);
    std::fs::set_permissions(&locked, std::fs::Permissions::from_mode(0o700)).unwrap();
    c.close();
    // Running as root can read the dir; then the test has nothing to check.
    if std::fs::read_dir(&locked).is_ok() && r.is_ok() {
        return;
    }
    assert!(r.is_err(), "{r:?}");
}

#[test]
fn the_mcp_server_of_a_skipped_process_lists_no_tool_and_answers_no_call() {
    let data = temp_dir();
    let d = data.to_str().unwrap();
    let mut c = Chat::open(cfg(&data.join("pando").join("work"))).unwrap();
    c.append(KIND_USER, "the blue door code is 4417");
    c.close();
    let input = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n\
                 {\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n\
                 {\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"recall\",\"arguments\":{\"query\":\"door\"}}}\n";
    let home = data.join("nohome");
    let (_, out, _) = cli_env_run(&data, &home, &[("DAISUGI_PANDO_SKIP", "1")], &["serve", "--data-dir", d], input);
    assert!(!out.contains("4417") && !out.contains("\"name\":\"recall\"") && out.contains("\"tools\":[]") && out.contains(super::mcp::CLOSED_TEXT), "{out}");
    let (_, out, _) = cli_env_run(&data, &home, &[], &["serve", "--data-dir", d], input);
    assert!(out.contains("\"name\":\"recall\""), "{out}");
}
