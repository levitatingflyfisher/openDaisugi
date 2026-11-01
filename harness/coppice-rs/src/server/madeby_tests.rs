//! made_by: a pane another pane's connection created is marked, and a
//! resumed pane keeps the mark.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use std::time::Duration;

use super::{Config, PeerFacts, Server};
use crate::layout;

struct Conn {
    w: UnixStream,
    r: BufReader<UnixStream>,
}

impl Conn {
    fn new(s: &Arc<Server>) -> Conn {
        let (a, b) = UnixStream::pair().unwrap();
        let s = s.clone();
        std::thread::spawn(move || s.serve_conn(b, PeerFacts::default()));
        a.set_read_timeout(Some(Duration::from_secs(15))).unwrap();
        Conn { r: BufReader::new(a.try_clone().unwrap()), w: a }
    }

    fn call(&mut self, line: &str, id: &str) -> serde_json::Value {
        self.w.write_all(line.as_bytes()).unwrap();
        self.w.write_all(b"\n").unwrap();
        loop {
            let mut l = String::new();
            assert!(self.r.read_line(&mut l).expect("no reply within 15 s") > 0);
            let v: serde_json::Value = serde_json::from_str(&l).unwrap();
            if v["id"] == id {
                return v;
            }
        }
    }
}

#[test]
fn a_resumed_pane_keeps_made_by() {
    let dir = crate::testhome::guard(
        std::env::temp_dir().join(format!("coppice-rs-resume-madeby-{}", std::process::id())),
    );
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        steward: Default::default(),
    });
    let mut c = Conn::new(&s);
    let cwd = dir.to_string_lossy().to_string();
    let r = c.call(
        &format!(r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","kind":"pty","cmd_argv":["sh","-c","exit 0"]}}"#),
        "c",
    );
    let id = r["result"]["pane"].as_str().unwrap().to_string();
    for _ in 0..100 {
        if s.tree.pane(&id).is_some_and(|p| p.closed) {
            break;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    s.update_pane(&id, |p| p.made_by = layout::MADE_BY_AGENT.into()).unwrap();
    let r = c.call(&format!(r#"{{"id":"r","cmd":"pane.resume","pane":"{id}"}}"#), "r");
    assert_eq!(r["ok"], true, "{r}");
    let new_id = r["result"]["pane"].as_str().unwrap();
    assert_eq!(s.tree.pane(new_id).unwrap().made_by, layout::MADE_BY_AGENT);
}
