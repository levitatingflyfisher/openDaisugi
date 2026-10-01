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
    let d = std::env::temp_dir().join(format!(
        "coppice-rs-fm-{}-{}",
        std::process::id(),
        N.fetch_add(1, Ordering::SeqCst)
    ));
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
        "foreman: node 0+8: no such node; the chat has 5 messages"
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
        (
            "bad tree line",
            "a bad line in tree/2026-10-08.jsonl",
            Box::new(|dir, _| append_to(&dir.join("tree/2026-10-08.jsonl"), "{\n")),
        ),
        (
            "node outside the log",
            "a bad line in tree/2026-10-08.jsonl",
            Box::new(|dir, _| {
                append_to(
                    &dir.join("tree/2026-10-08.jsonl"),
                    "{\"l\":3,\"i\":0,\"text\":\"x\",\"size\":1}\n",
                )
            }),
        ),
        (
            "view outside the log",
            "view.json does not match the log",
            Box::new(|dir, _| {
                std::fs::write(
                    dir.join("view.json"),
                    "{\"count\":99,\"batch\":false,\"lines\":[[0,0]]}\n",
                )
                .unwrap()
            }),
        ),
        (
            "bad view",
            "view.json is not valid JSON",
            Box::new(|dir, _| std::fs::write(dir.join("view.json"), "[[").unwrap()),
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
