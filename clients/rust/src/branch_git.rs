//! `opendaisugi.branch_git`: read a branch worktree without trusting what
//! the child wrote in it. The child can rewrite the worktree's `.git`
//! pointer, so the parent takes the repository and the base commit from what
//! it recorded itself, and checks, before each git call, that the worktree
//! still belongs to that repository. The base is a full object id, never
//! read as an option.

use std::io::Write;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use crate::warden::py::text::{decode_utf8_replace, strip};

/// `tree.is_object_id`: 40 or 64 lower-case hex digits.
pub fn is_object_id(s: &str) -> bool {
    (s.len() == 40 || s.len() == 64) && s.bytes().all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}

/// What every git call the parent makes in a branch worktree, or in the tree
/// an adopt writes, passes first (`branch_git.GUARD`). The shared config and
/// the hooks of a repository are a place a branch child may reach, so a file
/// system monitor and a hooks directory the config names are switched off.
/// `/dev/null` holds no hook, so no hook is found.
///
/// It also ignores submodules: a nested repository the child made would be
/// asked for its status, and that status reads the nested config and runs the
/// filters the child wrote there.
pub const GUARD: [&str; 8] = [
    "-c",
    "core.fsmonitor=false",
    "-c",
    "core.hooksPath=/dev/null",
    "-c",
    "diff.ignoreSubmodules=all",
    "-c",
    "submodule.recurse=false",
];

/// The object store a fenced branch child writes: inside its own admin
/// directory. The parent never reads it and never writes it.
pub fn store(repo: &str, worktree: &str) -> String {
    let name = std::path::Path::new(worktree).file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    format!("{repo}/worktrees/{name}/objects")
}

/// Remove `path` without following a symbolic link at any level.
fn drop_path(path: &str) {
    match std::fs::symlink_metadata(path) {
        Ok(m) if m.file_type().is_dir() => {
            let _ = std::fs::remove_dir_all(path);
        }
        Ok(_) => {
            let _ = std::fs::remove_file(path);
        }
        Err(_) => {}
    }
}

/// `branch_git._drop`, for the parent's own clean up of a branch admin
/// directory.
pub fn drop_tree(path: &str) {
    drop_path(path);
}

/// `branch_git.reclaim`: give the parent back its rights on a branch admin
/// directory and all under it: owner rwx on each directory, owner rw on each
/// regular file. The child has the same uid and may have taken them away.
/// Each entry is read with lstat; a link is never followed and nothing
/// outside `admin` is changed. Call it only once the child and all its
/// processes are gone.
pub fn reclaim(admin: &str) {
    use std::os::unix::fs::PermissionsExt;
    let mut todo = vec![std::path::PathBuf::from(admin)];
    while let Some(path) = todo.pop() {
        let Ok(m) = std::fs::symlink_metadata(&path) else { continue };
        let mode = m.permissions().mode() & 0o7777;
        let want = if m.file_type().is_dir() {
            mode | 0o700
        } else if m.file_type().is_file() {
            mode | 0o600
        } else {
            continue;
        };
        if want != mode {
            let _ = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(want));
        }
        if m.file_type().is_dir() {
            if let Ok(rd) = std::fs::read_dir(&path) {
                for e in rd.flatten() {
                    todo.push(e.path());
                }
            }
        }
    }
}

/// `branch_git.fold`: fold a branch child's own object store away, once the
/// child and all its processes are gone and before the parent reads its
/// worktree. The worktree's HEAD goes back to `base`, its index to `base`'s
/// tree, and its reflogs, its own refs and its object store are deleted. The
/// child's commits then survive only as the files in the worktree, and the
/// diff against `base` reads those files. Without this, the operator's
/// repository holds a HEAD, an index and reflogs that name objects only the
/// child's store holds, so gc, fsck and log --all fail, and a parent write
/// skips an object the child store already has.
///
/// git runs on the recorded git directory, never through the worktree's
/// `.git` pointer, which the child can rewrite. A link the child made in its
/// admin directory is removed, never followed.
///
/// It first gives the parent back its rights on the admin directory, so a
/// child that took them cannot keep its logs, refs or store. True only when
/// the logs, the refs, any `reftable` directory and the store are gone, HEAD
/// names `base` and the index holds `base`'s tree.
pub fn fold(repo: &str, worktree: &str, base: &str) -> bool {
    if !is_object_id(base) || repo.is_empty() {
        return false;
    }
    let name = std::path::Path::new(worktree).file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    let admin = format!("{repo}/worktrees/{name}");
    if !std::fs::symlink_metadata(&admin).map(|m| m.file_type().is_dir()).unwrap_or(false) {
        return false;
    }
    reclaim(&admin);
    // A fenced run refuses a reftable repository before any worktree is
    // made, so a reftable directory here is one the child planted. A lock the
    // child put on its worktree would keep prune from removing it.
    for part in ["logs", "refs", "reftable", "locked", "HEAD.lock", "index.lock"] {
        drop_path(&format!("{admin}/{part}"));
    }
    for part in ["HEAD", "index"] {
        let p = format!("{admin}/{part}");
        if let Ok(m) = std::fs::symlink_metadata(&p) {
            if !m.file_type().is_file() {
                drop_path(&p);
            }
        }
    }
    let git_dir = format!("--git-dir={repo}");
    let head = format!("worktrees/{name}/HEAD");
    let index = format!("{admin}/index");
    let run = |args: &[&str]| -> bool {
        let mut cmd = command();
        cmd.args(["-c", "core.logAllRefUpdates=false", git_dir.as_str()])
            .args(args)
            .env_remove("GIT_OBJECT_DIRECTORY")
            .env_remove("GIT_ALTERNATE_OBJECT_DIRECTORIES")
            .env_remove("GIT_DIR")
            .env("GIT_INDEX_FILE", &index)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        cmd.status().map(|s| s.success()).unwrap_or(false)
    };
    if !run(&["update-ref", "--no-deref", &head, base]) || !run(&["read-tree", base]) {
        return false;
    }
    drop_path(&format!("{admin}/logs"));
    drop_path(&store(repo, worktree));
    for part in ["logs", "refs", "reftable", "objects"] {
        if std::fs::symlink_metadata(format!("{admin}/{part}")).is_ok() {
            return false;
        }
    }
    let head_file = format!("{admin}/HEAD");
    if !std::fs::symlink_metadata(&head_file).map(|m| m.file_type().is_file()).unwrap_or(false) {
        return false;
    }
    match std::fs::read(&head_file) {
        Ok(b) if String::from_utf8_lossy(&b).trim() == base => {}
        _ => return false,
    }
    if !std::fs::symlink_metadata(&index).map(|m| m.file_type().is_file()).unwrap_or(false) {
        return false;
    }
    run(&["diff-index", "--cached", "--quiet", base, "--"])
}

/// `branch_git.ref_format`: the ref format of the repository whose git
/// directory is `repo`, from its own config (`extensions.refStorage`; none
/// is `files`). Read before any child runs, from the operator's config,
/// never from a path a child can write.
pub fn ref_format(repo: &str) -> String {
    let out = command()
        .args(["config", "--file", &format!("{repo}/config"), "--get", "extensions.refStorage"])
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .output();
    match out {
        Ok(o) if o.status.success() => {
            let v = String::from_utf8_lossy(&o.stdout).trim().to_lowercase();
            if v.is_empty() { "files".into() } else { v }
        }
        Ok(o) if o.status.code() == Some(1) => "files".into(),
        _ => "unknown".into(),
    }
}

/// A `git` command with the guard on.
pub fn command() -> Command {
    let mut c = Command::new("git");
    c.args(GUARD);
    c
}

/// Run git; stdout, stderr and whether it exited 0. `limit` of zero is none.
pub fn git(stdin: Option<&[u8]>, limit: Duration, args: &[&str]) -> (Vec<u8>, Vec<u8>, bool) {
    let mut cmd = command();
    cmd.args(args)
        .stdin(if stdin.is_some() { Stdio::piped() } else { Stdio::null() })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let Ok(mut child) = cmd.spawn() else { return (vec![], vec![], false) };
    if let (Some(input), Some(mut si)) = (stdin, child.stdin.take()) {
        let input = input.to_vec();
        std::thread::spawn(move || {
            let _ = si.write_all(&input);
        });
    }
    let mut out = child.stdout.take();
    let mut err = child.stderr.take();
    let t_out = std::thread::spawn(move || {
        let mut b = Vec::new();
        if let Some(o) = out.as_mut() {
            let _ = std::io::Read::read_to_end(o, &mut b);
        }
        b
    });
    let t_err = std::thread::spawn(move || {
        let mut b = Vec::new();
        if let Some(o) = err.as_mut() {
            let _ = std::io::Read::read_to_end(o, &mut b);
        }
        b
    });
    let t0 = Instant::now();
    let ok = loop {
        match child.try_wait() {
            Ok(Some(st)) => break st.success(),
            Ok(None) => {
                if !limit.is_zero() && t0.elapsed() > limit {
                    let _ = child.kill();
                    let _ = child.wait();
                    break false;
                }
                std::thread::sleep(Duration::from_millis(2));
            }
            Err(_) => break false,
        }
    };
    (t_out.join().unwrap_or_default(), t_err.join().unwrap_or_default(), ok)
}

/// The absolute common git directory `path` resolves to now.
pub fn common_dir(path: &str) -> Option<String> {
    let (out, _, ok) = git(None, Duration::ZERO, &["-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir"]);
    if !ok {
        return None;
    }
    let s = strip(&decode_utf8_replace(&out)).to_string();
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

fn real(p: &str) -> std::path::PathBuf {
    std::fs::canonicalize(p).unwrap_or_else(|_| std::path::PathBuf::from(p))
}

/// True when `path` is the linked worktree git made for it in `repo`: its
/// `.git` is a regular file and its git directory is exactly
/// `<repo>/worktrees/<name>`. The common directory alone also admits the
/// repository's main git directory and a sibling's.
pub fn owns(path: &str, repo: &str) -> bool {
    if std::fs::symlink_metadata(path).map(|m| m.file_type().is_symlink()).unwrap_or(true) {
        return false;
    }
    let is_file = std::fs::symlink_metadata(format!("{path}/.git")).map(|m| m.file_type().is_file()).unwrap_or(false);
    if !is_file {
        return false;
    }
    // With extensions.worktreeConfig on, git reads this file, which the child
    // can write, and the -c guard cannot switch that off.
    let name = std::path::Path::new(path).file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    if std::fs::symlink_metadata(format!("{repo}/worktrees/{name}/config.worktree")).is_ok() {
        return false;
    }
    // A store of the child's own that was not folded away: the worktree's
    // HEAD and index may name objects only that store holds.
    if std::fs::symlink_metadata(store(repo, path)).is_ok() {
        return false;
    }
    let (out, _, ok) = git(None, Duration::ZERO, &["-C", path, "rev-parse", "--path-format=absolute", "--git-dir"]);
    if !ok {
        return false;
    }
    let found = strip(&decode_utf8_replace(&out)).to_string();
    !found.is_empty() && real(&found) == real(&format!("{repo}/worktrees/{name}")) && belongs(path, repo)
}

/// True when the work tree at `path` resolves to `repo`. For a tree the
/// owner named (an adopt target); a branch worktree needs `owns`.
pub fn belongs(path: &str, repo: &str) -> bool {
    if std::fs::symlink_metadata(path).map(|m| m.file_type().is_symlink()).unwrap_or(false) {
        return false;
    }
    match common_dir(path) {
        Some(found) => real(&found) == real(repo),
        None => false,
    }
}

/// The refusal text for a worktree that left its repository.
pub fn lost(session: &str, repo: &str) -> String {
    format!("the worktree of {session} no longer belongs to {repo}")
}

/// A base that is an object id with a non-empty repo.
pub fn usable(base: &str, repo: &str) -> bool {
    is_object_id(base) && !repo.is_empty()
}

/// `git add -A` (`--intent-to-add` when `intent`) of a branch worktree,
/// leaving out the nested repositories the index already holds as gitlinks.
/// An add refreshes each index entry, and refreshing a gitlink asks the nested
/// repository for its status, which reads its config and runs the filters the
/// child wrote there.
pub fn stage(worktree: &str, intent: bool) {
    let (listed, _, ok) = git(None, Duration::ZERO, &["-C", worktree, "ls-files", "-s", "-z"]);
    let mut excludes: Vec<String> = vec![];
    if ok {
        for rec in decode_utf8_replace(&listed).split('\0') {
            if rec.starts_with("160000 ") {
                if let Some((_, name)) = rec.split_once('\t') {
                    excludes.push(format!(":(exclude,literal){name}"));
                }
            }
        }
    }
    let mut args: Vec<&str> = vec!["-C", worktree, "add", "-A"];
    if intent {
        args.push("--intent-to-add");
    }
    args.push("--");
    args.push(".");
    args.extend(excludes.iter().map(|e| e.as_str()));
    git(None, Duration::ZERO, &args);
}

/// Mark new files so a diff shows them. Only after `owns`.
pub fn intent(worktree: &str) {
    stage(worktree, true);
}

/// The worktree's changes against `base`; None when the worktree does not
/// belong to `repo`, the base is not an object id, or git fails.
pub fn diff(worktree: &str, base: &str, repo: &str, extra: &[&str], mark_new: bool, limit: Duration) -> Option<Vec<u8>> {
    if !is_object_id(base) || !owns(worktree, repo) {
        return None;
    }
    if mark_new {
        intent(worktree);
    }
    let mut args: Vec<&str> = vec!["-C", worktree, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all"];
    args.extend_from_slice(extra);
    args.push(base);
    args.push("--");
    let (out, _, ok) = git(None, limit, &args);
    if ok {
        Some(out)
    } else {
        None
    }
}
