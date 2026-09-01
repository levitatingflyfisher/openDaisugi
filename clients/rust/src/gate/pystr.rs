//! Python's string rules the gate relies on: `str.isspace`, `strip()`,
//! `split()`, `len()`, slicing on code points, and the hook's session id
//! sanitizer.

/// Python's `str.isspace` for one character.
pub fn is_py_space(c: char) -> bool {
    matches!(
        c,
        '\t' | '\n' | '\u{b}' | '\u{c}' | '\r' | '\u{1c}' | '\u{1d}' | '\u{1e}' | '\u{1f}' | ' ' | '\u{85}'
            | '\u{a0}' | '\u{1680}' | '\u{2028}' | '\u{2029}' | '\u{202f}' | '\u{205f}' | '\u{3000}'
    ) || ('\u{2000}'..='\u{200a}').contains(&c)
}

/// `str.strip()` with no argument.
pub fn py_strip(s: &str) -> &str {
    s.trim_matches(is_py_space)
}

/// `str.split()` with no argument.
pub fn py_split(s: &str) -> Vec<&str> {
    s.split(is_py_space).filter(|w| !w.is_empty()).collect()
}

/// `len(str)`: the number of code points.
pub fn py_len(s: &str) -> usize {
    s.chars().count()
}

/// `s[:n]` on code points.
pub fn py_head(s: &str, n: usize) -> &str {
    match s.char_indices().nth(n) {
        Some((i, _)) => &s[..i],
        None => s,
    }
}

/// `hook._safe_session_id` on a string: every character outside
/// `[A-Za-z0-9._-]` becomes `_`, dots are stripped from both ends, and the
/// result is cut to 128 characters, or `no-session` when empty.
pub fn safe_session_id(raw: &str) -> String {
    let mapped: String = raw
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') { c } else { '_' })
        .collect();
    let s = mapped.trim_matches('.');
    let s = if s.len() > 128 { &s[..128] } else { s };
    if s.is_empty() {
        "no-session".into()
    } else {
        s.to_string()
    }
}

/// Python's `str.splitlines()`.
pub fn splitlines(s: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut start = 0;
    let mut it = s.char_indices().peekable();
    while let Some((i, c)) = it.next() {
        match c {
            '\n' | '\u{b}' | '\u{c}' | '\u{1c}' | '\u{1d}' | '\u{1e}' | '\u{85}' | '\u{2028}' | '\u{2029}' => {
                out.push(&s[start..i]);
                start = i + c.len_utf8();
            }
            '\r' => {
                out.push(&s[start..i]);
                start = i + 1;
                if let Some(&(_, '\n')) = it.peek() {
                    it.next();
                    start += 1;
                }
            }
            _ => {}
        }
    }
    if start < s.len() {
        out.push(&s[start..]);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strip_split_and_lines_are_pythons() {
        assert_eq!(py_strip("\u{1c} a b\u{b}"), "a b");
        assert_eq!(py_split(" a\u{1f}b  c\u{3000}"), vec!["a", "b", "c"]);
        assert_eq!(splitlines("a\nb\r\nc\u{2028}d\re"), vec!["a", "b", "c", "d", "e"]);
        assert_eq!(splitlines("a\n"), vec!["a"]);
        assert_eq!(py_head("héllo", 2), "hé");
        assert_eq!(safe_session_id("..a/b.."), "a_b");
        assert_eq!(safe_session_id("é"), "_");
        assert_eq!(safe_session_id("..."), "no-session");
    }
}
