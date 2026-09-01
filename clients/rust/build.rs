//! Sets DAISUGI_VERSION, the version `daisugi --version` prints, and links
//! the C++ standard library that the statically built Z3 needs.
//!
//! Z3 is built from the pinned source in the z3-src crate. A box with no
//! system C++ compiler builds it with `zig c++`, which compiles against
//! LLVM's libc++, so the binary must link libc++ and libc++abi rather
//! than GNU libstdc++. `tools/z3-build-env.sh` sets DAISUGI_LIBCXX_DIR to a
//! directory holding those two static archives, and CXXSTDLIB to empty so
//! z3-sys does not ask for libstdc++. With DAISUGI_LIBCXX_DIR unset, the
//! build leaves the choice to z3-sys (libstdc++ from a system g++).

use std::process::Command;

fn git(args: &[&str]) -> Option<String> {
    let out = Command::new("git").args(args).output().ok()?;
    if !out.status.success() {
        return None;
    }
    let s = String::from_utf8(out.stdout).ok()?.trim().to_string();
    (!s.is_empty()).then_some(s)
}

/// The version `daisugi --version` prints: DAISUGI_VERSION when set, else
/// `git describe --tags --always --dirty`, else "unknown". A new commit or
/// checkout moves HEAD's log, which makes cargo run this again.
fn version() -> String {
    println!("cargo:rerun-if-env-changed=DAISUGI_VERSION");
    if let Ok(v) = std::env::var("DAISUGI_VERSION") {
        if !v.is_empty() {
            return v;
        }
    }
    if let Some(log) = git(&["rev-parse", "--path-format=absolute", "--git-path", "logs/HEAD"]) {
        println!("cargo:rerun-if-changed={log}");
    }
    git(&["describe", "--tags", "--always", "--dirty"]).unwrap_or_else(|| "unknown".into())
}

fn main() {
    println!("cargo:rustc-env=DAISUGI_VERSION={}", version());
    println!("cargo:rerun-if-env-changed=DAISUGI_LIBCXX_DIR");
    if let Ok(dir) = std::env::var("DAISUGI_LIBCXX_DIR") {
        if !dir.is_empty() {
            println!("cargo:rustc-link-search=native={dir}");
            println!("cargo:rustc-link-lib=static=c++");
            println!("cargo:rustc-link-lib=static=c++abi");
        }
    }
}
