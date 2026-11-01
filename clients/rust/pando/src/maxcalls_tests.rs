//! `--max` caps every model call, tend prints a line for each node built,
//! and status counts clipped nodes: the Rust twin of Go's maxcalls_test.go.

use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use super::chat::*;
use super::chat_tests::{open_t, temp_dir, TestClock};
use super::cli::{run, CliEnv};
use super::compactor::{CallUsage, ModelSummarizer};
use super::store::store_dir;
use super::tend::ProgressSink;

fn words(_: &str) -> Vec<f64> {
    vec![0.0; 26]
}

fn seed(dir: &Path, n: usize) {
    let mut c = open_t(dir, &TestClock::new(), |_| {});
    for _ in 0..n {
        let (_, err) = c.append(KIND_USER, &"word ".repeat(300));
        assert!(err.is_none());
    }
    c.close();
}

/// Runs a command with a model that answers `reply` every time; returns the
/// model calls, stdout, stderr and the progress lines.
fn go(data: &Path, argv: &[&str], reply: &'static str) -> (usize, i32, String, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(Vec::new());
    let calls = Arc::new(AtomicUsize::new(0));
    let c2 = calls.clone();
    let mk = move |_: &str| -> Result<Box<dyn Summarizer>, String> {
        let c3 = c2.clone();
        Ok(Box::new(ModelSummarizer::new(Box::new(move |_, _, _| {
            c3.fetch_add(1, Ordering::SeqCst);
            Ok((reply.repeat(if reply == "x" { 4000 } else { 1 }), CallUsage::default()))
        }))))
    };
    let buf = Arc::new(Mutex::new(Vec::<u8>::new()));
    let sink: ProgressSink = buf.clone();
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: data.join("nohome"),
            new_summarizer: Some(&mk),
            progress: Some(sink),
            now: None,
            embed: words,
            hermes_source: None,
            getenv: None,
        },
    );
    let progress = String::from_utf8(buf.lock().unwrap().clone()).unwrap();
    (calls.load(Ordering::SeqCst), code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap(), progress)
}

#[test]
fn max_caps_every_model_call() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 12);
    let (calls, code, out, err, progress) = go(&d, &["tend", "--yes", "--max", "7"], "x");
    assert_eq!((calls, code), (7, 0), "{out} {err}");
    let n = progress.matches("built level ").count();
    assert!((1..=7).contains(&n), "{progress}");
}

#[test]
fn each_node_prints_one_progress_line_and_quiet_hides_them() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 3);
    let (_, code, _, _, progress) = go(&d, &["tend", "--yes", "--max", "100"], "x");
    assert_eq!(code, 0);
    let lines: Vec<&str> = progress.lines().filter(|l| l.starts_with("built level ")).collect();
    assert!(lines.len() >= 3, "{progress}");
    for l in lines {
        assert!(l.contains(" index ") && l.contains("s (") && l.ends_with("model calls so far)"), "{l}");
    }
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 3);
    let (_, _, _, _, progress) = go(&d, &["tend", "--yes", "-q"], "x");
    assert!(progress.is_empty(), "{progress}");
}

#[test]
fn max_caps_every_model_call_of_a_redo() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 8);
    go(&d, &["tend", "--yes", "-q"], "x");
    let (calls, code, out, err, _) = go(&d, &["tend", "--redo-level", "0", "--max", "9", "--yes"], "x");
    assert_eq!((calls, code), (9, 0), "{out} {err}");
}

#[test]
fn status_counts_clipped_nodes() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 1);
    let (_, code, ..) = go(&d, &["tend", "--yes", "-q"], "");
    assert_eq!(code, 0);
    let (_, _, out, ..) = go(&d, &["status"], "");
    assert!(out.contains("clipped nodes: 1\n"), "{out}");
    let (_, _, out, ..) = go(&d, &["status", "--json"], "");
    assert!(out.contains("\"clipped\":1"), "{out}");
}
