//! The chat's tests, ported from Go's internal/foreman chat_test.go,
//! build_test.go, retry_test.go, fresh_test.go, appendfail_test.go,
//! perms_test.go, snapshot_test.go, viewtext_test.go and cli_test.go.

use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use super::chat::*;
use super::ctx::Ctx;
use super::fsutil::{self, clip, split, FailMode, FAIL_APPEND};
use super::order::NodeId;

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

#[test]
fn append_logs_word_for_word() {
    let dir = temp_dir().join("fm");
    let clk = TestClock::new();
    let f = Fake::default();
    let mut c = open_t(&dir, &clk, |_| {});
    must_append(&mut c, &f, KIND_USER, "hello <world> & \"you\"\nline two");
    clk.advance(Duration::from_secs(180));
    must_append(&mut c, &f, KIND_FOREMAN, "héllo");
    must_append(&mut c, &f, KIND_WORK, "[Mason] report: done");
    must_append(&mut c, &f, KIND_PAST, "an old chat line");
    must_append(&mut c, &f, KIND_NOTE, "a note");
    assert_eq!(c.count(), 5);
    let m = c.message(0).unwrap();
    assert_eq!(m.text, "hello <world> & \"you\"\nline two");
    assert_eq!(
        (m.kind.as_str(), m.size, m.date.as_str()),
        (KIND_USER, 30, "2026-10-08T23:58:00Z")
    );
    let day1 = std::fs::read_to_string(dir.join("main/2026-10-08.jsonl")).unwrap();
    assert_eq!(
        day1,
        "{\"i\":0,\"kind\":\"user\",\"text\":\"hello <world> & \\\"you\\\"\\nline two\",\"size\":30,\"date\":\"2026-10-08T23:58:00Z\"}\n"
    );
    assert_eq!(c.date(1).unwrap(), "2026-10-09T00:01:00Z");
    assert!(dir.join("main/2026-10-09.jsonl").exists());
}

#[test]
fn modes() {
    let dir = temp_dir().join("a").join("fm");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    must_append(&mut c, &Fake::default(), KIND_USER, "x");
    fn walk(d: &Path) {
        for e in std::fs::read_dir(d).unwrap().flatten() {
            let p = e.path();
            if p.is_dir() {
                assert_eq!(mode(&p), 0o700, "{}", p.display());
                walk(&p);
            } else {
                assert_eq!(mode(&p), 0o600, "{}", p.display());
            }
        }
    }
    assert_eq!(mode(&dir), 0o700);
    walk(&dir);
}

#[test]
fn open_tightens_an_open_dir() {
    let dir = temp_dir().join("fm");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755)).unwrap();
    let _c = open_t(&dir, &TestClock::new(), |_| {});
    assert_eq!(mode(&dir), 0o700);
}

#[test]
fn no_thoughts_and_no_unknown_kinds() {
    let dir = temp_dir();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    for k in ["thought", "thinking", "", "tools"] {
        assert!(
            c.append(k, "secret plans").1.is_some(),
            "kind {k:?} was logged"
        );
    }
    assert_eq!(c.count(), 0);
    assert_eq!(std::fs::read_dir(dir.join("main")).unwrap().count(), 0);
    assert_eq!(
        c.append("thought", "x").1.unwrap(),
        "foreman: \"thought\" is not a message kind"
    );
}

#[test]
fn long_text_splits_into_messages_in_a_row() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |cfg| cfg.split_bytes = 10);
    let ids = must_append(
        &mut c,
        &Fake::default(),
        KIND_USER,
        "0123456789abcdefghijéé",
    );
    assert_eq!(ids, vec![0, 1, 2]);
    let got: Vec<String> = ids.iter().map(|&i| c.message(i).unwrap().text).collect();
    for i in &ids {
        assert!(c.message(*i).unwrap().size <= 10);
    }
    assert_eq!(got.concat(), "0123456789abcdefghijéé");
    assert_eq!(got[2], "éé");
}

#[test]
fn split_and_clip_keep_runes() {
    assert_eq!(clip("aé", 2), "a");
    assert_eq!(split("ééé", 3), vec!["é", "é", "é"]);
    assert_eq!(split("", 3), vec![""]);
    assert_eq!(split("€", 2), vec!["€"]);
    assert_eq!(fsutil::to_valid_utf8(b"a\xffb"), "a\u{fffd}b");
    assert_eq!(fsutil::to_valid_utf8(b"a\xff\xfeb"), "a\u{fffd}b");
}

#[test]
fn the_tree_joins_short_lines_and_summarizes_long_ones() {
    let s = Fake::default();
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    must_append(&mut c, &s, KIND_USER, "one");
    must_append(&mut c, &s, KIND_FOREMAN, "two");
    assert_eq!(c.node(NodeId { l: 1, i: 0 }).unwrap().text, "one\ntwo");
    assert_eq!(
        s.compress.load(Ordering::SeqCst) + s.merge.load(Ordering::SeqCst),
        0
    );
    must_append(&mut c, &s, KIND_USER, &"x".repeat(600));
    assert_eq!(s.compress.load(Ordering::SeqCst), 1);
    assert_eq!(
        c.node(NodeId { l: 0, i: 2 }).unwrap().text,
        format!("c {}", "x".repeat(60))
    );
    must_append(&mut c, &s, KIND_USER, &"y".repeat(500));
    assert_eq!(s.merge.load(Ordering::SeqCst), 1);
    assert!(c.node(NodeId { l: 2, i: 0 }).is_some());
}

#[test]
fn a_summary_is_clipped_to_the_node_bound() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    must_append(&mut c, &Long, KIND_USER, &"z".repeat(1000));
    let n = c.node(NodeId { l: 0, i: 0 }).unwrap();
    assert_eq!(
        (n.size, n.text.len()),
        (DEFAULT_NODE_BYTES as i64, DEFAULT_NODE_BYTES)
    );
}

#[test]
fn the_view_stays_in_bounds_and_sawtooths() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), small_view);
    let f = Fake::default();
    let mut saw_batch = false;
    for i in 0..600 {
        let mut text = format!("message {i} {}", "w".repeat(i % 90));
        if i % 13 == 0 {
            text = "long ".repeat(120);
        }
        must_append(&mut c, &f, KIND_USER, &text);
        let size = c.view_size();
        assert!(size <= 2048, "after message {i} the view is {size} bytes");
        if i > 50 && size <= 1024 {
            saw_batch = true;
        }
        let mut next = 0;
        for id in c.view_lines() {
            assert_eq!(id.first(), next, "a gap after message {i}");
            next = id.last() + 1;
        }
        assert_eq!(next, c.count());
    }
    assert!(saw_batch);
}

#[test]
fn the_view_is_incremental() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), small_view);
    let f = Fake::default();
    let mut prev: Vec<NodeId> = Vec::new();
    for i in 0..300 {
        must_append(&mut c, &f, KIND_USER, &format!("m{i} {}", "q".repeat(40)));
        let cur = c.view_lines();
        for p in &prev {
            assert!(
                cur.iter()
                    .any(|q| q.first() <= p.first() && p.last() <= q.last() && q.l >= p.l),
                "message {i}: line {} left the view without a merge",
                p.name()
            );
        }
        prev = cur;
    }
}

fn script(i: usize) -> (&'static str, String) {
    let kinds = [KIND_USER, KIND_FOREMAN, KIND_WORK, KIND_FOREMAN];
    let mut text = format!("line {i}: {}", "r".repeat((i * 37) % 300));
    if i % 11 == 0 {
        text = "long text ".repeat(70);
    }
    (kinds[i % kinds.len()], text)
}

#[test]
fn a_restart_is_byte_for_byte() {
    let (a, b) = (temp_dir(), temp_dir());
    let (ka, kb) = (TestClock::new(), TestClock::new());
    let f = Fake::default();
    let mut ca = open_t(&a, &ka, small_view);
    let mut cb = open_t(&b, &kb, small_view);
    for i in 0..400 {
        let (k, text) = script(i);
        ka.advance(Duration::from_secs(60));
        kb.set(ka.now());
        must_append(&mut ca, &f, k, &text);
        must_append(&mut cb, &f, k, &text);
        if i % 37 == 36 {
            let before = std::fs::read(b.join("view.json")).unwrap();
            let render = cb.render();
            cb.close();
            cb = open_t(&b, &kb, small_view);
            assert_eq!(std::fs::read(b.join("view.json")).unwrap(), before);
            assert_eq!(cb.render(), render);
        }
    }
    assert_eq!(read_all(&a), read_all(&b));
}

#[test]
fn a_second_writer_is_refused() {
    let dir = temp_dir();
    let _c = open_t(&dir, &TestClock::new(), |_| {});
    let e = Chat::open(Config {
        dir: dir.clone(),
        ..Default::default()
    })
    .err()
    .unwrap();
    assert_eq!(e, ERR_LOCKED);
    assert!(Chat::open_read_only(&dir).is_ok());
}

#[test]
fn crash_recovery() {
    let (a, b) = (temp_dir(), temp_dir());
    let clk = TestClock::new();
    let f = Fake::default();
    let mut ca = open_t(&a, &clk, small_view);
    let mut cb = open_t(&b, &clk, small_view);
    let mut saved = Vec::new();
    for i in 0..120 {
        let (k, text) = script(i);
        must_append(&mut ca, &f, k, &text);
        must_append(&mut cb, &f, k, &text);
        if i == 99 {
            saved = std::fs::read(b.join("view.json")).unwrap();
        }
    }
    cb.close();
    drop(cb);
    std::fs::write(b.join("view.json"), &saved).unwrap();
    let day = b
        .join("main")
        .join(format!("{}.jsonl", day_text(clk.now())));
    let mut text = std::fs::read(&day).unwrap();
    text.extend_from_slice(b"{\"i\":120,\"kind\":\"user\",\"te");
    std::fs::write(&day, text).unwrap();
    let cb = open_t(&b, &clk, small_view);
    assert_eq!(cb.count(), 120);
    assert_eq!(read_all(&a), read_all(&b));
}

#[test]
fn a_summarizer_failure_keeps_the_message() {
    let dir = temp_dir();
    let s = Fake::default();
    s.fail.store(true, Ordering::SeqCst);
    let clk = TestClock::new();
    let mut c = open_t(&dir, &clk, |_| {});
    let (ids, err) = c.append(KIND_USER, &"v".repeat(700));
    assert!(err.is_none() && ids.len() == 1);
    assert_eq!(c.drain(&Ctx::background(), &s).unwrap_err(), ERR_FAKE);
    assert_eq!((c.count(), c.pending()), (1, 1));
    assert!(!c.render().contains("0+1|"));
    let tail = c.tail().unwrap();
    assert_eq!(tail.len(), 1);
    assert_eq!(tail[0].text, "v".repeat(700));
    s.fail.store(false, Ordering::SeqCst);
    must_append(&mut c, &s, KIND_USER, "next");
    assert_eq!(c.pending(), 0);
    assert!(!c.render().contains(UNBUILT));
    c.close();
    drop(c);
    let mut c2 = Chat::open(Config {
        dir: dir.clone(),
        now: Some(clk.clock()),
        ..Default::default()
    })
    .unwrap();
    c2.close();
}

#[test]
fn zoom_and_date() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    let f = Fake::default();
    for i in 0..5 {
        must_append(&mut c, &f, KIND_USER, &format!("msg {i}\nsecond line"));
    }
    assert_eq!(c.zoom(3, 1).unwrap(), "msg 3\nsecond line");
    assert_eq!(
        c.zoom(0, 4).unwrap(),
        "0+2|msg 0 second line msg 1 second line\n2+2|msg 2 second line msg 3 second line\n"
    );
    for (id, n) in [(0, 3), (1, 2), (4, 2), (0, 8), (5, 1), (-1, 1)] {
        assert!(c.zoom(id, n).is_err(), "Zoom({id},{n})");
    }
    assert_eq!(
        c.zoom(0, 8).unwrap_err(),
        "foreman: node 0+8: no such node; the chat has 5 messages. The root nodes are 0+4, 4+1."
    );
    assert_eq!(c.date(4).unwrap(), "2026-10-08T23:58:00Z");
    assert_eq!(c.date(5).unwrap_err(), "foreman: no message 5");
}

#[test]
fn day_reads_one_day() {
    let clk = TestClock::new();
    let f = Fake::default();
    let mut c = open_t(&temp_dir(), &clk, |_| {});
    must_append(&mut c, &f, KIND_USER, "late");
    clk.advance(Duration::from_secs(300));
    must_append(&mut c, &f, KIND_USER, "early");
    must_append(&mut c, &f, KIND_FOREMAN, "reply");
    let got = c.day("2026-10-09").unwrap();
    assert_eq!(got.len(), 2);
    assert_eq!((got[0].i, got[1].text.as_str()), (1, "reply"));
    assert!(c.day("2026-10-01").unwrap().is_empty());
    assert!(c.day("yesterday").is_err());
    assert!(c.day("2026-02-30").is_err());
}

#[test]
fn read_only_does_not_write() {
    let dir = temp_dir();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    must_append(&mut c, &Fake::default(), KIND_USER, "one");
    let day = dir.join("main/2026-10-08.jsonl");
    let mut t = std::fs::read(&day).unwrap();
    t.extend_from_slice(b"{\"i\":1,\"ki");
    std::fs::write(&day, t).unwrap();
    let before = read_all(&dir);
    let mut r = Chat::open_read_only(&dir).unwrap();
    assert_eq!(r.count(), 1);
    assert_eq!(r.render(), c.render());
    assert!(r.append(KIND_USER, "x").1.is_some());
    assert_eq!(read_all(&dir), before);
}

#[test]
fn a_batch_waits_for_unbuilt_parents() {
    // Through the public calls: four long lines with no parent built keep
    // the batch open; it goes on once the parents are built.
    let clk = TestClock::new();
    let mut c = open_t(&temp_dir(), &clk, |cfg| {
        cfg.view_max = 1000;
        cfg.view_min = 500;
    });
    let s = Fake::default();
    // Lines of 300 bytes each need no model; their parents (600 bytes) do.
    for _ in 0..4 {
        let (_, err) = c.append(KIND_USER, &"p".repeat(300));
        assert!(err.is_none());
    }
    // Message 1's parent needs the model, so the view waits at message 1.
    assert_eq!(c.view_lines().len(), 1);
    c.drain(&Ctx::background(), &s).unwrap();
    assert!(c.view_size() <= 1000);
    assert_eq!(c.pending(), 0);
}

#[test]
fn the_view_takes_a_message_only_once_its_nodes_are_written() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    assert!(c.append(KIND_USER, &long_text(0)).1.is_none());
    assert!(c.append(KIND_USER, "short").1.is_none());
    assert!(c.view_lines().is_empty());
    let tail = c.tail().unwrap();
    assert_eq!(tail.len(), 2);
    assert_eq!(tail[1].text, "short");
    let (job, err) = c.next();
    assert!(err.is_none());
    let job = job.unwrap();
    assert_eq!(job.id, NodeId { l: 0, i: 0 });
    c.commit(&job, "c line").unwrap();
    assert_eq!((c.view_lines().len(), c.pending()), (2, 0));
    assert!(c.tail().unwrap().is_empty());
}

fn long_text(i: usize) -> String {
    format!("{i}:{}", "w".repeat(600))
}

fn fill_long(c: &mut Chat, n: usize) {
    for i in 0..n {
        let text = if i % 3 == 1 {
            format!("short {i}")
        } else {
            long_text(i)
        };
        assert!(c.append(KIND_USER, &text).1.is_none());
    }
}

#[test]
fn nodes_are_built_one_at_a_time_in_queue_order() {
    struct Counting {
        inflight: AtomicUsize,
        max: AtomicUsize,
    }
    impl Counting {
        fn enter(&self) {
            let n = self.inflight.fetch_add(1, Ordering::SeqCst) + 1;
            self.max.fetch_max(n, Ordering::SeqCst);
            std::thread::sleep(Duration::from_micros(200));
            self.inflight.fetch_sub(1, Ordering::SeqCst);
        }
    }
    impl Summarizer for Counting {
        fn compress(&self, _: &Ctx, m: &Message, _: usize) -> Result<String, String> {
            self.enter();
            Ok(format!("c {}", clip(&m.text, 60)))
        }
        fn merge(&self, _: &Ctx, _: NodeId, l: &str, r: &str, _: usize) -> Result<String, String> {
            self.enter();
            Ok(format!("m {} | {}", clip(l, 40), clip(r, 40)))
        }
    }
    let mut c = open_t(&temp_dir(), &TestClock::new(), small_view);
    fill_long(&mut c, 90);
    let s = Counting {
        inflight: AtomicUsize::new(0),
        max: AtomicUsize::new(0),
    };
    c.drain(&Ctx::background(), &s).unwrap();
    assert_eq!(c.pending(), 0);
    assert_eq!(s.max.load(Ordering::SeqCst), 1);
}

#[test]
fn a_failed_job_is_the_front_job_again() {
    let clk = TestClock::new();
    let mut c = open_t(&temp_dir(), &clk, |_| {});
    for i in 0..3 {
        assert!(c.append(KIND_USER, &long_text(i)).1.is_none());
    }
    let job = c.next().0.unwrap();
    assert_eq!(job.id.i, 0);
    assert_eq!(c.next().0.unwrap().id, job.id);
    c.fail_err(&job, ERR_FAKE);
    let (j, e) = c.next();
    assert!(j.is_none() && e.as_deref() == Some(ERR_FAKE));
    clk.advance(Duration::from_secs(31));
    assert_eq!(c.next().0.unwrap().id, job.id);
    assert!(c.node(NodeId { l: 0, i: 1 }).is_none());
    assert_eq!(c.pending(), 4);
}

#[test]
fn drain_keeps_the_failed_nodes_pending_and_builds_them_after_their_wait() {
    let clk = TestClock::new();
    let mut c = open_t(&temp_dir(), &clk, |_| {});
    fill_long(&mut c, 9);
    let down = Fake::default();
    down.fail.store(true, Ordering::SeqCst);
    assert!(c.drain(&Ctx::background(), &down).is_err());
    assert!(c.pending() > 0);
    assert!(c.drain(&Ctx::background(), &Fake::default()).is_err());
    assert!(c.pending() > 0);
    clk.advance(Duration::from_secs(31));
    c.drain(&Ctx::background(), &Fake::default()).unwrap();
    assert_eq!(c.pending(), 0);
}

#[test]
fn a_failed_write_keeps_the_models_line_and_retries_only_the_write() {
    if crate::sys::getuid() == 0 {
        return;
    }
    let dir = temp_dir();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    assert!(c.append(KIND_USER, &long_text(0)).1.is_none());
    let job = c.next().0.unwrap();
    let tree = dir.join("tree");
    std::fs::set_permissions(&tree, std::fs::Permissions::from_mode(0o500)).unwrap();
    assert!(c.commit(&job, "the model line").is_err());
    std::fs::set_permissions(&tree, std::fs::Permissions::from_mode(0o700)).unwrap();
    let (j, e) = c.next();
    assert!(j.is_none() && e.is_none());
    assert_eq!(c.node(job.id).unwrap().text, "the model line");
    assert_eq!(c.pending(), 0);
}

/// Fails the messages whose text starts with a name in bad, and counts the
/// calls to each message.
struct Picky {
    bad: Mutex<Vec<String>>,
    calls: Mutex<HashMap<i64, usize>>,
}

impl Picky {
    fn new(bad: &[&str]) -> Picky {
        Picky {
            bad: Mutex::new(bad.iter().map(|s| s.to_string()).collect()),
            calls: Mutex::new(HashMap::new()),
        }
    }
    fn count(&self, i: i64) -> usize {
        *self.calls.lock().unwrap().get(&i).unwrap_or(&0)
    }
    fn heal(&self) {
        self.bad.lock().unwrap().clear();
    }
}

impl Summarizer for Picky {
    fn compress(&self, _: &Ctx, m: &Message, _: usize) -> Result<String, String> {
        *self.calls.lock().unwrap().entry(m.i).or_default() += 1;
        let head = m.text.split(':').next().unwrap_or("");
        if self.bad.lock().unwrap().iter().any(|b| b == head) {
            return Err(ERR_FAKE.into());
        }
        Ok(format!("c {}", clip(&m.text, 20)))
    }
    fn merge(&self, _: &Ctx, _: NodeId, l: &str, r: &str, _: usize) -> Result<String, String> {
        Ok(format!("m {} | {}", clip(l, 20), clip(r, 20)))
    }
}

fn log_longs(c: &mut Chat, n: usize) {
    for i in 0..n {
        assert!(c.append(KIND_USER, &long_text(i)).1.is_none());
    }
}

#[test]
fn a_failed_node_stops_the_nodes_behind_it_until_it_is_built() {
    let clk = TestClock::new();
    let mut c = open_t(&temp_dir(), &clk, |_| {});
    log_longs(&mut c, 6);
    let s = Picky::new(&["2"]);
    assert_eq!(c.drain(&Ctx::background(), &s).unwrap_err(), ERR_FAKE);
    for i in [3, 4, 5] {
        assert_eq!(s.count(i), 0);
    }
    assert_eq!(c.view_lines().iter().map(|v| v.count()).sum::<i64>(), 2);
    let tail = c.tail().unwrap();
    assert_eq!((tail.len(), tail[0].i), (4, 2));
    s.heal();
    clk.advance(Duration::from_secs(31));
    c.drain(&Ctx::background(), &s).unwrap();
    assert_eq!(c.pending(), 0);
    assert_eq!(c.view_lines().iter().map(|v| v.count()).sum::<i64>(), 6);
    for i in [3, 4, 5] {
        assert_eq!(s.count(i), 1);
    }
}

#[test]
fn a_failed_node_is_asked_again_only_after_its_wait() {
    let clk = TestClock::new();
    let mut c = open_t(&temp_dir(), &clk, |_| {});
    log_longs(&mut c, 1);
    let s = Picky::new(&["0"]);
    let steps: [(u64, usize); 10] = [
        (0, 1),
        (29, 1),
        (2, 2),
        (119, 2),
        (2, 3),
        (299, 3),
        (2, 4),
        (299, 4),
        (2, 5),
        (1, 5),
    ];
    for (k, (adv, calls)) in steps.iter().enumerate() {
        clk.advance(Duration::from_secs(*adv));
        for _ in 0..3 {
            assert_eq!(
                c.drain(&Ctx::background(), &s).unwrap_err(),
                ERR_FAKE,
                "step {k}"
            );
        }
        assert_eq!(s.count(0), *calls, "step {k}");
    }
}

#[test]
fn retry_in_says_when_the_next_try_is() {
    let clk = TestClock::new();
    let mut c = open_t(&temp_dir(), &clk, |_| {});
    assert!(c.retry_in().is_none());
    log_longs(&mut c, 1);
    let _ = c.drain(&Ctx::background(), &Picky::new(&["0"]));
    assert_eq!(c.retry_in(), Some(Duration::from_secs(30)));
    clk.advance(Duration::from_secs(10));
    assert_eq!(c.retry_in(), Some(Duration::from_secs(20)));
    clk.advance(Duration::from_secs(60));
    assert_eq!(c.retry_in(), Some(Duration::ZERO));
}

#[test]
fn an_append_asks_a_failed_node_again_at_once() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    log_longs(&mut c, 1);
    let s = Picky::new(&["0"]);
    let _ = c.drain(&Ctx::background(), &s);
    assert_eq!(s.count(0), 1);
    s.heal();
    assert!(c.append(KIND_USER, "short").1.is_none());
    c.drain(&Ctx::background(), &s).unwrap();
    assert_eq!((s.count(0), c.pending()), (2, 0));
}

#[test]
fn an_append_ends_the_wait_but_keeps_the_count_of_failures() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    log_longs(&mut c, 1);
    let s = Picky::new(&["0"]);
    let _ = c.drain(&Ctx::background(), &s);
    assert!(c.append(KIND_USER, "short").1.is_none());
    let _ = c.drain(&Ctx::background(), &s);
    assert_eq!(s.count(0), 2);
    assert_eq!(c.retry_in(), Some(Duration::from_secs(120)));
}

#[test]
fn a_stalled_compactor_loses_no_message_from_the_tail() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    log_longs(&mut c, 30);
    let _ = c.drain(&Ctx::background(), &Picky::new(&["0"]));
    assert_eq!(c.tail().unwrap().len(), 30);
}

// A broken history: coppice must always start.

const BROKEN_NAME: &str = "foreman.broken-20261008T235800Z";

fn make_history(parent: &Path) -> (PathBuf, PathBuf) {
    let dir = parent.join("foreman");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    for _ in 0..5 {
        must_append(&mut c, &f, KIND_USER, "old words");
    }
    c.close();
    (dir.clone(), dir.join("main/2026-10-08.jsonl"))
}

fn append_to(path: &Path, text: &str) {
    let mut t = std::fs::read(path).unwrap_or_default();
    t.extend_from_slice(text.as_bytes());
    std::fs::write(path, t).unwrap();
}

#[allow(clippy::type_complexity)]
fn corruptions() -> Vec<(&'static str, &'static str, Box<dyn Fn(&Path, &Path)>)> {
    vec![
        (
            "bad log line",
            "a bad line in main/2026-10-08.jsonl",
            Box::new(|_, day| append_to(day, "not json\n")),
        ),
        (
            "duplicate id",
            "message 0 is in the log twice",
            Box::new(|_, day| {
                let data = std::fs::read_to_string(day).unwrap();
                let first = data.split_inclusive('\n').next().unwrap().to_string();
                append_to(day, &first);
            }),
        ),
        (
            "id gap",
            "message 2 is missing",
            Box::new(|_, day| {
                let data = std::fs::read_to_string(day).unwrap();
                let lines: Vec<&str> = data.split_inclusive('\n').collect();
                let kept: String = lines
                    .iter()
                    .enumerate()
                    .filter(|(k, _)| *k != 2)
                    .map(|(_, l)| *l)
                    .collect();
                std::fs::write(day, kept).unwrap();
            }),
        ),
    ]
}

#[test]
fn a_broken_history_starts_fresh() {
    for (name, reason, apply) in corruptions() {
        let parent = temp_dir();
        let (dir, day) = make_history(&parent);
        apply(&dir, &day);
        let want = read_all(&dir);
        let clk = TestClock::new();
        let mut c = Chat::open(Config {
            dir: dir.clone(),
            now: Some(clk.clock()),
            ..Default::default()
        })
        .unwrap_or_else(|e| panic!("{name}: {e}"));
        let broken = parent.join(BROKEN_NAME);
        assert_eq!(
            c.notice(),
            format!(
                "Foreman history could not be read ({reason}); it was saved to {} and a new chat started.",
                broken.display()
            ),
            "{name}"
        );
        assert_eq!((c.count(), c.render().as_str()), (0, "<chat>\n</chat>\n"));
        assert_eq!(mode(&broken), 0o700);
        assert_eq!(read_all(&broken), want, "{name}");
        must_append(&mut c, &Fake::default(), KIND_USER, "a new start");
        assert_eq!(c.count(), 1);
    }
}

#[test]
fn a_broken_history_never_overwrites_an_earlier_one() {
    let parent = temp_dir();
    let (dir, day) = make_history(&parent);
    let earlier = parent.join(BROKEN_NAME);
    std::fs::create_dir_all(&earlier).unwrap();
    std::fs::write(earlier.join("keep"), "earlier").unwrap();
    append_to(&day, "not json\n");
    let clk = TestClock::new();
    let c = Chat::open(Config {
        dir,
        now: Some(clk.clock()),
        ..Default::default()
    })
    .unwrap();
    assert!(
        c.notice().contains(&format!("{BROKEN_NAME}-2 ")),
        "{}",
        c.notice()
    );
    assert_eq!(
        std::fs::read_to_string(earlier.join("keep")).unwrap(),
        "earlier"
    );
}

#[test]
fn a_healthy_open_has_no_notice() {
    let (dir, _) = make_history(&temp_dir());
    let c = open_t(&dir, &TestClock::new(), |_| {});
    assert_eq!((c.notice(), c.count()), ("", 5));
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

#[test]
fn an_io_error_leaves_the_history_in_place() {
    if crate::sys::getuid() == 0 {
        return;
    }
    let parent = temp_dir();
    let (dir, day) = make_history(&parent);
    std::fs::set_permissions(&day, std::fs::Permissions::from_mode(0)).unwrap();
    let res = Chat::open(Config {
        dir,
        ..Default::default()
    });
    std::fs::set_permissions(&day, std::fs::Permissions::from_mode(0o600)).unwrap();
    assert!(res.is_err(), "Open hid an I/O error");
    let names: Vec<String> = std::fs::read_dir(&parent)
        .unwrap()
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .collect();
    assert_eq!(names, ["foreman"]);
}

#[test]
fn a_move_aside_error_names_the_broken_path() {
    let parent = temp_dir();
    let (dir, day) = make_history(&parent);
    append_to(&day, "not json\n");
    FAIL_CHMOD_ASIDE.with(|c| c.set(true));
    let clk = TestClock::new();
    let res = Chat::open(Config {
        dir,
        now: Some(clk.clock()),
        ..Default::default()
    });
    FAIL_CHMOD_ASIDE.with(|c| c.set(false));
    let broken = parent.join(BROKEN_NAME);
    let e = res.err().unwrap();
    assert!(
        e.contains(&broken.to_string_lossy().into_owned()) && e.contains("injected"),
        "{e}"
    );
    assert!(broken.exists());
}

// Failed appends.

#[test]
fn a_failed_append_leaves_a_clean_log() {
    for fm in [FailMode::Write, FailMode::Sync] {
        let dir = temp_dir();
        let clk = TestClock::new();
        let f = Fake::default();
        let mut c = open_t(&dir, &clk, |_| {});
        must_append(&mut c, &f, KIND_USER, "one");
        let before = read_all(&dir.join("main"));
        FAIL_APPEND.with(|x| x.set(Some((0, fm))));
        let (ids, err) = c.append(KIND_USER, "two");
        assert!(
            ids.is_empty() && err.unwrap().contains("injected"),
            "{fm:?}"
        );
        assert_eq!(c.count(), 1);
        assert_eq!(read_all(&dir.join("main")), before);
        assert_eq!(must_append(&mut c, &f, KIND_USER, "three"), vec![1]);
        c.close();
        drop(c);
        let c2 = open_t(&dir, &clk, |_| {});
        assert_eq!(c2.count(), 2);
        assert_eq!(c2.message(1).unwrap().text, "three");
    }
}

#[test]
fn a_failed_split_append_keeps_the_view_whole() {
    let dir = temp_dir();
    let clk = TestClock::new();
    let mut c = open_t(&dir, &clk, |cfg| cfg.split_bytes = 4);
    FAIL_APPEND.with(|x| x.set(Some((1, FailMode::Write))));
    let (ids, err) = c.append(KIND_USER, "aaaabbbbcccc");
    FAIL_APPEND.with(|x| x.set(None));
    assert!(err.unwrap().contains("injected"));
    assert_eq!(ids.len(), 1);
    let coverage = |c: &Chat| c.view_lines().last().map_or(0, |id| id.last() + 1);
    assert_eq!(coverage(&c), c.count());
    must_append(&mut c, &Fake::default(), KIND_USER, "dddd");
    assert_eq!((coverage(&c), c.count()), (2, 2));
    c.close();
    drop(c);
    let _ = open_t(&dir, &clk, |cfg| cfg.split_bytes = 4);
}

#[test]
fn open_tightens_old_files_and_makes_dirs_0700() {
    let top = temp_dir();
    let dir = top.join("a/b/fm");
    std::fs::create_dir_all(dir.join("main")).unwrap();
    let day = dir.join("main/2026-10-08.jsonl");
    std::fs::write(
        &day,
        "{\"i\":0,\"kind\":\"user\",\"text\":\"x\",\"size\":1,\"date\":\"2026-10-08T23:58:00Z\"}\n",
    )
    .unwrap();
    std::fs::set_permissions(&day, std::fs::Permissions::from_mode(0o644)).unwrap();
    std::fs::write(dir.join("lock"), "").unwrap();
    std::fs::set_permissions(dir.join("lock"), std::fs::Permissions::from_mode(0o644)).unwrap();
    let _c = open_t(&dir, &TestClock::new(), |_| {});
    assert_eq!(mode(&day), 0o600);
    assert_eq!(mode(&dir.join("lock")), 0o600);
    let fresh = top.join("x/y/fm");
    let _c2 = open_t(&fresh, &TestClock::new(), |_| {});
    for p in [top.join("x"), top.join("x/y"), fresh] {
        assert_eq!(mode(&p), 0o700, "{}", p.display());
    }
}

#[test]
fn a_reader_sees_a_snapshot_while_the_writer_appends() {
    use std::cell::RefCell;
    use std::rc::Rc;
    for stage in ["view", "main"] {
        let dir = temp_dir();
        let w = Rc::new(RefCell::new(open_t(&dir, &TestClock::new(), |_| {})));
        let f = Rc::new(Fake::default());
        for _ in 0..7 {
            must_append(&mut w.borrow_mut(), &*f, KIND_USER, "before");
        }
        let (want_render, want_count) = (w.borrow().render(), w.borrow().count());
        let fired = Rc::new(RefCell::new(0));
        let (w2, f2, fired2) = (w.clone(), f.clone(), fired.clone());
        LOAD_HOOK.with(|h| {
            *h.borrow_mut() = Some(Box::new(move |s: &str| {
                if s == stage {
                    *fired2.borrow_mut() += 1;
                    for _ in 0..7 {
                        must_append(&mut w2.borrow_mut(), &*f2, KIND_USER, "during");
                    }
                }
            }))
        });
        let r = Chat::open_read_only(&dir);
        LOAD_HOOK.with(|h| *h.borrow_mut() = None);
        let r = r.unwrap();
        assert!(*fired.borrow() > 0);
        assert_eq!(r.count(), want_count, "{stage}");
        assert_eq!(r.render(), want_render, "{stage}");
    }
}

#[test]
fn view_text_leaves_out_the_skipped_message_and_nothing_else() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    for w in ["one", "two", "three"] {
        assert!(c.append(KIND_USER, w).1.is_none());
    }
    let id = |s: &str| s.to_string();
    let all = c.view_text(id, -1);
    assert_eq!(all.len(), 3);
    assert_eq!(all[1].text, "1+1|two\n");
    let got = c.view_text(id, 1);
    assert_eq!(got.len(), 2);
    assert_eq!(
        (got[0].text.as_str(), got[1].text.as_str()),
        ("0+1|one\n", "2+1|three\n")
    );
    assert_eq!(
        c.view_size(),
        all.iter().map(|l| l.text.len()).sum::<usize>()
    );
}

// The command line.

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
    must_append(&mut c, &f, KIND_FOREMAN, "second");
    let before = read_all(&dir);
    assert_eq!(
        run_cli(None, &["--data-dir", d, "log"]),
        (
            0,
            "0 2026-10-08T23:58:00Z user|first line\n1 2026-10-08T23:58:00Z foreman|second\n"
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
    assert!(code == 0 && out.contains("coppice foreman"));
}

#[test]
fn the_cli_uses_the_coppice_data_dir() {
    let top = temp_dir();
    let mut c = open_t(&top.join("foreman"), &TestClock::new(), |_| {});
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
fn an_act_is_known_after_a_reopen() {
    let dir = temp_dir().join("fm");
    let clk = TestClock::new();
    let f = Fake::default();
    let mut c = open_t(&dir, &clk, |_| {});
    let (ids, err) = c.append_act(KIND_USER, "build the thing", None, "act-1");
    assert!(err.is_none() && ids.len() == 1);
    c.drain(&Ctx::background(), &f).unwrap();
    assert_eq!(c.act_msg("act-1"), Some(ids[0]));
    assert_eq!(c.act_msg("act-2"), None);
    drop(c);
    let c2 = open_t(&dir, &clk, |_| {});
    assert_eq!(c2.act_msg("act-1"), Some(ids[0]));
    let m = c2.message(ids[0]).unwrap();
    assert_eq!((m.act.as_str(), m.kind.as_str()), ("act-1", KIND_USER));
    let day = std::fs::read_to_string(dir.join("main/2026-10-08.jsonl")).unwrap();
    assert!(day.ends_with(",\"date\":\"2026-10-08T23:58:00Z\",\"act\":\"act-1\"}\n"), "{day}");
}

#[test]
fn the_act_rides_on_every_part_of_a_split_text() {
    let dir = temp_dir().join("fm");
    let clk = TestClock::new();
    let mut c = open_t(&dir, &clk, |cfg| cfg.split_bytes = 100);
    let text = "w".repeat(250);
    let (ids, err) = c.append_act(KIND_USER, &text, None, "act-long");
    assert!(err.is_none() && ids.len() == 3);
    assert_eq!(c.act_msg("act-long"), Some(ids[2]));
    for &id in &ids {
        assert_eq!(c.message(id).unwrap().act, "act-long");
    }
    assert!(c.act_matches("act-long", &text));
    assert!(!c.act_matches("act-long", &format!("{text}x")));
    assert!(!c.act_matches("act-long", "other"));
    drop(c);
    let c2 = open_t(&dir, &clk, |cfg| cfg.split_bytes = 100);
    assert_eq!(c2.act_msg("act-long"), Some(ids[2]));
    assert!(c2.act_matches("act-long", &text));
}

#[test]
fn an_act_is_only_for_the_words_it_first_carried() {
    let dir = temp_dir().join("fm");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let (_, err) = c.append_act(KIND_USER, "first sentence", None, "a1");
    assert!(err.is_none());
    assert!(c.act_matches("a1", "first sentence"));
    assert!(!c.act_matches("a1", "second sentence"));
    assert!(!c.act_matches("nope", "first sentence"));
}

#[test]
fn a_message_with_no_act_writes_no_act_key() {
    let dir = temp_dir().join("fm");
    let clk = TestClock::new();
    let mut c = open_t(&dir, &clk, |_| {});
    assert!(c.append(KIND_USER, "plain").1.is_none());
    let day = std::fs::read_to_string(dir.join("main/2026-10-08.jsonl")).unwrap();
    assert!(!day.contains("act"), "{day}");
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

// U3: a bad tree line or a bad view.json is repaired, not moved aside.

const STAMP: &str = "20261008T235800Z";

/// make_history with a long first message, so its node needs the model.
fn make_long(parent: &Path) -> (PathBuf, PathBuf) {
    let dir = parent.join("foreman");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    must_append(&mut c, &f, KIND_USER, &"x".repeat(600));
    for _ in 0..4 {
        must_append(&mut c, &f, KIND_USER, "old words");
    }
    c.close();
    (dir.clone(), dir.join("main/2026-10-08.jsonl"))
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

fn open_now(dir: &Path) -> Chat {
    Chat::open(Config {
        dir: dir.to_path_buf(),
        now: Some(TestClock::new().clock()),
        ..Default::default()
    })
    .unwrap()
}

#[test]
fn a_bad_tree_line_is_repaired_not_moved_aside() {
    let parent = temp_dir();
    let (dir, _) = make_long(&parent);
    let want_view = std::fs::read_to_string(dir.join("view.json")).unwrap();
    let (path, bad) = break_node0(&dir);
    let mut c = open_now(&dir);
    assert_eq!(std::fs::read_dir(&parent).unwrap().count(), 1);
    assert_eq!(c.count(), 5);
    let saved = format!("{}.broken-{STAMP}", path.display());
    assert_eq!(std::fs::read_to_string(&saved).unwrap(), bad);
    assert_eq!(mode(Path::new(&saved)), 0o600);
    assert_eq!(
        c.notice(),
        format!(
            "Foreman tree repaired: 1 bad line in tree/2026-10-08.jsonl was copied to {saved} and removed. The nodes those lines held will be built again from the log."
        )
    );
    assert!(!std::fs::read_to_string(&path).unwrap().contains("garbage"));
    assert_eq!(c.pending(), 1);
    c.drain(&Ctx::background(), &Fake::default()).unwrap();
    assert!(c.node(NodeId { l: 0, i: 0 }).is_some());
    assert_eq!(c.pending(), 0);
    assert_eq!(c.view_json(), want_view);
    c.close();
    let c2 = open_now(&dir);
    assert_eq!((c2.notice(), c2.pending()), ("", 0));
}

#[test]
fn a_node_outside_the_log_is_repaired_too() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    append_to(
        &dir.join("tree/2026-10-08.jsonl"),
        "{\"l\":3,\"i\":0,\"text\":\"x\",\"size\":1}\n",
    );
    let c = open_now(&dir);
    assert!(
        c.notice().starts_with("Foreman tree repaired: 1 bad line"),
        "{}",
        c.notice()
    );
    assert_eq!((c.count(), std::fs::read_dir(&parent).unwrap().count()), (5, 1));
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
fn a_bad_view_is_moved_alone_and_rebuilt() {
    for (name, body) in [
        ("not json", "[["),
        (
            "outside the log",
            "{\"count\":99,\"batch\":false,\"lines\":[[0,0]]}\n",
        ),
    ] {
        let parent = temp_dir();
        let (dir, _) = make_history(&parent);
        let good = std::fs::read_to_string(dir.join("view.json")).unwrap();
        std::fs::write(dir.join("view.json"), body).unwrap();
        let c = open_now(&dir);
        assert_eq!(
            (std::fs::read_dir(&parent).unwrap().count(), c.count()),
            (1, 5),
            "{name}"
        );
        let saved = dir.join(format!("view.json.broken-{STAMP}"));
        assert_eq!(std::fs::read_to_string(&saved).unwrap(), body, "{name}");
        assert_eq!(std::fs::read_to_string(dir.join("view.json")).unwrap(), good);
        assert_eq!(
            c.notice(),
            format!(
                "Foreman view.json could not be used. It was copied to {} and the view was rebuilt from the tree.",
                saved.display()
            ),
            "{name}"
        );
    }
}

#[test]
fn a_reader_rebuilds_a_bad_view_in_memory() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    let good = std::fs::read_to_string(dir.join("view.json")).unwrap();
    std::fs::write(dir.join("view.json"), "[[").unwrap();
    let before = read_all(&dir);
    let c = Chat::open_read_only(&dir).unwrap();
    assert_eq!(c.view_json(), good);
    assert!(c.notice().contains("view.json could not be used"), "{}", c.notice());
    assert_eq!(read_all(&dir), before);
}

#[test]
fn a_failed_view_rebuild_falls_back_to_move_aside() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    std::fs::write(dir.join("view.json"), "[[").unwrap();
    FAIL_VIEW_WRITE.with(|c| c.set(true));
    let c = open_now(&dir);
    FAIL_VIEW_WRITE.with(|c| c.set(false));
    assert!(
        c.notice().contains("view.json could not be rebuilt"),
        "{}",
        c.notice()
    );
    assert_eq!(c.count(), 0);
    assert!(parent.join(BROKEN_NAME).exists());
}

#[test]
fn a_requeued_node_keeps_its_order() {
    let parent = temp_dir();
    let (dir, _) = make_long(&parent);
    break_node0(&dir);
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let (ids, err) = c.append(KIND_USER, "live words");
    assert!(err.is_none() && ids.len() == 1);
    assert_eq!(
        c.pending_ids()[..2],
        [NodeId { l: 0, i: 0 }, NodeId { l: 0, i: 5 }]
    );
    let tail = c.tail().unwrap();
    assert_eq!((tail.len(), tail[0].text.as_str()), (1, "live words"));
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
fn zoom_pages_and_hints_the_roots() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    let f = Fake::default();
    for _ in 0..5 {
        must_append(&mut c, &f, KIND_USER, "0123456789abcdefghij");
    }
    assert_eq!(
        c.zoom_page(2, 1, 0, 8).unwrap(),
        "01234567\n[bytes 0-8 of 20. More: offset 8]"
    );
    assert_eq!(
        c.zoom_page(2, 1, 8, 8).unwrap(),
        "89abcdef\n[bytes 8-16 of 20. More: offset 16]"
    );
    assert_eq!(
        c.zoom_page(2, 1, 16, 8).unwrap(),
        "ghij\n[bytes 16-20 of 20. End.]"
    );
    assert_eq!(c.zoom_page(2, 1, 0, 0).unwrap(), "0123456789abcdefghij");
    assert!(c.zoom_page(2, 1, 21, 8).unwrap_err().contains("past the end"));
    assert!(c.zoom_page(2, 1, -1, 8).is_err());
    must_append(&mut c, &f, KIND_USER, "ééééé");
    assert_eq!(
        c.zoom_page(5, 1, 0, 3).unwrap(),
        "é\n[bytes 0-2 of 10. More: offset 2]"
    );
    for (id, n) in [(9, 1), (0, 3), (1, 2), (0, 64)] {
        let e = c.zoom(id, n).unwrap_err();
        assert!(e.contains("The root nodes are 0+4, 4+2."), "Zoom({id},{n}) = {e}");
    }
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

// U3 fix round 1.

use super::fsutil::FAIL_TREE_APPEND;
use super::order::{merge_at, most_due, PLANNER_MERGES};

fn open_view_bad(dir: &Path, reader: bool) -> Chat {
    if reader {
        Chat::open_read_only(dir).unwrap()
    } else {
        open_now(dir)
    }
}

// I-1: a bad view.json together with a cut node that needs the model must
// not leave an empty view and the whole log as the tail.
#[test]
fn a_bad_view_with_a_cut_model_node_still_rebuilds_the_view() {
    for reader in [false, true] {
        let parent = temp_dir();
        let (dir, _) = make_long(&parent);
        break_node0(&dir);
        std::fs::write(dir.join("view.json"), "[[").unwrap();
        let c = open_view_bad(&dir, reader);
        let tail = c.tail().unwrap();
        assert_eq!(
            (c.view_lines().len(), tail.len()),
            (5, 0),
            "reader={reader}: view lines and tail"
        );
        if !reader {
            assert!(dir.join("view.json").exists());
        }
    }
}

#[test]
fn a_bad_view_keeps_an_unbuilt_tail_word_for_word() {
    let parent = temp_dir();
    let dir = parent.join("foreman");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    for _ in 0..4 {
        must_append(&mut c, &f, KIND_USER, "short words");
    }
    assert!(c.append(KIND_USER, &"y".repeat(600)).1.is_none());
    c.close();
    std::fs::write(dir.join("view.json"), "[[").unwrap();
    let c2 = open_now(&dir);
    assert_eq!((c2.view_lines().len(), c2.tail().unwrap().len()), (4, 1));
}

// M-1: only a failed save of the rebuilt view.json moves the history.
#[test]
fn a_bad_view_and_a_tree_write_error_does_not_move_the_history() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    let tree = dir.join("tree/2026-10-08.jsonl");
    let data = std::fs::read_to_string(&tree).unwrap();
    let lines: Vec<&str> = data.split_inclusive('\n').collect();
    std::fs::write(&tree, lines[..lines.len() - 1].concat()).unwrap();
    std::fs::write(dir.join("view.json"), "[[").unwrap();
    FAIL_TREE_APPEND.with(|c| c.set(true));
    let res = Chat::open(Config {
        dir,
        now: Some(TestClock::new().clock()),
        ..Default::default()
    });
    FAIL_TREE_APPEND.with(|c| c.set(false));
    assert!(res.is_err(), "Open hid a tree write error");
    assert_eq!(std::fs::read_dir(&parent).unwrap().count(), 1);
}

// M-2: a reader that rejects view.json loads the whole log.
#[test]
fn a_reader_with_a_bad_view_count_reads_the_whole_log() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    std::fs::write(
        dir.join("view.json"),
        "{\"count\":0,\"batch\":false,\"lines\":[[0,3]]}\n",
    )
    .unwrap();
    let c = Chat::open_read_only(&dir).unwrap();
    assert_eq!((c.count(), c.view_lines().len()), (5, 5));
}

// M-4 and M-9a: null, a wrong key case and a missing key are bad lines, and
// the note counts them in the plural.
#[test]
fn strict_tree_lines_and_the_plural_note() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    let tree = dir.join("tree/2026-10-08.jsonl");
    let before = std::fs::read_to_string(&tree).unwrap();
    append_to(
        &tree,
        "null\n{\"L\":0,\"I\":3,\"text\":\"x\",\"size\":1}\n{\"l\":0,\"i\":3,\"text\":\"x\"}\n",
    );
    let c = open_now(&dir);
    assert!(
        c.notice()
            .contains("3 bad lines in tree/2026-10-08.jsonl were copied to "),
        "{}",
        c.notice()
    );
    assert_eq!(std::fs::read_to_string(&tree).unwrap(), before);
}

#[test]
fn two_bad_tree_files_make_one_note() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    append_to(&dir.join("tree/2026-10-08.jsonl"), "{\n");
    std::fs::write(dir.join("tree/2026-10-07.jsonl"), "x\n").unwrap();
    let c = open_now(&dir);
    let n = c.notice();
    let a = n.find("1 bad line in tree/2026-10-07.jsonl was copied");
    let b = n.find("1 bad line in tree/2026-10-08.jsonl was copied");
    assert!(
        a.is_some() && b > a && n.matches("Foreman tree repaired").count() == 1,
        "{n}"
    );
}

// M-6: a torn last tree line gets a .broken copy.
#[test]
fn a_torn_tree_tail_is_copied_before_it_is_cut() {
    let parent = temp_dir();
    let (dir, _) = make_history(&parent);
    let tree = dir.join("tree/2026-10-08.jsonl");
    append_to(&tree, "{\"l\":0,\"i\":");
    let c = open_now(&dir);
    let saved = format!("{}.broken-{STAMP}", tree.display());
    assert_eq!(std::fs::read_to_string(&saved).unwrap_or_default(), "{\"l\":0,\"i\":");
    assert!(
        c.notice().contains(&format!(
            "1 torn last line in tree/2026-10-08.jsonl was copied to {saved} and removed."
        )),
        "{}",
        c.notice()
    );
}

// M-8: an offset inside the last rune is past the end.
#[test]
fn a_zoom_offset_inside_the_last_rune_is_past_the_end() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), |_| {});
    must_append(&mut c, &Fake::default(), KIND_USER, "12345678é");
    let e = c.zoom_page(0, 1, 9, 4).unwrap_err();
    assert!(e.contains("past the end"), "{e}");
}

// M-9c: the view's batch merge goes through the planner and gives the lines
// of the old MostDue loop.
#[test]
fn the_view_step_uses_the_planner_and_gives_the_old_lines() {
    let mut c = open_t(&temp_dir(), &TestClock::new(), small_view);
    let f = Fake::default();
    let before = PLANNER_MERGES.with(|p| p.get());
    let mut reference: Vec<NodeId> = Vec::new();
    let mut batch = false;
    for t in 0..300i64 {
        must_append(
            &mut c,
            &f,
            KIND_USER,
            &format!("message {t} {}", "w".repeat((7 * t % 50) as usize)),
        );
        reference.push(NodeId { l: 0, i: t });
        let len = |id: NodeId| match c.node(id) {
            Some(n) => format!("{}|{}\n", id.name(), flat(&n.text)).len(),
            None => format!("{}|{}\n", id.name(), UNBUILT).len(),
        };
        let mut size: usize = reference.iter().map(|&id| len(id)).sum();
        if size > 2048 {
            batch = true;
        }
        let built = |id: NodeId| c.node(id).is_some() && id.last() <= t;
        while batch {
            if size <= 1024 {
                batch = false;
                break;
            }
            let Some(k) = most_due(&reference, t + 1, built) else {
                break;
            };
            size -= len(reference[k]) + len(reference[k + 1]);
            merge_at(&mut reference, k);
            size += len(reference[k]);
        }
        assert_eq!(c.view_lines(), reference, "message {t}");
    }
    assert!(
        PLANNER_MERGES.with(|p| p.get()) > before,
        "the view step made no merge through the planner"
    );
}

// U3 fix round 2.

use super::chat::FAIL_VIEW_WRITE as FAIL_VIEW;

/// A history whose message 0 is short (a node without the model) and message
/// 1 is long (a node that needs the model), with both leaf lines broken in
/// the tree file.
fn make_cut_two(parent: &Path) -> PathBuf {
    let dir = parent.join("foreman");
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let f = Fake::default();
    must_append(&mut c, &f, KIND_USER, "old words");
    must_append(&mut c, &f, KIND_USER, &"x".repeat(600));
    for _ in 0..3 {
        must_append(&mut c, &f, KIND_USER, "old words");
    }
    c.close();
    let path = dir.join("tree/2026-10-08.jsonl");
    let data = std::fs::read_to_string(&path).unwrap();
    let mut cut = 0;
    let lines: Vec<String> = data
        .split_inclusive('\n')
        .map(|l| {
            if l.contains("\"l\":0,\"i\":0,") || l.contains("\"l\":0,\"i\":1,") {
                cut += 1;
                "garbage {\n".to_string()
            } else {
                l.to_string()
            }
        })
        .collect();
    assert_eq!(cut, 2);
    std::fs::write(&path, lines.concat()).unwrap();
    dir
}

fn open_now_res(dir: &Path) -> Result<Chat, String> {
    Chat::open(Config {
        dir: dir.to_path_buf(),
        now: Some(TestClock::new().clock()),
        ..Default::default()
    })
}

// N-1: a tree error and a view save error together return the tree error.
// The history does not move.
#[test]
fn a_tree_error_with_a_view_save_error_does_not_move_the_history() {
    let parent = temp_dir();
    let dir = make_cut_two(&parent);
    std::fs::write(dir.join("view.json"), "[[").unwrap();
    FAIL_TREE_APPEND.with(|c| c.set(true));
    FAIL_VIEW.with(|c| c.set(true));
    let res = open_now_res(&dir);
    FAIL_TREE_APPEND.with(|c| c.set(false));
    FAIL_VIEW.with(|c| c.set(false));
    assert!(res.is_err(), "Open hid a tree write error");
    assert_eq!(std::fs::read_dir(&parent).unwrap().count(), 1);
}

// N-2: a failed open leaves the bad view.json in place, so the next open
// repairs it again, skips the cut nodes and says so.
#[test]
fn a_failed_open_keeps_the_bad_view_for_the_next_open() {
    let parent = temp_dir();
    let dir = make_cut_two(&parent);
    let view = dir.join("view.json");
    std::fs::write(&view, "[[").unwrap();
    FAIL_TREE_APPEND.with(|c| c.set(true));
    FAIL_VIEW.with(|c| c.set(true));
    let res = open_now_res(&dir);
    FAIL_TREE_APPEND.with(|c| c.set(false));
    FAIL_VIEW.with(|c| c.set(false));
    assert!(res.is_err(), "Open hid a tree write error");
    assert_eq!(std::fs::read_to_string(&view).unwrap(), "[[");
    let c = open_now(&dir);
    assert_eq!(
        (c.view_lines().len(), c.tail().unwrap().len()),
        (5, 0),
        "view lines and tail"
    );
    assert!(c.notice().contains("view.json could not be used"), "{}", c.notice());
    let copies = std::fs::read_dir(&dir)
        .unwrap()
        .flatten()
        .filter(|e| e.file_name().to_string_lossy().starts_with("view.json.broken-"))
        .count();
    assert!(copies > 0, "no copy of the bad view.json");
}

// N-2: no view.json and a cut model node: the replay steps past it.
#[test]
fn a_missing_view_with_a_cut_model_node_still_rebuilds_the_view() {
    for reader in [false, true] {
        let parent = temp_dir();
        let (dir, _) = make_long(&parent);
        break_node0(&dir);
        std::fs::remove_file(dir.join("view.json")).unwrap();
        let c = open_view_bad(&dir, reader);
        let tail = c.tail().unwrap();
        assert_eq!(
            (c.view_lines().len(), tail.len()),
            (5, 0),
            "reader={reader}: view lines and tail"
        );
    }
}

// N-3: an extra key that differs from one of the four only in case is a bad
// line, as in Go; so are null and a lone surrogate escape.
#[test]
fn hand_edited_tree_lines_are_bad_as_in_go() {
    for bad in [
        r#"{"l":null,"i":3,"text":"x","size":1}"#,
        r#"{"l":0,"i":3,"text":"x","size":1,"TEXT":"y"}"#,
        "{\"l\":0,\"i\":3,\"text\":\"x\",\"size\":1,\"\u{17f}ize\":2}",
        r#"{"l":0,"i":3,"text":"\ud800","size":1}"#,
        r#"{"l":0,"i":3,"text":"a\udc00b","size":1}"#,
    ] {
        assert!(super::chat::parse_node(bad.as_bytes()).is_none(), "{bad}");
    }
    for good in [
        r#"{"l":0,"i":3,"text":"😀","size":1}"#,
        r#"{"l":0,"i":3,"text":"a\\ud800","size":1,"other":1}"#,
    ] {
        assert!(super::chat::parse_node(good.as_bytes()).is_some(), "{good}");
    }
}
