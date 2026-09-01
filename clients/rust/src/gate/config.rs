//! `config.load_config` as the gate reads it: the YAML beside the gate
//! root, validated by pydantic's `Config` model, for its `gate_mode` and
//! `verifier_client`.
//!
//! The reader takes the YAML that `save_config` writes and that a person
//! edits by hand: a block mapping, one level of nested mapping, plain,
//! single-quoted and double-quoted scalars, empty flow collections, block
//! sequences of scalars, and comments. Plain scalars resolve as PyYAML's
//! YAML 1.1 resolvers resolve them. Anything else (anchors, tags, block or
//! multi-line scalars, flow content, tabs) is not decided by the port.

use super::paths::{path_join, path_parent};
use super::{catch, undecided, Runner, R};

/// A YAML value, as `yaml.safe_load` gives it, as far as the gate reads it.
#[derive(Debug, Clone, PartialEq)]
pub enum Y {
    Null,
    Bool(bool),
    /// An int, saturated to the i128 range (validity is all that matters).
    Int(i128),
    Float(f64),
    Str(String),
    List(Vec<Y>),
    Map(Vec<(Y, Y)>),
}

/// PyYAML's bool resolver.
fn yaml_bool(s: &str) -> Option<bool> {
    match s {
        "yes" | "Yes" | "YES" | "true" | "True" | "TRUE" | "on" | "On" | "ON" => Some(true),
        "no" | "No" | "NO" | "false" | "False" | "FALSE" | "off" | "Off" | "OFF" => Some(false),
        _ => None,
    }
}

fn all_chars(s: &str, f: impl Fn(u8) -> bool) -> bool {
    s.bytes().all(f)
}

/// PyYAML's int resolver and constructor.
fn yaml_int(s: &str) -> Option<R<i128>> {
    let (neg, body) = match s.as_bytes().first() {
        Some(b'-') => (true, &s[1..]),
        Some(b'+') => (false, &s[1..]),
        _ => (false, s),
    };
    let parse = |digits: &str, radix: u32| -> i128 {
        let mut v: i128 = 0;
        for c in digits.chars().filter(|c| *c != '_') {
            v = v.saturating_mul(radix as i128).saturating_add(c.to_digit(radix).unwrap_or(0) as i128);
        }
        v
    };
    let v = if let Some(d) = body.strip_prefix("0b") {
        if d.is_empty() || !all_chars(d, |c| c == b'0' || c == b'1' || c == b'_') {
            return None;
        }
        parse(d, 2)
    } else if let Some(d) = body.strip_prefix("0x") {
        if d.is_empty() || !all_chars(d, |c| c.is_ascii_hexdigit() || c == b'_') {
            return None;
        }
        parse(d, 16)
    } else if body == "0" {
        0
    } else if body.len() > 1 && body.starts_with('0') {
        if !all_chars(&body[1..], |c| (b'0'..=b'7').contains(&c) || c == b'_') {
            return None;
        }
        parse(&body[1..], 8)
    } else if !body.is_empty() && body.as_bytes()[0].is_ascii_digit() && body.as_bytes()[0] != b'0' {
        if body.contains(':') {
            // Sexagesimal: the port does not decide it.
            let ok = body.split(':').enumerate().all(|(i, p)| {
                !p.is_empty() && all_chars(p, |c| c.is_ascii_digit() || (i == 0 && c == b'_'))
            });
            return if ok { Some(undecided("a sexagesimal integer in config.yaml")) } else { None };
        }
        if !all_chars(body, |c| c.is_ascii_digit() || c == b'_') {
            return None;
        }
        parse(body, 10)
    } else {
        return None;
    };
    Some(Ok(if neg { -v } else { v }))
}

/// PyYAML's float resolver (a dot is required) and constructor.
fn yaml_float(s: &str) -> Option<R<f64>> {
    let (sign, body) = match s.as_bytes().first() {
        Some(b'-') => (-1.0, &s[1..]),
        Some(b'+') => (1.0, &s[1..]),
        _ => (1.0, s),
    };
    if matches!(body, ".inf" | ".Inf" | ".INF") {
        return Some(Ok(sign * f64::INFINITY));
    }
    if matches!(s, ".nan" | ".NaN" | ".NAN") {
        return Some(Ok(f64::NAN));
    }
    let b = body.as_bytes();
    let dot = body.find('.')?;
    let (int_part, rest) = (&body[..dot], &body[dot + 1..]);
    if int_part.contains(':') {
        let sexagesimal = int_part.as_bytes()[0].is_ascii_digit()
            && all_chars(int_part, |c| c.is_ascii_digit() || c == b'_' || c == b':')
            && all_chars(rest, |c| c.is_ascii_digit() || c == b'_');
        return if sexagesimal { Some(undecided("a sexagesimal float in config.yaml")) } else { None };
    }
    let int_ok = if int_part.is_empty() {
        // \.[0-9][0-9_]* form: no sign allowed, a digit first after the dot.
        s == body && !rest.is_empty() && rest.as_bytes()[0].is_ascii_digit()
    } else {
        int_part.as_bytes()[0].is_ascii_digit() && all_chars(int_part, |c| c.is_ascii_digit() || c == b'_')
    };
    if !int_ok {
        return None;
    }
    let (frac, exp) = match rest.find(['e', 'E']) {
        Some(k) => (&rest[..k], Some(&rest[k + 1..])),
        None => (rest, None),
    };
    if !all_chars(frac, |c| c.is_ascii_digit() || c == b'_') {
        return None;
    }
    if let Some(e) = exp {
        let eb = e.as_bytes();
        if eb.len() < 2 || !(eb[0] == b'+' || eb[0] == b'-') || !all_chars(&e[1..], |c| c.is_ascii_digit()) {
            return None;
        }
    }
    let _ = b;
    let text: String = body.chars().filter(|c| *c != '_').collect();
    let text = if text.starts_with('.') { format!("0{text}") } else { text };
    let text = if text.ends_with('.') || text.contains(".e") || text.contains(".E") {
        text.replacen('.', ".0", 1)
    } else {
        text
    };
    Some(Ok(sign * text.parse::<f64>().unwrap_or(f64::NAN)))
}

/// PyYAML's timestamp resolver, which the port does not decide.
fn looks_timestamp(s: &str) -> bool {
    let b = s.as_bytes();
    b.len() >= 8 && b[..4].iter().all(u8::is_ascii_digit) && b[4] == b'-'
}

/// A plain scalar, resolved.
fn resolve_plain(s: &str) -> R<Y> {
    if matches!(s, "" | "~" | "null" | "Null" | "NULL") {
        return Ok(Y::Null);
    }
    if let Some(b) = yaml_bool(s) {
        return Ok(Y::Bool(b));
    }
    if let Some(i) = yaml_int(s) {
        return Ok(Y::Int(i?));
    }
    if let Some(f) = yaml_float(s) {
        return Ok(Y::Float(f?));
    }
    if looks_timestamp(s) {
        return undecided("a timestamp-like value in config.yaml");
    }
    if s == "<<" || s == "=" {
        return undecided("a merge or value key in config.yaml");
    }
    Ok(Y::Str(s.to_string()))
}

/// Strips a comment: ` #` to the end, outside quotes (the caller has
/// checked that a quoted scalar ends the line).
fn strip_comment(s: &str) -> &str {
    let b = s.as_bytes();
    for i in 0..b.len() {
        if b[i] == b'#' && (i == 0 || b[i - 1] == b' ') {
            return s[..i].trim_end_matches(' ');
        }
    }
    s.trim_end_matches(' ')
}

/// One scalar written after `key:` or as an item, on one line.
fn scalar(text: &str) -> R<Y> {
    let t = text.trim_start_matches(' ');
    if t.is_empty() || t.starts_with('#') {
        return Ok(Y::Null);
    }
    let first = t.as_bytes()[0];
    if first == b'\'' {
        let mut out = String::new();
        let chars: Vec<char> = t.chars().collect();
        let mut i = 1;
        loop {
            match chars.get(i) {
                None => return undecided("a quoted scalar over several lines in config.yaml"),
                Some('\'') if chars.get(i + 1) == Some(&'\'') => {
                    out.push('\'');
                    i += 2;
                }
                Some('\'') => {
                    i += 1;
                    break;
                }
                Some(c) => {
                    out.push(*c);
                    i += 1;
                }
            }
        }
        let rest: String = chars[i..].iter().collect();
        if !strip_comment(&rest).trim().is_empty() {
            return undecided("text after a quoted scalar in config.yaml");
        }
        return Ok(Y::Str(out));
    }
    if first == b'"' {
        let chars: Vec<char> = t.chars().collect();
        let mut out = String::new();
        let mut i = 1;
        loop {
            match chars.get(i) {
                None => return undecided("a quoted scalar over several lines in config.yaml"),
                Some('"') => {
                    i += 1;
                    break;
                }
                Some('\\') => {
                    let e = chars.get(i + 1).copied();
                    i += 2;
                    let hex = |n: usize, i: &mut usize| -> R<char> {
                        let h: String = chars.get(*i..*i + n).map(|s| s.iter().collect()).unwrap_or_default();
                        *i += n;
                        match u32::from_str_radix(&h, 16).ok().and_then(char::from_u32) {
                            Some(c) if h.len() == n => Ok(c),
                            _ => undecided("an escape in config.yaml the port does not read"),
                        }
                    };
                    let c = match e {
                        Some('0') => '\0',
                        Some('a') => '\x07',
                        Some('b') => '\x08',
                        Some('t') | Some('\t') => '\t',
                        Some('n') => '\n',
                        Some('v') => '\x0b',
                        Some('f') => '\x0c',
                        Some('r') => '\r',
                        Some('e') => '\x1b',
                        Some(' ') => ' ',
                        Some('"') => '"',
                        Some('/') => '/',
                        Some('\\') => '\\',
                        Some('N') => '\u{85}',
                        Some('_') => '\u{a0}',
                        Some('L') => '\u{2028}',
                        Some('P') => '\u{2029}',
                        Some('x') => hex(2, &mut i)?,
                        Some('u') => hex(4, &mut i)?,
                        Some('U') => hex(8, &mut i)?,
                        _ => return undecided("an escape in config.yaml the port does not read"),
                    };
                    out.push(c);
                }
                Some(c) => {
                    out.push(*c);
                    i += 1;
                }
            }
        }
        let rest: String = chars[i..].iter().collect();
        if !strip_comment(&rest).trim().is_empty() {
            return undecided("text after a quoted scalar in config.yaml");
        }
        return Ok(Y::Str(out));
    }
    let v = strip_comment(t);
    if v == "{}" {
        return Ok(Y::Map(vec![]));
    }
    if v == "[]" {
        return Ok(Y::List(vec![]));
    }
    if b"[]{}&*!|>%@`,?".contains(&first) || (first == b'-' && (v == "-" || v.starts_with("- "))) {
        return undecided("YAML in config.yaml the port does not read");
    }
    if v.contains(": ") || v.ends_with(':') || v.contains(" #") || v.contains('\t') {
        return undecided("YAML in config.yaml the port does not read");
    }
    resolve_plain(v)
}

/// A key before `:`.
fn key_of(text: &str) -> R<Y> {
    if text.starts_with(['\'', '"']) {
        return scalar(text);
    }
    let text = text.trim_end_matches(' ');
    if text.is_empty() || b"[]{}&*!|>%@`,?#-".contains(&text.as_bytes()[0]) || text.contains('\t') {
        return undecided("a key in config.yaml the port does not read");
    }
    resolve_plain(text)
}

/// Where `key: value` splits: the first `:` followed by a space or the end
/// of the line, outside a leading quoted key.
fn split_key(line: &str) -> Option<(&str, &str)> {
    let b = line.as_bytes();
    let start = if b.first() == Some(&b'"') || b.first() == Some(&b'\'') {
        let q = b[0];
        let mut i = 1;
        while i < b.len() {
            if b[i] == q {
                if q == b'\'' && b.get(i + 1) == Some(&b'\'') {
                    i += 2;
                    continue;
                }
                if q == b'"' && b[i - 1] == b'\\' {
                    i += 1;
                    continue;
                }
                break;
            }
            i += 1;
        }
        i + 1
    } else {
        0
    };
    for i in start..b.len() {
        if b[i] == b':' && (i + 1 == b.len() || b[i + 1] == b' ') {
            return Some((&line[..i], &line[i + 1..]));
        }
        if b[i] == b'#' && i > 0 && b[i - 1] == b' ' {
            return None;
        }
    }
    None
}

/// `yaml.safe_load` of the subset the port reads.
pub fn safe_load(text: &str) -> R<Y> {
    if text.contains('\t') || text.contains('\r') {
        return undecided("tabs or carriage returns in config.yaml");
    }
    let mut lines: Vec<(usize, &str)> = Vec::new();
    for raw in text.split('\n') {
        let t = raw.trim_start_matches(' ');
        if t.is_empty() || t.starts_with('#') {
            continue;
        }
        if raw == "---" || raw == "..." || raw.starts_with("--- ") || raw.starts_with('%') {
            if raw == "---" && lines.is_empty() {
                continue;
            }
            return undecided("YAML directives or documents in config.yaml");
        }
        lines.push((raw.len() - t.len(), t));
    }
    if lines.is_empty() {
        return Ok(Y::Null);
    }
    let (v, used) = block(&lines, 0, lines[0].0)?;
    if used != lines.len() {
        return undecided("YAML in config.yaml the port does not read");
    }
    Ok(v)
}

/// A block node at `indent` starting at line `i`: a mapping or a sequence.
fn block(lines: &[(usize, &str)], mut i: usize, indent: usize) -> R<(Y, usize)> {
    if lines[i].1 == "-" || lines[i].1.starts_with("- ") {
        let mut items = Vec::new();
        while i < lines.len() && lines[i].0 == indent && (lines[i].1 == "-" || lines[i].1.starts_with("- ")) {
            let item = lines[i].1.trim_start_matches('-');
            if split_key(item.trim_start_matches(' ')).is_some() {
                return undecided("a mapping inside a sequence in config.yaml");
            }
            items.push(scalar(item)?);
            i += 1;
        }
        if i < lines.len() && lines[i].0 > indent {
            return undecided("YAML in config.yaml the port does not read");
        }
        return Ok((Y::List(items), i));
    }
    let mut pairs: Vec<(Y, Y)> = Vec::new();
    while i < lines.len() && lines[i].0 == indent {
        let (k, v) = match split_key(lines[i].1) {
            Some(kv) => kv,
            None => return undecided("YAML in config.yaml the port does not read"),
        };
        let key = key_of(k)?;
        let rest = strip_comment(v).trim_start_matches(' ');
        i += 1;
        let value = if rest.is_empty() {
            if i < lines.len() && lines[i].0 > indent {
                let (v, next) = block(lines, i, lines[i].0)?;
                i = next;
                v
            } else if i < lines.len() && lines[i].0 == indent && (lines[i].1 == "-" || lines[i].1.starts_with("- ")) {
                let (v, next) = block(lines, i, indent)?;
                i = next;
                v
            } else {
                Y::Null
            }
        } else {
            if i < lines.len() && lines[i].0 > indent {
                return undecided("a scalar over several lines in config.yaml");
            }
            scalar(v)?
        };
        // A repeated key keeps its first place and takes the last value.
        match pairs.iter_mut().find(|(k2, _)| *k2 == key) {
            Some(p) => p.1 = value,
            None => pairs.push((key, value)),
        }
    }
    if i < lines.len() && lines[i].0 > indent {
        return undecided("YAML in config.yaml the port does not read");
    }
    Ok((Y::Map(pairs), i))
}

/// pydantic's lax `int` from a YAML value.
fn valid_int(v: &Y) -> bool {
    match v {
        Y::Bool(_) | Y::Int(_) => true,
        Y::Float(f) => f.is_finite() && f.fract() == 0.0,
        Y::Str(s) => {
            let t = s.trim();
            let t = match t.find('.') {
                Some(d) if t[d + 1..].bytes().all(|c| c == b'0') => &t[..d],
                Some(_) => return false,
                None => t,
            };
            let body = t.strip_prefix(['+', '-']).unwrap_or(t);
            !body.is_empty()
                && body.bytes().all(|c| c.is_ascii_digit() || c == b'_')
                && !body.starts_with('_')
                && !body.ends_with('_')
                && !body.contains("__")
        }
        _ => false,
    }
}

/// pydantic's lax `bool` from a YAML value.
fn valid_bool(v: &Y) -> bool {
    match v {
        Y::Bool(_) => true,
        Y::Int(i) => *i == 0 || *i == 1,
        Y::Float(f) => *f == 0.0 || *f == 1.0,
        Y::Str(s) => matches!(
            s.to_ascii_lowercase().as_str(),
            "0" | "off" | "f" | "false" | "n" | "no" | "1" | "on" | "t" | "true" | "y" | "yes"
        ),
        _ => false,
    }
}

fn is_str(v: &Y) -> bool {
    matches!(v, Y::Str(_))
}

/// `Config(**filtered)` accepts these values.
fn valid_config(pairs: &[(Y, Y)]) -> bool {
    for (k, v) in pairs {
        let key = match k {
            Y::Str(s) => s.as_str(),
            _ => continue,
        };
        let ok = match key {
            "model" | "gateway_router" | "switchyard_route_id" | "switchyard_capable_model" | "gate_mode"
            | "verifier_client" | "matcher_model" | "envelope_source" | "pathway_store_backend" | "voice_engine"
            | "voice_model" | "voice_device" | "voice_compute_type" | "voice_server_url" => is_str(v),
            "max_task_chars" | "z3_timeout_ms" => valid_int(v),
            "data_dir" => is_str(v),
            "auto_tend" => *v == Y::Null || valid_bool(v),
            "shell_allow_decomposition" | "gate_ask" | "voice_cleanup" => valid_bool(v),
            "gateway_local_model" | "switchyard_efficient_model" | "switchyard_api_key_env" | "llm_backend"
            | "llm_base_url" | "llm_host_kind" | "llm_host_model" | "floor_report" | "voice_cleanup_model"
            | "voice_cleanup_base_url" => *v == Y::Null || is_str(v),
            "llm_context_window" => *v == Y::Null || valid_int(v),
            "floor" => match v {
                Y::Map(inner) => inner.iter().all(|(k2, v2)| match k2 {
                    Y::Str(s) => match s.as_str() {
                        "backend" => is_str(v2),
                        "notify_cmd" | "tmux_socket" | "coppice_socket" => *v2 == Y::Null || is_str(v2),
                        _ => true,
                    },
                    _ => true,
                }),
                _ => false,
            },
            _ => true,
        };
        if !ok {
            return false;
        }
    }
    true
}

/// A field of `load_config(path)`, or None where `load_config` raises.
pub(crate) fn load_field(r: &Runner, path: &str, field: &str, default: &str) -> R<Option<String>> {
    let _f = super::frames::frame();
    if !super::paths::exists(path)? {
        return Ok(Some(default.to_string()));
    }
    let bytes = match std::fs::read(super::paths::os_path("open", path)?) {
        Ok(b) => b,
        Err(e) => return Err(super::paths::os_error(&e, path).into()),
    };
    let text = match String::from_utf8(bytes) {
        Ok(t) => t,
        Err(_) => return Ok(None),
    };
    let _ = r;
    let raw = safe_load(&text)?;
    let pairs = match raw {
        Y::Null | Y::Bool(false) => vec![],
        Y::Int(0) => vec![],
        Y::Float(f) if f == 0.0 => vec![],
        Y::Str(ref s) if s.is_empty() => vec![],
        Y::List(ref l) if l.is_empty() => vec![],
        Y::Map(p) => p,
        // Any other document: raw.items() raises.
        _ => return Ok(None),
    };
    if !valid_config(&pairs) {
        return Ok(None);
    }
    for (k, v) in &pairs {
        if *k == Y::Str(field.into()) {
            if let Y::Str(s) = v {
                return Ok(Some(s.clone()));
            }
        }
    }
    Ok(Some(default.to_string()))
}

impl Runner {
    /// The config file beside the gate root: `Path(root).parent / "config.yaml"`.
    pub fn config_path(&self) -> String {
        path_join(&path_parent(&self.root), "config.yaml")
    }

    /// `gate.resolve_gate_mode(None, root=...)`: config's gate_mode when it
    /// is shadow or enforce, else shadow, and shadow on any error.
    pub fn config_gate_mode(&self) -> R<String> {
        let _f = super::frames::frame();
        let got = catch(load_field(self, &self.config_path(), "gate_mode", "shadow"))?;
        Ok(match got {
            Ok(Some(m)) if m == "shadow" || m == "enforce" => m,
            _ => "shadow".into(),
        })
    }

    /// `gate._configured_verifier_client(root)[1]`: config's
    /// verifier_client, or python on any error.
    pub fn verifier_client(&self) -> R<String> {
        let _f = super::frames::frame();
        Ok(match catch(load_field(self, &self.config_path(), "verifier_client", "python"))? {
            Ok(Some(c)) if !c.is_empty() => c,
            _ => "python".into(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn map(text: &str) -> Vec<(Y, Y)> {
        match safe_load(text).unwrap() {
            Y::Map(p) => p,
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn the_file_save_config_writes_reads_back() {
        let p = map(
            "auto_tend: null\ndata_dir: /home/u/.opendaisugi\nfloor:\n  backend: auto\n  notify_cmd: null\ngate_ask: false\n\
             gate_mode: shadow\nmax_task_chars: 4000\nvoice_server_url: http://127.0.0.1:7477\nz3_timeout_ms: 500\n",
        );
        assert!(valid_config(&p));
        assert!(p.contains(&(Y::Str("gate_mode".into()), Y::Str("shadow".into()))));
        assert!(p.contains(&(Y::Str("gate_ask".into()), Y::Bool(false))));
    }

    #[test]
    fn scalars_resolve_as_pyyaml_resolves_them() {
        for (t, want) in [
            ("yes", Y::Bool(true)),
            ("Off", Y::Bool(false)),
            ("0x1F", Y::Int(31)),
            ("010", Y::Int(8)),
            ("1_000", Y::Int(1000)),
            ("1e3", Y::Str("1e3".into())),
            ("1.5", Y::Float(1.5)),
            ("~", Y::Null),
            ("'x y'", Y::Str("x y".into())),
            ("\"a\\tb\"", Y::Str("a\tb".into())),
            ("python # comment", Y::Str("python".into())),
        ] {
            assert_eq!(scalar(t).unwrap(), want, "{t}");
        }
        assert!(!valid_config(&map("max_task_chars: 1.5\n")));
        assert!(!valid_config(&map("gate_mode: 5\n")));
        assert!(valid_config(&map("max_task_chars: '1_000'\nshell_allow_decomposition: 'Yes'\n")));
        assert!(safe_load("a: &x 1\n").is_err());
    }
}
