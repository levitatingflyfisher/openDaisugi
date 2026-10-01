//! Links the pinned libghostty-vt and the small C layer over it.
//!
//! COPPICE_GHOSTTY_PREFIX names the prefix: the directory with
//! include/ghostty and lib/libghostty-vt.a. clients/go/scripts/native.sh
//! --print-prefix names the one this repo builds; the coppice CI job and
//! scripts/toolchain.sh build the same ghostty commit. The C layer is
//! compiled with the system C compiler (CC, else cc) and archived with ar,
//! so no build crate is needed.
//!
//! It also writes the list of the shipped plugins' files, which the binary
//! carries as Go's embed does: each directory the Go embed line names, with
//! every name that starts with . or _ under it left out.
//!
//! It also writes the list of the phone page's files, which the binary
//! carries from harness/coppice/internal/web/static as Go's embed does.
//!
//! It also sets COPPICE_VERSION, what `coppice --version` prints:
//! COPPICE_VERSION when set, else `git describe --tags --always`.

use std::env;
use std::path::PathBuf;
use std::process::Command;

fn run(cmd: &mut Command) {
    let status = cmd
        .status()
        .unwrap_or_else(|e| panic!("cannot run {cmd:?}: {e}"));
    if !status.success() {
        panic!("{cmd:?} failed with {status}");
    }
}

fn version() -> String {
    println!("cargo:rerun-if-env-changed=COPPICE_VERSION");
    if let Ok(v) = env::var("COPPICE_VERSION") {
        if !v.is_empty() {
            return v;
        }
    }
    let out = Command::new("git")
        .args(["describe", "--tags", "--always"])
        .output();
    match out {
        Ok(o) if o.status.success() => {
            let s = String::from_utf8_lossy(&o.stdout).trim().to_string();
            if s.is_empty() {
                "unknown".into()
            } else {
                s
            }
        }
        _ => "unknown".into(),
    }
}

/// The directories harness/coppice/plugins/embed.go names.
const SHIPPED: &[&str] = &[
    "_lib",
    "tree",
    "minimap",
    "kanban",
    "colony",
    "shift-log",
    "herdr-grid",
    "inbox",
    "merge-on-green",
    "close-quiet",
    "turn-budget",
    "notify-ntfy",
    "notify-lockscreen",
    "notify-voice",
];

fn walk(root: &std::path::Path, rel: &str, out: &mut Vec<String>) {
    let dir = root.join(rel);
    println!("cargo:rerun-if-changed={}", dir.display());
    let mut names: Vec<String> = std::fs::read_dir(&dir)
        .unwrap_or_else(|e| panic!("cannot read {}: {e}", dir.display()))
        .map(|e| {
            e.expect("dir entry")
                .file_name()
                .to_string_lossy()
                .into_owned()
        })
        .collect();
    names.sort();
    for n in names {
        if n.starts_with('.') || n.starts_with('_') {
            continue;
        }
        let r = format!("{rel}/{n}");
        let p = root.join(&r);
        let md = std::fs::symlink_metadata(&p).expect("stat");
        if md.is_dir() {
            walk(root, &r, out);
        } else if md.is_file() {
            println!("cargo:rerun-if-changed={}", p.display());
            out.push(r);
        }
    }
}

fn shipped_plugins(out_dir: &std::path::Path) {
    let root = PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("manifest dir"))
        .join("..")
        .join("coppice")
        .join("plugins");
    let root = root.canonicalize().expect("the Go plugins directory");
    let mut files = Vec::new();
    for d in SHIPPED {
        walk(&root, d, &mut files);
    }
    let mut src = String::from("/// The shipped plugins' files: each path and its bytes.\npub static FILES: &[(&str, &[u8])] = &[\n");
    for f in &files {
        src.push_str(&format!(
            "    ({f:?}, include_bytes!({:?})),\n",
            root.join(f).display().to_string()
        ));
    }
    src.push_str("];\n");
    std::fs::write(out_dir.join("shipped.rs"), src).expect("write shipped.rs");
}

/// The phone page, from the same directory Go's `//go:embed static`
/// names, with every name that starts with . or _ left out at any depth,
/// as the embed leaves them out. One copy of the page: the Go tree's.
fn web_static(out_dir: &std::path::Path) {
    let root = PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("manifest dir"))
        .join("..")
        .join("coppice")
        .join("internal")
        .join("web");
    let root = root.canonicalize().expect("the Go web directory");
    let mut files = Vec::new();
    walk(&root, "static", &mut files);
    let mut src = String::from("/// The phone page's files: each path under static/ and its bytes.\npub static FILES: &[(&str, &[u8])] = &[\n");
    for f in &files {
        src.push_str(&format!(
            "    ({:?}, include_bytes!({:?})),\n",
            f.strip_prefix("static/").expect("under static"),
            root.join(f).display().to_string()
        ));
    }
    src.push_str("];\n");
    std::fs::write(out_dir.join("web_static.rs"), src).expect("write web_static.rs");
}

fn main() {
    println!("cargo:rustc-env=COPPICE_VERSION={}", version());
    shipped_plugins(&PathBuf::from(env::var("OUT_DIR").expect("OUT_DIR")));
    web_static(&PathBuf::from(env::var("OUT_DIR").expect("OUT_DIR")));
    println!("cargo:rerun-if-env-changed=COPPICE_GHOSTTY_PREFIX");
    println!("cargo:rerun-if-env-changed=CC");
    println!("cargo:rerun-if-changed=csrc/vt_shim.c");
    let prefix = env::var("COPPICE_GHOSTTY_PREFIX").unwrap_or_else(|_| {
        panic!(
            "set COPPICE_GHOSTTY_PREFIX to the libghostty-vt prefix. \
             Run: export COPPICE_GHOSTTY_PREFIX=$(clients/go/scripts/native.sh --print-prefix)"
        )
    });
    let prefix = PathBuf::from(prefix);
    let lib = prefix.join("lib").join("libghostty-vt.a");
    if !lib.is_file() {
        panic!(
            "{} is missing. Build the native prefix first.",
            lib.display()
        );
    }
    println!("cargo:rerun-if-changed={}", lib.display());
    let out = PathBuf::from(env::var("OUT_DIR").expect("OUT_DIR"));
    let obj = out.join("vt_shim.o");
    let cc = env::var("CC").unwrap_or_else(|_| "cc".into());
    run(Command::new(&cc)
        .args(["-c", "-O2", "-fPIC", "-DGHOSTTY_STATIC", "-Wall", "-Werror"])
        .arg(format!("-I{}", prefix.join("include").display()))
        .arg("csrc/vt_shim.c")
        .arg("-o")
        .arg(&obj));
    let archive = out.join("libcopvt.a");
    let _ = std::fs::remove_file(&archive);
    run(Command::new("ar").arg("crs").arg(&archive).arg(&obj));
    println!("cargo:rustc-link-search=native={}", out.display());
    println!(
        "cargo:rustc-link-search=native={}",
        prefix.join("lib").display()
    );
    println!("cargo:rustc-link-lib=static=copvt");
    println!("cargo:rustc-link-lib=static=ghostty-vt");
}
