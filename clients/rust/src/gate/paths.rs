//! The posixpath and pathlib behavior the Python gate relies on, in
//! Python 3.12's exact form, so that every path check lands on the same
//! string.

use super::py::text::{fsdecode, fsencode, repr};
use super::py::PyErr;
use super::{undecided, Runner, R};
use std::collections::HashMap;
use std::os::unix::fs::MetadataExt;

pub fn isabs(p: &str) -> bool {
    p.starts_with('/')
}

/// `posixpath.normpath`.
pub fn normpath(p: &str) -> String {
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
    let mut out: Vec<&str> = Vec::new();
    for c in p.split('/') {
        if c.is_empty() || c == "." {
            continue;
        }
        if c != ".." || (initial == 0 && out.is_empty()) || out.last() == Some(&"..") {
            out.push(c);
        } else if !out.is_empty() {
            out.pop();
        }
    }
    let s = format!("{}{}", "/".repeat(initial), out.join("/"));
    if s.is_empty() {
        ".".into()
    } else {
        s
    }
}

/// `posixpath.join`.
pub fn join(a: &str, rest: &[&str]) -> String {
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

pub fn join2(a: &str, b: &str) -> String {
    join(a, &[b])
}

/// `posixpath.split`.
pub fn split(p: &str) -> (String, String) {
    let i = p.rfind('/').map(|i| i + 1).unwrap_or(0);
    let (head, tail) = (&p[..i], &p[i..]);
    let head = if !head.is_empty() && head.chars().any(|c| c != '/') { head.trim_end_matches('/') } else { head };
    (head.to_string(), tail.to_string())
}

/// `posixpath.basename`.
pub fn basename(p: &str) -> &str {
    match p.rfind('/') {
        Some(i) => &p[i + 1..],
        None => p,
    }
}

fn non_empty(p: &str) -> Vec<&str> {
    p.split('/').filter(|c| !c.is_empty()).collect()
}

/// `posixpath.commonpath` for paths of one kind. `None` where Python raises.
pub fn commonpath(paths: &[&str]) -> Option<String> {
    if paths.is_empty() {
        return None;
    }
    let abs = isabs(paths[0]);
    let mut split: Vec<Vec<&str>> = Vec::new();
    for p in paths {
        if isabs(p) != abs {
            return None;
        }
        split.push(p.split('/').filter(|c| !c.is_empty() && *c != ".").collect());
    }
    let s1 = split.iter().min().unwrap();
    let s2 = split.iter().max().unwrap();
    let mut common: &[&str] = s1;
    for (i, c) in s1.iter().enumerate() {
        if *c != s2[i] {
            common = &s1[..i];
            break;
        }
    }
    let prefix = if abs { "/" } else { "" };
    Some(format!("{prefix}{}", common.join("/")))
}

/// `str(PurePosixPath(p))`: slashes collapsed, `.` parts dropped, a
/// trailing slash removed, and a leading `//` kept.
pub fn path_str(p: &str) -> String {
    let root = if p.starts_with('/') {
        if p.starts_with("//") && !p.starts_with("///") {
            "//"
        } else {
            "/"
        }
    } else {
        ""
    };
    let keep: Vec<&str> = p.split('/').filter(|c| !c.is_empty() && *c != ".").collect();
    let s = format!("{root}{}", keep.join("/"));
    if s.is_empty() {
        ".".into()
    } else {
        s
    }
}

/// `PurePosixPath(p).parts`.
pub fn path_parts(p: &str) -> Vec<String> {
    let s = path_str(p);
    if s == "." {
        return vec![];
    }
    let mut out = Vec::new();
    let rest = if let Some(r) = s.strip_prefix("//") {
        out.push("//".to_string());
        r
    } else if let Some(r) = s.strip_prefix('/') {
        out.push("/".to_string());
        r
    } else {
        &s
    };
    if !rest.is_empty() {
        out.extend(rest.split('/').map(String::from));
    }
    out
}

/// `PurePosixPath(p).name`: the last part, or empty for the root.
pub fn path_name(p: &str) -> String {
    let parts = path_parts(p);
    match parts.last() {
        None => String::new(),
        Some(l) if parts.len() == 1 && (l == "/" || l == "//") => String::new(),
        Some(l) => l.clone(),
    }
}

/// `str(PurePosixPath(p).parent)`.
pub fn path_parent(p: &str) -> String {
    let parts = path_parts(p);
    if parts.is_empty() {
        return ".".into();
    }
    if parts[0] == "/" || parts[0] == "//" {
        if parts.len() <= 2 {
            return parts[0].clone();
        }
        return format!("{}{}", parts[0], parts[1..parts.len() - 1].join("/"));
    }
    if parts.len() == 1 {
        return ".".into();
    }
    parts[..parts.len() - 1].join("/")
}

/// `str(PurePosixPath(a) / b)`.
pub fn path_join(a: &str, b: &str) -> String {
    if isabs(b) {
        return path_str(b);
    }
    path_str(&format!("{}/{}", path_str(a), b))
}

/// Whether `PurePosixPath(p) == d` or `d` is one of its parents. Both must
/// be `path_str`-normalized.
pub fn path_under_or_equal(p: &str, d: &str) -> bool {
    if p == d {
        return true;
    }
    let mut p = p.to_string();
    loop {
        let parent = path_parent(&p);
        if parent == p {
            return false;
        }
        if parent == d {
            return true;
        }
        p = parent;
    }
}

fn errno_of(e: &std::io::Error) -> Option<i32> {
    e.raw_os_error()
}

/// Python's recursion limit.
pub const RECURSION_LIMIT: i64 = 1000;

/// The deepest symlink nesting the port follows: past any recursion limit.
const REALPATH_NEST_CAP: i64 = 2 * RECURSION_LIMIT;

/// A path as an `os` function takes it: NUL raises ValueError naming the
/// function, and a lone surrogate the UTF-8 codec cannot escape raises
/// UnicodeEncodeError.
pub fn os_path(func: &str, p: &str) -> Result<std::path::PathBuf, PyErr> {
    use std::os::unix::ffi::OsStringExt;
    if p.contains('\0') {
        return Err(PyErr::value(format!("{func}: embedded null character in path")));
    }
    let b = fsencode(p)?;
    Ok(std::path::PathBuf::from(std::ffi::OsString::from_vec(b)))
}

/// An OSError as Python prints it: `[Errno N] strerror: 'path'`.
pub fn os_error(e: &std::io::Error, path: &str) -> PyErr {
    match e.raw_os_error() {
        Some(n) => PyErr::new(os_error_class(n), format!("[Errno {n}] {}: {}", strerror(n), repr(path))),
        None => PyErr::os(e.to_string()),
    }
}

/// The OSError subclass Python raises for an errno.
pub fn os_error_class(n: i32) -> &'static str {
    match n {
        libc::ENOENT => "FileNotFoundError",
        libc::EEXIST => "FileExistsError",
        libc::EISDIR => "IsADirectoryError",
        libc::ENOTDIR => "NotADirectoryError",
        libc::EACCES | libc::EPERM => "PermissionError",
        _ => "OSError",
    }
}

/// `os.strerror(n)`.
pub fn strerror(n: i32) -> String {
    unsafe {
        let p = libc::strerror(n);
        if p.is_null() {
            return format!("Unknown error {n}");
        }
        std::ffi::CStr::from_ptr(p).to_string_lossy().into_owned()
    }
}

/// `Path.exists()` of Python 3.12: false for the errors that mean "no such
/// file" and for a path the OS cannot take; any other OSError raises.
pub fn exists(p: &str) -> R<bool> {
    let path = match os_path("stat", p) {
        Ok(x) => x,
        Err(_) => return Ok(false),
    };
    match std::fs::metadata(&path) {
        Ok(_) => Ok(true),
        Err(e) => match errno_of(&e) {
            Some(libc::ENOENT) | Some(libc::ENOTDIR) | Some(libc::EBADF) | Some(libc::ELOOP) => Ok(false),
            _ => Err(os_error(&e, p).into()),
        },
    }
}

/// `os.path.lexists`.
pub fn lexists(p: &str) -> bool {
    match os_path("lstat", p) {
        Ok(x) => std::fs::symlink_metadata(x).is_ok(),
        Err(_) => false,
    }
}

impl Runner {
    /// `posixpath.expanduser` of Python 3.12: `~` is HOME (or the password
    /// database's entry for this uid when HOME is unset), `~user` is that
    /// user's home, and an unknown user leaves the path as it is.
    pub fn expanduser(&self, p: &str) -> R<String> {
        if !p.starts_with('~') {
            return Ok(p.to_string());
        }
        let i = match p[1..].find('/') {
            Some(k) => k + 1,
            None => p.len(),
        };
        let userhome = if i == 1 {
            match self.env.get("HOME") {
                Some(h) => h.clone(),
                None => match pw_dir_of_uid(unsafe { libc::getuid() }) {
                    Some(h) => h,
                    None => return Ok(p.to_string()),
                },
            }
        } else {
            match pw_dir_of_name(&p[1..i])? {
                Some(h) => h,
                None => return Ok(p.to_string()),
            }
        };
        let out = format!("{}{}", userhome.trim_end_matches('/'), &p[i..]);
        Ok(if out.is_empty() { "/".into() } else { out })
    }

    /// `str(Path(p).expanduser())`: a relative path whose first part starts
    /// with `~` takes that part's home; an unknown user raises.
    pub fn path_expanduser(&self, p: &str) -> R<String> {
        let norm = path_str(p);
        if isabs(&norm) || !norm.starts_with('~') {
            return Ok(norm);
        }
        let (first, rest) = match norm.find('/') {
            Some(k) => (&norm[..k], Some(&norm[k + 1..])),
            None => (norm.as_str(), None),
        };
        let homedir = self.expanduser(first)?;
        if homedir.starts_with('~') {
            return Err(PyErr::new("RuntimeError", "Could not determine home directory.").into());
        }
        Ok(match rest {
            Some(r) => path_join(&homedir, r),
            None => path_str(&homedir),
        })
    }

    /// `posixpath.abspath`.
    pub fn abspath(&self, p: &str) -> R<String> {
        if isabs(p) {
            return Ok(normpath(p));
        }
        Ok(normpath(&join2(&self.getwd()?, p)))
    }

    pub fn getwd(&self) -> R<String> {
        let wd = self.wd.get_or_init(|| std::env::current_dir().ok().and_then(|p| p.to_str().map(String::from)));
        match wd {
            Some(w) => Ok(w.clone()),
            None => undecided("the process working directory cannot be read"),
        }
    }

    /// `posixpath.realpath(strict=False)` of Python 3.12. Python resolves
    /// each symlink in a nested call, so a long chain of symlinks raises
    /// RecursionError once the nesting passes the recursion limit; the port
    /// raises it at the same point, from the Python frames on the stack at
    /// the call (`frames::depth`).
    pub fn realpath(&self, filename: &str) -> R<String> {
        super::frames::trace("realpath");
        let frames = super::frames::depth();
        // One call's answers are kept for the rest of the call: a long line
        // asks for the same few paths from each cwd many times, and each
        // answer costs an lstat per component. The file system is taken as
        // still for one call, as the oracle assumes between its lookups.
        let cached = self.real_cache.lock().ok().and_then(|c| c.get(filename).cloned());
        let (answer, nest) = match cached {
            Some(v) => v,
            None => {
                let mut seen: HashMap<String, Option<String>> = HashMap::new();
                let mut deepest = 0i64;
                let answer = match self.joinrealpath(String::new(), filename, &mut seen, 0, &mut deepest) {
                    Ok((p, _)) => self.abspath(&p).map_err(|e| match e {
                        super::Fault::Raised(e) => e,
                        super::Fault::Undecided(w) => PyErr::undecided(w),
                    }),
                    Err(e) => Err(e),
                };
                if let Ok(mut c) = self.real_cache.lock() {
                    c.insert(filename.to_string(), (answer.clone(), deepest));
                }
                (answer, deepest)
            }
        };
        // realpath runs one frame above the caller and _joinrealpath one
        // more; each nested symlink adds one. Each _joinrealpath calls
        // isabs and join, which call _get_sep: two frames above it.
        if frames + 4 + nest > RECURSION_LIMIT {
            return Err(PyErr::recursion().into());
        }
        Ok(answer?)
    }

    /// `posixpath._joinrealpath`, at nesting `k`. `deepest` records the
    /// deepest nesting reached before the answer (or the error) was found.
    fn joinrealpath(
        &self,
        mut path: String,
        rest: &str,
        seen: &mut HashMap<String, Option<String>>,
        k: i64,
        deepest: &mut i64,
    ) -> Result<(String, bool), PyErr> {
        *deepest = (*deepest).max(k);
        if k > REALPATH_NEST_CAP {
            // Far past any recursion limit: the caller raises.
            return Err(PyErr::recursion());
        }
        let mut rest = rest.to_string();
        if isabs(&rest) {
            rest = rest[1..].to_string();
            path = "/".into();
        }
        while !rest.is_empty() {
            let (name, tail) = match rest.find('/') {
                Some(i) => (rest[..i].to_string(), rest[i + 1..].to_string()),
                None => (rest.clone(), String::new()),
            };
            rest = tail;
            if name.is_empty() || name == "." {
                continue;
            }
            if name == ".." {
                if !path.is_empty() {
                    let (head, t) = split(&path);
                    path = head;
                    if t == ".." {
                        path = join(&path, &["..", ".."]);
                    }
                } else {
                    path = "..".into();
                }
                continue;
            }
            let newpath = join2(&path, &name);
            let is_link = match std::fs::symlink_metadata(os_path("lstat", &newpath)?) {
                Ok(m) => m.file_type().is_symlink(),
                Err(_) => false,
            };
            if !is_link {
                path = newpath;
                continue;
            }
            if let Some(v) = seen.get(&newpath) {
                if let Some(resolved) = v {
                    path = resolved.clone();
                    continue;
                }
                return Ok((join2(&newpath, &rest), false));
            }
            seen.insert(newpath.clone(), None);
            let target = match std::fs::read_link(os_path("readlink", &newpath)?) {
                Ok(t) => {
                    use std::os::unix::ffi::OsStrExt;
                    fsdecode(t.as_os_str().as_bytes())
                }
                Err(e) => return Err(os_error(&e, &newpath)),
            };
            let (p, ok) = self.joinrealpath(path, &target, seen, k + 1, deepest)?;
            path = p;
            if !ok {
                return Ok((join2(&path, &rest), false));
            }
            seen.insert(newpath, Some(path.clone()));
        }
        Ok((path, true))
    }

    /// `posixpath.relpath`.
    pub fn relpath(&self, path: &str, start: &str) -> R<String> {
        let s = self.abspath(start)?;
        let p = self.abspath(path)?;
        let sl = non_empty(&s);
        let pl = non_empty(&p);
        let mut i = 0;
        while i < sl.len() && i < pl.len() && sl[i] == pl[i] {
            i += 1;
        }
        let mut rel: Vec<&str> = vec![".."; sl.len() - i];
        rel.extend_from_slice(&pl[i..]);
        if rel.is_empty() {
            return Ok(".".into());
        }
        Ok(join(rel[0], &rel[1..]))
    }

    /// `Path.resolve(strict=False)` of Python 3.12, in a frame of its own:
    /// realpath, then a stat that turns a symlink loop into RuntimeError.
    pub fn resolve(&self, p: &str) -> R<String> {
        let _f = super::frames::frame();
        let s = self.realpath(p)?;
        let s = path_str(&s);
        if let Err(e) = std::fs::metadata(os_path("stat", &s)?) {
            if e.raw_os_error() == Some(libc::ELOOP) {
                return Err(PyErr::new("RuntimeError", format!("Symlink loop from {}", repr(&s))).into());
            }
        }
        Ok(s)
    }

    /// `str(Path.home())`: `Path("~").expanduser()`.
    pub fn path_home(&self) -> R<String> {
        self.path_expanduser("~")
    }
}

/// The password database's home for a user name, as `pwd.getpwnam`
/// reads it: None for an unknown user; the name is encoded as the file
/// system encodes it, and a NUL raises.
fn pw_dir_of_name(name: &str) -> Result<Option<String>, PyErr> {
    let b = fsencode(name)?;
    if b.contains(&0) {
        return Err(PyErr::value("embedded null byte"));
    }
    let c = std::ffi::CString::new(b).map_err(|_| PyErr::value("embedded null byte"))?;
    Ok(pw_lookup(|pw, buf, len, res| unsafe { libc::getpwnam_r(c.as_ptr(), pw, buf, len, res) }))
}

/// `pwd.getpwuid(uid).pw_dir`, or None.
fn pw_dir_of_uid(uid: libc::uid_t) -> Option<String> {
    pw_lookup(|pw, buf, len, res| unsafe { libc::getpwuid_r(uid, pw, buf, len, res) })
}

fn pw_lookup(
    f: impl Fn(*mut libc::passwd, *mut libc::c_char, libc::size_t, *mut *mut libc::passwd) -> libc::c_int,
) -> Option<String> {
    let mut size = 1024usize;
    loop {
        let mut buf = vec![0 as libc::c_char; size];
        let mut pw: libc::passwd = unsafe { std::mem::zeroed() };
        let mut res: *mut libc::passwd = std::ptr::null_mut();
        let rc = f(&mut pw, buf.as_mut_ptr(), size, &mut res);
        if rc == libc::ERANGE && size < (1 << 20) {
            size *= 2;
            continue;
        }
        if rc != 0 || res.is_null() || pw.pw_dir.is_null() {
            return None;
        }
        let dir = unsafe { std::ffi::CStr::from_ptr(pw.pw_dir) };
        return Some(fsdecode(dir.to_bytes()));
    }
}

/// The owner uid of a path, without following a final symlink.
pub fn lstat_uid_is_socket(p: &str) -> Option<(u32, bool)> {
    use std::os::unix::fs::FileTypeExt;
    let m = std::fs::symlink_metadata(p).ok()?;
    Some((m.uid(), m.file_type().is_socket()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn helpers_match_python() {
        for (i, w) in [("", "."), ("//a//b/", "//a/b"), ("///a/../b", "/b"), ("a/./b/..", "a"), ("../x", "../x")] {
            assert_eq!(normpath(i), w, "normpath({i:?})");
        }
        assert_eq!(path_parent("gate"), ".");
        assert_eq!(path_parent("/a/b/"), "/a");
        assert_eq!(path_parent("/a"), "/");
        assert_eq!(commonpath(&["/a/b/c", "/a/b"]).as_deref(), Some("/a/b"));
        assert_eq!(commonpath(&["/a", "b"]), None);
        assert_eq!(split("/a/b"), ("/a".into(), "b".into()));
        assert_eq!(split("//"), ("//".into(), "".into()));
        assert_eq!(path_join("/a", "b/./c/"), "/a/b/c");
        assert!(path_under_or_equal("/a/b/c", "/a"));
        assert!(!path_under_or_equal("/ab", "/a"));
    }
}
