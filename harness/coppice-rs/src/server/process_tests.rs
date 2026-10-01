//! The process fact at the moment of a read (CP-R-50): a read computes it
//! from the last write and its own clock, not from the last 1 s tick.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use serde_json::Value;

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
        Conn {
            r: BufReader::new(a.try_clone().unwrap()),
            w: a,
        }
    }

    fn call(&mut self, line: &str, id: &str) -> Value {
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

/// A pane that printed and was seen to print lists as working at once; the
/// tick used to leave "no output yet" for up to a second after the output
/// was on the screen.
#[test]
fn a_read_after_output_sees_the_output_at_once() {
    let dir = std::env::temp_dir().join(format!("coppice-rs-proc-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
        foreman: Default::default(),
    });
    let mut c = Conn::new(&s);
    let cwd = dir.to_string_lossy().to_string();
    for i in 0..5 {
        let r = c.call(
            &format!(
                r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","cmd_argv":["sh","-c","echo ready; exec sleep 30"],"kind":"pty"}}"#
            ),
            "c",
        );
        assert_eq!(r["ok"], true, "{r}");
        let id = r["result"]["pane"].as_str().unwrap().to_string();
        let r = c.call(
            &format!(
                r#"{{"id":"w","cmd":"pane.wait_output","pane":"{id}","contains":"ready","timeout_ms":10000}}"#
            ),
            "w",
        );
        assert_eq!(r["ok"], true, "{r}");
        let r = c.call(r#"{"id":"l","cmd":"pane.list"}"#, "l");
        let row = r["result"]["panes"]
            .as_array()
            .unwrap()
            .iter()
            .find(|p| p["id"] == id.as_str())
            .unwrap()
            .clone();
        assert_eq!(
            (row["state"].as_str(), row["detail"].as_str()),
            (Some("working"), Some("output in the last 5 s")),
            "run {i}: {row}"
        );
    }
    s.close();
    let _ = std::fs::remove_dir_all(&dir);
}
