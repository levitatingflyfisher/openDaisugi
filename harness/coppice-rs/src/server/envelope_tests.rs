//! The envelope proposal flow, as Go's envelope_propose_test.go tests it.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::{Arc, Mutex, MutexGuard};
use std::thread;
use std::time::Duration;

use serde_json::Value;

use super::steward_work::envelope_dir;
use super::{Config, PeerFacts, Server};

const ENV_A: &str = r#"{"id":"env_a","generated_by":"steward","task":"build trellis","permissions":{"file_read":["/src/trellis"],"file_write":["/src/trellis/lib"],"shell":true,"shell_allowlist":["go test"],"network":true,"network_hosts":["pub.dev"]}}"#;

/// COPPICE_DAISUGI_BIN, the config file and the envelope dir are process
/// globals. Each test here sets them for itself, so the tests take turns.
static SERIAL: Mutex<()> = Mutex::new(());

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
        let id: Value = serde_json::from_str::<Value>(line).unwrap()["id"].clone();
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

fn setup(name: &str) -> (Arc<Server>, String, MutexGuard<'static, ()>) {
    let turn = SERIAL.lock().unwrap_or_else(|e| e.into_inner());
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-env-{}-{}", name, std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    // `tree check F F` passes unless F holds the word BAD.
    let bin = dir.join("daisugi");
    std::fs::write(
        &bin,
        "#!/bin/sh\nif [ \"$1 $2\" = \"tree check\" ] && grep -q BAD \"$3\"; then echo 'permissions: not valid'; exit 2; fi\nexit 0\n",
    )
    .unwrap();
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    std::env::set_var("COPPICE_DAISUGI_BIN", &bin);
    let project = dir.join(name);
    std::fs::create_dir_all(&project).unwrap();
    let mut cfg = crate::config::load().ok().flatten().unwrap_or_default();
    cfg.projects.push(project.to_string_lossy().into_owned());
    crate::config::save(&cfg).unwrap();
    let _ = std::fs::remove_file(envelope_dir().join(format!("{name}.json")));
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        steward: Default::default(),
    });
    (s, name.to_string(), turn)
}

fn propose(c: &mut Conn, name: &str, env: &str) -> Value {
    c.call(&format!(
        r#"{{"id":"p","cmd":"envelope.propose","project":"{name}","envelope":{env},"tokens":50000,"turns":30}}"#
    ))
}

fn pending(c: &mut Conn) -> Vec<Value> {
    let v = c.call(r#"{"id":"l","cmd":"envelope.pending"}"#);
    assert_eq!(v["ok"], true, "{v}");
    v["result"]["proposals"].as_array().unwrap().clone()
}

/// Reads the pending card and approves the stamp it shows, as the phone does.
fn approve(c: &mut Conn, name: &str) -> Value {
    let at = pending(c)
        .iter()
        .find(|p| p["project"] == name)
        .map(|p| p["at"].as_str().unwrap().to_string())
        .unwrap_or_else(|| "none".into());
    c.call(&format!(r#"{{"id":"a","cmd":"envelope.approve","project":"{name}","at":"{at}"}}"#))
}

#[test]
fn a_proposal_never_writes_the_file_and_approve_writes_exactly_it() {
    let (s, name, _turn) = setup("envprop-a");
    let mut c = Conn::new(&s);
    let r = propose(&mut c, &name, ENV_A);
    assert_eq!(r["ok"], true, "{r}");
    let file = envelope_dir().join(format!("{name}.json"));
    assert!(!file.exists(), "a proposal wrote the operator's file");
    let cards = pending(&mut c);
    assert_eq!(cards.len(), 1);
    assert_eq!(cards[0]["project"], name.as_str());
    assert_eq!(cards[0]["tokens"], 50000);
    assert_eq!(cards[0]["write"][0], "/src/trellis/lib");
    assert_eq!(cards[0]["reach"]["hosts"][0], "pub.dev");
    let r = approve(&mut c, &name);
    assert_eq!(r["ok"], true, "{r}");
    let text = std::fs::read_to_string(&file).unwrap();
    let got: Value = serde_json::from_str(&text).unwrap();
    assert_eq!(got["tokens"], 50000);
    assert_eq!(got["turns"], 30);
    assert!(text.contains(&format!("\"envelope\": {ENV_A}")), "{text}");
    assert!(pending(&mut c).is_empty());
    let _ = std::fs::remove_file(&file);
}

#[test]
fn decline_drops_it_and_a_second_proposal_replaces_the_first() {
    let (s, name, _turn) = setup("envprop-b");
    let mut c = Conn::new(&s);
    propose(&mut c, &name, ENV_A);
    let env_b = ENV_A.replace("env_a", "env_b");
    propose(&mut c, &name, &env_b);
    assert_eq!(pending(&mut c).len(), 1);
    let r = c.call(&format!(r#"{{"id":"d","cmd":"envelope.decline","project":"{name}"}}"#));
    assert_eq!(r["ok"], true);
    assert!(pending(&mut c).is_empty());
    assert!(!envelope_dir().join(format!("{name}.json")).exists());
    propose(&mut c, &name, ENV_A);
    propose(&mut c, &name, &env_b);
    approve(&mut c, &name);
    let file = envelope_dir().join(format!("{name}.json"));
    let text = std::fs::read_to_string(&file).unwrap();
    assert!(text.contains("env_b") && !text.contains("env_a"), "{text}");
    let _ = std::fs::remove_file(&file);
}

#[test]
fn a_bad_proposal_is_refused_and_a_pane_never_approves() {
    let (s, name, _turn) = setup("envprop-c");
    let mut c = Conn::new(&s);
    let r = propose(&mut c, &name, &ENV_A.replace("build trellis", "BAD"));
    assert_eq!(r["ok"], false);
    assert!(r["error"]["message"].as_str().unwrap().contains("check"), "{r}");
    let r = c.call(&format!(
        r#"{{"id":"p","cmd":"envelope.propose","project":"{name}","envelope":{ENV_A},"checks":{{"t":"rm -rf /"}}}}"#
    ));
    assert_eq!(r["ok"], false);
    assert!(pending(&mut c).is_empty());
    // A pane proposes, and the operator's verbs refuse it.
    let mut p = Conn::new(&s);
    p.call(r#"{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}"#);
    assert_eq!(propose(&mut p, &name, ENV_A)["ok"], true);
    for (id, cmd) in [("a", "envelope.approve"), ("d", "envelope.decline"), ("l", "envelope.pending")] {
        let r = p.call(&format!(r#"{{"id":"{id}","cmd":"{cmd}","project":"{name}"}}"#));
        assert_eq!(r["ok"], false, "{cmd}");
        assert_eq!(r["error"]["message"], super::envelope::PROPOSAL_REFUSAL);
    }
    assert!(!envelope_dir().join(format!("{name}.json")).exists());
}

fn hand_pending(s: &Arc<Server>, name: &str, body: &str) {
    let dir = s.cfg.data_dir.join("envelope-proposals");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(dir.join(format!("{name}.json")), body).unwrap();
}

#[test]
fn approve_needs_the_stamp_and_a_same_second_replacement_changes_it() {
    let (s, name, _turn) = setup("envprop-d");
    let mut c = Conn::new(&s);
    propose(&mut c, &name, ENV_A);
    for line in [
        format!(r#"{{"id":"a","cmd":"envelope.approve","project":"{name}"}}"#),
        format!(r#"{{"id":"a","cmd":"envelope.approve","project":"{name}","at":""}}"#),
    ] {
        assert_eq!(c.call(&line)["ok"], false, "{line}");
    }
    assert!(!envelope_dir().join(format!("{name}.json")).exists());
    assert_eq!(pending(&mut c).len(), 1);
    let at1 = pending(&mut c)[0]["at"].as_str().unwrap().to_string();
    propose(&mut c, &name, &ENV_A.replace("env_a", "env_b"));
    let at2 = pending(&mut c)[0]["at"].as_str().unwrap().to_string();
    assert_ne!(at1, at2);
    let r = c.call(&format!(r#"{{"id":"a","cmd":"envelope.approve","project":"{name}","at":"{at1}"}}"#));
    assert_eq!(r["ok"], false, "{r}");
    assert!(!envelope_dir().join(format!("{name}.json")).exists());
}

#[test]
fn approve_checks_the_stored_envelope_again() {
    let (s, name, _turn) = setup("envprop-e");
    let mut c = Conn::new(&s);
    let cases = [
        ("a failing envelope", r#"{"project":"NAME","dir":"/x","envelope":{"task":"BAD"},"tokens":1,"turns":1,"at":"t1"}"#.to_string()),
        (
            "checks beside the envelope",
            format!(r#"{{"project":"NAME","dir":"/x","envelope":{ENV_A},"checks":{{"t":"rm -rf /"}},"tokens":1,"turns":1,"at":"t1"}}"#),
        ),
        ("a list", r#"{"project":"NAME","dir":"/x","envelope":["x"],"tokens":1,"turns":1,"at":"t1"}"#.to_string()),
        ("text", r#"{"project":"NAME","dir":"/x","envelope":"x","tokens":1,"turns":1,"at":"t1"}"#.to_string()),
    ];
    for (what, body) in cases {
        hand_pending(&s, &name, &body.replace("NAME", &name));
        let r = c.call(&format!(r#"{{"id":"a","cmd":"envelope.approve","project":"{name}","at":"t1"}}"#));
        assert_eq!(r["ok"], false, "{what} was approved");
        assert!(!envelope_dir().join(format!("{name}.json")).exists(), "{what}: approve wrote the file");
    }
    hand_pending(&s, &name, &format!(r#"{{"project":"{name}","dir":"/x","envelope":{{"task":"BAD"}},"tokens":1,"turns":1,"at":"t1"}}"#));
    let r = c.call(&format!(r#"{{"id":"a","cmd":"envelope.approve","project":"{name}","at":"t1"}}"#));
    assert!(r["error"]["message"].as_str().unwrap().contains("check"), "{r}");
}

#[test]
fn the_card_shows_every_field_that_widens() {
    let (s, name, _turn) = setup("envprop-f");
    let mut c = Conn::new(&s);
    let env = r#"{"id":"e","generated_by":"steward","task":"t","shell_interpreter_policy":"allow","permissions":{"shell":true,"shell_allowlist":["go"],"shell_allow_decomposition":true,"custom_step_allowlist":["approach_dish"],"mcp_rules":[{"tool":"github/*"}],"max_execution_time_s":900,"max_output_size_mb":50}}"#;
    propose(&mut c, &name, env);
    let also: Vec<String> =
        pending(&mut c)[0]["also"].as_array().unwrap().iter().map(|v| v.as_str().unwrap().to_string()).collect();
    let text = also.join("\n");
    for want in ["compound shell", "approach_dish", "github/*", "900 s per step", "50 MB", "interpreters: allow"] {
        assert!(text.contains(want), "the card omits {want}:\n{text}");
    }
    propose(&mut c, &name, ENV_A);
    let also = pending(&mut c)[0]["also"].as_array().unwrap().clone();
    assert_eq!(also.len(), 1);
    assert!(also[0].as_str().unwrap().contains("30 s per step"));
}

#[test]
fn approve_writes_the_proposed_bytes_whatever_the_escapes_and_key_order() {
    let (s, name, _turn) = setup("envprop-g");
    let mut c = Conn::new(&s);
    // Key order, a \u escape and a slash escape: a parse and write-back would change them.
    let env = r#"{"task":"caf\u00e9 a\/b","id":"z","generated_by":"s","permissions":{"shell":false}}"#;
    propose(&mut c, &name, env);
    let r = approve(&mut c, &name);
    assert_eq!(r["ok"], true, "{r}");
    let text = std::fs::read_to_string(envelope_dir().join(format!("{name}.json"))).unwrap();
    assert!(text.contains(&format!("\"envelope\": {env}")), "{text}");
    let _ = std::fs::remove_file(envelope_dir().join(format!("{name}.json")));
}
