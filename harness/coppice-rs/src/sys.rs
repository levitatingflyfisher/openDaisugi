//! The operating system calls coppice needs, and the Go texts some of them
//! must print the same way: the quoting of %q and the words of an errno.

use std::io;
use std::os::unix::io::AsRawFd;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};

pub fn getuid() -> u32 {
    // SAFETY: getuid has no failure mode and takes no pointers.
    unsafe { libc::getuid() }
}

pub fn getpid() -> i32 {
    std::process::id() as i32
}

/// The pid, uid and gid of a socket peer, from SO_PEERCRED. None when the
/// kernel will not say, and the connection is then refused.
pub fn peer_cred(s: &UnixStream) -> Option<(i32, u32, u32)> {
    let mut cred = libc::ucred {
        pid: 0,
        uid: 0,
        gid: 0,
    };
    let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    // SAFETY: cred and len point at live, correctly sized values.
    let rc = unsafe {
        libc::getsockopt(
            s.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut libc::ucred as *mut libc::c_void,
            &mut len,
        )
    };
    if rc != 0 {
        return None;
    }
    Some((cred.pid, cred.uid, cred.gid))
}

/// A pid's parent and session, from /proc/<pid>/stat. The command name
/// sits in parentheses and may hold spaces, so the fields are read after
/// the last closing parenthesis.
pub fn proc_stat(pid: i32) -> Result<(i32, i32), String> {
    let b = std::fs::read(format!("/proc/{pid}/stat")).map_err(|e| e.to_string())?;
    let s = String::from_utf8_lossy(&b);
    let i = s
        .rfind(')')
        .ok_or_else(|| format!("cannot read /proc/{pid}/stat"))?;
    let f: Vec<&str> = s[i + 1..].split_whitespace().collect();
    if f.len() < 4 {
        return Err(format!("cannot read /proc/{pid}/stat"));
    }
    match (f[1].parse::<i32>(), f[3].parse::<i32>()) {
        (Ok(ppid), Ok(sid)) => Ok((ppid, sid)),
        _ => Err(format!("cannot read /proc/{pid}/stat")),
    }
}

/// Whether this process leads its own session.
pub fn leads_session() -> bool {
    matches!(proc_stat(getpid()), Ok((_, sid)) if sid == getpid())
}

/// The path made absolute against the working directory, cleaned the way
/// Go's filepath.Abs cleans it.
pub fn abs_path(p: &Path) -> io::Result<PathBuf> {
    let joined = if p.is_absolute() {
        p.to_path_buf()
    } else {
        std::env::current_dir()?.join(p)
    };
    Ok(PathBuf::from(clean(&joined.to_string_lossy())))
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

/// The home directory, as Go's os.UserHomeDir reads it: $HOME.
pub fn home_dir() -> Option<PathBuf> {
    match std::env::var_os("HOME") {
        Some(h) if !h.is_empty() => Some(PathBuf::from(h)),
        _ => None,
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quote_matches_go() {
        assert_eq!(go_quote("w9:p9"), "\"w9:p9\"");
        assert_eq!(
            go_quote("a\"b\\\n\u{1}\u{7f}é\u{200b}"),
            "\"a\\\"b\\\\\\n\\x01\\x7fé\\u200b\""
        );
    }

    #[test]
    fn clean_matches_go() {
        assert_eq!(clean("/a/b/../c/./"), "/a/c");
        assert_eq!(clean("/.."), "/");
        assert_eq!(clean("a/../.."), "..");
    }
}
