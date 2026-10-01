//! Where openDaisugi keeps its data when no flag names a directory, the
//! rule of `opendaisugi.datahome` and Go coppice's `internal/datahome`:
//!
//! 1. `$OPENDAISUGI_HOME` when it is usable: a leading `~` expanded, then
//!    absolute;
//! 2. else `$XDG_DATA_HOME/opendaisugi` when XDG_DATA_HOME is usable and
//!    `~/.opendaisugi` does not exist;
//! 3. else `~/.opendaisugi`.
//!
//! An existing install keeps its directory. coppice's data directory and
//! its fallback socket sit under it.

use std::ffi::OsString;
use std::path::{Path, PathBuf};

use crate::sys;

/// `v` with a leading `~` or `~/` replaced by `home`, when the result is
/// an absolute path; else None. Empty, relative and `~user` values are
/// ignored, as the XDG spec says of a relative XDG path.
fn usable(v: Option<OsString>, home: &Path) -> Option<PathBuf> {
    let v = v.filter(|v| !v.is_empty())?;
    let s = v.to_string_lossy().into_owned();
    let p = if s == "~" {
        home.to_path_buf()
    } else if let Some(rest) = s.strip_prefix("~/") {
        home.join(rest)
    } else {
        PathBuf::from(v)
    };
    p.is_absolute().then_some(p)
}

/// The rule. `get` reads the environment; `home` is the home directory, or
/// None when there is none (the legacy name is then relative, as Go's is).
pub fn dir(
    get: impl Fn(&str) -> Option<OsString>,
    home: Option<PathBuf>,
    exists: impl Fn(&Path) -> bool,
) -> PathBuf {
    let home = home.unwrap_or_default();
    if let Some(d) = usable(get("OPENDAISUGI_HOME"), &home) {
        return d;
    }
    let dot = home.join(".opendaisugi");
    if let Some(x) = usable(get("XDG_DATA_HOME"), &home) {
        if !exists(&dot) {
            return x.join("opendaisugi");
        }
    }
    dot
}

/// `dir` for this process: its environment and its home directory.
pub fn default() -> PathBuf {
    dir(|k| std::env::var_os(k), sys::home_dir(), |p| p.exists())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn env(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<OsString> {
        let m: HashMap<String, OsString> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), OsString::from(v)))
            .collect();
        move |k: &str| m.get(k).cloned()
    }

    #[test]
    fn the_rule() {
        let h = || Some(PathBuf::from("/h"));
        let yes = |_: &Path| true;
        let no = |_: &Path| false;
        assert_eq!(dir(env(&[]), h(), no), PathBuf::from("/h/.opendaisugi"));
        assert_eq!(
            dir(
                env(&[("OPENDAISUGI_HOME", "/o"), ("XDG_DATA_HOME", "/x")]),
                h(),
                yes
            ),
            PathBuf::from("/o")
        );
        assert_eq!(
            dir(env(&[("XDG_DATA_HOME", "/x")]), h(), no),
            PathBuf::from("/x/opendaisugi")
        );
        assert_eq!(
            dir(env(&[("XDG_DATA_HOME", "/x")]), h(), yes),
            PathBuf::from("/h/.opendaisugi")
        );
        assert_eq!(
            dir(
                env(&[("OPENDAISUGI_HOME", ""), ("XDG_DATA_HOME", "")]),
                h(),
                no
            ),
            PathBuf::from("/h/.opendaisugi")
        );
        assert_eq!(dir(env(&[]), None, no), PathBuf::from(".opendaisugi"));
    }

    #[test]
    fn a_tilde_is_expanded_and_a_relative_value_ignored() {
        let h = || Some(PathBuf::from("/h"));
        let no = |_: &Path| false;
        assert_eq!(
            dir(env(&[("OPENDAISUGI_HOME", "~/od")]), h(), no),
            PathBuf::from("/h/od")
        );
        assert_eq!(
            dir(env(&[("OPENDAISUGI_HOME", "~")]), h(), no),
            PathBuf::from("/h")
        );
        assert_eq!(
            dir(env(&[("XDG_DATA_HOME", "~/x")]), h(), no),
            PathBuf::from("/h/x/opendaisugi")
        );
        for bad in ["od", "./od", "~user/od", "~od"] {
            assert_eq!(
                dir(env(&[("OPENDAISUGI_HOME", bad)]), h(), no),
                PathBuf::from("/h/.opendaisugi")
            );
            assert_eq!(
                dir(env(&[("XDG_DATA_HOME", bad)]), h(), no),
                PathBuf::from("/h/.opendaisugi")
            );
        }
        assert_eq!(
            dir(
                env(&[("OPENDAISUGI_HOME", "od"), ("XDG_DATA_HOME", "/x")]),
                h(),
                no
            ),
            PathBuf::from("/x/opendaisugi")
        );
    }
}
