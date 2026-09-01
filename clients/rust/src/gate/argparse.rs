//! The gate's command line, read as Python 3.12's argparse reads it for
//! `gate._build_parser()`: abbreviations, `--opt=value`, `--`, negative
//! numbers, type conversion, choices, `--help` and every error, with the
//! usage and help text argparse prints at the terminal width it would use.

use super::py::text::{is_decimal, is_space, repr};

pub const PROG: &str = "opendaisugi.gate";

/// One option of the parser.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Opt {
    Help,
    Mode,
    Root,
    Format,
    VerifyTimeout,
    CapturesRoot,
    Session,
    Ask,
    AskTimeout,
    Checkpoints,
}

/// The option strings, in the order argparse keeps them.
const OPTION_STRINGS: &[(&str, Opt)] = &[
    ("-h", Opt::Help),
    ("--help", Opt::Help),
    ("--mode", Opt::Mode),
    ("--root", Opt::Root),
    ("--format", Opt::Format),
    ("--verify-timeout", Opt::VerifyTimeout),
    ("--captures-root", Opt::CapturesRoot),
    ("--session", Opt::Session),
    ("--ask", Opt::Ask),
    ("--ask-timeout", Opt::AskTimeout),
    ("--checkpoints", Opt::Checkpoints),
];

impl Opt {
    /// `_get_action_name`: the option strings joined with `/`.
    fn name(self) -> &'static str {
        match self {
            Opt::Help => "-h/--help",
            Opt::Mode => "--mode",
            Opt::Root => "--root",
            Opt::Format => "--format",
            Opt::VerifyTimeout => "--verify-timeout",
            Opt::CapturesRoot => "--captures-root",
            Opt::Session => "--session",
            Opt::Ask => "--ask",
            Opt::AskTimeout => "--ask-timeout",
            Opt::Checkpoints => "--checkpoints",
        }
    }

    /// Whether the option takes one value (nargs None) or none (nargs 0).
    fn takes_value(self) -> bool {
        !matches!(self, Opt::Help | Opt::Ask | Opt::Checkpoints)
    }
}

fn lookup(s: &str) -> Option<Opt> {
    OPTION_STRINGS.iter().find(|(o, _)| *o == s).map(|(_, a)| *a)
}

/// The parsed namespace.
#[derive(Debug, Clone, PartialEq)]
pub struct Args {
    pub mode: Option<String>,
    /// `--root` as given (a Path), or None for the default gate root.
    pub root: Option<String>,
    pub fmt: String,
    pub verify_timeout: f64,
    pub captures_root: Option<String>,
    pub session: Option<String>,
    pub ask: bool,
    pub ask_timeout: f64,
    pub checkpoints: bool,
}

/// How parsing ended when it did not give a namespace.
#[derive(Debug, Clone, PartialEq)]
pub enum Exit {
    /// `--help`: the help text on stdout, exit 0.
    Help(String),
    /// A parse error: the usage and the error on stderr, exit 2.
    Error(String),
}

/// One argument string's reading, as `_parse_optional` gives it.
#[derive(Debug, Clone)]
struct OptionTuple {
    action: Option<Opt>,
    option_string: String,
    sep: Option<String>,
    explicit_arg: Option<String>,
}

/// `_negative_number_matcher`: `^-\d+$|^-\d*\.\d+$` (`\d` is Unicode).
fn looks_negative_number(s: &str) -> bool {
    let rest = match s.strip_prefix('-') {
        Some(r) => r,
        None => return false,
    };
    let chars: Vec<char> = rest.chars().collect();
    if !chars.is_empty() && chars.iter().all(|c| is_decimal(*c)) {
        return true;
    }
    match chars.iter().position(|c| *c == '.') {
        Some(dot) => {
            chars[..dot].iter().all(|c| is_decimal(*c))
                && dot + 1 < chars.len()
                && chars[dot + 1..].iter().all(|c| is_decimal(*c))
        }
        None => false,
    }
}

/// `_get_option_tuples`.
fn option_tuples(arg: &str) -> Vec<OptionTuple> {
    let chars: Vec<char> = arg.chars().collect();
    let mut out = Vec::new();
    if chars[0] == '-' && chars.get(1) == Some(&'-') {
        let (prefix, sep, explicit) = match arg.split_once('=') {
            Some((p, e)) => (p, Some("=".to_string()), Some(e.to_string())),
            None => (arg, None, None),
        };
        for (o, a) in OPTION_STRINGS {
            if o.starts_with(prefix) {
                out.push(OptionTuple {
                    action: Some(*a),
                    option_string: o.to_string(),
                    sep: sep.clone(),
                    explicit_arg: explicit.clone(),
                });
            }
        }
    } else {
        let (prefix, sep, explicit) = match arg.split_once('=') {
            Some((p, e)) => (p, Some("=".to_string()), Some(e.to_string())),
            None => (arg, None, None),
        };
        let short: String = chars[..2.min(chars.len())].iter().collect();
        let short_explicit: String = chars[2.min(chars.len())..].iter().collect();
        for (o, a) in OPTION_STRINGS {
            if *o == short {
                out.push(OptionTuple {
                    action: Some(*a),
                    option_string: o.to_string(),
                    sep: Some(String::new()),
                    explicit_arg: Some(short_explicit.clone()),
                });
            } else if o.starts_with(prefix) {
                out.push(OptionTuple {
                    action: Some(*a),
                    option_string: o.to_string(),
                    sep: sep.clone(),
                    explicit_arg: explicit.clone(),
                });
            }
        }
    }
    out
}

/// `_parse_optional`: None for a positional.
fn parse_optional(arg: &str) -> Option<Vec<OptionTuple>> {
    if arg.is_empty() || !arg.starts_with('-') {
        return None;
    }
    if let Some(a) = lookup(arg) {
        return Some(vec![OptionTuple { action: Some(a), option_string: arg.into(), sep: None, explicit_arg: None }]);
    }
    if arg.chars().count() == 1 {
        return None;
    }
    if let Some((o, e)) = arg.split_once('=') {
        if let Some(a) = lookup(o) {
            return Some(vec![OptionTuple {
                action: Some(a),
                option_string: o.into(),
                sep: Some("=".into()),
                explicit_arg: Some(e.into()),
            }]);
        }
    }
    let t = option_tuples(arg);
    if !t.is_empty() {
        return Some(t);
    }
    if looks_negative_number(arg) {
        return None;
    }
    if arg.contains(' ') {
        return None;
    }
    Some(vec![OptionTuple { action: None, option_string: arg.into(), sep: None, explicit_arg: None }])
}

/// Python's `float(s)`.
pub fn py_float(s: &str) -> Option<f64> {
    let t = to_ascii_numeric(s)?;
    let t = t.trim_matches(|c: char| matches!(c, ' ' | '\t' | '\n' | '\x0b' | '\x0c' | '\r'));
    let (sign, body) = match t.strip_prefix('-') {
        Some(b) => (-1.0, b),
        None => (1.0, t.strip_prefix('+').unwrap_or(t)),
    };
    let low = body.to_ascii_lowercase();
    if low == "inf" || low == "infinity" {
        return Some(sign * f64::INFINITY);
    }
    if low == "nan" {
        return Some(f64::NAN);
    }
    let clean = strip_underscores_between_digits(body)?;
    let b = clean.as_bytes();
    // digits [. digits] [e [+-] digits], with digits on one side of the dot
    let mut i = 0;
    let int_digits = b.iter().take_while(|c| c.is_ascii_digit()).count();
    i += int_digits;
    let mut frac_digits = 0;
    if i < b.len() && b[i] == b'.' {
        i += 1;
        frac_digits = b[i..].iter().take_while(|c| c.is_ascii_digit()).count();
        i += frac_digits;
    }
    if int_digits == 0 && frac_digits == 0 {
        return None;
    }
    if i < b.len() && (b[i] == b'e' || b[i] == b'E') {
        i += 1;
        if i < b.len() && (b[i] == b'+' || b[i] == b'-') {
            i += 1;
        }
        let e = b[i..].iter().take_while(|c| c.is_ascii_digit()).count();
        if e == 0 {
            return None;
        }
        i += e;
    }
    if i != b.len() {
        return None;
    }
    clean.parse::<f64>().ok().map(|v| sign * v)
}

/// Python's `int(s)` for base 10, as an i128 (saturating).
pub fn py_int(s: &str) -> Option<i128> {
    let t = to_ascii_numeric(s)?;
    let t = t.trim_matches(|c: char| matches!(c, ' ' | '\t' | '\n' | '\x0b' | '\x0c' | '\r'));
    let (neg, body) = match t.strip_prefix('-') {
        Some(b) => (true, b),
        None => (false, t.strip_prefix('+').unwrap_or(t)),
    };
    if body.is_empty() || !body.bytes().all(|c| c.is_ascii_digit() || c == b'_') {
        return None;
    }
    let clean = strip_underscores_between_digits(body)?;
    let mut v: i128 = 0;
    for c in clean.bytes() {
        v = v.saturating_mul(10).saturating_add((c - b'0') as i128);
    }
    Some(if neg { -v } else { v })
}

/// `_PyUnicode_TransformDecimalAndSpaceToASCII`: a non-ASCII space is a
/// space, a non-ASCII decimal digit its ASCII digit; any other non-ASCII
/// character makes the text no number.
fn to_ascii_numeric(s: &str) -> Option<String> {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if (c as u32) < 127 {
            out.push(c);
        } else if is_space(c) {
            out.push(' ');
        } else if is_decimal(c) {
            out.push(char::from_digit(c.to_digit(10).or_else(|| decimal_value(c))?, 10)?);
        } else {
            return None;
        }
    }
    Some(out)
}

/// The value of a Unicode decimal digit: its offset from the zero of its
/// run of ten.
fn decimal_value(c: char) -> Option<u32> {
    let cp = c as u32;
    for k in 0..10 {
        let zero = cp.checked_sub(k)?;
        if let Some(z) = char::from_u32(zero) {
            if is_decimal(z) && (zero == 0 || !char::from_u32(zero - 1).map(is_decimal).unwrap_or(false)) {
                return Some(k);
            }
        }
    }
    None
}

/// Underscores are allowed only between two digits; they are dropped.
fn strip_underscores_between_digits(s: &str) -> Option<String> {
    let b = s.as_bytes();
    let mut out = String::with_capacity(s.len());
    for (i, &c) in b.iter().enumerate() {
        if c == b'_' {
            let ok = i > 0 && b[i - 1].is_ascii_digit() && i + 1 < b.len() && b[i + 1].is_ascii_digit();
            if !ok {
                return None;
            }
            continue;
        }
        out.push(c as char);
    }
    Some(out)
}

/// `argparse` for the gate's parser. `width` is the formatter width
/// (`shutil.get_terminal_size().columns - 2`).
pub fn parse(argv: &[String], width: i64) -> Result<Args, Exit> {
    let err = |msg: String| Exit::Error(format!("{}{PROG}: error: {msg}\n", usage(width)));
    let mut args = Args {
        mode: None,
        root: None,
        fmt: "claude".into(),
        verify_timeout: 10.0,
        captures_root: None,
        session: None,
        ask: false,
        ask_timeout: 90.0,
        checkpoints: false,
    };
    // Classify each argument string.
    let mut pattern: Vec<char> = Vec::new();
    let mut tuples: Vec<Option<Vec<OptionTuple>>> = Vec::new();
    let mut i = 0;
    while i < argv.len() {
        if argv[i] == "--" {
            pattern.push('-');
            tuples.push(None);
            for _ in i + 1..argv.len() {
                pattern.push('A');
                tuples.push(None);
            }
            break;
        }
        match parse_optional(&argv[i]) {
            None => {
                pattern.push('A');
                tuples.push(None);
            }
            Some(t) => {
                pattern.push('O');
                tuples.push(Some(t));
            }
        }
        i += 1;
    }
    let mut extras: Vec<String> = Vec::new();
    let mut start = 0usize;
    let max_opt = tuples.iter().rposition(|t| t.is_some());
    if let Some(max_opt) = max_opt {
        while start <= max_opt {
            let next = (start..tuples.len()).find(|k| tuples[*k].is_some()).unwrap_or(max_opt);
            // No positionals: what stands before the next option is extra.
            if tuples[start].is_none() {
                extras.extend(argv[start..next].iter().cloned());
                start = next;
            }
            // consume_optional
            let ts = tuples[start].clone().unwrap_or_default();
            if ts.len() > 1 {
                let names: Vec<&str> = ts.iter().map(|t| t.option_string.as_str()).collect();
                return Err(err(format!("ambiguous option: {} could match {}", argv[start], names.join(", "))));
            }
            let t = &ts[0];
            let mut action = t.action;
            let mut option_string = t.option_string.clone();
            let mut sep = t.sep.clone();
            let mut explicit = t.explicit_arg.clone();
            let mut taken: Vec<(Opt, Vec<String>)> = Vec::new();
            let stop;
            loop {
                let a = match action {
                    None => {
                        extras.push(argv[start].clone());
                        stop = start + 1;
                        break;
                    }
                    Some(a) => a,
                };
                if let Some(ex) = explicit.clone() {
                    let arg_count = if a.takes_value() { 1 } else { 0 };
                    let second = option_string.chars().nth(1);
                    if arg_count == 0 && second != Some('-') && !ex.is_empty() {
                        if sep.as_deref().is_some_and(|s| !s.is_empty()) || ex.starts_with('-') {
                            return Err(err(format!("argument {}: ignored explicit argument {}", a.name(), repr(&ex))));
                        }
                        taken.push((a, vec![]));
                        let first: char = ex.chars().next().unwrap_or('-');
                        option_string = format!("-{first}");
                        match lookup(&option_string) {
                            Some(next_a) => {
                                action = Some(next_a);
                                let rest: String = ex.chars().skip(1).collect();
                                if rest.is_empty() {
                                    sep = None;
                                    explicit = None;
                                } else if let Some(r) = rest.strip_prefix('=') {
                                    sep = Some("=".into());
                                    explicit = Some(r.to_string());
                                } else {
                                    sep = Some(String::new());
                                    explicit = Some(rest);
                                }
                            }
                            None => {
                                extras.push(format!("-{ex}"));
                                stop = start + 1;
                                break;
                            }
                        }
                    } else if arg_count == 1 {
                        taken.push((a, vec![ex]));
                        stop = start + 1;
                        break;
                    } else {
                        return Err(err(format!("argument {}: ignored explicit argument {}", a.name(), repr(&ex))));
                    }
                } else {
                    let s = start + 1;
                    let n = if a.takes_value() {
                        if pattern.get(s) == Some(&'A') {
                            1
                        } else {
                            return Err(err(format!("argument {}: expected one argument", a.name())));
                        }
                    } else {
                        0
                    };
                    taken.push((a, argv[s..s + n].to_vec()));
                    stop = s + n;
                    break;
                }
            }
            for (a, vals) in taken {
                take_action(&mut args, a, &vals, width).map_err(err)?.map_or(Ok(()), Err)?;
            }
            start = stop;
        }
    }
    extras.extend(argv[start.min(argv.len())..].iter().cloned());
    if !extras.is_empty() {
        return Err(err(format!("unrecognized arguments: {}", extras.join(" "))));
    }
    Ok(args)
}

/// `take_action` for one option: Ok(Some(exit)) when it ends parsing
/// (`--help`), Err(message) for a conversion or choice error.
fn take_action(args: &mut Args, a: Opt, vals: &[String], width: i64) -> Result<Option<Exit>, String> {
    let v = vals.first().cloned().unwrap_or_default();
    let float = |v: &str| -> Result<f64, String> {
        py_float(v).ok_or_else(|| format!("argument {}: invalid float value: {}", a.name(), repr(v)))
    };
    match a {
        Opt::Help => return Ok(Some(Exit::Help(help_text(width)))),
        Opt::Mode => {
            if v != "shadow" && v != "enforce" {
                return Err(format!(
                    "argument --mode: invalid choice: {} (choose from shadow, enforce)",
                    repr(&v)
                ));
            }
            args.mode = Some(v);
        }
        Opt::Root => args.root = Some(v),
        Opt::Format => args.fmt = v,
        Opt::VerifyTimeout => args.verify_timeout = float(&v)?,
        Opt::CapturesRoot => args.captures_root = Some(v),
        Opt::Session => args.session = Some(v),
        Opt::Ask => args.ask = true,
        Opt::AskTimeout => args.ask_timeout = float(&v)?,
        Opt::Checkpoints => args.checkpoints = true,
    }
    Ok(None)
}

const OPT_PARTS: &[&str] = &[
    "[-h]",
    "[--mode {shadow,enforce}]",
    "[--root ROOT]",
    "[--format FMT]",
    "[--verify-timeout VERIFY_TIMEOUT]",
    "[--captures-root CAPTURES_ROOT]",
    "[--session SESSION]",
    "[--ask]",
    "[--ask-timeout ASK_TIMEOUT]",
    "[--checkpoints]",
];

/// `HelpFormatter._format_usage` with `_format_help`'s tidying: the usage
/// block as `print_usage` prints it.
pub fn usage(width: i64) -> String {
    let prefix = "usage: ";
    let usage_full = format!("{PROG} {}", OPT_PARTS.join(" "));
    let text_width = width;
    let body = if (prefix.len() + usage_full.len()) as i64 > text_width {
        let get_lines = |parts: &[&str], indent: &str, prefix: Option<&str>| -> Vec<String> {
            let mut lines: Vec<String> = Vec::new();
            let mut line: Vec<&str> = Vec::new();
            let indent_len = indent.len() as i64;
            let mut line_len = match prefix {
                Some(p) => p.len() as i64 - 1,
                None => indent_len - 1,
            };
            for part in parts {
                if line_len + 1 + part.len() as i64 > text_width && !line.is_empty() {
                    lines.push(format!("{indent}{}", line.join(" ")));
                    line.clear();
                    line_len = indent_len - 1;
                }
                line.push(part);
                line_len += part.len() as i64 + 1;
            }
            if !line.is_empty() {
                lines.push(format!("{indent}{}", line.join(" ")));
            }
            if prefix.is_some() && !lines.is_empty() {
                lines[0] = lines[0][indent.len()..].to_string();
            }
            lines
        };
        let lines: Vec<String> = if ((prefix.len() + PROG.len()) as f64) <= 0.75 * text_width as f64 {
            let indent = " ".repeat(prefix.len() + PROG.len() + 1);
            let mut parts = vec![PROG];
            parts.extend(OPT_PARTS);
            get_lines(&parts, &indent, Some(prefix))
        } else {
            let indent = " ".repeat(prefix.len());
            let mut lines = get_lines(OPT_PARTS, &indent, None);
            if lines.len() > 1 {
                lines = get_lines(OPT_PARTS, &indent, None);
            }
            let mut out = vec![PROG.to_string()];
            out.extend(lines);
            out
        };
        lines.join("\n")
    } else {
        usage_full
    };
    format!("{prefix}{body}\n")
}

/// One option's help entry: its invocation and its help chunks (as
/// `textwrap` splits the help, which does not depend on the width).
struct Entry {
    invocation: &'static str,
    chunks: &'static [&'static str],
}

const ENTRIES: &[Entry] = &[
    Entry {
        invocation: "-h, --help",
        chunks: &["show", " ", "this", " ", "help", " ", "message", " ", "and", " ", "exit"],
    },
    Entry { invocation: "--mode {shadow,enforce}", chunks: &[] },
    Entry { invocation: "--root ROOT", chunks: &[] },
    Entry { invocation: "--format FMT", chunks: &[] },
    Entry { invocation: "--verify-timeout VERIFY_TIMEOUT", chunks: &[] },
    Entry { invocation: "--captures-root CAPTURES_ROOT", chunks: &[] },
    Entry {
        invocation: "--session SESSION",
        chunks: &[
            "Pin", " ", "the", " ", "envelope", " ", "to", " ", "this", " ", "registered", " ", "session,", " ",
            "ignoring", " ", "the", " ", "session", " ", "id", " ", "in", " ", "the", " ", "payload", " ",
            "(authorization", " ", "must", " ", "not", " ", "key", " ", "on", " ", "caller-", "influenceable", " ",
            "input).",
        ],
    },
    Entry {
        invocation: "--ask",
        chunks: &[
            "Enforce", " ", "mode", " ", "only:", " ", "hand", " ", "a", " ", "would-", "deny", " ", "to", " ", "a",
            " ", "present", " ", "operator", " ", "for", " ", "up", " ", "to", " ", "--ask-", "timeout", " ",
            "seconds", " ", "before", " ", "letting", " ", "the", " ", "deny", " ", "stand.", " ", "Off", " ", "by",
            " ", "default.",
        ],
    },
    Entry { invocation: "--ask-timeout ASK_TIMEOUT", chunks: &[] },
    Entry {
        invocation: "--checkpoints",
        chunks: &[
            "Snapshot", " ", "the", " ", "workspace", " ", "into", " ", "a", " ", "private", " ", "git", " ", "ref",
            " ", "at", " ", "most", " ", "once", " ", "per", " ", "new", " ", "prompt", " ", "boundary.", " ", "Off",
            " ", "by", " ", "default;", " ", "best-", "effort", " ", "and", " ", "never", " ", "affects", " ", "the",
            " ", "verdict.",
        ],
    },
];

/// `textwrap.wrap` of pre-split chunks at `width`, with the defaults the
/// help formatter uses (break long words, break on hyphens, drop
/// whitespace). Err is the ValueError a width under 1 raises.
fn wrap_chunks(chunks: &[&str], width: i64) -> Result<Vec<String>, String> {
    if width <= 0 {
        return Err(format!("invalid width {width} (must be > 0)"));
    }
    let mut rev: Vec<String> = chunks.iter().rev().map(|s| s.to_string()).collect();
    let mut lines: Vec<String> = Vec::new();
    let len = |s: &str| s.chars().count() as i64;
    while !rev.is_empty() {
        let mut cur: Vec<String> = Vec::new();
        let mut cur_len: i64 = 0;
        let w = width;
        if rev.last().is_some_and(|c| c.trim().is_empty()) && !lines.is_empty() {
            rev.pop();
        }
        while let Some(c) = rev.last() {
            let l = len(c);
            if cur_len + l <= w {
                cur_len += l;
                cur.push(rev.pop().unwrap_or_default());
            } else {
                break;
            }
        }
        if rev.last().is_some_and(|c| len(c) > w) {
            // _handle_long_word
            let space_left = if w < 1 { 1 } else { w - cur_len };
            let chunk: Vec<char> = rev.last().map(|c| c.chars().collect()).unwrap_or_default();
            let mut end = space_left as usize;
            if (chunk.len() as i64) > space_left {
                let head = &chunk[..(space_left as usize).min(chunk.len())];
                if let Some(h) = head.iter().rposition(|c| *c == '-') {
                    if h > 0 && chunk[..h].iter().any(|c| *c != '-') {
                        end = h + 1;
                    }
                }
            }
            let end = end.min(chunk.len());
            cur.push(chunk[..end].iter().collect());
            if let Some(last) = rev.last_mut() {
                *last = chunk[end..].iter().collect();
            }
            cur_len = cur.iter().map(|c| len(c)).sum();
        }
        if cur.last().is_some_and(|c| c.trim().is_empty()) {
            cur_len -= len(cur.last().map(String::as_str).unwrap_or(""));
            cur.pop();
        }
        let _ = cur_len;
        if !cur.is_empty() {
            lines.push(cur.concat());
        }
    }
    Ok(lines)
}

/// `format_help()` of the gate's parser at formatter width `width`: the
/// text `--help` prints. Help lines wrap at 11 columns or more, so
/// textwrap never refuses the width here.
pub fn help_text(width: i64) -> String {
    let indent = 2i64;
    let max_help_position = 24i64.min((width - 20).max(4));
    let action_max_length = ENTRIES.iter().map(|e| e.invocation.len() as i64 + indent).max().unwrap_or(0);
    let help_position = (action_max_length + 2).min(max_help_position);
    let help_width = (width - help_position).max(11);
    let action_width = help_position - indent - 2;
    let mut items = String::new();
    for e in ENTRIES {
        let pad = " ".repeat(indent as usize);
        if e.chunks.is_empty() {
            items.push_str(&format!("{pad}{}\n", e.invocation));
            continue;
        }
        let indent_first;
        if e.invocation.len() as i64 <= action_width {
            items.push_str(&format!("{pad}{:<w$}  ", e.invocation, w = action_width.max(0) as usize));
            indent_first = 0;
        } else {
            items.push_str(&format!("{pad}{}\n", e.invocation));
            indent_first = help_position;
        }
        let lines = wrap_chunks(e.chunks, help_width).unwrap_or_default();
        items.push_str(&format!("{}{}\n", " ".repeat(indent_first.max(0) as usize), lines[0]));
        for l in &lines[1..] {
            items.push_str(&format!("{}{}\n", " ".repeat(help_position.max(0) as usize), l));
        }
    }
    let usage_block = format!("{}\n", usage(width));
    let mut help = format!("{usage_block}\noptions:\n{items}\n");
    while help.contains("\n\n\n") {
        help = help.replace("\n\n\n", "\n\n");
    }
    format!("{}\n", help.trim_matches('\n'))
}

/// `shutil.get_terminal_size().columns`: COLUMNS when it is a positive
/// integer, else the terminal's width (`tty`, as the process's stdout
/// reports it), else 80.
pub fn terminal_columns(columns_env: Option<&str>, lines_env: Option<&str>, tty: Option<(i64, i64)>) -> i64 {
    let columns = columns_env.and_then(py_int).map(|v| v.clamp(i64::MIN as i128, i64::MAX as i128) as i64).unwrap_or(0);
    let lines = lines_env.and_then(py_int).map(|v| v.clamp(i64::MIN as i128, i64::MAX as i128) as i64).unwrap_or(0);
    if columns > 0 && lines > 0 {
        return columns;
    }
    if columns > 0 {
        return columns;
    }
    match tty {
        Some((c, _)) if c > 0 => c,
        _ => 80,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(s: &[&str]) -> Vec<String> {
        s.iter().map(|x| x.to_string()).collect()
    }

    #[test]
    fn usage_and_help_at_80_columns_are_pythons() {
        let u = usage(78);
        assert_eq!(
            u,
            "usage: opendaisugi.gate [-h] [--mode {shadow,enforce}] [--root ROOT]\n                        [--format FMT] [--verify-timeout VERIFY_TIMEOUT]\n                        [--captures-root CAPTURES_ROOT] [--session SESSION]\n                        [--ask] [--ask-timeout ASK_TIMEOUT] [--checkpoints]\n"
        );
        let h = help_text(78);
        assert!(h.contains("  --session SESSION     Pin the envelope to this registered session, ignoring\n"));
        assert!(h.contains("                        once per new prompt boundary. Off by default; best-\n"));
    }

    #[test]
    fn errors_read_as_argparse_words_them() {
        let e = |args: &[&str]| match parse(&a(args), 78) {
            Err(Exit::Error(t)) => t.lines().last().unwrap().to_string(),
            other => panic!("{other:?}"),
        };
        assert_eq!(e(&["--bogus"]), "opendaisugi.gate: error: unrecognized arguments: --bogus");
        assert_eq!(e(&["--mode", "x"]), "opendaisugi.gate: error: argument --mode: invalid choice: 'x' (choose from shadow, enforce)");
        assert_eq!(e(&["--verify-timeout", "abc"]), "opendaisugi.gate: error: argument --verify-timeout: invalid float value: 'abc'");
        assert_eq!(e(&["--ask=1"]), "opendaisugi.gate: error: argument --ask: ignored explicit argument '1'");
        assert_eq!(e(&["--a"]), "opendaisugi.gate: error: ambiguous option: --a could match --ask, --ask-timeout");
        assert_eq!(e(&["pos"]), "opendaisugi.gate: error: unrecognized arguments: pos");
        assert_eq!(e(&["--mode"]), "opendaisugi.gate: error: argument --mode: expected one argument");
        assert!(matches!(parse(&a(&["-h", "--bogus"]), 78), Err(Exit::Help(_))));
        assert!(matches!(parse(&a(&["-hx"]), 78), Err(Exit::Help(_))));
        let ok = parse(&a(&["--mo", "enforce", "--verify-timeout", "-5", "--ro=/x"]), 78).unwrap();
        assert_eq!((ok.mode.as_deref(), ok.verify_timeout, ok.root.as_deref()), (Some("enforce"), -5.0, Some("/x")));
    }

    #[test]
    fn numbers_parse_as_python_does() {
        assert_eq!(py_float(" 1_0.5 "), Some(10.5));
        assert_eq!(py_float("\u{0661}.\u{0665}"), Some(1.5));
        assert!(py_float("1_.5").is_none() && py_float("0x10").is_none() && py_float("\u{1c}5").is_none());
        assert_eq!(py_float("-iNF"), Some(f64::NEG_INFINITY));
        assert_eq!(py_int("\u{2009}80\u{2009}"), Some(80));
        assert_eq!(py_int("\u{ff18}\u{ff10}"), Some(80));
        assert!(py_int("\u{1c}80").is_none() && py_int("1__0").is_none() && py_int("80.0").is_none());
    }
}
