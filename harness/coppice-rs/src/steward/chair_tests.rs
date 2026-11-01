//! The chat's tests, ported from Go's internal/steward chat_test.go,
//! build_test.go, retry_test.go, fresh_test.go, appendfail_test.go,
//! perms_test.go, snapshot_test.go, viewtext_test.go and cli_test.go.

use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use pando::chat::*;
use pando::ctx::Ctx;
use pando::fsutil::{self, clip, split};
use pando::order::NodeId;

pub const ERR_FAKE: &str = "fake summarizer is down";

/// The deterministic summarizer of the tests and the golden fixtures.
#[derive(Default)]
pub struct Fake {
    pub compress: AtomicUsize,
    pub merge: AtomicUsize,
    pub fail: std::sync::atomic::AtomicBool,
}

impl Summarizer for Fake {
    fn compress(&self, _: &Ctx, m: &Message, _: usize) -> Result<String, String> {
        if self.fail.load(Ordering::SeqCst) {
            return Err(ERR_FAKE.into());
        }
        self.compress.fetch_add(1, Ordering::SeqCst);
        Ok(format!("c {}", clip(&m.text, 60)))
    }
    fn merge(&self, _: &Ctx, _: NodeId, l: &str, r: &str, _: usize) -> Result<String, String> {
        if self.fail.load(Ordering::SeqCst) {
            return Err(ERR_FAKE.into());
        }
        self.merge.fetch_add(1, Ordering::SeqCst);
        Ok(format!("m {} | {}", clip(l, 40), clip(r, 40)))
    }
}

/// Returns more than the limit, to test the clip.
struct Long;

impl Summarizer for Long {
    fn compress(&self, _: &Ctx, _: &Message, limit: usize) -> Result<String, String> {
        Ok(format!("{:0>w$}", 7, w = limit * 2))
    }
    fn merge(&self, _: &Ctx, _: NodeId, _: &str, _: &str, limit: usize) -> Result<String, String> {
        Ok(format!("{:0>w$}", 8, w = limit * 2))
    }
}

/// A clock a test moves by hand.
#[derive(Clone)]
pub struct TestClock(pub Arc<Mutex<SystemTime>>);

impl TestClock {
    pub fn new() -> TestClock {
        TestClock(Arc::new(Mutex::new(
            parse_date("2026-10-08T23:58:00Z").unwrap(),
        )))
    }
    pub fn clock(&self) -> Clock {
        let c = self.0.clone();
        Arc::new(move || *c.lock().unwrap())
    }
    pub fn advance(&self, d: Duration) {
        *self.0.lock().unwrap() += d;
    }
    pub fn set(&self, t: SystemTime) {
        *self.0.lock().unwrap() = t;
    }
    pub fn now(&self) -> SystemTime {
        *self.0.lock().unwrap()
    }
}

static N: AtomicUsize = AtomicUsize::new(0);

/// A fresh scratch dir on real disk.
pub fn temp_dir() -> PathBuf {
    let d = crate::testhome::guard(std::env::temp_dir().join(format!(
        "coppice-rs-fm-{}-{}",
        std::process::id(),
        N.fetch_add(1, Ordering::SeqCst)
    )));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

pub fn open_t(dir: &Path, clk: &TestClock, tweak: impl FnOnce(&mut Config)) -> Chat {
    fsutil::NO_SYNC.store(true, Ordering::Relaxed);
    let mut cfg = Config {
        dir: dir.to_path_buf(),
        now: Some(clk.clock()),
        ..Default::default()
    };
    tweak(&mut cfg);
    Chat::open(cfg).unwrap_or_else(|e| panic!("Open: {e}"))
}

pub fn must_append(c: &mut Chat, s: &dyn Summarizer, kind: &str, text: &str) -> Vec<i64> {
    let (ids, err) = c.append(kind, text);
    assert!(err.is_none(), "Append({kind}): {err:?}");
    c.drain(&Ctx::background(), s).expect("Drain");
    ids
}

fn small_view(cfg: &mut Config) {
    cfg.view_max = 2048;
    cfg.view_min = 1024;
}

/// Every file under dir but the lock, with its relative path.
pub fn read_all(dir: &Path) -> String {
    fn walk(d: &Path, out: &mut Vec<PathBuf>) {
        if let Ok(rd) = std::fs::read_dir(d) {
            for e in rd.flatten() {
                let p = e.path();
                if p.is_dir() {
                    walk(&p, out);
                } else if p.file_name().is_some_and(|n| n != "lock") {
                    out.push(p);
                }
            }
        }
    }
    let mut paths = Vec::new();
    walk(dir, &mut paths);
    paths.sort();
    let mut b = String::new();
    for p in paths {
        let rel = p.strip_prefix(dir).unwrap().to_string_lossy().into_owned();
        b.push_str(&format!(
            "== {rel}\n{}",
            std::fs::read_to_string(&p).unwrap()
        ));
    }
    b
}

fn mode(p: &Path) -> u32 {
    std::fs::metadata(p).unwrap().permissions().mode() & 0o777
}


const BROKEN_NAME: &str = "steward.broken-20261008T235800Z";

fn make_history(parent: &Path) -> (PathBuf, PathBuf) {
    let dir = parent.join("steward");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    for _ in 0..5 {
        must_append(&mut c, &f, KIND_USER, "old words");
    }
    c.close();
    (dir.clone(), dir.join("main/2026-10-08.jsonl"))
}

const STAMP: &str = "20261008T235800Z";

/// make_history with a long first message, so its node needs the model.
fn make_long(parent: &Path) -> (PathBuf, PathBuf) {
    let dir = parent.join("steward");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    must_append(&mut c, &f, KIND_USER, &"x".repeat(600));
    for _ in 0..4 {
        must_append(&mut c, &f, KIND_USER, "old words");
    }
    c.close();
    (dir.clone(), dir.join("main/2026-10-08.jsonl"))
}

#[test]
fn a_reader_reports_a_broken_history() {
    let parent = temp_dir();
    let (dir, day) = make_history(&parent);
    append_to(&day, "not json\n");
    let before = read_all(&dir);
    let e = Chat::open_read_only(&dir).err().unwrap();
    assert!(e.contains(&dir.to_string_lossy().into_owned()), "{e}");
    assert_eq!(read_all(&dir), before);
    assert_eq!(std::fs::read_dir(&parent).unwrap().count(), 1);
    let ds = dir.to_string_lossy().into_owned();
    let (code, _, err) = run_cli(None, &["--data-dir", ds.as_str(), "view"]);
    assert_eq!(code, 1);
    assert!(err.contains(&dir.to_string_lossy().into_owned()));
}

pub fn run_cli(coppice_dir: Option<&Path>, argv: &[&str]) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut errw) = (Vec::new(), Vec::new());
    let code = super::cli::main(&argv, coppice_dir, &mut out, &mut errw);
    (
        code,
        String::from_utf8(out).unwrap(),
        String::from_utf8(errw).unwrap(),
    )
}

#[test]
fn the_cli_reads() {
    let dir = temp_dir();
    let ds = dir.to_string_lossy().into_owned();
    let d = ds.as_str();
    let f = Fake::default();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    must_append(&mut c, &f, KIND_USER, "first\nline");
    must_append(&mut c, &f, KIND_STEWARD, "second");
    let before = read_all(&dir);
    assert_eq!(
        run_cli(None, &["--data-dir", d, "log"]),
        (
            0,
            "0 2026-10-08T23:58:00Z user|first line\n1 2026-10-08T23:58:00Z steward|second\n"
                .into(),
            String::new()
        )
    );
    let (code, out, _) = run_cli(
        None,
        &["log", "--json", "--day", "2026-10-08", "--data-dir", d],
    );
    assert_eq!(code, 0);
    assert_eq!(out.matches('\n').count(), 2);
    assert!(out.contains("\"text\":\"first\\nline\""));
    assert_eq!(
        run_cli(None, &["--data-dir", d, "log", "--day", "2026-10-09"]).1,
        ""
    );
    assert_eq!(run_cli(None, &["--data-dir", d, "view"]).1, c.render());
    assert_eq!(
        run_cli(None, &["--data-dir", d, "zoom", "0+2"]).1,
        "0+1|first line\n1+1|second\n"
    );
    assert_eq!(
        run_cli(None, &["--data-dir", d, "zoom", "0"]).1,
        "first\nline\n"
    );
    assert_eq!(
        run_cli(None, &["--data-dir", d, "date", "1"]).1,
        "2026-10-08T23:58:00Z\n"
    );
    assert_eq!(read_all(&dir), before);
}

#[test]
fn the_cli_errors() {
    let dir = temp_dir();
    let ds = dir.to_string_lossy().into_owned();
    let d = ds.as_str();
    let ms = dir.join("missing").to_string_lossy().into_owned();
    let missing = ms.as_str();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    must_append(&mut c, &Fake::default(), KIND_USER, "x");
    let cases: Vec<Vec<&str>> = vec![
        vec![],
        vec!["--data-dir", d],
        vec!["--data-dir", d, "nope"],
        vec!["--data-dir", d, "zoom"],
        vec!["--data-dir", d, "zoom", "0+3"],
        vec!["--data-dir", d, "zoom", "4+1"],
        vec!["--data-dir", d, "date", "x"],
        vec!["--data-dir", d, "date", "9"],
        vec!["--data-dir", d, "view", "extra"],
        vec!["--data-dir", d, "--bogus", "view"],
        vec!["--data-dir"],
        vec!["--data-dir", missing, "view"],
        vec!["--data-dir", d, "log", "--day", "bad"],
    ];
    for argv in cases {
        assert_eq!(run_cli(None, &argv).0, 1, "{argv:?}");
    }
    let (code, out, _) = run_cli(None, &["--help"]);
    assert!(code == 0 && out.contains("coppice steward"));
}

#[test]
fn the_cli_uses_the_coppice_data_dir() {
    let top = temp_dir();
    let mut c = open_t(&top.join("pando").join("work"), &TestClock::new(), |_| {});
    must_append(&mut c, &Fake::default(), KIND_USER, "here");
    let (code, out, _) = run_cli(Some(&top), &["log"]);
    assert!(code == 0 && out.contains("user|here"), "{out}");
}

#[test]
fn stats_through_the_command() {
    let dir = temp_dir();
    super::turns::append_turn(
        &dir,
        &super::turns::TurnRecord {
            msg: 0,
            status: "ok".into(),
            usage: Some(super::compactor::CallUsage {
                input: 1,
                cache_read: 3,
                ..Default::default()
            }),
            ..Default::default()
        },
    )
    .unwrap();
    let ds = dir.to_string_lossy().into_owned();
    let (code, out, err) = run_cli(None, &["stats", "--data-dir", ds.as_str()]);
    assert_eq!(code, 0, "{err}");
    assert!(out.contains("cache read share: 75.0%"), "{out}");
}

#[test]
fn a_broken_history_carries_its_dangling_calls_to_the_new_chat() {
    use super::calls::*;
    let parent = temp_dir();
    let (dir, day) = make_history(&parent);
    let rec = |call: &str, phase: &str, turn: i64, args: &str| CallRecord {
        call: call.into(),
        phase: phase.into(),
        turn,
        args: args.into(),
        at: "t".into(),
        tool: if phase == CALL_START { "spawn".into() } else { String::new() },
        outcome: if phase == CALL_END { "ok".into() } else { String::new() },
    };
    append_call(&dir, &rec("c4.1", CALL_START, 4, "{\"x\":1}")).unwrap();
    append_call(&dir, &rec("c3.1", CALL_START, 3, "")).unwrap();
    append_call(&dir, &rec("c3.1", CALL_END, 0, "")).unwrap();
    append_to(&day, "not json\n");
    let c = Chat::open(Config {
        dir: dir.clone(),
        now: Some(TestClock::new().clock()),
        on_moved_aside: Some(std::sync::Arc::new(|b: &Path, f: &Path| carry_dangling_calls(b, f))),
        ..Default::default()
    })
    .unwrap();
    let dang = dangling_calls(&read_calls(&dir).unwrap());
    assert_eq!(dang.len(), 1, "{dang:?}");
    assert!(dang[0].call.ends_with(".c4.1") && dang[0].call != "c4.1", "{}", dang[0].call);
    assert_eq!((dang[0].turn, dang[0].args.as_str()), (-1, "{\"x\":1}"));
    assert_eq!(read_calls(&parent.join(BROKEN_NAME)).unwrap().len(), 3);
    drop(c);
}

#[test]
fn a_reader_skips_a_bad_tree_line() {
    let parent = temp_dir();
    let (dir, _) = make_long(&parent);
    break_node0(&dir);
    let before = read_all(&dir);
    let c = Chat::open_read_only(&dir).unwrap();
    assert!(
        c.notice()
            .contains("1 bad line in tree/2026-10-08.jsonl was skipped")
            && c.notice().contains("Nothing was changed."),
        "{}",
        c.notice()
    );
    assert!(c.render().contains(&format!("0+1|{UNBUILT}")));
    assert_eq!(read_all(&dir), before);
    let ds = dir.to_string_lossy().into_owned();
    let (code, out, err) = run_cli(None, &["--data-dir", ds.as_str(), "view"]);
    assert_eq!(code, 0);
    assert!(out.contains(&format!("0+1|{UNBUILT}")) && err.contains("skipped"), "{err}");
    assert_eq!(read_all(&dir), before);
}

#[test]
fn a_repair_keeps_the_call_log() {
    use super::calls::*;
    let parent = temp_dir();
    let (dir, _) = make_long(&parent);
    let rec = CallRecord {
        call: "c4.1".into(),
        phase: CALL_START.into(),
        turn: 4,
        args: "{\"x\":1}".into(),
        at: "t1".into(),
        tool: "spawn".into(),
        outcome: String::new(),
    };
    append_call(&dir, &rec).unwrap();
    break_node0(&dir);
    let _c = open_t(&dir, &TestClock::new(), |_| {});
    let recs = read_calls(&dir).unwrap();
    let dang = dangling_calls(&recs);
    assert_eq!((recs.len(), dang.len()), (1, 1));
    assert_eq!((dang[0].call.as_str(), dang[0].turn), ("c4.1", 4));
}

#[test]
fn the_zoom_cli_pages_and_hints() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    let ds = dir.to_string_lossy().into_owned();
    let (code, out, _) = run_cli(
        None,
        &["--data-dir", ds.as_str(), "zoom", "1+1", "--size", "4", "--offset", "4"],
    );
    assert_eq!((code, out.as_str()), (0, "word\n[bytes 4-8 of 9. More: offset 8]\n"));
    for bad in ["77+1", "1+2"] {
        let (code, _, err) = run_cli(None, &["--data-dir", ds.as_str(), "zoom", bad]);
        assert!(code == 1 && err.contains("The root nodes are 0+4, 4+1."), "{bad}: {err}");
    }
}

fn append_to(path: &Path, text: &str) {
    let mut t = std::fs::read(path).unwrap_or_default();
    t.extend_from_slice(text.as_bytes());
    std::fs::write(path, t).unwrap();
}

/// Puts a bad line where node 0+1 was; returns the tree file and the line.
fn break_node0(dir: &Path) -> (PathBuf, String) {
    let path = dir.join("tree/2026-10-08.jsonl");
    let data = std::fs::read_to_string(&path).unwrap();
    let mut lines: Vec<String> = data.split_inclusive('\n').map(String::from).collect();
    assert!(lines[0].contains("\"l\":0,\"i\":0"), "{}", lines[0]);
    let bad = "garbage {\n".to_string();
    lines[0] = bad.clone();
    std::fs::write(&path, lines.concat()).unwrap();
    (path, bad)
}
