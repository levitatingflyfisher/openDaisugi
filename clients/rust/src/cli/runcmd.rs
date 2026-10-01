//! `daisugi run PLAN -e ENVELOPE`: a plan executed under the supervisor,
//! with the flags, output, exit codes and journal of the Python CLI. The
//! Go client's `cli/runcmd.go` is the reference.

use std::collections::BTreeMap;
use std::time::Instant;

use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text::{rstrip, splitlines};
use crate::gate::pyjson::{dumps_indent, Object, Value};
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::pathways::verify::Outcome;
use crate::pathways::PwErr;
use crate::supervise::{self, approval::is_terminal, DefaultApproval, DryRun, Executor, Session, Supervisor};
use crate::violation::Violation;

/// The whole-plan verify a run makes first; a test swaps it.
pub type PlanCheck = fn(&Value, &Value, Option<bool>, u32) -> Result<Outcome, PwErr>;

/// A plan and envelope read and verified before anything is written.
pub struct Prepared {
    pub verification: Object,
}

/// click's `_ansi_re`: typer.echo drops these sequences from what it
/// writes to a stream that is not a terminal.
fn strip_ansi(text: &str) -> String {
    use std::sync::OnceLock;
    static RE: OnceLock<regex::Regex> = OnceLock::new();
    RE.get_or_init(|| regex::Regex::new("\x1b\\[[;?0-9]*[a-zA-Z]").expect("a fixed pattern")).replace_all(text, "").into_owned()
}

/// `yaml.safe_load(path.read_text())` for a plan or envelope file: the
/// value, the exception it raises, or a refusal (`Err(Err(why))`) for a
/// value this binary does not model.
pub(super) fn load_yaml(path: &str, text: &str) -> Result<Value, Result<crate::pyyaml::Exc, String>> {
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    let v = match crate::pyyaml::load(&text) {
        Ok(v) => v,
        Err(crate::pyyaml::Fail::Unsupported(why)) => {
            return Err(Err(format!("{path} holds YAML this binary does not read ({why})")))
        }
        Err(crate::pyyaml::Fail::Exc(e)) => return Err(Ok(e)),
    };
    // A mapping holding a date or a key that is not text is refused; a
    // list is passed on by its type, for the caller's own error.
    match crate::pyyaml::to_json(&v) {
        Some(j) => Ok(j),
        None if matches!(v, crate::pyyaml::Val::List(_)) => Ok(Value::List(vec![])),
        None => Err(Err(format!("{path} holds a date or a key that is not text"))),
    }
}

/// Whether the step outputs of a finished run go to a terminal.
pub(super) fn tty(fd: i32) -> bool {
    is_terminal(fd)
}

/// Verifies `plan` against `env` as `Supervisor.run` does first, and
/// refuses what this binary cannot journal the way the oracle does. A Z3
/// check that did not finish fails the run here, where the oracle's
/// lenient verify keeps it as a warning (K2-2).
pub fn prepare(plan: &Object, env: &Object, check: PlanCheck) -> Result<Prepared, String> {
    let why = crate::pathways::verify::stage2_refusal(&Value::Obj(env.clone()));
    if !why.is_empty() {
        return Err(why);
    }
    let t0 = Instant::now();
    let mut r = check(&Value::Obj(plan.clone()), &Value::Obj(env.clone()), None, 500).map_err(|e| e.to_string())?;
    let d = supervise::ms(t0);
    if !r.timeouts.is_empty() {
        r.violations.push(Violation::plan("z3").msg(r.timeouts[0].clone()).with(Object::new(), None));
    }
    let verification =
        supervise::verification_dump(&r.violations, &r.warnings, r.warnings_unmodeled, env.value("id"), plan.value("id"), d)?;
    Ok(Prepared { verification })
}

impl Env {
    /// click.Path(exists=True, readable=True): click's usage error, or
    /// None.
    pub(super) fn click_path(name: &str, path: &str) -> Option<String> {
        if std::fs::metadata(path).is_err() {
            return Some(format!("Invalid value for {name}: Path '{path}' does not exist."));
        }
        let c = std::ffi::CString::new(path).ok()?;
        if unsafe { libc::access(c.as_ptr(), libc::R_OK) } != 0 {
            return Some(format!("Invalid value for {name}: Path '{path}' is not readable."));
        }
        None
    }

    /// click's usage error for a command with a positional.
    pub(super) fn usage_args(&mut self, cmd: &str, args: &str, msg: &str) -> Res {
        self.errf(&format!("Usage: daisugi {cmd} [OPTIONS] {args}\nTry 'daisugi {cmd} --help' for help.\n\nError: {msg}\n"));
        exit(2)
    }

    /// `typer.echo(text)` to stdout.
    pub(super) fn echo(&mut self, text: &str) {
        let t = if tty(1) { text.to_string() } else { strip_ansi(text) };
        self.out(&t);
    }

    /// `typer.echo(text, err=True)`.
    pub(super) fn echo_err(&mut self, text: &str) {
        let t = if tty(2) { text.to_string() } else { strip_ansi(text) };
        self.errf(&t);
    }

    /// `Model(**yaml.safe_load(path.read_text()))`: the model's dump, or
    /// the first line of the error the oracle prints (`Err(Ok(..))`), or
    /// a refusal for what this binary does not read the way Python does
    /// (`Err(Err(..))`).
    pub(super) fn load_model_yaml(path: &str, id: Id) -> Result<Object, Result<String, String>> {
        let raw = std::fs::read(path).map_err(|e| Err(format!("{path} cannot be read: {e}")))?;
        let text = String::from_utf8(raw).map_err(|_| Err(format!("{path} is not UTF-8")))?;
        let v = match load_yaml(path, &text) {
            Ok(v) => v,
            Err(Err(why)) => return Err(Err(why)),
            Err(Ok(e)) if crate::pyyaml::caught(&e) => return Err(Ok(crate::pyyaml::first_line(&e))),
            Err(Ok(e)) => {
                return Err(Err(format!(
                    "{path} holds YAML the oracle rejects with a {}, which it does not catch",
                    e.kind
                )))
            }
        };
        if v.as_obj().is_none() {
            return Err(Err(format!("{path} does not hold a mapping")));
        }
        match validate_model(id, &v, Mode::Python) {
            Ok(Value::Obj(o)) => Ok(o),
            Ok(_) => Err(Err(format!("{path} does not hold a mapping"))),
            Err(e) => match e.unreadable() {
                Some(why) => Err(Err(format!("{path}: {why}"))),
                None => Err(Ok(e.text().split('\n').next().unwrap_or("").to_string())),
            },
        }
    }

    pub(super) fn run_cmd(&mut self, args: &[String]) -> Res {
        self.run_cmd_with(args, crate::pathways::verify::verify)
    }

    pub(super) fn run_cmd_with(&mut self, args: &[String], check: PlanCheck) -> Res {
        const CMD: &str = "run";
        let opts = [
            Opt::val(&["--envelope", "-e"], "PATH", "Path to envelope YAML."),
            Opt::val(&["--data-dir"], "PATH", "Root data directory for the journal."),
            Opt::flag(&["--dry-run"], "Use DryRunExecutor — no real subprocesses."),
            Opt::flag(&["--yes", "-y"], "Auto-approve every step (sets DAISUGI_APPROVE=always for this run)."),
            Opt::flag(&["--json"], "Emit the run session as JSON on stdout."),
            super::weavecmd::AGENT_OPT,
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "PLAN_PATH", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " PLAN_PATH", "Execute PLAN against ENVELOPE under runtime supervision.", &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "PLAN_PATH", "Missing argument 'PLAN_PATH'.");
        }
        let plan_path = p.args[0].clone();
        if let Some(m) = Self::click_path("'plan_path'", &plan_path) {
            return self.usage_args(CMD, "PLAN_PATH", &m);
        }
        if !p.has("--envelope") {
            return self.usage_args(CMD, "PLAN_PATH", "Missing option '--envelope' / '-e'.");
        }
        let env_path = p.str("--envelope", "");
        if let Some(m) = Self::click_path("'--envelope' / '-e'", &env_path) {
            return self.usage_args(CMD, "PLAN_PATH", &m);
        }
        let agent = self.check_agent(&p)?;
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        let env = match Self::load_model_yaml(&env_path, Id::Envelope) {
            Ok(o) => o,
            Err(Err(why)) => return self.refuse(CMD, &why),
            Err(Ok(perr)) => {
                return Err(self.fail3(
                    &format!("Tried to read the envelope {env_path}."),
                    &format!("It did not parse: {perr}"),
                    "Fix the file and run again.",
                    2,
                ))
            }
        };
        let plan = match Self::load_model_yaml(&plan_path, Id::ActionPlan) {
            Ok(o) => o,
            Err(Err(why)) => return self.refuse(CMD, &why),
            Err(Ok(perr)) => {
                return Err(self.fail3(
                    &format!("Tried to read the plan {plan_path}."),
                    &format!("It did not parse: {perr}"),
                    "Fix the file and run again.",
                    2,
                ))
            }
        };
        if p.flag("--yes") {
            self.env.insert("DAISUGI_APPROVE".into(), "always".into());
        }
        let pre = match prepare(&plan, &env, check) {
            Ok(p) => p,
            Err(why) => return self.refuse(CMD, &why),
        };
        let fallback = match env.value("fallback") {
            Value::Obj(f) if f.value("strategy").as_str() == Some("tier2_recompute") => {
                Some(supervise::recompute(self.llm_client(), env.clone(), 500))
            }
            _ => None,
        };
        let j = self.open_journal(CMD, &data_dir)?;
        let dry = p.flag("--dry-run");
        if dry {
            self.out("Dry run — no real subprocesses will be spawned\n");
        }
        let mut executors: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
        if dry {
            for k in ["shell", "file_read", "file_write", "network", "agentic"] {
                executors.insert(k.into(), Box::new(DryRun));
            }
        } else {
            for (k, ex) in supervise::default_executors(self.env.clone()) {
                executors.insert(k, ex);
            }
            let agentic = match self.agentic(&env, &agent) {
                Ok(a) => a,
                Err(e) => return self.fail(CMD, &e),
            };
            executors.insert("agentic".into(), Box::new(agentic));
        }
        let terminal = tty(0) && tty(1);
        if terminal {
            // A prompt shows after what was printed so far.
            self.flush();
        }
        let approval = Box::new(DefaultApproval { env: self.env.clone(), terminal });
        let mut sup = Supervisor::new(executors, approval, Some(&j));
        sup.fallback = fallback;
        let sess = sup.run(&plan, &env, pre.verification);
        if let Some(e) = sup.log_err.take() {
            drop(sup);
            return self.fail(CMD, &e);
        }
        drop(sup);
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(sess.json()), 2, true)));
        } else {
            self.print_run(&sess);
        }
        match sess.status.as_str() {
            supervise::SUCCEEDED => Ok(()),
            supervise::REJECTED => {
                let vs = match sess.verification.value("violations") {
                    Value::List(l) => l.clone(),
                    _ => vec![],
                };
                let field = |v: &Value, k: &str| v.as_obj().map(|o| o.value(k).as_str().unwrap_or("").to_string()).unwrap_or_default();
                for v in &vs {
                    self.echo_err(&format!("  {}: {}\n", field(v, "stage"), field(v, "message")));
                }
                let first = match vs.first() {
                    Some(v) => format!("[{}] {}", field(v, "stage"), field(v, "message")),
                    None => "Verification rejected the plan.".into(),
                };
                Err(self.fail3(
                    &format!("Tried to run the plan {plan_path}."),
                    &first,
                    "Edit the plan or widen the envelope; see the violation above.",
                    2,
                ))
            }
            supervise::ABORTED => exit(130),
            _ => exit(1),
        }
    }

    fn print_run(&mut self, s: &Session) {
        self.echo(&format!("Run {} ({})\n", s.id, s.status));
        for o in &s.steps {
            let rc = o.rc.map(|n| n.to_string()).unwrap_or_else(|| "None".into());
            let by = o.approved_by.clone().unwrap_or_else(|| "None".into());
            self.echo(&format!("  {}: {} (rc={rc}, approved_by={by}, {:.1} ms)\n", o.step_id, o.status, o.duration_ms));
            if let Some(e) = o.error.as_ref().filter(|e| !e.is_empty()) {
                self.echo(&format!("      error: {e}\n"));
            }
            if !o.stdout.is_empty() {
                for ln in splitlines(rstrip(&o.stdout)).into_iter().take(5) {
                    self.echo(&format!("      {ln}\n"));
                }
            }
        }
        if let Some(t) = &s.trace_id {
            self.echo(&format!("Journal: {t}\n"));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pathways::verify::Outcome as Verified;

    fn verify_unknown(_: &Value, _: &Value, _: Option<bool>, _: u32) -> Result<Verified, PwErr> {
        Ok(Verified { violations: vec![], timeouts: vec!["Z3 returned unknown".into()], warnings: vec![], warnings_unmodeled: false })
    }

    /// A Z3 check that does not finish in the whole-plan verify rejects
    /// the run (fail closed): nothing runs, and the run is journaled with
    /// the z3 violation.
    #[test]
    fn a_run_whose_z3_check_did_not_finish_is_rejected() {
        let home = crate::supervise::tests::scratch("run");
        let env = format!("{home}/e.yaml");
        let plan = format!("{home}/p.yaml");
        let marker = format!("{home}/ran");
        std::fs::write(&env, "generated_by: t\ntask: t\npermissions:\n  shell: true\n  shell_allowlist:\n  - touch\n").unwrap();
        std::fs::write(&plan, format!("source: t\ntask: t\nsteps:\n- id: s1\n  type: shell\n  command: touch {marker}\n")).unwrap();
        let mut e = Env {
            args: vec![],
            env: [("HOME".to_string(), home.clone()), ("PATH".to_string(), "/usr/bin:/bin".to_string())].into(),
            home: home.clone(),
            out: vec![],
            err: vec![],
            tend_failed: false,
            raised: false,
            quiet: false,
            plain: false,
        };
        let args: Vec<String> = ["run", &plan, "-e", &env, "--yes"].iter().map(|s| s.to_string()).collect();
        let code = match e.run_cmd_with(&args[1..], verify_unknown) {
            Ok(()) => 0,
            Err(super::super::Stop::Exit(c)) => c,
            Err(super::super::Stop::Usage(_)) => 2,
        };
        let out = String::from_utf8_lossy(&e.out).into_owned();
        let err = String::from_utf8_lossy(&e.err).into_owned();
        assert_eq!(code, 2, "out {out:?} err {err:?}");
        assert!(out.contains("(rejected)") && err.contains("z3: Z3 returned unknown"), "out {out:?} err {err:?}");
        assert!(std::fs::metadata(&marker).is_err(), "the step ran");
        let traces: Vec<_> = std::fs::read_dir(format!("{home}/.opendaisugi/journal/traces")).unwrap().collect();
        assert_eq!(traces.len(), 1);
        let body = std::fs::read_to_string(traces[0].as_ref().unwrap().path()).unwrap();
        assert!(body.contains("stage: z3") && body.contains("status: rejected"), "{body}");
        let _ = std::fs::remove_dir_all(home);
    }
}
