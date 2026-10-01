//! How the gate opens the files it keeps under its root (GR-R-1, GR-R-2).
//! The twin of `src/opendaisugi/_state_files.py` and of the Go package
//! `internal/statefile`; the three do the same thing and write the same
//! words.
//!
//! The gate root is opened as a directory without following a symlink at
//! its last component (the path down to the root may follow links), then
//! each name below it is opened by the fd of the directory above. A
//! directory must be this user's and writable by no group and no other
//! user; a file must be a regular file this user owns, with one link,
//! writable by no group and no other user. A refused file reads as absent:
//! DISARMED as not disarmed, an envelope as none, a marker as none. A
//! refusal that is not a plain "missing" leaves one stderr note, once per
//! file and reason in a process.

use std::collections::HashSet;
use std::ffi::CString;
use std::io::Read;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::ffi::OsStrExt;
use std::sync::Mutex;

use super::config::euid;
use super::paths::os_path;

const DIR_FLAGS: libc::c_int = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
const FILE_FLAGS: libc::c_int = libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC;

#[cfg(test)]
thread_local! {
    /// The notes this thread gave, for a test.
    static NOTES: std::cell::RefCell<Vec<String>> = const { std::cell::RefCell::new(Vec::new()) };
}

#[cfg(not(test))]
static NOTED: Mutex<Option<HashSet<String>>> = Mutex::new(None);

// Under test the set is per thread, like NOTES: tests run in parallel, and a
// reset in one must not clear the "given once" memory of another.
#[cfg(test)]
thread_local! {
    static NOTED_T: std::cell::RefCell<HashSet<String>> = std::cell::RefCell::new(HashSet::new());
}

/// True the first time this process (this thread, under test) sees `line`.
fn first_time(line: &str) -> bool {
    #[cfg(not(test))]
    {
        NOTED.lock().unwrap_or_else(|e| e.into_inner()).get_or_insert_with(HashSet::new).insert(line.to_string())
    }
    #[cfg(test)]
    {
        NOTED_T.with(|n| n.borrow_mut().insert(line.to_string()))
    }
}

/// Where a gate child keeps its notes until it has its answer. A hook
/// call is decided in a child whose stderr nobody reads, and the answer's
/// stderr is what the host sees, so the notes go in front of it (see
/// `hook::child`). None outside a child: a note goes to stderr at once.
static COLLECT: Mutex<Option<Vec<u8>>> = Mutex::new(None);

/// `_state_files.state_note`: `daisugi gate: not reading <refusal text>`
/// on stderr, once per text.
pub fn note(text: &str) {
    let line = format!("daisugi gate: not reading {text}");
    if !first_time(&line) {
        return;
    }
    #[cfg(test)]
    NOTES.with(|n| n.borrow_mut().push(line.clone()));
    let mut c = COLLECT.lock().unwrap_or_else(|e| e.into_inner());
    match c.as_mut() {
        Some(buf) => {
            buf.extend_from_slice(line.as_bytes());
            buf.push(b'\n');
        }
        None => eprintln!("{line}"),
    }
}

/// `_state_files.quote`: the path in single quotes, a quote inside it
/// written as quote, backslash, quote, quote. Always quoted, so the three
/// ports write the same bytes.
pub fn quote(path: &str) -> String {
    format!("'{}'", path.replace('\'', "'\\''"))
}

/// `_state_files.fix_for`: what the owner does about a refusal.
pub fn fix_for(why: &str, path: &str, is_dir: bool) -> String {
    let q = quote(path);
    match why {
        "is a symbolic link" => format!("remove the link {q}"),
        "is not a directory" | "is not a regular file" => format!("move {q} aside"),
        "is owned by another user" => format!("chown \"$(id -un)\" {q}"),
        "has more than one hard link" => format!("cp {q} {q}.new && mv {q}.new {q}"),
        "is writable by group or others" => format!("chmod {} {q}", if is_dir { "700" } else { "600" }),
        w if w.starts_with("is larger than ") => format!("shorten or remove {q}"),
        _ => format!("check that this user can read {q}"),
    }
}

/// `_state_files.refusal_text`: `<path>: <why> (fix: ...)`.
pub fn refusal_text(path: &str, why: &str, is_dir: bool) -> String {
    format!("{path}: {why} (fix: {})", fix_for(why, path, is_dir))
}

/// Leaves the note and returns the refusal text.
fn refuse(note_path: &str, why: &str, is_dir: bool) -> String {
    let text = refusal_text(note_path, why, is_dir);
    note(&text);
    text
}

/// Why an open failed: the path is plainly missing, or the gate refuses it
/// (the refusal text, `<path>: <why> (fix: ...)`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Miss {
    Missing,
    Refused(String),
}

impl Miss {
    /// The refusal text, or None for a path that is plainly missing.
    pub fn refusal(&self) -> Option<&str> {
        match self {
            Miss::Missing => None,
            Miss::Refused(t) => Some(t),
        }
    }
}

/// Keep the notes instead of printing them (a gate child).
pub fn collect_notes() {
    *COLLECT.lock().unwrap_or_else(|e| e.into_inner()) = Some(Vec::new());
}

/// The notes kept since `collect_notes`, and an end to keeping them.
pub fn take_notes() -> Vec<u8> {
    COLLECT.lock().unwrap_or_else(|e| e.into_inner()).take().unwrap_or_default()
}

/// Print the notes a child gave, each line once in this process (a
/// resident server's own stderr, as the Python server's notes are).
pub fn give_notes(notes: &[u8]) {
    for line in String::from_utf8_lossy(notes).lines() {
        if first_time(line) {
            eprintln!("{line}");
        }
    }
}

/// Forget which notes were given, for a test.
pub fn reset_notes() {
    #[cfg(not(test))]
    {
        *NOTED.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }
    #[cfg(test)]
    NOTED_T.with(|n| n.borrow_mut().clear());
}

/// `str(Path(root))`: empty and "." parts dropped, ".." kept.
pub fn norm(root: &str) -> String {
    let out = root.split('/').filter(|p| !p.is_empty() && *p != ".").collect::<Vec<_>>().join("/");
    if root.starts_with('/') {
        format!("/{out}")
    } else if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

/// The directory `<root>/<dirs>` as a note spells it.
pub fn note_dir(root: &str, dirs: &[&str]) -> String {
    let mut p = norm(root);
    for d in dirs {
        p.push('/');
        p.push_str(d);
    }
    p
}

fn open_at(dirfd: RawFd, name: &[u8], flags: libc::c_int) -> Result<OwnedFd, i32> {
    let c = CString::new(name).map_err(|_| libc::EINVAL)?;
    let fd = unsafe { libc::openat(dirfd, c.as_ptr(), flags) };
    if fd >= 0 {
        Ok(unsafe { OwnedFd::from_raw_fd(fd) })
    } else {
        Err(std::io::Error::last_os_error().raw_os_error().unwrap_or(libc::EIO))
    }
}

fn fstat(fd: &OwnedFd) -> Option<libc::stat> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    (unsafe { libc::fstat(fd.as_raw_fd(), &mut st) } == 0).then_some(st)
}

/// `_state_files._why_not_opened`: why an open of `name` failed; None when
/// it is plainly missing. `open_err` is the errno of that open: ENOENT with
/// no-follow flags means the name did not exist when it was opened, and that
/// stands, because a writer may rename a file into place a moment later and a
/// second look at the name would call a plain miss a refusal.
fn why_not(parent: RawFd, name: &[u8], want_dir: bool, open_err: i32) -> Option<&'static str> {
    if open_err == libc::ENOENT {
        return None;
    }
    match open_at(parent, name, libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC) {
        Err(e) if e == libc::ENOENT || e == libc::ENOTDIR => None,
        Err(_) => Some("cannot be opened"),
        Ok(fd) => match fstat(&fd) {
            None => Some("cannot be opened"),
            Some(st) if st.st_mode & libc::S_IFMT == libc::S_IFLNK => Some("is a symbolic link"),
            Some(st) if want_dir && st.st_mode & libc::S_IFMT != libc::S_IFDIR => Some("is not a directory"),
            Some(_) => Some("cannot be opened"),
        },
    }
}

/// `_state_files._open_dir`: an fd for the directory `name` under `parent`
/// (`AT_FDCWD` for the root's path), or why not.
fn open_dir(parent: RawFd, name: &[u8], note_path: &str) -> Result<OwnedFd, Miss> {
    let fd = match open_at(parent, name, DIR_FLAGS) {
        Ok(fd) => fd,
        Err(e) => {
            return Err(match why_not(parent, name, true, e) {
                Some(why) => Miss::Refused(refuse(note_path, why, true)),
                None => Miss::Missing,
            });
        }
    };
    let why = match fstat(&fd) {
        None => "cannot be opened",
        Some(st) if st.st_uid != euid() => "is owned by another user",
        Some(st) if st.st_mode & 0o022 != 0 => "is writable by group or others",
        Some(_) => return Ok(fd),
    };
    Err(Miss::Refused(refuse(note_path, why, true)))
}

/// `_state_files.probe_state_dirs`: the fds of the gate root and each
/// directory below it, in order, or why not (missing, or the refusal).
pub fn probe_dirs(root: &str, dirs: &[&str]) -> Result<Vec<OwnedFd>, Miss> {
    let base = norm(root);
    let path = os_path("open", &base).map_err(|_| Miss::Missing)?;
    let mut fds = vec![open_dir(libc::AT_FDCWD, path.as_os_str().as_bytes(), &base)?];
    let mut rel = base;
    for d in dirs {
        rel.push('/');
        rel.push_str(d);
        let last = fds.last().map(|f| f.as_raw_fd()).ok_or(Miss::Missing)?;
        let next = open_dir(last, d.as_bytes(), &rel)?;
        fds.push(next);
    }
    Ok(fds)
}

/// `probe_dirs` without the refusal (`_state_files.open_state_dirs`).
pub fn open_dirs(root: &str, dirs: &[&str]) -> Option<Vec<OwnedFd>> {
    probe_dirs(root, dirs).ok()
}

/// `_state_files.probe_file_in`: an fd for `name` in the directory `dirfd`
/// (called `note_dir` in a note), or why not. A refusal also leaves a note.
/// `max_bytes` is a size limit.
pub fn probe_file_in(dirfd: RawFd, note_dir: &str, name: &str, max_bytes: Option<u64>) -> Result<OwnedFd, Miss> {
    let note_path = format!("{note_dir}/{name}");
    let fd = match open_at(dirfd, name.as_bytes(), FILE_FLAGS) {
        Ok(fd) => fd,
        Err(e) => {
            return Err(match why_not(dirfd, name.as_bytes(), false, e) {
                Some(why) => Miss::Refused(refuse(&note_path, why, false)),
                None => Miss::Missing,
            });
        }
    };
    let why = match fstat(&fd) {
        None => "cannot be opened".to_string(),
        Some(st) if st.st_mode & libc::S_IFMT != libc::S_IFREG => "is not a regular file".into(),
        Some(st) if st.st_uid != euid() => "is owned by another user".into(),
        Some(st) if st.st_nlink != 1 => "has more than one hard link".into(),
        Some(st) if st.st_mode & 0o022 != 0 => "is writable by group or others".into(),
        Some(st) if max_bytes.is_some_and(|m| st.st_size as u64 > m) => {
            format!("is larger than {} bytes", max_bytes.unwrap_or(0))
        }
        Some(_) => return Ok(fd),
    };
    Err(Miss::Refused(refuse(&note_path, &why, false)))
}

/// `probe_file_in` without the refusal (`_state_files.open_file_in`).
pub fn open_file_in(dirfd: RawFd, note_dir: &str, name: &str, max_bytes: Option<u64>) -> Option<OwnedFd> {
    probe_file_in(dirfd, note_dir, name, max_bytes).ok()
}

/// `_state_files.probe_state_file`: an fd for `<root>/<dirs>/<name>`, or
/// why not.
pub fn probe(root: &str, dirs: &[&str], name: &str, max_bytes: Option<u64>) -> Result<OwnedFd, Miss> {
    let fds = probe_dirs(root, dirs)?;
    let last = fds.last().map(|f| f.as_raw_fd()).ok_or(Miss::Missing)?;
    probe_file_in(last, &note_dir(root, dirs), name, max_bytes)
}

/// `probe` without the refusal (`_state_files.open_state_file`).
pub fn open(root: &str, dirs: &[&str], name: &str, max_bytes: Option<u64>) -> Option<OwnedFd> {
    probe(root, dirs, name, max_bytes).ok()
}

/// The bytes of the file from its fd, to the end.
pub fn read_all(fd: OwnedFd) -> std::io::Result<Vec<u8>> {
    let mut buf = Vec::new();
    std::fs::File::from(fd).read_to_end(&mut buf)?;
    Ok(buf)
}

/// The bytes of `<root>/<dirs>/<name>` as `probe` opens it: Err(why not)
/// for a file that is missing or refused; Ok(Err) for a failed read of an
/// open file.
pub fn probe_read(root: &str, dirs: &[&str], name: &str) -> Result<std::io::Result<Vec<u8>>, Miss> {
    probe(root, dirs, name, None).map(read_all)
}

/// `probe_read` without the refusal. None when the file is missing or
/// refused; Some(Err) for a failed read of an open file.
pub fn read_file(root: &str, dirs: &[&str], name: &str) -> Option<std::io::Result<Vec<u8>>> {
    probe_read(root, dirs, name).ok()
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::os::unix::fs::{symlink, PermissionsExt};
    use std::path::{Path, PathBuf};

    // GR-R-1, GR-R-2: the gate reads its state through no link, and only
    // from directories and regular files this user owns that group and
    // other cannot write. Each test fails when the check is taken out.

    /// A fresh directory of the test's own, on real disk: under TMPDIR when
    /// the runner sets one (it is a disk path on this box, not /tmp), else
    /// under the crate's target directory. Never the system's /tmp, which is
    /// RAM here.
    pub(crate) fn dir() -> PathBuf {
        static N: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let n = N.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let base = match std::env::var_os("TMPDIR") {
            Some(t) if !t.is_empty() => PathBuf::from(t),
            _ => Path::new(env!("CARGO_MANIFEST_DIR")).join("target").join("test-tmp"),
        };
        let d = base.join(format!("dgs-{}-{n}", std::process::id()));
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    pub(crate) fn mode(p: &Path, m: u32) {
        std::fs::set_permissions(p, std::fs::Permissions::from_mode(m)).unwrap();
    }

    pub(crate) fn root() -> (PathBuf, String) {
        let r = dir().join("gate");
        std::fs::create_dir_all(r.join("envelopes")).unwrap();
        mode(&r, 0o700);
        mode(&r.join("envelopes"), 0o700);
        let s = r.to_str().unwrap().to_string();
        (r, s)
    }

    pub(crate) fn file(p: &Path, m: u32) {
        std::fs::write(p, "x").unwrap();
        mode(p, m);
    }

    fn opens(root: &str, dirs: &[&str], name: &str) -> bool {
        open(root, dirs, name, None).is_some()
    }

    /// The notes this thread gave, each less its `(fix: ...)` tail: these
    /// tests are about the reason. The tail is held in `statewrite`'s tests.
    pub(crate) fn notes() -> Vec<String> {
        NOTES.with(|n| {
            std::mem::take(&mut *n.borrow_mut())
                .into_iter()
                .map(|l| match l.find(" (fix: ") {
                    Some(i) => l[..i].to_string(),
                    None => l,
                })
                .collect()
        })
    }

    /// The notes this thread gave, whole.
    pub(crate) fn notes_raw() -> Vec<String> {
        NOTES.with(|n| std::mem::take(&mut *n.borrow_mut()))
    }

    // The flake behind the ask-channel deny test: the open saw no file, a
    // writer renamed one into place, and a second look at the name called the
    // plain miss a refusal. The errno of the open decides: ENOENT is a miss.
    #[test]
    fn enoent_from_the_open_stands_even_if_the_name_exists_later() {
        let (r, _s) = root();
        file(&r.join("x.json"), 0o600);
        let p = r.join("x.json");
        let b = p.to_str().unwrap().as_bytes();
        assert_eq!(why_not(libc::AT_FDCWD, b, false, libc::ENOENT), None);
        assert_eq!(why_not(libc::AT_FDCWD, b, false, libc::EACCES), Some("cannot be opened"));
    }

    #[test]
    fn a_plain_file_is_opened_and_silent() {
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o600);
        file(&r.join("envelopes/default.json"), 0o600);
        notes();
        assert!(opens(&s, &[], "DISARMED") && opens(&s, &["envelopes"], "default.json"));
        assert!(!opens(&s, &[], "missing"));
        assert!(!opens(&format!("{}/nothing", dir().display()), &[], "DISARMED"));
        assert!(notes().is_empty());
    }

    #[test]
    fn a_refusal_leaves_one_note_naming_the_file_and_the_reason() {
        let other = dir();
        file(&other.join("target"), 0o600);
        type Spoil = fn(&Path, &Path);
        let cases: [(&str, Spoil, &[&str], &str, &str, &str); 5] = [
            ("symlink", |p, o| symlink(o.join("target"), p).unwrap(), &[], "DISARMED", "DISARMED", "is a symbolic link"),
            (
                "hard link",
                |p, o| {
                    file(p, 0o600);
                    std::fs::hard_link(p, o.join("second")).unwrap();
                },
                &[],
                "DISARMED",
                "DISARMED",
                "has more than one hard link",
            ),
            ("group writable", |p, _| file(p, 0o620), &[], "DISARMED", "DISARMED", "is writable by group or others"),
            (
                "other writable",
                |p, _| file(p, 0o602),
                &["envelopes"],
                "default.json",
                "envelopes/default.json",
                "is writable by group or others",
            ),
            ("directory", |p, _| std::fs::create_dir(p).unwrap(), &[], "DISARMED", "DISARMED", "is not a regular file"),
        ];
        for (name, spoil, dirs, f, where_, why) in cases {
            let (r, s) = root();
            let mut p = r.clone();
            for d in dirs {
                p.push(d);
            }
            p.push(f);
            spoil(&p, &other);
            notes();
            assert!(!opens(&s, dirs, f), "{name}: opened");
            assert!(!opens(&s, dirs, f), "{name}: opened");
            assert_eq!(notes(), vec![format!("daisugi gate: not reading {s}/{where_}: {why}")], "{name}");
        }
    }

    #[test]
    fn a_directory_is_opened_through_no_link() {
        let (r, s) = root();
        file(&r.join("envelopes/default.json"), 0o600);
        let real = r.parent().unwrap().join("real-envelopes");
        std::fs::rename(r.join("envelopes"), &real).unwrap();
        symlink(&real, r.join("envelopes")).unwrap();
        notes();
        assert!(!opens(&s, &["envelopes"], "default.json"));
        assert_eq!(notes(), vec![format!("daisugi gate: not reading {s}/envelopes: is a symbolic link")]);
    }

    #[test]
    fn a_symlinked_root_is_refused_and_a_symlinked_parent_is_followed() {
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o600);
        let link = dir().join("link");
        symlink(&r, &link).unwrap();
        let l = link.to_str().unwrap().to_string();
        notes();
        assert!(!opens(&l, &[], "DISARMED"));
        assert_eq!(notes(), vec![format!("daisugi gate: not reading {l}: is a symbolic link")]);
        // A link ABOVE the root is followed (GR-R-2).
        let home = dir().join("home");
        symlink(r.parent().unwrap(), &home).unwrap();
        let via = format!("{}/gate", home.display());
        assert!(opens(&via, &[], "DISARMED") && opens(&s, &[], "DISARMED"));
        assert!(notes().is_empty());
    }

    #[test]
    fn a_root_others_can_write_is_refused() {
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o600);
        for m in [0o770, 0o707, 0o777] {
            mode(&r, m);
            notes();
            assert!(!opens(&s, &[], "DISARMED"), "mode {m:o}");
            assert_eq!(notes(), vec![format!("daisugi gate: not reading {s}: is writable by group or others")]);
            // The note is given once, so reset for the next mode.
            reset_notes();
        }
    }

    #[test]
    fn a_file_another_user_owns_is_refused() {
        let (r, s) = root();
        file(&r.join("DISARMED"), 0o600);
        assert!(opens(&s, &[], "DISARMED"));
        super::super::config::TEST_EUID.with(|c| c.set(Some(unsafe { libc::geteuid() } + 1)));
        notes();
        let got = opens(&s, &[], "DISARMED");
        super::super::config::TEST_EUID.with(|c| c.set(None));
        assert!(!got);
        // The root is the first thing refused, so it is the one named.
        assert_eq!(notes(), vec![format!("daisugi gate: not reading {s}: is owned by another user")]);
    }

    #[test]
    fn a_fifo_is_refused_without_blocking() {
        let (r, s) = root();
        let c = CString::new(r.join("DISARMED").as_os_str().as_bytes()).unwrap();
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o600) }, 0);
        notes();
        assert!(!opens(&s, &[], "DISARMED"));
        assert_eq!(notes(), vec![format!("daisugi gate: not reading {s}/DISARMED: is not a regular file")]);
    }
}
