//! Writes layout.json once per burst of changes. Every site that changes the
//! tree calls `mark_layout_dirty`. The write happens `settle` after the LAST
//! change (each change moves the deadline), and at once on close. With no
//! change there is no write. A settle of zero writes at once on every
//! change, which the tests use.

use std::sync::{Arc, Weak};
use std::time::{Duration, Instant};

use super::Server;

#[derive(Default)]
pub struct LayoutSave {
    settle: Duration,
    dirty: bool,
    deadline: Option<Instant>,
    waiting: bool,
    /// Set by the first flush (close). From then on a change is written at
    /// once, since a waiter would wake after the data dir shuts.
    closing: bool,
}

impl Server {
    /// Sets the wait after the last layout change. Zero writes at once.
    pub fn set_layout_settle(&self, settle: Duration) {
        self.layout_save
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .settle = settle;
    }

    /// Says the tree changed and must reach disk.
    pub fn mark_layout_dirty(&self) {
        let spawn = {
            let mut ls = self.layout_save.lock().unwrap_or_else(|e| e.into_inner());
            if ls.settle.is_zero() || ls.closing {
                None
            } else {
                ls.dirty = true;
                ls.deadline = Some(Instant::now() + ls.settle);
                let first = !ls.waiting;
                ls.waiting = true;
                Some(first)
            }
        };
        match spawn {
            None => self.save_layout(),
            Some(true) => {
                let me = self.me.clone();
                std::thread::spawn(move || wait_and_save(me));
            }
            Some(false) => {}
        }
    }

    /// Writes the layout now if a change is waiting, and makes every later
    /// change write at once. Close calls it. The lock is not held
    /// across the write.
    pub(super) fn flush_layout(&self) {
        let was = {
            let mut ls = self.layout_save.lock().unwrap_or_else(|e| e.into_inner());
            ls.deadline = None;
            ls.closing = true;
            std::mem::take(&mut ls.dirty)
        };
        if was {
            self.save_layout();
        }
    }

    /// Writes the layout. Best effort: a layout write failure must never
    /// fail a command that already succeeded in the world.
    pub fn save_layout(&self) {
        self.write_data(|| {
            let _ = self.tree.save(&self.cfg.data_dir.join("layout.json"));
        });
    }
}

/// Sleeps until the deadline stops moving, then writes if still dirty. It
/// holds no strong reference while it sleeps.
fn wait_and_save(me: Weak<Server>) {
    loop {
        let Some(s) = me.upgrade() else { return };
        let wait = {
            let mut ls = s.layout_save.lock().unwrap_or_else(|e| e.into_inner());
            match ls.deadline {
                Some(d) if Instant::now() < d => Some(d - Instant::now()),
                _ => {
                    ls.waiting = false;
                    ls.deadline = None;
                    None
                }
            }
        };
        match wait {
            Some(d) => {
                drop(s);
                std::thread::sleep(d);
            }
            None => {
                let was = {
                    let mut ls = s.layout_save.lock().unwrap_or_else(|e| e.into_inner());
                    std::mem::take(&mut ls.dirty)
                };
                if was {
                    s.save_layout();
                }
                return;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::server::Config;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn counted(name: &str, settle: Duration) -> (Arc<Server>, Arc<AtomicUsize>) {
        let dir = std::env::temp_dir().join(format!("coppice-rs-ls-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let s = Server::new(Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.clone(),
            start_dir: dir,
            voice: Default::default(),
            foreman: Default::default(),
        });
        s.set_layout_settle(settle);
        let n = Arc::new(AtomicUsize::new(0));
        let c = n.clone();
        let hook: Box<dyn Fn() + Send + Sync> = Box::new(move || {
            c.fetch_add(1, Ordering::SeqCst);
        });
        assert!(s.before_data_write.set(hook).is_ok());
        (s, n)
    }

    #[test]
    fn a_burst_of_changes_gives_one_write_after_settle() {
        let (s, n) = counted("burst", Duration::from_millis(80));
        for _ in 0..5 {
            s.mark_layout_dirty();
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(n.load(Ordering::SeqCst), 0, "wrote before the settle time");
        let end = Instant::now() + Duration::from_secs(3);
        while n.load(Ordering::SeqCst) == 0 && Instant::now() < end {
            std::thread::sleep(Duration::from_millis(10));
        }
        std::thread::sleep(Duration::from_millis(200));
        assert_eq!(n.load(Ordering::SeqCst), 1);
        s.close();
    }

    #[test]
    fn close_writes_a_pending_save_before_the_slow_steps() {
        let (s, n) = counted("top", Duration::from_secs(3600));
        s.turns_wg.add();
        s.mark_layout_dirty();
        let c = s.clone();
        let h = std::thread::spawn(move || c.close());
        let end = Instant::now() + Duration::from_secs(5);
        while n.load(Ordering::SeqCst) == 0 && Instant::now() < end {
            std::thread::sleep(Duration::from_millis(10));
        }
        let at_wait = n.load(Ordering::SeqCst);
        s.turns_wg.done();
        h.join().unwrap();
        assert_eq!(at_wait, 1, "no write while close waited on the turns");
    }

    #[test]
    fn a_change_after_the_flush_writes_at_once() {
        let (s, n) = counted("after", Duration::from_secs(3600));
        s.flush_layout();
        s.mark_layout_dirty();
        assert_eq!(n.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn close_flushes_a_pending_save() {
        let (s, n) = counted("flush", Duration::from_secs(3600));
        s.mark_layout_dirty();
        assert_eq!(n.load(Ordering::SeqCst), 0);
        s.close();
        assert_eq!(n.load(Ordering::SeqCst), 1);
    }
}
