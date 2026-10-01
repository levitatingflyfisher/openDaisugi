//! `daisugi registry init|pull|publish|status|pull-and-tend`
//! (`opendaisugi/git_pathway_store.py` and the CLI over it): a team's
//! shared pathway registry as a git repository of signed bundles. The Go
//! client's `cli/registrycmd.go` is the reference.

use std::process::Command;

use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::py::text::{repr, strip};
use crate::gate::pyjson::{dumps, py_str, Value};
use crate::lerr::{LErr, PyError};
use crate::pathways::store::Store as LocalStore;
use crate::registry::{self, called_process_error, look_path, no_git, Options};

const REGISTRY_HELP: &str = "Usage: daisugi registry [OPTIONS] COMMAND [ARGS]...

  The shared pathway registry: a git repository of signed pathway bundles.

Commands:
  init           Clone a registry repo to a local directory.
  pull           git pull and materialize new pathway bundles into the local cache.
  publish        Sign + commit + push a local pathway as a bundle to the registry.
  status         Show the local clone's diagnostic info.
  pull-and-tend  Cron-friendly: pull new bundles from the registry, then run tend.
";

const REPO_PATH: Opt = Opt::val(&["--repo-path"], "PATH", "The registry's local clone.");
const DATA_DIR: Opt = Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.");

impl Env {
    pub(super) fn registry_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(REGISTRY_HELP);
            return Ok(());
        }
        let rest = &args[1..];
        match args[0].as_str() {
            "init" => self.registry_init(rest),
            "pull" => self.registry_pull(rest),
            "publish" => self.registry_publish(rest),
            "status" => self.registry_status(rest),
            "pull-and-tend" => self.registry_pull_and_tend(rest),
            other => {
                self.errf(&format!(
                    "Usage: daisugi registry [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi registry --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// Ends a command the way the oracle ends one that raised: exit 1, the
    /// exception's qualified type and its text on stderr. Input this
    /// binary does not read the way the oracle does is refused instead.
    pub(super) fn raise(&mut self, cmd: &str, e: LErr) -> Res {
        match e {
            LErr::Unread(why) => self.refuse(cmd, &why),
            LErr::Pw(pw) => Err(self.pw_err(cmd, pw)),
            LErr::Invalid(v) => {
                let text = v.text();
                let first = text.split('\n').next().unwrap_or("");
                self.errf(&format!(
                    "daisugi {cmd}: pydantic_core._pydantic_core.ValidationError: {first}\n"
                ));
                exit(1)
            }
            other => {
                let (t, m) = other.py();
                let ctx = match &other {
                    LErr::Py(p) if !p.context.is_empty() => {
                        format!("{}; during its handling, ", p.context)
                    }
                    _ => String::new(),
                };
                if m.is_empty() {
                    self.errf(&format!("daisugi {cmd}: {ctx}{t}\n"));
                } else {
                    self.errf(&format!("daisugi {cmd}: {ctx}{t}: {m}\n"));
                }
                exit(1)
            }
        }
    }

    fn default_registry(&self) -> String {
        join(&self.data_home(), "registry")
    }

    fn registry_init(&mut self, args: &[String]) -> Res {
        const CMD: &str = "registry init";
        let opts = [Opt::val(&["--clone-to"], "PATH", "Local clone directory.")];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "GIT_URL", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " GIT_URL",
                "Clone a registry repo to a local directory.",
                &opts,
            );
        }
        let Some(url) = p.args.first().cloned() else {
            return self.usage_args(CMD, "GIT_URL", "Missing argument 'git_url'.");
        };
        let clone_to = path_str(&p.str("--clone-to", &self.default_registry()));
        // git runs a command for an ext:: or fd:: URL at clone time.
        if url.starts_with("ext::") || url.starts_with("fd::") {
            self.errf(&format!(
                "refusing registry URL {}: the git ext::/fd:: transports execute a command and are not allowed.\n",
                repr(&url)
            ));
            return exit(2);
        }
        if std::fs::metadata(&clone_to).is_ok()
            && std::fs::metadata(join(&clone_to, ".git")).is_ok()
        {
            self.out(&format!("already cloned at {clone_to}\n"));
            return Ok(());
        }
        let parent = crate::supervise::executors::py_dirname(&clone_to);
        if !parent.is_empty() {
            if let Err(e) = mkdir_parents_py(&parent) {
                return self.raise(CMD, e);
            }
        }
        self.out(&format!("cloning {url} → {clone_to}\n"));
        let path = self.env.get("PATH").cloned().unwrap_or_default();
        let Some(git) = look_path("git", &path) else {
            return self.raise(CMD, no_git().into());
        };
        // The output goes through as it comes, after what was printed.
        self.flush();
        // No inherited GIT_* variable is passed on; the allowed protocols
        // are enforced in git itself too (submodules, and any transport
        // not caught above).
        let status = Command::new(&git)
            .arg("clone")
            .arg(&url)
            .arg(&clone_to)
            .env_clear()
            .envs(registry::scrubbed(&self.env))
            .env("GIT_ALLOW_PROTOCOL", "https:http:ssh:git:file")
            .status();
        match status {
            Ok(s) if s.success() => {}
            Ok(s) => {
                let args = ["git".to_string(), "clone".into(), url, clone_to];
                return self.raise(
                    CMD,
                    called_process_error(&args, s.code().unwrap_or(-1)).into(),
                );
            }
            Err(e) => return self.raise(CMD, LErr::io(e, &git)),
        }
        self.out(&format!("clone ready at {clone_to}\n"));
        Ok(())
    }

    fn open_registry(
        &mut self,
        cmd: &str,
        repo: &str,
        mut o: Options,
    ) -> Result<registry::Store, Stop> {
        o.environ = self.env.clone();
        match registry::Store::open(repo, o) {
            Ok(s) => Ok(s),
            Err(e) => Err(self.raise(cmd, e).unwrap_err()),
        }
    }

    fn registry_pull(&mut self, args: &[String]) -> Res {
        const CMD: &str = "registry pull";
        let opts = [
            REPO_PATH,
            Opt::pair(
                &["--require-signed"],
                "--allow-unsigned",
                "Refuse bundles without a valid signature from a trusted signer.",
            ),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "git pull and materialize new pathway bundles into the local cache.",
                &opts,
            );
        }
        let require = !p.flag_set("--require-signed") || p.flag("--require-signed");
        let repo = p.str("--repo-path", &self.default_registry());
        let s = self.open_registry(
            CMD,
            &repo,
            Options {
                require_signed: require,
                ..Default::default()
            },
        )?;
        match s.pull() {
            Ok(n) => {
                self.out(&format!("pulled; {n} new pathway(s) cached\n"));
                Ok(())
            }
            Err(e) => self.raise(CMD, e),
        }
    }

    fn registry_publish(&mut self, args: &[String]) -> Res {
        const CMD: &str = "registry publish";
        let opts = [
            REPO_PATH,
            Opt::val(
                &["--private-key"],
                "PATH",
                "Path to a base64 ed25519 private key file.",
            ),
            Opt::val(
                &["--public-key"],
                "PATH",
                "Path to a base64 ed25519 public key file.",
            ),
            Opt::val(
                &["--publisher"],
                "TEXT",
                "Human-readable publisher id stamped on the bundle.",
            ),
            Opt::pair(&["--push"], "--no-push", "Push after the commit."),
            DATA_DIR,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "PATHWAY_ID", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " PATHWAY_ID",
                "Sign + commit + push a local pathway as a bundle to the registry.",
                &opts,
            );
        }
        let Some(id) = p.args.first().cloned() else {
            return self.usage_args(CMD, "PATHWAY_ID", "Missing argument 'pathway_id'.");
        };
        for o in ["--private-key", "--public-key"] {
            if !p.has(o) {
                return self.usage_args(CMD, "PATHWAY_ID", &format!("Missing option '{o}'."));
            }
        }
        let private = match read_text_strip(&path_str(&p.str("--private-key", ""))) {
            Ok(t) => t,
            Err(e) => return self.raise(CMD, e),
        };
        let public = match read_text_strip(&path_str(&p.str("--public-key", ""))) {
            Ok(t) => t,
            Err(e) => return self.raise(CMD, e),
        };
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        let found = (|| -> Result<Option<crate::pathways::pathway::Pathway>, LErr> {
            let local = LocalStore::open(&join(&data_dir, "pathways.db"))?;
            let all = local.read_all(&|x| x == id)?;
            Ok(all.into_iter().find(|x| x.id() == id).map(|x| x.full()))
        })();
        let pw = match found {
            Ok(Some(pw)) => pw,
            Ok(None) => {
                self.errf(&format!("error: no pathway {id} in local store\n"));
                return exit(1);
            }
            Err(e) => return self.raise(CMD, e),
        };
        let o = Options {
            private: Some(private),
            public: Some(public),
            publisher: Some(p.str("--publisher", "opendaisugi-instance")),
            require_signed: true,
            ..Default::default()
        };
        let s = self.open_registry(CMD, &p.str("--repo-path", &self.default_registry()), o)?;
        let push = !p.flag_set("--push") || p.flag("--push");
        match s.publish(&pw, crate::tracejournal::runs::now(), push) {
            Ok(h) => {
                self.out(&format!("{h}\n"));
                Ok(())
            }
            Err(e) => self.raise(CMD, e),
        }
    }

    fn registry_status(&mut self, args: &[String]) -> Res {
        const CMD: &str = "registry status";
        let opts = [
            REPO_PATH,
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(CMD, "", "Show the local clone's diagnostic info.", &opts);
        }
        let repo = p.str("--repo-path", &self.default_registry());
        let s = self.open_registry(
            CMD,
            &repo,
            Options {
                require_signed: true,
                ..Default::default()
            },
        )?;
        let st = match s.status() {
            Ok(st) => st,
            Err(e) => return self.raise(CMD, e),
        };
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps(&Value::Obj(st), true)));
            return Ok(());
        }
        for (k, v) in st.iter() {
            let text = py_str(v).unwrap_or_default();
            self.out(&format!("  {k}: {text}\n"));
        }
        Ok(())
    }

    fn registry_pull_and_tend(&mut self, args: &[String]) -> Res {
        const CMD: &str = "registry pull-and-tend";
        let opts = [REPO_PATH, DATA_DIR];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Cron-friendly: pull new bundles from the registry, then run tend.",
                &opts,
            );
        }
        let repo = p.str("--repo-path", &self.default_registry());
        let s = self.open_registry(
            CMD,
            &repo,
            Options {
                require_signed: true,
                ..Default::default()
            },
        )?;
        let n = match s.pull() {
            Ok(n) => n,
            Err(e) => return self.raise(CMD, e),
        };
        drop(s);
        self.out(&format!("pulled; {n} new pathway(s) cached\n"));
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        if let Some(rep) = self.run_tend_quiet(&data_dir)? {
            self.out(&format!(
                "tend: created={} updated={} skipped={}\n",
                rep.rep.created, rep.rep.updated, rep.rep.skipped
            ));
        }
        Ok(())
    }
}

/// `Path.mkdir(parents=True, exist_ok=True)`: an existing file at the path
/// is FileExistsError.
pub(super) fn mkdir_parents_py(d: &str) -> Result<(), LErr> {
    if let Ok(m) = std::fs::metadata(d) {
        if m.is_dir() {
            return Ok(());
        }
        return Err(PyError::new(
            "FileExistsError",
            format!("[Errno 17] File exists: {}", repr(d)),
        )
        .into());
    }
    std::fs::create_dir_all(d).map_err(|e| LErr::io(e, d))
}

/// `path.read_text(encoding="utf-8").strip()`.
pub(super) fn read_text_strip(path: &str) -> Result<String, LErr> {
    Ok(strip(&read_text(path)?).to_string())
}

/// `path.read_text()`: the file's text, which must be UTF-8.
pub(super) fn read_text(path: &str) -> Result<String, LErr> {
    let raw = std::fs::read(path).map_err(|e| LErr::io(e, path))?;
    String::from_utf8(raw).map_err(|_| {
        PyError::new(
            "UnicodeDecodeError",
            format!("'utf-8' codec can't decode the file {path}"),
        )
        .into()
    })
}
