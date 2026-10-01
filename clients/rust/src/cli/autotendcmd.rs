//! `daisugi hook auto-tend`: captured sessions turned into journal traces,
//! then a tend, with the flags, output, exit codes and files of the Python
//! CLI. Every conversion is read and checked before the first write.

use std::cell::RefCell;
use std::rc::Rc;

use super::gardencmd::{now_seconds, read_stamp};
use super::gateroot::{join, path_str};
use super::journalparse::journal_result;
use super::tendcmd::{CheckStop, TendReport, DEFAULT_MODEL};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::capture::{infer_envelope, list_sessions, plan, records, CapErr, Session};
use crate::distill::token_hex;
use crate::gate::pyjson::{float_repr, Object};
use crate::pathways::PwErr;
use crate::shell_decompose::ShellParser;
use crate::tracejournal::{iso_seconds, Journal};

/// One session this run converts, checked before anything is written.
pub(super) struct Conversion {
    pub(super) task: String,
    pub(super) env: Object,
    pub(super) plan: Object,
    pub(super) result: Object,
}

/// What a session comes to: a conversion, or a skip with its text.
enum Prepared {
    Convert(Session, Box<Conversion>),
    Skip(Session, String),
}

/// `captures_to_trace` up to the journal write: the records read, the
/// envelope inferred, the plan built and verified. An exception Python
/// raises is `CapErr::Raised` (a plan that does not validate is
/// `CapErr::InvalidPlan`); input this binary does not read the Python way
/// is unreadable. A trace whose verify fails is converted with its
/// violations, worded as journal ingest words them.
pub(super) fn convert_capture(
    parser: &mut ShellParser,
    path: &str,
    id: &str,
    task: &str,
    decompose: bool,
) -> Result<Conversion, CapErr> {
    let recs = records(path)?;
    if recs.is_empty() {
        return Err(CapErr::Raised("ValueError".into(), format!("no records in {path}")));
    }
    let env = infer_envelope(parser, &recs, task, decompose)?;
    let plan = plan(&recs, task)?;
    let v = journal_result(&env, &plan, task, &format!("session {id}")).map_err(|e| match e {
        PwErr::Invalid(why) => PwErr::Unreadable(format!("session {id}: the verifier raised ({why})")),
        other => other,
    })?;
    Ok(Conversion { task: task.to_string(), env, plan, result: v.result? })
}

/// `captures_to_trace` under the default task, as auto-tend runs it: a
/// session it raises ValueError on is skipped with its text; an exception
/// of another kind, or a capture this binary does not read the Python way,
/// is an error.
fn prepare_conversion(parser: &mut ShellParser, s: &Session, root: &str, decompose: bool) -> Result<Prepared, CapErr> {
    let path = join(&path_str(root), &format!("{}.jsonl", s.id));
    match convert_capture(parser, &path, &s.id, &format!("captured session {}", s.id), decompose) {
        Ok(c) => Ok(Prepared::Convert(s.clone(), Box::new(c))),
        Err(CapErr::Raised(typ, msg)) if typ == "ValueError" || typ == "UnicodeDecodeError" => {
            Ok(Prepared::Skip(s.clone(), msg))
        }
        Err(CapErr::InvalidPlan(text)) => Ok(Prepared::Skip(s.clone(), text)),
        Err(e) => Err(e),
    }
}

impl Env {
    pub(super) fn hook(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(
                "Usage: daisugi hook [OPTIONS] COMMAND [ARGS]...\n\n  Capture hooks. This binary carries record, report, \
                 list, to-trace and auto-tend.\n\nCommands:\n  record     Read a hook payload from stdin, record it, return \
                 the host's continue contract.\n  report     Read one PaneStateEvent JSON line from stdin and deliver \
                 it.\n  list       List captured sessions with call counts.\n  to-trace   \
                 Convert a captured session into a journal trace.\n  auto-tend  Close the captures to traces to \
                 distillation loop in one call.\n",
            );
            return Ok(());
        }
        match args[0].as_str() {
            "auto-tend" => return self.auto_tend(&args[1..]),
            "record" => return self.hook_record(&args[1..]),
            "list" => return self.hook_list(&args[1..]),
            "to-trace" => return self.hook_to_trace(&args[1..]),
            "report" => return self.hook_report(&args[1..]),
            _ => {}
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
        let root = p.str("--captures-root", &super::gateroot::join(&self.data_home(), "captures"));
        let data_dir = p.str("--data-dir", &self.data_home());
        let force = p.flag("--force");
        let cfg = match super::config::load(&format!("{data_dir}/config.yaml")) {
            Ok(c) => c,
            Err(super::config::ConfigErr::Invalid | super::config::ConfigErr::Yaml(_)) => {
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
        let sessions = list_sessions(&root).map_err(|e| self.cap_err(CMD, e))?;
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
            let c = prepare_conversion(&mut parser, s, &root, decompose).map_err(|e| self.cap_err(CMD, e))?;
            todo.push(c);
        }
        drop(jr);
        let nconv = todo.iter().filter(|c| matches!(c, Prepared::Convert(..))).count();
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
            if matches!(c, Prepared::Convert(..)) {
                ids.push(token_hex().map_err(|e| self.pw_err(CMD, e))?);
            }
        }
        let j = Journal::open(&data_dir).map_err(|e| self.pw_err(CMD, e))?;
        let mut converted = vec![];
        let mut ids = ids.into_iter();
        for c in &todo {
            match c {
                Prepared::Skip(s, text) => self.errf(&format!("  skipped {}: {text}\n", s.id)),
                Prepared::Convert(session, c) => {
                    let created = format!("{}Z", iso_seconds(now_seconds() as i64));
                    let trace_id = format!("{}-{}", &created[..10], ids.next().unwrap_or_default());
                    j.log(&c.task, &c.env, &c.plan, &c.result, &trace_id, &created).map_err(|e| self.pw_err(CMD, e))?;
                    j.mark_converted(&session.id, &trace_id, now_seconds()).map_err(|e| self.pw_err(CMD, e))?;
                    self.out(&format!("  converted {} \u{2192} {trace_id}\n", session.id));
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

    /// A capture error: an exception Python raises that this command does
    /// not answer, or input it does not read the Python way, is refused
    /// before anything is written.
    pub(super) fn cap_err(&mut self, cmd: &str, e: CapErr) -> Stop {
        match e {
            CapErr::Raised(typ, msg) => self.pw_err(cmd, PwErr::Unreadable(format!("{typ}: {msg}"))),
            CapErr::InvalidPlan(text) => {
                self.pw_err(cmd, PwErr::Unreadable(format!("pydantic_core._pydantic_core.ValidationError: {text}")))
            }
            CapErr::Pw(e) => self.pw_err(cmd, e),
        }
    }

    /// `Daisugi(data_dir=...).tend()` as auto-tend runs it: the defaults,
    /// and a failure reported as "tend failed" rather than raised.
    pub(super) fn run_tend_quiet(&mut self, data_dir: &str) -> Result<Option<TendReport>, Stop> {
        self.tend_failed = true;
        let r = self.run_tend(data_dir, DEFAULT_MODEL, 3, 30, false, None);
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
