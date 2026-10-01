//! The process fact at the moment of a read (CP-R-50): a read computes it
//! from the last write and its own clock, not from the last 1 s tick.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use serde_json::Value;

use super::{Config, PeerFacts, Server};

struct Conn {
    w: UnixStream,
    r: BufReader<UnixStream>,
}

impl Conn {
    fn new(s: &Arc<Server>) -> Conn {
        let (a, b) = UnixStream::pair().unwrap();
        let s = s.clone();
        thread::spawn(move || s.serve_conn(b, PeerFacts::default()));
        a.set_read_timeout(Some(Duration::from_secs(15))).unwrap();
        Conn {
            r: BufReader::new(a.try_clone().unwrap()),
            w: a,
        }
    }

    fn call(&mut self, line: &str, id: &str) -> Value {
        self.w.write_all(line.as_bytes()).unwrap();
        self.w.write_all(b"\n").unwrap();
        loop {
            let mut l = String::new();
            let n = self.r.read_line(&mut l).expect("no reply within 15 s");
            assert!(n > 0, "the connection closed");
            let v: Value = serde_json::from_str(&l).unwrap();
            if v["id"] == id {
                return v;
            }
        }
    }
}

/// A pane that printed and was seen to print lists as working at once; the
/// tick used to leave "no output yet" for up to a second after the output
/// was on the screen.
#[test]
fn a_read_after_output_sees_the_output_at_once() {
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-proc-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let mut c = Conn::new(&s);
    let cwd = dir.to_string_lossy().to_string();
    for i in 0..5 {
        let r = c.call(
            &format!(
                r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","cmd_argv":["sh","-c","echo ready; exec sleep 30"],"kind":"pty"}}"#
            ),
            "c",
        );
        assert_eq!(r["ok"], true, "{r}");
        let id = r["result"]["pane"].as_str().unwrap().to_string();
        let r = c.call(
            &format!(
                r#"{{"id":"w","cmd":"pane.wait_output","pane":"{id}","contains":"ready","timeout_ms":10000}}"#
            ),
            "w",
        );
        assert_eq!(r["ok"], true, "{r}");
        let r = c.call(r#"{"id":"l","cmd":"pane.list"}"#, "l");
        let row = r["result"]["panes"]
            .as_array()
            .unwrap()
            .iter()
            .find(|p| p["id"] == id.as_str())
            .unwrap()
            .clone();
        assert_eq!(
            (row["state"].as_str(), row["detail"].as_str()),
            (Some("working"), Some("output in the last 5 s")),
            "run {i}: {row}"
        );
    }
    s.close();
    let _ = std::fs::remove_dir_all(&dir);
}

/// Closes the server (and so every pane child) on every exit path, a panic
/// included.
pub(super) struct CloseOnDrop(pub(super) Arc<Server>, pub(super) std::path::PathBuf);

impl Drop for CloseOnDrop {
    fn drop(&mut self) {
        self.0.close();
        let _ = std::fs::remove_dir_all(&self.1);
    }
}

fn gate_idle(s: &Server, id: &str) {
    s.apply_state(
        id,
        crate::state::Event {
            v: 1,
            ts: Server::now_seconds(),
            session_id: id.into(),
            pane: Some(id.into()),
            state: crate::state::STATE_IDLE.into(),
            source: crate::state::SRC_GATE.into(),
            ..Default::default()
        },
    );
}

/// A gate idle stands against a short redraw of a quiet pane (CP-R-56): a
/// clock or a status line ticking on an idle harness is no work. Output
/// that goes on for a second more is work. The Go twin is
/// TestAShortRedrawDoesNotOutrankAStandingGateIdle.
#[test]
fn a_short_redraw_does_not_outrank_a_standing_gate_idle() {
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-redraw-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let mut c = Conn::new(&s);
    let cwd = dir.to_string_lossy().to_string();
    let r = c.call(
        &format!(
            r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","cmd_argv":["sh","-c","exec sleep 30"],"kind":"pty"}}"#
        ),
        "c",
    );
    assert_eq!(r["ok"], true, "{r}");
    let id = r["result"]["pane"].as_str().unwrap().to_string();
    let lp = s.live(&id).unwrap();
    let skew = |secs: i64| {
        s.clock_skew
            .store(secs * 1_000_000_000, std::sync::atomic::Ordering::SeqCst)
    };
    // A turn drew the screen, the gate said idle, and 6 s later the process
    // source called the quiet screen idle too. That idle replaces the
    // gate's in the merged state, so the pane must remember the gate's.
    lp.grid.write(b"turn output").unwrap();
    s.process_tick(&id, &lp);
    gate_idle(&s, &id);
    skew(6);
    s.process_tick(&id, &lp);
    let st = s.effective_state(&id).unwrap();
    assert_eq!(
        (st.state.as_str(), st.source.as_str()),
        ("idle", "process"),
        "6 s after the turn"
    );
    // The grid marks a burst by its writes: the turn was 6 s ago.
    lp.grid
        .stamp_write_for_test(crate::pane::now_nanos() - 6_000_000_000);
    lp.grid.write(b"12:01").unwrap();
    skew(0);
    s.process_tick(&id, &lp);
    let st = s.effective_state(&id).unwrap();
    assert_eq!(
        st.state, "idle",
        "after a short redraw (from {})",
        st.source
    );
    thread::sleep(Duration::from_millis(1100));
    lp.grid.write(b"12:02").unwrap();
    s.process_tick(&id, &lp);
    let st = s.effective_state(&id).unwrap();
    assert_eq!(
        (st.state.as_str(), st.source.as_str()),
        ("working", "process"),
        "after output for a second more"
    );
}

pub(super) fn stamp_pane(s: &std::sync::Arc<Server>, dir: &std::path::Path) -> (String, std::sync::Arc<super::panes::LivePane>) {
    let cwd = dir.to_string_lossy().to_string();
    let mut c = Conn::new(s);
    let r = c.call(
        &format!(
            r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","cmd_argv":["sh","-c","exec sleep 30"],"kind":"pty"}}"#
        ),
        "c",
    );
    assert_eq!(r["ok"], true, "{r}");
    let id = r["result"]["pane"].as_str().unwrap().to_string();
    let lp = s.live(&id).unwrap();
    (id, lp)
}

pub(super) fn stamp_ev(id: &str, st: &str, src: &str) -> crate::state::Event {
    crate::state::Event {
        v: 1,
        ts: Server::now_seconds(),
        session_id: id.into(),
        harness: "claude-code".into(),
        pane: Some(id.into()),
        state: st.into(),
        source: src.into(),
        detail: "session stop".into(),
        ..Default::default()
    }
}

/// The Stop hook reports its idle as source headless (hook.py), so that is
/// the event the stamp follows (CP-R-57). The Go twin is
/// TestTheStampFollowsTheHookIdleAsTheHookSendsIt.
#[test]
fn the_stamp_follows_the_hook_idle_as_the_hook_sends_it() {
    use crate::state::*;
    use std::sync::atomic::Ordering::SeqCst;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-stamp-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    s.apply_state(&id, stamp_ev(&id, STATE_IDLE, SRC_PROCESS));
    assert_eq!(lp.gate_idle_since.load(SeqCst), 0, "a process idle set the stamp");
    s.apply_state(&id, stamp_ev(&id, STATE_IDLE, SRC_HEADLESS));
    let first = lp.gate_idle_since.load(SeqCst);
    assert_ne!(first, 0, "the Stop hook idle (source headless) did not set the stamp");
    s.apply_state(&id, stamp_ev(&id, STATE_IDLE, SRC_GATE));
    assert_eq!(lp.gate_idle_since.load(SeqCst), first, "a second idle moved the stamp");
    s.apply_state(&id, stamp_ev(&id, STATE_WORKING, SRC_GATE));
    assert_eq!(lp.gate_idle_since.load(SeqCst), 0, "a gate working did not clear it");
    s.apply_state(&id, stamp_ev(&id, STATE_IDLE, SRC_HEADLESS));
    s.apply_state(&id, stamp_ev(&id, STATE_WORKING, SRC_HEADLESS));
    assert_eq!(lp.gate_idle_since.load(SeqCst), 0, "a headless working did not clear it");
}

/// The 3 s boundary runs on the server clock, and a submitted line clears
/// the stamp (CP-R-57). The Go twins are TestAStampStandsOnlyAfterThreeSeconds
/// and TestASubmittedLineClearsAStaleStamp.
#[test]
fn a_stamp_stands_after_three_seconds_and_a_submitted_line_clears_it() {
    use crate::state::*;
    use std::sync::atomic::Ordering::SeqCst;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-stamp2-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    s.apply_state(&id, stamp_ev(&id, STATE_IDLE, SRC_HEADLESS));
    let working = stamp_ev(&id, STATE_WORKING, SRC_PROCESS);
    s.clock_skew.store(2_900_000_000, SeqCst);
    assert!(!s.gate_idle_stands(&lp, &working), "a 2.9 s stamp stands");
    s.clock_skew.store(10_000_000_000, SeqCst);
    assert!(s.gate_idle_stands(&lp, &working), "a 10 s stamp does not stand");
    assert!(!s.gate_idle_stands(&lp, &stamp_ev(&id, STATE_WORKING, SRC_GATE)));
    s.mark_input(&id, false);
    assert_ne!(lp.gate_idle_since.load(SeqCst), 0, "a half line cleared the stamp");
    s.mark_input(&id, true);
    assert!(!s.gate_idle_stands(&lp, &working), "a stale stamp stands after a line");
}

/// An idle the merge held back changes nothing (CP-R-57): the pane stays
/// blocked behind a gate hold, so a headless idle there is no stamp. The Go
/// twin is TestAHeldBackHookIdleSetsNoStamp.
#[test]
fn a_held_back_hook_idle_sets_no_stamp() {
    use crate::state::*;
    use std::sync::atomic::Ordering::SeqCst;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-held-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    let mut ask = stamp_ev(&id, STATE_BLOCKED, SRC_GATE);
    ask.ask = Some(Ask {
        id: "a".into(),
        tool: "Bash".into(),
        summary: "x".into(),
        deadline: Server::now_seconds() + 600.0,
        ..Default::default()
    });
    s.apply_state(&id, ask);
    s.apply_state(&id, stamp_ev(&id, STATE_IDLE, SRC_HEADLESS));
    assert_eq!(lp.gate_idle_since.load(SeqCst), 0, "an idle the gate hold discarded set the stamp");
}

/// A hook idle older than a fresh pane's first draw does not stand
/// (CP-R-57). The Go twin is TestAStampOlderThanTheFirstDrawDoesNotSettleAFreshPane.
#[test]
fn a_stamp_older_than_the_first_draw_does_not_settle_a_fresh_pane() {
    use crate::state::*;
    use std::sync::atomic::Ordering::SeqCst;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-fresh-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    lp.grid.write(b"drawn").unwrap();
    s.process_tick(&id, &lp);
    let first = lp.grid.first_write().unwrap();
    lp.gate_idle_since.store(first - 10_000_000_000, SeqCst);
    let run = |fresh: bool| {
        let (tx, rx) = std::sync::mpsc::channel();
        let s2 = s.clone();
        let id2 = id.clone();
        std::thread::spawn(move || {
            let r = s2.settle_for_test(&id2, fresh);
            let _ = tx.send(r);
        });
        rx.recv_timeout(Duration::from_millis(1500)).is_ok()
    };
    assert!(run(false), "a 10 s stamp must settle a pane that is not fresh");
    assert!(!run(true), "a stamp older than the first draw settled a fresh pane");
}

/// A hook idle older than a fresh pane's first draw says nothing, and the
/// CP-R-56 hold keeps a short draw from reaching the process source, so no
/// process idle replaces it. A pane that drew and kept quiet for
/// PROCESS_QUIET is ready all the same. The Go twin is
/// TestAStaleHookIdleDoesNotKeepAQuietFreshPaneFromSettling.
#[test]
fn a_stale_hook_idle_does_not_keep_a_quiet_fresh_pane_from_settling() {
    use crate::state::*;
    use std::sync::atomic::Ordering::SeqCst;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-stale-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    let mut old = stamp_ev(&id, STATE_IDLE, SRC_GATE);
    old.ts = 1.0;
    s.apply_state(&id, old);
    lp.grid.write(b"REPORT ok").unwrap();
    s.process_tick(&id, &lp);
    let run = || {
        let (tx, rx) = std::sync::mpsc::channel();
        let s2 = s.clone();
        let id2 = id.clone();
        std::thread::spawn(move || {
            let _ = tx.send(s2.settle_for_test(&id2, true));
        });
        rx.recv_timeout(Duration::from_millis(1500)).is_ok()
    };
    assert!(!run(), "a pane that drew a moment ago settled on a stale idle");
    s.clock_skew.store(6_000_000_000, SeqCst);
    assert!(run(), "a pane quiet for 6 s did not settle on a stale hook idle");
}

/// A burst starts at a write after a quiet gap, not at a look that saw the
/// gap (CP-R-56). The Go twin is TestABurstAfterAGapIsNewEvenIfNoLookSawTheGap.
#[test]
fn a_burst_after_a_gap_is_new_even_if_no_look_saw_the_gap() {
    use crate::state::*;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-gap-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    let mut idle = stamp_ev(&id, STATE_IDLE, SRC_GATE);
    idle.ts = 1.0;
    s.apply_state(&id, idle);
    lp.grid.write(b"turn").unwrap();
    s.process_tick(&id, &lp);
    let first = lp.grid.last_write().unwrap();
    lp.grid.stamp_write_for_test(first + 7_000_000_000);
    s.clock_skew
        .store(7_500_000_000, std::sync::atomic::Ordering::SeqCst);
    s.process_tick(&id, &lp);
    let eff = s.effective_state(&id).unwrap();
    assert_eq!((eff.state.as_str(), eff.source.as_str()), ("idle", "gate"), "{eff:?}");
}

/// A hook idle that stands is not replaced by the first look's "no output
/// yet" (CP-R-56). The Go twin is TestTheFirstLookDoesNotReplaceAStandingHookIdle.
#[test]
fn the_first_look_does_not_replace_a_standing_hook_idle() {
    use crate::state::*;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-first-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    lp.proc_state.lock().unwrap().state = String::new();
    let mut idle = stamp_ev(&id, STATE_IDLE, SRC_GATE);
    idle.ts = 1.0;
    s.apply_state(&id, idle);
    s.process_tick(&id, &lp);
    let eff = s.effective_state(&id).unwrap();
    assert_eq!((eff.state.as_str(), eff.source.as_str()), ("idle", "gate"), "{eff:?}");
}

/// The same holds when a draw was held first. The Go twin is
/// TestAQuietIdleAfterAHeldDrawDoesNotReplaceAStandingHookIdle.
#[test]
fn a_quiet_idle_after_a_held_draw_does_not_replace_a_standing_hook_idle() {
    use crate::state::*;
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-held2-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let _guard = CloseOnDrop(s.clone(), dir.clone());
    let (id, lp) = stamp_pane(&s, &dir);
    lp.proc_state.lock().unwrap().state = String::new();
    let mut idle = stamp_ev(&id, STATE_IDLE, SRC_GATE);
    idle.ts = 1.0;
    s.apply_state(&id, idle);
    lp.grid.write(b"turn").unwrap();
    s.process_tick(&id, &lp);
    s.clock_skew
        .store(6_000_000_000, std::sync::atomic::Ordering::SeqCst);
    s.process_tick(&id, &lp);
    let eff = s.effective_state(&id).unwrap();
    assert_eq!((eff.state.as_str(), eff.source.as_str()), ("idle", "gate"), "{eff:?}");
}
