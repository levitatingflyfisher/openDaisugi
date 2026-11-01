//! The store rules, children, migrate, the web importer, the MCP server and
//! the verbs: the Rust twin of Go's pando_test.go.

use std::path::{Path, PathBuf};

use super::chat::*;
use super::chat_tests::{must_append, open_t, temp_dir, Fake, TestClock};
use super::child::*;
use super::cli::{run, CliEnv};
use super::ctx::Ctx;
use super::import::{ImportOpts, SRC_WEB};
use super::import_web::*;
use super::mcp::*;
use super::migrate::*;
use super::store::*;
use super::tend::tend;

fn seed(data: &Path, store: &str, lines: &[&str]) -> PathBuf {
    let dir = store_dir(data, store).unwrap();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    for l in lines {
        must_append(&mut c, &f, KIND_USER, l);
    }
    c.close();
    dir
}

fn words(text: &str) -> Vec<f64> {
    // A stand-in matcher: one bucket per first letter.
    let mut v = vec![0.0; 26];
    for w in text.to_lowercase().split(|c: char| !c.is_alphanumeric()) {
        if let Some(c) = w.bytes().next().filter(u8::is_ascii_lowercase) {
            v[(c - b'a') as usize] += 1.0;
        }
    }
    v
}

fn cli(data: &Path, argv: &[&str], stdin: &str) -> (i32, String, String) {
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
            embed: words,
            hermes_source: None,
            getenv: None,
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

#[test]
fn store_dir_rules_match_go() {
    let d = temp_dir();
    assert_eq!(store_dir(&d, "work").unwrap(), d.join("pando").join("work"));
    assert!(matches!(store_dir(&d, "all"), Err(StoreError::All)));
    assert_eq!(
        store_dir(&d, "all").unwrap_err().to_string(),
        "one store at a time: pando stores are separate on purpose."
    );
    for bad in ["", "Work", "1x", "a/b", "a b"] {
        assert!(store_dir(&d, bad).is_err(), "{bad:?} must be refused");
    }
    assert!(!valid_store("x-very-long-name-that-goes-past-thirty-two-chars"));
    assert_eq!(DEFAULT_STORE, "work");
}

#[test]
fn list_stores_names_only_real_ones() {
    let d = temp_dir();
    for (n, with_main) in [("work", true), ("home", true), ("junk", false)] {
        let mut p = d.join("pando").join(n);
        if with_main {
            p = p.join("main");
        }
        std::fs::create_dir_all(p).unwrap();
    }
    assert_eq!(list_stores(&d).unwrap(), vec!["home", "work"]);
}

#[test]
fn the_read_verbs_work_while_a_writer_holds_the_store() {
    let d = temp_dir();
    let dir = seed(&d, "work", &["alpha words", "beta words"]);
    let w = Chat::open(Config {
        dir: dir.clone(),
        ..Default::default()
    })
    .unwrap();
    let (code, out, _) = cli(&d, &["log"], "");
    assert_eq!(code, 0);
    assert!(out.contains("\tuser\t") && out.contains("alpha words"), "{out}");
    let (code, out, _) = cli(&d, &["zoom", "0", "1"], "");
    assert_eq!((code, out.as_str()), (0, "alpha words\n"));
    let (code, out, _) = cli(&d, &["stores", "--json"], "");
    assert_eq!((code, out.as_str()), (0, "[\"work\"]\n"));
    drop(w);
}

#[test]
fn all_and_a_missing_store_are_refused() {
    let d = temp_dir();
    let (code, _, err) = cli(&d, &["--store", "all", "view"], "");
    assert_eq!(code, 2);
    assert!(err.contains("one store at a time"), "{err}");
    let (code, _, err) = cli(&d, &["--store", "home", "view"], "");
    assert_eq!(code, 1);
    assert!(err.starts_with("no store home at "), "{err}");
}

#[test]
fn append_makes_the_store_and_recall_ranks() {
    let d = temp_dir();
    let (code, out, _) = cli(&d, &["--store", "home", "append", "-"], "apples and ants\n");
    assert_eq!((code, out.as_str()), (0, "0\n"));
    let (code, out, _) = cli(&d, &["--store", "home", "append", "zebra only"], "");
    assert_eq!((code, out.as_str()), (0, "1\n"));
    let (code, out, _) = cli(&d, &["--store", "home", "recall", "apple"], "");
    assert_eq!(code, 0);
    assert!(out.starts_with("0+1\t"), "{out}");
}

#[test]
fn tend_builds_pending_nodes_with_a_fake_and_a_dry_run_calls_none() {
    let d = temp_dir();
    let dir = store_dir(&d, "work").unwrap();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let long = "x".repeat(600);
    for _ in 0..3 {
        c.append(KIND_USER, &long);
    }
    assert!(c.pending() > 0);
    c.close();
    let (code, out, _) = cli(&d, &["tend", "--dry-run"], "");
    assert_eq!(code, 0);
    assert!(out.starts_with("nodes to build: ") && out.contains("\npending: ") && out.ends_with("nothing was changed\n"), "{out}");
    let mut c = Chat::open(Config {
        dir,
        ..Default::default()
    })
    .unwrap();
    let f = Fake::default();
    let (built, err) = tend(&Ctx::background(), &mut c, &f, 0);
    assert!(err.is_none() && built > 0 && c.pending() == 0, "{built} {err:?}");
}

#[test]
fn the_mcp_server_lists_three_read_tools_and_zooms() {
    let d = temp_dir();
    let dir = seed(&d, "work", &["first words"]);
    let c = Chat::open_read_only(&dir).unwrap();
    let input = concat!(
        "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n",
        "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n",
        "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n",
        "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"zoom\",\"arguments\":{\"id\":0,\"n\":1}}}\n",
        "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"write\",\"arguments\":{}}}\n",
    );
    let mut out = Vec::new();
    serve_mcp(&c, words, &mut std::io::Cursor::new(input.as_bytes()), &mut out).unwrap();
    let text = String::from_utf8(out).unwrap();
    let lines: Vec<&str> = text.lines().collect();
    assert_eq!(lines.len(), 4, "{text}");
    assert!(lines[0].contains("\"serverInfo\":{\"name\":\"pando\",\"version\":\"1\"}"), "{}", lines[0]);
    for t in ["zoom", "date", "recall"] {
        assert!(lines[1].contains(&format!("\"name\":\"{t}\"")), "{}", lines[1]);
    }
    assert!(!lines[1].contains("append"));
    assert!(lines[2].contains("\"text\":\"first words\""), "{}", lines[2]);
    assert!(lines[3].contains("-32601"), "{}", lines[3]);
}

#[test]
fn migrate_copies_checks_renames_and_deletes_nothing() {
    let d = temp_dir();
    let old = d.join("foreman");
    {
        let mut c = open_t(&old, &TestClock::new(), |_| {});
        let f = Fake::default();
        must_append(&mut c, &f, KIND_USER, "kept words");
        c.close();
    }
    std::fs::write(old.join("turns.jsonl"), "{}\n").unwrap();
    let rep = migrate(&old, &d, "work", std::time::SystemTime::UNIX_EPOCH).unwrap();
    assert!(!old.exists());
    assert_eq!(rep.messages, 1);
    assert_eq!(rep.kept, vec!["turns.jsonl"]);
    assert!(rep.moved_to.ends_with("foreman.moved-19700101T000000Z"), "{}", rep.moved_to);
    assert!(Path::new(&rep.moved_to).join("main").is_dir());
    let again = migrate(&old, &d, "work", std::time::SystemTime::UNIX_EPOCH).unwrap();
    assert!(again.already);
}

#[test]
fn migrate_refuses_while_open_and_when_the_store_exists() {
    let d = temp_dir();
    let old = d.join("foreman");
    let mut c = open_t(&old, &TestClock::new(), |_| {});
    must_append(&mut c, &Fake::default(), KIND_USER, "w");
    let e = migrate(&old, &d, "work", std::time::SystemTime::UNIX_EPOCH).unwrap_err();
    assert_eq!(e, ERR_OPEN_ELSEWHERE);
    assert!(!d.join("pando").exists());
    c.close();
    seed(&d, "work", &["x"]);
    let e = migrate(&old, &d, "work", std::time::SystemTime::UNIX_EPOCH).unwrap_err();
    assert_eq!(e, ERR_TARGET_EXISTS);
    assert!(old.join("main").is_dir());
}

fn web_fixture() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../fixtures/pando/transcripts/web/conversations.json")
}

#[test]
fn the_web_scan_keys_by_conversation_and_message() {
    let ss = scan_web(&web_fixture(), &ImportOpts::default(), None).unwrap();
    assert!(ss.len() >= 2);
    for s in &ss {
        assert!(s.key.starts_with("web:") && s.src == SRC_WEB);
    }
    let mut keys: Vec<_> = ss.iter().map(|s| s.key.clone()).collect();
    keys.sort();
    keys.dedup();
    assert_eq!(keys.len(), ss.len());
}

#[test]
fn a_map_rewrites_a_project_path_only() {
    let m = parse_map("/old/work=/new/work").unwrap();
    let o = ImportOpts {
        map: vec![m],
        ..Default::default()
    };
    assert_eq!(o.project("/old/work/app"), "/new/work/app");
    assert_eq!(o.project("/old/work"), "/new/work");
    assert_eq!(o.project("/old/workshop"), "/old/workshop");
    assert!(parse_map("nope").is_err());
}

#[test]
fn a_proposal_refuses_all_and_the_accept_goes_to_the_named_store() {
    let mut r = std::io::Cursor::new(b"web:a\tall\tr\tt\n".to_vec());
    let e = read_proposal(&mut r).unwrap_err();
    assert!(e.contains("one store at a time"), "{e}");
    let d = temp_dir();
    let prop = d.join("p.tsv");
    let web = web_fixture();
    let w = web.to_string_lossy().into_owned();
    let (code, _, _) = cli(&d, &["import", "--from", "web", "--web", &w, "--propose", &prop.to_string_lossy()], "");
    assert_eq!(code, 0);
    let text = std::fs::read_to_string(&prop).unwrap();
    std::fs::write(&prop, text.replace("\twork\t", "\thome\t")).unwrap();
    let (code, out, _) = cli(&d, &["import", "--from", "web", "--web", &w, "--accept", &prop.to_string_lossy()], "");
    assert_eq!(code, 0, "{out}");
    assert!(d.join("pando/home/main").is_dir() && !d.join("pando/work").exists());
}

#[test]
fn a_child_store_is_separate_and_tended() {
    let d = temp_dir();
    let store = seed(&d, "work", &["parent"]);
    assert!(child_dir(&store, "../x").is_err());
    let f = Fake::default();
    let mut c = open_child(&store, "w-1", Config::default()).unwrap();
    assert!(c.append(KIND_USER, &"y".repeat(700)).1.is_none());
    c.close();
    assert_eq!(list_children(&store), vec!["w-1"]);
    let (n, err) = tend_children(&Ctx::background(), &store, &f, 0, None);
    assert!(err.is_none() && n > 0, "{n} {err:?}");
    let child = Chat::open_read_only(&child_dir(&store, "w-1").unwrap()).unwrap();
    assert_eq!(child.count(), 1);
    assert_eq!(Chat::open_read_only(&store).unwrap().count(), 1);
    let (code, out, _) = cli(&d, &["zoom", "--child", "w-1", "0", "1"], "");
    assert_eq!(code, 0);
    assert!(out.starts_with("yyyy"), "{out}");
}
