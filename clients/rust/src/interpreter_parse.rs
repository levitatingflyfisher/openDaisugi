//! Port of `opendaisugi/interpreter_parse.py`: shell-interpreter payload
//! extraction (`sh -c`, `xargs`, `find -exec`, `env`, ADR-0014 transparent
//! wrappers) plus a hand-rolled POSIX `shlex.split`/`shlex.quote` since Rust
//! has no stdlib equivalent.

use std::collections::HashSet;

pub const SHELL_C_INTERPRETERS: &[&str] = &["sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh"];

pub const OPAQUE_INTERPRETERS: &[&str] = &[
    "python", "python3", "python2", "perl", "ruby", "node", "deno", "awk", "gawk", "sed", "make", "eval",
    "exec", "source", "sudo", "doas", "watch",
];

/// (name, (value_flags, positional_skip))
pub const TRANSPARENT_WRAPPERS: &[(&str, &[&str], usize)] = &[
    ("timeout", &["-k", "--kill-after", "-s", "--signal"], 1),
    ("nice", &["-n", "--adjustment"], 0),
    ("nohup", &[], 0),
    ("time", &[], 0),
    ("stdbuf", &["-i", "-o", "-e"], 0),
    ("command", &[], 0),
    ("setsid", &[], 0),
    ("ionice", &["-c", "-n", "-t"], 0),
];

pub const XARGS_VALUE_FLAGS: &[&str] = &[
    "-n", "-I", "-P", "-L", "-d", "-E", "-s", "-a", "--max-args", "--replace", "--max-procs", "--max-lines",
    "--delimiter", "--eof", "--max-chars", "--arg-file",
];

pub const FIND_EXEC_FLAGS: &[&str] = &["-exec", "-execdir", "-ok", "-okdir"];

#[derive(Debug, Clone, Default)]
pub struct InterpreterPayload {
    pub head: String,
    pub inner_commands: Vec<String>,
    pub opaque: bool,
}

// --- shlex ------------------------------------------------------------------------

/// `shlex.split(s, posix=True)` — whitespace-split, single quotes literal,
/// double quotes escape `\ $ " \``, backslash escapes the next char outside
/// quotes. Returns `Err` on an unterminated quote / trailing backslash,
/// mirroring shlex's `ValueError` (the caller treats that as "not
/// tokenizable").
pub fn shlex_split(s: &str) -> Result<Vec<String>, String> {
    let mut tokens = Vec::new();
    let mut chars = s.chars().peekable();
    let mut cur = String::new();
    let mut in_token = false;

    while let Some(c) = chars.next() {
        match c {
            ' ' | '\t' | '\r' | '\n' => {
                if in_token {
                    tokens.push(std::mem::take(&mut cur));
                    in_token = false;
                }
            }
            '\'' => {
                in_token = true;
                loop {
                    match chars.next() {
                        Some('\'') => break,
                        Some(ch) => cur.push(ch),
                        None => return Err("no closing quotation".into()),
                    }
                }
            }
            '"' => {
                in_token = true;
                loop {
                    match chars.next() {
                        Some('"') => break,
                        Some('\\') => match chars.next() {
                            Some(n) if matches!(n, '"' | '\\' | '$' | '`') => cur.push(n),
                            Some(n) => {
                                cur.push('\\');
                                cur.push(n);
                            }
                            None => return Err("no closing quotation".into()),
                        },
                        Some(ch) => cur.push(ch),
                        None => return Err("no closing quotation".into()),
                    }
                }
            }
            '\\' => {
                in_token = true;
                match chars.next() {
                    Some(n) => cur.push(n),
                    None => return Err("no escaped character".into()),
                }
            }
            other => {
                in_token = true;
                cur.push(other);
            }
        }
    }
    if in_token {
        tokens.push(cur);
    }
    Ok(tokens)
}

fn is_shlex_safe(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '_' | '@' | '%' | '+' | '=' | ':' | ',' | '.' | '/' | '-')
}

/// `shlex.quote` — used when reassembling wrapper/xargs/find/env inner
/// commands from their tokenized arguments.
pub fn shlex_quote(s: &str) -> String {
    if s.is_empty() {
        return "''".to_string();
    }
    if s.chars().all(is_shlex_safe) {
        return s.to_string();
    }
    format!("'{}'", s.replace('\'', "'\"'\"'"))
}

fn quote_join(tokens: &[String]) -> String {
    tokens.iter().map(|t| shlex_quote(t)).collect::<Vec<_>>().join(" ")
}

// --- per-interpreter parsers --------------------------------------------------------

fn parse_shell_c(head: &str, tokens: &[String]) -> InterpreterPayload {
    for i in 1..tokens.len() {
        let tok = &tokens[i];
        let chars: Vec<char> = tok.chars().collect();
        if chars.len() < 2 || chars[0] != '-' || chars[1] == '-' || !tok.contains('c') {
            continue;
        }
        let cluster: Vec<char> = chars[1..].to_vec();
        let cpos = match cluster.iter().position(|&c| c == 'c') {
            Some(p) => p,
            None => continue,
        };
        let before: String = cluster[..cpos].iter().collect();
        if !before.is_empty() && !before.chars().all(|c| c.is_alphabetic()) {
            continue;
        }
        let attached: String = cluster[cpos + 1..].iter().collect();
        if !attached.is_empty() {
            return InterpreterPayload { head: head.into(), inner_commands: vec![attached], opaque: false };
        }
        if i + 1 < tokens.len() {
            return InterpreterPayload {
                head: head.into(),
                inner_commands: vec![tokens[i + 1].clone()],
                opaque: false,
            };
        }
        return InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false };
    }
    InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false }
}

fn parse_wrapper(head: &str, tokens: &[String]) -> InterpreterPayload {
    let (value_flags, positional_skip) =
        TRANSPARENT_WRAPPERS.iter().find(|(n, _, _)| *n == head).map(|(_, f, p)| (*f, *p)).unwrap();
    let value_flags: HashSet<&str> = value_flags.iter().copied().collect();
    let mut i = 1usize;
    while i < tokens.len() {
        let t = &tokens[i];
        if t == "--" {
            i += 1;
            break;
        }
        if t.starts_with('-') && t != "-" {
            if value_flags.contains(t.as_str()) && i + 1 < tokens.len() {
                i += 2;
                continue;
            }
            i += 1;
            continue;
        }
        break;
    }
    i += positional_skip.min(tokens.len().saturating_sub(i));
    if i < tokens.len() {
        InterpreterPayload { head: head.into(), inner_commands: vec![quote_join(&tokens[i..])], opaque: false }
    } else {
        InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false }
    }
}

fn parse_xargs(head: &str, tokens: &[String]) -> InterpreterPayload {
    let value_flags: HashSet<&str> = XARGS_VALUE_FLAGS.iter().copied().collect();
    let mut i = 1usize;
    while i < tokens.len() {
        let t = &tokens[i];
        if t == "--" {
            i += 1;
            break;
        }
        if t.starts_with('-') {
            if value_flags.contains(t.as_str()) && i + 1 < tokens.len() {
                i += 2;
                continue;
            }
            i += 1;
            continue;
        }
        break;
    }
    if i < tokens.len() {
        InterpreterPayload { head: head.into(), inner_commands: vec![quote_join(&tokens[i..])], opaque: false }
    } else {
        InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false }
    }
}

fn parse_find(head: &str, tokens: &[String]) -> InterpreterPayload {
    let mut inners = Vec::new();
    let mut i = 0usize;
    while i < tokens.len() {
        if FIND_EXEC_FLAGS.contains(&tokens[i].as_str()) {
            let start = i + 1;
            let mut j = start;
            while j < tokens.len() && tokens[j] != ";" && tokens[j] != "+" {
                j += 1;
            }
            if j > start {
                inners.push(quote_join(&tokens[start..j]));
            }
            i = j + 1;
        } else {
            i += 1;
        }
    }
    InterpreterPayload { head: head.into(), inner_commands: inners, opaque: false }
}

fn parse_env(head: &str, tokens: &[String]) -> InterpreterPayload {
    let mut i = 1usize;
    while i < tokens.len() {
        let t = &tokens[i];
        if t.starts_with('-') {
            i += 1;
            continue;
        }
        if t.contains('=') && !t.starts_with('=') {
            i += 1;
            continue;
        }
        break;
    }
    if i < tokens.len() {
        InterpreterPayload { head: head.into(), inner_commands: vec![quote_join(&tokens[i..])], opaque: false }
    } else {
        InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false }
    }
}

/// `parse_interpreter` — the dispatcher. `None` means "not an interpreter
/// (or unparseable); apply the normal allowlist check only".
pub fn parse_interpreter(command: &str) -> Option<InterpreterPayload> {
    let stripped = command.trim();
    if stripped.is_empty() {
        return None;
    }
    let tokens = shlex_split(stripped).ok()?;
    let head = tokens.first()?.clone();
    if !crate::models::SHELL_INTERPRETERS.contains(&head.as_str()) {
        return None;
    }
    if OPAQUE_INTERPRETERS.contains(&head.as_str()) {
        return Some(InterpreterPayload { head, inner_commands: vec![], opaque: true });
    }
    if SHELL_C_INTERPRETERS.contains(&head.as_str()) {
        return Some(parse_shell_c(&head, &tokens));
    }
    if head == "xargs" {
        return Some(parse_xargs(&head, &tokens));
    }
    if head == "find" {
        return Some(parse_find(&head, &tokens));
    }
    if head == "env" {
        return Some(parse_env(&head, &tokens));
    }
    if TRANSPARENT_WRAPPERS.iter().any(|(n, _, _)| *n == head) {
        return Some(parse_wrapper(&head, &tokens));
    }
    Some(InterpreterPayload { head, inner_commands: vec![], opaque: true })
}
