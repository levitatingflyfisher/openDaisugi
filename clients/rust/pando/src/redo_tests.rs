//! tend counts and explains clipped nodes, stops when the first five clip,
//! `--redo-clipped`: the Rust twin of Go's redo_test.go.

use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;

use super::chat::*;
use super::chat_tests::{open_t, temp_dir, TestClock};
use super::cli::{run, CliEnv, CLIPPED_WHY};
use super::compactor::{CallUsage, ModelSummarizer};
use super::ctx::Ctx;
use super::order::parse_name;
use super::store::store_dir;
use super::tend::CLIP_STOP_AFTER;

/// A fake model that can be broken (no text, as a thinking model out of
/// tokens gives) and fixed.
#[derive(Default)]
struct Gap {
    broken: AtomicBool,
    /// A merge still gives text while the model is broken.
    merge_ok: AtomicBool,
    calls: AtomicUsize,
    n: AtomicUsize,
}

fn model(g: &Arc<Gap>) -> Box<dyn Summarizer> {
    let g = g.clone();
    Box::new(ModelSummarizer::new(Box::new(move |_: &Ctx, _s: &str, prompt: &str| {
        g.calls.fetch_add(1, Ordering::SeqCst);
        let n = g.n.fetch_add(1, Ordering::SeqCst) + 1;
        let merge = prompt.contains("<left>");
        if g.broken.load(Ordering::SeqCst) && !(merge && g.merge_ok.load(Ordering::SeqCst)) {
            return Ok((
                String::new(),
                CallUsage {
                    stop: "length",
                    ..Default::default()
                },
            ));
        }
        Ok((format!("line {}", (b'a' + (n % 26) as u8) as char), CallUsage::default()))
    })))
}

fn words(_: &str) -> Vec<f64> {
    vec![0.0; 26]
}

fn seed(dir: &Path, n: usize) {
    let mut c = open_t(dir, &TestClock::new(), |_| {});
    for i in 0..n {
        let word = if i % 2 == 0 { "alpha " } else { "beta " };
        let (_, err) = c.append(KIND_USER, &word.repeat(200));
        assert!(err.is_none());
    }
    c.close();
}

fn go(data: &Path, g: &Arc<Gap>, argv: &[&str]) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(Vec::new());
    let mk = |_: &str| -> Result<Box<dyn Summarizer>, String> { Ok(model(g)) };
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: data.join("nohome"),
            new_summarizer: Some(&mk),
            progress: None,
            now: None,
            embed: words,
            hermes_source: None,
            getenv: None,
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

fn text(data: &Path, name: &str) -> String {
    let c = Chat::open_read_only(&store_dir(data, "work").unwrap()).unwrap();
    c.node(parse_name(name).unwrap()).expect("a node").text.clone()
}

fn two_long() -> std::path::PathBuf {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 2);
    d
}

fn broken(merge_ok: bool) -> Arc<Gap> {
    let g = Arc::new(Gap::default());
    g.broken.store(true, Ordering::SeqCst);
    g.merge_ok.store(merge_ok, Ordering::SeqCst);
    g
}

#[test]
fn tend_counts_and_explains_clipped_nodes() {
    let d = two_long();
    let g = broken(false);
    let (code, out, err) = go(&d, &g, &["tend", "--yes"]);
    assert!(code == 0 && out.contains("built: 3, fell back to clipped: 3\npending: 0\n"), "{code} {out} {err}");
    assert!(out.contains(CLIPPED_WHY) && out.contains("--redo-clipped"), "{out}");
    assert!(text(&d, "0+1").starts_with("[clipped] "));
    // A run with no fallback says nothing more.
    let d2 = two_long();
    let (code, out, _) = go(&d2, &Arc::new(Gap::default()), &["tend", "--yes"]);
    assert!(code == 0 && out.contains("fell back to clipped: 0\n") && !out.contains(CLIPPED_WHY), "{out}");
}

#[test]
fn tend_stops_when_the_first_five_calls_all_clip() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 8);
    let g = broken(false);
    let (code, _, err) = go(&d, &g, &["tend", "--yes"]);
    assert_eq!((code, g.calls.load(Ordering::SeqCst)), (1, CLIP_STOP_AFTER), "{err}");
    assert!(
        err.contains("the model returned no text for the first 5 nodes") && err.contains("max_tokens") && err.contains("--redo-clipped"),
        "{err}"
    );
    // A run that gets one line in the first five goes on.
    let d2 = temp_dir();
    seed(&store_dir(&d2, "work").unwrap(), 8);
    let (code, _, err) = go(&d2, &broken(true), &["tend", "--yes"]);
    assert_eq!(code, 0, "{err}");
}

#[test]
fn redo_clipped_builds_the_nodes_again_and_the_ones_above() {
    let d = two_long();
    // The leaves clip, the merge above them gives text: the parent is not a
    // clipped node, but it is made from clipped lines.
    let g = broken(true);
    assert_eq!(go(&d, &g, &["tend", "--yes"]).0, 0);
    let before = text(&d, "0+2");
    assert!(!before.starts_with("[clipped] "), "{before}");
    let (code, out, _) = go(&d, &g, &["tend", "--redo-clipped", "--dry-run"]);
    assert!(code == 0 && out.starts_with("nodes to build again: 3\nmodel calls: at most 3\n0+1\n1+1\n0+2\n") && out.contains("nothing was changed"), "{out}");
    assert!(text(&d, "0+1").starts_with("[clipped] "), "a dry run changed a node");
    // Fix the model. The two leaves and the parent above them are built
    // again. The parent joins two short lines, so it takes no model call.
    g.broken.store(false, Ordering::SeqCst);
    g.calls.store(0, Ordering::SeqCst);
    let (code, out, err) = go(&d, &g, &["tend", "--redo-clipped", "--yes"]);
    assert!(code == 0 && out.contains("built again: 3, fell back to clipped: 0\n") && g.calls.load(Ordering::SeqCst) == 2, "{code} {out} {err}");
    for n in ["0+1", "1+1", "0+2"] {
        assert!(!text(&d, n).starts_with("[clipped] "), "{n} is still clipped");
    }
    assert_ne!(text(&d, "0+2"), before, "the parent was not made again");
    // Nothing is left.
    g.calls.store(0, Ordering::SeqCst);
    let (code, out, _) = go(&d, &g, &["tend", "--redo-clipped", "--yes"]);
    assert!(code == 0 && out.starts_with("nodes to build again: 0\n") && g.calls.load(Ordering::SeqCst) == 0, "{out}");
    // The view shows no clipped line.
    let (code, out, _) = go(&d, &g, &["view"]);
    assert!(code == 0 && !out.contains("[clipped] "), "{out}");
}

#[test]
fn redo_clipped_keeps_one_budget_and_goes_oldest_first() {
    let d = two_long();
    let g = broken(false);
    assert_eq!(go(&d, &g, &["tend", "--yes"]).0, 0);
    g.broken.store(false, Ordering::SeqCst);
    g.calls.store(0, Ordering::SeqCst);
    let (code, out, _) = go(&d, &g, &["tend", "--redo-clipped", "--max", "1"]);
    assert!(code == 0 && g.calls.load(Ordering::SeqCst) == 1 && out.contains("built again: 1,"), "{out}");
    assert!(!text(&d, "0+1").starts_with("[clipped] ") && text(&d, "1+1").starts_with("[clipped] "));
    g.calls.store(0, Ordering::SeqCst);
    let (code, out, _) = go(&d, &g, &["tend", "--redo-clipped"]);
    assert!(code == 0 && g.calls.load(Ordering::SeqCst) == 1 && out.contains("built again: 2, fell back to clipped: 0\n"), "{out}");
}

fn clip_all(d: &Path, n: usize) {
    let dir = store_dir(d, "work").unwrap();
    seed(&dir, n);
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    let g = broken(false);
    c.drain(&Ctx::background(), model(&g).as_ref()).unwrap();
    c.close();
}

#[test]
fn redo_clipped_stops_when_the_model_is_still_broken() {
    let d = temp_dir();
    clip_all(&d, 8);
    let g = broken(false);
    let (code, _, err) = go(&d, &g, &["tend", "--redo-clipped", "--yes"]);
    assert_eq!((code, g.calls.load(Ordering::SeqCst)), (1, CLIP_STOP_AFTER), "{err}");
    assert!(err.contains("no text for the first 5 nodes"), "{err}");
    assert!(text(&d, "0+1").starts_with("[clipped] "), "a node changed");
}

#[test]
fn redo_clipped_refuses_a_paid_run_over_the_limit() {
    let d = temp_dir();
    clip_all(&d, 30);
    let g = Arc::new(Gap::default());
    let (code, out, err) = go(&d, &g, &["tend", "--redo-clipped"]);
    assert!(code == 1 && g.calls.load(Ordering::SeqCst) == 0 && err.contains("Claude Code (paid)") && out.starts_with("nodes to build again: "), "{out} {err}");
    let (code, _, err) = go(&d, &g, &["tend", "--redo-clipped", "--max", "3"]);
    assert_eq!((code, g.calls.load(Ordering::SeqCst)), (0, 3), "{err}");
    let (code, _, err) = go(&d, &g, &["tend", "--redo-clipped", "--profile", "desk", "--max", "40"]);
    assert_eq!(code, 0, "{err}");
}

#[test]
fn redo_clipped_goes_with_tend_only() {
    let d = temp_dir();
    let (code, _, err) = go(&d, &Arc::new(Gap::default()), &["view", "--redo-clipped"]);
    assert!(code == 2 && err.contains("--redo-clipped goes with tend"), "{err}");
}

#[test]
fn a_reload_takes_the_last_line_of_a_node() {
    let d = two_long();
    let g = broken(false);
    go(&d, &g, &["tend", "--yes"]);
    g.broken.store(false, Ordering::SeqCst);
    go(&d, &g, &["tend", "--redo-clipped", "--yes"]);
    let dir = store_dir(&d, "work").unwrap();
    let mut lines = 0;
    for e in std::fs::read_dir(dir.join("tree")).unwrap().flatten() {
        lines += std::fs::read_to_string(e.path()).unwrap().matches("\"l\":0,\"i\":0,").count();
    }
    assert_eq!(lines, 2, "the old and the new line of node 0+1");
    let c = Chat::open(Config {
        dir,
        ..Default::default()
    })
    .unwrap();
    assert!(c.notice().is_empty(), "a rewritten node is not a bad line: {}", c.notice());
    assert!(!c.node(parse_name("0+1").unwrap()).unwrap().text.starts_with("[clipped] "));
}

// -- clip markers, --redo-level and the redo plan ------------------------

use std::sync::Mutex;

use super::compactor::unclip;
use super::order::NodeId;

/// A fake model that writes a long line for a message and a short one for a
/// merge, and counts both.
#[derive(Default)]
struct Layers {
    n: AtomicUsize,
    leaves: AtomicUsize,
    merges: AtomicUsize,
    /// The call that fails, counted from the next one; 0 is none.
    fail_at: AtomicUsize,
}

fn layer_model(l: &Arc<Layers>) -> Box<dyn Summarizer> {
    let l = l.clone();
    Box::new(ModelSummarizer::new(Box::new(move |_: &Ctx, _s: &str, prompt: &str| {
        let f = l.fail_at.load(Ordering::SeqCst);
        if f > 0 {
            l.fail_at.store(f - 1, Ordering::SeqCst);
            if f == 1 {
                return Err("the model is gone".into());
            }
        }
        let n = l.n.fetch_add(1, Ordering::SeqCst) + 1;
        if prompt.contains("<left>") {
            l.merges.fetch_add(1, Ordering::SeqCst);
            return Ok((format!("merge {n}"), CallUsage::default()));
        }
        l.leaves.fetch_add(1, Ordering::SeqCst);
        Ok((format!("leaf {n} {}", "x".repeat(300)), CallUsage::default()))
    })))
}

fn run_with(data: &Path, l: &Arc<Layers>, argv: &[&str]) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(Vec::new());
    let mk = |_: &str| -> Result<Box<dyn Summarizer>, String> { Ok(layer_model(l)) };
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: data.join("nohome"),
            new_summarizer: Some(&mk),
            progress: None,
            now: None,
            embed: words,
            hermes_source: None,
            getenv: None,
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

fn four_long() -> std::path::PathBuf {
    let d = temp_dir();
    let mut c = open_t(&store_dir(&d, "work").unwrap(), &TestClock::new(), |_| {});
    for _ in 0..4 {
        let (_, err) = c.append(KIND_USER, &"word ".repeat(130));
        assert!(err.is_none());
    }
    c.close();
    d
}

#[test]
fn unclip_drops_every_leading_marker() {
    assert_eq!(unclip("plain"), "plain");
    assert_eq!(unclip("[clipped] a"), "a");
    assert_eq!(unclip("[clipped] [clipped] [clipped] a"), "a");
    assert_eq!(unclip("a [clipped] b"), "a [clipped] b");
    assert_eq!(unclip(""), "");
}

#[test]
fn a_joined_parent_carries_one_marker_when_a_part_is_clipped() {
    use super::chat::join_lines;
    assert_eq!(join_lines("a", "b"), "a\nb");
    assert_eq!(join_lines("[clipped] a", "b"), "[clipped] a\nb");
    assert_eq!(join_lines("a", "[clipped] b"), "[clipped] a\nb");
    assert_eq!(join_lines("[clipped] [clipped] a", "[clipped] b"), "[clipped] a\nb");
}

#[test]
fn a_model_merge_never_sees_a_clip_marker_and_a_fallback_keeps_one() {
    let seen = Arc::new(Mutex::new(String::new()));
    let s2 = seen.clone();
    let m = ModelSummarizer::new(Box::new(move |_: &Ctx, _s: &str, prompt: &str| {
        *s2.lock().unwrap() = prompt.to_string();
        Ok((String::new(), CallUsage { stop: "length", ..Default::default() }))
    }));
    let left = format!("[clipped] [clipped] {}", "alpha ".repeat(80));
    let right = format!("[clipped] {}", "beta ".repeat(80));
    let got = m.merge(&Ctx::default(), NodeId { l: 1, i: 0 }, &left, &right, 512).unwrap();
    assert!(!seen.lock().unwrap().contains("[clipped]"), "the model saw a marker");
    assert!(got.starts_with("[clipped] alpha ") && got.matches("[clipped] ").count() == 1, "{got}");
}

#[test]
fn a_tended_parent_of_clipped_leaves_holds_one_marker() {
    let d = four_long();
    // The chat's own drain has no stop rule: every node clips.
    let dir = store_dir(&d, "work").unwrap();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    c.drain(&Ctx::background(), model(&broken(false)).as_ref()).unwrap();
    c.close();
    for n in ["0+2", "2+2", "0+4"] {
        let t = text(&d, n);
        assert!(t.starts_with("[clipped] ") && t.matches("[clipped] ").count() == 1, "{n}: {t}");
    }
}

#[test]
fn redo_level_builds_every_node_from_that_level_up() {
    let d = four_long();
    let l = Arc::new(Layers::default());
    let (code, _, err) = run_with(&d, &l, &["tend", "--yes"]);
    assert!(code == 0 && l.leaves.load(Ordering::SeqCst) == 4 && l.merges.load(Ordering::SeqCst) == 2, "{err}");
    let (old_leaf, old_merge, old_root) = (text(&d, "0+1"), text(&d, "0+2"), text(&d, "0+4"));
    let (code, out, _) = run_with(&d, &l, &["tend", "--redo-level", "1", "--dry-run"]);
    assert!(
        code == 0 && out.starts_with("nodes to build again: 3\nmodel calls: at most 3\n0+2\n2+2\n0+4\n") && out.contains("nothing was changed"),
        "{out}"
    );
    assert!(l.leaves.load(Ordering::SeqCst) == 4 && l.merges.load(Ordering::SeqCst) == 2, "a dry run called the model");
    let (code, out, err) = run_with(&d, &l, &["tend", "--redo-level", "1", "--yes"]);
    assert!(
        code == 0 && out.contains("built again: 3, fell back to clipped: 0\n") && l.leaves.load(Ordering::SeqCst) == 4 && l.merges.load(Ordering::SeqCst) == 4,
        "{code} {out} {err}"
    );
    assert_eq!(text(&d, "0+1"), old_leaf, "a leaf below the level changed");
    assert!(text(&d, "0+2") != old_merge && text(&d, "0+4") != old_root);
    l.leaves.store(0, Ordering::SeqCst);
    l.merges.store(0, Ordering::SeqCst);
    let (code, out, err) = run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    assert!(
        code == 0 && out.contains("built again: 7, ") && l.leaves.load(Ordering::SeqCst) == 4 && l.merges.load(Ordering::SeqCst) == 2,
        "{code} {out} {err}"
    );
}

#[test]
fn redo_level_takes_max_and_the_union() {
    let d = four_long();
    let l = Arc::new(Layers::default());
    run_with(&d, &l, &["tend", "--yes"]);
    l.leaves.store(0, Ordering::SeqCst);
    l.merges.store(0, Ordering::SeqCst);
    let (code, out, err) = run_with(&d, &l, &["tend", "--redo-level", "0", "--max", "2", "--yes"]);
    let made = l.leaves.load(Ordering::SeqCst) + l.merges.load(Ordering::SeqCst);
    assert!(code == 0 && made == 2 && out.contains("built again: 2, "), "{code} {out} {err} ({made})");
    let d2 = four_long();
    go(&d2, &broken(true), &["tend", "--yes"]);
    let (_, out, _) = run_with(&d2, &l, &["tend", "--redo-level", "1", "--dry-run"]);
    assert!(out.starts_with("nodes to build again: 3\n"), "{out}");
    let (_, out, _) = run_with(&d2, &l, &["tend", "--redo-level", "1", "--redo-clipped", "--dry-run"]);
    assert!(out.starts_with("nodes to build again: 7\n") && out.matches("0+4\n").count() == 1, "{out}");
}

#[test]
fn redo_level_goes_with_tend_and_takes_a_number() {
    let d = temp_dir();
    let g = Arc::new(Gap::default());
    let (code, _, err) = go(&d, &g, &["view", "--redo-level", "1"]);
    assert!(code == 2 && err.contains("--redo-level goes with tend"), "{err}");
    let (code, _, err) = go(&d, &g, &["tend", "--redo-level", "x"]);
    assert!(code == 2 && err.contains("--redo-level needs a number"), "{err}");
}

#[test]
fn redo_refusal_counts_calls_not_nodes() {
    let d = temp_dir();
    let dir = store_dir(&d, "work").unwrap();
    let mut c = open_t(&dir, &TestClock::new(), |_| {});
    for _ in 0..30 {
        assert!(c.append(KIND_USER, "w").1.is_none());
    }
    c.close();
    let g = Arc::new(Gap::default());
    let (code, _, err) = go(&d, &g, &["tend", "--yes"]);
    assert!(code == 0 && g.calls.load(Ordering::SeqCst) == 0, "{err}");
    let (code, out, err) = go(&d, &g, &["tend", "--redo-level", "1"]);
    assert!(code == 0 && out.starts_with("nodes to build again: 26\nmodel calls: at most 11\n"), "{code} {out} {err}");
    // Level 0 of long messages is one call each: refused over 25.
    let long = temp_dir();
    let ldir = store_dir(&long, "work").unwrap();
    let mut c = open_t(&ldir, &TestClock::new(), |_| {});
    for _ in 0..30 {
        assert!(c.append(KIND_USER, &"word ".repeat(300)).1.is_none());
    }
    c.close();
    let (code, _, err) = go(&long, &g, &["tend", "--redo-level", "0"]);
    assert_eq!(code, 0, "an empty plan is not refused: {err}");
    go(&long, &Arc::new(Gap::default()), &["tend", "--yes"]);
    let (code, _, err) = go(&long, &g, &["tend", "--redo-level", "0"]);
    assert!(code == 1 && err.contains("Claude Code (paid)"), "{err}");
}

/// A redo that stopped on a failed call goes on where it stopped: a node the
/// earlier run built is not called again.
#[test]
fn redo_level_resumes_after_a_failed_call() {
    let d = four_long();
    let l = Arc::new(Layers::default());
    run_with(&d, &l, &["tend", "--yes"]);
    l.leaves.store(0, Ordering::SeqCst);
    l.merges.store(0, Ordering::SeqCst);
    l.fail_at.store(4, Ordering::SeqCst);
    let (code, _, err) = run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    assert!(
        code == 1 && l.leaves.load(Ordering::SeqCst) == 3 && l.merges.load(Ordering::SeqCst) == 0 && err.contains("the model is gone"),
        "{code} {err}"
    );
    let marker = store_dir(&d, "work").unwrap().join("tree").join(REDO_FILE);
    assert!(marker.exists(), "an unfinished redo leaves its marker");
    let (code, out, _) = run_with(&d, &l, &["tend", "--redo-level", "0", "--dry-run"]);
    assert!(code == 0 && out.starts_with("nodes to build again: 4\n"), "the plan skips the built leaves: {out}");
    let (code, out, err) = run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    assert!(
        code == 0 && l.leaves.load(Ordering::SeqCst) == 4 && l.merges.load(Ordering::SeqCst) == 2 && out.contains("built again: 4, "),
        "{code} {out} {err}"
    );
    assert!(!marker.exists(), "a finished redo removes its marker");
    l.leaves.store(0, Ordering::SeqCst);
    l.merges.store(0, Ordering::SeqCst);
    let (code, out, _) = run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    assert!(code == 0 && out.contains("built again: 7, ") && l.leaves.load(Ordering::SeqCst) == 4 && l.merges.load(Ordering::SeqCst) == 2, "{out}");
}

/// Another set or another profile does not resume: it is its own redo.
#[test]
fn redo_with_another_profile_starts_again() {
    let d = four_long();
    let l = Arc::new(Layers::default());
    run_with(&d, &l, &["tend", "--yes"]);
    l.fail_at.store(3, Ordering::SeqCst);
    run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    let (code, out, _) = run_with(&d, &l, &["tend", "--redo-level", "0", "--profile", "other", "--yes"]);
    assert!(code == 0 && out.contains("built again: 7, "), "{out}");
}

/// A redo for another set does not delete the marker of an unfinished one.
#[test]
fn a_redo_for_another_set_keeps_the_marker() {
    let d = four_long();
    let l = Arc::new(Layers::default());
    run_with(&d, &l, &["tend", "--yes"]);
    l.fail_at.store(1, Ordering::SeqCst);
    run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    let marker = store_dir(&d, "work").unwrap().join("tree").join(REDO_FILE);
    assert!(marker.exists());
    let (code, _, err) = run_with(&d, &l, &["tend", "--redo-clipped", "--yes"]);
    assert!(code == 0, "{err}");
    assert!(marker.exists(), "the clipped run took the level run's marker");
    let (_, out, _) = run_with(&d, &l, &["tend", "--redo-level", "0", "--dry-run"]);
    assert!(out.starts_with("nodes to build again: 7\n"), "{out}");
}

/// A child store that was locked and skipped keeps its marker.
#[test]
fn a_locked_child_keeps_its_marker() {
    let d = four_long();
    let dir = store_dir(&d, "work").unwrap();
    let cd = super::child::child_dir(&dir, "a1").unwrap();
    let mut c = open_t(&cd, &TestClock::new(), |_| {});
    let (_, err) = c.append(KIND_USER, &"word ".repeat(130));
    assert!(err.is_none());
    let l = Arc::new(Layers::default());
    run_with(&d, &l, &["tend", "--yes"]);
    let marker = cd.join("tree").join(REDO_FILE);
    std::fs::write(&marker, "{\"after\":0,\"model\":\"default\",\"set\":\"level:0\"}\n").unwrap();
    let (code, _, err) = run_with(&d, &l, &["tend", "--redo-level", "0", "--yes"]);
    assert!(code == 0, "{err}");
    assert!(marker.exists(), "a skipped child lost its marker");
    c.close();
}
