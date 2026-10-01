//! The server's import tests (FM-R-71 to FM-R-77), ported from Go's
//! server/foreman_import_test.go. Fake transcripts only, in a scratch home:
//! two Claude Code messages and one Codex message that count.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use super::foreman_chat::{ForemanChat, ForemanConfig};
use super::{Config, Server};
use crate::foreman::ctx::Ctx;
use crate::foreman::import::{read_import_state, ImportOpts};

static N: AtomicUsize = AtomicUsize::new(0);

fn scratch(tag: &str) -> PathBuf {
    let d = std::env::temp_dir().join(format!(
        "coppice-rs-fmi-{}-{}-{tag}",
        std::process::id(),
        N.fetch_add(1, Ordering::SeqCst)
    ));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
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

fn import_home(dir: &Path) -> String {
    let home = dir.join("home");
    let w = |p: &str, lines: &[&str]| {
        let p = home.join(p);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, lines.join("\n") + "\n").unwrap();
    };
    w(
        ".claude/projects/p/s1.jsonl",
        &[
            r#"{"type":"user","uuid":"u1","sessionId":"s1","timestamp":"2026-03-01T10:00:00.000Z","cwd":"/w/proj","message":{"role":"user","content":"hello"}}"#,
            r#"{"type":"assistant","uuid":"a1","sessionId":"s1","timestamp":"2026-03-01T10:00:05.000Z","cwd":"/w/proj","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}"#,
        ],
    );
    w(
        ".codex/sessions/2026/03/05/rollout-x.jsonl",
        &[
            r#"{"timestamp":"2026-03-05T08:00:00.000Z","type":"session_meta","payload":{"id":"c1","cwd":"/w/o"}}"#,
            r#"{"timestamp":"2026-03-05T08:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"write a test"}]}}"#,
        ],
    );
    home.to_string_lossy().into_owned()
}

fn server(dir: &Path, chat: bool, home: &str) -> Arc<Server> {
    crate::foreman::fsutil::NO_SYNC.store(true, Ordering::Relaxed);
    let f = ForemanConfig {
        mode: if chat { "chat".into() } else { String::new() },
        claude_bin: format!("{}/no-claude", dir.display()),
        sprig_bin: format!("{}/no-sprig", dir.display()),
        daisugi_bin: format!("{}/no-daisugi", dir.display()),
        self_cmd: vec!["/bin/false".into(), "foreman".into()],
        import_home: home.to_string(),
        import_poll: Duration::from_millis(20),
        ..Default::default()
    };
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

fn kind_count(fc: &ForemanChat, kind: &str) -> usize {
    log_of(fc).iter().filter(|(k, _)| k == kind).count()
}

fn import_call(s: &Arc<Server>, action: &str) -> serde_json::Value {
    let r = s.floor_import(&req(&format!(
        r#"{{"id":"1","cmd":"floor.import","action":"{action}"}}"#
    )));
    assert!(r.ok, "{:?}", r.error);
    r.result.unwrap()
}

#[test]
fn an_import_is_offered_once_and_never_started_by_the_server() {
    let dir = scratch("offer");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    let text = log_of(&fc)[0].1.clone();
    assert!(
        text.contains("2 past chats (about 3 messages)") && text.contains("coppice foreman import"),
        "{text}"
    );
    std::thread::sleep(Duration::from_millis(200));
    assert_eq!(kind_count(&fc, "past"), 0, "imported without being asked");
    let st = read_import_state(&s.foreman_log_dir()).unwrap();
    assert_eq!((st.word(), st.offered), ("off", true));
    s.close();
    // A second start does not offer again.
    let s2 = server(&dir, true, &home);
    let fc2 = s2.ensure_chat();
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(kind_count(&fc2, "note"), 1);
    s2.close();
}

#[test]
fn no_offer_when_there_are_no_old_chats() {
    let dir = scratch("none");
    let empty = dir.join("empty");
    std::fs::create_dir_all(&empty).unwrap();
    let s = server(&dir, true, &empty.to_string_lossy());
    let fc = s.ensure_chat();
    std::thread::sleep(Duration::from_millis(300));
    assert!(log_of(&fc).is_empty());
    s.close();
}

#[test]
fn floor_import_accept_feeds_the_queue_and_pause_holds() {
    let dir = scratch("accept");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    import_call(&s, "accept");
    import_call(&s, "pause");
    std::thread::sleep(Duration::from_millis(300));
    let before = kind_count(&fc, "past");
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(
        kind_count(&fc, "past"),
        before,
        "messages grew while paused"
    );
    let r = import_call(&s, "resume");
    assert_eq!(r["paused"], false);
    wait_for("the import", || {
        kind_count(&fc, "past") == 3 && fc.lock().as_ref().unwrap().pending() == 0
    });
    let d = s.foreman_log_dir();
    wait_for("done", || {
        let st = read_import_state(&d).unwrap();
        st.word() == "done" && st.done == 3 && st.total == 3
    });
    // The floor's chat leaves past messages out unless asked.
    let r = s.floor_chat_log(&req(r#"{"id":"4","cmd":"floor.chat"}"#));
    // The offer note may be there; no past message is.
    for m in r.result.unwrap()["messages"].as_array().unwrap() {
        assert_eq!(m["role"], "note", "{m}");
    }
    s.close();
}

#[test]
fn import_waits_while_a_turn_runs() {
    let dir = scratch("turn");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    s.turns_lock().running = true;
    import_call(&s, "accept");
    std::thread::sleep(Duration::from_millis(400));
    assert_eq!(kind_count(&fc, "past"), 0, "imported while a turn ran");
    s.turns_lock().running = false;
    let d = s.foreman_log_dir();
    wait_for("the import after the turn", || {
        read_import_state(&d).unwrap().word() == "done"
    });
    s.close();
}

#[test]
fn declining_is_remembered_by_the_server() {
    let dir = scratch("decline");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    s.ensure_chat();
    assert_eq!(import_call(&s, "decline")["state"], "declined");
    let st = read_import_state(&s.foreman_log_dir()).unwrap();
    assert_eq!(st.word(), "declined");
    // Choosing later still works.
    let r = import_call(&s, "accept");
    assert!(r["state"] == "running" || r["state"] == "done", "{r}");
    s.close();
}

#[test]
fn floor_import_needs_chat_mode_and_a_known_action() {
    let dir = scratch("pane");
    let p = server(&dir, false, "");
    let r = p.floor_import(&req(r#"{"id":"3","cmd":"floor.import"}"#));
    assert!(!r.ok && r.error.unwrap().1.contains("chat mode"));
    let dir2 = scratch("bad");
    let s = server(&dir2, true, &import_home(&dir2));
    let r = s.floor_import(&req(r#"{"id":"3","cmd":"floor.import","action":"nuke"}"#));
    assert!(!r.ok);
    let r = s.floor_import(&req(
        r#"{"id":"3","cmd":"floor.import","action":"accept","from":"gemini"}"#,
    ));
    assert!(!r.ok);
    s.close();
}

// ---- fix round 1 of the import review (IM-R-6, 7, 10, 11, 12) ----

fn scans_of(fc: &ForemanChat) -> u64 {
    fc.est_flight.lock().unwrap().scans
}

/// FM-R-83: many floor.import status calls make one scan, not one each.
#[test]
fn many_status_calls_make_one_scan() {
    let dir = scratch("onescan");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    let (tx, rx) = std::sync::mpsc::channel::<()>();
    let rx = Arc::new(std::sync::Mutex::new(rx));
    *fc.scan_hold.lock().unwrap() = Some(Arc::new(move || {
        let _ = rx.lock().unwrap().recv();
    }));
    fc.est.lock().unwrap().0 = false;
    let base = scans_of(&fc);
    let hs: Vec<_> = (0..12)
        .map(|_| {
            let s = s.clone();
            std::thread::spawn(move || import_call(&s, "status"))
        })
        .collect();
    for h in hs {
        h.join().unwrap();
    }
    drop(tx);
    wait_for("the scan", || fc.est.lock().unwrap().0);
    assert_eq!(scans_of(&fc) - base, 1, "12 status calls made more than one scan");
    let r = import_call(&s, "status");
    assert_eq!((r["sessions"].as_i64(), r["messages"].as_i64()), (Some(2), Some(3)));
    s.close();
}

/// IM-R-12 (round 2): the Import button joins the scan that runs, instead of
/// parsing every transcript on its own beside it.
#[test]
fn the_import_button_shares_the_running_scan() {
    let dir = scratch("btnshare");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    let (tx, rx) = std::sync::mpsc::channel::<()>();
    let rx = Arc::new(std::sync::Mutex::new(rx));
    *fc.scan_hold.lock().unwrap() = Some(Arc::new(move || {
        let _ = rx.lock().unwrap().recv();
    }));
    fc.est.lock().unwrap().0 = false;
    let base = scans_of(&fc);
    import_call(&s, "status"); // starts the scan
    wait_for("the scan starts", || scans_of(&fc) - base == 1);
    let (dtx, drx) = std::sync::mpsc::channel();
    {
        let s = s.clone();
        std::thread::spawn(move || {
            let _ = dtx.send(import_call(&s, "accept"));
        });
    }
    assert!(
        drx.recv_timeout(Duration::from_millis(300)).is_err(),
        "the accept returned while a scan was held: it scanned on its own"
    );
    drop(tx);
    let r = drx.recv_timeout(Duration::from_secs(10)).expect("the accept did not return");
    assert_eq!((r["state"].as_str(), r["total"].as_i64()), (Some("running"), Some(3)));
    wait_for("the rescan", || fc.est.lock().unwrap().0);
    assert_eq!(scans_of(&fc) - base, 2, "want the shared scan and the rescan only");
    s.close();
}

/// IM-R-6: after No thanks, the floor's status reads no transcript.
#[test]
fn no_scan_after_a_decline() {
    let dir = scratch("nodecl");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    import_call(&s, "decline");
    fc.est.lock().unwrap().0 = false;
    let base = scans_of(&fc);
    for _ in 0..3 {
        let r = import_call(&s, "status");
        assert_eq!(r["state"], "declined");
        assert_eq!(r["estimating"], false);
        assert_eq!(r["sessions"], 0);
    }
    std::thread::sleep(Duration::from_millis(100));
    assert_eq!(scans_of(&fc) - base, 0, "a scan ran after a decline");
    s.close();
}

/// IM-R-7: the offer says where the text goes.
#[test]
fn the_offer_says_the_foremans_model_summarizes_the_text() {
    let dir = scratch("offertxt");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    let text = log_of(&fc)[0].1.clone();
    assert!(
        text.contains("summarized by the foreman's model like any chat message"),
        "{text}"
    );
    s.close();
}

/// IM-R-10: the poll knob is a test knob, clamped.
#[test]
fn the_poll_knob_is_test_only_and_clamped() {
    use super::foreman_chat::poll_from_env as p;
    let ms = Duration::from_millis;
    let five = Duration::from_secs(5);
    assert_eq!(p(None, Some("1")), five);
    assert_eq!(p(Some(""), Some("1")), five);
    assert_eq!(p(Some("1"), Some("1")), ms(10));
    assert_eq!(p(Some("1"), Some("250")), ms(250));
    assert_eq!(p(Some("1"), Some("3600000")), ms(5000));
    assert_eq!(p(Some("1"), Some("junk")), five);
    assert!(p(Some("1"), Some("off")) > Duration::from_secs(86400));
    assert_eq!(p(None, Some("off")), five);
}

/// FM-R-78: sessions of coppice's own in the home are not offered, and a
/// compactor session is not imported when the owner accepts.
#[test]
fn own_sessions_are_not_offered_or_imported() {
    let dir = scratch("own");
    let home = import_home(&dir);
    let w = |p: &str, text: &str| {
        let p = std::path::Path::new(&home).join(p);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, text).unwrap();
    };
    let marker = serde_json::to_string(&format!(
        "Limit: 9 bytes.\n{}\n",
        crate::foreman::compactor::COMPACTOR_MARKER
    ))
    .unwrap();
    let s = server(&dir, true, &home);
    let comp = s.compactor_dir();
    w(
        ".claude/projects/-tmp/k.jsonl",
        &format!(
            "{}\n{}\n",
            format!(
                r#"{{"type":"user","uuid":"k1","sessionId":"k","timestamp":"2026-03-03T10:00:00.000Z","cwd":"/tmp","message":{{"role":"user","content":{marker}}}}}"#
            ),
            format!(
                r#"{{"type":"assistant","uuid":"k2","sessionId":"k","timestamp":"2026-03-03T10:00:01.000Z","cwd":"{}/x","message":{{"role":"assistant","content":[{{"type":"text","text":"sum"}}]}}}}"#,
                comp.display()
            )
        ),
    );
    w(
        ".claude/projects/-cd/k3.jsonl",
        &(format!(
            r#"{{"type":"user","uuid":"k3","sessionId":"k3","timestamp":"2026-03-03T10:00:00.000Z","cwd":"{}/foreman/compactor","message":{{"role":"user","content":"hello"}}}}"#,
            dir.join("data").display()
        ) + "\n"),
    );
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    let text = log_of(&fc)[0].1.clone();
    assert!(text.contains("2 past chats (about 3 messages)"), "{text}");
    import_call(&s, "accept");
    let d = s.foreman_log_dir();
    wait_for("the import", || {
        read_import_state(&d).unwrap().word() == "done"
    });
    assert_eq!(kind_count(&fc, "past"), 3);
    s.close();
}

/// FM-R-85: the progress action reads the state alone and starts no scan.
#[test]
fn progress_starts_no_scan() {
    let dir = scratch("progress");
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    fc.est.lock().unwrap().0 = false;
    let base = scans_of(&fc);
    let r = import_call(&s, "progress");
    assert_eq!(r["state"], "off");
    assert_eq!(r["estimating"], false);
    std::thread::sleep(Duration::from_millis(100));
    assert_eq!(scans_of(&fc) - base, 0);
    s.close();
}

// Fix round 3 of the import review (F-1, F-2, F-4).

fn started_import(tag: &str) -> (Arc<Server>, Arc<ForemanChat>, String) {
    let dir = scratch(tag);
    let home = import_home(&dir);
    let s = server(&dir, true, &home);
    let fc = s.ensure_chat();
    wait_for("the offer", || kind_count(&fc, "note") == 1);
    wait_for("the estimate", || fc.est.lock().unwrap().0);
    (s, fc, home)
}

fn accept_with(s: &Arc<Server>, extra: &str) -> serde_json::Value {
    let r = s.floor_import(&req(&format!(
        r#"{{"id":"1","cmd":"floor.import","action":"accept"{extra}}}"#
    )));
    assert!(r.ok, "{:?}", r.error);
    r.result.unwrap()
}

/// F-1: a transcript that appeared after the cached scan is still queued.
#[test]
fn an_accept_sees_a_chat_written_after_the_cached_scan() {
    let (s, _fc, home) = started_import("f1new");
    let p = std::path::Path::new(&home).join(".claude/projects/p/s9.jsonl");
    std::fs::write(
        p,
        r#"{"type":"user","uuid":"u9","sessionId":"s9","timestamp":"2026-04-01T10:00:00.000Z","cwd":"/w/proj","message":{"role":"user","content":"a new chat"}}
"#,
    )
    .unwrap();
    let r = accept_with(&s, "");
    assert_eq!(r["total"].as_i64(), Some(4), "reply = {r}: want the new chat counted");
    s.close();
}

/// F-1: a cache older than a minute is not trusted; a fresh one is.
#[test]
fn an_accept_rescans_after_a_minute_and_uses_a_fresh_cache() {
    let (s, fc, _home) = started_import("f1age");
    let base = scans_of(&fc);
    let r = accept_with(&s, "");
    assert_eq!(r["total"].as_i64(), Some(3));
    wait_for("the rescan", || fc.est.lock().unwrap().0);
    assert_eq!(scans_of(&fc) - base, 1, "a fresh cache should leave only the rescan");
    // Age the (re-made) cache.
    fc.est_flight.lock().unwrap().at = Some(Instant::now() - Duration::from_secs(61));
    let base = scans_of(&fc);
    accept_with(&s, "");
    assert!(scans_of(&fc) - base >= 1, "the stale cache served the accept");
    s.close();
}

/// F-1: an accept that finds nothing says so.
#[test]
fn an_accept_that_finds_nothing_says_so() {
    let (s, _fc, _home) = started_import("f1none");
    let r = accept_with(&s, r#","since":"2030-01-01""#);
    assert_eq!(r["note"], "nothing new to import", "reply = {r}");
    s.close();
}

/// F-2: a panic in a scan frees the flight; the next caller is not stuck.
#[test]
fn a_panicking_scan_frees_the_flight() {
    let (s, fc, _home) = started_import("f2panic");
    for own in [false, true] {
        *fc.scan_hold.lock().unwrap() = Some(Arc::new(|| panic!("scan blew up")));
        fc.est.lock().unwrap().0 = false;
        let r = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            if own {
                s.scan_with(&fc, &Ctx::background(), &ImportOpts::default());
            } else {
                s.import_estimate(&fc, &Ctx::background(), true);
            }
        }));
        assert!(r.is_err());
        *fc.scan_hold.lock().unwrap() = None;
        assert!(!fc.est_flight.lock().unwrap().running, "own={own}: the flight stayed busy");
        let (tx, rx) = std::sync::mpsc::channel();
        let (s2, fc2) = (s.clone(), fc.clone());
        std::thread::spawn(move || {
            let _ = tx.send(s2.import_estimate(&fc2, &Ctx::background(), true).len());
        });
        assert_eq!(rx.recv_timeout(Duration::from_secs(10)).expect("the next scan waits for ever"), 2);
    }
    s.close();
}

/// F-4: a cancelled own scan queues nothing.
#[test]
fn a_cancelled_own_scan_gives_nothing() {
    let (s, fc, _home) = started_import("f4cancel");
    let ctx = Ctx::background();
    ctx.cancel();
    let o = ImportOpts { tools: true, ..Default::default() };
    assert!(s.scan_with(&fc, &ctx, &o).is_empty());
    s.close();
}
