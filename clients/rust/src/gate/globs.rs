//! The verifier's two glob engines, `verify._head_allowed` and
//! `verify._path_matches_any` / `_match_glob`, on `fnmatch.fnmatchcase` as
//! Python 3.12 has it: `fnmatch.translate` ported line for line and the
//! result run on the port of `re`.
//!
//! `_match_glob` recurses over `**` segments and stops past
//! `verify.GLOB_MATCH_STEP_LIMIT` calls of its inner matcher with
//! GlobTooComplex. The port counts those calls exactly (a memo over the
//! recursion gives the count, however large) and raises the same error.

use super::frames::depth;
use super::paths::normpath;
use super::py::re::{self as pyre, pycp};
use super::{undecided, PyErr, R};

/// `verify.GLOB_MATCH_STEP_LIMIT`.
pub const GLOB_MATCH_STEP_LIMIT: u128 = 100_000;

/// `fnmatch.translate(pat)`.
pub fn translate(pat: &str) -> R<String> {
    enum Part {
        Star,
        Text(String),
    }
    let p: Vec<char> = pat.chars().collect();
    let cp = |c: char| pycp(c);
    let n = p.len();
    let mut res: Vec<Part> = Vec::new();
    let mut i = 0;
    while i < n {
        let c = p[i];
        i += 1;
        if c == '*' {
            if !matches!(res.last(), Some(Part::Star)) {
                res.push(Part::Star);
            }
        } else if c == '?' {
            res.push(Part::Text(".".into()));
        } else if c == '[' {
            let mut j = i;
            if j < n && p[j] == '!' {
                j += 1;
            }
            if j < n && p[j] == ']' {
                j += 1;
            }
            while j < n && p[j] != ']' {
                j += 1;
            }
            if j >= n {
                res.push(Part::Text("\\[".into()));
            } else {
                let mut stuff: String;
                if !p[i..j].contains(&'-') {
                    stuff = p[i..j].iter().collect::<String>().replace('\\', "\\\\");
                } else {
                    let mut chunks: Vec<Vec<char>> = Vec::new();
                    let mut k = if p[i] == '!' { i + 2 } else { i + 1 };
                    loop {
                        // pat.find('-', k, j)
                        let found = if k < j { p[k..j].iter().position(|&x| x == '-').map(|o| k + o) } else { None };
                        match found {
                            None => break,
                            Some(f) => {
                                chunks.push(p[i..f].to_vec());
                                i = f + 1;
                                k = f + 3;
                            }
                        }
                    }
                    let chunk: Vec<char> = if i < j { p[i..j].to_vec() } else { vec![] };
                    if !chunk.is_empty() {
                        chunks.push(chunk);
                    } else {
                        match chunks.last_mut() {
                            Some(l) => l.push('-'),
                            None => return Err(PyErr::new("IndexError", "list index out of range").into()),
                        }
                    }
                    let mut k = chunks.len().saturating_sub(1);
                    while k > 0 {
                        let a = match chunks[k - 1].last() {
                            Some(&a) => a,
                            None => return Err(PyErr::new("IndexError", "string index out of range").into()),
                        };
                        let b = match chunks[k].first() {
                            Some(&b) => b,
                            None => return Err(PyErr::new("IndexError", "string index out of range").into()),
                        };
                        if cp(a) > cp(b) {
                            let mut merged = chunks[k - 1][..chunks[k - 1].len() - 1].to_vec();
                            merged.extend_from_slice(&chunks[k][1..]);
                            chunks[k - 1] = merged;
                            chunks.remove(k);
                        }
                        k -= 1;
                    }
                    stuff = chunks
                        .iter()
                        .map(|s| s.iter().collect::<String>().replace('\\', "\\\\").replace('-', "\\-"))
                        .collect::<Vec<_>>()
                        .join("-");
                }
                // re.sub(r'([&~|])', r'\\\1', stuff)
                let mut esc = String::with_capacity(stuff.len());
                for ch in stuff.chars() {
                    if matches!(ch, '&' | '~' | '|') {
                        esc.push('\\');
                    }
                    esc.push(ch);
                }
                stuff = esc;
                i = j + 1;
                if stuff.is_empty() {
                    res.push(Part::Text("(?!)".into()));
                } else if stuff == "!" {
                    res.push(Part::Text(".".into()));
                } else {
                    if let Some(rest) = stuff.strip_prefix('!') {
                        stuff = format!("^{rest}");
                    } else if stuff.starts_with('^') || stuff.starts_with('[') {
                        stuff = format!("\\{stuff}");
                    }
                    res.push(Part::Text(format!("[{stuff}]")));
                }
            }
        } else {
            res.push(Part::Text(re_escape(c)));
        }
    }
    let mut out = String::new();
    let mut i = 0;
    let n = res.len();
    while i < n && !matches!(res[i], Part::Star) {
        if let Part::Text(t) = &res[i] {
            out.push_str(t);
        }
        i += 1;
    }
    while i < n {
        i += 1;
        if i == n {
            out.push_str(".*");
            break;
        }
        let mut fixed = String::new();
        while i < n && !matches!(res[i], Part::Star) {
            if let Part::Text(t) = &res[i] {
                fixed.push_str(t);
            }
            i += 1;
        }
        if i == n {
            out.push_str(".*");
            out.push_str(&fixed);
        } else {
            out.push_str(&format!("(?>.*?{fixed})"));
        }
    }
    Ok(format!("(?s:{out})\\Z"))
}

/// `re.escape(c)` of one character.
fn re_escape(c: char) -> String {
    if "()[]{}?*+-|^$\\.&~# \t\n\r\x0b\x0c".contains(c) {
        format!("\\{c}")
    } else {
        c.to_string()
    }
}

/// `fnmatch.fnmatchcase(name, pat)`, counted as one call.
pub fn fnmatchcase(name: &str, pat: &str) -> R<bool> {
    if depth() > 950 {
        return undecided("a glob match deep in the oracle's stack");
    }
    let rx = translate(pat)?;
    Ok(pyre::cached_compile(&rx)?.match_start(name)?)
}

/// `verify._GLOB_CHARS_RE.search(pat)`.
fn has_glob_chars(pat: &str) -> bool {
    pat.contains(['*', '?', '['])
}

/// `verify._head_allowed(head, allowlist)`.
pub fn head_allowed(head: &str, allowlist: &[String]) -> R<bool> {
    for pat in allowlist {
        if head == pat {
            return Ok(true);
        }
        if !has_glob_chars(pat) {
            continue;
        }
        let hs: Vec<&str> = head.split('/').collect();
        let ps: Vec<&str> = pat.split('/').collect();
        if hs.len() != ps.len() {
            continue;
        }
        let mut all = true;
        for (h, p) in hs.iter().zip(ps.iter()) {
            if !fnmatchcase(h, p)? {
                all = false;
                break;
            }
        }
        if all {
            return Ok(true);
        }
    }
    Ok(false)
}

/// `verify._path_matches_any(path, globs)`.
pub fn path_matches_any(path: &str, globs: &[String]) -> R<bool> {
    let norm = normpath(path);
    for g in globs {
        if match_glob(&norm, g)? {
            return Ok(true);
        }
    }
    Ok(false)
}

/// `verify._match_glob(norm, glob)`: the answer, and the calls counted.
fn match_glob(norm: &str, glob: &str) -> R<bool> {
    if let Some(raw) = glob.strip_suffix("/**") {
        if raw.is_empty() {
            return Ok(norm.starts_with('/'));
        }
        let prefix = normpath(raw);
        if prefix == "." {
            return Ok(!norm.starts_with('/') && norm != ".." && !norm.starts_with("../"));
        }
        return Ok(norm == prefix || norm.starts_with(&format!("{prefix}/")));
    }
    let ps: Vec<&str> = glob.split('/').collect();
    let ts: Vec<&str> = norm.split('/').collect();
    // The deepest the recursion could go: a frame per pattern segment and
    // one more for each `**` generator, above _path_matches_any, its
    // generator and _match_glob.
    if depth() + 3 + 2 * ps.len() as i64 + 2 > 950 {
        return undecided("a glob with enough segments to reach Python's recursion limit");
    }
    let mut m = Memo { ps: &ps, ts: &ts, memo: vec![vec![None; ts.len() + 1]; ps.len() + 1] };
    let (ans, calls) = m.from(0, 0)?;
    if calls > GLOB_MATCH_STEP_LIMIT {
        return Err(PyErr::value(format!(
            "file glob {} is too complex to match: more than {GLOB_MATCH_STEP_LIMIT} steps",
            super::py::text::repr(glob)
        ))
        .into());
    }
    Ok(ans)
}

/// `match_from` memoized: each state's answer and the calls the oracle
/// makes under it, visited in the oracle's order so the first error is
/// the oracle's.
struct Memo<'a> {
    ps: &'a [&'a str],
    ts: &'a [&'a str],
    memo: Vec<Vec<Option<(bool, u128)>>>,
}

impl Memo<'_> {
    fn from(&mut self, pi: usize, ti: usize) -> R<(bool, u128)> {
        if let Some(r) = self.memo[pi][ti] {
            return Ok(r);
        }
        let (p, t) = (self.ps.len(), self.ts.len());
        let r = if pi == p {
            (ti == t, 1)
        } else if self.ps[pi] == "**" {
            let mut calls: u128 = 1;
            let mut ans = false;
            for k in ti..=t {
                let (a, c) = self.from(pi + 1, k)?;
                calls = calls.saturating_add(c);
                if a {
                    ans = true;
                    break;
                }
            }
            (ans, calls)
        } else if ti == t {
            (false, 1)
        } else if fnmatchcase(self.ts[ti], self.ps[pi])? {
            let (a, c) = self.from(pi + 1, ti + 1)?;
            (a, c.saturating_add(1))
        } else {
            (false, 1)
        };
        self.memo[pi][ti] = Some(r);
        Ok(r)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn translate_is_fnmatchs() {
        assert_eq!(translate("[a[]b*c*d").unwrap(), "(?s:[a[]b(?>.*?c).*d)\\Z");
        assert_eq!(translate("*.py").unwrap(), "(?s:.*\\.py)\\Z");
        assert_eq!(translate("[!z-a]").unwrap(), "(?s:.)\\Z");
        assert_eq!(translate("[z-a]").unwrap(), "(?s:(?!))\\Z");
        assert_eq!(translate("[").unwrap(), "(?s:\\[)\\Z");
        assert_eq!(translate("[c-b-a]").unwrap(), "(?s:[\\-a])\\Z");
    }

    #[test]
    fn globs_match_as_the_verifier_does() {
        let g = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert!(path_matches_any("/work/a/b.txt", &g(&["/work/**"])).unwrap());
        assert!(path_matches_any("/work/a/b.txt", &g(&["/work/*/*.txt"])).unwrap());
        assert!(!path_matches_any("/etc/x", &g(&["/work/**"])).unwrap());
        assert!(path_matches_any("/a/x/y/b", &g(&["/a/**/b"])).unwrap());
        assert!(head_allowed(".venv/bin/pytest", &g(&[".venv/bin/*"])).unwrap());
        assert!(!head_allowed("/x/.venv/bin/pytest", &g(&[".venv/bin/*"])).unwrap());
    }

    #[test]
    fn a_blowup_is_the_step_limit() {
        let glob = format!("/{}/b", ["**", "a"].repeat(9).join("/"));
        let path = format!("/{}/c", vec!["a"; 40].join("/"));
        let e = path_matches_any(&path, &[glob]).unwrap_err();
        assert!(matches!(e, super::super::Fault::Raised(p) if p.msg.contains("more than 100000 steps")));
    }
}
