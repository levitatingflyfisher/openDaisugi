//! Python `str` rules on Rust strings.
//!
//! A Python string can hold a lone UTF-16 surrogate (U+D800 to U+DFFF),
//! which a Rust string cannot. The JSON reader and the environment reader
//! map each lone surrogate to one character of a reserved block at the top
//! of plane 16 (U+10F800 to U+10FFFF, one for each surrogate), and every
//! rule here reads such a character as the surrogate it stands for. Input
//! that already holds a character of that block is not decided by the port.

use super::tables;

/// The first character of the block that stands for the lone surrogates.
pub const SURROGATE_BASE: u32 = 0x10F800;

/// The character that stands for the lone surrogate `unit`.
pub fn surrogate_char(unit: u32) -> char {
    char::from_u32(SURROGATE_BASE + (unit - 0xD800)).expect("the block is valid")
}

/// The surrogate `c` stands for, when it stands for one.
pub fn as_surrogate(c: char) -> Option<u32> {
    let cp = c as u32;
    (cp >= SURROGATE_BASE).then(|| 0xD800 + (cp - SURROGATE_BASE))
}

/// The character for Python code point `cp`: itself, or for a lone
/// surrogate the character that stands for it.
pub fn cp_char(cp: u32) -> char {
    match char::from_u32(cp) {
        Some(c) => c,
        None => surrogate_char(cp),
    }
}

pub fn cp_char_string(cp: u32) -> String {
    cp_char(cp).to_string()
}

pub fn has_surrogate(s: &str) -> bool {
    s.chars().any(|c| as_surrogate(c).is_some())
}

fn in_ranges(table: &[(u32, u32)], cp: u32) -> bool {
    table
        .binary_search_by(|&(lo, hi)| {
            if hi < cp {
                std::cmp::Ordering::Less
            } else if lo > cp {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Equal
            }
        })
        .is_ok()
}

/// `c.isspace()`.
pub fn is_space(c: char) -> bool {
    if c.is_ascii() {
        return matches!(c, '\t' | '\n' | '\u{b}' | '\u{c}' | '\r' | '\u{1c}'..='\u{1f}' | ' ');
    }
    as_surrogate(c).is_none() && in_ranges(tables::SPACE, c as u32)
}

/// `c.isalnum()`.
pub fn is_alnum(c: char) -> bool {
    if c.is_ascii() {
        return c.is_ascii_alphanumeric();
    }
    as_surrogate(c).is_none() && in_ranges(tables::ALNUM, c as u32)
}

/// A `\w` character of Python's `re`: `isalnum()` or `_`.
pub fn is_word(c: char) -> bool {
    c == '_' || is_alnum(c)
}

/// `c.isdecimal()`, the `\d` of Python's `re`.
pub fn is_decimal(c: char) -> bool {
    if c.is_ascii() {
        return c.is_ascii_digit();
    }
    as_surrogate(c).is_none() && in_ranges(tables::DECIMAL, c as u32)
}

/// `c.isdigit()`.
pub fn is_digit(c: char) -> bool {
    if c.is_ascii() {
        return c.is_ascii_digit();
    }
    as_surrogate(c).is_none() && in_ranges(tables::DIGIT, c as u32)
}

/// `s.isdigit()`: every character a digit, and at least one.
pub fn str_is_digit(s: &str) -> bool {
    !s.is_empty() && s.chars().all(is_digit)
}

/// `c.isprintable()`.
pub fn is_printable(c: char) -> bool {
    if c.is_ascii() {
        return (' '..='~').contains(&c);
    }
    as_surrogate(c).is_none() && in_ranges(tables::PRINTABLE, c as u32)
}

/// `s.strip()`.
pub fn strip(s: &str) -> &str {
    s.trim_matches(is_space)
}

/// `s.lstrip()`.
pub fn lstrip(s: &str) -> &str {
    s.trim_start_matches(is_space)
}

/// `s.rstrip()`.
pub fn rstrip(s: &str) -> &str {
    s.trim_end_matches(is_space)
}

/// `s.split()`.
pub fn split(s: &str) -> Vec<&str> {
    s.split(is_space).filter(|w| !w.is_empty()).collect()
}

/// `len(s)`.
pub fn len(s: &str) -> usize {
    s.chars().count()
}

/// `s[:n]` on code points.
pub fn head(s: &str, n: usize) -> &str {
    match s.char_indices().nth(n) {
        Some((i, _)) => &s[..i],
        None => s,
    }
}

/// `s.splitlines()`.
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

fn map_chars(s: &str, table: &[(u32, &str)], ascii: fn(char) -> char) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if c.is_ascii() {
            out.push(ascii(c));
            continue;
        }
        match table.binary_search_by_key(&(c as u32), |&(cp, _)| cp) {
            Ok(i) => out.push_str(table[i].1),
            Err(_) => out.push(c),
        }
    }
    out
}

/// `s.lower()`. Python maps a final capital sigma by its context, which
/// this table cannot; a string holding U+03A3 is lowered char by char with
/// the context rule applied here.
pub fn lower(s: &str) -> String {
    if !s.contains('\u{3a3}') {
        return map_chars(s, tables::LOWER, |c| c.to_ascii_lowercase());
    }
    let chars: Vec<char> = s.chars().collect();
    let mut out = String::with_capacity(s.len());
    for (i, &c) in chars.iter().enumerate() {
        if c == '\u{3a3}' {
            out.push(if final_sigma(&chars, i) { '\u{3c2}' } else { '\u{3c3}' });
        } else {
            out.push_str(&map_chars(&c.to_string(), tables::LOWER, |c| c.to_ascii_lowercase()));
        }
    }
    out
}

/// CPython's final-sigma rule: a cased letter comes before (skipping case
/// ignorable characters) and none comes after. The two properties come
/// from tables read off Python's own `str.lower`.
fn final_sigma(chars: &[char], i: usize) -> bool {
    let cased = |c: char| in_ranges(tables::SIGMA_CASED, c as u32);
    let ignorable = |c: char| in_ranges(tables::SIGMA_IGNORABLE, c as u32);
    let mut j = i;
    let mut before = false;
    while j > 0 {
        j -= 1;
        if ignorable(chars[j]) {
            continue;
        }
        before = cased(chars[j]);
        break;
    }
    if !before {
        return false;
    }
    for &c in &chars[i + 1..] {
        if ignorable(c) {
            continue;
        }
        return !cased(c);
    }
    true
}

/// `s.upper()`.
pub fn upper(s: &str) -> String {
    map_chars(s, tables::UPPER, |c| c.to_ascii_uppercase())
}

/// `s.casefold()`.
pub fn casefold(s: &str) -> String {
    map_chars(s, tables::CASEFOLD, |c| c.to_ascii_lowercase())
}

/// `repr(s)`.
pub fn repr(s: &str) -> String {
    let quote = if s.contains('\'') && !s.contains('"') { '"' } else { '\'' };
    let mut b = String::with_capacity(s.len() + 2);
    b.push(quote);
    for c in s.chars() {
        if c == quote || c == '\\' {
            b.push('\\');
            b.push(c);
            continue;
        }
        match c {
            '\t' => b.push_str("\\t"),
            '\n' => b.push_str("\\n"),
            '\r' => b.push_str("\\r"),
            c if (c as u32) < 0x20 || c as u32 == 0x7f => b.push_str(&format!("\\x{:02x}", c as u32)),
            c if c.is_ascii() => b.push(c),
            c => match as_surrogate(c) {
                Some(u) => b.push_str(&format!("\\u{u:04x}")),
                None if is_printable(c) => b.push(c),
                None => {
                    let cp = c as u32;
                    if cp < 0x100 {
                        b.push_str(&format!("\\x{cp:02x}"));
                    } else if cp < 0x10000 {
                        b.push_str(&format!("\\u{cp:04x}"));
                    } else {
                        b.push_str(&format!("\\U{cp:08x}"));
                    }
                }
            },
        }
    }
    b.push(quote);
    b
}

/// `repr(list_of_str)`.
pub fn repr_list(ss: &[String]) -> String {
    let parts: Vec<String> = ss.iter().map(|s| repr(s)).collect();
    format!("[{}]", parts.join(", "))
}

/// The UnicodeEncodeError the UTF-8 codec raises for the surrogates at
/// code point positions `start..end` of `s`.
fn encode_error(s: &[char], start: usize, end: usize) -> super::PyErr {
    let msg = if end - start == 1 {
        let u = as_surrogate(s[start]).unwrap_or(s[start] as u32);
        format!("'utf-8' codec can't encode character '\\u{u:04x}' in position {start}: surrogates not allowed")
    } else {
        format!("'utf-8' codec can't encode characters in position {start}-{}: surrogates not allowed", end - 1)
    };
    super::PyErr::new("UnicodeEncodeError", msg)
}

/// UTF-8 of `s` as CPython's encoder writes it: a run of lone surrogates
/// is one error, and with `escape` (the surrogateescape handler) each
/// surrogate from U+DC80 to U+DCFF at the start of a run is the raw byte it
/// stands for; the error then covers the rest of the run.
fn encode(s: &str, escape: bool) -> Result<Vec<u8>, super::PyErr> {
    if !has_surrogate(s) {
        return Ok(s.as_bytes().to_vec());
    }
    let cs: Vec<char> = s.chars().collect();
    let mut out = Vec::with_capacity(s.len());
    let mut i = 0;
    while i < cs.len() {
        if as_surrogate(cs[i]).is_none() {
            let mut buf = [0u8; 4];
            out.extend_from_slice(cs[i].encode_utf8(&mut buf).as_bytes());
            i += 1;
            continue;
        }
        let mut end = i;
        while end < cs.len() && as_surrogate(cs[end]).is_some() {
            end += 1;
        }
        let mut k = i;
        if escape {
            while k < end {
                match as_surrogate(cs[k]) {
                    Some(u) if (0xDC80..=0xDCFF).contains(&u) => out.push((u - 0xDC00) as u8),
                    _ => break,
                }
                k += 1;
            }
        }
        if k < end {
            return Err(encode_error(&cs, k, end));
        }
        i = end;
    }
    Ok(out)
}

/// `s.encode("utf-8")` in strict mode: a lone surrogate raises.
pub fn encode_utf8_strict(s: &str) -> Result<Vec<u8>, super::PyErr> {
    encode(s, false)
}

/// `os.fsencode(s)`: UTF-8 with the surrogateescape handler, so a surrogate
/// from U+DC80 to U+DCFF is the raw byte it stands for, and any other
/// surrogate raises.
pub fn fsencode(s: &str) -> Result<Vec<u8>, super::PyErr> {
    encode(s, true)
}

/// `os.fsdecode(b)`: UTF-8 with surrogateescape.
pub fn fsdecode(b: &[u8]) -> String {
    let mut out = String::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match std::str::from_utf8(&b[i..]) {
            Ok(s) => {
                out.push_str(s);
                break;
            }
            Err(e) => {
                let good = e.valid_up_to();
                out.push_str(std::str::from_utf8(&b[i..i + good]).expect("valid prefix"));
                i += good;
                let bad = e.error_len().unwrap_or(b.len() - i);
                for &x in &b[i..i + bad] {
                    out.push(surrogate_char(0xDC00 + x as u32));
                }
                i += bad;
            }
        }
    }
    out
}

/// `b.decode("utf-8", "replace")`.
pub fn decode_utf8_replace(b: &[u8]) -> String {
    String::from_utf8_lossy(b).into_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classes_match_python_for_common_characters() {
        assert!(is_space('\u{85}') && is_space('\u{3000}') && !is_space('\u{200b}'));
        assert!(is_word('é') && is_word('中') && is_word('½') && !is_word('\u{301}'));
        assert!(is_decimal('٣') && !is_decimal('²') && is_digit('²'));
        assert!(!is_printable('\u{a0}') && is_printable('é'));
        assert_eq!(repr("a\u{a0}é\u{f0000}"), "'a\\xa0é\\U000f0000'");
        assert_eq!(repr(&format!("x{}", surrogate_char(0xd800))), "'x\\ud800'");
        assert_eq!(casefold("Straße ﬁ K"), "strasse fi k");
        assert_eq!(lower("İ"), "i\u{307}");
        assert_eq!(lower("ΟΔΟΣ ΟΔΟΣ"), "οδος οδος");
    }

    #[test]
    fn fs_codec_round_trips_escaped_bytes() {
        let s = fsdecode(b"a\xffb\xc3");
        assert_eq!(fsencode(&s).unwrap(), b"a\xffb\xc3");
        assert!(fsencode(&surrogate_char(0xd800).to_string()).is_err());
        let sc = |u: u32| surrogate_char(u).to_string();
        let run = format!("a{}{}{}", sc(0xd800), sc(0xd801), sc(0xd802));
        assert_eq!(
            encode_utf8_strict(&run).unwrap_err().msg,
            "'utf-8' codec can't encode characters in position 1-3: surrogates not allowed"
        );
        assert_eq!(
            fsencode(&format!("a{}{}", sc(0xdc80), sc(0xd800))).unwrap_err().msg,
            "'utf-8' codec can't encode character '\\ud800' in position 2: surrogates not allowed"
        );
        assert_eq!(
            fsencode(&format!("a{}{}", sc(0xd800), sc(0xdc80))).unwrap_err().msg,
            "'utf-8' codec can't encode characters in position 1-2: surrogates not allowed"
        );
        assert_eq!(fsencode(&format!("a{}{}", sc(0xdc80), sc(0xdcff))).unwrap(), b"a\x80\xff");
    }
}
