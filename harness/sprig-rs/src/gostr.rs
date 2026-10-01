//! Go string handling on bytes. A Go string is any bytes, not only UTF-8,
//! so the text a tool reads or a process prints is kept as bytes here, and
//! these follow the Go functions sprig calls on it.

/// Go's utf8.DecodeRune: the first rune of b and its size, or (U+FFFD, 1)
/// for a byte that does not start a valid sequence, and (U+FFFD, 0) for
/// no bytes.
pub fn decode_rune(b: &[u8]) -> (char, usize) {
    let Some(&c) = b.first() else {
        return ('\u{fffd}', 0);
    };
    let n = match c {
        0x00..=0x7f => return (c as char, 1),
        0xc2..=0xdf => 2,
        0xe0..=0xef => 3,
        0xf0..=0xf4 => 4,
        _ => return ('\u{fffd}', 1),
    };
    if b.len() < n {
        return ('\u{fffd}', 1);
    }
    match std::str::from_utf8(&b[..n]) {
        Ok(s) => (s.chars().next().unwrap_or('\u{fffd}'), n),
        Err(_) => ('\u{fffd}', 1),
    }
}

/// Go's utf8.DecodeLastRune.
pub fn decode_last_rune(b: &[u8]) -> (char, usize) {
    if b.is_empty() {
        return ('\u{fffd}', 0);
    }
    let end = b.len();
    let mut start = end - 1;
    if b[start] < 0x80 {
        return (b[start] as char, 1);
    }
    let lim = end.saturating_sub(4);
    while start > lim && (b[start] & 0xc0) == 0x80 {
        start -= 1;
    }
    let (r, size) = decode_rune(&b[start..end]);
    if start + size != end {
        return ('\u{fffd}', 1);
    }
    (r, size)
}

/// The runes of b as Go's `[]rune(string(b))` makes them: each byte that
/// is not valid UTF-8 becomes one U+FFFD.
pub fn runes(b: &[u8]) -> Vec<char> {
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        let (r, n) = decode_rune(&b[i..]);
        out.push(r);
        i += n;
    }
    out
}

/// Go's strings.TrimSpace.
pub fn trim_space(b: &[u8]) -> &[u8] {
    let mut s = b;
    while !s.is_empty() {
        let (r, n) = decode_rune(s);
        if n == 1 && r == '\u{fffd}' || !r.is_whitespace() {
            break;
        }
        s = &s[n..];
    }
    while !s.is_empty() {
        let (r, n) = decode_last_rune(s);
        if n == 1 && r == '\u{fffd}' || !r.is_whitespace() {
            break;
        }
        s = &s[..s.len() - n];
    }
    s
}

/// Go's strings.Fields.
pub fn fields(b: &[u8]) -> Vec<Vec<u8>> {
    let mut out = Vec::new();
    let mut cur: Option<usize> = None;
    let mut i = 0;
    while i < b.len() {
        let (r, n) = decode_rune(&b[i..]);
        let space = !(n == 1 && r == '\u{fffd}') && r.is_whitespace();
        match (space, cur) {
            (true, Some(st)) => {
                out.push(b[st..i].to_vec());
                cur = None;
            }
            (false, None) => cur = Some(i),
            _ => {}
        }
        i += n;
    }
    if let Some(st) = cur {
        out.push(b[st..].to_vec());
    }
    out
}

/// Go's strings.Count.
pub fn count(s: &[u8], sub: &[u8]) -> usize {
    if sub.is_empty() {
        return runes(s).len() + 1;
    }
    let mut n = 0;
    let mut i = 0;
    while let Some(j) = index(&s[i..], sub) {
        n += 1;
        i += j + sub.len();
    }
    n
}

/// Go's strings.Index.
pub fn index(s: &[u8], sub: &[u8]) -> Option<usize> {
    if sub.is_empty() {
        return Some(0);
    }
    if sub.len() > s.len() {
        return None;
    }
    (0..=s.len() - sub.len()).find(|&i| &s[i..i + sub.len()] == sub)
}

/// Go's strings.Replace(s, old, new, 1).
pub fn replace_once(s: &[u8], old: &[u8], new: &[u8]) -> Vec<u8> {
    match index(s, old) {
        Some(i) => [&s[..i], new, &s[i + old.len()..]].concat(),
        None => s.to_vec(),
    }
}

/// sprig's truncateRunes: s cut to n runes, where a long s is first made
/// runes (so a byte that is not UTF-8 becomes U+FFFD) and a short one is
/// kept as it is.
pub fn truncate_runes(s: &[u8], n: usize) -> Vec<u8> {
    if s.len() <= n {
        return s.to_vec();
    }
    let r = runes(s);
    if r.len() <= n {
        return s.to_vec();
    }
    r[..n].iter().collect::<String>().into_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decode_follows_go() {
        assert_eq!(decode_rune(b"a"), ('a', 1));
        assert_eq!(decode_rune("é".as_bytes()), ('é', 2));
        assert_eq!(decode_rune(b"\xff"), ('\u{fffd}', 1));
        assert_eq!(decode_rune(b"\xe2\x82"), ('\u{fffd}', 1));
        assert_eq!(decode_rune(b"\xed\xa0\x80"), ('\u{fffd}', 1));
        assert_eq!(decode_rune(b"\xc0\x80"), ('\u{fffd}', 1));
        assert_eq!(decode_rune(b""), ('\u{fffd}', 0));
        assert_eq!(decode_last_rune("aé".as_bytes()), ('é', 2));
        assert_eq!(decode_last_rune(b"a\x82"), ('\u{fffd}', 1));
        assert_eq!(decode_last_rune(b"\xe2\x82"), ('\u{fffd}', 1));
    }

    #[test]
    fn runes_replace_each_bad_byte() {
        assert_eq!(
            runes(b"\xff\xe2\x82a"),
            vec!['\u{fffd}', '\u{fffd}', '\u{fffd}', 'a']
        );
    }

    #[test]
    fn trim_and_fields() {
        assert_eq!(trim_space(" \t\u{a0}x y\u{3000}\n".as_bytes()), b"x y");
        assert_eq!(trim_space(b" \xff "), b"\xff");
        assert_eq!(trim_space(b"   "), b"");
        assert_eq!(
            fields(b"  a  b\tc\n"),
            vec![b"a".to_vec(), b"b".to_vec(), b"c".to_vec()]
        );
        assert!(fields(b"   ").is_empty());
    }

    #[test]
    fn count_follows_go() {
        assert_eq!(count(b"ab ab ab", b"ab"), 3);
        assert_eq!(count(b"aaa", b"aa"), 1);
        assert_eq!(count("hé".as_bytes(), b""), 3);
        assert_eq!(count(b"h\xff", b""), 3);
        assert_eq!(count(b"", b""), 1);
        assert_eq!(index(b"xxab", b"ab"), Some(2));
        assert_eq!(index(b"x", b"ab"), None);
        assert_eq!(replace_once(b"a b a", b"a", b"Z"), b"Z b a");
        assert_eq!(replace_once(b"ab", b"", b"Z"), b"Zab");
    }

    #[test]
    fn truncate_follows_go() {
        assert_eq!(truncate_runes(b"abc", 5), b"abc");
        assert_eq!(truncate_runes(b"abcdef", 3), b"abc");
        // Short in runes but long in bytes: kept as it is, bad byte too.
        let s = "中中\u{ff}".as_bytes().to_vec();
        let mut bad = "中中".as_bytes().to_vec();
        bad.push(0xff);
        assert_eq!(truncate_runes(&bad, 4), bad);
        assert_eq!(truncate_runes(&s, 2), "中中".as_bytes());
        // Long both ways: cut as runes, a bad byte becomes U+FFFD.
        assert_eq!(truncate_runes(b"\xffabc", 2), "\u{fffd}a".as_bytes());
    }
}
