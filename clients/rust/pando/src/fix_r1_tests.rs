//! Review round 1 of the Pando move: the twins of Go's backup_test,
//! migrate_atomic_test, keep_test, tail_test, link_test and
//! mcp_child_test.

use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use super::chat::*;
use super::chat_tests::{must_append, open_t, temp_dir, Fake, TestClock};
use super::child::*;
use super::cli::{run, CliEnv};
use super::mcp::*;
use super::migrate::*;
use super::store::*;

fn seed(data: &Path, store: &str, lines: &[&str]) -> PathBuf {
    let dir = store_dir(data, store).unwrap();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    for l in lines {
        must_append(&mut c, &Fake::default(), KIND_USER, l);
    }
    c.close();
    dir
}

fn words(_: &str) -> Vec<f64> {
    vec![1.0]
}

fn cli(data: &Path, argv: &[&str]) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(Vec::new());
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
            embed: words,
            hermes_source: None,
            getenv: None,
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

fn old_chat(n: u64) -> PathBuf {
    let old = temp_dir().join("foreman");
    let mut c = open_t(&old, &TestClock::new(), |_| {});
    for i in 0..n {
        // Each message on its own day, so main/ holds many files.
        let day = SystemTime::UNIX_EPOCH + Duration::from_secs(1_767_225_600 + i * 86_400);
        let (_, err) = c.append_at(KIND_USER, "m", Some(day));
        assert!(err.is_none(), "{err:?}");
    }
    c.close();
    old
}

#[test]
fn backup_rels_hold_every_part_of_each_store_and_never_the_lock() {
    let got = backup_rels(&["home".into(), "work".into()]);
    let want = [
        "home/main", "home/tree", "home/view.json", "home/import.json", "home/children",
        "work/main", "work/tree", "work/view.json", "work/import.json", "work/children",
    ];
    assert_eq!(got, want);
}

#[test]
fn backup_stores_are_the_stores_on_disk_and_the_ones_an_archive_names() {
    let d = temp_dir();
    for s in ["work", "notes-2", "work.broken-20261101T000000Z", ".work.migrating-20261101T000000Z", "Bad"] {
        std::fs::create_dir_all(d.join("pando").join(s).join("main")).unwrap();
    }
    let archived: Vec<String> = ["home/main/2026-10-24.jsonl", "home/children/w1/main/x.jsonl", "../etc/x", "Bad/main/x", "lone"]
        .iter()
        .map(|s| s.to_string())
        .collect();
    assert_eq!(backup_stores(&d.join("pando"), &archived), vec!["home", "notes-2", "work"]);
}

/// The child half of a_migrate_killed_midway_leaves_no_store: it migrates
/// and kills itself after the second file it copies.
#[test]
fn migrate_child_process() {
    let (Ok(from), Ok(data)) = (std::env::var("PANDO_MIGRATE_FROM"), std::env::var("PANDO_MIGRATE_DATA")) else {
        return;
    };
    let n = std::cell::Cell::new(0);
    MIGRATE_HOOK.with(|h| {
        *h.borrow_mut() = Some(Box::new(move |_: &str| {
            n.set(n.get() + 1);
            if n.get() == 2 {
                unsafe { libc::kill(libc::getpid(), libc::SIGKILL) };
            }
        }))
    });
    let _ = migrate(Path::new(&from), Path::new(&data), "work", SystemTime::now());
    panic!("the migrate was not killed");
}

#[test]
fn a_migrate_killed_midway_leaves_no_store() {
    use std::os::unix::process::ExitStatusExt;
    let old = old_chat(4);
    let data = temp_dir();
    let st = std::process::Command::new(std::env::current_exe().unwrap())
        .args(["fix_r1_tests::migrate_child_process", "--exact", "--test-threads=1"])
        .env("PANDO_MIGRATE_FROM", &old)
        .env("PANDO_MIGRATE_DATA", &data)
        .output()
        .unwrap();
    assert_eq!(st.status.signal(), Some(libc::SIGKILL), "{st:?}");
    assert!(std::fs::symlink_metadata(data.join("pando").join("work")).is_err(), "a cut migrate must leave no pando/work");
    let tmps: Vec<_> = std::fs::read_dir(data.join("pando"))
        .unwrap()
        .flatten()
        .filter(|e| e.file_name().to_string_lossy().starts_with(&format!(".work{MIGRATING_INFIX}")))
        .collect();
    assert_eq!(tmps.len(), 1, "the partial copy must stay in its temp dir");
    assert!(list_stores(&data).unwrap().is_empty());
    assert!(old.join("main").is_dir(), "the old dir must stay");
    let rep = migrate(&old, &data, "work", SystemTime::now()).unwrap();
    assert_eq!(rep.messages, 4);
    let c = Chat::open_read_only(&store_dir(&data, "work").unwrap()).unwrap();
    assert_eq!(c.count(), 4);
}

#[test]
fn a_migrate_whose_copy_fails_its_check_leaves_no_store() {
    let old = old_chat(3);
    let data = temp_dir();
    let pando = data.join("pando");
    MIGRATE_HOOK.with(|h| {
        *h.borrow_mut() = Some(Box::new(move |stage: &str| {
            // The copy loses a day file after it is written.
            if stage.starts_with("main/") {
                for e in std::fs::read_dir(&pando).unwrap().flatten() {
                    if let Some(f) = std::fs::read_dir(e.path().join("main")).ok().and_then(|mut r| r.next()) {
                        let _ = std::fs::remove_file(f.unwrap().path());
                    }
                }
            }
        }))
    });
    let e = migrate(&old, &data, "work", SystemTime::now()).unwrap_err();
    MIGRATE_HOOK.with(|h| *h.borrow_mut() = None);
    assert!(e.contains(MIGRATING_INFIX), "the error must name the temp dir: {e}");
    assert!(std::fs::symlink_metadata(data.join("pando").join("work")).is_err());
    assert!(old.join("main").is_dir());
}

#[test]
fn a_migrate_never_writes_over_a_store_made_during_the_copy() {
    let old = old_chat(2);
    let data = temp_dir();
    let dir = store_dir(&data, "work").unwrap();
    let other: std::rc::Rc<std::cell::RefCell<Option<Chat>>> = Default::default();
    let o = other.clone();
    let d2 = dir.clone();
    MIGRATE_HOOK.with(|h| {
        *h.borrow_mut() = Some(Box::new(move |_: &str| {
            if o.borrow().is_none() {
                let mut c = Chat::open(Config { dir: d2.clone(), ..Default::default() }).unwrap();
                let (_, err) = c.append(KIND_USER, "written meanwhile");
                assert!(err.is_none());
                *o.borrow_mut() = Some(c);
            }
        }))
    });
    let r = migrate(&old, &data, "work", SystemTime::now());
    MIGRATE_HOOK.with(|h| *h.borrow_mut() = None);
    assert!(r.is_err(), "the migrate must fail");
    assert_eq!(Chat::open_read_only(&dir).unwrap().count(), 1);
    assert!(old.join("main").is_dir());
    drop(other);
}

#[test]
fn a_moved_aside_history_is_never_pruned() {
    let keep = broken_keep();
    set_broken_keep(2);
    let parent = temp_dir();
    let dir = parent.join("steward");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    for _ in 0..5 {
        must_append(&mut c, &Fake::default(), KIND_USER, "old words");
    }
    c.close();
    let mut older = Vec::new();
    for i in 1..=7 {
        let p = parent.join(format!("steward.broken-2026010{i}T000000Z"));
        std::fs::create_dir_all(p.join("main")).unwrap();
        older.push(p);
    }
    let day = std::fs::read_dir(dir.join("main")).unwrap().flatten().next().unwrap().path();
    let mut f = std::fs::OpenOptions::new().append(true).open(day).unwrap();
    std::io::Write::write_all(&mut f, b"not json\n").unwrap();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    c.close();
    set_broken_keep(keep);
    for p in &older {
        assert!(p.join("main").is_dir(), "a moved-aside history was removed: {}", p.display());
    }
    let n = std::fs::read_dir(&parent)
        .unwrap()
        .flatten()
        .filter(|e| e.file_name().to_string_lossy().starts_with("steward.broken-"))
        .count();
    assert_eq!(n, 8);
}

#[test]
fn the_keep_setting_is_pandos_own_and_covers_leaf_copies() {
    let keep = broken_keep();
    for k in [0usize, 2] {
        set_broken_keep(k);
        let dir = temp_dir().join("s");
        let mut c = open_t(&dir, &TestClock::new(), |_| {});
        must_append(&mut c, &Fake::default(), KIND_USER, "x");
        c.close();
        let view = dir.join("view.json");
        for i in 1..=4 {
            std::fs::write(format!("{}.broken-2026010{i}T000000Z", view.display()), "old").unwrap();
        }
        std::fs::write(&view, "not json").unwrap();
        let mut c = open_t(&dir, &TestClock::new(), |_| {});
        c.close();
        let n = std::fs::read_dir(&dir)
            .unwrap()
            .flatten()
            .filter(|e| e.file_name().to_string_lossy().starts_with("view.json.broken-"))
            .count();
        assert_eq!(n, if k == 2 { 2 } else { 5 }, "keep {k}");
    }
    set_broken_keep(keep);
}

#[test]
fn readers_see_every_appended_message() {
    let data = temp_dir();
    for i in 0..5 {
        let text = format!("message {i} {}", "w".repeat(320));
        let (code, _, err) = cli(&data, &["append", &text]);
        assert_eq!(code, 0, "{err}");
    }
    let r = Chat::open_read_only(&store_dir(&data, "work").unwrap()).unwrap();
    assert_eq!(r.count(), 5);
    assert!(r.not_in_view() >= 1 && r.pending() >= 1, "the test needs an untended tail");
    let (code, out, _) = cli(&data, &["status"]);
    assert_eq!(code, 0);
    assert!(out.contains("messages: 5\n"), "{out}");
    assert!(out.contains(&format!("pending nodes: {}\n", r.pending())), "{out}");
    assert!(out.contains(&format!("not in the view yet: {}\n", r.not_in_view())), "{out}");
    let (code, out, err) = cli(&data, &["zoom", "4", "1"]);
    assert!(code == 0 && out.contains("message 4 "), "{code} {out} {err}");
    let (_, out, _) = cli(&data, &["log"]);
    assert!(out.contains("message 4 "), "{out}");
    let (_, out, _) = cli(&data, &["recall", "message 4"]);
    assert!(out.contains("4+1"), "{out}");
    let (_, out, _) = cli(&data, &["tend", "--dry-run"]);
    assert!(out.contains(&format!("pending: {}\nnot in the view yet: {}\n", r.pending(), r.not_in_view())), "{out}");
}

#[test]
fn a_symlink_in_a_store_path_is_refused() {
    let data = temp_dir();
    seed(&data, "home", &["a private home line"]);
    let work = seed(&data, "work", &["a work line"]);
    std::fs::create_dir_all(work.join(CHILDREN_DIR)).unwrap();
    std::os::unix::fs::symlink("../../home", work.join(CHILDREN_DIR).join("evil")).unwrap();
    let (code, out, err) = cli(&data, &["zoom", "--child", "evil", "0", "1"]);
    assert!(code != 0 && !out.contains("private home") && err.contains("link"), "{code} {out} {err}");
    assert!(child_dir(&work, "evil").is_err());
    assert!(list_children(&work).is_empty());

    std::os::unix::fs::symlink("home", data.join("pando").join("other")).unwrap();
    let (code, out, _) = cli(&data, &["zoom", "0", "1", "--store", "other"]);
    assert!(code != 0 && !out.contains("private home"));
    assert!(store_dir(&data, "other").is_err());
    assert!(!list_stores(&data).unwrap().contains(&"other".to_string()));

    let side = data.join("pando").join("side");
    std::fs::create_dir_all(&side).unwrap();
    std::os::unix::fs::symlink("../home/main", side.join("main")).unwrap();
    assert!(Chat::open_read_only(&side).is_err());
    assert!(Chat::open(Config { dir: side, ..Default::default() }).is_err());
}

#[test]
fn the_mcp_zoom_reads_a_child_store() {
    let data = temp_dir();
    let work = seed(&data, "work", &["the parent line"]);
    seed(&data, "home", &["a private home line"]);
    let mut ch = open_t(&child_dir(&work, "w1").unwrap(), &TestClock::new(), |_| {});
    must_append(&mut ch, &Fake::default(), KIND_USER, "the worker's own words");
    ch.close();
    let c = Chat::open_read_only(&work).unwrap();
    let input = concat!(
        "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n",
        "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"zoom\",\"arguments\":{\"id\":0,\"n\":1,\"child\":\"w1\"}}}\n",
        "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"zoom\",\"arguments\":{\"id\":0,\"n\":1,\"child\":\"../../home\"}}}\n",
        "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"zoom\",\"arguments\":{\"id\":0,\"n\":1,\"child\":\"nobody\"}}}\n",
    );
    let mut out = Vec::new();
    serve_mcp(&c, words, &mut std::io::Cursor::new(input.as_bytes()), &mut out).unwrap();
    let text = String::from_utf8(out).unwrap();
    let lines: Vec<&str> = text.lines().collect();
    assert!(lines[0].contains("\"child\""), "{}", lines[0]);
    assert!(lines[1].contains("the worker's own words"), "{}", lines[1]);
    for l in &lines[2..] {
        assert!(l.contains("\"isError\":true") && !l.contains("private home"), "{l}");
    }
}

/// A running server answers from the store as it is now: a message logged
/// after the server started is found.
#[test]
fn the_mcp_server_sees_messages_logged_after_it_started() {
    let data = temp_dir();
    let work = seed(&data, "work", &["the first line"]);
    let c = Chat::open_read_only(&work).unwrap();
    let mut w = Chat::open(Config { dir: work.clone(), ..Default::default() }).unwrap();
    let (_, err) = w.append(KIND_USER, "a line logged later");
    assert!(err.is_none());
    w.close();
    let input = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"zoom\",\"arguments\":{\"id\":1,\"n\":1}}}\n";
    let mut out = Vec::new();
    serve_mcp(&c, words, &mut std::io::Cursor::new(input.as_bytes()), &mut out).unwrap();
    let text = String::from_utf8(out).unwrap();
    assert!(text.contains("a line logged later"), "{text}");
}

/// A day file in main/ that links into another store is never read, and a
/// writer never appends through it.
#[test]
fn a_linked_day_file_is_neither_read_nor_written_through() {
    let data = temp_dir();
    let home = seed(&data, "home", &["a private home line"]);
    let day = std::fs::read_dir(home.join("main")).unwrap().flatten().next().unwrap().path();
    let before = std::fs::read(&day).unwrap();
    let work = store_dir(&data, "work").unwrap();
    std::fs::create_dir_all(work.join("main")).unwrap();
    std::os::unix::fs::symlink(&day, work.join("main").join(day.file_name().unwrap())).unwrap();
    assert_eq!(Chat::open_read_only(&work).unwrap().count(), 0);
    let mut w = open_t(&work, &TestClock::new(), |_| {});
    let _ = w.append(KIND_USER, "written into work");
    w.close();
    assert_eq!(std::fs::read(&day).unwrap(), before, "a writer appended through the link");
}
