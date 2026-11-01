//! The system dialect (`opendaisugi/dialect.py`): the words, the glob
//! translation that fills their typed hole, and the write paths of one
//! step that `forall_writes` reads (`opendaisugi/write_paths.py`). The
//! tables are in `dialect_gen.rs`, written from the oracle by
//! `clients/gen_dialect.py`.

use crate::dialect_gen::{
    WriterSpec, CWD_MOVERS, DD_KEYS, FIND_ARGS, MAX_GLOB_CHARS, MAX_GLOB_DOUBLE_STARS, MAX_GLOB_STARS, READERS, SYNONYMS,
    TAR_FLAGS, TAR_LONG, TAR_LONG_VALUE, TAR_MODES, TAR_VALUE, UNKNOWN_REASONS, WORDS, WRITERS,
};
use crate::glob_engine::normpath;
use crate::interpreter_parse::{parse_interpreter, shlex_split};
use crate::shell_decompose::{Decomposition, ShellParser};
use crate::warden::frames::frame;
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
const SANCTIONED_READ_SOURCES: &[&str] = &["/dev/null", "/dev/stdin"];
const MAX_INTERPRETER_DEPTH: u32 = 4;
const PARSER_MISSING: &str = "shell decomposition parser not installed (opendaisugi[shell])";

thread_local! {
    static PARSER: RefCell<Option<ShellParser>> = const { RefCell::new(None) };
}

// The reasons, by their place in `write_paths.UNKNOWN_REASONS`.
const R_GRAMMAR: usize = 0;
const R_EXTRA: usize = 1;
const R_DEPTH: usize = 2;
const R_SPLIT: usize = 3;
const R_UNSAFE: usize = 4;
const R_FLAG: usize = 5;
const R_XARGS: usize = 6;
const R_FIND_EXEC: usize = 7;
const R_EXECDIR: usize = 8;
const R_MOVES: usize = 9;
const R_TILDE: usize = 10;
const R_FIND_ARG: usize = 11;
const R_TAR: usize = 12;
const R_RUNS: usize = 13;
const R_EMBED: usize = 14;
const R_REMOTE_TAR: usize = 15;
const R_OPTION: usize = 16;
const R_SED: usize = 17;

/// `write_paths.RUNNERS`: the heads that run the command they are given,
/// which the placer cannot read. Allowlisting one allows any command it runs.
const RUNNERS: &[&str] = &[
    "exec", "eval", "builtin", "source", ".", "chroot", "flock", "busybox", "script", "parallel", "coproc", "trap",
    "sudo", "doas", "watch", "systemd-run", "screen", "unbuffer", "runuser", "taskset", "chrt", "prlimit",
    "unshare", "nsenter", "entr",
];

/// `write_paths.STDIN_RUNNERS`: heads that read their commands from standard
/// input, unknown even with no argument.
const STDIN_RUNNERS: &[&str] = &["batch", "expect"];

const TMUX_RUNS: &[&str] = &[
    "new-session", "new", "new-window", "neww", "split-window", "splitw", "send-keys", "send", "run-shell", "run",
    "respawn-pane", "respawnp", "respawn-window", "respawnw", "pipe-pane", "pipep", "if-shell", "if",
    "display-popup", "popup", "display-menu", "menu", "source-file", "source", "set-hook", "bind-key", "bind",
    "command-prompt", "confirm-before", "confirm", "choose-tree", "choose-client", "choose-buffer",
];

const VIM_HEADS: &[&str] = &["vim", "nvim", "vi", "view", "vimdiff", "ex", "rvim", "rview", "gvim"];
const SSH_VALUE: &str = "BbcDEeFIiJLlmOopQRSWw";
const GIT_VALUE_LONG: &[&str] = &["--git-dir", "--work-tree", "--namespace", "--super-prefix", "--attr-source"];

fn ssh_runs_text(v: &str) -> bool {
    let l = v.to_lowercase();
    ["command", "match", "include", "exec"].iter().any(|w| l.contains(w))
}

fn git_runs(args: &[String]) -> bool {
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_str();
        if !a.starts_with('-') {
            return false;
        }
        if a == "-C" || GIT_VALUE_LONG.contains(&a) {
            i += 2;
            continue;
        }
        if a.starts_with("-c") || a.starts_with("--config-env") || a.starts_with("--exec-path") {
            return true;
        }
        i += 1;
    }
    false
}

fn vim_runs(args: &[String]) -> bool {
    args.iter().any(|a| a.starts_with('+') || (a.starts_with('-') && a != "-"))
}

fn ssh_runs(args: &[String]) -> bool {
    let mut i = 0;
    let mut host = false;
    let mut ended = false;
    while i < args.len() {
        let a = args[i].as_str();
        i += 1;
        if !ended && a == "--" {
            ended = true;
            continue;
        }
        if !ended && a.starts_with('-') && a != "-" {
            let body = &a[1..];
            for (j, c) in body.char_indices() {
                if !SSH_VALUE.contains(c) {
                    continue;
                }
                let rest = &body[j + c.len_utf8()..];
                let mut value = rest;
                if rest.is_empty() && i < args.len() {
                    value = args[i].as_str();
                }
                if rest.is_empty() {
                    i += 1;
                }
                if c == 'F' || c == 'E' || (c == 'o' && ssh_runs_text(value)) {
                    return true;
                }
                break;
            }
            continue;
        }
        if host {
            return true;
        }
        host = true;
    }
    false
}

fn su_runs(args: &[String]) -> bool {
    args.iter().any(|a| {
        a.starts_with("--command")
            || a.starts_with("--session-command")
            || (a.starts_with('-') && !a.starts_with("--") && a.contains('c'))
    })
}

fn tmux_runs(args: &[String]) -> bool {
    let mut i = 0;
    let n = args.len();
    let mut start = true;
    let mut first = true;
    while i < n {
        let a = args[i].as_str();
        i += 1;
        if first && a.starts_with('-') && a != "-" {
            let body = &a[1..];
            let last = body.chars().count();
            for (j, c) in body.chars().enumerate() {
                if c == 'c' || c == 'f' {
                    return true;
                }
                if c == 'L' || c == 'S' || c == 'T' {
                    if j + 1 == last {
                        i += 1;
                    }
                    break;
                }
            }
            continue;
        }
        first = false;
        if a == ";" {
            start = true;
            continue;
        }
        if start {
            start = false;
            if TMUX_RUNS.iter().any(|name| name.starts_with(a)) {
                return true;
            }
        }
    }
    false
}

/// `write_paths.runs_a_command`.
fn runs_a_command(head: &str, raw_head: &str, args: &[String]) -> bool {
    if STDIN_RUNNERS.contains(&head) {
        return true;
    }
    if RUNNERS.contains(&head) || RUNNERS.contains(&raw_head) {
        return !args.is_empty();
    }
    match head {
        "git" => git_runs(args),
        "ssh" => ssh_runs(args),
        "su" => su_runs(args),
        "tmux" => tmux_runs(args),
        "crontab" | "at" => !(args.len() == 1 && args[0] == "-l"),
        h if VIM_HEADS.contains(&h) => vim_runs(args),
        _ => false,
    }
}

fn reason(i: usize, head: &str) -> String {
    UNKNOWN_REASONS[i].replace("{head}", head)
}

/// `write_paths.Access`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Access {
    pub path: String,
    pub tree: bool,
    /// "operand" or "redirect".
    pub via: &'static str,
}

/// `write_paths.Accesses`; None is known.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Accesses {
    pub reads: Vec<Access>,
    pub writes: Vec<Access>,
    pub reads_unknown: Option<String>,
    pub writes_unknown: Option<String>,
}

struct Raw {
    path: String,
    tree: bool,
    via: &'static str,
    bases: Vec<String>,
}

type Dec<'a, E> = &'a dyn Fn(&str) -> Result<Decomposition, E>;

struct Ctx<'a, E> {
    reads: Vec<Raw>,
    writes: Vec<Raw>,
    ru: Option<String>,
    wu: Option<String>,
    movers: u32,
    dec: Dec<'a, E>,
}

impl<E> Ctx<'_, E> {
    fn unknown_reads(&mut self, why: String) {
        if self.ru.is_none() {
            self.ru = Some(why);
        }
    }
    fn unknown_writes(&mut self, why: String) {
        if self.wu.is_none() {
            self.wu = Some(why);
        }
    }
    fn unknown(&mut self, why: &str, reads: bool, writes: bool) {
        if reads {
            self.unknown_reads(why.to_string());
        }
        if writes {
            self.unknown_writes(why.to_string());
        }
    }
}

/// `write_paths._norm`: normpath keeps a leading "//", and on Linux "//etc"
/// is "/etc".
fn acc_norm(path: &str) -> String {
    let out = normpath(path);
    if out.starts_with("//") {
        format!("/{}", out.trim_start_matches('/'))
    } else {
        out
    }
}

fn join_base(base: &str, path: &str) -> String {
    if path.starts_with('/') || base.is_empty() {
        acc_norm(path)
    } else {
        acc_norm(&format!("{base}/{path}"))
    }
}

fn finalize<E>(mut c: Ctx<'_, E>, has_base: bool) -> Accesses {
    let rel = c.reads.iter().chain(c.writes.iter()).any(|a| !a.path.starts_with('/'));
    if c.movers > 0 && rel {
        c.unknown(UNKNOWN_REASONS[R_MOVES], true, true);
    }
    let mut tilde = [false, false];
    let mut place = |raw: &[Raw], sinks: &[&str], which: usize| -> Vec<Access> {
        let mut out: Vec<Access> = Vec::new();
        for r in raw {
            for base in &r.bases {
                if r.path.starts_with('~') && has_base {
                    tilde[which] = true;
                    continue;
                }
                let full = join_base(base, &r.path);
                if sinks.contains(&full.as_str()) {
                    continue;
                }
                let a = Access { path: full, tree: r.tree, via: r.via };
                if !out.contains(&a) {
                    out.push(a);
                }
            }
        }
        out
    };
    let reads = place(&c.reads, SANCTIONED_READ_SOURCES, 0);
    let writes = place(&c.writes, SANCTIONED_WRITE_SINKS, 1);
    if tilde[0] {
        c.unknown(UNKNOWN_REASONS[R_TILDE], true, false);
    }
    if tilde[1] {
        c.unknown(UNKNOWN_REASONS[R_TILDE], false, true);
    }
    Accesses { reads, writes, reads_unknown: c.ru, writes_unknown: c.wu }
}

fn plain_decompose(command: &str) -> Result<Decomposition, std::convert::Infallible> {
    Ok(PARSER.with(|p| p.borrow_mut().get_or_insert_with(ShellParser::new).decompose(command)))
}

/// `write_paths.shell_accesses` with the decomposer: the reads and writes of
/// one shell line placed from `base`, or why they are unknown. Opens the
/// frames Python opens, so a decomposer that reads the stack depth sees the
/// oracle's.
pub fn shell_accesses_with<E>(command: &str, base: Option<&str>, dec: Dec<'_, E>) -> Result<Accesses, E> {
    let _f = frame();
    let mut c = Ctx { reads: vec![], writes: vec![], ru: None, wu: None, movers: 0, dec };
    level(command, 0, &[base.unwrap_or("").to_string()], None, &mut c)?;
    Ok(finalize(c, base.is_some_and(|b| !b.is_empty())))
}

/// `write_paths.shell_accesses` with no stack bound on the decomposition.
pub fn shell_accesses(command: &str, base: Option<&str>) -> Accesses {
    match shell_accesses_with(command, base, &plain_decompose) {
        Ok(a) => a,
        Err(never) => match never {},
    }
}

/// `write_paths.operand_accesses`: one simple command. `under` is None,
/// "xargs", "find-exec" or "find-execdir".
pub fn operand_accesses(command: &str, under: Option<&str>) -> Accesses {
    let dec: Dec<'_, std::convert::Infallible> = &plain_decompose;
    let mut c = Ctx { reads: vec![], writes: vec![], ru: None, wu: None, movers: 0, dec };
    let Ok(()) = simple(command, 0, &[String::new()], under, &mut c, false);
    finalize(c, false)
}

/// `write_paths.step_accesses` with the decomposer, over the step's type,
/// path and command.
pub fn step_accesses_with<E>(
    kind: Option<&str>,
    path: Option<&str>,
    command: Option<&str>,
    base: Option<&str>,
    dec: Dec<'_, E>,
) -> Result<Accesses, E> {
    let _f = frame();
    let bad = || Accesses {
        reads_unknown: Some(UNKNOWN_REASONS[R_SPLIT].into()),
        writes_unknown: Some(UNKNOWN_REASONS[R_SPLIT].into()),
        ..Accesses::default()
    };
    match kind {
        Some(k @ ("file_write" | "file_read")) => {
            let Some(p) = path else { return Ok(bad()) };
            let mut c = Ctx { reads: vec![], writes: vec![], ru: None, wu: None, movers: 0, dec };
            let entry = Raw { path: p.to_string(), tree: false, via: "operand", bases: vec![base.unwrap_or("").to_string()] };
            if k == "file_write" {
                c.writes.push(entry);
            } else {
                c.reads.push(entry);
            }
            Ok(finalize(c, base.is_some_and(|b| !b.is_empty())))
        }
        Some("shell") => {
            let Some(cmd) = command else { return Ok(bad()) };
            shell_accesses_with(cmd, base, dec)
        }
        _ => Ok(Accesses::default()),
    }
}

/// `write_paths.step_write_paths` over a step's JSON, with no stack bound
/// on the shell decomposition: the paths, or None when unknown. With a
/// base each relative path resolves against it.
pub fn step_write_paths(step: &Value, base: Option<&str>) -> Option<Vec<String>> {
    let kind = step.get("type").and_then(Value::as_str);
    let path = step.get("path").and_then(Value::as_str);
    let command = step.get("command").and_then(Value::as_str);
    step_write_paths_of(kind, path, command, base, &plain_decompose).unwrap_or_else(|never| match never {})
}

/// `write_paths.step_write_paths` over the parts of a step, with the
/// decomposer. The frame of `step_write_paths` itself is the caller's.
pub fn step_write_paths_of<E>(
    kind: Option<&str>,
    path: Option<&str>,
    command: Option<&str>,
    base: Option<&str>,
    dec: Dec<'_, E>,
) -> Result<Option<Vec<String>>, E> {
    if kind == Some("file_write") && path.is_none() {
        return Ok(None);
    }
    if kind == Some("shell") && command.is_none() {
        return Ok(None);
    }
    let acc = step_accesses_with(kind, path, command, base, dec)?;
    if acc.writes_unknown.is_some() {
        return Ok(None);
    }
    Ok(Some(acc.writes.into_iter().map(|a| a.path).collect()))
}

/// `write_paths.operand_writes`: the normalized files one simple command
/// writes through its operands, or None when unknown. `adds` is true under
/// xargs or find -exec. A command not in the writer table writes none.
pub fn operand_writes(command: &str, adds: bool) -> Option<Vec<String>> {
    let acc = operand_accesses(command, if adds { Some("xargs") } else { None });
    if acc.writes_unknown.is_some() {
        return None;
    }
    Some(acc.writes.into_iter().map(|a| a.path).collect())
}

fn head_of(tokens: &[String]) -> (usize, &str, &str) {
    let mut i = 0;
    while i < tokens.len() && is_env_assign(&tokens[i]) {
        i += 1;
    }
    if i == tokens.len() {
        return (i, "", "");
    }
    let raw = tokens[i].as_str();
    (i, raw, raw.rsplit('/').next().unwrap_or(""))
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

/// `write_paths._level`.
fn level<E>(command: &str, depth: u32, bases: &[String], under: Option<&str>, c: &mut Ctx<'_, E>) -> Result<(), E> {
    let _f = frame();
    if depth > MAX_INTERPRETER_DEPTH {
        c.unknown(UNKNOWN_REASONS[R_DEPTH], true, true);
        return Ok(());
    }
    let stripped = command.trim();
    if stripped.is_empty() {
        return Ok(());
    }
    let mut cmds: Vec<String> = vec![stripped.to_string()];
    let mut decomp: Option<Decomposition> = None;
    if crate::verify::shell_metachar_hit(command) {
        let d = (c.dec)(command)?;
        if !d.ok {
            let why = if d.reason == PARSER_MISSING { R_EXTRA } else { R_GRAMMAR };
            c.unknown(UNKNOWN_REASONS[why], true, true);
            return Ok(());
        }
        cmds = d.commands.clone();
        decomp = Some(d);
    }
    let mut bases: Vec<String> = bases.to_vec();
    let mut lead = false;
    if let Some(lit) = leading_cd(&cmds, stripped) {
        if bases.len() == 1 {
            let second = join_base(&bases[0], &lit);
            bases.push(second);
            lead = true;
        }
    }
    if let Some(d) = &decomp {
        for p in &d.writes {
            c.writes.push(Raw { path: p.clone(), tree: false, via: "redirect", bases: bases.clone() });
        }
        for p in &d.reads {
            c.reads.push(Raw { path: p.clone(), tree: false, via: "redirect", bases: bases.clone() });
        }
    }
    for (n, s) in cmds.iter().enumerate() {
        simple(s, depth, &bases, under, c, n == 0 && lead)?;
    }
    Ok(())
}

/// `write_paths._leading_cd`: the literal operand of a first command that is
/// `cd LIT`.
fn leading_cd(cmds: &[String], line: &str) -> Option<String> {
    let first = cmds.first()?;
    if !line.starts_with(first.as_str()) {
        return None;
    }
    let tokens = shlex_split(first).ok()?;
    if tokens.len() != 2 || tokens[0] != "cd" || unsafe_words(first) {
        return None;
    }
    let lit = &tokens[1];
    if lit.is_empty() || lit.starts_with('-') || lit.starts_with('~') {
        return None;
    }
    Some(lit.clone())
}

const ENV_VALUE_SHORT: &str = "uCSa";

fn is_mover(head: &str, raw_head: &str, args: &[String]) -> bool {
    if CWD_MOVERS.contains(&head) || CWD_MOVERS.contains(&raw_head) {
        return true;
    }
    if head != "env" {
        return false;
    }
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_str();
        i += 1;
        if a.starts_with("--") {
            let Some((name, value, kind)) = crate::interpreter_parse::resolve_long(a, crate::interpreter_parse::ENV_LONG)
            else {
                return false; // the payload is opaque: the line is unknown anyway
            };
            if name == "--chdir" || name == "--split-string" {
                return true;
            }
            if kind == 'v' && value.is_none() {
                i += 1;
            }
        } else if a.starts_with('-') && a != "-" {
            let n = a.chars().count();
            for (j, c) in a.chars().enumerate().skip(1) {
                if c == 'C' || c == 'S' {
                    return true;
                }
                if ENV_VALUE_SHORT.contains(c) {
                    if j == n - 1 {
                        i += 1;
                    }
                    break;
                }
            }
        } else if !a.contains('=') {
            break;
        }
    }
    false
}

/// `write_paths._strip_process_substitution`: text without its `<(...)` and
/// `>(...)` words, which are inner commands the decomposition checks on
/// their own; None when a span does not close.
fn strip_process_substitution(text: &str) -> Option<String> {
    let b: Vec<char> = text.chars().collect();
    let n = b.len();
    let mut out = String::new();
    let mut quote: Option<char> = None;
    let mut i = 0;
    while i < n {
        let c = b[i];
        if let Some(q) = quote {
            out.push(c);
            if c == '\\' && q == '"' && i + 1 < n {
                out.push(b[i + 1]);
                i += 1;
            } else if c == q {
                quote = None;
            }
        } else if c == '\\' && i + 1 < n {
            out.push(c);
            out.push(b[i + 1]);
            i += 1;
        } else if c == '\'' || c == '"' {
            quote = Some(c);
            out.push(c);
        } else if (c == '<' || c == '>') && i + 1 < n && b[i + 1] == '(' {
            let mut depth = 0i32;
            let mut j = i + 1;
            let mut q: Option<char> = None;
            while j < n {
                let d = b[j];
                if let Some(qq) = q {
                    if d == '\\' && qq == '"' && j + 1 < n {
                        j += 1;
                    } else if d == qq {
                        q = None;
                    }
                } else if d == '\\' && j + 1 < n {
                    j += 1;
                } else if d == '\'' || d == '"' {
                    q = Some(d);
                } else if d == '(' {
                    depth += 1;
                } else if d == ')' {
                    depth -= 1;
                    if depth == 0 {
                        break;
                    }
                }
                j += 1;
            }
            if j >= n {
                return None;
            }
            out.push(' ');
            i = j;
        } else {
            out.push(c);
        }
        i += 1;
    }
    Some(out)
}

/// `write_paths._simple`.
fn simple<E>(
    text: &str,
    depth: u32,
    bases: &[String],
    under: Option<&str>,
    c: &mut Ctx<'_, E>,
    skip_mover: bool,
) -> Result<(), E> {
    let _f = frame();
    let Some(unsub) = strip_process_substitution(text) else {
        c.unknown(UNKNOWN_REASONS[R_SPLIT], true, true);
        return Ok(());
    };
    let Ok(tokens) = shlex_split(&unsub) else {
        c.unknown(UNKNOWN_REASONS[R_SPLIT], true, true);
        return Ok(());
    };
    let (i, raw_head, head) = head_of(&tokens);
    if head.is_empty() {
        return Ok(());
    }
    let args = &tokens[i + 1..];
    if !skip_mover && is_mover(head, raw_head, args) {
        c.movers += 1;
        if head == "env" {
            c.unknown(UNKNOWN_REASONS[R_MOVES], true, true);
            return Ok(());
        }
    }
    if runs_a_command(head, raw_head, args) {
        c.unknown(UNKNOWN_REASONS[R_RUNS], true, true);
        return Ok(());
    }
    let wrapped = if i > 0 { tokens[i..].iter().map(|t| crate::interpreter_parse::shlex_quote(t)).collect::<Vec<_>>().join(" ") } else { text.to_string() };
    let payload = match parse_interpreter(&wrapped) {
        Some(p) if !p.opaque => p,
        Some(p) => {
            if crate::interpreter_parse::is_option_head(&p.head) {
                c.unknown(UNKNOWN_REASONS[R_OPTION], true, true);
                return Ok(());
            }
            operands(head, args, text, bases, under, c);
            return Ok(());
        }
        None => {
            operands(head, args, text, bases, under, c);
            return Ok(());
        }
    };
    if head == "find" {
        find(args, text, bases, under, c);
    } else if head == "xargs" {
        xargs_files(args, bases, c);
    } else if head == "time" {
        time_files(args, bases, c);
    }
    for (n, inner) in payload.inner_commands.iter().enumerate() {
        let kind = payload.inner_kinds.get(n).map_or("", String::as_str);
        let inner_under = if under == Some("xargs") || head == "xargs" {
            Some("xargs")
        } else if head == "find" {
            Some(if kind == "execdir" { "find-execdir" } else { "find-exec" })
        } else {
            under
        };
        level(inner, depth + 1, bases, inner_under, c)?;
    }
    Ok(())
}

fn xargs_files<E>(args: &[String], bases: &[String], c: &mut Ctx<'_, E>) {
    let Some((_, opts)) = crate::interpreter_parse::xargs_options(args, 0) else { return };
    for (name, value) in opts {
        if name == "-a" || name == "--arg-file" {
            if let Some(v) = value {
                if v != "-" {
                    c.reads.push(Raw { path: v, tree: false, via: "operand", bases: bases.to_vec() });
                }
            }
        }
    }
}

/// `write_paths._time_files`: GNU `time -o FILE` writes FILE.
fn time_files<E>(args: &[String], bases: &[String], c: &mut Ctx<'_, E>) {
    let Some((_, opts)) = crate::interpreter_parse::wrapper_options("time", args, 0) else { return };
    for (name, value) in opts {
        if name == "-o" || name == "--output" {
            if let Some(v) = value {
                c.writes.push(Raw { path: v, tree: false, via: "operand", bases: bases.to_vec() });
            }
        }
    }
}

fn accesses_unsafe(text: &str) -> bool {
    unsafe_words(&text.replace("{}", "_"))
}

fn find_count(tok: &str) -> Option<i32> {
    FIND_ARGS.iter().find(|(n, _)| *n == tok).map(|(_, c)| *c)
}

type Pairs = Vec<(String, bool)>;

fn find<E>(args: &[String], text: &str, bases: &[String], under: Option<&str>, c: &mut Ctx<'_, E>) {
    if under == Some("xargs") {
        c.unknown(&reason(R_XARGS, "find"), true, true);
        return;
    }
    if accesses_unsafe(text) {
        c.unknown(UNKNOWN_REASONS[R_UNSAFE], true, true);
        return;
    }
    let mut i = 0;
    while i < args.len() && matches!(args[i].as_str(), "-H" | "-L" | "-P") {
        i += 1;
    }
    let mut starts: Vec<String> = Vec::new();
    while i < args.len() && find_count(&args[i]).is_none() && !args[i].starts_with('-') {
        starts.push(args[i].clone());
        i += 1;
    }
    if starts.is_empty() {
        starts.push(".".into());
    }
    let reads: Pairs = starts.iter().map(|s| (s.clone(), true)).collect();
    let mut writes: Pairs = Vec::new();
    let n = args.len();
    while i < n {
        let tok = args[i].as_str();
        let Some(count) = find_count(tok) else {
            c.unknown(UNKNOWN_REASONS[R_FIND_ARG], true, true);
            return;
        };
        if count < 0 {
            let mut j = i + 1;
            while j < n && args[j] != ";" && args[j] != "+" {
                j += 1;
            }
            i = j + 1;
            continue;
        }
        let count = count as usize;
        if i + count > n - 1 {
            c.unknown(UNKNOWN_REASONS[R_FIND_ARG], true, true);
            return;
        }
        match tok {
            "-delete" => writes.extend(starts.iter().map(|s| (s.clone(), true))),
            "-fprint" | "-fprint0" | "-fls" | "-fprintf" => writes.push((args[i + 1].clone(), false)),
            _ => {}
        }
        i += 1 + count;
    }
    for (p, t) in reads {
        c.reads.push(Raw { path: p, tree: t, via: "operand", bases: bases.to_vec() });
    }
    for (p, t) in writes {
        c.writes.push(Raw { path: p, tree: t, via: "operand", bases: bases.to_vec() });
    }
}

fn spec_of(head: &str) -> Option<(&'static WriterSpec, bool, bool)> {
    let w = WRITERS.iter().find(|(n, _)| *n == head).map(|(_, s)| s);
    let r = READERS.iter().find(|(n, _)| *n == head).map(|(_, s)| s);
    w.or(r).map(|s| (s, r.is_some(), w.is_some()))
}

fn operands<E>(head: &str, args: &[String], text: &str, bases: &[String], under: Option<&str>, c: &mut Ctx<'_, E>) {
    let Some((spec, is_r, is_w)) = spec_of(head) else { return };
    if under == Some("xargs") {
        c.unknown(&reason(R_XARGS, head), is_r, is_w);
        return;
    }
    if accesses_unsafe(text) {
        c.unknown(UNKNOWN_REASONS[R_UNSAFE], is_r, is_w);
        return;
    }
    let execing = matches!(under, Some("find-exec" | "find-execdir"));
    if execing {
        if args.iter().any(|a| a != "{}" && a.contains("{}")) {
            c.unknown(UNKNOWN_REASONS[R_EMBED], is_r, is_w);
            return;
        }
        if is_w {
            c.unknown_writes(UNKNOWN_REASONS[R_FIND_EXEC].to_string());
        }
    }
    let (mut reads, writes) = match head_accesses(head, spec, args) {
        Got::Ok(r, w) => (r, w),
        Got::Flag => {
            // A flag outside the closed set can read or write a file the table
            // does not know (sort -o FILE, less -o FILE): both are unknown.
            c.unknown(&reason(R_FLAG, head), true, true);
            return;
        }
        Got::Why(i) => {
            c.unknown(&reason(i, head), is_r, is_w);
            return;
        }
    };
    if execing {
        // a bare {} is the file find found: find's own start tree covers it
        reads.retain(|(p, _)| p != "{}");
    }
    if !is_r {
        reads.clear();
    }
    if !execing {
        for (p, t) in writes {
            c.writes.push(Raw { path: p, tree: t, via: "operand", bases: bases.to_vec() });
        }
    }
    for (p, t) in reads {
        if under == Some("find-execdir") && !p.starts_with('/') {
            if p.split('/').any(|s| s == "..") {
                c.unknown_reads(UNKNOWN_REASONS[R_EXECDIR].to_string());
            }
            continue;
        }
        c.reads.push(Raw { path: p, tree: t, via: "operand", bases: bases.to_vec() });
    }
}

enum Got {
    Ok(Pairs, Pairs),
    Flag,
    Why(usize),
}

fn has(opts: &[(String, Option<String>)], names: &[&str]) -> bool {
    opts.iter().any(|(n, _)| names.contains(&n.as_str()))
}

fn pairs(ops: &[String], tree: bool) -> Pairs {
    ops.iter().map(|o| (o.clone(), tree)).collect()
}

/// `write_paths._head_accesses`.
fn head_accesses(head: &str, spec: &WriterSpec, args: &[String]) -> Got {
    let rule = spec.rule;
    match rule {
        "tar" => return tar(args),
        "find" => return Got::Ok(vec![], vec![]),
        "dd" => {
            let (mut r, mut w) = (Vec::new(), Vec::new());
            for a in args {
                let Some((key, value)) = a.split_once('=') else { return Got::Flag };
                if a.starts_with('-') || !DD_KEYS.contains(&key) {
                    return Got::Flag;
                }
                if key == "if" {
                    r.push((value.to_string(), false));
                } else if key == "of" {
                    w.push((value.to_string(), false));
                }
            }
            return Got::Ok(r, w);
        }
        _ => {}
    }
    let Some((opts, mut ops)) = parse_flags(args, spec) else { return Got::Flag };
    if rule == "inout" {
        if ops.len() > 2 {
            return Got::Flag;
        }
        let r: Pairs = ops.iter().take(1).filter(|o| *o != "-").map(|o| (o.clone(), false)).collect();
        let w: Pairs = ops.iter().skip(1).filter(|o| *o != "-").map(|o| (o.clone(), false)).collect();
        return Got::Ok(r, w);
    }
    if !matches!(rule, "grep" | "rg" | "sed") {
        ops.retain(|o| o != "-");
    }
    match rule {
        "files" => return Got::Ok(pairs(&ops, false), vec![]),
        "diff" => return Got::Ok(pairs(&ops, has(&opts, &["-r", "--recursive"])), vec![]),
        "grep" | "rg" => return Got::Ok(grep(rule, &opts, &ops), vec![]),
        "ls" => {
            let rec = has(&opts, &["-R", "--recursive"]);
            if ops.is_empty() {
                ops.push(".".into());
            }
            return Got::Ok(pairs(&ops, rec), vec![]);
        }
        "all" => {
            let rec = head == "rm" && has(&opts, &["-r", "-R", "--recursive"]);
            return Got::Ok(vec![], pairs(&ops, rec));
        }
        "sed" => {
            let scripts: Vec<String> = opts
                .iter()
                .filter(|(n, _)| n == "-e" || n == "--expression")
                .map(|(_, v)| v.clone().unwrap_or_default())
                .collect();
            let has_file = has(&opts, &["-f", "--file"]);
            let script = if !scripts.is_empty() {
                scripts.join("\n")
            } else {
                ops.first().cloned().unwrap_or_default()
            };
            if has_file || sed_script_risky(&script) {
                return Got::Why(R_SED);
            }
            let inplace: Vec<&Option<String>> =
                opts.iter().filter(|(n, _)| n == "-i" || n == "--in-place").map(|(_, v)| v).collect();
            let scripted = has(&opts, &["-e", "--expression", "-f", "--file"]);
            let src: Vec<String> = if scripted { ops.clone() } else { ops.iter().skip(1).cloned().collect() };
            let files: Vec<String> = src.into_iter().filter(|f| f != "-").collect();
            let mut sreads: Pairs = opts
                .iter()
                .filter(|(n, _)| n == "-f" || n == "--file")
                .map(|(_, v)| (v.clone().unwrap_or_default(), false))
                .collect();
            sreads.extend(pairs(&files, false));
            let Some(last) = inplace.last() else { return Got::Ok(sreads, vec![]) };
            let suffix = (*last).clone().unwrap_or_default();
            if suffix.is_empty() {
                return Got::Ok(sreads, pairs(&files, false));
            }
            if suffix.contains(['*', '/']) {
                return Got::Flag;
            }
            let mut all = files.clone();
            all.extend(files.iter().map(|f| format!("{f}{suffix}")));
            return Got::Ok(sreads, pairs(&all, false));
        }
        "install" if has(&opts, &["-d", "--directory"]) => return Got::Ok(vec![], pairs(&ops, false)),
        _ => {}
    }
    let target: Vec<String> = opts
        .iter()
        .filter(|(n, _)| n == "-t" || n == "--target-directory")
        .map(|(_, v)| v.clone().unwrap_or_default())
        .collect();
    let no_target = has(&opts, &["-T", "--no-target-directory"]);
    if !target.is_empty() && no_target {
        return Got::Flag;
    }
    let (dest, sources): (String, Vec<String>) = if let Some(t) = target.last() {
        (t.clone(), ops)
    } else if head == "ln" && ops.len() == 1 {
        return Got::Ok(vec![], vec![(write_basename(&ops[0]).to_string(), false)]);
    } else if ops.len() < 2 {
        return Got::Ok(vec![], vec![]);
    } else {
        let (last, rest) = ops.split_last().expect("two operands");
        (last.clone(), rest.to_vec())
    };
    if rule == "rsync" {
        let local: Vec<String> = sources.iter().filter(|s| !is_remote(s)).cloned().collect();
        let mut rw: Pairs = if has(&opts, &["--remove-source-files"]) { pairs(&local, true) } else { vec![] };
        if !is_remote(&dest) {
            rw.extend(pairs(&write_under(&dest, &sources), true));
        }
        return Got::Ok(pairs(&local, true), rw);
    }
    if rule == "move" {
        let written = if no_target { vec![dest] } else { write_under(&dest, &sources) };
        let mut all = sources.clone();
        all.extend(written);
        return Got::Ok(pairs(&sources, true), pairs(&all, true));
    }
    let rec = head == "cp" && has(&opts, &["-r", "-R", "-a", "--recursive", "--archive"]);
    let wr: Pairs = if no_target {
        vec![(dest, rec)]
    } else {
        let mut w = vec![(dest.clone(), false)];
        w.extend(pairs(&write_under(&dest, &sources)[1..], rec));
        w
    };
    let rd: Pairs = if head == "cp" {
        pairs(&sources, rec)
    } else if rule == "install" {
        pairs(&sources, false)
    } else {
        vec![]
    };
    Got::Ok(rd, wr)
}

fn grep(rule: &str, opts: &[(String, Option<String>)], ops: &[String]) -> Pairs {
    let mut has_pattern = has(opts, &["-e", "--regexp", "-f", "--file"]);
    if rule == "rg" && has(opts, &["--files"]) {
        has_pattern = true;
    }
    let files: &[String] = if has_pattern { ops } else { ops.get(1..).unwrap_or(&[]) };
    let mut out: Pairs = opts
        .iter()
        .filter(|(n, v)| (n == "-f" || n == "--file") && v.as_deref() != Some("-"))
        .map(|(_, v)| (v.clone().unwrap_or_default(), false))
        .collect();
    let recursive = rule == "rg"
        || has(opts, &["-r", "-R", "--recursive", "--dereference-recursive"])
        || opts.iter().any(|(n, v)| (n == "-d" || n == "--directories") && v.as_deref() == Some("recurse"));
    let mut files: Vec<String> = files.iter().filter(|f| f.as_str() != "-").cloned().collect();
    if files.is_empty() && recursive {
        files.push(".".into());
    }
    out.extend(files.into_iter().map(|f| (f, recursive)));
    out
}

fn tar(args: &[String]) -> Got {
    let (mut modes, mut archive, mut dirs, mut ops): (Vec<char>, Vec<String>, Vec<String>, Vec<String>) =
        (vec![], vec![], vec![], vec![]);
    let n = args.len();
    let mut i = 0;
    let mut first = true;
    while i < n {
        let a = args[i].as_str();
        i += 1;
        if a == "--" {
            ops.extend(args[i..].iter().cloned());
            break;
        }
        if a.starts_with("--") {
            let (name, eq, value) = match a.split_once('=') {
                Some((nm, v)) => (nm, true, v.to_string()),
                None => (a, false, String::new()),
            };
            let long_mode = match name {
                "--create" => Some('c'),
                "--extract" => Some('x'),
                "--list" => Some('t'),
                _ => None,
            };
            if let (Some(m), false) = (long_mode, eq) {
                modes.push(m);
            } else if TAR_LONG.contains(&name) && !eq {
            } else if TAR_LONG_VALUE.contains(&name) {
                let mut value = value;
                if !eq {
                    if i >= n {
                        return Got::Flag;
                    }
                    value = args[i].clone();
                    i += 1;
                }
                if name == "--file" {
                    archive.push(value);
                } else if name == "--directory" {
                    if !ops.is_empty() {
                        return Got::Flag;
                    }
                    dirs.push(value);
                }
            } else {
                return Got::Flag;
            }
        } else if a.starts_with('-') && a != "-" {
            let body = &a[1..];
            let bytes = body.as_bytes();
            let mut j = 0;
            while j < bytes.len() {
                let ch = bytes[j] as char;
                j += 1;
                if TAR_MODES.contains(ch) {
                    modes.push(ch);
                } else if TAR_FLAGS.contains(ch) {
                } else if TAR_VALUE.contains(ch) {
                    let rest = &body[j..];
                    let value = if !rest.is_empty() {
                        rest.to_string()
                    } else if i < n {
                        i += 1;
                        args[i - 1].clone()
                    } else {
                        return Got::Flag;
                    };
                    if ch == 'f' {
                        archive.push(value);
                    } else {
                        if !ops.is_empty() {
                            return Got::Flag;
                        }
                        dirs.push(value);
                    }
                    break;
                } else {
                    return Got::Flag;
                }
            }
        } else if first && a != "-" {
            for ch in a.chars() {
                if TAR_MODES.contains(ch) {
                    modes.push(ch);
                } else if TAR_FLAGS.contains(ch) {
                } else if TAR_VALUE.contains(ch) {
                    if i >= n {
                        return Got::Flag;
                    }
                    let value = args[i].clone();
                    i += 1;
                    if ch == 'f' {
                        archive.push(value);
                    } else {
                        dirs.push(value);
                    }
                } else {
                    return Got::Flag;
                }
            }
        } else {
            ops.push(a.to_string());
        }
        first = false;
    }
    if archive.len() > 1 || dirs.len() > 1 {
        return Got::Flag;
    }
    if modes.len() != 1 || archive.is_empty() {
        return Got::Why(R_TAR);
    }
    let where_ = &archive[0];
    if is_remote(where_) {
        return Got::Why(R_REMOTE_TAR);
    }
    let stream = where_ == "-";
    let (mut reads, mut writes): (Pairs, Pairs) = (vec![], vec![]);
    if modes[0] == 'c' {
        if !stream {
            writes.push((where_.clone(), false));
        }
        for o in &ops {
            let o = match dirs.first() {
                Some(d) if !o.starts_with('/') => normpath(&format!("{d}/{o}")),
                _ => o.clone(),
            };
            reads.push((o, true));
        }
    } else {
        if !stream {
            reads.push((where_.clone(), false));
        }
        if modes[0] == 'x' {
            writes.push((dirs.first().cloned().unwrap_or_else(|| ".".into()), true));
        }
    }
    Got::Ok(reads, writes)
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
        assert_eq!(crate::router::route::sha256_hex16(DIALECT_JSON.as_bytes()), DIALECT_HASH);
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

// `write_paths.sed_script_risky` and its helpers: true when a sed script may
// read, write or run a file the placer cannot place (r, R, w, W, e, s flags w
// and e), or holds anything the reader does not understand.

const SED_DIGITS: &str = "0123456789";
const SED_PLAIN: &str = "=dDgGhHnNpPxzF";
const SED_NUMBERED: &str = "lqQL";
const SED_END: &str = " \t\n;}#";

fn sed_skip_regex(s: &[char], mut j: usize, delim: char) -> Option<usize> {
    let n = s.len();
    while j < n {
        let c = s[j];
        if c == '\\' {
            j += 2;
        } else if c == '[' {
            j += 1;
            if j < n && s[j] == '^' {
                j += 1;
            }
            if j < n && s[j] == ']' {
                j += 1;
            }
            while j < n && s[j] != ']' {
                if s[j] == '[' && j + 1 < n && ":.=".contains(s[j + 1]) {
                    let mut end = None;
                    let mut k = j + 2;
                    while k + 1 < n {
                        if s[k] == s[j + 1] && s[k + 1] == ']' {
                            end = Some(k);
                            break;
                        }
                        k += 1;
                    }
                    j = end? + 2;
                } else {
                    j += 1;
                }
            }
            if j >= n {
                return None;
            }
            j += 1;
        } else if c == delim {
            return Some(j + 1);
        } else if c == '\n' {
            return None;
        } else {
            j += 1;
        }
    }
    None
}

fn sed_skip_replacement(s: &[char], mut j: usize, delim: char) -> Option<usize> {
    let n = s.len();
    while j < n {
        let c = s[j];
        if c == '\\' {
            j += 2;
        } else if c == delim {
            return Some(j + 1);
        } else {
            j += 1;
        }
    }
    None
}

fn sed_skip_line(s: &[char], mut j: usize, continued: bool) -> usize {
    let n = s.len();
    while j < n && s[j] != '\n' {
        j += if continued && s[j] == '\\' { 2 } else { 1 };
    }
    j.min(n)
}

/// (new index, ok); ok is false when an address does not close.
fn sed_address(s: &[char], mut i: usize, second: bool) -> (usize, bool) {
    let n = s.len();
    if i >= n {
        return (i, true);
    }
    let c = s[i];
    if SED_DIGITS.contains(c) {
        while i < n && SED_DIGITS.contains(s[i]) {
            i += 1;
        }
        if i < n && s[i] == '~' {
            i += 1;
            while i < n && SED_DIGITS.contains(s[i]) {
                i += 1;
            }
        }
        return (i, true);
    }
    if c == '$' {
        return (i + 1, true);
    }
    if second && (c == '+' || c == '~') {
        i += 1;
        while i < n && SED_DIGITS.contains(s[i]) {
            i += 1;
        }
        return (i, true);
    }
    if c == '/' || c == '\\' {
        let mut d = '/';
        if c == '\\' {
            i += 1;
            if i >= n || s[i] == '\n' || s[i] == '\\' {
                return (i, false);
            }
            d = s[i];
        }
        let Some(next) = sed_skip_regex(s, i + 1, d) else { return (i, false) };
        i = next;
        while i < n && (s[i] == 'I' || s[i] == 'M') {
            i += 1;
        }
        return (i, true);
    }
    (i, true)
}

pub fn sed_script_risky(script: &str) -> bool {
    let s: Vec<char> = script.chars().collect();
    let n = s.len();
    let mut i = 0;
    while i < n {
        while i < n && " \t\n;".contains(s[i]) {
            i += 1;
        }
        if i >= n {
            break;
        }
        if s[i] == '#' {
            i = sed_skip_line(&s, i, false);
            continue;
        }
        for k in 0..2 {
            let (next, ok) = sed_address(&s, i, k == 1);
            i = next;
            if !ok {
                return true;
            }
            while i < n && (s[i] == ' ' || s[i] == '\t') {
                i += 1;
            }
            if k == 0 {
                if i < n && s[i] == ',' {
                    i += 1;
                    while i < n && (s[i] == ' ' || s[i] == '\t') {
                        i += 1;
                    }
                } else {
                    break;
                }
            }
        }
        while i < n && " \t!".contains(s[i]) {
            i += 1;
        }
        if i >= n {
            return true;
        }
        let cmd = s[i];
        i += 1;
        if cmd == '{' {
            continue;
        } else if cmd == '}' || SED_PLAIN.contains(cmd) {
        } else if SED_NUMBERED.contains(cmd) {
            while i < n && SED_DIGITS.contains(s[i]) {
                i += 1;
            }
        } else if cmd == 'a' || cmd == 'i' || cmd == 'c' {
            i = sed_skip_line(&s, i, true);
            continue;
        } else if "btT:".contains(cmd) {
            while i < n && (s[i] == ' ' || s[i] == '\t') {
                i += 1;
            }
            while i < n && !" \t\n;".contains(s[i]) {
                i += 1;
            }
            continue;
        } else if cmd == 'y' {
            if i >= n || s[i] == '\n' || s[i] == '\\' {
                return true;
            }
            let d = s[i];
            let Some(a) = sed_skip_replacement(&s, i + 1, d) else { return true };
            let Some(b) = sed_skip_replacement(&s, a, d) else { return true };
            i = b;
        } else if cmd == 's' {
            if i >= n || s[i] == '\n' || s[i] == '\\' {
                return true;
            }
            let d = s[i];
            let Some(a) = sed_skip_regex(&s, i + 1, d) else { return true };
            let Some(b) = sed_skip_replacement(&s, a, d) else { return true };
            i = b;
            while i < n && (SED_DIGITS.contains(s[i]) || "gpiImM".contains(s[i])) {
                i += 1;
            }
        } else {
            return true;
        }
        if i < n && !SED_END.contains(s[i]) {
            return true;
        }
    }
    false
}

#[cfg(test)]
mod sed_tests {
    use super::sed_script_risky;

    #[test]
    fn sed_scripts_that_write_or_run_a_file_are_risky() {
        for s in [
            "s/a/b/w /etc/x", "w /etc/x", "1w /etc/x", "/a/W /x", "s/a/b/e", "e echo", "r /etc/passwd", "R /x",
            "s/a/b/;w x", "s/[/]/x/w f", "s|a|b|gw f", "a\\\nfoo\nw /x", "s/\\//x/w f", "s/a/b", "zz",
            "s/[\\]/x/w f",
        ] {
            assert!(sed_script_risky(s), "{s:?} should be risky");
        }
        for s in [
            "s/a/b/", "s/a/b/g;p", "1,5p", "$!d", "/x/,/y/{p;d}", "s/world/x/", "y/abc/xyz/", "a hello; w /x",
            "/a/I!d", "\\,a,p", "b end;:end", "s/a/b/2p", "5q", "{p;p}", "p}", ":a;N;$!ba;s/\\n/ /g",
        ] {
            assert!(!sed_script_risky(s), "{s:?} should not be risky");
        }
    }
}
