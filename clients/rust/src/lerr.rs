//! How a stage-L operation (the registry, batch proofs, release signing,
//! the deed ledger, the strata store) stops: an exception the oracle
//! raises, a pydantic error, an OS error, or input this binary does not
//! read the way the oracle does.

use crate::pathways::pmodel::ValidationError;
use crate::pathways::PwErr;

/// An exception the oracle raises: its qualified type name, as a
/// traceback's last line names it, and its message.
#[derive(Debug, Clone, PartialEq)]
pub struct PyError {
    pub typ: String,
    pub msg: String,
    /// The exception being handled when this one was raised ("Type:
    /// msg"), which a traceback names first.
    pub context: String,
}

impl PyError {
    pub fn new(typ: impl Into<String>, msg: impl Into<String>) -> PyError {
        PyError {
            typ: typ.into(),
            msg: msg.into(),
            context: String::new(),
        }
    }

    /// Whether Python's `except (ValueError, TypeError)` catches it:
    /// binascii.Error is a ValueError.
    pub fn is_value_error(&self) -> bool {
        matches!(
            self.typ.as_str(),
            "ValueError" | "binascii.Error" | "TypeError"
        )
    }
}

impl std::fmt::Display for PyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if !self.context.is_empty() {
            write!(f, "{}; during its handling, ", self.context)?;
        }
        f.write_str(&self.typ)?;
        if !self.msg.is_empty() {
            write!(f, ": {}", self.msg)?;
        }
        Ok(())
    }
}

/// Why a stage-L operation stopped.
#[derive(Debug)]
pub enum LErr {
    /// The oracle raises this exception.
    Py(PyError),
    /// pydantic's ValidationError.
    Invalid(ValidationError),
    /// A pathway store or journal error.
    Pw(PwErr),
    /// An OS error on a path, which Python raises as an OSError.
    Io(std::io::Error, String),
    /// Input this binary does not read the way the oracle does: the
    /// command refuses it with nothing changed.
    Unread(String),
}

impl From<PyError> for LErr {
    fn from(e: PyError) -> LErr {
        LErr::Py(e)
    }
}

impl From<ValidationError> for LErr {
    fn from(e: ValidationError) -> LErr {
        LErr::Invalid(e)
    }
}

impl From<PwErr> for LErr {
    fn from(e: PwErr) -> LErr {
        match e {
            PwErr::Unreadable(w) => LErr::Unread(w),
            other => LErr::Pw(other),
        }
    }
}

impl LErr {
    /// An OS error on `path`.
    pub fn io(e: std::io::Error, path: &str) -> LErr {
        LErr::Io(e, path.to_string())
    }

    /// The exception's type and text, as the probe prints them.
    pub fn py(&self) -> (String, String) {
        match self {
            LErr::Py(p) => (p.typ.clone(), p.msg.clone()),
            LErr::Invalid(_) => (
                "pydantic_core._pydantic_core.ValidationError".into(),
                String::new(),
            ),
            LErr::Pw(PwErr::Invalid(s)) => match s.split_once(": ") {
                Some((t, m)) => (t.to_string(), m.to_string()),
                None => (s.clone(), String::new()),
            },
            LErr::Pw(other) => ("Unported".into(), other.to_string()),
            LErr::Io(e, path) => match crate::supervise::executors::os_error(e, path) {
                Some((name, text)) => (name.to_string(), text),
                None => ("OSError".into(), e.to_string()),
            },
            LErr::Unread(w) => ("Unread".into(), w.clone()),
        }
    }
}

pub type LR<T> = Result<T, LErr>;
