//! `opendaisugi.datahome`: where openDaisugi keeps its data when no flag
//! names a directory.
//!
//! 1. `$OPENDAISUGI_HOME` when it is usable (see `usable`);
//! 2. else `$XDG_DATA_HOME/opendaisugi` when XDG_DATA_HOME is usable and
//!    `~/.opendaisugi` does not exist;
//! 3. else `~/.opendaisugi`.
//!
//! An existing install keeps its directory. coppice keeps its own rule
//! (`coppice_dir`). The gate guards every
//! directory the rule can pick (`guarded`), with no exists check. Paths
//! print as pathlib prints them.

use std::path::Path;

/// The data directory's name under HOME.
pub const LEGACY_NAME: &str = ".opendaisugi";

/// `datahome.usable`: `v` with a leading `~` or `~/` replaced by `home`,
/// when the result is an absolute path; else None (empty, relative and
/// `~user` values are ignored, as the XDG spec says of a relative XDG path).
pub fn usable(v: Option<String>, home: &str) -> Option<String> {
    let v = v.filter(|v| !v.is_empty())?;
    let v = if v == "~" {
        home.to_string()
    } else if let Some(rest) = v.strip_prefix("~/") {
        format!("{home}/{rest}")
    } else {
        v
    };
    v.starts_with('/').then(|| clean(&v))
}

/// `data_home(env, home)`: `get` reads the environment, `home` is
/// `Path.home()` as pathlib prints it, `exists` is `Path.exists`.
pub fn dir(get: impl Fn(&str) -> Option<String>, home: &str, exists: impl Fn(&str) -> bool) -> String {
    let h = clean(home);
    if let Some(d) = usable(get("OPENDAISUGI_HOME"), &h) {
        return d;
    }
    let dot = join(&h, LEGACY_NAME);
    if let Some(x) = usable(get("XDG_DATA_HOME"), &h) {
        if !exists(&dot) {
            return join(&x, "opendaisugi");
        }
    }
    dot
}

/// The rule coppice uses for its data directory
/// (`harness/coppice-rs/src/datahome.rs`). It never asks whether
/// `~/.opendaisugi` exists, because `daisugi install --gate` creates it and
/// the same box would then use two places, depending on the order of two
/// commands: `$OPENDAISUGI_HOME`, else `$XDG_DATA_HOME/opendaisugi` when
/// that is usable, else `~/.opendaisugi`. XDG is chosen only when the
/// variable is set, because `~/.local/share/opendaisugi` is in no guard set
/// otherwise. One exception, as in coppice: an old `~/.opendaisugi/coppice`
/// that holds data is kept while the XDG coppice directory holds none.
/// `has` says whether a path exists and holds data (see `has`).
pub fn coppice_dir(
    get: impl Fn(&str) -> Option<String>,
    home: &str,
    has: impl Fn(&str) -> bool,
) -> String {
    let h = clean(home);
    if let Some(d) = usable(get("OPENDAISUGI_HOME"), &h) {
        return d;
    }
    let dot = join(&h, LEGACY_NAME);
    let Some(x) = usable(get("XDG_DATA_HOME"), &h) else {
        return dot;
    };
    let xdg = join(&x, "opendaisugi");
    if !has(&join(&xdg, "coppice")) && has(&join(&dot, "coppice")) {
        return dot;
    }
    xdg
}

/// Whether `p` exists and holds data: a file, or a directory with at least
/// one entry. Links are followed.
pub fn has(p: &str) -> bool {
    match std::fs::metadata(p) {
        Err(_) => false,
        Ok(m) if !m.is_dir() => true,
        Ok(_) => std::fs::read_dir(p).map(|mut d| d.next().is_some()).unwrap_or(false),
    }
}

/// `guarded_data_dirs(env, home)`: every directory `dir` can pick, legacy
/// first, without duplicates and with no exists check.
pub fn guarded(get: impl Fn(&str) -> Option<String>, home: &str) -> Vec<String> {
    let h = clean(home);
    let mut all = vec![join(&h, LEGACY_NAME)];
    if let Some(d) = usable(get("OPENDAISUGI_HOME"), &h) {
        all.push(d);
    }
    if let Some(x) = usable(get("XDG_DATA_HOME"), &h) {
        all.push(join(&x, "opendaisugi"));
    }
    let mut out: Vec<String> = Vec::new();
    for p in all {
        if !out.contains(&p) {
            out.push(p);
        }
    }
    out
}

/// `Path.exists`: follows links; any error is false.
pub fn exists(p: &str) -> bool {
    Path::new(p).exists()
}

/// `str(PurePosixPath(p))`: repeated slashes collapsed (two leading
/// slashes kept), "." parts dropped, no trailing slash.
fn clean(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let lead = if p.starts_with("//") && !p.starts_with("///") {
        "//"
    } else if p.starts_with('/') {
        "/"
    } else {
        ""
    };
    let parts: Vec<&str> = p.split('/').filter(|s| !s.is_empty() && *s != ".").collect();
    let out = format!("{lead}{}", parts.join("/"));
    if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

/// pathlib's `a / b` for a relative `b`.
fn join(a: &str, b: &str) -> String {
    match a {
        "." | "" => b.to_string(),
        "/" | "//" => format!("{a}{b}"),
        _ => format!("{a}/{b}"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn env(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let m: HashMap<String, String> = pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect();
        move |k: &str| m.get(k).cloned()
    }

    #[test]
    fn the_rule() {
        let yes = |_: &str| true;
        let no = |_: &str| false;
        assert_eq!(dir(env(&[]), "/h", no), "/h/.opendaisugi");
        assert_eq!(dir(env(&[("OPENDAISUGI_HOME", "/o"), ("XDG_DATA_HOME", "/x")]), "/h", yes), "/o");
        assert_eq!(dir(env(&[("XDG_DATA_HOME", "/x")]), "/h", no), "/x/opendaisugi");
        assert_eq!(dir(env(&[("XDG_DATA_HOME", "/x")]), "/h", yes), "/h/.opendaisugi");
        assert_eq!(dir(env(&[("OPENDAISUGI_HOME", ""), ("XDG_DATA_HOME", "")]), "/h", no), "/h/.opendaisugi");
        assert_eq!(dir(env(&[("XDG_DATA_HOME", "/h//x/")]), "/h", no), "/h/x/opendaisugi");
    }

    #[test]
    fn coppice_dir_ignores_whether_the_dot_dir_exists() {
        let dot_only = |p: &str| p == "/h/.opendaisugi";
        let none = |_: &str| false;
        assert_eq!(coppice_dir(env(&[("XDG_DATA_HOME", "/x")]), "/h", dot_only), "/x/opendaisugi");
        assert_eq!(coppice_dir(env(&[]), "/h", dot_only), "/h/.opendaisugi");
        assert_eq!(
            coppice_dir(env(&[("OPENDAISUGI_HOME", "/o"), ("XDG_DATA_HOME", "/x")]), "/h", none),
            "/o"
        );
        // Old coppice data is kept while the XDG one holds none.
        let old = |p: &str| p == "/h/.opendaisugi/coppice";
        assert_eq!(coppice_dir(env(&[("XDG_DATA_HOME", "/x")]), "/h", old), "/h/.opendaisugi");
    }

    #[test]
    fn a_tilde_is_expanded_and_a_relative_value_ignored() {
        let no = |_: &str| false;
        assert_eq!(dir(env(&[("OPENDAISUGI_HOME", "~/od")]), "/h", no), "/h/od");
        assert_eq!(dir(env(&[("OPENDAISUGI_HOME", "~")]), "/h", no), "/h");
        assert_eq!(dir(env(&[("XDG_DATA_HOME", "~/x")]), "/h", no), "/h/x/opendaisugi");
        for bad in ["od", "./od", "~user/od", "~od"] {
            assert_eq!(dir(env(&[("OPENDAISUGI_HOME", bad)]), "/h", no), "/h/.opendaisugi", "{bad}");
            assert_eq!(dir(env(&[("XDG_DATA_HOME", bad)]), "/h", no), "/h/.opendaisugi", "{bad}");
        }
        assert_eq!(
            dir(env(&[("OPENDAISUGI_HOME", "od"), ("XDG_DATA_HOME", "/x")]), "/h", no),
            "/x/opendaisugi"
        );
        assert_eq!(
            guarded(env(&[("OPENDAISUGI_HOME", "~/od"), ("XDG_DATA_HOME", "rel")]), "/h"),
            vec!["/h/.opendaisugi", "/h/od"]
        );
    }

    #[test]
    fn the_guarded_set_holds_every_place() {
        assert_eq!(
            guarded(env(&[("OPENDAISUGI_HOME", "/h/od"), ("XDG_DATA_HOME", "/h/x")]), "/h"),
            vec!["/h/.opendaisugi", "/h/od", "/h/x/opendaisugi"]
        );
        assert_eq!(guarded(env(&[]), "/h"), vec!["/h/.opendaisugi"]);
        assert_eq!(guarded(env(&[("OPENDAISUGI_HOME", "/h/.opendaisugi")]), "/h"), vec!["/h/.opendaisugi"]);
    }
}
