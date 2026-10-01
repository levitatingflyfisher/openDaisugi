//! Go's tui tests, ported: every test that needs no server. The ones that
//! drive a floor against a live server are covered by the screen compare
//! (cases/tui).

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixListener;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use super::facts::{decode_facts, hint_min_cols, GATE_HINT};
use super::keys::tests::feed;
use super::keys::{hit_row, hit_tile, join_keys, parse_sgr, read_key_from, table, Click, Key};
use super::model::*;
use super::prompt::*;
use super::recent::ended_text;
use super::render::*;
use super::run::{whole_runes, Floor, FrameWriter, Options};
use super::speak::*;
use super::tree::{decode_tasks, new_tree_view, render_tree, TaskRow};
use crate::textwidth;
use crate::tiles::{Layout, Tiles};

fn o(v: Value) -> Obj {
    v.as_object().expect("an object").clone()
}

fn list(v: Value) -> Vec<Obj> {
    v.as_array()
        .expect("an array")
        .iter()
        .map(|x| x.as_object().expect("an object").clone())
        .collect()
}

fn row(id: &str, state: &str) -> Row {
    Row {
        id: id.into(),
        state: state.into(),
        ..Default::default()
    }
}

fn model(rows: Vec<Row>) -> Model {
    Model {
        rows,
        header_y: -1,
        ended_y: -1,
        body_cur: -1,
        ..Default::default()
    }
}

/// Removes a scratch dir when the test ends.
struct Gone(PathBuf);

impl Drop for Gone {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn scratch(tag: &str) -> PathBuf {
    let base = std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into());
    // One number per call: tests run in parallel, and two that share a
    // dir would remove each other's files.
    static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    let n = NEXT.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    let d = PathBuf::from(base).join(format!("coppice-rs-tui-{tag}-{}-{n}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

/// A floor with no server behind it. The keys never come and never close.
fn test_floor(m: Model, cols: i64) -> Floor<'static> {
    let (tx, rx) = std::sync::mpsc::channel::<Option<u8>>();
    let rx: &'static _ = Box::leak(Box::new(rx));
    Box::leak(Box::new(tx));
    let (a, _b) = std::os::unix::net::UnixStream::pair().unwrap();
    Box::leak(Box::new(_b));
    let (lt, lr) = std::sync::mpsc::channel();
    Box::leak(Box::new(lt));
    let (at, ar) = std::sync::mpsc::channel();
    Floor {
        o: Options {
            socket: PathBuf::new(),
            size: Box::new(move || (cols, 20)),
            cwd: String::new(),
            default: String::new(),
            data_dir: String::new(),
            leave: 0,
            views: vec![],
            web_url: String::new(),
            talk: 0,
            out: Box::new(std::io::sink()),
            tty: false,
        },
        m,
        fw: FrameWriter::new(),
        keys: rx,
        lines: lr,
        conn: a,
        detaching: Default::default(),
        sizes: Default::default(),
        sizing: Default::default(),
        kr: None,
        hold_at: None,
        text_q: vec![],
        text_busy: false,
        last_press: None,
        last_slot: 0,
        last_pane: String::new(),
        async_tx: at,
        async_rx: ar,
        ended_due: false,
        ended_read: None,
        replay: Default::default(),
        pause_until: None,
        pause_at: None,
        drag_from: String::new(),
        sp: Speaker::default(),
        rec: None,
        rec_for: String::new(),
        rec_to: None,
        checking: false,
        keys_to_talk: None,
        await_device: false,
        hearing: 0,
        utf8_pending: vec![],
    }
}

fn byte(b: u8) -> Key {
    Key::Byte(b)
}

fn plain(lines: &[String]) -> String {
    strip_sgr(&lines.join("\n"))
}

// ---- model ----

#[test]
fn rows_group_by_whether_you_are_needed() {
    let mut rows = vec![
        row("a", "working"),
        row("b", "blocked"),
        row("c", "done"),
        row("d", "blocked"),
    ];
    rows[1].age = 120.0;
    rows[3].age = 30.0;
    let secs = group(&rows);
    assert_eq!(secs[0].title, "NEEDS YOU");
    assert_eq!(secs[0].rows[0].id, "b");
    assert_eq!(secs[0].rows[1].id, "d");
    assert_eq!(secs[1].title, "WORKING");
    assert_eq!(secs[2].title, "DONE");
}

#[test]
fn group_always_returns_three_sections_in_order() {
    let secs = group(&[]);
    let want = ["NEEDS YOU", "WORKING", "DONE"];
    assert_eq!(secs.len(), 3);
    for (i, s) in secs.iter().enumerate() {
        assert_eq!(s.title, want[i]);
        assert!(s.rows.is_empty());
    }
}

#[test]
fn idle_and_unknown_rows_are_working_and_equal_ages_sort_by_id() {
    let secs = group(&[row("a", "idle"), row("b", "unknown")]);
    assert_eq!(secs[1].rows.len(), 2);
    let secs = group(&[row("b", "working"), row("a", "working")]);
    assert_eq!(secs[1].rows[0].id, "a");
}

#[test]
fn need_you_follows_the_list_and_a_closed_row_is_dropped() {
    let mut m = model(vec![]);
    m.apply(
        &list(json!([{"id":"a","state":"blocked"},{"id":"b","state":"working"}])),
        0.0,
    );
    assert_eq!(m.need_you, 1);
    m.apply(
        &list(json!([{"id":"a","state":"done","closed":true},{"id":"b","state":"working","closed":false}])),
        0.0,
    );
    assert_eq!(m.rows.len(), 1);
    assert_eq!(m.rows[0].id, "b");
}

#[test]
fn apply_copies_the_list_fields() {
    let mut m = model(vec![]);
    m.apply(
        &list(json!([
            {"id":"a","label":"gate-refactor","cwd":"/work/gate","harness":"claude","state":"working","source":"gate","detail":"editing","quiet_for":4.5},
            {"id":"b","state":"idle","source":null}
        ])),
        0.0,
    );
    let a = &m.rows[0];
    assert_eq!(
        (
            a.label.as_str(),
            a.worktree.as_str(),
            a.harness.as_str(),
            a.source.as_str(),
            a.line.as_str()
        ),
        ("gate-refactor", "/work/gate", "claude", "gate", "editing")
    );
    assert_eq!(a.quiet_for, 4.5);
    assert_eq!(m.rows[1].source, "");
}

#[test]
fn line_prefers_the_ask_with_its_tool_and_age_is_now_minus_ts() {
    let mut m = model(vec![]);
    m.apply(
        &list(json!([
            {"id":"a","state":"blocked","detail":"ignored","ask":{"tool":"git","summary":"push --force"}},
            {"id":"b","state":"blocked","detail":"ignored","ask":{"summary":"rm -rf build"}}
        ])),
        0.0,
    );
    assert_eq!(m.rows[0].line, "git: push --force");
    assert_eq!(m.rows[1].line, "rm -rf build");
    m.apply(
        &list(json!([{"id":"a","state":"working","ts":100.0},{"id":"b","state":"working"}])),
        160.0,
    );
    assert_eq!(m.rows[0].age, 60.0);
    assert_eq!(m.rows[1].age, 0.0);
}

#[test]
fn the_cursor_follows_the_pane_and_clamps() {
    let mut m = model(vec![]);
    m.apply(
        &list(json!([{"id":"a","state":"working"},{"id":"b","state":"working"}])),
        0.0,
    );
    m.move_by(1);
    assert_eq!(m.selected().unwrap().id, "b");
    m.apply(
        &list(json!([{"id":"b","state":"blocked"},{"id":"a","state":"working"}])),
        0.0,
    );
    assert_eq!(m.selected().unwrap().id, "b");
    let mut m = model(vec![]);
    m.apply(
        &list(json!([{"id":"a","state":"working"},{"id":"b","state":"working"}])),
        0.0,
    );
    m.move_by(1);
    m.apply(&list(json!([{"id":"a","state":"working"}])), 0.0);
    assert_eq!(m.selected().unwrap().id, "a");
    m.apply(&[], 0.0);
    assert!(m.selected().is_none());
    assert_eq!(m.cursor, 0);
}

#[test]
fn move_stays_inside_the_rows() {
    let mut m = model(vec![]);
    m.apply(
        &list(json!([{"id":"a","state":"working"},{"id":"b","state":"working"}])),
        0.0,
    );
    m.move_by(-1);
    assert_eq!(m.cursor, 0);
    m.move_by(5);
    assert_eq!(m.cursor, 2);
}

#[test]
fn events_change_a_known_pane_only() {
    let mut m = model(vec![]);
    m.apply(&list(json!([{"id":"a","state":"working","ts":10.0}])), 70.0);
    m.event(&o(json!({"event":"state","pane":"a","state":"blocked","source":"gate","detail":"","ask":{"tool":"Bash","summary":"ls"}})));
    let r = &m.rows[0];
    assert_eq!(
        (r.state.as_str(), r.source.as_str(), r.line.as_str(), r.age),
        ("blocked", "gate", "Bash: ls", 0.0)
    );
    assert_eq!(m.need_you, 1);
    let mut m = model(vec![]);
    m.apply(&list(json!([{"id":"a","state":"working"}])), 0.0);
    m.event(&o(
        json!({"event":"state","pane":"zzz","state":"blocked","source":"gate"}),
    ));
    m.event(&o(
        json!({"event":"state","pane":null,"state":"blocked","source":"gate"}),
    ));
    assert_eq!(m.rows.len(), 1);
    assert_eq!(m.rows[0].state, "working");
    assert_eq!(m.need_you, 0);
}

#[test]
fn apply_closes_the_peek_when_its_pane_is_gone_and_keeps_it_otherwise() {
    let mut m = model(vec![]);
    m.apply(
        &list(json!([{"id":"a","state":"working"},{"id":"b","state":"working"}])),
        0.0,
    );
    m.open_peek("text", None);
    assert_eq!(m.peek.as_ref().unwrap().pane, "a");
    m.apply(&list(json!([{"id":"b","state":"working"}])), 0.0);
    assert!(m.peek.is_none());
    let mut m = model(vec![]);
    m.apply(&list(json!([{"id":"a","state":"working"}])), 0.0);
    m.open_peek("text", None);
    m.apply(
        &list(json!([{"id":"a","state":"blocked"},{"id":"b","state":"working"}])),
        0.0,
    );
    assert_eq!(m.peek.as_ref().unwrap().pane, "a");
}

#[test]
fn next_need_cycles_through_asks_then_working() {
    let mut rows = vec![
        row("w", "working"),
        row("b1", "blocked"),
        row("b2", "blocked"),
    ];
    rows[1].age = 60.0;
    rows[2].age = 10.0;
    let mut m = model(rows);
    m.cursor = m.index_of("w", 0);
    assert_eq!(m.selected().unwrap().id, "w");
    for want in ["b1", "b2", "b1"] {
        m.next_need();
        assert_eq!(m.selected().unwrap().id, want);
    }
    let mut m = model(vec![
        row("d", "done"),
        row("w1", "working"),
        row("w2", "working"),
    ]);
    m.cursor = m.index_of("d", 0);
    for want in ["w1", "w2", "w1"] {
        m.next_need();
        assert_eq!(m.selected().unwrap().id, want);
    }
}

#[test]
fn next_need_does_nothing_with_nothing_to_go_to() {
    let mut m = model(vec![]);
    m.next_need();
    assert_eq!(m.cursor, 0);
    let mut m = model(vec![row("d1", "done"), row("d2", "done")]);
    m.cursor = 1;
    m.next_need();
    assert_eq!(m.cursor, 1);
}

// ---- render ----

#[test]
fn render_fits_the_screen_and_ends_with_the_prompt() {
    let mut m = model(vec![Row {
        id: "a".into(),
        label: "gate-refactor".into(),
        harness: "claude".into(),
        state: "blocked".into(),
        line: "wants git push --force".into(),
        ..Default::default()
    }]);
    m.need_you = 1;
    let lines = render(&mut m, 80, 12);
    assert_eq!(lines.len(), 12);
    assert!(lines[0].contains("1 needs you"));
    assert!(strip_sgr(&lines[11]).starts_with("› "));
    for l in &lines {
        assert!(textwidth::width(&strip_sgr(l)) <= 80);
    }
}

#[test]
fn every_line_is_exactly_cols_wide() {
    let mk = |id: &str, label: &str, h: &str, st: &str, line: &str| Row {
        id: id.into(),
        label: label.into(),
        harness: h.into(),
        state: st.into(),
        line: line.into(),
        ..Default::default()
    };
    let mut m = model(vec![
        mk(
            "a",
            "gate-refactor",
            "claude",
            "blocked",
            "wants git push --force",
        ),
        mk("b", "docs", "pi", "working", "editing README"),
        mk("c", "tests", "codex", "done", ""),
        mk("d", "", "", "idle", ""),
    ]);
    m.need_you = 1;
    m.message = "hello".into();
    m.prompt = b"close b".to_vec();
    for l in render(&mut m, 60, 14) {
        assert_eq!(textwidth::width(&strip_sgr(&l)), 60, "{l:?}");
    }
}

#[test]
fn the_bar_and_the_empty_floor_say_what_they_should() {
    let lines = render(&mut model(vec![row("a", "working")]), 40, 6);
    assert!(lines[0].starts_with("coppice · quiet"));
    let mut m = model(vec![]);
    m.default = "claude".into();
    assert!(render(&mut m, 80, 6)
        .join("\n")
        .contains("Nothing running. Enter opens claude here."));
    m.default = "pi".into();
    assert!(render(&mut m, 80, 6)
        .join("\n")
        .contains("Enter opens pi here."));
}

fn footer_at(lines: &[String], n: usize) -> String {
    lines[lines.len() - 1 - n..lines.len() - 1]
        .iter()
        .map(|l| strip_sgr(l).trim_end_matches(' ').to_string())
        .collect::<Vec<_>>()
        .join("  ")
}

#[test]
fn the_footer_is_the_key_hints_and_wraps_keeping_every_key() {
    let mut m = model(vec![row("a", "working")]);
    let lines = render(&mut m, 80, 10);
    let n = footer_lines(&mut m, 80).len();
    assert_eq!(n, 2);
    assert_eq!(
        footer_at(&lines, n),
        format!("{}  ctrl-\\ speak", join_keys(&table()))
    );
    for cols in [60, 80, 120, 200] {
        let fl = footer_lines(&mut m, cols);
        for l in &fl {
            assert_eq!(textwidth::width(l) as i64, cols);
        }
        let joined = fl.join(" ");
        for b in table() {
            assert!(joined.contains(&format!("{} {}", b.keys, b.help)));
        }
    }
}

#[test]
fn the_rail_groups_by_project_with_counts_and_by_task_first() {
    let mk = |id: &str, st: &str, wt: &str, task: &str| Row {
        id: id.into(),
        state: st.into(),
        worktree: wt.into(),
        task: task.into(),
        ..Default::default()
    };
    let mut m = model(vec![
        mk("w1:p1", "working", "/src/trellis", ""),
        mk("w1:p2", "blocked", "/src/glean", ""),
        mk("w1:p3", "idle", "/src/trellis", ""),
    ]);
    let out = plain(&render(&mut m, 80, 12));
    let glean = out.find("▾ glean  1 needs").unwrap();
    let trellis = out.find("▾ trellis  1 working · 1 idle").unwrap();
    assert!(trellis < glean);
    assert!(!out.contains("0 "));
    let mut m = model(vec![
        mk("w1:p1", "idle", "/src/glean", ""),
        mk("w1:p2", "working", "", "t1"),
        mk("w1:p3", "blocked", "", "t1"),
    ]);
    m.tasks = vec![
        TaskRow {
            id: "t1".into(),
            label: "trellis-fix".into(),
            ..Default::default()
        },
        TaskRow {
            id: "t2".into(),
            label: "empty".into(),
            ..Default::default()
        },
    ];
    let out = plain(&render(&mut m, 80, 12));
    let task = out.find("▾ trellis-fix  1 needs · 1 working").unwrap();
    let project = out.find("▾ glean  1 idle").unwrap();
    assert!(task < project);
    assert!(!out.contains("empty") && !out.contains("no task"));
}

#[test]
fn a_header_folds_and_rows_keep_their_order() {
    let mut a = Row {
        id: "w1:p2".into(),
        state: "working".into(),
        worktree: "/a".into(),
        age: 1.0,
        ..Default::default()
    };
    let mut b = a.clone();
    b.id = "w1:p10".into();
    b.age = 0.0;
    a.age = 1.0;
    let m = model(vec![a, b]);
    let got = m.tree_rows();
    assert_eq!(
        (got[0].id.as_str(), got[1].id.as_str()),
        ("w1:p2", "w1:p10")
    );
    let mut f = test_floor(m, 80);
    f.m.move_by(-1);
    assert_eq!(f.m.selected_item().unwrap().kind, ItemKind::Group);
    f.handle(byte(b'\r')).unwrap();
    let out = plain(&render(&mut f.m, 80, 10));
    assert!(
        out.contains("▸ a  2 working") && !out.contains("w1:p2"),
        "{out}"
    );
    f.handle(byte(b' ')).unwrap();
    assert!(plain(&render(&mut f.m, 80, 10)).contains("w1:p10"));
    f.m.event(&o(
        json!({"pane":"w1:p10","state":"blocked","source":"gate"}),
    ));
    assert_eq!(f.m.tree_rows()[0].id, "w1:p2");
}

#[test]
fn wide_glyphs_and_tiny_screens_never_overflow() {
    let mut m = model(vec![Row {
        id: "a".into(),
        label: "日本語".repeat(20),
        harness: "claude".into(),
        state: "working".into(),
        line: "字".repeat(40),
        ..Default::default()
    }]);
    for l in render(&mut m, 40, 8) {
        assert_eq!(textwidth::width(&strip_sgr(&l)), 40);
    }
    let rows: Vec<Row> = (0..30u8)
        .map(|i| row(&((b'a' + i) as char).to_string(), "working"))
        .collect();
    let mut m = model(rows);
    let lines = render(&mut m, 80, 6);
    assert_eq!(lines.len(), 6);
    assert!(strip_sgr(&lines[5]).starts_with("› "));
    assert_eq!(
        footer_at(&lines, 2),
        format!("{}  ctrl-\\ speak", join_keys(&table()))
    );
    assert_eq!(render(&mut m, 80, 1).len(), 1);
    assert!(render(&mut m, 80, 0).is_empty());
}

#[test]
fn the_cursor_marker_sits_on_the_cursor_row_only() {
    let mut m = model(vec![row("a", "working"), row("b", "working")]);
    m.cursor = 2;
    let marked: Vec<String> = render(&mut m, 60, 10)
        .iter()
        .map(|l| strip_sgr(l))
        .filter(|p| p.starts_with("  › "))
        .collect();
    assert_eq!(marked.len(), 1);
    assert!(marked[0].contains(" b "));
}

#[test]
fn states_are_colored_and_reset() {
    let mut m = model(vec![
        row("a", "blocked"),
        row("b", "working"),
        row("c", "done"),
        row("d", "idle"),
    ]);
    let lines = render(&mut m, 60, 14);
    let out = lines.join("\n");
    for code in ["\x1b[33m", "\x1b[34m", "\x1b[32m", "\x1b[2m"] {
        assert!(out.contains(code));
    }
    for l in lines {
        if l.contains("\x1b[") {
            assert!(l.ends_with("\x1b[0m"), "{l:?}");
        }
    }
}

#[test]
fn the_message_and_the_prompt_show() {
    let mut m = model(vec![row("a", "working")]);
    m.message = "swap is not wired yet.".into();
    let lines = render(&mut m, 60, 12);
    let at = lines.len() - 2 - footer_lines(&mut m, 60).len();
    assert!(strip_sgr(&lines[at]).starts_with("swap is not wired yet."));
    let mut m = model(vec![]);
    m.prompt = b"close w1:p1".to_vec();
    assert!(strip_sgr(&render(&mut m, 60, 5)[4]).starts_with("› close w1:p1"));
    assert_eq!(strip_sgr("\x1b[33mab\x1b[0m"), "ab");
    assert_eq!(strip_sgr("plain"), "plain");
}

#[test]
fn the_peek_renders_the_ask_the_verdict_and_the_last_text() {
    let mut m = model(vec![Row {
        id: "a".into(),
        label: "x".into(),
        state: "blocked".into(),
        ..Default::default()
    }]);
    m.open_peek(
        "line1\nline2",
        Some(&o(
            json!({"tool":"git","summary":"push --force","gate":{"verdict":"deny","rule":3}}),
        )),
    );
    let lines = render(&mut m, 80, 20);
    let (mut header, mut ask, mut verdict) = (0, 0, 0);
    for (i, l) in lines.iter().enumerate() {
        let p = strip_sgr(l);
        if p.starts_with("peek a x") {
            header = i;
        } else if p.starts_with("asks: git: push --force") {
            ask = i;
        } else if p.starts_with("The gate says no, rule 3") {
            verdict = i;
        }
    }
    assert!(header > 0 && ask == header + 1 && verdict == header + 2);
    let out = lines.join("\n");
    assert!(out.contains("line1") && out.contains("line2"));
    let mut m = model(vec![row("a", "blocked")]);
    m.open_peek(
        "",
        Some(&o(
            json!({"summary":"ls","gate":{"verdict":"allow","rule":1.0}}),
        )),
    );
    assert!(render(&mut m, 80, 20)
        .join("\n")
        .contains("The gate says yes"));
    m.open_peek("text", Some(&o(json!({"summary":"ls"}))));
    let out = render(&mut m, 80, 20).join("\n");
    assert!(!out.contains("The gate says") && out.contains("asks: ls"));
}

#[test]
fn the_peek_text_is_the_last_lines_that_fit() {
    let mut m = model(vec![row("a", "working")]);
    let text: Vec<String> = (1..=40).map(|i| format!("row{}", "x".repeat(i))).collect();
    m.open_peek(&format!("{}\n", text.join("\n")), None);
    let lines = render(&mut m, 80, 12);
    assert_eq!(lines.len(), 12);
    let out = lines.join("\n");
    assert!(out.contains(&format!("row{}", "x".repeat(40))));
    assert!(!out.contains(&format!("row{}\n", "x".repeat(30))));
    assert!(out.contains("▾ "));
    m.close_peek();
    m.open_peek("secret", None);
    m.close_peek();
    assert!(!render(&mut m, 80, 12).join("\n").contains("secret"));
}

#[test]
fn no_escape_from_a_label_reaches_the_screen() {
    let evil = "x\x1b]0;owned\x07\x1b[2J\u{9b}1m";
    let mut m = model(vec![Row {
        id: "a".into(),
        label: evil.into(),
        state: "blocked".into(),
        line: evil.into(),
        ..Default::default()
    }]);
    m.ended = vec![Row {
        id: "e".into(),
        label: evil.into(),
        state: "ended".into(),
        exit_code: Some(2),
        ..Default::default()
    }];
    m.message = format!("{evil} stopped.");
    m.need_you = 1;
    let e = m.ended[0].clone();
    m.note_ended(&e);
    let check = |lines: Vec<String>| {
        for l in lines {
            let p = strip_sgr(&l);
            assert!(!p.contains(['\x1b', '\x07', '\u{9b}']), "{p:?}");
        }
    };
    check(render(&mut m, 100, 20));
    m.renaming = "a".into();
    check(render(&mut m, 100, 20));
    m.renaming.clear();
    m.confirm = "a".into();
    check(render(&mut m, 100, 20));
}

// ---- words.json ----

#[test]
fn the_shared_words_match_the_web_page() {
    let raw = std::fs::read(format!(
        "{}/../coppice/testdata/words.json",
        env!("CARGO_MANIFEST_DIR")
    ))
    .unwrap();
    let w: Obj = serde_json::from_slice(&raw).unwrap();
    let r = |code: Option<i64>| Row {
        id: "x".into(),
        label: "fine".into(),
        exit_code: code,
        ..Default::default()
    };
    let got: Vec<(&str, Value)> = vec![
        ("ended", json!(ended_text(&r(None)))),
        ("ended_exit_0", json!(ended_text(&r(Some(0))))),
        ("ended_exit_3", json!(ended_text(&r(Some(3))))),
        ("ended_killed", json!(ended_text(&r(Some(-1))))),
        ("kill_confirm", json!(stop_question("fine"))),
        ("clear_all_confirm_2", json!(clear_all_question(2))),
        ("clear_all_confirm_1", json!(clear_all_question(1))),
        ("blocked_by_gate", json!(BLOCKED_BY_GATE)),
        ("blocked_own_question", json!(BLOCKED_OWN_QUESTION)),
        (
            "ended_clear_ms",
            json!(super::recent::ENDED_CLEAR.as_millis() as f64),
        ),
    ];
    for (k, v) in &got {
        let want = w.get(*k).unwrap_or_else(|| panic!("words.json lacks {k}"));
        if let (Some(a), Some(b)) = (v.as_f64(), want.as_f64()) {
            assert_eq!(a, b, "{k}");
        } else {
            assert_eq!(v, want, "{k}");
        }
    }
    assert_eq!(got.len(), w.len() - 1);
}

#[test]
fn a_block_with_no_ask_says_who_found_it() {
    let rows = rows_from(
        &list(json!([
            {"id":"a","state":"blocked","source":"manifest","detail":"live_prompt_box"},
            {"id":"b","state":"blocked","source":"gate"},
            {"id":"c","state":"blocked","source":"gate","ask":{"tool":"Bash","summary":"rm"}}
        ])),
        0.0,
    );
    assert_eq!(rows[0].line, BLOCKED_OWN_QUESTION);
    assert_eq!(rows[1].line, BLOCKED_BY_GATE);
    assert_eq!(rows[2].line, "Bash: rm");
}

// ---- the prompt ----

#[test]
fn harness_names_and_short_verb_lines_are_plumbing() {
    let h = ["claude", "pi"];
    assert_eq!(classify("claude", &h), Kind::Plumbing);
    assert_eq!(classify("pi --model llama", &h), Kind::Plumbing);
    assert_eq!(classify("close docs", &h), Kind::Plumbing);
    assert_eq!(
        classify("open a pane with pi on the docs", &h),
        Kind::TalkLine
    );
    let h = HARNESSES;
    assert_eq!(classify("open claude --model opus", h), Kind::Plumbing);
    assert_eq!(
        classify("open claude --model opus --verbose", h),
        Kind::TalkLine
    );
    assert_eq!(
        classify("pi please fix the docs and then the tests", h),
        Kind::Plumbing
    );
    assert_eq!(classify("please open claude", h), Kind::TalkLine);
    assert_eq!(classify("  list  ", h), Kind::Plumbing);
    assert_eq!(classify("", h), Kind::TalkLine);
    assert_eq!(HARNESSES.join(" "), "claude codex pi sprig opencode");
    assert_eq!(
        VERBS.join(" "),
        "list read close open swap rotate reset lock unlock layout tree zoom foreman"
    );
    assert!(PROMOTION_LINE.starts_with("Run: coppice skill foreman."));
}

#[test]
fn open_peek_reads_the_ask_and_the_gate() {
    let mut m = model(vec![Row {
        id: "a".into(),
        label: "x".into(),
        state: "blocked".into(),
        ..Default::default()
    }]);
    m.open_peek(
        "line1\nline2",
        Some(&o(
            json!({"summary":"push --force","tool":"git","gate":{"verdict":"deny","rule":3.0}}),
        )),
    );
    let p = m.peek.clone().unwrap();
    assert_eq!(
        (p.pane.as_str(), p.label.as_str(), p.text.as_str()),
        ("a", "x", "line1\nline2")
    );
    assert_eq!(
        (p.ask.as_str(), p.tool.as_str()),
        ("git: push --force", "git")
    );
    assert!(p.has_gate && p.verdict == "deny" && p.rule == 3);
    let mut m = model(vec![]);
    m.open_peek("text", None);
    assert!(m.peek.is_none());
}

fn prompt_floor() -> (Floor<'static>, Gone) {
    let mut rows = vec![
        row("a", "working"),
        row("b", "working"),
        row("c", "working"),
    ];
    for (i, r) in rows.iter_mut().enumerate() {
        r.age = (i + 1) as f64;
    }
    let mut m = model(rows);
    m.tiles = Some(Tiles::new(Layout::Focus, 2));
    let mut f = test_floor(m, 200);
    let dir = scratch("prompt");
    f.o.data_dir = dir.display().to_string();
    (f, Gone(dir))
}

fn run_prompt(f: &mut Floor, line: &str) {
    f.m.talking = true;
    f.m.prompt = line.as_bytes().to_vec();
    f.enter().unwrap();
}

fn tiles<'a>(f: &'a mut Floor<'_>) -> &'a mut Tiles {
    f.m.tiles.as_mut().unwrap()
}

#[test]
fn swap_rotate_and_reset_move_the_slots() {
    let (mut f, _dir) = prompt_floor();
    tiles(&mut f).fill("a");
    tiles(&mut f).fill("b");
    tiles(&mut f).focus = 0;
    run_prompt(&mut f, "swap 1 2");
    assert_eq!(tiles(&mut f).slots, ["b", "a"]);
    assert_eq!(tiles(&mut f).focused(), "a");
    assert_eq!(f.m.message, "");
    for line in ["swap", "swap 1", "swap a b", "swap 1 2 3"] {
        let (mut f, _dir) = prompt_floor();
        run_prompt(&mut f, line);
        assert_eq!(f.m.message, SWAP_NEEDS_MESSAGE, "{line}");
    }
    for line in ["swap 1 3", "swap 0 1", "swap 2 -1"] {
        let (mut f, _dir) = prompt_floor();
        run_prompt(&mut f, line);
        assert_eq!(f.m.message, "swap needs slot numbers from 1 to 2", "{line}");
    }
    let (mut f, _dir) = prompt_floor();
    tiles(&mut f).fill("c");
    tiles(&mut f).fill("b");
    run_prompt(&mut f, "reset");
    assert_eq!(tiles(&mut f).slots, ["a", "b"]);
    let (mut f, _dir) = prompt_floor();
    tiles(&mut f).fill("a");
    tiles(&mut f).fill("b");
    run_prompt(&mut f, "rotate");
    assert_eq!(tiles(&mut f).slots, ["b", "a"]);
}

#[test]
fn lock_refuses_until_unlock() {
    let (mut f, _dir) = prompt_floor();
    tiles(&mut f).fill("a");
    tiles(&mut f).fill("b");
    run_prompt(&mut f, "lock");
    for line in ["swap 1 2", "rotate", "reset", "layout all"] {
        run_prompt(&mut f, line);
        assert_eq!(
            f.m.message, "Layout is locked. Type unlock first.",
            "{line}"
        );
    }
    assert_eq!(tiles(&mut f).slots, ["a", "b"]);
    run_prompt(&mut f, "unlock");
    run_prompt(&mut f, "swap 1 2");
    assert_eq!(tiles(&mut f).slots[0], "b");
}

#[test]
fn layout_words_rebuild_the_tiles_and_are_kept_in_floor_json() {
    let (mut f, _dir) = prompt_floor();
    tiles(&mut f).fill("c");
    run_prompt(&mut f, "layout all");
    assert_eq!(tiles(&mut f).layout, Layout::All);
    assert_eq!(tiles(&mut f).slots.join(" "), "c a b");
    run_prompt(&mut f, "layout one");
    assert_eq!(tiles(&mut f).slots.join(" "), "c");
    run_prompt(&mut f, "layout focus");
    assert_eq!(tiles(&mut f).slots.join(" "), "c a");
    for line in ["layout", "layout grid"] {
        let (mut f, _dir) = prompt_floor();
        run_prompt(&mut f, line);
        assert_eq!(f.m.message, LAYOUT_NEEDS_MESSAGE);
    }
    let (mut f, _dir) = prompt_floor();
    run_prompt(&mut f, "layout one");
    run_prompt(&mut f, "lock");
    let dir = f.o.data_dir.clone();
    let b = std::fs::read_to_string(format!("{dir}/floor.json")).unwrap();
    assert_eq!(b.trim(), r#"{"layout":"one","locked":true}"#);
    assert_eq!(read_floor_state(&dir), (Layout::One, true));
    let names: Vec<String> = std::fs::read_dir(&dir)
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    assert_eq!(names, ["floor.json"]);
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_missing_or_unreadable_floor_json_is_focus_unlocked() {
    let dir = scratch("floorjson");
    let d = dir.display().to_string();
    assert_eq!(read_floor_state(&d), (Layout::Focus, false));
    std::fs::write(dir.join("floor.json"), "not json").unwrap();
    assert_eq!(read_floor_state(&d), (Layout::Focus, false));
    std::fs::write(dir.join("floor.json"), r#"{"layout":"grid","locked":true}"#).unwrap();
    assert_eq!(read_floor_state(&d), (Layout::Focus, true));
    assert_eq!(read_floor_state(""), (Layout::Focus, false));
    let _ = std::fs::remove_dir_all(&dir);
}

// ---- the answer keys ----

/// A fake server that records every request and answers ok, or with the
/// refusal refuse returns.
struct AnswerSocket {
    _dir: Gone,
    path: PathBuf,
    reqs: Arc<Mutex<Vec<Obj>>>,
}

fn answer_socket(refuse: fn(&Obj) -> String) -> AnswerSocket {
    let dir = scratch("answer");
    let path = dir.join("s.sock");
    let ln = UnixListener::bind(&path).unwrap();
    let reqs = Arc::new(Mutex::new(Vec::new()));
    let r2 = reqs.clone();
    std::thread::spawn(move || {
        for c in ln.incoming() {
            let Ok(mut c) = c else { return };
            let mut line = String::new();
            if BufReader::new(&c).read_line(&mut line).is_err() {
                continue;
            }
            let req: Obj = serde_json::from_str(&line).unwrap_or_default();
            r2.lock().unwrap().push(req.clone());
            let msg = refuse(&req);
            let out = if msg.is_empty() {
                json!({"id": req.get("id"), "ok": true, "result": {}})
            } else {
                json!({"id": req.get("id"), "ok": false, "error": {"code": "unauthorized", "message": msg}})
            };
            let _ = c.write_all(format!("{out}\n").as_bytes());
        }
    });
    AnswerSocket {
        _dir: Gone(dir),
        path,
        reqs,
    }
}

impl AnswerSocket {
    fn requests(&self) -> Vec<Obj> {
        self.reqs.lock().unwrap().clone()
    }
}

fn never(_: &Obj) -> String {
    String::new()
}

fn peek_floor(label: &str, tier: &str, sock: &AnswerSocket) -> Floor<'static> {
    let mut m = model(vec![Row {
        id: "w1:p1".into(),
        label: label.into(),
        state: "blocked".into(),
        ..Default::default()
    }]);
    let mut ask = json!({"id":"toolu_1","tool":"Bash","summary":"git push --force"});
    if !tier.is_empty() {
        ask["tier"] = json!(tier);
    }
    m.open_peek("screen text", Some(&o(ask)));
    let mut f = test_floor(m, 100);
    f.o.socket = sock.path.clone();
    f
}

fn press(f: &mut Floor, keys: &[u8]) {
    for b in keys {
        f.handle(byte(*b)).unwrap();
    }
}

fn rendered(f: &mut Floor) -> String {
    render(&mut f.m, 100, 20).join("\n")
}

#[test]
fn a_permanent_peek_asks_for_the_name() {
    let sock = answer_socket(never);
    let mut f = peek_floor("gate-refactor", "permanent", &sock);
    let out = rendered(&mut f);
    for want in [
        "Deny is the default.",
        "n deny",
        "type gate-refactor and enter",
    ] {
        assert!(out.contains(want), "{out}");
    }
    assert!(!out.contains("y allow once"));
    let mut f = peek_floor("gate-refactor", "", &sock);
    assert!(rendered(&mut f).contains("type gate-refactor and enter"));
    let mut f = peek_floor("", "permanent", &sock);
    assert!(rendered(&mut f).contains("type w1:p1 and enter"));
}

#[test]
fn y_on_a_permanent_peek_only_says_type_the_name() {
    let sock = answer_socket(never);
    let mut f = peek_floor("gate-refactor", "permanent", &sock);
    press(&mut f, b"y");
    assert_eq!(f.m.message, "type the pane name to allow");
    assert!(sock.requests().is_empty());
    assert!(f.m.prompt.is_empty() && !f.m.talking);
    press(&mut f, b"t");
    assert_eq!(f.m.message, "type the pane name to allow");
    assert!(sock.requests().is_empty());
}

#[test]
fn the_name_then_enter_allows_and_a_wrong_name_allows_nothing() {
    let sock = answer_socket(never);
    let mut f = peek_floor("gate-refactor", "permanent", &sock);
    press(&mut f, b"\x14gate-refactor\r");
    let reqs = sock.requests();
    assert_eq!(reqs.len(), 1);
    assert_eq!(reqs[0]["cmd"], "agent.allow");
    assert_eq!(reqs[0]["confirm"], "gate-refactor");
    assert_eq!(reqs[0]["pane"], "w1:p1");
    assert_eq!(reqs[0]["ask"], "toolu_1");
    assert!(f.m.peek.is_none());
    let sock = answer_socket(never);
    let mut f = peek_floor("gate-refactor", "permanent", &sock);
    press(&mut f, b"\x14gate-refactr\r");
    assert!(sock.requests().is_empty());
    assert!(f.m.message.contains("not the pane name"));
    assert!(f.m.peek.is_some());
    press(&mut f, b"n");
    let reqs = sock.requests();
    assert_eq!(reqs.len(), 1);
    assert_eq!(reqs[0]["cmd"], "agent.deny");
}

#[test]
fn n_denies_y_allows_once_and_t_allows_for_the_task() {
    for tier in ["permanent", "undoable"] {
        let sock = answer_socket(never);
        let mut f = peek_floor("gate-refactor", tier, &sock);
        press(&mut f, b"n");
        let reqs = sock.requests();
        assert_eq!(reqs.len(), 1);
        assert_eq!(reqs[0]["cmd"], "agent.deny");
        assert!(!reqs[0].contains_key("confirm"));
    }
    let sock = answer_socket(never);
    let mut f = peek_floor("gate-refactor", "undoable", &sock);
    assert!(rendered(&mut f).contains("y allow once  t allow for this task  n deny"));
    press(&mut f, b"y");
    let reqs = sock.requests();
    assert_eq!(reqs[0]["cmd"], "agent.allow");
    assert!(!reqs[0].contains_key("confirm") && !reqs[0].contains_key("scope"));
    let sock = answer_socket(never);
    let mut f = peek_floor("gate-refactor", "undoable", &sock);
    press(&mut f, b"t");
    assert_eq!(sock.requests()[0]["scope"], "task");
}

#[test]
fn a_refusal_shows_and_keeps_the_peek() {
    let sock = answer_socket(|_| {
        "this cannot be undone. Type the pane name to allow: gate-refactor".into()
    });
    let mut f = peek_floor("gate-refactor", "undoable", &sock);
    press(&mut f, b"y");
    assert!(f.m.message.contains("this cannot be undone"));
    assert!(f.m.peek.is_some());
}

#[test]
fn keys_with_no_ask_are_text() {
    let sock = answer_socket(never);
    let mut m = model(vec![row("w1:p1", "working")]);
    m.open_peek("text", None);
    let mut f = test_floor(m, 100);
    f.o.socket = sock.path.clone();
    press(&mut f, b"\x14ytn");
    assert_eq!(f.m.prompt, b"ytn");
    assert!(sock.requests().is_empty());
}

#[test]
fn the_trust_peek_answers_with_y_and_n() {
    let detail = "asks to trust this folder (rule=coppice_first_run_trust region=whole_recent)";
    let rows = rows_from(
        &list(
            json!([{"id":"w1:p1","label":"claude-new","state":"blocked","source":"manifest","detail":detail}]),
        ),
        0.0,
    );
    assert!(rows[0].trust && rows[0].line == TRUST_LINE);
    for (key, want) in [(b'y', true), (b'n', false)] {
        let sock = answer_socket(never);
        let mut m = model(vec![Row {
            id: "w1:p1".into(),
            label: "claude-new".into(),
            state: "blocked".into(),
            trust: true,
            ..Default::default()
        }]);
        m.open_peek("screen text", None);
        assert!(peek_lines(m.peek.as_ref().unwrap(), 100, 10)
            .join("\n")
            .contains(TRUST_KEYS));
        let mut f = test_floor(m, 100);
        f.o.socket = sock.path.clone();
        press(&mut f, &[key]);
        let reqs = sock.requests();
        assert_eq!(reqs.len(), 1);
        assert_eq!(reqs[0]["cmd"], "pane.trust");
        assert_eq!(reqs[0]["trust"], want);
        assert!(f.m.peek.is_none());
    }
}

// ---- subagents, holds and looking ----

fn list_with_child() -> Vec<Obj> {
    list(json!([
        {"id":"w1:p1","label":"floor","harness":"claude","state":"working","ts":10.0,
         "children":[{"id":"a1","label":"Explore","state":"working","ts":11.0}]},
        {"id":"w1:p2","label":"docs","harness":"claude","state":"working","ts":9.0}
    ]))
}

#[test]
fn a_child_row_sits_under_its_parent() {
    let mut m = model(vec![]);
    m.apply(&list_with_child(), 12.0);
    let all = m.tree_rows();
    assert_eq!(all.len(), 3);
    assert_eq!(all[1].parent, "w1:p1");
    assert_eq!(all[1].label, "Explore");
    let lines: Vec<String> = render(&mut m, 80, 12)
        .iter()
        .map(|l| strip_sgr(l))
        .collect();
    let p = lines
        .iter()
        .position(|l| l.contains("floor  claude"))
        .unwrap();
    assert!(lines[p + 1].contains("Explore"));
    let col = |l: &str, w: &str| l[..l.find(w).unwrap()].chars().count();
    assert_eq!(col(&lines[p + 1], "Explore"), col(&lines[p], "floor") + 2);
}

#[test]
fn keys_on_a_child_say_why() {
    let mut m = model(vec![]);
    m.tiles = Some(Tiles::new(Layout::Focus, 1));
    m.apply(&list_with_child(), 12.0);
    m.cursor = m.index_of("w1:p1/a1", 0);
    let mut f = test_floor(m, 140);
    for b in [b'\r', b' ', 0x17] {
        f.handle(byte(b)).unwrap();
        assert_eq!(f.m.message, CHILD_MESSAGE);
        assert!(f.m.typing.is_empty() && f.m.confirm.is_empty() && f.m.peek.is_none());
        assert!(tiles(&mut f).slots.iter().all(|s| s.is_empty()));
        f.m.message.clear();
    }
    run_prompt(&mut f, "zoom");
    assert_eq!(f.m.message, CHILD_MESSAGE);
}

#[test]
fn child_events_update_and_children_count_for_nothing() {
    let mut m = model(vec![]);
    m.apply(&list_with_child(), 12.0);
    m.child(&o(
        json!({"event":"child","pane":"w1:p1","child":"a1","state":"done","label":"Explore"}),
    ));
    m.child(&o(
        json!({"event":"child","pane":"w1:p1","child":"a2","state":"working","label":"Plan"}),
    ));
    let all = m.tree_rows();
    assert_eq!(all.len(), 4);
    assert_eq!(all[1].state, "done");
    assert_eq!(all[2].label, "Plan");
    let mut l = list_with_child();
    l[0].insert(
        "children".into(),
        json!([{"id":"a1","label":"Explore","state":"blocked"}]),
    );
    let mut m = model(vec![]);
    m.apply(&l, 12.0);
    assert_eq!(m.need_you, 0);
    m.cursor = 0;
    m.next_need();
    assert!(m.selected().unwrap().parent.is_empty());
    assert!(m.roster_order().iter().all(|id| !id.contains('/')));
}

#[test]
fn a_held_ask_renders_under_working() {
    let held = || {
        list(json!([
            {"id":"w1:p1","label":"writer","state":"blocked","ts":100.0,"task":"t2",
             "ask":{"id":"ask-1","summary":"rm -rf build/","tier":"undoable"},
             "held":{"by":"w1:p9","task":"t1","task_label":"review-team","since":40.0,"until":160.0}},
            {"id":"w1:p2","label":"loner","state":"blocked","ts":100.0,
             "ask":{"id":"ask-2","summary":"git push","tier":"permanent"}}
        ]))
    };
    let mut m = model(vec![]);
    m.apply(&held(), 100.0);
    assert_eq!(m.need_you, 1);
    let secs = group(&m.rows);
    assert_eq!(secs[0].rows[0].id, "w1:p2");
    assert_eq!(secs[1].rows[0].id, "w1:p1");
    let text = plain(&render(&mut m, 100, 20));
    assert!(
        text.contains("waiting on review-team's foreman · 1m"),
        "{text}"
    );
    assert!(text.contains("▾ t2  1 working") && text.contains("▾ no directory  1 needs"));
    m.event(&o(json!({"pane":"w1:p1","state":"blocked","source":"gate","ask":{"id":"ask-1","summary":"rm -rf build/"}})));
    assert_eq!(m.need_you, 2);
}

#[test]
fn a_row_says_who_is_looking() {
    let rows = rows_from(
        &list(json!([
            {"id":"a","label":"auth","state":"working","looking":["alice","bob"]},
            {"id":"b","label":"docs","state":"working"}
        ])),
        0.0,
    );
    assert_eq!(rows[0].looking.join(","), "alice,bob");
    assert!(rows[1].looking.is_empty());
    let one = Row {
        id: "a".into(),
        label: "auth".into(),
        state: "working".into(),
        looking: vec!["alice".into()],
        ..Default::default()
    };
    assert!(strip_sgr(&row_line(&one, false, false, false, "", 80)).contains("· alice looking"));
    assert!(strip_sgr(&row_line(&rows[0], false, false, false, "", 80))
        .contains("· alice, bob looking"));
    assert!(!strip_sgr(&row_line(&rows[1], false, false, false, "", 80)).contains("looking"));
}

// ---- views and the tree ----

#[test]
fn a_view_id_alone_says_where_the_view_lives() {
    let mut m = model(vec![]);
    m.tiles = Some(Tiles::new(Layout::Focus, 1));
    let mut f = test_floor(m, 120);
    f.o.views = vec!["tree".into(), "minimap".into()];
    f.o.web_url = "https://127.0.0.1:8443".into();
    run_prompt(&mut f, "minimap");
    assert_eq!(
        f.m.message,
        "views live on the floor page: https://127.0.0.1:8443/#/view/minimap"
    );
    f.o.web_url.clear();
    run_prompt(&mut f, "minimap");
    assert_eq!(
        f.m.message,
        "views live on the floor page, and the web server is off. Run: coppice web serve --persist"
    );
    assert_eq!(classify("minimap", HARNESSES), Kind::TalkLine);
    assert_eq!(
        classify_with("minimap", HARNESSES, &["minimap".into()]),
        Kind::Plumbing
    );
    assert_eq!(
        classify_with("minimap shows the map", HARNESSES, &["minimap".into()]),
        Kind::TalkLine
    );
}

#[test]
fn render_tree_matches_the_shared_fixture() {
    let raw = std::fs::read(format!(
        "{}/../coppice/internal/web/static/_tests/fixtures/tree.json",
        env!("CARGO_MANIFEST_DIR")
    ))
    .unwrap();
    let fx: Value = serde_json::from_slice(&raw).unwrap();
    let tasks = decode_tasks(&list(fx["tasks"].clone()));
    let rows: Vec<Row> = fx["panes"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| Row {
            id: p["id"].as_str().unwrap_or("").into(),
            label: p["label"].as_str().unwrap_or("").into(),
            state: p["state"].as_str().unwrap_or("").into(),
            ..Default::default()
        })
        .collect();
    let want: Vec<String> = fx["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| l.as_str().unwrap().to_string())
        .collect();
    assert_eq!(
        render_tree(&tasks, &rows, fx["cols"].as_i64().unwrap()),
        want
    );
}

fn tree_fixture() -> (Vec<TaskRow>, Vec<Row>) {
    let t = |id: &str, label: &str, parent: &str, wt: &str, st: &str, panes: &[&str]| TaskRow {
        id: id.into(),
        label: label.into(),
        parent: parent.into(),
        worktree: wt.into(),
        state: st.into(),
        panes: panes.iter().map(|s| s.to_string()).collect(),
        ..Default::default()
    };
    let mut tasks = vec![
        t("t1", "review-team", "", "", "working", &[]),
        t("t2", "docs", "t1", "", "working", &["w1:p1", "w1:p2"]),
        t(
            "t3",
            "gate-refactor",
            "",
            "/repo-worktrees/gate-refactor",
            "blocked",
            &["w1:p3"],
        ),
    ];
    tasks[2].ahead = 3;
    tasks[2].has_ahead = true;
    let p = |id: &str, label: &str, task: &str, st: &str| Row {
        id: id.into(),
        label: label.into(),
        task: task.into(),
        state: st.into(),
        ..Default::default()
    };
    let panes = vec![
        p("w1:p1", "writer", "t2", "working"),
        p("w1:p2", "checker", "t2", "idle"),
        p("w1:p3", "claude", "t3", "blocked"),
        p("w1:p4", "loose", "", "working"),
    ];
    (tasks, panes)
}

#[test]
fn render_tree_draws_tasks_then_panes() {
    let (tasks, panes) = tree_fixture();
    let lines = render_tree(&tasks, &panes, 80);
    let text = lines.join("\n");
    for want in [
        "├─",
        "└─",
        "review-team",
        "docs",
        "gate-refactor",
        "writer",
        "checker",
        "● needs you",
        "● working",
        "○ idle",
        "+3",
    ] {
        assert!(text.contains(want), "{want}: {text}");
    }
    assert!(!text.contains("/repo-worktrees"));
    assert_eq!(text.matches("gate-refactor").count(), 2);
    assert!(!text.contains("loose"));
    let docs = lines.iter().position(|l| l.contains("docs")).unwrap();
    assert!(lines[docs + 1].contains("writer"));
    for cols in [80, 24, 8] {
        for l in render_tree(&tasks, &panes, cols) {
            assert!(textwidth::width(&l) as i64 <= cols);
        }
    }
    assert!(render_tree(&[], &[], 80).is_empty());
}

#[test]
fn the_tree_cursor_walks_task_lines_only_and_push_scopes() {
    let (tasks, panes) = tree_fixture();
    let mut v = new_tree_view(&tasks, &panes, 80);
    assert_eq!(v.lines.len(), v.task_at.len());
    assert_eq!(v.selected(), "t1");
    for want in ["t2", "t3", "t3"] {
        v.move_by(1);
        assert_eq!(v.selected(), want);
    }
    v.move_by(-5);
    assert_eq!(v.selected(), "t1");
    assert_eq!(new_tree_view(&[], &[], 80).selected(), "");
    let rows = decode_tasks(&list(json!([
        {"id":"t1","label":"a","parent":"","worktree":"/w/a","model":"opus","state":"blocked","panes":["w1:p1"],"ahead":2.0},
        {"id":"t2","label":"b","parent":"t1","state":""}
    ])));
    assert!(rows[0].has_ahead && rows[0].ahead == 2 && rows[0].panes == ["w1:p1"]);
    assert!(rows[1].parent == "t1" && !rows[1].has_ahead && rows[1].panes.is_empty());
    let mut m = model(panes);
    m.tasks = tasks;
    m.push("t1");
    let ids = m.roster_order();
    assert_eq!(ids.len(), 2);
    m.pop();
    assert_eq!(m.roster_order().len(), 4);
}

// ---- the talk key ----

fn at(base: Instant, ms: u64) -> Instant {
    base + Duration::from_millis(ms)
}

#[test]
fn parse_kitty_csi_reads_keys_and_the_query_answer() {
    let k = |code, mods, event| KittyKey {
        code,
        mods,
        event,
        query: false,
    };
    let q = KittyKey {
        query: true,
        ..Default::default()
    };
    for (seq, want) in [
        (&b"\x1b[92;5u"[..], k(92, 5, KITTY_PRESS)),
        (b"\x1b[92;5:1u", k(92, 5, KITTY_PRESS)),
        (b"\x1b[92;5:2u", k(92, 5, KITTY_REPEAT)),
        (b"\x1b[92;1:3u", k(92, 1, KITTY_RELEASE)),
        (b"\x1b[97:65;6u", k(97, 6, KITTY_PRESS)),
        (b"\x1b[27u", k(27, 1, KITTY_PRESS)),
        (b"\x1b[?0u", q),
        (b"\x1b[?15u", q),
        (b"\x1b[99;5:3;99u", k(99, 5, KITTY_RELEASE)),
    ] {
        assert_eq!(parse_kitty_csi(seq), Some(want), "{seq:?}");
    }
    for seq in [&b"\x1b[A"[..], b"\x1b[?u", b"\x1b[xu", b"\x1b[92;xu", b"x"] {
        assert_eq!(parse_kitty_csi(seq), None, "{seq:?}");
    }
    assert!(is_talk_code(0x1c, b'\\' as i64) && is_talk_code(0x07, b'g' as i64));
    assert!(is_talk_code(0x00, b' ' as i64) && is_talk_code(0x1e, b'6' as i64));
    assert!(!is_talk_code(0x1c, b'g' as i64));
}

#[test]
fn hold_to_talk_where_the_terminal_answers() {
    let base = Instant::now();
    let mut s = Speaker::default();
    let act = s.feed(SpeakInput::TalkByte, at(base, 0));
    assert!(act.start && act.write == KITTY_QUERY && s.recording && !s.hold);
    let act = s.feed(SpeakInput::KittyAnswer, at(base, 5));
    assert!(act.write == KITTY_PUSH && s.hold);
    for input in [
        SpeakInput::TalkByte,
        SpeakInput::KittyRepeat,
        SpeakInput::KittyRepeat,
    ] {
        let act = s.feed(input, at(base, 600));
        assert!(!act.stop && !act.start && act.write.is_empty());
    }
    let act = s.feed(SpeakInput::KittyRelease, at(base, 2000));
    assert!(act.stop && act.write == KITTY_POP && !s.recording && !s.hold);
    let act = s.feed(SpeakInput::TalkByte, at(base, 5000));
    assert!(act.start && act.write == KITTY_PUSH);
}

#[test]
fn a_device_answer_with_no_kitty_answer_means_no() {
    let base = Instant::now();
    let mut s = Speaker::default();
    s.feed(SpeakInput::TalkByte, at(base, 0));
    s.feed(SpeakInput::DeviceAnswer, at(base, 5));
    assert!(s.kitty == KittySupport::No && !s.hold);
    let mut k = Speaker::default();
    k.feed(SpeakInput::TalkByte, at(base, 0));
    k.feed(SpeakInput::KittyAnswer, at(base, 4));
    k.feed(SpeakInput::DeviceAnswer, at(base, 5));
    assert!(k.kitty == KittySupport::Yes && k.hold);
    let mut s = Speaker {
        kitty: KittySupport::Yes,
        ..Default::default()
    };
    s.feed(SpeakInput::TalkByte, at(base, 0));
    let act = s.feed(SpeakInput::KittyPress, at(base, 3000));
    assert!(act.stop && act.write == KITTY_POP);
}

#[test]
fn press_to_start_and_press_to_stop_where_the_terminal_is_silent() {
    let base = Instant::now();
    let mut s = Speaker::default();
    s.feed(SpeakInput::TalkByte, at(base, 0));
    let mut ms = 500;
    while ms < 2000 {
        assert!(!s.feed(SpeakInput::TalkByte, at(base, ms)).stop, "{ms}");
        ms += 30;
    }
    let act = s.feed(SpeakInput::TalkByte, at(base, 4000));
    assert!(act.stop && act.write.is_empty() && !s.recording);
    assert_eq!(s.kitty, KittySupport::No);
    let act = s.feed(SpeakInput::TalkByte, at(base, 9000));
    assert!(act.start && act.write.is_empty());
    assert_eq!(
        s.speak_line("ctrl-\\"),
        "recording… press ctrl-\\ to stop  Esc throws it away"
    );
}

#[test]
fn esc_throws_the_clip_away_and_close_pops_once() {
    let base = Instant::now();
    let mut s = Speaker {
        kitty: KittySupport::Yes,
        ..Default::default()
    };
    s.feed(SpeakInput::TalkByte, at(base, 0));
    assert_eq!(
        s.speak_line("ctrl-\\"),
        "recording… let go of ctrl-\\, or press it again, to stop  Esc throws it away"
    );
    let act = s.feed(SpeakInput::Cancel, at(base, 100));
    assert!(act.cancel && !act.stop && act.write == KITTY_POP && !s.recording);
    let mut s = Speaker {
        kitty: KittySupport::Yes,
        ..Default::default()
    };
    s.feed(SpeakInput::TalkByte, at(base, 0));
    assert_eq!(s.close(), KITTY_POP);
    assert_eq!(s.close(), "");
    let mut idle = Speaker::default();
    assert_eq!(idle.close(), "");
    for input in [
        SpeakInput::KittyPress,
        SpeakInput::KittyRelease,
        SpeakInput::KittyRepeat,
        SpeakInput::Cancel,
    ] {
        assert_eq!(idle.feed(input, at(base, 0)), SpeakAct::default());
    }
}

// ---- facts ----

#[test]
fn the_header_row_shows_the_floor_facts() {
    let f = decode_facts(&o(json!({
        "daisugi": {"mode":"watching","armed":true,"enforcing":1.0,"watching":2.0,"off":0.0},
        "working": 9.0, "needing_you": 9.0,
        "tokens_today": {"fresh":1000.0,"cache_read":1200000.0,"cache_write":3000.0,"out":5000.0},
        "gateway": {"url":"http://127.0.0.1:8787","answers":true}
    })));
    let mut m = model(vec![
        Row {
            id: "a".into(),
            state: "blocked".into(),
            line: "x".into(),
            ..Default::default()
        },
        row("b", "working"),
        Row {
            id: "c".into(),
            state: "blocked".into(),
            held: Some(Held {
                task_label: "t".into(),
                ..Default::default()
            }),
            ..Default::default()
        },
        Row {
            id: "c/k".into(),
            parent: "c".into(),
            state: "working".into(),
            ..Default::default()
        },
    ]);
    m.need_you = 1;
    m.facts = f;
    let got = strip_sgr(&render(&mut m, 120, 20)[1]);
    for want in [
        "daisugi · 1 enforcing · 2 watching",
        "gateway answers",
        "2 working",
        "1 needs you",
        "tokens today 1.2M",
    ] {
        assert!(got.contains(want), "{got} lacks {want}");
    }
    assert!(!strip_sgr(&render(&mut m, 99, 20)[1]).contains("daisugi"));
    m.facts = decode_facts(&o(
        json!({"daisugi":{"armed":false,"off":1.0},"gateway":{"url":"http://box:1"}}),
    ));
    let got = strip_sgr(&render(&mut m, 120, 20)[1]);
    assert!(got.contains("daisugi · 1 off · disarmed") && got.contains("gateway ?"));
}

#[test]
fn the_floor_names_the_gate_install_until_a_hook_is_installed() {
    let has = |m: &mut Model, cols: i64| {
        render(m, cols, 20)
            .iter()
            .any(|l| strip_sgr(l).contains(GATE_HINT))
    };
    let mut m = model(vec![]);
    m.facts = decode_facts(&o(json!({"daisugi":{"armed":true,"installed":false}})));
    for cols in [hint_min_cols(), 80, 120] {
        assert!(has(&mut m, cols), "{cols}");
    }
    assert!(!has(&mut m, hint_min_cols() - 1));
    for d in [
        json!({"armed":true,"installed":true}),
        json!({"armed":true,"installed":false,"watching":1.0}),
        json!({"armed":true}),
    ] {
        let mut m = model(vec![]);
        m.facts = decode_facts(&o(json!({ "daisugi": d })));
        assert!(!has(&mut m, 120));
    }
}

// ---- windows ----

#[test]
fn window_count_and_widths_fit_readable_windows() {
    for (cols, want) in [
        (80, 0),
        (118, 0),
        (120, 1),
        (199, 1),
        (200, 2),
        (280, 2),
        (290, 3),
        (400, 4),
    ] {
        assert_eq!(window_count(cols), want, "{cols}");
    }
    for cols in 100..=420 {
        let n = window_count(cols);
        if n == 0 {
            continue;
        }
        let (rail, widths) = split_widths(cols, n);
        let mut sum = rail;
        for w in widths {
            assert!(w >= MIN_WINDOW);
            sum += w + 1;
        }
        assert_eq!(sum, cols);
    }
}

fn tiled_model() -> Model {
    let mut m = model(vec![
        Row {
            id: "a".into(),
            label: "gate-refactor".into(),
            harness: "claude".into(),
            state: "blocked".into(),
            line: "wants git push --force".into(),
            ..Default::default()
        },
        Row {
            id: "b".into(),
            label: "docs".into(),
            harness: "pi".into(),
            state: "working".into(),
            line: "editing README".into(),
            ..Default::default()
        },
    ]);
    m.need_you = 1;
    let mut ts = Tiles::new(Layout::Focus, 1);
    ts.fill("a");
    m.tiles = Some(ts);
    m.cursor = m.index_of("a", 0);
    m
}

fn right_of_divider(lines: &[String]) -> Vec<String> {
    let mut out = Vec::new();
    for l in lines {
        let p = strip_sgr(l).replace(ACCENT_DIVIDER, DIVIDER);
        let parts: Vec<&str> = p.split(DIVIDER).collect();
        if parts.len() > 1 {
            out.extend(parts[1..].iter().map(|s| s.to_string()));
        }
    }
    out
}

#[test]
fn a_wide_screen_splits_and_the_tile_carries_a_header() {
    let mut m = tiled_model();
    let lines = render(&mut m, 140, 30);
    let right = right_of_divider(&lines);
    assert!(right.join("\n").contains("gate-refactor  claude  blocked"));
    assert!(!lines[0].contains('│'));
    assert!(plain(&lines).contains("  › 1 gate-refactor"));
    for l in render(&mut m, 80, 24) {
        assert!(!l.contains('│'));
    }
    m.message = "hello".into();
    m.screens
        .insert("a".into(), crate::attach::Screen::new(3, 1));
    for cols in [120, 140, 200, 201] {
        for l in render(&mut m, cols, 20) {
            assert_eq!(textwidth::width(&strip_sgr(&l)) as i64, cols);
        }
    }
}

#[test]
fn an_empty_slot_says_what_to_do_and_a_gone_pane_leaves_its_slot() {
    let mut m = tiled_model();
    m.tiles = Some(Tiles::new(Layout::Focus, 1));
    assert!(right_of_divider(&render(&mut m, 140, 20))
        .join("\n")
        .contains(EMPTY_SLOT_TEXT));
    let mut m = tiled_model();
    let body = right_of_divider(&render(&mut m, 140, 20))
        .iter()
        .filter(|r| !r.trim().is_empty())
        .count();
    assert_eq!(body, 1);
    m.apply(&list(json!([{"id":"b","state":"working"}])), 0.0);
    assert!(m.tiles.as_ref().unwrap().slot_of("a").is_none());
}

#[test]
fn cut_sgr_keeps_codes_and_cuts_cells() {
    let input = "\x1b[31mabc\x1b[0mdef\x1b[0m";
    let got = cut_sgr(input, 4);
    assert_eq!(strip_sgr(&got), "abc…");
    assert!(got.starts_with("\x1b[31mabc"));
    assert_eq!(textwidth::width(&strip_sgr(&cut_sgr(input, 10))), 10);
    assert!(cut_sgr(input, 10).contains(SGR_RESET));
}

#[test]
fn a_typing_tile_draws_its_header_in_reverse_with_the_leave_key() {
    let typing_header = |lines: &[String]| {
        lines
            .iter()
            .find(|l| strip_sgr(l).contains("typing here · "))
            .cloned()
            .unwrap_or_default()
    };
    let mut m = tiled_model();
    assert!(typing_header(&render(&mut m, 140, 30)).is_empty());
    m.typing = "a".into();
    let head = typing_header(&render(&mut m, 140, 30));
    assert!(head.contains(SGR_REVERSE));
    assert!(strip_sgr(&head).contains("typing here · ctrl-space leaves"));
    m.leave = 0x1d;
    assert!(
        strip_sgr(&typing_header(&render(&mut m, 140, 30))).contains("typing here · ctrl-] leaves")
    );
    m.leave = 0;
    let joined = plain(&render(&mut m, 140, 30));
    assert!(joined.contains(
        "ctrl-space back to the rail, then ctrl-c quits  click another window to type there"
    ));
    assert!(!joined.contains(&join_keys(&table())));
}

#[test]
fn render_records_the_inner_size_of_every_shown_tile() {
    let mut m = tiled_model();
    render(&mut m, 140, 30);
    let (_, widths) = split_widths(140, 1);
    assert_eq!(
        m.tile_sizes.get("a"),
        Some(&Size {
            cols: widths[0],
            rows: 30 - 3 - 1
        })
    );
    render(&mut m, 80, 30);
    assert!(m.tile_sizes.is_empty());
}

fn typing_floor() -> Floor<'static> {
    let mut m = model(vec![row("a", "working"), row("b", "working")]);
    let mut ts = Tiles::new(Layout::Focus, 2);
    ts.fill("a");
    ts.fill("b");
    m.tiles = Some(ts);
    let mut f = test_floor(m, 200);
    render(&mut f.m, 200, 20);
    f
}

#[test]
fn clicks_give_and_take_the_keyboard() {
    let mut f = typing_floor();
    f.handle(Key::Mouse(Click {
        button: 0,
        x: 199,
        y: 5,
        release: false,
    }))
    .unwrap();
    assert_eq!(f.m.typing, "b");
    assert_eq!(tiles(&mut f).focus, 1);
    for (x, y) in [(3, 10), (150, 20), (150, 19)] {
        f.m.typing = "a".into();
        f.click(Click {
            button: 0,
            x,
            y,
            release: false,
        })
        .unwrap();
        assert!(f.m.typing.is_empty(), "{x},{y}");
    }
    let mut f = typing_floor();
    tiles(&mut f).close_slot(1);
    render(&mut f.m, 200, 20);
    f.m.typing = "a".into();
    f.click(Click {
        button: 0,
        x: 199,
        y: 5,
        release: false,
    })
    .unwrap();
    assert!(f.m.typing.is_empty());
}

#[test]
fn whole_runes_keeps_a_split_rune_for_later() {
    let b = "hé".as_bytes();
    let (text, rest) = whole_runes(&b[..2]);
    assert_eq!((text.as_str(), rest.as_slice()), ("h", &b[1..2]));
    let mut more = rest.clone();
    more.push(b[2]);
    let (text, rest) = whole_runes(&more);
    assert!(text == "é" && rest.is_empty());
    let (text, rest) = whole_runes(&[b'a', 0xff, b'b']);
    assert!(text == "ab" && rest.is_empty());
    assert_eq!(whole_runes(b"\r\x03\x00\x1b[A").0, "\r\x03\x00\x1b[A");
}

// ---- the mouse ----

#[test]
fn parse_sgr_reads_reports_and_refuses_malformed_ones() {
    let c = |button, x, y, release| Click {
        button,
        x,
        y,
        release,
    };
    assert_eq!(parse_sgr(b"\x1b[<0;3;4M"), Some(c(0, 3, 4, false)));
    assert_eq!(parse_sgr(b"\x1b[<0;3;4m"), Some(c(0, 3, 4, true)));
    assert_eq!(parse_sgr(b"\x1b[<1;10;2M"), Some(c(1, 10, 2, false)));
    assert_eq!(parse_sgr(b"\x1b[<64;1;1M").map(|c| c.button), Some(64));
    for input in [
        &b""[..],
        b"\x1b[<0;3M",
        b"\x1b[<a;b;cM",
        b"\x1b[<0;3;4",
        b"\x1b[<0;3;4X",
        b"0;3;4M",
        b"\x1b[<0;3;4;5M",
    ] {
        assert_eq!(parse_sgr(input), None, "{input:?}");
    }
}

#[test]
fn read_key_returns_mouse_keys_esc_then_byte_and_drops_long_reports() {
    let (ks, closed) = read_key_from(&mut feed(b"[<0;3;4M"), 0x1b);
    assert!(!closed);
    assert_eq!(
        ks,
        [Key::Mouse(Click {
            button: 0,
            x: 3,
            y: 4,
            release: false
        })]
    );
    let (ks, _) = read_key_from(&mut feed(b"q"), 0x1b);
    assert_eq!(ks, [Key::Esc, Key::Byte(b'q')]);
    let (ks, _) = read_key_from(&mut feed(b"[B"), 0x1b);
    assert_eq!(ks, [Key::Down]);
    let long = format!("[<{}M", "1".repeat(40));
    let (ks, closed) = read_key_from(&mut feed(long.as_bytes()), 0x1b);
    assert!(ks.is_empty() && !closed);
}

#[test]
fn render_fills_row_at_and_tile_at() {
    let mut m = tiled_model();
    m.rows[0].state = "working".into();
    let lines = render(&mut m, 140, 20);
    assert_eq!(m.row_at.len(), lines.len());
    assert_eq!(m.tile_at.len(), 140);
    assert_eq!(m.tile_at[0], -1);
    assert_eq!(m.tile_at[139], 0);
    assert_eq!(hit_tile(&m, 1), None);
    assert_eq!(hit_tile(&m, 140), Some(0));
    assert_eq!(hit_row(&m, 1), None);
    assert_eq!(hit_row(&m, 0), None);
    assert_eq!(hit_row(&m, 21), None);
    render(&mut m, 80, 20);
    for x in 1..=80 {
        assert_eq!(hit_tile(&m, x), None);
    }
}

#[test]
fn a_middle_click_opens_a_slot_and_a_press_in_a_tile_focuses_it() {
    let mut m = model(vec![row("a", "working"), row("b", "working")]);
    let mut ts = Tiles::new(Layout::Focus, 1);
    ts.fill("a");
    m.tiles = Some(ts);
    let mut f = test_floor(m, 140);
    render(&mut f.m, 140, 20);
    let target = f.m.index_of("b", 0);
    let y = f.m.row_at.iter().position(|r| *r == target).unwrap() as i64 + 1;
    f.handle(Key::Mouse(Click {
        button: 1,
        x: 3,
        y,
        release: false,
    }))
    .unwrap();
    let ts = tiles(&mut f);
    assert!(ts.slots.len() == 2 && ts.slots[1] == "b" && ts.focus == 1);
    let mut f = typing_floor();
    tiles(&mut f).focus = 0;
    f.handle(Key::Mouse(Click {
        button: 0,
        x: 199,
        y: 5,
        release: false,
    }))
    .unwrap();
    assert_eq!(tiles(&mut f).focus, 1);
    f.handle(Key::Mouse(Click {
        button: 0,
        x: 100,
        y: 5,
        release: true,
    }))
    .unwrap();
    assert_eq!(tiles(&mut f).focus, 1);
    let cursor = f.m.cursor;
    f.handle(Key::Mouse(Click {
        button: 64,
        x: 3,
        y: 3,
        release: false,
    }))
    .unwrap();
    assert!(f.m.cursor == cursor && tiles(&mut f).focus == 1);
}

#[test]
fn t_and_o_are_text_after_ctrl_t_and_no_tiles_does_not_panic() {
    let mut m = model(vec![row("a", "working"), row("b", "working")]);
    let mut ts = Tiles::new(Layout::Focus, 1);
    ts.fill("a");
    m.tiles = Some(ts);
    m.cursor = 1;
    let mut f = test_floor(m, 140);
    press(&mut f, b"\x14to");
    assert!(f.m.prompt == b"to" && f.m.talking);
    assert_eq!(tiles(&mut f).slots.join(" "), "a");
    let m = model(vec![row("a", "working"), row("b", "working")]);
    let mut f = test_floor(m, 140);
    render(&mut f.m, 140, 20);
    f.handle(byte(b't')).unwrap();
    let mc = |button, x, y| {
        Key::Mouse(Click {
            button,
            x,
            y,
            release: false,
        })
    };
    f.handle(mc(1, 3, 3)).unwrap();
    f.handle(mc(0, 100, 3)).unwrap();
    f.handle(mc(0, 3, 4)).unwrap();
    assert_eq!(f.m.cursor, f.m.index_of("b", 0));
    f.handle(Key::Esc).unwrap();
    assert!(f.handle(byte(b' ')).is_err());
}

// ---- voice ----

#[test]
fn the_footer_names_the_talk_key_when_there_is_room() {
    let mut m = model(vec![]);
    m.ended = vec![Row {
        id: "e".into(),
        state: "ended".into(),
        ..Default::default()
    }];
    m.recent_open = true;
    m.cursor = m.index_of("ended:e", 0);
    let text = footer_lines(&mut m, 120).join(" ");
    assert!(
        text.contains("Enter resume  ctrl-w forget  Esc up  ctrl-\\ speak"),
        "{text}"
    );
    m.talk = 0x07;
    assert!(footer_lines(&mut m, 120).join(" ").contains("ctrl-g speak"));
}

/// A terminal in mode 1000 sends a release after every press. A press on
/// a close mark asks whether to stop the agent, and the release that
/// follows must not answer that question, nor may motion. Only a key or a
/// new press answers it (CP-R-44).
#[test]
fn a_release_or_motion_does_not_answer_the_close_question() {
    let mut f = test_floor(tiled_model(), 140);
    render(&mut f.m, 140, 20);
    let at = Click {
        button: 0,
        x: f.m.tile_x[0] + f.m.tile_w[0],
        y: f.m.header_y + 1,
        release: false,
    };
    f.handle(Key::Mouse(at)).unwrap();
    assert_eq!(f.m.confirm, "a", "a press on the header close mark");
    f.handle(Key::Mouse(Click {
        release: true,
        ..at
    }))
    .unwrap();
    assert_eq!(
        f.m.confirm, "a",
        "the release after the press answered the question"
    );
    f.handle(Key::Mouse(Click {
        button: 32,
        x: 5,
        y: 5,
        release: false,
    }))
    .unwrap();
    assert_eq!(f.m.confirm, "a", "motion answered the question");
    f.handle(Key::Mouse(Click {
        button: 0,
        x: 5,
        y: 5,
        release: false,
    }))
    .unwrap();
    assert_eq!(f.m.confirm, "", "a new press did not answer the question");

    f.m.clear_all = true;
    f.handle(Key::Mouse(Click {
        button: 0,
        x: 5,
        y: 5,
        release: true,
    }))
    .unwrap();
    assert!(f.m.clear_all, "a release answered the clear-all question");
    f.handle(Key::Esc).unwrap();
    assert!(!f.m.clear_all, "Esc did not answer the clear-all question");
}
