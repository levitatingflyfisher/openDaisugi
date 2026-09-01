//! The system dialect (`opendaisugi/dialect.py`): the words, the glob
//! translation that fills their typed hole, and the write paths of one
//! step that `forall_writes` reads (`opendaisugi/write_paths.py`). The
//! tables are in `dialect_gen.rs`, written from the oracle by
//! `clients/gen_dialect.py`.

use crate::dialect_gen::{WriterSpec, DD_KEYS, MAX_GLOB_CHARS, MAX_GLOB_DOUBLE_STARS, MAX_GLOB_STARS, SYNONYMS, WORDS, WRITERS};
use crate::glob_engine::normpath;
use crate::interpreter_parse::{parse_interpreter, shlex_split};
use crate::shell_decompose::ShellParser;
use serde_json::Value;
use std::cell::RefCell;

pub use crate::dialect_gen::{AUDIT_PREFIX, DIALECT_HASH, DIALECT_JSON};

/// `dialect.word_for`: the word an invariant type names.
pub fn word_for(type_name: &str) -> Option<&'static str> {
    if let Some((w, ..)) = WORDS.iter().find(|(w, ..)| *w == type_name) {
        return Some(w);
    }
    SYNONYMS.iter().find(|(n, _)| *n == type_name).map(|(_, w)| *w)
}

/// `dialect.target_of`: the given target, else the word's default.
pub fn word_target(word: &str, target: Option<&str>) -> String {
    match target {
        Some(t) if !t.is_empty() => t.to_string(),
        _ => WORDS.iter().find(|(w, ..)| *w == word).map(|(_, _, d, _)| d.to_string()).unwrap_or_default(),
    }
}

/// `dialect.UnsupportedGlob`; the text is the oracle's.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UnsupportedGlob(pub String);

const REGEX_SPECIALS: &str = "\\.^$*+?{}[]|()";
const ANY_SEGMENTS: &str = "(?:/[^/]*)*";
const END: &str = "\\Z";

fn escape(s: &str) -> String {
    let mut out = String::new();
    for c in s.chars() {
        if REGEX_SPECIALS.contains(c) {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

fn segment(seg: &str) -> String {
    let mut out = String::new();
    for c in seg.chars() {
        match c {
            '*' => out.push_str("[^/]*"),
            '?' => out.push_str("[^/]"),
            c => out.push_str(&escape(&c.to_string())),
        }
    }
    out
}

fn rest(elems: &[Option<String>]) -> String {
    elems
        .iter()
        .map(|e| match e {
            None => ANY_SEGMENTS.to_string(),
            Some(s) => format!("/{s}"),
        })
        .collect()
}

fn first(elems: &[Option<String>]) -> String {
    match elems.split_first() {
        None => String::new(),
        Some((Some(head), tail)) => format!("{head}{}", rest(tail)),
        Some((None, tail)) => format!("(?:{}|[^/]*{ANY_SEGMENTS}{})", first(tail), rest(tail)),
    }
}

/// `dialect.glob_regex`: a regex for `re.search` that matches exactly the
/// normalized paths the file-scope matcher matches with this glob.
pub fn glob_regex(glob: &str) -> Result<String, UnsupportedGlob> {
    let fail = |m: String| Err(UnsupportedGlob(m));
    if glob.is_empty() {
        return fail("the glob is empty".into());
    }
    if glob.chars().count() > MAX_GLOB_CHARS {
        return fail(format!("the glob is longer than {MAX_GLOB_CHARS} characters"));
    }
    if glob.contains('[') || glob.contains(']') {
        return fail("character classes ([...]) are not supported".into());
    }
    if glob.matches('*').count() > MAX_GLOB_STARS {
        return fail(format!("the glob has more than {MAX_GLOB_STARS} stars"));
    }
    if let Some(raw) = glob.strip_suffix("/**") {
        if raw.is_empty() {
            return Ok(format!("^{}+{END}", &ANY_SEGMENTS[..ANY_SEGMENTS.len() - 1]));
        }
        let prefix = normpath(raw);
        if prefix == "." {
            return fail("a /** glob whose prefix is . is not supported".into());
        }
        return Ok(format!("^{}{ANY_SEGMENTS}{END}", escape(&prefix)));
    }
    let segs: Vec<&str> = glob.split('/').collect();
    if segs.iter().filter(|s| **s == "**").count() > MAX_GLOB_DOUBLE_STARS {
        return fail(format!("the glob has more than {MAX_GLOB_DOUBLE_STARS} ** segments"));
    }
    let elems: Vec<Option<String>> = segs.iter().map(|s| if *s == "**" { None } else { Some(segment(s)) }).collect();
    Ok(format!("^(?:{}){END}", first(&elems)))
}

/// `dialect._fill`: a string that is exactly `$name` becomes the value.
fn fill(v: &Value, name: &str, value: &str) -> Value {
    match v {
        Value::String(s) if s.strip_prefix('$') == Some(name) => Value::String(value.to_string()),
        Value::Array(a) => Value::Array(a.iter().map(|x| fill(x, name, value)).collect()),
        Value::Object(o) => Value::Object(o.iter().map(|(k, x)| (k.clone(), fill(x, name, value))).collect()),
        other => other.clone(),
    }
}

/// `dialect.resolve_target`: the target glob placed from `base` (None
/// for no base).
pub fn resolve_target(target: &str, base: Option<&str>) -> Result<String, UnsupportedGlob> {
    let Some(base) = base else { return Ok(target.to_string()) };
    if target.starts_with('/') || target.starts_with("**") {
        return Ok(target.to_string());
    }
    if base.contains(['*', '?', '[', ']']) {
        return Err(UnsupportedGlob("the working directory holds a glob character".into()));
    }
    if target.starts_with('~') {
        return Err(UnsupportedGlob("a target that starts with ~ cannot be placed from the cwd".into()));
    }
    if !target.ends_with("/**") && target.split('/').any(|s| s == "." || s == "..") {
        return Err(UnsupportedGlob("a relative target with a . or .. segment cannot be placed".into()));
    }
    Ok(format!("{}/{target}", base.trim_end_matches('/')))
}

/// `verify._dialect_base`: an absolute path, normalized, else None.
pub fn dialect_base(base: Option<&str>) -> Option<String> {
    base.filter(|b| b.starts_with('/')).map(normpath)
}

/// The JSON of `dialect.unfold(word, target, base)` before
/// `parse_expression`.
pub fn unfold_json(word: &str, target: &str, base: Option<&str>) -> Result<Value, UnsupportedGlob> {
    let (_, param, _, body) = WORDS.iter().find(|(w, ..)| *w == word).expect("a word of the table");
    let regex = glob_regex(&resolve_target(target, base)?)?;
    let body: Value = serde_json::from_str(body).expect("the generated body is JSON");
    Ok(fill(&body, param, &regex))
}

/// A Python pattern for the `regex` crate: `\Z` (the end of the string)
/// is `\z` there, read in one pass so an escaped backslash stays one.
pub fn py_pattern(pattern: &str) -> String {
    let mut out = String::with_capacity(pattern.len());
    let mut chars = pattern.chars();
    while let Some(c) = chars.next() {
        if c != '\\' {
            out.push(c);
            continue;
        }
        match chars.next() {
            Some('Z') => out.push_str("\\z"),
            Some(n) => {
                out.push('\\');
                out.push(n);
            }
            None => out.push('\\'),
        }
    }
    out
}

const SANCTIONED_WRITE_SINKS: &[&str] = &["/dev/null", "/dev/stdout", "/dev/stderr"];
const MAX_INTERPRETER_DEPTH: u32 = 4;

thread_local! {
    static PARSER: RefCell<Option<ShellParser>> = const { RefCell::new(None) };
}

/// `write_paths.step_write_paths` over a step's JSON, with no stack bound
/// on the shell decomposition: the paths, or None when unknown. With a
/// base each relative path resolves against it.
pub fn step_write_paths(step: &Value, base: Option<&str>) -> Option<Vec<String>> {
    let paths = match step.get("type").and_then(Value::as_str) {
        Some("file_write") => vec![normpath(step.get("path")?.as_str()?)],
        Some("shell") => shell_writes(step.get("command")?.as_str()?, 0, false)?,
        _ => return Some(vec![]),
    };
    match base {
        None => Some(paths),
        Some(b) => resolve_writes(&paths, b),
    }
}

/// `write_paths.resolve_writes`: each relative path resolved against base;
/// None when a path starts with `~`.
pub fn resolve_writes(paths: &[String], base: &str) -> Option<Vec<String>> {
    let mut out = Vec::with_capacity(paths.len());
    for p in paths {
        if p.starts_with('/') {
            out.push(p.clone());
        } else if p.starts_with('~') {
            return None;
        } else {
            out.push(normpath(&format!("{base}/{p}")));
        }
    }
    Some(out)
}

/// `write_paths.CWD_HEADS`: a head that moves the line's cwd.
pub fn moves_cwd<S: AsRef<str>>(heads: &[S]) -> bool {
    heads.iter().any(|h| matches!(h.as_ref(), "cd" | "pushd" | "popd"))
}

/// `write_paths._has_relative`.
pub fn has_relative(paths: &[String]) -> bool {
    paths.iter().any(|p| !p.starts_with('/'))
}

/// `write_paths._ADDS_OPERANDS`: the wrappers that add operands the line
/// does not show.
pub fn adds_operands(head: &str) -> bool {
    head == "xargs" || head == "find"
}

fn shell_writes(command: &str, depth: u32, adds: bool) -> Option<Vec<String>> {
    if depth > MAX_INTERPRETER_DEPTH {
        return None;
    }
    let stripped = command.trim();
    if stripped.is_empty() {
        return Some(vec![]);
    }
    if !crate::verify::shell_metachar_hit(command) {
        return payload_writes(stripped, depth, adds);
    }
    let d = PARSER.with(|p| p.borrow_mut().get_or_insert_with(ShellParser::new).decompose(command));
    if !d.ok {
        return None;
    }
    let mut out: Vec<String> =
        d.writes.iter().filter(|w| !SANCTIONED_WRITE_SINKS.contains(&w.as_str())).map(|w| normpath(w)).collect();
    for simple in &d.commands {
        out.extend(payload_writes(simple, depth, adds)?);
    }
    if moves_cwd(&d.heads) && has_relative(&out) {
        return None;
    }
    Some(out)
}

fn payload_writes(command: &str, depth: u32, adds: bool) -> Option<Vec<String>> {
    let p = match parse_interpreter(command) {
        Some(p) if !p.opaque => p,
        _ => return operand_writes(command, adds),
    };
    let inner_adds = adds || adds_operands(&p.head);
    let mut out = Vec::new();
    for inner in &p.inner_commands {
        out.extend(shell_writes(inner, depth + 1, inner_adds)?);
    }
    Some(out)
}

fn is_env_assign(tok: &str) -> bool {
    let b = tok.as_bytes();
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

/// `write_paths.operand_writes`: the normalized files one simple command
/// writes through its operands, or None when unknown. `adds` is true under
/// xargs or find -exec. A command not in the writer table writes none.
pub fn operand_writes(command: &str, adds: bool) -> Option<Vec<String>> {
    let tokens = shlex_split(command).ok()?;
    let mut i = 0;
    while i < tokens.len() && is_env_assign(&tokens[i]) {
        i += 1;
    }
    if i == tokens.len() {
        return Some(vec![]);
    }
    let head = tokens[i].rsplit('/').next().unwrap_or("");
    let Some((_, spec)) = WRITERS.iter().find(|(n, _)| *n == head) else { return Some(vec![]) };
    if adds || unsafe_words(command) {
        return None;
    }
    let raw = writer_paths(head, spec, &tokens[i + 1..])?;
    Some(raw.iter().filter(|p| !SANCTIONED_WRITE_SINKS.contains(&p.as_str())).map(|p| normpath(p)).collect())
}

/// `write_paths._parse_flags`: the flags (name and value) and the operands,
/// or None when a flag is outside spec or lacks its value.
#[allow(clippy::type_complexity)]
fn parse_flags(args: &[String], spec: &WriterSpec) -> Option<(Vec<(String, Option<String>)>, Vec<String>)> {
    let mut opts = Vec::new();
    let mut ops = Vec::new();
    let mut i = 0;
    let mut ended = false;
    while i < args.len() {
        let a = &args[i];
        i += 1;
        if ended || a == "-" || !a.starts_with('-') {
            ops.push(a.clone());
            continue;
        }
        if a == "--" {
            ended = true;
            continue;
        }
        if a.starts_with("--") {
            if let Some((name, value)) = a.split_once('=') {
                if spec.long_value.contains(&name) || spec.long_optional.contains(&name) {
                    opts.push((name.to_string(), Some(value.to_string())));
                    continue;
                }
                return None;
            }
            if spec.long.contains(&a.as_str()) || spec.long_optional.contains(&a.as_str()) {
                opts.push((a.clone(), None));
                continue;
            }
            if spec.long_value.contains(&a.as_str()) {
                let v = args.get(i)?.clone();
                i += 1;
                opts.push((a.clone(), Some(v)));
                continue;
            }
            return None;
        }
        let body = &a[1..];
        let bytes = body.as_bytes();
        for (j, &c) in bytes.iter().enumerate() {
            if !c.is_ascii() {
                return None;
            }
            let ch = c as char;
            if spec.short.contains(ch) {
                opts.push((format!("-{ch}"), None));
                continue;
            }
            if spec.short_optional.contains(ch) {
                opts.push((format!("-{ch}"), Some(body[j + 1..].to_string())));
                break;
            }
            if spec.short_value.contains(ch) {
                let rest = &body[j + 1..];
                let v = if rest.is_empty() {
                    let v = args.get(i)?.clone();
                    i += 1;
                    v
                } else {
                    rest.to_string()
                };
                opts.push((format!("-{ch}"), Some(v)));
                break;
            }
            return None;
        }
    }
    Some((opts, ops))
}

fn write_basename(p: &str) -> &str {
    p.trim_end_matches('/').rsplit('/').next().unwrap_or("")
}

/// `write_paths._under`: dest, then dest/basename(source).
fn write_under(dest: &str, sources: &[String]) -> Vec<String> {
    let mut out = vec![dest.to_string()];
    for s in sources {
        let b = write_basename(s);
        if !matches!(b, "" | "." | "..") {
            out.push(format!("{dest}/{b}"));
        }
    }
    out
}

/// `write_paths._is_remote`.
fn is_remote(word: &str) -> bool {
    if word.starts_with("rsync://") {
        return true;
    }
    match word.find(':') {
        Some(c) => c > 0 && !word[..c].contains('/'),
        None => false,
    }
}

/// `write_paths._writer_paths`: the raw paths one writing command writes.
fn writer_paths(head: &str, spec: &WriterSpec, args: &[String]) -> Option<Vec<String>> {
    if spec.rule == "dd" {
        let mut out = Vec::new();
        for a in args {
            let (key, value) = a.split_once('=')?;
            if a.starts_with('-') || !DD_KEYS.contains(&key) {
                return None;
            }
            if key == "of" {
                out.push(value.to_string());
            }
        }
        return Some(out);
    }
    let (opts, ops) = parse_flags(args, spec)?;
    let has = |names: &[&str]| opts.iter().any(|(n, _)| names.contains(&n.as_str()));
    if spec.rule == "all" {
        return Some(ops);
    }
    if spec.rule == "sed" {
        let inplace: Vec<&Option<String>> =
            opts.iter().filter(|(n, _)| n == "-i" || n == "--in-place").map(|(_, v)| v).collect();
        let Some(last) = inplace.last() else { return Some(vec![]) };
        let files: Vec<String> = if has(&["-e", "--expression", "-f", "--file"]) {
            ops
        } else {
            ops.into_iter().skip(1).collect()
        };
        let suffix = (**last).clone().unwrap_or_default();
        if suffix.is_empty() {
            return Some(files);
        }
        if suffix.contains(['*', '/']) {
            return None;
        }
        let mut out = files.clone();
        out.extend(files.iter().map(|f| format!("{f}{suffix}")));
        return Some(out);
    }
    if spec.rule == "install" && has(&["-d", "--directory"]) {
        return Some(ops);
    }
    let target: Vec<String> = opts
        .iter()
        .filter(|(n, _)| n == "-t" || n == "--target-directory")
        .map(|(_, v)| v.clone().unwrap_or_default())
        .collect();
    let no_target = has(&["-T", "--no-target-directory"]);
    if !target.is_empty() && no_target {
        return None;
    }
    let (dest, sources): (String, Vec<String>) = if let Some(t) = target.last() {
        (t.clone(), ops)
    } else if head == "ln" && ops.len() == 1 {
        return Some(vec![write_basename(&ops[0]).to_string()]);
    } else if ops.len() < 2 {
        return Some(vec![]);
    } else {
        let (last, rest) = ops.split_last().expect("two operands");
        (last.clone(), rest.to_vec())
    };
    if spec.rule == "rsync" {
        let mut out: Vec<String> = if has(&["--remove-source-files"]) {
            sources.iter().filter(|s| !is_remote(s)).cloned().collect()
        } else {
            vec![]
        };
        if !is_remote(&dest) {
            out.extend(write_under(&dest, &sources));
        }
        return Some(out);
    }
    let written = if no_target { vec![dest] } else { write_under(&dest, &sources) };
    if spec.rule == "move" {
        let mut out = sources;
        out.extend(written);
        return Some(out);
    }
    Some(written)
}

fn is_shell_space(c: u8) -> bool {
    matches!(c, b' ' | b'\t' | b'\n' | b'\r' | 0x0b | 0x0c | 0x1c..=0x1f)
}

/// `effects._unsafe_words`: true when the shell would change a word before
/// the command sees it: an unquoted glob or brace, a ~+, ~- or ~user at
/// the start of a word, or a $ or backtick anywhere.
pub fn unsafe_words(text: &str) -> bool {
    let b = text.as_bytes();
    let n = b.len();
    let mut i = 0;
    let mut start = true;
    let mut quote = 0u8;
    while i < n {
        let c = b[i];
        if quote == b'\'' {
            if c == b'\'' {
                quote = 0;
            }
            i += 1;
            continue;
        }
        if quote == b'"' {
            if c == b'\\' {
                i += 2;
                continue;
            }
            if c == b'"' {
                quote = 0;
            } else if c == b'$' || c == b'`' {
                return true;
            }
            i += 1;
            continue;
        }
        if c == b'\\' {
            i += 2;
            start = false;
            continue;
        }
        if c == b'\'' || c == b'"' {
            quote = c;
            start = false;
            i += 1;
            continue;
        }
        if is_shell_space(c) {
            start = true;
            i += 1;
            continue;
        }
        if b"$`*?[{}".contains(&c) {
            return true;
        }
        if c == b'~' && start && i + 1 < n && b[i + 1] != b'/' && !is_shell_space(b[i + 1]) {
            return true;
        }
        start = false;
        i += 1;
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn step_write_paths_match_the_oracle() {
        let raw = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/dialect/writes.json")).unwrap();
        let fx: Value = serde_json::from_str(&raw).unwrap();
        let base = fx["base"].as_str().unwrap();
        let want = |v: &Value| -> Option<Vec<String>> {
            v.as_array().map(|a| a.iter().map(|p| p.as_str().unwrap().to_string()).collect())
        };
        for c in fx["commands"].as_array().unwrap() {
            let command = c["command"].as_str().unwrap();
            let step = serde_json::json!({"id": "s1", "type": "shell", "command": command});
            assert_eq!(step_write_paths(&step, None), want(&c["writes"]), "{command:?}");
            assert_eq!(step_write_paths(&step, Some(base)), want(&c["placed"]), "{command:?} placed");
        }
    }

    #[test]
    fn resolve_target_places_a_relative_glob() {
        assert_eq!(resolve_target("src/**", Some("/repo")).unwrap(), "/repo/src/**");
        assert_eq!(resolve_target("**", Some("/repo")).unwrap(), "**");
        assert_eq!(resolve_target("src/**", None).unwrap(), "src/**");
        assert_eq!(resolve_target("src/**", Some("/")).unwrap(), "/src/**");
        assert!(resolve_target("src/**", Some("/r*")).is_err());
        assert!(resolve_target("./a.py", Some("/r")).is_err());
    }

    #[test]
    fn the_hash_is_the_hash_of_the_definitions() {
        assert_eq!(crate::gateway::route::sha256_hex16(DIALECT_JSON.as_bytes()), DIALECT_HASH);
    }

    #[test]
    fn glob_regex_matches_the_oracle() {
        let raw = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/dialect/globs.json")).unwrap();
        let fx: Value = serde_json::from_str(&raw).unwrap();
        let paths: Vec<&str> = fx["paths"].as_array().unwrap().iter().map(|p| p.as_str().unwrap()).collect();
        for g in fx["globs"].as_array().unwrap() {
            let glob = g["glob"].as_str().unwrap();
            let got = glob_regex(glob);
            if let Some(err) = g["error"].as_str() {
                assert_eq!(got, Err(UnsupportedGlob(err.to_string())), "{glob:?}");
                continue;
            }
            let regex = got.unwrap();
            assert_eq!(regex, g["regex"].as_str().unwrap(), "{glob:?}");
            let re = regex::Regex::new(&py_pattern(&regex)).unwrap();
            let want: Vec<&str> = g["matches"].as_array().unwrap().iter().map(|p| p.as_str().unwrap()).collect();
            for p in &paths {
                assert_eq!(re.is_match(p), want.contains(p), "glob {glob:?} regex {regex:?} path {p:?}");
            }
        }
    }

    #[test]
    fn synonyms_name_one_word() {
        for n in ["file_unchanged", "read_only", "no_modifications", "file_immutable", "file_preservation", "keep_unchanged"] {
            assert_eq!(word_for(n), Some("keep_unchanged"), "{n}");
        }
        assert_eq!(word_for("no_force_push"), None);
    }

    #[test]
    fn py_pattern_bridges_the_end_of_string() {
        assert_eq!(py_pattern("^a\\Z"), "^a\\z");
        assert_eq!(py_pattern("\\\\Z"), "\\\\Z");
        assert_eq!(py_pattern("a\\.b\\"), "a\\.b\\");
    }
}
