//! `daisugi gate init|register|arm|disarm|status|report|settings|proposals`.

use super::config::{self, ConfigErr};
use super::envelope::{self, Bad};
use super::gateroot::{self, join, parent, path_str};
use super::install::{self, HookOptions};
use super::journal::{self, JournalErr};
use super::words::KIND_UNKNOWN;
use super::yaml::{self, Kind, Node};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::pyjson::{dumps, dumps_indent, loads, LoadError, Object, Value};

const ROOT_OPT: Opt = Opt::val(&["--root"], "PATH", "Gate state directory (envelopes, audit log, disarm marker).");
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
        join(&self.data_home(), "gate")
    }

    /// `Path.cwd()`, or home when the directory is gone.
    pub(super) fn cwd(&self) -> String {
        gateroot::getwd().unwrap_or_else(|_| self.home.clone())
    }

    pub(super) fn gate_disarm(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate disarm", m),
        };
        if p.help {
            return self.cmd_help("gate disarm", "", "Kill switch: the gate allows everything until re-armed.", &opts);
        }
        let (marker, followed, res) = gateroot::disarm(&self.root(&p));
        self.note_file(&marker, &followed);
        if let Err(e) = res {
            return self.fail("gate disarm", &e.to_string());
        }
        self.out(&format!("gate DISARMED (marker: {marker}) — `daisugi gate arm` to re-enable\n"));
        Ok(())
    }

    pub(super) fn gate_arm(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate arm", m),
        };
        if p.help {
            return self.cmd_help("gate arm", "", "Remove the disarm marker; the gate resumes evaluating calls.", &opts);
        }
        if let Err(e) = gateroot::arm(&self.root(&p)) {
            return self.fail("gate arm", &e.to_string());
        }
        self.out("gate armed\n");
        Ok(())
    }

    pub(super) fn gate_status(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT, JSON_OPT];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate status", m),
        };
        if p.help {
            return self.cmd_help(
                "gate status",
                "",
                "Show armed/disarmed state, the verdict mode, and the registered envelopes.",
                &opts,
            );
        }
        let root = self.root(&p);
        let armed = !gateroot::is_disarmed(&root);
        let cwd = self.cwd();
        let eff = match config::effective_hook_mode(&self.home, &cwd) {
            Ok(e) => e,
            Err(_) => {
                return self.refuse("gate status", "a Claude Code settings.json holds hooks this binary does not read yet")
            }
        };
        let (mut mode, mut source) = (eff.mode.clone(), config::source_label(&eff).to_string());
        if mode.is_empty() {
            mode = match config::gate_mode(&join(&parent(&root), "config.yaml")) {
                Ok(m) => m,
                Err(e) => return self.refuse("gate status", &e.to_string()),
            };
            source = "config".into();
        }
        let envs = match gateroot::envelopes(&root) {
            Ok(e) => e,
            Err(why) => return self.refuse("gate status", &why),
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
        // cli._gate_dialect_status: the dialect's hash, the pin config.yaml
        // names, the mode that follows, and the calls a word would deny.
        let pin = match config::dialect_pin(&join(&parent(&root), "config.yaml")) {
            Ok(p) => p,
            Err(e) => return self.refuse("gate status", &e.to_string()),
        };
        let word_would_deny = match super::journal::word_would_deny(&root) {
            Ok(n) => n,
            Err(e) => return self.refuse("gate status", &e.to_string()),
        };
        let hash = crate::dialect::DIALECT_HASH;
        let dialect_mode = match &pin {
            None => "audit",
            Some(p) if p == hash => "enforce",
            Some(_) => "pin_mismatch",
        };
        let dialect = Object::new()
            .with("hash", hash)
            .with("pin", pin.clone().map(Value::Str).unwrap_or(Value::Null))
            .with("mode", dialect_mode)
            .with("word_would_deny", word_would_deny as i64);
        if p.flag("--json") {
            let o = Object::new()
                .with("armed", armed)
                .with("mode", mode.as_str())
                .with("mode_source", source.as_str())
                .with("envelopes", Value::List(envs.iter().map(|s| Value::Str(s.clone())).collect()))
                .with("dialect", dialect);
            self.out(&format!("{}\n", dumps(&Value::Obj(o), true)));
            for g in &gone {
                self.errf(&format!("{g}\n"));
            }
            return Ok(());
        }
        let state = if armed { "armed" } else { "DISARMED" };
        // A gate hook in a form this CLI does not read. It may enforce.
        let shown = if mode == KIND_UNKNOWN { "unknown gate hook".to_string() } else { mode };
        self.out(&format!("gate: {state} · mode: {shown} ({source})\n"));
        if envs.is_empty() {
            self.out("no envelopes registered — enforce mode would deny everything\n");
        }
        for name in envs {
            self.out(&format!("  envelope: {name}\n"));
        }
        let head = match (&pin, dialect_mode) {
            (Some(p), "pin_mismatch") => format!("dialect: {hash} · pinned {p}, not this build's: every word use denies"),
            _ => format!("dialect: {hash} · words: {dialect_mode}"),
        };
        self.out(&format!("{head} · word would-denies: {word_would_deny}\n"));
        for g in &gone {
            self.errf(&format!("{g}\n"));
        }
        Ok(())
    }

    pub(super) fn gate_report(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--session"], "TEXT", "One session's log only."),
            ROOT_OPT,
            Opt::flag(&["--json"], "Emit the full report as JSON."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate report", m),
        };
        if p.help {
            return self.cmd_help(
                "gate report",
                "",
                "Summarize the audit log: what an enforcing gate would have denied.",
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
            Err(e @ JournalErr::Crash(_)) => self.fail("gate report", &e.to_string()),
            Err(e) => self.refuse("gate report", &e.to_string()),
        }
    }

    pub(super) fn gate_settings(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::flag(&["--enforce"], "Emit enforce-mode settings (default is audit: observation only)."),
            ROOT_OPT,
            Opt::val(&["--format"], "TEXT", "Host contract (default claude)."),
            Opt::val(&["--session"], "TEXT", "Pin the gate to this registered session's envelope."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate settings", m),
        };
        if p.help {
            return self.cmd_help(
                "gate settings",
                "",
                "Print the Claude Code hooks-settings JSON that wires in the gate. Usage: claude --settings \"$(daisugi gate settings)\"",
                &opts,
            );
        }
        let me = match self.self_path() {
            Ok(s) => s,
            Err(e) => return self.fail("gate settings", &e),
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

    /// `cli._resolve_decompose` for an unset flag.
    pub(super) fn decompose_default(&mut self, root: &str, cmd: &str) -> Result<bool, super::Stop> {
        match config::load(&join(&parent(root), "config.yaml")) {
            Ok(c) => Ok(c.shell_allow_decomposition),
            Err(e @ (ConfigErr::Invalid | ConfigErr::Yaml(_))) => Err(self.fail(cmd, &e.to_string()).unwrap_err()),
            Err(e) => Err(self.refuse(cmd, &e.to_string()).unwrap_err()),
        }
    }

    pub(super) fn gate_init(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--workspace"], "PATH", "The directory the session may read/write (default: cwd)."),
            Opt::val(&["--session"], "TEXT", "Bind the envelope to this session id."),
            ROOT_OPT,
            Opt::flag(&["--force"], "Overwrite an existing envelope."),
            DECOMPOSE_OPT,
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate init", m),
        };
        if p.help {
            return self.cmd_help("gate init", "", "Generate and register a reviewable starter envelope for this session.", &opts);
        }
        let root = self.root(&p);
        let decompose = if p.flag_set("--allow-shell-decomposition") {
            p.flag("--allow-shell-decomposition")
        } else {
            self.decompose_default(&root, "gate init")?
        };
        let session = p.str("--session", "");
        let name = if session.is_empty() { "default".to_string() } else { session.clone() };
        // The file register writes: the session id made safe, so the check
        // and the write look at one path.
        let file = if session.is_empty() { "default".to_string() } else { gateroot::safe_session_id(&session) };
        let target = join(&gateroot::envelopes_dir(&root), &format!("{file}.json"));
        if gateroot::exists(&target) && !p.flag("--force") {
            self.errf(&format!(
                "an envelope for '{name}' is already registered at {target}; pass --force to overwrite\n"
            ));
            return exit(1);
        }
        let ws = if p.has("--workspace") {
            p.str("--workspace", "")
        } else {
            match gateroot::getwd() {
                Ok(w) => w,
                Err(e) => return self.fail("gate init", &e.to_string()),
            }
        };
        let ws = match gateroot::resolve(&ws) {
            Ok(w) => w,
            Err(e) => return self.fail("gate init", &e.to_string()),
        };
        let env = match envelope::starter(&ws, decompose) {
            Ok(e) => e,
            Err(e) => return self.fail("gate init", &e.to_string()),
        };
        let (path, followed, res) = envelope::register(&env, &session, &root);
        self.note_file(&path, &followed);
        if let Err(e) = res {
            return self.fail("gate init", &e.to_string());
        }
        self.out(&format!("registered a starter envelope for {ws} → {path}\n"));
        self.out("REVIEW it before enforcing — it is a tight default, not a finished policy.\n");
        self.out("Then launch audit mode:\n");
        // A session's envelope is read only by a gate pinned to it.
        let pin = if session.is_empty() {
            String::new()
        } else {
            format!(" --session {}", crate::interpreter_parse::shlex_quote(&session))
        };
        self.out(&format!("  claude --settings \"$(daisugi gate settings --root {root}{pin})\"\n"));
        Ok(())
    }

    pub(super) fn gate_register(&mut self, args: &[String]) -> Res {
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
            Err(m) => return self.usage("gate register", m),
        };
        if p.help {
            return self.cmd_help(
                "gate register",
                " ENVELOPE_PATH",
                "Register the envelope the gate checks this session's calls against.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage("gate register", "Missing argument 'ENVELOPE_PATH'.".into());
        }
        let file = p.args[0].clone();
        let raw = match std::fs::read(&file) {
            Ok(r) => r,
            Err(e) => return self.fail("gate register", &e.to_string()),
        };
        let text = match String::from_utf8(raw) {
            Ok(t) => t,
            Err(_) => return self.fail("gate register", &format!("{file} is not valid UTF-8")),
        };
        // yaml.safe_load reads the file, JSON included, as YAML 1.1: to it
        // 1e-09 is a string, not a number.
        let y = match yaml::parse(&text) {
            Ok(y) => y,
            // safe_load raises: the oracle's traceback names the exception.
            Err(e @ yaml::YErr::Exc(_)) => return self.fail("gate register", &e.traceback_line()),
            Err(_) => return self.refuse("gate register", "the envelope uses YAML this binary does not read yet"),
        };
        let v = match yaml_to_json(&y) {
            Ok(v) => v,
            Err(why) => return self.refuse("gate register", &why),
        };
        let input = match v {
            Value::Obj(o) => o,
            _ => return self.fail("gate register", "the envelope file does not hold a mapping"),
        };
        let env = match envelope::validate(&input) {
            Ok(e) => e,
            Err(e @ Bad::Invalid(_)) => return self.fail("gate register", &e.to_string()),
            Err(e) => return self.refuse("gate register", &e.to_string()),
        };
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
        let (path, followed, res) = envelope::register(&env, &session, &self.root(&p));
        self.note_file(&path, &followed);
        if let Err(e) = res {
            return self.fail("gate register", &e.to_string());
        }
        let which = if session.is_empty() { "default".to_string() } else { format!("session {session}") };
        self.out(&format!("registered {which} envelope → {path}\n"));
        Ok(())
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
        let target = super::gateroot::join(&super::gateroot::envelopes_dir(root), &format!("{}.json", super::gateroot::safe_session_id(session)));
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

    pub(super) fn gate_proposals(&mut self, args: &[String]) -> Res {
        let opts = [ROOT_OPT, Opt::flag(&["--json"], "Emit proposals as a JSON array.")];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage("gate proposals", m),
        };
        if p.help {
            return self.cmd_help(
                "gate proposals",
                "",
                "List recorded envelope-edit proposals. A proposal is a file; nobody applies it automatically.",
                &opts,
            );
        }
        let d = join(&self.root(&p), "proposals");
        let mut props: Vec<Object> = vec![];
        if gateroot::is_dir(&d) {
            let entries = match std::fs::read_dir(&d) {
                Ok(e) => e,
                Err(e) => return self.fail("gate proposals", &e.to_string()),
            };
            let mut names = vec![];
            for en in entries {
                let en = match en {
                    Ok(en) => en,
                    Err(e) => return self.fail("gate proposals", &e.to_string()),
                };
                let name = match en.file_name().into_string() {
                    Ok(n) => n,
                    Err(_) => return self.refuse("gate proposals", gateroot::ERR_NAME),
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
                        return self.refuse("gate proposals", "input outside the modeled subset")
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
                                .refuse("gate proposals", "a proposal holds a value this binary does not print yet")
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
