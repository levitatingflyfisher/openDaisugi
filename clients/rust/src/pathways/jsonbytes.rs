//! The text `json.loads` reads from bytes.

use super::PwErr;
use crate::gate::py::text::cp_char;

/// `json.detect_encoding` picks UTF-8 (with or without a BOM), UTF-16 or
/// UTF-32 from the first bytes, and the bytes are decoded with
/// 'surrogatepass', so a lone surrogate is kept. A decode that fails is
/// Python's UnicodeDecodeError. The Go client's `pathways.JSONText` is the
/// reference.
pub fn json_text(b: &[u8]) -> Result<String, PwErr> {
    let fail = || PwErr::Invalid("UnicodeDecodeError: the bytes do not decode in the encoding json detects".into());
    if b.starts_with(b"\x00\x00\xfe\xff") || b.starts_with(b"\xff\xfe\x00\x00") {
        return utf32(&b[4..], b[0] == 0xff).ok_or_else(fail);
    }
    if b.starts_with(b"\xfe\xff") || b.starts_with(b"\xff\xfe") {
        return utf16(&b[2..], b[0] == 0xff).ok_or_else(fail);
    }
    if b.starts_with(b"\xef\xbb\xbf") {
        return utf8_pass(&b[3..]).ok_or_else(fail);
    }
    if b.len() >= 4 {
        if b[0] == 0 {
            return if b[1] != 0 { utf16(b, false) } else { utf32(b, false) }.ok_or_else(fail);
        }
        if b[1] == 0 {
            return if b[2] != 0 || b[3] != 0 { utf16(b, true) } else { utf32(b, true) }.ok_or_else(fail);
        }
    } else if b.len() == 2 {
        if b[0] == 0 {
            return utf16(b, false).ok_or_else(fail);
        }
        if b[1] == 0 {
            return utf16(b, true).ok_or_else(fail);
        }
    }
    utf8_pass(b).ok_or_else(fail)
}

/// UTF-8 with 'surrogatepass': an encoded surrogate (ED A0..BF xx) is a
/// lone surrogate.
fn utf8_pass(b: &[u8]) -> Option<String> {
    let mut out = String::new();
    let mut i = 0;
    while i < b.len() {
        match std::str::from_utf8(&b[i..]) {
            Ok(s) => {
                out.push_str(s);
                break;
            }
            Err(e) => {
                let good = e.valid_up_to();
                out.push_str(std::str::from_utf8(&b[i..i + good]).ok()?);
                i += good;
                if i + 2 < b.len() && b[i] == 0xED && (0xA0..=0xBF).contains(&b[i + 1]) && (0x80..=0xBF).contains(&b[i + 2]) {
                    let cp = 0xD000 | ((b[i + 1] as u32 & 0x3F) << 6) | (b[i + 2] as u32 & 0x3F);
                    out.push(cp_char(cp));
                    i += 3;
                } else {
                    return None;
                }
            }
        }
    }
    Some(out)
}

fn utf16(b: &[u8], le: bool) -> Option<String> {
    if b.len() % 2 != 0 {
        return None; // truncated data
    }
    let unit = |i: usize| if le { u16::from_le_bytes([b[i], b[i + 1]]) } else { u16::from_be_bytes([b[i], b[i + 1]]) };
    let mut out = String::new();
    let mut i = 0;
    while i < b.len() {
        let u = unit(i) as u32;
        let mut cp = u;
        if (0xD800..0xDC00).contains(&u) && i + 3 < b.len() {
            let v = unit(i + 2) as u32;
            if (0xDC00..0xE000).contains(&v) {
                cp = 0x10000 + ((u - 0xD800) << 10) + (v - 0xDC00);
                i += 2;
            }
        }
        out.push(cp_char(cp));
        i += 2;
    }
    Some(out)
}

fn utf32(b: &[u8], le: bool) -> Option<String> {
    if b.len() % 4 != 0 {
        return None; // truncated data
    }
    let mut out = String::new();
    for c in b.chunks(4) {
        let u = if le { u32::from_le_bytes([c[0], c[1], c[2], c[3]]) } else { u32::from_be_bytes([c[0], c[1], c[2], c[3]]) };
        if u > 0x10FFFF {
            return None; // code point not in range(0x110000)
        }
        out.push(cp_char(u));
    }
    Some(out)
}
