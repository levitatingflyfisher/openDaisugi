//! The gate's state directory as `opendaisugi.gate` lays it out:
//! `envelopes/` (default.json and one file per session), the DISARMED
//! marker, `shadow/` (the verdict log, read by `journal`) and
//! `proposals/` (read only). Every file written here is the file Python
//! writes. The Go client's `gateroot` package is the reference.

use std::collections::HashMap;
use std::fs;
use std::io::{self, Write};
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::Path;
use std::sync::OnceLock;

/// The kill-switch marker's name in the gate root.
pub const DISARM_FILE: &str = "DISARMED";

/// An entry name that is not valid UTF-8, which Python carries as
/// surrogate escapes.
pub const ERR_NAME: &str = "an envelope file name is not valid UTF-8";

/// `gate._envelopes_dir`.
pub fn envelopes_dir(root: &str) -> String {
    join(root, "envelopes")
}

/// `gate._shadow_dir`.
pub fn shadow_dir(root: &str) -> String {
    join(root, "shadow")
}

/// `gate.disarm`: the root made private and the marker written. The second
/// value is the file written when the marker path was a symlink.
pub fn disarm(root: &str) -> (String, Option<String>, io::Result<()>) {
    let marker = join(root, DISARM_FILE);
    if let Err(e) = mkdir_private(root) {
        return (marker, None, Err(e));
    }
    match write_file(&marker, "disarmed by operator\n") {
        Ok(f) => (marker, f, Ok(())),
        Err(e) => (marker, None, Err(e)),
    }
}

/// `gate.arm`: the marker removed if `Path.exists()` sees it.
pub fn arm(root: &str) -> io::Result<()> {
    let marker = join(root, DISARM_FILE);
    if exists(&marker) {
        return fs::remove_file(&marker);
    }
    Ok(())
}

/// `gate.is_disarmed`.
pub fn is_disarmed(root: &str) -> bool {
    exists(&join(root, DISARM_FILE))
}

/// `sorted(e.stem for e in envelopes.glob("*.json"))`: every entry whose
/// name ends in .json, hidden ones and directories included, by stem.
pub fn envelopes(root: &str) -> Result<Vec<String>, String> {
    let d = envelopes_dir(root);
    if !exists(&d) {
        return Ok(vec![]);
    }
    let entries = match fs::read_dir(&d) {
        Ok(e) => e,
        Err(e) => {
            // pathlib globs a file as an empty directory.
            if fs::metadata(&d).map(|m| !m.is_dir()).unwrap_or(false) {
                return Ok(vec![]);
            }
            return Err(e.to_string());
        }
    };
    let mut out = vec![];
    for e in entries {
        let e = e.map_err(|e| e.to_string())?;
        let name = match e.file_name().into_string() {
            Ok(n) => n,
            Err(_) => return Err(ERR_NAME.into()),
        };
        if name.ends_with(".json") {
            out.push(stem(&name).to_string());
        }
    }
    out.sort();
    Ok(out)
}

/// `PurePath.stem`.
fn stem(name: &str) -> &str {
    match name.rfind('.') {
        Some(i) if 0 < i && i < name.len() - 1 => &name[..i],
        _ => name,
    }
}

/// `str(pathlib.PurePosixPath(p))`.
pub fn path_str(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let lead = if p.starts_with("//") && !p.starts_with("///") {
        "//"
    } else if p.starts_with('/') {
        "/"
    } else {
        ""
    };
    let parts: Vec<&str> = p.split('/').filter(|s| !s.is_empty() && *s != ".").collect();
    let out = format!("{lead}{}", parts.join("/"));
    if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

/// pathlib's `a / b` for a relative b.
pub fn join(a: &str, b: &str) -> String {
    if a == "." || a.is_empty() {
        return b.into();
    }
    if a == "/" || a == "//" {
        return format!("{a}{b}");
    }
    format!("{a}/{b}")
}

/// `PurePosixPath(p).parent`.
pub fn parent(p: &str) -> String {
    match p.rfind('/') {
        None => ".".into(),
        Some(_) if p == "/" || p == "//" => p.into(),
        Some(0) => "/".into(),
        Some(1) if p.starts_with("//") => "//".into(),
        Some(i) => p[..i].into(),
    }
}

/// `os.getcwd()`: the kernel's answer, never $PWD.
pub fn getwd() -> io::Result<String> {
    let d = std::env::current_dir()?;
    d.into_os_string().into_string().map_err(|_| io::Error::other("the working directory is not valid UTF-8"))
}

/// pathlib's `absolute()`.
pub fn abs(p: &str) -> io::Result<String> {
    let p = path_str(p);
    if p.starts_with('/') {
        return Ok(p);
    }
    let cwd = getwd()?;
    if p == "." {
        return Ok(cwd);
    }
    Ok(join(&cwd, &p))
}

/// `Path.resolve()` (strict=False).
pub fn resolve(p: &str) -> io::Result<String> {
    let a = abs(p)?;
    Ok(realpath(&a))
}

fn realpath(filename: &str) -> String {
    let (path, _) = joinrealpath(String::new(), filename, &mut HashMap::new());
    normpath(&path)
}

fn is_symlink(p: &str) -> bool {
    fs::symlink_metadata(p).map(|m| m.file_type().is_symlink()).unwrap_or(false)
}

fn joinrealpath(mut path: String, rest: &str, seen: &mut HashMap<String, Option<String>>) -> (String, bool) {
    let mut rest = rest.to_string();
    if let Some(r) = rest.strip_prefix('/') {
        rest = r.to_string();
        path = "/".into();
    }
    while !rest.is_empty() {
        let (name, r) = match rest.split_once('/') {
            Some((a, b)) => (a.to_string(), b.to_string()),
            None => (rest.clone(), String::new()),
        };
        rest = r;
        if name.is_empty() || name == "." {
            continue;
        }
        if name == ".." {
            if !path.is_empty() {
                let (head, tail) = split(&path);
                path = head;
                if tail == ".." {
                    path = pjoin(&path, &["..", ".."]);
                }
            } else {
                path = "..".into();
            }
            continue;
        }
        let newpath = pjoin(&path, &[&name]);
        if !is_symlink(&newpath) {
            path = newpath;
            continue;
        }
        if let Some(v) = seen.get(&newpath) {
            match v {
                Some(p) => {
                    path = p.clone();
                    continue;
                }
                None => return (pjoin(&newpath, &[&rest]), false),
            }
        }
        seen.insert(newpath.clone(), None);
        let target = match fs::read_link(&newpath).ok().and_then(|t| t.into_os_string().into_string().ok()) {
            Some(t) => t,
            None => {
                path = newpath;
                continue;
            }
        };
        let (p, ok) = joinrealpath(path, &target, seen);
        path = p;
        if !ok {
            return (pjoin(&path, &[&rest]), false);
        }
        seen.insert(newpath, Some(path.clone()));
    }
    (path, true)
}

fn pjoin(a: &str, rest: &[&str]) -> String {
    let mut path = a.to_string();
    for b in rest {
        if b.starts_with('/') {
            path = b.to_string();
        } else if path.is_empty() || path.ends_with('/') {
            path.push_str(b);
        } else {
            path.push('/');
            path.push_str(b);
        }
    }
    path
}

fn split(p: &str) -> (String, String) {
    let i = p.rfind('/').map(|i| i + 1).unwrap_or(0);
    let (mut head, tail) = (p[..i].to_string(), p[i..].to_string());
    if !head.is_empty() && !head.trim_matches('/').is_empty() {
        head = head.trim_end_matches('/').to_string();
    }
    (head, tail)
}

/// `posixpath.normpath`.
fn normpath(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let initial = if p.starts_with('/') {
        if p.starts_with("//") && !p.starts_with("///") {
            2
        } else {
            1
        }
    } else {
        0
    };
    let mut comps: Vec<&str> = vec![];
    for c in p.split('/') {
        if c.is_empty() || c == "." {
            continue;
        }
        if c != ".." || (initial == 0 && comps.is_empty()) || comps.last() == Some(&"..") {
            comps.push(c);
        } else if !comps.is_empty() {
            comps.pop();
        }
    }
    let out = format!("{}{}", "/".repeat(initial), comps.join("/"));
    if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

/// `hook._safe_session_id` on a string.
pub fn safe_session_id(raw: &str) -> String {
    let s: String =
        raw.chars().map(|r| if r.is_ascii_alphanumeric() || r == '.' || r == '_' || r == '-' { r } else { '_' }).collect();
    let s = s.trim_matches('.');
    // Only ASCII is left, so bytes are characters.
    let s = &s[..s.len().min(128)];
    if s.is_empty() {
        "no-session".into()
    } else {
        s.into()
    }
}

/// `Path.exists()`: stat follows symlinks; any error is false.
pub fn exists(p: &str) -> bool {
    fs::metadata(p).is_ok()
}

/// `gate._mkdir_private`: parents with the default mode, the directory
/// itself 0700 whether it was made now or not.
pub fn mkdir_private(d: &str) -> io::Result<()> {
    mkdir_parents(d, 0o700)?;
    let _ = fs::set_permissions(d, fs::Permissions::from_mode(0o700));
    Ok(())
}

/// `Path.mkdir(parents=True, exist_ok=True, mode=mode)`.
pub fn mkdir_parents(d: &str, mode: u32) -> io::Result<()> {
    if let Ok(m) = fs::metadata(d) {
        if m.is_dir() {
            return Ok(());
        }
        return Err(io::Error::from_raw_os_error(libc::EEXIST));
    }
    let p = parent(d);
    if p != d && p != "." {
        mkdir_parents(&p, 0o777)?;
    }
    use std::os::unix::fs::DirBuilderExt;
    match fs::DirBuilder::new().mode(mode).create(d) {
        Err(e) if e.kind() == io::ErrorKind::AlreadyExists && fs::metadata(d).map(|m| m.is_dir()).unwrap_or(false) => {
            Ok(())
        }
        r => r,
    }
}

/// `os.makedirs(d, exist_ok=True)` with the default mode.
pub fn mkdir_all(d: &str) -> io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    fs::DirBuilder::new().recursive(true).mode(0o777).create(d)
}

const MAX_LINKS: usize = 40;

/// The file a write through `p` lands on, following symlinks, dangling
/// ones included.
fn resolve_links(p: &str) -> io::Result<String> {
    let mut p = p.to_string();
    for _ in 0..MAX_LINKS {
        if !is_symlink(&p) {
            return Ok(p);
        }
        let target = fs::read_link(&p)?
            .into_os_string()
            .into_string()
            .map_err(|_| io::Error::other("a symlink target is not valid UTF-8"))?;
        p = if target.starts_with('/') {
            target
        } else {
            clean(&format!("{}/{}", dir_of(&p), target))
        };
    }
    Err(io::Error::from_raw_os_error(libc::ELOOP))
}

/// `filepath.Dir`.
fn dir_of(p: &str) -> String {
    match p.rfind('/') {
        None => ".".into(),
        Some(0) => "/".into(),
        Some(i) => clean(&p[..i]),
    }
}

/// `filepath.Clean`: lexical, `..` applied.
fn clean(p: &str) -> String {
    let rooted = p.starts_with('/');
    let mut out: Vec<&str> = vec![];
    for c in p.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                if out.last().is_some_and(|l| *l != "..") {
                    out.pop();
                } else if !rooted {
                    out.push("..");
                }
            }
            c => out.push(c),
        }
    }
    let s = out.join("/");
    match (rooted, s.is_empty()) {
        (true, _) => format!("/{s}"),
        (false, true) => ".".into(),
        (false, false) => s,
    }
}

/// The process umask, read from /proc/self/status, which never changes it.
fn process_umask() -> u32 {
    static U: OnceLock<u32> = OnceLock::new();
    *U.get_or_init(|| {
        if let Ok(raw) = fs::read_to_string("/proc/self/status") {
            for line in raw.lines() {
                if let Some(v) = line.strip_prefix("Umask:") {
                    if let Ok(n) = u32::from_str_radix(v.trim(), 8) {
                        return n & 0o777;
                    }
                }
            }
        }
        unsafe {
            let old = libc::umask(0o077);
            libc::umask(old);
            old as u32
        }
    })
}

/// Writes `text` to `p` as `Path.write_text` does, but atomically. When p
/// is a symlink the file it points to is written, and its path returned.
pub fn write_file(p: &str, text: &str) -> io::Result<Option<String>> {
    let target = resolve_links(p)?;
    replace_file(&target, text, None)?;
    Ok((target != p).then_some(target))
}

/// `write_file` for a private file: `mode` from the moment it exists.
pub fn write_file_mode(p: &str, text: &str, mode: u32) -> io::Result<Option<String>> {
    let target = resolve_links(p)?;
    replace_file(&target, text, Some(mode))?;
    Ok((target != p).then_some(target))
}

/// Writes `text` to `p` atomically. A symlink at p is replaced by the new
/// file, never written through.
pub fn replace(p: &str, text: &str) -> io::Result<()> {
    replace_file(p, text, None)
}

fn replace_file(p: &str, text: &str, forced: Option<u32>) -> io::Result<()> {
    let mut mode = 0o666 & !process_umask();
    match fs::symlink_metadata(p) {
        Ok(m) if m.file_type().is_file() => mode = m.mode() & 0o777,
        Ok(m) if m.is_dir() => return Err(io::Error::from_raw_os_error(libc::EISDIR)),
        _ => {}
    }
    if let Some(f) = forced {
        mode = f;
    }
    let dir = dir_of(p);
    let base = p.rsplit('/').next().unwrap_or(p);
    let (tmp, mut f) = create_temp(&dir, base)?;
    let done = (|| -> io::Result<()> {
        f.set_permissions(fs::Permissions::from_mode(mode))?;
        f.write_all(text.as_bytes())?;
        f.sync_all()?;
        drop(f);
        fs::rename(&tmp, p)
    })();
    if done.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    done
}

/// `os.CreateTemp(dir, "." + base + ".tmp*")`: a new file, 0600.
fn create_temp(dir: &str, base: &str) -> io::Result<(String, fs::File)> {
    use std::os::unix::fs::OpenOptionsExt;
    for _ in 0..10000 {
        let mut b = [0u8; 5];
        if let Ok(mut r) = fs::File::open("/dev/urandom") {
            use std::io::Read;
            let _ = r.read_exact(&mut b);
        }
        let n = u64::from_le_bytes([b[0], b[1], b[2], b[3], b[4], 0, 0, 0]);
        let name = format!("{}/.{}.tmp{}", dir, base, n % 10_000_000_000);
        match fs::OpenOptions::new().write(true).create_new(true).mode(0o600).open(&name) {
            Ok(f) => return Ok((name, f)),
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    }
    Err(io::Error::other("no temporary file name was free"))
}

/// Whether `p` names a symlink.
pub fn is_link(p: &str) -> bool {
    is_symlink(p)
}

/// `os.Stat(p).IsDir()`.
pub fn is_dir(p: &str) -> bool {
    fs::metadata(p).map(|m| m.is_dir()).unwrap_or(false)
}

/// `os.Lstat(p).IsDir()`.
pub fn is_dir_no_follow(p: &str) -> bool {
    fs::symlink_metadata(p).map(|m| m.is_dir()).unwrap_or(false)
}

/// An `io::Error` in Go's words: `op path: reason`, as the Go client
/// prints it.
pub fn err_text(e: &io::Error) -> String {
    let s = e.to_string();
    // Rust appends " (os error N)"; the Go client prints the bare reason.
    match s.rfind(" (os error ") {
        Some(i) => s[..i].to_lowercase_first(),
        None => s,
    }
}

trait LowerFirst {
    fn to_lowercase_first(&self) -> String;
}

impl LowerFirst for str {
    fn to_lowercase_first(&self) -> String {
        let mut c = self.chars();
        match c.next() {
            Some(f) => f.to_lowercase().collect::<String>() + c.as_str(),
            None => String::new(),
        }
    }
}

/// Whether a path is there at all (a dangling symlink included).
pub fn lexists(p: &str) -> bool {
    Path::new(p).symlink_metadata().is_ok()
}
