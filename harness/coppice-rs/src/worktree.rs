//! One git worktree per task, beside the repo, under
//! `<repo>-worktrees/<name>`. Every git command runs with `git -C DIR`, so
//! this module never changes the process directory.

use std::path::Path;
use std::process::{Command, Stdio};

/// The refusal for a task name outside [a-z0-9-].
pub const ERR_BAD_NAME: &str = "task names use lowercase letters, digits, and dashes";

/// The refusal for a repo with no current branch.
pub const ERR_DETACHED: &str = "the repo is not on a branch. Check out one first.";

/// Whether name can be a worktree and a branch name.
pub fn valid_name(name: &str) -> Result<(), String> {
    if !name.is_empty()
        && name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
    {
        Ok(())
    } else {
        Err(ERR_BAD_NAME.into())
    }
}

/// Where the worktree for name lives, beside the repo. The repo path is
/// cleaned, never resolved through symlinks.
pub fn path(repo: &str, name: &str) -> String {
    let base = format!("{}-worktrees", crate::sys::clean(repo));
    crate::sys::clean(&format!("{base}/{name}"))
}

/// Go's text for a process that failed with nothing on stderr.
pub(crate) fn status_text(st: std::process::ExitStatus) -> String {
    use std::os::unix::process::ExitStatusExt;
    match (st.code(), st.signal()) {
        (Some(c), _) => format!("exit status {c}"),
        (None, Some(sig)) => format!("signal: {}", signal_name(sig)),
        _ => "exit status -1".into(),
    }
}

/// The signal names Go's os.ProcessState prints, for the ones a git run
/// can meet.
fn signal_name(sig: i32) -> String {
    match sig {
        libc::SIGHUP => "hangup".into(),
        libc::SIGINT => "interrupt".into(),
        libc::SIGQUIT => "quit".into(),
        libc::SIGABRT => "aborted".into(),
        libc::SIGKILL => "killed".into(),
        libc::SIGSEGV => "segmentation fault".into(),
        libc::SIGPIPE => "broken pipe".into(),
        libc::SIGTERM => "terminated".into(),
        n => format!("signal {n}"),
    }
}

/// Runs one git command in dir and returns trimmed stdout. A failure
/// carries git's stderr.
fn git(dir: &str, args: &[&str]) -> Result<String, String> {
    let mut cmd = Command::new("git");
    cmd.arg("-C").arg(dir).args(args).stdin(Stdio::null());
    // Under test, git reads no system or user config and no inherited
    // GIT_* variable. This is set on the command, never on the process.
    #[cfg(test)]
    {
        for (k, _) in std::env::vars_os() {
            if k.to_string_lossy().starts_with("GIT_") {
                cmd.env_remove(k);
            }
        }
        cmd.env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null");
    }
    let out = cmd.output();
    let first = args.first().copied().unwrap_or("");
    let out = match out {
        Ok(o) => o,
        Err(e) => {
            let msg = if e.kind() == std::io::ErrorKind::NotFound {
                "exec: \"git\": executable file not found in $PATH".to_string()
            } else {
                e.to_string()
            };
            return Err(format!("git {first}: {msg}"));
        }
    };
    if !out.status.success() {
        let mut msg = String::from_utf8_lossy(&out.stderr).trim().to_string();
        if msg.is_empty() {
            msg = status_text(out.status);
        }
        return Err(format!("git {first}: {msg}"));
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// The branch dir is on. A detached HEAD is ERR_DETACHED.
fn current_branch(dir: &str) -> Result<String, String> {
    let b = git(dir, &["rev-parse", "--abbrev-ref", "HEAD"])?;
    if b == "HEAD" {
        return Err(ERR_DETACHED.into());
    }
    Ok(b)
}

/// Creates a worktree on a new branch called name at path(repo, name),
/// with its upstream set to the branch the repo is on. When the upstream
/// cannot be set, the worktree and the branch go again.
pub fn add(repo: &str, name: &str) -> Result<String, String> {
    valid_name(name)?;
    let base = current_branch(repo)?;
    let p = path(repo, name);
    git(repo, &["worktree", "add", "-b", name, &p])?;
    if let Err(e) = git(&p, &["branch", &format!("--set-upstream-to={base}")]) {
        let _ = git(repo, &["worktree", "remove", "--force", &p]);
        let _ = git(repo, &["branch", "-D", name]);
        return Err(e);
    }
    Ok(p)
}

/// Deletes the worktree for name, unless keep. Git refuses a worktree with
/// uncommitted changes, and that refusal comes back as is. The branch
/// stays.
pub fn remove(repo: &str, name: &str, keep: bool) -> Result<(), String> {
    if keep {
        return Ok(());
    }
    remove_at(repo, &path(repo, name))
}

/// Deletes the worktree checked out at p, one of repo's worktrees. A caller
/// that recorded the path add returned passes it here: a worktree added
/// from inside another worktree sits beside that worktree, not beside the
/// main repo, so path of the main repo does not name it. Git's refusal of a
/// dirty worktree comes back as is. The branch stays.
pub fn remove_at(repo: &str, p: &str) -> Result<(), String> {
    git(repo, &["worktree", "remove", p]).map(|_| ())
}

/// How far the worktree's branch is ahead of and behind its upstream, and
/// the branch. A worktree with no upstream is an error.
pub fn status(p: &str) -> Result<(i64, i64, String), String> {
    let branch = current_branch(p)?;
    let out = git(
        p,
        &["rev-list", "--left-right", "--count", "HEAD...@{upstream}"],
    )?;
    let bad = || {
        format!(
            "git rev-list: unexpected output {}",
            crate::sys::go_quote(&out)
        )
    };
    let fields: Vec<&str> = out.split_whitespace().collect();
    if fields.len() != 2 {
        return Err(bad());
    }
    let ahead = go_atoi(fields[0]).ok_or_else(bad)?;
    let behind = go_atoi(fields[1]).ok_or_else(bad)?;
    Ok((ahead, behind, branch))
}

/// strconv.Atoi: an optional sign, then decimal digits.
fn go_atoi(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

/// Whether the worktree has uncommitted or untracked changes.
pub fn dirty(p: &str) -> Result<bool, String> {
    Ok(!git(p, &["status", "--porcelain"])?.is_empty())
}

/// The root of the repo that holds cwd. A cwd outside any repo is an error.
pub fn toplevel(cwd: &str) -> Result<String, String> {
    git(cwd, &["rev-parse", "--show-toplevel"])
}

/// The main repo of the worktree at p: the directory that holds the shared
/// .git.
pub fn repo(p: &str) -> Result<String, String> {
    let common = git(
        p,
        &["rev-parse", "--path-format=absolute", "--git-common-dir"],
    )?;
    Ok(Path::new(&common)
        .parent()
        .map(|d| d.to_string_lossy().into_owned())
        .filter(|d| !d.is_empty())
        .unwrap_or_else(|| ".".into()))
}

#[cfg(test)]
mod tests {
    //! Each test makes its own repo under a fresh directory. The helper
    //! run() gives git a cleared environment; the module's own git drops
    //! every inherited GIT_* variable and reads no system or user config
    //! under test. No test changes the process directory, the environment
    //! or the umask.
    use super::*;
    use std::path::PathBuf;

    /// A scratch directory under TMPDIR, which the test run sets.
    fn scratch(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("worktree-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(d.join("home")).unwrap();
        d
    }

    /// Runs git in dir with only the variables a scratch repo needs.
    fn run(root: &Path, dir: &Path, args: &[&str]) -> String {
        let out = Command::new("/usr/bin/git")
            .arg("-C")
            .arg(dir)
            .args(args)
            .env_clear()
            .env("HOME", root.join("home"))
            .env("PATH", "/usr/bin:/bin")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CEILING_DIRECTORIES", root)
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    /// A repo with one commit on main, and the user identity in its own
    /// config so later commits need no flags.
    fn make_repo(root: &Path) -> String {
        let repo = root.join("repo");
        std::fs::create_dir_all(&repo).unwrap();
        std::fs::write(repo.join("README"), "x\n").unwrap();
        run(root, &repo, &["init", "-q", "-b", "main"]);
        run(root, &repo, &["config", "user.name", "t"]);
        run(root, &repo, &["config", "user.email", "t@example.invalid"]);
        run(root, &repo, &["add", "README"]);
        run(root, &repo, &["commit", "-q", "-m", "first"]);
        std::fs::canonicalize(&repo)
            .unwrap()
            .to_string_lossy()
            .into_owned()
    }

    #[test]
    fn names_and_paths() {
        assert!(valid_name("fix-2").is_ok());
        for bad in ["", "Fix", "a b", "a/b", "a_b", "é"] {
            assert_eq!(valid_name(bad).unwrap_err(), ERR_BAD_NAME, "{bad:?}");
        }
        assert_eq!(path("/a/b/", "x"), "/a/b-worktrees/x");
        assert_eq!(path("/a/./b", "x"), "/a/b-worktrees/x");
        assert_eq!(go_atoi("+3"), Some(3));
        assert_eq!(go_atoi("3a"), None);
    }

    // The tests below call the module's git, which resolves git on the
    // process PATH. They pass every path explicitly and read only what
    // the repo itself holds.

    #[test]
    fn add_status_dirty_remove() {
        let root = scratch("add");
        let repo = make_repo(&root);
        let wt = add(&repo, "feat").unwrap();
        assert_eq!(wt, format!("{repo}-worktrees/feat"));
        assert_eq!(status(&wt).unwrap(), (0, 0, "feat".to_string()));
        assert_eq!(super::repo(&wt).unwrap(), repo);
        assert_eq!(toplevel(&wt).unwrap(), wt);
        assert!(add(&repo, "feat")
            .unwrap_err()
            .starts_with("git worktree: "));
        assert_eq!(add(&repo, "Feat").unwrap_err(), ERR_BAD_NAME);

        run(
            &root,
            Path::new(&wt),
            &["commit", "-q", "--allow-empty", "-m", "s"],
        );
        assert_eq!(status(&wt).unwrap().0, 1);
        assert!(!dirty(&wt).unwrap());
        std::fs::write(Path::new(&wt).join("new.txt"), "x").unwrap();
        assert!(dirty(&wt).unwrap());
        assert!(remove(&repo, "feat", false)
            .unwrap_err()
            .starts_with("git worktree: "));
        remove(&repo, "feat", true).unwrap();
        std::fs::remove_file(Path::new(&wt).join("new.txt")).unwrap();
        remove(&repo, "feat", false).unwrap();
        assert!(!Path::new(&wt).exists());
        // The branch stays.
        assert_eq!(
            run(&root, Path::new(&repo), &["branch", "--list", "feat"]),
            "feat"
        );
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn remove_at_deletes_a_worktree_added_from_a_worktree() {
        let root = scratch("nested");
        let repo = make_repo(&root);
        let outer = add(&repo, "outer").unwrap();
        let inner = add(&outer, "inner").unwrap();
        assert_eq!(inner, format!("{outer}-worktrees/inner"));
        let main = super::repo(&inner).unwrap();
        assert_eq!(main, repo);
        // The path built from the main repo names no worktree.
        assert!(remove(&main, "inner", false)
            .unwrap_err()
            .contains("is not a working tree"));
        remove_at(&main, &inner).unwrap();
        assert!(!Path::new(&inner).exists());
        assert!(Path::new(&outer).join(".git").exists());
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn a_detached_repo_is_refused() {
        let root = scratch("detached");
        let repo = make_repo(&root);
        let head = run(&root, Path::new(&repo), &["rev-parse", "HEAD"]);
        run(
            &root,
            Path::new(&repo),
            &["checkout", "-q", "--detach", &head],
        );
        assert_eq!(add(&repo, "x").unwrap_err(), ERR_DETACHED);
        assert!(status(&repo).is_err());
        let _ = std::fs::remove_dir_all(&root);
    }
}
