//! A small stand-in for Go's context: a call can be cancelled, or given a
//! deadline, and a child ends when its parent does.

use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::Arc;
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
        let end = Instant::now() + d;
        loop {
            if self.err().is_some() {
                return true;
            }
            let now = Instant::now();
            if now >= end {
                return false;
            }
            std::thread::sleep((end - now).min(Duration::from_millis(20)));
        }
    }
}
