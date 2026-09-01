//! A gate hook command found by its words, never by a substring, the way
//! `opendaisugi.config` reads it. The command is split as the shell splits
//! it (quotes, backslashes, runs of ; | & as their own words), each simple
//! command is read on its own, and these forms are a gate hook:
//!
//! ```text
//! [NAME=val ...] [env [opts] [NAME=val ...]] [uv run [opts]] python*|py [flags] -m opendaisugi.gate[_client] ARGS
//! [NAME=val ...] .../daisugi gate check ARGS            (this binary)
//! sh|bash|dash|zsh|ksh [opts] -c '<one of the above>'
//! ```
//!
//! A command whose words hold opendaisugi.gate[_client] (alone, or joined
//! to -m) in any other form is an unknown gate hook: it may gate, but its
//! mode cannot be read, so status says so and uninstall refuses it. The Go
//! client's `config/words.go` is the reference.

use crate::gate::pyjson::Value;
use regex::Regex;
use std::sync::OnceLock;

/// The first word of every hook command this binary writes. Python's
/// `config.gate_hook_args` accepts it as the entry point of a compiled
/// gate.
pub const GATE_HOOK_MARKER: &str = "DAISUGI_GATE_HOOK=opendaisugi.gate";

pub const KIND_NONE: &str = "";
pub const KIND_GATE: &str = "gate";
pub const KIND_UNKNOWN: &str = "unknown";

const PY_NO_ARG_FLAGS: &[u8] = b"bBdEhiIOPqRsSuvx";
const MAX_SHELL_DEPTH: usize = 3;

fn re(which: usize) -> &'static Regex {
    static RES: OnceLock<Vec<Regex>> = OnceLock::new();
    &RES.get_or_init(|| {
        [r"^[A-Za-z_][A-Za-z0-9_]*=", r"^(python[0-9.]*|py)$", r"^-[A-Za-z]*m(opendaisugi\.gate(_client)?)$"]
            .iter()
            .map(|p| Regex::new(p).expect("a fixed pattern"))
            .collect()
    })[which]
}

fn is_assignment(w: &str) -> bool {
    re(0).is_match(w)
}

fn is_gate_module(w: &str) -> bool {
    w == "opendaisugi.gate" || w == "opendaisugi.gate_client"
}

fn env_value_opt(w: &str) -> bool {
    matches!(w, "-u" | "--unset" | "-C" | "--chdir")
}

fn uv_value_opt(w: &str) -> bool {
    matches!(
        w,
        "--project"
            | "--directory"
            | "--python"
            | "-p"
            | "--with"
            | "--with-editable"
            | "--with-requirements"
            | "--env-file"
            | "--extra"
            | "--group"
            | "--only-group"
            | "--no-group"
            | "--package"
            | "--index"
            | "--default-index"
            | "--index-url"
            | "--extra-index-url"
            | "--find-links"
            | "-f"
            | "--cache-dir"
            | "--config-file"
            | "-w"
    )
}

/// One word of a command, or an operator (a run of ; | &).
#[derive(Debug, Clone)]
pub struct Token {
    pub text: String,
    pub op: bool,
}

/// `config.command_tokens`: the command split as a POSIX shell splits it.
/// `None` on an unclosed quote.
pub fn command_tokens(command: &str) -> Option<Vec<Token>> {
    let s = command.as_bytes();
    let n = s.len();
    let mut out = vec![];
    let mut cur: Vec<u8> = vec![];
    let mut have = false;
    let flush = |out: &mut Vec<Token>, cur: &mut Vec<u8>, have: &mut bool| {
        if *have {
            out.push(Token { text: String::from_utf8_lossy(cur).into_owned(), op: false });
        }
        cur.clear();
        *have = false;
    };
    let mut i = 0;
    while i < n {
        let c = s[i];
        match c {
            b' ' | b'\t' | b'\r' | b'\n' => {
                flush(&mut out, &mut cur, &mut have);
                i += 1;
            }
            b';' | b'|' | b'&' => {
                flush(&mut out, &mut cur, &mut have);
                let mut j = i;
                while j < n && matches!(s[j], b';' | b'|' | b'&') {
                    j += 1;
                }
                out.push(Token { text: String::from_utf8_lossy(&s[i..j]).into_owned(), op: true });
                i = j;
            }
            b'\'' => {
                let j = s[i + 1..].iter().position(|b| *b == b'\'')?;
                cur.extend_from_slice(&s[i + 1..i + 1 + j]);
                have = true;
                i += 2 + j;
            }
            b'"' => {
                have = true;
                i += 1;
                loop {
                    if i >= n {
                        return None;
                    }
                    let c = s[i];
                    if c == b'"' {
                        i += 1;
                        break;
                    }
                    if c == b'\\' && i + 1 < n && b"\"\\$`\n".contains(&s[i + 1]) {
                        if s[i + 1] != b'\n' {
                            cur.push(s[i + 1]);
                        }
                        i += 2;
                        continue;
                    }
                    cur.push(c);
                    i += 1;
                }
            }
            b'\\' => {
                if i + 1 < n && s[i + 1] != b'\n' {
                    // The escaped character may be a multi-byte one: copy it whole.
                    let mut k = i + 2;
                    while k < n && s[k] & 0xC0 == 0x80 {
                        k += 1;
                    }
                    cur.extend_from_slice(&s[i + 1..k]);
                    have = true;
                    i = k;
                    continue;
                }
                i += 2;
            }
            _ => {
                cur.push(c);
                have = true;
                i += 1;
            }
        }
    }
    flush(&mut out, &mut cur, &mut have);
    Some(out)
}

fn segments(tokens: &[Token]) -> Vec<Vec<String>> {
    let mut out = vec![];
    let mut cur: Vec<String> = vec![];
    for t in tokens {
        if t.op {
            if !cur.is_empty() {
                out.push(std::mem::take(&mut cur));
            }
            continue;
        }
        cur.push(t.text.clone());
    }
    if !cur.is_empty() {
        out.push(cur);
    }
    out
}

/// The last "/"-separated part.
fn path_base(p: &str) -> &str {
    match p.rfind('/') {
        Some(i) => &p[i + 1..],
        None => p,
    }
}

/// Drops leading NAME=val words and an `env [opts] [NAME=val ...]` prefix.
fn skip_env(mut words: &[String]) -> &[String] {
    while !words.is_empty() && is_assignment(&words[0]) {
        words = &words[1..];
    }
    if !words.is_empty() && path_base(&words[0]) == "env" {
        words = &words[1..];
        while !words.is_empty() && words[0].starts_with('-') {
            if words[0] == "--" {
                words = &words[1..];
                break;
            }
            if env_value_opt(&words[0]) {
                words = &words[2.min(words.len())..];
            } else {
                words = &words[1..];
            }
        }
        while !words.is_empty() && is_assignment(&words[0]) {
            words = &words[1..];
        }
    }
    words
}

/// `python* [flags] -m MODULE ARGS`: the words after a gate MODULE.
fn python_module_args(words: &[String]) -> Option<Vec<String>> {
    let mut i = 1;
    while i < words.len() {
        let w = &words[i];
        if !w.starts_with('-') || w == "-" || w.starts_with("--") {
            return None;
        }
        let rest = &w.as_bytes()[1..];
        let mut module: Option<String> = None;
        let mut k = 0;
        while k < rest.len() {
            let ch = rest[k];
            if PY_NO_ARG_FLAGS.contains(&ch) {
                k += 1;
                continue;
            }
            if ch == b'm' {
                let m = String::from_utf8_lossy(&rest[k + 1..]).into_owned();
                if m.is_empty() {
                    if i + 1 >= words.len() {
                        return None;
                    }
                    i += 1;
                    module = Some(words[i].clone());
                } else {
                    module = Some(m);
                }
                break;
            }
            if ch == b'W' || ch == b'X' {
                if k + 1 == rest.len() {
                    i += 1;
                }
                break;
            }
            return None;
        }
        if let Some(m) = module {
            if is_gate_module(&m) {
                return Some(words[i + 1..].to_vec());
            }
            return None;
        }
        i += 1;
    }
    None
}

fn is_shell(b: &str) -> bool {
    matches!(b, "sh" | "bash" | "dash" | "zsh" | "ksh")
}

fn segment_gate_args(seg: &[String], depth: usize) -> (&'static str, Option<Vec<String>>) {
    let mut words = skip_env(seg);
    if words.len() >= 2 && path_base(&words[0]) == "uv" && words[1] == "run" {
        words = &words[2..];
        while !words.is_empty() && words[0].starts_with('-') {
            if words[0] == "--" {
                words = &words[1..];
                break;
            }
            if uv_value_opt(&words[0]) {
                words = &words[2.min(words.len())..];
            } else {
                words = &words[1..];
            }
        }
        words = skip_env(words);
    }
    if !words.is_empty() && re(1).is_match(path_base(&words[0])) {
        if let Some(args) = python_module_args(words) {
            return (KIND_GATE, Some(args));
        }
    } else if words.len() >= 3 && path_base(&words[0]) == "daisugi" && words[1] == "gate" && words[2] == "check" {
        return (KIND_GATE, Some(words[3..].to_vec()));
    } else if !words.is_empty() && is_shell(path_base(&words[0])) && depth < MAX_SHELL_DEPTH {
        for i in 1..words.len() {
            let w = &words[i];
            if !w.starts_with('-') || w.starts_with("--") {
                break;
            }
            if w[1..].contains('c') && i + 1 < words.len() {
                return gate_hook(&words[i + 1], depth + 1);
            }
        }
    }
    for w in words {
        if is_gate_module(w) || re(2).is_match(w) {
            return (KIND_UNKNOWN, None);
        }
    }
    (KIND_NONE, None)
}

fn gate_hook(command: &str, depth: usize) -> (&'static str, Option<Vec<String>>) {
    let tokens = match command_tokens(command) {
        Some(t) => t,
        None => {
            if command.contains("opendaisugi.gate") {
                return (KIND_UNKNOWN, None);
            }
            return (KIND_NONE, None);
        }
    };
    let mut unknown = false;
    for seg in segments(&tokens) {
        let (kind, args) = segment_gate_args(&seg, depth);
        if kind == KIND_GATE {
            return (kind, Some(args.unwrap_or_default()));
        }
        unknown = unknown || kind == KIND_UNKNOWN;
    }
    if unknown {
        (KIND_UNKNOWN, None)
    } else {
        (KIND_NONE, None)
    }
}

/// `config.gate_hook_kind`.
/// `config.gate_hook_program`: the program of a gate hook in the
/// `daisugi gate check` form (the form the Go and Rust installs write), or
/// "" for any other command.
pub fn gate_hook_program(command: &Value) -> String {
    let Value::Str(s) = command else { return String::new() };
    let Some(tokens) = command_tokens(s) else { return String::new() };
    for seg in segments(&tokens) {
        let words = skip_env(&seg);
        if words.len() >= 3 && path_base(&words[0]) == "daisugi" && words[1] == "gate" && words[2] == "check" {
            return words[0].clone();
        }
    }
    String::new()
}

pub fn gate_hook_kind(command: &Value) -> &'static str {
    match command {
        Value::Str(s) => gate_hook(s, 0).0,
        _ => KIND_NONE,
    }
}

/// `config.gate_hook_args`: the words after the gate entry point, when
/// command is a gate hook in a form read.
pub fn gate_hook_args(command: &Value) -> Option<Vec<String>> {
    match command {
        Value::Str(s) => match gate_hook(s, 0) {
            (KIND_GATE, args) => Some(args.unwrap_or_default()),
            _ => None,
        },
        _ => None,
    }
}

/// Whether command is a gate hook in a form read.
pub fn is_gate_hook(command: &Value) -> bool {
    gate_hook_args(command).is_some()
}

/// `config.gate_hook_mode`: the last --mode a gate hook passes, or "".
pub fn gate_hook_mode(command: &Value) -> String {
    let args = gate_hook_args(command).unwrap_or_default();
    let mut mode = String::new();
    for (i, w) in args.iter().enumerate() {
        if w == "--mode" && i + 1 < args.len() {
            mode = args[i + 1].clone();
        } else if let Some(v) = w.strip_prefix("--mode=") {
            mode = v.to_string();
        }
    }
    if mode == "shadow" || mode == "enforce" {
        mode
    } else {
        String::new()
    }
}

/// `config.is_record_hook`: a `daisugi hook record` simple command; with
/// `event` non-empty, only one passing `--event <event>`.
pub fn is_record_hook(command: &Value, event: &str) -> bool {
    let s = match command {
        Value::Str(s) => s,
        _ => return false,
    };
    let tokens = match command_tokens(s) {
        Some(t) => t,
        None => return false,
    };
    for seg in segments(&tokens) {
        let words = skip_env(&seg);
        if words.len() < 3 || path_base(&words[0]) != "daisugi" || words[1] != "hook" || words[2] != "record" {
            continue;
        }
        if event.is_empty() {
            return true;
        }
        for (i, w) in words.iter().enumerate() {
            if (w == "--event" && i + 1 < words.len() && words[i + 1] == event) || *w == format!("--event={event}") {
                return true;
            }
        }
    }
    false
}
