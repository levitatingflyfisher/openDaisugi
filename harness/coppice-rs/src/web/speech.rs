//! A speech.say reply carries one sentence of audio as base64 inside a
//! JSON line. The socket sends it to the page as a binary frame instead:
//!
//! - 4 bytes: the big-endian length N of the header
//! - N bytes: `{"event":"speech","id":<the request id>,"type":"audio/mpeg"}`
//! - the rest: the audio
//!
//! The frame goes only to the socket whose request it answers, since the
//! reply comes back on that socket's own upstream session. A speech.say
//! that fails stays a text line with the server's error. Go's
//! internal/web/speech.go says the same.

use std::collections::HashSet;

use serde_json::Value;

/// The most speech.say ids one socket keeps: a request that fails leaves
/// its id until the cap clears the set.
const IDS_MAX: usize = 256;

/// Reads one line the browser sent, and keeps its id when it is a
/// speech.say request. Only an answer to one of these becomes a frame.
pub fn note(line: &[u8], ids: &mut HashSet<String>) {
    if !line.windows(10).any(|w| w == b"speech.say") {
        return;
    }
    let Ok(v) = serde_json::from_slice::<Value>(line) else {
        return;
    };
    let (Some(id), Some(cmd)) = (
        v.get("id").and_then(|i| i.as_str()),
        v.get("cmd").and_then(|c| c.as_str()),
    ) else {
        return;
    };
    if cmd != "speech.say" || id.is_empty() {
        return;
    }
    if ids.len() >= IDS_MAX {
        ids.clear();
    }
    ids.insert(id.to_string());
}

fn unbase64(s: &str) -> Option<Vec<u8>> {
    let mut out = Vec::with_capacity(s.len() / 4 * 3);
    let b = s.as_bytes();
    if b.len() % 4 != 0 {
        return None;
    }
    let val = |c: u8| -> Option<u32> {
        match c {
            b'A'..=b'Z' => Some((c - b'A') as u32),
            b'a'..=b'z' => Some((c - b'a') as u32 + 26),
            b'0'..=b'9' => Some((c - b'0') as u32 + 52),
            b'+' => Some(62),
            b'/' => Some(63),
            _ => None,
        }
    };
    let chunks = b.len() / 4;
    for (i, c) in b.chunks(4).enumerate() {
        let last = i + 1 == chunks;
        let pad = c.iter().rev().take_while(|&&x| x == b'=').count();
        if pad > 2 || (pad > 0 && !last) {
            return None;
        }
        let mut n = 0u32;
        for (j, &x) in c.iter().enumerate() {
            n <<= 6;
            if j < 4 - pad {
                n |= val(x)?;
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

/// The binary frame for a speech.say reply line, or None for any other
/// line, which goes to the page as it is.
pub fn speech_frame(line: &[u8], ids: &mut HashSet<String>) -> Option<Vec<u8>> {
    if !line.windows(9).any(|w| w == b"\"audio\":\"") {
        return None;
    }
    let v: Value = serde_json::from_slice(line).ok()?;
    if v.get("ok")? != &Value::Bool(true) {
        return None;
    }
    let id = v.get("id")?;
    if !id.is_string() {
        return None;
    }
    let res = v.get("result")?;
    let audio = unbase64(res.get("audio")?.as_str()?)?;
    let bytes = res.get("bytes")?.as_u64()?;
    if audio.is_empty() || audio.len() as u64 != bytes {
        return None;
    }
    if !ids.remove(id.as_str()?) {
        return None;
    }
    let typ = res.get("type").and_then(|t| t.as_str()).unwrap_or("");
    let head = format!(
        "{{\"event\":\"speech\",\"id\":{},\"type\":{}}}",
        serde_json::to_string(id).ok()?,
        serde_json::to_string(typ).ok()?
    );
    let mut out = (head.len() as u32).to_be_bytes().to_vec();
    out.extend_from_slice(head.as_bytes());
    out.extend_from_slice(&audio);
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_reply_becomes_a_frame_and_others_do_not() {
        let line = br#"{"id":"w1","ok":true,"result":{"audio":"SUQzIGZha2UgYXVkaW8=","bytes":14,"type":"audio/mpeg"}}"#;
        let mut ids = HashSet::new();
        // An answer to no speech.say, or to another verb, stays text.
        assert!(speech_frame(line, &mut ids).is_none());
        note(br#"{"id":"w1","cmd":"pane.list"}"#, &mut ids);
        assert!(speech_frame(line, &mut ids).is_none());
        note(br#"{"id":"w1","cmd":"speech.say","text":"x"}"#, &mut ids);
        let f = speech_frame(line, &mut ids).unwrap();
        // One request gives one frame.
        assert!(speech_frame(line, &mut ids).is_none());
        let n = u32::from_be_bytes([f[0], f[1], f[2], f[3]]) as usize;
        assert_eq!(
            std::str::from_utf8(&f[4..4 + n]).unwrap(),
            r#"{"event":"speech","id":"w1","type":"audio/mpeg"}"#
        );
        assert_eq!(&f[4 + n..], b"ID3 fake audio");
        for bad in [
            r#"{"id":"1","ok":true,"result":{"audio":"!!!","bytes":3}}"#,
            r#"{"id":"1","ok":true,"result":{"audio":"AAAA","bytes":9,"type":"audio/mpeg"}}"#,
            r#"{"id":7,"ok":true,"result":{"audio":"AAAA","bytes":3,"type":"audio/mpeg"}}"#,
            r#"{"id":"1","ok":false,"result":{"audio":"AAAA","bytes":3,"type":"audio/mpeg"}}"#,
            r#"not json "audio":""#,
        ] {
            let mut ids = HashSet::new();
            note(br#"{"id":"1","cmd":"speech.say","text":"x"}"#, &mut ids);
            assert!(speech_frame(bad.as_bytes(), &mut ids).is_none(), "{bad}");
        }
    }
}
