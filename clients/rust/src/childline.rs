//! The line protocol both resident children share: the voice engine
//! (`voice::resident`) and a pack's worker (`pack::client`). A request is
//! a frame; each answer is one JSON object per line; a parent that sees a
//! child end names how it ended. It is below both modules, so the pack
//! client does not use the voice module. The one definition is
//! src/opendaisugi/childline.py.

use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

/// One request: the body's length, big-endian, then the body.
pub fn frame(body: &[u8]) -> Vec<u8> {
    let mut out = (body.len() as u32).to_be_bytes().to_vec();
    out.extend_from_slice(body);
    out
}

/// `childline.parse_line`: the line as a JSON object, or None.
pub fn parse_line(raw: &[u8]) -> Option<Object> {
    let t = std::str::from_utf8(raw).ok()?;
    match pyjson::loads_py(t, 900) {
        Ok(Value::Obj(o)) => Some(o),
        _ => None,
    }
}

/// `childline.exit_reason` for a child that ended with `rc`.
pub fn exit_reason(rc: i32, tail: &[String]) -> String {
    let what = if rc < 0 { format!("killed by signal {}", -rc) } else { format!("exited {rc}") };
    match tail.last().map(|l| text::head(text::strip(l), 200).to_string()) {
        Some(last) if !last.is_empty() => format!("{what}: {last}"),
        _ => what,
    }
}
