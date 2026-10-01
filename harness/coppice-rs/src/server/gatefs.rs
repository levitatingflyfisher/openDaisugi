//! Opens the files under the daisugi gate root as the gate does (GR-R-1,
//! GR-R-7 to GR-R-10), so coppice reads and writes only what the gate would
//! read, and says so when the gate would refuse. The Rust twin of
//! `internal/gatefs` in the Go coppice, and a COPY of the checks in the
//! daisugi module's `statefile` (clients/go, clients/rust), because coppice
//! is a separate crate. The three must agree.
//!
//! The root is opened as a directory without following a symlink at its
//! last component (the path down to the root may follow links), each
//! directory below it by the fd of the one above, and each file by its fd. A
//! directory must be this user's and writable by no group and no other
//! user. A file must be a regular file this user owns, with one link,
//! writable by no group and no other user. The reasons and the fix words are
//! the gate's.

use std::ffi::CString;
use std::io::{self, Read};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::path::{Path, PathBuf};

const DIR_FLAGS: libc::c_int = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
const FILE_FLAGS: libc::c_int = libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC;

/// `str(Path(root))` as the gate cleans a root: empty and "." parts dropped,
/// ".." kept. A trailing slash or a "." part would make the kernel resolve a
/// symlink at the last component, and the no-follow open would follow it.
pub fn norm(root: &Path) -> PathBuf {
    use std::os::unix::ffi::{OsStrExt, OsStringExt};
    let b = root.as_os_str().as_bytes();
    let parts: Vec<&[u8]> = b.split(|c| *c == b'/').filter(|p| !p.is_empty() && *p != b".").collect();
    let mut out: Vec<u8> = Vec::new();
    if b.first() == Some(&b'/') {
        out.push(b'/');
    }
    out.extend(parts.join(&b'/'));
    if out.is_empty() {
        out.push(b'.');
    }
    PathBuf::from(std::ffi::OsString::from_vec(out))
}

/// The path in single quotes, a quote inside it written as quote,
/// backslash, quote, quote.
pub fn quote(p: &str) -> String {
    format!("'{}'", p.replace('\'', "'\\''"))
}

/// `<path>: <why> (fix: ...)`, the gate's words.
pub fn refusal(p: &str, why: &str, is_dir: bool) -> String {
    let q = quote(p);
    let fix = match why {
        "is a symbolic link" => format!("remove the link {q}"),
        "is not a directory" | "is not a regular file" => format!("move {q} aside"),
        "is owned by another user" => format!("chown \"$(id -un)\" {q}"),
        "has more than one hard link" => format!("cp {q} {q}.new && mv {q}.new {q}"),
        "is writable by group or others" => format!("chmod {} {q}", if is_dir { "700" } else { "600" }),
        w if w.starts_with("is larger than ") => format!("shorten or remove {q}"),
        _ => format!("check that this user can read {q}"),
    };
    format!("{p}: {why} (fix: {fix})")
}

/// An error that carries a refusal: a path the gate would refuse.
#[derive(Debug)]
pub struct Refused(pub String);

impl std::fmt::Display for Refused {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for Refused {}

/// The refusal text in an error `write` returned, or None for any other.
/// Only the tests read it: callers match on the `Refused` value itself.
#[cfg(test)]
pub fn refused_text(e: &io::Error) -> Option<&str> {
    e.get_ref().and_then(|i| i.downcast_ref::<Refused>()).map(|r| r.0.as_str())
}

/// Why a path is not readable: plainly missing, or the gate's refusal line.
#[derive(Debug, PartialEq, Eq)]
pub enum Miss {
    Missing,
    Refused(String),
}

fn euid() -> u32 {
    unsafe { libc::geteuid() }
}

fn cstr(b: &[u8]) -> Option<CString> {
    CString::new(b).ok()
}

fn open_at(dirfd: RawFd, name: &[u8], flags: libc::c_int, mode: u32) -> io::Result<OwnedFd> {
    let c = cstr(name).ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
    let fd = unsafe { libc::openat(dirfd, c.as_ptr(), flags, mode as libc::c_uint) };
    if fd < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(unsafe { OwnedFd::from_raw_fd(fd) })
    }
}

fn fstat(fd: RawFd) -> Option<libc::stat> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    (unsafe { libc::fstat(fd, &mut st) } == 0).then_some(st)
}

/// Why an open failed: None when the path is plainly missing.
fn why_not(parent: RawFd, name: &[u8], want_dir: bool) -> Option<&'static str> {
    match open_at(parent, name, libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC, 0) {
        Err(e) if matches!(e.raw_os_error(), Some(libc::ENOENT) | Some(libc::ENOTDIR)) => None,
        Err(_) => Some("cannot be opened"),
        Ok(fd) => match fstat(fd.as_raw_fd()) {
            None => Some("cannot be opened"),
            Some(st) if st.st_mode & libc::S_IFMT == libc::S_IFLNK => Some("is a symbolic link"),
            Some(st) if want_dir && st.st_mode & libc::S_IFMT != libc::S_IFDIR => Some("is not a directory"),
            Some(_) => Some("cannot be opened"),
        },
    }
}

fn open_dir(parent: RawFd, name: &[u8], shown: &str) -> Result<OwnedFd, Miss> {
    let fd = match open_at(parent, name, DIR_FLAGS, 0) {
        Ok(fd) => fd,
        Err(_) => {
            return Err(match why_not(parent, name, true) {
                Some(why) => Miss::Refused(refusal(shown, why, true)),
                None => Miss::Missing,
            })
        }
    };
    let why = match fstat(fd.as_raw_fd()) {
        None => "cannot be opened",
        Some(st) if st.st_uid != euid() => "is owned by another user",
        Some(st) if st.st_mode & 0o022 != 0 => "is writable by group or others",
        Some(_) => return Ok(fd),
    };
    Err(Miss::Refused(refusal(shown, why, true)))
}

/// Opens the root and each directory below it, as the gate does.
pub fn open_dirs(root: &Path, dirs: &[&str]) -> Result<Vec<OwnedFd>, Miss> {
    use std::os::unix::ffi::OsStrExt;
    let root = norm(root);
    let root = root.as_path();
    let mut shown = root.to_string_lossy().into_owned();
    let mut fds = vec![open_dir(libc::AT_FDCWD, root.as_os_str().as_bytes(), &shown)?];
    for d in dirs {
        shown.push('/');
        shown.push_str(d);
        let last = fds.last().map(|f| f.as_raw_fd()).ok_or(Miss::Missing)?;
        let next = open_dir(last, d.as_bytes(), &shown)?;
        fds.push(next);
    }
    Ok(fds)
}

fn open_file_in(dirfd: RawFd, shown_dir: &str, name: &str, max: Option<u64>) -> Result<OwnedFd, Miss> {
    let shown = format!("{shown_dir}/{name}");
    let fd = match open_at(dirfd, name.as_bytes(), FILE_FLAGS, 0) {
        Ok(fd) => fd,
        Err(_) => {
            return Err(match why_not(dirfd, name.as_bytes(), false) {
                Some(why) => Miss::Refused(refusal(&shown, why, false)),
                None => Miss::Missing,
            })
        }
    };
    let why = match fstat(fd.as_raw_fd()) {
        None => "cannot be opened".to_string(),
        Some(st) if st.st_mode & libc::S_IFMT != libc::S_IFREG => "is not a regular file".into(),
        Some(st) if st.st_uid != euid() => "is owned by another user".into(),
        Some(st) if st.st_nlink != 1 => "has more than one hard link".into(),
        Some(st) if st.st_mode & 0o022 != 0 => "is writable by group or others".into(),
        Some(st) if max.is_some_and(|m| st.st_size as u64 > m) => {
            format!("is larger than {} bytes", max.unwrap_or(0))
        }
        Some(_) => return Ok(fd),
    };
    Err(Miss::Refused(refusal(&shown, &why, false)))
}

/// Reads `<root>/<dirs>/<name>` as the gate reads it: Ok(bytes), or why not
/// (missing, or the gate's refusal line). A file over `max` bytes is refused.
pub fn read(root: &Path, dirs: &[&str], name: &str, max: Option<u64>) -> Result<Vec<u8>, Miss> {
    let root = norm(root);
    let root = root.as_path();
    let fds = open_dirs(root, dirs)?;
    let last = fds.last().map(|f| f.as_raw_fd()).ok_or(Miss::Missing)?;
    let mut shown_dir = root.to_string_lossy().into_owned();
    for d in dirs {
        shown_dir.push('/');
        shown_dir.push_str(d);
    }
    let fd = open_file_in(last, &shown_dir, name, max)?;
    let mut buf = Vec::new();
    std::fs::File::from(fd)
        .read_to_end(&mut buf)
        .map_err(|_| Miss::Refused(refusal(&format!("{shown_dir}/{name}"), "cannot be opened", false)))?;
    Ok(buf)
}

fn refused_err(text: String) -> io::Error {
    io::Error::other(Refused(text))
}

/// Opens the root and each directory below it, making a missing one 0700.
/// A link at or below the root, a directory another user owns, or one that
/// group or others can write, is a refusal (see `refused_text`): nothing is
/// chmod-ed (GR-R-8).
fn make_dirs(root: &Path, dirs: &[&str]) -> io::Result<Vec<OwnedFd>> {
    use std::os::unix::ffi::OsStrExt;
    let root = norm(root);
    let root = root.as_path();
    let mut fds: Vec<OwnedFd> = Vec::new();
    let mut shown = root.to_string_lossy().into_owned();
    for i in 0..=dirs.len() {
        let (parent, name): (RawFd, Vec<u8>) = if i == 0 {
            (libc::AT_FDCWD, root.as_os_str().as_bytes().to_vec())
        } else {
            shown.push('/');
            shown.push_str(dirs[i - 1]);
            (fds.last().map(|f| f.as_raw_fd()).unwrap_or(libc::AT_FDCWD), dirs[i - 1].as_bytes().to_vec())
        };
        let c = cstr(&name).ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
        if unsafe { libc::mkdirat(parent, c.as_ptr(), 0o700) } != 0 {
            let e = io::Error::last_os_error();
            if e.raw_os_error() != Some(libc::EEXIST) {
                return Err(e);
            }
        }
        let fd = match open_at(parent, &name, DIR_FLAGS, 0) {
            Ok(fd) => fd,
            Err(_) => {
                let why = why_not(parent, &name, true).unwrap_or("cannot be opened");
                return Err(refused_err(refusal(&shown, why, true)));
            }
        };
        let st = fstat(fd.as_raw_fd()).ok_or_else(io::Error::last_os_error)?;
        if st.st_uid != euid() {
            return Err(refused_err(refusal(&shown, "is owned by another user", true)));
        }
        if st.st_mode & 0o022 != 0 {
            return Err(refused_err(refusal(&shown, "is writable by group or others", true)));
        }
        fds.push(fd);
    }
    Ok(fds)
}

fn random_name() -> io::Result<String> {
    let mut b = [0u8; 6];
    std::fs::File::open("/dev/urandom")?.read_exact(&mut b)?;
    Ok(b.iter().map(|x| format!("{x:02x}")).collect())
}

/// Writes `data` to `<root>/<dirs>/<name>` (0600) through the directory fd:
/// a temp file made O_CREAT|O_EXCL|O_NOFOLLOW under a name nobody can
/// predict, then renamed inside the directory, so a link planted at the name
/// is replaced and never written through. A directory the gate refuses is a
/// refusal (see `refused_text`), and nothing is written.
pub fn write(root: &Path, dirs: &[&str], name: &str, data: &[u8]) -> io::Result<()> {
    use std::io::Write;
    let fds = make_dirs(root, dirs)?;
    let dfd = fds.last().map(|f| f.as_raw_fd()).ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
    let flags = libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_NOFOLLOW | libc::O_CLOEXEC;
    let mut made: Option<(OwnedFd, String)> = None;
    for _ in 0..16 {
        let tmp = format!(".{name}.{}.tmp", random_name()?);
        match open_at(dfd, tmp.as_bytes(), flags, 0o600) {
            Ok(fd) => {
                made = Some((fd, tmp));
                break;
            }
            Err(e) if e.raw_os_error() == Some(libc::EEXIST) => continue,
            Err(e) => return Err(e),
        }
    }
    let Some((fd, tmp)) = made else {
        return Err(io::Error::from_raw_os_error(libc::EEXIST));
    };
    let res = (|| {
        if unsafe { libc::fchmod(fd.as_raw_fd(), 0o600) } != 0 {
            return Err(io::Error::last_os_error());
        }
        let mut f = std::fs::File::from(fd);
        f.write_all(data)?;
        drop(f);
        let (t, n) = (cstr(tmp.as_bytes()), cstr(name.as_bytes()));
        let (Some(t), Some(n)) = (t, n) else {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        };
        if unsafe { libc::renameat(dfd, t.as_ptr(), dfd, n.as_ptr()) } != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    })();
    if res.is_err() {
        if let Some(t) = cstr(tmp.as_bytes()) {
            unsafe { libc::unlinkat(dfd, t.as_ptr(), 0) };
        }
    }
    res
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{symlink, PermissionsExt};

    fn scratch(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("gatefs-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn real_root(d: &Path) -> PathBuf {
        let r = d.join("gate");
        std::fs::create_dir(&r).unwrap();
        std::fs::set_permissions(&r, std::fs::Permissions::from_mode(0o700)).unwrap();
        r
    }

    // GR r1 M4: a trailing slash or a "." part makes the kernel resolve a
    // symlink at the last component, so the root is cleaned before the
    // no-follow open, as the gate cleans it.
    #[test]
    fn a_root_with_a_trailing_slash_does_not_follow_a_symlink() {
        let d = scratch("slash");
        let real = d.join("real");
        std::fs::create_dir(&real).unwrap();
        std::fs::set_permissions(&real, std::fs::Permissions::from_mode(0o700)).unwrap();
        let link = d.join("gate");
        symlink(&real, &link).unwrap();
        for spelled in [format!("{}", link.display()), format!("{}/", link.display()), format!("{}/.", link.display()), format!("{}//", link.display()), format!("{}/./gate", d.display())] {
            assert!(open_dirs(Path::new(&spelled), &[]).is_err(), "{spelled}: opened a symlinked root");
            let e = make_dirs(Path::new(&spelled), &["answers"]).err().unwrap_or_else(|| panic!("{spelled}: made a directory through a symlinked root"));
            assert!(refused_text(&e).is_some(), "{spelled}: {e}");
        }
        assert!(!real.join("answers").exists());
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn a_clean_root_is_shown_as_the_gate_shows_it() {
        let d = scratch("show");
        let r = real_root(&d);
        std::fs::set_permissions(&r, std::fs::Permissions::from_mode(0o775)).unwrap();
        match open_dirs(Path::new(&format!("{}/", r.display())), &[]) {
            Err(Miss::Refused(t)) => assert_eq!(t, refusal(&r.display().to_string(), "is writable by group or others", true)),
            other => panic!("{:?}", other.map(|_| ())),
        }
        let _ = std::fs::remove_dir_all(&d);
    }

    // GR r1 M2: coppice makes no directory more private than the gate reads it.
    #[test]
    fn make_dirs_refuses_a_group_writable_root_and_keeps_its_mode() {
        let d = scratch("mode");
        let r = real_root(&d);
        std::fs::set_permissions(&r, std::fs::Permissions::from_mode(0o775)).unwrap();
        let e = make_dirs(&r, &["answers"]).err().expect("a refusal");
        assert!(refused_text(&e).is_some());
        assert_eq!(std::fs::metadata(&r).unwrap().permissions().mode() & 0o777, 0o775);
        assert!(!r.join("answers").exists());
        let _ = std::fs::remove_dir_all(&d);
    }
}
