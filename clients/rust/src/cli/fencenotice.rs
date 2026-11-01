//! The one-line notice when the warden is on and coppice's config turns the
//! fence off. Only a fence stops a file access (ADR-0028). The Python oracle
//! holds the same text and rule (`fence_notice.py`).

use super::Env;
use std::path::Path;

pub(super) const UNCONFINED_LINE: &str = "Files are not confined: the warden checks commands, but only a fence stops \
a file access. Turn the fence on with fence = \"auto\" in coppice.toml.";

/// `^\s*fence\s*=\s*("off"|'off')\s*(#.*)?$` without a regex engine.
fn is_fence_off(line: &str) -> bool {
    let Some(rest) = line.trim_start().strip_prefix("fence") else { return false };
    let Some(rest) = rest.trim_start().strip_prefix('=') else { return false };
    let rest = rest.trim_start();
    let Some(rest) = rest.strip_prefix("\"off\"").or_else(|| rest.strip_prefix("'off'")) else { return false };
    let rest = rest.trim_start();
    rest.is_empty() || rest.starts_with('#')
}

/// True when the top level of the file says fence = "off". A missing file,
/// another value, or a key under a table does not count.
pub(super) fn config_turns_fence_off(path: &Path) -> bool {
    let Ok(text) = std::fs::read_to_string(path) else { return false };
    for line in text.lines() {
        if line.trim_start().starts_with('[') {
            return false;
        }
        if is_fence_off(line) {
            return true;
        }
    }
    false
}

/// True when the root holds envelopes and no DISARMED marker.
fn warden_is_on(root: &str) -> bool {
    let root = Path::new(root);
    root.join("envelopes").exists() && std::fs::symlink_metadata(root.join("DISARMED")).is_err()
}

impl Env {
    /// $XDG_CONFIG_HOME/coppice/coppice.toml, else ~/.config/coppice/coppice.toml.
    fn coppice_config_path(&self) -> std::path::PathBuf {
        let base = match self.env.get("XDG_CONFIG_HOME") {
            Some(b) if !b.is_empty() => std::path::PathBuf::from(b),
            _ => Path::new(&self.home).join(".config"),
        };
        base.join("coppice").join("coppice.toml")
    }

    /// True when the warden at root is on and coppice's config turns the
    /// fence off.
    pub(super) fn unconfined(&self, root: &str) -> bool {
        warden_is_on(root) && config_turns_fence_off(&self.coppice_config_path())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_a_top_level_fence_off_counts() {
        let dir = std::env::temp_dir().join(format!("fence-notice-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cases = [
            ("fence = \"off\"\n", true),
            ("fence = 'off'  # mine\n", true),
            ("fence = \"auto\"\n", false),
            ("fence = \"on\"\n", false),
            ("", false),
            ("[ui]\nfence = \"off\"\n", false),
            ("theme = \"x\"\nfence = \"off\"\n", true),
            ("fencex = \"off\"\n", false),
        ];
        for (text, want) in cases {
            let p = dir.join("coppice.toml");
            std::fs::write(&p, text).unwrap();
            assert_eq!(config_turns_fence_off(&p), want, "{text:?}");
        }
        assert!(!config_turns_fence_off(&dir.join("none.toml")));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
