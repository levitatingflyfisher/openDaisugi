//! Where coppice keeps its data when no flag names a directory, the rule
//! of Go coppice's `internal/datahome`. One rule, so a friend can predict
//! it (F11, 2026-10-24). It never asks whether `~/.opendaisugi` exists:
//!
//! 1. `$OPENDAISUGI_HOME` when it is usable: a leading `~` expanded, then
//!    absolute;
//! 2. else `$XDG_DATA_HOME/opendaisugi` when XDG_DATA_HOME is usable;
//! 3. else `~/.opendaisugi`;
//! 4. except that, when step 2 chose, an existing `~/.opendaisugi/coppice`
//!    that holds data is kept while the XDG coppice directory holds none.
//!    The user keeps the old state, and `legacy_note` says so in one line.
//!
//! Before this rule the answer depended on whether `~/.opendaisugi`
//! existed, and `daisugi install --gate` creates it. Both places are in the
//! set the gate and the fence guard (`guarded_data_dirs`). Step 2 needs the
//! variable to be set: when XDG_DATA_HOME is unset, `~/.local/share/opendaisugi`
//! is in no guard set, so the default stays `~/.opendaisugi`. coppice's data
//! directory, its fallback socket and the foreman's directory sit under it.

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

/// `$XDG_DATA_HOME/opendaisugi`, or None when XDG_DATA_HOME is unset or
/// not usable.
fn xdg_dir(get: &impl Fn(&str) -> Option<OsString>, home: &Path) -> Option<PathBuf> {
    usable(get("XDG_DATA_HOME"), home).map(|x| x.join("opendaisugi"))
}

/// The rule. `get` reads the environment; `home` is the home directory, or
/// None when there is none (the legacy name is then relative, as Go's is);
/// `has` says whether a path exists and holds data. The bool is true when
/// the old `~/.opendaisugi` was kept.
pub fn resolve(
    get: impl Fn(&str) -> Option<OsString>,
    home: Option<PathBuf>,
    has: impl Fn(&Path) -> bool,
) -> (PathBuf, bool) {
    let home = home.unwrap_or_default();
    if let Some(d) = usable(get("OPENDAISUGI_HOME"), &home) {
        return (d, false);
    }
    let dot = home.join(".opendaisugi");
    // XDG_DATA_HOME not set: ~/.opendaisugi, whether or not it exists.
    let Some(xdg) = xdg_dir(&get, &home) else {
        return (dot, false);
    };
    if !has(&xdg.join("coppice")) && has(&dot.join("coppice")) {
        return (dot, true);
    }
    (xdg, false)
}

/// `resolve` without the legacy flag.
pub fn dir(
    get: impl Fn(&str) -> Option<OsString>,
    home: Option<PathBuf>,
    has: impl Fn(&Path) -> bool,
) -> PathBuf {
    resolve(get, home, has).0
}

/// Whether `p` exists and holds data: a file, or a directory with at least
/// one entry. Links are followed.
pub fn has(p: &Path) -> bool {
    match std::fs::metadata(p) {
        Err(_) => false,
        Ok(m) if !m.is_dir() => true,
        Ok(_) => std::fs::read_dir(p).map(|mut d| d.next().is_some()).unwrap_or(false),
    }
}

/// `dir` for this process: its environment and its home directory.
pub fn default() -> PathBuf {
    dir(|k| std::env::var_os(k), sys::home_dir(), has)
}

/// The one-line note for a process that keeps the old
/// `~/.opendaisugi/coppice`, or None when it does not.
pub fn legacy_note() -> Option<String> {
    let home = sys::home_dir();
    let (d, legacy) = resolve(|k| std::env::var_os(k), home.clone(), has);
    if !legacy {
        return None;
    }
    let xdg = xdg_dir(&|k| std::env::var_os(k), &home.unwrap_or_default())?;
    Some(format!(
        "coppice: keeping the data in {}. New installs keep it in {}.",
        d.join("coppice").display(),
        xdg.join("coppice").display()
    ))
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

    fn h() -> Option<PathBuf> {
        Some(PathBuf::from("/h"))
    }

    fn holding(paths: &'static [&'static str]) -> impl Fn(&Path) -> bool {
        move |p: &Path| paths.iter().any(|q| Path::new(q) == p)
    }

    #[test]
    fn the_rule() {
        let none = holding(&[]);
        let cases: Vec<(Vec<(&str, &str)>, &'static [&'static str], &str, bool)> = vec![
            (vec![("OPENDAISUGI_HOME", "/o"), ("XDG_DATA_HOME", "/x")], &["/h/.opendaisugi/coppice"], "/o", false),
            (vec![("XDG_DATA_HOME", "/x")], &[], "/x/opendaisugi", false),
            (vec![], &[], "/h/.opendaisugi", false),
            (vec![("XDG_DATA_HOME", "/x")], &["/h/.opendaisugi/gate"], "/x/opendaisugi", false),
            (vec![("OPENDAISUGI_HOME", ""), ("XDG_DATA_HOME", "")], &[], "/h/.opendaisugi", false),
            (vec![("XDG_DATA_HOME", "/x")], &["/h/.opendaisugi/coppice"], "/h/.opendaisugi", true),
            (vec![("XDG_DATA_HOME", "/x")], &["/h/.opendaisugi/coppice", "/x/opendaisugi/coppice"], "/x/opendaisugi", false),
            (vec![], &["/h/.opendaisugi/coppice"], "/h/.opendaisugi", false),
        ];
        for (pairs, present, want, legacy) in cases {
            let (got, was_legacy) = resolve(env(&pairs), h(), holding(present));
            assert_eq!((got, was_legacy), (PathBuf::from(want), legacy), "{pairs:?} {present:?}");
        }
        assert_eq!(dir(env(&[]), None, none), PathBuf::from(".opendaisugi"));
    }

    #[test]
    fn a_tilde_is_expanded_and_a_relative_value_ignored() {
        let no = |_: &Path| false;
        assert_eq!(dir(env(&[("OPENDAISUGI_HOME", "~/od")]), h(), no), PathBuf::from("/h/od"));
        assert_eq!(dir(env(&[("OPENDAISUGI_HOME", "~")]), h(), no), PathBuf::from("/h"));
        assert_eq!(dir(env(&[("XDG_DATA_HOME", "~/x")]), h(), no), PathBuf::from("/h/x/opendaisugi"));
        for bad in ["od", "./od", "~user/od", "~od"] {
            assert_eq!(dir(env(&[("OPENDAISUGI_HOME", bad)]), h(), no), PathBuf::from("/h/.opendaisugi"));
            assert_eq!(dir(env(&[("XDG_DATA_HOME", bad)]), h(), no), PathBuf::from("/h/.opendaisugi"));
        }
        assert_eq!(
            dir(env(&[("OPENDAISUGI_HOME", "od"), ("XDG_DATA_HOME", "/x")]), h(), no),
            PathBuf::from("/x/opendaisugi")
        );
    }

    #[test]
    fn has_means_a_file_or_a_directory_with_an_entry() {
        let d = std::env::temp_dir().join(format!("dh-has-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(d.join("empty")).unwrap();
        std::fs::create_dir_all(d.join("full")).unwrap();
        std::fs::write(d.join("full").join("layout.json"), "{}").unwrap();
        assert!(!has(&d.join("missing")));
        assert!(!has(&d.join("empty")));
        assert!(has(&d.join("full")));
        assert!(has(&d.join("full").join("layout.json")));
        let _ = std::fs::remove_dir_all(&d);
    }
}
