//! `fence_runs.py`: what the resident gate trusts about a caller, and what
//! it never takes from a request (FN-R-21).
//!
//! The resident gate runs outside the fence and makes every write. So it
//! takes no path from a request: its root and captures root are fixed when
//! it starts. A resident call's session id must be plain. A checkpoint is
//! taken only inside the workspace of the fence run above the caller:
//! `daisugi fence`, still outside the fence, writes
//! `<root>/fence-runs/<pid>.json`, which no fenced process can write.
//!
//! A checkpoint never lets git choose the tree (FN-R-27): the tree is the
//! bound workspace, git is told GIT_DIR, GIT_COMMON_DIR and GIT_WORK_TREE,
//! and a .git that leads out of the workspace denies the call, except a
//! git worktree's gitfile whose link checks out by realpath (FN-R-34). A resident
//! transcript is read only when it is a regular file under
//! ~/.claude/projects of the password entry's home (FN-R-29).

use std::path::{Path, PathBuf};

use super::{statefile, statewrite};

pub const RUNS_DIR: &str = "fence-runs";
const MAX_DEPTH: usize = 64;

pub const SESSION_REFUSAL: &str =
    "the resident gate takes a session id only when it matches [A-Za-z0-9._-]{1,128}, so it denies this call";
pub const PATH_REFUSAL: &str = "the resident gate takes no path from a request: its root and captures root are \
     fixed when it starts. Drop --root and --captures-root from the request";
pub const NO_WORKSPACE_REFUSAL: &str = "the resident gate takes a checkpoint only inside the workspace a fence run \
     bound, and this caller runs under no fence run, so it denies this call";
/// FN-R-36: the unfenced server never runs git for a fenced session.
pub const FENCED_CHECKPOINT_REFUSAL: &str = "checkpoints are off inside the fence";
pub const OUTSIDE_REFUSAL: &str =
    "the checkpoint cwd is not inside the workspace this caller's fence run bound, so the resident gate denies this call";

pub const GITDIR_REFUSAL: &str = "the workspace's git dir is not a plain directory inside the workspace this caller's \
     fence run bound (a gitfile, a link, a common dir, or objects or refs that lead out), so the resident gate \
     denies this call";

/// The largest transcript a resident checkpoint reads.
pub const TRANSCRIPT_CAP: u64 = 128 * 1024 * 1024;

const PATH_FLAGS: [&str; 2] = ["--root", "--captures-root"];

/// `session_ok`: [A-Za-z0-9._-]{1,128}, not . or ..
pub fn session_ok(sid: &str) -> bool {
    !sid.is_empty()
        && sid.len() <= 128
        && sid != "."
        && sid != ".."
        && sid.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'.' || c == b'_' || c == b'-')
}

/// `names_a_path`: a word whose name part is --root or --captures-root, or
/// a prefix argparse would read as one.
pub fn names_a_path(argv: &[String]) -> bool {
    argv.iter().any(|w| {
        let name = w.split('=').next().unwrap_or("");
        name.len() >= 3 && name.starts_with("--") && PATH_FLAGS.iter().any(|f| f.starts_with(name))
    })
}

/// `strip_path_flags`: argv without --root and --captures-root and their
/// values.
pub fn strip_path_flags(argv: &[String]) -> Vec<String> {
    let mut out = Vec::with_capacity(argv.len());
    let mut skip = false;
    for w in argv {
        if skip {
            skip = false;
        } else if PATH_FLAGS.contains(&w.as_str()) {
            skip = true;
        } else if !w.starts_with("--root=") && !w.starts_with("--captures-root=") {
            out.push(w.clone());
        }
    }
    out
}

fn stat_fields(pid: i64) -> Option<Vec<String>> {
    let s = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let i = s.rfind(')')?;
    Some(s.get(i + 2..)?.split_whitespace().map(String::from).collect())
}

/// The start time /proc gives pid (field 22).
pub fn proc_start(pid: i64) -> Option<String> {
    stat_fields(pid)?.get(19).cloned()
}

fn parent(pid: i64) -> Option<i64> {
    stat_fields(pid)?.get(1)?.parse().ok()
}

/// `bound_workspace`: the workspace of the fence run the caller pid runs
/// under.
pub fn bound_workspace(root: &str, peer: Option<i64>) -> Option<String> {
    let mut pid = peer?;
    for _ in 0..MAX_DEPTH {
        if pid <= 1 {
            return None;
        }
        if let Some(Ok(raw)) = statefile::read_file(root, &[RUNS_DIR], &format!("{pid}.json")) {
            if let Ok(serde_json::Value::Object(o)) = serde_json::from_slice::<serde_json::Value>(&raw) {
                let ws = o.get("workspace").and_then(|v| v.as_str());
                if o.get("pid").and_then(|v| v.as_i64()) == Some(pid)
                    && o.get("start").and_then(|v| v.as_str()).map(String::from) == proc_start(pid)
                    && o.get("start").is_some_and(|v| v.is_string())
                    && ws.is_some_and(|w| w.starts_with('/'))
                {
                    return ws.map(String::from);
                }
            }
        }
        pid = parent(pid)?;
    }
    None
}

/// Writes the fence run record for pid: the fence calls it outside the
/// fence, before the ruleset. Records of pids that are gone are swept.
pub fn register(root: &str, pid: i64, workspace: &str) -> std::io::Result<()> {
    let ws = std::fs::canonicalize(workspace).unwrap_or_else(|_| PathBuf::from(workspace));
    sweep(root);
    let rec = serde_json::json!({"pid": pid, "start": proc_start(pid), "workspace": ws.to_string_lossy()});
    statewrite::write_file(root, &[RUNS_DIR], &format!("{pid}.json"), &rec.to_string(), true).map(|_| ())
}

fn sweep(root: &str) {
    let Ok(rd) = std::fs::read_dir(Path::new(root).join(RUNS_DIR)) else { return };
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        if let Some(pid) = name.strip_suffix(".json").and_then(|p| p.parse::<i64>().ok()) {
            if proc_start(pid).is_none() {
                let _ = statewrite::unlink_file(root, &[RUNS_DIR], &name);
            }
        }
    }
}

/// `os.path.realpath` for an absolute path: every link followed, and a part
/// that does not exist kept as it is written.
pub fn realpath(p: &str) -> String {
    if let Ok(r) = std::fs::canonicalize(p) {
        return r.to_string_lossy().into_owned();
    }
    let t = p.trim_end_matches('/');
    let Some(i) = t.rfind('/') else { return p.to_string() };
    let (dir, base) = (&t[..i], &t[i + 1..]);
    if dir.is_empty() {
        return format!("/{base}");
    }
    let rd = realpath(dir);
    match base {
        "" | "." => rd,
        ".." => Path::new(&rd).parent().map(|x| x.to_string_lossy().into_owned()).unwrap_or(rd),
        _ => format!("{}/{base}", rd.trim_end_matches('/')),
    }
}

/// `inside`: cwd, every link followed, is ws or below it.
pub fn inside(cwd: &str, ws: &str) -> bool {
    let real = realpath(cwd);
    let w = ws.trim_end_matches('/');
    w.is_empty() || real == w || real.starts_with(&format!("{w}/"))
}

fn lstat_kind(p: &str) -> Result<Option<std::fs::FileType>, ()> {
    match std::fs::symlink_metadata(p) {
        Ok(m) => Ok(Some(m.file_type())),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(_) => Err(()),
    }
}

/// `_read_link_file`: the first line of a small git link file (a gitfile,
/// gitdir, commondir), read with O_NOFOLLOW, or None.
fn read_link_file(path: &str) -> Option<String> {
    use std::io::Read;
    use std::os::unix::fs::OpenOptionsExt;
    let mut f = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
        .open(path)
        .ok()?;
    if !f.metadata().ok()?.file_type().is_file() {
        return None;
    }
    let mut buf = vec![0u8; 4097];
    let n = f.read(&mut buf).ok()?;
    if n > 4096 {
        return None;
    }
    let data = &buf[..n];
    if data.contains(&0) {
        return None;
    }
    let text = std::str::from_utf8(data).ok()?;
    let line = text.split('\n').next().unwrap_or("");
    let line = line.strip_suffix('\r').unwrap_or(line);
    let line = line.trim_end_matches([' ', '\t', '\n', '\r', '\x0b', '\x0c']);
    if line.is_empty() {
        None
    } else {
        Some(line.to_string())
    }
}

/// os.path.join(dir, p): p itself when it is absolute.
fn join_from(dir: &str, p: &str) -> String {
    if p.starts_with('/') {
        p.to_string()
    } else {
        format!("{}/{p}", dir.trim_end_matches('/'))
    }
}

fn dirname(p: &str) -> String {
    Path::new(p).parent().map(|x| x.to_string_lossy().into_owned()).unwrap_or_else(|| "/".into())
}

/// `_worktree_dirs`: the git dir and common dir of a git worktree whose
/// link checks out by realpath (FN-R-34), or None.
fn worktree_dirs(ws: &str, gitfile: &str) -> Option<(String, String)> {
    let line = read_link_file(gitfile)?;
    let named = line.strip_prefix("gitdir: ")?;
    if named.is_empty() {
        return None;
    }
    let admin = realpath(&join_from(ws, named));
    let parent = dirname(&admin);
    let common = dirname(&parent);
    if Path::new(&parent).file_name().map(|n| n != "worktrees").unwrap_or(true) || !Path::new(&admin).is_dir() {
        return None;
    }
    let back = read_link_file(&format!("{admin}/gitdir"))?;
    if realpath(&join_from(&admin, &back)) != realpath(gitfile) {
        return None;
    }
    let cd = read_link_file(&format!("{admin}/commondir"))?;
    if realpath(&join_from(&admin, &cd)) != common {
        return None;
    }
    Some((admin, common))
}

/// The git dir and common dir of a checkpoint (FN-R-34).
pub type GitDirs = (String, String);

/// `git_layout`: the refusal, or the git dir and common dir of a
/// checkpoint of ws. Ok(None) when ws has no .git: it gets no checkpoint.
/// A plain .git dir is both dirs; a gitfile is taken only for a git
/// worktree whose link checks out (FN-R-34).
pub fn git_layout(ws: &str) -> Result<Option<GitDirs>, &'static str> {
    if realpath(ws) != ws {
        return Err(GITDIR_REFUSAL);
    }
    let g = format!("{}/.git", ws.trim_end_matches('/'));
    let dirs = match lstat_kind(&g) {
        Ok(None) => return Ok(None),
        Ok(Some(t)) if t.is_dir() => (g.clone(), g.clone()),
        Ok(Some(t)) if t.is_file() => worktree_dirs(ws, &g).ok_or(GITDIR_REFUSAL)?,
        _ => return Err(GITDIR_REFUSAL),
    };
    let c = dirs.1.as_str();
    match lstat_kind(c) {
        Ok(Some(t)) if t.is_dir() => {}
        _ => return Err(GITDIR_REFUSAL),
    }
    if !matches!(lstat_kind(&format!("{c}/commondir")), Ok(None)) {
        return Err(GITDIR_REFUSAL);
    }
    for sub in ["objects", "refs", "refs/daisugi"] {
        match lstat_kind(&format!("{c}/{sub}")) {
            Ok(None) => {}
            Ok(Some(t)) if t.is_dir() => {}
            _ => return Err(GITDIR_REFUSAL),
        }
    }
    match std::fs::read_dir(format!("{c}/objects")) {
        Ok(rd) => {
            for e in rd {
                match e.and_then(|e| e.file_type()) {
                    Ok(t) if !t.is_symlink() => {}
                    _ => return Err(GITDIR_REFUSAL),
                }
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => return Err(GITDIR_REFUSAL),
    }
    fn walk(d: &Path) -> bool {
        let Ok(rd) = std::fs::read_dir(d) else { return true };
        for e in rd.flatten() {
            match e.file_type() {
                Ok(t) if t.is_symlink() => return false,
                Ok(t) if t.is_dir() => {
                    if !walk(&e.path()) {
                        return false;
                    }
                }
                _ => {}
            }
        }
        true
    }
    if !walk(Path::new(&format!("{c}/refs/daisugi"))) {
        return Err(GITDIR_REFUSAL);
    }
    Ok(Some(dirs))
}

/// `git_dir_refusal`: why a checkpoint of ws would write where git's own
/// layout does not lead, or None. None also when ws has no .git.
pub fn git_dir_refusal(ws: &str) -> Option<&'static str> {
    git_layout(ws).err()
}

thread_local! {
    /// A test points transcript_root at a fake home.
    static TRANSCRIPT_ROOT: std::cell::RefCell<Option<String>> = const { std::cell::RefCell::new(None) };
}

/// For tests: the projects dir transcript_root gives on this thread.
pub fn set_transcript_root(root: Option<String>) {
    TRANSCRIPT_ROOT.with(|r| *r.borrow_mut() = root);
}

/// `transcript_root`: ~/.claude/projects of the password entry's home,
/// never $HOME.
pub fn transcript_root() -> Option<String> {
    if let Some(r) = TRANSCRIPT_ROOT.with(|r| r.borrow().clone()) {
        return Some(r);
    }
    let home = super::paths::pw_home()?;
    Some(format!("{}/.claude/projects", home.trim_end_matches('/')))
}

/// `resident_transcript`: the bytes of the transcript a resident payload
/// names, or None. Only a regular file under transcript_root (realpath)
/// is read, opened O_NOFOLLOW|O_NONBLOCK.
pub fn resident_transcript(path: &str) -> Option<Vec<u8>> {
    use std::io::Read;
    use std::os::unix::fs::{FileTypeExt, OpenOptionsExt};
    if path.is_empty() || path.contains('\0') || !path.starts_with('/') {
        return None;
    }
    let base = realpath(&transcript_root()?);
    let real = realpath(path);
    if real == base || !inside(&real, &base) {
        return None;
    }
    let f = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
        .open(&real)
        .ok()?;
    let m = f.metadata().ok()?;
    let t = m.file_type();
    if !t.is_file() || t.is_fifo() || m.len() > TRANSCRIPT_CAP {
        return None;
    }
    let mut buf = Vec::new();
    f.take(TRANSCRIPT_CAP + 1).read_to_end(&mut buf).ok()?;
    if buf.len() as u64 > TRANSCRIPT_CAP {
        return None;
    }
    Some(buf)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_ids() {
        for s in ["a", "A.b_c-9", &"a".repeat(128)] {
            assert!(session_ok(s), "{s}");
        }
        for s in ["", ".", "..", "../x", "a/b", "a b", "é", &"a".repeat(129)] {
            assert!(!session_ok(s), "{s}");
        }
    }

    #[test]
    fn path_words() {
        for w in ["--root", "--root=/x", "--ro", "--r=/x", "--captures-root", "--cap", "--c"] {
            assert!(names_a_path(&[w.to_string()]), "{w}");
        }
        for w in ["--rootx", "--format", "-r", "--", "--session=--root"] {
            assert!(!names_a_path(&[w.to_string()]), "{w}");
        }
        let a: Vec<String> =
            ["--mode", "enforce", "--root", "/r", "--captures-root=/c", "--format", "pi"].iter().map(|s| s.to_string()).collect();
        assert_eq!(strip_path_flags(&a), ["--mode", "enforce", "--format", "pi"]);
    }

    #[test]
    fn the_walk_finds_the_parents_record() {
        let d = crate::gate::statefile::tests::dir();
        let root = d.join("gate").to_string_lossy().into_owned();
        let ws = d.join("ws");
        std::fs::create_dir_all(&ws).unwrap();
        let me = std::process::id() as i64;
        assert_eq!(bound_workspace(&root, Some(me)), None);
        let pp = parent(me).unwrap();
        register(&root, pp, ws.to_str().unwrap()).unwrap();
        let real = std::fs::canonicalize(&ws).unwrap().to_string_lossy().into_owned();
        assert_eq!(bound_workspace(&root, Some(me)), Some(real));
        let f = Path::new(&root).join(RUNS_DIR).join(format!("{pp}.json"));
        std::fs::write(&f, format!(r#"{{"pid":{pp},"start":"1","workspace":"/"}}"#)).unwrap();
        assert_eq!(bound_workspace(&root, Some(me)), None);
    }

    #[test]
    fn inside_follows_links() {
        let d = crate::gate::statefile::tests::dir();
        let base = std::fs::canonicalize(&d).unwrap();
        let ws = base.join("ws");
        let out = base.join("out");
        std::fs::create_dir_all(ws.join("sub")).unwrap();
        std::fs::create_dir_all(&out).unwrap();
        std::os::unix::fs::symlink(&out, ws.join("link")).unwrap();
        let w = ws.to_str().unwrap();
        let s = |p: PathBuf| p.to_string_lossy().into_owned();
        assert!(inside(w, w));
        assert!(inside(&s(ws.join("sub")), w));
        assert!(inside(&s(ws.join("missing/x")), w));
        assert!(!inside(&s(out.clone()), w));
        assert!(!inside(&s(ws.join("link")), w));
        assert!(!inside(&s(ws.join("link/missing")), w));
        assert!(!inside(&format!("{w}/sub/../../out"), w));
        assert!(!inside(&format!("{w}x"), w));
    }
}
