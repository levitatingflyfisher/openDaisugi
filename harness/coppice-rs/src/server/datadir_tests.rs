//! A server that has closed writes nothing more into its data dir.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::channel;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use super::{Config, Server};

fn scratch(name: &str) -> std::path::PathBuf {
    let d = std::env::temp_dir().join(format!("coppice-rs-datadir-{}-{name}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

/// Close waits for a save into the data dir already in flight, and a save
/// that starts after close writes nothing. Before the fix, close returned
/// with the save still running, as a headless pane's pump could save the
/// layout while a test removed the data dir.
#[test]
fn close_waits_for_a_data_write_in_flight_and_allows_none_after() {
    let dir = scratch("late");
    let s = Server::new(Config {
        socket_path: dir.join("s.sock"),
        data_dir: dir.clone(),
        start_dir: dir.clone(),
        voice: Default::default(),
    });
    let armed = Arc::new(AtomicBool::new(true));
    let (entered_tx, entered_rx) = channel::<()>();
    let (release_tx, release_rx) = channel::<()>();
    let entered_tx = Mutex::new(Some(entered_tx));
    let release_rx = Mutex::new(release_rx);
    {
        let armed = armed.clone();
        let hook: Box<dyn Fn() + Send + Sync> = Box::new(move || {
            if armed.swap(false, Ordering::SeqCst) {
                if let Some(tx) = entered_tx.lock().unwrap().take() {
                    let _ = tx.send(());
                }
                let _ = release_rx.lock().unwrap().recv();
            }
        });
        assert!(s.before_data_write.set(hook).is_ok());
    }
    let saver = {
        let s = s.clone();
        thread::spawn(move || s.save_layout())
    };
    entered_rx
        .recv_timeout(Duration::from_secs(3))
        .expect("the save never began");
    let (closed_tx, closed_rx) = channel::<()>();
    let closer = {
        let s = s.clone();
        thread::spawn(move || {
            s.close();
            let _ = closed_tx.send(());
        })
    };
    let early = closed_rx.recv_timeout(Duration::from_millis(300)).is_ok();
    release_tx.send(()).unwrap();
    assert!(
        !early,
        "close returned while a save into the data dir was still in flight"
    );
    closed_rx
        .recv_timeout(Duration::from_secs(10))
        .expect("close never returned after the save finished");
    closer.join().unwrap();
    saver.join().unwrap();

    let layout = dir.join("layout.json");
    let _ = std::fs::remove_file(&layout);
    s.save_layout();
    assert!(!layout.exists(), "a closed server wrote layout.json again");
    let _ = std::fs::remove_dir_all(&dir);
}
