//! A Python exception, as far as the gate reads one: its class name and
//! its `str(exc)`. The Python gate catches exceptions at fixed places and
//! writes `str(exc)` into a deny reason, so the port raises the same text
//! where Python would raise, and catches it where Python catches it.

/// A raised exception.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PyErr {
    /// The exception class, such as `ValueError` or `RecursionError`.
    pub kind: &'static str,
    /// `str(exc)`.
    pub msg: String,
}

pub type PyResult<T> = Result<T, PyErr>;

impl PyErr {
    pub fn new(kind: &'static str, msg: impl Into<String>) -> Self {
        PyErr { kind, msg: msg.into() }
    }
    pub fn value(msg: impl Into<String>) -> Self {
        PyErr::new("ValueError", msg)
    }
    pub fn type_err(msg: impl Into<String>) -> Self {
        PyErr::new("TypeError", msg)
    }
    pub fn attribute(msg: impl Into<String>) -> Self {
        PyErr::new("AttributeError", msg)
    }
    pub fn recursion() -> Self {
        PyErr::new("RecursionError", "maximum recursion depth exceeded")
    }
    pub fn os(msg: impl Into<String>) -> Self {
        PyErr::new("OSError", msg)
    }
    /// A case the port does not decide, raised from code that knows only
    /// Python's rules. The gate denies it with a reason.
    pub fn undecided(what: impl Into<String>) -> Self {
        PyErr::new("Undecided", what)
    }
    pub fn is(&self, kind: &str) -> bool {
        self.kind == kind
    }
}

impl std::fmt::Display for PyErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.msg)
    }
}

/// Raise `err` from inside a closure that returns a plain value.
pub fn raise<T>(err: PyErr) -> PyResult<T> {
    Err(err)
}
