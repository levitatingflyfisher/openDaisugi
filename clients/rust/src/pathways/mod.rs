//! The pathway store and the matchers (`daisugi pathways ...` and
//! `find`), ported from the Python oracle: `pathway_store.py`,
//! `pathway.py`, `portability.py` and `_search.py`. The binary reads and
//! writes the oracle's SQLite file. The Go client's `internal/pathways`
//! is the reference.

pub mod blake2b;
pub mod dumped;
pub mod export;
pub mod find;
pub mod importer;
pub mod literal;
pub mod lexical;
pub mod pathway;
pub mod pmodel;
pub mod potion;
pub mod scan;
pub mod store;
pub mod verify;
pub mod yamldump;

/// Why a pathway command stopped.
#[derive(Debug)]
pub enum PwErr {
    /// Input this binary cannot read the way Python does: the command
    /// refuses with nothing changed.
    Unreadable(String),
    /// Python raises: "<exception type>: <message>".
    Invalid(String),
    /// `portability.PathwayImportError`: a code and a message.
    Import(String, String),
    /// An OS error, named as Python names it.
    Io(std::io::Error),
    /// A part of a command this binary does not carry.
    NotYet(String),
}

impl PwErr {
    pub fn import(code: &str, msg: impl Into<String>) -> PwErr {
        PwErr::Import(code.into(), msg.into())
    }
}

impl std::fmt::Display for PwErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PwErr::Unreadable(s) | PwErr::Invalid(s) | PwErr::NotYet(s) => f.write_str(s),
            PwErr::Import(code, msg) => write!(f, "[{code}] {msg}"),
            PwErr::Io(e) => write!(f, "{e}"),
        }
    }
}
