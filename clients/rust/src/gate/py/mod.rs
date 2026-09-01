//! Python's own rules for text and errors, as the Python gate meets them:
//! the Unicode character classes of `str` and `re`, `repr`, `lower` and
//! `casefold`, lone surrogates, and exceptions carried by their `str(exc)`.

pub mod err;
pub mod re;
pub mod tables;
pub mod text;

pub use err::{PyErr, PyResult};
