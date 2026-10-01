//! The environment of the processes sprig starts, the key descriptor and
//! the redaction of the key, as the Go module's env.go does them.

use crate::agent::{Message, Model};
use std::collections::BTreeMap;
use std::io::Read;
use std::os::fd::FromRawFd;
use std::os::unix::ffi::OsStrExt;
use std::sync::Mutex;

/// The variables that can hold a provider key. No process sprig starts
/// gets them.
pub const KEY_NAMES: &[&str] = &[
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "OPENAI_API_KEY",
    "SPRIG_API_KEY",
];

static SCRUB_EXTRA: Mutex<Vec<Vec<u8>>> = Mutex::new(Vec::new());

/// Adds names (from --scrub-env) to the variables no child gets. Empty
/// names are skipped.
pub fn set_scrub_env(names: &[Vec<u8>]) {
    let mut s = SCRUB_EXTRA.lock().unwrap_or_else(|e| e.into_inner());
    *s = names.iter().filter(|n| !n.is_empty()).cloned().collect();
}

pub(crate) fn scrubbed(name: &[u8]) -> bool {
    if KEY_NAMES.iter().any(|k| k.as_bytes() == name) {
        return true;
    }
    let s = SCRUB_EXTRA.lock().unwrap_or_else(|e| e.into_inner());
    s.iter().any(|n| n.as_slice() == name)
}

/// The environment of every process sprig starts: its own less the key
/// variables and the --scrub-env names, sorted by name. With dir set,
/// PWD is that directory made absolute.
pub fn child_env(dir: Option<&[u8]>) -> BTreeMap<Vec<u8>, Vec<u8>> {
    let mut env = BTreeMap::new();
    for (k, v) in std::env::vars_os() {
        let k = k.as_bytes().to_vec();
        if scrubbed(&k) || (dir.is_some() && k == b"PWD") {
            continue;
        }
        env.insert(k, v.as_bytes().to_vec());
    }
    if let Some(d) = dir {
        let abs = if d.first() == Some(&b'/') {
            Some(crate::osx::clean(d))
        } else {
            crate::osx::getwd().map(|wd| crate::osx::join(&wd, d))
        };
        if let Some(a) = abs {
            env.insert(b"PWD".to_vec(), a);
        }
    }
    env
}

extern "C" {
    fn fcntl(fd: i32, cmd: i32, ...) -> i32;
}

// F_GETFD is 1, F_SETFD is 2 and FD_CLOEXEC is 1 on Linux.
const F_GETFD: i32 = 1;
const F_SETFD: i32 = 2;
const FD_CLOEXEC: i32 = 1;

/// Reads a key from descriptor fd to its end, drops one trailing newline
/// and closes the descriptor. The error is a line for stderr, without the
/// "sprig: " prefix.
pub fn read_key_fd(flag: &str, fd: i64) -> Result<Vec<u8>, Vec<u8>> {
    if (0..=2).contains(&fd) {
        return Err(format!(
            "{flag} {fd} is stdin, stdout or stderr; pass a descriptor of 3 or more"
        )
        .into_bytes());
    }
    let cannot = || format!("{flag} {fd} cannot be read").into_bytes();
    let Ok(raw) = i32::try_from(fd) else {
        return Err(cannot());
    };
    if unsafe { fcntl(raw, F_GETFD) } < 0 {
        return Err(cannot());
    }
    let mut f = unsafe { std::fs::File::from_raw_fd(raw) };
    let mut b = Vec::new();
    let r = f.read_to_end(&mut b);
    drop(f);
    if r.is_err() {
        return Err(cannot());
    }
    if b.last() == Some(&b'\n') {
        b.pop();
    }
    Ok(b)
}

/// Whether a key holds a control byte (below 0x20, or DEL): such a key
/// cannot go in a header, so sprig refuses it at once.
pub fn key_has_control(key: &[u8]) -> bool {
    key.iter().any(|&c| c < 0x20 || c == 0x7f)
}

/// Marks every descriptor above 2 close-on-exec, so no process sprig
/// starts inherits one.
pub fn close_inherited_fds() {
    let Ok(rd) = std::fs::read_dir("/proc/self/fd") else {
        return;
    };
    let fds: Vec<i32> = rd
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().to_str().and_then(|s| s.parse().ok()))
        .collect();
    for fd in fds {
        if fd > 2 {
            unsafe {
                fcntl(fd, F_SETFD, FD_CLOEXEC);
            }
        }
    }
}

/// The shapes a provider can echo a key in: as it is, with its slashes
/// escaped, and JSON-escaped with or without the escaped slashes.
fn key_forms(key: &[u8]) -> Vec<Vec<u8>> {
    let slash = |k: &[u8]| -> Vec<u8> {
        let mut o = Vec::new();
        for &c in k {
            if c == b'/' {
                o.extend_from_slice(b"\\/");
            } else {
                o.push(c);
            }
        }
        o
    };
    let mut js = Vec::new();
    for &c in key {
        match c {
            b'"' => js.extend_from_slice(b"\\\""),
            b'\\' => js.extend_from_slice(b"\\\\"),
            b'\n' => js.extend_from_slice(b"\\n"),
            b'\r' => js.extend_from_slice(b"\\r"),
            b'\t' => js.extend_from_slice(b"\\t"),
            b'<' | b'>' | b'&' => js.extend_from_slice(format!("\\u00{:02x}", c).as_bytes()),
            c if c < 0x20 => js.extend_from_slice(format!("\\u00{:02x}", c).as_bytes()),
            c => js.push(c),
        }
    }
    let mut out: Vec<Vec<u8>> = Vec::new();
    for f in [key.to_vec(), slash(key), js.clone(), slash(&js)] {
        if !f.is_empty() && !out.contains(&f) {
            out.push(f);
        }
    }
    out
}

fn replace_all(text: &[u8], key: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(text.len());
    let mut i = 0;
    while i < text.len() {
        if text[i..].starts_with(key) {
            out.extend_from_slice(b"[key]");
            i += key.len();
        } else {
            out.push(text[i]);
            i += 1;
        }
    }
    out
}

/// Replaces the key in text with [key], in the shapes key_forms lists. A
/// key split over lines or spaces cannot be cut out of the text, so then the
/// whole text becomes [key]. An empty key changes nothing.
pub fn redact(text: &[u8], key: &[u8]) -> Vec<u8> {
    if key.is_empty() {
        return text.to_vec();
    }
    let forms = key_forms(key);
    let mut out = text.to_vec();
    for f in &forms {
        out = replace_all(&out, f);
    }
    let squeezed: Vec<u8> = out
        .iter()
        .copied()
        .filter(|c| !c.is_ascii_whitespace())
        .collect();
    for f in &forms {
        if squeezed.windows(f.len()).any(|w| w == f.as_slice()) {
            return b"[key]".to_vec();
        }
    }
    out
}

/// A model whose errors never hold the key: a provider can echo the
/// request in its error body.
pub struct Redacting {
    pub inner: Box<dyn Model>,
    pub key: Vec<u8>,
}

impl Model for Redacting {
    fn next(&mut self, history: &[Message]) -> Result<Message, Vec<u8>> {
        self.inner.next(history).map_err(|e| redact(&e, &self.key))
    }

    fn model_name(&self) -> String {
        self.inner.model_name()
    }

    fn held_key(&self) -> Vec<u8> {
        self.key.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn redact_follows_go() {
        assert_eq!(redact(b"x sk-1 y sk-1", b"sk-1"), b"x [key] y [key]");
        assert_eq!(redact(b"abc", b""), b"abc");
        assert_eq!(redact(b"aaa", b"aa"), b"[key]a");
    }

    #[test]
    fn redact_hides_escaped_and_split_shapes() {
        let key = b"sk-ab/cd+ef\"gh";
        for text in [
            &b"echo sk-ab\\/cd+ef\\\"gh end"[..],
            &b"echo sk-ab/cd+ef\\\"gh end"[..],
            &b"echo sk-ab/cd+ef\"gh end"[..],
            &b"echo sk-ab/cd+\nef\"gh end"[..],
            &b"echo sk-ab/cd \n  +ef\"gh end"[..],
        ] {
            let got = redact(text, key);
            assert!(!String::from_utf8_lossy(&got).contains("sk-ab"), "{:?} -> {:?}", String::from_utf8_lossy(text), String::from_utf8_lossy(&got));
        }
        assert_eq!(redact(b"ok", b"sk-x"), b"ok");
    }

    #[test]
    fn key_fd_reads_and_closes() {
        let (mut r, mut w) = std::io::pipe().unwrap();
        use std::io::Write;
        use std::os::fd::IntoRawFd;
        w.write_all(b"sk-test\n\n").unwrap();
        drop(w);
        let _ = &mut r;
        let fd = r.into_raw_fd();
        assert_eq!(read_key_fd("--key-fd", fd as i64).unwrap(), b"sk-test\n");
        assert!(unsafe { fcntl(fd, F_GETFD) } < 0);
        assert_eq!(
            read_key_fd("--key-fd", 1).unwrap_err(),
            b"--key-fd 1 is stdin, stdout or stderr; pass a descriptor of 3 or more"
        );
        assert_eq!(
            read_key_fd("--key-fd", 999).unwrap_err(),
            b"--key-fd 999 cannot be read"
        );
    }

    #[test]
    fn child_env_scrubs_and_sets_pwd() {
        // The --scrub-env list is global and the cli tests reset it, so
        // this test reads only the fixed key names.
        std::env::set_var("OPENAI_API_KEY", "k");
        let env = child_env(Some(b"/tmp/./x"));
        assert!(!env.contains_key(b"OPENAI_API_KEY".as_slice()));
        assert_eq!(env.get(b"PWD".as_slice()).unwrap(), b"/tmp/x");
        std::env::remove_var("OPENAI_API_KEY");
    }
}
