//! `daisugi verify PLAN --envelope ENVELOPE [--json]`, the verifier on two
//! files, and `daisugi hook report`, one pane state event on stdin.

use std::io::Read;
use std::time::Instant;

use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::pyjson::{dumps_indent, py_repr, Value};
use crate::pathways::pmodel::Id;
use crate::supervise;

impl Env {
    /// typer's `Path(exists=True, dir_okay=False, readable=True)`: its
    /// usage error, or None.
    fn typer_file(name: &str, path: &str) -> Option<String> {
        let Ok(md) = std::fs::metadata(path) else {
            return Some(format!(
                "Invalid value for {name}: File '{path}' does not exist."
            ));
        };
        if md.is_dir() {
            return Some(format!(
                "Invalid value for {name}: File '{path}' is a directory."
            ));
        }
        let c = std::ffi::CString::new(path).ok()?;
        if unsafe { libc::access(c.as_ptr(), libc::R_OK) } != 0 {
            return Some(format!(
                "Invalid value for {name}: File '{path}' is not readable."
            ));
        }
        None
    }

    /// `daisugi verify`: `verify(plan, envelope)` as the oracle answers it.
    /// A Z3 check that does not finish is a warning, as in the oracle's
    /// lenient verify.
    pub(super) fn verify_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "verify";
        const ARGS: &str = "{plan_path}";
        let opts = [
            Opt::val(
                &["--envelope"],
                "FILE",
                "Path to a YAML file containing a serialized Envelope.",
            ),
            Opt::flag(&["--json"], "Emit VerificationResult as JSON."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, ARGS, &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " {plan_path}",
                "Verify an action plan against a safety envelope.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, ARGS, "Missing argument 'plan_path'.");
        }
        let plan_path = p.args[0].clone();
        if let Some(m) = Self::typer_file("'plan_path'", &plan_path) {
            return self.usage_args(CMD, ARGS, &m);
        }
        if !p.has("--envelope") {
            return self.usage_args(CMD, ARGS, "Missing option '--envelope'.");
        }
        let env_path = p.str("--envelope", "");
        if let Some(m) = Self::typer_file("'--envelope'", &env_path) {
            return self.usage_args(CMD, ARGS, &m);
        }
        let plan = match Self::load_model_yaml(&plan_path, Id::ActionPlan) {
            Ok(o) => o,
            Err(Err(why)) => return self.refuse(CMD, &why),
            Err(Ok(perr)) => {
                return Err(self.fail3(
                    &format!("Tried to verify the plan {plan_path}."),
                    &format!("It did not parse: {perr}"),
                    "Fix the file and run again.",
                    2,
                ))
            }
        };
        let env = match Self::load_model_yaml(&env_path, Id::Envelope) {
            Ok(o) => o,
            Err(Err(why)) => return self.refuse(CMD, &why),
            Err(Ok(perr)) => {
                return Err(self.fail3(
                    &format!("Tried to verify against the envelope {env_path}."),
                    &format!("It did not parse: {perr}"),
                    "Fix the file and run again.",
                    2,
                ))
            }
        };
        let why = crate::pathways::verify::stage2_refusal(&Value::Obj(env.clone()));
        if !why.is_empty() {
            return self.refuse(CMD, &why);
        }
        let t0 = Instant::now();
        let r = match crate::pathways::verify::verify(
            &Value::Obj(plan.clone()),
            &Value::Obj(env.clone()),
            None,
            500,
        ) {
            Ok(r) => r,
            Err(e) => return self.refuse(CMD, &e.to_string()),
        };
        let d = supervise::ms(t0);
        let dump = match supervise::verification_dump(
            &r.violations,
            &r.warnings,
            r.warnings_unmodeled,
            env.value("id"),
            plan.value("id"),
            d,
        ) {
            Ok(o) => o,
            Err(why) => return self.refuse(CMD, &why),
        };
        let ok = r.violations.is_empty();
        if p.flag("--json") {
            self.echo(&format!("{}\n", dumps_indent(&Value::Obj(dump), 2, true)));
        } else {
            let status = if ok { "OK" } else { "FAILED" };
            self.echo(&format!("Verification: {status}\n"));
            self.echo(&format!("  plan:     {}\n", py_str(plan.value("id"))));
            self.echo(&format!("  envelope: {}\n", py_str(env.value("id"))));
            self.echo(&format!("  duration: {d:.2}ms\n"));
            let vs = match dump.value("violations") {
                Value::List(l) => l.clone(),
                _ => vec![],
            };
            if !vs.is_empty() {
                self.echo("  violations:\n");
                for v in &vs {
                    let o = v.as_obj().cloned().unwrap_or_default();
                    self.echo(&format!(
                        "    - [{}] {}\n",
                        py_str(o.value("stage")),
                        py_str(o.value("message"))
                    ));
                }
            }
            if !r.warnings.is_empty() {
                self.echo("  warnings:\n");
                for w in &r.warnings {
                    self.echo(&format!("    - {w}\n"));
                }
            }
        }
        if ok {
            Ok(())
        } else {
            exit(1)
        }
    }

    /// `daisugi hook report [--pane P] [--root R]`: one pane state event
    /// on stdin, validated, appended to the session tree and delivered, as
    /// `_state_report.hook_report_argv` does from the command line.
    pub(super) fn hook_report(&mut self, args: &[String]) -> Res {
        const CMD: &str = "hook report";
        let opts = [
            Opt::val(
                &["--pane"],
                "TEXT",
                "Pane id to stamp onto the event, if known.",
            ),
            Opt::val(
                &["--root"],
                "PATH",
                "Gate data root \u{2014} where the session tree this event appends to lives.",
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
                "Read one PaneStateEvent JSON line from stdin and deliver it.",
                &opts,
            );
        }
        let mut raw = vec![];
        let _ = std::io::stdin().read_to_end(&mut raw);
        let default_root = super::gateroot::join(&self.data_home(), "gate");
        let mut argv = vec![
            "--root".to_string(),
            path_str(&p.str("--root", &default_root)),
        ];
        if p.has("--pane") {
            argv.push("--pane".into());
            argv.push(p.str("--pane", ""));
        }
        let (stdout, stderr, code) =
            crate::gate::resident::hook_report_cli(&argv, &raw, &self.env, &self.home);
        if !stdout.is_empty() {
            self.echo(&format!("{stdout}\n"));
        }
        if !stderr.is_empty() {
            self.echo_err(&format!("{stderr}\n"));
        }
        if code != 0 {
            return exit(code);
        }
        Ok(())
    }
}

/// `str(v)` of a dumped value.
fn py_str(v: &Value) -> String {
    match v {
        Value::Str(s) => s.clone(),
        other => py_repr(other).unwrap_or_default(),
    }
}
