//! The file helpers of the chat: modes, atomic writes, appends that are cut
//! back when they fail, and JSON lines in the bytes Go writes.

use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};

use crate::sys;

/// The chat is the owner's whole working life: only the owner may read it.
pub const DIR_MODE: u32 = 0o700;
pub const FILE_MODE: u32 = 0o600;

/// Skips every fsync. Only the tests set it, for the ones that append
/// hundreds of lines and do not test durability.
pub static NO_SYNC: AtomicBool = AtomicBool::new(false);

fn no_sync() -> bool {
    NO_SYNC.load(Ordering::Relaxed)
}

/// Go's text for an error on a path: "op path: reason".
pub fn path_err(op: &str, p: &Path, e: &io::Error) -> String {
    sys::go_path_err(op, p, e)
}

/// Makes dir and every missing parent with mode 0700, and tightens dir
/// itself to 0700 if it was already there. A parent that was already there
/// is left as it is.
pub fn ensure_dir(dir: &Path) -> Result<(), String> {
    let mut missing = Vec::new();
    let mut d = PathBuf::from(sys::clean(&dir.to_string_lossy()));
    loop {
        if fs::symlink_metadata(&d).is_ok() {
            break;
        }
        let parent = match d.parent() {
            Some(p) if !p.as_os_str().is_empty() => p.to_path_buf(),
            Some(_) => PathBuf::from("."),
            None => d.clone(),
        };
        if parent == d {
            break;
        }
        missing.push(d.clone());
        d = parent;
    }
    {
        use std::os::unix::fs::DirBuilderExt;
        fs::DirBuilder::new()
            .recursive(true)
            .mode(DIR_MODE)
            .create(dir)
            .map_err(|e| path_err("mkdir", dir, &e))?;
    }
    for d in &missing {
        fs::set_permissions(d, fs::Permissions::from_mode(DIR_MODE))
            .map_err(|e| path_err("chmod", d, &e))?;
    }
    fs::set_permissions(dir, fs::Permissions::from_mode(DIR_MODE))
        .map_err(|e| path_err("chmod", dir, &e))
}

/// os.MkdirAll(dir, 0700): makes dir and its missing parents, and leaves
/// the mode of one that was there.
pub fn ensure_dir_plain(dir: &Path) -> Result<(), String> {
    use std::os::unix::fs::DirBuilderExt;
    fs::DirBuilder::new()
        .recursive(true)
        .mode(DIR_MODE)
        .create(dir)
        .map_err(|e| path_err("mkdir", dir, &e))
}

/// Sets every regular file in the chat dir, main/ and tree/ to 0600 when it
/// is open to the group or others. A narrower file is left as it is.
pub fn tighten_files(dir: &Path) -> Result<(), String> {
    for d in [dir.to_path_buf(), dir.join("main"), dir.join("tree")] {
        let rd = fs::read_dir(&d).map_err(|e| path_err("open", &d, &e))?;
        let mut ents: Vec<_> = rd.flatten().collect();
        ents.sort_by_key(|e| e.file_name());
        for e in ents {
            let Ok(ft) = e.file_type() else { continue };
            if !ft.is_file() {
                continue;
            }
            let p = e.path();
            let md = fs::symlink_metadata(&p).map_err(|e| path_err("lstat", &p, &e))?;
            if md.permissions().mode() & 0o077 != 0 {
                fs::set_permissions(&p, fs::Permissions::from_mode(FILE_MODE))
                    .map_err(|e| path_err("chmod", &p, &e))?;
            }
        }
    }
    Ok(())
}

/// Makes a new or renamed entry in dir durable.
pub fn sync_dir(dir: &Path) -> Result<(), String> {
    if no_sync() {
        return Ok(());
    }
    let f = fs::File::open(dir).map_err(|e| path_err("open", dir, &e))?;
    f.sync_all().map_err(|e| path_err("sync", dir, &e))
}

/// A random suffix for a temp file, as os.CreateTemp makes one.
fn temp_suffix() -> String {
    let mut b = [0u8; 8];
    if let Ok(mut f) = fs::File::open("/dev/urandom") {
        let _ = io::Read::read_exact(&mut f, &mut b);
    }
    let n = u64::from_le_bytes(b) % 1_000_000_000;
    n.to_string()
}

/// Replaces path with data: a temp file in the same directory, fsync,
/// rename, then fsync the directory. A crash leaves the old file or the new
/// one, never a mix.
pub fn write_atomic(path: &Path, data: &[u8]) -> Result<(), String> {
    let dir = path.parent().unwrap_or(Path::new("."));
    let base = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let mut tmp;
    let mut f;
    loop {
        tmp = dir.join(format!(".{base}.tmp-{}", temp_suffix()));
        match OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(FILE_MODE)
            .open(&tmp)
        {
            Ok(x) => {
                f = x;
                break;
            }
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(path_err("open", &tmp, &e)),
        }
    }
    let res = (|| {
        f.set_permissions(fs::Permissions::from_mode(FILE_MODE))
            .map_err(|e| path_err("chmod", &tmp, &e))?;
        f.write_all(data).map_err(|e| path_err("write", &tmp, &e))?;
        if !no_sync() {
            f.sync_all().map_err(|e| path_err("sync", &tmp, &e))?;
        }
        fs::rename(&tmp, path).map_err(|e| {
            format!(
                "rename {} {}: {}",
                tmp.display(),
                path.display(),
                sys::go_errno_text(&e)
            )
        })
    })();
    if res.is_err() {
        let _ = fs::remove_file(&tmp);
        return res;
    }
    sync_dir(dir)
}

/// A failed append could not be cut back off its file. The file may now end
/// in a bad line, so the chat must take no more appends.
pub const CUT_BACK: &str = "a failed append could not be removed from the log";

/// Why an append failed: the error text, and whether the file could not be
/// cut back.
#[derive(Debug)]
pub struct AppendErr {
    pub msg: String,
    pub cut_back: bool,
}

/// How a test makes an append to the message log fail.
#[cfg(test)]
#[derive(Clone, Copy, Debug)]
pub enum FailMode {
    /// The write stops half way.
    Write,
    /// The write is whole and the fsync fails.
    Sync,
}

#[cfg(test)]
thread_local! {
    /// A test sets this to make an append to the message log (a path under
    /// main/) fail: after letting the given number of such appends pass.
    pub static FAIL_APPEND: std::cell::Cell<Option<(u32, FailMode)>> = const { std::cell::Cell::new(None) };
}

#[cfg(test)]
thread_local! {
    /// A test sets this to make every append to the tree (a path under
    /// tree/) fail with an I/O error.
    pub static FAIL_TREE_APPEND: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

/// Appends one line to path, creates it 0600 if needed, and fsyncs it
/// before it returns. It returns the offset the line starts at. When the
/// write or the fsync fails, the file is cut back to its length before the
/// append, so the next append starts on a clean end.
pub fn append_line(path: &Path, line: &[u8]) -> Result<u64, AppendErr> {
    let plain = |msg: String| AppendErr {
        msg,
        cut_back: false,
    };
    let existed = fs::metadata(path).is_ok();
    let mut f = OpenOptions::new()
        .append(true)
        .create(true)
        .mode(FILE_MODE)
        .open(path)
        .map_err(|e| plain(path_err("open", path, &e)))?;
    let off = f
        .metadata()
        .map_err(|e| plain(path_err("stat", path, &e)))?
        .len();
    let undo = |f: &fs::File, msg: String| -> AppendErr {
        match f.set_len(off) {
            Err(te) => AppendErr {
                msg: format!("{msg} ({CUT_BACK}: {})", path_err("truncate", path, &te)),
                cut_back: true,
            },
            Ok(()) => {
                let _ = f.sync_all();
                AppendErr {
                    msg,
                    cut_back: false,
                }
            }
        }
    };
    #[cfg(test)]
    if path.to_string_lossy().contains("/tree/") && FAIL_TREE_APPEND.with(|c| c.get()) {
        return Err(plain("injected disk error".to_string()));
    }
    #[cfg(test)]
    if path.to_string_lossy().contains("/main/") {
        if let Some((pass, mode)) = FAIL_APPEND.with(|c| c.get()) {
            if pass > 0 {
                FAIL_APPEND.with(|c| c.set(Some((pass - 1, mode))));
            } else {
                FAIL_APPEND.with(|c| c.set(None));
                let n = match mode {
                    FailMode::Write => line.len() / 2,
                    FailMode::Sync => line.len(),
                };
                let _ = f.write_all(&line[..n]);
                return Err(undo(&f, "injected disk error".to_string()));
            }
        }
    }
    if let Err(e) = f.write_all(line) {
        return Err(undo(&f, path_err("write", path, &e)));
    }
    if !no_sync() {
        if let Err(e) = f.sync_all() {
            return Err(undo(&f, path_err("sync", path, &e)));
        }
    }
    if !existed {
        sync_dir(path.parent().unwrap_or(Path::new("."))).map_err(plain)?;
    }
    Ok(off)
}

/// Appends s as a JSON string the way Go's encoder writes it with HTML
/// escaping off: `<`, `>` and `&` stay as they are, U+2028 and U+2029 are
/// still escaped.
pub fn push_json_str(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
}

/// A JSON string as push_json_str writes it.
pub fn json_str(s: &str) -> String {
    let mut out = String::new();
    push_json_str(&mut out, s);
    out
}

/// The longest prefix of s that has at most n bytes and ends on a char
/// boundary.
pub fn clip(s: &str, n: usize) -> &str {
    if s.len() <= n {
        return s;
    }
    let mut n = n;
    while n > 0 && !s.is_char_boundary(n) {
        n -= 1;
    }
    &s[..n]
}

/// Cuts s into parts of at most n bytes, each cut on a char boundary. An
/// empty s is one empty part. A char wider than n goes whole.
pub fn split(s: &str, n: usize) -> Vec<String> {
    if s.len() <= n {
        return vec![s.to_string()];
    }
    let mut out = Vec::new();
    let mut s = s;
    while !s.is_empty() {
        let mut p = clip(s, n);
        if p.is_empty() {
            let w = s.chars().next().map_or(1, char::len_utf8);
            p = &s[..w];
        }
        out.push(p.to_string());
        s = &s[p.len()..];
    }
    out
}

/// Go's strings.ToValidUTF8(s, "\u{fffd}") for bytes: each run of bytes that
/// are not UTF-8 becomes one U+FFFD.
pub fn to_valid_utf8(b: &[u8]) -> String {
    let mut out = String::new();
    let mut rest = b;
    let mut in_bad = false;
    while !rest.is_empty() {
        match std::str::from_utf8(rest) {
            Ok(s) => {
                out.push_str(s);
                break;
            }
            Err(e) => {
                let good = e.valid_up_to();
                if good > 0 {
                    out.push_str(std::str::from_utf8(&rest[..good]).unwrap_or(""));
                    in_bad = false;
                }
                if !in_bad {
                    out.push('\u{fffd}');
                    in_bad = true;
                }
                let bad = e.error_len().unwrap_or(rest.len() - good);
                rest = &rest[good + bad.max(1)..];
            }
        }
    }
    out
}
