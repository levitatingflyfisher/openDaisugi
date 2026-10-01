//! Go's Run tests, ported: the floor driven end to end against a Rust
//! server in this process, on its own scratch socket and data dir. Keys
//! come from a channel that closes after the script, as Go's tests read a
//! strings.Reader to EOF.

use std::io::Write;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::{json, Map, Value};

use super::render::strip_sgr;
use super::run::{call, params, run, FloorErr, Options};
use crate::server::{self, Server};

/// A server on a scratch socket, closed when dropped.
struct TestServer {
    srv: Arc<Server>,
    sock: PathBuf,
    dir: PathBuf,
}

impl Drop for TestServer {
    fn drop(&mut self) {
        self.srv.close();
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn scratch(tag: &str) -> PathBuf {
    let base = std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into());
    static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    let n = NEXT.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    let d = PathBuf::from(base).join(format!("cop-tui-{tag}-{}-{n}", std::process::id()));
    std::fs::create_dir_all(&d).unwrap();
    d
}

fn new_test_server(tag: &str) -> TestServer {
    let dir = scratch(tag);
    let sock = dir.join("s.sock");
    let srv = Server::new(server::Config {
        socket_path: sock.clone(),
        data_dir: dir.join("data"),
        start_dir: dir.clone(),
        voice: server::voice::VoiceConfig {
            off: true,
            ..Default::default()
        },
    });
    srv.acquire_start_lock().unwrap();
    let ln = srv.listen().unwrap();
    let s2 = srv.clone();
    std::thread::spawn(move || s2.serve(ln));
    TestServer { srv, sock, dir }
}

fn raw_call(
    sock: &std::path::Path,
    cmd: &str,
    p: Option<Map<String, Value>>,
) -> Map<String, Value> {
    call(sock, cmd, p).unwrap_or_else(|e| panic!("{cmd}: {}", e.text()))
}

/// A pty pane running argv, once it has drawn something.
fn create_shell_pane(s: &TestServer, argv: &[&str]) -> String {
    let res = raw_call(
        &s.sock,
        "pane.create",
        params(vec![
            ("cwd", json!(s.dir.display().to_string())),
            ("kind", json!("pty")),
            ("cmd_argv", json!(argv)),
            ("cols", json!(80)),
            ("rows", json!(24)),
        ]),
    );
    let id = res["pane"].as_str().unwrap().to_string();
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        let r = raw_call(
            &s.sock,
            "pane.read",
            params(vec![("pane", json!(id)), ("source", json!("visible"))]),
        );
        if !r["text"].as_str().unwrap_or("").trim().is_empty() {
            return id;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    panic!("pane {id} never drew anything");
}

/// What the floor wrote.
#[derive(Clone, Default)]
struct Out(Arc<Mutex<Vec<u8>>>);

impl Write for Out {
    fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(b);
        Ok(b.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl Out {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.0.lock().unwrap()).into_owned()
    }
}

/// Runs the floor on keys, then their end, at cols by rows.
fn run_floor(
    sock: &std::path::Path,
    keys: &[&str],
    cols: i64,
    rows: i64,
    data_dir: &str,
) -> (String, Result<(), FloorErr>) {
    let (tx, rx) = mpsc::channel();
    for k in keys {
        for b in k.bytes() {
            tx.send(Some(b)).unwrap();
        }
    }
    drop(tx);
    let out = Out::default();
    let res = run(
        Options {
            socket: sock.to_path_buf(),
            size: Box::new(move || (cols, rows)),
            cwd: std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into()),
            default: "claude".into(),
            data_dir: data_dir.into(),
            leave: 0,
            views: vec![],
            web_url: String::new(),
            talk: 0,
            out: Box::new(out.clone()),
            tty: false,
        },
        &rx,
    );
    (out.text(), res)
}

/// The frames of an output: each starts with the cursor home.
fn split_frames(s: &str) -> Vec<String> {
    let mut parts: Vec<String> = s.split("\x1b[H").map(String::from).collect();
    if !parts.is_empty() && (parts[0].is_empty() || parts[0] == "\x1b[2J") {
        parts.remove(0);
    }
    parts
}

fn last_frame(s: &str) -> String {
    split_frames(s).pop().unwrap_or_default()
}

/// The name on the one cursor row of a frame.
fn marked_row(frame: &str) -> String {
    let mut found = Vec::new();
    for l in strip_sgr(frame).split("\r\n") {
        let l = l.trim_start_matches("\x1b[K");
        if let Some(rest) = l.strip_prefix("  › ") {
            let fs: Vec<&str> = rest.split_whitespace().collect();
            let name = if fs.len() > 1 && fs[0].len() == 1 && fs[0] >= "1" && fs[0] <= "9" {
                fs[1]
            } else {
                fs.first().copied().unwrap_or("")
            };
            found.push(name.to_string());
        }
    }
    assert_eq!(found.len(), 1, "{found:?} in {frame:?}");
    found.remove(0)
}

#[test]
fn the_first_frame_lists_the_pane_and_the_bar() {
    let s = new_test_server("first");
    let id = create_shell_pane(&s, &["sh", "-c", "echo hello; sleep 30"]);
    let (out, res) = run_floor(&s.sock, &["\x03"], 80, 24, "");
    res.unwrap();
    let first = strip_sgr(&split_frames(&out)[0]);
    assert!(
        first.contains("coppice ·") && first.contains(&id),
        "{first}"
    );
    assert!(first.contains("working") || first.contains("idle"));
    for b in super::keys::table() {
        assert!(first.contains(&format!("{} {}", b.keys, b.help)), "{first}");
    }
}

#[test]
fn q_is_text_only_after_ctrl_t_and_ctrl_c_and_the_end_return_ok() {
    let s = new_test_server("q");
    let (out, res) = run_floor(&s.sock, &["q", "\x14q", "\x03"], 80, 24, "");
    res.unwrap();
    assert!(out.contains("Nothing running. Enter opens claude here."));
    let frames = split_frames(&out);
    assert!(frames.len() >= 3);
    assert!(strip_sgr(&frames[1]).contains(super::rail::RAIL_HINT));
    assert!(strip_sgr(&last_frame(&out)).contains("› q"));
    run_floor(&s.sock, &[], 80, 24, "").1.unwrap();
}

#[test]
fn space_opens_the_peek_and_esc_closes_it() {
    let s = new_test_server("peek");
    create_shell_pane(&s, &["sh", "-c", "echo hello; sleep 30"]);
    let (out, res) = run_floor(&s.sock, &[" ", "\x1b", "\x03"], 80, 24, "");
    res.unwrap();
    let frames = split_frames(&out);
    assert!(frames.len() >= 3);
    assert!(frames[1..].iter().any(|f| f.contains("hello")));
    assert!(!frames.last().unwrap().contains("hello"));
}

#[test]
fn the_down_arrow_moves_the_cursor() {
    let s = new_test_server("down");
    create_shell_pane(&s, &["sh", "-c", "echo one; sleep 30"]);
    create_shell_pane(&s, &["sh", "-c", "echo two; sleep 30"]);
    let (out, res) = run_floor(&s.sock, &["\x1b[B", "\x1b[B", "\x03"], 80, 24, "");
    res.unwrap();
    let frames = split_frames(&out);
    assert_ne!(marked_row(&frames[0]), marked_row(frames.last().unwrap()));
}

#[test]
fn close_at_the_prompt_closes_the_pane() {
    let s = new_test_server("close");
    let id = create_shell_pane(&s, &["sh", "-c", "echo hello; sleep 30"]);
    let (out, res) = run_floor(&s.sock, &[&format!("\x14close {id}\r"), "\x03"], 80, 24, "");
    res.unwrap();
    let last = strip_sgr(&last_frame(&out));
    assert!(
        !last.contains(&id) && last.contains("Nothing running."),
        "{last}"
    );
}

#[test]
fn talk_with_no_default_harness_says_how_to_set_one() {
    let s = new_test_server("talk");
    let (out, res) = run_floor(&s.sock, &["\x14hello there\r", "\x03"], 80, 24, "");
    res.unwrap();
    let last = strip_sgr(&last_frame(&out));
    assert!(last.contains("no default harness"), "{last}");
    assert!(!last.contains("› hello there"));
}

#[test]
fn the_prompt_echoes_and_backspace_deletes_a_rune() {
    let s = new_test_server("echo");
    let (out, res) = run_floor(&s.sock, &["\x14ab", "\x7f", "\x03"], 80, 24, "");
    res.unwrap();
    assert!(strip_sgr(&last_frame(&out)).contains("› a "));
}

#[test]
fn ctrl_w_asks_esc_keeps_and_enter_stops() {
    let s = new_test_server("stop");
    let id = create_shell_pane(&s, &["sh", "-c", "echo hello; sleep 30"]);
    run_floor(&s.sock, &["\x17", "\x1b", "\x03"], 80, 24, "")
        .1
        .unwrap();
    let list = raw_call(&s.sock, "pane.list", None);
    assert!(list["panes"]
        .as_array()
        .unwrap()
        .iter()
        .any(|p| p["id"] == json!(id)));
    let (out, res) = run_floor(&s.sock, &["\x17", "\r", "\x03"], 80, 24, "");
    res.unwrap();
    assert!(strip_sgr(&out).contains(&format!("› {}", super::model::stop_question(&id))));
    let last = strip_sgr(&last_frame(&out));
    assert!(
        last.contains("Nothing running.") && !last.contains("Recent") && !last.contains("ended")
    );
}

#[test]
fn the_lock_and_the_layout_round_trip_through_floor_json() {
    let s = new_test_server("floorjson");
    let dir = s.dir.join("fj");
    std::fs::create_dir_all(&dir).unwrap();
    let d = dir.display().to_string();
    run_floor(&s.sock, &["\x14lock\r", "\x03"], 200, 40, &d)
        .1
        .unwrap();
    assert_eq!(
        std::fs::read_to_string(dir.join("floor.json")).unwrap(),
        r#"{"layout":"focus","locked":true}"#
    );
    let (out, _) = run_floor(&s.sock, &["\x14swap 1 2\r", "\x03"], 200, 40, &d);
    assert!(out.contains("Layout is locked. Type unlock first."));
    run_floor(
        &s.sock,
        &["\x14unlock\r", "\x14layout one\r", "\x03"],
        200,
        40,
        &d,
    )
    .1
    .unwrap();
    let (out, _) = run_floor(&s.sock, &["\x14swap 1 2\r", "\x03"], 200, 40, &d);
    assert!(out.contains("swap needs slot numbers from 1 to 1"));
    std::fs::write(dir.join("floor.json"), "{not json").unwrap();
    let (out, _) = run_floor(&s.sock, &["\x14swap 1 2\r", "\x03"], 200, 40, &d);
    assert!(!out.contains("swap needs") && !out.contains("locked"));
}

#[test]
fn a_server_that_stops_and_no_server_are_server_gone() {
    let dir = scratch("none");
    let (_, res) = run_floor(&dir.join("none.sock"), &["\x03"], 80, 24, "");
    assert!(matches!(res, Err(FloorErr::Gone(_))), "{res:?}");
    let _ = std::fs::remove_dir_all(&dir);
    let s = new_test_server("gone");
    let (tx, rx) = mpsc::channel::<Option<u8>>();
    let sock = s.sock.clone();
    let done = std::thread::spawn(move || {
        let out = Out::default();
        run(
            Options {
                socket: sock,
                size: Box::new(|| (80, 24)),
                cwd: String::new(),
                default: "claude".into(),
                data_dir: String::new(),
                leave: 0,
                views: vec![],
                web_url: String::new(),
                talk: 0,
                out: Box::new(out),
                tty: false,
            },
            &rx,
        )
    });
    std::thread::sleep(Duration::from_millis(500));
    s.srv.close();
    let res = done.join().unwrap();
    drop(tx);
    assert!(matches!(res, Err(FloorErr::Gone(_))), "{res:?}");
}

#[test]
fn a_wide_floor_opens_with_the_agent_that_needs_you_typing() {
    let s = new_test_server("start");
    let a = create_shell_pane(&s, &["sh", "-c", "echo one; sleep 30"]);
    let b = create_shell_pane(&s, &["sh", "-c", "echo two; sleep 30"]);
    raw_call(
        &s.sock,
        "pane.report_state",
        params(vec![
            ("pane", json!(b)),
            (
                "event",
                json!({"v":1,"ts":1,"session_id":"s","harness":"claude-code","pane":b,"state":"blocked",
                       "source":"gate","ask":{"id":"t1","tool":"Bash","summary":"ls","deadline":4102444800u64,"tier":"undoable"},
                       "detail":"verdict=ask"}),
            ),
        ]),
    );
    let (out, res) = run_floor(&s.sock, &["\x03"], 200, 40, "");
    res.unwrap();
    let first = strip_sgr(&split_frames(&out)[0]);
    assert!(first.contains(&format!("1› {b}")), "{first}");
    assert!(first.contains("typing here"), "{first}");
    assert!(first.contains(&format!("2  {a}")), "{first}");
    // The keys went to the window, so ctrl-c reached the pane, and the
    // floor ended with the keys.
    let _ = UnixStream::connect(&s.sock);
}
