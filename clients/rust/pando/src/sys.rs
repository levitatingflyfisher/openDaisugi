//! The operating system calls the store needs, and the Go texts some of them
//! must print the same way: the quoting of %q and the words of an errno.

use std::io;
use std::path::Path;

pub fn getuid() -> u32 {
    // SAFETY: getuid has no failure mode and takes no pointers.
    unsafe { libc::getuid() }
}

/// Go's path.Clean for a slash path.
pub fn clean(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let rooted = p.starts_with('/');
    let mut out: Vec<&str> = Vec::new();
    for part in p.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                if out.last().is_some_and(|l| *l != "..") {
                    out.pop();
                } else if !rooted {
                    out.push("..");
                }
            }
            x => out.push(x),
        }
    }
    let body = out.join("/");
    if rooted {
        format!("/{body}")
    } else if body.is_empty() {
        ".".into()
    } else {
        body
    }
}

/// Whether Go's strconv.IsPrint holds for r. The table names the
/// non-printing ranges coppice can meet: controls, format characters,
/// separators other than the space, private use and noncharacters.
pub fn is_print_rune(r: char) -> bool {
    let c = r as u32;
    if c < 0x20 || (0x7f..=0x9f).contains(&c) {
        return false;
    }
    !matches!(c,
        0xAD | 0x600..=0x605 | 0x61C | 0x6DD | 0x70F | 0x890..=0x891 | 0x8E2 | 0x180E
        | 0x200B..=0x200F | 0x2028..=0x202E | 0x2060..=0x2064 | 0x2066..=0x206F
        | 0xFEFF | 0xFFF9..=0xFFFB | 0x110BD | 0x110CD | 0x13430..=0x1343F
        | 0x1BCA0..=0x1BCA3 | 0x1D173..=0x1D17A | 0xE0001 | 0xE0020..=0xE007F
        | 0xA0 | 0x1680 | 0x2000..=0x200A | 0x205F | 0x3000
        | 0xD800..=0xDFFF | 0xE000..=0xF8FF | 0xF0000..=0x10FFFF
        | 0xFDD0..=0xFDEF | 0xFFFE | 0xFFFF)
}

/// Go's strconv.Quote, which fmt's %q uses for a string.
pub fn go_quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for r in s.chars() {
        match r {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            r if is_print_rune(r) => out.push(r),
            r if (r as u32) < 0x20 || r as u32 == 0x7f => {
                out.push_str(&format!("\\x{:02x}", r as u32));
            }
            r if (r as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", r as u32)),
            r => out.push_str(&format!("\\U{:08x}", r as u32)),
        }
    }
    out.push('"');
    out
}

/// The words Go's syscall.Errno prints for the errno behind e.
pub fn go_errno_text(e: &io::Error) -> String {
    let n = match e.raw_os_error() {
        Some(n) => n,
        None => return e.to_string(),
    };
    let s = match n {
        libc::EPERM => "operation not permitted",
        libc::ENOENT => "no such file or directory",
        libc::EACCES => "permission denied",
        libc::EEXIST => "file exists",
        libc::ENOTDIR => "not a directory",
        libc::EISDIR => "is a directory",
        libc::EINVAL => "invalid argument",
        libc::ENOSPC => "no space left on device",
        libc::EROFS => "read-only file system",
        libc::EADDRINUSE => "address already in use",
        libc::ENAMETOOLONG => "file name too long",
        libc::ENOEXEC => "exec format error",
        libc::E2BIG => "argument list too long",
        libc::ELOOP => "too many levels of symbolic links",
        libc::EIO => "input/output error",
        _ => {
            let t = e.to_string();
            let t = t.split(" (os error").next().unwrap_or(&t).to_string();
            let mut cs = t.chars();
            return match cs.next() {
                Some(f) => f.to_lowercase().collect::<String>() + cs.as_str(),
                None => t,
            };
        }
    };
    s.to_string()
}

/// Go's *PathError text: "op path: errno words".
pub fn go_path_err(op: &str, p: &Path, e: &io::Error) -> String {
    format!("{op} {}: {}", p.display(), go_errno_text(e))
}
