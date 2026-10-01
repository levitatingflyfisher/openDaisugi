//! `opendaisugi.pack`: the ML packs (a pinned CPython, a venv, the wheels
//! of a hashed lock) under DATA/packs/NAME, and the caller side of the
//! worker protocol daisugi-pack-1, which `src/opendaisugi/pack/worker.py`
//! defines. The Go client's `internal/pack` is the other port.

pub mod catalog;
pub mod client;
pub mod manage;
pub mod ustar;

pub use catalog::{Catalog, Pack, Req};
pub use client::{run_job, Outcome};
pub use manage::Ctx;

/// `worker.PROTOCOL`.
pub const PROTOCOL: &str = "daisugi-pack-1";
/// The worker's name in a pack.
pub const WORKER_FILE: &str = "daisugi_pack_worker.py";
/// The catalog the command line reads from the environment.
pub const CATALOG_ENV: &str = "OPENDAISUGI_PACK_CATALOG";

pub const WORKER_PY: &str = include_str!("../../../../src/opendaisugi/pack/worker.py");
pub const LORA_TRAIN_PY: &str = include_str!("../../../../src/opendaisugi/lora/train.py");
pub const VLA_ORACLE_PY: &str = include_str!("../../../../src/opendaisugi/pack/vla_oracle.py");
pub const CATALOG_JSON: &str = include_str!("../../../../packs/catalog.json");
pub const TRAIN_LOCK: &str = include_str!("../../../../packs/train.lock");
pub const VLA_REF_LOCK: &str = include_str!("../../../../packs/vla-ref.lock");

#[cfg(test)]
mod tests;
