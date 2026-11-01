//! `daisugi warden init|register|arm|disarm|status|report|settings|proposals`.

use super::config::{self, ConfigErr};
use super::envelope::{self, Bad};
use super::wardenroot::{self, join, parent, path_str};
use super::install::{self, HookOptions};
use super::journal::{self, JournalErr};
use super::words::KIND_UNKNOWN;
use super::yaml::{self, Kind, Node};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::warden::pyjson::{dumps, dumps_indent, loads, LoadError, Object, Value};

const ROOT_OPT: Opt = Opt::val(&["--root"], "PATH", "Warden state directory (envelopes, audit log, disarm marker).");
const JSON_OPT: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");
pub(super) const DECOMPOSE_OPT: Opt = Opt::pair(
    &["--allow-shell-decomposition"],
    "--no-allow-shell-decomposition",
    "Let the envelope admit compound shell: 'a && b' and pipes, every head checked. Unset reads shell_allow_decomposition from config.yaml.",
);

impl Env {
    fn root(&self, p: &super::Parsed) -> String {
        if p.has("--root") {
            return path_str(&p.str("--root", ""));
        }
        join(&self.data_home(), "warden")
    }

    /// `Path.cwd()`, or home when the directory is gone.
    pub(super) fn cwd(&self) -> String {
        wardenroot::getwd().unwrap_or_else(|_| self.home.clone())
    }

    pub(super) fn warden_disarm(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden disarm", m),
        };
        if p.help {
            return self.cmd_help("warden disarm", "", "Kill switch: the warden allows everything until re-armed.", &opts);
        }
        let (marker, res) = wardenroot::disarm(&self.root(&p));
        if let Err(e) = res {
            if let Some(text) = crate::warden::statewrite::write_refused(&e) {
                self.errf(&format!("warden NOT disarmed: {text}\n"));
                return exit(1);
            }
            return self.fail("warden disarm", &e.to_string());
        }
        self.out(&format!("warden DISARMED (marker: {marker}) — `daisugi warden arm` to re-enable\n"));
        Ok(())
    }

    pub(super) fn warden_arm(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden arm", m),
        };
        if p.help {
            return self.cmd_help("warden arm", "", "Remove the disarm marker; the warden resumes evaluating calls.", &opts);
        }
        if let Err(e) = wardenroot::arm(&self.root(&p)) {
            if let Some(text) = crate::warden::statewrite::write_refused(&e) {
                self.errf(&format!("warden NOT armed: {text}\n"));
                return exit(1);
            }
            return self.fail("warden arm", &e.to_string());
        }
        self.out("warden armed\n");
        Ok(())
    }

    pub(super) fn warden_status(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT, JSON_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden status", m),
        };
        if p.help {
            return self.cmd_help(
                "warden status",
                "",
                "Show armed/disarmed state, the verdict mode, and the registered envelopes.",
                &opts,
            );
        }
        let root = self.root(&p);
        let armed = !wardenroot::is_disarmed(&root);
        let cwd = self.cwd();
        let eff = match config::effective_hook_mode(&self.home, &cwd) {
            Ok(e) => e,
            Err(_) => {
                return self.refuse("warden status", "a Claude Code settings.json holds hooks this binary does not read yet")
            }
        };
        let (mut mode, mut source) = (eff.mode.clone(), config::source_label(&eff).to_string());
        if mode.is_empty() {
            mode = match config::warden_mode(&join(&parent(&root), "config.yaml")) {
                Ok(m) => m,
                Err(e) => return self.refuse("warden status", &e.to_string()),
            };
            source = "config".into();
        }
        let envs = match wardenroot::envelopes(&root) {
            Ok(e) => e,
            Err(why) => return self.refuse("warden status", &why),
        };
        // A hook whose program is gone fails on every call: an enforce hook
        // then denies them all. Say so on stderr, whatever the output format.
        let mut files = vec![format!("{}/.claude/settings.json", self.home)];
        let c = format!("{cwd}/.claude/settings.json");
        if c != files[0] {
            files.push(c);
        }
        let mut gone: Vec<String> = files
            .iter()
            .flat_map(|f| config::missing_hook_programs(f).into_iter().map(move |g| config::missing_hook_warning(f, &g)))
            .collect();
        gone.extend(files.iter().filter(|f| config::stale_mode_hooks(f)).map(|f| config::stale_mode_warning(f)));
        // An old config.yaml may name a verifier client this box lacks (a
        // restored Python-era config). The warden still decides; say so once.
        if let Ok(cfg) = config::load(&join(&parent(&root), "config.yaml")) {
            let msg = crate::warden::dispatch::client_problem(&cfg.verifier_client, &self.env);
            if !msg.is_empty() {
                gone.push(msg);
            }
        }
        // cli._warden_dialect_status: the dialect's hash, the pin config.yaml
        // names, the mode that follows, and the calls a word would deny.
        let pin = match config::dialect_pin(&join(&parent(&root), "config.yaml")) {
            Ok(p) => p,
            Err(e) => return self.refuse("warden status", &e.to_string()),
        };
        let word_would_deny = match super::journal::word_would_deny(&root) {
            Ok(n) => n,
            Err(e) => return self.refuse("warden status", &e.to_string()),
        };
        let hash = crate::dialect::DIALECT_HASH;
        let (dialect_mode, dialect_source) = match &pin {
            Some(p) if p != hash => ("mismatch", "pin"),
            Some(_) => ("enforced", "pin"),
            None if mode == "enforce" => ("enforced", "warden_mode"),
            None => ("audit", "none"),
        };
        let dialect = Object::new()
            .with("hash", hash)
            .with("pin", pin.clone().map(Value::Str).unwrap_or(Value::Null))
            .with("mode", dialect_mode)
            .with("source", dialect_source)
            .with("word_would_deny", word_would_deny as i64);
        let hints = self.rename_hints();
        // Each path the warden refuses to read, with the fix (GR-R-8); the key
        // and the lines appear only when there is one.
        let refused = crate::warden::statewrite::refused_paths(&root);
        let open_files = self.unconfined(&root);
        if p.flag("--json") {
            let mut o = Object::new()
                .with("armed", armed)
                .with("mode", mode.as_str())
                .with("mode_source", source.as_str())
                .with("envelopes", Value::List(envs.iter().map(|s| Value::Str(s.clone())).collect()))
                .with("dialect", dialect);
            if !refused.is_empty() {
                o.set("refused", Value::List(refused.iter().map(|s| Value::Str(s.clone())).collect()));
            }
            if open_files {
                o.set("unconfined", true);
            }
            self.out(&format!("{}\n", dumps(&Value::Obj(o), true)));
            for g in gone.iter().chain(hints.iter()) {
                self.errf(&format!("{g}\n"));
            }
            return Ok(());
        }
        let state = if armed { "armed" } else { "DISARMED" };
        // A warden hook in a form this CLI does not read. It may enforce.
        let shown = if mode == KIND_UNKNOWN { "unknown warden hook".to_string() } else { mode };
        self.out(&format!("warden: {state} · mode: {shown} ({source})\n"));
        if envs.is_empty() {
            self.out("no envelopes registered — enforce mode would deny everything\n");
        }
        for name in envs {
            self.out(&format!("  envelope: {name}\n"));
        }
        for line in &refused {
            self.out(&format!("refused: {line}\n"));
        }
        let head = match (&pin, dialect_mode, dialect_source) {
            (Some(p), "mismatch", _) => format!(
                "dialect {hash}: pin mismatch: {p} is not this build's {hash}; pin is from an earlier dialect; \
                 set dialect_enforce: {hash} or remove it"
            ),
            (_, _, "pin") => format!("dialect {hash}: enforced (pinned)"),
            (_, "enforced", _) => format!("dialect {hash}: enforced (warden mode enforce)"),
            _ => format!("dialect {hash}: audit (warden mode audit)"),
        };
        self.out(&format!("{head} · word would-denies: {word_would_deny}\n"));
        if open_files {
            self.out(&format!("{}\n", super::fencenotice::UNCONFINED_LINE));
        }
        for h in &hints {
            self.out(&format!("{h}\n"));
        }
        for g in &gone {
            self.errf(&format!("{g}\n"));
        }
        Ok(())
    }

    /// `cli._rename_hints`: lines for state that still carries a name from
    /// before the rename, the old data directory and each old config key.
    /// `daisugi install` moves them.
    fn rename_hints(&self) -> Vec<String> {
        let data = self.data_home();
        let mut out = vec![];
        let old = format!("{data}/gate"); // names-ok: old dir
        if std::fs::metadata(&old).map(|m| m.is_dir()).unwrap_or(false)
            && std::fs::metadata(format!("{data}/warden")).is_err()
        {
            out.push(format!("found {old} from before the rename: run daisugi install to move it"));
        }
        for (old, new) in config::stale_keys(&format!("{data}/config.yaml")) {
            out.push(format!("config: unknown key {old} (now {new})"));
        }
        out
    }

    pub(super) fn warden_report(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--session"], "TEXT", "One session's log only."),
            ROOT_OPT,
            Opt::flag(&["--json"], "Emit the full report as JSON."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden report", m),
        };
        if p.help {
            return self.cmd_help(
                "warden report",
                "",
                "Summarize the audit log: what an enforcing warden would have denied.",
                &opts,
            );
        }
        let session = p.has("--session").then(|| p.str("--session", ""));
        let res = journal::files(&self.root(&p), session.as_deref()).and_then(|f| journal::build(&f)).and_then(|rep| {
            if p.flag("--json") {
                Ok(format!("{}\n", rep.json()))
            } else {
                rep.text()
            }
        });
        match res {
            Ok(text) => {
                self.out(&text);
                Ok(())
            }
            Err(e @ JournalErr::Crash(_)) => self.fail("warden report", &e.to_string()),
            Err(e) => self.refuse("warden report", &e.to_string()),
        }
    }

    pub(super) fn warden_settings(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::flag(&["--enforce"], "Emit enforce-mode settings (default is audit: observation only)."),
            ROOT_OPT,
            Opt::val(&["--format"], "TEXT", "Host contract (default claude)."),
            Opt::val(&["--session"], "TEXT", "Pin the warden to this registered session's envelope."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden settings", m),
        };
        if p.help {
            return self.cmd_help(
                "warden settings",
                "",
                "Print the Claude Code hooks-settings JSON that wires in the warden. Usage: claude --settings \"$(daisugi warden settings)\"",
                &opts,
            );
        }
        let me = match self.self_path() {
            Ok(s) => s,
            Err(e) => return self.fail("warden settings", &e),
        };
        let o = HookOptions {
            mode: if p.flag("--enforce") { "enforce".into() } else { "audit".into() },
            root: self.root(&p),
            format: p.str("--format", "claude"),
            captures_root: None,
            session: p.has("--session").then(|| p.str("--session", "")),
            ask: false,
        };
        self.out(&format!("{}\n", install::settings_json(&me, &o)));
        Ok(())
    }

    /// The path an installed hook runs: this binary as it was started.
    pub(super) fn self_path(&self) -> Result<String, String> {
        let arg0 = std::env::args().next().unwrap_or_default();
        install::self_path(&arg0, self.env.get("PATH").map(String::as_str).unwrap_or("")).map_err(|e| e.to_string())
    }

    /// `install.daisugi_exe`: the program an installed hook or MCP entry runs.
    pub(super) fn entry_path(&self) -> Result<String, String> {
        let arg0 = std::env::args().next().unwrap_or_default();
        install::entry_path(&arg0, self.env.get("PATH").map(String::as_str).unwrap_or("")).map_err(|e| e.to_string())
    }

    /// `cli._resolve_decompose` for an unset flag.
    pub(super) fn decompose_default(&mut self, root: &str, cmd: &str) -> Result<bool, super::Stop> {
        match config::load(&join(&parent(root), "config.yaml")) {
            Ok(c) => Ok(c.shell_allow_decomposition),
            Err(e @ (ConfigErr::Invalid | ConfigErr::Yaml(_))) => Err(self.fail(cmd, &e.to_string()).unwrap_err()),
            Err(e) => Err(self.refuse(cmd, &e.to_string()).unwrap_err()),
        }
    }

    pub(super) fn warden_init(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--workspace"], "PATH", "The directory the session may read/write (default: cwd)."),
            Opt::val(&["--session"], "TEXT", "Bind the envelope to this session id."),
            ROOT_OPT,
            Opt::flag(&["--force"], "Overwrite an existing envelope."),
            DECOMPOSE_OPT,
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden init", m),
        };
        if p.help {
            return self.cmd_help("warden init", "", "Generate and register a reviewable starter envelope for this session.", &opts);
        }
        let root = self.root(&p);
        let decompose = if p.flag_set("--allow-shell-decomposition") {
            p.flag("--allow-shell-decomposition")
        } else {
            self.decompose_default(&root, "warden init")?
        };
        let session = p.str("--session", "");
        let name = if session.is_empty() { "default".to_string() } else { session.clone() };
        // The file register writes: the session id made safe, so the check
        // and the write look at one path.
        let file = if session.is_empty() { "default".to_string() } else { wardenroot::safe_session_id(&session) };
        let target = join(&wardenroot::envelopes_dir(&root), &format!("{file}.json"));
        if wardenroot::exists(&target) && !p.flag("--force") {
            self.errf(&format!(
                "an envelope for '{name}' is already registered at {target}; pass --force to overwrite\n"
            ));
            return exit(1);
        }
        let ws = if p.has("--workspace") {
            p.str("--workspace", "")
        } else {
            match wardenroot::getwd() {
                Ok(w) => w,
                Err(e) => return self.fail("warden init", &e.to_string()),
            }
        };
        let ws = match wardenroot::resolve(&ws) {
            Ok(w) => w,
            Err(e) => return self.fail("warden init", &e.to_string()),
        };
        let env = match envelope::starter(&ws, decompose) {
            Ok(e) => e,
            Err(e) => return self.fail("warden init", &e.to_string()),
        };
        let (path, res) = envelope::register(&env, &session, &root);
        if let Err(e) = res {
            if let Some(text) = crate::warden::statewrite::write_refused(&e) {
                self.errf(&format!("not registered: {text}\n"));
                return exit(1);
            }
            return self.fail("warden init", &e.to_string());
        }
        self.out(&format!("registered a starter envelope for {ws} → {path}\n"));
        self.out("REVIEW it before enforcing — it is a tight default, not a finished policy.\n");
        self.out("Then launch audit mode:\n");
        // A session's envelope is read only by a warden pinned to it.
        let pin = if session.is_empty() {
            String::new()
        } else {
            format!(" --session {}", crate::interpreter_parse::shlex_quote(&session))
        };
        self.out(&format!("  claude --settings \"$(daisugi warden settings --root {root}{pin})\"\n"));
        Ok(())
    }

    pub(super) fn warden_register(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--session"], "TEXT", "Bind to one session id; omit to register the default envelope."),
            ROOT_OPT,
            Opt::val(
                &["--parent"],
                "TEXT",
                "Register a child of this session: the edge to it is proved first, and a refused edge registers nothing.",
            ),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage("warden register", m),
        };
        if p.help {
            return self.cmd_help(
                "warden register",
                " ENVELOPE_PATH",
                "Register the envelope the warden checks this session's calls against.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage("warden register", "Missing argument 'ENVELOPE_PATH'.".into());
        }
        let file = p.args[0].clone();
        let raw = match std::fs::read(&file) {
            Ok(r) => r,
            Err(e) => return self.fail("warden register", &e.to_string()),
        };
        let text = match String::from_utf8(raw) {
            Ok(t) => t,
            Err(_) => return self.fail("warden register", &format!("{file} is not valid UTF-8")),
        };
        // yaml.safe_load reads the file, JSON included, as YAML 1.1: to it
        // 1e-09 is a string, not a number.
        let y = match yaml::parse(&text) {
            Ok(y) => y,
            // safe_load raises: the oracle's traceback names the exception.
            Err(e @ yaml::YErr::Exc(_)) => return self.fail("warden register", &e.traceback_line()),
            Err(_) => return self.refuse("warden register", "the envelope uses YAML this binary does not read yet"),
        };
        let v = match yaml_to_json(&y) {
            Ok(v) => v,
            Err(why) => return self.refuse("warden register", &why),
        };
        let input = match v {
            Value::Obj(o) => o,
            _ => return self.fail("warden register", "the envelope file does not hold a mapping"),
        };
        let env = match envelope::validate(&input) {
            Ok(e) => e,
            Err(e @ Bad::Invalid(_)) => return self.fail("warden register", &e.to_string()),
            Err(e) => return self.refuse("warden register", &e.to_string()),
        };
        let unchecked = opaque_unrecognized(&env);
        if !unchecked.is_empty() {
            self.errf(&format!(
                "envelope declares {} item(s) nothing can check: {}. Give each an expr, name a word (keep_unchanged), \
                 or set enforce: false.\n",
                unchecked.len(),
                unchecked.join(", ")
            ));
            return exit(2);
        }
        if self.lint_mcp_rules(&env, &self.root(&p)) {
            return exit(2);
        }
        let session = p.str("--session", "");
        let env = if p.has("--parent") {
            let root = self.root(&p);
            match self.proved_child(&env, &session, &p.str("--parent", ""), &root) {
                Ok(e) => e,
                Err(stop) => return Err(stop),
            }
        } else {
            env
        };
        let (path, res) = envelope::register(&env, &session, &self.root(&p));
        if let Err(e) = res {
            if let Some(text) = crate::warden::statewrite::write_refused(&e) {
                self.errf(&format!("not registered: {text}\n"));
                return exit(1);
            }
            return self.fail("warden register", &e.to_string());
        }
        let which = if session.is_empty() { "default".to_string() } else { format!("session {session}") };
        self.out(&format!("registered {which} envelope → {path}\n"));
        Ok(())
    }

    /// `cli._lint_mcp_rules`: notes go to stderr; a refused rule prints one
    /// line each and registers nothing. True when something was refused.
    fn lint_mcp_rules(&mut self, env: &Object, root: &str) -> bool {
        let rules = match env.value("permissions").as_obj().map(|p| p.value("mcp_rules")) {
            Some(Value::List(l)) if !l.is_empty() => l.clone(),
            _ => return false,
        };
        let mut warns = Vec::new();
        let schemas = super::mcpschemas::load_schemas(root, &mut |s| warns.push(s));
        let perms = env.value("permissions").clone();
        let (refusals, notes) = super::mcpschemas::lint_rules(&rules, &schemas, Some(&perms));
        for w in warns.into_iter().chain(notes) {
            self.errf(&format!("{w}\n"));
        }
        for r in &refusals {
            self.errf(&format!("not registered: {r}\n"));
        }
        !refusals.is_empty()
    }

    /// `daisugi warden schemas import|list`.
    pub(super) fn warden_schemas(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out("Usage: daisugi warden schemas [OPTIONS] COMMAND [ARGS]...\n\n  The MCP tool schemas on file, which `warden register` checks mcp_rules\n  against.\n\nCommands:\n  import  Store a tools/list result as the schema of one MCP server.\n  list    Print the schemas on file.\n");
            return Ok(());
        }
        let opts = [ROOT_OPT];
        match args[0].as_str() {
            "import" => {
                let p = match parse_args(&args[1..], &opts, 2) {
                    Ok(p) => p,
                    Err(m) => return self.usage("warden schemas import", m),
                };
                if p.help {
                    return self.cmd_help(
                        "warden schemas import",
                        " SERVER FILE",
                        "Store a tools/list result as the schema of one MCP server.",
                        &opts,
                    );
                }
                if p.args.len() < 2 {
                    let name = if p.args.len() == 1 { "FILE" } else { "SERVER" };
                    return self.usage("warden schemas import", format!("Missing argument '{name}'."));
                }
                let root = self.root(&p);
                match super::mcpschemas::import_schema(&root, &p.args[0], &p.args[1]) {
                    Ok((path, count)) => {
                        self.out(&format!("stored the schema of {} ({count} tools) at {path}\n", p.args[0]));
                        Ok(())
                    }
                    Err(e) => {
                        self.errf(&format!("daisugi warden schemas import: {}\n", e.0));
                        exit(2)
                    }
                }
            }
            "list" => {
                let p = match parse_args(&args[1..], &opts, 0) {
                    Ok(p) => p,
                    Err(m) => return self.usage("warden schemas list", m),
                };
                if p.help {
                    return self.cmd_help("warden schemas list", "", "Print the schemas on file.", &opts);
                }
                let root = self.root(&p);
                let mut warns = Vec::new();
                let schemas = super::mcpschemas::load_schemas(&root, &mut |s| warns.push(s));
                for w in warns {
                    self.errf(&format!("{w}\n"));
                }
                if schemas.is_empty() {
                    self.out("no schemas on file; import one with: daisugi warden schemas import <server> <tools-list.json>\n");
                    return Ok(());
                }
                for s in &schemas {
                    let n = match s.obj.value("tools") {
                        Value::List(l) => l.len(),
                        _ => 0,
                    };
                    self.out(&format!(
                        "{}  {n} tools  {}\n",
                        s.server,
                        wardenroot::join(&wardenroot::join(&root, "schemas"), &format!("{}.json", s.server))
                    ));
                }
                Ok(())
            }
            other => {
                self.errf(&format!(
                    "Usage: daisugi warden schemas [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi warden schemas --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// `cli._proved_child`: the child as it registers once the edge from
    /// `parent` is proved, or a one-line refusal and exit 1.
    fn proved_child(&mut self, env: &Object, session: &str, parent: &str, root: &str) -> Result<Object, Stop> {
        let mut refuse = |why: String| {
            self.errf(&format!("not registered: {why}\n"));
            Stop::Exit(1)
        };
        if parent.is_empty() {
            return Err(refuse("--parent needs a session id".into()));
        }
        if session.is_empty() {
            return Err(refuse("--parent needs --session; the starter names the child's session".into()));
        }
        if !crate::tree::valid_session(session) {
            return Err(refuse(format!("the child's session id {} is not one the tree takes", crate::tree::q(&Value::Str(session.into())))));
        }
        let target = super::wardenroot::join(&super::wardenroot::envelopes_dir(root), &format!("{}.json", super::wardenroot::safe_session_id(session)));
        if std::fs::metadata(&target).is_ok() {
            return Err(refuse(format!("session {session} is registered already")));
        }
        let penv = crate::tree::load_registered(parent, root);
        let res = crate::tree::edge_ok(penv.as_ref(), Some(env), crate::tree::DEFAULT_TIMEOUT_MS);
        if !res.holds {
            return Err(refuse(format!("the edge from {parent} to {session} is refused: {}", res.reasons.join("; "))));
        }
        let child = res.child.expect("the edge holds");
        let id = penv.expect("the edge holds, so the parent has an envelope").value("id").clone();
        Ok(crate::tree::with_key(&child, "parent_envelope", id))
    }

    pub(super) fn warden_proposals(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT, Opt::flag(&["--json"], "Emit proposals as a JSON array.")];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("warden proposals", m),
        };
        if p.help {
            return self.cmd_help(
                "warden proposals",
                "",
                "List recorded envelope-edit proposals. A proposal is a file; nobody applies it automatically.",
                &opts,
            );
        }
        let d = join(&self.root(&p), "proposals");
        let mut props: Vec<Object> = vec![];
        if wardenroot::is_dir(&d) {
            let entries = match std::fs::read_dir(&d) {
                Ok(e) => e,
                Err(e) => return self.fail("warden proposals", &e.to_string()),
            };
            let mut names = vec![];
            for en in entries {
                let en = match en {
                    Ok(en) => en,
                    Err(e) => return self.fail("warden proposals", &e.to_string()),
                };
                let name = match en.file_name().into_string() {
                    Ok(n) => n,
                    Err(_) => return self.refuse("warden proposals", wardenroot::ERR_NAME),
                };
                if name.ends_with(".json") {
                    names.push(name);
                }
            }
            names.sort();
            for n in names {
                let text = match std::fs::read(join(&d, &n)).ok().and_then(|r| String::from_utf8(r).ok()) {
                    Some(t) => t,
                    None => continue,
                };
                match loads(&text) {
                    Err(LoadError::Unsupported) => {
                        return self.refuse("warden proposals", "input outside the modeled subset")
                    }
                    Ok(Value::Obj(o)) => props.push(o),
                    _ => {}
                }
            }
        }
        if p.flag("--json") {
            let l = Value::List(props.into_iter().map(Value::Obj).collect());
            self.out(&format!("{}\n", dumps_indent(&l, 2, true)));
            return Ok(());
        }
        if props.is_empty() {
            self.out("(no pending proposals)\n");
            return Ok(());
        }
        for o in props {
            let mut f: Vec<String> = vec![];
            for k in ["id", "kind", "scope", "expiresAt"] {
                match o.get(k) {
                    None => f.push("?".into()),
                    Some(v) => match plain_str(v) {
                        Some(s) => f.push(s),
                        None => {
                            return self
                                .refuse("warden proposals", "a proposal holds a value this binary does not print yet")
                        }
                    },
                }
            }
            self.out(&format!("{}  [{}]  scope={}  expires={}\n", f[0], f[1], f[2], f[3]));
        }
        Ok(())
    }
}

/// `str(v)` for the JSON values whose str is plain.
fn plain_str(v: &Value) -> Option<String> {
    Some(match v {
        Value::Null => "None".into(),
        Value::Bool(true) => "True".into(),
        Value::Bool(false) => "False".into(),
        Value::Int(t) => t.clone(),
        Value::Str(s) => s.clone(),
        _ => return None,
    })
}

/// A parsed YAML node as the values json.loads would give for the same
/// data. A mapping with a key that is not a string is refused: pydantic's
/// handling of it is not modelled.
pub(super) fn yaml_to_json(v: &Node) -> Result<Value, String> {
    Ok(match v.kind {
        Kind::Null => Value::Null,
        Kind::Bool => Value::Bool(v.b),
        Kind::Int => loads(&v.text).map_err(|_| "an integer this binary does not read yet".to_string())?,
        Kind::Float => Value::Float(v.text.parse::<f64>().map_err(|e| e.to_string())?),
        Kind::Str => Value::Str(v.text.clone()),
        Kind::Other => return Err("the envelope holds a date".into()),
        Kind::Seq => Value::List(v.items.iter().map(yaml_to_json).collect::<Result<Vec<_>, _>>()?),
        Kind::Map => {
            if v.non_str_keys > 0 {
                return Err("the envelope has a mapping key that is not a string".into());
            }
            let mut o = Object::new();
            for k in &v.keys {
                o.set(k, yaml_to_json(&v.map[k])?);
            }
            Value::Obj(o)
        }
    })
}

/// `models.opaque_unrecognized` over a validated envelope: the type of each
/// enforced item that declares a safety property nothing can check (SG-8,
/// SG-10), invariants first, in order.
fn opaque_unrecognized(env: &Object) -> Vec<String> {
    let mut out = Vec::new();
    for (key, label) in [("invariants", "invariant"), ("postconditions", "postcondition")] {
        let Value::List(items) = env.value(key) else { continue };
        for it in items {
            let Value::Obj(o) = it else { continue };
            if !matches!(o.value("enforce"), Value::Bool(true)) || !matches!(o.value("expr"), Value::Null) {
                continue;
            }
            let Some(t) = o.value("type").as_str() else { continue };
            let unrecognized = if label == "invariant" {
                crate::dialect::word_for(t).is_none() && !crate::z3_checks::RECOGNIZED_OPAQUE_TYPES.contains(&t)
            } else {
                !["exit_code", "file_exists", "file_size_range"].contains(&t)
            };
            if unrecognized {
                out.push(t.to_string());
            }
        }
    }
    out
}
