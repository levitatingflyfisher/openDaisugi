//! Keeps the tests out of the operator's home. Before main runs, the test
//! binary points HOME and every XDG directory into one scratch directory,
//! as Go's internal/testhome does from TestMain, so no test reads, writes
//! or dials anything of the operator's: their coppice.toml, their gate
//! hooks, their running server's socket, their foreman directory.
//!
//! It runs from .init_array, so it is in place before the test harness
//! starts any thread, and every test sees it whatever its name or module.

use std::path::{Path, PathBuf};
use std::sync::OnceLock;

static REAL_HOME: OnceLock<String> = OnceLock::new();
static DIR: OnceLock<PathBuf> = OnceLock::new();

/// The directory OPENDAISUGI_HOME names during the tests. Nothing may be
/// written there: a test that falls back to a default data home lands in
/// it, and the run fails at exit.
pub fn canary() -> PathBuf {
    dir().join("canary")
}

/// Every path under the canary.
pub fn canary_written() -> Vec<PathBuf> {
    fn walk(d: &Path, out: &mut Vec<PathBuf>) {
        if let Ok(rd) = std::fs::read_dir(d) {
            for e in rd.flatten() {
                out.push(e.path());
                walk(&e.path(), out);
            }
        }
    }
    let mut out = Vec::new();
    walk(&canary(), &mut out);
    out
}

/// The home the process had before isolation.
pub fn real_home() -> &'static str {
    REAL_HOME.get().map(|s| s.as_str()).unwrap_or("")
}

/// The scratch directory every test home lives in.
pub fn dir() -> &'static Path {
    DIR.get().map(|p| p.as_path()).unwrap_or(Path::new(""))
}

const VARS: [(&str, &str); 7] = [
    ("HOME", "home"),
    ("XDG_CONFIG_HOME", "config"),
    ("XDG_DATA_HOME", "data"),
    ("XDG_STATE_HOME", "state"),
    ("XDG_CACHE_HOME", "cache"),
    ("XDG_BIN_HOME", "bin"),
    ("XDG_RUNTIME_DIR", "run"),
];

extern "C" fn remove_dir() {
    let written = canary_written();
    if let Some(d) = DIR.get() {
        let _ = std::fs::remove_dir_all(d);
    }
    if !written.is_empty() {
        eprintln!("a test wrote under OPENDAISUGI_HOME: {written:?}");
        // SAFETY: _exit ends the process at once with a failing status.
        unsafe { libc::_exit(1) };
    }
}

extern "C" fn isolate() {
    let _ = REAL_HOME.set(std::env::var("HOME").unwrap_or_default());
    let base = std::env::temp_dir().join(format!("coppice-rs-test-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&base);
    let xdg: Vec<_> = std::env::vars_os()
        .filter(|(k, _)| k.to_string_lossy().starts_with("XDG_"))
        .map(|(k, _)| k)
        .collect();
    for k in xdg {
        std::env::remove_var(k);
    }
    for k in ["OPENDAISUGI_HOME", "COPPICE_DATA_DIR", "CLAUDE_CONFIG_DIR"] {
        std::env::remove_var(k);
    }
    for (k, sub) in VARS {
        let p = base.join(sub);
        if std::fs::create_dir_all(&p).is_err() {
            // A test home that cannot be made must not fall back to the
            // real one.
            std::process::abort();
        }
        std::env::set_var(k, &p);
    }
    if std::fs::create_dir_all(base.join("canary")).is_err() {
        std::process::abort();
    }
    std::env::set_var("OPENDAISUGI_HOME", base.join("canary"));
    let _ = DIR.set(base);
    // SAFETY: remove_dir is a plain extern "C" fn with no arguments.
    unsafe {
        libc::atexit(remove_dir);
    }
}

#[used]
#[link_section = ".init_array"]
static ISOLATE: extern "C" fn() = isolate;

#[cfg(test)]
mod tests {
    use super::*;

    fn inside(p: &Path) -> bool {
        p.starts_with(dir())
    }

    /// No test resolves a path under the real home: HOME, the XDG dirs, and
    /// every default the floor falls back to are in the test home.
    #[test]
    fn no_test_resolves_a_path_under_the_real_home() {
        assert!(!real_home().is_empty(), "the real home was not recorded");
        assert!(!dir().as_os_str().is_empty(), "the test home was not made");
        assert_ne!(Path::new(real_home()), dir());
        for (k, _) in VARS {
            let v = PathBuf::from(std::env::var(k).unwrap());
            assert!(inside(&v), "{k} is {}", v.display());
        }
        assert_eq!(
            std::env::var_os("OPENDAISUGI_HOME").map(PathBuf::from),
            Some(canary())
        );
        for k in ["COPPICE_DATA_DIR", "CLAUDE_CONFIG_DIR"] {
            assert!(std::env::var_os(k).is_none(), "{k} is still set");
        }
        let paths = [
            ("coppice.toml", crate::config::path()),
            ("socket", crate::cli::socket_path()),
            ("data", crate::cli::data_dir()),
            (
                "foreman",
                PathBuf::from(crate::server::foreman_dir_for_tests()),
            ),
        ];
        for (name, p) in paths {
            assert!(
                inside(&p),
                "{name} {} is outside the test home",
                p.display()
            );
        }
    }
}
