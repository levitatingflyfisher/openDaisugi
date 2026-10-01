//! The ring of recent state events the floor page reads after a reload:
//! the last two hours, at most 10,000 events, fed by the web server's own
//! subscription. It refuses to answer while that subscription is down, so
//! a gap never reads as a quiet hour.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::json;

use super::upstream::{self, Dialer};
use crate::godec::Any;

const RING_MAX: usize = 10000;
const RING_SPAN: f64 = 2.0 * 3600.0;
const RETRY_MAX: Duration = Duration::from_secs(30);

struct Inner {
    events: Vec<(f64, Vec<u8>)>,
    live: bool,
    from: f64,
}

pub struct EventRing {
    inner: Mutex<Inner>,
}

fn now() -> f64 {
    crate::server::Server::now_seconds()
}

impl EventRing {
    pub fn new() -> EventRing {
        EventRing {
            inner: Mutex::new(Inner {
                events: Vec::new(),
                live: false,
                from: 0.0,
            }),
        }
    }

    /// Keeps line when it is a state event with a time.
    pub fn add(&self, line: &[u8]) {
        let Ok(m) = crate::godec::decode_map(line) else {
            return;
        };
        if m.str_of("event") != "state" {
            return;
        }
        let Some(Any::F64(ts)) = m.get("ts") else {
            return;
        };
        let mut st = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        st.events.push((*ts, line.to_vec()));
        prune(&mut st);
    }

    pub fn from(&self) -> f64 {
        let mut st = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        prune(&mut st);
        st.from.max(now() - RING_SPAN)
    }

    /// The held events after since, oldest first.
    pub fn since(&self, since: f64) -> Vec<Vec<u8>> {
        let mut st = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        prune(&mut st);
        st.events
            .iter()
            .filter(|(ts, _)| *ts > since)
            .map(|(_, raw)| raw.clone())
            .collect()
    }

    pub fn live(&self) -> bool {
        self.inner.lock().unwrap_or_else(|e| e.into_inner()).live
    }

    fn set_live(&self, v: bool) {
        self.inner.lock().unwrap_or_else(|e| e.into_inner()).live = v;
    }

    fn subscribed(&self) {
        let mut st = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        st.live = true;
        st.from = now();
    }

    /// Feeds the ring until stop is set. A lost subscription is tried again
    /// after one second, then after twice the last wait, up to 30 s.
    pub fn run(self: Arc<Self>, d: Dialer, stop: Arc<AtomicBool>) {
        let first = Duration::from_secs(1);
        let mut wait = first;
        loop {
            let err = self.watch(&d, &stop, &mut wait, first);
            self.set_live(false);
            if stop.load(Ordering::SeqCst) {
                return;
            }
            super::log(
                "WARN",
                "web: the event ring lost its subscription",
                &[("err", &err)],
            );
            let until = Instant::now() + wait;
            while Instant::now() < until {
                if stop.load(Ordering::SeqCst) {
                    return;
                }
                std::thread::sleep(Duration::from_millis(50));
            }
            wait = (wait * 2).min(RETRY_MAX);
        }
    }

    fn watch(&self, d: &Dialer, stop: &AtomicBool, wait: &mut Duration, first: Duration) -> String {
        let s = match d.open() {
            Ok(s) => s,
            Err(e) => return e,
        };
        let sub = crate::gojson::marshal(&crate::gojson::map(vec![
            ("id", json!("ring-sub")),
            ("cmd", json!("events.subscribe")),
            ("panes", json!("*")),
            ("kinds", json!(["state"])),
        ]));
        if let Err(e) = s.send(sub.as_bytes()) {
            return e;
        }
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut subscribed = false;
        loop {
            if stop.load(Ordering::SeqCst) {
                return "context canceled".into();
            }
            if !subscribed && Instant::now() >= deadline {
                return "coppice-server did not answer events.subscribe in 5 s".into();
            }
            match s.lines.recv_timeout(Duration::from_millis(100)) {
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => continue,
                Err(_) => return "coppice-server closed the event stream".into(),
                Ok(line) => {
                    if !subscribed {
                        if let Ok(m) = crate::godec::decode_map(&line) {
                            if m.str_of("id") == "ring-sub" {
                                if !matches!(m.get("ok"), Some(Any::Bool(true))) {
                                    return upstream::refusal(&m).to_string();
                                }
                                subscribed = true;
                                self.subscribed();
                                *wait = first;
                                continue;
                            }
                        }
                    }
                    self.add(&line);
                }
            }
        }
    }
}

/// Drops events older than two hours and the oldest past the cap.
fn prune(st: &mut Inner) {
    let cut = now() - RING_SPAN;
    let mut drop = st.events.iter().take_while(|(ts, _)| *ts < cut).count();
    let mut capped = false;
    if st.events.len() > drop + RING_MAX {
        drop = st.events.len() - RING_MAX;
        capped = true;
    }
    if drop > 0 {
        st.events.drain(..drop);
    }
    if capped {
        if let Some((ts, _)) = st.events.first() {
            if *ts > st.from {
                st.from = *ts;
            }
        }
    }
}
