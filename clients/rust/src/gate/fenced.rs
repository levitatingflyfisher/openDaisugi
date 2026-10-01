//! `gate check` inside `daisugi fence`: a thin client of the resident gate
//! (FN-R-12), as the Go client's `internal/gate/fenced.go`.
//!
//! The fence keeps the gate's state read only, so the hook cannot write
//! asks, audit or sessions itself. It sends the call to `<root>/gate.sock`,
//! where `daisugi gate serve`, outside the fence, decides and writes. Any
//! failure to reach it, or a reply it cannot read, is the format's deny.

use std::ffi::OsString;
use std::io::{Read, Write};
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::os::unix::net::UnixStream;
use std::time::{Duration, Instant};

use super::resident::MAX_REQUEST_BYTES;
use super::{child_deadline_s, deny_text, py_argv, GateResult};

/// The variable `daisugi fence` sets in the harness's environment.
pub const FENCED_ENV: &str = "DAISUGI_FENCED";

/// Whether this process runs under `daisugi fence`.
pub fn fenced() -> bool {
    std::env::var_os(FENCED_ENV).is_some_and(|v| !v.is_empty())
}

/// The gate root the argv names (--root), else the data dir's gate.
pub fn fenced_root(argv: &[String]) -> String {
    for (i, a) in argv.iter().enumerate() {
        if a == "--root" {
            if let Some(v) = argv.get(i + 1) {
                return v.clone();
            }
        }
        if let Some(v) = a.strip_prefix("--root=") {
            return v.to_string();
        }
    }
    let home = std::env::var("HOME").unwrap_or_default();
    let d = crate::datahome::dir(|k| std::env::var(k).ok(), &home, |p| std::path::Path::new(p).exists());
    format!("{d}/gate")
}

/// gate_client._socket_is_trustworthy: a socket, not a link, owned by this
/// uid, mode 0600.
fn socket_ok(sock: &str) -> bool {
    match std::fs::symlink_metadata(sock) {
        Ok(m) => m.file_type().is_socket() && m.uid() == unsafe { libc::getuid() } && m.mode() & 0o7777 == 0o600,
        Err(_) => false,
    }
}

/// The fenced hook's answer for `args` and `stdin`.
pub fn fenced_check(args: &[OsString], stdin: &[u8]) -> GateResult {
    let argv = py_argv(args);
    let sock = format!("{}/gate.sock", fenced_root(&argv));
    let deny = |why: &str| {
        deny_text(
            &argv,
            &format!(
                "the fenced gate hook cannot reach the resident gate ({why}), so it denies. Start it with: daisugi gate serve"
            ),
        )
    };
    if !socket_ok(&sock) {
        return deny(&format!("no private socket at {sock}"));
    }
    let mut req = serde_json::Map::new();
    req.insert("v".into(), 1.into());
    // --root only found the socket: the server has its own root and takes
    // no path from a request (FN-R-21).
    req.insert(
        "argv".into(),
        super::fenceruns::strip_path_flags(&argv).into_iter().map(serde_json::Value::String).collect(),
    );
    req.insert("stdin_b64".into(), b64(stdin).into());
    let env = |k: &str| std::env::var(k).ok().filter(|v| !v.is_empty());
    if let (Some(s), Some(p)) = (env("COPPICE_SOCK"), env("COPPICE_PANE")) {
        req.insert("coppice_sock".into(), s.into());
        req.insert("coppice_pane".into(), p.into());
    }
    if let Some(h) = env("HERDR_PANE_ID").or_else(|| env("HERDR_PANE")) {
        req.insert("herdr_pane".into(), h.into());
    }
    if let Some(d) = env("COPPICE_DATA_DIR").filter(|d| d.starts_with('/')) {
        req.insert("coppice_data_dir".into(), d.into());
    }
    let mut line = serde_json::to_vec(&serde_json::Value::Object(req)).unwrap_or_default();
    line.push(b'\n');
    let budget = Duration::from_secs_f64(child_deadline_s(args) + 10.0);
    let end = Instant::now() + budget;
    let mut c = match UnixStream::connect(&sock) {
        Ok(c) => c,
        Err(e) => return deny(&e.to_string()),
    };
    let _ = c.set_write_timeout(Some(budget));
    if let Err(e) = c.write_all(&line) {
        return deny(&e.to_string());
    }
    let mut buf = Vec::new();
    let mut chunk = [0u8; 65536];
    while buf.len() < MAX_REQUEST_BYTES {
        let left = end.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return deny("no reply in time");
        }
        let _ = c.set_read_timeout(Some(left));
        match c.read(&mut chunk) {
            Ok(0) => return deny("the server ended the connection with no reply"),
            Ok(n) => {
                buf.extend_from_slice(&chunk[..n]);
                if buf.last() == Some(&b'\n') {
                    break;
                }
            }
            Err(e) => return deny(&e.to_string()),
        }
    }
    let reply: serde_json::Value = match serde_json::from_slice(&buf) {
        Ok(v) => v,
        Err(_) => return deny("a reply it cannot read"),
    };
    let (Some(1), Some(out), Some(err), Some(code)) = (
        reply.get("v").and_then(|v| v.as_i64()),
        reply.get("stdout").and_then(|v| v.as_str()),
        reply.get("stderr").and_then(|v| v.as_str()),
        reply.get("exit_code").and_then(|v| v.as_i64()),
    ) else {
        return deny("a reply it cannot read");
    };
    // gate_client.main prints each non-empty stream with a newline.
    let line = |s: &str| if s.is_empty() { vec![] } else { format!("{s}\n").into_bytes() };
    GateResult {
        stdout: line(out),
        stderr: line(err),
        exit: code as i32,
        native: true,
        out_stdout: out.to_string(),
        out_stderr: err.to_string(),
        ..Default::default()
    }
}

fn b64(data: &[u8]) -> String {
    const A: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for c in data.chunks(3) {
        let n = (c[0] as u32) << 16 | (*c.get(1).unwrap_or(&0) as u32) << 8 | *c.get(2).unwrap_or(&0) as u32;
        for i in 0..4 {
            if i <= c.len() {
                out.push(A[(n >> (18 - 6 * i) & 63) as usize] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn b64_matches_the_standard_alphabet() {
        assert_eq!(b64(b""), "");
        assert_eq!(b64(b"f"), "Zg==");
        assert_eq!(b64(b"fo"), "Zm8=");
        assert_eq!(b64(b"foo"), "Zm9v");
        assert_eq!(b64(b"{\"a\":1}"), "eyJhIjoxfQ==");
    }

    #[test]
    fn the_root_comes_from_the_argv() {
        let a = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert_eq!(fenced_root(&a(&["--mode", "enforce", "--root", "/r"])), "/r");
        assert_eq!(fenced_root(&a(&["--root=/s"])), "/s");
    }
}
