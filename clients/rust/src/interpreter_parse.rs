//! Port of `opendaisugi/interpreter_parse.py`: shell-interpreter payload
//! extraction (`sh -c`, `xargs`, `find -exec`, `env`, ADR-0014 transparent
//! wrappers) plus a hand-rolled POSIX `shlex.split`/`shlex.quote` since Rust
//! has no stdlib equivalent.

pub const SHELL_C_INTERPRETERS: &[&str] = &["sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh"];

pub const OPAQUE_INTERPRETERS: &[&str] = &[
    "python", "python3", "python2", "perl", "ruby", "node", "deno", "awk", "gawk", "sed", "make", "eval",
    "exec", "source", "sudo", "doas", "watch", "chroot", "flock", "busybox", "script", "parallel", "coproc", "trap",
    "systemd-run", "screen", "unbuffer", "expect", "runuser", "taskset", "chrt", "prlimit", "unshare", "nsenter", "entr", "batch",
];

/// Long options by kind: 'n' takes no value, 'v' takes a value (`--name=V`
/// or the next word), 'o' takes a value only as `--name=V`.
pub type LongOpts = &'static [(&'static str, char)];

pub const ENV_LONG: LongOpts = &[
    ("--ignore-environment", 'n'),
    ("--null", 'n'),
    ("--unset", 'v'),
    ("--chdir", 'v'),
    ("--split-string", 'v'),
    ("--argv0", 'v'),
    ("--block-signal", 'o'),
    ("--default-signal", 'o'),
    ("--ignore-signal", 'o'),
    ("--list-signal-handling", 'n'),
    ("--debug", 'n'),
    ("--help", 'n'),
    ("--version", 'n'),
];

pub const XARGS_LONG: LongOpts = &[
    ("--null", 'n'),
    ("--arg-file", 'v'),
    ("--delimiter", 'v'),
    ("--eof", 'o'),
    ("--replace", 'o'),
    ("--max-lines", 'o'),
    ("--max-args", 'v'),
    ("--open-tty", 'n'),
    ("--max-procs", 'v'),
    ("--interactive", 'n'),
    ("--process-slot-var", 'v'),
    ("--no-run-if-empty", 'n'),
    ("--max-chars", 'v'),
    ("--show-limits", 'n'),
    ("--verbose", 'n'),
    ("--exit", 'n'),
    ("--help", 'n'),
    ("--version", 'n'),
];

/// xargs short options: these take a value; `e`, `i` and `l` take one only
/// attached.
pub const XARGS_SHORT_VALUE: &str = "nIPLdEsa";
pub const XARGS_SHORT_OPTIONAL: &str = "eil";

pub struct WrapperSpec {
    pub name: &'static str,
    pub short_value: &'static str,
    pub short_optional: &'static str,
    pub long: LongOpts,
    pub positional_skip: usize,
}

/// `interpreter_parse._TRANSPARENT_WRAPPERS`: the options are those of each
/// tool's man page (GNU coreutils 9.7, util-linux ionice and setsid, GNU time).
pub const TRANSPARENT_WRAPPERS: &[WrapperSpec] = &[
    WrapperSpec {
        name: "timeout",
        short_value: "ks",
        short_optional: "",
        long: &[
            ("--foreground", 'n'),
            ("--kill-after", 'v'),
            ("--preserve-status", 'n'),
            ("--signal", 'v'),
            ("--verbose", 'n'),
            ("--help", 'n'),
            ("--version", 'n'),
        ],
        positional_skip: 1,
    },
    WrapperSpec {
        name: "nice",
        short_value: "n",
        short_optional: "",
        long: &[("--adjustment", 'v'), ("--help", 'n'), ("--version", 'n')],
        positional_skip: 0,
    },
    WrapperSpec {
        name: "nohup",
        short_value: "",
        short_optional: "",
        long: &[("--help", 'n'), ("--version", 'n')],
        positional_skip: 0,
    },
    // GNU time: -o FILE writes FILE, -f FORMAT takes a format.
    WrapperSpec {
        name: "time",
        short_value: "fo",
        short_optional: "",
        long: &[
            ("--append", 'n'),
            ("--format", 'v'),
            ("--output", 'v'),
            ("--portability", 'n'),
            ("--quiet", 'n'),
            ("--verbose", 'n'),
            ("--help", 'n'),
            ("--version", 'n'),
        ],
        positional_skip: 0,
    },
    WrapperSpec {
        name: "stdbuf",
        short_value: "ioe",
        short_optional: "",
        long: &[("--input", 'v'), ("--output", 'v'), ("--error", 'v'), ("--help", 'n'), ("--version", 'n')],
        positional_skip: 0,
    },
    WrapperSpec { name: "command", short_value: "", short_optional: "", long: &[], positional_skip: 0 },
    WrapperSpec {
        name: "setsid",
        short_value: "",
        short_optional: "",
        long: &[("--ctty", 'n'), ("--fork", 'n'), ("--wait", 'n'), ("--help", 'n'), ("--version", 'n')],
        positional_skip: 0,
    },
    // util-linux ionice: -c, -n, -p, -P and -u take values; -t takes none.
    WrapperSpec {
        name: "ionice",
        short_value: "cnpPu",
        short_optional: "",
        long: &[
            ("--class", 'v'),
            ("--classdata", 'v'),
            ("--pid", 'v'),
            ("--pgid", 'v'),
            ("--ignore", 'n'),
            ("--uid", 'v'),
            ("--help", 'n'),
            ("--version", 'n'),
        ],
        positional_skip: 0,
    },
];

pub fn wrapper_spec(head: &str) -> Option<&'static WrapperSpec> {
    TRANSPARENT_WRAPPERS.iter().find(|w| w.name == head)
}

/// `interpreter_parse.OPTION_HEADS`: the heads whose payload is opaque when
/// an option cannot be followed.
pub fn is_option_head(head: &str) -> bool {
    head == "env" || head == "xargs" || wrapper_spec(head).is_some()
}

/// `interpreter_parse.resolve_long`: the long option `arg` selects, as GNU
/// getopt_long reads it: (full name, inline value, kind). An exact name wins,
/// otherwise a unique prefix; an ambiguous or unknown name, or a value given
/// to an option that takes none, is None.
pub fn resolve_long(arg: &str, table: LongOpts) -> Option<(&'static str, Option<String>, char)> {
    let (name, value) = match arg.split_once('=') {
        Some((n, v)) => (n, Some(v.to_string())),
        None => (arg, None),
    };
    let (full, kind) = match table.iter().find(|(k, _)| *k == name) {
        Some((k, kind)) => (*k, *kind),
        None => {
            let found: Vec<&(&'static str, char)> = table.iter().filter(|(k, _)| k.starts_with(name)).collect();
            if name.chars().count() <= 2 || found.len() != 1 {
                return None;
            }
            (found[0].0, found[0].1)
        }
    };
    if value.is_some() && kind == 'n' {
        return None;
    }
    Some((full, value, kind))
}

pub type OptList = Vec<(String, Option<String>)>;

/// `interpreter_parse.scan_options`: the leading options of `tokens[start..]`:
/// (index of the first word that is not one, the options), or None when a long
/// option is ambiguous or unknown. `--` ends the options and is consumed.
pub fn scan_options(
    tokens: &[String],
    start: usize,
    short_value: &str,
    short_optional: &str,
    long: LongOpts,
) -> Option<(usize, OptList)> {
    let n = tokens.len();
    let mut i = start;
    let mut opts: OptList = Vec::new();
    while i < n {
        let t = tokens[i].as_str();
        if t == "--" {
            i += 1;
            break;
        }
        if t.starts_with("--") {
            let (name, mut value, kind) = resolve_long(t, long)?;
            i += 1;
            if kind == 'v' && value.is_none() && i < n {
                value = Some(tokens[i].clone());
                i += 1;
            }
            opts.push((name.to_string(), value));
            continue;
        }
        if t.starts_with('-') && t != "-" {
            i += 1;
            let chars: Vec<char> = t.chars().collect();
            for j in 1..chars.len() {
                let c = chars[j];
                let rest: String = chars[j + 1..].iter().collect();
                if short_value.contains(c) {
                    if !rest.is_empty() {
                        opts.push((format!("-{c}"), Some(rest)));
                    } else if i < n {
                        opts.push((format!("-{c}"), Some(tokens[i].clone())));
                        i += 1;
                    } else {
                        opts.push((format!("-{c}"), None));
                    }
                    break;
                }
                if short_optional.contains(c) {
                    opts.push((format!("-{c}"), if rest.is_empty() { None } else { Some(rest) }));
                    break;
                }
                opts.push((format!("-{c}"), None));
            }
            continue;
        }
        break;
    }
    Some((i, opts))
}

pub fn xargs_options(tokens: &[String], start: usize) -> Option<(usize, OptList)> {
    scan_options(tokens, start, XARGS_SHORT_VALUE, XARGS_SHORT_OPTIONAL, XARGS_LONG)
}

pub fn wrapper_options(head: &str, tokens: &[String], start: usize) -> Option<(usize, OptList)> {
    let spec = wrapper_spec(head)?;
    scan_options(tokens, start, spec.short_value, spec.short_optional, spec.long)
}

pub const FIND_EXEC_FLAGS: &[&str] = &["-exec", "-execdir", "-ok", "-okdir"];

#[derive(Debug, Clone, Default)]
pub struct InterpreterPayload {
    pub head: String,
    pub inner_commands: Vec<String>,
    pub opaque: bool,
    /// For `find`, "exec" or "execdir" per inner command; empty otherwise.
    pub inner_kinds: Vec<String>,
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
                        // Python's shlex escapes only `"` and `\` inside
                        // double quotes; `\$` and `` \` `` keep the backslash.
                        Some('\\') => match chars.next() {
                            Some(n) if matches!(n, '"' | '\\') => cur.push(n),
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
            return InterpreterPayload { head: head.into(), inner_commands: vec![attached], opaque: false, inner_kinds: vec![] };
        }
        if i + 1 < tokens.len() {
            return InterpreterPayload {
                head: head.into(),
                inner_commands: vec![tokens[i + 1].clone()],
                opaque: false, inner_kinds: vec![],
            };
        }
        return InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false, inner_kinds: vec![] };
    }
    InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false, inner_kinds: vec![] }
}

fn parse_wrapper(head: &str, tokens: &[String]) -> InterpreterPayload {
    let Some((mut i, _)) = wrapper_options(head, tokens, 1) else {
        return InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: true, inner_kinds: vec![] };
    };
    let positional_skip = wrapper_spec(head).map_or(0, |w| w.positional_skip);
    i += positional_skip.min(tokens.len().saturating_sub(i));
    if i < tokens.len() {
        InterpreterPayload { head: head.into(), inner_commands: vec![quote_join(&tokens[i..])], opaque: false, inner_kinds: vec![] }
    } else {
        InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false, inner_kinds: vec![] }
    }
}

fn parse_xargs(head: &str, tokens: &[String]) -> InterpreterPayload {
    let Some((i, _)) = xargs_options(tokens, 1) else {
        return InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: true, inner_kinds: vec![] };
    };
    if i < tokens.len() {
        InterpreterPayload { head: head.into(), inner_commands: vec![quote_join(&tokens[i..])], opaque: false, inner_kinds: vec![] }
    } else {
        InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false, inner_kinds: vec![] }
    }
}

fn parse_find(head: &str, tokens: &[String]) -> InterpreterPayload {
    let mut inners = Vec::new();
    let mut kinds = Vec::new();
    let mut i = 0usize;
    while i < tokens.len() {
        if FIND_EXEC_FLAGS.contains(&tokens[i].as_str()) {
            let flag = tokens[i].as_str();
            let start = i + 1;
            let mut j = start;
            while j < tokens.len() && tokens[j] != ";" && tokens[j] != "+" {
                j += 1;
            }
            if j > start {
                inners.push(quote_join(&tokens[start..j]));
                kinds.push(if flag == "-execdir" || flag == "-okdir" { "execdir" } else { "exec" }.to_string());
            }
            i = j + 1;
        } else {
            i += 1;
        }
    }
    InterpreterPayload { head: head.into(), inner_commands: inners, opaque: false, inner_kinds: kinds }
}

/// `interpreter_parse._ENV_VALUE_*`: the env options whose value is the next
/// word (or the rest of the bundle).
const ENV_VALUE_SHORT: &str = "uCSa";

/// `interpreter_parse._parse_env`: the options that take a value consume it,
/// and `-S STRING` is split into the words that take its place.
fn parse_env(head: &str, tokens: &[String]) -> InterpreterPayload {
    let mut toks: Vec<String> = tokens.to_vec();
    let mut i = 1usize;
    while i < toks.len() {
        let t = toks[i].clone();
        if t.starts_with("--") && t != "--" {
            let Some((name, value, kind)) = resolve_long(&t, ENV_LONG) else {
                return InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: true, inner_kinds: vec![] };
            };
            if name == "--split-string" {
                if let Some(v) = value {
                    toks.splice(i..i + 1, env_split(&v));
                } else if i + 1 < toks.len() {
                    let words = env_split(&toks[i + 1]);
                    toks.splice(i..i + 2, words);
                } else {
                    i += 1;
                }
                continue;
            }
            i += if kind == 'v' && value.is_none() { 2 } else { 1 };
            continue;
        }
        if t.starts_with('-') && t != "-" {
            let mut skip = 1usize;
            let chars: Vec<char> = t.chars().collect();
            for j in 1..chars.len() {
                let c = chars[j];
                if !ENV_VALUE_SHORT.contains(c) {
                    continue;
                }
                let rest: String = chars[j + 1..].iter().collect();
                if c == 'S' {
                    if !rest.is_empty() {
                        toks.splice(i..i + 1, env_split(&rest));
                    } else if i + 1 < toks.len() {
                        let words = env_split(&toks[i + 1]);
                        toks.splice(i..i + 2, words);
                    } else {
                        i += 1;
                    }
                    skip = 0;
                } else if !rest.is_empty() {
                    skip = 1;
                } else {
                    skip = 2;
                }
                break;
            }
            i += skip;
            continue;
        }
        if t.contains('=') && !t.starts_with('=') {
            i += 1;
            continue;
        }
        break;
    }
    if i < toks.len() {
        InterpreterPayload { head: head.into(), inner_commands: vec![quote_join(&toks[i..])], opaque: false, inner_kinds: vec![] }
    } else {
        InterpreterPayload { head: head.into(), inner_commands: vec![], opaque: false, inner_kinds: vec![] }
    }
}

/// `interpreter_parse._env_split`: the words of an env -S string, or the
/// string as one word when they cannot be read exactly.
fn env_split(value: &str) -> Vec<String> {
    if value.contains(['\\', '$', '#']) {
        return vec![value.to_string()];
    }
    shlex_split(value).unwrap_or_else(|_| vec![value.to_string()])
}

/// `parse_interpreter` — the dispatcher. `None` means "not an interpreter
/// (or unparseable); apply the normal allowlist check only".
pub fn parse_interpreter(command: &str) -> Option<InterpreterPayload> {
    let stripped = command.trim();
    if stripped.is_empty() {
        return None;
    }
    let tokens = shlex_split(stripped).ok()?;
    let mut head = tokens.first()?.clone();
    if head.starts_with('/') {
        // An absolute path names the program: /usr/bin/env is env. A relative
        // path (./timeout, bin/env) is the project's own file and is not.
        head = head.rsplit('/').next().unwrap_or("").to_string();
    }
    if !crate::models::SHELL_INTERPRETERS.contains(&head.as_str()) {
        return None;
    }
    if OPAQUE_INTERPRETERS.contains(&head.as_str()) {
        return Some(InterpreterPayload { head, inner_commands: vec![], opaque: true, inner_kinds: vec![] });
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
    if wrapper_spec(&head).is_some() {
        return Some(parse_wrapper(&head, &tokens));
    }
    Some(InterpreterPayload { head, inner_commands: vec![], opaque: true, inner_kinds: vec![] })
}

#[cfg(test)]
mod tests {
    use super::{parse_interpreter, shlex_split};

    /// The rows are tests/test_interpreter_recursion.py: env options that
    /// take a value consume it, and -S is split into the words that take its
    /// place.
    #[test]
    fn env_options_consume_their_values() {
        let rows: &[(&str, &[&str])] = &[
            ("env -u cat curl http://evil", &["curl http://evil"]),
            ("env -ucat curl x", &["curl x"]),
            ("env --unset cat curl x", &["curl x"]),
            ("env --unset=cat curl x", &["curl x"]),
            ("env -C /etc tee hosts", &["tee hosts"]),
            ("env --chdir /etc tee hosts", &["tee hosts"]),
            ("env -i -u A -u B FOO=1 rm f", &["rm f"]),
            ("env -iu A rm f", &["rm f"]),
            ("env -S 'curl http://evil'", &["curl http://evil"]),
            ("env -S 'A=1 -i rm f' x", &["rm f x"]),
            ("env -S 'curl $X'", &["'curl $X'"]),
            ("env -i rm file", &["rm file"]),
        ];
        for (line, want) in rows {
            let p = parse_interpreter(line).expect("env is an interpreter");
            assert_eq!(p.inner_commands, want.to_vec(), "{line}");
        }
    }

    /// Review round 2: long options match as getopt_long does (an exact name,
    /// else a unique prefix); an ambiguous or unknown one is opaque.
    #[test]
    fn long_options_match_by_unique_prefix() {
        let rows: &[(&str, &[&str])] = &[
            ("env --u cat tee /etc/x", &["tee /etc/x"]),
            ("env --uns cat tee /etc/x", &["tee /etc/x"]),
            ("env --ar cat tee /etc/x", &["tee /etc/x"]),
            ("env --ch /etc tee x", &["tee x"]),
            ("xargs --arg-f in tee x", &["tee x"]),
            ("xargs -rI cat tee x", &["tee x"]),
            ("xargs --replace cat tee x", &["cat tee x"]),
            ("xargs --replace=R cat x", &["cat x"]),
            ("ionice -t tee cat /etc/passwd", &["tee cat /etc/passwd"]),
            ("ionice -c 2 -n 5 tee x", &["tee x"]),
            ("time -o cat tee /etc/x", &["tee /etc/x"]),
            ("time -f %e cat x", &["cat x"]),
            ("time -p cat x", &["cat x"]),
            ("nice --adj 5 tee x", &["tee x"]),
            ("timeout --kill 5 30 tee x", &["tee x"]),
        ];
        for (line, want) in rows {
            let p = parse_interpreter(line).expect("an interpreter");
            assert!(!p.opaque, "{line}");
            assert_eq!(p.inner_commands, want.to_vec(), "{line}");
        }
        for line in [
            "env --i cat tee x",
            "env --d cat tee x",
            "env --nope tee x",
            "env --null=1 tee x",
            "xargs --max cat tee x",
            "xargs --bogus tee x",
            "nice --nope tee x",
            "timeout --nope 30 tee x",
            "ionice --nope tee x",
            "nohup --nope tee x",
        ] {
            let p = parse_interpreter(line).expect("an interpreter");
            assert!(p.opaque, "{line}");
        }
    }

    /// Every row of `clients/go/internal/verify/testdata/shlex_fixture.jsonl`
    /// is what CPython's `shlex.split(s, posix=True)` produced.
    #[test]
    fn shlex_split_matches_python() {
        let p = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../go/internal/verify/testdata/shlex_fixture.jsonl");
        let text = std::fs::read_to_string(p).unwrap();
        let mut n = 0;
        for line in text.lines().filter(|l| !l.trim().is_empty()) {
            let row: serde_json::Value = serde_json::from_str(line).unwrap();
            let cmd = row["command"].as_str().unwrap();
            let got = shlex_split(cmd);
            match row.get("tokens") {
                Some(t) if !t.is_null() => {
                    let want: Vec<String> =
                        t.as_array().unwrap().iter().map(|x| x.as_str().unwrap().to_string()).collect();
                    assert_eq!(got.as_ref().ok(), Some(&want), "shlex_split({cmd:?})");
                }
                _ => assert!(got.is_err(), "shlex_split({cmd:?}) should fail: {got:?}"),
            }
            n += 1;
        }
        assert!(n >= 50);
        // Inside double quotes Python escapes only `"` and `\`.
        assert_eq!(shlex_split(r#""a\$b" "a\`b" "a\xb""#).unwrap(), vec![r"a\$b", r"a\`b", r"a\xb"]);
    }
}
