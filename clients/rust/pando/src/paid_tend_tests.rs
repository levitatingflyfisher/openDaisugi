//! tend asks before paid calls, `--max` is one budget, `import --dir`: the
//! Rust twin of Go's paid_tend_test.go.

use std::cell::{Cell, RefCell};
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering;
use std::sync::Arc;

use super::chat::*;
use super::chat_tests::{open_t, temp_dir, Fake, TestClock};
use super::child::child_dir;
use super::cli::{run, CliEnv};
use super::ctx::Ctx;
use super::store::store_dir;

struct Shared(Arc<Fake>);

impl Summarizer for Shared {
    fn compress(&self, c: &Ctx, m: &Message, limit: usize) -> Result<String, String> {
        self.0.compress(c, m, limit)
    }
    fn merge(&self, c: &Ctx, id: super::order::NodeId, l: &str, r: &str, limit: usize) -> Result<String, String> {
        self.0.merge(c, id, l, r, limit)
    }
}

fn words(_: &str) -> Vec<f64> {
    vec![0.0; 26]
}

fn long() -> String {
    "word ".repeat(300)
}

fn seed(dir: &Path, n: usize) {
    let mut c = open_t(dir, &TestClock::new(), |_| {});
    for _ in 0..n {
        let (_, err) = c.append(KIND_USER, &long());
        assert!(err.is_none());
    }
    c.close();
}

/// Runs one command with a summarizer that counts the ones asked for.
fn go(data: &Path, home: &Path, argv: &[&str], fake: &Arc<Fake>, asked: &Cell<usize>, profile: &RefCell<String>) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(Vec::new());
    let mk = |p: &str| -> Result<Box<dyn Summarizer>, String> {
        asked.set(asked.get() + 1);
        *profile.borrow_mut() = p.to_string();
        Ok(Box::new(Shared(fake.clone())))
    };
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: home.to_path_buf(),
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

fn calls(f: &Fake) -> usize {
    f.compress.load(Ordering::SeqCst) + f.merge.load(Ordering::SeqCst)
}

#[test]
fn tend_prints_the_node_count_first() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 3);
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let (code, out, _) = go(&d, &d.join("nohome"), &["tend", "--dry-run"], &fake, &asked, &profile);
    assert!(code == 0 && out.starts_with("nodes to build: "), "{out}");
    let (code, out, err) = go(&d, &d.join("nohome"), &["tend"], &fake, &asked, &profile);
    assert!(code == 0 && out.starts_with("nodes to build: "), "{out} {err}");
}

#[test]
fn tend_refuses_the_default_model_over_the_limit() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 30);
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let home = d.join("nohome");
    let (code, out, err) = go(&d, &home, &["tend"], &fake, &asked, &profile);
    assert_eq!((code, asked.get()), (1, 0), "{out} {err}");
    assert!(out.starts_with("nodes to build: "), "{out}");
    assert!(err.contains("Claude Code (paid)") && err.contains("--profile NAME") && err.contains("--yes"), "{err}");
    let c = Chat::open_read_only(&store_dir(&d, "work").unwrap()).unwrap();
    assert!(c.pending() > 0);
    // A named profile goes on, and so does --yes.
    let (code, _, _) = go(&d, &home, &["tend", "--profile", "desk", "--max", "3"], &fake, &asked, &profile);
    assert_eq!((code, profile.borrow().as_str()), (0, "desk"));
    let (code, out, err) = go(&d, &home, &["tend", "--yes"], &fake, &asked, &profile);
    assert!(code == 0 && out.contains("pending: 0\n"), "{out} {err}");
}

#[test]
fn max_keeps_the_paid_calls_under_the_limit_and_is_its_own_guard() {
    let d = temp_dir();
    seed(&store_dir(&d, "work").unwrap(), 30);
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let (code, _, err) = go(&d, &d.join("nohome"), &["tend", "--max", "5"], &fake, &asked, &profile);
    assert_eq!((code, calls(&fake)), (0, 5), "{err}");
}

#[test]
fn max_is_one_budget_across_the_store_and_its_children() {
    let d = temp_dir();
    let dir = store_dir(&d, "work").unwrap();
    seed(&dir, 2);
    for label in ["a1", "b2"] {
        seed(&child_dir(&dir, label).unwrap(), 3);
    }
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let (code, _, err) = go(&d, &d.join("nohome"), &["tend", "--max", "4"], &fake, &asked, &profile);
    assert_eq!((code, calls(&fake)), (0, 4), "{err}");
}

fn write_lines(path: &Path, lines: &[String]) {
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, lines.join("\n") + "\n").unwrap();
}

fn claude_line(uuid: &str, ts: &str, text: &str) -> String {
    format!(
        r#"{{"type":"user","uuid":"{uuid}","sessionId":"s1","timestamp":"{ts}","cwd":"/work/proj","message":{{"role":"user","content":"{text}"}}}}"#
    )
}

#[test]
fn import_tend_refuses_the_default_model_over_the_limit_and_keeps_the_messages() {
    let home = temp_dir();
    let lines: Vec<String> = (0..40).map(|i| claude_line(&format!("u{i}"), "2026-03-01T10:00:00.000Z", &long())).collect();
    write_lines(&home.join(".claude/projects/-p/s1.jsonl"), &lines);
    let data = temp_dir();
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let (code, out, err) = go(&data, &home, &["import", "--from", "claude", "--tend"], &fake, &asked, &profile);
    assert_eq!(code, 1, "{out} {err}");
    assert!(err.contains("Claude Code (paid)") && out.contains("logged: 40 messages"), "{out} {err}");
}

#[test]
fn import_dir_reads_a_folder_in_place_of_the_claude_home() {
    let home = temp_dir();
    write_lines(&home.join(".claude/projects/-work-proj/s1.jsonl"), &[claude_line("h1", "2026-03-01T10:00:00.000Z", "fix the bug")]);
    let other = temp_dir();
    write_lines(&other.join("-old-proj/z1.jsonl"), &[claude_line("x1", "2026-02-01T10:00:00.000Z", "from the folder")]);
    write_lines(
        &other.join("flat.jsonl"),
        &[r#"{"type":"user","uuid":"y1","sessionId":"s2","timestamp":"2026-02-02T10:00:00.000Z","cwd":"/work/proj","message":{"role":"user","content":"a flat file"}}"#.to_string()],
    );
    let data = temp_dir();
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let dir = other.to_string_lossy().into_owned();
    let (code, out, err) = go(&data, &home, &["import", "--dir", &dir, "--dry-run"], &fake, &asked, &profile);
    assert!(code == 0 && out.starts_with("past chats: 2 (about 2 messages)\n  claude: 2 chats, 2 messages\n"), "{out} {err}");
    let (code, _, err) = go(&data, &home, &["import", "--dir", &dir], &fake, &asked, &profile);
    assert_eq!(code, 0, "{err}");
    let (_, out, _) = go(&data, &home, &["log"], &fake, &asked, &profile);
    assert!(out.contains("from the folder") && out.contains("a flat file") && !out.contains("fix the bug"), "{out}");
}

#[test]
fn import_dir_refusals() {
    let data = temp_dir();
    let (fake, asked, profile) = (Arc::new(Fake::default()), Cell::new(0), RefCell::new(String::new()));
    let none: PathBuf = data.join("none");
    let (code, _, err) = go(&data, &data, &["import", "--dir", &none.to_string_lossy()], &fake, &asked, &profile);
    assert!(code == 1 && err.contains("no folder "), "{err}");
    let (code, _, err) = go(&data, &data, &["import", "--dir", &data.to_string_lossy(), "--from", "codex"], &fake, &asked, &profile);
    assert!(code == 2 && err.contains("--dir holds Claude Code chats"), "{err}");
}
