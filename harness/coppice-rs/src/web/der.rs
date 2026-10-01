//! The little DER the local CA needs: build a certificate the way Go's
//! x509.CreateCertificate lays it out, and read back the parts a later
//! run needs (the subject, the key id, the validity).

pub const SEQUENCE: u8 = 0x30;
pub const SET: u8 = 0x31;
pub const INTEGER: u8 = 0x02;
pub const BIT_STRING: u8 = 0x03;
pub const OCTET_STRING: u8 = 0x04;
pub const OID: u8 = 0x06;
pub const BOOLEAN: u8 = 0x01;
pub const UTF8_STRING: u8 = 0x0c;
pub const PRINTABLE_STRING: u8 = 0x13;
pub const UTC_TIME: u8 = 0x17;
pub const GENERALIZED_TIME: u8 = 0x18;

/// One TLV.
pub fn tlv(tag: u8, body: &[u8]) -> Vec<u8> {
    let mut out = vec![tag];
    let n = body.len();
    if n < 0x80 {
        out.push(n as u8);
    } else {
        let bytes = n.to_be_bytes();
        let skip = bytes.iter().take_while(|&&b| b == 0).count();
        out.push(0x80 | (bytes.len() - skip) as u8);
        out.extend_from_slice(&bytes[skip..]);
    }
    out.extend_from_slice(body);
    out
}

pub fn seq(parts: &[Vec<u8>]) -> Vec<u8> {
    tlv(SEQUENCE, &parts.concat())
}

/// An OID from its dotted arcs.
pub fn oid(arcs: &[u64]) -> Vec<u8> {
    let mut body = Vec::new();
    let mut push = |mut v: u64| {
        let mut tmp = vec![(v & 0x7f) as u8];
        v >>= 7;
        while v > 0 {
            tmp.push(0x80 | (v & 0x7f) as u8);
            v >>= 7;
        }
        tmp.reverse();
        body.extend(tmp);
    };
    push(arcs[0] * 40 + arcs[1]);
    for &a in &arcs[2..] {
        push(a);
    }
    tlv(OID, &body)
}

/// A non-negative INTEGER from big-endian bytes, in its shortest form.
pub fn uint(be: &[u8]) -> Vec<u8> {
    let skip = be.iter().take_while(|&&b| b == 0).count();
    let mut body: Vec<u8> = be[skip..].to_vec();
    if body.is_empty() {
        body.push(0);
    } else if body[0] & 0x80 != 0 {
        body.insert(0, 0);
    }
    tlv(INTEGER, &body)
}

pub fn bit_string(bytes: &[u8], unused: u8) -> Vec<u8> {
    let mut body = vec![unused];
    body.extend_from_slice(bytes);
    tlv(BIT_STRING, &body)
}

/// Go's asn1 printable-string test, which decides between
/// PrintableString and UTF8String for a Go string.
fn printable(s: &str) -> bool {
    s.bytes().all(|b| {
        b.is_ascii_alphanumeric()
            || matches!(
                b,
                b' ' | b'\'' | b'(' | b')' | b'+' | b',' | b'-' | b'.' | b'/' | b':' | b'=' | b'?'
            )
    })
}

/// A Go string as encoding/asn1 writes it.
pub fn go_string(s: &str) -> Vec<u8> {
    let tag = if printable(s) {
        PRINTABLE_STRING
    } else {
        UTF8_STRING
    };
    tlv(tag, s.as_bytes())
}

/// A pkix.Name with an organization and a common name, as Go orders the
/// attributes: O, then CN, each in its own RDN.
pub fn name(org: &str, cn: &str) -> Vec<u8> {
    let attr = |arcs: &[u64], v: &str| tlv(SET, &seq(&[oid(arcs), go_string(v)]));
    let mut rdns = vec![attr(&[2, 5, 4, 10], org)];
    if !cn.is_empty() {
        rdns.push(attr(&[2, 5, 4, 3], cn));
    }
    seq(&rdns)
}

/// A time as Go's asn1 writes a certificate's validity: UTCTime for the
/// years 1950 to 2049, GeneralizedTime else. secs is Unix seconds, UTC.
pub fn time(secs: i64) -> Vec<u8> {
    let (y, mo, d, h, mi, s) = civil(secs);
    if (1950..2050).contains(&y) {
        tlv(
            UTC_TIME,
            format!("{:02}{mo:02}{d:02}{h:02}{mi:02}{s:02}Z", y % 100).as_bytes(),
        )
    } else {
        tlv(
            GENERALIZED_TIME,
            format!("{y:04}{mo:02}{d:02}{h:02}{mi:02}{s:02}Z").as_bytes(),
        )
    }
}

/// Unix seconds as a UTC date and time.
pub fn civil(secs: i64) -> (i64, u32, u32, u32, u32, u32) {
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    // Howard Hinnant's days-to-civil.
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let y = if m <= 2 { y + 1 } else { y };
    (
        y,
        m,
        d,
        (rem / 3600) as u32,
        (rem % 3600 / 60) as u32,
        (rem % 60) as u32,
    )
}

/// The Unix seconds of a UTC date and time.
pub fn unix(y: i64, m: u32, d: u32, h: u32, mi: u32, s: u32) -> i64 {
    let y2 = if m <= 2 { y - 1 } else { y };
    let era = y2.div_euclid(400);
    let yoe = y2 - era * 400;
    let mp = (m as i64 + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d as i64 - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146097 + doe - 719468;
    days * 86400 + h as i64 * 3600 + mi as i64 * 60 + s as i64
}

/// A TLV read from DER: its tag, its body, and the whole of it.
#[derive(Clone, Copy)]
pub struct Item<'a> {
    pub tag: u8,
    pub body: &'a [u8],
    pub full: &'a [u8],
}

/// Reads one TLV from the front of b, and the rest after it.
pub fn read(b: &[u8]) -> Option<(Item<'_>, &[u8])> {
    if b.len() < 2 {
        return None;
    }
    let tag = b[0];
    let (len, head) = if b[1] < 0x80 {
        (b[1] as usize, 2)
    } else {
        let n = (b[1] & 0x7f) as usize;
        if n == 0 || n > 4 || b.len() < 2 + n {
            return None;
        }
        let mut len = 0usize;
        for &x in &b[2..2 + n] {
            len = len << 8 | x as usize;
        }
        (len, 2 + n)
    };
    if b.len() < head + len {
        return None;
    }
    Some((
        Item {
            tag,
            body: &b[head..head + len],
            full: &b[..head + len],
        },
        &b[head + len..],
    ))
}

/// Every TLV inside body, in order.
pub fn items(mut body: &[u8]) -> Option<Vec<Item<'_>>> {
    let mut out = Vec::new();
    while !body.is_empty() {
        let (it, rest) = read(body)?;
        out.push(it);
        body = rest;
    }
    Some(out)
}

/// The parts of a certificate a later run needs.
pub struct Cert<'a> {
    pub subject: &'a [u8],
    pub not_after: i64,
    pub spki_key: &'a [u8],
    pub subject_key_id: Option<&'a [u8]>,
}

fn read_time(it: &Item) -> Option<i64> {
    let s = std::str::from_utf8(it.body).ok()?;
    let s = s.strip_suffix('Z')?;
    let digits = |t: &str| t.bytes().all(|b| b.is_ascii_digit());
    let (y, rest) = match it.tag {
        UTC_TIME if s.len() == 12 && digits(s) => {
            let yy: i64 = s[..2].parse().ok()?;
            (if yy < 50 { 2000 + yy } else { 1900 + yy }, &s[2..])
        }
        GENERALIZED_TIME if s.len() == 14 && digits(s) => (s[..4].parse().ok()?, &s[4..]),
        _ => return None,
    };
    let n = |i: usize| rest[i..i + 2].parse::<u32>().ok();
    Some(unix(y, n(0)?, n(2)?, n(4)?, n(6)?, n(8)?))
}

/// Parses a certificate's DER far enough for the CA and the expiry check.
pub fn parse_cert(der: &[u8]) -> Option<Cert<'_>> {
    let (cert, rest) = read(der)?;
    if cert.tag != SEQUENCE || !rest.is_empty() {
        return None;
    }
    let parts = items(cert.body)?;
    if parts.len() != 3 || parts[0].tag != SEQUENCE {
        return None;
    }
    let tbs = items(parts[0].body)?;
    let mut i = 0;
    if tbs.first()?.tag == 0xa0 {
        i += 1;
    }
    let _serial = tbs.get(i)?;
    let _alg = tbs.get(i + 1)?;
    let _issuer = tbs.get(i + 2)?;
    let validity = items(tbs.get(i + 3)?.body)?;
    let subject = tbs.get(i + 4)?;
    let spki = items(tbs.get(i + 5)?.body)?;
    let not_after = read_time(validity.get(1)?)?;
    let key = spki.get(1)?;
    if key.tag != BIT_STRING || key.body.is_empty() {
        return None;
    }
    let mut skid = None;
    for t in &tbs[i + 6..] {
        if t.tag != 0xa3 {
            continue;
        }
        let exts = items(read(t.body)?.0.body)?;
        for e in exts {
            let f = items(e.body)?;
            if f.first()?.full == oid(&[2, 5, 29, 14]).as_slice() {
                let val = f.last()?;
                let inner = read(val.body)?.0;
                if inner.tag == OCTET_STRING {
                    skid = Some(inner.body);
                }
            }
        }
    }
    Some(Cert {
        subject: subject.full,
        not_after,
        spki_key: &key.body[1..],
        subject_key_id: skid,
    })
}

/// The base64 of b in the standard alphabet, padded.
pub fn base64(b: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for c in b.chunks(3) {
        let n = (c[0] as u32) << 16
            | (*c.get(1).unwrap_or(&0) as u32) << 8
            | *c.get(2).unwrap_or(&0) as u32;
        out.push(T[(n >> 18) as usize & 63] as char);
        out.push(T[(n >> 12) as usize & 63] as char);
        out.push(if c.len() > 1 {
            T[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if c.len() > 2 {
            T[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}

/// Decodes standard base64, ignoring the whitespace PEM puts in it.
pub fn unbase64(s: &str) -> Option<Vec<u8>> {
    let val = |c: u8| -> Option<u32> {
        Some(match c {
            b'A'..=b'Z' => (c - b'A') as u32,
            b'a'..=b'z' => (c - b'a' + 26) as u32,
            b'0'..=b'9' => (c - b'0' + 52) as u32,
            b'+' => 62,
            b'/' => 63,
            _ => return None,
        })
    };
    let clean: Vec<u8> = s.bytes().filter(|b| !b.is_ascii_whitespace()).collect();
    if !clean.len().is_multiple_of(4) {
        return None;
    }
    let mut out = Vec::new();
    for q in clean.chunks(4) {
        let pad = q.iter().rev().take_while(|&&c| c == b'=').count();
        if pad > 2 {
            return None;
        }
        let mut n = 0u32;
        for (i, &c) in q.iter().enumerate() {
            n <<= 6;
            if i < 4 - pad {
                n |= val(c)?;
            }
        }
        out.push((n >> 16) as u8);
        if pad < 2 {
            out.push((n >> 8) as u8);
        }
        if pad < 1 {
            out.push(n as u8);
        }
    }
    Some(out)
}

/// Go's pem.EncodeToMemory: 64 characters a line.
pub fn pem(block: &str, der: &[u8]) -> String {
    let b = base64(der);
    let mut out = format!("-----BEGIN {block}-----\n");
    for chunk in b.as_bytes().chunks(64) {
        out.push_str(std::str::from_utf8(chunk).unwrap_or_default());
        out.push('\n');
    }
    out.push_str(&format!("-----END {block}-----\n"));
    out
}

/// The PEM blocks in text, each its type and bytes, in order. A block
/// whose base64 does not decode ends the list, as Go's pem.Decode finds
/// no block there.
pub fn pem_blocks(text: &[u8]) -> Vec<(String, Vec<u8>)> {
    let text = String::from_utf8_lossy(text);
    let mut out = Vec::new();
    let mut rest: &str = &text;
    while let Some(start) = rest.find("-----BEGIN ") {
        let after = &rest[start + 11..];
        let Some(end_type) = after.find("-----") else {
            break;
        };
        let kind = after[..end_type].to_string();
        let body_start = &after[end_type + 5..];
        let end_marker = format!("-----END {kind}-----");
        let Some(end) = body_start.find(&end_marker) else {
            break;
        };
        let body = &body_start[..end];
        // Header lines ("Key: value") are not used by any file here.
        match unbase64(body) {
            Some(b) => out.push((kind, b)),
            None => {
                rest = &body_start[end + end_marker.len()..];
                continue;
            }
        }
        rest = &body_start[end + end_marker.len()..];
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn civil_round_trips() {
        for s in [0i64, 951782400, 1790954810, 4102444800, -1] {
            let (y, mo, d, h, mi, sec) = civil(s);
            assert_eq!(unix(y, mo, d, h, mi, sec), s);
        }
        assert_eq!(civil(4102444800).0, 2100);
    }

    #[test]
    fn base64_round_trips() {
        for b in [&b""[..], b"a", b"ab", b"abc", b"abcd\xff\x00"] {
            assert_eq!(unbase64(&base64(b)).unwrap(), b);
        }
    }

    #[test]
    fn an_oid_encodes_as_go_does() {
        assert_eq!(
            oid(&[1, 2, 840, 10045, 2, 1]),
            b"\x06\x07\x2a\x86\x48\xce\x3d\x02\x01"
        );
    }
}
