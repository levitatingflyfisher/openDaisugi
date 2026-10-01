//! `voice/deliver.py`: arm grants, and the one arm decision before a send.
//! Also `server._extract_multipart_audio`.

use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, PermissionsExt};

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

use super::pynum::py_float;

/// `urllib.parse.quote(s, safe="")`.
pub fn quote(s: &str) -> String {
    let mut b = String::with_capacity(s.len());
    for &c in s.as_bytes() {
        if c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.' | b'-' | b'~') {
            b.push(c as char);
        } else {
            b.push_str(&format!("%{c:02X}"));
        }
    }
    b
}

/// The grant file name of a pane key.
pub fn armed_name(pane_key: &str) -> String {
    format!("{}.json", quote(pane_key))
}

pub struct ArmEntry {
    pub pane_key: String,
    pub armed_at: f64,
    pub expires_at: f64,
}

/// `Path.mkdir(mode=0o700, parents=True, exist_ok=True)`.
fn mkdir_700(dir: &str) -> std::io::Result<()> {
    match std::fs::metadata(dir) {
        Ok(m) if m.is_dir() => return Ok(()),
        Ok(_) => return Err(std::io::Error::from_raw_os_error(libc::EEXIST)),
        Err(_) => {}
    }
    if let Some(parent) = std::path::Path::new(dir).parent() {
        if !parent.as_os_str().is_empty() {
            std::fs::create_dir_all(parent)?;
        }
    }
    match std::fs::DirBuilder::new().mode(0o700).create(dir) {
        Err(_) if std::fs::metadata(dir).map(|m| m.is_dir()).unwrap_or(false) => Ok(()),
        r => r,
    }
}

/// `deliver.arm`: the grant written at 0600, in a directory made at 0700.
/// `now_raw`, when set, is written as armed_at, so an int clock stays an
/// int as Python keeps it.
pub fn arm(pane_key: &str, minutes: f64, armed_dir: &str, now: f64, now_raw: Option<Value>) -> std::io::Result<ArmEntry> {
    mkdir_700(armed_dir)?;
    let e = ArmEntry { pane_key: pane_key.to_string(), armed_at: now, expires_at: now + minutes * 60.0 };
    let mut o = Object::new();
    o.set("pane_key", pane_key);
    o.set("armed_at", now_raw.unwrap_or(Value::Float(now)));
    o.set("expires_at", e.expires_at);
    let p = format!("{armed_dir}/{}", armed_name(pane_key));
    let mut f = std::fs::OpenOptions::new().write(true).create(true).truncate(true).open(&p)?;
    f.write_all(pyjson::dumps(&Value::Obj(o), true).as_bytes())?;
    drop(f);
    std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o600))?;
    Ok(e)
}

/// `Path.exists()` on an error: False for these, a raise otherwise.
fn absent(e: &std::io::Error) -> bool {
    matches!(e.raw_os_error(), Some(libc::ENOENT) | Some(libc::ENOTDIR) | Some(libc::EBADF) | Some(libc::ELOOP))
}

/// `deliver.disarm`: true when a grant was there and is gone. A directory
/// in its place is IsADirectoryError, never removed.
pub fn disarm(pane_key: &str, armed_dir: &str) -> std::io::Result<bool> {
    let p = format!("{armed_dir}/{}", armed_name(pane_key));
    if let Err(e) = std::fs::metadata(&p) {
        if absent(&e) {
            return Ok(false);
        }
        return Err(e);
    }
    let c = std::ffi::CString::new(p).map_err(|_| std::io::Error::from_raw_os_error(libc::EINVAL))?;
    if unsafe { libc::unlink(c.as_ptr()) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(true)
}

/// `float(v)` for a value json.loads made.
fn py_float_value(v: &Value) -> Option<f64> {
    match v {
        Value::Bool(b) => Some(if *b { 1.0 } else { 0.0 }),
        Value::Int(t) => t.parse::<f64>().ok().filter(|f| f.is_finite()),
        Value::Float(f) => Some(*f),
        Value::Str(s) => py_float(s),
        _ => None,
    }
}

/// `deliver.is_armed`: a readable grant whose expires_at, as float()
/// reads it, is after now. Every failure reads as unarmed.
pub fn is_armed(pane_key: &str, armed_dir: &str, now: f64) -> bool {
    let p = format!("{armed_dir}/{}", armed_name(pane_key));
    let Ok(raw) = std::fs::read(&p) else { return false };
    let Ok(t) = String::from_utf8(raw) else { return false };
    let Ok(v) = pyjson::loads_py(&t, 900) else { return false };
    let Value::Obj(o) = v else { return false };
    let Some(exp) = o.get("expires_at").and_then(py_float_value) else { return false };
    exp > now
}

pub struct DeliverResult {
    pub delivered: &'static str,
    pub reason: Option<String>,
}

/// `deliver.deliver`: the one arm decision, then `send` only when it says
/// send. Preview, an unknown mode and a refused send never call it.
pub fn deliver<E>(
    pane_id: &str,
    text_: &str,
    mode: &str,
    armed_dir: &str,
    pane_key: Option<&str>,
    now: f64,
    send: impl FnOnce(&str) -> Result<(), E>,
) -> Result<DeliverResult, E> {
    if mode != "preview" && mode != "send" {
        return Ok(DeliverResult {
            delivered: "refused",
            reason: Some(format!("unknown mode {}. Use preview or send.", text::repr(mode))),
        });
    }
    let key = pane_key.unwrap_or(pane_id);
    if mode == "preview" {
        return Ok(DeliverResult { delivered: "preview", reason: None });
    }
    if !is_armed(key, armed_dir, now) {
        return Ok(DeliverResult {
            delivered: "refused",
            reason: Some(format!("pane not armed for direct send. Run: daisugi voice arm {key} --for 30m")),
        });
    }
    send(text_)?;
    Ok(DeliverResult { delivered: "sent", reason: None })
}

/// Bytes decoded as latin-1.
pub fn latin1(b: &[u8]) -> String {
    b.iter().map(|&c| c as char).collect()
}

fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

fn split_bytes<'a>(hay: &'a [u8], sep: &[u8]) -> Vec<&'a [u8]> {
    let mut out = vec![];
    let mut rest = hay;
    while let Some(i) = find(rest, sep) {
        out.push(&rest[..i]);
        rest = &rest[i + sep.len()..];
    }
    out.push(rest);
    out
}

/// `server._extract_multipart_audio`: the first part with a
/// Content-Disposition and a blank line, and its Content-Type (the last
/// one given), or application/octet-stream. `content_type` is the header
/// latin-1 decoded.
pub fn extract_multipart_audio(body: &[u8], content_type: &str) -> Result<(Vec<u8>, String), String> {
    let Some(idx) = content_type.find("boundary=") else {
        return Err("multipart request is missing a boundary".into());
    };
    let boundary = text::strip(&content_type[idx + 9..]).trim_matches('"');
    let mut sep = b"--".to_vec();
    sep.extend_from_slice(boundary.as_bytes());
    for part in split_bytes(body, &sep) {
        if find(part, b"Content-Disposition").is_none() {
            continue;
        }
        let Some(end) = find(part, b"\r\n\r\n") else { continue };
        let headers = latin1(&part[..end]);
        let mut payload = &part[end + 4..];
        if payload.ends_with(b"\r\n") {
            payload = &payload[..payload.len() - 2];
        }
        let mut part_type = "application/octet-stream".to_string();
        for line in text::splitlines(&headers) {
            if line.len() >= 13 && line.is_char_boundary(13) && line[..13].eq_ignore_ascii_case("content-type:") {
                part_type = text::strip(&line[13..]).to_string();
            }
        }
        return Ok((payload.to_vec(), part_type));
    }
    Err("multipart request has no file part".into())
}
