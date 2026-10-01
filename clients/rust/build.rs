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
    if let Some(log) = git(&[
        "rev-parse",
        "--path-format=absolute",
        "--git-path",
        "logs/HEAD",
    ]) {
        println!("cargo:rerun-if-changed={log}");
    }
    git(&["describe", "--tags", "--always", "--dirty"]).unwrap_or_else(|| "unknown".into())
}

/// With the mujoco feature: compile the C layer the Go and Rust MuJoCo
/// wrappers share, and link libmujoco.so and libEGL. The MuJoCo prefix is
/// DAISUGI_MUJOCO_PREFIX, else the clients/go/.mujoco link that
/// `clients/go/scripts/native.sh --mujoco` makes. No rpath is written: the
/// loader finds libmujoco.so through LD_LIBRARY_PATH.
#[cfg(feature = "mujoco")]
fn mujoco() {
    use std::path::PathBuf;
    println!("cargo:rerun-if-env-changed=DAISUGI_MUJOCO_PREFIX");
    let manifest = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    let prefix = match std::env::var("DAISUGI_MUJOCO_PREFIX") {
        Ok(p) if !p.is_empty() => PathBuf::from(p),
        _ => manifest.join("../go/.mujoco"),
    };
    let include = prefix.join("include");
    if !include.join("mujoco/mujoco.h").exists() {
        panic!(
            "no MuJoCo headers under {}: run clients/go/scripts/native.sh --mujoco",
            include.display()
        );
    }
    let shim = manifest.join("../native/mujoco");
    println!("cargo:rerun-if-changed={}", shim.join("dmj.c").display());
    println!("cargo:rerun-if-changed={}", shim.join("dmj.h").display());
    cc::Build::new()
        .file(shim.join("dmj.c"))
        .include(&include)
        .include(&shim)
        .compile("dmj");
    println!(
        "cargo:rustc-link-search=native={}",
        prefix.join("lib").display()
    );
    println!("cargo:rustc-link-lib=dylib=mujoco");
    println!("cargo:rustc-link-lib=dylib=EGL");
    onnxruntime();
}

/// With the mujoco feature (the robotics build option): compile the C
/// layer the Go and Rust ONNX Runtime wrappers share, and link
/// libonnxruntime.so. The prefix is DAISUGI_ONNXRUNTIME_PREFIX, else the
/// clients/go/.onnxruntime link that `clients/go/scripts/native.sh
/// --onnxruntime` makes. No rpath is written: the loader finds the library
/// through LD_LIBRARY_PATH.
#[cfg(feature = "mujoco")]
fn onnxruntime() {
    use std::path::PathBuf;
    println!("cargo:rerun-if-env-changed=DAISUGI_ONNXRUNTIME_PREFIX");
    let manifest = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    let prefix = match std::env::var("DAISUGI_ONNXRUNTIME_PREFIX") {
        Ok(p) if !p.is_empty() => PathBuf::from(p),
        _ => manifest.join("../go/.onnxruntime"),
    };
    let include = prefix.join("include");
    if !include.join("onnxruntime_c_api.h").exists() {
        panic!(
            "no ONNX Runtime headers under {}: run clients/go/scripts/native.sh --onnxruntime",
            include.display()
        );
    }
    let shim = manifest.join("../native/ort");
    println!("cargo:rerun-if-changed={}", shim.join("dort.c").display());
    println!("cargo:rerun-if-changed={}", shim.join("dort.h").display());
    cc::Build::new()
        .file(shim.join("dort.c"))
        .include(&include)
        .include(&shim)
        .compile("dort");
    println!(
        "cargo:rustc-link-search=native={}",
        prefix.join("lib").display()
    );
    println!("cargo:rustc-link-lib=dylib=onnxruntime");
}

fn main() {
    #[cfg(feature = "mujoco")]
    mujoco();
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
