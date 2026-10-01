//! Keeps the tests from leaving temp dirs behind. Before main runs, the test
//! binary points TMPDIR at a new directory inside the one it started with.
//! Every `std::env::temp_dir().join(...)` in a test then lands in that
//! directory, and it is removed when the process exits. A test panic is
//! caught by the harness, so a failing run is cleaned too. An abort or a
//! kill is not. Many tests make a temp dir and never remove it (it is easy to
//! forget on the failing path); this makes that harmless.
//!
//! Set COPPICE_TEST_KEEP_TMPDIR=1 to leave TMPDIR alone, so a script can
//! see what the tests leave. Integration tests (tests/*.rs) include this file with
//! `#[path = "../src/testtmp.rs"] mod testtmp;`.
//!
//! It runs from .init_array, so it is in place before the test harness
//! starts a thread.

use std::ffi::{CStr, CString};
use std::sync::atomic::{AtomicPtr, AtomicU32, Ordering};

static DIR: AtomicPtr<libc::c_char> = AtomicPtr::new(std::ptr::null_mut());
static OWNER: AtomicU32 = AtomicU32::new(0);

extern "C" fn remove_dir() {
    // A forked child that calls exit runs this too. Only the process that
    // made the directory removes it.
    if std::process::id() != OWNER.load(Ordering::SeqCst) {
        return;
    }
    let p = DIR.load(Ordering::SeqCst);
    if !p.is_null() {
        // SAFETY: p came from CString::into_raw in isolate and is never freed.
        let path = unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned();
        let _ = std::fs::remove_dir_all(path);
    }
}

extern "C" fn isolate() {
    if std::env::var_os("COPPICE_TEST_KEEP_TMPDIR").is_some() {
        return;
    }
    let dir = std::env::temp_dir().join(format!("dtt-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    if std::fs::create_dir_all(&dir).is_err() {
        // No private dir: leave TMPDIR as it is rather than fail every test.
        return;
    }
    if let Ok(c) = CString::new(dir.to_string_lossy().into_owned()) {
        DIR.store(c.into_raw(), Ordering::SeqCst);
    }
    OWNER.store(std::process::id(), Ordering::SeqCst);
    std::env::set_var("TMPDIR", &dir);
    // SAFETY: remove_dir is a plain extern "C" fn with no arguments.
    unsafe {
        libc::atexit(remove_dir);
    }
}

#[used]
#[link_section = ".init_array"]
static ISOLATE: extern "C" fn() = isolate;
