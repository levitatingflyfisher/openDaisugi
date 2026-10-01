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

use std::cell::RefCell;

/// Removes a directory when it is dropped.
struct Guard(PathBuf);

impl Drop for Guard {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

thread_local! {
    static GUARDS: RefCell<Vec<Guard>> = const { RefCell::new(Vec::new()) };
}

/// Marks `p` to be removed, with everything in it, when the current test
/// thread ends. That is when the test ends, on a panic too, because the
/// harness runs each test on a thread of its own. It returns `p`, so a
/// test writes `let dir = guard(std::env::temp_dir().join("x"));`.
pub fn guard(p: PathBuf) -> PathBuf {
    GUARDS.with(|g| g.borrow_mut().push(Guard(p.clone())));
    p
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
    let base = std::env::temp_dir().join(format!("crs-{}", std::process::id()));
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
    // COPPICE_TEST_KEEP_TMPDIR=1 leaves TMPDIR as the run started with it,
    // so scripts/check-test-tmp.sh can see what the tests leave. Without it,
    // TMPDIR is the scratch directory itself. A test that makes a temp dir
    // and never removes it (many do: std::env::temp_dir().join(...) with no
    // guard) now makes it in here, and it goes away at exit with the rest, on
    // a failing run too (a panic is caught by the harness; an abort or a kill
    // is not). A test should still clean up after itself, with `guard` or a
    // `TempDir`: the sweep at exit is the last net, not the plan. The path
    // stays short on purpose: a foreman socket
    // path may not reach 100 bytes. scripts/check-test-tmp.sh checks that
    // nothing is left in the TMPDIR the run started with.
    if std::env::var_os("COPPICE_TEST_KEEP_TMPDIR").is_none() {
        std::env::set_var("TMPDIR", &base);
    }
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
