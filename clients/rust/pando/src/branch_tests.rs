//! Branches: the Rust twin of Go's branch_test.go.

use std::path::{Path, PathBuf};

use super::branch::*;
use super::chat::*;
use super::chat_tests::temp_dir;
use super::cli::{run, CliEnv};
use super::live::{follow_once, ClaudeSource, FollowOpts, SKIP_VAR};

fn put(store: &Path, path: &str, text: &str) {
    let d = if path.is_empty() { store.to_path_buf() } else { ensure_branch(store, path, &None).unwrap() };
    let mut c = Chat::open(Config { dir: d, ..Default::default() }).unwrap();
    assert!(c.append(KIND_USER, text).1.is_none());
    c.close();
}

/// work, trellis, trellis/ui and other, one message each.
fn fixture() -> (PathBuf, PathBuf) {
    let data = temp_dir();
    let store = data.join("pando").join("work");
    put(&store, "", "the household budget sheet lives in the attic");
    put(&store, "trellis", "trellis reader uses the epub parser");
    put(&store, "trellis/ui", "the ui pane uses a lattice icon");
    put(&store, "other", "the other project keeps a secret ledger");
    (data, store)
}

fn cli(data: &Path, vars: &[(&str, &str)], argv: &[&str], stdin: &str) -> (i32, String, String) {
    let argv: Vec<String> = argv.iter().map(|s| s.to_string()).chain(["--data-dir".to_string(), data.to_string_lossy().into_owned()]).collect();
    let (mut out, mut err) = (Vec::new(), Vec::new());
    let mut inp = std::io::Cursor::new(stdin.as_bytes().to_vec());
    let vars: Vec<(String, String)> = vars.iter().map(|(a, b)| (a.to_string(), b.to_string())).collect();
    let g = move |k: &str| vars.iter().find(|(a, _)| a == k).map(|(_, b)| b.clone()).unwrap_or_default();
    let code = run(
        &argv,
        &mut CliEnv {
            stdin: &mut inp,
            out: &mut out,
            err: &mut err,
            data_home: data.to_path_buf(),
            home: data.join("nohome"),
            new_summarizer: None,
            progress: None,
            now: None,
            embed: super::lexical::vector,
            hermes_source: None,
            getenv: Some(&g),
        },
    );
    (code, String::from_utf8(out).unwrap(), String::from_utf8(err).unwrap())
}

fn call(name: &str, args: serde_json::Value) -> String {
    serde_json::json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":name,"arguments":args}}).to_string()
}

#[test]
fn parse_branch_refuses_escapes() {
    for bad in ["", "/abs", "/", "..", "a/..", "a/../b", ".", "a/./b", "a//b", "a/", "a b", "a\\b", "a/b/c/d/e/f/g/h/i"] {
        assert!(parse_branch(bad).is_err(), "{bad:?} was taken");
    }
    for good in ["trellis", "trellis/ui", "a.b-c_d/E9"] {
        assert!(parse_branch(good).is_ok(), "{good}");
    }
}

#[test]
fn a_link_at_any_level_is_refused() {
    let (_, store) = fixture();
    std::os::unix::fs::symlink(&store, store.join(BRANCHES_DIR).join("evil")).unwrap();
    assert!(branch_dir(&store, "evil").is_err());
    std::os::unix::fs::symlink(store.join(BRANCHES_DIR).join("other"), store.join(BRANCHES_DIR).join("trellis").join(BRANCHES_DIR).join("sib")).unwrap();
    assert!(branch_dir(&store, "trellis/sib").is_err());
}

#[test]
fn a_bad_marker_is_closed_never_the_store() {
    let (data, store) = fixture();
    std::os::unix::fs::symlink(&store, store.join(BRANCHES_DIR).join("loop")).unwrap();
    for bad in ["..", "/tmp", "nope", "loop", "trellis/../other"] {
        let list = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}".to_string();
        let input = format!("{list}\n{}\n", call("recall", serde_json::json!({"query":"household budget sheet"})));
        let (_, out, _) = cli(&data, &[(BRANCH_VAR, bad)], &["serve"], &input);
        assert!(out.contains("\"tools\":[]") && !out.contains("budget"), "{bad}: {out}");
        let (code, text, _) = cli(&data, &[(BRANCH_VAR, bad)], &["context"], "");
        assert!(code == 0 && text.is_empty(), "{bad}: {code} {text}");
        let (code, text, _) = cli(&data, &[(BRANCH_VAR, bad)], &["recall", "budget"], "");
        assert!(code != 0 && !text.contains("budget"), "{bad}: {code} {text}");
    }
}

#[test]
fn a_branch_serves_itself_and_below_only() {
    let (data, store) = fixture();
    std::fs::create_dir_all(store.join(BRANCHES_DIR).join("trellis").join("children")).unwrap();
    std::os::unix::fs::symlink(store.join(BRANCHES_DIR).join("other"), store.join(BRANCHES_DIR).join("trellis").join("children").join("w-9")).unwrap();
    let calls = [
        call("recall", serde_json::json!({"query":"household budget sheet"})),
        call("recall", serde_json::json!({"query":"secret ledger"})),
        call("recall", serde_json::json!({"query":"epub parser lattice icon"})),
        call("zoom", serde_json::json!({"id":0,"n":1,"branch":".."})),
        call("zoom", serde_json::json!({"id":0,"n":1,"branch":"../other"})),
        call("zoom", serde_json::json!({"id":0,"n":1,"branch":"other"})),
        call("zoom", serde_json::json!({"id":0,"n":1,"child":"w-9"})),
        call("zoom", serde_json::json!({"id":0,"n":1,"branch":"/etc"})),
        call("zoom", serde_json::json!({"id":0,"n":1,"branch":"ui"})),
    ];
    let (_, out, _) = cli(&data, &[(BRANCH_VAR, "trellis")], &["serve"], &(calls.join("\n") + "\n"));
    let lines: Vec<&str> = out.trim().lines().collect();
    assert_eq!(lines.len(), 9, "{out}");
    assert!(!lines[0].contains("budget") && !lines[1].contains("ledger"), "{out}");
    assert!(lines[2].contains("0+1\\t") && lines[2].contains("ui:0+1\\t"), "{}", lines[2]);
    for l in &lines[3..8] {
        assert!(l.contains("\"isError\":true") && !l.contains("ledger"), "{l}");
    }
    assert!(lines[8].contains("lattice icon"), "{}", lines[8]);
    // A worker of the branch: no view tool, no view.
    let list = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}";
    let (_, out, _) = cli(&data, &[(BRANCH_VAR, "trellis"), (SKIP_VAR, "1")], &["serve"], &format!("{list}\n"));
    assert!(out.contains("\"recall\"") && !out.contains("\"view\""), "{out}");
    let (code, text, _) = cli(&data, &[(BRANCH_VAR, "trellis"), (SKIP_VAR, "1")], &["context"], "");
    assert!(code == 0 && text.is_empty());
}

#[test]
fn the_root_sees_every_branch_with_its_label_and_names_the_live_ones() {
    let (data, store) = fixture();
    let (_, out, _) = cli(&data, &[], &["recall", "secret ledger epub lattice budget"], "");
    for want in ["0+1\t", "trellis:0+1\t", "trellis/ui:0+1\t", "other:0+1\t"] {
        assert!(out.contains(want), "no {want} in {out}");
    }
    close_branch(&store, "other", &None).unwrap();
    let text = super::live::view_context_of(&store).unwrap();
    assert!(text.contains(BRANCH_LINES_HEAD) && text.contains("- trellis: trellis reader uses the epub parser"), "{text}");
    assert!(!text.contains("- other:") && !text.contains("- ui:"), "{text}");
    assert_eq!(Chat::open_read_only(&store).unwrap().view_context(), text);
    let (_, out, _) = cli(&data, &[], &["branches"], "");
    assert!(out.contains("other\tclosed\t1\t") && out.contains("trellis/ui\topen\t1\t"), "{out}");
    let (_, out, _) = cli(&data, &[], &["zoom", "0", "1", "--branch", "trellis/ui"], "");
    assert!(out.contains("lattice icon"), "{out}");
}

#[test]
fn a_branch_process_cannot_write_by_hand() {
    let (data, _) = fixture();
    for verb in [&["append", "x"][..], &["tend"], &["stores"], &["close", "trellis"], &["import"], &["migrate"]] {
        let (code, _, err) = cli(&data, &[(BRANCH_VAR, "trellis")], verb, "");
        assert!(code == 1 && err.contains("a branch of the memory"), "{verb:?}: {code} {err}");
    }
}

fn transcript(dir: &Path, sid: &str, lines: &[(&str, &str)]) -> PathBuf {
    std::fs::create_dir_all(dir).unwrap();
    let p = dir.join(format!("{sid}.jsonl"));
    let body: Vec<String> = lines
        .iter()
        .enumerate()
        .map(|(k, (typ, text))| {
            format!("{{\"type\":\"{typ}\",\"uuid\":\"{sid}-{k}\",\"sessionId\":\"{sid}\",\"cwd\":\"/work/proj\",\"timestamp\":\"2026-11-05T10:00:00Z\",\"message\":{{\"role\":\"{typ}\",\"content\":[{{\"type\":\"text\",\"text\":\"{text}\"}}]}}}}")
        })
        .collect();
    std::fs::write(&p, body.join("\n") + "\n").unwrap();
    p
}

fn texts(dir: &Path) -> String {
    if !has_main(dir) {
        return String::new();
    }
    let c = Chat::open_read_only(dir).unwrap();
    (0..c.count()).map(|i| c.message(i).unwrap().text + "\n").collect()
}

#[test]
fn the_branch_hook_logs_into_the_branch_never_the_store() {
    let (data, store) = fixture();
    let proj = temp_dir().join("projects").join("-p");
    let tr = transcript(&proj, "lead1", &[("user", "lead brief: build the reader"), ("assistant", "reader plan ready")]);
    let ev = |e: &str| serde_json::json!({"hook_event_name": e, "session_id": "lead1", "transcript_path": tr.to_string_lossy()}).to_string();
    let (code, out, _) = cli(&data, &[(BRANCH_VAR, "trellis")], &["hook"], &ev("SessionStart"));
    assert!(code == 0 && out.contains("epub parser") && !out.contains("budget"), "{out}");
    assert_eq!(routes_for(&load_routes(&store), "lead1", ""), "trellis");
    cli(&data, &[(BRANCH_VAR, "trellis")], &["hook"], &ev("Stop"));
    assert!(texts(&store.join(BRANCHES_DIR).join("trellis")).contains("reader plan ready"));
    assert!(!texts(&store).contains("reader"));
    // The owner's own hook on a mapped session routes it too.
    transcript(&proj, "lead1", &[("user", "lead brief: build the reader"), ("assistant", "reader plan ready"), ("user", "resumed from a plain terminal")]);
    cli(&data, &[], &["hook"], &ev("Stop"));
    assert!(!texts(&store).contains("resumed"));
    assert!(texts(&store.join(BRANCHES_DIR).join("trellis")).contains("resumed"));
}

#[test]
fn follow_routes_a_mapped_session_from_its_first_line() {
    let (_, store) = fixture();
    let proj = temp_dir().join("projects").join("-p");
    let sid = new_branch_session(&store, "trellis/ui").unwrap();
    transcript(&proj, &sid, &[("user", "first words of the ui lead")]);
    transcript(&proj, "owner1", &[("user", "the owner speaks at the root")]);
    let mut log = Vec::new();
    let mut o = FollowOpts::new(store.clone(), &mut log);
    o.sources = vec![Box::new(ClaudeSource::new(proj.parent().unwrap().to_path_buf()))];
    follow_once(&super::ctx::Ctx::default(), &mut o).unwrap();
    let root = texts(&store);
    assert!(!root.contains("ui lead") && root.contains("owner speaks"), "{root}");
    assert!(texts(&store.join(BRANCHES_DIR).join("trellis").join(BRANCHES_DIR).join("ui")).contains("first words of the ui lead"));
}

#[test]
fn context_with_a_session_maps_a_hermes_chat() {
    let (data, store) = fixture();
    let (code, out, _) = cli(&data, &[(BRANCH_VAR, "trellis")], &["context", "--session", "h-77"], "");
    assert!(code == 0 && out.contains("epub parser"), "{out}");
    assert_eq!(load_routes(&store).get("hermes:h-77").map(|s| s.as_str()), Some("trellis"));
    add_route(&store, "other", &["hermes:h-77"]).unwrap();
    assert_eq!(load_routes(&store).get("hermes:h-77").map(|s| s.as_str()), Some("trellis"));
}

#[test]
fn tend_lists_branches_and_their_workers() {
    let (data, store) = fixture();
    let long = "word ".repeat(300);
    for p in ["", "trellis", "trellis/ui", "other"] {
        put(&store, p, &long);
    }
    let d = existing_branch(&store, "trellis").unwrap();
    let mut w = super::child::open_child(&d, "trellis-1", Config::default()).unwrap();
    assert!(w.append(KIND_USER, &long).1.is_none());
    w.close();
    let (_, out, _) = cli(&data, &[], &["tend", "--dry-run"], "");
    for want in ["branches/trellis/1+1", "branches/trellis/children/trellis-1/0+1", "branches/trellis/branches/ui/1+1", "branches/other/1+1"] {
        assert!(out.contains(want), "no {want} in {out}");
    }
}

// Review fix 1: the Rust twins of Go's branch_fix1_test.go.
#[test]
fn a_forged_session_start_cannot_claim_the_owners_chat() {
    let (data, store) = fixture();
    let proj = temp_dir().join("projects").join("-p");
    let tr = transcript(&proj, "owner1", &[("user", "owner first words at the root")]);
    let ev = |e: &str| serde_json::json!({"hook_event_name": e, "session_id": "owner1", "transcript_path": tr.to_string_lossy()}).to_string();
    cli(&data, &[], &["hook"], &ev("Stop"));
    assert!(root_holds(&store, "owner1"));
    cli(&data, &[(BRANCH_VAR, "trellis")], &["hook"], &ev("SessionStart"));
    assert!(routes_for(&load_routes(&store), "owner1", &tr.to_string_lossy()).is_empty());
    assert!(add_route(&store, "trellis", &["owner1"]).is_err());
    std::fs::write(store.join(ROUTE_FILE), "owner1\ttrellis\n").unwrap();
    transcript(&proj, "owner1", &[("user", "owner first words at the root"), ("user", "owner second words")]);
    cli(&data, &[], &["hook"], &ev("Stop"));
    assert!(texts(&store).contains("owner second words"));
    assert!(!texts(&store.join(BRANCHES_DIR).join("trellis")).contains("owner"));
    let (code, out, _) = cli(&data, &[], &["routes"], "");
    assert!(code == 0 && out.contains("owner1\ttrellis"), "{out}");
    let (code, _, err) = cli(&data, &[(BRANCH_VAR, "trellis")], &["unroute", "owner1"], "");
    assert!(code == 1 && err.contains("a branch of the memory"), "{err}");
    let (code, out, _) = cli(&data, &[], &["unroute", "owner1"], "");
    assert!(code == 0 && out.contains("unrouted: owner1"));
    assert!(!load_routes(&store).contains_key("owner1"));
}

#[test]
fn a_dir_route_sends_the_worktrees_sessions_to_the_branch() {
    let (_, store) = fixture();
    let wt = temp_dir().join("trellis-worktrees").join("lead-trellis");
    add_route(&store, "trellis", &[&format!("{DIR_ROUTE_PREFIX}{}", wt.display())]).unwrap();
    let proj = temp_dir().join("projects").join("-wt");
    std::fs::create_dir_all(&proj).unwrap();
    let line = format!(
        "{{\"type\":\"user\",\"uuid\":\"cp1-a\",\"sessionId\":\"cp1\",\"cwd\":{},\"timestamp\":\"2026-11-05T10:00:00Z\",\"message\":{{\"role\":\"user\",\"content\":[{{\"type\":\"text\",\"text\":\"draft the release notes\"}}]}}}}\n",
        serde_json::to_string(&wt.join("sub").to_string_lossy()).unwrap()
    );
    std::fs::write(proj.join("cp1.jsonl"), line).unwrap();
    let mut log = Vec::new();
    let mut o = FollowOpts::new(store.clone(), &mut log);
    o.sources = vec![Box::new(ClaudeSource::new(proj.parent().unwrap().to_path_buf()))];
    follow_once(&super::ctx::Ctx::default(), &mut o).unwrap();
    assert!(!texts(&store).contains("release notes"));
    assert!(texts(&store.join(BRANCHES_DIR).join("trellis")).contains("release notes"));
}

#[test]
fn a_closed_branch_stays_closed_and_view_names_branches() {
    let (data, store) = fixture();
    close_branch(&store, "trellis", &None).unwrap();
    ensure_branch(&store, "trellis", &None).unwrap();
    let d = existing_branch(&store, "trellis").unwrap();
    assert_eq!(read_branch(&d).unwrap().state, BRANCH_CLOSED);
    reopen_branch(&store, "trellis", &None).unwrap();
    assert_eq!(read_branch(&d).unwrap().state, BRANCH_OPEN);
    let (_, _, err) = cli(&data, &[(SKIP_VAR, "1")], &["stores"], "");
    assert!(err.contains("this process is a worker of the memory; stores"), "{err}");
    let (_, out, _) = cli(&data, &[], &["view"], "");
    assert!(out.contains(BRANCH_LINES_HEAD) && out.contains("- trellis: "), "{out}");
}

#[test]
fn the_owners_session_start_claims_its_session() {
    let (data, store) = fixture();
    let proj = temp_dir().join("projects").join("-p");
    let tr = transcript(&proj, "fresh1", &[("user", "a brand new owner chat")]);
    let ev = |e: &str| serde_json::json!({"hook_event_name": e, "session_id": "fresh1", "transcript_path": tr.to_string_lossy()}).to_string();
    cli(&data, &[], &["hook"], &ev("SessionStart"));
    assert_eq!(load_routes(&store).get("fresh1").map(|s| s.as_str()), Some(ROOT_CLAIM));
    cli(&data, &[(BRANCH_VAR, "trellis")], &["hook"], &ev("SessionStart"));
    assert_eq!(load_routes(&store).get("fresh1").map(|s| s.as_str()), Some(ROOT_CLAIM));
    cli(&data, &[], &["hook"], &ev("Stop"));
    assert!(texts(&store).contains("brand new owner chat"));
    assert!(!skip_with_routes(&store).contains("fresh1"));
}
