//! `gate._maybe_checkpoint` and `checkpoints.snapshot`: once per new
//! prompt, the workspace is committed through a temporary index to a
//! private ref, `refs/daisugi/checkpoints/<session>/<entry>`, with the same
//! git commands the oracle runs. Best-effort: any failure stops the
//! checkpoint where Python's exception would, and never touches the
//! verdict.

use super::logs::{chmod_600, mkdir_private, Tree};
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
    let mut cmd = Command::new("git");
    cmd.arg("-C").arg(repo);
    for a in SAFE_GIT_ARGS.iter().chain(args.iter()) {
        cmd.arg(std::ffi::OsStr::from_bytes(a));
    }
    cmd.env_remove(super::TTY_ENV).env("GIT_CONFIG_NOSYSTEM", "1").env("GIT_CONFIG_GLOBAL", "/dev/null");
    for (k, v) in env {
        cmd.env(k, v);
    }
    cmd.stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
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
            let _ = child.kill();
            let _ = child.wait();
            return Err(GitErr::Other);
        }
    };
    let status = loop {
        match child.try_wait() {
            Ok(Some(st)) => break st,
            Ok(None) if Instant::now() >= end => {
                let _ = child.kill();
                let _ = child.wait();
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
fn filter_overrides(repo: &PathBuf, env: &[(&str, &str)], deadline: Instant) -> Step<Vec<Vec<u8>>> {
    let raw = match git_call(repo, &[b"config", b"-z", b"--get-regexp", b"^filter\\."], env, deadline) {
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
    let text = match std::fs::read(&p) {
        Ok(raw) => match decode_utf8_strict(&raw) {
            Ok(t) => t,
            Err(_) => return Ok(None),
        },
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
fn snapshot(repo: PathBuf, sid: &str, entry: &Value, deadline: Instant) -> Step<(String, String, usize, Vec<String>)> {
    let dl = deadline;
    if !is_repo(&repo, deadline) {
        return Err(());
    }
    let top = git(&repo, &[b"rev-parse", b"--show-toplevel"], &[], dl)?;
    if top.is_empty() {
        return Err(());
    }
    let repo = PathBuf::from(std::ffi::OsStr::from_bytes(&super::py::text::fsencode(&top).map_err(|_| ())?));
    let entry_text = match entry {
        Value::Str(s) => s.clone(),
        other => py_repr(other).map_err(|_| ())?,
    };
    let reference = format!("refs/daisugi/checkpoints/{}/{}", safe_ref(sid), safe_entry(entry)?);
    let gd = git(&repo, &[b"rev-parse", b"--git-dir"], &[], dl)?;
    let gd = PathBuf::from(std::ffi::OsStr::from_bytes(&super::py::text::fsencode(&gd).map_err(|_| ())?));
    let git_dir = if gd.is_absolute() { gd } else { repo.join(gd) };
    let index = temp_index(&git_dir)?;
    let index_str = index.to_string_lossy().into_owned();
    let result = (|| -> Step<(String, String, usize, Vec<String>)> {
        let env = [("GIT_INDEX_FILE", index_str.as_str())];
        // The same overrides ride every call: git's racy-git re-check can
        // run a filter again on a later call.
        let filters = filter_overrides(&repo, &env, deadline)?;
        let with = |rest: &[&[u8]]| -> Vec<Vec<u8>> {
            let mut v: Vec<Vec<u8>> = filters.clone();
            v.extend(rest.iter().map(|x| x.to_vec()));
            v
        };
        let run = |args: Vec<Vec<u8>>, env: &[(&str, &str)]| -> Step<String> {
            let refs: Vec<&[u8]> = args.iter().map(|a| a.as_slice()).collect();
            git(&repo, &refs, env, dl)
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
        let parent = match git_call(&repo, &[b"rev-parse", b"--verify", b"-q", b"HEAD"], &[], dl) {
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
        if !is_repo(&repo, deadline) {
            return Ok(());
        }
        let tpath = match tpath {
            Value::Str(s) => s.clone(),
            _ => return stop(),
        };
        let prompt = match last_prompt(&tpath) {
            Ok(Some(u)) => u,
            Ok(None) => return Ok(()),
            Err(()) => return stop(),
        };
        let key = if session_id.truthy() { session_id } else { p.value("session_id") };
        let sid = safe_session_value(key)?;
        let state_dir = path_join(&self.root, "checkpoint-state");
        let state = path_join(&state_dir, &format!("{sid}.json"));
        if super::paths::exists(&state)? {
            let raw = std::fs::read(os_path("open", &state)?).map_err(|e| super::paths::os_error(&e, &state))?;
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
        let (reference, commit, covers, skipped) = match snapshot(repo, &sid, &entry, deadline) {
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
        mkdir_private(&state_dir)?;
        let body = dumps(&Value::Obj(Object::new().with("prompt", prompt.as_str())), true);
        std::fs::write(os_path("open", &state)?, body).map_err(|e| super::paths::os_error(&e, &state))?;
        chmod_600(&state);
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

    #[test]
    fn int_rules() {
        for (s, ok) in [(" 12 ", true), ("1_000", true), ("1__0", false), ("1e3", false), ("١٢", true), ("", false)] {
            assert_eq!(int_ok(&Value::Str(s.into())), ok, "{s}");
        }
    }
}
