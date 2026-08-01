//! The two independent glob engines `clients/PORTING-NOTES.md` warns never
//! to unify, plus the `posixpath.normpath` port they both build on.
//!
//! `head_allowed` (`verify._head_allowed`) is LEFT-anchored: literal
//! entries need exact equality; glob entries require equal segment counts,
//! each segment matched case-sensitively.
//!
//! `path_matches_any` (`verify._path_matches_any` / `_match_glob`) is now a
//! NATIVE left-anchored, `/`-aware matcher (2026-08-21 oracle fix,
//! ADJUDICATIONS F-1/F-2). The oracle used to delegate to Python's
//! `PurePosixPath.match`, which matched relative patterns from the RIGHT —
//! `file_write: ["out.txt"]` admitted `/etc/cron.d/out.txt`, a real scope
//! escape — and gave mid-pattern `**` Python-version-dependent meaning.
//! Both bugs are gone: a pattern must consume the WHOLE path (so a relative
//! pattern can never match an absolute one — plain `str.split('/')`, NOT
//! filtered of empty segments, so an absolute path keeps its leading `""`
//! segment and a relative pattern's segment count can never line up with
//! it), and `**` recursively spans zero or more segments identically on
//! every Python version. `*`/`?`/`[...]` still stay within one segment.
//! A glob ending in `/**` is a dedicated fast path (root/`.`/prefix cases);
//! everything else goes through the general anchored recursive matcher.
//! Both engines share `fnmatch_segment`, a hand-rolled single-path-segment
//! `fnmatch.fnmatchcase` (`*`, `?`, `[seq]`, `[!seq]`, ranges) — no regex
//! crate needed since a path segment never contains `/`.

/// `posixpath.normpath` (POSIX flavor): collapse redundant separators and
/// resolve `.`/`..` lexically. Preserves the POSIX quirk that exactly two
/// leading slashes stay `//` (3+ collapse to one).
pub fn normpath(path: &str) -> String {
    if path.is_empty() {
        return ".".to_string();
    }
    let mut lead = 0usize;
    for ch in path.chars() {
        if ch == '/' {
            lead += 1;
        } else {
            break;
        }
    }
    let initial_slashes = if lead == 0 {
        0
    } else if lead == 2 {
        2
    } else {
        1
    };
    let rest = &path[lead..];
    let comps: Vec<&str> = rest.split('/').collect();
    let mut new_comps: Vec<&str> = Vec::new();
    for comp in comps {
        if comp.is_empty() || comp == "." {
            continue;
        }
        if comp != ".." || (initial_slashes == 0 && new_comps.is_empty()) || new_comps.last() == Some(&"..") {
            new_comps.push(comp);
        } else if !new_comps.is_empty() {
            new_comps.pop();
        }
    }
    let prefix = "/".repeat(initial_slashes);
    let joined = new_comps.join("/");
    let result = format!("{prefix}{joined}");
    if result.is_empty() { ".".to_string() } else { result }
}

// --- single-path-segment fnmatch (shared by both engines) -------------------------

#[derive(Debug, Clone)]
enum GlobUnit {
    Literal(char),
    Any,
    Star,
    Class { negate: bool, ranges: Vec<(char, char)>, chars: Vec<char> },
}

fn tokenize_glob(pat: &str) -> Vec<GlobUnit> {
    let chars: Vec<char> = pat.chars().collect();
    let n = chars.len();
    let mut units = Vec::new();
    let mut i = 0;
    while i < n {
        let c = chars[i];
        if c == '*' {
            units.push(GlobUnit::Star);
            i += 1;
            while i < n && chars[i] == '*' {
                i += 1;
            }
            continue;
        }
        if c == '?' {
            units.push(GlobUnit::Any);
            i += 1;
            continue;
        }
        if c == '[' {
            let mut j = i + 1;
            let mut negate = false;
            if j < n && chars[j] == '!' {
                negate = true;
                j += 1;
            }
            let start_content = j;
            if j < n && chars[j] == ']' {
                j += 1; // a leading ']' right after '[' / '[!' is a literal member
            }
            while j < n && chars[j] != ']' {
                j += 1;
            }
            if j >= n {
                units.push(GlobUnit::Literal('['));
                i += 1;
                continue;
            }
            let content: Vec<char> = chars[start_content..j].to_vec();
            let mut ranges = Vec::new();
            let mut lits = Vec::new();
            let mut k = 0;
            while k < content.len() {
                if k + 2 < content.len() && content[k + 1] == '-' {
                    ranges.push((content[k], content[k + 2]));
                    k += 3;
                } else {
                    lits.push(content[k]);
                    k += 1;
                }
            }
            units.push(GlobUnit::Class { negate, ranges, chars: lits });
            i = j + 1;
            continue;
        }
        units.push(GlobUnit::Literal(c));
        i += 1;
    }
    units
}

fn unit_matches(u: &GlobUnit, c: char) -> bool {
    match u {
        GlobUnit::Literal(l) => *l == c,
        GlobUnit::Any => true,
        GlobUnit::Star => unreachable!("Star is consumed structurally, not per-char"),
        GlobUnit::Class { negate, ranges, chars } => {
            let hit = chars.contains(&c) || ranges.iter().any(|(a, b)| *a <= c && c <= *b);
            hit != *negate
        }
    }
}

fn glob_match_units(text: &[char], units: &[GlobUnit]) -> bool {
    fn rec(text: &[char], ti: usize, units: &[GlobUnit], ui: usize) -> bool {
        if ui == units.len() {
            return ti == text.len();
        }
        match &units[ui] {
            GlobUnit::Star => {
                for k in ti..=text.len() {
                    if rec(text, k, units, ui + 1) {
                        return true;
                    }
                }
                false
            }
            other => ti < text.len() && unit_matches(other, text[ti]) && rec(text, ti + 1, units, ui + 1),
        }
    }
    rec(text, 0, units, 0)
}

/// `fnmatch.fnmatchcase` restricted to one path segment (no `/`).
pub fn fnmatch_segment(text: &str, pattern: &str) -> bool {
    let units = tokenize_glob(pattern);
    let t: Vec<char> = text.chars().collect();
    glob_match_units(&t, &units)
}

fn has_glob_chars(pat: &str) -> bool {
    pat.contains(['*', '?', '['])
}

/// `verify._head_allowed` — left-anchored: literal entries need exact
/// equality; glob entries require equal segment counts (both split on
/// `/`), each segment matched case-sensitively.
pub fn head_allowed(head: &str, allowlist: &[String]) -> bool {
    for pat in allowlist {
        if head == pat {
            return true;
        }
        if !has_glob_chars(pat) {
            continue;
        }
        let head_segs: Vec<&str> = head.split('/').collect();
        let pat_segs: Vec<&str> = pat.split('/').collect();
        if head_segs.len() != pat_segs.len() {
            continue;
        }
        if head_segs.iter().zip(pat_segs.iter()).all(|(h, p)| fnmatch_segment(h, p)) {
            return true;
        }
    }
    false
}

/// `_match_glob(norm, glob)` — left-anchored, `/`-aware match of a
/// normalized path against one glob. Mirrors `verify._match_glob` exactly,
/// including the dedicated `"/**"`-suffix fast path (root / `.` / literal
/// prefix cases) before falling through to the general recursive matcher.
fn match_glob(norm: &str, glob: &str) -> bool {
    if let Some(raw) = glob.strip_suffix("/**") {
        if raw.is_empty() {
            // "/**" — the root: any absolute path.
            return norm.starts_with('/');
        }
        let prefix = normpath(raw);
        if prefix == "." {
            // "./**" — any relative path (never absolute, never ".." itself
            // or anything that walks above the starting directory).
            return !norm.starts_with('/') && norm != ".." && !norm.starts_with("../");
        }
        return norm == prefix || norm.starts_with(&format!("{prefix}/"));
    }

    // Plain `split('/')`, NOT filtered of empty segments: an absolute path's
    // leading "" segment must survive so a relative pattern (whose own
    // split has no leading "") can never line up with it segment-for-segment.
    let pat_segs: Vec<&str> = glob.split('/').collect();
    let path_segs: Vec<&str> = norm.split('/').collect();

    fn match_from(pat_segs: &[&str], path_segs: &[&str], pi: usize, ti: usize) -> bool {
        if pi == pat_segs.len() {
            return ti == path_segs.len();
        }
        if pat_segs[pi] == "**" {
            return (ti..=path_segs.len()).any(|k| match_from(pat_segs, path_segs, pi + 1, k));
        }
        if ti == path_segs.len() {
            return false;
        }
        fnmatch_segment(path_segs[ti], pat_segs[pi]) && match_from(pat_segs, path_segs, pi + 1, ti + 1)
    }

    match_from(&pat_segs, &path_segs, 0, 0)
}

/// `verify._path_matches_any`.
pub fn path_matches_any(path: &str, globs: &[String]) -> bool {
    let normalized = normpath(path);
    globs.iter().any(|g| match_glob(&normalized, g))
}
