//! The oracle's git-backed pathway registry (`opendaisugi/git_pathway_store.py`):
//! a local clone of a shared git repository whose `pathways/` directory
//! holds signed bundles, with the local SQLite pathway store as its cache.
//!
//! git runs as a child process, as the oracle runs it: `git -C <repo> ...`
//! with this process's environment less every `GIT_*` variable, a ceiling
//! at the directory above the clone, and its output captured. The Go
//! client's `internal/registry` is the reference.

use std::collections::HashMap;
use std::process::{Command, Stdio};

use crate::bundle;
use crate::gate::py::text::{repr_list, strip};
use crate::gate::pyjson::{loads_py, Object, Value};
use crate::lerr::{LErr, PyError, LR};
use crate::pathways::importer::py_row;
use crate::pathways::pathway::Pathway;
use crate::pathways::store::Store as Cache;

pub const PATHWAYS_SUBDIR: &str = "pathways";
pub const TRUSTED_SIGNERS_FILE: &str = "trusted-signers.json";

/// `environ` without any `GIT_*` variable (the oracle's `git_env`): an
/// inherited GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_CONFIG_* or
/// GIT_SSH_COMMAND must not point git at another repository or change
/// what it runs.
pub fn scrubbed(environ: &HashMap<String, String>) -> Vec<(String, String)> {
    let mut out: Vec<(String, String)> = environ
        .iter()
        .filter(|(k, _)| !k.starts_with("GIT_"))
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect();
    out.sort();
    out
}

/// `str(Path(repo).resolve().parent)`: git looks for a repository in
/// `repo` itself and never in a directory above it.
fn ceiling(repo: &str) -> String {
    let p = match std::fs::canonicalize(repo) {
        Ok(p) => p.to_string_lossy().into_owned(),
        Err(_) => {
            let cwd = std::env::current_dir()
                .map(|d| d.to_string_lossy().into_owned())
                .unwrap_or_default();
            if repo.starts_with('/') {
                repo.to_string()
            } else {
                format!("{cwd}/{repo}")
            }
        }
    };
    match p.rfind('/') {
        Some(0) | None => "/".into(),
        Some(i) => p[..i].to_string(),
    }
}

/// The first executable `name` on `path`, as `subprocess` finds it.
pub fn look_path(name: &str, path: &str) -> Option<String> {
    use std::os::unix::fs::PermissionsExt;
    for dir in path.split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let p = format!("{}/{name}", dir.trim_end_matches('/'));
        if let Ok(m) = std::fs::metadata(&p) {
            if m.is_file() && m.permissions().mode() & 0o111 != 0 {
                return Some(p);
            }
        }
    }
    None
}

/// `FileNotFoundError` for a git that is not on PATH.
pub fn no_git() -> PyError {
    PyError::new(
        "FileNotFoundError",
        "[Errno 2] No such file or directory: 'git'",
    )
}

/// The oracle's CalledProcessError for a git run with check=True.
pub fn called_process_error(args: &[String], code: i32) -> PyError {
    PyError::new(
        "subprocess.CalledProcessError",
        format!(
            "Command '{}' returned non-zero exit status {code}.",
            repr_list(args)
        ),
    )
}

/// A finished git run.
pub struct GitResult {
    pub code: i32,
    pub stdout: String,
}

/// Runs git the way the oracle's `_git` does.
#[derive(Clone, Default)]
pub struct Git {
    pub environ: HashMap<String, String>,
}

impl Git {
    /// `subprocess.run(["git", "-C", repo, *args], capture_output=True)`.
    /// A missing git is FileNotFoundError, as in the oracle.
    pub fn run(&self, repo: &str, args: &[&str]) -> LR<GitResult> {
        let path = self.environ.get("PATH").map(String::as_str).unwrap_or("");
        let git = look_path("git", path).ok_or_else(no_git)?;
        let out = Command::new(&git)
            .arg("-C")
            .arg(repo)
            .args(args)
            .env_clear()
            .envs(scrubbed(&self.environ))
            .env("GIT_CEILING_DIRECTORIES", ceiling(repo))
            .stdin(Stdio::null())
            .output()
            .map_err(|e| LErr::io(e, &git))?;
        Ok(GitResult {
            code: out.status.code().unwrap_or(-1),
            stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
        })
    }

    fn check(&self, repo: &str, args: &[&str]) -> LR<()> {
        let r = self.run(repo, args)?;
        if r.code != 0 {
            let mut all = vec!["git".to_string(), "-C".into(), repo.to_string()];
            all.extend(args.iter().map(|a| a.to_string()));
            return Err(called_process_error(&all, r.code).into());
        }
        Ok(())
    }
}

/// `GitPathwayStore`.
pub struct Store {
    /// As pathlib prints it.
    pub repo_path: String,
    pub private: Option<String>,
    pub public: Option<String>,
    pub publisher: String,
    pub require_signed: bool,
    pub git: Git,
    cache: Cache,
    trust_path: String,
}

/// `GitPathwayStore`'s keyword arguments.
#[derive(Default)]
pub struct Options {
    pub private: Option<String>,
    pub public: Option<String>,
    pub publisher: Option<String>,
    pub require_signed: bool,
    pub environ: HashMap<String, String>,
}

fn join(a: &str, b: &str) -> String {
    crate::cli::gateroot::join(a, b)
}

/// One bundle file read: its validated dump, or None when the oracle
/// skips it (unparseable or invalid).
struct Found {
    bundle: Option<Object>,
}

impl Store {
    /// `GitPathwayStore(repo_path=...)`: the repository must exist; the
    /// cache (repo/.cache/pathways.db) is made, and the bundles already in
    /// the clone are put in it. The count put is dropped, as the
    /// constructor drops it.
    pub fn open(repo_path: &str, o: Options) -> LR<Store> {
        let repo = crate::cli::gateroot::path_str(repo_path);
        if std::fs::metadata(&repo).is_err() {
            return Err(PyError::new(
                "ValueError",
                format!("GitPathwayStore: repo_path {repo} does not exist; clone it first with `daisugi registry init`"),
            )
            .into());
        }
        let cache_dir = join(&repo, ".cache");
        let found = read_bundles(&join(&repo, PATHWAYS_SUBDIR))?;
        crate::cli::gateroot::mkdir_parents(&cache_dir, 0o777)
            .map_err(|e| LErr::io(e, &cache_dir))?;
        let cache = Cache::open(&join(&cache_dir, "pathways.db"))?;
        let s = Store {
            repo_path: repo,
            private: o.private,
            public: o.public,
            publisher: o.publisher.unwrap_or_else(|| "opendaisugi-instance".into()),
            require_signed: o.require_signed,
            git: Git { environ: o.environ },
            cache,
            trust_path: join(&cache_dir, TRUSTED_SIGNERS_FILE),
        };
        s.materialize(&found)?;
        Ok(s)
    }

    fn pathways_dir(&self) -> String {
        join(&self.repo_path, PATHWAYS_SUBDIR)
    }

    /// `_load_trusted_signers`: the distinct str values of the local
    /// anchor file, or none when it is absent, not JSON, or not an object.
    /// The in-repo trusted-signers.json is never read.
    pub fn trusted_signers(&self) -> LR<Vec<String>> {
        let raw = match std::fs::read(&self.trust_path) {
            Ok(r) => r,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(vec![]),
            Err(e) => return Err(LErr::io(e, &self.trust_path)),
        };
        let text = String::from_utf8(raw)
            .map_err(|_| LErr::Unread(format!("{} is not UTF-8", self.trust_path)))?;
        let Ok(Value::Obj(o)) = loads_py(&text, 900) else {
            return Ok(vec![]);
        };
        let mut out: Vec<String> = vec![];
        for (_, v) in o.iter() {
            if let Value::Str(s) = v {
                if !out.contains(s) {
                    out.push(s.clone());
                }
            }
        }
        Ok(out)
    }

    /// `_materialize_local_bundles` over bundles already read.
    fn materialize(&self, found: &[Found]) -> LR<usize> {
        if std::fs::metadata(self.pathways_dir()).is_err() {
            return Ok(0);
        }
        let trusted = self.trusted_signers()?;
        let mut have: Vec<String> = self
            .cache
            .read_all(&|_| false)?
            .iter()
            .map(|p| p.id().to_string())
            .collect();
        let mut n = 0;
        for f in found {
            let Some(b) = &f.bundle else { continue };
            let trust = if self.require_signed {
                Some(trusted.as_slice())
            } else {
                None
            };
            let Ok(pw) = bundle::from_bundle(b, trust, self.require_signed) else {
                continue;
            };
            let p = Pathway::new(pw);
            if have.iter().any(|h| h == p.id()) {
                continue;
            }
            self.cache.put(&py_row(&p)?)?;
            have.push(p.id().to_string());
            n += 1;
        }
        Ok(n)
    }

    /// `pull()`: `git pull --ff-only` (a failure tolerated, the store being
    /// offline-tolerant), then the bundles now in the clone put in the
    /// cache. How many were new.
    pub fn pull(&self) -> LR<usize> {
        self.git.run(&self.repo_path, &["pull", "--ff-only"])?;
        let found = read_bundles(&self.pathways_dir())?;
        self.materialize(&found)
    }

    /// `publish(pathway, push=push)`: the bundle signed and written to
    /// pathways/<hash>.yaml, added, committed, pushed when asked (a push
    /// failure tolerated), and the pathway put in the cache. The bundle
    /// hash.
    pub fn publish(&self, p: &Pathway, published_at: f64, push: bool) -> LR<String> {
        let Some(private) = &self.private else {
            return Err(PyError::new(
                "ValueError",
                "GitPathwayStore.publish requires private_key_b64 at construction; the bundle must be signed.",
            )
            .into());
        };
        let b = bundle::to_bundle(
            &p.obj,
            &self.publisher,
            &Value::Float(published_at),
            Some(private),
            self.public.as_deref(),
        )?;
        let text = crate::pathways::yamldump::safe_dump(&Value::Obj(b.clone())).map_err(|u| {
            LErr::Unread(format!(
                "the bundle holds a value this binary does not write as YAML ({})",
                u.0
            ))
        })?;
        let dir = self.pathways_dir();
        crate::cli::gateroot::mkdir_parents(&dir, 0o777).map_err(|e| LErr::io(e, &dir))?;
        let hash = b.value("bundle_hash").as_str().unwrap_or("").to_string();
        let rel = format!("{PATHWAYS_SUBDIR}/{hash}.yaml");
        let full = join(&self.repo_path, &rel);
        std::fs::write(&full, text).map_err(|e| LErr::io(e, &full))?;
        self.git.check(&self.repo_path, &["add", &rel])?;
        let msg = format!(
            "publish pathway {} (bundle {})",
            p.id(),
            &hash[..12.min(hash.len())]
        );
        self.git.check(&self.repo_path, &["commit", "-m", &msg])?;
        if push {
            self.git.run(&self.repo_path, &["push"])?;
        }
        self.cache.put(&py_row(p)?)?;
        Ok(hash)
    }

    /// `status()`: the registry's diagnostic fields, in order.
    pub fn status(&self) -> LR<Object> {
        let commit = match self.git.run(&self.repo_path, &["rev-parse", "HEAD"]) {
            Ok(r) => strip(&r.stdout).to_string(),
            Err(LErr::Py(e)) if e.typ == "FileNotFoundError" => "(git binary missing)".into(),
            Err(e) => return Err(e),
        };
        let names = yaml_names(&self.pathways_dir())?;
        let cached = self.cache.read_all(&|_| false)?;
        let trusted = self.trusted_signers()?;
        Ok(Object::new()
            .with("repo_path", self.repo_path.as_str())
            .with("head_commit", commit)
            .with("bundle_files", names.len() as i64)
            .with("cached_pathways", cached.len() as i64)
            .with("trusted_signers", trusted.len() as i64)
            .with("publisher", self.publisher.as_str())
            .with("signing_configured", self.private.is_some()))
    }
}

/// `sorted(pathways_dir.glob("*.yaml"))`: every entry whose name ends in
/// .yaml, dot names included.
fn yaml_names(dir: &str) -> LR<Vec<String>> {
    if !std::fs::metadata(dir).map(|m| m.is_dir()).unwrap_or(false) {
        return Ok(vec![]);
    }
    let mut out = vec![];
    for e in std::fs::read_dir(dir).map_err(|e| LErr::io(e, dir))? {
        let e = e.map_err(|e| LErr::io(e, dir))?;
        let Some(n) = e.file_name().to_str().map(str::to_string) else {
            return Err(LErr::Unread("a bundle file name is not UTF-8".into()));
        };
        if n.ends_with(".yaml") {
            out.push(n);
        }
    }
    out.sort();
    Ok(out)
}

/// Each bundle file in `dir`, read. YAML this binary does not read is
/// skipped, as the oracle skips a file it cannot parse: a bundle is never
/// admitted on a reading the oracle might not share (L-3).
fn read_bundles(dir: &str) -> LR<Vec<Found>> {
    if std::fs::metadata(dir).is_err() {
        return Ok(vec![]);
    }
    let mut out = vec![];
    for n in yaml_names(dir)? {
        let p = join(dir, &n);
        // read_text raises (a directory, bytes that are not UTF-8), which
        // the oracle catches and skips.
        let text = match std::fs::read(&p)
            .ok()
            .and_then(|r| String::from_utf8(r).ok())
        {
            Some(t) => t,
            None => {
                out.push(Found { bundle: None });
                continue;
            }
        };
        let text = text.replace("\r\n", "\n").replace('\r', "\n");
        // A YAML error, or a value this binary does not model, is skipped.
        let b = crate::pyyaml::load(&text)
            .ok()
            .and_then(|v| crate::pyyaml::to_json(&v))
            .and_then(|v| bundle::validate(&v).ok());
        out.push(Found { bundle: b });
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Box0 {
        root: String,
        env: HashMap<String, String>,
    }

    /// A scratch root whose user git config holds a fixed identity and
    /// branch; the environment has no system config and a fixed identity
    /// for the test's own git runs.
    fn scratch(name: &str) -> Box0 {
        let d = std::env::temp_dir().join(format!("registry-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        let root = std::fs::canonicalize(&d)
            .unwrap()
            .to_string_lossy()
            .into_owned();
        let cfg =
            "[init]\n\tdefaultBranch = main\n[user]\n\tname = t\n\temail = t@example.invalid\n";
        std::fs::write(format!("{root}/.gitconfig"), cfg).unwrap();
        let env: HashMap<String, String> = [
            ("PATH", "/usr/bin:/bin"),
            ("HOME", root.as_str()),
            ("GIT_CONFIG_NOSYSTEM", "1"),
            ("GIT_TERMINAL_PROMPT", "0"),
        ]
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
        Box0 { root, env }
    }

    fn git(b: &Box0, args: &[&str]) -> String {
        let out = Command::new("git")
            .args(args)
            .env_clear()
            .envs(&b.env)
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn pathway() -> Pathway {
        let v = crate::gate::pyjson::loads(
            r#"{"id": "pw_1", "task_description": "deploy", "task_embedding": [0.5, 0.0],
            "envelope": {"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {}},
            "plan_template": {"id": "plan_00000001", "source": "script", "task": "t",
                "steps": [{"id": "s1", "type": "shell", "command": "make"}]},
            "source_trace_ids": [], "distilled_at": 1.5}"#,
        )
        .unwrap();
        match crate::pathways::pmodel::validate_model(
            crate::pathways::pmodel::Id::CompiledPathway,
            &v,
            crate::pathways::pmodel::Mode::Python,
        )
        .unwrap()
        {
            Value::Obj(o) => Pathway::new(o),
            _ => unreachable!(),
        }
    }

    /// An inherited GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE or GIT_CONFIG_*
    /// does not reach git: status reads the clone, publish commits to it,
    /// and the decoy and its hooks are left alone.
    #[test]
    fn git_ignores_an_inherited_git_environment() {
        let b = scratch("hostile");
        let r = &b.root;
        git(&b, &["init", "-q", "--bare", &format!("{r}/remote.git")]);
        git(
            &b,
            &["clone", "-q", &format!("{r}/remote.git"), &format!("{r}/a")],
        );
        git(
            &b,
            &[
                "-C",
                &format!("{r}/a"),
                "commit",
                "-q",
                "--allow-empty",
                "-m",
                "first",
            ],
        );
        git(&b, &["init", "-q", &format!("{r}/decoy")]);
        git(
            &b,
            &[
                "-C",
                &format!("{r}/decoy"),
                "commit",
                "-q",
                "--allow-empty",
                "-m",
                "decoy",
            ],
        );
        let head = git(&b, &["-C", &format!("{r}/a"), "rev-parse", "HEAD"]);
        let decoy = git(&b, &["-C", &format!("{r}/decoy"), "rev-parse", "HEAD"]);
        std::fs::create_dir_all(format!("{r}/hooks")).unwrap();
        // A child writes the hook, so this process holds no write handle
        // to a file git may run (see RF-2).
        let hook = format!("{r}/hooks/pre-commit");
        let w = Command::new("/bin/sh")
            .arg("-c")
            .arg(format!(
                "printf '#!/bin/sh\\ntouch {r}/hook-ran\\n' > {hook} && chmod 755 {hook}"
            ))
            .status()
            .unwrap();
        assert!(w.success());
        let mut hostile = b.env.clone();
        for (k, v) in [
            ("GIT_DIR", format!("{r}/decoy/.git")),
            ("GIT_WORK_TREE", format!("{r}/decoy")),
            ("GIT_INDEX_FILE", format!("{r}/decoy-index")),
            ("GIT_CONFIG_COUNT", "1".into()),
            ("GIT_CONFIG_KEY_0", "core.hooksPath".into()),
            ("GIT_CONFIG_VALUE_0", format!("{r}/hooks")),
        ] {
            hostile.insert(k.into(), v);
        }
        let (private, public) = crate::signing::generate_keypair().unwrap();
        let o = Options {
            private: Some(private),
            public: Some(public),
            require_signed: true,
            environ: hostile,
            ..Default::default()
        };
        let s = Store::open(&format!("{r}/a"), o).unwrap();
        assert_eq!(
            s.status().unwrap().value("head_commit").as_str(),
            Some(head.as_str())
        );
        s.publish(&pathway(), 1600000000.5, false).unwrap();
        assert_eq!(
            git(&b, &["-C", &format!("{r}/decoy"), "rev-parse", "HEAD"]),
            decoy
        );
        assert_ne!(
            git(&b, &["-C", &format!("{r}/a"), "rev-parse", "HEAD"]),
            head
        );
        assert!(std::fs::metadata(format!("{r}/hook-ran")).is_err());
        assert!(std::fs::metadata(format!("{r}/decoy-index")).is_err());
        let _ = std::fs::remove_dir_all(r);
    }

    /// A repo path with no .git of its own inside another repository is
    /// not a registry: git does not climb to the repository above it.
    #[test]
    fn git_does_not_climb_into_a_parent_repo() {
        let b = scratch("climb");
        let r = &b.root;
        git(&b, &["init", "-q", &format!("{r}/outer")]);
        git(
            &b,
            &[
                "-C",
                &format!("{r}/outer"),
                "commit",
                "-q",
                "--allow-empty",
                "-m",
                "first",
            ],
        );
        let head = git(&b, &["-C", &format!("{r}/outer"), "rev-parse", "HEAD"]);
        std::fs::create_dir_all(format!("{r}/outer/reg")).unwrap();
        let (private, public) = crate::signing::generate_keypair().unwrap();
        let o = Options {
            private: Some(private),
            public: Some(public),
            require_signed: true,
            environ: b.env.clone(),
            ..Default::default()
        };
        let s = Store::open(&format!("{r}/outer/reg"), o).unwrap();
        assert_eq!(s.status().unwrap().value("head_commit").as_str(), Some(""));
        assert!(s.publish(&pathway(), 1600000000.5, false).is_err());
        assert_eq!(
            git(&b, &["-C", &format!("{r}/outer"), "rev-parse", "HEAD"]),
            head
        );
        let _ = std::fs::remove_dir_all(r);
    }
}
