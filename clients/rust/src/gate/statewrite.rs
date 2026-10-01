//! How the gate's operator tools write the files it keeps under its root
//! (GR-R-9). The twin of `_state_files.write_state_file` and of the Go
//! `statefile.WriteFile`; the three do the same thing and write the same
//! words.
//!
//! A writer works from a directory fd: the root and each directory below it
//! are opened without following a link, and the file is made by openat
//! (O_CREAT | O_EXCL | O_NOFOLLOW, 0600) under a temp name nobody can
//! predict, then renamed over its name inside the same directory. A link a
//! concurrent write planted at a name is never written through. The gate's
//! own files (checkpoint state, the ask channel) replace whatever stands at
//! their name (`replace_link`); the operator's files (DISARMED, an
//! envelope, a graft rule) refuse a link or a non-file there, so the owner
//! is told.

use std::ffi::CString;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::ffi::OsStrExt;

use super::config::euid;
use super::paths::os_path;
use super::statefile::{note_dir, norm, probe, probe_dirs, probe_file_in, refusal_text, Miss};

const DIR_FLAGS: libc::c_int = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;

/// A writer's refusal of a path the reader would refuse. Its text is one
/// line: `<path>: <reason> (fix: ...)`.
#[derive(Debug)]
pub struct WriteRefused(pub String);

impl std::fmt::Display for WriteRefused {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for WriteRefused {}

fn refused(text: String) -> io::Error {
    io::Error::other(WriteRefused(text))
}

/// The refusal text in an error a writer returned, or None for any other.
pub fn write_refused(e: &io::Error) -> Option<&str> {
    e.get_ref().and_then(|i| i.downcast_ref::<WriteRefused>()).map(|w| w.0.as_str())
}

fn cstr(b: &[u8]) -> io::Result<CString> {
    CString::new(b).map_err(|_| io::Error::from_raw_os_error(libc::EINVAL))
}

fn os_err() -> io::Error {
    io::Error::last_os_error()
}

fn openat(dirfd: RawFd, name: &[u8], flags: libc::c_int, mode: u32) -> io::Result<OwnedFd> {
    let c = cstr(name)?;
    let fd = unsafe { libc::openat(dirfd, c.as_ptr(), flags, mode as libc::c_uint) };
    if fd < 0 {
        Err(os_err())
    } else {
        Ok(unsafe { OwnedFd::from_raw_fd(fd) })
    }
}

fn fstat(fd: RawFd) -> Option<libc::stat> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    (unsafe { libc::fstat(fd, &mut st) } == 0).then_some(st)
}

/// Why `name` in `dirfd` is not a directory this module may open: the
/// reason words of `statefile`, or "cannot be opened".
fn why_dir(dirfd: RawFd, name: &[u8]) -> &'static str {
    match openat(dirfd, name, libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC, 0) {
        Err(_) => "cannot be opened",
        Ok(fd) => match fstat(fd.as_raw_fd()) {
            Some(st) if st.st_mode & libc::S_IFMT == libc::S_IFLNK => "is a symbolic link",
            Some(st) if st.st_mode & libc::S_IFMT != libc::S_IFDIR => "is not a directory",
            _ => "cannot be opened",
        },
    }
}

fn parent_of(base: &str) -> &str {
    match base.rfind('/') {
        None => ".",
        Some(0) => "/",
        Some(i) => &base[..i],
    }
}

/// `_state_files._make_dirs`: the fds of the root and each directory below
/// it, made 0700 when missing. A link at or below the root, or a directory
/// another user owns, is a refusal (see `write_refused`). So is a directory
/// that group or others can write, as the reader refuses it (GR-R-8), unless
/// `repair` is set, which is the operator's own command: it sets every
/// directory to 0700 and says so on stderr when the mode was loose. The
/// gate's own writers never repair and never chmod.
pub fn make_dirs(root: &str, dirs: &[&str], repair: bool) -> io::Result<Vec<OwnedFd>> {
    let base = norm(root);
    let path = os_path("open", &base).map_err(|_| io::Error::from_raw_os_error(libc::EINVAL))?;
    std::fs::create_dir_all(parent_of(&base))?;
    let mut fds: Vec<OwnedFd> = Vec::new();
    let mut shown = base.clone();
    for i in 0..=dirs.len() {
        let (parent, name): (RawFd, Vec<u8>) = if i == 0 {
            (libc::AT_FDCWD, path.as_os_str().as_bytes().to_vec())
        } else {
            shown.push('/');
            shown.push_str(dirs[i - 1]);
            (fds.last().map(|f| f.as_raw_fd()).unwrap_or(libc::AT_FDCWD), dirs[i - 1].as_bytes().to_vec())
        };
        let c = cstr(&name)?;
        if unsafe { libc::mkdirat(parent, c.as_ptr(), 0o700) } != 0 {
            let e = os_err();
            if e.raw_os_error() != Some(libc::EEXIST) {
                return Err(e);
            }
        }
        let fd = match openat(parent, &name, DIR_FLAGS, 0) {
            Ok(fd) => fd,
            // GR-R-18: ENOENT from the open stands (the entry went after the
            // mkdir); no second look at the name, as in Python and Go.
            Err(e) if e.raw_os_error() == Some(libc::ENOENT) => {
                return Err(refused(refusal_text(&shown, "cannot be opened", true)))
            }
            Err(_) => return Err(refused(refusal_text(&shown, why_dir(parent, &name), true))),
        };
        let st = fstat(fd.as_raw_fd()).ok_or_else(os_err)?;
        if st.st_uid != euid() {
            return Err(refused(refusal_text(&shown, "is owned by another user", true)));
        }
        let loose = st.st_mode & 0o022 != 0;
        if loose && !repair {
            return Err(refused(refusal_text(&shown, "is writable by group or others", true)));
        }
        if repair {
            if unsafe { libc::fchmod(fd.as_raw_fd(), 0o700) } != 0 {
                return Err(os_err());
            }
            if loose {
                eprintln!("daisugi gate: repaired {shown}: it was writable by group or others; now 700");
            }
        }
        fds.push(fd);
    }
    Ok(fds)
}

/// Says why the entry `name` in `dirfd` is not a regular file ("is a
/// symbolic link", "is not a regular file"), or None when it is one or is
/// not there. It does not follow a link.
fn non_regular(dirfd: RawFd, name: &str) -> Option<&'static str> {
    let fd = openat(dirfd, name.as_bytes(), libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC, 0).ok()?;
    let st = fstat(fd.as_raw_fd())?;
    match st.st_mode & libc::S_IFMT {
        libc::S_IFREG => None,
        libc::S_IFLNK => Some("is a symbolic link"),
        _ => Some("is not a regular file"),
    }
}

fn random_name() -> io::Result<String> {
    use std::io::Read;
    let mut b = [0u8; 6];
    std::fs::File::open("/dev/urandom")?.read_exact(&mut b)?;
    Ok(b.iter().map(|x| format!("{x:02x}")).collect())
}

/// `_state_files.write_state_file`: `text` written to `<root>/<dirs>/<name>`
/// (0600) through the directory fd. With `replace_link` a link or other
/// entry at the name is replaced; without it a link or a non-regular file
/// there is refused. Returns the path.
pub fn write_file(root: &str, dirs: &[&str], name: &str, text: &str, replace_link: bool) -> io::Result<String> {
    let fds = make_dirs(root, dirs, !replace_link)?;
    let dfd = fds.last().map(|f| f.as_raw_fd()).ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
    let path = format!("{}/{name}", note_dir(root, dirs));
    if !replace_link {
        if let Some(why) = non_regular(dfd, name) {
            return Err(refused(refusal_text(&path, why, false)));
        }
    }
    let flags = libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_NOFOLLOW | libc::O_CLOEXEC;
    let mut made: Option<(OwnedFd, String)> = None;
    for _ in 0..16 {
        let tmp = format!(".{name}.{}.tmp", random_name()?);
        match openat(dfd, tmp.as_bytes(), flags, 0o600) {
            Ok(fd) => {
                made = Some((fd, tmp));
                break;
            }
            Err(e) if e.raw_os_error() == Some(libc::EEXIST) => continue,
            Err(e) => return Err(e),
        }
    }
    let Some((fd, tmp)) = made else {
        return Err(refused(refusal_text(&path, "cannot be opened", false)));
    };
    let res = (|| {
        if unsafe { libc::fchmod(fd.as_raw_fd(), 0o600) } != 0 {
            return Err(os_err());
        }
        let mut f = std::fs::File::from(fd);
        io::Write::write_all(&mut f, text.as_bytes())?;
        drop(f);
        let (t, n) = (cstr(tmp.as_bytes())?, cstr(name.as_bytes())?);
        if unsafe { libc::renameat(dfd, t.as_ptr(), dfd, n.as_ptr()) } != 0 {
            return Err(os_err());
        }
        Ok(())
    })();
    if let Err(e) = res {
        if let Ok(t) = cstr(tmp.as_bytes()) {
            unsafe { libc::unlinkat(dfd, t.as_ptr(), 0) };
        }
        return Err(e);
    }
    Ok(path)
}

/// `_state_files._open_for_unlink`: the directory fds for a removal. A
/// missing directory is quiet (Ok(None)); a link or a directory another
/// user owns is a refusal text (Err).
fn open_for_unlink(root: &str, dirs: &[&str]) -> Result<Option<Vec<OwnedFd>>, String> {
    let base = norm(root);
    let Ok(path) = os_path("open", &base) else {
        return Ok(None);
    };
    let mut fds: Vec<OwnedFd> = Vec::new();
    let mut shown = base;
    for i in 0..=dirs.len() {
        let (parent, name): (RawFd, Vec<u8>) = if i == 0 {
            (libc::AT_FDCWD, path.as_os_str().as_bytes().to_vec())
        } else {
            shown.push('/');
            shown.push_str(dirs[i - 1]);
            (fds.last().map(|f| f.as_raw_fd()).unwrap_or(libc::AT_FDCWD), dirs[i - 1].as_bytes().to_vec())
        };
        match openat(parent, &name, DIR_FLAGS, 0) {
            Ok(fd) => {
                if fstat(fd.as_raw_fd()).is_some_and(|st| st.st_uid == euid()) {
                    fds.push(fd);
                } else {
                    return Err(refusal_text(&shown, "is owned by another user", true));
                }
            }
            Err(e) => return unlink_dir_miss(parent, &name, &shown, e.raw_os_error()).map(|()| None),
        }
    }
    Ok(Some(fds))
}

/// Why a directory open for a removal failed: `Ok(())` for a plain miss,
/// a refusal text otherwise. GR-R-18: ENOENT from the open is a miss and
/// stands, with no second look at the name (a writer may have renamed
/// something into place a moment later).
fn unlink_dir_miss(parent: RawFd, name: &[u8], shown: &str, errno: Option<i32>) -> Result<(), String> {
    if errno == Some(libc::ENOENT) {
        return Ok(());
    }
    // O_DIRECTORY | O_NOFOLLOW fails with ENOTDIR for a link too, so the
    // errno does not say "missing". Look at the entry itself (no follow):
    // nothing there is quiet, a link or a file is a refusal.
    match openat(parent, name, libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC, 0) {
        Err(e) if matches!(e.raw_os_error(), Some(libc::ENOENT) | Some(libc::ENOTDIR)) => Ok(()),
        _ => Err(refusal_text(shown, why_dir(parent, name), true)),
    }
}

/// `_state_files.unlink_state_file`: `<root>/<dirs>/<name>` removed through
/// the directory fd; true when a file was. A missing file, directory or root
/// is no error; a refused directory is a refusal. A link at the name is
/// removed as the link it is.
pub fn unlink_file(root: &str, dirs: &[&str], name: &str) -> io::Result<bool> {
    let fds = match open_for_unlink(root, dirs) {
        Err(text) => return Err(refused(text)),
        Ok(None) => return Ok(false),
        Ok(Some(f)) => f,
    };
    let dfd = fds.last().map(|f| f.as_raw_fd()).ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
    let c = cstr(name.as_bytes())?;
    if unsafe { libc::unlinkat(dfd, c.as_ptr(), 0) } == 0 {
        return Ok(true);
    }
    let e = os_err();
    if e.raw_os_error() == Some(libc::ENOENT) {
        Ok(false)
    } else {
        Err(e)
    }
}

/// `_state_files.verify_written`: a file the operator just wrote, read back
/// with the gate's reader. A refusal is an error (see `write_refused`), so a
/// writer never claims success for a file the gate would not read.
pub fn verify_written(root: &str, dirs: &[&str], name: &str) -> io::Result<()> {
    match probe(root, dirs, name, None) {
        Ok(_) => Ok(()),
        Err(Miss::Refused(t)) => Err(refused(t)),
        Err(Miss::Missing) => {
            Err(refused(refusal_text(&format!("{}/{name}", note_dir(root, dirs)), "cannot be opened", false)))
        }
    }
}

/// The sorted names ending in `.json` in the directory `dirfd` (dotfiles
/// and directories included, as `Path.glob("*.json")`).
pub fn list_json(dirfd: RawFd) -> Vec<String> {
    let Ok(rd) = std::fs::read_dir(format!("/proc/self/fd/{dirfd}")) else {
        return vec![];
    };
    let mut names: Vec<String> = rd
        .filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().into_string().ok())
        .filter(|n| n.ends_with(".json"))
        .collect();
    names.sort();
    names
}

/// `_state_files.refused_paths`: every path under the gate root the gate
/// would refuse to read, each as `<path>: <reason> (fix: ...)`: the root;
/// then, if it opens, DISARMED, and each of `envelopes/`, `grafts/` and
/// `checkpoint-state/` with every `*.json` file in it. A refused directory
/// hides what is below it. What is missing is not listed.
pub fn refused_paths(root: &str) -> Vec<String> {
    let mut out = Vec::new();
    match probe_dirs(root, &[]) {
        Err(Miss::Refused(t)) => return vec![t],
        Err(Miss::Missing) => return out,
        Ok(_) => {}
    }
    if let Err(Miss::Refused(t)) = probe(root, &[], "DISARMED", None) {
        out.push(t);
    }
    for sub in ["envelopes", "grafts", "checkpoint-state"] {
        let fds = match probe_dirs(root, &[sub]) {
            Ok(f) => f,
            Err(Miss::Refused(t)) => {
                out.push(t);
                continue;
            }
            Err(Miss::Missing) => continue,
        };
        let Some(dir) = fds.last().map(|f| f.as_raw_fd()) else {
            continue;
        };
        for name in list_json(dir) {
            if let Err(Miss::Refused(t)) = probe_file_in(dir, &note_dir(root, &[sub]), &name, None) {
                out.push(t);
            }
        }
    }
    out
}

/// The tail of `gate._mkdir_private`: the directory at `path` set to 0700
/// on its own fd, opened without following a link, and only when this user
/// owns it. A link at `path` is left alone, so a link planted there is
/// never chmod-ed through.
pub fn chmod_private(path: &str) {
    let Ok(c) = cstr(path.as_bytes()) else {
        return;
    };
    let fd = unsafe { libc::open(c.as_ptr(), DIR_FLAGS) };
    if fd < 0 {
        return;
    }
    let fd = unsafe { OwnedFd::from_raw_fd(fd) };
    if fstat(fd.as_raw_fd()).is_some_and(|st| st.st_uid == euid()) {
        unsafe { libc::fchmod(fd.as_raw_fd(), 0o700) };
    }
}

#[cfg(test)]
mod tests {
    use super::super::statefile::tests::{dir, file, mode, notes_raw, root};
    use super::super::statefile::{probe, quote, reset_notes, Miss};
    use super::*;
    use std::os::unix::fs::{symlink, PermissionsExt};

    // GR-R-7 to GR-R-10: a refusal is a third answer beside opened and
    // missing; the note ends with the fix; a writer works from a directory
    // fd, so a link planted at a name is replaced (the gate's own files) or
    // refused (the operator's), never written through. Python twin:
    // tests/test_gate_state_writers.py.

    fn refusal(r: Result<OwnedFd, Miss>) -> Option<String> {
        match r {
            Err(Miss::Refused(t)) => Some(t),
            _ => None,
        }
    }

    #[test]
    fn the_note_ends_with_the_fix() {
        reset_notes();
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o666);
        notes_raw();
        let got = refusal(probe(&s, &[], "DISARMED", None)).unwrap();
        let want = format!("{s}/DISARMED: is writable by group or others (fix: chmod 600 '{s}/DISARMED')");
        assert_eq!(got, want);
        assert_eq!(notes_raw(), vec![format!("daisugi gate: not reading {want}")]);
    }

    #[test]
    fn the_fix_for_a_directory_and_a_link() {
        reset_notes();
        let (r, s) = root();
        mode(&r, 0o775);
        let got = refusal(probe(&s, &[], "DISARMED", None)).unwrap();
        assert_eq!(got, format!("{s}: is writable by group or others (fix: chmod 700 '{s}')"));
        mode(&r, 0o700);
        std::fs::remove_dir(r.join("envelopes")).unwrap();
        symlink(dir(), r.join("envelopes")).unwrap();
        let got = refusal(probe(&s, &["envelopes"], "default.json", None)).unwrap();
        assert_eq!(got, format!("{s}/envelopes: is a symbolic link (fix: remove the link '{s}/envelopes')"));
    }

    #[test]
    fn a_quote_in_a_path_is_quoted_like_the_python_one() {
        assert_eq!(quote("/a/it's"), "'/a/it'\\''s'");
        reset_notes();
        let r = dir().join("it's");
        std::fs::create_dir(&r).unwrap();
        mode(&r, 0o775);
        let got = refusal(probe(r.to_str().unwrap(), &[], "DISARMED", None)).unwrap();
        assert!(got.ends_with(&format!("(fix: chmod 700 '{}/it'\\''s')", r.parent().unwrap().display())), "{got}");
    }

    // GR-R-18 (N4): ENOENT from the open stands even if the name exists a
    // moment later, for the unlink path and for make_dirs.
    #[test]
    fn enoent_from_the_open_is_a_miss_even_if_the_name_exists_later() {
        let (r, s) = root();
        let d = r.join("envelopes");
        let name = b"envelopes".to_vec();
        let rfd = openat(libc::AT_FDCWD, r.to_str().unwrap().as_bytes(), DIR_FLAGS, 0).unwrap();
        assert!(d.is_dir());
        assert_eq!(unlink_dir_miss(rfd.as_raw_fd(), &name, &s, Some(libc::ENOENT)), Ok(()));
        // Any other errno with a plain directory there is a refusal.
        assert!(unlink_dir_miss(rfd.as_raw_fd(), &name, &s, Some(libc::EACCES)).is_err());
    }

    #[test]
    fn missing_is_not_refused() {
        reset_notes();
        let (_, s) = root();
        assert!(matches!(probe(&s, &[], "DISARMED", None), Err(Miss::Missing)));
        assert!(matches!(probe(&s, &["grafts"], "a.json", None), Err(Miss::Missing)));
    }

    #[test]
    fn the_gates_own_writer_replaces_a_link_and_writes_nothing_through_it() {
        reset_notes();
        let (r, s) = root();
        let victim = dir().join("victim");
        std::fs::write(&victim, "keep").unwrap();
        let d = r.join("checkpoint-state");
        std::fs::create_dir(&d).unwrap();
        mode(&d, 0o700);
        symlink(&victim, d.join("s1.json")).unwrap();
        for n in 0..8 {
            symlink(&victim, d.join(format!(".s1.json.{n}.tmp"))).unwrap();
        }
        write_file(&s, &["checkpoint-state"], "s1.json", "{\"prompt\": \"p\"}", true).unwrap();
        assert_eq!(std::fs::read_to_string(&victim).unwrap(), "keep");
        let m = std::fs::symlink_metadata(d.join("s1.json")).unwrap();
        assert!(m.file_type().is_file() && m.permissions().mode() & 0o777 == 0o600);
        assert_eq!(std::fs::read_to_string(d.join("s1.json")).unwrap(), "{\"prompt\": \"p\"}");
    }

    #[test]
    fn the_gates_own_writer_refuses_a_group_writable_root_and_keeps_its_mode() {
        let (r, s) = root();
        mode(&r, 0o775);
        let e = write_file(&s, &["checkpoint-state"], "s1.json", "{}", true).unwrap_err();
        assert_eq!(write_refused(&e), Some(format!("{s}: is writable by group or others (fix: chmod 700 '{s}')").as_str()));
        assert_eq!(std::fs::metadata(&r).unwrap().permissions().mode() & 0o777, 0o775);
        assert!(!r.join("checkpoint-state").exists());
    }

    #[test]
    fn the_gates_own_writer_refuses_a_group_writable_subdirectory() {
        let (r, s) = root();
        std::fs::create_dir(r.join("asks")).unwrap();
        mode(&r.join("asks"), 0o770);
        let e = write_file(&s, &["asks"], "a.json", "{}", true).unwrap_err();
        assert!(write_refused(&e).is_some());
        assert_eq!(std::fs::metadata(r.join("asks")).unwrap().permissions().mode() & 0o777, 0o770);
    }

    #[test]
    fn an_operator_writer_repairs_the_mode() {
        let (r, s) = root();
        mode(&r, 0o775);
        write_file(&s, &[], "DISARMED", "x\n", false).unwrap();
        assert_eq!(std::fs::metadata(&r).unwrap().permissions().mode() & 0o777, 0o700);
    }

    #[test]
    fn an_operator_writer_refuses_a_link_at_the_name() {
        reset_notes();
        let (r, s) = root();
        let victim = dir().join("victim");
        std::fs::write(&victim, "keep").unwrap();
        symlink(&victim, r.join("DISARMED")).unwrap();
        let e = write_file(&s, &[], "DISARMED", "x\n", false).unwrap_err();
        assert_eq!(write_refused(&e), Some(format!("{s}/DISARMED: is a symbolic link (fix: remove the link '{s}/DISARMED')").as_str()));
        assert_eq!(std::fs::read_to_string(&victim).unwrap(), "keep");
    }

    #[test]
    fn a_writer_refuses_a_symlinked_directory() {
        reset_notes();
        let (r, s) = root();
        std::fs::remove_dir(r.join("envelopes")).unwrap();
        let elsewhere = dir();
        symlink(&elsewhere, r.join("envelopes")).unwrap();
        let e = write_file(&s, &["envelopes"], "default.json", "{}", false).unwrap_err();
        assert!(write_refused(&e).is_some());
        assert!(!elsewhere.join("default.json").exists());
    }

    #[test]
    fn a_writer_makes_things_private_whatever_the_umask() {
        reset_notes();
        let old = unsafe { libc::umask(0o002) };
        let r = dir().join("home").join("gate");
        let s = r.to_str().unwrap().to_string();
        let a = write_file(&s, &["envelopes"], "default.json", "{}", false);
        mode(&r.join("envelopes"), 0o775);
        let b = write_file(&s, &["envelopes"], "default.json", "{}", false);
        unsafe { libc::umask(old) };
        a.unwrap();
        b.unwrap();
        let m = |p: &std::path::Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!((m(&r), m(&r.join("envelopes")), m(&r.join("envelopes/default.json"))), (0o700, 0o700, 0o600));
    }

    #[test]
    fn verify_written_raises_for_a_refused_file() {
        reset_notes();
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o666);
        assert!(verify_written(&s, &[], "DISARMED").is_err());
        mode(&r.join("DISARMED"), 0o600);
        assert!(verify_written(&s, &[], "DISARMED").is_ok());
    }

    #[test]
    fn unlink_reports_whether_a_file_was_removed_and_removes_a_link_as_a_link() {
        reset_notes();
        let (r, s) = root();
        assert!(!unlink_file(&s, &["envelopes"], "gone.json").unwrap());
        let victim = dir().join("victim");
        std::fs::write(&victim, "keep").unwrap();
        symlink(&victim, r.join("envelopes/l.json")).unwrap();
        assert!(unlink_file(&s, &["envelopes"], "l.json").unwrap());
        assert!(victim.exists());
        assert!(!unlink_file(&format!("{s}/nothing"), &["envelopes"], "x.json").unwrap());
    }

    #[test]
    fn unlink_through_a_symlinked_root_is_a_refusal_and_removes_nothing() {
        let (r, _) = root();
        let real = dir();
        std::fs::write(real.join("DISARMED"), "x").unwrap();
        let link = dir().join("gate");
        symlink(&real, &link).unwrap();
        let e = unlink_file(link.to_str().unwrap(), &[], "DISARMED").unwrap_err();
        assert!(write_refused(&e).is_some_and(|t| t.contains("is a symbolic link")), "{e}");
        assert!(real.join("DISARMED").exists());
        assert!(r.exists());
    }

    #[test]
    fn refused_paths_lists_each_refused_file() {
        reset_notes();
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o666);
        file(&r.join("envelopes/default.json"), 0o600);
        file(&r.join("envelopes/s1.json"), 0o664);
        let got = refused_paths(&s);
        assert_eq!(
            got,
            vec![
                format!("{s}/DISARMED: is writable by group or others (fix: chmod 600 '{s}/DISARMED')"),
                format!("{s}/envelopes/s1.json: is writable by group or others (fix: chmod 600 '{s}/envelopes/s1.json')"),
            ]
        );
        mode(&r, 0o775);
        let got = refused_paths(&s);
        assert_eq!(got.len(), 1);
        assert!(got[0].starts_with(&format!("{s}: is writable")));
    }
}
