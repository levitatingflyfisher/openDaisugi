//! pane.attach with fit, through the server's own connection loop: the
//! agent is fitted only while it is alone, the layout record keeps the
//! owner's size, and a detach undoes the fit.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::channel;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use serde_json::Value;

use super::{Config, PeerFacts, Server};

fn scratch(name: &str) -> std::path::PathBuf {
    let d = crate::testhome::guard(std::env::temp_dir().join(format!("coppice-rs-fit-{}-{name}", std::process::id())));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

fn server(dir: &std::path::Path) -> Arc<Server> {
    Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.to_path_buf(),
        start_dir: dir.to_path_buf(),
        voice: Default::default(),
        foreman: Default::default(),
    })
}

/// One connection to the server: send a request, read until its reply.
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

    fn send(&mut self, line: &str) {
        self.w.write_all(line.as_bytes()).unwrap();
        self.w.write_all(b"\n").unwrap();
    }

    fn reply(&mut self, id: &str) -> Value {
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

    fn call(&mut self, line: &str, id: &str) -> Value {
        self.send(line);
        self.reply(id)
    }
}

fn size(s: &Arc<Server>) -> (i64, i64) {
    s.live("w1:p1").unwrap().grid.size()
}

fn floor(name: &str) -> (Arc<Server>, std::path::PathBuf, Conn, Conn) {
    let dir = scratch(name);
    let s = server(&dir);
    let mut a = Conn::new(&s);
    let b = Conn::new(&s);
    let cwd = dir.to_string_lossy().to_string();
    let r = a.call(
        &format!(
            r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","cmd_argv":["sh","-c","exec sleep 600"],"kind":"pty","cols":40,"rows":10}}"#
        ),
        "c",
    );
    assert_eq!(r["ok"], true, "{r}");
    (s, dir, a, b)
}

#[test]
fn a_fit_sizes_the_agent_when_alone_keeps_the_record_and_detach_puts_it_back() {
    let (s, dir, mut a, _b) = floor("alone");
    let r = a.call(
        r#"{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "1",
    );
    assert_eq!(r["result"]["fitted"], true, "{r}");
    assert_eq!(size(&s), (50, 30));
    let rec = s.tree.pane("w1:p1").unwrap();
    assert_eq!(
        (rec.cols, rec.rows),
        (40, 10),
        "the record holds the phone's size"
    );
    let saved = std::fs::read_to_string(dir.join("layout.json")).unwrap();
    assert!(
        !saved.contains("\"cols\": 50") && !saved.contains("\"cols\":50"),
        "{saved}"
    );
    let r = a.call(
        r#"{"id":"2","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":44,"rows":20}}"#,
        "2",
    );
    assert_eq!(r["result"]["fitted"], true);
    assert_eq!(size(&s), (44, 20));
    a.call(r#"{"id":"3","cmd":"pane.detach","pane":"w1:p1"}"#, "3");
    assert_eq!(size(&s), (40, 10));
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_fit_does_nothing_beside_another_client_and_another_attach_ends_one() {
    let (s, dir, mut a, mut b) = floor("shared");
    a.call(
        r#"{"id":"1","cmd":"pane.attach","pane":"w1:p1","view_only":true}"#,
        "1",
    );
    let r = b.call(
        r#"{"id":"2","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "2",
    );
    assert_eq!(r["result"]["fitted"], false, "{r}");
    assert_eq!(size(&s), (40, 10));
    b.call(r#"{"id":"3","cmd":"pane.detach","pane":"w1:p1"}"#, "3");
    a.call(r#"{"id":"4","cmd":"pane.detach","pane":"w1:p1"}"#, "4");
    // Alone again, b fits; a second attach by a puts the old size back first.
    let r = b.call(
        r#"{"id":"5","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "5",
    );
    assert_eq!(r["result"]["fitted"], true, "{r}");
    a.call(
        r#"{"id":"6","cmd":"pane.attach","pane":"w1:p1","view_only":true}"#,
        "6",
    );
    assert_eq!(size(&s), (40, 10));
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_resize_ends_a_fit_and_a_fit_is_capped() {
    let (s, dir, mut a, mut b) = floor("resize");
    a.call(
        r#"{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "1",
    );
    let r = b.call(
        r#"{"id":"2","cmd":"pane.resize","pane":"w1:p1","cols":60,"rows":20}"#,
        "2",
    );
    assert_eq!(r["ok"], true, "{r}");
    a.call(r#"{"id":"3","cmd":"pane.detach","pane":"w1:p1"}"#, "3");
    assert_eq!(size(&s), (60, 20), "the detach undid the resize");
    for (i, bad) in [
        r#"{"cols":1001,"rows":5}"#,
        r#"{"cols":5,"rows":501}"#,
        r#"{"cols":0,"rows":5}"#,
        r#""x""#,
    ]
    .iter()
    .enumerate()
    {
        let id = format!("b{i}");
        let r = a.call(
            &format!(r#"{{"id":"{id}","cmd":"pane.attach","pane":"w1:p1","fit":{bad}}}"#),
            &id,
        );
        assert_eq!(r["error"]["code"], "bad_request", "{bad}: {r}");
    }
    let _ = std::fs::remove_dir_all(&dir);
}

/// An attach is in place before its save, which comes after the fit lock
/// is let go and before the reply (CP-R-53). A detach that arrives while
/// the attach waits in that save therefore finds the pane attached: it is
/// ok and puts the size back. Once the attach replies, no frame pump, fit
/// or subscription of that attach is left.
#[test]
fn a_detach_during_a_held_attach_save_is_ok_and_leaves_nothing() {
    let dir = scratch("held");
    let s = server(&dir);
    let armed = Arc::new(AtomicBool::new(false));
    let (entered_tx, entered_rx) = channel::<()>();
    let (release_tx, release_rx) = channel::<()>();
    let entered_tx = Mutex::new(entered_tx);
    let release_rx = Mutex::new(release_rx);
    {
        let armed = armed.clone();
        let hook: Box<dyn Fn() + Send + Sync> = Box::new(move || {
            if armed.swap(false, Ordering::SeqCst) {
                let _ = entered_tx.lock().unwrap().send(());
                let _ = release_rx.lock().unwrap().recv();
            }
        });
        assert!(s.before_data_write.set(hook).is_ok());
    }
    let mut a = Conn::new(&s);
    let cwd = dir.to_string_lossy().to_string();
    a.call(
        &format!(
            r#"{{"id":"c","cmd":"pane.create","cwd":"{cwd}","cmd_argv":["sh","-c","exec sleep 600"],"kind":"pty","cols":40,"rows":10}}"#
        ),
        "c",
    );
    armed.store(true, Ordering::SeqCst);
    a.send(
        r#"{"id":"a","cmd":"pane.attach","pane":"w1:p1","cols":44,"rows":12,"fit":{"cols":50,"rows":30}}"#,
    );
    entered_rx
        .recv_timeout(Duration::from_secs(10))
        .expect("the attach never reached its save");
    // The attach's flow: its pump and the pump's drain each hold it.
    let flow = s
        .clients()
        .iter()
        .find_map(|c| c.st.lock().unwrap().flows.get("w1:p1").cloned())
        .expect("the attach is not in place during its save");
    let r = a.call(r#"{"id":"d0","cmd":"pane.detach","pane":"w1:p1"}"#, "d0");
    assert_eq!(r["ok"], true, "a detach during the attach's save: {r}");
    assert_eq!(size(&s), (44, 12), "the explicit size did not come back");
    release_tx.send(()).unwrap();
    let r = a.reply("a");
    assert_eq!(r["ok"], true, "{r}");
    for c in s.clients() {
        let st = c.st.lock().unwrap();
        assert!(
            !st.attached.contains_key("w1:p1"),
            "the pane is still attached"
        );
        assert!(!st.flows.contains_key("w1:p1"), "a flow is left");
        assert!(
            !st.sub_panes.contains_key("w1:p1") && !st.auto_panes.contains("w1:p1"),
            "a subscription is left"
        );
    }
    assert!(s.fits.lock().unwrap().is_empty(), "a fit hold is left");
    // The pump and its drain end on the pump's next tick and let go of
    // the flow.
    let end = std::time::Instant::now() + Duration::from_secs(3);
    while Arc::strong_count(&flow) > 1 {
        assert!(
            std::time::Instant::now() < end,
            "the pump or its drain still runs: {} holders of the flow",
            Arc::strong_count(&flow) - 1
        );
        thread::sleep(Duration::from_millis(10));
    }
    let _ = std::fs::remove_dir_all(&dir);
}

/// A resize from another client ends a phone's fit, and the presence it
/// sends makes the phone attach with its fit again. That refit must not
/// undo the resize: the server refuses to fit that client again until it
/// detaches and attaches fresh.
#[test]
fn a_resize_that_ends_a_fit_refuses_that_clients_refit_until_a_fresh_attach() {
    let (s, dir, mut phone, mut desk) = floor("resized");
    let r = phone.call(
        r#"{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "1",
    );
    assert_eq!(r["result"]["fitted"], true, "{r}");
    let r = desk.call(
        r#"{"id":"2","cmd":"pane.resize","pane":"w1:p1","cols":60,"rows":20}"#,
        "2",
    );
    assert_eq!(r["ok"], true, "{r}");
    let r = phone.call(
        r#"{"id":"3","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "3",
    );
    assert_eq!(r["result"]["fitted"], false, "{r}");
    assert_eq!(size(&s), (60, 20), "the refit undid the resize");
    phone.call(r#"{"id":"4","cmd":"pane.detach","pane":"w1:p1"}"#, "4");
    let r = phone.call(
        r#"{"id":"5","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}"#,
        "5",
    );
    assert_eq!(
        r["result"]["fitted"], true,
        "a fresh attach was refused its fit: {r}"
    );
    phone.call(r#"{"id":"6","cmd":"pane.detach","pane":"w1:p1"}"#, "6");
    assert_eq!(size(&s), (60, 20));
    let _ = std::fs::remove_dir_all(&dir);
}
