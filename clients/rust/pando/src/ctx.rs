//! A small stand-in for Go's context: a call can be cancelled, or given a
//! deadline, and a child ends when its parent does.

use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

/// Why a context ended.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CtxErr {
    Canceled,
    DeadlineExceeded,
}

impl CtxErr {
    /// Go's text for the error.
    pub fn text(self) -> &'static str {
        match self {
            CtxErr::Canceled => "context canceled",
            CtxErr::DeadlineExceeded => "context deadline exceeded",
        }
    }
}

/// Every cancel wakes the sleepers; each one then looks at its own context.
static WAKE: (Mutex<()>, Condvar) = (Mutex::new(()), Condvar::new());

struct Inner {
    parent: Option<Ctx>,
    deadline: Option<Instant>,
    /// 0 live, 1 cancelled.
    state: AtomicU8,
}

/// One context. Clones share it.
#[derive(Clone)]
pub struct Ctx(Arc<Inner>);

impl Default for Ctx {
    fn default() -> Self {
        Ctx::background()
    }
}

impl Ctx {
    /// A context that never ends unless it is cancelled.
    pub fn background() -> Ctx {
        Ctx(Arc::new(Inner {
            parent: None,
            deadline: None,
            state: AtomicU8::new(0),
        }))
    }

    /// A child that ends with this one, or when it is cancelled.
    pub fn child(&self) -> Ctx {
        Ctx(Arc::new(Inner {
            parent: Some(self.clone()),
            deadline: None,
            state: AtomicU8::new(0),
        }))
    }

    /// A child that also ends after d.
    pub fn with_timeout(&self, d: Duration) -> Ctx {
        Ctx(Arc::new(Inner {
            parent: Some(self.clone()),
            deadline: Some(Instant::now() + d),
            state: AtomicU8::new(0),
        }))
    }

    /// Ends this context and every child of it.
    pub fn cancel(&self) {
        self.0.state.store(1, Ordering::SeqCst);
        // Taking the lock first means a sleeper that has checked but not yet
        // waited cannot miss this wake.
        let _g = WAKE.0.lock().unwrap_or_else(|e| e.into_inner());
        WAKE.1.notify_all();
    }

    /// The earliest deadline of this context and its parents.
    fn first_deadline(&self) -> Option<Instant> {
        let own = self.0.deadline;
        let up = self.0.parent.as_ref().and_then(|p| p.first_deadline());
        match (own, up) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        }
    }

    /// Why the context ended, or None while it is live. A cancel wins over
    /// a deadline that passed after it.
    pub fn err(&self) -> Option<CtxErr> {
        if self.0.state.load(Ordering::SeqCst) == 1 {
            return Some(CtxErr::Canceled);
        }
        if let Some(p) = &self.0.parent {
            if let Some(e) = p.err() {
                return Some(e);
            }
        }
        if let Some(d) = self.0.deadline {
            if Instant::now() >= d {
                return Some(CtxErr::DeadlineExceeded);
            }
        }
        None
    }

    /// Whether this context's own deadline ended it (and not its parent).
    pub fn own_deadline_passed(&self) -> bool {
        self.err() == Some(CtxErr::DeadlineExceeded)
            && self.0.parent.as_ref().is_none_or(|p| p.err().is_none())
    }

    /// Sleeps d, or less when the context ends first. True when the context
    /// ended.
    pub fn sleep(&self, d: Duration) -> bool {
        let mut end = Instant::now() + d;
        if let Some(dl) = self.first_deadline() {
            end = end.min(dl);
        }
        let mut g = WAKE.0.lock().unwrap_or_else(|e| e.into_inner());
        loop {
            if self.err().is_some() {
                return true;
            }
            let now = Instant::now();
            if now >= end {
                return false;
            }
            g = WAKE.1.wait_timeout(g, end - now).unwrap_or_else(|e| e.into_inner()).0;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_cancel_wakes_a_long_sleep_at_once() {
        let ctx = Ctx::background();
        let c2 = ctx.clone();
        let h = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(50));
            c2.cancel();
        });
        let t = Instant::now();
        assert!(ctx.child().sleep(Duration::from_secs(3600)));
        assert!(t.elapsed() < Duration::from_secs(5));
        h.join().unwrap();
    }

    #[test]
    fn a_sleep_ends_at_its_own_deadline_and_a_short_sleep_ends_false() {
        let ctx = Ctx::background();
        assert!(ctx.with_timeout(Duration::from_millis(30)).sleep(Duration::from_secs(3600)));
        assert!(!ctx.sleep(Duration::from_millis(10)));
    }
}
