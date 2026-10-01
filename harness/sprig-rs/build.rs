//! Sets SPRIG_VERSION, what `sprig --version` prints: SPRIG_VERSION when
//! set (scripts/install.sh and scripts/release.sh set it), else the
//! checkout's commit, cut to 12 characters, with -dirty for a tree with
//! changes, as the Go build reports its own; else "unknown".
use std::env;
use std::process::Command;

fn git(args: &[&str]) -> Option<String> {
    let o = Command::new("git").args(args).output().ok()?;
    if !o.status.success() {
        return None;
    }
    Some(String::from_utf8_lossy(&o.stdout).trim().to_string())
}

fn version() -> String {
    println!("cargo:rerun-if-env-changed=SPRIG_VERSION");
    if let Ok(v) = env::var("SPRIG_VERSION") {
        if !v.is_empty() {
            return v;
        }
    }
    let Some(mut rev) = git(&["rev-parse", "HEAD"]).filter(|r| !r.is_empty()) else {
        return "unknown".into();
    };
    rev.truncate(12);
    if git(&["status", "--porcelain"]).is_some_and(|s| !s.is_empty()) {
        rev.push_str("-dirty");
    }
    rev
}

fn main() {
    println!("cargo:rustc-env=SPRIG_VERSION={}", version());
}
