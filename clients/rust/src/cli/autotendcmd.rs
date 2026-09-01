//! `daisugi hook auto-tend`: captured sessions turned into journal traces,
//! then a tend, with the flags, output, exit codes and files of the Python
//! CLI. Every conversion is read and checked before the first write.

use std::cell::RefCell;
use std::rc::Rc;
use std::time::Instant;

use super::gardencmd::{now_seconds, read_stamp};
use super::tendcmd::{CheckStop, TendReport, DEFAULT_MODEL};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::capture::{infer_envelope, list_sessions, plan, records, Session, Skip};
use crate::distill::token_hex;
use crate::gate::pyjson::{float_repr, Object, Value};
use crate::pathways::PwErr;
use crate::shell_decompose::ShellParser;
use crate::tracejournal::{iso_seconds, trace_body, Journal};

/// One session this run converts, checked before anything is written.
struct Conversion {
    session: Session,
    task: String,
    env: Object,
    plan: Object,
    result: Object,
}

/// What a session comes to: a conversion, a skip with its text, or no
/// records at all.
enum Prepared {
    Convert(Box<Conversion>),
    Skip(Session, String),
    Empty(Session),
}

/// `captures_to_trace` up to the journal write: the records read, the
/// envelope inferred, the plan built and verified. A capture whose trace
/// would carry a violation is refused: the binary does not word every
/// violation's detail yet.
fn prepare_conversion(parser: &mut ShellParser, s: &Session, decompose: bool) -> Result<Prepared, PwErr> {
    let recs = records(&s.path)?;
    if recs.is_empty() {
        return Ok(Prepared::Empty(s.clone()));
    }
    let task = format!("captured session {}", s.id);
    let env = infer_envelope(parser, &recs, &task, decompose)?;
    let plan = match plan(&recs, &task)? {
        Ok(p) => p,
        Err(Skip::InvalidPlan(text)) => return Ok(Prepared::Skip(s.clone(), text)),
    };
    let t0 = Instant::now();
    let out = crate::pathways::verify::verify(&Value::Obj(plan.clone()), &Value::Obj(env.clone()), None, 500)?;
    let ms = t0.elapsed().as_secs_f64() * 1000.0;
    if let Some(v) = out.violations.first() {
        return Err(PwErr::Unreadable(format!(
            "session {} would be journaled with a violation ({}), whose detail this binary does not write yet",
            s.id, v.message
        )));
    }
    let mut result = Object::new();
    result.set("ok", true);
    result.set("violations", Value::List(vec![]));
    result.set("warnings", Value::List(out.timeouts.into_iter().map(Value::Str).collect()));
    result.set("envelope_id", env.value("id").clone());
    result.set("plan_id", plan.value("id").clone());
    result.set("duration_ms", ms);
    result.set("client", "python");
    result.set("fallback", Value::Null);
    result.set("client_verdict", Value::Null);
    // The body is written after other sessions are: one this binary cannot
    // write must refuse now, before the first write.
    trace_body(&task, &env, &plan, &result, "2000-01-01-00000000", "2000-01-01T00:00:00Z")?;
    Ok(Prepared::Convert(Box::new(Conversion { session: s.clone(), task, env, plan, result })))
}

impl Env {
    pub(super) fn hook(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(
                "Usage: daisugi hook [OPTIONS] COMMAND [ARGS]...\n\n  Capture hooks. This binary carries auto-tend.\n\n\
                 Commands:\n  auto-tend  Close the captures to traces to distillation loop in one call.\n",
            );
            return Ok(());
        }
        if args[0] == "auto-tend" {
            return self.auto_tend(&args[1..]);
        }
        let what = format!("daisugi hook {}", args[0]);
        self.not_yet(&what)
    }

    fn auto_tend(&mut self, args: &[String]) -> Res {
        const CMD: &str = "hook auto-tend";
        let opts = [
            Opt::val(&["--captures-root"], "PATH", "Where the captured sessions are."),
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::val(&["--min-interval"], "INTEGER", "Skip if last auto-tend was newer than this many seconds."),
            Opt::flag(&["--force"], "Ignore the min-interval gate."),
            Opt::flag(&["--skip-distill"], "Convert captures-to-traces but don't run tend afterwards."),
            Opt::pair(&["--allow-shell-decomposition"], "--no-allow-shell-decomposition", "Let the envelope admit compound shell (ADR-0010)."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Close the captures->traces->distillation loop in one cron-friendly call.", &opts);
        }
        let min_interval = self.click_int(CMD, &p, "--min-interval", 3600)?;
        let root = p.str("--captures-root", &format!("{}/.opendaisugi/captures", self.home));
        let data_dir = p.str("--data-dir", &format!("{}/.opendaisugi", self.home));
        let force = p.flag("--force");
        let cfg = match super::config::load(&format!("{data_dir}/config.yaml")) {
            Ok(c) => c,
            Err(super::config::ConfigErr::Invalid) => {
                self.errf(&format!("daisugi {CMD}: pydantic_core._pydantic_core.ValidationError: the config file does not validate\n"));
                return exit(1);
            }
            Err(e) => return self.refuse(CMD, &format!("the config file is not one this binary reads: {e}")),
        };
        if !force && cfg.auto_tend != Some(true) {
            self.out("skipped: background distillation is off. Run `daisugi install` to opt in, or pass --force to tend once anyway.\n");
            return Ok(());
        }
        let stamp = format!("{data_dir}/.hook-auto-tend-last-run");
        let now = now_seconds();
        let last = read_stamp(&stamp).map_err(|e| self.pw_err(CMD, e))?;
        if !force && now - last < min_interval as f64 {
            self.out(&format!(
                "skipped: last run {}s ago (< --min-interval={min_interval}); use --force to override\n",
                (now - last) as i64
            ));
            return Ok(());
        }
        let decompose =
            if p.flag_set("--allow-shell-decomposition") { p.flag("--allow-shell-decomposition") } else { cfg.shell_allow_decomposition };
        // Everything this run would convert is read and checked first, so a
        // capture this binary cannot convert changes nothing.
        let sessions = list_sessions(&root).map_err(|e| self.pw_err(CMD, e))?;
        // Read only: the journal is made or migrated after every refusal.
        let jr = if std::fs::metadata(format!("{data_dir}/journal/index.db")).is_ok() {
            Some(Journal::open_read_only(&data_dir).map_err(|e| self.pw_err(CMD, e))?)
        } else {
            None
        };
        let mut parser = ShellParser::new();
        let mut todo: Vec<Prepared> = vec![];
        for s in &sessions {
            if let Some(j) = &jr {
                if j.is_converted(&s.id).map_err(|e| self.pw_err(CMD, e))? {
                    continue;
                }
            }
            let c = prepare_conversion(&mut parser, s, decompose).map_err(|e| self.pw_err(CMD, e))?;
            todo.push(c);
        }
        drop(jr);
        let nconv = todo.iter().filter(|c| matches!(c, Prepared::Convert(_))).count();
        if nconv > 0 && !p.flag("--skip-distill") {
            // The tend that follows the conversions must not refuse after
            // they are written: its checks run now, with the conversions
            // counted in. Daisugi() raises only after the conversions, in
            // Python; that one is reported when tend runs.
            let notes = Rc::new(RefCell::new(String::new()));
            match self.tend_check(&data_dir, DEFAULT_MODEL, 3, 30, false, nconv, now_seconds(), &notes) {
                Ok(_) | Err(CheckStop::Raised(_)) => {}
                Err(CheckStop::Stop(s)) => return Err(s),
            }
        }
        // The trace ids are drawn before the first write, so a failed draw
        // changes nothing.
        let mut ids: Vec<String> = vec![];
        for c in &todo {
            if matches!(c, Prepared::Convert(_)) {
                ids.push(token_hex().map_err(|e| self.pw_err(CMD, e))?);
            }
        }
        let j = Journal::open(&data_dir).map_err(|e| self.pw_err(CMD, e))?;
        let mut converted = vec![];
        let mut ids = ids.into_iter();
        for c in &todo {
            match c {
                Prepared::Skip(s, text) => self.errf(&format!("  skipped {}: {text}\n", s.id)),
                Prepared::Empty(s) => self.errf(&format!("  skipped {}: no records in {}\n", s.id, s.path)),
                Prepared::Convert(c) => {
                    let created = format!("{}Z", iso_seconds(now_seconds() as i64));
                    let trace_id = format!("{}-{}", &created[..10], ids.next().unwrap_or_default());
                    j.log(&c.task, &c.env, &c.plan, &c.result, &trace_id, &created).map_err(|e| self.pw_err(CMD, e))?;
                    j.mark_converted(&c.session.id, &trace_id, now_seconds()).map_err(|e| self.pw_err(CMD, e))?;
                    self.out(&format!("  converted {} \u{2192} {trace_id}\n", c.session.id));
                    converted.push(trace_id);
                }
            }
        }
        drop(j);
        self.out(&format!("converted {} sessions\n", converted.len()));
        if !converted.is_empty() && !p.flag("--skip-distill") {
            if let Some(r) = self.run_tend_quiet(&data_dir)? {
                self.out(&format!("tend: created={} updated={} skipped={}\n", r.rep.created, r.rep.updated, r.rep.skipped));
            }
        }
        let write = std::fs::create_dir_all(&data_dir).and_then(|_| std::fs::write(&stamp, float_repr(now)));
        if let Err(e) = write {
            return Err(self.pw_err(CMD, PwErr::Io(e)));
        }
        Ok(())
    }

    /// `Daisugi(data_dir=...).tend()` as auto-tend runs it: the defaults,
    /// and a failure reported as "tend failed" rather than raised.
    fn run_tend_quiet(&mut self, data_dir: &str) -> Result<Option<TendReport>, Stop> {
        self.tend_failed = true;
        let r = self.run_tend(data_dir, DEFAULT_MODEL, 3, 30, false);
        self.tend_failed = false;
        match r {
            Ok(r) => Ok(Some(r)),
            Err(s) if self.raised => Err(s),
            // run_tend printed its reason as the command's error; auto-tend
            // catches the exception and goes on.
            Err(Stop::Exit(1)) => Ok(None),
            Err(s) => Err(s),
        }
    }
}
