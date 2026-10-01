//! `daisugi graft install|status|remove`: the large-read rule file in the
//! gate root, and the refusal beside a rival PreToolUse hook (GR-3).

use num_bigint::BigInt;

use super::gateroot::{getwd, join, path_str};
use super::words::{gate_hook_kind, is_record_hook, KIND_GATE};
use super::{exit, parse_args, Env, Opt, Res};
use crate::delegate::{load_rules, parse_rule, Rule};
use crate::gate::pyjson::{dumps, dumps_indent, loads, LoadError, Object, Value};

const GRAFT_HELP: &str = "Usage: daisugi graft [OPTIONS] COMMAND [ARGS]...

  Install, show and remove the gate's graft rule for large reads.

Commands:
  install  Write the large-read graft rule into <data-dir>/gate/grafts.
  status   Show the graft rule files, the one in force, and any rival...
  remove   Remove a graft rule file.
";

const NOT_READABLE: &str = "not readable JSON";

fn valid_id(id: &str) -> bool {
    !id.is_empty() && id.chars().count() <= 64 && id.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
}

fn refusal(file: &str, what: &str) -> String {
    if what == NOT_READABLE {
        format!(
            "Refused: {file} is not readable JSON, so its PreToolUse hooks cannot be checked. \
             No graft was installed; the gate is not changed."
        )
    } else {
        format!(
            "Refused: {file} has a PreToolUse hook that may rewrite a tool's input: {what}. \
             No graft was installed; the gate is not changed."
        )
    }
}

/// `json.loads(path.read_bytes().decode("utf-8"))`: None for a missing
/// file, Some(Err(())) for one that does not read, Some(Ok(v)) else.
fn read_json(path: &str) -> Result<Option<Result<Value, ()>>, String> {
    let raw = match std::fs::read(path) {
        Ok(r) => r,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Ok(Some(Err(()))),
    };
    let Ok(text) = std::str::from_utf8(&raw) else {
        return Ok(Some(Err(())));
    };
    match loads(text) {
        Ok(v) => Ok(Some(Ok(v))),
        Err(LoadError::Unsupported) => Err("a settings file holds JSON this binary does not read".into()),
        Err(_) => Ok(Some(Err(()))),
    }
}

impl Env {
    pub(super) fn graft(&mut self, args: &[String]) -> Res {
        if args.is_empty() {
            self.out(GRAFT_HELP);
            return exit(2);
        }
        if args[0] == "--help" {
            self.out(GRAFT_HELP);
            return Ok(());
        }
        match args[0].as_str() {
            "install" => self.graft_install(&args[1..]),
            "status" => self.graft_status(&args[1..]),
            "remove" => self.graft_remove(&args[1..]),
            other => {
                self.errf(&format!(
                    "Usage: daisugi graft [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi graft --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// `graft_install.settings_files`.
    fn graft_settings_files(&self) -> Vec<String> {
        let cwd = getwd().unwrap_or_else(|_| self.home.clone());
        let mut out: Vec<String> = Vec::new();
        for p in [
            join(&join(&self.home, ".claude"), "settings.json"),
            join(&join(&cwd, ".claude"), "settings.json"),
            join(&join(&cwd, ".claude"), "settings.local.json"),
            join(&join(&self.home, ".codex"), "hooks.json"),
        ] {
            if !out.contains(&p) {
                out.push(p);
            }
        }
        out
    }

    /// `graft_install.rival_hooks`: (file, what) for each hook that may
    /// rewrite input.
    fn graft_rivals(&self) -> Result<Vec<(String, String)>, String> {
        let mut out = Vec::new();
        for path in self.graft_settings_files() {
            let data = match read_json(&path)? {
                None => continue,
                Some(Err(())) => {
                    out.push((path, NOT_READABLE.to_string()));
                    continue;
                }
                Some(Ok(v)) => v,
            };
            let Value::Obj(obj) = data else { continue };
            let Value::Obj(hooks) = obj.value("hooks") else { continue };
            let Value::List(entries) = hooks.value("PreToolUse") else { continue };
            for ent in entries {
                let Value::Obj(eo) = ent else { continue };
                let Value::List(inner) = eo.value("hooks") else { continue };
                for h in inner {
                    if let Value::Obj(ho) = h {
                        let c = ho.value("command");
                        if gate_hook_kind(c) == KIND_GATE || is_record_hook(c, "") {
                            continue;
                        }
                        if let Value::Str(s) = c {
                            out.push((path.clone(), dumps(&Value::Str(s.clone()), true)));
                            continue;
                        }
                    }
                    out.push((path.clone(), dumps(h, true)));
                }
            }
        }
        Ok(out)
    }

    fn graft_data_dir(&self, p: &super::Parsed) -> String {
        path_str(&p.str("--data-dir", &self.data_home()))
    }

    fn graft_id(&mut self, p: &super::Parsed) -> Result<String, super::Stop> {
        let id = p.str("--id", "big-read");
        if !valid_id(&id) {
            self.echo_err("Error: --id must be 1 to 64 of A-Z a-z 0-9 . _ -\n");
            return Err(super::Stop::Exit(2));
        }
        Ok(id)
    }

    fn graft_install(&mut self, args: &[String]) -> Res {
        const CMD: &str = "graft install";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::val(&["--id"], "TEXT", "The rule's id and file name."),
            Opt::val(
                &["--state"],
                "TEXT",
                "audit (only records), trial (acts in the seeded graft arm of sessions) or active.",
            ),
            Opt::val(&["--file-lines-over"], "INTEGER", "Redirect reads of more lines."),
            Opt::flag(&["--allow-remote"], "Let the router pick a remote worker the envelope grants."),
            Opt::val(&["--seed"], "INTEGER", "With --state trial: the seed that splits sessions (default 0)."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Write the large-read graft rule into <data-dir>/gate/grafts.", &opts);
        }
        let lines = self.click_int(CMD, &p, "--file-lines-over", 350)?;
        let seed = self.click_int(CMD, &p, "--seed", 0)?;
        let has_seed = p.has("--seed");
        let id = self.graft_id(&p)?;
        let state = p.str("--state", "audit");
        if state != "audit" && state != "trial" && state != "active" {
            self.echo_err("Error: --state must be audit, trial or active.\n");
            return exit(2);
        }
        if has_seed && state != "trial" {
            self.echo_err("Error: --seed needs --state trial.\n");
            return exit(2);
        }
        if has_seed && !(0..=(1i128 << 53)).contains(&seed) {
            self.echo_err("Error: --seed must be from 0 to 2**53.\n");
            return exit(2);
        }
        if lines < 1 {
            self.echo_err("Error: --file-lines-over must be 1 or more.\n");
            return exit(2);
        }
        let rivals = match self.graft_rivals() {
            Ok(r) => r,
            Err(why) => return self.refuse(CMD, &why),
        };
        if let Some((file, what)) = rivals.first() {
            let line = refusal(file, what);
            self.echo_err(&format!("{line}\n"));
            return exit(1);
        }
        let dir = join(&join(&self.graft_data_dir(&p), "gate"), "grafts");
        let path = join(&dir, &format!("{id}.json"));
        let mut version = BigInt::from(0);
        if let Ok(Some(Ok(v))) = read_json(&path) {
            let name = format!("{id}.json");
            if let Ok(r) = parse_rule(&v, &name) {
                if r.id == id {
                    version = r.version.parse::<BigInt>().unwrap_or_default();
                }
            }
        }
        version += 1;
        let mut matched = Object::new();
        matched.set("tool", "Read");
        matched.set("file_lines_over", Value::Int(lines.to_string()));
        let mut redirect = Object::new();
        redirect.set("tool", "delegate");
        let mut worker = Object::new();
        worker.set("choose", "router");
        worker.set("allow_remote", p.flag("--allow-remote"));
        let mut rule = Object::new();
        rule.set("id", id.as_str());
        rule.set("version", Value::Int(version.to_string()));
        rule.set("shape", "deny_redirect");
        rule.set("state", state.as_str());
        rule.set("match", Value::Obj(matched));
        rule.set("redirect", Value::Obj(redirect));
        rule.set("worker", Value::Obj(worker));
        let mut shown = state.clone();
        if state == "trial" {
            let mut trial = Object::new();
            trial.set("seed", Value::Int(seed.to_string()));
            rule.set("trial", Value::Obj(trial));
            shown = format!("trial, seed {seed}");
        }
        if let Err(e) = std::fs::create_dir_all(&dir) {
            return self.fail(CMD, &e.to_string());
        }
        let tmp = join(&dir, &format!(".{id}.json.tmp"));
        let text = dumps_indent(&Value::Obj(rule), 2, true) + "\n";
        if let Err(e) = std::fs::write(&tmp, text) {
            return self.fail(CMD, &e.to_string());
        }
        if let Err(e) = std::fs::rename(&tmp, &path) {
            return self.fail(CMD, &e.to_string());
        }
        self.echo(&format!(
            "Installed graft rule {id} (version {version}, state {shown}, reads over {lines} lines): {path}\n"
        ));
        Ok(())
    }

    fn graft_status(&mut self, args: &[String]) -> Res {
        const CMD: &str = "graft status";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Show the graft rule files, the one in force, and any rival PreToolUse hook.",
                &opts,
            );
        }
        let root = join(&self.graft_data_dir(&p), "gate");
        let (rules, bad) = load_rules(&root);
        let rivals = match self.graft_rivals() {
            Ok(r) => r,
            Err(why) => return self.refuse(CMD, &why),
        };
        let acting: Option<usize> = rules.iter().position(Rule::acting);
        let why_of = |i: usize, r: &Rule| -> Option<String> {
            match acting {
                Some(a) if a == i => None,
                _ if !r.acting() => Some(format!("state {} does not act", r.state)),
                Some(a) => Some(format!("{} acts first", rules[a].file)),
                None => None,
            }
        };
        let dir = join(&root, "grafts");
        if p.flag("--json") {
            let mut out_rules = Vec::new();
            for (i, r) in rules.iter().enumerate() {
                let mut o = Object::new();
                o.set("file", r.file.as_str());
                o.set("id", r.id.as_str());
                o.set("version", Value::Int(r.version.clone()));
                o.set("state", r.state.as_str());
                o.set("file_lines_over", Value::Int(r.min_lines.clone()));
                o.set("allow_remote", r.allow_remote);
                o.set(
                    "trial_seed",
                    if r.state == "trial" { Value::Int(r.seed.to_string()) } else { Value::Null },
                );
                o.set("in_force", acting == Some(i));
                o.set("why", why_of(i, r).map(Value::Str).unwrap_or(Value::Null));
                out_rules.push(Value::Obj(o));
            }
            let unused = bad
                .iter()
                .map(|(f, w)| {
                    let mut o = Object::new();
                    o.set("file", f.as_str());
                    o.set("why", w.as_str());
                    Value::Obj(o)
                })
                .collect();
            let rh = rivals
                .iter()
                .map(|(f, w)| {
                    let mut o = Object::new();
                    o.set("file", f.as_str());
                    o.set("hook", w.as_str());
                    Value::Obj(o)
                })
                .collect();
            let mut doc = Object::new();
            doc.set("dir", dir.as_str());
            doc.set("rules", Value::List(out_rules));
            doc.set("unused", Value::List(unused));
            doc.set("rival_hooks", Value::List(rh));
            let text = dumps_indent(&Value::Obj(doc), 2, true);
            self.echo(&format!("{text}\n"));
            return Ok(());
        }
        let mut text = String::new();
        if !rules.is_empty() || !bad.is_empty() {
            text += &format!("Graft rules in {dir}:\n");
        } else {
            text += &format!("No graft rules in {dir}.\n");
        }
        for (i, r) in rules.iter().enumerate() {
            let tail = match why_of(i, r) {
                None => "in force".to_string(),
                Some(w) => format!("not in force: {w}"),
            };
            let state =
                if r.state == "trial" { format!("trial (seed {})", r.seed) } else { r.state.clone() };
            text += &format!(
                "  {}: {} v{}, state {}, reads over {} lines, {tail}\n",
                r.file, r.id, r.version, state, r.min_lines
            );
        }
        for (f, w) in &bad {
            text += &format!("  {f}: not used: {w}\n");
        }
        if rivals.is_empty() {
            text += "PreToolUse hooks that may rewrite input: none\n";
        } else {
            text += "PreToolUse hooks that may rewrite input (install refuses):\n";
            for (f, w) in &rivals {
                text += &format!("  {f}: {w}\n");
            }
        }
        self.echo(&text);
        Ok(())
    }

    fn graft_remove(&mut self, args: &[String]) -> Res {
        const CMD: &str = "graft remove";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::val(&["--id"], "TEXT", "The rule's id and file name."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Remove a graft rule file. The gate is not changed.", &opts);
        }
        let id = self.graft_id(&p)?;
        let path = join(&join(&join(&self.graft_data_dir(&p), "gate"), "grafts"), &format!("{id}.json"));
        match std::fs::remove_file(&path) {
            Ok(()) => self.echo(&format!("Removed graft rule {id}: {path}\n")),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                self.echo(&format!("No graft rule {id} at {path}.\n"))
            }
            Err(e) => return self.fail(CMD, &e.to_string()),
        }
        Ok(())
    }
}
