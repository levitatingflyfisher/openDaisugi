//! `search_rule.py`: no search reaches coppice's secrets, and the pane
//! rule's check that no shell glob can match a secret file.
//!
//! A recursive search reads every file under its root, so a root that is
//! an ancestor of a coppice data directory reads its secrets. The rule
//! finds the roots of each search in a shell line (rg, ag, ack, rgrep,
//! ugrep, ug, git grep; grep, egrep, fgrep and zgrep when they recurse;
//! find with -exec and its kin), places each from the cwd the line has
//! reached, and refuses a root that is an ancestor of a data directory, a
//! glob that can reach a secret, or a root the gate cannot place. A
//! file-read tool other than Glob and Read is checked on each path it
//! names, or on its cwd.
//!
//! The glob matcher and the brace expansion work on Python code points,
//! as the oracle's do, and count Python's stack frames: a pattern that
//! would run Python out of stack counts as a hit there, since the oracle
//! treats any error in a rule as a hit.

use super::paths::{basename, isabs, join2, normpath};
use super::py::text::as_surrogate;
use super::pyjson::{Object, Value};
use super::record::Record;
use super::frames::{depth, frame, frames, trace};
use super::{undecided, Runner, R};
use crate::interpreter_parse::shlex_split;

pub const SEARCH_REFUSAL: &str = "this search reaches coppice's secrets. Search a narrower directory.";

/// `pane_rule._SECRET_NAMES`: each secret file, relative to its coppice
/// data directory.
pub const SECRET_NAMES: &[&str] =
    &["web/token", "web/ca/ca.key", "web/ca/leaf.key", "web/tls/tailscale.key", "voice/token"];

const ALWAYS: &[&str] = &["rg", "ag", "ack", "rgrep", "ugrep", "ug"];
const GREPS: &[&str] = &["grep", "egrep", "fgrep", "zgrep"];
const FIND_EXEC: &[&str] = &["-exec", "-execdir", "-ok", "-okdir"];
const PROGRAMS: &[&str] = &["rg", "ag", "ack", "rgrep", "ugrep", "ug", "grep", "egrep", "fgrep", "zgrep", "find"];
const GREP_FLAGS: &str = "rRnilLcvwxsqHhoEFGPIazZUbTy";
const RG_FLAGS: &str = "iIlLcvwxsqHhoFPazUbuSNnp0.";
const OTHER_FLAGS: &str = "ilLcvwxHhonRr";
const LONG_FLAGS: &[&str] = &[
    "--recursive",
    "--dereference-recursive",
    "--hidden",
    "--no-ignore",
    "--no-ignore-vcs",
    "--line-number",
    "--ignore-case",
    "--files-with-matches",
    "--files-without-match",
    "--count",
    "--invert-match",
    "--word-regexp",
    "--line-regexp",
    "--fixed-strings",
    "--follow",
    "--no-heading",
    "--with-filename",
    "--no-filename",
    "--files",
    "--unrestricted",
    "--null",
    "--text",
    "--smart-case",
    "--case-sensitive",
    "--no-messages",
    "--quiet",
    "--only-matching",
    "--multiline",
    "--json",
    "--vimgrep",
    "--search-zip",
    "--no-index",
];
const PATTERN_OPTS: &[&str] = &["-e", "-f", "--regexp", "--file", "--files"];
const PREFIXES: &[&str] = &[
    "env", "sudo", "doas", "timeout", "nice", "ionice", "stdbuf", "time", "command", "exec", "nohup", "xargs",
    "setsid", "chrt", "taskset", "strace", "busybox",
];
const MAX_DEPTH: usize = 2;
const MAX_BRACES: usize = 64;

/// Python's recursion limit.
const RECURSION_LIMIT: i64 = 1000;

/// The work the glob matcher may do before it gives up. The oracle has
/// no bound and would run on for a very long time; the port denies.
const GLOB_STEPS: u64 = 5_000_000;

/// A word char in `_PROGRAM_TEXT`'s class `[A-Za-z0-9_.-]`.
fn program_word_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '_' || c == '.' || c == '-'
}

/// `_PROGRAM_TEXT.search(text)`: a search program's name as a word.
pub fn program_text(text: &str) -> bool {
    let chars: Vec<char> = text.chars().collect();
    for i in 0..chars.len() {
        if i > 0 && program_word_char(chars[i - 1]) {
            continue;
        }
        for name in PROGRAMS {
            let n: Vec<char> = name.chars().collect();
            if chars.len() >= i + n.len()
                && chars[i..i + n.len()] == n[..]
                && (i + n.len() == chars.len() || !program_word_char(chars[i + n.len()]))
            {
                return true;
            }
        }
    }
    false
}

/// `_GLOB.search(w)`.
pub fn has_glob(w: &str) -> bool {
    w.contains(['*', '?', '[', '{'])
}

/// `_UNRESOLVED.search(w)`: a variable, a substitution or a backtick.
fn unresolved(w: &str) -> bool {
    let b = w.as_bytes();
    b.contains(&b'`')
        || b.windows(2).any(|p| {
            p[0] == b'$'
                && (p[1].is_ascii_alphanumeric() || b"_{(@*#?!$-".contains(&p[1]))
        })
}

fn unresolvable(w: &str) -> bool {
    unresolved(w) || w.starts_with('~')
}

/// `_ASSIGN.match(w)`.
fn is_assign(t: &str) -> bool {
    let b = t.as_bytes();
    if b.is_empty() || !(b[0].is_ascii_alphabetic() || b[0] == b'_') {
        return false;
    }
    for &c in &b[1..] {
        if c == b'=' {
            return true;
        }
        if !(c.is_ascii_alphanumeric() || c == b'_') {
            return false;
        }
    }
    false
}

fn flags_for(head: &str) -> &'static str {
    if GREPS.contains(&head) {
        GREP_FLAGS
    } else if head == "rg" {
        RG_FLAGS
    } else {
        OTHER_FLAGS
    }
}

/// `_takes_value`: the word after this option may be its value.
fn takes_value(word: &str, flags: &str) -> bool {
    if word.starts_with("--") {
        return !word.contains('=') && !LONG_FLAGS.contains(&word);
    }
    let chars: Vec<char> = word.chars().collect();
    for (i, c) in chars.iter().enumerate().skip(1) {
        if !flags.contains(*c) {
            return i == chars.len() - 1;
        }
    }
    false
}

fn rest_has(w: &str, cs: &[char]) -> bool {
    w.chars().skip(1).any(|c| cs.contains(&c))
}

/// `re.fullmatch(r"-[0-9]+", w)`.
fn dash_digits(w: &str) -> bool {
    w.len() > 1 && w.starts_with('-') && w[1..].bytes().all(|b| b.is_ascii_digit())
}

/// `_grep_recurses`.
fn grep_recurses(words: &[String]) -> bool {
    for w in words {
        if w.contains("recurse") {
            return true;
        }
        if w.starts_with("--") {
            let name = w.split('=').next().unwrap_or("");
            if name.chars().count() > 2
                && ("--recursive".starts_with(name) || "--dereference-recursive".starts_with(name))
            {
                return true;
            }
        } else if w.starts_with('-') && rest_has(w, &['r', 'R']) {
            return true;
        }
        if dash_digits(w) || w.starts_with("--depth") {
            return true;
        }
    }
    false
}

/// `_gives_pattern`.
fn gives_pattern(words: &[String]) -> bool {
    for w in words {
        if w == "--" {
            return false;
        }
        for opt in PATTERN_OPTS {
            if w == opt || w.starts_with(&format!("{opt}=")) {
                return true;
            }
        }
        if w.starts_with('-') && !w.starts_with("--") && rest_has(w, &['e', 'f']) {
            return true;
        }
    }
    false
}

/// `_tool_roots`: every word the tool may search.
fn tool_roots(head: &str, args: &[String]) -> Vec<String> {
    let flags = flags_for(head);
    let mut every = Vec::new();
    let mut sure: Vec<String> = Vec::new();
    let (mut ended, mut value_next) = (false, false);
    for w in args {
        if ended {
            every.push(w.clone());
            sure.push(w.clone());
            continue;
        }
        if w == "--" {
            ended = true;
            value_next = false;
            continue;
        }
        if w.starts_with('-') && w.chars().count() > 1 {
            value_next = takes_value(w, flags);
            continue;
        }
        every.push(w.clone());
        if !value_next {
            sure.push(w.clone());
        }
        value_next = false;
    }
    if !sure.is_empty() && !gives_pattern(args) {
        sure.remove(0);
    }
    if sure.is_empty() {
        every.push(".".into());
    }
    every
}

/// `re.fullmatch(r"-O[0-9]*", w)`.
fn opt_level(w: &str) -> bool {
    w.starts_with("-O") && w[2..].bytes().all(|b| b.is_ascii_digit())
}

/// `_find_roots`.
fn find_roots(args: &[String]) -> Vec<String> {
    let mut roots = Vec::new();
    let mut i = 0;
    while i < args.len() {
        let w = args[i].as_str();
        if matches!(w, "-H" | "-L" | "-P" | "--") || opt_level(w) {
            i += 1;
            continue;
        }
        if w == "-D" {
            i += 2;
            continue;
        }
        if w.starts_with('-') || w.starts_with('(') || w.starts_with('!') {
            break;
        }
        roots.push(w.to_string());
        i += 1;
    }
    if roots.is_empty() {
        roots.push(".".into());
    }
    roots
}

/// `_search_roots`: every root a simple command searches, or None when it
/// is no search the rule knows.
fn search_roots(tokens: &[String]) -> Option<Vec<String>> {
    let words: Vec<String> =
        tokens.iter().map(|t| t.strip_prefix('\\').map(String::from).unwrap_or_else(|| t.clone())).collect();
    let mut start = 0;
    while start < words.len() && is_assign(&words[start]) {
        start += 1;
    }
    let mut prefixed = false;
    for i in start..words.len() {
        let head = basename(&words[i]);
        if i > start && !prefixed {
            prefixed = PREFIXES.contains(&basename(&words[i - 1]));
            if !prefixed {
                continue;
            }
        }
        let rest = &words[i + 1..];
        if head == "git" {
            let g = match rest.iter().position(|x| x == "grep") {
                None => continue,
                Some(g) => g,
            };
            let mut out: Vec<String> = rest[..g].iter().filter(|p| !p.starts_with('-')).cloned().collect();
            out.extend(tool_roots("rg", &rest[g + 1..]));
            return Some(out);
        }
        if ALWAYS.contains(&head) || (GREPS.contains(&head) && grep_recurses(rest)) {
            if words[..i].iter().any(|x| basename(x) == "xargs") {
                return Some(vec!["$(xargs)".into()]);
            }
            return Some(tool_roots(head, rest));
        }
        if head == "find" && rest.iter().any(|x| FIND_EXEC.contains(&x.as_str())) {
            return Some(find_roots(rest));
        }
    }
    None
}

/// Each match of `_BRACE` (`\{([^{}]*)\}`) in `word`, in order: the byte
/// positions of its `{` and `}`.
fn braces(word: &str) -> Vec<(usize, usize)> {
    let b = word.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'{' {
            if let Some(k) = b[i + 1..].iter().position(|&x| x == b'{' || x == b'}') {
                let j = i + 1 + k;
                if b[j] == b'}' {
                    out.push((i, j));
                    i = j + 1;
                    continue;
                }
            }
        }
        i += 1;
    }
    out
}

/// One end of a sequence brace: `-?[0-9]+` or one ASCII letter.
fn seq_atom(s: &str) -> Option<(&str, &str)> {
    let b = s.as_bytes();
    if !b.is_empty() && b[0].is_ascii_alphabetic() {
        return Some((&s[..1], &s[1..]));
    }
    let start = usize::from(b.first() == Some(&b'-'));
    let n = b[start..].iter().take_while(|c| c.is_ascii_digit()).count();
    if n == 0 {
        return None;
    }
    Some((&s[..start + n], &s[start + n..]))
}

/// `_sequence`: the words of a bash sequence brace such as `a..e` or
/// `1..9..2`; None when the body is no sequence; `Some(None)` for one the
/// gate will not expand (zero-padded, mixed, past nine digits or 64 words).
fn sequence(body: &str) -> Option<Option<Vec<String>>> {
    let (a, rest) = seq_atom(body)?;
    let rest = rest.strip_prefix("..")?;
    let (b, rest) = seq_atom(rest)?;
    let step = if rest.is_empty() {
        None
    } else {
        let r = rest.strip_prefix("..")?;
        let digits = r.strip_prefix('-').unwrap_or(r);
        if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
            return None;
        }
        Some(r)
    };
    let num = |x: &str| {
        let t = x.trim_start_matches('-');
        !t.is_empty() && t.bytes().all(|c| c.is_ascii_digit())
    };
    let ints = num(a) && num(b);
    if !ints && (num(a) || num(b)) {
        return Some(None);
    }
    if ints && [a, b].iter().any(|x| {
        let t = x.trim_start_matches('-');
        t.len() > 1 && t.starts_with('0')
    }) {
        return Some(None);
    }
    if [Some(a), Some(b), step].iter().flatten().any(|x| x.trim_start_matches('-').len() > 9) {
        return Some(None);
    }
    let val = |x: &str| -> i64 { if ints { x.parse().unwrap_or(0) } else { x.as_bytes()[0] as i64 } };
    let (lo, hi) = (val(a), val(b));
    let mut n = step.map(|s| s.parse::<i64>().unwrap_or(0).abs()).unwrap_or(1);
    if n == 0 {
        n = 1;
    }
    if (hi - lo).abs() / n + 1 > MAX_BRACES as i64 {
        return Some(None);
    }
    let d = if hi >= lo { n } else { -n };
    let mut vals = Vec::new();
    let mut v = lo;
    let stop = hi + if d > 0 { 1 } else { -1 };
    while (d > 0 && v < stop) || (d < 0 && v > stop) {
        vals.push(if ints { v.to_string() } else { (v as u8 as char).to_string() });
        v += d;
    }
    Some(Some(vals))
}

/// `_brace_expand`, with `depth` the Python frame of this call: false when
/// the word holds more than the gate expands, or a sequence it will not
/// expand. Stops where Python would run out of stack.
fn brace_expand(word: &str, out: &mut Vec<String>, depth: i64) -> Result<bool, Stop> {
    if depth > RECURSION_LIMIT {
        return Err(Stop::Deep);
    }
    for (s, e) in braces(word) {
        let body = &word[s + 1..e];
        let alts: Vec<String> = if body.contains(',') {
            body.split(',').map(String::from).collect()
        } else {
            match sequence(body) {
                None => continue,
                Some(None) => return Ok(false),
                Some(Some(v)) => v,
            }
        };
        for part in alts {
            if !brace_expand(&format!("{}{}{}", &word[..s], part, &word[e + 1..]), out, depth + 1)? {
                return Ok(false);
            }
            if out.len() > MAX_BRACES {
                return Ok(false);
            }
        }
        return Ok(true);
    }
    out.push(word.to_string());
    Ok(true)
}

/// Python code points of a str.
fn cps(s: &str) -> Vec<u32> {
    s.chars().map(|c| as_surrogate(c).unwrap_or(c as u32)).collect()
}

/// Why the glob matcher stopped short.
enum Stop {
    /// Python would raise RecursionError here.
    Deep,
    /// Past the port's work bound.
    Long,
}

struct Matcher {
    steps: u64,
}

impl Matcher {
    /// `_match_part` at Python stack depth `depth` (frames in use once
    /// this call is on the stack).
    fn part(&mut self, pat: &[u32], name: &[u32], depth: i64) -> Result<bool, Stop> {
        if depth > RECURSION_LIMIT {
            return Err(Stop::Deep);
        }
        self.steps += 1;
        if self.steps > GLOB_STEPS {
            return Err(Stop::Long);
        }
        let star = '*' as u32;
        let (q, lb, rb, bang, caret, dash) =
            ('?' as u32, '[' as u32, ']' as u32, '!' as u32, '^' as u32, '-' as u32);
        if pat.is_empty() {
            return Ok(name.is_empty());
        }
        let c = pat[0];
        if c == star {
            // any(... for i in ...) runs a generator frame under this one.
            for i in 0..=name.len() {
                if self.part(&pat[1..], &name[i..], depth + 2)? {
                    return Ok(true);
                }
            }
            return Ok(false);
        }
        if name.is_empty() {
            return Ok(false);
        }
        if c == q {
            return self.part(&pat[1..], &name[1..], depth + 1);
        }
        if c == lb {
            let neg1 = pat.len() > 1 && (pat[1] == bang || pat[1] == caret);
            let find = |from: usize| -> Option<usize> {
                if from > pat.len() {
                    return None;
                }
                pat[from..].iter().position(|&x| x == rb).map(|k| from + k)
            };
            let mut end = find(if neg1 { 2 } else { 1 });
            if neg1 && end == Some(2) {
                end = find(3);
            }
            let end = match end {
                None => {
                    return Ok(name[0] == lb && self.part(&pat[1..], &name[1..], depth + 1)?);
                }
                Some(e) => e,
            };
            let mut body = &pat[1..end];
            let neg = !body.is_empty() && (body[0] == bang || body[0] == caret);
            if neg {
                body = &body[1..];
            }
            let mut hit = false;
            let mut j = 0;
            while j < body.len() {
                if j + 2 < body.len() && body[j + 1] == dash {
                    if body[j] <= name[0] && name[0] <= body[j + 2] {
                        hit = true;
                    }
                    j += 3;
                } else {
                    if body[j] == name[0] {
                        hit = true;
                    }
                    j += 1;
                }
            }
            return Ok(hit != neg && self.part(&pat[end + 1..], &name[1..], depth + 1)?);
        }
        Ok(c == name[0] && self.part(&pat[1..], &name[1..], depth + 1)?)
    }
}

fn parts(p: &str) -> Vec<&str> {
    p.split('/').filter(|x| !x.is_empty()).collect()
}

impl Runner {
    /// `search_rule._place`.
    fn place(&self, word: &str, cwd: &str) -> R<String> {
        let _f = frame();
        let p = self.expanduser(&self.home_spellings(word)?)?;
        if isabs(&p) {
            return Ok(p);
        }
        let base = if cwd.is_empty() { self.getwd()? } else { cwd.to_string() };
        Ok(join2(&base, &p))
    }

    /// `search_rule.glob_matches`: a glob word, brace-expanded and placed
    /// from base, can match a target path, part by part. A pattern that
    /// runs Python out of stack is a hit, as the oracle's rule reads any
    /// error.
    fn glob_matches(&self, word: &str, base: &str, targets: &[String]) -> R<bool> {
        let _f = frame();
        trace("glob");
        let at = depth();
        let mut alts = Vec::new();
        match brace_expand(word, &mut alts, at + 1) {
            Ok(true) => {}
            Ok(false) => return self.overflow_reaches(word, base, targets),
            // RecursionError: the rule reads it as a hit.
            Err(_) => return Ok(true),
        }
        let mut m = Matcher { steps: 0 };
        for alt in &alts {
            let placed = normpath(&self.place(alt, base)?);
            let pp: Vec<Vec<u32>> = parts(&placed).iter().map(|s| cps(s)).collect();
            for t in targets {
                let tp = parts(t);
                if tp.len() != pp.len() {
                    continue;
                }
                let mut all = true;
                for (a, b) in pp.iter().zip(tp.iter()) {
                    // all(... for ...) runs a generator frame, and
                    // _match_part is called from it.
                    match m.part(a, &cps(b), at + 2) {
                        Ok(true) => {}
                        Ok(false) => {
                            all = false;
                            break;
                        }
                        Err(Stop::Deep) => return Ok(true),
                        Err(Stop::Long) => {
                            return undecided("a glob the oracle's matcher would take very long to match")
                        }
                    }
                }
                if all {
                    return Ok(true);
                }
            }
        }
        Ok(false)
    }

    /// `search_rule._overflow_reaches`: for a word the gate will not
    /// expand, true unless the text before its first brace shows it cannot
    /// reach a target.
    fn overflow_reaches(&self, word: &str, base: &str, targets: &[String]) -> R<bool> {
        let _f = frame();
        if !word.contains('/') {
            let prefix = format!("{}/", base.trim_end_matches('/'));
            return Ok(targets.iter().any(|t| t == base || t.starts_with(&prefix)));
        }
        let pre = match word.find('{') {
            Some(i) => {
                let _g = frame();
                self.home_spellings(&word[..i])?
            }
            None => word.to_string(),
        };
        let pre = self.expanduser(&pre)?;
        if !pre.starts_with('/') || has_glob(&pre) {
            return Ok(true);
        }
        if normpath(&pre).trim_end_matches('/') != pre.trim_end_matches('/') && pre != "/" {
            return Ok(true);
        }
        Ok(targets.iter().any(|t| t.starts_with(&pre)))
    }

    /// `search_rule._data_dirs`.
    fn data_dirs(&self) -> R<Vec<String>> {
        let _f = frame();
        self.coppice_data_dirs()
    }

    /// `search_rule.secret_files`.
    pub fn secret_files(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut out = Vec::new();
        for d in self.data_dirs()? {
            for s in SECRET_NAMES {
                out.push(format!("{d}/{s}"));
            }
        }
        Ok(out)
    }

    /// `search_rule._reach_targets`: each prefix of each secret file.
    fn reach_targets(&self) -> R<Vec<String>> {
        let _f = frame();
        let mut out = vec!["/".to_string()];
        for f in self.secret_files()? {
            let ps = parts(&f);
            for i in 1..=ps.len() {
                let p = format!("/{}", ps[..i].join("/"));
                if !out.contains(&p) {
                    out.push(p);
                }
            }
        }
        Ok(out)
    }

    /// `search_rule.path_above_secret`.
    fn path_above_secret(&self, raw: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        if raw.is_empty() {
            return Ok(false);
        }
        let p = self.place(raw, cwd)?;
        let cands = [normpath(&p), self.resolve(&p)?];
        let data = self.data_dirs()?;
        Ok(data
            .iter()
            .any(|d| cands.iter().any(|c| d.starts_with(&format!("{}/", c.trim_end_matches('/'))))))
    }

    /// `search_rule._root_reaches`.
    fn root_reaches(&self, word: &str, here: Option<&str>) -> R<bool> {
        let _f = frame();
        let w = self.expand_home(word)?;
        if unresolvable(&w) {
            return Ok(true);
        }
        if !isabs(&w) && here.is_none() {
            return Ok(true);
        }
        let base = match here {
            Some(h) if !h.is_empty() => h,
            _ => "/",
        };
        if has_glob(&w) {
            let targets = self.reach_targets()?;
            return self.glob_matches(&w, base, &targets);
        }
        self.path_above_secret(&w, base)
    }

    /// `search_rule._line_reaches`.
    fn line_reaches(&self, command: &str, cwd: Option<String>, depth: usize) -> R<bool> {
        let _f = frame();
        let d = match self.decompose(command) {
            Ok(d) => d,
            Err(e) => return Err(e),
        };
        if !d.ok || d.commands.is_empty() {
            return Ok(program_text(command));
        }
        let mut here = cwd;
        for text in &d.commands {
            let raw = match shlex_split(text) {
                Ok(r) => r,
                Err(_) => return Ok(program_text(command)),
            };
            for w in &raw {
                // Past the depth the gate follows, a line that still names a
                // search program is refused.
                if w.contains(' ')
                    && program_text(w)
                    && (depth >= MAX_DEPTH || self.line_reaches(w, here.clone(), depth + 1)?)
                {
                    return Ok(true);
                }
            }
            let roots = {
                let _g = frame();
                search_roots(&raw)
            };
            if let Some(roots) = roots {
                // any(_root_reaches(r, here) for r in roots) runs a
                // generator frame.
                let _g = frame();
                for r in &roots {
                    if self.root_reaches(r, here.as_deref())? {
                        return Ok(true);
                    }
                }
            }
            let mut tokens = Vec::with_capacity(raw.len());
            for t in &raw {
                tokens.push(self.expand_home(t)?);
            }
            here = self.next_cwd(&tokens, here)?;
        }
        Ok(false)
    }

    /// `search_rule.shell_search_reaches`.
    pub fn shell_search_reaches(&self, command: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        let here = if isabs(cwd) { Some(cwd.to_string()) } else { None };
        self.line_reaches(command, here, 0)
    }

    /// `search_rule.search_above_secret_hit`, less its catch (the caller
    /// reads any error as a hit).
    pub fn search_above_secret_hit(&self, p: &Object, cwd: &str, rec: Option<&Record>) -> R<bool> {
        let _f = frame();
        let rec = match rec {
            None => return Ok(false),
            Some(r) => r,
        };
        match rec.step_type.as_str() {
            "shell" => self.shell_search_reaches(&rec.command, cwd),
            "file_read" => {
                let tool = rec.tool_name.as_str();
                if tool == "Glob" || tool == "Read" || tool == "read" {
                    return Ok(false);
                }
                let paths = {
                    let _g = frame();
                    search_paths(p)
                };
                let paths = match paths {
                    None => return Ok(true),
                    Some(ps) => ps,
                };
                let mut placed = true;
                {
                    // all(... for p in paths) runs a generator frame, and
                    // _home_text one more.
                    let _g = frames(2);
                    for x in &paths {
                        if !isabs(&self.expanduser(&self.home_spellings(x)?)?) {
                            placed = false;
                            break;
                        }
                    }
                }
                if !isabs(cwd) && (paths.is_empty() || !placed) {
                    return Ok(true);
                }
                if !paths.is_empty() {
                    let _g = frame();
                    for x in &paths {
                        if self.path_above_secret(x, cwd)? {
                            return Ok(true);
                        }
                    }
                    return Ok(false);
                }
                self.path_above_secret(cwd, cwd)
            }
            _ => Ok(false),
        }
    }

    /// `pane_rule.shell_globs_secret`: a shell word holds a glob that,
    /// placed from the cwd the line has reached, can match a secret file.
    pub fn shell_globs_secret(&self, command: &str, cwd: &str) -> R<bool> {
        let _f = frame();
        let d = self.decompose(command)?;
        if !d.ok || d.commands.is_empty() {
            return Ok(false);
        }
        let files = self.secret_files()?;
        let mut here: Option<String> = if cwd.is_empty() { None } else { Some(cwd.to_string()) };
        for text in &d.commands {
            let raw = match shlex_split(text) {
                Ok(r) => r,
                Err(_) => return Ok(false),
            };
            let mut tokens = Vec::with_capacity(raw.len());
            for t in &raw {
                tokens.push(self.expand_home(t)?);
            }
            for t in &tokens {
                let mut w = t.as_str();
                if w.starts_with('-') && w.contains('=') {
                    w = w.split_once('=').map(|x| x.1).unwrap_or("");
                }
                let known = here.as_deref().is_some_and(|h| !h.is_empty());
                if !has_glob(w) || !(isabs(w) || known) {
                    continue;
                }
                let base = here.as_deref().filter(|h| !h.is_empty()).unwrap_or("/").to_string();
                if self.glob_matches(w, &base, &files)? {
                    return Ok(true);
                }
            }
            here = self.next_cwd(&tokens, here)?;
        }
        Ok(false)
    }
}

/// `search_rule.search_paths`: the paths a file-read tool's input names,
/// or None when it names a path that is not a string.
pub fn search_paths(p: &Object) -> Option<Vec<String>> {
    let inp = ["tool_input", "args", "input"].iter().map(|k| p.value(k)).find(|v| v.truthy());
    let o = match inp {
        Some(Value::Obj(o)) => o,
        _ => return Some(vec![]),
    };
    let mut out = Vec::new();
    for k in ["file_path", "path", "filePath"] {
        let v = o.value(k);
        if !v.truthy() {
            continue;
        }
        match v {
            Value::Str(s) => out.push(s.clone()),
            _ => return None,
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn w(s: &str) -> Vec<String> {
        s.split(' ').map(String::from).collect()
    }

    #[test]
    fn roots_follow_the_oracle() {
        assert_eq!(search_roots(&w("rg foo /")), Some(w("foo /")));
        assert_eq!(search_roots(&w("rg foo")), Some(w("foo .")));
        assert_eq!(search_roots(&w("rg -g x foo")), Some(w("x foo .")));
        assert_eq!(search_roots(&w("rg -e foo /a")), Some(w("foo /a")));
        assert_eq!(search_roots(&w("grep foo /")), None);
        assert_eq!(search_roots(&w("grep --rec foo /")), Some(w("foo / .")));
        assert_eq!(search_roots(&w("env A=1 grep -r foo")), Some(w("foo .")));
        assert_eq!(search_roots(&w("ls | xargs rg foo")), Some(w("$(xargs)")));
        assert_eq!(search_roots(&w("xargs rg foo")), Some(w("$(xargs)")));
        assert_eq!(search_roots(&w("git -C /a grep foo")), Some(w("/a foo .")));
        assert_eq!(search_roots(&w("find -L / -name x -exec cat {} ;")), Some(w("/")));
        assert_eq!(search_roots(&w("find -D x -O2 -- /a -exec y")), Some(w("/a")));
        assert_eq!(search_roots(&w("\\rg x /")), Some(w("x /")));
    }

    #[test]
    fn program_text_and_braces() {
        assert!(program_text("sh -c 'rg x'") && program_text("find") && !program_text("rgx") && !program_text("a.rg"));
        assert!(program_text("rgrep") && program_text("x;grep"));
        let x = |word: &str| {
            let mut out = Vec::new();
            match brace_expand(word, &mut out, 0) {
                Ok(true) => Some(out),
                Ok(false) => None,
                Err(_) => panic!("deep"),
            }
        };
        assert_eq!(x("/{a,b}/{c,d}").unwrap(), w("/a/c /a/d /b/c /b/d"));
        assert!(x(&"{a,b}".repeat(7)).is_none());
        assert_eq!(x("{a..c}").unwrap(), w("a b c"));
        assert_eq!(x("{5..1..2}").unwrap(), w("5 3 1"));
        assert_eq!(x("{-1..1}").unwrap(), w("-1 0 1"));
        assert_eq!(x("{a}{1..2}").unwrap(), w("{a}1 {a}2"));
        assert!(x("{01..3}").is_none() && x("{a..3}").is_none() && x("{1..1000}").is_none());
        assert!(x("{1..1234567890}").is_none());
        let mut out = Vec::new();
        assert!(brace_expand(&"{1..1}".repeat(2000), &mut out, 10).is_err());
        assert!(unresolved("$HOME") && unresolved("a`b") && !unresolved("a$") && !unresolved("$)"));
    }

    #[test]
    fn part_matching_is_pythons() {
        let mut m = Matcher { steps: 0 };
        let mut t = |p: &str, n: &str| m.part(&cps(p), &cps(n), 0).ok().unwrap();
        assert!(t("*", "abc") && t("a?c", "abc") && t("[a-c]bc", "bbc") && t("[!x]bc", "abc"));
        assert!(!t("[]]x", "]x") && t("[!]]x", "ax") && t("[x", "[x") && !t("[x", "x"));
        assert!(t("[^a]", "b") && !t("", "a") && t("", ""));
    }
}
