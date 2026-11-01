//! The skip list and the cheap view read: the Rust twin of Go's
//! skiplist_test.go and contextfast_test.go.

use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use super::chat::*;
use super::chat_tests::temp_dir;
use super::cli::{run, CliEnv};
use super::ctx::Ctx;
use super::live::*;

fn sess_line(sid: &str, uuid: &str, text: &str) -> String {
    format!(
        "{{\"type\":\"user\",\"uuid\":\"{uuid}\",\"sessionId\":\"{sid}\",\"cwd\":\"/work/proj\",\"timestamp\":\"{}\",\"message\":{{\"role\":\"user\",\"content\":[{{\"type\":\"text\",\"text\":\"{text}\"}}]}}}}",
        date_text(SystemTime::now())
    )
}

fn write_tr(p: &Path, lines: &[String]) {
    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
    std::fs::write(p, lines.join("\n") + "\n").unwrap();
}

fn cli_env(data: &Path, env: &[(&str, &str)], argv: &[&str], stdin: &str) -> (i32, String, String) {
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
            home: data.join("nohome"),
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

fn count_text(dir: &Path, needle: &str) -> usize {
    let Ok(c) = Chat::open_read_only(dir) else { return 0 };
    (0..c.count()).filter(|&i| c.message(i).map_or(false, |m| m.text.contains(needle))).count()
}

fn fixture() -> (PathBuf, PathBuf) {
    let root = temp_dir();
    let proj = root.join("projects").join("-work-proj");
    std::fs::create_dir_all(&proj).unwrap();
    (root.join("pando").join("work"), proj)
}

#[test]
fn a_hook_in_a_skipped_process_puts_its_session_on_the_skip_list() {
    let data = temp_dir();
    let tr = data.join("w-1.jsonl");
    let ev = format!("{{\"hook_event_name\":\"SessionStart\",\"session_id\":\"w-1\",\"transcript_path\":{:?}}}", tr.to_str().unwrap());
    let d = data.to_str().unwrap();
    let r = cli_env(&data, &[("DAISUGI_PANDO_SKIP", "1")], &["hook", "--data-dir", d], &ev);
    assert_eq!((r.0, r.1.as_str()), (0, ""));
    let set = load_skip(&skip_list_in(&data));
    assert!(set.contains("w-1") && set.contains(tr.to_str().unwrap()), "{set:?}");
    // The owner's hook adds nothing.
    let ev = "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"o-1\",\"transcript_path\":\"/x/o-1.jsonl\"}";
    cli_env(&data, &[], &["hook", "--data-dir", d], ev);
    assert!(!load_skip(&skip_list_in(&data)).contains("o-1"));
}

#[test]
fn follow_drops_a_listed_session_and_keeps_the_owners_in_the_same_project_dir() {
    let (dir, proj) = fixture();
    write_tr(&proj.join("worker-1.jsonl"), &[sess_line("worker-1", "w1", "WORKERLINE one")]);
    write_tr(&proj.join("owner-1.jsonl"), &[sess_line("owner-1", "o1", "OWNERLINE one")]);
    append_skip(&skip_list_path(&dir), &["worker-1"]).unwrap();
    let mut log: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.clone(), &mut log);
    o.sources.push(Box::new(ClaudeSource::new(proj.parent().unwrap().to_path_buf())));
    let (added, _, _) = follow_once(&Ctx::background(), &mut o).unwrap();
    drop(o);
    assert_eq!(added, 1);
    assert_eq!((count_text(&dir, "WORKERLINE"), count_text(&dir, "OWNERLINE")), (0, 1));
}

#[test]
fn a_listed_path_drops_a_session_whose_id_the_file_does_not_show() {
    let (dir, proj) = fixture();
    let p = proj.join("renamed.jsonl");
    write_tr(&p, &[sess_line("inner-id", "w1", "WORKERLINE")]);
    append_skip(&skip_list_path(&dir), &[p.to_str().unwrap()]).unwrap();
    let mut log: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.clone(), &mut log);
    o.sources.push(Box::new(ClaudeSource::new(proj.parent().unwrap().to_path_buf())));
    assert_eq!(follow_once(&Ctx::background(), &mut o).unwrap().0, 0);
}

#[test]
fn the_importer_reads_the_same_list() {
    let (dir, proj) = fixture();
    write_tr(&proj.join("worker-1.jsonl"), &[sess_line("worker-1", "w1", "WORKERLINE")]);
    write_tr(&proj.join("owner-1.jsonl"), &[sess_line("owner-1", "o1", "OWNERLINE")]);
    append_skip(&skip_list_path(&dir), &["worker-1"]).unwrap();
    let o = super::import::ImportOpts {
        from: "claude".into(),
        skip_sessions: load_skip(&skip_list_path(&dir)),
        ..Default::default()
    };
    let r = super::import::read_session(&proj.join("worker-1.jsonl"), "claude", &o).unwrap();
    assert_eq!(r.msgs.len(), 0);
    let r = super::import::read_session(&proj.join("owner-1.jsonl"), "claude", &o).unwrap();
    assert_eq!(r.msgs.len(), 1);
}

// The follow service runs while a worker writes. The worker's hook has put
// its session on the list before its first transcript line, so nothing of it
// is logged, however the polls fall. The owner's chat in the same dir is.
#[test]
fn follow_running_while_a_worker_writes_logs_only_the_owner() {
    let (dir, proj) = fixture();
    let data = dir.parent().unwrap().parent().unwrap().to_path_buf();
    let ctx = Ctx::background();
    let (dir2, root2, ctx2) = (dir.clone(), proj.parent().unwrap().to_path_buf(), ctx.clone());
    let h = std::thread::spawn(move || {
        let mut log: Vec<u8> = Vec::new();
        let mut o = FollowOpts::new(dir2, &mut log);
        o.interval = Duration::from_millis(5);
        o.sources.push(Box::new(ClaudeSource::new(root2)));
        while ctx2.err().is_none() {
            let _ = follow_once(&ctx2, &mut o);
            std::thread::sleep(Duration::from_millis(5));
        }
    });
    let wpath = proj.join("worker-9.jsonl");
    let hook = format!("{{\"hook_event_name\":\"SessionStart\",\"session_id\":\"worker-9\",\"transcript_path\":{:?}}}", wpath.to_str().unwrap());
    assert_eq!(cli_env(&data, &[("DAISUGI_PANDO_SKIP", "1")], &["hook", "--data-dir", data.to_str().unwrap()], &hook).0, 0);
    let mut lines = Vec::new();
    for i in 0..12 {
        lines.push(sess_line("worker-9", &format!("w{i}"), &format!("WORKERLINE {i}")));
        write_tr(&wpath, &lines);
        if i == 4 {
            write_tr(&proj.join("owner-9.jsonl"), &[sess_line("owner-9", "o1", "OWNERLINE")]);
        }
        std::thread::sleep(Duration::from_millis(8));
    }
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    while count_text(&dir, "OWNERLINE") == 0 && std::time::Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(10));
    }
    std::thread::sleep(Duration::from_millis(60));
    ctx.cancel();
    h.join().unwrap();
    assert_eq!(count_text(&dir, "WORKERLINE"), 0, "the follow service logged a worker");
    assert_eq!(count_text(&dir, "OWNERLINE"), 1);
}

#[test]
fn a_spawner_lists_the_id_before_the_child_exists() {
    let (dir, proj) = fixture();
    let data = dir.parent().unwrap().parent().unwrap();
    let id = new_skipped_session(data).unwrap();
    assert_eq!(id.len(), 36);
    assert!(load_skip(&skip_list_path(&dir)).contains(&id));
    write_tr(&proj.join(format!("{id}.jsonl")), &[sess_line(&id, "w1", "WORKERLINE")]);
    let mut log: Vec<u8> = Vec::new();
    let mut o = FollowOpts::new(dir.clone(), &mut log);
    o.sources.push(Box::new(ClaudeSource::new(proj.parent().unwrap().to_path_buf())));
    assert_eq!(follow_once(&Ctx::background(), &mut o).unwrap().0, 0);
}

#[test]
fn a_skipped_serve_answers_closed_even_when_there_is_no_store() {
    let data = temp_dir();
    let input = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n";
    let (code, out, _) = cli_env(&data, &[("DAISUGI_PANDO_SKIP", "1")], &["serve", "--data-dir", data.to_str().unwrap()], input);
    assert_eq!(code, 0);
    assert!(out.contains("\"tools\":[]"), "{out}");
    assert!(!data.join("pando").join("work").exists(), "a skipped server made the store");
}

fn big_store(n: usize, days: bool) -> PathBuf {
    let root = temp_dir();
    let dir = root.join("pando").join("work");
    let t = std::sync::Arc::new(std::sync::Mutex::new(SystemTime::UNIX_EPOCH + Duration::from_secs(1_791_500_000)));
    let t2 = t.clone();
    let clock: Clock = std::sync::Arc::new(move || *t2.lock().unwrap());
    let mut c = Chat::open(Config { dir: dir.clone(), now: Some(clock), ..Default::default() }).unwrap();
    for i in 0..n {
        c.append(KIND_USER, &format!("message number {i} about the blue door"));
        let mut g = t.lock().unwrap();
        *g += if days && i % 7 == 6 { Duration::from_secs(26 * 3600) } else { Duration::from_secs(60) };
    }
    c.close();
    dir
}

// The cheap read gives the same bytes as the full read, whatever the shape of
// the store: few messages, many, in one day file or in several.
#[test]
fn the_view_context_reads_less_and_gives_the_same_bytes() {
    for (n, days) in [(1, false), (5, false), (21, false), (60, false), (60, true), (200, true)] {
        let dir = big_store(n, days);
        let want = Chat::open_read_only(&dir).unwrap().view_context();
        let fast = Chat::open_for_context(&dir, 20).unwrap_or_else(|| panic!("n={n} days={days}: the fast read gave up")).view_context();
        assert!(!want.is_empty());
        assert_eq!(fast, want, "n={n} days={days}");
        assert_eq!(view_context_of(&dir).unwrap(), want);
    }
}

#[test]
fn the_view_context_falls_back_when_the_store_is_not_plain() {
    let dir = big_store(30, true);
    let want = Chat::open_read_only(&dir).unwrap().view_context();
    let mut days: Vec<_> = std::fs::read_dir(dir.join("main")).unwrap().flatten().map(|e| e.path()).collect();
    days.sort();
    // A torn last line is skipped by a reader; both reads agree.
    use std::io::Write;
    let mut f = std::fs::OpenOptions::new().append(true).open(days.last().unwrap()).unwrap();
    f.write_all(b"{\"i\":30,\"kind\":\"user\",\"te").unwrap();
    assert_eq!(view_context_of(&dir).unwrap(), want);
    // A line lost from an older file: ids no longer run on from the count.
    let t = std::fs::read_to_string(&days[0]).unwrap();
    let rest: String = t.split_inclusive('\n').skip(1).collect();
    std::fs::write(&days[0], rest).unwrap();
    assert!(Chat::open_for_context(&dir, 20).is_none(), "the fast read trusted a log with a missing message");
    assert!(view_context_of(&temp_dir().join("none")).is_err());
}

#[test]
fn a_hooks_session_is_listed_once() {
    let data = temp_dir();
    let d = data.to_str().unwrap();
    let ev = "{\"hook_event_name\":\"Stop\",\"session_id\":\"w-9\",\"transcript_path\":\"/x/w-9.jsonl\"}";
    for _ in 0..4 {
        cli_env(&data, &[("DAISUGI_PANDO_SKIP", "1")], &["hook", "--data-dir", d], ev);
    }
    let t = std::fs::read_to_string(skip_list_in(&data)).unwrap();
    assert_eq!(t.matches("w-9").count(), 2, "{t}");
}

#[test]
fn a_hook_lists_in_the_list_the_spawner_names() {
    let data = temp_dir();
    let other = temp_dir().join("pando").join("skip.sessions");
    let d = data.to_str().unwrap();
    let ev = "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"w-7\",\"transcript_path\":\"/x/w-7.jsonl\"}";
    cli_env(&data, &[("DAISUGI_PANDO_SKIP", "1"), ("DAISUGI_PANDO_SKIPLIST", other.to_str().unwrap())], &["hook", "--data-dir", d], ev);
    assert!(load_skip(&other).contains("w-7") && load_skip(&skip_list_in(&data)).contains("w-7"));
}

#[test]
fn a_session_id_is_minted_only_for_a_marked_process() {
    let data = temp_dir();
    let list = skip_list_in(&data);
    let l = list.to_str().unwrap();
    assert!(session_args(Some(l), "").is_empty() && session_args(Some(l), "0").is_empty());
    assert!(!list.exists());
    let a = session_args(Some(l), "1");
    assert_eq!(a[0], "--session-id");
    assert!(load_skip(&list).contains(&a[1]));
}
