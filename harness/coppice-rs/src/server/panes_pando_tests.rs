//! A pane an agent creates is a worker for Pando (the twin of the Go
//! `panes_pando_test.go`).

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::Value;

use super::process_tests::CloseOnDrop;
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
        Conn { r: BufReader::new(a.try_clone().unwrap()), w: a }
    }

    fn call(&mut self, line: &str) -> Value {
        let id = serde_json::from_str::<Value>(line).unwrap()["id"].clone();
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

fn setup(name: &str) -> (CloseOnDrop, std::path::PathBuf, String, std::path::PathBuf) {
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-pandopane-{}-{name}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        steward: Default::default(),
    });
    let out = dir.join("args.txt");
    let prog = dir.join("claude-stub.sh");
    std::fs::write(&prog, format!("#!/bin/sh\necho \"$@\" >> {}\nexit 0\n", out.display())).unwrap();
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(&prog, std::fs::Permissions::from_mode(0o755)).unwrap();
    (CloseOnDrop(s, dir.clone()), dir, prog.to_string_lossy().into_owned(), out)
}

fn create(c: &mut Conn, dir: &std::path::Path, prog: &str) -> String {
    let r = c.call(&format!(
        r#"{{"id":"c","cmd":"pane.create","cwd":"{}","kind":"pty","harness":"claude","cmd_argv":["{prog}","--model","opus"]}}"#,
        dir.display()
    ));
    assert_eq!(r["ok"], true, "{r}");
    r["result"]["pane"].as_str().unwrap().to_string()
}

fn lines(out: &std::path::Path, n: usize) -> Vec<String> {
    let end = Instant::now() + Duration::from_secs(5);
    loop {
        let t = std::fs::read_to_string(out).unwrap_or_default();
        let v: Vec<String> = t.lines().map(str::to_string).collect();
        if v.len() >= n || Instant::now() > end {
            return v;
        }
        thread::sleep(Duration::from_millis(20));
    }
}

fn listed(dir: &std::path::Path) -> std::collections::HashSet<String> {
    pando::live::load_skip(&pando::live::skip_list_in(&crate::steward::store_root(dir)))
}

#[test]
fn a_pane_an_agent_creates_is_marked_and_its_session_is_listed() {
    let (g, dir, prog, out) = setup("agent");
    let s = &g.0;
    let mut p = Conn::new(s);
    p.call(r#"{"id":"0","cmd":"hello","role":"pane","pane":"w1:p9"}"#);
    let id = create(&mut p, &dir, &prog);
    let rec = s.tree.pane(&id).unwrap();
    assert_eq!(rec.env.get(pando::live::SKIP_VAR).map(String::as_str), Some("1"));
    assert_eq!(
        rec.env.get(pando::live::SKIP_LIST_VAR).map(String::as_str),
        Some(pando::live::skip_list_in(&crate::steward::store_root(&dir)).to_str().unwrap())
    );
    let l = lines(&out, 1);
    let f: Vec<&str> = l[0].split_whitespace().collect();
    assert!(f.len() == 4 && f[0] == "--session-id" && f[2] == "--model", "{l:?}");
    assert!(listed(&dir).contains(f[1]));
    let end = Instant::now() + Duration::from_secs(2);
    while s.tree.pane(&id).unwrap().harness_session_id != f[1] {
        assert!(Instant::now() < end, "the record never got the session id");
        thread::sleep(Duration::from_millis(20));
    }
    assert!(!s.tree.pane(&id).unwrap().argv.unwrap().contains(&"--session-id".to_string()));
}

#[test]
fn an_operators_pane_is_not_marked() {
    let (g, dir, prog, out) = setup("operator");
    let mut c = Conn::new(&g.0);
    let id = create(&mut c, &dir, &prog);
    let rec = g.0.tree.pane(&id).unwrap();
    assert!(!rec.env.contains_key(pando::live::SKIP_VAR) && rec.harness_session_id.is_empty(), "{:?}", rec.env);
    let l = lines(&out, 1);
    assert!(!l[0].contains("--session-id"), "{l:?}");
    assert!(listed(&dir).is_empty());
}

#[test]
fn a_server_made_pane_such_as_the_stewards_is_not_marked() {
    let (g, dir, prog, _out) = setup("steward");
    let (req, _, err) = crate::proto::decode_line(&format!(
        r#"{{"id":"1","cmd":"pane.create","cwd":"{}","kind":"pty","harness":"claude","cmd_argv":["{prog}"]}}"#,
        dir.display()
    ));
    assert!(err.is_none());
    let resp = g.0.pane_create(None, &req);
    assert!(resp.ok, "{:?}", resp.error);
    for p in g.0.tree.panes() {
        assert!(!p.env.contains_key(pando::live::SKIP_VAR) && p.harness_session_id.is_empty());
    }
}

#[test]
fn a_resumed_agent_pane_keeps_the_marker() {
    let (g, dir, prog, out) = setup("resume");
    let mut p = Conn::new(&g.0);
    p.call(r#"{"id":"0","cmd":"hello","role":"pane","pane":"w1:p9"}"#);
    let id = create(&mut p, &dir, &prog);
    let r = p.call(&format!(r#"{{"id":"w","cmd":"pane.wait_output","pane":"{id}","state":"done","timeout_ms":5000}}"#));
    assert_eq!(r["ok"], true, "{r}");
    let r = p.call(&format!(r#"{{"id":"r","cmd":"pane.resume","pane":"{id}"}}"#));
    assert_eq!(r["ok"], true, "{r}");
    let new = r["result"]["pane"].as_str().unwrap().to_string();
    let rec = g.0.tree.pane(&new).unwrap();
    assert_eq!(rec.env.get(pando::live::SKIP_VAR).map(String::as_str), Some("1"));
    assert!(rec.env.contains_key(pando::live::SKIP_LIST_VAR));
    let l = lines(&out, 2);
    for line in &l {
        let sid = line.split_whitespace().nth(1).unwrap_or("");
        assert!(listed(&dir).contains(sid), "{line}");
    }
}

// Round 8b: Pando branches (the twin of the Go `panes_branch_test.go`).

fn lead(c: &mut Conn, s: &Arc<Server>, dir: &std::path::Path, prog: &str, branch: &str) -> String {
    pando::branch::ensure_branch(&s.branch_store_dir(), branch, &None).unwrap();
    let r = c.call(&format!(
        r#"{{"id":"c","cmd":"pane.create","cwd":"{}","kind":"pty","harness":"claude","cmd_argv":["{prog}","--model","opus"],"env":{{"{}":"{branch}"}}}}"#,
        dir.display(),
        pando::branch::BRANCH_VAR
    ));
    assert_eq!(r["ok"], true, "{r}");
    r["result"]["pane"].as_str().unwrap().to_string()
}

fn create_as(s: &Arc<Server>, as_pane: &str, dir: &std::path::Path, prog: &str, extra: &str) -> Value {
    let mut p = Conn::new(s);
    p.call(&format!(r#"{{"id":"0","cmd":"hello","role":"pane","pane":"{as_pane}"}}"#));
    p.call(&format!(
        r#"{{"id":"c","cmd":"pane.create","cwd":"{}","kind":"pty","harness":"claude","cmd_argv":["{prog}"]{extra}}}"#,
        dir.display()
    ))
}

#[test]
fn a_lead_pane_starts_with_a_mapped_session() {
    let (g, dir, prog, out) = setup("lead");
    let s = &g.0;
    let mut c = Conn::new(s);
    let id = lead(&mut c, s, &dir, &prog, "trellis");
    let l = lines(&out, 1);
    let f: Vec<&str> = l[0].split_whitespace().collect();
    assert!(f.len() >= 2 && f[0] == "--session-id", "{l:?}");
    let routes = pando::branch::load_routes(&s.branch_store_dir());
    assert_eq!(pando::branch::routes_for(&routes, f[1], ""), "trellis");
    let rec = s.tree.pane(&id).unwrap();
    assert!(!rec.env.contains_key(pando::live::SKIP_VAR));
    assert!(listed(&dir).is_empty());
}

#[test]
fn a_pane_a_lead_asks_for_stays_inside_its_branch() {
    let (g, dir, prog, _out) = setup("leadkids");
    let s = &g.0;
    let mut c = Conn::new(s);
    let lead_id = lead(&mut c, s, &dir, &prog, "trellis");
    let r = create_as(s, &lead_id, &dir, &prog, &format!(r#","env":{{"{}":"other","{}":"0"}}"#, pando::branch::BRANCH_VAR, pando::live::SKIP_VAR));
    assert_eq!(r["ok"], true, "{r}");
    let w = s.tree.pane(r["result"]["pane"].as_str().unwrap()).unwrap();
    assert_eq!(w.env.get(pando::branch::BRANCH_VAR).map(String::as_str), Some("trellis"));
    assert_eq!(w.env.get(pando::live::SKIP_VAR).map(String::as_str), Some("1"));
    let r = create_as(s, &lead_id, &dir, &prog, r#","long_running":true,"pando_branch":"ui""#);
    assert_eq!(r["ok"], true, "{r}");
    let n = s.tree.pane(r["result"]["pane"].as_str().unwrap()).unwrap();
    assert_eq!(n.env.get(pando::branch::BRANCH_VAR).map(String::as_str), Some("trellis/ui"));
    assert!(!n.env.contains_key(pando::live::SKIP_VAR));
    for bad in ["..", "../other", "/abs", "a/../b"] {
        let r = create_as(s, &lead_id, &dir, &prog, &format!(r#","long_running":true,"pando_branch":"{bad}""#));
        assert_ne!(r["ok"], true, "{bad}: {r}");
    }
    let r = create_as(s, &w.id, &dir, &prog, r#","long_running":true,"pando_branch":"x""#);
    assert_ne!(r["ok"], true, "a worker started a lead: {r}");
    let r = create_as(s, "w1:p99", &dir, &prog, r#","long_running":true,"pando_branch":"x""#);
    assert_ne!(r["ok"], true, "an agent with no branch started a lead: {r}");
}

#[test]
fn a_fenced_lead_reads_its_branch_only() {
    let (g, _dir, _prog, _out) = setup("fence");
    let s = &g.0;
    let d = pando::branch::ensure_branch(&s.branch_store_dir(), "trellis", &None).unwrap();
    let mut rec = crate::layout::Pane { harness: "claude".into(), cwd: "/work".into(), ..Default::default() };
    rec.env.insert(pando::branch::BRANCH_VAR.into(), "trellis".into());
    assert_eq!(s.pane_branch_dir(&rec), Some(d));
    rec.env.insert(pando::branch::BRANCH_VAR.into(), "../work".into());
    assert_eq!(s.pane_branch_dir(&rec), None);
    rec.env.clear();
    assert_eq!(s.pane_branch_dir(&rec), None);
}

#[test]
fn a_long_running_pane_must_be_claude() {
    let (g, dir, prog, _out) = setup("leadharness");
    let s = &g.0;
    let mut c = Conn::new(s);
    let lead_id = lead(&mut c, s, &dir, &prog, "trellis");
    let mut p = Conn::new(s);
    p.call(&format!(r#"{{"id":"0","cmd":"hello","role":"pane","pane":"{lead_id}"}}"#));
    let r = p.call(&format!(
        r#"{{"id":"c","cmd":"pane.create","cwd":"{}","kind":"pty","harness":"codex","cmd_argv":["{prog}"],"long_running":true,"pando_branch":"x"}}"#,
        dir.display()
    ));
    assert_ne!(r["ok"], true, "{r}");
    assert!(r.to_string().contains("takes harness claude"), "{r}");
}

#[test]
fn lead_worktree_names_are_distinct_and_a_stray_dir_refuses() {
    let mut seen = std::collections::HashSet::new();
    for b in ["trellis/ui", "trellis.ui", "trellis-ui", "Trellis-ui", "trellis_ui"] {
        assert!(seen.insert(super::panes_pando::lead_worktree_name(b)), "{b}");
    }
    let (g, dir, _prog, _out) = setup("leadwt");
    let repo = dir.join("repo");
    for c in [vec!["init", "-q", "-b", "main", repo.to_str().unwrap()], vec!["-C", repo.to_str().unwrap(), "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"]] {
        assert!(std::process::Command::new("git").args(&c).status().unwrap().success());
    }
    let r = repo.to_string_lossy().into_owned();
    let (wt, _) = g.0.lead_worktree(&r, "trellis").unwrap();
    assert!(std::process::Command::new("git").args(["-C", &r, "worktree", "remove", &wt]).status().unwrap().success());
    let (wt2, _) = g.0.lead_worktree(&r, "trellis").unwrap();
    assert_eq!(wt, wt2);
    assert!(std::process::Command::new("git").args(["-C", &r, "worktree", "remove", &wt]).status().unwrap().success());
    std::fs::create_dir_all(&wt).unwrap();
    let e = g.0.lead_worktree(&r, "trellis").unwrap_err();
    assert!(e.contains("does not start"), "{e}");
}
