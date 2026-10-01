//! `gate._maybe_checkpoint` and `checkpoints.snapshot`: once per new
//! prompt, the workspace is committed through a temporary index to a
//! private ref, `refs/daisugi/checkpoints/<session>/<entry>`, with the same
//! git commands the oracle runs. Best-effort: any failure stops the
//! checkpoint where Python's exception would, and never touches the
//! verdict.

use super::logs::Tree;
use super::paths::{os_path, path_join, path_parent, path_str};
use super::py::text::{is_alnum, is_space, strip};
use super::pymodel::{decode_utf8_strict, universal_newlines};
use super::pyjson::{dumps, loads, py_repr, py_str, Object, Value};
use super::pystr::splitlines;
use super::record::safe_session_value;
use super::{random_hex, PyErr, Runner, R};
use std::os::unix::ffi::OsStrExt;
use std::path::PathBuf;
use std::io::Read;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

/// Why a checkpoint stopped: the exception Python would have raised.
type Step<T> = Result<T, ()>;

const AUTHOR: &[(&str, &str)] = &[
    ("GIT_AUTHOR_NAME", "daisugi"),
    ("GIT_AUTHOR_EMAIL", "daisugi@localhost"),
    ("GIT_COMMITTER_NAME", "daisugi"),
    ("GIT_COMMITTER_EMAIL", "daisugi@localhost"),
];

/// How a git call failed: CalledProcessError (a non-zero exit), or any
/// other exception (OSError, a decode error, TimeoutExpired).
#[derive(PartialEq, Eq)]
enum GitErr {
    Called,
    Other,
}

/// `checkpoints._SAFE_GIT_ARGS`: a workspace repo's own config runs no
/// program (no fsmonitor, no file or config-defined hook).
const SAFE_GIT_ARGS: &[&[u8]] = &[
    b"-c",
    b"core.fsmonitor=false",
    b"-c",
    b"core.untrackedCache=false",
    b"-c",
    b"core.hooksPath=/dev/null",
    b"-c",
    b"hook.post-index-change.enabled=false",
    b"-c",
    b"hook.reference-transaction.enabled=false",
];

/// The share of the child's time one checkpoint may spend on git. The
/// oracle gives each is_repo 8 s and a snapshot 10 s, but this checkpoint
/// runs in the child, and the parent denies a call whose child is still
/// running at its deadline, even a call already allowed. So every git call
/// of a checkpoint shares one deadline: at most 5 s, and 2 s before the
/// child's deadline so the frame still gets out. The Go gate does the same.
const GIT_DEADLINE_MARGIN: Duration = Duration::from_secs(2);
const GIT_DEADLINE_CEILING: Duration = Duration::from_secs(5);

impl Runner {
    /// The one deadline for a checkpoint's git calls (see above).
    fn git_deadline(&self) -> Instant {
        let mut budget = self.verify_timeout_s;
        if self.ask {
            budget += self.ask_timeout_s.max(0.0);
        }
        if budget.is_nan() || budget > 3600.0 {
            budget = 3600.0;
        }
        let child_end = self.t0 + Duration::from_secs_f64(budget.max(1.0) + 3.0);
        let now = Instant::now();
        let left = child_end.saturating_duration_since(now).saturating_sub(GIT_DEADLINE_MARGIN);
        now + left.min(GIT_DEADLINE_CEILING)
    }
}

/// `checkpoints._git(repo, *args, env=..., deadline=...)`: its stdout,
/// `rstrip("\n")`, or how it failed. The system and global git config are
/// kept out, and the call is killed past the deadline.
fn git_call(repo: &PathBuf, args: &[&[u8]], env: &[(&str, &str)], end: Instant) -> Result<String, GitErr> {
    git_call_in(repo, None, args, env, end)
}

/// GIT_DIR, GIT_COMMON_DIR and GIT_WORK_TREE of a resident checkpoint
/// (FN-R-27): git never chooses the tree from repo config.
type Bound<'a> = Option<&'a [(String, String)]>;

/// `checkpoints._git(..., bound=...)`: with bound, core.worktree is
/// cleared on the command line as well.
fn git_call_in(repo: &PathBuf, bound: Bound, args: &[&[u8]], env: &[(&str, &str)], end: Instant) -> Result<String, GitErr> {
    let mut cmd = Command::new("git");
    cmd.arg("-C").arg(repo);
    for a in SAFE_GIT_ARGS.iter() {
        cmd.arg(std::ffi::OsStr::from_bytes(a));
    }
    if bound.is_some() {
        cmd.arg("-c").arg("core.worktree=");
    }
    for a in args.iter() {
        cmd.arg(std::ffi::OsStr::from_bytes(a));
    }
    cmd.env_remove(super::TTY_ENV).env("GIT_CONFIG_NOSYSTEM", "1").env("GIT_CONFIG_GLOBAL", "/dev/null");
    for (k, v) in bound.unwrap_or(&[]) {
        cmd.env(k, v);
    }
    for (k, v) in env {
        cmd.env(k, v);
    }
    cmd.stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
    super::procgroup::in_group(&mut cmd);
    let mut child = cmd.spawn().map_err(|_| GitErr::Other)?;
    let (mut o, mut e) = (child.stdout.take(), child.stderr.take());
    let out_t = std::thread::Builder::new().spawn(move || {
        let mut b = Vec::new();
        if let Some(o) = o.as_mut() {
            let _ = o.read_to_end(&mut b);
        }
        b
    });
    let err_t = std::thread::Builder::new().spawn(move || {
        let mut b = Vec::new();
        if let Some(e) = e.as_mut() {
            let _ = e.read_to_end(&mut b);
        }
        b
    });
    let (out_t, err_t) = match (out_t, err_t) {
        (Ok(a), Ok(b)) => (a, b),
        _ => {
            super::procgroup::kill_group(&mut child);
            return Err(GitErr::Other);
        }
    };
    let status = loop {
        match child.try_wait() {
            Ok(Some(st)) => break st,
            Ok(None) if Instant::now() >= end => {
                super::procgroup::kill_group(&mut child);
                return Err(GitErr::Other);
            }
            Ok(None) => std::thread::sleep(Duration::from_millis(2)),
            Err(_) => return Err(GitErr::Other),
        }
    };
    let stdout = out_t.join().unwrap_or_default();
    let stderr = err_t.join().unwrap_or_default();
    // text=True decodes both streams strictly, with universal newlines.
    let stdout = decode_utf8_strict(&stdout).map_err(|_| GitErr::Other)?;
    decode_utf8_strict(&stderr).map_err(|_| GitErr::Other)?;
    if !status.success() {
        return Err(GitErr::Called);
    }
    Ok(universal_newlines(&stdout).trim_end_matches('\n').to_string())
}

/// A git call whose any failure stops the checkpoint.
fn git(repo: &PathBuf, args: &[&[u8]], env: &[(&str, &str)], deadline: Instant) -> Step<String> {
    git_call(repo, args, env, deadline).map_err(|_| ())
}

/// `checkpoints.is_repo(path)`: false for any failure.
fn is_repo(repo: &PathBuf, deadline: Instant) -> bool {
    matches!(git_call(repo, &[b"rev-parse", b"--is-inside-work-tree"], &[], deadline), Ok(s) if s == "true")
}

/// `checkpoints._filter_overrides`: every filter driver the repo's own
/// config defines, blanked on the command line, with required=false.
fn filter_overrides(repo: &PathBuf, bound: Bound, env: &[(&str, &str)], deadline: Instant) -> Step<Vec<Vec<u8>>> {
    let raw = match git_call_in(repo, bound, &[b"config", b"-z", b"--get-regexp", b"^filter\\."], env, deadline) {
        Ok(r) => r,
        Err(GitErr::Called) => return Ok(vec![]),
        Err(GitErr::Other) => return Err(()),
    };
    let mut names: Vec<String> = Vec::new();
    for record in raw.split('\0').filter(|r| !r.is_empty()) {
        let key = record.split('\n').next().unwrap_or("");
        let parts: Vec<&str> = key.split('.').collect();
        if parts.len() >= 3 && parts[0] == "filter" {
            let name = parts[1..parts.len() - 1].join(".");
            if !names.contains(&name) {
                names.push(name);
            }
        }
    }
    names.sort_by_key(|n| n.chars().map(super::py::re::pycp).collect::<Vec<u32>>());
    let mut out = Vec::new();
    for n in names {
        for sub in ["clean", "smudge", "process"] {
            out.push(b"-c".to_vec());
            out.push(super::py::text::fsencode(&format!("filter.{n}.{sub}=")).map_err(|_| ())?);
        }
        out.push(b"-c".to_vec());
        out.push(super::py::text::fsencode(&format!("filter.{n}.required=false")).map_err(|_| ())?);
    }
    Ok(out)
}

/// `checkpoints._safe(raw)` of a str: every character Python calls
/// alphanumeric kept, with `.`, `_` and `-`; dots stripped from the ends;
/// at most 128 characters; or `none`.
fn safe_ref(raw: &str) -> String {
    let s: String = raw.chars().map(|c| if is_alnum(c) || matches!(c, '.' | '_' | '-') { c } else { '_' }).collect();
    let s: String = s.trim_matches('.').chars().take(128).collect();
    if s.is_empty() {
        "none".into()
    } else {
        s
    }
}

/// `checkpoints._safe(raw)` of the entry id as the tree gave it: a str as
/// above, a dict by its keys (each kept whole when it is alphanumeric or
/// a substring of "._-"), anything else the TypeError Python raises.
fn safe_entry(raw: &Value) -> Step<String> {
    match raw {
        Value::Str(s) => Ok(safe_ref(s)),
        Value::Obj(o) => {
            let s: String = o
                .keys()
                .iter()
                .map(|k| {
                    let alnum = !k.is_empty() && k.chars().all(is_alnum);
                    if alnum || "._-".contains(k.as_str()) {
                        k.clone()
                    } else {
                        "_".into()
                    }
                })
                .collect();
            let s: String = s.trim_matches('.').chars().take(128).collect();
            Ok(if s.is_empty() { "none".into() } else { s })
        }
        _ => Err(()),
    }
}

/// Python's `int(x)` for a usage count: whether it raises.
fn int_ok(v: &Value) -> bool {
    match v {
        Value::Null | Value::Bool(_) | Value::Int(_) => true,
        Value::Float(f) => f.is_finite(),
        Value::Str(s) => {
            let t: String = s.chars().map(|c| if is_space(c) { ' ' } else { c }).collect();
            let t = t.trim_matches(' ');
            let t = t.strip_prefix(['-', '+']).unwrap_or(t);
            let digits: Vec<char> = t.chars().collect();
            if digits.is_empty() || digits.iter().filter(|c| **c != '_').count() > 4300 {
                return false;
            }
            for (i, c) in digits.iter().enumerate() {
                if *c == '_' {
                    let ok = i > 0
                        && i + 1 < digits.len()
                        && super::py::text::is_decimal(digits[i - 1])
                        && super::py::text::is_decimal(digits[i + 1]);
                    if !ok {
                        return false;
                    }
                } else if !super::py::text::is_decimal(*c) {
                    return false;
                }
            }
            true
        }
        _ => false,
    }
}

/// `claude_transcript.read_turns` then `last_prompt_uuid`: the uuid, or
/// Err for an exception read_turns lets through.
fn last_prompt(tpath: &str) -> Step<Option<String>> {
    let p = os_path("open", tpath).map_err(|_| ())?;
    match std::fs::read(&p) {
        Ok(raw) => last_prompt_of(&raw),
        Err(_) => Ok(None),
    }
}

/// `last_prompt_uuid(parse_turns(text))`: the same, on a transcript read.
fn last_prompt_of(raw: &[u8]) -> Step<Option<String>> {
    let text = match decode_utf8_strict(raw) {
        Ok(t) => t,
        Err(_) => return Ok(None),
    };
    let mut last: Option<String> = None;
    for line in splitlines(&text) {
        if strip(line).is_empty() {
            continue;
        }
        let row = match loads(line) {
            Ok(v) => v,
            Err(super::pyjson::LoadError::Syntax(_)) => continue,
            // A digit-limit ValueError and a RecursionError are no
            // JSONDecodeError.
            Err(_) => return Err(()),
        };
        let row = match row {
            Value::Obj(o) => o,
            _ => continue,
        };
        let kind = row.value("type");
        let user = kind == &Value::Str("user".into());
        if !(user || kind == &Value::Str("assistant".into())) {
            continue;
        }
        let msg = match row.value("message") {
            Value::Obj(m) => m.clone(),
            _ => continue,
        };
        if !row.value("uuid").truthy() {
            continue;
        }
        // _content(msg)
        let (text, results) = match msg.value("content") {
            Value::Str(s) => (s.chars().take(400).collect::<String>(), 0usize),
            v => {
                let blocks: Vec<Value> = match v {
                    Value::List(l) => l.clone(),
                    Value::Obj(_) => vec![],
                    x if !x.truthy() => vec![],
                    _ => return Err(()),
                };
                let mut texts: Vec<String> = Vec::new();
                let mut results = 0usize;
                for b in blocks {
                    let b = match b {
                        Value::Obj(o) => o,
                        _ => continue,
                    };
                    let t = b.value("type");
                    if t == &Value::Str("text".into()) {
                        texts.push(match b.get("text") {
                            None => String::new(),
                            Some(x) => py_str(x).map_err(|_| ())?,
                        });
                    } else if t == &Value::Str("tool_result".into()) && b.value("tool_use_id").truthy() {
                        py_str(b.value("tool_use_id")).map_err(|_| ())?;
                        results += 1;
                    }
                }
                (texts.join(" ").chars().take(400).collect(), results)
            }
        };
        let uuid = py_str(row.value("uuid")).map_err(|_| ())?;
        if row.get("timestamp").is_some() {
            py_str(row.value("timestamp")).map_err(|_| ())?;
        }
        if !user {
            // _usage(msg): `msg.get("usage") or {}`, each count through int().
            match msg.value("usage") {
                Value::Obj(u) => {
                    for k in ["input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"] {
                        let v = u.value(k);
                        if v.truthy() && !int_ok(v) {
                            return Err(());
                        }
                    }
                }
                v if !v.truthy() => {}
                _ => return Err(()),
            }
        }
        if user && !text.is_empty() && results == 0 {
            last = Some(uuid);
        }
    }
    Ok(last)
}

/// `gate._cap_skipped_for_line`.
fn cap_skipped(skipped: &[String]) -> Vec<Value> {
    let mut out = Vec::new();
    let mut used = 0usize;
    for name in skipped {
        let cost = dumps(&Value::Str(name.clone()), true).chars().count() + 1;
        if used + cost > 1500 {
            break;
        }
        out.push(Value::Str(name.clone()));
        used += cost;
    }
    out
}

/// `tempfile.NamedTemporaryFile(dir=git_dir, prefix="daisugi-index-")`'s
/// name, made then removed.
fn temp_index(git_dir: &PathBuf) -> Step<PathBuf> {
    const CHARS: &[u8] = b"abcdefghijklmnopqrstuvwxyz0123456789_";
    for _ in 0..100 {
        let hex = random_hex(8).map_err(|_| ())?;
        let name: String = hex
            .as_bytes()
            .chunks(2)
            .map(|c| CHARS[u8::from_str_radix(std::str::from_utf8(c).unwrap_or("0"), 16).unwrap_or(0) as usize % CHARS.len()] as char)
            .collect();
        let p = git_dir.join(format!("daisugi-index-{name}"));
        use std::os::unix::fs::OpenOptionsExt;
        match std::fs::OpenOptions::new().write(true).create_new(true).mode(0o600).open(&p) {
            Ok(_) => {
                let _ = std::fs::remove_file(&p);
                return Ok(p);
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(_) => return Err(()),
        }
    }
    Err(())
}

/// `checkpoints.snapshot(repo, session_id=sid, entry_id=entry)`: the ref,
/// the commit, and the covered and skipped paths.
///
/// `workspace` (a resident checkpoint, FN-R-27) is the bound workspace,
/// resolved: then repo is ignored, the tree is that workspace, its .git is
/// the git dir, git is told both, and a file whose resolved path leaves
/// the workspace is left out and named in skipped.
fn snapshot(
    repo: PathBuf,
    sid: &str,
    entry: &Value,
    deadline: Instant,
    workspace: Option<&str>,
) -> Step<(String, String, usize, Vec<String>)> {
    let dl = deadline;
    let mut bound_env: Vec<(String, String)> = Vec::new();
    let (repo, git_dir) = if let Some(ws) = workspace {
        let Ok(Some((gd, common))) = super::fenceruns::git_layout(ws) else {
            return Err(());
        };
        bound_env.push(("GIT_DIR".into(), gd.clone()));
        bound_env.push(("GIT_COMMON_DIR".into(), common));
        bound_env.push(("GIT_WORK_TREE".into(), ws.to_string()));
        (PathBuf::from(ws), PathBuf::from(gd))
    } else {
        if !is_repo(&repo, deadline) {
            return Err(());
        }
        let top = git(&repo, &[b"rev-parse", b"--show-toplevel"], &[], dl)?;
        if top.is_empty() {
            return Err(());
        }
        let repo = PathBuf::from(std::ffi::OsStr::from_bytes(&super::py::text::fsencode(&top).map_err(|_| ())?));
        let gd = git(&repo, &[b"rev-parse", b"--git-dir"], &[], dl)?;
        let gd = PathBuf::from(std::ffi::OsStr::from_bytes(&super::py::text::fsencode(&gd).map_err(|_| ())?));
        let git_dir = if gd.is_absolute() { gd } else { repo.join(gd) };
        (repo, git_dir)
    };
    let bound: Bound = if workspace.is_some() { Some(&bound_env) } else { None };
    let entry_text = match entry {
        Value::Str(s) => s.clone(),
        other => py_repr(other).map_err(|_| ())?,
    };
    let reference = format!("refs/daisugi/checkpoints/{}/{}", safe_ref(sid), safe_entry(entry)?);
    let index = temp_index(&git_dir)?;
    let index_str = index.to_string_lossy().into_owned();
    let result = (|| -> Step<(String, String, usize, Vec<String>)> {
        let env = [("GIT_INDEX_FILE", index_str.as_str())];
        // The same overrides ride every call: git's racy-git re-check can
        // run a filter again on a later call.
        let filters = filter_overrides(&repo, bound, &env, deadline)?;
        let with = |rest: &[&[u8]]| -> Vec<Vec<u8>> {
            let mut v: Vec<Vec<u8>> = filters.clone();
            v.extend(rest.iter().map(|x| x.to_vec()));
            v
        };
        let run = |args: Vec<Vec<u8>>, env: &[(&str, &str)]| -> Step<String> {
            let refs: Vec<&[u8]> = args.iter().map(|a| a.as_slice()).collect();
            git_call_in(&repo, bound, &refs, env, dl).map_err(|_| ())
        };
        run(with(&[b"add", b"-A", b"--", b"."]), &env)?;
        let listed = run(with(&[b"ls-files", b"-z"]), &env)?;
        let mut covers: Vec<String> = Vec::new();
        let mut skipped: Vec<String> = Vec::new();
        for name in listed.split('\0').filter(|n| !n.is_empty()) {
            let bytes = super::py::text::fsencode(name).map_err(|_| ())?;
            let p = repo.join(std::ffi::OsStr::from_bytes(&bytes));
            match std::fs::symlink_metadata(&p) {
                Err(_) => skipped.push(name.to_string()),
                Ok(m) if !m.file_type().is_symlink() && m.len() > 5_000_000 => skipped.push(name.to_string()),
                Ok(_) if workspace.is_some_and(|ws| !stays_in(&p, ws)) => skipped.push(name.to_string()),
                Ok(_) => covers.push(name.to_string()),
            }
        }
        if !skipped.is_empty() {
            let mut args = with(&[b"rm", b"--cached", b"-q", b"--"]);
            for s in &skipped {
                args.push(super::py::text::fsencode(s).map_err(|_| ())?);
            }
            run(args, &env)?;
        }
        let tree = run(with(&[b"write-tree"]), &env)?;
        // _head: a CalledProcessError is no HEAD; any other error stops.
        let parent = match git_call_in(&repo, bound, &[b"rev-parse", b"--verify", b"-q", b"HEAD"], &[], dl) {
            Ok(p) => Some(p),
            Err(GitErr::Called) => None,
            Err(GitErr::Other) => return Err(()),
        };
        let msg = format!("daisugi checkpoints {sid}/{entry_text}");
        let msg_b = super::py::text::fsencode(&msg).map_err(|_| ())?;
        let mut args = with(&[b"commit-tree", tree.as_bytes(), b"-m", &msg_b, b"--no-gpg-sign"]);
        if let Some(p) = &parent {
            if !p.is_empty() {
                args.push(b"-p".to_vec());
                args.push(p.as_bytes().to_vec());
            }
        }
        let mut cenv: Vec<(&str, &str)> = env.to_vec();
        cenv.extend_from_slice(AUTHOR);
        let commit = run(args, &cenv)?;
        run(with(&[b"update-ref", reference.as_bytes(), commit.as_bytes()]), &[])?;
        let key = |s: &String| s.chars().map(super::py::re::pycp).collect::<Vec<u32>>();
        covers.sort_by_key(key);
        skipped.sort_by_key(key);
        Ok((reference.clone(), commit, covers.len(), skipped))
    })();
    let _ = std::fs::remove_file(&index);
    result
}

/// `checkpoints._stays_in`: path, every link followed, is still inside
/// workspace.
fn stays_in(path: &std::path::Path, workspace: &str) -> bool {
    let real = super::fenceruns::realpath(&path.to_string_lossy());
    real == workspace || real.starts_with(&format!("{}/", workspace.trim_end_matches('/')))
}

impl Runner {
    /// `gate._maybe_checkpoint`: best-effort.
    pub fn maybe_checkpoint(&self, p: &Object, session_id: &Value) -> R<()> {
        let _ = super::best_effort(self.checkpoint_body(p, session_id));
        Ok(())
    }

    fn checkpoint_body(&self, p: &Object, session_id: &Value) -> R<()> {
        let stop = || -> R<()> { Err(PyErr::value("checkpoint stopped").into()) };
        let (cwd, tpath) = (p.value("cwd"), p.value("transcript_path"));
        if !cwd.truthy() || !tpath.truthy() {
            return Ok(());
        }
        let cwd = match cwd {
            Value::Str(s) => s.clone(),
            _ => return stop(),
        };
        let repo = match os_path("open", &path_str(&cwd)) {
            Ok(r) => r,
            Err(_) => return stop(),
        };
        let deadline = self.git_deadline();
        // A resident call snapshots the workspace its fence run bound and
        // reads only a transcript under ~/.claude/projects (FN-R-27,
        // FN-R-29); resident_refusal already denied a cwd outside it and a
        // git dir that leads out.
        let workspace = if self.resident { self.caller.workspace.clone() } else { None };
        let found = if self.resident {
            let Some(_) = workspace.as_deref() else { return Ok(()) };
            let Value::Str(t) = tpath else { return Ok(()) };
            match super::fenceruns::resident_transcript(t) {
                Some(raw) => last_prompt_of(&raw),
                None => return Ok(()),
            }
        } else {
            if !is_repo(&repo, deadline) {
                return Ok(());
            }
            let tpath = match tpath {
                Value::Str(s) => s.clone(),
                _ => return stop(),
            };
            last_prompt(&tpath)
        };
        let prompt = match found {
            Ok(Some(u)) => u,
            Ok(None) => return Ok(()),
            Err(()) => return stop(),
        };
        let key = if session_id.truthy() { session_id } else { p.value("session_id") };
        let sid = safe_session_value(key)?;
        let state_dir = path_join(&self.root, "checkpoint-state");
        let state = path_join(&state_dir, &format!("{sid}.json"));
        // GR-R-1: read through no link; a file that is refused is none.
        let got = super::statefile::read_file(&path_str(&self.root), &["checkpoint-state"], &format!("{sid}.json"));
        if let Some(got) = got {
            // A read that fails after the open abandons the checkpoint, as
            // the broad except in gate._maybe_checkpoint does.
            let raw = match got {
                Ok(r) => r,
                Err(_) => return stop(),
            };
            let text = match decode_utf8_strict(&raw) {
                Ok(t) => t,
                Err(_) => return stop(),
            };
            let last = match loads(&text) {
                Ok(Value::Obj(o)) => o.value("prompt").clone(),
                _ => return stop(),
            };
            if super::predicate::py_eq(&last, &Value::Str(prompt.clone())) {
                return Ok(());
            }
        }
        let sessions = path_join(&path_parent(&self.root), "sessions");
        let mut tree = match Tree::open(&sessions, &sid)? {
            Some(t) => t,
            None => return stop(),
        };
        let head = tree.head()?;
        let entry = if head.truthy() { head } else { Value::Str("root".into()) };
        let (reference, commit, covers, skipped) = match snapshot(repo, &sid, &entry, deadline, workspace.as_deref()) {
            Ok(x) => x,
            Err(()) => return stop(),
        };
        let data = Object::new()
            .with("ref", reference.as_str())
            .with("commit", commit.as_str())
            .with("coversCount", covers as i64)
            .with("skipped", Value::List(cap_skipped(&skipped)))
            .with("skippedCount", skipped.len() as i64)
            .with("promptUuid", prompt.as_str());
        tree.append("checkpoint", &data, None)?;
        // A link planted at the state file is replaced, never written through
        // (GR-R-9).
        let body = dumps(&Value::Obj(Object::new().with("prompt", prompt.as_str())), true);
        super::statewrite::write_file(&path_str(&self.root), &["checkpoint-state"], &format!("{sid}.json"), &body, true)
            .map_err(|e| super::paths::os_error(&e, &state))?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ref_names() {
        assert_eq!(safe_ref("s-1.x"), "s-1.x");
        assert_eq!(safe_ref("..a b.."), "a_b");
        assert_eq!(safe_ref("é"), "é");
        assert_eq!(safe_ref(""), "none");
    }

    use crate::gate::procgroup::testing::{kill_all, left_after, live_pids, marker};

    fn sh(args: &[&str]) {
        let st = Command::new(args[0]).args(&args[1..]).status().unwrap();
        assert!(st.success(), "{args:?}");
    }

    fn git_repo(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("dg-ckpt-{}-{tag}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let repo = dir.join("repo");
        sh(&["git", "init", "-q", repo.to_str().unwrap()]);
        repo
    }

    /// A git call past its deadline ends at once, and so does everything
    /// git started. The repo's alias runs a sleep that holds git's stdout.
    #[test]
    fn a_git_call_past_its_deadline_leaves_nothing_behind() {
        let repo = git_repo("slow");
        let m = marker();
        sh(&["git", "-C", repo.to_str().unwrap(), "config", "alias.slow", &format!("!sleep {m}")]);
        let t0 = Instant::now();
        let r = git_call(&repo, &[b"slow"], &[], t0 + Duration::from_millis(300));
        let took = t0.elapsed();
        let left = left_after(&m);
        let _ = std::fs::remove_dir_all(repo.parent().unwrap());
        assert!(r.is_err(), "a git call past its deadline succeeded");
        assert!(took < Duration::from_secs(3), "{took:?}");
        assert!(left.is_empty(), "processes from the call outlived it: {left:?}");
    }

    /// Not a test on its own: the next test runs the test binary with it
    /// selected, as a gate whose git call hangs.
    #[test]
    fn helper_git_blocks_on_a_fifo() {
        if let Some(repo) = std::env::var_os("DAISUGI_TEST_FIFO_REPO") {
            let _ = git_call(&PathBuf::from(repo), &[b"rev-parse"], &[], Instant::now() + Duration::from_secs(60));
        }
    }

    /// A repo whose config includes a fifo with no writer makes git block
    /// for ever. When the gate is killed while that git call runs (the
    /// guarded parent does this at its own deadline), git must die with it.
    #[test]
    fn a_gate_killed_mid_git_takes_its_git_with_it() {
        let repo = git_repo("fifo");
        let fifo = repo.parent().unwrap().join("include.cfg");
        sh(&["mkfifo", fifo.to_str().unwrap()]);
        sh(&["git", "-C", repo.to_str().unwrap(), "config", "include.path", fifo.to_str().unwrap()]);
        let needle = repo.to_str().unwrap().to_string();
        let name = format!("{}::helper_git_blocks_on_a_fifo", module_path!().split_once("::").unwrap().1);
        let mut helper = Command::new(std::env::current_exe().unwrap())
            .args([name.as_str(), "--exact", "--test-threads=1"])
            .env("DAISUGI_TEST_FIFO_REPO", &repo)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let helper_pid = helper.id() as i32;
        let started = (0..250).any(|_| {
            if live_pids(&needle).iter().any(|&p| p != helper_pid) {
                return true;
            }
            std::thread::sleep(Duration::from_millis(20));
            false
        });
        let _ = helper.kill();
        let _ = helper.wait();
        let left = left_after(&needle);
        kill_all(&needle);
        let _ = std::fs::remove_dir_all(repo.parent().unwrap());
        assert!(started, "the helper's git call never started");
        assert!(left.is_empty(), "processes from the call outlived it: {left:?}");
    }

    /// Checkpoints are off inside the fence (FN-R-36); the bound snapshot
    /// (FN-R-27, FN-R-34) stays for unfenced use and is called directly; a
    /// resident transcript is read only under ~/.claude/projects (FN-R-29).
    /// tests/test_gate_resident_checkpoint.py is the oracle's copy.
    mod resident {
        use super::sh;
        use crate::gate::{fenceruns, resident::Caller, resident::Request, run_resident};
        use std::collections::HashMap;
        use std::path::{Path, PathBuf};
        use std::process::Command;

        const SECRET: &str = "the-secret-the-fence-denies\n";

        struct Cp {
            dir: PathBuf,
            root: PathBuf,
            projects: PathBuf,
        }

        impl Drop for Cp {
            fn drop(&mut self) {
                fenceruns::set_transcript_root(None);
                let _ = std::fs::remove_dir_all(&self.dir);
            }
        }

        fn cp() -> Cp {
            let dir = std::fs::canonicalize(crate::gate::statefile::tests::dir()).unwrap();
            let root = dir.join("gate");
            std::fs::create_dir_all(&root).unwrap();
            let projects = dir.join("home/.claude/projects");
            std::fs::create_dir_all(projects.join("p")).unwrap();
            fenceruns::set_transcript_root(Some(projects.to_string_lossy().into_owned()));
            Cp { dir, root, projects }
        }

        fn git(dir: &Path, args: &[&str]) -> String {
            let out = Command::new("git")
                .arg("-C")
                .arg(dir)
                .args(args)
                .env("GIT_CONFIG_NOSYSTEM", "1")
                .env("GIT_CONFIG_GLOBAL", "/dev/null")
                .output()
                .unwrap();
            assert!(out.status.success(), "git {args:?}");
            String::from_utf8_lossy(&out.stdout).into_owned()
        }

        fn repo(dir: &Path) -> PathBuf {
            std::fs::create_dir_all(dir).unwrap();
            sh(&["git", "init", "-q", dir.to_str().unwrap()]);
            git(dir, &["config", "user.email", "t@t"]);
            git(dir, &["config", "user.name", "t"]);
            std::fs::write(dir.join("a.txt"), "one\n").unwrap();
            git(dir, &["add", "a.txt"]);
            git(dir, &["commit", "-q", "-m", "init"]);
            dir.to_path_buf()
        }

        impl Cp {
            /// The bound snapshot, called directly: the unfenced use it
            /// stays for (FN-R-36).
            fn snap(&self, ws: &Path, cwd: &Path) {
                let end = std::time::Instant::now() + std::time::Duration::from_secs(10);
                let r = super::super::snapshot(cwd.to_path_buf(), "s1", &super::super::Value::Str("e1".into()), end, Some(ws.to_str().unwrap()));
                assert!(r.is_ok(), "no snapshot of {}", ws.display());
            }

            /// Whether ws's git layout is refused and no snapshot is taken.
            fn refused(&self, ws: &Path) -> bool {
                let end = std::time::Instant::now() + std::time::Duration::from_secs(10);
                let w = ws.to_str().unwrap();
                let r = super::super::snapshot(ws.to_path_buf(), "s1", &super::super::Value::Str("e1".into()), end, Some(w));
                fenceruns::git_dir_refusal(w).is_some() && r.is_err()
            }

            fn prompt(&self, path: &Path) -> String {
                std::fs::create_dir_all(path.parent().unwrap()).unwrap();
                let row = r#"{"type": "user", "uuid": "u1", "parentUuid": null, "sessionId": "s1", "timestamp": "2026-10-23T10:00:00Z", "message": {"role": "user", "content": "hi"}}"#;
                std::fs::write(path, format!("{row}\n")).unwrap();
                path.to_string_lossy().into_owned()
            }

            fn call(&self, ws: &Path, cwd: &Path, transcript: &str) -> (i32, String) {
                let pl = serde_json::json!({
                    "session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": "/x"},
                    "cwd": cwd.to_string_lossy(), "transcript_path": transcript,
                });
                let q = Request {
                    argv: ["--root", self.root.to_str().unwrap(), "--mode", "audit", "--checkpoints"]
                        .iter()
                        .map(|s| s.to_string())
                        .collect(),
                    stdin: pl.to_string().into_bytes(),
                    caller: Caller { workspace: Some(ws.to_string_lossy().into_owned()), ..Default::default() },
                };
                let mut env = HashMap::new();
                env.insert("HOME".to_string(), self.dir.join("home").to_string_lossy().into_owned());
                env.insert("PATH".to_string(), std::env::var("PATH").unwrap_or_default());
                let res = run_resident(&q, &env);
                (res.exit, String::from_utf8_lossy(&res.stderr).into_owned() + &res.out_stderr)
            }
        }

        fn refs(repo: &Path) -> Vec<String> {
            git(repo, &["for-each-ref", "--format=%(refname)", "refs/daisugi"]).split_whitespace().map(String::from).collect()
        }

        fn names(repo: &Path, r: &str) -> Vec<String> {
            git(repo, &["ls-tree", "-r", "--name-only", r]).split_whitespace().map(String::from).collect()
        }

        fn hashed(repo: &Path) -> bool {
            let out = Command::new("git")
                .arg("-C")
                .arg(repo)
                .args(["cat-file", "--batch-all-objects", "--batch"])
                .output()
                .unwrap();
            String::from_utf8_lossy(&out.stdout).contains(SECRET)
        }

        // Checkpoints are off inside the fence (FN-R-36): the unfenced
        // server would run git on config the fenced agent writes.
        #[test]
        fn a_fenced_checkpoint_call_is_denied() {
            let c = cp();
            let ws = repo(&c.dir.join("ws"));
            let t = c.prompt(&c.projects.join("p/t.jsonl"));
            let (code, err) = c.call(&ws, &ws, &t);
            assert_eq!(code, 2, "{err}");
            assert!(err.contains("checkpoints are off inside the fence"), "{err}");
            assert!(refs(&ws).is_empty(), "the unfenced server took a checkpoint for a fenced session");
        }

        #[test]
        fn a_transcript_outside_the_projects_dir_or_a_fifo_is_ignored() {
            let c = cp();
            let t = c.prompt(&c.dir.join("elsewhere/t.jsonl"));
            assert!(fenceruns::resident_transcript(&t).is_none());
            let link = c.projects.join("p/l.jsonl");
            std::os::unix::fs::symlink(&t, &link).unwrap();
            assert!(fenceruns::resident_transcript(link.to_str().unwrap()).is_none());
            let inside = c.prompt(&c.projects.join("p/t.jsonl"));
            assert!(fenceruns::resident_transcript(&inside).is_some());
            let fifo = c.projects.join("p/f.jsonl");
            sh(&["mkfifo", fifo.to_str().unwrap()]);
            let t0 = std::time::Instant::now();
            assert!(fenceruns::resident_transcript(fifo.to_str().unwrap()).is_none());
            assert!(t0.elapsed() < std::time::Duration::from_secs(3), "the read waited on a FIFO");
        }

        #[test]
        fn core_worktree_cannot_move_the_tree() {
            let c = cp();
            let other = c.dir.join("other");
            std::fs::create_dir_all(&other).unwrap();
            std::fs::write(other.join("id_key"), SECRET).unwrap();
            let ws = repo(&c.dir.join("ws"));
            git(&ws, &["config", "core.worktree", other.to_str().unwrap()]);
            c.snap(&ws, &ws);
            let r = refs(&ws);
            assert_eq!(r.len(), 1, "{r:?}");
            let n = names(&ws, &r[0]);
            assert!(n.contains(&"a.txt".into()) && !n.contains(&"id_key".into()), "{n:?}");
            assert!(!hashed(&ws), "a file outside the workspace was hashed");
        }

        #[test]
        fn a_git_dir_that_leads_out_denies_the_call() {
            for shape in ["gitfile", "link", "commondir", "objects-link", "fanout-link"] {
                let c = cp();
                let other = repo(&c.dir.join("other"));
                let ws = c.dir.join("ws");
                let g = ws.join(".git");
                if shape == "gitfile" {
                    std::fs::create_dir_all(&ws).unwrap();
                    std::fs::write(&g, format!("gitdir: {}\n", other.join(".git").display())).unwrap();
                } else {
                    repo(&ws);
                }
                match shape {
                    "link" => {
                        std::fs::rename(&g, c.dir.join("moved.git")).unwrap();
                        std::os::unix::fs::symlink(c.dir.join("moved.git"), &g).unwrap();
                    }
                    "commondir" => std::fs::write(g.join("commondir"), format!("{}\n", other.join(".git").display())).unwrap(),
                    "objects-link" => {
                        std::fs::rename(g.join("objects"), c.dir.join("moved-objects")).unwrap();
                        std::os::unix::fs::symlink(c.dir.join("moved-objects"), g.join("objects")).unwrap();
                    }
                    "fanout-link" => {
                        std::fs::create_dir_all(c.dir.join("far")).unwrap();
                        std::os::unix::fs::symlink(c.dir.join("far"), g.join("objects/ab")).unwrap();
                    }
                    _ => {}
                }
                assert!(c.refused(&ws), "{shape}");
                assert!(refs(&other).is_empty(), "{shape}");
            }
        }

        #[test]
        fn no_walk_up_to_a_parent_repo() {
            let c = cp();
            let parent = repo(&c.dir.join("parent"));
            let ws = parent.join("ws");
            std::fs::create_dir_all(&ws).unwrap();
            std::fs::write(ws.join("b.txt"), "two\n").unwrap();
            assert!(!c.refused(&ws), "no .git is not a refusal");
            let end = std::time::Instant::now() + std::time::Duration::from_secs(10);
            let r = super::super::snapshot(ws.clone(), "s1", &super::super::Value::Str("e1".into()), end, Some(ws.to_str().unwrap()));
            assert!(r.is_err(), "a workspace with no .git got a snapshot");
            assert!(refs(&parent).is_empty(), "the parent repo was checkpointed");
        }

        #[test]
        fn a_subdir_cwd_snapshots_the_bound_workspace() {
            let c = cp();
            let ws = repo(&c.dir.join("ws"));
            std::fs::create_dir_all(ws.join("sub")).unwrap();
            std::fs::write(ws.join("sub/c.txt"), "three\n").unwrap();
            c.snap(&ws, &ws.join("sub"));
            let r = refs(&ws);
            assert_eq!(r.len(), 1);
            let n = names(&ws, &r[0]);
            assert!(n.contains(&"a.txt".into()) && n.contains(&"sub/c.txt".into()), "{n:?}");
        }

        #[test]
        fn a_link_that_leaves_the_workspace_is_not_copied() {
            let c = cp();
            let other = c.dir.join("other");
            std::fs::create_dir_all(&other).unwrap();
            std::fs::write(other.join("id_key"), SECRET).unwrap();
            let ws = repo(&c.dir.join("ws"));
            std::os::unix::fs::symlink(other.join("id_key"), ws.join("key")).unwrap();
            std::os::unix::fs::symlink(&other, ws.join("dir")).unwrap();
            c.snap(&ws, &ws);
            let r = refs(&ws);
            assert_eq!(r.len(), 1);
            let n = names(&ws, &r[0]);
            assert!(!n.contains(&"key".into()) && !n.contains(&"dir".into()) && n.contains(&"a.txt".into()), "{n:?}");
            assert!(!hashed(&ws));
        }

        // A git worktree workspace (W-1): its .git is a gitfile, taken when
        // git's own link checks out by realpath.
        fn worktree(c: &Cp) -> (PathBuf, PathBuf) {
            let main = repo(&c.dir.join("main"));
            let wt = c.dir.join("wt");
            git(&main, &["worktree", "add", "-q", "--detach", wt.to_str().unwrap()]);
            (main, wt)
        }

        #[test]
        fn a_git_worktree_takes_a_checkpoint_and_restores_it() {
            let c = cp();
            let (main, wt) = worktree(&c);
            std::fs::write(wt.join("w.txt"), "in the worktree\n").unwrap();
            c.snap(&wt, &wt);
            let r = refs(&wt);
            assert_eq!(r.len(), 1, "{r:?}");
            let n = names(&wt, &r[0]);
            assert!(n.contains(&"a.txt".into()) && n.contains(&"w.txt".into()), "{n:?}");
            // The Rust port has no restore of its own; git restores the ref.
            std::fs::write(wt.join("a.txt"), "changed\n").unwrap();
            std::fs::remove_file(wt.join("w.txt")).unwrap();
            git(&wt, &["restore", &format!("--source={}", r[0]), "--worktree", "--", "."]);
            assert_eq!(std::fs::read_to_string(wt.join("a.txt")).unwrap(), "one\n");
            assert_eq!(std::fs::read_to_string(wt.join("w.txt")).unwrap(), "in the worktree\n");
            assert!(!main.join("w.txt").exists(), "the main tree was written");
        }

        #[test]
        fn a_forged_worktree_gitfile_denies_the_call() {
            for shape in ["other-worktree", "no-back-link", "back-link-elsewhere", "commondir-elsewhere", "not-under-worktrees"] {
                let c = cp();
                let (main, wt) = worktree(&c);
                let other = repo(&c.dir.join("other"));
                let ws = c.dir.join("ws");
                std::fs::create_dir_all(&ws).unwrap();
                let head = git(&main, &["rev-parse", "HEAD"]);
                let admin = main.join(".git/worktrees/forged");
                let target = match shape {
                    "other-worktree" => main.join(".git/worktrees/wt"),
                    "not-under-worktrees" => {
                        let t = c.dir.join("loose");
                        std::fs::create_dir_all(&t).unwrap();
                        std::fs::write(t.join("gitdir"), format!("{}\n", ws.join(".git").display())).unwrap();
                        std::fs::write(t.join("commondir"), format!("{}\n", main.join(".git").display())).unwrap();
                        std::fs::write(t.join("HEAD"), &head).unwrap();
                        t
                    }
                    _ => {
                        std::fs::create_dir_all(&admin).unwrap();
                        std::fs::write(admin.join("HEAD"), &head).unwrap();
                        if shape != "no-back-link" {
                            let back = if shape == "back-link-elsewhere" { wt.join(".git") } else { ws.join(".git") };
                            std::fs::write(admin.join("gitdir"), format!("{}\n", back.display())).unwrap();
                        }
                        let common = if shape == "commondir-elsewhere" { other.join(".git") } else { PathBuf::from("../..") };
                        std::fs::write(admin.join("commondir"), format!("{}\n", common.display())).unwrap();
                        admin.clone()
                    }
                };
                std::fs::write(ws.join(".git"), format!("gitdir: {}\n", target.display())).unwrap();
                assert!(c.refused(&ws), "{shape}");
                assert!(refs(&main).is_empty() && refs(&other).is_empty(), "{shape}");
            }
        }

        #[test]
        fn a_worktree_runs_no_filter_or_fsmonitor_from_the_common_config() {
            let c = cp();
            let (main, wt) = worktree(&c);
            let mark = c.dir.join("marks");
            std::fs::create_dir_all(&mark).unwrap();
            let mon = c.dir.join("fsmonitor.sh");
            std::fs::write(&mon, format!("#!/bin/sh\ntouch {}/fsmonitor\n", mark.display())).unwrap();
            sh(&["chmod", "755", mon.to_str().unwrap()]);
            git(&main, &["config", "filter.x.clean", &format!("sh -c 'touch {}/clean; cat'", mark.display())]);
            git(&main, &["config", "filter.x.required", "true"]);
            git(&main, &["config", "core.fsmonitor", mon.to_str().unwrap()]);
            std::fs::write(wt.join(".gitattributes"), "*.txt filter=x\n").unwrap();
            std::fs::write(wt.join("c.txt"), "three\n").unwrap();
            c.snap(&wt, &wt);
            assert_eq!(refs(&wt).len(), 1);
            let left: Vec<_> = std::fs::read_dir(&mark).unwrap().flatten().map(|e| e.file_name()).collect();
            assert!(left.is_empty(), "a filter or fsmonitor from the common config ran: {left:?}");
        }
    }

    #[test]
    fn int_rules() {
        for (s, ok) in [(" 12 ", true), ("1_000", true), ("1__0", false), ("1e3", false), ("١٢", true), ("", false)] {
            assert_eq!(int_ok(&Value::Str(s.into())), ok, "{s}");
        }
    }
}
