//! Text measured in terminal cells. A wide glyph, such as a CJK ideograph,
//! takes two cells. A control rune takes none. The server uses printable;
//! the width functions serve the floor's screens, a later slice.
#![allow(dead_code)]

/// The one cell truncate ends a cut string with.
pub const ELLIPSIS: &str = "…";

fn wide_rune(r: char) -> bool {
    let r = r as u32;
    match r {
        0x1100..=0x115F => true,
        0x2E80..=0xA4CF => !(r == 0x303F || (0x4DC0..=0x4DFF).contains(&r)),
        0xAC00..=0xD7A3 => true,
        0xF900..=0xFAFF => true,
        0xFF00..=0xFF60 => true,
        0xFFE0..=0xFFE6 => true,
        0x20000..=0x3FFFD => true,
        _ => false,
    }
}

/// Whether text is a single rune a terminal draws two cells wide.
pub fn is_wide(text: &str) -> bool {
    let mut it = text.chars();
    match (it.next(), it.next()) {
        (Some(c), None) => wide_rune(c),
        _ => false,
    }
}

fn rune_width(r: char) -> usize {
    if (r as u32) < 0x20 || r as u32 == 0x7f {
        return 0;
    }
    if wide_rune(r) {
        2
    } else {
        1
    }
}

/// The cells s takes on one terminal line.
pub fn width(s: &str) -> usize {
    s.chars().map(rune_width).sum()
}

/// s cut to at most max cells, ending with the ellipsis when cut.
pub fn truncate(s: &str, max: i64) -> String {
    if max <= 0 {
        return String::new();
    }
    let max = max as usize;
    if width(s) <= max {
        return s.to_string();
    }
    if max == 1 {
        return match s.chars().next() {
            Some(r) if rune_width(r) <= 1 => r.to_string(),
            _ => String::new(),
        };
    }
    let room = max - width(ELLIPSIS);
    let mut out = String::new();
    let mut w = 0;
    for r in s.chars() {
        let rw = rune_width(r);
        if w + rw > room {
            break;
        }
        out.push(r);
        w += rw;
    }
    out.push_str(ELLIPSIS);
    out
}

/// s without its C0 and C1 control runes, DEL, and U+FFFD (what a byte that
/// was not UTF-8 became), cut to max runes when max is above zero.
pub fn printable(s: &str, max: usize) -> String {
    let mut out = String::new();
    let mut n = 0;
    for r in s.chars() {
        let c = r as u32;
        if c < 0x20 || (0x7f..=0x9f).contains(&c) || r == '\u{FFFD}' {
            continue;
        }
        if max > 0 && n == max {
            break;
        }
        out.push(r);
        n += 1;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wide_knows_cjk_and_not_ascii() {
        assert!(is_wide("字") && is_wide("ア"));
        assert!(!is_wide("a") && !is_wide("") && !is_wide("ab") && !is_wide("䷀"));
    }

    #[test]
    fn printable_drops_controls_and_cuts() {
        assert_eq!(printable("a\u{1b}[31mb\u{9b}c\u{FFFD}", 0), "a[31mbc");
        assert_eq!(printable("abcdef", 3), "abc");
    }

    #[test]
    fn truncate_ends_with_ellipsis() {
        assert_eq!(truncate("abcdef", 4), "abc…");
        assert_eq!(truncate("字字", 1), "");
        assert_eq!(truncate("ab", 0), "");
    }
}
