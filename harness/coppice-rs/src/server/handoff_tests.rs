//! F12B-R-6: a new agent that cannot take its brief goes, and the reply
//! says no new agent started.

use std::sync::Arc;
use std::time::{Duration, Instant};

use serde_json::json;

use super::{Client, Config, Server};
use crate::proto;

#[test]
fn a_refused_brief_removes_the_new_agent() {
    let dir = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-move-{}", std::process::id())));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let screen = format!(
        "{}/../coppice/testdata/screens/claude/blocked-10.txt",
        env!("CARGO_MANIFEST_DIR")
    );
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    s.start_detection();
    let (a, _b) = std::os::unix::net::UnixStream::pair().unwrap();
    let (c, _rx) = Client::new(a);
    let line = json!({
        "id": "mk", "cmd": "pane.create", "cwd": dir.to_string_lossy(), "kind": "pty",
        "harness": "claude", "label": "fake", "cols": 120, "rows": 30,
        "cmd_argv": ["bash", "-c", format!("grep -v '^#rule:' {screen}; sleep 20")],
    });
    let (req, _, _) = proto::decode_line(&line.to_string());
    let made = s.pane_create(Some(&c), &req);
    assert!(made.ok, "{:?}", made.error);
    let id = made.result.as_ref().unwrap()["pane"]
        .as_str()
        .unwrap()
        .to_string();
    // Wait until the screen reads as a question: then every prompt is refused.
    let end = Instant::now() + Duration::from_secs(15);
    while s
        .screen_verdict(&id)
        .map(|v| v.state.as_str() != "blocked")
        .unwrap_or(true)
    {
        assert!(Instant::now() < end, "the trust screen never read blocked");
        std::thread::sleep(Duration::from_millis(50));
    }
    let got = send(&s, &c, &id);
    assert!(!got.ok);
    let msg = got.error.as_ref().unwrap().1.clone();
    assert!(
        msg.starts_with("codex could not take the brief, so no new agent started: "),
        "{msg}"
    );
    assert!(
        s.tree.pane(&id).map(|p| p.closed).unwrap_or(true),
        "the new agent stayed"
    );
    s.close();
    let _ = std::fs::remove_dir_all(&dir);
}

fn send(s: &Arc<Server>, c: &Arc<Client>, id: &str) -> proto::Response {
    s.send_brief(c, "mv", id, "codex", "the brief")
}
